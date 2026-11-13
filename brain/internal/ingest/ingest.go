// Package ingest rebuilds intervals from samples.
//
// Cold points make the rebuild stateless: an idle sample outside a call, or a
// sleep/resign/term row, closes every interval, resets carry-over and cannot be inside a
// call, so attribution restarted at a cold row sees exactly what a full pass would have
// seen. A cold row inside a call's gap window (less than gap_minutes after an in-call row,
// e.g. a one-tick mic drop) is skipped as well, so a call stretch
// never straddles a rebuild start. Every run deletes the intervals after the latest cold
// point that is at least 15 minutes before the last processed sample and rebuilds them, so
// a restart mid-run is harmless. Without a qualifying cold point (no watermark yet, a
// watermark row that Purge has removed, or no cold row 15 minutes back) the rebuild starts
// at the oldest retained sample, which is a full rebuild of the retained history; it never
// touches intervals that end before that sample. `recompute --since <day>` forces the same
// from a chosen day, never later than a run would start.
package ingest

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/rules"
)

const (
	// MetaWatermark is the meta key holding the last processed samples.id (contract: collector/schema.sql).
	MetaWatermark = "watermark_rowid"
	// RebuildWindow: the rebuild starts at a cold point at least this far before the watermark ts.
	RebuildWindow = 15 * 60
	// chunkMinRows keeps chunks from becoming one-row transactions during long idle
	// stretches. It is an id distance, not a row count: ids are dense except where Purge
	// removed rows, so a chunk may hold fewer rows; harmless.
	chunkMinRows = 5000
)

// ErrEmpty: Recompute found no samples at all (nothing to rebuild from).
var ErrEmpty = errors.New("no samples yet")

// Params derives attribute.Params from the config; Now caps the last sample's coverage.
func Params(cfg *config.Config, m *rules.Matcher, now time.Time) attribute.Params {
	return attribute.Params{
		GapSeconds:       int64(cfg.Settings.GapMinutes) * 60,
		IdleSeconds:      int64(cfg.Settings.IdleMinutes) * 60,
		CarryoverSeconds: int64(cfg.Settings.CarryoverMinutes) * 60,
		Now:              now.Unix(),
		CallApps:         cfg.CallAppSet(),
		Match:            m.Match,
	}
}

// Result says where a rebuild started.
type Result struct {
	FromID    int64 // id of the first sample the rebuild read (the cold point or the oldest sample)
	FromTS    int64 // delete floor: the start row's ts, or lower after a backwards clock step
	FromStart bool  // true when it started at the oldest retained sample
	FirstRun  bool  // true when there was no watermark yet
	Rows      int   // distinct samples read (a chunk boundary row is read by both chunks but counted once)
}

// point is a sample row position.
type point struct{ id, ts int64 }

// Run rebuilds the tail of intervals from the latest suitable cold point and advances
// the watermark. With no samples at all it does nothing.
func Run(sq *sql.DB, cfg *config.Config, m *rules.Matcher, now time.Time) (Result, error) {
	p := Params(cfg, m, now)
	start, ok, wm, err := runStart(sq, p)
	if err != nil {
		return Result{}, err
	}
	res, err := rebuild(sq, p, start, ok, retentionCutoff(cfg, now), nil)
	res.FirstRun = wm == 0
	return res, err
}

// RunStart is the sample id a Run would rebuild from right now: the latest cold point at
// least RebuildWindow before the watermark row's ts, else the oldest retained sample. ok is
// false with no samples. Call detection reads from here, captured before anything (a
// resumed recompute, applied pop-up decisions) moves the watermark.
func RunStart(sq *sql.DB, p attribute.Params) (id int64, ok bool, err error) {
	start, haveStart, _, err := runStart(sq, p)
	if err != nil {
		return 0, false, err
	}
	start, ok, err = firstRow(sq, start, haveStart)
	return start.id, ok, err
}

// runStart is Run's cold point: the latest one at least RebuildWindow before the watermark
// row's ts. haveStart is false without one (no watermark yet, its row purged, or no cold
// row that far back); the rebuild then starts at the oldest retained sample (firstRow).
// wm is the watermark id, 0 before the first run. Run and RunStart share it.
func runStart(sq *sql.DB, p attribute.Params) (start point, haveStart bool, wm int64, err error) {
	if wm, err = watermark(sq); err != nil || wm == 0 {
		return point{}, false, wm, err
	}
	wmTS, found, err := db.TSOfID(sq, wm)
	if err != nil || !found {
		return point{}, false, wm, err
	}
	start, haveStart, err = coldPointBefore(sq, wm, wmTS-RebuildWindow, p)
	return start, haveStart, wm, err
}

// firstRow is the row a rebuild reads first: start when haveStart, else the oldest
// retained sample. ok is false with no samples at all. rebuild and RunStart share it.
func firstRow(sq *sql.DB, start point, haveStart bool) (point, bool, error) {
	if haveStart {
		return start, true, nil
	}
	return oldestSample(sq)
}

// Recompute rebuilds intervals from the last cold point at or before since (local midnight
// of the given day), or from Run's start when that is earlier: a start past the watermark
// would skip the rows no run has ingested yet (e.g. a future-date typo). It
// fails with db.ErrBeforeHorizon when that day precedes the oldest sample and with ErrEmpty
// when there are no samples at all.
func Recompute(sq *sql.DB, cfg *config.Config, m *rules.Matcher, since time.Time, now time.Time) (Result, error) {
	return RecomputeApplying(sq, cfg, m, since, now, nil)
}

// RecomputeApplying is Recompute that also honours the answers of the stretches rows in
// applying (by from_ts), which the apply step is about to mark applied.
func RecomputeApplying(sq *sql.DB, cfg *config.Config, m *rules.Matcher, since time.Time, now time.Time, applying map[int64]bool) (Result, error) {
	oldest, ok, err := db.OldestSampleTS(sq)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, ErrEmpty
	}
	oldestDay := time.Unix(oldest, 0).In(cfg.Location).Format("2006-01-02")
	if since.In(cfg.Location).Format("2006-01-02") < oldestDay {
		return Result{}, fmt.Errorf("%w: oldest sample is on %s", db.ErrBeforeHorizon, oldestDay)
	}
	p := Params(cfg, m, now)
	start, ok, err := coldPointBefore(sq, 1<<62-1, since.Unix(), p)
	if err != nil {
		return Result{}, err
	}
	run, haveRun, _, err := runStart(sq, p)
	if err != nil {
		return Result{}, err
	}
	if !haveRun || run.id < start.id {
		start, ok = run, haveRun
	}
	return rebuild(sq, p, start, ok, retentionCutoff(cfg, now), applying)
}

// retentionCutoff is the ts below which samples are not retained: Purge deletes them (it
// runs after ingest, so rows under the cutoff can still be read once, in the run that
// purges them) and the rebuild's delete floor stops there, so the two can never drift apart.
func retentionCutoff(cfg *config.Config, now time.Time) int64 {
	return now.Unix() - int64(cfg.Settings.RawRetentionDays)*86400
}

// Purge deletes samples older than raw_retention_days, and the calls and stretches rows that
// ended before that (their samples are gone, the decided intervals stay). Returns the sample count.
func Purge(sq *sql.DB, cfg *config.Config, now time.Time) (int64, error) {
	cutoff := retentionCutoff(cfg, now)
	res, err := sq.Exec(`DELETE FROM samples WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	if _, err := db.PurgeCalls(sq, cutoff); err != nil {
		return 0, err
	}
	if _, err := db.PurgeStretches(sq, cutoff); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// rebuild deletes intervals with to_ts > floor(start) and re-derives them chunk by chunk,
// each chunk running from one cold point to the next (inclusive, so the closing cold
// row's effect is applied). One short transaction per chunk. Without a cold point
// (haveStart false) it starts at the oldest retained sample.
//
// The delete floor cannot simply be start.ts: a backwards clock step can leave samples
// with id >= start.id but ts < start.ts (the collector trusts the OS clock), and those
// rows re-derive an interval that ends before start.ts, below a to_ts > start.ts filter,
// so it would never be deleted and every run would re-insert a duplicate. The floor is
// therefore the lower of start.ts and the earliest ts among samples from start.id on,
// clamped at the retention cutoff: intervals are kept forever while samples are not, so
// Run never deletes an interval that ends before the cutoff — it could not re-derive it.
//
// Later chunks (i > 0) use their own cold point's ts as the floor. A backwards step inside
// such a chunk re-derives an interval below that floor, but the chunks' delete windows
// tile [floor, ∞) and every window is cleared before its chunk inserts, so the interval
// lands in an earlier chunk's window: deleted there first, re-inserted by its own chunk,
// never duplicated. On a later run whose start precedes the step, chunk 0's floor (a
// minimum over every id from the start on) covers it directly.
func rebuild(sq *sql.DB, p attribute.Params, start point, haveStart bool, cutoff int64, applying map[int64]bool) (Result, error) {
	res := Result{FromStart: !haveStart}
	start, ok, err := firstRow(sq, start, haveStart)
	if err != nil || !ok {
		return res, err // no samples: nothing to do
	}
	res.FromID = start.id
	floor, err := deleteFloor(sq, start, cutoff)
	if err != nil {
		return res, err
	}
	res.FromTS = floor
	colds, err := coldPointsAfter(sq, start.id, p)
	if err != nil {
		return res, err
	}
	bounds := []point{start}
	for _, c := range colds {
		if c.id-bounds[len(bounds)-1].id >= chunkMinRows {
			bounds = append(bounds, c)
		}
	}
	var last int64
	for i, from := range bounds {
		to := point{}
		if i+1 < len(bounds) {
			to = bounds[i+1]
		}
		fromTS := from.ts
		if i == 0 {
			fromTS = floor
		}
		samples, err := db.ReadSamples(sq, from.id, to.id)
		if err != nil {
			return res, err
		}
		res.Rows += len(samples)
		if i > 0 && len(samples) > 0 {
			res.Rows-- // from is the previous chunk's closing row, already counted
		}
		if len(samples) > 0 {
			last = samples[len(samples)-1].ID
		}
		// Decided calls rows overlapping this chunk steer its in-call blocks.
		// Read before the chunk's transaction opens (one connection in the pool).
		q := p
		if q.Decisions, err = Decisions(sq, fromTS, to.ts, p); err != nil {
			return res, err
		}
		if q.Answers, err = Answers(sq, fromTS, to.ts, applying); err != nil {
			return res, err
		}
		if err := writeChunk(sq, attribute.Attribute(samples, q), fromTS, to.ts, last, i+1 == len(bounds)); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Decisions is db.DecisionsOverlapping with each background decision's call app set. The app
// comes from the whole call's samples, never a rebuild chunk's slice, so every chunk of one
// call books the same app (an empty app would make the decision cover nothing).
func Decisions(sq *sql.DB, fromTS, toTS int64, p attribute.Params) ([]attribute.Decision, error) {
	ds, err := db.DecisionsOverlapping(sq, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	for i, d := range ds {
		if d.Decision != attribute.DecisionBackground {
			continue
		}
		samples, err := db.ReadSamplesBetween(sq, d.FromTS, d.ToTS+p.GapSeconds)
		if err != nil {
			return nil, err
		}
		ds[i].BackgroundApp = attribute.BackgroundApp(samples, d, p)
	}
	return ds, nil
}

// Answers is the stretch answers a rebuild of [fromTS, toTS) honours (toTS <= 0: unbounded):
// rows applied since their last decision, plus the rows in applying, which this run is about
// to mark applied. An unapplied or failed row's answer reaches no rebuild.
// A row whose answer no longer parses (edited by hand; the apply step parses every
// answer before it marks it applied) is skipped rather than failing every rebuild.
func Answers(sq *sql.DB, fromTS, toTS int64, applying map[int64]bool) ([]attribute.Answer, error) {
	rows, err := db.AnsweredStretchesOverlapping(sq, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	var out []attribute.Answer
	for _, r := range rows {
		if r.AppliedAt < r.DecidedAt && !applying[r.FromTS] {
			continue
		}
		if a, err := attribute.ParseAnswer(r.FromTS, r.ToTS, r.Answer); err == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// deleteFloor is the lower of start.ts and the earliest ts among samples with
// id >= start.id, so a backwards clock step can't leave a re-derived interval below the
// delete floor used for the first chunk. That earliest ts is bounded below by the
// retention cutoff: one row with a wild ts (a dead PRAM battery, a boot before NTP sync)
// would otherwise drag the floor under every interval ever written and delete history
// that the retained samples cannot rebuild. A single read, run before any transaction
// opens (SetMaxOpenConns(1): no *sql.DB read may happen while a *sql.Tx is open).
func deleteFloor(sq *sql.DB, start point, cutoff int64) (int64, error) {
	var minTS sql.NullInt64
	if err := sq.QueryRow(`SELECT MIN(ts) FROM samples WHERE id >= ?`, start.id).Scan(&minTS); err != nil {
		return 0, err
	}
	if minTS.Valid && minTS.Int64 < start.ts {
		return min(start.ts, max(minTS.Int64, cutoff)), nil
	}
	return start.ts, nil
}

func writeChunk(sq *sql.DB, ivs []attribute.Interval, fromTS, toTS, lastID int64, final bool) error {
	tx, err := sq.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.DeleteIntervals(tx, fromTS, toTS); err != nil {
		return err
	}
	if err := db.InsertIntervals(tx, ivs); err != nil {
		return err
	}
	if final && lastID > 0 {
		if err := db.SetMetaTx(tx, MetaWatermark, strconv.FormatInt(lastID, 10)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func watermark(sq *sql.DB) (int64, error) {
	v, ok, err := db.GetMeta(sq, MetaWatermark)
	if err != nil || !ok {
		return 0, err
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("meta.%s = %q: %w", MetaWatermark, v, err)
	}
	return id, nil
}

// WatermarkTS is the ts of the watermark row; ok is false with no watermark yet or when
// Purge has removed that row.
func WatermarkTS(sq *sql.DB) (int64, bool, error) {
	wm, err := watermark(sq)
	if err != nil || wm == 0 {
		return 0, false, err
	}
	return db.TSOfID(sq, wm)
}

func oldestSample(sq *sql.DB) (point, bool, error) {
	var c point
	err := sq.QueryRow(`SELECT id, ts FROM samples ORDER BY id LIMIT 1`).Scan(&c.id, &c.ts)
	if err == sql.ErrNoRows {
		return point{}, false, nil
	}
	return c, err == nil, err
}

// nonWorkApps is attribute.NonWorkApps' keys in a fixed (sorted) order, matching the
// app IN (...) placeholders built into coldWhere below: one source of truth for which
// frontmost apps can never be work.
var nonWorkApps = sortedKeys(attribute.NonWorkApps)

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// coldWhere selects cold rows: sleep/resign/term, a work row past the idle threshold, or a
// work row whose frontmost app can never be work (attribute.NonWorkApps: the lock screen,
// the screen saver). Two filters are applied in Go (see coldCandidate) because call_app is
// a list: in-call rows, and rows inside a call's gap window, so a stretch
// never straddles a rebuild start.
var coldWhere = fmt.Sprintf(
	`(kind IN ('sleep','resign','term') OR (kind IN ('sample','heartbeat') AND (COALESCE(idle_s,0) >= ? OR app IN (%s))))`,
	strings.TrimSuffix(strings.Repeat("?,", len(nonWorkApps)), ","),
)

// coldArgs are coldWhere's placeholder args, in order: the idle threshold, then each
// attribute.NonWorkApps app name (nonWorkApps order).
func coldArgs(p attribute.Params) []any {
	args := make([]any, 0, 1+len(nonWorkApps))
	args = append(args, p.IdleSeconds)
	for _, a := range nonWorkApps {
		args = append(args, a)
	}
	return args
}

// coldSelect lists a coldWhere candidate with its own call_app and its predecessor's (by
// id) call_app and ts, for coldCandidate. Callers append the WHERE clause.
const coldSelect = `SELECT s.id, s.ts, COALESCE(s.call_app,''),
	COALESCE((SELECT p.call_app FROM samples p WHERE p.id < s.id ORDER BY p.id DESC LIMIT 1),''),
	COALESCE((SELECT p.ts FROM samples p WHERE p.id < s.id ORDER BY p.id DESC LIMIT 1),0)
	FROM samples s WHERE `

// coldCandidate scans one coldSelect row and reports whether it is a cold point: not in a
// call, and not inside a call's gap window (an in-call predecessor less than GapSeconds
// earlier). The second case is a one-tick mic drop while listening hands-off: the idle
// non-call row sits between two in-call rows of one stretch, and a rebuild starting there
// would hand call detection only the stretch's second half.
func coldCandidate(rows *sql.Rows, p attribute.Params) (point, bool, error) {
	var c point
	var call, prevCall string
	var prevTS int64
	if err := rows.Scan(&c.id, &c.ts, &call, &prevCall, &prevTS); err != nil {
		return point{}, false, err
	}
	if attribute.InCall(call, p.CallApps) {
		return c, false, nil
	}
	if attribute.InCall(prevCall, p.CallApps) && c.ts-prevTS < p.GapSeconds {
		return c, false, nil
	}
	return c, true, nil
}

// coldPointBefore is the latest cold row with id <= maxID and ts <= maxTS. In-call rows
// and rows inside a call's gap window are skipped in Go, so the query carries no LIMIT (a
// LIMIT 1 would stop at a skipped row); ORDER BY id DESC walks the rowid index backwards
// and rows.Close ends the scan at the first accepted row.
func coldPointBefore(sq *sql.DB, maxID, maxTS int64, p attribute.Params) (point, bool, error) {
	args := append([]any{maxID, maxTS}, coldArgs(p)...)
	rows, err := sq.Query(coldSelect+`s.id <= ? AND s.ts <= ? AND `+coldWhere+` ORDER BY s.id DESC`, args...)
	if err != nil {
		return point{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		c, ok, err := coldCandidate(rows, p)
		if err != nil {
			return point{}, false, err
		}
		if ok {
			return c, true, nil
		}
	}
	return point{}, false, rows.Err()
}

// coldPointsAfter lists cold rows with id > afterID in id order.
func coldPointsAfter(sq *sql.DB, afterID int64, p attribute.Params) ([]point, error) {
	args := append([]any{afterID}, coldArgs(p)...)
	rows, err := sq.Query(coldSelect+`s.id > ? AND `+coldWhere+` ORDER BY s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []point
	for rows.Next() {
		c, ok, err := coldCandidate(rows, p)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

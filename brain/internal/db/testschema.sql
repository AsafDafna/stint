-- TEST FIXTURE: a byte-for-byte copy of collector/schema.sql (the collector owns the
-- schema). If collector/schema.sql changes, copy it here again; the brain never applies
-- this file to the production database.
-- samples.call_app is a comma-joined, deduplicated list of the bundle ids holding the mic (NULL when none).
-- stint schema, owned by the collector. Applied on every collector start; idempotent.
-- Contract with the brain:
--   samples.rowid  : stable ingest watermark (INTEGER PRIMARY KEY aliases rowid, survives VACUUM)
--   samples.ts     : unix epoch seconds, UTC
--   meta.schema_version : "4"; the brain refuses unknown versions
--   meta keys written by the brain (the collector never touches them):
--     watermark_rowid   : last samples.id ingested; the next run resumes from it
--     health            : "ok" or a short problem string, written every run
--     health_at         : unix epoch seconds of the last health write; the menu bar warns when stale > 30 min
--     health_stale_ts   : newest samples.ts seen by the last in-active-hours run (stale-collector counter)
--     health_stale_runs : consecutive in-active-hours runs with no new sample; alerts at 12
--     recompute_since   : "YYYY-MM-DD" while a recompute is in progress; the next run finishes it
--     purge_day         : "YYYY-MM-DD" of the day being tallied by purge_rows
--     purge_rows        : samples purged so far on purge_day; logged once on the next day's first purge
--   notified            : one row per push sent (kind, project, period_key), written by the brain after a 2xx
--   totals.day      : "YYYY-MM-DD" in the Mac's system timezone; totals.project may be "parent/sub", the menu rolls up on "/"
--   samples.call_app: every process holding the mic, each resolved to its top-most ancestor bundle id below launchd
--                     (".helper*" suffixes stripped), deduplicated, comma-joined in enumeration order; none -> NULL.
--                     The brain matches ANY element against its call_apps allowlist.
--   samples.idle_s  : the collector writes a row when idle crosses 300 s; the brain's idle_minutes*60 must equal 300
--                     (a different value is only seen at the next heartbeat, i.e. up to 60 s late).
--   meta.projects   : sorted, comma-joined project names from rules.toml, written by the brain every run;
--                     the collector's meeting pop-up fills its project picker from it.
--   meta.week_start : lowercase weekday ("sunday" ... "saturday") that starts the weeks in totals.week_seconds, written
--                     by the brain in the same transaction as totals. The menu bar starts its week on it; missing or
--                     unknown means sunday.
--   calls           : one row per closed call stretch >= 300 s. The brain inserts rows
--                     (guess + breakdown JSON) and sets applied_at; the collector's pop-up writes decision,
--                     project, subproject and decided_at. from_ts = ts of the stretch's first in-call sample.
--                     decision is 'project', 'split' or 'background'; a 'background' row has NULL project and subproject.
--   meta.picker     : JSON array of "project" and "project/subproject" strings, written by the brain every run: every
--                     declared project, each followed by its sub-projects with time in the last 30 days (worktree-folded).
--                     The collector's stretch pop-up fills its picker from it.
--   stretches       : one row per closed stretch with >= 600 s unattributed. The brain inserts and
--                     refreshes rows, deletes never-decided ones, applies rows (applied_at, rules_error) and its
--                     `stretches reopen` CLI resets answer, decided_at and rules_error. The collector's panel writes
--                     answer and decided_at, only WHERE answer IS NULL.
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS samples (
  id       INTEGER PRIMARY KEY,
  ts       INTEGER NOT NULL,
  kind     TEXT    NOT NULL CHECK (kind IN ('sample','heartbeat','sleep','wake','resign','active','term')),
  app      TEXT,
  title    TEXT,
  url      TEXT,
  cwd      TEXT,
  idle_s   INTEGER,
  call_app TEXT
);
CREATE INDEX IF NOT EXISTS samples_ts ON samples(ts);

CREATE TABLE IF NOT EXISTS intervals (
  from_ts    INTEGER NOT NULL,
  to_ts      INTEGER NOT NULL,
  project    TEXT    NOT NULL,
  subproject TEXT,
  rule_id    TEXT
);
CREATE INDEX IF NOT EXISTS intervals_from_ts ON intervals(from_ts);

CREATE TABLE IF NOT EXISTS totals (
  project      TEXT    NOT NULL,
  day          TEXT    NOT NULL,
  day_seconds  INTEGER NOT NULL DEFAULT 0,
  week_seconds INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (project, day)
);

CREATE TABLE IF NOT EXISTS notified (
  kind       TEXT    NOT NULL,
  project    TEXT    NOT NULL,
  period_key TEXT    NOT NULL,
  sent_at    INTEGER NOT NULL,
  PRIMARY KEY (kind, project, period_key)
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS calls (
  from_ts          INTEGER PRIMARY KEY,
  to_ts            INTEGER NOT NULL,
  call_app         TEXT    NOT NULL,
  guess_project    TEXT    NOT NULL,
  guess_subproject TEXT,
  breakdown        TEXT    NOT NULL,
  decision         TEXT    CHECK (decision IN ('project','split','background')),
  project          TEXT,
  subproject       TEXT,
  decided_at       INTEGER,
  applied_at       INTEGER
);

CREATE TABLE IF NOT EXISTS stretches (
  from_ts        INTEGER PRIMARY KEY,   -- start of the stretch's first unmatched span (carry-over head included)
  to_ts          INTEGER NOT NULL,      -- end of its last unmatched span; [from_ts, to_ts) is fixed at insert
  unattributed_s INTEGER NOT NULL,      -- rule_id 'unattributed' seconds inside the window, refreshed
  breakdown      TEXT    NOT NULL,      -- JSON: [{"key","seconds","no_always"}]
  answer         TEXT,                  -- JSON: [{"key","project","subproject","always","label"}]; NULL = unanswered
  decided_at     INTEGER,               -- set by an answer or a reopen; NULL = never decided
  applied_at     INTEGER,               -- the brain applies while decided_at > COALESCE(applied_at, 0)
  rules_error    TEXT                   -- why the last apply attempt failed; NULL when fine
);

INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', '4');

import Foundation

/// One observation. Optional fields become NULL in SQLite.
struct Sample: Equatable {
    var ts: Int             // unix epoch seconds, UTC
    var kind: String        // sample | heartbeat | sleep | wake | resign | active | term
    var app: String?        // frontmost bundle id
    var title: String?      // normalized focused-window title
    var url: String?        // active tab URL when frontmost is one of Bundles.browsers
    var cwd: String?        // terminal foreground cwd
    var idleS: Int?         // seconds since last input event
    var callApp: String?    // bundle id of the app holding the mic
}

struct TotalRow: Equatable {
    var project: String
    var daySeconds: Int
    var weekSeconds: Int
}

/// A calls row awaiting the pop-up (brain-written; the collector only reads these columns).
struct PendingCall: Equatable {
    var fromTs: Int
    var toTs: Int
    var callApp: String
    var guessProject: String
    var guessSubproject: String?
    var breakdown: String   // JSON array from the brain, see Pure.parseBreakdown
}

struct BreakdownLine: Equatable {
    var project: String     // Pure.meetingLine for the minutes a split books to the guess
    var subproject: String?
    var seconds: Int
}

enum CallDecision: Equatable {
    case project(String, subproject: String?)
    case split
    case background
}

enum CallChoice { case whole, split, background }

struct BackgroundLine: Equatable {
    var label: String       // "<project>", "<project>/<subproject>", "unattributed" or "away"
    var seconds: Int
}

/// What the UPDATE binds: decision text, project and subproject (NULL when nil).
struct DecisionBinding: Equatable {
    var decision: String
    var project: String?
    var subproject: String?
}

/// A stretches row awaiting the pop-up (brain-written; the collector reads these columns).
struct PendingStretch: Equatable {
    var fromTs: Int
    var toTs: Int
    var unattributedS: Int
    var breakdown: String   // JSON array from the brain, see Pure.parseStretchBreakdown
}

/// One breakdown entry: a row key ("host:grok.com", "app:<bundle id>") and its unmatched seconds.
struct StretchKey: Equatable {
    var key: String
    var seconds: Int
    var noAlways: Bool
}

/// One panel row: a single key, or the "other" row that folds the small and surplus keys.
struct StretchRow: Equatable {
    var keys: [String]
    var seconds: Int
    var isOther: Bool
    var noAlways: Bool
}

/// Where a row's minutes go.
enum StretchTarget: Hashable {
    case project(String, sub: String?)
    case leave
}

/// Menu and auto-open identity: calls and stretches share one queue.
struct PendingID: Hashable {
    enum Kind: Hashable { case call, stretch }
    var kind: Kind
    var fromTs: Int
}

enum Pure {
    static let idleThresholdS = 300
    static let heartbeatS = 60
    /// Follows the Mac's timezone while the app runs; the brain uses the same zone for totals.day.
    static let systemZone = TimeZone.autoupdatingCurrent

    /// True for an existing `calls` table whose CHECK predates 'background'. `nil` means no table yet.
    static func callsNeedsV4Rebuild(createSQL: String?) -> Bool {
        guard let createSQL else { return false }
        return !createSQL.contains("'background'")
    }

    /// SQLite cannot alter a CHECK in place; keep the columns identical to schema.sql's `calls`.
    static let callsV4Rebuild = """
    BEGIN IMMEDIATE;
    CREATE TABLE calls_v4 (
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
    INSERT INTO calls_v4 (from_ts, to_ts, call_app, guess_project, guess_subproject, breakdown, decision, project, subproject, decided_at, applied_at)
      SELECT from_ts, to_ts, call_app, guess_project, guess_subproject, breakdown, decision, project, subproject, decided_at, applied_at FROM calls;
    DROP TABLE calls;
    ALTER TABLE calls_v4 RENAME TO calls;
    COMMIT;
    """

    // MARK: title normalization

    /// Strips leading "(n) " unread counters and a trailing "hh:mm:ss" / "mm:ss" timer.
    /// Empty or whitespace-only input yields nil (stored as NULL).
    static func normalizeTitle(_ raw: String?) -> String? {
        guard var s = raw?.trimmingCharacters(in: .whitespacesAndNewlines), !s.isEmpty else { return nil }
        while let r = s.range(of: #"^\(\d+\)\s*"#, options: .regularExpression) {
            s.removeSubrange(r)
        }
        if let r = s.range(of: #"(\s*[-–—|·:]\s*|\s+)\d{1,2}:\d{2}(:\d{2})?\s*$"#, options: .regularExpression) {
            s.removeSubrange(r)
        }
        s = s.trimmingCharacters(in: .whitespacesAndNewlines)
        return s.isEmpty ? nil : s
    }

    // MARK: change detection

    /// Decides whether `current` must be written given the last written row.
    /// Returns "sample" on any field change or an idle-threshold crossing,
    /// "heartbeat" when `heartbeatS` has elapsed since the last write, nil otherwise.
    static func writeKind(lastWritten: Sample?, current: Sample) -> String? {
        guard let last = lastWritten else { return "sample" }
        if last.kind != "sample" && last.kind != "heartbeat" { return "sample" }  // after a boundary row, re-baseline
        if last.app != current.app || last.title != current.title || last.url != current.url
            || last.cwd != current.cwd || last.callApp != current.callApp {
            return "sample"
        }
        let wasIdle = (last.idleS ?? 0) >= idleThresholdS
        let isIdle = (current.idleS ?? 0) >= idleThresholdS
        if wasIdle != isIdle { return "sample" }
        if current.ts - last.ts >= heartbeatS { return "heartbeat" }
        return nil
    }

    // MARK: cwd fallback file selection

    struct CwdFile: Equatable {
        var name: String
        var mtime: Date
        var contents: String
    }

    /// Newest file wins; ties break on name so the result is deterministic. Blank contents are skipped.
    static func newestCwd(_ files: [CwdFile]) -> String? {
        let usable = files
            .map { CwdFile(name: $0.name, mtime: $0.mtime, contents: $0.contents.trimmingCharacters(in: .whitespacesAndNewlines)) }
            .filter { !$0.contents.isEmpty }
        let best = usable.max { a, b in
            if a.mtime != b.mtime { return a.mtime < b.mtime }
            return a.name < b.name
        }
        return best?.contents
    }

    // MARK: herdr socket protocol (protocol 20, verified 2026-09-16)

    /// Exactly what `herdr pane current` sends: one JSON object terminated by "\n".
    static func herdrRequest(id: String) -> Data {
        let obj: [String: Any] = ["id": id, "method": "pane.current", "params": [String: Any]()]
        var data = try! JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys])
        data.append(0x0A)
        return data
    }

    /// `result.pane.foreground_cwd` of the focused pane; falls back to `cwd`; nil on error responses.
    static func herdrForegroundCwd(_ response: Data) -> String? {
        guard let obj = try? JSONSerialization.jsonObject(with: response) as? [String: Any],
              let result = obj["result"] as? [String: Any],
              let pane = result["pane"] as? [String: Any] else { return nil }
        if let fg = pane["foreground_cwd"] as? String, !fg.isEmpty { return fg }
        if let cwd = pane["cwd"] as? String, !cwd.isEmpty { return cwd }
        return nil
    }

    /// Focused pane's `terminal_title_stripped` (agent glyphs removed; verified 2026-09-22), falling back to
    /// `terminal_title`; nil on error responses, garbage, or a pane without a title.
    static func herdrPaneTitle(_ response: Data) -> String? {
        guard let obj = try? JSONSerialization.jsonObject(with: response) as? [String: Any],
              let result = obj["result"] as? [String: Any],
              let pane = result["pane"] as? [String: Any] else { return nil }
        if let t = pane["terminal_title_stripped"] as? String, !t.isEmpty { return t }
        if let t = pane["terminal_title"] as? String, !t.isEmpty { return t }
        return nil
    }

    // MARK: remote input (herdr client tty atime) vs HID idle

    /// Effective idle: the fresher of HID idle and the remote-input age, whichever signals exist.
    static func effectiveIdle(hidIdle: Int?, remoteAge: Int?) -> Int? {
        [hidIdle, remoteAge].compactMap { $0 }.min()
    }

    /// True when the sample should be attributed to remote work (iPad over mosh into a herdr client): a remote
    /// keystroke exists, it is strictly fresher than the last HID event (a tie is not evidence), it is under the
    /// idle threshold, and the frontmost app is not a herdr host (there the local herdr client's tty would also
    /// be fresh, and the ordinary herdr branch already attributes the time). A missing HID reading does not veto:
    /// a fresh remote keystroke is the only activity signal then.
    static func remoteDecision(hidIdle: Int?, remoteAge: Int?, app: String?, herdrHosts: Set<String>) -> Bool {
        guard let r = remoteAge, r < idleThresholdS else { return false }
        if let h = hidIdle, r >= h { return false }
        if let a = app, herdrHosts.contains(a) { return false }
        return true
    }

    // MARK: dates

    static func dayString(_ date: Date, tz: TimeZone = systemZone) -> String {
        let f = DateFormatter()
        f.calendar = Calendar(identifier: .gregorian)
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = tz
        f.dateFormat = "yyyy-MM-dd"
        return f.string(from: date)
    }

    /// meta.week_start as a Calendar weekday (1 = Sunday). Missing or unknown means Sunday, so the
    /// collector and the brain can be deployed in either order.
    static func firstWeekday(_ name: String?) -> Int {
        let names = ["sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"]
        guard let name, let i = names.firstIndex(of: name) else { return 1 }
        return i + 1
    }

    /// The day that starts the half-open week containing `date`, as "yyyy-MM-dd"; `firstWeekday` is a Calendar weekday.
    static func weekStartDay(_ date: Date, firstWeekday: Int = 1, tz: TimeZone = systemZone) -> String {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = tz
        cal.firstWeekday = firstWeekday
        let start = cal.dateInterval(of: .weekOfYear, for: date)!.start
        return dayString(start, tz: tz)
    }

    // MARK: menu bar rollup and formatting

    /// "acme/acme-app" -> "acme"
    static func topLevelProject(_ name: String) -> String {
        if let i = name.firstIndex(of: "/") { return String(name[..<i]) }
        return name
    }

    /// Sums sub-projects into their top-level project, sorted by week seconds descending then name.
    static func rollup(_ rows: [TotalRow]) -> [TotalRow] {
        var acc: [String: TotalRow] = [:]
        for r in rows {
            let key = topLevelProject(r.project)
            var t = acc[key] ?? TotalRow(project: key, daySeconds: 0, weekSeconds: 0)
            t.daySeconds += r.daySeconds
            t.weekSeconds += r.weekSeconds
            acc[key] = t
        }
        return acc.values.sorted { a, b in
            if a.weekSeconds != b.weekSeconds { return a.weekSeconds > b.weekSeconds }
            return a.project < b.project
        }
    }

    /// 7530 -> "2:05"
    static func formatHours(_ seconds: Int) -> String {
        let s = max(0, seconds)
        return String(format: "%d:%02d", s / 3600, (s % 3600) / 60)
    }

    /// Boundary kinds after which nothing is counted until the next ordinary sample: `sleep`/`resign`/`term`
    /// end the interval, `wake`/`active` start nothing by themselves.
    static let openIntervalResetKinds: Set<String> = ["sleep", "resign", "term", "wake", "active"]

    /// Seconds of the open interval the brain has not yet counted: time since its last `totals` write,
    /// only while the user is active, capped at 15 minutes (the brain re-derives the last 15 min anyway).
    static func openIntervalSeconds(now: Int, lastTotalsUpdate: Int?, idleS: Int?, lastKind: String?) -> Int {
        guard let last = lastTotalsUpdate else { return 0 }
        if (idleS ?? 0) >= idleThresholdS { return 0 }
        if let k = lastKind, openIntervalResetKinds.contains(k) { return 0 }
        return min(max(0, now - last), 900)
    }

    /// True when the brain has not reported "ok" within the last 30 minutes.
    static func healthWarning(health: String?, healthAt: Int?, now: Int) -> Bool {
        guard let h = health, let at = healthAt else { return true }
        return h != "ok" || now - at > 1800
    }

    // MARK: call_app list

    /// "com.google.Chrome.helper.renderer" -> "com.google.Chrome"
    static func normalizeCallApp(_ bundleId: String) -> String {
        var s = bundleId
        if let r = s.range(of: #"\.[Hh]elper(\..*)?$"#, options: .regularExpression) { s.removeSubrange(r) }
        return s
    }

    /// Normalized, empties dropped, deduplicated in first-seen order, comma-joined; nothing -> nil (NULL).
    static func joinCallApps(_ bundleIds: [String]) -> String? {
        var seen: [String] = []
        for raw in bundleIds {
            let b = normalizeCallApp(raw)
            if !b.isEmpty && !seen.contains(b) { seen.append(b) }
        }
        return seen.isEmpty ? nil : seen.joined(separator: ",")
    }

    // MARK: meeting pop-up

    static let unattributed = "unattributed"
    static let meetingLine = "meeting"

    /// Lenient: garbage -> [], entries without a "project" are dropped, a missing "subproject" is nil,
    /// a missing "seconds" is 0, unknown keys are ignored. The menu must never fail on a brain-written row.
    static func parseBreakdown(_ json: String) -> [BreakdownLine] {
        guard let data = json.data(using: .utf8),
              let arr = (try? JSONSerialization.jsonObject(with: data)) as? [[String: Any]] else { return [] }
        return arr.compactMap { e in
            guard let p = e["project"] as? String, !p.isEmpty else { return nil }
            let sub = (e["subproject"] as? String).flatMap { $0.isEmpty ? nil : $0 }
            let secs = (e["seconds"] as? NSNumber)?.intValue ?? 0
            return BreakdownLine(project: p, subproject: sub, seconds: secs)
        }
    }

    static let backgroundTitle = "It ran in the background: count only what I did"
    static let awayLabel = "away, not counted"

    /// nil when no entry has a "background" key (a row written by the old brain); entries without a
    /// "seconds" count 0.
    static func parseBackground(_ json: String) -> [BackgroundLine]? {
        guard let data = json.data(using: .utf8),
              let arr = (try? JSONSerialization.jsonObject(with: data)) as? [[String: Any]] else { return nil }
        let lines = arr.compactMap { e -> BackgroundLine? in
            guard let l = e["background"] as? String, !l.isEmpty else { return nil }
            return BackgroundLine(label: l, seconds: (e["seconds"] as? NSNumber)?.intValue ?? 0)
        }
        return lines.isEmpty ? nil : lines
    }

    static func backgroundLines(_ lines: [BackgroundLine]) -> [String] {
        lines.map { "\($0.label == "away" ? awayLabel : $0.label)  \(minutesText($0.seconds))" }
    }

    /// 1860 -> "31 min", 3900 -> "1 h 05 min"; rounds to the nearest minute.
    static func minutesText(_ seconds: Int) -> String {
        let m = (max(0, seconds) + 30) / 60
        return m < 60 ? "\(m) min" : String(format: "%d h %02d min", m / 60, m % 60)
    }

    /// One display line per breakdown entry; the meeting bucket names where a split books it.
    static func breakdownLines(_ lines: [BreakdownLine], guess: String) -> [String] {
        lines.map { l in
            if l.project == meetingLine { return "meeting / unmatched  \(minutesText(l.seconds)) → \(guess)" }
            let name = l.subproject.map { "\(l.project)/\($0)" } ?? l.project
            return "\(name)  \(minutesText(l.seconds))"
        }
    }

    /// Split is pointless when every minute would go to the guess anyway.
    static func splitEnabled(_ lines: [BreakdownLine]) -> Bool {
        lines.contains { $0.project != meetingLine }
    }

    private static func formatter(_ format: String, tz: TimeZone) -> DateFormatter {
        let f = DateFormatter()
        f.calendar = Calendar(identifier: .gregorian)
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = tz
        f.dateFormat = format
        return f
    }

    private static func dayText(_ format: String, _ ts: Int, tz: TimeZone) -> String {
        formatter(format, tz: tz).string(from: Date(timeIntervalSince1970: TimeInterval(ts)))
    }

    private static func clockRange(from: Int, to: Int, tz: TimeZone) -> String {
        let hm = formatter("HH:mm", tz: tz)
        let a = hm.string(from: Date(timeIntervalSince1970: TimeInterval(from)))
        let b = hm.string(from: Date(timeIntervalSince1970: TimeInterval(to)))
        return "\(a)–\(b)"
    }

    private static func timeSpan(from: Int, to: Int, tz: TimeZone) -> String {
        "\(clockRange(from: from, to: to, tz: tz)) · \(minutesText(to - from))"
    }

    /// "Wed 23 Sep · 19:01–19:58 · 57 min"
    static func callRange(from: Int, to: Int, tz: TimeZone = systemZone) -> String {
        let day = formatter("EEE d MMM", tz: tz).string(from: Date(timeIntervalSince1970: TimeInterval(from)))
        return "\(day) · \(timeSpan(from: from, to: to, tz: tz))"
    }

    /// "Zoom · 19:01–19:58 · 57 min"
    static func callMenuTitle(app: String, from: Int, to: Int, tz: TimeZone = systemZone) -> String {
        "\(app) · \(timeSpan(from: from, to: to, tz: tz))"
    }

    /// Comma-joined bundle ids -> "Zoom + Slack" via lookup (nil falls back to the id); nothing -> "call".
    static func callAppNames(_ callApp: String, lookup: (String) -> String?) -> String {
        let names = callApp.split(separator: ",").map(String.init).filter { !$0.isEmpty }.map { lookup($0) ?? $0 }
        return names.isEmpty ? "call" : names.joined(separator: " + ")
    }

    /// Picker items: the stretch pop-up's targets plus the guess (with its sub-project) when it is missing;
    /// "unattributed" never appears.
    static func pickerItems(picker: String?, projects: String?, guess: String, guessSub: String?) -> [StretchTarget] {
        let items = pickerTargets(picker: picker, projects: projects)
        let g = StretchTarget.project(guess, sub: guessSub)
        return guess == unattributed || items.contains(g) ? items : items + [g]
    }

    /// The decision to save, or nil when nothing valid was chosen. The chosen item's sub-project is booked
    /// as is, so a top-level item books none.
    static func decision(chosen: StretchTarget?, choice: CallChoice) -> CallDecision? {
        switch choice {
        case .split: return .split
        case .background: return .background
        case .whole: break
        }
        guard case .project(let p, let sub)? = chosen, p != unattributed, !p.isEmpty else { return nil }
        return .project(p, subproject: sub)
    }

    static func decisionBinding(_ d: CallDecision) -> DecisionBinding {
        switch d {
        case .split: return DecisionBinding(decision: "split", project: nil, subproject: nil)
        case .background: return DecisionBinding(decision: "background", project: nil, subproject: nil)
        case .project(let p, let sub): return DecisionBinding(decision: "project", project: p, subproject: sub)
        }
    }

    // MARK: stretch pop-up

    static let leaveTitle = "Leave unattributed"
    static let newSubprojectTitle = "New sub-project…"
    static let followAllTitle = "(all)"
    static let rowMinSeconds = 60
    static let maxRows = 8

    /// Lenient like parseBreakdown: garbage -> [], entries without a "key" are dropped.
    static func parseStretchBreakdown(_ json: String) -> [StretchKey] {
        guard let data = json.data(using: .utf8),
              let arr = (try? JSONSerialization.jsonObject(with: data)) as? [[String: Any]] else { return [] }
        return arr.compactMap { e in
            guard let k = e["key"] as? String, !k.isEmpty else { return nil }
            return StretchKey(key: k, seconds: (e["seconds"] as? NSNumber)?.intValue ?? 0,
                              noAlways: (e["no_always"] as? Bool) ?? false)
        }
    }

    /// Keys of at least 60 s become rows, at most 8, by seconds; the rest fold into one "other" row.
    static func stretchRows(_ keys: [StretchKey]) -> [StretchRow] {
        let sorted = keys.sorted { $0.seconds != $1.seconds ? $0.seconds > $1.seconds : $0.key < $1.key }
        var rows: [StretchRow] = []
        var other: [StretchKey] = []
        for k in sorted {
            if k.seconds >= rowMinSeconds && rows.count < maxRows {
                rows.append(StretchRow(keys: [k.key], seconds: k.seconds, isOther: false, noAlways: k.noAlways))
            } else {
                other.append(k)
            }
        }
        if !other.isEmpty {
            rows.append(StretchRow(keys: other.map(\.key), seconds: other.reduce(0) { $0 + $1.seconds }, isOther: true, noAlways: true))
        }
        return rows
    }

    /// "host:grok.com" -> "grok.com"; "app:<id>" -> the app's name via lookup, else the id.
    static func keyLabel(_ key: String, lookup: (String) -> String?) -> String {
        if key.hasPrefix("host:") { return String(key.dropFirst(5)) }
        let id = key.hasPrefix("app:") ? String(key.dropFirst(4)) : key
        return lookup(id) ?? id
    }

    /// A row's label; the "other" row reads "other: Finder +3".
    static func rowLabel(_ row: StretchRow, lookup: (String) -> String?) -> String {
        let first = keyLabel(row.keys.first ?? "", lookup: lookup)
        guard row.isOther else { return first }
        return row.keys.count > 1 ? "other: \(first) +\(row.keys.count - 1)" : "other: \(first)"
    }

    /// The picker's targets from meta.picker (["project", "project/sub", …]), without empty or
    /// "unattributed" entries; a missing, unreadable or empty picker (the brain has not run yet)
    /// falls back to the projects in meta.projects.
    static func pickerTargets(picker: String?, projects: String?) -> [StretchTarget] {
        if let p = picker, let data = p.data(using: .utf8),
           let arr = (try? JSONSerialization.jsonObject(with: data)) as? [String] {
            let targets: [StretchTarget] = arr.compactMap { s in
                let parts = s.split(separator: "/", maxSplits: 1).map(String.init)
                guard let project = parts.first, project != unattributed else { return nil }
                return .project(project, sub: parts.count > 1 ? parts[1] : nil)
            }
            if !targets.isEmpty { return targets }
        }
        return (projects ?? "").split(separator: ",").map { .project(String($0), sub: nil) }
    }

    /// "personal-dev / budget-tracker", "phd", "Leave unattributed"
    static func targetTitle(_ t: StretchTarget) -> String {
        switch t {
        case .leave: return leaveTitle
        case .project(let p, let sub): return sub.map { "\(p) / \($0)" } ?? p
        }
    }

    /// The declared projects among the targets, in order: the New sub-project sheet's parents.
    static func parentProjects(_ targets: [StretchTarget]) -> [String] {
        targets.compactMap { if case .project(let p, nil) = $0 { return p } else { return nil } }
    }

    /// The parent's sub-projects among the targets (the names a new one is checked against).
    static func knownSubprojects(_ targets: [StretchTarget], parent: String) -> [String] {
        targets.compactMap { if case .project(parent, let sub?) = $0 { return sub } else { return nil } }
    }

    /// The targets with parent / name inserted after the parent's last entry.
    static func addingSubproject(_ targets: [StretchTarget], parent: String, name: String) -> [StretchTarget] {
        var out = targets
        let last = out.lastIndex { if case .project(parent, _) = $0 { return true } else { return false } }
        out.insert(.project(parent, sub: name), at: last.map { $0 + 1 } ?? out.endIndex)
        return out
    }

    /// nil when name can become a new sub-project beside known, else why not. A name that is a
    /// "-"-prefix of a known one, or extends one with "-", would be merged with it by the weekly page.
    static func subprojectNameError(_ name: String, known: [String]) -> String? {
        if name.range(of: #"^[a-z0-9][a-z0-9-]{0,39}$"#, options: .regularExpression) == nil {
            return "use 1–40 of a–z, 0–9 and -, starting with a letter or digit"
        }
        if name == unattributed { return "\"unattributed\" is reserved" }
        if known.contains(name) { return "\(name) already exists" }
        if let k = known.first(where: { $0.hasPrefix(name + "-") || name.hasPrefix($0 + "-") }) {
            return "too close to \(k): the weekly page would merge the two"
        }
        return nil
    }

    /// A row follows "Attribute all to" until it is changed by hand.
    static func effectiveTarget(choice: StretchTarget?, preset: StretchTarget?) -> StretchTarget? {
        choice ?? preset
    }

    /// Save needs a value on every row; a panel with no rows saves at once.
    static func saveEnabled(_ values: [StretchTarget?]) -> Bool {
        values.allSatisfy { $0 != nil }
    }

    /// "always" needs a project value, and never applies to the "other" row or a no_always key.
    static func alwaysEnabled(_ row: StretchRow, value: StretchTarget?) -> Bool {
        guard !row.isOther, !row.noAlways, case .project? = value else { return false }
        return true
    }

    /// The answer JSON: one entry per key, the "other" row expanded; "always" entries
    /// carry the row's label. nil while a row has no value, or when values, always and labels are
    /// not one per row.
    static func answerJSON(rows: [StretchRow], values: [StretchTarget?], always: [Bool], labels: [String]) -> String? {
        guard values.count == rows.count, always.count == rows.count, labels.count == rows.count else { return nil }
        var out: [[String: Any]] = []
        for (i, row) in rows.enumerated() {
            guard let v = values[i] else { return nil }
            for key in row.keys {
                var e: [String: Any] = ["key": key]
                switch v {
                case .leave:
                    e["project"] = unattributed
                case .project(let p, let sub):
                    e["project"] = p
                    if let s = sub { e["subproject"] = s }
                    if always[i] && alwaysEnabled(row, value: v) {
                        e["always"] = true
                        e["label"] = labels[i]
                    }
                }
                out.append(e)
            }
        }
        let data = try! JSONSerialization.data(withJSONObject: out, options: [.sortedKeys, .withoutEscapingSlashes])
        return String(decoding: data, as: UTF8.self)
    }

    /// "Unattributed · Mon 11:57–12:54 · 47 min" (the minutes are unattributed_s).
    static func stretchMenuTitle(from: Int, to: Int, unattributedS: Int, tz: TimeZone = systemZone) -> String {
        "Unattributed · \(dayText("EEE", from, tz: tz)) \(clockRange(from: from, to: to, tz: tz)) · \(minutesText(unattributedS))"
    }

    /// "Mon 5 Oct · 11:57–12:54 · 47 min unattributed"
    static func stretchRange(from: Int, to: Int, unattributedS: Int, tz: TimeZone = systemZone) -> String {
        "\(dayText("EEE d MMM", from, tz: tz)) · \(clockRange(from: from, to: to, tz: tz)) · \(minutesText(unattributedS)) unattributed"
    }

    /// Pending calls and stretches as one list by start time (a call first on a tie).
    static func mergedPending(calls: [PendingCall], stretches: [PendingStretch]) -> [PendingID] {
        let ids = calls.map { PendingID(kind: .call, fromTs: $0.fromTs) } + stretches.map { PendingID(kind: .stretch, fromTs: $0.fromTs) }
        return ids.sorted { $0.fromTs != $1.fromTs ? $0.fromTs < $1.fromTs : ($0.kind == .call && $1.kind == .stretch) }
    }

    /// One refresh of the auto-open bookkeeping: `shown` keeps only the ids still pending, so a row that
    /// leaves the pending set and returns (answered, then re-opened) pops again; Later keeps a row pending,
    /// so it does not. Returns the new set and the newly pending ids to queue, in order.
    static func autoOpen(shown: Set<PendingID>, pending: [PendingID]) -> (shown: Set<PendingID>, queue: [PendingID]) {
        let queue = pending.filter { !shown.contains($0) }
        return (shown.intersection(pending).union(queue), queue)
    }

    /// The collector's own "in a call": its latest sample has a mic holder. It cannot read the brain's
    /// call_apps allowlist, so a mic test or dictation counts too; that only delays a stretch pop-up.
    static func inCall(_ latest: Sample?) -> Bool {
        !(latest?.callApp ?? "").isEmpty
    }

    /// The next panel to open and the queue left behind: ids no longer pending are dropped; while in a
    /// call, stretch ids are kept but skipped, so a stretch pops after the call.
    /// `clicked` holds menu clicks queued behind an open panel: menu clicks are never deferred.
    static func nextToPresent(queue: [PendingID], pending: Set<PendingID>, inCall: Bool, clicked: Set<PendingID> = []) -> (present: PendingID?, queue: [PendingID]) {
        var chosen: PendingID?
        var rest: [PendingID] = []
        for id in queue where pending.contains(id) {
            let deferred = inCall && id.kind == .stretch && !clicked.contains(id)
            if chosen == nil && !deferred { chosen = id } else { rest.append(id) }
        }
        return (chosen, rest)
    }

    // MARK: --dump

    static func sampleJSON(_ s: Sample) -> String {
        var obj: [String: Any] = ["ts": s.ts, "kind": s.kind]
        obj["app"] = s.app ?? NSNull()
        obj["title"] = s.title ?? NSNull()
        obj["url"] = s.url ?? NSNull()
        obj["cwd"] = s.cwd ?? NSNull()
        obj["idle_s"] = s.idleS ?? NSNull()
        obj["call_app"] = s.callApp ?? NSNull()
        let data = try! JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys])
        return String(decoding: data, as: UTF8.self)
    }
}

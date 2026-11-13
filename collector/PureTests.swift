import Foundation
import SQLite3

// Minimal assertion runner: no XCTest, no Xcode. Exit code 1 on any failure.
// Compiled with -parse-as-library so this file may live next to main.swift without clashing.
@main
enum PureTests {
    static var failures = 0
    static var checks = 0

    static func expect<T: Equatable>(_ actual: T, _ expected: T, _ label: String, line: Int = #line) {
        checks += 1
        if actual != expected {
            failures += 1
            print("FAIL \(label) (line \(line)): got \(String(describing: actual)), expected \(String(describing: expected))")
        }
    }

    static func main() {

        // --- normalizeTitle
        expect(Pure.normalizeTitle("(3) Slack | acme"), "Slack | acme", "strip unread counter")
        expect(Pure.normalizeTitle("(12) (1) Inbox"), "Inbox", "strip repeated counters")
        expect(Pure.normalizeTitle("Zoom Meeting 01:23:45"), "Zoom Meeting", "strip hh:mm:ss")
        expect(Pure.normalizeTitle("Meet - 12:34"), "Meet", "strip mm:ss with dash")
        expect(Pure.normalizeTitle("Timer: 05:00"), "Timer", "strip with colon separator")
        expect(Pure.normalizeTitle("Report 2026"), "Report 2026", "leave plain title alone")
        expect(Pure.normalizeTitle("12:34"), "12:34", "lone timer is kept")
        expect(Pure.normalizeTitle("  "), nil, "blank -> nil")
        expect(Pure.normalizeTitle(nil), nil, "nil -> nil")
        expect(Pure.normalizeTitle("(2) "), nil, "counter only -> nil")

        // --- writeKind
        let base = Sample(ts: 1000, kind: "sample", app: "com.mitchellh.ghostty", title: "zsh", url: nil, cwd: "/Users/a/dev", idleS: 3, callApp: nil)
        expect(Pure.writeKind(lastWritten: nil, current: base), "sample", "first sample always written")
        expect(Pure.writeKind(lastWritten: base, current: base), nil, "identical -> nothing")
        var t = base; t.ts = 1005; t.title = "vim"
        expect(Pure.writeKind(lastWritten: base, current: t), "sample", "title change")
        var u = base; u.ts = 1005; u.url = "https://x"
        expect(Pure.writeKind(lastWritten: base, current: u), "sample", "url change")
        var c = base; c.ts = 1005; c.cwd = nil
        expect(Pure.writeKind(lastWritten: base, current: c), "sample", "cwd change to nil")
        var k = base; k.ts = 1005; k.callApp = "us.zoom.xos"
        expect(Pure.writeKind(lastWritten: base, current: k), "sample", "call_app change")
        var i1 = base; i1.ts = 1005; i1.idleS = 299
        expect(Pure.writeKind(lastWritten: base, current: i1), nil, "idle below threshold is not a change")
        var i2 = base; i2.ts = 1005; i2.idleS = 300
        expect(Pure.writeKind(lastWritten: base, current: i2), "sample", "idle crossing 300 up")
        var i3 = i2; i3.ts = 1010; i3.idleS = 2
        expect(Pure.writeKind(lastWritten: i2, current: i3), "sample", "idle crossing back down")
        var i4 = i2; i4.ts = 1010; i4.idleS = 305
        expect(Pure.writeKind(lastWritten: i2, current: i4), nil, "still idle, no heartbeat yet")
        var h = base; h.ts = 1060
        expect(Pure.writeKind(lastWritten: base, current: h), "heartbeat", "60 s -> heartbeat")
        var h2 = base; h2.ts = 1059
        expect(Pure.writeKind(lastWritten: base, current: h2), nil, "59 s -> no heartbeat")
        var wake = base; wake.kind = "wake"; wake.ts = 1005
        var afterWake = base; afterWake.ts = 1010
        expect(Pure.writeKind(lastWritten: wake, current: afterWake), "sample", "wake -> next tick writes sample")

        // --- newestCwd
        let d0 = Date(timeIntervalSince1970: 100), d1 = Date(timeIntervalSince1970: 200)
        expect(Pure.newestCwd([]), nil, "no files -> nil")
        expect(Pure.newestCwd([
            Pure.CwdFile(name: "ttys001", mtime: d0, contents: "/old\n"),
            Pure.CwdFile(name: "ttys002", mtime: d1, contents: "/new\n"),
        ]), "/new", "newest mtime wins, trimmed")
        expect(Pure.newestCwd([
            Pure.CwdFile(name: "ttys001", mtime: d1, contents: "/a"),
            Pure.CwdFile(name: "ttys002", mtime: d1, contents: "/b"),
        ]), "/b", "tie -> greatest name")
        expect(Pure.newestCwd([
            Pure.CwdFile(name: "ttys001", mtime: d0, contents: "/old"),
            Pure.CwdFile(name: "ttys002", mtime: d1, contents: "  "),
        ]), "/old", "blank newest is skipped")

        // --- herdr protocol
        expect(String(decoding: Pure.herdrRequest(id: "stint-1"), as: UTF8.self),
               "{\"id\":\"stint-1\",\"method\":\"pane.current\",\"params\":{}}\n", "request wire format")
        let ok = #"{"id":"stint-1","result":{"type":"pane_current","pane":{"pane_id":"wH:p24","focused":true,"cwd":"/Users/a/x","foreground_cwd":"/Users/a/x/sub"}}}"#
        expect(Pure.herdrForegroundCwd(Data(ok.utf8)), "/Users/a/x/sub", "foreground_cwd preferred")
        let noFg = #"{"id":"1","result":{"type":"pane_current","pane":{"focused":true,"cwd":"/Users/a/x","foreground_cwd":""}}}"#
        expect(Pure.herdrForegroundCwd(Data(noFg.utf8)), "/Users/a/x", "empty foreground_cwd falls back to cwd")
        let err = #"{"id":"","error":{"code":"invalid_request","message":"missing field"}}"#
        expect(Pure.herdrForegroundCwd(Data(err.utf8)), nil, "error response -> nil")
        expect(Pure.herdrForegroundCwd(Data("garbage".utf8)), nil, "garbage -> nil")
        let titled = #"{"id":"1","result":{"type":"pane_current","pane":{"focused":true,"cwd":"/x","terminal_title":"✳ Release flow","terminal_title_stripped":"Release flow"}}}"#
        expect(Pure.herdrPaneTitle(Data(titled.utf8)), "Release flow", "terminal_title_stripped preferred")
        let rawTitle = #"{"id":"1","result":{"type":"pane_current","pane":{"focused":true,"cwd":"/x","terminal_title":"✳ Release flow","terminal_title_stripped":""}}}"#
        expect(Pure.herdrPaneTitle(Data(rawTitle.utf8)), "✳ Release flow", "empty stripped falls back to terminal_title")
        expect(Pure.herdrPaneTitle(Data(ok.utf8)), nil, "no title fields -> nil")
        expect(Pure.herdrPaneTitle(Data(err.utf8)), nil, "title: error response -> nil")
        expect(Pure.herdrPaneTitle(Data("garbage".utf8)), nil, "title: garbage -> nil")

        // --- remote input (herdr client tty atime) vs HID idle
        expect(Pure.effectiveIdle(hidIdle: 3600, remoteAge: 12), 12, "remote fresher -> remote age")
        expect(Pure.effectiveIdle(hidIdle: 4, remoteAge: 900), 4, "hid fresher -> hid idle")
        expect(Pure.effectiveIdle(hidIdle: 4, remoteAge: nil), 4, "no remote -> hid idle")
        expect(Pure.effectiveIdle(hidIdle: nil, remoteAge: 7), 7, "no hid -> remote age")
        expect(Pure.effectiveIdle(hidIdle: nil, remoteAge: nil), nil, "neither -> nil")
        let hosts: Set<String> = ["com.mitchellh.ghostty"]
        let lock = "com.apple.loginwindow"
        expect(Pure.remoteDecision(hidIdle: 3600, remoteAge: 12, app: lock, herdrHosts: hosts), true, "locked Mac, fresh remote keystroke -> remote")
        expect(Pure.remoteDecision(hidIdle: 3600, remoteAge: nil, app: lock, herdrHosts: hosts), false, "nil remoteAge -> not remote")
        expect(Pure.remoteDecision(hidIdle: 12, remoteAge: 12, app: lock, herdrHosts: hosts), false, "tie -> not remote")
        expect(Pure.remoteDecision(hidIdle: 5, remoteAge: 12, app: lock, herdrHosts: hosts), false, "hid fresher -> not remote")
        expect(Pure.remoteDecision(hidIdle: 3600, remoteAge: 300, app: lock, herdrHosts: hosts), false, "remote fresher but at threshold -> not remote")
        expect(Pure.remoteDecision(hidIdle: 3600, remoteAge: 299, app: lock, herdrHosts: hosts), true, "just under threshold -> remote")
        expect(Pure.remoteDecision(hidIdle: 3600, remoteAge: 12, app: "com.mitchellh.ghostty", herdrHosts: hosts), false, "herdr host frontmost -> not remote")
        expect(Pure.remoteDecision(hidIdle: 3600, remoteAge: 12, app: nil, herdrHosts: hosts), true, "no frontmost app -> remote")
        expect(Pure.remoteDecision(hidIdle: nil, remoteAge: 12, app: lock, herdrHosts: hosts), true, "no hid reading -> remote signal decides")

        // --- dates
        let tz = TimeZone(identifier: "Asia/Jerusalem")!
        // 2026-09-16 is a Wednesday; week starts Sunday 2026-09-13
        let wed = Date(timeIntervalSince1970: 1789549200) // 2026-09-16 09:00 UTC
        expect(Pure.dayString(wed, tz: tz), "2026-09-16", "dayString")
        expect(Pure.weekStartDay(wed, tz: tz), "2026-09-13", "week starts on Sunday")
        // Saturday 2026-09-19 23:30 Jerusalem (UTC+3) is still the same week; Sunday 00:30 is the next
        let satLate = Date(timeIntervalSince1970: 1789849800) // 2026-09-19 20:30 UTC = Sat 23:30 IDT
        expect(Pure.weekStartDay(satLate, tz: tz), "2026-09-13", "Saturday night still same week")
        let sunEarly = Date(timeIntervalSince1970: 1789853400) // 2026-09-19 21:30 UTC = Sun 00:30 IDT
        expect(Pure.weekStartDay(sunEarly, tz: tz), "2026-09-20", "Sunday 00:30 -> next week")
        expect(Pure.dayString(sunEarly, tz: tz), "2026-09-20", "day rolls at Jerusalem midnight, not UTC")
        // The default zone follows a system zone change while the app runs (setenv + reset stands in
        // for the system's change notification), not the zone it launched in.
        let savedTZ = ProcessInfo.processInfo.environment["TZ"]
        setenv("TZ", "Asia/Tokyo", 1); NSTimeZone.resetSystemTimeZone()
        expect(Pure.dayString(sunEarly), "2026-09-20", "default zone follows a zone change (Tokyo)")
        setenv("TZ", "America/Los_Angeles", 1); NSTimeZone.resetSystemTimeZone()
        expect(Pure.dayString(sunEarly), "2026-09-19", "default zone follows a zone change (Los Angeles)")
        if let savedTZ { setenv("TZ", savedTZ, 1) } else { unsetenv("TZ") }
        NSTimeZone.resetSystemTimeZone()

        // --- week start from meta.week_start
        expect(Pure.firstWeekday("sunday"), 1, "meta week_start sunday")
        expect(Pure.firstWeekday("monday"), 2, "meta week_start monday")
        expect(Pure.firstWeekday("saturday"), 7, "meta week_start saturday")
        expect(Pure.firstWeekday(nil), 1, "missing meta week_start -> Sunday")
        expect(Pure.firstWeekday("funday"), 1, "unknown meta week_start -> Sunday")
        let mon = Pure.firstWeekday("monday")
        expect(Pure.weekStartDay(wed, firstWeekday: mon, tz: tz), "2026-09-14", "Monday start: Wednesday")
        let monEarly = Date(timeIntervalSince1970: 1789335000) // Mon 2026-09-14 00:30 IDT
        expect(Pure.weekStartDay(monEarly, firstWeekday: mon, tz: tz), "2026-09-14", "Monday start: Monday 00:30 opens the week")
        let sunLate = Date(timeIntervalSince1970: 1789331400) // Sun 2026-09-13 23:30 IDT
        expect(Pure.weekStartDay(sunLate, firstWeekday: mon, tz: tz), "2026-09-07", "Monday start: Sunday night is the previous week")
        let sat = Pure.firstWeekday("saturday")
        expect(Pure.weekStartDay(wed, firstWeekday: sat, tz: tz), "2026-09-12", "Saturday start: Wednesday")
        expect(Pure.weekStartDay(satLate, firstWeekday: sat, tz: tz), "2026-09-19", "Saturday start: Saturday opens the week")
        let friLate = Date(timeIntervalSince1970: 1789763400) // Fri 2026-09-18 23:30 IDT
        expect(Pure.weekStartDay(friLate, firstWeekday: sat, tz: tz), "2026-09-12", "Saturday start: Friday night is the previous week")

        // --- rollup / formatting
        expect(Pure.topLevelProject("acme/acme-app"), "acme", "rollup key")
        expect(Pure.topLevelProject("phd"), "phd", "flat project unchanged")
        let rolled = Pure.rollup([
            TotalRow(project: "personal-dev/stint", daySeconds: 100, weekSeconds: 1000),
            TotalRow(project: "acme/acme-app", daySeconds: 200, weekSeconds: 5000),
            TotalRow(project: "acme", daySeconds: 50, weekSeconds: 500),
            TotalRow(project: "personal-dev/garden", daySeconds: 10, weekSeconds: 6000),
        ])
        expect(rolled, [
            TotalRow(project: "personal-dev", daySeconds: 110, weekSeconds: 7000),
            TotalRow(project: "acme", daySeconds: 250, weekSeconds: 5500),
        ], "rollup sums and sorts by week desc")
        expect(Pure.formatHours(7530), "2:05", "formatHours")
        expect(Pure.formatHours(0), "0:00", "formatHours zero")
        expect(Pure.formatHours(-5), "0:00", "formatHours clamps negatives")

        // --- open interval / health
        expect(Pure.openIntervalSeconds(now: 2000, lastTotalsUpdate: 1700, idleS: 4, lastKind: "sample"), 300, "open interval since last totals write")
        expect(Pure.openIntervalSeconds(now: 2000, lastTotalsUpdate: nil, idleS: 4, lastKind: "sample"), 0, "no totals -> 0")
        expect(Pure.openIntervalSeconds(now: 2000, lastTotalsUpdate: 1700, idleS: 400, lastKind: "sample"), 0, "idle -> 0")
        expect(Pure.openIntervalSeconds(now: 2000, lastTotalsUpdate: 1700, idleS: 4, lastKind: "sleep"), 0, "after sleep boundary -> 0")
        expect(Pure.openIntervalSeconds(now: 2000, lastTotalsUpdate: 1700, idleS: 4, lastKind: "wake"), 0, "after wake boundary -> 0")
        expect(Pure.openIntervalSeconds(now: 9000, lastTotalsUpdate: 1700, idleS: 4, lastKind: "sample"), 900, "capped at 15 min")
        expect(Pure.healthWarning(health: "ok", healthAt: 1000, now: 1500), false, "fresh ok")
        expect(Pure.healthWarning(health: "ok", healthAt: 1000, now: 2900), true, "stale > 30 min")
        expect(Pure.healthWarning(health: "url NULL for 1h", healthAt: 1400, now: 1500), true, "not ok")
        expect(Pure.healthWarning(health: nil, healthAt: nil, now: 1500), true, "never written -> warn")

        // --- call_app list
        expect(Pure.joinCallApps([]), nil, "no mic holders -> nil")
        expect(Pure.joinCallApps(["com.google.Chrome.helper", "com.google.Chrome", "us.zoom.xos"]), "com.google.Chrome,us.zoom.xos", "helper suffix stripped, deduped, order kept")
        expect(Pure.joinCallApps(["com.bookry.wavebox.Helper.renderer", ""]), "com.bookry.wavebox", "nested helper suffix and empties")
        expect(Pure.normalizeCallApp("com.foo.helperapp.xyz"), "com.foo.helperapp.xyz", "helperapp (not .helper) unchanged")
        expect(Pure.normalizeCallApp("com.google.Chrome.helper.renderer"), "com.google.Chrome", ".helper.renderer stripped")

        // --- --dump JSON
        let js = Pure.sampleJSON(Sample(ts: 1, kind: "sample", app: "a", title: nil, url: nil, cwd: "/x", idleS: 2, callApp: nil))
        expect(js, #"{"app":"a","call_app":null,"cwd":"\/x","idle_s":2,"kind":"sample","title":null,"ts":1,"url":null}"#, "dump JSON shape")

        // --- meeting pop-up: breakdown parsing, tolerant of missing keys and garbage
        let bd = Pure.parseBreakdown(#"[{"project":"personal-dev","subproject":"stint","seconds":1860},{"project":"acme","seconds":840},{"project":"meeting","seconds":720,"extra":1}]"#)
        expect(bd, [BreakdownLine(project: "personal-dev", subproject: "stint", seconds: 1860),
                    BreakdownLine(project: "acme", subproject: nil, seconds: 840),
                    BreakdownLine(project: "meeting", subproject: nil, seconds: 720)], "breakdown parses; missing subproject = nil; unknown keys ignored")
        expect(Pure.parseBreakdown("not json"), [], "garbage -> empty")
        expect(Pure.parseBreakdown(#"[{"seconds":5},{"project":"x"}]"#), [BreakdownLine(project: "x", subproject: nil, seconds: 0)], "entries without a project are dropped; missing seconds = 0")
        expect(Pure.minutesText(1860), "31 min", "minutes")
        expect(Pure.minutesText(3429), "57 min", "rounds to nearest minute")
        expect(Pure.minutesText(3900), "1 h 05 min", "over an hour")
        expect(Pure.breakdownLines(bd, guess: "acme"),
               ["personal-dev/stint  31 min", "acme  14 min", "meeting / unmatched  12 min → acme"], "display lines")
        expect(Pure.splitEnabled(bd), true, "split enabled with a non-meeting line")
        expect(Pure.splitEnabled([BreakdownLine(project: "meeting", subproject: nil, seconds: 720)]), false, "meeting-only breakdown disables split")
        expect(Pure.splitEnabled([]), false, "empty breakdown disables split")

        // --- meeting pop-up: text
        expect(Pure.callRange(from: 1790179286, to: 1790182690, tz: tz), "Wed 23 Sep · 19:01–19:58 · 57 min", "range line (Asia/Jerusalem)")
        expect(Pure.callMenuTitle(app: "Zoom", from: 1790179286, to: 1790182690, tz: tz), "Zoom · 19:01–19:58 · 57 min", "menu row")
        expect(Pure.callAppNames("us.zoom.xos,com.tinyspeck.slackmacgap", lookup: { $0 == "us.zoom.xos" ? "Zoom" : nil }), "Zoom + com.tinyspeck.slackmacgap", "names joined, unknown falls back to the id")
        expect(Pure.callAppNames("", lookup: { _ in nil }), "call", "empty list")

        // --- meeting pop-up: picker and decision, unattributed guess
        let callPicker = #"["acme","acme/acme-app","freelance","freelance/globex","phd"]"#
        let listed = Pure.pickerItems(picker: callPicker, projects: "acme,freelance,phd", guess: "acme", guessSub: "acme-app")
        expect(listed, [.project("acme", sub: nil), .project("acme", sub: "acme-app"), .project("freelance", sub: nil),
                        .project("freelance", sub: "globex"), .project("phd", sub: nil)], "picker from meta.picker; guess sub-project already listed")
        expect(Pure.pickerItems(picker: callPicker, projects: nil, guess: "acme", guessSub: "automations").last, .project("acme", sub: "automations"), "guess sub-project not in the list is appended")
        expect(Pure.pickerItems(picker: nil, projects: "acme,phd", guess: "old-client", guessSub: nil), [.project("acme", sub: nil), .project("phd", sub: nil), .project("old-client", sub: nil)], "no meta.picker -> meta.projects, guess appended")
        expect(Pure.pickerItems(picker: nil, projects: nil, guess: "acme", guessSub: nil), [.project("acme", sub: nil)], "no meta.picker or meta.projects -> the guess only")
        expect(Pure.pickerItems(picker: callPicker, projects: nil, guess: "unattributed", guessSub: nil).count, 5, "unattributed never enters the picker")
        expect(Pure.pickerItems(picker: nil, projects: nil, guess: "unattributed", guessSub: nil), [], "unattributed with no list -> nothing to pick")
        expect(Pure.decision(chosen: listed[1], choice: .whole), .project("acme", subproject: "acme-app"), "unchanged guess keeps its sub-project")
        expect(Pure.decision(chosen: listed[3], choice: .whole), .project("freelance", subproject: "globex"), "project/sub-project item books both")
        expect(Pure.decision(chosen: listed[0], choice: .whole), .project("acme", subproject: nil), "top-level item books no sub-project, even the guess's project")
        expect(Pure.decision(chosen: nil, choice: .split), .split, "split")
        expect(Pure.decision(chosen: nil, choice: .whole), nil, "no pick, no split -> nothing to save")
        expect(Pure.decision(chosen: .project("unattributed", sub: nil), choice: .whole), nil, "unattributed is never a valid choice")
        expect(Pure.decisionBinding(.split), DecisionBinding(decision: "split", project: nil, subproject: nil), "split binding")
        expect(Pure.decisionBinding(.project("phd", subproject: nil)), DecisionBinding(decision: "project", project: "phd", subproject: nil), "project binding")
        expect(Pure.decision(chosen: nil, choice: .background), .background, "background saves with nothing picked")
        expect(Pure.decisionBinding(.background), DecisionBinding(decision: "background", project: nil, subproject: nil), "background binding")
        let mixed = #"[{"project":"acme","subproject":"acme-app","seconds":17241},{"background":"acme/acme-app","seconds":7500},{"background":"unattributed","seconds":90},{"background":"away","seconds":10200}]"#
        expect(Pure.parseBreakdown(mixed), [BreakdownLine(project: "acme", subproject: "acme-app", seconds: 17241)], "split lines ignore background entries")
        expect(Pure.parseBackground(mixed), [BackgroundLine(label: "acme/acme-app", seconds: 7500), BackgroundLine(label: "unattributed", seconds: 90), BackgroundLine(label: "away", seconds: 10200)], "background entries parse in order")
        expect(Pure.parseBackground(#"[{"project":"acme","seconds":60}]"#), nil, "old breakdown: no background numbers")
        expect(Pure.backgroundLines(Pure.parseBackground(mixed)!), ["acme/acme-app  2 h 05 min", "unattributed  2 min", "away, not counted  2 h 50 min"], "background line text")

        // --- stretch pop-up: breakdown -> rows
        let sk = Pure.parseStretchBreakdown(#"[{"key":"host:grok.com","seconds":2040},{"key":"app:net.whatsapp.WhatsApp","seconds":600},{"key":"app:com.google.Chrome","seconds":90,"no_always":true},{"seconds":5},{"key":"app:com.apple.finder","seconds":40,"x":1}]"#)
        expect(sk, [StretchKey(key: "host:grok.com", seconds: 2040, noAlways: false),
                    StretchKey(key: "app:net.whatsapp.WhatsApp", seconds: 600, noAlways: false),
                    StretchKey(key: "app:com.google.Chrome", seconds: 90, noAlways: true),
                    StretchKey(key: "app:com.apple.finder", seconds: 40, noAlways: false)], "breakdown parses; no key dropped; unknown keys ignored")
        expect(Pure.parseStretchBreakdown("garbage"), [], "garbage -> no keys")
        let rows = Pure.stretchRows(sk)
        expect(rows, [StretchRow(keys: ["host:grok.com"], seconds: 2040, isOther: false, noAlways: false),
                      StretchRow(keys: ["app:net.whatsapp.WhatsApp"], seconds: 600, isOther: false, noAlways: false),
                      StretchRow(keys: ["app:com.google.Chrome"], seconds: 90, isOther: false, noAlways: true),
                      StretchRow(keys: ["app:com.apple.finder"], seconds: 40, isOther: true, noAlways: true)], "keys under 60 s fold into other")
        let many = (1...10).map { StretchKey(key: "app:k\($0)", seconds: 1000 - $0, noAlways: false) }
        let manyRows = Pure.stretchRows(many)
        expect(manyRows.count, 9, "at most 8 rows plus other")
        expect(manyRows.last, StretchRow(keys: ["app:k9", "app:k10"], seconds: 991 + 990, isOther: true, noAlways: true), "surplus keys fold into other")
        expect(Pure.stretchRows([]), [], "no keys, no rows")
        let names: (String) -> String? = { $0 == "com.apple.finder" ? "Finder" : ($0 == "net.whatsapp.WhatsApp" ? "WhatsApp" : nil) }
        expect(Pure.rowLabel(rows[0], lookup: names), "grok.com", "host label as-is")
        expect(Pure.rowLabel(rows[1], lookup: names), "WhatsApp", "app label via lookup")
        expect(Pure.rowLabel(rows[2], lookup: names), "com.google.Chrome", "unknown app falls back to the id")
        expect(Pure.rowLabel(rows[3], lookup: names), "other: Finder", "other with one key")
        expect(Pure.rowLabel(manyRows[8], lookup: names), "other: k9 +1", "other: first label and how many more")

        // --- stretch pop-up: picker, preset, always, Save
        let targets = Pure.pickerTargets(picker: #"["acme","acme/automations","personal-dev","personal-dev/budget-tracker","phd"]"#, projects: "acme,phd")
        expect(targets, [.project("acme", sub: nil), .project("acme", sub: "automations"), .project("personal-dev", sub: nil),
                         .project("personal-dev", sub: "budget-tracker"), .project("phd", sub: nil)], "picker from meta.picker")
        expect(Pure.pickerTargets(picker: nil, projects: "acme,phd"), [.project("acme", sub: nil), .project("phd", sub: nil)], "no meta.picker -> meta.projects")
        expect(Pure.pickerTargets(picker: "not json", projects: nil), [], "unreadable picker and no projects -> nothing")
        expect(Pure.pickerTargets(picker: "[]", projects: "acme,phd"), [.project("acme", sub: nil), .project("phd", sub: nil)], "empty meta.picker -> meta.projects")
        expect(Pure.pickerTargets(picker: #"["unattributed","","unattributed/x","phd"]"#, projects: "acme"), [.project("phd", sub: nil)], "unattributed and empty entries dropped")
        expect(Pure.pickerTargets(picker: #"["unattributed",""]"#, projects: "acme"), [.project("acme", sub: nil)], "nothing usable in meta.picker -> meta.projects")
        expect(Pure.targetTitle(.project("personal-dev", sub: "budget-tracker")), "personal-dev / budget-tracker", "sub-project title")
        expect(Pure.targetTitle(.leave), "Leave unattributed", "leave title")
        expect(Pure.parentProjects(targets), ["acme", "personal-dev", "phd"], "parents for the sheet")
        expect(Pure.knownSubprojects(targets, parent: "personal-dev"), ["budget-tracker"], "known sub-projects of a parent")
        expect(Pure.addingSubproject(targets, parent: "acme", name: "zz"),
               [.project("acme", sub: nil), .project("acme", sub: "automations"), .project("acme", sub: "zz"), .project("personal-dev", sub: nil),
                .project("personal-dev", sub: "budget-tracker"), .project("phd", sub: nil)], "a new sub-project lands after its parent's entries")
        let pd: StretchTarget = .project("personal-dev", sub: nil)
        let phd: StretchTarget = .project("phd", sub: nil)
        expect(Pure.effectiveTarget(choice: nil, preset: nil), nil, "(all) row has no value while the preset is empty")
        expect(Pure.effectiveTarget(choice: nil, preset: pd), pd, "(all) row follows the preset")
        expect(Pure.effectiveTarget(choice: phd, preset: pd), phd, "a row changed by hand keeps its value when the preset changes")
        expect(Pure.saveEnabled([pd, nil]), false, "Save waits for every row")
        expect(Pure.saveEnabled([pd, .leave]), true, "every row has a value")
        expect(Pure.saveEnabled([]), true, "a panel with no rows saves at once")
        expect(Pure.alwaysEnabled(rows[0], value: pd), true, "always on a host row with a project")
        expect(Pure.alwaysEnabled(rows[0], value: .leave), false, "never for Leave")
        expect(Pure.alwaysEnabled(rows[0], value: nil), false, "never without a value")
        expect(Pure.alwaysEnabled(rows[2], value: pd), false, "never for a no_always key")
        expect(Pure.alwaysEnabled(rows[3], value: pd), false, "never for the other row")

        // --- stretch pop-up: answer JSON (other expanded, label only on always entries)
        let answerRows = [rows[0], rows[1], StretchRow(keys: ["app:a", "app:b"], seconds: 90, isOther: true, noAlways: true)]
        expect(Pure.answerJSON(rows: answerRows, values: [.project("personal-dev", sub: "budget-tracker"), .leave, phd],
                               always: [true, true, true], labels: ["grok.com", "WhatsApp", "other: a +1"]),
               #"[{"always":true,"key":"host:grok.com","label":"grok.com","project":"personal-dev","subproject":"budget-tracker"},{"key":"app:net.whatsapp.WhatsApp","project":"unattributed"},{"key":"app:a","project":"phd"},{"key":"app:b","project":"phd"}]"#,
               "answer JSON")
        expect(Pure.answerJSON(rows: answerRows, values: [pd, nil, phd], always: [false, false, false], labels: ["", "", ""]), nil, "no JSON while a row has no value")
        expect(Pure.answerJSON(rows: [], values: [], always: [], labels: []), "[]", "no rows -> []")
        expect(Pure.answerJSON(rows: answerRows, values: [pd, pd, phd], always: [true, true], labels: ["", "", ""]), nil, "always shorter than rows -> nil")
        expect(Pure.answerJSON(rows: answerRows, values: [pd, pd, phd], always: [true, true, true], labels: [""]), nil, "labels shorter than rows -> nil")
        expect(Pure.answerJSON(rows: answerRows, values: [pd], always: [false, false, false], labels: ["", "", ""]), nil, "values shorter than rows -> nil")
        expect(Pure.answerJSON(rows: answerRows, values: [pd, pd, phd, phd], always: [false, false, false], labels: ["", "", ""]), nil, "values longer than rows -> nil")

        // --- stretch pop-up: new sub-project names
        let known = ["budget-tracker", "stint"]
        expect(Pure.subprojectNameError("grok-bot", known: known), nil, "a fresh name is fine")
        expect(Pure.subprojectNameError("Grok Bot", known: known) != nil, true, "pattern: lower case, digits, dashes")
        expect(Pure.subprojectNameError("-x", known: known) != nil, true, "pattern: no leading dash")
        expect(Pure.subprojectNameError(String(repeating: "a", count: 41), known: known) != nil, true, "pattern: at most 40")
        expect(Pure.subprojectNameError("unattributed", known: known) != nil, true, "reserved")
        expect(Pure.subprojectNameError("stint", known: known) != nil, true, "existing")
        expect(Pure.subprojectNameError("budget", known: known) != nil, true, "a dash-prefix of a known name folds into it")
        expect(Pure.subprojectNameError("stint-app", known: known) != nil, true, "extending a known name with a dash folds into it")
        expect(Pure.subprojectNameError("stints", known: known), nil, "a plain prefix without a dash is fine")

        // --- stretch pop-up: menu text, merged ordering, auto-open
        expect(Pure.stretchMenuTitle(from: 1791190620, to: 1791194040, unattributedS: 2820, tz: tz), "Unattributed · Mon 11:57–12:54 · 47 min", "stretch menu row (Asia/Jerusalem)")
        expect(Pure.stretchRange(from: 1791190620, to: 1791194040, unattributedS: 2820, tz: tz), "Mon 5 Oct · 11:57–12:54 · 47 min unattributed", "panel subtitle")
        let call = PendingCall(fromTs: 200, toTs: 500, callApp: "us.zoom.xos", guessProject: "phd", guessSubproject: nil, breakdown: "[]")
        let st = PendingStretch(fromTs: 100, toTs: 900, unattributedS: 700, breakdown: "[]")
        let tie = PendingStretch(fromTs: 200, toTs: 900, unattributedS: 700, breakdown: "[]")
        expect(Pure.mergedPending(calls: [call], stretches: [tie, st]),
               [PendingID(kind: .stretch, fromTs: 100), PendingID(kind: .call, fromTs: 200), PendingID(kind: .stretch, fromTs: 200)], "one list by start time, call first on a tie")
        let a = PendingID(kind: .stretch, fromTs: 100), b = PendingID(kind: .call, fromTs: 200)
        let first = Pure.autoOpen(shown: [], pending: [a, b])
        expect(first.queue, [a, b], "new rows are queued in order")
        let later = Pure.autoOpen(shown: first.shown, pending: [a, b])
        expect(later.queue, [], "a row still pending (Later) does not pop again")
        let answered = Pure.autoOpen(shown: later.shown, pending: [b])
        expect(answered.shown, [b], "a row that left the pending set leaves shown")
        expect(Pure.autoOpen(shown: answered.shown, pending: [a, b]).queue, [a], "re-opened and pending again: it pops again")

        // --- stretch pop-up: no auto-open during a call; after it, stretch first, then the meeting
        expect(Pure.inCall(nil), false, "no sample yet: not in a call")
        expect(Pure.inCall(Sample(ts: 1, kind: "heartbeat", app: "a", title: nil, url: nil, cwd: nil, idleS: 0, callApp: nil)), false, "no mic holder")
        expect(Pure.inCall(Sample(ts: 1, kind: "heartbeat", app: "a", title: nil, url: nil, cwd: nil, idleS: 0, callApp: "com.apple.VoiceMemos")), true, "any mic holder counts (a mic test only delays the pop-up)")
        let endsAtCall = PendingStretch(fromTs: 100, toTs: 1200, unattributedS: 1100, breakdown: "[]")
        let meeting = PendingCall(fromTs: 1200, toTs: 1800, callApp: "us.zoom.xos", guessProject: "phd", guessSubproject: nil, breakdown: "[]")
        let sID = PendingID(kind: .stretch, fromTs: 100), cID = PendingID(kind: .call, fromTs: 1200)
        let during = Pure.nextToPresent(queue: Pure.autoOpen(shown: [], pending: [sID]).queue, pending: [sID], inCall: true)
        expect(during.present, nil, "a stretch does not open by itself during the call")
        expect(during.queue, [sID], "it stays queued")
        let both = Pure.mergedPending(calls: [meeting], stretches: [endsAtCall])
        expect(both, [sID, cID], "the stretch that ended at the call's start comes first")
        let after = Pure.nextToPresent(queue: during.queue + Pure.autoOpen(shown: [sID], pending: both).queue, pending: Set(both), inCall: false)
        expect(after.present, sID, "after the call: the stretch opens first")
        expect(after.queue, [cID], "the meeting waits its turn")
        let then = Pure.nextToPresent(queue: after.queue, pending: [cID], inCall: false)
        expect(then.present, cID, "then the meeting, once the stretch panel is done")
        expect(then.queue, [], "one at a time: nothing else queued")
        expect(Pure.nextToPresent(queue: [sID, cID], pending: [cID], inCall: true).present, cID, "an answered stretch is dropped; a call row is never deferred")
        let clicked = Pure.nextToPresent(queue: [sID, cID], pending: [sID, cID], inCall: true, clicked: [sID])
        expect(clicked.present, sID, "a stretch clicked in the menu while another panel was open is never deferred by a call")
        expect(clicked.queue, [cID], "the rest stays queued")

        // --- schema SQL applies to a real in-memory SQLite, twice (idempotent)
        var db: OpaquePointer?
        expect(sqlite3_open_v2(":memory:", &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, nil), SQLITE_OK, "open memory db")
        expect(sqlite3_exec(db, Schema.sql, nil, nil, nil), SQLITE_OK, "schema applies")
        expect(sqlite3_exec(db, Schema.sql, nil, nil, nil), SQLITE_OK, "schema is idempotent")
        func scalar(_ sql: String) -> String {
            var stmt: OpaquePointer?
            sqlite3_prepare_v2(db, sql, -1, &stmt, nil)
            defer { sqlite3_finalize(stmt) }
            guard sqlite3_step(stmt) == SQLITE_ROW, let c = sqlite3_column_text(stmt, 0) else { return "" }
            return String(cString: c)
        }
        func scalarIn(_ h: OpaquePointer?, _ sql: String) -> String {
            var stmt: OpaquePointer?
            sqlite3_prepare_v2(h, sql, -1, &stmt, nil)
            defer { sqlite3_finalize(stmt) }
            guard sqlite3_step(stmt) == SQLITE_ROW, let c = sqlite3_column_text(stmt, 0) else { return "" }
            return String(cString: c)
        }
        expect(scalar("SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_master WHERE type='table' ORDER BY name)"),
               "calls,intervals,meta,notified,samples,stretches,totals", "all seven tables exist")
        expect(scalar("SELECT value FROM meta WHERE key='schema_version'"), "4", "schema_version = 4")
        expect(sqlite3_exec(db, "INSERT INTO samples(ts,kind) VALUES (1,'bogus')", nil, nil, nil), SQLITE_CONSTRAINT, "kind CHECK rejects unknown kinds")
        expect(sqlite3_exec(db, "INSERT INTO samples(ts,kind,app) VALUES (1,'sample','a'),(2,'heartbeat','a'),(3,'term',NULL)", nil, nil, nil), SQLITE_OK, "valid kinds insert")
        expect(scalar("SELECT max(rowid) FROM samples"), "3", "rowid is addressable for the brain watermark")
        expect(sqlite3_exec(db, "INSERT INTO calls(from_ts,to_ts,call_app,guess_project,breakdown,decision) VALUES (1,2,'x','p','[]','maybe')", nil, nil, nil), SQLITE_CONSTRAINT, "calls.decision CHECK rejects unknown values")
        expect(sqlite3_exec(db, "INSERT INTO calls(from_ts,to_ts,call_app,guess_project,breakdown) VALUES (1,2,'x','p','[]')", nil, nil, nil), SQLITE_OK, "pending calls row inserts with NULL decision")
        expect(sqlite3_exec(db, "UPDATE calls SET decision='background' WHERE from_ts=1", nil, nil, nil), SQLITE_OK, "fresh schema accepts background")
        expect(sqlite3_exec(db, "INSERT OR IGNORE INTO calls(from_ts,to_ts,call_app,guess_project,breakdown) VALUES (1,9,'y','q','[]')", nil, nil, nil), SQLITE_OK, "duplicate from_ts is ignored")
        expect(scalar("SELECT to_ts FROM calls WHERE from_ts=1"), "2", "INSERT OR IGNORE left the first row alone")
        expect(sqlite3_exec(db, "INSERT INTO stretches(from_ts,to_ts,unattributed_s,breakdown) VALUES (1,700,600,'[]')", nil, nil, nil), SQLITE_OK, "unanswered stretches row inserts with NULL answer")
        expect(sqlite3_exec(db, "INSERT INTO stretches(from_ts,to_ts,breakdown) VALUES (2,9,'[]')", nil, nil, nil), SQLITE_CONSTRAINT, "stretches.unattributed_s is required")
        expect(scalar("SELECT COUNT(*) FROM stretches WHERE answer IS NULL AND decided_at IS NULL AND applied_at IS NULL AND rules_error IS NULL"), "1", "new row has no answer, decision, apply or error")
        sqlite3_close(db)

        // --- schema 4: a schema-3 calls table is rebuilt once
        var m: OpaquePointer?
        expect(sqlite3_open_v2(":memory:", &m, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, nil), SQLITE_OK, "open v3 db")
        let v3 = """
        CREATE TABLE calls (from_ts INTEGER PRIMARY KEY, to_ts INTEGER NOT NULL, call_app TEXT NOT NULL, guess_project TEXT NOT NULL,
          guess_subproject TEXT, breakdown TEXT NOT NULL, decision TEXT CHECK (decision IN ('project','split')), project TEXT,
          subproject TEXT, decided_at INTEGER, applied_at INTEGER);
        INSERT INTO calls VALUES (1,9,'us.zoom.xos','acme','acme-app','[]','project','phd',NULL,5,6);
        INSERT INTO calls VALUES (10,20,'us.zoom.xos','acme',NULL,'[{"project":"meeting","seconds":10}]',NULL,NULL,NULL,NULL,NULL);
        """
        expect(sqlite3_exec(m, v3, nil, nil, nil), SQLITE_OK, "v3 calls table with a decided and a pending row")
        expect(Pure.callsNeedsV4Rebuild(createSQL: scalarIn(m, "SELECT sql FROM sqlite_master WHERE name='calls'")), true, "v3 table needs the rebuild")
        expect(sqlite3_exec(m, Pure.callsV4Rebuild, nil, nil, nil), SQLITE_OK, "rebuild runs")
        expect(sqlite3_exec(m, Schema.sql, nil, nil, nil), SQLITE_OK, "schema applies after the rebuild")
        expect(scalarIn(m, "SELECT group_concat(from_ts||'|'||to_ts||'|'||call_app||'|'||guess_project||'|'||COALESCE(guess_subproject,'-')||'|'||breakdown||'|'||COALESCE(decision,'-')||'|'||COALESCE(project,'-')||'|'||COALESCE(decided_at,'-')||'|'||COALESCE(applied_at,'-'), ';') FROM (SELECT * FROM calls ORDER BY from_ts)"),
               #"1|9|us.zoom.xos|acme|acme-app|[]|project|phd|5|6;10|20|us.zoom.xos|acme|-|[{"project":"meeting","seconds":10}]|-|-|-|-"#, "rows and values survive")
        expect(sqlite3_exec(m, "UPDATE calls SET decision='background' WHERE from_ts=10", nil, nil, nil), SQLITE_OK, "background accepted after the rebuild")
        expect(sqlite3_exec(m, "UPDATE calls SET decision='maybe' WHERE from_ts=10", nil, nil, nil), SQLITE_CONSTRAINT, "unknown decisions still rejected")
        expect(Pure.callsNeedsV4Rebuild(createSQL: scalarIn(m, "SELECT sql FROM sqlite_master WHERE name='calls'")), false, "second open: no rebuild")
        expect(Pure.callsNeedsV4Rebuild(createSQL: nil), false, "fresh database: schema.sql creates the table")
        expect(scalarIn(m, "SELECT value FROM meta WHERE key='schema_version'"), "4", "schema version 4")
        sqlite3_close(m)

        print("\(checks - failures)/\(checks) checks passed")
        exit(failures == 0 ? 0 : 1)
    }
}

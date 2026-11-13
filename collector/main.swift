import AppKit
import ApplicationServices
import CoreAudio
import CoreGraphics
import Darwin
import Foundation
import IOKit
import SQLite3

// MARK: - Paths and constants

enum Paths {
    static let home = FileManager.default.homeDirectoryForCurrentUser.path
    static let dbDir = home + "/Library/Application Support/stint"
    static let db = dbDir + "/stint.db"
    static let cwdDir = home + "/.local/state/stint/cwd"
    static let herdrSocket = home + "/.config/herdr/herdr.sock"
}

enum Bundles {
    static let wavebox = "com.bookry.wavebox"
    static let chrome = "com.google.Chrome"
    static let search = "com.officecommun.search"
    static let browsers = [wavebox, chrome, search]
    /// herdr is a TUI; the GUI process that is frontmost while using it is Ghostty (discovered 2026-09-16).
    static let herdrHosts: Set<String> = ["com.mitchellh.ghostty"]
    /// Synthetic `app` for samples driven by remote input (iPad over mosh into a herdr client) while the Mac is
    /// locked. Not a real bundle id on purpose: it must never collide with the brain's NonWorkApps
    /// (`com.apple.loginwindow`, the screen saver), and rules attribute these samples by cwd.
    static let remote = "remote.herdr"
    /// Terminals that get the zsh-hook cwd fallback. Ghostty is handled by the herdr branch first.
    /// Editors with a built-in terminal (Zed, VS Code) are excluded: their frontmost window is usually the
    /// editor, not a shell, so the hook file would attribute editor time to whatever shell last drew a prompt.
    static let terminals: Set<String> = ["com.apple.Terminal", "com.googlecode.iterm2", "dev.warp.Warp-Stable"]
}

let tickIntervalS = 5
let browserBackoffS: TimeInterval = 60
let dbRetryS = 60
let menuRefreshS = 60

let isoFormatter = ISO8601DateFormatter()  // thread-safe, shared by every log line

func log(_ msg: String) {
    FileHandle.standardError.write((isoFormatter.string(from: Date()) + " " + msg + "\n").data(using: .utf8)!)
}

func nowTs() -> Int { Int(Date().timeIntervalSince1970) }

// MARK: - Store (SQLite, system library)

final class Store {
    /// Serializes all access to the mutable fields below (db, insert, buffer, lastRetry, _lastError).
    /// SQLITE_OPEN_FULLMUTEX (below) only serializes SQLite's own C calls against each other; it says
    /// nothing about this class's Swift-level fields, which can be reassigned by write() (tick queue)
    /// while menuData() (menu refresh path) reads them. Every public entry point runs its body inside
    /// `queue.sync`; private helpers assume they already run on `queue` and must never call back into
    /// a public method (that would deadlock on this non-reentrant serial queue).
    private let queue = DispatchQueue(label: "dev.asafdafna.stint.store")
    private var db: OpaquePointer?
    private var insert: OpaquePointer?
    private var buffer: [Sample] = []
    private var lastRetry = 0
    private var _lastError: String?

    init() {
        // 0700: the DB holds window titles and URLs, so no other local account reads it (this also covers
        // the -wal and -shm files SQLite creates alongside it).
        try? FileManager.default.createDirectory(atPath: Paths.dbDir, withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        open()  // safe unlocked: init() runs before this instance is published to any other thread
    }

    /// Assumes it runs on `queue` (or, for the one call from init(), before publication).
    private func open() {
        close()
        var handle: OpaquePointer?
        let flags = SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_FULLMUTEX
        guard sqlite3_open_v2(Paths.db, &handle, flags, nil) == SQLITE_OK, let h = handle else {
            _lastError = "open: " + (handle.map { String(cString: sqlite3_errmsg($0)) } ?? "unknown")
            sqlite3_close(handle)
            return
        }
        db = h
        sqlite3_busy_timeout(h, 5000)
        var errMsg: UnsafeMutablePointer<CChar>?
        var callsSQL: String?
        var probe: OpaquePointer?
        var migrateError: String?
        if sqlite3_prepare_v2(h, "SELECT sql FROM sqlite_master WHERE type='table' AND name='calls'", -1, &probe, nil) != SQLITE_OK {
            migrateError = String(cString: sqlite3_errmsg(h))
        } else {
            switch sqlite3_step(probe) {
            case SQLITE_ROW: callsSQL = sqlite3_column_text(probe, 0).map { String(cString: $0) }
            case SQLITE_DONE: break
            default: migrateError = String(cString: sqlite3_errmsg(h))
            }
        }
        sqlite3_finalize(probe)
        if migrateError == nil, Pure.callsNeedsV4Rebuild(createSQL: callsSQL),
           sqlite3_exec(h, Pure.callsV4Rebuild, nil, nil, &errMsg) != SQLITE_OK {
            migrateError = errMsg.map { String(cString: $0) } ?? "unknown"
            sqlite3_free(errMsg)
            errMsg = nil
        }
        if let migrateError {
            sqlite3_exec(h, "ROLLBACK", nil, nil, nil)
            _lastError = "migrate: " + migrateError
            close()
            return
        }
        if sqlite3_exec(h, Schema.sql, nil, nil, &errMsg) != SQLITE_OK {
            _lastError = "schema: " + (errMsg.map { String(cString: $0) } ?? "unknown")
            sqlite3_free(errMsg)
            close()
            return
        }
        let sql = "INSERT INTO samples (ts, kind, app, title, url, cwd, idle_s, call_app) VALUES (?,?,?,?,?,?,?,?)"
        if sqlite3_prepare_v2(h, sql, -1, &insert, nil) != SQLITE_OK {
            _lastError = "prepare: " + String(cString: sqlite3_errmsg(h))
            close()
            return
        }
        // sqlite3_open_v2 creates the file under the process umask (0644 under launchd); tighten it.
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: Paths.db)
        _lastError = nil
    }

    /// Assumes it runs on `queue`.
    private func close() {
        if insert != nil { sqlite3_finalize(insert); insert = nil }
        if db != nil { sqlite3_close(db); db = nil }
    }

    var isOpen: Bool { queue.sync { db != nil } }
    var bufferedCount: Int { queue.sync { buffer.count } }
    var lastError: String? { queue.sync { _lastError } }

    /// Writes one row. On any error the row is buffered; buffered rows are retried every 60 s. Never throws, never exits.
    func write(_ s: Sample) {
        queue.sync {
            if db == nil {
                buffer.append(s)
                retryIfDue()
                return
            }
            if !buffer.isEmpty { flushBuffer() }
            if !insertRow(s) {
                buffer.append(s)
                log("sqlite write failed: \(_lastError ?? "?"); buffering \(buffer.count) rows")
                close()
            }
        }
    }

    /// Assumes it runs on `queue`.
    private func insertRow(_ s: Sample) -> Bool {
        guard let stmt = insert, let h = db else { return false }
        let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)
        sqlite3_reset(stmt)
        sqlite3_bind_int64(stmt, 1, Int64(s.ts))
        sqlite3_bind_text(stmt, 2, s.kind, -1, transient)
        func bindText(_ i: Int32, _ v: String?) {
            if let v = v { sqlite3_bind_text(stmt, i, v, -1, transient) } else { sqlite3_bind_null(stmt, i) }
        }
        bindText(3, s.app); bindText(4, s.title); bindText(5, s.url); bindText(6, s.cwd)
        if let i = s.idleS { sqlite3_bind_int64(stmt, 7, Int64(i)) } else { sqlite3_bind_null(stmt, 7) }
        bindText(8, s.callApp)
        let rc = sqlite3_step(stmt)
        if rc != SQLITE_DONE {
            _lastError = "step: " + String(cString: sqlite3_errmsg(h))
            return false
        }
        return true
    }

    /// Assumes it runs on `queue`. On any failure the buffer is left untouched so the next retry can try again.
    private func flushBuffer() {
        guard let h = db else { return }
        if sqlite3_exec(h, "BEGIN", nil, nil, nil) != SQLITE_OK {
            _lastError = "begin: " + String(cString: sqlite3_errmsg(h))   // autocommit mode; nothing was inserted, buffer untouched
            log("flush failed: \(_lastError ?? "?")")
            close()
            return
        }
        var failed = false
        for s in buffer {
            if !insertRow(s) { failed = true; break }
        }
        if failed {
            sqlite3_exec(h, "ROLLBACK", nil, nil, nil)   // nothing written; the buffer stays intact for the next retry
            log("flush failed: \(_lastError ?? "?")")
            close()
        } else {
            sqlite3_exec(h, "COMMIT", nil, nil, nil)
            log("flushed \(buffer.count) buffered rows")
            buffer.removeAll()
        }
    }

    /// Assumes it runs on `queue`.
    private func retryIfDue() {
        let now = nowTs()
        guard now - lastRetry >= dbRetryS else { return }
        lastRetry = now
        open()
        if db != nil { log("sqlite reopened"); flushBuffer() } else { log("sqlite reopen failed: \(_lastError ?? "?")") }
        if buffer.count > 20_000 {
            let dropped = buffer.count - 20_000
            buffer.removeFirst(dropped)
            log("buffer overflow: dropped \(dropped) oldest rows")
        }
    }

    // MARK: reads for the menu bar

    struct MenuData {
        var rows: [TotalRow]
        var lastTotalsUpdate: Int?
        var health: String?
        var healthAt: Int?
        var pending: [PendingCall] = []   // calls rows with decision IS NULL, oldest first
        var projects: String? = nil       // meta.projects, comma-joined
        var stretches: [PendingStretch] = []  // pending stretches rows, oldest first
        var picker: String? = nil         // meta.picker, a JSON array
    }

    func menuData(now: Date) -> MenuData {
        queue.sync {
            var out = MenuData(rows: [], lastTotalsUpdate: nil, health: nil, healthAt: nil)
            guard let h = db else { return out }
            let today = Pure.dayString(now)
            let firstWeekday = Pure.firstWeekday(scalarText("SELECT value FROM meta WHERE key = 'week_start'"))
            let weekStart = Pure.weekStartDay(now, firstWeekday: firstWeekday)
            let sql = """
            SELECT t.project, CASE WHEN t.day = ?1 THEN t.day_seconds ELSE 0 END, t.week_seconds
            FROM totals t JOIN (SELECT project, MAX(day) AS day FROM totals WHERE day >= ?2 AND day <= ?1 GROUP BY project) m
              ON m.project = t.project AND m.day = t.day
            """
            var stmt: OpaquePointer?
            if sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK {
                let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)
                sqlite3_bind_text(stmt, 1, today, -1, transient)
                sqlite3_bind_text(stmt, 2, weekStart, -1, transient)
                while sqlite3_step(stmt) == SQLITE_ROW {
                    let p = String(cString: sqlite3_column_text(stmt, 0))
                    out.rows.append(TotalRow(project: p, daySeconds: Int(sqlite3_column_int64(stmt, 1)), weekSeconds: Int(sqlite3_column_int64(stmt, 2))))
                }
            }
            sqlite3_finalize(stmt)
            out.lastTotalsUpdate = scalarInt("SELECT MAX(updated_at) FROM totals")
            out.health = scalarText("SELECT value FROM meta WHERE key = 'health'")
            out.healthAt = scalarInt("SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'health_at'")
            out.pending = readPendingCalls()
            out.projects = scalarText("SELECT value FROM meta WHERE key = 'projects'")
            out.stretches = readPendingStretches()
            out.picker = scalarText("SELECT value FROM meta WHERE key = 'picker'")
            return out
        }
    }

    /// Assumes it runs on `queue`.
    private func scalarText(_ sql: String) -> String? {
        guard let h = db else { return nil }
        var stmt: OpaquePointer?
        defer { sqlite3_finalize(stmt) }
        guard sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK, sqlite3_step(stmt) == SQLITE_ROW,
              let c = sqlite3_column_text(stmt, 0) else { return nil }
        return String(cString: c)
    }

    /// Assumes it runs on `queue`.
    private func scalarInt(_ sql: String) -> Int? {
        guard let h = db else { return nil }
        var stmt: OpaquePointer?
        defer { sqlite3_finalize(stmt) }
        guard sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK, sqlite3_step(stmt) == SQLITE_ROW,
              sqlite3_column_type(stmt, 0) != SQLITE_NULL else { return nil }
        return Int(sqlite3_column_int64(stmt, 0))
    }

    /// Assumes it runs on `queue`.
    private func readPendingCalls() -> [PendingCall] {
        guard let h = db else { return [] }
        var stmt: OpaquePointer?
        defer { sqlite3_finalize(stmt) }
        let sql = "SELECT from_ts, to_ts, call_app, guess_project, guess_subproject, breakdown FROM calls WHERE decision IS NULL ORDER BY from_ts"
        guard sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK else { return [] }
        var out: [PendingCall] = []
        while sqlite3_step(stmt) == SQLITE_ROW {
            func text(_ i: Int32) -> String? { sqlite3_column_text(stmt, i).map { String(cString: $0) } }
            out.append(PendingCall(fromTs: Int(sqlite3_column_int64(stmt, 0)), toTs: Int(sqlite3_column_int64(stmt, 1)),
                                   callApp: text(2) ?? "", guessProject: text(3) ?? Pure.unattributed,
                                   guessSubproject: text(4), breakdown: text(5) ?? "[]"))
        }
        return out
    }

    /// Assumes it runs on `queue`. Pending = unanswered, and never decided or re-opened and already
    /// recomputed by the brain, so a re-opened row does not pop before its breakdown is fresh.
    private func readPendingStretches() -> [PendingStretch] {
        guard let h = db else { return [] }
        var stmt: OpaquePointer?
        defer { sqlite3_finalize(stmt) }
        let sql = "SELECT from_ts, to_ts, unattributed_s, breakdown FROM stretches WHERE answer IS NULL AND (decided_at IS NULL OR applied_at >= decided_at) ORDER BY from_ts"
        guard sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK else { return [] }
        var out: [PendingStretch] = []
        while sqlite3_step(stmt) == SQLITE_ROW {
            out.append(PendingStretch(fromTs: Int(sqlite3_column_int64(stmt, 0)), toTs: Int(sqlite3_column_int64(stmt, 1)),
                                      unattributedS: Int(sqlite3_column_int64(stmt, 2)),
                                      breakdown: sqlite3_column_text(stmt, 3).map { String(cString: $0) } ?? "[]"))
        }
        return out
    }

    enum AnswerOutcome: Equatable {
        case saved
        case gone            // no unanswered row with that from_ts: the brain dropped it or it was answered
        case failed(String)  // SQLite error text for the panel; Save retries
    }

    /// Writes the stretch pop-up's answer, only while the row has none. Like
    /// setDecision it never touches the sample buffer or closes the handle.
    func setStretchAnswer(fromTs: Int, answer: String, now: Int) -> AnswerOutcome {
        queue.sync {
            guard let h = db else { return .failed(_lastError ?? "database is closed") }
            var stmt: OpaquePointer?
            defer { sqlite3_finalize(stmt) }
            let sql = "UPDATE stretches SET answer = ?, decided_at = ? WHERE from_ts = ? AND answer IS NULL"
            guard sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK else { return .failed(String(cString: sqlite3_errmsg(h))) }
            let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)
            sqlite3_bind_text(stmt, 1, answer, -1, transient)
            sqlite3_bind_int64(stmt, 2, Int64(now))
            sqlite3_bind_int64(stmt, 3, Int64(fromTs))
            guard sqlite3_step(stmt) == SQLITE_DONE else { return .failed(String(cString: sqlite3_errmsg(h))) }
            return sqlite3_changes(h) == 1 ? .saved : .gone
        }
    }

    /// Writes the pop-up answer. nil on success, else the SQLite error text for the dialog. Unlike a failed
    /// sample insert this never closes the handle or touches the buffer: the dialog stays open and Save
    /// retries.
    func setDecision(fromTs: Int, _ d: CallDecision, now: Int) -> String? {
        queue.sync {
            guard let h = db else { return _lastError ?? "database is closed" }
            let b = Pure.decisionBinding(d)
            var stmt: OpaquePointer?
            defer { sqlite3_finalize(stmt) }
            let sql = "UPDATE calls SET decision = ?, project = ?, subproject = ?, decided_at = ? WHERE from_ts = ? AND decision IS NULL"
            guard sqlite3_prepare_v2(h, sql, -1, &stmt, nil) == SQLITE_OK else { return String(cString: sqlite3_errmsg(h)) }
            let transient = unsafeBitCast(-1, to: sqlite3_destructor_type.self)
            sqlite3_bind_text(stmt, 1, b.decision, -1, transient)
            if let p = b.project { sqlite3_bind_text(stmt, 2, p, -1, transient) } else { sqlite3_bind_null(stmt, 2) }
            if let s = b.subproject { sqlite3_bind_text(stmt, 3, s, -1, transient) } else { sqlite3_bind_null(stmt, 3) }
            sqlite3_bind_int64(stmt, 4, Int64(now))
            sqlite3_bind_int64(stmt, 5, Int64(fromTs))
            guard sqlite3_step(stmt) == SQLITE_DONE else { return String(cString: sqlite3_errmsg(h)) }
            return sqlite3_changes(h) == 1 ? nil : "call \(fromTs) is no longer pending"
        }
    }
}

// MARK: - Sensors

/// Frontmost app, cached on the main thread from NSWorkspace activation notifications so the tick never touches
/// NSWorkspace off-main and never has to `main.sync` (the sleep handler does `tickQueue.sync` from main; a
/// `main.sync` from the tick would deadlock against it).
final class FrontmostCache {
    private let lock = NSLock()
    private var app: NSRunningApplication?

    init() {
        app = NSWorkspace.shared.frontmostApplication
        NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didActivateApplicationNotification, object: nil, queue: .main
        ) { [weak self] n in
            guard let self = self else { return }
            let a = n.userInfo?[NSWorkspace.applicationUserInfoKey] as? NSRunningApplication
            self.lock.lock(); self.app = a; self.lock.unlock()
        }
    }

    var current: NSRunningApplication? {
        lock.lock(); defer { lock.unlock() }
        return app
    }
}

enum Sensors {
    static let frontmostCache = FrontmostCache()   // touch once from the main thread (Collector.start / selftest) before the tick runs
    static func frontmost() -> NSRunningApplication? { frontmostCache.current }

    /// AX focused-window title (pattern after aw-watcher-window/macos.swift). nil when AX is not trusted or the app has no window.
    static func focusedWindowTitle(pid: pid_t) -> String? {
        let app = AXUIElementCreateApplication(pid)
        AXUIElementSetMessagingTimeout(app, 1)   // a hung app costs at most 1 s per tick, never the default 6 s
        var window: CFTypeRef?
        guard AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &window) == .success,
              let w = window else { return nil }
        guard CFGetTypeID(w) == AXUIElementGetTypeID() else { return nil }  // `as?` to AXUIElement always succeeds statically, so check the real CF type instead
        let win = w as! AXUIElement
        AXUIElementSetMessagingTimeout(win, 1)   // per-element timeout: the window element defaults to 6 s independently of the app element
        var title: CFTypeRef?
        guard AXUIElementCopyAttributeValue(win, kAXTitleAttribute as CFString, &title) == .success else { return nil }
        return title as? String
    }

    /// Seconds since the last input event. CGEventSource first (permission-free, verified on macOS 26.6); IOKit HIDIdleTime fallback.
    static func idleSeconds() -> Int? {
        let any = CGEventType(rawValue: ~0)!  // kCGAnyInputEventType; `.null` measures the wrong thing
        let cg = CGEventSource.secondsSinceLastEventType(.hidSystemState, eventType: any)
        if cg.isFinite && cg >= 0 && cg < 86_400 * 30 { return Int(cg) }
        return idleSecondsIOKit()
    }

    static func idleSecondsIOKit() -> Int? {
        let service = IOServiceGetMatchingService(kIOMainPortDefault, IOServiceMatching("IOHIDSystem"))
        guard service != 0 else { return nil }
        defer { IOObjectRelease(service) }
        guard let v = IORegistryEntryCreateCFProperty(service, "HIDIdleTime" as CFString, kCFAllocatorDefault, 0)?.takeRetainedValue(),
              let ns = (v as? NSNumber)?.int64Value else { return nil }
        return Int(ns / 1_000_000_000)
    }

    /// Every process currently running audio input, each resolved to its top-most ancestor bundle id below launchd
    /// (browser helper processes map to the browser), deduplicated and comma-joined; nil when nobody holds the mic.
    static func callApp() -> String? {
        var addr = AudioObjectPropertyAddress(mSelector: kAudioHardwarePropertyProcessObjectList,
                                              mScope: kAudioObjectPropertyScopeGlobal,
                                              mElement: kAudioObjectPropertyElementMain)
        var size: UInt32 = 0
        guard AudioObjectGetPropertyDataSize(AudioObjectID(kAudioObjectSystemObject), &addr, 0, nil, &size) == noErr, size > 0 else { return nil }
        var ids = [AudioObjectID](repeating: 0, count: Int(size) / MemoryLayout<AudioObjectID>.size)
        guard AudioObjectGetPropertyData(AudioObjectID(kAudioObjectSystemObject), &addr, 0, nil, &size, &ids) == noErr else { return nil }
        var found: [String] = []
        for id in ids.prefix(Int(size) / MemoryLayout<AudioObjectID>.size) {
            var inAddr = AudioObjectPropertyAddress(mSelector: kAudioProcessPropertyIsRunningInput, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
            var running: UInt32 = 0
            var rs = UInt32(MemoryLayout<UInt32>.size)
            guard AudioObjectGetPropertyData(id, &inAddr, 0, nil, &rs, &running) == noErr, running != 0 else { continue }
            var pidAddr = AudioObjectPropertyAddress(mSelector: kAudioProcessPropertyPID, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
            var pid: pid_t = 0
            var ps = UInt32(MemoryLayout<pid_t>.size)
            guard AudioObjectGetPropertyData(id, &pidAddr, 0, nil, &ps, &pid) == noErr else { continue }
            if let b = topmostBundleId(from: pid) ?? audioBundleId(id) { found.append(b) }
        }
        return Pure.joinCallApps(found)
    }

    /// Walks the parent chain up to (not including) launchd and returns the bundle id of the highest ancestor
    /// that NSRunningApplication knows; nil when no process in the chain has one.
    static func topmostBundleId(from start: pid_t) -> String? {
        var pid = start
        var best: String?
        for _ in 0..<16 {
            if pid <= 1 { break }
            if let b = NSRunningApplication(processIdentifier: pid)?.bundleIdentifier { best = b }
            guard let parent = parentPid(of: pid), parent > 1 else { break }
            pid = parent
        }
        return best
    }

    /// CoreAudio's own idea of the process bundle id (fallback when the chain has no NSRunningApplication).
    static func audioBundleId(_ id: AudioObjectID) -> String? {
        var addr = AudioObjectPropertyAddress(mSelector: kAudioProcessPropertyBundleID, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
        var value: Unmanaged<CFString>?
        var size = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
        let st = withUnsafeMutablePointer(to: &value) { AudioObjectGetPropertyData(id, &addr, 0, nil, &size, $0) }
        guard st == noErr, let v = value?.takeRetainedValue() else { return nil }
        let str = v as String
        return str.isEmpty ? nil : str
    }

    static func parentPid(of pid: pid_t) -> pid_t? {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.size
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0, size > 0 else { return nil }
        return info.kp_eproc.e_ppid
    }

    /// Remote input signal: the newest st_atime among the controlling ttys of *remote* herdr client processes
    /// (`pbi_comm == "herdr"`; the pane shells are zsh, so `herdr pane send-keys` automation never counts). A
    /// pty's atime advances only when the client reads a keystroke (verified 2026-09-22: output moves mtime only,
    /// polling reads do not touch atime), so over mosh from the iPad it is the last remote keystroke while HID
    /// idle climbs and the Mac is locked. Only session leaders count (mosh-server forkpty's the client, so it
    /// leads its tty's session; verified 2026-09-22): that excludes the Ghostty-hosted client, whose tty tracks
    /// local typing and lags HID by ~1 s (would read as "remote fresher than HID" right after switching away
    /// from Ghostty), and `herdr pane wait-output`-style CLI calls running inside a pane, whose controlling tty
    /// is the pane's pty. Returns the tty name (for --selftest) and the age; nil when no remote herdr client has a
    /// tty or on any error. Cost: one proc_pidinfo per process, a few ms per tick.
    static func remoteInput() -> (tty: String, age: Int)? {
        var count = proc_listpids(UInt32(PROC_ALL_PIDS), 0, nil, 0)
        guard count > 0 else { return nil }
        var pids = [pid_t](repeating: 0, count: Int(count) / MemoryLayout<pid_t>.size + 64)   // slack for processes spawned in between
        count = proc_listpids(UInt32(PROC_ALL_PIDS), 0, &pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        guard count > 0 else { return nil }
        var newest: (tty: String, atime: TimeInterval)?
        for pid in pids.prefix(Int(count) / MemoryLayout<pid_t>.size) where pid > 0 {
            var info = proc_bsdinfo()
            let want = Int32(MemoryLayout<proc_bsdinfo>.size)
            guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, want) == want else { continue }
            guard info.e_tdev != UInt32.max else { continue }   // NODEV: no controlling tty (the herdr server, daemons)
            let comm = withUnsafeBytes(of: &info.pbi_comm) { raw in String(decoding: raw.prefix { $0 != 0 }, as: UTF8.self) }
            guard comm == "herdr", info.pbi_flags & UInt32(PROC_FLAG_SLEADER) != 0 else { continue }
            // e_tdev is the tty's st_rdev; devname_r maps it back to "ttysNNN" and the stat below confirms it.
            var name = [CChar](repeating: 0, count: 64)
            guard devname_r(dev_t(bitPattern: info.e_tdev), mode_t(S_IFCHR), &name, Int32(name.count)) != nil else { continue }
            let tty = String(cString: name)
            var st = stat()
            guard tty.hasPrefix("ttys"), stat("/dev/" + tty, &st) == 0, UInt32(bitPattern: st.st_rdev) == info.e_tdev else { continue }
            let atime = TimeInterval(st.st_atimespec.tv_sec) + TimeInterval(st.st_atimespec.tv_nsec) / 1e9
            if newest == nil || atime > newest!.atime { newest = (tty, atime) }
        }
        guard let n = newest else { return nil }
        return (n.tty, max(0, Int(Date().timeIntervalSince1970 - n.atime)))
    }

    /// Fallback cwd: newest file written by the zsh hook in ~/.local/state/stint/cwd/.
    static func cwdFromHookFiles() -> String? {
        let fm = FileManager.default
        guard let names = try? fm.contentsOfDirectory(atPath: Paths.cwdDir) else { return nil }
        var files: [Pure.CwdFile] = []
        for n in names where !n.hasPrefix(".") {
            let p = Paths.cwdDir + "/" + n
            guard let attrs = try? fm.attributesOfItem(atPath: p), let m = attrs[.modificationDate] as? Date,
                  let c = try? String(contentsOfFile: p, encoding: .utf8) else { continue }
            files.append(Pure.CwdFile(name: n, mtime: m, contents: c))
        }
        return Pure.newestCwd(files)
    }
}

// MARK: - herdr socket client (unix socket, newline JSON, 1 s timeout)

enum Herdr {
    static var counter = 0

    /// What one `pane.current` round trip yields about the focused pane. Both fields come from the same
    /// response so the remote branch, which needs cwd and title, costs one socket call like the herdr branch.
    struct Pane {
        var cwd: String?
        var title: String?
    }

    static func currentPane(timeoutS: Int = 1) -> Pane? {
        guard let data = roundTrip(Pure.herdrRequest(id: "stint-\(nextId())"), timeoutS: timeoutS) else { return nil }
        return Pane(cwd: Pure.herdrForegroundCwd(data), title: Pure.herdrPaneTitle(data))
    }

    static func foregroundCwd(timeoutS: Int = 1) -> String? { currentPane(timeoutS: timeoutS)?.cwd }

    static func reachable() -> Bool {
        let req = "{\"id\":\"stint-ping\",\"method\":\"ping\",\"params\":{}}\n".data(using: .utf8)!
        guard let d = roundTrip(req, timeoutS: 1), let s = String(data: d, encoding: .utf8) else { return false }
        return s.contains("\"pong\"")
    }

    private static func nextId() -> Int { counter += 1; return counter }

    static func roundTrip(_ request: Data, timeoutS: Int) -> Data? {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        defer { Darwin.close(fd) }
        var on: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))  // avoid SIGPIPE killing the daemon if herdr closes between connect and send
        var tv = timeval(tv_sec: timeoutS, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let path = Paths.herdrSocket
        guard path.utf8.count < MemoryLayout.size(ofValue: addr.sun_path) else { return nil }
        withUnsafeMutableBytes(of: &addr.sun_path) { $0.copyBytes(from: path.utf8) }  // struct is zeroed, so it stays NUL-terminated
        let len = socklen_t(MemoryLayout<sockaddr_un>.size)
        let connected = withUnsafePointer(to: &addr) { p in
            p.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, len) }
        }
        guard connected == 0 else { return nil }
        let sent = request.withUnsafeBytes { buf in Darwin.send(fd, buf.baseAddress, buf.count, 0) }
        guard sent == request.count else { return nil }
        var out = Data()
        var chunk = [UInt8](repeating: 0, count: 65_536)
        let deadline = Date().addingTimeInterval(TimeInterval(timeoutS))
        while Date() < deadline {
            let n = recv(fd, &chunk, chunk.count, 0)
            if n <= 0 { break }
            out.append(contentsOf: chunk[0..<n])
            if out.last == 0x0A { break }
        }
        return out.isEmpty ? nil : out
    }
}

// MARK: - Browser URL (Apple Events; NSAppleScript is main-thread-only, so the send hops to main; 60 s backoff after any failure)

/// Worst case ~3 s main-thread stall per hung browser sample (three Apple Events, each bounded by
/// `with timeout of 1 second`), then 60 s backoff. The first-run TCC consent prompt is asked in
/// `refresh`, off the main thread (see there).
final class BrowserURL {
    private let queue = DispatchQueue(label: "dev.asafdafna.stint.browser")   // serializes gating and result bookkeeping
    private let lock = NSLock()
    private let cacheTtlS: TimeInterval = 15
    private var scripts: [String: NSAppleScript] = [:]   // main thread only
    private var inFlight: Set<String> = []               // guarded by lock, keyed by bundle id
    private var backoffUntil: [String: Date] = [:]       // guarded by lock
    private var latest: [String: (url: String, fetchedAt: Date)] = [:]   // guarded by lock; absent = unknown / incognito / not yet fetched
    private var consentAnswered: Set<String> = []        // guarded by lock

    enum Outcome { case url(String), none, failure(String) }

    /// Returns the last known URL for `bundleId` if it was fetched within the last 15 s (else nil, even
    /// if a stale value is cached), and schedules one refresh unless one is in flight or backed off.
    /// Never blocks the caller (the tick).
    func url(for bundleId: String) -> String? {
        lock.lock()
        let entry = latest[bundleId]
        let busy = inFlight.contains(bundleId)
        let backedOff = (backoffUntil[bundleId] ?? .distantPast) > Date()
        if !busy && !backedOff { inFlight.insert(bundleId) }
        lock.unlock()
        if !busy && !backedOff { queue.async { [self] in refresh(bundleId) } }
        guard let entry = entry, Date().timeIntervalSince(entry.fetchedAt) < cacheTtlS else { return nil }
        return entry.url
    }

    /// Browser queue. Never launches the browser: only a running instance is addressed.
    private func refresh(_ bundleId: String) {
        guard let app = NSRunningApplication.runningApplications(withBundleIdentifier: bundleId).first else {
            finish(bundleId, .none)
            return
        }
        lock.lock(); let answered = consentAnswered.contains(bundleId); lock.unlock()
        if !answered {
            // fetch()'s 1 s Apple Event timeout also closes the Automation consent prompt, so ask once without a
            // timeout. The ask blocks until the user answers, so it runs off this queue and other browsers keep going;
            // this browser stays in `inFlight` until it returns, so it never prompts twice.
            let pid = app.processIdentifier
            DispatchQueue.global(qos: .utility).async { [self] in
                let target = NSAppleEventDescriptor(processIdentifier: pid)
                let status = withExtendedLifetime(target) {
                    target.aeDesc.map { AEDeterminePermissionToAutomateTarget($0, typeWildCard, typeWildCard, true) }
                        ?? OSStatus(procNotFound)
                }
                if status != noErr { log("automation consent \(bundleId): OSStatus \(status)") }
                if status != OSStatus(procNotFound) {   // procNotFound: the browser quit; ask again when it runs
                    lock.lock(); consentAnswered.insert(bundleId); lock.unlock()
                }
                queue.async { self.finish(bundleId, .none) }
            }
            return
        }
        DispatchQueue.main.async { [self] in
            // The browser may have quit between the check above and this hop onto main; re-check here
            // so `tell application id` inside fetch() never launches it.
            guard !NSRunningApplication.runningApplications(withBundleIdentifier: bundleId).isEmpty else {
                finish(bundleId, .none)
                return
            }
            let result = fetch(bundleId)   // main thread; each Apple Event reply is bounded by `with timeout of 1 second`, at most three events (~3 s worst case)
            queue.async { self.finish(bundleId, result) }
        }
    }

    /// Called from the browser queue, or directly from the main thread on the running-check miss above;
    /// safe from either thread via `lock`.
    private func finish(_ bundleId: String, _ result: Outcome) {
        lock.lock(); defer { lock.unlock() }
        inFlight.remove(bundleId)
        switch result {
        case .url(let u):
            latest[bundleId] = (u, Date())
        case .none:
            latest[bundleId] = nil
        case .failure(let msg):
            // Leave `latest` alone: the 15 s TTL in url(for:) expires it on its own, and dropping it here
            // would blank the URL of a sample taken moments before a transient failure.
            backoffUntil[bundleId] = Date().addingTimeInterval(browserBackoffS)
            log("browser url \(bundleId) failed, backing off \(browserBackoffS) s: \(msg)")
        }
    }

    /// Main thread only. "INCOGNITO" marks a private window (Chromium `mode`, verified on Wavebox 2026-09-16)
    /// or a Search private tab (its sdef: `current tab` is missing value); a script-level error comes back as
    /// "ERR:<number>:<message>". At most three Apple Events, each under `with timeout of 1 second`: ~3 s worst case.
    private func fetch(_ bundleId: String) -> Outcome {
        let script: NSAppleScript
        if let cached = scripts[bundleId] {
            script = cached
        } else {
            let tabURL = bundleId == Bundles.search
                ? """
                  set t to current tab of w
                  if t is missing value then return "INCOGNITO"
                  return URL of t
                  """
                : """
                  set m to mode of w
                  if m is "incognito" then return "INCOGNITO"
                  return URL of active tab of w
                  """
            let src = """
            tell application id "\(bundleId)"
                with timeout of 1 second
                    try
                        set w to front window
                        \(tabURL)
                    on error errMsg number errNum
                        return "ERR:" & errNum & ":" & errMsg
                    end try
                end timeout
            end tell
            """
            guard let compiled = NSAppleScript(source: src) else { return .failure("cannot build script") }
            scripts[bundleId] = compiled
            script = compiled
        }
        var err: NSDictionary?
        let out = script.executeAndReturnError(&err)
        if let e = err {
            if isNoWindow(e[NSAppleScript.errorNumber] as? Int) { return .none }
            return .failure((e[NSAppleScript.errorMessage] as? String) ?? "\(e)")
        }
        let value = out.stringValue ?? ""
        if value == "INCOGNITO" { return .none }
        if value.hasPrefix("ERR:") {
            let body = String(value.dropFirst(4))
            if isNoWindow(Int(body.prefix(while: { $0 != ":" }))) { return .none }
            return .failure(body)
        }
        return value.isEmpty ? .none : .url(value)
    }

    /// -1719 (errAEIllegalIndex) and -1728 (errAENoSuchObject) mean "no front window" / "can't get that
    /// object": the browser is running with no window, or the window closed between two of the three events.
    /// Ordinary, not a fault, so it maps to `.none` — no backoff and no log line every 5 s.
    private func isNoWindow(_ number: Int?) -> Bool {
        number == -1719 || number == -1728
    }
}

// MARK: - Collector

final class Collector {
    let store: Store?
    let dump: Bool
    let tickQueue = DispatchQueue(label: "dev.asafdafna.stint.tick")
    let browser = BrowserURL()
    private var timer: DispatchSourceTimer?
    private(set) var lastWritten: Sample?
    private(set) var current: Sample?

    init(dump: Bool) {
        self.dump = dump
        self.store = dump ? nil : Store()
    }

    func start() {
        _ = Sensors.frontmostCache   // create on the main thread before the first tick
        let t = DispatchSource.makeTimerSource(queue: tickQueue)
        t.schedule(deadline: .now(), repeating: .seconds(tickIntervalS), leeway: .milliseconds(500))
        t.setEventHandler { [weak self] in self?.tick() }
        t.resume()
        timer = t
        let nc = NSWorkspace.shared.notificationCenter
        let pairs: [(Notification.Name, String)] = [
            (NSWorkspace.willSleepNotification, "sleep"),
            (NSWorkspace.didWakeNotification, "wake"),
            (NSWorkspace.sessionDidResignActiveNotification, "resign"),
            (NSWorkspace.sessionDidBecomeActiveNotification, "active"),
        ]
        for (name, kind) in pairs {
            nc.addObserver(forName: name, object: nil, queue: nil) { [weak self] _ in self?.boundary(kind) }
        }
        signal(SIGTERM, SIG_IGN)
        let sig = DispatchSource.makeSignalSource(signal: SIGTERM, queue: tickQueue)
        sig.setEventHandler { [weak self] in
            self?.writeBoundary("term")
            log("SIGTERM, exiting")
            exit(0)
        }
        sig.resume()
        sigSource = sig
    }
    private var sigSource: DispatchSourceSignal?

    func sample() -> Sample {
        let front = Sensors.frontmost()
        let app = front?.bundleIdentifier
        let hidIdle = Sensors.idleSeconds()
        let remoteAge = Sensors.remoteInput()?.age
        let idle = Pure.effectiveIdle(hidIdle: hidIdle, remoteAge: remoteAge)
        if Pure.remoteDecision(hidIdle: hidIdle, remoteAge: remoteAge, app: app, herdrHosts: Bundles.herdrHosts) {
            // Working from the iPad: the Mac is locked (frontmost is loginwindow, which the brain drops), so the
            // sample describes the herdr pane instead. Title is never nil (the brain's health check would flag
            // "title NULL"); herdr focus is session-wide, so the focused pane stands in for the remote client's.
            let pane = Herdr.currentPane()
            return Sample(ts: nowTs(), kind: "sample", app: Bundles.remote, title: pane?.title ?? "remote", url: nil,
                          cwd: pane?.cwd ?? Sensors.cwdFromHookFiles(), idleS: idle, callApp: Sensors.callApp())
        }
        var title: String?
        if let pid = front?.processIdentifier { title = Pure.normalizeTitle(Sensors.focusedWindowTitle(pid: pid)) }
        var url: String?
        if let a = app, Bundles.browsers.contains(a) {
            url = browser.url(for: a)
        }
        var cwd: String?
        if let a = app, Bundles.herdrHosts.contains(a) {
            cwd = Herdr.foregroundCwd() ?? Sensors.cwdFromHookFiles()
        } else if let a = app, Bundles.terminals.contains(a) {
            cwd = Sensors.cwdFromHookFiles()
        }
        return Sample(ts: nowTs(), kind: "sample", app: app, title: title, url: url, cwd: cwd,
                      idleS: idle, callApp: Sensors.callApp())
    }

    private func tick() {
        let s = sample()
        current = s
        if let kind = Pure.writeKind(lastWritten: lastWritten, current: s) {
            var row = s
            row.kind = kind
            emit(row)
        }
    }

    private func boundary(_ kind: String) {
        tickQueue.sync { self.writeBoundary(kind) }
    }

    /// Boundary rows are written unconditionally with the latest sensor values. `wake` and `active` resume
    /// activity, so they re-sample: the cached `current` is from before the gap and would record stale
    /// app/title/idle values against a fresh timestamp. `sleep`/`resign`/`term` keep `current`, which is
    /// what was in front of the user just before the boundary.
    private func writeBoundary(_ kind: String) {
        var row = (kind == "wake" || kind == "active") ? sample() : (current ?? sample())
        row.ts = nowTs()
        row.kind = kind
        emit(row)
    }

    private func emit(_ row: Sample) {
        lastWritten = row
        if dump {
            print(Pure.sampleJSON(row))
            fflush(stdout)
        } else {
            store?.write(row)
        }
    }
}

// MARK: - Menu bar

final class StatusBar: NSObject, NSMenuDelegate {
    private let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    private let menu = NSMenu()
    private let collector: Collector
    private var timer: Timer?
    private var pendingCalls: [Int: PendingCall] = [:]
    private var pendingStretches: [Int: PendingStretch] = [:]
    private var projects: String?
    private var picker: String?
    private var shown: Set<PendingID> = []    // pending rows whose panel already opened by itself (Pure.autoOpen)
    private var dialogQueue: [PendingID] = []
    private var clicked: Set<PendingID> = []  // menu clicks queued behind the open panel: never deferred by a call
    private var pendingIDs: Set<PendingID> = []
    private var inCall = false                // Pure.inCall of the latest sample, refreshed with the menu
    private var dialog: PendingPanel?

    init(collector: Collector) {
        self.collector = collector
        super.init()
        menu.delegate = self
        menu.autoenablesItems = false // rows have no action; auto-enabling would dim them all
        item.menu = menu
        item.button?.title = "stint"
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: TimeInterval(menuRefreshS), repeats: true) { [weak self] _ in self?.refresh() }
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    func refresh() {
        guard let store = collector.store else { return }
        let now = Date()
        collector.tickQueue.async { [self] in
            let data = store.menuData(now: now)
            let cur = collector.current
            let last = collector.lastWritten
            let buffered = store.bufferedCount
            let dbError = store.lastError
            DispatchQueue.main.async { self.render(data, current: cur, lastKind: last?.kind, buffered: buffered, dbError: dbError, nowTs: Int(now.timeIntervalSince1970)) }
        }
    }

    private func render(_ data: Store.MenuData, current: Sample?, lastKind: String?, buffered: Int, dbError: String?, nowTs: Int) {
        let rows = Pure.rollup(data.rows)
        let open = Pure.openIntervalSeconds(now: nowTs, lastTotalsUpdate: data.lastTotalsUpdate, idleS: current?.idleS, lastKind: lastKind)
        let todayTotal = rows.reduce(0) { $0 + $1.daySeconds } + open
        let warn = Pure.healthWarning(health: data.health, healthAt: data.healthAt, now: nowTs) || dbError != nil
        let title = (warn ? "⚠︎ " : "") + Pure.formatHours(todayTotal)
        item.button?.title = title
        log("menu title \(title) projects=\(rows.count) open=\(open)s health=\(data.health ?? "none")")

        menu.removeAllItems()
        menu.addItem(label("Today · Week", header: true))
        if rows.isEmpty { menu.addItem(label("no totals yet (brain has not run)")) }
        for r in rows {
            menu.addItem(label("\(r.project)  \(Pure.formatHours(r.daySeconds)) · \(Pure.formatHours(r.weekSeconds))"))
        }
        if open > 0 { menu.addItem(label("open, not yet attributed  +\(Pure.formatHours(open))")) }
        // A closed database reads as no pending rows: keep the pending state and the auto-open
        // bookkeeping until it is back, so the rows answered Later do not pop again on recovery.
        if dbError == nil {
            pendingCalls = Dictionary(uniqueKeysWithValues: data.pending.map { ($0.fromTs, $0) })
            pendingStretches = Dictionary(uniqueKeysWithValues: data.stretches.map { ($0.fromTs, $0) })
            projects = data.projects
            picker = data.picker
        }
        let items = Pure.mergedPending(calls: data.pending, stretches: data.stretches)
        if !items.isEmpty {
            menu.addItem(.separator())
            menu.addItem(label("To attribute (\(items.count))", header: true))
            for id in items {
                let row = NSMenuItem(title: menuTitle(id), action: #selector(openPending(_:)), keyEquivalent: "")
                row.target = self
                row.representedObject = id
                row.isEnabled = true
                menu.addItem(row)
            }
        }
        menu.addItem(.separator())
        let health = data.health ?? "never reported"
        let age = data.healthAt.map { "\(max(0, nowTs - $0) / 60) min ago" } ?? "—"
        menu.addItem(label("health: \(health) (\(age))"))
        if let e = dbError { menu.addItem(label("db: \(e); \(buffered) rows buffered")) }
        if let c = current {
            menu.addItem(label("now: \(c.app ?? "—")  idle \(c.idleS ?? -1)s" + (c.callApp.map { "  call: \($0)" } ?? "")))
        }
        guard dbError == nil else { return }
        pendingIDs = Set(items)
        clicked.formIntersection(pendingIDs)
        inCall = Pure.inCall(current)
        if let d = dialog, !isPending(d.id) { finish(d) }
        let next = Pure.autoOpen(shown: shown, pending: items)
        shown = next.shown
        for id in next.queue where !dialogQueue.contains(id) { dialogQueue.append(id) }
        presentNext()
    }

    private func isPending(_ id: PendingID) -> Bool {
        switch id.kind {
        case .call: return pendingCalls[id.fromTs] != nil
        case .stretch: return pendingStretches[id.fromTs] != nil
        }
    }

    private func menuTitle(_ id: PendingID) -> String {
        switch id.kind {
        case .call:
            guard let c = pendingCalls[id.fromTs] else { return "" }
            return Pure.callMenuTitle(app: Pure.callAppNames(c.callApp, lookup: appName), from: c.fromTs, to: c.toTs)
        case .stretch:
            guard let s = pendingStretches[id.fromTs] else { return "" }
            return Pure.stretchMenuTitle(from: s.fromTs, to: s.toTs, unattributedS: s.unattributedS)
        }
    }

    @objc private func openPending(_ sender: NSMenuItem) {
        guard let id = sender.representedObject as? PendingID, isPending(id) else { return }
        if dialog?.id == id { dialog?.show(activate: true); return }   // already showing: just bring it forward
        // A stretch deferred during a call is still queued; opened by hand it must not pop again after Later.
        dialogQueue.removeAll { $0 == id }
        if dialog == nil { present(id, activate: true); return }
        dialogQueue.insert(id, at: 0)
        clicked.insert(id)
    }

    /// Opens the next queued row that is still pending (one answered meanwhile, by Save or the CLI, is
    /// dropped); a stretch waits while a call is in progress and opens after it, unless it was clicked.
    private func presentNext() {
        guard dialog == nil else { return }
        let next = Pure.nextToPresent(queue: dialogQueue, pending: pendingIDs, inCall: inCall, clicked: clicked)
        dialogQueue = next.queue
        if let id = next.present { present(id, activate: clicked.remove(id) != nil) }
    }

    private func present(_ id: PendingID, activate: Bool) {
        switch id.kind {
        case .call: if let c = pendingCalls[id.fromTs] { presentCall(c, activate: activate) }
        case .stretch: if let s = pendingStretches[id.fromTs] { presentStretch(s, activate: activate) }
        }
    }

    private func presentStretch(_ s: PendingStretch, activate: Bool) {
        let d = StretchDialog(stretch: s, targets: Pure.pickerTargets(picker: picker, projects: projects), lookup: appName)
        d.onSave = { [weak self, weak d] answer in
            guard let self = self, let d = d, let store = self.collector.store else { return }
            switch store.setStretchAnswer(fromTs: s.fromTs, answer: answer, now: nowTs()) {
            case .saved:
                log("stretch \(s.fromTs) answered")
                self.finish(d)
                self.refresh()
            case .gone:
                log("stretch \(s.fromTs) was already attributed")
                d.showError("Already attributed")
                DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { [weak self, weak d] in
                    guard let self = self, let d = d, self.dialog === d else { return }
                    self.finish(d)
                    self.refresh()
                }
            case .failed(let err):
                log("stretch \(s.fromTs) answer failed: \(err)")
                d.showError(err)
            }
        }
        d.onLater = { [weak self, weak d] in
            guard let self = self, let d = d else { return }
            self.finish(d)
        }
        dialog = d
        d.show(activate: activate)
    }

    private func presentCall(_ c: PendingCall, activate: Bool) {
        let d = CallDialog(call: c, targets: Pure.pickerItems(picker: picker, projects: projects, guess: c.guessProject, guessSub: c.guessSubproject),
                           appName: Pure.callAppNames(c.callApp, lookup: appName))
        d.onSave = { [weak self, weak d] decision in
            guard let self = self, let d = d, let store = self.collector.store else { return }
            if let err = store.setDecision(fromTs: c.fromTs, decision, now: nowTs()) {
                log("call \(c.fromTs) decision failed: \(err)")
                d.showError(err)
                return
            }
            log("call \(c.fromTs) decided: \(Pure.decisionBinding(decision).decision)")
            self.finish(d)
            self.refresh()
        }
        d.onLater = { [weak self, weak d] in
            guard let self = self, let d = d else { return }
            self.finish(d)
        }
        dialog = d
        d.show(activate: activate)
    }

    private func finish(_ d: PendingPanel) {
        d.close()
        dialog = nil
        presentNext()
    }

    /// Localized app name for a bundle id (CFBundleDisplayName, then CFBundleName); nil when not installed.
    private func appName(_ bundleId: String) -> String? {
        guard let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: bundleId), let b = Bundle(url: url) else { return nil }
        return (b.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String) ?? (b.object(forInfoDictionaryKey: "CFBundleName") as? String)
    }

    /// A read-only row: no action, but enabled so AppKit draws it in the full label colour
    /// (disabled items are dimmed, unreadable over the translucent menu). Headers stay dimmed.
    private func label(_ title: String, header: Bool = false) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        i.isEnabled = !header
        return i
    }
}

// MARK: - Meeting attribution panel

/// What StatusBar needs from either pop-up panel.
protocol PendingPanel: AnyObject {
    var id: PendingID { get }
    func show(activate: Bool)
    func close()
}

/// Floating, non-modal, excluded from screen capture. Enter = Save, Escape / close box = Later.
final class CallDialog: NSObject, NSWindowDelegate, PendingPanel {
    var onSave: ((CallDecision) -> Void)?
    var onLater: (() -> Void)?
    var id: PendingID { PendingID(kind: .call, fromTs: call.fromTs) }
    private let panel: NSPanel
    private let call: PendingCall
    private let whole = NSButton(radioButtonWithTitle: "Whole meeting to:", target: nil, action: nil)
    private let split = NSButton(radioButtonWithTitle: "Split by what I was doing:", target: nil, action: nil)
    private let background = NSButton(radioButtonWithTitle: Pure.backgroundTitle, target: nil, action: nil)
    private let picker = NSPopUpButton(frame: .zero, pullsDown: false)
    private let errorLabel = NSTextField(labelWithString: "")
    private let saveButton = NSButton(title: "Save", target: nil, action: nil)
    private var shownOnce = false

    init(call: PendingCall, targets: [StretchTarget], appName: String) {
        self.call = call
        panel = NSPanel(contentRect: NSRect(x: 0, y: 0, width: 440, height: 10), styleMask: [.titled, .closable], backing: .buffered, defer: false)
        super.init()
        panel.title = "Attribute this meeting"
        panel.level = .floating
        panel.hidesOnDeactivate = false    // stay up when the user switches apps (e.g. into the call itself)
        panel.sharingType = .none          // never visible in a screen share
        panel.isReleasedWhenClosed = false
        panel.delegate = self

        let lines = Pure.parseBreakdown(call.breakdown)
        let subtitle = NSTextField(labelWithString: "\(appName) · \(Pure.callRange(from: call.fromTs, to: call.toTs))")
        for t in targets {
            let item = NSMenuItem(title: Pure.targetTitle(t), action: nil, keyEquivalent: "")
            item.representedObject = t
            picker.menu?.addItem(item)
        }
        let guess = StretchTarget.project(call.guessProject, sub: call.guessSubproject)
        if let i = targets.firstIndex(of: guess) { picker.selectItem(at: i) } else { picker.select(nil) }
        picker.target = self; picker.action = #selector(pickerChanged)
        whole.target = self; whole.action = #selector(radioChanged(_:)); whole.state = .on
        split.target = self; split.action = #selector(radioChanged(_:))
        split.isEnabled = Pure.splitEnabled(lines)
        background.target = self; background.action = #selector(radioChanged(_:))
        let wholeRow = NSStackView(views: [whole, picker]); wholeRow.orientation = .horizontal
        let breakdown = NSStackView(views: Pure.breakdownLines(lines, guess: call.guessProject).map { NSTextField(labelWithString: $0) })
        breakdown.orientation = .vertical; breakdown.alignment = .leading; breakdown.spacing = 2
        breakdown.edgeInsets = NSEdgeInsets(top: 0, left: 24, bottom: 0, right: 0)
        let backgroundBlock = NSStackView(views: Pure.backgroundLines(Pure.parseBackground(call.breakdown) ?? []).map { text in
            let field = NSTextField(labelWithString: text)
            if text.hasPrefix(Pure.awayLabel) { field.textColor = .secondaryLabelColor }
            return field
        })
        backgroundBlock.orientation = .vertical; backgroundBlock.alignment = .leading; backgroundBlock.spacing = 2
        backgroundBlock.edgeInsets = NSEdgeInsets(top: 0, left: 24, bottom: 0, right: 0)
        let later = NSButton(title: "Later", target: self, action: #selector(laterTapped)); later.keyEquivalent = "\u{1b}"
        saveButton.target = self; saveButton.action = #selector(saveTapped); saveButton.keyEquivalent = "\r"
        errorLabel.textColor = .systemRed
        errorLabel.lineBreakMode = .byTruncatingTail
        let buttons = NSStackView(views: [errorLabel, later, saveButton]); buttons.orientation = .horizontal
        buttons.setHuggingPriority(.defaultLow, for: .horizontal)
        let root = NSStackView(views: [subtitle, wholeRow, split, breakdown, background, backgroundBlock, buttons])
        root.orientation = .vertical; root.alignment = .leading; root.spacing = 10
        root.edgeInsets = NSEdgeInsets(top: 16, left: 20, bottom: 16, right: 20)
        panel.contentView = root
        panel.setContentSize(root.fittingSize)
        radioChanged(whole)
    }

    /// The radios live in different stack views, so AppKit does not group them: do it by hand.
    @objc private func radioChanged(_ sender: NSButton) {
        let picked = sender === split || sender === background ? sender : whole
        for r in [whole, split, background] { r.state = r === picked ? .on : .off }
        picker.isEnabled = whole.state == .on
        updateSave()
    }

    @objc private func pickerChanged() { updateSave() }

    private func updateSave() {
        saveButton.isEnabled = whole.state == .off || picker.indexOfSelectedItem >= 0
    }

    @objc private func saveTapped() {
        let choice: CallChoice = split.state == .on ? .split : background.state == .on ? .background : .whole
        let chosen = choice == .whole ? picker.selectedItem?.representedObject as? StretchTarget : nil
        guard let d = Pure.decision(chosen: chosen, choice: choice) else {
            showError("pick a project first"); return
        }
        onSave?(d)
    }
    @objc private func laterTapped() { onLater?() }
    func windowShouldClose(_ sender: NSWindow) -> Bool { onLater?(); return false }

    func showError(_ text: String) { errorLabel.stringValue = text }
    /// `activate: false` is the auto-open path (a fresh call becoming pending): it must never steal
    /// keyboard focus, so a Return typed in another app can't silently hit Save. `activate: true` is the
    /// menu-click path, where the user just asked for this panel and Enter/Escape should reach it.
    func show(activate: Bool) {
        if !shownOnce { panel.center(); shownOnce = true }
        if activate {
            NSApp.activate(ignoringOtherApps: true)   // accessory app: needed so Enter/Escape reach the panel
            panel.makeKeyAndOrderFront(nil)
        } else {
            panel.orderFrontRegardless()
        }
    }
    func close() { panel.delegate = nil; panel.close() }
}

// MARK: - Stretch attribution panel

/// Floating, non-modal, excluded from screen capture, like CallDialog. Enter = Save (enabled once every
/// row has a value), Escape / close box = Later.
final class StretchDialog: NSObject, NSWindowDelegate, PendingPanel {
    var onSave: ((String) -> Void)?
    var onLater: (() -> Void)?
    var id: PendingID { PendingID(kind: .stretch, fromTs: stretch.fromTs) }
    private static let followAllTag = 1, newSubprojectTag = 2
    private let panel: NSPanel
    private let stretch: PendingStretch
    private let rows: [StretchRow]
    private let labels: [String]
    private var targets: [StretchTarget]
    private var preset: StretchTarget?
    private var choices: [StretchTarget?]     // nil = "(all)": follow the preset
    private let allPicker = NSPopUpButton(frame: .zero, pullsDown: false)
    private var rowPickers: [NSPopUpButton] = []
    private var alwaysBoxes: [NSButton] = []
    private var pickerWidths: [NSLayoutConstraint] = []
    private let errorLabel = NSTextField(labelWithString: "")
    private let saveButton = NSButton(title: "Save", target: nil, action: nil)
    private var shownOnce = false

    init(stretch: PendingStretch, targets: [StretchTarget], lookup: (String) -> String?) {
        self.stretch = stretch
        self.targets = targets
        rows = Pure.stretchRows(Pure.parseStretchBreakdown(stretch.breakdown))
        labels = rows.map { Pure.rowLabel($0, lookup: lookup) }
        choices = Array(repeating: nil, count: rows.count)
        panel = NSPanel(contentRect: NSRect(x: 0, y: 0, width: 520, height: 10), styleMask: [.titled, .closable], backing: .buffered, defer: false)
        super.init()
        panel.title = "Attribute this time"
        panel.level = .floating
        panel.hidesOnDeactivate = false
        panel.sharingType = .none
        panel.isReleasedWhenClosed = false
        panel.delegate = self

        let subtitle = NSTextField(labelWithString: Pure.stretchRange(from: stretch.fromTs, to: stretch.toTs, unattributedS: stretch.unattributedS))
        allPicker.target = self; allPicker.action = #selector(allChanged)
        let allRow = NSStackView(views: [NSTextField(labelWithString: "Attribute all to:"), allPicker]); allRow.orientation = .horizontal
        let grid = NSGridView(numberOfColumns: 4, rows: 0)
        grid.rowSpacing = 6; grid.columnSpacing = 10
        for (i, row) in rows.enumerated() {
            let picker = NSPopUpButton(frame: .zero, pullsDown: false)
            picker.target = self; picker.action = #selector(rowChanged(_:)); picker.tag = i
            rowPickers.append(picker)
            let always = NSButton(checkboxWithTitle: "always", target: nil, action: nil)
            alwaysBoxes.append(always)
            grid.addRow(with: [NSTextField(labelWithString: labels[i]), NSTextField(labelWithString: Pure.minutesText(row.seconds)),
                               picker, row.isOther ? NSGridCell.emptyContentView : always])
        }
        if grid.numberOfRows > 0 { grid.column(at: 1).xPlacement = .trailing }
        let later = NSButton(title: "Later", target: self, action: #selector(laterTapped)); later.keyEquivalent = "\u{1b}"
        saveButton.target = self; saveButton.action = #selector(saveTapped); saveButton.keyEquivalent = "\r"
        errorLabel.textColor = .systemRed
        errorLabel.lineBreakMode = .byTruncatingTail
        let buttons = NSStackView(views: [errorLabel, later, saveButton]); buttons.orientation = .horizontal
        buttons.setHuggingPriority(.defaultLow, for: .horizontal)
        let root = NSStackView(views: [subtitle, allRow, grid, buttons])
        root.orientation = .vertical; root.alignment = .leading; root.spacing = 10
        root.edgeInsets = NSEdgeInsets(top: 16, left: 20, bottom: 16, right: 20)
        panel.contentView = root
        rebuildMenus()
        sync()
        fitPanel()
    }

    /// targets, a separator, Leave, New sub-project…; row pickers start with "(all)".
    private func fill(_ popup: NSPopUpButton, followAll: Bool) {
        popup.removeAllItems()
        guard let menu = popup.menu else { return }
        func item(_ title: String, _ target: StretchTarget?, tag: Int = 0) {
            let i = NSMenuItem(title: title, action: nil, keyEquivalent: "")
            i.representedObject = target
            i.tag = tag
            menu.addItem(i)
        }
        if followAll { item(Pure.followAllTitle, nil, tag: Self.followAllTag) }
        for t in targets { item(Pure.targetTitle(t), t) }
        menu.addItem(.separator())
        item(Pure.leaveTitle, .leave)
        item(Pure.newSubprojectTitle, nil, tag: Self.newSubprojectTag)
    }

    private func rebuildMenus() {
        fill(allPicker, followAll: false)
        for p in rowPickers { fill(p, followAll: true) }
        fitPickers()
    }

    /// A popup sizes to its selected title, so picking a long project would push the always column off a panel
    /// sized once at open: every picker gets the width of the longest title any of them offers.
    private func fitPickers() {
        let pickers = [allPicker] + rowPickers
        let font = allPicker.font ?? .systemFont(ofSize: NSFont.systemFontSize)
        let widest = pickers.flatMap(\.itemTitles).map { ($0 as NSString).size(withAttributes: [.font: font]).width }.max() ?? 0
        NSLayoutConstraint.deactivate(pickerWidths)
        pickerWidths = pickers.map { $0.widthAnchor.constraint(equalToConstant: ceil(widest) + 44) }
        NSLayoutConstraint.activate(pickerWidths)
        fitPanel()
    }

    /// A vertical stack's fitting width can drop its trailing inset, leaving the always column flush with the edge:
    /// size from the widest row plus both insets.
    private func fitPanel() {
        guard let root = panel.contentView as? NSStackView else { return }
        root.layoutSubtreeIfNeeded()
        let widest = root.arrangedSubviews.map(\.fittingSize.width).max() ?? 0
        let fit = root.fittingSize
        panel.setContentSize(NSSize(width: max(fit.width, ceil(widest) + root.edgeInsets.left + root.edgeInsets.right), height: fit.height))
    }

    private func select(_ popup: NSPopUpButton, _ t: StretchTarget?, followAll: Bool) {
        guard let t = t else {
            if followAll { popup.selectItem(withTag: Self.followAllTag) } else { popup.select(nil) }
            return
        }
        if let i = popup.itemArray.firstIndex(where: { ($0.representedObject as? StretchTarget) == t }) { popup.selectItem(at: i) } else { popup.select(nil) }
    }

    private func values() -> [StretchTarget?] { choices.map { Pure.effectiveTarget(choice: $0, preset: preset) } }

    /// Puts the model back on the controls: selections, the always ticks, Save.
    private func sync() {
        select(allPicker, preset, followAll: false)
        let vs = values()
        for (i, p) in rowPickers.enumerated() {
            select(p, choices[i], followAll: true)
            let ok = Pure.alwaysEnabled(rows[i], value: vs[i])
            alwaysBoxes[i].isEnabled = ok
            if !ok { alwaysBoxes[i].state = .off }
        }
        saveButton.isEnabled = Pure.saveEnabled(vs)
    }

    @objc private func allChanged() {
        if allPicker.selectedItem?.tag == Self.newSubprojectTag {
            askNewSubproject { [weak self] t in self?.preset = t }
            return
        }
        preset = allPicker.selectedItem?.representedObject as? StretchTarget
        sync()
    }

    @objc private func rowChanged(_ sender: NSPopUpButton) {
        let i = sender.tag
        if sender.selectedItem?.tag == Self.newSubprojectTag {
            askNewSubproject { [weak self] t in self?.choices[i] = t }
            return
        }
        choices[i] = sender.selectedItem?.representedObject as? StretchTarget   // "(all)" -> nil
        sync()
    }

    /// The New sub-project sheet: Add puts parent / name in this panel's picker and hands it to
    /// apply (the row or preset that opened the sheet). Nothing is written until Save.
    private func askNewSubproject(_ apply: @escaping (StretchTarget) -> Void) {
        let alert = NSAlert()
        alert.messageText = "New sub-project"
        alert.informativeText = "Created on Save; no rules.toml entry needed."
        let name = NSTextField(frame: NSRect(x: 0, y: 30, width: 240, height: 22))
        name.placeholderString = "name (a–z, 0–9, -)"
        let parent = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 240, height: 26), pullsDown: false)
        parent.addItems(withTitles: Pure.parentProjects(targets))
        let box = NSView(frame: NSRect(x: 0, y: 0, width: 240, height: 56))
        box.addSubview(name); box.addSubview(parent)
        alert.accessoryView = box
        alert.addButton(withTitle: "Add")
        alert.addButton(withTitle: "Cancel")
        alert.window.initialFirstResponder = name
        alert.window.sharingType = .none
        alert.beginSheetModal(for: panel) { [weak self] response in
            guard let self = self else { return }
            defer { self.sync() }
            guard response == .alertFirstButtonReturn, let p = parent.titleOfSelectedItem else { return }
            let n = name.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            if let err = Pure.subprojectNameError(n, known: Pure.knownSubprojects(self.targets, parent: p)) {
                self.showError(err)
                return
            }
            self.targets = Pure.addingSubproject(self.targets, parent: p, name: n)
            self.rebuildMenus()
            apply(.project(p, sub: n))
            self.showError("")
        }
    }

    @objc private func saveTapped() {
        guard let answer = Pure.answerJSON(rows: rows, values: values(), always: alwaysBoxes.map { $0.state == .on }, labels: labels) else {
            showError("choose where every row goes first"); return
        }
        onSave?(answer)
    }
    @objc private func laterTapped() { onLater?() }
    func windowShouldClose(_ sender: NSWindow) -> Bool { onLater?(); return false }

    func showError(_ text: String) { errorLabel.stringValue = text }
    /// Same focus rule as CallDialog.show: opening by itself never takes key focus.
    func show(activate: Bool) {
        if !shownOnce { panel.center(); shownOnce = true }
        if activate {
            NSApp.activate(ignoringOtherApps: true)
            panel.makeKeyAndOrderFront(nil)
        } else {
            panel.orderFrontRegardless()
        }
    }
    func close() { panel.delegate = nil; panel.close() }
}

// MARK: - selftest

/// TCC answers depend on the *responsible process*: run from a terminal, these lines reflect the terminal's grants.
/// The daemon logs the same lines at startup, so the authoritative result is in ~/Library/Logs/stint/collector.err.log after kickstart.
/// `redacted` is what the daemon logs: the window title and the two cwd values are user content and have no
/// business in a log file that lives forever, so those lines report presence only. The interactive
/// `--selftest` print keeps the values, which is the point of running it by hand.
func selftestLines(redacted: Bool = false) -> [String] {
    func presence(_ v: String?) -> String { v == nil ? "nil" : "set" }
    var out: [String] = []
    out.append("bundle id:          \(Bundle.main.bundleIdentifier ?? "MISSING (Info.plist section not embedded)")")
    out.append("AXIsProcessTrusted: \(AXIsProcessTrusted())")
    for b in Bundles.browsers {
        guard let app = NSRunningApplication.runningApplications(withBundleIdentifier: b).first else {
            out.append("automation \(b): not running (start it and re-run to test)")
            continue
        }
        let target = NSAppleEventDescriptor(processIdentifier: app.processIdentifier)
        guard let desc = target.aeDesc else {   // runs on every daemon start under KeepAlive; never force-unwrap here
            out.append("automation \(b): unavailable")
            continue
        }
        let status = AEDeterminePermissionToAutomateTarget(desc, typeWildCard, typeWildCard, false)
        let meaning: String
        switch status {
        case 0: meaning = "allowed"
        case -1744: meaning = "not yet asked (errAEEventWouldRequireUserConsent)"
        case -1743: meaning = "DENIED (errAEEventNotPermitted) -> tccutil reset AppleEvents dev.asafdafna.stint.collector"
        default: meaning = "status \(status)"
        }
        out.append("automation \(b): \(meaning)")
    }
    out.append("idle CGEventSource: \(Sensors.idleSeconds().map(String.init) ?? "nil")  IOKit: \(Sensors.idleSecondsIOKit().map(String.init) ?? "nil")")
    out.append("remote input:       \(Sensors.remoteInput().map { "\($0.tty) age=\($0.age)s" } ?? "none")")
    out.append("call_app:           \(Sensors.callApp() ?? "none")")
    out.append("herdr socket:       \(Herdr.reachable() ? "reachable (pong)" : "unreachable at \(Paths.herdrSocket)")")
    let herdrCwd = Herdr.foregroundCwd()
    out.append("herdr cwd:          \(redacted ? presence(herdrCwd) : (herdrCwd ?? "nil"))")
    let hookCwd = Sensors.cwdFromHookFiles()
    out.append("hook cwd:           \(redacted ? presence(hookCwd) : (hookCwd ?? "nil (no files in \(Paths.cwdDir))"))")
    if let f = Sensors.frontmost() {
        let t = Pure.normalizeTitle(Sensors.focusedWindowTitle(pid: f.processIdentifier))
        out.append("frontmost:          \(f.bundleIdentifier ?? "?") title=\(redacted ? presence(t) : (t ?? "nil"))")
    }
    return out
}

func selftest() {
    print("stint collector selftest (TCC lines reflect the launching terminal's grants; see the daemon log for the real ones)")
    _ = Sensors.frontmostCache
    for line in selftestLines() { print(line) }
    let store = Store()
    print("db \(Paths.db): \(store.isOpen ? "writable, schema applied" : "NOT writable: \(store.lastError ?? "?")")")
}

// MARK: - main

let args = CommandLine.arguments.dropFirst()
if let bad = args.first(where: { $0 != "--selftest" && $0 != "--dump" }) {
    FileHandle.standardError.write("stint-collector: unrecognised argument \(bad)\nusage: stint-collector [--selftest|--dump]\n".data(using: .utf8)!)
    exit(2)
}
if args.contains("--selftest") {
    selftest()
    exit(0)
}
let dumpMode = args.contains("--dump")
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let collector = Collector(dump: dumpMode)
collector.start()
var statusBar: StatusBar?
if !dumpMode {
    statusBar = StatusBar(collector: collector)
    log("stint collector started, db \(Paths.db)")
    if !AXIsProcessTrusted() && getppid() == 1 {
        // Only when launchd spawned us: the prompt then lists the app bundle in System Settings and the grant lands on it.
        // From a terminal the grant would land on the terminal, so no prompt there. AXIsProcessTrusted() alone never prompts.
        let opts = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        _ = AXIsProcessTrustedWithOptions(opts)
    }
    for line in selftestLines(redacted: true) { log("selftest " + line) }
}
app.run()

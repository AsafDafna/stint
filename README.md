# stint

stint tracks your working hours per project on one Mac. The **collector** (Swift, menu bar, always on) samples
the frontmost app, window title, browser URL and terminal directory every 5 s into a local SQLite file. The
**brain** (Go, run by launchd every 5 min) applies your rules to the samples and writes per-project intervals,
daily and weekly totals, optional phone pushes and weekly markdown reports.

stint is a personal tool, shared as-is. It runs on macOS only, and there is no promise of support or fixes.
It is released under the MIT license (see `LICENSE`).

## Requirements

- macOS. stint is developed on macOS 27; older versions are untested.
- Xcode Command Line Tools (`xcode-select --install`), for `swiftc`, `codesign`, `xcrun` and the macOS SDK.
- Go 1.27.1 or newer (see `brain/go.mod`).
- The SQLite that ships with macOS. The brain links `/usr/lib/libsqlite3`, not Homebrew's.
- Optional: an [ntfy](https://ntfy.sh) topic for pushes to your phone.

Clone your fork and work from its root; every command below is relative to it:

```
git clone <your fork URL> stint
cd stint
```

## Collector

### One-time: signing identity

TCC (Accessibility, Automation) keys its grants to the code signature. Ad-hoc signatures change on every
rebuild and silently drop the grants, so the collector is signed with a persistent self-signed identity:

1. `open -a "Keychain Access"`
2. Menu **Keychain Access → Certificate Assistant → Create a Certificate…**
3. Name `stint`, Identity Type **Self Signed Root**, Certificate Type **Code Signing**. Leave
   "Let me override defaults" unchecked. **Create**, then **Done**.
4. In the *login* keychain, category **My Certificates**, double-click `stint`, expand **Trust**, set
   **Code Signing** to **Always Trust**, close the window and enter your login password.
5. Verify: `security find-identity -v -p codesigning` lists `"stint"`.

The first `codesign` asks for keychain access to the `stint` key: choose **Always Allow**.
`build.sh` reads the identity name from `STINT_SIGN_IDENTITY` (default `stint`).

### Build, test, install

```
collector/test.sh     # pure-function tests (swiftc, no Xcode)
collector/build.sh    # -> ~/Applications/stint-collector.app, signed
```

The collector is a minimal app bundle (`Contents/MacOS/stint-collector` + `Contents/Info.plist`), not a bare
binary, so TCC keys its grants by bundle id `dev.asafdafna.stint.collector` and `tccutil reset … <bundle id>`
works. The bundle id and the launchd labels `dev.asafdafna.stint.*` work on any Mac as they are. Nothing runs
from `collector/.build/`; `build.sh` installs to `~/Applications` on every run, then `launchctl kickstart -k`
picks the new build up.

### Install (LaunchAgent) and grant permissions

TCC attributes a grant to the *responsible process*. A collector started from a terminal is the terminal's
child and the grant lands on the terminal. So install the LaunchAgent first and let launchd start it. The
plists in `launchd/` hold the placeholder `/Users/YOUR_USER`; the `sed` line writes your home directory in:

```
mkdir -p ~/Library/Logs/stint
mkdir -p ~/Library/LaunchAgents
sed "s|/Users/YOUR_USER|$HOME|g" launchd/dev.asafdafna.stint.collector.plist > ~/Library/LaunchAgents/dev.asafdafna.stint.collector.plist
launchctl bootout gui/$UID/dev.asafdafna.stint.collector 2>/dev/null || true
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.asafdafna.stint.collector.plist
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
```

Then answer the prompts the launchd-spawned collector raises:

1. **Accessibility** prompt ("stint collector would like to control this computer…"): Open System Settings
   and enable `stint collector` under Privacy & Security → Accessibility (on macOS 27 this section is under
   **Device Control and Data Access** instead).
2. **Automation** prompts appear the first time each of Wavebox, Chrome and Search (a web browser, bundle id
   `com.officecommun.search`) is frontmost. Switch to each browser once and click **OK** on "stint collector
   wants access to control Wavebox / Google Chrome / Search". Browser URLs come only from these three; in other
   browsers only the window title is recorded, so match them with `title_regex`.

Check the result in the collector's own log (the authoritative view; `--selftest` from a terminal shows the
terminal's grants, not the collector's):

```
grep selftest ~/Library/Logs/stint/collector.err.log | tail -10
```

Expect `AXIsProcessTrusted: true` and `automation com.bookry.wavebox: allowed` / `automation com.google.Chrome: allowed`
/ `automation com.officecommun.search: allowed`.
Restart after granting Accessibility so the AX title sampler picks it up:
`launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector`.

Exclude the database from Time Machine once it exists. Exclude the *directory*, not the file: SQLite's
`-wal` and `-shm` companions are separate files, and an exclusion on `stint.db` alone leaves them backed up.

```
tmutil addexclusion "$HOME/Library/Application Support/stint"
tmutil isexcluded "$HOME/Library/Application Support/stint"
```

### Terminal directories

With herdr (a terminal multiplexer) in Ghostty, the collector asks herdr's socket for the focused pane's
directory. For Ghostty without herdr, Terminal, iTerm2 and Warp, add these lines to `~/.zshrc`. Other
terminals record no directory.

```zsh
# stint: expose the active shell's cwd to the collector (fallback for terminals other than herdr/Ghostty)
_stint_cwd() { local d="$HOME/.local/state/stint/cwd"; [[ -d "$d" ]] || command mkdir -p -m 700 "$d"; ( umask 077; print -rn -- "$PWD" > "$d/${${TTY:t}:-notty}" ) }
autoload -Uz add-zsh-hook && add-zsh-hook chpwd _stint_cwd && add-zsh-hook precmd _stint_cwd
```

The `umask 077` is scoped to the subshell that does the write, so the cwd files are 0600 and the shell's own
umask is untouched. The collector's DB directory is 0700 and the DB 0600 for the same reason: paths, window
titles and URLs are private.

Accepted imprecision: the newest file is the shell that most recently printed a prompt, which may be an
unfocused pane; the misattribution lasts until the focused shell prints its next prompt.

### Recovering a denied permission

A "Don't Allow" on Automation never re-prompts. Reset, then kickstart and answer again:

```
tccutil reset AppleEvents dev.asafdafna.stint.collector
tccutil reset Accessibility dev.asafdafna.stint.collector
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
```

### Everyday commands

| Want | Run |
|---|---|
| Rebuild, reinstall and restart | See **Rebuild while it is running** below |
| Watch live samples from a terminal (headless, no DB writes; reflects the terminal's grants) | `~/Applications/stint-collector.app/Contents/MacOS/stint-collector --dump` |
| Watch what the daemon wrote | `sqlite3 "$HOME/Library/Application Support/stint/stint.db" "select id,kind,app,title,url,cwd,idle_s,call_app from samples order by id desc limit 20"` |
| Sensor and permission report | `~/Applications/stint-collector.app/Contents/MacOS/stint-collector --selftest` (TCC lines reflect the terminal; see the log) |
| Logs | `tail -f ~/Library/Logs/stint/collector.err.log` |
| Stop | `launchctl bootout gui/$UID/dev.asafdafna.stint.collector` |
| List recent calls and their pop-up state | `stint calls` |
| Answer a call from the shell | `stint calls set <from_ts> --project NAME` (or `NAME/SUB`, e.g. `acme/acme-app`) or `--split` (applied on the next run). A bare `NAME` equal to the guess's project keeps the guess's sub-project |
| Answer a call as background from the shell | `stint calls set <from_ts> --background` |
| List unattributed stretches and the learned rules | `stint stretches` |
| Ask about an answered stretch again | `stint stretches reopen <from_ts>` (the panel returns after the next run) |

**Rebuild while it is running.** `build.sh` replaces `~/Applications/stint-collector.app` with `ditto`, which
fails on a bundle whose binary is in use. Stop the agent first, and bring it back with `bootstrap` so launchd
picks up the new bundle:

```
launchctl bootout gui/$UID/dev.asafdafna.stint.collector
collector/build.sh
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.asafdafna.stint.collector.plist
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
```

Wait ~30 s, then confirm the TCC grants survived the rebuild (they do as long as the signing identity is
unchanged): `grep selftest ~/Library/Logs/stint/collector.err.log | tail -10` shows `AXIsProcessTrusted: true`
and `automation com.bookry.wavebox: allowed`.

### What the collector records

`call_app` lists every app holding the mic (comma-joined, helper processes resolved to their app); the brain's
`call_apps` list decides which of them count as a call.

Remote work (iPad over mosh into herdr) is tracked too: the collector treats a herdr client tty's atime (advances
only on a keystroke) as a second input signal, and while the Mac is locked it writes those samples as
`app = remote.herdr` with the focused pane's title and cwd, so the usual cwd rules attribute them.

After every call of at least 5 minutes the brain writes a pending `calls` row and the menu bar opens
"Attribute this meeting": whole meeting to one project or sub-project (the same list as the stretch pop-up,
the brain's guess prefilled) or split per sample, with the split shown first. A sub-project item books both;
a top-level item books no sub-project. "Later" keeps the row under "To attribute (n)" in the menu; the ledger
keeps the guess until you answer. Answers are applied by the next brain run (≤ 5 min) with a recompute
from the call's day.

After every stretch with at least 10 unattributed minutes (closed by idle, the lock screen, a call, or a
gap of `gap_minutes`) the brain writes a pending `stretches` row and the menu bar opens "Attribute this time":
one row per site or app (at most 8, the rest under "other"), an "Attribute all to…" preset that starts empty
(Save waits until every row has a value), "Leave unattributed", "New sub-project…", and an **always** tick that
turns the answer into a learned rule (off for browsers, terminals, localhost and IP hosts). Calls and stretches
share "To attribute (n)" in the menu, and the 08:00 push names both while any wait. Answers apply on the next
brain run.

The menu bar shows today's hours (from `totals`, written by the brain, plus the open interval since its last
run). A `⚠︎` prefix means the brain has not reported `health = ok` in the last 30 min, or the collector cannot
write the database (rows are buffered in memory and retried every 60 s).

## Brain (Go)

The brain reads the collector's database every 5 minutes and writes intervals, totals,
health, ntfy pushes and weekly reports. It never creates the database: install and run
the collector first.

Build and install (links the libsqlite3 macOS ships; needs Command Line Tools):

```
brain/build.sh        # -> ~/.local/bin/stint
```

The `stint` commands below assume `~/.local/bin` is on your `PATH`.

Tests (same SQLite engine as the binary):

```
brain/test.sh
```

Upgrades: when a new version changes the database schema, rebuild and restart the collector first, then install
the new brain. A brain refuses a database whose schema version it does not know and sends a health push, so
expect one push between the two steps.

### Config

Create the rules file from the example, from the repository root:

```
mkdir -p ~/.config/stint
cp rules.example.toml ~/.config/stint/rules.toml
```

Then replace its example projects and rules with your own. Every setting is documented in the file. For pushes,
set `ntfy_topic` to a random string of at least 20 characters (the topic is the password) and subscribe to it in
the ntfy app; an empty topic disables pushes.
Weekly reports go to `vault_reports_dir`; the brain creates the directory on the first report.

Days and weeks use the Mac's system timezone, in the brain and in the menu bar. `week_start` (`"sunday"` …
`"saturday"`, default `"sunday"`) is the first day of the week for totals, pushes, the menu bar and the weekly
reports. After a change, the next run rebuilds every total with the new week. Weekly report files written before
the change can keep the old week; `stint recompute --since <date>` rewrites every week from that date. An older
`rules.toml` with a `timezone = …` line fails to load: delete that line before you install this brain.

Rule keys beyond the conditions: `subproject = "name"` books a fixed sub-project (with `cwd_prefix` it replaces the
next path segment), and `from` / `until = "YYYY-MM-DD"` (local days, inclusive, either optional) limit a rule to
those days. A dated rule is the durable way to correct past days, since `stint recompute` re-applies it; put it
above the rule it corrects. Learned rules never get dates.

Learned rules: ticking **always** in the stretch pop-up makes the brain append a rule at the end of `rules.toml`,
under a `# added by stint pop-up <date> | stretch <start> | <label>` line, and keep the file as it was before that
run in `rules.toml.bak`. Appended rules come last in first-match order, so a hand-written rule above them still
wins. To undo a wrong one, edit or delete it and run `stint recompute --since <the stretch's date>` (the
`stretch …` part of its marker; the marker's own date can be a day later); `stint stretches` lists every learned
rule with its hours over the last 7 and 30 days.

### LaunchAgent

```
mkdir -p ~/Library/Logs/stint
sed "s|/Users/YOUR_USER|$HOME|g" launchd/dev.asafdafna.stint.brain.plist > ~/Library/LaunchAgents/dev.asafdafna.stint.brain.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.asafdafna.stint.brain.plist
launchctl kickstart gui/$(id -u)/dev.asafdafna.stint.brain
tail -n 5 ~/Library/Logs/stint/brain.log ~/Library/Logs/stint/brain.err.log
```

### Everyday commands

```
stint explain --yesterday                 # tune rules daily during the first month
stint recompute --since 2026-09-14        # after editing rules.toml (rebuilds intervals and totals)
stint explain 2026-09-16T10:00:00         # why was that moment (local time) credited where it was
stint report --week 2026-09-16            # write and print the weekly markdown
stint run                                 # one manual pipeline run
stint stretches                           # pop-up stretches, learned rules and their hours (7d/30d)
stint help                                # every command and flag
```

After `build.sh` the next scheduled run uses the new binary; no reload needed. To stop:
`launchctl bootout gui/$(id -u)/dev.asafdafna.stint.brain`.

# stint

stint tracks your working hours per project on one Mac. The **collector** (Swift, menu bar, always on) samples
the frontmost app, window title, browser URL and terminal directory every 5 s into a local SQLite file. The
**brain** (Go, run by launchd every 5 min) applies your rules to the samples and writes per-project intervals,
daily and weekly totals, optional phone pushes and weekly markdown reports.

stint is a personal tool, shared as-is. It runs on macOS only, and there is no promise of support or fixes.
It is released under the MIT license (see `LICENSE`).

## For coding agents

If you are an AI coding agent installing stint for someone, follow these rules:

- Do the steps in **Install** in order. Run each step's check, and go on only when it passes.
- **Person** marks an action only the person can do: a click in a macOS dialog or in System Settings, a
  password, or a look at the menu bar. Tell them exactly what to do *before* you run the command that triggers
  it. Wait until they confirm, then run the check.
- Ask the person before each optional step, and before you edit `~/.zshrc` or any other shell startup file.
- Before you write `~/.config/stint/rules.toml`, ask which projects the person tracks and how to recognize each
  one (folders, sites, apps). Do not keep the example projects. Keep the `[settings]` defaults unless the person
  asks for a change.
- Browser URLs come only from Wavebox, Google Chrome and Search. A browser the person does not use shows
  `not running` in the checks; that is not a failure.
- Never print, log or commit the `ntfy_topic` value: the topic is the password for the pushes. The person reads
  it from the file.
- If a check fails, read **Troubleshooting** before you change any code.

## Install

### Requirements

- macOS. stint is developed on macOS 27; older versions are untested.
- Xcode Command Line Tools, for `swiftc`, `codesign`, `xcrun` and the macOS SDK. Install them with
  `xcode-select --install` (**Person**: it opens an installer window).
- Go 1.27.1 or newer (see `brain/go.mod`). Install it with `brew install go`, or with the package from
  [go.dev/dl](https://go.dev/dl) (**Person**: the package asks for an admin password).
- The SQLite that ships with macOS. The brain links `/usr/lib/libsqlite3`, not Homebrew's.
- Optional: the [ntfy](https://ntfy.sh) app on a phone, for pushes.

Check: `sw_vers -productVersion` prints a version, `xcode-select -p` prints a path, and `go version` prints
go1.27.1 or newer.

If you are not already in a clone, clone the repository (or your fork) and work from its root. Every command
below runs from the repository root:

```
git clone https://github.com/AsafDafna/stint.git
cd stint
```

### 1. Create the signing identity (Person)

macOS keys the Accessibility and Automation grants to the code signature. An ad-hoc signature changes on every
rebuild and silently drops the grants, so the collector is signed with a persistent self-signed identity:

1. `open -a "Keychain Access"`
2. Menu **Keychain Access → Certificate Assistant → Create a Certificate…**
3. Name `stint`, Identity Type **Self Signed Root**, Certificate Type **Code Signing**. Leave
   "Let me override defaults" unchecked. Click **Create**, then **Done**.
4. In the *login* keychain, category **My Certificates**, double-click `stint`, expand **Trust**, set
   **Code Signing** to **Always Trust**, close the window and enter your login password.

Check: `security find-identity -v -p codesigning` lists `"stint"`.

`collector/build.sh` reads the identity name from `STINT_SIGN_IDENTITY` (default `stint`).

### 2. Build and install the collector

```
collector/test.sh     # pure-function tests (swiftc, no Xcode)
```

Check: the exit status is 0, and the last line reads `N/N checks passed` with both numbers equal.

**Person**, before the next command: the first `codesign` opens a dialog that asks for the login keychain
password to use the `stint` key. Enter it and click **Always Allow**. The command waits until then.

```
collector/build.sh    # builds, signs and installs ~/Applications/stint-collector.app
```

Check: the exit status is 0, and the last line reads `installed …/Applications/stint-collector.app`.

### 3. Start the collector with launchd

macOS gives a grant to the *responsible process*. A collector started from a terminal is the terminal's child,
so the grant would land on the terminal. Let launchd start it instead. The plists in `launchd/` hold the
placeholder `/Users/YOUR_USER`; the `sed` line writes your home directory in.

**Person**, before the commands: the collector asks for Accessibility as soon as it starts. Grant it now, or
leave the dialog for step 4.

```
mkdir -p ~/Library/Logs/stint ~/Library/LaunchAgents
sed "s|/Users/YOUR_USER|$HOME|g" launchd/dev.asafdafna.stint.collector.plist > ~/Library/LaunchAgents/dev.asafdafna.stint.collector.plist
launchctl bootout gui/$UID/dev.asafdafna.stint.collector 2>/dev/null || true
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.asafdafna.stint.collector.plist
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
```

Check: `launchctl print gui/$UID/dev.asafdafna.stint.collector | grep 'state ='` shows `state = running`.

**Person** check: within 60 s a new menu bar item shows hours with a `⚠︎` prefix. The prefix stays until the
brain first runs (step 8).

### 4. Grant the permissions (Person)

1. **Accessibility.** Open System Settings and enable `stint collector` under Privacy & Security →
   Accessibility (on macOS 27 this section is under **Device Control and Data Access**).
2. **Automation.** Do this for each browser you use among Wavebox (`com.bookry.wavebox`), Google Chrome
   (`com.google.Chrome`) and Search (a web browser, `com.officecommun.search`): bring it to the front once, and
   click **OK** on "stint collector wants access to control …". Other browsers record only the window title,
   so rules match them with `title_regex`.

Keep each browser you use open. Then restart the collector, so it picks up the grants and logs a fresh
self-test:

```
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
sleep 10
grep -E 'selftest (AXIsProcessTrusted|automation)' ~/Library/Logs/stint/collector.err.log | tail -4
```

Check: the lines show `AXIsProcessTrusted: true`, and `automation <bundle id>: allowed` for each browser the
person uses. Other values:

- `not running`: the browser was closed at the restart. This is fine for a browser the person does not use.
  Otherwise open it and restart again.
- `not yet asked`: bring the browser to the front again, answer the prompt, and restart again.
- `DENIED`: someone clicked "Don't Allow"; see **Troubleshooting**.

Use the log, not `stint-collector --selftest` from a terminal: the terminal run shows the terminal's grants,
not the collector's.

### 5. Optional: keep the database out of Time Machine (ask the person)

The database exists once the collector has run. Exclude the *directory*, not the file: SQLite's `-wal` and
`-shm` companions are separate files, and an exclusion on `stint.db` alone leaves them backed up.

```
tmutil addexclusion "$HOME/Library/Application Support/stint"
```

Check: `tmutil isexcluded "$HOME/Library/Application Support/stint"` prints `[Excluded]`.

### 6. Optional: terminal directories (ask the person)

Ask which terminal and shell the person uses. With herdr (a terminal multiplexer) in Ghostty, the collector
asks herdr's socket for the focused pane's directory; skip this step. For zsh in Ghostty without herdr,
Terminal, iTerm2 or Warp, add these lines to `~/.zshrc`. Other terminals and shells record no directory.

```zsh
# stint: expose the active shell's cwd to the collector (fallback for terminals other than herdr/Ghostty)
_stint_cwd() { local d="$HOME/.local/state/stint/cwd"; [[ -d "$d" ]] || command mkdir -p -m 700 "$d"; ( umask 077; print -rn -- "$PWD" > "$d/${${TTY:t}:-notty}" ) }
autoload -Uz add-zsh-hook && add-zsh-hook chpwd _stint_cwd && add-zsh-hook precmd _stint_cwd
```

**Person** check: open a new terminal window. Then `ls ~/.local/state/stint/cwd` lists at least one file.

### 7. Build and install the brain

```
brain/test.sh         # same SQLite engine as the binary
brain/build.sh        # builds ~/.local/bin/stint
```

Check: both exit with status 0, and `brain/test.sh` prints no `FAIL` line. The first run also prints
`go: downloading …` lines; they are normal. `brain/build.sh` ends with `built …/.local/bin/stint`.

The checks below call `~/.local/bin/stint` by its full path. For everyday use, `~/.local/bin` must be on the
person's `PATH`. If it is not, offer to add `export PATH="$HOME/.local/bin:$PATH"` to `~/.zshrc`.

### 8. Write the rules file (ask the person)

```
mkdir -p ~/.config/stint
cp rules.example.toml ~/.config/stint/rules.toml
```

Replace the example projects and rules with the person's own, and keep the `[settings]` defaults unless they
ask for a change. Every setting is documented in the file; see also **Configuration** below. To find an app's
bundle id for an `app` rule, run `osascript -e 'id of app "Slack"'`.

Pushes are optional; ask the person. To turn them on, write a random topic into the file without printing it:

```
T=$(openssl rand -hex 16); sed -i '' "s/^ntfy_topic = \"\"/ntfy_topic = \"$T\"/" ~/.config/stint/rules.toml; unset T
```

Check: `grep -cE '^ntfy_topic = "[0-9a-f]{32}"' ~/.config/stint/rules.toml` prints `1`.

**Person:** copy the `ntfy_topic` value from the file and subscribe to it in the ntfy app. To open the file
for them, run `open -e ~/.config/stint/rules.toml` (Finder hides `~/.config`). An empty topic disables pushes.

Then run the brain once by hand:

```
~/.local/bin/stint run
```

Check: the exit status is 0. On the first run it prints `first run: built intervals from the oldest sample
(N rows)`, which is normal. A `health: …` line names a problem with the collector and keeps the exit status
0; see **Troubleshooting**. An error that names `rules.toml` means the file does not load: fix it and run
again.

### 9. Start the brain with launchd

```
launchctl bootout gui/$UID/dev.asafdafna.stint.brain 2>/dev/null || true
sed "s|/Users/YOUR_USER|$HOME|g" launchd/dev.asafdafna.stint.brain.plist > ~/Library/LaunchAgents/dev.asafdafna.stint.brain.plist
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.asafdafna.stint.brain.plist
launchctl kickstart gui/$UID/dev.asafdafna.stint.brain
```

Check, after 10 s:

```
launchctl print gui/$UID/dev.asafdafna.stint.brain | grep -E 'runs =|last exit code ='
test -f ~/Library/Logs/stint/brain.err.log && ! test -s ~/Library/Logs/stint/brain.err.log && echo "error log empty"
sqlite3 "$HOME/Library/Application Support/stint/stint.db" "select value from meta where key = 'health'"
```

Expect `runs = 1` or more, `last exit code = 0`, `error log empty`, and `ok`. Any other health value names the
problem; see **Troubleshooting**. Right after install, `title NULL on …` is expected: check again in an hour,
and do not change any code for it. Outside `active_hours` (default 07:00–23:00) health is always `ok`, so a
night install proves less.

**Person** check: within 60 s the menu bar item loses its `⚠︎` prefix.

## Using stint

| Want | Run |
|---|---|
| Tune rules (daily during the first month) | `stint explain --yesterday` |
| Rebuild intervals and totals after editing `rules.toml` | `stint recompute --since 2026-09-14` |
| Why a moment (local time) was credited where it was | `stint explain 2026-09-16T10:00:00` |
| Write and print the weekly report | `stint report --week 2026-09-16` |
| One manual pipeline run | `stint run` |
| List recent calls and their pop-up state | `stint calls` |
| Answer a call from the shell | `stint calls set <from_ts> --project NAME` (or `NAME/SUB`, e.g. `acme/acme-app`) or `--split` (applied on the next run). A bare `NAME` equal to the guess's project keeps the guess's sub-project |
| Answer a call as background from the shell | `stint calls set <from_ts> --background` |
| List unattributed stretches and the learned rules (hours over 7 and 30 days) | `stint stretches` |
| Ask about an answered stretch again | `stint stretches reopen <from_ts>` (the panel returns after the next run) |
| Every command and flag | `stint help` |
| Watch live samples from a terminal (headless, no DB writes; reflects the terminal's grants) | `~/Applications/stint-collector.app/Contents/MacOS/stint-collector --dump` |
| Watch what the collector wrote | `sqlite3 "$HOME/Library/Application Support/stint/stint.db" "select id,kind,app,title,url,cwd,idle_s,call_app from samples order by id desc limit 20"` |
| Sensor and permission report (the permission lines reflect the terminal; the log is the real view) | `~/Applications/stint-collector.app/Contents/MacOS/stint-collector --selftest` |
| Collector logs | `tail -f ~/Library/Logs/stint/collector.err.log` |
| Brain logs (`brain.log` holds the health, first-run, push and report lines) | `tail -n 5 ~/Library/Logs/stint/brain.log ~/Library/Logs/stint/brain.err.log` |
| Stop the collector | `launchctl bootout gui/$UID/dev.asafdafna.stint.collector` |
| Stop the brain | `launchctl bootout gui/$UID/dev.asafdafna.stint.brain` |

### Rebuild after a code change

The brain needs no reload: after `brain/build.sh` the next scheduled run uses the new binary.

The collector must be stopped first. `build.sh` replaces `~/Applications/stint-collector.app` with `ditto`,
which fails on a bundle whose binary is in use. Bring it back with `bootstrap` so launchd picks up the new
bundle:

```
launchctl bootout gui/$UID/dev.asafdafna.stint.collector
collector/build.sh
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.asafdafna.stint.collector.plist
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
```

Wait about 30 s, then run the check from Install step 4. The grants survive a rebuild as long as the signing
identity is unchanged.

When a new version changes the database schema, rebuild and restart the collector first, then install the new
brain. A brain refuses a database whose schema version it does not know and sends a health push, so expect one
push between the two steps.

## Configuration

`~/.config/stint/rules.toml` documents every setting. A few need more words:

- **Timezone and week.** Days and weeks use the Mac's system timezone, in the brain and in the menu bar.
  `week_start` (`"sunday"` … `"saturday"`, default `"sunday"`) is the first day of the week for totals, pushes,
  the menu bar and the weekly reports. After a change, the next run rebuilds every total with the new week.
  Weekly report files written before the change can keep the old week; `stint recompute --since <date>`
  rewrites every week from that date.
- **Weekly reports** go to `vault_reports_dir`; the brain creates the directory on the first report. Keep it
  out of `~/Desktop`, `~/Documents` and `~/Downloads`: macOS privacy protection can block the launchd brain
  there.
- **Rule keys beyond the conditions.** `subproject = "name"` books a fixed sub-project (with `cwd_prefix` it
  replaces the next path segment). `from` / `until = "YYYY-MM-DD"` (local days, inclusive, either optional)
  limit a rule to those days. A dated rule is the durable way to correct past days, since `stint recompute`
  re-applies it; put it above the rule it corrects. Learned rules never get dates.
- **Learned rules.** Ticking **always** in the stretch pop-up makes the brain append a rule at the end of
  `rules.toml`, under a `# added by stint pop-up <date> | stretch <start> | <label>` line, and keep the file as
  it was before that run in `rules.toml.bak`. Appended rules come last in first-match order, so a hand-written
  rule above them still wins. To undo a wrong one, edit or delete it and run
  `stint recompute --since <the stretch's date>` (the `stretch …` part of its marker; the marker's own date can
  be a day later). `stint stretches` lists every learned rule with its hours over the last 7 and 30 days.

## How it works

The collector is a minimal app bundle (`Contents/MacOS/stint-collector` + `Contents/Info.plist`), not a bare
binary, so macOS keys its grants by the bundle id `dev.asafdafna.stint.collector` and `tccutil reset … <bundle
id>` works. The bundle id and the launchd labels `dev.asafdafna.stint.*` work on any Mac as they are. Nothing
runs from `collector/.build/`.

The brain reads the collector's database every 5 minutes and writes intervals, totals, health, ntfy pushes and
weekly reports. It never creates the database, which is why the collector is installed first.

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

The shell hook's `umask 077` is scoped to the subshell that does the write, so the cwd files are 0600 and the
shell's own umask is untouched. The collector's database directory is 0700 and the database 0600 for the same
reason: paths, window titles and URLs are private. Accepted imprecision: the newest cwd file is the shell that
most recently printed a prompt, which may be an unfocused pane; the misattribution lasts until the focused shell
prints its next prompt.


## Troubleshooting

**A denied permission.** A "Don't Allow" on Automation never asks again. Reset, restart, and answer the prompts
again (Install step 4):

```
tccutil reset AppleEvents dev.asafdafna.stint.collector
tccutil reset Accessibility dev.asafdafna.stint.collector
launchctl kickstart -k gui/$UID/dev.asafdafna.stint.collector
```

**Grants gone after a rebuild.** The signing identity changed. Check that
`security find-identity -v -p codesigning` lists `"stint"`, rebuild, then grant again (Install step 4).

**The brain does not run.** `~/Library/Logs/stint/brain.err.log` names the cause. A missing or invalid
`~/.config/stint/rules.toml` stops every run; `stint run` prints the same error. `schema version mismatch`
means the collector is older than the brain: rebuild the collector first.

**`meta.health` is not `ok`.** The value lists the reasons, separated by `; `. For example:

- `no samples in the database`: the collector is not running. Check Install step 3.
- `title NULL on N% of samples in the last hour (Accessibility grant?)`: the collector records no window
  titles without the Accessibility grant. Right after a fresh install this clears within an hour of the grant.
  If it stays, check Install step 4.

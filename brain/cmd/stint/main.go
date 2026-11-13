// Command stint is the brain: it turns collector samples into per-project hours.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/health"
	"stint/internal/notify"
	"stint/internal/rules"
)

const (
	exitOK      = 0
	exitError   = 1
	exitHorizon = 2 // asked for a time before the oldest retained sample
)

const usage = `usage: stint [--config PATH] [--db PATH] <command>

commands:
  run                        ingest → attribute → totals → health → notify → report → purge
  recompute --since DATE     rebuild intervals from DATE (YYYY-MM-DD, local) after a rules change
  explain <moment>           show the sample and the rule that claimed that moment;
                             <moment> is unix seconds, RFC3339, or YYYY-MM-DDTHH:MM:SS (local)
  explain --yesterday        yesterday's top unattributed titles/URLs
  report --week DATE         write and print the weekly markdown for the week containing DATE
  calls                      list the last 30 days of calls with their guess and decision
  calls set <from_ts> --project NAME[/SUB] | --split | --background
                             answer a call's pop-up from the shell; applied on the next run
  stretches                  list the last 30 days of unattributed stretches and the learned rules
  stretches reopen <from_ts> clear a stretch's answer; the pop-up asks again after the next run
  help                       print this text (also --help, -h)

defaults: --config ~/.config/stint/rules.toml, --db ~/Library/Application Support/stint/stint.db
exit codes: 0 ok, 1 error, 2 time before the retention horizon
`

// app holds what every subcommand needs.
type app struct {
	cfg     *config.Config
	cfgPath string // rules.toml: learned rules are appended to it and it is reloaded after
	sq      *sql.DB
	matcher *rules.Matcher
	stdout  io.Writer
	stderr  io.Writer
	now     time.Time
}

// commands maps a command name to its handler; every handler needs the open database.
var commands = map[string]func(*app, []string) error{
	"run":       (*app).run,
	"recompute": (*app).recompute,
	"explain":   (*app).explain,
	"report":    (*app).report,
	"calls":     (*app).callsCmd,
	"stretches": (*app).stretchesCmd,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now()))
}

// run is main without os.Exit so tests can drive it. argv is parsed and the command
// chosen before the config or the database is touched, so usage questions and typos are
// answered without either: --help/-h/help print usage to stdout and exit 0; no command
// or an unknown one prints usage to stderr and exits 1.
func run(args []string, stdout, stderr io.Writer, now time.Time) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	fs := flag.NewFlagSet("stint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // printed below, on the stream the outcome calls for
	configPath := fs.String("config", config.DefaultPath(home), "rules file")
	dbPath := fs.String("db", db.DefaultPath(home), "database file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return exitOK
		}
		fmt.Fprint(stderr, usage)
		return exitError
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, usage)
		return exitError
	}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	if cmd == "help" {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	command, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
		return exitError
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	sq, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer sq.Close()
	a := &app{cfg: cfg, cfgPath: *configPath, sq: sq, matcher: rules.New(cfg), stdout: stdout, stderr: stderr, now: now}
	if err := db.CheckSchema(sq); err != nil {
		if cmd == "run" && errors.Is(err, db.ErrSchema) {
			a.schemaFailure(err)
		}
		fmt.Fprintln(stderr, err)
		return exitError
	}
	err = command(a, rest)
	switch {
	case errors.Is(err, flag.ErrHelp): // the subcommand's FlagSet already printed its usage
		return exitOK
	case errors.Is(err, db.ErrBeforeHorizon):
		fmt.Fprintln(stderr, err)
		return exitHorizon
	case err != nil:
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return exitOK
}

// noArgs rejects stray positional arguments left after a subcommand's flags.
func noArgs(cmd string, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%s: unexpected argument %q", cmd, args[0])
	}
	return nil
}

// schemaFailure records the mismatch for the menu bar and pushes the daily health alert.
func (a *app) schemaFailure(err error) {
	reason := health.SchemaReason(err)
	if werr := health.WriteMeta(a.sq, []string{reason}, a.now); werr != nil {
		fmt.Fprintln(a.stderr, werr)
	}
	if s := a.sender(); s != nil {
		if _, perr := notify.PushHealth(a.sq, a.cfg, s, []string{reason}, a.now); perr != nil {
			fmt.Fprintln(a.stderr, perr)
		}
	}
}

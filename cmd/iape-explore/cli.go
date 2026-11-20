package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"iape-ui/internal/eval/infrastructure/config"
	"iape-ui/internal/explore"
)

// Exit codes.
const (
	exitOK     = 0
	exitUsage  = 1 // wrong usage or settings
	exitFailed = 2 // a path did not end where it should
	exitInfra  = 4 // the browser could not be opened or signed in
)

// defaultEnvFile is the settings file when none is named. It may be missing: the settings are then the environment's alone.
const defaultEnvFile = ".env"

// deps is what the command is given, so that a test can give it a page that is not a browser.
type deps struct {
	stdout, stderr io.Writer
	loadConfig     func(envFile string) (config.Config, error)
	open           func(ctx context.Context, cfg config.Config, settle time.Duration) (session, error)
	now            func() time.Time
}

type options struct {
	envFile string
	pathID  string
	all     bool
	list    bool
	out     string
	contact string
	amount  string
	settle  time.Duration
	headed  bool
	steps   []string
}

// usageError is a mistake in the call.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("iape-explore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.envFile, "env", defaultEnvFile, "settings file")
	fs.StringVar(&o.pathID, "path", "", "a path of the catalog (see -list)")
	fs.BoolVar(&o.all, "all", false, "run every path of the catalog")
	fs.BoolVar(&o.list, "list", false, "list the paths and exit")
	fs.StringVar(&o.out, "out", "", "directory for the records (default temp/iape-explore/<time>)")
	fs.StringVar(&o.contact, "contact", "", `contact to pick in a payment, such as "Maria Torres" (default the first)`)
	fs.StringVar(&o.amount, "amount", "", "amount in soles for a payment (default the path's)")
	fs.BoolVar(&o.headed, "headed", false, "show Chrome: run it with a window, to watch the run")
	fs.DurationVar(&o.settle, "wait", 90*time.Second, "how long a step waits for the page to settle")
	if err := fs.Parse(args); err != nil {
		return o, usageError{msg: err.Error()}
	}
	o.steps = fs.Args()
	return o, o.check()
}

func (o options) check() error {
	chosen := 0
	for _, set := range []bool{o.list, o.all, o.pathID != "", len(o.steps) > 0} {
		if set {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return usageError{"nothing to do: pass -list, -path ID, -all, or steps"}
	case chosen > 1:
		return usageError{"pass one of -list, -path, -all, or steps"}
	case o.settle <= 0:
		return usageError{"-wait takes a duration above zero"}
	case o.amount != "" && !explore.ValidAmount(o.amount):
		return usageError{fmt.Sprintf("-amount %q is not an amount of soles", o.amount)}
	case strings.ContainsFunc(o.contact, unicode.IsControl):
		return usageError{"-contact has a control character"}
	}
	if o.pathID != "" {
		if _, ok := explore.Find(o.pathID); !ok {
			return usageError{fmt.Sprintf("no path %q (see -list)", o.pathID)}
		}
	}
	if len(o.steps) > 0 {
		if _, err := explore.ParseAll(o.steps); err != nil {
			return usageError{err.Error()}
		}
	}
	return nil
}

// plan is a path to run: its id and its steps, as they are written.
type plan struct {
	id    string
	steps []string
}

func (o options) plans() []plan {
	switch {
	case o.all:
		var out []plan
		for _, p := range explore.Paths() {
			out = append(out, plan{p.ID, p.Steps(o.amount, o.contact)})
		}
		return out
	case o.pathID != "":
		p, _ := explore.Find(o.pathID)
		return []plan{{p.ID, p.Steps(o.amount, o.contact)}}
	}
	return []plan{{"custom", o.steps}}
}

func (d deps) defaultOut() string {
	return filepath.Join("temp", "iape-explore", d.now().Format("20060102-150405"))
}

func (d deps) run(ctx context.Context, args []string) int {
	opts, err := parseOptions(args, d.stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(d.stderr, "iape-explore:", err)
		}
		return exitUsage
	}
	if opts.list {
		d.list()
		return exitOK
	}
	cfg, err := d.loadConfig(opts.envFile)
	if err == nil {
		err = cfg.RequireUser()
	}
	if err != nil {
		fmt.Fprintln(d.stderr, "iape-explore:", err)
		return exitUsage
	}
	out := opts.out
	if out == "" {
		out = d.defaultOut()
	}

	if opts.headed {
		cfg.MCP.Args = slices.DeleteFunc(slices.Clone(cfg.MCP.Args), func(a string) bool { return a == "--headless" })
	}
	sess, err := d.open(ctx, cfg, opts.settle)
	if err != nil {
		fmt.Fprintln(d.stderr, "iape-explore: opening the browser:", err)
		return exitInfra
	}
	defer func() { _ = sess.Close(context.WithoutCancel(ctx)) }()

	return d.runPlans(ctx, sess, opts.plans(), out, cfg.Eval.UserPassword.Reveal())
}

func (d deps) list() {
	for _, p := range explore.Paths() {
		fmt.Fprintf(d.stdout, "  %-8s %s\n", p.ID, p.Title)
	}
}

// runPlans runs each plan in a conversation of its own, records it under out/<id>, and writes a summary. The exit code is the worst.
func (d deps) runPlans(ctx context.Context, sess session, plans []plan, out, password string) int {
	var summary strings.Builder
	summary.WriteString("| Path | Result | Steps | Took | Why |\n|---|---|---|---|---|\n")
	code := exitOK
	for _, p := range plans {
		start := d.now()
		rep := d.runPlan(ctx, sess, p, filepath.Join(out, p.id), password)
		took := d.now().Sub(start).Round(time.Millisecond)
		verdict, why := "PASSED", ""
		if f := rep.Failure(); f != nil {
			verdict, why, code = "FAILED", fmt.Sprintf("step %d: %v", f.N, f.Err), exitFailed
		}
		fmt.Fprintf(d.stdout, "%-8s %s (%d steps, %s)\n", p.id, verdict, len(rep.Steps), took)
		if why != "" {
			fmt.Fprintf(d.stdout, "         %s\n", why)
		}
		fmt.Fprintf(&summary, "| %s | %s | %d | %s | %s |\n", p.id, verdict, len(rep.Steps), took, strings.ReplaceAll(why, "|", `\|`))
		if ctx.Err() != nil {
			break
		}
	}
	fmt.Fprintf(d.stdout, "records: %s\n", out)
	if err := writeFile(filepath.Join(out, "summary.md"), summary.String()); err != nil {
		fmt.Fprintln(d.stderr, "iape-explore:", err)
	}
	return code
}

func (d deps) runPlan(ctx context.Context, sess session, p plan, dir, password string) explore.Report {
	steps, err := explore.ParseAll(p.steps)
	if err == nil {
		err = sess.Fresh(ctx)
	}
	if err != nil {
		return explore.Report{Path: p.id, Steps: []explore.StepResult{{N: 1, Raw: "(start)", Err: err}}}
	}
	sink := &dirSink{dir: dir, secrets: []string{password}}
	rep := (&explore.Runner{Browser: sess, Sink: sink}).Run(ctx, p.id, steps)
	if err := writeFile(filepath.Join(dir, "report.md"), rep.Markdown()); err != nil {
		fmt.Fprintln(d.stderr, "iape-explore:", err)
	}
	if sink.redacted > 0 {
		fmt.Fprintf(d.stderr, "iape-explore: %d secrets were taken out of the records of %s\n", sink.redacted, p.id)
	}
	return rep
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

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
	loadSettings   func(envFile string) (settings, error)
	open           func(ctx context.Context, s settings, o openOptions) (session, error)
	now            func() time.Time
}

type options struct {
	envFile  string
	pathID   string
	all      bool
	list     bool
	out      string
	contact  string
	amount   string
	settle   time.Duration
	headless bool
	// withGuard adds the paths that burn the account to -all.
	withGuard bool
	steps     []string
}

// usageError is a mistake in the call.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// helpRequested is what parseOptions returns for -h: not a mistake in the call.
var helpRequested = flag.ErrHelp

func parseOptions(args []string) (options, error) {
	var o options
	fs := newFlagSet(&o)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, helpRequested
		}
		return o, usageError{msg: err.Error()}
	}
	if err := checkPathFlag(fs, o); err != nil {
		return o, err
	}
	o.steps = fs.Args()
	if err := flagAmongSteps(args, o.steps); err != nil {
		return o, err
	}
	return o, o.check()
}

// checkPathFlag refuses -path given with no id: it would otherwise be taken for a flag that was not passed.
func checkPathFlag(fs *flag.FlagSet, o options) error {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "path" })
	if given && o.pathID == "" {
		return usageError{"-path takes the id of a path (see -list)"}
	}
	return nil
}

// flagAmongSteps refuses a step that starts with a dash. The flag package stops at the first argument that is no flag, so a flag
// written after the steps ("Hola -headless") would be sent to the agent as a message. After "--" the dash is a character like any
// other: the way to send a message that starts with one.
func flagAmongSteps(args, steps []string) error {
	if len(steps) == 0 {
		return nil
	}
	if before := len(args) - len(steps) - 1; before >= 0 && args[before] == "--" {
		return nil
	}
	for _, step := range steps {
		if strings.HasPrefix(step, "-") {
			return usageError{fmt.Sprintf("%q comes after the steps, where it is a message, not a flag: flags must come before the steps "+
				"(use -- before the steps to send a message that starts with a dash)", step)}
		}
	}
	return nil
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
	// burns says the path counts against the account's guard: see explore.Path.Burns.
	burns bool
}

func (o options) plans() []plan {
	switch {
	case o.all:
		var out []plan
		for _, p := range explore.Paths() {
			if p.Burns && !o.withGuard {
				continue
			}
			out = append(out, plan{id: p.ID, steps: p.Steps(o.amount, o.contact), burns: p.Burns})
		}
		return out
	case o.pathID != "":
		p, _ := explore.Find(o.pathID)
		return []plan{{id: p.ID, steps: p.Steps(o.amount, o.contact), burns: p.Burns}}
	}
	return []plan{{id: "custom", steps: o.steps}}
}

func (d deps) defaultOut() string {
	return filepath.Join("temp", "iape-explore", d.now().Format("20060102-150405"))
}

func (d deps) run(ctx context.Context, args []string) int {
	opts, err := parseOptions(args)
	if errors.Is(err, helpRequested) {
		_, _ = fmt.Fprint(d.stdout, helpText())
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "iape-explore:", err)
		_, _ = fmt.Fprintln(d.stderr, "Try: iape-explore -h")
		return exitUsage
	}
	if opts.list {
		d.list()
		return exitOK
	}
	cfg, err := d.loadSettings(opts.envFile)
	if err == nil {
		err = cfg.requireUser()
	}
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "iape-explore:", err)
		return exitUsage
	}
	out := opts.out
	if out == "" {
		out = d.defaultOut()
	}
	// A run that cannot keep its records is no run: it is said before Chrome is opened.
	if err := os.MkdirAll(out, 0o700); err != nil {
		_, _ = fmt.Fprintf(d.stderr, "iape-explore: the records cannot be kept in %s: %v\n", out, err)
		return exitInfra
	}

	sess, err := d.open(ctx, cfg, openOptions{Headless: opts.headless, Settle: opts.settle})
	if err != nil {
		_, _ = fmt.Fprintln(d.stderr, "iape-explore: opening the browser:", err)
		return exitInfra
	}
	defer func() { _ = sess.Close(context.WithoutCancel(ctx)) }()

	plans := opts.plans()
	for _, p := range plans {
		if p.burns {
			_, _ = fmt.Fprintf(d.stderr, "iape-explore: %s trips the agent's guard, which counts against the account: a few runs in a short time can get it banned, "+
				"and its ban can last as long as the service is set to.\n", p.id)
		}
	}
	if opts.all && !opts.withGuard {
		_, _ = fmt.Fprintln(d.stderr, "iape-explore: -all leaves out guard, which can get the account banned; add -with-guard to run it")
	}
	return d.runPlans(ctx, sess, plans, out, cfg.Password.Reveal())
}

func (d deps) list() {
	for _, p := range explore.Paths() {
		_, _ = fmt.Fprintf(d.stdout, "  %-8s %s\n", p.ID, p.Title)
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
			// An error can carry what the page showed: it goes through the same filter as the records, on the screen too.
			verdict, why, code = "FAILED", (&dirSink{secrets: []string{password}}).clean(fmt.Sprintf("step %d: %v", f.N, f.Err)), exitFailed
		}
		_, _ = fmt.Fprintf(d.stdout, "%-8s %s (%d steps, %s)\n", p.id, verdict, len(rep.Steps), took)
		if why != "" {
			_, _ = fmt.Fprintf(d.stdout, "         %s\n", why)
		}
		_, _ = fmt.Fprintf(&summary, "| %s | %s | %d | %s | %s |\n", p.id, verdict, len(rep.Steps), took, cell(why))
		if ctx.Err() != nil {
			break
		}
	}
	_, _ = fmt.Fprintf(d.stdout, "records: %s\n", out)
	if err := writeFile(filepath.Join(out, "summary.md"), summary.String()); err != nil {
		_, _ = fmt.Fprintln(d.stderr, "iape-explore: the records cannot be kept:", err)
		if code == exitOK {
			code = exitInfra // the paths passed, and nothing says so
		}
	}
	return code
}

// cell makes text fit a cell of a Markdown table: one line, with its pipes and backslashes escaped.
func cell(s string) string {
	return strings.NewReplacer(`\`, `\\`, "|", `\|`, "\n", " ", "\r", " ").Replace(s)
}

func (d deps) runPlan(ctx context.Context, sess session, p plan, dir, password string) explore.Report {
	steps, err := explore.ParseAll(p.steps)
	if err == nil && len(steps) == 0 {
		err = errors.New("the plan has no steps: a run that checks nothing proves nothing")
	}
	if err == nil {
		err = sess.Fresh(ctx)
	}
	if err != nil {
		return explore.Report{Path: p.id, Steps: []explore.StepResult{{N: 1, Raw: "(start)", Err: err}}}
	}
	sink := &dirSink{dir: dir, secrets: []string{password}}
	rep := (&explore.Runner{Browser: sess, Sink: sink}).Run(ctx, p.id, steps)
	if err := writeFile(filepath.Join(dir, "report.md"), sink.clean(rep.Markdown())); err != nil {
		_, _ = fmt.Fprintln(d.stderr, "iape-explore:", err)
	}
	if sink.redacted > 0 {
		_, _ = fmt.Fprintf(d.stderr, "iape-explore: %d secrets were taken out of the records of %s\n", sink.redacted, p.id)
	}
	return rep
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

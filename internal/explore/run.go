package explore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"
)

// Browser is the page the run drives, the way a person does. Each call is one thing a person does or sees; how it is done (Chrome
// DevTools, say) is the adapter's.
type Browser interface {
	// Type puts text in the composer after what is there, without sending.
	Type(ctx context.Context, text string) error
	// Send presses Send.
	Send(ctx context.Context) error
	// Pick chooses a contact in the @ picker: the first, or the first with the text.
	Pick(ctx context.Context, text string) error
	// Click presses the first button, option or link whose name has the text. found is false when there is none.
	Click(ctx context.Context, text string) (found bool, err error)
	// FillOTP types the code in the field of the card that asks for one and confirms it.
	FillOTP(ctx context.Context, code string) error
	// Settle waits until no reply is being written and the page has stopped changing.
	Settle(ctx context.Context) error
	// Text is the page as a person who cannot see it reads it: its accessibility tree, one element a line.
	Text(ctx context.Context) (string, error)
	// Cards are the cards on the page, oldest first.
	Cards(ctx context.Context) ([]Card, error)
	// Requests are the requests the page made, one a line.
	Requests(ctx context.Context) (string, error)
	// Screenshot saves the page as a PNG at path.
	Screenshot(ctx context.Context, path string) error
}

// Sink is where the run puts what it records. Path says where the file of a step goes, for what writes it itself (a screenshot);
// Write saves bytes there.
type Sink interface {
	Path(n int, name, ext string) string
	Write(n int, name, ext string, data []byte) error
}

// Runner follows steps through a page.
type Runner struct {
	Browser Browser
	Sink    Sink
	// Sleep pauses a wait step; nil sleeps for real.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now tells the time; nil is the clock.
	Now func() time.Time
}

// StepResult is what one step did.
type StepResult struct {
	N    int
	Raw  string
	Kind Kind
	Took time.Duration
	// Err is why the step failed; nil when it did what it said.
	Err error
	// NewLines are the lines the page shows that it did not show after the step before.
	NewLines []string
}

// Report is how a run went.
type Report struct {
	Path  string
	Steps []StepResult
}

// Passed reports whether every step did what it said.
func (r Report) Passed() bool { return r.Failure() == nil }

// Failure is the step that failed, or nil.
func (r Report) Failure() *StepResult {
	for i := range r.Steps {
		if r.Steps[i].Err != nil {
			return &r.Steps[i]
		}
	}
	return nil
}

// Run follows the steps. The first step that fails ends the run, after what the page shows then is recorded: it is the evidence.
// After each step the run records the page's accessibility tree, a screenshot and the requests the page made.
func (r *Runner) Run(ctx context.Context, id string, steps []Step) Report {
	rep := Report{Path: id}
	// What the page shows before the first step is the line the first step's news are measured from.
	seen := map[string]int{}
	if base, err := r.Browser.Text(ctx); err == nil {
		newLines(seen, uidPrefix.ReplaceAllString(base, "$1"))
	}
	for i, st := range steps {
		res := StepResult{N: i + 1, Raw: strings.TrimSpace(st.Raw), Kind: st.Kind}
		start := r.now()
		res.Err = r.act(ctx, st)

		var text string
		if ctx.Err() == nil {
			text = r.record(ctx, res.N, slug(st), &res)
		}
		if res.Err == nil && ctx.Err() == nil {
			res.Err = r.expect(ctx, st, text)
		}
		if res.Err == nil && ctx.Err() != nil {
			res.Err = ctx.Err()
		}
		res.Took = r.now().Sub(start)
		res.NewLines = newLines(seen, text)
		rep.Steps = append(rep.Steps, res)
		if res.Err != nil {
			break
		}
	}
	return rep
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// act does what a step does to the page, and lets the page settle when it may have changed.
func (r *Runner) act(ctx context.Context, st Step) error {
	b := r.Browser
	settle := func(err error) error {
		if err != nil {
			return err
		}
		return b.Settle(ctx)
	}
	switch st.Kind {
	case KindMessage:
		if err := b.Type(ctx, st.Arg); err != nil {
			return err
		}
		return settle(b.Send(ctx))
	case KindType:
		return b.Type(ctx, st.Arg)
	case KindSend:
		return settle(b.Send(ctx))
	case KindPick:
		return b.Pick(ctx, st.Arg)
	case KindClick, KindClickIf:
		found, err := b.Click(ctx, st.Arg)
		if err == nil && !found && st.Kind == KindClick {
			err = fmt.Errorf("no button, option or link with %q on the page", st.Arg)
		}
		return settle(err)
	case KindOTP:
		return settle(b.FillOTP(ctx, st.Arg))
	case KindWait:
		return r.sleep(ctx, st.Wait)
	}
	return nil // the expectations look, they do not touch
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordTimeout bounds the recording of a step: what the page shows is saved even when the step took the time it had.
const recordTimeout = 30 * time.Second

var uidPrefix = regexp.MustCompile(`(?m)^(\s*)uid=\d+_\d+\s*`)

// record saves what the page shows after a step and returns the page's text, without the uids that change from one snapshot to
// the next. A page that cannot be read at this moment does not fail the step: the step did what it was asked.
func (r *Runner) record(ctx context.Context, n int, name string, res *StepResult) string {
	rctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	text, err := r.Browser.Text(rctx)
	if err != nil {
		text = ""
	} else {
		text = uidPrefix.ReplaceAllString(text, "$1")
		_ = r.Sink.Write(n, name, "snapshot.txt", []byte(text))
	}
	if reqs, err := r.Browser.Requests(rctx); err == nil {
		_ = r.Sink.Write(n, name, "network.txt", []byte(reqs))
	}
	_ = r.Browser.Screenshot(rctx, r.Sink.Path(n, name, "png"))
	return text
}

// expect checks what an expectation says against the page.
func (r *Runner) expect(ctx context.Context, st Step, text string) error {
	switch st.Kind {
	case KindExpect:
		if !strings.Contains(text, st.Arg) {
			return fmt.Errorf("the page does not show %q", st.Arg)
		}
	case KindExpectCard, KindExpectNoCard:
		cards, err := r.Browser.Cards(ctx)
		if err != nil {
			return fmt.Errorf("reading the cards of the page: %w", err)
		}
		if st.Kind == KindExpectNoCard {
			if len(cards) > 0 {
				return fmt.Errorf("expected no card, but the page has: %s", describe(cards))
			}
			return nil
		}
		for _, c := range slicesBackward(cards) {
			if c.Kind == st.Card.Kind {
				if c.State == st.Card.State {
					return nil
				}
				break
			}
		}
		return fmt.Errorf("expected card %s, but the page has: %s", st.Card, describe(cards))
	}
	return nil
}

func describe(cards []Card) string {
	if len(cards) == 0 {
		return "no card"
	}
	parts := make([]string, len(cards))
	for i, c := range cards {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}

// slicesBackward is the cards newest first.
func slicesBackward(cards []Card) []Card {
	out := make([]Card, len(cards))
	for i, c := range cards {
		out[len(cards)-1-i] = c
	}
	return out
}

// newLines are the lines of text that the page did not show after the step before, which is what seen holds; it is updated.
func newLines(seen map[string]int, text string) []string {
	now := map[string]int{}
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		now[line]++
		if now[line] > seen[line] {
			out = append(out, line)
		}
	}
	clear(seen)
	maps.Copy(seen, now)
	return out
}

var notAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slug names the files of a step after what the step says.
func slug(st Step) string {
	s := strings.Trim(notAlnum.ReplaceAllString(strings.ToLower(strings.TrimSpace(st.Raw)), "-"), "-")
	if r := []rune(s); len(r) > 32 {
		s = strings.TrimRight(string(r[:32]), "-")
	}
	return cmp.Or(s, string(st.Kind))
}

// Markdown is the report as a page: one row for each step, and why the run failed if it did.
func (r Report) Markdown() string {
	var b strings.Builder
	verdict := "PASSED"
	if !r.Passed() {
		verdict = "FAILED"
	}
	fmt.Fprintf(&b, "# %s: %s\n\n| # | Step | Result | Took | New on the page |\n|---|---|---|---|---|\n", r.Path, verdict)
	for _, s := range r.Steps {
		result := "ok"
		if s.Err != nil {
			result = "FAILED: " + s.Err.Error()
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", s.N, cell(s.Raw), cell(result), s.Took.Round(time.Millisecond), cell(summary(s.NewLines)))
	}
	if f := r.Failure(); f != nil {
		fmt.Fprintf(&b, "\nStep %d failed: %v\n", f.N, f.Err)
		if errors.Is(f.Err, context.Canceled) {
			b.WriteString("The run was interrupted.\n")
		}
	}
	return b.String()
}

// summary is the first lines of what appeared, for a table cell.
func summary(lines []string) string {
	const shown = 3
	if len(lines) <= shown {
		return strings.Join(lines, "; ")
	}
	return strings.Join(lines[:shown], "; ") + fmt.Sprintf("; … (%d more)", len(lines)-shown)
}

// cell makes text fit a cell of a Markdown table.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

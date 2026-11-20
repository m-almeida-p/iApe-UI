package explore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/explore"
)

// stubBrowser is a fakeBrowser whose reads can fail, and whose text can change from one read to the next.
type stubBrowser struct {
	fakeBrowser
	textErr, cardsErr, requestsErr, screenshotErr, settleErr error
	// texts are the successive answers of Text: the first is the page before the first step. The last one is repeated.
	texts     []string
	textCalls int
	// textFails says which calls of Text (counting from 1) fail.
	textFails map[int]bool
	cardsCall int
}

func (s *stubBrowser) Text(context.Context) (string, error) {
	s.textCalls++
	if s.textErr != nil || s.textFails[s.textCalls] {
		return "", cmpErr(s.textErr, "the accessibility tree is not there")
	}
	if len(s.texts) == 0 {
		return s.text, nil
	}
	return s.texts[min(s.textCalls-1, len(s.texts)-1)], nil
}

func cmpErr(err error, fallback string) error {
	if err != nil {
		return err
	}
	return errors.New(fallback)
}

func (s *stubBrowser) Cards(context.Context) ([]explore.Card, error) {
	s.cardsCall++
	return s.cards, s.cardsErr
}
func (s *stubBrowser) Requests(context.Context) (string, error) { return "GET /x [200]", s.requestsErr }
func (s *stubBrowser) Screenshot(_ context.Context, path string) error {
	_ = s.note("screenshot " + path)
	return s.screenshotErr
}
func (s *stubBrowser) Settle(ctx context.Context) error {
	_ = s.note("settle")
	if s.settleErr != nil {
		return s.settleErr
	}
	return nil
}

// failingSink fails every write.
type failingSink struct{ memorySink }

func (f *failingSink) Write(int, string, string, []byte) error { return errors.New("the disk is full") }

func TestRun_ZeroStepsIsAnEmptyReportThatHasNothingToFail(t *testing.T) {
	for name, in := range map[string][]explore.Step{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			b := &stubBrowser{}

			rep := runner(b, newSink()).Run(t.Context(), "nothing", in)

			assert.Empty(t, rep.Steps)
			assert.Nil(t, rep.Failure())
			assert.True(t, rep.Passed(), "a run with no step has no step that failed; the command refuses to start one")
			assert.Equal(t, "nothing", rep.Path)
			assert.Contains(t, rep.Markdown(), "# nothing: PASSED")
			for _, call := range b.calls {
				assert.NotContains(t, call, "type", "no step: nothing is done to the page")
			}
		})
	}
}

func TestReport_EmptyReport(t *testing.T) {
	var rep explore.Report

	assert.Nil(t, rep.Failure())
	assert.True(t, rep.Passed())
	assert.NotPanics(t, func() { _ = rep.Markdown() })
	assert.Contains(t, rep.Markdown(), "PASSED")
}

func TestRun_AContextThatIsAlreadyCanceledDoesNothingToThePage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b := &stubBrowser{}
	sink := newSink()

	rep := runner(b, sink).Run(ctx, "d", steps(t, "Hola", "send"))

	require.False(t, rep.Passed())
	require.Len(t, rep.Steps, 1, "the first step ends the run")
	assert.ErrorIs(t, rep.Failure().Err, context.Canceled)
	assert.Equal(t, 1, rep.Failure().N)
	for _, call := range b.calls {
		assert.False(t, strings.HasPrefix(call, "type") || call == "send", "a canceled run must not send a message: %s", call)
	}
	assert.Empty(t, sink.files, "nothing was done, so nothing is recorded")
	assert.Contains(t, rep.Markdown(), "The run was interrupted.")
}

func TestRun_ACancelDuringSettleStopsTheRunWithoutRecording(t *testing.T) {
	t.Run("settle gives up with the context's error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		b := &stubBrowser{}
		b.onCall = func(call string) {
			if call == "settle" {
				cancel()
				b.settleErr = ctx.Err()
			}
		}
		sink := newSink()

		rep := runner(b, sink).Run(ctx, "d", steps(t, "Hola", "send"))

		require.False(t, rep.Passed())
		assert.ErrorIs(t, rep.Failure().Err, context.Canceled)
		assert.Len(t, rep.Steps, 1)
		assert.Empty(t, sink.files, "a page that is being abandoned is not written down")
	})
	t.Run("settle returns normally but the context is gone", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		b := &stubBrowser{}
		b.onCall = func(call string) {
			if call == "settle" {
				cancel()
			}
		}

		rep := runner(b, newSink()).Run(ctx, "d", steps(t, "expect-no-card", "Hola", "send"))

		require.False(t, rep.Passed())
		assert.Equal(t, 2, rep.Failure().N)
		assert.ErrorIs(t, rep.Failure().Err, context.Canceled)
	})
}

func TestRun_ADeadlineThatPassesDuringARealWaitEndsItAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	r := &explore.Runner{Browser: &stubBrowser{}, Sink: newSink()} // Sleep is the real one

	start := time.Now()
	rep := r.Run(ctx, "d", steps(t, "wait:5m"))

	require.False(t, rep.Passed())
	assert.ErrorIs(t, rep.Failure().Err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 10*time.Second, "a wait of five minutes does not outlive its context")
}

func TestRun_TheRealSleepWaitsForReal(t *testing.T) {
	r := &explore.Runner{Browser: &stubBrowser{}, Sink: newSink()}

	start := time.Now()
	rep := r.Run(t.Context(), "d", steps(t, "wait:20ms"))

	require.True(t, rep.Passed())
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
}

func TestRun_AWaitThatCannotSleepFailsTheStepAndIsRecorded(t *testing.T) {
	sink := newSink()
	r := &explore.Runner{Browser: &stubBrowser{}, Sink: sink,
		Sleep: func(context.Context, time.Duration) error { return errors.New("the clock broke") }}

	rep := r.Run(t.Context(), "d", steps(t, "wait:2s", "Hola"))

	require.False(t, rep.Passed())
	assert.Len(t, rep.Steps, 1)
	assert.ErrorContains(t, rep.Failure().Err, "the clock broke")
	assert.Contains(t, sink.files, "01-wait-2s.snapshot.txt")
}

func TestRun_ASinkThatCannotWriteDoesNotFailTheSteps(t *testing.T) {
	b := &stubBrowser{text: "uid=1_1 heading \"x\"", cards: []explore.Card{{Kind: "approval", State: "pending"}}}

	rep := runner(b, &failingSink{*newSink()}).Run(t.Context(), "d", steps(t, "Hola", "expect:heading", "expect-card:approval=pending"))

	assert.True(t, rep.Passed(), rep.Markdown())
	assert.Len(t, rep.Steps, 3)
}

func TestRun_APageThatCannotBeReadAfterAStepDoesNotFailTheStep(t *testing.T) {
	b := &stubBrowser{textErr: errors.New("the page is gone")}
	sink := newSink()

	rep := runner(b, sink).Run(t.Context(), "d", steps(t, "Hola", "send", "expect-no-card"))

	assert.True(t, rep.Passed(), rep.Markdown())
	for name := range sink.files {
		assert.NotContains(t, name, "snapshot", "there is no snapshot to write")
	}
	assert.Contains(t, sink.files, "01-hola.network.txt", "what could be read is still recorded")
}

func TestRun_ExpectingTextOfAPageThatCannotBeReadSaysTheReadFailed(t *testing.T) {
	b := &stubBrowser{textErr: errors.New("the page is gone")}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect:Hola"))

	require.False(t, rep.Passed())
	assert.ErrorContains(t, rep.Failure().Err, "reading the page")
	assert.ErrorContains(t, rep.Failure().Err, "the page is gone", "it says why, not that the text is missing")
	assert.NotContains(t, rep.Failure().Err.Error(), "does not show")
}

func TestRun_AReadThatFailsInTheMiddleDoesNotMakeTheWholePageNewAgain(t *testing.T) {
	b := &stubBrowser{
		texts:     []string{"line a\nline b", "line a\nline b\nline c", "(read fails)", "line a\nline b\nline c"},
		textFails: map[int]bool{3: true},
	}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "one", "two", "three"))

	require.True(t, rep.Passed(), rep.Markdown())
	assert.Equal(t, []string{"line c"}, rep.Steps[0].NewLines)
	assert.Empty(t, rep.Steps[1].NewLines, "no text, so nothing can be said to be new")
	assert.Empty(t, rep.Steps[2].NewLines, "the page is as it was before the read that failed: nothing is new")
}

func TestRun_CardsThatCannotBeReadAreAFailureOfTheExpectationThatNeedsThem(t *testing.T) {
	for _, raw := range []string{"expect-card:approval=paid", "expect-no-card"} {
		t.Run(raw, func(t *testing.T) {
			b := &stubBrowser{cardsErr: errors.New("the DOM is not there")}

			rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, raw, "Hola"))

			require.False(t, rep.Passed())
			assert.ErrorContains(t, rep.Failure().Err, "reading the cards")
			assert.ErrorContains(t, rep.Failure().Err, "the DOM is not there")
			assert.Len(t, rep.Steps, 1)
		})
	}
	t.Run("a step that does not look at the cards never reads them", func(t *testing.T) {
		b := &stubBrowser{cardsErr: errors.New("the DOM is not there")}

		rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "Hola", "expect:x", "click?:Pay", "wait:1s"))

		assert.Zero(t, b.cardsCall)
		assert.Equal(t, 2, rep.Failure().N, "only the text expectation fails")
	})
}

func TestRun_ExpectingACardOnAPageWithNoCardsSaysSo(t *testing.T) {
	rep := runner(&stubBrowser{}, newSink()).Run(t.Context(), "d", steps(t, "expect-card:approval=paid"))

	require.False(t, rep.Passed())
	assert.ErrorContains(t, rep.Failure().Err, "no card")
}

func TestRun_OnlyTheNewestCardOfTheKindCountsEvenIfAnOlderOneMatches(t *testing.T) {
	b := &stubBrowser{cards: []explore.Card{{Kind: "approval", State: "paid"}, {Kind: "approval", State: "pending"}}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect-card:approval=paid"))

	require.False(t, rep.Passed())
	assert.ErrorContains(t, rep.Failure().Err, "approval=paid, approval=pending")
}

func TestRun_AScreenshotThatFailsDoesNotFailTheStep(t *testing.T) {
	b := &stubBrowser{screenshotErr: errors.New("no screen")}
	sink := newSink()

	rep := runner(b, sink).Run(t.Context(), "d", steps(t, "Hola", "send"))

	assert.True(t, rep.Passed(), rep.Markdown())
	assert.Contains(t, sink.files, "01-hola.snapshot.txt")
	assert.Contains(t, sink.files, "02-send.snapshot.txt")
}

func TestRun_RequestsThatCannotBeReadAreNotWritten(t *testing.T) {
	b := &stubBrowser{requestsErr: errors.New("no log")}
	sink := newSink()

	rep := runner(b, sink).Run(t.Context(), "d", steps(t, "Hola"))

	assert.True(t, rep.Passed())
	assert.Contains(t, sink.files, "01-hola.snapshot.txt")
	assert.NotContains(t, sink.files, "01-hola.network.txt")
}

func TestRun_AClickThatFindsNothingEndsTheRunBeforeAnOptionalOne(t *testing.T) {
	b := &stubBrowser{}
	b.missing = map[string]bool{"Pay": true}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "click:Pay", "click?:Pay"))

	require.False(t, rep.Passed())
	assert.Len(t, rep.Steps, 1)
	assert.Equal(t, 1, rep.Failure().N)
	assert.ErrorContains(t, rep.Failure().Err, "no button, option or link")
}

func TestRun_AnOptionalClickThatFindsNothingStillLetsThePageSettleAndRecords(t *testing.T) {
	b := &stubBrowser{}
	b.missing = map[string]bool{"Pay": true}
	sink := newSink()

	rep := runner(b, sink).Run(t.Context(), "d", steps(t, "click?:Pay", "click?:Pay"))

	require.True(t, rep.Passed())
	assert.Len(t, rep.Steps, 2)
	assert.Contains(t, b.calls, "settle")
	assert.Contains(t, sink.files, "01-click-pay.snapshot.txt")
	assert.Contains(t, sink.files, "02-click-pay.snapshot.txt")
}

func TestRun_AClickThatErrorsIsAFailureEvenWhenOptional(t *testing.T) {
	b := &stubBrowser{}
	b.failOn = "click Pay"

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "click?:Pay"))

	require.False(t, rep.Passed())
	assert.ErrorContains(t, rep.Failure().Err, "the page broke")
}

func TestRun_ExpectingTextOnAnEmptyPage(t *testing.T) {
	t.Run("the text is not there", func(t *testing.T) {
		rep := runner(&stubBrowser{}, newSink()).Run(t.Context(), "d", steps(t, "expect:anything"))

		require.False(t, rep.Passed())
		assert.ErrorContains(t, rep.Failure().Err, `"anything"`)
	})
	t.Run("the uids of the page are not part of its text", func(t *testing.T) {
		b := &stubBrowser{text: "uid=1_0 button \"Pay\""}

		rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect:uid=1_0"))

		require.False(t, rep.Passed(), "the uids are taken out of the text, so they cannot be expected")
	})
	t.Run("expect is case sensitive", func(t *testing.T) {
		b := &stubBrowser{text: `StaticText "Hola"`}

		assert.False(t, runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect:hola")).Passed())
		assert.True(t, runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect:Hola")).Passed())
	})
}

func TestRun_TwoIdenticalStepsDoNotShareFiles(t *testing.T) {
	sink := newSink()

	rep := runner(&stubBrowser{}, sink).Run(t.Context(), "d", steps(t, "Hola", "Hola", "Hola"))

	require.True(t, rep.Passed())
	for _, name := range []string{"01-hola.snapshot.txt", "02-hola.snapshot.txt", "03-hola.snapshot.txt"} {
		assert.Contains(t, sink.files, name)
	}
	assert.Len(t, sink.files, 6, "three steps, a snapshot and a network file each")
}

func TestRun_TheFilesOfAStepAreNamedAfterTheStepOrItsKind(t *testing.T) {
	tests := map[string]string{
		"😀🎉":                         "01-message",
		"日本語":                        "01-message",
		"مرحبا":                      "01-message",
		"@":                          "01-type",
		"@pick":                      "01-pick",
		"send":                       "01-send",
		"expect:😀":                   "01-expect",
		"click:Pay Now!":             "01-click-pay-now",
		"../../etc/passwd":           "01-etc-passwd",
		"a/b\\c:d":                   "01-a-b-c-d",
		"MAYÚSCULAS y Ñandú":         "01-may-sculas-y-and",
		"expect-no-card":             "01-expect-no-card",
		"wait:1.5s":                  "01-wait-1-5s",
		strings.Repeat("a", 100_000): "01-" + strings.Repeat("a", 32),
		"quiero yapear 20 soles a mi tía María": "01-quiero-yapear-20-soles-a-mi-t-a",
	}
	for raw, prefix := range tests {
		t.Run(raw[:min(len(raw), 40)], func(t *testing.T) {
			sink := newSink()

			rep := runner(&stubBrowser{text: raw}, sink).Run(t.Context(), "d", steps(t, raw))

			require.True(t, rep.Passed(), rep.Markdown())
			assert.Contains(t, sink.files, prefix+".snapshot.txt")
			for name := range sink.files {
				assert.NotContains(t, name, "/")
				assert.NotContains(t, name, "..")
				assert.LessOrEqual(t, len(name), 80)
			}
		})
	}
}

func TestRun_WhatIsNewIsMeasuredFromThePageBeforeTheFirstStep(t *testing.T) {
	b := &stubBrowser{texts: []string{"uid=1_0 heading \"Chat\"\nold line", "uid=9_0 heading \"Chat\"\nold line\nnew line"}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "Hola"))

	assert.Equal(t, []string{"new line"}, rep.Steps[0].NewLines, "the uids change and the heading is the same line")
}

func TestRun_ALineThatAppearsTwiceIsNewTwiceAndThenNotAtAll(t *testing.T) {
	b := &stubBrowser{texts: []string{"a", "a\nb\nb\n  b  ", "a\nb\nb\nb", "b\nb\nb\na", "b\nb\nb\nb\na"}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "one", "two", "three", "four"))

	require.True(t, rep.Passed())
	assert.Equal(t, []string{"b", "b", "b"}, rep.Steps[0].NewLines, "a line that is shown three times is new three times")
	assert.Empty(t, rep.Steps[1].NewLines, "the same page again")
	assert.Empty(t, rep.Steps[2].NewLines, "the same lines in another order are not new")
	assert.Equal(t, []string{"b"}, rep.Steps[3].NewLines, "one more of a line is one new line")
}

func TestRun_ALineThatCameBackAfterGoingIsNewAgain(t *testing.T) {
	b := &stubBrowser{texts: []string{"a\nb", "a", "a\nb"}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "one", "two"))

	assert.Empty(t, rep.Steps[0].NewLines)
	assert.Equal(t, []string{"b"}, rep.Steps[1].NewLines)
}

func TestRun_BlankLinesAndIndentationAreNotNews(t *testing.T) {
	b := &stubBrowser{texts: []string{"a", "\n\n   \n  a  \n\t\n"}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "one"))

	assert.Empty(t, rep.Steps[0].NewLines)
}

func TestReport_AHugeNumberOfNewLinesIsSummarizedInTheTable(t *testing.T) {
	var lines []string
	for i := range 10_000 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	b := &stubBrowser{texts: []string{"", strings.Join(lines, "\n")}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "Hola"))
	md := rep.Markdown()

	require.Len(t, rep.Steps[0].NewLines, 10_000)
	assert.Contains(t, md, "line 0; line 1; line 2; … (9997 more)")
	assert.NotContains(t, md, "line 3;")
	assert.Less(t, len(md), 1_000, "the table holds a summary, not the page")
}

func TestReport_TheSummaryOfNewLines(t *testing.T) {
	tests := map[string]struct {
		lines []string
		want  string
	}{
		"none":      {nil, "|  |"},
		"one":       {[]string{"a"}, "| a |"},
		"three":     {[]string{"a", "b", "c"}, "| a; b; c |"},
		"four":      {[]string{"a", "b", "c", "d"}, "| a; b; c; … (1 more) |"},
		"with pipe": {[]string{"a|b"}, `| a\|b |`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rep := explore.Report{Path: "p", Steps: []explore.StepResult{{N: 1, Raw: "x", NewLines: tc.lines}}}

			assert.Contains(t, rep.Markdown(), tc.want)
		})
	}
}

// countCells counts the cells of a row of the table of a report: the pipes that are not escaped split it.
func countCells(row string) int {
	n := 0
	for i := 0; i < len(row); i++ {
		switch row[i] {
		case '\\':
			i++
		case '|':
			n++
		}
	}
	return n - 1
}

func TestReport_MarkdownKeepsEveryRowOfTheTableInFiveCells(t *testing.T) {
	hostile := []string{
		"a | b", "a || b", "|", "a\nb", "a\r\nb", `a\|b`, `a\\|b`, `trailing backslash\`, "a\n| 9 | injected | row |", "tab\there",
		"`code` | **bold**", "<b>html</b>", "😀 | 日本語",
	}
	var steps []explore.StepResult
	for i, h := range hostile {
		steps = append(steps, explore.StepResult{N: i + 1, Raw: h, NewLines: []string{h, h}})
	}
	steps = append(steps, explore.StepResult{N: len(hostile) + 1, Raw: "x", Err: errors.New("it said: a | b\nand more\r\n|")})
	rep := explore.Report{Path: "p", Steps: steps}

	md := rep.Markdown()

	rows := 0
	for line := range strings.SplitSeq(md, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| # ") {
			continue
		}
		rows++
		assert.Equal(t, 5, countCells(line), "a row of the table has five cells: %q", line)
	}
	assert.Equal(t, len(steps), rows, "each step is one row: a newline cannot start another")
}

func TestReport_MarkdownNamesTheFailureAndTheInterruption(t *testing.T) {
	t.Run("a failure", func(t *testing.T) {
		rep := explore.Report{Path: "p", Steps: []explore.StepResult{{N: 1, Raw: "a"}, {N: 2, Raw: "b", Err: errors.New("boom")}}}

		md := rep.Markdown()

		assert.Contains(t, md, "# p: FAILED")
		assert.Contains(t, md, "Step 2 failed: boom")
		assert.NotContains(t, md, "interrupted")
	})
	t.Run("an interruption, even when wrapped", func(t *testing.T) {
		rep := explore.Report{Path: "p", Steps: []explore.StepResult{{N: 1, Raw: "a", Err: fmt.Errorf("waiting: %w", context.Canceled)}}}

		assert.Contains(t, rep.Markdown(), "The run was interrupted.")
	})
	t.Run("a step that took no time and one that took a long one", func(t *testing.T) {
		rep := explore.Report{Path: "p", Steps: []explore.StepResult{{N: 1, Raw: "a"}, {N: 2, Raw: "b", Took: 1234567 * time.Microsecond}}}

		md := rep.Markdown()

		assert.Contains(t, md, "| 0s |")
		assert.Contains(t, md, "| 1.235s |")
	})
}

func TestReport_FailureIsTheFirstStepThatFailedNotALaterOne(t *testing.T) {
	rep := explore.Report{Steps: []explore.StepResult{{N: 1}, {N: 2, Err: errors.New("first")}, {N: 3, Err: errors.New("second")}}}

	require.NotNil(t, rep.Failure())
	assert.Equal(t, 2, rep.Failure().N)
	assert.False(t, rep.Passed())
}

func TestRun_TookUsesTheClockOfTheRunner(t *testing.T) {
	var tick time.Time
	r := &explore.Runner{Browser: &stubBrowser{}, Sink: newSink(),
		Now: func() time.Time { tick = tick.Add(250 * time.Millisecond); return tick }}

	rep := r.Run(t.Context(), "d", steps(t, "expect-no-card", "expect-no-card"))

	require.Len(t, rep.Steps, 2)
	for _, s := range rep.Steps {
		assert.Equal(t, 250*time.Millisecond, s.Took, "the clock is read at the start of the step and at its end, and the time between them is the step's")
	}
}

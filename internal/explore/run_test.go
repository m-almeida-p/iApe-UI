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

// fakeBrowser is a page that does what the steps ask and says so: it keeps the calls, a text the page shows and its cards.
type fakeBrowser struct {
	calls   []string
	text    string
	cards   []explore.Card
	failOn  string // a call that fails
	missing map[string]bool
	onCall  func(call string)
}

func (f *fakeBrowser) note(call string) error {
	f.calls = append(f.calls, call)
	if f.onCall != nil {
		f.onCall(call)
	}
	if f.failOn == call {
		return errors.New("the page broke")
	}
	return nil
}

func (f *fakeBrowser) Type(_ context.Context, text string) error { return f.note("type " + text) }
func (f *fakeBrowser) Send(context.Context) error                { return f.note("send") }
func (f *fakeBrowser) Pick(_ context.Context, text string) error { return f.note("pick " + text) }
func (f *fakeBrowser) FillOTP(_ context.Context, code string) error {
	return f.note("otp " + code)
}
func (f *fakeBrowser) Settle(context.Context) error { return f.note("settle") }
func (f *fakeBrowser) Click(_ context.Context, text string) (bool, error) {
	if err := f.note("click " + text); err != nil {
		return false, err
	}
	return !f.missing[text], nil
}
func (f *fakeBrowser) Text(context.Context) (string, error)          { return f.text, nil }
func (f *fakeBrowser) Cards(context.Context) ([]explore.Card, error) { return f.cards, nil }
func (f *fakeBrowser) Requests(context.Context) (string, error)      { return "GET /x [200]", nil }
func (f *fakeBrowser) Screenshot(_ context.Context, path string) error {
	return f.note("screenshot " + path)
}

// memorySink keeps what the run records, by file name.
type memorySink struct{ files map[string]string }

func newSink() *memorySink { return &memorySink{files: map[string]string{}} }

func (m *memorySink) Path(n int, name, ext string) string {
	return fmt.Sprintf("%02d-%s.%s", n, name, ext)
}
func (m *memorySink) Write(n int, name, ext string, data []byte) error {
	m.files[m.Path(n, name, ext)] = string(data)
	return nil
}

func runner(b explore.Browser, sink explore.Sink) *explore.Runner {
	return &explore.Runner{Browser: b, Sink: sink, Sleep: func(context.Context, time.Duration) error { return nil }}
}

func steps(t *testing.T, raws ...string) []explore.Step {
	t.Helper()
	s, err := explore.ParseAll(raws)
	require.NoError(t, err)
	return s
}

func TestRun_EachStepDoesWhatItSaysAndTheRunSettlesAfterIt(t *testing.T) {
	b := &fakeBrowser{}

	rep := runner(b, newSink()).Run(t.Context(), "demo", steps(t,
		"quiero yapear", "@", "@pick:Juan", "type:20 soles", "send", "click:Pay", "otp:123456"))

	require.True(t, rep.Passed(), rep.Markdown())
	calls := strings.Join(b.calls, " | ")
	for _, want := range []string{
		"type quiero yapear | send | settle",
		"type @", "pick Juan", "type 20 soles", "click Pay | settle", "otp 123456 | settle",
	} {
		assert.Contains(t, calls, want)
	}
}

func TestRun_AfterEachStepItRecordsWhatThePageShows(t *testing.T) {
	b := &fakeBrowser{text: "uid=1_0 heading \"How can I help?\"\nuid=1_1 button \"Send\""}
	sink := newSink()

	rep := runner(b, sink).Run(t.Context(), "demo", steps(t, "Hola", "send"))

	require.True(t, rep.Passed())
	for _, name := range []string{"01-hola.snapshot.txt", "01-hola.network.txt", "01-hola.png", "02-send.snapshot.txt"} {
		if strings.HasSuffix(name, ".png") {
			assert.Contains(t, strings.Join(b.calls, " "), "screenshot "+name, "the screenshot is saved where the sink says")
			continue
		}
		assert.Contains(t, sink.files, name)
	}
	assert.Equal(t, "GET /x [200]", sink.files["01-hola.network.txt"])
}

func TestRun_SnapshotsLeaveOutTheUidsThatChangeFromOneToTheNext(t *testing.T) {
	b := &fakeBrowser{text: "uid=12_3 button \"Pay\""}
	sink := newSink()

	runner(b, sink).Run(t.Context(), "demo", steps(t, "Hola"))

	assert.Equal(t, `button "Pay"`, strings.TrimSpace(sink.files["01-hola.snapshot.txt"]))
}

func TestRun_ReportsWhatAppearedSincethePreviousStep(t *testing.T) {
	b := &fakeBrowser{text: `heading "Chat"`}
	b.onCall = func(call string) {
		if call == "send" {
			b.text = "heading \"Chat\"\nStaticText \"¿A quién necesitas yapearle?\""
		}
	}

	rep := runner(b, newSink()).Run(t.Context(), "demo", steps(t, "quiero yapear", "send"))

	require.Len(t, rep.Steps, 2)
	assert.Equal(t, []string{`StaticText "¿A quién necesitas yapearle?"`}, rep.Steps[0].NewLines)
	assert.Empty(t, rep.Steps[1].NewLines)
}

func TestRun_AnExpectationThatFailsStopsTheRunAndSaysWhatTheCardWas(t *testing.T) {
	b := &fakeBrowser{cards: []explore.Card{{Kind: "approval", State: "pending"}}}

	rep := runner(b, newSink()).Run(t.Context(), "demo", steps(t, "Hola", "expect-card:approval=paid", "click:Pay"))

	require.False(t, rep.Passed())
	failure := rep.Failure()
	require.NotNil(t, failure)
	assert.Equal(t, 2, failure.N)
	assert.Contains(t, failure.Err.Error(), "approval=paid")
	assert.Contains(t, failure.Err.Error(), "approval=pending", "it says what the page had")
	assert.Len(t, rep.Steps, 2, "the steps after the failure are not run")
	assert.NotContains(t, strings.Join(b.calls, " "), "click Pay")
}

func TestRun_TheNewestCardOfTheKindIsTheOneThatCounts(t *testing.T) {
	b := &fakeBrowser{cards: []explore.Card{
		{Kind: "contact", State: "confirmed"}, {Kind: "approval", State: "pending"}, {Kind: "approval", State: "paid"},
	}}

	rep := runner(b, newSink()).Run(t.Context(), "demo", steps(t, "expect-card:approval=paid", "expect-card:contact=confirmed"))

	assert.True(t, rep.Passed(), rep.Markdown())
}

func TestRun_ExpectingNoCard(t *testing.T) {
	t.Run("passes on a page without cards", func(t *testing.T) {
		assert.True(t, runner(&fakeBrowser{}, newSink()).Run(t.Context(), "d", steps(t, "expect-no-card")).Passed())
	})
	t.Run("fails on a page with one", func(t *testing.T) {
		b := &fakeBrowser{cards: []explore.Card{{Kind: "approval", State: "pending"}}}

		rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect-no-card"))

		require.False(t, rep.Passed())
		assert.Contains(t, rep.Failure().Err.Error(), "approval=pending")
	})
}

func TestRun_ExpectText(t *testing.T) {
	b := &fakeBrowser{text: `StaticText "El máximo para cualquier usuario es S/500"`}

	assert.True(t, runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect:S/500")).Passed())
	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "expect:S/900"))
	require.False(t, rep.Passed())
	assert.Contains(t, rep.Failure().Err.Error(), "S/900")
}

func TestRun_AClickOnNothingIsAnErrorUnlessItIsOptional(t *testing.T) {
	b := &fakeBrowser{missing: map[string]bool{"Pay": true}}

	rep := runner(b, newSink()).Run(t.Context(), "d", steps(t, "click:Pay"))
	require.False(t, rep.Passed())
	assert.Contains(t, rep.Failure().Err.Error(), `"Pay"`)

	assert.True(t, runner(b, newSink()).Run(t.Context(), "d", steps(t, "click?:Pay", "expect-no-card")).Passed())
}

func TestRun_AFailureOfThePageIsTheStepsAndItIsStillRecorded(t *testing.T) {
	b := &fakeBrowser{failOn: "send"}
	sink := newSink()

	rep := runner(b, sink).Run(t.Context(), "d", steps(t, "Hola"))

	require.False(t, rep.Passed())
	assert.Contains(t, rep.Failure().Err.Error(), "the page broke")
	assert.Contains(t, sink.files, "01-hola.snapshot.txt", "what the page showed when it broke is the evidence")
}

func TestRun_WaitPausesWithoutTouchingThePage(t *testing.T) {
	var slept []time.Duration
	b := &fakeBrowser{}
	r := &explore.Runner{Browser: b, Sink: newSink(), Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}

	rep := r.Run(t.Context(), "d", steps(t, "wait:8s"))

	require.True(t, rep.Passed())
	assert.Equal(t, []time.Duration{8 * time.Second}, slept)
	for _, call := range b.calls {
		assert.False(t, strings.HasPrefix(call, "type") || strings.HasPrefix(call, "click") || call == "send", "a wait touches nothing: %s", call)
	}
}

func TestRun_ACanceledRunStopsAndSaysSo(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	b := &fakeBrowser{}
	b.onCall = func(call string) {
		if call == "send" {
			cancel()
		}
	}

	rep := runner(b, newSink()).Run(ctx, "d", steps(t, "Hola", "Adiós"))

	require.False(t, rep.Passed())
	assert.ErrorIs(t, rep.Failure().Err, context.Canceled)
}

func TestReport_Markdown(t *testing.T) {
	b := &fakeBrowser{cards: []explore.Card{{Kind: "approval", State: "pending"}}}

	rep := runner(b, newSink()).Run(t.Context(), "pay", steps(t, "Hola", "expect-card:approval=paid"))
	md := rep.Markdown()

	assert.Contains(t, md, "# pay: FAILED")
	assert.Contains(t, md, "| 1 | Hola |")
	assert.Contains(t, md, "| 2 | expect-card:approval=paid |")
	assert.Contains(t, md, "approval=pending")
}

func TestReport_AllPassedSaysSo(t *testing.T) {
	rep := runner(&fakeBrowser{}, newSink()).Run(t.Context(), "guard", steps(t, "expect-no-card"))

	assert.Contains(t, rep.Markdown(), "# guard: PASSED")
}

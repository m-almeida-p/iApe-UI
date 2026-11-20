package chrome_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
	"iape-ui/internal/explore"
)

var _ explore.Browser = (*chrome.Browser)(nil)

// fakeDriver is the page: it answers the scripts and the clicks the browser asks of it.
type fakeDriver struct {
	calls   []string
	value   string              // what the composer holds
	buttons map[string][]string // role -> names the page has
	evals   map[string]any      // a script's marker -> what it returns
	busy    []bool              // what the page says about being busy, one for each look
	tools   map[string]string   // tool -> its text
	err     map[string]error    // call -> error
	filled  map[string]string
}

func (f *fakeDriver) fail(call string) error { return f.err[call] }

func (f *fakeDriver) Tool(_ context.Context, name string, args map[string]any, _ ...chrome.CallOption) (chrome.Result, error) {
	call := "tool " + name
	if k, ok := args["key"]; ok {
		call += " " + fmt.Sprint(k)
	}
	f.calls = append(f.calls, call)
	return chrome.Result{Text: f.tools[name]}, f.fail(call)
}

func (f *fakeDriver) Click(_ context.Context, q chrome.Query) error {
	f.calls = append(f.calls, fmt.Sprintf("click %s %q", q.Role, cmpOr(q.Name, q.NameContains)))
	for _, name := range f.buttons[q.Role] {
		if (q.Name != "" && name == q.Name) || (q.Name == "" && strings.Contains(name, q.NameContains)) {
			return f.fail("click")
		}
	}
	return &chrome.NotFoundError{Query: q}
}

func (f *fakeDriver) Fill(_ context.Context, q chrome.Query, value string) error {
	f.calls = append(f.calls, fmt.Sprintf("fill %s %q = %q", q.Role, cmpOr(q.Name, q.NameContains), value))
	if f.filled == nil {
		f.filled = map[string]string{}
	}
	f.filled[cmpOr(q.Name, q.NameContains)] = value
	return f.fail("fill")
}

func (f *fakeDriver) Eval(_ context.Context, function string, out any) error {
	f.calls = append(f.calls, "eval")
	var result any
	switch {
	case strings.Contains(function, "composer-input"):
		result = f.value
	case strings.Contains(function, `data-testid="card"`):
		result = f.evals["cards"]
	case strings.Contains(function, "pending"):
		busy := false
		if len(f.busy) > 0 {
			busy, f.busy = f.busy[0], f.busy[1:]
		}
		result = busy
	}
	raw, _ := json.Marshal(result)
	return json.Unmarshal(raw, out)
}

func (f *fakeDriver) Screenshot(_ context.Context, path string) error {
	f.calls = append(f.calls, "screenshot "+path)
	return f.fail("screenshot")
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func newBrowser(d *fakeDriver) *chrome.Browser {
	return &chrome.Browser{Driver: d, Poll: time.Millisecond, SettleWithin: 50 * time.Millisecond, PickWithin: 20 * time.Millisecond}
}

func TestBrowser_TypeAddsToWhatTheComposerHolds(t *testing.T) {
	d := &fakeDriver{value: "@(Juan Perez) "}

	require.NoError(t, newBrowser(d).Type(t.Context(), "20 soles"))

	assert.Equal(t, "@(Juan Perez) 20 soles", d.filled["Message"])
}

func TestBrowser_SendPressesSend(t *testing.T) {
	d := &fakeDriver{buttons: map[string][]string{"button": {"Send message"}}}

	require.NoError(t, newBrowser(d).Send(t.Context()))

	assert.Contains(t, d.calls, `click button "Send message"`)
}

func TestBrowser_ClickFindsButtonsThenOptionsThenLinks(t *testing.T) {
	d := &fakeDriver{buttons: map[string][]string{"option": {"Juan Perez · 910968515"}, "link": {"Docs"}}}
	b := newBrowser(d)

	found, err := b.Click(t.Context(), "Juan Perez")
	require.NoError(t, err)
	assert.True(t, found)

	found, err = b.Click(t.Context(), "Docs")
	require.NoError(t, err)
	assert.True(t, found)

	found, err = b.Click(t.Context(), "Pay")
	require.NoError(t, err, "no such element is not an error: the step decides")
	assert.False(t, found)
}

func TestBrowser_ClickPassesOnAFailureThatIsNotMissingAnElement(t *testing.T) {
	d := &fakeDriver{buttons: map[string][]string{"button": {"Pay"}}, err: map[string]error{"click": errors.New("the browser is gone")}}

	_, err := newBrowser(d).Click(t.Context(), "Pay")

	require.ErrorContains(t, err, "the browser is gone")
}

func TestBrowser_PickChoosesAnOptionOfThePicker(t *testing.T) {
	d := &fakeDriver{buttons: map[string][]string{"option": {"Juan Perez · 910968515", "Maria Torres · 942236054"}}}

	require.NoError(t, newBrowser(d).Pick(t.Context(), "Maria"))
	assert.Contains(t, d.calls, `click option "Maria"`)

	d.calls = nil
	require.NoError(t, newBrowser(d).Pick(t.Context(), ""))
	assert.Contains(t, d.calls, `click option ""`, "no text is the first one")
}

func TestBrowser_PickSaysSoWhenThePickerNeverShowsTheContact(t *testing.T) {
	d := &fakeDriver{buttons: map[string][]string{"option": {"Juan Perez · 910968515"}}}

	err := newBrowser(d).Pick(t.Context(), "Zoe")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"Zoe"`)
}

func TestBrowser_FillOTPTypesTheCodeAndConfirmsIt(t *testing.T) {
	d := &fakeDriver{}
	d.buttons = map[string][]string{"textbox": {"Code from the bank"}}

	require.NoError(t, newBrowser(d).FillOTP(t.Context(), "123456"))

	assert.Equal(t, "123456", d.filled["Code from the bank"])
	assert.Contains(t, d.calls, "tool press_key Enter")
}

func TestBrowser_SettleWaitsForTheReplyToEnd(t *testing.T) {
	d := &fakeDriver{busy: []bool{true, true, false, false}}

	require.NoError(t, newBrowser(d).Settle(t.Context()))

	assert.Empty(t, d.busy, "it looked until the page was quiet twice")
}

func TestBrowser_SettleGivesUpOnAReplyThatNeverEnds(t *testing.T) {
	d := &fakeDriver{busy: make([]bool, 1000)}
	for i := range d.busy {
		d.busy[i] = true
	}

	err := newBrowser(d).Settle(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "still")
}

func TestBrowser_SettleStopsWhenTheRunIsCanceled(t *testing.T) {
	d := &fakeDriver{busy: make([]bool, 1000)}
	for i := range d.busy {
		d.busy[i] = true
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, newBrowser(d).Settle(ctx), context.Canceled)
}

func TestBrowser_CardsAreReadFromThePage(t *testing.T) {
	d := &fakeDriver{evals: map[string]any{"cards": []map[string]string{
		{"kind": "contact", "state": "confirmed"}, {"kind": "approval", "state": "pending"},
	}}}

	cards, err := newBrowser(d).Cards(t.Context())

	require.NoError(t, err)
	assert.Equal(t, []explore.Card{{Kind: "contact", State: "confirmed"}, {Kind: "approval", State: "pending"}}, cards)
}

func TestBrowser_TextAndRequestsAreWhatTheToolsSay(t *testing.T) {
	d := &fakeDriver{tools: map[string]string{"take_snapshot": "uid=1_0 heading", "list_network_requests": "GET / [200]"}}
	b := newBrowser(d)

	text, err := b.Text(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "uid=1_0 heading", text)

	reqs, err := b.Requests(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "GET / [200]", reqs)
}

func TestBrowser_ScreenshotSavesWhereItIsTold(t *testing.T) {
	d := &fakeDriver{}

	require.NoError(t, newBrowser(d).Screenshot(t.Context(), "/out/01.png"))

	assert.Contains(t, d.calls, "screenshot /out/01.png")
}

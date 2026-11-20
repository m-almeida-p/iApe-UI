package chrome_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
	"iape-ui/internal/explore"
)

// hookDriver is a page whose every answer the test says.
type hookDriver struct {
	click      func(n int, q chrome.Query) error
	fill       func(q chrome.Query, value string) error
	eval       func(function string, out any) error
	tool       func(name string, args map[string]any) (chrome.Result, error)
	screenshot func(path string) error

	clicks, evals atomic.Int32
	calls         []string
}

func (h *hookDriver) Tool(_ context.Context, name string, args map[string]any, _ ...chrome.CallOption) (chrome.Result, error) {
	h.calls = append(h.calls, "tool "+name)
	if h.tool == nil {
		return chrome.Result{}, nil
	}
	return h.tool(name, args)
}

func (h *hookDriver) Click(_ context.Context, q chrome.Query) error {
	n := int(h.clicks.Add(1))
	h.calls = append(h.calls, fmt.Sprintf("click %s %q", q.Role, q.NameContains))
	if h.click == nil {
		return nil
	}
	return h.click(n, q)
}

func (h *hookDriver) Fill(_ context.Context, q chrome.Query, value string) error {
	h.calls = append(h.calls, fmt.Sprintf("fill %s %q = %q", q.Role, q.Name, value))
	if h.fill == nil {
		return nil
	}
	return h.fill(q, value)
}

func (h *hookDriver) Eval(_ context.Context, function string, out any) error {
	h.evals.Add(1)
	h.calls = append(h.calls, "eval")
	if h.eval == nil {
		return nil
	}
	return h.eval(function, out)
}

func (h *hookDriver) Screenshot(_ context.Context, path string) error {
	h.calls = append(h.calls, "screenshot "+path)
	if h.screenshot == nil {
		return nil
	}
	return h.screenshot(path)
}

// returning makes an Eval that answers every script with value.
func returning(value any) func(string, any) error {
	return func(_ string, out any) error {
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	}
}

// idleThenBusy answers the busy script from a list, and the others as the composer is empty.
func busyAnswers(answers ...bool) func(string, any) error {
	var i int
	return func(_ string, out any) error {
		busy := false
		if i < len(answers) {
			busy = answers[i]
		}
		i++
		raw, _ := json.Marshal(busy)
		return json.Unmarshal(raw, out)
	}
}

func notFound(q chrome.Query) error { return &chrome.NotFoundError{Query: q} }

func TestBrowser_TypeEdgeCases(t *testing.T) {
	t.Run("the composer cannot be read", func(t *testing.T) {
		d := &hookDriver{eval: func(string, any) error { return errors.New("the page is gone") }}

		err := newBrowserOn(d).Type(t.Context(), "hola")

		require.ErrorContains(t, err, "reading the composer: the page is gone")
		assert.NotContains(t, d.calls, `fill textbox "Message" = "hola"`, "nothing is typed over what is unknown")
		assert.Len(t, d.calls, 1)
	})
	t.Run("the composer cannot be filled", func(t *testing.T) {
		d := &hookDriver{eval: returning(""), fill: func(chrome.Query, string) error { return notFound(chrome.Query{Role: "textbox", Name: "Message"}) }}

		err := newBrowserOn(d).Type(t.Context(), "hola")

		var nf *chrome.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.ErrorContains(t, err, "typing in the composer")
	})
	t.Run("what is in the composer is kept whatever it is", func(t *testing.T) {
		for _, current := range []string{"", "@(Juan Perez) ", "línea 1\nlínea 2\n", "😀 \"quoted\" \\ back", "\ttab", strings.Repeat("x", 100_000)} {
			var filled string
			d := &hookDriver{eval: returning(current), fill: func(_ chrome.Query, value string) error { filled = value; return nil }}

			require.NoError(t, newBrowserOn(d).Type(t.Context(), "20 soles 💸\nya"))

			assert.Equal(t, current+"20 soles 💸\nya", filled)
		}
	})
	t.Run("no text types what is already there, and nothing else", func(t *testing.T) {
		var filled string
		d := &hookDriver{eval: returning("hola"), fill: func(_ chrome.Query, value string) error { filled = value; return nil }}

		require.NoError(t, newBrowserOn(d).Type(t.Context(), ""))

		assert.Equal(t, "hola", filled)
	})
}

func newBrowserOn(d chrome.Driver) *chrome.Browser {
	return &chrome.Browser{Driver: d, Poll: time.Millisecond, SettleWithin: 50 * time.Millisecond, PickWithin: 20 * time.Millisecond}
}

func TestBrowser_SendWhenThereIsNoButton(t *testing.T) {
	d := &hookDriver{click: func(_ int, q chrome.Query) error { return notFound(q) }}

	err := newBrowserOn(d).Send(t.Context())

	var nf *chrome.NotFoundError
	require.ErrorAs(t, err, &nf)
	assert.EqualError(t, err, `pressing Send: the page has no button named "Send message"`)
}

func TestBrowser_SendFailsForAnotherReason(t *testing.T) {
	d := &hookDriver{click: func(int, chrome.Query) error { return errors.New("the browser is gone") }}

	err := newBrowserOn(d).Send(t.Context())

	require.ErrorContains(t, err, "pressing Send: the browser is gone")
}

func TestBrowser_ClickEdgeCases(t *testing.T) {
	t.Run("the first role that has the text wins: a button before an option before a link", func(t *testing.T) {
		tests := []struct {
			present []string
			want    string
		}{
			{[]string{"button", "option", "link"}, "button"},
			{[]string{"option", "link"}, "option"},
			{[]string{"link"}, "link"},
		}
		for _, tc := range tests {
			d := &hookDriver{click: func(_ int, q chrome.Query) error {
				if slices.Contains(tc.present, q.Role) {
					return nil
				}
				return notFound(q)
			}}

			found, err := newBrowserOn(d).Click(t.Context(), "Pay")

			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, fmt.Sprintf("click %s %q", tc.want, "Pay"), d.calls[len(d.calls)-1])
		}
	})
	t.Run("a button and a link with the same text: the button", func(t *testing.T) {
		d := &hookDriver{}

		found, err := newBrowserOn(d).Click(t.Context(), "Docs")

		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, []string{`click button "Docs"`}, d.calls, "the link was not looked for")
	})
	t.Run("nothing to press is not the first button of the page", func(t *testing.T) {
		for _, text := range []string{"", " ", "\t\n "} {
			d := &hookDriver{}

			found, err := newBrowserOn(d).Click(t.Context(), text)

			require.Error(t, err, "%q", text)
			assert.False(t, found)
			assert.Empty(t, d.calls, "the page was not touched")
		}
	})
	t.Run("text of every kind", func(t *testing.T) {
		for _, text := range []string{`Say "hi"`, `back\slash`, "emoji 😀", "日本語", "שלום", strings.Repeat("x", 10_000)} {
			d := &hookDriver{}

			found, err := newBrowserOn(d).Click(t.Context(), text)

			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, fmt.Sprintf("click button %q", text), d.calls[0])
		}
	})
	t.Run("a failure in the middle of the roles stops there", func(t *testing.T) {
		d := &hookDriver{click: func(_ int, q chrome.Query) error {
			if q.Role == "button" {
				return notFound(q)
			}
			return errors.New("the browser is gone")
		}}

		found, err := newBrowserOn(d).Click(t.Context(), "Pay")

		require.ErrorContains(t, err, `pressing "Pay": the browser is gone`)
		assert.False(t, found)
		assert.Equal(t, []string{`click button "Pay"`, `click option "Pay"`}, d.calls, "no link was tried")
	})
	t.Run("a stale element is the driver's to try again, and its refusal is passed on", func(t *testing.T) {
		d := &hookDriver{click: func(int, chrome.Query) error {
			return &chrome.ToolError{Tool: "click", Message: `Element uid "1_3" not found on page 1.`}
		}}

		_, err := newBrowserOn(d).Click(t.Context(), "Pay")

		var toolErr *chrome.ToolError
		require.ErrorAs(t, err, &toolErr)
	})
}

func TestBrowser_PickEdgeCases(t *testing.T) {
	t.Run("the first option that has the text", func(t *testing.T) {
		d := &hookDriver{eval: busyAnswers()}

		require.NoError(t, newBrowserOn(d).Pick(t.Context(), "Maria"))

		assert.Equal(t, []string{`click option "Maria"`, "eval", "eval"}, d.calls, "one click, then it settles")
	})
	t.Run("an option that shows up on the fourth look", func(t *testing.T) {
		d := &hookDriver{eval: busyAnswers(), click: func(n int, q chrome.Query) error {
			if n < 4 {
				return notFound(q)
			}
			return nil
		}}
		b := newBrowserOn(d)
		b.PickWithin = time.Second

		require.NoError(t, b.Pick(t.Context(), "Maria"))

		assert.EqualValues(t, 4, d.clicks.Load())
	})
	t.Run("an option that never shows up is looked for for as long as it is given and no longer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &hookDriver{click: func(_ int, q chrome.Query) error { return notFound(q) }}
			b := &chrome.Browser{Driver: d, Poll: 250 * time.Millisecond, PickWithin: 2 * time.Second}
			start := time.Now()

			err := b.Pick(t.Context(), "Zoe")

			require.ErrorContains(t, err, `the contact picker did not show "Zoe" within 2s`)
			assert.Equal(t, 2*time.Second, time.Since(start))
			assert.EqualValues(t, 9, d.clicks.Load(), "every 250 ms, and a look at the end")
		})
	})
	t.Run("the default is five seconds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &hookDriver{click: func(_ int, q chrome.Query) error { return notFound(q) }}
			start := time.Now()

			err := (&chrome.Browser{Driver: d}).Pick(t.Context(), "Zoe")

			require.ErrorContains(t, err, "within 5s")
			assert.Equal(t, 5*time.Second, time.Since(start))
		})
	})
	t.Run("a poll longer than the time given does not make the wait longer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &hookDriver{click: func(_ int, q chrome.Query) error { return notFound(q) }}
			b := &chrome.Browser{Driver: d, Poll: time.Hour, PickWithin: time.Second}
			start := time.Now()

			err := b.Pick(t.Context(), "Zoe")

			require.Error(t, err)
			assert.Equal(t, time.Second, time.Since(start))
		})
	})
	t.Run("no text and no option says the picker showed no contact", func(t *testing.T) {
		d := &hookDriver{click: func(_ int, q chrome.Query) error { return notFound(q) }}

		err := newBrowserOn(d).Pick(t.Context(), "")

		require.ErrorContains(t, err, "the contact picker showed no contact within 20ms")
	})
	t.Run("a run that is canceled while the picker is looked at", func(t *testing.T) {
		d := &hookDriver{click: func(_ int, q chrome.Query) error { return notFound(q) }}
		b := newBrowserOn(d)
		b.PickWithin = time.Hour
		b.Poll = time.Hour
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()

		err := b.Pick(ctx, "Zoe")

		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("a failure that is not a missing option is not looked at again", func(t *testing.T) {
		d := &hookDriver{click: func(int, chrome.Query) error { return errors.New("the browser is gone") }}
		b := newBrowserOn(d)
		b.PickWithin = time.Second

		err := b.Pick(t.Context(), "Maria")

		require.ErrorContains(t, err, "choosing a contact in the picker: the browser is gone")
		assert.EqualValues(t, 1, d.clicks.Load())
	})
	t.Run("the choice is clicked and the page then does not settle", func(t *testing.T) {
		d := &hookDriver{eval: func(string, any) error { return errors.New("the page is gone") }}

		err := newBrowserOn(d).Pick(t.Context(), "Maria")

		require.ErrorContains(t, err, "looking at the page: the page is gone")
	})
	t.Run("the choice is clicked and the page keeps being busy", func(t *testing.T) {
		d := &hookDriver{eval: returning(true)}

		err := newBrowserOn(d).Pick(t.Context(), "Maria")

		require.ErrorContains(t, err, "still shows a reply being written")
		assert.EqualValues(t, 1, d.clicks.Load(), "and the choice was not clicked again")
	})
}

func TestBrowser_FillOTPEdgeCases(t *testing.T) {
	t.Run("the field is not there", func(t *testing.T) {
		d := &hookDriver{fill: func(q chrome.Query, _ string) error { return notFound(q) }}

		err := newBrowserOn(d).FillOTP(t.Context(), "123456")

		var nf *chrome.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.ErrorContains(t, err, "typing the code")
		assert.NotContains(t, d.calls, "tool press_key", "Enter is not pressed on a field that was not filled")
	})
	t.Run("Enter cannot be pressed", func(t *testing.T) {
		d := &hookDriver{tool: func(string, map[string]any) (chrome.Result, error) { return chrome.Result{}, errors.New("no focus") }}

		err := newBrowserOn(d).FillOTP(t.Context(), "123456")

		require.ErrorContains(t, err, "confirming the code: no focus")
	})
	t.Run("a code that is not a number", func(t *testing.T) {
		for _, code := range []string{`12"34`, `a'b`, `\n`, "12\n34", "😀", "", strings.Repeat("9", 10_000)} {
			var filled string
			var key any
			d := &hookDriver{
				fill: func(_ chrome.Query, value string) error { filled = value; return nil },
				tool: func(_ string, args map[string]any) (chrome.Result, error) {
					key = args["key"]
					return chrome.Result{}, nil
				},
			}

			require.NoError(t, newBrowserOn(d).FillOTP(t.Context(), code))

			assert.Equal(t, code, filled, "the code goes as it is")
			assert.Equal(t, "Enter", key)
		}
	})
}

func TestBrowser_SettleEdgeCases(t *testing.T) {
	t.Run("a page that is busy, quiet, busy again and then quiet needs two quiet looks in a row", func(t *testing.T) {
		looks := 0
		d := &hookDriver{eval: func(_ string, out any) error {
			looks++
			return json.Unmarshal([]byte([]string{"true", "false", "true", "false", "false", "false"}[looks-1]), out)
		}}

		require.NoError(t, newBrowserOn(d).Settle(t.Context()))

		assert.Equal(t, 5, looks, "the quiet look between two busy ones did not count")
	})
	t.Run("a page that is quiet at once is looked at twice", func(t *testing.T) {
		d := &hookDriver{eval: busyAnswers()}

		require.NoError(t, newBrowserOn(d).Settle(t.Context()))

		assert.EqualValues(t, 2, d.evals.Load())
	})
	t.Run("the page cannot be read in the middle", func(t *testing.T) {
		looks := 0
		d := &hookDriver{eval: func(_ string, out any) error {
			if looks++; looks == 3 {
				return errors.New("the page navigated")
			}
			return json.Unmarshal([]byte("true"), out)
		}}

		err := newBrowserOn(d).Settle(t.Context())

		require.ErrorContains(t, err, "looking at the page: the page navigated")
	})
	t.Run("the script says something that is not a yes or a no", func(t *testing.T) {
		d := &hookDriver{eval: returning("busy")}

		err := newBrowserOn(d).Settle(t.Context())

		require.ErrorContains(t, err, "looking at the page")
	})
	t.Run("a run canceled while the page is busy", func(t *testing.T) {
		d := &hookDriver{eval: returning(true)}
		b := newBrowserOn(d)
		b.SettleWithin = time.Hour
		b.Poll = time.Hour
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)

		require.ErrorIs(t, b.Settle(ctx), context.Canceled)
	})
	t.Run("no time given means ninety seconds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &hookDriver{eval: returning(true)}
			start := time.Now()

			err := (&chrome.Browser{Driver: d}).Settle(t.Context())

			require.ErrorContains(t, err, "after 1m30s")
			assert.Equal(t, 90*time.Second, time.Since(start))
		})
	})
	t.Run("a poll longer than the time given does not make the wait longer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &hookDriver{eval: returning(true)}
			start := time.Now()

			err := (&chrome.Browser{Driver: d, Poll: time.Hour, SettleWithin: time.Second}).Settle(t.Context())

			require.ErrorContains(t, err, "after 1s")
			assert.Equal(t, time.Second, time.Since(start))
		})
	})
	t.Run("the page that goes quiet just as the time ends is settled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var looks int
			d := &hookDriver{eval: func(_ string, out any) error {
				looks++
				return json.Unmarshal([]byte(fmt.Sprint(looks <= 4)), out) // busy for the first four looks, a second apart
			}}

			err := (&chrome.Browser{Driver: d, Poll: time.Second, SettleWithin: 5 * time.Second}).Settle(t.Context())

			require.NoError(t, err)
			assert.Equal(t, 6, looks)
		})
	})
}

func TestBrowser_CardsEdgeCases(t *testing.T) {
	cardsOf := func(t *testing.T, page string) ([]explore.Card, error) {
		t.Helper()
		d := &hookDriver{eval: func(_ string, out any) error { return json.Unmarshal([]byte(page), out) }}
		return newBrowserOn(d).Cards(t.Context())
	}
	tests := []struct {
		name string
		page string
		want []explore.Card
	}{
		{"null", `null`, []explore.Card{}},
		{"no cards", `[]`, []explore.Card{}},
		{"a card with nothing in it", `[{}]`, []explore.Card{{}}},
		{"a card with no state", `[{"kind":"approval"}]`, []explore.Card{{Kind: "approval"}}},
		{"a card with no kind", `[{"state":"pending"}]`, []explore.Card{{State: "pending"}}},
		{"empty kind and state", `[{"kind":"","state":""}]`, []explore.Card{{}}},
		{"members that are not asked for", `[{"kind":"approval","state":"paid","extra":{"a":[1,2]},"n":1}]`, []explore.Card{{Kind: "approval", State: "paid"}}},
		{"unicode", `[{"kind":"contacto 😀","state":"confirmado ñ"}]`, []explore.Card{{Kind: "contacto 😀", State: "confirmado ñ"}}},
		{"order is the page's", `[{"kind":"a","state":"1"},{"kind":"b","state":"2"},{"kind":"a","state":"1"}]`, []explore.Card{{Kind: "a", State: "1"}, {Kind: "b", State: "2"}, {Kind: "a", State: "1"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cardsOf(t, tc.page)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("a thousand cards", func(t *testing.T) {
		page := make([]map[string]string, 1000)
		for i := range page {
			page[i] = map[string]string{"kind": "approval", "state": fmt.Sprint("s", i)}
		}
		raw, err := json.Marshal(page)
		require.NoError(t, err)

		got, err := cardsOf(t, string(raw))

		require.NoError(t, err)
		require.Len(t, got, 1000)
		assert.Equal(t, explore.Card{Kind: "approval", State: "s999"}, got[999])
	})
	t.Run("what is not a list of cards", func(t *testing.T) {
		for _, page := range []string{`{}`, `"cards"`, `[1]`, `[{"kind":1}]`, `[null]`} {
			got, err := cardsOf(t, page)

			if page == `[null]` {
				require.NoError(t, err, "a card that is null is an empty one")
				assert.Equal(t, []explore.Card{{}}, got)
				continue
			}
			require.Error(t, err, page)
			assert.Nil(t, got)
		}
	})
	t.Run("the page cannot be read", func(t *testing.T) {
		d := &hookDriver{eval: func(string, any) error { return errors.New("the page is gone") }}

		got, err := newBrowserOn(d).Cards(t.Context())

		require.EqualError(t, err, "the page is gone")
		assert.Nil(t, got)
	})
}

func TestBrowser_TextRequestsAndScreenshotPassOnWhatTheDriverSays(t *testing.T) {
	gone := errors.New("the browser is gone")
	d := &hookDriver{
		tool:       func(string, map[string]any) (chrome.Result, error) { return chrome.Result{Text: "partial"}, gone },
		screenshot: func(string) error { return gone },
	}
	b := newBrowserOn(d)

	_, err := b.Text(t.Context())
	require.ErrorIs(t, err, gone)
	_, err = b.Requests(t.Context())
	require.ErrorIs(t, err, gone)
	require.ErrorIs(t, b.Screenshot(t.Context(), "/out/1.png"), gone)
	assert.Equal(t, []string{"tool take_snapshot", "tool list_network_requests", "screenshot /out/1.png"}, d.calls)
}

func TestBrowser_TextAndRequestsAreWhatTheToolsSayWhateverTheyAre(t *testing.T) {
	d := &hookDriver{tool: func(name string, _ map[string]any) (chrome.Result, error) {
		return chrome.Result{Text: strings.Repeat("😀 ", 100_000) + name}, nil
	}}
	b := newBrowserOn(d)

	text, err := b.Text(t.Context())
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(text, "take_snapshot"))
	reqs, err := b.Requests(t.Context())
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(reqs, "list_network_requests"))
}

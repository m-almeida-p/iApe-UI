package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/service/mocks"
)

func TestContactPicker(t *testing.T) {
	t.Run("offers the candidates for what was typed, each with its index", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodGet, "/contacts?q=mar%C3%ADa", nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
		root := parseHTML(t, rec.Body.String())
		options := byTestID(root, "contact-option")
		want := domain.Candidates("maría")
		require.Len(t, options, len(want))
		for i, c := range want {
			assert.Contains(t, textContent(options[i]), c.Name)
			assert.Equal(t, string(rune('0'+i)), attrOf(options[i], "data-choice"))
			assert.Equal(t, "option", attrOf(options[i], "role"))
		}
		assert.Equal(t, "maría", attrOf(byTestID(root, "contact-picker")[0], "data-query"))
	})

	t.Run("without a query it offers the default contacts", func(t *testing.T) {
		h := newChatHarness(t)

		root := parseHTML(t, h.do(http.MethodGet, "/contacts", nil, htmx).Body.String())

		assert.Len(t, byTestID(root, "contact-option"), len(domain.Candidates("")))
	})

	t.Run("what was typed is text, never markup", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodGet, "/contacts?q="+url.QueryEscape(`<script>alert(1)</script>`), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), "<script>")
	})

	t.Run("it needs a signed-in user", func(t *testing.T) {
		rec := serve(t, mocks.NewAuth(t), Options{}, httptest.NewRequest(http.MethodGet, "/contacts", nil))

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Contains(t, rec.Header().Get("Location"), "/login")
	})
}

func TestConfirmCandidate(t *testing.T) {
	form := url.Values{"q": {"maria"}, "choice": {"0"}}

	t.Run("confirms the choice and answers the mention for the message", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "maria", 0).Return(domain.Contact{Name: "Maria Torres", Phone: "912345678"}, nil)

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
		assert.Equal(t, "@(Maria Torres)", rec.Body.String())
	})

	t.Run("a refusal is a 422 with the service's words", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "maria", 0).Return(domain.Contact{}, &domain.ActionError{Message: "no such contact"})

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assert.Equal(t, "no such contact", strings.TrimSpace(rec.Body.String()))
	})

	t.Run("the agent being down is a 502", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "maria", 0).Return(domain.Contact{}, domain.ErrAgentUnavailable)

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		assert.Equal(t, http.StatusBadGateway, rec.Code)
	})

	t.Run("a choice that is not a number is not asked", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {"maria"}, "choice": {"x"}}, nil)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("any other failure is a 500", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "maria", 0).Return(domain.Contact{}, errors.New("boom"))

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "boom")
	})
}

func pickerFor(t *testing.T, query string) (*httptest.ResponseRecorder, *html.Node) {
	t.Helper()
	h := newChatHarness(t)
	rec := h.do(http.MethodGet, "/contacts?q="+url.QueryEscape(query), nil, htmx)
	return rec, parseHTML(t, rec.Body.String())
}

func TestContactPicker_WhateverIsTyped(t *testing.T) {
	queries := map[string]string{
		"a newline":                  "ana\nrojas",
		"a NUL":                      "ana\x00rojas",
		"an emoji":                   "🙂 ana",
		"only emoji":                 "🙂🙂🙂",
		"a right-to-left override":   "\u202eana",
		"quotes":                     `ana" onmouseover="alert(1)`,
		"a closed attribute":         `"><img src=x onerror=alert(1)>`,
		"single quotes":              `ana' onfocus='alert(1)`,
		"markup":                     `<script>alert(1)</script>`,
		"an entity":                  `&quot;&lt;b&gt;`,
		"a javascript URL":           "javascript:alert(1)",
		"parentheses and at signs":   "@(ana)",
		"only an at sign":            "@",
		"spaces":                     "   ",
		"a hundred kilobytes":        strings.Repeat("ñandú ", 100<<10/12),
		"a hundred kilobytes of one": strings.Repeat("ñ", 100<<10),
		"bytes that are not UTF-8":   "ana\xff\xfe",
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			rec, root := pickerFor(t, query)

			require.Equal(t, http.StatusOK, rec.Code)
			options := byTestID(root, "contact-option")
			assert.NotEmpty(t, options, "there is somebody to choose")
			assert.LessOrEqual(t, len(options), 5)
			noMarkupFrom(t, root, rec.Body.String())
			assert.Len(t, byTestID(root, "contact-picker"), 1)
			assert.NotContains(t, rec.Body.String(), "\x00")
			for i, o := range options {
				assert.Equal(t, strconv.Itoa(i), attrOf(o, "data-choice"))
				assert.NotEmpty(t, attrOf(o, "data-name"))
			}
		})
	}

	t.Run("the query comes back in its attribute as it was typed, escaped, so a quote cannot end it", func(t *testing.T) {
		const query = `ana" data-x="1`
		rec, root := pickerFor(t, query)

		picker := byTestID(root, "contact-picker")[0]

		assert.Equal(t, query, attrOf(picker, "data-query"))
		assert.False(t, hasAttr(picker, "data-x"))
		assert.NotContains(t, rec.Body.String(), `data-query="ana" `)
	})

	t.Run("the options are the candidates of the query, so what the picker shows is what confirm will pick", func(t *testing.T) {
		for _, query := range []string{"ana", "  ANA ", "@ana", "(ana)", "ana\x00", "María"} {
			_, root := pickerFor(t, query)
			want := domain.Candidates(query)

			options := byTestID(root, "contact-option")

			require.Len(t, options, len(want), query)
			for i, c := range want {
				assert.Equal(t, c.Name, attrOf(options[i], "data-name"), query)
				assert.Contains(t, textContent(options[i]), c.Phone)
			}
		}
	})

	t.Run("a query given twice uses the first", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodGet, "/contacts?q=ana&q=luis", nil, htmx)

		assert.Equal(t, domain.Candidates("ana")[0].Name, attrOf(byTestID(parseHTML(t, rec.Body.String()), "contact-option")[0], "data-name"))
	})

	t.Run("it is an answer for a list that is drawn, not a page: no layout", func(t *testing.T) {
		rec, _ := pickerFor(t, "ana")

		assert.NotContains(t, rec.Body.String(), "<html")
		assert.NotContains(t, rec.Body.String(), "<body")
	})

	t.Run("only GET asks for the list", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			h := newChatHarness(t)

			rec := h.do(method, "/contacts?q=ana", nil, htmx)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		}
	})
}

func TestConfirmCandidate_WhatTheFormHolds(t *testing.T) {
	t.Run("no q is an empty query: the default contacts", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "", 0).Return(domain.Contact{Name: "Juan Perez", Phone: "911111111"}, nil).Once()

		rec := h.do(http.MethodPost, "/contacts/confirm", url.Values{"choice": {"0"}}, nil)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "@(Juan Perez)", rec.Body.String())
	})

	t.Run("a choice the use case refuses is a 422 with its words, even when it is huge", func(t *testing.T) {
		for _, choice := range []string{"5", "99", "999"} {
			h := newChatHarness(t)
			n, _ := strconv.Atoi(choice)
			h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "maria", n).Return(domain.Contact{}, &domain.ActionError{Message: "Choose one of the contacts."}).Once()

			rec := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {"maria"}, "choice": {choice}}, nil)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, choice)
		}
	})

	t.Run("the mention is the name as text, whatever the name holds", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "x", 0).Return(domain.Contact{Name: `<b>"Ana"</b>`, Phone: "9"}, nil).Once()

		rec := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {"x"}, "choice": {"0"}}, nil)

		assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
		assert.Equal(t, `@(<b>"Ana"</b>)`, rec.Body.String(), "plain text, which the composer puts in its box as text")
	})

	t.Run("a hundred kilobytes of query reach the use case as they are, which cuts them", func(t *testing.T) {
		h := newChatHarness(t)
		long := strings.Repeat("ñ", 100<<10/2)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, long, 0).Return(domain.Contact{Name: "N", Phone: "9"}, nil).Once()

		rec := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {long}, "choice": {"0"}}, nil)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("only POST confirms", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			h := newChatHarness(t)

			rec := h.do(method, "/contacts/confirm", nil, nil)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		}
	})

	t.Run("it needs a signed-in user", func(t *testing.T) {
		rec := serve(t, mocks.NewAuth(t), Options{}, postForm("/contacts/confirm", url.Values{"choice": {"0"}}, nil))

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Contains(t, rec.Header().Get("Location"), "/login")
	})
}

// The picker says whom the user saw: if the list for the query is no longer what they saw (they typed on, or the list changed between
// the drawing and the click), a different person must not be confirmed with the service.
func TestConfirmCandidate_ThePersonThatWasShownIsThePersonThatIsConfirmed(t *testing.T) {
	maria := domain.Candidates("maria")[0]

	t.Run("the name that was shown, at that place of the list, is confirmed", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, alice.ID, "maria", 0).Return(maria, nil).Once()

		rec := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {"maria"}, "choice": {"0"}, "name": {maria.Name}}, nil)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "@("+maria.Name+")", rec.Body.String())
	})

	for name, form := range map[string]url.Values{
		"another person is at that place":                      {"q": {"maria"}, "choice": {"0"}, "name": {"Someone Else"}},
		"the place is not in the list":                         {"q": {"maria"}, "choice": {"7"}, "name": {maria.Name}},
		"the name is of the same person with its case changed": {"q": {"maria"}, "choice": {"0"}, "name": {strings.ToUpper(maria.Name)}},
	} {
		t.Run("nobody is confirmed when "+name, func(t *testing.T) {
			h := newChatHarness(t)

			rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Contains(t, rec.Body.String(), "list changed")
			h.cards.AssertNotCalled(t, "ConfirmCandidate", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

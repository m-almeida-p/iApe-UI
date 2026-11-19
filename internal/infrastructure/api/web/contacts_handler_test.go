package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

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
		h.cards.On("ConfirmCandidate", mock.Anything, "maria", 0).Return(domain.Contact{Name: "Maria Torres", Phone: "912345678"}, nil)

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
		assert.Equal(t, "@(Maria Torres)", rec.Body.String())
	})

	t.Run("a refusal is a 422 with the service's words", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, "maria", 0).Return(domain.Contact{}, &domain.ActionError{Message: "no such contact"})

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assert.Equal(t, "no such contact", strings.TrimSpace(rec.Body.String()))
	})

	t.Run("the agent being down is a 502", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmCandidate", mock.Anything, "maria", 0).Return(domain.Contact{}, domain.ErrAgentUnavailable)

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
		h.cards.On("ConfirmCandidate", mock.Anything, "maria", 0).Return(domain.Contact{}, errors.New("boom"))

		rec := h.do(http.MethodPost, "/contacts/confirm", form, nil)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "boom")
	})
}

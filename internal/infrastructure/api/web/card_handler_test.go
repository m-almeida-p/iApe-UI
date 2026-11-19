package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

func cardPath(action string) string {
	return "/c/" + convID.String() + "/m/" + streamMsgID.String() + "/card/" + action
}

// cardReply is a finished reply with a card in the given state.
func cardReply(state domain.InteractionState) domain.Message {
	m := reply(domain.StatusComplete, "Review your transfer.")
	m.Meta.Interaction = &domain.Interaction{
		Kind: domain.InteractionApproval, State: state, ApprovalID: "appr_1", RunID: "run-1", ChallengeID: "trf_9",
		Transfer: domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa <b>Diaz</b>", DestinationPhone: "987654321", Message: "lunch"},
	}
	return m
}

func TestCardIsDrawnFromTheMessage(t *testing.T) {
	t.Run("a pending card shows the transfer and offers Pay and Reject", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(cardReply(domain.InteractionPending), nil)

		rec := h.do(http.MethodPost, stopPath(), nil, htmx) // any route that answers with a reply
		root := parseHTML(t, rec.Body.String())

		card := byTestID(root, "card")
		require.Len(t, card, 1)
		assert.Equal(t, "pending", attrOf(card[0], "data-state"))
		assert.Equal(t, "S/ 20.00", strings.TrimSpace(textContent(byTestID(root, "card-amount")[0])))
		dest := textContent(byTestID(root, "card-destination")[0])
		assert.Contains(t, dest, "Rosa <b>Diaz</b>", "the name is text, never markup")
		assert.Contains(t, dest, "987654321")
		pay, reject := byTestID(root, "card-pay"), byTestID(root, "card-reject")
		require.Len(t, pay, 1)
		require.Len(t, reject, 1)
		assert.Equal(t, cardPath("approve"), attrOf(pay[0], "hx-post"))
		assert.Equal(t, cardPath("reject"), attrOf(reject[0], "hx-post"))
		assert.Equal(t, "closest .reply", attrOf(pay[0], "hx-target"))
		assert.Equal(t, "outerHTML", attrOf(pay[0], "hx-swap"))
		assert.Empty(t, byTestID(root, "card-otp-input"))
		assert.Len(t, byTestID(root, "message-body"), 1, "the reply's own text is still there")
	})

	t.Run("a card that asks for a code shows the field, and can still be rejected", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(cardReply(domain.InteractionOTPRequired), nil)

		root := parseHTML(t, h.do(http.MethodPost, stopPath(), nil, htmx).Body.String())

		assert.Empty(t, byTestID(root, "card-pay"))
		input := byTestID(root, "card-otp-input")
		require.Len(t, input, 1)
		assert.Equal(t, "code", attrOf(input[0], "name"))
		assert.Equal(t, "one-time-code", attrOf(input[0], "autocomplete"))
		form := elementsOf(root, "form")
		require.Len(t, form, 1)
		assert.Equal(t, cardPath("otp"), attrOf(form[0], "hx-post"))
		assert.Len(t, byTestID(root, "card-reject"), 1)
	})

	t.Run("a closed card shows how it ended and no way to act", func(t *testing.T) {
		for state, want := range map[domain.InteractionState]string{domain.InteractionPaid: "paid", domain.InteractionRejected: "rejected"} {
			h := newChatHarness(t)
			m := cardReply(state)
			m.Meta.Interaction.Result = "Done. TxnID: OP-1"
			h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(m, nil)

			root := parseHTML(t, h.do(http.MethodPost, stopPath(), nil, htmx).Body.String())

			assert.Equal(t, want, attrOf(byTestID(root, "card")[0], "data-state"))
			assert.Equal(t, "Done. TxnID: OP-1", strings.TrimSpace(textContent(byTestID(root, "card-result")[0])))
			assert.Empty(t, byTestID(root, "card-pay"), want)
			assert.Empty(t, byTestID(root, "card-reject"), want)
			assert.Empty(t, byTestID(root, "card-otp-input"), want)
		}
	})

	t.Run("a reply without a card draws none", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(reply(domain.StatusComplete, "hello"), nil)

		root := parseHTML(t, h.do(http.MethodPost, stopPath(), nil, htmx).Body.String())

		assert.Empty(t, byTestID(root, "card"))
	})
}

func TestApproveRejectAndConfirmCode(t *testing.T) {
	t.Run("Pay answers with the reply, its card now in the new state", func(t *testing.T) {
		h := newChatHarness(t)
		paid := cardReply(domain.InteractionPaid)
		paid.Meta.Interaction.Result = "Transfer paid successfully. TxnID: OP-1"
		h.cards.On("Approve", mock.Anything, alice.ID, convID, streamMsgID).Return(paid, nil)

		rec := h.do(http.MethodPost, cardPath("approve"), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "reply", attrOf(elementsOf(root, "div")[0], "class"))
		assert.Equal(t, "paid", attrOf(byTestID(root, "card")[0], "data-state"))
		assert.Contains(t, textContent(byTestID(root, "card-result")[0]), "OP-1")
	})

	t.Run("Reject answers the same way", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("Reject", mock.Anything, alice.ID, convID, streamMsgID).Return(cardReply(domain.InteractionRejected), nil)

		rec := h.do(http.MethodPost, cardPath("reject"), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "rejected", attrOf(byTestID(parseHTML(t, rec.Body.String()), "card")[0], "data-state"))
	})

	t.Run("the code is read from the form", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, "123456").Return(cardReply(domain.InteractionPaid), nil)

		rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"code": {"123456"}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "paid", attrOf(byTestID(parseHTML(t, rec.Body.String()), "card")[0], "data-state"))
	})

	t.Run("a refusal shows the card again with the service's reason, as text", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, "000000").
			Return(cardReply(domain.InteractionOTPRequired), &domain.ActionError{Message: "OTP <i>verification</i> failed"})

		rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"code": {"000000"}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code, "htmx swaps only a 2xx, and the card must show the reason")
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "otp_required", attrOf(byTestID(root, "card")[0], "data-state"), "the card stays as it was")
		assert.Equal(t, "OTP <i>verification</i> failed", strings.TrimSpace(textContent(byTestID(root, "card-error")[0])))
		assert.Len(t, byTestID(root, "card-otp-input"), 1, "and can be tried again")
	})

	t.Run("the agent being down shows the card again and says to try again", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("Approve", mock.Anything, alice.ID, convID, streamMsgID).
			Return(cardReply(domain.InteractionPending), domain.ErrAgentUnavailable)

		rec := h.do(http.MethodPost, cardPath("approve"), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Contains(t, textContent(byTestID(root, "card-error")[0]), "Try again")
		assert.Len(t, byTestID(root, "card-pay"), 1)
	})

	t.Run("without htmx it goes back to the conversation", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("Reject", mock.Anything, alice.ID, convID, streamMsgID).Return(cardReply(domain.InteractionRejected), nil)

		rec := h.do(http.MethodPost, cardPath("reject"), nil, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
	})

	failures := []struct {
		name string
		err  error
		want int
	}{
		{"a card that was answered already is a conflict", domain.ErrInvalidState, http.StatusConflict},
		{"a message that is not the user's is not found", domain.ErrNotFound, http.StatusNotFound},
		{"any other failure is a 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			h.cards.On("Approve", mock.Anything, alice.ID, convID, streamMsgID).Return(domain.Message{}, tc.err)

			rec := h.do(http.MethodPost, cardPath("approve"), nil, htmx)

			assert.Equal(t, tc.want, rec.Code)
			assert.NotContains(t, rec.Body.String(), "boom")
		})
	}

	t.Run("a path that holds no ids is not found, and nothing is asked", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodPost, "/c/x/m/y/card/approve", nil, htmx)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("only POST answers a card", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodGet, cardPath("approve"), nil, htmx)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}

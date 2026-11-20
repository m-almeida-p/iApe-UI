package web

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/service"
	"iape-ui/internal/core/port/service/mocks"
	"iape-ui/internal/infrastructure/config"
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

func contactReplyMsg(state domain.InteractionState) domain.Message {
	m := reply(domain.StatusComplete, "Confirma que sea la persona correcta antes de yapearle.")
	m.Meta.Interaction = &domain.Interaction{Kind: domain.InteractionContact, State: state, RequestID: "req-1", RunID: "run-1", Query: "Maria", Prompt: "Confirma que sea la persona correcta antes de yapearle."}
	if state == domain.InteractionConfirmed {
		m.Meta.Interaction.Contact = domain.Contact{Name: "Maria <b>Torres</b>", Phone: "912345678"}
	}
	return m
}

func TestContactCardIsDrawnFromTheMessage(t *testing.T) {
	t.Run("a pending card offers each candidate as a button, in order", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(contactReplyMsg(domain.InteractionPending), nil)

		root := parseHTML(t, h.do(http.MethodPost, stopPath(), nil, htmx).Body.String())

		card := byTestID(root, "card")
		require.Len(t, card, 1)
		assert.Equal(t, "pending", attrOf(card[0], "data-state"))
		assert.Equal(t, "contact", attrOf(card[0], "data-kind"))
		options := byTestID(root, "card-contact")
		want := domain.Candidates("Maria")
		require.Len(t, options, len(want))
		for i, c := range want {
			assert.Contains(t, textContent(options[i]), c.Name)
			assert.Contains(t, textContent(options[i]), c.Phone)
			assert.Equal(t, cardPath("contact"), attrOf(options[i], "hx-post"))
			assert.Equal(t, `{"choice":"`+string(rune('0'+i))+`"}`, attrOf(options[i], "hx-vals"))
			assert.Equal(t, "closest .reply", attrOf(options[i], "hx-target"))
		}
		assert.Empty(t, byTestID(root, "card-pay"), "it is not a transfer")
		assert.Empty(t, byTestID(root, "card-amount"))
	})

	t.Run("a confirmed card says whom, as text, and offers nothing", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(contactReplyMsg(domain.InteractionConfirmed), nil)

		root := parseHTML(t, h.do(http.MethodPost, stopPath(), nil, htmx).Body.String())

		assert.Equal(t, "confirmed", attrOf(byTestID(root, "card")[0], "data-state"))
		assert.Contains(t, textContent(byTestID(root, "card-chosen")[0]), "Maria <b>Torres</b>")
		assert.Empty(t, byTestID(root, "card-contact"))
	})
}

func TestChooseContact(t *testing.T) {
	resumed := func() service.Turn {
		user := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: 3, Role: domain.RoleUser, Status: domain.StatusComplete, Content: "usuario confirmado"}
		next := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: 4, Role: domain.RoleAssistant, Status: domain.StatusPending}
		return service.Turn{User: user, Reply: next}
	}
	form := url.Values{"choice": {"1"}}

	t.Run("the choice closes the card and the agent is asked to go on, as 'usuario confirmado'", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(contactReplyMsg(domain.InteractionConfirmed), nil)
		turn := resumed()
		h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(turn, nil)

		rec := h.do(http.MethodPost, cardPath("contact"), form, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "confirmed", attrOf(byTestID(root, "card")[0], "data-state"))
		var oob *html.Node
		for _, d := range elementsOf(root, "div") {
			if hasAttr(d, "hx-swap-oob") {
				oob = d
			}
		}
		require.NotNil(t, oob, "the next turn is added to the conversation out of band")
		assert.Equal(t, "beforeend:#messages", attrOf(oob, "hx-swap-oob"))
		assert.Contains(t, textContent(oob), "usuario confirmado")
		assert.Contains(t, rec.Body.String(), "/c/"+convID.String()+"/m/"+turn.Reply.ID.String()+"/stream", "the new reply opens its stream")
	})

	t.Run("when the agent cannot go on yet the card is still answered", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(contactReplyMsg(domain.InteractionConfirmed), nil)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(service.Turn{}, domain.ErrInvalidState)

		rec := h.do(http.MethodPost, cardPath("contact"), form, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "confirmed", attrOf(byTestID(root, "card")[0], "data-state"))
		assert.NotContains(t, rec.Body.String(), "hx-swap-oob")
	})

	t.Run("a refusal shows the card again with the reason and does not go on", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).
			Return(contactReplyMsg(domain.InteractionPending), &domain.ActionError{Message: "no such contact"})

		rec := h.do(http.MethodPost, cardPath("contact"), form, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "pending", attrOf(byTestID(root, "card")[0], "data-state"))
		assert.Equal(t, "no such contact", strings.TrimSpace(textContent(byTestID(root, "card-error")[0])))
	})

	t.Run("a choice that is not a number is not asked", func(t *testing.T) {
		for _, bad := range []string{"", "x", "1.5", "-1x"} {
			h := newChatHarness(t)

			rec := h.do(http.MethodPost, cardPath("contact"), url.Values{"choice": {bad}}, htmx)

			assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
		}
	})

	t.Run("a card answered already is a conflict", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(domain.Message{}, domain.ErrInvalidState)

		rec := h.do(http.MethodPost, cardPath("contact"), form, htmx)

		assert.Equal(t, http.StatusConflict, rec.Code)
	})

	t.Run("without htmx it goes back to the conversation", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(contactReplyMsg(domain.InteractionConfirmed), nil)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(resumed(), nil)

		rec := h.do(http.MethodPost, cardPath("contact"), form, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
	})
}

// drawn renders a reply as the page does and parses it.
func drawn(t *testing.T, m domain.Message) (*html.Node, string) {
	t.Helper()
	app := testApp(t)
	out, err := app.renderer.Block("partial:reply", app.messageView(m))
	require.NoError(t, err)
	return parseHTML(t, out), out
}

// noMarkupFrom fails when the parsed page holds an element or an attribute that text typed into a card could have made.
func noMarkupFrom(t *testing.T, root *html.Node, raw string) {
	t.Helper()
	for _, tag := range []string{"script", "img", "svg", "iframe", "style", "a", "form"} {
		assert.Empty(t, elementsOf(root, tag), "a <%s> that no template wrote: %s", tag, raw)
	}
	walkHTML(root, func(n *html.Node) {
		for _, a := range n.Attr {
			assert.False(t, strings.HasPrefix(a.Key, "on"), "an event handler attribute %q", a.Key)
			switch a.Key {
			case "href", "src", "action", "formaction", "xlink:href", "srcdoc":
				assert.Fail(t, "an attribute that loads or runs something", "%s=%q", a.Key, a.Val)
			}
		}
	})
}

func TestCardsAreOnlyDrawnOnAReplyThatCompleted(t *testing.T) {
	for _, status := range []domain.Status{domain.StatusPending, domain.StatusStreaming, domain.StatusStopped, domain.StatusFailed} {
		t.Run("a "+string(status)+" reply draws no card, so there is nothing to pay on it", func(t *testing.T) {
			for name, card := range map[string]domain.Message{"approval": cardReply(domain.InteractionPending), "contact": contactReplyMsg(domain.InteractionPending)} {
				m := card
				m.Status = status

				root, raw := drawn(t, m)

				assert.Empty(t, byTestID(root, "card"), name)
				assert.Empty(t, byTestID(root, "card-pay"), name)
				assert.NotContains(t, raw, "/card/", name+": no route to answer it")
			}
		})
	}

	t.Run("a reply that completed draws it", func(t *testing.T) {
		root, _ := drawn(t, cardReply(domain.InteractionPending))

		assert.Len(t, byTestID(root, "card-pay"), 1)
	})
}

func TestNewCardView_WhatIsDrawn(t *testing.T) {
	t.Run("a kind this version does not know draws no card", func(t *testing.T) {
		m := cardReply(domain.InteractionPending)
		m.Meta.Interaction.Kind = "survey"

		assert.Nil(t, newCardView(m))
	})

	t.Run("a card with no kind draws no card", func(t *testing.T) {
		m := reply(domain.StatusComplete, "hi")
		m.Meta.Interaction = &domain.Interaction{State: domain.InteractionPending}

		assert.Nil(t, newCardView(m))
	})

	t.Run("a reply with no card draws none", func(t *testing.T) {
		assert.Nil(t, newCardView(reply(domain.StatusComplete, "hi")))
	})

	t.Run("the routes of a card are those of its own reply", func(t *testing.T) {
		v := newCardView(cardReply(domain.InteractionPending))

		assert.Equal(t, cardPath("approve"), v.ApproveURL)
		assert.Equal(t, cardPath("reject"), v.RejectURL)
		assert.Equal(t, cardPath("otp"), v.CodeURL)
	})

	t.Run("a contact card that is not pending offers nobody to choose", func(t *testing.T) {
		for _, state := range []domain.InteractionState{domain.InteractionConfirmed, domain.InteractionPaid, domain.InteractionRejected, domain.InteractionOTPRequired} {
			v := newCardView(contactReplyMsg(state))

			assert.Empty(t, v.Candidates, string(state))
		}
	})

	t.Run("a contact card with no query offers the default contacts", func(t *testing.T) {
		m := contactReplyMsg(domain.InteractionPending)
		m.Meta.Interaction.Query = ""

		assert.Len(t, newCardView(m).Candidates, len(domain.Candidates("")))
	})

	t.Run("a confirmed contact card with no contact says nobody", func(t *testing.T) {
		m := contactReplyMsg(domain.InteractionConfirmed)
		m.Meta.Interaction.Contact = domain.Contact{}

		root, _ := drawn(t, m)

		assert.Empty(t, byTestID(root, "card-chosen"), "a lone separator is not a person")
	})

	t.Run("a confirmed contact card with a name only shows the name alone", func(t *testing.T) {
		m := contactReplyMsg(domain.InteractionConfirmed)
		m.Meta.Interaction.Contact = domain.Contact{Name: "Maria"}

		assert.Equal(t, "Maria", newCardView(m).Chosen)
	})
}

func TestAmountText(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		amount   float64
		want     string
	}{
		{"soles are written with their sign", "PEN", 20, "S/ 20.00"},
		{"no currency is soles", "", 20, "S/ 20.00"},
		{"soles in lower case are soles", "pen", 20, "S/ 20.00"},
		{"soles with spaces around are soles", " PEN ", 20, "S/ 20.00"},
		{"another currency follows the number", "USD", 20, "20.00 USD"},
		{"a currency is shown as sent", "usd", 20, "20.00 usd"},
		{"the sign of soles as a currency is not soles", "S/", 20, "20.00 S/"},
		{"two decimals, rounded", "PEN", 20.126, "S/ 20.13"},
		{"half a cent is rounded as the binary number is, up for 0.005", "PEN", 0.005, "S/ 0.01"},
		{"and down for 0.015, which is a little under", "PEN", 0.015, "S/ 0.01"},
		{"no thousands separator", "PEN", 1234567.891, "S/ 1234567.89"},
		{"a very large amount", "PEN", 1e21, "S/ 1000000000000000000000.00"},
		{"zero", "PEN", 0, "S/ 0.00"},
		{"negative", "PEN", -5, "S/ -5.00"},
		{"not a number", "PEN", math.NaN(), "S/ NaN"},
		{"endless", "PEN", math.Inf(1), "S/ +Inf"},
		{"endless below", "USD", math.Inf(-1), "-Inf USD"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := amountText(domain.Transfer{Amount: tc.amount, Currency: tc.currency})

			assert.Equal(t, tc.want, got)
		})
	}
}

// A card that does not say how much, or to whom, is not one to pay: the user could not tell what they are paying.
func TestApprovalCard_ThatCannotBePaidFromHere(t *testing.T) {
	withTransfer := func(t domain.Transfer) domain.Message {
		m := cardReply(domain.InteractionPending)
		m.Meta.Interaction.Transfer = t
		return m
	}

	for name, transfer := range map[string]domain.Transfer{
		"no amount":                      {DestinationName: "Rosa", DestinationPhone: "987654321"},
		"a negative amount":              {Amount: -5, DestinationName: "Rosa"},
		"an amount that is not a number": {Amount: math.NaN(), DestinationName: "Rosa"},
		"an endless amount":              {Amount: math.Inf(1), DestinationName: "Rosa"},
		"no destination":                 {Amount: 20, Currency: "PEN"},
		"a destination of spaces":        {Amount: 20, DestinationName: "  ", DestinationPhone: "\t"},
	} {
		t.Run(name+" offers Reject and no Pay, and says why", func(t *testing.T) {
			root, _ := drawn(t, withTransfer(transfer))

			assert.Empty(t, byTestID(root, "card-pay"))
			assert.Len(t, byTestID(root, "card-reject"), 1, "it can still be turned down")
			assert.Len(t, byTestID(root, "card-unpayable"), 1)
		})
	}

	for name, transfer := range map[string]domain.Transfer{
		"a name only":       {Amount: 20, DestinationName: "Rosa"},
		"a phone only":      {Amount: 20, DestinationPhone: "987654321"},
		"a smallest amount": {Amount: 0.01, DestinationName: "Rosa"},
	} {
		t.Run(name+" is enough to pay", func(t *testing.T) {
			root, _ := drawn(t, withTransfer(transfer))

			assert.Len(t, byTestID(root, "card-pay"), 1)
			assert.Empty(t, byTestID(root, "card-unpayable"))
		})
	}

	t.Run("the destination reads well with a phone and no name", func(t *testing.T) {
		root, _ := drawn(t, withTransfer(domain.Transfer{Amount: 20, DestinationPhone: "987654321"}))

		assert.Equal(t, "987654321", strings.TrimSpace(textContent(byTestID(root, "card-destination")[0])))
	})

	t.Run("a name with no phone is the name alone", func(t *testing.T) {
		root, _ := drawn(t, withTransfer(domain.Transfer{Amount: 20, DestinationName: "Rosa"}))

		assert.Equal(t, "Rosa", strings.TrimSpace(textContent(byTestID(root, "card-destination")[0])))
	})

	t.Run("no message draws no line for it, and no result either", func(t *testing.T) {
		root, _ := drawn(t, withTransfer(domain.Transfer{Amount: 20, DestinationName: "Rosa"}))

		assert.Empty(t, byTestID(root, "card-message"))
		assert.Empty(t, byTestID(root, "card-result"))
		assert.Empty(t, byTestID(root, "card-error"))
	})

	t.Run("a card that asks for a code is not blocked by its amount: the transfer was already approved", func(t *testing.T) {
		m := cardReply(domain.InteractionOTPRequired)
		m.Meta.Interaction.Transfer = domain.Transfer{}

		root, _ := drawn(t, m)

		assert.Len(t, byTestID(root, "card-otp-input"), 1)
		assert.Empty(t, byTestID(root, "card-unpayable"), "there is nothing to pay: the code is what is asked")
	})

	t.Run("a closed card is not 'unpayable': it is over", func(t *testing.T) {
		m := cardReply(domain.InteractionPaid)
		m.Meta.Interaction.Transfer = domain.Transfer{}

		root, _ := drawn(t, m)

		assert.Empty(t, byTestID(root, "card-unpayable"))
	})
}

var hostileTexts = map[string]string{
	"a script":                  `<script>alert(1)</script>`,
	"a closed attribute":        `"><img src=x onerror=alert(1)>`,
	"a closed single quote":     `'><svg onload=alert(1)>`,
	"a javascript URL":          `javascript:alert(1)`,
	"a comment":                 `<!-- x`,
	"an entity":                 `&lt;b&gt;&amp;`,
	"a template action":         `{{.}} {{template "x"}}`,
	"a style":                   `<style>*{display:none}</style>`,
	"a link":                    `<a href="javascript:alert(1)">pay</a>`,
	"a form":                    `<form action="//evil.example"><input name=code></form>`,
	"a closing tag of the card": `</div></section></article><button>Pay</button>`,
}

func TestCards_EveryTextIsData(t *testing.T) {
	for name, hostile := range hostileTexts {
		t.Run("the approval card with "+name, func(t *testing.T) {
			m := cardReply(domain.InteractionPending)
			m.Meta.Interaction.Transfer = domain.Transfer{Amount: 20, Currency: hostile, DestinationName: hostile, DestinationPhone: hostile, Message: hostile}
			m.Meta.Interaction.Result = hostile

			root, raw := drawn(t, m)

			noMarkupFrom(t, root, raw)
			assert.Len(t, byTestID(root, "card-pay"), 1, "the card has its own Pay button only")
			assert.Len(t, elementsOf(byTestID(root, "card")[0], "button"), 2, "Pay and Reject: none that the text added")
			assert.Contains(t, textContent(byTestID(root, "card-destination")[0]), hostile)
			assert.Equal(t, hostile, strings.TrimSpace(textContent(byTestID(root, "card-message")[0])))
			assert.Equal(t, hostile, strings.TrimSpace(textContent(byTestID(root, "card-result")[0])))
		})

		t.Run("the contact card with "+name, func(t *testing.T) {
			m := contactReplyMsg(domain.InteractionPending)
			m.Meta.Interaction.Query = hostile
			m.Meta.Interaction.Prompt = hostile

			root, raw := drawn(t, m)

			noMarkupFrom(t, root, raw)
			for _, option := range byTestID(root, "card-contact") {
				assert.Equal(t, "/c/"+convID.String()+"/m/"+streamMsgID.String()+"/card/contact", attrOf(option, "hx-post"))
				assert.Regexp(t, `^\{"choice":"[0-4]"\}$`, attrOf(option, "hx-vals"))
			}
		})

		t.Run("the confirmed contact card with "+name, func(t *testing.T) {
			m := contactReplyMsg(domain.InteractionConfirmed)
			m.Meta.Interaction.Contact = domain.Contact{Name: hostile, Phone: hostile}

			root, raw := drawn(t, m)

			noMarkupFrom(t, root, raw)
			assert.Equal(t, hostile+" · "+hostile, strings.TrimSpace(textContent(byTestID(root, "card-chosen")[0])))
		})

		t.Run("the reason a card was refused with "+name, func(t *testing.T) {
			h := newChatHarness(t)
			h.cards.On("Approve", mock.Anything, alice.ID, convID, streamMsgID).
				Return(cardReply(domain.InteractionPending), &domain.ActionError{Message: hostile})

			rec := h.do(http.MethodPost, cardPath("approve"), nil, htmx)

			require.Equal(t, http.StatusOK, rec.Code)
			root := parseHTML(t, rec.Body.String())
			noMarkupFrom(t, root, rec.Body.String())
			assert.Equal(t, hostile, strings.TrimSpace(textContent(byTestID(root, "card-error")[0])))
		})
	}

	t.Run("a right-to-left override cannot turn the name around the phone next to it", func(t *testing.T) {
		m := cardReply(domain.InteractionPending)
		m.Meta.Interaction.Transfer.DestinationName = "Rosa \u202e987654321 · Diaz\u202c \u2066x\u2069\u200f\u200e\u061c"

		root, raw := drawn(t, m)

		for _, r := range "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c" {
			assert.NotContains(t, raw, string(r), "%U", r)
		}
		assert.Contains(t, textContent(byTestID(root, "card-destination")[0]), "Rosa 987654321 · Diaz x")
	})

	t.Run("a NUL is not written to the page", func(t *testing.T) {
		m := cardReply(domain.InteractionPending)
		m.Meta.Interaction.Transfer.DestinationName = "Ro\x00sa"
		m.Meta.Interaction.Transfer.Message = "a\x00b\x01c"

		root, raw := drawn(t, m)

		assert.NotContains(t, raw, "\x00")
		assert.NotContains(t, raw, "\x01")
		assert.Contains(t, textContent(byTestID(root, "card-destination")[0]), "Rosa")
		assert.Equal(t, "abc", strings.TrimSpace(textContent(byTestID(root, "card-message")[0])))
	})

	t.Run("new lines in a message are kept for the page to show", func(t *testing.T) {
		m := cardReply(domain.InteractionPending)
		m.Meta.Interaction.Transfer.Message = "line one\nline two\tindented"

		root, _ := drawn(t, m)

		assert.Equal(t, "line one\nline two\tindented", strings.TrimSpace(textContent(byTestID(root, "card-message")[0])))
	})
}

// cardRoutes are the four routes that answer a card, with a form that is good for each.
var cardRoutes = []struct {
	name, action string
	method       string
	form         url.Values
	call         string // the method of the use case it ends in
	args         []any  // what it is called with after the ids
}{
	{"approve", "approve", http.MethodPost, nil, "Approve", nil},
	{"reject", "reject", http.MethodPost, nil, "Reject", nil},
	{"code", "otp", http.MethodPost, url.Values{"code": {"123456"}}, "ConfirmOTP", []any{"123456"}},
	{"contact", "contact", http.MethodPost, url.Values{"choice": {"1"}}, "ChooseContact", []any{1}},
}

func (h *chatHarness) expectCard(call string, args []any, m domain.Message, err error) {
	all := append([]any{mock.Anything, alice.ID, convID, streamMsgID}, args...)
	h.cards.On(call, all...).Return(m, err).Once()
}

func TestCardRoutes_EachRouteHoldsTheSameLine(t *testing.T) {
	t.Run("only POST answers a card, on every route", func(t *testing.T) {
		for _, rt := range cardRoutes {
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
				h := newChatHarness(t)

				rec := h.do(method, cardPath(rt.action), nil, htmx)

				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rt.name+" "+method)
				assert.Contains(t, rec.Header().Get("Allow"), "POST")
			}
		}
	})

	t.Run("a message that is not the user's, or not there, is a 404 and nothing of it is said, on every route", func(t *testing.T) {
		for _, rt := range cardRoutes {
			h := newChatHarness(t)
			h.expectCard(rt.call, rt.args, domain.Message{}, domain.ErrNotFound)

			rec := h.do(rt.method, cardPath(rt.action), rt.form, htmx)

			assert.Equal(t, http.StatusNotFound, rec.Code, rt.name)
			assert.NotContains(t, rec.Body.String(), streamMsgID.String())
		}
	})

	t.Run("ids that are not UUIDs are a 404 and nothing is asked, on every route", func(t *testing.T) {
		for _, rt := range cardRoutes {
			for _, ids := range [][2]string{
				{"x", streamMsgID.String()}, {convID.String(), "y"}, {convID.String(), ""}, {convID.String(), "00000000-0000-0000-0000-00000000000"},
				{convID.String(), "' OR 1=1 --"}, {convID.String(), strings.Repeat("a", 1000)}, {convID.String(), streamMsgID.String() + "x"},
			} {
				h := newChatHarness(t)

				rec := h.do(rt.method, "/c/"+url.PathEscape(ids[0])+"/m/"+url.PathEscape(ids[1])+"/card/"+rt.action, rt.form, htmx)

				assert.Contains(t, []int{http.StatusNotFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusSeeOther}, rec.Code, rt.name+" "+ids[1])
			}
		}
	})

	t.Run("without the htmx header, success goes back to the conversation, whatever the header holds", func(t *testing.T) {
		for _, header := range []map[string]string{nil, {"HX-Request": "false"}, {"HX-Request": "TRUE"}, {"HX-Request": "1"}, {"HX-Request": ""}, {"HX-Request": " true"}} {
			for _, rt := range cardRoutes {
				h := newChatHarness(t)
				m := cardReply(domain.InteractionPaid)
				h.expectCard(rt.call, rt.args, m, nil)
				if rt.call == "ChooseContact" {
					h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(service.Turn{}, domain.ErrInvalidState).Once()
				}

				rec := h.do(rt.method, cardPath(rt.action), rt.form, header)

				assert.Equal(t, http.StatusSeeOther, rec.Code, "%s with %v", rt.name, header)
				assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
				assert.Empty(t, rec.Header().Get("HX-Redirect"))
			}
		}
	})

	t.Run("without htmx a refusal goes back to the conversation too, and the reason is not lost to a blank page", func(t *testing.T) {
		for _, refusal := range []error{&domain.ActionError{Message: "wrong code"}, domain.ErrAgentUnavailable} {
			for _, rt := range cardRoutes {
				h := newChatHarness(t)
				h.expectCard(rt.call, rt.args, cardReply(domain.InteractionPending), refusal)

				rec := h.do(rt.method, cardPath(rt.action), rt.form, nil)

				assert.Equal(t, http.StatusSeeOther, rec.Code, rt.name)
				assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
				assert.Empty(t, rec.Body.String(), "a redirect has no page")
			}
		}
	})

	t.Run("a card answered already is a conflict that says so in plain words, on every route", func(t *testing.T) {
		for _, rt := range cardRoutes {
			h := newChatHarness(t)
			h.expectCard(rt.call, rt.args, domain.Message{}, domain.ErrInvalidState)

			rec := h.do(rt.method, cardPath(rt.action), rt.form, htmx)

			assert.Equal(t, http.StatusConflict, rec.Code, rt.name)
			assert.Contains(t, rec.Body.String(), "answered already")
		}
	})

	t.Run("a failure is a 500 that tells nothing of the failure, on every route", func(t *testing.T) {
		for _, rt := range cardRoutes {
			h := newChatHarness(t)
			h.expectCard(rt.call, rt.args, domain.Message{}, errors.New("pq: password authentication failed for user app"))

			rec := h.do(rt.method, cardPath(rt.action), rt.form, htmx)

			assert.Equal(t, http.StatusInternalServerError, rec.Code, rt.name)
			assert.NotContains(t, rec.Body.String(), "pq:")
			assert.NotContains(t, rec.Body.String(), "password")
		}
	})

	t.Run("it needs a signed-in user, on every route", func(t *testing.T) {
		for _, rt := range cardRoutes {
			rec := serve(t, mocks.NewAuth(t), Options{}, postForm(cardPath(rt.action), rt.form, nil))

			assert.Equal(t, http.StatusSeeOther, rec.Code, rt.name)
			assert.Contains(t, rec.Header().Get("Location"), "/login")
		}
	})
}

func TestChoiceOfAContact_OnlyAWholeSmallNumberIsAChoice(t *testing.T) {
	for _, bad := range []string{
		"", " ", " 1", "1 ", "+1", "-1", "-0", "1e1", "0x1", "1_0", "1.0", "1,0", "99999999999999999999", "9223372036854775807",
		"٣", "१", "１", "１２", "\x00", "1\x00", "one", "1;2", "1%00", "०", strings.Repeat("1", 100_000),
	} {
		t.Run(fmt.Sprintf("%.12q is refused with a 400 and nothing is asked", bad), func(t *testing.T) {
			h := newChatHarness(t)

			card := h.do(http.MethodPost, cardPath("contact"), url.Values{"choice": {bad}}, htmx)
			picker := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {"maria"}, "choice": {bad}}, htmx)

			assert.Equal(t, http.StatusBadRequest, card.Code, "on a card")
			assert.Equal(t, http.StatusBadRequest, picker.Code, "on the picker")
			assert.Contains(t, card.Body.String(), "Choose one of the contacts.")
		})
	}

	t.Run("a missing field is refused the same way", func(t *testing.T) {
		h := newChatHarness(t)

		card := h.do(http.MethodPost, cardPath("contact"), url.Values{"other": {"1"}}, htmx)
		picker := h.do(http.MethodPost, "/contacts/confirm", url.Values{"q": {"maria"}}, htmx)

		assert.Equal(t, http.StatusBadRequest, card.Code)
		assert.Equal(t, http.StatusBadRequest, picker.Code)
	})

	t.Run("a choice in the query string and none in the body is none", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodPost, cardPath("contact")+"?choice=1", url.Values{}, htmx)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("digits that are numbers are taken as they are, and the use case says whether they are one of the contacts", func(t *testing.T) {
		for choice, want := range map[string]int{"0": 0, "1": 1, "4": 4, "5": 5, "007": 7, "999": 999} {
			h := newChatHarness(t)
			h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, want).
				Return(contactReplyMsg(domain.InteractionPending), &domain.ActionError{Message: "Choose one of the contacts."}).Once()

			rec := h.do(http.MethodPost, cardPath("contact"), url.Values{"choice": {choice}}, htmx)

			require.Equal(t, http.StatusOK, rec.Code, choice)
			assert.Equal(t, "Choose one of the contacts.", strings.TrimSpace(textContent(byTestID(parseHTML(t, rec.Body.String()), "card-error")[0])))
		}
	})
}

func TestConfirmCode_WhatTheFormHolds(t *testing.T) {
	refused := &domain.ActionError{Message: "Enter the code the bank sent you."}

	t.Run("the code is trimmed, and what is inside it is not touched", func(t *testing.T) {
		for typed, sent := range map[string]string{
			"  123456\n": "123456", "\t12 34\t": "12 34", "12\x0034": "12\x0034", "１２３４": "１２３４", "": "",
		} {
			h := newChatHarness(t)
			h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, sent).Return(cardReply(domain.InteractionOTPRequired), refused).Once()

			rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"code": {typed}}, htmx)

			assert.Equal(t, http.StatusOK, rec.Code, "%q", typed)
		}
	})

	t.Run("no code field is an empty code, and the use case says so", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, "").Return(cardReply(domain.InteractionOTPRequired), refused).Once()

		rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"other": {"x"}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "Enter the code the bank sent you.", strings.TrimSpace(textContent(byTestID(parseHTML(t, rec.Body.String()), "card-error")[0])))
	})

	t.Run("only the first of two codes is taken", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, "111111").Return(cardReply(domain.InteractionPaid), nil).Once()

		rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"code": {"111111", "222222"}}, htmx)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("a hundred kilobytes of code reach the use case, which refuses it, and the card is shown again", func(t *testing.T) {
		h := newChatHarness(t)
		long := strings.Repeat("9", 100<<10)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, long).Return(cardReply(domain.InteractionOTPRequired), refused).Once()

		rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"code": {long}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Less(t, rec.Body.Len(), 8<<10, "the code is not echoed back")
	})

	t.Run("the code is not echoed in the page it is answered with", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, "135790").Return(cardReply(domain.InteractionOTPRequired), &domain.ActionError{Message: "OTP verification failed"}).Once()

		rec := h.do(http.MethodPost, cardPath("otp"), url.Values{"code": {"135790"}}, htmx)

		assert.NotContains(t, rec.Body.String(), "135790")
	})

	t.Run("a form that cannot be read is a 400, and nothing is asked", func(t *testing.T) {
		h := newChatHarness(t)
		req := httptest.NewRequest(http.MethodPost, cardPath("otp"), strings.NewReader("code=%zz"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		rec := httptest.NewRecorder()

		h.route.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// behindTheChain is the app as the server runs it, with a body limit.
func (h *chatHarness) behindTheChain(t *testing.T, limit int64) http.Handler {
	t.Helper()
	h.auth.ExpectedCalls = nil // the body limit refuses a request before it is known who sent it
	h.auth.On("Authenticate", mock.Anything, "tok").Return(alice, nil).Maybe()
	chain, err := Handler(config.Config{
		App:  config.App{BaseURL: "http://localhost:8300"},
		HTTP: config.HTTP{MaxBodyBytes: limit, BodyTimeout: time.Minute, ResponseTimeout: time.Minute},
	}, testLogger(), h.route)
	require.NoError(t, err)
	return chain
}

func TestCardRoutes_ABodyOverTheServerLimit(t *testing.T) {
	big := "code=" + strings.Repeat("9", 4096)

	for _, rt := range []struct{ name, path, body string }{
		{"code", cardPath("otp"), big},
		{"contact", cardPath("contact"), "choice=1&pad=" + strings.Repeat("x", 4096)},
		{"the picker's confirmation", "/contacts/confirm", "choice=1&q=" + strings.Repeat("x", 4096)},
	} {
		t.Run(rt.name+" with a declared length is a 413", func(t *testing.T) {
			h := newChatHarness(t)
			req := postForm(rt.path, url.Values{}, htmx)
			req.Body, req.ContentLength = io.NopCloser(strings.NewReader(rt.body)), int64(len(rt.body))
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
			rec := httptest.NewRecorder()

			h.behindTheChain(t, 1024).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		})

		t.Run(rt.name+" streamed with no length is a 413 too, not a card that says the code is missing", func(t *testing.T) {
			h := newChatHarness(t)
			req := postForm(rt.path, url.Values{}, htmx)
			req.Body, req.ContentLength = io.NopCloser(io.MultiReader(strings.NewReader(rt.body))), -1
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
			rec := httptest.NewRecorder()

			h.behindTheChain(t, 1024).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		})
	}

	t.Run("approve and reject read no body, so a big one is only refused by the limit", func(t *testing.T) {
		h := newChatHarness(t)
		req := postForm(cardPath("approve"), url.Values{}, htmx)
		req.Body, req.ContentLength = io.NopCloser(strings.NewReader(big)), int64(len(big))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		rec := httptest.NewRecorder()

		h.behindTheChain(t, 1024).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	})
}

func TestChooseContact_WhenTheAgentCannotGoOn(t *testing.T) {
	form := url.Values{"choice": {"1"}}

	for name, sendErr := range map[string]error{
		"the message is refused as invalid":   domain.ErrMessageInvalid,
		"the message is too long":             domain.TooLong(10),
		"the agent is unreachable":            domain.ErrAgentUnavailable,
		"another turn is still being written": domain.ErrInvalidState,
		"the server is busy":                  domain.ErrBusy,
		"the conversation is gone":            domain.ErrNotFound,
		"the store fails":                     errors.New("pq: connection refused"),
	} {
		t.Run("when "+name+" the card is still answered, and nothing is added to the conversation", func(t *testing.T) {
			h := newChatHarness(t)
			h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(contactReplyMsg(domain.InteractionConfirmed), nil).Once()
			h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(service.Turn{}, sendErr).Once()

			rec := h.do(http.MethodPost, cardPath("contact"), form, htmx)

			require.Equal(t, http.StatusOK, rec.Code)
			root := parseHTML(t, rec.Body.String())
			assert.Equal(t, "confirmed", attrOf(byTestID(root, "card")[0], "data-state"))
			assert.NotContains(t, rec.Body.String(), "hx-swap-oob")
			assert.NotContains(t, rec.Body.String(), "pq:")
		})
	}

	t.Run("a card answered with no card left on the reply is drawn as the reply alone, and the agent goes on", func(t *testing.T) {
		h := newChatHarness(t)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(reply(domain.StatusComplete, "Done."), nil).Once()
		h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(resumedTurn(), nil).Once()

		rec := h.do(http.MethodPost, cardPath("contact"), form, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Empty(t, byTestID(root, "card"))
		assert.Contains(t, rec.Body.String(), "hx-swap-oob")
	})
}

func resumedTurn() service.Turn {
	user := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: 3, Role: domain.RoleUser, Status: domain.StatusComplete, Content: "usuario confirmado"}
	next := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: 4, Role: domain.RoleAssistant, Status: domain.StatusPending}
	return service.Turn{User: user, Reply: next}
}

// Choosing a contact sends a message to the agent, so it is counted with the sends: a page that is clicked at a thousand
// times a second does not make the agent run a thousand times.
func TestChooseContact_IsLimitedLikeASend(t *testing.T) {
	limited := func(t *testing.T, burst int) *chatHarness {
		t.Helper()
		auth := mocks.NewAuth(t)
		auth.On("Authenticate", mock.Anything, "tok").Return(alice, nil).Maybe()
		h := &chatHarness{t: t, auth: auth, chat: mocks.NewChat(t), convs: mocks.NewConversation(t), cards: mocks.NewInteractions(t)}
		h.route = testAppWith(t, Services{Auth: h.auth, Chat: h.chat, Conversations: h.convs, Interactions: h.cards},
			Options{RateLimitRPS: 0.0001, RateLimitBurst: burst}).Routes()
		return h
	}

	t.Run("a burst beyond the limit is a 429 with the wait, and the card is not answered", func(t *testing.T) {
		h := limited(t, 2)
		h.cards.On("ChooseContact", mock.Anything, alice.ID, convID, streamMsgID, 1).Return(contactReplyMsg(domain.InteractionConfirmed), nil).Twice()
		h.chat.On("Send", mock.Anything, alice.ID, convID, "usuario confirmado").Return(resumedTurn(), nil).Twice()
		for range 2 {
			require.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, cardPath("contact"), url.Values{"choice": {"1"}}, htmx).Code)
		}

		rec := h.as("tok", http.MethodPost, cardPath("contact"), url.Values{"choice": {"1"}}, htmx)

		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
		h.cards.AssertNumberOfCalls(t, "ChooseContact", 2)
	})

	t.Run("it shares the bucket of the messages", func(t *testing.T) {
		h := limited(t, 1)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "hi").Return(resumedTurn(), nil).Once()
		require.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, "/c/"+convID.String()+"/messages", url.Values{"content": {"hi"}}, htmx).Code)

		rec := h.as("tok", http.MethodPost, cardPath("contact"), url.Values{"choice": {"1"}}, htmx)

		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	})

	t.Run("paying, rejecting, the code and the picker are not counted", func(t *testing.T) {
		h := limited(t, 1)
		h.cards.On("Approve", mock.Anything, alice.ID, convID, streamMsgID).Return(cardReply(domain.InteractionPaid), nil).Times(3)
		h.cards.On("Reject", mock.Anything, alice.ID, convID, streamMsgID).Return(cardReply(domain.InteractionRejected), nil).Times(3)
		h.cards.On("ConfirmOTP", mock.Anything, alice.ID, convID, streamMsgID, "123456").Return(cardReply(domain.InteractionPaid), nil).Times(3)
		for range 3 {
			assert.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, cardPath("approve"), nil, htmx).Code)
			assert.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, cardPath("reject"), nil, htmx).Code)
			assert.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, cardPath("otp"), url.Values{"code": {"123456"}}, htmx).Code)
			assert.Equal(t, http.StatusOK, h.as("tok", http.MethodGet, "/contacts?q=ma", nil, htmx).Code)
		}
	})
}

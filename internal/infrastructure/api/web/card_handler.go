package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"iape-ui/internal/core/domain"
)

// cardView is what the card template draws: the transfer an approval card asks the user to pay or reject.
type cardView struct {
	State       string // the card's state, for the page and the explorer
	Open        bool   // still waits for the user
	CanPay      bool   // pending: Pay and Reject
	AsksForCode bool   // the bank asked for a code: the field, and Reject
	Amount      string
	Name, Phone string
	Message     string
	Result      string // the service's words about the last answer
	Error       string // why the last answer did not go through
	ApproveURL  string
	RejectURL   string
	CodeURL     string
}

// newCardView draws the card of m, or none when the reply has no approval card.
func newCardView(m domain.Message) *cardView {
	card := m.Meta.Interaction
	if card == nil || card.Kind != domain.InteractionApproval {
		return nil
	}
	base := "/c/" + m.ConversationID.String() + "/m/" + m.ID.String() + "/card/"
	return &cardView{
		State:       string(card.State),
		Open:        card.Open(),
		CanPay:      card.State == domain.InteractionPending,
		AsksForCode: card.State == domain.InteractionOTPRequired,
		Amount:      amountText(card.Transfer),
		Name:        card.Transfer.DestinationName,
		Phone:       card.Transfer.DestinationPhone,
		Message:     card.Transfer.Message,
		Result:      card.Result,
		ApproveURL:  base + "approve",
		RejectURL:   base + "reject",
		CodeURL:     base + "otp",
	}
}

// amountText is the amount as a person reads it: soles with their sign, any other currency after the number.
func amountText(t domain.Transfer) string {
	switch t.Currency {
	case "PEN", "":
		return fmt.Sprintf("S/ %.2f", t.Amount)
	default:
		return fmt.Sprintf("%.2f %s", t.Amount, t.Currency)
	}
}

func (a *App) approveCard(w http.ResponseWriter, r *http.Request) {
	a.answerCard(w, r, func(ref messageRef) (domain.Message, error) {
		return a.svc.Interactions.Approve(r.Context(), ref.userID, ref.convID, ref.msgID)
	})
}

func (a *App) rejectCard(w http.ResponseWriter, r *http.Request) {
	a.answerCard(w, r, func(ref messageRef) (domain.Message, error) {
		return a.svc.Interactions.Reject(r.Context(), ref.userID, ref.convID, ref.msgID)
	})
}

func (a *App) confirmCode(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PostFormValue("code"))
	a.answerCard(w, r, func(ref messageRef) (domain.Message, error) {
		return a.svc.Interactions.ConfirmOTP(r.Context(), ref.userID, ref.convID, ref.msgID, code)
	})
}

// answerCard answers a request that answers a card with the reply to swap in, its card in the new state. When the answer did not
// go through, the reply comes back with the card as it was and the reason on it, with a 200: htmx swaps only successful responses,
// and the card has to show why.
func (a *App) answerCard(w http.ResponseWriter, r *http.Request, answer func(messageRef) (domain.Message, error)) {
	ref, ok := messageRefOf(w, r)
	if !ok {
		return
	}
	msg, err := answer(ref)
	switch refused, isRefusal := errors.AsType[*domain.ActionError](err); {
	case err == nil:
		a.answerWithReply(w, r, ref.convID, msg)
	case errors.Is(err, domain.ErrInvalidState): // answered already, perhaps from another tab
		http.Error(w, "That card was answered already.", http.StatusConflict)
	case isRefusal || errors.Is(err, domain.ErrAgentUnavailable):
		view := a.messageView(msg)
		if view.Card != nil {
			view.Card.Error = cardErrorText(refused)
		}
		if !fromHTMX(r) {
			redirect(w, r, "/c/"+ref.convID.String())
			return
		}
		a.block(w, r, http.StatusOK, "partial:reply", view)
	default:
		a.fail(w, r, err)
	}
}

// cardErrorText is what the card says when its answer did not go through: the service's own words when it refused, else a plain
// invitation to try again.
func cardErrorText(refused *domain.ActionError) string {
	if refused != nil {
		return refused.Message
	}
	return "The payment service did not answer. Try again."
}

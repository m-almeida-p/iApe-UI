package web

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"iape-ui/internal/core/domain"
)

// cardView is what the card template draws: the transfer an approval card asks the user to pay or reject.
type cardView struct {
	State       string // the card's state, for the page and the explorer
	Open        bool   // still waits for the user
	CanPay      bool   // pending, and it says how much and to whom: Pay and Reject
	Unpayable   bool   // pending, but it does not say how much or to whom: Reject only
	AsksForCode bool   // the bank asked for a code: the field, and Reject
	Amount      string
	Name, Phone string
	Message     string
	Result      string // the service's words about the last answer
	Error       string // why the last answer did not go through
	ApproveURL  string
	RejectURL   string
	CodeURL     string
	// A contact card: who the user may choose (each button posts its index) and whom they chose.
	IsContact  bool
	Candidates []candidateView
	ContactURL string
	Chosen     string
}

// candidateView is a person to choose, with the index its button posts.
type candidateView struct {
	Name, Phone string
	Choice      string
}

// newCardView draws the card of m, or none when the reply has no approval card.
func newCardView(m domain.Message) *cardView {
	card := m.Meta.Interaction
	if card == nil {
		return nil
	}
	base := "/c/" + m.ConversationID.String() + "/m/" + m.ID.String() + "/card/"
	if card.Kind == domain.InteractionContact {
		return newContactView(card, base)
	}
	if card.Kind != domain.InteractionApproval {
		return nil
	}
	pending := card.State == domain.InteractionPending
	return &cardView{
		State:       string(card.State),
		Open:        card.Open(),
		CanPay:      pending && payable(card.Transfer),
		Unpayable:   pending && !payable(card.Transfer),
		AsksForCode: card.State == domain.InteractionOTPRequired,
		Amount:      amountText(card.Transfer),
		Name:        plain(card.Transfer.DestinationName),
		Phone:       plain(card.Transfer.DestinationPhone),
		Message:     plain(card.Transfer.Message),
		Result:      plain(card.Result),
		ApproveURL:  base + "approve",
		RejectURL:   base + "reject",
		CodeURL:     base + "otp",
	}
}

// newContactView draws a contact card: the candidates while it is pending, whom the user chose once it is confirmed.
func newContactView(card *domain.Interaction, base string) *cardView {
	v := &cardView{State: string(card.State), Open: card.Open(), IsContact: true, ContactURL: base + "contact"}
	if card.State == domain.InteractionPending {
		for i, c := range domain.Candidates(card.Query) {
			v.Candidates = append(v.Candidates, candidateView{Name: c.Name, Phone: c.Phone, Choice: strconv.Itoa(i)})
		}
	}
	if card.State == domain.InteractionConfirmed {
		var who []string
		for _, part := range []string{card.Contact.Name, card.Contact.Phone} {
			if part = strings.TrimSpace(plain(part)); part != "" {
				who = append(who, part)
			}
		}
		v.Chosen = strings.Join(who, " · ")
	}
	return v
}

// payable reports whether a transfer says how much and to whom, which a person needs to know to pay it: an amount above zero,
// and a name or a phone. A card that does not is shown, and can be rejected, but not paid from here.
func payable(t domain.Transfer) bool {
	return t.Amount > 0 && !math.IsInf(t.Amount, 1) && (strings.TrimSpace(t.DestinationName) != "" || strings.TrimSpace(t.DestinationPhone) != "")
}

// plain is a text that came from the agent's service without what it could use to pass for something else on the page: the
// characters that turn the direction of the text around (a name that reads "Rosa" and puts the phone before it), and the
// control characters but the line break and the tab.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), r == '\u061c', r == '\u200e', r == '\u200f', r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			return -1
		}
		return r
	}, s)
}

// amountText is the amount as a person reads it: soles with their sign, any other currency after the number.
func amountText(t domain.Transfer) string {
	currency := strings.TrimSpace(t.Currency)
	if currency == "" || strings.EqualFold(currency, "PEN") {
		return fmt.Sprintf("S/ %.2f", t.Amount)
	}
	return fmt.Sprintf("%.2f %s", t.Amount, plain(currency))
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
	if !a.parseForm(w, r) {
		return
	}
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
	if err == nil {
		a.answerWithReply(w, r, ref.convID, msg)
		return
	}
	a.cardFailed(w, r, ref, msg, err)
}

// cardFailed answers a request whose answer to a card did not go through: the reply comes back with the card as it was, and the
// reason on it, with a 200 (htmx swaps only successful responses and the card has to show why). A card answered already, perhaps
// from another tab, is a 409.
func (a *App) cardFailed(w http.ResponseWriter, r *http.Request, ref messageRef, msg domain.Message, err error) {
	refused, isRefusal := errors.AsType[*domain.ActionError](err)
	switch {
	case errors.Is(err, domain.ErrInvalidState):
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

// resumeText is what the user's choice of a contact says to the agent, so that it goes on. The BFF's own page sends the same.
const resumeText = "usuario confirmado"

// chooseContact confirms the contact the user chose and asks the agent to go on, as the BFF's own page does: the choice is a
// message of the conversation, "usuario confirmado". The answer is the card as it is now, and the next turn added to the
// conversation out of band.
func (a *App) chooseContact(w http.ResponseWriter, r *http.Request) {
	ref, ok := messageRefOf(w, r)
	if !ok {
		return
	}
	if !a.parseForm(w, r) {
		return
	}
	choice, ok := parseChoice(r.PostFormValue("choice"))
	if !ok {
		http.Error(w, "Choose one of the contacts.", http.StatusBadRequest)
		return
	}
	msg, err := a.svc.Interactions.ChooseContact(r.Context(), ref.userID, ref.convID, ref.msgID, choice)
	if err != nil {
		a.cardFailed(w, r, ref, msg, err)
		return
	}
	turn, sendErr := a.svc.Chat.Send(r.Context(), ref.userID, ref.convID, resumeText)
	if sendErr != nil { // the card is answered; the user can write to the agent themselves
		a.log.WarnContext(r.Context(), "the agent was not asked to go on after a contact was chosen", "error", sendErr)
		a.answerWithReply(w, r, ref.convID, msg)
		return
	}
	if !fromHTMX(r) {
		redirect(w, r, "/c/"+ref.convID.String())
		return
	}
	card, err := a.renderer.Block("partial:reply", a.messageView(msg))
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	var next strings.Builder
	for _, m := range []domain.Message{turn.User, turn.Reply} {
		html, err := a.renderer.Block("partial:message", a.messageView(m))
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		next.WriteString(html)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, card+`<div hx-swap-oob="beforeend:#messages">`+next.String()+`</div>`)
}

// parseChoice reads the index of a candidate: one to three digits, as the buttons send them. A sign, a space, a digit of
// another script or a number that no list is long enough for is no choice.
func parseChoice(s string) (int, bool) {
	if s == "" || len(s) > 3 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// cardErrorText is what the card says when its answer did not go through: the service's own words when it refused, else a plain
// invitation to try again.
func cardErrorText(refused *domain.ActionError) string {
	if refused != nil {
		return plain(refused.Message)
	}
	return "The payment service did not answer. Try again."
}

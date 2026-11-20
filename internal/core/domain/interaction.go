package domain

import (
	"fmt"
	"strings"
	"time"
)

// InteractionKind says what a card asks of the user.
type InteractionKind string

// The cards there are.
const (
	// InteractionApproval is a transfer the user has to pay or reject.
	InteractionApproval InteractionKind = "approval"
	// InteractionContact asks the user to confirm who the agent should pay.
	InteractionContact InteractionKind = "contact"
)

// InteractionState is where a card is. It only goes forward: a card that was paid or rejected is closed.
type InteractionState string

// The states of an approval card.
const (
	// InteractionPending waits for the user to pay or reject.
	InteractionPending InteractionState = "pending"
	// InteractionOTPRequired was paid and asks for the code the bank sent.
	InteractionOTPRequired InteractionState = "otp_required"
	// InteractionPaid is closed: the transfer went through.
	InteractionPaid InteractionState = "paid"
	// InteractionRejected is closed: the user said no.
	InteractionRejected InteractionState = "rejected"
	// InteractionConfirmed is closed: the user chose the contact.
	InteractionConfirmed InteractionState = "confirmed"
	// InteractionProcessing is an answer on its way to the agent's service: someone claimed the card before asking, so that no other
	// answer to it is sent too. The card's state before the claim is in Resume.
	InteractionProcessing InteractionState = "processing"
)

// ClaimTTL is how long an answer is given to come back before its claim can be taken over: a server that died in the middle of a call
// must not leave a card nobody can answer.
const ClaimTTL = 2 * time.Minute

// Transfer is what an approval card shows. The amount is for display, it is never computed with.
type Transfer struct {
	Amount           float64 `json:"amount,omitzero"`
	Currency         string  `json:"currency,omitempty"`
	DestinationName  string  `json:"destination_name,omitempty"`
	DestinationPhone string  `json:"destination_phone,omitempty"`
	Message          string  `json:"message,omitempty"`
}

// Contact is a person the user can pay.
type Contact struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

// Interaction is a card in the agent's reply that the user answers. It is kept in the message's Meta, so the card is
// the same after a reload and a card that was answered cannot be answered twice.
type Interaction struct {
	Kind  InteractionKind  `json:"kind"`
	State InteractionState `json:"state"`
	// ApprovalID names the transfer to the agent's service.
	ApprovalID string `json:"approval_id"`
	// RunID is the run of the turn that produced the card: the service wants it with every answer.
	RunID    string   `json:"run_id,omitempty"`
	Transfer Transfer `json:"transfer,omitzero"`
	// ChallengeID names the code the bank asked for, once it did.
	ChallengeID string `json:"challenge_id,omitempty"`
	// Result is what the service said about the last answer, to show on the card.
	Result string `json:"result,omitempty"`

	// RequestID, Query and Prompt are those of a contact card: the agent's request, what it asked about, and what it says to the user.
	RequestID string `json:"request_id,omitempty"`
	Query     string `json:"query,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	// Contact is whom the user chose, once they did.
	Contact Contact `json:"contact,omitzero"`

	// ClaimID, ClaimedAt and Resume are the claim of a card in state processing: who holds it, since when, and the state it goes back to
	// if the answer is refused.
	ClaimID   string           `json:"claim_id,omitempty"`
	ClaimedAt time.Time        `json:"claimed_at,omitzero"`
	Resume    InteractionState `json:"resume,omitempty"`
}

// Open reports whether the card still waits for the user.
func (i Interaction) Open() bool {
	return i.State == InteractionPending || i.State == InteractionOTPRequired
}

// Approved records that the user paid. When the bank wants a code, challengeID names it and the card asks for it; else the
// transfer is paid. A card that is not pending cannot be approved: ErrInvalidState.
func (i Interaction) Approved(result, challengeID string) (Interaction, error) {
	if i.Kind != InteractionApproval || i.State != InteractionPending {
		return i, fmt.Errorf("approving a %s card that is %s: %w", i.Kind, i.State, ErrInvalidState)
	}
	i.Result = result
	if challengeID != "" {
		i.State, i.ChallengeID = InteractionOTPRequired, challengeID
		return i, nil
	}
	i.State = InteractionPaid
	return i, nil
}

// Rejected records that the user said no. A card that is closed cannot be rejected: ErrInvalidState.
func (i Interaction) Rejected(result string) (Interaction, error) {
	if i.Kind != InteractionApproval || !i.Open() {
		return i, fmt.Errorf("rejecting a %s card that is %s: %w", i.Kind, i.State, ErrInvalidState)
	}
	i.State, i.Result = InteractionRejected, result
	return i, nil
}

// CodeConfirmed records that the code the bank asked for was right, and the transfer is paid. Only a card that asked for a
// code takes one: ErrInvalidState.
func (i Interaction) CodeConfirmed(result string) (Interaction, error) {
	if i.Kind != InteractionApproval || i.State != InteractionOTPRequired {
		return i, fmt.Errorf("confirming a code on a %s card that is %s: %w", i.Kind, i.State, ErrInvalidState)
	}
	i.State, i.Result = InteractionPaid, result
	return i, nil
}

// ActionResult is what the agent's service answered to a card's action.
type ActionResult struct {
	// Message is the service's own words for the user ("Transfer paid successfully. TxnID: ...").
	Message string
	// ChallengeID names the code the bank asks for; empty when none is needed.
	ChallengeID string
}

// ActionError is a card's action that the service refused (a wrong code, a transfer that is no longer open): its message is
// for the user and the card stays as it was. Resolved is what the approval already is when the refusal says so ("APPROVED",
// "REJECTED"): an answer that was sent twice, or whose reply was lost, finds out here what became of it.
type ActionError struct {
	Message  string
	Resolved string
}

func (e *ActionError) Error() string { return "action refused: " + e.Message }

// ContactChosen records whom the user chose. Only a contact card that is pending takes it, and only a contact that has a name
// and a phone, which the agent's service would otherwise be told is confirmed: ErrInvalidState.
func (i Interaction) ContactChosen(c Contact) (Interaction, error) {
	if i.Kind != InteractionContact || i.State != InteractionPending {
		return i, fmt.Errorf("choosing a contact on a %s card that is %s: %w", i.Kind, i.State, ErrInvalidState)
	}
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Phone) == "" {
		return i, fmt.Errorf("choosing a contact that has no name or no phone: %w", ErrInvalidState)
	}
	i.State, i.Contact = InteractionConfirmed, c
	return i, nil
}

// HoldsPayment reports whether the agent's service may have taken or be taking the transfer of the card: its answer is on its way,
// it waits for the code, or it paid. The reply that holds such a card must not be replaced.
func (i Interaction) HoldsPayment() bool {
	return i.State == InteractionProcessing || i.State == InteractionOTPRequired || i.State == InteractionPaid
}

// Claim marks the card as being answered, by the claim named id at now. The service is asked only by whoever holds the claim, so a
// double tap, a second tab or a retry of the phone's network cannot send the same answer twice. A card that cannot be answered (closed,
// or already claimed) is ErrInvalidState: take over a stale claim with Reclaim.
func (i Interaction) Claim(id string, now time.Time) (Interaction, error) {
	if id == "" || (i.State != InteractionPending && i.State != InteractionOTPRequired) {
		return i, fmt.Errorf("claiming a %s card that is %s: %w", i.Kind, i.State, ErrInvalidState)
	}
	return i.claimed(id, now, i.State), nil
}

func (i Interaction) claimed(id string, now time.Time, resume InteractionState) Interaction {
	i.State, i.ClaimID, i.ClaimedAt, i.Resume = InteractionProcessing, id, now, resume
	return i
}

// Stale reports whether the claim on the card has had its time (ClaimTTL) at now.
func (i Interaction) Stale(now time.Time) bool {
	return i.State == InteractionProcessing && !now.Before(i.ClaimedAt.Add(ClaimTTL))
}

// Reclaim takes over a stale claim for the claim named id, which keeps the state the card goes back to. A card whose claim is still
// within its time, or that is not claimed, is ErrInvalidState.
func (i Interaction) Reclaim(id string, now time.Time) (Interaction, error) {
	if id == "" || !i.Stale(now) {
		return i, fmt.Errorf("taking over a %s card that is %s: %w", i.Kind, i.State, ErrInvalidState)
	}
	return i.claimed(id, now, i.Resume), nil
}

// Base is the card as it was before it was claimed; a card that was not claimed is its own base.
func (i Interaction) Base() Interaction {
	if i.State != InteractionProcessing {
		return i
	}
	i.State, i.ClaimID, i.ClaimedAt, i.Resume = i.Resume, "", time.Time{}, ""
	return i
}

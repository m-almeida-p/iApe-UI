package domain

import "fmt"

// InteractionKind says what a card asks of the user.
type InteractionKind string

// The cards there are.
const (
	// InteractionApproval is a transfer the user has to pay or reject.
	InteractionApproval InteractionKind = "approval"
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
)

// Transfer is what an approval card shows. The amount is for display, it is never computed with.
type Transfer struct {
	Amount           float64 `json:"amount,omitzero"`
	Currency         string  `json:"currency,omitempty"`
	DestinationName  string  `json:"destination_name,omitempty"`
	DestinationPhone string  `json:"destination_phone,omitempty"`
	Message          string  `json:"message,omitempty"`
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
}

// Open reports whether the card still waits for the user.
func (i Interaction) Open() bool {
	return i.State == InteractionPending || i.State == InteractionOTPRequired
}

// Approved records that the user paid. When the bank wants a code, challengeID names it and the card asks for it; else the
// transfer is paid. A card that is not pending cannot be approved: ErrInvalidState.
func (i Interaction) Approved(result, challengeID string) (Interaction, error) {
	if i.State != InteractionPending {
		return i, fmt.Errorf("approving a card that is %s: %w", i.State, ErrInvalidState)
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
	if !i.Open() {
		return i, fmt.Errorf("rejecting a card that is %s: %w", i.State, ErrInvalidState)
	}
	i.State, i.Result = InteractionRejected, result
	return i, nil
}

// CodeConfirmed records that the code the bank asked for was right, and the transfer is paid. Only a card that asked for a
// code takes one: ErrInvalidState.
func (i Interaction) CodeConfirmed(result string) (Interaction, error) {
	if i.State != InteractionOTPRequired {
		return i, fmt.Errorf("confirming a code on a card that is %s: %w", i.State, ErrInvalidState)
	}
	i.State, i.Result = InteractionPaid, result
	return i, nil
}

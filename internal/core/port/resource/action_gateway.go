package resource

import (
	"context"

	"iape-ui/internal/core/domain"
)

// ActionGateway answers the cards of the agent's replies: it tells the agent's service what the user chose. An answer is not a
// turn of the agent, so it has its own port. The card says which transfer and which run; the gateway adds who is asking.
//
//go:generate mockery --name ActionGateway --filename action_gateway.go
type ActionGateway interface {
	// Approve pays the transfer. The result carries a ChallengeID when the bank asks for a code first.
	Approve(ctx context.Context, card domain.Interaction) (domain.ActionResult, error)

	// Reject cancels the transfer.
	Reject(ctx context.Context, card domain.Interaction) (domain.ActionResult, error)

	// ConfirmContact tells the agent's service whom the user chose for query, the reference the agent gave. It is also what the
	// composer's @ picker does before the name goes in a message.
	ConfirmContact(ctx context.Context, query string, contact domain.Contact) error

	// ConfirmOTP gives the code the bank asked for. A wrong code is a *domain.ActionError and the card stays as it was.
	ConfirmOTP(ctx context.Context, card domain.Interaction, code string) (domain.ActionResult, error)
}

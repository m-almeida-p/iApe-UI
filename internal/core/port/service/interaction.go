package service

import (
	"context"
	"uuid"

	"iape-ui/internal/core/domain"
)

// Interactions answers the cards in the agent's replies. Each call returns the reply as saved, with its card in the new state.
// All of them: a message that is not the user's is domain.ErrNotFound; a reply with no card, or one in a state that does not
// take the answer, is domain.ErrInvalidState. When the answer did not go through (the service
// refused it, a wrong code for one, or it could not be reached: ErrAgentUnavailable) the reply is returned with its card as it
// was, along with the error, so the page can show it again with the reason.
//
//go:generate mockery --name Interactions --filename interaction.go
type Interactions interface {
	// Approve pays the transfer. The card then asks for a code or is paid.
	Approve(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)

	// Reject cancels the transfer.
	Reject(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)

	// ConfirmOTP gives the code the card asked for.
	ConfirmOTP(ctx context.Context, userID, convID, msgID uuid.UUID, code string) (domain.Message, error)
}

package resource

import (
	"context"
	"uuid"

	"iape-ui/internal/core/domain"
)

// HistoryGateway reads the conversation history, the only place the chats are kept. Every call is made for one account, and the
// history answers with what that account owns: a conversation of another account is domain.ErrNotFound, the same as one that does
// not exist.
//
//go:generate mockery --name HistoryGateway --filename history_gateway.go
type HistoryGateway interface {
	// Conversations lists the account's conversations, the one that last received a message first. An invalid page is
	// domain.ErrInvalidPage.
	Conversations(ctx context.Context, account uuid.UUID, page domain.PageRequest) (domain.ConversationList, error)

	// Messages reads a page of a conversation, oldest message first. Without a cursor it is the latest page. A conversation that is
	// not the account's is domain.ErrNotFound. When the history cannot be read, the error wraps domain.ErrHistoryUnavailable.
	Messages(ctx context.Context, account uuid.UUID, conversationID string, page domain.PageRequest) (domain.Transcript, error)
}

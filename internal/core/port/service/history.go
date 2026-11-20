package service

import (
	"context"
	"uuid"

	"iape-ui/internal/core/domain"
)

// History reads the conversation history for a user: the conversations of their Yape account, and what each one said. It is what the
// sidebar and a conversation page show, and the only place the chats are kept.
//
//go:generate mockery --name History --filename history.go
type History interface {
	// Conversations lists the user's conversations, the one that last received a message first. An invalid page is
	// domain.ErrInvalidPage; a history that cannot be read wraps domain.ErrHistoryUnavailable.
	Conversations(ctx context.Context, userID uuid.UUID, page domain.PageRequest) (domain.ConversationList, error)

	// Transcript reads a page of one conversation, oldest message first; without a cursor, the latest page. A conversation that is not
	// the user's, or that does not exist, is domain.ErrNotFound.
	Transcript(ctx context.Context, userID uuid.UUID, convID string, page domain.PageRequest) (domain.Transcript, error)
}

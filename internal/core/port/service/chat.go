// Package service declares the inbound ports: the use cases the HTTP adapter
// calls. The packages under internal/core implement them.
package service

import (
	"context"
	"iter"
	"uuid"

	"iape-ui/internal/core/domain"
)

// Turn is what sending a message produces: the user's message and the empty
// reply that is waiting to be streamed.
type Turn struct {
	User  domain.Message
	Reply domain.Message
}

// StreamKind says what a StreamEvent carries.
type StreamKind string

// Stream event kinds.
const (
	// StreamSnapshot carries the whole reply so far.
	StreamSnapshot StreamKind = "snapshot"
	// StreamDone ends the stream and carries the final state.
	StreamDone StreamKind = "done"
)

// StreamEvent is one step of a streamed reply. Snapshots carry the full text so
// far rather than a fragment, so a consumer can drop or coalesce them freely.
//
// Pattern: Whole-Text Snapshots
type StreamEvent struct {
	Kind StreamKind
	Text string
	// The fields below are set on StreamDone only.
	Status domain.Status
	// Error is a message for the user when Status is failed.
	Error string
	Meta  domain.Meta
}

// Chat is the chat use case: the turns of the conversations that are live in this server. A turn lives in memory, in the
// conversation the UI made the id of; the conversation history (see History) is where the chats are kept, and what a turn adds to
// it arrives there from the agent's own record. Every call is for one user, and what is not theirs is domain.ErrNotFound.
//
//go:generate mockery --name Chat --filename chat.go
type Chat interface {
	// NewConversation makes the id of a chat that is about to start (see domain.NewConversationID) and keeps it for the user: only
	// they can write to it.
	NewConversation(userID uuid.UUID) string

	// Owns reports whether the conversation was started here by the user, and the history may not know it yet.
	Owns(userID uuid.UUID, convID string) bool

	// Send adds a message to a conversation: one the user started here (NewConversation), or one of theirs that the history has,
	// which is how a conversation is continued. An id that is not a conversation id is domain.ErrInvalidConversationID; one that
	// is neither is domain.ErrNotFound, which is also what another user's conversation is. Bad input is a *domain.ValidationError; a
	// turn already in flight is domain.ErrInvalidState; a history that cannot be asked is domain.ErrHistoryUnavailable.
	Send(ctx context.Context, userID uuid.UUID, convID, content string) (Turn, error)

	// Stream produces a pending reply by running the agent, and yields the
	// text as it grows, then a StreamDone. A reply that already finished is
	// replayed from memory without calling the agent. A reply another
	// connection is producing is domain.ErrAlreadyStreaming.
	//
	// When ctx ends or the caller stops iterating, the agent run is canceled
	// and the text so far is kept as a stopped reply.
	Stream(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) iter.Seq2[StreamEvent, error]

	// Stop ends a reply that is being written, or that was never started, and
	// returns it as kept: stopped, with the text so far. A reply that already
	// finished is returned as it is.
	Stop(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error)

	// Reply returns a live reply as it is now.
	Reply(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error)
}

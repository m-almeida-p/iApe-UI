package resource

import (
	"uuid"

	"iape-ui/internal/core/domain"
)

// LiveStore keeps what is live in this server: the replies being written or just written, with their cards, and whose the
// conversations that were started here are. It is memory. Nothing in it is a record: the conversation history is where the chats are
// kept, and a restart empties the store. What a reply is worth after that is what the history has of it.
//
// Every call that names a user answers for that user alone: a reply of another user is domain.ErrNotFound, the same as one that does
// not exist.
//
//go:generate mockery --name LiveStore --filename live_store.go
type LiveStore interface {
	// Adopt records that the conversation is the user's, if it has no owner yet, and reports whether it is the user's: false when
	// another user adopted it first.
	Adopt(userID uuid.UUID, convID string) bool

	// Owner reports who adopted the conversation, and whether anyone did.
	Owner(convID string) (userID uuid.UUID, known bool)

	// Begin registers a reply in the conversation it names. A conversation with a reply that is not finished takes no other:
	// domain.ErrInvalidState. A store that is full of unfinished replies is domain.ErrBusy.
	Begin(userID uuid.UUID, reply domain.Message) error

	// Get returns the live reply.
	Get(userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error)

	// Update changes a live reply in one step: change gets it as it is and returns what it becomes, or an error, which leaves it as it
	// was and is returned. change runs with the reply held, so it must be quick and must not call the store. The reply as it became is
	// returned.
	Update(userID uuid.UUID, convID string, msgID uuid.UUID, change func(domain.Message) (domain.Message, error)) (domain.Message, error)
}

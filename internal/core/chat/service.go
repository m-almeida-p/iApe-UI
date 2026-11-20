// Package chat implements the chat use case: sending messages and streaming the agent's answer, for the conversations that are live
// in this server. The conversation history is where the chats are kept (see package history); here a turn lives in memory.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
)

// DefaultMaxReplyBytes is how big a reply may grow before it is cut off. It is
// the size beyond which the page stops interpreting Markdown, and a reply that
// large is not an answer a person will read.
const DefaultMaxReplyBytes = 1 << 20

// Dependencies are what the chat service needs.
type Dependencies struct {
	Live  resource.LiveStore
	Agent resource.AgentGateway
	// History is asked whether a conversation that was not started here is the user's, which is how a conversation of the history is
	// continued. It is nil when there is no history: then only the conversations started here can be written to.
	History         resource.HistoryGateway
	Logger          *slog.Logger
	MaxMessageChars int
	// MaxReplyBytes is how big a reply may grow; zero means DefaultMaxReplyBytes.
	MaxReplyBytes int
}

// Service implements service.Chat.
type Service struct {
	deps Dependencies
	runs runs
}

var _ service.Chat = (*Service)(nil)

// NewService returns a chat service.
func NewService(d Dependencies) *Service {
	if d.MaxReplyBytes <= 0 {
		d.MaxReplyBytes = DefaultMaxReplyBytes
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{deps: d}
}

// NewConversation implements service.Chat.
func (s *Service) NewConversation(userID uuid.UUID) string {
	id := domain.NewConversationID()
	s.deps.Live.Adopt(userID, id) // a new id has no owner yet
	return id
}

// Owns implements service.Chat.
func (s *Service) Owns(userID uuid.UUID, convID string) bool {
	owner, known := s.deps.Live.Owner(convID)
	return known && owner == userID
}

// Send implements service.Chat.
func (s *Service) Send(ctx context.Context, userID uuid.UUID, convID, content string) (service.Turn, error) {
	if _, err := domain.ParseConversationID(convID); err != nil {
		return service.Turn{}, domain.ErrInvalidConversationID
	}
	content, err := domain.NormalizeContent(content, s.deps.MaxMessageChars)
	if err != nil {
		return service.Turn{}, err
	}
	if err := s.own(ctx, userID, convID); err != nil {
		return service.Turn{}, err
	}
	user := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Role: domain.RoleUser, Content: content, Status: domain.StatusComplete}
	reply := domain.Message{
		ID: uuid.NewV7(), ConversationID: convID, Role: domain.RoleAssistant, Prompt: content, Status: domain.StatusPending,
	}
	if err := s.deps.Live.Begin(userID, reply); err != nil {
		return service.Turn{}, fmt.Errorf("sending message: %w", err)
	}
	return service.Turn{User: user, Reply: reply}, nil
}

// own makes sure the conversation is the user's before anything is written to it. One that was started here is known; any other is
// asked of the history, which answers for the account of the user only, and is the user's if it has it. The agent does not check whose
// a conversation is, so this is the one place that does: an id that nobody here knows and the history does not have for the user is
// not written to, which also keeps one user from writing into the conversation of another.
func (s *Service) own(ctx context.Context, userID uuid.UUID, convID string) error {
	if owner, known := s.deps.Live.Owner(convID); known {
		if owner != userID {
			return domain.ErrNotFound
		}
		return nil
	}
	if s.deps.History == nil {
		return domain.ErrNotFound
	}
	if _, err := s.deps.History.Messages(ctx, userID, convID, domain.PageRequest{Size: 1}); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("checking the conversation: %w", err)
	}
	if !s.deps.Live.Adopt(userID, convID) {
		return domain.ErrNotFound
	}
	return nil
}

// Reply implements service.Chat.
func (s *Service) Reply(_ context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error) {
	return s.deps.Live.Get(userID, convID, msgID)
}

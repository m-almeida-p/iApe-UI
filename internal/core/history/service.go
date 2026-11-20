// Package history implements the use case of reading the conversation history for a user.
package history

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
)

// Dependencies are what the service needs. A nil Logger is the default logger.
type Dependencies struct {
	History resource.HistoryGateway
	Logger  *slog.Logger
}

// Service implements service.History.
type Service struct{ deps Dependencies }

var _ service.History = (*Service)(nil)

// NewService returns the service.
func NewService(d Dependencies) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{deps: d}
}

// Conversations implements service.History.
func (s *Service) Conversations(ctx context.Context, userID uuid.UUID, page domain.PageRequest) (domain.ConversationList, error) {
	// The id of a user is the id of their Yape account: the history is asked for the account of the user, and for no other.
	list, err := s.deps.History.Conversations(ctx, userID, page)
	s.note(ctx, "listing the conversations", err)
	return list, err
}

// Transcript implements service.History.
func (s *Service) Transcript(ctx context.Context, userID uuid.UUID, convID string, page domain.PageRequest) (domain.Transcript, error) {
	t, err := s.deps.History.Messages(ctx, userID, convID, page)
	s.note(ctx, "reading a conversation", err)
	return t, err
}

// note logs what went wrong with the history itself: a user's mistake (a page that is not one, a conversation that is not there) is
// not a fault of the service, and neither is a caller that left.
func (s *Service) note(ctx context.Context, what string, err error) {
	if err != nil && errors.Is(err, domain.ErrHistoryUnavailable) && ctx.Err() == nil {
		s.deps.Logger.WarnContext(ctx, "the history could not be read: "+what, "error", err)
	}
}

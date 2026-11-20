package chat

import (
	"context"
	"errors"
	"time"
	"uuid"

	"iape-ui/internal/core/domain"
)

// stopWait is how long Stop waits for a canceled run to keep its reply. A
// browser that stopped reading can hold a run up for a moment; Stop does not
// wait on it forever.
const stopWait = 3 * time.Second

// stopAttempts bounds the look-again loop in Stop. A second pass is needed only
// when a stream starts at the very moment of the stop.
const stopAttempts = 3

// Stop implements service.Chat.
//
// Pattern: Cancel, Wait, Read Back
func (s *Service) Stop(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error) {
	for range stopAttempts {
		msg, err := s.deps.Live.Get(userID, convID, msgID)
		if err != nil {
			return domain.Message{}, err
		}
		if msg.Status.Terminal() {
			return msg, nil
		}

		if active, ok := s.runs.get(msgID); ok {
			active.cancel()
			select {
			case <-active.done:
			case <-time.After(stopWait):
				s.deps.Logger.WarnContext(ctx, "a stopped reply was slow to be kept", "message_id", msgID)
			case <-ctx.Done():
				return domain.Message{}, ctx.Err()
			}
			continue // read the kept state
		}

		if msg.Status != domain.StatusPending {
			// Streaming, yet nothing here is writing it.
			return s.endLost(userID, msg)
		}
		// Nobody has started this reply. Mark it stopped so it never runs.
		err = s.stopUnstarted(userID, convID, msgID)
		if err != nil && !errors.Is(err, domain.ErrAlreadyStreaming) && !errors.Is(err, domain.ErrInvalidState) {
			return domain.Message{}, err
		}
		// Either way, read the result: a stream that won the race is a run to stop,
		// and a reply that ended in the meantime is returned as it is kept.
	}
	return s.deps.Live.Get(userID, convID, msgID)
}

// stopUnstarted moves a reply that no one has started to stopped. It registers as the reply's writer first, as Stream does: a stream
// that arrives in between is told that the reply is being written. It returns domain.ErrAlreadyStreaming when somebody else was
// writing the reply first, and domain.ErrInvalidState when the reply moved on before it could be stopped.
func (s *Service) stopUnstarted(userID uuid.UUID, convID string, msgID uuid.UUID) error {
	active, ok := s.runs.begin(msgID, func() {}) // there is no run to cancel
	if !ok {
		return domain.ErrAlreadyStreaming
	}
	defer s.runs.end(msgID, active)

	_, err := s.deps.Live.Update(userID, convID, msgID, func(m domain.Message) (domain.Message, error) {
		if m.Status != domain.StatusPending {
			return m, domain.ErrInvalidState
		}
		m.Status, m.Prompt = domain.StatusStopped, ""
		return m, nil
	})
	return err
}

// endLost ends a reply that is streaming while nothing here is writing it: the run was lost and nothing will ever finish it. The
// page would wait for it for good and no other turn could start. The reply is failed, with the text it has. If it finished by itself
// in the meantime, that is what comes back: a finished reply is never overwritten.
//
// Pattern: Lost-Run Repair
func (s *Service) endLost(userID uuid.UUID, msg domain.Message) (domain.Message, error) {
	ended := false
	got, err := s.deps.Live.Update(userID, msg.ConversationID, msg.ID, func(m domain.Message) (domain.Message, error) {
		if m.Status.Terminal() {
			return m, nil
		}
		ended = true
		if m.Content == "" {
			m.Content = msg.Content
		}
		m.Status, m.Error, m.Prompt = domain.StatusFailed, domain.FailureInterrupted, ""
		return m, nil
	})
	if err != nil {
		return domain.Message{}, err
	}
	if ended {
		s.deps.Logger.Warn("ended a reply that nothing was writing", "message_id", msg.ID)
	}
	return got, nil
}

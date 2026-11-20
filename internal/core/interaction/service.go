// Package interaction implements the use case of answering the cards in the agent's replies: pay or reject a transfer and give
// the code the bank asks for.
package interaction

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
)

// saveTimeout bounds the saving of an answer after the request is gone: the service has acted, so the card must say so.
const saveTimeout = 5 * time.Second

// maxCodeLen is the longest code the Web UI sends on. The banks' codes are 4 to 8 digits.
const maxCodeLen = 12

// Dependencies are what the service needs.
type Dependencies struct {
	Messages resource.MessageStore
	Actions  resource.ActionGateway
	Logger   *slog.Logger
}

// Service implements service.Interactions.
type Service struct{ deps Dependencies }

var _ service.Interactions = (*Service)(nil)

// NewService returns the service.
func NewService(d Dependencies) *Service { return &Service{deps: d} }

// Approve implements service.Interactions.
func (s *Service) Approve(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	return s.answer(ctx, userID, convID, msgID, func(card domain.Interaction) bool { return card.State == domain.InteractionPending },
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			res, err := s.deps.Actions.Approve(ctx, card)
			if err != nil {
				return card, err
			}
			return card.Approved(res.Message, res.ChallengeID)
		})
}

// Reject implements service.Interactions.
func (s *Service) Reject(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	return s.answer(ctx, userID, convID, msgID, domain.Interaction.Open,
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			res, err := s.deps.Actions.Reject(ctx, card)
			if err != nil {
				return card, err
			}
			return card.Rejected(res.Message)
		})
}

// ConfirmOTP implements service.Interactions.
func (s *Service) ConfirmOTP(ctx context.Context, userID, convID, msgID uuid.UUID, code string) (domain.Message, error) {
	code = strings.TrimSpace(code)
	return s.answer(ctx, userID, convID, msgID, func(card domain.Interaction) bool { return card.State == domain.InteractionOTPRequired },
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			if code == "" || len(code) > maxCodeLen || strings.ContainsFunc(code, func(r rune) bool { return r == ' ' || r == '\t' }) {
				return card, &domain.ActionError{Message: "Enter the code the bank sent you."}
			}
			res, err := s.deps.Actions.ConfirmOTP(ctx, card, code)
			if err != nil {
				return card, err
			}
			return card.CodeConfirmed(res.Message)
		})
}

// ChooseContact implements service.Interactions.
func (s *Service) ChooseContact(ctx context.Context, userID, convID, msgID uuid.UUID, choice int) (domain.Message, error) {
	return s.answer(ctx, userID, convID, msgID,
		func(card domain.Interaction) bool {
			return card.Kind == domain.InteractionContact && card.State == domain.InteractionPending
		},
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			contact, err := s.confirm(ctx, card.Query, choice)
			if err != nil {
				return card, err
			}
			return card.ContactChosen(contact)
		})
}

// ConfirmCandidate implements service.Interactions. As the BFF's own page does for its picker, the service is told the chosen
// name: what the user typed after the @ may be nothing, or part of a name.
func (s *Service) ConfirmCandidate(ctx context.Context, query string, choice int) (domain.Contact, error) {
	candidates := domain.Candidates(query)
	if choice < 0 || choice >= len(candidates) {
		return domain.Contact{}, &domain.ActionError{Message: "Choose one of the contacts."}
	}
	chosen := candidates[choice]
	if err := s.deps.Actions.ConfirmContact(ctx, chosen.Name, chosen); err != nil {
		return domain.Contact{}, err
	}
	return chosen, nil
}

// confirm tells the agent's service whom the user chose among the candidates for the reference the agent gave.
func (s *Service) confirm(ctx context.Context, reference string, choice int) (domain.Contact, error) {
	candidates := domain.Candidates(reference)
	if choice < 0 || choice >= len(candidates) {
		return domain.Contact{}, &domain.ActionError{Message: "Choose one of the contacts."}
	}
	if err := s.deps.Actions.ConfirmContact(ctx, reference, candidates[choice]); err != nil {
		return domain.Contact{}, err
	}
	return candidates[choice], nil
}

// answer is what the three answers share: load the card, check it takes the answer, let the service act, and save the card in
// its new state only if it is still in the state that was read (the compare-and-set). An action that did not go through (a refusal, the agent down) returns the
// reply with the card as it was, along with the error.
func (s *Service) answer(
	ctx context.Context, userID, convID, msgID uuid.UUID,
	accepts func(domain.Interaction) bool,
	act func(context.Context, domain.Interaction) (domain.Interaction, error),
) (domain.Message, error) {
	msg, err := s.deps.Messages.Get(ctx, userID, convID, msgID)
	if err != nil {
		return domain.Message{}, err
	}
	card := msg.Meta.Interaction
	if msg.Role != domain.RoleAssistant || card == nil || !accepts(*card) {
		return domain.Message{}, fmt.Errorf("the message has no card that takes this answer: %w", domain.ErrInvalidState)
	}

	next, err := act(ctx, *card)
	if err != nil {
		return msg, err // the card is as it was: the page can show it again with the reason
	}

	// The service has acted: save the answer even if the request is gone.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()
	if err := s.deps.Messages.SaveInteraction(saveCtx, userID, convID, msgID, card.State, next); err != nil {
		s.deps.Logger.ErrorContext(ctx, "the service answered a card but its answer was not saved",
			"approval_id", card.ApprovalID, "from", card.State, "to", next.State, "error", err)
		return domain.Message{}, err
	}
	msg.Meta.Interaction = &next
	return msg, nil
}

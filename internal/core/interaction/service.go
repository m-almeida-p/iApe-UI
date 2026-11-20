// Package interaction implements the use case of answering the cards in the agent's replies: pay or reject a transfer and give
// the code the bank asks for.
package interaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
)

// maxCodeLen is the longest code the Web UI sends on, in characters. The banks' codes are 4 to 8 digits.
const maxCodeLen = 12

// plausibleCode reports whether code can be a code the bank sent: some text, no longer than maxCodeLen characters, with no
// space or control character in it. It is not checked for digits: the bank says what a code is.
func plausibleCode(code string) bool {
	return code != "" && utf8.ValidString(code) && utf8.RuneCountInString(code) <= maxCodeLen &&
		!strings.ContainsFunc(code, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// Dependencies are what the service needs. A nil Logger is the default logger.
type Dependencies struct {
	Live    resource.LiveStore
	Actions resource.ActionGateway
	Logger  *slog.Logger
}

// Service implements service.Interactions.
type Service struct{ deps Dependencies }

var _ service.Interactions = (*Service)(nil)

// NewService returns the service.
func NewService(d Dependencies) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{deps: d}
}

// Approve implements service.Interactions.
func (s *Service) Approve(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error) {
	return s.answer(ctx, userID, convID, msgID, func(card domain.Interaction) bool {
		return card.Kind == domain.InteractionApproval && card.State == domain.InteractionPending
	}, nil,
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			res, err := s.deps.Actions.Approve(ctx, userID, card)
			if err != nil {
				return resolvedAlready(card, err)
			}
			return card.Approved(res.Message, res.ChallengeID)
		})
}

// Reject implements service.Interactions.
func (s *Service) Reject(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID) (domain.Message, error) {
	return s.answer(ctx, userID, convID, msgID,
		func(card domain.Interaction) bool { return card.Kind == domain.InteractionApproval && card.Open() }, nil,
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			res, err := s.deps.Actions.Reject(ctx, userID, card)
			if err != nil {
				return resolvedAlready(card, err)
			}
			return card.Rejected(res.Message)
		})
}

// ConfirmOTP implements service.Interactions.
func (s *Service) ConfirmOTP(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID, code string) (domain.Message, error) {
	code = strings.TrimSpace(code)
	return s.answer(ctx, userID, convID, msgID, func(card domain.Interaction) bool {
		return card.Kind == domain.InteractionApproval && card.State == domain.InteractionOTPRequired
	},
		func(domain.Interaction) error {
			if !plausibleCode(code) {
				return &domain.ActionError{Message: "Enter the code the bank sent you."}
			}
			return nil
		},
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			res, err := s.deps.Actions.ConfirmOTP(ctx, userID, card, code)
			if err != nil {
				return card, err
			}
			return card.CodeConfirmed(res.Message)
		})
}

// ChooseContact implements service.Interactions.
func (s *Service) ChooseContact(ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID, choice int) (domain.Message, error) {
	return s.answer(ctx, userID, convID, msgID,
		func(card domain.Interaction) bool {
			return card.Kind == domain.InteractionContact && card.State == domain.InteractionPending
		},
		func(card domain.Interaction) error {
			_, err := candidateAt(card.Query, choice)
			return err
		},
		func(ctx context.Context, card domain.Interaction) (domain.Interaction, error) {
			chosen, err := candidateAt(card.Query, choice)
			if err != nil {
				return card, err
			}
			if err := s.deps.Actions.ConfirmContact(ctx, userID, card.Query, chosen); err != nil {
				return card, err
			}
			return card.ContactChosen(chosen)
		})
}

// ConfirmCandidate implements service.Interactions. As the BFF's own page does for its picker, the service is told the chosen
// name: what the user typed after the @ may be nothing, or part of a name.
func (s *Service) ConfirmCandidate(ctx context.Context, userID uuid.UUID, query string, choice int) (domain.Contact, error) {
	chosen, err := candidateAt(query, choice)
	if err != nil {
		return domain.Contact{}, err
	}
	if err := s.deps.Actions.ConfirmContact(ctx, userID, chosen.Name, chosen); err != nil {
		return domain.Contact{}, err
	}
	return chosen, nil
}

// candidateAt is the candidate the user chose among the ones for query, or the refusal that tells them to choose again.
func candidateAt(query string, choice int) (domain.Contact, error) {
	candidates := domain.Candidates(query)
	if choice < 0 || choice >= len(candidates) {
		return domain.Contact{}, &domain.ActionError{Message: "Choose one of the contacts."}
	}
	return candidates[choice], nil
}

// answer is what the answers share. The service of the agent is asked with the card as it is, and:
//   - it acted: the card is kept in its new state;
//   - it said no, or could not be reached, or did not confirm: the card is as it was, with the reason, and can be answered again.
//     That is safe whatever happened over there: the service ignores a repeat of an answer, so an answer that did go through finds out
//     what became of it (see resolvedAlready) instead of being done twice. It is also what keeps two answers to the same card (a
//     double tap, a second tab) from paying twice: this server does not hold the card while it asks, the service decides.
//
// accepts says which cards take the answer; check (it may be nil) refuses an answer that cannot be right before the service is asked.
func (s *Service) answer(
	ctx context.Context, userID uuid.UUID, convID string, msgID uuid.UUID,
	accepts func(domain.Interaction) bool,
	check func(domain.Interaction) error,
	act func(context.Context, domain.Interaction) (domain.Interaction, error),
) (domain.Message, error) {
	msg, err := s.deps.Live.Get(userID, convID, msgID)
	if err != nil {
		return domain.Message{}, err
	}
	card := msg.Meta.Interaction
	// Only a finished reply has a card that can be answered. A reply that failed or was stopped keeps its text, and its card is not
	// for paying.
	if msg.Role != domain.RoleAssistant || msg.Status != domain.StatusComplete || card == nil || !accepts(*card) {
		return domain.Message{}, fmt.Errorf("the message has no card that takes this answer: %w", domain.ErrInvalidState)
	}
	if check != nil {
		if err := check(*card); err != nil {
			return msg, err // a mistake of the user is not worth asking the service
		}
	}

	next, err := act(ctx, *card)
	if err != nil {
		return msg, err // the card is as it was
	}

	// The service has acted: the card says so. If another answer moved the card in the meantime, that one was first and stays.
	kept, err := s.deps.Live.Update(userID, convID, msgID, func(m domain.Message) (domain.Message, error) {
		if m.Meta.Interaction == nil || m.Meta.Interaction.State != card.State {
			return m, nil
		}
		m.Meta.Interaction = &next
		return m, nil
	})
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "the service answered a card but the reply was gone",
			"approval_id", card.ApprovalID, "from", card.State, "to", next.State, "error", err)
		msg.Meta.Interaction = &next
		return msg, nil // the answer was made: the page shows it, once
	}
	return kept, nil
}

// resolvedAlready is what an answer to a transfer becomes when the service says the approval was answered already: an answer that was
// sent twice, or whose reply was lost, finds out here what became of it, and the card says so instead of asking again. When the
// card cannot become that (it expired, or it waits for a code the approval needs), err is returned as it came.
func resolvedAlready(card domain.Interaction, err error) (domain.Interaction, error) {
	refused, ok := errors.AsType[*domain.ActionError](err)
	if !ok {
		return card, err
	}
	var next domain.Interaction
	var transition error
	switch strings.ToUpper(refused.Resolved) {
	case "APPROVED":
		next, transition = card.Approved("This transfer was already approved. Its result is in your history.", "")
	case "REJECTED":
		next, transition = card.Rejected("This transfer was already rejected.")
	default:
		return card, err
	}
	if transition != nil {
		return card, err
	}
	return next, nil
}

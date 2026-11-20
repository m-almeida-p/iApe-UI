// Package interaction implements the use case of answering the cards in the agent's replies: pay or reject a transfer and give
// the code the bank asks for.
package interaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
)

// saveTimeout bounds the saving of an answer after the request is gone: the service has acted, so the card must say so.
const saveTimeout = 5 * time.Second

// maxCodeLen is the longest code the Web UI sends on, in characters. The banks' codes are 4 to 8 digits.
const maxCodeLen = 12

// plausibleCode reports whether code can be a code the bank sent: some text, no longer than maxCodeLen characters, with no
// space or control character in it. It is not checked for digits: the bank says what a code is.
func plausibleCode(code string) bool {
	return code != "" && utf8.ValidString(code) && utf8.RuneCountInString(code) <= maxCodeLen &&
		!strings.ContainsFunc(code, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// Dependencies are what the service needs. Logger, Now and NewClaimID default to the default logger, the clock and a new id each; the
// last two are there for tests.
type Dependencies struct {
	Messages   resource.MessageStore
	Actions    resource.ActionGateway
	Logger     *slog.Logger
	Now        func() time.Time
	NewClaimID func() string
}

// Service implements service.Interactions.
type Service struct{ deps Dependencies }

var _ service.Interactions = (*Service)(nil)

// NewService returns the service.
func NewService(d Dependencies) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewClaimID == nil {
		d.NewClaimID = func() string { return uuid.NewV7().String() }
	}
	return &Service{deps: d}
}

// Approve implements service.Interactions.
func (s *Service) Approve(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
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
func (s *Service) Reject(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
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
func (s *Service) ConfirmOTP(ctx context.Context, userID, convID, msgID uuid.UUID, code string) (domain.Message, error) {
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
func (s *Service) ChooseContact(ctx context.Context, userID, convID, msgID uuid.UUID, choice int) (domain.Message, error) {
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

// answer is what the answers share. The card is claimed (saved as processing, with the compare-and-set) BEFORE the service is
// asked, so that of several answers to the same card (a double tap, a second tab, the retries of a phone's network) only the one that
// holds the claim reaches the service. Then:
//   - the service acted: the card is saved in its new state, from the claim;
//   - the service said no, or the request certainly never left (domain.ErrNotSent): nothing was done, so the claim is given back and
//     the card is as it was, with the reason;
//   - anything else (a timeout, a dropped connection, a gateway error): the service may have acted, so the card stays claimed, and
//     shows as being answered, until its claim goes stale (domain.ClaimTTL) and someone answers again.
//
// accepts says which cards take the answer; check (it may be nil) refuses an answer that cannot be right before anything is claimed.
func (s *Service) answer(
	ctx context.Context, userID, convID, msgID uuid.UUID,
	accepts func(domain.Interaction) bool,
	check func(domain.Interaction) error,
	act func(context.Context, domain.Interaction) (domain.Interaction, error),
) (domain.Message, error) {
	msg, err := s.deps.Messages.Get(ctx, userID, convID, msgID)
	if err != nil {
		return domain.Message{}, err
	}
	card := msg.Meta.Interaction
	now := s.deps.Now()
	// Only a finished reply has a card that can be answered. A reply that failed or was stopped keeps its text, and its card is not
	// for paying. A card that is being answered is not answered again, unless its claim went stale.
	if msg.Role != domain.RoleAssistant || msg.Status != domain.StatusComplete || card == nil ||
		(card.State == domain.InteractionProcessing && !card.Stale(now)) || !accepts(card.Base()) {
		return domain.Message{}, fmt.Errorf("the message has no card that takes this answer: %w", domain.ErrInvalidState)
	}
	base := card.Base()
	if check != nil {
		if err := check(base); err != nil {
			return msg, err // a mistake of the user is not worth a claim
		}
	}

	claim := card.Claim
	if card.State == domain.InteractionProcessing {
		claim = card.Reclaim
	}
	claimed, err := claim(s.deps.NewClaimID(), now)
	if err != nil {
		return domain.Message{}, err
	}
	if err := s.deps.Messages.SaveInteraction(ctx, userID, convID, msgID, *card, claimed); err != nil {
		return domain.Message{}, err // someone else claimed it first, or the store failed: the service is not asked
	}

	next, err := act(ctx, base)
	if err != nil {
		msg.Meta.Interaction = &claimed
		if !untouched(err) {
			return msg, err // it may have gone through: the card stays claimed
		}
		if releaseErr := s.save(ctx, userID, convID, msgID, claimed, base); releaseErr != nil {
			s.deps.Logger.ErrorContext(ctx, "a card was claimed, the service refused it, and the claim was not given back",
				"approval_id", base.ApprovalID, "error", releaseErr)
			return msg, err
		}
		msg.Meta.Interaction = &base
		return msg, err // the page shows the card again with the reason
	}

	// The service has acted: save the answer even if the request is gone.
	if err := s.save(ctx, userID, convID, msgID, claimed, next); err != nil {
		s.deps.Logger.ErrorContext(ctx, "the service answered a card but its answer was not saved",
			"approval_id", base.ApprovalID, "from", base.State, "to", next.State, "error", err)
		return domain.Message{}, err
	}
	msg.Meta.Interaction = &next
	return msg, nil
}

// save writes the card even if the request is gone: what the service did must be on the card.
func (s *Service) save(ctx context.Context, userID, convID, msgID uuid.UUID, from, card domain.Interaction) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()
	return s.deps.Messages.SaveInteraction(ctx, userID, convID, msgID, from, card)
}

// untouched reports whether a failed answer certainly did nothing at the service: it said no, or the request never reached it.
func untouched(err error) bool {
	_, refused := errors.AsType[*domain.ActionError](err)
	return refused || errors.Is(err, domain.ErrNotSent)
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

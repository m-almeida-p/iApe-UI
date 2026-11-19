package interaction_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/interaction"
	"iape-ui/internal/core/port/resource/mocks"
)

var (
	userID  = uuid.NewV7()
	convID  = uuid.NewV7()
	replyID = uuid.NewV7()
)

type harness struct {
	msgs    *mocks.MessageStore
	actions *mocks.ActionGateway
	logs    *bytes.Buffer
	svc     *interaction.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{msgs: mocks.NewMessageStore(t), actions: mocks.NewActionGateway(t), logs: &bytes.Buffer{}}
	h.svc = interaction.NewService(interaction.Dependencies{Messages: h.msgs, Actions: h.actions, Logger: slog.New(slog.NewTextHandler(h.logs, nil))})
	return h
}

func cardIn(state domain.InteractionState) *domain.Interaction {
	return &domain.Interaction{
		Kind: domain.InteractionApproval, State: state, ApprovalID: "appr_1", RunID: "run-1", ChallengeID: challengeFor(state),
		Transfer: domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz"},
	}
}

func challengeFor(state domain.InteractionState) string {
	if state == domain.InteractionOTPRequired {
		return "trf_9"
	}
	return ""
}

func replyWith(card *domain.Interaction) domain.Message {
	return domain.Message{ID: replyID, ConversationID: convID, Role: domain.RoleAssistant, Status: domain.StatusComplete,
		Content: "Review your transfer.", Meta: domain.Meta{Interaction: card}}
}

func (h *harness) expectGet(m domain.Message) {
	h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(m, nil).Once()
}

// expectSave expects the new card to be saved from state, with a context that is still usable.
func (h *harness) expectSave(from domain.InteractionState, want domain.Interaction, err error) {
	h.msgs.On("SaveInteraction",
		mock.MatchedBy(func(ctx context.Context) bool { return ctx.Err() == nil }),
		userID, convID, replyID, from, want).Return(err).Once()
}

func TestApprove(t *testing.T) {
	t.Run("a payment that needs no code leaves the card paid with the service's words", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Return(domain.ActionResult{Message: "Transfer paid successfully. TxnID: OP-1"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.Result = "Transfer paid successfully. TxnID: OP-1"
		h.expectSave(domain.InteractionPending, paid, nil)

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
		assert.Equal(t, "Transfer paid successfully. TxnID: OP-1", got.Meta.Interaction.Result)
		assert.Equal(t, "Review your transfer.", got.Content, "the reply's text is untouched")
	})

	t.Run("a payment the bank wants a code for leaves the card asking for it", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Return(domain.ActionResult{Message: "Enter the OTP", ChallengeID: "trf_9"}, nil).Once()
		asking := *cardIn(domain.InteractionOTPRequired)
		asking.Result = "Enter the OTP"
		h.expectSave(domain.InteractionPending, asking, nil)

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State)
		assert.Equal(t, "trf_9", got.Meta.Interaction.ChallengeID)
	})

	t.Run("a card that is not pending is refused before the service is asked", func(t *testing.T) {
		for _, state := range []domain.InteractionState{domain.InteractionPaid, domain.InteractionRejected, domain.InteractionOTPRequired} {
			h := newHarness(t)
			h.expectGet(replyWith(cardIn(state)))

			_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

			require.ErrorIs(t, err, domain.ErrInvalidState, string(state))
		}
	})

	t.Run("a reply with no card is refused", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(nil))

		_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("the user's message is not a card", func(t *testing.T) {
		h := newHarness(t)
		m := replyWith(cardIn(domain.InteractionPending))
		m.Role = domain.RoleUser
		h.expectGet(m)

		_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("a message that is not found is not found", func(t *testing.T) {
		h := newHarness(t)
		h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(domain.Message{}, domain.ErrNotFound).Once()

		_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a refusal of the service comes back with the card as it was", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		refusal := &domain.ActionError{Message: "Transfer is no longer open"}
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{}, refusal).Once()

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, "Transfer is no longer open", refused.Message)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State, "the page shows the card again, with the reason")
	})

	t.Run("the agent being down leaves the card as it was and says so", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{}, domain.ErrAgentUnavailable).Once()

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
	})

	t.Run("losing the race to save is an invalid state, and it is logged because the service did act", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{Message: "ok"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.Result = "ok"
		h.expectSave(domain.InteractionPending, paid, domain.ErrInvalidState)

		_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Contains(t, h.logs.String(), "appr_1")
	})

	t.Run("the answer is saved even when the request is gone", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyWith(cardIn(domain.InteractionPending)), nil).Once()
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Run(func(mock.Arguments) { cancel() }).Return(domain.ActionResult{Message: "ok"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.Result = "ok"
		h.expectSave(domain.InteractionPending, paid, nil)

		_, err := h.svc.Approve(ctx, userID, convID, replyID)

		require.NoError(t, err)
	})
}

func TestReject(t *testing.T) {
	t.Run("a pending card and one that asks for a code can be rejected", func(t *testing.T) {
		for _, state := range []domain.InteractionState{domain.InteractionPending, domain.InteractionOTPRequired} {
			h := newHarness(t)
			h.expectGet(replyWith(cardIn(state)))
			h.actions.On("Reject", mock.Anything, *cardIn(state)).Return(domain.ActionResult{Message: "Transfer rejected successfully."}, nil).Once()
			closed := *cardIn(domain.InteractionRejected)
			closed.ChallengeID = challengeFor(state)
			closed.Result = "Transfer rejected successfully."
			h.expectSave(state, closed, nil)

			got, err := h.svc.Reject(t.Context(), userID, convID, replyID)

			require.NoError(t, err, string(state))
			assert.Equal(t, domain.InteractionRejected, got.Meta.Interaction.State)
		}
	})

	t.Run("a paid card cannot be rejected", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPaid)))

		_, err := h.svc.Reject(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})
}

func TestConfirmOTP(t *testing.T) {
	asking := func() domain.Message { return replyWith(cardIn(domain.InteractionOTPRequired)) }

	t.Run("the right code pays the transfer", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(asking())
		h.actions.On("ConfirmOTP", mock.Anything, *cardIn(domain.InteractionOTPRequired), "123456").
			Return(domain.ActionResult{Message: "Transfer paid successfully. TxnID: OP-2"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.ChallengeID = "trf_9"
		paid.Result = "Transfer paid successfully. TxnID: OP-2"
		h.expectSave(domain.InteractionOTPRequired, paid, nil)

		got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, " 123456 ")

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State, "the code is trimmed")
	})

	t.Run("a wrong code keeps the card asking, with the reason", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(asking())
		h.actions.On("ConfirmOTP", mock.Anything, *cardIn(domain.InteractionOTPRequired), "000000").
			Return(domain.ActionResult{}, &domain.ActionError{Message: "OTP verification failed"}).Once()

		got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "000000")

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, "OTP verification failed", refused.Message)
		assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State)
	})

	t.Run("a code that cannot be one is refused without asking the service", func(t *testing.T) {
		for _, code := range []string{"", "   ", "12 34", "1234567890123"} {
			h := newHarness(t)
			h.expectGet(asking())

			got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, code)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused, "%q", code)
			assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State)
		}
	})

	t.Run("a card that did not ask for a code is refused", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))

		_, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "123456")

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("any other failure of the service is passed on", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(asking())
		boom := errors.New("boom")
		h.actions.On("ConfirmOTP", mock.Anything, *cardIn(domain.InteractionOTPRequired), "123456").Return(domain.ActionResult{}, boom).Once()

		_, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "123456")

		require.ErrorIs(t, err, boom)
	})
}

func contactReply(state domain.InteractionState) domain.Message {
	return replyWith(&domain.Interaction{Kind: domain.InteractionContact, State: state, RequestID: "req-1", RunID: "run-1", Query: "Maria"})
}

func TestChooseContact(t *testing.T) {
	maria := domain.Candidates("Maria")[0]

	t.Run("the chosen candidate is confirmed with the agent's service and the card closes", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(contactReply(domain.InteractionPending))
		h.actions.On("ConfirmContact", mock.Anything, "Maria", maria).Return(nil).Once()
		want := domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionConfirmed, RequestID: "req-1", RunID: "run-1", Query: "Maria", Contact: maria}
		h.expectSave(domain.InteractionPending, want, nil)

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionConfirmed, got.Meta.Interaction.State)
		assert.Equal(t, maria, got.Meta.Interaction.Contact)
	})

	t.Run("a choice that is not a candidate is refused without asking the service", func(t *testing.T) {
		for _, choice := range []int{-1, len(domain.Candidates("Maria")), 99} {
			h := newHarness(t)
			h.expectGet(contactReply(domain.InteractionPending))

			got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, choice)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused, "%d", choice)
			assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
		}
	})

	t.Run("a card that is closed, or is a transfer, takes no contact", func(t *testing.T) {
		for _, m := range []domain.Message{contactReply(domain.InteractionConfirmed), replyWith(cardIn(domain.InteractionPending))} {
			h := newHarness(t)
			h.expectGet(m)

			_, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

			require.ErrorIs(t, err, domain.ErrInvalidState)
		}
	})

	t.Run("a service that refuses leaves the card pending, with the reason", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(contactReply(domain.InteractionPending))
		h.actions.On("ConfirmContact", mock.Anything, "Maria", maria).Return(&domain.ActionError{Message: "no such contact"}).Once()

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
	})
}

func TestConfirmCandidate(t *testing.T) {
	t.Run("the contact is confirmed and given back for the message", func(t *testing.T) {
		h := newHarness(t)
		juan := domain.Candidates("")[0]
		h.actions.On("ConfirmContact", mock.Anything, "", juan).Return(nil).Once()

		got, err := h.svc.ConfirmCandidate(t.Context(), "", 0)

		require.NoError(t, err)
		assert.Equal(t, juan, got)
	})

	t.Run("a choice that is not a candidate is refused without asking the service", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.svc.ConfirmCandidate(t.Context(), "maria", 7)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
	})

	t.Run("a failure of the service is passed on", func(t *testing.T) {
		h := newHarness(t)
		h.actions.On("ConfirmContact", mock.Anything, "maria", domain.Candidates("maria")[0]).Return(domain.ErrAgentUnavailable).Once()

		_, err := h.svc.ConfirmCandidate(t.Context(), "maria", 0)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	})
}

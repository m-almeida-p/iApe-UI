package interaction_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/interaction"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/resource/mocks"
)

const convID = "conv_v1_11111111-1111-4111-8111-111111111111"

var (
	userID  = uuid.New()
	otherID = uuid.New()
	replyID = uuid.New()
)

// fakeLive holds replies for users; Update is atomic, as the real one is. The other calls of the port are not used here.
type fakeLive struct {
	resource.LiveStore
	mu      sync.Mutex
	replies map[uuid.UUID]domain.Message
	owner   uuid.UUID
}

func (f *fakeLive) Get(u uuid.UUID, conv string, id uuid.UUID) (domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.replies[id]
	if !ok || u != f.owner || conv != m.ConversationID {
		return domain.Message{}, domain.ErrNotFound
	}
	return m, nil
}

func (f *fakeLive) Update(u uuid.UUID, conv string, id uuid.UUID, change func(domain.Message) (domain.Message, error)) (domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.replies[id]
	if !ok || u != f.owner || conv != m.ConversationID {
		return domain.Message{}, domain.ErrNotFound
	}
	next, err := change(m)
	if err != nil {
		return m, err
	}
	f.replies[id] = next
	return next, nil
}

type harness struct {
	live    *fakeLive
	actions *mocks.ActionGateway
	logs    *bytes.Buffer
	svc     *interaction.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{live: &fakeLive{replies: map[uuid.UUID]domain.Message{}, owner: userID}, actions: mocks.NewActionGateway(t), logs: &bytes.Buffer{}}
	h.svc = interaction.NewService(interaction.Dependencies{Live: h.live, Actions: h.actions, Logger: slog.New(slog.NewTextHandler(h.logs, nil))})
	return h
}

// with puts a complete reply that holds card in the store.
func (h *harness) with(card *domain.Interaction) {
	h.live.replies[replyID] = domain.Message{ID: replyID, ConversationID: convID, Role: domain.RoleAssistant, Status: domain.StatusComplete,
		Content: "Review your transfer.", Meta: domain.Meta{Interaction: card}}
}

func (h *harness) card() *domain.Interaction { return h.live.replies[replyID].Meta.Interaction }

func transfer(state domain.InteractionState) *domain.Interaction {
	c := &domain.Interaction{Kind: domain.InteractionApproval, State: state, ApprovalID: "appr_1", RunID: "run-1",
		Transfer: domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz"}}
	if state == domain.InteractionOTPRequired {
		c.ChallengeID = "trf_9"
	}
	return c
}

func contact(state domain.InteractionState) *domain.Interaction {
	return &domain.Interaction{Kind: domain.InteractionContact, State: state, RequestID: "req_1", Query: "maria"}
}

// answers are the four ways of answering a card, each with the card it is made for.
var answers = map[string]func(h *harness) (domain.Message, error){
	"approve": func(h *harness) (domain.Message, error) {
		return h.svc.Approve(context.Background(), userID, convID, replyID)
	},
	"reject": func(h *harness) (domain.Message, error) {
		return h.svc.Reject(context.Background(), userID, convID, replyID)
	},
	"code": func(h *harness) (domain.Message, error) {
		return h.svc.ConfirmOTP(context.Background(), userID, convID, replyID, "123456")
	},
	"contact": func(h *harness) (domain.Message, error) {
		return h.svc.ChooseContact(context.Background(), userID, convID, replyID, 0)
	},
}

func TestApprove(t *testing.T) {
	t.Run("a payment that needs no code leaves the card paid with the service's words", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionPending))
		h.actions.On("Approve", mock.Anything, userID, *transfer(domain.InteractionPending)).Return(domain.ActionResult{Message: "Paid. TxnID: 1"}, nil).Once()

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
		assert.Equal(t, "Paid. TxnID: 1", got.Meta.Interaction.Result)
		assert.Equal(t, domain.InteractionPaid, h.card().State, "and the reply in the store says so")
	})

	t.Run("a payment the bank wants a code for leaves the card asking for it", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionPending))
		h.actions.On("Approve", mock.Anything, userID, mock.Anything).Return(domain.ActionResult{Message: "Enter the code.", ChallengeID: "trf_9"}, nil).Once()

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State)
		assert.Equal(t, "trf_9", got.Meta.Interaction.ChallengeID)
	})

	t.Run("a refusal of the service comes back with the card as it was", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionPending))
		refusal := &domain.ActionError{Message: "Insufficient funds"}
		h.actions.On("Approve", mock.Anything, userID, mock.Anything).Return(domain.ActionResult{}, refusal).Once()

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
		assert.Equal(t, domain.InteractionPending, h.card().State)
	})

	for name, failure := range map[string]error{
		"the agent being down":             fmt.Errorf("%w: refused", domain.ErrAgentUnavailable),
		"a service that did not confirm":   fmt.Errorf("%w: the BFF did not confirm the transfer", domain.ErrAgentUnavailable),
		"a request that ran out of time":   context.DeadlineExceeded,
		"anything else the service throws": errors.New("boom"),
	} {
		t.Run(name+" leaves the card as it was, to be answered again", func(t *testing.T) {
			h := newHarness(t)
			h.with(transfer(domain.InteractionPending))
			h.actions.On("Approve", mock.Anything, userID, mock.Anything).Return(domain.ActionResult{}, failure).Once()

			got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

			require.ErrorIs(t, err, failure)
			assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
			assert.Equal(t, domain.InteractionPending, h.card().State)
		})
	}
}

func TestApprove_AnAnswerThatWasMadeAlreadyIsWhatTheCardBecomes(t *testing.T) {
	approved := "This transfer was already approved. Its result is in your history."
	rejected := "This transfer was already rejected."
	tests := []struct {
		name     string
		answer   string
		resolved string
		state    domain.InteractionState
		result   string
	}{
		{"approve, it was approved", "approve", "APPROVED", domain.InteractionPaid, approved},
		{"approve, it was rejected", "approve", "REJECTED", domain.InteractionRejected, rejected},
		{"reject, it was rejected", "reject", "REJECTED", domain.InteractionRejected, rejected},
		{"reject, it was approved", "reject", "APPROVED", domain.InteractionPaid, approved},
		{"the case of the word does not matter", "approve", "approved", domain.InteractionPaid, approved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.with(transfer(domain.InteractionPending))
			method := map[string]string{"approve": "Approve", "reject": "Reject"}[tc.answer]
			h.actions.On(method, mock.Anything, userID, mock.Anything).Return(domain.ActionResult{}, &domain.ActionError{Message: "x", Resolved: tc.resolved}).Once()

			got, err := answers[tc.answer](h)

			require.NoError(t, err, "it is not a failure: the card says what became of it")
			assert.Equal(t, tc.state, got.Meta.Interaction.State)
			assert.Equal(t, tc.result, got.Meta.Interaction.Result)
		})
	}

	t.Run("a refusal that says nothing of the kind stays a refusal", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionPending))
		h.actions.On("Approve", mock.Anything, userID, mock.Anything).Return(domain.ActionResult{}, &domain.ActionError{Message: "expired", Resolved: "EXPIRED"}).Once()

		_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		var refused *domain.ActionError
		assert.ErrorAs(t, err, &refused)
		assert.Equal(t, domain.InteractionPending, h.card().State)
	})
}

// Several answers to one card at once: those that find the card open reach the service, which does one and says so to the others; those
// that come after the card was paid are told it is closed. The card ends as what was done, and nothing is done twice by this server.
func TestApprove_EightAtOnceEndWithOnePaymentAndTheCardPaid(t *testing.T) {
	h := newHarness(t)
	h.with(transfer(domain.InteractionPending))
	var paid, repeats sync.Mutex
	var payments, repeated int
	h.actions.On("Approve", mock.Anything, userID, mock.Anything).Return(func(context.Context, uuid.UUID, domain.Interaction) (domain.ActionResult, error) {
		paid.Lock()
		defer paid.Unlock()
		if payments == 0 {
			payments++
			return domain.ActionResult{Message: "Paid."}, nil
		}
		repeats.Lock()
		repeated++
		repeats.Unlock()
		return domain.ActionResult{}, &domain.ActionError{Message: "approval already resolved: APPROVED", Resolved: "APPROVED"}
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := h.svc.Approve(t.Context(), userID, convID, replyID)
			if errors.Is(err, domain.ErrInvalidState) {
				return // it came after the card was paid: the page is shown the reply as it is (see the card handler)
			}
			if assert.NoError(t, err) && assert.NotNil(t, got.Meta.Interaction) {
				assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
			}
		})
	}
	wg.Wait()

	assert.Equal(t, 1, payments, "one payment, whatever came at once")
	assert.LessOrEqual(t, repeated, 7)
	assert.Equal(t, domain.InteractionPaid, h.card().State)
	assert.Equal(t, "Paid.", h.card().Result, "the first answer is the one that stays")
}

func TestReject(t *testing.T) {
	for _, state := range []domain.InteractionState{domain.InteractionPending, domain.InteractionOTPRequired} {
		t.Run("a card that is "+string(state)+" can be rejected", func(t *testing.T) {
			h := newHarness(t)
			h.with(transfer(state))
			h.actions.On("Reject", mock.Anything, userID, *transfer(state)).Return(domain.ActionResult{Message: "Transfer rejected successfully."}, nil).Once()

			got, err := h.svc.Reject(t.Context(), userID, convID, replyID)

			require.NoError(t, err)
			assert.Equal(t, domain.InteractionRejected, got.Meta.Interaction.State)
			if state == domain.InteractionOTPRequired {
				assert.Equal(t, "trf_9", got.Meta.Interaction.ChallengeID, "the challenge stays on the rejected card")
			}
		})
	}
}

func TestConfirmOTP(t *testing.T) {
	t.Run("the right code pays the transfer", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionOTPRequired))
		h.actions.On("ConfirmOTP", mock.Anything, userID, *transfer(domain.InteractionOTPRequired), "123456").Return(domain.ActionResult{Message: "Paid."}, nil).Once()

		got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "123456")

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
	})

	t.Run("a wrong code keeps the card asking, with the service's reason", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionOTPRequired))
		h.actions.On("ConfirmOTP", mock.Anything, userID, mock.Anything, "000000").Return(domain.ActionResult{}, &domain.ActionError{Message: "Invalid code"}).Once()

		got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "000000")

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, "Invalid code", refused.Message)
		assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State)
	})

	t.Run("the ends of a code are trimmed before it goes", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionOTPRequired))
		h.actions.On("ConfirmOTP", mock.Anything, userID, mock.Anything, "123456").Return(domain.ActionResult{}, nil).Once()

		_, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "  123456\n")

		assert.NoError(t, err)
	})

	for name, code := range map[string]string{
		"empty": "", "spaces only": "   ", "a space inside": "12 34", "a tab inside": "12\t34", "a newline inside": "12\n34", "a NUL": "12\x0034",
		"too long": strings.Repeat("1", 13), "not UTF-8": "12\xff34", "a zero-width character": "12\u200b34", "a bidi override": "12\u202e34",
	} {
		t.Run("a code that cannot be one never leaves the Web UI: "+name, func(t *testing.T) {
			h := newHarness(t)
			h.with(transfer(domain.InteractionOTPRequired))

			got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, code)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, "Enter the code the bank sent you.", refused.Message)
			assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State)
		})
	}

	t.Run("a code of the longest length is sent", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionOTPRequired))
		h.actions.On("ConfirmOTP", mock.Anything, userID, mock.Anything, strings.Repeat("ñ", 12)).Return(domain.ActionResult{}, nil).Once()

		_, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, strings.Repeat("ñ", 12))

		assert.NoError(t, err)
	})
}

func TestChooseContact(t *testing.T) {
	t.Run("the chosen candidate is confirmed with the service and the card closes", func(t *testing.T) {
		h := newHarness(t)
		h.with(contact(domain.InteractionPending))
		want := domain.Candidates("maria")[0]
		h.actions.On("ConfirmContact", mock.Anything, userID, "maria", want).Return(nil).Once()

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionConfirmed, got.Meta.Interaction.State)
		assert.Equal(t, want, got.Meta.Interaction.Contact)
	})

	t.Run("every candidate can be chosen, the last one too", func(t *testing.T) {
		candidates := domain.Candidates("maria")
		for i, c := range candidates {
			h := newHarness(t)
			h.with(contact(domain.InteractionPending))
			h.actions.On("ConfirmContact", mock.Anything, userID, "maria", c).Return(nil).Once()

			got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, i)

			require.NoError(t, err)
			assert.Equal(t, c, got.Meta.Interaction.Contact)
		}
	})

	for _, choice := range []int{-1, 99, len(domain.Candidates("maria"))} {
		t.Run(fmt.Sprintf("a choice of %d is refused without asking the service", choice), func(t *testing.T) {
			h := newHarness(t)
			h.with(contact(domain.InteractionPending))

			got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, choice)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, "Choose one of the contacts.", refused.Message)
			assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
		})
	}

	t.Run("a service that refuses leaves the card pending, with the reason", func(t *testing.T) {
		h := newHarness(t)
		h.with(contact(domain.InteractionPending))
		h.actions.On("ConfirmContact", mock.Anything, userID, "maria", mock.Anything).Return(&domain.ActionError{Message: "no such contact"}).Once()

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.Error(t, err)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
	})
}

func TestAnswer_OnlyTheRightCardOnAFinishedReplyIsAnswered(t *testing.T) {
	cases := map[string]struct {
		card     *domain.Interaction
		status   domain.Status
		role     domain.Role
		answers  []string
		wantInOK bool
	}{
		"a paid card takes nothing":            {transfer(domain.InteractionPaid), domain.StatusComplete, domain.RoleAssistant, []string{"approve", "reject", "code", "contact"}, false},
		"a rejected card takes nothing":        {transfer(domain.InteractionRejected), domain.StatusComplete, domain.RoleAssistant, []string{"approve", "reject", "code", "contact"}, false},
		"a pending transfer takes no code":     {transfer(domain.InteractionPending), domain.StatusComplete, domain.RoleAssistant, []string{"code", "contact"}, false},
		"a transfer that asks a code only too": {transfer(domain.InteractionOTPRequired), domain.StatusComplete, domain.RoleAssistant, []string{"approve", "contact"}, false},
		"a contact card is not paid":           {contact(domain.InteractionPending), domain.StatusComplete, domain.RoleAssistant, []string{"approve", "reject", "code"}, false},
		"a confirmed contact takes nothing":    {contact(domain.InteractionConfirmed), domain.StatusComplete, domain.RoleAssistant, []string{"contact"}, false},
		"a card of an unknown kind":            {&domain.Interaction{Kind: "mystery", State: domain.InteractionPending}, domain.StatusComplete, domain.RoleAssistant, []string{"approve", "reject", "code", "contact"}, false},
		"a card of no kind":                    {&domain.Interaction{State: domain.InteractionPending}, domain.StatusComplete, domain.RoleAssistant, []string{"approve", "reject", "code", "contact"}, false},
		"a reply that failed":                  {transfer(domain.InteractionPending), domain.StatusFailed, domain.RoleAssistant, []string{"approve", "reject"}, false},
		"a reply that was stopped":             {transfer(domain.InteractionPending), domain.StatusStopped, domain.RoleAssistant, []string{"approve", "reject"}, false},
		"a reply still streaming":              {transfer(domain.InteractionPending), domain.StatusStreaming, domain.RoleAssistant, []string{"approve", "reject"}, false},
		"the user's own message":               {transfer(domain.InteractionPending), domain.StatusComplete, domain.RoleUser, []string{"approve", "reject"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, answer := range tc.answers {
				h := newHarness(t)
				h.with(tc.card)
				m := h.live.replies[replyID]
				m.Status, m.Role = tc.status, tc.role
				h.live.replies[replyID] = m

				_, err := answers[answer](h) // the service has no call expected: asking it would fail the test

				assert.ErrorIs(t, err, domain.ErrInvalidState, answer)
			}
		})
	}

	t.Run("a reply with no card", func(t *testing.T) {
		h := newHarness(t)
		h.with(nil)

		_, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		assert.ErrorIs(t, err, domain.ErrInvalidState)
	})
}

func TestAnswer_AReplyThatIsNotTheUsersIsNotFound(t *testing.T) {
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.with(transfer(domain.InteractionPending))
			h.live.owner = otherID

			_, err := answer(h)

			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
	}
	t.Run("a reply that is gone, or that is in another conversation", func(t *testing.T) {
		h := newHarness(t)
		h.with(transfer(domain.InteractionPending))

		_, err := h.svc.Approve(t.Context(), userID, "conv_v1_22222222-2222-4222-8222-222222222222", replyID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = h.svc.Approve(t.Context(), userID, convID, uuid.New())
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestApprove_AReplyForgottenWhileTheServiceAnsweredStillShowsTheAnswerOnce(t *testing.T) {
	h := newHarness(t)
	h.with(transfer(domain.InteractionPending))
	h.actions.On("Approve", mock.Anything, userID, mock.Anything).Run(func(mock.Arguments) {
		delete(h.live.replies, replyID) // the store forgot it while the service was answering
	}).Return(domain.ActionResult{Message: "Paid."}, nil).Once()

	got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

	require.NoError(t, err)
	assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
	assert.Contains(t, h.logs.String(), "the service answered a card but the reply was gone")
	assert.Contains(t, h.logs.String(), "appr_1", "which transfer")
}

func TestConfirmCandidate(t *testing.T) {
	t.Run("the contact is confirmed and given back for the message", func(t *testing.T) {
		h := newHarness(t)
		juan := domain.Candidates("")[0]
		h.actions.On("ConfirmContact", mock.Anything, userID, juan.Name, juan).Return(nil).Once()

		got, err := h.svc.ConfirmCandidate(t.Context(), userID, "", 0)

		require.NoError(t, err)
		assert.Equal(t, juan, got)
	})

	t.Run("a hostile query reaches the service only as the name of the candidate", func(t *testing.T) {
		h := newHarness(t)
		query := "'; DROP TABLE users;-- \n\x00"
		want := domain.Candidates(query)[0]
		h.actions.On("ConfirmContact", mock.Anything, userID, want.Name, want).Return(nil).Once()

		_, err := h.svc.ConfirmCandidate(t.Context(), userID, query, 0)

		assert.NoError(t, err)
	})

	t.Run("a choice that is not a candidate is refused without asking the service", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.svc.ConfirmCandidate(t.Context(), userID, "maria", 7)

		var refused *domain.ActionError
		assert.ErrorAs(t, err, &refused)
	})

	t.Run("a failure of the service is passed on, with no contact", func(t *testing.T) {
		h := newHarness(t)
		h.actions.On("ConfirmContact", mock.Anything, userID, mock.Anything, mock.Anything).Return(domain.ErrAgentUnavailable).Once()

		got, err := h.svc.ConfirmCandidate(t.Context(), userID, "maria", 0)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, domain.Contact{}, got)
	})
}

func TestNewService_HasALogger(t *testing.T) {
	assert.NotNil(t, interaction.NewService(interaction.Dependencies{}))
}

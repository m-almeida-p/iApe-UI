package interaction_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/interaction"
	"iape-ui/internal/core/port/resource"
)

func newService(messages resource.MessageStore, actions resource.ActionGateway) *interaction.Service {
	return interaction.NewService(interaction.Dependencies{Messages: messages, Actions: actions, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// A card takes the answer of its own kind, and only on a reply that is finished: the agent's service is asked first and the
// answer saved after, and the save is refused for a card of another kind or on a reply that did not complete, by which time
// the money would have moved.
func TestAnswer_OnlyTheRightCardOnAFinishedReplyIsAnswered(t *testing.T) {
	answers := map[string]func(*harness) error{
		"approve": func(h *harness) error { _, err := h.svc.Approve(t.Context(), userID, convID, replyID); return err },
		"reject":  func(h *harness) error { _, err := h.svc.Reject(t.Context(), userID, convID, replyID); return err },
		"code": func(h *harness) error {
			_, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "123456")
			return err
		},
		"contact": func(h *harness) error {
			_, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)
			return err
		},
	}

	t.Run("a contact card is neither paid, rejected nor given a code, and the service is not asked", func(t *testing.T) {
		for name, answer := range answers {
			if name == "contact" {
				continue
			}
			for _, state := range []domain.InteractionState{domain.InteractionPending, domain.InteractionOTPRequired} {
				h := newHarness(t)
				m := contactReply(state)
				h.expectGet(m)

				err := answer(h)

				require.ErrorIs(t, err, domain.ErrInvalidState, name+" "+string(state))
			}
		}
	})

	t.Run("a card of a kind this version does not know takes no answer, and the service is not asked", func(t *testing.T) {
		for name, answer := range answers {
			h := newHarness(t)
			card := cardIn(domain.InteractionPending)
			card.Kind = "survey"
			h.expectGet(replyWith(card))

			err := answer(h)

			require.ErrorIs(t, err, domain.ErrInvalidState, name)
		}
	})

	t.Run("a card with no kind takes no answer, and the service is not asked", func(t *testing.T) {
		for name, answer := range answers {
			h := newHarness(t)
			h.expectGet(replyWith(&domain.Interaction{State: domain.InteractionPending}))

			err := answer(h)

			require.ErrorIs(t, err, domain.ErrInvalidState, name)
		}
	})

	t.Run("an approval card is not given a contact", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))

		_, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("a reply that did not complete takes no answer, and the service is not asked", func(t *testing.T) {
		for _, status := range []domain.Status{domain.StatusPending, domain.StatusStreaming, domain.StatusStopped, domain.StatusFailed} {
			for name, answer := range answers {
				h := newHarness(t)
				state := domain.InteractionPending
				if name == "code" {
					state = domain.InteractionOTPRequired
				}
				m := replyWith(cardIn(state))
				if name == "contact" {
					m = contactReply(state)
				}
				m.Status = status
				h.expectGet(m)

				err := answer(h)

				require.ErrorIs(t, err, domain.ErrInvalidState, name+" on a "+string(status)+" reply")
			}
		}
	})
}

func TestApprove_TheServiceAndTheStoreAnswerOddly(t *testing.T) {
	t.Run("a service that says nothing pays the card with nothing to show", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{}, nil).Once()
		h.expectSave(domain.InteractionPending, *cardIn(domain.InteractionPaid), nil)

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, got.Meta.Interaction.State)
		assert.Empty(t, got.Meta.Interaction.Result)
	})

	t.Run("a store that fails after the service paid says so, logs which transfer, and returns no reply", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{Message: "ok"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.Result = "ok"
		boom := errors.New("connection reset by peer")
		h.expectSave(domain.InteractionPending, paid, boom)

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrInvalidState, "it is not a lost race")
		assert.Equal(t, domain.Message{}, got)
		assert.Contains(t, h.logs.String(), "appr_1")
		assert.Contains(t, h.logs.String(), "connection reset by peer")
		assert.Contains(t, h.logs.String(), "pending")
		assert.Contains(t, h.logs.String(), "paid")
	})

	t.Run("a reply that vanished before the save is not found", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{Message: "ok"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.Result = "ok"
		h.expectSave(domain.InteractionPending, paid, domain.ErrNotFound)

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrNotFound)
		assert.Equal(t, domain.Message{}, got)
		assert.Contains(t, h.logs.String(), "appr_1", "the service did act, so it is logged")
	})

	t.Run("a store that cannot read the reply fails before the service is asked", func(t *testing.T) {
		h := newHarness(t)
		boom := errors.New("db down")
		h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(domain.Message{}, boom).Once()

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.ErrorIs(t, err, boom)
		assert.Equal(t, domain.Message{}, got)
	})

	t.Run("the request ending while the agent is unreachable saves nothing", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Run(func(mock.Arguments) { cancel() }).Return(domain.ActionResult{}, domain.ErrAgentUnavailable).Once()

		got, err := h.svc.Approve(ctx, userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State, "the card is as it was")
		h.msgs.AssertNotCalled(t, "SaveInteraction")
	})

	t.Run("the request ending while the agent refuses saves nothing", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Run(func(mock.Arguments) { cancel() }).Return(domain.ActionResult{}, &domain.ActionError{Message: "closed"}).Once()

		_, err := h.svc.Approve(ctx, userID, convID, replyID)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		h.msgs.AssertNotCalled(t, "SaveInteraction")
	})

	t.Run("the card handed to the store is the one that was answered, and the reply is the one that was read", func(t *testing.T) {
		h := newHarness(t)
		read := replyWith(cardIn(domain.InteractionPending))
		read.Content = "Review <b>this</b>"
		h.expectGet(read)
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).Return(domain.ActionResult{Message: "ok"}, nil).Once()
		paid := *cardIn(domain.InteractionPaid)
		paid.Result = "ok"
		h.expectSave(domain.InteractionPending, paid, nil)

		got, err := h.svc.Approve(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, "Review <b>this</b>", got.Content)
		assert.Equal(t, replyID, got.ID)
		assert.Equal(t, domain.InteractionPending, read.Meta.Interaction.State, "the card that was read is not edited in place")
	})
}

// memoryStore is a message store with the compare-and-set of the real one, behind a lock.
type memoryStore struct {
	resource.MessageStore // the methods the service does not use

	mu    sync.Mutex
	msg   domain.Message
	saves int
}

func (s *memoryStore) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.msg
	card := *m.Meta.Interaction
	m.Meta.Interaction = &card
	return m, nil
}

func (s *memoryStore) SaveInteraction(_ context.Context, _, _, _ uuid.UUID, from domain.InteractionState, card domain.Interaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msg.Meta.Interaction.State != from {
		return domain.ErrInvalidState
	}
	s.msg.Meta.Interaction = &card
	s.saves++
	return nil
}

// gate is an action gateway that holds every caller until all of them have arrived, so that all of them have read the card
// as pending before any answer is saved.
type gate struct {
	resource.ActionGateway

	want    int32
	entered atomic.Int32
	calls   atomic.Int32
	open    chan struct{}
}

func (g *gate) Approve(context.Context, domain.Interaction) (domain.ActionResult, error) {
	g.calls.Add(1)
	if g.entered.Add(1) == g.want {
		close(g.open)
	}
	<-g.open
	return domain.ActionResult{Message: "paid"}, nil
}

// Two requests for the same card, from two tabs or two servers, both read it as pending and both ask the service: the
// service is idempotent about a transfer that is paid already (it is its own to refuse the second), so the Web UI does not
// lock around the call. What it guarantees is the save: one wins and the rest are told the card was answered.
func TestApprove_ManyAtOnceOneSaveWins(t *testing.T) {
	const callers = 50
	store := &memoryStore{msg: replyWith(cardIn(domain.InteractionPending))}
	actions := &gate{want: callers, open: make(chan struct{})}
	svc := newService(store, actions)

	var wg sync.WaitGroup
	var won, lost, other atomic.Int32
	for range callers {
		wg.Go(func() {
			_, err := svc.Approve(t.Context(), userID, convID, replyID)
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, domain.ErrInvalidState):
				lost.Add(1)
			default:
				other.Add(1)
			}
		})
	}
	wg.Wait()

	assert.Equal(t, int32(callers), actions.calls.Load(), "the service was asked by every caller that read the card as pending")
	assert.Equal(t, int32(1), won.Load(), "exactly one answer is saved")
	assert.Equal(t, int32(callers-1), lost.Load(), "the rest are told the card was answered")
	assert.Zero(t, other.Load())
	assert.Equal(t, 1, store.saves)
	assert.Equal(t, domain.InteractionPaid, store.msg.Meta.Interaction.State)
}

func TestConfirmOTP_WhatIsTakenForACode(t *testing.T) {
	asking := func() domain.Message { return replyWith(cardIn(domain.InteractionOTPRequired)) }

	// The service wants a non-empty code of at most twelve characters with no space or control character in it. It does not
	// check for digits: the bank decides what a code is.
	t.Run("codes that go to the service, as they are after the ends are trimmed", func(t *testing.T) {
		for _, tc := range []struct{ typed, sent string }{
			{"123456", "123456"},
			{" 123456\n", "123456"},
			{"\t123456 \r\n", "123456"},
			{" 123456 ", "123456"},
			{"1234", "1234"},
			{"1", "1"},
			{"123456789012", "123456789012"},
			{"abcdef", "abcdef"},
			{"ÁÉÍÓÚÑ", "ÁÉÍÓÚÑ"},
			{"１２３４５６", "１２３４５６"}, // fullwidth digits: six characters, and the bank says whether it takes them
			{"１２３４５６７８９０１２", "１２３４５６７８９０１２"},
			{"12-34", "12-34"},
		} {
			h := newHarness(t)
			h.expectGet(asking())
			h.actions.On("ConfirmOTP", mock.Anything, *cardIn(domain.InteractionOTPRequired), tc.sent).
				Return(domain.ActionResult{}, &domain.ActionError{Message: "refused by the bank"}).Once()

			_, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, tc.typed)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused, "%q", tc.typed)
			assert.Equal(t, "refused by the bank", refused.Message, "%q reached the service", tc.typed)
		}
	})

	t.Run("codes that never leave the Web UI", func(t *testing.T) {
		for name, code := range map[string]string{
			"empty":                    "",
			"spaces":                   "  \t\n ",
			"a space inside":           "123 456",
			"a tab inside":             "123\t456",
			"a newline inside":         "123\n456",
			"a no-break space inside":  "123 456",
			"a NUL":                    "123\x00456",
			"a NUL at the end":         "123456\x00",
			"a control character":      "123\x1b[31m",
			"a delete":                 "123\x7f456",
			"a right-to-left override": "123\u202e456",
			"bytes that are not UTF-8": "123\xff456",
			"thirteen characters":      "1234567890123",
			"thirteen wide characters": strings.Repeat("１", 13),
			"a hundred kilobytes":      strings.Repeat("1", 100<<10),
		} {
			h := newHarness(t)
			h.expectGet(asking())

			got, err := h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, code)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused, name)
			assert.Equal(t, "Enter the code the bank sent you.", refused.Message, name)
			assert.Equal(t, domain.InteractionOTPRequired, got.Meta.Interaction.State, name+": the card stays asking")
		}
	})

	t.Run("a refusal because of the code is not even logged as a failure to save", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(asking())

		_, _ = h.svc.ConfirmOTP(t.Context(), userID, convID, replyID, "12 34")

		assert.Empty(t, h.logs.String())
	})
}

func TestReject_AfterTheBankAskedForACode(t *testing.T) {
	t.Run("the challenge stays on the rejected card", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(replyWith(cardIn(domain.InteractionOTPRequired)))
		h.actions.On("Reject", mock.Anything, *cardIn(domain.InteractionOTPRequired)).Return(domain.ActionResult{Message: "Rejected"}, nil).Once()
		closed := *cardIn(domain.InteractionRejected)
		closed.ChallengeID = "trf_9"
		closed.Result = "Rejected"
		h.expectSave(domain.InteractionOTPRequired, closed, nil)

		got, err := h.svc.Reject(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, "trf_9", got.Meta.Interaction.ChallengeID)
	})

	t.Run("a rejected, paid or confirmed card is not rejected again", func(t *testing.T) {
		for _, state := range []domain.InteractionState{domain.InteractionRejected, domain.InteractionPaid, domain.InteractionConfirmed} {
			h := newHarness(t)
			h.expectGet(replyWith(cardIn(state)))

			_, err := h.svc.Reject(t.Context(), userID, convID, replyID)

			require.ErrorIs(t, err, domain.ErrInvalidState, string(state))
		}
	})
}

func TestChooseContact_TheChoiceMustBeOneOfTheCandidates(t *testing.T) {
	t.Run("a number that is no index is refused without asking the service", func(t *testing.T) {
		n := len(domain.Candidates("Maria"))
		for _, choice := range []int{-1, n, n + 1, math.MaxInt, math.MinInt} {
			h := newHarness(t)
			h.expectGet(contactReply(domain.InteractionPending))

			got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, choice)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused, "%d", choice)
			assert.Equal(t, "Choose one of the contacts.", refused.Message)
			assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
		}
	})

	t.Run("every candidate can be chosen, the last one too", func(t *testing.T) {
		for i, want := range domain.Candidates("") {
			h := newHarness(t)
			card := &domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionPending, RequestID: "r", RunID: "run-1"}
			h.expectGet(replyWith(card))
			h.actions.On("ConfirmContact", mock.Anything, "", want).Return(nil).Once()
			chosen := *card
			chosen.State, chosen.Contact = domain.InteractionConfirmed, want
			h.expectSave(domain.InteractionPending, chosen, nil)

			got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, i)

			require.NoError(t, err, "choice %d", i)
			assert.Equal(t, want, got.Meta.Interaction.Contact)
		}
	})

	t.Run("a card with no query offers the default contacts, and one past them is refused", func(t *testing.T) {
		h := newHarness(t)
		card := &domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionPending}
		h.expectGet(replyWith(card))

		_, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, len(domain.Candidates("")))

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
	})

	t.Run("the agent's own words are what the service is told it was about, and the contact comes from them", func(t *testing.T) {
		h := newHarness(t)
		const reference = "  '; DROP TABLE x;--  "
		card := &domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionPending, Query: reference}
		h.expectGet(replyWith(card))
		want := domain.Candidates(reference)[0]
		h.actions.On("ConfirmContact", mock.Anything, reference, want).Return(nil).Once()
		chosen := *card
		chosen.State, chosen.Contact = domain.InteractionConfirmed, want
		h.expectSave(domain.InteractionPending, chosen, nil)

		_, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.NoError(t, err)
	})

	t.Run("the agent being down leaves the card pending and nothing is saved", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(contactReply(domain.InteractionPending))
		h.actions.On("ConfirmContact", mock.Anything, "Maria", domain.Candidates("Maria")[0]).Return(domain.ErrAgentUnavailable).Once()

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
		h.msgs.AssertNotCalled(t, "SaveInteraction")
	})

	t.Run("a lost race to save is an invalid state and is logged", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(contactReply(domain.InteractionPending))
		maria := domain.Candidates("Maria")[0]
		h.actions.On("ConfirmContact", mock.Anything, "Maria", maria).Return(nil).Once()
		h.msgs.On("SaveInteraction", mock.Anything, userID, convID, replyID, domain.InteractionPending, mock.Anything).Return(domain.ErrInvalidState).Once()

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Equal(t, domain.Message{}, got)
		assert.NotEmpty(t, h.logs.String())
	})
}

func TestConfirmCandidate_TheServiceIsToldWhoWasChosen(t *testing.T) {
	t.Run("a hostile query reaches the service only as the name of the candidate", func(t *testing.T) {
		for _, query := range []string{
			"@(root)", "ana\x00rojas", "\u202eana", "'; DROP TABLE x;--", "<script>alert(1)</script>", "ana\nluis", strings.Repeat("ñ", 10_000), "@@",
		} {
			h := newHarness(t)
			want := domain.Candidates(query)[0]
			h.actions.On("ConfirmContact", mock.Anything, want.Name, want).Return(nil).Once()

			got, err := h.svc.ConfirmCandidate(t.Context(), query, 0)

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.NotContains(t, want.Name, "\x00")
			assert.NotContains(t, want.Name, "\n")
			assert.NotContains(t, want.Name, "@")
			assert.NotContains(t, want.Name, "(")
		}
	})

	t.Run("a choice that is no index is refused without asking the service", func(t *testing.T) {
		for _, choice := range []int{-1, 5, 6, math.MaxInt, math.MinInt} {
			h := newHarness(t)

			_, err := h.svc.ConfirmCandidate(t.Context(), "", choice)

			var refused *domain.ActionError
			require.ErrorAs(t, err, &refused, "%d", choice)
		}
	})

	t.Run("an empty query offers the defaults, five of them", func(t *testing.T) {
		h := newHarness(t)
		last := domain.Candidates("")[4]
		h.actions.On("ConfirmContact", mock.Anything, last.Name, last).Return(nil).Once()

		got, err := h.svc.ConfirmCandidate(t.Context(), "   ", 4)

		require.NoError(t, err)
		assert.Equal(t, "Luis Vargas", got.Name)
	})

	t.Run("a service that refuses gives back no contact", func(t *testing.T) {
		h := newHarness(t)
		h.actions.On("ConfirmContact", mock.Anything, mock.Anything, mock.Anything).Return(&domain.ActionError{Message: "no"}).Once()

		got, err := h.svc.ConfirmCandidate(t.Context(), "maria", 0)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, domain.Contact{}, got)
	})
}

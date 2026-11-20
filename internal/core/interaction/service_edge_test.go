package interaction_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

// errNotSent is a failure of the request before it left: the card is given back.
var errNotSent = fmt.Errorf("%w: %w: no VPN", domain.ErrAgentUnavailable, domain.ErrNotSent)

// A card takes the answer of its own kind, and only on a reply that is finished: the card is claimed before the agent's service
// is asked, and the claim is refused for a card of another kind or on a reply that did not complete, so the money never moves.
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

	t.Run("the request ending while the agent is unreachable still gives the claim back", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Run(func(mock.Arguments) { cancel() }).Return(domain.ActionResult{}, errNotSent).Once()
		h.expectRelease(domain.InteractionPending) // the claim is given back with a context that is alive, though the request is gone

		got, err := h.svc.Approve(ctx, userID, convID, replyID)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State, "the card is as it was")
	})

	t.Run("the request ending while the agent refuses still gives the claim back", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		h.expectGet(replyWith(cardIn(domain.InteractionPending)))
		h.actions.On("Approve", mock.Anything, *cardIn(domain.InteractionPending)).
			Run(func(mock.Arguments) { cancel() }).Return(domain.ActionResult{}, &domain.ActionError{Message: "closed"}).Once()
		h.expectRelease(domain.InteractionPending)

		got, err := h.svc.Approve(ctx, userID, convID, replyID)

		var refused *domain.ActionError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, *cardIn(domain.InteractionPending), *got.Meta.Interaction, "the card is the base one: pending, with no claim on it")
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
			h.expectRelease(domain.InteractionOTPRequired)

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
			h.expectSaveFrom(*card, chosen, nil)

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
		h.expectSaveFrom(*card, chosen, nil)

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionConfirmed, got.Meta.Interaction.State)
		assert.Equal(t, want, got.Meta.Interaction.Contact, "the contact comes from the agent's own words")
	})

	t.Run("the agent being down leaves the card pending and nothing is saved", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(contactReply(domain.InteractionPending))
		h.actions.On("ConfirmContact", mock.Anything, "Maria", domain.Candidates("Maria")[0]).Return(errNotSent).Once()
		h.expectReleaseOf(*contactReply(domain.InteractionPending).Meta.Interaction)

		got, err := h.svc.ChooseContact(t.Context(), userID, convID, replyID, 0)

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Equal(t, domain.InteractionPending, got.Meta.Interaction.State)
		h.msgs.AssertNumberOfCalls(t, "SaveInteraction", 2) // the claim, and the claim given back: nothing else was written
	})

	t.Run("a lost race to save is an invalid state and is logged", func(t *testing.T) {
		h := newHarness(t)
		h.expectGet(contactReply(domain.InteractionPending))
		maria := domain.Candidates("Maria")[0]
		h.actions.On("ConfirmContact", mock.Anything, "Maria", maria).Return(nil).Once()
		lost := *contactReply(domain.InteractionPending).Meta.Interaction
		lost.State, lost.Contact = domain.InteractionConfirmed, maria
		h.expectSaveFrom(*contactReply(domain.InteractionPending).Meta.Interaction, lost, domain.ErrInvalidState) // the claim was won, the finish lost to a takeover

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

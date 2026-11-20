package chat_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
)

func cardEvent(c domain.Interaction) domain.AgentEvent {
	return domain.AgentEvent{Kind: domain.AgentInteraction, Interaction: c}
}

func approvalCard(id string) domain.Interaction {
	return domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionPending, ApprovalID: id, RunID: "run-1",
		Transfer: domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz"}}
}

// streamed runs one reply through the service with the agent's events and says what was saved and what the page was told.
func streamed(t *testing.T, want resource.Outcome, items ...any) domain.Meta {
	t.Helper()
	h := newHarness(t)
	h.expectClaim()
	h.expectRun(script(nil, items...))
	h.expectFinish(want, nil)

	evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

	require.NoError(t, err)
	return lastOf(t, evs).Meta
}

func TestStream_TheCardOfAReplyThatCompleted(t *testing.T) {
	t.Run("a card that arrives after the text is final is still kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			card := approvalCard("appr_1")

			meta := streamed(t,
				resource.Outcome{Content: "Review your transfer.", Status: domain.StatusComplete, Meta: domain.Meta{Interaction: &card}},
				final("Review your transfer."), cardEvent(card))

			require.NotNil(t, meta.Interaction)
			assert.Equal(t, "appr_1", meta.Interaction.ApprovalID)
		})
	})

	t.Run("a card that comes before any text is kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			card := approvalCard("appr_1")

			meta := streamed(t,
				resource.Outcome{Content: "Review.", Status: domain.StatusComplete, Meta: domain.Meta{Interaction: &card}},
				cardEvent(card), final("Review."))

			require.NotNil(t, meta.Interaction)
		})
	})

	t.Run("the last of two cards wins even when it comes after the final text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			first, second := approvalCard("appr_1"), approvalCard("appr_2")

			meta := streamed(t,
				resource.Outcome{Content: "Review.", Status: domain.StatusComplete, Meta: domain.Meta{Interaction: &second}},
				cardEvent(first), final("Review."), cardEvent(second))

			assert.Equal(t, "appr_2", meta.Interaction.ApprovalID)
		})
	})

	t.Run("a contact card has no approval id and is kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			card := domain.Interaction{Kind: domain.InteractionContact, State: domain.InteractionPending, RequestID: "req-1", RunID: "run-1", Query: "Maria"}

			meta := streamed(t,
				resource.Outcome{Content: "Who?", Status: domain.StatusComplete, Meta: domain.Meta{Interaction: &card}},
				final("Who?"), cardEvent(card))

			require.NotNil(t, meta.Interaction)
			assert.Equal(t, "req-1", meta.Interaction.RequestID)
		})
	})

	// An event with no kind, or one this version does not know, is not a card the page can draw or the store can answer: it
	// is not kept, and it does not take the place of a good card that came before it.
	t.Run("an empty card is not kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			meta := streamed(t,
				resource.Outcome{Content: "Hi.", Status: domain.StatusComplete},
				cardEvent(domain.Interaction{}), final("Hi."))

			assert.Nil(t, meta.Interaction)
		})
	})

	t.Run("a card of a kind nobody knows is not kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			meta := streamed(t,
				resource.Outcome{Content: "Hi.", Status: domain.StatusComplete},
				cardEvent(domain.Interaction{Kind: "survey", State: domain.InteractionPending, ApprovalID: "x"}), final("Hi."))

			assert.Nil(t, meta.Interaction)
		})
	})

	t.Run("an empty card after a good one does not erase it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			card := approvalCard("appr_1")

			meta := streamed(t,
				resource.Outcome{Content: "Review.", Status: domain.StatusComplete, Meta: domain.Meta{Interaction: &card}},
				cardEvent(card), cardEvent(domain.Interaction{}), final("Review."))

			require.NotNil(t, meta.Interaction)
			assert.Equal(t, "appr_1", meta.Interaction.ApprovalID)
		})
	})
}

// A card can be paid. The agent's service has a transfer waiting for it, and the page pays it through the card; a reply that
// did not complete is not one the user can act on, so the card of such a reply is not kept, and the page cannot be made to
// offer a payment on a reply that says it failed.
func TestStream_ARepliesThatDidNotCompleteKeepsNoCard(t *testing.T) {
	card := approvalCard("appr_1")

	t.Run("the agent fails after the card", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			meta := streamed(t,
				resource.Outcome{Content: "par", Status: domain.StatusFailed, Error: domain.FailureIncomplete, Meta: domain.Meta{ToolCalls: []string{"request_transfer_approval"}}},
				tool("request_transfer_approval"), cardEvent(card), delta("par"), domain.ErrIncompleteStream)

			assert.Nil(t, meta.Interaction, "the page is not given a card to pay on a failed reply")
		})
	})

	t.Run("the agent is unreachable after the card", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			meta := streamed(t,
				resource.Outcome{Status: domain.StatusFailed, Error: domain.FailureUnavailable},
				cardEvent(card), domain.ErrAgentUnavailable)

			assert.Nil(t, meta.Interaction)
		})
	})

	t.Run("the agent ends with a card and no text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			meta := streamed(t,
				resource.Outcome{Status: domain.StatusFailed, Error: domain.FailureIncomplete},
				cardEvent(card))

			assert.Nil(t, meta.Interaction)
		})
	})

	t.Run("the reply grows too long after the card", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarnessWithReplyLimit(t, 5)
			h.expectClaim()
			h.expectRun(script(nil, cardEvent(card), delta("0123456789")))
			h.expectFinish(resource.Outcome{Content: "0123456789", Status: domain.StatusFailed, Error: domain.FailureTooLong}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Nil(t, lastOf(t, evs).Meta.Interaction)
		})
	})

	t.Run("the user stops the reply after the card", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil, cardEvent(card), delta("Hel"), delta("lo")))
			h.expectFinish(resource.Outcome{Content: "Hel", Status: domain.StatusStopped}, nil)

			for ev := range h.svc.Stream(t.Context(), userID, convID, replyID) {
				assert.Equal(t, "Hel", ev.Text)
				break
			}
		})
	})
}

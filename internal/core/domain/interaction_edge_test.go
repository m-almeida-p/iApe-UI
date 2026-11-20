package domain_test

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

func TestInteraction_StoredJSON(t *testing.T) {
	t.Run("members a later version added are ignored, so an older server reads a newer row", func(t *testing.T) {
		stored := `{"kind":"approval","state":"pending","approval_id":"appr_1","future":{"a":[1,2]},"transfer":{"amount":5,"tip":1}}`

		var card domain.Interaction
		require.NoError(t, json.Unmarshal([]byte(stored), &card))

		assert.Equal(t, domain.InteractionApproval, card.Kind)
		assert.Equal(t, "appr_1", card.ApprovalID)
		assert.InDelta(t, 5, card.Transfer.Amount, 0)
		assert.True(t, card.Open())
	})

	t.Run("a kind and a state this version does not know decode, and the card takes no answer", func(t *testing.T) {
		var card domain.Interaction
		require.NoError(t, json.Unmarshal([]byte(`{"kind":"survey","state":"snoozed","approval_id":"x"}`), &card))

		assert.False(t, card.Open(), "an unknown state is not waiting for the user")
		for name, move := range map[string]func() (domain.Interaction, error){
			"approve": func() (domain.Interaction, error) { return card.Approved("ok", "") },
			"reject":  func() (domain.Interaction, error) { return card.Rejected("no") },
			"code":    func() (domain.Interaction, error) { return card.CodeConfirmed("ok") },
			"contact": func() (domain.Interaction, error) { return card.ContactChosen(domain.Contact{Name: "A", Phone: "9"}) },
		} {
			got, err := move()
			require.ErrorIs(t, err, domain.ErrInvalidState, name)
			assert.Equal(t, card, got, name)
		}
	})

	t.Run("a known state on an unknown kind takes no answer either", func(t *testing.T) {
		card := domain.Interaction{Kind: "survey", State: domain.InteractionPending}

		_, err := card.Approved("ok", "")
		require.ErrorIs(t, err, domain.ErrInvalidState)
		_, err = card.Rejected("no")
		require.ErrorIs(t, err, domain.ErrInvalidState)
		_, err = card.ContactChosen(domain.Contact{Name: "A", Phone: "9"})
		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("the zero card is not open and takes no answer", func(t *testing.T) {
		var card domain.Interaction

		assert.False(t, card.Open())
		_, err := card.Approved("ok", "")
		require.ErrorIs(t, err, domain.ErrInvalidState)
		_, err = card.Rejected("no")
		require.ErrorIs(t, err, domain.ErrInvalidState)
		_, err = card.CodeConfirmed("ok")
		require.ErrorIs(t, err, domain.ErrInvalidState)
		_, err = card.ContactChosen(domain.Contact{Name: "A", Phone: "9"})
		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("a card with only its identity round trips without the optional members", func(t *testing.T) {
		card := domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionPending, ApprovalID: "a"}

		stored, err := json.Marshal(card)
		require.NoError(t, err)

		assert.JSONEq(t, `{"kind":"approval","state":"pending","approval_id":"a"}`, string(stored))
	})
}

func TestMeta_TheCardInTheStoredJSON(t *testing.T) {
	t.Run("a nil card is left out and a card is kept", func(t *testing.T) {
		withCard := approval()
		kept, err := json.Marshal(domain.Meta{Interaction: &withCard})
		require.NoError(t, err)
		left, err := json.Marshal(domain.Meta{Interaction: nil})
		require.NoError(t, err)

		assert.Contains(t, string(kept), `"interaction":{`)
		assert.JSONEq(t, `{}`, string(left))
	})

	t.Run("a null card reads as no card", func(t *testing.T) {
		var meta domain.Meta
		require.NoError(t, json.Unmarshal([]byte(`{"interaction":null}`), &meta))

		assert.Nil(t, meta.Interaction)
	})

	// The amount is for display, but a card that cannot be encoded cannot be saved: a NaN or an infinity does not reach
	// the database, the encoder says so instead of writing a broken row, and nothing panics.
	t.Run("an amount that is not a number cannot be stored, and says so without a panic", func(t *testing.T) {
		for _, amount := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			card := approval()
			card.Transfer.Amount = amount

			assert.NotPanics(t, func() {
				_, err := json.Marshal(domain.Meta{Interaction: &card})
				require.Error(t, err, "%v", amount)
			})
		}
	})

	t.Run("odd but finite amounts are stored as they are", func(t *testing.T) {
		for _, amount := range []float64{-5, 0.005, math.MaxFloat64, math.SmallestNonzeroFloat64, 1e21} {
			card := approval()
			card.Transfer.Amount = amount

			stored, err := json.Marshal(domain.Meta{Interaction: &card})
			require.NoError(t, err, "%v", amount)
			var back domain.Meta
			require.NoError(t, json.Unmarshal(stored, &back))

			assert.InDelta(t, amount, back.Interaction.Transfer.Amount, 0, "%v", amount)
		}
	})

	t.Run("a zero amount is left out and reads back as zero", func(t *testing.T) {
		card := approval()
		card.Transfer.Amount = 0

		stored, err := json.Marshal(card)
		require.NoError(t, err)
		var back domain.Interaction
		require.NoError(t, json.Unmarshal(stored, &back))

		assert.NotContains(t, string(stored), `"amount"`)
		assert.Zero(t, back.Transfer.Amount)
	})
}

func TestInteraction_TheEdgesOfEachMove(t *testing.T) {
	t.Run("an empty result and an empty challenge are allowed: the card is paid with nothing to say", func(t *testing.T) {
		paid, err := approval().Approved("", "")

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, paid.State)
		assert.Empty(t, paid.Result)
		assert.Empty(t, paid.ChallengeID)
	})

	t.Run("approving a card that asks for a code is refused: the code is what pays it", func(t *testing.T) {
		asking, err := approval().Approved("code?", "trf_1")
		require.NoError(t, err)

		got, err := asking.Approved("again", "trf_2")

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Equal(t, asking, got)
		assert.Equal(t, "trf_1", got.ChallengeID, "the challenge is not replaced")
	})

	t.Run("a card that is closed cannot be rejected, whichever way it closed", func(t *testing.T) {
		for _, state := range []domain.InteractionState{domain.InteractionPaid, domain.InteractionRejected, domain.InteractionConfirmed} {
			card := approval()
			card.State = state

			got, err := card.Rejected("no")

			require.ErrorIs(t, err, domain.ErrInvalidState, string(state))
			assert.Equal(t, card, got, string(state))
		}
	})

	t.Run("rejecting from a card that asks for a code keeps the challenge it had", func(t *testing.T) {
		asking, err := approval().Approved("code?", "trf_1")
		require.NoError(t, err)

		rejected, err := asking.Rejected("no")

		require.NoError(t, err)
		assert.Equal(t, "trf_1", rejected.ChallengeID)
		assert.Equal(t, "no", rejected.Result)
	})

	t.Run("a code confirmed with an empty result still pays and clears the earlier words", func(t *testing.T) {
		asking, err := approval().Approved("Enter the code", "trf_1")
		require.NoError(t, err)

		paid, err := asking.CodeConfirmed("")

		require.NoError(t, err)
		assert.Equal(t, domain.InteractionPaid, paid.State)
		assert.Empty(t, paid.Result)
	})

	t.Run("a code is no move for a closed card", func(t *testing.T) {
		for _, state := range []domain.InteractionState{domain.InteractionPaid, domain.InteractionRejected} {
			card := approval()
			card.State = state

			_, err := card.CodeConfirmed("ok")

			require.ErrorIs(t, err, domain.ErrInvalidState, string(state))
		}
	})

	t.Run("a contact card is not rejected, even though it is open", func(t *testing.T) {
		_, err := contactCard().Rejected("no")

		require.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("a refused answer names the kind and the state, and still matches the sentinel", func(t *testing.T) {
		card := approval()
		card.State = domain.InteractionPaid

		_, err := card.Approved("ok", "")

		require.ErrorIs(t, err, domain.ErrInvalidState)
		assert.Contains(t, err.Error(), "approval")
		assert.Contains(t, err.Error(), "paid")
	})
}

func TestInteraction_AMoveLeavesTheCardItWasMadeOnAlone(t *testing.T) {
	// Interaction is a value: the move returns the new card and the receiver is as it was.
	pending := approval()
	asking, err := approval().Approved("code?", "trf_1")
	require.NoError(t, err)
	contact := contactCard()

	moves := map[string]struct {
		from domain.Interaction
		do   func(domain.Interaction) (domain.Interaction, error)
	}{
		"approved":        {pending, func(i domain.Interaction) (domain.Interaction, error) { return i.Approved("ok", "") }},
		"approved (code)": {pending, func(i domain.Interaction) (domain.Interaction, error) { return i.Approved("ok", "trf") }},
		"rejected":        {pending, func(i domain.Interaction) (domain.Interaction, error) { return i.Rejected("no") }},
		"code confirmed":  {asking, func(i domain.Interaction) (domain.Interaction, error) { return i.CodeConfirmed("ok") }},
		"contact chosen": {contact, func(i domain.Interaction) (domain.Interaction, error) {
			return i.ContactChosen(domain.Contact{Name: "Maria Torres", Phone: "912345678"})
		}},
	}
	for name, mv := range moves {
		t.Run(name, func(t *testing.T) {
			before := mv.from

			after, err := mv.do(mv.from)

			require.NoError(t, err)
			assert.Equal(t, before, mv.from, "the receiver is not changed")
			assert.NotEqual(t, before.State, after.State)
		})
	}
}

func TestInteraction_AContactIsOnlyChosenWhole(t *testing.T) {
	tests := []struct {
		name    string
		contact domain.Contact
	}{
		{"no name", domain.Contact{Phone: "912345678"}},
		{"no phone", domain.Contact{Name: "Maria Torres"}},
		{"nothing at all", domain.Contact{}},
		{"a name of spaces", domain.Contact{Name: "  \t", Phone: "912345678"}},
		{"a phone of spaces", domain.Contact{Name: "Maria Torres", Phone: " "}},
	}
	for _, tc := range tests {
		t.Run(tc.name+" is refused and the card stays pending", func(t *testing.T) {
			card := contactCard()

			got, err := card.ContactChosen(tc.contact)

			require.ErrorIs(t, err, domain.ErrInvalidState)
			assert.Equal(t, card, got)
			assert.True(t, got.Open())
		})
	}
}

func TestActionError(t *testing.T) {
	t.Run("it says the service refused, with the service's words", func(t *testing.T) {
		err := &domain.ActionError{Message: "OTP verification failed"}

		assert.Equal(t, "action refused: OTP verification failed", err.Error())
	})

	t.Run("it is found through wrapping, and its words are the user's", func(t *testing.T) {
		wrapped := fmt.Errorf("confirming: %w", fmt.Errorf("gateway: %w", &domain.ActionError{Message: "wrong code"}))

		refused, ok := errors.AsType[*domain.ActionError](wrapped)

		require.True(t, ok)
		assert.Equal(t, "wrong code", refused.Message)
	})

	t.Run("it is not an invalid state, so a page does not call a wrong code a double answer", func(t *testing.T) {
		assert.NotErrorIs(t, &domain.ActionError{Message: "x"}, domain.ErrInvalidState)
	})

	t.Run("an empty message is still an error", func(t *testing.T) {
		assert.Equal(t, "action refused: ", (&domain.ActionError{}).Error())
	})
}

func TestAgentError_SaysItIsTheAgentsAndKeepsItsWordsForTheLogs(t *testing.T) {
	err := fmt.Errorf("turn failed: %w", &domain.AgentError{Detail: "scripted failure requested with #error"})

	got, ok := errors.AsType[*domain.AgentError](err)

	require.True(t, ok)
	assert.Equal(t, "scripted failure requested with #error", got.Detail)
	assert.Equal(t, "agent reported an error: scripted failure requested with #error", got.Error())
	assert.Equal(t, "agent reported an error: ", (&domain.AgentError{}).Error(), "an agent that said nothing still reads as one")
}

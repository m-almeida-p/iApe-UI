package domain_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

var at = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func TestClaim_AnAnswerableCardIsClaimedBeforeTheServiceIsAsked(t *testing.T) {
	for _, state := range []domain.InteractionState{domain.InteractionPending, domain.InteractionOTPRequired} {
		card := approval()
		card.State = state

		claimed, err := card.Claim("claim-1", at)

		require.NoError(t, err, string(state))
		assert.Equal(t, domain.InteractionProcessing, claimed.State)
		assert.Equal(t, state, claimed.Resume, "it remembers what it was")
		assert.Equal(t, "claim-1", claimed.ClaimID)
		assert.Equal(t, at, claimed.ClaimedAt)
		assert.Equal(t, card.Transfer, claimed.Transfer, "the rest of the card is as it was")
		assert.Equal(t, card.ApprovalID, claimed.ApprovalID)
		assert.False(t, claimed.Open(), "nobody can answer a card that is being answered")
	}
}

func TestClaim_ACardThatCannotBeAnsweredCannotBeClaimed(t *testing.T) {
	for _, state := range []domain.InteractionState{domain.InteractionPaid, domain.InteractionRejected, domain.InteractionConfirmed, domain.InteractionProcessing, ""} {
		card := approval()
		card.State = state

		got, err := card.Claim("claim-1", at)

		require.ErrorIs(t, err, domain.ErrInvalidState, "state %q", state)
		assert.Equal(t, card, got, "a refused claim changes nothing")
	}
}

func TestClaim_AClaimNeedsAnIdentity(t *testing.T) {
	_, err := approval().Claim("", at)

	require.ErrorIs(t, err, domain.ErrInvalidState, "without an id nobody could tell its claim from another's")
}

func TestClaim_ReleasedGivesTheCardBackAsItWas(t *testing.T) {
	card := approval()
	card.State, card.ChallengeID, card.Result = domain.InteractionOTPRequired, "trf_9", "Enter the code"
	claimed, err := card.Claim("claim-1", at)
	require.NoError(t, err)

	assert.Equal(t, card, claimed.Base(), "the claim leaves no mark once it is let go")
	assert.Equal(t, card, card.Base(), "a card that was never claimed is its own base")
}

func TestClaim_ItGoesStaleAfterTheTimeAnswersAreGiven(t *testing.T) {
	claimed, err := approval().Claim("claim-1", at)
	require.NoError(t, err)

	assert.False(t, claimed.Stale(at), "just claimed")
	assert.False(t, claimed.Stale(at.Add(domain.ClaimTTL-time.Nanosecond)))
	assert.True(t, claimed.Stale(at.Add(domain.ClaimTTL)), "the time is up")
	assert.True(t, claimed.Stale(at.Add(24*time.Hour)))
	assert.False(t, claimed.Stale(at.Add(-time.Hour)), "a clock that went back does not free a claim")
	assert.False(t, approval().Stale(at.Add(time.Hour)), "only a claim can be stale")
}

func TestClaim_AStaleClaimCanBeTakenOverAndOnlyThen(t *testing.T) {
	claimed, err := approval().Claim("claim-1", at)
	require.NoError(t, err)

	_, err = claimed.Reclaim("claim-2", at.Add(time.Minute))
	require.ErrorIs(t, err, domain.ErrInvalidState, "while it is being answered it is not taken")

	taken, err := claimed.Reclaim("claim-2", at.Add(domain.ClaimTTL))
	require.NoError(t, err)
	assert.Equal(t, "claim-2", taken.ClaimID)
	assert.Equal(t, at.Add(domain.ClaimTTL), taken.ClaimedAt)
	assert.Equal(t, domain.InteractionPending, taken.Resume, "it still goes back to what it was")
	assert.Equal(t, approval(), taken.Base())

	_, err = approval().Reclaim("claim-2", at)
	require.ErrorIs(t, err, domain.ErrInvalidState, "a card that is not claimed is claimed, not reclaimed")
	_, err = claimed.Reclaim("", at.Add(domain.ClaimTTL))
	require.ErrorIs(t, err, domain.ErrInvalidState)
}

func TestClaim_ItSurvivesTheJSONOfTheMeta(t *testing.T) {
	claimed, err := approval().Claim("claim-1", at)
	require.NoError(t, err)

	stored, err := json.Marshal(domain.Meta{Interaction: &claimed})
	require.NoError(t, err)
	var back domain.Meta
	require.NoError(t, json.Unmarshal(stored, &back))

	assert.Equal(t, claimed, *back.Interaction)
	plain, err := json.Marshal(domain.Meta{Interaction: new(approval())})
	require.NoError(t, err)
	assert.NotContains(t, string(plain), "claim", "a card nobody claimed carries no mark of one")
}

func TestClaim_AClaimedCardRefusesEveryAnswerUntilItIsLetGo(t *testing.T) {
	claimed, err := approval().Claim("claim-1", at)
	require.NoError(t, err)

	_, err = claimed.Approved("Paid", "")
	require.ErrorIs(t, err, domain.ErrInvalidState)
	_, err = claimed.Rejected("no")
	require.ErrorIs(t, err, domain.ErrInvalidState)
	_, err = claimed.CodeConfirmed("Paid")
	require.ErrorIs(t, err, domain.ErrInvalidState)
}

// A card whose answer is on its way, waits for a code, or paid is a payment the agent's service knows of: the reply that holds it
// cannot be thrown away and asked again, which could ask for the same transfer a second time.
func TestHoldsPayment_OnlyWhatTheServiceMayHaveDone(t *testing.T) {
	for state, want := range map[domain.InteractionState]bool{
		domain.InteractionProcessing: true, domain.InteractionOTPRequired: true, domain.InteractionPaid: true,
		domain.InteractionPending: false, domain.InteractionRejected: false, domain.InteractionConfirmed: false, "": false,
	} {
		card := approval()
		card.State = state
		assert.Equal(t, want, card.HoldsPayment(), "state %q", state)
	}
}

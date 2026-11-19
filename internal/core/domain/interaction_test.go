package domain_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

func approval() domain.Interaction {
	return domain.Interaction{
		Kind:       domain.InteractionApproval,
		State:      domain.InteractionPending,
		ApprovalID: "appr_1",
		RunID:      "run-1",
		Transfer:   domain.Transfer{Amount: 20, Currency: "PEN", DestinationName: "Rosa Diaz", DestinationPhone: "987654321"},
	}
}

func TestInteraction_ThePaymentGoesForward(t *testing.T) {
	tests := []struct {
		name      string
		from      domain.InteractionState
		do        func(domain.Interaction) (domain.Interaction, error)
		wantState domain.InteractionState
		wantErr   bool
	}{
		{name: "approving a pending card pays it", from: domain.InteractionPending,
			do:        func(i domain.Interaction) (domain.Interaction, error) { return i.Approved("Paid", "") },
			wantState: domain.InteractionPaid},
		{name: "approving a pending card that needs a code asks for it", from: domain.InteractionPending,
			do:        func(i domain.Interaction) (domain.Interaction, error) { return i.Approved("Enter the code", "trf_9") },
			wantState: domain.InteractionOTPRequired},
		{name: "rejecting a pending card closes it", from: domain.InteractionPending,
			do:        func(i domain.Interaction) (domain.Interaction, error) { return i.Rejected("Rejected") },
			wantState: domain.InteractionRejected},
		{name: "the code pays a card that asked for it", from: domain.InteractionOTPRequired,
			do:        func(i domain.Interaction) (domain.Interaction, error) { return i.CodeConfirmed("Paid") },
			wantState: domain.InteractionPaid},
		{name: "a card that asked for a code can still be rejected", from: domain.InteractionOTPRequired,
			do:        func(i domain.Interaction) (domain.Interaction, error) { return i.Rejected("Rejected") },
			wantState: domain.InteractionRejected},
		{name: "a paid card cannot be approved again", from: domain.InteractionPaid, wantErr: true,
			do: func(i domain.Interaction) (domain.Interaction, error) { return i.Approved("Paid", "") }},
		{name: "a rejected card cannot be paid", from: domain.InteractionRejected, wantErr: true,
			do: func(i domain.Interaction) (domain.Interaction, error) { return i.Approved("Paid", "") }},
		{name: "a paid card cannot be rejected", from: domain.InteractionPaid, wantErr: true,
			do: func(i domain.Interaction) (domain.Interaction, error) { return i.Rejected("Rejected") }},
		{name: "a code does nothing on a card that did not ask for one", from: domain.InteractionPending, wantErr: true,
			do: func(i domain.Interaction) (domain.Interaction, error) { return i.CodeConfirmed("Paid") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			card := approval()
			card.State = tc.from

			got, err := tc.do(card)

			if tc.wantErr {
				require.ErrorIs(t, err, domain.ErrInvalidState)
				assert.Equal(t, card, got, "a refused move changes nothing")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantState, got.State)
			assert.Equal(t, card.ApprovalID, got.ApprovalID, "the card keeps its identity")
			assert.Equal(t, card.Transfer, got.Transfer)
		})
	}
}

func TestInteraction_TheResultAndTheChallengeAreKept(t *testing.T) {
	asked, err := approval().Approved("Enter the code", "trf_9")
	require.NoError(t, err)
	assert.Equal(t, "trf_9", asked.ChallengeID)
	assert.Equal(t, "Enter the code", asked.Result)

	paid, err := asked.CodeConfirmed("Transfer paid. TxnID: OP-1")
	require.NoError(t, err)
	assert.Equal(t, "Transfer paid. TxnID: OP-1", paid.Result)
}

func TestInteraction_Open(t *testing.T) {
	tests := map[domain.InteractionState]bool{
		domain.InteractionPending:     true,
		domain.InteractionOTPRequired: true,
		domain.InteractionPaid:        false,
		domain.InteractionRejected:    false,
	}
	for state, want := range tests {
		t.Run(string(state), func(t *testing.T) {
			card := approval()
			card.State = state
			assert.Equal(t, want, card.Open())
		})
	}
}

func TestMeta_TheCardIsStoredInTheMessageAndOldRowsStillRead(t *testing.T) {
	card := approval()
	meta := domain.Meta{TTFTMs: 120, ToolCalls: []string{"draft_transfer"}, Interaction: &card}

	stored, err := json.Marshal(meta)
	require.NoError(t, err)
	var back domain.Meta
	require.NoError(t, json.Unmarshal(stored, &back))

	assert.Equal(t, meta, back, "the card comes back as it went in")
	assert.Contains(t, string(stored), `"interaction"`)

	// A row written before cards existed has no such key, and a reply without a card writes none.
	var old domain.Meta
	require.NoError(t, json.Unmarshal([]byte(`{"ttft_ms":50,"duration_ms":900}`), &old))
	assert.Nil(t, old.Interaction)
	plain, err := json.Marshal(domain.Meta{TTFTMs: 50})
	require.NoError(t, err)
	assert.NotContains(t, string(plain), "interaction")
}

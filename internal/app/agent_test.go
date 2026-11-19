package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/adkrest"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/infrastructure/config"
)

func TestNewAgent_TheProtocolPicksTheAdapter(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Agent
		want any
	}{
		{name: "an ADK REST agent", cfg: config.Agent{Protocol: config.ProtocolADKREST, BaseURL: "http://localhost:9095/api", AppName: "yape_agent"}, want: &adkrest.Client{}},
		{name: "the AG-UI BFF", cfg: config.Agent{Protocol: config.ProtocolAGUIBFF, BaseURL: "http://localhost:4000", AccountID: "25906ac5-9f18-4099-b3fd-e1b74d8549a4", BearerBin: "yape-token"}, want: &aguibff.Client{}},
		{name: "no protocol is the ADK REST agent, as before", cfg: config.Agent{BaseURL: "http://localhost:9093/api", AppName: "ui_agent"}, want: &adkrest.Client{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.IsType(t, tc.want, newAgent(tc.cfg))
		})
	}
}

func TestNewActions_TheAgentThatMakesCardsAnswersThem(t *testing.T) {
	bff := newAgent(config.Agent{Protocol: config.ProtocolAGUIBFF, BaseURL: "http://localhost:4000", AccountID: "25906ac5-9f18-4099-b3fd-e1b74d8549a4", BearerBin: "yape-token"})

	assert.Same(t, bff, newActions(bff), "the BFF client is also the action gateway")
}

func TestNewActions_AnAgentWithoutCardsCannotAnswerOnesLeftBehind(t *testing.T) {
	actions := newActions(newAgent(config.Agent{Protocol: config.ProtocolADKREST, BaseURL: "http://localhost:9095/api", AppName: "yape_agent"}))
	card := domain.Interaction{Kind: domain.InteractionApproval, State: domain.InteractionPending, ApprovalID: "appr_1"}

	_, approve := actions.Approve(t.Context(), card)
	_, reject := actions.Reject(t.Context(), card)
	_, code := actions.ConfirmOTP(t.Context(), card, "123456")
	contact := actions.ConfirmContact(t.Context(), "Maria", domain.Contact{Name: "Maria Torres", Phone: "912345678"})

	for _, err := range []error{approve, reject, code, contact} {
		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	}
}

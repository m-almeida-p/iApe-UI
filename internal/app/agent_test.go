package app

import (
	"testing"

	"github.com/stretchr/testify/assert"

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

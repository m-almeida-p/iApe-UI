package yapeagent_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/platform/envcfg"
	"iape-ui/internal/yapeagent"
)

func settings(extra map[string]string) envcfg.LookupFunc {
	values := map[string]string{
		"YAPETECH_AI_GATEWAY":                "https://gateway.example.com/v1",
		"YAPETECH_ANONYMOUS_TOKEN_BASE_URL":  "https://login.example.com",
		"YAPETECH_ANONYMOUS_TOKEN_PATH":      "/tenant/oauth2/v2.0/token",
		"YAPETECH_ANONYMOUS_TOKEN_USERNAME":  "anon",
		"YAPETECH_ANONYMOUS_TOKEN_PASSWORD":  "hunter2-secret",
		"YAPETECH_ANONYMOUS_TOKEN_CLIENT_ID": "client-1",
	}
	for k, v := range extra {
		values[k] = v
	}
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := yapeagent.LoadConfig(settings(nil))
	require.NoError(t, err)

	assert.Equal(t, "https://gateway.example.com/v1", cfg.GatewayURL)
	assert.Equal(t, yapeagent.DefaultModel, cfg.Model)
	assert.Equal(t, yapeagent.DefaultMCPURL, cfg.MCPURL)
	assert.Equal(t, yapeagent.Headers{DomainID: "ai-assistants", AppID: "ms-ml-genai-agents-bs", ProviderBS: "azure_openai"}, cfg.Headers)
	assert.Equal(t, "password", cfg.Token.GrantType)
	assert.Equal(t, yapeagent.DefaultTimeout, cfg.Token.Timeout)
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := yapeagent.LoadConfig(settings(map[string]string{
		"YAPE_AGENT_MODEL":   "gpt-5.4-nano",
		"YAPE_MCP_URL":       "http://127.0.0.1:4000/mcp",
		"LLM_GATEWAY_APP_ID": "other-app",
		"YAPETECH_ANONYMOUS_TOKEN_TIMEOUT_SECONDS": "3",
		"YAPETECH_ANONYMOUS_TOKEN_SCOPE":           "openid custom",
	}))
	require.NoError(t, err)

	assert.Equal(t, "gpt-5.4-nano", cfg.Model)
	assert.Equal(t, "http://127.0.0.1:4000/mcp", cfg.MCPURL)
	assert.Equal(t, "other-app", cfg.Headers.AppID)
	assert.Equal(t, 3*time.Second, cfg.Token.Timeout)
	assert.Equal(t, "openid custom", cfg.Token.Scope)
}

func TestLoadConfigNamesWhatIsMissingNotItsValue(t *testing.T) {
	_, err := yapeagent.LoadConfig(func(string) (string, bool) { return "", false })

	require.ErrorIs(t, err, yapeagent.ErrMissingSettings)
	for _, key := range []string{"YAPETECH_AI_GATEWAY", "YAPETECH_ANONYMOUS_TOKEN_BASE_URL", "YAPETECH_ANONYMOUS_TOKEN_PATH",
		"YAPETECH_ANONYMOUS_TOKEN_USERNAME", "YAPETECH_ANONYMOUS_TOKEN_PASSWORD", "YAPETECH_ANONYMOUS_TOKEN_CLIENT_ID"} {
		assert.Contains(t, err.Error(), key)
	}
}

func TestLoadConfigRejectsUnsafeURLs(t *testing.T) {
	cases := map[string]map[string]string{
		"gateway over http":      {"YAPETECH_AI_GATEWAY": "http://gateway.example.com/v1"},
		"token endpoint on http": {"YAPETECH_ANONYMOUS_TOKEN_BASE_URL": "http://login.example.com"},
		"MCP not on this laptop": {"YAPE_MCP_URL": "https://mcp.example.com/mcp"},
		"MCP over plain http":    {"YAPE_MCP_URL": "http://mcp.example.com/mcp"},
		"URL with a user":        {"YAPETECH_AI_GATEWAY": "https://user:pw@gateway.example.com/v1"},
		"not a URL":              {"YAPETECH_AI_GATEWAY": "gateway.example.com"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := yapeagent.LoadConfig(settings(extra))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "hunter2-secret")
			assert.NotContains(t, err.Error(), "user:pw")
		})
	}
}

func TestLoadConfigRejectsABadTimeout(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-5"} {
		_, err := yapeagent.LoadConfig(settings(map[string]string{"YAPETECH_ANONYMOUS_TOKEN_TIMEOUT_SECONDS": raw}))
		require.Error(t, err, raw)
	}
}

func TestThePasswordNeverPrints(t *testing.T) {
	cfg, err := yapeagent.LoadConfig(settings(nil))
	require.NoError(t, err)

	assert.NotContains(t, fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), "hunter2-secret")
}

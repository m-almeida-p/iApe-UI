// Package yapeagent is an ADK agent that answers with the models of the Yape AI
// gateway and reads the conversation history through the history service's MCP
// server. It reuses the settings the Yape agent service already has (the
// gateway, its headers and the credentials of the anonymous token) and knows
// neither the Web UI nor the evaluator.
package yapeagent

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"iape-ui/internal/platform/envcfg"
)

// AppName is the application the Web UI asks this agent for (AGENT_APP_NAME).
const AppName = "yape_agent"

// Defaults for what a setting may leave out. The names of the settings are the
// ones the Yape agent service reads, so the same .env serves both.
const (
	DefaultModel      = "gpt-5.4-mini-priority"
	DefaultMCPURL     = "http://localhost:3200/mcp"
	DefaultDomainID   = "ai-assistants"
	DefaultAppID      = "ms-ml-genai-agents-bs"
	DefaultProviderBS = "azure_openai"
	DefaultGrantType  = "password"
	DefaultTimeout    = 10 * time.Second
)

// Headers are the headers the gateway wants on every model request.
type Headers struct {
	DomainID   string
	AppID      string
	ProviderBS string
}

// TokenConfig says where and how to ask for the anonymous bearer token that the
// gateway accepts.
type TokenConfig struct {
	BaseURL  string
	Path     string
	Username string
	Password envcfg.Secret
	ClientID string
	// GrantType is "password" unless the settings say otherwise.
	GrantType string
	// Scope is "openid <ClientID>" when empty, as the Yape agent service does.
	Scope   string
	Timeout time.Duration
}

// Config is everything the agent needs to start.
type Config struct {
	// GatewayURL is the base URL of the gateway's OpenAI-compatible API.
	GatewayURL string
	// Model is the gateway's name for the model.
	Model   string
	Headers Headers
	Token   TokenConfig
	// MCPURL is the endpoint of the history service's MCP server.
	MCPURL string
}

// ErrMissingSettings is wrapped by LoadConfig when required settings are absent.
var ErrMissingSettings = errors.New("missing settings")

// LoadConfig reads the agent's settings. The error names a setting that is
// wrong, never its value: some of them are credentials.
func LoadConfig(lookup envcfg.LookupFunc) (Config, error) {
	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}
	var missing []string
	need := func(key string) string {
		v := get(key, "")
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	cfg := Config{
		GatewayURL: need("YAPETECH_AI_GATEWAY"),
		Model:      get("YAPE_AGENT_MODEL", DefaultModel),
		MCPURL:     get("YAPE_MCP_URL", DefaultMCPURL),
		Headers: Headers{
			DomainID:   get("LLM_GATEWAY_DOMAIN_ID", DefaultDomainID),
			AppID:      get("LLM_GATEWAY_APP_ID", DefaultAppID),
			ProviderBS: get("LLM_GATEWAY_PROVIDER_BS", DefaultProviderBS),
		},
		Token: TokenConfig{
			BaseURL:   need("YAPETECH_ANONYMOUS_TOKEN_BASE_URL"),
			Path:      need("YAPETECH_ANONYMOUS_TOKEN_PATH"),
			Username:  need("YAPETECH_ANONYMOUS_TOKEN_USERNAME"),
			Password:  envcfg.Secret(need("YAPETECH_ANONYMOUS_TOKEN_PASSWORD")),
			ClientID:  need("YAPETECH_ANONYMOUS_TOKEN_CLIENT_ID"),
			GrantType: get("YAPETECH_ANONYMOUS_TOKEN_GRANT_TYPE", DefaultGrantType),
			Scope:     get("YAPETECH_ANONYMOUS_TOKEN_SCOPE", ""),
			Timeout:   DefaultTimeout,
		},
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("%w: %s (the Yape agent service's .env has them; see YAPE_AGENT_BS_ENV in .env.example)",
			ErrMissingSettings, strings.Join(missing, ", "))
	}

	if raw := get("YAPETECH_ANONYMOUS_TOKEN_TIMEOUT_SECONDS", ""); raw != "" {
		seconds, err := time.ParseDuration(raw + "s")
		if err != nil || seconds <= 0 {
			return Config{}, fmt.Errorf("YAPETECH_ANONYMOUS_TOKEN_TIMEOUT_SECONDS=%q is not a positive number of seconds", raw)
		}
		cfg.Token.Timeout = seconds
	}

	// The bearer token travels to the gateway and the token endpoint: both must be
	// https. The MCP server has no authentication, so it must be this computer.
	for _, check := range []struct {
		key, value string
		onlyHere   bool
	}{
		{"YAPETECH_AI_GATEWAY", cfg.GatewayURL, false},
		{"YAPETECH_ANONYMOUS_TOKEN_BASE_URL", cfg.Token.BaseURL, false},
		{"YAPE_MCP_URL", cfg.MCPURL, true},
	} {
		if err := checkURL(check.key, check.value, check.onlyHere); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// checkURL accepts an https URL, or an http one for this computer. onlyHere
// accepts nothing but this computer.
func checkURL(key, raw string, onlyHere bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%s is not a URL of the form scheme://host[:port]/path (without a user)", key)
	}
	here := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	switch {
	case onlyHere && !here:
		return fmt.Errorf("%s must point to this computer (localhost): the MCP server has no authentication", key)
	case u.Scheme == "https", u.Scheme == "http" && here:
		return nil
	default:
		return fmt.Errorf("%s must be https (http only for this computer): a bearer token travels to it", key)
	}
}

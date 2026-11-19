package yapeagent

import (
	"context"
	"errors"
	"net/http"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

// gatewayTransport puts the bearer token and the gateway's headers on every
// request, asking for the token each time so that a long-lived agent never
// sends an expired one.
type gatewayTransport struct {
	base    http.RoundTripper
	tokens  TokenProvider
	headers Headers
}

func (t gatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.tokens.Token(req.Context())
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token.Reveal())
	for name, value := range map[string]string{
		"x-domain-id":   t.headers.DomainID,
		"x-app-id":      t.headers.AppID,
		"x-provider-bs": t.headers.ProviderBS,
	} {
		if value != "" {
			clone.Header.Set(name, value)
		}
	}
	return t.base.RoundTrip(clone)
}

// NewModel returns the gateway's model cfg.Model, called through the OpenAI
// Responses API. base is the transport under the one that adds the token; nil
// means the default.
func NewModel(ctx context.Context, cfg Config, tokens TokenProvider, base http.RoundTripper) (model.LLM, error) {
	if tokens == nil {
		return nil, errors.New("yapeagent: a token provider is required")
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return openaimodel.NewModel(ctx, cfg.Model, &openaimodel.ClientConfig{
		BaseURL: cfg.GatewayURL,
		// No Timeout: the client also carries the streamed replies.
		HTTPClient: &http.Client{Transport: gatewayTransport{base: base, tokens: tokens, headers: cfg.Headers}},
	})
}

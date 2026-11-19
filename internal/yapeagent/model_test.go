package yapeagent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"iape-ui/internal/platform/envcfg"
	"iape-ui/internal/yapeagent"
)

type fakeTokens struct {
	token string
	err   error
	calls int
}

func (f *fakeTokens) Token(context.Context) (envcfg.Secret, error) {
	f.calls++
	return envcfg.Secret(f.token), f.err
}

// responsesAnswer is the smallest Responses API answer that carries a text.
const responsesAnswer = `{"id":"resp_1","object":"response","status":"completed","model":"m","output":[
  {"type":"message","id":"msg_1","role":"assistant","status":"completed",
   "content":[{"type":"output_text","text":"pong","annotations":[]}]}],
  "usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`

func ask(t *testing.T, llm model.LLM) (string, error) {
	t.Helper()
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("ping", genai.RoleUser)}}
	var text string
	for resp, err := range llm.GenerateContent(context.Background(), req, false) {
		if err != nil {
			return "", err
		}
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				text += p.Text
			}
		}
	}
	return text, nil
}

func TestModelCallsTheGatewayWithTheTokenAndItsHeaders(t *testing.T) {
	var got http.Header
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesAnswer))
	}))
	defer server.Close()
	tokens := &fakeTokens{token: "tok-abc"}
	cfg := yapeagent.Config{
		GatewayURL: server.URL + "/v1", Model: "gpt-test",
		Headers: yapeagent.Headers{DomainID: "dom", AppID: "app", ProviderBS: "prov"},
	}

	llm, err := yapeagent.NewModel(context.Background(), cfg, tokens, nil)
	require.NoError(t, err)
	text, err := ask(t, llm)

	require.NoError(t, err)
	assert.Equal(t, "pong", text)
	assert.Equal(t, "/v1/responses", path)
	assert.Equal(t, "Bearer tok-abc", got.Get("Authorization"))
	assert.Equal(t, "dom", got.Get("x-domain-id"))
	assert.Equal(t, "app", got.Get("x-app-id"))
	assert.Equal(t, "prov", got.Get("x-provider-bs"))
	assert.Equal(t, "gpt-test", llm.Name())
}

func TestModelAsksForTheTokenOnEveryRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesAnswer))
	}))
	defer server.Close()
	tokens := &fakeTokens{token: "tok"}
	llm, err := yapeagent.NewModel(context.Background(), yapeagent.Config{GatewayURL: server.URL, Model: "m"}, tokens, nil)
	require.NoError(t, err)

	_, err = ask(t, llm)
	require.NoError(t, err)
	_, err = ask(t, llm)
	require.NoError(t, err)

	assert.Equal(t, 2, tokens.calls, "a long-lived agent must not keep an old token")
}

func TestModelSendsNoRequestWithoutAToken(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer server.Close()
	llm, err := yapeagent.NewModel(context.Background(), yapeagent.Config{GatewayURL: server.URL, Model: "m"},
		&fakeTokens{err: errors.New("token request: the token endpoint answered 400 (invalid_grant)")}, nil)
	require.NoError(t, err)

	_, err = ask(t, llm)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_grant")
	assert.Zero(t, hits, "the gateway never saw a request without a token")
}

func TestNewModelNeedsAProviderAndAName(t *testing.T) {
	_, err := yapeagent.NewModel(context.Background(), yapeagent.Config{Model: "m"}, nil, nil)
	require.Error(t, err)

	_, err = yapeagent.NewModel(context.Background(), yapeagent.Config{}, &fakeTokens{}, nil)
	require.Error(t, err, "openaimodel refuses an empty model name")
}

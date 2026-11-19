package yapeagent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/platform/envcfg"
	"iape-ui/internal/yapeagent"
)

func tokenConfig(baseURL string) yapeagent.TokenConfig {
	return yapeagent.TokenConfig{
		BaseURL: baseURL, Path: "/tenant/token", Username: "anon", Password: envcfg.Secret("hunter2-secret"),
		ClientID: "client-1", Timeout: time.Second,
	}
}

func TestTokenSourceAsksWithThePasswordGrant(t *testing.T) {
	var form map[string][]string
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form, contentType = r.PostForm, r.Header.Get("Content-Type")
		assert.Equal(t, "/tenant/token", r.URL.Path)
		_, _ = w.Write([]byte(`{"access_token":"tok-1","token_type":"Bearer","expires_in":"43200"}`))
	}))
	defer server.Close()

	token, err := yapeagent.NewTokenSource(tokenConfig(server.URL+"/"), nil).Token(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "tok-1", token.Reveal())
	assert.Equal(t, "application/x-www-form-urlencoded", contentType)
	assert.Equal(t, []string{"anon"}, form["username"])
	assert.Equal(t, []string{"hunter2-secret"}, form["password"])
	assert.Equal(t, []string{"password"}, form["grant_type"])
	assert.Equal(t, []string{"openid client-1"}, form["scope"], "no scope setting: the Yape agent service's default")
	assert.Equal(t, []string{"client-1"}, form["client_id"])
	assert.Equal(t, []string{"token"}, form["response_type"])
}

func TestTokenSourceKeepsTheTokenUntilMostOfItsLifeIsGone(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		_, _ = w.Write([]byte(map[int32]string{
			1: `{"access_token":"first","expires_in":100}`,
			2: `{"access_token":"second","expires_in":100}`,
		}[n]))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	source := yapeagent.NewTokenSource(tokenConfig(server.URL), nil)
	source.SetClock(func() time.Time { return now })

	first, err := source.Token(context.Background())
	require.NoError(t, err)
	now = now.Add(79 * time.Second)
	again, err := source.Token(context.Background())
	require.NoError(t, err)
	now = now.Add(2 * time.Second) // 81 s of 100: past 80%
	renewed, err := source.Token(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "first", first.Reveal())
	assert.Equal(t, "first", again.Reveal(), "still fresh: no second request")
	assert.Equal(t, "second", renewed.Reveal())
	assert.EqualValues(t, 2, calls.Load())
}

func TestTokenSourceFailuresNeverEchoTheAnswerOrTheCredentials(t *testing.T) {
	cases := map[string]struct {
		status   int
		body     string
		contains string
	}{
		"oauth error":       {http.StatusBadRequest, `{"error":"invalid_grant","error_description":"user anon password hunter2-secret wrong"}`, "400 (invalid_grant)"},
		"error with a text": {http.StatusUnauthorized, `{"error":"Bad Thing With Spaces"}`, "answered 401"},
		"html":              {http.StatusBadGateway, `<html>hunter2-secret</html>`, "answered 502"},
		"no token":          {http.StatusOK, `{"expires_in":10}`, "no access_token"},
		"no lifetime":       {http.StatusOK, `{"access_token":"x"}`, "no valid expires_in"},
		"not json":          {http.StatusOK, `nope`, "no access_token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := yapeagent.NewTokenSource(tokenConfig(server.URL), nil).Token(context.Background())

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.contains)
			assert.NotContains(t, err.Error(), "hunter2-secret")
			assert.NotContains(t, err.Error(), "description")
		})
	}
}

func TestTokenSourceWhenTheEndpointIsDown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	_, err := yapeagent.NewTokenSource(tokenConfig(url), nil).Token(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer")
	assert.NotContains(t, err.Error(), url, "the error does not quote the URL")
}

func TestTokenSourceHonoursCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := yapeagent.NewTokenSource(tokenConfig(server.URL), nil).Token(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestTokenSourceRefusesAnEndpointThatMakesNoRequest(t *testing.T) {
	cfg := tokenConfig("http://bad host")

	_, err := yapeagent.NewTokenSource(cfg, nil).Token(context.Background())

	require.Error(t, err)
}

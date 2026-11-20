package aguibff_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/infrastructure/agent/aguibff"
	"iape-ui/internal/infrastructure/config"
)

// fakeTokens is a TokenSource that also refreshes: it hands out the tokens in order.
type fakeTokens struct {
	mu        sync.Mutex
	token     string
	fresh     string
	err       error
	refreshed int
	// asked is the account of every call, in order.
	asked []uuid.UUID
}

func (f *fakeTokens) Token(_ context.Context, account uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, account)
	return f.token, f.err
}

func (f *fakeTokens) Refresh(_ context.Context, account uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed++
	f.asked = append(f.asked, account)
	f.token = f.fresh
	return f.fresh, f.err
}

// bff is a BFF that serves a fixture and records what it was asked.
type bff struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func newBFF(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) *bff {
	b := &bff{t: t}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.requests = append(b.requests, r.Clone(context.Background()))
		b.bodies = append(b.bodies, body)
		n := len(b.requests)
		b.mu.Unlock()
		handler(w, r, n)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bff) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

func serveFixture(t *testing.T, name string) func(http.ResponseWriter, *http.Request, int) {
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func clientFor(b *bff, tokens aguibff.TokenSource) *aguibff.Client {
	return aguibff.NewClient(config.Agent{BaseURL: b.srv.URL, RequestTimeout: 5 * time.Second}, tokens, nil)
}

func turn(text string) domain.AgentRequest {
	return domain.AgentRequest{UserID: accountID, ConversationID: uuid.New(), Text: text}
}

func TestClient_RunAsksTheBFFAsTheAccount(t *testing.T) {
	b := newBFF(t, serveFixture(t, "guard.sse"))
	req := turn("Hola")

	events, err := collect(clientFor(b, &fakeTokens{token: "tok-1"}).Run(t.Context(), req))

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:No puedo ayudarte con esa solicitud. ¿Intentamos con otra?"}, shape(events))
	require.Equal(t, 1, b.count())
	r := b.requests[0]
	assert.Equal(t, http.MethodPost, r.Method)
	assert.Equal(t, "/agent", r.URL.Path)
	assert.Equal(t, "Bearer tok-1", r.Header.Get("Authorization"))
	assert.Equal(t, account, r.Header.Get("X-Yape-Account-Id"))
	assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
	assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))

	var got struct {
		ThreadID string `json:"threadId"`
		RunID    string `json:"runId"`
		Messages []struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		State          map[string]any `json:"state"`
		Tools          []any          `json:"tools"`
		Context        []any          `json:"context"`
		ForwardedProps map[string]any `json:"forwardedProps"`
	}
	require.NoError(t, json.Unmarshal(b.bodies[0], &got))
	assert.Equal(t, req.ConversationID.String(), got.ThreadID, "the conversation is the thread: the agent remembers it")
	_, err = uuid.Parse(got.RunID)
	assert.NoError(t, err, "every turn is a new run")
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "user", got.Messages[0].Role)
	assert.Equal(t, "Hola", got.Messages[0].Content)
	assert.NotNil(t, got.State)
	assert.NotNil(t, got.Tools)
	assert.NotNil(t, got.Context)
	assert.NotNil(t, got.ForwardedProps)
}

func TestClient_EveryTurnIsANewRun(t *testing.T) {
	b := newBFF(t, serveFixture(t, "text.sse"))
	c := clientFor(b, &fakeTokens{token: "t"})

	for range 2 {
		_, err := collect(c.Run(t.Context(), turn("Hola")))
		require.NoError(t, err)
	}

	var runs []string
	for _, body := range b.bodies {
		var got struct {
			RunID string `json:"runId"`
		}
		require.NoError(t, json.Unmarshal(body, &got))
		runs = append(runs, got.RunID)
	}
	assert.NotEqual(t, runs[0], runs[1])
}

func TestClient_AnExpiredTokenIsRefreshedOnceAndTheTurnIsAskedAgain(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("Authorization") != "Bearer new" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		serveFixture(t, "text.sse")(w, r, n)
	})
	tokens := &fakeTokens{token: "old", fresh: "new"}

	events, err := collect(clientFor(b, tokens).Run(t.Context(), turn("Hola")))

	require.NoError(t, err)
	assert.Equal(t, 2, b.count())
	assert.Equal(t, 1, tokens.refreshed)
	assert.Equal(t, "Bearer new", b.requests[1].Header.Get("Authorization"))
	assert.Len(t, events, 2)
}

func TestClient_ARefusedTokenIsNotRetriedForever(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { http.Error(w, "no", http.StatusUnauthorized) })
	tokens := &fakeTokens{token: "old", fresh: "also-bad"}

	_, err := collect(clientFor(b, tokens).Run(t.Context(), turn("Hola")))

	var httpErr *aguibff.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusUnauthorized, httpErr.Status)
	assert.Equal(t, 2, b.count(), "one retry, with a fresh token, and no more")
}

func TestClient_NoTokenMeansNoRequestAndAnUnavailableAgent(t *testing.T) {
	b := newBFF(t, serveFixture(t, "text.sse"))
	tokens := &fakeTokens{err: aguibff.ErrToken}

	_, err := collect(clientFor(b, tokens).Run(t.Context(), turn("Hola")))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	require.ErrorIs(t, err, aguibff.ErrToken)
	assert.Zero(t, b.count())
}

func TestClient_AGatewayErrorIsAnUnavailableAgent(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { http.Error(w, "down", status) })

			_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

			require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		})
	}
}

func TestClient_AnotherContentTypeIsNotAStream(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>login</html>")
	})

	_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "text/event-stream")
}

func TestClient_AServerThatDoesNotAnswerTimesOut(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) { <-r.Context().Done() })
	c := aguibff.NewClient(config.Agent{BaseURL: b.srv.URL, RequestTimeout: 150 * time.Millisecond}, &fakeTokens{token: "t"}, nil)

	_, err := collect(c.Run(t.Context(), turn("Hola")))

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "timed out")
}

func TestClient_TheCallerCanStopATurn(t *testing.T) {
	started := make(chan struct{})
	b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m\"}\n\ndata: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m\",\"delta\":\"hola\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // the BFF sees the caller leave
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var got []domain.AgentEvent
	var runErr error

	for ev, err := range clientFor(b, &fakeTokens{token: "t"}).Run(ctx, turn("Hola")) {
		if err != nil {
			runErr = err
			break
		}
		got = append(got, ev)
		<-started
		cancel()
	}

	require.ErrorIs(t, runErr, context.Canceled)
	assert.Equal(t, []string{"delta"}, shape(got))
}

func TestClient_Ping(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "a healthy BFF", status: http.StatusOK},
		{name: "an unhealthy BFF", status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				assert.Equal(t, "/health", r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"status":"ok","service":"agui-bff"}`)
			})

			err := clientFor(b, &fakeTokens{token: "t"}).Ping(t.Context())

			if tc.wantErr {
				require.ErrorIs(t, err, domain.ErrAgentUnavailable)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, b.requests[0].Header.Get("Authorization"), "/health needs no token")
		})
	}
}

func TestClient_PingOfAnUnreachableBFF(t *testing.T) {
	b := newBFF(t, func(http.ResponseWriter, *http.Request, int) {})
	c := clientFor(b, &fakeTokens{token: "t"})
	b.srv.Close()

	err := c.Ping(t.Context())

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
}

func TestClient_TheTokenIsNeverInAnError(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { http.Error(w, "nope", http.StatusForbidden) })

	_, err := collect(clientFor(b, &fakeTokens{token: "tok-secret-canary-55"}).Run(t.Context(), turn("Hola")))

	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), "tok-secret-canary-55"))
}

func TestClient_TheBFFAnswersAStreamWith201(t *testing.T) {
	body := fixture(t, "guard.sse")
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusCreated) // what a NestJS POST answers by default: the real BFF does
		_, _ = io.WriteString(w, body)
	})

	events, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

	require.NoError(t, err)
	assert.Equal(t, []string{"delta", "final:No puedo ayudarte con esa solicitud. ¿Intentamos con otra?"}, shape(events))
}

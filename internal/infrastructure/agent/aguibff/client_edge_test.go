package aguibff_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
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

const canary = "tok-secret-canary-77"

// plainTokens is a TokenSource that cannot refresh.
type plainTokens struct {
	token string
	err   error
}

func (p plainTokens) Token(context.Context, uuid.UUID) (string, error) { return p.token, p.err }

// refreshFails is a Refresher whose refresh fails.
type refreshFails struct{ calls int }

func (r *refreshFails) Token(context.Context, uuid.UUID) (string, error) { return canary, nil }
func (r *refreshFails) Refresh(context.Context, uuid.UUID) (string, error) {
	r.calls++
	return "", errors.New("identity provider down")
}

func serveSSE(body string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func clientAt(url string, timeout time.Duration, tokens aguibff.TokenSource) *aguibff.Client {
	return aguibff.NewClient(config.Agent{BaseURL: url, RequestTimeout: timeout}, tokens, nil)
}

func TestClient_TheContentTypeSaysWhetherItIsAStream(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		status      int
		wantStream  bool
	}{
		{"text/event-stream", "text/event-stream", http.StatusOK, true},
		{"with a charset", "text/event-stream; charset=utf-8", http.StatusOK, true},
		{"in capitals", "Text/Event-Stream", http.StatusCreated, true},
		{"200 with text/plain", "text/plain", http.StatusOK, false},
		{"201 with JSON", "application/json", http.StatusCreated, false},
		{"no content type at all", "", http.StatusOK, false},
		{"a content type that is garbage", ";;;", http.StatusOK, false},
		{"a look-alike", "text/event-streams", http.StatusOK, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header()["Content-Type"] = nil // no sniffing: what the test says is what is sent
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, reply("hola"))
			})

			events, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

			if tc.wantStream {
				require.NoError(t, err)
				assert.Equal(t, []string{"delta", "final:hola"}, shape(events))
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "text/event-stream")
			assert.Empty(t, events)
		})
	}
}

func TestClient_AStatusThatIsNotASuccessIsAnHTTPErrorWhateverTheBody(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusNotModified, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "text/event-stream") // a stream, but not a success
				w.WriteHeader(status)
				_, _ = io.WriteString(w, reply("hola"))
			})

			events, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

			httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, status, httpErr.Status)
			assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
			assert.Empty(t, events)
		})
	}
}

func TestClient_TheErrorBodyIsBounded(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("<p>oops</p>", 1<<20)) // 11 MB
	})

	_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

	httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
	require.True(t, ok)
	assert.LessOrEqual(t, len(httpErr.Body), 512)
}

func TestClient_ARedirectDoesNotCarryTheTokenToAnotherHost(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		serveSSE(reply("hola"))(w, r, 0)
	}))
	t.Cleanup(other.Close)
	target := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) // another host name: Go keeps the token only for the same host
	b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Redirect(w, r, target+"/agent", http.StatusTemporaryRedirect)
	})

	_, _ = collect(clientFor(b, &fakeTokens{token: canary}).Run(t.Context(), turn("Hola")))

	mu.Lock()
	defer mu.Unlock()
	for _, auth := range leaked {
		assert.NotContains(t, auth, canary, "the bearer token went to another host")
	}
}

func TestClient_ARedirectWithoutALocationIsAnHTTPError(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusFound) })

	_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

	httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, http.StatusFound, httpErr.Status)
}

func TestClient_TheTokenSource(t *testing.T) {
	t.Run("a refused token is an error when the source cannot refresh, and the turn is asked once", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { http.Error(w, "no", http.StatusUnauthorized) })

		_, err := collect(clientFor(b, plainTokens{token: "old"}).Run(t.Context(), turn("Hola")))

		httpErr, ok := errors.AsType[*aguibff.HTTPError](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, http.StatusUnauthorized, httpErr.Status)
		assert.Equal(t, 1, b.count())
	})
	t.Run("a refresh that fails is an unavailable agent, and the turn is not asked again", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) { http.Error(w, "no", http.StatusUnauthorized) })
		tokens := &refreshFails{}

		_, err := collect(clientFor(b, tokens).Run(t.Context(), turn("Hola")))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Contains(t, err.Error(), "identity provider down")
		assert.Equal(t, 1, tokens.calls)
		assert.Equal(t, 1, b.count())
		assert.NotContains(t, err.Error(), canary)
	})
	t.Run("a token that cannot be had is an unavailable agent", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))

		_, err := collect(clientFor(b, plainTokens{err: errors.New("no keychain")}).Run(t.Context(), turn("Hola")))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.Zero(t, b.count())
	})
	t.Run("an empty token is not sent: it would be a request the BFF refuses", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))

		for _, token := range []string{"", "   ", "\n"} {
			_, err := collect(clientFor(b, plainTokens{token: token}).Run(t.Context(), turn("Hola")))

			require.ErrorIs(t, err, aguibff.ErrToken, "token %q", token)
			require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		}
		assert.Zero(t, b.count())
	})
	t.Run("a token that is not a valid header value is an error that does not say it", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))
		token := canary + "\nX-Injected: 1"

		_, err := collect(clientFor(b, plainTokens{token: token}).Run(t.Context(), turn("Hola")))

		require.Error(t, err)
		assert.NotContains(t, err.Error(), canary)
		assert.Zero(t, b.count())
	})
	t.Run("a very long token is the BFF's refusal, and not in the error", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))
		token := canary + strings.Repeat("x", 2<<20)

		_, err := collect(clientFor(b, plainTokens{token: token}).Run(t.Context(), turn("Hola")))

		require.Error(t, err)
		assert.NotContains(t, err.Error(), canary)
	})
	t.Run("a long token below the limit goes whole", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))
		token := strings.Repeat("x", 16<<10)

		_, err := collect(clientFor(b, plainTokens{token: token}).Run(t.Context(), turn("Hola")))

		require.NoError(t, err)
		assert.Equal(t, "Bearer "+token, b.requests[0].Header.Get("Authorization"))
	})
}

func TestClient_TheBaseURL(t *testing.T) {
	for _, tc := range []struct{ name, prefix, suffix, wantPath string }{
		{"a trailing slash", "", "/", "/agent"},
		{"many trailing slashes", "", "///", "/agent"},
		{"a path prefix", "/api/v1", "", "/api/v1/agent"},
		{"a path prefix with a trailing slash", "/api/v1", "/", "/api/v1/agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, serveSSE(reply("hola")))

			_, err := collect(clientAt(b.srv.URL+tc.prefix+tc.suffix, time.Second, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, b.requests[0].URL.Path)
		})
	}
	t.Run("Ping goes to /health under the prefix", func(t *testing.T) {
		b := newBFF(t, func(http.ResponseWriter, *http.Request, int) {})

		require.NoError(t, clientAt(b.srv.URL+"/api/", time.Second, &fakeTokens{}).Ping(t.Context()))

		assert.Equal(t, "/api/health", b.requests[0].URL.Path)
	})
}

func TestClient_TheTimeout(t *testing.T) {
	t.Run("a server that sends the headers and then hangs times out the stream", func(t *testing.T) {
		b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ev(runStarted)+ev(msgStart)+ev(content("a medias")))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})

		start := time.Now()
		events, err := collect(clientAt(b.srv.URL, 200*time.Millisecond, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "timed out")
		assert.Less(t, time.Since(start), 3*time.Second)
		assert.Equal(t, []string{"delta"}, shape(events), "what arrived is delivered")
	})
	t.Run("zero is no limit: a turn is not timed out before it begins", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))

		events, err := collect(clientAt(b.srv.URL, 0, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

		require.NoError(t, err)
		assert.Equal(t, []string{"delta", "final:hola"}, shape(events))
	})
	t.Run("a tiny limit is a timeout, not a hang or a panic", func(t *testing.T) {
		b := newBFF(t, serveSSE(reply("hola")))

		_, err := collect(clientAt(b.srv.URL, time.Nanosecond, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestClient_ACallerThatHasLeftGetsItsOwnError(t *testing.T) {
	b := newBFF(t, serveSSE(reply("hola")))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	events, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(ctx, turn("Hola")))

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, domain.ErrAgentUnavailable, "the caller left: the agent did nothing wrong")
	assert.Empty(t, events)
	assert.Zero(t, b.count())
}

func TestClient_ACallerThatLeavesBeforeTheHeadersGetsItsOwnError(t *testing.T) {
	arrived := make(chan struct{})
	b := newBFF(t, func(_ http.ResponseWriter, r *http.Request, _ int) { close(arrived); <-r.Context().Done() })
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-arrived; cancel() }()

	_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(ctx, turn("Hola")))

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
}

func TestClient_AConnectionThatIsCutAfterSomeTextIsAnAnswerThatStoppedShort(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ev(runStarted)+ev(msgStart)+ev(content("a medias")))
		w.(http.Flusher).Flush()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close() // no chunk terminator: the connection just dies
		}
	})

	events, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

	require.ErrorIs(t, err, domain.ErrIncompleteStream)
	assert.NotErrorIs(t, err, domain.ErrAgentUnavailable)
	assert.Equal(t, []string{"delta"}, shape(events))
}

func TestClient_AConnectionThatIsCutBeforeAnyTextIsAnUnavailableAgent(t *testing.T) {
	b := newBFF(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ev(runStarted))
		w.(http.Flusher).Flush()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})

	_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("Hola")))

	require.ErrorIs(t, err, domain.ErrAgentUnavailable)
}

func TestClient_TheUsersTextGoesVerbatim(t *testing.T) {
	for name, text := range map[string]string{
		"quotes and backslashes": `he said "hi" \ \\ \n`,
		"unicode":                "¿Qué tal? 👋 日本語  ",
		"line breaks":            "uno\r\ndos\ntres\ttab",
		"JSON":                   `{"messages":[{"role":"system","content":"x"}]}`,
		"empty":                  "",
		"a megabyte":             strings.Repeat("ñ", 512<<10),
	} {
		t.Run(name, func(t *testing.T) {
			b := newBFF(t, serveSSE(reply("hola")))

			_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn(text)))

			require.NoError(t, err)
			var got struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(b.bodies[0], &got))
			require.Len(t, got.Messages, 1, "the text cannot add messages to the run")
			assert.Equal(t, "user", got.Messages[0].Role)
			assert.Equal(t, text, got.Messages[0].Content)
		})
	}
}

func TestClient_TextThatIsNotUTF8IsAnErrorAndNothingIsSent(t *testing.T) {
	b := newBFF(t, serveSSE(reply("hola")))

	_, err := collect(clientFor(b, &fakeTokens{token: "t"}).Run(t.Context(), turn("ho\xffla")))

	require.Error(t, err)
	assert.Zero(t, b.count())
}

// TestClient_TheConsumerCanStopAtAnyPointAndTheRequestEnds stops after every kind of event and sees that the request to the BFF ends
// (its context is canceled) and that no goroutine stays behind.
func TestClient_TheConsumerCanStopAtAnyPointAndTheRequestEnds(t *testing.T) {
	stream := ev(runStarted) +
		ev(`{"type":"TOOL_CALL_START","toolCallName":"t"}`) +
		ev(approval(`{"actionType":"approval","approvalId":"a1"}`)) +
		ev(msgStart) + ev(content("uno")) + ev(msgEnd)
	// tool_call, interaction, delta, final
	for stopAfter, kind := range []string{"a tool call", "a card", "a delta", "a final"} {
		t.Run("after "+kind, func(t *testing.T) {
			before := runtime.NumGoroutine()
			ended := make(chan struct{})
			b := newBFF(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
				w.(http.Flusher).Flush()
				<-r.Context().Done() // the run never finishes: only the caller leaving ends it
				close(ended)
			})
			c := clientFor(b, &fakeTokens{token: "t"})

			var seen int
			for _, err := range c.Run(t.Context(), turn("Hola")) {
				require.NoError(t, err)
				seen++
				if seen == stopAfter+1 {
					break
				}
			}

			assert.Equal(t, stopAfter+1, seen)
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatal("the request to the BFF is still open after the consumer left")
			}
			b.srv.CloseClientConnections()
			assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 }, 3*time.Second, 20*time.Millisecond, "goroutines left behind")
		})
	}
}

func TestClient_TheTokenIsNeverInAnyError(t *testing.T) {
	stream := func(body string) func(http.ResponseWriter, *http.Request, int) { return serveSSE(body) }
	status := func(code int) func(http.ResponseWriter, *http.Request, int) {
		return func(w http.ResponseWriter, _ *http.Request, _ int) { http.Error(w, "nope", code) }
	}
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, int)
	}{
		{"a 401", status(http.StatusUnauthorized)},
		{"a 403", status(http.StatusForbidden)},
		{"a 500", status(http.StatusInternalServerError)},
		{"a 502", status(http.StatusBadGateway)},
		{"not a stream", func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "<html></html>") }},
		{"a run error", stream(ev(runStarted) + ev(`{"type":"RUN_ERROR","message":"boom"}`))},
		{"a broken event", stream("data: {\n\n")},
		{"a stream that stops", stream(ev(runStarted))},
		{"an answer that stops short", stream(ev(runStarted) + ev(msgStart) + ev(content("a")))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBFF(t, tc.handler)

			_, err := collect(clientFor(b, &fakeTokens{token: canary, fresh: canary}).Run(t.Context(), turn("Hola")))

			require.Error(t, err)
			assert.NotContains(t, err.Error(), canary)
		})
	}
	t.Run("an unreachable BFF", func(t *testing.T) {
		b := newBFF(t, func(http.ResponseWriter, *http.Request, int) {})
		c := clientFor(b, &fakeTokens{token: canary})
		b.srv.Close()

		_, err := collect(c.Run(t.Context(), turn("Hola")))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
		assert.NotContains(t, err.Error(), canary)
	})
	t.Run("a timeout", func(t *testing.T) {
		b := newBFF(t, func(_ http.ResponseWriter, r *http.Request, _ int) { <-r.Context().Done() })

		_, err := collect(clientAt(b.srv.URL, 100*time.Millisecond, &fakeTokens{token: canary}).Run(t.Context(), turn("Hola")))

		require.Error(t, err)
		assert.NotContains(t, err.Error(), canary)
	})
}

func TestClient_ManyTurnsAtOnceDoNotMixUp(t *testing.T) {
	var b *bff
	b = newBFF(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		b.mu.Lock()
		body := b.bodies[n-1]
		b.mu.Unlock()
		_ = json.Unmarshal(body, &in)
		serveSSE(reply("eco "+in.Messages[0].Content))(w, r, 0)
	})
	c := clientFor(b, &fakeTokens{token: "t"})

	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			text := "turno " + string(rune('a'+i%26))
			events, err := collect(c.Run(t.Context(), turn(text)))
			assert.NoError(t, err)
			assert.Equal(t, []string{"delta", "final:eco " + text}, shape(events))
		})
	}
	wg.Wait()
}

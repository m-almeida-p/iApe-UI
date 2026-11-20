package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/testbff"
)

func TestOptions_ValuesThatAreSetMustBeValid(t *testing.T) {
	tests := map[string]map[string]string{
		"a chunk count of zero":            {"TEST_BFF_SLOW_CHUNKS": "0"},
		"a negative chunk count":           {"TEST_BFF_SLOW_CHUNKS": "-1"},
		"a chunk count with a decimal":     {"TEST_BFF_SLOW_CHUNKS": "1.5"},
		"a chunk count in exponent form":   {"TEST_BFF_SLOW_CHUNKS": "5e2"},
		"a chunk count of letters":         {"TEST_BFF_SLOW_CHUNKS": "many"},
		"a chunk count with a sign":        {"TEST_BFF_SLOW_CHUNKS": "+5x"},
		"blanks as a chunk count":          {"TEST_BFF_SLOW_CHUNKS": "  "},
		"one chunk too many":               {"TEST_BFF_SLOW_CHUNKS": "1000001"},
		"a chunk count that overflows":     {"TEST_BFF_SLOW_CHUNKS": "99999999999999999999"},
		"a chunk count of a billion":       {"TEST_BFF_SLOW_CHUNKS": "1000000000"},
		"a word delay without a unit":      {"TEST_BFF_WORD_DELAY": "10"},
		"a negative word delay":            {"TEST_BFF_WORD_DELAY": "-1ms"},
		"a slow delay of letters":          {"TEST_BFF_SLOW_DELAY": "slow"},
		"a slow delay with a space inside": {"TEST_BFF_SLOW_DELAY": "1 s"},
	}
	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			opts, err := options(lookupOf(env))

			require.Error(t, err)
			assert.Nil(t, opts)
			for key := range env {
				assert.ErrorContains(t, err, key, "it says which setting is wrong")
			}
		})
	}
}

func TestOptions_ValuesThatAreAccepted(t *testing.T) {
	tests := map[string]map[string]string{
		"the least number of chunks":    {"TEST_BFF_SLOW_CHUNKS": "1"},
		"the most chunks":               {"TEST_BFF_SLOW_CHUNKS": "1000000"},
		"a zero delay":                  {"TEST_BFF_WORD_DELAY": "0", "TEST_BFF_SLOW_DELAY": "0s"},
		"a delay in microseconds":       {"TEST_BFF_WORD_DELAY": "500us"},
		"a long delay":                  {"TEST_BFF_SLOW_DELAY": "1h"},
		"several units":                 {"TEST_BFF_SLOW_DELAY": "1m30s"},
		"empty values are the defaults": {"TEST_BFF_WORD_DELAY": "", "TEST_BFF_SLOW_DELAY": "", "TEST_BFF_SLOW_CHUNKS": ""},
		"the address is not an option":  {"TEST_BFF_ADDR": "not an address at all"},
	}
	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			opts, err := options(lookupOf(env))

			require.NoError(t, err)
			assert.Len(t, opts, 2)
		})
	}
}

// ask sends a message to the handler the options made, and returns what it streams.
func ask(t *testing.T, opts []testbff.Option, message string) string {
	t.Helper()
	srv := httptest.NewServer(testbff.NewHandler(opts...))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/agent",
		strings.NewReader(`{"threadId":"t","runId":"r","messages":[{"role":"user","content":"`+message+`"}]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("X-Yape-Account-Id", "a")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func TestOptions_TheValuesReachTheHandler(t *testing.T) {
	t.Run("the number of chunks of a slow reply", func(t *testing.T) {
		opts, err := options(lookupOf(map[string]string{"TEST_BFF_SLOW_CHUNKS": "3", "TEST_BFF_SLOW_DELAY": "0s", "TEST_BFF_WORD_DELAY": "0s"}))
		require.NoError(t, err)

		body := ask(t, opts, "#slow")

		assert.Contains(t, body, "tick 3")
		assert.NotContains(t, body, "tick 4")
	})
	t.Run("a single chunk", func(t *testing.T) {
		opts, err := options(lookupOf(map[string]string{"TEST_BFF_SLOW_CHUNKS": "1", "TEST_BFF_SLOW_DELAY": "0s", "TEST_BFF_WORD_DELAY": "0s"}))
		require.NoError(t, err)

		body := ask(t, opts, "#slow")

		assert.Contains(t, body, "tick 1")
		assert.NotContains(t, body, "tick 2")
	})
	t.Run("a zero delay makes the reply quick", func(t *testing.T) {
		opts, err := options(lookupOf(map[string]string{"TEST_BFF_SLOW_CHUNKS": "200", "TEST_BFF_SLOW_DELAY": "0s", "TEST_BFF_WORD_DELAY": "0s"}))
		require.NoError(t, err)

		start := time.Now()
		body := ask(t, opts, "#slow")

		assert.Contains(t, body, "tick 200")
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}

func TestHealthcheck_EdgeCases(t *testing.T) {
	status := func(code int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return srv
	}
	t.Run("only 200 is healthy", func(t *testing.T) {
		for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusForbidden, http.StatusUnauthorized} {
			assert.Equal(t, 1, healthcheck([]string{status(code).URL}), "%d", code)
		}
		assert.Equal(t, 0, healthcheck([]string{status(http.StatusOK).URL}))
	})
	t.Run("a redirect to a healthy page is followed", func(t *testing.T) {
		healthy := status(http.StatusOK)
		redirect := httptest.NewServer(http.RedirectHandler(healthy.URL, http.StatusFound))
		t.Cleanup(redirect.Close)

		assert.Equal(t, 0, healthcheck([]string{redirect.URL}))
	})
	t.Run("a redirect that never ends is unhealthy", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, srv.URL, http.StatusFound) }))
		t.Cleanup(srv.Close)

		assert.Equal(t, 1, healthcheck([]string{srv.URL}))
	})
	t.Run("an address that is no address", func(t *testing.T) {
		for _, bad := range []string{"", " ", "://nope", "http://", "http://[::1", "ftp://127.0.0.1/health", "file:///etc/passwd", "not a url", "http://exa mple.com", "\x00"} {
			assert.Equal(t, 1, healthcheck([]string{bad}), "%q", bad)
		}
	})
	t.Run("nothing listens there", func(t *testing.T) {
		assert.Equal(t, 1, healthcheck([]string{"http://127.0.0.1:1/health"}))
	})
	t.Run("a big body is not read to its end", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			chunk := []byte(strings.Repeat("x", 1<<16))
			for range 1_000 { // 64 MB, or until the client goes
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		t.Cleanup(srv.Close)

		start := time.Now()

		assert.Equal(t, 0, healthcheck([]string{srv.URL}))
		assert.Less(t, time.Since(start), 3*time.Second)
	})
	t.Run("only the first argument counts", func(t *testing.T) {
		assert.Equal(t, 0, healthcheck([]string{status(http.StatusOK).URL, "http://127.0.0.1:1/never"}))
	})
	t.Run("an answer that does not come in time is unhealthy", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { close(release); srv.Close() })
		old := healthTimeout
		healthTimeout = 100 * time.Millisecond
		t.Cleanup(func() { healthTimeout = old })

		start := time.Now()

		assert.Equal(t, 1, healthcheck([]string{srv.URL}))
		assert.Less(t, time.Since(start), 2*time.Second, "it gives up at its timeout")
	})
}

func TestTheBinaryAnswersTheTokenQuestionWithAndWithoutAnAccount(t *testing.T) {
	for name, args := range map[string][]string{
		"the account is missing": {"--account"},
		"the account is empty":   {"--account", ""},
		"extra arguments":        {"--account", "a", "b"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "go", append([]string{"run", "."}, args...)...).Output()

			require.NoError(t, err)
			assert.Equal(t, "test-token\n", string(out), "it has one token, whoever asks")
		})
	}
}

func TestTheBinaryHealthcheckExitsWithTheStatusOfTheCheck(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(down.Close)

	err := exec.CommandContext(t.Context(), "go", "run", ".", "healthcheck", down.URL).Run()

	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 1, exit.ExitCode())
}

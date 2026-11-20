package main

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lookupOf(values map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := values[k]; return v, ok }
}

func TestOptions(t *testing.T) {
	t.Run("the defaults need no settings", func(t *testing.T) {
		opts, err := options(lookupOf(nil))

		require.NoError(t, err)
		assert.Len(t, opts, 2)
	})
	t.Run("a value that is set must be valid", func(t *testing.T) {
		for key, bad := range map[string]string{
			"TEST_BFF_WORD_DELAY": "soon", "TEST_BFF_SLOW_DELAY": "-1s", "TEST_BFF_SLOW_CHUNKS": "0",
		} {
			_, err := options(lookupOf(map[string]string{key: bad}))
			require.Error(t, err, key)
			assert.Contains(t, err.Error(), key)
		}
	})
	t.Run("good values are accepted", func(t *testing.T) {
		_, err := options(lookupOf(map[string]string{"TEST_BFF_WORD_DELAY": "5ms", "TEST_BFF_SLOW_DELAY": "20ms", "TEST_BFF_SLOW_CHUNKS": "10"}))

		require.NoError(t, err)
	})
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(ok.Close)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(down.Close)

	assert.Equal(t, 0, healthcheck([]string{ok.URL}))
	assert.Equal(t, 1, healthcheck([]string{down.URL}))
	assert.Equal(t, 1, healthcheck([]string{"http://127.0.0.1:1/health"}))
}

func TestTheBinaryIsAlsoATokenProgram(t *testing.T) {
	// The Web UI asks yape-token as `yape-token --account ID`; the scripted BFF answers the same way, so that its binary can stand in
	// for it where no other program is at hand.
	out, err := exec.CommandContext(t.Context(), "go", "run", ".", "--account", "25906ac5-9f18-4099-b3fd-e1b74d8549a4").Output()

	require.NoError(t, err)
	assert.Equal(t, "test-token\n", string(out))
}

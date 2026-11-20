package chrome_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

var privacyFlags = []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}

func TestBuildCommand(t *testing.T) {
	t.Run("a pinned version with the four flags", func(t *testing.T) {
		cmd, err := chrome.BuildCommand(t.Context(), []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, privacyFlags, []string{"PATH=/bin"}, false)

		require.NoError(t, err)
		assert.Equal(t, []string{"npx", "-y", "chrome-devtools-mcp@1.10.1", "--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}, cmd.Args)
		assert.Equal(t, []string{"PATH=/bin"}, cmd.Env)
	})
	// The server is a package fetched from a registry. The project directory holds
	// the settings file with the tokens in it, and it has no business there.
	t.Run("it runs somewhere other than the project directory", func(t *testing.T) {
		here, err := os.Getwd()
		require.NoError(t, err)

		cmd, err := chrome.BuildCommand(t.Context(), []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, privacyFlags, nil, false)

		require.NoError(t, err)
		assert.NotEmpty(t, cmd.Dir, "an empty directory means the current one")
		assert.Equal(t, os.TempDir(), cmd.Dir)
		assert.NotEqual(t, here, cmd.Dir)
	})
	t.Run("extra flags are fine", func(t *testing.T) {
		_, err := chrome.BuildCommand(t.Context(), []string{"npx", "chrome-devtools-mcp@1.10.1"}, append([]string{"--viewport", "1280x800"}, privacyFlags...), nil, false)

		require.NoError(t, err)
	})
	t.Run("what it refuses", func(t *testing.T) {
		tests := []struct {
			name    string
			command []string
			args    []string
			want    []string
		}{
			{"a version that floats", []string{"npx", "-y", "chrome-devtools-mcp@latest"}, privacyFlags, []string{"must pin a version"}},
			{"no version at all", []string{"npx", "-y", "chrome-devtools-mcp"}, privacyFlags, []string{"must pin a version"}},
			{"a range", []string{"npx", "chrome-devtools-mcp@^1.10.1"}, privacyFlags, []string{"must pin a version"}},
			{"not headless", []string{"npx", "chrome-devtools-mcp@1.10.1"}, []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}, []string{"missing: --headless"}},
			{"a real profile", []string{"npx", "chrome-devtools-mcp@1.10.1"}, []string{"--headless", "--no-usage-statistics", "--no-performance-crux"}, []string{"missing: --isolated"}},
			{"usage statistics on", []string{"npx", "chrome-devtools-mcp@1.10.1"}, []string{"--headless", "--isolated", "--no-performance-crux"}, []string{"missing: --no-usage-statistics"}},
			{"CrUX on", []string{"npx", "chrome-devtools-mcp@1.10.1"}, []string{"--headless", "--isolated", "--no-usage-statistics"}, []string{"missing: --no-performance-crux"}},
			{"both wrong, both said", []string{"npx", "chrome-devtools-mcp"}, nil, []string{"must pin a version", "missing: --headless --isolated --no-usage-statistics --no-performance-crux"}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				cmd, err := chrome.BuildCommand(t.Context(), tc.command, tc.args, nil, false)

				require.Error(t, err)
				assert.Nil(t, cmd)
				for _, want := range tc.want {
					assert.ErrorContains(t, err, want)
				}
			})
		}
	})
}

func TestChildEnv(t *testing.T) {
	const canary = "CANARY-5c1e"
	environ := []string{
		// what the server needs
		"PATH=/usr/bin", "HOME=/home/me", "TMPDIR=/tmp", "LANG=en_US.UTF-8", "LC_ALL=C", "TERM=xterm", "USER=me",
		"XDG_CACHE_HOME=/home/me/.cache", "NODE_OPTIONS=--max-old-space-size=512", "NPM_CONFIG_REGISTRY=https://registry.example/",
		"npm_config_cache=/home/me/.npm", "CHROME_PATH=/opt/chrome", "PUPPETEER_EXECUTABLE_PATH=/opt/chrome",
		"HTTPS_PROXY=http://proxy:3128", "https_proxy=http://proxy:3128", "NO_PROXY=localhost", "SSL_CERT_FILE=/etc/ssl/cert.pem",
		"NVM_DIR=/home/me/.nvm", "SystemRoot=C:\\Windows",
		// what it must not get
		"HF_TOKEN=" + canary, "EVAL_PASSWORD=" + canary, "EVAL_USER=eval@example.com", "ADMIN_PASSWORD=" + canary,
		"DB_URL=postgres://u:" + canary + "@h/db", "AGENT_URL=http://agent", "OPENAI_API_KEY=" + canary,
		"NODE_AUTH_TOKEN=" + canary, "NPM_TOKEN=" + canary, "CHROME_COOKIE_JAR=" + canary, "client_secret=" + canary,
		"GOOGLE_API_KEY=" + canary, "AWS_SECRET_ACCESS_KEY=" + canary, "SESSION_COOKIE=" + canary,
		// malformed
		"NOEQUALS", "=novalue", "",
	}

	got := chrome.ChildEnv(environ)

	assert.Equal(t, []string{
		"PATH=/usr/bin", "HOME=/home/me", "TMPDIR=/tmp", "LANG=en_US.UTF-8", "LC_ALL=C", "TERM=xterm", "USER=me",
		"XDG_CACHE_HOME=/home/me/.cache", "NODE_OPTIONS=--max-old-space-size=512", "NPM_CONFIG_REGISTRY=https://registry.example/",
		"npm_config_cache=/home/me/.npm", "CHROME_PATH=/opt/chrome", "PUPPETEER_EXECUTABLE_PATH=/opt/chrome",
		"HTTPS_PROXY=http://proxy:3128", "https_proxy=http://proxy:3128", "NO_PROXY=localhost", "SSL_CERT_FILE=/etc/ssl/cert.pem",
		"NVM_DIR=/home/me/.nvm", "SystemRoot=C:\\Windows",
	}, got)
	assert.NotContains(t, strings.Join(got, "\n"), canary)
	assert.Empty(t, chrome.ChildEnv(nil))
}

func TestTailBuffer(t *testing.T) {
	t.Run("keeps the end", func(t *testing.T) {
		b := chrome.NewTailBuffer(10)

		n, err := b.Write([]byte("0123456789ABCDEF"))

		require.NoError(t, err)
		assert.Equal(t, 16, n, "it takes everything it is given")
		assert.Equal(t, "6789ABCDEF", b.String())
		_, _ = b.Write([]byte("xy"))
		assert.Equal(t, "89ABCDEFxy", b.String())
	})
	t.Run("short output is kept whole, without padding", func(t *testing.T) {
		b := chrome.NewTailBuffer(100)
		_, _ = b.Write([]byte("  warn: x\n"))

		assert.Equal(t, "warn: x", b.String())
	})
	t.Run("safe for many writers", func(t *testing.T) {
		b := chrome.NewTailBuffer(64)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				for j := range 100 {
					_, _ = fmt.Fprintf(b, "writer %d line %d\n", i, j)
					_ = b.String()
				}
			})
		}
		wg.Wait()

		assert.LessOrEqual(t, len(b.String()), 64)
	})
}

func TestBuildCommand_AHeadedRunMayGoWithoutHeadlessAndOnlyThen(t *testing.T) {
	command := []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}
	headed := []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}

	_, err := chrome.BuildCommand(t.Context(), command, headed, nil, true)
	require.NoError(t, err, "a headed run needs no --headless")

	_, err = chrome.BuildCommand(t.Context(), command, headed, nil, false)
	require.Error(t, err, "a headless run does")
	assert.Contains(t, err.Error(), "--headless")

	for _, flag := range headed {
		without := slices.DeleteFunc(slices.Clone(headed), func(a string) bool { return a == flag })
		_, err := chrome.BuildCommand(t.Context(), command, without, nil, true)
		require.Error(t, err, "even a headed run keeps %s: the profile stays a fresh one and nothing is sent", flag)
		assert.Contains(t, err.Error(), flag)
	}
}

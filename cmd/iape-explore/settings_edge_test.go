package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSettings_ThePasswordIsUsedExactlyAsGiven(t *testing.T) {
	tests := map[string]string{
		"leading spaces":       "  secret",
		"trailing spaces":      "secret  ",
		"both":                 "  se cret  ",
		"a tab at the end":     "secret\t",
		"a non-breaking space": " secret",
		"only inside":          "se cret",
		"no spaces":            "secret",
	}
	for name, password := range tests {
		t.Run(name, func(t *testing.T) {
			for _, keys := range [][2]string{{"EXPLORE_USER_EMAIL", "EXPLORE_USER_PASSWORD"}, {"ADMIN_EMAIL", "ADMIN_PASSWORD"}} {
				s, err := loadSettings(lookupOf(map[string]string{keys[0]: "qa@example.com", keys[1]: password}))

				require.NoError(t, err, keys[1])
				assert.Equal(t, password, s.Password.Reveal(), "%s is the password, spaces and all", keys[1])
			}
		})
	}
	t.Run("the email and the other settings are still trimmed", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{
			"EXPLORE_USER_EMAIL": "  qa@example.com  ", "EXPLORE_USER_PASSWORD": "x", "EXPLORE_BASE_URL": " http://localhost:9000 ",
		}))

		require.NoError(t, err)
		assert.Equal(t, "qa@example.com", s.Email)
		assert.Equal(t, "http://localhost:9000", s.BaseURL)
	})
	t.Run("a password of blanks is no password", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": "qa@example.com", "EXPLORE_USER_PASSWORD": " \t "}))

		require.ErrorContains(t, err, "EXPLORE_USER_PASSWORD")
	})
	t.Run("a password of blanks alone does not stand in for an email either", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_PASSWORD": "  ", "ADMIN_EMAIL": "a@example.com", "ADMIN_PASSWORD": "admin-secret"}))

		require.NoError(t, err, "a blank value is an unset one, so the admin is used")
		assert.Equal(t, "admin-secret", s.Password.Reveal())
	})
}

func TestLoadSettings_TheUserPairs(t *testing.T) {
	t.Run("an admin email without its password names the admin's variables", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{"ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": ""}))

		require.Error(t, err)
		assert.ErrorContains(t, err, "ADMIN_PASSWORD")
		assert.ErrorContains(t, err, "ADMIN_EMAIL")
		assert.NotContains(t, err.Error(), "EXPLORE_USER", "the message is about the pair that was given")
	})
	t.Run("an admin password without its email", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{"ADMIN_PASSWORD": "admin-secret"}))

		require.Error(t, err)
		assert.ErrorContains(t, err, "ADMIN_EMAIL")
		assert.NotContains(t, err.Error(), "admin-secret")
	})
	t.Run("the explorer's email alone is an error even when the admin pair is complete", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{
			"EXPLORE_USER_EMAIL": "qa@example.com", "ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "admin-secret",
		}))

		require.Error(t, err, "the admin's password must never be sent for another user")
		assert.ErrorContains(t, err, "EXPLORE_USER_PASSWORD")
	})
	t.Run("the explorer's password alone is an error even when the admin pair is complete", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{
			"EXPLORE_USER_PASSWORD": "qa-secret", "ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "admin-secret",
		}))

		require.Error(t, err)
		assert.ErrorContains(t, err, "EXPLORE_USER_EMAIL")
	})
	t.Run("one from each pair is an error", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": "qa@example.com", "ADMIN_PASSWORD": "admin-secret"}))

		require.Error(t, err)
	})
}

func TestLoadSettings_TheEmail(t *testing.T) {
	good := []string{"qa@example.com", "Qa+Test@Example.COM", "first.last@sub.example.co.uk", "a@b.c", "o'brien@example.com", "ñandú@example.com"}
	for _, email := range good {
		t.Run("accepts "+email, func(t *testing.T) {
			s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": email, "EXPLORE_USER_PASSWORD": "x"}))

			require.NoError(t, err)
			assert.Equal(t, email, s.Email, "the email is used as it was given: no lower case, no change")
		})
	}
	bad := []string{"qa", "@example.com", "qa@", "Qa <qa@example.com>", "qa@example.com, other@example.com", "<qa@example.com>", "qa@@example.com", "(c) qa@example.com"}
	for _, email := range bad {
		t.Run("refuses "+email, func(t *testing.T) {
			_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": email, "EXPLORE_USER_PASSWORD": "x"}))

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "x", "no password in the message")
		})
	}
}

func TestLoadSettings_BaseURLs(t *testing.T) {
	accepted := map[string]string{
		"HTTP://x":                      "http://x",
		"HTTPS://Example.COM":           "https://Example.COM",
		"http://x/app":                  "http://x/app",
		"http://x/app/":                 "http://x/app",
		"http://x/app//":                "http://x/app",
		"http://[::1]:8300":             "http://[::1]:8300",
		"http://[::1]":                  "http://[::1]",
		"http://127.0.0.1:1":            "http://127.0.0.1:1",
		"http://x:65535":                "http://x:65535",
		"https://x.example.com:8443/ui": "https://x.example.com:8443/ui",
		"http://x/a%20b":                "http://x/a%20b",
		"  http://localhost:8300  ":     "http://localhost:8300",
		"http://" + strings.Repeat("a", 60) + ".example.com": "http://" + strings.Repeat("a", 60) + ".example.com",
	}
	for raw, want := range accepted {
		t.Run("accepts "+raw, func(t *testing.T) {
			s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": raw}))

			require.NoError(t, err)
			assert.Equal(t, want, s.BaseURL)
		})
	}

	refused := map[string]string{
		"a query":                 "http://x?a=1",
		"a query after a path":    "http://x/app?a=1",
		"an empty query":          "http://x/?",
		"a fragment":              "http://x#top",
		"a fragment after a path": "http://x/app#top",
		"an empty fragment":       "http://x/#",
		"port zero":               "http://x:0",
		"port 65536":              "http://x:65536",
		"port 99999999999":        "http://x:99999999999",
		"a negative port":         "http://x:-1",
		"a port of letters":       "http://x:abc",
		"no host, a port":         "http://:8300",
		"no host":                 "http:///app",
		"a space in the host":     "http://exa mple.com",
		"a space before the path": "http://x /app",
		"a newline":               "http://x/\nfoo",
		"a scheme only":           "http://",
		"another scheme":          "file:///etc/passwd",
		"javascript":              "javascript:alert(1)",
		"no scheme":               "localhost:8300",
		"relative":                "/app",
		"a user name":             "http://user@x",
		"a user and a password":   "http://user:pw@x",
		"an empty user":           "http://@x",
		"opaque":                  "http:x",
		"a very long address":     "http://x/" + strings.Repeat("a", 5_000) + "?q=1",
	}
	for name, raw := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": raw}))

			require.Error(t, err, "%q gave %q", raw, s.BaseURL)
			assert.ErrorContains(t, err, "EXPLORE_BASE_URL")
			assert.NotContains(t, err.Error(), "pw", "an address that carries a secret is not echoed")
		})
	}

	t.Run("a very long path is only a long path", func(t *testing.T) {
		raw := "http://x/" + strings.Repeat("a", 10_000)

		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": raw}))

		require.NoError(t, err)
		assert.Equal(t, raw, s.BaseURL)
	})
	t.Run("a base URL that is refused leaves the default and the rest is still checked", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": "nope", "EXPLORE_USER_EMAIL": "qa@example.com"}))

		require.Error(t, err)
		assert.ErrorContains(t, err, "EXPLORE_BASE_URL")
		assert.ErrorContains(t, err, "EXPLORE_USER_PASSWORD", "every problem is said at once")
		assert.Equal(t, "http://localhost:8300", s.BaseURL)
	})
	t.Run("a blank value is the default", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": "   "}))

		require.NoError(t, err)
		assert.Equal(t, "http://localhost:8300", s.BaseURL)
	})
}

func TestLoadSettings_TheBrowserCommandAndItsFlags(t *testing.T) {
	t.Run("any run of spaces and tabs splits the command", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_COMMAND": "  npx   -y\tchrome-devtools-mcp@1.10.1  "}))

		require.NoError(t, err)
		assert.Equal(t, []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, s.Command)
	})
	t.Run("any run of spaces, tabs and newlines splits the flags", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_ARGS": "--isolated   --no-usage-statistics\t--no-performance-crux\n--extra"}))

		require.NoError(t, err)
		assert.Equal(t, []string{"--isolated", "--no-usage-statistics", "--no-performance-crux", "--extra"}, s.Args)
	})
	t.Run("a flag given twice is kept as given and --headless is taken out whatever the times", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{
			"EXPLORE_MCP_ARGS": "--isolated --isolated --headless --no-usage-statistics --headless --no-performance-crux --no-usage-statistics",
		}))

		require.NoError(t, err)
		assert.Equal(t, 2, strings.Count(strings.Join(s.Args, " "), "--isolated"), "a repeated flag is not the explorer's to decide on")
		assert.NotContains(t, s.browserArgs(false), "--headless")
		headless := s.browserArgs(true)
		count := 0
		for _, a := range headless {
			if a == "--headless" {
				count++
			}
		}
		assert.Equal(t, 1, count, "one --headless, however many the settings had")
		assert.Equal(t, "--headless", headless[0])
	})
	t.Run("a command that is only blanks is the default", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_COMMAND": " \t "}))

		require.NoError(t, err)
		assert.Equal(t, []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, s.Command)
	})
	t.Run("an unpinned or lookalike package is refused", func(t *testing.T) {
		for _, command := range []string{
			"npx -y chrome-devtools-mcp", "npx -y chrome-devtools-mcp@", "npx -y chrome-devtools-mcp@latest", "npx -y evil-chrome-devtools-mcp@1.10.1",
			"sh -c chrome-devtools-mcp@1.10.1",
		} {
			_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_COMMAND": command}))

			assert.Error(t, err, command)
		}
	})
	t.Run("the privacy flags cannot be left out, one at a time", func(t *testing.T) {
		for _, missing := range []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"} {
			args := []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}
			var kept []string
			for _, a := range args {
				if a != missing {
					kept = append(kept, a)
				}
			}

			_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_ARGS": strings.Join(kept, " ")}))

			require.Error(t, err, missing)
			assert.ErrorContains(t, err, missing)
		}
	})
}

func TestLoadSettings_EveryProblemIsSaidAtOnce(t *testing.T) {
	_, err := loadSettings(lookupOf(map[string]string{
		"EXPLORE_BASE_URL": "ftp://x", "EXPLORE_MCP_COMMAND": "npx chrome-devtools-mcp@latest", "EXPLORE_USER_EMAIL": "not-an-email", "EXPLORE_USER_PASSWORD": "x",
	}))

	require.Error(t, err)
	for _, want := range []string{"EXPLORE_BASE_URL", "EXPLORE_MCP_COMMAND", "plain email"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestSettings_RequireUser(t *testing.T) {
	tests := map[string]settings{
		"nothing":         {},
		"an email only":   {Email: "qa@example.com"},
		"a password only": {Password: "x"},
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			err := s.requireUser()

			require.Error(t, err)
			assert.NotContains(t, fmt.Sprint(err), "x@")
		})
	}
	assert.NoError(t, settings{Email: "qa@example.com", Password: " "}.requireUser(), "a password of a blank is a password: it is used as given")
}

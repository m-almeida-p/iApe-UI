package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lookupOf(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}

func TestLoadSettings_Defaults(t *testing.T) {
	s, err := loadSettings(lookupOf(nil))

	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8300", s.BaseURL)
	assert.Equal(t, []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, s.Command)
	assert.Equal(t, []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}, s.Args, "Chrome has a window unless the run says otherwise")
	assert.Empty(t, s.Email)
	assert.Error(t, s.requireUser(), "nobody to sign in as")
}

func TestLoadSettings_TheUserOfTheExplorerOrTheAdminOfTheWebUI(t *testing.T) {
	t.Run("the explorer's own user wins", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{
			"EXPLORE_USER_EMAIL": "qa@example.com", "EXPLORE_USER_PASSWORD": "qa-secret",
			"ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "admin-secret",
		}))

		require.NoError(t, err)
		assert.Equal(t, "qa@example.com", s.Email)
		assert.Equal(t, "qa-secret", s.Password.Reveal())
		require.NoError(t, s.requireUser())
	})
	t.Run("the first user of the Web UI is the usual account in development", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "admin-secret"}))

		require.NoError(t, err)
		assert.Equal(t, "admin@example.com", s.Email)
		require.NoError(t, s.requireUser())
	})
	t.Run("an email without its password, or the other way round, is refused", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": "qa@example.com"}))
		require.ErrorContains(t, err, "EXPLORE_USER_PASSWORD")
		_, err = loadSettings(lookupOf(map[string]string{"EXPLORE_USER_PASSWORD": "x"}))
		require.ErrorContains(t, err, "EXPLORE_USER_EMAIL")
	})
	t.Run("the email must be a plain address", func(t *testing.T) {
		_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": "Qa <qa@example.com>", "EXPLORE_USER_PASSWORD": "x"}))
		require.ErrorContains(t, err, "plain email")
	})
	t.Run("the password is in no message and no print", func(t *testing.T) {
		s, err := loadSettings(lookupOf(map[string]string{"EXPLORE_USER_EMAIL": "qa@example.com", "EXPLORE_USER_PASSWORD": "qa-secret"}))
		require.NoError(t, err)

		assert.NotContains(t, fmt.Sprintf("%v %+v %#v", s, s, s), "qa-secret")
	})
}

func TestLoadSettings_ABaseURL(t *testing.T) {
	good, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": "http://localhost:9000/"}))
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:9000", good.BaseURL, "the slash at the end is taken off")

	for _, bad := range []string{"localhost:9000", "ftp://x", "http://", "not a url", "http://user:pw@host"} {
		_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_BASE_URL": bad}))
		require.Error(t, err, bad)
		assert.NotContains(t, err.Error(), "pw@", "an address with credentials is not echoed")
	}
}

func TestLoadSettings_TheBrowserIsPinnedAndKeepsItsPrivacyFlags(t *testing.T) {
	_, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_COMMAND": "npx -y chrome-devtools-mcp@latest"}))
	require.ErrorContains(t, err, "pin")

	_, err = loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_ARGS": "--headless --isolated"}))
	require.ErrorContains(t, err, "--no-usage-statistics")

	s, err := loadSettings(lookupOf(map[string]string{
		"EXPLORE_MCP_COMMAND": "npx chrome-devtools-mcp@1.10.1", "EXPLORE_MCP_ARGS": "--isolated --no-usage-statistics --no-performance-crux",
	}))
	require.NoError(t, err, "the settings need no --headless: the run says whether Chrome has a window")
	assert.NotContains(t, s.Args, "--headless")
}

func TestSettings_BrowserArgsHaveAWindowUnlessTheRunIsHeadless(t *testing.T) {
	withHeadless, err := loadSettings(lookupOf(map[string]string{"EXPLORE_MCP_ARGS": "--headless --isolated --no-usage-statistics --no-performance-crux"}))
	require.NoError(t, err, "a settings file that has --headless still loads")
	without, err := loadSettings(lookupOf(nil))
	require.NoError(t, err)

	for _, s := range []settings{without, withHeadless} {
		assert.NotContains(t, s.browserArgs(false), "--headless", "a window is the default, whatever the settings say")
		assert.Equal(t, "--headless", s.browserArgs(true)[0], "-headless asks for none")
		assert.Subset(t, s.browserArgs(true), []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}, "and keeps the rest")
	}
	assert.Equal(t, []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}, without.Args, "the settings are not changed under whoever gave them")
}

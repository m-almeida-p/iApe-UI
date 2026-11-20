package chrome_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

func TestChildEnv_Names(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		kept  bool
	}{
		{"a name that is allowed", "PATH=/usr/bin", true},
		{"allowed names in lower case", "path=/usr/bin", true},
		{"allowed names in mixed case", "Home=/home/me", true},
		{"an allowed prefix", "NODE_OPTIONS=--x", true},
		{"an allowed prefix in lower case", "npm_config_cache=/c", true},
		{"a name that is not on the list", "FOO=bar", false},
		{"a name that only starts like one on the list", "PATHS=/x", false},
		{"a name that only ends like one on the list", "MY_PATH=/x", false},
		{"a prefix that is not one", "NODEJS_X=1", false},
		{"the prefix alone", "NODE_=1", true},

		{"password in lower case", "password=x", false},
		{"password in mixed case", "Password=x", false},
		{"a mixed case key", "Api_Key=x", false},
		{"a key with a dash", "api-key=x", false},
		{"a key with no separator", "APIKEY=x", false},
		{"a private key", "PRIVATE_KEY=x", false},
		{"a token as the whole name", "TOKEN=x", false},
		{"a token inside a longer name", "NODE_AUTH_TOKEN=x", false},
		{"a token of GitHub", "GITHUB_TOKEN=x", false},
		{"a token in the middle", "NPM_TOKEN_FOR_CI=x", false},
		{"an allowed prefix and a secret name", "NPM_TOKEN=x", false},
		{"an allowed prefix and a secret name in lower case", "npm_config_password=x", false},
		{"an allowed prefix with a registry token written as npm does", "NPM_CONFIG_//registry.npmjs.org/:_authToken=x", false},
		{"an allowed prefix with npm's _auth", "NPM_CONFIG__AUTH=x", false},
		{"an allowed prefix with a bearer", "NODE_BEARER=x", false},
		{"an allowed prefix with a passphrase", "NODE_EXTRA_PASSPHRASE=x", false},
		{"an allowed prefix with a secret", "CHROME_SECRET=x", false},
		{"an allowed prefix with a credential", "PUPPETEER_CREDENTIALS=x", false},
		{"an allowed prefix with a cookie", "CHROME_COOKIE_JAR=x", false},
		{"a secret that is on the list of names", "SHELL_PASSWD=x", false},
		{"XAUTHORITY is not npm's _auth", "XAUTHORITY=/home/me/.Xauthority", true},
		{"a name with auth inside a word is fine", "NODE_AUTHOR=me", true},

		{"a value with an equals sign", "NODE_OPTIONS=--a=b", true},
		{"an empty value", "TERM=", true},
		{"a value that looks like a secret is the caller's to keep out", "HOME=/x?token=abc", true},
		{"no equals sign", "PATH", false},
		{"no name", "=PATH", false},
		{"only an equals sign", "=", false},
		{"an empty entry", "", false},
		{"white space around the name is part of the name", " PATH=/x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chrome.ChildEnv([]string{tc.entry})

			if tc.kept {
				assert.Equal(t, []string{tc.entry}, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
	t.Run("a name given twice is passed twice, in order, for exec to take the last", func(t *testing.T) {
		got := chrome.ChildEnv([]string{"PATH=/a", "FOO=1", "PATH=/b"})

		assert.Equal(t, []string{"PATH=/a", "PATH=/b"}, got)
	})
	t.Run("the input is not changed", func(t *testing.T) {
		in := []string{"HF_TOKEN=x", "PATH=/a"}

		_ = chrome.ChildEnv(in)

		assert.Equal(t, []string{"HF_TOKEN=x", "PATH=/a"}, in)
	})
	t.Run("a very large environment", func(t *testing.T) {
		var in []string
		for i := range 10_000 {
			in = append(in, "VAR_"+strings.Repeat("x", i%50)+"=v", "NODE_TOKEN_"+strings.Repeat("y", i%50)+"=s")
		}
		in = append(in, "PATH=/usr/bin", "LANG="+strings.Repeat("z", 1<<20))

		got := chrome.ChildEnv(in)

		require.Len(t, got, 2)
		assert.Equal(t, "PATH=/usr/bin", got[0])
	})
}

func TestBuildCommand_Environment(t *testing.T) {
	command := []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}

	t.Run("the process gets the trimmed environment and not the evaluator's", func(t *testing.T) {
		cmd, err := chrome.BuildCommand(t.Context(), command, privacyFlags, []string{"PATH=/bin", "HF_TOKEN=CANARY", "NODE_AUTH_TOKEN=CANARY", "NPM_TOKEN=CANARY"}, false)

		require.NoError(t, err)
		assert.Equal(t, []string{"PATH=/bin"}, cmd.Env)
	})
	t.Run("an empty environment is an empty one, not this process's", func(t *testing.T) {
		t.Setenv("HF_TOKEN", "CANARY")

		cmd, err := chrome.BuildCommand(t.Context(), command, privacyFlags, []string{}, false)

		require.NoError(t, err)
		assert.Empty(t, cmd.Env)
		assert.NotNil(t, cmd.Env, "a nil one would be this process's environment, which exec fills in")
	})
	t.Run("the arguments of the caller are not changed", func(t *testing.T) {
		cmdArgs := append([]string{}, privacyFlags...)
		cmdCommand := make([]string, 3, 10) // room to append in place
		copy(cmdCommand, command)

		cmd, err := chrome.BuildCommand(t.Context(), cmdCommand, cmdArgs, nil, false)

		require.NoError(t, err)
		assert.Len(t, cmd.Args, 7)
		assert.Equal(t, command, cmdCommand)
		assert.Equal(t, privacyFlags, cmdArgs)
		assert.Empty(t, cmdCommand[:cap(cmdCommand)][3], "the spare room of the caller's slice stays empty")
	})
	t.Run("a command that is not pinned does not start even with a good environment", func(t *testing.T) {
		cmd, err := chrome.BuildCommand(t.Context(), []string{"npx", "chrome-devtools-mcp@latest"}, privacyFlags, []string{"PATH=/bin"}, false)

		require.Error(t, err)
		assert.Nil(t, cmd)
	})
	t.Run("a flag that undoes a privacy flag is refused", func(t *testing.T) {
		cmd, err := chrome.BuildCommand(t.Context(), command, append([]string{"--isolated=false"}, privacyFlags...), nil, false)

		require.Error(t, err)
		assert.Nil(t, cmd)
		assert.ErrorContains(t, err, "--isolated=false")
	})
}

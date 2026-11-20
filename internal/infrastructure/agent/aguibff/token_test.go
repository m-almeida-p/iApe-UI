package aguibff_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/infrastructure/agent/aguibff"
)

const account = "25906ac5-9f18-4099-b3fd-e1b74d8549a4"

var accountID = uuid.MustParse(account)

// fakeBin writes an executable shell script and returns its path.
func fakeBin(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yape-token")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700))
	return path
}

func TestCommandToken_Token(t *testing.T) {
	const secret = "tok-secret-canary-91"

	tests := []struct {
		name      string
		script    string
		want      string
		wantErr   bool
		wantInErr []string
	}{
		{name: "the token is what the program prints, without the line break", script: `printf '` + secret + `\n'`, want: secret},
		{name: "the program is asked for the account", script: `[ "$1" = "--account" ] && [ "$2" = "` + account + `" ] && printf '` + secret + `'`, want: secret},
		{name: "a failing program is an error that says why, from its own diagnostics", script: `echo '{"level":"ERROR","msg":"command failed","error":"config file not found"}' >&2; exit 1`, wantErr: true, wantInErr: []string{"config file not found"}},
		{name: "the token is never in the error, even when the program prints it and then fails", script: `printf '` + secret + `'; echo boom >&2; exit 3`, wantErr: true, wantInErr: []string{"exit status 3"}},
		{name: "no output is an error", script: `exit 0`, wantErr: true, wantInErr: []string{"printed no token"}},
		{name: "output with spaces is not a token", script: `printf 'not a token'`, wantErr: true, wantInErr: []string{"not a token"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := aguibff.CommandToken{Bin: fakeBin(t, tc.script), Timeout: 5 * time.Second}

			got, err := src.Token(t.Context(), accountID)

			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, aguibff.ErrToken)
				for _, s := range tc.wantInErr {
					assert.Contains(t, err.Error(), s)
				}
				assert.NotContains(t, err.Error(), secret, "a token never travels in an error")
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCommandToken_MissingProgramNamesIt(t *testing.T) {
	src := aguibff.CommandToken{Bin: filepath.Join(t.TempDir(), "no-such-program"), Timeout: time.Second}

	_, err := src.Token(t.Context(), accountID)

	require.ErrorIs(t, err, aguibff.ErrToken)
	assert.Contains(t, err.Error(), "no-such-program")
}

func TestCommandToken_ASlowProgramIsStopped(t *testing.T) {
	src := aguibff.CommandToken{Bin: fakeBin(t, "sleep 5"), Timeout: 100 * time.Millisecond}
	start := time.Now()

	_, err := src.Token(t.Context(), accountID)

	require.ErrorIs(t, err, aguibff.ErrToken)
	assert.Less(t, time.Since(start), 3*time.Second, "the timeout ends the program")
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "the cause is the deadline")
}

func TestCommandToken_TheCallerCanCancel(t *testing.T) {
	src := aguibff.CommandToken{Bin: fakeBin(t, "sleep 5"), Timeout: time.Minute}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := src.Token(ctx, accountID)

	require.ErrorIs(t, err, aguibff.ErrToken)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestCommandToken_NothingIsLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	src := aguibff.CommandToken{Bin: fakeBin(t, `printf 'tok-secret-canary-91'`), Timeout: time.Second}

	_, err := src.Token(t.Context(), accountID)

	require.NoError(t, err)
	assert.NotContains(t, logs.String(), "tok-secret-canary-91")
}

func TestCommandToken_RefreshSkipsTheCache(t *testing.T) {
	bin := fakeBin(t, `case "$*" in *--refresh*) printf 'fresh-token';; *) printf 'cached-token';; esac`)
	src := aguibff.CommandToken{Bin: bin, Timeout: 5 * time.Second}

	cached, err := src.Token(t.Context(), accountID)
	require.NoError(t, err)
	fresh, err := src.Refresh(t.Context(), accountID)
	require.NoError(t, err)

	assert.Equal(t, "cached-token", cached)
	assert.Equal(t, "fresh-token", fresh, "--refresh asks the identity provider again")
}

// The program is another program's: it does not get what only this server is to know (the database, the admin's password), and it does
// keep what it needs to find its own configuration and the network.
func TestCommandToken_DoesNotSeeTheServersOwnSecrets(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("DB_URL", "postgres://app:canary-db-1@h/d")
	t.Setenv("DATABASE_URL", "postgres://app:canary-db-2@h/d")
	t.Setenv("ADMIN_PASSWORD", "canary-admin-3")
	t.Setenv("POSTGRES_PASSWORD", "canary-pg-4")
	t.Setenv("AZURE_CLIENT_SECRET", "its-own-secret") // the program's own: it may need it
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("MY_OWN_SETTING", "kept")
	src := aguibff.CommandToken{Bin: fakeBin(t, `env > '`+dump+`'; printf tok`), Timeout: 5 * time.Second}

	got, err := src.Token(t.Context(), accountID)

	require.NoError(t, err)
	assert.Equal(t, "tok", got)
	seen, err := os.ReadFile(dump)
	require.NoError(t, err)
	for _, name := range []string{"DB_URL", "DATABASE_URL", "ADMIN_PASSWORD", "POSTGRES_PASSWORD"} {
		assert.NotContains(t, string(seen), name+"=", "%s is the server's own", name)
	}
	assert.NotContains(t, string(seen), "canary")
	for _, kept := range []string{"PATH=", "HTTPS_PROXY=http://proxy.example:3128", "MY_OWN_SETTING=kept", "AZURE_CLIENT_SECRET=its-own-secret"} {
		assert.Contains(t, string(seen), kept)
	}
}

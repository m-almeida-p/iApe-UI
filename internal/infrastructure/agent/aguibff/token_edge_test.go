package aguibff_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/infrastructure/agent/aguibff"
)

const secret = "tok-secret-canary-91"

func tokenFrom(t *testing.T, script string) (string, error) {
	t.Helper()
	return aguibff.CommandToken{Bin: fakeBin(t, script), Timeout: 5 * time.Second}.Token(t.Context(), accountID)
}

func TestCommandToken_WhatTheProgramPrintsIsTheTokenOrNothing(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		want    string
		wantErr string
	}{
		{name: "a CRLF line ending is trimmed", script: `printf 'abc.def\r\n'`, want: "abc.def"},
		{name: "spaces around it are trimmed", script: `printf '  abc.def \t \n\n'`, want: "abc.def"},
		{name: "blank lines around it are trimmed", script: `printf '\n\nabc.def\n\n'`, want: "abc.def"},
		{name: "a JWT is one token", script: `printf 'eyJhbGciOi.eyJzdWIiOiIxIn0.c2ln-_x\n'`, want: "eyJhbGciOi.eyJzdWIiOiIxIn0.c2ln-_x"},
		{name: "two lines are not a token: it would be a two-line header", script: `printf 'abc\ndef\n'`, wantErr: "not a token"},
		{name: "a token and a trailing note are not a token", script: `printf 'abc # expires soon\n'`, wantErr: "not a token"},
		{name: "a token with an inner CR is not a token", script: `printf 'abc\rdef'`, wantErr: "not a token"},
		{name: "a control character is not a token", script: `printf 'abc\001def'`, wantErr: "not a token"},
		{name: "an escape sequence is not a token", script: `printf '\033[31mabc\033[0m'`, wantErr: "not a token"},
		{name: "a DEL is not a token", script: `printf 'abc\177'`, wantErr: "not a token"},
		{name: "a no-break space is not a token", script: `printf 'abc\302\240def'`, wantErr: "not a token"},
		{name: "nothing is no token", script: `exit 0`, wantErr: "printed no token"},
		{name: "only whitespace is no token", script: `printf ' \t\r\n \n'`, wantErr: "printed no token"},
		{name: "only a line break is no token", script: `echo`, wantErr: "printed no token"},
		{name: "what goes to stderr is not the token", script: `printf 'abc' >&2`, wantErr: "printed no token"},
		{name: "stderr noise next to the token is ignored", script: `echo '{"level":"INFO","msg":"cache hit"}' >&2; printf 'abc'`, want: "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tokenFrom(t, tc.script)

			if tc.wantErr != "" {
				require.ErrorIs(t, err, aguibff.ErrToken)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCommandToken_AFailureSaysWhyWithoutTheTokenOrTheNoise(t *testing.T) {
	tests := []struct {
		name      string
		script    string
		wantInErr []string
		notInErr  []string
	}{
		{
			name:      "an error that talks about tokens",
			script:    `echo '{"level":"ERROR","error":"token expired for the account; log in again"}' >&2; exit 1`,
			wantInErr: []string{"token expired for the account; log in again", "exit status 1"},
		},
		{
			name:      "the last JSON line is the explanation",
			script:    `echo '{"error":"first"}' >&2; echo 'plain noise' >&2; echo '{"error":"second"}' >&2; echo 'more noise' >&2; exit 1`,
			wantInErr: []string{"second"},
			notInErr:  []string{"first", "noise"},
		},
		{
			name:     "a stderr that is not JSON is left out",
			script:   `echo 'Traceback: secret stuff' >&2; exit 2`,
			notInErr: []string{"Traceback", "secret stuff"},
		},
		{
			name:     "a JSON line without an error is left out",
			script:   `echo '{"msg":"just a log line"}' >&2; exit 2`,
			notInErr: []string{"just a log line"},
		},
		{
			name:     "the token the program printed before failing is never in the error, even if its explanation repeats it",
			script:   `printf '` + secret + `'; echo '{"error":"the token ` + secret + ` was refused"}' >&2; exit 1`,
			notInErr: []string{secret},
		},
		{
			name:     "an explanation that has control characters cannot make a second log line",
			script:   `echo '{"error":"line one\nFAKE LOG: admin logged in\r\u001b[31m"}' >&2; exit 1`,
			notInErr: []string{"\n", "\r", "\x1b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tokenFrom(t, tc.script)

			require.ErrorIs(t, err, aguibff.ErrToken)
			assert.Empty(t, got)
			for _, s := range tc.wantInErr {
				assert.Contains(t, err.Error(), s)
			}
			for _, s := range tc.notInErr {
				assert.NotContains(t, err.Error(), s)
			}
		})
	}
}

func TestCommandToken_ALongExplanationIsShortAndNeverHalfACharacter(t *testing.T) {
	for _, unit := range []string{"x", "ñ", "日", "🦤"} {
		for offset := range 4 { // shift the cut over every byte of the character
			t.Run(unit+strings.Repeat(".", offset), func(t *testing.T) {
				long := strings.Repeat(".", offset) + strings.Repeat(unit, 300)

				_, err := tokenFrom(t, `echo '{"error":"`+long+`"}' >&2; exit 1`)

				require.ErrorIs(t, err, aguibff.ErrToken)
				assert.True(t, utf8.ValidString(err.Error()), "the error is text: %q", err.Error())
				assert.Less(t, len(err.Error()), 400, "a long explanation is cut")
				assert.Contains(t, err.Error(), "…")
			})
		}
	}
}

func TestCommandToken_AShortExplanationIsNotCut(t *testing.T) {
	exactly := strings.Repeat("a", 200)

	_, err := tokenFrom(t, `echo '{"error":"`+exactly+`"}' >&2; exit 1`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), exactly)
	assert.NotContains(t, err.Error(), "…")
}

func TestCommandToken_AGiantOutputIsBoundedAndNotAToken(t *testing.T) {
	t.Run("on standard error", func(t *testing.T) {
		start := time.Now()

		_, err := tokenFrom(t, `head -c 10000000 /dev/zero | tr '\0' 'x' >&2; echo '{"error":"after the noise"}' >&2; exit 1`)

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Less(t, len(err.Error()), 1000)
		assert.Less(t, time.Since(start), 4*time.Second)
	})
	t.Run("on standard output", func(t *testing.T) {
		_, err := tokenFrom(t, `head -c 10000000 /dev/zero | tr '\0' 'x'`)

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Less(t, len(err.Error()), 1000)
		assert.NotContains(t, err.Error(), "xxxx")
	})
}

func TestCommandToken_TheProgram(t *testing.T) {
	t.Run("one that is not there", func(t *testing.T) {
		_, err := aguibff.CommandToken{Bin: filepath.Join(t.TempDir(), "nope")}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Contains(t, err.Error(), "nope")
	})
	t.Run("no program named", func(t *testing.T) {
		_, err := aguibff.CommandToken{}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, aguibff.ErrToken)
	})
	t.Run("one that is not executable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "yape-token")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf abc\n"), 0o600))

		_, err := aguibff.CommandToken{Bin: path}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Contains(t, err.Error(), "permission denied")
	})
	t.Run("one that is a directory", func(t *testing.T) {
		_, err := aguibff.CommandToken{Bin: t.TempDir()}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, aguibff.ErrToken)
	})
	t.Run("one that is not a program", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "yape-token")
		require.NoError(t, os.WriteFile(path, []byte("just text"), 0o700))

		_, err := aguibff.CommandToken{Bin: path}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, aguibff.ErrToken)
	})
	t.Run("one that is killed by a signal", func(t *testing.T) {
		_, err := tokenFrom(t, `kill -9 $$`)

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Contains(t, err.Error(), "signal")
	})
}

func TestCommandToken_AProgramThatHangsIsStopped(t *testing.T) {
	t.Run("a plain hang", func(t *testing.T) {
		start := time.Now()

		_, err := aguibff.CommandToken{Bin: fakeBin(t, "sleep 30"), Timeout: 100 * time.Millisecond}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(start), 3*time.Second)
	})
	t.Run("a hang of a child that holds the output open does not hold the caller", func(t *testing.T) {
		start := time.Now()

		_, err := aguibff.CommandToken{Bin: fakeBin(t, "sleep 8 & wait"), Timeout: 100 * time.Millisecond}.Token(t.Context(), accountID)

		require.ErrorIs(t, err, aguibff.ErrToken)
		assert.Less(t, time.Since(start), 4*time.Second, "killing the shell must not leave the caller waiting for the sleep")
	})
	t.Run("a negative timeout is not an instant failure", func(t *testing.T) {
		got, err := aguibff.CommandToken{Bin: fakeBin(t, "printf abc"), Timeout: -time.Second}.Token(t.Context(), accountID)

		require.NoError(t, err)
		assert.Equal(t, "abc", got)
	})
}

// The account is a UUID, so what the program is run with is its canonical text, whatever the caller held: two arguments, the flag and
// the account, whole.
func TestCommandToken_TheAccountIsOneArgumentInItsCanonicalForm(t *testing.T) {
	accounts := []uuid.UUID{
		accountID,
		uuid.MustParse("5E565F30-3320-4C95-8080-A56CF8F3EB2E"), // capitals: the text is in lower case
		uuid.New(),
	}
	for _, id := range accounts {
		t.Run(id.String(), func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "args")
			bin := fakeBin(t, `printf '%s\n' "$#" "$1" "$2" "$3" > '`+record+`'; printf abc`)

			got, err := aguibff.CommandToken{Bin: bin, Timeout: 5 * time.Second}.Token(t.Context(), id)

			require.NoError(t, err)
			assert.Equal(t, "abc", got)
			recorded, err := os.ReadFile(record)
			require.NoError(t, err)
			assert.Equal(t, "2\n--account\n"+strings.ToLower(id.String())+"\n\n", string(recorded))
		})
	}
}

func TestCommandToken_NoAccountIsNoToken(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	src := aguibff.CommandToken{Bin: fakeBin(t, `touch '`+ran+`'; printf abc`), Timeout: 5 * time.Second}

	_, errToken := src.Token(t.Context(), uuid.Nil())
	_, errRefresh := src.Refresh(t.Context(), uuid.Nil())

	require.ErrorIs(t, errToken, aguibff.ErrToken)
	require.ErrorIs(t, errRefresh, aguibff.ErrToken)
	assert.NoFileExists(t, ran, "the program was not asked for the token of no one")
}

func TestCommandToken_RefreshAddsTheFlagAfterTheAccount(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	bin := fakeBin(t, `printf '%s\n' "$#" "$1" "$2" "$3" > '`+record+`'; printf abc`)
	src := aguibff.CommandToken{Bin: bin, Timeout: 5 * time.Second}

	_, err := src.Refresh(t.Context(), accountID)
	require.NoError(t, err)
	refreshed, _ := os.ReadFile(record)
	_, err = src.Token(t.Context(), accountID)
	require.NoError(t, err)
	plain, _ := os.ReadFile(record)

	assert.Equal(t, "3\n--account\n"+account+"\n--refresh\n", string(refreshed))
	assert.Equal(t, "2\n--account\n"+account+"\n\n", string(plain), "Token never asks for a refresh")
}

func TestCommandToken_RefreshFailsLikeToken(t *testing.T) {
	src := aguibff.CommandToken{Bin: fakeBin(t, `echo '{"error":"idp said no"}' >&2; exit 1`), Timeout: 5 * time.Second}

	_, err := src.Refresh(t.Context(), accountID)

	require.ErrorIs(t, err, aguibff.ErrToken)
	assert.Contains(t, err.Error(), "idp said no")
}

func TestCommandToken_ManyCallersAtOnceEachGetTheToken(t *testing.T) {
	src := aguibff.CommandToken{Bin: fakeBin(t, `printf '%s' "$2"`), Timeout: 10 * time.Second}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := src.Token(t.Context(), accountID)
			assert.NoError(t, err)
			assert.Equal(t, account, got)
			got, err = src.Refresh(t.Context(), accountID)
			assert.NoError(t, err)
			assert.Equal(t, account, got)
		})
	}
	wg.Wait()
}

func TestCommandToken_ACanceledCallerNeverStartsAnything(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := fakeBin(t, `touch '`+marker+`'; printf abc`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := aguibff.CommandToken{Bin: bin}.Token(ctx, accountID)

	require.ErrorIs(t, err, aguibff.ErrToken)
	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr))
}

// Package aguibff reaches the agent through the AG-UI BFF of the Yape agent service (agui-bff): it asks the BFF for a
// turn as a Yape account, decodes the AG-UI events it streams, and carries out what the user decides on the cards.
package aguibff

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"
)

// ErrToken marks a failure to get the account's token. Its text says what failed, never what the token is.
var ErrToken = errors.New("agui-bff: no token")

// TokenSource gives the bearer token the BFF asks for, for the Yape account that is asking. A token is one account's: what is
// given for an account is never given for another.
type TokenSource interface {
	Token(ctx context.Context, account uuid.UUID) (string, error)
}

// Refresher is a TokenSource that can also get a new token when the BFF refuses the one it gave.
type Refresher interface {
	TokenSource
	Refresh(ctx context.Context, account uuid.UUID) (string, error)
}

const (
	defaultTokenTimeout = 20 * time.Second
	// maxProgramOutput is the most of a program's standard output or error that is kept. A token is a few kilobytes; a program
	// that prints more is not giving one, and its output must not fill the memory.
	maxProgramOutput = 64 << 10
	// stopGrace is how long a program that was stopped may keep the caller waiting for its output (a child can hold the pipe open).
	stopGrace = time.Second
)

// CommandToken gets the token from a program that prints it (yape-token): it is run for an account (--account), its standard output
// is the token, and it caches the token of each account until it is about to expire, so asking on every turn costs nothing.
type CommandToken struct {
	Bin string
	// Timeout is how long the program may take. Zero is twenty seconds.
	Timeout time.Duration
}

// Token runs the program. The token is the only thing read from its output; an error carries the program's own explanation
// (the "error" of the JSON line it writes to its standard error) and never its output.
func (c CommandToken) Token(ctx context.Context, account uuid.UUID) (string, error) {
	return c.run(ctx, account)
}

// Refresh is Token with the program's own cache skipped (--refresh): the identity provider is asked again. It is for a token
// the BFF refused.
func (c CommandToken) Refresh(ctx context.Context, account uuid.UUID) (string, error) {
	return c.run(ctx, account, "--refresh")
}

// serverSecretNames are the settings that only this server is to know; the token program has no use for them. Only these are named:
// the program may need a secret of its own from the environment (an identity provider's client secret), so no pattern is cut.
var serverSecretNames = map[string]bool{"DB_URL": true, "DATABASE_URL": true, "ADMIN_PASSWORD": true, "POSTGRES_PASSWORD": true}

// withoutServerSecrets is env without the server's own secrets: the database's address and the admin's and the database's
// passwords. The rest stays, because the program needs it to find its configuration, its identity provider and the network.
func withoutServerSecrets(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if serverSecretNames[strings.ToUpper(name)] {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

func (c CommandToken) run(ctx context.Context, account uuid.UUID, extra ...string) (string, error) {
	if account == uuid.Nil() { // the program would be asked for the token of no one
		return "", fmt.Errorf("%w: no account to ask for", ErrToken)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTokenTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, append([]string{"--account", account.String()}, extra...)...)
	stdout, stderr := &capture{limit: maxProgramOutput}, &capture{limit: maxProgramOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = withoutServerSecrets(os.Environ())
	cmd.WaitDelay = stopGrace
	if err := cmd.Run(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return "", fmt.Errorf("%w: %s did not finish: %w", ErrToken, c.Bin, cause)
		}
		if why := diagnostic(stderr.buf.Bytes(), strings.TrimSpace(stdout.buf.String())); why != "" {
			return "", fmt.Errorf("%w: %s failed: %w (%s)", ErrToken, c.Bin, err, why)
		}
		return "", fmt.Errorf("%w: %s failed: %w", ErrToken, c.Bin, err)
	}

	token := strings.TrimSpace(stdout.buf.String())
	switch {
	case token == "" && !stdout.over:
		return "", fmt.Errorf("%w: %s printed no token", ErrToken, c.Bin)
	case stdout.over || strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return "", fmt.Errorf("%w: %s printed something that is not a token", ErrToken, c.Bin)
	}
	return token, nil
}

// capture keeps the start of what is written to it, up to limit bytes, and says whether there was more. It never fails a write:
// a program that prints a lot is not stopped, only not remembered.
type capture struct {
	limit int
	buf   bytes.Buffer
	over  bool
}

func (c *capture) Write(p []byte) (int, error) {
	room := max(c.limit-c.buf.Len(), 0)
	if len(p) > room {
		c.over = true
		p = p[:room]
	}
	c.buf.Write(p)
	return len(p), nil
}

// diagnostic is the "error" of the last JSON line a program wrote to its standard error: its own explanation, short, on one line
// and without the token it printed, if it repeats it. Anything else it wrote is left out, because a program's output is not for an
// error message.
func diagnostic(stderr []byte, token string) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	for _, line := range slices.Backward(lines) {
		var record struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Error != "" {
			why := record.Error
			if token != "" {
				why = strings.ReplaceAll(why, token, "[token]")
			}
			why = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, why)
			return truncate(why, 200)
		}
	}
	return ""
}

// truncate is the start of s, at most n bytes, never the half of a character, with an ellipsis when it cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

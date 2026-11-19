// Package aguibff reaches the agent through the AG-UI BFF of the Yape agent service (agui-bff): it asks the BFF for a
// turn as a Yape account, decodes the AG-UI events it streams, and carries out what the user decides on the cards.
package aguibff

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ErrToken marks a failure to get the account's token. Its text says what failed, never what the token is.
var ErrToken = errors.New("agui-bff: no token")

// TokenSource gives the bearer token the BFF asks for.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Refresher is a TokenSource that can also get a new token when the BFF refuses the one it gave.
type Refresher interface {
	TokenSource
	Refresh(ctx context.Context) (string, error)
}

const defaultTokenTimeout = 20 * time.Second

// CommandToken gets the token from a program that prints it (yape-token): it is run for the account, its standard output
// is the token, and it caches the token until it is about to expire, so asking on every turn costs nothing.
type CommandToken struct {
	Bin       string
	AccountID string
	// Timeout is how long the program may take. Zero is twenty seconds.
	Timeout time.Duration
}

// Token runs the program. The token is the only thing read from its output; an error carries the program's own explanation
// (the "error" of the JSON line it writes to its standard error) and never its output.
func (c CommandToken) Token(ctx context.Context) (string, error) { return c.run(ctx) }

// Refresh is Token with the program's own cache skipped (--refresh): the identity provider is asked again. It is for a token
// the BFF refused.
func (c CommandToken) Refresh(ctx context.Context) (string, error) { return c.run(ctx, "--refresh") }

func (c CommandToken) run(ctx context.Context, extra ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(c.Timeout, defaultTokenTimeout))
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, append([]string{"--account", c.AccountID}, extra...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return "", fmt.Errorf("%w: %s did not finish: %w", ErrToken, c.Bin, cause)
		}
		if why := diagnostic(stderr.Bytes()); why != "" {
			return "", fmt.Errorf("%w: %s failed: %w (%s)", ErrToken, c.Bin, err, why)
		}
		return "", fmt.Errorf("%w: %s failed: %w", ErrToken, c.Bin, err)
	}

	token := strings.TrimSpace(stdout.String())
	switch {
	case token == "":
		return "", fmt.Errorf("%w: %s printed no token", ErrToken, c.Bin)
	case strings.ContainsFunc(token, unicode.IsSpace):
		return "", fmt.Errorf("%w: %s printed something that is not a token", ErrToken, c.Bin)
	}
	return token, nil
}

// diagnostic is the "error" of the last JSON line a program wrote to its standard error: its own explanation, short. Anything
// else it wrote is left out, because a program's output is not for an error message.
func diagnostic(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	for _, line := range slices.Backward(lines) {
		var record struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Error != "" {
			return truncate(record.Error, 200)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

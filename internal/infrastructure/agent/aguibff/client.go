package aguibff

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/infrastructure/config"
)

var _ resource.AgentGateway = (*Client)(nil)

const (
	maxErrorBody = 512
	// maxRefusalBody is how much of a refusal is read to find the service's words in it. The error carries only maxErrorBody.
	maxRefusalBody = 16 << 10
	dialTimeout    = 5 * time.Second
)

// HTTPError is a refusal by the BFF.
type HTTPError struct {
	Status int
	Body   string // the start of what the BFF said
	full   string // more of it, for the one that looks for the service's words
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("agui-bff returned HTTP %d: %s", e.Status, e.Body)
}

// Client talks to the AG-UI BFF as the Yape account of each call: a turn is for the account of the user who sends it (the id of a
// user is the id of their Yape account), and the token and the X-Yape-Account-Id header of a request are that account's.
// It implements resource.AgentGateway and resource.ActionGateway.
type Client struct {
	base    string
	timeout time.Duration
	tokens  TokenSource
	http    *http.Client
}

// NewClient makes a client for the BFF at cfg.BaseURL, with the tokens that tokens gives for each account. A nil httpClient gets a
// transport with a dial timeout.
func NewClient(cfg config.Agent, tokens TokenSource, httpClient *http.Client) *Client {
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
		transport.MaxIdleConnsPerHost = 64
		// The BFF is at the address of the settings and nowhere else: a redirect is not followed, so the token and the user's code do not
		// go to another place.
		httpClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		timeout: cfg.RequestTimeout,
		tokens:  tokens,
		http:    httpClient,
	}
}

// runInput is the AG-UI RunAgentInput the BFF reads: the thread and run ids and the user's message. Empty state, tools,
// context and props are part of the shape.
type runInput struct {
	ThreadID       string         `json:"threadId"`
	RunID          string         `json:"runId"`
	State          map[string]any `json:"state"`
	Tools          []any          `json:"tools"`
	Context        []any          `json:"context"`
	ForwardedProps map[string]any `json:"forwardedProps"`
	Messages       []message      `json:"messages"`
}

type message struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Run implements resource.AgentGateway: one turn, as a new run of the conversation's thread.
func (c *Client) Run(ctx context.Context, req domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		runCtx, cancel := c.withTimeout(ctx)
		defer cancel() // also cancels the upstream request when the caller stops iterating

		body, err := c.open(runCtx, req)
		if err != nil {
			yield(domain.AgentEvent{}, c.classify(ctx, runCtx, c.timeout, err))
			return
		}
		defer func() { _ = body.Close() }()

		for ev, err := range Decode(body) {
			if err != nil {
				yield(domain.AgentEvent{}, c.classify(ctx, runCtx, c.timeout, err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// withTimeout limits a request to the configured time. Zero is no limit.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return withLimit(ctx, c.timeout)
}

func withLimit(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, limit)
}

// maxActionTime is the longest an answer to a card is given: an answer is a call to the payment service, and a person is waiting for
// it. The limit in the settings, which is for a whole turn of the agent, may be much longer.
const maxActionTime = time.Minute

// actionLimit is the time an answer to a card is given: the configured limit, but never more than maxActionTime. No limit in the
// settings is the ceiling too.
func (c *Client) actionLimit() time.Duration {
	ceiling := maxActionTime
	if c.timeout <= 0 {
		return ceiling
	}
	return min(c.timeout, ceiling)
}

// classify tells what ended a request: the caller leaving, the time limit, or the failure itself.
// unreached wraps the failure of a request: the agent's service could not be reached, or did not answer.
func unreached(err error) error {
	return fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
}

func (c *Client) classify(caller, run context.Context, limit time.Duration, err error) error {
	switch {
	case caller.Err() != nil:
		return caller.Err()
	case run.Err() != nil:
		return fmt.Errorf("agent request timed out after %s: %w", limit, context.DeadlineExceeded)
	}
	return err
}

// errNoAccount is what a request for no account is refused with: it is not made, for it would be made as no one.
var errNoAccount = errors.New("the request is for no account")

// send makes a request with the account's token. A 401 means the token expired: it is refreshed once (when its source can) and
// the request is made again.
func (c *Client) send(ctx context.Context, account uuid.UUID, request func(token string) (*http.Response, error)) (*http.Response, error) {
	if account == uuid.Nil() {
		return nil, fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, errNoAccount)
	}
	token, err := c.tokens.Token(ctx, account)
	if err == nil && strings.TrimSpace(token) == "" {
		err = fmt.Errorf("%w: the token source gave an empty token", ErrToken)
	}
	if err != nil { // no token: nothing was sent
		return nil, fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
	}
	resp, err := request(token)
	if err != nil {
		return nil, unreached(err)
	}
	if refresher, ok := c.tokens.(Refresher); ok && resp.StatusCode == http.StatusUnauthorized {
		_ = readHTTPError(resp)
		if token, err = refresher.Refresh(ctx, account); err != nil { // a 401 is a request the service did not act on
			return nil, fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
		}
		if resp, err = request(token); err != nil {
			return nil, unreached(err)
		}
	}
	return resp, nil
}

// open sends the turn and returns the stream.
func (c *Client) open(ctx context.Context, req domain.AgentRequest) (io.ReadCloser, error) {
	resp, err := c.send(ctx, req.UserID, func(token string) (*http.Response, error) { return c.post(ctx, req, token) })
	if err != nil {
		return nil, err
	}

	// The BFF answers the stream with 201: it is a NestJS POST. Any success is a stream if it says so.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, classifyStatus(readHTTPError(resp))
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected content type %q from the BFF (expected text/event-stream)", resp.Header.Get("Content-Type"))
	}
	return resp.Body, nil
}

func (c *Client) post(ctx context.Context, req domain.AgentRequest, token string) (*http.Response, error) {
	runID := uuid.New().String()
	payload, err := json.Marshal(runInput{
		ThreadID: req.ConversationID,
		RunID:    runID,
		Messages: []message{{ID: runID, Role: "user", Content: req.Text}},
	})
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/agent", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+token)
	hr.Header.Set("X-Yape-Account-Id", req.UserID.String())
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	return c.http.Do(hr)
}

// Ping implements resource.AgentGateway: the BFF answers its health check. It needs no token.
func (c *Client) Ping(ctx context.Context) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return classifyStatus(readHTTPError(resp))
	}
	_ = resp.Body.Close()
	return nil
}

// readHTTPError reads (a bounded part of) an error response and closes it.
func readHTTPError(resp *http.Response) *HTTPError {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	full := strings.TrimSpace(string(body))
	return &HTTPError{Status: resp.StatusCode, Body: excerpt(full, maxErrorBody), full: full}
}

// excerpt is the start of s, at most n bytes and never the half of a character.
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// classifyStatus marks gateway-class failures as "agent unavailable".
func classifyStatus(e *HTTPError) error {
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, e)
	}
	return e
}

package yapeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"iape-ui/internal/platform/envcfg"
)

// refreshAfter is the part of a token's lifetime after which a new one is asked
// for, so a request never carries a token about to expire.
const refreshAfter = 0.8

// maxTokenBody bounds what is read from the token endpoint.
const maxTokenBody = 1 << 16

// TokenProvider gives the bearer token for the next gateway request.
type TokenProvider interface {
	Token(ctx context.Context) (envcfg.Secret, error)
}

// TokenSource asks the token endpoint for the anonymous bearer token and keeps
// it until most of its lifetime has passed. It is safe for concurrent use.
type TokenSource struct {
	cfg    TokenConfig
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache cachedToken
}

// cachedToken holds the token in an exported field, so that fmt and slog print
// the Secret's placeholder (the rule of internal/architecture).
type cachedToken struct {
	Token     envcfg.Secret
	RefreshAt time.Time
}

// NewTokenSource returns a source that asks cfg's endpoint. A nil client gets a
// plain one with cfg.Timeout.
func NewTokenSource(cfg TokenConfig, client *http.Client) *TokenSource {
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &TokenSource{cfg: cfg, client: client, now: time.Now}
}

// Token returns the cached token, or asks for a new one when there is none or
// it is old enough.
func (s *TokenSource) Token(ctx context.Context) (envcfg.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache.Token != "" && s.now().Before(s.cache.RefreshAt) {
		return s.cache.Token, nil
	}
	token, lifetime, err := s.fetch(ctx)
	if err != nil {
		return "", err
	}
	s.cache = cachedToken{Token: token, RefreshAt: s.now().Add(time.Duration(float64(lifetime) * refreshAfter))}
	return token, nil
}

// fetch asks the endpoint for a token with the password grant.
func (s *TokenSource) fetch(ctx context.Context) (envcfg.Secret, time.Duration, error) {
	scope := s.cfg.Scope
	if scope == "" {
		scope = "openid " + s.cfg.ClientID
	}
	grant := s.cfg.GrantType
	if grant == "" {
		grant = DefaultGrantType
	}
	form := url.Values{}
	form.Set("username", s.cfg.Username)
	form.Set("password", s.cfg.Password.Reveal())
	form.Set("grant_type", grant)
	form.Set("scope", scope)
	form.Set("client_id", s.cfg.ClientID)
	form.Set("response_type", "token")

	endpoint := strings.TrimRight(s.cfg.BaseURL, "/") + s.cfg.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, errors.New("token request: the token endpoint settings do not make a valid request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// The error of the HTTP client quotes the URL: nothing in it is secret, but
		// keep to the reason, not the text.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", 0, fmt.Errorf("token request: %w", err)
		}
		return "", 0, errors.New("token request: the token endpoint did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token request: the token endpoint answered %d%s", resp.StatusCode, errorCode(body))
	}
	var payload struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.AccessToken == "" {
		return "", 0, errors.New("token request: the answer has no access_token")
	}
	seconds, err := strconv.Atoi(strings.Trim(string(payload.ExpiresIn), `" `))
	if err != nil || seconds <= 0 {
		return "", 0, errors.New("token request: the answer has no valid expires_in")
	}
	return envcfg.Secret(payload.AccessToken), time.Duration(seconds) * time.Second, nil
}

var errorCodePattern = regexp.MustCompile(`^[a-z_]{1,40}$`)

// errorCode returns " (invalid_grant)" for an OAuth error body, and nothing for
// anything else: the description may repeat what was sent.
func errorCode(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || !errorCodePattern.MatchString(payload.Error) {
		return ""
	}
	return " (" + payload.Error + ")"
}

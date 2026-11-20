// Package auth implements signing in: checking credentials, starting and
// ending sessions, and creating the first account.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
	"iape-ui/internal/core/port/service"
)

const (
	tokenBytes       = 32
	maxTokenLen      = 128  // our tokens are 43 characters; longer input is not ours
	maxPasswordBytes = 1024 // far beyond any real password, small enough to bound hashing work
)

// Dependencies are what the auth service needs.
type Dependencies struct {
	Users      resource.UserStore
	Sessions   resource.SessionStore
	Hasher     resource.PasswordHasher
	Logger     *slog.Logger
	SessionTTL time.Duration
	Now        func() time.Time // defaults to time.Now
}

// Service implements service.Auth.
type Service struct {
	deps Dependencies
	// decoyHash lets an unknown email cost as much as a wrong password, so
	// response time does not reveal which emails have accounts.
	decoyHash string
}

var _ service.Auth = (*Service)(nil)

// NewService returns an auth service. It hashes a random decoy password once.
func NewService(d Dependencies) (*Service, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	decoy, err := d.Hasher.Hash(context.Background(), token) // at start-up nobody else is waiting for a turn
	if err != nil {
		return nil, fmt.Errorf("preparing decoy hash: %w", err)
	}
	return &Service{deps: d, decoyHash: decoy}, nil
}

// Login implements service.Auth.
func (s *Service) Login(ctx context.Context, email, password string) (service.AuthSession, error) {
	if len(password) > maxPasswordBytes {
		return service.AuthSession{}, domain.ErrInvalidCredentials
	}
	normalized, err := domain.NormalizeEmail(email)
	if err != nil {
		return service.AuthSession{}, s.refuse(ctx, password)
	}

	user, err := s.deps.Users.ByEmail(ctx, normalized)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return service.AuthSession{}, s.refuse(ctx, password)
	case err != nil:
		return service.AuthSession{}, fmt.Errorf("looking up user: %w", err)
	}
	match, err := s.deps.Hasher.Verify(ctx, password, user.PasswordHash)
	switch {
	case err != nil:
		return service.AuthSession{}, fmt.Errorf("checking the password: %w", err)
	case !match:
		return service.AuthSession{}, domain.ErrInvalidCredentials
	}

	token, err := newToken()
	if err != nil {
		return service.AuthSession{}, err
	}
	now := s.deps.Now()
	expires := now.Add(s.deps.SessionTTL)
	if err := s.deps.Sessions.Create(ctx, domain.Session{ID: digest(token), UserID: user.ID, ExpiresAt: expires}); err != nil {
		return service.AuthSession{}, fmt.Errorf("creating session: %w", err)
	}
	// Housekeeping rides on logins; a failure here must not block anyone.
	if _, err := s.deps.Sessions.DeleteExpired(ctx, now); err != nil {
		s.deps.Logger.WarnContext(ctx, "removing expired sessions failed", "error", err)
	}
	return service.AuthSession{Token: token, ExpiresAt: expires, User: user}, nil
}

// refuse is the answer for an email that has no account. It checks the password
// against the decoy hash first, so that it costs as much as a wrong password.
// When that check cannot be made the answer is the same error as for an email
// that has an account: a busy server must not tell which emails have one.
//
// Pattern: Decoy Verification
func (s *Service) refuse(ctx context.Context, password string) error {
	if _, err := s.deps.Hasher.Verify(ctx, password, s.decoyHash); err != nil {
		return fmt.Errorf("checking the password: %w", err)
	}
	return domain.ErrInvalidCredentials
}

// Authenticate implements service.Auth.
func (s *Service) Authenticate(ctx context.Context, token string) (domain.User, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenLen {
		return domain.User{}, domain.ErrInvalidCredentials
	}
	id := digest(token)

	session, err := s.deps.Sessions.Get(ctx, id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.User{}, domain.ErrInvalidCredentials
	case err != nil:
		return domain.User{}, fmt.Errorf("looking up session: %w", err)
	}
	if !s.deps.Now().Before(session.ExpiresAt) {
		s.discard(ctx, id)
		return domain.User{}, domain.ErrInvalidCredentials
	}

	user, err := s.deps.Users.ByID(ctx, session.UserID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		s.discard(ctx, id)
		return domain.User{}, domain.ErrInvalidCredentials
	case err != nil:
		return domain.User{}, fmt.Errorf("looking up user: %w", err)
	}
	return user, nil
}

// discard removes a session that can no longer be used. Failing to is only
// logged: the caller is already being refused.
func (s *Service) discard(ctx context.Context, id string) {
	if err := s.deps.Sessions.Delete(ctx, id); err != nil {
		s.deps.Logger.WarnContext(ctx, "removing a dead session failed", "error", err)
	}
}

// Logout implements service.Auth.
func (s *Service) Logout(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	if err := s.deps.Sessions.Delete(ctx, digest(token)); err != nil {
		return fmt.Errorf("ending session: %w", err)
	}
	return nil
}

// BootstrapAdmin implements service.Auth.
func (s *Service) BootstrapAdmin(ctx context.Context, account uuid.UUID, email, password string) (bool, error) {
	if account == uuid.Nil() {
		return false, &domain.ValidationError{Code: "account_missing", Field: "account", Message: "A Yape account is needed."}
	}
	email, err := domain.NormalizeEmail(email)
	if err != nil {
		return false, err
	}
	if utf8.RuneCountInString(password) < domain.MinPasswordChars {
		return false, &domain.ValidationError{
			Code: "password_short", Field: "password",
			Message: fmt.Sprintf("Use at least %d characters.", domain.MinPasswordChars),
		}
	}

	// The user of the account is already there: it is left as it is, with its password and its chats, whatever the email says now.
	if _, err := s.deps.Users.ByID(ctx, account); err == nil {
		return false, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return false, fmt.Errorf("looking for the user of the account: %w", err)
	}
	hash, err := s.deps.Hasher.Hash(ctx, password)
	if err != nil {
		return false, fmt.Errorf("hashing password: %w", err)
	}
	// The id of the user is the id of the Yape account: the history is kept by account, and the user reads what is kept for theirs.
	_, err = s.deps.Users.Create(ctx, domain.User{ID: account, Email: email, PasswordHash: hash})
	switch {
	case errors.Is(err, domain.ErrEmailTaken):
		if _, byID := s.deps.Users.ByID(ctx, account); byID == nil {
			return false, nil // another instance got there first
		}
		// The email is another user's. That user is not touched: the account's user needs an email of their own.
		return false, fmt.Errorf("%w by another user than the account's: use another email", domain.ErrEmailTaken)
	case err != nil:
		return false, fmt.Errorf("creating the user of the account: %w", err)
	}
	return true, nil
}

// newToken returns 32 random bytes as URL-safe base64: the cookie value.
func newToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// digest is what the database stores for a token: the token itself is never
// persisted, so a leaked table cannot be replayed as live sessions.
//
// Pattern: Digest Sessions
func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

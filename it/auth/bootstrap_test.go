// The user of a Yape account, made at startup, against the real database. Group A: needs Docker.
package auth_test

import (
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authcore "iape-ui/internal/core/auth"
	"iape-ui/internal/core/domain"
	authinfra "iape-ui/internal/infrastructure/auth"
	"iape-ui/internal/infrastructure/postgres"
	"iape-ui/it/internal/dbtest"
)

func TestBootstrapAdmin_TheUserOfAnAccountHasTheIDOfTheAccount(t *testing.T) {
	pool := dbtest.Pool(t)
	sql := dbtest.Statements(t)
	users := postgres.NewUserStore(pool, sql)
	svc, err := authcore.NewService(authcore.Dependencies{
		Users: users, Sessions: postgres.NewSessionStore(pool, sql), Hasher: authinfra.NewArgon2(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), SessionTTL: time.Hour,
	})
	require.NoError(t, err)
	ctx := t.Context()
	const password = "a-long-enough-password"
	ana, beto := uuid.New(), uuid.New()

	t.Run("a user that was there before the change is the one with another id", func(t *testing.T) {
		old, err := users.Create(ctx, domain.User{ID: uuid.NewV7(), Email: "admin@example.com", PasswordHash: "old-hash"})
		require.NoError(t, err)

		created, err := svc.BootstrapAdmin(ctx, ana, "admin@example.com", password)

		require.ErrorIs(t, err, domain.ErrEmailTaken, "the email is the old user's: the account's user needs another")
		assert.False(t, created)
		got, err := users.ByID(ctx, old.ID)
		require.NoError(t, err)
		assert.Equal(t, "old-hash", got.PasswordHash, "the old user is not changed")
		_, err = users.ByID(ctx, ana)
		assert.ErrorIs(t, err, domain.ErrNotFound, "and the account has no user yet")
		n, err := users.Count(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})

	t.Run("with an email of its own the user is made, with the id of the account", func(t *testing.T) {
		created, err := svc.BootstrapAdmin(ctx, ana, "ana@example.com", password)

		require.NoError(t, err)
		assert.True(t, created)
		got, err := users.ByID(ctx, ana)
		require.NoError(t, err)
		assert.Equal(t, ana, got.ID)
		assert.Equal(t, "ana@example.com", got.Email)
		session, err := svc.Login(ctx, "ana@example.com", password)
		require.NoError(t, err)
		user, err := svc.Authenticate(ctx, session.Token)
		require.NoError(t, err)
		assert.Equal(t, ana, user.ID, "who signs in is the account")
	})

	t.Run("a second start does nothing, even with another email or password", func(t *testing.T) {
		before, err := users.ByID(ctx, ana)
		require.NoError(t, err)

		for _, email := range []string{"ana@example.com", "otro@example.com"} {
			created, err := svc.BootstrapAdmin(ctx, ana, email, "another-long-password")

			require.NoError(t, err)
			assert.False(t, created)
		}
		after, err := users.ByID(ctx, ana)
		require.NoError(t, err)
		assert.Equal(t, before, after, "the user of the account is as it was")
		_, err = users.ByEmail(ctx, "otro@example.com")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("another account is another user", func(t *testing.T) {
		created, err := svc.BootstrapAdmin(ctx, beto, "beto@example.com", password)

		require.NoError(t, err)
		assert.True(t, created)
		a, err := users.ByID(ctx, ana)
		require.NoError(t, err)
		b, err := users.ByID(ctx, beto)
		require.NoError(t, err)
		assert.NotEqual(t, a.ID, b.ID)
		n, err := users.Count(ctx)
		require.NoError(t, err)
		assert.Equal(t, 3, n, "the old user, ana and beto")
	})
}

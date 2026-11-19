// Package dbtest builds on pgenv: a database with the web UI's schema and seeded users for the
// integration suites of the PostgreSQL stores. Group A: needs Docker.
package dbtest

import (
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"iape-ui/internal/infrastructure/config"
	"iape-ui/internal/infrastructure/postgres"
	"iape-ui/internal/platform/envcfg"
	"iape-ui/it/internal/pgenv"
)

// Statements loads the stores' SQL, as the composition root does at startup.
func Statements(t testing.TB) postgres.Statements {
	t.Helper()
	sql, err := postgres.LoadStatements()
	if err != nil {
		t.Fatalf("loading the stores' SQL: %v", err)
	}
	return sql
}

// Pool returns a pool on a fresh database with the web UI's schema in place.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.NewPool(t.Context(), config.DB{URL: envcfg.Secret(pgenv.NewDatabase(t)), MaxConns: 8})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SeedUser inserts a user and returns its ID. Email must already be lowercase.
func SeedUser(t testing.TB, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	_, err := pool.Exec(t.Context(),
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'test-hash')`, id, email)
	if err != nil {
		t.Fatalf("seeding user %s: %v", email, err)
	}
	return id
}

// Package pgtest gives tests a real PostgreSQL to run against.
//
// The wallet's guarantees (row locks, SKIP LOCKED, constraints) live in the database, so they can
// only be tested against one. Point WALLET_TEST_DATABASE_URL at any PostgreSQL 13+; `make
// test-db-up` starts a throwaway one in Docker.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New returns a pool on a fresh, migrated schema of the test database. Each call gets its own
// schema, so tests are isolated and can run in parallel; it is dropped when the test ends.
//
// Without WALLET_TEST_DATABASE_URL the test is skipped, unless WALLET_REQUIRE_TEST_DB is set, in
// which case a missing database is a failure (that is what `make test` does).
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("WALLET_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("WALLET_REQUIRE_TEST_DB") != "" {
			t.Fatal("WALLET_REQUIRE_TEST_DB is set but WALLET_TEST_DATABASE_URL is not")
		}
		t.Skip("WALLET_TEST_DATABASE_URL is not set: skipping the PostgreSQL tests (make test-db-up && make test)")
	}

	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}

	schema := "wallet_test_" + randomHex(8)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse the test database url: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 16

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the test schema: %v", err)
	}

	return pool
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b)
}

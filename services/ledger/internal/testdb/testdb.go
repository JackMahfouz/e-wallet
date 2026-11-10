// Package testdb gives tests a real PostgreSQL to run against.
//
// The ledger's guarantees (append-only triggers, constraints, the per-account lock) live in the
// database, so they can only be tested against one. Point LEDGER_TEST_DATABASE_URL at any
// PostgreSQL 13+; `make test-db-up` starts a throwaway one in Docker.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New returns a pool on a fresh, migrated schema of the test database. Each call gets its own
// schema, so tests are isolated and can run in parallel; it is dropped when the test ends.
//
// Without LEDGER_TEST_DATABASE_URL the test is skipped, unless LEDGER_REQUIRE_TEST_DB is set, in
// which case a missing database is a failure (that is what `make test` does).
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("LEDGER_REQUIRE_TEST_DB") != "" {
			t.Fatal("LEDGER_REQUIRE_TEST_DB is set but LEDGER_TEST_DATABASE_URL is not")
		}
		t.Skip("LEDGER_TEST_DATABASE_URL is not set: skipping the PostgreSQL tests (make test-db-up && make test)")
	}

	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}

	schema := "ledger_test_" + randomHex(8)
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
	config.MaxConns = 12

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the test schema: %v", err)
	}

	return pool
}

// UUID returns a random UUID, for account and transaction ids.
func UUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// InsertEntryAt appends an entry stamped with a chosen time, straight into the table, so tests can
// build ledgers that span months and years. The service has no such ability: it cannot backdate.
// It does not check the balance. It returns the entry's sequence number.
func InsertEntryAt(t testing.TB, pool *pgxpool.Pool, accountID string, direction domain.Direction, cents int64, at time.Time) int64 {
	t.Helper()

	var seq int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO ledger_entries (account_id, seq, transaction_id, direction, amount, created_at)
		 VALUES ($1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM ledger_entries WHERE account_id = $1), $2, $3, $4, $5)
		 RETURNING seq`,
		accountID, UUID(), string(direction), cents, at).Scan(&seq)
	if err != nil {
		t.Fatalf("insert entry: %v", err)
	}

	return seq
}

// BalanceThroughSeq sums the account's entries up to and including seq (0 for all of them),
// straight from the entries and ignoring checkpoints. It is the independent answer that a
// checkpoint-based balance must always agree with.
func BalanceThroughSeq(t testing.TB, pool *pgxpool.Pool, accountID string, seq int64) int64 {
	t.Helper()

	var balance int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END), 0)::bigint
		 FROM ledger_entries
		 WHERE account_id = $1 AND ($2 = 0 OR seq <= $2)`,
		accountID, seq).Scan(&balance)
	if err != nil {
		t.Fatalf("sum entries: %v", err)
	}

	return balance
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b)
}

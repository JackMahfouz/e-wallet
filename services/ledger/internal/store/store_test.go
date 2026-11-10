package store_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/store"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()

func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)

	return store.New(pool), pool
}

func at(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}

	return n
}

// sqlState returns the PostgreSQL error code behind err, or "" when there is none.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}

	return ""
}

// checkpointsUpTo appends every checkpoint of the period that has ended before closedBefore.
func checkpointsUpTo(t *testing.T, s *store.Store, accountID string, period domain.Period, closedBefore time.Time) int {
	t.Helper()

	var written int
	for {
		appended, err := s.AppendNextCheckpoint(ctx, accountID, period, closedBefore)
		if err != nil {
			t.Fatalf("AppendNextCheckpoint(%s): %v", period, err)
		}
		if !appended {
			return written
		}
		written++
	}
}

func listCheckpoints(t *testing.T, s *store.Store, accountID string, period domain.Period) []domain.Checkpoint {
	t.Helper()

	checkpoints, err := s.ListCheckpoints(ctx, accountID, period, 100)
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}

	return checkpoints
}

func TestMigrateIsSafeToRunAgain(t *testing.T) {
	_, pool := newStore(t)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestLedgerRefusesEveryChangeToExistingRows(t *testing.T) {
	s, pool := newStore(t)
	account := testdb.UUID()
	testdb.InsertEntryAt(t, pool, account, domain.Credit, 10000, at(2025, 1, 10, 9))
	testdb.InsertEntryAt(t, pool, account, domain.Debit, 2500, at(2025, 1, 12, 9))
	checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2025, 3, 1, 0))
	if count(t, pool, "ledger_checkpoints") != 1 {
		t.Fatal("setup: want one checkpoint")
	}

	statements := map[string]string{
		"update an entry's amount":    `UPDATE ledger_entries SET amount = amount + 1`,
		"update an entry's direction": `UPDATE ledger_entries SET direction = 'credit'`,
		"update to the same value":    `UPDATE ledger_entries SET amount = amount`,
		"delete entries":              `DELETE FROM ledger_entries`,
		"delete one entry":            `DELETE FROM ledger_entries WHERE seq = 1`,
		"truncate entries":            `TRUNCATE ledger_entries`,
		"truncate entries, cascading": `TRUNCATE ledger_entries CASCADE`,
		"update a checkpoint":         `UPDATE ledger_checkpoints SET balance = 0`,
		"delete checkpoints":          `DELETE FROM ledger_checkpoints`,
		"truncate checkpoints":        `TRUNCATE ledger_checkpoints`,
		"truncate both tables":        `TRUNCATE ledger_entries, ledger_checkpoints`,
	}

	for name, statement := range statements {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, statement)

			if got := sqlState(err); got != "23001" {
				t.Fatalf("%s: SQLSTATE = %q (err %v), want 23001 restrict_violation", statement, got, err)
			}
			if err.Error() == "" || !containsAll(err.Error(), "append-only") {
				t.Errorf("err = %v, want it to say the ledger is append-only", err)
			}
		})
	}

	if got := testdb.BalanceThroughSeq(t, pool, account, 0); got != 7500 {
		t.Errorf("balance after the refused changes = %d, want 7500", got)
	}
	if entries, checkpoints := count(t, pool, "ledger_entries"), count(t, pool, "ledger_checkpoints"); entries != 2 || checkpoints != 1 {
		t.Errorf("rows = %d entries, %d checkpoints; want 2 and 1", entries, checkpoints)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !contains(s, part) {
			return false
		}
	}

	return true
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}

	return false
}

func TestDatabaseRejectsRowsThatBreakTheLedgerRules(t *testing.T) {
	_, pool := newStore(t)
	account, transaction := testdb.UUID(), testdb.UUID()

	insertEntry := `INSERT INTO ledger_entries (account_id, seq, transaction_id, direction, amount) VALUES ($1, $2, $3, $4, $5)`
	if _, err := pool.Exec(ctx, insertEntry, account, 1, transaction, "credit", 100); err != nil {
		t.Fatalf("setup entry: %v", err)
	}

	insertCheckpoint := `INSERT INTO ledger_checkpoints (account_id, period, period_start, period_end, last_seq, balance) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := pool.Exec(ctx, insertCheckpoint, account, "month", at(2025, 1, 1, 0), at(2025, 2, 1, 0), 1, 100); err != nil {
		t.Fatalf("setup checkpoint: %v", err)
	}

	tests := map[string]struct {
		sql  string
		args []any
		code string
	}{
		"zero amount":              {insertEntry, []any{account, 2, testdb.UUID(), "credit", 0}, "23514"},
		"negative amount":          {insertEntry, []any{account, 2, testdb.UUID(), "credit", -5}, "23514"},
		"unknown direction":        {insertEntry, []any{account, 2, testdb.UUID(), "transfer", 5}, "23514"},
		"sequence zero":            {insertEntry, []any{account, 0, testdb.UUID(), "credit", 5}, "23514"},
		"account id is not a uuid": {insertEntry, []any{"alice", 2, testdb.UUID(), "credit", 5}, "22P02"},
		"a second entry 1":         {insertEntry, []any{account, 1, testdb.UUID(), "credit", 5}, "23505"},
		"the same leg twice":       {insertEntry, []any{account, 2, transaction, "credit", 5}, "23505"},

		"checkpoint: negative balance":      {insertCheckpoint, []any{account, "year", at(2025, 1, 1, 0), at(2026, 1, 1, 0), 1, -1}, "23514"},
		"checkpoint: unknown period":        {insertCheckpoint, []any{account, "week", at(2025, 1, 1, 0), at(2025, 1, 8, 0), 1, 0}, "23514"},
		"checkpoint: ends before it starts": {insertCheckpoint, []any{account, "year", at(2026, 1, 1, 0), at(2025, 1, 1, 0), 1, 0}, "23514"},
		"checkpoint: last_seq zero":         {insertCheckpoint, []any{account, "year", at(2025, 1, 1, 0), at(2026, 1, 1, 0), 0, 0}, "23514"},
		"checkpoint: the same period twice": {insertCheckpoint, []any{account, "month", at(2025, 1, 1, 0), at(2025, 2, 1, 0), 1, 100}, "23505"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)

			if got := sqlState(err); got != tc.code {
				t.Errorf("SQLSTATE = %q (err %v), want %s", got, err, tc.code)
			}
		})
	}

	// The same transaction may have one leg in each direction (a debit and its matching credit).
	if _, err := pool.Exec(ctx, insertEntry, account, 2, transaction, "debit", 50); err != nil {
		t.Errorf("the opposite leg of the same transaction was refused: %v", err)
	}
}

func TestBalance(t *testing.T) {
	t.Run("an account with no entries has a balance of zero", func(t *testing.T) {
		s, _ := newStore(t)

		balance, err := s.Balance(ctx, testdb.UUID())

		if err != nil || balance != (domain.AccountBalance{}) {
			t.Errorf("Balance = %+v, %v; want zero", balance, err)
		}
	})

	t.Run("without checkpoints it is the sum of the entries", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 10000, at(2025, 1, 1, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Debit, 2500, at(2025, 1, 2, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 300, at(2025, 1, 3, 0))

		balance, err := s.Balance(ctx, account)

		if err != nil || balance != (domain.AccountBalance{Balance: 7800, LastSeq: 3}) {
			t.Errorf("Balance = %+v, %v; want 7800 at seq 3", balance, err)
		}
	})

	t.Run("it is the latest checkpoint plus the entries after it", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 10000, at(2025, 1, 10, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Debit, 2000, at(2025, 1, 20, 0))
		checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2025, 2, 1, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 500, at(2025, 2, 5, 0))

		balance, err := s.Balance(ctx, account)

		if err != nil || balance.Balance != 8500 || balance.Balance != testdb.BalanceThroughSeq(t, pool, account, 0) {
			t.Errorf("Balance = %+v, %v; want 8500, the same as summing every entry", balance, err)
		}
	})

	t.Run("it really starts from the checkpoint rather than re-adding history", func(t *testing.T) {
		// A checkpoint is trusted, so a deliberately wrong one shows through: the balance is the
		// checkpoint plus what came after it, not a fresh sum of everything.
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 10, 0))
		_, err := pool.Exec(ctx,
			`INSERT INTO ledger_checkpoints (account_id, period, period_start, period_end, last_seq, balance)
			 VALUES ($1, 'month', $2, $3, 1, 999999)`, account, at(2025, 1, 1, 0), at(2025, 2, 1, 0))
		if err != nil {
			t.Fatalf("insert checkpoint: %v", err)
		}

		before, _ := s.Balance(ctx, account)
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 1, at(2025, 2, 2, 0))
		after, _ := s.Balance(ctx, account)

		if before.Balance != 999999 || after.Balance != 1_000_000 {
			t.Errorf("balances = %d then %d; want 999999 then 1000000 (checkpoint, then checkpoint plus the new entry)", before.Balance, after.Balance)
		}
	})

	t.Run("it uses the checkpoint that covers the most entries", func(t *testing.T) {
		// Real checkpoints agree with one another, so starting from any of them gives the same
		// total. These two deliberately disagree, which makes visible which one was used.
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 1000, at(2025, 1, 5, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 200, at(2025, 4, 5, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 30, at(2025, 4, 6, 0))
		insert := `INSERT INTO ledger_checkpoints (account_id, period, period_start, period_end, last_seq, balance)
		           VALUES ($1, $2, $3, $4, $5, $6)`
		for _, checkpoint := range []struct {
			period     string
			start, end time.Time
			lastSeq    int64
			balance    int64
		}{
			{"month", at(2025, 1, 1, 0), at(2025, 2, 1, 0), 1, 5000},
			{"quarter", at(2025, 4, 1, 0), at(2025, 7, 1, 0), 2, 7000},
		} {
			if _, err := pool.Exec(ctx, insert, account, checkpoint.period, checkpoint.start, checkpoint.end, checkpoint.lastSeq, checkpoint.balance); err != nil {
				t.Fatalf("insert checkpoint: %v", err)
			}
		}

		balance, err := s.Balance(ctx, account)

		// The newer checkpoint (seq 2, 7000) plus the one entry after it (+30). Starting from the
		// older one would give 5000 + 200 + 30 = 5230.
		if err != nil || balance.Balance != 7030 || balance.LastSeq != 3 {
			t.Errorf("Balance = %+v, %v; want 7030 at seq 3", balance, err)
		}
	})
}

func TestAppendNextCheckpoint(t *testing.T) {
	t.Run("does nothing while the period is still open", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2026, 11, 9, 12))

		appended, err := s.AppendNextCheckpoint(ctx, account, domain.PeriodMonth, at(2026, 11, 30, 0))

		if err != nil || appended || count(t, pool, "ledger_checkpoints") != 0 {
			t.Errorf("appended = %v, err = %v, checkpoints = %d; want nothing", appended, err, count(t, pool, "ledger_checkpoints"))
		}
	})

	t.Run("does nothing for an account without entries", func(t *testing.T) {
		s, _ := newStore(t)

		appended, err := s.AppendNextCheckpoint(ctx, testdb.UUID(), domain.PeriodMonth, at(2030, 1, 1, 0))

		if err != nil || appended {
			t.Errorf("appended = %v, err = %v; want false", appended, err)
		}
	})

	t.Run("closes a finished month with its bounds, last entry and balance", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 10000, at(2025, 1, 3, 9))
		testdb.InsertEntryAt(t, pool, account, domain.Debit, 2500, at(2025, 1, 20, 9))

		appended, err := s.AppendNextCheckpoint(ctx, account, domain.PeriodMonth, at(2025, 2, 1, 0))

		if err != nil || !appended {
			t.Fatalf("appended = %v, err = %v; want true", appended, err)
		}
		got := listCheckpoints(t, s, account, domain.PeriodMonth)
		if len(got) != 1 {
			t.Fatalf("got %d checkpoints, want 1", len(got))
		}
		c := got[0]
		if !c.PeriodStart.Equal(at(2025, 1, 1, 0)) || !c.PeriodEnd.Equal(at(2025, 2, 1, 0)) || c.LastSeq != 2 || c.Balance != 7500 || c.AccountID != account {
			t.Errorf("checkpoint = %+v, want January 2025, last_seq 2, balance 7500", c)
		}
	})

	t.Run("an entry exactly at the end of a period belongs to the next one", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 15, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 50, at(2025, 2, 1, 0))

		written := checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2025, 3, 1, 0))

		got := listCheckpoints(t, s, account, domain.PeriodMonth)
		if written != 2 || len(got) != 2 {
			t.Fatalf("wrote %d checkpoints (listed %d), want 2", written, len(got))
		}
		// Newest first: February holds the boundary entry, January does not.
		if got[0].LastSeq != 2 || got[0].Balance != 150 || got[1].LastSeq != 1 || got[1].Balance != 100 {
			t.Errorf("checkpoints = %+v", got)
		}
	})

	t.Run("skips months with no entries and carries the balance across the gap", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 15, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 50, at(2025, 4, 15, 0))

		written := checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2026, 1, 1, 0))

		got := listCheckpoints(t, s, account, domain.PeriodMonth)
		if written != 2 || len(got) != 2 {
			t.Fatalf("wrote %d checkpoints, want 2 (January and April only)", written)
		}
		if !got[0].PeriodStart.Equal(at(2025, 4, 1, 0)) || got[0].Balance != 150 || !got[1].PeriodStart.Equal(at(2025, 1, 1, 0)) {
			t.Errorf("checkpoints = %+v", got)
		}
	})

	t.Run("closes several owed periods oldest first, one per call, then reports none due", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 10, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 2, 10, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Debit, 30, at(2025, 3, 10, 0))
		closedBefore := at(2025, 4, 1, 0)

		var balances []int64
		for i := 0; i < 3; i++ {
			appended, err := s.AppendNextCheckpoint(ctx, account, domain.PeriodMonth, closedBefore)
			if err != nil || !appended {
				t.Fatalf("call %d: appended = %v, err = %v; want true", i+1, appended, err)
			}
			balances = append(balances, listCheckpoints(t, s, account, domain.PeriodMonth)[0].Balance)
		}
		appended, err := s.AppendNextCheckpoint(ctx, account, domain.PeriodMonth, closedBefore)

		if err != nil || appended {
			t.Errorf("a fourth call: appended = %v, err = %v; want false", appended, err)
		}
		if balances[0] != 100 || balances[1] != 200 || balances[2] != 170 {
			t.Errorf("balances after each call = %v, want [100 200 170]", balances)
		}
	})

	t.Run("an entry stamped before the previous checkpoint ended lands in the next period", func(t *testing.T) {
		// If the clock ever steps backwards an entry can be older than the checkpoint before it.
		// It must not be pushed into a period that is already closed.
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 10, 0))
		checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2025, 3, 1, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 50, at(2025, 1, 31, 0))

		written := checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2025, 3, 1, 0))

		got := listCheckpoints(t, s, account, domain.PeriodMonth)
		if written != 1 || len(got) != 2 {
			t.Fatalf("wrote %d more checkpoints (%d in total), want 1 (2 in total)", written, len(got))
		}
		if !got[0].PeriodStart.Equal(at(2025, 2, 1, 0)) || got[0].Balance != 150 || got[0].LastSeq != 2 {
			t.Errorf("checkpoint = %+v, want February with balance 150 at seq 2", got[0])
		}
	})

	t.Run("a checkpoint still equals its entries when timestamps are out of order", func(t *testing.T) {
		// seq 2 is stamped in February but seq 3 (appended later) is stamped in January, as if the
		// clock had stepped back. The January checkpoint covers up to seq 3 and so must include seq 2.
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 10, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 20, at(2025, 2, 5, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 3, at(2025, 1, 20, 0))

		checkpointsUpTo(t, s, account, domain.PeriodMonth, at(2025, 2, 1, 0))

		got := listCheckpoints(t, s, account, domain.PeriodMonth)
		if len(got) != 1 || got[0].LastSeq != 3 || got[0].Balance != 123 {
			t.Fatalf("checkpoints = %+v, want one covering seq 3 with balance 123", got)
		}
		balance, err := s.Balance(ctx, account)
		if err != nil || balance.Balance != 123 {
			t.Errorf("Balance = %+v, %v; want 123: no entry may be lost or counted twice", balance, err)
		}
	})

	t.Run("each period type keeps its own chain, and every chain agrees with the entries", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 1000, at(2024, 12, 20, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 200, at(2025, 2, 10, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Debit, 300, at(2025, 5, 5, 0))
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 50, at(2026, 3, 3, 0))
		closedBefore := at(2030, 6, 1, 0)

		for _, period := range domain.AllPeriods {
			checkpointsUpTo(t, s, account, period, closedBefore)
		}

		type closing struct {
			start   time.Time
			end     time.Time
			lastSeq int64
			balance int64
		}
		want := map[domain.Period][]closing{
			domain.PeriodMonth: {
				{at(2024, 12, 1, 0), at(2025, 1, 1, 0), 1, 1000},
				{at(2025, 2, 1, 0), at(2025, 3, 1, 0), 2, 1200},
				{at(2025, 5, 1, 0), at(2025, 6, 1, 0), 3, 900},
				{at(2026, 3, 1, 0), at(2026, 4, 1, 0), 4, 950},
			},
			domain.PeriodQuarter: {
				{at(2024, 10, 1, 0), at(2025, 1, 1, 0), 1, 1000},
				{at(2025, 1, 1, 0), at(2025, 4, 1, 0), 2, 1200},
				{at(2025, 4, 1, 0), at(2025, 7, 1, 0), 3, 900},
				{at(2026, 1, 1, 0), at(2026, 4, 1, 0), 4, 950},
			},
			domain.PeriodYear: {
				{at(2024, 1, 1, 0), at(2025, 1, 1, 0), 1, 1000},
				{at(2025, 1, 1, 0), at(2026, 1, 1, 0), 3, 900},
				{at(2026, 1, 1, 0), at(2027, 1, 1, 0), 4, 950},
			},
			domain.PeriodFiveYear: {
				{at(2020, 1, 1, 0), at(2025, 1, 1, 0), 1, 1000},
				{at(2025, 1, 1, 0), at(2030, 1, 1, 0), 4, 950},
			},
		}

		for period, closings := range want {
			got := listCheckpoints(t, s, account, period) // newest first
			if len(got) != len(closings) {
				t.Errorf("%s: %d checkpoints, want %d", period, len(got), len(closings))
				continue
			}
			for i, w := range closings {
				c := got[len(got)-1-i] // oldest first
				if !c.PeriodStart.Equal(w.start) || !c.PeriodEnd.Equal(w.end) || c.LastSeq != w.lastSeq || c.Balance != w.balance {
					t.Errorf("%s #%d = [%v, %v) seq %d balance %d; want [%v, %v) seq %d balance %d",
						period, i+1, c.PeriodStart, c.PeriodEnd, c.LastSeq, c.Balance, w.start, w.end, w.lastSeq, w.balance)
				}
				if sum := testdb.BalanceThroughSeq(t, pool, account, c.LastSeq); sum != c.Balance {
					t.Errorf("%s #%d: balance %d, but the entries up to seq %d add up to %d", period, i+1, c.Balance, c.LastSeq, sum)
				}
			}
		}
	})

	t.Run("two checkpointers at once never write the same period twice", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		for month := time.January; month <= time.June; month++ {
			testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, month, 10, 0))
		}

		results := make(chan int, 4)
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			go func() {
				written := 0
				for {
					appended, err := s.AppendNextCheckpoint(ctx, account, domain.PeriodMonth, at(2025, 12, 1, 0))
					if err != nil {
						errs <- err
						return
					}
					if !appended {
						results <- written
						return
					}
					written++
				}
			}()
		}

		total := 0
		for i := 0; i < 4; i++ {
			select {
			case written := <-results:
				total += written
			case err := <-errs:
				t.Fatalf("a checkpointer failed: %v", err)
			}
		}
		if total != 6 || count(t, pool, "ledger_checkpoints") != 6 {
			t.Errorf("checkpointers wrote %d in total (%d rows), want exactly 6", total, count(t, pool, "ledger_checkpoints"))
		}
	})
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)

	return out
}

func TestAccountsPendingCheckpoint(t *testing.T) {
	s, pool := newStore(t)
	untouched, current, behind := testdb.UUID(), testdb.UUID(), testdb.UUID()
	for _, account := range []string{untouched, current, behind} {
		testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 1, 10, 0))
	}
	checkpointsUpTo(t, s, current, domain.PeriodMonth, at(2025, 2, 1, 0))
	checkpointsUpTo(t, s, behind, domain.PeriodMonth, at(2025, 2, 1, 0))
	testdb.InsertEntryAt(t, pool, behind, domain.Credit, 100, at(2025, 3, 10, 0))

	month, err := s.AccountsPendingCheckpoint(ctx, domain.PeriodMonth)
	if err != nil {
		t.Fatalf("AccountsPendingCheckpoint: %v", err)
	}
	year, err := s.AccountsPendingCheckpoint(ctx, domain.PeriodYear)
	if err != nil {
		t.Fatalf("AccountsPendingCheckpoint: %v", err)
	}

	if got, want := sorted(month), sorted([]string{untouched, behind}); !equal(got, want) {
		t.Errorf("pending for month = %v, want %v (not the account whose month checkpoint is up to date)", got, want)
	}
	if got, want := sorted(year), sorted([]string{untouched, current, behind}); !equal(got, want) {
		t.Errorf("pending for year = %v, want every account: none has a yearly checkpoint yet", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestListEntries(t *testing.T) {
	s, pool := newStore(t)
	account, other := testdb.UUID(), testdb.UUID()
	for i := 0; i < 5; i++ {
		testdb.InsertEntryAt(t, pool, account, domain.Credit, int64(100+i), at(2025, 1, 1+i, 0))
	}
	testdb.InsertEntryAt(t, pool, other, domain.Credit, 7, at(2025, 1, 1, 0))

	page := func(afterSeq int64, limit int) []int64 {
		entries, err := s.ListEntries(ctx, account, afterSeq, limit)
		if err != nil {
			t.Fatalf("ListEntries: %v", err)
		}
		var seqs []int64
		for _, e := range entries {
			if e.AccountID != account {
				t.Errorf("entry %d belongs to another account", e.Seq)
			}
			seqs = append(seqs, e.Seq)
		}

		return seqs
	}

	tests := map[string]struct {
		afterSeq int64
		limit    int
		want     []int64
	}{
		"the first page":       {0, 3, []int64{1, 2, 3}},
		"the next page":        {3, 10, []int64{4, 5}},
		"after the last entry": {5, 10, nil},
		"a limit of one":       {1, 1, []int64{2}},
		"in the middle":        {2, 2, []int64{3, 4}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := page(tc.afterSeq, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestListEntriesReturnsStoredFields(t *testing.T) {
	s, pool := newStore(t)
	account := testdb.UUID()
	testdb.InsertEntryAt(t, pool, account, domain.Debit, 1999, at(2025, 3, 4, 5))

	entries, err := s.ListEntries(ctx, account, 0, 10)

	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, err = %v", entries, err)
	}
	e := entries[0]
	if e.Direction != domain.Debit || e.Amount != 1999 || e.Seq != 1 || !e.CreatedAt.Equal(at(2025, 3, 4, 5)) || e.CreatedAt.Location() != time.UTC || e.ID == "" || e.TransactionID == "" {
		t.Errorf("entry = %+v", e)
	}
}

func TestListCheckpoints(t *testing.T) {
	s, pool := newStore(t)
	account := testdb.UUID()
	testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2025, 2, 10, 0))
	testdb.InsertEntryAt(t, pool, account, domain.Credit, 100, at(2026, 3, 3, 0))
	for _, period := range domain.AllPeriods {
		checkpointsUpTo(t, s, account, period, at(2030, 6, 1, 0))
	}

	t.Run("newest first, and the finest period first when several end together", func(t *testing.T) {
		got := listCheckpoints(t, s, account, "")

		type closing struct {
			period domain.Period
			end    time.Time
		}
		var order []closing
		for _, c := range got {
			order = append(order, closing{c.Period, c.PeriodEnd})
		}
		want := []closing{
			{domain.PeriodFiveYear, at(2030, 1, 1, 0)},
			{domain.PeriodYear, at(2027, 1, 1, 0)},    // 2026
			{domain.PeriodMonth, at(2026, 4, 1, 0)},   // March 2026 and Q1 2026 end together:
			{domain.PeriodQuarter, at(2026, 4, 1, 0)}, // the finest period comes first
			{domain.PeriodYear, at(2026, 1, 1, 0)},    // 2025
			{domain.PeriodQuarter, at(2025, 4, 1, 0)},
			{domain.PeriodMonth, at(2025, 3, 1, 0)},
		}
		if len(order) != len(want) {
			t.Fatalf("got %d checkpoints %v, want %d", len(order), order, len(want))
		}
		for i := range want {
			if order[i] != want[i] {
				t.Errorf("checkpoint %d = %v, want %v (all: %v)", i, order[i], want[i], order)
			}
		}
	})

	t.Run("filters by period", func(t *testing.T) {
		for _, period := range domain.AllPeriods {
			for _, c := range listCheckpoints(t, s, account, period) {
				if c.Period != period {
					t.Errorf("asked for %s, got a %s checkpoint", period, c.Period)
				}
			}
		}
		if got := listCheckpoints(t, s, account, domain.PeriodMonth); len(got) != 2 {
			t.Errorf("got %d monthly checkpoints, want 2", len(got))
		}
	})

	t.Run("honours the limit and other accounts see nothing", func(t *testing.T) {
		limited, err := s.ListCheckpoints(ctx, account, "", 2)
		if err != nil || len(limited) != 2 {
			t.Errorf("limited = %d, err = %v; want 2", len(limited), err)
		}
		if got := listCheckpoints(t, s, testdb.UUID(), ""); len(got) != 0 {
			t.Errorf("a stranger sees %d checkpoints", len(got))
		}
	})
}

func TestWithAccountLock(t *testing.T) {
	t.Run("commits when the callback succeeds and rolls back when it fails", func(t *testing.T) {
		s, pool := newStore(t)
		account := testdb.UUID()
		newEntry := domain.NewEntry{AccountID: account, TransactionID: testdb.UUID(), Direction: domain.Credit, Amount: 100}
		boom := errors.New("boom")

		err := s.WithAccountLock(ctx, account, func(tx domain.AccountTx) error {
			if _, err := tx.Append(ctx, newEntry); err != nil {
				return err
			}

			return boom
		})
		if !errors.Is(err, boom) || count(t, pool, "ledger_entries") != 0 {
			t.Fatalf("err = %v, entries = %d; want boom and the append rolled back", err, count(t, pool, "ledger_entries"))
		}

		err = s.WithAccountLock(ctx, account, func(tx domain.AccountTx) error {
			_, err := tx.Append(ctx, newEntry)

			return err
		})
		if err != nil || count(t, pool, "ledger_entries") != 1 {
			t.Errorf("err = %v, entries = %d; want the append committed", err, count(t, pool, "ledger_entries"))
		}
	})

	t.Run("lets one writer into an account at a time", func(t *testing.T) {
		s, _ := newStore(t)
		account := testdb.UUID()
		holding, release, secondEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})

		go func() {
			_ = s.WithAccountLock(ctx, account, func(domain.AccountTx) error {
				close(holding)
				<-release

				return nil
			})
		}()
		<-holding

		go func() {
			_ = s.WithAccountLock(ctx, account, func(domain.AccountTx) error {
				close(secondEntered)

				return nil
			})
		}()

		select {
		case <-secondEntered:
			t.Fatal("a second writer got into the account while the first still held it")
		case <-time.After(300 * time.Millisecond):
		}

		close(release)
		select {
		case <-secondEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("the second writer never got in after the first finished")
		}
	})

	t.Run("does not make other accounts wait", func(t *testing.T) {
		s, _ := newStore(t)
		holding, release, otherEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})

		go func() {
			_ = s.WithAccountLock(ctx, testdb.UUID(), func(domain.AccountTx) error {
				close(holding)
				<-release

				return nil
			})
		}()
		<-holding
		defer close(release)

		go func() {
			_ = s.WithAccountLock(ctx, testdb.UUID(), func(domain.AccountTx) error {
				close(otherEntered)

				return nil
			})
		}()

		select {
		case <-otherEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("a different account was blocked by the first account's lock")
		}
	})
}

//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestTightenSandboxLifetimeDeadline_OnlyBringsItEarlier pins technical
// plan §35.2's rule for the exact value a sandbox-agent reports:
// TightenSandboxLifetimeDeadline keeps the earlier of the row's deadline
// and now() plus the report, so a report never moves a deadline later; a
// report that would change nothing writes nothing -- no row affected, not
// even updated_at -- so a heartbeat every 30 seconds costs no row version;
// a report of a gen that is not live changes nothing; and the live gen
// that has no deadline of its own (a gen the previous binary spawned, or a
// row stamped with none) takes the report as its deadline, recorded
// against it, with lifetime_seconds cleared.
//
// Every report runs in a transaction that then reads now(), the same
// instant the statement used, so each expected deadline is exact.
func TestTightenSandboxLifetimeDeadline_OnlyBringsItEarlier(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)

	type setup int
	const (
		stamped        setup = iota // gen 1, the claim's estimate: 2h
		previousBinary              // gen 2 the previous binary spawned: gen 1's deadline left
		noDeadline                  // gen 1 stamped with no lifetime
	)
	tests := []struct {
		name      string
		setup     setup
		pin       *int32 // set the deadline to now() plus this, in the report's own transaction
		gen       int32
		remaining int32
		// wantTightened is the statement's verdict; when it is false the
		// row, updated_at included, must be exactly as it was.
		wantTightened   bool
		wantFromNow     time.Duration // the deadline, from the report's now()
		wantSeconds     *int32
		wantDeadlineGen int32
	}{
		{name: "an earlier report tightens", setup: stamped, gen: 1, remaining: 3600,
			wantTightened: true, wantFromNow: time.Hour, wantSeconds: ptrInt32(7200), wantDeadlineGen: 1},
		{name: "a report of zero ends it now", setup: stamped, gen: 1, remaining: 0,
			wantTightened: true, wantFromNow: 0, wantSeconds: ptrInt32(7200), wantDeadlineGen: 1},
		{name: "a later report changes nothing", setup: stamped, gen: 1, remaining: 9000},
		{name: "an equal report affects no row", setup: stamped, pin: ptrInt32(1800), gen: 1, remaining: 1800},
		{name: "a stale gen's report changes nothing", setup: stamped, gen: 0, remaining: 60},
		{name: "a future gen's report changes nothing", setup: stamped, gen: 2, remaining: 60},
		{name: "a gen the previous binary spawned takes the report", setup: previousBinary, gen: 2, remaining: 9000,
			wantTightened: true, wantFromNow: 9000 * time.Second, wantSeconds: nil, wantDeadlineGen: 2},
		{name: "a gen with no deadline takes the report", setup: noDeadline, gen: 1, remaining: 600,
			wantTightened: true, wantFromNow: 10 * time.Minute, wantSeconds: nil, wantDeadlineGen: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			var lifetime *int32
			if tc.setup != noDeadline {
				lifetime = ptrInt32(7200)
			}
			tokenHash := "token-hash-tighten"
			if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: lifetime}); err != nil {
				t.Fatalf("seed the sandbox: %v", err)
			}
			if tc.setup == previousBinary {
				// The previous binary's UpsertSandboxForSpawn: gen bumped,
				// the lifetime columns left at gen 1's.
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = gen + 1, status = 'spawning', last_seen_at = now(), updated_at = now() WHERE session_id = $1`, sessionID); err != nil {
					t.Fatalf("the previous binary's respawn: %v", err)
				}
			}

			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if tc.pin != nil {
				if _, err := tx.Exec(ctx, `UPDATE sandboxes SET lifetime_deadline_at = now() + make_interval(secs => $2) WHERE session_id = $1`, sessionID, *tc.pin); err != nil {
					t.Fatalf("pin the deadline: %v", err)
				}
			}
			before := readTightenRow(ctx, t, tx, sessionID)
			tightened, err := store.WithTx(tx).TightenLifetimeDeadline(ctx, sessionID, tc.gen, tc.remaining)
			if err != nil {
				t.Fatalf("TightenLifetimeDeadline: %v", err)
			}
			after := readTightenRow(ctx, t, tx, sessionID)
			var now time.Time
			if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
				t.Fatalf("read the transaction's now(): %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}

			if tightened != tc.wantTightened {
				t.Fatalf("TightenLifetimeDeadline(gen %d, %ds) = %v, want %v", tc.gen, tc.remaining, tightened, tc.wantTightened)
			}
			if !tc.wantTightened {
				if after != before {
					t.Fatalf("the row changed though no row was affected:\n before %+v\n after  %+v", before, after)
				}
				return
			}
			if !after.deadline.Valid || !after.deadline.Time.Equal(now.Add(tc.wantFromNow)) {
				t.Errorf("lifetime_deadline_at = %v, want now() plus %v = %v", after.deadline, tc.wantFromNow, now.Add(tc.wantFromNow))
			}
			if before.deadline.Valid && before.deadlineGen.Valid && before.deadlineGen.Int32 == before.gen && !after.deadline.Time.Before(before.deadline.Time) {
				t.Errorf("lifetime_deadline_at = %v, not earlier than the live gen's own %v", after.deadline.Time, before.deadline.Time)
			}
			if !equalInt32Ptr(int4Ptr(after.seconds), tc.wantSeconds) {
				t.Errorf("lifetime_seconds = %v, want %v", derefInt32(int4Ptr(after.seconds)), derefInt32(tc.wantSeconds))
			}
			if !after.deadlineGen.Valid || after.deadlineGen.Int32 != tc.wantDeadlineGen || after.gen != tc.wantDeadlineGen {
				t.Errorf("lifetime_deadline_gen = %v at gen %d, want the live gen %d", after.deadlineGen, after.gen, tc.wantDeadlineGen)
			}
			if !after.updatedAt.Equal(now) {
				t.Errorf("updated_at = %v, want the report's now() %v", after.updatedAt, now)
			}
		})
	}
}

// tightenRow is the part of a sandbox row TightenSandboxLifetimeDeadline
// writes, comparable with ==.
type tightenRow struct {
	gen                  int32
	deadline             pgtype.Timestamptz
	seconds, deadlineGen pgtype.Int4
	updatedAt            time.Time
}

func readTightenRow(ctx context.Context, t *testing.T, q pgx.Tx, sessionID pgtype.UUID) tightenRow {
	t.Helper()
	var row tightenRow
	if err := q.QueryRow(ctx, `SELECT gen, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen, updated_at FROM sandboxes WHERE session_id = $1`, sessionID).
		Scan(&row.gen, &row.deadline, &row.seconds, &row.deadlineGen, &row.updatedAt); err != nil {
		t.Fatalf("read the sandbox's lifetime columns: %v", err)
	}
	return row
}

func int4Ptr(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	return &v.Int32
}

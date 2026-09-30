//go:build integration

package outboxworker_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// lockRow takes a row lock on an outbox row in a transaction of its own,
// so the worker's next write to that row waits for it.
func lockRow(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Errorf("begin the locking transaction: %v", err)
		return nil
	}
	if _, err := tx.Exec(context.Background(), `SELECT id FROM outbox WHERE id = $1 FOR UPDATE`, id); err != nil {
		_ = tx.Rollback(context.Background())
		t.Errorf("lock the row: %v", err)
		return nil
	}
	return tx
}

// waitForBlockedOutboxWrite returns once a backend on this database is
// waiting on a lock with an outbox UPDATE: the worker's write is in flight
// and held.
func waitForBlockedOutboxWrite(pool *pgxpool.Pool) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query LIKE '%UPDATE outbox%'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("no outbox write ever waited on the lock")
}

// TestShutdown_OutcomeWriteTheShutdownOvertakesLands pins the ordering the
// shutdown state's two readings cannot see: the delivery has returned, the
// state read not begun, and the shutdown begins while the outcome's write
// is in flight -- held here on a row lock, so it is. The outcome is
// already known and is recorded all the same, with no error: a delivery
// that succeeded is marked delivered, never left to be delivered again once
// its claim lapses, and a failure is recorded as the counted failure it
// was.
func TestShutdown_OutcomeWriteTheShutdownOvertakesLands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    ports.NotificationKind
		result  error
		checkFn func(t *testing.T, got sqlcgen.Outbox)
	}{
		{
			name: "a delivery that succeeded",
			kind: ports.NotificationKindGitHub,
			checkFn: func(t *testing.T, got sqlcgen.Outbox) {
				if got.Status != sqlcgen.OutboxStatusDelivered {
					t.Fatalf("status = %q, want delivered: left pending, the comment would be posted again", got.Status)
				}
			},
		},
		{
			name:   "a delivery that failed on its own merits",
			kind:   ports.NotificationKindGitHub,
			result: errors.New("remote answered 500"),
			checkFn: func(t *testing.T, got sqlcgen.Outbox) {
				requireRow(t, got, want{attempts: 1, interrupted: 0, backedOff: true, lastErrorHas: "remote answered 500"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := IntegrationTestPool(t)
			store := narvipg.NewOutboxStore(pool, false)
			logs := captureLogs(t)
			row := seedOutboxEntry(ctx, t, store, string(tc.kind), map[string]any{"text": "hi"})

			p := newProcess(t)
			held := make(chan pgx.Tx, 1)
			notifier := &deliveryFunc{fn: func(context.Context) error {
				// Returns with the row locked elsewhere: the outcome's
				// write will wait for it.
				held <- lockRow(t, pool, row.ID)
				return tc.result
			}}
			var shutdown errgroup.Group
			shutdown.Go(func() error {
				tx := <-held
				if tx == nil {
					return errors.New("the row was never locked")
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if err := waitForBlockedOutboxWrite(pool); err != nil {
					return err
				}
				p.shutDown()
				return tx.Rollback(context.Background())
			})

			builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{tc.kind: notifier}, platform.DefaultTimeouts(), p.state)
			if err := builder.PumpOnce(p.ctx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}
			if err := shutdown.Wait(); err != nil {
				t.Fatal(err)
			}
			if !p.state.Begun() {
				t.Fatal("the shutdown never began: the ordering under test did not happen")
			}
			tc.checkFn(t, getRow(t, store, row.ID))
			logs.requireNoErrorLogs(t)
		})
	}
}

// TestShutdown_OutcomeBoundStartsAtTheShutdown pins when
// OutboxShutdownRecordTimeout starts to run: at the shutdown, never at the
// tick's first write. With no shutdown, a write late in a long tick -- the
// second row delivered well past the bound after the first -- still
// lands, where a bound counted from the first write would already have
// cancelled it and left a delivered row to be delivered again.
func TestShutdown_OutcomeBoundStartsAtTheShutdown(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	logs := captureLogs(t)
	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxShutdownRecordTimeout = 200 * time.Millisecond

	first := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-1"})
	second := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-2"})
	if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() - interval '1 minute' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("order the batch: %v", err)
	}

	p := newProcess(t)
	notifier := &deliveryFunc{}
	notifier.fn = func(context.Context) error {
		if notifier.callCount() == 2 {
			time.Sleep(2 * timeouts.OutboxShutdownRecordTimeout)
		}
		return nil
	}
	builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: notifier}, timeouts, p.state)
	if err := builder.PumpOnce(p.ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	for _, id := range []pgtype.UUID{first.ID, second.ID} {
		if got := getRow(t, store, id); got.Status != sqlcgen.OutboxStatusDelivered {
			t.Fatalf("row %s status = %q, want delivered", id.String(), got.Status)
		}
	}
	logs.requireNoErrorLogs(t)
}

// TestShutdown_OutcomeWritesAreBoundedAfterTheShutdown pins that the bound
// is applied: a write after the shutdown that cannot complete -- held on a
// row lock far longer than OutboxShutdownRecordTimeout -- is given up once
// the bound has passed, so the tick, and the process waiting for it,
// never outlast the shutdown by more than that bound.
func TestShutdown_OutcomeWritesAreBoundedAfterTheShutdown(t *testing.T) {
	ctx := context.Background()
	pool := IntegrationTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	captureLogs(t)
	timeouts := platform.DefaultTimeouts()
	timeouts.OutboxShutdownRecordTimeout = 300 * time.Millisecond
	const lockHeld = 3 * time.Second

	row := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "blob-1"})

	p := newProcess(t)
	var locks errgroup.Group
	notifier := &deliveryFunc{fn: func(ctx context.Context) error {
		tx := lockRow(t, pool, row.ID)
		locks.Go(func() error {
			if tx == nil {
				return errors.New("the row was never locked")
			}
			time.Sleep(lockHeld)
			return tx.Rollback(context.Background())
		})
		return p.cutShort(ctx)
	}}
	builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: notifier}, timeouts, p.state)

	start := time.Now()
	if err := builder.PumpOnce(p.ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	took := time.Since(start)
	if err := locks.Wait(); err != nil {
		t.Fatal(err)
	}
	if took >= lockHeld-time.Second {
		t.Fatalf("PumpOnce returned after %v with its write held for %v: OutboxShutdownRecordTimeout (%v) was not applied", took, lockHeld, timeouts.OutboxShutdownRecordTimeout)
	}
}

// TestShutdown_DeliveryTimeIsMeasuredFromTheDelivery pins where the
// outlived-delivery-timeout rule starts its clock: when this delivery
// started, never the row's creation nor the batch's claim. A row created
// long ago, and a row whose turn in the batch came after slower
// deliveries, each cut short at once, keep their attempt.
func TestShutdown_DeliveryTimeIsMeasuredFromTheDelivery(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before is how many rows of the same batch are delivered, each
		// taking slowEach, ahead of the one the shutdown cuts short.
		before   int
		slowEach time.Duration
		// age backdates the interrupted row's creation.
		age time.Duration
	}{
		{name: "a row created long before its delivery", age: 10 * time.Minute},
		{name: "a row whose turn came after slower deliveries in its batch", before: 2, slowEach: 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := IntegrationTestPool(t)
			store := narvipg.NewOutboxStore(pool, false)
			logs := captureLogs(t)
			timeouts := platform.DefaultTimeouts()
			timeouts.OutboxDeliveryTimeout = 500 * time.Millisecond

			for i := range tc.before {
				ahead := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": fmt.Sprintf("ahead-%d", i)})
				if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() - make_interval(mins => $2::int) WHERE id = $1`, ahead.ID, 10-i); err != nil {
					t.Fatalf("order the batch: %v", err)
				}
			}
			row := seedOutboxEntry(ctx, t, store, string(repeatableKind), map[string]any{"key": "interrupted"})
			if tc.age > 0 {
				if _, err := pool.Exec(ctx, `UPDATE outbox SET created_at = now() - make_interval(secs => $2::float8) WHERE id = $1`, row.ID, tc.age.Seconds()); err != nil {
					t.Fatalf("backdate the row: %v", err)
				}
			}

			p := newProcess(t)
			notifier := &deliveryFunc{}
			notifier.fn = func(ctx context.Context) error {
				if notifier.callCount() <= tc.before {
					time.Sleep(tc.slowEach)
					return nil
				}
				return p.cutShort(ctx)
			}
			builder := newShutdownTestBuilder(t, pool, store, map[ports.NotificationKind]ports.Notifier{repeatableKind: notifier}, timeouts, p.state)
			if err := builder.PumpOnce(p.ctx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}
			if notifier.callCount() != tc.before+1 {
				t.Fatalf("deliveries = %d, want %d", notifier.callCount(), tc.before+1)
			}
			requireRow(t, getRow(t, store, row.ID), want{attempts: 0, interrupted: 1, settling: true})
			logs.requireShutdownWarning(t, row.ID, "shutdown_interrupted")
			logs.requireNoErrorLogs(t)
		})
	}
}

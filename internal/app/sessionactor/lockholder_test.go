package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/platform"
)

// TestStatementLost proves which failed lock-connection statements presume
// the connection lost (statementLost, lockholder.go), and so close it, stop
// every actor locked on it and release all their locks: only a connection
// that is gone -- closed, a statement past its bound, an error that is not
// the server's, or a FATAL or PANIC from the server. A server-side ERROR,
// such as the shared lock table being full, leaves the backend and its
// locks as they were, and must not. Treating every error as a loss fails
// the ERROR rows.
func TestStatementLost(t *testing.T) {
	t.Parallel()

	serverErr := func(severity, unlocalized, code string) error {
		return fmt.Errorf("sessionactor: try advisory lock: %w",
			&pgconn.PgError{Severity: severity, SeverityUnlocalized: unlocalized, Code: code, Message: "injected"})
	}
	for _, tc := range []struct {
		name         string
		connClosed   bool
		boundExpired bool
		err          error
		want         bool
	}{
		{"a server ERROR on a live connection: out of shared memory", false, false, serverErr("ERROR", "ERROR", "53200"), false},
		{"a server ERROR from a server that sends no unlocalized severity", false, false, serverErr("ERROR", "", "53200"), false},
		{"a localized ERROR is read from the unlocalized severity", false, false, serverErr("FEHLER", "ERROR", "57014"), false},
		{"a server FATAL: the backend was terminated", false, false, serverErr("FATAL", "FATAL", "57P01"), true},
		{"a localized FATAL is read from the unlocalized severity", false, false, serverErr("SCHWERWIEGEND", "FATAL", "57P01"), true},
		{"a server FATAL from a server that sends no unlocalized severity", false, false, serverErr("FATAL", "", "57P01"), true},
		{"a server PANIC", false, false, serverErr("PANIC", "PANIC", "XX000"), true},
		{"a network error is not the server's", false, false, &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}, true},
		{"an unexpected EOF is not the server's", false, false, fmt.Errorf("sessionactor: probe lock connection: %w", io.ErrUnexpectedEOF), true},
		{"the connection is closed, whatever the error", true, false, serverErr("ERROR", "ERROR", "53200"), true},
		{"the statement overran its bound, whatever the error", false, true, serverErr("ERROR", "ERROR", "57014"), true},
		{"the statement overran its bound: the context's own error", false, true, context.DeadlineExceeded, true},
		{"an error with no cause at all is not the server's", false, false, errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statementLost(tc.connClosed, tc.boundExpired, tc.err); got != tc.want {
				t.Fatalf("statementLost(closed=%v, boundExpired=%v, %v) = %v, want %v", tc.connClosed, tc.boundExpired, tc.err, got, tc.want)
			}
		})
	}
}

// TestLockHolder_UnlockWaitsItsTurnOnlyForTheCurrentGeneration pins
// Unlock's wait for the lock connection's mutex (sem), held here as a
// running statement would hold it. An unlock of the current generation
// waits its turn whatever its context -- an unlock that has to happen is
// never skipped because its caller's bound ran out -- while an unlock of an
// earlier generation returns at once, without queueing: its lock went with
// that generation's connection. The holder has no connection and dials
// none (its pool connects lazily), so an unlock that gets its turn is a
// no-op.
func TestLockHolder_UnlockWaitsItsTurnOnlyForTheCurrentGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{"an earlier generation's unlock returns at once, without waiting its turn", true},
		{"the current generation's unlock waits its turn, its context done or not", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, "postgres://narvi@127.0.0.1:1/narvi")
			if err != nil {
				t.Fatalf("pgxpool.New: %v", err)
			}
			t.Cleanup(pool.Close)
			h := newLockHolder(ctx, pool, platform.DefaultTimeouts(), nil)
			t.Cleanup(h.Close)

			gen := h.currentGen()
			if tc.stale {
				h.gen.Add(1) // as a loss bumps it
			}
			doneCtx, cancel := context.WithCancel(ctx)
			cancel()

			h.sem <- struct{}{}
			returned := make(chan struct{})
			var unlocking errgroup.Group
			unlocking.Go(func() error {
				defer close(returned)
				h.Unlock(doneCtx, pgtype.UUID{Valid: true}, gen)
				return nil
			})
			var returnedWhileHeld bool
			select {
			case <-returned:
				returnedWhileHeld = true
			case <-time.After(200 * time.Millisecond):
			}
			h.leave()
			_ = unlocking.Wait()

			switch {
			case tc.stale && !returnedWhileHeld:
				t.Fatal("an earlier generation's Unlock waited for the lock connection's mutex")
			case !tc.stale && returnedWhileHeld:
				t.Fatal("the current generation's Unlock returned while the lock connection's mutex was held: it skipped its turn")
			}
		})
	}
}

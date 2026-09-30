//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestTimerStore_ArmedAtMovesOnlyWhenArmed pins session_timers.armed_at
// (migration 000153), the instant technical plan §2 ages a kind the session
// actor does not know from: the insert and every re-arm (Upsert) set it to
// the database's now(), while the pump's claim (Claim) and the backoff
// (PostponeIfArmedAt) move fires_at alone. created_at keeps the first
// insert. The two guarded writes hold only while the row carries the
// armed_at the caller read: after a re-arm, the stale value moves and
// deletes nothing, and the current one does.
func TestTimerStore_ArmedAtMovesOnlyWhenArmed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	timers := narvipg.NewTimerStore(pool)
	const name = "a_kind_from_a_newer_binary"
	at := func(d time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: time.Now().Add(d).Truncate(time.Microsecond), Valid: true}
	}
	get := func() sqlcgen.SessionTimer {
		t.Helper()
		row, err := timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: name})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return row
	}

	armed, err := timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: sessionID, Name: name, FiresAt: at(time.Minute)})
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if !armed.ArmedAt.Valid || !armed.ArmedAt.Time.Equal(armed.CreatedAt.Time) {
		t.Fatalf("armed_at at the insert = %v, want its created_at %v", armed.ArmedAt, armed.CreatedAt.Time)
	}
	if _, err := pool.Exec(ctx, `UPDATE session_timers
		SET armed_at = armed_at - interval '1 hour', created_at = created_at - interval '1 hour'
		WHERE session_id = $1 AND name = $2`, sessionID, name); err != nil {
		t.Fatal(err)
	}
	old := get()

	claimed, err := timers.Claim(ctx, sqlcgen.ClaimDueTimerParams{FiresAt: at(30 * time.Second), SessionID: sessionID, Name: name})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed.ArmedAt.Time.Equal(old.ArmedAt.Time) {
		t.Fatalf("armed_at after a claim = %v, want %v: a claim is not an arm", claimed.ArmedAt.Time, old.ArmedAt.Time)
	}

	backoff := at(10 * time.Minute)
	if n, err := timers.PostponeIfArmedAt(ctx, sqlcgen.PostponeSessionTimerIfArmedAtParams{FiresAt: backoff, SessionID: sessionID, Name: name, ArmedAt: old.ArmedAt}); err != nil || n != 1 {
		t.Fatalf("postpone with the current armed_at = %d, %v; want 1 row", n, err)
	}
	if got := get(); !got.FiresAt.Time.Equal(backoff.Time) || !got.ArmedAt.Time.Equal(old.ArmedAt.Time) {
		t.Fatalf("after the backoff fires_at = %v, armed_at = %v; want %v and %v, unmoved", got.FiresAt.Time, got.ArmedAt.Time, backoff.Time, old.ArmedAt.Time)
	}

	rearmed, err := timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: sessionID, Name: name, FiresAt: at(time.Hour)})
	if err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if !rearmed.ArmedAt.Time.After(old.ArmedAt.Time.Add(59 * time.Minute)) {
		t.Fatalf("armed_at after a re-arm = %v, want about now, not the hour-old %v", rearmed.ArmedAt.Time, old.ArmedAt.Time)
	}
	if !rearmed.CreatedAt.Time.Equal(old.CreatedAt.Time) {
		t.Fatalf("created_at after a re-arm = %v, want the first insert's %v", rearmed.CreatedAt.Time, old.CreatedAt.Time)
	}

	if n, err := timers.PostponeIfArmedAt(ctx, sqlcgen.PostponeSessionTimerIfArmedAtParams{FiresAt: at(20 * time.Minute), SessionID: sessionID, Name: name, ArmedAt: old.ArmedAt}); err != nil || n != 0 {
		t.Fatalf("postpone with a stale armed_at = %d, %v; want 0 rows", n, err)
	}
	if n, err := timers.DeleteIfArmedAt(ctx, sqlcgen.DeleteSessionTimerIfArmedAtParams{SessionID: sessionID, Name: name, ArmedAt: old.ArmedAt}); err != nil || n != 0 {
		t.Fatalf("delete with a stale armed_at = %d, %v; want 0 rows", n, err)
	}
	if got := get(); !got.FiresAt.Time.Equal(rearmed.FiresAt.Time) || !got.ArmedAt.Time.Equal(rearmed.ArmedAt.Time) {
		t.Fatalf("after the stale writes fires_at = %v, armed_at = %v; want the re-arm's %v and %v", got.FiresAt.Time, got.ArmedAt.Time, rearmed.FiresAt.Time, rearmed.ArmedAt.Time)
	}

	if n, err := timers.DeleteIfArmedAt(ctx, sqlcgen.DeleteSessionTimerIfArmedAtParams{SessionID: sessionID, Name: name, ArmedAt: rearmed.ArmedAt}); err != nil || n != 1 {
		t.Fatalf("delete with the current armed_at = %d, %v; want 1 row", n, err)
	}
	if n, err := timers.DeleteIfArmedAt(ctx, sqlcgen.DeleteSessionTimerIfArmedAtParams{SessionID: sessionID, Name: name, ArmedAt: rearmed.ArmedAt}); err != nil || n != 0 {
		t.Fatalf("delete of a row already gone = %d, %v; want 0 rows", n, err)
	}
}

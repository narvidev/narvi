//go:build integration

package sessionactor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves the bounds on a timer kind this binary does not know
// (technical plan §2; handleUnknownTimer, DecideUnknownTimer) on real
// Postgres, through the real timer pump and the real session actor. No
// test waits for real time to pass: a row's age is set directly
// (armTimerAged), and advanceTimer moves one timer's clock forward by
// dating both its fires_at and its created_at back -- exactly what the
// passing of that much time does to the row, relative to the database's
// now().

// newerBinaryKind is a timer kind this binary does not know: one a newer
// binary would arm.
const newerBinaryKind = "a_kind_from_a_newer_binary"

// The three log lines handleUnknownTimer writes, one per delivery.
const (
	unknownTimerKeptMsg      = "sessionactor: ignoring TimerFired with unknown name"
	unknownTimerBackedOffMsg = "sessionactor: timer kind this binary does not know backed off"
	unknownTimerDeletedMsg   = "sessionactor: timer kind this binary does not know deleted"
)

// unknownTimerMetric is the counter handleUnknownTimer records.
const unknownTimerMetric = "session_timer_unknown_kind_total"

// clockSlack absorbs the difference between the two clocks a timer's due
// instant is read on: the pump claims on the replica's clock (time.Now()
// plus TimerClaimDuration) and polls on the database's (fires_at <=
// now()), which in a container can drift from the host's by a little. It
// is two orders of magnitude below UnknownTimerBackoff, so it never lets a
// backed-off timer pass for one at the claim cadence.
const clockSlack = 2 * time.Second

// armTimerAged arms name on sessionID due a second ago, its row created
// age ago on the database's clock.
func armTimerAged(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, name string, age time.Duration) {
	t.Helper()
	if _, err := narvipg.NewTimerStore(pool).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      name,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true},
	}); err != nil {
		t.Fatalf("arm timer %q: %v", name, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE session_timers SET created_at = now() - make_interval(secs => $3::double precision)
		 WHERE session_id = $1 AND name = $2`,
		sessionID, name, age.Seconds(),
	); err != nil {
		t.Fatalf("date timer %q back by %v: %v", name, age, err)
	}
}

// advanceTimer moves the clock of one timer forward by d: its fires_at and
// its created_at both d earlier, as d passing would leave them relative to
// the database's now().
func advanceTimer(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, name string, d time.Duration) {
	t.Helper()
	tag, err := pool.Exec(ctx,
		`UPDATE session_timers
		 SET fires_at = fires_at - make_interval(secs => $3::double precision),
		     created_at = created_at - make_interval(secs => $3::double precision)
		 WHERE session_id = $1 AND name = $2`,
		sessionID, name, d.Seconds(),
	)
	if err != nil {
		t.Fatalf("advance timer %q by %v: %v", name, d, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("advance timer %q by %v: %d rows, want 1", name, d, tag.RowsAffected())
	}
}

// timerRow reads name's row on sessionID; ok is false when there is none.
func timerRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, name string) (sqlcgen.SessionTimer, bool) {
	t.Helper()
	row, err := narvipg.NewTimerStore(pool).Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.SessionTimer{}, false
	}
	if err != nil {
		t.Fatalf("get timer %q: %v", name, err)
	}
	return row, true
}

// dbNow is the database's now().
func dbNow(ctx context.Context, t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("read the database's now(): %v", err)
	}
	return now
}

// logLines counts the log lines in buf whose msg is msg and whose name is
// name.
func logLines(t *testing.T, buf *syncLogBuffer, msg, name string) int {
	t.Helper()
	n := 0
	for _, line := range bytes.Split([]byte(buf.String()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == msg && entry["name"] == name {
			n++
		}
	}
	return n
}

// unknownTimerLines counts every line handleUnknownTimer wrote for name.
func unknownTimerLines(t *testing.T, buf *syncLogBuffer, name string) int {
	t.Helper()
	return logLines(t, buf, unknownTimerKeptMsg, name) +
		logLines(t, buf, unknownTimerBackedOffMsg, name) +
		logLines(t, buf, unknownTimerDeletedMsg, name)
}

// waitForLogLines waits until buf holds want lines of msg naming name.
func waitForLogLines(t *testing.T, buf *syncLogBuffer, msg, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := logLines(t, buf, msg, name)
		if got == want {
			return
		}
		if got > want || time.Now().After(deadline) {
			t.Fatalf("%d log lines %q naming %q, want %d; full log output:\n%s", got, msg, name, want, buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// unknownTimerCounts reads session_timer_unknown_kind_total by action.
func unknownTimerCounts(ctx context.Context, t *testing.T) (backedOff, deleted int64) {
	t.Helper()
	return readCounterSumByAttr(ctx, t, otelReader, unknownTimerMetric, "action", UnknownTimerBackOff.String()),
		readCounterSumByAttr(ctx, t, otelReader, unknownTimerMetric, "action", UnknownTimerDelete.String())
}

func newUnknownTimerRegistry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, timeouts platform.Timeouts) *Registry {
	t.Helper()
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	return r
}

// TestUnknownTimer_InsideTheGraceComesBackWithinOneClaimWindow proves a
// kind this binary does not know, on a row younger than UnknownTimerGrace,
// is handled as it always was: the actor writes nothing, so the row keeps
// the fires_at the pump's claim gave it -- at most one TimerClaimDuration
// ahead -- and the pump delivers it again once that window has passed. It
// is not counted. An implementation that backed it off inside the grace
// would leave it UnknownTimerBackoff ahead, and the second delivery would
// not happen.
func TestUnknownTimer_InsideTheGraceComesBackWithinOneClaimWindow(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	timeouts := platform.DefaultTimeouts()
	r := newUnknownTimerRegistry(ctx, t, pool, timeouts)

	// A minute short of the grace, so a backoff applied anywhere inside it,
	// even only near its end, is caught; the deploy test below covers a row
	// just armed.
	armTimerAged(ctx, t, pool, sessionID, newerBinaryKind, timeouts.UnknownTimerGrace-time.Minute)
	backedOffBefore, deletedBefore := unknownTimerCounts(ctx, t)

	beforeClaim := time.Now()
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	afterClaim := time.Now()
	waitForLogLines(t, logs, unknownTimerKeptMsg, newerBinaryKind, 1)

	row, ok := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	if !ok {
		t.Fatalf("timer %q is gone after its delivery, want it kept inside the grace", newerBinaryKind)
	}
	if earliest, latest := beforeClaim.Add(timeouts.TimerClaimDuration), afterClaim.Add(timeouts.TimerClaimDuration); row.FiresAt.Time.Before(earliest) || row.FiresAt.Time.After(latest) {
		t.Fatalf("fires_at after the delivery = %v, want the claim's own, within [%v, %v]: the actor moved a timer it must leave at the claim cadence",
			row.FiresAt.Time, earliest, latest)
	}

	// One claim window later, the pump delivers it again.
	advanceTimer(ctx, t, pool, sessionID, newerBinaryKind, timeouts.TimerClaimDuration+clockSlack)
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after one claim window: %v", err)
	}
	waitForLogLines(t, logs, unknownTimerKeptMsg, newerBinaryKind, 2)

	if got := unknownTimerLines(t, logs, newerBinaryKind); got != 2 {
		t.Errorf("%d lines about %q, want only the two deliveries, both kept", got, newerBinaryKind)
	}
	if backedOff, deleted := unknownTimerCounts(ctx, t); backedOff != backedOffBefore || deleted != deletedBefore {
		t.Errorf("%s moved (backed_off %+d, deleted %+d) inside the grace, want no count", unknownTimerMetric, backedOff-backedOffBefore, deleted-deletedBefore)
	}
}

// TestUnknownTimer_PastTheGraceComesBackAtTheBackoff proves a kind this
// binary does not know, on a row older than UnknownTimerGrace, is re-armed
// UnknownTimerBackoff ahead of the database's now() at each delivery,
// logged at WARN with its name and counted: one claim window later it is
// not delivered, one backoff later it is.
func TestUnknownTimer_PastTheGraceComesBackAtTheBackoff(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	timeouts := platform.DefaultTimeouts()
	r := newUnknownTimerRegistry(ctx, t, pool, timeouts)

	// Just past the grace.
	armTimerAged(ctx, t, pool, sessionID, newerBinaryKind, timeouts.UnknownTimerGrace+time.Minute)
	backedOffBefore, deletedBefore := unknownTimerCounts(ctx, t)

	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	waitForLogLines(t, logs, unknownTimerBackedOffMsg, newerBinaryKind, 1)

	row, ok := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	if !ok {
		t.Fatalf("timer %q is gone after its delivery, want it backed off: it is younger than the deletion bound", newerBinaryKind)
	}
	now := dbNow(ctx, t, pool)
	if earliest, latest := now.Add(timeouts.UnknownTimerBackoff-10*time.Second), now.Add(timeouts.UnknownTimerBackoff); row.FiresAt.Time.Before(earliest) || row.FiresAt.Time.After(latest) {
		t.Fatalf("fires_at after the delivery = %v, want the database's now plus UnknownTimerBackoff, within [%v, %v]", row.FiresAt.Time, earliest, latest)
	}
	if backedOff, _ := unknownTimerCounts(ctx, t); backedOff-backedOffBefore != 1 {
		t.Errorf("%s{action=backed_off} moved by %d, want 1", unknownTimerMetric, backedOff-backedOffBefore)
	}

	// One claim window later it is not due: the pump leaves it alone.
	advanceTimer(ctx, t, pool, sessionID, newerBinaryKind, timeouts.TimerClaimDuration+clockSlack)
	before, _ := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after one claim window: %v", err)
	}
	after, _ := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	if !after.FiresAt.Time.Equal(before.FiresAt.Time) {
		t.Fatalf("fires_at moved from %v to %v one claim window after a backoff: the pump claimed it at the claim cadence", before.FiresAt.Time, after.FiresAt.Time)
	}

	// One backoff later it is delivered, and backed off again.
	advanceTimer(ctx, t, pool, sessionID, newerBinaryKind, timeouts.UnknownTimerBackoff)
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after the backoff: %v", err)
	}
	waitForLogLines(t, logs, unknownTimerBackedOffMsg, newerBinaryKind, 2)

	if got := unknownTimerLines(t, logs, newerBinaryKind); got != 2 {
		t.Errorf("%d lines about %q, want only the two backed-off deliveries", got, newerBinaryKind)
	}
	if backedOff, deleted := unknownTimerCounts(ctx, t); backedOff-backedOffBefore != 2 || deleted != deletedBefore {
		t.Errorf("%s moved by backed_off %+d, deleted %+d; want 2 and 0", unknownTimerMetric, backedOff-backedOffBefore, deleted-deletedBefore)
	}
}

// TestUnknownTimer_GoneAfterTheDeletionBoundWithOneWarning follows one kind
// this binary does not know through its whole life: kept while its row is
// younger than the grace, backed off past it, and deleted at its first
// delivery past UnknownTimerDeleteAfter, with exactly one WARN naming it
// and one count. Nothing is delivered after that.
func TestUnknownTimer_GoneAfterTheDeletionBoundWithOneWarning(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	timeouts := platform.DefaultTimeouts()
	r := newUnknownTimerRegistry(ctx, t, pool, timeouts)

	armTimerAged(ctx, t, pool, sessionID, newerBinaryKind, 0)
	_, deletedBefore := unknownTimerCounts(ctx, t)

	for _, step := range []struct {
		name    string
		advance time.Duration
		wantMsg string
	}{
		{"just armed", 0, unknownTimerKeptMsg},
		{"past the grace", timeouts.UnknownTimerGrace, unknownTimerBackedOffMsg},
		// Past the grace but still inside the deletion bound: backed off
		// again, never deleted early.
		{"just inside the deletion bound", timeouts.UnknownTimerDeleteAfter - timeouts.UnknownTimerGrace - timeouts.UnknownTimerBackoff - time.Minute, unknownTimerBackedOffMsg},
		{"past the deletion bound", timeouts.UnknownTimerBackoff + 2*time.Minute, unknownTimerDeletedMsg},
	} {
		if step.advance > 0 {
			advanceTimer(ctx, t, pool, sessionID, newerBinaryKind, step.advance)
		}
		want := logLines(t, logs, step.wantMsg, newerBinaryKind) + 1
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatalf("%s: PumpOnce: %v", step.name, err)
		}
		waitForLogLines(t, logs, step.wantMsg, newerBinaryKind, want)
		_, exists := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
		if wantExists := step.wantMsg != unknownTimerDeletedMsg; exists != wantExists {
			t.Fatalf("%s: timer %q exists = %v after its delivery, want %v", step.name, newerBinaryKind, exists, wantExists)
		}
	}

	// Gone: later ticks find nothing to deliver.
	for range 2 {
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce after the deletion: %v", err)
		}
	}
	if _, exists := timerRow(ctx, t, pool, sessionID, newerBinaryKind); exists {
		t.Fatalf("timer %q is back after its deletion", newerBinaryKind)
	}
	if got := logLines(t, logs, unknownTimerDeletedMsg, newerBinaryKind); got != 1 {
		t.Errorf("%d deletion warnings naming %q, want exactly 1", got, newerBinaryKind)
	}
	for _, msg := range []string{unknownTimerKeptMsg, unknownTimerBackedOffMsg} {
		if got := logLines(t, logs, msg, newerBinaryKind); got < 1 {
			t.Errorf("no %q line naming %q: the kind skipped a stage of its life", msg, newerBinaryKind)
		}
	}
	if _, deleted := unknownTimerCounts(ctx, t); deleted-deletedBefore != 1 {
		t.Errorf("%s{action=deleted} moved by %d, want 1", unknownTimerMetric, deleted-deletedBefore)
	}
}

// TestUnknownTimer_NewerReplicasTimerSurvivesADeploy is the rolling-deploy
// guarantee: a kind a newer replica armed during a deploy, claimed and
// delivered first by an older replica that does not know it, is still
// there at the claim cadence when the newer replica's pump claims it --
// while the older replica still hosts the session, and again once the
// rollout has ended the older pod and the session's actor moves to the
// newer one, which then receives it.
func TestUnknownTimer_NewerReplicasTimerSurvivesADeploy(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	timeouts := platform.DefaultTimeouts()
	older := newUnknownTimerRegistry(ctx, t, pool, timeouts)
	newer := newUnknownTimerRegistry(ctx, t, pool, timeouts)

	// The newer replica arms its kind, as its own code would: just now.
	armTimerAged(ctx, t, pool, sessionID, newerBinaryKind, 0)

	// The older replica's pump gets there first.
	if err := older.PumpOnce(ctx); err != nil {
		t.Fatalf("older PumpOnce: %v", err)
	}
	afterOlderClaim := time.Now()
	waitForLogLines(t, logs, unknownTimerKeptMsg, newerBinaryKind, 1)
	if older.lookup(sessionID) == nil {
		t.Fatal("the older replica hosts no actor for the session: its pump did not deliver the timer")
	}
	row, ok := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	if !ok {
		t.Fatalf("the older replica dropped the newer replica's timer %q", newerBinaryKind)
	}
	if latest := afterOlderClaim.Add(timeouts.TimerClaimDuration); row.FiresAt.Time.After(latest) {
		t.Fatalf("fires_at after the older replica's delivery = %v, want at most one claim window ahead (%v)", row.FiresAt.Time, latest)
	}

	// One claim window later the newer replica's pump claims it, while
	// the older replica still hosts the session.
	advanceTimer(ctx, t, pool, sessionID, newerBinaryKind, timeouts.TimerClaimDuration+clockSlack)
	due, _ := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	beforeNewerClaim := time.Now()
	if err := newer.PumpOnce(ctx); err != nil {
		t.Fatalf("newer PumpOnce: %v", err)
	}
	claimed, _ := timerRow(ctx, t, pool, sessionID, newerBinaryKind)
	if !claimed.FiresAt.Time.After(due.FiresAt.Time) || claimed.FiresAt.Time.Before(beforeNewerClaim.Add(timeouts.TimerClaimDuration)) {
		t.Fatalf("fires_at %v -> %v across the newer replica's tick at %v: its pump did not claim the timer", due.FiresAt.Time, claimed.FiresAt.Time, beforeNewerClaim)
	}

	// The rollout ends the older pod; the next claim reaches the newer
	// replica's own actor.
	if err := older.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("older Shutdown: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool { return len(advisoryLockHolders(ctx, t, pool, sessionID)) == 0 })
	advanceTimer(ctx, t, pool, sessionID, newerBinaryKind, timeouts.TimerClaimDuration+clockSlack)
	if err := newer.PumpOnce(ctx); err != nil {
		t.Fatalf("newer PumpOnce after the rollout: %v", err)
	}
	waitForLogLines(t, logs, unknownTimerKeptMsg, newerBinaryKind, 2)
	if newer.lookup(sessionID) == nil {
		t.Fatal("the newer replica hosts no actor for the session: the timer did not reach it")
	}
	if _, ok := timerRow(ctx, t, pool, sessionID, newerBinaryKind); !ok {
		t.Fatalf("timer %q is gone after the deploy", newerBinaryKind)
	}
}

// TestUnknownTimer_KnownKindsUnaffectedByAge pins that every kind this
// binary declares is handled by its own handler whatever its row's age:
// each, armed on a bare session (no sandbox, no turn, no pull request) both
// just now and past UnknownTimerDeleteAfter, and delivered by the real
// pump, ends the same way at both ages -- deleted, or re-armed the same
// distance ahead -- and never draws a line or a count from the unknown-kind
// path.
func TestUnknownTimer_KnownKindsUnaffectedByAge(t *testing.T) {
	logs := captureDefaultLoggerJSONSync(t)
	ctx := context.Background()
	pool := newTestPool(t)
	timeouts := platform.DefaultTimeouts()
	// A claim window no known handler re-arms by, so a handled row is
	// always told apart from one only claimed.
	timeouts.TimerClaimDuration = 45 * time.Second
	r := newUnknownTimerRegistry(ctx, t, pool, timeouts)

	declared := DeclaredTimerKindsForTest(t)
	kinds := make([]string, 0, len(declared))
	for _, value := range declared {
		kinds = append(kinds, value)
	}
	sort.Strings(kinds)
	if len(kinds) < 7 {
		t.Fatalf("found %d declared timer kinds (%v), want at least seven: the scan is broken", len(kinds), kinds)
	}

	type armed struct {
		kind      string
		aged      bool
		sessionID pgtype.UUID
	}
	var all []armed
	for _, kind := range kinds {
		for _, aged := range []bool{false, true} {
			a := armed{kind: kind, aged: aged, sessionID: createTestSession(ctx, t, pool)}
			age := time.Duration(0)
			if aged {
				age = timeouts.UnknownTimerDeleteAfter + time.Hour
			}
			armTimerAged(ctx, t, pool, a.sessionID, kind, age)
			all = append(all, a)
		}
	}
	backedOffBefore, deletedBefore := unknownTimerCounts(ctx, t)

	claimed, err := r.claimDueTimers(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != len(all) {
		t.Fatalf("claimed %d timers, want %d", len(claimed), len(all))
	}
	claimedAt := map[pgtype.UUID]time.Time{}
	for _, c := range claimed {
		claimedAt[c.SessionID] = c.FiresAt.Time
	}
	handledAt := time.Now()
	if delivered, skipped := deliverBatch(ctx, claimed, r.deliver); delivered != len(claimed) || skipped != 0 {
		t.Fatalf("delivered %d, skipped %d; want %d and 0", delivered, skipped, len(claimed))
	}

	// Each handler re-arms or deletes its timer: wait for every row to
	// leave the state the claim left it in.
	outcome := map[pgtype.UUID]*sqlcgen.SessionTimer{}
	for _, a := range all {
		waitUntil(t, 10*time.Second, func() bool {
			row, ok := timerRow(ctx, t, pool, a.sessionID, a.kind)
			if !ok {
				outcome[a.sessionID] = nil
				return true
			}
			if !row.FiresAt.Time.Equal(claimedAt[a.sessionID]) {
				outcome[a.sessionID] = &row
				return true
			}
			return false
		})
	}

	for i := 0; i < len(all); i += 2 {
		young, aged := all[i], all[i+1]
		youngRow, agedRow := outcome[young.sessionID], outcome[aged.sessionID]
		switch {
		case (youngRow == nil) != (agedRow == nil):
			t.Errorf("%s: deleted just armed = %v, deleted past the deletion bound = %v: its age changed its handling", young.kind, youngRow == nil, agedRow == nil)
		case youngRow != nil:
			youngAhead, agedAhead := youngRow.FiresAt.Time.Sub(handledAt), agedRow.FiresAt.Time.Sub(handledAt)
			if diff := youngAhead - agedAhead; diff > 5*time.Second || diff < -5*time.Second {
				t.Errorf("%s: re-armed %v ahead just armed, %v ahead past the deletion bound: its age changed its handling", young.kind, youngAhead, agedAhead)
			}
		}
		if got := unknownTimerLines(t, logs, young.kind); got != 0 {
			t.Errorf("%s: %d unknown-kind lines name it: a declared kind reached the unknown-kind path", young.kind, got)
		}
	}
	if backedOff, deleted := unknownTimerCounts(ctx, t); backedOff != backedOffBefore || deleted != deletedBefore {
		t.Errorf("%s moved (backed_off %+d, deleted %+d) for declared kinds, want no count", unknownTimerMetric, backedOff-backedOffBefore, deleted-deletedBefore)
	}
}

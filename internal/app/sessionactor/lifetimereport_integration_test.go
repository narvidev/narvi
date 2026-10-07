//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves technical plan §35.2's reader on the real event path:
// the claim's estimate stands for an agent that reports nothing, and the
// exact value an agent reports on a ready or heartbeat only ever brings the
// gen's deadline earlier -- never later, never for a gen that is not live
// -- and is recorded as the deadline of a live gen that has none of its
// own.

// lifetimeReportRig is a live actor over one session whose sandbox row
// the test seeds itself; no provider is needed, since nothing here spawns.
type lifetimeReportRig struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	actor     *Actor
	logs      *syncLogBuffer
}

// newLifetimeReportRig seeds a Ready sandbox at gen 1 whose claim stamped
// the default kind's estimate, 7200 seconds, then hydrates the actor.
// prepare, when set, runs on the seeded row first.
func newLifetimeReportRig(ctx context.Context, t *testing.T, prepare func(*pgxpool.Pool, pgtype.UUID)) *lifetimeReportRig {
	t.Helper()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	tokenHash := "token-hash-lifetime-report"
	if _, err := narvipg.NewSandboxStore(pool).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: int32Ptr(7200),
	}); err != nil {
		t.Fatalf("seed the sandbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("make the sandbox ready: %v", err)
	}
	if prepare != nil {
		prepare(pool, sessionID)
	}

	logs := captureDefaultLoggerJSONSync(t)
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return &lifetimeReportRig{pool: pool, sessionID: sessionID, actor: a, logs: logs}
}

// lifetimeFrame builds a ready or heartbeat of gen, reporting remaining
// seconds when it is not nil and nothing otherwise.
func lifetimeFrame(eventType string, gen int, messageID string, remaining *int) SandboxEvent {
	frame := map[string]any{
		"type": eventType, "messageId": messageID, "sessionId": "00000000-0000-0000-0000-000000000000",
		"gen": gen, "timestamp": "2026-10-06T12:00:00Z",
	}
	if eventType == "ready" {
		frame["agentVersion"], frame["imageDigest"] = "dev", "unknown"
	} else {
		frame["conversationId"], frame["lastBootPhase"] = nil, nil
	}
	if remaining != nil {
		frame["lifetimeRemainingSeconds"] = *remaining
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		panic(err)
	}
	return SandboxEvent{Type: eventType, Gen: gen, MessageID: messageID, Raw: raw}
}

// send delivers cmd and reports, on the database's clock, the instants
// just before and just after it was handled: a deadline the event set is
// now() plus the report for some instant between the two.
func (r *lifetimeReportRig) send(ctx context.Context, t *testing.T, cmd SandboxEvent) (outcome SandboxEventOutcome, before, after time.Time) {
	t.Helper()
	before = r.dbClock(ctx, t)
	outcome = sendSandboxEvent(ctx, t, r.actor, cmd)
	after = r.dbClock(ctx, t)
	return outcome, before, after
}

func (r *lifetimeReportRig) dbClock(ctx context.Context, t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// tightenedLines counts the "deadline brought earlier" lines once every
// event sent so far has run its post-commit work: a further event's reply
// comes only after the actor has finished the one before it.
func (r *lifetimeReportRig) tightenedLines(ctx context.Context, t *testing.T, gen int) int {
	t.Helper()
	sendSandboxEvent(ctx, t, r.actor, lifetimeFrame("heartbeat", gen, fmt.Sprintf("flush-%d", time.Now().UnixNano()), nil))
	return countLogLines(t, r.logs, lifetimeTightenedLogMessage)
}

func assertDeadlineUnchanged(t *testing.T, got, want lifetimeRow, after string) {
	t.Helper()
	if got.gen != want.gen || (got.deadline == nil) != (want.deadline == nil) ||
		(got.deadline != nil && !got.deadline.Equal(*want.deadline)) ||
		int32Value(got.seconds) != int32Value(want.seconds) || int32Value(got.deadlineGen) != int32Value(want.deadlineGen) {
		t.Fatalf("after %s: gen %d, deadline %v, lifetime_seconds %v at gen %v; want it unchanged: gen %d, deadline %v, %v at gen %v",
			after, got.gen, got.deadline, int32Value(got.seconds), int32Value(got.deadlineGen),
			want.gen, want.deadline, int32Value(want.seconds), int32Value(want.deadlineGen))
	}
}

// assertDeadlineFromReport fails unless row's deadline is the report's:
// remaining seconds after some instant in [before, after], recorded for
// the live gen with wantSeconds as its lifetime_seconds.
func assertDeadlineFromReport(t *testing.T, row lifetimeRow, before, after time.Time, remaining int, wantSeconds *int32) {
	t.Helper()
	if row.deadline == nil || row.deadlineGen == nil || *row.deadlineGen != row.gen {
		t.Fatalf("gen %d: deadline %v recorded at gen %v; want one for the live gen", row.gen, row.deadline, int32Value(row.deadlineGen))
	}
	reported := time.Duration(remaining) * time.Second
	if row.deadline.Before(before.Add(reported)) || row.deadline.After(after.Add(reported)) {
		t.Errorf("lifetime_deadline_at = %v, want now() plus %v for a now() in [%v, %v]", *row.deadline, reported, before, after)
	}
	if int32Value(row.seconds) != int32Value(wantSeconds) {
		t.Errorf("lifetime_seconds = %v, want %v", int32Value(row.seconds), int32Value(wantSeconds))
	}
}

// TestSandboxLifetime_AgentThatReportsNothingKeepsTheEstimate is §35.2's
// "estimate first": an agent that reports nothing -- every agent until a
// provider states a deadline to its sandbox, and every agent in an older
// snapshot -- leaves the claim's estimate exactly as it was, across its
// ready and every heartbeat.
func TestSandboxLifetime_AgentThatReportsNothingKeepsTheEstimate(t *testing.T) {
	ctx := context.Background()
	rig := newLifetimeReportRig(ctx, t, nil)
	estimate := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)
	if estimate.deadline == nil || int32Value(estimate.deadlineGen) != int32(1) {
		t.Fatalf("seeded row: deadline %v at gen %v, want the claim's estimate at gen 1", estimate.deadline, int32Value(estimate.deadlineGen))
	}

	frames := []SandboxEvent{lifetimeFrame("ready", 1, "r-none", nil)}
	for i := range 3 {
		frames = append(frames, lifetimeFrame("heartbeat", 1, fmt.Sprintf("h-none-%d", i), nil))
	}
	for _, cmd := range frames {
		if outcome, _, _ := rig.send(ctx, t, cmd); !outcome.Persisted {
			t.Fatalf("%s %s: Persisted = false, want true", cmd.Type, cmd.MessageID)
		}
		assertDeadlineUnchanged(t, readLifetimeRow(ctx, t, rig.pool, rig.sessionID), estimate, cmd.Type+" "+cmd.MessageID)
	}
	if n := rig.tightenedLines(ctx, t, 1); n != 0 {
		t.Errorf("%d tightened lines for an agent that reported nothing, want 0", n)
	}
}

// TestSandboxLifetime_ReportedRemainingOnlyBringsTheDeadlineEarlier is
// §35.2's exact value: a report earlier than the deadline brings it to
// now() plus the report; a later report changes nothing; the same report
// again a moment later changes nothing either -- a steady heartbeat writes
// once -- and a ready reports like a heartbeat. The estimate's
// lifetime_seconds stays: the kind's lifetime is still what §35.3's
// runway gate reads.
func TestSandboxLifetime_ReportedRemainingOnlyBringsTheDeadlineEarlier(t *testing.T) {
	ctx := context.Background()
	rig := newLifetimeReportRig(ctx, t, nil)
	estimate := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)

	// Earlier: tightened.
	_, before, after := rig.send(ctx, t, lifetimeFrame("heartbeat", 1, "h-earlier", intPtr(1800)))
	tightened := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)
	assertDeadlineFromReport(t, tightened, before, after, 1800, int32Ptr(7200))
	if !tightened.deadline.Before(*estimate.deadline) {
		t.Fatalf("lifetime_deadline_at = %v, want earlier than the estimate %v", *tightened.deadline, *estimate.deadline)
	}
	line := waitForLogEntry(t, rig.logs, 5*time.Second, lifetimeTightenedLogMessage)
	if line["event_type"] != "heartbeat" || line["gen"] != float64(1) || line["lifetime_remaining_seconds"] != float64(1800) {
		t.Errorf("logged %v, want the heartbeat of gen 1 reporting 1800 seconds", line)
	}

	// Later: unchanged.
	rig.send(ctx, t, lifetimeFrame("heartbeat", 1, "h-later", intPtr(5400)))
	assertDeadlineUnchanged(t, readLifetimeRow(ctx, t, rig.pool, rig.sessionID), tightened, "a later report")

	// The same report again, a moment later: unchanged, nothing written.
	rig.send(ctx, t, lifetimeFrame("heartbeat", 1, "h-again", intPtr(1800)))
	assertDeadlineUnchanged(t, readLifetimeRow(ctx, t, rig.pool, rig.sessionID), tightened, "the same report again")
	if n := rig.tightenedLines(ctx, t, 1); n != 1 {
		t.Fatalf("%d tightened lines after an earlier, a later and a repeated report, want 1", n)
	}

	// A ready reports like a heartbeat.
	_, before, after = rig.send(ctx, t, lifetimeFrame("ready", 1, "r-earlier", intPtr(600)))
	assertDeadlineFromReport(t, readLifetimeRow(ctx, t, rig.pool, rig.sessionID), before, after, 600, int32Ptr(7200))
	if n := rig.tightenedLines(ctx, t, 1); n != 2 {
		t.Errorf("%d tightened lines after the ready's report, want 2", n)
	}
}

// TestSandboxLifetime_ReportOnAGenThePreviousBinarySpawnedIsRecorded is
// the rolling-deploy half: a gen the previous binary spawned has no
// deadline of its own -- its lifetime_deadline_gen still names the gen
// before -- so the first report is its deadline, recorded against it, even
// later than the old gen's, which never applied to it; lifetime_seconds is
// cleared, since no kind's lifetime was stamped for it.
func TestSandboxLifetime_ReportOnAGenThePreviousBinarySpawnedIsRecorded(t *testing.T) {
	ctx := context.Background()
	rig := newLifetimeReportRig(ctx, t, func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
		// The previous binary's respawn: gen bumped, the lifetime columns
		// left at gen 1's, whose deadline has long passed.
		if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = gen + 1, last_seen_at = now(),
			    lifetime_deadline_at = now() - interval '1 hour'
			 WHERE session_id = $1`, sessionID); err != nil {
			t.Fatalf("the previous binary's respawn: %v", err)
		}
	})
	old := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)
	if old.gen != 2 || int32Value(old.deadlineGen) != int32(1) {
		t.Fatalf("seeded row: gen %d, deadline at gen %v; want gen 2 with gen 1's deadline", old.gen, int32Value(old.deadlineGen))
	}

	_, before, after := rig.send(ctx, t, lifetimeFrame("ready", 2, "r-previous-binary", intPtr(5400)))
	row := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)
	assertDeadlineFromReport(t, row, before, after, 5400, nil)
	if row.gen != 2 {
		t.Errorf("gen = %d, want 2", row.gen)
	}
}

// TestSandboxLifetime_StaleGenReportIgnored is the gen fence: a report
// from a gen that is no longer live -- an old sandbox still connected
// after a respawn -- changes nothing, however early it is.
func TestSandboxLifetime_StaleGenReportIgnored(t *testing.T) {
	ctx := context.Background()
	rig := newLifetimeReportRig(ctx, t, func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
		// A respawn by this binary: gen 2, with its own estimate.
		tokenHash := "token-hash-lifetime-report-2"
		if _, err := narvipg.NewSandboxStore(pool).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
			SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: int32Ptr(7200),
		}); err != nil {
			t.Fatalf("respawn: %v", err)
		}
	})
	live := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)
	if live.gen != 2 || int32Value(live.deadlineGen) != int32(2) {
		t.Fatalf("seeded row: gen %d, deadline at gen %v; want gen 2's own", live.gen, int32Value(live.deadlineGen))
	}

	for _, cmd := range []SandboxEvent{
		lifetimeFrame("heartbeat", 1, "h-stale", intPtr(60)),
		lifetimeFrame("ready", 1, "r-stale", intPtr(0)),
	} {
		if outcome, _, _ := rig.send(ctx, t, cmd); outcome.Persisted {
			t.Fatalf("%s of stale gen 1: Persisted = true, want the gen fence to drop it", cmd.Type)
		}
		assertDeadlineUnchanged(t, readLifetimeRow(ctx, t, rig.pool, rig.sessionID), live, "a stale gen's "+cmd.Type)
	}
	if n := rig.tightenedLines(ctx, t, 2); n != 0 {
		t.Errorf("%d tightened lines for a stale gen's reports, want 0", n)
	}
}

func intPtr(n int) *int { return &n }

// TestSandboxLifetime_AReportNeverCostsTheReadyItsCapabilities is the
// event-path half of TestReadyCapabilities_SurviveAnyLifetimeReport: a
// ready whose lifetimeRemainingSeconds the generated Ready cannot hold --
// 60.0, 6e1 and 1e20 are integers the contract allows, the rest it does
// not -- still records its promptReceipt, reviewCheckout and maxFrameBytes
// against the gen (technical plan §3.3, §21.1), and still tightens the deadline when the lenient
// read takes the value.
func TestSandboxLifetime_AReportNeverCostsTheReadyItsCapabilities(t *testing.T) {
	ctx := context.Background()
	rig := newLifetimeReportRig(ctx, t, nil)
	for i, value := range []string{`60.0`, `6e1`, `1e20`, `99999999999999999999`, `59.9`, `"60"`, `1e400`, `"soon"`, `{}`} {
		raw := json.RawMessage(`{"type":"ready","messageId":"r-capabilities-` + fmt.Sprint(i) + `","sessionId":"00000000-0000-0000-0000-000000000000","gen":1,` +
			`"timestamp":"2026-10-06T12:00:00Z","agentVersion":"dev","imageDigest":"unknown",` +
			`"capabilities":{"promptReceipt":true,"reviewCheckout":true,"maxFrameBytes":1048576},"lifetimeRemainingSeconds":` + value + `}`)
		if outcome, _, _ := rig.send(ctx, t, SandboxEvent{Type: "ready", Gen: 1, MessageID: fmt.Sprintf("r-capabilities-%d", i), Raw: raw}); !outcome.Persisted {
			t.Fatalf("ready with lifetimeRemainingSeconds %s: Persisted = false, want true", value)
		}
		var promptReceiptGen, reviewCheckoutGen, maxFrameBytes, maxFrameBytesGen *int32
		if err := rig.pool.QueryRow(ctx, `SELECT prompt_receipt_gen, review_checkout_gen, agent_max_frame_bytes, agent_max_frame_bytes_gen FROM sandboxes WHERE session_id = $1`, rig.sessionID).
			Scan(&promptReceiptGen, &reviewCheckoutGen, &maxFrameBytes, &maxFrameBytesGen); err != nil {
			t.Fatalf("read the recorded capabilities: %v", err)
		}
		if int32Value(promptReceiptGen) != int32(1) || int32Value(reviewCheckoutGen) != int32(1) ||
			int32Value(maxFrameBytes) != int32(1048576) || int32Value(maxFrameBytesGen) != int32(1) {
			t.Errorf("ready with lifetimeRemainingSeconds %s recorded prompt_receipt_gen %v, review_checkout_gen %v, agent_max_frame_bytes %v at gen %v; want gen 1, gen 1, 1048576 at gen 1",
				value, int32Value(promptReceiptGen), int32Value(reviewCheckoutGen), int32Value(maxFrameBytes), int32Value(maxFrameBytesGen))
		}
	}
	// 59.9 is the earliest value the lenient read took: the deadline is
	// that report's, not the estimate's.
	row := readLifetimeRow(ctx, t, rig.pool, rig.sessionID)
	if row.deadline == nil || time.Until(*row.deadline) > time.Minute {
		t.Errorf("lifetime_deadline_at = %v, want it brought within a minute by the reports", row.deadline)
	}
}

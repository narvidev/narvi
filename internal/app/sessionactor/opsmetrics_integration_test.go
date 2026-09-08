//go:build integration

package sessionactor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves §5.3's ("ops: dashboards, alerts, runbooks", §5.3)
// own five new instruments (opsmetrics.go) are genuinely emitted by their
// real production call sites -- driving each through the real Actor entry
// point (Send/handleEnsureDispatched, exactly like every other test in
// this package) and reading the result back from otelReader (the SAME
// process-wide ManualReader contractdrift_integration_test.go's own
// readContractDriftDetected already reads from -- see that file's own
// TestMain doc comment for why exactly one, process-wide, and why every
// assertion here is a BEFORE/AFTER delta, never an absolute value).

// readCounterSum sums every data point of the narvi/sessionactor meter's
// named Int64 counter -- readContractDriftDetected's own shape
// (contractdrift_integration_test.go), generalized to any counter name so
// this file does not need four near-identical copies.
func readCounterSum(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s metric data = %T, want metricdata.Sum[int64]", name, m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// readCounterSumByAttr mirrors readCounterSum, but sums only the data
// points carrying attrValue for attrKey -- used where this file also
// wants to prove a specific attribute tag, not just that the instrument
// recorded something.
func readCounterSumByAttr(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, name, attrKey, attrValue string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s metric data = %T, want metricdata.Sum[int64]", name, m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(attrKey)); ok && v.AsString() == attrValue {
					total += dp.Value
				}
			}
			return total
		}
	}
	return 0
}

// readHistogramCount sums every data point's own Count for the named
// Float64 histogram under the narvi/sessionactor meter scope -- "did this
// histogram record anything at all" is this file's own bar (proving
// emission), not asserting on the recorded VALUE itself.
func readHistogramCount(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader, name string) uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s metric data = %T, want metricdata.Histogram[float64]", name, m.Data)
			}
			var total uint64
			for _, dp := range hist.DataPoints {
				total += dp.Count
			}
			return total
		}
	}
	return 0
}

// TestExecuteSpawn_RecordsSpawnDurationHistogram drives a real spawn
// through handleEnsureDispatched (mirroring
// TestHandleEnsureDispatched_NoSandbox_Spawns, dispatch_integration_test.go
// exactly) and confirms sandbox_spawn_duration_seconds (§5.3: spawn
// latency) genuinely records once the fake provider's CreateSandbox call
// returns -- proving executeSpawn's own recordSpawnDuration call site,
// not merely that the recorder works in isolation.
func TestExecuteSpawn_RecordsSpawnDurationHistogram(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	before := readHistogramCount(ctx, t, otelReader, "sandbox_spawn_duration_seconds")

	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-object-opsmetrics-1"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	waitUntil(t, 5*time.Second, func() bool {
		return provider.callCount() == 1
	})

	sandboxStore := narvipg.NewSandboxStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		row, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && row.Status == sqlcgen.SandboxStatusConnecting
	})

	waitUntil(t, 5*time.Second, func() bool {
		return readHistogramCount(ctx, t, otelReader, "sandbox_spawn_duration_seconds") > before
	})

	after := readHistogramCount(ctx, t, otelReader, "sandbox_spawn_duration_seconds")
	if after <= before {
		t.Errorf("sandbox_spawn_duration_seconds count = %d, want > %d (executeSpawn's real CreateSandbox call must record it)", after, before)
	}
}

// TestHandleLivenessCheckTimerFired_RecordsWatchdogActivationAndLivenessGap
// mirrors TestLivenessCheckTimerFired_FullRoundTrip
// (timerfired_integration_test.go) exactly for its Suspect-transition
// setup, adding this Step's own assertions: watchdog_activation_total
// (tagged watchdog="liveness_check") and sandbox_liveness_gap_seconds
// both record once the real liveness_check watchdog actually fires and
// transitions the sandbox to Suspect -- proving transitionSandboxToSuspect's
// own real call site, not the recorder in isolation.
func TestHandleLivenessCheckTimerFired_RecordsWatchdogActivationAndLivenessGap(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	staleSince := time.Now().Add(-1 * time.Hour) // comfortably past the default 90s SteadyHeartbeatBudget
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID:  sessionID,
		Status:     sqlcgen.SandboxStatusReady,
		LastSeenAt: pgtype.Timestamptz{Time: staleSince, Valid: true},
	}); err != nil {
		t.Fatalf("move sandbox to ready with stale last_seen_at: %v", err)
	}

	timerStore := narvipg.NewTimerStore(pool)
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      TimerLivenessCheck,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true}, // already overdue
	}); err != nil {
		t.Fatalf("seed overdue liveness_check timer: %v", err)
	}

	activationBefore := readCounterSumByAttr(ctx, t, otelReader, "watchdog_activation_total", "watchdog", "liveness_check")
	gapBefore := readHistogramCount(ctx, t, otelReader, "sandbox_liveness_gap_seconds")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerLivenessCheck}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusSuspect
	})

	activationAfter := readCounterSumByAttr(ctx, t, otelReader, "watchdog_activation_total", "watchdog", "liveness_check")
	if activationAfter <= activationBefore {
		t.Errorf("watchdog_activation_total{watchdog=liveness_check} = %d, want > %d", activationAfter, activationBefore)
	}
	gapAfter := readHistogramCount(ctx, t, otelReader, "sandbox_liveness_gap_seconds")
	if gapAfter <= gapBefore {
		t.Errorf("sandbox_liveness_gap_seconds count = %d, want > %d", gapAfter, gapBefore)
	}
}

// TestHandleSandboxEvent_SuspectRecovery_RecordsWatchdogFalseAlarm mirrors
// TestHandleSandboxEvent_SuspectRecovery_ReturnsToPreSuspectStatus
// (suspectrecovery_integration_test.go) exactly, adding this Step's own
// assertion: watchdog_false_alarm_total (tagged recovered_from="ready")
// records once a Suspect sandbox genuinely recovers during its own
// terminal_grace window.
func TestHandleSandboxEvent_SuspectRecovery_RecordsWatchdogFalseAlarm(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	seedSuspectSandboxWithPreSuspectStatus(ctx, t, pool, sessionID, sqlcgen.SandboxStatusReady)

	falseAlarmBefore := readCounterSumByAttr(ctx, t, otelReader, "watchdog_false_alarm_total", "recovered_from", "ready")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "heartbeat",
		Gen:  1,
		Raw:  heartbeatRaw("false-alarm-recovery-1"),
	})
	if !outcome.Persisted {
		t.Fatal("outcome.Persisted = false, want true")
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusReady
	})

	falseAlarmAfter := readCounterSumByAttr(ctx, t, otelReader, "watchdog_false_alarm_total", "recovered_from", "ready")
	if falseAlarmAfter <= falseAlarmBefore {
		t.Errorf("watchdog_false_alarm_total{recovered_from=ready} = %d, want > %d (a real Suspect recovery must record it)", falseAlarmAfter, falseAlarmBefore)
	}
}

// TestHandleTurnDeadlineTimer_ThenLateExecutionComplete_RecordsFalseFailure
// reproduces this Step's own "false failure" definition end to end
// (recordFalseFailureIfApplicable's own doc comment, pushpr.go): a turn is
// genuinely terminalized Failed with failure_reason=timeout by a real
// turn_deadline firing (mirroring TestTurnDeadlineTimerFired_FullRoundTrip,
// timerfired_integration_test.go, exactly) -- and only AFTER that does a
// real, late, wire-level execution_complete{outcome:completed} arrive for
// the SAME session. turn_false_failure_total must record exactly this
// case, driven through the real handleSandboxEvent entry point, not the
// recorder called directly.
func TestHandleTurnDeadlineTimer_ThenLateExecutionComplete_RecordsFalseFailure(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default

	turnStore := narvipg.NewTurnStore(pool)
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchedAt := time.Now().Add(-1 * time.Hour) // comfortably past the tiny deadline
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:           created.ID,
		Status:       sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	falseFailureBefore := readCounterSum(ctx, t, otelReader, "turn_false_failure_total")

	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// Step 1: the real turn_deadline timer fires first, exactly like
	// TestTurnDeadlineTimerFired_FullRoundTrip -- the control plane's own
	// inference that this turn is stuck, with no wire signal yet.
	if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
		t.Fatalf("Send TimerFired turn_deadline: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	sessionStore := narvipg.NewSessionStore(pool)
	gotSession, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSession.Status != sqlcgen.SessionStatusFailed || gotSession.FailureReason == nil || *gotSession.FailureReason != sqlcgen.SessionFailureReasonTimeout {
		t.Fatalf("session status/failure_reason = %s/%v, want failed/timeout (precondition for this test's own scenario)", gotSession.Status, gotSession.FailureReason)
	}

	// Step 2: the sandbox, unaware the control plane already gave up,
	// genuinely finishes and reports success -- late.
	outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})
	if !outcome.Persisted {
		t.Error("outcome.Persisted = false, want true (the raw event is always persisted, per handleSandboxEvent's own top comment)")
	}

	waitUntil(t, 5*time.Second, func() bool {
		return readCounterSum(ctx, t, otelReader, "turn_false_failure_total") > falseFailureBefore
	})

	falseFailureAfter := readCounterSum(ctx, t, otelReader, "turn_false_failure_total")
	if falseFailureAfter <= falseFailureBefore {
		t.Errorf("turn_false_failure_total = %d, want > %d (a late completed execution_complete after a timeout-failed turn must record it)", falseFailureAfter, falseFailureBefore)
	}

	// The turn itself must stay Failed -- domain/turn.State has no Failed
	// -> Completed edge (state.go's own top comment); this instrument is
	// observability only, never a state reconciliation.
	gotTurn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusFailed {
		t.Errorf("turn status = %s, want unchanged %s (the late completion must never resurrect a terminalized turn)", gotTurn.Status, sqlcgen.TurnStatusFailed)
	}

	// §12.2 item 6's own durable sibling: the SAME call that just
	// incremented turn_false_failure_total must also have inserted
	// exactly one false_failures row for this session, in the same
	// transaction.
	falseFailureStore := narvipg.NewFalseFailureStore(pool)
	sinceEpoch := pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true}
	gotCount, err := falseFailureStore.CountInWindow(ctx, sinceEpoch)
	if err != nil {
		t.Fatalf("count false_failures: %v", err)
	}
	if gotCount != 1 {
		t.Errorf("false_failures count = %d, want 1 (one durable row for the one genuine false failure)", gotCount)
	}
}

// TestHandleSandboxEvent_RedeliveredLateExecutionComplete_RecordsFalseFailureOnce
// is this Step's own mutation test for the confirmed audit finding (MEDIUM):
// completeProcessingTurn's no-turn-currently-Processing branch (pushpr.go)
// used to gate recordFalseFailureIfApplicable on trig == turn.TriggerComplete
// ALONE, with no defense against a wire-level redelivery of the SAME
// execution_complete event re-entering that branch and re-incrementing
// turn_false_failure_total once per resend -- exactly the scenario §6.1's
// ack protocol makes routine (the buffered-critical-event resend-until-acked
// mechanism test/resilience/scenario7_ack_redelivery_test.go exercises),
// and one this Step's own four pre-existing false-failure tests never
// happened to cover because none of them ever delivered the SAME
// execution_complete twice.
//
// Reproduces TestHandleTurnDeadlineTimer_ThenLateExecutionComplete_
// RecordsFalseFailure's own exact setup (a turn genuinely terminalized
// Failed/timeout by a real turn_deadline fire), then sends the identical
// raw execution_complete wire bytes -- same messageID, so
// appendRawEvent's own upsert-on-(session_id, message_id) sees the second
// send as a genuine redelivery (Inserted == false), not a new event --
// TWICE, and asserts turn_false_failure_total moved by exactly 1, not 2.
// Removing the inserted gate this Step adds to completeProcessingTurn (the
// fix for the confirmed finding) makes this test fail: it would instead
// observe the counter incrementing by 2.
func TestHandleSandboxEvent_RedeliveredLateExecutionComplete_RecordsFalseFailureOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default

	turnStore := narvipg.NewTurnStore(pool)
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchedAt := time.Now().Add(-1 * time.Hour) // comfortably past the tiny deadline
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:           created.ID,
		Status:       sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	falseFailureBefore := readCounterSum(ctx, t, otelReader, "turn_false_failure_total")

	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// Step 1: the real turn_deadline timer fires first, exactly like
	// TestHandleTurnDeadlineTimer_ThenLateExecutionComplete_RecordsFalseFailure.
	if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
		t.Fatalf("Send TimerFired turn_deadline: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	sessionStore := narvipg.NewSessionStore(pool)
	gotSession, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSession.Status != sqlcgen.SessionStatusFailed || gotSession.FailureReason == nil || *gotSession.FailureReason != sqlcgen.SessionFailureReasonTimeout {
		t.Fatalf("session status/failure_reason = %s/%v, want failed/timeout (precondition for this test's own scenario)", gotSession.Status, gotSession.FailureReason)
	}

	// Step 2: the sandbox, unaware the control plane already gave up,
	// genuinely finishes and reports success -- late. The SAME raw bytes
	// (same messageID) are sent TWICE, mirroring a real §6.1 ack-protocol
	// resend of this exact critical event before its own ack lands --
	// never a fresh executionCompleteRaw call per send, which would mint a
	// DIFFERENT messageID and so a genuinely distinct event instead of a
	// redelivery of this one.
	raw := executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted)

	firstOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  raw,
	})
	if !firstOutcome.Persisted {
		t.Error("first delivery: outcome.Persisted = false, want true")
	}
	waitUntil(t, 5*time.Second, func() bool {
		return readCounterSum(ctx, t, otelReader, "turn_false_failure_total") > falseFailureBefore
	})

	secondOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  raw, // identical messageID -- a genuine wire-level redelivery.
	})
	if !secondOutcome.Persisted {
		t.Error("redelivery: outcome.Persisted = false, want true (still persisted -- appendRawEvent's own upsert always succeeds)")
	}

	// No extra wait needed here: sendSandboxEventForTest only returns once
	// cmd.Reply has fired, and handleSandboxEvent sends that reply
	// synchronously right after its own transact commits (sandboxevent.go's
	// own doc comment) -- i.e. AFTER recordFalseFailure's own in-transact
	// Add call would already have run for this second delivery, if the gate
	// under test failed to prevent it. By the time secondOutcome is in
	// hand, whatever this redelivery did (or correctly did not do) to the
	// counter has already happened.
	falseFailureAfter := readCounterSum(ctx, t, otelReader, "turn_false_failure_total")
	if falseFailureAfter != falseFailureBefore+1 {
		t.Errorf("turn_false_failure_total = %d, want exactly %d (delivered the same execution_complete twice; a redelivery of an already-counted false failure must never re-increment)", falseFailureAfter, falseFailureBefore+1)
	}

	gotTurn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusFailed {
		t.Errorf("turn status = %s, want unchanged %s (redelivery must never resurrect a terminalized turn either)", gotTurn.Status, sqlcgen.TurnStatusFailed)
	}

	// The durable false_failures row must obey the identical "once, not
	// once per redelivery" gate the OTel counter above was just proven to
	// obey -- both are written from the SAME call, inside the SAME
	// transaction (recordFalseFailureIfApplicable, pushpr.go).
	falseFailureStore := narvipg.NewFalseFailureStore(pool)
	sinceEpoch := pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true}
	gotCount, err := falseFailureStore.CountInWindow(ctx, sinceEpoch)
	if err != nil {
		t.Fatalf("count false_failures: %v", err)
	}
	if gotCount != 1 {
		t.Errorf("false_failures count = %d, want exactly 1 (a redelivery of an already-counted false failure must never insert a second row)", gotCount)
	}
}

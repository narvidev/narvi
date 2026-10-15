//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// The runway gate's tests (technical plan §35.3, runway.go) run the real
// actor on Postgres. The sandbox is seeded at gen 1 with a provider object,
// provider-gen-1, a lifetime deadline stamped for it from a 2h lifetime at
// the runway each test asks for, on the database's clock, and a previous
// snapshot, snap-previous, recorded half an hour ago with its provenance.

const (
	runwayProviderGen1    = "provider-gen-1"
	runwayRestoredObject  = "provider-restored"
	runwayFreshObject     = "provider-fresh"
	runwayPreviousSnap    = "snap-previous"
	runwayRotationSnap    = "snap-rotation"
	runwayLifetimeSeconds = 7200
)

// runwayRig is one session on the real actor, with a pending turn queued
// for a live sandbox.
type runwayRig struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	turnID    pgtype.UUID
	provider  *fakeSpawnProvider
	commander *fakeSendCommander
	registry  *Registry
	actor     *Actor
}

// runwayRigOptions shape a runwayRig; the zero value is a ready sandbox
// with an hour of runway, the shipped timeouts, a provider that snapshots
// and stops explicitly, and a commander that writes every command.
type runwayRigOptions struct {
	pool       *pgxpool.Pool
	status     sqlcgen.SandboxStatus
	runway     time.Duration
	noDeadline bool
	timeouts   *platform.Timeouts
	sessionID  pgtype.UUID
	provider   *fakeSpawnProvider
	commander  *fakeSendCommander
	// beforeActor runs once the sandbox and turn are seeded, before the
	// actor is hydrated.
	beforeActor func(pool *pgxpool.Pool, sessionID pgtype.UUID)
}

func newRunwayRig(ctx context.Context, t *testing.T, opts runwayRigOptions) *runwayRig {
	t.Helper()
	rig := &runwayRig{pool: opts.pool, provider: opts.provider, commander: opts.commander}
	if rig.pool == nil {
		rig.pool = newTestPool(t)
	}
	rig.sessionID = opts.sessionID
	if !rig.sessionID.Valid {
		rig.sessionID = createTestSession(ctx, t, rig.pool)
	}
	if opts.status == "" {
		opts.status = sqlcgen.SandboxStatusReady
	}
	if opts.runway == 0 && !opts.noDeadline {
		opts.runway = time.Hour
	}
	seedRunwaySandbox(ctx, t, rig.pool, rig.sessionID, opts.status, opts.runway, !opts.noDeadline)
	rig.turnID = createTurnArmingDispatch(ctx, t, rig.pool, rig.sessionID, "the turn queued for the sandbox").ID
	if opts.beforeActor != nil {
		opts.beforeActor(rig.pool, rig.sessionID)
	}
	if rig.provider == nil {
		rig.provider = &fakeSpawnProvider{explicitStop: true}
	}
	rig.provider.nextRestoreRef = ports.SandboxRef{ProviderID: runwayRestoredObject}
	rig.provider.nextRef = ports.SandboxRef{ProviderID: runwayFreshObject}
	if rig.commander == nil {
		rig.commander = &fakeSendCommander{}
	}
	to := platform.DefaultTimeouts()
	if opts.timeouts != nil {
		to = *opts.timeouts
	}
	r, err := NewRegistry(ctx, rig.pool, to, nil, rig.commander, rig.provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	rig.registry = r
	if rig.actor, err = r.GetOrSpawn(ctx, rig.sessionID); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return rig
}

// seedRunwaySandbox gives sessionID its sandbox at gen 1 in status, last
// seen now, provider-gen-1 its object, a deadline runway from the
// database's now stamped for gen 1 from a 2h lifetime (none when
// !withDeadline), and snap-previous recorded 30 minutes ago with its
// provenance.
func seedRunwaySandbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, status sqlcgen.SandboxStatus, runway time.Duration, withDeadline bool) {
	t.Helper()
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = $2::sandbox_status, gen = 1, provider_id = $3, last_seen_at = now(),
		lifetime_deadline_at = CASE WHEN $4::boolean THEN now() + make_interval(secs => $5::double precision) END,
		lifetime_seconds = CASE WHEN $4::boolean THEN $6::integer END,
		lifetime_deadline_gen = CASE WHEN $4::boolean THEN 1 END,
		snapshot_id = $7, snapshot_provenance_id = $7, snapshot_minted_at = now() - interval '30 minutes'
		WHERE session_id = $1`,
		sessionID, string(status), runwayProviderGen1, withDeadline, runway.Seconds(), runwayLifetimeSeconds, runwayPreviousSnap); err != nil {
		t.Fatalf("seed the sandbox: %v", err)
	}
	if status == sqlcgen.SandboxStatusSnapshotting {
		if _, err := pool.Exec(ctx, `UPDATE sandboxes SET pending_snapshot_message_id = 'post-turn-snapshot', snapshot_started_at = now() WHERE session_id = $1`, sessionID); err != nil {
			t.Fatalf("seed the snapshot in progress: %v", err)
		}
	}
}

func (r *runwayRig) sandbox(ctx context.Context, t *testing.T) sqlcgen.Sandbox {
	t.Helper()
	row, err := narvipg.NewSandboxStore(r.pool).Get(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row
}

func (r *runwayRig) runway(ctx context.Context, t *testing.T) sqlcgen.GetSandboxRunwayRow {
	t.Helper()
	row, err := narvipg.NewSandboxStore(r.pool).Runway(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("read the runway: %v", err)
	}
	return row
}

func (r *runwayRig) turn(ctx context.Context, t *testing.T) sqlcgen.Turn {
	t.Helper()
	row, err := narvipg.NewTurnStore(r.pool).Get(ctx, r.turnID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return row
}

// commandsOf is every command sent of type typ, in order.
func commandsOf(t *testing.T, c *fakeSendCommander, typ string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, payload := range sentPayloads(c) {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("decode a sent command: %v", err)
		}
		if head.Type == typ {
			out = append(out, payload)
		}
	}
	return out
}

// onlySnapshotCommand is the one snapshot command sent, failing unless
// exactly one was.
func onlySnapshotCommand(t *testing.T, c *fakeSendCommander) sandboxws.Snapshot {
	t.Helper()
	sent := commandsOf(t, c, "snapshot")
	if len(sent) != 1 {
		t.Fatalf("%d snapshot commands sent, want 1 (commands %v)", len(sent), sentCommandTypes(t, c))
	}
	var cmd sandboxws.Snapshot
	if err := json.Unmarshal(sent[0], &cmd); err != nil {
		t.Fatalf("the snapshot command fails its contract: %v", err)
	}
	return cmd
}

// shutdownGens is the gen of every shutdown command sent, in order.
func shutdownGens(t *testing.T, c *fakeSendCommander) []int {
	t.Helper()
	var gens []int
	for _, payload := range commandsOf(t, c, "shutdown") {
		var cmd sandboxws.Shutdown
		if err := json.Unmarshal(payload, &cmd); err != nil {
			t.Fatalf("a shutdown command fails its contract: %v", err)
		}
		gens = append(gens, cmd.Gen)
	}
	return gens
}

// runwayGateCount is sandbox_runway_gate_total{action, reason}.
func runwayGateCount(ctx context.Context, t *testing.T, action sandbox.RunwayAction, reason sandbox.RunwayReason) int64 {
	t.Helper()
	return counterByAttrs(ctx, t, "sandbox_runway_gate_total", map[string]string{"action": string(action), "reason": string(reason)})
}

// rotationRestoreCount is sandbox_rotation_restore_total{outcome}.
func rotationRestoreCount(ctx context.Context, t *testing.T, outcome string) int64 {
	t.Helper()
	return counterByAttrs(ctx, t, "sandbox_rotation_restore_total", map[string]string{"outcome": outcome})
}

func counterByAttrs(ctx context.Context, t *testing.T, name string, attrs map[string]string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
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
				t.Fatalf("%s data = %T, want metricdata.Sum[int64]", name, m.Data)
			}
		points:
			for _, dp := range sum.DataPoints {
				for k, want := range attrs {
					if v, ok := dp.Attributes.Value(attribute.Key(k)); !ok || v.AsString() != want {
						continue points
					}
				}
				total += dp.Value
			}
		}
	}
	return total
}

// warningRow is one persisted warning event.
type warningRow struct {
	messageID string
	gen       int
	message   string
}

func runwaySessionWarnings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []warningRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT message_id, (payload->>'gen')::int, payload->>'message' FROM events
		WHERE session_id = $1 AND type = 'warning' ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("read the warnings: %v", err)
	}
	var out []warningRow
	for rows.Next() {
		var w warningRow
		if err := rows.Scan(&w.messageID, &w.gen, &w.message); err != nil {
			rows.Close()
			t.Fatalf("scan a warning: %v", err)
		}
		out = append(out, w)
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		t.Fatalf("read the warnings: %v", iterErr)
	}
	return out
}

// fallbackWarnings is the rotation fallback warnings of the session.
func fallbackWarnings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []warningRow {
	t.Helper()
	var out []warningRow
	for _, w := range runwaySessionWarnings(ctx, t, pool, sessionID) {
		if strings.HasPrefix(w.messageID, rotationFallbackMessageIDPrefix) {
			out = append(out, w)
		}
	}
	return out
}

// backdateRotation moves the rotation's start and its snapshot's start by
// d into the past, on the database's clock, and the dispatch timer due
// now, as if d had passed.
func backdateRotation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET rotation_started_at = rotation_started_at - make_interval(secs => $2::double precision),
		snapshot_started_at = snapshot_started_at - make_interval(secs => $2::double precision) WHERE session_id = $1`, sessionID, d.Seconds()); err != nil {
		t.Fatalf("backdate the rotation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, sessionID, TimerDispatch); err != nil {
		t.Fatalf("bring the dispatch timer due: %v", err)
	}
}

// pumpDue delivers every due timer, as the pump does, and waits for the
// actor to have handled it.
func (r *runwayRig) pumpDue(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := r.registry.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	barrier(ctx, t, r.actor)
}

// waitRestored waits until the provider has been asked for n restores.
func (r *runwayRig) waitRestored(t *testing.T, n int) fakeRestoreCall {
	t.Helper()
	waitUntil(t, 5*time.Second, func() bool { return r.provider.restoreCallCount() == n })
	return r.provider.lastRestoreCall()
}

// bootGen moves gen, the sandbox's new gen, to ready, as its boot would,
// and evaluates the dispatch, as the ready's own evaluation does.
func (r *runwayRig) bootGen(ctx context.Context, t *testing.T, gen int) {
	t.Helper()
	tag, err := r.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', last_seen_at = now() WHERE session_id = $1 AND gen = $2`, r.sessionID, gen)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("boot gen %d: %v (%d rows)", gen, err, tag.RowsAffected())
	}
	settle(ctx, t, r.actor)
}

// TestRunwayRotation_BelowTheFloorRotatesBeforeDispatch is row 137's
// rotation (technical plan §35.3): a turn queued for a sandbox under its
// runway threshold is not sent to it. The sandbox's snapshot is taken
// first, the turn still pending; its report retires the gen -- `shutdown`
// sent to it, its provider object stopped -- and the next evaluation
// restores the rotation's own snapshot under gen 2 with a fresh deadline,
// through the ordinary restore path. The turn then runs on gen 2 and
// completes: never failed.
func TestRunwayRotation_BelowTheFloorRotatesBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute})
	rotatesBefore := runwayGateCount(ctx, t, sandbox.RunwayRotate, sandbox.RunwayReasonBelowThreshold)
	takenBefore := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonRotationSnapshotTaken)
	rotatedBefore := rotationRestoreCount(ctx, t, rotationOutcomeRotated)

	settle(ctx, t, rig.actor)

	cmd := onlySnapshotCommand(t, rig.commander)
	if got := sentCommandTypes(t, rig.commander); len(got) != 1 {
		t.Fatalf("commands = %v, want the rotation's snapshot alone: nothing else goes to a sandbox under its threshold", got)
	}
	if cmd.Gen != 1 || cmd.SessionId != rig.sessionID.String() {
		t.Fatalf("snapshot command = %+v, want gen 1 of the session", cmd)
	}
	row := rig.sandbox(ctx, t)
	if row.Status != sqlcgen.SandboxStatusSnapshotting || row.Gen != 1 || !rotating(row) || !row.RotationStartedAt.Valid ||
		row.PendingSnapshotMessageID == nil || *row.PendingSnapshotMessageID != cmd.MessageId || !row.SnapshotStartedAt.Valid {
		t.Fatalf("sandbox after the rotation started: %s gen %d rotation %v pending %v started %v; want snapshotting gen 1 rotating, waiting on the command",
			row.Status, row.Gen, row.RotationGen, row.PendingSnapshotMessageID, row.SnapshotStartedAt.Valid)
	}
	if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusPending || got.DispatchedAt.Valid {
		t.Fatalf("turn while its sandbox rotates: %s, dispatched %v; want pending", got.Status, got.DispatchedAt.Valid)
	}
	wait := platform.DefaultTimeouts().RotationSnapshotWait
	if lead, ok := dispatchTimerLead(ctx, t, rig.pool, rig.sessionID); !ok || lead > wait || lead < wait-10*time.Second {
		t.Fatalf("dispatch timer in %s (%v), want at the rotation's snapshot wait, %s", lead, ok, wait)
	}
	if got := runwayGateCount(ctx, t, sandbox.RunwayRotate, sandbox.RunwayReasonBelowThreshold) - rotatesBefore; got != 1 {
		t.Fatalf("sandbox_runway_gate_total{rotate,below_threshold} moved by %d, want 1", got)
	}

	deliver(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, runwayRotationSnap, ""))

	call := rig.waitRestored(t, 1)
	if call.snapshotID != runwayRotationSnap || call.spec.Gen != 2 {
		t.Fatalf("RestoreFromSnapshot(%q) for gen %d, want the rotation's own %q for gen 2", call.snapshotID, call.spec.Gen, runwayRotationSnap)
	}
	if n := rig.provider.callCount(); n != 0 {
		t.Fatalf("CreateSandbox called %d times, want none: a rotation is a restore", n)
	}
	if gens := shutdownGens(t, rig.commander); len(gens) != 1 || gens[0] != 1 {
		t.Fatalf("shutdown sent to gens %v, want gen 1 once", gens)
	}
	if stopped := rig.provider.stoppedProviderIDs(); len(stopped) != 1 || stopped[0] != runwayProviderGen1 {
		t.Fatalf("StopSandbox(%v), want %s once", stopped, runwayProviderGen1)
	}
	waitUntil(t, 5*time.Second, func() bool { return rig.sandbox(ctx, t).Status == sqlcgen.SandboxStatusConnecting })
	row = rig.sandbox(ctx, t)
	runway := rig.runway(ctx, t)
	if row.Gen != 2 || rotating(row) || row.RotationGen != nil || row.RotationStartedAt.Valid ||
		!runway.DeadlineKnown || time.Duration(runway.RunwayNanos) < 2*time.Hour-time.Minute || row.ProviderID == nil || *row.ProviderID != runwayRestoredObject {
		t.Fatalf("sandbox after the restore: gen %d rotation %v started %v, deadline known %v runway %v, object %v; want gen 2, no rotation, a fresh 2h deadline, the restored object",
			row.Gen, row.RotationGen, row.RotationStartedAt.Valid, runway.DeadlineKnown, time.Duration(runway.RunwayNanos), row.ProviderID)
	}
	if n := len(fallbackWarnings(ctx, t, rig.pool, rig.sessionID)); n != 0 {
		t.Fatalf("%d fallback warnings, want none: the rotation's own snapshot was restored", n)
	}
	if got := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonRotationSnapshotTaken) - takenBefore; got != 1 {
		t.Fatalf("sandbox_runway_gate_total{retire,rotation_snapshot_taken} moved by %d, want 1", got)
	}
	if got := rotationRestoreCount(ctx, t, rotationOutcomeRotated) - rotatedBefore; got != 1 {
		t.Fatalf("sandbox_rotation_restore_total{rotated} moved by %d, want 1", got)
	}
	if n := promptCount(rig.commander); n != 0 {
		t.Fatalf("%d prompts sent before the replacement was up, want none", n)
	}

	rig.bootGen(ctx, t, 2)
	prompts := sentPromptsOf(t, rig.commander)
	if len(prompts) != 1 || prompts[0].Gen != 2 {
		t.Fatalf("prompts %+v, want one, to gen 2", prompts)
	}
	deliver(ctx, t, rig.actor, completedEvent(2))
	waitUntil(t, 5*time.Second, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	if got := rig.turn(ctx, t); got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 2 || got.EndReason != nil {
		t.Fatalf("turn = dispatched on gen %v, end reason %v; want completed on gen 2", got.DispatchedSandboxGen, got.EndReason)
	}
}

// TestRunwayRotation_RaisingTheFloorBringsTheRotationForward is the actor
// half of row 137's amended monotonicity test: at the shipped 2h lifetime,
// with 15 minutes of runway, a 13-minute floor dispatches the turn and a
// 19-minute floor, read from the actor's own timeouts, rotates first. A
// floor read but never compared passes neither.
func TestRunwayRotation_RaisingTheFloorBringsTheRotationForward(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		floor  time.Duration
		rotate bool
	}{
		{floor: 13 * time.Minute, rotate: false},
		{floor: 19 * time.Minute, rotate: true},
	} {
		t.Run(tc.floor.String(), func(t *testing.T) {
			to := platform.DefaultTimeouts()
			to.RotationRunwayFloor = tc.floor
			if err := to.Validate(); err != nil {
				t.Fatalf("a %s floor is not a valid configuration: %v", tc.floor, err)
			}
			rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 15 * time.Minute, timeouts: &to})
			settle(ctx, t, rig.actor)

			snapshots, prompts := len(commandsOf(t, rig.commander, "snapshot")), promptCount(rig.commander)
			if tc.rotate && (snapshots != 1 || prompts != 0 || !rotating(rig.sandbox(ctx, t))) {
				t.Fatalf("floor %s: %d snapshots, %d prompts; want the rotation first, no prompt", tc.floor, snapshots, prompts)
			}
			if !tc.rotate && (snapshots != 0 || prompts != 1 || rotating(rig.sandbox(ctx, t))) {
				t.Fatalf("floor %s: %d snapshots, %d prompts; want the turn dispatched, no rotation", tc.floor, snapshots, prompts)
			}
		})
	}
}

// TestRunwayRotation_Declined_DispatchesAsBefore: a rotation the gate
// cannot make is declined, and the turn is dispatched as before, so the
// queue never stalls on the gate -- with no deadline known, a provider that
// takes no snapshots, a session that needs a container daemon (never
// snapshotted, §27.8), and a suspect sandbox (no suspect -> snapshotting
// edge). Each with five minutes of runway, under the threshold.
func TestRunwayRotation_Declined_DispatchesAsBefore(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		reason sandbox.RunwayReason
		opts   func(ctx context.Context, t *testing.T) runwayRigOptions
	}{
		{name: "DeadlineUnknown", reason: sandbox.RunwayReasonDeadlineUnknown, opts: func(context.Context, *testing.T) runwayRigOptions {
			return runwayRigOptions{noDeadline: true}
		}},
		{name: "NoSnapshotCapability", reason: sandbox.RunwayReasonNoSnapshot, opts: func(context.Context, *testing.T) runwayRigOptions {
			return runwayRigOptions{runway: 5 * time.Minute, provider: &fakeSpawnProvider{explicitStop: true, noSnapshots: true}}
		}},
		{name: "ContainerDaemonRequired", reason: sandbox.RunwayReasonNoSnapshot, opts: func(ctx context.Context, t *testing.T) runwayRigOptions {
			pool := newTestPool(t)
			env := createTestEnvironmentWithDocker(ctx, t, pool)
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, EnvironmentID: env})
			if err != nil {
				t.Fatalf("create the session: %v", err)
			}
			return runwayRigOptions{pool: pool, runway: 5 * time.Minute, sessionID: session.ID, provider: &fakeSpawnProvider{explicitStop: true, dockerSupported: true}}
		}},
		{name: "Suspect", reason: sandbox.RunwayReasonSuspect, opts: func(context.Context, *testing.T) runwayRigOptions {
			return runwayRigOptions{runway: 5 * time.Minute, status: sqlcgen.SandboxStatusSuspect}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := runwayGateCount(ctx, t, sandbox.RunwayPass, tc.reason)
			rig := newRunwayRig(ctx, t, tc.opts(ctx, t))
			settle(ctx, t, rig.actor)

			if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "prompt" {
				t.Fatalf("commands = %v, want the prompt alone: a declined rotation dispatches as before", got)
			}
			row := rig.sandbox(ctx, t)
			if row.Gen != 1 || rotating(row) || len(rig.provider.stoppedProviderIDs()) != 0 {
				t.Fatalf("sandbox gen %d rotation %v stopped %v; want gen 1 untouched", row.Gen, row.RotationGen, rig.provider.stoppedProviderIDs())
			}
			if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
				t.Fatalf("turn %s, want processing", got.Status)
			}
			if got := runwayGateCount(ctx, t, sandbox.RunwayPass, tc.reason) - before; got != 1 {
				t.Fatalf("sandbox_runway_gate_total{pass,%s} moved by %d, want 1", tc.reason, got)
			}
		})
	}
}

// TestRunwayGate_PastItsDeadline_MarkedStoppedAndRestored: a sandbox whose
// status still reads live past its deadline -- it died idle at its
// deadline, or is about to -- is retired to stopped before anything is
// sent to it, ready, snapshotting or suspect alike, and the next
// evaluation restores its last snapshot. Nothing but `shutdown` goes to the
// old gen, and no fallback warning is written: it was not rotating.
func TestRunwayGate_PastItsDeadline_MarkedStoppedAndRestored(t *testing.T) {
	ctx := context.Background()
	for _, status := range []sqlcgen.SandboxStatus{sqlcgen.SandboxStatusReady, sqlcgen.SandboxStatusSnapshotting, sqlcgen.SandboxStatusSuspect} {
		t.Run(string(status), func(t *testing.T) {
			before := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonDeadlinePassed)
			rig := newRunwayRig(ctx, t, runwayRigOptions{runway: -time.Minute, status: status})
			settle(ctx, t, rig.actor)

			call := rig.waitRestored(t, 1)
			if call.snapshotID != runwayPreviousSnap || call.spec.Gen != 2 {
				t.Fatalf("RestoreFromSnapshot(%q) for gen %d, want the last snapshot %q for gen 2", call.snapshotID, call.spec.Gen, runwayPreviousSnap)
			}
			if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "shutdown" {
				t.Fatalf("commands = %v, want shutdown alone: nothing else goes to a sandbox past its deadline", got)
			}
			if gens := shutdownGens(t, rig.commander); gens[0] != 1 {
				t.Fatalf("shutdown to gen %d, want gen 1", gens[0])
			}
			if stopped := rig.provider.stoppedProviderIDs(); len(stopped) != 1 || stopped[0] != runwayProviderGen1 {
				t.Fatalf("StopSandbox(%v), want %s once", stopped, runwayProviderGen1)
			}
			row := rig.sandbox(ctx, t)
			if row.Gen != 2 || row.PendingSnapshotMessageID != nil || row.SnapshotStartedAt.Valid {
				t.Fatalf("sandbox gen %d pending %v started %v; want gen 2 with no snapshot outstanding", row.Gen, row.PendingSnapshotMessageID, row.SnapshotStartedAt.Valid)
			}
			if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusPending {
				t.Fatalf("turn %s, want pending for the replacement", got.Status)
			}
			if n := len(fallbackWarnings(ctx, t, rig.pool, rig.sessionID)); n != 0 {
				t.Fatalf("%d fallback warnings, want none: the gen was not rotating", n)
			}
			if got := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonDeadlinePassed) - before; got != 1 {
				t.Fatalf("sandbox_runway_gate_total{retire,deadline_passed} moved by %d, want 1", got)
			}
		})
	}
}

// assertFellBack checks the restore of a rotation whose snapshot was not
// taken: snap-previous restored for gen 2, never a fresh spawn, and the one
// fallback warning at gen 2, naming it and when it was taken.
func assertFellBack(ctx context.Context, t *testing.T, rig *runwayRig) {
	t.Helper()
	call := rig.waitRestored(t, 1)
	if call.snapshotID != runwayPreviousSnap || call.spec.Gen != 2 {
		t.Fatalf("RestoreFromSnapshot(%q) for gen %d, want the previous snapshot %q for gen 2", call.snapshotID, call.spec.Gen, runwayPreviousSnap)
	}
	if n := rig.provider.callCount(); n != 0 {
		t.Fatalf("CreateSandbox called %d times, want none", n)
	}
	warnings := fallbackWarnings(ctx, t, rig.pool, rig.sessionID)
	if len(warnings) != 1 {
		t.Fatalf("%d fallback warnings, want 1", len(warnings))
	}
	w := warnings[0]
	minted := rig.sandbox(ctx, t).SnapshotMintedAt.Time.UTC().Format("2006-01-02 15:04 UTC")
	if w.messageID != rotationFallbackMessageIDPrefix+"1" || w.gen != 2 ||
		!strings.Contains(w.message, runwayPreviousSnap) || !strings.Contains(w.message, "(taken "+minted+")") || !strings.Contains(w.message, "never forced") {
		t.Fatalf("fallback warning = %+v, want sandbox_rotation_fallback:1 at gen 2 naming %s taken %s", w, runwayPreviousSnap, minted)
	}
}

// TestRunwayRotation_SnapshotNotSent_FallsBackToThePreviousSnapshot: the
// rotation's snapshot command finds no live connection -- the sandbox is
// gone. The sandbox goes back to ready, the next evaluation retires the
// gen (rotation_snapshot_failed), and the restore falls back to the
// previous snapshot, recording the failure.
func TestRunwayRotation_SnapshotNotSent_FallsBackToThePreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	failedBefore := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonRotationSnapshotFailed)
	fellBackBefore := rotationRestoreCount(ctx, t, rotationOutcomeFellBack)
	logs := captureDefaultLoggerLocked(t) // before the rig: the actor takes its logger at hydrate
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute,
		commander: &fakeSendCommander{failTypes: map[string]error{"snapshot": ports.ErrNoLiveSandboxConnection}}})
	settle(ctx, t, rig.actor)

	assertFellBack(ctx, t, rig)
	if line := logs.entry(t, "sessionactor: runway gate: the rotation's snapshot command could not be sent; the sandbox goes back to ready and its gen is retired, falling back to its previous snapshot"); line["reason"] != "send_failed" || line["no_live_connection"] != true {
		t.Fatalf("send failure log = %v, want reason send_failed, no live connection", line)
	}
	if got := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonRotationSnapshotFailed) - failedBefore; got != 1 {
		t.Fatalf("sandbox_runway_gate_total{retire,rotation_snapshot_failed} moved by %d, want 1", got)
	}
	if got := rotationRestoreCount(ctx, t, rotationOutcomeFellBack) - fellBackBefore; got != 1 {
		t.Fatalf("sandbox_rotation_restore_total{fell_back} moved by %d, want 1", got)
	}
}

// TestRunwayRotation_SnapshotNeverReported_FallsBackAtTheWait: the agent
// never reports the rotation's snapshot -- its mint failed, which it
// reports with no frame. The dispatch timer the rotation armed is due at
// the snapshot wait, and its firing retires the gen
// (rotation_snapshot_not_reported) and falls back.
func TestRunwayRotation_SnapshotNeverReported_FallsBackAtTheWait(t *testing.T) {
	ctx := context.Background()
	before := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonRotationSnapshotNotReported)
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute})
	settle(ctx, t, rig.actor)
	cmd := onlySnapshotCommand(t, rig.commander)
	wait := platform.DefaultTimeouts().RotationSnapshotWait
	if lead, ok := dispatchTimerLead(ctx, t, rig.pool, rig.sessionID); !ok || lead > wait || lead < wait-10*time.Second {
		t.Fatalf("dispatch timer in %s (%v), want at the wait, %s: the wait is durable", lead, ok, wait)
	}
	// Within the wait, an evaluation holds.
	settle(ctx, t, rig.actor)
	if n := rig.provider.restoreCallCount(); n != 0 || !rotating(rig.sandbox(ctx, t)) {
		t.Fatalf("within the wait: %d restores, rotating %v; want the rotation still waiting", n, rotating(rig.sandbox(ctx, t)))
	}

	backdateRotation(ctx, t, rig.pool, rig.sessionID, wait)
	rig.pumpDue(ctx, t)

	assertFellBack(ctx, t, rig)
	if got := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonRotationSnapshotNotReported) - before; got != 1 {
		t.Fatalf("sandbox_runway_gate_total{retire,rotation_snapshot_not_reported} moved by %d, want 1", got)
	}
	if row := rig.sandbox(ctx, t); row.PendingSnapshotMessageID != nil && *row.PendingSnapshotMessageID == cmd.MessageId {
		t.Fatal("the unreported snapshot is still pending after the retirement")
	}
}

// TestRunwayRotation_DeadlinePassesWhileWaiting_FallsBack: the deadline
// passes while the rotation waits for its snapshot. The deadline row comes
// first: the gen is retired, and the restore falls back.
func TestRunwayRotation_DeadlinePassesWhileWaiting_FallsBack(t *testing.T) {
	ctx := context.Background()
	before := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonDeadlinePassed)
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute})
	settle(ctx, t, rig.actor)
	onlySnapshotCommand(t, rig.commander)

	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET lifetime_deadline_at = now() - interval '1 second' WHERE session_id = $1`, rig.sessionID); err != nil {
		t.Fatal(err)
	}
	settle(ctx, t, rig.actor)

	assertFellBack(ctx, t, rig)
	if got := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonDeadlinePassed) - before; got != 1 {
		t.Fatalf("sandbox_runway_gate_total{retire,deadline_passed} moved by %d, want 1", got)
	}
}

// TestRunwayRotation_AFailedAttemptNeverBecomesARestorePoint is row 137's
// acceptance: a rotation whose snapshot never reported falls back, and its
// report, arriving late, never becomes a restore point. Each guard is
// checked where it acts: the retirement forgets the pending command; a
// report to the stopped gen is not taken as a snapshot (it is stored, the
// row's snapshot unchanged); one after the restore is fenced off by gen,
// not stored. The restore is of the previous snapshot.
func TestRunwayRotation_AFailedAttemptNeverBecomesARestorePoint(t *testing.T) {
	ctx := context.Background()
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute})
	settle(ctx, t, rig.actor)
	cmd := onlySnapshotCommand(t, rig.commander)

	// Hold the restore back, so the stopped gen can be seen: the spawn
	// circuit breaker is open.
	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET spawn_failure_count = 3, last_spawn_failure_at = now() WHERE session_id = $1`, rig.sessionID); err != nil {
		t.Fatal(err)
	}
	backdateRotation(ctx, t, rig.pool, rig.sessionID, platform.DefaultTimeouts().RotationSnapshotWait)
	rig.pumpDue(ctx, t)

	row := rig.sandbox(ctx, t)
	if row.Status != sqlcgen.SandboxStatusStopped || row.Gen != 1 || rig.provider.restoreCallCount() != 0 {
		t.Fatalf("sandbox %s gen %d, %d restores; want gen 1 stopped, the restore held back", row.Status, row.Gen, rig.provider.restoreCallCount())
	}
	// Guard 1: the retirement forgot the command.
	if row.PendingSnapshotMessageID != nil || row.SnapshotStartedAt.Valid {
		t.Fatalf("pending snapshot %v (started %v) after the retirement, want none", row.PendingSnapshotMessageID, row.SnapshotStartedAt.Valid)
	}

	// Guard 2: the late report reaches the stopped gen, and is no snapshot.
	if outcome := sendSandboxEvent(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, "snap-late", "")); !outcome.Persisted {
		t.Fatal("the stopped gen's late report was not stored; its gen is still the row's")
	}
	barrier(ctx, t, rig.actor)
	if row := rig.sandbox(ctx, t); row.SnapshotID == nil || *row.SnapshotID != runwayPreviousSnap || row.Status != sqlcgen.SandboxStatusStopped {
		t.Fatalf("after the late report to the stopped gen: snapshot %v, %s; want %s kept, still stopped", row.SnapshotID, row.Status, runwayPreviousSnap)
	}

	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET spawn_failure_count = 0, last_spawn_failure_at = NULL WHERE session_id = $1`, rig.sessionID); err != nil {
		t.Fatal(err)
	}
	settle(ctx, t, rig.actor)
	assertFellBack(ctx, t, rig)

	// Guard 3: after the restore, the old gen's report is fenced off.
	if outcome := sendSandboxEvent(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, "snap-later", "")); outcome.Persisted {
		t.Fatal("the retired gen's report after the restore was stored, want it fenced off by gen")
	}
	barrier(ctx, t, rig.actor)
	if row := rig.sandbox(ctx, t); row.SnapshotID == nil || *row.SnapshotID != runwayPreviousSnap || row.Gen != 2 {
		t.Fatalf("after the late report to the retired gen: snapshot %v gen %d; want %s at gen 2", row.SnapshotID, row.Gen, runwayPreviousSnap)
	}
}

// TestRunwayRotation_FallbackWarningNamesTheSnapshot: the fallback is a
// persisted session warning, once, at the new gen, under
// sandbox_rotation_fallback:{retired gen}, naming the snapshot restored and
// when it was taken -- stored for the timeline, posted to no channel. The
// restore is a restore (RestoreFromSnapshot, never CreateSandbox), so it
// is no fresh lineage. An evaluation after it writes no second warning.
func TestRunwayRotation_FallbackWarningNamesTheSnapshot(t *testing.T) {
	ctx := context.Background()
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute,
		commander: &fakeSendCommander{failTypes: map[string]error{"snapshot": ports.ErrNoLiveSandboxConnection}}})
	settle(ctx, t, rig.actor)
	assertFellBack(ctx, t, rig)

	if n := countRows(ctx, t, rig.pool, `SELECT count(*) FROM outbox WHERE session_id = $1`, rig.sessionID); n != 0 {
		t.Fatalf("%d outbox rows, want none: the warning is the session's timeline alone", n)
	}
	rig.bootGen(ctx, t, 2)
	settle(ctx, t, rig.actor)
	if n := len(fallbackWarnings(ctx, t, rig.pool, rig.sessionID)); n != 1 {
		t.Fatalf("%d fallback warnings after more evaluations, want the one", n)
	}
}

// TestRunwayRotation_RestorePassesTheProvenanceDecision: the rotation only
// retires the gen; the restore that follows is the ordinary decision's, so
// a previous snapshot whose provenance this control plane does not restore
// is refused as any other is (technical plan §35.5b): spawned fresh, the
// refusal's own warning written and the snapshot cleared, and no fallback
// warning, since nothing was restored.
func TestRunwayRotation_RestorePassesTheProvenanceDecision(t *testing.T) {
	ctx := context.Background()
	freshBefore := rotationRestoreCount(ctx, t, rotationOutcomeFresh)
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute,
		commander: &fakeSendCommander{failTypes: map[string]error{"snapshot": ports.ErrNoLiveSandboxConnection}},
		beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_agent_protocol = '0.9.0' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
		}})
	settle(ctx, t, rig.actor)

	waitUntil(t, 5*time.Second, func() bool { return rig.provider.callCount() == 1 })
	if n := rig.provider.restoreCallCount(); n != 0 {
		t.Fatalf("RestoreFromSnapshot called %d times, want none: the snapshot's provenance is refused", n)
	}
	if n := len(fallbackWarnings(ctx, t, rig.pool, rig.sessionID)); n != 0 {
		t.Fatalf("%d fallback warnings, want none: nothing was restored", n)
	}
	refusals := 0
	for _, w := range runwaySessionWarnings(ctx, t, rig.pool, rig.sessionID) {
		if w.messageID == snapshotRefusalMessageIDPrefix+runwayPreviousSnap {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("%d refusal warnings for %s, want 1", refusals, runwayPreviousSnap)
	}
	if row := rig.sandbox(ctx, t); row.Gen != 2 || row.SnapshotID != nil {
		t.Fatalf("sandbox gen %d snapshot %v; want gen 2 spawned fresh, the refused snapshot cleared", row.Gen, row.SnapshotID)
	}
	if got := rotationRestoreCount(ctx, t, rotationOutcomeFresh) - freshBefore; got != 1 {
		t.Fatalf("sandbox_rotation_restore_total{fresh} moved by %d, want 1", got)
	}
}

// TestRunwayRotation_ReviewTurn_NoCheckoutToAGenAboutToRotate_ChecksOutOnTheNewGen:
// the gate runs before a review turn's checkout (technical plan §21.1), so
// no checkout is sent to a gen about to rotate; the new gen is asked again,
// a checkout being requested per gen, and the turn is sent once it holds
// the head.
func TestRunwayRotation_ReviewTurn_NoCheckoutToAGenAboutToRotate_ChecksOutOnTheNewGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/rotating-review", 811, pgtype.UUID{})
	if _, err := f.prSessions.ClearPendingRetriggerHeadSHA(ctx, "acme/rotating-review", 811, holdPushedHead); err != nil {
		t.Fatal(err)
	}
	seedRunwaySandbox(ctx, t, pool, f.sessionID, sqlcgen.SandboxStatusReady, 10*time.Minute, true)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET review_checkout_gen = 1 WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, false)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)
	rig.provider.explicitStop = true
	rig.provider.nextRestoreRef = ports.SandboxRef{ProviderID: runwayRestoredObject}

	settle(ctx, t, rig.actor)
	if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "snapshot" {
		t.Fatalf("commands = %v, want the rotation's snapshot alone: no checkout to a gen about to rotate", got)
	}
	cmd := onlySnapshotCommand(t, rig.commander)
	deliver(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, runwayRotationSnap, ""))
	waitUntil(t, 5*time.Second, func() bool { return rig.provider.restoreCallCount() == 1 })
	if n := len(sentCheckouts(t, rig.commander)); n != 0 {
		t.Fatalf("%d checkouts sent before the new gen was up, want none", n)
	}

	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', review_checkout_gen = 2 WHERE session_id = $1 AND gen = 2`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	settle(ctx, t, rig.actor)
	checkout := lastCheckout(t, rig.commander, 1)
	if checkout.Gen != 2 || checkout.Repos[0].Sha != coHead {
		t.Fatalf("checkout = %+v, want one to gen 2 for %s", checkout, coHead)
	}
	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusPending || got.CheckoutGen == nil || *got.CheckoutGen != 2 {
		t.Fatalf("review turn %s, checkout gen %v; want pending on gen 2's checkout", got.Status, got.CheckoutGen)
	}
	deliver(ctx, t, rig.actor, checkoutReply(t, checkout, 2, "checked_out", strp(coHead), strp(coHead), nil))
	if prompts := sentPromptsOf(t, rig.commander); len(prompts) != 1 || prompts[0].Gen != 2 {
		t.Fatalf("prompts %+v, want one, to gen 2", prompts)
	}
}

// TestRunwayRotation_CappedSessionNeverRotates: the session spend cap's
// dispatch-time guard (technical plan §40.1) runs before the gate, so a
// session that has spent its cap ends its queued turn and never rotates
// its sandbox for it, however short its runway.
func TestRunwayRotation_CappedSessionNeverRotates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := capReviewSession(ctx, t, pool, "acme/capped-rotation", "1.00")
	rig := newRunwayRig(ctx, t, runwayRigOptions{pool: pool, runway: 5 * time.Minute, sessionID: sessionID,
		beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
			// The session has spent past its cap since the turn was queued.
			if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, prompt, cost_usd, dispatched_at, completed_at) VALUES ($1, 'completed', 'spent it', 1.40, now(), now())`, sessionID); err != nil {
				t.Fatal(err)
			}
		}})
	settle(ctx, t, rig.actor)

	waitUntil(t, 5*time.Second, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusFailed })
	if got := rig.turn(ctx, t); got.EndReason == nil || *got.EndReason != turn.EndReasonSpendCap {
		t.Fatalf("queued turn end reason %v, want spend_cap", got.EndReason)
	}
	if got := sentCommandTypes(t, rig.commander); len(got) != 0 {
		t.Fatalf("commands = %v, want none: a capped session never rotates", got)
	}
	if row := rig.sandbox(ctx, t); rotating(row) || row.Status != sqlcgen.SandboxStatusReady || row.Gen != 1 {
		t.Fatalf("sandbox %s gen %d rotation %v; want gen 1 ready, untouched", row.Status, row.Gen, row.RotationGen)
	}
}

// TestRunwayRotation_FreezeDoesNotHoldIt: a rotation is not an automatic
// action (technical plan §40.2: the dispatch of a turn already enqueued is
// not one), so the autonomy freeze does not hold it.
func TestRunwayRotation_FreezeDoesNotHoldIt(t *testing.T) {
	ctx := context.Background()
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 10 * time.Minute})
	freezeAutonomyForActorTest(ctx, t, rig.pool)
	settle(ctx, t, rig.actor)

	onlySnapshotCommand(t, rig.commander)
	if row := rig.sandbox(ctx, t); !rotating(row) {
		t.Fatal("the freeze held the rotation, want it started")
	}
}

// TestRunwayRotation_HeldWhileADeliveryIsOpen: a completed turn's push and
// pull request are still being delivered, and the agent runs push and
// snapshot one after the other on its read loop, so the rotation does not
// start: the turn is held, looked at again at the delivery's bound. The
// push_complete ends the delivery, and the rotation starts then.
func TestRunwayRotation_HeldWhileADeliveryIsOpen(t *testing.T) {
	ctx := context.Background()
	before := runwayGateCount(ctx, t, sandbox.RunwayHold, sandbox.RunwayReasonDeliveryOpen)
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 12 * time.Minute,
		beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = now() - interval '1 minute' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
		}})
	settle(ctx, t, rig.actor)

	if got := sentCommandTypes(t, rig.commander); len(got) != 0 {
		t.Fatalf("commands = %v, want none while the delivery is open", got)
	}
	window := platform.DefaultTimeouts().MCPStatusDeliveryWindow
	if lead, ok := dispatchTimerLead(ctx, t, rig.pool, rig.sessionID); !ok || lead > window-time.Minute || lead < window-time.Minute-10*time.Second {
		t.Fatalf("dispatch timer in %s (%v), want at the delivery's bound, %s", lead, ok, window-time.Minute)
	}
	if got := runwayGateCount(ctx, t, sandbox.RunwayHold, sandbox.RunwayReasonDeliveryOpen) - before; got != 1 {
		t.Fatalf("sandbox_runway_gate_total{hold,delivery_open} moved by %d, want 1", got)
	}

	messageID := "push-complete-" + rig.sessionID.String()
	raw := pushCompleteRawWithMessageID(t, messageID, rig.sessionID.String(), 1, "widgets", "feature", "cafef00d00000000000000000000000000000000")
	deliver(ctx, t, rig.actor, SandboxEvent{Type: "push_complete", Gen: 1, MessageID: messageID, Raw: raw})

	onlySnapshotCommand(t, rig.commander)
	if row := rig.sandbox(ctx, t); !rotating(row) || row.PrDeliveryStartedAt.Valid {
		t.Fatalf("after push_complete: rotating %v, delivery %v; want the rotation started, the delivery over", rotating(row), row.PrDeliveryStartedAt.Valid)
	}
}

// TestRunwayGate_InFlightReenqueue_NeverRotates_ExpiredIsStopped: a turn in
// flight re-sent to a newer gen passes the gate too. Nothing is rotated
// under it -- under the threshold, the prompt is re-sent as before -- but a
// gen past its deadline is retired before the prompt is sent to it, and the
// re-send goes to its replacement.
func TestRunwayGate_InFlightReenqueue_NeverRotates_ExpiredIsStopped(t *testing.T) {
	ctx := context.Background()
	// inFlightOnGen0 makes the rig's turn processing, dispatched to gen 0,
	// which the sandbox's gen 1 replaced.
	inFlightOnGen0 := func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
		if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'processing', dispatched_at = now(), dispatched_sandbox_gen = 0 WHERE session_id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("under the threshold", func(t *testing.T) {
		before := runwayGateCount(ctx, t, sandbox.RunwayPass, sandbox.RunwayReasonTurnInFlight)
		rig := newRunwayRig(ctx, t, runwayRigOptions{runway: 5 * time.Minute, beforeActor: inFlightOnGen0})
		settle(ctx, t, rig.actor)
		prompts := sentPromptsOf(t, rig.commander)
		if len(prompts) != 1 || prompts[0].Gen != 1 || len(commandsOf(t, rig.commander, "snapshot")) != 0 || rotating(rig.sandbox(ctx, t)) {
			t.Fatalf("prompts %+v, commands %v; want the re-send to gen 1 and no rotation", prompts, sentCommandTypes(t, rig.commander))
		}
		if got := runwayGateCount(ctx, t, sandbox.RunwayPass, sandbox.RunwayReasonTurnInFlight) - before; got != 1 {
			t.Fatalf("sandbox_runway_gate_total{pass,turn_in_flight} moved by %d, want 1", got)
		}
	})
	t.Run("past its deadline", func(t *testing.T) {
		rig := newRunwayRig(ctx, t, runwayRigOptions{runway: -time.Minute, beforeActor: inFlightOnGen0})
		settle(ctx, t, rig.actor)
		rig.waitRestored(t, 1)
		if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "shutdown" {
			t.Fatalf("commands = %v, want shutdown alone: the prompt is not sent to a gen past its deadline", got)
		}
		rig.bootGen(ctx, t, 2)
		prompts := sentPromptsOf(t, rig.commander)
		if len(prompts) != 1 || prompts[0].Gen != 2 {
			t.Fatalf("prompts %+v, want the re-send to gen 2", prompts)
		}
		if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 2 {
			t.Fatalf("turn %s on gen %v, want processing on gen 2", got.Status, got.DispatchedSandboxGen)
		}
	})
}

// TestRunwayRotation_ARotatedGenIsRestoredNeverResumed: on a provider that
// resumes, a gen the gate retired for its runway -- rotated, or past its
// deadline -- is restored, never resumed: a resume claims the same
// provider object and carries its deadline forward (technical plan §35.2),
// which would bring back the very sandbox the rotation replaces.
func TestRunwayRotation_ARotatedGenIsRestoredNeverResumed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		opts runwayRigOptions
	}{
		{name: "rotated", opts: runwayRigOptions{runway: 10 * time.Minute,
			commander: &fakeSendCommander{failTypes: map[string]error{"snapshot": ports.ErrNoLiveSandboxConnection}}}},
		{name: "past its deadline", opts: runwayRigOptions{runway: -time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.provider = &fakeSpawnProvider{explicitStop: true, resumeSupported: true}
			rig := newRunwayRig(ctx, t, tc.opts)
			settle(ctx, t, rig.actor)
			rig.waitRestored(t, 1)
			if n := rig.provider.resumeCallCount(); n != 0 {
				t.Fatalf("ResumeSandbox called %d times, want none", n)
			}
		})
	}
}

// TestHandleEnsureDispatched_PendingTurnWhileSnapshotting_HeldNoErrorNoBackoff
// pins a turn queued behind a post-turn snapshot through the real
// evaluation (technical plan §35.3; §3.3's "complete turn -> trigger
// snapshot -> ... -> dispatch next"): it is held, not failed. The
// evaluation commits, spawns, sends and moves nothing, and re-arms the
// dispatch timer at the snapshot's report wait from its start
// (SnapshotReportWait) -- never backed off, and never left unarmed. The
// snapshot's own snapshot_ready then returns the sandbox to Ready and the
// same gen takes the turn.
func TestHandleEnsureDispatched_PendingTurnWhileSnapshotting_HeldNoErrorNoBackoff(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	// Gen 1, mid post-turn snapshot started 30 seconds ago, spawned and last
	// seen an hour ago: past SpawnCooldown, SpawnReadyWait and
	// SpawnStuckTimeout alike.
	seedReadySandbox(ctx, t, pool, sessionID)
	const snapshotCommandID = "snapshot-command-1"
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'snapshotting', pending_snapshot_message_id = $2,
		snapshot_started_at = now() - interval '30 seconds',
		created_at = now() - interval '1 hour', last_seen_at = now() - interval '1 hour' WHERE session_id = $1`,
		sessionID, snapshotCommandID); err != nil {
		t.Fatalf("seed the snapshotting sandbox: %v", err)
	}
	queued := createTurnArmingDispatch(ctx, t, pool, sessionID, "the turn queued behind the snapshot")
	logs := captureDefaultLoggerLocked(t) // before GetOrSpawn: the actor takes its logger at hydrate

	commander := &fakeSendCommander{}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-object-should-never-be-used"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	settle(ctx, t, a)

	wait := platform.DefaultTimeouts().SnapshotReportWait
	lead, ok := dispatchTimerLead(ctx, t, pool, sessionID)
	if !ok || lead > wait-30*time.Second || lead < wait-40*time.Second {
		t.Fatalf("dispatch timer in %s (%v), want at the report wait from the snapshot's start, about %s", lead, ok, wait-30*time.Second)
	}
	if n := provider.callCount() + provider.restoreCallCount() + provider.resumeCallCount(); n != 0 {
		t.Errorf("provider called %d times, want none: a snapshotting sandbox is neither respawned nor replaced", n)
	}
	if n := commander.callCount(); n != 0 {
		t.Errorf("SendCommand called %d times, want none while the snapshot is in progress", n)
	}
	sandboxRow, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if sandboxRow.Status != sqlcgen.SandboxStatusSnapshotting || sandboxRow.Gen != 1 {
		t.Errorf("sandbox = %s at gen %d, want snapshotting at gen 1, untouched", sandboxRow.Status, sandboxRow.Gen)
	}
	turnStore := narvipg.NewTurnStore(pool)
	if got, err := turnStore.Get(ctx, queued.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("queued turn = %v (err %v), want it still pending", got.Status, err)
	}
	held := logs.entry(t, snapshotHeldLogMessage)
	if held["gen"] != float64(1) || held["pending_snapshot_message_id"] != snapshotCommandID || held["reason"] != string(sandbox.RunwayReasonSnapshotInProgress) {
		t.Errorf("held-turn log = %v, want gen 1, the pending snapshot command, snapshot_in_progress", held)
	}
	if secs, ok := held["snapshotting_seconds"].(float64); !ok || secs < 30 || secs > 40 {
		t.Errorf("held-turn log snapshotting_seconds = %v, want about 30 (the snapshot's start)", held["snapshotting_seconds"])
	}

	// The snapshot's own report ends the hold: the turn goes to the same gen.
	outcome := sendSandboxEvent(ctx, t, a, SandboxEvent{Type: "snapshot_ready", Gen: 1, Raw: json.RawMessage(
		`{"type":"snapshot_ready","messageId":"sr-1","sessionId":"s","gen":1,"ackId":"snapshot_ready:sr-1","snapshotId":"snap-1","commandMessageId":"` + snapshotCommandID + `"}`)})
	if !outcome.Persisted {
		t.Fatal("snapshot_ready: Persisted = false, want true")
	}
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })
	got, err := turnStore.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 1 {
		t.Errorf("queued turn = %s on gen %v, want processing on gen 1", got.Status, got.DispatchedSandboxGen)
	}
	if n := provider.callCount() + provider.restoreCallCount() + provider.resumeCallCount(); n != 0 {
		t.Errorf("provider called %d times, want none", n)
	}
}

// TestRunwayGate_ASnapshotNeverReported_IsBoundedByTheReportWait pins PR
// 393's round-1 finding F1 and its fix (technical plan §35.3): a post-turn
// snapshot that never reports -- the agent's mint failed, which it reports
// with no frame, or the agent died mid-mint -- used to hold the queued turn
// with nothing left to look again. Now the evaluation holds it for at most
// SnapshotReportWait from the snapshot's start, with the dispatch timer
// armed at that bound, and then gives the snapshot up:
//
//   - its agent silent past the heartbeat budget (F1's own setup: no start
//     recorded, created and last seen 3 hours ago, no deadline known): the
//     agent is gone, so the gen is retired and the turn runs on a
//     replacement, never sent into a socket that is gone;
//   - its agent still heard from: only the snapshot failed, so the
//     sandbox goes back to ready with its work and takes the turn, and the
//     snapshot's late report matches nothing.
func TestRunwayGate_ASnapshotNeverReported_IsBoundedByTheReportWait(t *testing.T) {
	ctx := context.Background()
	wait := platform.DefaultTimeouts().SnapshotReportWait

	// seed is F1's state: gen 1 snapshotting, its command pending, no start
	// recorded, no deadline, and a pending turn whose dispatch timer is
	// armed; silent says whether the agent was last seen 3 hours ago.
	seed := func(t *testing.T, silent bool) *runwayRig {
		t.Helper()
		rig := newRunwayRig(ctx, t, runwayRigOptions{noDeadline: true, beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
			lastSeen := "now()"
			if silent {
				lastSeen = "now() - interval '3 hours'"
			}
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'snapshotting', pending_snapshot_message_id = 'post-turn-snapshot', snapshot_started_at = NULL,
				created_at = now() - interval '3 hours', last_seen_at = `+lastSeen+` WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
		}})
		settle(ctx, t, rig.actor)
		row := rig.sandbox(ctx, t)
		if row.Status != sqlcgen.SandboxStatusSnapshotting || !row.SnapshotStartedAt.Valid {
			t.Fatalf("after the first evaluation: %s, start recorded %v; want held, the snapshot given a start", row.Status, row.SnapshotStartedAt.Valid)
		}
		if lead, ok := dispatchTimerLead(ctx, t, rig.pool, rig.sessionID); !ok || lead > wait || lead < wait-10*time.Second {
			t.Fatalf("dispatch timer in %s (%v), want at the report wait, %s: the hold is bounded", lead, ok, wait)
		}
		if n := len(sentPayloads(rig.commander)); n != 0 {
			t.Fatalf("%d commands sent within the wait, want none", n)
		}
		// The wait runs out: the timer fires.
		if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET snapshot_started_at = snapshot_started_at - make_interval(secs => $2::double precision) WHERE session_id = $1`,
			rig.sessionID, wait.Seconds()); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, rig.sessionID, TimerDispatch); err != nil {
			t.Fatal(err)
		}
		rig.pumpDue(ctx, t)
		return rig
	}

	t.Run("its agent silent: retired, the turn runs on a replacement", func(t *testing.T) {
		before := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonSnapshotAgentSilent)
		rig := seed(t, true)
		call := rig.waitRestored(t, 1)
		if call.snapshotID != runwayPreviousSnap || call.spec.Gen != 2 {
			t.Fatalf("RestoreFromSnapshot(%q) for gen %d, want the last snapshot for gen 2", call.snapshotID, call.spec.Gen)
		}
		if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "shutdown" {
			t.Fatalf("commands = %v, want shutdown alone: no prompt to an agent that is gone", got)
		}
		if got := runwayGateCount(ctx, t, sandbox.RunwayRetire, sandbox.RunwayReasonSnapshotAgentSilent) - before; got != 1 {
			t.Fatalf("sandbox_runway_gate_total{retire,snapshot_agent_silent} moved by %d, want 1", got)
		}
		rig.bootGen(ctx, t, 2)
		if prompts := sentPromptsOf(t, rig.commander); len(prompts) != 1 || prompts[0].Gen != 2 {
			t.Fatalf("prompts %+v, want one, to gen 2", prompts)
		}
	})

	t.Run("its agent heard from: reverted, the turn runs on the same gen", func(t *testing.T) {
		before := runwayGateCount(ctx, t, sandbox.RunwayRevert, sandbox.RunwayReasonSnapshotNotReported)
		rig := seed(t, false)
		waitUntil(t, 5*time.Second, func() bool { return promptCount(rig.commander) == 1 })
		if prompts := sentPromptsOf(t, rig.commander); prompts[0].Gen != 1 {
			t.Fatalf("prompt to gen %d, want gen 1: the sandbox keeps its work", prompts[0].Gen)
		}
		row := rig.sandbox(ctx, t)
		if row.Status != sqlcgen.SandboxStatusReady || row.Gen != 1 || row.PendingSnapshotMessageID != nil || row.SnapshotStartedAt.Valid {
			t.Fatalf("sandbox %s gen %d pending %v; want gen 1 ready, the snapshot forgotten", row.Status, row.Gen, row.PendingSnapshotMessageID)
		}
		if n := rig.provider.restoreCallCount() + rig.provider.callCount() + len(rig.provider.stoppedProviderIDs()); n != 0 {
			t.Fatalf("provider called %d times, want none: the sandbox is kept", n)
		}
		if got := runwayGateCount(ctx, t, sandbox.RunwayRevert, sandbox.RunwayReasonSnapshotNotReported) - before; got != 1 {
			t.Fatalf("sandbox_runway_gate_total{revert,snapshot_not_reported} moved by %d, want 1", got)
		}
		// The given-up snapshot's late report matches nothing.
		deliver(ctx, t, rig.actor, snapshotReady(1, "post-turn-snapshot", "snap-late", ""))
		if row := rig.sandbox(ctx, t); row.SnapshotID == nil || *row.SnapshotID != runwayPreviousSnap {
			t.Fatalf("snapshot %v after the late report, want %s kept", row.SnapshotID, runwayPreviousSnap)
		}
	})
}

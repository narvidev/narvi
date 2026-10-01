//go:build integration

package sessionactor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// Technical plan §3.3's prompt receipts, with the dispatching replica
// killed after its commit (§9.3 scenario 1's own kill, on two pools and a
// real lock connection, resilience_killpod_integration_test.go): replica A
// commits the dispatch and dies inside its send, the sandbox reconnects on
// the same gen to replica B, and sends its ready and three heartbeats. A
// capable agent is sent the prompt once more and the turn completes; an
// agent without the capability -- this group's review's own reproduction --
// is sent nothing, and its turn ends at turn_deadline.

// killPodReceiptRig is the session, seeded on A's pool, and A holding it
// with its send held.
type killPodReceiptRig struct {
	poolB      *pgxpool.Pool
	sessionID  pgtype.UUID
	turnID     pgtype.UUID
	turns      *narvipg.TurnStore
	commanderA *receiptCommander
}

// killAfterDispatchCommit seeds a Ready sandbox at gen 1 and a pending
// turn, has replica A dispatch it on a ready of gen 1 (capable or not),
// holds A's send, and kills A's lock backend.
func killAfterDispatchCommit(ctx context.Context, t *testing.T, timeouts platform.Timeouts, capable bool) *killPodReceiptRig {
	t.Helper()
	poolA, poolB := newTestPoolPair(t)
	sessionID := createTestSession(ctx, t, poolA)
	sandboxes := narvipg.NewSandboxStore(poolA)
	if _, err := sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	turns := narvipg.NewTurnStore(poolA)
	turnID := createPendingTurn(ctx, t, turns, sessionID, "do the thing").ID

	commanderA := &receiptCommander{}
	held, release := commanderA.holdNextSend()
	// Released last thing, so A's held send returns once the test is done
	// with it; A itself is never shut down, as a killed pod is not.
	t.Cleanup(release)
	registryA, err := NewRegistry(ctx, poolA, timeouts, nil, commanderA, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry (A): %v", err)
	}
	actorA, err := registryA.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("registryA.GetOrSpawn: %v", err)
	}
	sendSandboxEvent(ctx, t, actorA, receiptReady(1, capable))
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("replica A never reached its send")
	}
	if got, err := turns.Get(ctx, turnID); err != nil || got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn after A's commit = %+v (%v), want processing", got, err)
	}

	// Kill pod A: its lock backend, as a killed process's socket does.
	killAdvisoryLockHolder(ctx, t, poolB)
	return &killPodReceiptRig{poolB: poolB, sessionID: sessionID, turnID: turnID, turns: narvipg.NewTurnStore(poolB), commanderA: commanderA}
}

// replicaB hydrates the session on a registry over poolB, with commander.
func (k *killPodReceiptRig) replicaB(ctx context.Context, t *testing.T, timeouts platform.Timeouts, commander *receiptCommander) (*Registry, *Actor) {
	t.Helper()
	registryB, err := NewRegistry(ctx, k.poolB, timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry (B): %v", err)
	}
	// LIFO: B shuts down before its pool closes.
	t.Cleanup(k.poolB.Close)
	t.Cleanup(func() { _ = registryB.Shutdown() })
	actorB, err := registryB.GetOrSpawn(ctx, k.sessionID)
	if err != nil {
		t.Fatalf("registryB.GetOrSpawn: %v", err)
	}
	return registryB, actorB
}

func (k *killPodReceiptRig) turnDeadline(ctx context.Context, t *testing.T) time.Time {
	t.Helper()
	var firesAt time.Time
	if err := k.poolB.QueryRow(ctx, `SELECT fires_at FROM session_timers WHERE session_id = $1 AND name = $2`,
		k.sessionID, TimerTurnDeadline).Scan(&firesAt); err != nil {
		t.Fatalf("read turn_deadline: %v", err)
	}
	return firesAt
}

func (k *killPodReceiptRig) executionCompleteRows(ctx context.Context, t *testing.T) (all, synthetic int) {
	t.Helper()
	if err := k.poolB.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE payload ? 'synthetic') FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		k.sessionID).Scan(&all, &synthetic); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	return all, synthetic
}

// reconnectToB is the sandbox's same-gen reconnect to B: its ready, then
// three heartbeats, each answered before the next is sent -- so every
// post-commit evaluation they cause has run once the last one returns.
func reconnectToB(ctx context.Context, t *testing.T, actorB *Actor, capable bool) {
	t.Helper()
	sendSandboxEvent(ctx, t, actorB, receiptReady(1, capable))
	for i := 0; i < 3; i++ {
		sendSandboxEvent(ctx, t, actorB, receiptHeartbeat(1))
	}
	sendSandboxEvent(ctx, t, actorB, receiptHeartbeat(1))
}

func TestResilience_KillPodAfterDispatchCommit_CapableAgent_PromptResentOnSameGenReconnect(t *testing.T) {
	ctx := context.Background()
	timeouts := platform.DefaultTimeouts()
	k := killAfterDispatchCommit(ctx, t, timeouts, true)

	dispatched, err := k.turns.Get(ctx, k.turnID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if dispatched.DispatchedMessageID == nil || dispatched.ReceiptRequestedMessageID == nil ||
		*dispatched.ReceiptRequestedMessageID != *dispatched.DispatchedMessageID {
		t.Fatalf("A's dispatch recorded request %v for messageId %v, want its own", dispatched.ReceiptRequestedMessageID, dispatched.DispatchedMessageID)
	}
	deadline := k.turnDeadline(ctx, t)

	commanderB := &receiptCommander{}
	_, actorB := k.replicaB(ctx, t, timeouts, commanderB)
	reconnectToB(ctx, t, actorB, true)

	prompts := commanderB.prompts(t)
	if len(prompts) != 1 {
		t.Fatalf("B sent %d prompts after the same-gen reconnect and three heartbeats, want exactly 1", len(prompts))
	}
	if prompts[0].MessageId != *dispatched.DispatchedMessageID || !prompts[0].asksReceipt(t) {
		t.Fatalf("B's prompt = %+v, want A's messageId %q, asking for a receipt", prompts[0].Prompt, *dispatched.DispatchedMessageID)
	}
	if got := k.turnDeadline(ctx, t); !got.Equal(deadline) {
		t.Fatalf("turn_deadline fires_at = %v, want %v: a re-send re-arms nothing", got, deadline)
	}

	// The agent, now holding the prompt, receipts it and runs it to the end.
	sendSandboxEvent(ctx, t, actorB, promptReceivedEvent(prompts[0].MessageId, 1, false))
	sendSandboxEvent(ctx, t, actorB, completedEvent(1))
	waitUntil(t, 5*time.Second, func() bool {
		got, err := k.turns.Get(ctx, k.turnID)
		return err == nil && got.Status == sqlcgen.TurnStatusCompleted
	})
	if all, synthetic := k.executionCompleteRows(ctx, t); all != 1 || synthetic != 0 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want the agent's one, no synthetic", all, synthetic)
	}
}

func TestResilience_KillPodAfterDispatchCommit_IncapableAgent_NotResent_EndsAtTurnDeadline(t *testing.T) {
	ctx := context.Background()
	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 2 * time.Second // injected; the lost prompt's turn ends here
	k := killAfterDispatchCommit(ctx, t, timeouts, false)

	commanderB := &receiptCommander{}
	registryB, actorB := k.replicaB(ctx, t, timeouts, commanderB)
	reconnectToB(ctx, t, actorB, false)
	if got := len(commanderB.prompts(t)); got != 0 {
		t.Fatalf("B sent %d prompts to an agent without the capability, want 0", got)
	}

	waitUntil(t, 15*time.Second, func() bool {
		if err := registryB.PumpOnce(ctx); err != nil {
			t.Logf("PumpOnce: %v (retrying)", err)
			return false
		}
		got, err := k.turns.Get(ctx, k.turnID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})
	if all, synthetic := k.executionCompleteRows(ctx, t); all != 1 || synthetic != 1 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want one synthetic: the turn timed out", all, synthetic)
	}
	session, err := narvipg.NewSessionStore(k.poolB).Get(ctx, k.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.FailureReason == nil || *session.FailureReason != sqlcgen.SessionFailureReasonTimeout {
		t.Fatalf("session failure_reason = %v, want timeout", session.FailureReason)
	}
	if got := len(commanderB.prompts(t)); got != 0 {
		t.Fatalf("B sent %d prompts in all, want 0", got)
	}
}

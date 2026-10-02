//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/platform"
)

// TestEndedTurn_LeftRunningOnlyWhenItsPromptMayHaveArrived drives a turn A
// to its end through the real actor, then stamps a later turn B on the same
// sandbox gen, and asks the review-verdict endpoint's question about B
// (TurnStore.EarlierTurnLeftRunning): may an earlier turn's agent still be
// running beside it? A turn whose prompt certainly never reached the
// sandbox -- a dispatch refused before the prompt was sent, or a send
// refused with no live connection, which writes nothing -- had no agent,
// so it never makes B's trace unreadable. A turn that timed out, or whose
// send failed after it may have written part of the prompt, still does.
// One pool, and one session per case.
func TestEndedTurn_LeftRunningOnlyWhenItsPromptMayHaveArrived(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	tests := []struct {
		name          string
		endA          func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (sessionID pgtype.UUID, turnA sqlcgen.Turn)
		wantRunning   bool
		wantDelivered bool // whether A's synthetic execution_complete may claim delivery
	}{
		{
			name: "a send refused with no live connection",
			endA: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
				return failDispatchThroughCommander(ctx, t, pool, ports.ErrNoLiveSandboxConnection)
			},
			wantRunning: false,
		},
		{
			name: "a send that failed after it may have written part of the prompt",
			endA: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
				return failDispatchThroughCommander(ctx, t, pool, errors.New("write: broken pipe"))
			},
			wantRunning:   true,
			wantDelivered: true,
		},
		{
			name:        "a dispatch refused by the cohort rollout before the prompt was sent",
			endA:        refuseDispatchByRollout,
			wantRunning: false,
		},
		{
			name:        "a dispatch refused for a prompt frame larger than a sandbox accepts",
			endA:        refuseOversizedPromptFrame,
			wantRunning: false,
		},
		{
			name:          "a turn that timed out, its agent not stopped",
			endA:          timeOutProcessingTurn,
			wantRunning:   true,
			wantDelivered: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID, turnA := tc.endA(ctx, t, pool)
			turns := narvipg.NewTurnStore(pool)
			endedA, err := turns.Get(ctx, turnA.ID)
			if err != nil {
				t.Fatalf("get turn A: %v", err)
			}
			if endedA.Status != sqlcgen.TurnStatusFailed || endedA.DispatchedSandboxGen == nil || endedA.DispatchedEventID == nil {
				t.Fatalf("turn A = status %s, gen %v, watermark %v; want failed and stamped at a gen", endedA.Status, endedA.DispatchedSandboxGen, endedA.DispatchedEventID)
			}

			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT payload FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'turn_id' = $2`, sessionID, turnA.ID.String()).Scan(&raw); err != nil {
				t.Fatalf("read turn A's synthetic execution_complete: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode turn A's synthetic execution_complete: %v", err)
			}
			delivered, marked := payload["delivered"]
			if undeliveredMark := marked && delivered == false; undeliveredMark == tc.wantDelivered {
				t.Errorf("turn A's synthetic execution_complete = %s; want the \"delivered\": false mark only when its prompt certainly never arrived", raw)
			}

			// Turn B, dispatched after A to the same sandbox gen, with its own
			// watermark above A's synthetic event.
			gen := *endedA.DispatchedSandboxGen
			watermark, err := narvipg.NewEventStore(pool).MaxEventIDForSession(ctx, sessionID)
			if err != nil {
				t.Fatalf("read turn B's watermark: %v", err)
			}
			turnB, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing})
			if err != nil {
				t.Fatalf("create turn B: %v", err)
			}
			if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnB.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen, DispatchedEventID: &watermark}); err != nil {
				t.Fatalf("stamp turn B's dispatch: %v", err)
			}

			running, err := turns.EarlierTurnLeftRunning(ctx, sessionID, turnB.ID, gen, watermark)
			if err != nil {
				t.Fatalf("EarlierTurnLeftRunning: %v", err)
			}
			if running != tc.wantRunning {
				t.Errorf("EarlierTurnLeftRunning(B) = %v, want %v", running, tc.wantRunning)
			}
		})
	}
}

// failDispatchThroughCommander dispatches a pending turn to a ready
// sandbox through the real actor, with a commander whose send fails with
// sendErr, and waits for failDispatchedTurn to end it.
func failDispatchThroughCommander(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sendErr error) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	created := createPendingTurn(ctx, t, turns, sessionID, "review this")
	readySandbox(ctx, t, pool, sessionID)

	commander := &fakeSendCommander{nextErr: sendErr}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitForTurnStatus(ctx, t, turns, created.ID, sqlcgen.TurnStatusFailed)
	if commander.callCount() != 1 {
		t.Fatalf("commander sends = %d, want 1 (the failed send)", commander.callCount())
	}
	return sessionID, created
}

// refuseDispatchByRollout dispatches a pending turn whose repository was
// de-enrolled from the cohort rollout while its sandbox is ready: the
// dispatch is refused before SendCommand is ever called.
func refuseDispatchByRollout(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	const repoFullName = "acme/undelivered-prompt-refusal"
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+repoFullName+".git", "")
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertSessionsEnabled(ctx, repoFullName, false); err != nil {
		t.Fatalf("de-enroll the repository: %v", err)
	}
	readySandbox(ctx, t, pool, sessionID)
	turns := narvipg.NewTurnStore(pool)
	created := createPendingTurn(ctx, t, turns, sessionID, "review this")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistryWithCommanderAndRolloutMode(t, ctx, pool, commander, rollout.ModeCohort)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitForTurnStatus(ctx, t, turns, created.ID, sqlcgen.TurnStatusFailed)
	if commander.callCount() != 0 {
		t.Fatalf("commander sends = %d, want 0 (a refused dispatch sends nothing)", commander.callCount())
	}
	return sessionID, created
}

// refuseOversizedPromptFrame dispatches a pending turn whose prompt frame,
// once encoded, is larger than platform.MaxPromptFrameBytes, to a ready
// sandbox: the dispatch is refused before SendCommand is ever called.
func refuseOversizedPromptFrame(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	sessionID := createTestSession(ctx, t, pool)
	readySandbox(ctx, t, pool, sessionID)
	turns := narvipg.NewTurnStore(pool)
	// Each '<' encodes as the six bytes <, so the text fits and
	// only its frame does not.
	created := createPendingTurn(ctx, t, turns, sessionID, strings.Repeat("<", platform.MaxPromptFrameBytes/6+1024))

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitForTurnStatus(ctx, t, turns, created.ID, sqlcgen.TurnStatusFailed)
	if commander.callCount() != 0 {
		t.Fatalf("commander sends = %d, want 0 (a frame over the limit is never written)", commander.callCount())
	}
	return sessionID, created
}

// timeOutProcessingTurn stamps a processing turn at gen 1 long past a tiny
// injected TurnDeadline and fires the deadline through the
// real actor (handleTurnDeadlineTimer), which fails the turn without
// stopping its agent.
func timeOutProcessingTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	watermark, err := narvipg.NewEventStore(pool).MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	gen := int32(1)
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		DispatchedSandboxGen: &gen, DispatchedEventID: &watermark,
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForTurnStatus(ctx, t, turns, created.ID, sqlcgen.TurnStatusFailed)
	return sessionID, created
}

// readySandbox creates sessionID's sandbox and moves it to ready.
func readySandbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
	t.Helper()
	sandboxes := narvipg.NewSandboxStore(pool)
	if _, err := sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
}

// waitForTurnStatus waits until turnID reaches want.
func waitForTurnStatus(ctx context.Context, t *testing.T, turns *narvipg.TurnStore, turnID pgtype.UUID, want sqlcgen.TurnStatus) {
	t.Helper()
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, turnID)
		return err == nil && got.Status == want
	})
}

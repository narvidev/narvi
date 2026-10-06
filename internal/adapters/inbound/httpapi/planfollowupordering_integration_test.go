//go:build integration

// Integration test for F5 (adversarial review): createTurnLocked's own
// plan_followup classification block (turn.go) declares, in its own doc
// comment, that the classifier's LLM call runs BEFORE tx.Begin and
// UNLOCKED -- "a real outbound LLM call must never hold a Postgres
// transaction/row lock open". Nothing in the pre-existing test suite
// actually pinned that ordering: mutating the block to run INSIDE the
// transaction (after the session row's own GetActorEpochForUpdate lock)
// left the entire httpapi integration suite green. This file closes that
// gap directly, following TestCreateSessionCore_ValidationFailure_
// NeverAcquiresConnection's own established style (createcore_integration_
// test.go) of proving a connection/locking property via a SECOND,
// independent connection against the same database, rather than
// inspecting createTurnLocked's own internals.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
)

// lockProbingPlanFollowupLLM is a ports.LLM fake whose Complete method,
// instead of merely returning a canned response like fakePlanFollowupLLM
// (planfollowupgate_integration_test.go), first tries to take
// `SELECT id FROM sessions WHERE id = $1 FOR UPDATE NOWAIT` on sessionID
// -- THE SAME ROW createTurnLocked's own GetActorEpochForUpdate call locks
// later in that function -- via a SEPARATE connection/transaction of its
// own (pool.Begin, not the caller's ctx-scoped tx, since createTurnLocked
// itself has no tx open yet at the point this runs if the ordering is
// correct). NOWAIT means this either succeeds immediately (the row is not
// currently locked by anyone) or fails immediately with Postgres error
// 55P03 lock_not_available (someone else already holds it) -- never
// blocks, so this test can never hang regardless of which way the
// ordering bug goes.
type lockProbingPlanFollowupLLM struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	response  json.RawMessage

	calls       int
	probeErr    error
	lockWasFree bool
}

func (f *lockProbingPlanFollowupLLM) Complete(ctx context.Context, _ ports.CompletionRequest) (ports.CompletionResponse, error) {
	f.calls++

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.probeErr = err
		return ports.CompletionResponse{Raw: f.response}, nil
	}
	// Release the probe's own lock (if it acquired one) immediately,
	// before returning -- createTurnLocked's own SUBSEQUENT, real
	// GetActorEpochForUpdate lock (taken after this Complete call returns,
	// when the ordering is correct) must never contend with a lock this
	// probe itself is still holding.
	defer func() { _ = tx.Rollback(ctx) }()

	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE NOWAIT`, f.sessionID).Scan(&id); err != nil {
		f.probeErr = err
	} else {
		f.lockWasFree = true
	}

	return ports.CompletionResponse{Raw: f.response}, nil
}

// TestCreateTurnCore_PlanFollowup_ClassifiesBeforeSessionRowLock is F5's
// own regression test: proves the classifier's own outbound LLM call
// (ClassifyPlanFollowup, via intentSvc.Complete) genuinely runs before
// createTurnLocked ever takes its own session-row lock, by proving a
// COMPLETELY SEPARATE connection can still acquire that exact row's own
// FOR UPDATE lock, NOWAIT, at the moment Complete() is called.
func TestCreateTurnCore_PlanFollowup_ClassifiesBeforeSessionRowLock(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	llm := &lockProbingPlanFollowupLLM{
		pool:      rig.pool,
		sessionID: session.ID,
		response:  planFollowupResponse(intentdomain.TargetAnswer, intentdomain.ConfidenceHigh),
	}
	templates := narvipg.NewPromptTemplateStore(rig.pool)
	classifier := intentclassifier.New(llm, "anthropic", "claude-haiku-4-5", templates, nil, nil)

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, classifier, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "yes, exactly that one", nil, false, false, pgtype.UUID{}, RejectIfOpen)

	assertAwaitingApprovalDecline(t, wasCreated, cerr)

	if llm.calls != 1 {
		t.Fatalf("llm.calls = %d, want 1 (the classifier must actually have been invoked)", llm.calls)
	}
	if llm.probeErr != nil {
		var pgErr *pgconn.PgError
		lockHeld := errors.As(llm.probeErr, &pgErr) && pgErr.Code == "55P03"
		t.Fatalf("probe error = %v (lock_not_available=%v), want nil -- a SEPARATE connection must still be able to take sessions.id's own FOR UPDATE NOWAIT lock while the classifier's Complete() call is running, proving createTurnLocked has not yet acquired its own session-row lock at that point (this block's own doc comment, turn.go: 'Runs BEFORE tx.Begin ... UNLOCKED -- a real outbound LLM call must never hold a Postgres transaction/row lock open')", llm.probeErr, lockHeld)
	}
	if !llm.lockWasFree {
		t.Error("lockWasFree = false, want true")
	}
}

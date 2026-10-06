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

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// This file is the session actor's half of technical plan §40.1's spend
// cap: the second check, made as a queued turn is about to be dispatched,
// which ends every queued turn of a session past its cap without failing
// the session, and never touches the turn in flight.

// capReviewSession creates a code-host review session whose pull-request
// claim names repo, and sets repo's cap to limit.
func capReviewSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, limit string) pgtype.UUID {
	t.Helper()
	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceGithub)
	if _, err := pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ($1, 9, $2)`, repo, sessionID); err != nil {
		t.Fatalf("claim the pull request: %v", err)
	}
	setRepoCap(ctx, t, pool, repo, limit)
	return sessionID
}

// setRepoCap sets repo's session spend cap.
func setRepoCap(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, limit string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repo, limit); err != nil {
		t.Fatalf("set the cap: %v", err)
	}
}

// guardRefusalFor is the refusal the guard makes for sessionID against
// repo's cap limit, having spent spent, with turns turns: the crossing.
func guardRefusalFor(t *testing.T, sessionID pgtype.UUID, repo, limit, spent string, turns int64) sessionguard.Refusal {
	t.Helper()
	c, err := sessionguard.ParseMicroUSD(limit)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sessionguard.ParseMicroUSD(spent)
	if err != nil {
		t.Fatal(err)
	}
	return sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: sessionID.Bytes, Cap: c, Spent: s,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}, Turns: turns}
}

// admitTurn creates a pending turn the way a writer outside the actor
// does: under the session's row lock, admitted by the session guard, its
// dispatch timer armed. It fails the test when the guard refuses.
func admitTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, prompt string) sqlcgen.Turn {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := narvipg.NewSessionStore(pool).WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	admission, refusal, err := turnguard.New(pool, nil, false).Admit(ctx, tx, sessionID, sessionguard.OriginAutomatic, turnguard.StageCreate)
	if err != nil || refusal != nil {
		t.Fatalf("admit %q: refusal %v, error %v", prompt, refusal, err)
	}
	created, err := narvipg.NewTurnStore(pool).WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt}, admission)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return created
}

// refuseTurn asks the guard for a turn the way a writer outside the actor
// does, expects a refusal, rolls back and records it
// (Guard.RecordRefusal), and returns it.
func refuseTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) *sessionguard.Refusal {
	t.Helper()
	guard := turnguard.New(pool, nil, false)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := narvipg.NewSessionStore(pool).WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_, refusal, err := guard.Admit(ctx, tx, sessionID, sessionguard.OriginPerson, turnguard.StageCreate)
	_ = tx.Rollback(ctx)
	if err != nil || refusal == nil {
		t.Fatalf("want a refusal, got %v (error %v)", refusal, err)
	}
	guard.RecordRefusal(ctx, sessionID, refusal)
	return refusal
}

// sendStepCost sends a step_finish costing usd at gen.
func sendStepCost(ctx context.Context, t *testing.T, a *Actor, sessionID pgtype.UUID, gen int, usd float64) {
	t.Helper()
	raw, messageID := stepFinishRaw(t, sessionID.String(), gen, costUSDf64(usd))
	if outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_finish", Gen: gen, MessageID: messageID, Raw: raw}); !outcome.Persisted {
		t.Fatal("step_finish not persisted")
	}
}

// commandTypes is the type of every command sent, in order.
func commandTypes(f *fakeSendCommander) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.payloads))
	for _, p := range f.payloads {
		var env struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p, &env)
		out = append(out, env.Type)
	}
	return out
}

func scalarInt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// guardCounts is the crossing's warnings (at its message id, and all) and
// the session-guard notices on the code host.
func guardCounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, r sessionguard.Refusal) (atID, warnings, notices int) {
	t.Helper()
	atID = scalarInt(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, sessionID, turnguard.WarningMessageID(r))
	warnings = scalarInt(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning'`, sessionID)
	notices = scalarInt(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubSessionGuard))
	return atID, warnings, notices
}

// overshootFixture runs technical plan §40.1's overshoot case on a review
// session capped at $1.00: turn 1 dispatched; $0.90 recorded on it; two
// turns queued behind it while the session is under its cap (as two
// mentions' would be); another $0.50 recorded, taking the session to
// $1.40; turn 1 completes. It returns the session, the three turns, the
// commander and the actor.
func overshootFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo string) (pgtype.UUID, [3]sqlcgen.Turn, *fakeSendCommander, *Actor) {
	t.Helper()
	sessionID := capReviewSession(ctx, t, pool, repo, "1.00")
	seedReadySandbox(ctx, t, pool, sessionID)
	var turns [3]sqlcgen.Turn
	turns[0] = admitTurn(ctx, t, pool, sessionID, "the first")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })

	sendStepCost(ctx, t, a, sessionID, 1, 0.90)
	turns[1] = admitTurn(ctx, t, pool, sessionID, "a mention queued behind it")
	turns[2] = admitTurn(ctx, t, pool, sessionID, "another mention queued behind it")
	sendStepCost(ctx, t, a, sessionID, 1, 0.50)
	sendExecutionComplete(ctx, t, a, sessionID)

	store := narvipg.NewTurnStore(pool)
	waitUntil(t, 10*time.Second, func() bool {
		for _, q := range turns[1:] {
			got, err := store.Get(ctx, q.ID)
			if err != nil || turn.State(got.Status) != turn.StateFailed {
				return false
			}
		}
		return true
	})
	return sessionID, turns, commander, a
}

// TestSpendCap_OvershootIsBoundedByTheTurnInFlight: the spend can pass the
// cap by the turn in flight's own cost and no more. Two turns queued behind
// it while the session was under its cap are each checked again as they
// are about to be dispatched, after the turn that crossed the cap ended,
// and end with the spend_cap end reason, never sent to the sandbox: the
// session's spend stays at what the one turn took it to, with one warning
// at the crossing's id, naming the overshoot and the lower bound, and one
// notice on the pull request.
func TestSpendCap_OvershootIsBoundedByTheTurnInFlight(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/overshoot"
	sessionID, turns, commander, _ := overshootFixture(ctx, t, pool, repo)

	store := narvipg.NewTurnStore(pool)
	first, err := store.Get(ctx, turns[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.State(first.Status) != turn.StateCompleted || first.EndReason != nil {
		t.Fatalf("the turn in flight: %s (end reason %v), want completed: it is never ended by the guard", first.Status, first.EndReason)
	}
	for _, q := range turns[1:] {
		got, err := store.Get(ctx, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.EndReason == nil || *got.EndReason != turn.EndReasonSpendCap || got.DispatchedAt.Valid {
			t.Fatalf("queued turn %s: end reason %v, dispatched %v; want ended spend_cap, never dispatched", q.ID.String(), got.EndReason, got.DispatchedAt.Valid)
		}
	}
	if n := promptCount(commander); n != 1 {
		t.Fatalf("prompts sent = %d, want the one in flight's only", n)
	}
	var spent string
	if err := pool.QueryRow(ctx, `SELECT SUM(cost_usd)::text FROM turns WHERE session_id = $1`, sessionID).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if spent != "1.400000" {
		t.Fatalf("spend = %s, want 1.400000: the turn in flight's own cost past the cap, and nothing more", spent)
	}
	want := guardRefusalFor(t, sessionID, repo, "1.00", "1.40", 3)
	atID, warnings, notices := guardCounts(ctx, t, pool, sessionID, want)
	if atID != 1 || notices != 1 {
		t.Fatalf("warnings at the crossing's id = %d (of %d), notices = %d; want one of each", atID, warnings, notices)
	}
	var message string
	if err := pool.QueryRow(ctx, `SELECT payload->>'message' FROM events WHERE session_id = $1 AND message_id = $2`, sessionID, turnguard.WarningMessageID(want)).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if message != sessionguard.Text(want) {
		t.Fatalf("warning = %q, want %q", message, sessionguard.Text(want))
	}
}

// TestSpendCap_EndedQueuedTurnsLeaveTheSessionUnfailed: the queued turns
// ended at the cap are no outcome of the session's: it reads completed, its
// last run is the turn that ran, nothing is queued or running, and its
// activity is finished -- waiting on a person, never failed.
func TestSpendCap_EndedQueuedTurnsLeaveTheSessionUnfailed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID, turns, _, _ := overshootFixture(ctx, t, pool, "acme/unfailed")

	sessions := narvipg.NewSessionStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		row, err := sessions.Get(ctx, sessionID)
		return err == nil && row.Status == sqlcgen.SessionStatusCompleted
	})
	row, err := sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != sqlcgen.SessionStatusCompleted || row.FailureReason != nil {
		t.Fatalf("session %s (failure reason %v), want completed: a turn ended at the cap fails nothing", row.Status, row.FailureReason)
	}
	facts, err := sessions.ActivityFacts(ctx, sessionID, ReviewAutoRetriggerBudget)
	if err != nil {
		t.Fatal(err)
	}
	if facts.LastRunTurnID != turns[0].ID || facts.LastRunStatus != "completed" {
		t.Fatalf("last run = %s %q, want the completed turn that ran", facts.LastRunTurnID.String(), facts.LastRunStatus)
	}
	var counts map[string]int
	if err := json.Unmarshal(facts.TurnCounts, &counts); err != nil {
		t.Fatal(err)
	}
	in := session.ActivityInput{TurnCounts: map[turn.State]int{}}
	for state, n := range counts {
		in.TurnCounts[turn.State(state)] = n
	}
	if got := session.DeriveActivity(in); got != session.ActivityFinished {
		t.Fatalf("activity = %s (turns %v), want finished", got, counts)
	}
}

// TestSpendCap_InFlightTurnIsNeverCancelled: a step cost that takes the
// session past its cap mid-turn stops nothing -- no stop is sent, the turn
// keeps running, and it completes as the agent ends it -- while a new turn
// asked for meanwhile is refused.
func TestSpendCap_InFlightTurnIsNeverCancelled(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := capReviewSession(ctx, t, pool, "acme/in-flight", "1.00")
	seedReadySandbox(ctx, t, pool, sessionID)
	inFlight := admitTurn(ctx, t, pool, sessionID, "the long one")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })

	sendStepCost(ctx, t, a, sessionID, 1, 2.50)
	_ = refuseTurn(ctx, t, pool, sessionID)
	sendEnsureDispatched(ctx, t, a)

	store := narvipg.NewTurnStore(pool)
	got, err := store.Get(ctx, inFlight.ID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.State(got.Status) != turn.StateProcessing || got.StopRequestedAt.Valid {
		t.Fatalf("the turn in flight past the cap: %s (stop flagged %v), want still processing", got.Status, got.StopRequestedAt.Valid)
	}
	sendExecutionComplete(ctx, t, a, sessionID)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := store.Get(ctx, inFlight.ID)
		return err == nil && turn.State(got.Status) == turn.StateCompleted
	})
	for i, typ := range commandTypes(commander) {
		if typ != "prompt" {
			t.Fatalf("command %d sent was %q: the guard sends nothing to the sandbox, a stop least of all", i, typ)
		}
	}
}

// TestSpendCap_ReenqueueOfTheInFlightTurnIsNotGated: the turn in flight
// re-sent to a new sandbox gen is the same turn, never asked about: a
// session past its cap still has its prompt re-sent, and the turn stays
// processing.
func TestSpendCap_ReenqueueOfTheInFlightTurnIsNotGated(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := capReviewSession(ctx, t, pool, "acme/reenqueue", "1.00")
	turnStore := narvipg.NewTurnStore(pool)
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing, Prompt: stringPtrForTest("finish the job")})
	if err != nil {
		t.Fatal(err)
	}
	staleGen := int32(1)
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_sandbox_gen = $2, dispatched_at = now(), cost_usd = 3 WHERE id = $1`, created.ID, staleGen); err != nil {
		t.Fatal(err)
	}
	sandboxStore := narvipg.NewSandboxStore(pool)
	tokenHash := "irrelevant-token-hash"
	for i := 0; i < 2; i++ {
		if _, err := sandboxStore.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &tokenHash}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatal(err)
	}

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })

	got, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.State(got.Status) != turn.StateProcessing || got.EndReason != nil || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 2 {
		t.Fatalf("the re-sent turn: %s, end reason %v, gen %v; want processing on gen 2", got.Status, got.EndReason, got.DispatchedSandboxGen)
	}
}

// TestSpendCap_NothingIsSpawnedForARefusedQueue: the dispatch-time check
// comes before a spawn is planned: a session past its cap with a queued
// turn and no sandbox spawns none, and the turn ends at the cap.
func TestSpendCap_NothingIsSpawnedForARefusedQueue(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := capReviewSession(ctx, t, pool, "acme/no-spawn", "1.00")
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1.5)`, sessionID); err != nil {
		t.Fatal(err)
	}
	queued := createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "queued before the cap was read")

	provider := &fakeSpawnProvider{}
	r := newDispatchTestRegistry(t, ctx, pool, provider, &fakeSendCommander{})
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	store := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := store.Get(ctx, queued.ID)
		return err == nil && got.EndReason != nil
	})
	if n := provider.callCount(); n != 0 {
		t.Fatalf("CreateSandbox calls = %d, want 0: nothing is spawned for a queue the cap refuses", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM sandboxes WHERE session_id = $1`, sessionID); n != 0 {
		t.Fatalf("a sandbox row exists for a refused queue")
	}
}

// TestSpendCap_OneWarningAndOneNoticePerCrossing: two refused creations and
// a dispatch-time end of the same crossing record one warning and send one
// notice between them, whoever observed the crossing first.
func TestSpendCap_OneWarningAndOneNoticePerCrossing(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/one-notice"
	sessionID := capReviewSession(ctx, t, pool, repo, "2.00")
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 2)`, sessionID); err != nil {
		t.Fatal(err)
	}
	// Queued before the crossing, as a turn admitted under the cap is.
	queued := createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "queued")
	_ = refuseTurn(ctx, t, pool, sessionID)
	_ = refuseTurn(ctx, t, pool, sessionID)

	seedReadySandbox(ctx, t, pool, sessionID)
	r := newDispatchTestRegistry(t, ctx, pool, nil, &fakeSendCommander{})
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	store := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := store.Get(ctx, queued.ID)
		return err == nil && got.EndReason != nil
	})

	atID, warnings, notices := guardCounts(ctx, t, pool, sessionID, guardRefusalFor(t, sessionID, repo, "2.00", "2", 2))
	if atID != 1 || warnings != 1 || notices != 1 {
		t.Fatalf("warnings %d (%d at the crossing's id), notices %d; want one warning and one notice for one crossing", warnings, atID, notices)
	}
}

// TestSpendCap_ANewCrossingAfterARaiseNotifiesAgain: a raised cap is a new
// crossing -- reached again, it records a warning and sends a notice of its
// own -- even with no turn taken in between: the turn in flight when the
// cap was raised went on spending past the raised cap too.
func TestSpendCap_ANewCrossingAfterARaiseNotifiesAgain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/new-crossing"
	sessionID := capReviewSession(ctx, t, pool, repo, "1.00")
	var inFlight pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO turns (session_id, status, dispatched_at, cost_usd) VALUES ($1, 'processing', now(), 1) RETURNING id`, sessionID).Scan(&inFlight); err != nil {
		t.Fatal(err)
	}
	_ = refuseTurn(ctx, t, pool, sessionID)

	setRepoCap(ctx, t, pool, repo, "3.00")
	if _, err := pool.Exec(ctx, `UPDATE turns SET cost_usd = 3.5 WHERE id = $1`, inFlight); err != nil {
		t.Fatal(err)
	}
	_ = refuseTurn(ctx, t, pool, sessionID)

	first, warnings, notices := guardCounts(ctx, t, pool, sessionID, guardRefusalFor(t, sessionID, repo, "1.00", "1", 1))
	second, _, _ := guardCounts(ctx, t, pool, sessionID, guardRefusalFor(t, sessionID, repo, "3.00", "3.5", 1))
	if first != 1 || second != 1 || warnings != 2 || notices != 2 {
		t.Fatalf("crossings' warnings %d and %d (of %d), notices %d; want one warning and one notice for each crossing", first, second, warnings, notices)
	}
}

// TestSpendCap_ACrossingAtACapValueSeenBeforeIsANewCrossing: a crossing is
// the session refused after it was last admitted a turn, so a session that
// crossed a cap, was admitted another turn once the cap was cleared, and
// meets the same cap value again is told again -- a new warning naming what
// it spent now, and a new notice -- whichever check meets it: a refused
// creation, or a queued turn ended at dispatch.
func TestSpendCap_ACrossingAtACapValueSeenBeforeIsANewCrossing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		dispatch bool
	}{
		{name: "refused at creation"},
		{name: "ended at dispatch", dispatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			const repo = "acme/same-cap-again"
			sessionID := capReviewSession(ctx, t, pool, repo, "1.00")
			var first pgtype.UUID
			if err := pool.QueryRow(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1) RETURNING id`, sessionID).Scan(&first); err != nil {
				t.Fatal(err)
			}
			_ = refuseTurn(ctx, t, pool, sessionID)

			// The cap cleared, a turn admitted, the spend at $5.00, and the
			// same cap set again.
			if _, err := pool.Exec(ctx, `UPDATE repo_settings SET session_spend_cap_usd = NULL WHERE repo_full_name = $1`, repo); err != nil {
				t.Fatal(err)
			}
			admitted := admitTurn(ctx, t, pool, sessionID, "taken while uncapped")
			if tc.dispatch {
				if _, err := pool.Exec(ctx, `UPDATE turns SET cost_usd = 5 WHERE id = $1`, first); err != nil {
					t.Fatal(err)
				}
			} else if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 4 WHERE id = $1`, admitted.ID); err != nil {
				t.Fatal(err)
			}
			setRepoCap(ctx, t, pool, repo, "1.00")

			commander := &fakeSendCommander{}
			if tc.dispatch {
				seedReadySandbox(ctx, t, pool, sessionID)
				r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
				t.Cleanup(func() { _ = r.Shutdown() })
				a, err := r.GetOrSpawn(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				sendEnsureDispatched(ctx, t, a)
				store := narvipg.NewTurnStore(pool)
				waitUntil(t, 5*time.Second, func() bool {
					got, err := store.Get(ctx, admitted.ID)
					return err == nil && got.EndReason != nil && *got.EndReason == turn.EndReasonSpendCap
				})
			} else {
				_ = refuseTurn(ctx, t, pool, sessionID)
			}

			before := guardRefusalFor(t, sessionID, repo, "1.00", "1", 1)
			again := guardRefusalFor(t, sessionID, repo, "1.00", "5", 2)
			firstAt, warnings, notices := guardCounts(ctx, t, pool, sessionID, before)
			againAt, _, _ := guardCounts(ctx, t, pool, sessionID, again)
			if firstAt != 1 || againAt != 1 || warnings != 2 || notices != 2 {
				t.Fatalf("warnings %d and %d at the two crossings' ids (of %d), notices %d; want one warning and one notice for each crossing", firstAt, againAt, warnings, notices)
			}
			var message string
			if err := pool.QueryRow(ctx, `SELECT payload->>'message' FROM events WHERE session_id = $1 AND message_id = $2`, sessionID, turnguard.WarningMessageID(again)).Scan(&message); err != nil {
				t.Fatal(err)
			}
			if message != sessionguard.Text(again) {
				t.Fatalf("the new crossing's warning = %q, want %q: it names what the session spent now", message, sessionguard.Text(again))
			}
			if n := promptCount(commander); n != 0 {
				t.Fatalf("prompts sent = %d, want none", n)
			}
		})
	}
}

// TestSpendCap_AWorkflowAttemptEndedAtDispatchEscalatesItsRun: a queued
// attempt a workflow run tracks, ended at dispatch because the session has
// spent its cap, finishes its step run and escalates its run to
// needs_review, with the guard's escalation notice as the crossing's one
// notice -- the run named, its review asked for, that it will not resume
// on its own, and the guard's text -- and no guard notice beside it. The
// attempt gets its undelivered execution_complete, nothing is sent to the
// sandbox, one warning is recorded, and the session reads completed.
func TestSpendCap_AWorkflowAttemptEndedAtDispatchEscalatesItsRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const repo = "acme/workflow-at-dispatch"
	sessionID := capReviewSession(ctx, t, pool, repo, "1.00")
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1.5)`, sessionID); err != nil {
		t.Fatal(err)
	}
	attempt := createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "the workflow's next attempt")
	run := workflowStepWithBlockedSelfEdge(ctx, t, pool, sessionID, attempt, "test-guard-ended-attempt")
	seedReadySandbox(ctx, t, pool, sessionID)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	store := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := store.Get(ctx, attempt.ID)
		return err == nil && got.EndReason != nil
	})

	if got, err := store.Get(ctx, attempt.ID); err != nil || *got.EndReason != turn.EndReasonSpendCap || got.DispatchedAt.Valid {
		t.Fatalf("the attempt: %+v (%v); want ended spend_cap, never dispatched", got, err)
	}
	workflows := narvipg.NewWorkflowStore(pool)
	runRow, err := workflows.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(runRow.Status) != "needs_review" {
		t.Fatalf("run status = %q, want needs_review", runRow.Status)
	}
	var stepStatus string
	var finished bool
	if err := pool.QueryRow(ctx, `SELECT status::text, finished_at IS NOT NULL FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID).Scan(&stepStatus, &finished); err != nil {
		t.Fatal(err)
	}
	if stepStatus == "running" || !finished {
		t.Fatalf("step run %s (finished %v), want finished: the attempt is no longer live", stepStatus, finished)
	}

	want := guardRefusalFor(t, sessionID, repo, "1.00", "1.5", 2)
	var notice string
	if err := pool.QueryRow(ctx, `SELECT payload->>'text' FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubWorkflowDecision)).Scan(&notice); err != nil {
		t.Fatalf("the run's escalation notice: %v", err)
	}
	for _, part := range []string{"This workflow run (" + run.ID.String() + ") now needs your review", "will not resume on its own", sessionguard.Text(want)} {
		if !strings.Contains(notice, part) {
			t.Errorf("escalation notice %q does not say %q", notice, part)
		}
	}
	atID, warnings, notices := guardCounts(ctx, t, pool, sessionID, want)
	if atID != 1 || warnings != 1 || notices != 0 {
		t.Fatalf("warnings %d (%d at the crossing's id), guard notices %d; want one warning and no guard notice beside the run's", warnings, atID, notices)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'delivered' = 'false'`, sessionID); n != 1 {
		t.Fatalf("undelivered execution_complete events = %d, want 1", n)
	}
	if n := promptCount(commander); n != 0 {
		t.Fatalf("prompts sent = %d, want none", n)
	}
	sessionRow, err := narvipg.NewSessionStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionRow.Status != sqlcgen.SessionStatusCompleted {
		t.Fatalf("session status = %q, want completed: a turn ended at the cap fails nothing", sessionRow.Status)
	}
}

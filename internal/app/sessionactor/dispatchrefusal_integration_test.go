//go:build integration

package sessionactor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/platform"
)

const dispatchRefusedRepo = "acme/r3-dispatch-refused"

// readyReviewSession is a pull request's review session on
// dispatchRefusedRepo, its sandbox ready at gen 1, holding one pending
// review attempt.
func readyReviewSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       reposJSONForTest(t, "r3-dispatch-refused", "https://github.com/"+dispatchRefusedRepo+".git", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimPullRequest(ctx, t, pool, dispatchRefusedRepo, 9, session.ID)
	seedReadySandbox(ctx, t, pool, session.ID)
	head := "cafef00d"
	attempt, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID, Status: sqlcgen.TurnStatusPending, Prompt: strPtr("review this pull request"),
		ReviewHeadSha: &head, IsReviewAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID, attempt
}

// readySlackSession is a Slack-origin session on dispatchRefusedRepo, its
// thread claimed and its sandbox ready at gen 1, holding one pending turn.
func readySlackSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceSlack,
		Repos:       reposJSONForTest(t, "r3-dispatch-refused", "https://github.com/"+dispatchRefusedRepo+".git", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C-DISPATCH", "3.0001", session.ID); err != nil || !ok {
		t.Fatalf("claim slack thread: ok=%v err=%v", ok, err)
	}
	seedReadySandbox(ctx, t, pool, session.ID)
	pending, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusPending, Prompt: strPtr("do the thing")})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID, pending
}

// TestDispatchFailure_NotifiesWhereTheTurnCameFrom: a turn executeDispatch
// ends after committing it processing -- the turn-dispatch-time rollout
// refusal on a live sandbox, or a prompt the commander could not deliver
// -- is told to the channel it came from, as every other ended turn is
// (failDispatchedTurn's enqueueOutboxNotification). A review attempt's
// check, already published running at dispatch, closes as not assessed
// naming the cause, rather than staying in progress for good; a Slack
// thread gets the failure notice. The rollout refusal also records one
// warning naming the repository and the remedy; a failed delivery records
// none.
func TestDispatchFailure_NotifiesWhereTheTurnCameFrom(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		mode        platform.RolloutMode
		sendErr     error
		setup       func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn)
		wantReason  reviewcheck.NotAssessedReason // "" for a Slack session
		wantWarning bool
	}{
		{"rollout refusal, a review attempt", rollout.ModeCohort, nil, readyReviewSession, reviewcheck.NotAssessedRolloutNotEnrolled, true},
		{"undelivered prompt, a review attempt", rollout.ModeOpen, errors.New("no live sandbox connection"), readyReviewSession, reviewcheck.NotAssessedPromptNotDelivered, false},
		{"rollout refusal, a Slack-origin session", rollout.ModeCohort, nil, readySlackSession, "", true},
		{"undelivered prompt, a Slack-origin session", rollout.ModeOpen, errors.New("no live sandbox connection"), readySlackSession, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			sessionID, attempt := tc.setup(ctx, t, pool)

			commander := &fakeSendCommander{nextErr: tc.sendErr}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{RolloutMode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			sendEnsureDispatched(ctx, t, a)
			turns := narvipg.NewTurnStore(pool)
			waitUntil(t, 5*time.Second, func() bool {
				got, err := turns.Get(ctx, attempt.ID)
				return err == nil && got.Status == sqlcgen.TurnStatusFailed
			})

			if tc.wantReason != "" {
				checks := outboxRows(ctx, t, pool, sessionID, ports.NotificationKindGitHubReviewCheck)
				var closing []map[string]any
				for _, c := range checks {
					if c["phase"] == string(reviewcheck.PhaseTerminalNotAssessed) {
						closing = append(closing, c)
					}
				}
				if len(closing) != 1 || closing[0]["attempt_id"] != attempt.ID.String() || closing[0]["not_assessed_reason"] != string(tc.wantReason) {
					t.Fatalf("review-check rows = %v, want one terminal_not_assessed for attempt %s naming %q", checks, attempt.ID.String(), tc.wantReason)
				}
			} else {
				notices := outboxRows(ctx, t, pool, sessionID, ports.NotificationKindSlack)
				if len(notices) != 1 || notices[0]["channel_id"] != "C-DISPATCH" {
					t.Fatalf("slack rows = %v, want the one failure notice in the session's thread", notices)
				}
				if text, _ := notices[0]["text"].(string); !strings.Contains(text, "failed") {
					t.Errorf("slack notice text = %q, want the failure notice", text)
				}
			}

			warnings := sessionWarnings(ctx, t, pool, sessionID)
			switch {
			case tc.wantWarning && (len(warnings) != 1 || !strings.Contains(warnings[0], dispatchRefusedRepo) || !strings.Contains(warnings[0], "enroll the repository")):
				t.Errorf("session warnings = %q, want one naming the repository and the remedy", warnings)
			case !tc.wantWarning && len(warnings) != 0:
				t.Errorf("session warnings = %q, want none for a failed delivery", warnings)
			}
		})
	}
}

// TestDispatchRefusal_WorkflowStepWithABlockedSelfEdgeEscalatesOnce: the
// turn-dispatch-time rollout refusal of a workflow-tracked turn reaches the
// engine as a refusal (OnTurnRefused), never as a blocked outcome. Read as
// blocked, the step's blocked self edge would queue the same step again,
// its turn would arm the dispatch timer, and the pump would bring it back
// to the same refusal every tick. With the pump ticking, the refusal is
// made once, no attempt is queued, and the run waits for a person.
func TestDispatchRefusal_WorkflowStepWithABlockedSelfEdgeEscalatesOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID, attempt := readyReviewSession(ctx, t, pool)

	var defID, stepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('review', 'test-dispatch-refused-blocked-self-edge', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $2, 'blocked')`, defID, stepID); err != nil {
		t.Fatalf("insert blocked self edge: %v", err)
	}
	workflows := narvipg.NewWorkflowStore(pool)
	run, err := workflows.CreateRun(ctx, sessionID, "review", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepRun, err := workflows.CreateStepRun(ctx, run.ID, stepID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	if err := workflows.AttachTurn(ctx, stepRun.ID, attempt.ID); err != nil {
		t.Fatalf("attach turn: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TimerClaimDuration = 300 * time.Millisecond
	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{RolloutMode: rollout.ModeCohort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sqlcgen.SessionSpawnSourceGithub))
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID); n != 1 {
		t.Errorf("turns = %d, want the refused attempt alone: the refusal followed the blocked self edge", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID); n != 1 {
		t.Errorf("step runs = %d, want the refused attempt alone", n)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sqlcgen.SessionSpawnSourceGithub)) - before; got != 1 {
		t.Errorf("session_rollout_refused_total grew by %d, want 1", got)
	}
	gotRun, err := workflows.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != sqlcgen.WorkflowRunStatusNeedsReview {
		t.Errorf("run status = %s, want needs_review", gotRun.Status)
	}
	if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
		t.Error("a dispatch timer is left: something was queued behind the refusal")
	}
}

// TestUndeliveredPrompt_AWorkflowRetryIsBackedOffNotLooped: a prompt the
// commander cannot deliver (no live connection) fails its turn, and a
// workflow step with a blocked self edge queues the same step again -- a
// turn whose dispatch timer is due at once. Were that timer left as it
// is, every pump tick would send the new turn to the same dead
// connection, fail it, notify the thread and queue the next. The failure
// backs the timer off instead (backOffAfterUndeliveredPrompt), doubling
// from DispatchRetryBackoff: here 1 s, so over 3 s of pump ticks every
// 100 ms the attempts come at about 0 s, 1 s and 2 s -- at least 2 and at
// most 4, one failed turn and one Slack notice each -- and the timer is
// left backed off. Once the connection is back, the next delivery sends
// the prompt.
func TestUndeliveredPrompt_AWorkflowRetryIsBackedOffNotLooped(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID, attempt := readySlackSession(ctx, t, pool)

	var defID, stepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'test-undelivered-blocked-self-edge', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $2, 'blocked')`, defID, stepID); err != nil {
		t.Fatalf("insert blocked self edge: %v", err)
	}
	workflows := narvipg.NewWorkflowStore(pool)
	run, err := workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepRun, err := workflows.CreateStepRun(ctx, run.ID, stepID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	if err := workflows.AttachTurn(ctx, stepRun.ID, attempt.ID); err != nil {
		t.Fatalf("attach turn: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TimerClaimDuration = 300 * time.Millisecond
	timeouts.DispatchRetryBackoff = time.Second
	timeouts.DispatchRetryBackoffMax = 10 * time.Second
	commander := &fakeSendCommander{nextErr: ports.ErrNoLiveSandboxConnection}
	r, err := NewRegistry(ctx, pool, timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	attempts := promptCount(commander)
	failed := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND status = 'failed'`, sessionID)
	notices := len(outboxRows(ctx, t, pool, sessionID, ports.NotificationKindSlack))
	t.Logf("delivery attempts over 3 s of pump ticks: %d (failed turns %d, Slack notices %d)", attempts, failed, notices)
	if attempts < 2 || attempts > 4 {
		t.Fatalf("delivery attempts = %d over 3 s, want 2 to 4: a failed delivery must back the retry off, not repeat it every pump tick", attempts)
	}
	if failed != attempts || notices != attempts {
		t.Errorf("failed turns = %d, Slack notices = %d, want one each per attempt (%d)", failed, notices, attempts)
	}
	var dueIn, chainAge float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM fires_at - now())::float8, EXTRACT(EPOCH FROM now() - created_at)::float8 FROM session_timers WHERE session_id = $1 AND name = $2`, sessionID, TimerDispatch).Scan(&dueIn, &chainAge); err != nil {
		t.Fatalf("read the dispatch timer: %v", err)
	}
	if dueIn <= 0 {
		t.Errorf("dispatch timer due in %.1fs, want it backed off", dueIn)
	}
	// The chain's first arm is carried from row to row, so the delay keeps
	// doubling: the timer the last re-queued turn armed is as old as the
	// first attempt, not as its own insert.
	if chainAge < 2.5 {
		t.Errorf("dispatch timer's created_at is %.1fs old, want the chain's first arm (about 3 s): each re-queued turn restarted the backoff", chainAge)
	}

	// The connection is back: the next delivery sends the prompt.
	commander.mu.Lock()
	commander.nextErr = nil
	commander.mu.Unlock()
	waitUntil(t, 15*time.Second, func() bool {
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND status = 'processing'`, sessionID) == 1
	})
	if got := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND status = 'failed'`, sessionID); got != failed {
		t.Errorf("failed turns = %d after the connection came back, want still %d", got, failed)
	}
}

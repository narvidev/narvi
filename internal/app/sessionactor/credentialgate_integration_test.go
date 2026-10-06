//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// This file proves, on real Postgres, the two actor-side readers of a
// session's provider-credential resolution under the one rule
// (providercredential.UserScopeTarget): the dispatch gate that refuses a
// turn whose model only a withheld personal link could run
// (credentialgate.go), and the counter-reviewer's choice of an opposing
// model (sessionconfig.go's reviewCredentialedProviders).

const credentialGateRepos = `[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`

// createLinkedMember creates a member whose own ChatGPT link (a
// scope=user/kind=oauth openai row) exists. The value is never decrypted
// on the paths these tests drive, so it is stored as opaque bytes.
func createLinkedMember(ctx context.Context, t *testing.T, pool *pgxpool.Pool, label string) pgtype.UUID {
	t.Helper()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("credential-gate-%s-%d@example.com", label, time.Now().UnixNano()),
		DisplayName:  "Linked member " + label,
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := narvipg.NewProviderCredentialStore(pool).UpsertOAuth(ctx, user.ID.String(), sqlcgen.ProviderCredentialProviderOpenai, []byte("opaque-oauth-blob"), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("link chatgpt account: %v", err)
	}
	return user.ID
}

func createGlobalCredential(ctx context.Context, t *testing.T, pool *pgxpool.Pool, provider sqlcgen.ProviderCredentialProvider) {
	t.Helper()
	if _, err := narvipg.NewProviderCredentialStore(pool).Create(ctx, sqlcgen.ProviderCredentialScopeGlobal, nil, provider, []byte("opaque-deployment-key")); err != nil {
		t.Fatalf("create global %s credential: %v", provider, err)
	}
}

func claimPullRequest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32, sessionID pgtype.UUID) {
	t.Helper()
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, sessionID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}
}

// TestDispatchGate_PersonalLinkOnly drives a real EnsureDispatched against
// a live sandbox for each kind of session, the creator holding a personal
// openai link. Only a session that never resolves the link, whose turn
// names a model only that link could run, is refused -- named, before
// anything is sent; every other one is dispatched as it was before.
func TestDispatchGate_PersonalLinkOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	const openaiModel = "openai/gpt-5.4"
	const anthropicModel = "anthropic/claude-opus-4-5"

	tests := []struct {
		name string
		// kind: "review", "web", "automation" or "child".
		kind            string
		model           *string
		deployment      []sqlcgen.ProviderCredentialProvider
		wantRefused     bool
		wantCheckReason bool
	}{
		{name: "review session, model only the requester's link provides", kind: "review", model: strPtr(openaiModel), deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderAnthropic}, wantRefused: true, wantCheckReason: true},
		{name: "review session, no deployment credential at all", kind: "review", model: strPtr(openaiModel), wantRefused: true, wantCheckReason: true},
		{name: "review session, the deployment credentials the provider too", kind: "review", model: strPtr(openaiModel), deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderOpenai}},
		{name: "review session, model of a provider the link does not carry", kind: "review", model: strPtr(anthropicModel), deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderAnthropic}},
		{name: "review session, turn names no model", kind: "review"},
		{name: "web session its owner created keeps its link", kind: "web", model: strPtr(openaiModel)},
		{name: "automation session has no link to name", kind: "automation", model: strPtr(openaiModel)},
		{name: "child session with a creator", kind: "child", model: strPtr(openaiModel), wantRefused: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creator := createLinkedMember(ctx, t, pool, fmt.Sprintf("gate-%d", i))
			// Deployment rows are global, so each case clears the previous
			// case's before seeding its own.
			if _, err := pool.Exec(ctx, `DELETE FROM provider_credentials WHERE scope = 'global'`); err != nil {
				t.Fatalf("clear global credentials: %v", err)
			}
			for _, p := range tc.deployment {
				createGlobalCredential(ctx, t, pool, p)
			}

			params := sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: creator, Repos: []byte(credentialGateRepos)}
			switch tc.kind {
			case "review":
				params.SpawnSource = sqlcgen.SessionSpawnSourceGithub
			case "automation":
				params.CreatedBy = pgtype.UUID{}
			case "child":
				params.SpawnSource = sqlcgen.SessionSpawnSourceGithub
				params.ParentSessionID = createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceGithub)
				params.SpawnDepth = int32(1)
			}
			session, err := narvipg.NewSessionStore(pool).Create(ctx, params)
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if tc.kind == "review" {
				claimPullRequest(ctx, t, pool, "acme/widgets", int32(100+i), session.ID)
			}

			turnStore := narvipg.NewTurnStore(pool)
			headSHA := "c0ffee"
			created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
				SessionID:       session.ID,
				Status:          sqlcgen.TurnStatusPending,
				Prompt:          strPtr("review this pull request"),
				ModelID:         tc.model,
				ReviewHeadSha:   &headSHA,
				IsReviewAttempt: tc.kind == "review",
			})
			if err != nil {
				t.Fatalf("create pending turn: %v", err)
			}

			sandboxStore := narvipg.NewSandboxStore(pool)
			if _, err := sandboxStore.Create(ctx, session.ID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: session.ID, Status: sqlcgen.SandboxStatusReady}); err != nil {
				t.Fatalf("move sandbox to ready: %v", err)
			}

			commander := &fakeSendCommander{}
			r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, session.ID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)

			waitUntil(t, 5*time.Second, func() bool {
				got, err := turnStore.Get(ctx, created.ID)
				return err == nil && got.Status != sqlcgen.TurnStatusPending
			})
			got, err := turnStore.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("get turn: %v", err)
			}

			if !tc.wantRefused {
				if got.Status != sqlcgen.TurnStatusProcessing {
					t.Errorf("turn status = %s, want %s (dispatched as before)", got.Status, sqlcgen.TurnStatusProcessing)
				}
				// The prompt is written after the commit that makes the turn
				// processing (technical plan §3.3), so the status above can be
				// read before it is sent.
				waitUntil(t, 5*time.Second, func() bool { return commander.callCount() >= 1 })
				if n := commander.callCount(); n != 1 {
					t.Errorf("prompts sent = %d, want 1", n)
				}
				return
			}

			if got.Status != sqlcgen.TurnStatusFailed {
				t.Fatalf("turn status = %s, want %s (refused before dispatch)", got.Status, sqlcgen.TurnStatusFailed)
			}
			if got.DispatchedAt.Valid {
				t.Error("turn dispatched_at set, want unset: a refused turn is never dispatched")
			}
			if n := commander.callCount(); n != 0 {
				t.Errorf("prompts sent = %d, want 0: a refused turn never reaches the sandbox", n)
			}

			gotSession, err := narvipg.NewSessionStore(pool).Get(ctx, session.ID)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if gotSession.Status != sqlcgen.SessionStatusFailed || gotSession.FailureReason == nil || *gotSession.FailureReason != sqlcgen.SessionFailureReasonNeverStarted {
				t.Errorf("session status/failure_reason = %s/%v, want failed/never_started", gotSession.Status, gotSession.FailureReason)
			}

			var reason string
			var dispatched bool
			if err := pool.QueryRow(ctx,
				`SELECT payload->>'reason', COALESCE((payload->>'dispatched')::boolean, false) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`,
				session.ID,
			).Scan(&reason, &dispatched); err != nil {
				t.Fatalf("read the refused turn's terminal event: %v", err)
			}
			if dispatched {
				t.Error("the refused turn's synthetic end is stamped dispatched, want unstamped: it never dispatched, and no page turn is its")
			}
			if !strings.HasPrefix(reason, string(providercredential.RefusalPersonalLinkOnly)+": ") || !strings.Contains(reason, *tc.model) {
				t.Errorf("terminal reason = %q, want it to name %q and the model %q", reason, providercredential.RefusalPersonalLinkOnly, *tc.model)
			}
			var warning string
			if err := pool.QueryRow(ctx,
				`SELECT payload->>'message' FROM events WHERE session_id = $1 AND type = 'warning'`,
				session.ID,
			).Scan(&warning); err != nil {
				t.Fatalf("read the refusal's session warning: %v", err)
			}
			if warning != reason {
				t.Errorf("session warning = %q, want the refusal's own reason %q", warning, reason)
			}

			var kind string
			var payload []byte
			err = pool.QueryRow(ctx, `SELECT kind, payload FROM outbox WHERE session_id = $1`, session.ID).Scan(&kind, &payload)
			if !tc.wantCheckReason {
				if n := countOutboxRowsForSession(ctx, t, pool, session.ID); n != 0 {
					t.Errorf("outbox rows = %d, want 0 (not a review attempt)", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("read the review check emission: %v", err)
			}
			if kind != string(ports.NotificationKindGitHubReviewCheck) {
				t.Fatalf("outbox kind = %q, want %q", kind, ports.NotificationKindGitHubReviewCheck)
			}
			var check ports.ReviewCheckPayload
			if err := json.Unmarshal(payload, &check); err != nil {
				t.Fatalf("unmarshal review check payload: %v", err)
			}
			if check.Phase != string(reviewcheck.PhaseTerminalNotAssessed) || check.NotAssessedReason != string(reviewcheck.NotAssessedPersonalLinkOnly) || check.AttemptID != created.ID.String() {
				t.Errorf("review check = phase %q, reason %q, attempt %q; want %q, %q, %q",
					check.Phase, check.NotAssessedReason, check.AttemptID,
					reviewcheck.PhaseTerminalNotAssessed, reviewcheck.NotAssessedPersonalLinkOnly, created.ID.String())
			}
		})
	}
}

// TestReviewCounterReviewerModel_NeverCountsTheRequestersLink runs a real
// spawn of a review session opened by a member with a linked ChatGPT
// account, on a Narvi-authored pull request whose authoring model is
// anthropic. The counter-reviewer's preference order puts openai before
// google, so a resolution that counted the requester's link would pin
// openai; it must pick among the deployment's credentials only, and with
// none, pin nothing -- the fallback it has when no opposing provider is
// credentialed.
func TestReviewCounterReviewerModel_NeverCountsTheRequestersLink(t *testing.T) {
	tests := []struct {
		name       string
		deployment []sqlcgen.ProviderCredentialProvider
		wantPrefix string // "" means no override pinned at all
	}{
		{name: "deployment credentials google", deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderGoogle}, wantPrefix: "google/"},
		{name: "deployment credentials nothing opposing", wantPrefix: ""},
		{name: "deployment credentials openai itself", deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderOpenai}, wantPrefix: "openai/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := newReviewCounterReviewerFixture(ctx, t, pool, "anthropic/claude-opus-4-5")

			creator := createLinkedMember(ctx, t, pool, "counter")
			if _, err := pool.Exec(ctx, `UPDATE sessions SET created_by = $1 WHERE id = $2`, creator, f.reviewSessionID); err != nil {
				t.Fatalf("attribute the review session to its first requester: %v", err)
			}
			for _, p := range tc.deployment {
				createGlobalCredential(ctx, t, pool, p)
			}

			got := f.spawnAndGetReviewCounterReviewerModel(ctx, t)
			switch {
			case tc.wantPrefix == "" && got != nil:
				t.Errorf("ReviewCounterReviewerModel = %q, want nil (only the requester's link carries an opposing provider)", *got)
			case tc.wantPrefix != "" && (got == nil || !strings.HasPrefix(*got, tc.wantPrefix)):
				t.Errorf("ReviewCounterReviewerModel = %v, want a %s model", got, tc.wantPrefix)
			}
		})
	}
}

// gatedReviewSession is a pull request's review session whose first
// requester holds a personal openai link, on a deployment holding only a
// global anthropic key, with a Ready sandbox: every turn naming an openai
// model is one the gate refuses.
func gatedReviewSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, prNumber int32) sqlcgen.Session {
	t.Helper()
	creator := createLinkedMember(ctx, t, pool, fmt.Sprintf("gated-%d", prNumber))
	if _, err := pool.Exec(ctx, `DELETE FROM provider_credentials WHERE scope = 'global'`); err != nil {
		t.Fatalf("clear global credentials: %v", err)
	}
	createGlobalCredential(ctx, t, pool, sqlcgen.ProviderCredentialProviderAnthropic)
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: creator, Repos: []byte(credentialGateRepos),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	claimPullRequest(ctx, t, pool, "acme/widgets", prNumber, session.ID)
	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: session.ID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	return session
}

func pendingTurnWithModel(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, model string, reviewAttempt bool) sqlcgen.Turn {
	t.Helper()
	headSHA := "c0ffee"
	created, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: strPtr("review this pull request"),
		ModelID: &model, ReviewHeadSha: &headSHA, IsReviewAttempt: reviewAttempt,
	})
	if err != nil {
		t.Fatalf("create pending turn: %v", err)
	}
	return created
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// TestDispatchGate_RefusedWorkflowStep_WithBlockedSelfEdge_EscalatesOnce
// drives a workflow-tracked review turn -- a custom review definition
// whose one step pins a model only the requester's link carries, with a
// self edge on `blocked`, the documented shape of an explicit retry loop --
// through the real actor. The refusal must not follow that edge: exactly
// one refusal, the round commits, the run waits for a person with a notice
// naming the refusal, the session row's lock is free for the stop route's
// own write, and a second dispatch round refuses nothing more.
func TestDispatchGate_RefusedWorkflowStep_WithBlockedSelfEdge_EscalatesOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	session := gatedReviewSession(ctx, t, pool, 400)

	var defID, stepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('review', 'test-refused-step-blocked-self-edge', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, model_id) VALUES ($1, 1, 'agent', '{{prompt}}', 'openai/gpt-5.4') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert step definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $2, 'blocked')`, defID, stepID); err != nil {
		t.Fatalf("insert blocked self edge: %v", err)
	}
	workflows := narvipg.NewWorkflowStore(pool)
	run, err := workflows.CreateRun(ctx, session.ID, "review", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepRun, err := workflows.CreateStepRun(ctx, run.ID, stepID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	refused := pendingTurnWithModel(ctx, t, pool, session.ID, "openai/gpt-5.4", true)
	if err := workflows.AttachTurn(ctx, stepRun.ID, refused.ID); err != nil {
		t.Fatalf("attach turn: %v", err)
	}

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	turns := narvipg.NewTurnStore(pool)
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, refused.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	assertOneRefusal := func(round string) {
		t.Helper()
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID); n != 1 {
			t.Errorf("%s: turns = %d, want the one refused turn and no re-queued attempt", round, n)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete'`, session.ID); n != 1 {
			t.Errorf("%s: terminal events = %d, want exactly one refusal", round, n)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID); n != 1 {
			t.Errorf("%s: step runs = %d, want the refused attempt alone", round, n)
		}
		if n := commander.callCount(); n != 0 {
			t.Errorf("%s: prompts sent = %d, want 0", round, n)
		}
	}
	assertOneRefusal("first round")

	gotRun, err := workflows.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.Status != sqlcgen.WorkflowRunStatusNeedsReview {
		t.Errorf("run status = %s, want needs_review", gotRun.Status)
	}
	gotStepRun, err := workflows.GetStepRun(ctx, stepRun.ID)
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	if gotStepRun.Status != sqlcgen.WorkflowStepRunStatusFailed {
		t.Errorf("step run status = %s, want failed", gotStepRun.Status)
	}
	var notice string
	if err := pool.QueryRow(ctx, `SELECT payload->>'text' FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubWorkflowDecision)).Scan(&notice); err != nil {
		t.Fatalf("read the run's escalation notice: %v", err)
	}
	if !strings.Contains(notice, "refused before it ran") || !strings.Contains(notice, string(providercredential.RefusalPersonalLinkOnly)) {
		t.Errorf("escalation notice = %q, want it to name the refusal", notice)
	}

	sendEnsureDispatched(ctx, t, a)
	// A second round has nothing to refuse: give it time to act, then
	// require that it did not.
	time.Sleep(500 * time.Millisecond)
	assertOneRefusal("second round")

	// The stop route's own write: the session row, locked FOR UPDATE, then
	// the request recorded -- within a lock timeout, as nothing still holds
	// the lock.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`, session.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("lock the session row: %v (the refusal round must have released it)", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET stop_requested_at = now() WHERE id = $1`, session.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("record a stop: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the stop: %v", err)
	}
}

// TestDispatchGate_RefusedTurnThenRunnableTurn_BothInOneRound queues a
// refused review turn ahead of one the deployment can run: the gate
// re-picks after the refusal, so the second turn is dispatched in the same
// round rather than left pending.
func TestDispatchGate_RefusedTurnThenRunnableTurn_BothInOneRound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	session := gatedReviewSession(ctx, t, pool, 401)
	first := pendingTurnWithModel(ctx, t, pool, session.ID, "openai/gpt-5.4", true)
	second := pendingTurnWithModel(ctx, t, pool, session.ID, "anthropic/claude-opus-4-5", false)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)

	turns := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool { return commander.callCount() == 1 })
	gotFirst, err := turns.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first turn: %v", err)
	}
	gotSecond, err := turns.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second turn: %v", err)
	}
	if gotFirst.Status != sqlcgen.TurnStatusFailed || gotSecond.Status != sqlcgen.TurnStatusProcessing {
		t.Errorf("turns = %s, %s; want the first refused (failed) and the second dispatched (processing) in one round", gotFirst.Status, gotSecond.Status)
	}
}

// TestRefusePersonalLinkOnlyPending_NeverRefusesATurnItsOwnRefusalQueued
// pins the within-round bound on its own, whatever the workflow engine
// does: a refusal that queues a new pending turn with the same refused
// model -- what following a retry edge would do -- ends the round after
// exactly one refusal, with the queued turn neither refused nor offered
// for dispatch, and left pending for the next round once this one commits.
func TestRefusePersonalLinkOnlyPending_NeverRefusesATurnItsOwnRefusalQueued(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	session := gatedReviewSession(ctx, t, pool, 402)
	pendingTurnWithModel(ctx, t, pool, session.ID, "openai/gpt-5.4", false)

	r := newDispatchTestRegistry(t, ctx, pool, nil, &fakeSendCommander{})
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	refusals := 0
	var queued pgtype.UUID
	requeue := func(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, turns []sqlcgen.Turn, target sqlcgen.Turn, provider providercredential.Provider, gen int, now time.Time) error {
		refusals++
		if refusals > 3 {
			return fmt.Errorf("refused %d turns in one round: the round refuses what its own refusals queue", refusals)
		}
		if err := a.refusePersonalLinkOnly(ctx, tx, sessionRow, turns, target, provider, gen, now); err != nil {
			return err
		}
		model := *target.ModelID
		created, err := a.stores.turn.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{
			SessionID: sessionRow.ID, Status: sqlcgen.TurnStatusPending, Prompt: strPtr("retry"), ModelID: &model,
		})
		if err != nil {
			return err
		}
		queued = created.ID
		return nil
	}

	var pickOK bool
	err = a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return err
		}
		turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
		if err != nil {
			return err
		}
		pendingID, hasPending := turn.NextToDispatch(toQueueEntries(turns))
		_, _, pickOK, err = a.refusePersonalLinkOnlyPending(ctx, tx, sessionRow, turns, pendingID, hasPending, 1, time.Now(), requeue)
		return err
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if refusals != 1 {
		t.Errorf("refusals = %d, want exactly one", refusals)
	}
	if pickOK {
		t.Error("the round offered a turn for dispatch, want none: the only pending turn is the one its refusal queued")
	}
	got, err := narvipg.NewTurnStore(pool).Get(ctx, queued)
	if err != nil {
		t.Fatalf("get queued turn: %v", err)
	}
	if got.Status != sqlcgen.TurnStatusPending {
		t.Errorf("queued turn = %s, want still pending, for the next round", got.Status)
	}
}

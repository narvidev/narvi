//go:build integration

package workflowengine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionnotice"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/workflow"
)

// This file is the workflow engine's half of technical plan §40.1's spend
// cap: the engine's next attempt is admitted by the session guard before
// its step run is created, an automatic advance refused escalates the run
// with the guard's text as its one notice, and a person's revision refused
// is answered with the refusal.

// capWorkflowSession is a Slack-origin session naming repo, capped at
// limit, its thread mapped.
func capWorkflowSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, limit, thread string) sqlcgen.Session {
	t.Helper()
	session, err := postgres.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceSlack,
		Repos:       []byte(`[{"url": "https://github.com/` + repo + `.git"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := postgres.NewSlackThreadSessionStore(pool).Claim(ctx, "C-GUARD", thread, session.ID); err != nil || !ok {
		t.Fatalf("claim slack thread: %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repo, limit); err != nil {
		t.Fatal(err)
	}
	return session
}

func countWhere(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// refusedAdvance runs an audit step whose turn took a Slack-origin session
// capped at $1.00 to $1.50 to its needs_fix end, through OnTurnCompleted,
// so the engine's automatic advance to the fix step meets the session
// guard. notifiedBefore claims the run's one escalation notice first, as
// an earlier escalation would have. It returns the session, the run and
// the refusal the guard makes.
func refusedAdvance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo, thread string, notifiedBefore bool) (sqlcgen.Session, pgtype.UUID, sessionguard.Refusal) {
	t.Helper()
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	session := capWorkflowSession(ctx, t, pool, repo, "1.00", thread)
	def := seedAuditFixLoopDefinition(t, ctx, pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}
	runID, auditStepRunID, auditTurnID := startRawRun(t, ctx, turns, workflows, session, def)
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_at = now(), cost_usd = 1.5 WHERE id = $1`, auditTurnID); err != nil {
		t.Fatal(err)
	}
	if notifiedBefore {
		if claimed, err := workflows.ClaimEscalationNotice(ctx, runID); err != nil || claimed != 1 {
			t.Fatalf("claim the run's notice beforehand: %d %v", claimed, err)
		}
	}
	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		completeWithOutcome(t, ctx, deps, session, auditStepRunID, auditTurnID, "needs_fix")
	})
	return session, runID, sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: session.ID.Bytes, Cap: 1_000_000, Spent: 1_500_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}, Turns: 1}
}

// TestWorkflowAdvance_AtSpendCap_EscalatesWithoutATurn: the engine's
// advance past a step that took the session past its cap is refused by the
// session guard before the next step run exists: no turn and no orphan step
// run are created, and the run escalates to needs_review -- the session
// waits on a person, nothing failed.
func TestWorkflowAdvance_AtSpendCap_EscalatesWithoutATurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	session, runID, _ := refusedAdvance(ctx, t, pool, "acme/workflow-cap", "2222.0001", false)

	runRow, err := postgres.NewWorkflowStore(pool).GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if string(runRow.Status) != "needs_review" {
		t.Fatalf("run status = %q, want needs_review: a refused advance escalates", runRow.Status)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID); n != 1 {
		t.Fatalf("step runs = %d, want the audit step's alone: the refused attempt's step run is never created", n)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID); n != 1 {
		t.Fatalf("turns = %d, want the audit turn alone", n)
	}
}

// TestSpendCap_WorkflowEscalationCarriesTheOneNotice: the crossing an
// automatic advance meets is told once. The run's escalation notice names
// the run, says it needs review and will not resume on its own even once
// the cap is raised, and carries the guard's text; the crossing's warning
// is recorded, and no separate guard notice is sent. When the run was
// already notified of an earlier escalation, the guard's own notice goes
// out instead.
func TestSpendCap_WorkflowEscalationCarriesTheOneNotice(t *testing.T) {
	ctx := context.Background()

	t.Run("the escalation's notice carries the guard's text", func(t *testing.T) {
		pool := newTestPool(t)
		session, runID, refusal := refusedAdvance(ctx, t, pool, "acme/workflow-notice", "2222.0002", false)
		var payload []byte
		if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = 'slack_workflow_decision'`, session.ID).Scan(&payload); err != nil {
			t.Fatalf("the escalation notice: %v", err)
		}
		var notice struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatal(err)
		}
		for _, part := range []string{
			"This workflow run (" + runID.String() + ") now needs your review",
			"the session reached its spend cap, so the run's next step was not started",
			"The run will not resume on its own, even once the cap is raised",
			"send the session a new turn",
			sessionguard.Text(refusal),
		} {
			if !strings.Contains(notice.Text, part) {
				t.Errorf("escalation notice %q does not say %q", notice.Text, part)
			}
		}
		if n := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = 'slack_session_guard'`, session.ID); n != 0 {
			t.Fatalf("guard notices = %d, want none: the escalation carries the one notice", n)
		}
		if n := countWhere(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, session.ID, turnguard.WarningMessageID(refusal)); n != 1 {
			t.Fatalf("warnings at the crossing's id = %d, want 1", n)
		}
	})

	t.Run("an already-notified run's crossing gets the guard's notice", func(t *testing.T) {
		pool := newTestPool(t)
		session, _, _ := refusedAdvance(ctx, t, pool, "acme/workflow-notified", "2222.0003", true)
		escalations := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = 'slack_workflow_decision'`, session.ID)
		guards := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = 'slack_session_guard'`, session.ID)
		if escalations != 0 || guards != 1 {
			t.Fatalf("escalation notices %d, guard notices %d; want the guard's one notice", escalations, guards)
		}
	})
}

// TestDispatchSameStepRevision_AtSpendCap_ReturnsTheRefusal: a person's
// revision of a step is refused with the guard's refusal as its error, and
// no step run or turn is created -- the decide endpoint answers 409 and
// rolls back.
func TestDispatchSameStepRevision_AtSpendCap_ReturnsTheRefusal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := postgres.NewTurnStore(pool)
	workflows := postgres.NewWorkflowStore(pool)
	session := capWorkflowSession(ctx, t, pool, "acme/workflow-revise", "1.00", "2222.0004")
	def := seedAuditFixLoopDefinition(t, ctx, pool)
	deps := workflowengine.Deps{
		Workflows:           workflows,
		Turns:               turns,
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
		Origin:              sessionguard.OriginPerson,
	}
	runID, _, auditTurnID := startRawRun(t, ctx, turns, workflows, session, def)
	if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 2 WHERE id = $1`, auditTurnID); err != nil {
		t.Fatal(err)
	}
	loaded, err := workflowengine.LoadDefinition(ctx, workflows, def.definitionID)
	if err != nil {
		t.Fatal(err)
	}
	var step workflow.StepDefinition
	for _, s := range loaded.Steps {
		if s.ID == workflow.ID(def.auditStepID.String()) {
			step = s
		}
	}
	var revisionErr error
	var turnID pgtype.UUID
	inTx(t, ctx, pool, deps, func(deps workflowengine.Deps) {
		turnID, revisionErr = workflowengine.DispatchSameStepRevision(ctx, deps, runID, step, "try again", session)
	})
	if _, ok := sessionguard.AsRefusal(revisionErr); !ok || turnID.Valid {
		t.Fatalf("revision past the cap = %v (turn %v), want the guard's refusal", revisionErr, turnID.Valid)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID); n != 1 {
		t.Fatalf("step runs = %d, want 1: a refused revision creates none", n)
	}
}

// TestSessionNoticeEnqueue_CarriesEachChannelsPayload: the one router of a
// session's notices (internal/app/sessionnotice) -- the workflow engine's
// and the session guard's -- enqueues to each source's own channel the
// payload its notifier delivers: the Slack thread, the issue tracker's
// agent session as a response (Success: an error activity otherwise), the
// pull request; and nothing for a web-origin session.
func TestSessionNoticeEnqueue_CarriesEachChannelsPayload(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := postgres.NewSessionStore(pool)
	stores := sessionnotice.Stores{
		SlackThreadSessions: postgres.NewSlackThreadSessionStore(pool),
		LinearAgentSessions: postgres.NewLinearAgentSessionStore(pool),
		GitHubPRSessions:    postgres.NewGitHubPRSessionStore(pool),
		Outbox:              postgres.NewOutboxStore(pool, false),
	}
	newSession := func(source sqlcgen.SessionSpawnSource) sqlcgen.Session {
		t.Helper()
		s, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: source})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	payloadOf := func(sessionID pgtype.UUID, kind string, into any) {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, kind).Scan(&raw); err != nil {
			t.Fatalf("the %s notice: %v", kind, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	const text = "the notice's text"

	slackSession := newSession(sqlcgen.SessionSpawnSourceSlack)
	if _, ok, err := stores.SlackThreadSessions.Claim(ctx, "C-NOTICE", "3333.0001", slackSession.ID); err != nil || !ok {
		t.Fatalf("map the thread: %v %v", ok, err)
	}
	linearSession := newSession(sqlcgen.SessionSpawnSourceLinear)
	if _, err := stores.LinearAgentSessions.Claim(ctx, "agent-session-notice", "org-notice"); err != nil {
		t.Fatal(err)
	}
	if err := stores.LinearAgentSessions.SetSessionID(ctx, "agent-session-notice", linearSession.ID); err != nil {
		t.Fatal(err)
	}
	githubSession := newSession(sqlcgen.SessionSpawnSourceGithub)
	if _, err := pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ('acme/notice', 77, $1)`, githubSession.ID); err != nil {
		t.Fatal(err)
	}
	webSession := newSession(sqlcgen.SessionSpawnSourceWeb)

	for _, s := range []sqlcgen.Session{slackSession, linearSession, githubSession, webSession} {
		enqueued, err := sessionnotice.Enqueue(ctx, stores, s, turnguard.NoticeKinds, text)
		if err != nil {
			t.Fatalf("%s: %v", s.SpawnSource, err)
		}
		if want := s.SpawnSource != sqlcgen.SessionSpawnSourceWeb; enqueued != want {
			t.Fatalf("%s: enqueued %v, want %v", s.SpawnSource, enqueued, want)
		}
	}

	var slackPayload slackapi.Payload
	payloadOf(slackSession.ID, string(ports.NotificationKindSlackSessionGuard), &slackPayload)
	if slackPayload != (slackapi.Payload{ChannelID: "C-NOTICE", ThreadTS: "3333.0001", Text: text}) {
		t.Errorf("slack payload = %+v", slackPayload)
	}
	var linearPayload linearapi.Payload
	payloadOf(linearSession.ID, string(ports.NotificationKindLinearSessionGuard), &linearPayload)
	if linearPayload != (linearapi.Payload{AgentSessionID: "agent-session-notice", OrganizationID: "org-notice", Text: text, Success: true}) {
		t.Errorf("issue tracker payload = %+v, want a response (Success) on its agent session", linearPayload)
	}
	var githubPayload githubapi.Payload
	payloadOf(githubSession.ID, string(ports.NotificationKindGitHubSessionGuard), &githubPayload)
	if githubPayload != (githubapi.Payload{Owner: "acme", Repo: "notice", PRNumber: 77, Text: text}) {
		t.Errorf("code host payload = %+v", githubPayload)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1`, webSession.ID); n != 0 {
		t.Errorf("web session notices = %d, want none", n)
	}
}

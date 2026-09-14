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

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves §5.1's ("outbox delivery", §5.1) own enqueue-side
// wiring: a slack/github/linear-origin session's turn completion writes
// exactly one correctly-shaped outbox row, and a web-origin session's
// completion writes none -- see outboxenqueue.go's own doc comment for the
// design this exercises.

// createTestSessionWithSpawnSource creates a bare session (no repos) with
// the given spawnSource -- mirrors createTestSession's own shape, just
// parameterized on the one field this file's own tests vary.
func createTestSessionWithSpawnSource(ctx context.Context, t *testing.T, pool *pgxpool.Pool, spawnSource sqlcgen.SessionSpawnSource) pgtype.UUID {
	t.Helper()
	created, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: spawnSource,
	})
	if err != nil {
		t.Fatalf("create test session with spawn source %q: %v", spawnSource, err)
	}
	return created.ID
}

// countOutboxRowsForSession counts every outbox row for sessionID.
func countOutboxRowsForSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return n
}

// getSoleOutboxRowForSession fetches the single outbox row for sessionID,
// failing the test if there isn't exactly one.
func getSoleOutboxRowForSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) sqlcgen.Outbox {
	t.Helper()
	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 1 {
		t.Fatalf("outbox row count for session = %d, want exactly 1", n)
	}

	rows, err := pool.Query(ctx, `SELECT id, session_id, kind, payload, status, attempts, next_attempt_at, delivered_at, last_error, created_at FROM outbox WHERE session_id = $1`, sessionID)
	if err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	defer rows.Close()

	var row sqlcgen.Outbox
	if !rows.Next() {
		t.Fatal("no outbox row found")
	}
	if err := rows.Scan(&row.ID, &row.SessionID, &row.Kind, &row.Payload, &row.Status, &row.Attempts, &row.NextAttemptAt, &row.DeliveredAt, &row.LastError, &row.CreatedAt); err != nil {
		t.Fatalf("scan outbox row: %v", err)
	}
	return row
}

// TestCompleteProcessingTurn_SlackOrigin_EnqueuesExactlyOneSlackOutboxRow
// proves a slack-origin session's SUCCESSFUL turn completion enqueues
// exactly one 'slack'-kind outbox row, shaped as slackapi.Payload, carrying
// the session's own reverse-looked-up channel/thread address.
func TestCompleteProcessingTurn_SlackOrigin_EnqueuesExactlyOneSlackOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)

	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	if row.Kind != string(ports.NotificationKindSlack) {
		t.Errorf("Kind = %q, want %q", row.Kind, ports.NotificationKindSlack)
	}
	if row.Status != sqlcgen.OutboxStatusPending {
		t.Errorf("Status = %q, want %q", row.Status, sqlcgen.OutboxStatusPending)
	}

	var payload slackapi.Payload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload as slackapi.Payload: %v", err)
	}
	if payload.ChannelID != "C123" {
		t.Errorf("ChannelID = %q, want %q", payload.ChannelID, "C123")
	}
	if payload.ThreadTS != "1700000000.000100" {
		t.Errorf("ThreadTS = %q, want %q", payload.ThreadTS, "1700000000.000100")
	}
	if payload.Text == "" {
		t.Error("Text is empty, want a human-readable outcome message")
	}
}

// TestCompleteProcessingTurn_GitHubOrigin_EnqueuesNoRawCommentOutboxRow
// proves a github-origin session's FAILED turn completion enqueues NO
// outbox row at all any more.
//
// §8.2 ("server-side verdict", §8.2/§5.2) audit fix -- RAW-COMMENT
// BLOCKING: a github-origin session is, by construction, a review
// session (github_pr_sessions is the ONLY mechanism that ever creates
// one, internal/adapters/inbound/github/doc.go) -- so this generic,
// system-synthesized outcome-text comment ("Turn completed successfully."
// / "Turn failed (...)."), posted completely independently of whatever
// the agent itself said, is exactly the "ordinary issue comment [that]
// bypasses the [verdict-posting] tool" this Step forbids. This test used
// to prove the OPPOSITE (that this path enqueued exactly one
// 'github'-kind row) -- it is renamed and inverted here to lock in the
// new, correct behavior: a github-origin turn completion enqueues NOTHING
// via this generic path any more. The verdict-posting tool
// (internal/adapters/inbound/httpapi/reviewverdict.go) is now the ONLY
// way a review session's turn reaches the PR as a comment/review.
func TestCompleteProcessingTurn_GitHubOrigin_EnqueuesNoRawCommentOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceGithub)

	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "acme/widgets", 42); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, "acme/widgets", 42, sessionID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeFailed),
	})

	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 0 {
		t.Errorf("outbox row count = %d, want 0 (raw-comment posting must be blocked for a review session, §8.2)", n)
	}
}

// TestCompleteProcessingTurn_LinearOrigin_EnqueuesExactlyOneLinearOutboxRow
// proves a linear-origin session's SUCCESSFUL turn completion enqueues
// exactly one 'linear'-kind outbox row, shaped as linearapi.Payload, with
// Success=true.
func TestCompleteProcessingTurn_LinearOrigin_EnqueuesExactlyOneLinearOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)

	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	agentSessions := narvipg.NewLinearAgentSessionStore(pool)
	if _, err := agentSessions.Claim(ctx, "agent-session-1", "org-1"); err != nil {
		t.Fatalf("claim linear agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, "agent-session-1", sessionID); err != nil {
		t.Fatalf("set linear agent session id: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	if row.Kind != string(ports.NotificationKindLinear) {
		t.Errorf("Kind = %q, want %q", row.Kind, ports.NotificationKindLinear)
	}

	var payload linearapi.Payload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.AgentSessionID != "agent-session-1" {
		t.Errorf("AgentSessionID = %q, want %q", payload.AgentSessionID, "agent-session-1")
	}
	if payload.OrganizationID != "org-1" {
		t.Errorf("OrganizationID = %q, want %q", payload.OrganizationID, "org-1")
	}
	if !payload.Success {
		t.Error("Success = false, want true for a completed turn")
	}
}

// TestTurnDeadlineTimeout_EnqueuesOutboxNotificationPerOrigin proves the
// turn_deadline TIMEOUT path (handleTurnDeadlineTimer, timerfired.go)
// enqueues the same per-origin outbox notification the real-
// execution_complete path (completeProcessingTurn) already did.
//
// The gap this locks closed: §5.1 wired enqueueOutboxNotification into
// completeProcessingTurn only. A turn that reached its terminal `failed`
// state via turn_deadline expiring -- the SAME turn.Transition +
// UpdateStatus + synthetic execution_complete shape, just driven by a
// timer instead of a wire event -- wrote no outbox row at all, so a Slack-
// or Linear-origin session whose turn simply timed out never told its
// originating channel anything. Only the web UI, which reads turn state
// directly and needs no notification, ever showed the failure.
//
// Table-driven across all four spawn_source values, since the correct
// answer genuinely differs per origin and each case must be pinned
// independently: slack/linear DO get a row, github does NOT (§8.2's own
// raw-comment blocking, §8.2/§5.2 -- see the github case in
// outboxenqueue.go), and web does NOT (no external channel at all).
func TestTurnDeadlineTimeout_EnqueuesOutboxNotificationPerOrigin(t *testing.T) {
	tests := []struct {
		name        string
		spawnSource sqlcgen.SessionSpawnSource
		// seedReverseLookup writes the origin's own reverse-lookup row
		// (slack_thread_sessions / linear_agent_sessions / ...), the same
		// row the real ingress path writes at session creation. nil for an
		// origin that has none.
		seedReverseLookup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID)
		wantRows          int
		wantKind          ports.NotificationKind
		// checkPayload asserts the origin-specific payload shape, run only
		// when wantRows is 1.
		checkPayload func(t *testing.T, raw []byte)
	}{
		{
			name:        "slack origin notifies its thread that the turn timed out",
			spawnSource: sqlcgen.SessionSpawnSourceSlack,
			seedReverseLookup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
				t.Helper()
				if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C999", "1700000000.000999", sessionID); err != nil || !ok {
					t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
				}
			},
			wantRows: 1,
			wantKind: ports.NotificationKindSlack,
			checkPayload: func(t *testing.T, raw []byte) {
				t.Helper()
				var payload slackapi.Payload
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatalf("unmarshal payload as slackapi.Payload: %v", err)
				}
				if payload.ChannelID != "C999" {
					t.Errorf("ChannelID = %q, want %q", payload.ChannelID, "C999")
				}
				if payload.ThreadTS != "1700000000.000999" {
					t.Errorf("ThreadTS = %q, want %q", payload.ThreadTS, "1700000000.000999")
				}
				// The whole point of notifying at all: the text must say the
				// turn FAILED, and name the timeout as the reason -- not
				// outcomeText's generic "Turn finished." fallback, which is
				// what an unhandled turn.TriggerTimeout used to render.
				if want := "Turn failed (timeout)."; payload.Text != want {
					t.Errorf("Text = %q, want %q", payload.Text, want)
				}
			},
		},
		{
			name:        "linear origin notifies its agent session with Success=false",
			spawnSource: sqlcgen.SessionSpawnSourceLinear,
			seedReverseLookup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
				t.Helper()
				agentSessions := narvipg.NewLinearAgentSessionStore(pool)
				if _, err := agentSessions.Claim(ctx, "agent-session-timeout", "org-timeout"); err != nil {
					t.Fatalf("claim linear agent session: %v", err)
				}
				if err := agentSessions.SetSessionID(ctx, "agent-session-timeout", sessionID); err != nil {
					t.Fatalf("set linear agent session id: %v", err)
				}
			},
			wantRows: 1,
			wantKind: ports.NotificationKindLinear,
			checkPayload: func(t *testing.T, raw []byte) {
				t.Helper()
				var payload linearapi.Payload
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatalf("unmarshal payload as linearapi.Payload: %v", err)
				}
				if payload.AgentSessionID != "agent-session-timeout" {
					t.Errorf("AgentSessionID = %q, want %q", payload.AgentSessionID, "agent-session-timeout")
				}
				if payload.OrganizationID != "org-timeout" {
					t.Errorf("OrganizationID = %q, want %q", payload.OrganizationID, "org-timeout")
				}
				// Linear renders an error AgentActivity, not a response one,
				// off this flag -- a timed-out turn is never a success.
				if payload.Success {
					t.Error("Success = true, want false for a timed-out turn")
				}
				if want := "Turn failed (timeout)."; payload.Text != want {
					t.Errorf("Text = %q, want %q", payload.Text, want)
				}
			},
		},
		{
			name:        "github origin stays silent (§8.2 raw-comment blocking)",
			spawnSource: sqlcgen.SessionSpawnSourceGithub,
			seedReverseLookup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
				t.Helper()
				prSessions := narvipg.NewGitHubPRSessionStore(pool)
				if err := prSessions.EnsureRow(ctx, "acme/widgets", 99); err != nil {
					t.Fatalf("ensure github pr session row: %v", err)
				}
				if err := prSessions.SetSessionID(ctx, "acme/widgets", 99, sessionID); err != nil {
					t.Fatalf("set github pr session id: %v", err)
				}
			},
			wantRows: 0,
		},
		{
			name:        "web origin stays silent (no external channel)",
			spawnSource: sqlcgen.SessionSpawnSourceWeb,
			wantRows:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)

			sessionID := createTestSessionWithSpawnSource(ctx, t, pool, tt.spawnSource)
			if tt.seedReverseLookup != nil {
				tt.seedReverseLookup(ctx, t, pool, sessionID)
			}

			timeouts := platform.DefaultTimeouts()
			timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default

			// Seeded exactly like TestTurnDeadlineTimerFired_FullRoundTrip
			// (timerfired_integration_test.go): a turn already Processing
			// whose dispatched_at is comfortably past the tiny injected
			// deadline, so EvaluateTurnDeadline genuinely reports IsTimedOut.
			turnStore := narvipg.NewTurnStore(pool)
			created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
				SessionID: sessionID,
				Status:    sqlcgen.TurnStatusPending,
			})
			if err != nil {
				t.Fatalf("create turn: %v", err)
			}
			if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
				ID:           created.ID,
				Status:       sqlcgen.TurnStatusProcessing,
				DispatchedAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
			}); err != nil {
				t.Fatalf("move turn to processing: %v", err)
			}

			r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })

			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			// The REAL production entry point -- a TimerFired command
			// through the actor's own mailbox, never a direct call into the
			// unexported handler.
			if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
				t.Fatalf("Send: %v", err)
			}

			// The turn reaching Failed is what tells us the handler's own
			// transact has committed -- the outbox row, written in that SAME
			// transaction (§5.1), is therefore already durable by then.
			waitUntil(t, 5*time.Second, func() bool {
				got, err := turnStore.Get(ctx, created.ID)
				return err == nil && got.Status == sqlcgen.TurnStatusFailed
			})

			if tt.wantRows == 0 {
				if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 0 {
					t.Errorf("outbox row count = %d, want 0", n)
				}
				return
			}

			row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
			if row.Kind != string(tt.wantKind) {
				t.Errorf("Kind = %q, want %q", row.Kind, tt.wantKind)
			}
			if row.Status != sqlcgen.OutboxStatusPending {
				t.Errorf("Status = %q, want %q", row.Status, sqlcgen.OutboxStatusPending)
			}
			if tt.checkPayload != nil {
				tt.checkPayload(t, row.Payload)
			}
		})
	}
}

// TestCompleteProcessingTurn_WebOrigin_EnqueuesNoOutboxRow proves a
// 'web'-origin session's turn completion enqueues NOTHING -- there is no
// external channel to notify.
func TestCompleteProcessingTurn_WebOrigin_EnqueuesNoOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSession(ctx, t, pool)

	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 0 {
		t.Errorf("outbox row count for web-origin session = %d, want 0", n)
	}
}

// planStepsBlockContent is a well-formed plan-mode turn's own streamed text:
// real prose, followed by exactly one valid ```plan-steps fenced block --
// the SAME shape RenderStructureInstruction (internal/domain/plan) asks a
// plan-mode model to emit. Shared by the two tests below, which exist to
// close a real fixture gap (this Step's own review): both
// enqueueOutboxNotification call sites that invoke plandomain.
// StripStructureBlock -- the Slack branch (this file's own PlanApprovalPayload
// construction) and the Linear branch (planApprovalLinearText) -- were,
// before these two tests, never exercised by ANY fixture anywhere in
// sessionactor, outboxworker, slack or linear that actually contained a
// ```plan-steps block. Deleting either StripStructureBlock call left every
// existing test green; these two are what would catch that deletion.
const planStepsBlockContent = "Here is my plan.\n\n1. Add a table.\n2. Wire it up.\n\n```plan-steps\n" +
	`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["a.sql"]}],"scopeEstimate":"1 file"}` +
	"\n```\n"

// seedPlanStepsTokenEvent seeds one "token" event carrying
// planStepsBlockContent as messageID's own cumulative text -- the exact
// shape ToContentEvents (planapprovalcontent.go) decodes, mirroring this
// file's own TestPlanContentText_LongEventHistory... precedent
// (planapprovalcontent_integration_test.go) for building a token payload.
func seedPlanStepsTokenEvent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, messageID string) {
	t.Helper()
	tokenPayload, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "token", Text: planStepsBlockContent})
	if err != nil {
		t.Fatalf("marshal token payload: %v", err)
	}
	if _, err := narvipg.NewEventStore(pool).Create(ctx, sqlcgen.CreateEventParams{
		SessionID: sessionID,
		Type:      "token",
		MessageID: messageID,
		Payload:   tokenPayload,
	}); err != nil {
		t.Fatalf("seed plan-steps token event: %v", err)
	}
}

// TestCompleteProcessingTurn_SlackOrigin_PlanApproval_StripsStructureBlockFromText
// closes the fixture gap this Step's own review found: a plan-mode turn's
// own content that genuinely carries a ```plan-steps block, run through the
// REAL completion pipeline, must reach the Slack plan-approval payload's own
// Text field with that block already removed -- never the raw JSON a human
// reading the Slack message was never supposed to see (structured.go's own
// StripStructureBlock doc comment).
func TestCompleteProcessingTurn_SlackOrigin_PlanApproval_StripsStructureBlockFromText(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurnWithPlanMode(ctx, t, turnStore, sessionID, true, nil)
	seedPlanStepsTokenEvent(ctx, t, pool, sessionID, "slack-plan-steps-msg")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	if row.Kind != "slack_plan_approval" {
		t.Fatalf("Kind = %q, want %q", row.Kind, "slack_plan_approval")
	}

	var payload slackapi.PlanApprovalPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload as slackapi.PlanApprovalPayload: %v", err)
	}
	if strings.Contains(payload.Text, "plan-steps") || strings.Contains(payload.Text, "scopeEstimate") {
		t.Errorf("Text = %q, want the ```plan-steps block stripped -- a human reading Slack must never see the raw machine block", payload.Text)
	}
	if !strings.Contains(payload.Text, "Here is my plan.") {
		t.Errorf("Text = %q, want the surrounding prose preserved", payload.Text)
	}
}

// TestCompleteProcessingTurn_LinearOrigin_PlanApproval_StripsStructureBlockFromText
// is the Linear twin of the Slack test above -- the SAME fixture gap, closed
// for outboxenqueue.go's OTHER StripStructureBlock call site
// (planApprovalLinearText).
func TestCompleteProcessingTurn_LinearOrigin_PlanApproval_StripsStructureBlockFromText(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)
	if _, err := agentSessions.Claim(ctx, "agent-session-plan", "org-plan"); err != nil {
		t.Fatalf("claim linear agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, "agent-session-plan", sessionID); err != nil {
		t.Fatalf("set linear agent session id: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurnWithPlanMode(ctx, t, turnStore, sessionID, true, nil)
	seedPlanStepsTokenEvent(ctx, t, pool, sessionID, "linear-plan-steps-msg")

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})

	row := getSoleOutboxRowForSession(ctx, t, pool, sessionID)
	if row.Kind != string(ports.NotificationKindLinear) {
		t.Fatalf("Kind = %q, want %q", row.Kind, ports.NotificationKindLinear)
	}

	var payload linearapi.Payload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload as linearapi.Payload: %v", err)
	}
	if strings.Contains(payload.Text, "plan-steps") || strings.Contains(payload.Text, "scopeEstimate") {
		t.Errorf("Text = %q, want the ```plan-steps block stripped -- a human reading Linear must never see the raw machine block", payload.Text)
	}
	if !strings.Contains(payload.Text, "Here is my plan.") {
		t.Errorf("Text = %q, want the surrounding prose preserved", payload.Text)
	}
	if !strings.Contains(payload.Text, "Plan v1 is ready for review") {
		t.Errorf("Text = %q, want the planApprovalLinearText wrapper", payload.Text)
	}
}

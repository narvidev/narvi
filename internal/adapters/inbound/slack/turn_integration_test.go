//go:build integration

// Audit-fix batch tests (findings H7/M6/L20) for internal/adapters/inbound/
// slack's own addTurn (turn.go), exercised through the real POST
// /webhooks/slack handler -- mirrors handler_integration_test.go's own
// conventions exactly (same package, same newTestPool/appMentionEnvelope/
// messageEnvelope/signedSlackRequest helpers), adding a request-capturing
// fake Slack API server (mirroring interactive_integration_test.go's own
// recordedSlackRequest precedent) so these tests can assert on the actual
// in-thread ack TEXT posted, not just the resulting Postgres state.
package slack_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// slackAckTestRig mirrors newSlackTestRig (handler_integration_test.go)
// exactly, except its own fake Slack API server CAPTURES every
// chat.postMessage/chat.postEphemeral request instead of just acking --
// this file's own tests need to assert on the actual ack TEXT posted
// (ackBusyText's own new wording, the M6 audit fix), not only on Postgres
// state.
type slackAckTestRig struct {
	handler  http.HandlerFunc
	pool     *pgxpool.Pool
	sessions *narvipg.SessionStore
	turns    *narvipg.TurnStore
	threads  *narvipg.SlackThreadSessionStore
	auditLog *narvipg.AuditLogStore

	requests chan recordedSlackRequestBody
}

// recordedSlackRequestBody captures one request the fake Slack API server
// observed -- path plus decoded JSON body.
type recordedSlackRequestBody struct {
	path string
	body map[string]any
}

func newSlackAckTestRig(t *testing.T, pool *pgxpool.Pool) *slackAckTestRig {
	t.Helper()
	ctx := context.Background()

	// Audit-fix batch addition: see newSlackTestRig's own identical call
	// (handler_integration_test.go) -- this file's own tests reuse the SAME
	// appMentionEnvelope/messageEnvelope fixed user ids, which must now
	// resolve to a genuinely linked, sufficiently-privileged actor.
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

	requests := make(chan recordedSlackRequestBody, 16)
	ackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- recordedSlackRequestBody{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ackServer.Close)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// prSessions (§31.4): see newSlackTestRigWithEpistemicCheckDefault's own
	// identical addition (handler_integration_test.go) -- this rig's own
	// DefaultRepoURL below is the SAME fixed repo, made known here once.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	handler := slack.NewHandler(slack.Deps{
		Pool:            pool,
		Sessions:        sessions,
		Turns:           turns,
		Environments:    environments,
		Registry:        registry,
		Deliveries:      deliveries,
		Threads:         threads,
		AuditLog:        auditLog,
		Participants:    narvipg.NewParticipantStore(pool),
		PRSessions:      prSessions,
		SigningSecret:   testSigningSecret,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:     slackapi.New(ackServer.Client(), ackServer.URL, "test-bot-token"),
		Timeouts:        platform.DefaultTimeouts(),
		IdentityLink: identitylink.Deps{
			Pool:          pool,
			Users:         narvipg.NewUserStore(pool),
			Identities:    narvipg.NewIdentityStore(pool),
			LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
			AuditLog:      auditLog,
			PublicBaseURL: "https://narvi.example.com",
			PromptTTL:     time.Hour,
		},
	})

	return &slackAckTestRig{
		handler: handler, pool: pool, sessions: sessions, turns: turns, threads: threads, auditLog: auditLog,
		requests: requests,
	}
}

// drainAckTexts reads every recorded chat.postMessage request's own "text"
// field off rig.requests -- every outbound call this package makes
// (postAckBounded/postEphemeralBounded) is synchronous WITHIN the
// handler's own request/response cycle, so by the time rig.handler(...)
// has returned, every request it made this call is already sitting in
// the buffered channel; a plain non-blocking drain (no timer, no sleep)
// is enough.
func (r *slackAckTestRig) drainAckTexts(t *testing.T) []string {
	t.Helper()
	var texts []string
	for {
		select {
		case req := <-r.requests:
			if text, ok := req.body["text"].(string); ok {
				texts = append(texts, text)
			}
		default:
			return texts
		}
	}
}

// TestHandler_ReplyOnMappedThread_WritesAuditLogRow is the audit-fix
// batch's own regression test for H7 ("Slack ... never writes to
// audit_log"): a reply turn added via addTurn (turn.go) now gets the SAME
// turn.create audit_log row REST's own CreateTurn already wrote.
func TestHandler_ReplyOnMappedThread_WritesAuditLogRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	firstEnvelope := appMentionEnvelope("Ev0AUDIT001", "C0AUDIT", "1700000050.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0AUDIT", "1700000050.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}

	firstTurns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil || len(firstTurns) != 1 {
		t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
	}
	// Drive the first turn terminal so the reply below is free to add a
	// second one (mirrors TestHandler_ReplyOnMappedThread_AddsTurnToSameSession's
	// own identical setup, handler_integration_test.go).
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: firstTurns[0].ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	replyEnvelope := messageEnvelope("Ev0AUDIT002", "C0AUDIT", "1700000051.000200", "1700000050.000100", "here is more context")
	req2 := signedSlackRequest(t, replyEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	finalTurns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession after reply: %v", err)
	}
	if len(finalTurns) != 2 {
		t.Fatalf("len(turns) after reply = %d, want 2", len(finalTurns))
	}

	// Identify the reply's own turn (the one that is NOT firstTurns[0]).
	var replyTurnID pgtype.UUID
	for _, tn := range finalTurns {
		if tn.ID != firstTurns[0].ID {
			replyTurnID = tn.ID
		}
	}
	if !replyTurnID.Valid {
		t.Fatal("could not identify the reply's own new turn")
	}

	var auditCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'turn.create' AND resource_type = 'turn' AND resource_id = $1`,
		replyTurnID.String(),
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit_log row count for reply turn = %d, want 1 (H7: Slack-originated turns must now be audited)", auditCount)
	}
}

// TestHandler_ReplyWhileTurnOpen_PostsHonestBusyMessage is the M6 audit
// fix's own regression test: a reply that arrives while the session
// already has an open turn must NOT create a second turn, and the in-
// thread ack posted back must be the NEW, honest ackBusyText wording --
// never the OLD "I'll pick this up next" promise, which nothing ever
// fulfilled.
func TestHandler_ReplyWhileTurnOpen_PostsHonestBusyMessage(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	firstEnvelope := appMentionEnvelope("Ev0BUSY001", "C0BUSY", "1700000060.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	rig.drainAckTexts(t) // discard the first mention's own ack

	mapping, err := rig.threads.Get(ctx, "C0BUSY", "1700000060.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	// The first turn is deliberately left Pending (never driven terminal)
	// -- exactly the "still open" precondition this test needs.

	replyEnvelope := messageEnvelope("Ev0BUSY002", "C0BUSY", "1700000061.000200", "1700000060.000100", "any update?")
	req2 := signedSlackRequest(t, replyEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply while busy: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (the busy reply must never be queued)", len(turns))
	}

	texts := rig.drainAckTexts(t)
	var gotBusyText bool
	for _, text := range texts {
		if strings.Contains(text, "I'll pick this up next") {
			t.Errorf("ack text %q still contains the OLD, dishonest promise -- M6 audit fix requires this wording be replaced", text)
		}
		if strings.Contains(text, "wasn't queued") {
			gotBusyText = true
		}
	}
	if !gotBusyText {
		t.Errorf("no ack text among %v contained the new, honest busy wording", texts)
	}

	// No audit row for a turn that was never created.
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'turn.create'`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit_log turn.create row count = %d, want exactly 1 (only the first mention's own turn, never the dropped reply)", auditCount)
	}
}

// TestHandler_ReplyOnMappedThread_LogsSessionAndTurnID is the L20 audit
// fix's own regression test: this package previously logged NOTHING on a
// successful turn add at all. Captures slog's own default logger output
// (mirroring internal/adapters/inbound/linear/setsessionid_retry_integration_test.go's
// own identical precedent) and asserts session_id/turn_id both appear.
func TestHandler_ReplyOnMappedThread_LogsSessionAndTurnID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	// syncLogBuffer (handler_integration_test.go), not a bare bytes.Buffer:
	// this test's own mention below creates a turn, which fires the SAME
	// fire-and-forget GetOrSpawn+EnsureDispatched dispatch trigger every
	// turn-creation call site uses -- the session's Actor can still be
	// mid-flight on its own background goroutine, logging through this SAME
	// redirected default logger, while this test's own goroutine reads
	// logOutput below. See syncLogBuffer's own doc comment for the full race.
	logBuf := &syncLogBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	envelope := appMentionEnvelope("Ev0LOG001", "C0LOG", "1700000070.000100", "", "please help")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0LOG", "1700000070.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil || len(turns) != 1 {
		t.Fatalf("ListForSession: turns=%v err=%v, want exactly 1", turns, err)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, mapping.SessionID.String()) {
		t.Errorf("log output missing session_id %q; got: %s", mapping.SessionID.String(), logOutput)
	}
	if !strings.Contains(logOutput, turns[0].ID.String()) {
		t.Errorf("log output missing turn_id %q; got: %s", turns[0].ID.String(), logOutput)
	}
	if !strings.Contains(logOutput, "slack: added turn") {
		t.Errorf("log output missing the success log line; got: %s", logOutput)
	}
}

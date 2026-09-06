//go:build integration

// Full HTTP-level integration test for POST /webhooks/linear (
// "Linear ingress", §8.10): a synthetic, correctly-signed Linear
// AgentSessionEvent webhook payload (built to mirror the REAL field
// names this Step's own investigation confirmed against Linear's live
// schema/docs -- see payload.go's own doc comment) is POSTed directly at
// linear.NewWebhookHandler, proving it results in a real session + turn
// row in Postgres, exactly the way a real Linear delegation/mention
// would. Also proves both dedupe layers (webhook delivery id AND Linear's
// own agent-session identity) actually prevent a duplicate session.
package linear_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go), not freshly per test/container as this function
// used to do itself. Kept as a thin wrapper under its own original
// name/signature so every existing call site in this package's own
// *_integration_test.go files keeps compiling unchanged. See sharedpool_
// integration_test.go's own top doc comment for the full container-reuse
// story.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

const testWebhookSecret = "test-linear-webhook-signing-secret"

// signBody computes Linear's own real signature scheme (hex HMAC-SHA256
// over the raw body, no prefix, no timestamp folded in -- see
// signature_test.go's own linearSign for the identical helper, duplicated
// here since this file lives in the external linear_test package).
func signBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// agentSessionCreatedPayload builds a synthetic, real-shaped
// AgentSessionEvent "created" webhook body -- every field name mirrors
// Linear's own live GraphQL schema (AgentSessionEventWebhookPayload /
// AgentSessionWebhookPayload / IssueWithDescriptionChildWebhookPayload),
// fetched directly during this Step's own investigation (see payload.go).
func agentSessionCreatedPayload(agentSessionID, organizationID string) []byte {
	body := fmt.Sprintf(`{
		"action": "created",
		"type": "AgentSessionEvent",
		"organizationId": %q,
		"webhookTimestamp": %d,
		"agentSession": {
			"id": %q,
			"issue": {"identifier": "ENG-123", "title": "Fix accessibility on checkout page"},
			"url": "https://linear.app/narvi/issue/ENG-123"
		},
		"promptContext": "<issue identifier=\"ENG-123\"><title>Fix accessibility on checkout page</title></issue>"
	}`, organizationID, time.Now().UnixMilli(), agentSessionID)
	return []byte(body)
}

func postWebhook(t *testing.T, handler http.HandlerFunc, body []byte, deliveryID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Linear-Event", "AgentSessionEvent")
	req.Header.Set("Linear-Delivery", deliveryID)
	req.Header.Set("Linear-Signature", signBody([]byte(testWebhookSecret), body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// syncLogBuffer is a mutex-guarded io.Writer + String() capture buffer for
// tests that redirect slog's own default logger (via slog.SetDefault) to
// assert on the log lines a webhook call emits -- mirrors this package's
// own shared-helper convention (postWebhook/newHandlerDeps/
// newGenericLinearGraphQLStub above and below): one definition, used from
// every test file in this package that needs it, instead of each file
// hand-rolling its own capture.
//
// A plain bytes.Buffer/strings.Builder here is NOT safe for concurrent
// use, and postWebhook's own synchronous request-handling goroutine is
// never the only writer once a reply creates a turn: httpapi.
// CreateTurnCore fires the SAME GetOrSpawn+EnsureDispatched post-commit
// dispatch trigger httpapi.TriggerDispatch's own doc comment documents as
// deliberately fire-and-forget (by design, never awaited by the caller --
// GetOrSpawn hydrates fast, but the actual dispatch decision runs
// entirely on the session's Actor's own background goroutine, started by
// Registry.GetOrSpawn via errgroup.Group.Go). That goroutine can still be
// mid-flight -- e.g. logging sessionactor/dispatch.go's own
// "spawn decision would proceed but no SandboxProvider is configured"
// Warn line, since every one of these tests' own newHandlerDeps wires up
// a nil SandboxProvider -- writing through this SAME redirected default
// logger while the test's own goroutine reads back what was captured so
// far. This is a plain unsynchronized-concurrent-access bug on the
// underlying writer itself (ordering is not the issue: every log line
// these tests actually assert on is written synchronously, on the
// request-handling goroutine, before postWebhook returns) -- caught by
// -race in CI run 30887614911 (TestWebhookHandler_Prompted_
// LogsSessionAndTurnID's own "WARNING: DATA RACE", log/slog.
// (*commonHandler).handle -> bytes.Buffer.Write racing that test's own
// logBuf.String() read).
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newHandlerDeps builds a full linear.Deps against pool -- nil sandbox
// provider/commander in the registry (mirroring httpapi_test's own
// newTestRig precedent exactly: these tests only assert that a session/
// turn is correctly PERSISTED and that EnsureDispatched is triggered, not
// what a real spawn/dispatch decision then does with it).
func newHandlerDeps(t *testing.T, pool *pgxpool.Pool) linear.Deps {
	t.Helper()
	ctx := context.Background()

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// prSessions (§31.4): CreateSessionCore's own entitlement gate
	// (checkRepoEntitlementGate, httpapi/repoentitlementgate.go) now
	// requires the fixed DefaultRepoURL below to be known to this
	// deployment (github_pr_sessions) -- Linear always targets that SAME
	// repo, never a per-message choice, so it is made known HERE, once.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	return linear.Deps{
		Pool:               pool,
		Sessions:           narvipg.NewSessionStore(pool),
		Turns:              narvipg.NewTurnStore(pool),
		Environments:       narvipg.NewEnvironmentStore(pool),
		Registry:           registry,
		Deliveries:         narvipg.NewWebhookDeliveryStore(pool),
		AgentSessions:      narvipg.NewLinearAgentSessionStore(pool),
		Installations:      narvipg.NewLinearInstallationStore(pool),
		AuditLog:           narvipg.NewAuditLogStore(pool),
		PRSessions:         prSessions,
		LinearClient:       linearapi.New(nil, "http://127.0.0.1:0"), // never actually called: no installation row exists for this test's organization, so postAcknowledgment skips before any HTTP call.
		WebhookSecret:      []byte(testWebhookSecret),
		TokenEncryptionKey: bytes.Repeat([]byte("k"), 32),
		DefaultRepoName:    "narvi",
		DefaultRepoURL:     "https://github.com/narvidev/narvi",
		Timeouts:           platform.DefaultTimeouts(),
	}
}

// TestWebhookHandler_Created_CreatesSessionAndTurn is this Step's own
// required end-to-end proof: a synthetic, correctly-signed `created`
// AgentSessionEvent webhook results in a real session + turn row in
// Postgres.
func TestWebhookHandler_Created_CreatesSessionAndTurn(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	handler := linear.NewWebhookHandler(deps)

	agentSessionID := "agent-session-e2e-1"
	organizationID := "org-e2e-1"
	body := agentSessionCreatedPayload(agentSessionID, organizationID)

	rec := postWebhook(t, handler, body, "delivery-e2e-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ctx := context.Background()

	var sessionCount int
	var spawnSource, title string
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`,
	).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("session count = %d, want 1", sessionCount)
	}

	var sessionID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, spawn_source::text, title FROM sessions WHERE spawn_source = 'linear'`,
	).Scan(&sessionID, &spawnSource, &title); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if title != "ENG-123: Fix accessibility on checkout page" {
		t.Errorf("session title = %q, want %q", title, "ENG-123: Fix accessibility on checkout page")
	}
	if spawnSource != "linear" {
		t.Errorf("spawn_source = %q, want %q", spawnSource, "linear")
	}

	var turnCount int
	var prompt string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(prompt), '') FROM turns WHERE session_id = $1::uuid`, sessionID,
	).Scan(&turnCount, &prompt); err != nil {
		t.Fatalf("query turns: %v", err)
	}
	if turnCount != 1 {
		t.Fatalf("turn count = %d, want 1", turnCount)
	}
	if prompt == "" {
		t.Error("turn prompt is empty, want the promptContext string")
	}

	// The Linear agent-session mapping itself must now point at this
	// exact session -- the `prompted`-event routing's own read path.
	var mappedSessionID string
	if err := pool.QueryRow(ctx,
		`SELECT session_id::text FROM linear_agent_sessions WHERE agent_session_id = $1`, agentSessionID,
	).Scan(&mappedSessionID); err != nil {
		t.Fatalf("query linear_agent_sessions: %v", err)
	}
	if mappedSessionID != sessionID {
		t.Errorf("linear_agent_sessions.session_id = %q, want %q", mappedSessionID, sessionID)
	}
}

// TestWebhookHandler_Prompted_ForwardsEpistemicCheckDefault is the
// test-wiring bundle's own addition (adversarial review): proves Deps.
// EpistemicCheckDefault (webhook.go) actually reaches handlePrompted's own
// ordinary-reply httpapi.CreateTurnCore call (webhook.go:825, DropIfOpen
// policy) and, through it, createTurnLocked's own epistemic-preamble gate.
// Mirrors planverdict_integration_test.go's own seed-session-directly
// scaffolding (skip resolveOrClaimSession/handleCreated's own "created"
// event entirely -- this test only cares about the ordinary-reply path) --
// a COMPLETED producing turn is seeded first so the reply below is free of
// the open-turn/busy gate, exactly like that file's own identical setup.
// Before this test existed, mutating webhook.go:825's own deps.
// EpistemicCheckDefault argument to a literal false failed nothing.
func TestWebhookHandler_Prompted_ForwardsEpistemicCheckDefault(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := newHandlerDeps(t, pool)
	deps.EpistemicCheckDefault = true
	// Participants (test-wiring bundle addition): newHandlerDeps's own
	// baseline fixture never sets this -- harmless for every EXISTING
	// caller (none reach authorizeSessionAction's own OwnedOrJoined check
	// with a non-nil actor the way this test's linked replier does), but
	// required here: actorauthz.OwnedOrJoined (identity.go) dereferences
	// it unconditionally once actorUserID.Valid.
	deps.Participants = narvipg.NewParticipantStore(pool)

	agentSessionID := "agent-session-epistemic-1"
	organizationID := "org-epistemic-1"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, _ := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const replierID = "linear-epistemic-replier-1"
	linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceLinear})
	if err != nil {
		t.Fatalf("create linear-origin session: %v", err)
	}
	if _, err := agentSessions.Claim(ctx, agentSessionID, organizationID); err != nil {
		t.Fatalf("claim agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, agentSessionID, session.ID); err != nil {
		t.Fatalf("attach session id: %v", err)
	}
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: false})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "also handle the edge case")
	rec := postWebhook(t, handler, body, "delivery-epistemic-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (the seeded producing turn + the new ordinary-reply turn)", len(allTurns))
	}
	var newTurn *sqlcgen.Turn
	for i := range allTurns {
		if allTurns[i].ID != producingTurn.ID {
			newTurn = &allTurns[i]
		}
	}
	if newTurn == nil {
		t.Fatal("no new turn found")
	}
	if newTurn.Prompt == nil {
		t.Fatal("new turn has a nil prompt")
	}
	wantPrefix := turn.RenderEpistemicPreamble()
	if !strings.HasPrefix(*newTurn.Prompt, wantPrefix) {
		t.Errorf("new turn prompt does not start with RenderEpistemicPreamble()'s own text -- Deps.EpistemicCheckDefault was not forwarded to handlePrompted's own CreateTurnCore call\ngot:  %q\nwant prefix: %q", *newTurn.Prompt, wantPrefix)
	}
	if !strings.Contains(*newTurn.Prompt, "also handle the edge case") {
		t.Errorf("new turn prompt lost the caller's own original text\ngot: %q", *newTurn.Prompt)
	}
}

// TestWebhookHandler_FailedFirstAttemptReleasesBothClaimsForRedelivery is
// the H2/H3 audit fix's own headline proof, mirroring github's identical
// TestGitHubIntegration_FailedFirstAttemptReleasesClaimForRedelivery
// (internal/adapters/inbound/github/handler_integration_test.go): neither
// the webhook-delivery claim (H2, the shared toolkit mechanism) NOR the
// SEPARATE linear_agent_sessions claim (H3, this package's own first-
// writer-wins table) may permanently poison a delivery/agent-session
// identity when the first attempt fails AFTER both claims succeed but
// BEFORE the session is actually created -- a redelivery of the SAME
// Linear-Delivery id (Linear's own real retry behavior on a non-2xx
// response or a timeout) must actually reprocess the event, not be
// silently swallowed as an already-claimed duplicate forever.
func TestWebhookHandler_FailedFirstAttemptReleasesBothClaimsForRedelivery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	const agentSessionID = "agent-session-retry-after-failure"
	const organizationID = "org-retry-after-failure"
	const deliveryID = "delivery-retry-after-failure-1"

	// First attempt: DefaultRepoURL is deliberately invalid (fails
	// reposource.ValidateRepoURL's own https-scheme requirement) --
	// CreateSessionCore fails INSIDE handleCreated, AFTER both the
	// webhook-delivery claim and the linear_agent_sessions claim have
	// already succeeded.
	failingDeps := newHandlerDeps(t, pool)
	failingDeps.DefaultRepoURL = "not-a-valid-https-url"
	failingHandler := linear.NewWebhookHandler(failingDeps)

	body := agentSessionCreatedPayload(agentSessionID, organizationID)
	first := postWebhook(t, failingHandler, body, deliveryID)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first (failing) delivery status = %d, want %d; body = %s", first.Code, http.StatusInternalServerError, first.Body.String())
	}

	// H2: the webhook-delivery claim must have been released -- no row
	// left behind poisoning this delivery id.
	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'linear' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries rows after failure: %v", err)
	}
	if deliveryRowCount != 0 {
		t.Fatalf("webhook_deliveries row count after failed attempt = %d, want 0 (H2: the webhook-delivery claim must be released on failure)", deliveryRowCount)
	}

	// H3: the SEPARATE linear_agent_sessions claim must ALSO have been
	// released -- no row left behind with a permanently-NULL session_id.
	var agentSessionRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM linear_agent_sessions WHERE agent_session_id = $1`, agentSessionID,
	).Scan(&agentSessionRowCount); err != nil {
		t.Fatalf("count linear_agent_sessions rows after failure: %v", err)
	}
	if agentSessionRowCount != 0 {
		t.Fatalf("linear_agent_sessions row count after failed attempt = %d, want 0 (H3: the agent-session claim must be released on failure)", agentSessionRowCount)
	}

	// Redelivery: the SAME Linear-Delivery id AND the SAME agentSession.id,
	// this time against a correctly-configured handler. It must be
	// processed, not skipped as an already-claimed duplicate.
	workingDeps := newHandlerDeps(t, pool)
	workingHandler := linear.NewWebhookHandler(workingDeps)

	second := postWebhook(t, workingHandler, body, deliveryID)
	if second.Code != http.StatusOK {
		t.Fatalf("redelivered (valid config) status = %d, want %d; body = %s", second.Code, http.StatusOK, second.Body.String())
	}

	var sessionCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`,
	).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("session count = %d, want exactly 1 (the redelivered valid payload must actually be processed)", sessionCount)
	}

	var mappedSessionID string
	if err := pool.QueryRow(ctx,
		`SELECT session_id::text FROM linear_agent_sessions WHERE agent_session_id = $1`, agentSessionID,
	).Scan(&mappedSessionID); err != nil {
		t.Fatalf("query linear_agent_sessions after redelivery: %v", err)
	}
	if mappedSessionID == "" {
		t.Error("linear_agent_sessions.session_id is empty after redelivery, want the newly-created session's id")
	}
}

// TestWebhookHandler_DuplicateDelivery_NoSecondSession proves the
// webhook_deliveries dedupe layer alone stops a REDELIVERED webhook (the
// SAME Linear-Delivery id) from creating a second session.
func TestWebhookHandler_DuplicateDelivery_NoSecondSession(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	handler := linear.NewWebhookHandler(deps)

	body := agentSessionCreatedPayload("agent-session-dup-delivery", "org-dup-delivery")

	first := postWebhook(t, handler, body, "delivery-dup-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", first.Code, http.StatusOK)
	}
	second := postWebhook(t, handler, body, "delivery-dup-1")
	if second.Code != http.StatusOK {
		t.Fatalf("redelivered status = %d, want %d (ack, not an error)", second.Code, http.StatusOK)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`,
	).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("session count = %d, want 1 (redelivery must not double-create)", count)
	}
}

// TestWebhookHandler_DuplicateAgentSession_DifferentDelivery_NoSecondSession
// proves the SECOND, independent dedupe layer: even a DIFFERENT
// Linear-Delivery id (so the webhook_deliveries claim alone would NOT
// catch it) for the SAME agentSession.id must still never create a
// second Narvi session -- Linear's own AgentSession identity is the real
// source of truth (migrations/000030_linear_agent_sessions.up.sql's own
// doc comment).
func TestWebhookHandler_DuplicateAgentSession_DifferentDelivery_NoSecondSession(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	handler := linear.NewWebhookHandler(deps)

	body := agentSessionCreatedPayload("agent-session-dup-identity", "org-dup-identity")

	first := postWebhook(t, handler, body, "delivery-identity-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", first.Code, http.StatusOK)
	}
	second := postWebhook(t, handler, body, "delivery-identity-2")
	if second.Code != http.StatusOK {
		t.Fatalf("second (different delivery id) status = %d, want %d (ack, not an error)", second.Code, http.StatusOK)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`,
	).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("session count = %d, want 1 (same agent session id must never create a second Narvi session, even via a fresh delivery id)", count)
	}
}

// TestWebhookHandler_TamperedSignature_Rejected proves an invalid
// signature never reaches the claim/processing path at all -- no
// webhook_deliveries row, no session, a plain 401.
func TestWebhookHandler_TamperedSignature_Rejected(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	handler := linear.NewWebhookHandler(deps)

	body := agentSessionCreatedPayload("agent-session-tampered", "org-tampered")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Linear-Event", "AgentSessionEvent")
	req.Header.Set("Linear-Delivery", "delivery-tampered-1")
	req.Header.Set("Linear-Signature", signBody([]byte("wrong-secret"), body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'linear' AND delivery_id = 'delivery-tampered-1'`,
	).Scan(&count); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if count != 0 {
		t.Errorf("webhook_deliveries row count = %d, want 0 (an unauthenticated request must never even reach the claim)", count)
	}
}

// TestWebhookHandler_StaleTimestamp_Rejected proves a correctly-signed
// but stale webhookTimestamp is rejected as a possible replay, per
// Linear's own real recommendation ("within a minute").
func TestWebhookHandler_StaleTimestamp_Rejected(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	handler := linear.NewWebhookHandler(deps)

	staleTimestampMs := time.Now().Add(-10 * time.Minute).UnixMilli()
	body := []byte(fmt.Sprintf(`{
		"action": "created",
		"type": "AgentSessionEvent",
		"organizationId": "org-stale",
		"webhookTimestamp": %d,
		"agentSession": {"id": "agent-session-stale", "url": "https://linear.app/narvi/issue/ENG-1"},
		"promptContext": "stale"
	}`, staleTimestampMs))

	rec := postWebhook(t, handler, body, "delivery-stale-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

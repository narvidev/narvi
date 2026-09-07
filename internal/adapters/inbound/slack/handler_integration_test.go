//go:build integration

// Full HTTP-level integration tests for internal/adapters/inbound/slack's
// POST /webhooks/slack handler, against a real Postgres instance --
// gated behind the "integration" build tag, matching this codebase's own
// testcontainers-Postgres-plus-embedded-migrations convention (each
// DB-touching package builds its own copy of this small helper rather
// than sharing one across package boundaries). Run via `make
// test-integration`.
package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

const testSigningSecret = "test-signing-secret"

// syncLogBuffer is a mutex-guarded io.Writer + String() capture buffer for
// tests that redirect slog's own default logger (via slog.SetDefault) to
// assert on the log lines a handler call emits -- mirrors internal/
// adapters/inbound/linear's own identical syncLogBuffer precedent exactly
// (webhook_integration_test.go there, commit 557a4fa,
// "fix(linear): stop the log-buffer assertion racing the async actor
// spawn"): one definition, used from every test file in this package that
// needs it, instead of each file hand-rolling its own capture.
//
// A plain bytes.Buffer/strings.Builder here is NOT safe for concurrent
// use, and a test's own synchronous request-handling goroutine is never
// the only writer once a reply creates a turn: httpapi.CreateTurnCore
// fires the SAME GetOrSpawn+EnsureDispatched post-commit dispatch trigger
// httpapi.TriggerDispatch's own doc comment documents as deliberately
// fire-and-forget (by design, never awaited by the caller -- GetOrSpawn
// hydrates fast, but the actual dispatch decision runs entirely on the
// session's Actor's own background goroutine, started by Registry.
// GetOrSpawn via errgroup.Group.Go). That goroutine can still be
// mid-flight -- e.g. logging sessionactor/dispatch.go's own decision Warn
// line -- writing through this SAME redirected default logger while the
// test's own goroutine reads back what was captured so far. This is a
// plain unsynchronized-concurrent-access bug on the underlying writer
// itself (ordering is not the issue: every log line these tests actually
// assert on is written synchronously, on the request-handling goroutine,
// before the handler call returns) -- caught directly here, in this
// package, by this branch's own -race verification of the container-
// reuse change (TestHandler_RevisePrefix_NonEmptyFeedback_
// LogsPlanModeTrueOnSuccess's own "WARNING: DATA RACE", log/slog.
// (*commonHandler).handle -> bytes.Buffer.Write racing that test's own
// logBuf.String() read) -- pre-existing and unrelated to container reuse
// itself (the same race is equally possible against a fresh per-test
// container; this package's own tests just hadn't been run under -race
// enough times before to hit it).
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

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go), not freshly per test/container as this function
// used to do itself. Kept as a thin wrapper under its own original
// name/signature so every existing call site in this package's own
// *_integration_test.go files keeps compiling unchanged. See sharedpool_
// integration_test.go's own top doc comment for the full container-reuse
// story: why per-test containers were never a deliberate correctness
// requirement here, why sharing one is safe against this package's own
// async sessionactor.Actor background work, and why each test still gets
// a byte-for-byte-empty (plus restored seed data), freshly-migrated-
// equivalent database via a t.Cleanup-registered reset rather than a
// real fresh container.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return IntegrationTestPool(t)
}

// slackTestRig bundles a fully-wired handler plus the stores/registry
// needed to assert against Postgres directly.
type slackTestRig struct {
	handler  http.HandlerFunc
	pool     *pgxpool.Pool
	sessions *narvipg.SessionStore
	turns    *narvipg.TurnStore
	threads  *narvipg.SlackThreadSessionStore
	plans    *narvipg.PlanStore
}

// linkSlackIdentityForTest links slackUserID directly to a NEW fixture
// Narvi user (role) via identities.Create -- bypassing any profile-email
// fetch entirely: identitylink.Resolve's own fast path
// (GetByProviderAndExternalID) always wins first regardless of what any
// fake Slack /users.info stub answers, so this package's many baseline
// HTTP-level tests (which only care about session/turn/audit-log/redelivery
// mechanics, never about the auto-linking algorithm itself) can exercise a
// genuinely LINKED, authorized actor.
//
// Audit-fix batch addition ("block unlinked actor state changes"): BEFORE
// this batch, every one of this package's baseline tests relied
// (incidentally, never as their own point) on the OLD "an actor that never
// resolves at all still gets its state-changing action allowed through,
// under bot attribution" precedent -- resolveSlackActor's own
// GetUserEmail call against these tests' generic {"ok":true}-only fake
// Slack servers never found a real email, so EVERY fixture event's own
// actor was, and stayed, unresolved. That precedent is exactly what this
// batch's own hardening removes (actorauthz.AuthorizeLinkedActor), so every
// baseline test's own fixture Slack user id must now be pre-linked here
// instead, to a role with unconditional (no ownership-carve-out-dependent)
// permission for every action these tests exercise.
func linkSlackIdentityForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slackUserID string, role sqlcgen.UserRole) sqlcgen.User {
	t.Helper()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: slackUserID + "@narvi-test.example.com",
		DisplayName:  slackUserID,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("create fixture user for %s: %v", slackUserID, err)
	}
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     user.ID,
		Provider:   sqlcgen.IdentityProviderSlack,
		ExternalID: slackUserID,
		LinkedVia:  sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("link fixture identity for %s: %v", slackUserID, err)
	}
	return user
}

// newSlackTestRig wires a real handler against pool -- a fake Slack Web
// API server (ackServer) stands in for chat.postMessage, so this
// package's own tests never make a real network call.
func newSlackTestRig(t *testing.T, pool *pgxpool.Pool) *slackTestRig {
	t.Helper()
	return newSlackTestRigWithEpistemicCheckDefault(t, pool, false)
}

// newSlackTestRigWithEpistemicCheckDefault is newSlackTestRig's own
// generalization (test-wiring bundle, adversarial review): every ORIGINAL
// caller keeps going through newSlackTestRig above (epistemicCheckDefault
// hardcoded false, preserving every existing test's own behavior
// byte-for-byte); TestHandler_OrdinaryReply_ForwardsEpistemicCheckDefault
// below is this parameter's own one real user, proving Deps.
// EpistemicCheckDefault (handler.go) actually reaches addTurn (handler.go:725)
// and, through it, createTurnLocked's own epistemic-preamble gate -- before
// this existed, nothing in this package proved the value was forwarded at
// all; mutating it to a literal false at handler.go:725 failed nothing.
func newSlackTestRigWithEpistemicCheckDefault(t *testing.T, pool *pgxpool.Pool, epistemicCheckDefault bool) *slackTestRig {
	t.Helper()
	ctx := context.Background()

	// Audit-fix batch addition: appMentionEnvelope/messageEnvelope's own
	// fixed "U0TESTUSER"/"U0OTHERUSER" ids (below) must now resolve to a
	// genuinely LINKED, sufficiently-privileged actor -- see
	// linkSlackIdentityForTest's own doc comment above for why. RoleMaintainer
	// is allowed unconditionally for every action this package's baseline
	// tests exercise (ActionCreateSession/ActionPromptSession/
	// ActionApprovePlan), regardless of session ownership.
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)

	ackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ackServer.Close)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)
	plans := narvipg.NewPlanStore(pool)
	events := narvipg.NewEventStore(pool)
	planDocuments := narvipg.NewPlanDocumentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// prSessions (§31.4): CreateSessionCore's own entitlement gate
	// (checkRepoEntitlementGate, httpapi/repoentitlementgate.go) now
	// requires the fixed DefaultRepoURL below to be known to this
	// deployment (github_pr_sessions) -- Slack always targets that SAME
	// repo, never a per-message choice, so it is made known HERE, once,
	// exactly like every other REQUIRED-but-otherwise-uninteresting
	// dependency this rig already constructs unconditionally.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	handler := slack.NewHandler(slack.Deps{
		Pool:          pool,
		Sessions:      sessions,
		Turns:         turns,
		Environments:  environments,
		Registry:      registry,
		Deliveries:    deliveries,
		Threads:       threads,
		Plans:         plans,
		PRSessions:    prSessions,
		Events:        events,
		PlanDocuments: planDocuments,
		AuditLog:      auditLog,
		// Participants (§13.2's own SECOND fix-pass addition, "identities
		// + full RBAC", §13.2/§13.3): authorizeSessionAction (identity.go)
		// needs this even though this rig's own fixture users never
		// auto-link (see this func's own doc comment) -- mirrors every
		// other Deps field here, always a real, non-nil store.
		Participants:    narvipg.NewParticipantStore(pool),
		SigningSecret:   testSigningSecret,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		// IdentityLink/SlackClient/Timeouts ("identities + full
		// RBAC", §13.2): ackServer above answers EVERY path (including
		// /users.info) with a bare {"ok":true}, so GetUserEmail resolves
		// to (email="", ok=false) for every fixture event's own "user" id
		// -- resolveSlackActor's own identity.Resolve call still needs
		// REAL (non-nil) stores to run against, even though this rig's
		// own fixture users never match anything (see
		// identity_integration_test.go for a rig that actually exercises
		// a real match).
		SlackClient: slackapi.New(ackServer.Client(), ackServer.URL, "test-bot-token"),
		Timeouts:    platform.DefaultTimeouts(),
		// EpistemicCheckDefault (test-wiring bundle, adversarial review):
		// every ORIGINAL caller (newSlackTestRig) gets false here, exactly
		// as before this parameter existed -- see this function's own doc
		// comment.
		EpistemicCheckDefault: epistemicCheckDefault,
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

	return &slackTestRig{handler: handler, pool: pool, sessions: sessions, turns: turns, threads: threads, plans: plans}
}

// signedSlackRequest builds a real, correctly-signed POST /webhooks/slack
// request carrying body -- mirrors Slack's own real "v0:{ts}:{body}"
// HMAC-SHA256 scheme exactly (see handler_test.go's own identical
// signRequest, duplicated here since this file lives in the external
// slack_test package).
func signedSlackRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	ts := time.Now().Unix()
	signedPayload := "v0:" + strconv.FormatInt(ts, 10) + ":" + body
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(signedPayload))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Slack-Signature", sig)
	return req
}

// appMentionEnvelope builds a real-shaped "event_callback"/"app_mention"
// Slack Events API envelope (confirmed against Slack's own current
// documentation at this Step's own design time).
func appMentionEnvelope(eventID, channel, ts, threadTS, text string) string {
	event := map[string]string{
		"type":    "app_mention",
		"channel": channel,
		"user":    "U0TESTUSER",
		"text":    text,
		"ts":      ts,
	}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	eventJSON, _ := json.Marshal(event)
	envelope := fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":"T0TEST","event":%s}`, eventID, eventJSON)
	return envelope
}

// messageEnvelope builds a real-shaped "event_callback"/"message" envelope
// for a plain reply within an existing thread.
func messageEnvelope(eventID, channel, ts, threadTS, text string) string {
	event := map[string]string{
		"type":      "message",
		"channel":   channel,
		"user":      "U0OTHERUSER",
		"text":      text,
		"ts":        ts,
		"thread_ts": threadTS,
	}
	eventJSON, _ := json.Marshal(event)
	envelope := fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":"T0TEST","event":%s}`, eventID, eventJSON)
	return envelope
}

// TestHandler_NewMention_CreatesSessionAndTurn proves a synthetic,
// correctly-signed app_mention event with no existing thread mapping
// results in a real session + turn in Postgres, and a slack_thread_sessions
// mapping row.
func TestHandler_NewMention_CreatesSessionAndTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackTestRig(t, pool)

	envelope := appMentionEnvelope("Ev0NEWTHREAD001", "C0CHANNEL", "1700000010.000100", "", "<@U0BOT> please *fix* the build")

	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0CHANNEL", "1700000010.000100")
	if err != nil {
		t.Fatalf("expected a thread mapping row, Get: %v", err)
	}

	session, err := rig.sessions.Get(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if session.SpawnSource != sqlcgen.SessionSpawnSourceSlack {
		t.Errorf("SpawnSource = %q, want %q", session.SpawnSource, sqlcgen.SessionSpawnSourceSlack)
	}

	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1", len(turns))
	}
	if turns[0].Prompt == nil || !strings.Contains(*turns[0].Prompt, "**fix**") {
		t.Errorf("turn prompt = %v, want normalized mrkdwn containing **fix**", turns[0].Prompt)
	}
	if turns[0].Prompt == nil || !strings.Contains(*turns[0].Prompt, "@U0BOT") {
		t.Errorf("turn prompt = %v, want the unwrapped @U0BOT mention", turns[0].Prompt)
	}
}

// TestHandler_OrdinaryReply_ForwardsEpistemicCheckDefault is the
// test-wiring bundle's own addition (adversarial review): proves Deps.
// EpistemicCheckDefault (handler.go) actually reaches addTurn
// (handler.go:725) and, through it, createTurnLocked's own epistemic-
// preamble gate -- an ordinary reply in an existing thread (never itself a
// new-session/new-turn edge case) is deliberately the scenario under test,
// since this package's own resolveOrClaimSession creates every brand-new
// session BARE (no Prompt at all, handler.go:990-995) -- addTurn is the
// ONE call that ever carries real prompt text on ANY Slack-originated
// turn, first message or reply alike. Before this test existed, mutating
// handler.go:725's own deps.EpistemicCheckDefault argument to a literal
// false failed nothing.
func TestHandler_OrdinaryReply_ForwardsEpistemicCheckDefault(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackTestRigWithEpistemicCheckDefault(t, pool, true)

	// First message creates the session/thread mapping (the bare-session
	// path, no prompt injection possible there at all -- see this test's
	// own doc comment); the SECOND, a plain reply in the same thread, is
	// what actually exercises addTurn/createTurnLocked's own preamble gate.
	first := appMentionEnvelope("Ev0EPI001", "C0EPI", "1700000030.000100", "", "please build the thing")
	rec1 := httptest.NewRecorder()
	rig.handler(rec1, signedSlackRequest(t, first))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first event status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0EPI", "1700000030.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	firstTurns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil || len(firstTurns) != 1 {
		t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
	}
	// Drive the first turn terminal so the reply below is free to add a
	// second one, rather than being dropped by the open-turn/busy gate --
	// mirrors TestHandler_ReplyOnMappedThread_AddsTurnToSameSession's own
	// identical setup (this file, below).
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: firstTurns[0].ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	reply := messageEnvelope("Ev0EPI002", "C0EPI", "1700000031.000100", "1700000030.000100", "actually also do this")
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, signedSlackRequest(t, reply))
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply event status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (the first message, then the reply)", len(turns))
	}
	// turns is ordered oldest-first (ListForSession's own doc comment) --
	// turns[1] is the reply, the one addTurn (handler.go:725) created.
	replyTurn := turns[1]
	if replyTurn.Prompt == nil {
		t.Fatal("reply turn has a nil prompt")
	}
	wantPrefix := turn.RenderEpistemicPreamble()
	if !strings.HasPrefix(*replyTurn.Prompt, wantPrefix) {
		t.Errorf("reply turn prompt does not start with RenderEpistemicPreamble()'s own text -- Deps.EpistemicCheckDefault was not forwarded to addTurn/createTurnLocked\ngot:  %q\nwant prefix: %q", *replyTurn.Prompt, wantPrefix)
	}
	if !strings.Contains(*replyTurn.Prompt, "actually also do this") {
		t.Errorf("reply turn prompt lost the caller's own original text\ngot: %q", *replyTurn.Prompt)
	}
}

// TestHandler_Redelivery_DoesNotDoubleProcess proves WebhookDeliveryStore's
// dedupe claim actually protects this handler end to end: POSTing the
// exact same signed event_id twice results in exactly one session/turn,
// never two.
func TestHandler_Redelivery_DoesNotDoubleProcess(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackTestRig(t, pool)

	envelope := appMentionEnvelope("Ev0REDELIVER001", "C0REDELIVER", "1700000020.000100", "", "please help")

	for i := 0; i < 2; i++ {
		req := signedSlackRequest(t, envelope)
		rec := httptest.NewRecorder()
		rig.handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}

	mapping, err := rig.threads.Get(ctx, "C0REDELIVER", "1700000020.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}

	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("len(turns) = %d, want exactly 1 (redelivery must not double-process)", len(turns))
	}

	var mappingCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_thread_sessions WHERE channel_id = $1 AND thread_ts = $2`,
		"C0REDELIVER", "1700000020.000100",
	).Scan(&mappingCount); err != nil {
		t.Fatalf("count mapping rows: %v", err)
	}
	if mappingCount != 1 {
		t.Errorf("mapping row count = %d, want exactly 1", mappingCount)
	}
}

// TestHandler_FailedFirstAttemptReleasesClaimForRedelivery is the H2
// audit fix's own headline proof, mirroring github's own identical
// TestGitHubIntegration_FailedFirstAttemptReleasesClaimForRedelivery
// (handler_integration_test.go): the webhook-delivery dedupe claim must
// NOT permanently poison an event_id when the first attempt fails AFTER
// the claim succeeds but BEFORE the event is actually processed --
// Slack's own real redelivery behavior (retrying on a non-2xx response or
// a timeout) means the SAME event_id must be reprocessable, not silently
// swallowed as an "already claimed" duplicate forever.
func TestHandler_FailedFirstAttemptReleasesClaimForRedelivery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackTestRig(t, pool)

	const eventID = "Ev0RETRYAFTERFAIL001"

	// First attempt: the outer envelope is well-formed (so it gets past
	// url_verification detection and claims the delivery), but the inner
	// "event" field is a JSON string, not the object slackEvent requires --
	// json.Unmarshal into ev fails AFTER the claim has already succeeded.
	malformedEnvelope := fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":"T0TEST","event":"not-an-object"}`, eventID)
	req := signedSlackRequest(t, malformedEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("first (malformed) delivery status = %d, want %d (body=%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	// The claim row must have been released by the failure path, not left
	// behind poisoning this event_id.
	var deliveryRowCountAfterFailure int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack' AND delivery_id = $1`, eventID,
	).Scan(&deliveryRowCountAfterFailure); err != nil {
		t.Fatalf("count webhook_deliveries rows after failure: %v", err)
	}
	if deliveryRowCountAfterFailure != 0 {
		t.Fatalf("webhook_deliveries row count after failed attempt = %d, want 0 (claim must be released on failure)", deliveryRowCountAfterFailure)
	}

	// Redelivery: the SAME event_id, this time a genuine, well-formed
	// app_mention payload. It must be processed, not skipped as an
	// already-claimed duplicate.
	validEnvelope := appMentionEnvelope(eventID, "C0RETRYAFTERFAIL", "1700000040.000100", "", "please help again")
	req2 := signedSlackRequest(t, validEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("redelivered (valid) status = %d, want %d (body=%s)", rec2.Code, http.StatusOK, rec2.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0RETRYAFTERFAIL", "1700000040.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}

	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("len(turns) = %d, want exactly 1 (the redelivered valid payload must actually be processed)", len(turns))
	}
}

// TestHandler_ReplyOnMappedThread_AddsTurnToSameSession proves the core
// thread<->session mapping requirement: a first message in a NEW thread
// creates a session, and a second message with the SAME (channel_id,
// thread_ts) creates a NEW TURN on the SAME session -- never a second
// session. The first turn is driven to a terminal state directly (this
// test environment's registry has no real sandbox provider wired, so
// nothing would ever organically complete it) purely to prove the
// mapping/turn-add logic in isolation, mirroring this codebase's own
// precedent (dispatch.go's own tests) of asserting persistence-layer
// outcomes directly rather than depending on a real end-to-end dispatch.
func TestHandler_ReplyOnMappedThread_AddsTurnToSameSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackTestRig(t, pool)

	firstEnvelope := appMentionEnvelope("Ev0THREAD001", "C0THREAD", "1700000030.000100", "", "start this task")
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, "C0THREAD", "1700000030.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	firstSessionID := mapping.SessionID

	firstTurns, err := rig.turns.ListForSession(ctx, firstSessionID)
	if err != nil || len(firstTurns) != 1 {
		t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
	}

	// Drive the first turn to a terminal state so the reply below is free
	// to add a second one (see this test's own doc comment).
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          firstTurns[0].ID,
		Status:      sqlcgen.TurnStatusCompleted,
		CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	replyEnvelope := messageEnvelope("Ev0THREAD002", "C0THREAD", "1700000031.000200", "1700000030.000100", "here is more context")
	req2 := signedSlackRequest(t, replyEnvelope)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	// Still exactly ONE mapping row for this thread -- a reply must never
	// create a second session/mapping.
	var mappingCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_thread_sessions WHERE channel_id = $1 AND thread_ts = $2`,
		"C0THREAD", "1700000030.000100",
	).Scan(&mappingCount); err != nil {
		t.Fatalf("count mapping rows: %v", err)
	}
	if mappingCount != 1 {
		t.Errorf("mapping row count = %d, want exactly 1 (reply must reuse the existing mapping)", mappingCount)
	}

	finalTurns, err := rig.turns.ListForSession(ctx, firstSessionID)
	if err != nil {
		t.Fatalf("ListForSession after reply: %v", err)
	}
	if len(finalTurns) != 2 {
		t.Fatalf("len(turns) after reply = %d, want 2 (the reply must add a NEW turn on the SAME session)", len(finalTurns))
	}
}

// TestHandler_URLVerification_DoesNotProcessAsEvent proves the
// url_verification handshake is handled distinctly from a real event:
// it echoes the challenge and never touches WebhookDeliveryStore/creates
// any session at all.
func TestHandler_URLVerification_DoesNotProcessAsEvent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackTestRig(t, pool)

	body := `{"type":"url_verification","challenge":"test-challenge-value","token":"xyz"}`
	req := signedSlackRequest(t, body)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "test-challenge-value") {
		t.Errorf("body = %q, want it to contain the echoed challenge", rec.Body.String())
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (url_verification must never create a session)", sessionCount)
	}
}

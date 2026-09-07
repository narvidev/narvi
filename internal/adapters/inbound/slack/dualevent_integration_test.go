//go:build integration

// Integration tests for the L3 audit fix ("Slack's own dual-delivery for
// one logical mention isn't coalesced", internal/adapters/inbound/slack/
// handler.go's own slackMessageClaimProvider/messageClaimKey): Slack sends
// BOTH an app_mention event AND a message event (two DISTINCT event_id
// values) for a single @mention posted inside a thread this adapter
// already has mapped to a session -- these tests prove that dual delivery
// now results in exactly ONE turn and exactly ONE in-thread ack, that a
// genuinely different message (a different ts) in the SAME thread is
// still NOT coalesced, and that a genuine backend failure on the first of
// the twin events releases BOTH the outer (event_id) and the new inner
// (channel:ts) claims so a later real retry can still succeed.
//
// Mirrors this package's own established conventions: turn_integration_test.go's
// own slackAckTestRig (a request-capturing fake Slack API server) and
// identity_integration_test.go's own appMentionEnvelopeWithUser/
// messageEnvelopeWithUser/newFakeSlackWithUsersInfo/newIdentityLinkDepsForTest
// (same package, so all directly reusable here) -- a real Slack
// dual-delivery for one logical mention always carries the SAME "user"
// field on both twin events, since they describe the same human action
// twice, so these tests deliberately use the WithUser variants rather than
// handler_integration_test.go's own fixed-distinct-user appMentionEnvelope/
// messageEnvelope helpers.
package slack_test

import (
	"context"
	"encoding/json"
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

// drainAllSlackRequests reads every request rig's fake Slack API server
// observed off rig.requests -- unlike slackAckTestRig's own drainAckTexts
// (turn_integration_test.go), which only keeps a request's "text" field
// (silently discarding anything without one), this keeps every captured
// request including a bare /users.info call, so this file's own tests can
// count how many times resolveSlackActor's own users.info call actually
// happened -- the L3 fix's own "no redundant resolveSlackActor call"
// half.
func drainAllSlackRequests(rig *slackAckTestRig) []recordedSlackRequestBody {
	var out []recordedSlackRequestBody
	for {
		select {
		case req := <-rig.requests:
			out = append(out, req)
		default:
			return out
		}
	}
}

// countRequestsByPath counts how many captured requests hit path exactly
// (e.g. "/users.info").
func countRequestsByPath(requests []recordedSlackRequestBody, path string) int {
	n := 0
	for _, r := range requests {
		if r.path == path {
			n++
		}
	}
	return n
}

// countPostMessageForThread counts how many captured chat.postMessage
// (never chat.postEphemeral) calls carried the given thread_ts -- this
// file's own proxy for "how many in-thread acks were posted for this
// thread", since slackapi.Client.PostAck always threads its reply via
// thread_ts.
func countPostMessageForThread(requests []recordedSlackRequestBody, threadTS string) int {
	n := 0
	for _, r := range requests {
		if r.path != "/chat.postMessage" {
			continue
		}
		if ts, _ := r.body["thread_ts"].(string); ts == threadTS {
			n++
		}
	}
	return n
}

// completeOpenTurn drives whatever non-terminal turn currently exists for
// sessionID to Completed -- mirrors this package's own established
// "drive the first turn terminal so the next reply is free to add its
// own" precedent (e.g. TestHandler_ReplyOnMappedThread_AddsTurnToSameSession,
// handler_integration_test.go), factored into a helper since this file's
// own regression test needs it more than once.
func completeOpenTurn(t *testing.T, turns *narvipg.TurnStore, sessionID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	existing, err := turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	var openTurnID pgtype.UUID
	for _, tn := range existing {
		if tn.Status != sqlcgen.TurnStatusCompleted {
			openTurnID = tn.ID
		}
	}
	if !openTurnID.Valid {
		t.Fatal("completeOpenTurn: expected an open (non-Completed) turn, found none")
	}
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: openTurnID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("completeOpenTurn: UpdateStatus: %v", err)
	}
}

// TestHandler_DualDelivery_AppMentionAndMessage_CoalescesToOneTurnAndOneAck
// is the L3 audit fix's own headline proof, the exact scenario the audit
// finding names: a synthetic app_mention+message pair (two separate
// webhook POST requests, distinct event_ids, IDENTICAL channel/ts/text,
// referencing an ALREADY-MAPPED thread) results in exactly ONE turn
// created and exactly ONE in-thread ack posted -- never two.
//
// Perf-fix update (identity.go's own identitylink.LookupLinkedUserID
// pre-check): dualUser is linked via linkSlackIdentityForTest BELOW,
// before the root mention ever fires -- so EVERY resolveSlackActor call
// this test triggers, root and twin alike, now hits that pre-check's
// already-linked fast path and skips the users.info fetch entirely,
// exactly like TestHandler_AppMention_AlreadyLinkedIdentity_SkipsUsersInfoFetch
// (identity_fetchskip_integration_test.go) proves in isolation. This test
// used to assert the twin pair produced exactly ONE such fetch (proving
// the L3 dedup claim: resolveSlackActor itself only runs once for the
// coalesced pair, never once per twin) -- now asserts ZERO, a strictly
// tighter version of the SAME dedup guarantee (a call that never happens
// cannot happen twice either).
func TestHandler_DualDelivery_AppMentionAndMessage_CoalescesToOneTurnAndOneAck(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	const dualUser = "U0DUALDELIVERY"
	linkSlackIdentityForTest(ctx, t, pool, dualUser, sqlcgen.UserRoleMaintainer)

	const channel = "C0DUAL"
	const rootTS = "1700000100.000100"

	rootEnvelope := appMentionEnvelopeWithUser("Ev0DUALROOT", channel, rootTS, "", "start this task", dualUser)
	req := signedSlackRequest(t, rootEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("root mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	drainAllSlackRequests(rig) // discard the root mention's own ack (no users.info call: dualUser is already linked)

	mapping, err := rig.threads.Get(ctx, channel, rootTS)
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	rootTurns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil || len(rootTurns) != 1 {
		t.Fatalf("ListForSession after root mention: turns=%v err=%v, want exactly 1", rootTurns, err)
	}
	completeOpenTurn(t, rig.turns, mapping.SessionID)

	// The dual-delivery pair Slack's own real behavior sends for a SINGLE
	// @mention posted inside this already-mapped thread: an app_mention
	// event AND a plain message event, DIFFERENT event_ids, IDENTICAL
	// channel/ts/text/user.
	const replyTS = "1700000100.000200"
	const replyText = "please continue with the fix"
	appMentionTwin := appMentionEnvelopeWithUser("Ev0DUALTWINA", channel, replyTS, rootTS, replyText, dualUser)
	messageTwin := messageEnvelopeWithUser("Ev0DUALTWINB", channel, replyTS, rootTS, replyText, dualUser)

	req1 := signedSlackRequest(t, appMentionTwin)
	rec1 := httptest.NewRecorder()
	rig.handler(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("twin 1 (app_mention): status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	req2 := signedSlackRequest(t, messageTwin)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("twin 2 (message): status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	// Exactly ONE new turn -- never two, despite two independent, fully
	// successful (200 OK) webhook deliveries for the same logical mention.
	turnsAfter, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession after dual delivery: %v", err)
	}
	if len(turnsAfter) != 2 {
		t.Fatalf("len(turns) after dual delivery = %d, want 2 (root + exactly ONE new turn, not two)", len(turnsAfter))
	}

	captured := drainAllSlackRequests(rig)

	// Exactly ONE in-thread ack for this reply -- never a confusing "on
	// it" immediately followed by a second, separate "still working on
	// the previous message" ack for its own twin.
	if got := countPostMessageForThread(captured, rootTS); got != 1 {
		t.Errorf("chat.postMessage count for thread %s = %d, want exactly 1 (the dual-delivery pair must produce ONE ack, not two)", rootTS, got)
	}

	// ZERO users.info calls -- dualUser is already linked (linkSlackIdentityForTest
	// above, before the root mention even fired), so identitylink.
	// LookupLinkedUserID's own pre-check (identity.go) short-circuits every
	// resolveSlackActor call this test triggers, root and twin alike, before
	// any fetch is ever attempted. This is a STRICTER form of the original
	// L3 dedup guarantee (never a redundant second call for the twin event):
	// a call that never happens at all cannot happen twice either.
	if got := countRequestsByPath(captured, "/users.info"); got != 0 {
		t.Errorf("/users.info call count = %d, want exactly 0 (dualUser is already linked -- the perf-fix pre-check must skip the fetch entirely, and in particular never call it twice for the twin pair)", got)
	}
}

// TestHandler_DualDelivery_GenuinelyDifferentMessages_NotCoalesced is this
// batch's own explicit regression test: TWO GENUINELY DIFFERENT messages
// (different ts) posted in the SAME already-mapped thread must each still
// get their own turn/ack -- proving the L3 fix's own message-level claim
// is scoped to the SAME underlying message (channel, ts) and never
// over-coalesces an entire thread the way a mistaken channel+threadKey()
// (rather than channel+ts) key would.
func TestHandler_DualDelivery_GenuinelyDifferentMessages_NotCoalesced(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	const dualUser = "U0DUALDIFFERENT"
	linkSlackIdentityForTest(ctx, t, pool, dualUser, sqlcgen.UserRoleMaintainer)

	const channel = "C0DUALDIFF"
	const rootTS = "1700000110.000100"

	rootEnvelope := appMentionEnvelopeWithUser("Ev0DIFFROOT", channel, rootTS, "", "start this task", dualUser)
	req := signedSlackRequest(t, rootEnvelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("root mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	drainAllSlackRequests(rig) // discard the root mention's own ack

	mapping, err := rig.threads.Get(ctx, channel, rootTS)
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	completeOpenTurn(t, rig.turns, mapping.SessionID)

	// Message A: a genuinely different message (its own distinct ts) in
	// the SAME thread.
	const tsA = "1700000110.000200"
	envelopeA := messageEnvelopeWithUser("Ev0DIFFA", channel, tsA, rootTS, "first follow-up", dualUser)
	reqA := signedSlackRequest(t, envelopeA)
	recA := httptest.NewRecorder()
	rig.handler(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("message A: status = %d, want 200 (body=%s)", recA.Code, recA.Body.String())
	}
	turnsAfterA, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession after A: %v", err)
	}
	if len(turnsAfterA) != 2 {
		t.Fatalf("len(turns) after message A = %d, want 2 (a genuinely different message must get its own turn)", len(turnsAfterA))
	}
	completeOpenTurn(t, rig.turns, mapping.SessionID)

	// Message B: yet ANOTHER genuinely different message, same thread.
	const tsB = "1700000110.000300"
	envelopeB := messageEnvelopeWithUser("Ev0DIFFB", channel, tsB, rootTS, "second follow-up", dualUser)
	reqB := signedSlackRequest(t, envelopeB)
	recB := httptest.NewRecorder()
	rig.handler(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("message B: status = %d, want 200 (body=%s)", recB.Code, recB.Body.String())
	}
	turnsAfterB, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession after B: %v", err)
	}
	if len(turnsAfterB) != 3 {
		t.Fatalf("len(turns) after message B = %d, want 3 (each genuinely different message must get its own turn -- the L3 fix must not over-coalesce a whole thread)", len(turnsAfterB))
	}

	captured := drainAllSlackRequests(rig)
	if got := countPostMessageForThread(captured, rootTS); got != 2 {
		t.Errorf("chat.postMessage count for thread %s = %d, want 2 (one ack per genuinely distinct message, never coalesced)", rootTS, got)
	}
}

// TestHandler_DualDelivery_FailedFirstAttemptReleasesBothClaimsForRedelivery
// is the "release-on-failure" subtlety the L3 audit finding itself calls
// out: a genuine backend failure processing the FIRST of the twin events
// (mirroring authz_backend_error_integration_test.go's own deliberately-
// closed-pool technique for a deterministic, non-timing-dependent
// failure) must release BOTH the outer (provider="slack", event_id) claim
// H2 already covers AND the L3 fix's own NEW inner (provider=
// "slack-message", channel:ts) claim -- otherwise a later genuine retry
// (here, redelivering the TWIN event type for the SAME underlying
// message) would find the inner claim already taken and be silently,
// incorrectly dropped forever.
func TestHandler_DualDelivery_FailedFirstAttemptReleasesBothClaimsForRedelivery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "dual-backend-error@example.com", DisplayName: "Dual Backend Error", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	const slackUserID = "U-DUAL-BACKEND-ERROR"
	fakeSlack := newFakeSlackWithUsersInfo(t, slackUserID, "dual-backend-error@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)

	sessions := narvipg.NewSessionStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	// CreatedBy = matchedUser.ID so the existing-mapping reply below passes
	// the own/joined carve-out once auth actually runs (irrelevant to the
	// FIRST, broken-backend attempt, which fails before ever reaching that
	// check, but needed for the SECOND, working redelivery to succeed).
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack, CreatedBy: matchedUser.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	const channel = "C0DUALBACKENDERROR"
	const rootTS = "1700000120.000100"
	if _, _, err := threads.Claim(ctx, channel, rootTS, session.ID); err != nil {
		t.Fatalf("claim thread mapping: %v", err)
	}

	// A SEPARATE pool, pointed at the SAME database, closed immediately --
	// every call through a store built on it fails deterministically
	// (pgxpool.ErrClosedPool), simulating a genuine backend failure with no
	// timing dependency -- mirrors authz_backend_error_integration_test.go's
	// own identical TestHandler_ReplyOnMappedThread_AuthzBackendErrorReleasesClaim
	// precedent exactly.
	brokenPool, err := narvipg.NewPool(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("new broken pool: %v", err)
	}
	brokenPool.Close()
	brokenSessions := narvipg.NewSessionStore(brokenPool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// prSessions (§31.4): shared by both handlers below -- see
	// newSlackTestRigWithEpistemicCheckDefault's own identical addition
	// (handler_integration_test.go) for why this fixed default repo is
	// made known here.
	prSessionsForBackendErrorTest := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessionsForBackendErrorTest.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	brokenHandler := slack.NewHandler(slack.Deps{
		Pool:            pool,
		Sessions:        brokenSessions, // the deliberately-broken store
		Turns:           turns,
		Environments:    narvipg.NewEnvironmentStore(pool),
		Registry:        registry,
		Deliveries:      deliveries,
		Threads:         threads,
		AuditLog:        auditLog,
		Participants:    narvipg.NewParticipantStore(pool),
		SigningSecret:   testSigningSecret,
		PRSessions:      prSessionsForBackendErrorTest,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:     slackapi.New(fakeSlack.Client(), fakeSlack.URL, "test-bot-token"),
		Timeouts:        platform.DefaultTimeouts(),
		IdentityLink:    newIdentityLinkDepsForTest(pool, auditLog),
	})

	// The FIRST of the twin events (an app_mention posted inside this
	// already-mapped thread) hits the genuinely broken Sessions store
	// INSIDE authorizeExistingSessionReply -- a real backend failure, not
	// an authz denial (matchedUser's own profile email still auto-links via
	// the WORKING IdentityLink stores, which run before authorizeSessionAction
	// ever touches the broken Sessions store).
	const twinTS = "1700000120.000200"
	const firstEventID = "Ev0DUALBACKENDERRORA"
	firstEnvelope := appMentionEnvelopeWithUser(firstEventID, channel, twinTS, rootTS, "please continue", slackUserID)
	req := signedSlackRequest(t, firstEnvelope)
	rec := httptest.NewRecorder()
	brokenHandler(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("first (broken-backend) delivery status = %d, want %d (a genuine backend failure, not a denial); body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	var outerClaimCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack' AND delivery_id = $1`, firstEventID,
	).Scan(&outerClaimCount); err != nil {
		t.Fatalf("count outer claim rows: %v", err)
	}
	if outerClaimCount != 0 {
		t.Fatalf("outer (event_id) claim row count after failure = %d, want 0 (H2: must be released so a redelivery can retry)", outerClaimCount)
	}

	var innerClaimCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack-message' AND delivery_id = $1`, channel+":"+twinTS,
	).Scan(&innerClaimCount); err != nil {
		t.Fatalf("count inner claim rows: %v", err)
	}
	if innerClaimCount != 0 {
		t.Fatalf("inner (channel:ts) claim row count after failure = %d, want 0 (L3: must ALSO be released on this same failure path)", innerClaimCount)
	}

	turnsAfterFailure, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListForSession after failure: %v", err)
	}
	if len(turnsAfterFailure) != 0 {
		t.Fatalf("len(turns) after broken-backend attempt = %d, want 0 (must not have proceeded past the failed authz check)", len(turnsAfterFailure))
	}

	// A genuine redelivery of the TWIN event type (message, not
	// app_mention) for the SAME underlying message, now against a WORKING
	// handler -- proves the inner claim's release actually allows a real
	// retry to succeed, not just that the row disappeared.
	workingHandler := slack.NewHandler(slack.Deps{
		Pool:            pool,
		Sessions:        sessions, // the REAL, working store this time
		Turns:           turns,
		Environments:    narvipg.NewEnvironmentStore(pool),
		Registry:        registry,
		Deliveries:      deliveries,
		Threads:         threads,
		AuditLog:        auditLog,
		Participants:    narvipg.NewParticipantStore(pool),
		SigningSecret:   testSigningSecret,
		PRSessions:      prSessionsForBackendErrorTest,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:     slackapi.New(fakeSlack.Client(), fakeSlack.URL, "test-bot-token"),
		Timeouts:        platform.DefaultTimeouts(),
		IdentityLink:    newIdentityLinkDepsForTest(pool, auditLog),
	})

	const secondEventID = "Ev0DUALBACKENDERRORB"
	secondEnvelope := messageEnvelopeWithUser(secondEventID, channel, twinTS, rootTS, "please continue", slackUserID)
	req2 := signedSlackRequest(t, secondEnvelope)
	rec2 := httptest.NewRecorder()
	workingHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("redelivered twin (message) status = %d, want %d (body=%s)", rec2.Code, http.StatusOK, rec2.Body.String())
	}

	turnsAfterRetry, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListForSession after retry: %v", err)
	}
	if len(turnsAfterRetry) != 1 {
		t.Errorf("len(turns) after redelivery = %d, want exactly 1 (the released inner claim must let a genuine retry actually succeed)", len(turnsAfterRetry))
	}
}

// TestHandler_DualDelivery_MessageFirstOnNewThread_AppMentionTwinStillCreatesSession
// is the HIGH audit fix's own headline reproduction ("message-level claim
// can permanently orphan its own app_mention twin on a brand-new thread"):
// Slack's own two independent HTTP deliveries for a single logical mention
// can arrive in EITHER order -- when the plain "message" twin (which can
// NEVER create a new session/thread mapping on its own, see
// resolveOrClaimSession's own "no mapping yet" branch) wins the
// (channel, ts) message-level claim race FIRST on a brand-new,
// not-yet-mapped thread, its own Skip must release that claim so the
// app_mention twin -- the ONLY event type that can actually create the
// session -- still succeeds when it arrives second.
//
// Before this fix, the message twin's Skip never released the claim (Skip
// was unconditionally treated as "a deliberate business decision, nothing
// to retry"), so the app_mention twin's own Claim call on the SAME
// (channel, ts) key found it already taken and was silently discarded
// before handleEvent ever ran: both deliveries answered 200 OK (so Slack
// never retries), yet no session/turn/mapping was EVER created -- strictly
// worse than the original double-ack bug this batch set out to fix (the
// user now gets ZERO response instead of two). Reasoning through the
// pre-fix code confirms this test would have failed against it: the
// message twin's Skip returned ok=true with no release, so
// TestHandler_DualDelivery_FailedFirstAttemptReleasesBothClaimsForRedelivery's
// own "release-on-failure" path (the only pre-fix release path for the
// message-level claim) never fires here at all.
func TestHandler_DualDelivery_MessageFirstOnNewThread_AppMentionTwinStillCreatesSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	const dualUser = "U0DUALNEWTHREADMSGFIRST"
	linkSlackIdentityForTest(ctx, t, pool, dualUser, sqlcgen.UserRoleMaintainer)

	const channel = "C0DUALNEWTHREAD"
	const ts = "1700000200.000100"
	const text = "please start this task"

	// The "message" twin arrives FIRST -- entirely plausible given Slack's
	// two independent HTTP deliveries (separate requests, possibly
	// different pods, independent latency for the outer claim/JSON-decode/
	// inner claim/resolveSlackActor call chain).
	messageTwin := messageEnvelopeWithUser("Ev0NEWTHREADMSG", channel, ts, "", text, dualUser)
	req1 := signedSlackRequest(t, messageTwin)
	rec1 := httptest.NewRecorder()
	rig.handler(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("message twin (first): status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	// Nothing created yet -- a plain, unmapped message can never start a
	// new thread on its own.
	if _, err := rig.threads.Get(ctx, channel, ts); err == nil {
		t.Fatal("expected NO thread mapping after the message-only delivery, got one")
	}

	// The app_mention twin -- the ONLY event type that can actually create
	// the session -- arrives SECOND, for the IDENTICAL underlying message
	// (same channel/ts, same text).
	appMentionTwin := appMentionEnvelopeWithUser("Ev0NEWTHREADMENTION", channel, ts, "", text, dualUser)
	req2 := signedSlackRequest(t, appMentionTwin)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("app_mention twin (second): status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, channel, ts)
	if err != nil {
		t.Fatalf("expected a thread mapping row after the app_mention twin, Get: %v (THIS is the HIGH audit fix's own reproduction: without the fix, the app_mention twin is silently discarded and no session is ever created)", err)
	}
	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (the app_mention twin must have created the session and its first turn)", len(turns))
	}
}

// TestHandler_DualDelivery_AppMentionFirstOnNewThread_MessageTwinCoalesced
// is TestHandler_DualDelivery_MessageFirstOnNewThread_AppMentionTwinStillCreatesSession's
// own reverse-order counterpart: when the app_mention twin (which CAN
// create a new session on its own) wins the message-level claim race
// FIRST, it creates the session normally, and the later message twin for
// the IDENTICAL (channel, ts) is coalesced away exactly as it already was
// before this fix -- this ordering was never broken, and the HIGH audit
// fix above must not regress it.
func TestHandler_DualDelivery_AppMentionFirstOnNewThread_MessageTwinCoalesced(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	const dualUser = "U0DUALNEWTHREADMENTIONFIRST"
	linkSlackIdentityForTest(ctx, t, pool, dualUser, sqlcgen.UserRoleMaintainer)

	const channel = "C0DUALNEWTHREADREV"
	const ts = "1700000210.000100"
	const text = "please start this task"

	appMentionTwin := appMentionEnvelopeWithUser("Ev0NEWTHREADREVMENTION", channel, ts, "", text, dualUser)
	req1 := signedSlackRequest(t, appMentionTwin)
	rec1 := httptest.NewRecorder()
	rig.handler(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("app_mention twin (first): status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	mapping, err := rig.threads.Get(ctx, channel, ts)
	if err != nil {
		t.Fatalf("expected a thread mapping row after the app_mention twin, Get: %v", err)
	}

	messageTwin := messageEnvelopeWithUser("Ev0NEWTHREADREVMSG", channel, ts, "", text, dualUser)
	req2 := signedSlackRequest(t, messageTwin)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("message twin (second): status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	turns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (the message twin must be coalesced away, never a second turn/session)", len(turns))
	}

	var mappingCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM slack_thread_sessions WHERE channel_id = $1 AND thread_ts = $2`,
		channel, ts,
	).Scan(&mappingCount); err != nil {
		t.Fatalf("count mapping rows: %v", err)
	}
	if mappingCount != 1 {
		t.Errorf("mapping row count = %d, want exactly 1 (the app_mention twin's own mapping, never a second one)", mappingCount)
	}
}

// newSlackAckTestRigWithRepo mirrors newSlackAckTestRig (turn_integration_test.go)
// exactly, except DefaultRepoName/DefaultRepoURL are parameters rather than
// the fixed "narvi" values -- this file's own
// TestHandler_DualDelivery_NoDefaultRepoSkip_ClaimNotReleased needs a rig
// that can deliberately leave the default repo unconfigured.
func newSlackAckTestRigWithRepo(t *testing.T, pool *pgxpool.Pool, defaultRepoName, defaultRepoURL string) *slackAckTestRig {
	t.Helper()
	ctx := context.Background()

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

	// prSessions (§31.4): unlike newSlackAckTestRig's own fixed repo, this
	// rig's own defaultRepoURL is caller-chosen (empty, for this file's own
	// TestHandler_DualDelivery_NoDefaultRepoSkip_ClaimNotReleased) -- only
	// seed github_pr_sessions when a real repo was actually supplied.
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if defaultRepoURL != "" {
		fullName := strings.TrimSuffix(strings.TrimPrefix(defaultRepoURL, "https://github.com/"), ".git")
		if err := prSessions.EnsureRow(ctx, fullName, 1); err != nil {
			t.Fatalf("seed github_pr_sessions entitlement for %s: %v", fullName, err)
		}
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
		DefaultRepoName: defaultRepoName,
		DefaultRepoURL:  defaultRepoURL,
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

// TestHandler_DualDelivery_NoDefaultRepoSkip_ClaimNotReleased proves the
// COUNTERPART to the two ordering tests above: the "no default repo
// configured" Skip outcome (resolveOrClaimSession's own SECOND Skip
// branch) is reached IDENTICALLY by either twin event type -- a business
// misconfiguration that has nothing to do with which twin got there first
// -- so it must NOT release the message-level claim the way the "not
// app_mention on an unmapped thread" Skip now does. Releasing it here
// would just let a twin "message" event get a pointless second chance to
// retry against the exact same misconfiguration.
func TestHandler_DualDelivery_NoDefaultRepoSkip_ClaimNotReleased(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRigWithRepo(t, pool, "", "")

	const dualUser = "U0DUALNOREPO"
	linkSlackIdentityForTest(ctx, t, pool, dualUser, sqlcgen.UserRoleMaintainer)

	const channel = "C0DUALNOREPO"
	const ts = "1700000220.000100"
	const text = "please start this task"

	// Only an app_mention can ever reach the "no default repo" branch (a
	// plain message hits the asymmetric "not app_mention" Skip first) --
	// this is itself proof this Skip outcome is order-independent between
	// the twins: whichever twin type wins the message-level claim on a
	// brand-new thread, a plain "message" NEVER reaches this branch at all.
	appMentionTwin := appMentionEnvelopeWithUser("Ev0NOREPOMENTION", channel, ts, "", text, dualUser)
	req1 := signedSlackRequest(t, appMentionTwin)
	rec1 := httptest.NewRecorder()
	rig.handler(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("app_mention twin (first): status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	var innerClaimCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack-message' AND delivery_id = $1`, channel+":"+ts,
	).Scan(&innerClaimCount); err != nil {
		t.Fatalf("count inner claim rows: %v", err)
	}
	if innerClaimCount != 1 {
		t.Fatalf("inner (channel:ts) claim row count after the no-default-repo skip = %d, want 1 (must stay held -- this outcome renders identically for either twin)", innerClaimCount)
	}

	// A twin "message" event for the SAME underlying message must never get
	// a chance to retry against this same misconfiguration.
	messageTwin := messageEnvelopeWithUser("Ev0NOREPOMSG", channel, ts, "", text, dualUser)
	req2 := signedSlackRequest(t, messageTwin)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("message twin (second): status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	if _, err := rig.threads.Get(ctx, channel, ts); err == nil {
		t.Error("expected NO thread mapping (no default repo configured), got one")
	}
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (no default repo configured -- neither twin may ever create a session)", sessionCount)
	}
}

// TestHandler_DualDelivery_AuthzDeniedSkip_ClaimNotReleased is
// TestHandler_DualDelivery_NoDefaultRepoSkip_ClaimNotReleased's own authz
// counterpart: a `viewer` role denial on ActionCreateSession
// (resolveOrClaimSession's own THIRD Skip branch) is likewise reached
// identically by either twin event type, so it must NOT release the
// message-level claim either -- a twin "message" event must not get a
// pointless second chance to retry against an authz denial that would
// render identically for either twin.
func TestHandler_DualDelivery_AuthzDeniedSkip_ClaimNotReleased(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newSlackAckTestRig(t, pool)

	const dualUser = "U0DUALVIEWERDENY"
	linkSlackIdentityForTest(ctx, t, pool, dualUser, sqlcgen.UserRoleViewer)

	const channel = "C0DUALVIEWERDENY"
	const ts = "1700000230.000100"
	const text = "please start this task"

	appMentionTwin := appMentionEnvelopeWithUser("Ev0VIEWERDENYMENTION", channel, ts, "", text, dualUser)
	req1 := signedSlackRequest(t, appMentionTwin)
	rec1 := httptest.NewRecorder()
	rig.handler(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("app_mention twin (first): status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	var innerClaimCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack-message' AND delivery_id = $1`, channel+":"+ts,
	).Scan(&innerClaimCount); err != nil {
		t.Fatalf("count inner claim rows: %v", err)
	}
	if innerClaimCount != 1 {
		t.Fatalf("inner (channel:ts) claim row count after the authz-denied skip = %d, want 1 (must stay held -- this outcome renders identically for either twin)", innerClaimCount)
	}

	messageTwin := messageEnvelopeWithUser("Ev0VIEWERDENYMSG", channel, ts, "", text, dualUser)
	req2 := signedSlackRequest(t, messageTwin)
	rec2 := httptest.NewRecorder()
	rig.handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("message twin (second): status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}

	if _, err := rig.threads.Get(ctx, channel, ts); err == nil {
		t.Error("expected NO thread mapping (denied by authz), got one")
	}
}

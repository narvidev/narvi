//go:build integration

// Integration tests proving the perf fix in identity.go (resolveSlackActor/
// resolveSlackActorSingleAttempt's own identitylink.LookupLinkedUserID
// pre-check): an ALREADY-LINKED Slack identity must resolve without ever
// calling Slack's own users.info API, while a not-yet-linked identity must
// still call it exactly as before -- mirrors identity_integration_test.go's
// own conventions (testcontainers Postgres, real handlers, synthetic
// real-shaped payloads), plus a fake Slack API server that COUNTS its own
// /users.info hits instead of just answering them.
package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newFakeSlackCountingUsersInfo mirrors newFakeSlackWithUsersInfo
// (identity_integration_test.go, same package) exactly, except it also
// counts every /users.info request it observes -- the call-count
// assertion this file's own tests need, that a plain "does it answer
// correctly" stub can't give.
func newFakeSlackCountingUsersInfo(t *testing.T, userID, email string) (*httptest.Server, *int32) {
	t.Helper()
	var usersInfoCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users.info" {
			atomic.AddInt32(&usersInfoCalls, 1)
			if r.URL.Query().Get("user") == userID {
				_, _ = w.Write([]byte(`{"ok":true,"user":{"profile":{"email":"` + email + `"}}}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, &usersInfoCalls
}

// TestHandler_AppMention_AlreadyLinkedIdentity_SkipsUsersInfoFetch proves
// the Events-API path (resolveSlackActor, handler.go's handleEvent): when
// ev.User is ALREADY linked to a Narvi user, no /users.info call is made at
// all -- the identity resolves via identitylink.LookupLinkedUserID's own
// pre-check, with the same eventual actor_user_id Resolve's own internal
// fast path would have produced, but without the discarded fetch this
// perf fix removes.
func TestHandler_AppMention_AlreadyLinkedIdentity_SkipsUsersInfoFetch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-ALREADY-LINKED-MENTION"
	users := narvipg.NewUserStore(pool)
	linkedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "already-linked-mention@example.com", DisplayName: "Already Linked", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: linkedUser.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: slackUserID,
		LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("seed linked identity: %v", err)
	}

	fakeSlack, usersInfoCalls := newFakeSlackCountingUsersInfo(t, slackUserID, "already-linked-mention@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	envelope := appMentionEnvelopeWithUser("Ev0ALREADYLINKED001", "C0ALREADYLINKED", "1700000060.000100", "", "please help", slackUserID)
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if got := atomic.LoadInt32(usersInfoCalls); got != 0 {
		t.Errorf("/users.info call count = %d, want 0 (identity already linked -- the fetch must be skipped entirely)", got)
	}

	// The session this app_mention created must still carry the REAL
	// linked user as created_by -- the pre-check must resolve the SAME
	// actor Resolve's own internal fast path would have, just without the
	// fetch.
	var createdBy string
	if err := pool.QueryRow(ctx,
		`SELECT created_by::text FROM sessions WHERE spawn_source = 'slack' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&createdBy); err != nil {
		t.Fatalf("query session created_by: %v", err)
	}
	if createdBy != linkedUser.ID.String() {
		t.Errorf("session created_by = %q, want %q (the already-linked user)", createdBy, linkedUser.ID.String())
	}
}

// TestHandler_AppMention_UnlinkedIdentity_CallsUsersInfoFetch is this
// file's own counterpart proving the OTHER half still holds: a genuinely
// not-yet-linked identity still calls /users.info exactly as before this
// perf fix (the pre-check must never suppress a fetch that's actually
// needed).
func TestHandler_AppMention_UnlinkedIdentity_CallsUsersInfoFetch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-UNLINKED-MENTION"
	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unlinked-mention@example.com", DisplayName: "Unlinked Mention", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack, usersInfoCalls := newFakeSlackCountingUsersInfo(t, slackUserID, "unlinked-mention@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	envelope := appMentionEnvelopeWithUser("Ev0UNLINKED001", "C0UNLINKED", "1700000061.000100", "", "please help", slackUserID)
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if got := atomic.LoadInt32(usersInfoCalls); got != 1 {
		t.Errorf("/users.info call count = %d, want 1 (not yet linked -- must still fetch)", got)
	}

	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v (auto-linked via the fetched email)", identity.UserID, matchedUser.ID)
	}
}

// TestInteractivityHandler_BlockActions_AlreadyLinkedIdentity_SkipsUsersInfoFetch
// proves the Interactivity-path variant (resolveSlackActorSingleAttempt,
// interactive.go's decideAndUpdateMessage) -- this Step's own MOST exposed
// instance of the pre-fix defect, since this path shares Slack's tight,
// non-retryable ~3s ack budget with a real Postgres write and a second
// outbound Slack call (see resolveSlackActorSingleAttempt's own doc
// comment). An already-linked clicker must decide the plan with NO
// /users.info call at all.
func TestInteractivityHandler_BlockActions_AlreadyLinkedIdentity_SkipsUsersInfoFetch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-ALREADY-LINKED-CLICK"
	users := narvipg.NewUserStore(pool)
	linkedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "already-linked-click@example.com", DisplayName: "Already Linked Clicker", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: linkedUser.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: slackUserID,
		LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("seed linked identity: %v", err)
	}

	fakeSlack, usersInfoCalls := newFakeSlackCountingUsersInfo(t, slackUserID, "already-linked-click@example.com")
	slackClient := slackapi.New(fakeSlack.Client(), fakeSlack.URL, "test-bot-token")

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	events := narvipg.NewEventStore(pool)
	planDocuments := narvipg.NewPlanDocumentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	handler := slack.NewInteractivityHandler(slack.InteractiveDeps{
		Pool:                pool,
		Sessions:            sessions,
		Turns:               turns,
		Plans:               plans,
		Events:              events,
		PlanDocuments:       planDocuments,
		Outbox:              narvipg.NewOutboxStore(pool, false),
		LinearAgentSessions: narvipg.NewLinearAgentSessionStore(pool),
		Registry:            registry,
		SlackClient:         slackClient,
		AuditLog:            auditLog,
		IdentityLink:        newIdentityLinkDepsForTest(pool, auditLog),
		Participants:        narvipg.NewParticipantStore(pool),
		SigningSecret:       testSigningSecret,
		Timeouts:            platform.DefaultTimeouts(),
	})

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack, CreatedBy: linkedUser.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	turn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C-ALREADY-LINKED", "1700000200.000100", "trigger-already-linked", slackUserID)

	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := atomic.LoadInt32(usersInfoCalls); got != 0 {
		t.Errorf("/users.info call count = %d, want 0 (identity already linked -- the fetch must be skipped entirely)", got)
	}

	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if !updatedPlan.DecidedBy.Valid || updatedPlan.DecidedBy != linkedUser.ID {
		t.Errorf("DecidedBy = %v, want %v (the already-linked user)", updatedPlan.DecidedBy, linkedUser.ID)
	}
}

// TestInteractivityHandler_BlockActions_UnlinkedIdentity_CallsUsersInfoFetch
// is this file's own counterpart proving the Interactivity-path variant
// still fetches for a genuinely not-yet-linked clicker, exactly as before
// this perf fix.
func TestInteractivityHandler_BlockActions_UnlinkedIdentity_CallsUsersInfoFetch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-UNLINKED-CLICK"
	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unlinked-click@example.com", DisplayName: "Unlinked Clicker", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack, usersInfoCalls := newFakeSlackCountingUsersInfo(t, slackUserID, "unlinked-click@example.com")
	slackClient := slackapi.New(fakeSlack.Client(), fakeSlack.URL, "test-bot-token")

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	events := narvipg.NewEventStore(pool)
	planDocuments := narvipg.NewPlanDocumentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	handler := slack.NewInteractivityHandler(slack.InteractiveDeps{
		Pool:                pool,
		Sessions:            sessions,
		Turns:               turns,
		Plans:               plans,
		Events:              events,
		PlanDocuments:       planDocuments,
		Outbox:              narvipg.NewOutboxStore(pool, false),
		LinearAgentSessions: narvipg.NewLinearAgentSessionStore(pool),
		Registry:            registry,
		SlackClient:         slackClient,
		AuditLog:            auditLog,
		IdentityLink:        newIdentityLinkDepsForTest(pool, auditLog),
		Participants:        narvipg.NewParticipantStore(pool),
		SigningSecret:       testSigningSecret,
		Timeouts:            platform.DefaultTimeouts(),
	})

	// CreatedBy = matchedUser.ID so the auto-linked actor owns the session
	// (this file's own tests are about the fetch call count, not RBAC, so
	// they mirror TestInteractivityHandler_BlockActions_ApprovePlan_AutoLinksUniqueMatch's
	// own ownership shape to keep the decision itself uneventful).
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack, CreatedBy: matchedUser.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	turn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C-UNLINKED", "1700000201.000100", "trigger-unlinked", slackUserID)

	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := atomic.LoadInt32(usersInfoCalls); got != 1 {
		t.Errorf("/users.info call count = %d, want 1 (not yet linked -- must still fetch)", got)
	}

	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if !updatedPlan.DecidedBy.Valid || updatedPlan.DecidedBy != matchedUser.ID {
		t.Errorf("DecidedBy = %v, want %v (auto-linked via the fetched email)", updatedPlan.DecidedBy, matchedUser.ID)
	}
}

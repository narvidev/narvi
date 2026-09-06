//go:build integration

// Integration test for the MEDIUM audit fix ("authorizeSessionAction
// conflates a genuine backend error with a real authorization denial",
// internal/adapters/inbound/slack/identity.go): a transient backend
// failure encountered WHILE checking authorization must be distinguished
// from a genuine denial -- the former now flows into the SAME
// release-the-claim-and-retry path H2 already wired up for every other
// post-claim failure, rather than being silently treated as "skip, no
// release" the way a one-off DB blip previously was. Mirrors
// identity_integration_test.go's own conventions (a real, auto-linkable
// fixture user, a real slack.NewHandler) exactly, except deps.Sessions is
// built on a pool that's already been closed -- every call through it
// fails deterministically (pgxpool.ErrClosedPool), with no timing
// dependency, simulating "deps.Sessions.Get hitting a dropped connection"
// (authorizeSessionAction's own ErrActorNotAuthorized doc comment) without
// needing an actual dropped network connection.
package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// TestHandler_ReplyOnMappedThread_AuthzBackendErrorReleasesClaim is the
// MEDIUM audit fix's own headline proof: a genuine backend failure INSIDE
// authorizeSessionAction (deps.Sessions.Get erroring for a reason having
// nothing to do with the actor's own authorization) must NOT be silently
// conflated with a real denial. Before this fix, authorizeSessionAction's
// own bare bool made the two indistinguishable: the claim was never
// released, the webhook answered 200, and the reply was silently dropped
// forever with no chance of a redelivery ever retrying it. This proves the
// SAME release-the-claim-and-answer-non-2xx path H2 already wired up for
// every other post-claim failure now fires here too.
func TestHandler_ReplyOnMappedThread_AuthzBackendErrorReleasesClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "backend-error-replier@example.com", DisplayName: "Backend Error Replier", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-BACKEND-ERROR", "backend-error-replier@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)

	sessions := narvipg.NewSessionStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)

	// CreatedBy = matchedUser.ID: ownership is irrelevant to this test (the
	// broken Sessions store below fails BEFORE OwnedOrJoined is ever
	// reached), but mirrors this package's own established fixture shape.
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack, CreatedBy: matchedUser.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := threads.Claim(ctx, "C0BACKENDERROR", "1700000090.000100", session.ID); err != nil {
		t.Fatalf("claim thread mapping: %v", err)
	}

	// A SEPARATE pool, pointed at the SAME database, closed immediately --
	// every subsequent call through a store built on it fails
	// deterministically (pgxpool.ErrClosedPool), simulating a genuine
	// "backend call failed while checking" with no timing dependency.
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

	// prSessions (§31.4): see newSlackTestRigWithEpistemicCheckDefault's own
	// identical addition (handler_integration_test.go).
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, "narvidev/narvi", 1); err != nil {
		t.Fatalf("seed github_pr_sessions entitlement: %v", err)
	}

	handler := slack.NewHandler(slack.Deps{
		Pool:            pool,
		Sessions:        brokenSessions, // the deliberately-broken store
		Turns:           turns,
		Environments:    narvipg.NewEnvironmentStore(pool),
		Registry:        registry,
		Deliveries:      narvipg.NewWebhookDeliveryStore(pool),
		Threads:         threads,
		AuditLog:        auditLog,
		Participants:    narvipg.NewParticipantStore(pool),
		PRSessions:      prSessions,
		SigningSecret:   testSigningSecret,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:     slackapi.New(fakeSlack.Client(), fakeSlack.URL, "test-bot-token"),
		Timeouts:        platform.DefaultTimeouts(),
		IdentityLink:    newIdentityLinkDepsForTest(pool, auditLog),
	})

	const eventID = "Ev0BACKENDERROR001"
	envelope := messageEnvelopeWithUser(eventID, "C0BACKENDERROR", "1700000090.000200", "1700000090.000100", "please continue", "U-BACKEND-ERROR")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (a genuine backend failure during the authz check, not a denial); body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack' AND delivery_id = $1`, eventID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 0 {
		t.Errorf("webhook_deliveries row count = %d, want 0 (the claim must be released so a redelivery can retry)", deliveryRowCount)
	}

	turnsAfter, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turnsAfter) != 0 {
		t.Errorf("len(turns) = %d, want 0 (must not have proceeded past the failed authz check)", len(turnsAfter))
	}
}

// TestInteractivityHandler_BlockActions_ApprovePlan_AuthzBackendError is
// the MEDIUM audit fix's own headline proof for interactive.go's OWN
// SEPARATE authorizeSessionAction copy ("Slack's interactive.go has its
// OWN separate, still-unfixed copy" of identity.go's already-fixed
// conflation): a genuine backend failure INSIDE it (deps.Sessions.Get
// erroring for a reason having nothing to do with the actor's own
// authorization) must NOT be silently conflated with a real denial. Before
// this fix, the click still acked 200 (this route's own unconditional
// contract, never changes), but chat.update showed the exact same
// misleading "you don't have permission" text
// (slackPlanForbiddenText/TestInteractivityHandler_BlockActions_ApprovePlan_DeniedForUnownedMember's
// own counterpart) a real denial would -- silently discarding the actor's
// real decision with no indication it was ever safe to retry. This proves
// the message shown is now the HONEST generic-error text instead, and that
// the plan itself was never actually decided (DecidePlan never reached).
func TestInteractivityHandler_BlockActions_ApprovePlan_AuthzBackendError(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "ix-backend-error-decider@example.com", DisplayName: "Interactivity Backend Error Decider", Role: sqlcgen.UserRoleMember,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack, requests := newFakeSlackRecordingWithUsersInfo(t, "U-IX-BACKEND-ERROR", "ix-backend-error-decider@example.com")
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

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack})
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

	// A SEPARATE pool, pointed at the SAME database, closed immediately --
	// every subsequent call through a store built on it fails
	// deterministically (pgxpool.ErrClosedPool), simulating a genuine
	// "backend call failed while checking" with no timing dependency.
	brokenPool, err := narvipg.NewPool(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("new broken pool: %v", err)
	}
	brokenPool.Close()
	brokenSessions := narvipg.NewSessionStore(brokenPool)

	handler := slack.NewInteractivityHandler(slack.InteractiveDeps{
		Pool:                pool,
		Sessions:            brokenSessions, // the deliberately-broken store
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

	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C-IX-BACKEND-ERROR", "1700000000.000400", "trigger-backend-error", "U-IX-BACKEND-ERROR")

	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (this route always acks 200 regardless of the underlying decision); body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if updatedPlan.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("Status = %v, want %v (a genuine backend error must never reach DecidePlan)", updatedPlan.Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if updatedPlan.DecidedBy.Valid {
		t.Errorf("DecidedBy = %v, want invalid", updatedPlan.DecidedBy)
	}

	// This fixture user auto-links for the FIRST time here, so a
	// chat.postEphemeral identity-link notice (resolveSlackActorSingleAttempt's
	// own side effect, delivered BEFORE authorizeSessionAction ever runs)
	// is recorded ahead of the chat.update this test actually cares about --
	// drain past it rather than assuming chat.update is the first request
	// on the channel.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-requests:
			if got.path != "/chat.update" {
				continue
			}
			text, _ := got.body["text"].(string)
			if text != "Something went wrong recording this decision. Please try again." {
				t.Errorf("chat.update text = %q, want the honest generic-error text -- NOT a misleading permission-denied message for what is actually a backend failure", text)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the synchronous chat.update call")
		}
	}
}

// TestInteractivityHandler_ViewSubmission_AuthzBackendError is
// TestInteractivityHandler_BlockActions_ApprovePlan_AuthzBackendError's own
// handleViewSubmission twin: a genuine backend failure inside
// authorizeSessionAction must surface the honest generic-error text via
// Slack's own "response_action":"errors" modal mechanism, NOT
// slackPromptForbiddenErrorText's misleading "you don't have permission"
// wording -- and must never create the request-changes turn.
func TestInteractivityHandler_ViewSubmission_AuthzBackendError(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "ix-backend-error-submitter@example.com", DisplayName: "Interactivity Backend Error Submitter", Role: sqlcgen.UserRoleMember,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-IX-VS-BACKEND-ERROR", "ix-backend-error-submitter@example.com")
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

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack})
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

	// A SEPARATE pool, pointed at the SAME database, closed immediately --
	// see the block_actions twin above for the full rationale.
	brokenPool, err := narvipg.NewPool(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("new broken pool: %v", err)
	}
	brokenPool.Close()
	brokenSessions := narvipg.NewSessionStore(brokenPool)

	handler := slack.NewInteractivityHandler(slack.InteractiveDeps{
		Pool:                pool,
		Sessions:            brokenSessions, // the deliberately-broken store
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

	privateMetadata := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	viewSubmission := map[string]any{
		"type": "view_submission",
		"user": map[string]string{"id": "U-IX-VS-BACKEND-ERROR"},
		"view": map[string]any{
			"callback_id":      slackapi.RequestChangesCallbackID,
			"private_metadata": privateMetadata,
			"state": map[string]any{
				"values": map[string]any{
					slackapi.RequestChangesBlockID: map[string]any{
						slackapi.RequestChangesActionID: map[string]any{
							"type":  "plain_text_input",
							"value": "please also fix the tests",
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(viewSubmission)
	if err != nil {
		t.Fatalf("marshal view_submission payload: %v", err)
	}

	req := signedInteractivityRequest(t, string(raw))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var respBody struct {
		ResponseAction string            `json:"response_action"`
		Errors         map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rec.Body.String())
	}
	if respBody.ResponseAction != "errors" {
		t.Fatalf("response_action = %q, want %q; body = %s", respBody.ResponseAction, "errors", rec.Body.String())
	}
	got := respBody.Errors[slackapi.RequestChangesBlockID]
	if got != "Something went wrong submitting this. Please try again." {
		t.Errorf("modal error text = %q, want the honest generic-error text -- NOT a misleading permission-denied message for what is actually a backend failure", got)
	}

	turnsAfter, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turnsAfter) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn -- must not have proceeded past the failed authz check)", len(turnsAfter))
	}
}

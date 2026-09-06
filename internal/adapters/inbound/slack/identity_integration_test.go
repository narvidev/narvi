//go:build integration

// Integration tests proving §13.2's ("identities + full RBAC", §13.2)
// own auto-linking wiring actually fires from a REAL POST
// /webhooks/slack/interactive request -- mirrors
// interactive_integration_test.go's own conventions (testcontainers
// Postgres, a real slack.NewInteractivityHandler, synthetic real-shaped
// payloads), plus a fake Slack API server that ALSO answers /users.info
// realistically (interactive_integration_test.go's own fakeSlack answers
// every path with a bare {"ok":true}, which is enough for chat.update/
// views.open but reports no email at all for users.info -- this file's
// own stub is deliberately richer).
package slack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newFakeSlackWithUsersInfo builds a fake Slack API server answering
// EVERY path (chat.postMessage/chat.update/views.open) with a bare
// {"ok":true} -- EXCEPT /users.info, which answers with a real profile
// email for userID, any other user id getting {"ok":true} with no email
// at all (mirrors newLinearGraphQLStub's own "one email, any id" simplicity
// for these tests' own purposes).
func newFakeSlackWithUsersInfo(t *testing.T, userID, email string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users.info" && r.URL.Query().Get("user") == userID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"profile": map[string]any{"email": email}},
			})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// blockActionsPayloadJSONWithUser mirrors blockActionsPayloadJSON
// (interactive_integration_test.go, same package) but also sets the
// top-level "user" field -- the field this Step's own auto-linking wiring
// reads (interactive.go's own blockActionsPayload.User).
func blockActionsPayloadJSONWithUser(actionID, value, channel, messageTS, triggerID, userID string) string {
	payload := map[string]any{
		"type":       "block_actions",
		"trigger_id": triggerID,
		"channel":    map[string]string{"id": channel},
		"message":    map[string]string{"ts": messageTS},
		"actions":    []map[string]string{{"action_id": actionID, "value": value}},
		"user":       map[string]string{"id": userID},
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

// recordedIdentityRequest captures one request a fake Slack API server
// observed -- enough for this file's own ephemeral-vs-public-delivery
// assertions (which endpoint, the decoded JSON body).
type recordedIdentityRequest struct {
	path string
	body map[string]any
}

// newFakeSlackRecordingWithUsersInfo mirrors newFakeSlackWithUsersInfo
// above, but ALSO records every request it observes (path + decoded JSON
// body) onto the returned channel -- used by this file's own tests
// proving the magic-link identity notice is delivered via
// chat.postEphemeral (scoped to one user), never chat.postMessage (the
// whole channel/thread), per this Step's own security-remediation fix
// (slackapi.Client.PostEphemeral's own doc comment).
func newFakeSlackRecordingWithUsersInfo(t *testing.T, userID, email string) (*httptest.Server, <-chan recordedIdentityRequest) {
	t.Helper()
	requests := make(chan recordedIdentityRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users.info" && r.URL.Query().Get("user") == userID {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"profile": map[string]any{"email": email}},
			})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- recordedIdentityRequest{path: r.URL.Path, body: body}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func newIdentityLinkDepsForTest(pool *pgxpool.Pool, auditLog *narvipg.AuditLogStore) identitylink.Deps {
	return identitylink.Deps{
		Pool:          pool,
		Users:         narvipg.NewUserStore(pool),
		Identities:    narvipg.NewIdentityStore(pool),
		LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:      auditLog,
		PublicBaseURL: "https://narvi.example.com",
		PromptTTL:     time.Hour,
	}
}

// TestInteractivityHandler_BlockActions_ApprovePlan_AutoLinksUniqueMatch
// proves an approve_plan click from a Slack user whose fetched profile
// email matches EXACTLY one existing user results in plans.decided_by
// being that user's own id -- not bot attribution -- and creates the
// identities row (linked_via=auto_email).
func TestInteractivityHandler_BlockActions_ApprovePlan_AutoLinksUniqueMatch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "slack-clicker@example.com", DisplayName: "Slack Clicker", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-CLICKER", "slack-clicker@example.com")
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

	// CreatedBy = matchedUser.ID: this Step's own RBAC gate (domain/authz.
	// Authorize(ActionApprovePlan)) now requires a `member` actor to own
	// or have joined the target session -- see
	// TestInteractivityHandler_BlockActions_ApprovePlan_DeniedForUnownedMember
	// below for the counterpart proving a member WITHOUT ownership is
	// rejected.
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
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C123", "1700000000.000100", "trigger-1", "U-CLICKER")

	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if !updatedPlan.DecidedBy.Valid || updatedPlan.DecidedBy != matchedUser.ID {
		t.Errorf("DecidedBy = %v, want %v (the auto-linked user, not bot attribution)", updatedPlan.DecidedBy, matchedUser.ID)
	}

	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-CLICKER")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail {
		t.Errorf("LinkedVia = %v, want auto_email", identity.LinkedVia)
	}
}

// TestInteractivityHandler_BlockActions_ApprovePlan_DeniedForUnownedMember
// is this Step's own regression test for a confirmed security review
// finding: BEFORE this fix, an approve_plan click from a Slack user who
// auto-links to a REAL Narvi user (any role, any ownership) unconditionally
// decided the plan -- domain/authz.Authorize was never consulted at all.
// This proves a `member` whose auto-linked account neither created NOR
// joined the target session is now REJECTED: the plan stays
// awaiting_approval, decided_by stays NULL -- exactly like the REST
// /api/sessions/:id/plans/:planId/approve endpoint already behaves for
// the identical (role, ownership) combination (canActOnPlan, planauthz.go).
// The identity itself still auto-links (this Step's own auto-linking work
// is not rolled back by a denied action) -- only the STATE-CHANGING EFFECT
// is refused.
func TestInteractivityHandler_BlockActions_ApprovePlan_DeniedForUnownedMember(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unowned-member@example.com", DisplayName: "Unowned Member", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-UNOWNED", "unowned-member@example.com")
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

	// Deliberately NO CreatedBy set, and no participants row -- matchedUser
	// neither created nor joined this session.
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

	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C123", "1700000000.000200", "trigger-2", "U-UNOWNED")

	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if updatedPlan.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("Status = %v, want %v (denied by authz, must not decide)", updatedPlan.Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if updatedPlan.DecidedBy.Valid {
		t.Errorf("DecidedBy = %v, want invalid (denied -- never decided)", updatedPlan.DecidedBy)
	}

	// The identity itself still auto-links -- only the effect is denied.
	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-UNOWNED")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v", identity.UserID, matchedUser.ID)
	}
}

// TestInteractivityHandler_BlockActions_ApprovePlan_DeniedForViewerEvenIfOwned
// proves the role gate itself, independent of ownership: a `viewer` who
// DOES own the session (CreatedBy == the auto-linked user) is STILL denied
// -- §13.3's own matrix has no own/joined carve-out for viewer at all
// (domain/authz's own matrix.go: ActionApprovePlan's allowIfOwned set is
// {member} only).
func TestInteractivityHandler_BlockActions_ApprovePlan_DeniedForViewerEvenIfOwned(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "owner-viewer@example.com", DisplayName: "Owner Viewer", Role: sqlcgen.UserRoleViewer,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-VIEWER", "owner-viewer@example.com")
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

	// Owned BY the viewer -- proves the denial is the ROLE gate, not the
	// ownership carve-out.
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
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C123", "1700000000.000300", "trigger-3", "U-VIEWER")

	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if updatedPlan.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("Status = %v, want %v (viewer must never decide, even on an owned session)", updatedPlan.Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if updatedPlan.DecidedBy.Valid {
		t.Errorf("DecidedBy = %v, want invalid", updatedPlan.DecidedBy)
	}
}

// TestInteractivityHandler_ViewSubmission_DeniedForUnownedMember proves
// the "Request changes" modal submission -- Linear's/Slack's own
// ActionPromptSession-gated turn creation -- is likewise denied for an
// auto-linked `member` with no ownership/participation in the target
// session, and responds with Slack's own "response_action": "errors"
// shape (this payload type has no channel/thread to post an ordinary
// denial message into, see interactive.go's own handleViewSubmission doc
// comment) instead of silently closing the modal.
func TestInteractivityHandler_ViewSubmission_DeniedForUnownedMember(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	_, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unowned-submitter@example.com", DisplayName: "Unowned Submitter", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-SUBMITTER", "unowned-submitter@example.com")
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

	// No CreatedBy, no participants row -- the submitter neither created
	// nor joined this session.
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

	privateMetadata := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	viewSubmission := map[string]any{
		"type": "view_submission",
		"user": map[string]string{"id": "U-SUBMITTER"},
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
	if !strings.Contains(rec.Body.String(), `"response_action":"errors"`) {
		t.Errorf("body = %s, want a response_action:errors body (denied by authz)", rec.Body.String())
	}

	turnsAfter, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turnsAfter) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn -- denied request-changes turn must never be created)", len(turnsAfter))
	}
}

// appMentionEnvelopeWithUser mirrors handler_integration_test.go's own
// appMentionEnvelope exactly, except the event's "user" field is a
// parameter rather than the fixed "U0TESTUSER" -- this file's own tests
// need to control which Slack user id an app_mention is attributed to, so
// resolveSlackActor (identity.go) can be exercised against a REAL,
// controllable identity.
func appMentionEnvelopeWithUser(eventID, channel, ts, threadTS, text, userID string) string {
	event := map[string]string{
		"type":    "app_mention",
		"channel": channel,
		"user":    userID,
		"text":    text,
		"ts":      ts,
	}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	eventJSON, _ := json.Marshal(event)
	return fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":"T0TEST","event":%s}`, eventID, eventJSON)
}

// messageEnvelopeWithUser mirrors handler_integration_test.go's own
// messageEnvelope exactly, except the event's "user" field is a parameter
// rather than the fixed "U0OTHERUSER" -- this file's own tests need to
// control which Slack user id a reply on an already-mapped thread is
// attributed to, so resolveSlackActor (identity.go) can be exercised
// against a REAL, controllable identity on the "existing thread mapping"
// path (this Step's own SECOND fix-pass regression tests below).
func messageEnvelopeWithUser(eventID, channel, ts, threadTS, text, userID string) string {
	event := map[string]string{
		"type":      "message",
		"channel":   channel,
		"user":      userID,
		"text":      text,
		"ts":        ts,
		"thread_ts": threadTS,
	}
	eventJSON, _ := json.Marshal(event)
	return fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":"T0TEST","event":%s}`, eventID, eventJSON)
}

// newSlackHandlerRigForIdentityTests wires a real slack.NewHandler (Events
// API ingress) against pool, using recordingSlackServer as its SlackClient
// AND its in-thread ack client -- unlike newSlackTestRig
// (handler_integration_test.go), this rig is given a REAL, wired
// identitylink.Deps, so a fixture user can actually auto-link.
func newSlackHandlerRigForIdentityTests(t *testing.T, pool *pgxpool.Pool, recordingSlackServer *httptest.Server, auditLog *narvipg.AuditLogStore) *slackTestRig {
	t.Helper()
	ctx := context.Background()

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	deliveries := narvipg.NewWebhookDeliveryStore(pool)
	threads := narvipg.NewSlackThreadSessionStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	handler := slack.NewHandler(slack.Deps{
		Pool:         pool,
		Sessions:     sessions,
		Turns:        turns,
		Environments: environments,
		Registry:     registry,
		Deliveries:   deliveries,
		Threads:      threads,
		AuditLog:     auditLog,
		// Participants (§13.2's own SECOND fix-pass addition, "identities
		// + full RBAC", §13.2/§13.3): authorizeSessionAction (identity.go)
		// needs this to resolve a `member` actor's own "own/joined"
		// carve-out on a reply to an already-mapped thread -- the SAME
		// participantStore instance every other test rig in this package
		// already wires.
		Participants:    narvipg.NewParticipantStore(pool),
		SigningSecret:   testSigningSecret,
		DefaultRepoName: "narvi",
		DefaultRepoURL:  "https://github.com/narvidev/narvi",
		TimestampWindow: 5 * time.Minute,
		AckTimeout:      platform.DefaultTimeouts().SlackAckTimeout,
		SlackClient:     slackapi.New(recordingSlackServer.Client(), recordingSlackServer.URL, "test-bot-token"),
		Timeouts:        platform.DefaultTimeouts(),
		IdentityLink:    newIdentityLinkDepsForTest(pool, auditLog),
	})

	return &slackTestRig{handler: handler, pool: pool, sessions: sessions, turns: turns, threads: threads}
}

// TestHandler_AppMention_CreateSessionDeniedForViewer is this Step's own
// regression test for a confirmed security review finding: BEFORE this
// fix, an app_mention from a Slack user who auto-links to a REAL Narvi
// user (any role) unconditionally created a new session -- domain/authz.
// Authorize was never consulted at all. This proves a `viewer`'s
// auto-linked account is now REJECTED: no session/thread mapping is ever
// created for this event -- exactly like the REST /api/sessions endpoint
// already rejects a viewer's own CreateSession call.
func TestHandler_AppMention_CreateSessionDeniedForViewer(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "viewer-mentioner@example.com", DisplayName: "Viewer Mentioner", Role: sqlcgen.UserRoleViewer,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-VIEWER-MENTION", "viewer-mentioner@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	envelope := appMentionEnvelopeWithUser("Ev0VIEWERDENY001", "C0VIEWERDENY", "1700000040.000100", "", "please help", "U-VIEWER-MENTION")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if _, err := rig.threads.Get(ctx, "C0VIEWERDENY", "1700000040.000100"); err == nil {
		t.Error("expected NO thread mapping row (denied by authz), got one")
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a viewer must never create a session, even auto-linked)", sessionCount)
	}

	// The identity itself still auto-links -- only the effect is denied.
	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-VIEWER-MENTION")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v", identity.UserID, matchedUser.ID)
	}
}

// TestHandler_AppMention_IdentityNoticeDeliveredEphemerally is this Step's
// own regression test for the SECOND confirmed security finding (magic-
// link identity hijack via non-ephemeral channel delivery): BEFORE this
// fix, the magic-link notice (or the "connected your account" confirmation)
// was appended to the ordinary, whole-channel-visible chat.postMessage ack
// -- ANY other member of a shared channel with an authenticated Narvi web
// session could open the link first and hijack the pending identity link.
// This proves the notice is now delivered via chat.postEphemeral, scoped
// to the mentioning user (Slack's own "user" field), and NEVER appears in
// the whole-channel-visible chat.postMessage ack at all.
func TestHandler_AppMention_IdentityNoticeDeliveredEphemerally(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// "nobody-matches" -- zero email matches, so Resolve mints a fresh
	// magic-link prompt (identitylink.Resolve's own "never guess" branch),
	// giving this test a real, sensitive magic-link URL as its own
	// notice text (a stronger proof than the "connected your account"
	// confirmation alone).
	fakeSlack, requests := newFakeSlackRecordingWithUsersInfo(t, "U-EPHEMERAL", "nobody-matches@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	envelope := appMentionEnvelopeWithUser("Ev0EPHEMERAL001", "C0EPHEMERAL", "1700000050.000100", "", "please help", "U-EPHEMERAL")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-EPHEMERAL"); err != nil {
		t.Fatalf("GetLatestForProviderAndExternalID: %v", err)
	}
	// The prompt row's own existence (above) proves a link was minted; its
	// plaintext nonce is never persisted (only its hash), so this test
	// inspects the REQUESTS this handler actually made instead, below.
	magicLinkPath := identitylink.MagicLinkPath

	// rig.handler(rec, req) above already ran the WHOLE handler
	// synchronously (every ack/notice call it makes happens before that
	// call returns) -- requests is a buffered channel, so every request
	// it ever sends is already sitting in it by now; drained
	// non-blockingly, never waiting on a timer.
	var sawEphemeralWithLink, sawPublicWithLink bool
drain:
	for {
		select {
		case got := <-requests:
			text, _ := got.body["text"].(string)
			switch got.path {
			case "/chat.postEphemeral":
				if strings.Contains(text, magicLinkPath) {
					sawEphemeralWithLink = true
					if got.body["user"] != "U-EPHEMERAL" {
						t.Errorf("chat.postEphemeral user = %v, want %q (scoped to the mentioning user)", got.body["user"], "U-EPHEMERAL")
					}
				}
			case "/chat.postMessage":
				if strings.Contains(text, magicLinkPath) {
					sawPublicWithLink = true
				}
			}
		default:
			break drain
		}
	}

	if !sawEphemeralWithLink {
		t.Error("no chat.postEphemeral call carried the magic-link notice -- want it delivered privately")
	}
	if sawPublicWithLink {
		t.Error("chat.postMessage (whole-channel-visible) carried the magic-link notice -- this is the confirmed hijack path, must never happen")
	}
}

// TestHandler_ReplyOnMappedThread_DeniedForUnownedMember is this Step's
// own SECOND fix-pass regression test for a confirmed re-review finding:
// BEFORE this fix, handleEvent's "existing thread mapping" branch
// (resolveOrClaimSession) returned the resolved session id
// UNCONDITIONALLY -- no domain/authz.Authorize call at all -- so ANY
// resolved actor (including a linked `member` with no ownership/
// participation in the target session) could enqueue a real turn via an
// ordinary reply, unlike the brand-new-thread/ActionCreateSession branch
// this Step's own FIRST fix pass already gated. This proves such a reply
// is now REJECTED: no new turn is added, and the identity itself still
// auto-links (only the state-changing effect is denied) -- mirroring
// TestHandler_AppMention_CreateSessionDeniedForViewer's own "still links,
// still denies the effect" shape for the create-session path.
//
// Also serves as the MEDIUM audit fix's own "denial keeps today's
// behavior" counterpart proof (authz_backend_error_integration_test.go
// proves the OTHER half, a genuine backend error): a real
// ErrActorNotAuthorized denial must still leave the webhook-delivery claim
// un-released and answer 200 -- unlike a genuine backend error, retrying
// via redelivery would just render the identical denial again.
func TestHandler_ReplyOnMappedThread_DeniedForUnownedMember(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unowned-replier@example.com", DisplayName: "Unowned Replier", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-UNOWNED-REPLY", "unowned-replier@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	// Seed an existing mapped thread directly (bypassing the app_mention
	// creation path, to control ownership precisely): a session neither
	// created by NOR joined by matchedUser -- deliberately "unowned" from
	// this actor's own perspective.
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := rig.threads.Claim(ctx, "C0UNOWNEDREPLY", "1700000060.000100", session.ID); err != nil {
		t.Fatalf("claim thread mapping: %v", err)
	}

	const eventID = "Ev0UNOWNEDREPLY001"
	envelope := messageEnvelopeWithUser(eventID, "C0UNOWNEDREPLY", "1700000060.000200", "1700000060.000100", "please continue", "U-UNOWNED-REPLY")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("len(turns) = %d, want 0 (denied by authz, must not add a turn)", len(turns))
	}

	// The identity itself still auto-links -- only the effect is denied.
	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-UNOWNED-REPLY")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v", identity.UserID, matchedUser.ID)
	}

	// MEDIUM audit fix: a genuine denial must NOT release the webhook-
	// delivery claim -- unlike a genuine backend error, redelivering this
	// SAME event_id would just render the identical denial again.
	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack' AND delivery_id = $1`, eventID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 1 {
		t.Errorf("webhook_deliveries row count = %d, want 1 (a genuine denial must leave the claim held, never released)", deliveryRowCount)
	}
}

// TestHandler_ReplyOnMappedThread_AllowedForOwningMember proves the OTHER
// side of the SAME gate this Step's own SECOND fix pass added: a `member`
// who legitimately owns the target session (CreatedBy == the auto-linked
// user) is NOT denied -- the existing-mapping branch's own domain/authz.
// Authorize(ActionPromptSession) verdict passes via the row 2 own/joined
// carve-out, and a real turn is added, exactly like a reply on an
// unlinked/bot-attributed thread already does
// (TestHandler_ReplyOnMappedThread_AddsTurnToSameSession,
// handler_integration_test.go, which this new gate must never regress).
func TestHandler_ReplyOnMappedThread_AllowedForOwningMember(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "owning-replier@example.com", DisplayName: "Owning Replier", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-OWNING-REPLY", "owning-replier@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack, CreatedBy: matchedUser.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := rig.threads.Claim(ctx, "C0OWNINGREPLY", "1700000070.000100", session.ID); err != nil {
		t.Fatalf("claim thread mapping: %v", err)
	}

	envelope := messageEnvelopeWithUser("Ev0OWNINGREPLY001", "C0OWNINGREPLY", "1700000070.000200", "1700000070.000100", "please continue", "U-OWNING-REPLY")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (owning member's reply must add a turn)", len(turns))
	}
}

// TestHandler_AppMention_CreateSessionDeniedForDisabledUser is this Step's
// own SECOND fix-pass regression test for a confirmed re-review finding:
// authorizeResolvedActor resolved a disabled user's ROLE and called
// domain/authz.Authorize with it, but never checked user.Disabled itself
// -- so a disabled `member` (whose role would otherwise permit
// ActionCreateSession) could still create a session via Slack, even
// though auth.Middleware's own Authenticate already rejects that SAME
// disabled user's web session outright (internal/adapters/inbound/auth/
// middleware.go). This proves a disabled member's app_mention is now
// REJECTED exactly like a role-based denial already is
// (TestHandler_AppMention_CreateSessionDeniedForViewer).
func TestHandler_AppMention_CreateSessionDeniedForDisabledUser(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "disabled-mentioner@example.com", DisplayName: "Disabled Mentioner", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	// No UserStore mutation exists for Disabled today (only ListMembers'
	// own read exposure, httpapi/members.go) -- set it directly, mirroring
	// this file's own established precedent of a raw SQL statement where
	// no store method exists yet (e.g. TestWebhookHandler_Prompted_...'s
	// own UPDATE turns, linear/identity_integration_test.go).
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, matchedUser.ID); err != nil {
		t.Fatalf("disable fixture user: %v", err)
	}

	fakeSlack := newFakeSlackWithUsersInfo(t, "U-DISABLED-MENTION", "disabled-mentioner@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	envelope := appMentionEnvelopeWithUser("Ev0DISABLEDDENY001", "C0DISABLEDDENY", "1700000080.000100", "", "please help", "U-DISABLED-MENTION")
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if _, err := rig.threads.Get(ctx, "C0DISABLEDDENY", "1700000080.000100"); err == nil {
		t.Error("expected NO thread mapping row (denied by authz), got one")
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a disabled user must never create a session, even auto-linked with an otherwise-permitting role)", sessionCount)
	}

	// The identity itself still auto-links -- only the effect is denied.
	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-DISABLED-MENTION")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v", identity.UserID, matchedUser.ID)
	}
}

// TestHandler_AppMention_UnknownActorDeniedWithNoSessionCreated is this
// batch's own SECOND review pass's direct-denial test for
// resolveOrClaimSession's own AuthorizeLinkedActor call site (LOW audit
// fix, "5 of the 6 hardened call sites have no DIRECT denial test (only
// indirect coverage via fixture pre-linking)") -- unlike
// TestHandler_AppMention_CreateSessionDeniedForViewer (a RESOLVED but
// insufficiently-privileged `viewer`), this mentioning Slack user's own
// fetched profile email matches NO existing user at all: a genuinely
// never-resolved actor. Proves no session/thread mapping is ever created,
// the identity is never linked (there was nothing to link it to), and the
// magic-link prompt is still minted so the SAME mention can be retried
// once linked.
func TestHandler_AppMention_UnknownActorDeniedWithNoSessionCreated(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-UNKNOWN-MENTION"
	fakeSlack := newFakeSlackWithUsersInfo(t, slackUserID, "nobody-matches@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	envelope := appMentionEnvelopeWithUser("Ev0UNKNOWNMENTION001", "C0UNKNOWNMENTION", "1700000090.000100", "", "please help", slackUserID)
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	if _, err := rig.threads.Get(ctx, "C0UNKNOWNMENTION", "1700000090.000100"); err == nil {
		t.Error("expected NO thread mapping row (denied by authz), got one")
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a genuinely never-resolved actor must never create a session)", sessionCount)
	}

	// Unlike TestHandler_AppMention_CreateSessionDeniedForViewer (a
	// RESOLVED actor, whose identity still auto-links), this actor never
	// resolved at all -- there is no identities row.
	if _, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID); err == nil {
		t.Error("expected NO identities row (never resolved, nothing to link), got one")
	}

	// The magic-link prompt is still sent exactly as before -- only the
	// state-changing effect (the session) is refused.
	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID); err != nil {
		t.Errorf("GetLatestForProviderAndExternalID = %v, want a real link-prompt row", err)
	}
}

// TestHandler_ReplyOnMappedThread_UnknownActorDenied is this batch's own
// SECOND review pass's direct-denial test for authorizeExistingSessionReply
// (LOW audit fix, same finding as
// TestHandler_AppMention_UnknownActorDeniedWithNoSessionCreated above) --
// unlike TestHandler_ReplyOnMappedThread_DeniedForUnownedMember (a RESOLVED
// but unowned `member`), this replying Slack user's own fetched profile
// email matches NO existing user at all: a genuinely never-resolved actor.
// Proves no turn is ever added, the identity is never linked, and the
// webhook-delivery claim is left held (not released) -- exactly like every
// other real denial, never treated as a retryable backend failure.
func TestHandler_ReplyOnMappedThread_UnknownActorDenied(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-UNKNOWN-REPLY"
	fakeSlack := newFakeSlackWithUsersInfo(t, slackUserID, "nobody-matches@example.com")
	auditLog := narvipg.NewAuditLogStore(pool)
	rig := newSlackHandlerRigForIdentityTests(t, pool, fakeSlack, auditLog)

	// Seed an existing mapped thread directly (bypassing the app_mention
	// creation path) -- ownership is irrelevant here, since a genuinely
	// never-resolved actor is denied before ANY own/joined lookup at all
	// (authorizeSessionAction's own top-of-function short-circuit,
	// identity.go).
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := rig.threads.Claim(ctx, "C0UNKNOWNREPLY", "1700000091.000100", session.ID); err != nil {
		t.Fatalf("claim thread mapping: %v", err)
	}

	const eventID = "Ev0UNKNOWNREPLY001"
	envelope := messageEnvelopeWithUser(eventID, "C0UNKNOWNREPLY", "1700000091.000200", "1700000091.000100", "please continue", slackUserID)
	req := signedSlackRequest(t, envelope)
	rec := httptest.NewRecorder()
	rig.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("len(turns) = %d, want 0 (denied by authz, must not add a turn)", len(turns))
	}

	if _, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID); err == nil {
		t.Error("expected NO identities row (never resolved, nothing to link), got one")
	}

	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID); err != nil {
		t.Errorf("GetLatestForProviderAndExternalID = %v, want a real link-prompt row", err)
	}

	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'slack' AND delivery_id = $1`, eventID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 1 {
		t.Errorf("webhook_deliveries row count = %d, want 1 (a genuine denial must leave the claim held, never released)", deliveryRowCount)
	}
}

// TestInteractivityHandler_ViewSubmission_UnknownActorDeniedAndNoticeDelivered
// is this batch's own SECOND review pass's direct-denial test for
// handleViewSubmission (LOW audit fix, "5 of the 6 hardened call sites
// have no DIRECT denial test") -- unlike
// TestInteractivityHandler_ViewSubmission_DeniedForUnownedMember (a
// RESOLVED but unowned `member`), this submitting Slack user's own fetched
// profile email matches NO existing user at all: a genuinely never-resolved
// actor. Proves no request-changes turn is ever created, AND (the LOW audit
// fix, "handleViewSubmission discards the magic-link notice on denial")
// that the magic-link notice IS now delivered via chat.postEphemeral,
// scoped to the submitting user and the plan's own stored Slack message
// ref -- BEFORE this fix, `actorUserID, _ := resolveSlackActorSingleAttempt(...)`
// discarded it outright, leaving this actor with no hint a magic link was
// ever sent.
func TestInteractivityHandler_ViewSubmission_UnknownActorDeniedAndNoticeDelivered(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-UNKNOWN-SUBMITTER"
	fakeSlack, requests := newFakeSlackRecordingWithUsersInfo(t, slackUserID, "nobody-matches-submitter@example.com")
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
	// postViewSubmissionLinkNotice (interactive.go) looks the plan's own
	// stored Slack channel/message-ts back up to scope the ephemeral notice
	// to -- a real Slack-originated plan always has this set
	// (PostPlanApprovalMessage/SetSlackMessageRef), so this mirrors that.
	if err := plans.SetSlackMessageRef(ctx, plan.ID, "C-SUBMITTER", "1700000098.000100"); err != nil {
		t.Fatalf("SetSlackMessageRef: %v", err)
	}

	privateMetadata := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	viewSubmission := map[string]any{
		"type": "view_submission",
		"user": map[string]string{"id": slackUserID},
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
	if !strings.Contains(rec.Body.String(), `"response_action":"errors"`) {
		t.Errorf("body = %s, want a response_action:errors body (denied by authz)", rec.Body.String())
	}

	turnsAfter, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turnsAfter) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn -- a never-resolved actor must never create a turn)", len(turnsAfter))
	}

	if _, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID); err == nil {
		t.Error("expected NO identities row (never resolved, nothing to link), got one")
	}
	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, slackUserID); err != nil {
		t.Errorf("GetLatestForProviderAndExternalID = %v, want a real link-prompt row", err)
	}

	// LOW audit fix ("handleViewSubmission discards the magic-link notice
	// on denial"): the magic-link URL must now actually reach the actor,
	// via chat.postEphemeral, scoped to slackUserID and the plan's own
	// stored channel/message-ts.
	var sawEphemeralWithLink bool
drain:
	for {
		select {
		case got := <-requests:
			if got.path == "/chat.postEphemeral" {
				text, _ := got.body["text"].(string)
				if strings.Contains(text, identitylink.MagicLinkPath) {
					sawEphemeralWithLink = true
					if got.body["channel"] != "C-SUBMITTER" || got.body["user"] != slackUserID {
						t.Errorf("chat.postEphemeral (channel, user) = (%v, %v), want (%q, %q)", got.body["channel"], got.body["user"], "C-SUBMITTER", slackUserID)
					}
				}
			}
		default:
			break drain
		}
	}
	if !sawEphemeralWithLink {
		t.Error("no chat.postEphemeral call carried the magic-link notice -- want it delivered even though this denial has no channel/message field of its own on the inbound payload")
	}
}

// TestInteractivityHandler_BlockActions_ApprovePlan_RetrySucceedsOnceLinked
// is the MEDIUM audit fix's own headline end-to-end regression test (audit-
// fix batch "block unlinked actor state changes", SECOND review pass,
// "no test for the end-to-end 'retry succeeds once linked' guarantee, for
// ANY of the 6 flows") -- prioritized for the Slack plan-decision flow
// specifically, since decideAndUpdateMessage is the ONE call site the SAME
// review pass found the HIGH-severity button-stripping bug in (see
// identity.go's own ErrActorNotLinked doc comment for the full incident).
// Proves the actual, full guarantee this whole batch exists to provide:
//
//  1. A click from an actor who has NEVER been seen before (fetched profile
//     email matches no existing user) is denied with ZERO side effects --
//     the plan stays awaiting_approval, decided_by stays invalid, no new
//     turn is created, and (the HIGH fix's own proof) NO /chat.update call
//     ever fires, so the Approve/Reject buttons are never stripped off the
//     original message. Only an ephemeral notice (carrying the magic-link
//     URL) is delivered.
//  2. The SAME identity is then linked via the REAL magic-link-consume flow
//     -- extracting the nonce from that EXACT magic-link URL text and
//     calling identitylink.Consume directly, mirroring identitylink's own
//     TestConsume_LinksIdentityAndDeletesPrompt (the established helper
//     pattern other tests in this codebase already use for this).
//  3. The IDENTICAL click (same channel/message/plan/session/action) is
//     retried -- and now succeeds: the plan flips to approved, decided_by
//     is the newly-linked user, and a real /chat.update call reflects the
//     outcome on the SAME message the first click's own buttons survived
//     on.
func TestInteractivityHandler_BlockActions_ApprovePlan_RetrySucceedsOnceLinked(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const slackUserID = "U-RETRY-ONCE-LINKED"
	fakeSlack, requests := newFakeSlackRecordingWithUsersInfo(t, slackUserID, "nobody-matches-retry@example.com")
	slackClient := slackapi.New(fakeSlack.Client(), fakeSlack.URL, "test-bot-token")

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	events := narvipg.NewEventStore(pool)
	planDocuments := narvipg.NewPlanDocumentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	identityLinkDeps := newIdentityLinkDepsForTest(pool, auditLog)

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
		IdentityLink:        identityLinkDeps,
		Participants:        narvipg.NewParticipantStore(pool),
		SigningSecret:       testSigningSecret,
		Timeouts:            platform.DefaultTimeouts(),
	})

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

	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, "C-RETRY", "1700000099.000100", "trigger-retry", slackUserID)

	// --- First click: NOT yet linked -- must be denied with ZERO side
	// effects, and the Approve/Reject buttons must be left untouched.
	req := signedInteractivityRequest(t, payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first click: status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	afterFirstClick, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan after first click: %v", err)
	}
	if afterFirstClick.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Fatalf("Status after first click = %v, want %v (not-yet-linked actor must be denied)", afterFirstClick.Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if afterFirstClick.DecidedBy.Valid {
		t.Fatalf("DecidedBy after first click = %v, want invalid", afterFirstClick.DecidedBy)
	}
	turnsAfterFirstClick, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns after first click: %v", err)
	}
	if len(turnsAfterFirstClick) != 1 {
		t.Fatalf("len(turns) after first click = %d, want 1 (the seeded producing turn only -- zero side effects)", len(turnsAfterFirstClick))
	}

	// HIGH audit fix's own headline proof: no /chat.update call ever fired
	// for the first click -- the Approve/Reject buttons must still be on
	// the original message, retryable, never stripped.
	var magicLinkURL string
drainFirst:
	for {
		select {
		case got := <-requests:
			if got.path == "/chat.update" {
				t.Errorf("first click: unexpected /chat.update call (body=%v) -- this would strip the Approve/Reject buttons off a message a not-yet-linked actor still needs to retry", got.body)
			}
			if got.path == "/chat.postEphemeral" {
				if text, _ := got.body["text"].(string); strings.Contains(text, identitylink.MagicLinkPath) {
					magicLinkURL = text[strings.Index(text, "https://"):]
				}
			}
		default:
			break drainFirst
		}
	}
	if magicLinkURL == "" {
		t.Fatal("first click: no ephemeral notice carried a magic-link URL")
	}
	nonce := magicLinkURL[len(identityLinkDeps.PublicBaseURL+identitylink.MagicLinkPath):]

	// --- Link the SAME identity via the REAL magic-link-consume flow.
	linkedUser, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "retry-once-linked@example.com", DisplayName: "Retry Once Linked", Role: sqlcgen.UserRoleMaintainer,
	})
	if err != nil {
		t.Fatalf("create fixture user to link: %v", err)
	}
	if _, err := identitylink.Consume(ctx, identityLinkDeps, nonce, linkedUser.ID); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	// --- Retry: the IDENTICAL click now succeeds.
	req2 := signedInteractivityRequest(t, payload)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, want %d; body = %s", rec2.Code, http.StatusOK, rec2.Body.String())
	}

	finalPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan after retry: %v", err)
	}
	if finalPlan.Status != sqlcgen.PlanStatusApproved {
		t.Errorf("Status after retry = %v, want %v (the identical click must now succeed once linked)", finalPlan.Status, sqlcgen.PlanStatusApproved)
	}
	if !finalPlan.DecidedBy.Valid || finalPlan.DecidedBy != linkedUser.ID {
		t.Errorf("DecidedBy after retry = %v, want %v (the newly-linked user)", finalPlan.DecidedBy, linkedUser.ID)
	}
	turnsAfterRetry, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns after retry: %v", err)
	}
	if len(turnsAfterRetry) != 2 {
		t.Errorf("len(turns) after retry = %d, want 2 (the seeded producing turn + the new implementation turn)", len(turnsAfterRetry))
	}

	sawChatUpdateAfterRetry := false
drainSecond:
	for {
		select {
		case got := <-requests:
			if got.path == "/chat.update" {
				sawChatUpdateAfterRetry = true
			}
		default:
			break drainSecond
		}
	}
	if !sawChatUpdateAfterRetry {
		t.Error("retry: expected a /chat.update call reflecting the now-successful decision")
	}
}

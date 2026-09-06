//go:build integration

// Integration tests proving §13.2's ("identities + full RBAC", §13.2)
// own auto-linking wiring actually fires from a REAL POST /webhooks/linear
// request -- mirrors webhook_integration_test.go's own conventions
// exactly (testcontainers Postgres, a real linear.NewWebhookHandler,
// synthetic real-shaped payloads), plus a real httptest.Server standing in
// for Linear's own GraphQL API (the user(id) { email } query
// GetUserEmail calls).
package linear_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/platform"
)

// newLinearGraphQLStub stands in for Linear's real GraphQL API, answering
// EVERY request's own "user(id) { email }" query with email regardless of
// the requested id -- these tests only ever ask about one id at a time,
// so this stub does not bother inspecting the request body.
func newLinearGraphQLStub(t *testing.T, email string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"user": map[string]any{"email": email}},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// installLinearFixture upserts a linear_installations row for
// organizationID so decryptLinearAccessToken (identity.go) succeeds --
// mirrors internal/adapters/inbound/linear/callback.go's own real
// EncryptToken-then-Upsert sequencing.
func installLinearFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, organizationID string, tokenEncryptionKey []byte) {
	t.Helper()
	encrypted, err := platform.EncryptToken(tokenEncryptionKey, []byte("fake-access-token"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	installations := narvipg.NewLinearInstallationStore(pool)
	if _, err := installations.Upsert(ctx, sqlcgen.UpsertLinearInstallationParams{
		OrganizationID:       organizationID,
		AppUserID:            "app-user-1",
		AccessTokenEncrypted: encrypted,
		ExpiresAt:            pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("Upsert installation: %v", err)
	}
}

// agentSessionCreatedPayloadWithCreator mirrors agentSessionCreatedPayload
// (webhook_integration_test.go) but also sets creatorId -- the field this
// Step's own auto-linking wiring reads (payload.go's own
// AgentSession.CreatorID).
func agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, creatorID string) []byte {
	body := fmt.Sprintf(`{
		"action": "created",
		"type": "AgentSessionEvent",
		"organizationId": %q,
		"webhookTimestamp": %d,
		"agentSession": {
			"id": %q,
			"creatorId": %q,
			"issue": {"identifier": "ENG-1", "title": "A fixture issue"},
			"url": "https://linear.app/narvi/issue/ENG-1"
		},
		"promptContext": "context"
	}`, organizationID, time.Now().UnixMilli(), agentSessionID, creatorID)
	return []byte(body)
}

// agentSessionPromptedPayloadWithUser mirrors handlePrompted's own real
// wire shape, setting agentActivity.userId -- the field this Step's own
// auto-linking wiring reads for a REPLY (payload.go's own
// AgentActivity.UserID), distinct from the session's own creatorId.
func agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, userID, body string) []byte {
	payload := fmt.Sprintf(`{
		"action": "prompted",
		"type": "AgentSessionEvent",
		"organizationId": %q,
		"webhookTimestamp": %d,
		"agentSession": {"id": %q},
		"agentActivity": {
			"userId": %q,
			"content": {"type": "prompt", "body": %q}
		}
	}`, organizationID, time.Now().UnixMilli(), agentSessionID, userID, body)
	return []byte(payload)
}

// TestWebhookHandler_Created_AutoLinksUniqueEmailMatch proves a `created`
// event whose creatorId's fetched profile email matches EXACTLY one
// existing user auto-links it and attributes the new session's own
// created_by to that user (not bot attribution) -- §13.2 steps 1-3.
func TestWebhookHandler_Created_AutoLinksUniqueEmailMatch(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)

	organizationID := "org-autolink-1"
	tokenEncryptionKey := deps.TokenEncryptionKey
	installLinearFixture(context.Background(), t, pool, organizationID, tokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "matched@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(context.Background(), sqlcgen.CreateUserParams{
		PrimaryEmail: "matched@example.com", DisplayName: "Matched", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	deps.IdentityLink = identitylink.Deps{
		Pool:          pool,
		Users:         users,
		Identities:    narvipg.NewIdentityStore(pool),
		LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:      deps.AuditLog,
		PublicBaseURL: "https://narvi.example.com",
		PromptTTL:     time.Hour,
	}

	handler := linear.NewWebhookHandler(deps)

	agentSessionID := "agent-session-autolink-1"
	body := agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, "linear-creator-1")

	rec := postWebhook(t, handler, body, "delivery-autolink-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ctx := context.Background()
	var createdBy string
	if err := pool.QueryRow(ctx,
		`SELECT created_by::text FROM sessions WHERE spawn_source = 'linear' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&createdBy); err != nil {
		t.Fatalf("query session created_by: %v", err)
	}
	if createdBy != matchedUser.ID.String() {
		t.Errorf("session created_by = %q, want %q (the auto-linked user, not bot attribution)", createdBy, matchedUser.ID.String())
	}

	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, "linear-creator-1")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail {
		t.Errorf("LinkedVia = %v, want auto_email", identity.LinkedVia)
	}
}

// TestWebhookHandler_Prompted_UnknownUserCreatesLinkPromptAndIsDenied is the
// audit-fix batch's own regression test for the finding this batch closes
// ("block unlinked actor state changes", docs/TECHNICAL_PLAN.md §13.2): a
// `prompted` reply from a NEVER-BEFORE-SEEN Linear user whose fetched email
// matches no one is now DENIED -- the reply must NOT create a turn -- while
// still leaving a link-prompt row behind (§13.2 step 4) so the SAME magic
// link is delivered exactly as before. This test's own PREVIOUS name/
// assertions (TestWebhookHandler_Prompted_UnknownUserCreatesLinkPromptButStillCreatesTurn)
// described the OLD, now user-decided-hardened-away behavior ("...But Still
// Creates Turn") -- renamed and rewritten to prove the inverted outcome:
// same link prompt, NO turn.
func TestWebhookHandler_Prompted_UnknownUserCreatesLinkPromptAndIsDenied(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)

	organizationID := "org-prompt-1"
	installLinearFixture(context.Background(), t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "nobody-matches@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	deps.IdentityLink = identitylink.Deps{
		Pool:          pool,
		Users:         narvipg.NewUserStore(pool),
		Identities:    narvipg.NewIdentityStore(pool),
		LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:      deps.AuditLog,
		PublicBaseURL: "https://narvi.example.com",
		PromptTTL:     time.Hour,
	}

	handler := linear.NewWebhookHandler(deps)

	agentSessionID := "agent-session-prompt-1"

	// First: `created`, no creatorId (automation-initiated) -- keeps this
	// test focused purely on the `prompted` reply's own actor resolution.
	createdBody := []byte(fmt.Sprintf(`{
		"action": "created",
		"type": "AgentSessionEvent",
		"organizationId": %q,
		"webhookTimestamp": %d,
		"agentSession": {"id": %q},
		"promptContext": "context"
	}`, organizationID, time.Now().UnixMilli(), agentSessionID))
	rec := postWebhook(t, handler, createdBody, "delivery-prompt-created-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("created status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The `created` event's own initial turn stays 'pending' in this test
	// (no real sandbox provider is wired, so nothing ever dispatches it) --
	// mark it 'completed' so the `prompted` event below can create its OWN
	// turn instead of being dropped by the EXISTING (unrelated to this
	// Step) "one open turn per session" precondition (webhook.go's own
	// hasOpenTurn), keeping this test's own focus on identity resolution,
	// not turn-lifecycle mechanics.
	if _, err := pool.Exec(context.Background(),
		`UPDATE turns SET status = 'completed' WHERE session_id = (SELECT session_id FROM linear_agent_sessions WHERE agent_session_id = $1)`,
		agentSessionID,
	); err != nil {
		t.Fatalf("mark fixture turn completed: %v", err)
	}

	promptedBody := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, "linear-unknown-user-1", "please also fix the tests")
	rec = postWebhook(t, handler, promptedBody, "delivery-prompt-prompted-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("prompted status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ctx := context.Background()
	var turnCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns`).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	// Audit-fix batch update: the reply is now DENIED -- only the `created`
	// event's own initial turn exists; the prompted reply must never create
	// a second one.
	if turnCount != 1 {
		t.Errorf("turnCount = %d, want 1 (only the created event's own initial turn -- the prompted reply must be denied, never create a turn)", turnCount)
	}

	// The magic-link prompt is still sent exactly as before -- only the
	// state-changing effect (the turn) is now refused.
	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, "linear-unknown-user-1"); err != nil {
		t.Errorf("GetLatestForProviderAndExternalID = %v, want a real link-prompt row", err)
	}
}

// TestWebhookHandler_Created_UnknownActorDeniedWithNoSessionCreated is this
// batch's own SECOND review pass's direct-denial test for handleCreated's
// own AuthorizeLinkedActor call site (LOW audit fix, "5 of the 6 hardened
// call sites have no DIRECT denial test (only indirect coverage via
// fixture pre-linking)") -- mirrors
// TestWebhookHandler_Prompted_UnknownUserCreatesLinkPromptAndIsDenied's own
// shape exactly (a `created` event whose creatorId's fetched profile email
// matches no existing user at all -- a genuinely NEVER-resolved actor, not
// merely a resolved-but-insufficient-role one, unlike
// TestWebhookHandler_Created_DeniedForViewer above), proving: no session is
// ever created, the identity itself is NOT linked (there was nothing to
// link it to), and the magic-link prompt is still minted so the same
// delegation can be retried once a real Narvi account is linked.
func TestWebhookHandler_Created_UnknownActorDeniedWithNoSessionCreated(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)

	organizationID := "org-created-unknown-1"
	installLinearFixture(context.Background(), t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "nobody-matches@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	handler := linear.NewWebhookHandler(deps)

	agentSessionID := "agent-session-created-unknown-1"
	const creatorID = "linear-created-unknown-1"
	body := agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, creatorID)

	rec := postWebhook(t, handler, body, "delivery-created-unknown-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ctx := context.Background()
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a genuinely never-resolved actor must never create a session)", sessionCount)
	}

	// Unlike TestWebhookHandler_Created_DeniedForViewer (a RESOLVED but
	// insufficiently-privileged actor, whose identity still auto-links),
	// this actor never resolved at all -- there is no identities row.
	if _, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, creatorID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetByProviderAndExternalID = %v, want pgx.ErrNoRows (never resolved, nothing to link)", err)
	}

	// The magic-link prompt is still sent exactly as before -- only the
	// state-changing effect (the session) is refused.
	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, creatorID); err != nil {
		t.Errorf("GetLatestForProviderAndExternalID = %v, want a real link-prompt row", err)
	}
}

// newIdentityLinkDepsForTest mirrors internal/adapters/inbound/slack's own
// identical helper (identity_integration_test.go, that package) -- built
// once here since this file's own earlier tests each construct an
// equivalent identitylink.Deps value inline instead.
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

// linkLinearIdentityForTest links externalID directly to a NEW fixture
// Narvi user (role) via identities.Create -- bypassing any profile-email
// fetch entirely: identitylink.Resolve's own fast path
// (GetByProviderAndExternalID) always wins first, mirroring
// internal/adapters/inbound/slack's own identical linkSlackIdentityForTest
// (handler_integration_test.go, that package).
//
// Audit-fix batch addition ("block unlinked actor state changes"): resolveActor
// (identity.go) ALSO requires decryptLinearAccessToken to succeed before it
// ever consults an already-linked identity at all -- so a caller wiring this
// in for a test must ALSO have called installLinearFixture for organizationID
// and given deps a reachable LinearClient (its response content is
// irrelevant once the identity is already linked, since Resolve's own fast
// path never inspects it -- see newGenericLinearGraphQLStub,
// turnconsolidation_integration_test.go, for a stub answering both possible
// GraphQL call shapes generically).
func linkLinearIdentityForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, externalID string, role sqlcgen.UserRole) sqlcgen.User {
	t.Helper()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: externalID + "@narvi-test.example.com",
		DisplayName:  externalID,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("create fixture user for %s: %v", externalID, err)
	}
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     user.ID,
		Provider:   sqlcgen.IdentityProviderLinear,
		ExternalID: externalID,
		LinkedVia:  sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("link fixture identity for %s: %v", externalID, err)
	}
	return user
}

// TestWebhookHandler_Created_DeniedForViewer is this Step's own regression
// test for a confirmed security review finding: BEFORE this fix, a
// `created` AgentSessionEvent whose creatorId auto-links to a REAL Narvi
// user (any role) unconditionally created the backing session --
// domain/authz.Authorize was never consulted. This proves a `viewer`'s
// auto-linked account is now REJECTED: no session is ever created for
// this agent session at all -- exactly like the REST /api/sessions
// endpoint already rejects a viewer's own CreateSession call
// (domain/authz's own matrix: ActionCreateSession has no viewer entry).
// The identity itself still auto-links (only the state-changing effect is
// refused).
func TestWebhookHandler_Created_DeniedForViewer(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)

	organizationID := "org-viewer-create-1"
	tokenEncryptionKey := deps.TokenEncryptionKey
	installLinearFixture(context.Background(), t, pool, organizationID, tokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "viewer-creator@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(context.Background(), sqlcgen.CreateUserParams{
		PrimaryEmail: "viewer-creator@example.com", DisplayName: "Viewer Creator", Role: sqlcgen.UserRoleViewer,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	handler := linear.NewWebhookHandler(deps)

	agentSessionID := "agent-session-viewer-create-1"
	body := agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, "linear-viewer-creator-1")

	rec := postWebhook(t, handler, body, "delivery-viewer-create-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ctx := context.Background()
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a viewer must never create a session, even auto-linked)", sessionCount)
	}

	// The identity itself still auto-links -- only the effect is denied.
	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, "linear-viewer-creator-1")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v", identity.UserID, matchedUser.ID)
	}
}

// TestWebhookHandler_PlanVerdict_DeniedForUnownedMember proves
// handlePlanVerdict (an `approve`/`reject` keyword reply) is denied for an
// auto-linked `member` who neither created nor joined the target session
// -- the plan stays awaiting_approval, decided_by stays NULL, exactly
// like the REST approve/reject endpoints already behave for the identical
// (role, ownership) combination (canActOnPlan, httpapi/planauthz.go).
//
// Also serves as the LOW audit fix's own "denial keeps today's behavior"
// counterpart proof (authz_backend_error_integration_test.go's
// TestWebhookHandler_PlanVerdict_AuthzBackendErrorReleasesClaim proves the
// OTHER half, a genuine backend error): a real ErrActorNotAuthorized
// denial from handlePlanVerdict must still leave the webhook-delivery
// claim un-released and answer 200 -- unlike a genuine backend error,
// retrying via redelivery would just render the identical denial again.
func TestWebhookHandler_PlanVerdict_DeniedForUnownedMember(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	organizationID := "org-plan-unowned-1"
	installLinearFixture(context.Background(), t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "unowned-decider@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(context.Background(), sqlcgen.CreateUserParams{
		PrimaryEmail: "unowned-decider@example.com", DisplayName: "Unowned Decider", Role: sqlcgen.UserRoleMember,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	handler := linear.NewWebhookHandler(deps)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-unowned-1"

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)

	// Deliberately NO CreatedBy, no participants row.
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
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	const deliveryID = "delivery-plan-unowned-1"
	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, "linear-unowned-decider-1", "approve")
	rec := postWebhook(t, handler, body, deliveryID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	var decidedBy pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus, &decidedBy); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (denied by authz, must not decide)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}
	if decidedBy.Valid {
		t.Errorf("decided_by = %v, want invalid (denied -- never decided)", decidedBy)
	}

	// LOW audit fix: a genuine denial must NOT release the webhook-delivery
	// claim -- unlike a genuine backend error, redelivering this SAME
	// delivery id would just render the identical denial again.
	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'linear' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 1 {
		t.Errorf("webhook_deliveries row count = %d, want 1 (a genuine denial must leave the claim held, never released)", deliveryRowCount)
	}
}

// TestWebhookHandler_Prompted_DeniedForUnownedMember proves the ordinary
// reply ("request changes" for Linear, handlePrompted's own top doc
// comment) fallthrough is likewise denied for an auto-linked `member` with
// no ownership/participation in the target session: no new turn is
// created.
//
// Also serves as the MEDIUM audit fix's own "denial keeps today's
// behavior" counterpart proof (authz_backend_error_integration_test.go
// proves the OTHER half, a genuine backend error): a real
// ErrActorNotAuthorized denial must still leave the webhook-delivery claim
// un-released and answer 200 -- unlike a genuine backend error, retrying
// via redelivery would just render the identical denial again.
func TestWebhookHandler_Prompted_DeniedForUnownedMember(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	organizationID := "org-prompt-unowned-1"
	installLinearFixture(context.Background(), t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "unowned-prompter@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(context.Background(), sqlcgen.CreateUserParams{
		PrimaryEmail: "unowned-prompter@example.com", DisplayName: "Unowned Prompter", Role: sqlcgen.UserRoleMember,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	handler := linear.NewWebhookHandler(deps)

	ctx := context.Background()
	agentSessionID := "agent-session-prompt-unowned-1"

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)

	// Deliberately NO CreatedBy, no participants row.
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
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}

	const deliveryID = "delivery-prompt-unowned-1"
	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, "linear-unowned-prompter-1", "please also fix the tests")
	rec := postWebhook(t, handler, body, deliveryID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn -- denied reply must never create a turn)", len(allTurns))
	}
	if allTurns[0].ID != producingTurn.ID {
		t.Errorf("remaining turn = %v, want the seeded producing turn %v", allTurns[0].ID, producingTurn.ID)
	}

	// MEDIUM audit fix: a genuine denial must NOT release the webhook-
	// delivery claim -- unlike a genuine backend error, redelivering this
	// SAME delivery id would just render the identical denial again.
	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'linear' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 1 {
		t.Errorf("webhook_deliveries row count = %d, want 1 (a genuine denial must leave the claim held, never released)", deliveryRowCount)
	}
}

// TestWebhookHandler_Created_DeniedForDisabledUser is this Step's own
// SECOND fix-pass regression test for a confirmed re-review finding:
// authorizeResolvedActor resolved a disabled user's ROLE and called
// domain/authz.Authorize with it, but never checked user.Disabled itself
// -- so a disabled `member` (whose role would otherwise permit
// ActionCreateSession) could still create a session via Linear, even
// though auth.Middleware's own Authenticate already rejects that SAME
// disabled user's web session outright (internal/adapters/inbound/auth/
// middleware.go). This proves a disabled creator's `created` event is now
// REJECTED exactly like a role-based denial already is
// (TestWebhookHandler_Created_DeniedForViewer).
func TestWebhookHandler_Created_DeniedForDisabledUser(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)

	organizationID := "org-disabled-create-1"
	tokenEncryptionKey := deps.TokenEncryptionKey
	installLinearFixture(context.Background(), t, pool, organizationID, tokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "disabled-creator@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(context.Background(), sqlcgen.CreateUserParams{
		PrimaryEmail: "disabled-creator@example.com", DisplayName: "Disabled Creator", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	// No UserStore mutation exists for Disabled today (only ListMembers'
	// own read exposure, httpapi/members.go) -- set it directly, mirroring
	// this file's own established precedent of a raw SQL statement where
	// no store method exists yet (e.g. this file's own UPDATE turns,
	// TestWebhookHandler_Prompted_UnknownUserCreatesLinkPromptAndIsDenied
	// above).
	if _, err := pool.Exec(context.Background(), `UPDATE users SET disabled = true WHERE id = $1`, matchedUser.ID); err != nil {
		t.Fatalf("disable fixture user: %v", err)
	}

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	handler := linear.NewWebhookHandler(deps)

	agentSessionID := "agent-session-disabled-create-1"
	body := agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, "linear-disabled-creator-1")

	rec := postWebhook(t, handler, body, "delivery-disabled-create-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ctx := context.Background()
	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'linear'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a disabled user must never create a session, even auto-linked with an otherwise-permitting role)", sessionCount)
	}

	// The identity itself still auto-links -- only the effect is denied.
	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, "linear-disabled-creator-1")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v", identity.UserID, matchedUser.ID)
	}
}

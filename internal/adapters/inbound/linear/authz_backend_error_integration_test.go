//go:build integration

// Integration test for the MEDIUM audit fix ("authorizeSessionAction
// conflates a genuine backend error with a real authorization denial",
// internal/adapters/inbound/linear/identity.go): a transient backend
// failure encountered WHILE checking authorization must be distinguished
// from a genuine denial -- the former now flows into the SAME
// release-the-claim-and-retry path H2 already wired up for every other
// post-claim failure, rather than being silently treated as "skip, no
// release" the way a one-off DB blip previously was. Mirrors
// identity_integration_test.go's own conventions (a real, auto-linkable
// fixture user, a real linear.NewWebhookHandler) exactly, except
// deps.Sessions is built on a pool that's already been closed -- every
// call through it fails deterministically (pgxpool.ErrClosedPool), with no
// timing dependency, simulating "deps.Sessions.Get hitting a dropped
// connection" (authorizeSessionAction's own ErrActorNotAuthorized doc
// comment) without needing an actual dropped network connection.
package linear_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestWebhookHandler_Prompted_AuthzBackendErrorReleasesClaim is the MEDIUM
// audit fix's own headline proof: a genuine backend failure INSIDE
// authorizeSessionAction (deps.Sessions.Get erroring for a reason having
// nothing to do with the actor's own authorization) must NOT be silently
// conflated with a real denial. Before this fix, authorizeSessionAction's
// own bare bool made the two indistinguishable: the claim was never
// released, the webhook answered 200, and the reply was silently dropped
// forever with no chance of a redelivery ever retrying it. This proves the
// SAME release-the-claim-and-answer-non-2xx path H2 already wired up for
// every other post-claim failure now fires here too.
func TestWebhookHandler_Prompted_AuthzBackendErrorReleasesClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	organizationID := "org-prompt-backend-error-1"
	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "backend-error-prompter@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "backend-error-prompter@example.com", DisplayName: "Backend Error Prompter", Role: sqlcgen.UserRoleMember,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	agentSessionID := "agent-session-prompt-backend-error-1"

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
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
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
	deps.Sessions = narvipg.NewSessionStore(brokenPool)

	handler := linear.NewWebhookHandler(deps)

	const deliveryID = "delivery-prompt-backend-error-1"
	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, "linear-backend-error-prompter-1", "please also fix the tests")
	rec := postWebhook(t, handler, body, deliveryID)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (a genuine backend failure during the authz check, not a denial); body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'linear' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 0 {
		t.Errorf("webhook_deliveries row count = %d, want 0 (the claim must be released so a redelivery can retry)", deliveryRowCount)
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn -- must not have proceeded past the failed authz check)", len(allTurns))
	}
	if allTurns[0].ID != producingTurn.ID {
		t.Errorf("remaining turn = %v, want the seeded producing turn %v", allTurns[0].ID, producingTurn.ID)
	}
}

// TestWebhookHandler_PlanVerdict_AuthzBackendErrorReleasesClaim is the LOW
// audit fix's own headline proof (second review pass, "handlePlanVerdict
// has the same conflation, explicitly left out of the first fix's
// scope"): a genuine backend failure INSIDE handlePlanVerdict's own
// authorizeSessionAction call (deps.Sessions.Get erroring for a reason
// having nothing to do with the actor's own authorization) must NOT be
// silently conflated with a real denial. Before this fix, handlePlanVerdict
// had no return value at all: a backend error here posted the same
// misleading "you don't have permission" message a real denial would, AND
// the webhook-delivery claim was never released, so no redelivery could
// ever retry the actual plan decision. This proves handlePlanVerdict now
// propagates ok=false up through handlePrompted into the SAME
// release-the-claim-and-answer-non-2xx path H2/the MEDIUM fix already
// wired up for the ordinary-reply gate -- mirrors
// TestWebhookHandler_Prompted_AuthzBackendErrorReleasesClaim above exactly,
// except a seeded awaiting_approval plan and an "approve" keyword reply
// route this request into handlePlanVerdict instead of the ordinary
// create-turn fallthrough.
func TestWebhookHandler_PlanVerdict_AuthzBackendErrorReleasesClaim(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	organizationID := "org-plan-backend-error-1"
	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub := newLinearGraphQLStub(t, "backend-error-decider@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "backend-error-decider@example.com", DisplayName: "Backend Error Decider", Role: sqlcgen.UserRoleMember,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	auditLog := narvipg.NewAuditLogStore(pool)
	deps.AuditLog = auditLog
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, auditLog)

	agentSessionID := "agent-session-plan-backend-error-1"

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
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
	producingTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
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
	deps.Sessions = narvipg.NewSessionStore(brokenPool)

	handler := linear.NewWebhookHandler(deps)

	const deliveryID = "delivery-plan-backend-error-1"
	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, "linear-backend-error-decider-1", "approve")
	rec := postWebhook(t, handler, body, deliveryID)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (a genuine backend failure during the authz check, not a denial); body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	var deliveryRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE provider = 'linear' AND delivery_id = $1`, deliveryID,
	).Scan(&deliveryRowCount); err != nil {
		t.Fatalf("count webhook_deliveries: %v", err)
	}
	if deliveryRowCount != 0 {
		t.Errorf("webhook_deliveries row count = %d, want 0 (the claim must be released so a redelivery can retry)", deliveryRowCount)
	}

	// The plan must be left exactly as it was -- the failed authz check
	// must never have reached httpapi.DecidePlan at all.
	updatedPlan, err := plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if updatedPlan.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("Status = %v, want %v (must not have proceeded past the failed authz check)", updatedPlan.Status, sqlcgen.PlanStatusAwaitingApproval)
	}
	if updatedPlan.DecidedBy.Valid {
		t.Errorf("DecidedBy = %v, want invalid", updatedPlan.DecidedBy)
	}
}

//go:build integration

// This file proves §8.1's ("plan mode, cross-channel", §8.1/§13.3) own
// Linear text-verdict parsing: handlePrompted's new check, ahead of its
// existing unconditional turn-creation, against a REAL Postgres instance
// -- mirrors webhook_integration_test.go's own newTestPool/newHandlerDeps/
// postWebhook conventions exactly (same package, same file's own helpers
// reused directly).
package linear_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestWebhookHandler_Prompted_ApproveKeyword_DecidesPlan proves a
// deterministic approve-keyword reply calls the shared decide-plan path
// instead of creating an ordinary turn: the plan flips to 'approved' and a
// new implementation turn is created -- exactly the SAME outcome the Slack
// button / REST endpoint produce.
//
// Audit-fix batch update ("block unlinked actor state changes"): the
// replying Linear user id is now pre-linked (linkLinearIdentityForTest) to
// a real, RoleMaintainer fixture user -- an unresolved actor is denied
// outright now, so this test (never actually ABOUT identity resolution)
// must exercise a genuinely linked, authorized one to keep proving what it
// always meant to prove (the approve-keyword decide-plan mechanics).
// decided_by is consequently that REAL user, not NULL.
func TestWebhookHandler_Prompted_ApproveKeyword_DecidesPlan(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-approve"
	organizationID := "org-plan-approve"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, _ := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const deciderID = "linear-planverdict-approve-1"
	decider := linkLinearIdentityForTest(ctx, t, pool, deciderID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

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

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, deciderID, "approve")
	rec := postWebhook(t, handler, body, "delivery-plan-approve")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	var decidedBy pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus, &decidedBy); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusApproved {
		t.Errorf("db status = %q, want %q", dbStatus, sqlcgen.PlanStatusApproved)
	}
	if !decidedBy.Valid || decidedBy != decider.ID {
		t.Errorf("decided_by = %v, want %v (the pre-linked fixture actor)", decidedBy, decider.ID)
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (the seeded producing turn + the new implementation turn -- the reply must NOT have created a third, ordinary turn)", len(allTurns))
	}
}

// TestWebhookHandler_Prompted_NonKeywordNonReviseText_BlockedByAwaitingPlan
// is this batch's own regression test for the follow-up fix
// (§8.1): a reply matching NEITHER a verdict keyword NOR the revise:
// prefix, while a plan IS awaiting approval, must be BLOCKED -- not
// dispatched as an ordinary build turn the human never approved (the exact
// gap this fix closes). Supersedes this file's own PREVIOUS
// TestWebhookHandler_Prompted_NonKeywordText_FallsThroughToOrdinaryTurn,
// which encoded the bug itself as the expected behavior ("falls through to
// the EXISTING create-turn behavior... no change to that half") -- that is
// now precisely wrong. Proves: the plan's own status is untouched, NO new
// turn is created, and the honest planAwaitingApprovalReplyText reply is
// posted back to the thread.
func TestWebhookHandler_Prompted_NonKeywordNonReviseText_BlockedByAwaitingPlan(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-feedback"
	organizationID := "org-plan-feedback"

	// Audit-fix batch update ("block unlinked actor state changes"): the
	// replying Linear user id must now be pre-linked -- an unresolved actor
	// is denied outright, so this test (never actually ABOUT identity
	// resolution) must exercise a genuinely linked, authorized one to keep
	// proving what it always meant to prove (the awaiting-plan gate itself).
	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, recordedBodies := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const replierID = "linear-planverdict-feedback-1"
	linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

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

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "please keep the env fallback path")
	rec := postWebhook(t, handler, body, "delivery-plan-feedback")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (a non-keyword reply must never decide the plan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (the seeded producing turn only -- the gate must block the ordinary reply from ever creating a second one)", len(allTurns))
	}

	var gotHonestReply bool
	for _, b := range recordedBodies() {
		if strings.Contains(b, "awaiting your approval") {
			gotHonestReply = true
		}
	}
	if !gotHonestReply {
		t.Error("no outbound activity contained the awaiting-plan honest reply")
	}
}

// TestWebhookHandler_Prompted_RevisePrefix_CreatesPlanModeTurnWithStrippedFeedback
// is this batch's own regression test for the deterministic revise: path
// (a follow-up fix, §8.1): a reply STARTING with the revise:
// prefix, while a plan is awaiting approval, creates a REAL plan_mode=true
// turn carrying the stripped feedback as its prompt -- the already-
// documented request-changes flow, newly reachable from chat (before this
// fix, chat users had no way to request changes at all: any reply became a
// rogue plan_mode=false build turn).
func TestWebhookHandler_Prompted_RevisePrefix_CreatesPlanModeTurnWithStrippedFeedback(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-revise"
	organizationID := "org-plan-revise"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, _ := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const replierID = "linear-planverdict-revise-1"
	linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

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

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "revise: drop the retry logic")
	rec := postWebhook(t, handler, body, "delivery-plan-revise")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (a revise reply must never itself decide the plan)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (the seeded producing turn + the new plan_mode=true revise turn)", len(allTurns))
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
	if !newTurn.PlanMode {
		t.Error("new turn PlanMode = false, want true")
	}
	if newTurn.Prompt == nil || *newTurn.Prompt != "drop the retry logic" {
		t.Errorf("new turn prompt = %v, want %q (the revise: prefix must be stripped)", newTurn.Prompt, "drop the retry logic")
	}
}

// TestWebhookHandler_Prompted_ApproveKeyword_UnknownActorDenied is this
// batch's own SECOND review pass's direct-denial test for handlePlanVerdict
// itself (LOW audit fix, "5 of the 6 hardened call sites have no DIRECT
// denial test") -- unlike TestWebhookHandler_PlanVerdict_DeniedForUnownedMember
// (identity_integration_test.go, a RESOLVED-but-unowned `member`), this
// decider's own fetched profile email matches NO existing user at all: a
// genuinely never-resolved actor. Proves the plan stays awaiting_approval,
// decided_by stays NULL, the identity is never linked, and the magic-link
// prompt is still minted so the SAME "approve" reply can be retried once
// linked.
func TestWebhookHandler_Prompted_ApproveKeyword_UnknownActorDenied(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-approve-unknown"
	organizationID := "org-plan-approve-unknown"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	graphqlStub := newLinearGraphQLStub(t, "nobody-matches@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const deciderID = "linear-planverdict-approve-unknown-1"

	handler := linear.NewWebhookHandler(deps)

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

	body := agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, deciderID, "approve")
	rec := postWebhook(t, handler, body, "delivery-plan-approve-unknown")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	var decidedBy pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus, &decidedBy); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (a genuinely never-resolved actor must never decide)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}
	if decidedBy.Valid {
		t.Errorf("decided_by = %v, want invalid", decidedBy)
	}

	allTurns, err := turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(allTurns) != 1 {
		t.Errorf("len(turns) = %d, want 1 (only the seeded producing turn)", len(allTurns))
	}

	linkPrompts := narvipg.NewIdentityLinkPromptStore(pool)
	if _, err := linkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, deciderID); err != nil {
		t.Errorf("GetLatestForProviderAndExternalID = %v, want a real link-prompt row", err)
	}
}

// Cross-channel "already decided" honesty (this Step's own point 5) is
// proved two ways elsewhere, deliberately NOT re-derived as a third,
// timing-sensitive integration test here: (1) renderLinearPlanOutcomeText
// itself is table-driven unit-tested (planverdict_test.go, same package,
// no DB/network) over every DecidePlanOutcome shape handlePlanVerdict can
// ever observe, won or lost; (2) the actual cross-channel RACE this text
// reacts to -- two different verdicts concurrently deciding the SAME
// plan, exactly one winning -- is proved at the shared httpapi.DecidePlan
// level by TestDecidePlan_FirstWinsAcrossChannels_ApproveVsReject
// (internal/adapters/inbound/httpapi), which handlePlanVerdict calls
// UNCHANGED. Reproducing that same race reliably a third time, HERE,
// through two full HTTP-shaped call paths of very different cost (a
// direct Go call vs. this package's own sign/parse/dedupe-claim/lookup
// webhook pipeline) is not a genuine, deterministic race at all -- one
// side (the direct call) wins essentially every time, which would make
// this file's own version of the test either flaky or silently
// non-representative rather than a meaningful proof.

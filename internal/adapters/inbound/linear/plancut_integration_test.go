//go:build integration

package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/framecut"
)

// seedCutPlanTextForLinear places turnID's window at the session's current
// watermark and stores, inside it, a text part whose only text is a frame
// the sandbox-agent cut on its way to the control plane (technical plan
// §6.1), as the session actor stores it: the empty first frame under the
// bare part id, then the cut one. Injected straight into `events`, since
// nothing produces a cut yet.
func seedCutPlanTextForLinear(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, turnID pgtype.UUID) framecut.Cut {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1) WHERE id = $2`, sessionID, turnID); err != nil {
		t.Fatalf("place the plan turn's window: %v", err)
	}
	cut := framecut.Cut{Kept: 20, Total: 40960}
	for _, f := range []struct {
		key, text string
		cut       *framecut.Cut
	}{
		{key: "prt_plan", text: ""},
		{key: "prt_plan#cut", text: "1. Add the migration\n[text cut at 20 of 40960 bytes on its way from the sandbox]", cut: &cut},
	} {
		payload := map[string]any{"type": "token", "messageId": "prt_plan", "sessionId": sessionID.String(), "gen": 1, "text": f.text}
		if f.cut != nil {
			payload["cut"] = f.cut
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal token frame: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO events (session_id, type, payload, message_id) VALUES ($1, 'token', $2, $3)`, sessionID, raw, f.key); err != nil {
			t.Fatalf("store token frame: %v", err)
		}
	}
	return cut
}

// TestWebhookHandler_Prompted_ApproveCutPlan_PostsTheReason proves an
// approve keyword replied to a plan whose text was cut on its way from the
// sandbox (technical plan §6.1) is refused (httpapi.ErrPlanCut), and that
// the refusal is posted as an agent activity carrying the reason -- where
// any other decide error is only logged -- with nothing changed: the plan
// stays awaiting approval, and no implementation turn is queued.
func TestWebhookHandler_Prompted_ApproveCutPlan_PostsTheReason(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	ctx := context.Background()
	agentSessionID := "agent-session-plan-cut"
	organizationID := "org-plan-cut"

	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, recordedBodies := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const deciderID = "linear-planverdict-cut-1"
	linkLinearIdentityForTest(ctx, t, pool, deciderID, sqlcgen.UserRoleMaintainer)

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
	cut := seedCutPlanTextForLinear(ctx, t, pool, session.ID, producingTurn.ID)
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	rec := postWebhook(t, handler, agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, deciderID, "approve"), "delivery-plan-cut")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var dbStatus sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
		t.Errorf("db status = %q, want %q (a refused approval changes nothing)", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
	}
	if allTurns, err := turns.ListForSession(ctx, session.ID); err != nil || len(allTurns) != 1 {
		t.Errorf("turns = %d (err %v), want 1: no implementation turn", len(allTurns), err)
	}

	reason := framecut.Reason(&cut)
	var posted bool
	for _, b := range recordedBodies() {
		if strings.Contains(b, "agentActivityCreate") && strings.Contains(b, reason) {
			posted = true
		}
	}
	if !posted {
		t.Errorf("no agent activity carried the reason %q; bodies: %v", reason, recordedBodies())
	}
}

// TestWebhookHandler_Prompted_AwaitingCutPlan_NonKeywordText_OffersNoApprove
// proves the notice posted for a reply that neither decides nor revises a
// plan awaiting approval reads that plan's cut report (technical plan
// §6.1): for a plan whose text was cut on its way from the sandbox, it
// gives the reason and offers no approve keyword, since the approval would
// be refused; for a whole plan it offers them as before. Either way the
// reply starts nothing and decides nothing.
func TestWebhookHandler_Prompted_AwaitingCutPlan_NonKeywordText_OffersNoApprove(t *testing.T) {
	tests := []struct {
		name string
		cut  bool
	}{
		{name: "a cut plan", cut: true},
		{name: "a whole plan"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newTestPool(t)
			deps := newHandlerDeps(t, pool)
			deps.Plans = narvipg.NewPlanStore(pool)
			deps.Events = narvipg.NewEventStore(pool)
			deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
			deps.Outbox = narvipg.NewOutboxStore(pool, false)
			deps.Participants = narvipg.NewParticipantStore(pool)

			ctx := context.Background()
			agentSessionID := "agent-session-awaiting-cut-" + strconv.Itoa(i)
			organizationID := "org-awaiting-cut-" + strconv.Itoa(i)
			installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
			stub, recordedBodies := newGenericLinearGraphQLStub(t)
			deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
			deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
			replierID := "linear-awaiting-cut-" + strconv.Itoa(i)
			linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)
			handler := linear.NewWebhookHandler(deps)

			turns := narvipg.NewTurnStore(pool)
			agentSessions := narvipg.NewLinearAgentSessionStore(pool)
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceLinear})
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
			var cut framecut.Cut
			if tt.cut {
				cut = seedCutPlanTextForLinear(ctx, t, pool, session.ID, producingTurn.ID)
			} else {
				if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1) WHERE id = $2`, session.ID, producingTurn.ID); err != nil {
					t.Fatalf("place the plan turn's window: %v", err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO events (session_id, type, payload, message_id) VALUES ($1, 'token', $2, 'prt_plan')`, session.ID, []byte(`{"type":"token","messageId":"prt_plan","gen":1,"text":"1. Add the migration"}`)); err != nil {
					t.Fatalf("store token frame: %v", err)
				}
			}
			plan, err := deps.Plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
			if err != nil {
				t.Fatalf("seed awaiting_approval plan: %v", err)
			}

			rec := postWebhook(t, handler, agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "looks good, go ahead"), "delivery-awaiting-cut-"+strconv.Itoa(i))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if all, err := turns.ListForSession(ctx, session.ID); err != nil || len(all) != 1 {
				t.Errorf("turns = %d (err %v), want 1", len(all), err)
			}
			var dbStatus sqlcgen.PlanStatus
			if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
				t.Fatalf("query plan row: %v", err)
			}
			if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
				t.Errorf("db status = %q, want %q", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
			}

			var notice string
			for _, b := range recordedBodies() {
				if strings.Contains(b, "agentActivityCreate") && strings.Contains(b, "A plan is awaiting your") {
					notice = b
				}
			}
			if notice == "" {
				t.Fatalf("no awaiting-plan notice was posted; bodies: %v", recordedBodies())
			}
			offersApprove := strings.Contains(notice, "approve/approved/lgtm")
			if offersApprove == tt.cut {
				t.Errorf("notice %s offers the approve keywords: %v, want %v", notice, offersApprove, !tt.cut)
			}
			if tt.cut && !strings.Contains(notice, framecut.Reason(&cut)) {
				t.Errorf("notice %s, want the reason %q", notice, framecut.Reason(&cut))
			}
		})
	}
}

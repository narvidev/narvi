//go:build integration

package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// linearSessionAtCap is a Linear-origin session on agentSessionID, naming
// repo, with a completed turn that took it past repo's $1.00 cap, and, when
// withPlan, a plan awaiting approval. It returns the session, the plan (zero
// without one) and the refusal the guard makes for it.
func linearSessionAtCap(ctx context.Context, t *testing.T, pool *pgxpool.Pool, agentSessionID, organizationID, repo string, withPlan bool) (sqlcgen.Session, sqlcgen.Plan, sessionguard.Refusal) {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceLinear,
		Repos:       []byte(`[{"url": "https://github.com/` + repo + `.git"}]`),
	})
	if err != nil {
		t.Fatalf("create linear-origin session: %v", err)
	}
	agentSessions := narvipg.NewLinearAgentSessionStore(pool)
	if _, err := agentSessions.Claim(ctx, agentSessionID, organizationID); err != nil {
		t.Fatalf("claim agent session: %v", err)
	}
	if err := agentSessions.SetSessionID(ctx, agentSessionID, session.ID); err != nil {
		t.Fatalf("attach session id: %v", err)
	}
	turns := int64(1)
	var plan sqlcgen.Plan
	if withPlan {
		producing, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
		if err != nil {
			t.Fatalf("seed producing turn: %v", err)
		}
		if plan, err = narvipg.NewPlanStore(pool).Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producing.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
			t.Fatalf("seed awaiting_approval plan: %v", err)
		}
		turns++
	}
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1.25)`, session.ID); err != nil {
		t.Fatalf("store the session's spend: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 1.00)`, repo); err != nil {
		t.Fatalf("cap the repository: %v", err)
	}
	return session, plan, sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: session.ID.Bytes, Cap: 1_000_000, Spent: 1_250_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}, Turns: turns,
	}
}

// linearGuardRecords counts sessionID's warnings at refusal's crossing,
// its issue-tracker guard notices, and its turns.
func linearGuardRecords(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, refusal sessionguard.Refusal) (warnings, notices, turns int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, sessionID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindLinearSessionGuard)).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	return warnings, notices, turns
}

// TestLinearPrompt_AtSpendCap_HonestReply: a prompt, or an approval typed
// for an awaiting plan, on the issue tracker for a session that has spent
// its cap (technical plan §40.1) creates no turn and is answered on the
// agent session with the refusal's own text -- a deterministic state, never
// a failed delivery. The plan stays awaiting approval. The crossing's
// warning is recorded, and that answer on the session's own agent session
// is the crossing's one telling: no outbox notice repeats it there.
func TestLinearPrompt_AtSpendCap_HonestReply(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		text     string
		withPlan bool
	}{
		{name: "a prompt", text: "one more change please"},
		{name: "an approval of the awaiting plan", text: "approve", withPlan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			deps := newHandlerDeps(t, pool)
			deps.Plans = narvipg.NewPlanStore(pool)
			deps.Events = narvipg.NewEventStore(pool)
			deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
			deps.Outbox = narvipg.NewOutboxStore(pool, false)
			deps.Participants = narvipg.NewParticipantStore(pool)

			const agentSessionID = "agent-session-spend-cap"
			const organizationID = "org-spend-cap"
			installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
			stub, recordedBodies := newGenericLinearGraphQLStub(t)
			deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
			deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
			const replierID = "linear-spend-cap-1"
			linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)
			handler := linear.NewWebhookHandler(deps)
			session, plan, refusal := linearSessionAtCap(ctx, t, pool, agentSessionID, organizationID, "acme/linear-cap", tc.withPlan)
			_, _, turnsBefore := linearGuardRecords(ctx, t, pool, session.ID, refusal)

			rec := postWebhook(t, handler, agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, tc.text), "delivery-spend-cap")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: a session at its cap is no failed delivery; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var replies int
			for _, b := range recordedBodies() {
				if carriesString([]byte(b), sessionguard.Text(refusal)) {
					replies++
				}
			}
			if replies != 1 {
				t.Fatalf("activities carrying the refusal's text = %d, want 1 (bodies %q)", replies, recordedBodies())
			}
			warnings, notices, turns := linearGuardRecords(ctx, t, pool, session.ID, refusal)
			if turns != turnsBefore {
				t.Fatalf("turns = %d, want the %d it had", turns, turnsBefore)
			}
			if warnings != 1 || notices != 0 {
				t.Fatalf("warnings %d, notices %d; want the crossing's warning, and no notice beside the answer on the agent session", warnings, notices)
			}
			if tc.withPlan {
				var status sqlcgen.PlanStatus
				if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != sqlcgen.PlanStatusAwaitingApproval {
					t.Fatalf("plan status = %q, want awaiting_approval", status)
				}
			}
		})
	}
}

// carriesString reports whether the JSON document raw holds want as one
// of its string values, at any depth.
func carriesString(raw []byte, want string) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(x any) bool {
		switch x := x.(type) {
		case map[string]any:
			for _, e := range x {
				if walk(e) {
					return true
				}
			}
		case []any:
			for _, e := range x {
				if walk(e) {
					return true
				}
			}
		case string:
			return x == want
		}
		return false
	}
	return walk(v)
}

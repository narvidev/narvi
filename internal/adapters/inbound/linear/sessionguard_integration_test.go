//go:build integration

package linear_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// linearNotices is the state of a session's issue-tracker guard notices:
// held (pending, never attempted, due later: its reply was being tried),
// withdrawn (delivered in place, never attempted: the reply on the agent
// session told the crossing), and any other row.
type linearNotices struct {
	held, withdrawn, other int
}

// linearGuardRecords counts sessionID's warnings at refusal's crossing,
// reads its issue-tracker guard notices, and counts its turns.
func linearGuardRecords(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, refusal sessionguard.Refusal) (warnings int, notices linearNotices, turns int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, sessionID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE status = 'pending' AND attempts = 0 AND next_attempt_at > now()),
			count(*) FILTER (WHERE status = 'delivered' AND attempts = 0),
			count(*) FILTER (WHERE NOT ((status = 'pending' AND attempts = 0 AND next_attempt_at > now()) OR (status = 'delivered' AND attempts = 0)))
		FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindLinearSessionGuard)).Scan(&notices.held, &notices.withdrawn, &notices.other); err != nil {
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
// warning is recorded, and it is told on the agent session exactly once:
// when the answer there lands, it is the telling, and the notice held while
// it was tried is withdrawn; when it fails (the issue tracker answers 500),
// that held notice stays, to be delivered.
func TestLinearPrompt_AtSpendCap_HonestReply(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		text        string
		withPlan    bool
		replyFails  bool
		wantNotices linearNotices
	}{
		{name: "a prompt", text: "one more change please", wantNotices: linearNotices{withdrawn: 1}},
		{name: "an approval of the awaiting plan", text: "approve", withPlan: true, wantNotices: linearNotices{withdrawn: 1}},
		{name: "a prompt whose answer fails", text: "one more change please", replyFails: true, wantNotices: linearNotices{held: 1}},
		{name: "an approval whose answer fails", text: "approve", withPlan: true, replyFails: true, wantNotices: linearNotices{held: 1}},
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
			var refusalText string
			if tc.replyFails {
				// Every call carrying the refusal's text fails; every other
				// call goes to the recording stub.
				failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					if refusalText != "" && carriesString(raw, refusalText) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					forward, err := http.NewRequestWithContext(r.Context(), r.Method, stub.URL+r.URL.Path, bytes.NewReader(raw))
					if err != nil {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					forward.Header = r.Header.Clone()
					resp, err := stub.Client().Do(forward)
					if err != nil {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					defer func() { _ = resp.Body.Close() }()
					w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
					w.WriteHeader(resp.StatusCode)
					_, _ = io.Copy(w, resp.Body)
				}))
				t.Cleanup(failing.Close)
				deps.LinearClient = linearapi.New(failing.Client(), failing.URL)
			}
			deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
			const replierID = "linear-spend-cap-1"
			linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)
			handler := linear.NewWebhookHandler(deps)
			session, plan, refusal := linearSessionAtCap(ctx, t, pool, agentSessionID, organizationID, "acme/linear-cap", tc.withPlan)
			refusalText = sessionguard.Text(refusal)
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
			if want := map[bool]int{false: 1, true: 0}[tc.replyFails]; replies != want {
				t.Fatalf("activities carrying the refusal's text that landed = %d, want %d (bodies %q)", replies, want, recordedBodies())
			}
			warnings, notices, turns := linearGuardRecords(ctx, t, pool, session.ID, refusal)
			if turns != turnsBefore {
				t.Fatalf("turns = %d, want the %d it had", turns, turnsBefore)
			}
			if warnings != 1 || notices != tc.wantNotices {
				t.Fatalf("warnings %d, notices %+v; want the crossing's warning, and notices %+v", warnings, notices, tc.wantNotices)
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

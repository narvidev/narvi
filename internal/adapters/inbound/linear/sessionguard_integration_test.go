//go:build integration

package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// TestLinearPrompt_AtSpendCap_HonestReply: a prompt on the issue tracker
// for a session that has spent its cap (technical plan §40.1) creates no
// turn and is answered on the agent session with the refusal's own text --
// a deterministic state, never a failed delivery -- and the crossing's
// warning and its one notice are recorded.
func TestLinearPrompt_AtSpendCap_HonestReply(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Plans = narvipg.NewPlanStore(pool)
	deps.Events = narvipg.NewEventStore(pool)
	deps.PlanDocuments = narvipg.NewPlanDocumentStore(pool)
	deps.Outbox = narvipg.NewOutboxStore(pool, false)
	deps.Participants = narvipg.NewParticipantStore(pool)

	const agentSessionID = "agent-session-spend-cap"
	const organizationID = "org-spend-cap"
	const repo = "acme/linear-cap"
	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)
	stub, recordedBodies := newGenericLinearGraphQLStub(t)
	deps.LinearClient = linearapi.New(stub.Client(), stub.URL)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)
	const replierID = "linear-spend-cap-1"
	linkLinearIdentityForTest(ctx, t, pool, replierID, sqlcgen.UserRoleMaintainer)
	handler := linear.NewWebhookHandler(deps)

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
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1.25)`, session.ID); err != nil {
		t.Fatalf("store the session's spend: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 1.00)`, repo); err != nil {
		t.Fatalf("cap the repository: %v", err)
	}
	refusal := sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: session.ID.Bytes, Cap: 1_000_000, Spent: 1_250_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo},
	}

	rec := postWebhook(t, handler, agentSessionPromptedPayloadWithUser(agentSessionID, organizationID, replierID, "one more change please"), "delivery-spend-cap")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: a session at its cap is no failed delivery; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var turns, warnings, notices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Fatalf("turns = %d, want the spend's alone", turns)
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

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, session.ID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindLinearSessionGuard)).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 || notices != 1 {
		t.Fatalf("warnings %d, notices %d; want the crossing's one of each", warnings, notices)
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

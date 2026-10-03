//go:build integration

package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestBuild_DecisionInboxReadsEachPlansCut proves the composition root
// hands the decision inbox the turn and event stores its cut report reads
// (decisioninbox.Deps.Turns and Events, technical plan §6.1). Both are
// nil-safe by design -- without them no row reports a cut -- so only a
// read through Build's own router shows they are wired: an awaiting plan
// whose text is a frame the sandbox-agent cut reports its cut on
// GET /api/decision-inbox, where a client shows the reason in place of
// "Approve & build", and a whole plan reports none. The cut frame is
// injected straight into the plan turn's window, since nothing produces a
// cut yet.
func TestBuild_DecisionInboxReadsEachPlansCut(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)
	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	installFakeGitHub(t)

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.registry.Shutdown() })
	server := httptest.NewServer(app.Router)
	t.Cleanup(server.Close)

	ctx := context.Background()
	cookie := createMaintainerSession(ctx, t, pool)
	cutPlan := seedAwaitingPlanWithFrame(ctx, t, pool, "1. Add the migration\n[text cut at 20 of 40960 bytes on its way from the sandbox]", `{"kept":20,"total":40960}`)
	wholePlan := seedAwaitingPlanWithFrame(ctx, t, pool, "1. Add the migration\n2. Wire the store\n", "")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/decision-inbox", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Cookie", platform.AuthSessionCookieName+"="+cookie)
	// The test's own request goes through a plain transport, never the
	// fake that stands in for GitHub.
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatalf("GET /api/decision-inbox: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/decision-inbox = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			PlanID       *string `json:"planId"`
			PlanCutKept  *int    `json:"planCutKept"`
			PlanCutTotal *int    `json:"planCutTotal"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode decision inbox: %v", err)
	}

	seen := map[string]bool{}
	for _, it := range body.Items {
		if it.PlanID == nil {
			continue
		}
		seen[*it.PlanID] = true
		switch *it.PlanID {
		case cutPlan.String():
			if it.PlanCutKept == nil || it.PlanCutTotal == nil || *it.PlanCutKept != 20 || *it.PlanCutTotal != 40960 {
				t.Errorf("the cut plan's row reports planCutKept/planCutTotal %v/%v through Build's router, want 20/40960 -- the composition root did not hand the inbox its turn and event stores", derefCut(it.PlanCutKept), derefCut(it.PlanCutTotal))
			}
		case wholePlan.String():
			if it.PlanCutKept != nil || it.PlanCutTotal != nil {
				t.Errorf("the whole plan's row reports planCutKept/planCutTotal %v/%v, want null", derefCut(it.PlanCutKept), derefCut(it.PlanCutTotal))
			}
		}
	}
	if !seen[cutPlan.String()] || !seen[wholePlan.String()] {
		t.Fatalf("the inbox lists plans %v, want both seeded plans", seen)
	}
}

// seedAwaitingPlanWithFrame creates a session whose completed plan-mode
// turn is dispatched at the session's watermark, stores one `token` frame
// of text in its window (carrying cut as its raw `cut` property, "" for
// none), and an awaiting_approval plan atop it.
func seedAwaitingPlanWithFrame(ctx context.Context, t *testing.T, pool *pgxpool.Pool, text, cut string) pgtype.UUID {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	turn, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create plan turn: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1) WHERE id = $2`, session.ID, turn.ID); err != nil {
		t.Fatalf("place the plan turn's window: %v", err)
	}
	frame := map[string]any{"type": "token", "messageId": "prt_plan", "sessionId": session.ID.String(), "gen": 1, "text": text}
	if cut != "" {
		frame["cut"] = json.RawMessage(cut)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal token frame: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (session_id, type, payload, message_id) VALUES ($1, 'token', $2, 'prt_plan')`, session.ID, raw); err != nil {
		t.Fatalf("store token frame: %v", err)
	}
	plan, err := narvipg.NewPlanStore(pool).Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return plan.ID
}

func derefCut(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

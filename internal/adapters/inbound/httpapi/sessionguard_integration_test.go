//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// This file is technical plan §40.1's spend cap as the REST routes meet
// it: a session that has spent its cap refuses the next turn whoever asks
// -- an admin's prompt, the review button, a plan approval -- with a 409
// whose body names the typed reason, records the crossing's warning once,
// and takes the next turn once the cap is raised.

// spendCapSeed is what a test puts on a session to bring it to its cap: a
// repository the session names and that repository's cap, and a turn the
// session ran that cost spent.
type spendCapSeed struct {
	repo  string
	cap   string
	spent string
}

// seedSpendCap names seed.repo as sessionID's repository (a clone URL, or
// nothing when claimed is true: a review session names it through its
// pull-request claim), sets the repository's cap, and stores a completed,
// dispatched turn of sessionID that cost seed.spent.
func seedSpendCap(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, seed spendCapSeed, claimed bool) {
	t.Helper()
	if !claimed {
		if _, err := pool.Exec(ctx, `UPDATE sessions SET repos = jsonb_build_array(jsonb_build_object('url', 'https://github.com/' || $2::text || '.git')) WHERE id = $1`, sessionID, seed.repo); err != nil {
			t.Fatalf("name the session's repository: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, $2::numeric)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, seed.repo, seed.cap); err != nil {
		t.Fatalf("set the repository's cap: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), $2::numeric)`, sessionID, seed.spent); err != nil {
		t.Fatalf("store the session's spend: %v", err)
	}
}

// expectedRefusal is the refusal the guard makes for seed on sessionID.
func expectedRefusal(t *testing.T, sessionID pgtype.UUID, seed spendCapSeed) sessionguard.Refusal {
	t.Helper()
	limit, err := sessionguard.ParseMicroUSD(seed.cap)
	if err != nil {
		t.Fatal(err)
	}
	spent, err := sessionguard.ParseMicroUSD(seed.spent)
	if err != nil {
		t.Fatal(err)
	}
	return sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: sessionID.Bytes, Cap: limit, Spent: spent,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: seed.repo, ID: seed.repo},
	}
}

// guardWarnings counts sessionID's warning events, and those stored at
// messageID.
func guardWarnings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, messageID string) (all, atID int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE message_id = $2) FROM events WHERE session_id = $1 AND type = 'warning'`, sessionID, messageID).Scan(&all, &atID); err != nil {
		t.Fatalf("count warnings: %v", err)
	}
	return all, atID
}

// countSessionTurns counts sessionID's turns.
func countSessionTurns(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	return n
}

// turnBody is a POST .../turns body asking for an ordinary turn.
func turnBody(prompt string) []byte {
	return []byte(`{"prompt": "` + prompt + `", "modelId": null, "effort": null, "planMode": false}`)
}

// refusalBody is a 409's body: the text, and the typed reason.
type refusalBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// TestSpendCap_RefusesEveryoneRegardlessOfWhoAsked: the cap applies to
// every turn on the session regardless of who asked for it -- §40.1's
// inversion of §24.6, stated where the turn is created
// (TestSpendCapAppliesRegardlessOfWhoAsked scans the statement). An
// admin's own prompt, the review button a maintainer presses and a plan
// approval are each refused with a 409 whose body is the refusal's text
// and its typed reason; nothing is created, a plan stays awaiting
// approval, and the crossing's warning is recorded once.
func TestSpendCap_RefusesEveryoneRegardlessOfWhoAsked(t *testing.T) {
	ctx := context.Background()
	seed := spendCapSeed{repo: "acme/capped", cap: "1.00", spent: "1.000000"}

	t.Run("an admin's REST prompt", func(t *testing.T) {
		rig := newTestRig(t)
		_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
		owner, _ := rig.createAuthenticatedUser(ctx, t)
		session := createSessionForUser(ctx, t, rig, owner.ID, nil)
		seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)

		var body refusalBody
		status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("one more thing"), &body, token)
		assertSpendCapRefusal(ctx, t, rig, session.ID, seed, status, body, 1)
	})

	t.Run("the review button", func(t *testing.T) {
		rig := newTestRig(t)
		owner, _ := rig.createAuthenticatedUser(ctx, t)
		_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
		session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, seed.repo, 7)
		seedSpendCap(ctx, t, rig.pool, session.ID, seed, true)

		var body refusalBody
		status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, &body, token)
		assertSpendCapRefusal(ctx, t, rig, session.ID, seed, status, body, 1)
	})

	t.Run("a plan approval", func(t *testing.T) {
		rig := newTestRig(t)
		owner, token := rig.createAuthenticatedUser(ctx, t)
		session := createSessionForUser(ctx, t, rig, owner.ID, nil)
		seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)
		plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

		var body refusalBody
		status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", nil, &body, token)
		assertSpendCapRefusal(ctx, t, rig, session.ID, seed, status, body, 2)
		after, err := rig.plans.Get(ctx, plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != sqlcgen.PlanStatusAwaitingApproval {
			t.Fatalf("plan status after a refused approval = %q, want awaiting_approval", after.Status)
		}
	})
}

// assertSpendCapRefusal checks a refused request: 409, the refusal's text
// and typed reason in the body, the session's turns as they were
// (wantTurns), no dispatch timer, and one warning at the crossing's id.
func assertSpendCapRefusal(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, seed spendCapSeed, status int, body refusalBody, wantTurns int) {
	t.Helper()
	want := expectedRefusal(t, sessionID, seed)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if body.Reason != string(sessionguard.ReasonSpendCap) || body.Error != sessionguard.Text(want) {
		t.Fatalf("body = %+v, want the refusal's text with reason %q", body, sessionguard.ReasonSpendCap)
	}
	if n := countSessionTurns(ctx, t, rig.pool, sessionID); n != wantTurns {
		t.Fatalf("turns = %d after a refusal, want %d: a refused turn is never inserted", n, wantTurns)
	}
	var timers int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'dispatch'`, sessionID).Scan(&timers); err != nil {
		t.Fatal(err)
	}
	if timers != 0 {
		t.Fatalf("a refused turn armed the dispatch timer")
	}
	if all, atID := guardWarnings(ctx, t, rig.pool, sessionID, turnguard.WarningMessageID(want)); all != 1 || atID != 1 {
		t.Fatalf("warnings = %d (%d at the crossing's id), want one", all, atID)
	}
}

// TestSpendCap_RaisedCapReadmitsTheNextTurn: a refused prompt is taken once
// the cap is raised -- here by writing the column directly, the audited
// write being a later step's -- and the turn it creates is queued for
// dispatch; a refusal after the raise is a new crossing, with a warning of
// its own.
func TestSpendCap_RaisedCapReadmitsTheNextTurn(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	seed := spendCapSeed{repo: "acme/raised", cap: "2.00", spent: "2.500000"}
	seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)

	var body refusalBody
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("go on"), &body, token)
	if status != http.StatusConflict || body.Reason != "spend_cap" {
		t.Fatalf("at the cap: status %d, body %+v; want a 409 with reason spend_cap", status, body)
	}

	if _, err := rig.pool.Exec(ctx, `UPDATE repo_settings SET session_spend_cap_usd = 5.00 WHERE repo_full_name = $1`, seed.repo); err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("go on"), &created, token)
	if status != http.StatusCreated || created.Status != "pending" {
		t.Fatalf("after the raise: status %d, %+v; want 201 and a pending turn", status, created)
	}
	// The dispatch evaluation the create triggers reads the guard again:
	// once it has run -- it deletes the dispatch timer first -- the turn is
	// still the queued one, not ended at the cap (this rig has no sandbox
	// provider, so it goes no further).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var timers int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'dispatch'`, session.ID).Scan(&timers); err != nil {
			t.Fatal(err)
		}
		if timers == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dispatch evaluation never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var turnStatus string
	var endReason *string
	if err := rig.pool.QueryRow(ctx, `SELECT status::text, end_reason FROM turns WHERE id = $1::uuid`, created.ID).Scan(&turnStatus, &endReason); err != nil {
		t.Fatal(err)
	}
	if turnStatus != "pending" || endReason != nil {
		t.Fatalf("the admitted turn after its dispatch evaluation: %s (end reason %v), want still pending: the raised cap admits its dispatch too", turnStatus, endReason)
	}

	// A new crossing: the raised cap reached again warns anew.
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 3 WHERE id = $1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	status = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("and again"), &body, token)
	if status != http.StatusConflict {
		t.Fatalf("past the raised cap: status %d, want 409", status)
	}
	raised := seed
	raised.cap = "5.00"
	if all, atID := guardWarnings(ctx, t, rig.pool, session.ID, turnguard.WarningMessageID(expectedRefusal(t, session.ID, raised))); all != 2 || atID != 1 {
		t.Fatalf("warnings = %d (%d at the new crossing's id), want two, one a crossing", all, atID)
	}
}

// TestSpendCap_CreateResponseIsTheRefusalText pins the 409's body shape a
// client reads: "error" is sessionguard.Text, "reason" the typed reason,
// and nothing else -- an unschema'd key, outside the contracts' policy.
func TestSpendCap_CreateResponseIsTheRefusalText(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	seed := spendCapSeed{repo: "acme/body", cap: "0.50", spent: "0.750000"}
	seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)

	var raw map[string]json.RawMessage
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", turnBody("x"), &raw, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if len(raw) != 2 || string(raw["reason"]) != `"spend_cap"` {
		t.Fatalf("body = %v, want exactly error and reason", raw)
	}
	var text string
	if err := json.Unmarshal(raw["error"], &text); err != nil {
		t.Fatal(err)
	}
	if want := sessionguard.Text(expectedRefusal(t, session.ID, seed)); text != want {
		t.Fatalf("error = %q, want %q", text, want)
	}
	if fmt.Sprint(expectedRefusal(t, session.ID, seed).Spent) != "$0.75" {
		t.Fatal("the refusal reads the session's spend wrong")
	}
}

// TestDecidePlan_AtSpendCap_PlanStaysAwaiting: the chat surface's and the
// issue tracker's plan approval (the pool-based DecidePlan) is refused for
// a session past its cap with the guard's refusal, the plan stays awaiting
// approval -- approving it again after a raise works -- no implementation
// turn is created, and the crossing's warning is recorded.
func TestDecidePlan_AtSpendCap_PlanStaysAwaiting(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)
	seed := spendCapSeed{repo: "acme/plan-cap", cap: "1.00", spent: "1.500000"}
	seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)
	plan := seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	decide := func() (httpapi.DecidePlanOutcome, error) {
		return httpapi.DecidePlan(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry,
			turnguard.New(rig.pool, nil, false), session.ID, plan.ID, httpapi.PlanVerdictApprove, owner.ID, false)
	}
	_, err := decide()
	refusal, ok := sessionguard.AsRefusal(err)
	if !ok || refusal.Reason != sessionguard.ReasonSpendCap {
		t.Fatalf("DecidePlan past the cap = %v, want the guard's refusal", err)
	}
	after, err := rig.plans.Get(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != sqlcgen.PlanStatusAwaitingApproval {
		t.Fatalf("plan status = %q, want awaiting_approval", after.Status)
	}
	if n := countSessionTurns(ctx, t, rig.pool, session.ID); n != 2 {
		t.Fatalf("turns = %d, want the spend's and the plan's: no implementation turn", n)
	}
	if all, atID := guardWarnings(ctx, t, rig.pool, session.ID, turnguard.WarningMessageID(expectedRefusal(t, session.ID, seed))); all != 1 || atID != 1 {
		t.Fatalf("warnings = %d (%d at the crossing's id), want one", all, atID)
	}

	if _, err := rig.pool.Exec(ctx, `UPDATE repo_settings SET session_spend_cap_usd = 10 WHERE repo_full_name = $1`, seed.repo); err != nil {
		t.Fatal(err)
	}
	outcome, err := decide()
	if err != nil || !outcome.Won || outcome.TurnID == nil {
		t.Fatalf("DecidePlan after the raise = %+v, %v; want the approval and its turn", outcome, err)
	}
}

// TestDecideWorkflowStep_AtSpendCap_409AndRolledBack: a person approving or
// revising a workflow step of a session past its cap is answered 409 with
// the typed reason, and the decision rolls back: the step stays awaiting
// its decision, no attempt is created, and the run is not escalated.
func TestDecideWorkflowStep_AtSpendCap_409AndRolledBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, body string }{
		{name: "approve", body: `{"verdict":"approve","text":null}`},
		{name: "revise", body: `{"verdict":"revise","text":"try again"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			owner, token := rig.createAuthenticatedUser(ctx, t)
			session := createSessionForUser(ctx, t, rig, owner.ID, nil)
			seed := spendCapSeed{repo: "acme/step-cap", cap: "1.00", spent: "1.000000"}
			seedSpendCap(ctx, t, rig.pool, session.ID, seed, false)
			runID, stepRunID, _ := seedAwaitingDecisionRun(ctx, t, rig, session.ID, 2, "ok")

			var got refusalBody
			status := rig.doJSON(t, http.MethodPost, decidePath(runID, stepRunID), []byte(tc.body), &got, token)
			if status != http.StatusConflict || got.Reason != "spend_cap" || got.Error != sessionguard.Text(expectedRefusal(t, session.ID, seed)) {
				t.Fatalf("status %d, body %+v; want 409 with the refusal", status, got)
			}
			stepRun, err := rig.workflows.GetStepRun(ctx, stepRunID)
			if err != nil {
				t.Fatal(err)
			}
			if string(stepRun.Status) != "awaiting_decision" {
				t.Fatalf("step run status = %q, want awaiting_decision: the decision rolled back", stepRun.Status)
			}
			run, err := rig.workflows.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if string(run.Status) != "running" {
				t.Fatalf("run status = %q, want running: a person's refused decision escalates nothing", run.Status)
			}
			var stepRuns int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, runID).Scan(&stepRuns); err != nil {
				t.Fatal(err)
			}
			if stepRuns != 1 {
				t.Fatalf("step runs = %d, want 1: no attempt was created", stepRuns)
			}
			if n := countSessionTurns(ctx, t, rig.pool, session.ID); n != 1 {
				t.Fatalf("turns = %d, want the spend's alone", n)
			}
		})
	}
}

//go:build integration

// Integration tests for §25.6's ("workflow execution engine", §25.6) own
// generic step-outcome-posting tool (workflowstepoutcome.go), against a
// real Postgres instance -- sharing this package's own testRig
// (httpapi_integration_test.go), mirroring reviewverdict_integration_test.go's
// own house style exactly (both are sandbox-bearer-authenticated tools).
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
)

func validStepOutcomeRequestJSON() string {
	return `{"status":"ok","summary":"all good"}`
}

// postWorkflowStepOutcome posts body to sessionID's own
// workflow/step-outcome endpoint, mirroring postReviewVerdict's own
// identical bearer/gen header convention (reviewverdict_integration_test.go).
func postWorkflowStepOutcome(t *testing.T, r testRig, sessionID, bearer, gen, body string) (int, restdtos.PostWorkflowStepOutcomeResponse) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/sessions/"+sessionID+"/workflow/step-outcome", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got restdtos.PostWorkflowStepOutcomeResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

// bareSessionWithSandbox creates a plain (no repos, no intent_decision)
// session plus a real sandbox row with a known bearer token -- the
// zero-config fixture every test below starts from.
func bareSessionWithSandbox(ctx context.Context, t *testing.T, r testRig, bearerToken string) sqlcgen.Session {
	t.Helper()
	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, r, session.ID, bearerToken)
	return session
}

func TestPostWorkflowStepOutcome_NotFound(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postWorkflowStepOutcome(t, rig, "00000000-0000-0000-0000-000000000000", "any-token", "1", validStepOutcomeRequestJSON())
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestPostWorkflowStepOutcome_MissingBearer_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-nobearer")

	status, _ := postWorkflowStepOutcome(t, rig, session.ID.String(), "", "1", validStepOutcomeRequestJSON())
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestPostWorkflowStepOutcome_WrongToken_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-wrongtoken")

	status, _ := postWorkflowStepOutcome(t, rig, session.ID.String(), "not-the-real-token", "1", validStepOutcomeRequestJSON())
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestPostWorkflowStepOutcome_GenMismatch_Forbidden(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-genmismatch")

	status, _ := postWorkflowStepOutcome(t, rig, session.ID.String(), "step-outcome-genmismatch", "999", validStepOutcomeRequestJSON())
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

func TestPostWorkflowStepOutcome_MalformedBody_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-malformed")

	status, _ := postWorkflowStepOutcome(t, rig, session.ID.String(), "step-outcome-malformed", "1", `{"status":"not-a-real-status","summary":"x"}`)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestPostWorkflowStepOutcome_NoRunningWorkflow_BadRequest proves the
// "meaningless call" 400: a session that has never dispatched any turn at
// all (so createTurnLocked's own ResolveStepForNewTurn has never run) has
// no running WorkflowRun to post onto.
func TestPostWorkflowStepOutcome_NoRunningWorkflow_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-norun")

	status, _ := postWorkflowStepOutcome(t, rig, session.ID.String(), "step-outcome-norun", "1", validStepOutcomeRequestJSON())
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestPostWorkflowStepOutcome_Success_RecordsOnLiveStepRun is the flagship
// happy-path proof: dispatching a REAL turn through the (now
// engine-mediated) CreateTurnCore first, so a genuine running WorkflowRun
// and live WorkflowStepRun exist for this session exactly like production
// traffic produces, then posting an outcome onto it via this endpoint --
// no run/step ids supplied by the caller at all, just {status, summary,
// structuredPayload}, matching a real agent's own call shape.
func TestPostWorkflowStepOutcome_Success_RecordsOnLiveStepRun(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-success")

	created, _, cerr := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "do the thing", nil, false, false, pgtype.UUID{}, httpapi.RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	_ = created

	status, resp := postWorkflowStepOutcome(t, rig, session.ID.String(), "step-outcome-success", "1",
		`{"status":"needs_fix","summary":"found a fixable issue","structuredPayload":{"foo":"bar"}}`)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.StepRunId == "" || resp.WorkflowRunId == "" {
		t.Fatalf("response = %+v, want non-empty stepRunId/workflowRunId", resp)
	}

	var outcomeStatus, summary string
	var payload []byte
	if err := rig.pool.QueryRow(ctx, `SELECT outcome_status, outcome_summary, outcome_payload FROM workflow_step_runs WHERE id = $1`, resp.StepRunId).
		Scan(&outcomeStatus, &summary, &payload); err != nil {
		t.Fatalf("query step-run: %v", err)
	}
	if outcomeStatus != "needs_fix" {
		t.Errorf("outcome_status = %q, want needs_fix", outcomeStatus)
	}
	if summary != "found a fixable issue" {
		t.Errorf("outcome_summary = %q, want %q", summary, "found a fixable issue")
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal outcome_payload: %v", err)
	}
	if decoded["foo"] != "bar" {
		t.Errorf("outcome_payload = %v, want {\"foo\":\"bar\"}", decoded)
	}
}

// TestPostWorkflowStepOutcome_SecondCallAfterStepFinished_BadRequest
// proves the guarded read-then-write's OWN outcome for the common
// "already finished by the time this call started" case: GetLiveStepRunForRun
// (step 7 of this handler's own outcome table) finds no live step at all
// once the attempt this endpoint originally targeted has already gone
// terminal, and reports the SAME 400 a session with no live step ever at
// all gets -- never silently posted onto a stale/already-decided attempt.
// The narrower 409 (step 8) is reserved for the tighter race where the
// read still sees a live attempt but the guarded UPDATE itself loses to a
// concurrent write in the microseconds after -- not exercised here.
func TestPostWorkflowStepOutcome_SecondCallAfterStepFinished_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "step-outcome-conflict")

	if _, _, cerr := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID, "do the thing", nil, false, false, pgtype.UUID{}, httpapi.RejectIfOpen); cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}

	status, resp := postWorkflowStepOutcome(t, rig, session.ID.String(), "step-outcome-conflict", "1", validStepOutcomeRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("first post: status = %d, want %d", status, http.StatusCreated)
	}

	// Force the live attempt terminal directly (bypassing OnTurnCompleted's
	// own machinery -- this test only needs "no longer running", not a
	// faithful re-derivation of how it got there).
	if _, err := rig.pool.Exec(ctx, `UPDATE workflow_step_runs SET status = 'completed', finished_at = now() WHERE id = $1`, resp.StepRunId); err != nil {
		t.Fatalf("force step-run terminal: %v", err)
	}

	status2, _ := postWorkflowStepOutcome(t, rig, session.ID.String(), "step-outcome-conflict", "1", validStepOutcomeRequestJSON())
	if status2 != http.StatusBadRequest {
		t.Errorf("second post: status = %d, want %d", status2, http.StatusBadRequest)
	}
}

//go:build integration

// This file is row 183's plan decisions, revisions and prompts over MCP
// (technical plan §43.21) on the router Build returns, driven by the
// official SDK client through the real consent flow, as
// TestOAuth_ProductionRouter's subtests.
//
//   - An approval over MCP runs the REST approve handler unchanged, so for
//     every role, on a session the user started and on one they did not,
//     its outcome and the database state it leaves -- the plan's row, the
//     implementation turn, the approved-content snapshot, the audit row and
//     the notices -- equal a REST approval's by the same user's cookie; the
//     MCP audit row also carries the grant.
//   - The open-turn gate refuses an approval, over MCP as over REST, while
//     any turn is pending, dispatched or processing, and changes nothing.
//   - The first verdict wins across channels: a plan decided over one is
//     refused over the other.
//   - A revision requested while an approved implementation is processing
//     is refused over MCP and REST alike, with the same text, and nothing
//     is queued, cancelled or decided: the implementation is still
//     processing and its plan still approved.
//   - A prompt while a turn is running is refused like REST (owner decision
//     O5), and nothing is queued; on an idle session the prompt and the
//     revision tools create exactly what REST creates, and the plan list
//     answers REST's bytes.
//
// Every approval that wins queues a turn and spawns a session actor, so
// these run on a createRouterRig (its own pool, its registry shut down
// before the pool closes). The actor's spawn attempt may move the
// implementation turn afterwards, so a comparison reads what the decision
// itself wrote -- never that turn's later status.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// busyRefusal is REST's own open-turn refusal, shared by turn creation
// (RejectIfOpen) and approval (the stale-plan guard).
const busyRefusal = "a turn is already pending, dispatched, or processing for this session"

// notAwaitingRefusal is REST's refusal of a decision on a plan that is no
// longer awaiting approval: the first verdict won.
const notAwaitingRefusal = "plan is not awaiting approval (already decided, or a stale id)"

// planForbidden is REST's refusal of a plan decision the user's role, or
// the own/joined rule, does not allow.
const planForbidden = "not authorized to act on this session's plans"

// seedPlannedSession creates a session started by creator whose plan v1,
// produced by a completed plan-mode turn, has status: the state a
// plan-mode session is in once its first turn completed.
func seedPlannedSession(ctx context.Context, t *testing.T, rig *oauthRouterRig, creator pgtype.UUID, status sqlcgen.PlanStatus) (sessionID, planID pgtype.UUID) {
	t.Helper()
	session, err := narvipg.NewSessionStore(rig.pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: creator})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	producing, err := narvipg.NewTurnStore(rig.pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create the plan-mode turn: %v", err)
	}
	plan, err := narvipg.NewPlanStore(rig.pool).Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producing.ID, Version: 1, Status: status})
	if err != nil {
		t.Fatalf("create plan v1: %v", err)
	}
	if status != sqlcgen.PlanStatusAwaitingApproval {
		if _, err := rig.pool.Exec(ctx, `UPDATE plans SET decided_at = now(), decided_by = $2 WHERE id = $1`, plan.ID, creator); err != nil {
			t.Fatalf("decide plan v1: %v", err)
		}
	}
	return session.ID, plan.ID
}

// seedTurn adds a turn to sessionID in status: plan-mode or not.
func seedTurn(ctx context.Context, t *testing.T, rig *oauthRouterRig, sessionID pgtype.UUID, status sqlcgen.TurnStatus, planMode bool) pgtype.UUID {
	t.Helper()
	prompt := "Implement the plan you just proposed."
	row, err := narvipg.NewTurnStore(rig.pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: status, PlanMode: planMode, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create %s turn: %v", status, err)
	}
	return row.ID
}

// callTool calls name through the SDK session with arguments.
func callTool(ctx context.Context, t *testing.T, s *sdkmcp.ClientSession, name string, arguments map[string]any) *sdkmcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

// resultText is a tool result's one text block.
func resultText(t *testing.T, res *sdkmcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("result has %d content blocks, want 1: %+v", len(res.Content), res)
	}
	text, _ := res.Content[0].(*sdkmcp.TextContent)
	if text == nil {
		t.Fatalf("result content is %T, want text", res.Content[0])
	}
	return text.Text
}

// restError decodes REST's {"error": ...} body.
type restError struct {
	Error string `json:"error"`
}

// decisionState is what one plan decision wrote, normalized so a decision
// over MCP and one over REST, on two sessions seeded alike, compare equal:
// ids become the role they play, and the MCP grant stamp on the audit row
// is taken out and returned apart.
type decisionState struct {
	PlanStatus     string
	DecidedByActor bool
	DecidedAtSet   bool
	// Implementation turns: every plan_mode=false turn, by what the
	// decision wrote into it (never its status, which the actor moves).
	Implementations []string
	Snapshots       int
	Audit           []string
	Outbox          []string
	OpenTurnStates  []string
}

// readDecisionState reads decisionState for plan planID of sessionID,
// decided (or not) by actor, and returns the MCP stamps the audit rows
// carried, one per row.
func readDecisionState(ctx context.Context, t *testing.T, rig *oauthRouterRig, sessionID, planID, actor pgtype.UUID) (decisionState, []map[string]any) {
	t.Helper()
	var st decisionState
	var decidedBy pgtype.UUID
	var decidedAt pgtype.Timestamptz
	if err := rig.pool.QueryRow(ctx, `SELECT status::text, decided_by, decided_at FROM plans WHERE id = $1`, planID).Scan(&st.PlanStatus, &decidedBy, &decidedAt); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	st.DecidedByActor = decidedBy.Valid && decidedBy == actor
	st.DecidedAtSet = decidedAt.Valid

	rows, err := rig.pool.Query(ctx, `SELECT id, coalesce(prompt, ''), coalesce(model_id, ''), coalesce(effort, '') FROM turns WHERE session_id = $1 AND NOT plan_mode ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatalf("read turns: %v", err)
	}
	implIDs := map[string]bool{}
	for rows.Next() {
		var id pgtype.UUID
		var prompt, model, effort string
		if err := rows.Scan(&id, &prompt, &model, &effort); err != nil {
			t.Fatal(err)
		}
		implIDs[id.String()] = true
		st.Implementations = append(st.Implementations, fmt.Sprintf("prompt=%q model=%q effort=%q", prompt, model, effort))
	}
	rows.Close()

	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM plan_documents WHERE plan_id = $1`, planID).Scan(&st.Snapshots); err != nil {
		t.Fatalf("read plan documents: %v", err)
	}

	var stamps []map[string]any
	rows, err = rig.pool.Query(ctx, `SELECT action, resource_type, resource_id, actor_user_id, detail_json FROM audit_log WHERE resource_id = $1 OR detail_json->>'session_id' = $2 ORDER BY created_at, action`, planID.String(), sessionID.String())
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	for rows.Next() {
		var action, resourceType, resourceID string
		var actorID pgtype.UUID
		var raw []byte
		if err := rows.Scan(&action, &resourceType, &resourceID, &actorID, &raw); err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode audit detail %s: %v", raw, err)
		}
		stamp, _ := detail["mcp"].(map[string]any)
		stamps = append(stamps, stamp)
		delete(detail, "mcp")
		for k, v := range detail {
			switch {
			case v == sessionID.String():
				detail[k] = "<session>"
			case v == planID.String():
				detail[k] = "<plan>"
			case implIDs[fmt.Sprint(v)]:
				detail[k] = "<implementation>"
			}
		}
		if resourceID == planID.String() {
			resourceID = "<plan>"
		} else if implIDs[resourceID] {
			resourceID = "<implementation>"
		}
		canon, _ := json.Marshal(detail)
		st.Audit = append(st.Audit, fmt.Sprintf("%s %s %s actor=%v %s", action, resourceType, resourceID, actorID == actor, canon))
	}
	rows.Close()

	rows, err = rig.pool.Query(ctx, `SELECT kind FROM outbox WHERE session_id = $1 ORDER BY kind`, sessionID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		st.Outbox = append(st.Outbox, kind)
	}
	rows.Close()

	rows, err = rig.pool.Query(ctx, `SELECT status::text FROM turns WHERE session_id = $1 AND plan_mode AND status IN ('pending', 'dispatched', 'processing') ORDER BY status`, sessionID)
	if err != nil {
		t.Fatalf("read open plan-mode turns: %v", err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		st.OpenTurnStates = append(st.OpenTurnStates, s)
	}
	rows.Close()
	return st, stamps
}

// sameState fails the test unless mcp and rest are the same state.
func sameState(t *testing.T, stage string, mcp, rest decisionState) {
	t.Helper()
	a, _ := json.Marshal(mcp)
	b, _ := json.Marshal(rest)
	if string(a) != string(b) {
		t.Fatalf("%s: the database differs after the MCP and the REST decision:\nMCP:  %s\nREST: %s", stage, a, b)
	}
}

// assertMCPStamps fails the test unless every audit row an MCP decision
// wrote carries the flow's grant and client, and the REST side's none.
func assertMCPStamps(ctx context.Context, t *testing.T, rig *oauthRouterRig, flow *sdkFlow, stage string, mcpStamps, restStamps []map[string]any) {
	t.Helper()
	grant := rig.grantOf(ctx, t, flow.member.ID)
	for _, s := range mcpStamps {
		if s == nil || s["grant_id"] != grant || s["client_id"] != flow.client.ClientId {
			t.Fatalf("%s: an audit row written over MCP carries %v, want the grant %s and the client %s", stage, s, grant, flow.client.ClientId)
		}
	}
	for _, s := range restStamps {
		if s != nil {
			t.Fatalf("%s: an audit row written by cookie carries an MCP stamp %v", stage, s)
		}
	}
}

// sdkApprovePlanParityEveryRole is TestOAuth_ProductionRouter's
// ApprovePlan_ParityEveryRole_SDKClient: for every role, on a session the
// user started and on one another member started, an approval over MCP
// and one by the same user's cookie over REST -- each on its own session,
// seeded alike -- answer the same (the body with ids aside, or the same
// refusal text) and leave the same database state. Who may approve is
// §13.3's: an admin or maintainer any plan, a member only their own, a
// viewer none -- a token never does more than its user.
func sdkApprovePlanParityEveryRole(t *testing.T, rig *oauthRouterRig) {
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleAdmin, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			ctx := oauthTestCtx(t)
			flow := rig.connectSDKClientAs(ctx, t, role, nil)
			other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
			for _, own := range []bool{true, false} {
				stage := fmt.Sprintf("%s, own session %v", role, own)
				creator := other.ID
				if own {
					creator = flow.member.ID
				}
				allowed := role == sqlcgen.UserRoleAdmin || role == sqlcgen.UserRoleMaintainer || (role == sqlcgen.UserRoleMember && own)

				mcpSession, mcpPlan := seedPlannedSession(ctx, t, rig, creator, sqlcgen.PlanStatusAwaitingApproval)
				restSession, restPlan := seedPlannedSession(ctx, t, rig, creator, sqlcgen.PlanStatusAwaitingApproval)

				res := callTool(ctx, t, flow.session, "narvi_approve_plan", map[string]any{"sessionId": mcpSession.String(), "planId": mcpPlan.String()})
				var restBody json.RawMessage
				restStatus := rig.doJSON(t, http.MethodPost, "/api/sessions/"+restSession.String()+"/plans/"+restPlan.String()+"/approve", nil, &restBody, flow.cookie)

				if allowed {
					if restStatus != http.StatusOK || res.IsError {
						t.Fatalf("%s: REST %d %s, MCP isError %v %+v -- want both to approve", stage, restStatus, restBody, res.IsError, res.Content)
					}
					got, _ := structured(t, res)
					var rest map[string]any
					if err := json.Unmarshal(restBody, &rest); err != nil {
						t.Fatal(err)
					}
					if got["planId"] != mcpPlan.String() || rest["planId"] != restPlan.String() || got["turnId"] == nil || rest["turnId"] == nil {
						t.Fatalf("%s: MCP %v, REST %v -- want each its own plan and a turn", stage, got, rest)
					}
					delete(got, "planId")
					delete(got, "turnId")
					delete(rest, "planId")
					delete(rest, "turnId")
					a, _ := json.Marshal(got)
					b, _ := json.Marshal(rest)
					if string(a) != string(b) || got["status"] != "approved" {
						t.Fatalf("%s: MCP %s, REST %s -- want the same approved body, ids aside", stage, a, b)
					}
				} else {
					var refused restError
					_ = json.Unmarshal(restBody, &refused)
					if restStatus != http.StatusForbidden || refused.Error != planForbidden || !res.IsError || resultText(t, res) != refused.Error {
						t.Fatalf("%s: REST %d %q, MCP isError %v %+v -- want both refused with %q", stage, restStatus, refused.Error, res.IsError, res.Content, planForbidden)
					}
				}

				mcpState, mcpStamps := readDecisionState(ctx, t, rig, mcpSession, mcpPlan, flow.member.ID)
				restState, restStamps := readDecisionState(ctx, t, rig, restSession, restPlan, flow.member.ID)
				sameState(t, stage, mcpState, restState)
				assertMCPStamps(ctx, t, rig, flow, stage, mcpStamps, restStamps)
				wantStatus, wantAudit := "awaiting_approval", 0
				if allowed {
					wantStatus, wantAudit = "approved", 1
				}
				if mcpState.PlanStatus != wantStatus || len(mcpState.Audit) != wantAudit || mcpState.DecidedByActor != allowed || len(mcpState.Implementations) != wantAudit || mcpState.Snapshots != wantAudit {
					t.Fatalf("%s: state %+v, want plan %s, decided by the user %v, %d implementation turn(s), snapshot(s) and audit row(s)", stage, mcpState, wantStatus, allowed, wantAudit)
				}
				if allowed && !strings.HasPrefix(mcpState.Audit[0], "plan.approve plan <plan> actor=true ") {
					t.Fatalf("%s: audit %v, want the user's plan.approve", stage, mcpState.Audit)
				}
			}
		})
	}
}

// sdkApprovePlanOpenTurnGate is TestOAuth_ProductionRouter's
// ApprovePlan_OpenTurnGate_SDKClient: while a request for changes to the
// plan is pending, dispatched or processing, an approval over MCP is
// refused with REST's own text, as REST refuses it, and nothing changes:
// the plan still awaits approval, nothing is queued, nothing audited, and
// the request for changes is still open.
func sdkApprovePlanOpenTurnGate(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	for _, open := range []sqlcgen.TurnStatus{sqlcgen.TurnStatusPending, sqlcgen.TurnStatusDispatched, sqlcgen.TurnStatusProcessing} {
		stage := "a " + string(open) + " revision"
		mcpSession, mcpPlan := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval)
		restSession, restPlan := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval)
		seedTurn(ctx, t, rig, mcpSession, open, true)
		seedTurn(ctx, t, rig, restSession, open, true)

		res := callTool(ctx, t, flow.session, "narvi_approve_plan", map[string]any{"sessionId": mcpSession.String(), "planId": mcpPlan.String()})
		var refused restError
		status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+restSession.String()+"/plans/"+restPlan.String()+"/approve", nil, &refused, flow.cookie)
		if status != http.StatusConflict || refused.Error != busyRefusal || !res.IsError || resultText(t, res) != busyRefusal {
			t.Fatalf("%s: REST %d %q, MCP isError %v %+v -- want both refused with %q", stage, status, refused.Error, res.IsError, res.Content, busyRefusal)
		}
		mcpState, _ := readDecisionState(ctx, t, rig, mcpSession, mcpPlan, flow.member.ID)
		restState, _ := readDecisionState(ctx, t, rig, restSession, restPlan, flow.member.ID)
		sameState(t, stage, mcpState, restState)
		if mcpState.PlanStatus != "awaiting_approval" || mcpState.DecidedAtSet || len(mcpState.Implementations) != 0 || len(mcpState.Audit) != 0 || len(mcpState.OpenTurnStates) != 1 || mcpState.OpenTurnStates[0] != string(open) {
			t.Fatalf("%s: state after the refused approval %+v, want the plan awaiting, nothing queued or audited, the revision still %s", stage, mcpState, open)
		}
	}
}

// sdkApprovePlanFirstVerdictWins is TestOAuth_ProductionRouter's
// ApprovePlan_FirstVerdictWinsAcrossRESTAndMCP: a plan decided over one
// channel is refused over the other, with REST's own text, and keeps the
// first verdict -- an approval after a rejection, either way round, and a
// rejection after an approval (which has no open-turn gate to hide behind:
// only the guarded update refuses it).
func sdkApprovePlanFirstVerdictWins(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	decideREST := func(session, plan pgtype.UUID, verdict string) (int, restError) {
		var body restError
		status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.String()+"/plans/"+plan.String()+"/"+verdict, nil, &body, flow.cookie)
		return status, body
	}
	decideMCP := func(session, plan pgtype.UUID, verdict string) *sdkmcp.CallToolResult {
		return callTool(ctx, t, flow.session, "narvi_"+verdict+"_plan", map[string]any{"sessionId": session.String(), "planId": plan.String()})
	}
	wantKept := func(stage string, session, plan pgtype.UUID, status string, implementations int) {
		t.Helper()
		st, _ := readDecisionState(ctx, t, rig, session, plan, flow.member.ID)
		if st.PlanStatus != status || !st.DecidedByActor || len(st.Implementations) != implementations || len(st.Audit) != 1 {
			t.Fatalf("%s: state %+v, want the first verdict (%s) kept, %d implementation turn(s), one audit row", stage, st, status, implementations)
		}
	}

	// Rejected over MCP, then approved over REST.
	s1, p1 := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval)
	if res := decideMCP(s1, p1, "reject"); res.IsError {
		t.Fatalf("reject over MCP: %+v", res.Content)
	}
	if status, body := decideREST(s1, p1, "approve"); status != http.StatusConflict || body.Error != notAwaitingRefusal {
		t.Fatalf("approve over REST after an MCP rejection: %d %q, want 409 %q", status, body.Error, notAwaitingRefusal)
	}
	wantKept("MCP reject, REST approve", s1, p1, "rejected", 0)

	// Rejected over REST, then approved over MCP.
	s2, p2 := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval)
	if status, body := decideREST(s2, p2, "reject"); status != http.StatusOK {
		t.Fatalf("reject over REST: %d %q", status, body.Error)
	}
	if res := decideMCP(s2, p2, "approve"); !res.IsError || resultText(t, res) != notAwaitingRefusal {
		t.Fatalf("approve over MCP after a REST rejection: %+v, want isError %q", res.Content, notAwaitingRefusal)
	}
	wantKept("REST reject, MCP approve", s2, p2, "rejected", 0)

	// Approved over MCP, then rejected over REST: the implementation is
	// queued, so only the guarded update stands between the rejection and
	// an approved plan turned rejected under its running work.
	s3, p3 := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval)
	if res := decideMCP(s3, p3, "approve"); res.IsError {
		t.Fatalf("approve over MCP: %+v", res.Content)
	}
	if status, body := decideREST(s3, p3, "reject"); status != http.StatusConflict || body.Error != notAwaitingRefusal {
		t.Fatalf("reject over REST after an MCP approval: %d %q, want 409 %q", status, body.Error, notAwaitingRefusal)
	}
	wantKept("MCP approve, REST reject", s3, p3, "approved", 1)

	// Approved over REST, then rejected over MCP.
	s4, p4 := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval)
	if status, body := decideREST(s4, p4, "approve"); status != http.StatusOK {
		t.Fatalf("approve over REST: %d %q", status, body.Error)
	}
	if res := decideMCP(s4, p4, "reject"); !res.IsError || resultText(t, res) != notAwaitingRefusal {
		t.Fatalf("reject over MCP after a REST approval: %+v, want isError %q", res.Content, notAwaitingRefusal)
	}
	wantKept("REST approve, MCP reject", s4, p4, "approved", 1)
}

// sessionTurns lists sessionID's turns as "status plan_mode" lines, oldest
// first, and counts its turn.create audit rows.
func sessionTurns(ctx context.Context, t *testing.T, rig *oauthRouterRig, sessionID pgtype.UUID) ([]string, int) {
	t.Helper()
	rows, err := rig.pool.Query(ctx, `SELECT status::text, plan_mode FROM turns WHERE session_id = $1 ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatalf("read turns: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var status string
		var planMode bool
		if err := rows.Scan(&status, &planMode); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s plan_mode=%v", status, planMode))
	}
	return out, rig.countOf(ctx, t, `SELECT count(*) FROM audit_log WHERE action = 'turn.create' AND detail_json->>'session_id' = $1`, sessionID.String())
}

// sdkRevisionMidImplementationRefused is TestOAuth_ProductionRouter's
// RevisionMidImplementation_RefusedAndStillRunning_SDKClient (technical
// plan §43.21): plan v1 is approved and its implementation is processing.
// A revision requested over MCP is refused with REST's own 409 text, as a
// revision by the same user's cookie over REST is: nothing is queued (no
// turn inserted, no turn.create audited), nothing cancelled (the
// implementation is still processing), nothing decided (v1 still approved,
// no v2). The token never gets a queue its user does not have.
func sdkRevisionMidImplementationRefused(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	const feedback = "use the env fallback after all"

	type side struct {
		session, plan, implementation pgtype.UUID
	}
	seed := func() side {
		session, plan := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusApproved)
		return side{session, plan, seedTurn(ctx, t, rig, session, sqlcgen.TurnStatusProcessing, false)}
	}
	mcpSide, restSide := seed(), seed()
	wantTurns := []string{"completed plan_mode=true", "processing plan_mode=false"}

	res := callTool(ctx, t, flow.session, "narvi_request_plan_revision", map[string]any{"sessionId": mcpSide.session.String(), "feedback": feedback})
	var refused restError
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+restSide.session.String()+"/turns", []byte(fmt.Sprintf(`{"prompt":%q,"modelId":null,"effort":null,"planMode":true}`, feedback)), &refused, flow.cookie)
	if status != http.StatusConflict || refused.Error != busyRefusal {
		t.Fatalf("REST revision mid-implementation: %d %q, want 409 %q", status, refused.Error, busyRefusal)
	}
	if !res.IsError || resultText(t, res) != busyRefusal {
		t.Fatalf("MCP revision mid-implementation: %+v, want isError %q", res.Content, busyRefusal)
	}

	for name, s := range map[string]side{"MCP": mcpSide, "REST": restSide} {
		turns, audited := sessionTurns(ctx, t, rig, s.session)
		if strings.Join(turns, ", ") != strings.Join(wantTurns, ", ") || audited != 0 {
			t.Fatalf("%s: turns %v with %d turn.create audit row(s) after the refused revision, want %v and none", name, turns, audited, wantTurns)
		}
		var implStatus string
		if err := rig.pool.QueryRow(ctx, `SELECT status::text FROM turns WHERE id = $1`, s.implementation).Scan(&implStatus); err != nil || implStatus != "processing" {
			t.Fatalf("%s: the implementation is %q (err %v), want still processing", name, implStatus, err)
		}
		var plans []string
		rows, err := rig.pool.Query(ctx, `SELECT version || ' ' || status::text FROM plans WHERE session_id = $1 ORDER BY version`, s.session)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			plans = append(plans, p)
		}
		rows.Close()
		if strings.Join(plans, ", ") != "1 approved" {
			t.Fatalf("%s: plans %v after the refused revision, want v1 still approved and nothing else", name, plans)
		}
	}
}

// sdkSendPromptWhileRunningRefused is TestOAuth_ProductionRouter's
// SendPrompt_WhileRunningRefusedLikeREST_SDKClient (owner decision O5,
// technical plan §43.21): with a turn processing, a prompt over MCP is
// refused with REST's own 409 text, as the same prompt by cookie is, and
// nothing is queued or audited -- never silently queued behind the running
// turn.
func sdkSendPromptWhileRunningRefused(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	const prompt = "and add a test for it"
	newBusy := func() pgtype.UUID {
		session, err := narvipg.NewSessionStore(rig.pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: flow.member.ID})
		if err != nil {
			t.Fatal(err)
		}
		seedTurn(ctx, t, rig, session.ID, sqlcgen.TurnStatusProcessing, false)
		return session.ID
	}
	mcpSession, restSession := newBusy(), newBusy()

	res := callTool(ctx, t, flow.session, "narvi_send_prompt", map[string]any{"sessionId": mcpSession.String(), "prompt": prompt})
	var refused restError
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+restSession.String()+"/turns", []byte(fmt.Sprintf(`{"prompt":%q,"modelId":null,"effort":null,"planMode":false}`, prompt)), &refused, flow.cookie)
	if status != http.StatusConflict || refused.Error != busyRefusal {
		t.Fatalf("REST prompt while running: %d %q, want 409 %q", status, refused.Error, busyRefusal)
	}
	if !res.IsError || resultText(t, res) != busyRefusal {
		t.Fatalf("MCP prompt while running: %+v, want isError %q", res.Content, busyRefusal)
	}
	for name, session := range map[string]pgtype.UUID{"MCP": mcpSession, "REST": restSession} {
		turns, audited := sessionTurns(ctx, t, rig, session)
		if strings.Join(turns, ", ") != "processing plan_mode=false" || audited != 0 {
			t.Fatalf("%s: turns %v with %d turn.create audit row(s), want the running turn alone and none", name, turns, audited)
		}
	}
}

// sdkPromptAndRevisionOnASettledSession is TestOAuth_ProductionRouter's
// PromptAndRevision_SettledSessionLikeREST_SDKClient: with nothing open,
// the two turn tools create what REST creates for the same user -- a
// prompt an ordinary turn, a revision a plan-mode turn that leaves the
// plan awaiting approval until it completes -- with the same answer, ids
// aside, the same turn row and the same audit row, which over MCP also
// carries the grant. The plan list answers REST's bytes.
func sdkPromptAndRevisionOnASettledSession(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	grant := rig.grantOf(ctx, t, flow.member.ID)

	for _, tc := range []struct {
		tool, field, text string
		planMode          bool
	}{
		{"narvi_send_prompt", "prompt", "add a changelog entry", false},
		{"narvi_request_plan_revision", "feedback", "split the migration in two", true},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			mcpSession, mcpPlan := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusRejected)
			restSession, restPlan := seedPlannedSession(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusRejected)
			if tc.planMode {
				// A revision is asked of a plan awaiting approval.
				for _, p := range []pgtype.UUID{mcpPlan, restPlan} {
					if _, err := rig.pool.Exec(ctx, `UPDATE plans SET status = 'awaiting_approval', decided_at = NULL, decided_by = NULL WHERE id = $1`, p); err != nil {
						t.Fatal(err)
					}
				}
			}

			res := callTool(ctx, t, flow.session, tc.tool, map[string]any{"sessionId": mcpSession.String(), tc.field: tc.text})
			var rest map[string]any
			status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+restSession.String()+"/turns", []byte(fmt.Sprintf(`{"prompt":%q,"modelId":null,"effort":null,"planMode":%v}`, tc.text, tc.planMode)), &rest, flow.cookie)
			if status != http.StatusCreated || res.IsError {
				t.Fatalf("REST %d %v, MCP %+v -- want both to create a turn", status, rest, res.Content)
			}
			got, _ := structured(t, res)
			mcpTurn, _ := got["id"].(string)
			restTurn, _ := rest["id"].(string)
			if mcpTurn == "" || restTurn == "" || got["status"] != rest["status"] || len(got) != len(rest) {
				t.Fatalf("MCP %v, REST %v -- want the same answer, ids aside", got, rest)
			}

			// The turn as its creation wrote it -- never its status, which the
			// actor moves -- with the session id, which a prompt may carry
			// (the upload note), written as its role.
			read := func(session pgtype.UUID, turn string) (string, map[string]any) {
				var row string
				if err := rig.pool.QueryRow(ctx, `SELECT 'plan_mode=' || plan_mode || ' prompt=' || coalesce(prompt, '') || ' model=' || coalesce(model_id, '') || ' effort=' || coalesce(effort, '') FROM turns WHERE id = $1`, turn).Scan(&row); err != nil {
					t.Fatalf("read turn %s: %v", turn, err)
				}
				row = strings.ReplaceAll(row, session.String(), "<session>")
				var raw []byte
				if err := rig.pool.QueryRow(ctx, `SELECT detail_json FROM audit_log WHERE action = 'turn.create' AND resource_id = $1 AND actor_user_id = $2`, turn, flow.member.ID).Scan(&raw); err != nil {
					t.Fatalf("read the turn.create audit row: %v", err)
				}
				var detail map[string]any
				if err := json.Unmarshal(raw, &detail); err != nil {
					t.Fatal(err)
				}
				if detail["session_id"] != session.String() || detail["plan_mode"] != tc.planMode {
					t.Fatalf("audit detail %s, want the session and plan_mode %v", raw, tc.planMode)
				}
				delete(detail, "session_id")
				return row, detail
			}
			mcpRow, mcpDetail := read(mcpSession, mcpTurn)
			restRow, restDetail := read(restSession, restTurn)
			stamp, _ := mcpDetail["mcp"].(map[string]any)
			if stamp == nil || stamp["grant_id"] != grant || stamp["client_id"] != flow.client.ClientId || restDetail["mcp"] != nil {
				t.Fatalf("MCP audit %v, REST audit %v -- want the grant stamp on the MCP row only", mcpDetail, restDetail)
			}
			delete(mcpDetail, "mcp")
			a, _ := json.Marshal(mcpDetail)
			b, _ := json.Marshal(restDetail)
			if mcpRow != restRow || string(a) != string(b) {
				t.Fatalf("MCP turn %q audit %s, REST turn %q audit %s -- want the same", mcpRow, a, restRow, b)
			}
			if !strings.Contains(mcpRow, fmt.Sprintf("plan_mode=%v prompt=", tc.planMode)) || !strings.Contains(mcpRow, tc.text) {
				t.Fatalf("MCP turn %q, want plan_mode %v carrying %q", mcpRow, tc.planMode, tc.text)
			}

			// The plan list, read over MCP and over REST, is the same bytes:
			// v1 still as it was (a revision supersedes it only once its turn
			// completes).
			list := callTool(ctx, t, flow.session, "narvi_list_plans", map[string]any{"sessionId": mcpSession.String()})
			var restList json.RawMessage
			if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+mcpSession.String()+"/plans", nil, &restList, flow.cookie); status != http.StatusOK || list.IsError {
				t.Fatalf("list plans: REST %d, MCP %+v", status, list.Content)
			}
			_, mcpList := structured(t, list)
			var x, y any
			_ = json.Unmarshal(mcpList, &x)
			_ = json.Unmarshal(restList, &y)
			ca, _ := json.Marshal(x)
			cb, _ := json.Marshal(y)
			if string(ca) != string(cb) {
				t.Fatalf("narvi_list_plans %s, REST %s -- want the same bytes", ca, cb)
			}
			var listed struct {
				Plans []struct {
					ID      string `json:"id"`
					Version int    `json:"version"`
					Status  string `json:"status"`
				} `json:"plans"`
			}
			_ = json.Unmarshal(restList, &listed)
			wantStatus := "rejected"
			if tc.planMode {
				wantStatus = "awaiting_approval"
			}
			if len(listed.Plans) != 1 || listed.Plans[0].ID != mcpPlan.String() || listed.Plans[0].Status != wantStatus {
				t.Fatalf("plans %+v, want v1 alone, %s", listed.Plans, wantStatus)
			}
		})
	}
}

// writeToolNames is every write tool, sorted.
func writeToolNames() []string {
	names := []string{"narvi_create_session", "narvi_approve_plan", "narvi_reject_plan", "narvi_request_plan_revision", "narvi_send_prompt"}
	sort.Strings(names)
	return names
}

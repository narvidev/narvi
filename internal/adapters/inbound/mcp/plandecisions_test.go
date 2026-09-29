package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
)

// This file pins what the plan and turn tools (technical plan §43.21), and
// the stop tool (§43.22), hand their twins: the twin's own method and path,
// the chi URL params the REST handler reads (sessionID, planId), no query, a
// body only for the turn tools -- the restdtos.CreateTurnRequest the tool
// built, planMode its own constant -- and never a header. Everything else
// (roles, the open-turn gate, the guarded update, the stop's walk, the audit
// row) is the twin's, and is proven against the real handlers on the
// production router (controlplane/mcp_plandecisions_integration_test.go,
// controlplane/mcp_stopsession_integration_test.go).

const (
	planTestSessionID = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
	planTestPlanID    = "6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"
)

// seenTwinCall is what a recording twin saw of one synthesized request.
type seenTwinCall struct {
	method    string
	path      string
	rawQuery  string
	sessionID string
	planID    string
	header    http.Header
	body      []byte
}

// recordingTwin answers status with body, recording each request.
func recordingTwin(mu *sync.Mutex, seen *[]seenTwinCall, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, seenTwinCall{
			method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
			sessionID: chi.URLParam(r, "sessionID"), planID: chi.URLParam(r, "planId"),
			header: r.Header.Clone(), body: raw,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// turnBody is the JSON the turn twin must receive for these fields.
func turnBody(t *testing.T, prompt string, modelID, effort *string, planMode bool) []byte {
	t.Helper()
	b, err := json.Marshal(restdtos.CreateTurnRequest{
		Prompt:   prompt,
		ModelId:  restdtos.CreateTurnRequestModelId(modelID),
		Effort:   restdtos.CreateTurnRequestEffort(effort),
		PlanMode: planMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPlanAndTurnTools_TwinGetsItsOwnRequest drives each tool through the
// full handler, with a bearer token and a cookie on the MCP request, and
// checks the one request its twin received.
func TestPlanAndTurnTools_TwinGetsItsOwnRequest(t *testing.T) {
	model, effort := "m", "high"
	tests := []struct {
		name       string
		tool       string
		arguments  string
		twinStatus int
		twinBody   string
		wantMethod string
		wantPath   string
		wantPlanID string
		wantBody   []byte // nil: no body
	}{
		{
			name: "list plans", tool: "narvi_list_plans",
			arguments:  `{"sessionId":"` + planTestSessionID + `"}`,
			twinStatus: http.StatusOK, twinBody: `{"plans":[]}`,
			wantMethod: http.MethodGet, wantPath: "/api/sessions/" + planTestSessionID + "/plans",
		},
		{
			name: "approve", tool: "narvi_approve_plan",
			arguments:  `{"sessionId":"` + planTestSessionID + `","planId":"` + planTestPlanID + `"}`,
			twinStatus: http.StatusOK, twinBody: `{"planId":"` + planTestPlanID + `","status":"approved","turnId":"t"}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/plans/" + planTestPlanID + "/approve", wantPlanID: planTestPlanID,
		},
		{
			name: "reject", tool: "narvi_reject_plan",
			arguments:  `{"sessionId":"` + planTestSessionID + `","planId":"` + planTestPlanID + `"}`,
			twinStatus: http.StatusOK, twinBody: `{"planId":"` + planTestPlanID + `","status":"rejected","turnId":null}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/plans/" + planTestPlanID + "/reject", wantPlanID: planTestPlanID,
		},
		{
			name: "revision, every field", tool: "narvi_request_plan_revision",
			arguments:  `{"sessionId":"` + planTestSessionID + `","feedback":"keep the env fallback","modelId":"m","effort":"high"}`,
			twinStatus: http.StatusCreated, twinBody: `{"id":"t","status":"pending"}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/turns",
			wantBody: turnBody(t, "keep the env fallback", &model, &effort, true),
		},
		{
			name: "revision, required fields only", tool: "narvi_request_plan_revision",
			arguments:  `{"sessionId":"` + planTestSessionID + `","feedback":"keep the env fallback"}`,
			twinStatus: http.StatusCreated, twinBody: `{"id":"t","status":"pending"}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/turns",
			wantBody: turnBody(t, "keep the env fallback", nil, nil, true),
		},
		{
			name: "prompt, every field", tool: "narvi_send_prompt",
			arguments:  `{"sessionId":"` + planTestSessionID + `","prompt":"run the tests again","modelId":"m","effort":"high"}`,
			twinStatus: http.StatusCreated, twinBody: `{"id":"t","status":"pending"}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/turns",
			wantBody: turnBody(t, "run the tests again", &model, &effort, false),
		},
		{
			name: "prompt, required fields only", tool: "narvi_send_prompt",
			arguments:  `{"sessionId":"` + planTestSessionID + `","prompt":"run the tests again"}`,
			twinStatus: http.StatusCreated, twinBody: `{"id":"t","status":"pending"}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/turns",
			wantBody: turnBody(t, "run the tests again", nil, nil, false),
		},
		{
			// 202, as POST .../stop answers: accepted, its body verbatim.
			name: "stop", tool: "narvi_stop_session",
			arguments:  `{"sessionId":"` + planTestSessionID + `"}`,
			twinStatus: http.StatusAccepted, twinBody: `{"sessionId":"` + planTestSessionID + `","requestedAt":"2026-01-01T00:00:00Z","reachedSessionIds":["` + planTestSessionID + `"],"openTurns":1}`,
			wantMethod: http.MethodPost, wantPath: "/api/sessions/" + planTestSessionID + "/stop",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []seenTwinCall
			record := recordingTwin(&mu, &seen, tc.twinStatus, tc.twinBody)
			twins := testTwins()
			twins.ListPlans, twins.ApprovePlan, twins.RejectPlan, twins.CreateTurn, twins.StopSession = record, record, record, record, record
			headers := callToolHeaders(tc.tool)
			headers["Authorization"] = "Bearer narvi_mcp_at_must-never-reach-a-twin"
			headers["Cookie"] = "narvi_auth_session=also-never"
			handler := handlerWithHeaders(newTestHandlerWithGrant(t, twins, scopes("mcp:read", "mcp:write")), headers)

			text, isError := postToolCall(t, handler, tc.tool, tc.arguments)
			if isError || text != tc.twinBody {
				t.Fatalf("result = (IsError %v, %q), want the twin's %d body %q as a success", isError, text, tc.twinStatus, tc.twinBody)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("twins called %d times, want 1", len(seen))
			}
			got := seen[0]
			if got.method != tc.wantMethod || got.path != tc.wantPath || got.rawQuery != "" {
				t.Errorf("twin got %s %s?%s, want %s %s with no query", got.method, got.path, got.rawQuery, tc.wantMethod, tc.wantPath)
			}
			if got.sessionID != planTestSessionID || got.planID != tc.wantPlanID {
				t.Errorf("twin read sessionID %q planId %q, want %q and %q", got.sessionID, got.planID, planTestSessionID, tc.wantPlanID)
			}
			if len(got.header) != 0 {
				t.Errorf("twin received headers %v, want none at all", got.header)
			}
			if string(got.body) != string(tc.wantBody) {
				t.Errorf("twin body = %s, want %s", got.body, tc.wantBody)
			}
		})
	}
}

// TestTurnTools_OpenTurnRefusalIsAnIsErrorAndIsNotRetried: the turn twin's
// 409 while a turn is open -- REST's own refusal, RejectIfOpen -- reaches
// the client as isError with REST's own text, after exactly one twin call:
// the adapter neither retries it nor turns it into a queue (technical plan
// §43.21, owner decision O5). The same holds for the approve twin's own
// open-turn 409.
func TestTurnTools_OpenTurnRefusalIsAnIsErrorAndIsNotRetried(t *testing.T) {
	const busy = "a turn is already pending, dispatched, or processing for this session"
	for tool, arguments := range map[string]string{
		"narvi_send_prompt":           `{"sessionId":"` + planTestSessionID + `","prompt":"again"}`,
		"narvi_request_plan_revision": `{"sessionId":"` + planTestSessionID + `","feedback":"change it"}`,
		"narvi_approve_plan":          `{"sessionId":"` + planTestSessionID + `","planId":"` + planTestPlanID + `"}`,
	} {
		t.Run(tool, func(t *testing.T) {
			var mu sync.Mutex
			var seen []seenTwinCall
			refuse := recordingTwin(&mu, &seen, http.StatusConflict, `{"error":"`+busy+`"}`)
			twins := testTwins()
			twins.ApprovePlan, twins.CreateTurn = refuse, refuse
			handler := newTestHandlerWithGrant(t, twins, scopes("mcp:read", "mcp:write"))

			text, isError := postToolCall(t, handler, tool, arguments)
			if !isError || text != busy {
				t.Fatalf("result = (IsError %v, %q), want isError with REST's own text %q", isError, text, busy)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("twin called %d times, want exactly 1 -- a refusal is answered, never retried", len(seen))
			}
		})
	}
}

// TestStopTool_OutcomesFollowTheTable: the stop twin's outcomes reach the
// client through mapOutcome's one table, after exactly one twin call each
// -- a refusal is never retried and a partial walk never repeated by the
// adapter (technical plan §43.22). 403 (a role or review-session refusal)
// and 404 are isError with REST's own text; the 500 that says not every
// session the stop started could be reached is -32603, its body not
// leaked, like any other server error.
func TestStopTool_OutcomesFollowTheTable(t *testing.T) {
	const partial = "the session was stopped, but not every session it started could be reached: repeat the request"
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError string // "" for the -32603 row
	}{
		{"forbidden", http.StatusForbidden, `{"error":"forbidden"}`, "forbidden"},
		{"not found", http.StatusNotFound, `{"error":"session not found"}`, "session not found"},
		{"a descendant not reached", http.StatusInternalServerError, `{"error":"` + partial + `"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []seenTwinCall
			twins := testTwins()
			twins.StopSession = recordingTwin(&mu, &seen, tc.status, tc.body)
			handler := newTestHandlerWithGrant(t, twins, scopes("mcp:read", "mcp:write"))
			arguments := `{"sessionId":"` + planTestSessionID + `"}`
			if tc.wantError != "" {
				text, isError := postToolCall(t, handler, "narvi_stop_session", arguments)
				if !isError || text != tc.wantError {
					t.Fatalf("result = (IsError %v, %q), want isError with REST's own text %q", isError, text, tc.wantError)
				}
			} else {
				status, raw := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_stop_session", arguments), callToolHeaders("narvi_stop_session"))
				var env jsonrpcEnvelope
				if err := json.Unmarshal(raw, &env); err != nil || status != http.StatusOK || env.Error == nil || env.Error.Code != -32603 || env.Error.Message != "internal error" {
					t.Fatalf("status %d body %s, want a -32603 internal error", status, raw)
				}
				if strings.Contains(string(raw), partial) {
					t.Fatalf("body %s leaks the twin's 500 text", raw)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("twin called %d times, want exactly 1", len(seen))
			}
		})
	}
}

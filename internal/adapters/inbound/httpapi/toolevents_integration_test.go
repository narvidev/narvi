//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// toolEventIdentity names an event of msg_1 by its type and the correlator
// that tells one of its kind from another: "tool_call call_read".
func toolEventIdentity(t *testing.T, payload json.RawMessage) (messageID, identity string) {
	t.Helper()
	var e struct {
		Type      string `json:"type"`
		MessageID string `json:"messageId"`
		CallID    string `json:"callId"`
		StepID    string `json:"stepId"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	correlator := e.CallID
	if correlator == "" {
		correlator = e.StepID
	}
	return e.MessageID, e.Type + " " + correlator
}

// TestEvents_ToolEventsOfOneMessage_ReturnedByEventsAndHistory: one
// assistant message's step_start, tool_call, tool_result and step_finish,
// written on the real sandbox socket in the shape the runtime adapter
// emits -- every one under the message's id, the step_start first -- are
// read back through both pages a client reads, GET
// /api/sessions/{id}/events and fetch_history on the real client socket,
// all four rows in each. Under the wire messageId alone the three after
// the step_start were never stored, and neither page held any tool
// activity.
func TestEvents_ToolEventsOfOneMessage_ReturnedByEventsAndHistory(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	sid := session.ID.String()
	_, cookie := rig.createAuthenticatedUser(ctx, t)
	if _, err := rig.sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// The message's turn is Processing, dispatched as tryPlanDispatch stamps
	// one: the actor stores these events only while their turn is live.
	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	watermark, err := rig.events.MaxEventIDForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("MaxEventIDForSession: %v", err)
	}
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedEventID: &watermark}); err != nil {
		t.Fatalf("stamp dispatched_event_id: %v", err)
	}

	// The control plane's own socket route, both handlers on it.
	timeouts := platform.DefaultTimeouts()
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewHandler(
		wshub.NewSandboxHandler(rig.registry, rig.sandboxes, wshub.NewSandboxRegistry(timeouts), timeouts),
		wshub.NewClientHandler(rig.registry, rig.sessions, rig.turns, rig.sandboxes, rig.events, rig.artifacts, rig.wsTokens, rig.users, wshub.NewHub(), timeouts),
	))
	sockets := httptest.NewServer(router)
	t.Cleanup(sockets.Close)
	wsURL := "ws" + strings.TrimPrefix(sockets.URL, "http") + "/sessions/" + sid + "/ws"

	header := http.Header{}
	header.Set("Authorization", "Bearer sandbox-token") // the fresh row's NULL token_hash admits any
	header.Set("X-Sandbox-ID", "sbx-tool-events")
	header.Set("X-Sandbox-Gen", "1")
	sandbox, _, err := websocket.Dial(ctx, wsURL+"?type=sandbox", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the sandbox socket: %v", err)
	}
	defer func() { _ = sandbox.CloseNow() }()
	usd := 0.0125
	for _, evt := range []any{
		sandboxws.StepStart{Type: "step_start", MessageId: "msg_1", SessionId: sid, Gen: 1, StepId: "prt_step"},
		sandboxws.ToolCall{Type: "tool_call", MessageId: "msg_1", SessionId: sid, Gen: 1, CallId: "call_read", ToolName: "read", Input: sandboxws.ToolCallInput{"filePath": "main.go"}},
		sandboxws.ToolResult{Type: "tool_result", MessageId: "msg_1", SessionId: sid, Gen: 1, CallId: "call_read", Output: sandboxws.ToolResultOutput{"output": "package main"}},
		sandboxws.StepFinish{Type: "step_finish", MessageId: "msg_1", SessionId: sid, Gen: 1, StepId: "prt_finish",
			Cost: sandboxws.StepFinishCost{Tokens: sandboxws.StepFinishCostTokens{Input: 1200, Output: 80}, Usd: &usd}},
	} {
		raw, err := json.Marshal(evt)
		if err != nil {
			t.Fatalf("marshal %T: %v", evt, err)
		}
		if err := sandbox.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatalf("write %T: %v", evt, err)
		}
	}
	want := []string{"step_finish prt_finish", "step_start prt_step", "tool_call call_read", "tool_result call_read"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND payload->>'messageId' = 'msg_1'`, session.ID).Scan(&n); err != nil {
			t.Fatalf("count msg_1's rows: %v", err)
		}
		if n >= len(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("msg_1 left %d rows within 10s, want %d", n, len(want))
		}
		time.Sleep(20 * time.Millisecond)
	}

	pick := func(payloads []json.RawMessage) []string {
		var got []string
		for _, p := range payloads {
			if messageID, identity := toolEventIdentity(t, p); messageID == "msg_1" {
				got = append(got, identity)
			}
		}
		sort.Strings(got)
		return got
	}

	// GET /api/sessions/{id}/events.
	req, err := http.NewRequest(http.MethodGet, rig.server.URL+"/api/sessions/"+sid+"/events?limit=100", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	var page struct {
		Events []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&page)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil {
		t.Fatalf("GET events = %d, %v; want 200 and a page", resp.StatusCode, decodeErr)
	}
	var restPayloads []json.RawMessage
	for _, e := range page.Events {
		restPayloads = append(restPayloads, e.Payload)
	}
	if got := pick(restPayloads); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("GET events holds msg_1's %v, want %v", got, want)
	}

	// fetch_history on the real client socket.
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate ws token: %v", err)
	}
	if _, err := rig.wsTokens.Create(ctx, sqlcgen.CreateWSTokenParams{SessionID: session.ID, TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}); err != nil {
		t.Fatalf("store ws token: %v", err)
	}
	client, _, err := websocket.Dial(ctx, wsURL+"?type=client", nil)
	if err != nil {
		t.Fatalf("dial the client socket: %v", err)
	}
	defer func() { _ = client.CloseNow() }()
	client.SetReadLimit(4 * platform.FetchHistoryMaxReplyBytes)
	read := func(what string) []byte {
		t.Helper()
		rc, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, data, err := client.Read(rc)
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		return data
	}
	subscribe, err := json.Marshal(clientws.SubscribeRequest{Token: token, ClientId: "tool-events"})
	if err != nil {
		t.Fatalf("marshal subscribe: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, subscribe); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
	read("the subscribed reply")
	if err := client.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":null,"limit":100}`, sid))); err != nil {
		t.Fatalf("write fetch_history: %v", err)
	}
	var history clientws.FetchHistoryResponse
	if err := json.Unmarshal(read("the fetch_history reply"), &history); err != nil {
		t.Fatalf("decode the fetch_history reply: %v", err)
	}
	raw, err := json.Marshal(history.Events)
	if err != nil {
		t.Fatalf("re-encode the history's events: %v", err)
	}
	var historyEvents []struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &historyEvents); err != nil {
		t.Fatalf("decode the history's events: %v", err)
	}
	var historyPayloads []json.RawMessage
	for _, e := range historyEvents {
		historyPayloads = append(historyPayloads, e.Payload)
	}
	if got := pick(historyPayloads); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fetch_history holds msg_1's %v, want %v", got, want)
	}
}

//go:build integration

// The events of one assistant message, as a real runtime turn produces
// them, reach the event log, a live page and the history every reader
// pages (technical plan §6.1, §6.2; docs/IMPLEMENTATION_PLAN.md row 229).
//
// The runtime adapter gives every event it derives from a part of an
// assistant message that message's id, and the message's step_start comes
// first. Stored under the wire messageId alone, every tool_call,
// tool_result and step_finish of a real turn deduped onto that step_start's
// row: none was stored or broadcast, so the timeline showed no tool call,
// the cost panel no cost, and the history held no tool activity. The tests
// that should have seen it minted a fresh id per event. This one runs the
// real OpenCode adapter (internal/adapters/outbound/opencode) against a
// scripted runtime server, so the events are the ones its translate path
// emits, through a real wsbridge.Bridge, the real wshub handlers and the
// real session actor on Postgres.
package resilience_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/opencode"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// scriptedRuntime is an agent runtime's HTTP server as the OpenCode adapter
// speaks to it: one conversation, ses_main, and on its prompt the SSE
// script of one assistant message, msg_1, then the session going idle.
type scriptedRuntime struct {
	script []string
	lines  chan string
}

func newScriptedRuntime(script []string) *scriptedRuntime {
	return &scriptedRuntime{script: script, lines: make(chan string, len(script)+1)}
}

func (s *scriptedRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/event":
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-s.lines:
				_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
				flusher.Flush()
			}
		}
	case r.Method == http.MethodPost && r.URL.Path == "/session":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"ses_main"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/session/ses_main/prompt_async":
		for _, line := range s.script {
			s.lines <- line
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// oneMessageScript is the SSE the runtime streams for one assistant
// message of the main conversation: its step-start, a text part (empty
// when it opens, whole when it closes), a `read` call and a `task` call
// that spawns a sub-agent, each running and then completed, and its
// step-finish; then the conversation goes idle, which ends the turn.
func oneMessageScript() []string {
	part := func(body string) string {
		return `{"type":"message.part.updated","properties":{"sessionID":"ses_main","part":{"messageID":"msg_1","sessionID":"ses_main",` + body + `}}}`
	}
	task := func(status, extra string) string {
		return part(`"id":"prt_task","type":"tool","tool":"task","callID":"call_task","state":{"status":"` + status +
			`","input":{"description":"Second opinion","prompt":"Review the change.","subagent_type":"counter-reviewer"},` +
			`"metadata":{"parentSessionId":"ses_main","sessionId":"ses_child"}` + extra + `}`)
	}
	return []string{
		`{"type":"message.updated","properties":{"sessionID":"ses_main","info":{"id":"msg_1","role":"assistant"}}}`,
		part(`"id":"prt_step","type":"step-start"`),
		part(`"id":"prt_text","type":"text","text":""`),
		part(`"id":"prt_text","type":"text","text":"Reading the file, then asking for a second opinion."`),
		part(`"id":"prt_read","type":"tool","tool":"read","callID":"call_read","state":{"status":"pending","input":{}}`),
		part(`"id":"prt_read","type":"tool","tool":"read","callID":"call_read","state":{"status":"running","input":{"filePath":"main.go"}}`),
		part(`"id":"prt_read","type":"tool","tool":"read","callID":"call_read","state":{"status":"completed","input":{"filePath":"main.go"},"output":"package main"}`),
		task("running", ""),
		task("completed", `,"output":"Looks right."`),
		part(`"id":"prt_finish","type":"step-finish","reason":"stop","tokens":{"input":1200,"output":80,"reasoning":0,"cache":{"read":300,"write":0}},"cost":0.42`),
		`{"type":"session.idle","properties":{"sessionID":"ses_main"}}`,
	}
}

// wantMessageRows is every row msg_1 leaves once stored, keyed: its
// step_start under the bare id, and each tool_call, tool_result and
// step_finish under the key its callId or stepId derives.
var wantMessageRows = []string{
	"step_start msg_1",
	"tool_call msg_1#tool_call:call_read",
	"tool_result msg_1#tool_result:call_read",
	"tool_call msg_1#tool_call:call_task",
	"tool_result msg_1#tool_result:call_task",
	"step_finish msg_1#step_finish:prt_finish",
}

// messageRows returns the session's rows of msg_1, by the payload's
// messageId, each as "type storage-key", in id order.
func messageRows(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) []string {
	t.Helper()
	rows, err := h.Pool.Query(ctx, `SELECT type, message_id FROM events WHERE session_id = $1 AND payload->>'messageId' = 'msg_1' ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("query msg_1's rows: %v", err)
	}
	var out []string
	for rows.Next() {
		var eventType, key string
		if err := rows.Scan(&eventType, &key); err != nil {
			rows.Close()
			t.Fatalf("scan msg_1's row: %v", err)
		}
		out = append(out, eventType+" "+key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate msg_1's rows: %v", err)
	}
	return out
}

// wireIdentity names a sandbox event by its type and the correlator that
// tells one of its kind from another in msg_1: "tool_call call_read".
func wireIdentity(payload []byte) string {
	var e struct {
		Type   string `json:"type"`
		CallID string `json:"callId"`
		StepID string `json:"stepId"`
	}
	if json.Unmarshal(payload, &e) != nil {
		return ""
	}
	switch {
	case e.CallID != "":
		return e.Type + " " + e.CallID
	case e.StepID != "":
		return e.Type + " " + e.StepID
	default:
		return e.Type
	}
}

// subscribedPage is a web client on the real client socket: subscribed,
// with every frame it receives queued on frames by one reader.
type subscribedPage struct {
	conn   *websocket.Conn
	frames chan []byte
}

func subscribePage(ctx context.Context, t *testing.T, rig *eventFrameRig, group *errgroup.Group) *subscribedPage {
	t.Helper()
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate ws token: %v", err)
	}
	if _, err := narvipg.NewWSTokenStore(rig.h.Pool).Create(ctx, sqlcgen.CreateWSTokenParams{
		SessionID: rig.sessionID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("store ws token: %v", err)
	}
	conn, _, err := websocket.Dial(ctx, rig.clientURL, nil)
	if err != nil {
		t.Fatalf("dial the client socket: %v", err)
	}
	conn.SetReadLimit(4 * platform.FetchHistoryMaxReplyBytes)
	subscribe, err := json.Marshal(clientws.SubscribeRequest{Token: token, ClientId: "tool-events-page"})
	if err != nil {
		t.Fatalf("marshal subscribe: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, subscribe); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, scenario23Wait)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err != nil {
		t.Fatalf("read the subscribed reply: %v", err)
	}
	page := &subscribedPage{conn: conn, frames: make(chan []byte, 256)}
	group.Go(func() error {
		defer close(page.frames)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return nil
			}
			select {
			case page.frames <- data:
			case <-ctx.Done():
				return nil
			}
		}
	})
	return page
}

// broadcastsUntil collects the sandbox events broadcast to the page until
// the one whose messageId is sentinel, which it does not return.
func (p *subscribedPage) broadcastsUntil(t *testing.T, sentinel string) []string {
	t.Helper()
	var got []string
	deadline := time.After(scenario23Wait)
	for {
		select {
		case data, ok := <-p.frames:
			if !ok {
				t.Fatalf("the page's socket closed before the %s broadcast; got %v", sentinel, got)
			}
			var e struct {
				MessageID string `json:"messageId"`
			}
			if json.Unmarshal(data, &e) == nil && e.MessageID == sentinel {
				return got
			}
			got = append(got, wireIdentity(data))
		case <-deadline:
			t.Fatalf("no %s broadcast within %v; got %v", sentinel, scenario23Wait, got)
		}
	}
}

// fetchHistory sends fetch_history from the start of the log and returns
// the page the reply holds, each event as "type messageId correlator".
func (p *subscribedPage) fetchHistory(ctx context.Context, t *testing.T, sessionID string) []string {
	t.Helper()
	request := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":null,"limit":100}`, sessionID)
	if err := p.conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatalf("write fetch_history: %v", err)
	}
	deadline := time.After(scenario23Wait)
	for {
		select {
		case data, ok := <-p.frames:
			if !ok {
				t.Fatal("the page's socket closed before the fetch_history reply")
			}
			var keys struct {
				Events *json.RawMessage `json:"events"`
			}
			if json.Unmarshal(data, &keys) != nil || keys.Events == nil {
				continue // a broadcast
			}
			var reply struct {
				Events []struct {
					Type    string          `json:"type"`
					Payload json.RawMessage `json:"payload"`
				} `json:"events"`
			}
			if err := json.Unmarshal(data, &reply); err != nil {
				t.Fatalf("decode the fetch_history reply: %v", err)
			}
			var out []string
			for _, e := range reply.Events {
				var m struct {
					MessageID string `json:"messageId"`
				}
				if json.Unmarshal(e.Payload, &m) != nil || m.MessageID != "msg_1" {
					continue
				}
				out = append(out, wireIdentity(e.Payload))
			}
			return out
		case <-deadline:
			t.Fatalf("no fetch_history reply within %v", scenario23Wait)
		}
	}
}

// toolEventsFixture is the turn the test below leaves in the log, as the
// web client receives it; the web tests read the same file
// (web/src/session/__tests__/timelineModel.test.ts, costRollup.test.ts,
// timelineRendering.test.tsx), so what the page draws is pinned against
// the rows the real handler and actor store. Set NARVI_UPDATE_FIXTURES=1
// to rewrite it from a run.
const toolEventsFixture = "../../web/src/session/__tests__/fixtures/toolEventsOfOneMessage.json"

// turnRowsAsFixture returns the session's turn-scoped rows as the web
// client receives them, with what varies between runs pinned: ids
// renumbered from 1, the session id replaced, one createdAt, and the fresh
// ids the adapter mints for a sub-task's bracket and the turn's end
// replaced by their type and position.
func (r *eventFrameRig) turnRowsAsFixture(ctx context.Context, t *testing.T) []fixtureEvent {
	t.Helper()
	rows, err := r.h.Pool.Query(ctx, `SELECT type, payload FROM events WHERE session_id = $1
		AND type IN ('step_start', 'token', 'tool_call', 'tool_result', 'sub_task_start', 'sub_task_finish', 'step_finish', 'execution_complete')
		ORDER BY id`, r.sessionID)
	if err != nil {
		t.Fatalf("query the turn's rows: %v", err)
	}
	type raw struct {
		eventType string
		payload   []byte
	}
	var scanned []raw
	for rows.Next() {
		var row raw
		if err := rows.Scan(&row.eventType, &row.payload); err != nil {
			rows.Close()
			t.Fatalf("scan event row: %v", err)
		}
		scanned = append(scanned, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event rows: %v", err)
	}
	minted := map[string]int{}
	out := make([]fixtureEvent, 0, len(scanned))
	for _, row := range scanned {
		var payload map[string]any
		if err := json.Unmarshal(row.payload, &payload); err != nil {
			t.Fatalf("decode %s payload: %v", row.eventType, err)
		}
		payload["sessionId"] = scenario23FixtureSessionID
		switch row.eventType {
		case "sub_task_start", "sub_task_finish", "execution_complete":
			minted[row.eventType]++
			id := fmt.Sprintf("%s-%d", row.eventType, minted[row.eventType])
			payload["messageId"] = id
			if _, ok := payload["ackId"]; ok {
				payload["ackId"] = row.eventType + ":" + id
			}
		}
		out = append(out, fixtureEvent{ID: len(out) + 1, Type: row.eventType, Payload: payload, CreatedAt: "2026-10-03T10:00:00Z"})
	}
	return out
}

// TestResilience_ToolEventsOfOneMessage_EachStoredOnce: the events the
// real translate path emits for one assistant message -- a read call and a
// task call that spawns a sub-task, each with its result, between a
// step_start and a step_finish -- through a real Bridge, the real wshub
// handler and the real actor: each tool_call, tool_result and the
// step_finish is stored once, under its derived key, and broadcast once to
// a page subscribed before the turn; a reconnect's replay adds no row and
// broadcasts none of them again; and a fetch_history on the real client
// socket returns the message's step_start, tool_calls, tool_results and
// step_finish.
func TestResilience_ToolEventsOfOneMessage_EachStoredOnce(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{first: headerControlPlane, fallback: headerControlPlane, clientSocket: true})

	runtime := httptest.NewServer(newScriptedRuntime(oneMessageScript()))
	t.Cleanup(runtime.Close)
	adapter := opencode.New(runtime.URL, 30*time.Second, 50*time.Millisecond, 5*time.Second, 5*time.Second, 10*time.Millisecond, "test-runtime", "sbx-tool-events")
	t.Cleanup(adapter.Close) // before runtime.Close: it ends the event stream's request

	var pages errgroup.Group
	pageCtx, stopPage := context.WithCancel(ctx)
	page := subscribePage(pageCtx, t, rig, &pages)
	t.Cleanup(func() {
		stopPage()
		_ = page.conn.CloseNow()
		_ = pages.Wait()
	})

	agent := startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.answerSnapshots = true
		a.onPrompt = func(a *scriptedAgent, _ int, cmd sandboxws.Prompt) {
			sink := func(event ports.AgentEvent) {
				if event.Critical {
					a.critical(event.Payload, event.AckID)
					return
				}
				a.best(event.Payload)
			}
			if _, err := adapter.StartTurn(a.ctx, cmd, sink, nil); err != nil {
				a.t.Errorf("StartTurn: %v", err)
			}
		}
	})

	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	if got := messageRows(ctx, t, rig.h, rig.sessionID); strings.Join(got, "|") != strings.Join(wantMessageRows, "|") {
		t.Fatalf("msg_1's stored rows = %q, want %q", got, wantMessageRows)
	}
	for eventType, want := range map[string]int{"sub_task_start": 1, "sub_task_finish": 1, "token": 2} {
		if got := rig.countType(ctx, t, eventType); got != want {
			t.Errorf("%d %s rows, want %d", got, eventType, want)
		}
	}
	var parentCallID string
	if err := rig.h.Pool.QueryRow(ctx, `SELECT COALESCE(payload->>'parentCallId', '') FROM events WHERE session_id = $1 AND type = 'sub_task_start'`,
		rig.sessionID).Scan(&parentCallID); err != nil {
		t.Fatalf("read the sub_task_start: %v", err)
	}
	if parentCallID != "call_task" {
		t.Errorf("sub_task_start parentCallId = %q, want call_task, the task call that spawned it", parentCallID)
	}
	gotJSON, err := json.MarshalIndent(rig.turnRowsAsFixture(ctx, t), "", "  ")
	if err != nil {
		t.Fatalf("marshal the turn's rows: %v", err)
	}
	gotJSON = append(gotJSON, '\n')
	path := filepath.FromSlash(toolEventsFixture)
	if os.Getenv("NARVI_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, gotJSON, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	wantJSON, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v (set NARVI_UPDATE_FIXTURES=1 to write it)", path, err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("the stored rows differ from %s, which the web tests read; set NARVI_UPDATE_FIXTURES=1 to rewrite it", path)
	}

	// A reconnect: the agent replays the best-effort events it still
	// buffers, every one of msg_1's among them, after the turn is over.
	agent.sentinel("sentinel-live")
	live := page.broadcastsUntil(t, "sentinel-live")
	rig.proxy.sever()
	waitUntil(t, scenario23Wait, func() bool { return rig.readyCount() == 2 })
	waitUntil(t, scenario23Wait, func() bool { return len(rig.relayedFrames("step_finish")) == 2 })
	agent.sentinel("sentinel-after-replay")
	afterReplay := page.broadcastsUntil(t, "sentinel-after-replay")
	if got := messageRows(ctx, t, rig.h, rig.sessionID); strings.Join(got, "|") != strings.Join(wantMessageRows, "|") {
		t.Fatalf("after the replay msg_1's rows = %q, want %q: the replay adds none", got, wantMessageRows)
	}

	// Broadcast once each, live, and not again on the replay.
	wantOnce := []string{"step_start prt_step", "tool_call call_read", "tool_result call_read", "tool_call call_task", "tool_result call_task", "step_finish prt_finish"}
	for _, identity := range wantOnce {
		if n := countOf(live, identity); n != 1 {
			t.Errorf("%q broadcast %d times while the turn ran, want once (got %v)", identity, n, live)
		}
		if n := countOf(afterReplay, identity); n != 0 {
			t.Errorf("%q broadcast %d times on the replay, want none", identity, n)
		}
	}

	// The history every reader pages holds the message's tool activity.
	history := page.fetchHistory(ctx, t, rig.sessionID.String())
	sort.Strings(history)
	want := append([]string(nil), wantOnce...)
	sort.Strings(want)
	if strings.Join(history, "|") != strings.Join(want, "|") {
		t.Fatalf("fetch_history's msg_1 events = %q, want %q", history, want)
	}
}

func countOf(items []string, item string) int {
	n := 0
	for _, it := range items {
		if it == item {
			n++
		}
	}
	return n
}

//go:build integration

package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// fetchHistoryPage sends one fetch_history from the start of the log and
// returns the decoded reply -- decoded through the generated type, which
// refuses a reply without its required sandbox key.
func fetchHistoryPage(ctx context.Context, t *testing.T, conn *websocket.Conn, sessionID pgtype.UUID) clientws.FetchHistoryResponse {
	t.Helper()
	msg := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":null,"limit":100}`, sessionID.String())
	if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("Write fetch_history: %v", err)
	}
	for {
		data := readFrame(ctx, t, conn)
		var resp clientws.FetchHistoryResponse
		if err := json.Unmarshal(data, &resp); err == nil {
			return resp
		}
		// A live broadcast that arrived before the reply: skip it.
	}
}

func readFrame(ctx context.Context, t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	rc, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(rc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return data
}

// replySandboxStatus is the status a fetch_history reply carries, "" for a
// null sandbox.
func replySandboxStatus(t *testing.T, resp clientws.FetchHistoryResponse) string {
	t.Helper()
	if resp.Sandbox == nil {
		return ""
	}
	status, ok := (*resp.Sandbox)["status"].(string)
	if !ok {
		t.Fatalf("reply sandbox carries no status string: %#v", *resp.Sandbox)
	}
	return status
}

// awaitStatusBroadcast reads live frames until the sandbox_status one
// reporting status arrives.
func awaitStatusBroadcast(ctx context.Context, t *testing.T, conn *websocket.Conn, status string) {
	t.Helper()
	for {
		data := readFrame(ctx, t, conn)
		var frame struct {
			Sandbox *struct {
				Status string `json:"status"`
			} `json:"sandbox"`
		}
		if json.Unmarshal(data, &frame) == nil && frame.Sandbox != nil && frame.Sandbox.Status == status {
			return
		}
	}
}

func sendToActor(ctx context.Context, t *testing.T, actor *sessionactor.Actor, ev sessionactor.SandboxEvent) {
	t.Helper()
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	ev.Reply = reply
	if err := actor.Send(ctx, ev); err != nil {
		t.Fatalf("actor.Send %s: %v", ev.Type, err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatalf("%s %s not persisted", ev.Type, ev.MessageID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s's outcome", ev.Type)
	}
}

// TestClientHandler_FetchHistoryCarriesTheSandbox: every fetch_history
// reply names the sandbox as the control plane holds it then -- null with
// none yet, its status once there is one, and a status changed since the
// subscribe reply.
func TestClientHandler_FetchHistoryCarriesTheSandbox(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientFetchHistoryMinInterval = 0

	tests := []struct {
		name string
		// before runs before the page subscribes, after once it has.
		before, after func(ctx context.Context, t *testing.T, rig clientTestRig, sessionID pgtype.UUID)
		want          string
	}{
		{name: "no sandbox yet", want: ""},
		{
			name: "a booting sandbox",
			before: func(ctx context.Context, t *testing.T, rig clientTestRig, sessionID pgtype.UUID) {
				setTestSandboxStatus(ctx, t, rig, sessionID, sqlcgen.SandboxStatusBooting)
			},
			want: "booting",
		},
		{
			name: "a status changed after the subscribe reply",
			before: func(ctx context.Context, t *testing.T, rig clientTestRig, sessionID pgtype.UUID) {
				setTestSandboxStatus(ctx, t, rig, sessionID, sqlcgen.SandboxStatusBooting)
			},
			after: func(ctx context.Context, t *testing.T, rig clientTestRig, sessionID pgtype.UUID) {
				if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
					t.Fatalf("mark ready: %v", err)
				}
			},
			want: "ready",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig, sessionRow := newClientTestRig(t, timeouts)
			ctx := context.Background()
			if tc.before != nil {
				tc.before(ctx, t, rig, sessionRow.ID)
			}
			token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))
			conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
			defer func() { _ = conn.CloseNow() }()
			if tc.after != nil {
				tc.after(ctx, t, rig, sessionRow.ID)
			}
			if got := replySandboxStatus(t, fetchHistoryPage(ctx, t, conn, sessionRow.ID)); got != tc.want {
				t.Errorf("reply sandbox status = %q, want %q", got, tc.want)
			}
		})
	}
}

func setTestSandboxStatus(ctx context.Context, t *testing.T, rig clientTestRig, sessionID pgtype.UUID, status sqlcgen.SandboxStatus) {
	t.Helper()
	if _, err := rig.sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: status}); err != nil {
		t.Fatalf("move sandbox to %s: %v", status, err)
	}
}

// TestClientHandler_OpenPageFollowsTheServersBoot is what an open session
// page sees during a boot (technical plan §3.2): the agent's `ready`
// leaves the sandbox booting, and the page learns it -- a sandbox_status
// broadcast, then a fetch_history reply naming booting, not ready; the
// null-phase heartbeat after boot evidence is what the page then learns
// as ready, the same way.
func TestClientHandler_OpenPageFollowsTheServersBoot(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientFetchHistoryMinInterval = 0
	rig, sessionRow := newClientTestRigWithBroadcast(t, timeouts)
	ctx := context.Background()
	setTestSandboxStatus(ctx, t, rig, sessionRow.ID, sqlcgen.SandboxStatusConnecting)

	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))
	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()
	actor, err := rig.registry.GetOrSpawn(ctx, sessionRow.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	event := func(typ, id, extra string) sessionactor.SandboxEvent {
		return sessionactor.SandboxEvent{Type: typ, Gen: 1, MessageID: id,
			Raw: json.RawMessage(fmt.Sprintf(`{"type":%q,"messageId":%q,"sessionId":%q,"gen":1%s}`, typ, id, sessionRow.ID.String(), extra))}
	}
	steps := []struct {
		name   string
		events []sessionactor.SandboxEvent
		want   string
	}{
		{name: "the agent's ready", events: []sessionactor.SandboxEvent{event("ready", "r", "")}, want: "booting"},
		{name: "a boot phase, then the null-phase heartbeat", events: []sessionactor.SandboxEvent{
			event("boot_progress", "bp", `,"phase":"clone"`),
			event("heartbeat", "h", `,"conversationId":null,"lastBootPhase":null`),
		}, want: "ready"},
	}
	for _, step := range steps {
		for _, ev := range step.events {
			sendToActor(ctx, t, actor, ev)
		}
		awaitStatusBroadcast(ctx, t, conn, step.want)
		if got := replySandboxStatus(t, fetchHistoryPage(ctx, t, conn, sessionRow.ID)); got != step.want {
			t.Errorf("%s: fetch_history reply sandbox status = %q, want %q", step.name, got, step.want)
		}
	}
}

// TestSandboxSocket_RefusesFramesASandboxMayNotSend: a sandbox that holds
// its token cannot put a status the server never recorded on an open page.
// Through the real sandbox handshake, none of these frames is stored,
// broadcast to an open page or acked, and the sandbox row keeps its gen and
// status:
//
//   - one typed as each event the control plane writes itself -- a forged
//     sandbox_status claiming gen 999 and ready, and the other two;
//   - one shaped like the fetch_history reply a page waits for, with a
//     forged sandbox row and a forged sandbox_status inside `events`, and
//     one with each of its keys alone;
//   - one with no type, and one with an empty type.
//
// An ordinary agent frame on the same socket is still stored and
// broadcast, and a critical one still acked.
func TestSandboxSocket_RefusesFramesASandboxMayNotSend(t *testing.T) {
	rig, sessionRow := newClientTestRigWithBroadcast(t, platform.DefaultTimeouts())
	ctx := context.Background()
	sessionID := sessionRow.ID
	sid := sessionID.String()
	setTestSandboxStatus(ctx, t, rig, sessionID, sqlcgen.SandboxStatusBooting)
	const secret = "the-sandbox-token"
	setSandboxTokenHash(ctx, t, rig.pool, sessionID, wshub.HashSandboxToken(secret))
	before := getSandbox(ctx, t, rig.pool, sessionID)

	token := createTestWSToken(ctx, t, rig.pool, sessionID, time.Now().Add(24*time.Hour))
	page := subscribeClient(ctx, t, rig.wsURL, sid, token)
	defer func() { _ = page.CloseNow() }()
	pageFrames, waitPage := startReader(page)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+secret)
	header.Set("X-Sandbox-ID", "sbx-test")
	header.Set("X-Sandbox-Gen", "1")
	sandboxConn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sid+"/ws?type=sandbox", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the sandbox socket: %v", err)
	}
	defer func() { _ = sandboxConn.CloseNow() }()
	sandboxFrames, waitSandbox := startReader(sandboxConn)
	send := func(frame string) {
		t.Helper()
		if err := sandboxConn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatalf("write %s: %v", frame, err)
		}
	}

	forged := []string{sessionactor.SandboxStatusEventType, "image_decision", narvipg.ShadowEgressSuppressedEventType}
	// sandbox_status carries the forged row where the server's event puts
	// it; the other two carry none of a reply's keys, so only their type
	// refuses them.
	send(fmt.Sprintf(`{"type":"sandbox_status","messageId":"forged-sandbox_status","sessionId":%q,"gen":1,"ackId":"sandbox_status:forged-sandbox_status","sandbox":{"gen":999,"status":"ready"}}`, sid))
	for _, typ := range forged[1:] {
		send(fmt.Sprintf(`{"type":%q,"messageId":"forged-%s","sessionId":%q,"gen":1,"ackId":"%s:forged-%s","reason":"selected"}`, typ, typ, sid, typ, typ))
	}
	// Shaped like a fetch_history reply under a type of its own; then each
	// of its keys alone, null values included; then no type at all.
	forgedRow := `{"id":"forged","gen":1,"status":"ready","lastSeenAt":null,"createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T00:00:00Z"}`
	send(fmt.Sprintf(`{"type":"x_probe","messageId":"forged-reply","sessionId":%q,"gen":1,"ackId":"x_probe:forged-reply","events":[{"id":9007199254740000,"type":"sandbox_status","payload":{"sandbox":{"gen":1,"status":"ready"}},"createdAt":"2026-10-01T00:00:00Z"}],"nextCursor":null,"sandbox":%s}`, sid, forgedRow))
	send(fmt.Sprintf(`{"type":"x_probe","messageId":"forged-events","sessionId":%q,"gen":1,"events":[]}`, sid))
	send(fmt.Sprintf(`{"type":"x_probe","messageId":"forged-cursor","sessionId":%q,"gen":1,"nextCursor":null}`, sid))
	send(fmt.Sprintf(`{"type":"x_probe","messageId":"forged-row","sessionId":%q,"gen":1,"sandbox":null}`, sid))
	send(fmt.Sprintf(`{"messageId":"forged-untyped","sessionId":%q,"gen":1}`, sid))
	send(fmt.Sprintf(`{"type":"","messageId":"forged-empty-type","sessionId":%q,"gen":1}`, sid))
	// The read loop hands the actor one frame at a time, so once this one
	// is stored and broadcast, every forged frame before it was handled.
	send(fmt.Sprintf(`{"type":"boot_progress","messageId":"bp-1","sessionId":%q,"gen":1,"phase":"clone"}`, sid))

	deadline := time.After(dispatchTestWait)
	for broadcast := false; !broadcast; {
		select {
		case data := <-pageFrames:
			var frame struct {
				MessageID string `json:"messageId"`
			}
			_ = json.Unmarshal(data, &frame)
			if strings.HasPrefix(frame.MessageID, "forged-") {
				t.Fatalf("an open page was sent a forged frame: %s", data)
			}
			broadcast = frame.MessageID == "bp-1"
		case <-deadline:
			t.Fatal("the ordinary agent frame never reached the open page")
		}
	}

	for _, typ := range forged {
		if got := countEvents(ctx, t, rig.pool, sessionID, typ); got != 0 {
			t.Errorf("%d %s events stored, want 0: a sandbox wrote an event the control plane owns", got, typ)
		}
	}
	var stored int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND message_id LIKE 'forged-%'`, sessionID).Scan(&stored); err != nil {
		t.Fatalf("count forged events: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d forged frames stored, want 0", stored)
	}
	if got := countEvents(ctx, t, rig.pool, sessionID, "boot_progress"); got != 1 {
		t.Errorf("boot_progress events = %d, want 1: the ordinary frame on the same socket must still be stored", got)
	}
	after := getSandbox(ctx, t, rig.pool, sessionID)
	if after.Gen != before.Gen || after.Status != before.Status || after.PreSuspectStatus != nil {
		t.Errorf("sandbox row moved: gen %d status %s pre_suspect %v, want gen %d status %s", after.Gen, after.Status, after.PreSuspectStatus, before.Gen, before.Status)
	}

	// No ack for any forged frame, though each carried an ackId; the
	// connection is still open and still acks a critical frame.
	expectNoMessage(t, sandboxFrames, 300*time.Millisecond)
	send(fmt.Sprintf(`{"type":"push_error","messageId":"pe-1","sessionId":%q,"gen":1,"ackId":"push_error:pe-1","error":"boom"}`, sid))
	expectAck(t, sandboxFrames, "push_error:pe-1")

	_ = sandboxConn.Close(websocket.StatusNormalClosure, "")
	_ = page.Close(websocket.StatusNormalClosure, "")
	if err := waitSandbox(); err != nil {
		t.Errorf("sandbox reader: %v", err)
	}
	if err := waitPage(); err != nil {
		t.Errorf("page reader: %v", err)
	}
}

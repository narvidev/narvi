//go:build integration

package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
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

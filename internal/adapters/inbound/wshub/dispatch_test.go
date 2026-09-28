//go:build integration

// End-to-end integration test proving internal/adapters/inbound/wshub's
// read/dispatch loop (dispatch.go) against a REAL client
// (github.com/coder/websocket.Dial, mirroring internal/sandboxagent/
// wsbridge's own test helpers) through a real handshake, a real Postgres
// instance, and a real internal/app/sessionactor.Actor -- gated behind the
// "integration" build tag. Run via `make test-integration`.
package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

const dispatchTestWait = 5 * time.Second

// wireEnvelope is this test file's own loose decode target for asserting
// on whatever subset of fields a given assertion cares about -- mirrors
// internal/sandboxagent/wsbridge/bridge_test.go's own identical
// testEnvelope precedent.
type wireEnvelope struct {
	Type  string `json:"type"`
	AckID string `json:"ackId"`
}

// startReader launches a background reader goroutine (via errgroup, never
// a naked `go` statement -- §11's nakedgoroutine rule has no test-file
// exemption) that pushes every inbound frame onto msgCh, reading with
// context.Background() deliberately: coder/websocket's own read.go arms a
// deadline-driven close via context.AfterFunc ONLY when ctx.Done() != nil,
// so passing a context with a deadline/timeout directly to conn.Read would
// close the ENTIRE connection the instant that one read timed out -- wrong
// for this test, which needs to assert "no message arrived" without ending
// the connection. The test's own select-with-timeout against msgCh is what
// expresses "wait up to N, but don't kill the connection if nothing comes."
func startReader(conn *websocket.Conn) (msgCh chan []byte, wait func() error) {
	msgCh = make(chan []byte, 32)
	var eg errgroup.Group
	eg.Go(func() error {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return nil
			}
			msgCh <- data
		}
	})
	return msgCh, eg.Wait
}

// expectNoMessage confirms nothing arrives on msgCh within `within`.
func expectNoMessage(t *testing.T, msgCh <-chan []byte, within time.Duration) {
	t.Helper()
	select {
	case data := <-msgCh:
		t.Fatalf("unexpected message: %s", data)
	case <-time.After(within):
	}
}

// expectAck confirms the next message on msgCh is an "ack" carrying
// wantAckID.
func expectAck(t *testing.T, msgCh <-chan []byte, wantAckID string) {
	t.Helper()
	select {
	case data := <-msgCh:
		var env wireEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v (%s)", err, data)
		}
		if env.Type != "ack" {
			t.Fatalf("message type = %q, want %q (%s)", env.Type, "ack", data)
		}
		if env.AckID != wantAckID {
			t.Errorf("ack AckId = %q, want %q", env.AckID, wantAckID)
		}
	case <-time.After(dispatchTestWait):
		t.Fatal("timed out waiting for ack")
	}
}

// TestDispatch_EndToEnd drives one real sandbox-WS connection through the
// full handshake and then a script of frames, asserting at every step
// against the real database and the real wire traffic coming back:
//
//   - a non-critical event ("token") persists with no ack;
//   - a critical event ("execution_complete") persists AND produces a
//     matching "ack" command over the same connection;
//   - "ready" while Connecting transitions the sandbox row to Booting;
//   - "heartbeat" with a nil lastBootPhase while Booting, once the gen
//     has shown boot evidence (a boot_progress here, §3.2), transitions to
//     Ready;
//   - "ready" while already Ready is a silent no-op (persisted, liveness
//     bumped, status unchanged, no error);
//   - a stale (too-low) gen event is rejected outright: not persisted,
//     last_seen_at untouched, no ack, and -- critically -- the connection
//     stays open, proven by one more fully-round-tripped critical event
//     sent right after it.
func TestDispatch_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createTestSandbox(ctx, t, pool, sessionID) // gen 1, Pending
	moveSandboxStatus(ctx, t, pool, sessionID, sqlcgen.SandboxStatusConnecting)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	sandboxStore := narvipg.NewSandboxStore(pool)

	server, wsURL := newTestServer(registry, sandboxStore, platform.DefaultTimeouts())
	t.Cleanup(server.Close)

	header := http.Header{}
	for k, v := range baseHeaders() {
		header.Set(k, v)
	}

	conn, _, err := websocket.Dial(ctx, wsURL+"/sessions/"+sessionID.String()+"/ws?type=sandbox",
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	msgCh, waitReader := startReader(conn)

	send := func(t *testing.T, payload string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	sid := sessionID.String()

	// A `token` is stored only while a turn is Processing
	// (sessionactor/tokenframe.go); (b)'s execution_complete ends it.
	if _, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing}); err != nil {
		t.Fatalf("create processing turn: %v", err)
	}

	// --- (a) non-critical event: persists, no ack. ---
	send(t, fmt.Sprintf(`{"type":"token","messageId":"tok-1","sessionId":%q,"gen":1,"text":"hello"}`, sid))
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, pool, sessionID, "token") == 1
	})
	expectNoMessage(t, msgCh, 300*time.Millisecond)

	// --- (b) critical event: persists AND acks with the matching ackId. ---
	send(t, fmt.Sprintf(`{"type":"execution_complete","messageId":"m1","sessionId":%q,"gen":1,"ackId":"execution_complete:m1","outcome":"completed"}`, sid))
	expectAck(t, msgCh, "execution_complete:m1")
	if got := countEvents(ctx, t, pool, sessionID, "execution_complete"); got != 1 {
		t.Errorf("execution_complete event count = %d, want 1", got)
	}

	// --- (c) "ready" while Connecting -> Booting. ---
	send(t, fmt.Sprintf(`{"type":"ready","messageId":"r1","sessionId":%q,"gen":1}`, sid))
	waitUntil(t, dispatchTestWait, func() bool {
		return getSandbox(ctx, t, pool, sessionID).Status == sqlcgen.SandboxStatusBooting
	})

	// --- (d) "heartbeat" with lastBootPhase:null while Booting -> Ready,
	// after this gen's boot evidence (a service's boot_progress, §3.2). ---
	send(t, fmt.Sprintf(`{"type":"boot_progress","messageId":"bp1","sessionId":%q,"gen":1,"phase":"web:ready"}`, sid))
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, pool, sessionID, "boot_progress") == 1
	})
	send(t, fmt.Sprintf(`{"type":"heartbeat","messageId":"h1","sessionId":%q,"gen":1,"conversationId":null,"lastBootPhase":null}`, sid))
	waitUntil(t, dispatchTestWait, func() bool {
		return getSandbox(ctx, t, pool, sessionID).Status == sqlcgen.SandboxStatusReady
	})

	// --- (e) "ready" while ALREADY Ready: silent no-op (persisted,
	// liveness bumped, status unchanged, no error/crash -- connection
	// stays open). ---
	before := getSandbox(ctx, t, pool, sessionID)
	time.Sleep(5 * time.Millisecond) // ensure a distinguishable later timestamp
	send(t, fmt.Sprintf(`{"type":"ready","messageId":"r2","sessionId":%q,"gen":1}`, sid))
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, pool, sessionID, "ready") == 2
	})
	after := getSandbox(ctx, t, pool, sessionID)
	if after.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("status after redundant ready = %s, want unchanged %s", after.Status, sqlcgen.SandboxStatusReady)
	}
	if !after.LastSeenAt.Time.After(before.LastSeenAt.Time) {
		t.Errorf("last_seen_at did not advance on redundant ready: before=%v after=%v", before.LastSeenAt.Time, after.LastSeenAt.Time)
	}
	expectNoMessage(t, msgCh, 300*time.Millisecond) // "ready" is never critical -- no ack

	// --- (f) stale (too-low) gen: NOT persisted, last_seen_at untouched, no
	// ack. Give the actor time to (not) process it, then confirm the
	// connection is STILL usable via one more fully-round-tripped critical
	// event. ---
	beforeStale := getSandbox(ctx, t, pool, sessionID)
	send(t, fmt.Sprintf(`{"type":"heartbeat","messageId":"h-stale","sessionId":%q,"gen":0}`, sid))
	time.Sleep(300 * time.Millisecond)
	expectNoMessage(t, msgCh, 10*time.Millisecond)

	afterStale := getSandbox(ctx, t, pool, sessionID)
	if !afterStale.LastSeenAt.Time.Equal(beforeStale.LastSeenAt.Time) {
		t.Errorf("last_seen_at moved on a stale-gen event: before=%v after=%v", beforeStale.LastSeenAt.Time, afterStale.LastSeenAt.Time)
	}
	if got := countEvents(ctx, t, pool, sessionID, "heartbeat"); got != 1 {
		t.Errorf("heartbeat event count = %d, want 1 (the stale-gen one must not have been persisted)", got)
	}

	send(t, fmt.Sprintf(`{"type":"push_error","messageId":"m2","sessionId":%q,"gen":1,"ackId":"push_error:m2","error":"boom"}`, sid))
	expectAck(t, msgCh, "push_error:m2")

	_ = conn.Close(websocket.StatusNormalClosure, "")
	if err := waitReader(); err != nil {
		t.Errorf("reader goroutine error = %v", err)
	}
}

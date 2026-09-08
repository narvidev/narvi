package wshub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"
)

// TestHub_BroadcastNonBlockingOnFullChannel proves Hub.Broadcast's own
// core correctness property directly (white-box, package wshub, no
// Docker/network needed -- this is the single most important property of
// the Hub design, per this Step's own test-plan wording): a connection
// whose own channel is already at capacity does NOT block Broadcast's
// caller -- the payload is silently dropped for that one connection
// (logged, not fatal), while every OTHER connection registered for the
// same session still receives it. The two "connections" here are bare
// *websocket.Conn zero values used purely as distinct map keys --
// Broadcast itself never calls a method on the conn key (only on the
// channel value), so no real network connection is needed to exercise
// this property in isolation. See client_test.go's own
// TestClientHandler_SlowConnectionDoesNotBlockOthers for the same
// property demonstrated end-to-end over real WS connections.
func TestHub_BroadcastNonBlockingOnFullChannel(t *testing.T) {
	t.Parallel()

	h := NewHub()
	const sessionID = "session-under-test"

	fullConn := &websocket.Conn{}
	fullCh := make(chan []byte, hubConnBufferSize)
	for i := 0; i < hubConnBufferSize; i++ {
		fullCh <- []byte("filler")
	}

	fastConn := &websocket.Conn{}
	fastCh := make(chan []byte, hubConnBufferSize)

	h.mu.Lock()
	h.conns[sessionID] = map[*websocket.Conn]hubConn{
		fullConn: {ch: fullCh},
		fastConn: {ch: fastCh},
	}
	h.mu.Unlock()

	payload := json.RawMessage(`{"type":"tick"}`)

	completed := make(chan struct{})
	var eg errgroup.Group
	eg.Go(func() error {
		h.Broadcast(sessionID, payload)
		close(completed)
		return nil
	})

	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("Hub.Broadcast blocked on a full connection channel -- want a non-blocking drop instead")
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("broadcast goroutine: %v", err)
	}

	if len(fullCh) != hubConnBufferSize {
		t.Errorf("fullCh len = %d, want unchanged %d (the new payload must have been dropped, not enqueued)", len(fullCh), hubConnBufferSize)
	}

	select {
	case got := <-fastCh:
		if string(got) != string(payload) {
			t.Errorf("fastCh received %s, want %s", got, payload)
		}
	default:
		t.Error("fastCh received nothing -- the OTHER connection must still get the broadcast even though fullCh was saturated")
	}
}

// TestHub_RegisterUnregister proves Register adds a connection's channel
// to the session-keyed map and unregister removes it, cleaning up the
// session entry entirely once empty -- proven via Broadcast's own
// observable behavior (a registered channel receives; an unregistered one
// no longer does) rather than reaching into the map directly a second
// time, so this test exercises the same public surface Broadcast itself
// depends on.
func TestHub_RegisterUnregister(t *testing.T) {
	t.Parallel()

	h := NewHub()
	const sessionID = "session-under-test"

	conn := &websocket.Conn{}
	ch := make(chan []byte, hubConnBufferSize)

	h.mu.Lock()
	h.conns[sessionID] = map[*websocket.Conn]hubConn{conn: {ch: ch}}
	h.mu.Unlock()

	h.Broadcast(sessionID, json.RawMessage(`"before-unregister"`))
	select {
	case got := <-ch:
		if string(got) != `"before-unregister"` {
			t.Errorf("got %s, want %q", got, "before-unregister")
		}
	default:
		t.Fatal("registered channel received nothing")
	}

	h.mu.Lock()
	delete(h.conns[sessionID], conn)
	if len(h.conns[sessionID]) == 0 {
		delete(h.conns, sessionID)
	}
	h.mu.Unlock()

	h.Broadcast(sessionID, json.RawMessage(`"after-unregister"`))
	select {
	case got := <-ch:
		t.Errorf("unregistered channel still received %s, want nothing", got)
	default:
	}

	h.mu.Lock()
	_, sessionStillTracked := h.conns[sessionID]
	h.mu.Unlock()
	if sessionStillTracked {
		t.Error("session entry still present in Hub.conns after its only connection unregistered -- want it cleaned up")
	}
}

//go:build integration

// A client that stops reading during the subscribed reply (technical plan
// §6.2, §13.3): the access re-check runs while that write is stalled, and
// the write itself is bounded by ClientSubscribedWriteTimeout.
package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// stallBufferBytes is the socket buffer both ends of a stalling connection
// ask for: the server's send buffer and the client's receive buffer, so a
// subscribed reply of a couple of MiB cannot fit in flight and the
// server's write blocks until the client reads.
const stallBufferBytes = 4096

// newStallTestServer is newDispatcherTestServer whose accepted connections
// have a small send buffer. The returned channel is closed when the client
// handler returns, for a server that serves one client connection.
func newStallTestServer(t *testing.T, rig clientTestRig, timeouts platform.Timeouts) (string, <-chan struct{}) {
	t.Helper()
	handlerReturned := make(chan struct{})
	clientHandler := wshub.NewClientHandler(rig.registry, rig.sessions, rig.turns, rig.sandboxes, rig.events, rig.artifacts, rig.wsTokens, rig.users, rig.hub, timeouts)
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewHandler(
		wshub.NewSandboxHandler(rig.registry, rig.sandboxes, wshub.NewSandboxRegistry(timeouts), timeouts),
		func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerReturned)
			clientHandler(w, r)
		},
	))
	server := httptest.NewUnstartedServer(router)
	server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if tc, ok := c.(*net.TCPConn); ok {
			if err := tc.SetWriteBuffer(stallBufferBytes); err != nil {
				t.Errorf("set the server's send buffer: %v", err)
			}
		}
		return ctx
	}
	server.Start()
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), handlerReturned
}

// dialStalling dials ?type=client with a small receive buffer (set before
// connecting, so the advertised window is small too), sends the subscribe
// frame and returns without reading anything.
func dialStalling(ctx context.Context, t *testing.T, wsURL, sessionID, token string) *websocket.Conn {
	t.Helper()
	dialer := &net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var setErr error
		if err := raw.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, stallBufferBytes)
		}); err != nil {
			return err
		}
		return setErr
	}}
	conn, _, err := websocket.Dial(ctx, wsURL+"/sessions/"+sessionID+"/ws?type=client", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.SetReadLimit(64 << 20)
	raw, err := json.Marshal(clientws.SubscribeRequest{Token: token, ClientId: "stalled-reader"})
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write subscribe: %v", err)
	}
	return conn
}

// seedLargeSubscribedReply gives session artifacts whose metadata adds up
// to about 2 MiB, all of it in the subscribed reply, far more than the
// stalled connection holds in flight. The re-check never reads artifacts.
func seedLargeSubscribedReply(ctx context.Context, t *testing.T, rig clientTestRig, session pgtype.UUID) {
	t.Helper()
	blob := strings.Repeat("x", 256<<10)
	for i := range 8 {
		metadata, err := json.Marshal(map[string]string{"blob": blob})
		if err != nil {
			t.Fatalf("marshal artifact metadata: %v", err)
		}
		if _, err := rig.artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
			SessionID: session,
			Type:      sqlcgen.ArtifactTypePr,
			Url:       fmt.Sprintf("https://example.com/pr/%d", i),
			Metadata:  metadata,
		}); err != nil {
			t.Fatalf("create artifact %d: %v", i, err)
		}
	}
}

// TestClientHandler_RecheckEndsAStalledSubscribedWrite pins that the
// access re-check runs while the subscribed reply's write is stalled by a
// client that stops reading, and that its close ends that write. The user
// is disabled during the stall at T; within twice the re-check interval
// the connection has left the Hub; a broadcast and a stored event made
// after T + 2*interval never reach the client.
//
//   - A client that reads again within the close's 5s wait gets the
//     subscribed reply, whose write had begun before T, then the 4001
//     close: no broadcast, no fetch_history reply.
//   - A client that never reads again is dropped: coder/websocket gives
//     up writing the close frame behind the stalled frame after 5s and
//     closes the connection, which unblocks the stalled write, and the
//     handler returns -- within 2*interval plus that 5s, long before
//     ClientSubscribedWriteTimeout (set to a minute here) could end it.
func TestClientHandler_RecheckEndsAStalledSubscribedWrite(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientWSAccessRecheckInterval = 100 * time.Millisecond
	timeouts.ClientWSPingInterval = time.Hour
	timeouts.ClientSubscribedWriteTimeout = time.Minute
	// closeWriteLimit is coder/websocket's own bound on writing a close
	// frame (writeClose), after which it closes the connection.
	const closeWriteLimit = 5 * time.Second

	tests := []struct {
		name   string
		resume bool // the client reads again soon after T
	}{
		{name: "a client that reads again gets the reply and then 4001", resume: true},
		{name: "a client that never reads again is dropped and the write ends", resume: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig, sessionRow := newClientTestRig(t, timeouts)
			ctx := context.Background()
			sid := sessionRow.ID.String()

			seedLargeSubscribedReply(ctx, t, rig, sessionRow.ID)
			user := createTestUser(ctx, t, rig.pool, uniqueTestEmail("stalled"), "Stalled Reader")
			token := createTestWSTokenForUser(ctx, t, rig.pool, sessionRow.ID, user, time.Now().Add(time.Hour))
			wsURL, handlerReturned := newStallTestServer(t, rig, timeouts)

			conn := dialStalling(ctx, t, wsURL, sid, token)
			defer func() { _ = conn.CloseNow() }()

			// Registered (the subscribe check passed), then give the handler
			// time to assemble the reply and block in its write.
			waitUntil(t, 5*time.Second, func() bool { return slices.Contains(rig.hub.Participants(sid), user.String()) })
			time.Sleep(500 * time.Millisecond)
			select {
			case <-handlerReturned:
				t.Fatal("the handler returned before the disable: the subscribed write did not stall")
			default:
			}

			// T: the user is disabled while the subscribed write is stalled.
			disabledAt := time.Now()
			if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user); err != nil {
				t.Fatalf("disable user: %v", err)
			}
			waitUntil(t, 5*time.Second, func() bool { return !slices.Contains(rig.hub.Participants(sid), user.String()) })
			t.Logf("the stalled connection left the Hub %v after the disable (re-check interval %v)", time.Since(disabledAt), timeouts.ClientWSAccessRecheckInterval)

			// After T + 2*interval: a broadcast and a new stored event.
			time.Sleep(time.Until(disabledAt.Add(2 * timeouts.ClientWSAccessRecheckInterval)))
			const tick = `{"type":"tick","n":"after-the-disable"}`
			rig.hub.Broadcast(sid, json.RawMessage(tick))
			const marker = "written-after-the-disable"
			if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
				SessionID: sessionRow.ID, Type: "token", MessageID: marker, Payload: []byte(`{"text":"` + marker + `"}`),
			}); err != nil {
				t.Fatalf("store an event after the disable: %v", err)
			}

			if !tc.resume {
				bound := 2*timeouts.ClientWSAccessRecheckInterval + closeWriteLimit
				select {
				case <-handlerReturned:
					t.Logf("the handler returned %v after the disable (bound %v)", time.Since(disabledAt), bound)
				case <-time.After(time.Until(disabledAt.Add(bound + 2*time.Second))):
					t.Fatalf("the handler was still in its stalled write %v after the disable, want it ended by the close within %v", time.Since(disabledAt), bound)
				}
				readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				_, data, err := conn.Read(readCtx)
				if err == nil {
					t.Fatalf("Read = %d bytes, want the connection dropped before the whole reply", len(data))
				}
				return
			}

			// The client reads again, well within the close's 5s wait behind
			// the stalled frame.
			readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, data, err := conn.Read(readCtx)
			if err != nil {
				t.Fatalf("Read the subscribed reply: %v (close code %d)", err, websocket.CloseStatus(err))
			}
			var payload clientws.SubscribedPayload
			if err := json.Unmarshal(data, &payload); err != nil || payload.SessionId != sid {
				t.Fatalf("first frame is not this session's subscribed reply (%d bytes, %v)", len(data), err)
			}
			if strings.Contains(string(data), marker) {
				t.Fatal("the subscribed reply holds the event stored after the disable")
			}

			fetch := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":null,"limit":100}`, sid)
			_ = conn.Write(ctx, websocket.MessageText, []byte(fetch)) // may fail once the close is processed

			_, data, err = conn.Read(readCtx)
			if err == nil {
				t.Fatalf("Read after the subscribed reply = %s, want the 4001 close and nothing else", data)
			}
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(4001) {
				t.Fatalf("close code = %d (%v), want 4001", got, err)
			}
		})
	}
}

// TestClientHandler_StalledSubscribedWriteIsDropped pins the subscribed
// write's own bound: a client that stops reading during the subscribed
// reply is dropped after ClientSubscribedWriteTimeout even though its
// access stands (the re-check is set out of the way), and never gets the
// whole reply.
func TestClientHandler_StalledSubscribedWriteIsDropped(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientWSAccessRecheckInterval = time.Hour
	timeouts.ClientWSPingInterval = time.Hour
	timeouts.ClientSubscribedWriteTimeout = 500 * time.Millisecond
	rig, sessionRow := newClientTestRig(t, timeouts)
	ctx := context.Background()
	sid := sessionRow.ID.String()

	seedLargeSubscribedReply(ctx, t, rig, sessionRow.ID)
	user := createTestUser(ctx, t, rig.pool, uniqueTestEmail("dropped"), "Dropped Reader")
	token := createTestWSTokenForUser(ctx, t, rig.pool, sessionRow.ID, user, time.Now().Add(time.Hour))
	wsURL, handlerReturned := newStallTestServer(t, rig, timeouts)

	conn := dialStalling(ctx, t, wsURL, sid, token)
	defer func() { _ = conn.CloseNow() }()

	waitUntil(t, 5*time.Second, func() bool { return slices.Contains(rig.hub.Participants(sid), user.String()) })
	registeredAt := time.Now()
	// The handler gives up on the write and returns; its deferred cleanup
	// unregisters the connection.
	select {
	case <-handlerReturned:
	case <-time.After(10 * time.Second):
		t.Fatalf("the handler was still in its stalled write after 10s, want it ended at the %v write bound", timeouts.ClientSubscribedWriteTimeout)
	}
	t.Logf("the stalled connection was dropped %v after it registered (write bound %v)", time.Since(registeredAt), timeouts.ClientSubscribedWriteTimeout)
	if got := rig.hub.Participants(sid); slices.Contains(got, user.String()) {
		t.Errorf("Hub.Participants after the handler returned = %v, want the user gone", got)
	}

	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err == nil {
		t.Fatalf("Read = %d bytes, want the connection dropped before the whole reply", len(data))
	}
	if got := websocket.CloseStatus(err); got != -1 {
		t.Errorf("close code = %d, want none: the connection is dropped, not closed with a code", got)
	}
}

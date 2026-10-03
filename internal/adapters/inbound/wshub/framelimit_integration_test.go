//go:build integration

package wshub_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// warnRecorder is a slog.Handler that keeps every record at WARN or above,
// so a test can find the line a handler logged.
type warnRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *warnRecorder) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnRecorder) WithGroup(string) slog.Handler      { return h }

// find returns the attrs of the first record whose message is msg.
func (h *warnRecorder) find(msg string) (map[string]slog.Value, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		attrs := map[string]slog.Value{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value
			return true
		})
		return attrs, true
	}
	return nil, false
}

// warningFrameOfSize is a sandbox `warning` event of exactly size bytes.
func warningFrameOfSize(t *testing.T, sessionID, messageID string, size int) []byte {
	t.Helper()
	frame := fmt.Sprintf(`{"type":"warning","messageId":%q,"sessionId":%q,"gen":1,"message":"%%s"}`, messageID, sessionID)
	pad := size - len(frame) + 2
	if pad < 0 {
		t.Fatalf("a %d-byte frame cannot hold the envelope", size)
	}
	return []byte(fmt.Sprintf(frame, strings.Repeat("w", pad)))
}

// TestSandboxSocket_FrameOverTheLimit_ClosedAndLogged: the control plane
// states the largest agent event it reads in the handshake
// (platform.MaxFrameBytesHeader) and reads exactly that much: a frame of
// platform.MaxEventFrameBytes is read and stored on the connection it came
// on, and one byte more closes the connection with StatusMessageTooBig and
// is logged at WARN with the limit -- the only trace on this side of an
// agent built before the header, which sends the frame again on every
// reconnect.
func TestSandboxSocket_FrameOverTheLimit_ClosedAndLogged(t *testing.T) {
	recorder := &warnRecorder{}
	previous := slog.Default()
	slog.SetDefault(slog.New(recorder))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := context.Background()
	pool := newTestPool(t)
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	server, wsURL := newTestServer(registry, narvipg.NewSandboxStore(pool), platform.DefaultTimeouts())
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name       string
		size       int
		wantClosed bool
	}{
		{name: "exactly the limit is read", size: platform.MaxEventFrameBytes},
		{name: "one byte over closes the connection", size: platform.MaxEventFrameBytes + 1, wantClosed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			createTestSandbox(ctx, t, pool, sessionID)
			header := http.Header{}
			for k, v := range baseHeaders() {
				header.Set(k, v)
			}
			conn, resp, err := websocket.Dial(ctx, wsURL+"/sessions/"+sessionID.String()+"/ws?type=sandbox", &websocket.DialOptions{HTTPHeader: header})
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			if got := resp.Header.Get(platform.MaxFrameBytesHeader); got != strconv.Itoa(platform.MaxEventFrameBytes) {
				t.Fatalf("handshake %s = %q, want %d", platform.MaxFrameBytesHeader, got, platform.MaxEventFrameBytes)
			}

			messageID := "w-" + strconv.Itoa(tc.size)
			if err := conn.Write(ctx, websocket.MessageText, warningFrameOfSize(t, sessionID.String(), messageID, tc.size)); err != nil {
				t.Fatalf("Write: %v", err)
			}

			if !tc.wantClosed {
				waitUntil(t, 10*time.Second, func() bool { return countEvents(ctx, t, pool, sessionID, "warning") == 1 })
				// Still the connection the frame came on.
				if err := conn.Write(ctx, websocket.MessageText, warningFrameOfSize(t, sessionID.String(), "w-after", 200)); err != nil {
					t.Fatalf("Write after the frame: %v", err)
				}
				waitUntil(t, 10*time.Second, func() bool { return countEvents(ctx, t, pool, sessionID, "warning") == 2 })
				if _, found := recorder.find("wshub: sandbox frame over the read limit; connection closed"); found {
					t.Fatal("a frame within the limit was logged as over it")
				}
				return
			}

			readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, _, err = conn.Read(readCtx)
			if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
				t.Fatalf("read after the frame = %v (status %d), want the connection closed with StatusMessageTooBig", err, status)
			}
			var attrs map[string]slog.Value
			waitUntil(t, 10*time.Second, func() bool {
				var found bool
				attrs, found = recorder.find("wshub: sandbox frame over the read limit; connection closed")
				return found
			})
			if limit := attrs["limit_bytes"]; limit.Int64() != platform.MaxEventFrameBytes {
				t.Fatalf("WARN limit_bytes = %v, want %d", limit, platform.MaxEventFrameBytes)
			}
			if n := countEvents(ctx, t, pool, sessionID, "warning"); n != 0 {
				t.Fatalf("%d warning rows stored, want none: the frame was never read", n)
			}
		})
	}
}

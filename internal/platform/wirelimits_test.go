package platform_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/internal/platform"
)

// TestDefaultFrameReadLimitBytes_IsTheLibraryDefault pins
// DefaultFrameReadLimitBytes to the read limit a fresh connection of the
// WebSocket library has when nothing calls SetReadLimit on it: a message of
// exactly that many bytes reads, and one byte more fails with
// ErrMessageTooBig. A library upgrade that moves its default fails here, so
// the bound the control plane holds an agent that states no read limit to
// (technical plan §3.3, §6.1) can never silently drift from what that agent
// reads.
func TestDefaultFrameReadLimitBytes_IsTheLibraryDefault(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		size    int
		wantErr error
	}{
		{name: "exactly the default reads", size: platform.DefaultFrameReadLimitBytes},
		{name: "one byte past it fails", size: platform.DefaultFrameReadLimitBytes + 1, wantErr: websocket.ErrMessageTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			type readResult struct {
				n   int
				err error
			}
			results := make(chan readResult, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					results <- readResult{err: err}
					return
				}
				defer func() { _ = conn.CloseNow() }()
				// A fresh connection: nothing calls SetReadLimit.
				_, data, err := conn.Read(r.Context())
				results <- readResult{n: len(data), err: err}
			}))
			defer server.Close()

			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			if err := conn.Write(ctx, websocket.MessageText, bytes.Repeat([]byte("a"), tc.size)); err != nil {
				t.Fatalf("Write(%d bytes): %v", tc.size, err)
			}

			var got readResult
			select {
			case got = <-results:
			case <-ctx.Done():
				t.Fatal("the server never finished its read")
			}
			if tc.wantErr == nil {
				if got.err != nil || got.n != tc.size {
					t.Fatalf("read of a %d-byte message on a fresh connection = %d bytes, %v; want it read whole", tc.size, got.n, got.err)
				}
				return
			}
			if !errors.Is(got.err, tc.wantErr) {
				t.Fatalf("read of a %d-byte message on a fresh connection = %d bytes, %v; want %v", tc.size, got.n, got.err, tc.wantErr)
			}
		})
	}
}

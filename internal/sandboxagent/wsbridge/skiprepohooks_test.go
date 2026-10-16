package wsbridge_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// readySkipRepoHooks decodes capabilities.skipRepoHooks from a ready
// frame: nil when the key is absent.
func readySkipRepoHooks(t *testing.T, ready []byte) *bool {
	t.Helper()
	var r struct {
		Type         string `json:"type"`
		Capabilities *struct {
			SkipRepoHooks *bool `json:"skipRepoHooks"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(ready, &r); err != nil || r.Type != "ready" {
		t.Fatalf("first frame %s is not a ready: %v", ready, err)
	}
	if r.Capabilities == nil {
		return nil
	}
	return r.Capabilities.SkipRepoHooks
}

// TestReady_AdvertisesSkipRepoHooksOnlyWhenTold: a Bridge told its agent
// honours SESSION_CONFIG's skipRepoHooks advertises it on its ready
// (technical plan §6.4, §27.1); one never told does not, as an agent that
// predates the key does not.
func TestReady_AdvertisesSkipRepoHooksOnlyWhenTold(t *testing.T) {
	t.Parallel()
	for _, advertise := range []bool{true, false} {
		t.Run(map[bool]string{true: "told", false: "never told"}[advertise], func(t *testing.T) {
			t.Parallel()
			ready := make(chan []byte, 1)
			script := func(conn *websocket.Conn) {
				frame, err := serverRead(conn, testWait)
				if err != nil {
					t.Errorf("read ready: %v", err)
					return
				}
				ready <- frame
				absorbForever(conn)
			}
			server := httptest.NewServer(&stepServer{steps: []func(*websocket.Conn){script}})
			t.Cleanup(server.Close)

			bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", &spyHandler{},
				testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
			if advertise {
				bridge.AdvertiseSkipRepoHooks()
			}
			ctx, cancel := context.WithCancel(context.Background())
			wait := runInBackground(ctx, bridge)

			got := readySkipRepoHooks(t, waitChan(t, ready, testWait))
			switch {
			case advertise && (got == nil || !*got):
				t.Errorf("ready capabilities.skipRepoHooks = %v, want true", got)
			case !advertise && got != nil:
				t.Errorf("ready capabilities.skipRepoHooks = %v, want it absent", *got)
			}
			cancel()
			if err := wait(); err != nil {
				t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
			}
		})
	}
}

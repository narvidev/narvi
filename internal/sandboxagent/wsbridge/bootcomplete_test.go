package wsbridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// A heartbeat's null lastBootPhase is the wire's "boot has completed"
// (events.schema.json, Heartbeat.lastBootPhase), and the control plane
// moves a Booting sandbox to Ready on it (technical plan §3.2). These
// tests pin what the agent itself puts on the wire, which is also all a
// control plane built before §3.2's boot-evidence rule reads.

// recordFrames returns a stepServer step that forwards every frame it
// reads to out, and returns (closing the connection, so the Bridge
// reconnects) once it has forwarded closeAfterHeartbeats heartbeats; 0
// means never.
func recordFrames(out chan<- []byte, closeAfterHeartbeats int) func(*websocket.Conn) {
	return func(conn *websocket.Conn) {
		heartbeats := 0
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			out <- data
			var env testEnvelope
			if json.Unmarshal(data, &env) == nil && env.Type == "heartbeat" {
				heartbeats++
				if closeAfterHeartbeats > 0 && heartbeats == closeAfterHeartbeats {
					return
				}
			}
		}
	}
}

// nextOfType reads frames off ch until one of type typ arrives, failing
// the test if none does within testWait.
func nextOfType(t *testing.T, ch <-chan []byte, typ string) testEnvelope {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		data := waitChan(t, ch, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed frame %s: %v", data, err)
		}
		if env.Type == typ {
			return env
		}
	}
	t.Fatalf("no %q frame within %s", typ, testWait)
	return testEnvelope{}
}

func phaseString(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// TestHeartbeat_NeverNullBeforeMarkBootComplete is the agent side of the
// Booting -> Ready regression: through a first connection, a reconnect in
// the middle of boot and a service phase, no heartbeat reports a null
// lastBootPhase until MarkBootComplete -- before any phase is reported
// (the clone and hooks window, which reports none) every heartbeat
// carries InitialBootPhase -- and the first heartbeat after it does.
func TestHeartbeat_NeverNullBeforeMarkBootComplete(t *testing.T) {
	t.Parallel()

	const heartbeatsPerConnection = 3
	first := make(chan []byte, 64)
	second := make(chan []byte, 64)
	fake := &stepServer{steps: []func(*websocket.Conn){
		recordFrames(first, heartbeatsPerConnection),
		recordFrames(second, 0),
	}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testShortHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	for _, conn := range []struct {
		name   string
		frames chan []byte
	}{
		{"first connection", first},
		{"after a reconnect mid-boot", second},
	} {
		nextOfType(t, conn.frames, "ready")
		for i := 0; i < heartbeatsPerConnection; i++ {
			env := nextOfType(t, conn.frames, "heartbeat")
			if env.LastBootPhase == nil || *env.LastBootPhase != wsbridge.InitialBootPhase {
				t.Fatalf("%s: heartbeat %d lastBootPhase = %s before any boot phase or MarkBootComplete, want %q",
					conn.name, i+1, phaseString(env.LastBootPhase), wsbridge.InitialBootPhase)
			}
		}
	}

	if err := bridge.SendBootProgress(ctx, services.BootProgressEvent{ServiceName: "web", Phase: services.PhaseReady}); err != nil {
		t.Fatalf("SendBootProgress() error = %v, want nil", err)
	}
	for {
		env := nextOfType(t, second, "heartbeat")
		if env.LastBootPhase == nil {
			t.Fatal("heartbeat lastBootPhase = null after a service phase and before MarkBootComplete")
		}
		if *env.LastBootPhase == "web:ready" {
			break
		}
	}

	bridge.MarkBootComplete()
	for {
		env := nextOfType(t, second, "heartbeat")
		if env.LastBootPhase == nil {
			break
		}
		// A heartbeat already built before MarkBootComplete ran may still
		// be in flight; the null one must follow it.
		if *env.LastBootPhase != "web:ready" {
			t.Fatalf("heartbeat lastBootPhase = %q after MarkBootComplete, want null (or the phase in flight)", *env.LastBootPhase)
		}
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// TestBootHeartbeats_SentAtOnce: with the regular heartbeat 10s away, the
// first heartbeat within testWait (2s) of ReportBootStarted or
// MarkBootComplete can only come from that call. ReportBootStarted's
// carries the boot phase -- the boot evidence §3.2 needs before a null
// phase counts, so a boot that completes within the first interval does
// not depend on boot_timing, best-effort telemetry, for it -- and
// MarkBootComplete's carries null, so Ready follows boot completion
// rather than up to one interval later. Called before the connection is
// up, either is delivered right after that connection's ready.
func TestBootHeartbeats_SentAtOnce(t *testing.T) {
	t.Parallel()

	initial := wsbridge.InitialBootPhase
	for _, tc := range []struct {
		name         string
		call         func(*wsbridge.Bridge)
		wantPhase    *string
		beforeDialed bool
	}{
		{name: "ReportBootStarted, connected", call: (*wsbridge.Bridge).ReportBootStarted, wantPhase: &initial},
		{name: "ReportBootStarted, before the connection is up", call: (*wsbridge.Bridge).ReportBootStarted, wantPhase: &initial, beforeDialed: true},
		{name: "MarkBootComplete, connected", call: (*wsbridge.Bridge).MarkBootComplete, wantPhase: nil},
		{name: "MarkBootComplete, before the connection is up", call: (*wsbridge.Bridge).MarkBootComplete, wantPhase: nil, beforeDialed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			frames := make(chan []byte, 32)
			fake := &stepServer{steps: []func(*websocket.Conn){recordFrames(frames, 0)}}
			server := httptest.NewServer(fake)
			t.Cleanup(server.Close)

			bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
				testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
			if tc.beforeDialed {
				tc.call(bridge)
			}

			ctx, cancel := context.WithCancel(context.Background())
			wait := runInBackground(ctx, bridge)

			nextOfType(t, frames, "ready")
			if !tc.beforeDialed {
				tc.call(bridge)
			}
			env := nextOfType(t, frames, "heartbeat")
			if phaseString(env.LastBootPhase) != phaseString(tc.wantPhase) {
				t.Errorf("heartbeat lastBootPhase = %s, want %s", phaseString(env.LastBootPhase), phaseString(tc.wantPhase))
			}

			cancel()
			if err := wait(); err != nil {
				t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
			}
		})
	}
}

// gatedServer refuses every dial with 503 -- not a fatal status, so the
// Bridge backs off and retries -- until open is called, then serves each
// connection through next. rejected counts the refused dials.
type gatedServer struct {
	next     http.Handler
	opened   atomic.Bool
	rejected atomic.Int32
}

func (g *gatedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.opened.Load() {
		g.rejected.Add(1)
		http.Error(w, "control plane unavailable", http.StatusServiceUnavailable)
		return
	}
	g.next.ServeHTTP(w, r)
}

// TestBootHeartbeats_EvidenceBeforeNullWhateverTheTiming: the boot's start
// phase reaches the control plane before its null phase however the boot
// and the connection interleave -- including a boot that completes before
// the first connection is up, where ReportBootStarted's and
// MarkBootComplete's signals would otherwise coalesce in forceHeartbeat's
// one pending slot into a single null heartbeat. No boot_timing is sent
// here: the start phase alone is this boot's evidence (technical plan
// §3.2), and the owed phase is written once, never again after it.
func TestBootHeartbeats_EvidenceBeforeNullWhateverTheTiming(t *testing.T) {
	t.Parallel()

	initial := wsbridge.InitialBootPhase
	for _, tc := range []struct {
		name string
		// before runs ahead of the first connection; after runs once it
		// has sent "ready". gate makes every dial fail until before has run.
		before, after func(*testing.T, *wsbridge.Bridge, <-chan []byte)
		gate          bool
	}{
		{
			name: "boot started and completed before Run",
			before: func(_ *testing.T, b *wsbridge.Bridge, _ <-chan []byte) {
				b.ReportBootStarted()
				b.MarkBootComplete()
			},
		},
		{
			name: "boot started and completed while the dial backs off",
			gate: true,
			before: func(_ *testing.T, b *wsbridge.Bridge, _ <-chan []byte) {
				b.ReportBootStarted()
				b.MarkBootComplete()
			},
		},
		{
			name: "connected throughout: each call's own heartbeat, nothing owed twice",
			after: func(t *testing.T, b *wsbridge.Bridge, frames <-chan []byte) {
				b.ReportBootStarted()
				if env := nextOfType(t, frames, "heartbeat"); phaseString(env.LastBootPhase) != initial {
					t.Fatalf("heartbeat after ReportBootStarted lastBootPhase = %s, want %q", phaseString(env.LastBootPhase), initial)
				}
				b.MarkBootComplete()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			frames := make(chan []byte, 32)
			gated := &gatedServer{next: &stepServer{steps: []func(*websocket.Conn){recordFrames(frames, 0)}}}
			if !tc.gate {
				gated.opened.Store(true)
			}
			server := httptest.NewServer(gated)
			t.Cleanup(server.Close)

			bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
				testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

			ctx, cancel := context.WithCancel(context.Background())
			var wait func() error
			if tc.gate {
				wait = runInBackground(ctx, bridge)
				deadline := time.Now().Add(testWait)
				for gated.rejected.Load() < 2 {
					if time.Now().After(deadline) {
						t.Fatalf("Run dialed %d times within %s, want at least 2 refused dials", gated.rejected.Load(), testWait)
					}
					time.Sleep(5 * time.Millisecond)
				}
				tc.before(t, bridge, frames)
				gated.opened.Store(true)
			} else {
				if tc.before != nil {
					tc.before(t, bridge, frames)
				}
				wait = runInBackground(ctx, bridge)
			}

			nextOfType(t, frames, "ready")
			if tc.after != nil {
				tc.after(t, bridge, frames)
			}
			var phases []string
			for {
				env := nextOfType(t, frames, "heartbeat")
				phases = append(phases, phaseString(env.LastBootPhase))
				if env.LastBootPhase == nil {
					break
				}
			}
			want := []string{initial, "null"}
			if tc.after != nil {
				want = []string{"null"} // the start phase already went out above
			}
			if strings.Join(phases, ",") != strings.Join(want, ",") {
				t.Fatalf("heartbeats up to the first null = %v, want %v", phases, want)
			}
			// Nothing else is owed: the regular heartbeat is 10s away, so
			// any further heartbeat here would be the start phase again.
			select {
			case data := <-frames:
				t.Fatalf("unexpected frame after the null heartbeat: %s", data)
			case <-time.After(200 * time.Millisecond):
			}

			cancel()
			if err := wait(); err != nil {
				t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
			}
		})
	}
}

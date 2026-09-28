package wsbridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
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

// frameKind names one frame for a wire-sequence comparison: its type, and
// for a heartbeat its boot phase too ("heartbeat:starting",
// "heartbeat:null").
func frameKind(t *testing.T, data []byte) string {
	t.Helper()
	var env testEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("malformed frame %s: %v", data, err)
	}
	if env.Type == "heartbeat" {
		return "heartbeat:" + phaseString(env.LastBootPhase)
	}
	return env.Type
}

// expectFrames reads the next len(want) frames off ch -- every frame, of
// any type -- and fails the test unless their kinds are exactly want, in
// order.
func expectFrames(t *testing.T, ch <-chan []byte, want ...string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for range want {
		select {
		case data := <-ch:
			got = append(got, frameKind(t, data))
		case <-time.After(testWait):
			t.Fatalf("frames = %v, then none within %s; want %v", got, testWait, want)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
}

// expectQuiet fails the test if any frame arrives on ch within d.
func expectQuiet(t *testing.T, ch <-chan []byte, d time.Duration) {
	t.Helper()
	select {
	case data := <-ch:
		t.Fatalf("unexpected frame %s", frameKind(t, data))
	case <-time.After(d):
	}
}

// recordUntil returns a stepServer step that forwards every frame it reads
// to out until drop is done, then closes the connection, so the Bridge
// reconnects.
func recordUntil(drop context.Context, out chan<- []byte) func(*websocket.Conn) {
	return func(conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(drop)
			if err != nil {
				return
			}
			out <- data
		}
	}
}

// TestBootHeartbeats_StartPhaseOnEveryConnectionBeforeItsFirstNull pins,
// frame by frame, the rule ReportBootStarted states: every connection
// carries the boot's start phase ahead of its first null heartbeat, at
// most once, and never after a null. It holds however the boot and the
// connection interleave -- including a boot that completes before the
// first connection is up, where ReportBootStarted's and
// MarkBootComplete's signals would otherwise coalesce in forceHeartbeat's
// one pending slot into a single null heartbeat -- and on the connection
// after a drop, which carries the start phase again: nothing acknowledges
// a heartbeat, so the one the dropped connection wrote may never have
// arrived. No boot_timing is sent: the start phase alone is this boot's
// evidence (technical plan §3.2).
//
// The regular heartbeat is 10s away, so each heartbeat here is forced --
// by ReportBootStarted, MarkBootComplete or a new conversation id -- and
// the test reads every frame the agent writes. A start phase sent again
// ahead of a later heartbeat would be an extra frame, which the exact
// sequences and the quiet checks both catch.
func TestBootHeartbeats_StartPhaseOnEveryConnectionBeforeItsFirstNull(t *testing.T) {
	t.Parallel()

	const (
		ready = "ready"
		start = "heartbeat:" + wsbridge.InitialBootPhase
		null  = "heartbeat:null"
	)
	for _, tc := range []struct {
		name string
		// before runs ahead of the first connection; gate makes every
		// dial fail until it has run.
		before func(*wsbridge.Bridge)
		gate   bool
		// completeInReplay completes the boot on the first connection
		// once its replay has caught up, before its heartbeat loop
		// starts: the connection comes up mid-boot, and its first
		// heartbeat finds the boot complete.
		completeInReplay bool
		// connected drives the boot once the first connection has sent
		// "ready", checking its frames up to the first null; nil means
		// that connection's frames are the start phase, then null.
		connected func(*testing.T, *wsbridge.Bridge, <-chan []byte)
	}{
		{
			name: "boot started and completed before Run",
			before: func(b *wsbridge.Bridge) {
				b.ReportBootStarted()
				b.MarkBootComplete()
			},
		},
		{
			name: "boot started and completed while the dial backs off",
			gate: true,
			before: func(b *wsbridge.Bridge) {
				b.ReportBootStarted()
				b.MarkBootComplete()
			},
		},
		{
			name:             "boot completed after ready, before the connection's first heartbeat",
			before:           (*wsbridge.Bridge).ReportBootStarted,
			completeInReplay: true,
		},
		{
			name: "connected throughout: each call's own heartbeat, nothing owed twice",
			connected: func(t *testing.T, b *wsbridge.Bridge, frames <-chan []byte) {
				b.ReportBootStarted()
				expectFrames(t, frames, start)
				b.MarkBootComplete()
				expectFrames(t, frames, null)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			first := make(chan []byte, 32)
			second := make(chan []byte, 32)
			dropCtx, dropFirst := context.WithCancel(context.Background())
			t.Cleanup(dropFirst)
			gated := &gatedServer{next: &stepServer{steps: []func(*websocket.Conn){
				recordUntil(dropCtx, first),
				recordUntil(context.Background(), second),
			}}}
			if !tc.gate {
				gated.opened.Store(true)
			}
			server := httptest.NewServer(gated)
			t.Cleanup(server.Close)

			bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
				testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
			if tc.completeInReplay {
				var once sync.Once
				wsbridge.SetReplayCaughtUpHookForTest(bridge, func() { once.Do(bridge.MarkBootComplete) })
			}

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
				tc.before(bridge)
				gated.opened.Store(true)
			} else {
				if tc.before != nil {
					tc.before(bridge)
				}
				wait = runInBackground(ctx, bridge)
			}

			expectFrames(t, first, ready)
			if tc.connected != nil {
				tc.connected(t, bridge, first)
			} else {
				expectFrames(t, first, start, null)
			}
			// Every later heartbeat on the same connection is null alone.
			for i := range 3 {
				conversationID := fmt.Sprintf("conv-%d", i)
				bridge.SetConversationID(&conversationID)
				expectFrames(t, first, null)
			}
			expectQuiet(t, first, 200*time.Millisecond)

			// The connection drops. The next one carries the start phase
			// again, right after its "ready" and ahead of its first null,
			// and only there.
			dropFirst()
			expectFrames(t, second, ready, start)
			for i := range 3 {
				conversationID := fmt.Sprintf("conv-after-reconnect-%d", i)
				bridge.SetConversationID(&conversationID)
				expectFrames(t, second, null)
			}
			expectQuiet(t, second, 200*time.Millisecond)

			cancel()
			if err := wait(); err != nil {
				t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
			}
		})
	}
}

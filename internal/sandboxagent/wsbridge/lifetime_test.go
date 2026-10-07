package wsbridge_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// These tests pin what a sandbox-agent reports of its provider's deadline
// (technical plan §35.2): every ready and every heartbeat carries
// lifetimeRemainingSeconds -- the whole seconds left until the deadline the
// provider stated, counted at that frame's own timestamp, never negative --
// and neither carries it when no deadline was stated, which the control
// plane reads as "keep your own estimate".

// lifetimeClock is a clock a test moves by hand, so the seconds left can
// be asserted exactly while the heartbeat loop runs on its real ticker.
type lifetimeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lifetimeClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lifetimeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lifetimeFrame is the part of a ready or heartbeat these tests read:
// its type, and lifetimeRemainingSeconds as it is on the wire.
type lifetimeFrame struct {
	frameType string
	remaining json.RawMessage // nil when the key is absent
}

func decodeLifetimeFrame(t *testing.T, data []byte) lifetimeFrame {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("malformed frame %s: %v", data, err)
	}
	var frameType string
	if err := json.Unmarshal(fields["type"], &frameType); err != nil {
		t.Fatalf("frame without a type: %s", data)
	}
	return lifetimeFrame{frameType: frameType, remaining: fields["lifetimeRemainingSeconds"]}
}

// wholeSeconds matches a JSON integer that is not negative: what every
// lifetimeRemainingSeconds this agent writes must be.
var wholeSeconds = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// remainingValue returns the frame's lifetimeRemainingSeconds, failing the
// test when it is absent or not a whole, non-negative number of seconds.
func (f lifetimeFrame) remainingValue(t *testing.T) int {
	t.Helper()
	if f.remaining == nil {
		t.Fatalf("%s carries no lifetimeRemainingSeconds, want one: a deadline was stated", f.frameType)
	}
	if !wholeSeconds.Match(f.remaining) {
		t.Fatalf("%s lifetimeRemainingSeconds = %s, want whole seconds, never negative", f.frameType, f.remaining)
	}
	var n int
	if err := json.Unmarshal(f.remaining, &n); err != nil {
		t.Fatalf("%s lifetimeRemainingSeconds = %s: %v", f.frameType, f.remaining, err)
	}
	return n
}

// startLifetimeBridge runs a Bridge against a fake control plane that
// forwards every frame it reads, with clock as the bridge's clock and,
// when deadline is not zero, that deadline stated.
func startLifetimeBridge(t *testing.T, clock *lifetimeClock, deadline time.Time) <-chan []byte {
	t.Helper()
	frames := make(chan []byte, 64)
	done := make(chan struct{})
	script := func(conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			select {
			case frames <- data:
			case <-done:
				return
			}
		}
	}
	server := httptest.NewServer(&stepServer{fallback: script})

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testShortHeartbeat, testMinBackoff, testMaxBackoff)
	wsbridge.SetClockForTest(bridge, clock.read)
	if !deadline.IsZero() {
		bridge.SetLifetimeDeadline(deadline)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)
	t.Cleanup(func() {
		cancel()
		close(done)
		if err := wait(); err != nil {
			t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
		}
		server.Close()
	})
	return frames
}

// TestSendReady_LifetimeRemaining is the ready half: the first frame of a
// connection states the seconds left when a deadline was stated, and
// nothing when none was.
func TestSendReady_LifetimeRemaining(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		deadline time.Time
		want     *int
	}{
		{name: "FromTheStatedDeadline", deadline: start.Add(90*time.Minute + 700*time.Millisecond), want: intPtr(5400)},
		{name: "AbsentWithoutOne"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &lifetimeClock{now: start}
			frames := startLifetimeBridge(t, clock, tc.deadline)

			ready := decodeLifetimeFrame(t, waitChan(t, frames, testWait))
			if ready.frameType != "ready" {
				t.Fatalf("first frame is a %s, want the ready", ready.frameType)
			}
			if tc.want == nil {
				if ready.remaining != nil {
					t.Fatalf("ready lifetimeRemainingSeconds = %s with no deadline stated, want the key absent", ready.remaining)
				}
				// A heartbeat states nothing either.
				for {
					frame := decodeLifetimeFrame(t, waitChan(t, frames, testWait))
					if frame.frameType != "heartbeat" {
						continue
					}
					if frame.remaining != nil {
						t.Fatalf("heartbeat lifetimeRemainingSeconds = %s with no deadline stated, want the key absent", frame.remaining)
					}
					return
				}
			}
			if got := ready.remainingValue(t); got != *tc.want {
				t.Fatalf("ready lifetimeRemainingSeconds = %d, want %d", got, *tc.want)
			}
		})
	}
}

// TestHeartbeat_LifetimeRemaining is the heartbeat half: each heartbeat
// counts the seconds left at its own instant, so they count down as the
// clock moves, round down to whole seconds, and stop at 0 once the
// deadline has passed, never going negative.
func TestHeartbeat_LifetimeRemaining(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	type step struct {
		advance time.Duration
		want    int
	}
	tests := []struct {
		name     string
		deadline time.Duration // after start
		steps    []step
	}{
		{name: "CountsDown", deadline: 100 * time.Second, steps: []step{{0, 100}, {30 * time.Second, 70}, {45 * time.Second, 25}}},
		{name: "NeverNegative", deadline: 10 * time.Second, steps: []step{{0, 10}, {10 * time.Second, 0}, {500 * time.Millisecond, 0}, {time.Hour, 0}}},
		{name: "WholeSeconds", deadline: 2*time.Second + 750*time.Millisecond, steps: []step{{0, 2}, {500 * time.Millisecond, 2}, {1500 * time.Millisecond, 0}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &lifetimeClock{now: start}
			frames := startLifetimeBridge(t, clock, start.Add(tc.deadline))

			previous := -1
			for _, s := range tc.steps {
				clock.advance(s.advance)
				// A heartbeat written before the clock moved may still be
				// in flight: it states the previous step's value. Any
				// other value is wrong.
				deadline := time.Now().Add(testWait)
				for {
					if time.Now().After(deadline) {
						t.Fatalf("after advancing %v, no heartbeat stated %d", s.advance, s.want)
					}
					frame := decodeLifetimeFrame(t, waitChan(t, frames, testWait))
					if frame.frameType != "heartbeat" {
						continue
					}
					got := frame.remainingValue(t)
					if got == s.want {
						break
					}
					if got != previous {
						t.Fatalf("heartbeat lifetimeRemainingSeconds = %d after advancing %v, want %d (or %d from before the advance)", got, s.advance, s.want, previous)
					}
				}
				previous = s.want
			}
		})
	}
}

func intPtr(n int) *int { return &n }

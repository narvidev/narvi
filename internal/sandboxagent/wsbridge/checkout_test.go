package wsbridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// checkoutSpy is a CommandHandler that also implements CheckoutHandler,
// recording every checkout and stop it is handed.
type checkoutSpy struct {
	spyHandler
	mu        sync.Mutex
	checkouts []sandboxws.Checkout
}

func (s *checkoutSpy) HandleCheckout(_ context.Context, cmd sandboxws.Checkout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkouts = append(s.checkouts, cmd)
}

func (s *checkoutSpy) checkoutsSnapshot() []sandboxws.Checkout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sandboxws.Checkout(nil), s.checkouts...)
}

func checkoutFrame(messageID string, gen int) string {
	return fmt.Sprintf(`{"type":"checkout","messageId":%q,"sessionId":%q,"gen":%d,"repos":[{"name":"widgets","ref":"refs/pull/7/head","sha":"0123456789abcdef0123456789abcdef01234567"}]}`,
		messageID, testSessionID, gen)
}

// readyReviewCheckout decodes capabilities.reviewCheckout from a ready
// frame: nil when the key is absent.
func readyReviewCheckout(t *testing.T, ready []byte) *bool {
	t.Helper()
	var r struct {
		Type         string `json:"type"`
		Capabilities *struct {
			ReviewCheckout *bool `json:"reviewCheckout"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(ready, &r); err != nil || r.Type != "ready" {
		t.Fatalf("first frame %s is not a ready: %v", ready, err)
	}
	if r.Capabilities == nil {
		return nil
	}
	return r.Capabilities.ReviewCheckout
}

// TestCheckout_RoutedToItsHandlerAndAdvertised: with a handler that runs
// checkouts, every ready advertises capabilities.reviewCheckout, a checkout
// of this gen reaches HandleCheckout, and one of another gen never does
// (technical plan §21.1, §30.4).
func TestCheckout_RoutedToItsHandlerAndAdvertised(t *testing.T) {
	t.Parallel()

	ready := make(chan []byte, 1)
	script := func(conn *websocket.Conn) {
		frame, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		ready <- frame
		for _, cmd := range []string{checkoutFrame("stale", 999), checkoutFrame("fresh", testGen)} {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(cmd)); err != nil {
				t.Errorf("write checkout: %v", err)
				return
			}
		}
		absorbForever(conn)
	}
	server := httptest.NewServer(&stepServer{steps: []func(*websocket.Conn){script}})
	t.Cleanup(server.Close)

	spy := &checkoutSpy{}
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", spy,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	if got := readyReviewCheckout(t, waitChan(t, ready, testWait)); got == nil || !*got {
		t.Errorf("ready capabilities.reviewCheckout = %v, want true for a handler that runs checkouts", got)
	}
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && len(spy.checkoutsSnapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// The fresh checkout follows the stale one on the wire, so once it has
	// arrived the stale one has been read and decided on.
	got := spy.checkoutsSnapshot()
	if len(got) != 1 || got[0].MessageId != "fresh" {
		t.Fatalf("HandleCheckout calls = %+v, want exactly the fresh-gen checkout", got)
	}
	if len(got[0].Repos) != 1 || got[0].Repos[0].Ref != "refs/pull/7/head" {
		t.Errorf("dispatched checkout repos = %+v, want the one decoded repo", got[0].Repos)
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// TestCheckout_WithoutAHandlerSkippedAndNotAdvertised: a handler that does
// not run checkouts is never advertised as one, and a checkout sent anyway
// is skipped like an unknown command: the read loop goes on to the next.
func TestCheckout_WithoutAHandlerSkippedAndNotAdvertised(t *testing.T) {
	t.Parallel()

	ready := make(chan []byte, 1)
	script := func(conn *websocket.Conn) {
		frame, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		ready <- frame
		stop := fmt.Sprintf(`{"type":"stop","messageId":"after","sessionId":%q,"gen":%d}`, testSessionID, testGen)
		for _, cmd := range []string{checkoutFrame("ignored", testGen), stop} {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(cmd)); err != nil {
				t.Errorf("write command: %v", err)
				return
			}
		}
		absorbForever(conn)
	}
	server := httptest.NewServer(&stepServer{steps: []func(*websocket.Conn){script}})
	t.Cleanup(server.Close)

	spy := &spyHandler{}
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", spy,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)
	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	if got := readyReviewCheckout(t, waitChan(t, ready, testWait)); got != nil {
		t.Errorf("ready capabilities.reviewCheckout = %v, want it absent for a handler that does not run checkouts", *got)
	}
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && len(spy.stopsSnapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if stops := spy.stopsSnapshot(); len(stops) != 1 || stops[0].MessageId != "after" {
		t.Fatalf("HandleStop calls = %+v, want the stop behind the skipped checkout", stops)
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

package wsbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// --- shared test fixtures -----------------------------------------------

const (
	testDialTimeout    = 2 * time.Second
	testMinBackoff     = 20 * time.Millisecond
	testMaxBackoff     = 100 * time.Millisecond
	testLongHeartbeat  = 10 * time.Second // long enough to never fire during a short test
	testShortHeartbeat = 40 * time.Millisecond
	testWait           = 2 * time.Second
)

const (
	testSessionID = "sess-test"
	testGen       = 7
)

func testSessionConfig(url string) sessionconfig.SessionConfig {
	return sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: url,
		CorrelationId:     nil,
		Gen:               testGen,
		Repos:             nil,
		SandboxToken:      "test-sandbox-token",
		SessionId:         testSessionID,
	}
}

// noopHandler implements wsbridge.CommandHandler doing nothing -- for
// tests that never expect a dispatch at all.
type noopHandler struct{}

func (noopHandler) HandlePrompt(context.Context, sandboxws.Prompt)                   {}
func (noopHandler) HandleStop(context.Context, sandboxws.Stop)                       {}
func (noopHandler) HandlePush(context.Context, sandboxws.Push)                       {}
func (noopHandler) HandleSnapshot(context.Context, sandboxws.Snapshot)               {}
func (noopHandler) HandleGitSyncComplete(context.Context, sandboxws.GitSyncComplete) {}

// spyHandler records every HandleStop call (the only command type this
// test file needs to spy on) behind a mutex, so tests can assert exactly
// which commands were actually dispatched.
type spyHandler struct {
	mu    sync.Mutex
	stops []sandboxws.Stop
}

func (*spyHandler) HandlePrompt(context.Context, sandboxws.Prompt) {}
func (s *spyHandler) HandleStop(_ context.Context, cmd sandboxws.Stop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, cmd)
}
func (*spyHandler) HandlePush(context.Context, sandboxws.Push)                       {}
func (*spyHandler) HandleSnapshot(context.Context, sandboxws.Snapshot)               {}
func (*spyHandler) HandleGitSyncComplete(context.Context, sandboxws.GitSyncComplete) {}

func (s *spyHandler) stopsSnapshot() []sandboxws.Stop {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sandboxws.Stop, len(s.stops))
	copy(out, s.stops)
	return out
}

// testEnvelope is this test file's own loose decode target for asserting
// on whatever subset of fields a given test cares about -- deliberately
// NOT contracts/gen/go/sandboxws's own generated types, since those
// enforce "type" equality/required-ness that would make it awkward to
// peek at a message without already knowing its exact shape.
type testEnvelope struct {
	Type           string  `json:"type"`
	AckID          string  `json:"ackId"`
	MessageID      string  `json:"messageId"`
	Phase          string  `json:"phase"`
	LastBootPhase  *string `json:"lastBootPhase"`
	ConversationID *string `json:"conversationId"`
	// AgentVersion/ImageDigest (§12.2 item 1's own runtime-fingerprint
	// gap) are only ever non-empty on a "ready" frame -- zero value ""
	// for every other message type this shared peek struct decodes.
	AgentVersion string `json:"agentVersion"`
	ImageDigest  string `json:"imageDigest"`
}

// runInBackground launches bridge.Run(ctx) through an errgroup (never a
// naked `go` statement, §11 -- nakedgoroutine applies to test files too)
// and returns a wait func the test calls once it is done driving the fake
// server, to observe Run's own return value.
func runInBackground(ctx context.Context, bridge *wsbridge.Bridge) func() error {
	var group errgroup.Group
	group.Go(func() error {
		return bridge.Run(ctx)
	})
	return group.Wait
}

// waitChan reads exactly one value from ch, failing the test if timeout
// elapses first.
func waitChan[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for a channel value", timeout)
		var zero T
		return zero
	}
}

// serverRead reads one message off conn bounded by timeout, returning the
// raw payload and any error -- used from fake-server handler goroutines,
// which must never call t.Fatal/t.FailNow directly (only the goroutine
// running the Test function may; see testing.T's own doc comment). Callers
// forward the result over a channel for the main test goroutine to assert
// on instead.
func serverRead(conn *websocket.Conn, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	return data, err
}

// absorbForever reads (and discards) messages until conn errors (the
// client disconnected, e.g. because the test canceled its own ctx) --
// used to keep a fake-server connection alive without asserting anything
// further about it.
func absorbForever(conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
	}
}

// stepServer is a fake sandbox-WS server whose behavior for each
// successive accepted connection is scripted in advance -- steps are
// consumed FIFO, one per connection; once exhausted, fallback runs for
// every further connection. Every step function runs on the http.Server's
// own per-request goroutine (never one this test package spawns itself via
// a naked `go`).
type stepServer struct {
	mu       sync.Mutex
	steps    []func(conn *websocket.Conn)
	fallback func(conn *websocket.Conn)
}

func (s *stepServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var step func(conn *websocket.Conn)
	if len(s.steps) > 0 {
		step = s.steps[0]
		s.steps = s.steps[1:]
	} else {
		step = s.fallback
	}
	s.mu.Unlock()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	if step != nil {
		step(conn)
	}
}

// --- fatal status classification -----------------------------------------

func TestRun_FatalStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Parallel()

			var attempts int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)

			bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
				testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()

			err := bridge.Run(ctx)

			var fatalErr *wsbridge.FatalConnectError
			if !errors.As(err, &fatalErr) {
				t.Fatalf("Run() error = %v (%T), want *FatalConnectError", err, err)
			}
			if fatalErr.Status != status {
				t.Errorf("FatalConnectError.Status = %d, want %d", fatalErr.Status, status)
			}
			if got := atomic.LoadInt32(&attempts); got != 1 {
				t.Errorf("connection attempts = %d, want exactly 1 (no retry on a fatal status)", got)
			}
		})
	}
}

// --- non-fatal retry with bounded backoff --------------------------------

func TestRun_NonFatalRetryThenSucceeds(t *testing.T) {
	t.Parallel()

	const failCount = 2
	var attempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= failCount {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		absorbForever(conn)
	}))
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	start := time.Now()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && atomic.LoadInt32(&attempts) <= failCount {
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&attempts); got <= failCount {
		t.Fatalf("connect never succeeded within %s (attempts = %d)", testWait, got)
	}

	// Lower bound: at least failCount backoff waits of at least
	// testMinBackoff each must have elapsed -- proves backoff is actually
	// applied, not a tight retry loop.
	lowerBound := failCount * testMinBackoff
	if elapsed < lowerBound {
		t.Errorf("elapsed = %s before succeeding, want at least %s (backoff should have been applied)", elapsed, lowerBound)
	}
	// Upper bound: generous, just catches a runaway/no-cap backoff bug.
	upperBound := 2 * time.Second
	if elapsed > upperBound {
		t.Errorf("elapsed = %s before succeeding, want under %s", elapsed, upperBound)
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- "ready" is always the first message, including after a reconnect ---

func TestRun_ReadyFirstOnConnectAndReconnect(t *testing.T) {
	t.Parallel()

	readyCh := make(chan []byte, 2)

	firstConn := func(conn *websocket.Conn) {
		data, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("conn1: read ready: %v", err)
			return
		}
		readyCh <- data
		// Force an abrupt disconnect so Run must reconnect.
	}
	secondConn := func(conn *websocket.Conn) {
		data, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("conn2: read ready: %v", err)
			return
		}
		readyCh <- data
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){firstConn, secondConn}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	for i := 0; i < 2; i++ {
		data := waitChan(t, readyCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("connection %d: malformed first message: %v", i+1, err)
		}
		if env.Type != "ready" {
			t.Errorf("connection %d: first message type = %q, want %q", i+1, env.Type, "ready")
		}
		// §12.2 item 1's own runtime-fingerprint gap: every "ready" frame,
		// including after a reconnect, must carry the SAME real
		// agentVersion/imageDigest this Bridge was built with.
		if env.AgentVersion != "test-agent-version" {
			t.Errorf("connection %d: AgentVersion = %q, want %q", i+1, env.AgentVersion, "test-agent-version")
		}
		if env.ImageDigest != "test-image-digest" {
			t.Errorf("connection %d: ImageDigest = %q, want %q", i+1, env.ImageDigest, "test-image-digest")
		}
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- critical event ack protocol: resend until acked, never after -------

func TestSendCritical_ResendUntilAckedThenNeverAgain(t *testing.T) {
	t.Parallel()

	readyCh := make(chan struct{}, 3)
	critCh := make(chan []byte, 2)
	const ackID = "execution_complete:msg-1"

	conn1 := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("conn1: read ready: %v", err)
			return
		}
		readyCh <- struct{}{}

		data, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("conn1: read critical event: %v", err)
			return
		}
		critCh <- data
		// Abrupt disconnect -- deliberately never ack.
	}

	conn2 := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("conn2: read ready: %v", err)
			return
		}
		readyCh <- struct{}{}

		data, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("conn2: read resent critical event: %v", err)
			return
		}
		critCh <- data

		ackMsg := fmt.Sprintf(`{"type":"ack","messageId":"ack-1","sessionId":%q,"gen":%d,"ackId":%q}`,
			testSessionID, testGen, ackID)
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(ackMsg)); err != nil {
			t.Errorf("conn2: write ack: %v", err)
			return
		}
		// Force a THIRD connection so the "never resent again, even
		// across a LATER reconnect" scenario is actually exercised.
	}

	noResendCh := make(chan bool, 1)
	conn3 := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("conn3: read ready: %v", err)
			return
		}
		readyCh <- struct{}{}

		_, err := serverRead(conn, 300*time.Millisecond)
		noResendCh <- err != nil // true = timed out = correctly NOT resent
		absorbForever(conn)
	}

	fake := &stepServer{
		steps: []func(*websocket.Conn){conn1, conn2, conn3},
		fallback: func(conn *websocket.Conn) {
			absorbForever(conn)
		},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	waitChan(t, readyCh, testWait) // conn1 ready

	msg := sandboxws.ExecutionComplete{
		Type:      "execution_complete",
		MessageId: "msg-1",
		SessionId: testSessionID,
		Gen:       testGen,
		AckId:     ackID,
		Outcome:   sandboxws.ExecutionCompleteOutcomeCompleted,
		Reason:    nil,
	}
	if err := bridge.SendCritical(ctx, msg, ackID); err != nil {
		t.Fatalf("SendCritical() error = %v, want nil", err)
	}

	first := waitChan(t, critCh, testWait)

	waitChan(t, readyCh, testWait) // conn2 ready (reconnect happened)
	second := waitChan(t, critCh, testWait)

	if string(first) != string(second) {
		t.Errorf("resent critical payload differs from original:\nfirst:  %s\nsecond: %s", first, second)
	}
	var env testEnvelope
	if err := json.Unmarshal(second, &env); err != nil {
		t.Fatalf("malformed resent critical payload: %v", err)
	}
	if env.AckID != ackID {
		t.Errorf("resent critical AckId = %q, want %q", env.AckID, ackID)
	}

	waitChan(t, readyCh, testWait) // conn3 ready (reconnect after ack)
	if !waitChan(t, noResendCh, testWait) {
		t.Error("critical event was resent again on conn3 even though it was already acked on conn2")
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- inbound per-message gen-fencing -------------------------------------

func TestDispatch_GenFencing(t *testing.T) {
	t.Parallel()

	script := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		stale := fmt.Sprintf(`{"type":"stop","messageId":"stale","sessionId":%q,"gen":999}`, testSessionID)
		fresh := fmt.Sprintf(`{"type":"stop","messageId":"fresh","sessionId":%q,"gen":%d}`, testSessionID, testGen)
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(stale)); err != nil {
			t.Errorf("write stale-gen stop: %v", err)
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(fresh)); err != nil {
			t.Errorf("write fresh-gen stop: %v", err)
			return
		}
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	spy := &spyHandler{}
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", spy,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && len(spy.stopsSnapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	stops := spy.stopsSnapshot()
	if len(stops) != 1 {
		t.Fatalf("HandleStop called %d times, want exactly 1 (stale-gen command must never dispatch)", len(stops))
	}
	if stops[0].MessageId != "fresh" {
		t.Errorf("dispatched stop.MessageId = %q, want %q", stops[0].MessageId, "fresh")
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- unrecognized command type: logged, skipped, doesn't wedge the loop --

func TestDispatch_UnrecognizedTypeIsSkippedNotFatal(t *testing.T) {
	t.Parallel()

	script := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		bogus := `{"type":"some_future_command_type","messageId":"b1"}`
		valid := fmt.Sprintf(`{"type":"stop","messageId":"ok","sessionId":%q,"gen":%d}`, testSessionID, testGen)
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(bogus)); err != nil {
			t.Errorf("write bogus command: %v", err)
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(valid)); err != nil {
			t.Errorf("write valid command: %v", err)
			return
		}
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	spy := &spyHandler{}
	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", spy,
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && len(spy.stopsSnapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	stops := spy.stopsSnapshot()
	if len(stops) != 1 || stops[0].MessageId != "ok" {
		t.Fatalf("HandleStop calls = %+v, want exactly one call with MessageId %q "+
			"(the unrecognized type before it must not crash/hang the read loop)", stops, "ok")
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- boot_progress translation -------------------------------------------

func TestSendBootProgress_Translation(t *testing.T) {
	t.Parallel()

	readyCh := make(chan struct{}, 1)
	bootCh := make(chan []byte, 1)

	script := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		readyCh <- struct{}{}

		data, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("read boot_progress: %v", err)
			return
		}
		bootCh <- data
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	waitChan(t, readyCh, testWait)

	if err := bridge.SendBootProgress(ctx, services.BootProgressEvent{
		ServiceName: "web",
		Phase:       services.PhaseReady,
	}); err != nil {
		t.Fatalf("SendBootProgress() error = %v, want nil", err)
	}

	data := waitChan(t, bootCh, testWait)
	var env testEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("malformed boot_progress payload: %v", err)
	}
	if env.Type != "boot_progress" {
		t.Errorf("type = %q, want %q", env.Type, "boot_progress")
	}
	if env.Phase != "web:ready" {
		t.Errorf("phase = %q, want %q", env.Phase, "web:ready")
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// TestSendBootProgress_ServiceNameWithColonIsEscaped proves a service name
// containing a colon can never be confused with the "<serviceName>:<phase>"
// separator itself -- servicemanifest's own Service.Name validation
// (§14.2) only requires non-empty/unique, no charset restriction, so this is a
// real input the wire encoding must handle unambiguously rather than
// assume away.
func TestSendBootProgress_ServiceNameWithColonIsEscaped(t *testing.T) {
	t.Parallel()

	readyCh := make(chan struct{}, 1)
	bootCh := make(chan []byte, 1)

	script := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		readyCh <- struct{}{}

		data, err := serverRead(conn, testWait)
		if err != nil {
			t.Errorf("read boot_progress: %v", err)
			return
		}
		bootCh <- data
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	waitChan(t, readyCh, testWait)

	// A service literally named "web:ready" reporting PhaseReady must NOT
	// produce the same wire string as service "web" reporting phase
	// "ready:ready" or any other service+phase pair -- i.e. the encoded
	// service-name portion and the phase portion must be unambiguously
	// separable regardless of what characters the name itself contains.
	if err := bridge.SendBootProgress(ctx, services.BootProgressEvent{
		ServiceName: "web:ready",
		Phase:       services.PhaseStarting,
	}); err != nil {
		t.Fatalf("SendBootProgress() error = %v, want nil", err)
	}

	data := waitChan(t, bootCh, testWait)
	var env testEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("malformed boot_progress payload: %v", err)
	}
	if env.Phase == "web:ready:starting" {
		t.Fatalf("phase = %q, ambiguous with service %q in phase %q", env.Phase, "web", "ready:starting")
	}
	// The service name itself must be recoverable by splitting on the
	// FIRST colon only after undoing the escaping -- i.e. everything before
	// the first literal ':' in the wire string, percent-decoded, must equal
	// the original service name exactly.
	firstColon := strings.IndexByte(env.Phase, ':')
	if firstColon < 0 {
		t.Fatalf("phase = %q, want a %q-delimited service:phase pair", env.Phase, ":")
	}
	decoded, err := url.QueryUnescape(env.Phase[:firstColon])
	if err != nil {
		t.Fatalf("QueryUnescape(%q): %v", env.Phase[:firstColon], err)
	}
	if decoded != "web:ready" {
		t.Errorf("decoded service name = %q, want %q", decoded, "web:ready")
	}
	if env.Phase[firstColon+1:] != "starting" {
		t.Errorf("phase suffix = %q, want %q", env.Phase[firstColon+1:], "starting")
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- heartbeat: interval, tracked boot phase, cleared on MarkBootComplete -

func TestHeartbeat_TracksBootPhaseAndClearsOnComplete(t *testing.T) {
	t.Parallel()

	msgCh := make(chan []byte, 32)

	script := func(conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			msgCh <- data
		}
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testShortHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	// First message is "ready"; drain it.
	readyData := waitChan(t, msgCh, testWait)
	var readyEnv testEnvelope
	if err := json.Unmarshal(readyData, &readyEnv); err != nil || readyEnv.Type != "ready" {
		t.Fatalf("first message = %s, want a ready event", readyData)
	}

	if err := bridge.SendBootProgress(ctx, services.BootProgressEvent{
		ServiceName: "web",
		Phase:       services.PhaseStarting,
	}); err != nil {
		t.Fatalf("SendBootProgress() error = %v, want nil", err)
	}

	// Drain messages until we see a heartbeat carrying the tracked phase.
	var sawTrackedPhase bool
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && !sawTrackedPhase {
		data := waitChan(t, msgCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v", err)
		}
		if env.Type != "heartbeat" {
			continue
		}
		if env.LastBootPhase != nil && *env.LastBootPhase == "web:starting" {
			sawTrackedPhase = true
		}
	}
	if !sawTrackedPhase {
		t.Fatal("never observed a heartbeat with lastBootPhase = \"web:starting\"")
	}

	bridge.MarkBootComplete()

	var sawClearedPhase bool
	deadline = time.Now().Add(testWait)
	for time.Now().Before(deadline) && !sawClearedPhase {
		data := waitChan(t, msgCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v", err)
		}
		if env.Type != "heartbeat" {
			continue
		}
		if env.LastBootPhase == nil {
			sawClearedPhase = true
		}
	}
	if !sawClearedPhase {
		t.Fatal("never observed a heartbeat with lastBootPhase = null after MarkBootComplete")
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

func TestHeartbeat_TracksConversationID(t *testing.T) {
	t.Parallel()

	msgCh := make(chan []byte, 32)

	script := func(conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			msgCh <- data
		}
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testShortHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	// First message is "ready"; drain it.
	readyData := waitChan(t, msgCh, testWait)
	var readyEnv testEnvelope
	if err := json.Unmarshal(readyData, &readyEnv); err != nil || readyEnv.Type != "ready" {
		t.Fatalf("first message = %s, want a ready event", readyData)
	}

	// Before any SetConversationID call, every heartbeat carries a null
	// conversationId -- events.schema.json's own Heartbeat.ConversationId
	// doc comment: "Null before the first turn has started a conversation."
	var sawNullConversationID bool
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && !sawNullConversationID {
		data := waitChan(t, msgCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v", err)
		}
		if env.Type == "heartbeat" && env.ConversationID == nil {
			sawNullConversationID = true
		}
	}
	if !sawNullConversationID {
		t.Fatal("never observed a heartbeat with conversationId = null before SetConversationID")
	}

	convID := "ses_abc123"
	bridge.SetConversationID(&convID)

	var sawTrackedConversationID bool
	deadline = time.Now().Add(testWait)
	for time.Now().Before(deadline) && !sawTrackedConversationID {
		data := waitChan(t, msgCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v", err)
		}
		if env.Type == "heartbeat" && env.ConversationID != nil && *env.ConversationID == convID {
			sawTrackedConversationID = true
		}
	}
	if !sawTrackedConversationID {
		t.Fatalf("never observed a heartbeat with conversationId = %q after SetConversationID", convID)
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// TestHeartbeat_SetConversationIDTriggersImmediateHeartbeat proves
// §3.3's ("turn recovery") own "at turn start... never lazily" fix: a
// heartbeat carrying a genuinely NEW, non-nil conversation id arrives well
// under the configured heartbeat interval, not only once that interval's
// own next regular tick happens to come due. Uses a deliberately LONG
// heartbeat interval (testLongHeartbeat, 10s): if SetConversationID's own
// immediate-trigger path did NOT work, the ONLY way a heartbeat carrying
// the new id could arrive within this test's own testWait window (2s)
// would be the regular ticker -- which isn't due for another 10s. Observing
// one within testWait therefore proves the immediate (forceHeartbeat) path
// fired, not the regular cadence.
func TestHeartbeat_SetConversationIDTriggersImmediateHeartbeat(t *testing.T) {
	t.Parallel()

	msgCh := make(chan []byte, 32)

	script := func(conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			msgCh <- data
		}
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	// First message is "ready"; drain it.
	readyData := waitChan(t, msgCh, testWait)
	var readyEnv testEnvelope
	if err := json.Unmarshal(readyData, &readyEnv); err != nil || readyEnv.Type != "ready" {
		t.Fatalf("first message = %s, want a ready event", readyData)
	}

	convID := "ses_immediate_1"
	bridge.SetConversationID(&convID)

	var sawImmediateHeartbeat bool
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && !sawImmediateHeartbeat {
		data := waitChan(t, msgCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v", err)
		}
		if env.Type == "heartbeat" && env.ConversationID != nil && *env.ConversationID == convID {
			sawImmediateHeartbeat = true
		}
	}
	if !sawImmediateHeartbeat {
		t.Fatalf("never observed an immediate heartbeat with conversationId = %q within %s (well under the %s regular interval)",
			convID, testWait, testLongHeartbeat)
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// TestHeartbeat_SetConversationIDUnchanged_NoRepeatedImmediateHeartbeat
// proves the OTHER half of §3.3's own requirement: SetConversationID
// only triggers an immediate heartbeat when the value GENUINELY changes,
// not on every call -- a second call carrying the SAME already-current id
// (e.g. a later turn resuming the same conversation) must not trigger a
// second one. Uses the SAME long-heartbeat-interval technique as the test
// above: with the regular ticker 10s away, any heartbeat observed within
// a short bounded window after the second, unchanged-value call can only
// be explained by an (incorrect) second immediate trigger.
func TestHeartbeat_SetConversationIDUnchanged_NoRepeatedImmediateHeartbeat(t *testing.T) {
	t.Parallel()

	msgCh := make(chan []byte, 32)

	script := func(conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			msgCh <- data
		}
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithCancel(context.Background())
	wait := runInBackground(ctx, bridge)

	readyData := waitChan(t, msgCh, testWait)
	var readyEnv testEnvelope
	if err := json.Unmarshal(readyData, &readyEnv); err != nil || readyEnv.Type != "ready" {
		t.Fatalf("first message = %s, want a ready event", readyData)
	}

	convID := "ses_stable_1"
	bridge.SetConversationID(&convID)

	// Drain until the first immediate heartbeat carrying it -- confirms
	// the forceHeartbeat signal from THIS call has already been consumed
	// by heartbeatLoop before this test calls SetConversationID again
	// below.
	var sawFirstImmediate bool
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) && !sawFirstImmediate {
		data := waitChan(t, msgCh, testWait)
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("malformed message: %v", err)
		}
		if env.Type == "heartbeat" && env.ConversationID != nil && *env.ConversationID == convID {
			sawFirstImmediate = true
		}
	}
	if !sawFirstImmediate {
		t.Fatal("never observed the first immediate heartbeat")
	}

	// Calling SetConversationID again with an equal (but distinct pointer)
	// value must NOT trigger a second immediate send -- only a genuine
	// CHANGE does.
	sameValue := convID
	bridge.SetConversationID(&sameValue)

	select {
	case data := <-msgCh:
		var env testEnvelope
		if err := json.Unmarshal(data, &env); err == nil && env.Type == "heartbeat" {
			t.Fatalf("observed an unexpected extra heartbeat after SetConversationID with an unchanged value: %s", data)
		}
	case <-time.After(300 * time.Millisecond):
		// Expected: nothing else arrives (the regular ticker is still
		// testLongHeartbeat=10s away).
	}

	cancel()
	if err := wait(); err != nil {
		t.Errorf("Run() error = %v, want nil after ctx cancellation", err)
	}
}

// --- shutdown command convergence ----------------------------------------

func TestRun_ShutdownCommandReturnsErrShutdownRequested(t *testing.T) {
	t.Parallel()

	script := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		shutdown := fmt.Sprintf(`{"type":"shutdown","messageId":"sd-1","sessionId":%q,"gen":%d}`, testSessionID, testGen)
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(shutdown)); err != nil {
			t.Errorf("write shutdown: %v", err)
			return
		}
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	err := bridge.Run(ctx)
	if !errors.Is(err, wsbridge.ErrShutdownRequested) {
		t.Fatalf("Run() error = %v, want ErrShutdownRequested", err)
	}
}

func TestRun_StaleGenShutdownIsIgnored(t *testing.T) {
	t.Parallel()

	script := func(conn *websocket.Conn) {
		if _, err := serverRead(conn, testWait); err != nil {
			t.Errorf("read ready: %v", err)
			return
		}
		stale := fmt.Sprintf(`{"type":"shutdown","messageId":"sd-1","sessionId":%q,"gen":999}`, testSessionID)
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(stale)); err != nil {
			t.Errorf("write stale-gen shutdown: %v", err)
			return
		}
		absorbForever(conn)
	}

	fake := &stepServer{steps: []func(*websocket.Conn){script}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	bridge := wsbridge.New(testSessionConfig(server.URL), "sbx-1", "test-agent-version", "test-image-digest", noopHandler{},
		testDialTimeout, testLongHeartbeat, testMinBackoff, testMaxBackoff)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := bridge.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (stale-gen shutdown must be ignored, ctx timeout should be what ends Run)", err)
	}
}

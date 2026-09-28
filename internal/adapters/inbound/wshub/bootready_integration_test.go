//go:build integration

// Booting -> Ready end to end, with the REAL sandbox-agent side of the
// wire: an internal/sandboxagent/wsbridge.Bridge (the exact client
// cmd/sandbox-agent runs) dialing this package's real sandbox handler, a
// real sessionactor.Actor and a real Postgres. The hand-written heartbeat
// frames every other test sends (dispatch_test.go's own step (d)) cannot
// show what the agent itself puts on the wire while its boot is still
// running, which is what this file pins (technical plan §3.2).
package wshub_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// bootReadyShortHeartbeat lets several heartbeats cross the wire while a
// test holds the agent's boot open; bootReadyLongHeartbeat keeps the
// regular heartbeat out of a test entirely, so any heartbeat it sees was
// forced.
const (
	bootReadyShortHeartbeat = 40 * time.Millisecond
	bootReadyLongHeartbeat  = 10 * time.Second
)

// noopCommands is a wsbridge.CommandHandler that ignores every command:
// these tests only drive the agent's own events.
type noopCommands struct{}

func (noopCommands) HandlePrompt(context.Context, sandboxws.Prompt)                   {}
func (noopCommands) HandleStop(context.Context, sandboxws.Stop)                       {}
func (noopCommands) HandlePush(context.Context, sandboxws.Push)                       {}
func (noopCommands) HandleSnapshot(context.Context, sandboxws.Snapshot)               {}
func (noopCommands) HandleGitSyncComplete(context.Context, sandboxws.GitSyncComplete) {}

// connSeverer wraps the sandbox handler and remembers every connection it
// upgrades, so a test can drop them all the way a network failure would --
// the agent then reconnects on its own.
type connSeverer struct {
	next  http.Handler
	mu    sync.Mutex
	conns []net.Conn
}

func (s *connSeverer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.next.ServeHTTP(&hijackRecorder{ResponseWriter: w, s: s}, r)
}

func (s *connSeverer) severAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

type hijackRecorder struct {
	http.ResponseWriter
	s *connSeverer
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(h.ResponseWriter).Hijack()
	if err == nil {
		h.s.mu.Lock()
		h.s.conns = append(h.s.conns, conn)
		h.s.mu.Unlock()
	}
	return conn, rw, err
}

// bootReadyFixture is one session whose gen-1 sandbox is Connecting,
// behind the real sandbox handler.
type bootReadyFixture struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	wsURL     string
	severer   *connSeverer
}

func newBootReadyFixture(ctx context.Context, t *testing.T) bootReadyFixture {
	t.Helper()
	return newBootReadyFixtureWithTimeouts(ctx, t, nil)
}

// newBootReadyFixtureWithTimeouts is newBootReadyFixture with the control
// plane's timeouts adjusted by adjust first (nil: the defaults).
func newBootReadyFixtureWithTimeouts(ctx context.Context, t *testing.T, adjust func(*platform.Timeouts)) bootReadyFixture {
	t.Helper()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createTestSandbox(ctx, t, pool, sessionID) // gen 1, Pending
	moveSandboxStatus(ctx, t, pool, sessionID, sqlcgen.SandboxStatusConnecting)

	timeouts := platform.DefaultTimeouts()
	if adjust != nil {
		adjust(&timeouts)
	}
	registry, err := sessionactor.NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	severer := &connSeverer{next: wshub.NewSandboxHandler(registry, narvipg.NewSandboxStore(pool), wshub.NewSandboxRegistry(timeouts), timeouts)}
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", severer.ServeHTTP)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return bootReadyFixture{
		pool:      pool,
		sessionID: sessionID,
		wsURL:     "ws" + strings.TrimPrefix(server.URL, "http") + "/sessions/" + sessionID.String() + "/ws?type=sandbox",
		severer:   severer,
	}
}

func (f bootReadyFixture) status(ctx context.Context, t *testing.T) sqlcgen.SandboxStatus {
	t.Helper()
	return getSandbox(ctx, t, f.pool, f.sessionID).Status
}

func (f bootReadyFixture) waitStatus(ctx context.Context, t *testing.T, want sqlcgen.SandboxStatus) {
	t.Helper()
	waitUntil(t, dispatchTestWait, func() bool { return f.status(ctx, t) == want })
}

// startBridge runs a real wsbridge.Bridge for the fixture's gen-1 sandbox,
// stopped and drained at test cleanup.
func (f bootReadyFixture) startBridge(t *testing.T, heartbeat time.Duration, beforeRun func(*wsbridge.Bridge)) *wsbridge.Bridge {
	t.Helper()
	return f.startBridgeAt(t, f.wsURL, heartbeat, beforeRun)
}

// startBridgeAt is startBridge dialing wsURL instead of the handler
// itself -- a proxy in front of it.
func (f bootReadyFixture) startBridgeAt(t *testing.T, wsURL string, heartbeat time.Duration, beforeRun func(*wsbridge.Bridge)) *wsbridge.Bridge {
	t.Helper()
	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: wsURL,
		Gen:               1,
		SandboxToken:      "some-token",
		SessionId:         f.sessionID.String(),
	}
	bridge := wsbridge.New(sc, "sbx-test", "test-agent-version", "test-image-digest", noopCommands{},
		2*time.Second, heartbeat, 20*time.Millisecond, 100*time.Millisecond)
	if beforeRun != nil {
		beforeRun(bridge)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var group errgroup.Group
	group.Go(func() error { return bridge.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("bridge.Run() error = %v, want nil after cancellation", err)
		}
	})
	return bridge
}

// TestBootReady_RealBridge_NotReadyUntilBootCompletes is the reproduction
// of a sandbox marked Ready while its boot had not run: the agent dials
// before it clones (cmd/sandbox-agent starts bridge.Run ahead of
// runBootSequence), and nothing in the clone, git-dir sync or repo hooks
// reports a boot phase -- only services and dockerd do. Every heartbeat of
// that window used to carry lastBootPhase: null, the contract's "boot has
// completed", and the first one moved the sandbox Booting -> Ready.
//
// Run against both control-plane behaviors: this one, with §3.2's
// boot-evidence rule, and the null-phase rule alone -- a control plane
// built before the evidence rule, emulated by recording evidence up front
// so the null phase is the only thing left to decide on. The agent alone
// must keep the second correct.
func TestBootReady_RealBridge_NotReadyUntilBootCompletes(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		seedEvidence bool
	}{
		{name: "boot-evidence rule", seedEvidence: false},
		{name: "null-phase rule alone (a control plane built before the evidence rule)", seedEvidence: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootReadyFixture(ctx, t)
			if tc.seedEvidence {
				if err := narvipg.NewSandboxStore(f.pool).MarkBootEvidence(ctx, f.sessionID, 1); err != nil {
					t.Fatalf("MarkBootEvidence: %v", err)
				}
			}
			bridge := f.startBridge(t, bootReadyShortHeartbeat, nil)

			// "ready": Connecting -> Booting.
			f.waitStatus(ctx, t, sqlcgen.SandboxStatusBooting)

			// The agent is now cloning / running its hooks: no boot phase
			// reported, MarkBootComplete not called. Let several heartbeats
			// reach the actor; each is persisted in the same transaction
			// that would have moved the status.
			waitUntil(t, dispatchTestWait, func() bool {
				return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") >= 3
			})
			if got := f.status(ctx, t); got != sqlcgen.SandboxStatusBooting {
				t.Fatalf("sandbox status = %s after %d heartbeats of a boot that has not completed, want %s",
					got, countEvents(ctx, t, f.pool, f.sessionID, "heartbeat"), sqlcgen.SandboxStatusBooting)
			}

			// Boot finishes.
			bridge.MarkBootComplete()
			f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)
		})
	}
}

// TestBootReady_RealBridge_FastBootIsReadyAtOnce: a boot that completes
// well inside the first heartbeat interval, with no service and no
// boot_timing at all. ReportBootStarted's heartbeat -- sent as the boot
// starts, before the connection is even up here, as cmd/sandbox-agent
// does -- is its boot evidence, and MarkBootComplete's is the null phase:
// Ready follows at once, with the regular heartbeat still 10s away, and
// without the best-effort telemetry event the evidence must never depend
// on (§33.3).
func TestBootReady_RealBridge_FastBootIsReadyAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newBootReadyFixture(ctx, t)
	bridge := f.startBridge(t, bootReadyLongHeartbeat, (*wsbridge.Bridge).ReportBootStarted)

	f.waitStatus(ctx, t, sqlcgen.SandboxStatusBooting)
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") == 1
	})
	if got := f.status(ctx, t); got != sqlcgen.SandboxStatusBooting {
		t.Fatalf("status after ReportBootStarted's heartbeat = %s, want %s", got, sqlcgen.SandboxStatusBooting)
	}

	start := time.Now()
	bridge.MarkBootComplete()
	f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)
	if elapsed := time.Since(start); elapsed >= bootReadyLongHeartbeat {
		t.Errorf("Ready %s after MarkBootComplete, want well under the %s heartbeat interval", elapsed, bootReadyLongHeartbeat)
	}
	if n := countEvents(ctx, t, f.pool, f.sessionID, "boot_timing"); n != 0 {
		t.Errorf("boot_timing events = %d, want 0: this boot's evidence came from its heartbeats alone", n)
	}
}

// TestBootReady_RealBridge_ReconnectMidBoot: the connection drops in the
// middle of boot and the agent reconnects. The new connection's "ready"
// finds the sandbox already Booting (no transition), and its heartbeats
// still carry the boot phase: the sandbox stays Booting until the boot
// really completes.
func TestBootReady_RealBridge_ReconnectMidBoot(t *testing.T) {
	ctx := context.Background()
	f := newBootReadyFixture(ctx, t)
	bridge := f.startBridge(t, bootReadyShortHeartbeat, nil)

	f.waitStatus(ctx, t, sqlcgen.SandboxStatusBooting)
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") >= 2
	})

	f.severer.severAll()
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, f.pool, f.sessionID, "ready") == 2
	})
	afterReconnect := countEvents(ctx, t, f.pool, f.sessionID, "heartbeat")
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") >= afterReconnect+3
	})
	if got := f.status(ctx, t); got != sqlcgen.SandboxStatusBooting {
		t.Fatalf("status after a reconnect mid-boot = %s, want %s", got, sqlcgen.SandboxStatusBooting)
	}

	bridge.MarkBootComplete()
	f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)
}

// TestBootReady_PreFixAgentWire: a sandbox-agent built before the fix,
// against this control plane -- its exact frames, written by hand since
// that agent's Bridge no longer exists: "ready", then heartbeats with a
// null phase all through its clone and hooks, then, its boot sequence
// done, the boot_timing it has sent since §33.3. The null phases before
// that boot_timing leave the sandbox Booting; the first one after it
// marks it Ready. (An agent built from 2026-08-28 sends that boot_timing
// just before its final pass re-owning the workspace, so a null phase
// landing during that pass is read as completion too: the window §3.2
// states for it, which nothing on its wire lets the control plane
// close.)
func TestBootReady_PreFixAgentWire(t *testing.T) {
	ctx := context.Background()
	f := newBootReadyFixture(ctx, t)

	header := http.Header{}
	for k, v := range baseHeaders() {
		header.Set(k, v)
	}
	conn, _, err := websocket.Dial(ctx, f.wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	msgCh, waitReader := startReader(conn)

	sid := f.sessionID.String()
	send := func(t *testing.T, frame string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	nullHeartbeat := func(id string) string {
		return fmt.Sprintf(`{"type":"heartbeat","messageId":%q,"sessionId":%q,"gen":1,"conversationId":null,"lastBootPhase":null,"timestamp":%q}`,
			id, sid, time.Now().UTC().Format(time.RFC3339Nano))
	}

	send(t, fmt.Sprintf(`{"type":"ready","messageId":"r1","sessionId":%q,"gen":1,"agentVersion":"old","imageDigest":"old"}`, sid))
	f.waitStatus(ctx, t, sqlcgen.SandboxStatusBooting)

	for i := 1; i <= 3; i++ {
		send(t, nullHeartbeat(fmt.Sprintf("h-boot-%d", i)))
		waitUntil(t, dispatchTestWait, func() bool {
			return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") == i
		})
		if got := f.status(ctx, t); got != sqlcgen.SandboxStatusBooting {
			t.Fatalf("status after null-phase heartbeat %d, mid-boot = %s, want %s", i, got, sqlcgen.SandboxStatusBooting)
		}
	}

	send(t, fmt.Sprintf(`{"type":"boot_timing","messageId":"bt-1","sessionId":%q,"gen":1,"metric":"boot_duration","seconds":95.5,"bootMode":"fresh","failed":false}`, sid))
	waitUntil(t, dispatchTestWait, func() bool {
		return countEvents(ctx, t, f.pool, f.sessionID, "boot_timing") == 1
	})
	send(t, nullHeartbeat("h-after-boot"))
	f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)
	expectNoMessage(t, msgCh, 100*time.Millisecond) // nothing here is critical: no ack

	_ = conn.Close(websocket.StatusNormalClosure, "")
	if err := waitReader(); err != nil {
		t.Errorf("reader goroutine error = %v", err)
	}
}

// heartbeatPhases returns the lastBootPhase of every heartbeat stored for
// the fixture's session, in arrival order ("null" for a null phase).
func (f bootReadyFixture) heartbeatPhases(ctx context.Context, t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT COALESCE(payload->>'lastBootPhase', 'null') FROM events WHERE session_id = $1 AND type = 'heartbeat' ORDER BY id`, f.sessionID)
	if err != nil {
		t.Fatalf("query heartbeats: %v", err)
	}
	defer rows.Close()
	var phases []string
	for rows.Next() {
		var phase string
		if err := rows.Scan(&phase); err != nil {
			t.Fatalf("scan heartbeat: %v", err)
		}
		phases = append(phases, phase)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("heartbeats: %v", err)
	}
	return phases
}

// TestBootReady_RealBridge_BootDoneBeforeFirstConnection: the boot starts
// and completes before the agent's first connection is up -- its dial
// backing off while the control plane is briefly unreachable. The two
// forced heartbeats would coalesce into one null; instead the start phase
// still arrives first, so the sandbox has its boot evidence before its
// null phase with no boot_timing at all, and goes Ready at once.
func TestBootReady_RealBridge_BootDoneBeforeFirstConnection(t *testing.T) {
	ctx := context.Background()
	f := newBootReadyFixture(ctx, t)
	f.startBridge(t, bootReadyLongHeartbeat, func(b *wsbridge.Bridge) {
		b.ReportBootStarted()
		b.MarkBootComplete()
	})

	f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)
	row := getSandbox(ctx, t, f.pool, f.sessionID)
	if row.BootEvidenceGen == nil || *row.BootEvidenceGen != 1 {
		t.Errorf("boot_evidence_gen = %v, want 1: the start phase is this boot's evidence", row.BootEvidenceGen)
	}
	if got, want := strings.Join(f.heartbeatPhases(ctx, t), ","), wsbridge.InitialBootPhase+",null"; got != want {
		t.Errorf("heartbeat phases = %s, want %s", got, want)
	}
	if n := countEvents(ctx, t, f.pool, f.sessionID, "boot_timing"); n != 0 {
		t.Errorf("boot_timing events = %d, want 0: the evidence must not depend on it", n)
	}
}

// TestBootReady_RealBridge_FixedAgentNeverNeedsTheFallback pins that §3.2's
// boot-evidence fallback is reachable only by an agent built before the
// fix. The control plane here accepts any null phase from a gen with no
// evidence after 1ms of Booting, so a fixed agent that ever sent a null
// phase without evidence first would go Ready through the fallback -- mid
// boot, or with boot_evidence_gen still NULL. Through a boot with
// heartbeats flowing, a reconnect in the middle of it, and a boot that
// completes before the first connection, it never does: it is Booting
// until MarkBootComplete, and Ready on evidence after.
func TestBootReady_RealBridge_FixedAgentNeverNeedsTheFallback(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		// completeBeforeRun completes the boot before the first
		// connection; otherwise the test holds it open, heartbeats
		// flowing, and completes it itself.
		completeBeforeRun bool
		// reconnectMidBoot drops the connection while the boot is open.
		reconnectMidBoot bool
	}{
		{name: "heartbeats flowing through the boot"},
		{name: "a reconnect mid-boot", reconnectMidBoot: true},
		{name: "boot completed before the first connection", completeBeforeRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootReadyFixtureWithTimeouts(ctx, t, func(to *platform.Timeouts) {
				to.BootEvidenceFallback = time.Millisecond
			})
			bridge := f.startBridge(t, bootReadyShortHeartbeat, func(b *wsbridge.Bridge) {
				b.ReportBootStarted()
				if tc.completeBeforeRun {
					b.MarkBootComplete()
				}
			})

			if !tc.completeBeforeRun {
				f.waitStatus(ctx, t, sqlcgen.SandboxStatusBooting)
				if tc.reconnectMidBoot {
					waitUntil(t, dispatchTestWait, func() bool {
						return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") >= 2
					})
					f.severer.severAll()
					waitUntil(t, dispatchTestWait, func() bool {
						return countEvents(ctx, t, f.pool, f.sessionID, "ready") == 2
					})
				}
				seen := countEvents(ctx, t, f.pool, f.sessionID, "heartbeat")
				waitUntil(t, dispatchTestWait, func() bool {
					return countEvents(ctx, t, f.pool, f.sessionID, "heartbeat") >= seen+3
				})
				if got := f.status(ctx, t); got != sqlcgen.SandboxStatusBooting {
					t.Fatalf("status mid-boot = %s, want %s: the fallback must never apply to a fixed agent", got, sqlcgen.SandboxStatusBooting)
				}
				bridge.MarkBootComplete()
			}

			f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)
			row := getSandbox(ctx, t, f.pool, f.sessionID)
			if row.BootEvidenceGen == nil || *row.BootEvidenceGen != 1 {
				t.Errorf("boot_evidence_gen at Ready = %v, want 1: Ready on evidence, not through the fallback", row.BootEvidenceGen)
			}
			phases := f.heartbeatPhases(ctx, t)
			for i, phase := range phases {
				if phase == "null" {
					if i == 0 {
						t.Errorf("heartbeat phases = %v: a null phase came first", phases)
					}
					break
				}
			}
		})
	}
}

// swallowingProxy is a TCP proxy between a real Bridge and the sandbox
// handler that loses frames the agent wrote successfully -- as a dropped
// connection, a network partition or a control-plane pod dying before its
// read loop gets to them all do. On its first connection it forwards the
// agent's HTTP upgrade and its first WebSocket frame, "ready", then reads
// and discards every frame after it, and closes both sides once it has
// discarded a null-phase heartbeat: by then the agent has written, without
// error, both its boot's start phase and a null phase on that connection,
// and neither has reached the control plane. Every later connection is
// forwarded unchanged, both ways. What reaches the agent is always
// forwarded.
type swallowingProxy struct {
	listener net.Listener
	upstream string

	mu        sync.Mutex
	accepted  int
	conns     []net.Conn
	swallowed []string

	group errgroup.Group
}

// newSwallowingProxy starts a swallowingProxy in front of the handler
// wsURL names, and returns it with the URL to dial it at instead. It is
// closed at test cleanup.
func newSwallowingProxy(t *testing.T, wsURL string) (*swallowingProxy, string) {
	t.Helper()
	target, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parse %q: %v", wsURL, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &swallowingProxy{listener: listener, upstream: target.Host}
	p.group.Go(p.accept)
	t.Cleanup(func() {
		_ = listener.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		_ = p.group.Wait()
	})
	proxied := *target
	proxied.Host = listener.Addr().String()
	return p, proxied.String()
}

func (p *swallowingProxy) accept() error {
	for {
		agent, err := p.listener.Accept()
		if err != nil {
			return nil // closed at cleanup
		}
		handler, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = agent.Close()
			continue
		}
		p.mu.Lock()
		p.accepted++
		first := p.accepted == 1
		p.conns = append(p.conns, agent, handler)
		p.mu.Unlock()

		p.group.Go(func() error {
			_, _ = io.Copy(agent, handler)
			_ = agent.Close()
			return nil
		})
		p.group.Go(func() error {
			if first {
				p.forwardReadyThenSwallow(agent, handler)
			} else {
				_, _ = io.Copy(handler, agent)
			}
			_ = handler.Close()
			_ = agent.Close()
			return nil
		})
	}
}

// forwardReadyThenSwallow forwards the agent's HTTP upgrade and its first
// frame, then discards its frames, recording each, until a null-phase
// heartbeat has been discarded.
func (p *swallowingProxy) forwardReadyThenSwallow(agent, handler net.Conn) {
	r := bufio.NewReader(agent)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if _, err := io.WriteString(handler, line); err != nil {
			return
		}
		if line == "\r\n" {
			break
		}
	}
	raw, _, err := readClientFrame(r)
	if err != nil {
		return
	}
	if _, err := handler.Write(raw); err != nil {
		return
	}
	for {
		_, payload, err := readClientFrame(r)
		if err != nil {
			return
		}
		var env struct {
			Type          string  `json:"type"`
			LastBootPhase *string `json:"lastBootPhase"`
			Metric        string  `json:"metric"`
		}
		_ = json.Unmarshal(payload, &env)
		kind := env.Type
		switch env.Type {
		case "heartbeat":
			kind += ":" + phaseOrNull(env.LastBootPhase)
		case "boot_timing":
			kind += ":" + env.Metric
		}
		p.mu.Lock()
		p.swallowed = append(p.swallowed, kind)
		p.mu.Unlock()
		if env.Type == "heartbeat" && env.LastBootPhase == nil {
			return
		}
	}
}

// swallowedFrames returns the kinds of the frames discarded so far.
func (p *swallowingProxy) swallowedFrames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.swallowed...)
}

// readClientFrame reads one WebSocket frame as a client writes it (RFC
// 6455 §5.2: masked, and never compressed here -- the Bridge negotiates no
// extension), returning its bytes as read and its unmasked payload.
func readClientFrame(r *bufio.Reader) (raw, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, nil, err
	}
	raw = append(raw, header...)
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, nil, err
		}
		raw = append(raw, ext...)
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, nil, err
		}
		raw = append(raw, ext...)
		length = binary.BigEndian.Uint64(ext)
	}
	var key []byte
	if header[1]&0x80 != 0 {
		key = make([]byte, 4)
		if _, err := io.ReadFull(r, key); err != nil {
			return nil, nil, err
		}
		raw = append(raw, key...)
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, nil, err
	}
	raw = append(raw, payload...)
	for i := range payload {
		if key != nil {
			payload[i] ^= key[i%4]
		}
	}
	return raw, payload, nil
}

func phaseOrNull(phase *string) string {
	if phase == nil {
		return "null"
	}
	return *phase
}

// storedKinds returns every event stored for the fixture's session, in
// arrival order: "ready", "heartbeat:<phase>" ("null" for a null phase),
// "boot_timing:<metric>", or the bare type for anything else.
func (f bootReadyFixture) storedKinds(ctx context.Context, t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT type, COALESCE(payload->>'lastBootPhase', 'null'), COALESCE(payload->>'metric', '')
		FROM events WHERE session_id = $1 ORDER BY id`, f.sessionID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var typ, phase, metric string
		if err := rows.Scan(&typ, &phase, &metric); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		switch typ {
		case "heartbeat":
			typ += ":" + phase
		case "boot_timing":
			typ += ":" + metric
		}
		kinds = append(kinds, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("events: %v", err)
	}
	return kinds
}

// upToFirstNull returns kinds up to and including its first null-phase
// heartbeat, or all of kinds when it has none.
func upToFirstNull(kinds []string) []string {
	if i := slices.Index(kinds, "heartbeat:null"); i >= 0 {
		return kinds[:i+1]
	}
	return kinds
}

// TestBootReady_RealBridge_StartPhaseLostInFlight: the connection the
// agent's boot evidence went out on is lost with it in flight. The boot
// completed before the first connection came up, in cmd/sandbox-agent's
// order -- ReportBootStarted, the boot's boot_duration (buffered, since
// nothing is connected yet), MarkBootComplete -- and a proxy loses
// everything the agent writes on that connection after "ready": its start
// phase, the boot_timing and its null phase, all written without error.
// The next connection must carry the start phase again, ahead of its first
// null, and that heartbeat must be the evidence the sandbox goes Ready on
// -- ahead of the replayed boot_timing when there is one, and alone when
// there is none: best-effort telemetry whose loss must never fail a boot
// (§33.3), and without which the sandbox would stay Booting for the
// fallback's whole hour. Heartbeats are never acknowledged, so an agent
// that took a successful write of its start phase as delivered sent only
// null phases on the next connection.
func TestBootReady_RealBridge_StartPhaseLostInFlight(t *testing.T) {
	ctx := context.Background()
	start := "heartbeat:" + wsbridge.InitialBootPhase

	for _, tc := range []struct {
		name          string
		bootTiming    bool
		wantSwallowed []string
		wantStored    []string
	}{
		{
			name:          "boot_timing buffered",
			bootTiming:    true,
			wantSwallowed: []string{start, "boot_timing:boot_duration", "heartbeat:null"},
			wantStored:    []string{"ready", "ready", start, "boot_timing:boot_duration", "heartbeat:null"},
		},
		{
			name:          "no boot_timing",
			wantSwallowed: []string{start, "heartbeat:null"},
			wantStored:    []string{"ready", "ready", start, "heartbeat:null"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootReadyFixture(ctx, t)
			proxy, proxiedURL := newSwallowingProxy(t, f.wsURL)
			f.startBridgeAt(t, proxiedURL, bootReadyShortHeartbeat, func(b *wsbridge.Bridge) {
				b.ReportBootStarted()
				if tc.bootTiming {
					mode, failed := "fresh", false
					if err := b.SendBestEffort(ctx, sandboxws.BootTiming{
						Type:      "boot_timing",
						MessageId: "bt-boot-duration",
						SessionId: f.sessionID.String(),
						Gen:       1,
						Metric:    sandboxws.BootTimingMetricBootDuration,
						Seconds:   1.5,
						BootMode:  &mode,
						Failed:    &failed,
					}); err != nil {
						t.Fatalf("SendBestEffort(boot_timing): %v", err)
					}
				}
				b.MarkBootComplete()
			})

			f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)

			// Connection 1 did carry the start phase, then a null phase,
			// and the control plane got neither.
			if got := proxy.swallowedFrames(); !slices.Equal(got, tc.wantSwallowed) {
				t.Fatalf("frames lost on connection 1 = %v, want %v", got, tc.wantSwallowed)
			}
			// Connection 2 carried the start phase ahead of its first null,
			// and it was the first evidence the control plane saw.
			if got := upToFirstNull(f.storedKinds(ctx, t)); !slices.Equal(got, tc.wantStored) {
				t.Errorf("stored events up to the first null phase = %v, want %v", got, tc.wantStored)
			}
			row := getSandbox(ctx, t, f.pool, f.sessionID)
			if row.BootEvidenceGen == nil || *row.BootEvidenceGen != 1 {
				t.Errorf("boot_evidence_gen at Ready = %v, want 1", row.BootEvidenceGen)
			}
		})
	}
}

// TestBootReady_RealBridge_StartPhaseAgainAfterReadyIsHarmless: every
// connection carries the boot's start phase ahead of its first null, so a
// sandbox that went Ready on its first connection hears it again on the
// next one. The control plane takes it in its stride -- evidence is
// recorded once per generation and a non-null phase moves no sandbox -- so
// the sandbox stays Ready throughout the reconnect.
func TestBootReady_RealBridge_StartPhaseAgainAfterReadyIsHarmless(t *testing.T) {
	ctx := context.Background()
	start := "heartbeat:" + wsbridge.InitialBootPhase
	f := newBootReadyFixture(ctx, t)
	f.startBridge(t, bootReadyShortHeartbeat, func(b *wsbridge.Bridge) {
		b.ReportBootStarted()
		b.MarkBootComplete()
	})
	f.waitStatus(ctx, t, sqlcgen.SandboxStatusReady)

	f.severer.severAll()
	// Wait for connection 2's first null phase, checking the status on
	// every poll: Ready, never back to Booting.
	var second int
	waitUntil(t, dispatchTestWait, func() bool {
		if got := f.status(ctx, t); got != sqlcgen.SandboxStatusReady {
			t.Fatalf("status during the reconnect = %s, want %s", got, sqlcgen.SandboxStatusReady)
		}
		kinds := f.storedKinds(ctx, t)
		second = slices.Index(kinds[1:], "ready") + 1
		return second > 0 && slices.Contains(kinds[second:], "heartbeat:null")
	})

	kinds := f.storedKinds(ctx, t)
	for _, conn := range []struct {
		name   string
		events []string
	}{
		{"connection 1", kinds[:second]},
		{"connection 2", kinds[second:]},
	} {
		if got, want := upToFirstNull(conn.events), []string{"ready", start, "heartbeat:null"}; !slices.Equal(got, want) {
			t.Errorf("%s's events up to its first null phase = %v, want %v", conn.name, got, want)
		}
	}
	if got := f.status(ctx, t); got != sqlcgen.SandboxStatusReady {
		t.Errorf("status after connection 2's start phase = %s, want %s", got, sqlcgen.SandboxStatusReady)
	}
	row := getSandbox(ctx, t, f.pool, f.sessionID)
	if row.BootEvidenceGen == nil || *row.BootEvidenceGen != 1 {
		t.Errorf("boot_evidence_gen = %v, want 1", row.BootEvidenceGen)
	}
}

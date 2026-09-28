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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createTestSandbox(ctx, t, pool, sessionID) // gen 1, Pending
	moveSandboxStatus(ctx, t, pool, sessionID, sqlcgen.SandboxStatusConnecting)

	timeouts := platform.DefaultTimeouts()
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
	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: f.wsURL,
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
// null phase all through its clone and hooks, then, its boot done, the
// boot_timing it has sent since §33.3. The null phases before that
// boot_timing leave the sandbox Booting; the first one after it marks it
// Ready.
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

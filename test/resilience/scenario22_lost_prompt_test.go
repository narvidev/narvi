//go:build integration

// Resilience scenario #22 (§9.3, docs/IMPLEMENTATION_PLAN.md row 226): "A
// prompt lost between dispatch and the sandbox -> the same-gen reconnect
// re-sends it once to a capable agent, which runs it once; never re-sent
// to an agent without the capability, whose turn ends at turn_deadline."
//
// The control plane commits a turn Processing, then writes its prompt in
// one frame nothing acknowledges. Here that frame is lost with its socket
// after the write returned: scenario #7's wsProxy, between a real
// wsbridge.Bridge and the real wshub sandbox handler, drops the first
// prompt it relays backend->client and severs both sides. The sandbox then
// reconnects on the same gen. A capable agent -- a real Bridge with its
// prompt journal open (EnablePromptReceipts) -- is sent the prompt again
// with the same messageId, runs it once and completes the turn; when what
// is lost is its receipt instead, the copy it is re-sent is answered with
// a duplicate receipt and not run again. An agent built before receipts --
// its frames written by hand, as TestBootReady_PreFixAgentWire writes them
// (internal/adapters/inbound/wshub) -- advertises nothing, is sent nothing
// more, and its turn ends at turn_deadline, as every lost prompt did
// before (technical plan §3.3, prompt receipts).
package resilience_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const scenario22Wait = 10 * time.Second

// lostPromptRig is one session on the real control-plane stack: a
// registry with a real wshub commander, the real sandbox WS handler behind
// a test server, and a proxy in front of it -- the sandbox's only way in.
type lostPromptRig struct {
	h         *Harness
	registry  *sessionactor.Registry
	sessionID pgtype.UUID
	turnID    pgtype.UUID
	proxy     *wsProxy
	proxyURL  string

	mu       sync.Mutex
	relayed  [][]byte // client->backend frames the proxy relayed
	received []sandboxws.PromptReceived
}

// newLostPromptRig seeds a Ready sandbox at gen 1 and a pending turn, and
// scripts the proxy's first connection with first.
func newLostPromptRig(ctx context.Context, t *testing.T, timeouts platform.Timeouts, first wsProxyStep) *lostPromptRig {
	return newLostPromptRigWithPrompt(ctx, t, timeouts, first, "scenario22 turn")
}

// newLostPromptRigWithPrompt is newLostPromptRig with the turn's text.
func newLostPromptRigWithPrompt(ctx context.Context, t *testing.T, timeouts platform.Timeouts, first wsProxyStep, prompt string) *lostPromptRig {
	t.Helper()
	h := newHarness(t)
	h.Timeouts = timeouts
	commander := wshub.NewSandboxRegistry(h.Timeouts)
	registry := h.NewRegistryWithCommander(ctx, t, commander)
	t.Cleanup(func() { _ = registry.Shutdown() })

	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, h.Sandboxes, commander, h.Timeouts))
	backend := httptest.NewServer(router)
	t.Cleanup(backend.Close)

	rig := &lostPromptRig{h: h, registry: registry, sessionID: h.CreateSession(ctx, t)}
	if _, err := h.Sandboxes.Create(ctx, rig.sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := h.Sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	created, err := h.Turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: rig.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	rig.turnID = created.ID

	rig.proxy = newWSProxy("ws"+strings.TrimPrefix(backend.URL, "http"), func(payload []byte) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.relayed = append(rig.relayed, append([]byte(nil), payload...))
		if relayType(payload) == "prompt_received" {
			var r sandboxws.PromptReceived
			if err := json.Unmarshal(payload, &r); err == nil {
				rig.received = append(rig.received, r)
			}
		}
	})
	rig.proxy.pushStep(first)
	proxyServer := httptest.NewServer(rig.proxy)
	t.Cleanup(proxyServer.Close)
	rig.proxyURL = "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/sessions/" + rig.sessionID.String() + "/ws?type=sandbox"
	return rig
}

func (r *lostPromptRig) receipts() []sandboxws.PromptReceived {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sandboxws.PromptReceived(nil), r.received...)
}

func (r *lostPromptRig) readyCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, payload := range r.relayed {
		if relayType(payload) == "ready" {
			n++
		}
	}
	return n
}

func (r *lostPromptRig) turn(ctx context.Context, t *testing.T) sqlcgen.Turn {
	t.Helper()
	got, err := r.h.Turns.Get(ctx, r.turnID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return got
}

func (r *lostPromptRig) executionCompleteRows(ctx context.Context, t *testing.T) (all, synthetic int) {
	t.Helper()
	if err := r.h.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE payload ? 'synthetic') FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		r.sessionID).Scan(&all, &synthetic); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	return all, synthetic
}

// runningAgent is a capable agent's CommandHandler: it counts each
// prompt's runs and ends each run with an execution_complete, as the
// agent runtime does.
type runningAgent struct {
	noopCommandHandler
	ctx    context.Context
	bridge *wsbridge.Bridge

	mu   sync.Mutex
	runs map[string]int
}

func (a *runningAgent) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	a.mu.Lock()
	a.runs[cmd.MessageId]++
	a.mu.Unlock()
	id := "ec-" + cmd.MessageId
	_ = a.bridge.SendCritical(a.ctx, sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	}, "execution_complete:"+id)
}

func (a *runningAgent) runCounts() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.runs))
	for k, v := range a.runs {
		out[k] = v
	}
	return out
}

// startCapableAgent runs a real Bridge, its prompt journal open, against
// the rig's proxy.
func startCapableAgent(ctx context.Context, t *testing.T, rig *lostPromptRig) *runningAgent {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	agent := &runningAgent{ctx: runCtx, runs: map[string]int{}}
	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: rig.proxyURL,
		Gen:               1,
		SandboxToken:      "scenario22-test-token", // the fresh row's NULL token_hash admits any (scenario #7's note)
		SessionId:         rig.sessionID.String(),
	}
	bridge := wsbridge.New(sc, "sbx-scenario22", "test-agent-version", "test-image-digest", agent,
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)
	agent.bridge = bridge
	if err := bridge.EnablePromptReceipts(t.TempDir()); err != nil {
		t.Fatalf("EnablePromptReceipts: %v", err)
	}
	var group errgroup.Group
	group.Go(func() error { return bridge.Run(runCtx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("bridge.Run() error = %v, want nil after ctx cancellation", err)
		}
	})
	return agent
}

func TestResilience_Scenario22_LostPromptFrame_CapableAgent_DeliveredOnceAndCompletes(t *testing.T) {
	ctx := context.Background()
	rig := newLostPromptRig(ctx, t, platform.DefaultTimeouts(), wsProxyStep{dropBackendType: "prompt"})
	agent := startCapableAgent(ctx, t, rig)

	waitUntil(t, scenario22Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })

	got := rig.turn(ctx, t)
	if got.DispatchedMessageID == nil {
		t.Fatal("turn has no dispatched_message_id")
	}
	messageID := *got.DispatchedMessageID
	if runs := agent.runCounts(); len(runs) != 1 || runs[messageID] != 1 {
		t.Fatalf("prompt runs = %v, want %q run exactly once", runs, messageID)
	}
	if got := rig.readyCount(); got < 2 {
		t.Fatalf("%d readies relayed, want at least 2: the lost frame's socket was severed and the agent reconnected", got)
	}
	receipts := rig.receipts()
	if len(receipts) == 0 || receipts[0].PromptMessageId != messageID || receipts[0].Duplicate {
		t.Fatalf("receipts relayed = %+v, want the re-sent prompt's, not a duplicate: the first copy never arrived", receipts)
	}
	if n := countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "prompt_received:"+messageID); n != 1 {
		t.Fatalf("%d stored receipts for %q, want 1", n, messageID)
	}
	if all, synthetic := rig.executionCompleteRows(ctx, t); all != 1 || synthetic != 0 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want the agent's one", all, synthetic)
	}
}

func TestResilience_Scenario22_LostReceiptFrame_CapableAgent_RunsOnce(t *testing.T) {
	ctx := context.Background()
	rig := newLostPromptRig(ctx, t, platform.DefaultTimeouts(), wsProxyStep{dropClientType: "prompt_received"})
	agent := startCapableAgent(ctx, t, rig)

	waitUntil(t, scenario22Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	messageID := *rig.turn(ctx, t).DispatchedMessageID

	// The re-sent copy is answered as a duplicate, and not run again.
	waitUntil(t, scenario22Wait, func() bool {
		for _, r := range rig.receipts() {
			if r.PromptMessageId == messageID && r.Duplicate {
				return true
			}
		}
		return false
	})
	if runs := agent.runCounts(); len(runs) != 1 || runs[messageID] != 1 {
		t.Fatalf("prompt runs = %v, want %q run exactly once though it was received twice", runs, messageID)
	}
	if all, synthetic := rig.executionCompleteRows(ctx, t); all != 1 || synthetic != 0 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want the agent's one", all, synthetic)
	}
}

// preChangeAgentConn is one connection of an agent built before prompt
// receipts, as it writes and reads the wire: a ready with no
// capabilities, heartbeats, and prompts counted -- it dedups nothing.
type preChangeAgentConn struct {
	conn  *websocket.Conn
	group errgroup.Group
}

// dialPreChangeAgent connects through url, sends the ready, and counts
// every prompt frame the connection reads into prompts.
func dialPreChangeAgent(ctx context.Context, t *testing.T, url string, sessionID string, prompts *promptTally) *preChangeAgentConn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer scenario22-test-token")
	header.Set("X-Sandbox-ID", "sbx-scenario22-old")
	header.Set("X-Sandbox-Gen", "1")
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c := &preChangeAgentConn{conn: conn}
	c.group.Go(func() error {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return nil
			}
			if relayType(data) == "prompt" {
				var p struct {
					MessageID string `json:"messageId"`
				}
				_ = json.Unmarshal(data, &p)
				prompts.add(p.MessageID)
			}
		}
	})
	c.write(ctx, t, fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":%q,"gen":1,"timestamp":%q,"agentVersion":"old","imageDigest":"old"}`,
		"r-"+time.Now().Format(time.RFC3339Nano), sessionID, time.Now().UTC().Format(time.RFC3339Nano)))
	return c
}

func (c *preChangeAgentConn) write(ctx context.Context, t *testing.T, frame string) {
	t.Helper()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func (c *preChangeAgentConn) close() {
	_ = c.conn.CloseNow()
	_ = c.group.Wait()
}

type promptTally struct {
	mu sync.Mutex
	n  map[string]int
}

func (p *promptTally) add(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n[id]++
}

func (p *promptTally) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, n := range p.n {
		total += n
	}
	return total
}

func TestResilience_Scenario22_LostPromptFrame_PreChangeAgent_NeverResent_EndsAtTurnDeadline(t *testing.T) {
	ctx := context.Background()
	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 3 * time.Second // injected; the lost prompt's turn ends here
	rig := newLostPromptRig(ctx, t, timeouts, wsProxyStep{dropBackendType: "prompt"})
	sid := rig.sessionID.String()
	prompts := &promptTally{n: map[string]int{}}

	// First connection: the ready dispatches the turn, whose prompt frame
	// the proxy drops, severing the connection.
	first := dialPreChangeAgent(ctx, t, rig.proxyURL, sid, prompts)
	waitUntil(t, scenario22Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusProcessing })
	_ = first.group.Wait()
	first.close()

	// The same gen reconnects: its ready, then three heartbeats.
	second := dialPreChangeAgent(ctx, t, rig.proxyURL, sid, prompts)
	t.Cleanup(second.close)
	for i := 0; i < 3; i++ {
		second.write(ctx, t, fmt.Sprintf(`{"type":"heartbeat","messageId":"h-%d","sessionId":%q,"gen":1,"conversationId":null,"lastBootPhase":null,"timestamp":%q}`,
			i, sid, time.Now().UTC().Format(time.RFC3339Nano)))
	}
	waitUntil(t, scenario22Wait, func() bool {
		var n int
		if err := rig.h.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'heartbeat'`, rig.sessionID).Scan(&n); err != nil {
			t.Fatalf("count heartbeats: %v", err)
		}
		return n == 3
	})
	if got := prompts.total(); got != 0 {
		t.Fatalf("the agent without the capability received %d prompts, want 0: never re-sent", got)
	}

	// Nothing ends the turn but its deadline.
	waitUntil(t, scenario22Wait+timeouts.TurnDeadline, func() bool {
		if err := rig.registry.PumpOnce(ctx); err != nil {
			t.Logf("PumpOnce: %v (retrying)", err)
			return false
		}
		return rig.turn(ctx, t).Status == sqlcgen.TurnStatusFailed
	})
	if all, synthetic := rig.executionCompleteRows(ctx, t); all != 1 || synthetic != 1 {
		t.Fatalf("execution_complete events = %d (%d synthetic), want one synthetic: the turn timed out", all, synthetic)
	}
	if got := prompts.total(); got != 0 {
		t.Fatalf("the agent received %d prompts in all, want 0", got)
	}
}

// TestResilience_Scenario22_PromptFrameOver32KiB_CapableAgent_DeliveredOnceAndCompletes:
// a prompt frame longer than the WebSocket library's default read limit
// (32 KiB) -- a review's, with its diff inlined -- reaches a capable agent
// whole, on the connection it was sent on, and runs once. Before the agent
// read up to platform.MaxPromptFrameBytes, that frame closed its
// connection and was lost; with receipts, every reconnect re-sent it, to
// be lost again.
func TestResilience_Scenario22_PromptFrameOver32KiB_CapableAgent_DeliveredOnceAndCompletes(t *testing.T) {
	ctx := context.Background()
	rig := newLostPromptRigWithPrompt(ctx, t, platform.DefaultTimeouts(), wsProxyStep{}, strings.Repeat("diff --git a/x b/x\n+<line>\n", 40*1024/25+1))
	agent := startCapableAgent(ctx, t, rig)

	waitUntil(t, scenario22Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	messageID := *rig.turn(ctx, t).DispatchedMessageID
	if runs := agent.runCounts(); len(runs) != 1 || runs[messageID] != 1 {
		t.Fatalf("prompt runs = %v, want %q run exactly once", runs, messageID)
	}
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1: the frame was read on the connection it came on", got)
	}
	var frameBytes int
	if err := rig.h.Pool.QueryRow(ctx, `SELECT octet_length(prompt) FROM turns WHERE id = $1`, rig.turnID).Scan(&frameBytes); err != nil {
		t.Fatal(err)
	}
	if frameBytes <= 40*1024 {
		t.Fatalf("prompt is %d bytes, want over 40 KiB", frameBytes)
	}
}

// TestResilience_Scenario22_PromptLostOnEveryDelivery_ResendCapStopsTheLoop:
// a prompt the sandbox can never take -- every copy dropped with its
// socket, each loss a reconnect -- is sent once and re-sent
// PromptResendMaxPerTurn times, and then the reconnects stop: the next
// ready is answered with nothing, so the connection stays up, and the turn
// waits for its deadline.
func TestResilience_Scenario22_PromptLostOnEveryDelivery_ResendCapStopsTheLoop(t *testing.T) {
	ctx := context.Background()
	timeouts := platform.DefaultTimeouts()
	rig := newLostPromptRig(ctx, t, timeouts, wsProxyStep{})
	rig.proxy.mu.Lock()
	rig.proxy.alwaysDropBackendType = "prompt"
	rig.proxy.mu.Unlock()
	agent := startCapableAgent(ctx, t, rig)

	want := 1 + timeouts.PromptResendMaxPerTurn
	waitUntil(t, scenario22Wait, func() bool { return rig.proxy.droppedCount() >= want && rig.readyCount() >= want+1 })
	// Settled: no further copy, no further reconnect.
	time.Sleep(time.Second)
	if got := rig.proxy.droppedCount(); got != want {
		t.Fatalf("%d prompt copies sent and lost, want %d: the dispatch and PromptResendMaxPerTurn re-sends", got, want)
	}
	if got := rig.readyCount(); got != want+1 {
		t.Fatalf("%d readies, want %d: the reconnects stop once nothing more is sent", got, want+1)
	}
	if runs := agent.runCounts(); len(runs) != 0 {
		t.Fatalf("prompt runs = %v, want none: every copy was lost", runs)
	}
	got := rig.turn(ctx, t)
	if got.Status != sqlcgen.TurnStatusProcessing || got.ReceiptResendCount != int32(timeouts.PromptResendMaxPerTurn) {
		t.Fatalf("turn status %s, receipt_resend_count %d; want processing, %d", got.Status, got.ReceiptResendCount, timeouts.PromptResendMaxPerTurn)
	}
}

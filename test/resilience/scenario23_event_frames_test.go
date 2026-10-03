//go:build integration

// Resilience scenario #23, the event direction (technical plan §6.1;
// docs/IMPLEMENTATION_PLAN.md row 228): "An agent event over 32 KiB → read
// on the connection it came on by a control plane that states a larger
// limit, written cut to one that states none, never a reconnect loop."
//
// The control plane read the sandbox socket at the WebSocket library's
// default, 32 KiB, and nothing bounded what an agent wrote: an event over
// it -- a text part, a tool's input or output, git push's stderr -- closed
// the connection, and the agent, which replays its buffer on every
// reconnect, closed each one the same way, about 320 times a second, with
// nothing behind the frame ever arriving. Now the control plane reads up
// to platform.MaxEventFrameBytes and states it in the handshake
// (platform.MaxFrameBytesHeader); the agent holds every write to what the
// handshake states, or to 32 KiB when it states nothing -- a control plane
// built before the header, or a proxy that drops it -- cutting a `token`,
// `tool_call` or `tool_result` that is over it, and marking the cut in the
// frame's `cut` property.
//
// Every test here runs a real wsbridge.Bridge, or an agent written by hand
// as scenario #22's are, against the real wshub sandbox handler and
// session actor on Postgres, behind scenario #7's relay. The relay plays
// both kinds of control plane: forwarding the backend's header, it is one
// built with the header; dropping it and reading at 32 KiB
// (clientReadLimit), it is one built before. The `tool_result` cases use
// the pinned runtime's id shape, the enclosing message's id behind that
// message's `step_start`, so a `tool_result` adds no row today (technical
// plan §6.1, "Stored token frames"): they assert what the wire carries.
package resilience_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/framecut"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const scenario23Wait = 15 * time.Second

// noHeaderControlPlane is the relay as a control plane built before the
// header: it states nothing, and reads at the library's default.
var noHeaderControlPlane = wsProxyStep{clientReadLimit: platform.DefaultFrameReadLimitBytes}

// headerControlPlane is the relay as a control plane built with the
// header: it states what the real handler states.
var headerControlPlane = wsProxyStep{forwardMaxFrameHeader: true}

// eventFrameRig is scenario #22's rig with what the event direction needs:
// a plan-mode turn, a session with a repo to push, and the frames the relay
// passes back to the agent.
type eventFrameRig struct {
	*lostPromptRig

	backendMu sync.Mutex
	backend   [][]byte // backend->client frames the relay passed on
}

type eventRigOptions struct {
	first, fallback wsProxyStep
	prompt          string
	planMode        bool
	repos           []byte
}

// newEventFrameRig seeds a Ready sandbox at gen 1 and a pending turn, and
// puts the relay in front of the real handler: first scripts its first
// connection, fallback every connection after.
func newEventFrameRig(ctx context.Context, t *testing.T, opts eventRigOptions) *eventFrameRig {
	t.Helper()
	h := newHarness(t)
	commander := wshub.NewSandboxRegistry(h.Timeouts)
	registry := h.NewRegistryWithCommander(ctx, t, commander)
	t.Cleanup(func() { _ = registry.Shutdown() })

	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, h.Sandboxes, commander, h.Timeouts))
	backend := httptest.NewServer(router)
	t.Cleanup(backend.Close)

	session, err := h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, Repos: opts.repos})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := h.Sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := h.Sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: session.ID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
	prompt := opts.prompt
	if prompt == "" {
		prompt = "scenario23 turn"
	}
	created, err := h.Turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, PlanMode: opts.planMode})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	rig := &eventFrameRig{lostPromptRig: &lostPromptRig{h: h, registry: registry, sessionID: session.ID, turnID: created.ID}}
	rig.proxy = newWSProxy("ws"+strings.TrimPrefix(backend.URL, "http"), func(payload []byte) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.relayed = append(rig.relayed, append([]byte(nil), payload...))
	})
	rig.proxy.onBackend = func(payload []byte) {
		rig.backendMu.Lock()
		defer rig.backendMu.Unlock()
		rig.backend = append(rig.backend, append([]byte(nil), payload...))
	}
	rig.proxy.pushStep(opts.first)
	rig.proxy.setFallback(opts.fallback)
	proxyServer := httptest.NewServer(rig.proxy)
	t.Cleanup(proxyServer.Close)
	rig.proxyURL = "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/sessions/" + session.ID.String() + "/ws?type=sandbox"
	return rig
}

// relayedFrames returns the client->backend frames of type frameType the
// relay passed on, in order ("" for every frame).
func (r *eventFrameRig) relayedFrames(frameType string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for _, payload := range r.relayed {
		if frameType == "" || relayType(payload) == frameType {
			out = append(out, payload)
		}
	}
	return out
}

// ackRelayed reports whether the relay passed an ack for ackID back to the
// agent.
func (r *eventFrameRig) ackRelayed(ackID string) bool {
	r.backendMu.Lock()
	defer r.backendMu.Unlock()
	for _, payload := range r.backend {
		var ack sandboxws.Ack
		if relayType(payload) == "ack" && json.Unmarshal(payload, &ack) == nil && ack.AckId == ackID {
			return true
		}
	}
	return false
}

// storedToken is one stored `token` row of a part.
type storedToken struct {
	key  string
	text string
	cut  *framecut.Cut
	raw  []byte
}

// tokenRows returns the stored `token` rows of part, in id order.
func (r *eventFrameRig) tokenRows(ctx context.Context, t *testing.T, part string) []storedToken {
	t.Helper()
	rows, err := r.h.Pool.Query(ctx,
		`SELECT message_id, payload FROM events WHERE session_id = $1 AND type = 'token' AND payload->>'messageId' = $2 ORDER BY id`,
		r.sessionID, part)
	if err != nil {
		t.Fatalf("query token rows: %v", err)
	}
	type raw struct {
		key     string
		payload []byte
	}
	var scanned []raw
	for rows.Next() {
		var row raw
		if err := rows.Scan(&row.key, &row.payload); err != nil {
			rows.Close()
			t.Fatalf("scan token row: %v", err)
		}
		scanned = append(scanned, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate token rows: %v", err)
	}
	out := make([]storedToken, 0, len(scanned))
	for _, row := range scanned {
		var frame struct {
			Text string          `json:"text"`
			Cut  json.RawMessage `json:"cut"`
		}
		if err := json.Unmarshal(row.payload, &frame); err != nil {
			t.Fatalf("decode token row: %v", err)
		}
		out = append(out, storedToken{key: row.key, text: frame.Text, cut: framecut.DecodeCut(frame.Cut), raw: row.payload})
	}
	return out
}

// countType returns how many rows of eventType the session has.
func (r *eventFrameRig) countType(ctx context.Context, t *testing.T, eventType string) int {
	t.Helper()
	var n int
	if err := r.h.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = $2`, r.sessionID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

func (r *eventFrameRig) readySeq(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := r.h.Pool.QueryRow(ctx, `SELECT ready_seq FROM sandboxes WHERE session_id = $1`, r.sessionID).Scan(&seq); err != nil {
		t.Fatalf("read ready_seq: %v", err)
	}
	return seq
}

// final reads the first turn's text as the plan reader does
// (sessionactor.ReadPlanFinal, the read the approval makes).
func (r *eventFrameRig) final(ctx context.Context, t *testing.T) plandomain.Final {
	t.Helper()
	final, found, err := sessionactor.ReadPlanFinal(ctx, r.h.Turns, r.h.Events, r.sessionID, r.turnID)
	if err != nil || !found {
		t.Fatalf("ReadPlanFinal = found %v, %v; want the turn's text", found, err)
	}
	return final
}

// awaitingPlan returns the plan the first turn recorded.
func (r *eventFrameRig) awaitingPlan(ctx context.Context, t *testing.T) sqlcgen.Plan {
	t.Helper()
	var plan sqlcgen.Plan
	waitUntil(t, scenario23Wait, func() bool {
		plans, err := narvipg.NewPlanStore(r.h.Pool).ListForSession(ctx, r.sessionID)
		if err != nil {
			t.Fatalf("list plans: %v", err)
		}
		for _, p := range plans {
			if p.TurnID == r.turnID {
				plan = p
				return true
			}
		}
		return false
	})
	return plan
}

// approve approves planID as every surface does (httpapi.DecidePlan).
func (r *eventFrameRig) approve(ctx context.Context, planID pgtype.UUID) (httpapi.DecidePlanOutcome, error) {
	pool := r.h.Pool
	return httpapi.DecidePlan(ctx, pool, r.h.Sessions, r.h.Turns, narvipg.NewPlanStore(pool), r.h.Events, narvipg.NewPlanDocumentStore(pool),
		narvipg.NewOutboxStore(pool, false), narvipg.NewLinearAgentSessionStore(pool), narvipg.NewAuditLogStore(pool), r.registry,
		r.sessionID, planID, httpapi.PlanVerdictApprove, pgtype.UUID{}, false)
}

func (r *eventFrameRig) planStatus(ctx context.Context, t *testing.T, planID pgtype.UUID) sqlcgen.PlanStatus {
	t.Helper()
	plan, err := narvipg.NewPlanStore(r.h.Pool).Get(ctx, planID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	return plan.Status
}

// scriptedAgent is a real sandbox agent's bridge with a CommandHandler a
// test scripts: each prompt runs onPrompt off the bridge's read loop, so a
// script may wait on the test without holding the connection's reads.
type scriptedAgent struct {
	noopCommandHandler
	t         *testing.T
	ctx       context.Context
	bridge    *wsbridge.Bridge
	sessionID string

	onPrompt        func(a *scriptedAgent, n int, cmd sandboxws.Prompt)
	onPush          func(a *scriptedAgent, cmd sandboxws.Push)
	answerSnapshots bool

	scripts errgroup.Group
	mu      sync.Mutex
	prompts []sandboxws.Prompt
}

func (a *scriptedAgent) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	a.mu.Lock()
	a.prompts = append(a.prompts, cmd)
	n := len(a.prompts)
	a.mu.Unlock()
	if a.onPrompt == nil {
		return
	}
	a.scripts.Go(func() error {
		a.onPrompt(a, n, cmd)
		return nil
	})
}

func (a *scriptedAgent) HandleSnapshot(_ context.Context, cmd sandboxws.Snapshot) {
	if !a.answerSnapshots {
		return
	}
	id := "snap-" + cmd.MessageId
	commandID := cmd.MessageId
	a.critical(sandboxws.SnapshotReady{Type: "snapshot_ready", MessageId: id, SessionId: a.sessionID, Gen: 1,
		AckId: "snapshot_ready:" + id, SnapshotId: "snap-scenario23", CommandMessageId: &commandID}, "snapshot_ready:"+id)
}

func (a *scriptedAgent) HandlePush(_ context.Context, cmd sandboxws.Push) {
	if a.onPush != nil {
		a.onPush(a, cmd)
	}
}

func (a *scriptedAgent) promptsRead() []sandboxws.Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sandboxws.Prompt(nil), a.prompts...)
}

func (a *scriptedAgent) best(msg any) {
	if err := a.bridge.SendBestEffort(a.ctx, msg); err != nil {
		a.t.Errorf("SendBestEffort: %v", err)
	}
}

func (a *scriptedAgent) critical(msg any, ackID string) {
	if err := a.bridge.SendCritical(a.ctx, msg, ackID); err != nil {
		a.t.Errorf("SendCritical(%s): %v", ackID, err)
	}
}

func (a *scriptedAgent) stepStart(messageID, stepID string) {
	a.best(sandboxws.StepStart{Type: "step_start", MessageId: messageID, SessionId: a.sessionID, Gen: 1, StepId: stepID})
}

func (a *scriptedAgent) token(part, text string) {
	a.best(sandboxws.Token{Type: "token", MessageId: part, SessionId: a.sessionID, Gen: 1, Text: text})
}

// toolResult sends a tool_result under its enclosing message's id, as the
// pinned runtime's adapter does.
func (a *scriptedAgent) toolResult(messageID, callID, output string) {
	a.best(sandboxws.ToolResult{Type: "tool_result", MessageId: messageID, SessionId: a.sessionID, Gen: 1, CallId: callID,
		Output: sandboxws.ToolResultOutput{"output": output}})
}

func (a *scriptedAgent) complete(id string) {
	a.critical(sandboxws.ExecutionComplete{Type: "execution_complete", MessageId: id, SessionId: a.sessionID, Gen: 1,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted}, "execution_complete:"+id)
}

// sentinel sends a small best-effort event: once its row is stored, the
// control plane has read every frame the connection carried before it.
func (a *scriptedAgent) sentinel(id string) {
	a.best(sandboxws.Warning{Type: "warning", MessageId: id, SessionId: a.sessionID, Gen: 1, Message: "scenario23 sentinel"})
}

// await waits for ch, or for the agent to stop; it reports which.
func (a *scriptedAgent) await(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-a.ctx.Done():
		return false
	}
}

// startScriptedAgent runs a real Bridge against the rig's relay, with the
// handler configure scripts.
func startScriptedAgent(ctx context.Context, t *testing.T, rig *eventFrameRig, configure func(a *scriptedAgent)) *scriptedAgent {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	agent := &scriptedAgent{t: t, ctx: runCtx, sessionID: rig.sessionID.String()}
	configure(agent)
	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: rig.proxyURL,
		Gen:               1,
		SandboxToken:      "scenario23-test-token", // the fresh row's NULL token_hash admits any (scenario #7's note)
		SessionId:         rig.sessionID.String(),
	}
	agent.bridge = wsbridge.New(sc, "sbx-scenario23", "test-agent-version", "test-image-digest", agent,
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)
	var group errgroup.Group
	group.Go(func() error { return agent.bridge.Run(runCtx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("bridge.Run() error = %v, want nil after ctx cancellation", err)
		}
		_ = agent.scripts.Wait()
	})
	return agent
}

// scenario23PlanBlock is a complete plan-steps block: whole, the plan
// reads as structured steps; cut, it reads as none, though the cut leaves
// the block intact near the start.
const scenario23PlanBlock = "```plan-steps\n" +
	`{"steps":[{"title":"Add the column","description":"One nullable column.","fileRefs":["migrations/000999.up.sql"]}],"scopeEstimate":"1 file"}` +
	"\n```\n"

// planTextOfSize is a plan of exactly n bytes of plain text: the
// plan-steps block within its first 4 KiB, then prose.
func planTextOfSize(n int) string {
	var b strings.Builder
	b.WriteString("Plan for the change.\n\n")
	b.WriteString(scenario23PlanBlock)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "Detail %05d: the step reads the stored rows and writes the new column.\n", i)
	}
	return b.String()[:n]
}

// TestResilience_Scenario23_AgentEventOver32KiB_ReadOnOneConnection: a
// control plane that states its limit reads a 40 KiB text part and a 40
// KiB tool_result on the connection they came on, after one ready: the
// part is stored whole, the tool_result reaches it whole and adds no row,
// and the execution_complete behind them completes the turn.
func TestResilience_Scenario23_AgentEventOver32KiB_ReadOnOneConnection(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{first: headerControlPlane, fallback: headerControlPlane})
	text := planTextOfSize(40 * 1024)
	output := strings.Repeat("ok  github.com/x/y 0.1s\n", 40*1024/24+1)
	startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.onPrompt = func(a *scriptedAgent, _ int, _ sandboxws.Prompt) {
			a.stepStart("msg_1", "s1")
			a.token("prt_1", "")
			a.token("prt_1", text)
			a.toolResult("msg_1", "call_1", output)
			a.complete("ec-1")
		}
	})

	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })

	rows := rig.tokenRows(ctx, t, "prt_1")
	if len(rows) != 2 || rows[1].text != text || rows[1].cut != nil {
		t.Fatalf("stored %d frames of the part; want the empty one and the 40 KiB one, whole", len(rows))
	}
	results := rig.relayedFrames("tool_result")
	if len(results) != 1 || len(results[0]) <= 40*1024 || strings.Contains(string(results[0]), `"cut"`) {
		t.Fatalf("the control plane was written %d tool_results; want one, whole and over 40 KiB", len(results))
	}
	if n := rig.countType(ctx, t, "tool_result"); n != 0 {
		t.Fatalf("%d tool_result rows, want none: it carries its message's id, which that message's step_start holds", n)
	}
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1: every frame was read on the connection it came on", got)
	}
}

// TestResilience_Scenario23_PushErrorOver32KiB_ReportedAndNextTurnCompletes:
// after a completed turn, the push's push_error carries 40 KiB of git
// stderr, capped as the agent caps it (wsbridge.CapCriticalText): it is
// stored, marked truncated, and acked, the delivery stamp ends, and the
// next turn's execution_complete behind it completes that turn. Before the
// cap and the raised limit, that push_error closed every reconnect, and no
// push failure was ever reported.
func TestResilience_Scenario23_PushErrorOver32KiB_ReportedAndNextTurnCompletes(t *testing.T) {
	ctx := context.Background()
	repos, err := json.Marshal([]map[string]any{{"name": "repo1", "url": "https://github.com/scenario23/repo1.git", "branch": "scenario23-branch"}})
	if err != nil {
		t.Fatal(err)
	}
	rig := newEventFrameRig(ctx, t, eventRigOptions{first: headerControlPlane, fallback: headerControlPlane, repos: repos})
	// A repository starts in shadow, which sends no push (§30.8): this one
	// is promoted, so the completed turn's push is sent.
	if _, err := narvipg.NewRepoSettingsStore(rig.h.Pool).UpsertLiveEgressEnabled(ctx, "scenario23/repo1", true); err != nil {
		t.Fatalf("promote the repository to live egress: %v", err)
	}
	stderr := strings.Repeat("remote: error: refusing to update the branch: hook declined\n", 40*1024/60+1)
	var pushes int
	var pushMu sync.Mutex
	startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.answerSnapshots = true
		a.onPrompt = func(a *scriptedAgent, n int, _ sandboxws.Prompt) { a.complete(fmt.Sprintf("ec-%d", n)) }
		a.onPush = func(a *scriptedAgent, cmd sandboxws.Push) {
			pushMu.Lock()
			pushes++
			id := fmt.Sprintf("push-error-%d", pushes)
			pushMu.Unlock()
			a.critical(sandboxws.PushError{Type: "push_error", MessageId: id, SessionId: a.sessionID, Gen: cmd.Gen, AckId: "push_error:" + id,
				Error: wsbridge.CapCriticalText("git push repo1: exited 1: " + stderr)}, "push_error:"+id)
		}
	})

	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	waitUntil(t, scenario23Wait, func() bool { return countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "push-error-1") == 1 })

	var reported string
	if err := rig.h.Pool.QueryRow(ctx, `SELECT payload->>'error' FROM events WHERE session_id = $1 AND message_id = 'push-error-1'`, rig.sessionID).Scan(&reported); err != nil {
		t.Fatalf("read the stored push_error: %v", err)
	}
	if len(reported) > 4096 || !strings.HasSuffix(reported, "...[truncated]") || !strings.HasPrefix(reported, "git push repo1: exited 1: remote: error") {
		t.Fatalf("stored push_error carries %d bytes; want git's stderr capped at 4096, its head kept and marked", len(reported))
	}
	waitUntil(t, scenario23Wait, func() bool { return rig.ackRelayed("push_error:push-error-1") })
	waitUntil(t, scenario23Wait, func() bool {
		var delivering bool
		if err := rig.h.Pool.QueryRow(ctx, `SELECT pr_delivery_started_at IS NOT NULL FROM sandboxes WHERE session_id = $1`, rig.sessionID).Scan(&delivering); err != nil {
			t.Fatalf("read pr_delivery_started_at: %v", err)
		}
		return !delivering
	})

	// The next turn, behind it on the same connection.
	prompt := "scenario23 next turn"
	next, err := rig.h.Turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: rig.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create the next turn: %v", err)
	}
	actor, err := rig.registry.GetOrSpawn(ctx, rig.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := actor.Send(ctx, sessionactor.EnsureDispatched{}); err != nil {
		t.Fatalf("EnsureDispatched: %v", err)
	}
	waitUntil(t, scenario23Wait, func() bool {
		got, err := rig.h.Turns.Get(ctx, next.ID)
		if err != nil {
			t.Fatalf("get the next turn: %v", err)
		}
		return got.Status == sqlcgen.TurnStatusCompleted
	})
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1", got)
	}

	// Acked, it left the agent's buffer: the next connection never
	// replays it.
	rig.proxy.sever()
	waitUntil(t, scenario23Wait, func() bool { return rig.readyCount() == 2 })
	time.Sleep(500 * time.Millisecond)
	replays := 0
	for _, payload := range rig.relayedFrames("push_error") {
		var pe sandboxws.PushError
		if json.Unmarshal(payload, &pe) == nil && pe.MessageId == "push-error-1" {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("the push_error was written %d times, want once: acked, it is never replayed", replays)
	}
}

// TestResilience_Scenario23_PreChangeAgent_EventOver32KiB_Stored: an agent
// built before the header, which bounds nothing it writes, needs no change
// for an event up to the raised limit: its 40 KiB text part is read on the
// connection it came on and stored whole.
func TestResilience_Scenario23_PreChangeAgent_EventOver32KiB_Stored(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{})
	sid := rig.sessionID.String()
	agent := dialPromptFrameAgent(ctx, t, rig.proxyURL, sid, "", 0, false)

	waitUntil(t, scenario23Wait, func() bool {
		prompts, _ := agent.read()
		return len(prompts) == 1
	})
	text := planTextOfSize(40 * 1024)
	agent.write(ctx, t, fmt.Sprintf(`{"type":"token","messageId":"prt_old","sessionId":%q,"gen":1,"text":""}`, sid))
	frame, err := json.Marshal(sandboxws.Token{Type: "token", MessageId: "prt_old", SessionId: sid, Gen: 1, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	agent.write(ctx, t, string(frame))
	agent.write(ctx, t, fmt.Sprintf(`{"type":"execution_complete","messageId":"ec-old","sessionId":%q,"gen":1,"ackId":"execution_complete:ec-old","outcome":"completed","reason":null}`, sid))

	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	rows := rig.tokenRows(ctx, t, "prt_old")
	if len(rows) != 2 || rows[1].text != text {
		t.Fatalf("stored %d frames of the part, want the empty one and the 40 KiB one whole", len(rows))
	}
	if _, readErr := agent.read(); readErr != nil {
		t.Fatalf("the agent's connection ended: %v; want the frame read on the connection it came on", readErr)
	}
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1", got)
	}
}

// TestResilience_Scenario23_PreChangeControlPlane_FramesCutToFit_NoReconnectLoop:
// a control plane built before the header states nothing and reads 32 KiB.
// The agent holds every write to that: a plan-mode turn's 40 KiB text part
// arrives cut, with `cut` set, so the plan reads as cut -- its marker
// visible, no structured steps though the plan-steps block in its first 4
// KiB survived the cut, and Approve refused -- and a 40 KiB tool_result
// arrives cut to 32 KiB; the execution_complete behind them completes the
// turn, and the connection stays the one the ready came on. Once the agent
// reconnects to a control plane that states its limit, its replay writes
// both whole, adding no row, the turn being over.
func TestResilience_Scenario23_PreChangeControlPlane_FramesCutToFit_NoReconnectLoop(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{first: noHeaderControlPlane, fallback: noHeaderControlPlane, planMode: true})
	text := planTextOfSize(40 * 1024)
	output := strings.Repeat("ok  github.com/x/y 0.1s\n", 40*1024/24+1)
	agent := startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.onPrompt = func(a *scriptedAgent, n int, _ sandboxws.Prompt) {
			if n > 1 {
				a.complete(fmt.Sprintf("ec-%d", n))
				return
			}
			a.stepStart("msg_1", "s1")
			a.token("prt_plan", "")
			a.token("prt_plan", text)
			a.toolResult("msg_1", "call_1", output)
			a.complete("ec-1")
		}
	})

	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })

	rows := rig.tokenRows(ctx, t, "prt_plan")
	if len(rows) != 2 || rows[1].cut == nil || rows[1].cut.Total != len(text) || !strings.HasPrefix(text, rows[1].text[:rows[1].cut.Kept]) {
		t.Fatalf("stored %d frames of the part; want the empty one and a cut of the 40 KiB one, `cut` set", len(rows))
	}
	final := rig.final(ctx, t)
	if final.Cut == nil || final.Text != rows[1].text || !strings.HasSuffix(final.Text, fmt.Sprintf("[text cut at %d of %d bytes on its way from the sandbox]", final.Cut.Kept, len(text))) {
		t.Fatalf("FinalText = cut %v, %d bytes; want the cut text, ending in its marker, and the cut reported", final.Cut, len(final.Text))
	}
	if plandomain.ExtractStructured(final.Text, nil) == nil {
		t.Fatal("the cut text holds no whole plan-steps block: this test could not tell a cut from a missing block")
	}
	if got := plandomain.ExtractStructured(final.Text, final.Cut); got != nil {
		t.Fatalf("ExtractStructured of the cut plan = %+v, want nil", got)
	}
	plan := rig.awaitingPlan(ctx, t)
	if _, err := rig.approve(ctx, plan.ID); !errors.Is(err, httpapi.ErrPlanCut) {
		t.Fatalf("approve the cut plan = %v, want ErrPlanCut", err)
	}
	if status := rig.planStatus(ctx, t, plan.ID); status != sqlcgen.PlanStatusAwaitingApproval {
		t.Fatalf("plan status %s after the refused approval, want awaiting_approval", status)
	}

	results := rig.relayedFrames("tool_result")
	if len(results) != 1 || len(results[0]) > platform.DefaultFrameReadLimitBytes || !strings.Contains(string(results[0]), `"cut":{`) {
		t.Fatalf("the control plane was written %d tool_results; want one, cut to at most 32 KiB", len(results))
	}
	for _, payload := range rig.relayedFrames("") {
		if len(payload) > platform.DefaultFrameReadLimitBytes {
			t.Fatalf("a %s frame of %d bytes reached a control plane that reads 32 KiB", relayType(payload), len(payload))
		}
	}
	if seq := rig.readySeq(ctx, t); seq != 1 {
		t.Fatalf("ready_seq = %d, want 1", seq)
	}
	time.Sleep(2 * time.Second)
	if seq := rig.readySeq(ctx, t); seq != 1 {
		t.Fatalf("ready_seq = %d two seconds on, want 1: no reconnect loop", seq)
	}

	// The agent reconnects to a control plane that states its limit, and
	// its replay writes the cut frames whole -- the turn being over, they
	// add no row.
	rowsBefore := len(rows)
	rig.proxy.setFallback(headerControlPlane)
	rig.proxy.sever()
	waitUntil(t, scenario23Wait, func() bool { return rig.readyCount() == 2 })
	waitUntil(t, scenario23Wait, func() bool {
		wholeToken, wholeResult := false, false
		for _, payload := range rig.relayedFrames("") {
			whole := len(payload) > 40*1024 && !strings.Contains(string(payload), `"cut":{`)
			wholeToken = wholeToken || (whole && relayType(payload) == "token")
			wholeResult = wholeResult || (whole && relayType(payload) == "tool_result")
		}
		return wholeToken && wholeResult
	})
	agent.sentinel("sentinel-after-replay")
	waitUntil(t, scenario23Wait, func() bool {
		return countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "sentinel-after-replay") == 1
	})
	if got := len(rig.tokenRows(ctx, t, "prt_plan")); got != rowsBefore {
		t.Fatalf("%d frames of the part stored after the replay, want %d: no row once the turn is over", got, rowsBefore)
	}
	if n := rig.countType(ctx, t, "tool_result"); n != 0 {
		t.Fatalf("%d tool_result rows, want none", n)
	}
}

// wholeThenCutFixture is the part the rollback test leaves in the log, as
// the web client receives it; the web timeline test reads the same file
// (web/src/session/__tests__/timelineModel.test.ts), so the rows the real
// handler and a pre-cut replica store are the rows the timeline is pinned
// against. Set NARVI_UPDATE_FIXTURES=1 to rewrite it from a run.
const wholeThenCutFixture = "../../web/src/session/__tests__/fixtures/tokenWholeThenCutOnRollback.json"

// scenario23FixtureSessionID stands in for the test session's random id in
// the fixture.
const scenario23FixtureSessionID = "00000000-0000-4000-8000-000000000001"

// fixtureEvent is one row as the web client's EventEnvelope carries it
// (web/src/ws/types.ts).
type fixtureEvent struct {
	ID        int            `json:"id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt string         `json:"createdAt"`
}

// stepAndTokenRowsAsFixture returns the session's step_start and token
// rows as the web client receives them, with what varies between runs
// pinned: ids renumbered from 1, the session id replaced, one createdAt.
func (r *eventFrameRig) stepAndTokenRowsAsFixture(ctx context.Context, t *testing.T) []fixtureEvent {
	t.Helper()
	rows, err := r.h.Pool.Query(ctx, `SELECT type, payload FROM events WHERE session_id = $1 AND type IN ('step_start', 'token') ORDER BY id`, r.sessionID)
	if err != nil {
		t.Fatalf("query the log: %v", err)
	}
	type raw struct {
		eventType string
		payload   []byte
	}
	var scanned []raw
	for rows.Next() {
		var row raw
		if err := rows.Scan(&row.eventType, &row.payload); err != nil {
			rows.Close()
			t.Fatalf("scan event row: %v", err)
		}
		scanned = append(scanned, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event rows: %v", err)
	}
	out := make([]fixtureEvent, 0, len(scanned))
	for _, row := range scanned {
		var payload map[string]any
		if err := json.Unmarshal(row.payload, &payload); err != nil {
			t.Fatalf("decode %s payload: %v", row.eventType, err)
		}
		payload["sessionId"] = scenario23FixtureSessionID
		out = append(out, fixtureEvent{ID: len(out) + 1, Type: row.eventType, Payload: payload, CreatedAt: "2026-10-03T10:00:00Z"})
	}
	return out
}

// TestResilience_Scenario23_WholeThenCutOnRollback_FinalTextReadsWhole: a
// 40 KiB text part of a Processing plan-mode turn is stored whole through a
// control plane that states its limit. The agent then reconnects to one
// that states nothing and replays the part cut. The real handler adds no
// row for the cut form -- the turn is live, so its guard decides, not the
// turn rule: the cut yields to the whole text it was taken from -- but a
// replica built before cuts stores it as a row of its own, `cut` in its
// raw payload, as it would during a rollback. Rolled forward, the plan
// reader and the web timeline read the part whole, no cut reported, and
// the plan is approved.
func TestResilience_Scenario23_WholeThenCutOnRollback_FinalTextReadsWhole(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{first: headerControlPlane, fallback: headerControlPlane, planMode: true})
	whole := planTextOfSize(40 * 1024)
	finish := make(chan struct{})
	agent := startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.onPrompt = func(a *scriptedAgent, n int, _ sandboxws.Prompt) {
			if n > 1 {
				a.complete(fmt.Sprintf("ec-%d", n))
				return
			}
			a.stepStart("msg_1", "s1")
			a.token("prt_plan", "")
			a.token("prt_plan", whole)
			if a.await(finish) {
				a.complete("ec-1")
			}
		}
	})

	// 1. Stored whole through a control plane that states its limit.
	waitUntil(t, scenario23Wait, func() bool {
		rows := rig.tokenRows(ctx, t, "prt_plan")
		return len(rows) == 2 && rows[1].text == whole
	})

	// 2. A reconnect to a control plane that states nothing: the replay
	// writes the part cut.
	rig.proxy.setFallback(noHeaderControlPlane)
	rig.proxy.sever()
	waitUntil(t, scenario23Wait, func() bool { return rig.readyCount() == 2 })
	var cutFrame []byte
	waitUntil(t, scenario23Wait, func() bool {
		for _, payload := range rig.relayedFrames("token") {
			if strings.Contains(string(payload), `"cut":{`) {
				cutFrame = payload
				return true
			}
		}
		return false
	})
	agent.sentinel("sentinel-after-cut")
	waitUntil(t, scenario23Wait, func() bool {
		return countEventsByMessageID(ctx, t, rig.h, rig.sessionID, "sentinel-after-cut") == 1
	})
	if rig.turn(ctx, t).Status != sqlcgen.TurnStatusProcessing {
		t.Fatal("the turn is no longer Processing: the turn rule, not the guard, would decide")
	}

	// 3. The real handler added no row for the cut form.
	if rows := rig.tokenRows(ctx, t, "prt_plan"); len(rows) != 2 {
		t.Fatalf("%d frames of the part stored, want 2: the cut form yields to the whole text stored, so it adds no row", len(rows))
	}

	// A replica built before cuts stores it all the same, under the key
	// it gives every later frame of a part.
	var cut struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(cutFrame, &cut); err != nil {
		t.Fatalf("decode the cut frame: %v", err)
	}
	sum := sha256.Sum256([]byte(cut.Text))
	if _, err := rig.h.Events.Create(ctx, sqlcgen.CreateEventParams{SessionID: rig.sessionID, Type: "token",
		MessageID: "prt_plan#" + hex.EncodeToString(sum[:16]), Payload: cutFrame}); err != nil {
		t.Fatalf("store the cut frame as a replica built before cuts does: %v", err)
	}

	// 4. Rolled forward, the part reads whole.
	if final := rig.final(ctx, t); final.Text != whole || final.Cut != nil {
		t.Fatalf("FinalText = %d bytes, cut %v; want the whole text, no cut", len(final.Text), final.Cut)
	}
	got := rig.stepAndTokenRowsAsFixture(ctx, t)
	if len(got) != 4 {
		t.Fatalf("the log holds %d step and token rows, want the step_start, the empty frame, the whole text and its cut", len(got))
	}
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal the log: %v", err)
	}
	gotJSON = append(gotJSON, '\n')
	path := filepath.FromSlash(wholeThenCutFixture)
	if os.Getenv("NARVI_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, gotJSON, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v (set NARVI_UPDATE_FIXTURES=1 to write it)", path, err)
	}
	if string(gotJSON) != string(want) {
		t.Fatalf("the stored rows differ from %s, which the web timeline test reads; set NARVI_UPDATE_FIXTURES=1 to rewrite it", path)
	}

	// 5. The turn completes, and the plan is approved.
	close(finish)
	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	plan := rig.awaitingPlan(ctx, t)
	outcome, err := rig.approve(ctx, plan.ID)
	if err != nil || !outcome.Won {
		t.Fatalf("approve = %+v, %v; want it approved", outcome, err)
	}
}

// TestResilience_Scenario23_EarlierFrameWholeThenFinalCut_CutReported: a
// part sent as a 35 KiB frame and then a 45 KiB one. The first is stored
// whole through a control plane that states its limit; the agent then
// reconnects to one that states nothing, whose replay writes the first cut
// -- it yields to the 35 KiB stored whole, and adds no row -- and the 45
// KiB frame arrives cut. The plan reads that cut, never the earlier 35 KiB
// frame, which shares its kept bytes but is not the text it was cut from,
// and the approval is refused.
func TestResilience_Scenario23_EarlierFrameWholeThenFinalCut_CutReported(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{first: headerControlPlane, fallback: headerControlPlane, planMode: true})
	final45 := planTextOfSize(45 * 1024)
	earlier35 := final45[:35*1024]
	next := make(chan struct{})
	startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.onPrompt = func(a *scriptedAgent, n int, _ sandboxws.Prompt) {
			if n > 1 {
				a.complete(fmt.Sprintf("ec-%d", n))
				return
			}
			a.stepStart("msg_1", "s1")
			a.token("prt_plan", "")
			a.token("prt_plan", earlier35)
			if !a.await(next) {
				return
			}
			a.token("prt_plan", final45)
			a.complete("ec-1")
		}
	})

	waitUntil(t, scenario23Wait, func() bool {
		rows := rig.tokenRows(ctx, t, "prt_plan")
		return len(rows) == 2 && rows[1].text == earlier35
	})
	rig.proxy.setFallback(noHeaderControlPlane)
	rig.proxy.sever()
	waitUntil(t, scenario23Wait, func() bool { return rig.readyCount() == 2 })
	waitUntil(t, scenario23Wait, func() bool {
		for _, payload := range rig.relayedFrames("token") {
			if strings.Contains(string(payload), fmt.Sprintf(`"total":%d`, len(earlier35))) {
				return true
			}
		}
		return false
	})
	close(next)
	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })

	rows := rig.tokenRows(ctx, t, "prt_plan")
	if len(rows) != 3 || rows[1].text != earlier35 || rows[2].cut == nil || rows[2].cut.Total != len(final45) {
		t.Fatalf("stored %d frames of the part; want the empty one, the 35 KiB one whole, and a cut of the 45 KiB one -- the replayed cut of the 35 KiB one adding none", len(rows))
	}
	final := rig.final(ctx, t)
	if final.Cut == nil || final.Cut.Total != len(final45) || final.Text != rows[2].text {
		t.Fatalf("FinalText = %d bytes, cut %v; want the 45 KiB frame's cut, reported", len(final.Text), final.Cut)
	}
	plan := rig.awaitingPlan(ctx, t)
	if _, err := rig.approve(ctx, plan.ID); !errors.Is(err, httpapi.ErrPlanCut) {
		t.Fatalf("approve = %v, want ErrPlanCut", err)
	}
	if status := rig.planStatus(ctx, t, plan.ID); status != sqlcgen.PlanStatusAwaitingApproval {
		t.Fatalf("plan status %s after the refused approval, want awaiting_approval", status)
	}
}

// TestResilience_Scenario23_NoHeader_WritesHeldTo32KiB_PromptOver32KiBStillRead:
// a missing header bounds what the agent writes, never what it reads. A
// control plane that states nothing is sent a 40 KiB prompt, which the
// agent reads and runs once, and is written nothing over 32 KiB.
func TestResilience_Scenario23_NoHeader_WritesHeldTo32KiB_PromptOver32KiBStillRead(t *testing.T) {
	ctx := context.Background()
	rig := newEventFrameRig(ctx, t, eventRigOptions{prompt: promptOver32KiB})
	text := planTextOfSize(40 * 1024)
	output := strings.Repeat("ok  github.com/x/y 0.1s\n", 40*1024/24+1)
	agent := startScriptedAgent(ctx, t, rig, func(a *scriptedAgent) {
		a.onPrompt = func(a *scriptedAgent, _ int, _ sandboxws.Prompt) {
			a.stepStart("msg_1", "s1")
			a.token("prt_1", "")
			a.token("prt_1", text)
			a.toolResult("msg_1", "call_1", output)
			a.complete("ec-1")
		}
	})

	waitUntil(t, scenario23Wait, func() bool { return rig.turn(ctx, t).Status == sqlcgen.TurnStatusCompleted })
	prompts := agent.promptsRead()
	if len(prompts) != 1 || len(prompts[0].Text) < 40*1024 {
		t.Fatalf("the agent read %d prompts; want the 40 KiB one, once", len(prompts))
	}
	for _, payload := range rig.relayedFrames("") {
		if len(payload) > platform.DefaultFrameReadLimitBytes {
			t.Fatalf("a %s frame of %d bytes was written to a control plane that states no limit, want at most %d",
				relayType(payload), len(payload), platform.DefaultFrameReadLimitBytes)
		}
	}
	if rows := rig.tokenRows(ctx, t, "prt_1"); len(rows) != 2 || rows[1].cut == nil {
		t.Fatalf("stored %d frames of the part; want the 40 KiB one cut", len(rows))
	}
	if got := rig.readyCount(); got != 1 {
		t.Fatalf("%d readies relayed, want 1", got)
	}
}

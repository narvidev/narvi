//go:build integration

// Resilience scenario #18, first half (§9.3, docs/IMPLEMENTATION_PLAN.md
// row 137): "Turn dispatched onto a sandbox past its runway floor ->
// rotation first, turn runs on the replacement, never a failure."
//
// The control plane is real: a registry with a real wshub commander, the
// real sandbox WS handler and the real snapshot-mint endpoint behind a test
// server, real Postgres. Each sandbox is a real wsbridge.Bridge dialling
// that server with its gen's SessionConfig. Gen 1 is seeded ready with ten
// minutes left before its lifetime deadline, under the 13-minute floor;
// its agent answers a snapshot at once and is told to shut down. The
// provider is a stand-in whose RestoreFromSnapshot starts gen 2's agent
// with the SessionConfig the restore claim minted -- its token, its gen --
// once the claim's provider object is recorded, and that agent boots and
// runs the turn (technical plan §35.3).
package resilience_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const (
	scenario18Wait       = 15 * time.Second
	scenario18Gen1Object = "provider-gen-1"
	scenario18Gen1Token  = "scenario18-gen-1-token"
	scenario18Snapshot   = "snap-gen-1-rotation"
)

// rotationAgent is one gen's sandbox-agent: it answers a snapshot command
// at once, and runs a prompt by recording it and completing it.
type rotationAgent struct {
	noopCommandHandler
	ctx    context.Context
	bridge *wsbridge.Bridge
	gen    int

	mu      sync.Mutex
	prompts []sandboxws.Prompt
	// ended is what Run returned, once it has.
	ended atomic.Pointer[error]
}

func (a *rotationAgent) HandlePrompt(_ context.Context, cmd sandboxws.Prompt) {
	a.mu.Lock()
	a.prompts = append(a.prompts, cmd)
	a.mu.Unlock()
	id := "ec-" + cmd.MessageId
	_ = a.bridge.SendCritical(a.ctx, sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	}, "execution_complete:"+id)
}

func (a *rotationAgent) HandleSnapshot(_ context.Context, cmd sandboxws.Snapshot) {
	id := uuid.NewString()
	command := cmd.MessageId
	snapshot := scenario18Snapshot
	if a.gen != 1 {
		snapshot = fmt.Sprintf("snap-gen-%d-%s", a.gen, id)
	}
	_ = a.bridge.SendCritical(a.ctx, sandboxws.SnapshotReady{
		Type: "snapshot_ready", MessageId: id, SessionId: cmd.SessionId, Gen: cmd.Gen,
		AckId: "snapshot_ready:" + id, SnapshotId: snapshot, CommandMessageId: &command,
	}, "snapshot_ready:"+id)
}

func (a *rotationAgent) seenPrompts() []sandboxws.Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sandboxws.Prompt(nil), a.prompts...)
}

// endedWith reports whether the agent's Run has returned, and what it
// returned.
func (a *rotationAgent) endedWith() (bool, error) {
	p := a.ended.Load()
	if p == nil {
		return false, nil
	}
	return true, *p
}

// scenario18Rig is one session on the real control-plane stack.
type scenario18Rig struct {
	h         *Harness
	registry  *sessionactor.Registry
	sessionID pgtype.UUID
	serverURL string
	provider  *rotationProvider
	ctx       context.Context
	group     *errgroup.Group

	mu     sync.Mutex
	agents map[int]*rotationAgent
}

// rotationProvider is the rig's ports.SandboxProvider: it records every
// stop and restore, and a restore starts the new gen's agent.
type rotationProvider struct {
	rig *scenario18Rig

	mu       sync.Mutex
	stops    []string
	restores []ports.SnapshotID
	creates  int
}

var _ ports.SandboxProvider = (*rotationProvider)(nil)

func (p *rotationProvider) Capabilities() ports.Capabilities {
	return ports.Capabilities{Snapshots: true, ExplicitStop: true}
}
func (p *rotationProvider) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	return ports.SandboxRef{}, errors.New("rotationProvider: a rotation restores, it never creates")
}
func (p *rotationProvider) StopSandbox(_ context.Context, ref ports.SandboxRef) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops = append(p.stops, ref.ProviderID)
	return nil
}
func (p *rotationProvider) ResumeSandbox(context.Context, ports.SandboxRef) error {
	return errors.New("rotationProvider: no resume")
}
func (p *rotationProvider) TakeSnapshot(_ context.Context, ref ports.SandboxRef) (ports.SnapshotID, error) {
	return ports.SnapshotID("snap-minted-" + ref.ProviderID), nil
}
func (p *rotationProvider) RestoreFromSnapshot(_ context.Context, id ports.SnapshotID, spec ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	p.restores = append(p.restores, id)
	p.mu.Unlock()
	object := fmt.Sprintf("provider-gen-%d", spec.Gen)
	p.rig.startAgentOnceRecorded(spec.SessionConfig, object)
	return ports.SandboxRef{ProviderID: object}, nil
}
func (p *rotationProvider) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	return ports.BuildOutcome{}, errors.New("rotationProvider: no image builds")
}
func (p *rotationProvider) DeleteImage(context.Context, ports.ImageRef) error { return nil }
func (p *rotationProvider) List(context.Context) ([]ports.SandboxRef, error)  { return nil, nil }

func (p *rotationProvider) recorded() (stops []string, restores []ports.SnapshotID, creates int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.stops...), append([]ports.SnapshotID(nil), p.restores...), p.creates
}

// newScenario18Rig seeds the session and gen 1, ready, ten minutes from
// its deadline, and serves the sandbox socket and the snapshot mint.
// breakerOpen opens the spawn circuit breaker, so a restore waits until
// closeBreaker.
func newScenario18Rig(ctx context.Context, t *testing.T, breakerOpen bool) *scenario18Rig {
	t.Helper()
	h := newHarness(t)
	runCtx, cancel := context.WithCancel(ctx)
	rig := &scenario18Rig{h: h, ctx: runCtx, group: &errgroup.Group{}, agents: map[int]*rotationAgent{}}
	t.Cleanup(func() {
		cancel()
		if err := rig.group.Wait(); err != nil {
			t.Errorf("agents: %v", err)
		}
	})
	rig.provider = &rotationProvider{rig: rig}

	var handler atomic.Pointer[http.Handler]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*handler.Load()).ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	rig.serverURL = server.URL

	commander := wshub.NewSandboxRegistry(h.Timeouts)
	rig.registry = h.NewRegistryWithProvider(ctx, t, commander, rig.provider, server.URL)
	t.Cleanup(func() { _ = rig.registry.Shutdown() })
	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(rig.registry, h.Sandboxes, commander, h.Timeouts))
	router.HandleFunc("/mint/{sessionID}", httpapi.SnapshotMint(h.Sandboxes, rig.provider))
	var routed http.Handler = router
	handler.Store(&routed)

	rig.sessionID = h.CreateSession(ctx, t)
	if _, err := h.Sandboxes.Create(ctx, rig.sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	breaker := 0
	if breakerOpen {
		breaker = 3
	}
	if _, err := h.Pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = 1, provider_id = $2, last_seen_at = now(),
		lifetime_deadline_at = now() + interval '10 minutes', lifetime_seconds = 7200, lifetime_deadline_gen = 1,
		spawn_failure_count = $3, last_spawn_failure_at = CASE WHEN $3 > 0 THEN now() END
		WHERE session_id = $1`, rig.sessionID, scenario18Gen1Object, breaker); err != nil {
		t.Fatalf("seed gen 1: %v", err)
	}
	return rig
}

// closeBreaker closes the spawn circuit breaker newScenario18Rig opened.
func (r *scenario18Rig) closeBreaker(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := r.h.Pool.Exec(ctx, `UPDATE sandboxes SET spawn_failure_count = 0, last_spawn_failure_at = NULL WHERE session_id = $1`, r.sessionID); err != nil {
		t.Fatal(err)
	}
}

func (r *scenario18Rig) wsURL() string {
	return "ws" + strings.TrimPrefix(r.serverURL, "http") + "/sessions/" + r.sessionID.String() + "/ws?type=sandbox"
}

// startAgent runs gen's agent with cfg, its boot completed.
func (r *scenario18Rig) startAgent(cfg sessionconfig.SessionConfig) *rotationAgent {
	agent := &rotationAgent{ctx: r.ctx, gen: cfg.Gen}
	agent.bridge = wsbridge.New(cfg, fmt.Sprintf("sbx-scenario18-gen-%d", cfg.Gen), "test-agent-version", "test-image-digest", agent,
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)
	agent.bridge.ReportBootStarted()
	agent.bridge.MarkBootComplete()
	r.mu.Lock()
	r.agents[cfg.Gen] = agent
	r.mu.Unlock()
	r.group.Go(func() error {
		err := agent.bridge.Run(r.ctx)
		agent.ended.Store(&err)
		return nil
	})
	return agent
}

// startAgentOnceRecorded starts the agent of a gen the provider was asked
// to restore once the claim has recorded its provider object, as a
// provider's sandbox dials back after the provider answered.
func (r *scenario18Rig) startAgentOnceRecorded(cfg sessionconfig.SessionConfig, object string) {
	r.group.Go(func() error {
		for {
			row, err := r.h.Sandboxes.Get(r.ctx, r.sessionID)
			if err == nil && int(row.Gen) == cfg.Gen && row.ProviderID != nil && *row.ProviderID == object {
				break
			}
			select {
			case <-r.ctx.Done():
				return nil
			case <-time.After(20 * time.Millisecond):
			}
		}
		r.startAgent(cfg)
		return nil
	})
}

func (r *scenario18Rig) agent(gen int) *rotationAgent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agents[gen]
}

// startGen1 runs gen 1's agent, and waits until its ready is stored.
func (r *scenario18Rig) startGen1(ctx context.Context, t *testing.T) *rotationAgent {
	t.Helper()
	agent := r.startAgent(sessionconfig.SessionConfig{
		BootMode: sessionconfig.SessionConfigBootModeFresh, ControlPlaneWsUrl: r.wsURL(),
		Gen: 1, SandboxToken: scenario18Gen1Token, SessionId: r.sessionID.String(),
	})
	waitUntil(t, scenario18Wait, func() bool {
		var n int
		err := r.h.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'ready'`, r.sessionID).Scan(&n)
		return err == nil && n > 0
	})
	return agent
}

// queueTurn creates a pending turn and asks for its dispatch, as the
// turn's creation does.
func (r *scenario18Rig) queueTurn(ctx context.Context, t *testing.T) pgtype.UUID {
	t.Helper()
	prompt := "the turn that arrives with ten minutes of runway left"
	created, err := r.h.Turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: r.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create the turn: %v", err)
	}
	r.ensureDispatched(ctx, t)
	return created.ID
}

func (r *scenario18Rig) ensureDispatched(ctx context.Context, t *testing.T) {
	t.Helper()
	actor, err := r.registry.GetOrSpawn(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := actor.Send(ctx, sessionactor.EnsureDispatched{}); err != nil {
		t.Fatalf("EnsureDispatched: %v", err)
	}
}

func (r *scenario18Rig) sandbox(ctx context.Context, t *testing.T) sqlcgen.Sandbox {
	t.Helper()
	row, err := r.h.Sandboxes.Get(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row
}

// TestResilience_Scenario18_TurnPastTheRunwayFloor_RotatesFirstAndRunsOnTheReplacement:
// the turn is never sent to gen 1. Its snapshot is taken, gen 1 is told to
// shut down and its provider object stopped, gen 2 is restored from that
// snapshot with a fresh deadline, boots, and runs the turn to completion:
// no failed turn, no execution_complete reporting a failure, no warning.
func TestResilience_Scenario18_TurnPastTheRunwayFloor_RotatesFirstAndRunsOnTheReplacement(t *testing.T) {
	ctx := context.Background()
	rig := newScenario18Rig(ctx, t, false)
	gen1 := rig.startGen1(ctx, t)

	turnID := rig.queueTurn(ctx, t)
	waitUntil(t, scenario18Wait, func() bool {
		got, err := rig.h.Turns.Get(ctx, turnID)
		return err == nil && got.Status == sqlcgen.TurnStatusCompleted
	})

	got, err := rig.h.Turns.Get(ctx, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 2 || got.EndReason != nil {
		t.Fatalf("turn dispatched on gen %v, end reason %v; want completed on gen 2", got.DispatchedSandboxGen, got.EndReason)
	}
	if prompts := gen1.seenPrompts(); len(prompts) != 0 {
		t.Fatalf("gen 1 was sent %d prompts, want none: it was under its runway floor", len(prompts))
	}
	gen2 := rig.agent(2)
	if gen2 == nil {
		t.Fatal("no gen 2 agent was started")
	}
	if prompts := gen2.seenPrompts(); len(prompts) != 1 || prompts[0].Gen != 2 {
		t.Fatalf("gen 2 prompts = %+v, want the turn, once", prompts)
	}
	waitUntil(t, scenario18Wait, func() bool { ended, _ := gen1.endedWith(); return ended })
	if _, err := gen1.endedWith(); !errors.Is(err, wsbridge.ErrShutdownRequested) {
		t.Fatalf("gen 1's agent ended with %v, want the shutdown it was sent", err)
	}
	stops, restores, creates := rig.provider.recorded()
	if len(stops) != 1 || stops[0] != scenario18Gen1Object || len(restores) != 1 || restores[0] != scenario18Snapshot || creates != 0 {
		t.Fatalf("provider: stops %v, restores %v, creates %d; want gen 1 stopped and its rotation snapshot restored", stops, restores, creates)
	}
	row := rig.sandbox(ctx, t)
	if row.Gen != 2 || row.RotationGen != nil || row.LifetimeDeadlineGen == nil || *row.LifetimeDeadlineGen != 2 ||
		!row.LifetimeDeadlineAt.Valid || time.Until(row.LifetimeDeadlineAt.Time) < 110*time.Minute {
		t.Fatalf("sandbox gen %d rotation %v deadline gen %v at %v; want gen 2 with a fresh 2h deadline", row.Gen, row.RotationGen, row.LifetimeDeadlineGen, row.LifetimeDeadlineAt.Time)
	}
	var failures, warnings int
	if err := rig.h.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE type = 'execution_complete' AND payload->>'outcome' IS DISTINCT FROM 'completed'),
		count(*) FILTER (WHERE type = 'warning')
		FROM events WHERE session_id = $1`, rig.sessionID).Scan(&failures, &warnings); err != nil {
		t.Fatal(err)
	}
	if failures != 0 || warnings != 0 {
		t.Fatalf("%d failed execution_complete and %d warnings, want neither: a rotation is never a failure", failures, warnings)
	}
}

// handshakeStatus is what the sandbox socket answers a handshake that
// presents gen and token, without upgrading.
func (r *scenario18Rig) handshakeStatus(t *testing.T, gen int, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.serverURL+"/sessions/"+r.sessionID.String()+"/ws?type=sandbox", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Sandbox-ID", "sbx-scenario18-old")
	req.Header.Set("X-Sandbox-Gen", fmt.Sprint(gen))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// mintStatus is what the snapshot mint answers gen and token.
func (r *scenario18Rig) mintStatus(t *testing.T, gen int, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.serverURL+"/mint/"+r.sessionID.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Sandbox-Gen", fmt.Sprint(gen))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestRunwayRotation_SupersededGenLockedOutAtOnce: from the commit that
// retires the rotated gen, its identity is locked out -- its handshake and
// its snapshot mint answer 410 while the row is stopped -- and once the
// restore claim has replaced the gen and its token, its handshake and mint
// answer 403 and a frame it still sends is fenced off by gen, not stored
// (technical plan §35.3, §9.3 scenario 6). The restore is held back by the
// spawn circuit breaker to see the stopped row.
func TestRunwayRotation_SupersededGenLockedOutAtOnce(t *testing.T) {
	ctx := context.Background()
	rig := newScenario18Rig(ctx, t, true)
	gen1 := rig.startGen1(ctx, t)

	turnID := rig.queueTurn(ctx, t)
	waitUntil(t, scenario18Wait, func() bool { return rig.sandbox(ctx, t).Status == sqlcgen.SandboxStatusStopped })
	waitUntil(t, scenario18Wait, func() bool { ended, _ := gen1.endedWith(); return ended })
	if row := rig.sandbox(ctx, t); row.Gen != 1 {
		t.Fatalf("stopped at gen %d, want gen 1 retired and the restore held back", row.Gen)
	}
	if got := rig.handshakeStatus(t, 1, scenario18Gen1Token); got != http.StatusGone {
		t.Fatalf("gen 1's handshake after its retirement = %d, want 410", got)
	}
	if got := rig.mintStatus(t, 1, scenario18Gen1Token); got != http.StatusGone {
		t.Fatalf("gen 1's snapshot mint after its retirement = %d, want 410", got)
	}

	rig.closeBreaker(ctx, t)
	rig.ensureDispatched(ctx, t)
	waitUntil(t, scenario18Wait, func() bool {
		got, err := rig.h.Turns.Get(ctx, turnID)
		return err == nil && got.Status == sqlcgen.TurnStatusCompleted
	})
	if row := rig.sandbox(ctx, t); row.Gen != 2 {
		t.Fatalf("sandbox at gen %d after the restore, want 2", row.Gen)
	}
	if got := rig.handshakeStatus(t, 1, scenario18Gen1Token); got != http.StatusForbidden {
		t.Fatalf("gen 1's handshake after the restore = %d, want 403", got)
	}
	if got := rig.mintStatus(t, 1, scenario18Gen1Token); got != http.StatusForbidden {
		t.Fatalf("gen 1's snapshot mint after the restore = %d, want 403", got)
	}
	actor, err := rig.registry.GetOrSpawn(ctx, rig.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	const lateID = "late-gen-1-heartbeat"
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "heartbeat", Gen: 1, MessageID: lateID, Reply: reply,
		Raw: []byte(`{"type":"heartbeat","messageId":"` + lateID + `","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-reply:
		if outcome.Persisted {
			t.Fatal("gen 1's late frame was stored, want it fenced off by gen")
		}
	case <-time.After(scenario18Wait):
		t.Fatal("no outcome for gen 1's late frame")
	}
	var stored int
	if err := rig.h.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND message_id = $2`, rig.sessionID, lateID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("gen 1's late frame stored %d times (%v), want none", stored, err)
	}
}

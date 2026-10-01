//go:build integration

package resilience_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is the exit of the durable dispatch trigger (technical plan §2,
// §3.3): every path that creates a turn arms the session's dispatch timer
// in its own transaction, so a post-commit trigger that fails costs
// latency, not the turn. Each scenario drives a real turn-creating path --
// createTurnLocked through httpapi.CreateTurnCore, session creation through
// httpapi.CreateSessionCore and CreateSessionOnTx -- against real
// registries, their real actors and their real timer pumps, on one
// database: two "replicas", one of which cannot deliver the trigger.

// recordingProvider is a ports.SandboxProvider that records each
// CreateSandbox call -- the dispatch evaluation of a pending turn on a
// session with no sandbox -- and succeeds.
type recordingProvider struct {
	mu      sync.Mutex
	creates int
}

var _ ports.SandboxProvider = (*recordingProvider)(nil)

func (p *recordingProvider) Capabilities() ports.Capabilities { return ports.Capabilities{} }
func (p *recordingProvider) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	return ports.SandboxRef{ProviderID: uuid.NewString()}, nil
}
func (p *recordingProvider) StopSandbox(context.Context, ports.SandboxRef) error   { return nil }
func (p *recordingProvider) ResumeSandbox(context.Context, ports.SandboxRef) error { return nil }
func (p *recordingProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", errors.New("recordingProvider: no snapshots")
}
func (p *recordingProvider) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	return ports.SandboxRef{}, errors.New("recordingProvider: no snapshots")
}
func (p *recordingProvider) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	return ports.BuildOutcome{}, errors.New("recordingProvider: no image builds")
}
func (p *recordingProvider) DeleteImage(context.Context, ports.ImageRef) error { return nil }
func (p *recordingProvider) List(context.Context) ([]ports.SandboxRef, error)  { return nil, nil }

func (p *recordingProvider) createCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

// promptRecorder is a ports.SandboxCommander counting the prompt commands
// it is asked to send -- the turns actually dispatched to a live sandbox.
type promptRecorder struct {
	mu      sync.Mutex
	prompts int
}

var _ ports.SandboxCommander = (*promptRecorder)(nil)

func (c *promptRecorder) SendCommand(_ string, payload json.RawMessage) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if head.Type == "prompt" {
		c.prompts++
	}
	return nil
}

func (c *promptRecorder) promptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prompts
}

// replica is one "pod": a registry over the harness's database, with its
// own fake provider and commander.
type replica struct {
	registry  *sessionactor.Registry
	provider  *recordingProvider
	commander *promptRecorder
}

// newReplica builds a replica. hydrateBound, when non-zero, replaces
// ActorHydrateTimeout: time.Nanosecond makes every hydration on it fail
// with sessionactor.ErrActorUnavailable, a replica that cannot host actors
// right now.
func newReplica(ctx context.Context, t *testing.T, h *Harness, timeouts platform.Timeouts, hydrateBound time.Duration) *replica {
	t.Helper()
	if hydrateBound != 0 {
		timeouts.ActorHydrateTimeout = hydrateBound
	}
	rep := &replica{provider: &recordingProvider{}, commander: &promptRecorder{}}
	r, err := sessionactor.NewRegistry(ctx, h.Pool, timeouts, h.Hub, rep.commander, rep.provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("sessionactor.NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	rep.registry = r
	return rep
}

// hasDispatchTimer reports whether sessionID holds its dispatch timer.
func hasDispatchTimer(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) bool {
	t.Helper()
	_, err := h.Timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: sessionactor.TimerDispatch})
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("get the dispatch timer: %v", err)
	}
	return true
}

// otherTimers counts sessionID's timers other than its dispatch timer.
func otherTimers(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name <> $2`, sessionID, sessionactor.TimerDispatch).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// createTurnThroughREST creates a turn on an existing session through
// createTurnLocked -- the core the web, Slack, Linear, the code-host mention
// and MCP reach -- whose post-commit trigger runs on via.
func createTurnThroughREST(ctx context.Context, t *testing.T, h *Harness, via *sessionactor.Registry, sessionID pgtype.UUID) {
	t.Helper()
	if _, _, cerr := httpapi.CreateTurnCore(ctx, h.Pool, h.Sessions, h.Turns, nil, nil, narvipg.NewAuditLogStore(h.Pool), via, sessionID,
		"do the thing", nil, false, false, pgtype.UUID{}, httpapi.RejectIfOpen); cerr != nil {
		t.Fatalf("CreateTurnCore: %d %s", cerr.Status, cerr.Message)
	}
}

// createSessionRequest is a session a code-host ingress would create, with
// a first turn: GitHub-sourced with no human creator, so the repository
// entitlement gate admits it without a user row (technical plan §31.4).
func createSessionRequest() restdtos.CreateSessionRequest {
	prompt := "do the thing"
	return restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "widgets", Url: "https://github.com/acme/widgets"},
		},
	}
}

// createSessionThroughCore creates a session and its first turn through
// httpapi.CreateSessionCore, whose post-commit trigger runs on via.
func createSessionThroughCore(ctx context.Context, t *testing.T, h *Harness, via *sessionactor.Registry) pgtype.UUID {
	t.Helper()
	created, cerr := httpapi.CreateSessionCore(ctx, h.Pool, h.Sessions, h.Turns, narvipg.NewEnvironmentStore(h.Pool), narvipg.NewAuditLogStore(h.Pool), via,
		createSessionRequest(), pgtype.UUID{}, false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(h.Pool), narvipg.NewGitHubPRSessionStore(h.Pool))
	if cerr != nil {
		t.Fatalf("CreateSessionCore: %d %s", cerr.Status, cerr.Message)
	}
	return created.ID
}

// TestDurableDispatchTrigger_AFailedTriggerIsDeliveredWhenAHostClaimsItsTimer:
// a turn created while its trigger fails, on a session with no sandbox and
// no other timer, is dispatched by the first replica able to host the
// actor that claims its dispatch timer. Here the replica whose trigger
// failed claims it first and fails again, keeping the row, and the hosting
// replica wins the single lapsed claim that follows, so the turn is
// dispatched within one claim window of the failed claim. That is all this
// case shows, and all the pump promises: each lapsed claim is raced by
// every replica's pump, and a replica that cannot host the actor may win it
// again, each such win costing one more claim window. "Dispatched" here is
// the dispatch evaluation of a turn with no sandbox: a spawn, on the
// hosting replica, exactly once.
func TestDurableDispatchTrigger_AFailedTriggerIsDeliveredWhenAHostClaimsItsTimer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	timeouts := h.Timeouts
	timeouts.TimerClaimDuration = 2 * time.Second

	for _, tc := range []struct {
		name string
		// elsewhere: the hosting replica already hosts the session's
		// actor, so the other answers actor_elsewhere. Otherwise the other
		// cannot host actors at all: actor_unavailable.
		elsewhere bool
		create    func(t *testing.T, via *sessionactor.Registry, sessionID pgtype.UUID) pgtype.UUID
	}{
		{"a turn on an existing session, actor_elsewhere", true, func(t *testing.T, via *sessionactor.Registry, sessionID pgtype.UUID) pgtype.UUID {
			createTurnThroughREST(ctx, t, h, via, sessionID)
			return sessionID
		}},
		{"a turn on an existing session, actor_unavailable", false, func(t *testing.T, via *sessionactor.Registry, sessionID pgtype.UUID) pgtype.UUID {
			createTurnThroughREST(ctx, t, h, via, sessionID)
			return sessionID
		}},
		{"a new session's first turn, actor_unavailable", false, func(t *testing.T, via *sessionactor.Registry, _ pgtype.UUID) pgtype.UUID {
			return createSessionThroughCore(ctx, t, h, via)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newReplica(ctx, t, h, timeouts, 0)
			bound := time.Duration(0)
			if !tc.elsewhere {
				bound = time.Nanosecond
			}
			failing := newReplica(ctx, t, h, timeouts, bound)

			existing := h.CreateSession(ctx, t)
			if tc.elsewhere {
				if _, err := host.registry.GetOrSpawn(ctx, existing); err != nil {
					t.Fatalf("host GetOrSpawn: %v", err)
				}
			}
			sessionID := tc.create(t, failing.registry, existing)

			if !hasDispatchTimer(ctx, t, h, sessionID) {
				t.Fatal("the turn was committed without its dispatch timer")
			}
			if n := otherTimers(ctx, t, h, sessionID); n != 0 {
				t.Fatalf("session holds %d other timers, want none", n)
			}
			if _, err := h.Sandboxes.Get(ctx, sessionID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("session has a sandbox (err %v): the failed trigger dispatched after all", err)
			}

			claimedAt := time.Now()
			if err := failing.registry.PumpOnce(ctx); err != nil {
				t.Fatalf("failing replica PumpOnce: %v", err)
			}
			if !hasDispatchTimer(ctx, t, h, sessionID) {
				t.Fatal("the replica that could not deliver the dispatch timer dropped it")
			}
			waitUntil(t, timeouts.TimerClaimDuration+time.Second, func() bool {
				if err := host.registry.PumpOnce(ctx); err != nil {
					t.Fatalf("host PumpOnce: %v", err)
				}
				return host.provider.createCount() == 1
			})
			if elapsed := time.Since(claimedAt); elapsed > timeouts.TimerClaimDuration+time.Second {
				t.Fatalf("dispatched %v after the failed claim, want within the one claim window (%v) the hosting replica's winning claim follows", elapsed, timeouts.TimerClaimDuration)
			}
			waitUntil(t, 5*time.Second, func() bool { return !hasDispatchTimer(ctx, t, h, sessionID) })
			if n := failing.provider.createCount(); n != 0 {
				t.Fatalf("the replica that could not host spawned %d sandboxes", n)
			}
			if n := host.provider.createCount(); n != 1 {
				t.Fatalf("the hosting replica spawned %d sandboxes, want 1", n)
			}
		})
	}
}

// TestDurableDispatchTrigger_AReplicaThatDiesBeforeItsTriggerLosesNothing:
// the transaction that creates a session and its first turn commits, and
// the replica dies before its post-commit trigger -- simulated by
// committing CreateSessionOnTx's transaction and never triggering. Another
// replica's next pump tick delivers the dispatch.
func TestDurableDispatchTrigger_AReplicaThatDiesBeforeItsTriggerLosesNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	survivor := newReplica(ctx, t, h, h.Timeouts, 0)

	req := createSessionRequest()
	auditLog := narvipg.NewAuditLogStore(h.Pool)
	entitlement, cerr := httpapi.ResolveRepoEntitlement(ctx, narvipg.NewGitHubPRSessionStore(h.Pool), auditLog, pgtype.UUID{}, req)
	if cerr != nil {
		t.Fatalf("ResolveRepoEntitlement: %d %s", cerr.Status, cerr.Message)
	}
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, hasPrompt, cerr := httpapi.CreateSessionOnTx(ctx, tx, h.Sessions, h.Turns, narvipg.NewEnvironmentStore(h.Pool), auditLog, req, pgtype.UUID{}, false,
		platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(h.Pool), entitlement)
	if cerr != nil || !hasPrompt {
		t.Fatalf("CreateSessionOnTx: %v, hasPrompt %v", cerr, hasPrompt)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// ...and the replica is gone: no TriggerDispatch.

	if !hasDispatchTimer(ctx, t, h, created.ID) {
		t.Fatal("the session's first turn was committed without its dispatch timer")
	}
	if err := survivor.registry.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return survivor.provider.createCount() == 1 })
	waitUntil(t, 5*time.Second, func() bool { return !hasDispatchTimer(ctx, t, h, created.ID) })
}

// TestDurableDispatchTrigger_ASucceededTriggerDispatchesOnceAndTheStatusSettles:
// a turn whose post-commit trigger succeeds is sent to its ready sandbox
// once and never again -- not by either replica's pump, not by a late
// delivery of the timer to its actor -- and once it has ended, the
// session's status (GET /api/sessions/{sessionID}/status) settles: no
// dispatch timer lingers.
func TestDurableDispatchTrigger_ASucceededTriggerDispatchesOnceAndTheStatusSettles(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	host := newReplica(ctx, t, h, h.Timeouts, 0)
	other := newReplica(ctx, t, h, h.Timeouts, 0)

	sessionID := h.CreateSession(ctx, t)
	if _, err := h.Sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = 1 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}

	createTurnThroughREST(ctx, t, h, host.registry, sessionID)
	waitUntil(t, 5*time.Second, func() bool { return host.commander.promptCount() == 1 })
	waitUntil(t, 5*time.Second, func() bool { return !hasDispatchTimer(ctx, t, h, sessionID) })

	actor, err := host.registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	// Both pumps tick, and a claim the pump took before the trigger's
	// evaluation deleted the row is delivered late.
	if err := other.registry.PumpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.registry.PumpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := actor.Send(ctx, sessionactor.TimerFired{Name: sessionactor.TimerDispatch}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := host.commander.promptCount() + other.commander.promptCount(); n != 1 {
		t.Fatalf("prompts sent = %d, want 1: a turn whose trigger succeeded was dispatched twice", n)
	}

	// The turn ends.
	messageID := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: messageID, SessionId: sessionID.String(), Gen: 1,
		AckId: "execution_complete:" + messageID, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reply:
	case <-time.After(5 * time.Second):
		t.Fatal("execution_complete was not handled")
	}

	router := chi.NewRouter()
	router.Get("/api/sessions/{sessionID}/status", httpapi.GetSessionStatus(h.Sessions, nil, h.Timeouts))
	var status restdtos.SessionActivity
	waitUntil(t, 5*time.Second, func() bool {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sessionID.String()+"/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return status.Settled
	})
	if status.Activity != restdtos.SessionActivityActivityFinished {
		t.Fatalf("activity = %s after the turn ended, want %s", status.Activity, restdtos.SessionActivityActivityFinished)
	}
	if hasDispatchTimer(ctx, t, h, sessionID) {
		t.Fatal("a dispatch timer lingers after a normal turn")
	}
}

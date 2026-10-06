//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/platform"
)

// Integration tests for POST /api/sessions/{sessionID}/stop (technical plan
// §3.3): real Postgres, the real session actor and timer pump, the real
// route behind the real auth middleware, a fake sandbox that records every
// command frame the actor sends it, and -- where a test needs the actor to
// replace a sandbox -- a fake provider that records every call.

// stopProvider is the fake sandbox provider: it records every spawn,
// restore and stop, and answers each spawn or restore with the provider
// object "provider-gen-<gen>". Snapshots and an explicit stop are
// supported, as on Modal; resume is not. docker also reports Docker in the
// sandbox, which a Docker-required session's spawn needs, as Modal does.
type stopProvider struct {
	docker bool

	mu       sync.Mutex
	created  []int
	restored []int
	stopped  []string
}

var _ ports.SandboxProvider = (*stopProvider)(nil)

var errStopProviderUnsupported = errors.New("stopProvider: not supported")

func (p *stopProvider) Capabilities() ports.Capabilities {
	return ports.Capabilities{Snapshots: true, ExplicitStop: true, DockerInSandbox: p.docker}
}

func (p *stopProvider) CreateSandbox(_ context.Context, spec ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, spec.Gen)
	return ports.SandboxRef{ProviderID: providerIDForGen(spec.Gen)}, nil
}

func (p *stopProvider) RestoreFromSnapshot(_ context.Context, _ ports.SnapshotID, spec ports.CreateSpec) (ports.SandboxRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restored = append(p.restored, spec.Gen)
	return ports.SandboxRef{ProviderID: providerIDForGen(spec.Gen)}, nil
}

func (p *stopProvider) StopSandbox(_ context.Context, ref ports.SandboxRef) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = append(p.stopped, ref.ProviderID)
	return nil
}

func (p *stopProvider) ResumeSandbox(context.Context, ports.SandboxRef) error {
	return errStopProviderUnsupported
}

func (p *stopProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	return "", errStopProviderUnsupported
}

func (p *stopProvider) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	return ports.BuildOutcome{}, errStopProviderUnsupported
}

func (p *stopProvider) DeleteImage(context.Context, ports.ImageRef) error {
	return errStopProviderUnsupported
}

func (p *stopProvider) List(context.Context) ([]ports.SandboxRef, error) { return nil, nil }

// calls returns a copy of what the provider was asked so far.
func (p *stopProvider) calls() (created, restored []int, stopped []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.created), slices.Clone(p.restored), slices.Clone(p.stopped)
}

// providerIDForGen is the provider object the fakes record for gen.
func providerIDForGen(gen int) string { return fmt.Sprintf("provider-gen-%d", gen) }

// stopCommander is the fake sandbox: a ports.SandboxCommander recording
// every frame, in order.
type stopCommander struct {
	mu     sync.Mutex
	frames []json.RawMessage
}

var _ ports.SandboxCommander = (*stopCommander)(nil)

func (c *stopCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, append(json.RawMessage(nil), payload...))
	return nil
}

// types returns the "type" of every recorded frame, in order, from the
// from-th on.
func (c *stopCommander) types(t *testing.T, from int) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, raw := range c.frames[min(from, len(c.frames)):] {
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode command frame %s: %v", raw, err)
		}
		out = append(out, frame.Type)
	}
	return out
}

// count returns how many frames have been recorded.
func (c *stopCommander) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// ofType decodes every recorded frame whose "type" is typ.
func (c *stopCommander) ofType(t *testing.T, typ string) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, raw := range c.frames {
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode command frame %s: %v", raw, err)
		}
		if frame["type"] == typ {
			out = append(out, frame)
		}
	}
	return out
}

// stopRig is one replica: its registry (actor host and timer pump) with
// the fake sandbox, and a server mounting the stop route beside the turn,
// plan-approval and workflow-step decision routes a person resumes a
// session through.
type stopRig struct {
	pool         *pgxpool.Pool
	sessions     *narvipg.SessionStore
	turns        *narvipg.TurnStore
	sandboxes    *narvipg.SandboxStore
	timers       *narvipg.TimerStore
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	userSessions *narvipg.UserSessionStore
	participants *narvipg.ParticipantStore
	plans        *narvipg.PlanStore
	auditLog     *narvipg.AuditLogStore
	workflows    *narvipg.WorkflowStore
	prSessions   *narvipg.GitHubPRSessionStore
	timeouts     platform.Timeouts
	provider     ports.SandboxProvider
	commander    *stopCommander
	registry     *sessionactor.Registry
	server       *httptest.Server
}

// stopRigConfig tunes a rig: grace overrides StopGrace (0 keeps the shipped
// 30s); noWake mounts the route with no registry, so nothing but the timer
// pump -- or a dispatch -- reaches the actor; provider, when set, is every
// replica's sandbox provider (none otherwise, so no sandbox is ever
// spawned); walk, when set, overrides StopDescendantWalkTimeout.
type stopRigConfig struct {
	grace    time.Duration
	noWake   bool
	provider ports.SandboxProvider
	walk     time.Duration
}

func newStopRig(t *testing.T, cfg stopRigConfig) *stopRig {
	t.Helper()
	pool := newTestPool(t)

	timeouts := platform.DefaultTimeouts()
	if cfg.grace > 0 {
		timeouts.StopGrace = cfg.grace
	}
	if cfg.walk > 0 {
		timeouts.StopDescendantWalkTimeout = cfg.walk
	}
	if err := timeouts.Validate(); err != nil {
		t.Fatalf("timeouts: %v", err)
	}
	r := &stopRig{
		pool:         pool,
		sessions:     narvipg.NewSessionStore(pool),
		turns:        narvipg.NewTurnStore(pool),
		sandboxes:    narvipg.NewSandboxStore(pool),
		timers:       narvipg.NewTimerStore(pool),
		users:        narvipg.NewUserStore(pool),
		identities:   narvipg.NewIdentityStore(pool),
		userSessions: narvipg.NewUserSessionStore(pool),
		participants: narvipg.NewParticipantStore(pool),
		plans:        narvipg.NewPlanStore(pool),
		auditLog:     narvipg.NewAuditLogStore(pool),
		workflows:    narvipg.NewWorkflowStore(pool),
		prSessions:   narvipg.NewGitHubPRSessionStore(pool),
		timeouts:     timeouts,
		provider:     cfg.provider,
	}
	r.commander, r.registry, _ = r.newReplica(t)

	routeRegistry := r.registry
	if cfg.noWake {
		routeRegistry = nil
	}
	router := chi.NewRouter()
	router.Route("/api/sessions", func(api chi.Router) {
		api.Use(auth.Middleware(r.userSessions, r.users))
		api.Post("/{sessionID}/stop", httpapi.StopSession(httpapi.StopSessionDeps{
			Pool:             pool,
			Sessions:         r.sessions,
			Turns:            r.turns,
			Timers:           r.timers,
			Participants:     r.participants,
			AuditLog:         r.auditLog,
			GitHubPRSessions: r.prSessions,
			Registry:         routeRegistry,
			Timeouts:         timeouts,
		}))
		api.Post("/{sessionID}/turns", httpapi.CreateTurn(pool, r.sessions, r.turns, r.plans, r.participants, r.auditLog, r.registry, turnguard.New(pool, nil, false), nil, nil, false))
		api.Post("/{sessionID}/plans/{planId}/approve", httpapi.ApprovePlan(pool, r.sessions, r.turns, r.plans, narvipg.NewEventStore(pool), narvipg.NewPlanDocumentStore(pool), r.participants, narvipg.NewOutboxStore(pool, false), narvipg.NewLinearAgentSessionStore(pool), r.auditLog, r.registry, turnguard.New(pool, nil, false), false))
	})
	// Mounted as controlplane/serve.go mounts it.
	router.Route("/api/workflow-runs", func(api chi.Router) {
		api.Use(auth.Middleware(r.userSessions, r.users))
		api.Post("/{runId}/steps/{stepRunId}/decide", httpapi.DecideWorkflowStep(pool, r.sessions, r.turns, r.participants, r.workflows, narvipg.NewSlackThreadSessionStore(pool), narvipg.NewLinearAgentSessionStore(pool), r.prSessions, narvipg.NewOutboxStore(pool, false), r.registry, turnguard.New(pool, nil, false), false))
	})
	r.server = httptest.NewServer(router)
	t.Cleanup(r.server.Close)
	return r
}

// newReplica builds another registry on the same database -- another
// replica -- with its own fake sandbox. lose shuts it down, at most once;
// cleanup calls it too, before the database reset.
func (r *stopRig) newReplica(t *testing.T) (commander *stopCommander, registry *sessionactor.Registry, lose func() error) {
	t.Helper()
	commander = &stopCommander{}
	registry, err := sessionactor.NewRegistry(context.Background(), r.pool, r.timeouts, nil, commander, r.provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var once sync.Once
	var shutdownErr error
	lose = func() error {
		once.Do(func() { shutdownErr = registry.Shutdown() })
		return shutdownErr
	}
	t.Cleanup(func() { _ = lose() })
	return commander, registry, lose
}

// user creates a signed-in user with role and returns it with its cookie.
func (r *stopRig) user(ctx context.Context, t *testing.T, role sqlcgen.UserRole) (sqlcgen.User, string) {
	t.Helper()
	externalID := fmt.Sprintf("stop-test-%s", uuid.NewString())
	email := externalID + "@example.com"
	user, err := r.users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: "Stop Test", Role: role})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := r.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := r.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID: user.ID, TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(r.timeouts.UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	return user, token
}

// session creates a session created by createdBy (invalid for a bot's), a
// child of parent when parent is valid.
func (r *stopRig) session(ctx context.Context, t *testing.T, createdBy, parent pgtype.UUID) sqlcgen.Session {
	t.Helper()
	params := sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: createdBy}
	if parent.Valid {
		params.SpawnSource = sqlcgen.SessionSpawnSourceGithub
		params.ParentSessionID = parent
		params.SpawnDepth = int32(1)
	}
	row, err := r.sessions.Create(ctx, params)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return row
}

// readySandbox gives sessionID a live sandbox at gen, backed by the provider
// object providerIDForGen(gen).
func (r *stopRig) readySandbox(ctx context.Context, t *testing.T, sessionID pgtype.UUID, gen int32) {
	t.Helper()
	if _, err := r.sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = $2, provider_id = $3 WHERE session_id = $1`, sessionID, gen, providerIDForGen(int(gen))); err != nil {
		t.Fatalf("make sandbox ready: %v", err)
	}
}

// sandboxRow returns sessionID's sandbox.
func (r *stopRig) sandboxRow(ctx context.Context, t *testing.T, sessionID pgtype.UUID) sqlcgen.Sandbox {
	t.Helper()
	row, err := r.sandboxes.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row
}

// promptGens returns the gen of every prompt frame sent, in order.
func (c *stopCommander) promptGens(t *testing.T) []int {
	t.Helper()
	var gens []int
	for _, frame := range c.ofType(t, "prompt") {
		gen, _ := frame["gen"].(float64)
		gens = append(gens, int(gen))
	}
	return gens
}

// pendingTurn inserts a pending turn.
func (r *stopRig) pendingTurn(ctx context.Context, t *testing.T, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	prompt := "queued work"
	row, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create pending turn: %v", err)
	}
	return row
}

// processingTurn inserts a turn processing on the sandbox at gen, with its
// turn_deadline armed as a real dispatch arms it.
func (r *stopRig) processingTurn(ctx context.Context, t *testing.T, sessionID pgtype.UUID, gen int32) sqlcgen.Turn {
	t.Helper()
	prompt := "running work"
	row, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing, Prompt: &prompt})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE turns SET dispatched_sandbox_gen = $2, dispatched_at = now() WHERE id = $1`, row.ID, gen); err != nil {
		t.Fatalf("stamp dispatch: %v", err)
	}
	if _, err := r.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID, Name: sessionactor.TimerTurnDeadline,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(r.timeouts.TurnDeadline), Valid: true},
	}); err != nil {
		t.Fatalf("arm turn_deadline: %v", err)
	}
	return row
}

// post sends method path with the cookie and an optional JSON body,
// returning the status and the raw body.
func (r *stopRig) post(t *testing.T, path, token string, body []byte) (int, []byte) {
	t.Helper()
	status, raw, err := r.postRequest(path, token, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, raw
}

// postRequest is post without t, for a goroutine.
func (r *stopRig) postRequest(path, token string, body []byte) (int, []byte, error) {
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, r.server.URL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// prompt creates a person's turn on sessionID through the REST route and
// returns its id, failing unless the route answers 201.
func (r *stopRig) prompt(t *testing.T, sessionID pgtype.UUID, token, text string) pgtype.UUID {
	t.Helper()
	body, err := json.Marshal(map[string]any{"prompt": text, "modelId": nil, "effort": nil, "planMode": false})
	if err != nil {
		t.Fatal(err)
	}
	status, raw := r.post(t, "/api/sessions/"+sessionID.String()+"/turns", token, body)
	if status != http.StatusCreated {
		t.Fatalf("prompt %q: status %d %s", text, status, raw)
	}
	var created restdtos.CreateTurnResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	var id pgtype.UUID
	if err := id.Scan(created.Id); err != nil {
		t.Fatal(err)
	}
	return id
}

// deliveringSession creates a session created by createdBy, under
// environmentID when it is valid, whose one repo has an explicit branch and
// live egress: a turn of it that completes is really sent its push, and
// its delivery stamped (sandboxes.pr_delivery_started_at) until the push
// reports back.
func (r *stopRig) deliveringSession(ctx context.Context, t *testing.T, createdBy, environmentID pgtype.UUID) sqlcgen.Session {
	t.Helper()
	repos, err := json.Marshal([]map[string]any{{"name": "widgets", "url": "https://github.com/example-org/widgets.git", "branch": "narvi/feature"}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: createdBy, Repos: repos, EnvironmentID: environmentID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := narvipg.NewRepoSettingsStore(r.pool).UpsertLiveEgressEnabled(ctx, "example-org/widgets", true); err != nil {
		t.Fatal(err)
	}
	return session
}

// stop POSTs the stop route and decodes a 202's body.
func (r *stopRig) stop(t *testing.T, sessionID, token string) (int, restdtos.StopSessionResponse) {
	t.Helper()
	status, resp, err := r.stopRequest(sessionID, token)
	if err != nil {
		t.Fatal(err)
	}
	return status, resp
}

// stopRequest is stop without t, for a goroutine.
func (r *stopRig) stopRequest(sessionID, token string) (int, restdtos.StopSessionResponse, error) {
	var resp restdtos.StopSessionResponse
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/api/sessions/"+sessionID+"/stop", http.NoBody)
	if err != nil {
		return 0, resp, err
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	}
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, resp, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return 0, resp, err
	}
	if httpResp.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return 0, resp, fmt.Errorf("decode StopSessionResponse %s: %w", raw, err)
		}
	}
	return httpResp.StatusCode, resp, nil
}

func (r *stopRig) turnRow(ctx context.Context, t *testing.T, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	row, err := r.turns.Get(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return row
}

func (r *stopRig) sessionRow(ctx context.Context, t *testing.T, id pgtype.UUID) sqlcgen.Session {
	t.Helper()
	row, err := r.sessions.Get(ctx, id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return row
}

// syntheticCompletes counts, per turn id, the synthetic execution_complete
// events recorded on sessionID -- §3.3 owes each cancelled turn exactly
// one terminal event, so a count, not a set.
func (r *stopRig) syntheticCompletes(ctx context.Context, t *testing.T, sessionID pgtype.UUID) map[string]int {
	t.Helper()
	rows, err := r.pool.Query(ctx, `SELECT payload->>'turn_id' FROM events
		WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`, sessionID)
	if err != nil {
		t.Fatalf("query synthetic events: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id]++
	}
	return out
}

// syntheticDispatched returns, per turn id, whether that turn's synthetic
// execution_complete carries the `dispatched` stamp
// (sessionactor/syntheticend.go): true only for the end of a turn that was
// dispatched, the one a page is showing.
func (r *stopRig) syntheticDispatched(ctx context.Context, t *testing.T, sessionID pgtype.UUID) map[string]bool {
	t.Helper()
	rows, err := r.pool.Query(ctx, `SELECT payload->>'turn_id', COALESCE((payload->>'dispatched')::boolean, false) FROM events
		WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`, sessionID)
	if err != nil {
		t.Fatalf("query synthetic events: %v", err)
	}
	out := map[string]bool{}
	var scanErr error
	for rows.Next() {
		var id string
		var dispatched bool
		if err := rows.Scan(&id, &dispatched); err != nil {
			scanErr = err
			break
		}
		out[id] = dispatched
	}
	rows.Close()
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	return out
}

// timerNames returns the timers armed on sessionID, sorted.
func (r *stopRig) timerNames(ctx context.Context, t *testing.T, sessionID pgtype.UUID) []string {
	t.Helper()
	rows, err := r.timers.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list timers: %v", err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}

// stopTimer returns when sessionID's stop timer fires, and whether it is
// armed at all.
func (r *stopRig) stopTimer(ctx context.Context, t *testing.T, sessionID pgtype.UUID) (time.Time, bool) {
	t.Helper()
	timer, err := r.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: sessionactor.TimerStop})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("get stop timer: %v", err)
	}
	return timer.FiresAt.Time, true
}

// stopAudits returns the detail of every session.stop audit row for
// sessionID.
func (r *stopRig) stopAudits(ctx context.Context, t *testing.T, sessionID pgtype.UUID) []map[string]any {
	t.Helper()
	rows, err := r.pool.Query(ctx, `SELECT detail_json FROM audit_log WHERE action = 'session.stop' AND resource_id = $1`, sessionID.String())
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

// agentReports delivers the agent's execution_complete for gen to the
// session's actor on registry and waits for it to be handled.
func agentReports(ctx context.Context, t *testing.T, registry *sessionactor.Registry, sessionID pgtype.UUID, gen int, outcome sandboxws.ExecutionCompleteOutcome) {
	t.Helper()
	messageID := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: messageID, SessionId: sessionID.String(),
		Gen: gen, AckId: "execution_complete:" + messageID, Outcome: outcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "execution_complete", Gen: gen, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("send execution_complete: %v", err)
	}
	select {
	case <-reply:
	case <-time.After(10 * time.Second):
		t.Fatal("execution_complete not handled")
	}
	actorBarrier(ctx, t, registry, sessionID)
}

// agentEvent delivers one agent event of type typ at gen, built by build
// from a fresh messageId, to the session's actor on registry, and waits
// for it to be handled, the post-commit work it starts included.
func agentEvent(ctx context.Context, t *testing.T, registry *sessionactor.Registry, sessionID pgtype.UUID, typ string, gen int, build func(messageID string) any) {
	t.Helper()
	messageID := uuid.NewString()
	raw, err := json.Marshal(build(messageID))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: typ, Gen: gen, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
	select {
	case <-reply:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s not handled", typ)
	}
	actorBarrier(ctx, t, registry, sessionID)
}

// agentSnapshotReady answers the snapshot a turn's end started, as the
// agent does once it has minted it: waits for the sandbox to be
// snapshotting, then sends snapshot_ready at gen echoing the last snapshot
// command's messageId, which moves the sandbox back to ready and
// dispatches whatever is queued.
func (r *stopRig) agentSnapshotReady(ctx context.Context, t *testing.T, sessionID pgtype.UUID, gen int) {
	t.Helper()
	stopEventually(t, 10*time.Second, "the snapshot a turn's end starts", func() bool {
		return r.sandboxRow(ctx, t, sessionID).Status == sqlcgen.SandboxStatusSnapshotting
	})
	commands := r.commander.ofType(t, "snapshot")
	if len(commands) == 0 {
		t.Fatal("the sandbox is snapshotting but no snapshot command was sent")
	}
	command, _ := commands[len(commands)-1]["messageId"].(string)
	agentEvent(ctx, t, r.registry, sessionID, "snapshot_ready", gen, func(messageID string) any {
		return sandboxws.SnapshotReady{
			Type: "snapshot_ready", MessageId: messageID, SessionId: sessionID.String(), Gen: gen,
			AckId: "snapshot_ready:" + messageID, CommandMessageId: &command, SnapshotId: "snapshot-" + messageID,
		}
	})
	if sb := r.sandboxRow(ctx, t, sessionID); sb.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("sandbox = %s after snapshot_ready, want ready", sb.Status)
	}
}

// actorBarrier returns once the session's actor on registry has finished
// every command sent to it before this call, the post-commit work of each
// included -- the snapshot, the dispatch and the push a sandbox event
// starts after the actor has already replied to it. The actor handles one
// command at a time, so the reply to a command sent now comes only after
// those. The command is an event from no gen a sandbox ever has, which the
// gen fence drops with no effect.
func actorBarrier(ctx context.Context, t *testing.T, registry *sessionactor.Registry, sessionID pgtype.UUID) {
	t.Helper()
	actor, err := registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	messageID := uuid.NewString()
	raw, err := json.Marshal(map[string]any{"type": "heartbeat", "messageId": messageID, "sessionId": sessionID.String(), "gen": -1})
	if err != nil {
		t.Fatal(err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "heartbeat", Gen: -1, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("send barrier: %v", err)
	}
	select {
	case <-reply:
	case <-time.After(10 * time.Second):
		t.Fatal("barrier not handled")
	}
}

// stopEventually polls cond every 25ms until it holds, failing after timeout.
func stopEventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", timeout, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// pumpUntil runs registry's timer pump every 50ms until cond holds.
func pumpUntil(ctx context.Context, t *testing.T, registry *sessionactor.Registry, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	stopEventually(t, timeout, what, func() bool {
		if err := registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		return cond()
	})
}

// TestStopSession_AuthzMatrix pins who may stop a session (technical plan
// §13.3, owner decision O1): admin and maintainer any session, a member
// their own or joined ones, a viewer none. A pull request's review session
// (a github_pr_sessions row points at it) is the exception to the member
// rule: whoever first mentioned the bot on the PR is its creator, and its
// work is everyone's, so stopping it takes admin or maintainer. A refused
// request writes nothing -- no flag, no timer, no audit row. 400 and 404
// are GET's.
func TestStopSession_AuthzMatrix(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})

	type relation string
	const (
		creator relation = "creator"
		joined  relation = "joined"
		none    relation = "neither"
	)
	var prNumber int32
	for _, tc := range []struct {
		role     sqlcgen.UserRole
		relation relation
		review   bool
		want     int
	}{
		{sqlcgen.UserRoleAdmin, none, false, http.StatusAccepted},
		{sqlcgen.UserRoleAdmin, creator, false, http.StatusAccepted},
		{sqlcgen.UserRoleMaintainer, none, false, http.StatusAccepted},
		{sqlcgen.UserRoleMaintainer, joined, false, http.StatusAccepted},
		{sqlcgen.UserRoleMember, creator, false, http.StatusAccepted},
		{sqlcgen.UserRoleMember, joined, false, http.StatusAccepted},
		{sqlcgen.UserRoleMember, none, false, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, creator, false, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, joined, false, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, none, false, http.StatusForbidden},
		// A pull request's review session: the member rule does not reach it.
		{sqlcgen.UserRoleAdmin, none, true, http.StatusAccepted},
		{sqlcgen.UserRoleMaintainer, none, true, http.StatusAccepted},
		{sqlcgen.UserRoleMaintainer, creator, true, http.StatusAccepted},
		{sqlcgen.UserRoleMember, creator, true, http.StatusForbidden},
		{sqlcgen.UserRoleMember, joined, true, http.StatusForbidden},
		{sqlcgen.UserRoleMember, none, true, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, creator, true, http.StatusForbidden},
	} {
		name := fmt.Sprintf("%s_%s", tc.role, tc.relation)
		if tc.review {
			name += "_review_session"
		}
		t.Run(name, func(t *testing.T) {
			caller, token := rig.user(ctx, t, tc.role)
			owner := caller
			if tc.relation != creator {
				owner, _ = rig.user(ctx, t, sqlcgen.UserRoleMember)
			}
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			if tc.relation == joined {
				if _, err := rig.participants.Create(ctx, session.ID, caller.ID); err != nil {
					t.Fatalf("join: %v", err)
				}
			}
			if tc.review {
				prNumber++
				if _, err := rig.pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ('example-org/widgets', $1, $2)`, prNumber, session.ID); err != nil {
					t.Fatalf("make it a review session: %v", err)
				}
			}
			queued := rig.pendingTurn(ctx, t, session.ID)

			status, resp := rig.stop(t, session.ID.String(), token)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}

			flagged := rig.turnRow(ctx, t, queued.ID).StopRequestedAt.Valid
			sessionFlagged := rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid
			audits := rig.stopAudits(ctx, t, session.ID)
			if tc.want == http.StatusForbidden {
				if flagged || sessionFlagged || len(audits) != 0 || slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop) {
					t.Fatalf("refused stop wrote something: turn flagged %v, session flagged %v, audits %v", flagged, sessionFlagged, audits)
				}
				return
			}
			if !flagged || !sessionFlagged || len(audits) != 1 {
				t.Fatalf("accepted stop: turn flagged %v, session flagged %v, audits %v; want both flagged and one audit", flagged, sessionFlagged, audits)
			}
			if resp.SessionId != session.ID.String() || resp.OpenTurns != 1 || !slices.Equal(resp.ReachedSessionIds, []string{session.ID.String()}) || resp.RequestedAt.IsZero() {
				t.Fatalf("response = %+v", resp)
			}
		})
	}

	_, adminToken := rig.user(ctx, t, sqlcgen.UserRoleAdmin)
	if status, _ := rig.stop(t, "not-a-uuid", adminToken); status != http.StatusBadRequest {
		t.Errorf("malformed id: status = %d, want 400", status)
	}
	if status, _ := rig.stop(t, uuid.NewString(), adminToken); status != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", status)
	}
	if status, _ := rig.stop(t, uuid.NewString(), ""); status != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want 401", status)
	}
}

// TestStopSession_DrainsQueueAndStopsInFlight: a stop cancels every queued
// turn, each with its synthetic execution_complete, and sends the running
// turn's sandbox a `stop` fenced with the sandbox's current gen; the
// agent's cancelled execution_complete then ends it cancelled, with nothing
// pushed and nothing dispatched. The dispatch gate cancels a flagged queued
// turn even when a dispatch runs before the stop timer does.
func TestStopSession_DrainsQueueAndStopsInFlight(t *testing.T) {
	ctx := context.Background()

	t.Run("the stop timer", func(t *testing.T) {
		rig := newStopRig(t, stopRigConfig{})
		owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
		session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
		const gen = 3
		rig.readySandbox(ctx, t, session.ID, gen)
		running := rig.processingTurn(ctx, t, session.ID, gen)
		queued := []sqlcgen.Turn{rig.pendingTurn(ctx, t, session.ID), rig.pendingTurn(ctx, t, session.ID)}

		status, resp := rig.stop(t, session.ID.String(), token)
		if status != http.StatusAccepted || resp.OpenTurns != 3 {
			t.Fatalf("stop: status %d, response %+v; want 202 with 3 open turns", status, resp)
		}

		stopEventually(t, 10*time.Second, "queued turns cancelled and the stop sent", func() bool {
			for _, q := range queued {
				if rig.turnRow(ctx, t, q.ID).Status != sqlcgen.TurnStatusCancelled {
					return false
				}
			}
			return len(rig.commander.ofType(t, "stop")) > 0
		})
		synthetic := rig.syntheticCompletes(ctx, t, session.ID)
		dispatched := rig.syntheticDispatched(ctx, t, session.ID)
		for _, q := range queued {
			if n := synthetic[q.ID.String()]; n != 1 {
				t.Errorf("queued turn %s cancelled with %d synthetic execution_complete events, want exactly 1", q.ID.String(), n)
			}
			// A queued turn never dispatched: its end must not end the
			// running turn a page shows.
			if dispatched[q.ID.String()] {
				t.Errorf("queued turn %s's synthetic end is stamped dispatched, want unstamped: it never dispatched", q.ID.String())
			}
		}
		stops := rig.commander.ofType(t, "stop")
		if len(stops) != 1 || stops[0]["gen"] != float64(gen) || stops[0]["sessionId"] != session.ID.String() {
			t.Fatalf("stop frames = %v, want one for session %s at gen %d", stops, session.ID.String(), gen)
		}
		if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusProcessing {
			t.Fatalf("running turn = %s before the agent answered, want processing", got)
		}
		if names := rig.timerNames(ctx, t, session.ID); !slices.Contains(names, sessionactor.TimerStop) {
			t.Fatalf("timers = %v, want the stop re-armed for the running turn's grace", names)
		}

		agentReports(ctx, t, rig.registry, session.ID, gen, sandboxws.ExecutionCompleteOutcomeCancelled)

		if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusCancelled {
			t.Fatalf("running turn = %s after the agent's cancel, want cancelled", got)
		}
		if n := rig.syntheticCompletes(ctx, t, session.ID)[running.ID.String()]; n != 0 {
			t.Errorf("%d synthetic execution_complete events for the running turn; the agent's own ended it, so none is owed", n)
		}
		if n := len(rig.commander.ofType(t, "push")) + len(rig.commander.ofType(t, "prompt")); n != 0 {
			t.Fatalf("%d push or prompt frames sent, want none", n)
		}
		row := rig.sessionRow(ctx, t, session.ID)
		if row.Status != sqlcgen.SessionStatusCancelled || row.FailureReason == nil || *row.FailureReason != sqlcgen.SessionFailureReasonCancelled {
			t.Fatalf("session = %s/%v, want cancelled/cancelled", row.Status, row.FailureReason)
		}
		if audits := rig.stopAudits(ctx, t, session.ID); len(audits) != 1 || audits[0]["open_turns"] != float64(3) {
			t.Fatalf("audit = %v, want one session.stop with open_turns 3", audits)
		}
	})

	t.Run("a dispatch before the stop timer", func(t *testing.T) {
		rig := newStopRig(t, stopRigConfig{noWake: true})
		owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
		session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
		rig.readySandbox(ctx, t, session.ID, 1)
		queued := rig.pendingTurn(ctx, t, session.ID)

		if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
			t.Fatalf("stop: status %d", status)
		}
		actor, err := rig.registry.GetOrSpawn(ctx, session.ID)
		if err != nil {
			t.Fatalf("GetOrSpawn: %v", err)
		}
		if err := actor.Send(ctx, sessionactor.EnsureDispatched{}); err != nil {
			t.Fatalf("send EnsureDispatched: %v", err)
		}
		stopEventually(t, 10*time.Second, "the flagged queued turn ends", func() bool {
			return rig.turnRow(ctx, t, queued.ID).Status != sqlcgen.TurnStatusPending
		})
		if got := rig.turnRow(ctx, t, queued.ID).Status; got != sqlcgen.TurnStatusCancelled {
			t.Fatalf("queued turn = %s, want cancelled by the dispatch gate", got)
		}
		if prompts := rig.commander.ofType(t, "prompt"); len(prompts) != 0 {
			t.Fatalf("prompt frames = %v, want none: a flagged turn must never dispatch", prompts)
		}
		if n := rig.syntheticCompletes(ctx, t, session.ID)[queued.ID.String()]; n != 1 {
			t.Fatalf("the gate cancelled the turn with %d synthetic execution_complete events, want exactly 1", n)
		}
		// The timer then finds nothing flagged still open and ends itself.
		pumpUntil(ctx, t, rig.registry, 10*time.Second, "the stop timer deletes itself", func() bool {
			return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
		})
	})
}

// TestStopSession_SilentAgentCancelledAfterGrace: when the running turn does
// not end within StopGrace -- the agent stays silent, or there is no
// sandbox to tell -- the stop timer's second fire cancels it with a
// synthetic execution_complete and retires the sandbox gen it ran on
// (stopped, at the same gen: the next dispatch restores or respawns it),
// and a late real execution_complete changes nothing.
func TestStopSession_SilentAgentCancelledAfterGrace(t *testing.T) {
	ctx := context.Background()
	const grace = 2 * time.Second

	for _, tc := range []struct {
		name       string
		hasSandbox bool
	}{
		{"a silent agent", true},
		{"no sandbox to tell", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStopRig(t, stopRigConfig{grace: grace})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			if tc.hasSandbox {
				rig.readySandbox(ctx, t, session.ID, 1)
			}
			running := rig.processingTurn(ctx, t, session.ID, 1)

			requested := time.Now()
			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}
			if tc.hasSandbox {
				stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
			}
			if err := rig.registry.PumpOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusProcessing && time.Since(requested) < grace {
				t.Fatalf("running turn = %s within its grace, want processing", got)
			}

			pumpUntil(ctx, t, rig.registry, 15*time.Second, "the silent turn is cancelled", func() bool {
				return rig.turnRow(ctx, t, running.ID).Status == sqlcgen.TurnStatusCancelled
			})
			if elapsed := time.Since(requested); elapsed < grace {
				t.Fatalf("cancelled after %v, before its %v grace", elapsed, grace)
			}
			if n := rig.syntheticCompletes(ctx, t, session.ID)[running.ID.String()]; n != 1 {
				t.Fatalf("%d synthetic execution_complete events for the cancelled turn, want exactly 1", n)
			}
			if !rig.syntheticDispatched(ctx, t, session.ID)[running.ID.String()] {
				t.Fatal("the running turn's synthetic end carries no dispatched stamp, want it: it ends the turn a page shows")
			}
			if names := rig.timerNames(ctx, t, session.ID); len(names) != 0 {
				t.Fatalf("timers = %v, want none: the stop and the turn's deadline both end with it", names)
			}
			row := rig.sessionRow(ctx, t, session.ID)
			if row.Status != sqlcgen.SessionStatusCancelled {
				t.Fatalf("session = %s, want cancelled", row.Status)
			}

			if tc.hasSandbox {
				if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != sqlcgen.SandboxStatusStopped || sb.Gen != 1 {
					t.Fatalf("sandbox = %s at gen %d after a silent grace, want stopped at gen 1: its gen is retired", sb.Status, sb.Gen)
				}
				agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
				if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusCancelled {
					t.Fatalf("a late execution_complete moved the cancelled turn to %s", got)
				}
			} else if stops := rig.commander.ofType(t, "stop"); len(stops) != 0 {
				t.Fatalf("stop frames = %v with no sandbox, want none", stops)
			}
		})
	}
}

// TestStopSession_PromptAfterStopRuns: a stop cancels only the turns open
// when it was requested. A prompt a person sends afterwards clears the
// session's request and runs, and the stop timer's later fire leaves it
// running. The agent confirmed the stopped turn's end within its grace, so
// the sandbox keeps its gen: the new turn runs on it, and nothing is
// retired.
func TestStopSession_PromptAfterStopRuns(t *testing.T) {
	ctx := context.Background()
	const grace = 2 * time.Second
	rig := newStopRig(t, stopRigConfig{grace: grace})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	stopped := rig.processingTurn(ctx, t, session.ID, 1)

	requested := time.Now()
	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
	agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCancelled)
	if got := rig.turnRow(ctx, t, stopped.ID).Status; got != sqlcgen.TurnStatusCancelled {
		t.Fatalf("stopped turn = %s, want cancelled", got)
	}
	// The turn's end triggers a snapshot after the event is acknowledged,
	// as every terminal event does; this fake sandbox never answers one, so
	// once it has started, stand in for its completion.
	stopEventually(t, 10*time.Second, "the snapshot the turn's end triggers", func() bool {
		sandbox, err := rig.sandboxes.Get(ctx, session.ID)
		return err == nil && sandbox.Status == sqlcgen.SandboxStatusSnapshotting
	})
	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, session.ID); err != nil {
		t.Fatal(err)
	}

	status, raw := rig.post(t, "/api/sessions/"+session.ID.String()+"/turns", token, []byte(`{"prompt":"carry on","modelId":null,"effort":null,"planMode":false}`))
	if status != http.StatusCreated {
		t.Fatalf("prompt after stop: status %d %s", status, raw)
	}
	var created restdtos.CreateTurnResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	var next pgtype.UUID
	if err := next.Scan(created.Id); err != nil {
		t.Fatal(err)
	}
	if rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid {
		t.Fatal("a person's prompt left the session's stop request set")
	}
	stopEventually(t, 10*time.Second, "the new turn dispatches", func() bool {
		return len(rig.commander.ofType(t, "prompt")) == 1
	})

	// The stop timer's re-armed fire, once the grace has run, finds no
	// flagged turn open and ends itself; the new turn keeps running.
	pumpUntil(ctx, t, rig.registry, 15*time.Second, "the stop timer's last fire", func() bool {
		return time.Since(requested) > grace && !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
	})
	row := rig.turnRow(ctx, t, next)
	if row.Status != sqlcgen.TurnStatusProcessing || row.StopRequestedAt.Valid {
		t.Fatalf("turn created after the stop = %s (flagged %v), want processing and unflagged", row.Status, row.StopRequestedAt.Valid)
	}
	if stops := rig.commander.ofType(t, "stop"); len(stops) != 1 {
		t.Fatalf("stop frames = %d, want only the one for the stopped turn", len(stops))
	}
	if gens := rig.commander.promptGens(t); !slices.Equal(gens, []int{1}) {
		t.Fatalf("prompt frames at gens %v, want one at gen 1: a confirmed stop keeps the sandbox", gens)
	}
	if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != sqlcgen.SandboxStatusReady || sb.Gen != 1 {
		t.Fatalf("sandbox = %s at gen %d, want ready at gen 1: nothing is retired when the agent confirms", sb.Status, sb.Gen)
	}
}

// TestStopSession_ReachesEveryDescendant: stopping a session stops its
// children and theirs (depth 2), each in its own transaction, audited with
// the parent it was reached through and authorized by the check on the
// session named; an unrelated session is untouched.
func TestStopSession_ReachesEveryDescendant(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)

	parent := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	child := rig.session(ctx, t, pgtype.UUID{}, parent.ID)
	grandchild := rig.session(ctx, t, pgtype.UUID{}, child.ID)
	unrelated := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	turns := map[string]sqlcgen.Turn{
		"parent":     rig.pendingTurn(ctx, t, parent.ID),
		"child":      rig.pendingTurn(ctx, t, child.ID),
		"grandchild": rig.pendingTurn(ctx, t, grandchild.ID),
	}
	untouched := rig.pendingTurn(ctx, t, unrelated.ID)

	status, resp := rig.stop(t, parent.ID.String(), token)
	if status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	want := []string{parent.ID.String(), child.ID.String(), grandchild.ID.String()}
	if !slices.Equal(resp.ReachedSessionIds, want) || resp.OpenTurns != 3 {
		t.Fatalf("response reached %v with %d open turns, want %v with 3", resp.ReachedSessionIds, resp.OpenTurns, want)
	}

	stopEventually(t, 10*time.Second, "every descendant's turn cancelled", func() bool {
		for _, tr := range turns {
			if rig.turnRow(ctx, t, tr.ID).Status != sqlcgen.TurnStatusCancelled {
				return false
			}
		}
		return true
	})
	for name, s := range map[string]sqlcgen.Session{"parent": parent, "child": child, "grandchild": grandchild} {
		if !rig.sessionRow(ctx, t, s.ID).StopRequestedAt.Valid {
			t.Errorf("%s session not flagged", name)
		}
	}
	for s, via := range map[pgtype.UUID]pgtype.UUID{child.ID: parent.ID, grandchild.ID: child.ID} {
		audits := rig.stopAudits(ctx, t, s)
		if len(audits) != 1 || audits[0]["via_parent_session_id"] != via.String() || audits[0]["requested_session_id"] != parent.ID.String() {
			t.Errorf("session %s audits = %v, want one reached via %s", s.String(), audits, via.String())
		}
	}
	if got := rig.turnRow(ctx, t, untouched.ID); got.Status != sqlcgen.TurnStatusPending || got.StopRequestedAt.Valid || rig.sessionRow(ctx, t, unrelated.ID).StopRequestedAt.Valid {
		t.Fatalf("unrelated session touched: turn %s, flagged %v", got.Status, got.StopRequestedAt.Valid)
	}
}

// holdSessionRow locks sessionID's row FOR UPDATE on a transaction of its
// own -- as a writer under the session's actor-epoch lock holds it -- until
// the returned release is called (at the latest when the test ends).
func (r *stopRig) holdSessionRow(ctx context.Context, t *testing.T, sessionID pgtype.UUID) (release func()) {
	t.Helper()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the row holder: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("lock the session row: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

// TestStopSession_CallerGoneMidWalkStillStopsTheTree: once the named
// session's request has committed, a caller that goes away -- a client
// timeout, a disconnect -- does not cut the rest short (technical plan
// §3.3). The parent commits while its child's row is held, so the walk
// waits on the child; the client then gives up and its connection closes,
// which cancels the request's context; the row is released only after
// that. The walk still reaches the child and the grandchild below it: both
// flagged and audited as reached through their parents, and every queued
// turn of the tree cancelled by its actor.
func TestStopSession_CallerGoneMidWalkStillStopsTheTree(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	parent := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	child := rig.session(ctx, t, pgtype.UUID{}, parent.ID)
	grandchild := rig.session(ctx, t, pgtype.UUID{}, child.ID)
	turns := map[string]sqlcgen.Turn{
		"parent":     rig.pendingTurn(ctx, t, parent.ID),
		"child":      rig.pendingTurn(ctx, t, child.ID),
		"grandchild": rig.pendingTurn(ctx, t, grandchild.ID),
	}

	release := rig.holdSessionRow(ctx, t, child.ID)
	waiters := rig.lockWaiters(ctx, t)

	callCtx, hangUp := context.WithCancel(ctx)
	defer hangUp()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, rig.server.URL+"/api/sessions/"+parent.ID.String()+"/stop", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	var call errgroup.Group
	call.Go(func() error {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return fmt.Errorf("the stop answered %d before the caller hung up", resp.StatusCode)
		}
		return nil
	})

	stopEventually(t, 10*time.Second, "the parent's request committed and the walk waiting on the child's row", func() bool {
		return rig.sessionRow(ctx, t, parent.ID).StopRequestedAt.Valid && rig.lockWaiters(ctx, t) > waiters
	})
	hangUp()
	if err := call.Wait(); err != nil {
		t.Fatal(err)
	}
	// Long enough for the server to see the connection close and cancel the
	// request's context; a walk still on that context fails here.
	time.Sleep(500 * time.Millisecond)
	release()

	stopEventually(t, 10*time.Second, "every turn of the tree cancelled", func() bool {
		for _, tr := range turns {
			if rig.turnRow(ctx, t, tr.ID).Status != sqlcgen.TurnStatusCancelled {
				return false
			}
		}
		return true
	})
	for s, via := range map[pgtype.UUID]pgtype.UUID{child.ID: parent.ID, grandchild.ID: child.ID} {
		if !rig.sessionRow(ctx, t, s).StopRequestedAt.Valid {
			t.Errorf("session %s not flagged", s.String())
		}
		audits := rig.stopAudits(ctx, t, s)
		if len(audits) != 1 || audits[0]["via_parent_session_id"] != via.String() || audits[0]["requested_session_id"] != parent.ID.String() {
			t.Errorf("session %s audits = %v, want one reached via %s", s.String(), audits, via.String())
		}
	}
}

// TestStopSession_WalkBoundedAfterCommit: what runs after the named
// session's commit is detached from the caller but bounded
// (StopDescendantWalkTimeout). With the child's row held past the bound,
// the walk gives up at the bound, not when the row is released: the caller,
// still there, gets the partial-walk 500 within the bound, and the child
// stays unflagged and unaudited once the row is released -- its request was
// never written, and a repeat reaches it.
func TestStopSession_WalkBoundedAfterCommit(t *testing.T) {
	ctx := context.Background()
	const bound = 500 * time.Millisecond
	rig := newStopRig(t, stopRigConfig{walk: bound})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	parent := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	child := rig.session(ctx, t, pgtype.UUID{}, parent.ID)
	rig.pendingTurn(ctx, t, parent.ID)
	childTurn := rig.pendingTurn(ctx, t, child.ID)

	release := rig.holdSessionRow(ctx, t, child.ID)
	// The row is released on its own after a while, whatever the request
	// does: a walk that waits for the row -- an unbounded one -- then answers
	// late and fails this test, instead of never answering at all.
	releaser := time.AfterFunc(8*bound, release)
	defer releaser.Stop()
	started := time.Now()
	status, raw := rig.post(t, "/api/sessions/"+parent.ID.String()+"/stop", token, nil)
	elapsed := time.Since(started)
	const partial = `{"error":"the session was stopped, but not every session it started could be reached: repeat the request"}`
	if status != http.StatusInternalServerError || strings.TrimSpace(string(raw)) != partial {
		t.Fatalf("stop with the child's row held past the bound: %d %s, want 500 %s", status, raw, partial)
	}
	if elapsed > 4*bound {
		t.Fatalf("the stop answered after %v, want about the %v bound: the walk waited for the row instead", elapsed, bound)
	}
	release()

	time.Sleep(500 * time.Millisecond)
	if rig.sessionRow(ctx, t, child.ID).StopRequestedAt.Valid || len(rig.stopAudits(ctx, t, child.ID)) != 0 || rig.turnRow(ctx, t, childTurn.ID).StopRequestedAt.Valid {
		t.Fatal("the child was stopped after the bound had run out")
	}
	if !rig.sessionRow(ctx, t, parent.ID).StopRequestedAt.Valid {
		t.Fatal("the parent's request did not stand")
	}
}

// childSpawn creates a child session of parentID on its own transaction,
// through httpapi.CreateSessionOnTx as the sentinel auto-fix outbox does,
// leaving the transaction for the caller to finish.
func (r *stopRig) childSpawn(ctx context.Context, t *testing.T, parentID pgtype.UUID) (pgxTx, sqlcgen.Session, *httpapi.CreateSessionError) {
	t.Helper()
	tx, created, cerr, err := r.childSpawnRequest(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	return tx, created, cerr
}

// childSpawnRequest is childSpawn without t, for a goroutine.
func (r *stopRig) childSpawnRequest(ctx context.Context, parentID pgtype.UUID) (pgxTx, sqlcgen.Session, *httpapi.CreateSessionError, error) {
	prompt := "fix the finding"
	branch := "narvi/sentinel-fix/1"
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "widgets", Url: "https://github.com/example-org/widgets", Branch: &branch},
		},
	}
	entitlement, eerr := httpapi.ResolveRepoEntitlement(ctx, narvipg.NewGitHubPRSessionStore(r.pool), r.auditLog, pgtype.UUID{}, req)
	if eerr != nil {
		return nil, sqlcgen.Session{}, nil, fmt.Errorf("entitlement: %s", eerr.Message)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, sqlcgen.Session{}, nil, err
	}
	tag := provenance.SentinelAutoFix
	created, _, cerr := httpapi.CreateSessionOnTx(ctx, tx, r.sessions, r.turns, narvipg.NewEnvironmentStore(r.pool), r.auditLog, req, pgtype.UUID{}, false,
		platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(r.pool), entitlement, httpapi.ChildSessionOptions{ParentSessionID: parentID, SpawnDepth: 1, ProvenanceTag: &tag})
	return tx, created, cerr, nil
}

// lockWaiters counts this database's backends waiting on a lock.
func (r *stopRig) lockWaiters(ctx context.Context, t *testing.T) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStopSession_RacingChildSpawnRefusedOrStopped: a child spawn racing a
// stop of its parent is either stopped (it committed first, so the walk
// finds it) or refused (it waits on the parent row FOR SHARE and then reads
// the stop). Never an orphan: a child created and not stopped.
func TestStopSession_RacingChildSpawnRefusedOrStopped(t *testing.T) {
	ctx := context.Background()

	type stopResult struct {
		status int
		resp   restdtos.StopSessionResponse
		err    error
	}

	t.Run("the spawn first", func(t *testing.T) {
		rig := newStopRig(t, stopRigConfig{})
		owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
		parent := rig.session(ctx, t, owner.ID, pgtype.UUID{})
		rig.pendingTurn(ctx, t, parent.ID)

		spawnTx, child, cerr := rig.childSpawn(ctx, t, parent.ID)
		if cerr != nil {
			t.Fatalf("spawn before any stop refused: %s", cerr.Message)
		}
		result := make(chan stopResult, 1)
		go func() {
			status, resp, err := rig.stopRequest(parent.ID.String(), token)
			result <- stopResult{status, resp, err}
		}()
		stopEventually(t, 10*time.Second, "the stop waits for the spawn", func() bool { return rig.lockWaiters(ctx, t) >= 1 })
		if err := spawnTx.Commit(ctx); err != nil {
			t.Fatalf("commit spawn: %v", err)
		}
		got := <-result
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != http.StatusAccepted || !slices.Contains(got.resp.ReachedSessionIds, child.ID.String()) {
			t.Fatalf("stop = %d reaching %v, want 202 reaching the child %s", got.status, got.resp.ReachedSessionIds, child.ID.String())
		}
		assertChildStopped(ctx, t, rig, child.ID)
	})

	t.Run("the stop first", func(t *testing.T) {
		rig := newStopRig(t, stopRigConfig{})
		owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
		parent := rig.session(ctx, t, owner.ID, pgtype.UUID{})
		held := rig.pendingTurn(ctx, t, parent.ID)

		// Hold the stop inside its transaction: it locks the parent row,
		// then waits here to flag this turn.
		holdTx, err := rig.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holdTx.Exec(ctx, `SELECT id FROM turns WHERE id = $1 FOR UPDATE`, held.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan stopResult, 1)
		go func() {
			status, resp, err := rig.stopRequest(parent.ID.String(), token)
			result <- stopResult{status, resp, err}
		}()
		stopEventually(t, 10*time.Second, "the stop holds the parent row", func() bool { return rig.lockWaiters(ctx, t) >= 1 })

		type spawnResult struct {
			tx    pgxTx
			child sqlcgen.Session
			cerr  *httpapi.CreateSessionError
			err   error
		}
		spawned := make(chan spawnResult, 1)
		go func() {
			tx, child, cerr, err := rig.childSpawnRequest(ctx, parent.ID)
			spawned <- spawnResult{tx, child, cerr, err}
		}()
		stopEventually(t, 10*time.Second, "the spawn waits on the parent row", func() bool { return rig.lockWaiters(ctx, t) >= 2 })
		if err := holdTx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		spawn := <-spawned
		if spawn.err != nil {
			t.Fatalf("spawn: %v", spawn.err)
		}
		if spawn.cerr != nil {
			// Refused: end the spawn at once, as its outbox does, so the
			// stop's wake of the parent's actor finds the row free.
			_ = spawn.tx.Rollback(ctx)
		}
		got := <-result
		if got.err != nil {
			t.Fatalf("stop: %v", got.err)
		}
		if got.status != http.StatusAccepted {
			t.Fatalf("stop: status %d", got.status)
		}
		if spawn.cerr != nil {
			if !spawn.cerr.ParentStopped {
				t.Fatalf("spawn refused for another reason: %s", spawn.cerr.Message)
			}
			var children int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE parent_session_id = $1`, parent.ID).Scan(&children); err != nil {
				t.Fatal(err)
			}
			if children != 0 {
				t.Fatalf("%d children exist after a refused spawn", children)
			}
			return
		}
		// The spawn was not refused: it commits after the stop's walk,
		// the worst case for it, and must have been stopped anyway.
		if err := spawn.tx.Commit(ctx); err != nil {
			t.Fatalf("commit spawn: %v", err)
		}
		if !slices.Contains(got.resp.ReachedSessionIds, spawn.child.ID.String()) {
			t.Fatalf("an orphan: child %s created after its parent's stop, neither refused nor reached (%v)", spawn.child.ID.String(), got.resp.ReachedSessionIds)
		}
		assertChildStopped(ctx, t, rig, spawn.child.ID)
	})
}

// pgxTx is the transaction CreateSessionOnTx runs on.
type pgxTx = interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}

// assertChildStopped waits for every turn of childID to end cancelled and
// checks the session carries the stop request.
func assertChildStopped(ctx context.Context, t *testing.T, rig *stopRig, childID pgtype.UUID) {
	t.Helper()
	if !rig.sessionRow(ctx, t, childID).StopRequestedAt.Valid {
		t.Fatal("child session not flagged")
	}
	turns, err := rig.turns.ListForSession(ctx, childID)
	if err != nil || len(turns) == 0 {
		t.Fatalf("child turns: %v (%d)", err, len(turns))
	}
	stopEventually(t, 10*time.Second, "the child's turns cancelled", func() bool {
		for _, tr := range turns {
			if rig.turnRow(ctx, t, tr.ID).Status != sqlcgen.TurnStatusCancelled {
				return false
			}
		}
		return true
	})
}

// TestStopSession_ChildRefusedUntilResumed: a stopped session refuses a new
// child with the permanent ParentStopped marker until a person resumes it --
// by a prompt, or by approving its plan.
func TestStopSession_ChildRefusedUntilResumed(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		resume func(t *testing.T, rig *stopRig, session sqlcgen.Session, token string)
	}{
		{"a prompt", func(t *testing.T, rig *stopRig, session sqlcgen.Session, token string) {
			if status, raw := rig.post(t, "/api/sessions/"+session.ID.String()+"/turns", token, []byte(`{"prompt":"resume","modelId":null,"effort":null,"planMode":false}`)); status != http.StatusCreated {
				t.Fatalf("prompt: status %d %s", status, raw)
			}
		}},
		{"approving its plan", func(t *testing.T, rig *stopRig, session sqlcgen.Session, token string) {
			producing, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producing.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
			if err != nil {
				t.Fatal(err)
			}
			if status, raw := rig.post(t, "/api/sessions/"+session.ID.String()+"/plans/"+plan.ID.String()+"/approve", token, nil); status != http.StatusOK {
				t.Fatalf("approve: status %d %s", status, raw)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStopRig(t, stopRigConfig{})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}

			tx, _, cerr := rig.childSpawn(ctx, t, session.ID)
			_ = tx.Rollback(ctx)
			if cerr == nil || !cerr.ParentStopped || cerr.Status != http.StatusConflict {
				t.Fatalf("child of a stopped session: %+v, want a 409 ParentStopped refusal", cerr)
			}

			tc.resume(t, rig, session, token)
			if rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid {
				t.Fatal("resuming left the session's stop request set")
			}
			tx, _, cerr = rig.childSpawn(ctx, t, session.ID)
			if cerr != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("child of a resumed session refused: %s", cerr.Message)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStopSession_WorkflowRunEndsCancelled: while a person's stop request
// stands, an attempt that ends -- however it ends -- ends its workflow run
// cancelled, and workflow.NextStep is not consulted: the definition's edge
// on 'blocked', or its next step in order on 'ok', would otherwise queue
// step 2. The attempt's step run keeps the status its turn really ended
// with. Covered: both ways a stop cancels an attempt's turn (the actor's
// own cancel of a queued one, and the agent's execution_complete{cancelled}
// answering the sandbox `stop`); a running attempt that completed, or
// failed, before the stop reached it; and an attempt whose turn was created
// after the request, so carries no flag of its own, completing while the
// session's request stands.
func TestStopSession_WorkflowRunEndsCancelled(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		// running: the attempt is processing when it ends, reporting outcome;
		// otherwise it is queued, and the stop cancels it.
		running bool
		outcome sandboxws.ExecutionCompleteOutcome
		// createdAfterStop: the attempt's turn is created after the request.
		createdAfterStop bool
		wantTurn         sqlcgen.TurnStatus
		wantStep         string
	}{
		{"a queued attempt", false, "", false, sqlcgen.TurnStatusCancelled, "cancelled"},
		{"a running attempt the agent cancels", true, sandboxws.ExecutionCompleteOutcomeCancelled, false, sqlcgen.TurnStatusCancelled, "cancelled"},
		{"a running attempt that completed before the stop reached it", true, sandboxws.ExecutionCompleteOutcomeCompleted, false, sqlcgen.TurnStatusCompleted, "completed"},
		{"a running attempt that failed before the stop reached it", true, sandboxws.ExecutionCompleteOutcomeFailed, false, sqlcgen.TurnStatusFailed, "failed"},
		{"an attempt created after the stop, completing while it stands", true, sandboxws.ExecutionCompleteOutcomeCompleted, true, sqlcgen.TurnStatusCompleted, "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStopRig(t, stopRigConfig{})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})

			var defID, firstStep, nextStep pgtype.UUID
			if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'stop-test', false, 1) RETURNING id`).Scan(&defID); err != nil {
				t.Fatal(err)
			}
			for order, id := range map[int]*pgtype.UUID{1: &firstStep, 2: &nextStep} {
				if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, $2, 'agent', '{{prompt}}') RETURNING id`, defID, order).Scan(id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := rig.pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $3, 'blocked')`, defID, firstStep, nextStep); err != nil {
				t.Fatal(err)
			}
			run, err := rig.workflows.CreateRun(ctx, session.ID, "request", defID, 1)
			if err != nil {
				t.Fatal(err)
			}
			stepRun, err := rig.workflows.CreateStepRun(ctx, run.ID, firstStep)
			if err != nil {
				t.Fatal(err)
			}
			attach := func() sqlcgen.Turn {
				var attempt sqlcgen.Turn
				if tc.running {
					rig.readySandbox(ctx, t, session.ID, 1)
					attempt = rig.processingTurn(ctx, t, session.ID, 1)
				} else {
					attempt = rig.pendingTurn(ctx, t, session.ID)
				}
				if err := rig.workflows.AttachTurn(ctx, stepRun.ID, attempt.ID); err != nil {
					t.Fatal(err)
				}
				return attempt
			}

			var attempt sqlcgen.Turn
			if !tc.createdAfterStop {
				attempt = attach()
			}
			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}
			if tc.createdAfterStop {
				pumpUntil(ctx, t, rig.registry, 10*time.Second, "the stop, with nothing open, handled", func() bool {
					return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
				})
				attempt = attach()
				if rig.turnRow(ctx, t, attempt.ID).StopRequestedAt.Valid || !rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid {
					t.Fatal("setup: want an unflagged attempt on a session whose stop request stands")
				}
			} else if tc.running {
				stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
			}
			if tc.running {
				agentReports(ctx, t, rig.registry, session.ID, 1, tc.outcome)
			}
			stopEventually(t, 10*time.Second, "the attempt's turn ends", func() bool {
				return rig.turnRow(ctx, t, attempt.ID).Status == tc.wantTurn
			})

			var runStatus, stepStatus string
			var finished pgtype.Timestamptz
			if err := rig.pool.QueryRow(ctx, `SELECT status::text, finished_at FROM workflow_runs WHERE id = $1`, run.ID).Scan(&runStatus, &finished); err != nil {
				t.Fatal(err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT status::text FROM workflow_step_runs WHERE id = $1`, stepRun.ID).Scan(&stepStatus); err != nil {
				t.Fatal(err)
			}
			if runStatus != "cancelled" || !finished.Valid || stepStatus != tc.wantStep {
				t.Fatalf("run %s (finished %v), attempt %s; want the run cancelled and finished, the attempt %s", runStatus, finished.Valid, stepStatus, tc.wantStep)
			}
			var attempts, sessionTurns int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id = $1`, run.ID).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&sessionTurns); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || sessionTurns != 1 {
				t.Fatalf("%d attempts and %d turns, want 1 and 1: the next step must not be queued", attempts, sessionTurns)
			}
		})
	}
}

// TestStopSession_DisarmsWorkCreatingTimers: a stop deletes the session's
// timers whose firing creates a turn (the re-review debounce) and keeps the
// sandbox's watchdogs.
func TestStopSession_DisarmsWorkCreatingTimers(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	later := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	for _, name := range []string{sessionactor.TimerReviewRetriggerDebounce, sessionactor.TimerInactivity} {
		if _, err := rig.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: session.ID, Name: name, FiresAt: later}); err != nil {
			t.Fatal(err)
		}
	}

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	stopEventually(t, 10*time.Second, "the stop handled", func() bool {
		return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
	})
	if names := rig.timerNames(ctx, t, session.ID); !slices.Equal(names, []string{sessionactor.TimerInactivity}) {
		t.Fatalf("timers = %v, want only %s", names, sessionactor.TimerInactivity)
	}
}

// TestStopSession_SurvivesReplicaLoss: the request is data. Written by a
// replica that does not host the session's actor, whose host is then lost
// before it ever hears of it, the stop is carried out by the next replica
// whose pump claims the timer.
func TestStopSession_SurvivesReplicaLoss(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	running := rig.processingTurn(ctx, t, session.ID, 1)
	queued := rig.pendingTurn(ctx, t, session.ID)

	// The replica hosting the session's actor.
	_, host, loseHost := rig.newReplica(t)
	if _, err := host.GetOrSpawn(ctx, session.ID); err != nil {
		t.Fatalf("host GetOrSpawn: %v", err)
	}

	// The request lands on the rig's replica, which cannot host the actor.
	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	// The host is lost before anything delivered it the stop.
	if err := loseHost(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("host shutdown: %v", err)
	}
	if got := rig.turnRow(ctx, t, queued.ID).Status; got != sqlcgen.TurnStatusPending {
		t.Fatalf("queued turn = %s before any replica handled the stop", got)
	}

	next, successor, _ := rig.newReplica(t)
	pumpUntil(ctx, t, successor, 10*time.Second, "the next replica carries out the stop", func() bool {
		return rig.turnRow(ctx, t, queued.ID).Status == sqlcgen.TurnStatusCancelled && len(next.ofType(t, "stop")) == 1
	})
	if stops := next.ofType(t, "stop"); stops[0]["gen"] != float64(1) {
		t.Fatalf("stop frame = %v, want gen 1", stops[0])
	}
	if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusProcessing {
		t.Fatalf("running turn = %s inside its grace, want processing", got)
	}
}

// agentHeartbeat delivers a heartbeat from the sandbox at gen to the
// session's actor on registry and waits for it to be handled, the dispatch
// every sandbox event runs after its commit included.
func agentHeartbeat(ctx context.Context, t *testing.T, registry *sessionactor.Registry, sessionID pgtype.UUID, gen int) {
	t.Helper()
	messageID := uuid.NewString()
	raw, err := json.Marshal(map[string]any{"type": "heartbeat", "messageId": messageID, "sessionId": sessionID.String(), "gen": gen})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := registry.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "heartbeat", Gen: gen, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	select {
	case <-reply:
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat not handled")
	}
	actorBarrier(ctx, t, registry, sessionID)
}

// TestStopSession_SilentGraceMovesTheNextTurnToANewGen: a stopped turn whose
// agent stays silent past its grace is cancelled, and the sandbox gen it ran
// on is retired, so the next turn never runs there. An execution_complete
// names a gen, never a turn: dispatched to the same gen, the next turn would
// take the stopped work's late end as its own. Here the next turn gets a
// new gen (a restore from the last snapshot, or a fresh spawn without one),
// the retired gen's provider object is stopped, the stopped work's late
// execution_complete at the old gen -- cancelled or completed, before the
// next turn is dispatched and after -- changes nothing, and the next turn
// ends with its own result. Two ways to a next turn: a person's prompt
// after the grace, and a turn queued after the stop, as an AlwaysQueue
// ingress queues one, which the stop timer's own dispatch picks up with no
// human step.
func TestStopSession_SilentGraceMovesTheNextTurnToANewGen(t *testing.T) {
	ctx := context.Background()
	const grace = 2 * time.Second

	for _, tc := range []struct {
		name            string
		queuedAfterStop bool
		snapshot        bool
	}{
		{"a prompt after the grace, restored from a snapshot", false, true},
		{"a turn queued after the stop, freshly spawned", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stopProvider{}
			rig := newStopRig(t, stopRigConfig{grace: grace, provider: provider})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			rig.readySandbox(ctx, t, session.ID, 1)
			if tc.snapshot {
				if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snapshot-after-turn-0' WHERE session_id = $1`, session.ID); err != nil {
					t.Fatal(err)
				}
			}
			stopped := rig.processingTurn(ctx, t, session.ID, 1)

			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}
			stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })

			var next pgtype.UUID
			if tc.queuedAfterStop {
				next = rig.pendingTurn(ctx, t, session.ID).ID
			}
			// The agent stays silent: nothing answers the stop.
			pumpUntil(ctx, t, rig.registry, 15*time.Second, "the grace cancels the silent turn", func() bool {
				return rig.turnRow(ctx, t, stopped.ID).Status == sqlcgen.TurnStatusCancelled
			})

			if !tc.queuedAfterStop {
				status, raw := rig.post(t, "/api/sessions/"+session.ID.String()+"/turns", token, []byte(`{"prompt":"try again","modelId":null,"effort":null,"planMode":false}`))
				if status != http.StatusCreated {
					t.Fatalf("prompt after the grace: status %d %s", status, raw)
				}
				var created restdtos.CreateTurnResponse
				if err := json.Unmarshal(raw, &created); err != nil {
					t.Fatal(err)
				}
				if err := next.Scan(created.Id); err != nil {
					t.Fatal(err)
				}
			}
			// Every dispatch the grace's cancel or the prompt started has run.
			actorBarrier(ctx, t, rig.registry, session.ID)
			if gens := rig.commander.promptGens(t); len(gens) != 0 {
				t.Fatalf("prompt frames at gens %v after the grace, want none: the next turn went to the stopped work's gen", gens)
			}
			sb := rig.sandboxRow(ctx, t, session.ID)
			if sb.Gen != 2 || sb.Status != sqlcgen.SandboxStatusConnecting {
				t.Fatalf("sandbox = %s at gen %d after the grace, want a new gen 2 connecting", sb.Status, sb.Gen)
			}
			created, restored, stoppedObjects := provider.calls()
			if tc.snapshot && !slices.Equal(restored, []int{2}) || !tc.snapshot && !slices.Equal(created, []int{2}) {
				t.Fatalf("provider spawns %v, restores %v; want gen 2 %s", created, restored, map[bool]string{true: "restored", false: "spawned"}[tc.snapshot])
			}
			if !slices.Equal(stoppedObjects, []string{providerIDForGen(1)}) {
				t.Fatalf("provider stops = %v, want the retired gen's object %s", stoppedObjects, providerIDForGen(1))
			}

			// The stopped work's late end, at gen 1: it changes no turn, no
			// sandbox, and sends nothing -- no prompt, and no snapshot of
			// the new gen.
			lateEnds := func(when string) {
				t.Helper()
				want := rig.turnRow(ctx, t, next).Status
				before := rig.sandboxRow(ctx, t, session.ID)
				prompts, snapshots := len(rig.commander.ofType(t, "prompt")), len(rig.commander.ofType(t, "snapshot"))
				for _, outcome := range []sandboxws.ExecutionCompleteOutcome{sandboxws.ExecutionCompleteOutcomeCancelled, sandboxws.ExecutionCompleteOutcomeCompleted} {
					agentReports(ctx, t, rig.registry, session.ID, 1, outcome)
					if got := rig.turnRow(ctx, t, next).Status; got != want {
						t.Fatalf("the stopped work's late %s at gen 1, %s, moved the next turn from %s to %s", outcome, when, want, got)
					}
					if got := rig.turnRow(ctx, t, stopped.ID).Status; got != sqlcgen.TurnStatusCancelled {
						t.Fatalf("the stopped work's late %s moved the stopped turn to %s", outcome, got)
					}
					if after := rig.sandboxRow(ctx, t, session.ID); after.Status != before.Status || after.Gen != before.Gen {
						t.Fatalf("the stopped work's late %s, %s, moved the sandbox from %s at gen %d to %s at gen %d", outcome, when, before.Status, before.Gen, after.Status, after.Gen)
					}
					if n, m := len(rig.commander.ofType(t, "prompt")), len(rig.commander.ofType(t, "snapshot")); n != prompts || m != snapshots {
						t.Fatalf("the stopped work's late %s, %s, sent %d prompt and %d snapshot frames", outcome, when, n-prompts, m-snapshots)
					}
				}
			}
			lateEnds("before the next turn is dispatched")
			if got := rig.turnRow(ctx, t, next).Status; got != sqlcgen.TurnStatusPending {
				t.Fatalf("next turn = %s while the new sandbox boots, want pending", got)
			}

			// Stand in for the new sandbox connecting and booting.
			if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1 AND gen = 2`, session.ID); err != nil {
				t.Fatal(err)
			}
			// With the new gen ready and the next turn still queued, a
			// dropped event that ran its post-commit dispatch would send
			// the next turn now: it must not.
			lateEnds("once the new gen is ready with the next turn still queued")
			agentHeartbeat(ctx, t, rig.registry, session.ID, 2)
			if gens := rig.commander.promptGens(t); !slices.Equal(gens, []int{2}) {
				t.Fatalf("prompt frames at gens %v, want one at gen 2", gens)
			}
			if row := rig.turnRow(ctx, t, next); row.Status != sqlcgen.TurnStatusProcessing || row.DispatchedSandboxGen == nil || *row.DispatchedSandboxGen != 2 {
				t.Fatalf("next turn = %s dispatched at %v, want processing at gen 2", row.Status, row.DispatchedSandboxGen)
			}
			lateEnds("after the next turn is dispatched")

			agentReports(ctx, t, rig.registry, session.ID, 2, sandboxws.ExecutionCompleteOutcomeCompleted)
			if got := rig.turnRow(ctx, t, next).Status; got != sqlcgen.TurnStatusCompleted {
				t.Fatalf("next turn = %s after its own execution_complete, want completed", got)
			}
			if n := rig.syntheticCompletes(ctx, t, session.ID)[stopped.ID.String()]; n != 1 {
				t.Fatalf("%d synthetic execution_complete events for the stopped turn, want exactly 1", n)
			}
		})
	}
}

// TestStopSession_FlaggedTurnNeverResentToANewGen: a flagged turn in flight
// whose sandbox was respawned since its dispatch (it ran on gen 1, the
// sandbox is at gen 2) is never re-sent to the new gen: that would start
// again the work the person stopped. Its grace then cancels it, and the
// sandbox keeps gen 2: gen 1 is already fenced off, so nothing is retired.
func TestStopSession_FlaggedTurnNeverResentToANewGen(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{grace: 2 * time.Second})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 2)
	running := rig.processingTurn(ctx, t, session.ID, 1)

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
	agentHeartbeat(ctx, t, rig.registry, session.ID, 2)
	if prompts := rig.commander.ofType(t, "prompt"); len(prompts) != 0 {
		t.Fatalf("prompt frames = %v, want none: a flagged turn is never re-sent to a new gen", prompts)
	}

	pumpUntil(ctx, t, rig.registry, 15*time.Second, "the grace cancels the turn", func() bool {
		return rig.turnRow(ctx, t, running.ID).Status == sqlcgen.TurnStatusCancelled
	})
	if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != sqlcgen.SandboxStatusReady || sb.Gen != 2 {
		t.Fatalf("sandbox = %s at gen %d, want ready at gen 2: the stopped turn's gen 1 was already fenced off", sb.Status, sb.Gen)
	}
	if prompts := rig.commander.ofType(t, "prompt"); len(prompts) != 0 {
		t.Fatalf("prompt frames = %v after the grace, want none", prompts)
	}
}

// TestStopSession_UnflaggedTurnDispatchesOnceFlaggedOnesEnd: a turn queued
// after the stop runs as soon as the stop timer has cancelled the flagged
// turns ahead of it -- the timer's own dispatch picks it up, with nothing
// else to wake the session -- and on the same sandbox gen: only queued
// turns were cancelled, so nothing is retired.
func TestStopSession_UnflaggedTurnDispatchesOnceFlaggedOnesEnd(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{noWake: true})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	flagged := rig.pendingTurn(ctx, t, session.ID)

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	later := rig.pendingTurn(ctx, t, session.ID)

	pumpUntil(ctx, t, rig.registry, 10*time.Second, "the turn queued after the stop dispatches", func() bool {
		return len(rig.commander.ofType(t, "prompt")) > 0
	})
	if got := rig.turnRow(ctx, t, flagged.ID).Status; got != sqlcgen.TurnStatusCancelled {
		t.Fatalf("flagged turn = %s, want cancelled", got)
	}
	if row := rig.turnRow(ctx, t, later.ID); row.Status != sqlcgen.TurnStatusProcessing || row.StopRequestedAt.Valid {
		t.Fatalf("turn queued after the stop = %s (flagged %v), want processing and unflagged", row.Status, row.StopRequestedAt.Valid)
	}
	if gens := rig.commander.promptGens(t); !slices.Equal(gens, []int{1}) {
		t.Fatalf("prompt frames at gens %v, want one at gen 1", gens)
	}
	if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != sqlcgen.SandboxStatusReady || sb.Gen != 1 {
		t.Fatalf("sandbox = %s at gen %d, want ready at gen 1: cancelling queued turns retires nothing", sb.Status, sb.Gen)
	}
}

// TestStopSession_GraceRunsFromTheFlag: a turn's grace runs from its own
// flag, on the database's clock, however late the stop timer's first fire
// comes (a busy replica, a lost one): the fire re-arms the timer for exactly
// stop_requested_at + StopGrace, never for its own instant plus the grace.
func TestStopSession_GraceRunsFromTheFlag(t *testing.T) {
	ctx := context.Background()
	const grace = 3 * time.Second
	rig := newStopRig(t, stopRigConfig{grace: grace, noWake: true})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	running := rig.processingTurn(ctx, t, session.ID, 1)

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	// Nothing delivers the stop for a while: the first fire comes late.
	time.Sleep(grace / 2)
	pumpUntil(ctx, t, rig.registry, 10*time.Second, "the late first fire sends the stop", func() bool {
		return len(rig.commander.ofType(t, "stop")) == 1
	})

	flag := rig.turnRow(ctx, t, running.ID).StopRequestedAt
	timer, err := rig.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: session.ID, Name: sessionactor.TimerStop})
	if err != nil {
		t.Fatalf("get stop timer: %v", err)
	}
	if want := flag.Time.Add(grace); !timer.FiresAt.Time.Equal(want) {
		t.Fatalf("stop re-armed for %v, want exactly the flag plus the grace, %v (off by %v)", timer.FiresAt.Time, want, timer.FiresAt.Time.Sub(want))
	}
}

// TestStopSession_PushAfterStopKeepsItsDebounce: a stop disarms the
// re-review debounce armed before it, and a push after the stop arms it
// again as new input (technical plan §3.3's effects table): the stop
// timer's later fire, at the end of the running turn's grace, while the
// session's request still stands, leaves that one armed.
func TestStopSession_PushAfterStopKeepsItsDebounce(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{grace: 2 * time.Second})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	running := rig.processingTurn(ctx, t, session.ID, 1)
	armDebounce := func() {
		t.Helper()
		if _, err := rig.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
			SessionID: session.ID, Name: sessionactor.TimerReviewRetriggerDebounce,
			FiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		}); err != nil {
			t.Fatalf("arm debounce: %v", err)
		}
	}
	armDebounce()

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
	if slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerReviewRetriggerDebounce) {
		t.Fatal("the debounce armed before the stop is still armed")
	}
	agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCancelled)
	if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusCancelled {
		t.Fatalf("running turn = %s, want cancelled", got)
	}

	armDebounce() // a push after the stop
	pumpUntil(ctx, t, rig.registry, 15*time.Second, "the stop timer's last fire", func() bool {
		return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
	})
	if !rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid {
		t.Fatal("setup: the session's stop request no longer stands")
	}
	if !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerReviewRetriggerDebounce) {
		t.Fatalf("timers = %v: the debounce a push armed after the stop was disarmed", rig.timerNames(ctx, t, session.ID))
	}
}

// TestStopSession_ResumeKeepsTimersArmedSince: once a person resumes a
// stopped session, a work-creating timer armed since is theirs, and the
// stop timer's late fire for the stopped turn's grace leaves it armed.
func TestStopSession_ResumeKeepsTimersArmedSince(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{grace: 2 * time.Second})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	rig.processingTurn(ctx, t, session.ID, 1)

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
	agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCancelled)
	if status, raw := rig.post(t, "/api/sessions/"+session.ID.String()+"/turns", token, []byte(`{"prompt":"resume","modelId":null,"effort":null,"planMode":false}`)); status != http.StatusCreated {
		t.Fatalf("prompt: status %d %s", status, raw)
	}
	if rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid {
		t.Fatal("setup: the prompt left the stop request standing")
	}
	if _, err := rig.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: session.ID, Name: sessionactor.TimerReviewRetriggerDebounce,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("arm debounce: %v", err)
	}

	pumpUntil(ctx, t, rig.registry, 15*time.Second, "the stop timer's late fire", func() bool {
		return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
	})
	if !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerReviewRetriggerDebounce) {
		t.Fatalf("timers = %v: a timer armed after the person resumed was disarmed", rig.timerNames(ctx, t, session.ID))
	}
}

// TestStopSession_RacingGrandchildSpawnReached: the walk lists a session's
// children only once that session's own stop has committed, at every
// depth -- so a grandchild spawn racing the walk, holding its parent (the
// child) FOR SHARE when the walk reaches it, is found once it commits,
// never left an orphan. Listing the whole tree once, right after the root's
// commit, would miss it.
func TestStopSession_RacingGrandchildSpawnReached(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	parent := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	child := rig.session(ctx, t, pgtype.UUID{}, parent.ID)
	rig.pendingTurn(ctx, t, parent.ID)
	rig.pendingTurn(ctx, t, child.ID)

	spawnTx, grandchild, cerr := rig.childSpawn(ctx, t, child.ID)
	if cerr != nil {
		t.Fatalf("grandchild spawn before any stop refused: %s", cerr.Message)
	}

	type stopResult struct {
		status int
		resp   restdtos.StopSessionResponse
		err    error
	}
	result := make(chan stopResult, 1)
	go func() {
		status, resp, err := rig.stopRequest(parent.ID.String(), token)
		result <- stopResult{status, resp, err}
	}()
	stopEventually(t, 10*time.Second, "the walk waits on the child the grandchild spawn holds", func() bool {
		return rig.sessionRow(ctx, t, parent.ID).StopRequestedAt.Valid && rig.lockWaiters(ctx, t) >= 1
	})
	if err := spawnTx.Commit(ctx); err != nil {
		t.Fatalf("commit grandchild spawn: %v", err)
	}

	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	want := []string{parent.ID.String(), child.ID.String(), grandchild.ID.String()}
	if got.status != http.StatusAccepted || !slices.Equal(got.resp.ReachedSessionIds, want) {
		t.Fatalf("stop = %d reaching %v, want 202 reaching %v: the grandchild is an orphan", got.status, got.resp.ReachedSessionIds, want)
	}
	assertChildStopped(ctx, t, rig, grandchild.ID)
}

// hitlRun is a run of a custom three-step definition: step 1 gated by a
// person's decision (hitl_after), then 'ok' edges from step 1 to step 2
// and from step 2 to step 3.
type hitlRun struct {
	id pgtype.UUID
}

// startHITLRun creates that definition and its run on sessionID, with step
// 1's attempt processing on the sandbox at gen 1.
func (r *stopRig) startHITLRun(ctx context.Context, t *testing.T, sessionID pgtype.UUID) hitlRun {
	t.Helper()
	var defID pgtype.UUID
	if err := r.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'stop-hitl-test', false, 1) RETURNING id`).Scan(&defID); err != nil {
		t.Fatal(err)
	}
	var steps [3]pgtype.UUID
	for i := range steps {
		if err := r.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template, hitl_after) VALUES ($1, $2, 'agent', '{{prompt}}', $3) RETURNING id`, defID, i+1, i == 0).Scan(&steps[i]); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i+1 < len(steps); i++ {
		if _, err := r.pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $3, 'ok')`, defID, steps[i], steps[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	run, err := r.workflows.CreateRun(ctx, sessionID, "request", defID, 1)
	if err != nil {
		t.Fatal(err)
	}
	stepRun, err := r.workflows.CreateStepRun(ctx, run.ID, steps[0])
	if err != nil {
		t.Fatal(err)
	}
	attempt := r.processingTurn(ctx, t, sessionID, 1)
	if err := r.workflows.AttachTurn(ctx, stepRun.ID, attempt.ID); err != nil {
		t.Fatal(err)
	}
	return hitlRun{id: run.ID}
}

// runState returns a workflow run's status and each of its step runs as
// "<step order>:<status>", oldest first.
func (r *stopRig) runState(ctx context.Context, t *testing.T, runID pgtype.UUID) (string, []string) {
	t.Helper()
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT status::text FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	rows, err := r.pool.Query(ctx, `SELECT sd.step_order, sr.status::text FROM workflow_step_runs sr
		JOIN workflow_step_definitions sd ON sd.id = sr.step_definition_id
		WHERE sr.workflow_run_id = $1 ORDER BY sr.created_at, sr.id`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var order int32
		var stepStatus string
		if err := rows.Scan(&order, &stepStatus); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, fmt.Sprintf("%d:%s", order, stepStatus))
	}
	return status, steps
}

// stepRunIn returns the id and turn of runID's one step run in status,
// failing with the run's whole state when there is none.
func (r *stopRig) stepRunIn(ctx context.Context, t *testing.T, runID pgtype.UUID, status string) (id, turnID pgtype.UUID) {
	t.Helper()
	if err := r.pool.QueryRow(ctx, `SELECT id, turn_id FROM workflow_step_runs WHERE workflow_run_id = $1 AND status::text = $2`, runID, status).Scan(&id, &turnID); err != nil {
		runStatus, steps := r.runState(ctx, t, runID)
		t.Fatalf("no step run %s (%v): run %s, step runs %v", status, err, runStatus, steps)
	}
	return id, turnID
}

// decide POSTs a person's decision on stepRunID of runID.
func (r *stopRig) decide(t *testing.T, token string, runID, stepRunID pgtype.UUID, body string) restdtos.WorkflowStepDecideResponse {
	t.Helper()
	status, raw := r.post(t, "/api/workflow-runs/"+runID.String()+"/steps/"+stepRunID.String()+"/decide", token, []byte(body))
	if status != http.StatusOK {
		t.Fatalf("decide %s: status %d %s", body, status, raw)
	}
	var resp restdtos.WorkflowStepDecideResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// runAttempt waits for the actor to dispatch turnID, lets the agent
// complete it at gen 1, and answers the snapshot its end starts, which
// dispatches whatever it queued.
func (r *stopRig) runAttempt(ctx context.Context, t *testing.T, sessionID, turnID pgtype.UUID) {
	t.Helper()
	stopEventually(t, 10*time.Second, "the attempt dispatches", func() bool {
		return r.turnRow(ctx, t, turnID).Status == sqlcgen.TurnStatusProcessing
	})
	agentReports(ctx, t, r.registry, sessionID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
	if got := r.turnRow(ctx, t, turnID).Status; got != sqlcgen.TurnStatusCompleted {
		t.Fatalf("attempt = %s after its execution_complete, want completed", got)
	}
	r.agentSnapshotReady(ctx, t, sessionID, 1)
}

// TestStopSession_HITLDecisionResumes: a workflow step awaiting a person's
// decision when the session is stopped keeps waiting, and the decision is a
// new human act, as approving the session's plan is (technical plan §3.3).
// Approving or revising the step resumes the session: the decide route
// clears the stop request in its own transaction, the attempt it
// dispatches runs, and the run goes on through its remaining steps to
// completion -- left standing, the request would let that attempt run and
// then end the run cancelled. Rejecting it ends the run failed, as it did
// before the stop existed, dispatches nothing, and leaves the stop
// standing, as rejecting a plan does. The real decide route behind the
// real auth middleware, with the real actor.
func TestStopSession_HITLDecisionResumes(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		verdict string
		body    string
	}{
		{"approve", `{"verdict":"approve","text":null}`},
		{"revise", `{"verdict":"revise","text":"narrow it to the parser"}`},
		{"reject", `{"verdict":"reject","text":null}`},
	} {
		t.Run(tc.verdict, func(t *testing.T) {
			rig := newStopRig(t, stopRigConfig{})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			rig.readySandbox(ctx, t, session.ID, 1)
			run := rig.startHITLRun(ctx, t, session.ID)

			// The first step's attempt completes; the step waits for a person.
			agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
			rig.agentSnapshotReady(ctx, t, session.ID, 1)
			awaiting, _ := rig.stepRunIn(ctx, t, run.id, "awaiting_decision")

			status, resp := rig.stop(t, session.ID.String(), token)
			if status != http.StatusAccepted || resp.OpenTurns != 0 {
				t.Fatalf("stop: status %d, %d open turns; want 202 with none open", status, resp.OpenTurns)
			}
			pumpUntil(ctx, t, rig.registry, 10*time.Second, "the stop, with nothing open, handled", func() bool {
				return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
			})
			if runStatus, steps := rig.runState(ctx, t, run.id); runStatus != "running" || !slices.Equal(steps, []string{"1:awaiting_decision"}) {
				t.Fatalf("after the stop: run %s, step runs %v; want the step still awaiting its decision", runStatus, steps)
			}

			decided := rig.decide(t, token, run.id, awaiting, tc.body)
			stopStands := rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid

			if tc.verdict == "reject" {
				if decided.RunStatus != "failed" || decided.TurnId != nil || !stopStands {
					t.Fatalf("reject: run %s, turn %v, stop standing %v; want the run failed, no turn, and the stop left standing", decided.RunStatus, decided.TurnId, stopStands)
				}
				actorBarrier(ctx, t, rig.registry, session.ID)
				runStatus, steps := rig.runState(ctx, t, run.id)
				turns, err := rig.turns.ListForSession(ctx, session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if runStatus != "failed" || !slices.Equal(steps, []string{"1:failed"}) || len(turns) != 1 || len(rig.commander.ofType(t, "prompt")) != 0 {
					t.Fatalf("reject: run %s, step runs %v, %d turns, prompts %v; want the run failed and nothing dispatched", runStatus, steps, len(turns), rig.commander.promptGens(t))
				}
				return
			}

			if decided.RunStatus != "running" || decided.TurnId == nil || stopStands {
				t.Fatalf("%s: run %s, turn %v, stop standing %v; want the run running with a turn, and the session resumed", tc.verdict, decided.RunStatus, decided.TurnId, stopStands)
			}
			var next pgtype.UUID
			if err := next.Scan(string(*decided.TurnId)); err != nil {
				t.Fatal(err)
			}
			want := []string{"1:completed"}

			if tc.verdict == "revise" {
				// The revised attempt runs, and stops at step 1's gate again:
				// the run goes on, awaiting the next decision.
				rig.runAttempt(ctx, t, session.ID, next)
				runStatus, steps := rig.runState(ctx, t, run.id)
				if runStatus != "running" || !slices.Equal(steps, []string{"1:completed", "1:awaiting_decision"}) {
					t.Fatalf("after the revised attempt: run %s, step runs %v; want the run running, step 1 awaiting its decision again", runStatus, steps)
				}
				awaiting, _ = rig.stepRunIn(ctx, t, run.id, "awaiting_decision")
				decided = rig.decide(t, token, run.id, awaiting, `{"verdict":"approve","text":null}`)
				if decided.RunStatus != "running" || decided.TurnId == nil {
					t.Fatalf("approve after the revision: run %s, turn %v", decided.RunStatus, decided.TurnId)
				}
				if err := next.Scan(string(*decided.TurnId)); err != nil {
					t.Fatal(err)
				}
				want = append(want, "1:completed")
			}

			// The second step's attempt runs and queues the third's, which
			// runs and ends the run.
			rig.runAttempt(ctx, t, session.ID, next)
			_, step3 := rig.stepRunIn(ctx, t, run.id, "running")
			rig.runAttempt(ctx, t, session.ID, step3)

			runStatus, steps := rig.runState(ctx, t, run.id)
			want = append(want, "2:completed", "3:completed")
			if runStatus != "completed" || !slices.Equal(steps, want) {
				t.Fatalf("run %s, step runs %v; want completed, %v", runStatus, steps, want)
			}
			for _, gen := range rig.commander.promptGens(t) {
				if gen != 1 {
					t.Fatalf("prompt frames at gens %v, want every one at gen 1", rig.commander.promptGens(t))
				}
			}
		})
	}
}

// TestStopSession_DecisionAndStopOrdered: a person's decision on a workflow
// step awaiting it and a stop of the same session, racing, are ordered by
// the session's actor-epoch lock, which both take before any write
// (technical plan §3.3). A stop that commits first is the request an
// approval or a revision clears: the attempt it dispatches carries no flag,
// and no stop is left standing. A decision that commits first inserts an
// attempt the stop then flags: the stop stands, and cancels it. Each order
// is forced: the first act is held inside its transaction, after its lock
// and its writes to the session, until the second waits on a lock too.
// Without the lock, both orders end with the stop standing over an
// unflagged attempt, which runs and then ends the run cancelled.
func TestStopSession_DecisionAndStopOrdered(t *testing.T) {
	ctx := context.Background()

	type stopResult struct {
		status int
		resp   restdtos.StopSessionResponse
		err    error
	}
	type decideResult struct {
		status int
		raw    []byte
		err    error
	}

	for _, verdict := range []struct {
		name string
		body string
	}{
		{"approve", `{"verdict":"approve","text":null}`},
		{"revise", `{"verdict":"revise","text":"narrow it to the parser"}`},
	} {
		for _, stopFirst := range []bool{true, false} {
			name := verdict.name + ", the decision first"
			if stopFirst {
				name = verdict.name + ", the stop first"
			}
			t.Run(name, func(t *testing.T) {
				rig := newStopRig(t, stopRigConfig{})
				owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
				session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
				rig.readySandbox(ctx, t, session.ID, 1)
				run := rig.startHITLRun(ctx, t, session.ID)
				agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
				rig.agentSnapshotReady(ctx, t, session.ID, 1)
				awaiting, _ := rig.stepRunIn(ctx, t, run.id, "awaiting_decision")

				hold, err := rig.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = hold.Rollback(ctx) }()
				if stopFirst {
					// The stop's last write is its audit row: it waits there,
					// having flagged the session.
					if _, err := hold.Exec(ctx, `LOCK TABLE audit_log IN SHARE MODE`); err != nil {
						t.Fatal(err)
					}
				} else {
					// The decision's attempt is a new step run of this run:
					// its foreign key waits on the run's row, after the
					// decision has taken the session's lock and cleared any
					// stop.
					if _, err := hold.Exec(ctx, `SELECT id FROM workflow_runs WHERE id = $1 FOR UPDATE`, run.id); err != nil {
						t.Fatal(err)
					}
				}

				stopped := make(chan stopResult, 1)
				decided := make(chan decideResult, 1)
				startStop := func() {
					go func() {
						status, resp, err := rig.stopRequest(session.ID.String(), token)
						stopped <- stopResult{status, resp, err}
					}()
				}
				startDecision := func() {
					go func() {
						status, raw, err := rig.postRequest("/api/workflow-runs/"+run.id.String()+"/steps/"+awaiting.String()+"/decide", token, []byte(verdict.body))
						decided <- decideResult{status, raw, err}
					}()
				}
				first, second, secondDone := startStop, startDecision, func() bool { return len(decided) == 1 }
				if !stopFirst {
					first, second, secondDone = startDecision, startStop, func() bool { return len(stopped) == 1 }
				}
				first()
				stopEventually(t, 10*time.Second, "the first act waits inside its transaction", func() bool { return rig.lockWaiters(ctx, t) >= 1 })
				second()
				// The second waits on a lock too -- or, with nothing to order
				// the two, runs to its end.
				stopEventually(t, 10*time.Second, "the second act waits on a lock, or ends", func() bool { return rig.lockWaiters(ctx, t) >= 2 || secondDone() })
				if err := hold.Rollback(ctx); err != nil {
					t.Fatal(err)
				}

				var stop stopResult
				var decision decideResult
				for _, wait := range []func(){
					func() {
						select {
						case stop = <-stopped:
						case <-time.After(10 * time.Second):
							t.Fatal("the stop never answered")
						}
					},
					func() {
						select {
						case decision = <-decided:
						case <-time.After(10 * time.Second):
							t.Fatal("the decision never answered")
						}
					},
				} {
					wait()
				}
				if stop.err != nil || decision.err != nil {
					t.Fatalf("stop: %v; decision: %v", stop.err, decision.err)
				}
				if stop.status != http.StatusAccepted || decision.status != http.StatusOK {
					t.Fatalf("stop status %d, decision status %d %s; want 202 and 200", stop.status, decision.status, decision.raw)
				}
				var resp restdtos.WorkflowStepDecideResponse
				if err := json.Unmarshal(decision.raw, &resp); err != nil {
					t.Fatal(err)
				}
				if resp.TurnId == nil {
					t.Fatalf("decision %s dispatched no attempt", decision.raw)
				}
				var attempt pgtype.UUID
				if err := attempt.Scan(string(*resp.TurnId)); err != nil {
					t.Fatal(err)
				}
				stopStands := rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid
				flagged := rig.turnRow(ctx, t, attempt).StopRequestedAt.Valid

				if stopFirst {
					if stopStands || flagged || stop.resp.OpenTurns != 0 {
						t.Fatalf("the stop first: stop standing %v, attempt flagged %v, the stop found %d open turns; want the decision to clear the stop it waited for, and its attempt unflagged", stopStands, flagged, stop.resp.OpenTurns)
					}
					rig.runAttempt(ctx, t, session.ID, attempt)
					if runStatus, steps := rig.runState(ctx, t, run.id); runStatus != "running" {
						t.Fatalf("the stop first: run %s, step runs %v once the attempt ran; want the run going on", runStatus, steps)
					}
					return
				}
				if !stopStands || !flagged || stop.resp.OpenTurns != 1 {
					t.Fatalf("the decision first: stop standing %v, attempt flagged %v, the stop found %d open turns; want the stop to wait for the decision and flag its attempt", stopStands, flagged, stop.resp.OpenTurns)
				}
				pumpUntil(ctx, t, rig.registry, 10*time.Second, "the stop cancels the attempt it flagged", func() bool {
					return rig.turnRow(ctx, t, attempt).Status == sqlcgen.TurnStatusCancelled
				})
				if runStatus, steps := rig.runState(ctx, t, run.id); runStatus != "cancelled" {
					t.Fatalf("the decision first: run %s, step runs %v; want the run cancelled by the stop", runStatus, steps)
				}
			})
		}
	}
}

// TestStopSession_RepeatKeepsEachTurnsFirstFlag: a repeated stop moves the
// session's request to its own instant, but a turn it finds already
// flagged keeps its first flag (technical plan §3.3), so the grace of a
// turn in flight runs from the first request: a person repeating the stop
// cannot push a silent turn's cancel back, one grace at a time.
// TestStopSession_RepeatDisarmsTimersArmedSinceTheFirst pins the session's
// own instant.
func TestStopSession_RepeatKeepsEachTurnsFirstFlag(t *testing.T) {
	ctx := context.Background()
	const grace = 3 * time.Second
	rig := newStopRig(t, stopRigConfig{grace: grace})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
	rig.readySandbox(ctx, t, session.ID, 1)
	running := rig.processingTurn(ctx, t, session.ID, 1)

	status, first := rig.stop(t, session.ID.String(), token)
	if status != http.StatusAccepted {
		t.Fatalf("first stop: status %d", status)
	}
	flag := rig.turnRow(ctx, t, running.ID).StopRequestedAt.Time
	stopEventually(t, 10*time.Second, "the first stop is sent and the timer armed for its grace's end", func() bool {
		firesAt, armed := rig.stopTimer(ctx, t, session.ID)
		return len(rig.commander.ofType(t, "stop")) == 1 && armed && firesAt.Equal(flag.Add(grace))
	})

	// Repeated inside the grace.
	time.Sleep(grace / 2)
	status, second := rig.stop(t, session.ID.String(), token)
	if status != http.StatusAccepted || !second.RequestedAt.After(first.RequestedAt) {
		t.Fatalf("second stop: status %d, requestedAt %v after %v; want 202 at its own, later instant", status, second.RequestedAt, first.RequestedAt)
	}
	stopEventually(t, 10*time.Second, "the repeat is handled", func() bool { return len(rig.commander.ofType(t, "stop")) == 2 })
	if got := rig.turnRow(ctx, t, running.ID).StopRequestedAt.Time; !got.Equal(flag) {
		t.Fatalf("the repeat moved the turn's flag from %v to %v", flag, got)
	}
	if firesAt, armed := rig.stopTimer(ctx, t, session.ID); !armed || !firesAt.Equal(flag.Add(grace)) {
		t.Fatalf("stop timer armed %v for %v after the repeat, want the first flag's grace end %v", armed, firesAt, flag.Add(grace))
	}

	// The agent stays silent: the first flag's grace cancels the turn.
	time.Sleep(time.Until(flag.Add(grace)))
	pumpUntil(ctx, t, rig.registry, 10*time.Second, "the first flag's grace cancels the silent turn", func() bool {
		return rig.turnRow(ctx, t, running.ID).Status == sqlcgen.TurnStatusCancelled
	})
	if ended := rig.turnRow(ctx, t, running.ID).CompletedAt.Time; !ended.Before(second.RequestedAt.Add(grace)) {
		t.Fatalf("turn cancelled at %v, not before the repeat's own grace end %v: the repeat pushed its grace back", ended, second.RequestedAt.Add(grace))
	}
}

// TestStopSession_RetirementWaitsForADelivery: a stopped turn whose grace
// ends while its sandbox gen still delivers the push and pull request of
// the turn that completed before it is cancelled all the same, at the
// grace's end; only the gen's retirement waits, since stopping the sandbox
// would kill that delivery, which technical plan §3.3 keeps as the
// completed turn's result. Meanwhile the gen takes nothing more: the
// stopped work's own late execution_complete completes nothing, pushes
// nothing and starts no snapshot, and a person's next prompt is accepted
// but not dispatched to it. The stop timer looks again every StopGrace,
// never past MCPStatusDeliveryWindow from the delivery's start, and its
// first fire after the delivery ends -- its push failed -- or after the
// window has run retires the gen, and the prompt goes to a new one. A gen
// that dies meanwhile is replaced for the prompt, and the new gen owes
// nothing.
func TestStopSession_RetirementWaitsForADelivery(t *testing.T) {
	ctx := context.Background()
	const grace = 2 * time.Second

	for _, tc := range []struct {
		name string
		// retires is whether the stop timer retires gen 1 itself, and so
		// stops its provider object.
		retires bool
		// deliveryEnds ends the delivery, lets its window run out, or ends
		// the sandbox.
		deliveryEnds func(t *testing.T, rig *stopRig, sessionID pgtype.UUID)
	}{
		{"its push fails", true, func(t *testing.T, rig *stopRig, sessionID pgtype.UUID) {
			agentEvent(ctx, t, rig.registry, sessionID, "push_error", 1, func(messageID string) any {
				return sandboxws.PushError{Type: "push_error", MessageId: messageID, SessionId: sessionID.String(), Gen: 1, AckId: "push_error:" + messageID, Error: "remote rejected"}
			})
			if rig.sandboxRow(ctx, t, sessionID).PrDeliveryStartedAt.Valid {
				t.Fatal("push_error left the delivery stamped")
			}
		}},
		{"its window runs out", true, func(t *testing.T, rig *stopRig, sessionID pgtype.UUID) {
			// The window now ends a second after the stop timer's next
			// look, so that look still finds the delivery under way, and
			// must look again at the window's end -- not a grace later,
			// past it.
			next, armed := rig.stopTimer(ctx, t, sessionID)
			if !armed {
				t.Fatal("the stop timer is not armed while the retirement waits")
			}
			window := rig.timeouts.MCPStatusDeliveryWindow
			if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = $2 WHERE session_id = $1`, sessionID, next.Add(time.Second-window)); err != nil {
				t.Fatal(err)
			}
			end := rig.sandboxRow(ctx, t, sessionID).PrDeliveryStartedAt.Time.Add(window)
			pumpUntil(ctx, t, rig.registry, 15*time.Second, "the stop timer's look before the window's end", func() bool {
				firesAt, armed := rig.stopTimer(ctx, t, sessionID)
				return !armed || !firesAt.Equal(next)
			})
			// The pump's claim moved the timer; the handler's own re-arm is
			// in once the actor has handled the fire.
			actorBarrier(ctx, t, rig.registry, sessionID)
			if rearmed, armed := rig.stopTimer(ctx, t, sessionID); !armed || !rearmed.Equal(end) {
				t.Fatalf("stop timer armed %v for %v, want it re-armed for the delivery window's end %v", armed, rearmed, end)
			}
		}},
		{"its sandbox dies, and the next turn respawns it", false, func(t *testing.T, rig *stopRig, sessionID pgtype.UUID) {
			// The liveness watchdog found the sandbox silent, and its
			// terminal grace fails it. The prompt waiting on the gen then
			// has it restored under a new gen, before the stop timer's next
			// look.
			if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'suspect', pre_suspect_status = 'ready' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
			actor, err := rig.registry.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := actor.Send(ctx, sessionactor.TimerFired{Name: sessionactor.TimerTerminalGrace}); err != nil {
				t.Fatal(err)
			}
			actorBarrier(ctx, t, rig.registry, sessionID)
			if sb := rig.sandboxRow(ctx, t, sessionID); sb.Gen != 2 || sb.StopRetireGen != nil {
				t.Fatalf("sandbox at gen %d, retirement owed on %v once the dead gen was replaced; want gen 2, owing nothing", sb.Gen, sb.StopRetireGen)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stopProvider{}
			rig := newStopRig(t, stopRigConfig{grace: grace, provider: provider})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.deliveringSession(ctx, t, owner.ID, pgtype.UUID{})
			rig.readySandbox(ctx, t, session.ID, 1)
			rig.processingTurn(ctx, t, session.ID, 1)

			// The turn before completes: its push is sent, and its delivery
			// stamped until the push reports back.
			agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
			if pushes := rig.commander.ofType(t, "push"); len(pushes) != 1 || !rig.sandboxRow(ctx, t, session.ID).PrDeliveryStartedAt.Valid {
				t.Fatalf("setup: %d push frames, delivery stamped %v; want the completed turn's push sent and its delivery under way", len(pushes), rig.sandboxRow(ctx, t, session.ID).PrDeliveryStartedAt.Valid)
			}
			rig.agentSnapshotReady(ctx, t, session.ID, 1)

			stopped := rig.prompt(t, session.ID, token, "next")
			stopEventually(t, 10*time.Second, "the next turn dispatches", func() bool {
				return rig.turnRow(ctx, t, stopped).Status == sqlcgen.TurnStatusProcessing
			})
			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}

			// The stop's first fire tells the agent and arms the timer for
			// exactly the grace's end, when it is next due.
			flag := rig.turnRow(ctx, t, stopped).StopRequestedAt.Time
			stopEventually(t, 10*time.Second, "the stop is sent and the timer armed for the grace's end", func() bool {
				firesAt, armed := rig.stopTimer(ctx, t, session.ID)
				return len(rig.commander.ofType(t, "stop")) >= 1 && armed && firesAt.Equal(flag.Add(grace))
			})

			// The grace runs out with the agent silent, still on the push.
			// The fire at the grace's end cancels the turn, and keeps its
			// gen, the retirement owed.
			time.Sleep(time.Until(flag.Add(grace)))
			pumpUntil(ctx, t, rig.registry, 15*time.Second, "the grace's end cancels the stopped turn", func() bool {
				return rig.turnRow(ctx, t, stopped).Status == sqlcgen.TurnStatusCancelled
			})
			sb := rig.sandboxRow(ctx, t, session.ID)
			if sb.Status != sqlcgen.SandboxStatusReady || sb.Gen != 1 || !sb.PrDeliveryStartedAt.Valid || sb.StopRetireGen == nil || *sb.StopRetireGen != 1 {
				t.Fatalf("sandbox = %s at gen %d, delivery stamped %v, retirement owed on %v; want ready at gen 1, still delivering, its retirement owed", sb.Status, sb.Gen, sb.PrDeliveryStartedAt.Valid, sb.StopRetireGen)
			}
			if _, _, stoppedObjects := provider.calls(); len(stoppedObjects) != 0 {
				t.Fatalf("provider stops = %v while the delivery is under way, want none", stoppedObjects)
			}
			if n := rig.syntheticCompletes(ctx, t, session.ID)[stopped.String()]; n != 1 {
				t.Fatalf("%d synthetic execution_complete events for the stopped turn, want exactly 1", n)
			}
			// Looked at again within a grace.
			if firesAt, armed := rig.stopTimer(ctx, t, session.ID); !armed || firesAt.After(time.Now().Add(grace+time.Second)) {
				t.Fatalf("stop timer armed %v for %v; want it re-armed within a grace", armed, firesAt)
			}

			// The gen takes nothing more. The stopped work's own late end
			// completes nothing, pushes nothing, and starts no snapshot of
			// what it changed ...
			pushes, snapshots := len(rig.commander.ofType(t, "push")), len(rig.commander.ofType(t, "snapshot"))
			agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
			if got := rig.turnRow(ctx, t, stopped).Status; got != sqlcgen.TurnStatusCancelled {
				t.Fatalf("stopped turn = %s after the stopped work's late end, want still cancelled", got)
			}
			if n, m := len(rig.commander.ofType(t, "push")), len(rig.commander.ofType(t, "snapshot")); n != pushes || m != snapshots {
				t.Fatalf("the stopped work's late end sent %d push and %d snapshot frames, want none", n-pushes, m-snapshots)
			}
			if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != sqlcgen.SandboxStatusReady || sb.Gen != 1 {
				t.Fatalf("sandbox = %s at gen %d after the stopped work's late end, want still ready at gen 1", sb.Status, sb.Gen)
			}
			// ... and a person's next prompt is accepted, and waits.
			next := rig.prompt(t, session.ID, token, "carry on")
			actorBarrier(ctx, t, rig.registry, session.ID)
			if gens := rig.commander.promptGens(t); !slices.Equal(gens, []int{1}) {
				t.Fatalf("prompt frames at gens %v, want only the stopped turn's at gen 1: the next turn went to the gen the stop has still to retire", gens)
			}
			if got := rig.turnRow(ctx, t, next).Status; got != sqlcgen.TurnStatusPending {
				t.Fatalf("next turn = %s while the retirement is owed, want pending", got)
			}

			tc.deliveryEnds(t, rig, session.ID)
			pumpUntil(ctx, t, rig.registry, 20*time.Second, "the gen retired once the delivery is over", func() bool {
				sb := rig.sandboxRow(ctx, t, session.ID)
				return sb.Gen != 1 || sb.Status != sqlcgen.SandboxStatusReady
			})
			// The retirement's own dispatch -- the restore of a new gen --
			// has run.
			actorBarrier(ctx, t, rig.registry, session.ID)
			if sb := rig.sandboxRow(ctx, t, session.ID); sb.Gen != 2 || sb.Status != sqlcgen.SandboxStatusConnecting || sb.StopRetireGen != nil {
				t.Fatalf("sandbox = %s at gen %d, retirement owed on %v; want gen 1 retired, the next turn's gen 2 connecting, nothing owed", sb.Status, sb.Gen, sb.StopRetireGen)
			}
			var wantStops []string
			if tc.retires {
				wantStops = []string{providerIDForGen(1)}
			}
			if _, restored, stoppedObjects := provider.calls(); !slices.Equal(stoppedObjects, wantStops) || !slices.Equal(restored, []int{2}) {
				t.Fatalf("provider stops = %v, restores %v; want stops %v, and gen 2 restored", stoppedObjects, restored, wantStops)
			}
			pumpUntil(ctx, t, rig.registry, 10*time.Second, "the stop timer ends", func() bool {
				_, armed := rig.stopTimer(ctx, t, session.ID)
				return !armed
			})
			if n := rig.syntheticCompletes(ctx, t, session.ID)[stopped.String()]; n != 1 {
				t.Fatalf("%d synthetic execution_complete events for the stopped turn, want exactly 1", n)
			}
		})
	}
}

// TestStopSession_DockerSessionsStoppedTurnNeverPushed: a Docker-required
// session takes no snapshot (§27.8), so a turn queued behind the one that
// completes is sent its prompt before that turn's push, and the agent,
// which runs a prompt while it handles a push, really runs it while the
// frames sent after the push -- its stop included -- wait. Its grace still
// cancels it at the grace's end (technical plan §3.3), while the completed
// turn's delivery goes on: its own late execution_complete{completed}
// finds nothing processing, so nothing is completed or pushed for it, and
// the gen is retired once the delivery is over. Left in flight through the
// delivery, the turn would be recorded completed and its work pushed.
func TestStopSession_DockerSessionsStoppedTurnNeverPushed(t *testing.T) {
	ctx := context.Background()
	const grace = 2 * time.Second
	provider := &stopProvider{docker: true}
	rig := newStopRig(t, stopRigConfig{grace: grace, provider: provider})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	env, err := narvipg.NewEnvironmentStore(rig.pool).Create(ctx, sqlcgen.CreateEnvironmentParams{DockerRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	session := rig.deliveringSession(ctx, t, owner.ID, env.ID)
	rig.readySandbox(ctx, t, session.ID, 1)
	rig.processingTurn(ctx, t, session.ID, 1)
	// Queued behind it, as a workflow's next step or a code-host mention is.
	queued := rig.pendingTurn(ctx, t, session.ID)

	before := rig.commander.count()
	agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
	if got := rig.commander.types(t, before); !slices.Equal(got, []string{"prompt", "push"}) {
		t.Fatalf("frames after the completed turn = %v, want [prompt push]: no snapshot, so the queued turn's prompt goes out before the push", got)
	}
	if got := rig.turnRow(ctx, t, queued.ID).Status; got != sqlcgen.TurnStatusProcessing || !rig.sandboxRow(ctx, t, session.ID).PrDeliveryStartedAt.Valid {
		t.Fatalf("setup: queued turn = %s, delivery stamped %v; want it processing, with the completed turn's delivery under way", got, rig.sandboxRow(ctx, t, session.ID).PrDeliveryStartedAt.Valid)
	}

	if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
		t.Fatalf("stop: status %d", status)
	}
	flag := rig.turnRow(ctx, t, queued.ID).StopRequestedAt.Time
	stopEventually(t, 10*time.Second, "the stop is sent and the timer armed for the grace's end", func() bool {
		firesAt, armed := rig.stopTimer(ctx, t, session.ID)
		return len(rig.commander.ofType(t, "stop")) >= 1 && armed && firesAt.Equal(flag.Add(grace))
	})

	// The agent is on the push, running the turn; the stop waits behind the
	// push. The grace's end cancels the turn all the same.
	time.Sleep(time.Until(flag.Add(grace)))
	pumpUntil(ctx, t, rig.registry, 15*time.Second, "the grace's end cancels the stopped turn", func() bool {
		return rig.turnRow(ctx, t, queued.ID).Status == sqlcgen.TurnStatusCancelled
	})
	if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != sqlcgen.SandboxStatusReady || sb.Gen != 1 || sb.StopRetireGen == nil || *sb.StopRetireGen != 1 {
		t.Fatalf("sandbox = %s at gen %d, retirement owed on %v; want ready at gen 1 while it delivers, its retirement owed", sb.Status, sb.Gen, sb.StopRetireGen)
	}
	if n := rig.syntheticCompletes(ctx, t, session.ID)[queued.ID.String()]; n != 1 {
		t.Fatalf("%d synthetic execution_complete events for the stopped turn, want exactly 1", n)
	}

	// The stopped work finishes, and reports it: nothing is processing, so
	// nothing is completed or pushed for it.
	agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCompleted)
	if got := rig.turnRow(ctx, t, queued.ID).Status; got != sqlcgen.TurnStatusCancelled {
		t.Fatalf("stopped turn = %s after the stopped work's own end, want still cancelled", got)
	}
	if pushes := rig.commander.ofType(t, "push"); len(pushes) != 1 {
		t.Fatalf("%d push frames, want only the completed turn's: the stopped turn's work was pushed", len(pushes))
	}

	// The completed turn's delivery goes on, and ends; then the gen is
	// retired.
	agentEvent(ctx, t, rig.registry, session.ID, "push_complete", 1, func(messageID string) any {
		return sandboxws.PushComplete{
			Type: "push_complete", MessageId: messageID, SessionId: session.ID.String(), Gen: 1, AckId: "push_complete:" + messageID,
			Repos: []sandboxws.PushCompleteReposElem{{Name: "widgets", Branch: "narvi/feature", Sha: "0123456789abcdef0123456789abcdef01234567"}},
		}
	})
	if rig.sandboxRow(ctx, t, session.ID).PrDeliveryStartedAt.Valid {
		t.Fatal("setup: the completed turn's delivery is still stamped once its push_complete was handled")
	}
	pumpUntil(ctx, t, rig.registry, 15*time.Second, "the gen retired once the delivery is over", func() bool {
		return rig.sandboxRow(ctx, t, session.ID).Status == sqlcgen.SandboxStatusStopped
	})
	// The provider stop comes after the retirement's commit.
	actorBarrier(ctx, t, rig.registry, session.ID)
	if sb := rig.sandboxRow(ctx, t, session.ID); sb.Gen != 1 || sb.StopRetireGen != nil {
		t.Fatalf("sandbox at gen %d, retirement owed on %v; want gen 1 retired, nothing owed", sb.Gen, sb.StopRetireGen)
	}
	if _, _, stoppedObjects := provider.calls(); !slices.Equal(stoppedObjects, []string{providerIDForGen(1)}) {
		t.Fatalf("provider stops = %v, want the retired gen's object %s", stoppedObjects, providerIDForGen(1))
	}
	pumpUntil(ctx, t, rig.registry, 10*time.Second, "the stop timer ends", func() bool {
		_, armed := rig.stopTimer(ctx, t, session.ID)
		return !armed
	})
	if pushes, gens := rig.commander.ofType(t, "push"), rig.commander.promptGens(t); len(pushes) != 1 || !slices.Equal(gens, []int{1}) {
		t.Fatalf("%d push frames, prompts at gens %v; want the completed turn's one push and the stopped turn's one prompt", len(pushes), gens)
	}
}

// TestStopSession_RepeatDisarmsTimersArmedSinceTheFirst: a repeated stop
// moves the session's request to its own instant, so the stop timer's
// handler also disarms a work-creating timer armed between the two
// requests -- the re-review debounce a push armed after the first -- which
// the person stopped over too. requestedAt answers the latest request.
func TestStopSession_RepeatDisarmsTimersArmedSinceTheFirst(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})
	owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
	session := rig.session(ctx, t, owner.ID, pgtype.UUID{})

	status, first := rig.stop(t, session.ID.String(), token)
	if status != http.StatusAccepted {
		t.Fatalf("first stop: status %d", status)
	}
	pumpUntil(ctx, t, rig.registry, 10*time.Second, "the first stop handled", func() bool {
		return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
	})
	// A push after the first stop arms the debounce.
	if _, err := rig.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: session.ID, Name: sessionactor.TimerReviewRetriggerDebounce,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("arm debounce: %v", err)
	}

	status, second := rig.stop(t, session.ID.String(), token)
	if status != http.StatusAccepted {
		t.Fatalf("second stop: status %d", status)
	}
	pumpUntil(ctx, t, rig.registry, 10*time.Second, "the second stop handled", func() bool {
		return !slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
	})
	if names := rig.timerNames(ctx, t, session.ID); slices.Contains(names, sessionactor.TimerReviewRetriggerDebounce) {
		t.Fatalf("timers = %v: the debounce armed between the two stops survived the second", names)
	}
	if !second.RequestedAt.After(first.RequestedAt) {
		t.Fatalf("requestedAt = %v, then %v: want the second request's own, later instant", first.RequestedAt, second.RequestedAt)
	}
	if got := rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Time; !got.Equal(second.RequestedAt) {
		t.Fatalf("sessions.stop_requested_at = %v, want the latest request's %v", got, second.RequestedAt)
	}
}

// TestStopSession_WorkflowCancelAfterAResumeEndsTheRun: a cancel the stop
// asked for ends its workflow run cancelled even once a person has resumed
// the session (technical plan §3.3): with the session's request cleared,
// the attempt's own flag is what keeps its implicit 'blocked' outcome from
// taking a custom definition's edge to the next step. Resumed here the one
// way a person can while the attempt is still in flight -- a code-host
// mention, which queues behind it (CreateTurnForBot, AlwaysQueue) -- and
// cancelled both ways a stop cancels a running attempt: the agent's own
// execution_complete{cancelled}, and the grace's synthetic one.
func TestStopSession_WorkflowCancelAfterAResumeEndsTheRun(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		agentCancels bool
	}{
		{"the agent's cancel", true},
		{"the grace's cancel", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStopRig(t, stopRigConfig{grace: 2 * time.Second})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			rig.readySandbox(ctx, t, session.ID, 1)

			var defID, firstStep, nextStep pgtype.UUID
			if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', 'stop-resume-test', false, 1) RETURNING id`).Scan(&defID); err != nil {
				t.Fatal(err)
			}
			for order, id := range map[int]*pgtype.UUID{1: &firstStep, 2: &nextStep} {
				if err := rig.pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, $2, 'agent', '{{prompt}}') RETURNING id`, defID, order).Scan(id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := rig.pool.Exec(ctx, `INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status) VALUES ($1, $2, $3, 'blocked')`, defID, firstStep, nextStep); err != nil {
				t.Fatal(err)
			}
			run, err := rig.workflows.CreateRun(ctx, session.ID, "request", defID, 1)
			if err != nil {
				t.Fatal(err)
			}
			stepRun, err := rig.workflows.CreateStepRun(ctx, run.ID, firstStep)
			if err != nil {
				t.Fatal(err)
			}
			attempt := rig.processingTurn(ctx, t, session.ID, 1)
			if err := rig.workflows.AttachTurn(ctx, stepRun.ID, attempt.ID); err != nil {
				t.Fatal(err)
			}

			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}
			stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })

			if _, err := httpapi.CreateTurnForBot(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, turnguard.New(rig.pool, nil, false), session.ID,
				"@narvi carry on", nil, false, false, owner.ID, nil, nil, nil, nil, nil, nil, nil, nil, false, nil, nil); err != nil {
				t.Fatalf("mention: %v", err)
			}
			if rig.sessionRow(ctx, t, session.ID).StopRequestedAt.Valid || !rig.turnRow(ctx, t, attempt.ID).StopRequestedAt.Valid || rig.turnRow(ctx, t, attempt.ID).Status != sqlcgen.TurnStatusProcessing {
				t.Fatal("setup: want the session resumed, with the flagged attempt still in flight")
			}

			if tc.agentCancels {
				agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCancelled)
			}
			pumpUntil(ctx, t, rig.registry, 15*time.Second, "the attempt cancelled", func() bool {
				return rig.turnRow(ctx, t, attempt.ID).Status == sqlcgen.TurnStatusCancelled
			})

			runStatus, steps := rig.runState(ctx, t, run.ID)
			if runStatus != "cancelled" || !slices.Equal(steps, []string{"1:cancelled"}) {
				t.Fatalf("run %s, step runs %v; want the run cancelled with no next step, after a resume", runStatus, steps)
			}
		})
	}
}

// TestStopSession_DeadSandboxAtTheTurnsGenEndsTheStop: a stopped turn whose
// sandbox dies at the turn's own gen within its grace is still cancelled
// once the grace has run, and the stop ends. There is no gen to retire --
// a dead sandbox has no edge to suspect, and planDispatch never respawns
// for a flagged turn in flight, so the gen does not move -- and the
// provider is not asked to stop or spawn anything. Were a dead sandbox
// retired like a live one, the illegal transition would roll the cancel
// back at every fire, and the turn would stay processing until its
// turn_deadline. The reachable way: the liveness watchdog found the
// sandbox silent, and its terminal grace failed it. A sandbox that dies in
// the middle of an earlier turn's push keeps its delivery stamp (nothing
// but a respawn, the pull request, a push_error or an unsendable push
// clears it), and a dead sandbox's delivery holds no retirement: there is
// none to hold, and waiting for a delivery it can never finish would keep
// the stop, and its timer, up to MCPStatusDeliveryWindow.
func TestStopSession_DeadSandboxAtTheTurnsGenEndsTheStop(t *testing.T) {
	ctx := context.Background()
	const grace = 3 * time.Second

	failedByTheWatchdog := func(t *testing.T, rig *stopRig, sessionID pgtype.UUID) {
		if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'suspect', pre_suspect_status = 'ready' WHERE session_id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
			SessionID: sessionID, Name: sessionactor.TimerTerminalGrace,
			FiresAt: pgtype.Timestamptz{Time: time.Now().Add(300 * time.Millisecond), Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
		pumpUntil(ctx, t, rig.registry, 10*time.Second, "the terminal grace fails the sandbox", func() bool {
			return rig.sandboxRow(ctx, t, sessionID).Status == sqlcgen.SandboxStatusFailed
		})
	}
	for _, tc := range []struct {
		name string
		want sqlcgen.SandboxStatus
		// delivering stamps an earlier turn's push and pull request as
		// still under way on the sandbox, before the stop.
		delivering bool
		// dies kills the sandbox at gen 1, within the grace.
		dies func(t *testing.T, rig *stopRig, sessionID pgtype.UUID)
	}{
		{"failed by the watchdog's terminal grace, mid-delivery", sqlcgen.SandboxStatusFailed, true, failedByTheWatchdog},
		{"failed by the watchdog's terminal grace", sqlcgen.SandboxStatusFailed, false, failedByTheWatchdog},
		{"stopped", sqlcgen.SandboxStatusStopped, false, func(t *testing.T, rig *stopRig, sessionID pgtype.UUID) {
			if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stopProvider{}
			rig := newStopRig(t, stopRigConfig{grace: grace, provider: provider})
			owner, token := rig.user(ctx, t, sqlcgen.UserRoleMember)
			session := rig.session(ctx, t, owner.ID, pgtype.UUID{})
			rig.readySandbox(ctx, t, session.ID, 1)
			if tc.delivering {
				if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = now() WHERE session_id = $1`, session.ID); err != nil {
					t.Fatal(err)
				}
			}
			running := rig.processingTurn(ctx, t, session.ID, 1)

			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}
			stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
			tc.dies(t, rig, session.ID)
			if got := rig.turnRow(ctx, t, running.ID).Status; got != sqlcgen.TurnStatusProcessing {
				t.Fatalf("setup: turn = %s once the sandbox died, want still processing within its grace", got)
			}
			if got := rig.sandboxRow(ctx, t, session.ID).PrDeliveryStartedAt.Valid; got != tc.delivering {
				t.Fatalf("setup: delivery stamped %v once the sandbox died, want %v", got, tc.delivering)
			}

			pumpUntil(ctx, t, rig.registry, 15*time.Second, "the grace cancels the turn and the stop ends", func() bool {
				return rig.turnRow(ctx, t, running.ID).Status == sqlcgen.TurnStatusCancelled &&
					!slices.Contains(rig.timerNames(ctx, t, session.ID), sessionactor.TimerStop)
			})
			if sb := rig.sandboxRow(ctx, t, session.ID); sb.Status != tc.want || sb.Gen != 1 || sb.StopRetireGen != nil {
				t.Fatalf("sandbox = %s at gen %d, retirement owed on %v; want %s at gen 1, nothing owed: a dead sandbox is left as it is", sb.Status, sb.Gen, sb.StopRetireGen, tc.want)
			}
			if created, restored, stoppedObjects := provider.calls(); len(created)+len(restored)+len(stoppedObjects) != 0 {
				t.Fatalf("provider spawns %v, restores %v, stops %v; want none", created, restored, stoppedObjects)
			}
			for _, name := range rig.timerNames(ctx, t, session.ID) {
				if name == sessionactor.TimerTurnDeadline || name == sessionactor.TimerTerminalGrace {
					t.Fatalf("timers = %v after the stop ended, want neither the turn's deadline nor a terminal grace", rig.timerNames(ctx, t, session.ID))
				}
			}
		})
	}
}

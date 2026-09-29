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
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/platform"
)

// Integration tests for POST /api/sessions/{sessionID}/stop (technical plan
// §3.3): real Postgres, the real session actor and timer pump, the real
// route behind the real auth middleware, and a fake sandbox that records
// every command frame the actor sends it.

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
// the fake sandbox, and a server mounting the stop route beside the turn
// and plan-approval routes a person resumes a session through.
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
	timeouts     platform.Timeouts
	commander    *stopCommander
	registry     *sessionactor.Registry
	server       *httptest.Server
}

// stopRigConfig tunes a rig: grace overrides StopGrace (0 keeps the shipped
// 30s); noWake mounts the route with no registry, so nothing but the timer
// pump -- or a dispatch -- reaches the actor.
type stopRigConfig struct {
	grace  time.Duration
	noWake bool
}

func newStopRig(t *testing.T, cfg stopRigConfig) *stopRig {
	t.Helper()
	pool := newTestPool(t)

	timeouts := platform.DefaultTimeouts()
	if cfg.grace > 0 {
		timeouts.StopGrace = cfg.grace
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
		timeouts:     timeouts,
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
			Pool:         pool,
			Sessions:     r.sessions,
			Turns:        r.turns,
			Timers:       r.timers,
			Participants: r.participants,
			AuditLog:     r.auditLog,
			Registry:     routeRegistry,
		}))
		api.Post("/{sessionID}/turns", httpapi.CreateTurn(pool, r.sessions, r.turns, r.plans, r.participants, r.auditLog, r.registry, nil, nil, false))
		api.Post("/{sessionID}/plans/{planId}/approve", httpapi.ApprovePlan(pool, r.sessions, r.turns, r.plans, narvipg.NewEventStore(pool), narvipg.NewPlanDocumentStore(pool), r.participants, narvipg.NewOutboxStore(pool, false), narvipg.NewLinearAgentSessionStore(pool), r.auditLog, r.registry, false))
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
	registry, err := sessionactor.NewRegistry(context.Background(), r.pool, r.timeouts, nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
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

// readySandbox gives sessionID a live sandbox at gen.
func (r *stopRig) readySandbox(ctx context.Context, t *testing.T, sessionID pgtype.UUID, gen int32) {
	t.Helper()
	if _, err := r.sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = $2 WHERE session_id = $1`, sessionID, gen); err != nil {
		t.Fatalf("make sandbox ready: %v", err)
	}
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
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, r.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
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

// syntheticCompletes returns the turn ids a synthetic execution_complete
// was recorded for on sessionID.
func (r *stopRig) syntheticCompletes(ctx context.Context, t *testing.T, sessionID pgtype.UUID) map[string]bool {
	t.Helper()
	rows, err := r.pool.Query(ctx, `SELECT payload->>'turn_id' FROM events
		WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`, sessionID)
	if err != nil {
		t.Fatalf("query synthetic events: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
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
// their own or joined ones, a viewer none. A refused request writes
// nothing -- no flag, no timer, no audit row. 400 and 404 are GET's.
func TestStopSession_AuthzMatrix(t *testing.T) {
	ctx := context.Background()
	rig := newStopRig(t, stopRigConfig{})

	type relation string
	const (
		creator relation = "creator"
		joined  relation = "joined"
		none    relation = "neither"
	)
	for _, tc := range []struct {
		role     sqlcgen.UserRole
		relation relation
		want     int
	}{
		{sqlcgen.UserRoleAdmin, none, http.StatusAccepted},
		{sqlcgen.UserRoleAdmin, creator, http.StatusAccepted},
		{sqlcgen.UserRoleMaintainer, none, http.StatusAccepted},
		{sqlcgen.UserRoleMaintainer, joined, http.StatusAccepted},
		{sqlcgen.UserRoleMember, creator, http.StatusAccepted},
		{sqlcgen.UserRoleMember, joined, http.StatusAccepted},
		{sqlcgen.UserRoleMember, none, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, creator, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, joined, http.StatusForbidden},
		{sqlcgen.UserRoleViewer, none, http.StatusForbidden},
	} {
		t.Run(fmt.Sprintf("%s_%s", tc.role, tc.relation), func(t *testing.T) {
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
		for _, q := range queued {
			if !synthetic[q.ID.String()] {
				t.Errorf("queued turn %s cancelled with no synthetic execution_complete", q.ID.String())
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
		if synthetic := rig.syntheticCompletes(ctx, t, session.ID); synthetic[running.ID.String()] {
			t.Errorf("the agent's own execution_complete ended the running turn; no synthetic one is owed")
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
		if !rig.syntheticCompletes(ctx, t, session.ID)[queued.ID.String()] {
			t.Fatal("the gate cancelled the turn with no synthetic execution_complete")
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
// synthetic execution_complete, and a late real execution_complete changes
// nothing.
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
			if !rig.syntheticCompletes(ctx, t, session.ID)[running.ID.String()] {
				t.Fatal("no synthetic execution_complete for the cancelled turn")
			}
			if names := rig.timerNames(ctx, t, session.ID); len(names) != 0 {
				t.Fatalf("timers = %v, want none: the stop and the turn's deadline both end with it", names)
			}
			row := rig.sessionRow(ctx, t, session.ID)
			if row.Status != sqlcgen.SessionStatusCancelled {
				t.Fatalf("session = %s, want cancelled", row.Status)
			}

			if tc.hasSandbox {
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
// running.
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

// TestStopSession_WorkflowRunEndsCancelled: an attempt a stop cancels ends
// its workflow run cancelled, and workflow.NextStep is not consulted -- the
// definition's edge on 'blocked' would otherwise queue its next step. Both
// ways a stop cancels an attempt's turn: the actor's own cancel of a queued
// one, and the agent's execution_complete{cancelled} answering the sandbox
// `stop`.
func TestStopSession_WorkflowRunEndsCancelled(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		running bool
	}{
		{"a queued attempt", false},
		{"a running attempt the agent cancels", true},
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

			if status, _ := rig.stop(t, session.ID.String(), token); status != http.StatusAccepted {
				t.Fatalf("stop: status %d", status)
			}
			if tc.running {
				stopEventually(t, 10*time.Second, "the stop is sent", func() bool { return len(rig.commander.ofType(t, "stop")) == 1 })
				agentReports(ctx, t, rig.registry, session.ID, 1, sandboxws.ExecutionCompleteOutcomeCancelled)
			}
			stopEventually(t, 10*time.Second, "the attempt's turn cancelled", func() bool {
				return rig.turnRow(ctx, t, attempt.ID).Status == sqlcgen.TurnStatusCancelled
			})

			var runStatus, stepStatus string
			var finished pgtype.Timestamptz
			if err := rig.pool.QueryRow(ctx, `SELECT status::text, finished_at FROM workflow_runs WHERE id = $1`, run.ID).Scan(&runStatus, &finished); err != nil {
				t.Fatal(err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT status::text FROM workflow_step_runs WHERE id = $1`, stepRun.ID).Scan(&stepStatus); err != nil {
				t.Fatal(err)
			}
			if runStatus != "cancelled" || !finished.Valid || stepStatus != "cancelled" {
				t.Fatalf("run %s (finished %v), attempt %s; want both cancelled and the run finished", runStatus, finished.Valid, stepStatus)
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

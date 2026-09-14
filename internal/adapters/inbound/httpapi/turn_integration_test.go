//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// §3.3 ("turn recovery", §8.7 "relaunch-and-resume"): integration tests
// for POST /api/sessions/{sessionID}/turns (turn.go's own CreateTurn),
// mirroring this package's own established house style (newTestRig,
// createAuthenticatedUser, doJSON) for the happy path/409/404 cases, and a
// bespoke, self-contained rig (mirroring internal/app/sessionactor's own
// dispatch_integration_test.go/resilience_killpod_integration_test.go
// precedent of building exactly what one test needs rather than forcing
// it through a shared fixture) for the one test that needs a real
// SandboxCommander wired in: proving the SAME OpenCode conversation id
// carries onto the resulting dispatch.

// fakeTurnCommander is a test-only ports.SandboxCommander recording every
// SendCommand call -- this file's own small local test double (each
// package here builds its own, rather than sharing one across package
// boundaries -- see httpapi_integration_test.go's own newTestPool doc
// comment for the identical precedent applied to pool construction).
type fakeTurnCommander struct {
	mu       sync.Mutex
	sessions []string
	payloads []json.RawMessage
}

var _ ports.SandboxCommander = (*fakeTurnCommander)(nil)

func (f *fakeTurnCommander) SendCommand(sessionID string, payload json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, sessionID)
	f.payloads = append(f.payloads, payload)
	return nil
}

func (f *fakeTurnCommander) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.payloads)
}

func (f *fakeTurnCommander) lastPayload() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payloads[len(f.payloads)-1]
}

// TestCreateTurn_HappyPath proves: an existing session with no in-flight
// turn -> 201, a new Pending turn row, and EnsureDispatched genuinely
// fires (the shared rig's registry has a nil provider/commander, so
// dispatch itself safely no-ops per those ports' own nil guards --
// internal/app/sessionactor/dispatch_integration_test.go already covers
// the full spawn/dispatch decision tree exhaustively; this test's own job
// is proving CreateTurn's own HTTP-level contract).
func TestCreateTurn_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)

	body := []byte(`{"prompt": "do the thing", "modelId": null,"effort":null, "planMode": false}`)
	var got restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if got.Id == "" {
		t.Error("Id is empty")
	}
	if got.Status != restdtos.CreateTurnResponseStatusPending {
		t.Errorf("Status = %q, want %q", got.Status, restdtos.CreateTurnResponseStatusPending)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1", len(turns))
	}
	if turns[0].Status != sqlcgen.TurnStatusPending {
		t.Errorf("turn status = %s, want %s", turns[0].Status, sqlcgen.TurnStatusPending)
	}
	// HasPrefix, not exact equality (FIX D follow-up fix, this batch): this
	// rig's own default objCfg is configured, so createTurnLocked also
	// appends the upload-tool note (gated on StorageConfigured, turn.go's
	// own doc comment) after the caller's own prompt text -- this generic
	// happy-path test only cares that the ORIGINAL prompt text survives
	// verbatim at the start, not that nothing else was ever appended
	// (upload_integration_test.go's own TestCreateTurn_NoAttachments_*
	// tests are where that exact-byte-count invariant is actually pinned
	// down, for both the storage-configured and not-configured cases).
	if turns[0].Prompt == nil || !strings.HasPrefix(*turns[0].Prompt, "do the thing") {
		t.Errorf("turn prompt = %v, want it to start with %q", turns[0].Prompt, "do the thing")
	}
}

// TestCreateTurn_EffortReachesTurnsEffort is H1's own regression guard
// (adversarial review, §29.8): every OTHER test in this file mechanically
// sends "effort":null, so createTurnLocked's own `Effort: effectiveEffort`
// write (turn.go) -- the actual turns.effort column write path -- was
// entirely unasserted; a regression silently dropping it (e.g. reverting to
// an unconditional Effort: nil) would have passed every existing test here.
// This POSTs a genuinely non-null effort and reads it back from the
// database, mirroring TestCreateTurn_HappyPath's own rig/assertion style.
func TestCreateTurn_EffortReachesTurnsEffort(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)

	body := []byte(`{"prompt": "think hard about this", "modelId": null, "effort": "high", "planMode": false}`)
	var got restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1", len(turns))
	}
	if turns[0].Effort == nil || *turns[0].Effort != "high" {
		gotEffort := "<nil>"
		if turns[0].Effort != nil {
			gotEffort = *turns[0].Effort
		}
		t.Errorf("turn effort = %s, want %q", gotEffort, "high")
	}
}

// TestCreateTurn_InFlightTurnExists_Returns409 proves the "exactly one
// processing per session" application-level check: a session with an
// already-Processing turn rejects a new CreateTurn with 409, and does NOT
// insert a second turn row at all.
func TestCreateTurn_InFlightTurnExists_Returns409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)

	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID,
		Status:    sqlcgen.TurnStatusProcessing,
	}); err != nil {
		t.Fatalf("seed in-flight turn: %v", err)
	}

	body := []byte(`{"prompt": "relaunch and resume", "modelId": null,"effort":null, "planMode": false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", body, nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (the 409 must not have inserted a second row)", len(turns))
	}
}

// TestCreateTurn_DispatchedTurnExists_Returns409 proves the SAME 409 gate
// covers Dispatched, not just Processing -- turn.HasInFlightTurn's own
// definition of "in flight" (internal/domain/turn/queue.go).
func TestCreateTurn_DispatchedTurnExists_Returns409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)

	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID,
		Status:    sqlcgen.TurnStatusDispatched,
	}); err != nil {
		t.Fatalf("seed dispatched turn: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns",
		[]byte(`{"prompt": "again", "modelId": null,"effort":null, "planMode": false}`), nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
}

// TestCreateTurn_ConcurrentRequests_OnlyOneSucceeds proves CreateTurn's own
// check-then-act sequence (list turns, check in-flight, insert) is genuinely
// serialized by SessionStore.GetActorEpochForUpdate's row lock, not merely
// checked-then-raced: firing N concurrent POSTs at a session with no
// existing turns must produce exactly one 201 and the rest 409s, and the
// turns table must end up with exactly one row -- never N Pending rows
// silently queued past the check this handler exists to enforce.
func TestCreateTurn_ConcurrentRequests_OnlyOneSucceeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)

	// Deliberately NOT using rig.doJSON here: it calls t.Fatalf internally
	// on transport errors, which the testing package requires be called
	// only from the goroutine running the test, not from goroutines this
	// test spawns. Build and fire each request directly instead, carrying
	// any error back through the results slice for the main goroutine to
	// report.
	const n = 8
	statuses := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/api/sessions/"+session.ID.String()+"/turns",
				bytes.NewReader([]byte(`{"prompt": "relaunch and resume", "modelId": null,"effort":null, "planMode": false}`)))
			if err != nil {
				errs[i] = fmt.Errorf("build request: %w", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs[i] = fmt.Errorf("do request: %w", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	created, conflicted := 0, 0
	for i, s := range statuses {
		if errs[i] != nil {
			t.Errorf("request %d: %v", i, errs[i])
			continue
		}
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicted++
		default:
			t.Errorf("unexpected status %d among concurrent responses", s)
		}
	}
	if created != 1 {
		t.Errorf("created = %d, want exactly 1", created)
	}
	if conflicted != n-1 {
		t.Errorf("conflicted = %d, want exactly %d", conflicted, n-1)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (no duplicate rows slipped past the in-flight check)", len(turns))
	}
}

// TestCreateTurn_AwaitingPlan_Returns409NothingCreated is this batch's own
// REST-level regression test for the new awaiting-plan gate (§8.1
// follow-up fix, §8.1): a plan_mode=false relaunch POST against a session
// that currently has a plan in StatusAwaitingApproval gets a 409 -- the
// SAME CreateTurnError shape TestCreateTurn_InFlightTurnExists_Returns409
// above already proves, just a different Message -- and inserts NO new
// turn row at all.
func TestCreateTurn_AwaitingPlan_Returns409NothingCreated(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)
	seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns",
		[]byte(`{"prompt": "build it now", "modelId": null,"effort":null, "planMode": false}`), nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}

	// The only turn present must be the seeded producing turn -- the gate
	// must never let an ordinary relaunch turn slip past it.
	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (the seeded producing turn only)", len(turns))
	}
}

// TestCreateTurn_AwaitingPlan_PlanModeTrue_Allowed proves the gate's own
// other half at the REST level: a web client submitting planMode=true
// directly (the request-changes flow) is never blocked by an
// awaiting-approval plan, exactly as before this fix.
func TestCreateTurn_AwaitingPlan_PlanModeTrue_Allowed(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, user.ID, nil)
	seedAwaitingApprovalPlan(ctx, t, rig, session.ID, 1)

	body := []byte(`{"prompt": "please also cover the edge case", "modelId": null,"effort":null, "planMode": true}`)
	var got restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (the seeded producing turn plus the new plan_mode=true one)", len(turns))
	}
}

// TestCreateTurn_UnknownSession_Returns404 proves a well-formed but
// nonexistent session id gets 404, matching GetSession's own precedent.
func TestCreateTurn_UnknownSession_Returns404(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	unknownID := "00000000-0000-0000-0000-000000000000"
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+unknownID+"/turns",
		[]byte(`{"prompt": "do the thing", "modelId": null,"effort":null, "planMode": false}`), nil, token)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestCreateTurn_MalformedID_Returns400 proves a malformed path segment
// gets 400, mirroring GetSession/parseSessionID's own precedent.
func TestCreateTurn_MalformedID_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/not-a-uuid/turns",
		[]byte(`{"prompt": "do the thing", "modelId": null,"effort":null, "planMode": false}`), nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestCreateTurn_CarriesExistingConversationID proves the reason CreateTurn
// needs no bespoke "resume" logic of its own (this handler's own doc
// comment): a session whose sessions.opencode_conversation_id is already
// non-null when a NEW turn is created via this endpoint dispatches that
// turn with the SAME conversation id, not a fresh one -- i.e. it genuinely
// continues the conversation. Builds its OWN standalone rig (rather than
// the shared newTestRig, whose registry has a nil commander) with a real
// fakeTurnCommander wired in, and a live Ready sandbox to dispatch to.
func TestCreateTurn_CarriesExistingConversationID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	sandboxes := narvipg.NewSandboxStore(pool)
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)
	plans := narvipg.NewPlanStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	commander := &fakeTurnCommander{}
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	router := chi.NewRouter()
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Post("/{sessionID}/turns", httpapi.CreateTurn(pool, sessions, turns, plans, participants, auditLog, registry, nil, nil, false))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	// A real authenticated user (mirrors testRig.createAuthenticatedUser's
	// own identical construction -- duplicated here since this test builds
	// its own standalone rig rather than the shared one).
	externalID := fmt.Sprintf("test-github-id-%d", time.Now().UnixNano())
	email := externalID + "@example.com"
	user, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: email, DisplayName: "Test User", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID: user.ID, TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create test user session: %v", err)
	}

	// CreatedBy is set to this test's own authenticated user -- §13.3's
	// "own/joined" gate (domain/authz.ActionPromptSession) requires it;
	// an unowned session would now get 403, not 201, from the CreateTurn
	// call below.
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	// sessions.opencode_conversation_id is ALREADY non-null going in --
	// exactly the precondition this test proves is honored.
	existingConversationID := "conv-already-in-progress"
	if _, err := sessions.UpdateConversationID(ctx, sqlcgen.UpdateSessionConversationIDParams{
		ID: session.ID, OpencodeConversationID: &existingConversationID,
	}); err != nil {
		t.Fatalf("seed existing conversation id: %v", err)
	}

	if _, err := sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: session.ID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	body := []byte(`{"prompt": "continue where we left off", "modelId": null,"effort":null, "planMode": false}`)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/"+session.ID.String()+"/turns", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && commander.callCount() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if commander.callCount() != 1 {
		t.Fatalf("commander.callCount() = %d, want 1 (the new turn must have dispatched)", commander.callCount())
	}

	var prompt sandboxws.Prompt
	if err := json.Unmarshal(commander.lastPayload(), &prompt); err != nil {
		t.Fatalf("unmarshal dispatched payload as sandboxws.Prompt: %v", err)
	}
	if prompt.ConversationId == nil || *prompt.ConversationId != existingConversationID {
		t.Errorf("dispatched Prompt.ConversationId = %v, want %q (the SAME, already-existing conversation, not a fresh one)",
			prompt.ConversationId, existingConversationID)
	}
	if prompt.Text != "continue where we left off" {
		t.Errorf("dispatched Prompt.Text = %q, want %q", prompt.Text, "continue where we left off")
	}
}

// --- §20 ("domain/turn: builder epistemic pre-action check", §20) --
// end-to-end coverage for the devil's-advocate preamble's own injection
// into a dispatched turn's REAL Prompt.Text, proven the SAME way
// TestCreateTurn_CarriesExistingConversationID above proves conversation-id
// threading: via the actual JSON payload the fake commander captured, not
// a re-derived approximation of what createTurnLocked is supposed to do.

// epistemicCheckTestRig bundles what every TestCreateTurn_EpistemicCheck*
// test below needs -- factored out of TestCreateTurn_CarriesExistingConversationID's
// own standalone rig above (this file's own top doc comment: "building
// exactly what one test needs rather than forcing it through a shared
// fixture") since three tests below need the IDENTICAL setup, varying only
// epistemicCheckDefault (CreateTurn's own new §20 constructor
// parameter) and the request body's own planMode.
type epistemicCheckTestRig struct {
	commander *fakeTurnCommander
	server    *httptest.Server
	sessionID pgtype.UUID
	token     string
	// pool (test-wiring bundle, adversarial review) is exposed so
	// TestCreateTurnCore_EpistemicCheckSessionOverride_* below can force
	// sessions.epistemic_check_enabled directly via SQL -- every one of
	// this rig's three ORIGINAL callers leaves the column NULL (the
	// zero-value CreateSessionParams below never sets it), so the session-
	// override half of §20.4's own precedence ("session override wins when
	// set, global default otherwise") was never actually exercised above
	// the pure function it's implemented in
	// (internal/domain/turn.ResolveEpistemicCheckEnabled, already covered
	// directly by that package's own unit test) -- only the platform-
	// default half was.
	pool *pgxpool.Pool
}

func newEpistemicCheckTestRig(t *testing.T, epistemicCheckDefault bool) epistemicCheckTestRig {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	sandboxes := narvipg.NewSandboxStore(pool)
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)
	plans := narvipg.NewPlanStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)

	commander := &fakeTurnCommander{}
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	router := chi.NewRouter()
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Post("/{sessionID}/turns", httpapi.CreateTurn(pool, sessions, turns, plans, participants, auditLog, registry, nil, nil, epistemicCheckDefault))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	externalID := fmt.Sprintf("test-github-id-%d", time.Now().UnixNano())
	email := externalID + "@example.com"
	user, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: email, DisplayName: "Test User", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create test identity: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID: user.ID, TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create test user session: %v", err)
	}

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	if _, err := sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: session.ID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	return epistemicCheckTestRig{commander: commander, server: server, sessionID: session.ID, token: token, pool: pool}
}

// dispatchAndCapturePrompt POSTs body to rig's own turns endpoint and
// returns the dispatched sandboxws.Prompt.Text the fake commander
// captured -- mirrors TestCreateTurn_CarriesExistingConversationID's own
// request/wait/unmarshal sequence exactly.
func dispatchAndCapturePrompt(t *testing.T, rig epistemicCheckTestRig, body []byte) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/api/sessions/"+rig.sessionID.String()+"/turns", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: rig.token})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && rig.commander.callCount() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if rig.commander.callCount() != 1 {
		t.Fatalf("commander.callCount() = %d, want 1 (the new turn must have dispatched)", rig.commander.callCount())
	}

	var prompt sandboxws.Prompt
	if err := json.Unmarshal(rig.commander.lastPayload(), &prompt); err != nil {
		t.Fatalf("unmarshal dispatched payload as sandboxws.Prompt: %v", err)
	}
	return prompt.Text
}

// TestCreateTurnCore_EpistemicCheckOff_ByteForByteNoOp pins §20.4's "off by
// default" requirement at the wire level: with epistemicCheckDefault=false
// (CreateTurn's own default -- exactly what cmd/control-plane/main.go
// wires when NARVI_EPISTEMIC_CHECK_DEFAULT is unset, platform/config.go)
// and no session-level override, the dispatched Prompt.Text is BYTE-FOR-
// BYTE identical to the prompt this request would have produced before
// §20 existed -- no preamble text, no placeholder tokens, nothing
// prepended at all. This is the required "assembled prompt is a
// byte-for-byte no-op versus today" proof CLAUDE.md's own prompt-byte-
// stability discipline demands.
func TestCreateTurnCore_EpistemicCheckOff_ByteForByteNoOp(t *testing.T) {
	rig := newEpistemicCheckTestRig(t, false)

	body := []byte(`{"prompt": "implement the feature", "modelId": null, "effort": null, "planMode": false}`)
	gotText := dispatchAndCapturePrompt(t, rig, body)

	if gotText != "implement the feature" {
		t.Fatalf("dispatched Prompt.Text = %q, want %q (byte-for-byte no-op, feature off by default)", gotText, "implement the feature")
	}
}

// TestCreateTurnCore_EpistemicCheckOn_BuildTurn_InjectsPreamble proves the
// positive case: with the platform default on and a non-plan-mode turn,
// the dispatched Prompt.Text is PRECEDED by turn.RenderEpistemicPreamble's
// own exact text (§20.1: "the turn prompt is preceded by"), with the
// caller's own original prompt text still present, verbatim, at the end.
func TestCreateTurnCore_EpistemicCheckOn_BuildTurn_InjectsPreamble(t *testing.T) {
	rig := newEpistemicCheckTestRig(t, true)

	body := []byte(`{"prompt": "implement the feature", "modelId": null, "effort": null, "planMode": false}`)
	gotText := dispatchAndCapturePrompt(t, rig, body)

	wantPrefix := turn.RenderEpistemicPreamble()
	if !strings.HasPrefix(gotText, wantPrefix) {
		t.Fatalf("dispatched Prompt.Text does not start with RenderEpistemicPreamble()'s own text\ngot:  %q\nwant prefix: %q", gotText, wantPrefix)
	}
	if !strings.HasSuffix(gotText, "implement the feature") {
		t.Fatalf("dispatched Prompt.Text does not end with the caller's own original prompt text, verbatim\ngot: %q", gotText)
	}
}

// TestCreateTurnCore_EpistemicCheckOn_PlanModeTurn_NoPreamble pins §20.3's
// exclusion at the SAME wire-level fidelity as the two tests above: even
// with the platform default on, a plan_mode=true turn's dispatched
// Prompt.Text carries NO epistemic preamble text at all. It is NOT
// byte-for-byte identical to the caller's own raw prompt any more --
// §12.2 item 3's own structured-plan-document instruction
// (plandomain.MaybeInjectStructureInstruction) is unconditionally
// prepended onto every plan_mode=true turn regardless of the epistemic
// check, an unrelated mechanism this test does not exist to prove or
// disprove -- so the assertion checks for that EXACT, expected prefix
// (never a bare inequality that a later, unrelated prompt-shaping addition
// could vacuously "fix" back to green) and, separately and explicitly,
// that the epistemic preamble specifically is absent.
func TestCreateTurnCore_EpistemicCheckOn_PlanModeTurn_NoPreamble(t *testing.T) {
	rig := newEpistemicCheckTestRig(t, true)

	body := []byte(`{"prompt": "make a plan for this", "modelId": null, "effort": null, "planMode": true}`)
	gotText := dispatchAndCapturePrompt(t, rig, body)

	wantText := plandomain.MaybeInjectStructureInstruction(true, "make a plan for this")
	if gotText != wantText {
		t.Fatalf("dispatched Prompt.Text = %q, want %q (only the structured-plan-document instruction, never the epistemic preamble, §20.3, even with the check enabled)", gotText, wantText)
	}
	if strings.Contains(gotText, turn.RenderEpistemicPreamble()) {
		t.Fatalf("dispatched Prompt.Text contains the epistemic preamble -- plan-mode turns must never get it (§20.3), even with the check enabled: %q", gotText)
	}
}

// setEpistemicCheckOverride forces sessions.epistemic_check_enabled to
// override directly via SQL -- the ONE thing rig's own CreateSessionParams
// (newEpistemicCheckTestRig) never sets, exactly like every other
// integration test in this package that forces a column no production
// insert path in THIS test's own scenario would otherwise reach (e.g.
// epistemicoutcome_integration_test.go's own `UPDATE turns SET status =
// ...` calls).
func setEpistemicCheckOverride(ctx context.Context, t *testing.T, rig epistemicCheckTestRig, override bool) {
	t.Helper()
	if _, err := rig.pool.Exec(ctx, `UPDATE sessions SET epistemic_check_enabled = $1 WHERE id = $2`, override, rig.sessionID); err != nil {
		t.Fatalf("force session epistemic_check_enabled override: %v", err)
	}
}

// TestCreateTurnCore_EpistemicCheckSessionOverride_FalseBeatsGlobalTrue is
// the test-wiring bundle's own addition (adversarial review): proves §20.4's
// "session override wins when set, global default otherwise" precedence at
// the SAME wire-level fidelity as the three tests above, with the session's
// own override column actually SET (every other test in this file leaves it
// NULL) -- before this test existed, createTurnLocked's own consultation of
// sessionRow.EpistemicCheckEnabled (turn.go) was exercised only by internal/
// domain/turn.ResolveEpistemicCheckEnabled's own pure-function unit test,
// never above it -- so the session-override wiring itself (SessionStore.Get
// actually returning the column, createTurnLocked actually reading it) could
// have been silently replaced by "always use the platform default" and
// nothing here would have caught it.
func TestCreateTurnCore_EpistemicCheckSessionOverride_FalseBeatsGlobalTrue(t *testing.T) {
	rig := newEpistemicCheckTestRig(t, true) // platform default: ON
	setEpistemicCheckOverride(context.Background(), t, rig, false)

	body := []byte(`{"prompt": "implement the feature", "modelId": null, "effort": null, "planMode": false}`)
	gotText := dispatchAndCapturePrompt(t, rig, body)

	if gotText != "implement the feature" {
		t.Fatalf("dispatched Prompt.Text = %q, want %q (session override=false must beat platform default=true)", gotText, "implement the feature")
	}
}

// TestCreateTurnCore_EpistemicCheckSessionOverride_TrueBeatsGlobalFalse is
// TestCreateTurnCore_EpistemicCheckSessionOverride_FalseBeatsGlobalTrue's
// own mirror, the other direction: session override=true must beat a
// platform default of false (CreateTurn's own real-world default,
// §20.4's own "off by default" -- this is the direction that actually
// lets an operator's per-session opt-in take effect at all).
func TestCreateTurnCore_EpistemicCheckSessionOverride_TrueBeatsGlobalFalse(t *testing.T) {
	rig := newEpistemicCheckTestRig(t, false) // platform default: OFF
	setEpistemicCheckOverride(context.Background(), t, rig, true)

	body := []byte(`{"prompt": "implement the feature", "modelId": null, "effort": null, "planMode": false}`)
	gotText := dispatchAndCapturePrompt(t, rig, body)

	wantPrefix := turn.RenderEpistemicPreamble()
	if !strings.HasPrefix(gotText, wantPrefix) {
		t.Fatalf("dispatched Prompt.Text does not start with RenderEpistemicPreamble()'s own text (session override=true must beat platform default=false)\ngot:  %q\nwant prefix: %q", gotText, wantPrefix)
	}
	if !strings.HasSuffix(gotText, "implement the feature") {
		t.Fatalf("dispatched Prompt.Text does not end with the caller's own original prompt text, verbatim\ngot: %q", gotText)
	}
}

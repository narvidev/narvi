//go:build integration

// Integration tests for CreateTurnCore's own CreateTurnPolicy parameter
// (turn.go) -- the audit-fix batch's own consolidation of REST/Slack/
// Linear/GitHub-bot turn creation into one shared core. Lives in package
// httpapi (not httpapi_test), mirroring bot_integration_test.go's own
// precedent exactly, since createTurnLocked itself is unexported. Builds
// its own minimal testcontainers-Postgres rig (newBotTestPool,
// bot_integration_test.go) rather than a fresh one, per this file set's
// own established "one small helper, several files" convention within
// this package.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// turnCoreTestRig bundles the stores/registry every test below needs --
// all built against the SAME testcontainers pool (newBotTestPool).
type turnCoreTestRig struct {
	pool     *pgxpool.Pool
	sessions *narvipg.SessionStore
	turns    *narvipg.TurnStore
	plans    *narvipg.PlanStore
	auditLog *narvipg.AuditLogStore
	registry *sessionactor.Registry
}

func newTurnCoreTestRig(t *testing.T) *turnCoreTestRig {
	t.Helper()
	ctx := context.Background()
	pool := newBotTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	return &turnCoreTestRig{
		pool:     pool,
		sessions: narvipg.NewSessionStore(pool),
		turns:    narvipg.NewTurnStore(pool),
		plans:    narvipg.NewPlanStore(pool),
		auditLog: narvipg.NewAuditLogStore(pool),
		registry: registry,
	}
}

// newFixtureSession creates a bare session with no turns.
func (r *turnCoreTestRig) newFixtureSession(t *testing.T, ctx context.Context) sqlcgen.Session {
	t.Helper()
	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create fixture session: %v", err)
	}
	return session
}

// newFixtureUser creates a real users row -- needed whenever a test wants
// a Valid actorUserID, since audit_log.actor_user_id is a real FK
// (migrations/000013_audit_log.up.sql).
func (r *turnCoreTestRig) newFixtureUser(t *testing.T, ctx context.Context) sqlcgen.User {
	t.Helper()
	user, err := narvipg.NewUserStore(r.pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("turncore-fixture-%d@example.com", time.Now().UnixNano()),
		DisplayName:  "Turn Core Fixture",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	return user
}

// auditRowCount counts audit_log rows for action='turn.create' whose
// resource_id matches turnID.
func (r *turnCoreTestRig) auditRowCount(t *testing.T, ctx context.Context, turnID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'turn.create' AND resource_type = 'turn' AND resource_id = $1`,
		turnID.String(),
	).Scan(&count); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	return count
}

// totalTurnCreateAuditRows counts EVERY turn.create audit_log row in the
// whole database -- used by the concurrency tests below, which care about
// "exactly one row total", not one specific (already-known) turn id.
func (r *turnCoreTestRig) totalTurnCreateAuditRows(t *testing.T, ctx context.Context) int {
	t.Helper()
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'turn.create'`).Scan(&count); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	return count
}

// seedAwaitingApprovalPlan seeds a producing turn (Completed, plan_mode
// true) and an awaiting_approval plans row atop it -- mirrors
// httpapi_test's own identical seedAwaitingApprovalPlan
// (planapprove_integration_test.go), duplicated here since this file lives
// in package httpapi (not httpapi_test, per this file's own top doc
// comment) and cannot reach that package's unexported helper.
func (r *turnCoreTestRig) seedAwaitingApprovalPlan(t *testing.T, ctx context.Context, sessionID pgtype.UUID) sqlcgen.Plan {
	t.Helper()
	producingTurn, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := r.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sessionID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}
	return plan
}

// TestCreateTurnCore_RejectIfOpen_Success_WritesAuditRowWithActor proves
// the success path for RejectIfOpen (REST's own policy): a real,
// resolved actorUserID ends up on the resulting turn.create audit_log row.
func TestCreateTurnCore_RejectIfOpen_Success_WritesAuditRowWithActor(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	actor := rig.newFixtureUser(t, ctx)

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "do the thing", nil, false, false, actor.ID, RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}

	if rig.auditRowCount(t, ctx, created.ID) != 1 {
		t.Fatalf("audit row count = %d, want 1", rig.auditRowCount(t, ctx, created.ID))
	}

	var actorUserID pgtype.UUID
	var detailRaw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT actor_user_id, detail_json FROM audit_log WHERE action = 'turn.create' AND resource_id = $1`, created.ID.String()).Scan(&actorUserID, &detailRaw); err != nil {
		t.Fatalf("query actor_user_id/detail_json: %v", err)
	}
	if actorUserID != actor.ID {
		t.Errorf("audit_log.actor_user_id = %v, want %v", actorUserID, actor.ID)
	}

	// Audit-fix batch addition (M9, completeness): this test's own
	// existence/actor assertions above predate this fix -- decode/assert
	// the detail_json shape createTurnLocked (turn.go) actually writes
	// (session_id/plan_mode), never checked before.
	var detail map[string]any
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("unmarshal detail_json: %v", err)
	}
	if detail["session_id"] != session.ID.String() {
		t.Errorf("detail_json[session_id] = %v, want %q", detail["session_id"], session.ID.String())
	}
	if detail["plan_mode"] != false {
		t.Errorf("detail_json[plan_mode] = %v, want false", detail["plan_mode"])
	}
}

// TestCreateTurnCore_RejectIfOpen_OpenTurn_ConflictsAndWritesNoAuditRow
// proves RejectIfOpen's own rejection path writes NO audit row at all --
// a rejected attempt must never leave a phantom audit trail for a turn
// that was never actually created.
func TestCreateTurnCore_RejectIfOpen_OpenTurn_ConflictsAndWritesNoAuditRow(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing}); err != nil {
		t.Fatalf("seed open turn: %v", err)
	}

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "again", nil, false, false, pgtype.UUID{}, RejectIfOpen)
	if cerr == nil {
		t.Fatal("cerr = nil, want a 409 CreateTurnError")
	}
	if cerr.Status != http.StatusConflict {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusConflict)
	}
	if wasCreated {
		t.Error("wasCreated = true, want false")
	}
	if got := rig.totalTurnCreateAuditRows(t, ctx); got != 0 {
		t.Errorf("total turn.create audit rows = %d, want 0", got)
	}
}

// TestCreateTurnCore_DropIfOpen_OpenTurn_ReturnsFalseNoErrorNoAuditRow
// proves DropIfOpen's own silent-decline path -- Slack's/Linear's shared
// policy: no error, no turn, no audit row.
func TestCreateTurnCore_DropIfOpen_OpenTurn_ReturnsFalseNoErrorNoAuditRow(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusDispatched}); err != nil {
		t.Fatalf("seed open turn: %v", err)
	}

	_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "reply", nil, false, false, pgtype.UUID{}, DropIfOpen)
	if cerr != nil {
		t.Fatalf("cerr = %+v, want nil (DropIfOpen never errors on an open turn)", cerr)
	}
	if wasCreated {
		t.Error("wasCreated = true, want false")
	}
	if got := rig.totalTurnCreateAuditRows(t, ctx); got != 0 {
		t.Errorf("total turn.create audit rows = %d, want 0", got)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (only the seeded open turn -- DropIfOpen must never insert a second)", len(turns))
	}
}

// TestCreateTurnCore_AlwaysQueue_SkipsOpenTurnCheck_WritesAuditRowPerCall
// proves AlwaysQueue's own policy -- CreateTurnForBot's fixed choice:
// unconditionally enqueues even while a turn is already open, and writes
// its OWN audit row for each call.
func TestCreateTurnCore_AlwaysQueue_SkipsOpenTurnCheck_WritesAuditRowPerCall(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing}); err != nil {
		t.Fatalf("seed open turn: %v", err)
	}

	first, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "first mention", nil, false, false, pgtype.UUID{}, AlwaysQueue)
	if cerr != nil {
		t.Fatalf("CreateTurnCore (first): status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated (first) = false, want true")
	}

	second, wasCreated2, cerr2 := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "second mention", nil, false, false, pgtype.UUID{}, AlwaysQueue)
	if cerr2 != nil {
		t.Fatalf("CreateTurnCore (second): status=%d message=%q", cerr2.Status, cerr2.Message)
	}
	if !wasCreated2 {
		t.Fatal("wasCreated (second) = false, want true")
	}
	if second.ID == first.ID {
		t.Error("second turn has the same id as the first, want a distinct new turn")
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("len(turns) = %d, want 3 (the seeded open turn plus both AlwaysQueue inserts)", len(turns))
	}

	if got := rig.auditRowCount(t, ctx, first.ID); got != 1 {
		t.Errorf("audit row count for first turn = %d, want 1", got)
	}
	if got := rig.auditRowCount(t, ctx, second.ID); got != 1 {
		t.Errorf("audit row count for second turn = %d, want 1", got)
	}
	// The seeded turn never went through CreateTurnCore at all -- it must
	// have no audit row of its own.
	if got := rig.totalTurnCreateAuditRows(t, ctx); got != 2 {
		t.Errorf("total turn.create audit rows = %d, want exactly 2 (one per AlwaysQueue call, none for the directly-seeded turn)", got)
	}
}

// TestCreateTurnCore_RejectIfOpen_ConcurrentRequests_OnlyOneSucceeds is
// the concurrency proof for RejectIfOpen at the Go-function level (not
// just HTTP, mirroring bot_integration_test.go's own house style, and
// bot_integration_test.go/turn_integration_test.go's own precedent for
// concurrency tests generally): firing N concurrent CreateTurnCore calls
// against a session with zero existing turns must produce exactly one
// success and, critically, exactly ONE turn.create audit row -- proving
// the SAME row lock that serializes the insert also serializes the audit
// write inside the identical transaction, not a separate race of its own.
func TestCreateTurnCore_RejectIfOpen_ConcurrentRequests_OnlyOneSucceeds(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	const n = 8
	type result struct {
		wasCreated bool
		cerr       *CreateTurnError
	}
	results := make(chan result, n)
	var eg errgroup.Group
	for i := 0; i < n; i++ {
		i := i
		eg.Go(func() error {
			_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, fmt.Sprintf("relaunch %d", i), nil, false, false, pgtype.UUID{}, RejectIfOpen)
			results <- result{wasCreated: wasCreated, cerr: cerr}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(results)

	var succeeded, conflicted int
	for r := range results {
		switch {
		case r.cerr == nil && r.wasCreated:
			succeeded++
		case r.cerr != nil && r.cerr.Status == http.StatusConflict:
			conflicted++
		default:
			t.Errorf("unexpected result wasCreated=%v cerr=%+v", r.wasCreated, r.cerr)
		}
	}

	if succeeded != 1 {
		t.Errorf("succeeded = %d, want exactly 1", succeeded)
	}
	if conflicted != n-1 {
		t.Errorf("conflicted = %d, want exactly %d", conflicted, n-1)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1 (no duplicate rows slipped past the lock)", len(turns))
	}
	if got := rig.totalTurnCreateAuditRows(t, ctx); got != 1 {
		t.Errorf("total turn.create audit rows = %d, want exactly 1", got)
	}
}

// TestCreateTurnCore_DropIfOpen_ConcurrentRequests_OnlyOneSucceeds is the
// identical concurrency proof for DropIfOpen (Slack's/Linear's own
// policy) -- this is what closes L2 for Linear specifically: the session
// row lock genuinely serializes N concurrent "add a reply turn" attempts,
// not just N sequential ones, so exactly one wins and the rest silently
// (per DropIfOpen's own contract) decline, never double-inserting.
func TestCreateTurnCore_DropIfOpen_ConcurrentRequests_OnlyOneSucceeds(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	const n = 8
	type result struct {
		wasCreated bool
		cerr       *CreateTurnError
	}
	results := make(chan result, n)
	var eg errgroup.Group
	for i := 0; i < n; i++ {
		i := i
		eg.Go(func() error {
			_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, fmt.Sprintf("reply %d", i), nil, false, false, pgtype.UUID{}, DropIfOpen)
			results <- result{wasCreated: wasCreated, cerr: cerr}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(results)

	var succeeded, dropped int
	for r := range results {
		if r.cerr != nil {
			t.Errorf("cerr = %+v, want nil", r.cerr)
			continue
		}
		if r.wasCreated {
			succeeded++
		} else {
			dropped++
		}
	}

	if succeeded != 1 {
		t.Errorf("succeeded = %d, want exactly 1", succeeded)
	}
	if dropped != n-1 {
		t.Errorf("dropped = %d, want exactly %d", dropped, n-1)
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want exactly 1", len(turns))
	}
	if got := rig.totalTurnCreateAuditRows(t, ctx); got != 1 {
		t.Errorf("total turn.create audit rows = %d, want exactly 1", got)
	}
}

// TestCreateTurnCore_AlwaysQueue_ConcurrentRequests_AllSucceed proves the
// lock is genuinely held for AlwaysQueue too, even though its own
// open-turn check is skipped entirely: N concurrent calls against a
// session with zero existing turns must all succeed with N DISTINCT turn
// rows and N distinct audit rows -- never a lost write, a duplicate id, or
// a corrupted insert from two goroutines racing the same session row.
func TestCreateTurnCore_AlwaysQueue_ConcurrentRequests_AllSucceed(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	const n = 8
	ids := make([]pgtype.UUID, n)
	var eg errgroup.Group
	for i := 0; i < n; i++ {
		i := i
		eg.Go(func() error {
			created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, fmt.Sprintf("mention %d", i), nil, false, false, pgtype.UUID{}, AlwaysQueue)
			if cerr != nil {
				t.Errorf("goroutine %d: cerr = %+v, want nil", i, cerr)
				return nil
			}
			if !wasCreated {
				t.Errorf("goroutine %d: wasCreated = false, want true (AlwaysQueue never declines)", i)
				return nil
			}
			ids[i] = created.ID
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}

	seen := make(map[string]bool, n)
	for i, id := range ids {
		if !id.Valid {
			t.Errorf("ids[%d] is invalid, want a real turn id", i)
			continue
		}
		if seen[id.String()] {
			t.Errorf("duplicate turn id %s across concurrent AlwaysQueue calls", id.String())
		}
		seen[id.String()] = true
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != n {
		t.Fatalf("len(turns) = %d, want exactly %d", len(turns), n)
	}
	if got := rig.totalTurnCreateAuditRows(t, ctx); got != n {
		t.Errorf("total turn.create audit rows = %d, want exactly %d", got, n)
	}
}

// TestCreateTurnCore_AwaitingPlan_OrdinaryTurn_Gated is this batch's own
// flagship regression test for the new awaiting-plan gate (§8.1
// follow-up fix, §8.1): an ordinary (planMode == false) turn creation
// attempt against a session that currently has a plan in
// StatusAwaitingApproval must be declined -- via httpapi.
// ErrPlanAwaitingApproval, recognizable through errors.Is -- with ZERO side
// effects (no turn row, no turn.create audit row), regardless of which
// CreateTurnPolicy the caller uses. Since Finding 3's own follow-up fix
// (see TestCreateTurnCore_OpenTurnDuringAwaitingApproval_BusyWins below),
// the core's own gate runs AFTER the policy-gated open-turn check, not
// before -- this test still passes because none of its subtests seed an
// open turn, so the open-turn check never short-circuits before the
// awaiting-plan gate runs; for AlwaysQueue specifically, the open-turn
// check is skipped entirely (it never runs regardless of an open turn),
// so this gate is reached and enforced exactly the same way for every
// policy either way. Each caller (REST/Slack/Linear/GitHub-bot) maps this
// one shared outcome onto its own transport-appropriate response (REST's
// own 409 body, Slack's/Linear's honest reply) -- proved separately at
// each of those call sites.
func TestCreateTurnCore_AwaitingPlan_OrdinaryTurn_Gated(t *testing.T) {
	for _, policy := range []CreateTurnPolicy{RejectIfOpen, DropIfOpen, AlwaysQueue} {
		t.Run(fmt.Sprintf("policy=%d", policy), func(t *testing.T) {
			ctx := context.Background()
			rig := newTurnCoreTestRig(t)
			session := rig.newFixtureSession(t, ctx)
			rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

			_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "build this now", nil, false, false, pgtype.UUID{}, policy)
			if cerr == nil {
				t.Fatal("cerr = nil, want a 409 CreateTurnError wrapping ErrPlanAwaitingApproval")
			}
			if cerr.Status != http.StatusConflict {
				t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusConflict)
			}
			if !errors.Is(cerr, ErrPlanAwaitingApproval) {
				t.Errorf("errors.Is(cerr, ErrPlanAwaitingApproval) = false, want true (cerr = %+v)", cerr)
			}
			if wasCreated {
				t.Error("wasCreated = true, want false")
			}

			// The ONLY turn present must be the seeded producing turn --
			// the gate must never let a second, ordinary turn slip past it.
			turns, err := rig.turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}
			if len(turns) != 1 {
				t.Fatalf("len(turns) = %d, want exactly 1 (the seeded producing turn only)", len(turns))
			}
			if got := rig.totalTurnCreateAuditRows(t, ctx); got != 0 {
				t.Errorf("total turn.create audit rows = %d, want 0 (a gated attempt must leave no audit trail)", got)
			}
		})
	}
}

// TestCreateTurnCore_AwaitingPlan_PlanModeTrue_Allowed proves the gate's own
// other half: a planMode == true turn (the request-changes flow, however it
// was reached -- Slack's "Request changes" modal, a revise:-prefixed chat
// reply, or a web client setting planMode directly) is NEVER blocked by an
// awaiting-approval plan, exactly as before this fix.
func TestCreateTurnCore_AwaitingPlan_PlanModeTrue_Allowed(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)
	rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "drop the retry", nil, true, false, pgtype.UUID{}, RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true (a plan_mode=true turn must never be gated)")
	}
	// §12.2 item 3's own structured-plan-document instruction is
	// unconditionally prepended onto every plan_mode=true turn's prompt
	// (plandomain.MaybeInjectStructureInstruction) -- this turn is exactly
	// such a turn, so its own stored prompt carries that prefix, never the
	// bare input string.
	wantPrompt := plandomain.MaybeInjectStructureInstruction(true, "drop the retry")
	if created.Prompt == nil || *created.Prompt != wantPrompt {
		t.Errorf("created.Prompt = %v, want %q", created.Prompt, wantPrompt)
	}
	if !created.PlanMode {
		t.Error("created.PlanMode = false, want true")
	}

	turns, err := rig.turns.ListForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (the seeded producing turn plus the new plan_mode=true one)", len(turns))
	}
}

// TestCreateTurnCore_NoAwaitingPlan_OrdinaryTurn_Unaffected is this fix's
// own explicit regression guard: with NO awaiting-approval plan for the
// session at all, an ordinary (planMode == false) turn creation behaves
// byte-for-byte as it did before this batch -- succeeds, unblocked. Every
// OTHER test in this file already proves this incidentally (none of them
// ever seed a plan row), but this test names the guarantee directly.
func TestCreateTurnCore_NoAwaitingPlan_OrdinaryTurn_Unaffected(t *testing.T) {
	ctx := context.Background()
	rig := newTurnCoreTestRig(t)
	session := rig.newFixtureSession(t, ctx)

	created, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "do the thing", nil, false, false, pgtype.UUID{}, RejectIfOpen)
	if cerr != nil {
		t.Fatalf("CreateTurnCore: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !wasCreated {
		t.Fatal("wasCreated = false, want true")
	}
	if created.Prompt == nil || *created.Prompt != "do the thing" {
		t.Errorf("created.Prompt = %v, want %q", created.Prompt, "do the thing")
	}
}

// TestCreateTurnCore_OpenTurnDuringAwaitingApproval_BusyWins is Finding 3's
// own regression test (a follow-up fix, gate-ordering audit
// finding): internal/app/sessionactor/planrecord.go's own
// recordPlanIfNeeded only supersedes the OLD awaiting_approval plan row at
// the END of a revise turn's (plan_mode=true) own processing, not at that
// turn's own creation time -- so for the ENTIRE duration an in-flight
// revise turn is open (Dispatched here, simulating "already picked up and
// being worked on"), BOTH "an open turn exists" AND "the old plan is
// still awaiting_approval" are simultaneously true. BEFORE this fix, the
// awaiting-plan gate ran FIRST and unconditionally, so an ordinary message
// arriving during that exact overlap window got the "plan is awaiting
// your approval" reply instead of the more accurate "still working on the
// previous message" busy reply -- misleading, since the user's own
// revision request is already being processed, not idly waiting on a
// decision. Proves the busy check now wins this overlap window for both
// RejectIfOpen (REST's own 409) and DropIfOpen (Slack's/Linear's own
// silent-drop-then-honest-reply convention) alike.
func TestCreateTurnCore_OpenTurnDuringAwaitingApproval_BusyWins(t *testing.T) {
	for _, policy := range []CreateTurnPolicy{RejectIfOpen, DropIfOpen} {
		t.Run(fmt.Sprintf("policy=%d", policy), func(t *testing.T) {
			ctx := context.Background()
			rig := newTurnCoreTestRig(t)
			session := rig.newFixtureSession(t, ctx)
			rig.seedAwaitingApprovalPlan(t, ctx, session.ID)

			// Seed an in-flight "request changes" turn -- open
			// (Dispatched), plan_mode=true -- simulating the exact overlap
			// window this fix closes: the plan row seeded above is STILL
			// awaiting_approval (recordPlanIfNeeded hasn't superseded it
			// yet, since THIS turn hasn't completed), while a turn is
			// simultaneously open/in-flight for this same session.
			if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusDispatched, PlanMode: true}); err != nil {
				t.Fatalf("seed in-flight revise turn: %v", err)
			}

			// An ordinary (plan_mode=false) message arrives during that
			// exact overlap window.
			_, wasCreated, cerr := CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry, session.ID, "any updates?", nil, false, false, pgtype.UUID{}, policy)

			if wasCreated {
				t.Error("wasCreated = true, want false (an open turn must still block a new one)")
			}

			switch policy {
			case RejectIfOpen:
				if cerr == nil {
					t.Fatal("cerr = nil, want a 409 CreateTurnError for the busy open-turn case")
				}
				if cerr.Status != http.StatusConflict {
					t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusConflict)
				}
				if cerr.Message != "a turn is already pending, dispatched, or processing for this session" {
					t.Errorf("cerr.Message = %q, want the busy message, not the awaiting-plan one", cerr.Message)
				}
				if errors.Is(cerr, ErrPlanAwaitingApproval) {
					t.Error("errors.Is(cerr, ErrPlanAwaitingApproval) = true, want false -- the busy reply must win this overlap window, not the awaiting-plan reply")
				}
			case DropIfOpen:
				if cerr != nil {
					t.Fatalf("cerr = %+v, want nil (DropIfOpen silently declines -- it must never surface the awaiting-plan 409 in this overlap window)", cerr)
				}
			}

			// The gate must never let a second, ordinary turn slip past it
			// -- only the seeded producing turn plus the seeded in-flight
			// revise turn.
			turnRows, err := rig.turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}
			if len(turnRows) != 2 {
				t.Fatalf("len(turns) = %d, want exactly 2 (the seeded producing turn + the seeded in-flight revise turn only)", len(turnRows))
			}
		})
	}
}

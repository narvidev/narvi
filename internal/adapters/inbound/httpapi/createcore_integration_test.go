//go:build integration

// -- §8.10, "Slack ingress" -- and split for tx support) --
// deliberately in package httpapi (not httpapi_test, unlike this
// package's other integration tests): even though CreateSessionCore/
// CreateSessionOnTx are exported today, they remain CreateSession's own
// internal implementation detail first and foremost, with the webhook
// ingress packages (§8.2/§8.10) as their only outside callers today.
// This file builds its own minimal rig rather than reusing httpapi_test's
// own newTestRig/newTestPool -- an external test package's unexported
// helpers are not reachable from an internal one, matching this
// codebase's own existing precedent that each DB-touching test file is
// free to set up what it needs directly (see e.g. sandbox_
// upsertforspawn_integration_test.go, which builds its own session
// directly via raw stores rather than a shared REST rig). Its pool now
// comes from sharedpool_integration_test.go's own IntegrationTestPool
// AndConnStr, not a container/migration run of its own -- see that
// file's own top doc comment for why.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newCoreTestPool now simply delegates to this whole binary's ONE shared
// pool (sharedpool_integration_test.go's own IntegrationTestPool) --
// kept under its own original name/signature so every call site in this
// file keeps compiling unchanged.
func newCoreTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := newCoreTestPoolAndConnStr(t)
	return pool
}

// newCoreTestPoolAndConnStr is newCoreTestPool plus the raw connection
// string, for the rare test (TestCreateSessionCore_ValidationFailure_
// NeverAcquiresConnection below) that needs to open its OWN
// differently-configured pool (a MaxConns:1 one) against the same
// database rather than reuse the shared-default pool every other test in
// this file gets. Delegates to sharedpool_integration_test.go's own
// IntegrationTestPoolAndConnStr.
func newCoreTestPoolAndConnStr(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	return IntegrationTestPoolAndConnStr(t)
}

// TestCreateSessionCore_NilCreator_StoresNullCreatedBy proves the
// extracted core function's own new capability: a NIL/invalid creator
// (pgtype.UUID{}, Valid == false -- exactly what a future webhook/bot
// ingress caller with no cookie-authenticated human passes) is accepted
// and stored as a genuine SQL NULL sessions.created_by, never rejected
// and never coerced into some fake placeholder id. This path is never
// exercised by httpapi_test's own HTTP-only tests, which all go through
// CreateSession's own hard-required authenticatedUserID.
func TestCreateSessionCore_NilCreator_StoresNullCreatedBy(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "narvi", Url: "https://github.com/narvidev/narvi"},
		},
	}

	var nilCreator pgtype.UUID // Valid == false: the explicit "no human caller" case.

	created, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionCore: status=%d message=%q", cerr.Status, cerr.Message)
	}

	if created.CreatedBy.Valid {
		t.Errorf("CreatedBy.Valid = true, want false (NULL) for a nil creator")
	}
	if created.SpawnSource != sqlcgen.SessionSpawnSourceGithub {
		t.Errorf("SpawnSource = %q, want %q", created.SpawnSource, sqlcgen.SessionSpawnSourceGithub)
	}

	// Confirm directly against Postgres too, not just the returned row --
	// proves the NULL genuinely round-tripped through the actual INSERT,
	// not merely reflected back from an in-memory struct.
	var createdByIsNull bool
	if err := pool.QueryRow(ctx,
		`SELECT created_by IS NULL FROM sessions WHERE id = $1`, created.ID,
	).Scan(&createdByIsNull); err != nil {
		t.Fatalf("query created_by: %v", err)
	}
	if !createdByIsNull {
		t.Error("sessions.created_by is NOT NULL in Postgres, want NULL for a nil creator")
	}
}

// TestCreateSessionCore_NilCreator_WithPromptDispatches proves a nil
// creator does not disturb the rest of CreateSessionCore's own existing
// behavior: a non-nil prompt still creates a pending turn AND still
// triggers the post-commit GetOrSpawn+EnsureDispatched path exactly like
// today's authenticated-human path already does (indirectly proven here
// by a successful, error-free call -- dispatch_integration_test.go in
// app/sessionactor is what exhaustively covers the decision tree itself;
// this test only proves CreateSessionCore's own wiring into it is
// unaffected by createdBy being NULL).
func TestCreateSessionCore_NilCreator_WithPromptDispatches(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	prompt := "fix the failing check"
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "narvi", Url: "https://github.com/narvidev/narvi"},
		},
	}

	var nilCreator pgtype.UUID

	created, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionCore: status=%d message=%q", cerr.Status, cerr.Message)
	}

	turnRows, err := turns.ListForSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turnRows) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (prompt was non-nil)", len(turnRows))
	}
	if turnRows[0].Prompt == nil || *turnRows[0].Prompt != prompt {
		t.Errorf("turn prompt = %v, want %q", turnRows[0].Prompt, prompt)
	}
}

// TestCreateSessionOnTx_CallerRollback_PersistsNothing proves
// CreateSessionOnTx never commits (or rolls back) the transaction it is
// handed -- it is entirely the CALLER's job. This test begins its own
// tx, calls CreateSessionOnTx successfully on it (both the session AND
// its turn insert succeed, since req.Prompt is non-nil), then explicitly
// rolls back that SAME outer tx itself and asserts NOTHING was actually
// persisted -- if CreateSessionOnTx had secretly committed anything
// internally, the row(s) would survive this rollback and the assertions
// below would fail.
func TestCreateSessionOnTx_CallerRollback_PersistsNothing(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	prompt := "fix the failing check"
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "narvi", Url: "https://github.com/narvidev/narvi"},
		},
	}

	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}

	created, hasPrompt, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		// The tx is still open at this point -- roll it back before
		// failing the test so we don't leak a connection.
		_ = tx.Rollback(ctx)
		t.Fatalf("CreateSessionOnTx: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !hasPrompt {
		t.Error("hasPrompt = false, want true (req.Prompt was non-nil)")
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid -- CreateSessionOnTx did not actually insert a session on tx")
	}

	// The caller (this test), not CreateSessionOnTx, decides the outer
	// transaction's fate -- roll it back deliberately.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("tx.Rollback: %v", err)
	}

	// Query through a completely separate connection (the shared pool,
	// outside the rolled-back tx) -- if CreateSessionOnTx had committed
	// anything on its own, it would be visible here despite the rollback
	// above.
	if _, err := sessions.Get(ctx, created.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("sessions.Get after rollback: err=%v, want pgx.ErrNoRows (session must not have survived the caller's rollback)", err)
	}

	turnRows, err := turns.ListForSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("list turns after rollback: %v", err)
	}
	if len(turnRows) != 0 {
		t.Errorf("len(turns) after rollback = %d, want 0 (turn must not have survived the caller's rollback)", len(turnRows))
	}
}

// TestCreateSessionOnTx_CallerCommit_Persists is
// TestCreateSessionOnTx_CallerRollback_PersistsNothing's mirror image:
// the SAME sequence, except the caller commits instead of rolling back --
// proving CreateSessionOnTx's writes DO durably persist once the caller
// (not CreateSessionOnTx itself) actually decides to commit.
func TestCreateSessionOnTx_CallerCommit_Persists(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	prompt := "fix the failing check"
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "narvi", Url: "https://github.com/narvidev/narvi"},
		},
	}

	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}

	created, hasPrompt, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CreateSessionOnTx: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if !hasPrompt {
		t.Error("hasPrompt = false, want true (req.Prompt was non-nil)")
	}

	// The caller, not CreateSessionOnTx, is responsible for committing --
	// CreateSessionOnTx itself never calls tx.Commit.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}

	got, err := sessions.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("sessions.Get after commit: %v", err)
	}
	if got.CreatedBy.Valid {
		t.Error("CreatedBy.Valid = true, want false (NULL) for a nil creator")
	}

	turnRows, err := turns.ListForSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("list turns after commit: %v", err)
	}
	if len(turnRows) != 1 {
		t.Fatalf("len(turns) after commit = %d, want 1", len(turnRows))
	}
}

// TestCreateSessionOnTx_ValidationFailure_NeverTouchesTx proves a
// request-validation failure (empty repos here) returns a CreateSessionError
// WITHOUT the caller's tx having been used for any write at all -- the
// caller's own tx is still perfectly usable afterward (this test proves
// it by successfully committing an empty tx), matching CreateSessionCore's
// own pre-existing "reject before ever writing" behavior for the exact
// same validation failure.
func TestCreateSessionOnTx_ValidationFailure_NeverTouchesTx(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Repos:       []restdtos.CreateSessionRequestReposElem{}, // empty: must fail validation
	}

	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, hasPrompt, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want a CreateSessionError for empty repos")
	}
	if cerr.Status != http.StatusBadRequest {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusBadRequest)
	}
	if hasPrompt {
		t.Error("hasPrompt = true, want false on a validation failure")
	}

	// The tx itself must still be perfectly usable (proving nothing was
	// written, and no error/abort state was left on the connection).
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("tx.Commit after validation failure: %v", err)
	}
}

// TestCreateSessionCore_ValidationFailure_NeverAcquiresConnection proves
// the trust-boundary invariant create.go's own doc comments describe ("a
// rejected repo spec never reaches Postgres at all") actually holds on
// CreateSessionCore's real pool-based path -- not just, as
// TestCreateSessionOnTx_ValidationFailure_NeverTouchesTx above already
// proves, that CreateSessionOnTx doesn't write to a tx the TEST itself
// opened.
//
// It opens its own MaxConns:1 pool against the same database, then
// deliberately holds that single connection open for the whole test (via
// a separate, still-open tx on a throwaway table) before ever calling
// CreateSessionCore with a request that fails pure in-memory validation
// (empty repos). If CreateSessionCore validated AFTER pool.Begin (the
// regression this test guards against), it would block forever waiting
// for a connection no one is going to release, and the bounded context
// below would fail the test with context.DeadlineExceeded instead of the
// expected 400. Validating BEFORE pool.Begin means the call returns
// immediately, with the one pooled connection still held elsewhere the
// entire time.
func TestCreateSessionCore_ValidationFailure_NeverAcquiresConnection(t *testing.T) {
	ctx := context.Background()
	_, connStr := newCoreTestPoolAndConnStr(t)

	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.MaxConns = 1

	limitedPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(limitedPool.Close)

	// Hold the pool's only connection open on an unrelated tx for the
	// whole test, so any attempt by CreateSessionCore to acquire a
	// SECOND connection (i.e. pool.Begin having already run) would block
	// with none available.
	holderTx, err := limitedPool.Begin(ctx)
	if err != nil {
		t.Fatalf("holderTx Begin: %v", err)
	}
	t.Cleanup(func() { _ = holderTx.Rollback(ctx) })

	sessions := narvipg.NewSessionStore(limitedPool)
	turns := narvipg.NewTurnStore(limitedPool)
	environments := narvipg.NewEnvironmentStore(limitedPool)
	auditLog := narvipg.NewAuditLogStore(limitedPool)
	repoSettings := narvipg.NewRepoSettingsStore(limitedPool)
	prSessions := narvipg.NewGitHubPRSessionStore(limitedPool)
	registry, err := sessionactor.NewRegistry(ctx, limitedPool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Repos:       []restdtos.CreateSessionRequestReposElem{}, // empty: must fail validation
	}
	var nilCreator pgtype.UUID

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, cerr := CreateSessionCore(callCtx, limitedPool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionCore: got nil error, want a CreateSessionError for empty repos")
	}
	if cerr.Status != http.StatusBadRequest {
		t.Errorf("cerr.Status = %d, want %d (got it without blocking on the held connection: %s)", cerr.Status, http.StatusBadRequest, cerr.Message)
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		t.Error("callCtx deadline exceeded -- CreateSessionCore blocked waiting for a connection instead of validating first")
	}
}

// TestTriggerDispatch_ExistingSession_SucceedsAndSpawns proves
// TriggerDispatch's own GetOrSpawn+Send(EnsureDispatched{}) sequencing in
// isolation, mirroring how CreateSessionCore's own post-commit dispatch is
// otherwise only ever proven indirectly (TestCreateSessionCore_
// NilCreator_WithPromptDispatches above, via a successful, error-free
// call): calling TriggerDispatch for a real, already-committed session
// leaves the registry holding a live actor for it -- proven here by a
// follow-up GetOrSpawn returning the SAME actor with no error (a second
// hydration attempt for an already-locally-registered session is a cheap
// in-memory lookup, not a new advisory-lock acquisition, so this only
// succeeds if TriggerDispatch's own GetOrSpawn call actually won and
// registered the actor).
func TestTriggerDispatch_ExistingSession_SucceedsAndSpawns(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	prompt := "fix the failing check"
	req := restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Prompt:      restdtos.CreateSessionRequestPrompt(&prompt),
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "narvi", Url: "https://github.com/narvidev/narvi"},
		},
	}

	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	created, hasPrompt, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CreateSessionOnTx: status=%d message=%q", cerr.Status, cerr.Message)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}
	if !hasPrompt {
		t.Fatal("hasPrompt = false, want true")
	}

	TriggerDispatch(ctx, registry, created.ID)

	actor, err := registry.GetOrSpawn(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn after TriggerDispatch: %v (want the actor TriggerDispatch itself spawned still registered)", err)
	}
	if actor == nil {
		t.Fatal("GetOrSpawn after TriggerDispatch returned a nil actor")
	}
}

// TestTriggerDispatch_UnknownSession_DoesNotPanic proves TriggerDispatch's
// own fire-and-forget contract for the FAILURE half of its sequencing: a
// sessionID with no backing sessions row (so GetOrSpawn's own
// hydrateAndAcquire fails at the BumpActorEpoch step) is only ever
// warn-logged, never panics and never returns anything the caller must
// check -- exactly why CreateSessionCore/CreateSessionOnTx callers can
// treat it as pure fire-and-forget.
func TestTriggerDispatch_UnknownSession_DoesNotPanic(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	unknownSessionID := pgtype.UUID{Bytes: [16]byte{0x01}, Valid: true}

	TriggerDispatch(ctx, registry, unknownSessionID)
}

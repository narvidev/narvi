//go:build integration

// Integration tests for AuthorizeResolvedActor/OwnedOrJoined against a
// REAL Postgres instance -- gated behind the "integration" build tag,
// mirroring internal/app/identitylink's own testcontainers-Postgres-plus-
// embedded-migrations convention exactly (each DB-touching package builds
// its own copy of this small helper rather than sharing one across package
// boundaries). Run via `make test-integration`.
//
// These cases are moved/adapted from internal/adapters/inbound/{slack,
// linear}'s own pre-extraction coverage (identity_integration_test.go in
// each package, which exercised authorizeResolvedActor/ownedOrJoined only
// indirectly, through a full webhook/interactivity request) -- proving
// the extraction into this package changed no behavior: same role matrix
// verdicts, same disabled-user-fails-closed check, same unresolved-actor
// short-circuit, same own/joined resolution.
package actorauthz_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/migrations"
)

// newTestPool spins up a throwaway Postgres container, runs every embedded
// migration up, and returns a ready *pgxpool.Pool -- a duplicate of
// identitylink's own newTestPool, necessarily so (see that file's own doc
// comment for this codebase's established per-package precedent).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	// startCtx bounds the container-startup call below via the ambient
	// context (image pull + Docker daemon round trip + Postgres's own
	// internal ready-wait) -- kept as defense in depth, but NOT solely
	// relied upon any more: CI run 30834918806 showed this exact bound
	// (added after CI run 30831633470's own ContainerStart hang) itself
	// fail to actually cut the call off when the hang recurred one layer
	// deeper, inside testcontainers-go's own wait.(*LogStrategy).
	// WaitUntilReady -- the goroutine dump showed it looping on a 100ms
	// poll for the FULL 10-minute panic window, never once observing
	// ctx.Done(), despite this same context chain being correctly wired
	// all the way through (confirmed directly: reproducing an
	// impossible-to-satisfy wait condition locally against this exact
	// call DOES correctly time out via this same context mechanism, at
	// testcontainers' own hardcoded 60s deadline -- so the mechanism is
	// sound in isolation, but evidently not dependable against whatever a
	// genuinely stalled CI-runner Docker daemon does to it in practice).
	//
	// Rather than keep chasing exactly why context cancellation isn't
	// always honored deep inside a third-party library under conditions
	// this dev machine cannot reproduce, the startup call now ALSO runs on
	// its own goroutine (via errgroup.Group.Go -- no naked `go` statement,
	// §11) raced against an independent, plain time.After watchdog:
	// whichever of "the call returned" or "the watchdog fired" happens
	// first decides the outcome, with no dependency on any context
	// cancellation actually being honored by anything downstream. If the
	// watchdog wins, the goroutine is deliberately abandoned (leaked, not
	// joined) rather than blocking this test's own cleanup on a call that
	// has already demonstrated it can ignore its own cancellation signal.
	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	const containerStartWatchdog = 2*time.Minute + 15*time.Second
	type containerStartResult struct {
		container *tcpostgres.PostgresContainer
		err       error
	}
	startCh := make(chan containerStartResult, 1)
	var startGroup errgroup.Group
	startGroup.Go(func() error {
		container, err := tcpostgres.Run(startCtx, "postgres:17-alpine",
			tcpostgres.WithDatabase("narvi_test"),
			tcpostgres.WithUsername("narvi"),
			tcpostgres.WithPassword("narvi"),
			tcpostgres.BasicWaitStrategies(),
		)
		startCh <- containerStartResult{container: container, err: err}
		return nil
	})

	var container *tcpostgres.PostgresContainer
	var err error
	select {
	case res := <-startCh:
		container, err = res.container, res.err
		if err != nil {
			t.Fatalf("start postgres container: %v", err)
		}
	case <-time.After(containerStartWatchdog):
		t.Fatalf("start postgres container: tcpostgres.Run did not return within %s -- Docker daemon likely "+
			"stalled without honoring context cancellation (see this function's own doc comment)", containerStartWatchdog)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrateDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = migrateDB.Close() })

	dbDriver, err := migratepg.WithInstance(migrateDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migratepg.WithInstance: %v", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestAuthorizeResolvedActor_UnresolvedActorAllowedWithNoLookup proves
// §13.2's own "unlinked actors get bot attribution ... the action
// proceeds" precedent: an invalid actorUserID short-circuits to
// allowed=true with NO users lookup at all -- proven here by passing a
// nil *postgres.UserStore, which would panic on any actual dereference.
func TestAuthorizeResolvedActor_UnresolvedActorAllowedWithNoLookup(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()

	got := actorauthz.AuthorizeResolvedActor(ctx, logger, "test", nil, pgtype.UUID{}, authz.ActionCreateSession, authz.Resource{})
	if !got {
		t.Error("AuthorizeResolvedActor() = false, want true for an unresolved (invalid) actor")
	}
}

// TestAuthorizeLinkedActor_UnresolvedActorDeniedWithNoLookup mirrors
// TestAuthorizeResolvedActor_UnresolvedActorAllowedWithNoLookup above --
// same shape, opposite verdict: AuthorizeLinkedActor is the audit-hardening
// counterpart that DENIES (rather than allows) an unresolved actor, and
// does so with NO users lookup at all, proven here identically by passing
// a nil *postgres.UserStore (which would panic on any actual dereference).
func TestAuthorizeLinkedActor_UnresolvedActorDeniedWithNoLookup(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()

	got := actorauthz.AuthorizeLinkedActor(ctx, logger, "test", nil, pgtype.UUID{}, authz.ActionCreateSession, authz.Resource{})
	if got {
		t.Error("AuthorizeLinkedActor() = true, want false for an unresolved (invalid) actor")
	}
}

// TestAuthorizeResolvedActor_UnknownUserFailsClosed proves a role-lookup
// failure (here: a syntactically valid but nonexistent user id) denies
// rather than silently proceeding -- "should be unreachable in practice"
// per this function's own doc comment, but still fails closed if it ever
// happens.
func TestAuthorizeResolvedActor_UnknownUserFailsClosed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	logger := discardLogger()

	var nonexistent pgtype.UUID
	if err := nonexistent.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("scan uuid: %v", err)
	}

	got := actorauthz.AuthorizeResolvedActor(ctx, logger, "test", users, nonexistent, authz.ActionCreateSession, authz.Resource{})
	if got {
		t.Error("AuthorizeResolvedActor() = true, want false (fail closed) for a user id with no matching row")
	}
}

// TestAuthorizeResolvedActorVerdict_UnknownUserReturnsError is W9's own
// required proof (confirmed LOW finding: "an error is recorded as a
// verdict"): the SAME "syntactically valid but nonexistent user id"
// scenario TestAuthorizeResolvedActor_UnknownUserFailsClosed proves fails
// closed at the bool call site must resolve to LinkedActorError specifically
// -- NOT LinkedActorDenied -- through the verdict-returning form, because a
// role-lookup failure says nothing about whether this actor is actually
// authorized. A caller collapsing this into LinkedActorDenied (as the old
// plain-bool callers had no choice but to) would persist a false "this
// creator is unauthorized" verdict against an identity that was simply
// never looked up successfully.
func TestAuthorizeResolvedActorVerdict_UnknownUserReturnsError(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	logger := discardLogger()

	var nonexistent pgtype.UUID
	if err := nonexistent.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("scan uuid: %v", err)
	}

	got := actorauthz.AuthorizeResolvedActorVerdict(ctx, logger, "test", users, nonexistent, authz.ActionCreateSession, authz.Resource{})
	if got != actorauthz.LinkedActorError {
		t.Errorf("AuthorizeResolvedActorVerdict() = %v, want LinkedActorError (a role-lookup failure is not a verdict) for a user id with no matching row", got)
	}
}

// TestAuthorizeResolvedActorVerdict_DisabledUserReturnsDenied is the
// verdict-returning sibling of TestAuthorizeResolvedActor_
// DisabledUserDeniedEvenWithPermittingRole: a disabled account is a
// GENUINE, completed verdict (LinkedActorDenied), not a lookup error --
// this authorization ran to completion and correctly said no.
func TestAuthorizeResolvedActorVerdict_DisabledUserReturnsDenied(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	logger := discardLogger()

	user, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "actorauthz-verdict-disabled@example.com",
		DisplayName:  "Disabled Verdict Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("disable fixture user: %v", err)
	}

	got := actorauthz.AuthorizeResolvedActorVerdict(ctx, logger, "test", users, user.ID, authz.ActionCreateSession, authz.Resource{})
	if got != actorauthz.LinkedActorDenied {
		t.Errorf("AuthorizeResolvedActorVerdict() = %v, want LinkedActorDenied for a disabled user (a genuine, completed verdict)", got)
	}
}

// TestAuthorizeLinkedActorVerdict_UnresolvedActorReturnsDenied mirrors
// TestAuthorizeLinkedActor_UnresolvedActorDeniedWithNoLookup: an invalid
// (not-yet-linked) actorUserID is LinkedActorDenied, never LinkedActorError
// -- it is not a lookup failure, it is the exact, deliberate, structural
// denial this function exists to return (see AuthorizeLinkedActorVerdict's
// own doc comment). Proven with a nil *postgres.UserStore, which would
// panic on any actual dereference -- pinning "no lookup happens at all".
func TestAuthorizeLinkedActorVerdict_UnresolvedActorReturnsDenied(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()

	got := actorauthz.AuthorizeLinkedActorVerdict(ctx, logger, "test", nil, pgtype.UUID{}, authz.ActionCreateSession, authz.Resource{})
	if got != actorauthz.LinkedActorDenied {
		t.Errorf("AuthorizeLinkedActorVerdict() = %v, want LinkedActorDenied for an unresolved (invalid) actor", got)
	}
}

// TestAuthorizeResolvedActor_RoleMatrix is table-driven over the §13.3 role
// matrix verdicts this function renders once an actor IS resolved --
// mirrors slack/linear's own pre-extraction integration coverage (a
// viewer denied ActionCreateSession, a member denied ActionPromptSession
// without ownership but allowed with it, an admin/maintainer allowed
// regardless of ownership).
func TestAuthorizeResolvedActor_RoleMatrix(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	logger := discardLogger()

	tests := []struct {
		name     string
		role     sqlcgen.UserRole
		action   authz.Action
		resource authz.Resource
		want     bool
	}{
		{name: "viewer denied create session", role: sqlcgen.UserRoleViewer, action: authz.ActionCreateSession, resource: authz.Resource{}, want: false},
		{name: "member allowed create session (no ownership concept)", role: sqlcgen.UserRoleMember, action: authz.ActionCreateSession, resource: authz.Resource{}, want: true},
		{name: "member denied prompt without ownership", role: sqlcgen.UserRoleMember, action: authz.ActionPromptSession, resource: authz.Resource{OwnedOrJoined: false}, want: false},
		{name: "member allowed prompt with ownership", role: sqlcgen.UserRoleMember, action: authz.ActionPromptSession, resource: authz.Resource{OwnedOrJoined: true}, want: true},
		{name: "viewer denied prompt even with ownership", role: sqlcgen.UserRoleViewer, action: authz.ActionPromptSession, resource: authz.Resource{OwnedOrJoined: true}, want: false},
		{name: "maintainer allowed prompt without ownership", role: sqlcgen.UserRoleMaintainer, action: authz.ActionPromptSession, resource: authz.Resource{OwnedOrJoined: false}, want: true},
		{name: "admin allowed approve plan without ownership", role: sqlcgen.UserRoleAdmin, action: authz.ActionApprovePlan, resource: authz.Resource{OwnedOrJoined: false}, want: true},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, err := users.Create(ctx, sqlcgen.CreateUserParams{
				PrimaryEmail: fmt.Sprintf("actorauthz-matrix-%d@example.com", i),
				DisplayName:  "Matrix Test User",
				Role:         tc.role,
			})
			if err != nil {
				t.Fatalf("create fixture user: %v", err)
			}

			got := actorauthz.AuthorizeResolvedActor(ctx, logger, "test", users, user.ID, tc.action, tc.resource)
			if got != tc.want {
				t.Errorf("AuthorizeResolvedActor(role=%s, action=%s, resource=%+v) = %v, want %v", tc.role, tc.action, tc.resource, got, tc.want)
			}
		})
	}
}

// TestAuthorizeResolvedActor_DisabledUserDeniedEvenWithPermittingRole
// proves user.Disabled is checked BEFORE domain/authz.Authorize -- a
// disabled member (whose role would otherwise permit ActionCreateSession
// unconditionally) is still denied, mirroring slack/linear's own identical
// pre-extraction regression coverage exactly.
func TestAuthorizeResolvedActor_DisabledUserDeniedEvenWithPermittingRole(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	logger := discardLogger()

	user, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "actorauthz-disabled@example.com",
		DisplayName:  "Disabled Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("disable fixture user: %v", err)
	}

	got := actorauthz.AuthorizeResolvedActor(ctx, logger, "test", users, user.ID, authz.ActionCreateSession, authz.Resource{})
	if got {
		t.Error("AuthorizeResolvedActor() = true, want false for a disabled user even with an otherwise-permitting role")
	}
}

// TestOwnedOrJoined_TrueWhenCreator proves the "created it" half of the
// §13.3 row 2 own/joined carve-out, with no participants row at all.
func TestOwnedOrJoined_TrueWhenCreator(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)

	creator, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "actorauthz-owner@example.com", DisplayName: "Owner", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: creator.ID})
	if err != nil {
		t.Fatalf("create fixture session: %v", err)
	}

	joined, err := actorauthz.OwnedOrJoined(ctx, participants, session, creator.ID)
	if err != nil {
		t.Fatalf("OwnedOrJoined: %v", err)
	}
	if !joined {
		t.Error("OwnedOrJoined() = false, want true for the session's own creator")
	}
}

// TestOwnedOrJoined_TrueWhenParticipant proves the "joined it" half: a
// user who did NOT create the session, but has a participants row for it,
// still counts as owned/joined.
func TestOwnedOrJoined_TrueWhenParticipant(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)

	creator, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "actorauthz-creator@example.com", DisplayName: "Creator", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create fixture creator: %v", err)
	}
	joiner, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "actorauthz-joiner@example.com", DisplayName: "Joiner", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create fixture joiner: %v", err)
	}
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: creator.ID})
	if err != nil {
		t.Fatalf("create fixture session: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO participants (session_id, user_id) VALUES ($1, $2)`, session.ID, joiner.ID); err != nil {
		t.Fatalf("insert participants row: %v", err)
	}

	joined, err := actorauthz.OwnedOrJoined(ctx, participants, session, joiner.ID)
	if err != nil {
		t.Fatalf("OwnedOrJoined: %v", err)
	}
	if !joined {
		t.Error("OwnedOrJoined() = false, want true for a joined (non-creator) participant")
	}
}

// TestOwnedOrJoined_FalseWhenNeitherCreatorNorParticipant proves the
// negative case: neither creator nor participant.
func TestOwnedOrJoined_FalseWhenNeitherCreatorNorParticipant(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)

	creator, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "actorauthz-creator2@example.com", DisplayName: "Creator", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create fixture creator: %v", err)
	}
	stranger, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "actorauthz-stranger@example.com", DisplayName: "Stranger", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create fixture stranger: %v", err)
	}
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: creator.ID})
	if err != nil {
		t.Fatalf("create fixture session: %v", err)
	}

	joined, err := actorauthz.OwnedOrJoined(ctx, participants, session, stranger.ID)
	if err != nil {
		t.Fatalf("OwnedOrJoined: %v", err)
	}
	if joined {
		t.Error("OwnedOrJoined() = true, want false for a user who neither created nor joined")
	}
}

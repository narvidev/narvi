//go:build integration

// Integration tests for Resolve/Consume against a REAL Postgres instance
// (§9.1) -- gated behind the "integration" build tag, mirroring internal/
// app/outboxworker/builder_integration_test.go's own conventions exactly
// (testcontainers Postgres, embedded migrations via golang-migrate's iofs
// source driver). Run via `make test-integration`.
package identitylink_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/migrations"
)

// newTestPool spins up a throwaway Postgres container, runs every embedded
// migration up, and returns a ready *pgxpool.Pool -- mirrors internal/app/
// outboxworker's own identical helper.
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
		t.Fatalf("migrate postgres driver: %v", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate iofs source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func newDeps(pool *pgxpool.Pool) identitylink.Deps {
	return identitylink.Deps{
		Pool:          pool,
		Users:         narvipg.NewUserStore(pool),
		Identities:    narvipg.NewIdentityStore(pool),
		LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:      narvipg.NewAuditLogStore(pool),
		PublicBaseURL: "https://narvi.example.com",
		PromptTTL:     24 * time.Hour,
	}
}

func createFixtureUser(ctx context.Context, t *testing.T, users *narvipg.UserStore, email string) sqlcgen.User {
	t.Helper()
	u, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: email, Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create fixture user %q: %v", email, err)
	}
	return u
}

// TestResolve_AlreadyLinkedIsFastPath proves an already-linked identity
// resolves to its user id with NO email fetch needed at all (emailOK is
// deliberately false here -- if Resolve tried to use it, this test would
// fail).
func TestResolve_AlreadyLinkedIsFastPath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUser(ctx, t, deps.Users, "already-linked@example.com")
	if _, err := deps.Identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     user.ID,
		Provider:   sqlcgen.IdentityProviderSlack,
		ExternalID: "U-ALREADY",
		LinkedVia:  sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create fixture identity: %v", err)
	}

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-ALREADY", identitylink.ProfileEmail{}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.UserID != user.ID {
		t.Errorf("UserID = %v, want %v", res.UserID, user.ID)
	}
	if res.AutoLinked {
		t.Error("AutoLinked = true, want false (already linked, not a new auto-link)")
	}
	if res.MagicLinkURL != "" {
		t.Errorf("MagicLinkURL = %q, want empty", res.MagicLinkURL)
	}
}

// TestResolve_EmailFetchFailedDoesNothing proves emailOK=false (every
// retry already exhausted) results in bot attribution with NO prompt
// created -- §13.2's own "never null-out an email on transient failure /
// never guess" rule.
func TestResolve_EmailFetchFailedDoesNothing(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-NOFETCH", identitylink.ProfileEmail{}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.UserID.Valid {
		t.Errorf("UserID = %v, want invalid (bot attribution)", res.UserID)
	}
	if res.MagicLinkURL != "" {
		t.Errorf("MagicLinkURL = %q, want empty (must not guess on a failed fetch)", res.MagicLinkURL)
	}

	if _, err := deps.LinkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-NOFETCH"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a link prompt was created despite a failed fetch: %v", err)
	}
}

// TestResolve_ExactlyOneMatchAutoLinks proves a unique email match
// creates the identities row (linked_via=auto_email, email_verified=true)
// and an audit_log entry, atomically.
func TestResolve_ExactlyOneMatchAutoLinks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUser(ctx, t, deps.Users, "unique-match@example.com")

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-UNIQUE", identitylink.ProfileEmail{Address: "unique-match@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.AutoLinked {
		t.Error("AutoLinked = false, want true")
	}
	if res.UserID != user.ID {
		t.Errorf("UserID = %v, want %v", res.UserID, user.ID)
	}
	if res.MagicLinkURL != "" {
		t.Errorf("MagicLinkURL = %q, want empty", res.MagicLinkURL)
	}

	linked, err := deps.Identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, "U-UNIQUE")
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if linked.LinkedVia != sqlcgen.IdentityLinkedViaAutoEmail {
		t.Errorf("LinkedVia = %v, want auto_email", linked.LinkedVia)
	}
	if !linked.EmailVerified {
		t.Error("EmailVerified = false, want true")
	}
	if !linked.EmailCheckedAt.Valid {
		t.Error("EmailCheckedAt not set, want the time the verified email was checked")
	}

	entries, err := deps.AuditLog.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AuditLog.List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Action != "identity.auto_linked" {
		t.Errorf("Action = %q, want %q", entries[0].Action, "identity.auto_linked")
	}
	if entries[0].ActorUserID.Valid {
		t.Error("ActorUserID.Valid = true, want false (system-driven auto-link, no human actor)")
	}
}

// TestResolve_ZeroMatchesCreatesLinkPrompt proves a fetched email that
// matches no user creates a link prompt and returns a magic link.
func TestResolve_ZeroMatchesCreatesLinkPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderLinear, "L-NOBODY", identitylink.ProfileEmail{Address: "nobody@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.UserID.Valid {
		t.Errorf("UserID = %v, want invalid", res.UserID)
	}
	if res.AutoLinked {
		t.Error("AutoLinked = true, want false")
	}
	if res.MagicLinkURL == "" {
		t.Fatal("MagicLinkURL = empty, want a real magic link")
	}

	prompt, err := deps.LinkPrompts.GetLatestForProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, "L-NOBODY")
	if err != nil {
		t.Fatalf("GetLatestForProviderAndExternalID: %v", err)
	}
	if prompt.ExternalID != "L-NOBODY" {
		t.Errorf("prompt.ExternalID = %q, want %q", prompt.ExternalID, "L-NOBODY")
	}
}

// TestResolve_MultipleMatchesCreatesLinkPrompt proves an email matching
// TWO distinct users is treated exactly like zero matches -- never guess.
func TestResolve_MultipleMatchesCreatesLinkPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	email := "shared@example.com"
	first := createFixtureUser(ctx, t, deps.Users, "first-holder@example.com")
	second := createFixtureUser(ctx, t, deps.Users, "second-holder@example.com")
	// Neither user's OWN primary_email is the shared one -- both match via
	// a verified identity carrying it instead, proving the "verified
	// identity email" half of step 2 can itself produce multiple matches.
	for i, u := range []sqlcgen.User{first, second} {
		e := email
		if _, err := deps.Identities.Create(ctx, sqlcgen.CreateIdentityParams{
			UserID:        u.ID,
			Provider:      sqlcgen.IdentityProviderGithub,
			ExternalID:    "gh-shared-" + string(rune('a'+i)),
			Email:         &e,
			EmailVerified: true,
			LinkedVia:     sqlcgen.IdentityLinkedViaAdmin,
		}); err != nil {
			t.Fatalf("create fixture identity #%d: %v", i, err)
		}
	}

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-AMBIGUOUS", identitylink.ProfileEmail{Address: email, Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.UserID.Valid {
		t.Errorf("UserID = %v, want invalid (must never guess between 2 matches)", res.UserID)
	}
	if res.MagicLinkURL == "" {
		t.Fatal("MagicLinkURL = empty, want a real magic link")
	}
}

// TestResolve_ReusesStillLiveLinkPrompt proves a second unresolved event
// for the SAME identity, while an earlier prompt is still unexpired,
// reuses it silently (empty MagicLinkURL) rather than minting/sending a
// second one.
func TestResolve_ReusesStillLiveLinkPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	first, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-REPEAT", identitylink.ProfileEmail{Address: "still-nobody@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve (first): %v", err)
	}
	if first.MagicLinkURL == "" {
		t.Fatal("first.MagicLinkURL = empty, want a real magic link")
	}

	second, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-REPEAT", identitylink.ProfileEmail{Address: "still-nobody@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve (second): %v", err)
	}
	if second.MagicLinkURL != "" {
		t.Errorf("second.MagicLinkURL = %q, want empty (must not re-send while the first is still live)", second.MagicLinkURL)
	}
}

// TestConsume_LinksIdentityAndDeletesPrompt proves the full magic-link
// round trip: Resolve mints a prompt (extracting the plaintext nonce from
// its own URL for this test's own convenience), then Consume, given the
// authenticated user, links the identity, records a human-actor audit-log
// entry, and deletes the prompt so it cannot be replayed.
//
// Deliberately UNCHANGED by §13.2's own security-remediation fix
// ("identities + full RBAC", §13.2): "the-clicker" here is simply this
// package's own test double for "whoever is authenticated and presents
// the nonce" -- Consume itself performs no correlation between that
// visitor and the identity that originally triggered the prompt (see
// Consume's own updated doc comment, service.go, for why that check
// cannot live here at all). This test's own assertions still correctly
// describe Consume's real, documented contract; what actually closes the
// confirmed hijack this shape once demonstrated is delivery-scoping
// upstream (chat.postEphemeral, internal/adapters/inbound/slack/ack.go),
// proved separately by internal/adapters/inbound/slack's own
// TestHandler_AppMention_IdentityNoticeDeliveredEphemerally.
func TestConsume_LinksIdentityAndDeletesPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderLinear, "L-CONSUME", identitylink.ProfileEmail{Address: "consume-me@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	nonce := res.MagicLinkURL[len(deps.PublicBaseURL+identitylink.MagicLinkPath):]

	authenticatedUser := createFixtureUser(ctx, t, deps.Users, "the-clicker@example.com")

	created, err := identitylink.Consume(ctx, deps, nonce, authenticatedUser.ID)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if created.UserID != authenticatedUser.ID {
		t.Errorf("created.UserID = %v, want %v", created.UserID, authenticatedUser.ID)
	}
	if created.Provider != sqlcgen.IdentityProviderLinear || created.ExternalID != "L-CONSUME" {
		t.Errorf("created = %+v, want provider=linear external_id=L-CONSUME", created)
	}
	if created.LinkedVia != sqlcgen.IdentityLinkedViaPrompt {
		t.Errorf("LinkedVia = %v, want prompt", created.LinkedVia)
	}

	entries, err := deps.AuditLog.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AuditLog.List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Action != "identity.linked_via_prompt" {
		t.Errorf("Action = %q, want %q", entries[0].Action, "identity.linked_via_prompt")
	}
	if !entries[0].ActorUserID.Valid || entries[0].ActorUserID != authenticatedUser.ID {
		t.Errorf("ActorUserID = %v, want %v (a real human actor)", entries[0].ActorUserID, authenticatedUser.ID)
	}

	// The prompt must be gone -- a replay attempt gets ErrLinkPromptNotFound.
	if _, err := identitylink.Consume(ctx, deps, nonce, authenticatedUser.ID); !errors.Is(err, identitylink.ErrLinkPromptNotFound) {
		t.Errorf("Consume (replay) = %v, want ErrLinkPromptNotFound", err)
	}
}

// TestConsume_UnknownNonceReturnsNotFound proves a bogus/never-issued
// nonce is rejected cleanly.
func TestConsume_UnknownNonceReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	user := createFixtureUser(ctx, t, deps.Users, "unknown-nonce@example.com")

	_, err := identitylink.Consume(ctx, deps, "totally-bogus-nonce", user.ID)
	if !errors.Is(err, identitylink.ErrLinkPromptNotFound) {
		t.Fatalf("Consume = %v, want ErrLinkPromptNotFound", err)
	}
}

// TestConsume_ExpiredPromptReturnsExpiredAndCleansUp proves an expired
// prompt is rejected AND deleted (so the same nonce next resolves to
// ErrLinkPromptNotFound, not ErrLinkPromptExpired again).
func TestConsume_ExpiredPromptReturnsExpiredAndCleansUp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)
	// PromptTTL in the past -- the prompt Resolve creates below is
	// already expired the instant it's created.
	deps.PromptTTL = -time.Hour

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-EXPIRED", identitylink.ProfileEmail{Address: "expired@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	nonce := res.MagicLinkURL[len(deps.PublicBaseURL+identitylink.MagicLinkPath):]

	user := createFixtureUser(ctx, t, deps.Users, "expired-clicker@example.com")

	if _, err := identitylink.Consume(ctx, deps, nonce, user.ID); !errors.Is(err, identitylink.ErrLinkPromptExpired) {
		t.Fatalf("Consume = %v, want ErrLinkPromptExpired", err)
	}

	if _, err := identitylink.Consume(ctx, deps, nonce, user.ID); !errors.Is(err, identitylink.ErrLinkPromptNotFound) {
		t.Fatalf("Consume (second attempt) = %v, want ErrLinkPromptNotFound (expired row must be cleaned up)", err)
	}
}

// TestConsume_AlreadyLinkedElsewhereReturnsConflict proves a race where
// the SAME (provider, externalID) gets linked by a DIFFERENT path (here:
// simulated by directly inserting the winning identities row) between
// this prompt being minted and this click being consumed surfaces as
// ErrIdentityAlreadyLinked, not a raw database unique-violation error.
func TestConsume_AlreadyLinkedElsewhereReturnsConflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	deps := newDeps(pool)

	res, err := identitylink.Resolve(ctx, deps, sqlcgen.IdentityProviderSlack, "U-RACE", identitylink.ProfileEmail{Address: "racer@example.com", Verified: true}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	nonce := res.MagicLinkURL[len(deps.PublicBaseURL+identitylink.MagicLinkPath):]

	// Simulate a concurrent winner linking the SAME identity by a
	// different path (e.g. an admin force-link, or a later auto-link)
	// before this prompt is ever consumed.
	winner := createFixtureUser(ctx, t, deps.Users, "race-winner@example.com")
	if _, err := deps.Identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:     winner.ID,
		Provider:   sqlcgen.IdentityProviderSlack,
		ExternalID: "U-RACE",
		LinkedVia:  sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create fixture identity (simulating a concurrent winner): %v", err)
	}

	loser := createFixtureUser(ctx, t, deps.Users, "race-loser@example.com")

	if _, err := identitylink.Consume(ctx, deps, nonce, loser.ID); !errors.Is(err, identitylink.ErrIdentityAlreadyLinked) {
		t.Fatalf("Consume = %v, want ErrIdentityAlreadyLinked", err)
	}
}

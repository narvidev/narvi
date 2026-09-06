//go:build integration

// This file (rolloutgate_integration_test.go) proves §10's own
// primary, session-creation-time gate (§10 Phase 6, §32): checkRolloutGate
// (rolloutgate.go), exercised indirectly through the real, exported
// CreateSessionOnTx entry point -- deliberately in package httpapi (not
// httpapi_test), mirroring createcore_integration_test.go's own precedent
// exactly (checkRolloutGate itself is unexported, and every test here
// needs to construct CreateSessionOnTx's own arguments directly, including
// its own transaction, the same way that file's tests do).
package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// rolloutTestRepo returns a repo clone URL and its "owner/repo" full name,
// unique per test (derived from t.Name()) so concurrent -tags=integration
// runs in this same binary never collide on the SAME repo_settings row --
// mirrors this package's own established "unique-per-test fixture key"
// precedent used throughout the reposettings/reviewverdict integration
// suites.
func rolloutTestRepo(t *testing.T) (url, fullName string) {
	t.Helper()
	name := "rollout-" + t.Name()
	return "https://github.com/acme/" + name + ".git", "acme/" + name
}

func newRolloutGateTestReq(repoURLs ...string) restdtos.CreateSessionRequest {
	repos := make([]restdtos.CreateSessionRequestReposElem, len(repoURLs))
	for i, u := range repoURLs {
		repos[i] = restdtos.CreateSessionRequestReposElem{Name: "widgets", Url: u}
	}
	return restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Repos:       repos,
	}
}

// TestCreateSessionOnTx_RolloutGate_OpenMode_NoOpEvenWithNoRepoSettingsRow
// is §32's own required no-op proof, at the gate layer: platform.
// RolloutModeOpen (config_test.go's own TestLoadRolloutMode proves this is
// EXACTLY the value platform.Load returns when NARVI_ROLLOUT_MODE is
// unset -- together the two tests prove the full unset-env-var-to-gate-
// behavior chain) admits a session for a repo with ZERO repo_settings
// rows at all, byte-for-byte the same as every session created before
// this Step existed.
//
// Mutation anchor: flipping checkRolloutGate's own `if mode !=
// rollout.ModeCohort { return nil }` guard (e.g. to `==`) makes this test
// fail -- proving this test actually exercises the no-op short-circuit,
// not merely a coincidentally-passing default.
func TestCreateSessionOnTx_RolloutGate_OpenMode_NoOpEvenWithNoRepoSettingsRow(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := rolloutTestRepo(t)
	req := newRolloutGateTestReq(repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionOnTx: status=%d message=%q, want success (open mode is a byte-for-byte no-op, even with zero repo_settings rows for this repo)", cerr.Status, cerr.Message)
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid -- CreateSessionOnTx did not actually insert a session")
	}
}

// TestCreateSessionOnTx_RolloutGate_CohortMode_EnrolledRepoAdmitted proves
// the positive case: a repo with sessions_enabled=true is admitted under
// ModeCohort.
func TestCreateSessionOnTx_RolloutGate_CohortMode_EnrolledRepoAdmitted(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, fullName := rolloutTestRepo(t)
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, fullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}

	req := newRolloutGateTestReq(repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionOnTx: status=%d message=%q, want success (repo is enrolled)", cerr.Status, cerr.Message)
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid")
	}
}

// TestCreateSessionOnTx_RolloutGate_CohortMode_AbsentRowRefused proves the
// fail-closed default: NO repo_settings row at all, under ModeCohort, is
// refused -- never silently treated as enrolled.
func TestCreateSessionOnTx_RolloutGate_CohortMode_AbsentRowRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := rolloutTestRepo(t)
	req := newRolloutGateTestReq(repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal for an unenrolled (no row at all) repo under ModeCohort")
	}
	if cerr.Status != http.StatusForbidden {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusForbidden)
	}
	if !cerr.RolloutRefusal {
		t.Error("cerr.RolloutRefusal = false, want true -- callers must be able to tell this apart from a transient failure structurally")
	}
}

// TestCreateSessionOnTx_RolloutGate_CohortMode_DisabledRowRefused proves
// the fail-closed default holds for an EXISTING row with
// sessions_enabled=false too, not just an absent row -- explicit
// de-enrollment must refuse exactly like never-having-enrolled-at-all.
func TestCreateSessionOnTx_RolloutGate_CohortMode_DisabledRowRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, fullName := rolloutTestRepo(t)
	// Explicitly write sessions_enabled=false (as opposed to
	// AbsentRowRefused above, which never writes a row at all) --
	// mirrors an operator's own rollback write (§32.9).
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, fullName, false); err != nil {
		t.Fatalf("seed disabled row: %v", err)
	}

	req := newRolloutGateTestReq(repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal for a repo whose row explicitly sets sessions_enabled=false")
	}
	if !cerr.RolloutRefusal {
		t.Error("cerr.RolloutRefusal = false, want true")
	}
}

// TestCreateSessionOnTx_RolloutGate_CohortMode_MultiRepoRequiresAllEnrolled
// is the mutation anchor for §32's own "multi-repo sessions: all named
// repos must be enrolled" requirement, at the real CreateSessionOnTx
// entry point (internal/domain/rollout's own unit tests already prove the
// pure decision; this proves the I/O-driving wrapper wires it correctly):
// one enrolled repo plus one unenrolled repo is refused, even though the
// FIRST repo alone would have been admitted.
func TestCreateSessionOnTx_RolloutGate_CohortMode_MultiRepoRequiresAllEnrolled(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	enrolledURL, enrolledFullName := "https://github.com/acme/"+t.Name()+"-enrolled.git", "acme/"+t.Name()+"-enrolled"
	unenrolledURL := "https://github.com/acme/" + t.Name() + "-unenrolled.git"
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, enrolledFullName, true); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}

	req := newRolloutGateTestReq(enrolledURL, unenrolledURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- one of the two named repos is not enrolled")
	}
	if !cerr.RolloutRefusal {
		t.Error("cerr.RolloutRefusal = false, want true")
	}
}

// TestCreateSessionOnTx_RolloutGate_CohortMode_CrossHostSpoofRefused is
// the mutation anchor for §32.3's own host-verification requirement:
// "acme/widgets"-shaped repo IS enrolled under github.com, but the
// REQUEST names a DIFFERENT host (evil.example) whose URL path happens to
// derive the identical owner/repo via reposource.ParseOwnerRepo's own
// deliberately host-agnostic parsing. This must be refused.
//
// Mutation anchor: removing resolveTrustedRepoFullName's own
// reposource.CheckRepoHost call (rolloutgate.go) -- i.e. calling
// reposource.ParseOwnerRepo directly on repo.Url with no host check first
// -- makes this test incorrectly PASS admission (spoofed as the real
// enrolled repo), flipping this test from refused to admitted and
// failing it.
func TestCreateSessionOnTx_RolloutGate_CohortMode_CrossHostSpoofRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	ownerRepo := "acme/" + t.Name() + "-spoof"
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, ownerRepo, true); err != nil {
		t.Fatalf("seed enrollment under github.com: %v", err)
	}

	// SAME owner/repo path, but a host that is NOT in
	// ports.SupportedSourceControlHosts() -- reposource.ParseOwnerRepo
	// alone would derive the IDENTICAL "acme/<repo>-spoof" full name from
	// this URL, since it never inspects the host at all.
	spoofedURL := "https://evil.example/" + ownerRepo + ".git"
	req := newRolloutGateTestReq(spoofedURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- evil.example must never be treated as the github.com repo it happens to share an owner/repo path with")
	}
	if !cerr.RolloutRefusal {
		t.Error("cerr.RolloutRefusal = false, want true")
	}
}

// TestCreateSessionOnTx_RolloutGate_CohortMode_ReadErrorFailsClosedButNotAsPolicy
// proves §32's own "read error -> refused, but retryably" rule (Root
// Cause 2 of the adversarial review that found this Step's own original
// version of this test locked in the WRONG expectation -- see below) using
// this package's established fault-injection idiom (an already-rolled-back
// tx standing in for a genuine store outage -- RepoSettingsStore.WithTx's
// own doc comment cites this exact precedent, TestBuild_
// CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub in
// internal/app/decisioninbox). Passing an already-closed tx as
// CreateSessionOnTx's OWN transaction means checkRolloutGate's
// repoSettings.WithTx(tx).Get call is the first thing to fail (it runs
// immediately after validateCreateSessionRequest, before any write) --
// exercising the read-error branch specifically, not merely "some
// downstream write failed".
//
// Renamed from this test's own original ReadErrorFailsClosed: fail-closed
// (cerr != nil, this repo is never silently admitted) is UNCHANGED and
// still asserted below, but this test's own ORIGINAL assertion --
// cerr.RolloutRefusal == true for a read error -- encoded exactly the bug
// an adversarial review found: fail-closed and "this is a permanent
// policy refusal" are different properties, and a degraded read is only
// the first one. See checkRolloutGate's own doc comment (rolloutgate.go)
// for the full "why" -- conflating them made a momentary database blip
// permanently drop legitimate work on all four ingress channels, with no
// retry.
//
// Mutation anchors: (1) changing checkRolloutGate's own `default:` case
// (a genuine read error) to treat it as enrolled=true instead of false
// would make this test incorrectly succeed, flipping it from refused to
// admitted; (2) re-marking this refusal as RolloutRefusal: true (reverting
// Root Cause 2's own fix) makes this test's own RolloutRefusal assertion
// fail.
func TestCreateSessionOnTx_RolloutGate_CohortMode_ReadErrorFailsClosedButNotAsPolicy(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := rolloutTestRepo(t)
	req := newRolloutGateTestReq(repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx (fault injection setup): %v", err)
	}
	// tx is now closed -- any query against it (including checkRolloutGate's
	// own repoSettings.WithTx(tx).Get) returns a genuine error, standing in
	// for a real Postgres outage (including a context-canceled or timed-out
	// query, which pgx surfaces the SAME way: a non-nil, non-ErrNoRows
	// error on the query call).

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- a genuine repo_settings read failure must fail CLOSED, never silently admit")
	}
	if cerr.RolloutRefusal {
		t.Errorf("cerr.RolloutRefusal = true, want false -- a transient repo_settings read failure is NOT a demonstrated policy decision; the four ingress channels must take their retry path, not their permanent-denial path (status=%d message=%q)", cerr.Status, cerr.Message)
	}
}

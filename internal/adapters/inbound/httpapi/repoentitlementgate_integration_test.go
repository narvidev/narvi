//go:build integration

// This file (repoentitlementgate_integration_test.go) proves §31.4's own
// primary, session-creation-time gate: checkRepoEntitlementGate
// (repoentitlementgate.go), exercised through the real, exported
// CreateSessionOnTx entry point -- deliberately in package httpapi (not
// httpapi_test), mirroring rolloutgate_integration_test.go's own precedent
// exactly (checkRepoEntitlementGate itself is unexported, and every test
// here needs to construct CreateSessionOnTx's own arguments directly,
// including its own transaction, the same way that file's tests do).
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

// entitlementTestRepo returns a repo clone URL and its "owner/repo" full
// name, unique per test (derived from t.Name()) -- mirrors
// rolloutTestRepo's own identical precedent (rolloutgate_integration_test.go)
// so concurrent -tags=integration runs never collide on the same
// github_pr_sessions row.
func entitlementTestRepo(t *testing.T) (url, fullName string) {
	t.Helper()
	name := "entitlement-" + t.Name()
	return "https://github.com/acme/" + name + ".git", "acme/" + name
}

// newEntitlementGateTestReq mirrors newRolloutGateTestReq exactly, except
// spawnSource is "web" -- checkRepoEntitlementGate's own doc comment
// requires a NON-github spawn source to actually exercise this gate at
// all (req.SpawnSource == github is unconditionally exempt).
func newEntitlementGateTestReq(spawnSource restdtos.CreateSessionRequestSpawnSource, repoURLs ...string) restdtos.CreateSessionRequest {
	repos := make([]restdtos.CreateSessionRequestReposElem, len(repoURLs))
	for i, u := range repoURLs {
		repos[i] = restdtos.CreateSessionRequestReposElem{Name: "widgets", Url: u}
	}
	return restdtos.CreateSessionRequest{
		SpawnSource: spawnSource,
		Repos:       repos,
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_KnownRepoAdmitted proves the
// positive case: a repo with a real github_pr_sessions row (the SAME
// signal a genuine GitHub PR mention would produce) is admitted.
func TestCreateSessionOnTx_RepoEntitlementGate_KnownRepoAdmitted(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, fullName := entitlementTestRepo(t)
	if err := prSessions.EnsureRow(ctx, fullName, 1); err != nil {
		t.Fatalf("seed github_pr_sessions: %v", err)
	}

	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionOnTx: status=%d message=%q, want success (repo is known)", cerr.Status, cerr.Message)
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid -- CreateSessionOnTx did not actually insert a session")
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_UnknownRepoRefused is the
// mutation anchor for this gate's own core existence: a repo with ZERO
// github_pr_sessions rows at all is refused -- never silently admitted --
// closing the exact clone amplification §31.4 names (an unentitled repo
// reaching sessions.repos, which the sandbox credential helper serves
// verbatim).
//
// Mutation anchor: removing the checkRepoEntitlementGate call from
// CreateSessionOnTx (create.go) makes this test incorrectly PASS admission
// (created.ID becomes valid, cerr is nil) -- flipping this test from
// refused to admitted and failing it. This is the literal "remove the gate
// and confirm a test catches an unentitled repo reaching persistence"
// proof the Step's own brief requires -- see this package's PR description
// for the mutation actually run.
func TestCreateSessionOnTx_RepoEntitlementGate_UnknownRepoRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal for a repo with no github_pr_sessions row")
	}
	if created.ID.Valid {
		t.Error("created.ID is valid, want the zero value -- a refused repo must never reach persistence")
	}
	if cerr.Status != http.StatusForbidden {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusForbidden)
	}
	if !cerr.RepoEntitlementDenied {
		t.Error("cerr.RepoEntitlementDenied = false, want true -- callers must be able to tell this apart from a transient failure structurally")
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_DeniedWritesAuditLogAndSurvivesRollback
// is this Step's own explicit divergence from checkRolloutGate's established
// "no audit_log row for a mere refusal" convention: a repo-entitlement
// denial IS audit-logged, on purpose (a plausible clone-amplification
// attempt, not merely an unfinished rollout) -- and that row must SURVIVE
// even though the surrounding tx (which never reaches tx.Commit) is rolled
// back by this test, exactly as every real CreateSessionOnTx caller does on
// any non-nil *CreateSessionError.
//
// Mutation anchor: changing denyRepoEntitlement's own auditlog.Record call
// from auditLog (pool-backed) to auditLog.WithTx(tx) makes this test fail
// -- the row would roll back along with the (never-committed) session
// insert, and the count below would read 0 instead of 1.
func TestCreateSessionOnTx_RepoEntitlementGate_DeniedWritesAuditLogAndSurvivesRollback(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, fullName := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	// Safety-net defer: an unexpected early Fatal below (e.g. this test
	// itself catching a real regression) must still release this
	// connection back to the pool rather than leaking it for the rest of
	// this package's own shared-pool test binary -- mirrors every other
	// test in this file/package's own "defer rollback" precedent. The
	// explicit tx.Rollback further down (this test's own real assertion
	// subject) runs first on the success path; a second Rollback call here
	// on an already-closed tx is a harmless, ignored pgx.ErrTxClosed.
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal")
	}
	if !cerr.RepoEntitlementDenied {
		t.Fatalf("cerr.RepoEntitlementDenied = false, want true (status=%d message=%q)", cerr.Status, cerr.Message)
	}

	// The caller's own transaction is rolled back here, exactly like every
	// real CreateSessionOnTx caller does on a non-nil *CreateSessionError
	// (create.go's own CreateSessionCore, childsession.go's
	// SpawnChildSession) -- proving the audit row below is NOT part of
	// this same, now-discarded transaction.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("tx.Rollback: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE resource_type = 'repo' AND resource_id = $1 AND action = 'session.repo_entitlement_denied'`,
		fullName,
	).Scan(&count); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit_log rows for denied repo %s = %d, want exactly 1 (and it must survive the caller's own rollback)", fullName, count)
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_GithubSpawnSourceExemptEvenWhenUnknown
// is the mutation anchor for the fork-vs-base correctness fix this gate's
// own doc comment explains at length: req.SpawnSource == github must be
// admitted even for a repo with NO github_pr_sessions row at all --
// removing this exemption would wrongly deny every cross-repo (fork-based)
// PR review session in production, since a fork's own clone URL never
// independently accumulates its own github_pr_sessions history.
//
// Mutation anchor: deleting checkRepoEntitlementGate's own `if
// req.SpawnSource == restdtos.CreateSessionRequestSpawnSourceGithub {
// return nil }` short-circuit makes this test fail (flips from admitted to
// refused).
func TestCreateSessionOnTx_RepoEntitlementGate_GithubSpawnSourceExemptEvenWhenUnknown(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	// A "fork" clone URL that has NEVER had a github_pr_sessions row of
	// its own -- simulating coalesce.go's own WINNER path cloning a
	// cross-repo PR's HEAD (fork) repo, whose claim key (repoFullName,
	// EnsureRow's own argument) is a DIFFERENT, base/upstream repo this
	// request's own req.Repos never even names.
	forkURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceGithub, forkURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionOnTx: status=%d message=%q, want success -- spawnSource=github must be exempt even for a never-known (fork-shaped) repo", cerr.Status, cerr.Message)
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid -- CreateSessionOnTx did not actually insert a session")
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_MultiRepoRequiresAllKnown
// mirrors TestCreateSessionOnTx_RolloutGate_CohortMode_MultiRepoRequiresAllEnrolled's
// own identical shape: one known repo plus one unknown repo is refused,
// even though the FIRST repo alone would have been admitted.
func TestCreateSessionOnTx_RepoEntitlementGate_MultiRepoRequiresAllKnown(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	knownURL, knownFullName := "https://github.com/acme/"+t.Name()+"-known.git", "acme/"+t.Name()+"-known"
	unknownURL := "https://github.com/acme/" + t.Name() + "-unknown.git"
	if err := prSessions.EnsureRow(ctx, knownFullName, 1); err != nil {
		t.Fatalf("seed github_pr_sessions: %v", err)
	}

	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, knownURL, unknownURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- one of the two named repos is not known")
	}
	if !cerr.RepoEntitlementDenied {
		t.Error("cerr.RepoEntitlementDenied = false, want true")
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_CrossHostSpoofRefused mirrors
// TestCreateSessionOnTx_RolloutGate_CohortMode_CrossHostSpoofRefused's own
// identical §32.3 host-verification shape, proving checkRepoEntitlementGate
// shares the SAME resolveTrustedRepoFullName pairing, not a
// second, independently-maintained (and possibly host-agnostic-by-mistake)
// copy of it.
//
// Mutation anchor: same as the rollout gate's own sibling test -- removing
// resolveTrustedRepoFullName's own reposource.CheckRepoHost call would make
// this test incorrectly PASS admission.
func TestCreateSessionOnTx_RepoEntitlementGate_CrossHostSpoofRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	ownerRepo := "acme/" + t.Name() + "-spoof"
	if err := prSessions.EnsureRow(ctx, ownerRepo, 1); err != nil {
		t.Fatalf("seed github_pr_sessions under github.com: %v", err)
	}

	// SAME owner/repo path, but a host that is NOT in
	// ports.SupportedSourceControlHosts() -- reposource.ParseOwnerRepo
	// alone would derive the IDENTICAL "acme/<repo>-spoof" full name from
	// this URL, since it never inspects the host at all.
	spoofedURL := "https://evil.example/" + ownerRepo + ".git"
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, spoofedURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- evil.example must never be treated as the github.com repo it happens to share an owner/repo path with")
	}
	if !cerr.RepoEntitlementDenied {
		t.Error("cerr.RepoEntitlementDenied = false, want true")
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_ReadErrorFailsClosedButNotAsPolicy
// mirrors TestCreateSessionOnTx_RolloutGate_CohortMode_ReadErrorFailsClosedButNotAsPolicy's
// own identical fault-injection idiom (an already-rolled-back tx standing
// in for a genuine github_pr_sessions read failure): fails closed (cerr !=
// nil, never silently admitted) but RepoEntitlementDenied stays false --
// this is an infrastructure blip, not a demonstrated policy denial, so
// callers that branch on RepoEntitlementDenied for terminal-vs-retry must
// take their ordinary retry path here, never their permanent-denial one.
func TestCreateSessionOnTx_RepoEntitlementGate_ReadErrorFailsClosedButNotAsPolicy(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx (fault injection setup): %v", err)
	}
	// tx is now closed -- any query against it (including
	// checkRepoEntitlementGate's own prSessions.WithTx(tx).RepoKnown)
	// returns a genuine error, standing in for a real Postgres outage.

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- a genuine github_pr_sessions read failure must fail CLOSED, never silently admit")
	}
	if cerr.RepoEntitlementDenied {
		t.Errorf("cerr.RepoEntitlementDenied = true, want false -- a transient github_pr_sessions read failure is NOT a demonstrated policy decision (status=%d message=%q)", cerr.Status, cerr.Message)
	}
	if cerr.Status != http.StatusServiceUnavailable {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusServiceUnavailable)
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_NilPRSessionsFailsClosedWithoutPanic
// proves checkRepoEntitlementGate's own defensive nil guard: a caller
// wiring defect (prSessions == nil, exactly the shape this codebase's own
// test suite hit before every Deps/Engine/Notifier constructor was
// updated to thread a real store through) degrades to a clean 503, never a
// nil-pointer panic that would crash the whole process a real session
// creation was about to run in.
//
// Mutation anchor: removing the `if prSessions == nil` guard reintroduces
// the panic this test would then catch as a test failure (a panicking
// test still fails the run, just noisily -- see this file's own PR
// description for the real panic this guard was added to fix).
func TestCreateSessionOnTx_RepoEntitlementGate_NilPRSessionsFailsClosedWithoutPanic(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, nil)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal -- a nil prSessions must fail closed")
	}
	if cerr.RepoEntitlementDenied {
		t.Error("cerr.RepoEntitlementDenied = true, want false -- a nil store is a wiring defect, not a demonstrated policy decision")
	}
	if cerr.Status != http.StatusServiceUnavailable {
		t.Errorf("cerr.Status = %d, want %d", cerr.Status, http.StatusServiceUnavailable)
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_RunsBeforeRolloutGate proves
// this Step's own explicit placement requirement: entitlement is checked
// FIRST -- an unknown-AND-unenrolled repo under cohort mode is refused as
// an entitlement denial, never a rollout one, so a caller branching on
// RepoEntitlementDenied vs RolloutRefusal always sees the more fundamental
// (entitlement) reason first.
func TestCreateSessionOnTx_RepoEntitlementGate_RunsBeforeRolloutGate(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	// Neither known (github_pr_sessions) NOR rollout-enrolled
	// (repo_settings.sessions_enabled) -- both gates would refuse this
	// repo independently; this proves WHICH ONE actually fires first.
	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal")
	}
	if !cerr.RepoEntitlementDenied {
		t.Errorf("cerr.RepoEntitlementDenied = false, want true -- entitlement must be checked before rollout (status=%d message=%q)", cerr.Status, cerr.Message)
	}
	if cerr.RolloutRefusal {
		t.Error("cerr.RolloutRefusal = true, want false -- the rollout gate must never even run once entitlement has already refused")
	}
}

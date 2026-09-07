//go:build integration

// This file (repoentitlementgate_integration_test.go) proves §31.4's own
// primary, session-creation-time gate: ResolveRepoEntitlement
// (repoentitlementgate.go), exercised both directly (the Defect-1 audit
// fix moved essentially all of the interesting I/O-driven repo-name-
// resolution/RepoKnown-read/denial logic into this one resolver, called
// by every CreateSessionOnTx caller before any transaction opens) and
// through the real, exported CreateSessionOnTx/CreateSessionCore entry
// points -- deliberately in package httpapi (not httpapi_test), mirroring
// rolloutgate_integration_test.go's own precedent exactly (every test here
// needs to construct these functions' own arguments directly, including,
// where relevant, an already-open transaction, the same way that file's
// tests do).
package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
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
// spawnSource is caller-chosen (most tests here use "web" --
// ResolveRepoEntitlement's own doc comment requires a NON-github spawn
// source to actually exercise this gate at all, since req.SpawnSource ==
// github is unconditionally exempt).
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
// positive case end-to-end: a repo with a real github_pr_sessions row (the
// SAME signal a genuine GitHub PR mention would produce) is admitted, all
// the way through ResolveRepoEntitlement (resolved before any transaction
// opens, per the Defect-1 audit fix) and CreateSessionOnTx (which only
// ever consults the already-made decision).
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

	entitlement, everr := ResolveRepoEntitlement(ctx, prSessions, auditLog, nilCreator, req)
	if everr != nil {
		t.Fatalf("ResolveRepoEntitlement: status=%d message=%q, want success (repo is known)", everr.Status, everr.Message)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, entitlement)
	if cerr != nil {
		t.Fatalf("CreateSessionOnTx: status=%d message=%q, want success (repo is known)", cerr.Status, cerr.Message)
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid -- CreateSessionOnTx did not actually insert a session")
	}
}

// TestResolveRepoEntitlement_UnknownRepoRefused is the mutation anchor for
// this gate's own core existence, tested at its real locus after the
// Defect-1 audit fix moved it there: a repo with ZERO github_pr_sessions
// rows at all is refused by ResolveRepoEntitlement itself -- never
// silently admitted -- closing the exact clone amplification §31.4 names
// (an unentitled repo reaching sessions.repos, which the sandbox
// credential helper serves verbatim).
//
// Mutation anchor: removing ResolveRepoEntitlement's own
// authz.AuthorizeRepo call (or short-circuiting it to always admit) makes
// this test incorrectly PASS admission (everr becomes nil, entitlement.
// admitted becomes true), flipping this test from refused to admitted and
// failing it.
func TestResolveRepoEntitlement_UnknownRepoRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	entitlement, everr := ResolveRepoEntitlement(ctx, prSessions, auditLog, nilCreator, req)
	if everr == nil {
		t.Fatal("ResolveRepoEntitlement: got nil error, want refusal for a repo with no github_pr_sessions row")
	}
	if everr.Status != http.StatusForbidden {
		t.Errorf("everr.Status = %d, want %d", everr.Status, http.StatusForbidden)
	}
	if !everr.RepoEntitlementDenied {
		t.Error("everr.RepoEntitlementDenied = false, want true -- callers must be able to tell this apart from a transient failure structurally")
	}
	if entitlement.admitted {
		t.Error("entitlement.admitted = true, want false -- a denied decision must never be silently admitting")
	}
}

// TestCreateSessionOnTx_RepoEntitlementGate_UnadmittedDecisionRefused is
// the mutation anchor for CreateSessionOnTx's own defensive backstop
// (create.go): a RepoEntitlementDecision that was never actually produced
// by ResolveRepoEntitlement's own admitting return path -- the zero
// value, exactly what a caller gets if it forgets to call the resolver at
// all -- must refuse, never silently create a session. This is what makes
// RepoEntitlementDecision's own "zero value means denied" property real,
// not just documentation: CreateSessionOnTx has no way to tell "a caller
// deliberately admitted this" apart from "a caller forgot to resolve
// anything" other than by refusing whenever admitted is false.
//
// Mutation anchor: removing CreateSessionOnTx's own `if
// !entitlement.admitted { ... }` guard makes this test incorrectly PASS
// admission (created.ID becomes valid, cerr is nil) despite the zero-value
// decision this test deliberately passes.
func TestCreateSessionOnTx_RepoEntitlementGate_UnadmittedDecisionRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	// A repo that WOULD be admitted by a real ResolveRepoEntitlement call
	// (spawnSource == github is unconditionally exempt) -- proving this
	// refusal comes from CreateSessionOnTx's own consultation of the
	// decision it was handed, never from anything about the request
	// itself.
	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceGithub, repoURL)
	var nilCreator pgtype.UUID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The zero value, deliberately -- exactly what a caller gets from a
	// bare var declaration, or from forgetting to call
	// ResolveRepoEntitlement at all. Never constructed via
	// ResolveRepoEntitlement here.
	var neverResolved RepoEntitlementDecision

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, neverResolved)
	if cerr == nil {
		t.Fatal("CreateSessionOnTx: got nil error, want refusal for an unresolved (zero-value) entitlement decision")
	}
	if created.ID.Valid {
		t.Error("created.ID is valid, want the zero value -- an unresolved entitlement decision must never reach persistence")
	}
	if cerr.Status != http.StatusServiceUnavailable {
		t.Errorf("cerr.Status = %d, want %d -- a never-resolved decision is a caller-wiring defect, not a demonstrated policy denial", cerr.Status, http.StatusServiceUnavailable)
	}
	if cerr.RepoEntitlementDenied {
		t.Error("cerr.RepoEntitlementDenied = true, want false -- this is a wiring defect, not a demonstrated policy decision")
	}
}

// TestResolveRepoEntitlement_Denied_WritesAuditLogRow is this Step's own
// explicit divergence from checkRolloutGate's established "no audit_log
// row for a mere refusal" convention: a repo-entitlement denial IS
// audit-logged, on purpose (a plausible clone-amplification attempt, not
// merely an unfinished rollout). Defect-1 audit fix update: this row is
// now written with NO transaction open AT ALL -- ResolveRepoEntitlement
// takes no tx parameter, so there is no caller rollback left to survive;
// resolution runs strictly before any caller ever opens one. See
// TestCreateSessionCore_DeniedRepo_DoesNotNeedASecondPoolConnection below
// for the proof that mattered in practice (the connection-pool-wedge
// defect this fixed, which this simpler test alone would not catch).
//
// Mutation anchor: removing denyRepoEntitlement's own auditlog.Record call
// (or changing its action/resource_type/resource_id arguments) makes this
// test fail -- the count below would read 0 instead of 1.
func TestResolveRepoEntitlement_Denied_WritesAuditLogRow(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, fullName := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	_, everr := ResolveRepoEntitlement(ctx, prSessions, auditLog, nilCreator, req)
	if everr == nil {
		t.Fatal("ResolveRepoEntitlement: got nil error, want refusal")
	}
	if !everr.RepoEntitlementDenied {
		t.Fatalf("everr.RepoEntitlementDenied = false, want true (status=%d message=%q)", everr.Status, everr.Message)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE resource_type = 'repo' AND resource_id = $1 AND action = 'session.repo_entitlement_denied'`,
		fullName,
	).Scan(&count); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit_log rows for denied repo %s = %d, want exactly 1", fullName, count)
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
// Mutation anchor: deleting ResolveRepoEntitlement's own `if
// req.SpawnSource == restdtos.CreateSessionRequestSpawnSourceGithub {
// return RepoEntitlementDecision{admitted: true}, nil }` short-circuit
// makes this test fail (flips from admitted to refused).
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

	entitlement, everr := ResolveRepoEntitlement(ctx, prSessions, auditLog, nilCreator, req)
	if everr != nil {
		t.Fatalf("ResolveRepoEntitlement: status=%d message=%q, want success -- spawnSource=github must be exempt even for a never-known (fork-shaped) repo", everr.Status, everr.Message)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("pool.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, _, cerr := CreateSessionOnTx(ctx, tx, sessions, turns, environments, auditLog, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, entitlement)
	if cerr != nil {
		t.Fatalf("CreateSessionOnTx: status=%d message=%q, want success -- spawnSource=github must be exempt even for a never-known (fork-shaped) repo", cerr.Status, cerr.Message)
	}
	if !created.ID.Valid {
		t.Fatal("created.ID is not valid -- CreateSessionOnTx did not actually insert a session")
	}
}

// TestResolveRepoEntitlement_MultiRepoRequiresAllKnown mirrors
// TestCreateSessionOnTx_RolloutGate_CohortMode_MultiRepoRequiresAllEnrolled's
// own identical shape: one known repo plus one unknown repo is refused,
// even though the FIRST repo alone would have been admitted.
func TestResolveRepoEntitlement_MultiRepoRequiresAllKnown(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	knownURL, knownFullName := "https://github.com/acme/"+t.Name()+"-known.git", "acme/"+t.Name()+"-known"
	unknownURL := "https://github.com/acme/" + t.Name() + "-unknown.git"
	if err := prSessions.EnsureRow(ctx, knownFullName, 1); err != nil {
		t.Fatalf("seed github_pr_sessions: %v", err)
	}

	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, knownURL, unknownURL)
	var nilCreator pgtype.UUID

	_, everr := ResolveRepoEntitlement(ctx, prSessions, auditLog, nilCreator, req)
	if everr == nil {
		t.Fatal("ResolveRepoEntitlement: got nil error, want refusal -- one of the two named repos is not known")
	}
	if !everr.RepoEntitlementDenied {
		t.Error("everr.RepoEntitlementDenied = false, want true")
	}
}

// TestResolveRepoEntitlement_CrossHostSpoofRefused mirrors
// TestCreateSessionOnTx_RolloutGate_CohortMode_CrossHostSpoofRefused's own
// identical §32.3 host-verification shape, proving ResolveRepoEntitlement
// shares the SAME resolveTrustedRepoFullName pairing, not a second,
// independently-maintained (and possibly host-agnostic-by-mistake) copy
// of it.
//
// Mutation anchor: same as the rollout gate's own sibling test -- removing
// resolveTrustedRepoFullName's own reposource.CheckRepoHost call would
// make this test incorrectly PASS admission.
func TestResolveRepoEntitlement_CrossHostSpoofRefused(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
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

	_, everr := ResolveRepoEntitlement(ctx, prSessions, auditLog, nilCreator, req)
	if everr == nil {
		t.Fatal("ResolveRepoEntitlement: got nil error, want refusal -- evil.example must never be treated as the github.com repo it happens to share an owner/repo path with")
	}
	if !everr.RepoEntitlementDenied {
		t.Error("everr.RepoEntitlementDenied = false, want true")
	}
}

// TestResolveRepoEntitlement_ReadErrorFailsClosedButNotAsPolicy mirrors
// TestCreateSessionOnTx_RolloutGate_CohortMode_ReadErrorFailsClosedButNotAsPolicy's
// own fault-injection idiom, adapted for a resolver that (per the Defect-1
// audit fix) takes no tx parameter at all: a context that is ALREADY
// canceled before ResolveRepoEntitlement is ever called stands in for a
// genuine github_pr_sessions read failure (RepoKnown's own pool-backed
// query surfaces context.Canceled the same way it would surface a real
// Postgres outage -- a non-nil, non-ErrNoRows error). Fails closed (everr
// != nil, never silently admitted) but RepoEntitlementDenied stays false
// -- this is an infrastructure blip, not a demonstrated policy denial, so
// callers that branch on RepoEntitlementDenied for terminal-vs-retry must
// take their ordinary retry path here, never their permanent-denial one.
//
// Mutation anchors: (1) changing ResolveRepoEntitlement's own read-error
// branch to treat it as known=true instead of false would make this test
// incorrectly succeed, flipping it from refused to admitted; (2) setting
// RepoEntitlementDenied: true on this branch (conflating fail-closed with
// terminal) makes this test's own RepoEntitlementDenied assertion fail.
func TestResolveRepoEntitlement_ReadErrorFailsClosedButNotAsPolicy(t *testing.T) {
	pool := newCoreTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	// canceledCtx is now done -- any query issued with it (including
	// ResolveRepoEntitlement's own prSessions.RepoKnown) returns a genuine
	// error, standing in for a real Postgres outage (including a
	// context-canceled or timed-out query, which pgx surfaces the SAME
	// way: a non-nil, non-ErrNoRows error on the query call).

	_, everr := ResolveRepoEntitlement(canceledCtx, prSessions, auditLog, nilCreator, req)
	if everr == nil {
		t.Fatal("ResolveRepoEntitlement: got nil error, want refusal -- a genuine github_pr_sessions read failure must fail CLOSED, never silently admit")
	}
	if everr.RepoEntitlementDenied {
		t.Errorf("everr.RepoEntitlementDenied = true, want false -- a transient github_pr_sessions read failure is NOT a demonstrated policy decision (status=%d message=%q)", everr.Status, everr.Message)
	}
	if everr.Status != http.StatusServiceUnavailable {
		t.Errorf("everr.Status = %d, want %d", everr.Status, http.StatusServiceUnavailable)
	}
}

// TestResolveRepoEntitlement_NilPRSessionsFailsClosedWithoutPanic proves
// ResolveRepoEntitlement's own defensive nil guard: a caller wiring
// defect (prSessions == nil, exactly the shape this codebase's own test
// suite hit before every Deps/Engine/Notifier constructor was updated to
// thread a real store through) degrades to a clean 503, never a
// nil-pointer panic that would crash the whole process a real session
// creation was about to run in.
//
// Mutation anchor: removing the `if prSessions == nil` guard reintroduces
// the panic this test would then catch as a test failure (a panicking
// test still fails the run, just noisily -- see this file's own PR
// description for the real panic this guard was added to fix).
func TestResolveRepoEntitlement_NilPRSessionsFailsClosedWithoutPanic(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)

	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	entitlement, everr := ResolveRepoEntitlement(ctx, nil, auditLog, nilCreator, req)
	if everr == nil {
		t.Fatal("ResolveRepoEntitlement: got nil error, want refusal -- a nil prSessions must fail closed")
	}
	if everr.RepoEntitlementDenied {
		t.Error("everr.RepoEntitlementDenied = true, want false -- a nil store is a wiring defect, not a demonstrated policy decision")
	}
	if everr.Status != http.StatusServiceUnavailable {
		t.Errorf("everr.Status = %d, want %d", everr.Status, http.StatusServiceUnavailable)
	}
	if entitlement.admitted {
		t.Error("entitlement.admitted = true, want false")
	}
}

// TestCreateSessionCore_RepoEntitlementGate_RunsBeforeRolloutGate proves
// this Step's own explicit placement requirement, now expressed at the
// real CreateSessionCore entry point rather than inside CreateSessionOnTx
// itself: the Defect-1 audit fix means entitlement is no longer one of two
// checks CreateSessionOnTx runs in order -- it is resolved by the CALLER
// (CreateSessionCore's own ResolveRepoEntitlement call), strictly before
// CreateSessionOnTx is ever invoked, and therefore strictly before
// checkRolloutGate too (which still runs INSIDE CreateSessionOnTx, on
// tx). An unknown-AND-unenrolled repo under cohort mode is refused as an
// entitlement denial, never a rollout one, and this call never even opens
// the transaction the rollout gate would have run on.
func TestCreateSessionCore_RepoEntitlementGate_RunsBeforeRolloutGate(t *testing.T) {
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

	// Neither known (github_pr_sessions) NOR rollout-enrolled
	// (repo_settings.sessions_enabled) -- both gates would refuse this
	// repo independently; this proves WHICH ONE actually fires first.
	repoURL, _ := entitlementTestRepo(t)
	req := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, repoURL)
	var nilCreator pgtype.UUID

	_, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeCohort, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionCore: got nil error, want refusal")
	}
	if !cerr.RepoEntitlementDenied {
		t.Errorf("cerr.RepoEntitlementDenied = false, want true -- entitlement must be resolved (and refuse) before this call ever reaches the rollout gate (status=%d message=%q)", cerr.Status, cerr.Message)
	}
	if cerr.RolloutRefusal {
		t.Error("cerr.RolloutRefusal = true, want false -- the rollout gate must never even run once entitlement has already refused")
	}
}

// TestCreateSessionCore_DeniedRepo_DoesNotNeedASecondPoolConnection is the
// Defect-1 audit fix's own reproduction/regression proof. Before the fix,
// checkRepoEntitlementGate ran INSIDE CreateSessionOnTx, on the caller's
// own already-open transaction (CreateSessionCore's own pool.Begin,
// above) -- and denyRepoEntitlement's own denial audit write was
// pool-backed, needing a SECOND, simultaneous connection out of the SAME
// pool while the first was still held open by that same tx. Under
// MaxConns:1, CreateSessionCore's own single pool.Begin already exhausted
// the whole pool, so the denial's own audit write could never acquire the
// second connection it needed, and blocked until the caller's own context
// deadline -- with NO audit row ever written (pgxpool.Acquire has no
// acquire timeout of its own).
//
// Mirrors TestCreateSessionCore_ValidationFailure_NeverAcquiresConnection's
// own MaxConns:1 rig (createcore_integration_test.go) closely, but WITHOUT
// that test's own separate "hold an unrelated connection open" step:
// CreateSessionCore's OWN pool.Begin is what exhausts this MaxConns:1 pool
// now that resolution has moved before it -- before the fix, that same
// exhaustion was caused by checkRepoEntitlementGate's own in-tx audit
// write instead, at the SAME single-connection ceiling.
//
// A known-repo control runs FIRST, on the exact same MaxConns:1 pool, to
// prove the pool/rig itself is not what makes the denial (below) slow --
// only the denial path was.
//
// FAILS against the pre-fix code (checkRepoEntitlementGate called from
// inside CreateSessionOnTx, on tx, with a pool-backed denial write): the
// denial call blocks until deniedCtx's own deadline (elapsed >= the bound
// below) and the audit_log row is never written (count == 0).
//
// PASSES after the fix (ResolveRepoEntitlement called by CreateSessionCore
// BEFORE pool.Begin -- repoentitlementgate.go/create.go): both calls
// return promptly (well under the bound) and the audit_log row exists
// (count == 1).
func TestCreateSessionCore_DeniedRepo_DoesNotNeedASecondPoolConnection(t *testing.T) {
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

	knownURL, knownFullName := entitlementTestRepo(t)
	if err := prSessions.EnsureRow(ctx, knownFullName, 1); err != nil {
		t.Fatalf("seed github_pr_sessions: %v", err)
	}
	deniedURL, deniedFullName := "https://github.com/acme/"+t.Name()+"-denied.git", "acme/"+t.Name()+"-denied"

	var nilCreator pgtype.UUID
	const bound = 1 * time.Second

	// Control: a KNOWN repo, on the SAME MaxConns:1 pool, must succeed and
	// return promptly -- proving MaxConns:1 alone is not what makes the
	// denial path (below) slow (the success path never needed a second
	// connection, before or after this fix).
	knownReq := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, knownURL)
	knownCtx, knownCancel := context.WithTimeout(ctx, 3*time.Second)
	defer knownCancel()
	knownStart := time.Now()
	_, cerr := CreateSessionCore(knownCtx, limitedPool, sessions, turns, environments, auditLog, registry, knownReq, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	knownElapsed := time.Since(knownStart)
	if cerr != nil {
		t.Fatalf("CreateSessionCore (known-repo control): status=%d message=%q", cerr.Status, cerr.Message)
	}
	if knownElapsed > bound {
		t.Errorf("CreateSessionCore (known-repo control) took %s on a MaxConns:1 pool, want well under %s", knownElapsed, bound)
	}

	// The actual proof: a DENIED repo, on the SAME MaxConns:1 pool. Before
	// the fix, CreateSessionCore's own pool.Begin (for the doomed
	// transaction the denial's own in-tx gate ran on) already exhausted
	// this pool's one connection, so the denial's own audit write could
	// never acquire the second connection it needed and blocked until
	// deniedCtx's own deadline below.
	deniedReq := newEntitlementGateTestReq(restdtos.CreateSessionRequestSpawnSourceWeb, deniedURL)
	deniedCtx, deniedCancel := context.WithTimeout(ctx, 3*time.Second)
	defer deniedCancel()
	deniedStart := time.Now()
	_, cerr = CreateSessionCore(deniedCtx, limitedPool, sessions, turns, environments, auditLog, registry, deniedReq, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	deniedElapsed := time.Since(deniedStart)

	if cerr == nil {
		t.Fatal("CreateSessionCore (denied repo): got nil error, want refusal for an unknown repo")
	}
	if !cerr.RepoEntitlementDenied {
		t.Errorf("cerr.RepoEntitlementDenied = false, want true (status=%d message=%q)", cerr.Status, cerr.Message)
	}
	if deniedElapsed > bound {
		t.Errorf("CreateSessionCore (denied repo) took %s to refuse on a MaxConns:1 pool, want well under %s (a known-repo control on the same pool took %s) -- it is blocking on a SECOND pool connection for its own denial audit write", deniedElapsed, bound, knownElapsed)
	}

	var count int
	if err := limitedPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE resource_type = 'repo' AND resource_id = $1 AND action = 'session.repo_entitlement_denied'`,
		deniedFullName,
	).Scan(&count); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit_log rows for denied repo %s = %d, want exactly 1 -- a denial must be loud (audited), never silent, even under pool pressure", deniedFullName, count)
	}
}

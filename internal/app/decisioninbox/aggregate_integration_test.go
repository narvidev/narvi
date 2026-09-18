//go:build integration

// Integration test for Build ("decision inbox: read model +
// API", §16) against a REAL Postgres instance -- gated behind the
// "integration" build tag, mirroring internal/app/actorauthz's own
// testcontainers-Postgres-plus-embedded-migrations convention exactly
// (each DB-touching package builds its own copy of newTestPool rather
// than sharing one across package boundaries). Run via `make
// test-integration`.
package decisioninbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	decisioninboxdomain "github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/migrations"
)

// testEligibleBaseRef/testEligibleBaseSHA (§21.1's amendment) are this
// package's own shared "this PR's base has not moved" fixture pair --
// every ports.OpenPR fixture in this file and revalidate_integration_
// test.go that claims to be "otherwise fully eligible" sets BOTH of
// these on the LIVE PR AND passes them to seedAutoApprovedVerdict below,
// so the verdict's own recorded context matches the PR's current one
// exactly, the precondition internal/domain/autoapproval.ComputeEligible
// now requires before CI/Shippable/diff-size/sensitive-path are ever
// even reached. A test that means to exercise base drift specifically
// (TestComputeEligible's own "a PR whose base changed while its head did
// not must lose eligibility" case lives in eligibility_test.go, a pure
// unit test -- this package's own integration tests are not where that
// guard is pinned) sets a DIFFERENT base on the live PR after seeding,
// exactly like every other negative subtest here perturbs one fact off
// of the eligible baseline.
const (
	testEligibleBaseRef = "main"
	testEligibleBaseSHA = "sha-eligible-base"
)

// newTestPool spins up a throwaway Postgres container, runs every embedded
// migration up, and returns a ready *pgxpool.Pool -- a duplicate of
// actorauthz's own newTestPool, necessarily so (see that file's own doc
// comment for this codebase's established per-package precedent).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

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
		t.Fatalf("start postgres container: tcpostgres.Run did not return within %s", containerStartWatchdog)
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

// fakeDecisionInboxSourceControl is a minimal test-only ports.
// SourceControl -- narrowed to exactly the two methods Build's own
// SCMCache calls (ListOpenPRsForUser, ResolveCodeOwners); every other
// method returns a plain "not implemented" error, mirroring internal/app/
// outboxworker's own fakeSentinelAutoFixSourceControl precedent.
type fakeDecisionInboxSourceControl struct {
	openPRsByExternalID map[string][]ports.OpenPR
	// openPRsTruncated/openPRsErr (this
	// fake previously hardcoded truncated=false and never errored, so
	// Result.SCMFetchFailed=true had no test coverage at all) let a test
	// drive ListOpenPRsForUser's own two degraded outcomes.
	openPRsTruncated bool
	openPRsErr       error

	// codeOwnersCalls captures every ResolveCodeOwners call this fake
	// receives, in order -- "reverting
	// Ref: pr.BaseRef -> pr.HeadSHA... passes everything" because nothing
	// previously inspected what spec this fake was actually called with.
	codeOwnersCalls []ports.ResolveCodeOwnersSpec

	// resolveBranchSHA/resolveBranchSHAErr (finding F1 (§21.1's amendment)) back
	// ResolveBranchSHA below -- revalidateCore now resolves the base
	// branch's LIVE tip independently, rather than trusting
	// ports.OpenPR.BaseSHA (GitHub's own possibly-stale
	// `pull_request.base.sha` snapshot, that field's own doc comment).
	// Both zero (every caller that never sets them) falls back to
	// scanning this fake's own already-seeded PRs (openPRsByExternalID)
	// for one reporting spec.Branch as its BaseRef, returning THAT PR's
	// own BaseSHA -- so every EXISTING fixture in this file, which
	// already sets BaseRef/BaseSHA consistently, gets a live resolution
	// "for free" with no per-test literal changes needed. A test proving
	// the F1 hazard
	// (revalidate_integration_test.go) overrides resolveBranchSHA
	// explicitly to simulate the base branch's real tip moving while
	// GitHub's own cached base.sha field (and the base ref name) both
	// stay exactly as they were.
	resolveBranchSHA    string
	resolveBranchSHAErr error
	// resolveBranchSHACalls (F2/F4) records every
	// ResolveBranchSHA call this fake receives, in order -- mirrors
	// isAncestorCalls' own identical "record what was actually called"
	// convention immediately below. Needed once Result.SCMFetchFailed
	// could ALSO turn true directly off ports.OpenPR.CIConclusionDegraded
	// (independent of whether this live call ever ran): a test proving
	// the probe still short-circuits BEFORE this call, for a PR the probe
	// already refuses on CIConclusionDegraded alone, can no longer rely on
	// SCMFetchFailed's own value to prove that (it is now true either
	// way) and needs this call count instead.
	resolveBranchSHACalls []ports.ResolveBranchSHASpec
	// resolveBranchSHAByBranch (round-11 finding A3) lets a test configure
	// a DISTINCT resolved sha per queried branch -- checked BEFORE both
	// the plain resolveBranchSHA override and the scan-fallback above,
	// specifically for a test that needs the IMMEDIATE base and a
	// GitHub-native stack's own ancestor ref to resolve to two
	// INDEPENDENTLY-controlled values in the same call (a fast-forward on
	// the ancestor chain alone, the base left genuinely unchanged) --
	// resolveBranchSHA's own single shared value cannot express that,
	// since it answers every branch identically. Every EXISTING test that
	// never populates this map is unaffected.
	resolveBranchSHAByBranch map[string]string

	// isAncestorResult/isAncestorErr/isAncestorCalls (D3, second
	// adversarial-review round) back IsAncestor below -- the fast-forward
	// tolerance the base-freshness gate now applies when a verdict's own
	// recorded base sha differs from the live tip but the base REF is
	// unchanged. Zero value (false, nil) reproduces this codebase's own
	// PRE-D3 behavior exactly (any base sha mismatch refuses, regardless
	// of ancestry) -- every EXISTING test in this file that never sets
	// this field is therefore unaffected by D3's own addition. A test
	// proving the D3 fix sets isAncestorResult = true to simulate an
	// unrelated, purely-forward merge to the base branch.
	isAncestorResult bool
	isAncestorErr    error
	isAncestorCalls  []ports.IsAncestorSpec
}

var _ ports.SourceControl = (*fakeDecisionInboxSourceControl)(nil)

// IsAncestor (D3, second adversarial-review round) reports
// isAncestorResult/isAncestorErr, recording every call it receives so a
// test can assert WHICH (ancestor, descendant) pair was actually
// compared -- mirrors this fake's own codeOwnersCalls precedent.
//
// Honors ctx (E7, third adversarial-review round): this previously
// ignored ctx entirely (the blank identifier in its own signature).
// ctx.Err() is checked FIRST, before either configured return: a caller
// context.WithTimeout'd with a non-positive duration is already expired
// the instant it is constructed (context's own documented behavior, no
// sleep/wall-clock dependency needed to observe it), so THIS MECHANISM
// makes a zero/missing timeout on the real call site detectable --
// something no version of this fake could do before E7.
//
// That capability sat unused (G5, fourth adversarial-review round): E7's
// own comment here previously claimed a zero/unset/never-wired timeout
// "could not make ANY test in this package fail", stated as though this
// fake already delivered that guarantee -- but no test anywhere in this
// package ever set platform.Timeouts.DecisionInboxIsAncestorTimeout to a
// non-positive value and asserted the resulting failure, so nothing
// actually exercised the branch this doc comment described. A mechanism
// nothing calls pins nothing.
// TestRevalidateForMerge_ZeroIsAncestorTimeout_TreatedAsAlreadyExpired
// (revalidate_integration_test.go) is the first test that does.
func (f *fakeDecisionInboxSourceControl) IsAncestor(ctx context.Context, spec ports.IsAncestorSpec) (bool, error) {
	f.isAncestorCalls = append(f.isAncestorCalls, spec)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if f.isAncestorErr != nil {
		return false, f.isAncestorErr
	}
	return f.isAncestorResult, nil
}

func (f *fakeDecisionInboxSourceControl) ListOpenPRsForUser(_ context.Context, spec ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	if f.openPRsErr != nil {
		return nil, false, f.openPRsErr
	}
	return f.openPRsByExternalID[spec.GitHubExternalID], f.openPRsTruncated, nil
}

// ResolveCodeOwners always reports no match -- every existing test's own
// scenario exercises Direct/RequestedReviewer provenance, not a real
// CODEOWNERS match (already covered exhaustively at the adapter layer,
// resolvecodeowners_test.go); this fake's own job here is only ever to
// record WHAT it was called with (codeOwnersCalls above).
func (f *fakeDecisionInboxSourceControl) ResolveCodeOwners(_ context.Context, spec ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	f.codeOwnersCalls = append(f.codeOwnersCalls, spec)
	return nil, nil
}

func (f *fakeDecisionInboxSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("fakeDecisionInboxSourceControl: CreatePR not implemented")
}

// Honors ctx (G4, fourth adversarial-review round -- mirroring
// IsAncestor's own identical E7 fix above, for the identical reason):
// this previously ignored ctx entirely, so a caller that failed to wrap
// this call in a timeout (revalidateCore's own ResolveBranchSHA call was
// exactly this, before G4) could not be caught by any test in this
// package. ctx.Err() is checked FIRST, before any configured/seeded
// return, for the identical "already-expired context is deterministic,
// no sleep needed" reason IsAncestor's own doc comment gives.
func (f *fakeDecisionInboxSourceControl) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.resolveBranchSHACalls = append(f.resolveBranchSHACalls, spec)
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if f.resolveBranchSHAErr != nil {
		return "", "", f.resolveBranchSHAErr
	}
	if sha, ok := f.resolveBranchSHAByBranch[spec.Branch]; ok {
		return sha, spec.Branch, nil
	}
	if f.resolveBranchSHA != "" {
		return f.resolveBranchSHA, spec.Branch, nil
	}
	for _, prs := range f.openPRsByExternalID {
		for _, pr := range prs {
			if pr.BaseRef == spec.Branch {
				return pr.BaseSHA, spec.Branch, nil
			}
		}
	}
	return "", "", fmt.Errorf("fakeDecisionInboxSourceControl: ResolveBranchSHA: no seeded PR reports base ref %q", spec.Branch)
}
func (f *fakeDecisionInboxSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("fakeDecisionInboxSourceControl: ResolveContractsFingerprint not implemented")
}
func (f *fakeDecisionInboxSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("fakeDecisionInboxSourceControl: CheckRepoAccess not implemented")
}
func (f *fakeDecisionInboxSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("fakeDecisionInboxSourceControl: GetFileContent not implemented")
}
func (f *fakeDecisionInboxSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("fakeDecisionInboxSourceControl: UpdateFileContent not implemented")
}
func (f *fakeDecisionInboxSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("fakeDecisionInboxSourceControl: RegisterPRStack not implemented")
}
func (f *fakeDecisionInboxSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("fakeDecisionInboxSourceControl: ListMergedBetween not implemented")
}
func (f *fakeDecisionInboxSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("fakeDecisionInboxSourceControl: CreateBranch not implemented")
}
func (f *fakeDecisionInboxSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("fakeDecisionInboxSourceControl: MergePR not implemented")
}

// GetOpenPR (§21.2 stage 2) backs RevalidateForAutoMerge, which no test in
// this package (decisioninbox) exercises -- that function's own coverage
// lives in internal/app/automerge's worker_integration_test.go, against
// ITS OWN fakeAutoMergeSourceControl, this fake's sibling. A prior version
// of this method carried a seedable getOpenPRByKey map whose doc named a
// TestRevalidateForAutoMerge this package has never had; removed rather
// than given a real user, mirroring the plain "not implemented" every
// other method on this fake already uses for a method Build's own
// SCMCache never calls (this type's own doc comment, above).
func (f *fakeDecisionInboxSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("fakeDecisionInboxSourceControl: GetOpenPR not implemented")
}
func (f *fakeDecisionInboxSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("fakeDecisionInboxSourceControl: GetPRBody not implemented")
}
func (f *fakeDecisionInboxSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("fakeDecisionInboxSourceControl: UpdatePRBody not implemented")
}

func strPtr(s string) *string { return &s }

// seedAutoApprovedVerdict inserts a review_verdicts row (§21.1)
// whose Shippable is 'auto' and whose head_sha matches headSHA exactly --
// the ONE fact internal/domain/autoapproval.ComputeEligible now requires
// before ANY PR can classify ready_to_merge (a missing verdict is
// unconditionally ineligible). Every existing "this PR must land
// ready_to_merge" / "X is the ONLY thing keeping this PR out of
// ready_to_merge" fixture in this file calls this so that claim stays
// genuinely single-variable -- omitting it would make every such test
// pass for the WRONG reason (no verdict on record) regardless of whether
// the ACTUAL behavior under test still works, exactly the double-gated-
// fixture trap this codebase's own review rounds have repeatedly found.
func seedAutoApprovedVerdict(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32, headSHA string) {
	t.Helper()
	store := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	// §30.8: decisioninbox's own classification deliberately reads the
	// SAME unfiltered GetLatest every other internal, operator-facing
	// caller uses (migrations/000105's own doc comment) -- this fixture
	// does not need to promote repoFullName to live for that read to see
	// it, unlike internal/app/automerge's own seedEligiblePR.
	// (§21.1's amendment): base_ref/base_sha/policy_version are
	// stamped to this package's own shared testEligibleBaseRef/
	// testEligibleBaseSHA/autoapproval.CurrentPolicyVersion -- matching
	// every "otherwise fully eligible" ports.OpenPR fixture in this file
	// and revalidate_integration_test.go, so ComputeEligible's own new
	// context-freshness check passes for the SAME reason head_sha above
	// already has to match. AncestorChain stays nil: none of this
	// package's fixtures are GitHub-native-stack PRs.
	verdictContext := reviewverdict.Context{
		BaseRef:       testEligibleBaseRef,
		BaseSHA:       testEligibleBaseSHA,
		PolicyVersion: autoapproval.CurrentPolicyVersion,
	}
	if _, err := appreviewverdict.Insert(ctx, store, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed auto-approved review_verdicts row for %s#%d: %v", repoFullName, prNumber, err)
	}
}

// TestBuild_FullScenario exercises every kind's own real inclusion
// criterion end to end against a real Postgres instance: ready_to_merge
// (platform-authored + eligible), needs_review (the same PR shape but
// NOT platform-authored), the §17 structural exclusion (a PR that is a
// registered sentinel-fix follow-up never appears regardless of
// assignment), a draft PR excluded outright, a plan awaiting approval,
// and the three needs_attention sources -- gated to ADMIN ONLY, proven by
// building the SAME inbox twice (member, then admin) from the SAME
// fixtures.
func TestBuild_FullScenario(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	participants := narvipg.NewParticipantStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	automations := narvipg.NewAutomationStore(pool)
	outbox := narvipg.NewOutboxStore(pool, false)
	reviewFindings := narvipg.NewReviewFindingStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "decisioninbox-actor@example.com", DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}

	const actorGitHubExternalID = "1001"
	tokenKey := []byte("01234567890123456789012345678901") // exactly 32 bytes
	encryptedToken, err := platform.EncryptToken(tokenKey, []byte("fake-gh-token"))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: actorGitHubExternalID,
		EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encryptedToken,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	// PR #10: platform-authored (an artifacts row records it), low-risk,
	// CI green, directly assigned to the actor -- must land ready_to_merge.
	platformSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://github.com/acme/widgets/pull/10", Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}
	// (§21.1/§21.2): PR #10 needs a Shippable=auto review_verdicts
	// row, at its own exact head sha, before the REAL eligibility engine
	// will ever classify it ready_to_merge -- see seedAutoApprovedVerdict's
	// own doc comment.
	seedAutoApprovedVerdict(ctx, t, pool, "acme/widgets", 10, "sha10")

	// PR #11: the SAME shape, but NOT platform-authored (no artifacts
	// row) -- must land needs_review, never ready_to_merge, despite being
	// low-risk/CI-green.

	// PR #12: a registered sentinel-auto-fix follow-up (§17) -- must
	// never appear at all, regardless of assignment.
	originSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	fix, err := sentinelFixes.WithTx(tx).Claim(ctx, "acme/widgets", 999, originSession.ID, "origin-branch")
	if err != nil {
		t.Fatalf("claim sentinel fix: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	if _, err := sentinelFixes.UpdateOpened(ctx, fix.ID, 12); err != nil {
		t.Fatalf("update sentinel fix opened: %v", err)
	}

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 10, Title: "scheduler: exponential backoff",
					HTMLURL: "https://github.com/acme/widgets/pull/10", HeadSHA: "sha10",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk"},
					CreatedAt:    time.Now().Add(-2 * time.Hour),
				},
				{
					Owner: "acme", Repo: "widgets", Number: 11, Title: "bump pgx to v5.6",
					HTMLURL: "https://github.com/acme/widgets/pull/11", HeadSHA: "sha11",
					RequestedReviewers: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion:       ports.CIConclusionSuccess,
					Labels:             []string{"review:low-risk"},
					CreatedAt:          time.Now().Add(-24 * time.Hour),
				},
				{
					Owner: "acme", Repo: "widgets", Number: 12, Title: "test: add missing coverage",
					HTMLURL: "https://github.com/acme/widgets/pull/12", HeadSHA: "sha12",
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk"},
					CreatedAt:    time.Now(),
				},
				{
					Owner: "acme", Repo: "widgets", Number: 13, Title: "wip: still drafting",
					HTMLURL: "https://github.com/acme/widgets/pull/13", HeadSHA: "sha13", Draft: true,
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CreatedAt: time.Now(),
				},
			},
		},
	}
	scmCache := decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts())

	// A plan-mode plan on a session the actor created.
	planSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{Title: strPtr("Migrate secrets to per-automation scope"), SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create plan session: %v", err)
	}
	turn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: planSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: planSession.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// needs_attention fixtures: a failed session, an auto-paused
	// automation, a dead-lettered outbox delivery.
	failedSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{Title: strPtr("Add e2e coverage for plan mode"), SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create failed session: %v", err)
	}
	failReason := sqlcgen.SessionFailureReasonTimeout
	if _, err := sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: failedSession.ID, Status: sqlcgen.SessionStatusFailed, FailureReason: &failReason}); err != nil {
		t.Fatalf("update session status: %v", err)
	}

	pausedAutomation, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "Weekly changelog draft", Repos: []byte("[]"), CreatedBy: actor.ID,
		TriggerType: sqlcgen.AutomationTriggerTypeManual, TriggerConfig: []byte("{}"), EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create automation: %v", err)
	}
	// Raw SQL for the fixture's own consecutive_failures/status -- no
	// store method sets both directly outside the real strike-evaluation
	// flow (internal/domain/automation.EvaluateFailureStrike), which this
	// test is not exercising.
	if _, err := pool.Exec(ctx, `UPDATE automations SET status = 'paused', consecutive_failures = 3 WHERE id = $1`, pausedAutomation.ID); err != nil {
		t.Fatalf("mark automation auto-paused: %v", err)
	}

	deadOutboxEntry, err := outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{Kind: "test_notification", Payload: []byte("{}")})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if _, err := outbox.MarkDeadLetter(ctx, deadOutboxEntry.ID, "notifier: permanent failure"); err != nil {
		t.Fatalf("mark outbox dead letter: %v", err)
	}

	deps := decisioninbox.Deps{
		Plans: plans, Sessions: sessions, Participants: participants, Automations: automations,
		Outbox: outbox, ReviewFindings: reviewFindings, SentinelFixes: sentinelFixes, Artifacts: artifacts,
		Identities: identities, SCMCache: scmCache, TokenEncryptionKey: tokenKey, Timeouts: platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}
	now := time.Now()

	// --- As a MEMBER: PR/plan items present, needs_attention absent. ---
	memberResult, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, now)
	if err != nil {
		t.Fatalf("Build() (member) error = %v", err)
	}
	if memberResult.SCMAsOf == nil {
		t.Error("SCMAsOf = nil, want a real fetch instant (actor has a linked GitHub identity)")
	}

	pr10 := findItemByPR(memberResult.Items, 10)
	if pr10 == nil {
		t.Fatal("PR #10 missing from the inbox entirely")
	}
	if pr10.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Errorf("PR #10 Kind = %s, want ready_to_merge (platform-authored, low-risk, CI green, eligible)", pr10.Kind)
	}
	if pr10.Provenance == nil || pr10.Provenance.Kind != decisioninboxdomain.ProvenanceDirect {
		t.Errorf("PR #10 Provenance = %+v, want ProvenanceDirect", pr10.Provenance)
	}

	pr11 := findItemByPR(memberResult.Items, 11)
	if pr11 == nil {
		t.Fatal("PR #11 missing from the inbox entirely")
	}
	if pr11.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("PR #11 Kind = %s, want needs_review (NOT platform-authored, so never ready_to_merge)", pr11.Kind)
	}
	if pr11.Provenance == nil || pr11.Provenance.Kind != decisioninboxdomain.ProvenanceRequestedReviewer {
		t.Errorf("PR #11 Provenance = %+v, want ProvenanceRequestedReviewer", pr11.Provenance)
	}

	if item := findItemByPR(memberResult.Items, 12); item != nil {
		t.Errorf("PR #12 present in the inbox (%+v), want structurally excluded as a sentinel-fix follow-up (§17)", item)
	}
	if item := findItemByPR(memberResult.Items, 13); item != nil {
		t.Errorf("PR #13 (draft) present in the inbox (%+v), want excluded", item)
	}

	foundPlan := false
	for _, it := range memberResult.Items {
		if it.Kind == decisioninboxdomain.KindAwaitingApproval && it.SessionID == planSession.ID.String() {
			foundPlan = true
		}
	}
	if !foundPlan {
		t.Error("the actor's own awaiting_approval plan is missing from the inbox")
	}

	for _, it := range memberResult.Items {
		if it.Kind == decisioninboxdomain.KindNeedsAttention {
			t.Errorf("needs_attention item present for a MEMBER actor (%+v), want admin-only", it)
		}
	}

	// --- As an ADMIN: the SAME fixtures now also surface needs_attention. ---
	adminResult, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleAdmin, now)
	if err != nil {
		t.Fatalf("Build() (admin) error = %v", err)
	}

	var foundFailedSession, foundPausedAutomation, foundDeadLetter bool
	for _, it := range adminResult.Items {
		if it.Kind != decisioninboxdomain.KindNeedsAttention {
			continue
		}
		switch {
		case it.SessionID == failedSession.ID.String():
			foundFailedSession = true
			if it.FailureReason != string(sqlcgen.SessionFailureReasonTimeout) {
				t.Errorf("failed session FailureReason = %q, want %q", it.FailureReason, sqlcgen.SessionFailureReasonTimeout)
			}
		case it.AutomationID == pausedAutomation.ID.String():
			foundPausedAutomation = true
		case it.OutboxID == deadOutboxEntry.ID.String():
			foundDeadLetter = true
		}
	}
	if !foundFailedSession {
		t.Error("failed session missing from the admin inbox's needs_attention section")
	}
	if !foundPausedAutomation {
		t.Error("auto-paused automation missing from the admin inbox's needs_attention section")
	}
	if !foundDeadLetter {
		t.Error("dead-lettered outbox entry missing from the admin inbox's needs_attention section")
	}

	// Ranking sanity: ready_to_merge sorts before needs_review, which
	// sorts before awaiting_approval, which sorts before needs_attention
	// (§16.1's own "by decision cost then age").
	lastCost := -1
	for _, it := range adminResult.Items {
		cost := decisioninboxdomain.DecisionCost(it.Kind)
		if cost < lastCost {
			t.Errorf("items not sorted by decision cost: %s (cost %d) appeared after cost %d", it.Kind, cost, lastCost)
		}
		lastCost = cost
	}
}

func findItemByPR(items []decisioninbox.Item, number int) *decisioninbox.Item {
	for i := range items {
		if items[i].PRNumber == number {
			return &items[i]
		}
	}
	return nil
}

// TestBuild_ReviewSessionIDAndReleaseCut proves the two gaps closed on top
// of TestBuild_FullScenario's own baseline: a PR-shaped row now carries a
// real review-session id when (and ONLY when) Narvi has actually been
// mentioned on that exact PR, never a session id belonging to a
// DIFFERENT PR (github_pr_sessions_integration coverage already pins
// this at the store layer; this proves the SAME property survives
// Build's own wiring); and a release cut (a persisted §15.2 manifest
// check) now renders as its own distinct row, classified needs_review
// EVEN when every ordinary ready_to_merge criterion is also met.
func TestBuild_ReviewSessionIDAndReleaseCut(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)
	githubPRSessions := narvipg.NewGitHubPRSessionStore(pool)
	releaseManifestChecks := narvipg.NewReleaseManifestCheckStore(pool)

	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "review-session-actor@example.com", DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	const actorGitHubExternalID = "2001"
	tokenKey := []byte("01234567890123456789012345678901") // exactly 32 bytes
	encryptedToken, err := platform.EncryptToken(tokenKey, []byte("fake-gh-token"))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: actorGitHubExternalID,
		EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encryptedToken,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	// claimReviewSession mirrors coalesce.go's own atomic
	// EnsureRow+LockForUpdate+SetSessionID claim sequence -- the ONLY
	// legitimate way a github_pr_sessions row is ever created in this
	// codebase, reused here rather than a raw INSERT so this fixture
	// stays faithful to the real write path.
	claimReviewSession := func(t *testing.T, repoFullName string, prNumber int32) pgtype.UUID {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin claim tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		txStore := githubPRSessions.WithTx(tx)
		if err := txStore.EnsureRow(ctx, repoFullName, prNumber); err != nil {
			t.Fatalf("EnsureRow: %v", err)
		}
		if _, err := txStore.LockForUpdate(ctx, repoFullName, prNumber); err != nil {
			t.Fatalf("LockForUpdate: %v", err)
		}
		created, err := sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
		if err != nil {
			t.Fatalf("create review session: %v", err)
		}
		if err := txStore.SetSessionID(ctx, repoFullName, prNumber, created.ID); err != nil {
			t.Fatalf("SetSessionID: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit claim tx: %v", err)
		}
		return created.ID
	}

	const repo = "acme/rockets"

	// PR #30: mentioned (a real github_pr_sessions claim), NOT platform-
	// authored, NOT a release cut -- the ordinary "open review" case.
	session30 := claimReviewSession(t, repo, 30)

	// PR #31: mentioned by a DIFFERENT claim than #30's -- exists purely
	// to prove #30 and #31 never cross-resolve each other's session id.
	session31 := claimReviewSession(t, repo, 31)
	if session30 == session31 {
		t.Fatalf("fixture bug: session30 and session31 are the same session (%+v)", session30)
	}

	// PR #32: never mentioned at all -- "open review" must stay external.

	// PR #33: a release cut (§15) that ALSO meets every ordinary
	// ready_to_merge criterion (platform-authored, low-risk, CI green,
	// an auto-approved verdict on record) -- proves release-cut
	// classification wins regardless.
	platformSession33, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session for PR #33: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession33.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://github.com/acme/rockets/pull/33", Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact for PR #33: %v", err)
	}
	seedAutoApprovedVerdict(ctx, t, pool, repo, 33, "sha33")
	session33 := claimReviewSession(t, repo, 33)
	findingsJSON33, err := json.Marshal([]map[string]any{
		{"kind": "admin_override", "prNumber": 100, "prTitle": "hotfix", "detail": "merged via admin override"},
		{"kind": "red_at_merge", "prNumber": 101, "prTitle": "flaky", "detail": "CI was red at merge sha"},
	})
	if err != nil {
		t.Fatalf("marshal findings fixture: %v", err)
	}
	if _, err := releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID: session33, RepoFullName: repo, PrNumber: 33, BaseRef: "main", HeadRef: "release/2026.09.01",
		ConstituentPrCount: 2, CoveragePartial: false, AggregateReviewTriggered: true,
		AggregateReviewTriggerReasons: []byte(`["3+ constituent PRs touch overlapping paths"]`),
		Findings:                      findingsJSON33,
		MergedPrs:                     []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert release manifest check for PR #33: %v", err)
	}

	// PR #34: the SAME ready-to-merge-eligible shape as #33, but NEVER
	// checked as a release cut -- the contrasting control: proves this
	// fixture's own #33 result is due to the release-cut signal
	// specifically, not some other unaccounted-for difference.
	platformSession34, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session for PR #34: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession34.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://github.com/acme/rockets/pull/34", Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact for PR #34: %v", err)
	}
	seedAutoApprovedVerdict(ctx, t, pool, repo, 34, "sha34")

	// PR #35: a release cut whose constituent-PR listing was TRUNCATED
	// (coverage_partial), with ZERO mechanical findings -- the confident-
	// zero case. Its findings count is honestly 0, but 0 over an
	// incomplete set is not the same claim as 0 over a complete one, and
	// nothing downstream may render the two identically.
	platformSession35, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session for PR #35: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession35.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://github.com/acme/rockets/pull/35", Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact for PR #35: %v", err)
	}
	session35 := claimReviewSession(t, repo, 35)
	if _, err := releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID: session35, RepoFullName: repo, PrNumber: 35, BaseRef: "main", HeadRef: "release/2026.09.02",
		ConstituentPrCount: 250, CoveragePartial: true, AggregateReviewTriggered: false,
		AggregateReviewTriggerReasons: []byte(`[]`),
		Findings:                      []byte(`[]`),
		MergedPrs:                     []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert release manifest check for PR #35: %v", err)
	}

	// PR #36: a release cut whose persisted findings blob is CORRUPT --
	// valid jsonb, but an object where the reader expects an array, so
	// json.Unmarshal into []json.RawMessage fails. coverage_partial is
	// deliberately false on this row: the truncation flag is NOT what
	// makes this one partial. A count that could not be decoded is not a
	// count of zero, and the row must not read as a clean release just
	// because the decode failure happened to be logged rather than
	// surfaced.
	platformSession36, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session for PR #36: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession36.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://github.com/acme/rockets/pull/36", Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact for PR #36: %v", err)
	}
	session36 := claimReviewSession(t, repo, 36)
	if _, err := releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID: session36, RepoFullName: repo, PrNumber: 36, BaseRef: "main", HeadRef: "release/2026.09.03",
		ConstituentPrCount: 4, CoveragePartial: false, AggregateReviewTriggered: false,
		AggregateReviewTriggerReasons: []byte(`[]`),
		Findings:                      []byte(`{"not":"an array"}`),
		MergedPrs:                     []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert release manifest check for PR #36: %v", err)
	}

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{Owner: "acme", Repo: "rockets", Number: 30, Title: "PR 30", HTMLURL: "https://github.com/acme/rockets/pull/30", HeadSHA: "sha30",
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, CreatedAt: time.Now()},
				{Owner: "acme", Repo: "rockets", Number: 31, Title: "PR 31", HTMLURL: "https://github.com/acme/rockets/pull/31", HeadSHA: "sha31",
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, CreatedAt: time.Now()},
				{Owner: "acme", Repo: "rockets", Number: 32, Title: "PR 32", HTMLURL: "https://github.com/acme/rockets/pull/32", HeadSHA: "sha32",
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, CreatedAt: time.Now()},
				{Owner: "acme", Repo: "rockets", Number: 33, Title: "release/2026.09.01 -- 2 PRs", HTMLURL: "https://github.com/acme/rockets/pull/33", HeadSHA: "sha33",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now()},
				{Owner: "acme", Repo: "rockets", Number: 34, Title: "PR 34", HTMLURL: "https://github.com/acme/rockets/pull/34", HeadSHA: "sha34",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now()},
				{Owner: "acme", Repo: "rockets", Number: 35, Title: "release/2026.09.02 -- truncated scan", HTMLURL: "https://github.com/acme/rockets/pull/35", HeadSHA: "sha35",
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now()},
				{Owner: "acme", Repo: "rockets", Number: 36, Title: "release/2026.09.03 -- corrupt findings blob", HTMLURL: "https://github.com/acme/rockets/pull/36", HeadSHA: "sha36",
					Assignees: []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}}, CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now()},
			},
		},
	}
	scmCache := decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts())

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: sessions, Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: identities, GitHubPRSessions: githubPRSessions, ReleaseManifestChecks: releaseManifestChecks,
		SCMCache: scmCache, TokenEncryptionKey: tokenKey, Timeouts: platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	pr30 := findItemByPR(result.Items, 30)
	if pr30 == nil {
		t.Fatal("PR #30 missing from the inbox entirely")
	}
	if pr30.SessionID != session30.String() {
		t.Errorf("PR #30 SessionID = %q, want %q (its own claimed session)", pr30.SessionID, session30.String())
	}
	if pr30.SessionID == session31.String() {
		t.Fatalf("PR #30 SessionID equals PR #31's own session -- cross-contamination")
	}

	pr31 := findItemByPR(result.Items, 31)
	if pr31 == nil {
		t.Fatal("PR #31 missing from the inbox entirely")
	}
	if pr31.SessionID != session31.String() {
		t.Errorf("PR #31 SessionID = %q, want %q (its own claimed session)", pr31.SessionID, session31.String())
	}
	if pr31.SessionID == session30.String() {
		t.Fatalf("PR #31 SessionID equals PR #30's own session -- cross-contamination")
	}

	pr32 := findItemByPR(result.Items, 32)
	if pr32 == nil {
		t.Fatal("PR #32 missing from the inbox entirely")
	}
	if pr32.SessionID != "" {
		t.Errorf("PR #32 (never mentioned) SessionID = %q, want empty", pr32.SessionID)
	}

	pr33 := findItemByPR(result.Items, 33)
	if pr33 == nil {
		t.Fatal("PR #33 missing from the inbox entirely")
	}
	if !pr33.IsRelease {
		t.Error("PR #33 IsRelease = false, want true (a persisted release manifest check exists)")
	}
	if pr33.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("PR #33 Kind = %s, want needs_review -- a release cut must never classify ready_to_merge even though every ordinary criterion (platform-authored/low-risk/CI-green/auto-approved-verdict) is met", pr33.Kind)
	}
	if pr33.ManifestFindingsCount != 2 {
		t.Errorf("PR #33 ManifestFindingsCount = %d, want 2", pr33.ManifestFindingsCount)
	}
	if !pr33.AggregateReviewTriggered {
		t.Error("PR #33 AggregateReviewTriggered = false, want true")
	}
	if pr33.SessionID != session33.String() {
		t.Errorf("PR #33 SessionID = %q, want %q (the review session that produced its own manifest check)", pr33.SessionID, session33.String())
	}
	if pr33.ManifestCoveragePartial {
		t.Error("PR #33 ManifestCoveragePartial = true, want false -- its persisted check recorded coverage_partial=false, and a complete scan must not be flagged as partial or the flag means nothing")
	}

	pr34 := findItemByPR(result.Items, 34)
	if pr34 == nil {
		t.Fatal("PR #34 missing from the inbox entirely")
	}
	if pr34.IsRelease {
		t.Error("PR #34 IsRelease = true, want false -- no release manifest check was ever persisted for it")
	}
	if pr34.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Errorf("PR #34 Kind = %s, want ready_to_merge -- the SAME ready-to-merge-eligible shape as PR #33, minus the release-cut signal, must still classify normally", pr34.Kind)
	}

	pr35 := findItemByPR(result.Items, 35)
	if pr35 == nil {
		t.Fatal("PR #35 missing from the inbox entirely")
	}
	if !pr35.IsRelease {
		t.Error("PR #35 IsRelease = false, want true (a persisted release manifest check exists)")
	}
	if pr35.ManifestFindingsCount != 0 {
		t.Errorf("PR #35 ManifestFindingsCount = %d, want 0 -- its persisted findings blob is empty", pr35.ManifestFindingsCount)
	}
	// The point of this row: a zero findings count over a TRUNCATED
	// constituent listing must arrive carrying the truncation, or the
	// inbox becomes the one consumer of this persisted row that reads a
	// partial scan as a clean audit -- the posted manifest comment
	// (reviewpost.RenderManifestComment) and the release-review readout
	// (httpapi's `coveragePartial`) both already refuse to.
	if !pr35.ManifestCoveragePartial {
		t.Error("PR #35 ManifestCoveragePartial = false, want true -- its persisted check recorded coverage_partial=true, so its zero findings count is a lower bound over an incomplete set, not a clean release")
	}

	pr36 := findItemByPR(result.Items, 36)
	if pr36 == nil {
		t.Fatal("PR #36 missing from the inbox entirely")
	}
	if !pr36.IsRelease {
		t.Error("PR #36 IsRelease = false, want true -- the check row exists, so this PR unambiguously IS a release cut however corrupt its findings blob is")
	}
	if pr36.ManifestFindingsCount != 0 {
		t.Errorf("PR #36 ManifestFindingsCount = %d, want 0 -- an undecodable blob yields no countable findings", pr36.ManifestFindingsCount)
	}
	// The distinct point of this row, and the reason it carries
	// coverage_partial=false: the forced flag must come from the DECODE
	// FAILURE itself, not from the truncation column. Without this, a
	// corrupt row renders exactly like a clean release cut with zero
	// findings, and the only trace is a log line nobody reading the inbox
	// will ever see.
	if !pr36.ManifestCoveragePartial {
		t.Error("PR #36 ManifestCoveragePartial = false, want true -- its findings blob could not be decoded at all, and a count that could not be read is not a count of zero (its coverage_partial column is false, so this must come from the decode failure)")
	}
}

// TestBuild_NoLinkedGitHubIdentity proves an actor with no linked GitHub
// identity still gets a usable (if PR-less) inbox -- resolveActorGitHub
// Credential's own ok=false path must never fail the whole Build call.
func TestBuild_NoLinkedGitHubIdentity(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "no-github@example.com", DisplayName: "No GitHub", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}

	deps := decisioninbox.Deps{
		Plans:              narvipg.NewPlanStore(pool),
		Sessions:           narvipg.NewSessionStore(pool),
		Participants:       narvipg.NewParticipantStore(pool),
		Automations:        narvipg.NewAutomationStore(pool),
		Outbox:             narvipg.NewOutboxStore(pool, false),
		ReviewFindings:     narvipg.NewReviewFindingStore(pool),
		SentinelFixes:      narvipg.NewSentinelFixStore(pool),
		Artifacts:          narvipg.NewArtifactStore(pool),
		Identities:         narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(&fakeDecisionInboxSourceControl{}, platform.DefaultTimeouts()),
		TokenEncryptionKey: []byte("01234567890123456789012345678901"),
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if result.SCMAsOf != nil {
		t.Errorf("SCMAsOf = %v, want nil (no GitHub identity linked, no SCM call attempted)", *result.SCMAsOf)
	}
	for _, it := range result.Items {
		if it.PRNumber != 0 {
			t.Errorf("unexpected PR item %+v with no linked GitHub identity", it)
		}
	}
}

// TestBuild_PlanOwnershipScoping proves buildPlanItems' own per-user
// scoping actually excludes/includes the right rows -- ListAwaitingApprovalPlans is DELIBERATELY unscoped by user
// (plans.sql's own doc comment: "this Step's own read model resolves
// per-user ELIGIBILITY at the app layer, not in this query"), so
// row.SessionCreatedBy==actor / participants.Exists is the ONLY per-user
// scoping over a deployment-wide plan scan -- a hardcoded
// ownedOrJoined := true would pass every OTHER existing test (every
// fixture elsewhere in this file is actor-owned).
func TestBuild_PlanOwnershipScoping(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	plans := narvipg.NewPlanStore(pool)
	participants := narvipg.NewParticipantStore(pool)

	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "b2-actor@example.com", DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}

	// Foreign-owned: a plan on a session created by a DIFFERENT user, with
	// actor neither its creator nor a participant -- must be EXCLUDED.
	foreignOwner, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "b2-foreign-owner@example.com", DisplayName: "Foreign Owner", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create foreign owner: %v", err)
	}
	foreignSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{Title: strPtr("Not the actor's own session"), SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: foreignOwner.ID})
	if err != nil {
		t.Fatalf("create foreign session: %v", err)
	}
	foreignTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: foreignSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create foreign turn: %v", err)
	}
	foreignPlan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: foreignSession.ID, TurnID: foreignTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create foreign plan: %v", err)
	}

	// Participant-joined: a plan on a session created by YET ANOTHER user,
	// but actor has a real participants row on that session -- must be
	// INCLUDED, exercising the "joined" half of ownedOrJoined that the
	// foreign-owned case above never touches.
	joinedOwner, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "b2-joined-owner@example.com", DisplayName: "Joined Session Owner", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create joined-session owner: %v", err)
	}
	joinedSession, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{Title: strPtr("Actor joined this one"), SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: joinedOwner.ID})
	if err != nil {
		t.Fatalf("create joined session: %v", err)
	}
	joinedTurn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: joinedSession.ID, Status: sqlcgen.TurnStatusCompleted})
	if err != nil {
		t.Fatalf("create joined-session turn: %v", err)
	}
	joinedPlan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: joinedSession.ID, TurnID: joinedTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("create joined-session plan: %v", err)
	}
	// No ParticipantStore.Create exists (participants.sql's own doc
	// comment: "nothing populates participants yet -- a distinct,
	// not-yet-scoped concern") -- raw SQL insert, exactly the row shape a
	// future Step's own writer would produce.
	if _, err := pool.Exec(ctx, `INSERT INTO participants (session_id, user_id) VALUES ($1, $2)`, joinedSession.ID, actor.ID); err != nil {
		t.Fatalf("insert participants row: %v", err)
	}
	// Sanity-check the fixture itself: Exists must now report true,
	// otherwise this test would trivially pass for the wrong reason.
	if exists, err := participants.Exists(ctx, joinedSession.ID, actor.ID); err != nil || !exists {
		t.Fatalf("participants.Exists() = (%v, %v), want (true, nil) -- fixture setup is broken", exists, err)
	}

	deps := decisioninbox.Deps{
		Plans: plans, Sessions: sessions, Participants: participants,
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(&fakeDecisionInboxSourceControl{}, platform.DefaultTimeouts()),
		TokenEncryptionKey: []byte("01234567890123456789012345678901"),
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	var foundForeign, foundJoined bool
	for _, it := range result.Items {
		switch it.PlanID {
		case foreignPlan.ID.String():
			foundForeign = true
		case joinedPlan.ID.String():
			foundJoined = true
		}
	}
	if foundForeign {
		t.Error("the foreign-owned plan (neither created by nor joined by the actor) is present in the actor's own inbox, want excluded")
	}
	if !foundJoined {
		t.Error("the participant-joined plan is missing from the actor's own inbox, want included (actor is a real participant on its session)")
	}
}

// TestBuild_PRLabelVariations covers two read-path PR classifications
// with no prior coverage:
//   - a PR carrying BOTH review:low-risk and review:needs-human must land
//     needs_review, never ready_to_merge (the needs-human escape hatch,
//     tested here alongside an otherwise-fully-eligible risk label so a
//     deleted needs-human check would be the ONLY thing making this test
//     fail).
//   - a handoff-labeled PR must land awaiting_approval AND still carry
//     its PR-shaped fields (CIGreen/Findings/IsHandoff) populated on the
//     domain Item itself -- the read-model half of this invariant
//     (the DTO-mapping half is covered separately in httpapi's own
//     decisioninbox_integration_test.go).
func TestBuild_PRLabelVariations(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)

	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "t4t6-actor@example.com", DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}

	const actorGitHubExternalID = "2001"
	tokenKey := []byte("01234567890123456789012345678901")
	encryptedToken, err := platform.EncryptToken(tokenKey, []byte("fake-gh-token"))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: actorGitHubExternalID,
		EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encryptedToken,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	const needsHumanPRURL = "https://github.com/acme/widgets/pull/20"
	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				// PR #20: needs-human ALONGSIDE low-risk, otherwise
				// IDENTICAL in shape to TestBuild_FullScenario's own
				// ready_to_merge PR #10 (platform-authored, CI green,
				// low-risk, directly assigned) -- needs-human must be the
				// ONLY thing keeping this PR out of ready_to_merge, so
				// this fixture deliberately marks it platform-authored
				// too (below): a deleted/bypassed needs-human check would
				// otherwise still correctly land this PR in needs_review
				// for the WRONG reason (not platform-authored), letting
				// the mutation survive undetected.
				{
					Owner: "acme", Repo: "widgets", Number: 20, Title: "needs-human + low-risk",
					HTMLURL: needsHumanPRURL, HeadSHA: "sha20",
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk", "review:needs-human"},
					CreatedAt:    time.Now(),
				},
				// PR #21: handoff-labeled -- must ride awaiting_approval,
				// never needs_review/ready_to_merge, while still carrying
				// its own PR-shaped fields.
				{
					Owner: "acme", Repo: "widgets", Number: 21, Title: "prototype: handoff to engineering",
					HTMLURL: "https://github.com/acme/widgets/pull/21", HeadSHA: "sha21",
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk", "handoff"},
					CreatedAt:    time.Now(),
				},
			},
		},
	}

	artifacts := narvipg.NewArtifactStore(pool)
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: needsHumanPRURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #20 platform-authored: %v", err)
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: identities,
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	needsHumanPR := findItemByPR(result.Items, 20)
	if needsHumanPR == nil {
		t.Fatal("PR #20 missing from the inbox entirely")
	}
	if needsHumanPR.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("PR #20 (needs-human + low-risk) Kind = %s, want needs_review (needs-human must force it out of ready_to_merge)", needsHumanPR.Kind)
	}

	handoffPR := findItemByPR(result.Items, 21)
	if handoffPR == nil {
		t.Fatal("PR #21 missing from the inbox entirely")
	}
	if handoffPR.Kind != decisioninboxdomain.KindAwaitingApproval {
		t.Errorf("PR #21 (handoff) Kind = %s, want awaiting_approval", handoffPR.Kind)
	}
	if !handoffPR.IsHandoff {
		t.Error("PR #21 (handoff) IsHandoff = false, want true")
	}
	if !handoffPR.CIGreen {
		t.Error("PR #21 (handoff) CIGreen = false, want true -- PR-shaped fields must still populate for a handoff row (the read-path half of the handoff-PR-fields invariant)")
	}
	if handoffPR.Findings != 0 {
		t.Errorf("PR #21 (handoff) Findings = %d, want 0", handoffPR.Findings)
	}
	if handoffPR.RepoFullName != "acme/widgets" {
		t.Errorf("PR #21 (handoff) RepoFullName = %q, want acme/widgets -- must still populate even though Kind is awaiting_approval", handoffPR.RepoFullName)
	}
}

// decisionInboxActorFixture creates a fresh member actor with a linked
// GitHub identity -- the setup every test below this point repeats
// verbatim, factored out once these three new tests made the duplication
// worth naming.
func decisionInboxActorFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, primaryEmail, externalID string, tokenKey []byte) sqlcgen.User {
	t.Helper()
	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)

	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: primaryEmail, DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	encryptedToken, err := platform.EncryptToken(tokenKey, []byte("fake-gh-token"))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encryptedToken,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	return actor
}

// seedReviewAttemptTurn (round 3, finding R10, adversarial review) seeds
// a real, minimal is_review_attempt=true turn (on a throwaway session)
// and returns its id -- HasNewerReviewAttempt now fails CLOSED
// (hasNewer=true, "deny the waiver") whenever an acceptance's own
// AttemptID is empty, so any fixture that means to create a CURRENTLY
// APPLICABLE acceptance (this package's own tests that assert
// Applicable()==true, ok=true via acceptance, or AcceptanceID/
// AcceptanceMergeable being set on a Build() Item) must record a real
// attempt id here rather than the zero pgtype.UUID{} many of these
// fixtures used before this fix -- otherwise Applicable's own final
// `!hasNewerAttempt` is unconditionally false, and the acceptance can
// never actually apply, regardless of what else this test means to
// isolate.
func seedReviewAttemptTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("seedReviewAttemptTurn: create session: %v", err)
	}
	turn, err := narvipg.NewTurnStore(pool).Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("seedReviewAttemptTurn: create turn: %v", err)
	}
	return turn.ID
}

// TestBuild_HasChangesRequestedDemotesFromReadyToMerge is the read-path
// regression test proving that buildPROpenItem consults
// HasChangesRequested when classifying Kind, since it is a HARD merge
// blocker at RevalidateForMerge -- previously such a PR sat in the TOP
// ready_to_merge section with a Merge button that would unconditionally
// 409 at click time. This
// fixture is otherwise IDENTICAL to TestBuild_FullScenario's own
// ready_to_merge PR #10 (platform-authored, low-risk, CI green, directly
// assigned) so HasChangesRequested is the ONLY thing keeping it out of
// ready_to_merge -- a deleted/bypassed check would otherwise still
// correctly land this PR in needs_review for the WRONG reason.
func TestBuild_HasChangesRequestedDemotesFromReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "4001"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "p14-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/40"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #40 platform-authored: %v", err)
	}
	// (§21.1/§21.2): PR #40 needs a Shippable=auto review_verdicts
	// row at its own exact head sha too -- see seedAutoApprovedVerdict's
	// own doc comment for why, WITHOUT this, HasChangesRequested would no
	// longer be the ONLY thing this fixture demonstrates keeps a PR out
	// of ready_to_merge (a missing verdict alone would already do that).
	seedAutoApprovedVerdict(ctx, t, pool, "acme/widgets", 40, "sha40")

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 40, Title: "otherwise fully eligible, but changes requested",
					HTMLURL: htmlURL, HeadSHA: "sha40",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees:           []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion:        ports.CIConclusionSuccess,
					Labels:              []string{"review:low-risk"},
					HasChangesRequested: true,
					CreatedAt:           time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr40 := findItemByPR(result.Items, 40)
	if pr40 == nil {
		t.Fatal("PR #40 missing from the inbox entirely")
	}
	if pr40.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("PR #40 (changes requested) Kind = %s, want needs_review -- HasChangesRequested must force it out of ready_to_merge even though it is otherwise fully eligible", pr40.Kind)
	}
	if !pr40.HasChangesRequested {
		t.Error("PR #40 HasChangesRequested = false, want true -- the domain Item field itself must also surface this fact")
	}
}

// TestBuild_AncestorChainMatches_LiveResolved_StaysReadyToMerge is
// round-10 finding B's own regression test for computeRealEligibility's
// (aggregate.go) CurrentAncestorChain wiring: pr.AncestorChain reports
// "main" with a DELIBERATELY WRONG cached SHA (GitHub's own per-PR cached
// stack field, the same shape finding F1 already proved stale-by-design
// for the immediate base), while the verdict's own recorded ancestor
// chain carries "main" paired with testEligibleBaseRef's own LIVE tip
// (testEligibleBaseSHA, the SAME value fakeDecisionInboxSourceControl's
// own ResolveBranchSHA fallback already resolves "main" to elsewhere in
// this file) -- proving the comparison uses the LIVE resolution, never
// the cached SHA, and stays ready_to_merge when they genuinely agree.
// Mutation-test target: deleting `CurrentAncestorChain: currentAncestorChain,`
// from either EligibilityInput literal in aggregate.go (defaulting it to
// nil) would wrongly mismatch this otherwise-agreeing fixture, turning
// this test's own KindReadyToMerge assertion into KindNeedsReview.
func TestBuild_AncestorChainMatches_LiveResolved_StaysReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "4003"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "p14c-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/42"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #42 platform-authored: %v", err)
	}

	// A real verdict whose own recorded ancestor chain matches EXACTLY
	// what a live resolution of "main" reports (testEligibleBaseSHA) --
	// never seedAutoApprovedVerdict, which always leaves this nil.
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	verdictContext := reviewverdict.Context{
		BaseRef:       testEligibleBaseRef,
		BaseSHA:       testEligibleBaseSHA,
		AncestorChain: []review.AncestorLink{{Ref: "main", SHA: testEligibleBaseSHA}},
		PolicyVersion: autoapproval.CurrentPolicyVersion,
	}
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, "acme/widgets", 42, "sha42", pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded verdict whose recorded ancestor chain matches the live resolution."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed verdict with a matching ancestor chain: %v", err)
	}

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 42, Title: "otherwise fully eligible, ancestor chain genuinely matches",
					HTMLURL: htmlURL, HeadSHA: "sha42",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					AncestorChain: []ports.PRAncestorLink{{Ref: "main", SHA: "stale-cached-sha-must-be-ignored"}},
					Assignees:     []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion:  ports.CIConclusionSuccess,
					Labels:        []string{"review:low-risk"},
					CreatedAt:     time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettings, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr42 := findItemByPR(result.Items, 42)
	if pr42 == nil {
		t.Fatal("PR #42 missing from the inbox entirely")
	}
	if pr42.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Errorf("PR #42 (ancestor chain genuinely matches, live-resolved) Kind = %s, want ready_to_merge -- the cached, deliberately-wrong pr.AncestorChain SHA must never be consulted", pr42.Kind)
	}
}

// TestBuild_AncestorChainUnknown_LiveResolveFails_DemotesAndMarksDegraded is
// round-11 finding A1's own regression test for computeRealEligibility's
// (aggregate.go) currentAncestorChain block, one specific hole finding E
// separately named: the block's own two-case switch (err != nil /
// liveAncestorSHA != "") had NO default arm at all, so a live resolve
// failure fell through to currentAncestorChain's own zero value, nil --
// INDISTINGUISHABLE, once compared, from "this PR was never in a stack at
// all".
//
// D3 (round-12 sweep, execution-verified): the PREVIOUS version of this
// fixture recorded the verdict's OWN ancestor chain as ALSO non-nil
// ({Ref: "release-parent", ...}) -- deliberately, per that version's own
// comment ("never seedAutoApprovedVerdict's own nil"). Executed with the
// EXACT mutation this comment used to name (only the switch's error-case
// `degraded = true` line deleted): the Kind assertion below still
// passed, unchanged -- only SCMFetchFailed caught it, and re-executing
// with the verdict's own ancestor chain changed to nil (below) did NOT
// change that: this specific mutation can never move Kind, for a
// reason worth being explicit about, because the FIRST version of this
// comment's own claim ("mutation-verified... the mutation flips Kind")
// was itself wrong and had to be corrected after actually running it.
// currentAncestorChain is preset to its own fail-closed unknownMarker
// (SHA == "", a non-nil link) BEFORE the switch below ever runs, so
// deleting ONLY the error case's `degraded = true` leaves
// currentAncestorChain exactly as unknown as it already was -- Kind was
// always going to refuse via ReasonAncestorChainUnknown regardless,
// which is a GOOD belt-and-suspenders property of the fix, not a gap,
// but it does mean this ONE mutation only ever pins SCMFetchFailed.
//
// A SECOND, sharper mutation -- deleting the preset assignment itself
// (`currentAncestorChain = unknownMarker` immediately before the
// switch, leaving currentAncestorChain at its bare nil zero value unless
// the success case fires) -- reproduces the ORIGINAL pre-A1-fix shape
// exactly ("a two-case switch with no default arm at all"). Executed
// with the verdict's own ancestor chain nil (below, this fixture's own
// fix): that mutation DOES flip the Kind assertion, from needs_review to
// ready_to_merge -- ancestorChainEqual(nil, nil) trivially matches once
// currentAncestorChain has nothing pinning it to an unknown marker, the
// exact silent-approval hazard A1 exists to close. With VerdictAncestorChain
// non-nil (the PREVIOUS version of this fixture), that same mutation
// instead refuses via ReasonAncestorChainChanged (a length mismatch,
// 0 vs 1) -- still fail-closed, but for the wrong reason, and Kind would
// never have caught the RIGHT one. Fixed by dropping this fixture's own
// second verdict insert -- buildEligibleReadyToMergeFixture's own
// seedAutoApprovedVerdict already seeds AncestorChain nil, exactly the
// "recorded before it was ever stacked" precondition this scenario
// needs.
//
// Mutation-test targets, each pinned by its OWN assertion below (not
// both by either, per the execution above):
//   - Deleting the currentAncestorChain block's own preset
//     `currentAncestorChain = unknownMarker` default (leaving it at nil
//     unless the success case fires) flips Kind from needs_review to
//     ready_to_merge.
//   - Deleting the switch's own `case liveAncestorErr != nil: ...
//     degraded = true` arm (leaving the marker in place but the row
//     un-flagged) flips SCMFetchFailed from true to false.
func TestBuild_AncestorChainUnknown_LiveResolveFails_DemotesAndMarksDegraded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5012"
	const repoFullName = "acme/build-ancestor-chain-unknown"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "a1-actor@example.com", actorGitHubExternalID, repoFullName, 80)

	// The live PR reports a real ancestor link on a branch
	// ("release-parent") NO seeded PR's own BaseRef names -- the fake's
	// own scan-fallback (ResolveBranchSHA's doc comment) then naturally
	// returns an error for this SPECIFIC branch while the immediate base
	// ("main", testEligibleBaseRef, matched by the seeded PR itself)
	// still resolves cleanly -- isolating this test to the ANCESTOR
	// resolve's own failure alone.
	fakeSCM.openPRsByExternalID[actorGitHubExternalID][0].AncestorChain = []ports.PRAncestorLink{{Ref: "release-parent", SHA: "cached-irrelevant-snapshot"}}

	// D3 fix: the verdict's OWN ancestor chain stays NIL --
	// buildEligibleReadyToMergeFixture's own seedAutoApprovedVerdict
	// already seeds exactly that ("recorded before it was ever stacked",
	// that helper's own doc comment) -- no second verdict insert needed,
	// and none is made here. This is the precondition the scenario above
	// actually requires: a buggy nil CurrentAncestorChain must be able to
	// trivially equal VerdictAncestorChain for the hazard to manifest at
	// all (see this test's own top doc comment).
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettings, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (a live SCM lookup failure must degrade ONE row, never fail the whole Build call)", err)
	}
	item := findItemByPR(result.Items, 80)
	if item == nil {
		t.Fatal("PR #80 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- an unresolvable ancestor-chain link must fail closed via ReasonAncestorChainUnknown, never silently read as no ancestor chain at all")
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- a failed ancestor-chain live resolve must mark the whole read degraded, exactly like a failed base ResolveBranchSHA call already does")
	}
}

// TestBuild_AncestorChainDegradedRef_DemotesAndMarksDegraded is D1's own
// regression test (round-12 sweep) for computeRealEligibility's
// (aggregate.go) currentAncestorChain block, exercised through the
// read-model path this time (revalidate_integration_test.go's own
// AncestorChainDegradedRef_Refused pins the identical scenario through
// the action-endpoint path). The live PR reports a stack position that
// PROVES a link exists but a degraded read left the ref itself empty --
// ports.PRAncestorLink{Ref: "", SHA: ""}, this port's own dedicated
// "could not be established" marker -- never the SAME nil this package's
// every OTHER fixture reports for "genuinely no ancestor at all". The
// verdict's own recorded ancestor chain stays nil
// (buildEligibleReadyToMergeFixture's own seedAutoApprovedVerdict,
// unchanged): "recorded before it was ever stacked", the precondition
// that makes this scenario dangerous -- before D1, this block's own
// guard required pr.AncestorChain[0].Ref != "" to even enter its
// comparison at all, so this exact degraded-ref case fell through to
// currentAncestorChain's own nil zero value, trivially matching the
// verdict's own nil, and this row would have rendered ready_to_merge
// despite GitHub itself reporting an ancestor link this code never even
// looked at.
//
// Mutation-test target: reinstating `&& pr.AncestorChain[0].Ref != ""`
// on this block's own guard (this fix's own inverse) turns this test's
// own KindNeedsReview/SCMFetchFailed assertions back into
// KindReadyToMerge/false.
func TestBuild_AncestorChainDegradedRef_DemotesAndMarksDegraded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5014"
	const repoFullName = "acme/build-ancestor-chain-degraded-ref"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "d1-actor@example.com", actorGitHubExternalID, repoFullName, 82)

	fakeSCM.openPRsByExternalID[actorGitHubExternalID][0].AncestorChain = []ports.PRAncestorLink{{Ref: "", SHA: ""}}

	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettings, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (a degraded ancestor-chain link must degrade ONE row, never fail the whole Build call)", err)
	}
	item := findItemByPR(result.Items, 82)
	if item == nil {
		t.Fatal("PR #82 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- a degraded stack read (position > 1, no ref decoded) must fail closed via ReasonAncestorChainUnknown, never silently read as no ancestor chain at all")
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- a degraded ancestor-chain link must mark the whole read degraded, exactly like a failed live resolve already does")
	}
}

// TestBuild_AncestorChainAdvanced_ConfirmedFastForward_StaysReadyToMerge is
// round-11 finding A3's own regression test for computeRealEligibility --
// the SAME fast-forward tolerance BaseBranchAdvanced_ConfirmedFastForward
// (revalidate_integration_test.go) pins for the immediate base, one link
// further out, exercised through the read-model path (aggregate.go) this
// time. Before this fix, ANY ancestor-chain sha movement refused
// unconditionally (autoapproval.ancestorChainEqual had no tolerance
// parameter at all).
func TestBuild_AncestorChainAdvanced_ConfirmedFastForward_StaysReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5013"
	const repoFullName = "acme/build-ancestor-chain-advanced-confirmed"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "a3-actor@example.com", actorGitHubExternalID, repoFullName, 81)

	fakeSCM.openPRsByExternalID[actorGitHubExternalID][0].AncestorChain = []ports.PRAncestorLink{{Ref: "release-parent", SHA: "cached-irrelevant-snapshot"}}
	fakeSCM.resolveBranchSHAByBranch = map[string]string{"release-parent": "release-parent-new-sha"}
	fakeSCM.isAncestorResult = true

	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	verdictContext := reviewverdict.Context{
		BaseRef:       testEligibleBaseRef,
		BaseSHA:       testEligibleBaseSHA,
		AncestorChain: []review.AncestorLink{{Ref: "release-parent", SHA: "release-parent-old-sha"}},
		PolicyVersion: autoapproval.CurrentPolicyVersion,
	}
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettings, false, repoFullName, 81, "sha-81", pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "verdict recorded before the ancestor chain's own confirmed fast-forward"}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed verdict with an advanced-but-confirmed ancestor chain: %v", err)
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettings, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 81)
	if item == nil {
		t.Fatal("PR #81 missing from the inbox entirely")
	}
	if item.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Errorf("Kind = %s, want ready_to_merge -- A3: an ancestor-chain movement CONFIRMED as a pure fast-forward must not refuse an otherwise-eligible PR", item.Kind)
	}
	if len(fakeSCM.isAncestorCalls) != 1 {
		t.Fatalf("IsAncestor called %d times, want 1", len(fakeSCM.isAncestorCalls))
	}
	gotCall := fakeSCM.isAncestorCalls[0]
	if gotCall.Ancestor != "release-parent-old-sha" || gotCall.Descendant != "release-parent-new-sha" {
		t.Errorf("IsAncestor(Ancestor, Descendant) = (%q, %q), want (%q, %q)",
			gotCall.Ancestor, gotCall.Descendant, "release-parent-old-sha", "release-parent-new-sha")
	}
}

// TestBuild_AncestorChainChanged_DemotesFromReadyToMerge is round-10
// finding A2's own regression test for computeRealEligibility's (aggregate.go)
// VerdictAncestorChain/CurrentAncestorChain wiring -- the SAME "otherwise
// fully eligible" fixture shape as
// TestBuild_HasChangesRequestedDemotesFromReadyToMerge above, but
// perturbing ports.OpenPR.AncestorChain instead of HasChangesRequested:
// seedAutoApprovedVerdict's own recorded verdict always carries a nil
// ancestor chain (its own doc comment: "none of this package's fixtures
// are GitHub-native-stack PRs"), so a LIVE PR that now reports one (this
// PR's parent moved beneath it in a GitHub-native stack, §21.1's
// amendment) must demote out of ready_to_merge -- the verdict never
// examined the code as it stands now.
//
// Round-11 finding E (corrected): the PREVIOUS version of this comment
// named its own mutation target as deleting
// `VerdictAncestorChain: record.Context.AncestorChain,` from EITHER of
// computeRealEligibility's two autoapproval.EligibilityInput literals --
// then, in its very next parenthetical, correctly explained why that
// mutation is a NO-OP for this specific fixture ("this fixture's own
// VerdictAncestorChain is already nil by construction, so it alone cannot
// distinguish 'wired to nil' from 'never wired at all'") -- a
// self-contradiction inside the same doc comment: record.Context.
// AncestorChain IS nil here (seedAutoApprovedVerdict's own doc comment
// above), so that wiring line's assigned value is indistinguishable from
// its own Go zero value, and deleting it changes nothing this test can
// observe.
//
// The mutation this test ACTUALLY pins is CurrentAncestorChain's own
// wiring (both the probe's and the final call's, in EITHER
// EligibilityInput literal in aggregate.go): this fixture's live PR
// reports a REAL ancestor chain (Position-2, a genuine
// `AncestorChain: []ports.PRAncestorLink{{Ref: "main", ...}}` below)
// against a verdict recorded with none -- the two only disagree at all
// because CurrentAncestorChain carries the live chain in. Every OTHER
// fixture in this file leaves the live PR's own AncestorChain nil too, so
// autoapproval.ancestorChainEqual(nil, nil) trivially passes regardless
// of whether CurrentAncestorChain's own wiring line even exists --
// exactly what made it independently deletable before this test existed.
// Mutation-test target: deleting `CurrentAncestorChain: currentAncestorChain,`
// from the FINAL EligibilityInput literal in aggregate.go must turn this
// test's own KindNeedsReview assertion back into KindReadyToMerge (its own
// zero value, nil, would then trivially match VerdictAncestorChain's
// identical nil) -- paired with
// TestBuild_AncestorChainMatches_LiveResolved_StaysReadyToMerge above,
// which independently pins the SAME wiring line's positive direction (a
// live chain that DOES match a real recorded one must stay eligible).
func TestBuild_AncestorChainChanged_DemotesFromReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "4002"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "p14b-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/41"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #41 platform-authored: %v", err)
	}
	// seedAutoApprovedVerdict's own recorded ancestor chain stays nil --
	// this fixture's whole point is that the LIVE chain below no longer
	// agrees with it.
	seedAutoApprovedVerdict(ctx, t, pool, "acme/widgets", 41, "sha41")

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 41, Title: "otherwise fully eligible, but the ancestor chain moved",
					HTMLURL: htmlURL, HeadSHA: "sha41",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					AncestorChain: []ports.PRAncestorLink{{Ref: "main", SHA: "a-parent-moved-beneath-this-pr"}},
					Assignees:     []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion:  ports.CIConclusionSuccess,
					Labels:        []string{"review:low-risk"},
					CreatedAt:     time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr41 := findItemByPR(result.Items, 41)
	if pr41 == nil {
		t.Fatal("PR #41 missing from the inbox entirely")
	}
	if pr41.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("PR #41 (ancestor chain changed) Kind = %s, want needs_review -- the verdict's own recorded ancestor chain (nil) no longer matches the PR's live one, and must force it out of ready_to_merge even though it is otherwise fully eligible", pr41.Kind)
	}
}

// TestBuild_AcceptedVerdict_BaseMoved_HidesStaleAcceptance pins finding F6
// (adversarial review): after a PR's base ref moves (a retarget), a row's
// own acceptanceJustification/acceptedAt must NOT keep rendering as
// active, even though the underlying review_verdict_acceptances row is
// still non-revoked and still Applicable (same verdict id) --
// contradicting the wire contract otherwise (DecisionInboxItem.
// acceptanceJustification's own description names "a moved base... a
// changed ancestor chain" as invalidating triggers) and telling a human
// the acceptance stands when a merge attempt would actually refuse on
// ReasonBaseMoved. Mutation-test target: deleting the
// acceptanceContextStillFresh(record.Context, pr) conjunct from
// buildPROpenItem's own acceptance-display condition (aggregate.go) must
// turn this test's own "acceptance hidden" assertion from a pass back
// into a failure (the row would then render the stale acceptance again).
func TestBuild_AcceptedVerdict_BaseMoved_HidesStaleAcceptance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "4003"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "f6-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/42"
	const repoFullName = "acme/widgets"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #42 platform-authored: %v", err)
	}

	// A high-risk (not Shippable=auto) verdict, otherwise fully eligible,
	// recorded against testEligibleBaseRef -- mirrors internal/app/
	// decisioninbox's own seedNotShippableAutoVerdict fixture (this
	// package's acceptance_integration_test.go), inlined here since this
	// file has no such helper of its own.
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettingsStore := narvipg.NewRepoSettingsStore(pool)
	acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelHigh,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableNeedsHuman,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if verdict.Shippable == review.ShippableAuto {
		t.Fatalf("fixture bug -- RiskLevelHigh computed Shippable=auto, want anything else")
	}
	verdictContext := reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion}
	record, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettingsStore, false, repoFullName, 42, "sha42", pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded high-risk verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{})
	if err != nil {
		t.Fatalf("seed not-shippable-auto review_verdicts row: %v", err)
	}

	var verdictID pgtype.UUID
	if err := verdictID.Scan(record.ID); err != nil {
		t.Fatalf("scan verdict id: %v", err)
	}
	if _, _, err := appreviewverdict.Accept(ctx, acceptances, appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      42,
		VerdictID:     verdictID,
		AttemptID:     seedReviewAttemptTurn(ctx, t, pool),
		HeadSHA:       "sha42",
		Context:       verdictContext,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "Accepted -- about to become stale via a base move.",
		AcceptedBy:    actor.ID,
	}); err != nil {
		t.Fatalf("Accept() error = %v, want nil", err)
	}

	// The live PR's own base ref has MOVED (a retarget) relative to the
	// verdict's own recorded testEligibleBaseRef -- nobody revoked the
	// acceptance.
	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 42, Title: "accepted, then retargeted",
					HTMLURL: htmlURL, HeadSHA: "sha42",
					BaseRef: "retargeted-branch", BaseSHA: "sha-retargeted-base",
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					CreatedAt:    time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettingsStore, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Acceptances: acceptances, Turns: narvipg.NewTurnStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr42 := findItemByPR(result.Items, 42)
	if pr42 == nil {
		t.Fatal("PR #42 missing from the inbox entirely")
	}
	if pr42.AcceptanceJustification != "" || pr42.AcceptanceID != "" || !pr42.AcceptedAt.IsZero() {
		t.Errorf("PR #42 (base moved since acceptance) still renders an acceptance -- AcceptanceID=%q AcceptanceJustification=%q AcceptedAt=%v, want all absent: a moved base makes this acceptance inapplicable, and the row must not tell a human otherwise", pr42.AcceptanceID, pr42.AcceptanceJustification, pr42.AcceptedAt)
	}

	// Confirm this is genuinely the "moved base" case, not "nobody
	// revoked it happened to also be gone" -- GetActiveAcceptance must
	// still report the SAME row as active underneath.
	stillActive, ok, err := appreviewverdict.GetActiveAcceptance(ctx, acceptances, repoFullName, 42)
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if !ok || stillActive.Revoked() {
		t.Fatal("GetActiveAcceptance: ok = false or Revoked() = true, want an active, non-revoked row -- this test's own point is that the base move ALONE hides it from the read model, not revocation")
	}
}

// TestBuild_AcceptedVerdict_HeadMoved_HidesStaleAcceptance pins finding R2
// (round 3, adversarial review): a PUSH -- with no new review attempt yet
// dispatched (the auto-retrigger is debounced and capped by
// reviewAutoRetriggerBudget, so once that budget is spent no new attempt
// EVER arrives to invalidate the acceptance) -- must ALSO hide a stale
// acceptance, exactly like a moved base already does above. Before this
// fix, buildPROpenItem's own acceptance-display condition compared only
// acceptance.Applicable(record.ID, hasNewerAttempt) (unaffected by a
// push with no new attempt: record.ID stays the SAME latest verdict, and
// hasNewerAttempt stays false) and acceptanceContextStillFresh's own
// base-ref/ancestor-chain check (ALSO unaffected here: both stay
// unchanged from the accepted verdict) -- so BOTH passed, and the row
// kept rendering this acceptance as active even though its own recorded
// head_sha no longer matched the PR's live HeadSHA, which
// autoapproval.ComputeEligible's own unconditional ReasonStaleVerdict
// check (checked BEFORE, and never waived by, the acceptance-waivable
// criteria) would refuse on at merge time.
//
// Mutation-test target: deleting the `record.HeadSHA == pr.HeadSHA`
// conjunct from buildPROpenItem's own acceptance-display condition
// (aggregate.go) must turn this test's own "acceptance hidden" assertion
// from a pass back into a failure (the row would then render the stale
// acceptance again) -- mirrors TestBuild_AcceptedVerdict_BaseMoved_
// HidesStaleAcceptance's own identical mutation-test-target discipline,
// immediately above, for the ref-level half of this same guard.
func TestBuild_AcceptedVerdict_HeadMoved_HidesStaleAcceptance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "4004"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "r2-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/43"
	const repoFullName = "acme/widgets"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #43 platform-authored: %v", err)
	}

	// A high-risk (not Shippable=auto) verdict, otherwise fully eligible,
	// recorded against testEligibleBaseRef/testEligibleBaseSHA and head
	// sha43 -- mirrors TestBuild_AcceptedVerdict_BaseMoved_
	// HidesStaleAcceptance's own identical fixture shape above, EXCEPT
	// only the head sha differs below (base ref/sha and ancestor chain
	// stay unchanged on both sides) -- isolating the head-SHA half of
	// "a new attempt, a moved base, or a changed ancestor chain" from the
	// ref-level half that test already pins.
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettingsStore := narvipg.NewRepoSettingsStore(pool)
	acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelHigh,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableNeedsHuman,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if verdict.Shippable == review.ShippableAuto {
		t.Fatalf("fixture bug -- RiskLevelHigh computed Shippable=auto, want anything else")
	}
	verdictContext := reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion}
	record, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettingsStore, false, repoFullName, 43, "sha43", pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded high-risk verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{})
	if err != nil {
		t.Fatalf("seed not-shippable-auto review_verdicts row: %v", err)
	}

	var verdictID pgtype.UUID
	if err := verdictID.Scan(record.ID); err != nil {
		t.Fatalf("scan verdict id: %v", err)
	}
	if _, _, err := appreviewverdict.Accept(ctx, acceptances, appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      43,
		VerdictID:     verdictID,
		AttemptID:     seedReviewAttemptTurn(ctx, t, pool),
		HeadSHA:       "sha43",
		Context:       verdictContext,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "Accepted -- about to become stale via a push, no new attempt yet dispatched.",
		AcceptedBy:    actor.ID,
	}); err != nil {
		t.Fatalf("Accept() error = %v, want nil", err)
	}

	// The live PR's own head sha has MOVED (a push) relative to what was
	// accepted -- base ref/sha are UNCHANGED, and nobody revoked the
	// acceptance, so acceptanceContextStillFresh's own ref-level check
	// alone would still pass this row through.
	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 43, Title: "accepted, then pushed",
					HTMLURL: htmlURL, HeadSHA: "sha43-pushed",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					CreatedAt:    time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettingsStore, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Acceptances: acceptances, Turns: narvipg.NewTurnStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr43 := findItemByPR(result.Items, 43)
	if pr43 == nil {
		t.Fatal("PR #43 missing from the inbox entirely")
	}
	if pr43.AcceptanceJustification != "" || pr43.AcceptanceID != "" || !pr43.AcceptedAt.IsZero() {
		t.Errorf("PR #43 (head moved since acceptance, no new attempt) still renders an acceptance -- AcceptanceID=%q AcceptanceJustification=%q AcceptedAt=%v, want all absent: a moved head makes this acceptance inapplicable, and the row must not tell a human otherwise", pr43.AcceptanceID, pr43.AcceptanceJustification, pr43.AcceptedAt)
	}

	// Confirm this is genuinely the "moved head, no new attempt" case,
	// not "nobody revoked it happened to also be gone" -- GetActiveAcceptance
	// must still report the SAME row as active underneath.
	stillActive, ok, err := appreviewverdict.GetActiveAcceptance(ctx, acceptances, repoFullName, 43)
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if !ok || stillActive.Revoked() {
		t.Fatal("GetActiveAcceptance: ok = false or Revoked() = true, want an active, non-revoked row -- this test's own point is that the head move ALONE hides it from the read model, not revocation")
	}
}

// TestBuild_AcceptedVerdict_AncestorChainChanged_HidesStaleAcceptance
// pins finding R4 (round 3, adversarial review): acceptanceContextStillFresh's
// own ancestor-chain-length comparison (aggregate.go) had NO regression
// test of its own -- it could be deleted outright with the whole suite
// still green, even though the function's own doc comment and
// TestBuild_AcceptedVerdict_BaseMoved_HidesStaleAcceptance's own doc
// comment (immediately above) advertise it as a mutation-test target
// alongside the base-ref half, which THAT test actually exercises. This
// test isolates the ancestor-chain half specifically: base ref/sha and
// head sha all stay UNCHANGED between the accepted verdict and the live
// PR -- only the ancestor chain differs (the verdict was recorded while
// this PR was stacked on a parent branch; the live PR now reports no
// stack at all, a length mismatch acceptanceContextStillFresh's own
// `len(recordContext.AncestorChain) != len(pr.AncestorChain)` check
// exists to catch).
//
// Mutation-test target: deleting acceptanceContextStillFresh's own
// ancestor-chain comparison (both the length check and the per-link Ref
// loop, aggregate.go) must turn this test's own "acceptance hidden"
// assertion from a pass back into a failure.
func TestBuild_AcceptedVerdict_AncestorChainChanged_HidesStaleAcceptance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "4005"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "r4-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/44"
	const repoFullName = "acme/widgets"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #44 platform-authored: %v", err)
	}

	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettingsStore := narvipg.NewRepoSettingsStore(pool)
	acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelHigh,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableNeedsHuman,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if verdict.Shippable == review.ShippableAuto {
		t.Fatalf("fixture bug -- RiskLevelHigh computed Shippable=auto, want anything else")
	}
	// The verdict was recorded while this PR was STACKED on a parent
	// branch -- verdictContext.AncestorChain carries ONE link.
	verdictContext := reviewverdict.Context{
		BaseRef:       testEligibleBaseRef,
		BaseSHA:       testEligibleBaseSHA,
		AncestorChain: []review.AncestorLink{{Ref: "feature/stack-parent", SHA: "sha-stack-parent"}},
		PolicyVersion: autoapproval.CurrentPolicyVersion,
	}
	record, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettingsStore, false, repoFullName, 44, "sha44", pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded high-risk verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{})
	if err != nil {
		t.Fatalf("seed not-shippable-auto review_verdicts row: %v", err)
	}

	var verdictID pgtype.UUID
	if err := verdictID.Scan(record.ID); err != nil {
		t.Fatalf("scan verdict id: %v", err)
	}
	if _, _, err := appreviewverdict.Accept(ctx, acceptances, appreviewverdict.AcceptInput{
		RepoFullName:  repoFullName,
		PRNumber:      44,
		VerdictID:     verdictID,
		AttemptID:     seedReviewAttemptTurn(ctx, t, pool),
		HeadSHA:       "sha44",
		Context:       verdictContext,
		Reason:        string(autoapproval.ReasonNotShippableAuto),
		Justification: "Accepted while stacked -- about to become stale via a restructure.",
		AcceptedBy:    actor.ID,
	}); err != nil {
		t.Fatalf("Accept() error = %v, want nil", err)
	}

	// The live PR reports NO ancestor chain at all -- base ref/sha and
	// head sha are all UNCHANGED from what was accepted; only the stack
	// itself changed (a restructure landed the stacked branch, so this PR
	// now sits directly on testEligibleBaseRef).
	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 44, Title: "accepted while stacked, then restructured",
					HTMLURL: htmlURL, HeadSHA: "sha44",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					AncestorChain: nil,
					Assignees:     []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion:  ports.CIConclusionSuccess,
					CreatedAt:     time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettingsStore, ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Acceptances: acceptances, Turns: narvipg.NewTurnStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr44 := findItemByPR(result.Items, 44)
	if pr44 == nil {
		t.Fatal("PR #44 missing from the inbox entirely")
	}
	if pr44.AcceptanceJustification != "" || pr44.AcceptanceID != "" || !pr44.AcceptedAt.IsZero() {
		t.Errorf("PR #44 (ancestor chain changed since acceptance) still renders an acceptance -- AcceptanceID=%q AcceptanceJustification=%q AcceptedAt=%v, want all absent: a changed ancestor chain makes this acceptance inapplicable, and the row must not tell a human otherwise", pr44.AcceptanceID, pr44.AcceptanceJustification, pr44.AcceptedAt)
	}

	// Confirm this is genuinely the "ancestor chain changed" case, not
	// "nobody revoked it happened to also be gone" -- GetActiveAcceptance
	// must still report the SAME row as active underneath.
	stillActive, ok, err := appreviewverdict.GetActiveAcceptance(ctx, acceptances, repoFullName, 44)
	if err != nil {
		t.Fatalf("GetActiveAcceptance: error = %v, want nil", err)
	}
	if !ok || stillActive.Revoked() {
		t.Fatal("GetActiveAcceptance: ok = false or Revoked() = true, want an active, non-revoked row -- this test's own point is that the ancestor-chain change ALONE hides it from the read model, not revocation")
	}
}

// TestBuild_AcceptanceMergeable pins finding R1 (round 3, adversarial
// review): DecisionInboxItem.AcceptanceMergeable/
// AcceptanceMergeBlockedReason (buildPROpenItem, aggregate.go) must
// reflect the SAME mandatory, never-waived criteria RevalidateForMerge
// enforces at click time -- never derived from AcceptanceID's own
// presence alone (hasAcceptedOverride, decisionInboxFormat.ts's own
// PRE-FIX heuristic, web/src/session). Table-driven over the two
// illustrative failure modes the finding itself names ("the case with
// open review findings and the case whose own chip is rendered one span
// to the left" -- prChipData's own chip order, decisionInboxFormat.ts:
// risk/findings, CI, changes requested, THEN accepted override, i.e.
// hasChangesRequested is the chip immediately to its left) plus the
// genuinely-mergeable-via-acceptance baseline every case perturbs FROM.
//
// Mutation-test target: replacing this test's own AcceptanceMergeable
// assertions with a bare `acceptanceID != ""` check (the pre-fix
// behavior) must turn the two blocked subtests' own "not mergeable"
// assertion from a pass into a failure -- both would then read
// AcceptanceMergeable = true purely because the acceptance row exists.
func TestBuild_AcceptanceMergeable(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	tests := []struct {
		name                string
		openFinding         bool
		hasChangesRequested bool
		wantMergeable       bool
		wantReasonContains  string
	}{
		{
			name:          "clean PR, blocked only by Shippable -- mergeable via acceptance",
			wantMergeable: true,
		},
		{
			name:               "open review finding -- never waived, blocks despite acceptance",
			openFinding:        true,
			wantMergeable:      false,
			wantReasonContains: "review finding",
		},
		{
			name:                "changes requested -- never waived, blocks despite acceptance",
			hasChangesRequested: true,
			wantMergeable:       false,
			wantReasonContains:  "changes requested",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prNumber := int32(60 + i)
			actorGitHubExternalID := fmt.Sprintf("60%d", i)
			tokenKey := []byte("01234567890123456789012345678901")
			actor := decisionInboxActorFixture(ctx, t, pool, fmt.Sprintf("r1-actor-%d@example.com", i), actorGitHubExternalID, tokenKey)

			artifacts := narvipg.NewArtifactStore(pool)
			htmlURL := fmt.Sprintf("https://github.com/acme/widgets/pull/%d", prNumber)
			const repoFullName = "acme/widgets"
			platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
			if err != nil {
				t.Fatalf("create platform session: %v", err)
			}
			if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
				SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
			}); err != nil {
				t.Fatalf("mark PR #%d platform-authored: %v", prNumber, err)
			}

			reviewFindings := narvipg.NewReviewFindingStore(pool)
			if tc.openFinding {
				if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
					RepoFullName: repoFullName, PrNumber: prNumber, IdentityHash: "r1-open-finding",
					Severity: "high", FilePath: "internal/foo.go", Description: "a real, still-open finding",
				}); err != nil {
					t.Fatalf("seed open finding: %v", err)
				}
			}

			// A high-risk (not Shippable=auto) verdict, otherwise fully
			// eligible -- mirrors this file's own TestBuild_
			// AcceptedVerdict_BaseMoved_HidesStaleAcceptance fixture shape
			// exactly.
			reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
			repoSettingsStore := narvipg.NewRepoSettingsStore(pool)
			acceptances := narvipg.NewReviewVerdictAcceptanceStore(pool)
			verdict := review.Verdict{
				RiskLevel:         review.RiskLevelHigh,
				Premise:           review.PremiseStateOK,
				TestsCoverage:     review.TestsCoverageStateAdequate,
				DocsDrift:         review.DocsDriftStateNone,
				ProposedShippable: review.ProposedShippableNeedsHuman,
				FilesChanged:      3,
			}
			verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
			if verdict.Shippable == review.ShippableAuto {
				t.Fatalf("fixture bug -- RiskLevelHigh computed Shippable=auto, want anything else")
			}
			headSHA := fmt.Sprintf("sha-r1-%d", i)
			verdictContext := reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion}
			record, err := appreviewverdict.Insert(ctx, reviewVerdicts, repoSettingsStore, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded high-risk verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{})
			if err != nil {
				t.Fatalf("seed not-shippable-auto review_verdicts row: %v", err)
			}

			var verdictID pgtype.UUID
			if err := verdictID.Scan(record.ID); err != nil {
				t.Fatalf("scan verdict id: %v", err)
			}
			if _, _, err := appreviewverdict.Accept(ctx, acceptances, appreviewverdict.AcceptInput{
				RepoFullName:  repoFullName,
				PRNumber:      prNumber,
				VerdictID:     verdictID,
				AttemptID:     seedReviewAttemptTurn(ctx, t, pool),
				HeadSHA:       headSHA,
				Context:       verdictContext,
				Reason:        string(autoapproval.ReasonNotShippableAuto),
				Justification: "Accepted -- table-driven AcceptanceMergeable fixture.",
				AcceptedBy:    actor.ID,
			}); err != nil {
				t.Fatalf("Accept() error = %v, want nil", err)
			}

			fakeSCM := &fakeDecisionInboxSourceControl{
				openPRsByExternalID: map[string][]ports.OpenPR{
					actorGitHubExternalID: {
						{
							Owner: "acme", Repo: "widgets", Number: int(prNumber), Title: tc.name,
							HTMLURL: htmlURL, HeadSHA: headSHA,
							BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
							Assignees:           []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
							CIConclusion:        ports.CIConclusionSuccess,
							HasChangesRequested: tc.hasChangesRequested,
							CreatedAt:           time.Now(),
						},
					},
				},
			}

			deps := decisioninbox.Deps{
				Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
				Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
				ReviewFindings: reviewFindings, SentinelFixes: narvipg.NewSentinelFixStore(pool),
				Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
				SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
				TokenEncryptionKey: tokenKey,
				Timeouts:           platform.DefaultTimeouts(),
				ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: reviewVerdicts, RepoSettings: repoSettingsStore, ReviewFindings: reviewFindings, AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Acceptances: acceptances, Turns: narvipg.NewTurnStore(pool), Timeouts: platform.DefaultTimeouts()},
			}

			result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
			if err != nil {
				t.Fatalf("Build() error = %v, want nil", err)
			}

			item := findItemByPR(result.Items, int(prNumber))
			if item == nil {
				t.Fatalf("PR #%d missing from the inbox entirely", prNumber)
			}
			if item.Kind != decisioninboxdomain.KindNeedsReview {
				t.Fatalf("PR #%d Kind = %s, want needs_review (Kind stays acceptance-blind by design)", prNumber, item.Kind)
			}
			if item.AcceptanceID == "" {
				t.Fatalf("PR #%d AcceptanceID is empty, want the active acceptance's own id -- fixture bug, not what this test means to check", prNumber)
			}
			if item.AcceptanceMergeable != tc.wantMergeable {
				t.Errorf("PR #%d AcceptanceMergeable = %v, want %v (reason=%q)", prNumber, item.AcceptanceMergeable, tc.wantMergeable, item.AcceptanceMergeBlockedReason)
			}
			if tc.wantReasonContains != "" && !strings.Contains(item.AcceptanceMergeBlockedReason, tc.wantReasonContains) {
				t.Errorf("PR #%d AcceptanceMergeBlockedReason = %q, want it to contain %q", prNumber, item.AcceptanceMergeBlockedReason, tc.wantReasonContains)
			}
			if tc.wantMergeable && item.AcceptanceMergeBlockedReason != "" {
				t.Errorf("PR #%d AcceptanceMergeBlockedReason = %q, want empty when AcceptanceMergeable is true", prNumber, item.AcceptanceMergeBlockedReason)
			}
		})
	}
}

// TestBuild_ChangedFilesListDegraded_NeverReadyToMerge is computeRealEligibility's
// own (aggregate.go) Phase 5 audit finding 1 regression test -- the SAME
// "otherwise fully eligible" fixture shape as
// TestBuild_HasChangesRequestedDemotesFromReadyToMerge immediately above,
// but perturbing ports.OpenPR.ChangedFilesListDegraded instead of
// HasChangesRequested: a swallowed changed-files fetch error (githubapi's
// own filesErr != nil path) must demote this PR out of ready_to_merge,
// never silently read as "confirmed zero files, nothing sensitive".
// Mutation-test target: reverting computeRealEligibility's own
// `TouchedBlastRadiusKnown: touchedBlastRadiusKnown` wiring (aggregate.go)
// back to always-true (or omitting the field) must turn this test's own
// KindReadyToMerge assertion from a failure back into a pass.
func TestBuild_ChangedFilesListDegraded_NeverReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "5001"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "phase5-actor@example.com", actorGitHubExternalID, tokenKey)

	artifacts := narvipg.NewArtifactStore(pool)
	const htmlURL = "https://github.com/acme/widgets/pull/50"
	platformSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: platformSession.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark PR #50 platform-authored: %v", err)
	}
	seedAutoApprovedVerdict(ctx, t, pool, "acme/widgets", 50, "sha50")

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 50, Title: "otherwise fully eligible, but the changed-files read was degraded",
					HTMLURL: htmlURL, HeadSHA: "sha50",
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk"},
					// ChangedFiles/ChangedFilesCount stay at their own
					// honest zero values -- exactly what githubapi still
					// reports on a fetch failure today. Degraded=true
					// ALONE is what this test proves must demote the PR;
					// a coincidentally-empty ChangedFiles is never, on its
					// own, distinguishable from a genuinely clean PR
					// without this flag.
					ChangedFilesListDegraded: true,
					CreatedAt:                time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: artifacts, Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	pr50 := findItemByPR(result.Items, 50)
	if pr50 == nil {
		t.Fatal("PR #50 missing from the inbox entirely")
	}
	if pr50.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("PR #50 (changed-files read degraded) Kind = %s, want needs_review -- a swallowed/degraded GitHub changed-files fetch must never silently render as ready_to_merge", pr50.Kind)
	}
}

// TestBuild_CodeOwnersResolvedAgainstBaseRefNeverHead is the B3 regression
// test named explicitly to close a real gap: "reverting Ref:
// pr.BaseRef -> pr.HeadSHA... passes everything" because no test captured
// the actual ResolveCodeOwnersSpec resolvePRProvenance builds. HeadSHA is
// deliberately a completely different, attacker-shaped string from
// BaseRef here, so a regression back to the PR's own head is
// unmistakable, never a coincidental match.
func TestBuild_CodeOwnersResolvedAgainstBaseRefNeverHead(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubExternalID = "3001"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "b3-baseref-actor@example.com", actorGitHubExternalID, tokenKey)

	const wantBaseRef = "release/1.0"
	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 30, Title: "codeowners base-ref check",
					HTMLURL: "https://github.com/acme/widgets/pull/30",
					// HeadSHA is deliberately attacker-shaped and distinct
					// from BaseRef -- if resolvePRProvenance ever regresses
					// to resolving CODEOWNERS at the PR's own head, the assertion below catches it
					// immediately.
					HeadSHA:      "attacker-controlled-head-sha",
					BaseRef:      wantBaseRef,
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk"},
					ChangedFiles: []string{"internal/app/scheduler/backoff.go"},
					CreatedAt:    time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	if _, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now()); err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	if len(fakeSCM.codeOwnersCalls) == 0 {
		t.Fatal("ResolveCodeOwners was never called -- test fixture problem, cannot verify Ref")
	}
	for _, call := range fakeSCM.codeOwnersCalls {
		if call.Ref != wantBaseRef {
			t.Errorf("ResolveCodeOwners called with Ref = %q, want the PR's own BASE ref %q (never HeadSHA --)", call.Ref, wantBaseRef)
		}
	}
}

// TestBuild_SCMFetchFailedSignal proves Result.SCMFetchFailed actually
// becomes true for its own producers (
// second round; SCMFetchFailed=true
// had zero test coverage -- the fake hardcoded truncated=false and never
// errored, so a mutation dropping the wiring entirely passed the whole
// suite). Each subtest isolates ONE producer.
func TestBuild_SCMFetchFailedSignal(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")

	t.Run("TruncatedRead_StillSurfacesPartialItemsWithARealAsOf", func(t *testing.T) {
		const actorGitHubExternalID = "5001"
		actor := decisionInboxActorFixture(ctx, t, pool, "c1-truncated-actor@example.com", actorGitHubExternalID, tokenKey)

		fakeSCM := &fakeDecisionInboxSourceControl{
			openPRsByExternalID: map[string][]ports.OpenPR{
				actorGitHubExternalID: {
					{Owner: "acme", Repo: "widgets", Number: 50, Title: "partial read", HTMLURL: "https://github.com/acme/widgets/pull/50", HeadSHA: "sha50", CreatedAt: time.Now()},
				},
			},
			openPRsTruncated: true,
		}
		deps := decisioninbox.Deps{
			Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
			Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
			Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
			SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
			TokenEncryptionKey: tokenKey,
			Timeouts:           platform.DefaultTimeouts(),
			ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
		}

		result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
		if err != nil {
			t.Fatalf("Build() error = %v, want nil", err)
		}
		if !result.SCMFetchFailed {
			t.Error("SCMFetchFailed = false, want true (the underlying SourceControl read reported truncated=true)")
		}
		if result.SCMAsOf == nil {
			t.Error("SCMAsOf = nil, want non-nil -- a truncated read is still a REAL, if partial, fetch: SCMAsOf and SCMFetchFailed are no longer mutually exclusive")
		}
	})

	t.Run("UnderlyingFetchError_NoAsOfNoItems", func(t *testing.T) {
		const actorGitHubExternalID = "5002"
		actor := decisionInboxActorFixture(ctx, t, pool, "c1-error-actor@example.com", actorGitHubExternalID, tokenKey)

		fakeSCM := &fakeDecisionInboxSourceControl{openPRsErr: errors.New("boom: github is down")}
		deps := decisioninbox.Deps{
			Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
			Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
			Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
			SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
			TokenEncryptionKey: tokenKey,
			Timeouts:           platform.DefaultTimeouts(),
			ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
		}

		result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
		if err != nil {
			t.Fatalf("Build() error = %v, want nil (a failed PR fetch must not fail the whole Build call)", err)
		}
		if !result.SCMFetchFailed {
			t.Error("SCMFetchFailed = false, want true (the underlying SourceControl call errored outright)")
		}
		if result.SCMAsOf != nil {
			t.Errorf("SCMAsOf = %v, want nil -- no fetch ever completed", *result.SCMAsOf)
		}
	})
}

// TestBuild_SentinelFixStoreErrorDegradesTheReadButNeverPanics is the
// P1-3 regression test: a genuine SentinelFixes
// store error inside buildPRItems' per-PR loop must both (1) exclude ONLY
// that one PR row (fail closed, exactly as before this fix) and (2) mark
// the overall read degraded via Result.SCMFetchFailed, rather than
// silently rendering a fresh, complete, empty-of-that-row queue. The
// SentinelFixes store is deliberately built on an ALREADY-ROLLED-BACK
// transaction (mirrors this same package's own eligiblePR/WithTx
// precedent, revalidate_integration_test.go) -- a real, reliable way to
// force exactly ONE dependency to fail without touching the healthy pool
// every other store in this same Build call still needs.
func TestBuild_SentinelFixStoreErrorDegradesTheReadButNeverPanics(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")

	const actorGitHubExternalID = "5003"
	actor := decisionInboxActorFixture(ctx, t, pool, "p1-3-actor@example.com", actorGitHubExternalID, tokenKey)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	// brokenSentinelFixes wraps the now-closed tx above -- every query
	// through it fails immediately with a real Postgres/pgx error, without
	// any real outage or a second container.
	brokenSentinelFixes := narvipg.NewSentinelFixStore(pool).WithTx(tx)

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: "acme", Repo: "widgets", Number: 60, Title: "sentinel-fix check errors",
					HTMLURL: "https://github.com/acme/widgets/pull/60", HeadSHA: "sha60",
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk"},
					CreatedAt:    time.Now(),
				},
			},
		},
	}

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: brokenSentinelFixes,
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (a per-PR store error must degrade, never fail, the whole Build call)", err)
	}
	if item := findItemByPR(result.Items, 60); item != nil {
		t.Errorf("PR #60 present in the inbox (%+v), want excluded -- the §17 exclusion check must fail CLOSED on a store error", item)
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- a per-PR SentinelFixStore error must degrade the overall read, never silently render a fresh, complete queue with that row simply missing")
	}
}

// TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub
// is the P2-1 regression test: a genuine
// identity-store error resolving the actor's OWN GitHub credential must
// route into the SAME degraded signal as P1-2/P1-3, never collapse into
// the identical, indistinguishable ok=false empty state "no GitHub linked
// at all" renders. Uses the same already-rolled-back-tx fault injection
// as the SentinelFixStore test above, applied to Identities instead.
func TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")

	users := narvipg.NewUserStore(pool)
	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "p2-1-actor@example.com", DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	brokenIdentities := narvipg.NewIdentityStore(pool).WithTx(tx)

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: brokenIdentities,
		SCMCache:           decisioninbox.NewSCMCache(&fakeDecisionInboxSourceControl{}, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict:      appreviewverdict.Deps{ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool), ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool), Timeouts: platform.DefaultTimeouts()},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (a credential-resolution error must degrade, never fail, the whole Build call)", err)
	}
	if result.SCMAsOf != nil {
		t.Errorf("SCMAsOf = %v, want nil -- no fetch was ever attempted", *result.SCMAsOf)
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- a genuine identity-store error must never render identically to \"no GitHub linked at all\"")
	}
}

// buildEligibleReadyToMergeFixture seeds a fully platform-authored,
// CI-green, low-risk, auto-approved-eligible PR for (repoFullName,
// prNumber) plus its actor -- the shared baseline both C3/C4 regression
// tests below start from, mirroring eligiblePR's own recipe
// (revalidate_integration_test.go, same package) but inlined here since
// this file's own existing fixtures (immediately above) already follow
// this exact "construct everything inline" convention rather than
// reaching across files.
func buildEligibleReadyToMergeFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tokenKey []byte, primaryEmail, actorGitHubExternalID, repoFullName string, prNumber int) (sqlcgen.User, *fakeDecisionInboxSourceControl) {
	t.Helper()

	actor := decisionInboxActorFixture(ctx, t, pool, primaryEmail, actorGitHubExternalID, tokenKey)

	owner, repo, ok := reposource.SplitFullName(repoFullName)
	if !ok {
		t.Fatalf("malformed repoFullName %q", repoFullName)
	}
	htmlURL := "https://github.com/" + repoFullName + "/pull/" + itoaTest(prNumber)
	headSHA := "sha-" + itoaTest(prNumber)

	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := narvipg.NewArtifactStore(pool).Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}
	seedAutoApprovedVerdict(ctx, t, pool, repoFullName, int32(prNumber), headSHA)

	fakeSCM := &fakeDecisionInboxSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			actorGitHubExternalID: {
				{
					Owner: owner, Repo: repo, Number: prNumber, Title: "eligible pr",
					HTMLURL: htmlURL, HeadSHA: headSHA,
					BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
					Assignees:    []ports.PRPerson{{ExternalID: actorGitHubExternalID, Login: "actor"}},
					CIConclusion: ports.CIConclusionSuccess,
					Labels:       []string{"review:low-risk"},
					CreatedAt:    time.Now(),
				},
			},
		},
	}
	return actor, fakeSCM
}

// itoaTest is a tiny, dependency-free int->string helper for building
// fixture URLs/SHAs above -- avoids pulling in strconv purely for this.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestBuild_EligibilityConfigStoreError_DemotesFromReadyToMerge is the C3
// regression test at the READ-MODEL level: an
// otherwise-fully-eligible PR must be demoted to needs_review, never
// rendered ready_to_merge, when this repo's own §21.2 eligibility config
// cannot be read (a genuine, non-ErrNoRows repo_settings error) --
// computeRealEligibility's own doc comment. Uses the SAME
// already-rolled-back-tx fault injection this file's own
// TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub
// establishes, applied to ReviewVerdict.RepoSettings instead of
// Identities.
func TestBuild_EligibilityConfigStoreError_DemotesFromReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5004"
	const repoFullName = "acme/build-eligibility-config-error"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "c3-actor@example.com", actorGitHubExternalID, repoFullName, 61)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	brokenRepoSettings := narvipg.NewRepoSettingsStore(pool).WithTx(tx)

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: brokenRepoSettings,
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (an eligibility-config store error must degrade ONE row, never fail the whole Build call)", err)
	}
	item := findItemByPR(result.Items, 61)
	if item == nil {
		t.Fatal("PR #61 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- an eligibility-config store error must fail CLOSED (never substitute the engine's own wider defaults for this repo's own configured policy)")
	}
}

// TestBuild_LiveSCMLookupFails_MarksSCMFetchFailed is E5's own regression
// test (third adversarial-review round): before this fix, a per-PR live
// SCM lookup failure inside computeRealEligibility (here,
// SCMCache.ResolveBranchSHA's own base-branch-tip resolution) demoted the
// row out of ready_to_merge via ReasonBaseSHAUnknown's own fail-closed
// path with NO way for the caller to tell that apart from a genuine,
// considered "not eligible" judgement -- Result.SCMFetchFailed stayed
// false throughout, so the row rendered as a confident normal state when
// the truth was a failed GitHub call, exactly the "failure rendering as
// a confident normal state" shape this codebase has repeatedly had to
// fix elsewhere (Result.SCMFetchFailed's own producer list, producers
// 1-5, aggregate.go). Uses the SAME fault-injection shape as
// TestBuild_EligibilityConfigStoreError_DemotesFromReadyToMerge above,
// applied to the fake SourceControl's own ResolveBranchSHA instead of a
// broken Postgres store.
func TestBuild_LiveSCMLookupFails_MarksSCMFetchFailed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5009"
	const repoFullName = "acme/build-live-scm-lookup-fails"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "e5-actor@example.com", actorGitHubExternalID, repoFullName, 73)
	fakeSCM.resolveBranchSHAErr = errors.New("boom: github is down")

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (a live SCM lookup failure must degrade ONE row, never fail the whole Build call)", err)
	}
	item := findItemByPR(result.Items, 73)
	if item == nil {
		t.Fatal("PR #73 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- a failed live base-branch-tip resolution must fail CLOSED via ReasonBaseSHAUnknown")
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- E5: a live SCM lookup failure inside computeRealEligibility must mark the whole read degraded, or this row renders as a considered ineligibility judgement rather than the truth (a lookup failed)")
	}
}

// TestBuild_IsAncestorLookupFails_MarksSCMFetchFailed is G9's own
// regression test (fourth adversarial-review round): the IDENTICAL E5
// degraded signal as TestBuild_LiveSCMLookupFails_MarksSCMFetchFailed
// immediately above, but for computeRealEligibility's OTHER live SCM
// lookup -- the fast-forward-ancestry confirmation (deps.SCMCache.
// IsAncestor), which producer (6)'s own doc comment (Result.
// SCMFetchFailed) has always named alongside ResolveBranchSHA but which,
// before this test, had no regression coverage of its own at all. The
// base branch's live tip is made to genuinely differ from the verdict's
// own recorded base sha (resolveBranchSHA overridden, mirroring
// TestBuild_BaseBranchAdvanced_LiveTipMoved_DemotesFromReadyToMerge
// below) so the ancestor-check branch is actually entered, and the
// ancestor check itself then fails (isAncestorErr set) rather than
// confirming or refuting the movement.
func TestBuild_IsAncestorLookupFails_MarksSCMFetchFailed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5010"
	const repoFullName = "acme/build-is-ancestor-lookup-fails"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "g9-actor@example.com", actorGitHubExternalID, repoFullName, 74)
	fakeSCM.resolveBranchSHA = "sha-main-has-actually-advanced"
	fakeSCM.isAncestorErr = errors.New("boom: github is down")

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil (a live SCM lookup failure must degrade ONE row, never fail the whole Build call)", err)
	}
	item := findItemByPR(result.Items, 74)
	if item == nil {
		t.Fatal("PR #74 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- a failed live fast-forward-ancestry confirmation must fail CLOSED via ReasonBaseMoved")
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- G9/E5: a failed IsAncestor call inside computeRealEligibility must mark the whole read degraded, exactly like a failed ResolveBranchSHA call already does, or this row renders as a considered ineligibility judgement rather than the truth (a lookup failed)")
	}
	if len(fakeSCM.isAncestorCalls) != 1 {
		t.Fatalf("IsAncestor called %d times, want 1 -- the ancestor-check branch must actually have been entered for this test to exercise anything", len(fakeSCM.isAncestorCalls))
	}
}

// TestBuild_BaseBranchAdvanced_ButAlreadyIneligibleForAnotherReason is
// G10's own regression test (fourth adversarial-review round): this PR's
// CI is red (a criterion computeRealEligibility's own probe checks
// WITHOUT any live SCM call) AND its base SHA has moved in a way whose
// live ancestor confirmation would fail if it were ever attempted.
// Before this fix, computeRealEligibility called ResolveBranchSHA/
// IsAncestor unconditionally, so a live-call failure marked
// Result.SCMFetchFailed degraded even though CI-red alone already
// guaranteed this row could never render ready_to_merge -- exactly the
// "base-SHA lookups that could not have affected the row" producer (6)'s
// own doc comment now says must not raise this signal. isAncestorErr is
// set specifically so that if the probe is ever bypassed or removed, the
// live call would fail and mark SCMFetchFailed=true instead of this
// test's own expected false, making a regression to the pre-G10
// behavior visible as a wrong degraded signal, not merely a silent
// behavior change.
func TestBuild_BaseBranchAdvanced_ButAlreadyIneligibleForAnotherReason(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5011"
	const repoFullName = "acme/build-base-advanced-and-ci-red"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "g10-actor@example.com", actorGitHubExternalID, repoFullName, 76)
	fakeSCM.openPRsByExternalID[actorGitHubExternalID][0].CIConclusion = ports.CIConclusionFailure
	fakeSCM.resolveBranchSHA = "sha-main-has-actually-advanced"
	fakeSCM.isAncestorErr = errors.New("boom: github is down")

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 76)
	if item == nil {
		t.Fatal("PR #76 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- CI is red")
	}
	if result.SCMFetchFailed {
		t.Error("SCMFetchFailed = true, want false -- G10: CI-red alone already refuses this PR, independent of the base-SHA question, so the live ResolveBranchSHA/IsAncestor calls could never have changed this row's own fate; their failure must not raise the inbox-wide degraded signal")
	}
	if len(fakeSCM.isAncestorCalls) != 0 {
		t.Errorf("IsAncestor called %d times, want 0 -- G10: a PR already ineligible on an independent, fully-known criterion must never spend a live ancestor-confirmation call that could not have changed the outcome", len(fakeSCM.isAncestorCalls))
	}
}

// TestBuild_CIConclusionDegraded_ProbeCatchesItBeforeAnyLiveCall is
// G10's own regression shape (immediately above), applied to
// ports.OpenPR.CIConclusionDegraded specifically: computeRealEligibility
// builds TWO EligibilityInput literals from the identical pr (the probe,
// checked before any live SCM call, and the final literal, checked
// after) -- both must carry CIConclusionDegraded, mirroring
// revalidateCore's own identical two-literal shape
// (revalidate_integration_test.go's own
// TestRevalidateForMerge_CIConclusionDegraded_ProbeCatchesItBeforeAnyLiveCall).
// CIConclusion is left at its own confirmed CIConclusionSuccess (the
// fixture's baseline) so this test isolates CIConclusionDegraded alone,
// exactly like that sibling test does. resolveBranchSHAErr forces the
// LIVE base-branch-tip resolution to fail -- if the probe does not ALSO
// carry CIConclusionDegraded, that live call actually runs.
//
// F2/F4 changed what this test can observe: buildPRItems
// now ALSO raises Result.SCMFetchFailed directly off pr.CIConclusionDegraded
// (producer (7) on that field's own doc comment), independent of whether
// computeRealEligibility's probe/final calls ever run at all -- so
// SCMFetchFailed reads true in this test regardless of the mutation below,
// and can no longer be the signal that proves the probe's own
// short-circuit. Mutation-test target, EXECUTION-VERIFIED (dropping
// CIConclusionDegraded from the probe literal specifically, aggregate.go,
// leaving the final literal and buildPRItems' own producer-(7) check
// untouched): item.Kind stays needs_review (the FINAL literal still
// carries pr.CIConclusionDegraded=true and refuses on its own) and
// SCMFetchFailed stays true (producer (7) fires independently of either
// EligibilityInput literal) -- neither flips. Only the ResolveBranchSHA
// call-count assertion below turns from a pass into a failure: the probe
// no longer refuses on CIConclusionDegraded, so its own early return
// (autoapproval.ComputeEligible(probe, cfg) with CIConclusionDegraded
// unset) no longer fires, and the LIVE base-branch-tip resolution
// actually runs.
func TestBuild_CIConclusionDegraded_ProbeCatchesItBeforeAnyLiveCall(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5012"
	const repoFullName = "acme/build-ci-conclusion-degraded-probe"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "ci-degraded-probe-actor@example.com", actorGitHubExternalID, repoFullName, 77)
	fakeSCM.openPRsByExternalID[actorGitHubExternalID][0].CIConclusionDegraded = true
	fakeSCM.resolveBranchSHAErr = errors.New("boom: github is down")

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 77)
	if item == nil {
		t.Fatal("PR #77 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- CIConclusionDegraded is true")
	}
	// F2/F4: SCMFetchFailed must now read true -- pr.
	// CIConclusionDegraded IS itself an incomplete-SCM-read fact
	// (buildPRItems' own producer (7), aggregate.go), independent of
	// whatever computeRealEligibility's own live calls do or don't do.
	// Before F2/F4, this assertion read the OPPOSITE way ("want false"),
	// on the reasoning that the live ResolveBranchSHA call never ran so
	// nothing degraded the read -- true as far as it went, but conflated
	// "the live call never ran" with "the read was complete", which it
	// was not: the CI composite itself was only ever half read. That
	// reasoning is exactly what F2/F4 found wrong.
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- pr.CIConclusionDegraded is itself an incomplete-SCM-read fact and must raise this field regardless of what the live base-branch lookup below does")
	}
	// The ORIGINAL property this test exists for -- the probe refuses
	// BEFORE ever attempting the live ResolveBranchSHA call -- is now
	// proven by the call count directly, since SCMFetchFailed's own value
	// can no longer distinguish it (it is true either way after the fix
	// above). resolveBranchSHAErr is still armed: if the probe did NOT
	// carry CIConclusionDegraded, this call would still run and its
	// failure would set currentBaseSHA = "" but would NOT be observable
	// via SCMFetchFailed anymore either (masked by producer (7)) -- this
	// call count is now the ONLY way to prove the probe's own
	// short-circuit still holds.
	if len(fakeSCM.resolveBranchSHACalls) != 0 {
		t.Errorf("ResolveBranchSHA called %d times, want 0 -- the probe's own CIConclusionDegraded refusal already refuses this PR, independent of the base-SHA question, so the live ResolveBranchSHA call could never have changed this row's own fate", len(fakeSCM.resolveBranchSHACalls))
	}
}

// TestBuild_CIConclusionDegraded_FinalLiteralWiredFromRealValue is F3/F9's
// own regression test: of computeRealEligibility's two
// EligibilityInput literals (the probe, above, and the FINAL one, checked
// after the live base-branch lookup succeeds), only the probe's own
// CIConclusionDegraded wiring had a dedicated test before this one -- the
// final literal's identical field (aggregate.go) was covered by no test at
// all.
//
// EXECUTION-VERIFIED, not assumed (this project's own standing rule):
// hardcoding the probe's CIConclusionDegraded to a REAL PR's own true
// value while the fixture's underlying pr.CIConclusionDegraded is false
// cannot be tested from outside this package by dropping the field (Go's
// own zero value for a bool IS false, so "dropped" and "false" are the
// same bit pattern) -- and dropping it from the FINAL literal specifically
// is, ITSELF, unobservable by any test: whenever the real value is true,
// the probe (built from the SAME pr.CIConclusionDegraded, checked first)
// already refuses via ComputeEligible's own CIConclusionDegraded check,
// and computeRealEligibility returns before ever constructing the final
// literal -- confirmed by running the full package suite (`go test
// -tags=integration`) against that exact mutation: zero tests failed. Only
// the OPPOSITE-directioned mistake -- the final literal wired to the
// WRONG value (a hardcoded/miswired constant instead of pr.
// CIConclusionDegraded) -- is reachable, and only when the real value is
// false, which is this test's own scenario. Confirmed the same way:
// hardcoding the final literal's CIConclusionDegraded to `true`
// unconditionally breaks this test (and, incidentally,
// TestBuild_FullScenario and several other existing happy-path tests that
// were never written with this field in mind) -- proving the final
// literal really is read from pr.CIConclusionDegraded and not some other
// source. Mutation-test target: hardcoding the final literal
// (aggregate.go) to `true` must turn this test from a pass into a
// failure; hardcoding it (or dropping the field) to `false` must not,
// since that is this test's own already-true baseline.
func TestBuild_CIConclusionDegraded_FinalLiteralWiredFromRealValue(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5013"
	const repoFullName = "acme/build-ci-conclusion-degraded-final-literal"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "ci-degraded-final-literal-actor@example.com", actorGitHubExternalID, repoFullName, 78)
	// CIConclusionDegraded is left at its own zero value (false) --
	// buildEligibleReadyToMergeFixture's own otherwise-fully-eligible
	// baseline, unperturbed. If the FINAL literal read anything other
	// than this real, false value, ComputeEligible would refuse via
	// ReasonCIConclusionDegraded and this PR would never reach
	// ready_to_merge.

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 78)
	if item == nil {
		t.Fatal("PR #78 missing from the inbox entirely, want present as ready_to_merge")
	}
	if item.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Errorf("Kind = %v, want ready_to_merge -- the FINAL EligibilityInput literal's own CIConclusionDegraded must read this PR's real (false) value, never a hardcoded/miswired constant", item.Kind)
	}
	if result.SCMFetchFailed {
		t.Error("SCMFetchFailed = true, want false -- nothing about this fixture is degraded")
	}
}

// TestBuild_BaseBranchAdvanced_LiveTipMoved_DemotesFromReadyToMerge is D2's
// own regression test (second adversarial-review round) at the
// READ-MODEL level -- computeRealEligibility's (aggregate.go) own sibling
// of revalidate_integration_test.go's identical
// "BaseBranchAdvanced_LiveTipMovedWhileGitHubsCachedBaseSHAFieldDidNot_Refused"
// case for RevalidateForMerge. Before this fix, computeRealEligibility
// was the LAST remaining ComputeEligible call site still comparing
// pr.BaseSHA (GitHub's own per-PR CACHED snapshot, frozen here at
// testEligibleBaseSHA, matching the verdict's own recorded context
// EXACTLY) against itself -- detecting nothing, no matter how far the
// base branch's real tip had actually moved, so a PR reviewed while based
// on another PR's branch (then retargeted) or whose parent moved beneath
// it could render ready_to_merge in the inbox from a stale verdict. The
// base ref and GitHub's own cached base.sha field are BOTH left exactly
// as buildEligibleReadyToMergeFixture seeds them -- only the fake's own
// live ResolveBranchSHA response (what SCMCache.ResolveBranchSHA now
// calls through to) reports a different commit, simulating the real
// branch tip having advanced.
func TestBuild_BaseBranchAdvanced_LiveTipMoved_DemotesFromReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5005"
	const repoFullName = "acme/build-base-branch-advanced"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "d2-actor@example.com", actorGitHubExternalID, repoFullName, 62)

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	// Positive control, proving the fixture itself is genuinely
	// ready_to_merge-eligible before this test's own perturbation --
	// otherwise a broken fixture demoting it for some UNRELATED reason
	// would pass this test for the wrong one.
	t0 := time.Now()
	before, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, t0)
	if err != nil {
		t.Fatalf("Build() (control) error = %v, want nil", err)
	}
	itemBefore := findItemByPR(before.Items, 62)
	if itemBefore == nil || itemBefore.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Fatalf("precondition: PR #62 Kind = %v, want ready_to_merge before the base branch perturbation below", itemBefore)
	}

	fakeSCM.resolveBranchSHA = "sha-main-has-actually-advanced"
	// t1 is deliberately past DecisionInboxSCMCacheTTL from t0: deps.
	// SCMCache is the SAME instance across both Build calls (exactly like
	// production, where one long-lived cache serves many requests), so a
	// second call within the TTL would silently serve the FIRST call's
	// own already-cached ResolveBranchSHA result regardless of what the
	// fake now reports -- this advances the clock far enough that the
	// cached entry has genuinely expired, forcing a fresh live read.
	t1 := t0.Add(platform.DefaultTimeouts().DecisionInboxSCMCacheTTL + time.Minute)

	after, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, t1)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	itemAfter := findItemByPR(after.Items, 62)
	if itemAfter == nil {
		t.Fatal("PR #62 missing from the inbox entirely, want present as needs_review")
	}
	if itemAfter.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- D2: the base branch's real live tip advanced while GitHub's own cached base.sha field and the base ref both stayed frozen; comparing the cached field against itself detects nothing, so this read model must resolve the LIVE tip exactly like RevalidateForMerge already does")
	}
}

// TestBuild_BaseBranchAdvanced_ConfirmedFastForward_StaysReadyToMerge is
// D3's own regression test (second adversarial-review round) at the
// READ-MODEL level -- computeRealEligibility's (aggregate.go) own sibling
// of revalidate_integration_test.go's identical
// "BaseBranchAdvanced_ConfirmedFastForward_NotRefused" case. The EXACT
// SAME base-tip movement as TestBuild_BaseBranchAdvanced_LiveTipMoved_
// DemotesFromReadyToMerge above, but this time the fake's own IsAncestor
// call CONFIRMS the movement was a pure fast-forward (an ordinary,
// unrelated merge landing on the base branch) -- the PR must NOT be
// demoted. Without this, "any unrelated merge to trunk permanently
// disqualifies a verdict" (D3's own named failure) would make auto-merge
// effectively never fire in an active repository, since the inbox would
// never even OFFER the row as ready_to_merge for a human to confirm.
func TestBuild_BaseBranchAdvanced_ConfirmedFastForward_StaysReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5006"
	const repoFullName = "acme/build-base-branch-advanced-confirmed"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "d3-actor@example.com", actorGitHubExternalID, repoFullName, 63)

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	t0 := time.Now()
	before, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, t0)
	if err != nil {
		t.Fatalf("Build() (control) error = %v, want nil", err)
	}
	itemBefore := findItemByPR(before.Items, 63)
	if itemBefore == nil || itemBefore.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Fatalf("precondition: PR #63 Kind = %v, want ready_to_merge before the base branch perturbation below", itemBefore)
	}

	fakeSCM.resolveBranchSHA = "sha-main-has-actually-advanced"
	fakeSCM.isAncestorResult = true
	// t1 past DecisionInboxSCMCacheTTL, exactly like the sibling test
	// above -- deps.SCMCache is the SAME instance across both Build
	// calls, so both the ResolveBranchSHA AND IsAncestor caches need this
	// to genuinely re-fetch.
	t1 := t0.Add(platform.DefaultTimeouts().DecisionInboxSCMCacheTTL + time.Minute)

	after, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, t1)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	itemAfter := findItemByPR(after.Items, 63)
	if itemAfter == nil {
		t.Fatal("PR #63 missing from the inbox entirely, want present")
	}
	if itemAfter.Kind != decisioninboxdomain.KindReadyToMerge {
		t.Errorf("Kind = %v, want ready_to_merge -- D3: a base movement CONFIRMED as a pure fast-forward (an unrelated merge to trunk) must not demote an otherwise-eligible PR out of ready_to_merge", itemAfter.Kind)
	}
	if len(fakeSCM.isAncestorCalls) == 0 {
		t.Error("IsAncestor was never called -- the ancestry-tolerance check must actually run when the base sha differs under an unchanged ref")
	}
}

// TestBuild_ReviewDecisionDegraded_DemotesFromReadyToMerge is the C4
// regression test at the READ-MODEL level: a
// degraded review-decision read (ports.OpenPR.ReviewDecisionDegraded)
// must demote an otherwise-fully-eligible PR out of ready_to_merge, and
// must mark the overall read SCMFetchFailed -- "we could not tell" must
// never render identically to "no changes requested".
func TestBuild_ReviewDecisionDegraded_DemotesFromReadyToMerge(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5005"
	const repoFullName = "acme/build-review-decision-degraded"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "c4-actor@example.com", actorGitHubExternalID, repoFullName, 62)
	// Perturb the ONE fact this test exercises -- HasChangesRequested
	// stays false (its own honest zero value), proving the demotion fires
	// on ReviewDecisionDegraded alone.
	prs := fakeSCM.openPRsByExternalID[actorGitHubExternalID]
	prs[0].ReviewDecisionDegraded = true
	fakeSCM.openPRsByExternalID[actorGitHubExternalID] = prs

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 62)
	if item == nil {
		t.Fatal("PR #62 missing from the inbox entirely, want present as needs_review")
	}
	if item.Kind == decisioninboxdomain.KindReadyToMerge {
		t.Error("Kind = ready_to_merge, want needs_review -- a degraded review-decision read must never render as the all-clear ready_to_merge promises")
	}
	if !result.SCMFetchFailed {
		t.Error("SCMFetchFailed = false, want true -- a per-PR degraded review-decision read must mark the overall read incomplete")
	}
}

// countAutoApprovalOutcomes returns (total, contested) for repoFullName
// within the last hour -- the shared assertion helper every T1 test below
// uses to confirm whether RecordOverridden actually fired.
// countAutoApprovalOutcomes reads through the CALIBRATION query, which
// §30.7 requires to exclude shadow-era outcomes -- so a repo with no
// repo_settings row (shadow by default) counts zero however many outcomes
// were recorded. The tests below are about whether an outcome is recorded
// at all, not about shadow semantics, so they arm the repo live first.
func countAutoApprovalOutcomes(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string) (total, contested int64) {
	t.Helper()
	total, contested, err := narvipg.NewAutoApprovalOutcomeStore(pool).CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	return total, contested
}

// TestBuild_Contested_HasChangesRequestedHalf_RecordsOverridden is the T1
// regression test for the HALF of the
// contested guard that was PROVABLY BROKEN before this fix:
// computeRealEligibility's own OLD code computed `eligible` gating on
// HasNeedsHumanLabel (a real ComputeEligible input), then only entered
// the RecordOverridden check when `!eligible` -- but
// pr.HasChangesRequested is NOT a ComputeEligible input at all, so a PR
// the engine would have approved on every REAL criterion, with
// hasNeedsHuman false and HasChangesRequested true, produced `eligible
// == true` from that first call, `!eligible` was FALSE, and
// RecordOverridden was UNREACHABLE for this exact population -- the
// contradiction-rate metric's own "a human overrode the engine via
// changes-requested" half could never fire, no matter how many real PRs
// hit it. This test builds exactly that fixture and asserts a
// 'overridden' outcome IS now recorded.
func TestBuild_Contested_HasChangesRequestedHalf_RecordsOverridden(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5006"
	const repoFullName = "acme/t1-contested-changes-requested"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "t1-changes-requested@example.com", actorGitHubExternalID, repoFullName, 70)
	// §30.7 stamps each recorded outcome with the egress mode that held
	// when it was OBSERVED, and the calibration query excludes shadow ones.
	// A repo with no repo_settings row resolves shadow, so this must be
	// armed BEFORE the outcome is recorded -- arming afterwards cannot
	// change a stamp already written. These tests are about whether an
	// outcome is recorded at all, not about egress mode.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}
	// The ONE fact under test: HasChangesRequested true, hasNeedsHuman
	// (no review:needs-human label) stays false -- isolates this half
	// from the OTHER half TestBuild_Contested_NeedsHumanLabelHalf_
	// RecordsOverridden below covers.
	prs := fakeSCM.openPRsByExternalID[actorGitHubExternalID]
	prs[0].HasChangesRequested = true
	fakeSCM.openPRsByExternalID[actorGitHubExternalID] = prs

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 70)
	if item == nil {
		t.Fatal("PR #70 missing from the inbox entirely")
	}
	if item.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("Kind = %q, want needs_review (HasChangesRequested demotes it)", item.Kind)
	}

	total, contested := countAutoApprovalOutcomes(ctx, t, pool, repoFullName)
	if total != 1 || contested != 1 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (1, 1) -- the engine would have approved this PR on every real criterion, but a reviewer requested changes: this MUST record 'overridden' (this exact half was previously unreachable)", total, contested)
	}
}

// TestBuild_Contested_NeedsHumanLabelHalf_RecordsOverridden is
// TestBuild_Contested_HasChangesRequestedHalf_RecordsOverridden's own
// sibling, covering the OTHER half of §21.2's own definition of
// "contested": a review:needs-human label applied to a PR the engine
// would otherwise have approved. This half already worked before the T1
// fix (HasNeedsHumanLabel is a real ComputeEligible input) -- kept here
// as this guard's OWN positive-case regression test, so both halves the
// task's own report explicitly asks for are covered in the same place,
// and so a future refactor of computeRealEligibility can't silently
// re-break this half while "fixing" the other.
func TestBuild_Contested_NeedsHumanLabelHalf_RecordsOverridden(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5007"
	const repoFullName = "acme/t1-contested-needs-human"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "t1-needs-human@example.com", actorGitHubExternalID, repoFullName, 71)
	// §30.7 stamps each recorded outcome with the egress mode that held
	// when it was OBSERVED, and the calibration query excludes shadow ones.
	// A repo with no repo_settings row resolves shadow, so this must be
	// armed BEFORE the outcome is recorded -- arming afterwards cannot
	// change a stamp already written. These tests are about whether an
	// outcome is recorded at all, not about egress mode.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}
	prs := fakeSCM.openPRsByExternalID[actorGitHubExternalID]
	prs[0].Labels = append(prs[0].Labels, "review:needs-human")
	fakeSCM.openPRsByExternalID[actorGitHubExternalID] = prs

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	item := findItemByPR(result.Items, 71)
	if item == nil {
		t.Fatal("PR #71 missing from the inbox entirely")
	}
	if item.Kind != decisioninboxdomain.KindNeedsReview {
		t.Errorf("Kind = %q, want needs_review (the needs-human label demotes it)", item.Kind)
	}

	total, contested := countAutoApprovalOutcomes(ctx, t, pool, repoFullName)
	if total != 1 || contested != 1 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (1, 1)", total, contested)
	}
}

// TestBuild_NotContested_WhenEngineWouldNotHaveApprovedAnyway proves the
// negative case both halves above must NOT trigger on: a PR that is
// BOTH genuinely ineligible on a REAL criterion (a stale verdict, here)
// AND carries a human-disagreement signal (HasChangesRequested) must
// NEVER record 'overridden' -- the engine never would have approved this
// PR regardless of the human signal, so there is no genuine contradiction
// to record. Guards against a fix that over-corrects T1 into recording
// EVERY human-disagreement signal unconditionally.
func TestBuild_NotContested_WhenEngineWouldNotHaveApprovedAnyway(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubExternalID = "5008"
	const repoFullName = "acme/t1-not-contested-stale"

	actor, fakeSCM := buildEligibleReadyToMergeFixture(ctx, t, pool, tokenKey, "t1-not-contested@example.com", actorGitHubExternalID, repoFullName, 72)
	// §30.7 stamps each recorded outcome with the egress mode that held
	// when it was OBSERVED, and the calibration query excludes shadow ones.
	// A repo with no repo_settings row resolves shadow, so this must be
	// armed BEFORE the outcome is recorded -- arming afterwards cannot
	// change a stamp already written. These tests are about whether an
	// outcome is recorded at all, not about egress mode.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}
	prs := fakeSCM.openPRsByExternalID[actorGitHubExternalID]
	prs[0].HasChangesRequested = true
	// Stale verdict: the live head sha no longer matches what
	// seedAutoApprovedVerdict recorded -- the engine would refuse this
	// PR on ITS OWN criteria regardless of HasChangesRequested.
	prs[0].HeadSHA = "a-new-commit-landed-after-the-verdict"
	fakeSCM.openPRsByExternalID[actorGitHubExternalID] = prs

	deps := decisioninbox.Deps{
		Plans: narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}

	if _, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now()); err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	total, contested := countAutoApprovalOutcomes(ctx, t, pool, repoFullName)
	if total != 0 || contested != 0 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (0, 0) -- the engine would have refused this PR on its own (stale verdict), so HasChangesRequested is not a genuine contradiction to record", total, contested)
	}
}

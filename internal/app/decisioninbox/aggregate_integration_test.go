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
	"errors"
	"fmt"
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
	decisioninboxdomain "github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/migrations"
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

	// getOpenPRByKey/getOpenPRErr (§21.2 stage 2) back GetOpenPR
	// below -- keyed by "owner/repo#number", the direct single-PR lookup
	// internal/app/decisioninbox.RevalidateForAutoMerge uses instead of a
	// user-scoped ListOpenPRsForUser search (revalidate_integration_test.go's
	// own TestRevalidateForAutoMerge is this field's one real user).
	getOpenPRByKey map[string]ports.OpenPR
	getOpenPRErr   error
}

var _ ports.SourceControl = (*fakeDecisionInboxSourceControl)(nil)

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
func (f *fakeDecisionInboxSourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "", "", errors.New("fakeDecisionInboxSourceControl: ResolveBranchSHA not implemented")
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

// GetOpenPR (§21.2 stage 2) looks up f.getOpenPRByKey by
// "owner/repo#number" -- a miss (the ordinary case for every test that
// never populates this field) reports found=false, err=nil, mirroring a
// confirmed GitHub 404 (ports.SourceControl.GetOpenPR's own doc comment)
// rather than an error, since most callers of this fake never exercise
// this method at all.
func (f *fakeDecisionInboxSourceControl) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	if f.getOpenPRErr != nil {
		return ports.OpenPR{}, false, f.getOpenPRErr
	}
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	pr, ok := f.getOpenPRByKey[key]
	return pr, ok, nil
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
	if _, err := appreviewverdict.Insert(ctx, store, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false); err != nil {
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

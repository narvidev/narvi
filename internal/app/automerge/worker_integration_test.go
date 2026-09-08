//go:build integration

// Integration tests for internal/app/automerge.Worker (§21.2
// stage 2) against a real Postgres instance.
package automerge_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automerge"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeAutoMergeSourceControl is a minimal, test-only ports.SourceControl
// -- only GetOpenPR/MergePR are ever exercised by internal/app/automerge,
// mirroring internal/app/decisioninbox's own fakeDecisionInboxSourceControl
// precedent (unimplemented methods return a clear error, never called by
// this package's own code paths).
type fakeAutoMergeSourceControl struct {
	mu sync.Mutex

	prsByKey      map[string]ports.OpenPR // "owner/repo#number"
	getErr        error
	getOpenPRHits int

	mergeCalls     []ports.MergePRSpec
	mergeSHA       string
	mergeErr       error
	mergeErrByRepo map[string]error // "owner/repo" -> error, takes priority over mergeErr (see MergePR's own doc comment)
}

var _ ports.SourceControl = (*fakeAutoMergeSourceControl)(nil)

func (f *fakeAutoMergeSourceControl) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getOpenPRHits++
	if f.getErr != nil {
		return ports.OpenPR{}, false, f.getErr
	}
	key := owner + "/" + repo + "#" + itoa(number)
	pr, ok := f.prsByKey[key]
	return pr, ok, nil
}

func (f *fakeAutoMergeSourceControl) getOpenPRCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getOpenPRHits
}
func (f *fakeAutoMergeSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("fakeAutoMergeSourceControl: GetPRBody not implemented")
}
func (f *fakeAutoMergeSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("fakeAutoMergeSourceControl: UpdatePRBody not implemented")
}

func (f *fakeAutoMergeSourceControl) MergePR(_ context.Context, spec ports.MergePRSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeCalls = append(f.mergeCalls, spec)
	// mergeErrByRepo (keyed "owner/repo") takes priority over the plain,
	// every-call mergeErr below -- needed by the two per-repository-scope
	// auth-dead-letter tests, which must fail ONE repo's own calls while
	// a genuinely unrelated, healthy repo's calls keep succeeding through
	// the SAME shared fake instance.
	if f.mergeErrByRepo != nil {
		if err, ok := f.mergeErrByRepo[spec.Owner+"/"+spec.Repo]; ok {
			return "", err
		}
	}
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	return f.mergeSHA, nil
}

func (f *fakeAutoMergeSourceControl) mergeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mergeCalls)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func (f *fakeAutoMergeSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "", "", errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("not implemented")
}
func (f *fakeAutoMergeSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("not implemented -- internal/app/automerge never calls this (see ports.SourceControl.GetOpenPR's own doc comment)")
}
func (f *fakeAutoMergeSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("not implemented")
}

// automergeTestRig bundles every store internal/app/automerge.Worker
// needs, backed by ONE shared Postgres pool (sharedpool_integration_test.go).
type automergeTestRig struct {
	pool          *pgxpool.Pool
	repoSettings  *narvipg.RepoSettingsStore
	reviewVerdict appreviewverdict.Deps
	auditLog      *narvipg.AuditLogStore
}

func newAutomergeTestRig(t *testing.T) *automergeTestRig {
	t.Helper()
	pool := newTestPool(t)
	return &automergeTestRig{
		pool:         pool,
		repoSettings: narvipg.NewRepoSettingsStore(pool),
		reviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts:       narvipg.NewReviewVerdictStore(pool),
			RepoSettings:         narvipg.NewRepoSettingsStore(pool),
			ReviewFindings:       narvipg.NewReviewFindingStore(pool),
			AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts:             platform.DefaultTimeouts(),
		},
		auditLog: narvipg.NewAuditLogStore(pool),
	}
}

func (rs *automergeTestRig) deps(sourceControl ports.SourceControl) automerge.Deps {
	return automerge.Deps{
		DecisionInbox: decisioninbox.Deps{
			Plans:          narvipg.NewPlanStore(rs.pool),
			Sessions:       narvipg.NewSessionStore(rs.pool),
			Participants:   narvipg.NewParticipantStore(rs.pool),
			Automations:    narvipg.NewAutomationStore(rs.pool),
			Outbox:         narvipg.NewOutboxStore(rs.pool, false),
			ReviewFindings: narvipg.NewReviewFindingStore(rs.pool),
			SentinelFixes:  narvipg.NewSentinelFixStore(rs.pool),
			Artifacts:      narvipg.NewArtifactStore(rs.pool),
			Identities:     narvipg.NewIdentityStore(rs.pool),
			Timeouts:       platform.DefaultTimeouts(),
			ReviewVerdict:  rs.reviewVerdict,
		},
		SourceControl: sourceControl,
		AuditLog:      rs.auditLog,
		BotToken:      "bot-token",
		Timeouts:      platform.DefaultTimeouts(),
	}
}

// seedEligiblePR seeds a fully platform-authored, eligible (Shippable=auto)
// PR at repoFullName#prNumber -- the fixture every test below starts
// from, mirroring internal/app/decisioninbox's own seedAutoApprovedVerdict
// precedent.
func (rs *automergeTestRig) seedEligiblePR(ctx context.Context, t *testing.T, repoFullName string, prNumber int32, headSHA string) (htmlURL string) {
	t.Helper()

	session, err := narvipg.NewSessionStore(rs.pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	htmlURL = "https://github.com/" + repoFullName + "/pull/" + itoa(int(prNumber))
	if _, err := narvipg.NewArtifactStore(rs.pool).Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}

	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)

	// §30.8: ListLatestAutoApprovedInRepo now excludes any verdict whose
	// own repo has not been promoted past the live_egress_promoted_at
	// fence -- this fixture means "a real, would-really-have-happened
	// candidate", so it promotes the repo first, exactly like a real
	// operator's own promotion gesture would before this repo could ever
	// have a real merge candidate.
	repoSettings := narvipg.NewRepoSettingsStore(rs.pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}
	if _, err := appreviewverdict.Insert(ctx, rs.reviewVerdict.ReviewVerdicts, repoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false); err != nil {
		t.Fatalf("seed review_verdicts row: %v", err)
	}
	return htmlURL
}

// TestPumpOnce_OffByDefault_NoMerge is §21's own explicitly-pinned
// mutation test: "auto-merge-off-by-default". A repo with a genuinely
// eligible, auto-approved candidate PR on record, but NO repo_settings
// row at all (the table's own established "absence means every flag
// defaults to its own safe value" precedent, migrations/000044) must
// never be scanned for merge candidates, let alone merged -- arming
// requires an EXPLICIT admin UpsertAutoApprovalSettings(autoMergeEnabled=true)
// call, never an implicit default.
func TestPumpOnce_OffByDefault_NoMerge(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-off-by-default"

	htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, 1, "sha-1")

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-off-by-default#1": {
				Owner: "acme", Repo: "automerge-off-by-default", Number: 1, HTMLURL: htmlURL,
				HeadSHA: "sha-1", CIConclusion: ports.CIConclusionSuccess,
			},
		},
		mergeSHA: "should-never-be-used",
	}
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}

	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0 (repo_settings.auto_merge_enabled defaults to false -- an unarmed repo must never be scanned for merge candidates at all)", got)
	}
}

// TestPumpOnce_ExplicitlyDisabled_NoMerge is OffByDefault's own sibling:
// an EXPLICIT auto_merge_enabled=false row (not merely a missing one)
// must ALSO never merge -- proving the gate reads the toggle's actual
// value, not just "row present or absent".
func TestPumpOnce_ExplicitlyDisabled_NoMerge(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-explicitly-off"

	htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, 2, "sha-2")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, false); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-explicitly-off#2": {
				Owner: "acme", Repo: "automerge-explicitly-off", Number: 2, HTMLURL: htmlURL,
				HeadSHA: "sha-2", CIConclusion: ports.CIConclusionSuccess,
			},
		},
	}
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0 (auto_merge_enabled explicitly false)", got)
	}
}

// TestPumpOnce_Armed_MergesEligibleCandidate proves the POSITIVE case:
// once a repo is explicitly armed, a genuinely eligible candidate DOES
// merge, using the bot token (never a human's), and records a
// 'confirmed' contradiction-rate outcome.
func TestPumpOnce_Armed_MergesEligibleCandidate(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-armed"

	htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, 3, "sha-3")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-armed#3": {
				Owner: "acme", Repo: "automerge-armed", Number: 3, HTMLURL: htmlURL,
				HeadSHA: "sha-3", CIConclusion: ports.CIConclusionSuccess,
			},
		},
		mergeSHA: "merged-commit-sha",
	}
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}

	if got := sc.mergeCallCount(); got != 1 {
		t.Fatalf("MergePR call count = %d, want 1", got)
	}
	if sc.mergeCalls[0].Token != "bot-token" {
		t.Errorf("MergePR token = %q, want the bot token (never a human actor's, since this is a machine-initiated merge)", sc.mergeCalls[0].Token)
	}
	if sc.mergeCalls[0].HeadSHA != "sha-3" {
		t.Errorf("MergePR HeadSHA = %q, want %q", sc.mergeCalls[0].HeadSHA, "sha-3")
	}

	total, contested, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	if total != 1 || contested != 0 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (1, 0) -- the merge must record a 'confirmed' outcome", total, contested)
	}
}

// TestPumpOnce_Armed_StaleVerdictNeverMerges proves the auto-merge
// worker reuses RevalidateForAutoMerge's own stale-head-SHA guard
// unchanged -- a new commit landed after the verdict was posted, so
// the live PR's own head sha no longer matches review_verdicts.head_sha.
func TestPumpOnce_Armed_StaleVerdictNeverMerges(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-stale"

	htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, 4, "sha-4-old")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-stale#4": {
				Owner: "acme", Repo: "automerge-stale", Number: 4, HTMLURL: htmlURL,
				HeadSHA: "sha-4-new-commit-landed", CIConclusion: ports.CIConclusionSuccess,
			},
		},
	}
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0 (the verdict on record names an earlier commit)", got)
	}
}

// TestPumpOnce_CandidateNoLongerOpen_NeverErrors proves a PR that closed/
// merged through some other path between discovery and this tick is an
// ordinary, expected race (GetOpenPR's own found=false), never an error
// that could abort other repos' own candidates in the same tick.
func TestPumpOnce_CandidateNoLongerOpen_NeverErrors(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-closed-race"

	rig.seedEligiblePR(ctx, t, repoFullName, 5, "sha-5")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{prsByKey: map[string]ports.OpenPR{}} // no PR found at all
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil (a closed/merged-elsewhere race is not a pump-level error)", err)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0", got)
	}
}

// TestPumpOnce_Armed_GetOpenPRErrors_NeverMergesNeverPanics is the T2
// regression test for fakeAutoMergeSourceControl.
// getErr: before this fix, that field was wired into the fake but set by
// NO test in this file, leaving RevalidateForAutoMerge's own genuine-
// error branch (mergeCandidate's `if err != nil { logger.Error(...);
// return }`) completely uncovered -- a worker tested only on the happy
// path is unacceptable for the one component that merges without a
// human. A transient GitHub error resolving the candidate's own live
// state (rate limit, timeout, 5xx) must be logged and skipped, never
// panic, never merge, and never abort the tick.
func TestPumpOnce_Armed_GetOpenPRErrors_NeverMergesNeverPanics(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-getopenpr-errors"

	rig.seedEligiblePR(ctx, t, repoFullName, 6, "sha-6")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{getErr: errors.New("github: rate limited")}
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil (a per-candidate GetOpenPR error must degrade, never fail, the whole tick)", err)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0 (a candidate whose live state could not be confirmed must never merge)", got)
	}

	total, contested, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	if total != 0 || contested != 0 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (0, 0) -- RecordConfirmed must never fire for a candidate that was never actually merged", total, contested)
	}
}

// TestPumpOnce_Armed_MergeFails_NeverPanics_NoConfirmedOutcomeRecorded is
// T2's own sibling for fakeAutoMergeSourceControl.mergeErr -- also wired
// into the fake but set by no test before this fix, leaving
// mergeCandidate's own merge-failed branch (`if err != nil {
// logger.Error(...); return }`, AFTER RevalidateForAutoMerge already
// said ok=true) completely uncovered. A genuinely eligible candidate
// whose real GitHub MergePR call itself fails (branch protection newly
// blocking it, a concurrent conflicting change, a transient 5xx) must be
// logged and skipped -- critically, RecordConfirmed must NEVER fire for
// a merge that did not actually happen (the ONE outcome-recording bug
// this test guards against: a naive refactor that moved the
// RecordConfirmed call BEFORE the MergePR error check would silently
// mis-record failed merges as confirmed).
func TestPumpOnce_Armed_MergeFails_NeverPanics_NoConfirmedOutcomeRecorded(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-merge-fails"

	htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, 7, "sha-7")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-merge-fails#7": {
				Owner: "acme", Repo: "automerge-merge-fails", Number: 7, HTMLURL: htmlURL,
				HeadSHA: "sha-7", CIConclusion: ports.CIConclusionSuccess,
			},
		},
		mergeErr: &ports.MergePRError{Status: http.StatusMethodNotAllowed, Message: "not mergeable"},
	}
	worker := automerge.New(rig.deps(sc))

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil (a per-candidate MergePR error must degrade, never fail, the whole tick)", err)
	}
	if got := sc.mergeCallCount(); got != 1 {
		t.Fatalf("MergePR call count = %d, want 1 (the merge WAS attempted -- this test proves the FAILURE path, not that it was skipped)", got)
	}

	total, contested, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	if total != 0 || contested != 0 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (0, 0) -- RecordConfirmed must never fire when the MergePR call itself failed", total, contested)
	}
}

// findAuditLogEntry returns the most recent audit_log row carrying the
// given action, or nil if none exists yet -- a small local helper for the
// two auth-dead-letter tests below, which need to confirm docs/
// TECHNICAL_PLAN.md §17's own "how does an operator learn" answer
// (worker.go's own recordAuthOutcome) actually wrote a durable, queryable
// row, not merely a log line.
func findAuditLogEntry(t *testing.T, pool *pgxpool.Pool, action string) *sqlcgen.AuditLog {
	t.Helper()
	entries, err := narvipg.NewAuditLogStore(pool).List(context.Background(), 100, 0)
	if err != nil {
		t.Fatalf("list audit log entries: %v", err)
	}
	for i := range entries {
		if entries[i].Action == action {
			return &entries[i]
		}
	}
	return nil
}

// TestPumpOnce_MergePR401_DeadLettersWorkerWide_StopsHammeringAllRepos is
// this Step's own end-to-end proof of docs/TECHNICAL_PLAN.md §17: a
// bad/empty bot token (modeled here as MergePR always returning a 401
// *ports.MergePRError, exactly what githubapi.MergePR produces for
// GitHub's real "Bad credentials" response) must, after
// domainautomerge.MaxAuthFailures consecutive failures, (1) stop this
// worker from calling GetOpenPR/MergePR again AT ALL, for ANY repo --
// including one that never itself failed, since every repo shares the
// SAME bot token -- and (2) leave a durable, queryable audit_log row
// behind, not merely a log line ("only rate-limit noise" is the plan's
// own name for the gap this closes).
//
// now is advanced by a full hour between ticks -- comfortably past
// platform.DefaultTimeouts().AutoMergeAuthBackoffMax (30min), so every
// tick after the first is guaranteed to be outside its own scope's
// backoff window without this test needing to know the exact schedule
// domain/automerge.EvaluateBackoff computes.
func TestPumpOnce_MergePR401_DeadLettersWorkerWide_StopsHammeringAllRepos(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoA = "acme/automerge-401-repo-a"

	htmlURLA := rig.seedEligiblePR(ctx, t, repoA, 10, "sha-10")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoA, true); err != nil {
		t.Fatalf("upsert auto-approval settings (repo-a): %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-401-repo-a#10": {
				Owner: "acme", Repo: "automerge-401-repo-a", Number: 10, HTMLURL: htmlURLA,
				HeadSHA: "sha-10", CIConclusion: ports.CIConclusionSuccess,
			},
		},
		mergeErr: &ports.MergePRError{Status: http.StatusUnauthorized, Message: "Bad credentials"},
	}
	worker := automerge.New(rig.deps(sc))

	now := time.Now()
	const maxAuthFailures = 5 // domainautomerge.MaxAuthFailures, kept as a literal to avoid this test depending on an internal/domain import for a single constant
	for i := 0; i < maxAuthFailures; i++ {
		if err := worker.PumpOnce(ctx, now); err != nil {
			t.Fatalf("PumpOnce() tick %d error = %v, want nil", i, err)
		}
		now = now.Add(time.Hour)
	}

	if got := sc.mergeCallCount(); got != maxAuthFailures {
		t.Fatalf("MergePR call count after %d ticks = %d, want %d (one real attempt per tick until dead-lettered)", maxAuthFailures, got, maxAuthFailures)
	}

	entry := findAuditLogEntry(t, rig.pool, "automerge.auth_dead_lettered")
	if entry == nil {
		t.Fatal("no audit_log row with action=automerge.auth_dead_lettered found -- an operator has no durable record of the dead-letter at all")
	}
	if entry.ResourceType != "automerge_worker" {
		t.Errorf("audit_log entry ResourceType = %q, want %q (worker-wide scope)", entry.ResourceType, "automerge_worker")
	}

	// Reset the fake's own call counters, then run several MORE ticks,
	// including a brand-new SECOND repo that has never itself failed --
	// the actual "stops hammering" proof: zero further GetOpenPR/MergePR
	// calls for EITHER repo, forever, for the rest of this process.
	sc.mu.Lock()
	sc.mergeCalls = nil
	sc.getOpenPRHits = 0
	sc.mu.Unlock()

	const repoB = "acme/automerge-401-repo-b-never-failed"
	htmlURLB := rig.seedEligiblePR(ctx, t, repoB, 11, "sha-11")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoB, true); err != nil {
		t.Fatalf("upsert auto-approval settings (repo-b): %v", err)
	}
	sc.mu.Lock()
	sc.prsByKey["acme/automerge-401-repo-b-never-failed#11"] = ports.OpenPR{
		Owner: "acme", Repo: "automerge-401-repo-b-never-failed", Number: 11, HTMLURL: htmlURLB,
		HeadSHA: "sha-11", CIConclusion: ports.CIConclusionSuccess,
	}
	sc.mu.Unlock()

	for i := 0; i < 3; i++ {
		now = now.Add(24 * time.Hour)
		if err := worker.PumpOnce(ctx, now); err != nil {
			t.Fatalf("PumpOnce() post-dead-letter tick %d error = %v, want nil", i, err)
		}
	}

	if got := sc.mergeCallCount(); got != 0 {
		t.Errorf("MergePR call count after dead-letter = %d, want 0 -- the worker must never call MergePR again, for ANY repo, once worker-wide dead-lettered", got)
	}
	if got := sc.getOpenPRCallCount(); got != 0 {
		t.Errorf("GetOpenPR call count after dead-letter = %d, want 0 (repo-a) -- this is the literal 'keeps polling GitHub, forever, at full rate' bug docs/TECHNICAL_PLAN.md §17 describes", got)
	}
}

// TestPumpOnce_MergePR403NonRateLimited_DeadLettersOnlyThatRepo is
// WorkerWide's own negative counterpart: a 403 GitHub's own rate-limit
// classification has already ruled out (RateLimited: false) is a
// PER-REPOSITORY denial (e.g. this repo revoked the bot account's own
// access) -- it must dead-letter ONLY the repo that actually failed, and
// a genuinely unrelated armed repo's candidates must keep merging
// normally throughout.
func TestPumpOnce_MergePR403NonRateLimited_DeadLettersOnlyThatRepo(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const deniedRepo = "acme/automerge-403-denied"
	const healthyRepo = "acme/automerge-403-healthy"

	htmlURLDenied := rig.seedEligiblePR(ctx, t, deniedRepo, 20, "sha-20")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, deniedRepo, true); err != nil {
		t.Fatalf("upsert auto-approval settings (denied repo): %v", err)
	}
	htmlURLHealthy := rig.seedEligiblePR(ctx, t, healthyRepo, 21, "sha-21")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, healthyRepo, true); err != nil {
		t.Fatalf("upsert auto-approval settings (healthy repo): %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			"acme/automerge-403-denied#20": {
				Owner: "acme", Repo: "automerge-403-denied", Number: 20, HTMLURL: htmlURLDenied,
				HeadSHA: "sha-20", CIConclusion: ports.CIConclusionSuccess,
			},
			"acme/automerge-403-healthy#21": {
				Owner: "acme", Repo: "automerge-403-healthy", Number: 21, HTMLURL: htmlURLHealthy,
				HeadSHA: "sha-21", CIConclusion: ports.CIConclusionSuccess,
			},
		},
		// mergeErrByRepo, not the plain every-call mergeErr: this test's
		// whole point is that the healthy repo's own calls keep
		// succeeding through the SAME shared fake/Worker instance while
		// ONLY the denied repo's calls fail -- a global mergeErr would
		// wrongly deny both.
		mergeErrByRepo: map[string]error{
			deniedRepo: &ports.MergePRError{Status: http.StatusForbidden, Message: "Resource not accessible by integration", RateLimited: false},
		},
		mergeSHA: "healthy-repo-merge-sha",
	}
	worker := automerge.New(rig.deps(sc))

	now := time.Now()
	const maxAuthFailures = 5 // domainautomerge.MaxAuthFailures, see the sibling test's own identical comment
	for i := 0; i < maxAuthFailures; i++ {
		if err := worker.PumpOnce(ctx, now); err != nil {
			t.Fatalf("PumpOnce() tick %d error = %v, want nil", i, err)
		}
		now = now.Add(time.Hour)
	}

	entry := findAuditLogEntry(t, rig.pool, "automerge.auth_dead_lettered")
	if entry == nil {
		t.Fatal("no audit_log row with action=automerge.auth_dead_lettered found")
	}
	if entry.ResourceType != "repository" {
		t.Errorf("audit_log entry ResourceType = %q, want %q (repo-scoped)", entry.ResourceType, "repository")
	}
	if entry.ResourceID != deniedRepo {
		t.Errorf("audit_log entry ResourceID = %q, want %q -- must name the SPECIFIC denied repository", entry.ResourceID, deniedRepo)
	}

	// Now clear mergeErrByRepo (simulating GitHub access being restored
	// for the denied repo) and confirm it STAYS dead-lettered regardless
	// -- a per-repository dead-letter is permanent for this process's own
	// lifetime (authguard.go's own doc comment), unlike a merely-backing-
	// off scope -- while the healthy repo, whose own calls never failed
	// in the first place, keeps merging normally throughout.
	sc.mu.Lock()
	sc.mergeErrByRepo = nil
	sc.mergeCalls = nil
	sc.mu.Unlock()

	now = now.Add(time.Hour)
	if err := worker.PumpOnce(ctx, now); err != nil {
		t.Fatalf("PumpOnce() post-dead-letter tick error = %v, want nil", err)
	}

	sc.mu.Lock()
	var deniedAttempted, healthyMerged bool
	for _, call := range sc.mergeCalls {
		if call.Owner+"/"+call.Repo == deniedRepo {
			deniedAttempted = true
		}
		if call.Owner+"/"+call.Repo == healthyRepo {
			healthyMerged = true
		}
	}
	sc.mu.Unlock()

	if deniedAttempted {
		t.Error("MergePR was called again for the denied repo after its own dead-letter -- per-repository dead-letter must be permanent")
	}
	if !healthyMerged {
		t.Error("MergePR was never called for the healthy, unrelated repo -- a per-repository dead-letter must never leak and block an unrelated repo's own merges")
	}
}

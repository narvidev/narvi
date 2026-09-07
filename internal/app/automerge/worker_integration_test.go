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

	prsByKey map[string]ports.OpenPR // "owner/repo#number"
	getErr   error

	mergeCalls []ports.MergePRSpec
	mergeSHA   string
	mergeErr   error
}

var _ ports.SourceControl = (*fakeAutoMergeSourceControl)(nil)

func (f *fakeAutoMergeSourceControl) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return ports.OpenPR{}, false, f.getErr
	}
	key := owner + "/" + repo + "#" + itoa(number)
	pr, ok := f.prsByKey[key]
	return pr, ok, nil
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

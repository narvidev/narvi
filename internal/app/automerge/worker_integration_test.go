//go:build integration

// Integration tests for internal/app/automerge.Worker (§21.2
// stage 2) against a real Postgres instance.
package automerge_test

import (
	"context"
	"errors"
	"fmt"
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

	// mergeRendezvous, when non-nil, is waited on INSIDE MergePR, AFTER
	// releasing f.mu -- models "blocks on a real GitHub round trip"
	// (docs/TECHNICAL_PLAN.md §17's own adversarial review, blocker #1)
	// so a test driving several armed repos through ONE PumpOnce tick can
	// force their own concurrent goroutines (worker.go's own errgroup
	// fan-out) to all clear their own authGuard.allow() check BEFORE any
	// one of them reports its own outcome -- exactly the interleaving the
	// review's own diagnosis names, and the ONE thing a zero-latency fake
	// can never reproduce (the review's own counterfactual: "with latency
	// removed from the fake... only one failure is recorded").
	//
	// A DETERMINISTIC rendezvous (below), not a sleep long enough to
	// "usually" win the race: this Step's own first version used a plain
	// time.Sleep, which was long enough locally but not on a loaded CI
	// runner, where one goroutine legitimately started late enough to
	// miss the window -- correct behavior (the guard blocking a late
	// arrival), misread by the test's own then-assertion as a bug. A
	// barrier that blocks every caller until ALL of them have arrived
	// removes that scheduling dependency entirely: either every
	// concurrent call is genuinely in flight together by construction, or
	// the barrier's own timeout fails the test loudly instead of hanging.
	mergeRendezvous *rendezvousBarrier
}

var _ ports.SourceControl = (*fakeAutoMergeSourceControl)(nil)

// rendezvousBarrier blocks every Wait call until n callers have all
// arrived, then releases them all together -- so a test can force N
// goroutines to be genuinely concurrent at one specific point, rather
// than hoping a sleep duration happens to outlast whatever scheduling
// noise a real CI runner introduces. timeout is a safety valve: if fewer
// than n callers ever arrive (e.g. a regression elsewhere makes one of
// them return early, before ever reaching Wait), every already-arrived
// caller is released once timeout elapses rather than hanging forever --
// the caller of Wait is expected to fail the test if that happens
// (mergeRendezvousTimedOut below).
type rendezvousBarrier struct {
	mu         sync.Mutex
	n          int
	arrived    int
	timeout    time.Duration
	release    chan struct{}
	hitTimeout bool
}

func newRendezvousBarrier(n int, timeout time.Duration) *rendezvousBarrier {
	return &rendezvousBarrier{n: n, timeout: timeout, release: make(chan struct{})}
}

// Wait blocks the calling goroutine until every one of b.n expected
// callers has itself called Wait, then returns for all of them at once.
func (b *rendezvousBarrier) Wait() {
	b.mu.Lock()
	b.arrived++
	release := b.release
	last := b.arrived >= b.n
	b.mu.Unlock()

	if last {
		close(release)
		return
	}

	select {
	case <-release:
	case <-time.After(b.timeout):
		b.mu.Lock()
		b.hitTimeout = true
		b.mu.Unlock()
	}
}

// timedOut reports whether ANY Wait call on this barrier ever hit its
// own timeout instead of a genuine rendezvous -- a test calling this
// after driving a tick to completion can fail loudly on it, rather than
// silently accepting a degraded (non-concurrent) run as if it had
// exercised the same thing a real rendezvous would have.
func (b *rendezvousBarrier) timedOut() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hitTimeout
}

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
	f.mergeCalls = append(f.mergeCalls, spec)
	// mergeErrByRepo (keyed "owner/repo") takes priority over the plain,
	// every-call mergeErr below -- needed by the two per-repository-scope
	// auth-dead-letter tests, which must fail ONE repo's own calls while
	// a genuinely unrelated, healthy repo's calls keep succeeding through
	// the SAME shared fake instance.
	var err error
	if f.mergeErrByRepo != nil {
		if e, ok := f.mergeErrByRepo[spec.Owner+"/"+spec.Repo]; ok {
			err = e
		}
	}
	if err == nil {
		err = f.mergeErr
	}
	sha := f.mergeSHA
	rendezvous := f.mergeRendezvous
	f.mu.Unlock()

	if rendezvous != nil {
		// Deliberately OUTSIDE f.mu -- every concurrent goroutine waits
		// here together, mirroring N real, independent outbound GitHub
		// calls in flight at once, never serialized by this fake's own
		// bookkeeping lock.
		rendezvous.Wait()
	}
	if err != nil {
		return "", err
	}
	return sha, nil
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

// newWorker builds a Worker from rig.deps(sourceControl), failing the
// test immediately on error -- automerge.New now constructs a real OTel
// counter (automerge_auth_dead_lettered_total, worker.go) at
// construction time and can fail, mirroring outboxworker.NewBuilder/
// imagebuild.NewBuilder's own identical "construction can fail" shape.
// Small helper so each of the tests below (which all previously called
// automerge.New directly, back when it could not fail) stays a one-line
// call.
func (rs *automergeTestRig) newWorker(t *testing.T, sourceControl ports.SourceControl) *automerge.Worker {
	t.Helper()
	worker, err := automerge.New(rs.deps(sourceControl))
	if err != nil {
		t.Fatalf("automerge.New: %v", err)
	}
	return worker
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
	worker := rig.newWorker(t, sc)

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
	worker := rig.newWorker(t, sc)

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
	worker := rig.newWorker(t, sc)

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
	worker := rig.newWorker(t, sc)

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
	worker := rig.newWorker(t, sc)

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
	worker := rig.newWorker(t, sc)

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
	worker := rig.newWorker(t, sc)

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
// auth-dead-letter tests below, which need to confirm docs/
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

// countAuditLogEntries is findAuditLogEntry's own counting sibling --
// docs/TECHNICAL_PLAN.md §17's own adversarial review, test-integrity
// finding: "the audit-log assertions cannot detect the operator noise the
// fix exists to remove: writing a bogus dead-letter row on every failed
// call still passes them." A mere findAuditLogEntry != nil check is
// satisfied by ONE row just as much as by a fresh row on every single
// tick -- exactly the "only rate-limit noise" regression §17 exists to
// close. Every test below that asserts on automerge.auth_dead_lettered
// now also asserts the COUNT stays exactly 1 across every tick AFTER the
// dead-lettering one.
func countAuditLogEntries(t *testing.T, pool *pgxpool.Pool, action string) int {
	t.Helper()
	entries, err := narvipg.NewAuditLogStore(pool).List(context.Background(), 100, 0)
	if err != nil {
		t.Fatalf("list audit log entries: %v", err)
	}
	count := 0
	for i := range entries {
		if entries[i].Action == action {
			count++
		}
	}
	return count
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
// platform.DefaultTimeouts().AutoMergeAuthBackoffMax (12min), so every
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
	worker := rig.newWorker(t, sc)

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

	// The operator-noise regression test: a bogus dead-letter row on
	// every subsequent failed call would still pass a mere
	// findAuditLogEntry != nil check above -- this is the assertion that
	// actually distinguishes "fired exactly once" from "fires on every
	// tick" (docs/TECHNICAL_PLAN.md §17's own adversarial review, test-
	// integrity finding).
	if got := countAuditLogEntries(t, rig.pool, "automerge.auth_dead_lettered"); got != 1 {
		t.Errorf("audit_log rows with action=automerge.auth_dead_lettered = %d after %d further ticks, want exactly 1 -- a fresh row on every subsequent failed call is the operator noise this fix exists to remove", got, 3)
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
	worker := rig.newWorker(t, sc)

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

	for i := 0; i < 3; i++ {
		now = now.Add(time.Hour)
		if err := worker.PumpOnce(ctx, now); err != nil {
			t.Fatalf("PumpOnce() post-dead-letter tick %d error = %v, want nil", i, err)
		}
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

	// The operator-noise regression test, mirroring the worker-wide
	// sibling test's own identical assertion: a bogus dead-letter row on
	// every subsequent failed call would still pass a mere
	// findAuditLogEntry != nil check above.
	if got := countAuditLogEntries(t, rig.pool, "automerge.auth_dead_lettered"); got != 1 {
		t.Errorf("audit_log rows with action=automerge.auth_dead_lettered = %d after 3 further ticks, want exactly 1", got)
	}
}

// TestPumpOnce_GetOpenPRAuthFailure_DeadLettersWorkerWide is
// TestPumpOnce_MergePR401_DeadLettersWorkerWide_StopsHammeringAllRepos'
// own sibling for mergeCandidate's OTHER recordAuthOutcome call site --
// docs/TECHNICAL_PLAN.md §17's own adversarial review, test-integrity
// finding: "the revalidate/GetOpenPR guard -- the actual 'polls GitHub
// forever' path this commit exists to fix -- is untested; removing it
// leaves unit and integration tests green." Every OTHER dead-letter test
// in this file drives a failure through MergePR (the SECOND
// recordAuthOutcome call site, worker.go's own mergeCandidate); this one
// drives it through RevalidateForAutoMerge's own GetOpenPR call (the
// FIRST), fed by fakeAutoMergeSourceControl.getErr -- proving worker.go's
// own `if err != nil { w.recordAuthOutcome(...) }` branch immediately
// after RevalidateForAutoMerge is real, wired code, not dead: deleting it
// would leave every OTHER test in this file green, since none of them
// ever cause GetOpenPR itself to return a classified error.
func TestPumpOnce_GetOpenPRAuthFailure_DeadLettersWorkerWide(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-getopenpr-401"

	rig.seedEligiblePR(ctx, t, repoFullName, 30, "sha-30")
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert auto-approval settings: %v", err)
	}

	sc := &fakeAutoMergeSourceControl{
		// getErr, not mergeErr: GetOpenPR itself fails, so
		// RevalidateForAutoMerge never even reaches revalidateCore, and
		// MergePR must never be attempted at all.
		getErr: &ports.MergePRError{Status: http.StatusUnauthorized, Message: "Bad credentials"},
	}
	worker := rig.newWorker(t, sc)

	now := time.Now()
	const maxAuthFailures = 5 // domainautomerge.MaxAuthFailures, see the sibling tests' own identical comment
	for i := 0; i < maxAuthFailures; i++ {
		if err := worker.PumpOnce(ctx, now); err != nil {
			t.Fatalf("PumpOnce() tick %d error = %v, want nil", i, err)
		}
		now = now.Add(time.Hour)
	}

	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count = %d, want 0 -- RevalidateForAutoMerge itself never succeeded, MergePR must never even be attempted", got)
	}

	entry := findAuditLogEntry(t, rig.pool, "automerge.auth_dead_lettered")
	if entry == nil {
		t.Fatal("no audit_log row with action=automerge.auth_dead_lettered found -- the revalidate/GetOpenPR failure path must dead-letter exactly like the MergePR path does")
	}
	if entry.ResourceType != "automerge_worker" {
		t.Errorf("audit_log entry ResourceType = %q, want %q (worker-wide scope)", entry.ResourceType, "automerge_worker")
	}

	// "stops hammering" proof, mirroring the MergePR-path sibling test:
	// zero further GetOpenPR calls once worker-wide dead-lettered.
	sc.mu.Lock()
	sc.getOpenPRHits = 0
	sc.mu.Unlock()

	now = now.Add(24 * time.Hour)
	if err := worker.PumpOnce(ctx, now); err != nil {
		t.Fatalf("PumpOnce() post-dead-letter tick error = %v, want nil", err)
	}
	if got := sc.getOpenPRCallCount(); got != 0 {
		t.Errorf("GetOpenPR call count after dead-letter = %d, want 0 -- the worker must never call GetOpenPR again once worker-wide dead-lettered", got)
	}
}

// TestPumpOnce_ConcurrentArmedRepos_OneTickAuthFailure_CoalescesNotDeadLetters
// is this Step's own worker-level, end-to-end reproduction of blocker #1
// (docs/TECHNICAL_PLAN.md §17's own adversarial review): "PumpOnce fans
// repos out concurrently, one errgroup goroutine per armed repo, each
// calling allow(repo, now) with the SAME now. Every goroutine clears
// allow() in microseconds and then blocks on a real GitHub round trip, so
// with >= MaxAuthFailures armed repos they all record a worker-scope
// failure at the same instant." fakeAutoMergeSourceControl.mergeRendezvous
// (above) models that real round trip -- a DETERMINISTIC barrier, not a
// sleep, so every one of armedRepos' own goroutines is GUARANTEED to be
// inside MergePR together before any of them reports its own outcome,
// rather than merely likely to be.
//
// What this test asserts, and why it does NOT assert mergeCallCount() ==
// armedRepos:
//
// How many of the armedRepos goroutines get their own real MergePR
// attempt in the racing tick is NOT the invariant this fix establishes
// -- it is a scheduling-dependent property of exactly how many
// goroutines clear their own allow() check before the FIRST coalesced
// failure lands and sets nextAttemptAt (EvaluateBackoff(1) =
// AutoMergeAuthBackoffBase, 2min, platform/timeouts.go). A goroutine
// scheduled late enough to call allow() AFTER that first failure already
// landed is CORRECTLY blocked by the very backoff window this fix
// installs -- that is the guard working, not a premature dead-letter. An
// earlier version of this test asserted mergeCallCount() == armedRepos
// (with a plain time.Sleep standing in for mergeRendezvous) and failed
// intermittently on a loaded CI runner (7 of 8, not 8) for exactly this
// reason: it pinned an incidental scheduling outcome, not the invariant
// it was named for -- the "a check that fails for a reason it does not
// describe" failure mode this codebase's own audits keep finding, here
// in the OTHER direction (a check that can also spuriously PASS if
// nothing raced at all, which is why mergeRendezvous replaces the sleep
// rather than merely loosening the assertion to >= 1).
//
// The actual invariant -- EXACTLY one consecutive failure recorded per
// concurrent wave, regardless of how many of the armedRepos goroutines
// overlapped -- is bracketed below via the backoff RUNG it produces
// (assertExactlyOneNewWorkerFailure), fully deterministic and observable
// without touching authGuard's own unexported state:
//
//  1. A probe tick 1 minute after the wave -- before the 2m rung
//     EvaluateBackoff(1) sets elapses -- must produce ZERO merge
//     attempts, proving the worker-wide scope IS backing off, i.e. AT
//     LEAST one failure was just recorded (zero failures would leave
//     nextAttemptAt at its zero value, which never blocks allow() at
//     all).
//  2. A probe tick 3 minutes after the wave -- past the 2m rung, but
//     before a SECOND failure's own 4m rung (EvaluateBackoff(2)) would
//     have elapsed -- must produce NON-ZERO merge attempts, proving the
//     rung actually reached was the FIRST one (2m), i.e. EXACTLY one
//     failure, not two or more (whose own rung, 4m, would still be
//     blocking at +3m).
//
// WITHOUT the authguard.go fix (a plain g.mu-protected increment with no
// generation coalescing), this test's own N=8 armed repos -- all
// GUARANTEED concurrent via mergeRendezvous, each failing with the SAME
// worker-wide 401 -- independently record 8 separate consecutive
// failures inside this ONE tick, which (>= domainautomerge.MaxAuthFailures,
// 5) dead-letters the worker mid-tick: the no-dead-letter assertion below
// fails immediately, well before the backoff-rung probes ever run.
func TestPumpOnce_ConcurrentArmedRepos_OneTickAuthFailure_CoalescesNotDeadLetters(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()

	const armedRepos = 8                       // > domainautomerge.MaxAuthFailures (5)
	const rendezvousTimeout = 10 * time.Second // generous CI-runner safety valve, never expected to fire
	sc := &fakeAutoMergeSourceControl{
		prsByKey:        map[string]ports.OpenPR{},
		mergeErr:        &ports.MergePRError{Status: http.StatusUnauthorized, Message: "Bad credentials"},
		mergeRendezvous: newRendezvousBarrier(armedRepos, rendezvousTimeout),
	}
	for i := 0; i < armedRepos; i++ {
		repo := fmt.Sprintf("automerge-concurrent-%d", i)
		repoFullName := "acme/" + repo
		prNumber := int32(100 + i)
		headSHA := fmt.Sprintf("sha-concurrent-%d", i)

		htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, prNumber, headSHA)
		if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
			t.Fatalf("upsert auto-approval settings (%s): %v", repoFullName, err)
		}
		sc.prsByKey[fmt.Sprintf("%s#%d", repoFullName, prNumber)] = ports.OpenPR{
			Owner: "acme", Repo: repo, Number: int(prNumber), HTMLURL: htmlURL,
			HeadSHA: headSHA, CIConclusion: ports.CIConclusionSuccess,
		}
	}
	worker := rig.newWorker(t, sc)

	now0 := time.Now()
	if err := worker.PumpOnce(ctx, now0); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}
	if sc.mergeRendezvous.timedOut() {
		t.Fatal("mergeRendezvous timed out waiting for all armedRepos callers to arrive -- fewer than armedRepos candidates reached MergePR concurrently, so this run never actually exercised the race this test is named for")
	}

	// The real subject of this test: a single tick's worth of
	// GUARANTEED-concurrent contemporaneous failures must never walk the
	// whole backoff ladder in one shot.
	if entry := findAuditLogEntry(t, rig.pool, "automerge.auth_dead_lettered"); entry != nil {
		t.Fatalf("audit_log row with action=automerge.auth_dead_lettered already exists after ONE tick's worth of %d concurrent contemporaneous failures (detail=%s) -- want none yet: a single tick must never walk the whole backoff ladder in one shot", armedRepos, string(entry.DetailJson))
	}

	// Bracket that tick's own worth of failures at EXACTLY one,
	// deterministically -- see this test's own doc comment for why NOT
	// mergeCallCount() == armedRepos. The probe's own second tick is
	// itself a second real, mergeRendezvous-guaranteed concurrent wave;
	// its return value is unused here (nothing further to chain from),
	// but is available to a future caller wanting to bracket a THIRD
	// wave the same way.
	_ = assertExactlyOneNewWorkerFailure(t, ctx, worker, sc, now0, armedRepos, rendezvousTimeout)

	if entry := findAuditLogEntry(t, rig.pool, "automerge.auth_dead_lettered"); entry != nil {
		t.Fatalf("audit_log row with action=automerge.auth_dead_lettered already exists after a SECOND concurrent wave's own single coalesced failure (detail=%s) -- want none yet (domainautomerge.MaxAuthFailures=5 requires several more waves)", string(entry.DetailJson))
	}
}

// assertExactlyOneNewWorkerFailure asserts that, starting from a
// concurrent wave already driven at waveNow, the worker-wide scope just
// recorded EXACTLY one NEW consecutive classified failure -- bracketed
// via the backoff rung it produces (EvaluateBackoff, internal/domain/
// automerge/backoff.go), rather than by counting how many of armedRepos'
// own goroutines happened to overlap in wall-clock time (a scheduling-
// dependent property CI's own loaded runners do not guarantee, and not
// the invariant this test is named for -- see
// TestPumpOnce_ConcurrentArmedRepos_OneTickAuthFailure_CoalescesNotDeadLetters's
// own doc comment for the full "why").
//
// Issues two further ticks:
//
//  1. waveNow+1m (before the 2m rung elapses): must produce ZERO merge
//     attempts, proving the scope IS backing off (>= 1 failure was
//     recorded by waveNow's own wave).
//  2. waveNow+3m (past the 2m rung, before a second failure's own 4m
//     rung): must produce NON-ZERO merge attempts, proving the rung
//     reached was the FIRST one -- i.e. EXACTLY one failure, not two or
//     more. This second probe is itself a real, mergeRendezvous-
//     guaranteed concurrent wave across every one of armedRepos (the
//     worker-wide backoff clears for all of them at once, since they
//     share the one scope), so it doubles as this test's own proof that
//     coalescing holds on a SECOND wave, not merely the very first.
//
// Resets sc's own recorded merge calls, and its rendezvous barrier,
// before each probe. Returns the second probe's own tick time
// (waveNow+3m) so a caller can chain further assertions from it.
func assertExactlyOneNewWorkerFailure(t *testing.T, ctx context.Context, worker *automerge.Worker, sc *fakeAutoMergeSourceControl, waveNow time.Time, armedRepos int, rendezvousTimeout time.Duration) time.Time {
	t.Helper()

	sc.mu.Lock()
	sc.mergeCalls = nil
	sc.mu.Unlock()
	probeA := waveNow.Add(time.Minute)
	if err := worker.PumpOnce(ctx, probeA); err != nil {
		t.Fatalf("PumpOnce() probe tick (+1m from %v) error = %v, want nil", waveNow, err)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR call count at +1m from %v = %d, want 0 -- the worker-wide scope must still be backing off on the 2m rung EvaluateBackoff(1) sets, proving at least one failure was recorded (zero would mean allow() never blocked at all)", waveNow, got)
	}

	sc.mu.Lock()
	sc.mergeCalls = nil
	// A fresh barrier: this probe is itself a real concurrent wave (every
	// one of armedRepos unblocks together once the 2m rung clears), and
	// the ORIGINAL barrier is already spent (every Wait call beyond its
	// own n-th would either double-count an arrival past n or, if replayed
	// after a close, panic on a second close of the same channel).
	sc.mergeRendezvous = newRendezvousBarrier(armedRepos, rendezvousTimeout)
	sc.mu.Unlock()
	probeB := waveNow.Add(3 * time.Minute)
	if err := worker.PumpOnce(ctx, probeB); err != nil {
		t.Fatalf("PumpOnce() probe tick (+3m from %v) error = %v, want nil", waveNow, err)
	}
	if got := sc.mergeCallCount(); got == 0 {
		t.Fatalf("MergePR call count at +3m from %v = 0, want > 0 -- the 2m rung must have already elapsed by +3m; zero attempts here would mean the SECOND rung (4m, i.e. consecutiveFailures==2) was reached instead, proving MORE than one failure was recorded", waveNow)
	}
	sc.mu.Lock()
	timedOut := sc.mergeRendezvous.timedOut()
	sc.mu.Unlock()
	if timedOut {
		t.Fatalf("mergeRendezvous timed out waiting for all armedRepos callers to arrive during the +3m probe wave from %v", waveNow)
	}

	return probeB
}

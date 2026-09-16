//go:build integration

// Integration tests for RevalidateForMerge ("decision inbox:
// read model + API", §16.2's own Merge endpoint) against a REAL Postgres
// instance -- gated behind the "integration" build tag, same package
// (decisioninbox_test) and newTestPool/fakeDecisionInboxSourceControl
// precedent as aggregate_integration_test.go. Deliberately ONE shared
// pool across every subtest below (t.Run, not separate top-level Test
// functions) -- mirrors this package's own TestBuild_FullScenario, and
// this repo's own documented aversion to spinning up more testcontainers
// than necessary (Makefile's own top comment on test-integration).
package decisioninbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// revalidateStores bundles the Postgres stores RevalidateForMerge itself
// reads (SentinelFixes/ReviewFindings/Artifacts) plus a fake SourceControl
// (RevalidateForMerge's own direct parameter -- never deps.SCMCache, per
// that function's own doc comment) shared across every subtest below.
type revalidateStores struct {
	deps          decisioninbox.Deps
	sourceControl *fakeDecisionInboxSourceControl
}

func newRevalidateStores(pool *pgxpool.Pool) *revalidateStores {
	sc := &fakeDecisionInboxSourceControl{openPRsByExternalID: map[string][]ports.OpenPR{}}
	return &revalidateStores{
		sourceControl: sc,
		deps: decisioninbox.Deps{
			Plans:          narvipg.NewPlanStore(pool),
			Sessions:       narvipg.NewSessionStore(pool),
			Participants:   narvipg.NewParticipantStore(pool),
			Automations:    narvipg.NewAutomationStore(pool),
			Outbox:         narvipg.NewOutboxStore(pool, false),
			ReviewFindings: narvipg.NewReviewFindingStore(pool),
			SentinelFixes:  narvipg.NewSentinelFixStore(pool),
			Artifacts:      narvipg.NewArtifactStore(pool),
			Identities:     narvipg.NewIdentityStore(pool),
			// Timeouts (E4/E7, third adversarial-review round): this rig's
			// own top-level decisioninbox.Deps.Timeouts was never set at
			// all before this fix -- revalidateCore had no reason to read
			// it until E4 wired DecisionInboxIsAncestorTimeout into its own
			// IsAncestor call, at which point this rig's own zero-valued
			// Timeouts silently supplied a 0s timeout to every subtest in
			// this file, making the fast-forward-tolerance subtest below
			// (BaseBranchAdvanced_ConfirmedFastForward_NotRefused) fail --
			// exactly the "zero/missing timeout observable" property E7
			// added the fake's own ctx-honoring for, just caught in this
			// fixture rather than in production. Mirrors ReviewVerdict.
			// Timeouts immediately below (already set, for a different
			// reason) and aggregate_integration_test.go's own rig, which
			// already sets this top-level field for its own pre-existing
			// deps.Timeouts.DecisionInboxStaleAfter consumer.
			Timeouts: platform.DefaultTimeouts(),
			// (§21.1/§21.2): the REAL auto-approval eligibility
			// engine's own store dependencies -- revalidateCore now reads
			// review_verdicts/repo_settings through this bundle.
			ReviewVerdict: appreviewverdict.Deps{
				ReviewVerdicts:       narvipg.NewReviewVerdictStore(pool),
				RepoSettings:         narvipg.NewRepoSettingsStore(pool),
				ReviewFindings:       narvipg.NewReviewFindingStore(pool),
				AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
				Timeouts:             platform.DefaultTimeouts(),
			},
		},
	}
}

// eligiblePR seeds a FULLY ELIGIBLE target PR for (repoFullName, prNumber)
// under actorGitHubID -- platform-authored (an artifacts row is created),
// low-risk, CI green, no findings, no needs-human label, no changes
// requested, not a draft/handoff/sentinel-fix. Every test below starts
// from this exact baseline and perturbs ONE fact (
// "the row renders ready_to_merge, then X changes -> assert the merge is
// refused"), proving each negative check independently actually gates
// the merge rather than merely existing in the source.
func (rs *revalidateStores) eligiblePR(ctx context.Context, t *testing.T, pool *pgxpool.Pool, actorGitHubID, repoFullName string, prNumber int) ports.OpenPR {
	t.Helper()

	owner, repo, ok := reposource.SplitFullName(repoFullName)
	if !ok {
		t.Fatalf("malformed repoFullName %q", repoFullName)
	}
	htmlURL := "https://github.com/" + repoFullName + "/pull/" + strconv.Itoa(prNumber)

	pr := ports.OpenPR{
		Owner: owner, Repo: repo, Number: prNumber,
		Title: "eligible pr", HTMLURL: htmlURL, HeadSHA: "sha-" + strconv.Itoa(prNumber),
		BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
		Assignees:    []ports.PRPerson{{ExternalID: actorGitHubID, Login: "actor"}},
		CIConclusion: ports.CIConclusionSuccess,
		Labels:       []string{"review:low-risk"},
	}
	rs.sourceControl.openPRsByExternalID[actorGitHubID] = append(rs.sourceControl.openPRsByExternalID[actorGitHubID], pr)

	// Platform-authored: a real artifacts row of type 'pr' at this exact
	// URL (isPlatformAuthored's own signal).
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	// (§21.1/§21.2): a matching Shippable=auto review_verdicts
	// row, at this exact head sha, is now REQUIRED before the real
	// eligibility engine will ever say ok=true -- see
	// seedAutoApprovedVerdict's own doc comment (aggregate_integration_test.go)
	// for why every "eligible baseline" fixture in this package needs one.
	seedAutoApprovedVerdict(ctx, t, pool, repoFullName, int32(prNumber), pr.HeadSHA)
	if _, err := narvipg.NewArtifactStore(pool).Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}

	return pr
}

// replaceTargetPR overwrites the ONE seeded OpenPR for actorGitHubID with
// a mutated copy -- the mechanism every negative subtest below uses to
// perturb exactly one fact off of the eligiblePR baseline.
func (rs *revalidateStores) replaceTargetPR(actorGitHubID string, pr ports.OpenPR) {
	rs.sourceControl.openPRsByExternalID[actorGitHubID] = []ports.OpenPR{pr}
}

// TestRevalidateForMerge_NegativeCases covers every fact RevalidateForMerge
// re-checks that used to have zero coverage (
// CRITICAL): the CI-green re-check, the needs-human label, the §17
// sentinel-fix exclusion, draft, handoff, and (
// folded in here since it is the SAME "the row renders ready_to_merge,
// then a fact changes" shape) HasChangesRequested. Each subtest starts
// from eligiblePR's own fully-eligible baseline and perturbs ONE fact.
func TestRevalidateForMerge_NegativeCases(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-actor-1"

	t.Run("EligibleBaseline_MergesOK", func(t *testing.T) {
		const repoFullName = "acme/revalidate-eligible"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 1)

		ok, headSHA, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("RevalidateForMerge() ok = false, reason = %q, want true (fixture is fully eligible)", reason)
		}
		if headSHA != pr.HeadSHA {
			t.Errorf("headSHA = %q, want %q", headSHA, pr.HeadSHA)
		}
	})

	t.Run("CIRed_Refused", func(t *testing.T) {
		// this is the one guard that directly
		// exercises the merge gate's own strict CI check -- deletable
		// before this test existed.
		const repoFullName = "acme/revalidate-ci-red"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 2)
		pr.CIConclusion = ports.CIConclusionFailure
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (CI is red)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// a degraded review-decision
	// read (GitHub's own reviews endpoint failed) must refuse the merge
	// exactly like a CONFIRMED changes-request would -- "we could not
	// tell" must never silently satisfy this gate. This is the ONE
	// subtest that exercises the unattended-merge-reachable half of C4's
	// fix directly (revalidateCore is shared by RevalidateForAutoMerge,
	// the auto-merge worker's own entry point).
	t.Run("ReviewDecisionDegraded_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-review-decision-degraded"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 100)
		pr.ReviewDecisionDegraded = true
		// HasChangesRequested stays false (its own honest zero value) --
		// proving this refusal fires on ReviewDecisionDegraded alone, not
		// on a coincidentally-true HasChangesRequested.
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (the review-decision read was degraded -- must fail closed)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// Phase 5 audit finding 1 (fixed) at the real revalidateCore wiring,
	// the audit's own named scenario: "GET /pulls/{n}/files returns 502
	// during aggregation or auto-merge revalidation". ChangedFiles stays
	// nil/empty (its own honest zero value, exactly what githubapi now
	// still returns on a fetch failure) -- ChangedFilesListDegraded=true
	// alone must refuse, never silently pass as "confirmed zero files,
	// nothing sensitive". Mutation-test target: reverting revalidateCore
	// back to `TouchedBlastRadiusKnown: true` unconditionally (or
	// dropping the field from the EligibilityInput literal entirely) must
	// turn this subtest's own "ok = true" assertion from a failure back
	// into a pass.
	t.Run("ChangedFilesListDegraded_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-changed-files-degraded"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 101)
		pr.ChangedFilesListDegraded = true
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- a degraded/swallowed changed-files read (a failed or page-truncated GitHub fetch) must never silently pass as 'confirmed clean'")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// Phase 5 audit finding 2 (fixed): the diff-size gate must use
	// ports.OpenPR.ChangedFilesCount (GitHub's own authoritative scalar),
	// never len(ChangedFiles) -- deliberately kept ChangedFilesListDegraded
	// FALSE here (unlike the subtest immediately above) to isolate the
	// size gate specifically: a caller that reverted to deriving
	// ChangedFileCount from len(target.ChangedFiles) would see only 3
	// harmless-looking paths and pass, even though GitHub's own scalar
	// says this PR genuinely touches far more files than this repo's
	// configured threshold allows. Mutation-test target: reverting
	// revalidateCore's `ChangedFileCount: target.ChangedFilesCount` back
	// to `len(target.ChangedFiles)` must turn this subtest's own
	// "ok = true" assertion from a failure back into a pass.
	t.Run("ChangedFilesCountScalarExceedsThreshold_RefusedDespiteSmallFetchedListing", func(t *testing.T) {
		const repoFullName = "acme/revalidate-changed-files-scalar"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 102)
		// A truncated GitHub Pull Request Files page would realistically
		// cap at 100 entries, not 3 -- but this fixture only needs SOME
		// small, non-sensitive listing to prove the size gate does not
		// read it via len(), so an intentionally tiny slice makes the
		// point most clearly.
		pr.ChangedFiles = []string{"README.md", "go.mod", "go.sum"}
		pr.ChangedFilesCount = 9999 // GitHub's own authoritative scalar: a genuinely huge diff
		pr.ChangedFilesListDegraded = false
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- the diff-size gate must read ChangedFilesCount (GitHub's own scalar), never len(ChangedFiles), which a PR author can pad past a page boundary")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// Paired with review:low-risk deliberately -- an otherwise-fully-
	// eligible risk label -- so needs-human is provably the ONE thing
	// keeping this refused, not a coincidental "no/unrecognized risk
	// label" refusal: this subtest and what used
	// to be a separate "NeedsHumanWithLowRisk_Refused" subtest were
	// byte-identical, both pairing needs-human with low-risk -- the
	// latter was deleted as pure duplication, adding zero coverage
	// despite its own comment's claim to cover "the OTHER half").
	t.Run("NeedsHumanLabel_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-needs-human"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 3)
		pr.Labels = []string{"review:low-risk", "review:needs-human"}
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (review:needs-human label applied)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	t.Run("SentinelFixFollowUp_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-sentinel"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 4)

		originSession, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
		if err != nil {
			t.Fatalf("create origin session: %v", err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		fix, err := rs.deps.SentinelFixes.WithTx(tx).Claim(ctx, repoFullName, 999, originSession.ID, "origin-branch")
		if err != nil {
			t.Fatalf("claim sentinel fix: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit tx: %v", err)
		}
		// UpdateOpened(fix.ID, pr.Number) registers THIS pr.Number as the
		// fix PR itself -- §17's own structural exclusion, checked
		// against sentinel_fixes.fix_pr_number.
		if _, err := rs.deps.SentinelFixes.UpdateOpened(ctx, fix.ID, int32(pr.Number)); err != nil {
			t.Fatalf("update sentinel fix opened: %v", err)
		}

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (this PR is a registered sentinel-auto-fix follow-up)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	t.Run("Draft_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-draft"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 5)
		pr.Draft = true
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (this PR is a draft)")
		}
		// This subtest is DOUBLE-GATED by
		// ComputeAutoApprovalEligible's own IsDraft input, so deleting
		// RevalidateForMerge's explicit `if target.Draft` early return
		// still passes an `ok`-only/reason-non-empty assertion -- the PR
		// still refuses, just via the generic downstream eligibility
		// check instead, for a DIFFERENT reason. This exact trap already
		// cost one previous review round. Assert the reason NAMES
		// "draft" specifically -- the fallback eligibility-based
		// refusal's own reason text never mentions "draft" at all, so
		// only the explicit check produces this exact string.
		if !strings.Contains(reason, "draft") {
			t.Errorf("reason = %q, want it to name \"draft\" specifically (proves the explicit draft check fired, not a coincidental eligibility refusal for some unrelated reason)", reason)
		}
	})

	// the handoff-label branch, merge-path half
	// (aggregate.go's own read-path half lives in
	// aggregate_integration_test.go's TestBuild_HandoffPR).
	t.Run("HandoffLabel_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-handoff"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 6)
		pr.Labels = []string{"review:low-risk", "handoff"}
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (this PR carries the handoff label)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// HasChangesRequested is a hard merge blocker
	// -- the one review-decision fact this endpoint's own "approval
	// state" re-check actually gates on (never HasApprovingReview, which
	// is display-only -- see RevalidateForMerge's own doc comment).
	t.Run("HasChangesRequested_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-changes-requested"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 7)
		pr.HasChangesRequested = true
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (a reviewer requested changes)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// (§21.2) note: classifyPRLabels' own "most restrictive risk
	// label wins" property is no longer
	// observable through RevalidateForMerge's own ok/refused OUTCOME --
	// the real auto-approval eligibility engine (internal/domain/
	// autoapproval) gates on the STORED verdict's own Shippable field,
	// never on a PR's GitHub risk labels at all (those labels are now
	// purely a display artifact synced FROM a past verdict, §8.2,
	// never fed back INTO eligibility). Coverage for classifyPRLabels'
	// own precedence rule moved to labels_test.go's own
	// TestClassifyPRLabels_MostRestrictiveRiskLabelWins, which asserts
	// the returned riskLabel value directly rather than through this
	// function's now-unrelated merge-eligibility outcome.

	// (§21.2): a PR with NO review_verdicts row at all must be
	// refused -- the auto-approval engine fails CLOSED on "no verdict on
	// record", never defaulting to eligible for lack of data.
	t.Run("NoVerdictOnRecord_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-no-verdict"
		owner, repo, ok := reposource.SplitFullName(repoFullName)
		if !ok {
			t.Fatalf("malformed repoFullName %q", repoFullName)
		}
		htmlURL := "https://github.com/" + repoFullName + "/pull/30"
		pr := ports.OpenPR{
			Owner: owner, Repo: repo, Number: 30,
			Title: "no verdict yet", HTMLURL: htmlURL, HeadSHA: "sha-30",
			Assignees:    []ports.PRPerson{{ExternalID: actorGitHubID, Login: "actor"}},
			CIConclusion: ports.CIConclusionSuccess,
		}
		rs.sourceControl.openPRsByExternalID[actorGitHubID] = append(rs.sourceControl.openPRsByExternalID[actorGitHubID], pr)
		session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
		if err != nil {
			t.Fatalf("create platform session: %v", err)
		}
		if _, err := narvipg.NewArtifactStore(pool).Create(ctx, sqlcgen.CreateArtifactParams{
			SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
		}); err != nil {
			t.Fatalf("create pr artifact: %v", err)
		}
		// Deliberately NO seedAutoApprovedVerdict call -- this is the
		// fact under test.

		ok2, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok2 {
			t.Fatal("RevalidateForMerge() ok = true, want false (no review_verdicts row exists for this PR)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// (§21.2): the stale-head-SHA guard -- a verdict on record
	// whose own head_sha no longer matches the PR's LIVE current head
	// must refuse, no matter how clean that verdict once looked.
	t.Run("StaleVerdictHeadSHA_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-stale-verdict"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 31)
		// A new commit landed after the verdict was posted -- the live
		// PR's own head sha has moved on, but review_verdicts.head_sha
		// (seeded by eligiblePR against the OLD pr.HeadSHA) has not.
		pr.HeadSHA = "sha-31-newer-commit"
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (the verdict on record was produced against an earlier commit)")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// (§21.1's amendment): the decisive case head-SHA equality alone
	// cannot catch -- the PR's own head has NOT moved (unlike
	// StaleVerdictHeadSHA_Refused immediately above), but its BASE has:
	// "a PR evaluated while based on another PR's branch, then
	// retargeted -- or whose parent moved beneath it -- keeps an
	// unchanged head, so the equality holds and the stale verdict reads
	// as fresh." This test would pass with no code change at all if it
	// only checked HeadSHA equality (it still equals headSHA) -- it is
	// the live BaseRef perturbation below, and ComputeEligible's own new
	// context comparison, that must be what actually refuses it.
	t.Run("RetargetedBase_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-retargeted-base"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 32)
		// The PR's own head is UNCHANGED -- only its base moved (a
		// GitHub retarget onto a different branch, or its stacked
		// parent's own branch moving beneath it). review_verdicts'
		// own recorded base (seeded by eligiblePR against
		// testEligibleBaseRef) no longer matches.
		pr.BaseRef = "release/2026.09"
		rs.replaceTargetPR(actorGitHubID, pr)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- THE HAZARD THIS AMENDMENT CLOSES: the head sha is unchanged, but the base has moved, and the stale verdict must not read as fresh")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// (finding F1): THE DECISIVE CASE this finding is named
	// for -- base ref unchanged, PR head unchanged, and even GitHub's own
	// `pull_request.base.sha` field (ports.OpenPR.BaseSHA, what this
	// fixture's own BaseSHA models) unchanged -- yet the base BRANCH's
	// REAL, live tip has advanced. Verified against real GitHub PRs
	// (golang/go PR #27813, ten open kubernetes/kubernetes PRs): that
	// field is a per-PR cached snapshot GitHub refreshes on its own
	// schedule, not on every push to the base branch, so it can -- and in
	// practice does -- stay frozen for months while the branch moves on.
	// Comparing it against itself detects NOTHING.
	//
	// This test fails against the PRE-FIX code: revalidateCore used to
	// read target.BaseSHA (this fixture's own frozen BaseSHA field)
	// directly as CurrentBaseSHA, which trivially equals the verdict's
	// own recorded BaseSHA (both testEligibleBaseSHA) -- eligible=true,
	// the bug. The fix instead resolves CurrentBaseSHA via a fresh
	// sourceControl.ResolveBranchSHA call, modeled here by the fake's own
	// resolveBranchSHA override reporting a DIFFERENT commit -- proving
	// the base branch's own live tip, not GitHub's cached field, is what
	// this gate actually compares now.
	t.Run("BaseBranchAdvanced_LiveTipMovedWhileGitHubsCachedBaseSHAFieldDidNot_Refused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-base-branch-advanced"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 33)
		// pr.BaseRef/pr.BaseSHA (GitHub's own possibly-stale
		// pull_request.base.sha snapshot) are BOTH left exactly as
		// eligiblePR seeded them (testEligibleBaseRef/testEligibleBaseSHA,
		// the SAME values review_verdicts' own recorded context carries)
		// -- neither the ref nor GitHub's cached field moved at all.
		rs.sourceControl.resolveBranchSHA = "sha-main-has-actually-advanced"
		defer func() { rs.sourceControl.resolveBranchSHA = "" }()

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- THE DECISIVE F1 HAZARD: base ref, head sha, AND GitHub's own cached base.sha field are all unchanged, but the base branch's REAL live tip advanced -- the stale verdict must not read as fresh")
		}
		if reason == "" {
			t.Error("reason is empty, want a human-readable explanation")
		}
	})

	// D3 (second adversarial-review round): "any unrelated merge to trunk
	// permanently disqualifies a verdict" -- the EXACT SAME base-tip
	// movement as BaseBranchAdvanced_LiveTipMovedWhileGitHubsCachedBaseSHAFieldDidNot_Refused
	// immediately above, but this time the fake's own IsAncestor call
	// CONFIRMS the movement was a pure fast-forward (an ordinary,
	// unrelated merge landing on the base branch, never a rewrite) --
	// RevalidateForMerge must NOT refuse it. This is what makes auto-merge
	// able to regain eligibility through an ordinary sequence of events
	// rather than being stuck the moment any base movement is observed at
	// all.
	t.Run("BaseBranchAdvanced_ConfirmedFastForward_NotRefused", func(t *testing.T) {
		const repoFullName = "acme/revalidate-base-branch-advanced-confirmed"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 34)
		rs.sourceControl.resolveBranchSHA = "sha-main-has-actually-advanced"
		rs.sourceControl.isAncestorResult = true
		// The PRECEDING subtest (BaseBranchAdvanced_LiveTipMovedWhile...)
		// ALSO triggers an IsAncestor call against the SAME shared fake --
		// reset here, at the START, rather than relying solely on that
		// subtest's own defer/ordering to have cleared it first.
		rs.sourceControl.isAncestorCalls = nil
		defer func() {
			rs.sourceControl.resolveBranchSHA = ""
			rs.sourceControl.isAncestorResult = false
			rs.sourceControl.isAncestorCalls = nil
		}()

		ok, headSHA, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("RevalidateForMerge() ok = false, reason = %q, want true -- D3: a base movement CONFIRMED as a pure fast-forward must not refuse an otherwise-eligible PR", reason)
		}
		if headSHA != pr.HeadSHA {
			t.Errorf("headSHA = %q, want %q", headSHA, pr.HeadSHA)
		}
		if len(rs.sourceControl.isAncestorCalls) != 1 {
			t.Fatalf("IsAncestor called %d times, want 1", len(rs.sourceControl.isAncestorCalls))
		}
		gotCall := rs.sourceControl.isAncestorCalls[0]
		if gotCall.Ancestor != testEligibleBaseSHA || gotCall.Descendant != "sha-main-has-actually-advanced" {
			t.Errorf("IsAncestor(Ancestor, Descendant) = (%q, %q), want (%q, %q) -- the verdict's OWN recorded base sha as the candidate ancestor, the LIVE resolved tip as the descendant",
				gotCall.Ancestor, gotCall.Descendant, testEligibleBaseSHA, "sha-main-has-actually-advanced")
		}
	})

	// TestRevalidateForMerge_NegativeCases/BaseBranchAdvanced_ButAlreadyIneligibleForAnotherReason
	// is G3's own regression test (fourth adversarial-review round): this
	// PR carries review:needs-human (a PERMANENT refusal, until a
	// maintainer removes it) AND a base-SHA movement whose own live
	// ancestor confirmation would fail if it were ever attempted. Before
	// this fix, the ancestor-check branch ran unconditionally and its own
	// failure preempted ComputeEligible entirely, so this PR would have
	// been told the TRANSIENT "try again shortly" -- exactly wrong for a
	// PR that is not going to become eligible no matter how many times the
	// caller retries. isAncestorErr is set specifically so that if the
	// probe (this fix) is ever bypassed or removed, the live call would
	// fail and produce the OLD, wrong "try again shortly" message instead
	// of this test's own expected reason -- making a regression to the
	// pre-G3 behavior visible as a WRONG reason string, not merely a
	// silent behavior change.
	t.Run("BaseBranchAdvanced_ButAlreadyIneligibleForAnotherReason", func(t *testing.T) {
		const repoFullName = "acme/revalidate-base-advanced-and-needs-human"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 37)
		pr.Labels = []string{"review:low-risk", "review:needs-human"}
		rs.replaceTargetPR(actorGitHubID, pr)
		rs.sourceControl.resolveBranchSHA = "sha-main-has-actually-advanced"
		rs.sourceControl.isAncestorErr = errors.New("boom: github is down")
		rs.sourceControl.isAncestorCalls = nil
		defer func() {
			rs.sourceControl.resolveBranchSHA = ""
			rs.sourceControl.isAncestorErr = nil
			rs.sourceControl.isAncestorCalls = nil
		}()

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false (review:needs-human label applied)")
		}
		const wantReason = "this pull request no longer meets the auto-approval eligibility criteria: review:needs-human label is present"
		if reason != wantReason {
			t.Errorf("reason = %q, want %q -- G3: the permanent, already-known needs-human refusal must win over the base movement's own unconfirmed, transient state", reason, wantReason)
		}
		if len(rs.sourceControl.isAncestorCalls) != 0 {
			t.Errorf("IsAncestor called %d times, want 0 -- G3/G4: a PR already ineligible on an independent, fully-known criterion must never spend a live ancestor-confirmation call that could not have changed the outcome", len(rs.sourceControl.isAncestorCalls))
		}
	})

	// E6 (third adversarial-review round): the SAME precondition as
	// BaseBranchAdvanced_ConfirmedFastForward_NotRefused immediately
	// above -- base ref unchanged, base sha genuinely differs on both
	// sides -- but this time the fake's own IsAncestor call itself FAILS,
	// rather than confirming or refuting the movement. Before this fix,
	// revalidateCore (the merge-gate path) swallowed that error with no
	// log at all -- unlike its read-model sibling in aggregate.go, which
	// already logs -- and fell through to autoapproval.ComputeEligible's
	// generic ReasonBaseMoved ("the pull request's base has changed since
	// this verdict was produced"): true, but not the whole truth. The
	// base SHA genuinely did change (that part IS confirmed, or this
	// branch is never reached at all), but whether that change was a
	// safe fast-forward or a genuine rewrite is exactly what the failed
	// IsAncestor call could not determine -- "the base changed" and "we
	// could not check whether tolerating that change was safe" are
	// different facts, and conflating them tells an operator retrying is
	// pointless when it may well not be.
	t.Run("BaseBranchAdvanced_AncestorCheckFails_LogsAndReturnsHonestReason", func(t *testing.T) {
		const repoFullName = "acme/revalidate-ancestor-check-fails"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 35)
		rs.sourceControl.resolveBranchSHA = "sha-main-has-actually-advanced"
		rs.sourceControl.isAncestorErr = errors.New("boom: github is down")
		// Mirrors the preceding subtest's own "reset at the START" note --
		// the SAME shared fake carries isAncestorCalls across subtests.
		rs.sourceControl.isAncestorCalls = nil
		defer func() {
			rs.sourceControl.resolveBranchSHA = ""
			rs.sourceControl.isAncestorErr = nil
			rs.sourceControl.isAncestorCalls = nil
		}()

		buf := captureDefaultLoggerJSON(t)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil -- an unconfirmable ancestry check is a domain refusal, never a Go error", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- a base movement that could not be confirmed safe must still refuse the merge")
		}
		const wantReason = "this pull request's base has changed since this verdict was produced, and whether that change was safe to tolerate could not be confirmed (a live check failed) -- try again shortly"
		if reason != wantReason {
			t.Errorf("reason = %q, want %q -- E6: a FAILED ancestry check must read differently from autoapproval.ReasonBaseMoved's own generic \"the base has changed\" text, which this code path must never fall through to", reason, wantReason)
		}
		if !hasLogEntry(t, buf, "decisioninbox: resolve base-advanced-without-rewrite ancestry failed, refusing merge -- could not confirm whether the base's forward movement was safe to tolerate") {
			t.Errorf("did not find the dedicated ancestor-check-failure log line -- E6: this call previously swallowed the error with no log at all; full log:\n%s", buf.String())
		}
	})

	// TestRevalidateForMerge_ZeroIsAncestorTimeout_TreatedAsAlreadyExpired
	// is G5's own regression test (fourth adversarial-review round) --
	// the actual proof that E4's fix (bounding revalidateCore's own
	// IsAncestor call with platform.Timeouts.DecisionInboxIsAncestorTimeout,
	// third round) is wired into the real call site, which nothing in
	// this file previously asserted: the subtest immediately above drives
	// isAncestorErr directly, which proves the FAILURE PATH is handled,
	// never that a timeout genuinely bounds the call. fakeDecisionInboxSourceControl.
	// IsAncestor's own ctx.Err()-checked-first mechanism (E7, third round)
	// exists to make exactly this provable, but its own doc comment
	// overclaimed that this already held (G5) -- no test anywhere set
	// DecisionInboxIsAncestorTimeout to a non-positive value. A ZERO
	// timeout makes context.WithTimeout construct an ALREADY-EXPIRED
	// context deterministically (context's own documented behavior for a
	// non-positive duration -- no sleep/wall-clock dependency needed);
	// isAncestorResult is deliberately set to TRUE (a value that would
	// otherwise confirm the base movement and let this PR through) so
	// that only the ctx.Err() check standing between it and a false
	// "eligible" answer is what this test actually exercises: strip the
	// context.WithTimeout wrap off the real call site (reverting it to
	// the bare ctx this test hands RevalidateForMerge) and the fake sees
	// a live, un-expired context, skips the ctx.Err() branch entirely,
	// and returns isAncestorResult=true -- eligible, not refused -- which
	// is exactly the mutation this test exists to catch.
	t.Run("ZeroIsAncestorTimeout_TreatedAsAlreadyExpired_RefusesRatherThanSucceeding", func(t *testing.T) {
		const repoFullName = "acme/revalidate-zero-ancestor-timeout"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 36)
		rs.sourceControl.resolveBranchSHA = "sha-main-has-actually-advanced"
		rs.sourceControl.isAncestorResult = true
		rs.sourceControl.isAncestorCalls = nil
		savedTimeout := rs.deps.Timeouts.DecisionInboxIsAncestorTimeout
		rs.deps.Timeouts.DecisionInboxIsAncestorTimeout = 0
		defer func() {
			rs.sourceControl.resolveBranchSHA = ""
			rs.sourceControl.isAncestorResult = false
			rs.sourceControl.isAncestorCalls = nil
			rs.deps.Timeouts.DecisionInboxIsAncestorTimeout = savedTimeout
		}()

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil -- an unconfirmable ancestry check is a domain refusal, never a Go error", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- a zero timeout must make this call fail closed, never silently succeed as though the context.WithTimeout wrap were never applied")
		}
		const wantReason = "this pull request's base has changed since this verdict was produced, and whether that change was safe to tolerate could not be confirmed (a live check failed) -- try again shortly"
		if reason != wantReason {
			t.Errorf("reason = %q, want %q", reason, wantReason)
		}
		if len(rs.sourceControl.isAncestorCalls) != 1 {
			t.Fatalf("IsAncestor called %d times, want 1 -- the ancestor-check branch must actually have been entered for this test to exercise anything", len(rs.sourceControl.isAncestorCalls))
		}
	})

	// TestRevalidateForMerge_NegativeCases/ZeroResolveBranchSHATimeout is
	// G4's own regression test (fourth adversarial-review round),
	// mirroring ZeroIsAncestorTimeout immediately above one live call
	// earlier: proves platform.Timeouts.DecisionInboxResolveBranchSHATimeout
	// genuinely bounds revalidateCore's own ResolveBranchSHA call, which
	// previously ran on the bare, unbounded ctx this function was handed.
	// A ZERO timeout makes context.WithTimeout construct an
	// ALREADY-EXPIRED context deterministically; fakeDecisionInboxSourceControl.
	// ResolveBranchSHA's own ctx.Err()-checked-first mechanism (this fix)
	// then fails the call before ever falling back to its seeded PR scan,
	// which would otherwise happily return testEligibleBaseSHA (a MATCH,
	// letting this fully-eligible PR through) -- so only the
	// context.WithTimeout wrap genuinely being applied stands between
	// this test's expected refusal and a false "eligible". The expected
	// reason/log below were updated for H2 (fifth adversarial-review
	// round): this resolution failure now returns EARLY with its own
	// honest, logged reason (mirroring the ancestor-check failure's own
	// E6/G3 precedent) rather than falling through to ComputeEligible's
	// generic ReasonBaseSHAUnknown, which this test previously (and
	// wrongly) pinned as the expected outcome.
	t.Run("ZeroResolveBranchSHATimeout_TreatedAsAlreadyExpired_RefusesRatherThanSucceeding", func(t *testing.T) {
		const repoFullName = "acme/revalidate-zero-resolve-branch-sha-timeout"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 38)
		savedTimeout := rs.deps.Timeouts.DecisionInboxResolveBranchSHATimeout
		rs.deps.Timeouts.DecisionInboxResolveBranchSHATimeout = 0
		defer func() {
			rs.deps.Timeouts.DecisionInboxResolveBranchSHATimeout = savedTimeout
		}()

		buf := captureDefaultLoggerJSON(t)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- a zero timeout must make this call fail closed, never silently succeed as though the context.WithTimeout wrap were never applied")
		}
		const wantReason = "this pull request's base commit could not be confirmed (a live check failed) -- try again shortly"
		if reason != wantReason {
			t.Errorf("reason = %q, want %q -- H2: a FAILED base-SHA resolution must read differently from autoapproval.ReasonBaseSHAUnknown's own generic \"could not be established\" text, which this code path must never fall through to", reason, wantReason)
		}
		if !hasLogEntry(t, buf, "decisioninbox: resolve base branch's live tip failed, refusing merge -- could not confirm the pull request's current base commit") {
			t.Errorf("did not find the dedicated resolve-branch-sha-failure log line -- H2: this call previously swallowed the error with no log at all; full log:\n%s", buf.String())
		}
	})

	// TestRevalidateForMerge_NegativeCases/ResolveBranchSHAFails_LogsAndReturnsHonestReason
	// is H2's own direct-error regression test (fifth adversarial-review
	// round), mirroring BaseBranchAdvanced_AncestorCheckFails_LogsAndReturnsHonestReason
	// above one live call earlier: drives fakeDecisionInboxSourceControl's
	// own resolveBranchSHAErr directly (rather than via a zero timeout,
	// the preceding subtest's own mechanism) so this failure path is
	// pinned independently of that one, exactly as the ancestor-check
	// failure and its own zero-timeout sibling are pinned independently
	// of each other above.
	t.Run("ResolveBranchSHAFails_LogsAndReturnsHonestReason", func(t *testing.T) {
		const repoFullName = "acme/revalidate-resolve-branch-sha-fails"
		pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 39)
		rs.sourceControl.resolveBranchSHAErr = errors.New("boom: github is down")
		defer func() {
			rs.sourceControl.resolveBranchSHAErr = nil
		}()

		buf := captureDefaultLoggerJSON(t)

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
		if err != nil {
			t.Fatalf("RevalidateForMerge() error = %v, want nil -- an unconfirmable base-sha resolution is a domain refusal, never a Go error", err)
		}
		if ok {
			t.Fatal("RevalidateForMerge() ok = true, want false -- a base-sha resolution that could not be confirmed must still refuse the merge")
		}
		const wantReason = "this pull request's base commit could not be confirmed (a live check failed) -- try again shortly"
		if reason != wantReason {
			t.Errorf("reason = %q, want %q", reason, wantReason)
		}
		if !hasLogEntry(t, buf, "decisioninbox: resolve base branch's live tip failed, refusing merge -- could not confirm the pull request's current base commit") {
			t.Errorf("did not find the dedicated resolve-branch-sha-failure log line -- H2: this call previously swallowed the error with no log at all; full log:\n%s", buf.String())
		}
	})

	// RevalidateForMerge's own
	// truncated->500 branch was never executed by any existing test --
	// when the target PR is not found in a TRUNCATED (partial/degraded)
	// read, this must return a genuine ERROR (the httpapi handler maps
	// this to a 500, prompting a client retry), never the confident "not
	// assigned to you" 409 a complete-but-empty read would legitimately
	// warrant: a degraded read cannot tell "genuinely not yours" apart
	// from "we simply failed to see it". Deliberately the LAST subtest
	// in this test function -- it mutates the shared fake's own
	// openPRsTruncated flag, reset via defer, but sequencing it last
	// avoids any risk of bleeding into an EARLIER subtest were t.Run's
	// own declaration-order-execution guarantee ever to change.
	t.Run("TruncatedReadWithoutTargetPR_ReturnsErrorNeverAConfident409", func(t *testing.T) {
		rs.sourceControl.openPRsTruncated = true
		defer func() { rs.sourceControl.openPRsTruncated = false }()

		ok, _, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, "acme/revalidate-truncated-not-found", 9999, "tok")
		if err == nil {
			t.Fatal("RevalidateForMerge() error = nil, want a non-nil error -- a truncated read must never confidently deny, it must fail loudly instead so the caller retries")
		}
		if ok {
			t.Error("RevalidateForMerge() ok = true, want false")
		}
		if reason != "" {
			t.Errorf("reason = %q, want empty (this is an error return, not a domain refusal reason)", reason)
		}
	})
}

// TestRevalidateForMerge_EligibilityConfigStoreError_FailsClosed is the C3
// regression test: a GENUINE repo_settings
// read error (never pgx.ErrNoRows) resolving this repo's own §21.2
// eligibility config must refuse the merge outright (a propagated error,
// mirroring this function's own existing §17 sentinel-fix-exclusion/
// open-findings-count error propagation) -- BEFORE this fix,
// LoadEligibilityConfig silently substituted the engine's own WIDER
// built-in defaults here, which could turn a genuinely-ineligible PR (one
// this repo's own narrower configured threshold/tag list would refuse)
// into an apparently-eligible one purely because Postgres could not be
// read this instant. Uses the SAME "already-rolled-back-tx fault
// injection" this package's own aggregate_integration_test.go already
// establishes (TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub)
// -- every query through a dead tx fails with a real pgx error, never
// pgx.ErrNoRows, with no need for a second container or a real outage.
func TestRevalidateForMerge_EligibilityConfigStoreError_FailsClosed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-actor-c3"
	const repoFullName = "acme/revalidate-eligibility-config-error"

	pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, 101)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	// brokenRepoSettings wraps the now-dead tx above -- every query through
	// it (including the one LoadEligibilityConfig issues) fails immediately
	// with a real pgx error, never pgx.ErrNoRows.
	rs.deps.ReviewVerdict.RepoSettings = narvipg.NewRepoSettingsStore(pool).WithTx(tx)

	ok, headSHA, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, pr.Number, "tok")
	if err == nil {
		t.Fatal("RevalidateForMerge() error = nil, want a non-nil error -- a genuine eligibility-config store error must fail CLOSED (refuse), never silently fall back to the engine's own wider defaults")
	}
	if ok {
		t.Error("RevalidateForMerge() ok = true, want false")
	}
	if headSHA != "" {
		t.Errorf("headSHA = %q, want empty on an error return", headSHA)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty (this is an error return, not a domain refusal reason)", reason)
	}
}

// captureDefaultLoggerJSON/hasLogEntry mirror internal/adapters/inbound/
// httpapi's own identical precedent (planapprove_integration_test.go's
// captureDefaultLoggerJSON/findLogEntry) -- platform.Logger(ctx), what
// revalidateCore actually calls, is itself built on top of
// slog.Default(), so redirecting THAT is how a test observes what got
// logged. Copied locally rather than shared cross-package (both are
// small, unexported test helpers with no natural shared home).
func captureDefaultLoggerJSON(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })
	return &buf
}

func hasLogEntry(t *testing.T, buf *bytes.Buffer, wantMsg string) bool {
	t.Helper()
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == wantMsg {
			return true
		}
	}
	return false
}

// TestRevalidateForMerge_LyingVerdictAgainstReal300FileSensitivePR is the
// C1 regression test at the FULL
// system level -- the exact attack the reviewers verified reproducible
// end to end: a review agent (whose own input includes the untrusted PR
// diff, §5.2) posts riskLevel=low/premise=ok/testsCoverage=adequate (so
// the server-computed Shippable legitimately computes to auto), PLUS
// filesChanged=1 and an empty blastRadius, for a PR that in REALITY
// rewrites 300 files under migrations/ and internal/domain/authz/. Both
// gates (diff-size, sensitive-path) must independently refuse eligibility
// once server-derived facts (ports.OpenPR.ChangedFiles, fetched via
// GitHub's own Pull Request Files API) gate them instead of the posted
// verdict's own self-report -- exercised here through RevalidateForMerge,
// the SAME core (revalidateCore) RevalidateForAutoMerge shares with the
// UNATTENDED auto-merge worker, so this proves the attack fails on
// exactly the path where it would otherwise merge code unattended.
func TestRevalidateForMerge_LyingVerdictAgainstReal300FileSensitivePR(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actorGitHubID = "revalidate-actor-c1-attack"
	const repoFullName = "acme/revalidate-c1-attack"
	const prNumber = 200
	const headSHA = "sha-attack"
	htmlURL := "https://github.com/" + repoFullName + "/pull/200"

	// Platform-authored (an artifacts row of type 'pr' at this URL) --
	// mirrors eligiblePR's own recipe.
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := narvipg.NewArtifactStore(pool).Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}

	// THE ATTACK: a verdict whose self-reported fields lie about the
	// diff's real shape, but whose OTHER fields legitimately compute
	// Shippable=auto -- exactly the reviewers' own verified-reproducible
	// payload shape.
	lyingVerdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      1,   // LIE: claims a trivial, one-file diff
		BlastRadius:       nil, // LIE: claims nothing sensitive touched
	}
	lyingVerdict.Shippable = review.ComputeShippable(lyingVerdict.RiskLevel, lyingVerdict.TestsCoverage, lyingVerdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if lyingVerdict.Shippable != review.ShippableAuto {
		t.Fatalf("test setup: lyingVerdict.Shippable = %v, want auto", lyingVerdict.Shippable)
	}
	// verdictContext (§21.1's amendment) matches the live PR's own
	// BaseRef/BaseSHA below exactly -- this test's whole point is
	// proving the diff-size/sensitive-path criteria refuse regardless of
	// the model's own lie, which requires the NEWER context-freshness
	// check to pass first so THOSE two criteria are what actually fire.
	verdictContext := reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion}
	if _, err := appreviewverdict.Insert(ctx, rs.deps.ReviewVerdict.ReviewVerdicts, rs.deps.ReviewVerdict.RepoSettings, false, repoFullName, prNumber, headSHA, pgtype.UUID{}, lyingVerdict, reviewpost.Digest{Summary: "Test-seeded lying verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed lying review_verdicts row: %v", err)
	}

	// THE TRUTH: GitHub's own Pull Request Files API reports 300 changed
	// files, including real migrations/ and internal/domain/authz/ paths --
	// this is what ports.OpenPR.ChangedFiles carries in production,
	// server-fetched, never something the reviewing agent's own POST body
	// can influence.
	changedFiles := make([]string, 0, 300)
	changedFiles = append(changedFiles, "migrations/000099_rewrite_everything.up.sql", "internal/domain/authz/rbac.go")
	for i := len(changedFiles); i < 300; i++ {
		changedFiles = append(changedFiles, "internal/app/widget/file"+strconv.Itoa(i)+".go")
	}

	pr := ports.OpenPR{
		Owner: "acme", Repo: "revalidate-c1-attack", Number: prNumber,
		Title: "innocuous-looking title", HTMLURL: htmlURL, HeadSHA: headSHA,
		BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
		Assignees:    []ports.PRPerson{{ExternalID: actorGitHubID, Login: "actor"}},
		CIConclusion: ports.CIConclusionSuccess,
		Labels:       []string{"review:low-risk"},
		ChangedFiles: changedFiles,
		// Phase 5 audit finding 2: the diff-size gate now reads
		// ChangedFilesCount (GitHub's own authoritative scalar), never
		// len(ChangedFiles) -- set explicitly here so this fixture's own
		// "both criteria independently refuse" claim below stays true
		// after the fix, exactly as it was before it. A real 300-file PR
		// would ACTUALLY truncate its own Pull Request Files fetch at
		// GitHub's own per_page=100 cap (ChangedFilesListDegraded=true in
		// production -- see TestListOpenPRsForUser_ChangedFilesCountAndDegraded,
		// internal/adapters/outbound/githubapi, for that adapter-level
		// behavior in isolation); this fixture deliberately keeps
		// ChangedFilesListDegraded at its own honest false, since THIS
		// test's own job is proving the size/sensitive-path criteria
		// themselves, not re-proving truncation detection a second time.
		ChangedFilesCount: len(changedFiles),
	}
	rs.sourceControl.openPRsByExternalID[actorGitHubID] = append(rs.sourceControl.openPRsByExternalID[actorGitHubID], pr)

	ok, headSHAGot, reason, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actorGitHubID, repoFullName, prNumber, "tok")
	if err != nil {
		t.Fatalf("RevalidateForMerge() error = %v, want nil", err)
	}
	if ok {
		t.Fatal("RevalidateForMerge() ok = true -- THE ATTACK SUCCEEDED: a lying verdict (filesChanged=1, blastRadius=[]) against a real 300-file diff touching migrations/ and authz/ was judged eligible for an unattended merge")
	}
	if headSHAGot != "" {
		t.Errorf("headSHA = %q, want empty on a refusal", headSHAGot)
	}
	// Both the diff-size AND sensitive-path criteria independently refuse
	// this PR -- ComputeEligible returns the FIRST criterion it hits
	// (diff size is checked before sensitive path, eligibility.go), so
	// the reason names diff size specifically; the sensitive-path half of
	// the fix is separately, directly pinned by
	// TestComputeEligible_IgnoresModelSelfReportedFilesChangedAndBlastRadius
	// (eligibility_test.go) in isolation (ChangedFileCount held small
	// there, so ONLY the sensitive-path criterion can fire).
	if reason == "" {
		t.Error("reason is empty, want a human-readable explanation")
	}
	t.Logf("attack correctly refused: %s", reason)
}

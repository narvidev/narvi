//go:build integration

// The decision inbox's live freshness read (§21.1b), on real Postgres: what
// the page shows when one of the live reads fails, for every read that can
// fail, and how many code-host reads one load of the inbox costs.
package decisioninbox_test

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	decisioninboxdomain "github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// countingCodeHost counts every read the decision inbox makes of the code
// host, by method -- the reads its SCMCache lets through, never its hits.
type countingCodeHost struct {
	*fakeDecisionInboxSourceControl

	mu    sync.Mutex
	reads map[string]int
}

func newCountingCodeHost(fake *fakeDecisionInboxSourceControl) *countingCodeHost {
	return &countingCodeHost{fakeDecisionInboxSourceControl: fake, reads: map[string]int{}}
}

func (h *countingCodeHost) count(method string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads[method]++
}

// take returns the reads counted since the last take, and starts again.
func (h *countingCodeHost) take() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	got := h.reads
	h.reads = map[string]int{}
	return got
}

func (h *countingCodeHost) ListOpenPRsForUser(ctx context.Context, spec ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	h.count("ListOpenPRsForUser")
	return h.fakeDecisionInboxSourceControl.ListOpenPRsForUser(ctx, spec)
}

func (h *countingCodeHost) ResolveCodeOwners(ctx context.Context, spec ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	h.count("ResolveCodeOwners")
	return h.fakeDecisionInboxSourceControl.ResolveCodeOwners(ctx, spec)
}

func (h *countingCodeHost) ListRequiredChecks(ctx context.Context, spec ports.ListRequiredChecksSpec) ([]ports.RequiredCheck, error) {
	h.count("ListRequiredChecks")
	return h.fakeDecisionInboxSourceControl.ListRequiredChecks(ctx, spec)
}

func (h *countingCodeHost) ResolveAppID(ctx context.Context, spec ports.ResolveAppIDSpec) (int64, error) {
	h.count("ResolveAppID")
	return h.fakeDecisionInboxSourceControl.ResolveAppID(ctx, spec)
}

func (h *countingCodeHost) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	h.count("ResolveBranchSHA")
	return h.fakeDecisionInboxSourceControl.ResolveBranchSHA(ctx, spec)
}

func (h *countingCodeHost) IsAncestor(ctx context.Context, spec ports.IsAncestorSpec) (bool, error) {
	h.count("IsAncestor")
	return h.fakeDecisionInboxSourceControl.IsAncestor(ctx, spec)
}

// reasonBaseCommitUnconfirmedText is what an accepted row's readout says
// when a live freshness read failed: the merge path's own refusal for a
// failed base read (revalidate.go), quoted so that a change to what a person
// reads is a change this test sees.
const reasonBaseCommitUnconfirmedText = "this pull request's base commit could not be confirmed (a live check failed) -- try again shortly"

// TestBuild_FreshnessReadFails_DegradedAndNeverEligible drives every live
// read the inbox's freshness check makes to a failure, one at a time, on an
// otherwise eligible pull request carrying an accepted verdict, so the
// page's every answer to it shows: the row is never ready to merge, the
// inbox says it is degraded, and the acceptance readout says the base could
// not be confirmed -- never that the pull request no longer qualifies. A
// failed read of the base's required checks beside a failed freshness read
// is the freshness read's, the first in the engine's order. The controls: a
// base tip read as empty with no error is an answer, not a failed read (the
// row is refused, undegraded), and a load where every read answers.
//
// Each case also reads the merge path, which must agree, and bounds the
// inbox's freshness reads by what it made before it read them through
// reviewfreshness.ReadLive (maxFreshnessReads): never more.
func TestBuild_FreshnessReadFails_DegradedAndNeverEligible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	errDown := errors.New("code host: http 502")

	const notEligibleEvenAccepted = "this pull request no longer meets the auto-approval eligibility criteria, even with its accepted override applied"

	tests := []struct {
		name string
		// stacked gives the pull request, and its verdict, an ancestor link
		// "parent", recorded at sha-parent. unreadableLink reports the pull
		// request's link with no ref at all (a degraded stack read).
		stacked        bool
		unreadableLink bool
		// baseTip and parentTip are the branches' live tips ("" leaves them
		// as recorded); emptyBaseTip and emptyParentTip read them as empty,
		// with no error.
		baseTip, parentTip           string
		emptyBaseTip, emptyParentTip bool
		// baseErr and parentErr fail one branch's read; ancestryErr fails
		// every ancestry check; requiredErr fails the base's required
		// checks.
		baseErr, parentErr, ancestryErr, requiredErr error

		wantReady           bool
		wantDegraded        bool
		wantAcceptanceShown bool
		wantReason          string
		// maxFreshnessReads is the ResolveBranchSHA and IsAncestor calls the
		// load made before the inbox read them through ReadLive.
		maxFreshnessReads int
	}{
		{
			name:                "the base branch's tip cannot be read",
			stacked:             true,
			baseErr:             errDown,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          reasonBaseCommitUnconfirmedText,
			maxFreshnessReads:   3,
		},
		{
			name:                "whether the base moved only forward cannot be read",
			stacked:             true,
			baseTip:             "sha-main-moved",
			ancestryErr:         errDown,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          reasonBaseCommitUnconfirmedText,
			maxFreshnessReads:   4,
		},
		{
			// The acceptance binds to the recorded link's ref, which the pull
			// request no longer reports, so no readout is shown at all.
			name:              "the ancestor link is reported with no ref",
			stacked:           true,
			unreadableLink:    true,
			wantDegraded:      true,
			maxFreshnessReads: 1,
		},
		{
			name:                "the ancestor link's tip cannot be read",
			stacked:             true,
			parentErr:           errDown,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          reasonBaseCommitUnconfirmedText,
			maxFreshnessReads:   3,
		},
		{
			name:                "the ancestor link's tip reads empty with no error",
			stacked:             true,
			emptyParentTip:      true,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          reasonBaseCommitUnconfirmedText,
			maxFreshnessReads:   2,
		},
		{
			name:                "whether the ancestor link moved only forward cannot be read",
			stacked:             true,
			parentTip:           "sha-parent-moved",
			ancestryErr:         errDown,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          reasonBaseCommitUnconfirmedText,
			maxFreshnessReads:   4,
		},
		{
			name:                "a failed base read beside a failed read of the required checks names the base",
			stacked:             true,
			baseErr:             errDown,
			requiredErr:         errDown,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          reasonBaseCommitUnconfirmedText,
			maxFreshnessReads:   3,
		},
		{
			name:                "a failed read of the required checks alone names them",
			requiredErr:         errDown,
			wantDegraded:        true,
			wantAcceptanceShown: true,
			wantReason:          string(autoapproval.ReasonRequiredChecksUnknown),
			maxFreshnessReads:   1,
		},
		{
			name:                "a base tip read as empty with no error refuses the row, undegraded",
			emptyBaseTip:        true,
			wantAcceptanceShown: true,
			wantReason:          notEligibleEvenAccepted,
			maxFreshnessReads:   1,
		},
		{
			name:                "every read answers, the base and the link moved only forward",
			stacked:             true,
			baseTip:             "sha-main-moved",
			parentTip:           "sha-parent-moved",
			wantReady:           true,
			wantAcceptanceShown: true,
			maxFreshnessReads:   4,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := 900 + i
			repoFullName := "acme/freshness-read-" + strconv.Itoa(n)
			actorGitHubID := "freshness-actor-" + strconv.Itoa(n)
			actor, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: actorGitHubID + "@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
			if err != nil {
				t.Fatalf("create actor: %v", err)
			}
			encrypted, err := platform.EncryptToken(tokenKey, []byte("person-token"))
			if err != nil {
				t.Fatalf("encrypt token: %v", err)
			}
			if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
				UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: actorGitHubID,
				EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encrypted,
			}); err != nil {
				t.Fatalf("create identity: %v", err)
			}

			rs := newRevalidateStores(pool)
			verdictContext := reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion}
			if tc.stacked {
				verdictContext.AncestorChain = []review.AncestorLink{{Ref: "parent", SHA: "sha-parent"}}
			}
			pr := seedPRWithVerdictContext(ctx, t, pool, rs, actorGitHubID, repoFullName, n, verdictContext)
			pr.CreatedAt = time.Now()
			if tc.stacked {
				pr.AncestorChain = []ports.PRAncestorLink{{Ref: "parent", SHA: "a-cached-sha-never-read"}}
			}
			if tc.unreadableLink {
				pr.AncestorChain = []ports.PRAncestorLink{{Ref: "", SHA: "a-cached-sha-never-read"}}
			}
			rs.replaceTargetPR(actorGitHubID, pr)

			record, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(n))
			if err != nil || !hasVerdict {
				t.Fatalf("GetLatest() = (%v, %v), want the seeded verdict", hasVerdict, err)
			}
			var verdictID pgtype.UUID
			if err := verdictID.Scan(record.ID); err != nil {
				t.Fatalf("scan verdict id: %v", err)
			}
			if _, _, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, appreviewverdict.AcceptInput{
				RepoFullName:  repoFullName,
				PRNumber:      int32(n),
				VerdictID:     verdictID,
				AttemptID:     seedReviewAttemptTurn(ctx, t, pool),
				HeadSHA:       record.HeadSHA,
				Context:       record.Context,
				Reason:        string(autoapproval.ReasonNotShippableAuto),
				Justification: "Accepted so the readout has an acceptance to describe.",
				AcceptedBy:    actor.ID,
			}); err != nil {
				t.Fatalf("Accept() error = %v", err)
			}

			fake := rs.sourceControl
			baseTip := testEligibleBaseSHA
			if tc.baseTip != "" {
				baseTip = tc.baseTip
			}
			if tc.emptyBaseTip {
				baseTip = ""
			}
			parentTip := "sha-parent"
			if tc.parentTip != "" {
				parentTip = tc.parentTip
			}
			if tc.emptyParentTip {
				parentTip = ""
			}
			fake.resolveBranchSHAByBranch = map[string]string{testEligibleBaseRef: baseTip, "parent": parentTip}
			fake.resolveBranchSHAErrByBranch = map[string]error{}
			if tc.baseErr != nil {
				fake.resolveBranchSHAErrByBranch[testEligibleBaseRef] = tc.baseErr
			}
			if tc.parentErr != nil {
				fake.resolveBranchSHAErrByBranch["parent"] = tc.parentErr
			}
			fake.isAncestorResult = true
			fake.isAncestorErr = tc.ancestryErr
			fake.requiredChecksErr = tc.requiredErr

			host := newCountingCodeHost(fake)
			deps := rs.deps
			deps.SCMCache = decisioninbox.NewSCMCache(host, platform.DefaultTimeouts())
			deps.TokenEncryptionKey = tokenKey

			result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMaintainer, time.Now())
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			reads := host.take()
			item := findItemByPR(result.Items, n)
			if item == nil {
				t.Fatalf("PR #%d missing from the inbox", n)
			}
			if ready := item.Kind == decisioninboxdomain.KindReadyToMerge; ready != tc.wantReady {
				t.Errorf("Kind = %s, want ready_to_merge %v", item.Kind, tc.wantReady)
			}
			if result.SCMFetchFailed != tc.wantDegraded {
				t.Errorf("SCMFetchFailed = %v, want %v", result.SCMFetchFailed, tc.wantDegraded)
			}
			if shown := item.AcceptanceID != ""; shown != tc.wantAcceptanceShown {
				t.Errorf("acceptance shown = %v, want %v", shown, tc.wantAcceptanceShown)
			}
			if item.AcceptanceMergeBlockedReason != tc.wantReason {
				t.Errorf("AcceptanceMergeBlockedReason = %q, want %q", item.AcceptanceMergeBlockedReason, tc.wantReason)
			}
			if item.AcceptanceMergeable != (tc.wantReady && tc.wantAcceptanceShown) {
				t.Errorf("AcceptanceMergeable = %v, want %v", item.AcceptanceMergeable, tc.wantReady && tc.wantAcceptanceShown)
			}
			if item.MergeableIfRequiredChecksPass {
				t.Error("MergeableIfRequiredChecksPass = true, want false: GitHub outbound is on")
			}
			freshnessReads := reads["ResolveBranchSHA"] + reads["IsAncestor"]
			t.Logf("freshness reads: %d (ResolveBranchSHA %d, IsAncestor %d)", freshnessReads, reads["ResolveBranchSHA"], reads["IsAncestor"])
			if freshnessReads > tc.maxFreshnessReads {
				t.Errorf("freshness reads = %d, want at most %d", freshnessReads, tc.maxFreshnessReads)
			}

			// The merge path reads live and agrees.
			ok, _, mergeReason, _, _, err := decisioninbox.RevalidateForMerge(ctx, deps, fake, actorGitHubID, repoFullName, n, "person-token")
			if err != nil {
				t.Fatalf("RevalidateForMerge() error = %v", err)
			}
			if ok != tc.wantReady {
				t.Errorf("RevalidateForMerge() ok = %v (%q), want %v, as the inbox", ok, mergeReason, tc.wantReady)
			}
		})
	}
}

// TestBuild_FreshnessReadCost reads the inbox of one person assigned N pull
// requests on M base branches, each stacked on a link of its base's, the
// base and the link each moved only forward since the verdicts -- so every
// row reaches the live freshness read and all four of its calls run. The
// reads one load costs are counted by method: the first load reads each
// base, each link and each forward move once, whatever N, and a load within
// DecisionInboxSCMCacheTTL reads nothing. (A load dated past the TTL is not
// counted: the cache stamps an entry with the wall clock when it fetches
// it, so such a load finds every entry it has just fetched already expired
// and reads once per row -- a property of the test's clock, not of a load.)
func TestBuild_FreshnessReadCost(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")
	const actorGitHubID = "freshness-cost-actor"
	const repoFullName = "acme/freshness-read-cost"
	bases := []string{"main", "release"}
	const perBase = 3
	n, m := perBase*len(bases), len(bases)

	actor := decisionInboxActorFixture(ctx, t, pool, "freshness-cost@example.com", actorGitHubID, tokenKey)
	rs := newRevalidateStores(pool)
	live := map[string]string{}
	var prs []ports.OpenPR
	number := 950
	for _, base := range bases {
		link := "stack-" + base
		live[base] = "new-" + base
		live[link] = "new-" + link
		for range perBase {
			number++
			pr := seedPRWithVerdictContext(ctx, t, pool, rs, actorGitHubID, repoFullName, number, reviewverdict.Context{
				BaseRef:       base,
				BaseSHA:       "old-" + base,
				AncestorChain: []review.AncestorLink{{Ref: link, SHA: "old-" + link}},
				PolicyVersion: autoapproval.CurrentPolicyVersion,
			})
			pr.BaseRef = base
			pr.BaseSHA = "a-cached-sha-never-read"
			pr.AncestorChain = []ports.PRAncestorLink{{Ref: link, SHA: "a-cached-sha-never-read"}}
			pr.CreatedAt = time.Now()
			prs = append(prs, pr)
		}
	}
	rs.sourceControl.openPRsByExternalID[actorGitHubID] = prs
	rs.sourceControl.resolveBranchSHAByBranch = live
	rs.sourceControl.isAncestorResult = true

	host := newCountingCodeHost(rs.sourceControl)
	deps := rs.deps
	deps.SCMCache = decisioninbox.NewSCMCache(host, platform.DefaultTimeouts())
	deps.TokenEncryptionKey = tokenKey

	firstLoad := map[string]int{
		"ListOpenPRsForUser": 1,
		"ResolveCodeOwners":  m,
		"ListRequiredChecks": m,
		"ResolveBranchSHA":   2 * m, // each base, and each link
		"IsAncestor":         2 * m, // each base's forward move, and each link's
	}
	ttl := platform.DefaultTimeouts().DecisionInboxSCMCacheTTL
	t0 := time.Now()
	loads := []struct {
		name string
		at   time.Time
		want map[string]int
	}{
		{"the first load", t0, firstLoad},
		{"a load within the cache's TTL", t0.Add(ttl / 2), map[string]int{}},
	}
	for _, load := range loads {
		result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, load.at)
		if err != nil {
			t.Fatalf("%s: Build() error = %v", load.name, err)
		}
		got := host.take()
		t.Logf("%s, %d pull requests on %d base branches: %v", load.name, n, m, got)
		if !maps.Equal(got, load.want) {
			t.Errorf("%s: code-host reads = %v, want %v", load.name, got, load.want)
		}
		if result.SCMFetchFailed {
			t.Errorf("%s: SCMFetchFailed = true, want false", load.name)
		}
		ready := 0
		for _, item := range result.Items {
			if item.Kind == decisioninboxdomain.KindReadyToMerge {
				ready++
			}
		}
		if ready != n {
			t.Errorf("%s: %d rows ready to merge, want all %d -- every row must reach the live read for the count to mean anything", load.name, ready, n)
		}
	}
}

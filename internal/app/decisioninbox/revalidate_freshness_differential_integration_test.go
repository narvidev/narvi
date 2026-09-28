//go:build integration

// A differential proof that a session's result (reviewfreshness.Assess,
// row 182, technical plan §43.20) and the merge path (RevalidateForMerge,
// through revalidateCore's probe) answer a verdict the record decides with
// the same reason, on the same pull request, against real Postgres and the
// same fake code host.
package decisioninbox_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// singlePRCodeHost answers GetOpenPR -- the result's pull request read --
// from the one pull request the decision inbox fake holds for actor (the
// merge path's own ListOpenPRsForUser answer), and counts every live call
// the result makes.
type singlePRCodeHost struct {
	*fakeDecisionInboxSourceControl
	actor string

	mu    sync.Mutex
	calls []string
}

func (h *singlePRCodeHost) note(call string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call)
}

func (h *singlePRCodeHost) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	h.note("open")
	for _, pr := range h.openPRsByExternalID[h.actor] {
		if pr.Owner == owner && pr.Repo == repo && pr.Number == number {
			return pr, true, nil
		}
	}
	return ports.OpenPR{}, false, nil
}

func (h *singlePRCodeHost) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	h.note("resolve " + spec.Branch)
	return h.fakeDecisionInboxSourceControl.ResolveBranchSHA(ctx, spec)
}

func (h *singlePRCodeHost) IsAncestor(ctx context.Context, spec ports.IsAncestorSpec) (bool, error) {
	h.note("ancestor")
	return h.fakeDecisionInboxSourceControl.IsAncestor(ctx, spec)
}

// TestAssess_AgreesWithTheMergePathOnRecordDecidedVerdicts is finding P8's
// differential test. Every verdict here carries a defect the record alone
// decides -- no context, an unknown base commit, an unknown ancestor link,
// an older policy -- on an otherwise fully eligible pull request whose live
// head and base ref are as recorded, or moved. For each, the merge path
// refuses at its probe, and the result must give the same reason, from the
// pull request read alone: a moved head is stale on both (the probe
// compares the live head first), never unconfirmed on one of them.
func TestAssess_AgreesWithTheMergePathOnRecordDecidedVerdicts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	rs := newRevalidateStores(pool)
	const actor = "differential-actor"

	defects := []struct {
		name    string
		context reviewverdict.Context
	}{
		{"no context recorded", reviewverdict.Context{}},
		{"base commit unknown", reviewverdict.Context{BaseRef: testEligibleBaseRef, PolicyVersion: autoapproval.CurrentPolicyVersion}},
		{"ancestor link unknown", reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion,
			AncestorChain: []review.AncestorLink{{Ref: "parent", SHA: ""}}}},
		{"older policy", reviewverdict.Context{BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, PolicyVersion: autoapproval.CurrentPolicyVersion - 1}},
	}
	lives := []struct {
		name      string
		headMoved bool
		retarget  bool
	}{
		{"head and base ref as recorded", false, false},
		{"head moved", true, false},
		{"base retargeted", false, true},
		{"head moved and base retargeted", true, true},
	}

	number := 200
	for _, d := range defects {
		for _, l := range lives {
			number++
			n := number
			t.Run(d.name+", "+l.name, func(t *testing.T) {
				repoFullName := fmt.Sprintf("acme/differential-%d", n)
				pr := seedPRWithVerdictContext(ctx, t, pool, rs, actor, repoFullName, n, d.context)
				if l.headMoved {
					pr.HeadSHA = "moved-" + pr.HeadSHA
				}
				if l.retarget {
					pr.BaseRef = "release"
				}
				rs.replaceTargetPR(actor, pr)

				ok, _, mergeReason, _, _, err := decisioninbox.RevalidateForMerge(ctx, rs.deps, rs.sourceControl, actor, repoFullName, n, "tok")
				if err != nil || ok {
					t.Fatalf("merge path: ok %v err %v (%q), want a refusal", ok, err, mergeReason)
				}

				record, found, err := appreviewverdict.GetLatestRecord(ctx, rs.deps.ReviewVerdict, repoFullName, int32(n))
				if err != nil || !found {
					t.Fatalf("read the verdict as the result does: found %v err %v", found, err)
				}
				host := &singlePRCodeHost{fakeDecisionInboxSourceControl: rs.sourceControl, actor: actor}
				got := reviewfreshness.Assess(ctx, reviewfreshness.Deps{SourceControl: host, Token: "tok", Timeouts: platform.DefaultTimeouts()},
					record, reviewfreshness.PullRequest{Owner: pr.Owner, Repo: pr.Repo, Number: n})

				if got.Reason == "" || !strings.HasSuffix(mergeReason, ": "+got.Reason) {
					t.Fatalf("result says %s (%q); the merge path says %q -- want the same reason", got.State, got.Reason, mergeReason)
				}
				if want := autoapproval.ClassifyFreshness(autoapproval.Reason(got.Reason)); string(got.State) != string(want) {
					t.Fatalf("result state %s for %q, want %s", got.State, got.Reason, want)
				}
				if l.headMoved && got.Reason != string(autoapproval.ReasonStaleVerdict) {
					t.Fatalf("head moved: result reason %q, want the stale-verdict reason, as the merge path gives", got.Reason)
				}
				if len(host.calls) != 1 || host.calls[0] != "open" {
					t.Fatalf("result's live calls = %v, want the pull request read alone: the record decides", host.calls)
				}
			})
		}
	}
}

// seedPRWithVerdictContext is eligiblePR (revalidate_integration_test.go)
// with a verdict recorded under verdictContext instead of the eligible one:
// the fully eligible pull request, platform-authored, and its auto verdict
// at its head.
func seedPRWithVerdictContext(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rs *revalidateStores, actorGitHubID, repoFullName string, prNumber int, verdictContext reviewverdict.Context) ports.OpenPR {
	t.Helper()
	owner, repo, _ := strings.Cut(repoFullName, "/")
	htmlURL := fmt.Sprintf("https://github.com/%s/pull/%d", repoFullName, prNumber)
	pr := ports.OpenPR{
		Owner: owner, Repo: repo, Number: prNumber,
		Title: "differential pr", HTMLURL: htmlURL, HeadSHA: fmt.Sprintf("sha-%d", prNumber),
		BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
		Assignees:    []ports.PRPerson{{ExternalID: actorGitHubID, Login: "actor"}},
		CIConclusion: ports.CIConclusionSuccess,
		Labels:       []string{"review:low-risk"},
	}
	rs.replaceTargetPR(actorGitHubID, pr)

	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := narvipg.NewArtifactStore(pool).Create(ctx, sqlcgen.CreateArtifactParams{SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}")}); err != nil {
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
	if _, err := appreviewverdict.Insert(ctx, narvipg.NewReviewVerdictStore(pool), narvipg.NewRepoSettingsStore(pool), false, repoFullName, int32(prNumber), pr.HeadSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false, verdictContext, pgtype.UUID{}); err != nil {
		t.Fatalf("seed the verdict for %s#%d: %v", repoFullName, prNumber, err)
	}
	return pr
}

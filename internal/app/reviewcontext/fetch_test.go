package reviewcontext_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeFetcher is a test-only reviewcontext.Fetcher -- no real HTTP round
// trip, exactly the point of that interface being narrow and locally
// defined (fetch.go's own doc comment).
//
// GetCompareDiff replaces the
// PREVIOUS GetPullRequestDiff here, and callOrder (below) is this file's
// own NEW assertion surface -- proving GetPullRequest always runs BEFORE
// GetCompareDiff, and that the diff fetch is PINNED to exactly what
// GetPullRequest reported (never an independently-supplied value), the
// two properties the whole fix depends on.
type fakeFetcher struct {
	pr       githubapi.PullRequest
	prErr    error
	prCalls  int
	prOwner  string
	prRepo   string
	prNumber int32
	prToken  string

	diff          string
	diffTruncated bool
	diffErr       error
	diffCalls     int
	diffOwner     string
	diffRepo      string
	diffBase      string
	diffHead      string
	diffToken     string

	// resolveBranchSHA/resolveBranchSHAResolved/resolveBranchSHAErr
	// (finding F1) back ResolveBranchSHA below -- the default (all zero
	// values) mirrors this fake's own pre-existing "unset means a plain,
	// harmless zero result" convention (see prErr/diffErr's own defaults):
	// an unset resolveBranchSHA reports ("", "", nil), which Fetch treats
	// identically to "resolution unavailable" (baseSHA stays "", the diff
	// fetch falls back to pinning on the base REF), so every EXISTING test
	// in this file that never configures this field keeps its own
	// pre-fix behavior for every OTHER assertion it makes.
	resolveBranchSHA         string
	resolveBranchSHAResolved string
	resolveBranchSHAErr      error
	resolveBranchSHACalls    int
	resolveBranchSHAOwner    string
	resolveBranchSHARepo     string
	resolveBranchSHABranch   string
	resolveBranchSHAToken    string

	// callOrder records each method invoked, in order ("pr",
	// "resolvebranchsha", "diff") -- asserted directly by
	// TestFetch_Success_CallOrderIsPRThenResolveBranchSHAThenDiff to pin
	// that GetPullRequest always resolves BEFORE the base-branch
	// resolution, which itself always resolves BEFORE the diff fetch is
	// even attempted (never the reverse, and never interleaved/
	// concurrent) -- the ordering the whole F1 fix depends on: the diff
	// fetch must be pinned to the SAME resolved base sha this call
	// reports, never a value obtained after it.
	callOrder []string
}

func (f *fakeFetcher) GetPullRequest(_ context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error) {
	f.prCalls++
	f.prOwner, f.prRepo, f.prNumber, f.prToken = owner, repo, number, token
	f.callOrder = append(f.callOrder, "pr")
	return f.pr, f.prErr
}

func (f *fakeFetcher) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.resolveBranchSHACalls++
	f.resolveBranchSHAOwner, f.resolveBranchSHARepo, f.resolveBranchSHABranch, f.resolveBranchSHAToken = spec.Owner, spec.Repo, spec.Branch, spec.Token
	f.callOrder = append(f.callOrder, "resolvebranchsha")
	return f.resolveBranchSHA, f.resolveBranchSHAResolved, f.resolveBranchSHAErr
}

func (f *fakeFetcher) GetCompareDiff(_ context.Context, owner, repo, base, head, token string) (string, bool, error) {
	f.diffCalls++
	f.diffOwner, f.diffRepo, f.diffBase, f.diffHead, f.diffToken = owner, repo, base, head, token
	f.callOrder = append(f.callOrder, "diff")
	return f.diff, f.diffTruncated, f.diffErr
}

func assertPRArgs(t *testing.T, f *fakeFetcher, wantOwner, wantRepo string, wantNumber int32, wantToken string) {
	t.Helper()
	if f.prOwner != wantOwner || f.prRepo != wantRepo || f.prNumber != wantNumber || f.prToken != wantToken {
		t.Errorf("GetPullRequest args = (%q, %q, %d, %q), want (%q, %q, %d, %q)",
			f.prOwner, f.prRepo, f.prNumber, f.prToken, wantOwner, wantRepo, wantNumber, wantToken)
	}
}

// assertDiffArgs checks the EXACT (owner, repo, base, head, token)
// GetCompareDiff was invoked with -- wantBase/wantHead are deliberately
// checked here (never hardcoded by a caller) so a regression that pins
// the diff fetch to the WRONG commit (e.g. a stale knownHeadSHA, or the
// wrong PR's own base) fails this assertion specifically.
func assertDiffArgs(t *testing.T, f *fakeFetcher, wantOwner, wantRepo, wantBase, wantHead, wantToken string) {
	t.Helper()
	if f.diffOwner != wantOwner || f.diffRepo != wantRepo || f.diffBase != wantBase || f.diffHead != wantHead || f.diffToken != wantToken {
		t.Errorf("GetCompareDiff args = (%q, %q, base=%q, head=%q, %q), want (%q, %q, base=%q, head=%q, %q)",
			f.diffOwner, f.diffRepo, f.diffBase, f.diffHead, f.diffToken, wantOwner, wantRepo, wantBase, wantHead, wantToken)
	}
}

// TestFetch_Success_DiffPinnedToExactlyWhatWasResolved is the C2/F1
// regression test at the unit level: the core atomicity property this
// whole fix exists to provide -- the diff fetch (GetCompareDiff) is
// parametrized by EXACTLY pr.HeadSHA and the LIVE-resolved base sha
// (finding F1: never pr.BaseRef alone, and never GetPullRequest's own
// possibly-stale base.sha field), the SAME values this call returns as
// HeadSHA/BaseSHA, never a second, independently-suppliable value that
// could disagree.
func TestFetch_Success_DiffPinnedToExactlyWhatWasResolved(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr:               githubapi.PullRequest{HeadRef: "feature-x", HeadSHA: "resolved-head-sha", BaseRef: "main", Title: "Fix the retry loop", Body: "Retries now back off exponentially."},
		diff:             "diff --git a/x b/x\n",
		resolveBranchSHA: "resolved-base-sha",
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.Diff != "diff --git a/x b/x\n" {
		t.Errorf("Diff = %q, want the fetched diff", got.Diff)
	}
	if got.DiffTruncated {
		t.Error("DiffTruncated = true, want false")
	}
	if got.Stack != nil {
		t.Errorf("Stack = %+v, want nil (PR reported no stack)", got.Stack)
	}
	if got.HeadSHA != "resolved-head-sha" {
		t.Errorf("HeadSHA = %q, want %q", got.HeadSHA, "resolved-head-sha")
	}
	// THE decisive F1 assertion: BaseSHA comes from the LIVE
	// ResolveBranchSHA call, never from GitHub's own possibly-stale
	// `base.sha` -- githubapi.PullRequest carries no such field at all
	// (D11), so there is no fixture value a regression could fall back to
	// reading; this assertion would simply fail against a real, non-empty
	// resolveBranchSHA fixture value if it ever stopped being used.
	if got.BaseSHA != "resolved-base-sha" {
		t.Errorf("BaseSHA = %q, want %q (the LIVE-resolved value)", got.BaseSHA, "resolved-base-sha")
	}
	if got.Title != "Fix the retry loop" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix the retry loop")
	}
	if got.Body != "Retries now back off exponentially." {
		t.Errorf("Body = %q, want %q", got.Body, "Retries now back off exponentially.")
	}
	if fetcher.prCalls != 1 {
		t.Errorf("prCalls = %d, want 1", fetcher.prCalls)
	}
	if fetcher.resolveBranchSHACalls != 1 {
		t.Errorf("resolveBranchSHACalls = %d, want 1", fetcher.resolveBranchSHACalls)
	}
	if fetcher.diffCalls != 1 {
		t.Errorf("diffCalls = %d, want 1", fetcher.diffCalls)
	}
	assertPRArgs(t, fetcher, "acme", "widgets", 42, "gho_bottoken")
	if fetcher.resolveBranchSHAOwner != "acme" || fetcher.resolveBranchSHARepo != "widgets" || fetcher.resolveBranchSHABranch != "main" || fetcher.resolveBranchSHAToken != "gho_bottoken" {
		t.Errorf("ResolveBranchSHA args = (%q, %q, branch=%q, %q), want (\"acme\", \"widgets\", branch=\"main\", \"gho_bottoken\")",
			fetcher.resolveBranchSHAOwner, fetcher.resolveBranchSHARepo, fetcher.resolveBranchSHABranch, fetcher.resolveBranchSHAToken)
	}
	// THE core atomicity assertion: GetCompareDiff's own base arg is
	// EXACTLY the resolved base sha, never pr.BaseRef (the branch name
	// GitHub would otherwise re-resolve itself, one more independently-
	// raceable read) -- githubapi.PullRequest has no BaseSHA field to
	// confuse this with (D11).
	assertDiffArgs(t, fetcher, "acme", "widgets", "resolved-base-sha", "resolved-head-sha", "gho_bottoken")
}

// TestFetch_BaseResolutionFails_DiffFallsBackToBaseRef_BaseSHAEmpty proves
// finding F1's own degradation path: a ResolveBranchSHA failure never
// fails the whole review turn's own creation (mirroring every other
// degrade-gracefully precedent in this function) -- BaseSHA stays "" (an
// honest "could not be established", never a stale or guessed value), and
// the diff fetch falls back to pinning on the base REF name, exactly the
// pre-fix behavior for that ONE call, so a transient GitHub failure here
// costs nothing beyond an empty BaseSHA (which autoapproval.ComputeEligible's
// own empty-base-sha guard, finding F2, then fails closed on -- never
// silently reads as a match).
func TestFetch_BaseResolutionFails_DiffFallsBackToBaseRef_BaseSHAEmpty(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr:                  githubapi.PullRequest{HeadSHA: "resolved-head-sha", BaseRef: "main"},
		diff:                "d",
		resolveBranchSHAErr: errors.New("network exploded"),
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want empty on a ResolveBranchSHA failure", got.BaseSHA)
	}
	if got.HeadSHA != "resolved-head-sha" {
		t.Errorf("HeadSHA = %q, want %q -- a base-resolution failure must not erase an already-confirmed head sha", got.HeadSHA, "resolved-head-sha")
	}
	if got.Diff != "d" {
		t.Errorf("Diff = %q, want %q -- a base-resolution failure must not prevent the diff fetch, only its pinning precision", got.Diff, "d")
	}
	assertDiffArgs(t, fetcher, "acme", "widgets", "main", "resolved-head-sha", "gho_bottoken")
}

// TestFetch_Success_CallOrderIsPRThenResolveBranchSHAThenDiff pins that
// GetPullRequest always resolves BEFORE ResolveBranchSHA, which itself
// always resolves BEFORE GetCompareDiff is even attempted -- the ordering
// the whole atomicity fix depends on (fetch.go's own doc comment: "resolve
// pr.HeadSHA ... FIRST, then fetch the diff ... PINNED to that exact
// pair", and finding F1's amendment: the base sha must be resolved before
// the diff fetch it pins).
func TestFetch_Success_CallOrderIsPRThenResolveBranchSHAThenDiff(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{pr: githubapi.PullRequest{HeadSHA: "sha", BaseRef: "main"}, diff: "d"}
	reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	want := []string{"pr", "resolvebranchsha", "diff"}
	if len(fetcher.callOrder) != len(want) {
		t.Fatalf("callOrder = %v, want %v", fetcher.callOrder, want)
	}
	for i := range want {
		if fetcher.callOrder[i] != want[i] {
			t.Errorf("callOrder = %v, want %v", fetcher.callOrder, want)
			break
		}
	}
}

// TestFetch_GetPullRequestAlwaysCalled_EvenWithKnownStack is the
// deliberate-tradeoff regression test named in fetch.go's own doc
// comment: UNLIKE the previous version of this function, a caller-
// supplied knownStack no longer skips the GetPullRequest call --
// correctness (a provably-pinned diff) now requires it unconditionally,
// since pr.BaseRef was never available from that old shortcut anyway.
func TestFetch_GetPullRequestAlwaysCalled_EvenWithKnownStack(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{pr: githubapi.PullRequest{HeadSHA: "sha", BaseRef: "main"}, diff: "d"}
	known := &review.StackContext{Position: 1, Size: 2, UltimateBaseRef: "main", UltimateBaseSHA: "abc123"}

	reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", known)

	if fetcher.prCalls != 1 {
		t.Errorf("prCalls = %d, want 1 -- GetPullRequest must run even when knownStack is already supplied (this function's own doc comment: correctness now requires this call unconditionally)", fetcher.prCalls)
	}
}

// TestFetch_KnownStackPreferredOverPRStack proves knownStack, when
// supplied, is used AS-IS (exact pointer) rather than re-derived from
// this call's own pr.Stack -- even when pr.Stack reports a DIFFERENT
// stack, proving preference, not a coincidental match.
func TestFetch_KnownStackPreferredOverPRStack(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr: githubapi.PullRequest{
			HeadSHA: "sha", BaseRef: "main",
			Stack: &githubapi.StackInfo{Position: 9, Size: 9, BaseRef: "other", BaseSHA: "other-sha"},
		},
		diff: "d",
	}
	known := &review.StackContext{Position: 1, Size: 2, UltimateBaseRef: "main", UltimateBaseSHA: "abc123"}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", known)

	if got.Stack != known {
		t.Errorf("Stack = %+v, want the exact knownStack pointer %+v (never overwritten by pr.Stack)", got.Stack, known)
	}
}

// TestFetch_NoKnownStack_DerivesFromPRStack proves a fresh GetPullRequest
// call reporting a real stack is converted into review.StackContext
// correctly when no knownStack was supplied.
func TestFetch_NoKnownStack_DerivesFromPRStack(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr: githubapi.PullRequest{
			HeadSHA: "sha", BaseRef: "main",
			Stack: &githubapi.StackInfo{Position: 2, Size: 3, BaseRef: "main", BaseSHA: "deadbeef"},
		},
		diff: "d",
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.Stack == nil {
		t.Fatal("Stack = nil, want non-nil when GetPullRequest reports one")
	}
	want := review.StackContext{Position: 2, Size: 3, UltimateBaseRef: "main", UltimateBaseSHA: "deadbeef"}
	if *got.Stack != want {
		t.Errorf("Stack = %+v, want %+v", *got.Stack, want)
	}
}

// TestFetch_NoKnownStack_NoPRStack_StaysNil proves the ordinary,
// non-stacked-PR case: neither knownStack nor pr.Stack present, Stack
// stays nil.
func TestFetch_NoKnownStack_NoPRStack_StaysNil(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{pr: githubapi.PullRequest{HeadSHA: "sha", BaseRef: "main"}, diff: "d"}
	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.Stack != nil {
		t.Errorf("Stack = %+v, want nil", got.Stack)
	}
}

// TestFetch_GetPullRequestFails_DiffNeverAttempted_OnlyKnownStackSurvives
// proves a GetPullRequest failure degrades gracefully -- Diff/HeadSHA
// both stay empty (nothing safe to pin a diff fetch to, nothing safe to
// persist as review_verdicts.head_sha), GetCompareDiff is NEVER even
// attempted (there is no head sha to pin it to), and knownStack -- when
// the caller already had it, at zero cost -- still survives.
func TestFetch_GetPullRequestFails_DiffNeverAttempted_OnlyKnownStackSurvives(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{prErr: errors.New("network exploded")}
	known := &review.StackContext{Position: 1, Size: 2, UltimateBaseRef: "main", UltimateBaseSHA: "abc123"}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", known)

	if got.Diff != "" {
		t.Errorf("Diff = %q, want empty on a GetPullRequest failure", got.Diff)
	}
	if got.DiffTruncated {
		t.Error("DiffTruncated = true, want false")
	}
	if got.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty on a GetPullRequest failure", got.HeadSHA)
	}
	if got.Stack != known {
		t.Errorf("Stack = %+v, want the exact knownStack pointer %+v (a caller-supplied value costs nothing to keep)", got.Stack, known)
	}
	if fetcher.diffCalls != 0 {
		t.Errorf("diffCalls = %d, want 0 -- GetCompareDiff must never be attempted with no confirmed head sha to pin it to", fetcher.diffCalls)
	}
}

// TestFetch_DiffFetchFails_HeadSHAStillReported proves the diff fetch
// failing independently of the (already-succeeded) GetPullRequest call:
// Diff/DiffTruncated degrade to their own zero value, but HeadSHA (and
// Stack) are UNAFFECTED -- pr.HeadSHA is still an honest fact about the
// PR's real head even when the diff transfer itself failed (fetch.go's
// own doc comment: "HeadSHA is reported here regardless of whether the
// diff fetch above itself succeeded").
func TestFetch_DiffFetchFails_HeadSHAStillReported(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr:      githubapi.PullRequest{HeadSHA: "resolved-head-sha", BaseRef: "main"},
		diffErr: errors.New("network exploded"),
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.Diff != "" {
		t.Errorf("Diff = %q, want empty on a diff-fetch failure", got.Diff)
	}
	if got.DiffTruncated {
		t.Error("DiffTruncated = true, want false")
	}
	if got.HeadSHA != "resolved-head-sha" {
		t.Errorf("HeadSHA = %q, want %q -- a diff-fetch failure must not erase an already-confirmed head sha", got.HeadSHA, "resolved-head-sha")
	}
}

// TestFetch_TitleBodyThreadedThrough_EvenWhenDiffFetchFails is the
// adversarial-review fix's own regression test (§26.2's own
// follow-up, review.PreFetchedContext.Title's own doc comment): Title/Body
// come from the SAME already-succeeded GetPullRequest call HeadSHA itself
// is resolved from, so a LATER diff-fetch failure must not erase them --
// mirrors TestFetch_DiffFetchFails_HeadSHAStillReported's own identical
// "HeadSHA/Title/Body are independent of the diff fetch's own outcome"
// property.
func TestFetch_TitleBodyThreadedThrough_EvenWhenDiffFetchFails(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr:      githubapi.PullRequest{HeadSHA: "resolved-head-sha", BaseRef: "main", Title: "Fix the retry loop", Body: "Retries now back off exponentially."},
		diffErr: errors.New("network exploded"),
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.Title != "Fix the retry loop" {
		t.Errorf("Title = %q, want %q -- a diff-fetch failure must not erase an already-fetched title", got.Title, "Fix the retry loop")
	}
	if got.Body != "Retries now back off exponentially." {
		t.Errorf("Body = %q, want %q -- a diff-fetch failure must not erase an already-fetched body", got.Body, "Retries now back off exponentially.")
	}
}

// TestFetch_GetPullRequestFails_TitleBodyStayEmpty proves the SAME
// graceful-degradation precedent Diff/HeadSHA already establish
// (TestFetch_GetPullRequestFails_DiffNeverAttempted_OnlyKnownStackSurvives)
// extends to Title/Body: a GetPullRequest failure leaves them at their own
// honest empty zero value, never a stale or fabricated value.
func TestFetch_GetPullRequestFails_TitleBodyStayEmpty(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{prErr: errors.New("network exploded")}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.Title != "" {
		t.Errorf("Title = %q, want empty on a GetPullRequest failure", got.Title)
	}
	if got.Body != "" {
		t.Errorf("Body = %q, want empty on a GetPullRequest failure", got.Body)
	}
}

// TestFetch_DiffTruncated proves DiffTruncated is carried through verbatim
// on a successful-but-capped fetch.
func TestFetch_DiffTruncated(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr:            githubapi.PullRequest{HeadSHA: "sha", BaseRef: "main"},
		diff:          "partial diff...",
		diffTruncated: true,
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if !got.DiffTruncated {
		t.Error("DiffTruncated = false, want true")
	}
	if got.Diff != "partial diff..." {
		t.Errorf("Diff = %q, want %q", got.Diff, "partial diff...")
	}
}

// TestFetch_DiffFetchFails_TruncatedIgnored proves diffTruncated is
// ignored (never surfaced) once diffErr fired -- mirrors this function's
// own pre-existing "an error return's OTHER values carry no signal"
// discipline.
func TestFetch_DiffFetchFails_TruncatedIgnored(t *testing.T) {
	t.Parallel()

	fetcher := &fakeFetcher{
		pr:            githubapi.PullRequest{HeadSHA: "sha", BaseRef: "main"},
		diffErr:       errors.New("network exploded"),
		diffTruncated: true, // must be ignored on error
	}

	got := reviewcontext.Fetch(context.Background(), discardLogger(), fetcher, platform.DefaultTimeouts(), "acme", "widgets", 42, "gho_bottoken", nil)

	if got.DiffTruncated {
		t.Error("DiffTruncated = true, want false (the diffTruncated=true fake field must be ignored once diffErr fired)")
	}
}

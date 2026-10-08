package github

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/domain/review"
)

// discardLogger is a *slog.Logger that writes nowhere -- every test below
// only cares about resolveIssueCommentHead's own RETURN value, never its
// log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePullRequestResolver is a test-only PullRequestResolver -- no real
// HTTP round trip, exactly the point of headresolve.go's own
// PullRequestResolver interface being narrow and locally defined.
type fakePullRequestResolver struct {
	pr        githubapi.PullRequest
	err       error
	gotOwner  string
	gotRepo   string
	gotNumber int32
	gotToken  string
	calls     int
}

func (f *fakePullRequestResolver) GetPullRequest(_ context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error) {
	f.calls++
	f.gotOwner = owner
	f.gotRepo = repo
	f.gotNumber = number
	f.gotToken = token
	return f.pr, f.err
}

func baseIssueCommentMention() mention {
	headBranch := (*string)(nil)
	return mention{
		RepoFullName: "acme/widgets",
		RepoName:     "widgets",
		RepoCloneURL: "https://github.com/acme/widgets.git",
		PRNumber:     42,
		HeadBranch:   headBranch,
		CommentBody:  "@narvi-bot please review",
	}
}

// TestResolveIssueCommentHead_Success proves a successful GetPullRequest
// call resolves the mention's REAL head branch AND head repository -- the
// H5 audit fix's own headline case: a mention on the Conversation tab of a
// PR whose head branch differs from the base repo's default branch
// resolves to that REAL head branch, not nil/default. The head repository
// here is a fork: it is recorded as the head's, and the repository the
// session clones stays the base (technical plan §30.4).
func TestResolveIssueCommentHead_Success(t *testing.T) {
	resolver := &fakePullRequestResolver{
		pr: githubapi.PullRequest{
			HeadRef:          "feature-x",
			HeadSHA:          "c0ffee0000000000000000000000000000000001",
			HeadRepoFullName: "contributor/widgets",
		},
	}

	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", baseIssueCommentMention())

	if resolver.calls != 1 {
		t.Fatalf("GetPullRequest call count = %d, want 1", resolver.calls)
	}
	if resolver.gotOwner != "acme" || resolver.gotRepo != "widgets" {
		t.Errorf("GetPullRequest(owner, repo) = (%q, %q), want (%q, %q)", resolver.gotOwner, resolver.gotRepo, "acme", "widgets")
	}
	if resolver.gotNumber != 42 {
		t.Errorf("GetPullRequest number = %d, want 42", resolver.gotNumber)
	}
	if resolver.gotToken != "gho_bottoken" {
		t.Errorf("GetPullRequest token = %q, want %q", resolver.gotToken, "gho_bottoken")
	}

	if got.HeadBranch == nil || *got.HeadBranch != "feature-x" {
		t.Errorf("HeadBranch = %v, want %q (the REAL head branch, not nil/default)", got.HeadBranch, "feature-x")
	}
	if got.HeadRepoFullName != "contributor/widgets" {
		t.Errorf("HeadRepoFullName = %q, want the PR's real head repository %q", got.HeadRepoFullName, "contributor/widgets")
	}
	if got.HeadSHA == nil || *got.HeadSHA != "c0ffee0000000000000000000000000000000001" {
		t.Errorf("HeadSHA = %v, want the PR's head commit", got.HeadSHA)
	}
	if got.RepoName != "widgets" || got.RepoCloneURL != "https://github.com/acme/widgets.git" {
		t.Errorf("RepoName/RepoCloneURL = %q/%q, want the base repository %q/%q, never the fork",
			got.RepoName, got.RepoCloneURL, "widgets", "https://github.com/acme/widgets.git")
	}
}

// TestResolveIssueCommentHead_APIFailureFallsBack proves a failed
// GetPullRequest call (network error, non-2xx, timeout) falls back to
// today's PRE-fix behavior -- the mention unchanged (HeadBranch nil, base
// repo) -- rather than dropping the mention or failing the whole webhook
// delivery.
func TestResolveIssueCommentHead_APIFailureFallsBack(t *testing.T) {
	resolver := &fakePullRequestResolver{err: errors.New("boom: connection refused")}

	m := baseIssueCommentMention()
	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", m)

	if got.HeadBranch != nil {
		t.Errorf("HeadBranch = %v, want nil (API call failed -- fall back to base repo's own default branch)", *got.HeadBranch)
	}
	if got.RepoName != m.RepoName || got.RepoCloneURL != m.RepoCloneURL {
		t.Errorf("RepoName/RepoCloneURL = %q/%q, want unchanged base repo %q/%q", got.RepoName, got.RepoCloneURL, m.RepoName, m.RepoCloneURL)
	}
}

// TestResolveIssueCommentHead_NilResolverFallsBack proves a nil resolver
// (no SourceControl wired -- e.g. handler_test.go's own minimal Config)
// falls back silently, with no call attempted at all.
func TestResolveIssueCommentHead_NilResolverFallsBack(t *testing.T) {
	m := baseIssueCommentMention()
	got := resolveIssueCommentHead(context.Background(), discardLogger(), nil, "gho_bottoken", m)

	if got.HeadBranch != nil {
		t.Errorf("HeadBranch = %v, want nil (resolver == nil)", *got.HeadBranch)
	}
	if got.RepoName != m.RepoName || got.RepoCloneURL != m.RepoCloneURL {
		t.Errorf("RepoName/RepoCloneURL changed with a nil resolver, want unchanged")
	}
}

// TestResolveIssueCommentHead_EmptyHeadRefFallsBack proves a successful
// but degenerate GetPullRequest response (empty head ref -- should never
// happen for a real, open GitHub PR, but not assumed away) falls back
// rather than setting HeadBranch to an empty string.
func TestResolveIssueCommentHead_EmptyHeadRefFallsBack(t *testing.T) {
	resolver := &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: ""}}

	m := baseIssueCommentMention()
	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", m)

	if got.HeadBranch != nil {
		t.Errorf("HeadBranch = %v, want nil (empty head ref reported)", *got.HeadBranch)
	}
}

// TestResolveIssueCommentHead_NullHeadRepoKeepsBaseRepo proves the L15
// "deleted fork" case is handled on this path too: a successful
// GetPullRequest call that reports a real head ref but an empty head repo
// (GitHub's own head.repo was null) still resolves HeadBranch, leaves the
// head repository unknown, and keeps RepoName/RepoCloneURL the base repo
// parseIssueComment already set.
func TestResolveIssueCommentHead_NullHeadRepoKeepsBaseRepo(t *testing.T) {
	resolver := &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "feature-x"}}

	m := baseIssueCommentMention()
	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", m)

	if got.HeadBranch == nil || *got.HeadBranch != "feature-x" {
		t.Errorf("HeadBranch = %v, want %q", got.HeadBranch, "feature-x")
	}
	if got.RepoName != m.RepoName || got.RepoCloneURL != m.RepoCloneURL {
		t.Errorf("RepoName/RepoCloneURL = %q/%q, want unchanged base repo %q/%q (head.repo was null)",
			got.RepoName, got.RepoCloneURL, m.RepoName, m.RepoCloneURL)
	}
	if got.HeadRepoFullName != "" {
		t.Errorf("HeadRepoFullName = %q, want empty: a deleted head repository is unknown", got.HeadRepoFullName)
	}
}

// TestResolveIssueCommentHead_StackThreadedThrough proves §8.2's own
// §17.6 amendment: a successful GetPullRequest response carrying a stack
// object populates m.Stack -- the SAME already-made call this function's
// own top doc comment describes, never a second one (this test's own
// resolver.calls assertion proves that directly).
func TestResolveIssueCommentHead_StackThreadedThrough(t *testing.T) {
	resolver := &fakePullRequestResolver{
		pr: githubapi.PullRequest{
			HeadRef: "feature-x",
			// Position/Size deliberately distinguishable (2 vs 3, not an
			// equal-valued pair) -- a confirmed audit finding noted that a
			// Position/Size field swap in this function's own mapping would
			// pass undetected against a fixture where both fields carried
			// the same value.
			Stack: &githubapi.StackInfo{Position: 2, Size: 3, BaseRef: "main", BaseSHA: "deadbeef"},
		},
	}

	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", baseIssueCommentMention())

	if resolver.calls != 1 {
		t.Fatalf("GetPullRequest call count = %d, want 1 (stack context must come from the SAME already-made call)", resolver.calls)
	}
	if got.Stack == nil {
		t.Fatal("Stack = nil, want non-nil when GetPullRequest reports one")
	}
	want := review.StackContext{Position: 2, Size: 3, UltimateBaseRef: "main", UltimateBaseSHA: "deadbeef"}
	if *got.Stack != want {
		t.Errorf("Stack = %+v, want %+v", *got.Stack, want)
	}
}

// TestResolveIssueCommentHead_NoStackLeavesNil proves the ordinary,
// non-stacked case leaves m.Stack nil, never a zero-valued, misleadingly-
// present struct.
func TestResolveIssueCommentHead_NoStackLeavesNil(t *testing.T) {
	resolver := &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "feature-x"}}

	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", baseIssueCommentMention())

	if got.Stack != nil {
		t.Errorf("Stack = %+v, want nil when GetPullRequest reports no stack", got.Stack)
	}
}

// TestResolveIssueCommentHead_UnsplittableRepoFullNameFallsBack proves a
// defensively-handled, never-expected-from-real-GitHub malformed
// RepoFullName (no "/") falls back rather than panicking or calling
// GetPullRequest with garbage owner/repo.
func TestResolveIssueCommentHead_UnsplittableRepoFullNameFallsBack(t *testing.T) {
	resolver := &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "feature-x"}}

	m := baseIssueCommentMention()
	m.RepoFullName = "not-a-valid-full-name"
	got := resolveIssueCommentHead(context.Background(), discardLogger(), resolver, "gho_bottoken", m)

	if resolver.calls != 0 {
		t.Errorf("GetPullRequest call count = %d, want 0 (never called with an unsplittable repo_full_name)", resolver.calls)
	}
	if got.HeadBranch != nil {
		t.Errorf("HeadBranch = %v, want nil", *got.HeadBranch)
	}
}

// Owner/repo splitting itself (formerly this file's own splitOwnerRepo) now
// lives at internal/domain/reposource.SplitFullName -- see that function's
// own doc comment for why ("review sessions", §8.2: a second
// caller, in a different package, needed the identical split). Its own
// table-driven test moved with it, to reposource_test.go's own
// TestSplitFullName; TestResolveIssueCommentHead_UnsplittableRepoFullNameFallsBack
// above still exercises this file's own resolveIssueCommentHead wrapping
// that shared function's ok=false return.

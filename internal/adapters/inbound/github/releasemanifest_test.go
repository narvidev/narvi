package github

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// fakePendingEnqueuer is a test-only stand-in for
// releasereview.PendingEnqueuer -- mirrors this file's own sibling
// fakePullRequestResolver (headresolve_test.go) precedent exactly: no
// real DB round trip. Blocking-finding fix #1: this replaces the
// pre-fix fakeMergedPRLister/fakeOutboxEnqueuer pair, since
// triggerReleaseManifestCheckBestEffort no longer calls ListMergedBetween
// or the outbox directly at all -- it only ever enqueues a
// release_manifest_pending row now; the actual check runs later, on
// internal/app/releasereview.Worker's own background loop.
type fakePendingEnqueuer struct {
	calls      int
	lastParams sqlcgen.CreateReleaseManifestPendingParams
	err        error
}

func (f *fakePendingEnqueuer) Create(_ context.Context, arg sqlcgen.CreateReleaseManifestPendingParams) (sqlcgen.ReleaseManifestPending, error) {
	f.calls++
	f.lastParams = arg
	if f.err != nil {
		return sqlcgen.ReleaseManifestPending{}, f.err
	}
	return sqlcgen.ReleaseManifestPending{SessionID: arg.SessionID}, nil
}

func testSessionID(t *testing.T) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan("11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("scan test session id: %v", err)
	}
	return id
}

func baseReleaseCfg(pr githubapi.PullRequest, pending *fakePendingEnqueuer) Config {
	return Config{
		Outbound:             platform.MustNewGitHubOutboundConfig("gho_bottoken"),
		PullRequests:         &fakePullRequestResolver{pr: pr},
		PendingChecks:        pending,
		ReleaseLabel:         "release",
		ReleaseBranchPattern: "release/*",
		Timeouts:             platform.DefaultTimeouts(),
	}
}

// TestTriggerReleaseManifestCheck_ReleaseBranchDetected proves a PR whose
// head branch matches the configured release pattern triggers the
// manifest-check ENQUEUE (never the check itself, which now only ever
// runs later, on Worker's own background loop) end to end: exactly one
// release_manifest_pending row is enqueued.
func TestTriggerReleaseManifestCheck_ReleaseBranchDetected(t *testing.T) {
	pending := &fakePendingEnqueuer{}
	cfg := baseReleaseCfg(githubapi.PullRequest{HeadRef: "release/2.4", BaseRef: "main"}, pending)

	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "acme/widgets", 42, testSessionID(t))

	if pending.calls != 1 {
		t.Fatalf("PendingChecks.Create calls = %d, want 1 (release PR should have been detected)", pending.calls)
	}
}

// TestTriggerReleaseManifestCheck_ReleaseLabelDetected proves the label
// axis of §15.1's OR also triggers the enqueue, independent of branch
// naming.
func TestTriggerReleaseManifestCheck_ReleaseLabelDetected(t *testing.T) {
	pending := &fakePendingEnqueuer{}
	cfg := baseReleaseCfg(githubapi.PullRequest{HeadRef: "develop", BaseRef: "main", Labels: []string{"release"}}, pending)

	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "acme/widgets", 42, testSessionID(t))

	if pending.calls != 1 {
		t.Fatalf("PendingChecks.Create calls = %d, want 1 (release label should have been detected)", pending.calls)
	}
}

// TestTriggerReleaseManifestCheck_OrdinaryPRNeverTriggers proves an
// ordinary PR (no release branch/label signal) never enqueues anything
// at all.
func TestTriggerReleaseManifestCheck_OrdinaryPRNeverTriggers(t *testing.T) {
	pending := &fakePendingEnqueuer{}
	cfg := baseReleaseCfg(githubapi.PullRequest{HeadRef: "feature/foo", BaseRef: "main"}, pending)

	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "acme/widgets", 42, testSessionID(t))

	if pending.calls != 0 {
		t.Errorf("PendingChecks.Create calls = %d, want 0 (not a release PR)", pending.calls)
	}
}

// TestTriggerReleaseManifestCheck_NilDepsSkipsEntirely proves a nil
// PendingChecks (this package's own handler_test.go, or any other
// minimal wiring) skips this function entirely -- no GetPullRequest call
// even attempted, mirroring cfg.DiffFetcher's own identical nil-safe
// precedent.
func TestTriggerReleaseManifestCheck_NilDepsSkipsEntirely(t *testing.T) {
	resolver := &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "release/2.4"}}
	cfg := Config{
		Outbound:     platform.MustNewGitHubOutboundConfig("gho_bottoken"),
		PullRequests: resolver,
		Timeouts:     platform.DefaultTimeouts(),
		// PendingChecks deliberately left nil.
	}

	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "acme/widgets", 42, testSessionID(t))

	if resolver.calls != 0 {
		t.Errorf("GetPullRequest calls = %d, want 0 (nil PendingChecks must skip before even fetching the PR)", resolver.calls)
	}
}

// TestTriggerReleaseManifestCheck_GetPullRequestFailsNeverPanics proves a
// failed GetPullRequest call (detection's own fresh fetch) degrades to
// "skip this PR", never a panic or an enqueue call with garbage data.
func TestTriggerReleaseManifestCheck_GetPullRequestFailsNeverPanics(t *testing.T) {
	pending := &fakePendingEnqueuer{}
	cfg := Config{
		Outbound:             platform.MustNewGitHubOutboundConfig("gho_bottoken"),
		PullRequests:         &fakePullRequestResolver{err: errors.New("network exploded")},
		PendingChecks:        pending,
		ReleaseLabel:         "release",
		ReleaseBranchPattern: "release/*",
		Timeouts:             platform.DefaultTimeouts(),
	}

	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "acme/widgets", 42, testSessionID(t))

	if pending.calls != 0 {
		t.Errorf("PendingChecks.Create calls = %d, want 0 (detection fetch itself failed)", pending.calls)
	}
}

// TestTriggerReleaseManifestCheck_UnsplittableRepoFullNameSkips proves a
// defensively-handled malformed repoFullName (no "/") never panics or
// calls GetPullRequest with garbage owner/repo.
func TestTriggerReleaseManifestCheck_UnsplittableRepoFullNameSkips(t *testing.T) {
	resolver := &fakePullRequestResolver{pr: githubapi.PullRequest{HeadRef: "release/2.4"}}
	pending := &fakePendingEnqueuer{}
	cfg := Config{
		Outbound:             platform.MustNewGitHubOutboundConfig("gho_bottoken"),
		PullRequests:         resolver,
		PendingChecks:        pending,
		ReleaseLabel:         "release",
		ReleaseBranchPattern: "release/*",
		Timeouts:             platform.DefaultTimeouts(),
	}

	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "not-a-valid-full-name", 42, testSessionID(t))

	if resolver.calls != 0 {
		t.Errorf("GetPullRequest calls = %d, want 0 (unsplittable repo_full_name)", resolver.calls)
	}
}

// TestTriggerReleaseManifestCheck_PayloadCarriesSessionAndPRIdentity
// proves the enqueued release_manifest_pending row correctly identifies
// the release PR -- a regression here would silently run the manifest
// check against the WRONG pull request once Worker later picks it up.
func TestTriggerReleaseManifestCheck_PayloadCarriesSessionAndPRIdentity(t *testing.T) {
	pending := &fakePendingEnqueuer{}
	cfg := baseReleaseCfg(githubapi.PullRequest{HeadRef: "release/2.4", BaseRef: "main"}, pending)

	sessionID := testSessionID(t)
	triggerReleaseManifestCheckBestEffort(context.Background(), discardLogger(), cfg, "acme/widgets", 42, sessionID)

	if pending.lastParams.SessionID != sessionID {
		t.Errorf("pending row SessionID = %v, want %v", pending.lastParams.SessionID, sessionID)
	}
	if pending.lastParams.Owner != "acme" || pending.lastParams.Repo != "widgets" || pending.lastParams.PrNumber != 42 {
		t.Errorf("pending row identity = %+v, want owner=acme repo=widgets pr_number=42", pending.lastParams)
	}
	if pending.lastParams.BaseRef != "main" || pending.lastParams.HeadRef != "release/2.4" {
		t.Errorf("pending row base/head = %+v, want base=main head=release/2.4", pending.lastParams)
	}
}

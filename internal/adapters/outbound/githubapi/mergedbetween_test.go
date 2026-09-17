package githubapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestListMergedBetween_FullScenario exercises ListMergedBetween end to
// end against a fake GitHub server standing in for every real endpoint
// it calls: compare (discovering constituent PRs from both the merge-
// commit and squash-merge message shapes), per-PR detail/reviews/files/
// commits, branch protection (admin-override inference), CI status +
// check-runs at the merge SHA, and the batch-wide revert search.
func TestListMergedBetween_FullScenario(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/compare/main...release-1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_commits": 2,
				"commits": []map[string]any{
					// Merge-commit strategy: PR #10.
					{"commit": map[string]any{"message": "Merge pull request #10 from acme/widgets/fix-thing\n\nFix thing"}},
					// Squash-merge strategy: PR #11.
					{"commit": map[string]any{"message": "Add feature (#11)"}},
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/10":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 10, "title": "Fix thing", "merged": true,
				"merged_at": "2024-01-01T00:00:00Z", "merge_commit_sha": "sha10",
				"base":   map[string]any{"ref": "main"},
				"labels": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/11":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 11, "title": "Add feature", "merged": true,
				"merged_at": "2024-01-02T00:00:00Z", "merge_commit_sha": "sha11",
				"base":   map[string]any{"ref": "main"},
				"labels": []map[string]any{{"name": "review:high-risk"}},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/10/reviews":
			// No approving review at all -- combined with branch
			// protection requiring one (below), this is the confirmed
			// admin-override case.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"state": "COMMENTED"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/11/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"state": "CHANGES_REQUESTED"},
				{"state": "APPROVED"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/99/reviews":
			// The revert PR's own review state: unreviewed.
			_ = json.NewEncoder(w).Encode([]map[string]any{})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/branches/main/protection":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"required_pull_request_reviews": map[string]any{"required_approving_review_count": 1},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha10/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "failure"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha11/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/10/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/domain/review/x.go"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/11/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/domain/review/y.go"}})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/10/commits":
			// Single-parent commits only -- no manual conflict resolution.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"parents": []map[string]any{{"sha": "p1"}}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/11/commits":
			// A 2-parent commit -- the manual-conflict-resolution heuristic.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"parents": []map[string]any{{"sha": "p1"}, {"sha": "p2"}}},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					// Body carries GitHub's own auto-generated "Reverts
					// owner/repo#N" reference -- blocking-finding fix #2's
					// PRIMARY, positively-linked identity match.
					{"number": 99, "title": `Revert "Fix thing"`, "body": "Reverts acme/widgets#10", "closed_at": "2024-01-01T02:00:00Z"},
				},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	merged, truncated, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if truncated {
		t.Error("ListMergedBetween() truncated = true, want false (full scenario, nothing capped, no fetch failures)")
	}
	if len(merged) != 2 {
		t.Fatalf("got %d merged PRs, want 2: %+v", len(merged), merged)
	}

	pr10, pr11 := merged[0], merged[1]
	if pr10.Number != 10 {
		t.Fatalf("merged[0].Number = %d, want 10 (compare's own commit order must be preserved)", pr10.Number)
	}
	if pr11.Number != 11 {
		t.Fatalf("merged[1].Number = %d, want 11", pr11.Number)
	}

	if pr10.HasApprovingReview {
		t.Error("PR 10: HasApprovingReview = true, want false (only a COMMENTED review present)")
	}
	if !pr10.MergedViaAdminOverride {
		t.Error("PR 10: MergedViaAdminOverride = false, want true (branch requires review, none present)")
	}
	if pr10.CIConclusionAtMergeSHA != ports.CIConclusionFailure {
		t.Errorf("PR 10: CIConclusionAtMergeSHA = %s, want %s", pr10.CIConclusionAtMergeSHA, ports.CIConclusionFailure)
	}
	if pr10.HadManualConflictResolution {
		t.Error("PR 10: HadManualConflictResolution = true, want false (single-parent commit only)")
	}
	if !pr10.WasReverted {
		t.Error("PR 10: WasReverted = false, want true (a matching revert PR was found)")
	}
	if pr10.RevertReviewState != ports.RevertReviewStateNotReviewed {
		t.Errorf("PR 10: RevertReviewState = %s, want %s (the revert PR carried no approving review)", pr10.RevertReviewState, ports.RevertReviewStateNotReviewed)
	}
	wantRevertedAt := time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)
	if pr10.RevertedAt == nil || !pr10.RevertedAt.Equal(wantRevertedAt) {
		t.Errorf("PR 10: RevertedAt = %v, want %v", pr10.RevertedAt, wantRevertedAt)
	}
	if len(pr10.ChangedPathPrefixes) != 1 || pr10.ChangedPathPrefixes[0] != "internal/domain" {
		t.Errorf("PR 10: ChangedPathPrefixes = %v, want [internal/domain]", pr10.ChangedPathPrefixes)
	}

	if !pr11.HasApprovingReview {
		t.Error("PR 11: HasApprovingReview = false, want true (an APPROVED review is present)")
	}
	if pr11.MergedViaAdminOverride {
		t.Error("PR 11: MergedViaAdminOverride = true, want false (it has an approving review)")
	}
	if pr11.CIConclusionAtMergeSHA != ports.CIConclusionSuccess {
		t.Errorf("PR 11: CIConclusionAtMergeSHA = %s, want %s", pr11.CIConclusionAtMergeSHA, ports.CIConclusionSuccess)
	}
	if !pr11.HadManualConflictResolution {
		t.Error("PR 11: HadManualConflictResolution = false, want true (a 2-parent commit is present)")
	}
	if pr11.WasReverted {
		t.Error("PR 11: WasReverted = true, want false (no matching revert PR)")
	}
	if len(pr11.Labels) != 1 || pr11.Labels[0] != "review:high-risk" {
		t.Errorf("PR 11: Labels = %v, want [review:high-risk]", pr11.Labels)
	}
}

// TestListMergedBetween_UnmergedCandidateExcluded proves a candidate PR
// number extracted from a commit message but reported by GitHub as NOT
// actually merged is silently excluded from the manifest -- never an
// error, never a fabricated entry.
func TestListMergedBetween_UnmergedCandidateExcluded(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/compare/main...release-1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commits": []map[string]any{
					{"commit": map[string]any{"message": "Merge pull request #10 from acme/widgets/fix-thing"}},
				},
			})
		case "/repos/acme/widgets/pulls/10":
			// merged: false -- e.g. a coincidental "(#10)" match, or a PR
			// later force-pushed/closed without merging.
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 10, "merged": false})
		case "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	merged, truncated, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if len(merged) != 0 {
		t.Fatalf("got %d merged PRs, want 0 (the one candidate was never actually merged): %+v", len(merged), merged)
	}
	if truncated {
		t.Error("ListMergedBetween() truncated = true, want false (a CONFIRMED not-merged exclusion is correct filtering, never a coverage gap)")
	}
}

// TestListMergedBetween_CompareFailurePropagatesError proves a genuine
// failure on the top-level compare call is a real, propagated error --
// this is the one call ListMergedBetween cannot degrade gracefully from
// (there is nothing to build a manifest over at all).
func TestListMergedBetween_CompareFailurePropagatesError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	_, _, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err == nil {
		t.Fatal("ListMergedBetween() error = nil, want a real error on a compare-call failure")
	}
}

// TestListMergedBetween_NoCandidates proves a range with no merge/squash-
// shaped commit messages at all (e.g. every commit was rebase-merged --
// this file's own documented, accepted limitation) returns an empty,
// non-nil-error manifest, never a spurious failure.
func TestListMergedBetween_NoCandidates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/compare/main...release-1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commits": []map[string]any{
					{"commit": map[string]any{"message": "Fix a typo"}},
				},
			})
		case "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	merged, truncated, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if len(merged) != 0 {
		t.Fatalf("got %d merged PRs, want 0: %+v", len(merged), merged)
	}
	if truncated {
		t.Error("ListMergedBetween() truncated = true, want false")
	}
}

// mergedPRServerFixture mounts every endpoint buildMergedPR unconditionally
// needs for ONE constituent PR (detail, reviews, branch protection,
// CI status/check-runs, files, commits) plus the batch-wide revert
// search -- factored out so the blocking-finding fix #2/#3/#4 regression
// tests below only need to override the ONE endpoint each is actually
// about, mirroring TestListMergedBetween_FullScenario's own shape without
// repeating its full switch statement three more times.
func mergedPRServerFixture(t *testing.T, prNumber int, prTitle string, mergedAt string, reviews []map[string]any, branchProtection map[string]any, searchItems []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		prPath := fmt.Sprintf("/repos/acme/widgets/pulls/%d", prNumber)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/compare/main...release-1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commits": []map[string]any{
					{"commit": map[string]any{"message": fmt.Sprintf("Merge pull request #%d from acme/widgets/x", prNumber)}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == prPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": prNumber, "title": prTitle, "merged": true,
				"merged_at": mergedAt, "merge_commit_sha": "sha",
				"base":   map[string]any{"ref": "main"},
				"labels": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == prPath+"/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			// Any OTHER PR's own /reviews call -- e.g. a revert candidate's
			// own review-state sub-fetch (fetchReverts, gated on its title
			// matching, independent of whether it ultimately gets accepted)
			// -- defaults to "no reviews at all"; the specific tests below
			// that care about a revert's own review state assert on it
			// directly rather than relying on this generic fallback.
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/branches/main/protection":
			if branchProtection == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(branchProtection)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == prPath+"/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == prPath+"/commits":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"parents": []map[string]any{{"sha": "p1"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": searchItems})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestListMergedBetween_UnrelatedOlderSameTitledRevertNeverFalselyMatched
// proves blocking-finding fix #2: a revert-titled search result matching
// this PR's own title, but that CLOSED BEFORE this PR ever merged (an
// unrelated, older revert of some OTHER, identically-titled PR -- e.g. a
// repeated Conventional-Commits title like "chore: bump dependencies"),
// must NEVER be accepted as THIS PR's own revert. Deliberately carries no
// body reference at all, so the ONLY way this could match is through the
// weaker title-only fallback -- exactly the path this fix's own
// revertedAt.After(mergedAt) guard closes.
func TestListMergedBetween_UnrelatedOlderSameTitledRevertNeverFalselyMatched(t *testing.T) {
	t.Parallel()

	server := mergedPRServerFixture(t, 50, "chore: bump dependencies",
		"2024-06-01T00:00:00Z",
		[]map[string]any{{"state": "APPROVED"}},
		nil,
		[]map[string]any{
			// An unrelated, MUCH OLDER revert PR of some other PR that
			// happened to carry the identical generic title -- closed
			// well BEFORE PR #50 above ever merged. No "body" field at
			// all -- this item can only ever match via the weaker
			// title-only fallback.
			{"number": 7, "title": `Revert "chore: bump dependencies"`, "closed_at": "2023-01-01T00:00:00Z"},
		},
	)
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	merged, _, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("got %d merged PRs, want 1: %+v", len(merged), merged)
	}
	if merged[0].WasReverted {
		t.Errorf("PR #50: WasReverted = true, want false (the only same-titled revert found is OLDER than this PR's own merge -- an unrelated revert of a different PR, not this one)")
	}
}

// TestListMergedBetween_RequiredApprovingReviewCountZero_NeverAdminOverride
// proves blocking-finding fix #3: a branch with "Require a pull request
// before merging" enabled but "Require approvals" left at 0 (a valid,
// common GitHub config) must NEVER be misread as requiring review --
// MergedViaAdminOverride must stay false for a PR merged there without
// any approving review.
func TestListMergedBetween_RequiredApprovingReviewCountZero_NeverAdminOverride(t *testing.T) {
	t.Parallel()

	server := mergedPRServerFixture(t, 60, "fix: thing",
		"2024-06-01T00:00:00Z",
		[]map[string]any{}, // no approving review at all
		map[string]any{"required_pull_request_reviews": map[string]any{"required_approving_review_count": 0}},
		[]map[string]any{},
	)
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	merged, _, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("got %d merged PRs, want 1: %+v", len(merged), merged)
	}
	if merged[0].HasApprovingReview {
		t.Fatal("PR #60: HasApprovingReview = true, want false (test setup: no reviews at all)")
	}
	if merged[0].MergedViaAdminOverride {
		t.Errorf("PR #60: MergedViaAdminOverride = true, want false (required_approving_review_count is 0 -- this branch never required a review at all, so there is nothing to override)")
	}
}

// TestListMergedBetween_RevertReviewFetchFails_ReportsUnknownNeverNotReviewed
// proves blocking-finding fix #4: a transient failure fetching the
// revert PR's OWN review state must degrade to
// ports.RevertReviewStateUnknown, never silently manufacture
// RevertReviewStateNotReviewed (the value that triggers the "unreviewed
// revert" finding) -- mirrors CIConclusionUnknown's own identical
// discipline elsewhere in this file.
func TestListMergedBetween_RevertReviewFetchFails_ReportsUnknownNeverNotReviewed(t *testing.T) {
	t.Parallel()

	const revertPRNumber = 88
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		revertReviewsPath := fmt.Sprintf("/repos/acme/widgets/pulls/%d/reviews", revertPRNumber)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == revertReviewsPath:
			// The revert PR's own review-state sub-fetch fails.
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/compare/main...release-1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commits": []map[string]any{
					{"commit": map[string]any{"message": "Merge pull request #70 from acme/widgets/x"}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/70":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 70, "title": "risky change", "merged": true,
				"merged_at": "2024-06-01T00:00:00Z", "merge_commit_sha": "sha70",
				"base":   map[string]any{"ref": "main"},
				"labels": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/70/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha70/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/70/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/70/commits":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"parents": []map[string]any{{"sha": "p1"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"number": revertPRNumber, "title": `Revert "risky change"`, "body": "Reverts acme/widgets#70", "closed_at": "2024-06-01T02:00:00Z"},
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	merged, _, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("got %d merged PRs, want 1: %+v", len(merged), merged)
	}
	if !merged[0].WasReverted {
		t.Fatal("PR #70: WasReverted = false, want true (the revert was positively identified by PR number -- only its OWN review state fetch failed)")
	}
	if merged[0].RevertReviewState != ports.RevertReviewStateUnknown {
		t.Errorf("PR #70: RevertReviewState = %s, want %s (a failed sub-fetch must never manufacture NotReviewed)", merged[0].RevertReviewState, ports.RevertReviewStateUnknown)
	}
}

// TestListMergedBetween_HalfReadCIFailsClosed is F5's own regression test
// fetchCIConclusion (mergedbetween.go), the RETROSPECTIVE
// sibling of fetchCIConclusionLive's own F1 fix (listopenprs.go), used to
// report a confident CIConclusionSuccess whenever ONE of its two GETs
// (combined-status, check-runs) succeeded reporting green while the OTHER
// itself failed or decoded a confirmed-truncated page -- exactly the same
// half-read-reports-green defect F1 closed for the live gate, left open
// here across an earlier round of this same review series. Fixed to
// report CIConclusionUnknown instead -- this package's own pre-existing,
// already-non-accusatory "could not be determined" value (see
// ComputeReleaseManifestFindings' own doc comment, manifestcheck.go, for
// why Unknown is safe here and Success is not) -- whenever either GET
// could not be fully read, mirroring fetchCIConclusionLive's own identical
// fail-closed shape one file over. A genuine, CONFIRMED failure from
// whichever GET DID succeed still wins regardless of the other GET's own
// health, exactly like that sibling.
func TestListMergedBetween_HalfReadCIFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		statusHandler func(w http.ResponseWriter)
		checksHandler func(w http.ResponseWriter)
		want          ports.CIConclusion
	}{
		{
			name: "both GETs succeed, both report success",
			statusHandler: func(w http.ResponseWriter) {
				_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
			},
			checksHandler: func(w http.ResponseWriter) {
				_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{{"conclusion": "success"}}})
			},
			want: ports.CIConclusionSuccess,
		},
		{
			// F5's own reproduction: a status GET succeeding beside a
			// failing check-runs GET must not report a confident Success
			// -- that Success could be masking a real, unread failure
			// that ComputeReleaseManifestFindings would otherwise have
			// flagged as ManifestFindingRedAtMerge.
			name: "status succeeds (success), check-runs GET fails (500)",
			statusHandler: func(w http.ResponseWriter) {
				_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
			},
			checksHandler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: ports.CIConclusionUnknown,
		},
		{
			// The mirror image -- the OTHER of the two GETs failing.
			name: "check-runs succeeds (success), status GET fails (500)",
			statusHandler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			checksHandler: func(w http.ResponseWriter) {
				_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{{"conclusion": "success"}}})
			},
			want: ports.CIConclusionUnknown,
		},
		{
			name: "both GETs fail (500)",
			statusHandler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			checksHandler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: ports.CIConclusionUnknown,
		},
		{
			// A genuine, CONFIRMED failure from the GET that DID succeed
			// is still real signal and still wins over the other GET's
			// own degraded read -- mirrors fetchCIConclusionLive's own
			// identical precedent.
			name: "check-runs reports a confirmed failure, status GET fails (500)",
			statusHandler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			checksHandler: func(w http.ResponseWriter) {
				_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{{"conclusion": "failure"}}})
			},
			want: ports.CIConclusionFailure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/compare/main...release-1.0":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"commits": []map[string]any{
							{"commit": map[string]any{"message": "Merge pull request #50 from acme/widgets/x"}},
						},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/50":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 50, "title": "x", "merged": true,
						"merged_at": "2024-06-01T00:00:00Z", "merge_commit_sha": "sha50",
						"base":   map[string]any{"ref": "main"},
						"labels": []map[string]any{},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/50/reviews":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED"}})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha50/status":
					tc.statusHandler(w)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha50/check-runs":
					tc.checksHandler(w)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/50/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/50/commits":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"parents": []map[string]any{{"sha": "p1"}}}})
				case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)
			merged, _, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
				Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
			})
			if err != nil {
				t.Fatalf("ListMergedBetween() error = %v", err)
			}
			if len(merged) != 1 {
				t.Fatalf("got %d merged PRs, want 1: %+v", len(merged), merged)
			}
			if merged[0].CIConclusionAtMergeSHA != tc.want {
				t.Errorf("CIConclusionAtMergeSHA = %v, want %v", merged[0].CIConclusionAtMergeSHA, tc.want)
			}
		})
	}
}

// TestListMergedBetween_CheckRunsTruncatedFirstPageFailsClosed is F5's own
// sibling of F1's identical fix (listopenprs.go, F1's own reproduction
// test) applied to fetchCIConclusion: a merge SHA carrying more check
// runs than GitHub's documented default page size (30, when no per_page is
// sent) must never report CIConclusionSuccess off a truncated first page
// -- reproduced exactly like F1's own listopenprs.go test, 40 check runs,
// run #35 (index 34) concluding "failure".
func TestListMergedBetween_CheckRunsTruncatedFirstPageFailsClosed(t *testing.T) {
	t.Parallel()

	const total = 40
	const failingIndex = 34 // run #35, 0-indexed

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/compare/main...release-1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commits": []map[string]any{
					{"commit": map[string]any{"message": "Merge pull request #51 from acme/widgets/x"}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/51":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 51, "title": "x", "merged": true,
				"merged_at": "2024-06-01T00:00:00Z", "merge_commit_sha": "sha51",
				"base":   map[string]any{"ref": "main"},
				"labels": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/51/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha51/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "total_count": 0})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/sha51/check-runs":
			perPage := r.URL.Query().Get("per_page")
			served := total
			if perPage != "100" {
				served = 30
			}
			runs := make([]map[string]any, 0, served)
			for i := 0; i < served; i++ {
				concl := "success"
				if i == failingIndex {
					concl = "failure"
				}
				runs = append(runs, map[string]any{"conclusion": concl})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "check_runs": runs})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/51/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/51/commits":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"parents": []map[string]any{{"sha": "p1"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	merged, _, err := adapter.ListMergedBetween(context.Background(), ports.ListMergedBetweenSpec{
		Owner: "acme", Repo: "widgets", BaseRef: "main", HeadRef: "release-1.0", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListMergedBetween() error = %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("got %d merged PRs, want 1: %+v", len(merged), merged)
	}
	if merged[0].CIConclusionAtMergeSHA != ports.CIConclusionFailure {
		t.Errorf("CIConclusionAtMergeSHA = %v, want failure -- per_page=100 must surface run #51's own confirmed failure, never a stale prefix's all-green view", merged[0].CIConclusionAtMergeSHA)
	}
}

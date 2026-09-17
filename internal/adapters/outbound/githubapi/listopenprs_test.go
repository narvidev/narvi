package githubapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestListOpenPRsForUser_FullScenario exercises ListOpenPRsForUser end to
// end: id->login resolution, both search qualifiers (with an overlapping
// PR deduplicated across them), and full per-PR detail assembly (review
// decision, CI at head SHA via both CI surfaces, changed files, labels,
// assignees/reviewers/teams).
func TestListOpenPRsForUser_FullScenario(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/9001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "login": "octocat"})

		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			switch {
			case strings.Contains(q, "assignee:octocat"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"items": []map[string]any{
						{"number": 1204, "repository_url": "https://api.github.com/repos/acme/widgets"},
					},
				})
			case strings.Contains(q, "review-requested:octocat"):
				// #1204 also shows up here -- must be deduplicated against
				// the assignee query's own result, never built twice.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"items": []map[string]any{
						{"number": 1204, "repository_url": "https://api.github.com/repos/acme/widgets"},
						{"number": 1187, "repository_url": "https://api.github.com/repos/acme/payroll-api"},
					},
				})
			default:
				t.Errorf("unexpected search query: %q", q)
			}

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/1204":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 1204, "title": "scheduler: exponential backoff", "html_url": "https://github.com/acme/widgets/pull/1204",
				"state": "open", "draft": false, "additions": 40, "deletions": 5,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha1204"},
				"base":                map[string]any{"ref": "main"},
				"labels":              []map[string]any{{"name": "review:low-risk"}},
				"assignees":           []map[string]any{{"id": 9001, "login": "octocat"}},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/1204/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED", "user": map[string]any{"id": 7001, "login": "reviewer-1204"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha1204/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha1204/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/1204/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/app/scheduler/backoff.go"}})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/payroll-api/pulls/1187":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 1187, "title": "harden webhook signature validation", "html_url": "https://github.com/acme/payroll-api/pull/1187",
				"state": "open", "draft": false, "additions": 120, "deletions": 30,
				"created_at": "2026-08-06T08:00:00Z", "updated_at": "2026-08-06T09:00:00Z",
				"user":                map[string]any{"id": 600, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha1187"},
				"base":                map[string]any{"ref": "main"},
				"labels":              []map[string]any{{"name": "review:medium-risk"}},
				"assignees":           []map[string]any{},
				"requested_reviewers": []map[string]any{{"id": 9001, "login": "octocat"}},
				"requested_teams":     []map[string]any{{"slug": "payroll-team"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/payroll-api/pulls/1187/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"state": "CHANGES_REQUESTED", "user": map[string]any{"id": 7002, "login": "reviewer-1187"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/payroll-api/commits/headsha1187/status":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/payroll-api/commits/headsha1187/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{{"conclusion": "failure"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/payroll-api/pulls/1187/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/webhook/verify.go"}})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	prs, truncated, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{
		GitHubExternalID: "9001", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListOpenPRsForUser() error = %v, want nil", err)
	}
	if truncated {
		t.Error("truncated = true, want false (both discovery queries succeeded)")
	}
	if len(prs) != 2 {
		t.Fatalf("ListOpenPRsForUser() returned %d PRs, want 2 (deduplicated): %+v", len(prs), prs)
	}

	byNumber := map[int]ports.OpenPR{}
	for _, pr := range prs {
		byNumber[pr.Number] = pr
	}

	pr1204, ok := byNumber[1204]
	if !ok {
		t.Fatal("missing PR #1204")
	}
	if pr1204.HeadSHA != "headsha1204" || !pr1204.HasApprovingReview || pr1204.HasChangesRequested {
		t.Errorf("PR #1204 = %+v, unexpected review/head-sha fields", pr1204)
	}
	if pr1204.CIConclusion != ports.CIConclusionSuccess {
		t.Errorf("PR #1204 CIConclusion = %v, want success", pr1204.CIConclusion)
	}
	if len(pr1204.Assignees) != 1 || pr1204.Assignees[0].ExternalID != "9001" {
		t.Errorf("PR #1204 Assignees = %+v, want [octocat]", pr1204.Assignees)
	}
	if len(pr1204.ChangedFiles) != 1 || pr1204.ChangedFiles[0] != "internal/app/scheduler/backoff.go" {
		t.Errorf("PR #1204 ChangedFiles = %+v", pr1204.ChangedFiles)
	}

	pr1187, ok := byNumber[1187]
	if !ok {
		t.Fatal("missing PR #1187")
	}
	if pr1187.HasApprovingReview || !pr1187.HasChangesRequested {
		t.Errorf("PR #1187 review decision = approving=%v changesRequested=%v, want false/true", pr1187.HasApprovingReview, pr1187.HasChangesRequested)
	}
	if pr1187.CIConclusion != ports.CIConclusionFailure {
		t.Errorf("PR #1187 CIConclusion = %v, want failure (from check-runs, combined status 404)", pr1187.CIConclusion)
	}
	if len(pr1187.RequestedReviewers) != 1 || pr1187.RequestedReviewers[0].Login != "octocat" {
		t.Errorf("PR #1187 RequestedReviewers = %+v", pr1187.RequestedReviewers)
	}
	if len(pr1187.RequestedTeams) != 1 || pr1187.RequestedTeams[0] != "acme/payroll-team" {
		t.Errorf("PR #1187 RequestedTeams = %+v, want [acme/payroll-team]", pr1187.RequestedTeams)
	}
}

func TestListOpenPRsForUser_OneQueryFailingDoesNotBlankTheOther(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "octocat"})
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "assignee:octocat"):
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "review-requested:octocat"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{"number": 5, "repository_url": "https://api.github.com/repos/acme/widgets"}},
			})
		case r.URL.Path == "/repos/acme/widgets/pulls/5":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 5, "title": "x", "html_url": "u", "state": "open", "head": map[string]any{"sha": "s"}, "base": map[string]any{"ref": "main"},
			})
		case r.URL.Path == "/repos/acme/widgets/pulls/5/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.URL.Path == "/repos/acme/widgets/commits/s/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.URL.Path == "/repos/acme/widgets/commits/s/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.URL.Path == "/repos/acme/widgets/pulls/5/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	prs, truncated, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1", Token: "tok"})
	if err != nil {
		t.Fatalf("ListOpenPRsForUser() error = %v, want nil (one query failing is best-effort)", err)
	}
	if len(prs) != 1 || prs[0].Number != 5 {
		t.Errorf("ListOpenPRsForUser() = %+v, want exactly PR #5 from the surviving query", prs)
	}
	// the surviving query's own result is still
	// returned in full (never blanked out), but truncated must still be
	// true -- this result is a known-incomplete picture (the assignee:
	// query's own failure means any PR only discoverable THAT way is
	// silently missing), which a caller must not cache/present as
	// confirmed-complete.
	if !truncated {
		t.Error("truncated = false, want true (the assignee: query failed)")
	}
}

// TestListOpenPRsForUser_QueuedOrCancelledCheckIsNotGreen proves
// fetchCIConclusionLive's own STRICT departure from fetchCIConclusion's
// lenient "any confirmed success, no confirmed failure" rule: a live, pre-merge read must never report CIConclusionSuccess
// while a required check is still queued/in_progress (Conclusion == nil)
// or was cancelled -- "some check finished green" must never stand in for
// "the whole suite is green" while other checks are still outstanding.
func TestListOpenPRsForUser_QueuedOrCancelledCheckIsNotGreen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		checkRuns []map[string]any
		want      ports.CIConclusion
	}{
		{
			name: "one success, one still queued",
			checkRuns: []map[string]any{
				{"conclusion": "success"},
				{"conclusion": nil},
			},
			want: ports.CIConclusionUnknown,
		},
		{
			name: "one success, one cancelled",
			checkRuns: []map[string]any{
				{"conclusion": "success"},
				{"conclusion": "cancelled"},
			},
			want: ports.CIConclusionUnknown,
		},
		{
			name: "one failure, one still queued -- failure wins",
			checkRuns: []map[string]any{
				{"conclusion": "failure"},
				{"conclusion": nil},
			},
			want: ports.CIConclusionFailure,
		},
		{
			name: "every check concluded success",
			checkRuns: []map[string]any{
				{"conclusion": "success"},
				{"conclusion": "neutral"},
			},
			want: ports.CIConclusionSuccess,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/user/1":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "octocat"})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "assignee:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"items": []map[string]any{{"number": 5, "repository_url": "https://api.github.com/repos/acme/widgets"}},
					})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "review-requested:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
				case r.URL.Path == "/repos/acme/widgets/pulls/5":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 5, "title": "x", "html_url": "u", "state": "open", "head": map[string]any{"sha": "s"}, "base": map[string]any{"ref": "main"},
					})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/reviews":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				case r.URL.Path == "/repos/acme/widgets/commits/s/status":
					// No legacy combined-status signal at all -- this test
					// isolates the Checks API surface.
					w.WriteHeader(http.StatusNotFound)
				case r.URL.Path == "/repos/acme/widgets/commits/s/check-runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": tc.checkRuns})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)

			prs, _, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1", Token: "tok"})
			if err != nil {
				t.Fatalf("ListOpenPRsForUser() error = %v, want nil", err)
			}
			if len(prs) != 1 {
				t.Fatalf("ListOpenPRsForUser() returned %d PRs, want 1", len(prs))
			}
			if prs[0].CIConclusion != tc.want {
				t.Errorf("CIConclusion = %v, want %v", prs[0].CIConclusion, tc.want)
			}
		})
	}
}

// TestListOpenPRsForUser_PendingCombinedStatusRequiresARealStatus is the
// P0 (BLOCKER) regression test for a second review round. GitHub's
// own documented rule for the combined-status endpoint is "pending if
// there are no statuses or a context is pending" -- a repo whose CI runs
// exclusively through GitHub Actions check-runs (the dominant modern CI
// configuration, including this repo's own) has ZERO legacy commit
// statuses, so a VALID commit at that repo returns 200
// {"state":"pending","statuses":[],"total_count":0} forever, never a 404
// (the previous test suite's own stub, which the review correctly called
// out as unrealistic). Before the fix, fetchCIConclusionLive read
// state=="pending" alone as "a legacy status is still in flight"
// (sawIncomplete=true), which outranks sawSuccess in the final switch --
// so CIConclusion could NEVER be Success on such a repo no matter how
// green every real check-run was: ciGreen would always be false
// (aggregate.go), ComputeAutoApprovalEligible would always refuse, and
// RevalidateForMerge would 409 every merge -- the headline ready_to_merge
// feature would be entirely non-functional. A genuinely in-flight legacy
// status (total_count > 0) must still correctly block green.
func TestListOpenPRsForUser_PendingCombinedStatusRequiresARealStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		statusTotalCount int
		checkRuns        []map[string]any
		want             ports.CIConclusion
	}{
		{
			name:             "Actions-only repo: statusless pending is not an incomplete signal",
			statusTotalCount: 0,
			checkRuns: []map[string]any{
				{"conclusion": "success"},
				{"conclusion": "neutral"},
			},
			want: ports.CIConclusionSuccess,
		},
		{
			name:             "a genuinely in-flight legacy status still blocks green",
			statusTotalCount: 1,
			checkRuns: []map[string]any{
				{"conclusion": "success"},
			},
			want: ports.CIConclusionUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/user/1":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "octocat"})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "assignee:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"items": []map[string]any{{"number": 5, "repository_url": "https://api.github.com/repos/acme/widgets"}},
					})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "review-requested:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
				case r.URL.Path == "/repos/acme/widgets/pulls/5":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 5, "title": "x", "html_url": "u", "state": "open", "head": map[string]any{"sha": "s"}, "base": map[string]any{"ref": "main"},
					})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/reviews":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				case r.URL.Path == "/repos/acme/widgets/commits/s/status":
					// The realistic shape a VALID commit always returns
					// (200, never 404): an Actions-only repo's own combined
					// status is state=="pending" with zero statuses, not an
					// absent resource.
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "statuses": []map[string]any{}, "total_count": tc.statusTotalCount})
				case r.URL.Path == "/repos/acme/widgets/commits/s/check-runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": tc.checkRuns})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)

			prs, _, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1", Token: "tok"})
			if err != nil {
				t.Fatalf("ListOpenPRsForUser() error = %v, want nil", err)
			}
			if len(prs) != 1 {
				t.Fatalf("ListOpenPRsForUser() returned %d PRs, want 1", len(prs))
			}
			if prs[0].CIConclusion != tc.want {
				t.Errorf("CIConclusion = %v, want %v", prs[0].CIConclusion, tc.want)
			}
		})
	}
}

// TestListOpenPRsForUser_ReviewDecisionReducesToLatestPerReviewer is the
// P1-1 regression test: fetchReviewDecision
// must reduce GitHub's own append-only review list to each reviewer's
// LATEST decision. A reviewer whose CHANGES_REQUESTED review is followed
// by their own later APPROVED review must read as approved, never
// changes-requested -- the naive "any CHANGES_REQUESTED row, ever" rule
// would make a legitimately re-approved PR permanently unmergeable
// (HasChangesRequested is a hard merge gate at RevalidateForMerge). The
// reverse order (approved, then later changes-requested by the SAME
// reviewer) must still correctly read as changes-requested. A later
// COMMENTED review must never reset a reviewer's own standing decision.
func TestListOpenPRsForUser_ReviewDecisionReducesToLatestPerReviewer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		reviews              []map[string]any
		wantApproving        bool
		wantChangesRequested bool
	}{
		{
			name: "changes requested then later approved by the same reviewer reads as approved",
			reviews: []map[string]any{
				{"state": "CHANGES_REQUESTED", "user": map[string]any{"id": 42, "login": "reviewer"}},
				{"state": "APPROVED", "user": map[string]any{"id": 42, "login": "reviewer"}},
			},
			wantApproving:        true,
			wantChangesRequested: false,
		},
		{
			name: "approved then later changes requested by the same reviewer reads as changes requested",
			reviews: []map[string]any{
				{"state": "APPROVED", "user": map[string]any{"id": 42, "login": "reviewer"}},
				{"state": "CHANGES_REQUESTED", "user": map[string]any{"id": 42, "login": "reviewer"}},
			},
			wantApproving:        false,
			wantChangesRequested: true,
		},
		{
			name: "a later COMMENTED review never withdraws a standing changes-requested decision",
			reviews: []map[string]any{
				{"state": "CHANGES_REQUESTED", "user": map[string]any{"id": 42, "login": "reviewer"}},
				{"state": "COMMENTED", "user": map[string]any{"id": 42, "login": "reviewer"}},
			},
			wantApproving:        false,
			wantChangesRequested: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/user/1":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "octocat"})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "assignee:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"items": []map[string]any{{"number": 5, "repository_url": "https://api.github.com/repos/acme/widgets"}},
					})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "review-requested:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
				case r.URL.Path == "/repos/acme/widgets/pulls/5":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 5, "title": "x", "html_url": "u", "state": "open", "head": map[string]any{"sha": "s"}, "base": map[string]any{"ref": "main"},
					})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/reviews":
					_ = json.NewEncoder(w).Encode(tc.reviews)
				case r.URL.Path == "/repos/acme/widgets/commits/s/status":
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
				case r.URL.Path == "/repos/acme/widgets/commits/s/check-runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)

			prs, _, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1", Token: "tok"})
			if err != nil {
				t.Fatalf("ListOpenPRsForUser() error = %v, want nil", err)
			}
			if len(prs) != 1 {
				t.Fatalf("ListOpenPRsForUser() returned %d PRs, want 1", len(prs))
			}
			if prs[0].HasApprovingReview != tc.wantApproving {
				t.Errorf("HasApprovingReview = %v, want %v", prs[0].HasApprovingReview, tc.wantApproving)
			}
			if prs[0].HasChangesRequested != tc.wantChangesRequested {
				t.Errorf("HasChangesRequested = %v, want %v", prs[0].HasChangesRequested, tc.wantChangesRequested)
			}
		})
	}
}

// TestListOpenPRsForUser_ChangedFilesCountAndDegraded is the Phase 5 audit
// (findings 1+2, both fixed) regression test at the adapter level, where
// both holes actually originated. ports.OpenPR.ChangedFilesCount must
// reflect GitHub's own authoritative "changed_files" scalar on the PR
// detail response (never len() of the SEPARATE, page-capped Pull Request
// Files listing) -- finding 2's own root cause, a PR author fully
// controls both filenames and diff order, so len() alone is gameable past
// a page boundary. ChangedFilesListDegraded must be set true whenever
// that separate listing cannot be trusted as a COMPLETE picture -- either
// because the fetch itself failed outright (finding 1: the exact
// GET /pulls/{n}/files 502 scenario the audit names), or because it
// succeeded but GitHub's own scalar total exceeds what one page
// (per_page=100) returned (finding 2's own truncation half). Mutation-test
// target: reverting fetchChangedFilePaths' wiring in buildOpenPRFromDetail
// back to trusting len(files) alone (dropping the detail.ChangedFiles >
// len(files) comparison) must turn the "large PR" case below from
// degraded=true back to degraded=false.
func TestListOpenPRsForUser_ChangedFilesCountAndDegraded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		changedFilesScalar    int
		filesEndpointStatus   int // 0 means respond 200 with a files page
		filesResponseCount    int // number of file entries the /files page returns
		wantChangedFilesCount int
		wantListDegraded      bool
		wantChangedFilesLen   int
	}{
		{
			name:                  "small PR: scalar and the one-page listing agree, never degraded",
			changedFilesScalar:    3,
			filesResponseCount:    3,
			wantChangedFilesCount: 3,
			wantListDegraded:      false,
			wantChangedFilesLen:   3,
		},
		{
			// The Finding 2 scenario: GitHub's own authoritative scalar
			// (150) exceeds what fetchChangedFilePaths' own per_page=100
			// cap returned (100) -- a genuine, truncated PREFIX. The
			// SCALAR, not len(), is what ChangedFilesCount must report.
			name:                  "large PR: scalar exceeds the one-page cap -- degraded, but ChangedFilesCount still reports the real scalar, never len()",
			changedFilesScalar:    150,
			filesResponseCount:    100,
			wantChangedFilesCount: 150,
			wantListDegraded:      true,
			wantChangedFilesLen:   100,
		},
		{
			// The Finding 1 scenario: the audit's own named example is
			// "GET /pulls/{n}/files returns 502 during aggregation" --
			// the separate changed-files fetch fails outright, but the
			// scalar (from the ALREADY-SUCCEEDED, separate detail call)
			// is still honestly reported.
			name:                  "changed-files fetch fails outright -- degraded, listing nil, but the scalar (from the separate, already-succeeded detail call) is still reported",
			changedFilesScalar:    42,
			filesEndpointStatus:   http.StatusInternalServerError,
			wantChangedFilesCount: 42,
			wantListDegraded:      true,
			wantChangedFilesLen:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/user/1":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "octocat"})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "assignee:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"items": []map[string]any{{"number": 5, "repository_url": "https://api.github.com/repos/acme/widgets"}},
					})
				case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "review-requested:octocat"):
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
				case r.URL.Path == "/repos/acme/widgets/pulls/5":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 5, "title": "x", "html_url": "u", "state": "open", "head": map[string]any{"sha": "s"}, "base": map[string]any{"ref": "main"},
						"changed_files": tc.changedFilesScalar,
					})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/reviews":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				case r.URL.Path == "/repos/acme/widgets/commits/s/status":
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
				case r.URL.Path == "/repos/acme/widgets/commits/s/check-runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
				case r.URL.Path == "/repos/acme/widgets/pulls/5/files":
					if tc.filesEndpointStatus != 0 {
						w.WriteHeader(tc.filesEndpointStatus)
						return
					}
					files := make([]map[string]any, tc.filesResponseCount)
					for i := range files {
						files[i] = map[string]any{"filename": "file" + strconv.Itoa(i) + ".go"}
					}
					_ = json.NewEncoder(w).Encode(files)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)

			prs, _, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1", Token: "tok"})
			if err != nil {
				t.Fatalf("ListOpenPRsForUser() error = %v, want nil", err)
			}
			if len(prs) != 1 {
				t.Fatalf("ListOpenPRsForUser() returned %d PRs, want 1", len(prs))
			}
			pr := prs[0]
			if pr.ChangedFilesCount != tc.wantChangedFilesCount {
				t.Errorf("ChangedFilesCount = %d, want %d", pr.ChangedFilesCount, tc.wantChangedFilesCount)
			}
			if pr.ChangedFilesListDegraded != tc.wantListDegraded {
				t.Errorf("ChangedFilesListDegraded = %v, want %v", pr.ChangedFilesListDegraded, tc.wantListDegraded)
			}
			if len(pr.ChangedFiles) != tc.wantChangedFilesLen {
				t.Errorf("len(ChangedFiles) = %d, want %d", len(pr.ChangedFiles), tc.wantChangedFilesLen)
			}
		})
	}
}

// TestListOpenPRsForUser_ClosedCandidate_DroppedFromResults is round-11
// finding C's own regression test: round-10 finding E added a
// detail.State check to GetOpenPR (getopenpr.go, the MACHINE-initiated
// discovery primitive RevalidateForAutoMerge uses) but never to
// buildOpenPR here -- the HUMAN merge-click path's own discovery
// primitive, reached via ListOpenPRsForUser and
// decisioninbox.RevalidateForMerge. searchOpenPRs' own "is:pr is:open"
// qualifier queries GitHub's Search API index, which is eventually
// consistent and can surface a candidate that has since closed (or
// merged -- GitHub reports both as state == "closed", never a distinct
// "merged" value, openPRDetailResponse.State's own doc comment) in the
// gap between the search and this detail fetch -- before this fix, a
// confirmed-present 200 response reporting state == "closed" still built
// a full ports.OpenPR and returned ok=true here, identically to a
// genuinely open PR.
func TestListOpenPRsForUser_ClosedCandidate_DroppedFromResults(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/9002":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9002, "login": "octocat2"})

		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			// Both qualifiers report the SAME stale search hit -- GitHub's
			// search index has not yet caught up with this PR's own real,
			// closed state.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"number": 2001, "repository_url": "https://api.github.com/repos/acme/widgets"},
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/2001":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 2001, "title": "stale search hit, actually closed", "html_url": "https://github.com/acme/widgets/pull/2001",
				"state": "closed", "draft": false, "additions": 10, "deletions": 2,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha2001"},
				"base":                map[string]any{"ref": "main"},
				"labels":              []map[string]any{},
				"assignees":           []map[string]any{{"id": 9002, "login": "octocat2"}},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
			})
			// No further GET should be attempted for this PR (reviews, CI,
			// changed files) -- buildOpenPR must drop it on the state check
			// alone, before any of buildOpenPRFromDetail's own further
			// fetches. Any request to those paths is caught by the default
			// case below and fails the test.

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	prs, truncated, err := adapter.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{
		GitHubExternalID: "9002", Token: "tok",
	})
	if err != nil {
		t.Fatalf("ListOpenPRsForUser() error = %v, want nil", err)
	}
	if truncated {
		t.Error("truncated = true, want false -- a per-PR drop (this PR's own detail confirms it is closed) is never a discovery-QUERY failure, mirrors buildOpenPR's own established per-PR-failure discipline")
	}
	if len(prs) != 0 {
		t.Fatalf("ListOpenPRsForUser() returned %d PRs, want 0 -- a closed pull request must never render as an open one, exactly like GetOpenPR (getopenpr.go, round-10 finding E) already refuses it on the machine-initiated path: %+v", len(prs), prs)
	}
}

package githubapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
)

// planUnavailableBody is GitHub's answer for a feature the repository's plan
// does not offer.
const planUnavailableBody = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","documentation_url":"https://docs.github.com/rest"}`

// protectedBranch is a branch object whose protection requires ci/build
// from App 15368, lint from any source, and deploy/preview by the older
// name-only list.
func protectedBranch() map[string]any {
	return map[string]any{
		"name":      "main",
		"protected": true,
		"protection": map[string]any{
			"enabled": true,
			"required_status_checks": map[string]any{
				"enforcement_level": "non_admins",
				"contexts":          []string{"ci/build", "lint", "deploy/preview"},
				"checks": []map[string]any{
					{"context": "ci/build", "app_id": 15368},
					{"context": "lint", "app_id": nil},
				},
			},
		},
	}
}

// unprotectedBranch is what GitHub returns for a branch nothing protects.
func unprotectedBranch() map[string]any {
	return map[string]any{
		"name":      "main",
		"protected": false,
		"protection": map[string]any{
			"enabled": false,
			"required_status_checks": map[string]any{
				"enforcement_level": "off",
				"contexts":          []string{},
				"checks":            []map[string]any{},
			},
		},
	}
}

// statusChecksRule is one active ruleset rule of type
// required_status_checks.
func statusChecksRule(checks ...map[string]any) map[string]any {
	return map[string]any{
		"type":                "required_status_checks",
		"ruleset_source_type": "Repository",
		"ruleset_source":      "acme/widgets",
		"ruleset_id":          42,
		"parameters": map[string]any{
			"strict_required_status_checks_policy": false,
			"required_status_checks":               checks,
		},
	}
}

type requiredChecksHandler func(w http.ResponseWriter, r *http.Request)

func writeJSON(v any) requiredChecksHandler {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeStatus(status int, body string, header map[string]string) requiredChecksHandler {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestListRequiredChecks reads both sources of a base branch's required
// checks against an httptest GitHub: the branch object's protection (never
// the admin-only protection endpoint -- any other path fails the test) and
// the branch's rulesets. A source the plan does not offer declares
// nothing; every other failure is an error, never "requires nothing".
func TestListRequiredChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		branch  requiredChecksHandler
		rules   func(page int) requiredChecksHandler
		want    []ports.RequiredCheck
		wantErr bool
	}{
		{
			name:   "protection and rulesets both declare checks",
			branch: writeJSON(protectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeJSON([]map[string]any{
					{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 1}},
					{"type": "deletion"},
					statusChecksRule(map[string]any{"context": "security/scan", "integration_id": 777}, map[string]any{"context": "e2e"}),
				})
			},
			want: []ports.RequiredCheck{
				{Name: "ci/build", AppID: 15368},
				{Name: "lint"},
				{Name: "deploy/preview"},
				{Name: "security/scan", AppID: 777},
				{Name: "e2e"},
			},
		},
		{
			name:   "an unprotected branch with no rules requires nothing",
			branch: writeJSON(unprotectedBranch()),
			rules:  func(int) requiredChecksHandler { return writeJSON([]map[string]any{}) },
			want:   nil,
		},
		{
			name: "an App id of -1 or null names no App",
			branch: writeJSON(map[string]any{"protection": map[string]any{"required_status_checks": map[string]any{
				"checks": []map[string]any{{"context": "a", "app_id": -1}, {"context": "b", "app_id": nil}, {"context": "c"}},
			}}}),
			rules: func(int) requiredChecksHandler {
				return writeJSON([]map[string]any{statusChecksRule(map[string]any{"context": "d", "integration_id": nil})})
			},
			want: []ports.RequiredCheck{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}},
		},
		{
			name:   "a protection with no status-check requirement declares none",
			branch: writeJSON(map[string]any{"protection": map[string]any{"enabled": true}}),
			rules:  func(int) requiredChecksHandler { return writeJSON([]map[string]any{}) },
			want:   nil,
		},
		{
			name:   "rulesets unavailable on the plan declare nothing",
			branch: writeJSON(protectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeStatus(http.StatusForbidden, planUnavailableBody, nil)
			},
			want: []ports.RequiredCheck{{Name: "ci/build", AppID: 15368}, {Name: "lint"}, {Name: "deploy/preview"}},
		},
		{
			name:   "branch protection unavailable on the plan declares nothing",
			branch: writeStatus(http.StatusForbidden, planUnavailableBody, nil),
			rules: func(int) requiredChecksHandler {
				return writeJSON([]map[string]any{statusChecksRule(map[string]any{"context": "e2e"})})
			},
			want: []ports.RequiredCheck{{Name: "e2e"}},
		},
		{
			name:   "a rulesets read that fails is an error",
			branch: writeJSON(protectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeStatus(http.StatusInternalServerError, `{"message":"boom"}`, nil)
			},
			wantErr: true,
		},
		{
			name:   "a 403 that is not the plan's answer is an error",
			branch: writeJSON(protectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeStatus(http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, nil)
			},
			wantErr: true,
		},
		{
			name:   "a rate-limited 403 is an error whatever its message says",
			branch: writeJSON(protectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeStatus(http.StatusForbidden, planUnavailableBody, map[string]string{"X-RateLimit-Remaining": "0"})
			},
			wantErr: true,
		},
		{
			name:    "a branch that cannot be found is an error",
			branch:  writeStatus(http.StatusNotFound, `{"message":"Branch not found"}`, nil),
			rules:   func(int) requiredChecksHandler { return writeJSON([]map[string]any{}) },
			wantErr: true,
		},
		{
			name:    "a branch object with no protection object is an error",
			branch:  writeJSON(map[string]any{"name": "main"}),
			rules:   func(int) requiredChecksHandler { return writeJSON([]map[string]any{}) },
			wantErr: true,
		},
		{
			name:    "a branch object that does not decode is an error",
			branch:  writeStatus(http.StatusOK, "not json", nil),
			rules:   func(int) requiredChecksHandler { return writeJSON([]map[string]any{}) },
			wantErr: true,
		},
		{
			name:    "rules that do not decode are an error",
			branch:  writeJSON(unprotectedBranch()),
			rules:   func(int) requiredChecksHandler { return writeStatus(http.StatusOK, `{"not":"a list"}`, nil) },
			wantErr: true,
		},
		{
			name:   "a required_status_checks rule with no parameters is an error",
			branch: writeJSON(unprotectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeJSON([]map[string]any{{"type": "required_status_checks"}})
			},
			wantErr: true,
		},
		{
			name:   "a full page of rules is followed to the next",
			branch: writeJSON(unprotectedBranch()),
			rules: func(page int) requiredChecksHandler {
				if page == 1 {
					return writeJSON(fullPageOfRules())
				}
				return writeJSON([]map[string]any{statusChecksRule(map[string]any{"context": "late"})})
			},
			want: []ports.RequiredCheck{{Name: "late"}},
		},
		{
			name:   "more pages of rules than the bound is an error, never the prefix read",
			branch: writeJSON(unprotectedBranch()),
			rules: func(int) requiredChecksHandler {
				return writeJSON(fullPageOfRules())
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var tokens []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				tokens = append(tokens, r.Header.Get("Authorization"))
				mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.EscapedPath() == "/repos/acme/widgets/branches/release%2F2026.09":
					tc.branch(w, r)
				case r.Method == http.MethodGet && r.URL.EscapedPath() == "/repos/acme/widgets/rules/branches/release%2F2026.09":
					if got := r.URL.Query().Get("per_page"); got != "100" {
						t.Errorf("rules per_page = %q, want 100", got)
					}
					page, err := strconv.Atoi(r.URL.Query().Get("page"))
					if err != nil {
						t.Errorf("rules page = %q, want a number", r.URL.Query().Get("page"))
					}
					tc.rules(page)(w, r)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)
			got, err := adapter.ListRequiredChecks(context.Background(), ports.ListRequiredChecksSpec{Owner: "acme", Repo: "widgets", Branch: "release/2026.09", Token: "bot-token"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ListRequiredChecks() error = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ListRequiredChecks() = %+v, want %+v", got, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, token := range tokens {
				if token != "Bearer bot-token" {
					t.Errorf("request Authorization = %q, want the spec's token", token)
				}
			}
		})
	}
}

// fullPageOfRules is a page of 100 rules, none of them a status-check rule.
func fullPageOfRules() []map[string]any {
	rules := make([]map[string]any, 100)
	for i := range rules {
		rules[i] = map[string]any{"type": "non_fast_forward"}
	}
	return rules
}

// TestGetOpenPR_ListsHeadChecks proves the live CI read lists the head's
// checks one by one for a base branch's required checks (ports.OpenPR.
// HeadChecks): every check run with its App id and every commit status
// context, each mapped to passed, pending or failed, from the SAME two
// responses the CI conclusion comes from -- and that listing them never
// changes that conclusion.
func TestGetOpenPR_ListsHeadChecks(t *testing.T) {
	t.Parallel()

	conclusion := func(c string) any { return c }
	tests := []struct {
		name             string
		status           requiredChecksHandler
		checkRuns        requiredChecksHandler
		wantChecks       []ports.HeadCheck
		wantListDegraded bool
		wantCI           ports.CIConclusion
		wantCIDegraded   bool
	}{
		{
			name: "check runs and statuses are listed with their App and state",
			status: writeJSON(map[string]any{"state": "failure", "total_count": 4, "statuses": []map[string]any{
				{"context": "deploy/preview", "state": "success"},
				{"context": "legacy/ci", "state": "pending"},
				{"context": "legacy/lint", "state": "failure"},
				{"context": "legacy/odd", "state": "error"},
			}}),
			checkRuns: writeJSON(map[string]any{"total_count": 10, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368}},
				{"name": "docs", "conclusion": conclusion("neutral"), "app": map[string]any{"id": 15368}},
				{"name": "optional", "conclusion": conclusion("skipped"), "app": map[string]any{"id": 15368}},
				{"name": "test", "conclusion": nil, "app": map[string]any{"id": 15368}},
				{"name": "flaky", "conclusion": conclusion("cancelled"), "app": map[string]any{"id": 15368}},
				{"name": "slow", "conclusion": conclusion("timed_out"), "app": map[string]any{"id": 15368}},
				{"name": "gate", "conclusion": conclusion("action_required"), "app": map[string]any{"id": 99}},
				{"name": "old", "conclusion": conclusion("stale"), "app": map[string]any{"id": 99}},
				{"name": "mystery", "conclusion": conclusion("something-new"), "app": map[string]any{"id": 99}},
				{"name": "narvi/review", "conclusion": conclusion("success"), "app": map[string]any{"id": 7}},
			}}),
			wantChecks: []ports.HeadCheck{
				{Name: "deploy/preview", Source: ports.HeadCheckSourceStatus, State: ports.HeadCheckStatePassed},
				{Name: "legacy/ci", Source: ports.HeadCheckSourceStatus, State: ports.HeadCheckStatePending},
				{Name: "legacy/lint", Source: ports.HeadCheckSourceStatus, State: ports.HeadCheckStateFailed},
				{Name: "legacy/odd", Source: ports.HeadCheckSourceStatus, State: ports.HeadCheckStateFailed},
				{Name: "build", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePassed},
				{Name: "docs", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePassed},
				{Name: "optional", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePassed},
				{Name: "test", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePending},
				{Name: "flaky", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStateFailed},
				{Name: "slow", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStateFailed},
				{Name: "gate", Source: ports.HeadCheckSourceCheckRun, AppID: 99, State: ports.HeadCheckStateFailed},
				{Name: "old", Source: ports.HeadCheckSourceCheckRun, AppID: 99, State: ports.HeadCheckStateFailed},
				{Name: "mystery", Source: ports.HeadCheckSourceCheckRun, AppID: 99, State: ports.HeadCheckStateFailed},
				{Name: "narvi/review", Source: ports.HeadCheckSourceCheckRun, AppID: 7, State: ports.HeadCheckStatePassed},
			},
			wantCI: ports.CIConclusionFailure,
		},
		{
			// The combined state covers every status, so the conclusion
			// stays green and undegraded; only the per-context listing,
			// which a required check's presence needs, is short.
			name: "statuses beyond the page served degrade the listing, not the conclusion",
			status: writeJSON(map[string]any{"state": "success", "total_count": 150, "statuses": []map[string]any{
				{"context": "deploy/preview", "state": "success"},
			}}),
			checkRuns: writeJSON(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368}},
			}}),
			wantChecks: []ports.HeadCheck{
				{Name: "deploy/preview", Source: ports.HeadCheckSourceStatus, State: ports.HeadCheckStatePassed},
				{Name: "build", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePassed},
			},
			wantListDegraded: true,
			wantCI:           ports.CIConclusionSuccess,
		},
		{
			name:   "a failed status read degrades both",
			status: writeStatus(http.StatusInternalServerError, `{"message":"boom"}`, nil),
			checkRuns: writeJSON(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368}},
			}}),
			wantChecks: []ports.HeadCheck{
				{Name: "build", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePassed},
			},
			wantListDegraded: true,
			wantCI:           ports.CIConclusionUnknown,
			wantCIDegraded:   true,
		},
		{
			name:   "a truncated check-run page degrades both",
			status: writeJSON(map[string]any{"state": "pending", "total_count": 0, "statuses": []map[string]any{}}),
			checkRuns: writeJSON(map[string]any{"total_count": 140, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368}},
			}}),
			wantChecks: []ports.HeadCheck{
				{Name: "build", Source: ports.HeadCheckSourceCheckRun, AppID: 15368, State: ports.HeadCheckStatePassed},
			},
			wantListDegraded: true,
			wantCI:           ports.CIConclusionUnknown,
			wantCIDegraded:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/widgets/pulls/9":
					writeJSON(map[string]any{"number": 9, "state": "open", "head": map[string]any{"sha": "h9"}, "base": map[string]any{"ref": "main"}})(w, r)
				case "/repos/acme/widgets/pulls/9/reviews":
					writeJSON([]map[string]any{})(w, r)
				case "/repos/acme/widgets/pulls/9/files":
					writeJSON([]map[string]any{})(w, r)
				case "/repos/acme/widgets/commits/h9/status":
					if got := r.URL.Query().Get("per_page"); got != "100" {
						t.Errorf("status per_page = %q, want 100", got)
					}
					tc.status(w, r)
				case "/repos/acme/widgets/commits/h9/check-runs":
					tc.checkRuns(w, r)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			pr, found, err := githubapi.New(server.Client(), server.URL).GetOpenPR(context.Background(), "acme", "widgets", 9, "tok")
			if err != nil || !found {
				t.Fatalf("GetOpenPR() = (found %v, err %v), want found and no error", found, err)
			}
			if !reflect.DeepEqual(pr.HeadChecks, tc.wantChecks) {
				t.Errorf("HeadChecks = %+v, want %+v", pr.HeadChecks, tc.wantChecks)
			}
			if pr.HeadChecksListDegraded != tc.wantListDegraded {
				t.Errorf("HeadChecksListDegraded = %v, want %v", pr.HeadChecksListDegraded, tc.wantListDegraded)
			}
			if pr.CIConclusion != tc.wantCI || pr.CIConclusionDegraded != tc.wantCIDegraded {
				t.Errorf("CI = (%v, degraded %v), want (%v, degraded %v)", pr.CIConclusion, pr.CIConclusionDegraded, tc.wantCI, tc.wantCIDegraded)
			}
		})
	}
}

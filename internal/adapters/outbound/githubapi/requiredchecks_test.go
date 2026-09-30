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
		{
			name:   "four full pages and a partial fifth are read in full",
			branch: writeJSON(unprotectedBranch()),
			rules:  pagesOfRules(t, 4, []map[string]any{statusChecksRule(map[string]any{"context": "on-page-5"})}),
			want:   []ports.RequiredCheck{{Name: "on-page-1"}, {Name: "on-page-2"}, {Name: "on-page-3"}, {Name: "on-page-4"}, {Name: "on-page-5"}},
		},
		{
			name:   "exactly five full pages, then an empty sixth, are read in full",
			branch: writeJSON(unprotectedBranch()),
			rules:  pagesOfRules(t, 5, nil),
			want:   []ports.RequiredCheck{{Name: "on-page-1"}, {Name: "on-page-2"}, {Name: "on-page-3"}, {Name: "on-page-4"}, {Name: "on-page-5"}},
		},
		{
			name:    "five full pages and rules on a sixth is an error",
			branch:  writeJSON(unprotectedBranch()),
			rules:   pagesOfRules(t, 5, []map[string]any{statusChecksRule(map[string]any{"context": "on-page-6"})}),
			wantErr: true,
		},
		{
			name:   "the plan's answer on a later page is an error, never a branch requiring nothing",
			branch: writeJSON(unprotectedBranch()),
			rules: func(page int) requiredChecksHandler {
				if page == 1 {
					return writeJSON(fullPageOfRules(map[string]any{"context": "e2e"}))
				}
				return writeStatus(http.StatusForbidden, planUnavailableBody, nil)
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

// fullPageOfRules is a page of 100 rules, none of them a status-check rule
// unless checks are given, in which case the last rule requires them.
func fullPageOfRules(checks ...map[string]any) []map[string]any {
	rules := make([]map[string]any, 100)
	for i := range rules {
		rules[i] = map[string]any{"type": "non_fast_forward"}
	}
	if len(checks) > 0 {
		rules[99] = statusChecksRule(checks...)
	}
	return rules
}

// pagesOfRules answers pages 1..full with a full page, the page after with
// last (nil for an empty page), and any later page with a failure: a read
// that goes further than it needs to must not pass.
func pagesOfRules(t *testing.T, full int, last []map[string]any) func(page int) requiredChecksHandler {
	return func(page int) requiredChecksHandler {
		switch {
		case page <= full:
			return writeJSON(fullPageOfRules(map[string]any{"context": "on-page-" + strconv.Itoa(page)}))
		case page == full+1:
			if last == nil {
				last = []map[string]any{}
			}
			return writeJSON(last)
		default:
			t.Errorf("rules page %d was requested, past the last page %d", page, full+1)
			return writeStatus(http.StatusInternalServerError, `{"message":"too far"}`, nil)
		}
	}
}

// TestGetOpenPR_ListsHeadChecks proves the live CI read lists the head's
// checks one by one for a base branch's required checks (ports.OpenPR.
// HeadChecks): every check run with its App id and every commit status
// context, each mapped to passed, pending or failed, from the SAME
// responses the CI conclusion comes from -- and that listing them never
// changes that conclusion. When the head carries statuses, the per-ref
// statuses listing says who posted each: an App's bot account ("<slug>[bot]",
// type Bot), attributed to that App's id when a check run of the same slug
// is at the head and to no id otherwise; a person (type User); or nobody
// known.
func TestGetOpenPR_ListsHeadChecks(t *testing.T) {
	t.Parallel()

	conclusion := func(c string) any { return c }
	bot := func(login string) map[string]any { return map[string]any{"login": login, "type": "Bot"} }
	user := func(login string) map[string]any { return map[string]any{"login": login, "type": "User"} }
	status := func(name string, poster ports.HeadCheckPoster, appID int64, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceStatus, AppID: appID, Poster: poster, State: state}
	}
	run := func(name string, appID int64, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceCheckRun, AppID: appID, Poster: ports.HeadCheckPosterApp, State: state}
	}
	const passed, pending, failed = ports.HeadCheckStatePassed, ports.HeadCheckStatePending, ports.HeadCheckStateFailed
	const app, person, unknown = ports.HeadCheckPosterApp, ports.HeadCheckPosterPerson, ports.HeadCheckPosterUnknown

	tests := []struct {
		name      string
		status    requiredChecksHandler
		checkRuns requiredChecksHandler
		// statusList answers the per-ref statuses listing; nil means the
		// listing must not be requested at all.
		statusList       requiredChecksHandler
		wantChecks       []ports.HeadCheck
		wantListDegraded bool
		wantCI           ports.CIConclusion
		wantCIDegraded   bool
	}{
		{
			name: "check runs and statuses are listed with their App, poster and state",
			status: writeJSON(map[string]any{"state": "failure", "total_count": 5, "statuses": []map[string]any{
				{"context": "deploy/preview", "state": "success"},
				{"context": "legacy/ci", "state": "pending"},
				{"context": "legacy/lint", "state": "failure"},
				{"context": "legacy/odd", "state": "error"},
				{"context": "codecov/patch", "state": "success"},
			}}),
			checkRuns: writeJSON(map[string]any{"total_count": 10, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
				{"name": "docs", "conclusion": conclusion("neutral"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
				{"name": "optional", "conclusion": conclusion("skipped"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
				{"name": "test", "conclusion": nil, "app": map[string]any{"id": 15368, "slug": "github-actions"}},
				{"name": "flaky", "conclusion": conclusion("cancelled"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
				{"name": "slow", "conclusion": conclusion("timed_out"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
				{"name": "gate", "conclusion": conclusion("action_required"), "app": map[string]any{"id": 99}},
				{"name": "old", "conclusion": conclusion("stale"), "app": map[string]any{"id": 99}},
				{"name": "mystery", "conclusion": conclusion("something-new"), "app": map[string]any{"id": 99}},
				{"name": "narvi/review", "conclusion": conclusion("success"), "app": map[string]any{"id": 7}},
			}}),
			// Newest first: legacy/ci's newest status is a person's, an
			// older one a bot's; the newest decides.
			statusList: writeJSON([]map[string]any{
				{"context": "deploy/preview", "creator": bot("previews[bot]")},
				{"context": "legacy/ci", "creator": user("octocat")},
				{"context": "legacy/lint", "creator": bot("github-actions[bot]")},
				{"context": "legacy/ci", "creator": bot("github-actions[bot]")},
				{"context": "codecov/patch", "creator": bot("codecov[bot]")},
			}),
			wantChecks: []ports.HeadCheck{
				status("deploy/preview", app, 0, passed),
				status("legacy/ci", person, 0, pending),
				status("legacy/lint", app, 15368, failed),
				status("legacy/odd", unknown, 0, failed),
				status("codecov/patch", app, 0, passed),
				run("build", 15368, passed),
				run("docs", 15368, passed),
				run("optional", 15368, passed),
				run("test", 15368, pending),
				run("flaky", 15368, failed),
				run("slow", 15368, failed),
				run("gate", 99, failed),
				run("old", 99, failed),
				run("mystery", 99, failed),
				run("narvi/review", 7, passed),
			},
			wantCI: ports.CIConclusionFailure,
		},
		{
			name: "a status listing that fails leaves every poster unknown, and the conclusion untouched",
			status: writeJSON(map[string]any{"state": "success", "total_count": 1, "statuses": []map[string]any{
				{"context": "codecov/patch", "state": "success"},
			}}),
			checkRuns: writeJSON(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
			}}),
			statusList: writeStatus(http.StatusInternalServerError, `{"message":"boom"}`, nil),
			wantChecks: []ports.HeadCheck{status("codecov/patch", unknown, 0, passed), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name: "a status with no creator, or no account type, has an unknown poster",
			status: writeJSON(map[string]any{"state": "success", "total_count": 2, "statuses": []map[string]any{
				{"context": "a", "state": "success"},
				{"context": "b", "state": "success"},
			}}),
			checkRuns:  writeJSON(map[string]any{"total_count": 0, "check_runs": []map[string]any{}}),
			statusList: writeJSON([]map[string]any{{"context": "a"}, {"context": "b", "creator": map[string]any{"login": "someone"}}}),
			wantChecks: []ports.HeadCheck{status("a", unknown, 0, passed), status("b", unknown, 0, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name:   "a head with no commit statuses never requests the listing",
			status: writeJSON(map[string]any{"state": "pending", "total_count": 0, "statuses": []map[string]any{}}),
			checkRuns: writeJSON(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
			}}),
			wantChecks: []ports.HeadCheck{run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
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
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
			}}),
			statusList:       writeJSON([]map[string]any{{"context": "deploy/preview", "creator": user("octocat")}}),
			wantChecks:       []ports.HeadCheck{status("deploy/preview", person, 0, passed), run("build", 15368, passed)},
			wantListDegraded: true,
			wantCI:           ports.CIConclusionSuccess,
		},
		{
			name:   "a failed status read degrades both",
			status: writeStatus(http.StatusInternalServerError, `{"message":"boom"}`, nil),
			checkRuns: writeJSON(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368}},
			}}),
			wantChecks:       []ports.HeadCheck{run("build", 15368, passed)},
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
			wantChecks:       []ports.HeadCheck{run("build", 15368, passed)},
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
				case "/repos/acme/widgets/commits/h9/statuses":
					if tc.statusList == nil {
						t.Errorf("the statuses listing was requested for a head with no commit status")
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if got := r.URL.Query().Get("per_page"); got != "100" {
						t.Errorf("statuses listing per_page = %q, want 100", got)
					}
					tc.statusList(w, r)
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

// TestGetOpenPR_NoHeadSHA_ListingIncomplete pins that a pull request whose
// head could not be read has its check listing marked incomplete, never
// read as "no checks at the head": nothing was read.
func TestGetOpenPR_NoHeadSHA_ListingIncomplete(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/9":
			writeJSON(map[string]any{"number": 9, "state": "open", "head": map[string]any{"sha": ""}, "base": map[string]any{"ref": "main"}})(w, r)
		case "/repos/acme/widgets/pulls/9/reviews", "/repos/acme/widgets/pulls/9/files":
			writeJSON([]map[string]any{})(w, r)
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
	if !pr.HeadChecksListDegraded || len(pr.HeadChecks) != 0 || pr.CIConclusion != ports.CIConclusionUnknown {
		t.Errorf("GetOpenPR() = (HeadChecks %+v, listing degraded %v, CI %v), want no checks, a degraded listing and CI unknown", pr.HeadChecks, pr.HeadChecksListDegraded, pr.CIConclusion)
	}
}

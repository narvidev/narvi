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
// statuses listing says who posted each, matched by status id: an App's
// bot account ("<slug>[bot]", type Bot), with its slug, and its App's id
// when a check run of the same slug is at the head; a person (type User);
// or nobody known. The listing is paged, to a bound.
func TestGetOpenPR_ListsHeadChecks(t *testing.T) {
	t.Parallel()

	conclusion := func(c string) any { return c }
	bot := func(login string) map[string]any { return map[string]any{"login": login, "type": "Bot"} }
	user := func(login string) map[string]any { return map[string]any{"login": login, "type": "User"} }
	entry := func(id int, context string, creator map[string]any) map[string]any {
		e := map[string]any{"id": id, "context": context}
		if creator != nil {
			e["creator"] = creator
		} else {
			e["creator"] = nil
		}
		return e
	}
	status := func(name string, poster ports.HeadCheckPoster, appID int64, slug string, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceStatus, AppID: appID, AppSlug: slug, Poster: poster, State: state}
	}
	overOthers := func(h ports.HeadCheck) ports.HeadCheck {
		h.EarlierFromOthers = true
		return h
	}
	run := func(name string, appID int64, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceCheckRun, AppID: appID, Poster: ports.HeadCheckPosterApp, State: state}
	}
	const passed, pending, failed = ports.HeadCheckStatePassed, ports.HeadCheckStatePending, ports.HeadCheckStateFailed
	const app, person, unknown = ports.HeadCheckPosterApp, ports.HeadCheckPosterPerson, ports.HeadCheckPosterUnknown
	oneBuildRun := writeJSON(map[string]any{"total_count": 1, "check_runs": []map[string]any{
		{"name": "build", "conclusion": conclusion("success"), "app": map[string]any{"id": 15368, "slug": "github-actions"}},
	}})
	combinedBuild := func(id int) requiredChecksHandler {
		return writeJSON(map[string]any{"state": "success", "total_count": 1, "statuses": []map[string]any{
			{"id": id, "context": "ci/build", "state": "success"},
		}})
	}
	// fullPageOfStatuses is a page of 100 listing entries of another
	// context, posted by another App.
	fullPageOfStatuses := func(firstID int) []map[string]any {
		page := make([]map[string]any, 100)
		for i := range page {
			page[i] = entry(firstID-i, "other", bot("other-ci[bot]"))
		}
		return page
	}
	// statusesOnPage puts ci/build's status (id 1, from real-ci) on page
	// `on` of the listing: every earlier page is full of other entries.
	statusesOnPage := func(on int) func(page int) requiredChecksHandler {
		return func(page int) requiredChecksHandler {
			switch {
			case page < on:
				return writeJSON(fullPageOfStatuses(100000 - page*100))
			case page == on:
				return writeJSON([]map[string]any{entry(1, "ci/build", bot("real-ci[bot]"))})
			default:
				return writeJSON([]map[string]any{})
			}
		}
	}
	singlePage := func(entries ...map[string]any) func(page int) requiredChecksHandler {
		return func(page int) requiredChecksHandler {
			if page == 1 {
				return writeJSON(entries)
			}
			return writeJSON([]map[string]any{})
		}
	}

	tests := []struct {
		name      string
		status    requiredChecksHandler
		checkRuns requiredChecksHandler
		// statusList answers each page of the per-ref statuses listing;
		// nil means the listing must not be requested at all.
		statusList func(page int) requiredChecksHandler
		// wantStatusPages is the listing pages requested, in order; nil
		// means page 1 alone whenever statusList is set.
		wantStatusPages  []int
		wantChecks       []ports.HeadCheck
		wantListDegraded bool
		wantCI           ports.CIConclusion
		wantCIDegraded   bool
	}{
		{
			name: "check runs and statuses are listed with their App, poster and state",
			status: writeJSON(map[string]any{"state": "failure", "total_count": 5, "statuses": []map[string]any{
				{"id": 50, "context": "deploy/preview", "state": "success"},
				{"id": 40, "context": "legacy/ci", "state": "pending"},
				{"id": 30, "context": "legacy/lint", "state": "failure"},
				{"id": 20, "context": "legacy/odd", "state": "error"},
				{"id": 10, "context": "coverage/patch", "state": "success"},
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
			// Newest first. legacy/ci's rolled-up status (id 40) is a
			// person's, over an older one of a bot's; legacy/odd's (id 20)
			// is not in the listing at all.
			statusList: singlePage(
				entry(50, "deploy/preview", bot("previews[bot]")),
				entry(40, "legacy/ci", user("octocat")),
				entry(30, "legacy/lint", bot("github-actions[bot]")),
				entry(35, "legacy/ci", bot("github-actions[bot]")),
				entry(10, "coverage/patch", bot("coverage[bot]")),
			),
			wantChecks: []ports.HeadCheck{
				status("deploy/preview", app, 0, "previews", passed),
				overOthers(status("legacy/ci", person, 0, "", pending)),
				status("legacy/lint", app, 15368, "github-actions", failed),
				status("legacy/odd", unknown, 0, "", failed),
				status("coverage/patch", app, 0, "coverage", passed),
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
			name:      "a status listing that fails leaves every poster unknown, and the conclusion untouched",
			status:    combinedBuild(1),
			checkRuns: oneBuildRun,
			statusList: func(int) requiredChecksHandler {
				return writeStatus(http.StatusInternalServerError, `{"message":"boom"}`, nil)
			},
			wantChecks: []ports.HeadCheck{status("ci/build", unknown, 0, "", passed), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name: "a status with no creator, or no account type, has an unknown poster",
			status: writeJSON(map[string]any{"state": "success", "total_count": 2, "statuses": []map[string]any{
				{"id": 2, "context": "a", "state": "success"},
				{"id": 1, "context": "b", "state": "success"},
			}}),
			checkRuns:  writeJSON(map[string]any{"total_count": 0, "check_runs": []map[string]any{}}),
			statusList: singlePage(entry(2, "a", nil), entry(1, "b", map[string]any{"login": "someone"})),
			wantChecks: []ports.HeadCheck{status("a", unknown, 0, "", passed), status("b", unknown, 0, "", passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			// The rolled-up status is matched by id: a newer entry of the
			// same context with no creator, and an older one with an App's,
			// do not lend that App's account to it.
			name:       "a rolled-up status with no creator stays unknown beside an older status of the same context from an App",
			status:     combinedBuild(2),
			checkRuns:  oneBuildRun,
			statusList: singlePage(entry(2, "ci/build", nil), entry(1, "ci/build", bot("real-ci[bot]"))),
			wantChecks: []ports.HeadCheck{overOthers(status("ci/build", unknown, 0, "", passed)), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			// A status posted between the combined read and the listing's
			// is newer than the one whose state was read, and says nothing
			// about who posted it.
			name:       "a status posted after the combined read does not lend its poster to the one read",
			status:     combinedBuild(1),
			checkRuns:  oneBuildRun,
			statusList: singlePage(entry(2, "ci/build", bot("real-ci[bot]")), entry(1, "ci/build", user("octocat"))),
			wantChecks: []ports.HeadCheck{status("ci/build", person, 0, "", passed), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name:       "an entry of the same context but another id is never taken for the rolled-up status",
			status:     combinedBuild(7),
			checkRuns:  oneBuildRun,
			statusList: singlePage(entry(8, "ci/build", bot("real-ci[bot]")), entry(6, "ci/build", bot("real-ci[bot]"))),
			wantChecks: []ports.HeadCheck{status("ci/build", unknown, 0, "", passed), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			// The masking case: another App's status is the latest, over
			// one of the App the base may name.
			name:       "a status posted over another account's earlier status of the same context says so",
			status:     combinedBuild(5),
			checkRuns:  oneBuildRun,
			statusList: singlePage(entry(5, "ci/build", bot("other-ci[bot]")), entry(3, "ci/build", bot("real-ci[bot]"))),
			wantChecks: []ports.HeadCheck{overOthers(status("ci/build", app, 0, "other-ci", passed)), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name:       "earlier statuses of the same context from the same account are no other source",
			status:     combinedBuild(5),
			checkRuns:  oneBuildRun,
			statusList: singlePage(entry(5, "ci/build", bot("real-ci[bot]")), entry(3, "ci/build", bot("real-ci[bot]"))),
			wantChecks: []ports.HeadCheck{status("ci/build", app, 0, "real-ci", passed), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name:       "a bot account whose login carries no App slug is an App that cannot be identified",
			status:     combinedBuild(1),
			checkRuns:  oneBuildRun,
			statusList: singlePage(entry(1, "ci/build", bot("github-actions"))),
			wantChecks: []ports.HeadCheck{status("ci/build", app, 0, "", passed), run("build", 15368, passed)},
			wantCI:     ports.CIConclusionSuccess,
		},
		{
			name:            "a rolled-up status on the last page within the bound is found there",
			status:          combinedBuild(1),
			checkRuns:       oneBuildRun,
			statusList:      statusesOnPage(5),
			wantStatusPages: []int{1, 2, 3, 4, 5},
			wantChecks:      []ports.HeadCheck{status("ci/build", app, 0, "real-ci", passed), run("build", 15368, passed)},
			wantCI:          ports.CIConclusionSuccess,
		},
		{
			// Past the bound the poster is unknown -- never a guess -- and
			// the page past it is never asked for.
			name:            "a rolled-up status past the bound has an unknown poster",
			status:          combinedBuild(1),
			checkRuns:       oneBuildRun,
			statusList:      statusesOnPage(6),
			wantStatusPages: []int{1, 2, 3, 4, 5},
			wantChecks:      []ports.HeadCheck{status("ci/build", unknown, 0, "", passed), run("build", 15368, passed)},
			wantCI:          ports.CIConclusionSuccess,
		},
		{
			// Every rolled-up status found on a full first page: the read
			// stops there, and what the unread pages hold is unknown.
			name:      "a listing not read to its end marks every status found as possibly over another account's",
			status:    combinedBuild(100000),
			checkRuns: oneBuildRun,
			statusList: func(int) requiredChecksHandler {
				p := fullPageOfStatuses(99999)
				p[0] = entry(100000, "ci/build", bot("real-ci[bot]"))
				return writeJSON(p)
			},
			wantChecks: []ports.HeadCheck{overOthers(status("ci/build", app, 0, "real-ci", passed)), run("build", 15368, passed)},
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
				{"id": 1, "context": "deploy/preview", "state": "success"},
			}}),
			checkRuns:        oneBuildRun,
			statusList:       singlePage(entry(1, "deploy/preview", user("octocat"))),
			wantChecks:       []ports.HeadCheck{status("deploy/preview", person, 0, "", passed), run("build", 15368, passed)},
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

			var mu sync.Mutex
			var statusPages []int
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
					page, err := strconv.Atoi(r.URL.Query().Get("page"))
					if err != nil {
						t.Errorf("statuses listing page = %q, want a number", r.URL.Query().Get("page"))
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mu.Lock()
					statusPages = append(statusPages, page)
					mu.Unlock()
					tc.statusList(page)(w, r)
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
			wantPages := tc.wantStatusPages
			if wantPages == nil && tc.statusList != nil {
				wantPages = []int{1}
			}
			mu.Lock()
			gotPages := statusPages
			mu.Unlock()
			if !reflect.DeepEqual(gotPages, wantPages) {
				t.Errorf("statuses listing pages requested = %v, want %v", gotPages, wantPages)
			}
		})
	}
}

// TestResolveAppID proves ResolveAppID reads an App's id from GET
// /apps/{app_slug} with the caller's token, keeps a successful answer (an
// App's id never changes), and keeps no failure: a slug naming no App, a
// failed read, an undecodable body and an answer with no id are errors,
// and the next call reads again.
func TestResolveAppID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		answer  requiredChecksHandler
		wantID  int64
		wantErr bool
		// wantRequests is how many requests two calls make.
		wantRequests int
	}{
		{name: "an App's id is read once and kept", answer: writeJSON(map[string]any{"id": 4242, "slug": "real-ci", "name": "Real CI"}), wantID: 4242, wantRequests: 1},
		{name: "a slug naming no App is an error, and is read again", answer: writeStatus(http.StatusNotFound, `{"message":"Not Found"}`, nil), wantErr: true, wantRequests: 2},
		{name: "a failed read is an error, and is read again", answer: writeStatus(http.StatusBadGateway, `{"message":"bad gateway"}`, nil), wantErr: true, wantRequests: 2},
		{name: "an undecodable answer is an error", answer: writeStatus(http.StatusOK, `{"id":`, nil), wantErr: true, wantRequests: 2},
		{name: "an answer with no id is an error", answer: writeJSON(map[string]any{"slug": "real-ci"}), wantErr: true, wantRequests: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apps/real-ci" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if got := r.Header.Get("Authorization"); got != "Bearer tok" {
					t.Errorf("Authorization = %q, want the caller's token", got)
				}
				mu.Lock()
				requests++
				mu.Unlock()
				tc.answer(w, r)
			}))
			defer server.Close()

			adapter := githubapi.New(server.Client(), server.URL)
			for call := 1; call <= 2; call++ {
				id, err := adapter.ResolveAppID(context.Background(), ports.ResolveAppIDSpec{Slug: "real-ci", Token: "tok"})
				if (err != nil) != tc.wantErr || id != tc.wantID {
					t.Errorf("call %d: ResolveAppID() = (%d, %v), want (%d, error %v)", call, id, err, tc.wantID, tc.wantErr)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != tc.wantRequests {
				t.Errorf("requests = %d, want %d", requests, tc.wantRequests)
			}
		})
	}
}

// TestResolveAppID_EmptySlug pins that an empty slug is an error, with no
// request made.
func TestResolveAppID_EmptySlug(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if id, err := githubapi.New(server.Client(), server.URL).ResolveAppID(context.Background(), ports.ResolveAppIDSpec{Token: "tok"}); err == nil || id != 0 {
		t.Errorf("ResolveAppID(empty slug) = (%d, %v), want an error", id, err)
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

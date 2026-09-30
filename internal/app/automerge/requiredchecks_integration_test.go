//go:build integration

package automerge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
)

// requiredChecksGitHub is an httptest GitHub for one pull request: every
// read GetOpenPR, the base-freshness check and ListRequiredChecks make,
// and the merge -- so the worker runs through the real githubapi adapter,
// the real SourceControl port and the real eligibility engine, with only
// the network faked.
type requiredChecksGitHub struct {
	repo   string
	number int
	head   string

	statuses  []map[string]any
	checkRuns []map[string]any
	// branch answers GET /branches/main; rules answers GET
	// /rules/branches/main (status and body).
	branch      map[string]any
	rulesStatus int
	rulesBody   string

	mu          sync.Mutex
	merged      bool
	botRequests int
	requests    []string
}

func (g *requiredChecksGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") == "Bearer bot-token" {
		g.botRequests++
	}
	g.mu.Unlock()

	prefix := "/repos/acme/" + g.repo
	pr := prefix + "/pulls/" + strconv.Itoa(g.number)
	w.Header().Set("Content-Type", "application/json")
	encode := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == pr:
		encode(map[string]any{
			"number": g.number, "title": "required checks", "state": "open", "changed_files": 1,
			"html_url": "https://github.com/acme/" + g.repo + "/pull/" + strconv.Itoa(g.number),
			"user":     map[string]any{"id": 500, "login": "narvi-bot"},
			"head":     map[string]any{"sha": g.head},
			"base":     map[string]any{"ref": testEligibleBaseRef, "sha": testEligibleBaseSHA},
		})
	case r.Method == http.MethodGet && r.URL.Path == pr+"/reviews":
		encode([]map[string]any{})
	case r.Method == http.MethodGet && r.URL.Path == pr+"/files":
		encode([]map[string]any{{"filename": "internal/app/widget/retry.go"}})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/commits/"+g.head+"/status":
		state := "success"
		for _, s := range g.statuses {
			if s["state"] != "success" {
				state = s["state"].(string)
			}
		}
		if len(g.statuses) == 0 {
			state = "pending"
		}
		encode(map[string]any{"state": state, "total_count": len(g.statuses), "statuses": g.statuses})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/commits/"+g.head+"/check-runs":
		encode(map[string]any{"total_count": len(g.checkRuns), "check_runs": g.checkRuns})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/commits/"+testEligibleBaseRef:
		encode(map[string]any{"sha": testEligibleBaseSHA})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/branches/"+testEligibleBaseRef:
		encode(g.branch)
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/rules/branches/"+testEligibleBaseRef:
		w.WriteHeader(g.rulesStatus)
		_, _ = w.Write([]byte(g.rulesBody))
	case r.Method == http.MethodPut && r.URL.Path == pr+"/merge":
		g.mu.Lock()
		g.merged = true
		g.mu.Unlock()
		encode(map[string]any{"sha": "merge-commit", "merged": true})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

// protection is a branch object whose protection requires checks.
func protection(checks ...map[string]any) map[string]any {
	contexts := make([]string, 0, len(checks))
	for _, c := range checks {
		contexts = append(contexts, c["context"].(string))
	}
	return map[string]any{"name": testEligibleBaseRef, "protected": len(checks) > 0, "protection": map[string]any{
		"enabled":                len(checks) > 0,
		"required_status_checks": map[string]any{"contexts": contexts, "checks": checks},
	}}
}

func rulesJSON(checks ...map[string]any) string {
	rules := []map[string]any{}
	if len(checks) > 0 {
		rules = append(rules, map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": checks}})
	}
	body, _ := json.Marshal(rules)
	return string(body)
}

func checkRunJSON(name string, appID int64, conclusion any) map[string]any {
	return map[string]any{"name": name, "conclusion": conclusion, "app": map[string]any{"id": appID}}
}

// TestPumpOnce_RequiredChecks_EndToEnd runs §21.2's "CI green means the
// required checks, not the checks that reported" through the unattended
// merge path end to end: the auto-merge worker on real Postgres, the real
// GitHub adapter against an httptest GitHub, and each scenario one fully
// eligible pull request with one fact about the checks changed. A refusal
// is read off the worker's own log line, which carries revalidation's
// reason.
func TestPumpOnce_RequiredChecks_EndToEnd(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()

	const planUnavailable = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`
	tests := []struct {
		name        string
		statuses    []map[string]any
		checkRuns   []map[string]any
		branch      map[string]any
		rulesStatus int
		rulesBody   string
		wantMerged  bool
		wantReason  string
	}{
		{
			name:        "a base requiring nothing merges, as today",
			checkRuns:   []map[string]any{checkRunJSON("build", 1, "success")},
			branch:      protection(),
			rulesStatus: http.StatusOK, rulesBody: rulesJSON(),
			wantMerged: true,
		},
		{
			name:        "a required check that has not reported never merges, and is named",
			checkRuns:   []map[string]any{checkRunJSON("build", 1, "success")},
			branch:      protection(),
			rulesStatus: http.StatusOK, rulesBody: rulesJSON(map[string]any{"context": "ci/slow-external"}),
			wantReason: `required check \"ci/slow-external\" has not reported at the current head`,
		},
		{
			name:        "a failing check beside a passing status of the same name never merges",
			statuses:    []map[string]any{{"context": "ci/build", "state": "success"}},
			checkRuns:   []map[string]any{checkRunJSON("ci/build", 1, "failure")},
			branch:      protection(map[string]any{"context": "ci/build", "app_id": nil}),
			rulesStatus: http.StatusOK, rulesBody: rulesJSON(),
			wantReason: `required check \"ci/build\" did not pass at the current head`,
		},
		{
			name:        "a required check satisfied by another App than the one named never merges",
			checkRuns:   []map[string]any{checkRunJSON("ci/build", 99, "success")},
			branch:      protection(map[string]any{"context": "ci/build", "app_id": 15368}),
			rulesStatus: http.StatusOK, rulesBody: rulesJSON(),
			wantReason: `from the App the base branch names (App id 15368)`,
		},
		{
			name:        "a base requiring narvi/review merges on its other checks",
			checkRuns:   []map[string]any{checkRunJSON("ci/build", 1, "success"), checkRunJSON("narvi/review", 7, nil)},
			branch:      protection(map[string]any{"context": "narvi/review", "app_id": 7}, map[string]any{"context": "ci/build", "app_id": nil}),
			rulesStatus: http.StatusOK, rulesBody: rulesJSON(map[string]any{"context": "narvi/review"}),
			wantMerged: true,
		},
		{
			name:        "a failing check the base does not require, beside satisfied required checks, never merges",
			checkRuns:   []map[string]any{checkRunJSON("ci/build", 1, "success"), checkRunJSON("security-scan", 1, "failure")},
			branch:      protection(map[string]any{"context": "ci/build", "app_id": nil}),
			rulesStatus: http.StatusOK, rulesBody: rulesJSON(),
			wantReason: string(autoapproval.ReasonCINotGreen),
		},
		{
			name:        "rulesets unavailable on the plan declare nothing, and the branch's own requirement is met",
			checkRuns:   []map[string]any{checkRunJSON("ci/build", 15368, "success")},
			branch:      protection(map[string]any{"context": "ci/build", "app_id": 15368}),
			rulesStatus: http.StatusForbidden, rulesBody: planUnavailable,
			wantMerged: true,
		},
		{
			name:        "any other failed read of the requirements never merges",
			checkRuns:   []map[string]any{checkRunJSON("build", 1, "success")},
			branch:      protection(),
			rulesStatus: http.StatusBadGateway, rulesBody: `{"message":"Server Error"}`,
			wantReason: string(autoapproval.ReasonRequiredChecksUnknown),
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := int32(900 + i)
			repo := "required-checks-e2e-" + strconv.Itoa(int(n))
			repoFullName := "acme/" + repo
			head := "sha-rc-" + strconv.Itoa(int(n))
			rig.seedEligiblePR(ctx, t, repoFullName, n, head)
			if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
				t.Fatalf("arm auto-merge: %v", err)
			}

			github := &requiredChecksGitHub{
				repo: repo, number: int(n), head: head,
				statuses: tc.statuses, checkRuns: tc.checkRuns,
				branch: tc.branch, rulesStatus: tc.rulesStatus, rulesBody: tc.rulesBody,
			}
			server := httptest.NewServer(github)
			defer server.Close()

			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
			defer slog.SetDefault(prev)

			worker := rig.newWorker(t, githubapi.New(server.Client(), server.URL))
			if err := worker.PumpOnce(ctx, time.Now()); err != nil {
				t.Fatalf("PumpOnce() error = %v", err)
			}
			slog.SetDefault(prev)

			github.mu.Lock()
			merged, requests, botRequests := github.merged, github.requests, github.botRequests
			github.mu.Unlock()
			if merged != tc.wantMerged {
				t.Fatalf("merged = %v, want %v (requests: %v; log: %s)", merged, tc.wantMerged, requests, logs.String())
			}
			if botRequests != len(requests) {
				t.Errorf("%d of %d requests carried the bot token, want all of them", botRequests, len(requests))
			}
			for _, req := range requests {
				if strings.HasSuffix(req, "/branches/main/protection") {
					t.Errorf("request %q reads the admin-only protection endpoint", req)
				}
			}
			if !strings.Contains(strings.Join(requests, "\n"), "GET /repos/"+repoFullName+"/rules/branches/main") {
				t.Errorf("the base's rulesets were never read (requests: %v)", requests)
			}
			if tc.wantReason != "" && !strings.Contains(logs.String(), tc.wantReason) {
				t.Errorf("the worker's log does not carry the reason %q:\n%s", tc.wantReason, logs.String())
			}

			total, _, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
			if err != nil {
				t.Fatalf("count outcomes: %v", err)
			}
			if wantTotal := map[bool]int64{true: 1, false: 0}[tc.wantMerged]; total != wantTotal {
				t.Errorf("auto-approval outcomes recorded = %d, want %d", total, wantTotal)
			}
		})
	}
}

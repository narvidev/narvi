//go:build integration

// Integration tests for §26.6's amendment, end to end through the real
// verdict handler (reviewverdict.go) against real Postgres: a finding the
// counter-reviewer adds counts as checked only when this turn's own
// persisted sub-task trace shows a fact-check sub-task that started after
// the counter-review and completed; any other addition is published
// marked unverified and counted apart; every published finding records
// its source; and the second fact-check run is recorded apart from the
// first. The trace is seeded through the same EventStore.Create path the
// session actor persists sandbox events with (seedSubTaskStart/
// seedSubTaskFinish, reviewverdict_integration_test.go), so the ordering
// the server checks is the order of real events.id values.
package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

const (
	additionsPrimaryDescription  = "The retry loop never backs off between attempts."
	additionsAddedDescription    = "Defect: `for attempt := 0; ; attempt++` has no exit. Path: Fetch -> retryLoop on a permanent 404. Consequence: the worker spins forever and starves the queue."
	additionsUnverifiedHeading   = "**Unverified -- added by the counter-review and not fact-checked (1, counted apart from the findings):**"
	additionsCheckedMarker       = "_(added by the counter-review; fact-checked after it)_"
	additionsNotFoundReason      = "a fact-check run over it was reported, but none that started after the counter-review and completed was found in this turn's trace when the verdict was posted"
	additionsNotRunReason        = "no fact-check run over it after the counter-review was reported"
	additionsUnconfirmedFragment = "could not be confirmed"
)

// deepVerdictWithAdditionsJSON is a legal deep-path verdict body
// (counterReview done) carrying one primary finding and one counter-review
// addition, with the second fact-check run reported as additionsFactCheck
// (omitted when "") and additionsFactCheckKilled. The first run reports
// factCheckKilled 2, so a merge of the two runs' outcomes is visible.
func deepVerdictWithAdditionsJSON(t *testing.T, additionsFactCheck string, additionsFactCheckKilled int) string {
	t.Helper()
	body := map[string]any{
		"riskLevel":         "low",
		"premise":           "ok",
		"blastRadius":       []string{},
		"filesChanged":      3,
		"testsCoverage":     "adequate",
		"docsDrift":         "none",
		"proposedShippable": "auto",
		"summary":           "One finding of mine, one the counter-review added.",
		"findings": []map[string]any{
			{"severity": "medium", "filePath": "internal/retry/retry.go", "description": additionsPrimaryDescription, "source": "primary"},
			{"severity": "high", "filePath": "internal/retry/retry.go", "description": additionsAddedDescription, "source": "counter_review"},
		},
		"digest": map[string]any{
			"summary":             "Adds a retry helper around the flaky call and swaps every call site onto it.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body accurately describes the retry helper.",
			"archDecisions":       []map[string]any{{"decision": "Reused the existing retry helper."}},
			"stackRisks":          "None of note -- purely additive.",
			"unverifiedLimits":    "Did not run against production traffic.",
		},
		"factCheck":       "done",
		"factCheckKilled": 2,
		"counterReview":   "done",
	}
	if additionsFactCheck != "" {
		body["additionsFactCheck"] = additionsFactCheck
		body["additionsFactCheckKilled"] = additionsFactCheckKilled
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal verdict body: %v", err)
	}
	return string(raw)
}

// findingSourceColumns reads one review_findings row's reported_source
// and addition_check, by description.
func findingSourceColumns(ctx context.Context, t *testing.T, rig testRig, repoFullName, description string) (source, check *string) {
	t.Helper()
	if err := rig.pool.QueryRow(ctx, `SELECT reported_source, addition_check FROM review_findings WHERE repo_full_name = $1 AND description = $2`,
		repoFullName, description).Scan(&source, &check); err != nil {
		t.Fatalf("read review_findings row %q: %v", description, err)
	}
	return source, check
}

// verdictFactCheckRuns is the one review_verdicts row's first and second
// fact-check runs.
type verdictFactCheckRuns struct {
	factCheck                *string
	factCheckKilled          *int32
	additionsFactCheck       *string
	additionsFactCheckKilled *int32
	additionsCheck           *string
}

func readVerdictFactCheckRuns(ctx context.Context, t *testing.T, rig testRig, repoFullName string) verdictFactCheckRuns {
	t.Helper()
	var got verdictFactCheckRuns
	if err := rig.pool.QueryRow(ctx, `SELECT fact_check, fact_check_killed, additions_fact_check, additions_fact_check_killed, additions_check
		FROM review_verdicts WHERE repo_full_name = $1`, repoFullName).Scan(
		&got.factCheck, &got.factCheckKilled, &got.additionsFactCheck, &got.additionsFactCheckKilled, &got.additionsCheck); err != nil {
		t.Fatalf("read review_verdicts row: %v", err)
	}
	return got
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func int32OrNil(p *int32) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

// seedCounterReview persists a completed counter-reviewer sub-task.
func seedCounterReview(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, id string) {
	t.Helper()
	seedSubTaskStart(ctx, t, rig, sessionID, "msg-start-"+id, id, review.CounterReviewerAgentName, 1)
	seedSubTaskFinish(ctx, t, rig, sessionID, "msg-finish-"+id, id, "completed", 1)
}

// seedFactCheck persists a completed fact-check sub-task.
func seedFactCheck(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, id string) {
	t.Helper()
	seedSubTaskStart(ctx, t, rig, sessionID, "msg-start-"+id, id, review.FactCheckAgentName, 1)
	seedSubTaskFinish(ctx, t, rig, sessionID, "msg-finish-"+id, id, "completed", 1)
}

// TestPostReviewVerdict_CounterReviewAddition_CheckedWhenSecondRunFollowsCounterReview
// is the positive case: the first fact-check, the counter-review, then a
// second fact-check that started after it and completed. The addition is
// published checked -- among the findings, marked as the counter-review's
// -- both findings record their source, and the second run's outcome is
// stored apart from the first's.
func TestPostReviewVerdict_CounterReviewAddition_CheckedWhenSecondRunFollowsCounterReview(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	const repo = "acme/additions-checked"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repo, 301)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-additions-checked", 1)
	seedFactCheck(ctx, t, rig, session.ID, "fc-first")
	seedCounterReview(ctx, t, rig, session.ID, "cr")
	seedFactCheck(ctx, t, rig, session.ID, "fc-second")

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, "done", 1))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
		t.Errorf("Shippable = %q, want auto", resp.Shippable)
	}

	if source, check := findingSourceColumns(ctx, t, rig, repo, additionsPrimaryDescription); strOrNil(source) != "primary" || check != nil {
		t.Errorf("primary finding: reported_source = %s, addition_check = %s; want primary, <nil>", strOrNil(source), strOrNil(check))
	}
	if source, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(source) != "counter_review" || strOrNil(check) != "checked" {
		t.Errorf("addition: reported_source = %s, addition_check = %s; want counter_review, checked", strOrNil(source), strOrNil(check))
	}

	runs := readVerdictFactCheckRuns(ctx, t, rig, repo)
	if strOrNil(runs.factCheck) != "done" || int32OrNil(runs.factCheckKilled) != "2" {
		t.Errorf("first run = %s/%s, want done/2 (the first run's own report, never merged with the second)", strOrNil(runs.factCheck), int32OrNil(runs.factCheckKilled))
	}
	if strOrNil(runs.additionsFactCheck) != "done" || int32OrNil(runs.additionsFactCheckKilled) != "1" || strOrNil(runs.additionsCheck) != "checked" {
		t.Errorf("second run = %s/%s resolved %s, want done/1 resolved checked", strOrNil(runs.additionsFactCheck), int32OrNil(runs.additionsFactCheckKilled), strOrNil(runs.additionsCheck))
	}

	body := verdictOutboxBody(ctx, t, rig, session.ID)
	if !strings.Contains(body, additionsCheckedMarker) {
		t.Errorf("posted body does not mark the checked addition as the counter-review's, Body:\n%s", body)
	}
	if strings.Contains(body, "**Unverified") {
		t.Errorf("posted body lists a checked addition as unverified, Body:\n%s", body)
	}
}

// TestPostReviewVerdict_CounterReviewAddition_UnverifiedWhenSecondRunPrecedesCounterReview:
// two completed fact-checks, both BEFORE the counter-review. Any-run-counts
// would call the addition checked; the ordering check finds no run after
// the counter-review, so it is published unverified, listed and counted
// apart in the comment, the readout and the finding-outcomes KPI, and it
// moves the Shippable class no more than a checked one does.
func TestPostReviewVerdict_CounterReviewAddition_UnverifiedWhenSecondRunPrecedesCounterReview(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	const repo = "acme/additions-run-before"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repo, 302)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-additions-run-before", 1)
	seedFactCheck(ctx, t, rig, session.ID, "fc-first")
	seedFactCheck(ctx, t, rig, session.ID, "fc-second-too-early")
	seedCounterReview(ctx, t, rig, session.ID, "cr")

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, "done", 0))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
		t.Errorf("Shippable = %q, want auto (an unverified addition raises the class no more than a checked one)", resp.Shippable)
	}

	if source, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(source) != "counter_review" || strOrNil(check) != "not_found" {
		t.Errorf("addition: reported_source = %s, addition_check = %s; want counter_review, not_found", strOrNil(source), strOrNil(check))
	}
	if runs := readVerdictFactCheckRuns(ctx, t, rig, repo); strOrNil(runs.additionsCheck) != "not_found" || strOrNil(runs.additionsFactCheck) != "done" {
		t.Errorf("second run = %s resolved %s, want done resolved not_found", strOrNil(runs.additionsFactCheck), strOrNil(runs.additionsCheck))
	}

	body := verdictOutboxBody(ctx, t, rig, session.ID)
	for _, want := range []string{
		"**Findings:**\n\n- [general/medium] `internal/retry/retry.go`: " + additionsPrimaryDescription + "\n",
		additionsUnverifiedHeading,
		additionsNotFoundReason,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("posted body missing %q, Body:\n%s", want, body)
		}
	}
	findingsStart := strings.Index(body, "**Findings:**")
	unverifiedStart := strings.Index(body, additionsUnverifiedHeading)
	if addition := strings.Index(body, "Path: Fetch"); addition < unverifiedStart || findingsStart > unverifiedStart {
		t.Errorf("the unverified addition is not listed under its own heading after the findings, Body:\n%s", body)
	}

	// The readout marks it, and carries the second run apart from the first.
	_, token := rig.createAuthenticatedUser(ctx, t)
	var readout restdtos.ReviewReadout
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &readout, token); status != http.StatusOK {
		t.Fatalf("readout status = %d, want %d", status, http.StatusOK)
	}
	sources := map[string]string{}
	checks := map[string]string{}
	for _, f := range readout.Findings {
		sources[f.Description] = strOrNil(f.Source)
		checks[f.Description] = strOrNil(f.AdditionCheck)
	}
	if sources[additionsPrimaryDescription] != "primary" || checks[additionsPrimaryDescription] != "<nil>" {
		t.Errorf("readout primary finding source/check = %s/%s, want primary/<nil>", sources[additionsPrimaryDescription], checks[additionsPrimaryDescription])
	}
	if sources[additionsAddedDescription] != "counter_review" || checks[additionsAddedDescription] != "not_found" {
		t.Errorf("readout addition source/check = %s/%s, want counter_review/not_found", sources[additionsAddedDescription], checks[additionsAddedDescription])
	}
	if readout.LatestVerdict == nil {
		t.Fatal("readout has no latest verdict")
	}
	if lv := readout.LatestVerdict; strOrNil(lv.FactCheck) != "done" || lv.FactCheckKilled != 2 || strOrNil(lv.AdditionsFactCheck) != "done" || lv.AdditionsFactCheckKilled == nil || *lv.AdditionsFactCheckKilled != 0 || strOrNil(lv.AdditionsCheck) != "not_found" {
		t.Errorf("readout verdict runs = first %s/%d, second %s/%v resolved %s; want done/2, done/0 resolved not_found",
			strOrNil(lv.FactCheck), lv.FactCheckKilled, strOrNil(lv.AdditionsFactCheck), lv.AdditionsFactCheckKilled, strOrNil(lv.AdditionsCheck))
	}

	// The finding-outcomes KPI counts the unverified addition apart.
	var analytics restdtos.ReviewAnalytics
	if status := rig.doJSON(t, http.MethodGet, "/api/repos/"+repo+"/review-analytics", nil, &analytics, token); status != http.StatusOK {
		t.Fatalf("analytics status = %d, want %d", status, http.StatusOK)
	}
	if !analytics.FindingOutcomesComputed || analytics.FindingOutcomes == nil || analytics.FindingOutcomesBySource == nil {
		t.Fatalf("analytics finding outcomes not computed: %+v", analytics)
	}
	total := 0
	for _, o := range *analytics.FindingOutcomes {
		total += o.Count
	}
	if total != 1 {
		t.Errorf("findingOutcomes counts %d findings, want 1 (the primary; the unverified addition counted apart)", total)
	}
	bySource := map[string]int{}
	for _, o := range *analytics.FindingOutcomesBySource {
		bySource[o.Source] += o.Count
	}
	if bySource["primary"] != 1 || bySource["counter_review_unverified"] != 1 || bySource["counter_review"] != 0 {
		t.Errorf("findingOutcomesBySource = %v, want primary 1, counter_review_unverified 1", bySource)
	}
}

// TestPostReviewVerdict_CounterReviewAddition_UnverifiedWithNoSecondRun
// covers the turn whose trace holds no fact-check after the counter-review
// at all: the reviewer reported no second run (not_run), reported it
// skipped -- out of the cost budget, say -- (not_run), or reported it done
// with nothing in the trace to show for it (not_found). Each is published
// unverified. One rig per case.
func TestPostReviewVerdict_CounterReviewAddition_UnverifiedWithNoSecondRun(t *testing.T) {
	tests := []struct {
		name       string
		reported   string
		killed     int
		wantCheck  string
		wantReason string
	}{
		{"no second run reported", "", 0, "not_run", additionsNotRunReason},
		{"second run reported skipped (cost budget)", "skipped", 0, "not_run", additionsNotRunReason},
		{"second run reported done, never in the trace", "done", 1, "not_found", additionsNotFoundReason},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/additions-no-second-run-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(310+i))
			seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-additions-no-second-run", 1)
			seedFactCheck(ctx, t, rig, session.ID, "fc-first")
			seedCounterReview(ctx, t, rig, session.ID, "cr")

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, tc.reported, tc.killed))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}
			if _, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(check) != tc.wantCheck {
				t.Errorf("addition_check = %s, want %s", strOrNil(check), tc.wantCheck)
			}
			runs := readVerdictFactCheckRuns(ctx, t, rig, repo)
			if strOrNil(runs.additionsCheck) != tc.wantCheck {
				t.Errorf("review_verdicts.additions_check = %s, want %s", strOrNil(runs.additionsCheck), tc.wantCheck)
			}
			wantReported, wantKilled := tc.reported, fmt.Sprint(tc.killed)
			if tc.reported == "" {
				wantReported, wantKilled = "<nil>", "<nil>"
			}
			if strOrNil(runs.additionsFactCheck) != wantReported || int32OrNil(runs.additionsFactCheckKilled) != wantKilled {
				t.Errorf("second run recorded %s/%s, want %s/%s", strOrNil(runs.additionsFactCheck), int32OrNil(runs.additionsFactCheckKilled), wantReported, wantKilled)
			}
			body := verdictOutboxBody(ctx, t, rig, session.ID)
			if !strings.Contains(body, additionsUnverifiedHeading) || !strings.Contains(body, tc.wantReason) {
				t.Errorf("posted body does not list the addition as unverified with %q, Body:\n%s", tc.wantReason, body)
			}
		})
	}
}

// TestPostReviewVerdict_CounterReviewAddition_CouldNotBeConfirmedWhenTraceUnreadable
// applies §26.1's lesson on the uncorroborated counter-review to the
// additions: when the trace was not read in full, the server says the
// check "could not be confirmed", never that the trace shows none --
// even though a qualifying second run is
// among the rows it did read. Two ways a trace goes unread: a row that
// does not decode, and a turn with no dispatched_event_id to scope a read
// to. One rig per case.
func TestPostReviewVerdict_CounterReviewAddition_CouldNotBeConfirmedWhenTraceUnreadable(t *testing.T) {
	tests := []struct {
		name   string
		spoil  func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, turn sqlcgen.Turn)
		wantCR restdtos.PostReviewVerdictResponseShippable
	}{
		{
			name: "a sub_task_start row whose payload does not decode",
			spoil: func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, _ sqlcgen.Turn) {
				payload := []byte(`{"type":"sub_task_start","gen":1,"subTaskId":7,"subAgentType":"counter-reviewer"}`)
				if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "sub_task_start", MessageID: "msg-start-malformed", Payload: payload}); err != nil {
					t.Fatalf("persist malformed sub_task_start: %v", err)
				}
			},
			// The counter-review itself is still corroborated by the rows
			// that did decode: a positive claim survives a skipped row.
			wantCR: restdtos.PostReviewVerdictResponseShippableAuto,
		},
		{
			name: "a turn with no dispatched_event_id",
			spoil: func(ctx context.Context, t *testing.T, rig testRig, _ pgtype.UUID, turn sqlcgen.Turn) {
				if _, err := rig.pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = NULL WHERE id = $1`, turn.ID); err != nil {
					t.Fatalf("clear dispatched_event_id: %v", err)
				}
			},
			wantCR: restdtos.PostReviewVerdictResponseShippableNeedsHuman,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/additions-unreadable-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(320+i))
			turn := seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-additions-unreadable", 1)
			seedFactCheck(ctx, t, rig, session.ID, "fc-first")
			seedCounterReview(ctx, t, rig, session.ID, "cr")
			seedFactCheck(ctx, t, rig, session.ID, "fc-second")
			tc.spoil(ctx, t, rig, session.ID, turn)

			status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, "done", 0))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}
			if resp.Shippable != tc.wantCR {
				t.Errorf("Shippable = %q, want %q", resp.Shippable, tc.wantCR)
			}
			if _, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(check) != "unconfirmed" {
				t.Errorf("addition_check = %s, want unconfirmed", strOrNil(check))
			}
			if runs := readVerdictFactCheckRuns(ctx, t, rig, repo); strOrNil(runs.additionsCheck) != "unconfirmed" {
				t.Errorf("review_verdicts.additions_check = %s, want unconfirmed", strOrNil(runs.additionsCheck))
			}
			body := verdictOutboxBody(ctx, t, rig, session.ID)
			if !strings.Contains(body, additionsUnverifiedHeading) || !strings.Contains(body, additionsUnconfirmedFragment) {
				t.Errorf("posted body does not say the check could not be confirmed, Body:\n%s", body)
			}
			for _, claim := range []string{"shows no", "was found in this turn's trace"} {
				if strings.Contains(body, claim) {
					t.Errorf("posted body claims %q about a trace the server did not read in full, Body:\n%s", claim, body)
				}
			}
		})
	}
}

// TestPostReviewVerdict_EveryPublishedFindingRecordsItsSource: a finding
// with a garbled source is refused before anything is written, a
// counter-review source is refused off the deep path, and a light-path
// primary finding is stored with its source. A finding with no source at
// all is admitted (TestPostReviewVerdict_FindingWithNoSource_ReadAsNotRecorded).
// One rig per case.
func TestPostReviewVerdict_EveryPublishedFindingRecordsItsSource(t *testing.T) {
	lightBody := func(source string) string {
		finding := `{"severity":"low","filePath":"a.go","description":"Stale comment."`
		if source != "" {
			finding += `,"source":"` + source + `"`
		}
		finding += `}`
		return `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"s","findings":[` + finding +
			`],"digest":{"summary":"Fixes a comment.","descriptionAdequacy":"ok","adequacyExplanation":"Accurate."},"factCheck":"done","factCheckKilled":0}`
	}
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantSource string
	}{
		{"garbled source: refused", lightBody("scribe"), http.StatusBadRequest, ""},
		{"source in the wrong case: refused", lightBody("Primary"), http.StatusBadRequest, ""},
		{"counter_review off the deep path: refused", lightBody("counter_review"), http.StatusBadRequest, ""},
		{"primary: recorded", lightBody("primary"), http.StatusCreated, "primary"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/additions-source-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(330+i))
			headSHA := "sha-additions-source"
			created, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA})
			if err != nil {
				t.Fatalf("seed processing turn: %v", err)
			}
			messageID := testDispatchMessageID
			if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &messageID}); err != nil {
				t.Fatalf("stamp dispatched_message_id: %v", err)
			}

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			var findings, verdicts, outboxRows int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_findings WHERE repo_full_name = $1`, repo).Scan(&findings); err != nil {
				t.Fatalf("count review_findings: %v", err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_verdicts WHERE repo_full_name = $1`, repo).Scan(&verdicts); err != nil {
				t.Fatalf("count review_verdicts: %v", err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).Scan(&outboxRows); err != nil {
				t.Fatalf("count verdict outbox rows: %v", err)
			}
			if tc.wantStatus != http.StatusCreated {
				if findings != 0 || verdicts != 0 || outboxRows != 0 {
					t.Errorf("refused payload wrote %d findings, %d verdicts, %d verdict outbox rows; want none", findings, verdicts, outboxRows)
				}
				return
			}
			if findings != 1 {
				t.Fatalf("review_findings rows = %d, want 1", findings)
			}
			if source, check := findingSourceColumns(ctx, t, rig, repo, "Stale comment."); strOrNil(source) != tc.wantSource || check != nil {
				t.Errorf("reported_source = %s, addition_check = %s; want %s, <nil>", strOrNil(source), strOrNil(check), tc.wantSource)
			}
			if runs := readVerdictFactCheckRuns(ctx, t, rig, repo); runs.additionsFactCheck != nil || runs.additionsFactCheckKilled != nil || runs.additionsCheck != nil {
				t.Errorf("a light-path verdict recorded a second fact-check run: %s/%s resolved %s", strOrNil(runs.additionsFactCheck), int32OrNil(runs.additionsFactCheckKilled), strOrNil(runs.additionsCheck))
			}
		})
	}
}

// basePromptFinding is a finding shaped exactly as the review prompt
// before finding sources asked for one: sentinelKind, severity, filePath,
// line, description and suggestedFix, and no source. A turn's prompt is
// rendered when the turn is created and re-sent as stored, so a turn
// rendered before the deploy posts this shape after it.
func basePromptFinding(description string) map[string]any {
	return map[string]any{
		"sentinelKind": nil,
		"severity":     "medium",
		"filePath":     "internal/retry/retry.go",
		"line":         12,
		"description":  description,
		"suggestedFix": "--- a/internal/retry/retry.go\n+++ b/internal/retry/retry.go\n@@ -12 +12 @@\n-\tfor {\n+\tfor attempt := 0; attempt < 3; attempt++ {\n",
	}
}

// basePromptVerdictJSON is a verdict body as the base prompt shaped it,
// carrying findings with no source: on the deep path, counterReview done
// and the three deep digest fields, and no second-run fields, which that
// prompt never named.
func basePromptVerdictJSON(t *testing.T, deep bool, findings ...map[string]any) string {
	t.Helper()
	digest := map[string]any{
		"summary":             "Adds a retry helper around the flaky call and swaps every call site onto it.",
		"descriptionAdequacy": "ok",
		"adequacyExplanation": "The PR body accurately describes the retry helper.",
	}
	body := map[string]any{
		"riskLevel":         "low",
		"premise":           "ok",
		"blastRadius":       []string{},
		"filesChanged":      3,
		"testsCoverage":     "adequate",
		"docsDrift":         "none",
		"proposedShippable": "auto",
		"summary":           "Findings posted the way the earlier prompt asked.",
		"findings":          findings,
		"digest":            digest,
		"factCheck":         "done",
		"factCheckKilled":   0,
	}
	if deep {
		digest["archDecisions"] = []map[string]any{{"decision": "Reused the existing retry helper."}}
		digest["stackRisks"] = "None of note -- purely additive."
		digest["unverifiedLimits"] = "Did not run against production traffic."
		body["counterReview"] = "done"
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal verdict body: %v", err)
	}
	return string(raw)
}

// earlierPromptFindingObject is the finding object of the review prompt
// rendered before finding sources, verbatim: what a turn created by the
// previous binary carries in turns.prompt.
const earlierPromptFindingObject = "    {\n" +
	"      \"sentinelKind\": \"coverage\" | \"docs_drift\" | null (null for an ordinary risk-map finding with no sentinel origin),\n" +
	"      \"severity\": \"low\" | \"medium\" | \"high\" (required, independent of the verdict's own overall riskLevel above),\n" +
	"      \"filePath\": \"<repo-relative path this finding is about>\" (required),\n" +
	"      \"line\": <integer, optional -- the specific line, if any; never treat this as identifying the finding, only as a human-readable pointer>,\n" +
	"      \"description\": \"<your own finding text>\" (required -- this is compared, normalized, against every future review pass on this same PR, so describe the SAME underlying issue with the SAME wording every time you re-report it, rather than paraphrasing),\n" +
	"      \"suggestedFix\": \"<optional unified-diff/patch text a maintainer's apply-suggestion action can attempt to apply>\"\n" +
	"    }\n"

// seedReviewTurnWithPrompt seeds the processing review turn the verdict is
// posted from, on the deep path (seedProcessingDeepPathTurn) or the light
// one, and stores prompt as its turns.prompt -- the text the server reads
// to tell a turn rendered before finding sources from one rendered after.
func seedReviewTurnWithPrompt(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, deep bool, prompt string) sqlcgen.Turn {
	t.Helper()
	var turn sqlcgen.Turn
	if deep {
		turn = seedProcessingDeepPathTurn(ctx, t, rig, sessionID, "sha-with-prompt", 1)
	} else {
		headSHA, light := "sha-with-prompt", string(reviewtriage.DepthLight)
		created, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA, ReviewDepth: &light})
		if err != nil {
			t.Fatalf("seed processing light-path turn: %v", err)
		}
		messageID := testDispatchMessageID
		if turn, err = rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &messageID}); err != nil {
			t.Fatalf("stamp dispatched_message_id: %v", err)
		}
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET prompt = $2 WHERE id = $1`, turn.ID, prompt); err != nil {
		t.Fatalf("store the turn's prompt: %v", err)
	}
	return turn
}

// TestPostReviewVerdict_FindingWithNoSource_ReadAsNotRecorded is the
// rolling-deploy case for finding sources: a review turn rendered before
// them -- queued, running, or re-sent across the deploy -- posts its
// findings exactly as its stored prompt shaped them, with no source. On
// the light and the deep path the verdict is accepted (201), and every
// finding is stored with no source recorded and read back as such, the
// way a finding last published before sources existed is read: neither
// primary nor an addition, listed among the findings with no marker,
// counted under not_recorded and in the main finding-outcomes
// distribution. One rig per case.
func TestPostReviewVerdict_FindingWithNoSource_ReadAsNotRecorded(t *testing.T) {
	const (
		first  = "The retry loop never backs off between attempts."
		second = "The timeout is read once, at package init, and never refreshed."
	)
	tests := []struct {
		name string
		deep bool
	}{
		{"light path", false},
		{"deep path", true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/no-source-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(340+i))
			seedReviewTurnWithPrompt(ctx, t, rig, session.ID, tc.deep, earlierPromptFindingObject)
			if tc.deep {
				seedFactCheck(ctx, t, rig, session.ID, "fc-first")
				seedCounterReview(ctx, t, rig, session.ID, "cr")
			}

			status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, basePromptVerdictJSON(t, tc.deep, basePromptFinding(first), basePromptFinding(second)))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d (a verdict its own stored prompt shaped must be accepted)", status, http.StatusCreated)
			}
			if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
				t.Errorf("Shippable = %q, want auto", resp.Shippable)
			}

			for _, description := range []string{first, second} {
				if source, check := findingSourceColumns(ctx, t, rig, repo, description); source != nil || check != nil {
					t.Errorf("finding %q: reported_source = %s, addition_check = %s; want <nil>, <nil> (source not recorded)", description, strOrNil(source), strOrNil(check))
				}
			}
			if runs := readVerdictFactCheckRuns(ctx, t, rig, repo); runs.additionsFactCheck != nil || runs.additionsFactCheckKilled != nil || runs.additionsCheck != nil {
				t.Errorf("a verdict with no addition and no second run recorded one: %s/%s resolved %s", strOrNil(runs.additionsFactCheck), int32OrNil(runs.additionsFactCheckKilled), strOrNil(runs.additionsCheck))
			}

			body := verdictOutboxBody(ctx, t, rig, session.ID)
			for _, want := range []string{
				"**Findings:**\n\n",
				"`internal/retry/retry.go`: " + first + "\n",
				"`internal/retry/retry.go`: " + second + "\n",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("posted body missing %q (a no-source finding renders as before), Body:\n%s", want, body)
				}
			}
			for _, marker := range []string{"**Unverified", "added by the counter-review"} {
				if strings.Contains(body, marker) {
					t.Errorf("posted body marks a no-source finding with %q, Body:\n%s", marker, body)
				}
			}

			_, token := rig.createAuthenticatedUser(ctx, t)
			var readout restdtos.ReviewReadout
			if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &readout, token); status != http.StatusOK {
				t.Fatalf("readout status = %d, want %d", status, http.StatusOK)
			}
			if len(readout.Findings) != 2 {
				t.Fatalf("readout findings = %d, want 2", len(readout.Findings))
			}
			for _, f := range readout.Findings {
				if f.Source != nil || f.AdditionCheck != nil {
					t.Errorf("readout finding %q: source %s, additionCheck %s; want null, null (source not recorded)", f.Description, strOrNil(f.Source), strOrNil(f.AdditionCheck))
				}
			}

			var analytics restdtos.ReviewAnalytics
			if status := rig.doJSON(t, http.MethodGet, "/api/repos/"+repo+"/review-analytics", nil, &analytics, token); status != http.StatusOK {
				t.Fatalf("analytics status = %d, want %d", status, http.StatusOK)
			}
			if !analytics.FindingOutcomesComputed || analytics.FindingOutcomes == nil || analytics.FindingOutcomesBySource == nil {
				t.Fatalf("analytics finding outcomes not computed: %+v", analytics)
			}
			total := 0
			for _, o := range *analytics.FindingOutcomes {
				total += o.Count
			}
			if total != 2 {
				t.Errorf("findingOutcomes counts %d findings, want 2 (no-source findings stay in the main distribution)", total)
			}
			bySource := map[string]int{}
			for _, o := range *analytics.FindingOutcomesBySource {
				bySource[o.Source] += o.Count
			}
			if bySource["not_recorded"] != 2 || len(bySource) != 1 {
				t.Errorf("findingOutcomesBySource = %v, want not_recorded 2 and nothing else", bySource)
			}
		})
	}
}

// seedLaterTurnAfterTimeout marks earlier (a turn past its deadline) failed
// -- its agent still running, as handleTurnDeadlineTimer leaves it -- and
// dispatches a later deep-path turn on the same session, to the same
// sandbox at the same gen, with its own watermark and message id: the
// state in which the earlier turn's late verdict is still resolved by its
// own message id while the later turn runs.
func seedLaterTurnAfterTimeout(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, earlier sqlcgen.Turn) sqlcgen.Turn {
	t.Helper()
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: earlier.ID, Status: sqlcgen.TurnStatusFailed}); err != nil {
		t.Fatalf("mark the earlier turn failed: %v", err)
	}
	headSHA, deep := "sha-later-turn", string(reviewtriage.DepthDeep)
	later, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA, ReviewDepth: &deep})
	if err != nil {
		t.Fatalf("create the later turn: %v", err)
	}
	watermark, err := rig.events.MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the later turn's watermark: %v", err)
	}
	gen, messageID := int32(1), "msg-later-turn"
	updated, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: later.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedSandboxGen: &gen, DispatchedEventID: &watermark, DispatchedMessageID: &messageID,
	})
	if err != nil {
		t.Fatalf("stamp the later turn's dispatch: %v", err)
	}
	return updated
}

// TestPostReviewVerdict_CounterReviewAddition_LaterTurnsRunsAreNotTheEarlierTurns
// is the timed-out-turn race: turn A (deep) runs its first fact-check and
// a counter-review, is marked failed at its deadline while its agent keeps
// running, and turn B is dispatched to the same sandbox at the same gen
// and runs its own routine first fact-check. A's agent then posts A's
// verdict, reporting the second run done. B's fact-check started after
// A's counter-review, but it is B's: A's trace is read up to B's dispatch
// watermark and no further, so the addition is not found -- never
// checked. A's own second run, when it ran before B was dispatched, still
// counts, and so does A's counter-review. One rig per case.
func TestPostReviewVerdict_CounterReviewAddition_LaterTurnsRunsAreNotTheEarlierTurns(t *testing.T) {
	tests := []struct {
		name          string
		earlierSecond bool
		wantCheck     string
	}{
		{"the later turn's first fact-check is not the earlier turn's second run", false, "not_found"},
		{"the earlier turn's own second run, before the later turn's dispatch, still counts", true, "checked"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/additions-later-turn-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(350+i))
			turnA := seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-turn-a", 1)
			seedFactCheck(ctx, t, rig, session.ID, "fc-a-first")
			seedCounterReview(ctx, t, rig, session.ID, "cr-a")
			if tc.earlierSecond {
				seedFactCheck(ctx, t, rig, session.ID, "fc-a-second")
			}
			seedLaterTurnAfterTimeout(ctx, t, rig, session.ID, turnA)
			seedFactCheck(ctx, t, rig, session.ID, "fc-b-first")

			status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, "done", 0))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}
			if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
				t.Errorf("Shippable = %q, want auto (A's own counter-review, before B's dispatch, still corroborates)", resp.Shippable)
			}
			if _, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(check) != tc.wantCheck {
				t.Errorf("addition_check = %s, want %s", strOrNil(check), tc.wantCheck)
			}
			if runs := readVerdictFactCheckRuns(ctx, t, rig, repo); strOrNil(runs.additionsCheck) != tc.wantCheck {
				t.Errorf("review_verdicts.additions_check = %s, want %s", strOrNil(runs.additionsCheck), tc.wantCheck)
			}
			body := verdictOutboxBody(ctx, t, rig, session.ID)
			if checked := strings.Contains(body, additionsCheckedMarker); checked != (tc.wantCheck == "checked") {
				t.Errorf("posted body marks the addition fact-checked: %v, want %v, Body:\n%s", checked, tc.wantCheck == "checked", body)
			}
		})
	}
}

// TestPostReviewVerdict_CounterReviewAddition_TurnSharingTheWatermarkCannotBeToldApart:
// turn A is dispatched, ends with no event persisted, and turn B is
// dispatched at the very same watermark. Every sub-task event after it
// could be either turn's, so A's trace is not read as A's alone: the
// addition could not be confirmed, and the counter-review claim is not
// corroborated -- never "checked" on events that might be B's.
func TestPostReviewVerdict_CounterReviewAddition_TurnSharingTheWatermarkCannotBeToldApart(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	const repo = "acme/additions-shared-watermark"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repo, 360)
	turnA := seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-turn-a", 1)
	turnB := seedLaterTurnAfterTimeout(ctx, t, rig, session.ID, turnA)
	if *turnB.DispatchedEventID != *turnA.DispatchedEventID {
		t.Fatalf("watermarks differ (%d, %d): this test needs two turns dispatched with no event between them", *turnA.DispatchedEventID, *turnB.DispatchedEventID)
	}
	seedFactCheck(ctx, t, rig, session.ID, "fc-first")
	seedCounterReview(ctx, t, rig, session.ID, "cr")
	seedFactCheck(ctx, t, rig, session.ID, "fc-second")

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, "done", 0))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
		t.Errorf("Shippable = %q, want needs_human (a counter-review the server cannot attribute to this turn is not corroborated)", resp.Shippable)
	}
	if _, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(check) != "unconfirmed" {
		t.Errorf("addition_check = %s, want unconfirmed", strOrNil(check))
	}
}

// TestPostReviewVerdict_CounterReviewAddition_IncompleteTraceIsUnconfirmed
// covers two traces that look complete row by row but are not, each
// through the handler on real Postgres: the second run's sub_task_finish
// whose payload does not decode (skipped, so the run's outcome is unknown,
// never "not found"), and a counter-reviewer's sub_task_finish whose
// best-effort sub_task_start never landed (a counter-review that may have
// run after the second fact-check). Either way the addition could not be
// confirmed. One rig per case.
func TestPostReviewVerdict_CounterReviewAddition_IncompleteTraceIsUnconfirmed(t *testing.T) {
	tests := []struct {
		name string
		seed func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID)
	}{
		{
			name: "the second run's sub_task_finish does not decode",
			seed: func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
				seedSubTaskStart(ctx, t, rig, sessionID, "msg-start-fc-second", "fc-second", review.FactCheckAgentName, 1)
				payload := []byte(`{"type":"sub_task_finish","gen":1,"subTaskId":"fc-second","outcome":{"x":1}}`)
				if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "sub_task_finish", MessageID: "msg-finish-fc-second-malformed", Payload: payload}); err != nil {
					t.Fatalf("persist malformed sub_task_finish: %v", err)
				}
			},
		},
		{
			name: "a counter-reviewer's finish after the second run, its start lost",
			seed: func(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
				seedFactCheck(ctx, t, rig, sessionID, "fc-second")
				seedSubTaskFinish(ctx, t, rig, sessionID, "msg-finish-cr2", "cr2", "completed", 1)
			},
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/additions-incomplete-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(370+i))
			seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-additions-incomplete", 1)
			seedFactCheck(ctx, t, rig, session.ID, "fc-first")
			seedCounterReview(ctx, t, rig, session.ID, "cr")
			tc.seed(ctx, t, rig, session.ID)

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepVerdictWithAdditionsJSON(t, "done", 0))
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}
			if _, check := findingSourceColumns(ctx, t, rig, repo, additionsAddedDescription); strOrNil(check) != "unconfirmed" {
				t.Errorf("addition_check = %s, want unconfirmed", strOrNil(check))
			}
			body := verdictOutboxBody(ctx, t, rig, session.ID)
			if !strings.Contains(body, additionsUnconfirmedFragment) || strings.Contains(body, "was found in this turn's trace") {
				t.Errorf("posted body does not say the check could not be confirmed, or claims what the trace shows, Body:\n%s", body)
			}
		})
	}
}

// additionsPublishedUnverifiedLogMsg is reviewverdict.go's own log line for
// a verdict that published counter-review additions marked unverified.
const additionsPublishedUnverifiedLogMsg = "httpapi: review-verdict: counter-review additions published unverified"

// TestPostReviewVerdict_SecondRunRemovedEveryAddition_NothingCalledUnverified:
// the counter-review added findings and the second fact-check removed all
// of them, so the verdict publishes none, and reports the run done with
// its kill count. The run's finish has not landed when the verdict is
// posted (§26.4's accepted race). The report and the count are recorded,
// but with no addition published nothing is resolved, logged or shown as
// unverified.
func TestPostReviewVerdict_SecondRunRemovedEveryAddition_NothingCalledUnverified(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	const repo = "acme/additions-all-removed"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repo, 380)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-additions-all-removed", 1)
	seedFactCheck(ctx, t, rig, session.ID, "fc-first")
	seedCounterReview(ctx, t, rig, session.ID, "cr")
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-fc-second", "fc-second", review.FactCheckAgentName, 1)

	var body map[string]any
	if err := json.Unmarshal([]byte(deepVerdictWithAdditionsJSON(t, "done", 2)), &body); err != nil {
		t.Fatalf("unmarshal verdict body: %v", err)
	}
	body["findings"] = []map[string]any{{"severity": "medium", "filePath": "internal/retry/retry.go", "description": additionsPrimaryDescription, "source": "primary"}}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal verdict body: %v", err)
	}

	buf := captureDefaultLoggerJSON(t)
	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, string(raw))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if hasLogEntry(t, buf, additionsPublishedUnverifiedLogMsg) {
		t.Errorf("logged %q for a verdict that published no addition; full log:\n%s", additionsPublishedUnverifiedLogMsg, buf.String())
	}

	runs := readVerdictFactCheckRuns(ctx, t, rig, repo)
	if strOrNil(runs.additionsFactCheck) != "done" || int32OrNil(runs.additionsFactCheckKilled) != "2" || runs.additionsCheck != nil {
		t.Errorf("second run = %s/%s resolved %s, want done/2 resolved <nil> (no addition published)", strOrNil(runs.additionsFactCheck), int32OrNil(runs.additionsFactCheckKilled), strOrNil(runs.additionsCheck))
	}
	if posted := verdictOutboxBody(ctx, t, rig, session.ID); strings.Contains(posted, "**Unverified") {
		t.Errorf("posted body lists unverified additions, but none was published, Body:\n%s", posted)
	}

	_, token := rig.createAuthenticatedUser(ctx, t)
	var readout restdtos.ReviewReadout
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &readout, token); status != http.StatusOK {
		t.Fatalf("readout status = %d, want %d", status, http.StatusOK)
	}
	if readout.LatestVerdict == nil {
		t.Fatal("readout has no latest verdict")
	}
	if lv := readout.LatestVerdict; strOrNil(lv.AdditionsFactCheck) != "done" || lv.AdditionsFactCheckKilled == nil || *lv.AdditionsFactCheckKilled != 2 || lv.AdditionsCheck != nil {
		t.Errorf("readout second run = %s/%v resolved %s, want done/2 resolved null", strOrNil(lv.AdditionsFactCheck), lv.AdditionsFactCheckKilled, strOrNil(lv.AdditionsCheck))
	}
}

// withoutAdditionSource is body with "source" deleted from its
// counter_review findings: an addition its reviewer forgot to label.
func withoutAdditionSource(t *testing.T, body string) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal verdict body: %v", err)
	}
	findings, _ := parsed["findings"].([]any)
	removed := 0
	for _, raw := range findings {
		if f, ok := raw.(map[string]any); ok && f["source"] == "counter_review" {
			delete(f, "source")
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("fixture bug: the body has no counter_review finding to unlabel")
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal verdict body: %v", err)
	}
	return string(out)
}

// TestPostReviewVerdict_FindingWithNoSource_RefusedFromACurrentPrompt: an
// absent source is admitted only from a turn rendered before sources
// existed. A turn whose stored prompt asked every finding for a source,
// on either path, gets the same 400 a garbled source gets, and so does a
// payload reporting the second fact-check run, a field only the current
// prompt names -- even from a turn whose stored prompt predates sources.
// Nothing is written: an addition its reviewer forgot to label is never
// published as an ordinary finding. One rig per case.
func TestPostReviewVerdict_FindingWithNoSource_RefusedFromACurrentPrompt(t *testing.T) {
	tests := []struct {
		name   string
		deep   bool
		prompt string
		body   func(t *testing.T) string
	}{
		{
			name:   "light, the turn's prompt asked for a source",
			prompt: review.RenderTurnPrompt("review this", review.PreFetchedContext{DeepPath: false}),
			body: func(t *testing.T) string {
				return basePromptVerdictJSON(t, false, basePromptFinding("The retry loop never backs off between attempts."))
			},
		},
		{
			name:   "deep, the turn's prompt asked for a source, an addition left unlabelled",
			deep:   true,
			prompt: review.RenderTurnPrompt("review this", review.PreFetchedContext{DeepPath: true}),
			body:   func(t *testing.T) string { return withoutAdditionSource(t, deepVerdictWithAdditionsJSON(t, "", 0)) },
		},
		{
			name:   "deep, an earlier prompt but a second-run report beside an unlabelled addition",
			deep:   true,
			prompt: earlierPromptFindingObject,
			body:   func(t *testing.T) string { return withoutAdditionSource(t, deepVerdictWithAdditionsJSON(t, "done", 0)) },
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repo := fmt.Sprintf("acme/no-source-refused-%d", i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repo, int32(390+i))
			seedReviewTurnWithPrompt(ctx, t, rig, session.ID, tc.deep, tc.prompt)
			if tc.deep {
				seedFactCheck(ctx, t, rig, session.ID, "fc-first")
				seedCounterReview(ctx, t, rig, session.ID, "cr")
			}

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, tc.body(t))
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
			}
			var findings, verdicts, outboxRows int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_findings WHERE repo_full_name = $1`, repo).Scan(&findings); err != nil {
				t.Fatalf("count review_findings: %v", err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_verdicts WHERE repo_full_name = $1`, repo).Scan(&verdicts); err != nil {
				t.Fatalf("count review_verdicts: %v", err)
			}
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).Scan(&outboxRows); err != nil {
				t.Fatalf("count verdict outbox rows: %v", err)
			}
			if findings != 0 || verdicts != 0 || outboxRows != 0 {
				t.Errorf("refused payload wrote %d findings, %d verdicts, %d verdict outbox rows; want none", findings, verdicts, outboxRows)
			}
		})
	}
}

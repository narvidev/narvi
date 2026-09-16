//go:build integration

// Integration tests for §22's own §22.1.1 content-anchored
// positioning, end to end through the real POST /sessions/:id/review/
// verdict handler (reviewverdict.go) against a real Postgres instance --
// gated behind the "integration" build tag, sharing this package's own
// testRig (httpapi_integration_test.go) and fakeReviewContextFetcher
// (reviewretrigger_integration_test.go).
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// positionAnchoringDiff carries a hunk whose "for i := 0; i < len(items);
// i++ {" line lands at new-file line 12 -- the SAME sliding-window
// vocabulary-overlap fixture shape internal/domain/reviewpost's own
// position_test.go already validates in isolation; this file's own job is
// proving the REAL handler wires diff-refetch -> MatchPosition ->
// RenderVerdictComment together correctly, not re-proving the matcher
// itself.
const positionAnchoringDiff = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -8,5 +10,6 @@ func run() error {
 	logger := setupLogger()
 	items := fetchItems()
-	for i := range items {
+	for i := 0; i < len(items); i++ {
+		validateBounds(i, items)
 		process(items[i])
 	}
`

func verdictRequestWithFinding(filePath, description string) string {
	body, err := json.Marshal(map[string]any{
		"riskLevel":         "low",
		"premise":           "ok",
		"blastRadius":       []string{},
		"filesChanged":      3,
		"testsCoverage":     "adequate",
		"docsDrift":         "none",
		"proposedShippable": "auto",
		"summary":           "Anchoring test verdict.",
		"findings": []map[string]any{
			{
				"severity":    "medium",
				"filePath":    filePath,
				"description": description,
			},
		},
		"digest": map[string]any{
			"summary":             "Reworks the loop bounds check.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body accurately describes this change.",
		},
		// factCheck/factCheckKilled (§26.6) are REQUIRED
		// unconditionally, both paths -- this is a light-path (unresolved-
		// depth) turn in this file's own tests, so counterReview stays
		// omitted (never required there, §26.9).
		"factCheck":       "done",
		"factCheckKilled": 0,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// TestPostReviewVerdict_PositionAnchoring_MatchableFindingRendersAnchoredLine
// is this Step's own central end-to-end proof: a finding whose
// description shares real vocabulary with the diff at a specific line
// gets that EXACT line rendered in the posted comment body -- proving the
// full chain (diffFetcher.GetCompareDiff -> reviewpost.MatchPosition ->
// RenderVerdictComment) is wired together correctly inside the real HTTP
// handler, not just unit-tested in isolation.
func TestPostReviewVerdict_PositionAnchoring_MatchableFindingRendersAnchoredLine(t *testing.T) {
	ctx := context.Background()
	fetcher := &fakeReviewContextFetcher{
		pr:   githubapi.PullRequest{HeadSHA: "anchoring-head-sha", BaseRef: "main"},
		diff: positionAnchoringDiff,
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.botToken = "test-bot-token"
	})

	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-anchor-match", 60)
	headSHA := "anchoring-head-sha"
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA})
	if err != nil {
		t.Fatalf("seed processing turn with review head sha: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	body := verdictRequestWithFinding("main.go", "for i := 0; i < len(items); i++ looks risky")
	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var raw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1`, session.ID).Scan(&raw); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	var payload githubapi.VerdictPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}

	if fetcher.diffHead != "anchoring-head-sha" {
		t.Errorf("GetCompareDiff was pinned to head=%q, want %q", fetcher.diffHead, "anchoring-head-sha")
	}

	if !strings.Contains(payload.Body, "main.go:12") {
		t.Errorf("posted comment body missing anchored position %q; got:\n%s", "main.go:12", payload.Body)
	}
}

// TestPostReviewVerdict_PositionAnchoring_UnmatchableFindingRendersNoLine
// is the fail-safe counterpart: a finding sharing no real vocabulary with
// the diff renders with NO line reference at all (never the model's own
// unverified Line, never a guessed position) -- §22.1.1's own "0 is a
// UI-branchable fact, not a plausible-looking wrong answer", proven
// through the real handler end to end.
func TestPostReviewVerdict_PositionAnchoring_UnmatchableFindingRendersNoLine(t *testing.T) {
	ctx := context.Background()
	fetcher := &fakeReviewContextFetcher{
		pr:   githubapi.PullRequest{HeadSHA: "anchoring-head-sha-2", BaseRef: "main"},
		diff: positionAnchoringDiff,
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.botToken = "test-bot-token"
	})

	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-anchor-nomatch", 61)
	headSHA := "anchoring-head-sha-2"
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA})
	if err != nil {
		t.Fatalf("seed processing turn with review head sha: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	body := verdictRequestWithFinding("main.go", "completely unrelated prose about quarterly revenue projections")
	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var raw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1`, session.ID).Scan(&raw); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	var payload githubapi.VerdictPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}

	if strings.Contains(payload.Body, "main.go:") {
		t.Errorf("posted comment body rendered a line reference for an unmatchable finding; got:\n%s", payload.Body)
	}
	if !strings.Contains(payload.Body, "`main.go`") {
		t.Errorf("posted comment body should still render the bare file path; got:\n%s", payload.Body)
	}
}

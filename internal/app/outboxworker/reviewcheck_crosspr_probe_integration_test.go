//go:build integration

// This file is C1's own reproduction: the round-3 review's HIGH finding
// that two open pull requests sharing one head commit collapse onto ONE
// GitHub check run, because a check run is scoped to a commit, never to
// a pull request, and (before the fix this probe pins) nothing in the
// adoption/recovery read scoped its match to one pull request either.
// Deliberately built against ONLY notifier.Deliver and store.
// GetByRepoAndPRNumber -- both public, both unchanged in shape across the
// fix -- so this exact file can run unmodified against the pre-fix
// library code (a `git stash` of the library files alone, this probe
// left in place) to capture the "before" failure, then again against the
// fix to capture the "after" pass. The report accompanying this Step
// carries both runs' actual output.
package outboxworker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestReviewCheckNotifier_TwoPRsOneCommit_NeverCollapseOntoOneCheckRun is
// the two-PR-one-commit probe: PR #101 and PR #102, same repo, the
// IDENTICAL head SHA (the realistic shape named in the finding -- the
// same branch opened against two different bases, or a fork PR beside a
// same-repo PR carrying the same commit). Each pull request's own
// review-check emissions must resolve to its OWN external check run,
// never share one -- sharing one means every later emission from EITHER
// pull request overwrites what the OTHER's Checks tab shows, exactly the
// corruption the finding's own report reproduces: PR #101 assessed
// (terminal_assessed) after PR #102 goes terminal_not_assessed on the
// SAME shared run leaves PR #101's Checks tab showing action_required
// even though its own review passed.
func TestReviewCheckNotifier_TwoPRsOneCommit_NeverCollapseOntoOneCheckRun(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReviewCheckRunStore(pool)

	fake := newFakeCheckRunGitHub()
	server := fake.server()
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := outboxworker.NewReviewCheckNotifier(pool, store, adapter, "tok")

	owner, repoName := "acme", fmt.Sprintf("crosspr-repo-%d", time.Now().UnixNano())
	const prA = 101
	const prB = 102
	const headSHA = "5ha4edc0mm17"

	attemptA := newTestAttempt(ctx, t, pool)
	attemptB := newTestAttempt(ctx, t, pool)
	createdA := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	createdB := createdA.Add(time.Minute)

	// Step 1: PR #101 goes "running" -- the first emission for this head
	// sha, deployment-wide -- so it necessarily creates a fresh check run.
	runningA, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prA, HeadSHA: headSHA,
		AttemptID: attemptA, AttemptCreatedAt: createdA, Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal running(A): %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningA}); err != nil {
		t.Fatalf("Deliver(A running) error = %v", err)
	}
	createsAfterA, _ := fake.counts()
	t.Logf("after PR #101 running: creates=%d", createsAfterA)
	if createsAfterA != 1 {
		t.Fatalf("creates after PR #101 running = %d, want 1", createsAfterA)
	}

	// Step 2: PR #102 -- a DIFFERENT pull request, SAME repo, SAME head
	// sha -- also goes "running". Nothing has been recorded locally for
	// PR #102 yet (its own review_check_runs row is brand new), so its
	// own Deliver call reaches the identical "no existing external id"
	// recovery/adoption read PR #101's own call already exercised.
	runningB, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prB, HeadSHA: headSHA,
		AttemptID: attemptB, AttemptCreatedAt: createdB, Phase: "running",
	})
	if err != nil {
		t.Fatalf("marshal running(B): %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: runningB}); err != nil {
		t.Fatalf("Deliver(B running) error = %v", err)
	}
	createsAfterB, updatesAfterB := fake.counts()
	t.Logf("after PR #102 running: creates=%d updates=%d", createsAfterB, updatesAfterB)

	rowA, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prA)
	if err != nil {
		t.Fatalf("get row A: %v", err)
	}
	rowB, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prB)
	if err != nil {
		t.Fatalf("get row B: %v", err)
	}
	if rowA.ExternalID == nil || rowB.ExternalID == nil {
		t.Fatalf("external ids not yet recorded: pr101.external_id=%v pr102.external_id=%v", rowA.ExternalID, rowB.ExternalID)
	}
	t.Logf("pr101.external_id=%d  pr102.external_id=%d", *rowA.ExternalID, *rowB.ExternalID)

	// The regression assertion: two DIFFERENT pull requests must never
	// share one external check-run identity.
	if *rowA.ExternalID == *rowB.ExternalID {
		t.Errorf("pr101.external_id == pr102.external_id == %d -- two DIFFERENT pull requests collapsed onto ONE GitHub check run", *rowA.ExternalID)
	}

	// Step 3: PR #101 concludes -- a real verdict was posted.
	terminalA, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prA, HeadSHA: headSHA,
		AttemptID: attemptA, AttemptCreatedAt: createdA, Phase: "terminal_assessed",
	})
	if err != nil {
		t.Fatalf("marshal terminal_assessed(A): %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: terminalA}); err != nil {
		t.Fatalf("Deliver(A terminal_assessed) error = %v", err)
	}
	t.Logf("PR #101 terminal_assessed -> run %d state = %+v", *rowA.ExternalID, fake.stateFor(*rowA.ExternalID))

	// Step 4: PR #102 concludes -- no verdict was ever posted for it.
	notAssessedB, err := json.Marshal(ports.ReviewCheckPayload{
		Owner: owner, Repo: repoName, PRNumber: prB, HeadSHA: headSHA,
		AttemptID: attemptB, AttemptCreatedAt: createdB, Phase: "terminal_not_assessed",
	})
	if err != nil {
		t.Fatalf("marshal terminal_not_assessed(B): %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: notAssessedB}); err != nil {
		t.Fatalf("Deliver(B terminal_not_assessed) error = %v", err)
	}
	t.Logf("PR #102 terminal_not_assessed -> run %d state = %+v", *rowB.ExternalID, fake.stateFor(*rowB.ExternalID))

	// The corruption the finding's own report describes: re-fetch PR
	// #101's own external run state AFTER PR #102's own terminal write.
	// If the two pull requests collapsed onto one identity, PR #101's own
	// run now reads action_required (PR #102's own write, landing last)
	// even though PR #101's own row still says terminal_assessed.
	finalStateA := fake.stateFor(*rowA.ExternalID)
	t.Logf("PR #101's own run, AFTER PR #102's own write: %+v", finalStateA)
	if finalStateA["conclusion"] != "success" {
		t.Errorf("PR #101's own check run conclusion = %v, want success (its own review passed) -- PR #102's own terminal_not_assessed write corrupted PR #101's own Checks-tab result, exactly the two-PR-one-commit collapse this probe reproduces",
			finalStateA["conclusion"])
	}

	rowAAfter, err := store.GetByRepoAndPRNumber(ctx, owner+"/"+repoName, prA)
	if err != nil {
		t.Fatalf("get row A after: %v", err)
	}
	if rowAAfter.Phase != "terminal_assessed" {
		t.Errorf("PR #101's own row phase = %q, want terminal_assessed (unaffected by PR #102)", rowAAfter.Phase)
	}
}

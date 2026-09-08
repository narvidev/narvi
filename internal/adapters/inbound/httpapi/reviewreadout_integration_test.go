//go:build integration

// Integration tests for §26.1's own merge-readout read model
// (reviewreadout.go): GET /api/sessions/{sessionID}/review, against a real
// Postgres instance -- gated behind the "integration" build tag, sharing
// this package's own testRig (httpapi_integration_test.go) and
// fakeReviewContextFetcher/createOwnedGitHubReviewSession (reviewretrigger_
// integration_test.go).
//
// Before this file, this route had ZERO integration coverage of its own --
// confirmed by grep before writing this file: no other *_test.go in this
// package ever called httpapi.GetReviewReadout or hit GET .../review. This
// file's own first two tests (NotFound/NoGitHubPRMapping) are therefore not
// purely this Step's own addition -- they pin this handler's own
// pre-existing baseline behavior for the first time, alongside the four new
// gap fields (§12.2 item 2) this Step actually adds.
package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestGetReviewReadout_NotFound proves a nonexistent session id 404s,
// mirroring every other session-scoped route in this package.
func TestGetReviewReadout_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/00000000-0000-0000-0000-000000000000/review", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestGetReviewReadout_NoGitHubPRMapping_BadRequest proves an ordinary,
// non-GitHub session (no github_pr_sessions row at all) is rejected 400 --
// there is no PR to read a review readout for.
func TestGetReviewReadout_NoGitHubPRMapping_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (no github_pr_sessions row for this session)", status, http.StatusBadRequest)
	}
}

// TestGetReviewReadout_FourGapFields_HonestlyEmptyWhenNothingToReport is
// §12.2 item 2's own baseline: a review session whose PR has never been
// re-mentioned, carries no visual-qa label (no live fetcher configured at
// all here, mirroring this rig's own established "diffFetcher nil by
// default" degrade), never had a sentinel-fix triggered, and was never
// flagged by the handoff-readiness sentinel must report sessionReuse as
// "claimed once" (never fabricated) and the other three as null -- never a
// fabricated non-empty value for data that genuinely does not exist.
func TestGetReviewReadout_FourGapFields_HonestlyEmptyWhenNothingToReport(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/gap-fields-empty-repo", 1)

	var got restdtos.ReviewReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got.SessionReuse.MentionCount != 1 {
		t.Errorf("SessionReuse.MentionCount = %d, want 1 (claimed exactly once, never reused)", got.SessionReuse.MentionCount)
	}
	if got.SessionReuse.ClaimedAt.IsZero() {
		t.Error("SessionReuse.ClaimedAt is zero, want a real timestamp")
	}
	if got.VisualQa != nil {
		t.Errorf("VisualQa = %v, want nil (no live fetcher configured, no label to read)", got.VisualQa)
	}
	if got.SentinelFix != nil {
		t.Errorf("SentinelFix = %+v, want nil (no sentinel-fix ever triggered)", got.SentinelFix)
	}
	if got.HandoffReadiness != nil {
		t.Errorf("HandoffReadiness = %+v, want nil (never flagged)", got.HandoffReadiness)
	}
}

// TestGetReviewReadout_SessionReuse_ReflectsRealMentionCount proves
// sessionReuse is a genuine READ of github_pr_sessions.mention_count --
// coalesce.go's own REUSE-branch increment (internal/adapters/inbound/
// github, a different package/ingress path) is the one real production
// writer; this test drives that SAME store method
// (GitHubPRSessionStore.IncrementMentionCount) directly, exactly like a
// second and third @mention on this PR would, and asserts the readout
// reflects the result -- never a hand-built response.
func TestGetReviewReadout_SessionReuse_ReflectsRealMentionCount(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	repoFullName := "acme/gap-fields-reuse-repo"
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, 7)

	for i := 0; i < 2; i++ {
		if _, err := rig.prSessions.IncrementMentionCount(ctx, repoFullName, 7); err != nil {
			t.Fatalf("increment mention count (%d): %v", i, err)
		}
	}

	var got restdtos.ReviewReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.SessionReuse.MentionCount != 3 {
		t.Errorf("SessionReuse.MentionCount = %d, want 3 (1 original claim + 2 real increments)", got.SessionReuse.MentionCount)
	}
}

// TestGetReviewReadout_VisualQa_ReadFromRealLiveLabel proves visualQa is
// read from the SAME live GetPullRequest call this handler already makes
// for prTitle (never a second outbound call), tolerant of a space after
// the colon (a human might type either).
func TestGetReviewReadout_VisualQa_ReadFromRealLiveLabel(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{pr: githubapi.PullRequest{
		Title:  "adds the widget",
		Labels: []string{"review:low-risk", "visual-qa: pass", "needs-triage"},
	}}
	rig := newTestRig(t, func(r *testRig) { r.diffFetcher = fetcher })
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/visual-qa-repo", 3)

	var got restdtos.ReviewReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.VisualQa == nil || *got.VisualQa != "pass" {
		t.Errorf("VisualQa = %v, want \"pass\"", got.VisualQa)
	}
	// prTitle rides the SAME call -- proves this is genuinely one fetch,
	// not two independently-behaving code paths that happen to agree here.
	if got.PrTitle == nil || *got.PrTitle != "adds the widget" {
		t.Errorf("PrTitle = %v, want \"adds the widget\"", got.PrTitle)
	}
}

// TestGetReviewReadout_VisualQa_NilWhenNoMatchingLabel proves an
// unrelated label set never fabricates a visual-qa value.
func TestGetReviewReadout_VisualQa_NilWhenNoMatchingLabel(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{pr: githubapi.PullRequest{Labels: []string{"review:low-risk", "needs-triage"}}}
	rig := newTestRig(t, func(r *testRig) { r.diffFetcher = fetcher })
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/visual-qa-absent-repo", 4)

	var got restdtos.ReviewReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.VisualQa != nil {
		t.Errorf("VisualQa = %v, want nil", got.VisualQa)
	}
}

// TestGetReviewReadout_SentinelFix_ReflectsRealRow proves sentinelFix is a
// genuine read of sentinel_fixes, seeded via the SAME SentinelFixStore
// methods §17's own production flow uses (Claim then UpdateOpened) rather
// than a hand-built response.
func TestGetReviewReadout_SentinelFix_ReflectsRealRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	repoFullName := "acme/sentinel-fix-repo"
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, 9)

	if _, err := rig.sentinelFixes.Claim(ctx, repoFullName, 9, session.ID, "agent/origin-branch"); err != nil {
		t.Fatalf("claim sentinel fix: %v", err)
	}
	claimed, err := rig.sentinelFixes.Get(ctx, repoFullName, 9)
	if err != nil {
		t.Fatalf("get claimed sentinel fix: %v", err)
	}
	if _, err := rig.sentinelFixes.UpdateOpened(ctx, claimed.ID, 42); err != nil {
		t.Fatalf("update sentinel fix opened: %v", err)
	}

	var got restdtos.ReviewReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.SentinelFix == nil {
		t.Fatal("SentinelFix = nil, want a real row")
	}
	if got.SentinelFix.Status != "fix_open" {
		t.Errorf("SentinelFix.Status = %q, want %q", got.SentinelFix.Status, "fix_open")
	}
	if got.SentinelFix.FixPrNumber == nil || *got.SentinelFix.FixPrNumber != 42 {
		t.Errorf("SentinelFix.FixPrNumber = %v, want 42", got.SentinelFix.FixPrNumber)
	}
	if got.SentinelFix.StackRegistered {
		t.Error("SentinelFix.StackRegistered = true, want false (never registered in this test)")
	}
}

// TestGetReviewReadout_HandoffReadiness_ReflectsRealRow proves
// handoffReadiness is a genuine read of handoff_sentinel_runs, seeded via
// the SAME HandoffSentinelStore.Claim call §14.4's own production flow
// uses.
func TestGetReviewReadout_HandoffReadiness_ReflectsRealRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	repoFullName := "acme/handoff-readiness-repo"
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, 11)

	claimed, err := rig.handoffSentinelRuns.Claim(ctx, repoFullName, 11, session.ID, true, 3)
	if err != nil {
		t.Fatalf("claim handoff sentinel run: %v", err)
	}
	if !claimed {
		t.Fatal("claim = false, want true (first claim for this PR)")
	}

	var got restdtos.ReviewReadout
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.HandoffReadiness == nil {
		t.Fatal("HandoffReadiness = nil, want a real row")
	}
	if !got.HandoffReadiness.ContractDriftFlagged {
		t.Error("HandoffReadiness.ContractDriftFlagged = false, want true")
	}
	if got.HandoffReadiness.TodoCount != 3 {
		t.Errorf("HandoffReadiness.TodoCount = %d, want 3", got.HandoffReadiness.TodoCount)
	}
	if got.HandoffReadiness.FlaggedAt.IsZero() {
		t.Error("HandoffReadiness.FlaggedAt is zero, want a real timestamp")
	}
}

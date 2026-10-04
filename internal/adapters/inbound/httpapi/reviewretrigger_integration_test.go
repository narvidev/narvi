//go:build integration

// Integration tests for §8.2's ("review sessions", §8.2) own manual
// re-trigger-via-BUTTON REST endpoint (reviewretrigger.go), against a real
// Postgres instance -- gated behind the "integration" build tag, sharing
// this package's own testRig (httpapi_integration_test.go).
package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeReviewContextFetcher is a test-only reviewcontext.Fetcher -- no real
// HTTP round trip, mirroring internal/adapters/inbound/github's own
// identically-named fixture (handler_integration_test.go) exactly.
// diffOwner/diffRepo/diffBase/diffHead/diffToken (audit fix, test-coverage
// finding, updated for) record GetCompareDiff's own
// last call args: this endpoint's own diffFetcher call site
// (reviewretrigger.go) previously had NO integration coverage at all with
// a non-nil diffFetcher wired in (this rig's own default leaves it nil)
// -- an owner/repo swap there would have been invisible to every test in
// the repo, unit or integration. pr's own HeadSHA/BaseRef (below) are
// what GetCompareDiff must be pinned to -- the C2 fix's own core
// property.
type fakeReviewContextFetcher struct {
	pr githubapi.PullRequest

	diff          string
	diffTruncated bool

	diffOwner string
	diffRepo  string
	diffBase  string
	diffHead  string
	diffToken string

	// resolveBranchSHA (finding F1/F4/F6 (§21.1's amendment)) is the value
	// ResolveBranchSHA below reports -- "" (the zero value, every
	// EXISTING test in this file) mirrors reviewcontext.Fetch's own
	// "resolution unavailable, fall back to pinning the diff fetch on
	// pr.BaseRef" degradation. A test proving the full review-context
	// handoff end to end (TestRetriggerReview_PersistsFullReviewVerdictContext_
	// ReadBackByPostReviewVerdict) sets this explicitly so
	// review.PreFetchedContext.BaseSHA -- and therefore the persisted
	// review_verdict_context.baseSha -- is genuinely non-empty.
	resolveBranchSHA string

	// prErr/diffErr fail the pull-request and diff reads -- the unreadable
	// inputs §26.3 routes deep (TestRetriggerReview_UnreadableInput_
	// RoutesDeepAndRecordsTheCause). nil in every other test.
	prErr   error
	diffErr error
}

func (f *fakeReviewContextFetcher) GetPullRequest(_ context.Context, _, _ string, _ int32, _ string) (githubapi.PullRequest, error) {
	return f.pr, f.prErr
}

// ResolveBranchSHA (finding F1 (§21.1's amendment)) reports resolveBranchSHA --
// "" (every EXISTING test in this file, none of which sets it) mirrors
// reviewcontext.Fetch's own "resolution unavailable" degradation, falling
// back to pinning the diff fetch on pr.BaseRef, exactly this fake's own
// pre-existing behavior, so every EXISTING assertion against diffBase in
// this file keeps passing unchanged.
func (f *fakeReviewContextFetcher) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return f.resolveBranchSHA, spec.Branch, nil
}

func (f *fakeReviewContextFetcher) GetCompareDiff(_ context.Context, owner, repo, base, head, token string) (string, bool, error) {
	f.diffOwner, f.diffRepo, f.diffBase, f.diffHead, f.diffToken = owner, repo, base, head, token
	return f.diff, f.diffTruncated, f.diffErr
}

// createOwnedGitHubReviewSession creates a session (CreatedBy = owner) plus
// a github_pr_sessions row pointing back at it -- exactly the fixture shape
// coalesce.go's own WINNER path produces in production, built directly via
// the stores here rather than round-tripping through the real webhook
// handler (a different package's own concern, already covered by
// internal/adapters/inbound/github's own integration tests).
func (r testRig) createOwnedGitHubReviewSession(ctx context.Context, t *testing.T, ownerID pgtype.UUID, repoFullName string, prNumber int32) sqlcgen.Session {
	t.Helper()

	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: ownerID})
	if err != nil {
		t.Fatalf("create test github review session: %v", err)
	}
	if err := r.prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := r.prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	return session
}

// TestRetriggerReview_NotFound proves a nonexistent session id is a plain
// 404, exactly like every other session-scoped route in this package.
func TestRetriggerReview_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/00000000-0000-0000-0000-000000000000/review/retrigger", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestRetriggerReview_NoGitHubPRMapping_BadRequest proves an ordinary,
// non-GitHub session (no github_pr_sessions row at all) is rejected 400 --
// this action is meaningless for a session with no PR to review. The
// caller must be authorized for authz.ActionRetriggerReview (maintainer)
// to reach that 400 at all -- the authz gate runs BEFORE the PR-mapping
// check.
func TestRetriggerReview_NoGitHubPRMapping_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (no github_pr_sessions row for this session)", status, http.StatusBadRequest)
	}
}

// TestRetriggerReview_MemberDenied_EvenAsSessionOwner is this endpoint's
// own regression test for a confirmed privilege-escalation audit finding:
// this handler used to authorize against authz.ActionPromptSession (the
// SAME own/joined-aware check CreateTurn's REST endpoint applies), which
// let a plain member re-trigger a review on any session THEY THEMSELVES
// created. §13.3 row 5 ("edit review verdicts; re-trigger reviews;
// auto-approval eligibility config") is admin/maintainer only, with NO
// member own/joined carve-out -- so even the session's own owning member
// must be denied here, unlike CreateTurn's own ordinary prompt gate.
func TestRetriggerReview_MemberDenied_EvenAsSessionOwner(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, ownerToken := rig.createAuthenticatedUser(ctx, t) // default role: member (httpapi_integration_test.go's own createAuthenticatedUser).

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/member-owner-repo", 1)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, ownerToken)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (a member must be denied even on a session they themselves own -- §13.3 row 5 has no member carve-out)", status, http.StatusForbidden)
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 0 {
		t.Errorf("turn count = %d, want 0 (the denied call must never queue a turn)", turnCount)
	}
}

// TestRetriggerReview_MaintainerAllowed_EvenAsNonOwner is this endpoint's
// own regression test for the OTHER half of the same §13.3 row 5 rule:
// unlike ActionPromptSession's own own/joined carve-out, ActionRetriggerReview
// has NO ownership concept at all -- a maintainer who neither created nor
// joined the target session is still allowed, admin/maintainer bypassing
// ownership entirely.
func TestRetriggerReview_MaintainerAllowed_EvenAsNonOwner(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t) // some other, unrelated member owns the session.
	_, maintainerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/maintainer-nonowner-repo", 2)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, maintainerToken)
	if status != http.StatusCreated {
		t.Errorf("status = %d, want %d (a maintainer must be allowed regardless of ownership/participation)", status, http.StatusCreated)
	}
}

// TestRetriggerReview_Success proves the happy path: a maintainer
// re-triggering an (unrelated) GitHub review session gets a new, queued
// turn, carrying this endpoint's own fixed, deterministic
// manualRetriggerPromptText (diffFetcher is nil in this rig's own default
// wiring, so no pre-fetched context is folded in -- the resulting prompt
// is exactly the fixed constant, unmodified). The caller is a maintainer,
// not the session's own owner -- authz.ActionRetriggerReview (§13.3 row 5)
// is admin/maintainer only, with no member own/joined carve-out at all
// (see TestRetriggerReview_MemberDenied_EvenAsSessionOwner above).
func TestRetriggerReview_Success(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	maintainer, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/retrigger-repo", 55)

	var resp restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, &resp, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Id == "" {
		t.Error("response Id is empty, want a real turn id")
	}

	var turnCount int
	var prompt string
	if err := rig.pool.QueryRow(ctx, `SELECT count(*), max(prompt) FROM turns WHERE session_id = $1`, session.ID).Scan(&turnCount, &prompt); err != nil {
		t.Fatalf("query turns: %v", err)
	}
	if turnCount != 1 {
		t.Fatalf("turn count = %d, want 1", turnCount)
	}
	if prompt != "Manual re-review requested via the web review button." {
		t.Errorf("turn prompt = %q, want the fixed manualRetriggerPromptText constant", prompt)
	}

	// Technical plan §24.9: the button records the person's request -- its
	// lane, its requester and its own sentence -- so a click that meets a
	// moved pull request when it is dispatched can be owed to that person
	// and re-run from that sentence.
	var trigger, text *string
	var requestedBy pgtype.UUID
	var moves *int32
	if err := rig.pool.QueryRow(ctx, `SELECT request_trigger, requested_by, request_text, context_moves FROM turns WHERE session_id = $1`, session.ID).
		Scan(&trigger, &requestedBy, &text, &moves); err != nil {
		t.Fatalf("read the turn's request: %v", err)
	}
	if trigger == nil || *trigger != "button" || requestedBy != maintainer.ID || text == nil || *text != prompt || moves != nil {
		t.Errorf("turn's request = (%v, %v, %v, %v), want the button's, by the maintainer who clicked, with its own sentence and no move", trigger, requestedBy, text, moves)
	}
}

// TestRetriggerReview_PreFetchesReviewContext_CorrectOwnerRepoArgs (audit
// fix, test-coverage finding) proves this endpoint's OWN diffFetcher call
// site (reviewretrigger.go, distinct code from internal/adapters/inbound/
// github's own identical-looking call) reaches reviewcontext.Fetch with
// the correct owner/repo/number/token split from prSession.RepoFullName --
// this rig's own default wiring leaves diffFetcher nil (this file's own
// testRig doc comment), so before this test, an owner/repo swap at THIS
// call site specifically would have been invisible to every test in the
// repo.
func TestRetriggerReview_PreFetchesReviewContext_CorrectOwnerRepoArgs(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{
		diff: "diff --git a/x b/x\n+hello\n",
		// HeadSHA/BaseRef are what GetCompareDiff
		// must be pinned to -- a zero-value fixture would only prove the
		// degenerate empty-base/empty-head case.
		pr: githubapi.PullRequest{HeadSHA: "resolved-head-sha", BaseRef: "main"},
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/prefetch-retrigger-repo", 42)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	// owner ("acme") and repo ("prefetch-retrigger-repo") are deliberately
	// distinguishable strings -- a swapped-argument regression at this
	// call site would fail this assertion. diffBase/diffHead additionally
	// prove the C2 fix's own core property: pinned to EXACTLY what
	// GetPullRequest reported.
	if fetcher.diffOwner != "acme" || fetcher.diffRepo != "prefetch-retrigger-repo" || fetcher.diffBase != "main" || fetcher.diffHead != "resolved-head-sha" || fetcher.diffToken != "test-bot-token" {
		t.Errorf("GetCompareDiff args = (%q, %q, base=%q, head=%q, %q), want (%q, %q, base=%q, head=%q, %q)",
			fetcher.diffOwner, fetcher.diffRepo, fetcher.diffBase, fetcher.diffHead, fetcher.diffToken, "acme", "prefetch-retrigger-repo", "main", "resolved-head-sha", "test-bot-token")
	}

	// the turn's own persisted review_head_sha
	// must equal exactly the SHA the diff above was pinned to.
	var reviewHeadSHA *string
	if err := rig.pool.QueryRow(ctx, `SELECT review_head_sha FROM turns WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`, session.ID).Scan(&reviewHeadSHA); err != nil {
		t.Fatalf("query turn review_head_sha: %v", err)
	}
	if reviewHeadSHA == nil || *reviewHeadSHA != "resolved-head-sha" {
		got := "<nil>"
		if reviewHeadSHA != nil {
			got = *reviewHeadSHA
		}
		t.Errorf("turns.review_head_sha = %s, want %q", got, "resolved-head-sha")
	}

	var prompt string
	if err := rig.pool.QueryRow(ctx, `SELECT prompt FROM turns WHERE session_id = $1`, session.ID).Scan(&prompt); err != nil {
		t.Fatalf("query turn prompt: %v", err)
	}
	if !strings.Contains(prompt, "diff --git a/x b/x") {
		t.Errorf("prompt = %q, want it to contain the pre-fetched diff", prompt)
	}
}

// TestRetriggerReview_PersistsFullReviewVerdictContext_ReadBackByPostReviewVerdict
// is §21.1's amendment's own findings F4/F6 regression test: "the turn->verdict
// context handoff has no test at all... every existing test injects the
// Context directly into Insert, so the handoff itself is executed by no
// assertion." This test exercises BOTH real production halves of that
// handoff, never a hand-built fixture standing in for either: the WRITE
// half is this endpoint's own real RetriggerReview handler (reviewretrigger.go),
// which calls the REAL reviewcontext.Fetch (through fakeReviewContextFetcher,
// this file's own narrow test double for the outbound GitHub call only) and
// persists its result onto turns.review_verdict_context via the REAL
// createTurnLocked insert (turn.go's own "ReviewVerdictContext:
// reviewVerdictContext" write); the READ half is PostReviewVerdict's own
// real unmarshal (reviewverdict.go). A regression that dropped the field at
// either end (turn.go's own write, or reviewverdict.go's own read) would
// pass every EXISTING test in this repository -- none of them drives both
// halves through their real production code paths at once -- and would
// silently turn ComputeEligible's answer into ReasonContextUnknown for
// EVERY future PR this repo reviews, permanently and silently disabling
// auto-approval/auto-merge (see reviewverdict.go's own doc comment on the
// unmarshal-failure branch, immediately below this test).
func TestRetriggerReview_PersistsFullReviewVerdictContext_ReadBackByPostReviewVerdict(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{
		diff: "diff --git a/x b/x\n+hello\n",
		pr:   githubapi.PullRequest{HeadSHA: "handoff-head-sha", BaseRef: "main"},
		// resolveBranchSHA (finding F1) is the base branch's own LIVE tip
		// -- deliberately set here (unlike every other test in this file)
		// so review.PreFetchedContext.BaseSHA, and therefore the
		// persisted review_verdict_context.baseSha, is genuinely
		// non-empty: this test's own point is proving the FULL context
		// round-trips, not merely the fields that already had coverage.
		resolveBranchSHA: "handoff-base-sha",
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	repoFullName := "acme/verdict-context-handoff"
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, 91)
	// The READ half below (postReviewVerdict) needs a real sandbox row to
	// authenticate against -- RetriggerReview itself needs none.
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	// THE WRITE HALF: the real RetriggerReview handler.
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var turnID pgtype.UUID
	var contextJSON []byte
	if err := rig.pool.QueryRow(ctx, `SELECT id, review_verdict_context FROM turns WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`, session.ID).Scan(&turnID, &contextJSON); err != nil {
		t.Fatalf("query turn review_verdict_context: %v", err)
	}
	if len(contextJSON) == 0 {
		t.Fatal("turns.review_verdict_context is NULL/empty -- the real write half (turn.go's own createTurnLocked insert) did not persist it")
	}
	var writtenContext reviewverdict.Context
	if err := json.Unmarshal(contextJSON, &writtenContext); err != nil {
		t.Fatalf("unmarshal turns.review_verdict_context: %v", err)
	}
	if writtenContext.BaseRef != "main" {
		t.Errorf("written context BaseRef = %q, want %q", writtenContext.BaseRef, "main")
	}
	if writtenContext.BaseSHA != "handoff-base-sha" {
		t.Errorf("written context BaseSHA = %q, want %q", writtenContext.BaseSHA, "handoff-base-sha")
	}
	if writtenContext.PolicyVersion != autoapproval.CurrentPolicyVersion {
		t.Errorf("written context PolicyVersion = %d, want %d", writtenContext.PolicyVersion, autoapproval.CurrentPolicyVersion)
	}

	// Simulates the (separately fixed and tested, finding F3) real
	// dispatch stamp -- the ONE piece of the real production path this
	// test does not drive live, since doing so would require a live
	// sessionactor.Actor/SandboxCommander; dispatch.go's own real
	// UpdateStatus write is covered directly by dispatch_test.go and by
	// reviewverdict_integration_test.go's own F3 regression test.
	dispatchMessageID := "handoff-dispatch-message-id"
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &dispatchMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on the real created turn: %v", err)
	}

	// THE READ HALF: the real PostReviewVerdict handler.
	verdictStatus, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", dispatchMessageID, validVerdictRequestJSON())
	if verdictStatus != http.StatusCreated {
		t.Fatalf("verdict status = %d, want %d", verdictStatus, http.StatusCreated)
	}

	var gotBaseRef, gotBaseSHA string
	var gotPolicyVersion int
	if err := rig.pool.QueryRow(ctx, `SELECT base_ref, base_sha, policy_version FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, 91).
		Scan(&gotBaseRef, &gotBaseSHA, &gotPolicyVersion); err != nil {
		t.Fatalf("query review_verdicts row: %v", err)
	}

	// THE DECISIVE HANDOFF ASSERTION: the verdict's own persisted context
	// matches EXACTLY what the real write half recorded -- proving the
	// full round trip, through two real production functions, never a
	// hand-injected Context.
	if gotBaseRef != writtenContext.BaseRef {
		t.Errorf("review_verdicts.base_ref = %q, want %q (the turn's own written context)", gotBaseRef, writtenContext.BaseRef)
	}
	if gotBaseSHA != writtenContext.BaseSHA {
		t.Errorf("review_verdicts.base_sha = %q, want %q (the turn's own written context)", gotBaseSHA, writtenContext.BaseSHA)
	}
	if gotPolicyVersion != writtenContext.PolicyVersion {
		t.Errorf("review_verdicts.policy_version = %d, want %d (the turn's own written context)", gotPolicyVersion, writtenContext.PolicyVersion)
	}
}

// TestRetriggerReview_AlwaysQueue_NeverRejectsAnAlreadyOpenTurn proves this
// endpoint's own AlwaysQueue policy (reviewretrigger.go's own top doc
// comment): unlike the ordinary REST CreateTurn endpoint's RejectIfOpen
// 409, a manual re-review click on a session that ALREADY has a Pending
// turn still succeeds, queuing behind it.
func TestRetriggerReview_AlwaysQueue_NeverRejectsAnAlreadyOpenTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/queue-repo", 7)

	// First click.
	first := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if first != http.StatusCreated {
		t.Fatalf("first click status = %d, want %d", first, http.StatusCreated)
	}
	// Second click, immediately -- the first turn is still Pending
	// (nothing dispatches it in this test's own minimal registry wiring,
	// httpapi_integration_test.go's own doc comment). An ordinary
	// CreateTurn call here would 409; this endpoint must not.
	second := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if second != http.StatusCreated {
		t.Fatalf("second click status = %d, want %d (AlwaysQueue must never reject an already-open turn)", second, http.StatusCreated)
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 2 {
		t.Errorf("turn count = %d, want 2 (both clicks must queue)", turnCount)
	}
}

// TestRetriggerReview_ConcurrentClicks_AllSucceedNoDeadlock is this
// endpoint's own real-concurrency proof: N concurrent manual re-review
// clicks against the SAME already-existing session all succeed, producing
// exactly N turns on that ONE session -- never a race, a deadlock, or a
// lost turn. Driven with real concurrent HTTP requests against the real
// handler/real Postgres, matching internal/adapters/inbound/github's own
// TestGitHubIntegration_ConcurrentMentionsCoalesceToOneSessionManyTurns
// style exactly (real goroutines, not sequential calls).
func TestRetriggerReview_ConcurrentClicks_AllSucceedNoDeadlock(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/concurrent-retrigger-repo", 303)

	const n = 8
	start := make(chan struct{})
	statuses := make([]int, n)

	var g errgroup.Group
	for i := 0; i < n; i++ {
		idx := i
		g.Go(func() error {
			<-start
			statuses[idx] = rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
			return nil
		})
	}
	close(start)
	if err := g.Wait(); err != nil {
		t.Fatalf("concurrent retrigger clicks: %v", err)
	}

	for i, status := range statuses {
		if status != http.StatusCreated {
			t.Errorf("statuses[%d] = %d, want %d", i, status, http.StatusCreated)
		}
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1`, session.ID).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("session count = %d, want exactly 1 (this endpoint never creates a session)", sessionCount)
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != n {
		t.Errorf("turn count = %d, want exactly %d (one turn per concurrent click, none lost/raced away)", turnCount, n)
	}
}

// TestRetriggerReview_AlreadyAnsweredFacts_PrependedNeverReplacingProse is
// §8.2's own explicitly required test (§22.1): an already-open
// review_findings row for this PR renders as a deterministic "already
// answered" fact block PREPENDED to -- never replacing -- the manual
// re-trigger's own fixed prose text (manualRetriggerPromptText,
// reviewretrigger.go).
func TestRetriggerReview_AlreadyAnsweredFacts_PrependedNeverReplacingProse(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	repoFullName := "acme/already-answered-repo"
	const prNumber = int32(55)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, prNumber)

	const wantDescription = "Missing test coverage for the timeout path."
	if _, err := rig.reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Severity:     "medium",
		FilePath:     "internal/foo/bar.go",
		Description:  wantDescription,
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var prompt string
	if err := rig.pool.QueryRow(ctx, `SELECT prompt FROM turns WHERE session_id = $1`, session.ID).Scan(&prompt); err != nil {
		t.Fatalf("query turn prompt: %v", err)
	}

	if !strings.Contains(prompt, wantDescription) {
		t.Fatalf("prompt = %q, want it to contain the already-answered finding's own description %q", prompt, wantDescription)
	}
	// The exact literal manualRetriggerPromptText renders (reviewretrigger.go)
	// -- unexported, so named here as a literal rather than imported (this
	// is package httpapi_test, mirroring every other black-box test in
	// this file).
	const wantProseFallback = "Manual re-review requested via the web review button."
	if !strings.Contains(prompt, wantProseFallback) {
		t.Fatalf("prompt = %q, want it to STILL contain the manual re-trigger's own prose fallback (prepended TO, never REPLACING it)", prompt)
	}

	factsIdx := strings.Index(prompt, wantDescription)
	proseIdx := strings.Index(prompt, wantProseFallback)
	if factsIdx >= proseIdx {
		t.Errorf("already-answered facts appear at index %d, prose fallback at index %d -- want facts PREPENDED (before), not appended after", factsIdx, proseIdx)
	}
}

// TestRetriggerReview_AlreadyAnsweredFacts_RetiresFindingWhoseFileLeftTheDiff
// is this Step's own end-to-end proof of §22.1.2's "determinable fact"
// retirement refinement, exercised through this handler's real HTTP path
// rather than reviewpost's own unit-test layer: a real diffFetcher is
// wired (so prCtx.ChangedPaths is genuinely populated from a fetched
// diff, not left nil), and a pre-existing review_findings row names a
// file that diff never touches. The persisted turn prompt must still
// carry that finding (never a silent drop, §22.3's advisory-never-a-
// filter posture) but annotated RETIRED -- and a second, still-live
// finding on a file the diff DOES touch must render with no such
// annotation, proving this endpoint's own reordering of the
// already-answered-facts call (moved, by this Step, to AFTER the diff
// fetch so prCtx.ChangedPaths is available) actually threads real diff
// data through, not just an empty/nil placeholder.
func TestRetriggerReview_AlreadyAnsweredFacts_RetiresFindingWhoseFileLeftTheDiff(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{
		diff: "diff --git a/internal/live.go b/internal/live.go\n--- a/internal/live.go\n+++ b/internal/live.go\n+hello\n",
		pr:   githubapi.PullRequest{HeadSHA: "resolved-head-sha", BaseRef: "main"},
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	repoFullName := "acme/retirement-repo"
	const prNumber = int32(56)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, prNumber)

	const staleDescription = "Old finding on a file this diff no longer touches."
	const liveDescription = "Finding on a file still in this diff."
	if _, err := rig.reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: "1111111111111111111111111111111111111111111111111111111111111111",
		Severity:     "medium",
		FilePath:     "internal/stale.go",
		Description:  staleDescription,
	}); err != nil {
		t.Fatalf("upsert stale review finding: %v", err)
	}
	if _, err := rig.reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: "2222222222222222222222222222222222222222222222222222222222222222",
		Severity:     "medium",
		FilePath:     "internal/live.go",
		Description:  liveDescription,
	}); err != nil {
		t.Fatalf("upsert live review finding: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var prompt string
	if err := rig.pool.QueryRow(ctx, `SELECT prompt FROM turns WHERE session_id = $1`, session.ID).Scan(&prompt); err != nil {
		t.Fatalf("query turn prompt: %v", err)
	}

	if !strings.Contains(prompt, staleDescription) || !strings.Contains(prompt, liveDescription) {
		t.Fatalf("prompt = %q, want it to contain BOTH findings -- retirement notes, never silently drops (§22.3)", prompt)
	}

	var staleLine, liveLine string
	for _, line := range strings.Split(prompt, "\n") {
		switch {
		case strings.Contains(line, staleDescription):
			staleLine = line
		case strings.Contains(line, liveDescription):
			liveLine = line
		}
	}
	if staleLine == "" || liveLine == "" {
		t.Fatalf("prompt = %q, could not locate both findings' own lines", prompt)
	}
	if !strings.Contains(staleLine, "RETIRED:") {
		t.Errorf("stale finding's own line = %q, want a RETIRED annotation (its file left the diff)", staleLine)
	}
	if strings.Contains(liveLine, "RETIRED:") {
		t.Errorf("live finding's own line = %q, want NO RETIRED annotation (its file is still in the diff)", liveLine)
	}
}

// TestRetriggerReview_AwaitingPlanAlwaysDeclines_NeverClassifies is F1's
// own regression test (§23 follow-up fix, review Finding 1) for this
// endpoint: a manual re-review click carries no human reply for the
// plan_followup classifier (ClassifyPlanFollowup) to legitimately read --
// reviewretrigger.go no longer even accepts an *intentclassifier.Service
// parameter at all (removed by this fix), so CreateTurnCore is always
// called with a literal nil intentSvc here, which -- per createTurnLocked's
// own nil-safe "skip classification entirely" contract (turn.go) --
// degrades this endpoint to the SAME safe, deterministic pre-existing
// "always decline while a plan is awaiting approval" outcome, structurally
// incapable of ever promoting a re-trigger into a plan-revision turn based
// on a misread of manualRetriggerPromptText or the pre-fetched diff. This
// wires a real diffFetcher (so review.RenderTurnPrompt's own diff/verdict-
// tool text IS folded into the prompt CreateTurnCore receives) specifically
// to prove the decline holds even then -- the exact scenario that, before
// this fix, would have fed that enriched text to the classifier instead of
// skipping it.
func TestRetriggerReview_AwaitingPlanAlwaysDeclines_NeverClassifies(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{
		diff: "diff --git a/x b/x\n+hello\n",
		pr:   githubapi.PullRequest{HeadSHA: "resolved-head-sha", BaseRef: "main"},
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = platform.MustNewGitHubOutboundConfig("test-bot-token")
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/awaiting-plan-retrigger-repo", 909)

	// Seed a producing turn (Completed, plan_mode true) and an
	// awaiting_approval plans row atop the session -- mirrors
	// planfollowupgate_integration_test.go's own seedAwaitingApprovalPlan
	// precedent (that file lives in package httpapi, unreachable from here;
	// this is this file's own inline equivalent).
	producingTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	if _, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	var errResp errorResponseForTest
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, &errResp, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d (a re-trigger must always decline while a plan is awaiting approval -- there is no classifier wired here to promote it)", status, http.StatusConflict)
	}
	if !strings.Contains(errResp.Error, "awaiting approval") {
		t.Errorf("error body = %q, want it to mention the plan is awaiting approval", errResp.Error)
	}

	// Only the seeded producing turn -- the decline must not have inserted
	// a new turn (revision or otherwise).
	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 1 {
		t.Errorf("turn count = %d, want 1 (only the seeded producing turn -- the decline must never enqueue a new turn)", turnCount)
	}
}

// TestRetriggerReview_DeepToLight_StaysFloorAtDeep is D1's own regression
// test for this lane (adversarial-review fix, "re-review depth floor
// applied at only 1 of 3 lanes"): before this fix, this endpoint fed the
// FRESH, unfloored triage decision straight through with no awareness of
// this PR's own prior depth at all -- a light-looking re-review (a
// one-line readable diff; an input never fetched now routes deep on its
// own, §26.3, which would hide the floor) through this button would have produced
// review_depth = "light" even though this PR had already gone deep once,
// silently defeating §24's own "once deep, a PR stays deep" floor for
// every OTHER lane reading review_verdicts.review_path back afterward
// (readReviewRetriggerState, internal/app/sessionactor/reviewretrigger.go).
// With the fix, ComputeDecision's own third return value (the SAME
// review_verdicts.GetLatest read already performed for the "prior high
// verdict" signal) feeds domainreviewtriage.Floor, and the floored --
// never the fresh -- depth is what actually gets persisted.
func TestRetriggerReview_DeepToLight_StaysFloorAtDeep(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = &fakeReviewContextFetcher{
			pr:   githubapi.PullRequest{HeadSHA: "sha-light-head", BaseRef: "main", ChangedFiles: 1, Additions: 1, Deletions: 1},
			diff: "diff --git a/internal/app/foo/a.go b/internal/app/foo/a.go\n--- a/internal/app/foo/a.go\n+++ b/internal/app/foo/a.go\n@@ -1 +1 @@\n-x\n+y\n",
		}
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)

	repoFullName := "acme/deep-to-light-retrigger-repo"
	const prNumber = 271
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, prNumber)

	// A PRIOR verdict already on record for this exact PR, review_path
	// "deep" -- the fact D1's own floor must consult.
	deepPath := "deep"
	if _, err := rig.reviewVerdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      repoFullName,
		PrNumber:          prNumber,
		HeadSha:           "sha-prior-deep-review",
		RiskLevel:         "low",
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "auto",
		Shippable:         "auto",
		SessionID:         session.ID,
		ReviewPath:        &deepPath,
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
		AncestorChain:     []byte(`[]`),
	}); err != nil {
		t.Fatalf("seed prior deep review verdict: %v", err)
	}

	// A one-line readable diff -- the FRESH decision this firing computes
	// is deterministically light (ReasonLightDefault) -- the floor, not
	// the fresh signal, is what this test isolates.
	var resp restdtos.CreateTurnResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, &resp, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var reviewDepth *string
	var reviewDepthDecisionJSON []byte
	if err := rig.pool.QueryRow(ctx, `SELECT review_depth, review_depth_decision FROM turns WHERE id = $1`, resp.Id).Scan(&reviewDepth, &reviewDepthDecisionJSON); err != nil {
		t.Fatalf("query turn review_depth: %v", err)
	}
	if reviewDepth == nil || *reviewDepth != "deep" {
		got := "<nil>"
		if reviewDepth != nil {
			got = *reviewDepth
		}
		t.Errorf("turns.review_depth = %s, want %q (a light-looking re-review must still floor at this PR's own prior deep depth)", got, "deep")
	}

	if reviewDepthDecisionJSON == nil {
		t.Fatal("turns.review_depth_decision is nil, want a recorded decision")
	}
	var record struct {
		Depth   string `json:"depth"`
		Floored bool   `json:"floored"`
	}
	if err := json.Unmarshal(reviewDepthDecisionJSON, &record); err != nil {
		t.Fatalf("unmarshal review_depth_decision: %v", err)
	}
	if record.Depth != "deep" {
		t.Errorf("review_depth_decision.depth = %q, want %q", record.Depth, "deep")
	}
	if !record.Floored {
		t.Error("review_depth_decision.floored = false, want true (the fresh decision was light; the floor is what actually decided)")
	}
}

// TestRetriggerReview_UnreadableInput_RoutesDeepAndRecordsTheCause pins
// §26.3's unreadable-input rule through this lane (the re-review button):
// whatever the review context could not read -- or never tried to -- the
// turn routes deep under its own reason and its routing record names the
// cause. One rig per case: each re-review spawns a session actor, which
// pins a connection of the rig's small pool.
func TestRetriggerReview_UnreadableInput_RoutesDeepAndRecordsTheCause(t *testing.T) {
	readablePR := githubapi.PullRequest{HeadSHA: "sha-unreadable-head", BaseRef: "main", ChangedFiles: 2, Additions: 5, Deletions: 1}
	tests := []struct {
		name          string
		fetcher       *fakeReviewContextFetcher
		wantDepth     string
		wantReason    string
		wantInputRead string
	}{
		{name: "no read made", fetcher: nil, wantDepth: "deep", wantReason: "review input could not be read in full", wantInputRead: "not_fetched"},
		{name: "pull request unreadable", fetcher: &fakeReviewContextFetcher{prErr: errors.New("github 502")}, wantDepth: "deep", wantReason: "review input could not be read in full", wantInputRead: "pr_unreadable"},
		{name: "diff unreadable", fetcher: &fakeReviewContextFetcher{pr: readablePR, diffErr: errors.New("github 502")}, wantDepth: "deep", wantReason: "review input could not be read in full", wantInputRead: "diff_unreadable"},
		{name: "diff names fewer files than the pull request", fetcher: &fakeReviewContextFetcher{pr: readablePR, diff: "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-x\n+y\n"}, wantDepth: "deep", wantReason: "review input could not be read in full", wantInputRead: "diff_truncated"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newTestRig(t, func(r *testRig) {
				if tt.fetcher != nil {
					r.diffFetcher = tt.fetcher
				}
			})
			ctx := context.Background()
			owner, _ := rig.createAuthenticatedUser(ctx, t)
			_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
			session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/unreadable-retrigger-repo", int32(300+i))

			var resp restdtos.CreateTurnResponse
			if status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, &resp, token); status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}

			var reviewDepth *string
			var recordJSON []byte
			if err := rig.pool.QueryRow(ctx, `SELECT review_depth, review_depth_decision FROM turns WHERE id = $1`, resp.Id).Scan(&reviewDepth, &recordJSON); err != nil {
				t.Fatalf("query turn: %v", err)
			}
			if reviewDepth == nil || *reviewDepth != tt.wantDepth {
				t.Errorf("turns.review_depth = %v, want %q", reviewDepth, tt.wantDepth)
			}
			var record struct {
				Reason    string `json:"reason"`
				InputRead string `json:"inputRead"`
			}
			if err := json.Unmarshal(recordJSON, &record); err != nil {
				t.Fatalf("unmarshal review_depth_decision %s: %v", recordJSON, err)
			}
			if record.Reason != tt.wantReason || record.InputRead != tt.wantInputRead {
				t.Errorf("record reason/inputRead = %q/%q, want %q/%q", record.Reason, record.InputRead, tt.wantReason, tt.wantInputRead)
			}
		})
	}
}

// TestRetriggerReview_SizePatternsReachTheDecision pins that the
// deployment's size patterns, carried on the Deps this route is built
// with, reach the re-review button's decision: 40 source lines beside 600
// test lines route light, on 40.
func TestRetriggerReview_SizePatternsReachTheDecision(t *testing.T) {
	var diff strings.Builder
	diff.WriteString("diff --git a/internal/app/billing/charge.go b/internal/app/billing/charge.go\n--- a/internal/app/billing/charge.go\n+++ b/internal/app/billing/charge.go\n@@ -0,0 +1,40 @@\n")
	for i := 0; i < 40; i++ {
		diff.WriteString("+source line\n")
	}
	diff.WriteString("diff --git a/internal/app/billing/charge_test.go b/internal/app/billing/charge_test.go\n--- a/internal/app/billing/charge_test.go\n+++ b/internal/app/billing/charge_test.go\n@@ -0,0 +1,600 @@\n")
	for i := 0; i < 600; i++ {
		diff.WriteString("+test line\n")
	}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = &fakeReviewContextFetcher{
			pr:   githubapi.PullRequest{HeadSHA: "sha-size-head", BaseRef: "main", ChangedFiles: 2, Additions: 640},
			diff: diff.String(),
		}
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/size-retrigger-repo", 373)

	var resp restdtos.CreateTurnResponse
	if status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, &resp, token); status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var reviewDepth *string
	var recordJSON []byte
	if err := rig.pool.QueryRow(ctx, `SELECT review_depth, review_depth_decision FROM turns WHERE id = $1`, resp.Id).Scan(&reviewDepth, &recordJSON); err != nil {
		t.Fatalf("query turn: %v", err)
	}
	var record struct {
		ChangedLines int  `json:"changedLines"`
		SourceLines  *int `json:"sourceLines"`
	}
	if err := json.Unmarshal(recordJSON, &record); err != nil {
		t.Fatalf("unmarshal review_depth_decision %s: %v", recordJSON, err)
	}
	if reviewDepth == nil || *reviewDepth != "light" || record.SourceLines == nil || *record.SourceLines != 40 || record.ChangedLines != 640 {
		t.Errorf("review_depth = %v, record = %s, want light with changedLines 640 and sourceLines 40", reviewDepth, recordJSON)
	}
}

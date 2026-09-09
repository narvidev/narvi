package releasereview_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/releasereview"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeCompositionTemplateFetcher is a test-only
// releasereview.CompositionTemplateFetcher.
type fakeCompositionTemplateFetcher struct {
	template string
	err      error
	calls    int
	lastName string
}

func (f *fakeCompositionTemplateFetcher) GetTemplate(_ context.Context, name string) (string, error) {
	f.calls++
	f.lastName = name
	if f.err != nil {
		return "", f.err
	}
	return f.template, nil
}

// fakeCompositionDiffFetcher is a test-only reviewcontext.Fetcher --
// mirrors internal/app/reviewcontext's own fakeFetcher precedent
// (fetch_test.go) exactly, narrowed to what these tests need.
//
// Test-integrity fix: BOTH methods now RECORD every argument they were
// called with (lastGetPullRequest*/lastGetCompareDiff*), rather than
// ignoring all of them the way this fake previously did -- a previous
// version of this fake accepted (and silently discarded) owner/repo/
// number/token/base/head on every call, which meant NO test in this file
// could ever have caught dispatchCompositionReview passing the wrong
// owner/repo/PR number/token through to reviewcontext.Fetch, or Fetch
// itself passing the wrong (base, head) pair to GetCompareDiff.
type fakeCompositionDiffFetcher struct {
	pr            githubapi.PullRequest
	prErr         error
	diff          string
	diffTruncated bool
	diffErr       error
	prCalls       int

	lastGetPullRequestOwner  string
	lastGetPullRequestRepo   string
	lastGetPullRequestNumber int32
	lastGetPullRequestToken  string

	compareDiffCalls        int
	lastGetCompareDiffOwner string
	lastGetCompareDiffRepo  string
	lastGetCompareDiffBase  string
	lastGetCompareDiffHead  string
	lastGetCompareDiffToken string
}

func (f *fakeCompositionDiffFetcher) GetPullRequest(_ context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error) {
	f.prCalls++
	f.lastGetPullRequestOwner = owner
	f.lastGetPullRequestRepo = repo
	f.lastGetPullRequestNumber = number
	f.lastGetPullRequestToken = token
	return f.pr, f.prErr
}

func (f *fakeCompositionDiffFetcher) GetCompareDiff(_ context.Context, owner, repo, base, head, token string) (string, bool, error) {
	f.compareDiffCalls++
	f.lastGetCompareDiffOwner = owner
	f.lastGetCompareDiffRepo = repo
	f.lastGetCompareDiffBase = base
	f.lastGetCompareDiffHead = head
	f.lastGetCompareDiffToken = token
	return f.diff, f.diffTruncated, f.diffErr
}

// fakeCompositionTurnInserter is a test-only
// releasereview.CompositionTurnInserter.
type fakeCompositionTurnInserter struct {
	err        error
	calls      int
	lastParams sqlcgen.CreateTurnParams
}

func (f *fakeCompositionTurnInserter) Create(_ context.Context, arg sqlcgen.CreateTurnParams) (sqlcgen.Turn, error) {
	f.calls++
	f.lastParams = arg
	if f.err != nil {
		return sqlcgen.Turn{}, f.err
	}
	return sqlcgen.Turn{ID: arg.SessionID}, nil
}

// fakeCompositionDispatcher is a test-only releasereview.CompositionDispatcher.
type fakeCompositionDispatcher struct {
	err         error
	calls       int
	lastSession pgtype.UUID
}

func (f *fakeCompositionDispatcher) EnsureDispatched(_ context.Context, sessionID pgtype.UUID) error {
	f.calls++
	f.lastSession = sessionID
	return f.err
}

// fullCompositionDeps builds a releasereview.Deps with every composition-
// dispatch dependency configured and healthy, plus a base MergedPRLister/
// OutboxEnqueuer -- the caller mutates individual fields to test a
// specific failure branch.
func fullCompositionDeps(lister *fakeMergedPRLister, outbox *fakeOutboxEnqueuer, templates *fakeCompositionTemplateFetcher, diffFetcher *fakeCompositionDiffFetcher, turns *fakeCompositionTurnInserter, dispatch *fakeCompositionDispatcher) releasereview.Deps {
	return releasereview.Deps{
		SourceControl:          lister,
		Outbox:                 outbox,
		CompositionTemplates:   templates,
		CompositionDiffFetcher: diffFetcher,
		CompositionTurns:       turns,
		CompositionDispatch:    dispatch,
		Timeouts:               platform.DefaultTimeouts(),
	}
}

// TestRun_AggregateReviewTriggered_DispatchesCompositionReviewTurn proves
// the core dispatch mechanism: when ShouldRunAggregateReview fires (here,
// via a high-risk-flagged constituent PR), Run fetches the composition
// prompt template, re-fetches the release PR's own diff, inserts a
// pending turn on the SAME session carrying a rendered composition
// prompt, and nudges the session actor to dispatch it.
func TestRun_AggregateReviewTriggered_DispatchesCompositionReviewTurn(t *testing.T) {
	t.Parallel()

	sessionID := testSessionID(t)
	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "Review this release's composition."}
	diffFetcher := &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{
		HeadSHA: "deadbeef",
		BaseRef: "main",
	}, diff: "diff --git a/x b/x\n+hello\n"}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: sessionID,
		Owner:     "acme", Repo: "widgets", PRNumber: 99, BaseRef: "main", HeadRef: "release/1.0", Token: "gho_bottoken",
	})

	if templates.calls != 1 {
		t.Fatalf("CompositionTemplates.GetTemplate calls = %d, want 1", templates.calls)
	}
	if templates.lastName != "release_composition_review" {
		t.Errorf("template name = %q, want %q", templates.lastName, "release_composition_review")
	}
	if diffFetcher.prCalls != 1 {
		t.Fatalf("CompositionDiffFetcher.GetPullRequest calls = %d, want 1", diffFetcher.prCalls)
	}
	// Test-integrity fix: pin WHAT was actually fetched -- a prior version
	// of this test's own fake accepted and silently discarded every
	// argument GetPullRequest/GetCompareDiff were called with, so a bug
	// passing the wrong owner/repo/PR-number/token (or the wrong (base,
	// head) pair once resolved) through to reviewcontext.Fetch would have
	// gone completely undetected here.
	if diffFetcher.lastGetPullRequestOwner != "acme" || diffFetcher.lastGetPullRequestRepo != "widgets" {
		t.Errorf("GetPullRequest(owner, repo) = (%q, %q), want (%q, %q)", diffFetcher.lastGetPullRequestOwner, diffFetcher.lastGetPullRequestRepo, "acme", "widgets")
	}
	if diffFetcher.lastGetPullRequestNumber != 99 {
		t.Errorf("GetPullRequest number = %d, want %d", diffFetcher.lastGetPullRequestNumber, 99)
	}
	if diffFetcher.lastGetPullRequestToken != "gho_bottoken" {
		t.Errorf("GetPullRequest token = %q, want %q", diffFetcher.lastGetPullRequestToken, "gho_bottoken")
	}
	if diffFetcher.compareDiffCalls != 1 {
		t.Fatalf("CompositionDiffFetcher.GetCompareDiff calls = %d, want 1", diffFetcher.compareDiffCalls)
	}
	if diffFetcher.lastGetCompareDiffOwner != "acme" || diffFetcher.lastGetCompareDiffRepo != "widgets" {
		t.Errorf("GetCompareDiff(owner, repo) = (%q, %q), want (%q, %q)", diffFetcher.lastGetCompareDiffOwner, diffFetcher.lastGetCompareDiffRepo, "acme", "widgets")
	}
	// base/head are PINNED to the pr's own resolved BaseRef/HeadSHA
	// (reviewcontext.Fetch's own "pin the compare call" fix), never
	// in.BaseRef/in.HeadRef (the release PR's own BRANCH names) -- proving
	// this call site actually goes through Fetch rather than some other,
	// unpinned path.
	if diffFetcher.lastGetCompareDiffBase != "main" || diffFetcher.lastGetCompareDiffHead != "deadbeef" {
		t.Errorf("GetCompareDiff(base, head) = (%q, %q), want (%q, %q) (pr.BaseRef, pr.HeadSHA)", diffFetcher.lastGetCompareDiffBase, diffFetcher.lastGetCompareDiffHead, "main", "deadbeef")
	}
	if diffFetcher.lastGetCompareDiffToken != "gho_bottoken" {
		t.Errorf("GetCompareDiff token = %q, want %q", diffFetcher.lastGetCompareDiffToken, "gho_bottoken")
	}
	if turns.calls != 1 {
		t.Fatalf("CompositionTurns.Create calls = %d, want 1", turns.calls)
	}
	if turns.lastParams.SessionID != sessionID {
		t.Errorf("inserted turn SessionID = %v, want %v", turns.lastParams.SessionID, sessionID)
	}
	if turns.lastParams.Status != sqlcgen.TurnStatusPending {
		t.Errorf("inserted turn Status = %v, want pending", turns.lastParams.Status)
	}
	if turns.lastParams.Prompt == nil || *turns.lastParams.Prompt == "" {
		t.Fatalf("inserted turn Prompt is nil/empty")
	}
	// Test-integrity fix: pin WHAT went into the prompt, not merely that
	// it is non-empty -- the fetched diff's own content and the
	// composition-findings tool's own instructions block must both
	// actually be present, proving RenderCompositionReviewPrompt was
	// called with the REAL fetched diff (never a stale/empty one) and
	// that this dispatch path really does render the composition-specific
	// prompt (never, say, an ordinary risk-map verdict prompt by
	// accident).
	if !strings.Contains(*turns.lastParams.Prompt, "+hello") {
		t.Errorf("inserted turn Prompt does not contain the fetched diff's own content (%q); prompt = %s", "+hello", *turns.lastParams.Prompt)
	}
	if !strings.Contains(*turns.lastParams.Prompt, "composition-findings-posting tool") {
		t.Errorf("inserted turn Prompt does not contain the composition-findings tool instructions; prompt = %s", *turns.lastParams.Prompt)
	}
	if !strings.Contains(*turns.lastParams.Prompt, "Review this release's composition.") {
		t.Errorf("inserted turn Prompt does not contain the fetched template's own text; prompt = %s", *turns.lastParams.Prompt)
	}
	if turns.lastParams.ReviewHeadSha == nil || *turns.lastParams.ReviewHeadSha != "deadbeef" {
		t.Errorf("inserted turn ReviewHeadSha = %v, want \"deadbeef\"", turns.lastParams.ReviewHeadSha)
	}
	if dispatch.calls != 1 {
		t.Fatalf("CompositionDispatch.EnsureDispatched calls = %d, want 1", dispatch.calls)
	}
	if dispatch.lastSession != sessionID {
		t.Errorf("EnsureDispatched session = %v, want %v", dispatch.lastSession, sessionID)
	}
}

// TestRun_TruncatedCoverageAloneTriggersCompositionDispatch is the
// confirmed-major fix's own regression test: coveragePartial (ListMergedBetween's
// own truncated return) used to be IGNORED by the dispatch gate entirely
// -- only review.ShouldRunAggregateReview's own three named criteria
// could ever trigger the composition pass, so a genuinely truncated
// constituent-PR listing (which could easily have HIDDEN a real trigger,
// e.g. undercounting the ≥3-overlapping-PRs condition) silently skipped
// the pass rather than running it conservatively. Proven here with a
// SINGLE, ordinary, non-high-risk, non-conflicted merged PR (none of the
// three named criteria fire on their own) plus truncated=true -- the
// composition pass must still dispatch.
func TestRun_TruncatedCoverageAloneTriggersCompositionDispatch(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{
		merged:    []ports.MergedPR{{Number: 1, Title: "an ordinary PR", HasApprovingReview: true}},
		truncated: true,
	}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	diffFetcher := &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"}, diff: "diff"}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if turns.calls != 1 {
		t.Fatalf("CompositionTurns.Create calls = %d, want 1 -- a truncated constituent-PR listing must itself trigger the composition pass, never silently skip it", turns.calls)
	}
	if dispatch.calls != 1 {
		t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 1", dispatch.calls)
	}

	// The rendered outbox comment's own trigger reasons must name the
	// truncation as a reason, not just silently dispatch with no
	// human-readable explanation.
	var payload githubapi.ReleaseManifestPayload
	if err := json.Unmarshal(outbox.lastParams.Payload, &payload); err != nil {
		t.Fatalf("decode outbox payload: %v", err)
	}
	if !strings.Contains(payload.Body, "coverage of the release was partial") {
		t.Errorf("rendered comment does not explain the truncation-driven trigger -- body:\n%s", payload.Body)
	}
}

// TestRun_AggregateReviewNotTriggered_NeverDispatchesCompositionReview
// proves the composition pass only ever dispatches when its own §15.3
// trigger fires -- an ordinary, clean release manifest must never spawn
// an extra turn.
func TestRun_AggregateReviewNotTriggered_NeverDispatchesCompositionReview(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	diffFetcher := &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{HeadSHA: "deadbeef"}}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if templates.calls != 0 {
		t.Errorf("CompositionTemplates.GetTemplate calls = %d, want 0 (aggregate review never triggered)", templates.calls)
	}
	if turns.calls != 0 {
		t.Errorf("CompositionTurns.Create calls = %d, want 0 (aggregate review never triggered)", turns.calls)
	}
	if dispatch.calls != 0 {
		t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 0 (aggregate review never triggered)", dispatch.calls)
	}
}

// TestRun_CompositionDepsNotConfigured_DegradesGracefully proves every one
// of the four composition-dispatch dependencies is INDEPENDENTLY nil-safe
// -- a caller that doesn't wire the composition dispatcher (e.g. every
// EXISTING test in run_test.go, predating this Step) must keep behaving
// exactly as before: the manifest check itself is unaffected, and nothing
// panics.
//
// Test-integrity fix: dispatchCompositionReview's own nil-check is a
// single `||` chain (deps.CompositionTemplates == nil ||
// deps.CompositionDiffFetcher == nil || deps.CompositionTurns == nil ||
// deps.CompositionDispatch == nil) -- Go's `||` short-circuits, so a
// single test that nils ALL FOUR at once (as a prior version of this
// test did, by simply never setting any of them) only ever actually
// EVALUATES the FIRST clause (deps.CompositionTemplates == nil); the
// other three clauses are never reached at all. Deleting any ONE of
// those three later clauses from the source would still leave that
// single "nil everything" test green. This is now four subtests, EACH
// nil-ing exactly ONE dependency while the other three are healthy,
// real fakes -- so each one is sensitive to its OWN clause specifically:
// if dispatchCompositionReview's own check for that ONE field were ever
// deleted, that subtest (and only that one) would start inserting a turn
// and fail.
func TestRun_CompositionDepsNotConfigured_DegradesGracefully(t *testing.T) {
	t.Parallel()

	newHealthyDeps := func() (lister *fakeMergedPRLister, outbox *fakeOutboxEnqueuer, templates *fakeCompositionTemplateFetcher, diffFetcher *fakeCompositionDiffFetcher, turns *fakeCompositionTurnInserter, dispatch *fakeCompositionDispatcher) {
		lister = &fakeMergedPRLister{merged: []ports.MergedPR{
			{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
		}}
		outbox = &fakeOutboxEnqueuer{}
		templates = &fakeCompositionTemplateFetcher{template: "t"}
		diffFetcher = &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"}, diff: "diff"}
		turns = &fakeCompositionTurnInserter{}
		dispatch = &fakeCompositionDispatcher{}
		return
	}

	tests := []struct {
		name string
		// nilField names WHICH of the four fields this subtest leaves nil
		// -- the other three are always healthy fakes, so this subtest
		// exercises ONLY that one field's own clause in the `||` chain.
		nilField string
	}{
		{name: "CompositionTemplates nil"},
		{name: "CompositionDiffFetcher nil"},
		{name: "CompositionTurns nil"},
		{name: "CompositionDispatch nil"},
	}
	tests[0].nilField = "templates"
	tests[1].nilField = "diffFetcher"
	tests[2].nilField = "turns"
	tests[3].nilField = "dispatch"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lister, outbox, templates, diffFetcher, turns, dispatch := newHealthyDeps()
			deps := releasereview.Deps{
				SourceControl:          lister,
				Outbox:                 outbox,
				CompositionTemplates:   templates,
				CompositionDiffFetcher: diffFetcher,
				CompositionTurns:       turns,
				CompositionDispatch:    dispatch,
				Timeouts:               platform.DefaultTimeouts(),
			}
			switch tt.nilField {
			case "templates":
				deps.CompositionTemplates = nil
			case "diffFetcher":
				deps.CompositionDiffFetcher = nil
			case "turns":
				deps.CompositionTurns = nil
			case "dispatch":
				deps.CompositionDispatch = nil
			default:
				t.Fatalf("unrecognized nilField %q", tt.nilField)
			}

			releasereview.Run(context.Background(), discardLogger(), deps, releasereview.Input{
				SessionID: testSessionID(t),
				Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
			})

			if outbox.calls != 1 {
				t.Errorf("Outbox.Create calls = %d, want 1 (the manifest check's own outbox comment must still be enqueued)", outbox.calls)
			}
			if turns.calls != 0 {
				t.Errorf("CompositionTurns.Create calls = %d, want 0 (nilField=%s must decline the whole dispatch)", turns.calls, tt.nilField)
			}
			if dispatch.calls != 0 {
				t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 0 (nilField=%s must decline the whole dispatch)", dispatch.calls, tt.nilField)
			}
		})
	}
}

// TestRun_CompositionTemplateFetchFails_NeverInsertsTurn proves a failed
// template fetch declines the whole dispatch -- never a turn inserted
// with a garbled/empty prompt.
func TestRun_CompositionTemplateFetchFails_NeverInsertsTurn(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{err: errors.New("db exploded")}
	diffFetcher := &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{HeadSHA: "deadbeef"}}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if turns.calls != 0 {
		t.Errorf("CompositionTurns.Create calls = %d, want 0 (template fetch failed)", turns.calls)
	}
	if dispatch.calls != 0 {
		t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 0 (template fetch failed)", dispatch.calls)
	}
}

// TestRun_CompositionDiffFetchFails_NeverInsertsTurn proves the §24-style
// fail-closed discipline: an unresolved head sha (GetPullRequest failed)
// declines the dispatch entirely rather than inserting a turn anchored to
// no real commit.
func TestRun_CompositionDiffFetchFails_NeverInsertsTurn(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	diffFetcher := &fakeCompositionDiffFetcher{prErr: errors.New("network exploded")}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if turns.calls != 0 {
		t.Errorf("CompositionTurns.Create calls = %d, want 0 (diff/head-sha fetch failed)", turns.calls)
	}
	if dispatch.calls != 0 {
		t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 0 (diff/head-sha fetch failed)", dispatch.calls)
	}
}

// TestRun_CompareDiffFetchFails_HeadSHAStillResolved_NeverInsertsTurn is
// the confirmed-major fix's own regression test -- DISTINCT from
// TestRun_CompositionDiffFetchFails_NeverInsertsTurn immediately above,
// which covers GetPullRequest itself failing (no head sha at all).
// reviewcontext.Fetch degrades HeadSHA and Diff INDEPENDENTLY: a
// GetPullRequest SUCCESS followed by a GetCompareDiff FAILURE leaves
// HeadSHA populated (the existing "reviewCtx.HeadSHA == \"\"" guard would
// NOT have caught this) while Diff stays "". Before this fix, dispatch
// proceeded anyway, sending RenderCompositionReviewPrompt a prompt with
// NO diff block at all -- the reviewing agent had nothing to review but
// the tool instructions, and could very plausibly still call the
// composition-findings tool with an empty findings array, which would
// have persisted as an INDISTINGUISHABLE-from-genuine "reviewed, found
// nothing" result. This proves the dispatch now declines instead.
func TestRun_CompareDiffFetchFails_HeadSHAStillResolved_NeverInsertsTurn(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	// pr resolves successfully (a real, non-empty HeadSHA) -- ONLY the
	// compare-diff call fails.
	diffFetcher := &fakeCompositionDiffFetcher{
		pr:      githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"},
		diffErr: errors.New("compare api exploded"),
	}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if diffFetcher.prCalls != 1 {
		t.Fatalf("GetPullRequest calls = %d, want 1 (head sha resolution must still be attempted)", diffFetcher.prCalls)
	}
	if turns.calls != 0 {
		t.Errorf("CompositionTurns.Create calls = %d, want 0 -- a pass that never reviewed a diff must never dispatch a turn that could get recorded as a clean result", turns.calls)
	}
	if dispatch.calls != 0 {
		t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 0", dispatch.calls)
	}
}

// TestRun_CompositionTurnInsertFails_NeverDispatches proves a failed turn
// insert never still nudges the session actor to dispatch -- there would
// be nothing pending to dispatch.
func TestRun_CompositionTurnInsertFails_NeverDispatches(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	diffFetcher := &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{HeadSHA: "deadbeef"}}
	turns := &fakeCompositionTurnInserter{err: errors.New("db exploded")}
	dispatch := &fakeCompositionDispatcher{}

	releasereview.Run(context.Background(), discardLogger(), fullCompositionDeps(lister, outbox, templates, diffFetcher, turns, dispatch), releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if dispatch.calls != 0 {
		t.Errorf("CompositionDispatch.EnsureDispatched calls = %d, want 0 (turn insert failed)", dispatch.calls)
	}
}

// fakeReleaseManifestCheckStore is a test-only combination of
// releasereview.ReleaseManifestCheckInserter (Insert) and
// releasereview.CompositionAnchorUpdater (UpdateCompositionAnchor) -- the
// SAME concrete *postgres.ReleaseManifestCheckStore satisfies both in
// production wiring (controlplane/serve.go), never two separate store
// instances; this fake mirrors that one-store-two-interfaces shape so a
// unit test can prove dispatchCompositionReview's own composition-anchor
// write (migrations/000128_release_manifest_checks_composition_anchor.
// up.sql) end to end, with no real DB.
type fakeReleaseManifestCheckStore struct {
	insertCalls int
	insertedID  pgtype.UUID

	anchorCalls         int
	lastAnchorID        pgtype.UUID
	lastAnchorHeadSHA   string
	lastAnchorTruncated bool
	anchorErr           error
}

func (f *fakeReleaseManifestCheckStore) Insert(_ context.Context, _ sqlcgen.InsertReleaseManifestCheckParams) (sqlcgen.ReleaseManifestCheck, error) {
	f.insertCalls++
	f.insertedID = pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4}, Valid: true}
	return sqlcgen.ReleaseManifestCheck{ID: f.insertedID}, nil
}

func (f *fakeReleaseManifestCheckStore) UpdateCompositionAnchor(_ context.Context, id pgtype.UUID, headSHA string, diffTruncated bool) (sqlcgen.ReleaseManifestCheck, error) {
	f.anchorCalls++
	f.lastAnchorID = id
	f.lastAnchorHeadSHA = headSHA
	f.lastAnchorTruncated = diffTruncated
	if f.anchorErr != nil {
		return sqlcgen.ReleaseManifestCheck{}, f.anchorErr
	}
	return sqlcgen.ReleaseManifestCheck{ID: id}, nil
}

// TestRun_CompositionAnchorRecorded_AtDispatchTime proves the confirmed-
// major auditability fix end to end: once persistReleaseManifestCheck has
// produced a real row id, dispatchCompositionReview records
// composition_head_sha/composition_diff_truncated against that SAME row,
// using the SAME head sha/truncated flag reviewcontext.Fetch itself
// resolved -- never a second, independently-derived value.
func TestRun_CompositionAnchorRecorded_AtDispatchTime(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	diffFetcher := &fakeCompositionDiffFetcher{
		pr:            githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"},
		diff:          "diff",
		diffTruncated: true,
	}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}
	checks := &fakeReleaseManifestCheckStore{}

	releasereview.Run(context.Background(), discardLogger(), releasereview.Deps{
		SourceControl:          lister,
		Outbox:                 outbox,
		ReleaseManifestChecks:  checks,
		CompositionTemplates:   templates,
		CompositionDiffFetcher: diffFetcher,
		CompositionTurns:       turns,
		CompositionDispatch:    dispatch,
		CompositionAnchor:      checks,
		Timeouts:               platform.DefaultTimeouts(),
	}, releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if checks.insertCalls != 1 {
		t.Fatalf("ReleaseManifestChecks.Insert calls = %d, want 1", checks.insertCalls)
	}
	if checks.anchorCalls != 1 {
		t.Fatalf("CompositionAnchor.UpdateCompositionAnchor calls = %d, want 1", checks.anchorCalls)
	}
	if checks.lastAnchorID != checks.insertedID {
		t.Errorf("anchor write id = %v, want the SAME id persistReleaseManifestCheck's own insert produced = %v", checks.lastAnchorID, checks.insertedID)
	}
	if checks.lastAnchorHeadSHA != "deadbeef" {
		t.Errorf("anchor headSHA = %q, want %q", checks.lastAnchorHeadSHA, "deadbeef")
	}
	if !checks.lastAnchorTruncated {
		t.Error("anchor diffTruncated = false, want true (fakeCompositionDiffFetcher.diffTruncated was true)")
	}
}

// TestRun_CompositionAnchorNotConfigured_DegradesGracefully proves
// CompositionAnchor is independently nil-safe (mirrors the other four
// composition-dispatch deps' own identical discipline,
// TestRun_CompositionDepsNotConfigured_DegradesGracefully immediately
// above) -- a caller that wires everything else but not this one
// deliverable still dispatches the composition turn normally, just
// without an anchor recorded.
func TestRun_CompositionAnchorNotConfigured_DegradesGracefully(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}
	templates := &fakeCompositionTemplateFetcher{template: "t"}
	diffFetcher := &fakeCompositionDiffFetcher{pr: githubapi.PullRequest{HeadSHA: "deadbeef", BaseRef: "main"}, diff: "diff"}
	turns := &fakeCompositionTurnInserter{}
	dispatch := &fakeCompositionDispatcher{}
	checks := &fakeReleaseManifestCheckStore{}

	releasereview.Run(context.Background(), discardLogger(), releasereview.Deps{
		SourceControl:          lister,
		Outbox:                 outbox,
		ReleaseManifestChecks:  checks,
		CompositionTemplates:   templates,
		CompositionDiffFetcher: diffFetcher,
		CompositionTurns:       turns,
		CompositionDispatch:    dispatch,
		// CompositionAnchor deliberately left nil.
		Timeouts: platform.DefaultTimeouts(),
	}, releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if turns.calls != 1 {
		t.Fatalf("CompositionTurns.Create calls = %d, want 1 (a nil CompositionAnchor must never block the turn dispatch itself)", turns.calls)
	}
	if checks.anchorCalls != 0 {
		t.Errorf("CompositionAnchor.UpdateCompositionAnchor calls = %d, want 0 (nil dep)", checks.anchorCalls)
	}
}

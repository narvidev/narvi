package releasereview_test

import (
	"context"
	"errors"
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
type fakeCompositionDiffFetcher struct {
	pr      githubapi.PullRequest
	prErr   error
	diff    string
	diffErr error
	prCalls int
}

func (f *fakeCompositionDiffFetcher) GetPullRequest(_ context.Context, _, _ string, _ int32, _ string) (githubapi.PullRequest, error) {
	f.prCalls++
	return f.pr, f.prErr
}

func (f *fakeCompositionDiffFetcher) GetCompareDiff(_ context.Context, _, _, _, _, _ string) (string, bool, error) {
	return f.diff, false, f.diffErr
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
// the core Step 125 mechanism: when ShouldRunAggregateReview fires (here,
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
// of the four composition-dispatch dependencies is independently nil-safe
// -- a caller that doesn't wire Step 125's own deliverable (e.g. every
// EXISTING test in run_test.go, predating this Step) must keep behaving
// exactly as before: the manifest check itself is unaffected, and nothing
// panics.
func TestRun_CompositionDepsNotConfigured_DegradesGracefully(t *testing.T) {
	t.Parallel()

	lister := &fakeMergedPRLister{merged: []ports.MergedPR{
		{Number: 1, Title: "a", HasApprovingReview: true, Labels: []string{reviewpost.LabelHighRisk}},
	}}
	outbox := &fakeOutboxEnqueuer{}

	releasereview.Run(context.Background(), discardLogger(), releasereview.Deps{
		SourceControl: lister,
		Outbox:        outbox,
		Timeouts:      platform.DefaultTimeouts(),
	}, releasereview.Input{
		SessionID: testSessionID(t),
		Owner:     "acme", Repo: "widgets", PRNumber: 1, BaseRef: "main", HeadRef: "release/1.0", Token: "t",
	})

	if outbox.calls != 1 {
		t.Errorf("Outbox.Create calls = %d, want 1 (the manifest check's own outbox comment must still be enqueued)", outbox.calls)
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

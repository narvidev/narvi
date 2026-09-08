package reviewcontext_test

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeArchDecisionsFetcher is a test-only reviewcontext.ArchDecisionsFetcher
// -- no real Postgres connection, mirroring fakeFalsePositiveFetcher's own
// established local-fake precedent in this same test package.
type fakeArchDecisionsFetcher struct {
	gated    []knowledge.Candidate
	gatedErr error

	recent    []knowledge.Candidate
	recentErr error

	gatedCalls  int
	recentCalls int

	// gotExcludePR records what the gate was asked to exclude, so a test
	// can assert the PR under review is passed rather than assumed.
	gotExcludePR int32
}

func (f *fakeArchDecisionsFetcher) ListGatedArchDecisions(_ context.Context, _ string, excludePR int32, _, _ []string, _ int32) ([]knowledge.Candidate, error) {
	f.gatedCalls++
	f.gotExcludePR = excludePR
	if f.gatedErr != nil {
		return nil, f.gatedErr
	}
	return f.gated, nil
}

func (f *fakeArchDecisionsFetcher) ListRecentArchDecisions(_ context.Context, _ string, excludePR int32, _ int32) ([]knowledge.Candidate, error) {
	f.recentCalls++
	f.gotExcludePR = excludePR
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	return f.recent, nil
}

// fakeRanker is a test-only ports.KnowledgeRanker.
type fakeRanker struct {
	name string

	scores []float64
	err    error
	block  bool // if true, Score blocks until ctx is done

	calls int
}

func (r *fakeRanker) Name() string {
	if r.name == "" {
		return "fake"
	}
	return r.name
}

func (r *fakeRanker) Score(ctx context.Context, _ knowledge.Query, _ []knowledge.Candidate) ([]float64, error) {
	r.calls++
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.scores, nil
}

func fastTimeouts() platform.Timeouts {
	return platform.Timeouts{KnowledgeRankerTimeout: 2 * time.Second}
}

func mkCand(id string, createdAt time.Time) knowledge.Candidate {
	return knowledge.Candidate{
		ID:                    id,
		VerdictID:             id,
		RepoFullName:          "acme/widgets",
		Decision:              "decision-" + id,
		RejectedAlternative:   "rejected-" + id,
		ConventionConformance: "conformance-" + id,
		CreatedAt:             createdAt,
	}
}

func TestFetchPriorArchDecisions_NilFetcher_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), nil, &fakeRanker{}, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})
	if block != "" {
		t.Errorf("block = %q, want empty for a nil fetcher", block)
	}
	if !rec.Empty() {
		t.Errorf("rec = %+v, want an empty InjectedRecord for a nil fetcher", rec)
	}
}

func TestFetchPriorArchDecisions_GateHit_RendersAndRecordsPathOverlap(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{gated: []knowledge.Candidate{mkCand("v1:0", time.Now())}}
	ranker := &fakeRanker{name: "recency"}

	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, ranker, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets", Tags: []string{"database"}})

	if !strings.Contains(block, "decision-v1:0") {
		t.Errorf("block = %q, want the gated candidate's own decision text", block)
	}
	if rec.Selector != reviewcontext.SelectorPathOverlap {
		t.Errorf("rec.Selector = %q, want %q", rec.Selector, reviewcontext.SelectorPathOverlap)
	}
	if rec.Ranker != "recency" {
		t.Errorf("rec.Ranker = %q, want %q", rec.Ranker, "recency")
	}
	if rec.Degradation != "" {
		t.Errorf("rec.Degradation = %q, want empty on the ordinary path", rec.Degradation)
	}
	if len(rec.IDs) != 1 || rec.IDs[0] != "v1:0" {
		t.Errorf("rec.IDs = %v, want [v1:0]", rec.IDs)
	}
	if len(rec.ContentHashes) != 1 || rec.ContentHashes[0] == "" {
		t.Errorf("rec.ContentHashes = %v, want one non-empty hash", rec.ContentHashes)
	}
	if fetcher.recentCalls != 0 {
		t.Errorf("recentCalls = %d, want 0 -- the gate hit, the fallback must never run", fetcher.recentCalls)
	}
}

func TestFetchPriorArchDecisions_EmptyOverlap_FallsBackToRecency(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{
		gated:  nil, // empty overlap
		recent: []knowledge.Candidate{mkCand("v2:0", time.Now())},
	}

	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, &fakeRanker{}, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if !strings.Contains(block, "decision-v2:0") {
		t.Errorf("block = %q, want the fallback candidate's own decision text", block)
	}
	if rec.Selector != reviewcontext.SelectorRecencyFallback {
		t.Errorf("rec.Selector = %q, want %q", rec.Selector, reviewcontext.SelectorRecencyFallback)
	}
	if fetcher.gatedCalls != 1 || fetcher.recentCalls != 1 {
		t.Errorf("gatedCalls=%d recentCalls=%d, want exactly one call to each", fetcher.gatedCalls, fetcher.recentCalls)
	}
}

func TestFetchPriorArchDecisions_BothEmpty_NoHistoryNoDegradation(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{}
	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, &fakeRanker{}, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if block != "" {
		t.Errorf("block = %q, want empty when the repo genuinely has no gated or recent history", block)
	}
	if rec.Degradation != "" {
		t.Errorf("rec.Degradation = %q, want empty -- an honestly empty repo history is not a degradation", rec.Degradation)
	}
	if !rec.Empty() {
		t.Errorf("rec = %+v, want Empty() true", rec)
	}
}

func TestFetchPriorArchDecisions_GateFetchError_DegradesToEmptyBlock(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{gatedErr: errors.New("db exploded")}
	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, &fakeRanker{}, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if block != "" {
		t.Errorf("block = %q, want empty on a gate fetch error", block)
	}
	if rec.Degradation != reviewcontext.DegradationFetchError {
		t.Errorf("rec.Degradation = %q, want %q", rec.Degradation, reviewcontext.DegradationFetchError)
	}
	if fetcher.recentCalls != 0 {
		t.Errorf("recentCalls = %d, want 0 -- a gate error must not fall through to the recency fallback", fetcher.recentCalls)
	}
}

func TestFetchPriorArchDecisions_FallbackFetchError_DegradesToEmptyBlock(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{recentErr: errors.New("db exploded")}
	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, &fakeRanker{}, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if block != "" {
		t.Errorf("block = %q, want empty on a fallback fetch error", block)
	}
	if rec.Degradation != reviewcontext.DegradationFetchError {
		t.Errorf("rec.Degradation = %q, want %q", rec.Degradation, reviewcontext.DegradationFetchError)
	}
}

// TestFetchPriorArchDecisions_RankerError_KeepsGateOrderAndRecordsDegradation
// is §34.7/ports.KnowledgeRanker's own "degrades to the gate order, never
// to empty" contract, exercised end to end.
func TestFetchPriorArchDecisions_RankerError_KeepsGateOrderAndRecordsDegradation(t *testing.T) {
	t.Parallel()

	now := time.Now()
	fetcher := &fakeArchDecisionsFetcher{gated: []knowledge.Candidate{mkCand("v1:0", now), mkCand("v2:0", now.Add(-time.Hour))}}
	ranker := &fakeRanker{err: errors.New("ranker blew up")}

	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, ranker, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if block == "" {
		t.Fatal("block is empty, want the gate's own candidates still rendered despite the ranker error")
	}
	if rec.Degradation != reviewcontext.DegradationRankerError {
		t.Errorf("rec.Degradation = %q, want %q", rec.Degradation, reviewcontext.DegradationRankerError)
	}
	// Gate order preserved: v1:0 (newer) before v2:0 (older).
	if rec.IDs[0] != "v1:0" || rec.IDs[1] != "v2:0" {
		t.Errorf("rec.IDs = %v, want the gate's own order [v1:0 v2:0] preserved on ranker error", rec.IDs)
	}
}

// TestFetchPriorArchDecisions_RankerTimeout_KeepsGateOrderAndRecordsTimeout
// exercises the OTHER named degradation: a ranker that never returns
// within timeouts.KnowledgeRankerTimeout.
func TestFetchPriorArchDecisions_RankerTimeout_KeepsGateOrderAndRecordsTimeout(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{gated: []knowledge.Candidate{mkCand("v1:0", time.Now())}}
	ranker := &fakeRanker{block: true}
	timeouts := platform.Timeouts{KnowledgeRankerTimeout: 20 * time.Millisecond}

	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, ranker, timeouts, knowledge.Query{RepoFullName: "acme/widgets"})

	if block == "" {
		t.Fatal("block is empty, want the gate's own candidates still rendered despite the ranker timeout")
	}
	if rec.Degradation != reviewcontext.DegradationRankerTimeout {
		t.Errorf("rec.Degradation = %q, want %q", rec.Degradation, reviewcontext.DegradationRankerTimeout)
	}
}

// TestFetchPriorArchDecisions_InvalidScores_KeepsGateOrderAndRecordsDegradation
// exercises OrderByScores' own rejection path (a length mismatch, in this
// case) as seen through this function.
func TestFetchPriorArchDecisions_InvalidScores_KeepsGateOrderAndRecordsDegradation(t *testing.T) {
	t.Parallel()

	fetcher := &fakeArchDecisionsFetcher{gated: []knowledge.Candidate{mkCand("v1:0", time.Now()), mkCand("v2:0", time.Now())}}
	ranker := &fakeRanker{scores: []float64{1, math.NaN()}} // NaN -> ErrInvalidScores

	block, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, ranker, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if block == "" {
		t.Fatal("block is empty, want the gate's own candidates still rendered despite invalid scores")
	}
	if rec.Degradation != reviewcontext.DegradationInvalidScores {
		t.Errorf("rec.Degradation = %q, want %q", rec.Degradation, reviewcontext.DegradationInvalidScores)
	}
}

// TestFetchPriorArchDecisions_CapAppliedAfterOrdering proves TakeTop runs
// AFTER the ranker's own reordering, never before: a candidate the gate
// placed LAST (oldest) is ranked FIRST by the fake ranker, and must
// survive a cap that would otherwise have excluded it by position alone.
//
// Mutation-verified: temporarily swapping this function's own
// `top := knowledge.TakeTop(ordered, knowledge.MaxInjected)` for
// `top := knowledge.TakeTop(cands, knowledge.MaxInjected)` (capping the
// GATE's pre-ranked order instead of the ranker's own) left `ordered`
// unused, which go vet/the compiler themselves reject -- an even
// stronger catch than a runtime assertion failure, since that mutation
// could never ship at all. Reverted after confirming the build failure.
func TestFetchPriorArchDecisions_CapAppliedAfterOrdering(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// knowledge.MaxInjected is 12; build 13 gated candidates so a
	// position-based (pre-rank) cap would drop the last one.
	cands := make([]knowledge.Candidate, 13)
	scores := make([]float64, 13)
	for i := 0; i < 13; i++ {
		cands[i] = mkCand("v"+string(rune('a'+i))+":0", now.Add(-time.Duration(i)*time.Minute))
		scores[i] = float64(i) // ascending: index 0 (gate's own FIRST/newest) scores LOWEST
	}
	// The gate's own LAST (oldest, index 12) candidate gets the HIGHEST
	// score, so a rank-then-cap pipeline must keep it; a cap-then-rank
	// pipeline would have already dropped it before the ranker ever saw it.
	fetcher := &fakeArchDecisionsFetcher{gated: cands}
	ranker := &fakeRanker{scores: scores}

	_, rec := reviewcontext.FetchPriorArchDecisions(context.Background(), discardLogger(), fetcher, ranker, fastTimeouts(), knowledge.Query{RepoFullName: "acme/widgets"})

	if len(rec.IDs) != knowledge.MaxInjected {
		t.Fatalf("len(rec.IDs) = %d, want %d", len(rec.IDs), knowledge.MaxInjected)
	}
	wantTopID := cands[12].ID
	if rec.IDs[0] != wantTopID {
		t.Errorf("rec.IDs[0] = %q, want %q (the highest-scored candidate, regardless of the gate's own pre-rank position)", rec.IDs[0], wantTopID)
	}
}

// TestFetchPriorArchDecisions_ExcludesThePRUnderReview pins the property
// three independent adversarial lenses raised about this pipeline.
//
// The block is "prior architecture decisions from this repository". A
// PR's own earlier verdict is not that: it is the same review's first
// pass. And it is not a rare match but the most likely one, because a
// re-review derives its tags and roots from the same changed paths that
// stamped that verdict, so the overlap is near-certain and recency puts
// it first.
//
// Two things go wrong without the exclusion, and the second is why this
// is a test rather than a note. A re-review, whose whole purpose is to
// reconsider after a push, is handed its own first-pass conclusions and
// biased toward agreeing with itself. And the verdict it then produces
// is stamped knowledge-influenced on pure self-reference -- entering the
// population a later Step would ingest, and the one the phase KPI joins
// contestation against, so the measurement would report something other
// than what it claims.
func TestFetchPriorArchDecisions_ExcludesThePRUnderReview(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fetcher *fakeArchDecisionsFetcher
		query   knowledge.Query
	}{
		{
			name:    "gate path",
			fetcher: &fakeArchDecisionsFetcher{gated: []knowledge.Candidate{{ID: "v1:0", Decision: "use a queue"}}},
			query:   knowledge.Query{RepoFullName: "o/r", Tags: []string{"api"}, PRNumber: 42},
		},
		{
			name:    "recency fallback path",
			fetcher: &fakeArchDecisionsFetcher{recent: []knowledge.Candidate{{ID: "v1:0", Decision: "use a queue"}}},
			query:   knowledge.Query{RepoFullName: "o/r", PRNumber: 42},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _ = reviewcontext.FetchPriorArchDecisions(t.Context(), slog.New(slog.DiscardHandler),
				tt.fetcher, knowledge.RecencyRanker{}, platform.DefaultTimeouts(), tt.query)

			if tt.fetcher.gotExcludePR != 42 {
				t.Errorf("gate asked to exclude PR %d, want 42 -- the PR under review must never be its own prior decision",
					tt.fetcher.gotExcludePR)
			}
		})
	}
}

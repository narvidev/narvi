package reviewcontext

import (
	"context"
	"errors"
	"log/slog"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/platform"
)

// This file implements §31.6 item 1's own "prior architecture decisions"
// block -- the flagship consumer of the knowledge-retrieval seam: the
// ranking half already lives in internal/domain/knowledge and
// internal/app/ports.KnowledgeRanker; the gate half lives in
// *postgres.ReviewVerdictStore's own ListGatedArchDecisions/
// ListRecentArchDecisions (reviewverdictarchdecisions.go).
// FetchPriorArchDecisions is this package's OWN impure fetch, exact
// sibling of FetchFalsePositivePatterns/FetchAlreadyAnswered: "impure
// fetch here, pure render in a sibling domain package" (knowledge.
// RenderPriorDecisionsBlock is the pure render half this function calls).

// gateCandidatePoolSize is the SQL gate's own LIMIT k (§31.6: "Selector
// v1 = one sqlc query: ... ORDER BY created_at DESC LIMIT k") -- a WIDER
// pool than knowledge.MaxInjected (12), the cap TakeTop applies AFTER
// ranking. Mode A's RecencyRanker never reorders, so its own final
// result is exactly the newest MaxInjected of this pool regardless of
// pool size, but a future ranker (mode B's hybrid RRF) needs real
// candidates beyond pure recency to re-rank at all. 30 mirrors §31.6's
// own illustrative figure for mode A's pre-gate, inject-all-window
// behavior ("at a few PRs/week, a 30-decision window covers months and
// recency approximates relevance").
const gateCandidatePoolSize int32 = 30

// ArchDecisionsFetcher is the narrow slice of *postgres.ReviewVerdictStore
// this function needs -- exact sibling of FalsePositivePatternsFetcher/
// FindingsFetcher (falsepositive.go/alreadyanswered.go, this same
// package): a small, locally-defined interface so a unit test can inject
// a fake with no real Postgres connection. Both methods return
// []knowledge.Candidate directly, never a raw row -- see
// docs/design/boundaries-design.md, section 2.2, and
// *postgres.ReviewVerdictStore's own reviewverdictarchdecisions.go for
// why the row -> Candidate conversion (one row can hold more than one
// decision) belongs in that adapter, never here.
type ArchDecisionsFetcher interface {
	// ListGatedArchDecisions is the GATE (§31.6): verdicts whose
	// INSERT-time tags/roots overlap (tags, roots), live-epoch only,
	// uncontested, newest first, LIMIT k. Mode-invariant.
	ListGatedArchDecisions(ctx context.Context, repoFullName string, tags, roots []string, k int32) ([]knowledge.Candidate, error)
	// ListRecentArchDecisions is the SAME gate's own recency fallback,
	// fired on empty overlap -- no tag/root predicate, same exclusions.
	ListRecentArchDecisions(ctx context.Context, repoFullName string, k int32) ([]knowledge.Candidate, error)
}

// Selector values recorded on knowledge.InjectedRecord.Selector -- which
// gate branch actually produced this fetch's own candidates, per §31.6's
// own "the injected-ids record's selector field records which membership
// rule actually ran, in both modes".
const (
	SelectorPathOverlap     = "path-overlap"
	SelectorRecencyFallback = "recency-fallback"
)

// Degradation reasons recorded on knowledge.InjectedRecord.Degradation --
// every one of them keeps the gate's own order rather than emptying the
// block, per §34.7/ports.KnowledgeRanker's own "degrades to the gate
// order, never to empty" contract.
const (
	DegradationFetchError    = "fetch-error"
	DegradationRankerError   = "ranker-error"
	DegradationRankerTimeout = "ranker-timeout"
	DegradationInvalidScores = "invalid-scores"
)

// FetchPriorArchDecisions is §31.6 item 1's own gate -> rank -> order ->
// take top N -> render -> record sequence. Best-effort, exactly like
// FetchFalsePositivePatterns/FetchAlreadyAnswered: never returns an error
// of its own -- a fetch failure degrades to an empty block (logged),
// with rec.Degradation set to DegradationFetchError so the caller's own
// turn-creation stamp records the degradation rather than silently
// looking like an ordinary zero-candidate turn.
//
// Sequencing, exactly as docs/design/boundaries-design.md, section 2.2
// specifies: ListGatedArchDecisions first; if it returns zero candidates,
// ListRecentArchDecisions (the recency fallback) with
// rec.Selector = SelectorRecencyFallback; ranker.Score, under
// context.WithTimeout(ctx, timeouts.KnowledgeRankerTimeout); on a ranker
// error, timeout, or an OrderByScores rejection (ErrInvalidScores), the
// GATE's own order is kept -- never empty -- and the degradation is
// recorded; knowledge.OrderByScores; knowledge.TakeTop(MaxInjected),
// applied AFTER ordering so a ranker can never raise the cap;
// knowledge.RenderPriorDecisionsBlock last.
//
// ranker == nil (a caller/test that never wires one) degrades to
// knowledge.RecencyRanker{} -- the SAME public default the composition
// root wires when no module is composed -- rather than panicking on a
// nil interface's Name()/Score() call.
//
// The caller is responsible for PREPENDING this function's own returned
// block to basePrompt BEFORE calling review.RenderTurnPrompt, exactly
// like FetchFalsePositivePatterns/FetchAlreadyAnswered's own identical
// division of responsibility, AND for persisting rec verbatim onto the
// creating turn's own turns.review_knowledge_decision (§31.6's own
// durable-record requirement) -- this function itself never touches
// Postgres beyond fetcher/ranker, and never touches basePrompt.
func FetchPriorArchDecisions(ctx context.Context, logger *slog.Logger, fetcher ArchDecisionsFetcher, ranker ports.KnowledgeRanker, timeouts platform.Timeouts, q knowledge.Query) (string, knowledge.InjectedRecord) {
	if fetcher == nil {
		return "", knowledge.InjectedRecord{}
	}
	if ranker == nil {
		ranker = knowledge.RecencyRanker{}
	}

	cands, err := fetcher.ListGatedArchDecisions(ctx, q.RepoFullName, q.Tags, q.Roots, gateCandidatePoolSize)
	if err != nil {
		logger.Warn("reviewcontext: fetch gated arch decisions failed, review turn will carry no prior-decisions block",
			"error", err, "repo_full_name", q.RepoFullName)
		return "", knowledge.InjectedRecord{Degradation: DegradationFetchError}
	}

	selector := SelectorPathOverlap
	if len(cands) == 0 {
		selector = SelectorRecencyFallback
		cands, err = fetcher.ListRecentArchDecisions(ctx, q.RepoFullName, gateCandidatePoolSize)
		if err != nil {
			logger.Warn("reviewcontext: fetch recent arch decisions (recency fallback) failed, review turn will carry no prior-decisions block",
				"error", err, "repo_full_name", q.RepoFullName)
			return "", knowledge.InjectedRecord{Degradation: DegradationFetchError}
		}
	}

	if len(cands) == 0 {
		// Nothing gated, nothing recent -- an honestly empty repository
		// history, not a degradation: rendering "" is the correct block
		// for "no history exists yet", mirroring FetchFalsePositivePatterns'
		// own identical empty-repo posture. Selector/Ranker are still
		// recorded (informational: which branch was tried), but
		// InjectedRecord.Empty() is true, so knowledge_influenced stays
		// false for this turn.
		return "", knowledge.InjectedRecord{Selector: selector, Ranker: ranker.Name()}
	}

	ordered := cands
	degradation := ""
	rankCtx, cancel := context.WithTimeout(ctx, timeouts.KnowledgeRankerTimeout)
	scores, scoreErr := ranker.Score(rankCtx, q, cands)
	rankTimedOut := errors.Is(rankCtx.Err(), context.DeadlineExceeded)
	cancel()
	switch {
	case scoreErr != nil:
		if rankTimedOut {
			degradation = DegradationRankerTimeout
		} else {
			degradation = DegradationRankerError
		}
		logger.Warn("reviewcontext: knowledge ranker failed, falling back to the gate's own order",
			"error", scoreErr, "ranker", ranker.Name(), "repo_full_name", q.RepoFullName)
	default:
		var orderErr error
		ordered, orderErr = knowledge.OrderByScores(cands, scores)
		if orderErr != nil {
			degradation = DegradationInvalidScores
			ordered = cands
			logger.Warn("reviewcontext: knowledge ranker returned invalid scores, falling back to the gate's own order",
				"error", orderErr, "ranker", ranker.Name(), "repo_full_name", q.RepoFullName)
		}
	}

	top := knowledge.TakeTop(ordered, knowledge.MaxInjected)

	ids := make([]string, len(top))
	hashes := make([]string, len(top))
	for i, c := range top {
		ids[i] = c.ID
		hashes[i] = c.ContentHash()
	}

	rec := knowledge.InjectedRecord{
		IDs:           ids,
		ContentHashes: hashes,
		Selector:      selector,
		Ranker:        ranker.Name(),
		Degradation:   degradation,
	}
	return knowledge.RenderPriorDecisionsBlock(top), rec
}

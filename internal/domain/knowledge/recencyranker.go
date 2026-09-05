package knowledge

import "context"

// RecencyRanker is the public product's own KnowledgeRanker
// (internal/app/ports/knowledgeranker.go): no opinion, ever -- Score
// always returns (nil, nil), which OrderByScores treats as "keep the
// gate's own order" (its own ORDER BY created_at DESC). This is the
// seam's own proof that the public product needs no private ranking
// substrate at all: wiring RecencyRanker in is the entire difference
// between "no module composed" and a module that supplies its own
// KnowledgeRanker.
//
// A zero-value RecencyRanker{} is complete and ready to use -- it holds
// no state, per its own "no opinion" contract.
type RecencyRanker struct{}

// Name identifies this ranker as "recency".
func (RecencyRanker) Name() string { return "recency" }

// Score always returns (nil, nil): RecencyRanker never scores anything,
// it only ever defers to whatever order its caller already had.
func (RecencyRanker) Score(context.Context, Query, []Candidate) ([]float64, error) {
	return nil, nil
}

package knowledge_test

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// TestRecencyRanker_NoOpinion is design note section 2.5's own named
// test: RecencyRanker always reports (nil, nil) regardless of what it is
// asked to score, which OrderByScores treats as "keep the caller's own
// order" -- the seam's proof that the public product needs no ranking
// substrate of its own.
func TestRecencyRanker_NoOpinion(t *testing.T) {
	t.Parallel()

	r := knowledge.RecencyRanker{}

	if got, want := r.Name(), "recency"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	tests := []struct {
		name  string
		query knowledge.Query
		cands []knowledge.Candidate
	}{
		{"empty candidates", knowledge.Query{}, nil},
		{
			"non-empty candidates and a populated query",
			knowledge.Query{RepoFullName: "acme/widgets", Tags: []string{"auth"}},
			mkCandidates("a", "b", "c"),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scores, err := r.Score(context.Background(), tt.query, tt.cands)
			if err != nil {
				t.Fatalf("Score() error = %v, want nil", err)
			}
			if scores != nil {
				t.Errorf("Score() = %v, want nil (no opinion)", scores)
			}
		})
	}
}

package knowledge_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// TestCloneForRanking_SeversEveryWriteChannel is the regression test for
// the hole "scores, never candidates" does not close: a ranker holding a
// slice header can rewrite what the caller still holds.
//
// Every mutation below was demonstrated against the un-cloned path before
// this helper existed -- a ranker writing cands[i].Decision and
// cands[i].Tags[0] rewrote the caller's own candidates -- so each row is a
// reproduction kept as a test, not a hypothetical.
func TestCloneForRanking_SeversEveryWriteChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(q *knowledge.Query, cands []knowledge.Candidate)
		verify func(t *testing.T, q knowledge.Query, cands []knowledge.Candidate)
	}{
		{
			name: "candidate prose",
			mutate: func(_ *knowledge.Query, c []knowledge.Candidate) {
				c[0].Decision = "INJECTED"
				c[0].RejectedAlternative = "INJECTED"
				c[0].ConventionConformance = "INJECTED"
			},
			verify: func(t *testing.T, _ knowledge.Query, c []knowledge.Candidate) {
				if c[0].Decision != "real" || c[0].RejectedAlternative != "alt" || c[0].ConventionConformance != "conv" {
					t.Errorf("caller's prose rewritten: %+v", c[0])
				}
			},
		},
		{
			name: "candidate tag and root backing arrays",
			mutate: func(_ *knowledge.Query, c []knowledge.Candidate) {
				c[0].Tags[0] = "INJECTED"
				c[0].Roots[0] = "INJECTED"
			},
			verify: func(t *testing.T, _ knowledge.Query, c []knowledge.Candidate) {
				if c[0].Tags[0] != "api" || c[0].Roots[0] != "internal" {
					t.Errorf("caller's tags/roots rewritten: %v %v", c[0].Tags, c[0].Roots)
				}
			},
		},
		{
			name: "query backing arrays",
			mutate: func(q *knowledge.Query, _ []knowledge.Candidate) {
				q.Tags[0] = "INJECTED"
				q.Roots[0] = "INJECTED"
				q.ChangedPaths[0] = "INJECTED"
			},
			verify: func(t *testing.T, q knowledge.Query, _ []knowledge.Candidate) {
				if q.Tags[0] != "api" || q.Roots[0] != "internal" || q.ChangedPaths[0] != "a.go" {
					t.Errorf("caller's query rewritten: %+v", q)
				}
			},
		},
		{
			name: "appending to the candidate slice",
			mutate: func(_ *knowledge.Query, c []knowledge.Candidate) {
				_ = append(c[:1], knowledge.Candidate{ID: "INJECTED"}) //nolint:gocritic // deliberately writing through the header
			},
			verify: func(t *testing.T, _ knowledge.Query, c []knowledge.Candidate) {
				if len(c) != 2 || c[1].ID != "b" {
					t.Errorf("caller's slice clobbered: %+v", c)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q := knowledge.Query{
				RepoFullName: "o/r",
				Tags:         []string{"api"},
				Roots:        []string{"internal"},
				ChangedPaths: []string{"a.go"},
			}
			cands := []knowledge.Candidate{
				{ID: "a", Decision: "real", RejectedAlternative: "alt", ConventionConformance: "conv", Tags: []string{"api"}, Roots: []string{"internal"}},
				{ID: "b"},
			}

			cq, ccands := knowledge.CloneForRanking(q, cands)
			tt.mutate(&cq, ccands)
			tt.verify(t, q, cands)
		})
	}
}

// TestCloneForRanking_PreservesNil keeps the copy indistinguishable from
// the original to a caller that checks for nil, so adding the clone can
// never change a downstream branch.
func TestCloneForRanking_PreservesNil(t *testing.T) {
	t.Parallel()

	q, cands := knowledge.CloneForRanking(knowledge.Query{}, []knowledge.Candidate{{ID: "a"}})
	if q.Tags != nil || q.Roots != nil || q.ChangedPaths != nil {
		t.Errorf("nil query slices became non-nil: %+v", q)
	}
	if cands[0].Tags != nil || cands[0].Roots != nil {
		t.Errorf("nil candidate slices became non-nil: %+v", cands[0])
	}
}

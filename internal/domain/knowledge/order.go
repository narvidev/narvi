package knowledge

import (
	"errors"
	"math"
	"sort"
)

// ErrInvalidScores is OrderByScores' own rejection of a non-nil scores
// slice it cannot use: one whose length does not match cands, or that
// contains a NaN or infinite value. Either shape would make "descending
// by score" meaningless -- there is no well-defined order for a NaN
// against anything, including itself, and neither +Inf nor -Inf survives
// arithmetic (a future ranker combining more than one signal into a
// single score) the way an ordinary float64 does.
var ErrInvalidScores = errors.New("knowledge: invalid scores")

// OrderByScores returns cands reordered by descending score, stable on
// ties -- so candidates the ranker scored identically keep the relative
// order the CALLER gave them in, which is how the gate's own recency
// ordering survives among equally-scored candidates rather than being
// scrambled by an opinionless or coarse-grained ranker.
//
// scores == nil means "no opinion": cands is returned unchanged, exactly
// as given -- this is what lets RecencyRanker (Score always returns
// nil, nil) add nothing to the gate's own order. A non-nil scores whose
// length does not match cands, or that contains any NaN/Inf entry, is
// ErrInvalidScores; the caller is expected to fall back to cands as
// given and record the degradation (a later Step's own concern -- this
// function itself only ever reports the error, it does not degrade on
// its own behalf).
//
// The result can never contain an element absent from cands, BY
// CONSTRUCTION: this function builds an index permutation over
// [0, len(cands)) and reads cands through it -- there is no code path
// here that constructs, copies field-by-field, or otherwise fabricates a
// Candidate value. A caller (or a future maintainer "helpfully" editing
// this function) cannot make it return anything other than a reordering
// of exactly the candidates it was given -- see this package's own
// TestOrderByScores_ResultIsPermutationOfInput, which verifies multiset
// equality by ID and would fail if that ever stopped being true.
func OrderByScores(cands []Candidate, scores []float64) ([]Candidate, error) {
	if scores == nil {
		return cands, nil
	}
	if len(scores) != len(cands) {
		return nil, ErrInvalidScores
	}
	for _, s := range scores {
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return nil, ErrInvalidScores
		}
	}

	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		return scores[idx[i]] > scores[idx[j]]
	})

	ordered := make([]Candidate, len(cands))
	for i, j := range idx {
		ordered[i] = cands[j]
	}
	return ordered, nil
}

// TakeTop returns the first n of cands -- cands already ordered by the
// caller, typically OrderByScores' own result -- applying MaxInjected
// (or any other cap) AFTER ordering, never before: the ranker that
// produced cands' own order had no say in n and cannot raise it. n <= 0
// returns an empty, non-nil slice; n >= len(cands) returns cands
// unchanged.
func TakeTop(cands []Candidate, n int) []Candidate {
	if n <= 0 {
		return []Candidate{}
	}
	if n >= len(cands) {
		return cands
	}
	return cands[:n]
}

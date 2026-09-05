package knowledge_test

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"

	"github.com/narvidev/narvi/internal/domain/knowledge"
)

// mkCandidates builds candidates carrying only an ID -- every test in this
// file cares about WHICH candidates come back and in what order, never
// about any other field.
func mkCandidates(ids ...string) []knowledge.Candidate {
	cands := make([]knowledge.Candidate, len(ids))
	for i, id := range ids {
		cands[i] = knowledge.Candidate{ID: id}
	}
	return cands
}

func idsOf(cands []knowledge.Candidate) []string {
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
	}
	return ids
}

// TestOrderByScores covers every branch design note section 2.5 names:
// nil scores keep order, equal scores keep order (stability), a length
// mismatch and a NaN/Inf entry both reject with ErrInvalidScores.
func TestOrderByScores(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cands   []knowledge.Candidate
		scores  []float64
		wantIDs []string // ignored when wantErr is set
		wantErr error
	}{
		{
			name:    "nil scores keeps cands in their original order",
			cands:   mkCandidates("a", "b", "c"),
			scores:  nil,
			wantIDs: []string{"a", "b", "c"},
		},
		{
			name:    "descending by score",
			cands:   mkCandidates("a", "b", "c"),
			scores:  []float64{0.1, 0.9, 0.5},
			wantIDs: []string{"b", "c", "a"},
		},
		{
			name:    "all-equal scores are stable: original order preserved among ties",
			cands:   mkCandidates("a", "b", "c", "d"),
			scores:  []float64{1, 1, 1, 1},
			wantIDs: []string{"a", "b", "c", "d"},
		},
		{
			name:    "a tied subgroup stays in its own original relative order",
			cands:   mkCandidates("a", "b", "c", "d"),
			scores:  []float64{0, 5, 5, 1},
			wantIDs: []string{"b", "c", "d", "a"},
		},
		{
			name:    "negative scores order the same as positive ones",
			cands:   mkCandidates("a", "b", "c"),
			scores:  []float64{-1, -3, -2},
			wantIDs: []string{"a", "c", "b"},
		},
		{
			name:    "empty cands, nil scores",
			cands:   nil,
			scores:  nil,
			wantIDs: []string{},
		},
		{
			name:    "empty cands, empty scores",
			cands:   []knowledge.Candidate{},
			scores:  []float64{},
			wantIDs: []string{},
		},
		{
			name:    "length mismatch: too few scores",
			cands:   mkCandidates("a", "b"),
			scores:  []float64{1},
			wantErr: knowledge.ErrInvalidScores,
		},
		{
			name:    "length mismatch: too many scores",
			cands:   mkCandidates("a", "b"),
			scores:  []float64{1, 2, 3},
			wantErr: knowledge.ErrInvalidScores,
		},
		{
			name:    "length mismatch: scores on empty cands",
			cands:   nil,
			scores:  []float64{1},
			wantErr: knowledge.ErrInvalidScores,
		},
		{
			name:    "NaN score",
			cands:   mkCandidates("a", "b"),
			scores:  []float64{1, math.NaN()},
			wantErr: knowledge.ErrInvalidScores,
		},
		{
			name:    "positive infinity score",
			cands:   mkCandidates("a", "b"),
			scores:  []float64{1, math.Inf(1)},
			wantErr: knowledge.ErrInvalidScores,
		},
		{
			name:    "negative infinity score",
			cands:   mkCandidates("a", "b"),
			scores:  []float64{1, math.Inf(-1)},
			wantErr: knowledge.ErrInvalidScores,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := knowledge.OrderByScores(tt.cands, tt.scores)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("OrderByScores() error = %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("OrderByScores() = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("OrderByScores() unexpected error: %v", err)
			}
			if got, want := idsOf(got), tt.wantIDs; !reflect.DeepEqual(got, want) {
				t.Errorf("OrderByScores() ids = %v, want %v", got, want)
			}
		})
	}
}

// TestOrderByScores_NilScoresReturnsSameSlice pins "unchanged" literally,
// not merely "equal": scores == nil must hand back the very slice it was
// given, per its own doc comment, not a copy that merely happens to
// contain the same elements in the same order.
func TestOrderByScores_NilScoresReturnsSameSlice(t *testing.T) {
	t.Parallel()

	cands := mkCandidates("a", "b", "c")
	got, err := knowledge.OrderByScores(cands, nil)
	if err != nil {
		t.Fatalf("OrderByScores() unexpected error: %v", err)
	}
	if len(got) == 0 || len(cands) == 0 {
		t.Fatal("test setup broken: cands must be non-empty")
	}
	if &got[0] != &cands[0] {
		t.Error("OrderByScores(cands, nil) returned a different slice, want the exact same one back")
	}
}

// TestOrderByScores_ResultIsPermutationOfInput is design note section
// 2.5's own "the result is a permutation of the input (assert multiset
// equality by ID)" test, and technical plan §34.7's "no return channel"
// property made executable: OrderByScores must never emit a Candidate
// absent from its input, and must never drop one either.
//
// Mutation-verified: temporarily changing OrderByScores (order.go) to
// append an extra Candidate{ID: "fabricated"} not present in cands to its
// result made this fail with a mismatched-multiset error, exactly as
// expected; the change was then reverted byte-for-byte and this test was
// re-run green.
func TestOrderByScores_ResultIsPermutationOfInput(t *testing.T) {
	t.Parallel()

	cands := mkCandidates("a", "b", "c", "d", "e")
	scores := []float64{3, 1, 4, 1, 5}

	got, err := knowledge.OrderByScores(cands, scores)
	if err != nil {
		t.Fatalf("OrderByScores() unexpected error: %v", err)
	}

	wantIDs := idsOf(cands)
	gotIDs := idsOf(got)
	sort.Strings(wantIDs)
	sort.Strings(gotIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("OrderByScores() ids (sorted) = %v, want exactly the input's own multiset (sorted) %v -- raw result was %v", gotIDs, wantIDs, idsOf(got))
	}
}

// TestTakeTop covers TakeTop's own boundary shapes: negative and zero n,
// n inside the slice, n exactly at the boundary, and n past the end.
func TestTakeTop(t *testing.T) {
	t.Parallel()

	cands := mkCandidates("a", "b", "c")

	tests := []struct {
		name    string
		n       int
		wantIDs []string
	}{
		{"negative n", -1, []string{}},
		{"zero n", 0, []string{}},
		{"n less than len", 2, []string{"a", "b"}},
		{"n equal to len", 3, []string{"a", "b", "c"}},
		{"n greater than len", 10, []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := knowledge.TakeTop(cands, tt.n)
			if gotIDs := idsOf(got); !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("TakeTop(cands, %d) ids = %v, want %v", tt.n, gotIDs, tt.wantIDs)
			}
		})
	}
}

// TestMaxInjected pins the cap's own value: a future change to it is a
// deliberate, reviewable edit, not an accidental one this test lets slip
// by silently.
func TestMaxInjected(t *testing.T) {
	t.Parallel()
	if knowledge.MaxInjected != 12 {
		t.Errorf("MaxInjected = %d, want 12 (design note section 2.2)", knowledge.MaxInjected)
	}
}

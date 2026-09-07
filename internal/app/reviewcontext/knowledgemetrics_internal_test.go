package reviewcontext

import "testing"

// TestEstimateTokens pins the len(block)/4 heuristic directly -- an
// internal test (package reviewcontext, not reviewcontext_test) since
// estimateTokens is unexported by design (§31.2's own gauge is the only
// consumer; this is not a general-purpose utility other packages should
// reach for).
//
// Mutation-verified: temporarily changing the divisor from 4 to 3 made
// this test's exact-value assertions fail; reverted after confirming the
// failure.
func TestEstimateTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"empty string", "", 0},
		{"shorter than one token", "abc", 0},
		{"exactly one token", "abcd", 1},
		{"eight bytes, two tokens", "abcdefgh", 2},
		{"rounds down on a partial final token", "abcdefghi", 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := estimateTokens(tt.in); got != tt.want {
				t.Errorf("estimateTokens(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

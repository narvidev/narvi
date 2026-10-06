package sessionguard_test

import (
	"errors"
	"math"
	"testing"

	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// TestValidateSessionSpendCap is the shared validator's table: nil clears
// the cap; a cap must be a finite number above zero with at most two
// decimal places, no larger than NUMERIC(10, 2) holds. NaN and the
// infinities are refused, as the columns' CHECK constraints refuse them.
func TestValidateSessionSpendCap(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name  string
		v     *float64
		valid bool
	}{
		{"nil clears the cap", nil, true},
		{"a cent", f(0.01), true},
		{"a round amount", f(25), true},
		{"two decimals", f(24.99), true},
		{"one decimal", f(0.1), true},
		{"the largest cap", f(99_999_999.99), true},
		{"zero", f(0), false},
		{"negative zero", f(math.Copysign(0, -1)), false},
		{"negative", f(-1), false},
		{"three decimals", f(1.005), false},
		{"below a cent", f(0.00001), false},
		{"above the largest cap", f(100_000_000), false},
		{"NaN", f(math.NaN()), false},
		{"positive infinity", f(math.Inf(1)), false},
		{"negative infinity", f(math.Inf(-1)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := sessionguard.ValidateSessionSpendCap(tc.v)
			if tc.valid && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.valid && !errors.Is(err, sessionguard.ErrInvalidSessionSpendCap) {
				t.Fatalf("err = %v, want ErrInvalidSessionSpendCap", err)
			}
		})
	}
}

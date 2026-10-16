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

// TestParseSessionSpendCap: a cap as a request writes it, a JSON number
// literal, is decided on the decimal it spells -- never on the float it
// parses to, which would round away digits a float64 cannot hold and store
// a cap nobody wrote. An exponent is read exactly; trailing zeros are no
// decimal places; anything that is not a JSON number is refused.
func TestParseSessionSpendCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		literal string
		want    float64 // 0 when refused
	}{
		{"25", 25},
		{"25.5", 25.5},
		{"0.01", 0.01},
		{"0.1", 0.1},
		{"0.29", 0.29},
		{"12.300", 12.3},
		{"2.50e1", 25},
		{"1E-2", 0.01},
		{"1e2", 100},
		{"0.5e1", 5},
		{"1.0e-1", 0.1},
		{"99999999.99", 99_999_999.99},
		{"9999999999e-2", 99_999_999.99},
		// More than two decimal places, however the float rounds them.
		{"99999999.989999999", 0},
		{"1.10000000000000008881784197001", 0},
		{"0.10000000000000000001", 0},
		{"1.100000000000000001", 0},
		{"0.30000000000000004", 0},
		{"1.005", 0},
		{"0.001", 0},
		{"1e-3", 0},
		{"5e-324", 0},
		{"1e-400", 0},
		{"99999999.995", 0},
		// Zero or less.
		{"0", 0},
		{"0.00", 0},
		{"-0", 0},
		{"-0.0", 0},
		{"0e5", 0},
		{"-1", 0},
		{"-0.01", 0},
		// Above the largest cap.
		{"100000000", 0},
		{"1e8", 0},
		{"1e9", 0},
		{"1e400", 0},
		{"1e99999999999999999999", 0},
		// Not a JSON number.
		{`"5"`, 0},
		{"", 0},
		{"+1", 0},
		{"01", 0},
		{"1.", 0},
		{".5", 0},
		{"NaN", 0},
		{"Infinity", 0},
		{"null", 0},
		{"5 ", 0},
	}
	for _, tc := range cases {
		t.Run(tc.literal, func(t *testing.T) {
			t.Parallel()
			got, err := sessionguard.ParseSessionSpendCap(tc.literal)
			if tc.want == 0 {
				if !errors.Is(err, sessionguard.ErrInvalidSessionSpendCap) {
					t.Fatalf("= %v, %v; want ErrInvalidSessionSpendCap", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("= %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

package postgres

import (
	"math"
	"testing"
)

// TestSessionSpendCapNumeric: a session spend cap converts to the exact
// NUMERIC its decimal reads, nil to SQL NULL (the cap cleared), and a value
// that is no number is an error -- never the NULL that would lift the cap
// the caller meant to set.
func TestSessionSpendCapNumeric(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		v       *float64
		want    string // the NUMERIC as the database would print it; "" for NULL
		wantErr bool
	}{
		{"nil clears the cap", nil, "", false},
		{"a cent", f(0.01), "0.01", false},
		{"one decimal stays exact", f(0.1), "0.1", false},
		{"two decimals", f(25.5), "25.5", false},
		{"the largest cap", f(99_999_999.99), "99999999.99", false},
		{"NaN", f(math.NaN()), "", true},
		{"positive infinity", f(math.Inf(1)), "", true},
		{"negative infinity", f(math.Inf(-1)), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := sessionSpendCapNumeric(tc.v)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("= %+v, nil error; want an error, never NULL", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if tc.want == "" {
				if n.Valid {
					t.Fatalf("= %+v, want NULL", n)
				}
				return
			}
			got, err := n.Value()
			if err != nil || got != tc.want {
				t.Fatalf("= %v (%v), want %s", got, err, tc.want)
			}
		})
	}
}

package sessionguard_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

func TestParseMicroUSD(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    sessionguard.MicroUSD
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "0.00", want: 0},
		{in: "25", want: 25_000_000},
		{in: "25.00", want: 25_000_000},
		{in: "24.999999", want: 24_999_999},
		{in: "0.000001", want: 1},
		{in: "1.4", want: 1_400_000},
		{in: "-1.5", want: -1_500_000},
		{in: "0099.990000", want: 99_990_000},
		{in: "99999999.99", want: 99_999_999_990_000},
		{in: "9223372036854.775807", want: 9_223_372_036_854_775_807},
		{in: "9223372036854.775808", wantErr: true},
		{in: "0.0000001", wantErr: true},
		{in: "", wantErr: true},
		{in: ".5", wantErr: true},
		{in: "5.", wantErr: true},
		{in: "1e3", wantErr: true},
		{in: "+1", wantErr: true},
		{in: " 1", wantErr: true},
		{in: "1,5", wantErr: true},
		{in: "NaN", wantErr: true},
		{in: "-", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := sessionguard.ParseMicroUSD(tc.in)
			if tc.wantErr {
				if !errors.Is(err, sessionguard.ErrMalformedAmount) {
					t.Fatalf("ParseMicroUSD(%q) = %d, %v; want ErrMalformedAmount", tc.in, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseMicroUSD(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestMicroUSD_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   sessionguard.MicroUSD
		want string
	}{
		{0, "$0.00"},
		{25_000_000, "$25.00"},
		{1_400_000, "$1.40"},
		{1_405_000, "$1.405"},
		{24_999_999, "$24.999999"},
		{1, "$0.000001"},
		{-500_000, "-$0.50"},
		{-9_223_372_036_854_775_808, "-$9223372036854.775808"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

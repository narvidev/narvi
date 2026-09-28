package main

import (
	"errors"
	"slices"
	"testing"
)

// TestFinishBootSequence_ReportsBootDurationOnlyAfterTheReownPass pins the
// order technical plan §3.2 needs at the end of a boot: a boot_duration
// with failed=false is boot evidence the control plane acts on, so it is
// sent only once the workspace re-own pass -- part of the boot, whose
// failure fails it -- has run, and its failed tag covers that pass.
func TestFinishBootSequence_ReportsBootDurationOnlyAfterTheReownPass(t *testing.T) {
	t.Parallel()

	errBoot := errors.New("setup.sh failed")
	errReown := errors.New("chown: permission denied")

	for _, tc := range []struct {
		name       string
		bootErr    error
		noSession  bool
		reownErr   error
		wantCalls  []string
		wantFailed bool
		wantErr    error
	}{
		{name: "boot and re-own succeed", wantCalls: []string{"reown", "report"}, wantFailed: false},
		{name: "re-own fails: the boot fails, and says so", reownErr: errReown, wantCalls: []string{"reown", "report"}, wantFailed: true, wantErr: errReown},
		{name: "boot failed: no re-own, reported failed", bootErr: errBoot, wantCalls: []string{"report"}, wantFailed: true, wantErr: errBoot},
		{name: "no live session: nothing to re-own", noSession: true, wantCalls: []string{"report"}, wantFailed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			var reported *bool
			var reown func() error
			if !tc.noSession {
				reown = func() error {
					calls = append(calls, "reown")
					return tc.reownErr
				}
			}
			err := finishBootSequence(tc.bootErr, reown, func(failed bool) {
				calls = append(calls, "report")
				reported = &failed
			})

			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tc.wantCalls)
			}
			switch {
			case reported == nil:
				t.Errorf("boot_duration never reported, want failed = %v", tc.wantFailed)
			case *reported != tc.wantFailed:
				t.Errorf("boot_duration failed = %v, want %v", *reported, tc.wantFailed)
			}
			if tc.wantErr == nil && err != nil {
				t.Errorf("error = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want one wrapping %v", err, tc.wantErr)
			}
		})
	}
}

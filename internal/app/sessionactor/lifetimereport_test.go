package sessionactor

import (
	"encoding/json"
	"testing"
	"time"
)

// TestReportedLifetimeRemaining pins how the control plane reads the
// lifetimeRemainingSeconds a ready or heartbeat carries (technical plan
// §35.2): leniently, so a value the contract does not allow never costs the
// frame, and clamped to whole seconds in [0, ProviderHardCap], every
// rounding toward an earlier deadline.
func TestReportedLifetimeRemaining(t *testing.T) {
	t.Parallel()
	const ceiling = 2 * time.Hour
	tests := []struct {
		name     string
		raw      string
		want     int32
		reported bool
	}{
		{name: "absent", raw: `{"type":"heartbeat","gen":1}`},
		{name: "null", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":null}`},
		{name: "a word", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":"soon"}`},
		{name: "an object", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":{"seconds":60}}`},
		{name: "a frame that is not JSON", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":60`},
		{name: "whole seconds", raw: `{"type":"ready","lifetimeRemainingSeconds":5400}`, want: 5400, reported: true},
		{name: "zero", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":0}`, want: 0, reported: true},
		{name: "negative reads as zero", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":-5}`, want: 0, reported: true},
		{name: "a fraction rounds down", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":59.9}`, want: 59, reported: true},
		{name: "under a second rounds to zero", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":0.5}`, want: 0, reported: true},
		{name: "an exponent", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":3.6e3}`, want: 3600, reported: true},
		{name: "the ceiling", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":7200}`, want: 7200, reported: true},
		{name: "above the ceiling", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":7201}`, want: 7200, reported: true},
		{name: "past an int64", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":99999999999999999999}`, want: 7200, reported: true},
		{name: "past a float64", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":1e400}`, want: 7200, reported: true},
		{name: "below a float64", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":-1e400}`, want: 0, reported: true},
		{name: "a number in quotes", raw: `{"type":"heartbeat","lifetimeRemainingSeconds":"60"}`, want: 60, reported: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reported := reportedLifetimeRemaining(json.RawMessage(tc.raw), ceiling)
			if got != tc.want || reported != tc.reported {
				t.Errorf("reportedLifetimeRemaining(%s) = (%d, %v), want (%d, %v)", tc.raw, got, reported, tc.want, tc.reported)
			}
		})
	}
}

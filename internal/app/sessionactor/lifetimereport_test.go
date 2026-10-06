package sessionactor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestReportedLifetimeRemaining pins how the control plane reads the
// lifetimeRemainingSeconds a ready or heartbeat carries (technical plan
// §35.2): leniently, and clamped to whole seconds in [0, ProviderHardCap],
// every rounding toward an earlier deadline.
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

// lifetimeReportValues are lifetimeRemainingSeconds values a ready can
// carry: the contract's own integers, values the contract allows that a Go
// int cannot hold (60.0, 6e1, 1e20, past an int64), values it does not
// allow, and values that are not numbers at all.
var lifetimeReportValues = []string{
	`60`, `0`, `-5`, `null`, `60.0`, `6e1`, `1e20`, `99999999999999999999`, `59.9`, `1e400`,
	`"60"`, `"soon"`, `{"seconds":60}`, `[60]`, `true`,
}

// TestReadyCapabilities_SurviveAnyLifetimeReport pins that the ready's
// lifetimeRemainingSeconds can never cost it its capabilities (technical
// plan §3.3, §35.2): whatever the key holds -- under its own name or with
// another case, which encoding/json would also match -- promptReceipt and
// maxFrameBytes read exactly as they do without it, as they did before
// the generated Ready named the key.
func TestReadyCapabilities_SurviveAnyLifetimeReport(t *testing.T) {
	t.Parallel()
	const base = `"type":"ready","messageId":"r1","sessionId":"s","gen":3,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown","capabilities":{"promptReceipt":true,"maxFrameBytes":1048576}`
	for _, key := range []string{"lifetimeRemainingSeconds", "LifetimeRemainingSeconds", "lifetimeremainingseconds"} {
		for _, value := range lifetimeReportValues {
			raw := json.RawMessage(`{` + base + `,"` + key + `":` + value + `}`)
			t.Run(key+"="+value, func(t *testing.T) {
				t.Parallel()
				if !readyAdvertisesPromptReceipt(raw) {
					t.Errorf("readyAdvertisesPromptReceipt(%s) = false, want true", raw)
				}
				if got := readyStatedMaxFrameBytes(raw); got == nil || *got != 1048576 {
					t.Errorf("readyStatedMaxFrameBytes(%s) = %v, want 1048576", raw, derefInt32(got))
				}
			})
		}
	}
}

// TestDecodeSnapshotReady_AnyProvenanceKeepsTheSnapshot pins that a
// snapshot_ready's provenance can never cost the snapshot (technical plan
// §35.5b): whatever it holds, the event decodes with its snapshot id and
// the command it answers, as it did before the generated SnapshotReady
// named the key.
func TestDecodeSnapshotReady_AnyProvenanceKeepsTheSnapshot(t *testing.T) {
	t.Parallel()
	const base = `"type":"snapshot_ready","messageId":"m1","sessionId":"s","gen":2,"ackId":"snapshot_ready:m1","snapshotId":"snap-1","commandMessageId":"c1"`
	for _, value := range []string{
		`{"agentProtocol":"1.25.0","runtimeVersion":"1.14.19"}`, `{"agentProtocol":"1.25.0","runtimeVersion":null}`, `{}`, `null`,
		`{"agentProtocol":42}`, `{"runtimeVersion":7}`, `"x"`, `[]`, `1e400`,
	} {
		raw := json.RawMessage(`{` + base + `,"provenance":` + value + `}`)
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			evt, err := decodeSnapshotReady(raw)
			if err != nil {
				t.Fatalf("decode %s: %v, want the snapshot decoded", raw, err)
			}
			if evt.SnapshotId != "snap-1" || evt.CommandMessageId == nil || *evt.CommandMessageId != "c1" {
				t.Errorf("decoded snapshot %q answering %v, want snap-1 answering c1", evt.SnapshotId, evt.CommandMessageId)
			}
		})
	}
}

// TestFrameWithout pins the helper both decodes above go through: the key
// goes, under every case encoding/json would match, and nothing else
// changes; a frame without it, or that is not an object, is returned as it
// was.
func TestFrameWithout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want map[string]string // nil: raw returned unchanged
	}{
		{name: "the key", raw: `{"a":1,"lifetimeRemainingSeconds":60.0}`, want: map[string]string{"a": `1`}},
		{name: "every case of it", raw: `{"a":1,"LifetimeRemainingSeconds":1,"lifetimeremainingseconds":2}`, want: map[string]string{"a": `1`}},
		{name: "other values kept verbatim", raw: `{"n":1e400,"s":"x","o":{"lifetimeRemainingSeconds":1},"lifetimeRemainingSeconds":1}`,
			want: map[string]string{"n": `1e400`, "s": `"x"`, "o": `{"lifetimeRemainingSeconds":1}`}},
		{name: "without the key", raw: `{"a":1}`},
		{name: "not an object", raw: `[1,2]`},
		{name: "not JSON", raw: `{"a":`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := frameWithout(json.RawMessage(tc.raw), lifetimeReportKey)
			if tc.want == nil {
				if string(got) != tc.raw {
					t.Errorf("frameWithout(%s) = %s, want it unchanged", tc.raw, got)
				}
				return
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatalf("frameWithout(%s) = %s: %v", tc.raw, got, err)
			}
			if len(fields) != len(tc.want) {
				t.Errorf("frameWithout(%s) = %s, want exactly %v", tc.raw, got, tc.want)
			}
			for name, value := range tc.want {
				if strings.TrimSpace(string(fields[name])) != value {
					t.Errorf("frameWithout(%s)[%q] = %s, want %s", tc.raw, name, fields[name], value)
				}
			}
		})
	}
}

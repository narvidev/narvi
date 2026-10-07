package sessionactor

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
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

// TestFrameWithout pins the helper both decodes above go through: every
// member the key names goes, under every case encoding/json would match,
// and every other member stays exactly as it was, in its place,
// duplicates included; a frame without the key, or that is not a single
// object, is returned as it was.
func TestFrameWithout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "the key, last", raw: `{"b":1,"a":"x","lifetimeRemainingSeconds":60.0}`, want: `{"b":1,"a":"x"}`},
		{name: "the key, first", raw: `{"lifetimeRemainingSeconds":60,"b":1,"a":2}`, want: `{"b":1,"a":2}`},
		{name: "the key, in the middle", raw: `{"b":1,"lifetimeRemainingSeconds":-5,"a":2}`, want: `{"b":1,"a":2}`},
		{name: "every case of it", raw: `{"z":0,"LifetimeRemainingSeconds":1,"y":true,"lifetimeremainingseconds":2}`, want: `{"z":0,"y":true}`},
		{name: "its name escaped", raw: `{"a":1,"lifetime\u0052emainingSeconds":1}`, want: `{"a":1}`},
		{name: "the only member", raw: `{"lifetimeRemainingSeconds":1}`, want: `{}`},
		{name: "other values kept verbatim", raw: `{"n":1e400,"s":"x\u00e9","o":{"lifetimeRemainingSeconds":1},"lifetimeRemainingSeconds":1,"f":60.0}`,
			want: `{"n":1e400,"s":"x\u00e9","o":{"lifetimeRemainingSeconds":1},"f":60.0}`},
		{name: "duplicates and case variants of others kept in order", raw: `{"c":{"p":true},"C":{"p":false},"lifetimeRemainingSeconds":1,"c":3}`,
			want: `{"c":{"p":true},"C":{"p":false},"c":3}`},
		{name: "whitespace", raw: " {\n  \"a\" : 1 ,\n  \"lifetimeRemainingSeconds\" : 1 ,\n  \"b\" : [ 2 ]\n} ", want: "{\"a\" : 1,\n  \"b\" : [ 2 ]}"},
		{name: "without the key", raw: `{"b":1,"a":2}`, want: `{"b":1,"a":2}`},
		{name: "not an object", raw: `[1,2]`, want: `[1,2]`},
		{name: "not JSON", raw: `{"a":`, want: `{"a":`},
		{name: "invalid after the key", raw: `{"lifetimeRemainingSeconds":1,"a":}`, want: `{"lifetimeRemainingSeconds":1,"a":}`},
		{name: "something after the object", raw: `{"lifetimeRemainingSeconds":1} {}`, want: `{"lifetimeRemainingSeconds":1} {}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := frameWithout(json.RawMessage(tc.raw), lifetimeReportKey)
			if string(got) != tc.want {
				t.Errorf("frameWithout(%s) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFrameWithout_EveryOtherFieldDecodesAsWithoutTheKey pins what the
// decodes through frameWithout promise: a frame decodes, field for field,
// exactly as the same frame never carrying the stripped key does -- the
// way it decoded before the generated types named that key -- including a
// frame whose other members repeat or vary in case, where encoding/json
// lets the last of them on the wire win.
func TestFrameWithout_EveryOtherFieldDecodesAsWithoutTheKey(t *testing.T) {
	t.Parallel()
	const ready = `"type":"ready","messageId":"r1","sessionId":"s","gen":3,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"`
	const snapshot = `"type":"snapshot_ready","messageId":"m1","sessionId":"s","gen":2,"ackId":"snapshot_ready:m1","commandMessageId":"c1"`
	readyTests := []struct{ name, before, after string }{
		{name: "capabilities then Capabilities", before: ready + `,"capabilities":{"promptReceipt":true,"maxFrameBytes":1048576},"Capabilities":{"promptReceipt":false}`},
		{name: "Capabilities then capabilities", before: ready + `,"Capabilities":{"promptReceipt":false},"capabilities":{"promptReceipt":true,"maxFrameBytes":1048576}`},
		{name: "the same member twice", before: ready + `,"capabilities":{"promptReceipt":true},"capabilities":{"maxFrameBytes":65536}`},
		{name: "the key between the variants", before: ready + `,"capabilities":{"promptReceipt":true}`, after: `"Capabilities":{"promptReceipt":false}`},
		{name: "agentVersion in two cases", before: ready + `,"AgentVersion":"v2","capabilities":{"promptReceipt":true}`},
	}
	for _, tc := range readyTests {
		t.Run("ready/"+tc.name, func(t *testing.T) {
			t.Parallel()
			members := tc.before
			if tc.after != "" {
				members += "," + tc.after
			}
			withKey := `{` + tc.before + `,"lifetimeRemainingSeconds":60.0`
			if tc.after != "" {
				withKey += "," + tc.after
			}
			withKey += `}`
			var want sandboxws.Ready
			wantErr := json.Unmarshal([]byte(`{`+members+`}`), &want)
			got, err := decodeReady(json.RawMessage(withKey))
			if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
				t.Errorf("decodeReady(%s) = %+v (%v), want %+v (%v): what the frame decodes to without the key", withKey, got, err, want, wantErr)
			}
		})
	}
	snapshotTests := []struct{ name, members string }{
		{name: "snapshotId then SnapshotId", members: snapshot + `,"snapshotId":"good","SnapshotId":"last"`},
		{name: "SnapshotId then snapshotId", members: snapshot + `,"SnapshotId":"first","snapshotId":"last"`},
	}
	for _, tc := range snapshotTests {
		t.Run("snapshot_ready/"+tc.name, func(t *testing.T) {
			t.Parallel()
			withKey := `{"provenance":{"agentProtocol":42},` + tc.members + `}`
			var want sandboxws.SnapshotReady
			wantErr := json.Unmarshal([]byte(`{`+tc.members+`}`), &want)
			got, err := decodeSnapshotReady(json.RawMessage(withKey))
			if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
				t.Errorf("decodeSnapshotReady(%s) = %+v (%v), want %+v (%v): what the frame decodes to without the key", withKey, got, err, want, wantErr)
			}
			if got.SnapshotId != "last" {
				t.Errorf("decodeSnapshotReady(%s).SnapshotId = %q, want the last on the wire", withKey, got.SnapshotId)
			}
		})
	}
}

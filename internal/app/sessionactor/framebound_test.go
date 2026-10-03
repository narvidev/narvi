package sessionactor

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

func int32Ptr(v int32) *int32 { return &v }

// checkPromptFrameBound runs one table of promptFrameBound cases.
func checkPromptFrameBound(t *testing.T, cases []struct {
	name string
	row  sqlcgen.Sandbox
	want int
}) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := promptFrameBound(tc.row); got != tc.want {
				t.Errorf("promptFrameBound(gen %d, agent_max_frame_bytes %v at gen %v, prompt_receipt_gen %v) = %d, want %d",
					tc.row.Gen, tc.row.AgentMaxFrameBytes, tc.row.AgentMaxFrameBytesGen, tc.row.PromptReceiptGen, got, tc.want)
			}
		})
	}
}

// TestPromptFrameBound_Stated: a read limit the live gen's latest ready
// stated is the bound, clamped to platform.MaxPromptFrameBytes, whatever
// else the ready advertised (technical plan §3.3).
func TestPromptFrameBound_Stated(t *testing.T) {
	t.Parallel()
	checkPromptFrameBound(t, []struct {
		name string
		row  sqlcgen.Sandbox
		want int
	}{
		{name: "the agent's own limit", row: sqlcgen.Sandbox{Gen: 3, AgentMaxFrameBytes: int32Ptr(platform.MaxPromptFrameBytes), AgentMaxFrameBytesGen: int32Ptr(3)}, want: platform.MaxPromptFrameBytes},
		{name: "a smaller limit, with promptReceipt", row: sqlcgen.Sandbox{Gen: 3, AgentMaxFrameBytes: int32Ptr(65536), AgentMaxFrameBytesGen: int32Ptr(3), PromptReceiptGen: int32Ptr(3)}, want: 65536},
		{name: "a smaller limit, without promptReceipt", row: sqlcgen.Sandbox{Gen: 3, AgentMaxFrameBytes: int32Ptr(1 << 20), AgentMaxFrameBytesGen: int32Ptr(3)}, want: 1 << 20},
		{name: "below the library's default", row: sqlcgen.Sandbox{Gen: 3, AgentMaxFrameBytes: int32Ptr(1024), AgentMaxFrameBytesGen: int32Ptr(3)}, want: 1024},
		{name: "above MaxPromptFrameBytes, clamped", row: sqlcgen.Sandbox{Gen: 3, AgentMaxFrameBytes: int32Ptr(math.MaxInt32), AgentMaxFrameBytesGen: int32Ptr(3)}, want: platform.MaxPromptFrameBytes},
	})
}

// TestPromptFrameBound_PromptReceiptOnly: a live gen whose latest ready
// stated no read limit but advertised promptReceipt is held to
// platform.MaxPromptFrameBytes -- every agent that advertises it was built
// with that read limit -- and a limit stated by an earlier gen counts for
// nothing.
func TestPromptFrameBound_PromptReceiptOnly(t *testing.T) {
	t.Parallel()
	checkPromptFrameBound(t, []struct {
		name string
		row  sqlcgen.Sandbox
		want int
	}{
		{name: "promptReceipt, nothing stated", row: sqlcgen.Sandbox{Gen: 2, PromptReceiptGen: int32Ptr(2)}, want: platform.MaxPromptFrameBytes},
		{name: "promptReceipt, a smaller limit stated by an earlier gen", row: sqlcgen.Sandbox{Gen: 2, PromptReceiptGen: int32Ptr(2), AgentMaxFrameBytes: int32Ptr(1024), AgentMaxFrameBytesGen: int32Ptr(1)}, want: platform.MaxPromptFrameBytes},
		{name: "promptReceipt, a stated value with no gen", row: sqlcgen.Sandbox{Gen: 2, PromptReceiptGen: int32Ptr(2), AgentMaxFrameBytes: int32Ptr(1024)}, want: platform.MaxPromptFrameBytes},
	})
}

// TestPromptFrameBound_Neither: a live gen whose latest ready stated no
// read limit and advertised no promptReceipt is held to the WebSocket
// library's default, platform.DefaultFrameReadLimitBytes -- including when
// an earlier gen stated or advertised more, since a respawn inherits
// nothing, and when a stored value is not a positive byte count.
func TestPromptFrameBound_Neither(t *testing.T) {
	t.Parallel()
	checkPromptFrameBound(t, []struct {
		name string
		row  sqlcgen.Sandbox
		want int
	}{
		{name: "nothing recorded", row: sqlcgen.Sandbox{Gen: 1}, want: platform.DefaultFrameReadLimitBytes},
		{name: "both recorded for an earlier gen", row: sqlcgen.Sandbox{Gen: 4, PromptReceiptGen: int32Ptr(3), AgentMaxFrameBytes: int32Ptr(platform.MaxPromptFrameBytes), AgentMaxFrameBytesGen: int32Ptr(3)}, want: platform.DefaultFrameReadLimitBytes},
		{name: "a gen with no value", row: sqlcgen.Sandbox{Gen: 4, AgentMaxFrameBytesGen: int32Ptr(4)}, want: platform.DefaultFrameReadLimitBytes},
		{name: "zero stated", row: sqlcgen.Sandbox{Gen: 4, AgentMaxFrameBytes: int32Ptr(0), AgentMaxFrameBytesGen: int32Ptr(4)}, want: platform.DefaultFrameReadLimitBytes},
		{name: "a negative value", row: sqlcgen.Sandbox{Gen: 4, AgentMaxFrameBytes: int32Ptr(-1), AgentMaxFrameBytesGen: int32Ptr(4)}, want: platform.DefaultFrameReadLimitBytes},
	})
}

// TestDispatchPlan_PromptFrameBound: a plan measures against the bound its
// planner read, and one that carries none against the smallest bound a gen
// is ever held to, never a larger one by omission.
func TestDispatchPlan_PromptFrameBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		bound int
		want  int
	}{
		{name: "the planner's bound", bound: platform.MaxPromptFrameBytes, want: platform.MaxPromptFrameBytes},
		{name: "a small bound", bound: 1024, want: 1024},
		{name: "none carried", bound: 0, want: platform.DefaultFrameReadLimitBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := &dispatchPlan{frameBound: tc.bound}
			if got := plan.promptFrameBound(); got != tc.want {
				t.Errorf("dispatchPlan{frameBound: %d}.promptFrameBound() = %d, want %d", tc.bound, got, tc.want)
			}
		})
	}
}

// TestFormatFrameBytes: a size reads in KiB below 1 MiB, so the library's
// default reads "32.0 KiB" and never "0.0 MiB", and in MiB from there.
func TestFormatFrameBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bytes int
		want  string
	}{
		{bytes: platform.DefaultFrameReadLimitBytes, want: "32.0 KiB"},
		{bytes: 41472, want: "40.5 KiB"},
		{bytes: 512, want: "0.5 KiB"},
		{bytes: 1<<20 - 1, want: "1024.0 KiB"},
		{bytes: 1 << 20, want: "1.0 MiB"},
		{bytes: platform.MaxPromptFrameBytes, want: "32.0 MiB"},
		{bytes: platform.MaxPromptFrameBytes + 3<<19, want: "33.5 MiB"},
	} {
		if got := formatFrameBytes(tc.bytes); got != tc.want {
			t.Errorf("formatFrameBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

// TestFormatFrameSizes: a refused frame's size and its bound always read
// differently to a person. Where formatFrameBytes would round both to the
// same text -- a frame 1 to 51 bytes over 32 KiB, or just over 32 MiB --
// both are given in exact bytes.
func TestFormatFrameSizes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		size, bound         int
		wantSize, wantBound string
	}{
		{size: 41472, bound: platform.DefaultFrameReadLimitBytes, wantSize: "40.5 KiB", wantBound: "32.0 KiB"},
		{size: platform.DefaultFrameReadLimitBytes + 1, bound: platform.DefaultFrameReadLimitBytes, wantSize: "32769 bytes", wantBound: "32768 bytes"},
		{size: platform.DefaultFrameReadLimitBytes + 51, bound: platform.DefaultFrameReadLimitBytes, wantSize: "32819 bytes", wantBound: "32768 bytes"},
		{size: platform.DefaultFrameReadLimitBytes + 52, bound: platform.DefaultFrameReadLimitBytes, wantSize: "32.1 KiB", wantBound: "32.0 KiB"},
		{size: platform.MaxPromptFrameBytes + 1, bound: platform.MaxPromptFrameBytes, wantSize: "33554433 bytes", wantBound: "33554432 bytes"},
		{size: platform.MaxPromptFrameBytes + 3<<19, bound: platform.MaxPromptFrameBytes, wantSize: "33.5 MiB", wantBound: "32.0 MiB"},
		{size: 1 << 20, bound: 36864, wantSize: "1.0 MiB", wantBound: "36.0 KiB"},
	} {
		gotSize, gotBound := formatFrameSizes(tc.size, tc.bound)
		if gotSize != tc.wantSize || gotBound != tc.wantBound {
			t.Errorf("formatFrameSizes(%d, %d) = %q, %q; want %q, %q", tc.size, tc.bound, gotSize, gotBound, tc.wantSize, tc.wantBound)
		}
		if gotSize == gotBound {
			t.Errorf("formatFrameSizes(%d, %d) renders both as %q", tc.size, tc.bound, gotSize)
		}
	}
}

// TestReadyStatedMaxFrameBytes pins how a ready's stated read limit is
// read: a positive value is recorded, saturated at what an int32 column
// holds; anything else -- absent, zero or less, or a ready that fails its
// schema decode -- states nothing.
func TestReadyStatedMaxFrameBytes(t *testing.T) {
	t.Parallel()

	const base = `"type":"ready","messageId":"r1","sessionId":"s","gen":3,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"`
	for _, tc := range []struct {
		name string
		raw  string
		want *int32
	}{
		{name: "the agent's own limit", raw: `{` + base + `,"capabilities":{"maxFrameBytes":33554432}}`, want: int32Ptr(33554432)},
		{name: "with promptReceipt", raw: `{` + base + `,"capabilities":{"promptReceipt":true,"maxFrameBytes":65536}}`, want: int32Ptr(65536)},
		{name: "past an int32, saturated", raw: `{` + base + `,"capabilities":{"maxFrameBytes":4294967296}}`, want: int32Ptr(math.MaxInt32)},
		{name: "capabilities without it", raw: `{` + base + `,"capabilities":{"promptReceipt":true}}`, want: nil},
		{name: "an agent that predates capabilities", raw: `{` + base + `}`, want: nil},
		{name: "zero", raw: `{` + base + `,"capabilities":{"maxFrameBytes":0}}`, want: nil},
		{name: "negative", raw: `{` + base + `,"capabilities":{"maxFrameBytes":-5}}`, want: nil},
		{name: "not an integer", raw: `{` + base + `,"capabilities":{"maxFrameBytes":"big"}}`, want: nil},
		{name: "fails its schema decode", raw: `{"type":"ready","messageId":"r1","gen":3,"capabilities":{"maxFrameBytes":65536}}`, want: nil},
		{name: "not JSON", raw: `not json`, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := readyStatedMaxFrameBytes(json.RawMessage(tc.raw))
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("readyStatedMaxFrameBytes(%s) = %v, want %v", tc.raw, derefInt32(got), derefInt32(tc.want))
			}
		})
	}
}

func derefInt32(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

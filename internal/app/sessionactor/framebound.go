// This file (framebound.go) is technical plan §3.3's per-gen prompt bound:
// the largest prompt frame the session actor writes to a sandbox's gen.
//
// A prompt is one WebSocket message, and an agent closes its connection on
// a message longer than its read limit (StatusMessageTooBig): the frame is
// lost, and with it the prompt. Agents differ in that limit. One built
// since platform.MaxPromptFrameBytes reads up to it, 32 MiB; one built
// before it, still running from an older snapshot or repo image, reads at
// the WebSocket library's default, platform.DefaultFrameReadLimitBytes
// (32 KiB). The control plane therefore measures every prompt frame
// against the bound of the gen it is written to -- a first dispatch, a
// re-enqueue and a receipt re-send alike -- and never writes one over it.
//
// The bound comes from the gen's latest ready, as the prompt-receipt
// capability does (promptreceipt.go), and never from the agent's build:
//
//   - The read limit the ready stated (capabilities.maxFrameBytes), clamped
//     to platform.MaxPromptFrameBytes, the most this control plane ever
//     writes. RecordSandboxReady records it against the gen
//     (sandboxes.agent_max_frame_bytes_gen,
//     migrations/000157_agent_max_frame_bytes.up.sql), and a later ready of
//     the gen that states none clears it.
//   - Otherwise platform.MaxPromptFrameBytes when that ready advertised
//     promptReceipt: every agent that advertises it was built with that
//     read limit, since both shipped together. Among them are the agents
//     built before maxFrameBytes, and a gen whose every ready a replica
//     built before this column recorded.
//   - Otherwise platform.DefaultFrameReadLimitBytes: an agent that states
//     nothing and advertises nothing may be one built before the larger
//     read limit. One that reads more but whose recorded ready shows
//     neither is held to it too -- an agent built before maxFrameBytes
//     whose prompt journal did not open, or one whose journal did not open
//     and whose every ready of the gen a replica built before the column
//     recorded: a larger prompt to it is refused, and named, where it could
//     have been read.
//
// A respawn, restore or resume bumps the gen, and inherits nothing: its
// first ready decides again.
//
// A frame over its gen's bound is never written. A first dispatch or a
// re-enqueue fails its turn there, as a refusal naming both sizes
// (executeDispatch); a receipt re-send is refused and counted, and fails
// nothing (executeReceiptResend).

package sessionactor

import (
	"fmt"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// promptFrameBound returns the largest prompt frame, encoded, that may be
// written to row's live gen: this file's top comment gives the rule.
func promptFrameBound(row sqlcgen.Sandbox) int {
	if row.AgentMaxFrameBytesGen != nil && *row.AgentMaxFrameBytesGen == row.Gen &&
		row.AgentMaxFrameBytes != nil && *row.AgentMaxFrameBytes > 0 {
		return min(int(*row.AgentMaxFrameBytes), platform.MaxPromptFrameBytes)
	}
	if promptReceiptCapable(row) {
		return platform.MaxPromptFrameBytes
	}
	return platform.DefaultFrameReadLimitBytes
}

// promptFrameBound returns the bound plan's prompt is measured against:
// the one its planner read from the gen's row, or, for a plan that carries
// none, platform.DefaultFrameReadLimitBytes, the smallest a gen is ever
// held to -- never a larger one by omission.
func (p *dispatchPlan) promptFrameBound() int {
	if p.frameBound <= 0 {
		return platform.DefaultFrameReadLimitBytes
	}
	return p.frameBound
}

// formatFrameBytes renders a byte count for a person, with one decimal: in
// KiB below 1 MiB, so the 32 KiB default reads "32.0 KiB" rather than
// "0.0 MiB", and in MiB from there.
func formatFrameBytes(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// This file (bootevidence.go) is §3.2's boot-evidence rule: a heartbeat
// whose lastBootPhase is null moves a sandbox Booting -> Ready only once
// the same generation has shown that its boot actually ran.
//
// The sandbox-ws contract reads a null lastBootPhase as "boot has
// completed" (events.schema.json, Heartbeat.lastBootPhase). A
// sandbox-agent built before 2026-09-28 broke that promise: its phase
// started null and stayed null until a service reported one, while it
// dials before it clones and the clone, the git-dir sync and the repo
// hooks report no phase. Its first heartbeat, 30s after connect, marked
// the sandbox Ready in the middle of its boot, and dispatch then sent a
// turn to a workspace that was not there yet. The fixed agent reports
// wsbridge.InitialBootPhase until its boot has completed, so its null is
// always the real thing -- but the agent is baked into the sandbox image,
// and every snapshot or repo image built before the fix keeps running the
// old one against this control plane. Evidence is what lets the control
// plane tell the two nulls apart without trusting the agent's build.
//
// Evidence is recorded durably (sandboxes.boot_evidence_gen,
// migrations/000147) rather than held in the actor: a control-plane
// restart, or the actor moving to another pod, must not strand a sandbox
// whose evidence arrived before it. Storing the gen scopes it to one
// generation: a respawn bumps gen and the old evidence stops matching.

package sessionactor

import (
	"encoding/json"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// bootEvidence reports whether cmd shows that its generation's boot has
// actually run, so that a later null lastBootPhase from the same
// generation can only mean "boot has completed":
//
//   - "boot_progress": a named phase was reported. Every agent build keeps
//     its tracked phase non-null from then until its boot completes.
//   - "heartbeat" with a non-null lastBootPhase: the same, observed
//     directly. A fixed agent sends one on every heartbeat of its boot,
//     starting with the one it forces as its boot starts
//     (wsbridge.Bridge.ReportBootStarted), so its own evidence never
//     waits for the first 30s tick or depends on boot_timing.
//   - "boot_timing" for "boot_duration" with failed=false: the agent's own
//     statement that its boot sequence finished (§33.3). Every agent built
//     since that event exists sends it, the fixed one included, before it
//     marks its boot complete, so it is also what covers a pre-fix agent
//     booting a repo with no service to report a phase.
//
// Nothing else is: "ready" only says the agent is connected, and the
// other boot_timing metrics, "git_sync" and a failed boot_duration speak
// to a boot still running, or one that did not finish. A frame that fails
// its schema decode is not evidence either.
func bootEvidence(cmd SandboxEvent) bool {
	switch cmd.Type {
	case "boot_progress":
		return true
	case "heartbeat":
		return cmd.LastBootPhase != nil
	case "boot_timing":
		var evt sandboxws.BootTiming
		if err := json.Unmarshal(cmd.Raw, &evt); err != nil {
			return false
		}
		return evt.Metric == sandboxws.BootTimingMetricBootDuration && evt.Failed != nil && !*evt.Failed
	default:
		return false
	}
}

// hasBootEvidence reports whether row's live generation has already shown
// boot evidence.
func hasBootEvidence(row sqlcgen.Sandbox) bool {
	return row.BootEvidenceGen != nil && *row.BootEvidenceGen == row.Gen
}

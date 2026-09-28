// This file (bootevidence.go) is §3.2's boot-evidence rule: a heartbeat
// whose lastBootPhase is null moves a sandbox Booting -> Ready only once
// the same generation has shown that its boot actually ran -- or, the
// fallback below, once it has been Booting longer than any boot that
// shows no evidence can still be running.
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
//
// The fallback. An agent built before boot_timing existed (2026-08-20),
// booting a repo with no service and no Docker, never shows evidence: it
// reports no phase and sends no boot_timing, and every heartbeat it sends
// is null. Every snapshot descended from a sandbox it booted keeps running
// it (§35.2), and a restore reuses the same snapshot. Nothing else times
// Booting out while heartbeats flow, so without a bound such a sandbox
// stayed Booting for its whole lifetime, and the next restore did the
// same. Once a generation with no evidence has been Booting -- measured on
// the database's clock from its Connecting -> Booting edge
// (sandboxes.booting_since, migrations/000148) -- for longer than
// platform.Timeouts.BootEvidenceFallback, which Validate keeps above the
// longest one-repo boot of that agent, its null phase is accepted as boot
// completion: what a control plane without the rule did at the first
// heartbeat, only after the bound. The heartbeat that finds the bound
// passed is the one that moves the sandbox, so heartbeats are flowing by
// construction. It is logged at WARN and counted
// (sandbox_boot_evidence_fallback_total).
//
// Who reaches the fallback, and when. Always a sandbox whose boot starts no
// service and no Docker, since a service or dockerd reports a
// boot_progress:
//
//   - An agent built before boot_timing existed (2026-08-20), at every such
//     boot, once the bound has passed. If that boot is still running then
//     -- several repos, each within the bound, together past it -- its null
//     phase is read as completion mid-boot: an hour in, where a control
//     plane without the rule read it at 30s.
//   - An agent built from 2026-08-20 up to the fix (2026-09-28). Its only
//     evidence is the successful boot_duration it sends once its boot
//     sequence has returned, so it gets here whenever that boot_duration
//     has not reached the control plane by the bound -- in practice, a
//     boot still running then, read as complete mid-boot as above: several
//     long repos, or, for an agent built from 2026-09-24 (b0bab4a, which
//     added gitdir.Seed's steps to every sync), even one repo with every
//     step at its timeout. The bound does not count that agent's steps
//     (platform.Timeouts.PreEvidenceAgentBootCeiling). A boot_duration it
//     did send is not lost on the way: it stays in the agent's buffer, and
//     every reconnect replays it, until evicted at the 1000-entry cap,
//     which a boot's own events do not reach. A failed boot never gets
//     here: that agent exits on it, and its heartbeats stop.
//   - A fixed agent does not: every connection it makes carries its boot's
//     start phase ahead of its first null phase
//     (wsbridge.Bridge.ReportBootStarted). Only a control plane that
//     failed to record every one of those start phases could bring it
//     here, and by then its boot has completed.

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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
//     directly. A fixed agent sends one on every heartbeat of its boot, one
//     as its boot starts, and one on every connection ahead of that
//     connection's first null -- even when its boot has completed before
//     the connection is up, or an earlier connection lost its frames in
//     flight (wsbridge.Bridge.ReportBootStarted) -- so its evidence reaches
//     the control plane ahead of its null phase, and never depends on
//     boot_timing.
//   - "boot_timing" for "boot_duration" with failed=false: the agent's own
//     statement that its boot finished (§33.3), which covers a pre-fix
//     agent booting a repo with no service to report a phase. The fixed
//     agent sends it once its whole boot has succeeded, after its final
//     pass re-owning the workspace for the agent runtime
//     (cmd/sandbox-agent). An agent built from 2026-08-28 (53e23a4, which
//     added that pass) up to the fix sends it just before that pass
//     instead, so for such an agent booting a repo with no service and no
//     Docker, a null heartbeat landing during the pass is still read as
//     completion: the rule narrows that agent's early window to the pass,
//     and cannot close it, since nothing on its wire marks the pass's end.
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

// unevidencedBootingFor reports how long gen has been Booting, on the
// database's clock, for the fallback (this file's top comment). The start
// is recorded on the Connecting -> Booting edge (handleSandboxEvent); a gen
// found with none -- a sandbox already Booting when migration 000148 ran --
// gets one now, in tx, which starts its clock late rather than never.
func (a *Actor) unevidencedBootingFor(ctx context.Context, tx pgx.Tx, gen int32) (time.Duration, error) {
	store := a.stores.sandbox.WithTx(tx)
	if err := store.MarkBootingSince(ctx, a.sessionID, gen); err != nil {
		return 0, fmt.Errorf("sessionactor: record booting start: %w", err)
	}
	bootingFor, ok, err := store.BootingElapsed(ctx, a.sessionID, gen)
	if err != nil {
		return 0, fmt.Errorf("sessionactor: read booting start: %w", err)
	}
	if !ok {
		// Unreachable while gen is the row's live gen: the statement
		// above recorded a start for it in this same transaction.
		return 0, fmt.Errorf("sessionactor: no booting start recorded for gen %d", gen)
	}
	return bootingFor, nil
}

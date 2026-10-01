// sessionStatus.ts -- decision 1 ("Statuses that tell the truth", mockups
// Session view): "cancelled, failed · timeout, booting, completed -- four
// semantic colors, four meanings. No more 'Failed' badge on a session
// that was merely stopped." This module is the ONE place that taxonomy is
// derived (from a restdtos.Session's own status/failureReason, plus its
// sandboxStatus when the caller is the sidebar list -- see Session.
// sandboxStatus's own schema doc comment for why GET /api/sessions/{id}
// never carries it) -- both the sidebar (SessionSidebar.tsx) and the
// session header consume this SAME function, so the two can never drift
// into showing a different chip for the same session.
import type { Session } from '@narvi/contracts/rest-dtos'

export type StatusTone = 'run' | 'ok' | 'warn' | 'crit' | 'neutral'

export interface StatusChip {
  tone: StatusTone
  /** Short label text, e.g. "running", "booting 2/4", "failed · timeout". */
  label: string
}

// BOOT_STAGES -- decision 2 ("Boot is a progression, not a spinner"): the
// FIXED 4-stage progression the rail's own "Sandbox" transitions list
// already draws literally (mockups.html: "spawning -> connecting",
// "connecting -> booting", ..., "booting -> ready") -- reused here as the
// honest denominator for the sidebar's compact "booting n/m" badge.
//
// This is a real, DELIBERATE approximation, not a wire guarantee: the
// sandbox_status Postgres enum (migrations/000006_sandboxes.up.sql) is
// exactly this fixed set, but the NAMED boot_progress phase a session is
// actually in (e.g. "installing deps") has no total-phase-count anywhere
// on the wire (contracts/sandbox-ws/v1/events.schema.json's own
// BootProgress carries only a `phase: string`, never an index/total --
// phases are dynamic per-repo/per-service, internal/sandboxagent/services'
// own doc comment). "n/m" here therefore reflects the sandbox's own
// coarse LIFECYCLE STAGE (a real, fixed, versioned enum), never the
// fine-grained named phase within it -- both are legitimate progress
// signals, this is the one this codebase can compute honestly today
// without a new persistence mechanism (out of this Step's own scope; see
// this Step's own PR description for the full reasoning).
const BOOT_TOTAL = 4 // spawning, connecting, booting, ready -- the 4 named nodes the rail's own transition chain draws
// 'pending' (sandbox row created, not yet spawned at all) is not one of
// the mockup's own 4 named nodes -- folded into the same "1/4" reading as
// 'spawning' rather than inventing a 5th stage the visual spec never
// draws; both genuinely mean "hasn't left the starting gate yet" from a
// reader's point of view.
const STAGE_INDEX: Record<string, number> = { pending: 1, spawning: 1, connecting: 2, booting: 3 }

/**
 * deriveBootProgress returns {index, total} while sandboxStatus is still
 * on the pre-ready path, or null once ready (or for any status this
 * module does not consider "booting" -- snapshotting/suspect/stopped/
 * failed/null all read as "not a boot-progress state" here; the session-
 * level status/failureReason are what characterize those instead, see
 * deriveStatusChip below).
 */
export function deriveBootProgress(sandboxStatus: string | null): { index: number; total: number } | null {
  if (sandboxStatus === null) return null
  const index = STAGE_INDEX[sandboxStatus]
  if (index === undefined) return null
  return { index, total: BOOT_TOTAL }
}

/**
 * isStillBooting reports whether an EMPTY timeline (no turns/warnings/
 * errors folded yet, timelineModel.ts's own hasContent check) should read
 * as "sandbox is booting" rather than a plain "no events yet" -- from the
 * sandbox status the SERVER derives (sandboxSnapshot.ts's snapshot, which
 * follows every change, ws/sessionStream.ts), never from the event log:
 * technical plan §3.2 keeps a sandbox `booting` after the agent's `ready`
 * until its boot has run and a heartbeat reports a null phase, and an
 * agent dials before it clones. True while the session itself can still
 * be booting (created/active) and the server's status is a pre-ready
 * stage (pending, spawning, connecting, booting -- deriveBootProgress's
 * own set), or the server has reported no sandbox yet: the session's
 * first turn spawns one. A completed/cancelled/failed session with zero
 * events has no boot in progress at all (its sandbox is long gone) and
 * must never claim otherwise -- caught live during the original browser
 * verification pass: a seeded 'completed' session with no events
 * rendered "Sandbox is booting…" before this check existed.
 */
export function isStillBooting(sessionStatus: Session['status'], sandboxStatus: string | null): boolean {
  if (sessionStatus !== 'created' && sessionStatus !== 'active') return false
  return sandboxStatus === null || deriveBootProgress(sandboxStatus) !== null
}

/**
 * deriveStatusChip is the single source of truth for a session's status
 * chip, everywhere one is rendered. sandboxStatus is optional -- GET
 * /api/sessions/{id} (the single-session header) never carries one (see
 * this module's own top comment), so an 'active' session there without a
 * known sandbox status renders as the honest, unspecific "active" rather
 * than guessing "running".
 */
export function deriveStatusChip(session: Pick<Session, 'status' | 'failureReason'>, sandboxStatus?: string | null): StatusChip {
  switch (session.status) {
    case 'cancelled':
      return { tone: 'neutral', label: 'cancelled' }
    case 'failed':
      return {
        tone: 'crit',
        label: session.failureReason ? `failed · ${session.failureReason}` : 'failed',
      }
    case 'completed':
      return { tone: 'ok', label: 'completed' }
    case 'created':
      return { tone: 'neutral', label: 'created' }
    case 'active': {
      const boot = sandboxStatus === undefined ? null : deriveBootProgress(sandboxStatus)
      if (boot !== null) {
        return { tone: 'warn', label: `booting ${boot.index}/${boot.total}` }
      }
      return { tone: 'run', label: sandboxStatus === undefined ? 'active' : 'running' }
    }
    default:
      // Exhaustive per the current session_status enum -- an unrecognized
      // value (a future enum member this client predates) still renders
      // something honest rather than throwing past the whole sidebar.
      return { tone: 'neutral', label: String(session.status) }
  }
}

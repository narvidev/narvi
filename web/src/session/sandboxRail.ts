// sandboxRail.ts -- decision 6 ("'What happened?' as self-service") /
// §12.2's own "sandbox rail (transitions, gen, fingerprint, boot phases,
// artifacts, cost incl. sub-task roll-up)": the pure model builder behind
// SessionRail.tsx's "Sandbox" and "Boot progress" panels. Combines
// sandboxSnapshot.ts's own one-shot WS snapshot (real, authoritative, but
// only refreshed on (re)subscribe) with this session's own live event log
// (boot_progress/ready/error, refreshed continuously) -- the same
// "REST/snapshot base + WS-derived overlay" shape SessionHeader.tsx
// already uses for session.title vs. model.latestTitle, applied here to
// sandbox status instead.
//
// # What this module can and cannot honestly show (documented, not
// silently gapped -- this codebase's own established convention,
// sessionStatus.ts's BOOT_TOTAL comment is the precedent)
//
// - status/gen/lastSeenAt: REAL. gen and "last seen" are derived
//   generically -- every sandbox-ws event carries its own `gen` field
//   (verified directly against contracts/sandbox-ws/v1/events.schema.json:
//   every one of its 20 event $defs requires `gen`), so the most recent
//   event of ANY type is a valid, honest proxy for "last seen" (§3.2's own
//   heartbeat-updated last_seen_at column, approximated the same way
//   sessionStatus.ts's own BOOT_TOTAL already approximates a different
//   field this codebase doesn't persist a finer-grained version of).
// - boot phases with durations: REAL, computed from consecutive
//   boot_progress event timestamps (each phase's duration = the next
//   phase's -- or 'ready''s -- own timestamp minus this phase's own). Not
//   sourced from the separate `boot_timing` event (real pre-measured
//   seconds, §33.1) -- deliberately deferred: boot_progress phases alone
//   already satisfy "boot phases with durations" honestly, and boot_timing
//   only covers 4 fixed metrics, not every named phase boot_progress
//   itself reports. A precision upgrade, not a gap in what this Step ships.
// - transitions: REAL but coarser than the mockup's own idealized
//   4-stage "spawning -> connecting -> booting -> ready" chain. No client-
//   visible source reports THOSE coarse lifecycle-stage transitions live
//   (sandbox_history, migrations/000007, is provably unpopulated by any
//   real code path today -- confirmed by reading every call site of the
//   pure internal/domain/sandbox.Transition function; every one commits
//   only sandboxes.status, never a history row). What IS real and shown
//   here instead: this session's own boot_progress/ready/error events,
//   each with its genuine server timestamp -- a richer, more specific
//   signal than the mockup's abstract stage names, just not the same
//   granularity. Labeled in the UI as observed from this session's own
//   events, never claimed as the authoritative transition ledger.
// - runtime fingerprint (agentVersion/imageDigest): REAL, sourced from
//   snapshot ONLY (state.sandbox's own agentVersion/imageDigest,
//   sandboxWireMap) -- never event-log-derived, because no CLIENT-visible
//   event carries it (sandbox-ws's own "ready" event does, §12.2 item 1,
//   but that fact reaches the browser only via the persisted sandboxes
//   row this snapshot already reads). Null until this gen's own first
//   "ready" event has landed server-side, or after a respawn resets it
//   (client.go's own UpsertSandboxForSpawn doc comment) -- SessionRail.tsx
//   renders an honest "not reported yet" for that window, never a stale
//   previous-gen value.
// - correlation id: a SEPARATE, per-request concept -- see
//   sessionCorrelationId.ts, not this module. Deliberately not folded into
//   SandboxRailModel alongside the fingerprint above even though both
//   render in the same rail panel: a fingerprint is a property of the
//   sandbox (this gen, stable for its whole life); a correlation id names
//   a REQUEST (this session's latest turn), a materially different
//   lifetime forcing the two into one shape would obscure.
import type { EventEnvelope } from '../ws/types'
import { isPlainObject } from '../ws/util'
import { asBootProgress, asReady, asSandboxError } from './eventPayloads'
import type { SandboxSnapshot } from './sandboxSnapshot'

/** shortDigest truncates an image digest for display -- mirrors SessionRail.tsx's own ArtifactRow's established `sha.slice(0, 7)` short-SHA convention, but strips a leading "algo:" prefix first (e.g. "sha256:") when present, since truncating THAT unchanged would just show the algorithm name, never any of the digest itself. */
export function shortDigest(digest: string): string {
  const withoutAlgo = digest.includes(':') ? digest.slice(digest.indexOf(':') + 1) : digest
  return withoutAlgo.slice(0, 7)
}

/** runtimeLabel renders the mockup's own "v1.4.2 · img 9f31c" convention (docs/design/mockups.html) from the sandbox's own real agentVersion/imageDigest -- null when either is still unreported (this gen's own "ready" event has not landed yet, or it was just reset by a respawn), so the caller (SessionRail.tsx) falls back to an honest "not reported yet" rather than a half-filled string. */
export function runtimeLabel(agentVersion: string | null, imageDigest: string | null): string | null {
  if (agentVersion === null || imageDigest === null) return null
  return `${agentVersion} · img ${shortDigest(imageDigest)}`
}

export interface BootPhase {
  phase: string
  startedAt: string
  /** Null while this is the most recently reported phase and nothing later (another phase, or 'ready') has arrived yet to bound its end. */
  endedAt: string | null
  /** Null while still open (endedAt is null) -- see endedAt's own doc comment. */
  seconds: number | null
}

export interface SandboxTransition {
  /** Stable React key -- event-id-derived, never re-derived from content (a hostile phase/message string must never collide two distinct transitions onto the same key). */
  id: string
  label: string
  at: string
  tone: 'neutral' | 'ok' | 'crit' | 'warn'
}

export interface SandboxRailModel {
  /** Matches sandbox_status verbatim when known; null when this session has no observed sandbox at all yet (sessionSnapshot null AND no gen-bearing event ever seen). */
  status: string | null
  gen: number | null
  lastSeenAt: string | null
  bootPhases: BootPhase[]
  transitions: SandboxTransition[]
  /** True once there is ANY evidence a sandbox exists (a snapshot, or at least one gen-bearing event) -- distinguishes "genuinely nothing yet" (a brand-new, not-yet-dispatched session) from "a sandbox exists but this session has nothing further to show". */
  hasSandbox: boolean
  /** §12.2 item 1's own runtime-fingerprint gap -- see this file's own top comment for why this is snapshot-only, never event-log-derived. Null until this gen's own first "ready" event reports it. */
  agentVersion: string | null
  /** Pairs with agentVersion -- same null-until-ready shape. */
  imageDigest: string | null
}

function extractGen(payload: unknown): number | null {
  if (!isPlainObject(payload)) return null
  return typeof payload.gen === 'number' ? payload.gen : null
}

export function buildSandboxRailModel(events: readonly EventEnvelope[], snapshot: SandboxSnapshot | null): SandboxRailModel {
  let status: string | null = snapshot?.status ?? null
  let gen: number | null = snapshot?.gen ?? null
  let lastSeenAt: string | null = snapshot?.lastSeenAt ?? null
  let hasSandbox = snapshot !== null

  const bootPhases: BootPhase[] = []
  const transitions: SandboxTransition[] = []
  let openPhase: BootPhase | null = null

  function closeOpenPhase(at: string): void {
    if (openPhase === null) return
    openPhase.endedAt = at
    const ms = new Date(at).getTime() - new Date(openPhase.startedAt).getTime()
    openPhase.seconds = Number.isFinite(ms) && ms >= 0 ? ms / 1000 : null
    openPhase = null
  }

  for (const event of events) {
    const eventGen = extractGen(event.payload)
    if (eventGen !== null) {
      gen = eventGen
      hasSandbox = true
      lastSeenAt = event.createdAt
    }

    const bootProgress = asBootProgress(event)
    if (bootProgress !== null) {
      closeOpenPhase(event.createdAt)
      openPhase = { phase: bootProgress.phase, startedAt: event.createdAt, endedAt: null, seconds: null }
      bootPhases.push(openPhase)
      status = 'booting'
      transitions.push({ id: `boot:${event.id}`, label: bootProgress.phase, at: event.createdAt, tone: 'neutral' })
      continue
    }

    const ready = asReady(event)
    if (ready !== null) {
      closeOpenPhase(event.createdAt)
      status = 'ready'
      transitions.push({ id: `ready:${event.id}`, label: 'ready', at: event.createdAt, tone: 'ok' })
      continue
    }

    const sandboxError = asSandboxError(event)
    if (sandboxError !== null) {
      if (sandboxError.fatal) {
        closeOpenPhase(event.createdAt)
        status = 'failed'
      }
      transitions.push({
        id: `error:${event.id}`,
        label: sandboxError.fatal ? `error: ${sandboxError.message}` : `warning: ${sandboxError.message}`,
        at: event.createdAt,
        tone: sandboxError.fatal ? 'crit' : 'warn',
      })
      continue
    }
  }

  return {
    status,
    gen,
    lastSeenAt,
    bootPhases,
    transitions,
    hasSandbox,
    agentVersion: snapshot?.agentVersion ?? null,
    imageDigest: snapshot?.imageDigest ?? null,
  }
}

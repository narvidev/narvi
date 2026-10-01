// sandboxRail.ts -- decision 6 ("'What happened?' as self-service") /
// §12.2's own "sandbox rail (transitions, gen, fingerprint, boot phases,
// artifacts, cost incl. sub-task roll-up)": the pure model builder behind
// SessionRail.tsx's "Sandbox" and "Boot progress" panels. Combines
// sandboxSnapshot.ts's sandbox row as the server holds it (the subscribe
// reply's, then every fetch_history reply's -- ws/sessionStream.ts) with
// this session's own event log, read for WHEN things happened.
//
// # What this module can and cannot honestly show (documented, not
// silently gapped -- this codebase's own established convention,
// sessionStatus.ts's BOOT_TOTAL comment is the precedent)
//
// - status: the SERVER's, from the snapshot alone -- never inferred from
//   an event. Technical plan §3.2 keeps a sandbox `booting` after the
//   agent's `ready` (the first event of every connection) until the same
//   generation has shown boot evidence and a heartbeat reports a null
//   phase; nor does an agent's `boot_progress` or fatal `error` move it.
//   An event-derived status read a sandbox still cloning as ready. The
//   snapshot follows the server: the control plane broadcasts a
//   sandbox_status event whenever it changes the status, and the
//   fetch_history reply that broadcast prompts carries the new row.
// - gen: the highest the server or the agent has reported -- the snapshot,
//   the agent's events (every sandbox-ws event carries its own `gen`,
//   contracts/sandbox-ws/v1/events.schema.json), the sandbox_status events.
//   A generation only ever grows, so the highest is the current one.
// - lastSeenAt: the most recent AGENT event of any type, or the snapshot's
//   before any -- §3.2's own last_seen_at, approximated. A sandbox_status
//   event is the server's, not a sign of life, and its gen sits under
//   `sandbox` so this never reads it as one.
// - boot phases with durations: REAL, from consecutive boot_progress event
//   timestamps. A phase ends at the next phase, or when the server reports
//   a status that ends the boot (a sandbox_status event,
//   sandboxSnapshot.ts's endsBootPhase) -- never at the agent's `ready`,
//   which comes before the boot. A phase still open when the server's
//   status says the boot is over (a boot that ended before the server
//   reported its changes) is shown finished, its duration unknown. Not
//   sourced from `boot_timing` (pre-measured seconds, §33.1): it covers
//   four fixed metrics, not every named phase.
// - transitions: this session's own boot_progress / ready / error events
//   and the server's own sandbox_status events, each at its real server
//   timestamp -- the agent's connection labelled as such, never as ready,
//   and the server's coarse lifecycle (spawning, connecting, booting,
//   ready, ...) as it reported it. A session whose sandbox changed status
//   before the control plane reported changes shows only the events it
//   has.
// - runtime fingerprint (agentVersion/imageDigest): REAL, from the
//   snapshot ONLY (state.sandbox's own agentVersion/imageDigest,
//   sandboxWireMap) -- no client-visible event carries it (sandbox-ws's
//   own "ready" event does, §12.2 item 1, but that fact reaches the
//   browser only via the persisted sandboxes row). Null until this gen's
//   own first "ready" event has landed server-side, or after a respawn
//   resets it (client.go's own UpsertSandboxForSpawn doc comment) --
//   SessionRail.tsx renders an honest "not reported yet" for that window.
// - correlation id: a SEPARATE, per-request concept -- see
//   sessionCorrelationId.ts, not this module. A fingerprint is a property
//   of the sandbox (this gen, stable for its whole life); a correlation id
//   names a REQUEST (this session's latest turn).
import type { EventEnvelope } from '../ws/types'
import { isPlainObject } from '../ws/util'
import { asBootProgress, asReady, asSandboxError } from './eventPayloads'
import { asSandboxStatusChange, endsBootPhase } from './sandboxSnapshot'
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
  /** When the phase ended: the next phase, or the server's sandbox_status ending the boot. Null while the phase is still open, and for a phase the server's status shows over without saying when (see `open`). */
  endedAt: string | null
  /** The phase's duration, null whenever endedAt is. */
  seconds: number | null
  /** True while this phase is still running: the latest one, and the server has not reported a status that ends the boot. */
  open: boolean
}

export interface SandboxTransition {
  /** Stable React key -- event-id-derived, never re-derived from content (a hostile phase/message string must never collide two distinct transitions onto the same key). */
  id: string
  label: string
  at: string
  tone: 'neutral' | 'ok' | 'crit' | 'warn'
}

export interface SandboxRailModel {
  /** The server's sandbox_status, verbatim, from the snapshot alone; null when the server has reported no sandbox (yet). */
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

function transitionTone(status: string): SandboxTransition['tone'] {
  if (status === 'ready') return 'ok'
  if (status === 'failed') return 'crit'
  if (status === 'suspect') return 'warn'
  return 'neutral'
}

export function buildSandboxRailModel(events: readonly EventEnvelope[], snapshot: SandboxSnapshot | null): SandboxRailModel {
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
    openPhase.open = false
    openPhase = null
  }

  function seeGen(next: number): void {
    gen = gen === null ? next : Math.max(gen, next)
    hasSandbox = true
  }

  for (const event of events) {
    // The server's own report of a status change: where a boot phase
    // ends, and a transition at the server's own timestamp. Never the
    // rail's status, which is the snapshot's (this file's top comment).
    const change = asSandboxStatusChange(event)
    if (change !== null) {
      seeGen(change.gen)
      if (endsBootPhase(change.status)) closeOpenPhase(event.createdAt)
      transitions.push({ id: `status:${event.id}`, label: change.status, at: event.createdAt, tone: transitionTone(change.status) })
      continue
    }

    const eventGen = extractGen(event.payload)
    if (eventGen !== null) {
      seeGen(eventGen)
      lastSeenAt = event.createdAt
    }

    const bootProgress = asBootProgress(event)
    if (bootProgress !== null) {
      closeOpenPhase(event.createdAt)
      openPhase = { phase: bootProgress.phase, startedAt: event.createdAt, endedAt: null, seconds: null, open: true }
      bootPhases.push(openPhase)
      transitions.push({ id: `boot:${event.id}`, label: bootProgress.phase, at: event.createdAt, tone: 'neutral' })
      continue
    }

    // The agent's `ready` is its connection coming up, ahead of its boot
    // (the sandbox-ws contract): it neither ends a phase nor makes the
    // sandbox ready -- the server says when that is.
    if (asReady(event) !== null) {
      transitions.push({ id: `ready:${event.id}`, label: 'agent connected', at: event.createdAt, tone: 'neutral' })
      continue
    }

    const sandboxError = asSandboxError(event)
    if (sandboxError !== null) {
      transitions.push({
        id: `error:${event.id}`,
        label: sandboxError.fatal ? `error: ${sandboxError.message}` : `warning: ${sandboxError.message}`,
        at: event.createdAt,
        tone: sandboxError.fatal ? 'crit' : 'warn',
      })
      continue
    }
  }

  // A phase still open while the server's status says the boot is over:
  // it ended, at a moment no sandbox_status event of this log records (a
  // boot the server finished before it reported its changes).
  const lastPhase: BootPhase | null = openPhase
  if (lastPhase !== null && snapshot !== null && endsBootPhase(snapshot.status)) {
    lastPhase.open = false
  }

  return {
    status: snapshot?.status ?? null,
    gen,
    lastSeenAt,
    bootPhases,
    transitions,
    hasSandbox,
    agentVersion: snapshot?.agentVersion ?? null,
    imageDigest: snapshot?.imageDigest ?? null,
  }
}

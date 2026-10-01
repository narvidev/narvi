// sandboxSnapshot.ts -- parses the ONE real, authoritative source this
// codebase has for a session's CURRENT sandbox row (id, gen, status,
// lastSeenAt): the WS subscribe reply's own `state.sandbox`
// (internal/adapters/inbound/wshub/client.go's own sandboxWireMap,
// verified directly against that Go source -- `{id, gen, status,
// lastSeenAt, createdAt, updatedAt, agentVersion, imageDigest}`,
// deliberately excluding tokenHash/providerId/spawnFailureCount/
// lastSpawnFailureAt, proven by
// TestClientHandler_SubscribedPayloadExcludesSandboxTokenHash), and every
// fetch_history reply's `sandbox` (FetchHistoryResponse.sandbox, the same
// shape, read again on each reply -- the control plane broadcasts a
// sandbox_status event whenever it changes the status, below, so the page
// re-reads it then). No REST endpoint exposes this at all (GET
// /api/sessions/:id/`sandboxStatus` is always null on the single-session
// view by design -- Session.sandboxStatus's own schema doc comment) -- the
// WS is genuinely the only source, which is why ws/sessionStream.ts
// captures it and sandboxRail.ts (below) is its first real consumer.
//
// `state` itself is `additionalProperties: true` on the wire
// (client-ws/v1/protocol.schema.json's own SubscribedPayload) -- untrusted
// exactly like every event payload this Step's session/eventPayloads.ts
// already treats defensively, so every field here is type-checked before
// being trusted, mirroring that module's own narrowing discipline.
import type { EventEnvelope } from '../ws/types'
import { isPlainObject } from '../ws/util'

export interface SandboxSnapshot {
  id: string
  gen: number
  /** Matches Postgres sandbox_status verbatim (pending/spawning/connecting/booting/ready/snapshotting/suspect/stopped/failed) -- kept as `string`, not a closed union, the same defensive posture eventPayloads.ts applies to every other server-owned enum: an unrecognized future value must render something honest, never crash this parser. */
  status: string
  /** Null when the sandbox has never reported a heartbeat/event yet. */
  lastSeenAt: string | null
  createdAt: string
  updatedAt: string
  /** §12.2 item 1's own runtime-fingerprint gap: this gen's own sandbox-agent binary version, or null until this gen's own first "ready" event reports it (or after a respawn resets it, client.go's own UpsertSandboxForSpawn doc comment). */
  agentVersion: string | null
  /** Pairs with agentVersion -- this gen's own sandbox image digest, same null-until-ready/reset-on-respawn shape. */
  imageDigest: string | null
}

/**
 * parseSandboxSnapshot narrows `raw` (SubscribedPayload.state.sandbox,
 * `unknown` until proven otherwise -- null when the session has no
 * sandbox row yet, sandboxWireMap's own caller in client.go) to a
 * SandboxSnapshot iff every required field is present with the right
 * primitive type. Never throws: a malformed or absent value is simply
 * "no snapshot", not an error -- callers (sandboxRail.ts) treat that
 * identically to "this session has no sandbox yet".
 */
export function parseSandboxSnapshot(raw: unknown): SandboxSnapshot | null {
  if (!isPlainObject(raw)) return null
  const { id, gen, status, lastSeenAt, createdAt, updatedAt, agentVersion, imageDigest } = raw
  if (typeof id !== 'string' || typeof gen !== 'number' || typeof status !== 'string') return null
  if (typeof createdAt !== 'string' || typeof updatedAt !== 'string') return null
  if (lastSeenAt !== null && typeof lastSeenAt !== 'string') return null
  if (agentVersion !== null && agentVersion !== undefined && typeof agentVersion !== 'string') return null
  if (imageDigest !== null && imageDigest !== undefined && typeof imageDigest !== 'string') return null
  return {
    id,
    gen,
    status,
    lastSeenAt: lastSeenAt ?? null,
    createdAt,
    updatedAt,
    agentVersion: agentVersion ?? null,
    imageDigest: imageDigest ?? null,
  }
}

/**
 * SANDBOX_STATUS_EVENT is the stored event the control plane appends
 * whenever it changes the sandbox's status or generation
 * (internal/app/sessionactor/sandboxstatus.go): `{sandbox: {gen, status}}`,
 * the status the server derives (technical plan §3.2) -- never one this
 * client infers from the agent's own events. It is what wakes an open page
 * to re-read the sandbox: its broadcast prompts a fetch_history, whose
 * reply carries the sandbox row (FetchHistoryResponse.sandbox,
 * ws/sessionStream.ts). The status shown is that row's; the event itself is
 * read for when each change happened (sandboxRail.ts's transitions, and
 * where a boot phase ends).
 */
export const SANDBOX_STATUS_EVENT = 'sandbox_status'

export interface SandboxStatusChange {
  gen: number
  /** sandbox_status verbatim, kept a `string` like SandboxSnapshot.status. */
  status: string
}

/**
 * asSandboxStatusChange narrows a sandbox_status event's payload, or
 * returns null for any other event or a payload that does not shape up.
 * The values sit under `sandbox`, never at the payload's top level, so a
 * reader taking a top-level `gen` as an agent event's (sandboxRail.ts's
 * last-seen) never mistakes this one for the agent's.
 */
export function asSandboxStatusChange(env: EventEnvelope): SandboxStatusChange | null {
  if (env.type !== SANDBOX_STATUS_EVENT || !isPlainObject(env.payload)) return null
  const sandbox = env.payload.sandbox
  if (!isPlainObject(sandbox)) return null
  const { gen, status } = sandbox
  if (typeof gen !== 'number' || typeof status !== 'string') return null
  return { gen, status }
}

/**
 * endsBootPhase reports whether the server, by reporting `status`, has
 * ended the boot phase in progress: a sandbox it marked ready (or past
 * it), one that stopped or failed, or a new generation starting over.
 * `booting` keeps the phase going, and so does `suspect` -- a liveness
 * doubt the server returns from to the status it left (§3.2's terminal
 * grace), not the end of a boot.
 */
export function endsBootPhase(status: string): boolean {
  return status !== 'booting' && status !== 'suspect'
}

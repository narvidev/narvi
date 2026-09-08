// sandboxSnapshot.ts -- parses the ONE real, authoritative source this
// codebase has for a session's CURRENT sandbox row (id, gen, status,
// lastSeenAt): the WS subscribe reply's own `state.sandbox`
// (internal/adapters/inbound/wshub/client.go's own sandboxWireMap,
// verified directly against that Go source -- `{id, gen, status,
// lastSeenAt, createdAt, updatedAt, agentVersion, imageDigest}`,
// deliberately excluding tokenHash/providerId/spawnFailureCount/
// lastSpawnFailureAt, proven by
// TestClientHandler_SubscribedPayloadExcludesSandboxTokenHash). No REST
// endpoint exposes this at all (GET /api/sessions/:id/`sandboxStatus` is
// always null on the single-session view by design -- Session.
// sandboxStatus's own schema doc comment) -- this WS field is genuinely
// the only source, which is why ws/sessionStream.ts (this Step's own
// change) captures it and sandboxRail.ts (below) is its first real
// consumer.
//
// `state` itself is `additionalProperties: true` on the wire
// (client-ws/v1/protocol.schema.json's own SubscribedPayload) -- untrusted
// exactly like every event payload this Step's session/eventPayloads.ts
// already treats defensively, so every field here is type-checked before
// being trusted, mirroring that module's own narrowing discipline.
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

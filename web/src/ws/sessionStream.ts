import type { QueryClient } from '@tanstack/react-query'
import type { FetchHistoryResponse, SubscribedPayload } from '@narvi/contracts/client-ws'

import type { BackoffOptions } from './transport'
import { ClientWsTransport } from './transport'
import { EventLog } from './eventLog'
import { initialSessionActivityState, reduceLog, type SessionActivityState } from './reducer'
import { invalidateForEvents } from './invalidation'
import type { ConnectionStatus, EventEnvelope, SyncState } from './types'
import { isPlainObject } from './util'

// SessionStream composes §12.1's full pipeline ("WS transport -> event log
// -> reducer -> query invalidation") into one object per viewed session.
// No view consumes this yet -- when one does, most likely via a
// `useSyncExternalStore(stream.subscribe, stream.getSnapshot)` hook built
// on top of subscribe()/getSnapshot() below. The job here is making the
// pipeline itself real and correct, not rendering it.
//
// # Why a live broadcast triggers a fetch_history round-trip instead of
// being appended directly
//
// A live broadcast frame carries no id (transport.ts's own top comment,
// grounded in internal/app/ports/eventbroadcaster.go's doc comment: "sent
// exactly as stored... no separate wrapper envelope"). EventLog can only
// dedup/order things that have an id. Rather than inventing a synthetic
// local id for broadcast-sourced data (which would make it indistinguishable
// from real history on replay, or require its own separate reconciliation
// pass later), this class treats every live broadcast purely as a
// low-latency "something changed, go get the real thing" signal and
// answers it by calling fetch_history from the log's own current
// highestId() forward -- the SAME id-bearing, dedup-safe path used for the
// initial replay and for post-reconnect backfill. This is not a
// workaround: internal/app/ports/eventbroadcaster.go's own doc comment
// says exactly this is the intended recovery path ("any client can always
// recover a missed live push via fetch_history's cursor-paginated
// replay").
//
// # syncState and the "must not silently present a gap as complete" rule
//
// syncState only ever becomes 'complete' when a fetch_history round-trip
// itself returns nextCursor: null -- i.e. the SERVER has confirmed there
// is nothing between the log's own highestId() and "now". It is forced
// back to 'syncing' (never left at a stale 'complete') the instant the
// transport reports anything other than 'open' (onStatusChange below):
// a dropped connection means this client can no longer vouch for
// completeness until a fresh subscribe + backfill pass proves it again.
export interface SessionStreamSnapshot {
  connectionStatus: ConnectionStatus
  syncState: SyncState
  events: EventEnvelope[]
  activity: SessionActivityState
  lastError: string | null
  /**
   * The subscribe reply's own `state.sandbox` (client.go's own
   * sandboxWireMap: `{id, gen, status, lastSeenAt, createdAt, updatedAt}`,
   * or null when this session has no sandbox row yet), passed through
   * verbatim and UNTYPED -- `state` is additionalProperties:true on the
   * wire, so this stays `unknown` here for the exact same reason `events`
   * elements stay a loose EventEnvelope rather than a fully-typed
   * SandboxEvent: the deeper, type-checked narrowing belongs to the
   * session view layer (session/sandboxSnapshot.ts's own
   * parseSandboxSnapshot, mirroring session/eventPayloads.ts's precedent),
   * never to this generic pipeline. Only refreshed on (re)subscribe --
   * `state` is not re-sent on every live broadcast/backfill page, so a
   * consumer wanting a LIVE status must layer this session's own event
   * log on top (session/sandboxRail.ts does exactly that).
   */
  sandboxState: unknown
  /**
   * The subscribe reply's own `state.correlationId` (§12.2 item 1's own
   * session-rail gap): the SESSION's own most-recently-created turn's
   * correlation id (client.go's own latestTurnCorrelationID), or null
   * when this session has no turns yet or its newest turn recorded none.
   * UNTYPED for the same reason sandboxState is -- session/
   * sessionCorrelationId.ts does the type-checked narrowing. Deliberately
   * a SEPARATE field from sandboxState, never merged into it: a
   * correlation id is a property of a request, not of the sandbox (see
   * that module's own top comment).
   */
  correlationIdState: unknown
  /**
   * The subscribe reply's own top-level `participants` array (§8.11,
   * "multiplayer presence") -- client.go's own resolveParticipants:
   * `{userId, displayName}` per currently-live, distinct user, passed
   * through verbatim and UNTYPED for the identical reason sandboxState
   * stays `unknown` above (the deeper, type-checked narrowing belongs to
   * session/participants.ts's own parseParticipants). Unlike sandboxState,
   * this field lives at the payload's own top level, not inside `state`
   * (protocol.schema.json's own SubscribedPayload shape) -- transport.ts's
   * looksLikeSubscribedPayload already proves it is an array before this
   * class ever sees it, so no defensive Array.isArray check is repeated
   * here. Only refreshed on (re)subscribe, exactly like sandboxState --
   * presence is a live, per-connection fact, so a client that stays
   * connected sees it go stale only until its next reconnect (a bounded,
   * honest staleness window, never presented as a push-updated roster).
   */
  participantsState: readonly { [k: string]: unknown }[]
}

export interface SessionStreamOptions {
  sessionId: string
  wsUrl: string
  clientId: string
  getToken: () => Promise<string>
  queryClient: QueryClient
  wsFactory?: (url: string) => WebSocket
  backoff?: BackoffOptions
  minFetchHistoryIntervalMs?: number
  fetchHistoryTimeoutMs?: number
  now?: () => number
}

function toEventEnvelope(raw: unknown): EventEnvelope | null {
  if (!isPlainObject(raw)) return null
  const { id, type, payload, createdAt } = raw
  if (typeof id !== 'number' || typeof type !== 'string' || typeof createdAt !== 'string') return null
  return { id, type, payload, createdAt }
}

/** parseEnvelopes converts a SubscribedPayload/FetchHistoryResponse `events` array (typed only as `{[k: string]: unknown}[]` by the generated schema -- see types.ts's own top comment for why) into EventEnvelopes, silently dropping (never throwing on) any element that doesn't shape up -- a malformed element from the server must not crash this pipeline, it must just not be trusted as log data. */
function parseEnvelopes(raw: readonly { [k: string]: unknown }[]): EventEnvelope[] {
  const out: EventEnvelope[] = []
  for (const item of raw) {
    const envelope = toEventEnvelope(item)
    if (envelope !== null) out.push(envelope)
  }
  return out
}

export class SessionStream {
  private readonly log = new EventLog()
  private readonly transport: ClientWsTransport
  private readonly sessionId: string
  private readonly queryClient: QueryClient

  private connectionStatus: ConnectionStatus = 'idle'
  private syncState: SyncState = 'idle'
  private activity: SessionActivityState = initialSessionActivityState
  private lastError: string | null = null
  private sandboxState: unknown = null
  private correlationIdState: unknown = null
  private participantsState: readonly { [k: string]: unknown }[] = []
  private readonly listeners = new Set<() => void>()
  private backfillInFlight = false
  private backfillDirty = false
  // cachedSnapshot -- fixed on first wiring this class into
  // React's useSyncExternalStore for the first time (this file's own top
  // comment already anticipated that exact hook shape, but getSnapshot()
  // below used to allocate a fresh object -- and a fresh `this.log.
  // entries()` array via .slice() -- on EVERY call. useSyncExternalStore
  // compares successive getSnapshot() results with Object.is to decide
  // whether a re-render is needed; a snapshot whose identity changes on
  // every call even with NO underlying state change makes React re-render
  // in a tight loop trying to reconcile a "torn" read, which manifests as
  // "Maximum update depth exceeded" (confirmed live: this exact failure,
  // in a real browser, before this fix). Invalidated (set to null) at the
  // top of notify() below -- the ONE place every mutation in this class
  // already funnels through before announcing a change -- so a caller
  // between two notify() calls always gets back the SAME reference, and a
  // caller after one gets a freshly computed one.
  private cachedSnapshot: SessionStreamSnapshot | null = null

  constructor(options: SessionStreamOptions) {
    this.sessionId = options.sessionId
    this.queryClient = options.queryClient
    this.transport = new ClientWsTransport({
      url: options.wsUrl,
      sessionId: options.sessionId,
      clientId: options.clientId,
      getToken: options.getToken,
      wsFactory: options.wsFactory,
      backoff: options.backoff,
      minFetchHistoryIntervalMs: options.minFetchHistoryIntervalMs,
      fetchHistoryTimeoutMs: options.fetchHistoryTimeoutMs,
      now: options.now,
      handlers: {
        onStatusChange: (status) => {
          this.connectionStatus = status
          if (status !== 'open' && this.syncState !== 'idle') {
            this.syncState = 'syncing'
          }
          this.notify()
        },
        onSubscribed: (payload) => {
          this.handleSubscribed(payload)
        },
        onBroadcast: () => {
          this.handleBroadcast()
        },
        onProtocolError: (message) => {
          this.lastError = message
          this.notify()
        },
      },
    })
  }

  start(): void {
    this.transport.start()
  }

  stop(): void {
    this.transport.stop()
  }

  getSnapshot(): SessionStreamSnapshot {
    if (this.cachedSnapshot === null) {
      this.cachedSnapshot = {
        connectionStatus: this.connectionStatus,
        syncState: this.syncState,
        events: this.log.entries(),
        activity: this.activity,
        lastError: this.lastError,
        sandboxState: this.sandboxState,
        correlationIdState: this.correlationIdState,
        participantsState: this.participantsState,
      }
    }
    return this.cachedSnapshot
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  private notify(): void {
    this.cachedSnapshot = null
    for (const listener of this.listeners) listener()
  }

  private handleSubscribed(payload: SubscribedPayload): void {
    // this.sandboxState is set unconditionally on every (re)subscribe --
    // including a reconnect, which is the ONLY way this client ever
    // learns about a sandbox transition that produced no client-visible
    // event of its own (see sandboxRail.ts's own top comment for the full
    // "what this can and cannot show" accounting).
    this.sandboxState = isPlainObject(payload.state) ? (payload.state.sandbox ?? null) : null
    this.correlationIdState = isPlainObject(payload.state) ? (payload.state.correlationId ?? null) : null
    this.participantsState = payload.participants
    const inserted = this.log.appendMany(parseEnvelopes(payload.events))
    this.applyNewEvents(inserted)
    // Always run at least one backfill pass after a (re)subscribe: the
    // initial replay is capped (client.go's own initialReplayLimit=200 /
    // maxInitialReplayBytes=16KiB) with no signal in the payload telling
    // this client whether it was actually truncated -- see this file's
    // own top comment and web/README.md's own "Reconnect gaps" section.
    // The only way to know for certain is to ask.
    void this.runBackfill()
  }

  private handleBroadcast(): void {
    void this.runBackfill()
  }

  private applyNewEvents(inserted: EventEnvelope[]): void {
    if (inserted.length > 0) {
      this.activity = reduceLog(this.log.entries())
      invalidateForEvents(this.queryClient, this.sessionId, inserted)
    }
    this.notify()
  }

  private async runBackfill(): Promise<void> {
    if (this.backfillInFlight) {
      // A pass is already walking forward; note that another is needed
      // once it finishes rather than firing a second, overlapping
      // fetch_history call (the transport only supports one in flight at
      // a time -- see transport.ts's own top comment).
      this.backfillDirty = true
      return
    }
    this.backfillInFlight = true
    try {
      for (;;) {
        this.setSyncState('syncing')
        const highest = this.log.highestId()
        const cursor = highest === null ? null : String(highest)
        let response: FetchHistoryResponse
        try {
          response = await this.transport.fetchHistory(cursor)
        } catch (err) {
          // Connection dropped, or the request timed out (transport.ts's
          // own fetchHistoryTimeoutMs) -- stop this pass. Do not spin
          // retrying against a connection that is either already
          // reconnecting (transport owns that backoff) or already dead;
          // a fresh 'subscribed' reply, once it arrives, starts a new
          // backfill pass on its own (handleSubscribed above).
          this.lastError = err instanceof Error ? err.message : String(err)
          this.notify()
          return
        }
        const inserted = this.log.appendMany(parseEnvelopes(response.events))
        this.applyNewEvents(inserted)
        if (response.nextCursor === null) break
      }
      this.setSyncState('complete')
    } finally {
      this.backfillInFlight = false
      if (this.backfillDirty) {
        this.backfillDirty = false
        void this.runBackfill()
      }
    }
  }

  private setSyncState(state: SyncState): void {
    if (this.syncState === state) return
    this.syncState = state
    this.notify()
  }
}

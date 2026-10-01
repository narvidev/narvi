import type { FetchHistoryRequest, FetchHistoryResponse, SubscribeRequest, SubscribedPayload } from '@narvi/contracts/client-ws'

import type { ConnectionStatus } from './types'
import { isPlainObject } from './util'

// transport.ts is the "WS transport" stage of §12.1's "WS transport ->
// event log -> reducer -> query invalidation" pipeline: it owns the raw
// WebSocket connection to GET /sessions/{sessionID}/ws?type=client
// (contracts/client-ws/v1/protocol.schema.json, §6.2) and turns its three
// frame kinds into three typed callbacks. It knows nothing about event
// logs, reducers, or query invalidation -- that composition lives in
// sessionStream.ts.
//
// # Frame classification: how this file tells the three kinds apart
//
// The protocol schema's own description is explicit about the sequence
// ("subscribe -> a single 'subscribed' reply -> broadcast stream") but,
// like the Go server implementing it, provides NO envelope/discriminator
// on inbound frames after the first one -- internal/adapters/inbound/
// wshub/doc.go's own doc comment: a live broadcast is "sent exactly as
// stored... no wrapper envelope". This file classifies each inbound frame
// the same way the protocol's own shape makes possible:
//   1. The very first frame received after open is always the `subscribed`
//      reply (SubscribedPayload) -- structurally validated (has the
//      required sessionId/state/events/artifacts/participants keys)
//      before being trusted, not merely cast.
//   2. Every frame after that is either the reply to THIS transport's own
//      most recent outstanding fetchHistory() call, or an unsolicited
//      live broadcast. There is no request-correlation id anywhere in
//      FetchHistoryRequest/FetchHistoryResponse (contracts/client-ws/v1/
//      protocol.schema.json has none), so this transport enforces "at
//      most one fetch_history in flight at a time" itself (fetchHistory
//      rejects a second call while one is pending) and then
//      shape-matches: a frame that parses with an `events` array AND a
//      `nextCursor` key AND no `type` key, WHILE a fetch_history call is
//      pending, is treated as that call's reply; everything else is a
//      live broadcast. The reply never has a `type` (FetchHistoryResponse
//      is additionalProperties:false, on a control plane older than its
//      `sandbox` too), while every broadcast of a sandbox's frame does:
//      the sandbox-ws contract requires one, and the control plane drops a
//      sandbox frame without one, or with a top-level `events`,
//      `nextCursor` or `sandbox` key (sessionactor's
//      refusedSandboxFrame). So a sandbox cannot answer this page's
//      fetch_history, and the reply's `sandbox` row, which the page shows
//      as the server's status, is the server's. An event the control plane
//      writes itself carries no `type` at its top level, but none of them
//      has an `events` array and a `nextCursor`: the remaining overlap is
//      a protocol with no correlation id, documented here, not assumed
//      away.
//
// # Redelivery this file does NOT attempt to solve
//
// A live broadcast frame carries no `id` (internal/app/ports/
// eventbroadcaster.go's own doc: the exact raw stored payload bytes, no
// wrapper) -- so this file cannot deduplicate a live broadcast against
// anything by identity, and does not try. It hands every live broadcast
// to onBroadcast verbatim, as an opaque, untrusted wake-up signal; dedup
// happens one layer up, in eventLog.ts, over the id-bearing frames
// (SubscribedPayload.events / FetchHistoryResponse.events) this file DOES
// validate and pass through structurally. See sessionStream.ts's own top
// comment for why a live broadcast is treated as "go re-fetch", not as
// directly-appendable log data.
//
// # Reconnect
//
// Any non-deliberate close (server-initiated, network drop, or one of the
// three documented custom close codes -- 4001 re-auth, 4002 token expired,
// 4003 idle timeout, wshub/doc.go) schedules a reconnect with exponential
// backoff + full jitter, capped at options.backoff.maxMs. getToken() is
// re-invoked on every single connection attempt, including the very
// first, rather than being read once and cached -- correct for the
// re-auth/expired codes specifically, and harmless (one extra REST call
// per reconnect) for every other close reason, so there is no need for
// this file to branch on the close code at all. Backoff resets to attempt
// 0 the moment a subscribe handshake actually completes -- a
// connection that stays up for a while should not inherit a stale, long
// delay from an earlier flapping period. stop() marks the transport
// user-closed; no reconnect is ever scheduled after it.

export interface TransportHandlers {
  onSubscribed: (payload: SubscribedPayload) => void
  /** onBroadcast receives one live broadcast frame's parsed JSON, verbatim, with no shape validation (the protocol places none on it) -- see this file's own top comment for why. */
  onBroadcast: (raw: unknown) => void
  onStatusChange: (status: ConnectionStatus) => void
  /** onProtocolError fires for a frame that could not be classified as any expected shape (malformed JSON, or the first frame not shaping up as SubscribedPayload) -- always followed by this transport closing and reconnecting, never a throw. */
  onProtocolError: (message: string) => void
}

export interface BackoffOptions {
  initialMs: number
  maxMs: number
  factor: number
}

const DEFAULT_BACKOFF: BackoffOptions = { initialMs: 500, maxMs: 15_000, factor: 2 }

export interface ClientWsTransportOptions {
  /** Full ws(s):// URL, e.g. `wss://host/sessions/<id>/ws?type=client` -- computed by the caller (sessionStream.ts), never by this file, so this file needs no browser globals (location) to be testable against a local fake server. */
  url: string
  sessionId: string
  clientId: string
  /** getToken mints (or re-mints) a per-participant WS token (POST /api/sessions/:id/ws-token, api/endpoints.ts's own mintWsToken) -- called fresh before every connection attempt. */
  getToken: () => Promise<string>
  handlers: TransportHandlers
  /** wsFactory constructs the underlying socket -- defaults to the global browser-standard `WebSocket` (present natively in both the browser and Node 22+, see this module's own package README note). Tests override it to point at a local `ws`-backed fake server. */
  wsFactory?: (url: string) => WebSocket
  backoff?: BackoffOptions
  /** Minimum spacing between fetch_history SENDS -- must stay above the server's own platform.Timeouts.ClientFetchHistoryMinInterval (250ms as configured today, internal/platform/timeouts.go) or the server silently drops the frame (no reply, ever) rather than queuing it, which would otherwise hang fetchHistory()'s returned promise until its own timeout. Defaults conservatively above that. */
  minFetchHistoryIntervalMs?: number
  /** How long fetchHistory() waits for a correlated reply before rejecting -- recovers from a request the server silently dropped (rate limit) or a connection that died mid-round-trip without yet firing 'close'. */
  fetchHistoryTimeoutMs?: number
  now?: () => number
}

interface PendingFetchHistory {
  resolve: (response: FetchHistoryResponse) => void
  reject: (error: Error) => void
  timeoutHandle: ReturnType<typeof setTimeout>
}

function looksLikeSubscribedPayload(value: unknown): value is SubscribedPayload {
  return (
    isPlainObject(value) &&
    typeof value.sessionId === 'string' &&
    isPlainObject(value.state) &&
    Array.isArray(value.events) &&
    Array.isArray(value.artifacts) &&
    Array.isArray(value.participants)
  )
}

/** looksLikeFetchHistoryResponse: an `events` array, a `nextCursor`, and no `type` -- the reply never has one, and every broadcast of a sandbox's frame does (see this file's top comment). */
function looksLikeFetchHistoryResponse(value: unknown): value is FetchHistoryResponse {
  return isPlainObject(value) && !('type' in value) && Array.isArray(value.events) && (typeof value.nextCursor === 'string' || value.nextCursor === null)
}

export class ClientWsTransport {
  private readonly options: ClientWsTransportOptions
  private readonly backoff: BackoffOptions
  private readonly wsFactory: (url: string) => WebSocket
  private readonly minFetchHistoryIntervalMs: number
  private readonly fetchHistoryTimeoutMs: number
  private readonly now: () => number

  private socket: WebSocket | null = null
  private awaitingSubscribedReply = false
  private userClosed = false
  private reconnectAttempt = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private pendingFetchHistory: PendingFetchHistory | null = null
  private lastFetchHistorySentAt = -Infinity
  private status: ConnectionStatus = 'idle'

  constructor(options: ClientWsTransportOptions) {
    this.options = options
    this.backoff = options.backoff ?? DEFAULT_BACKOFF
    this.wsFactory = options.wsFactory ?? ((url) => new WebSocket(url))
    this.minFetchHistoryIntervalMs = options.minFetchHistoryIntervalMs ?? 300
    this.fetchHistoryTimeoutMs = options.fetchHistoryTimeoutMs ?? 10_000
    this.now = options.now ?? (() => Date.now())
  }

  start(): void {
    this.userClosed = false
    this.connect()
  }

  stop(): void {
    this.userClosed = true
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.failPending(new Error('transport stopped'))
    this.socket?.close(1000, 'client stopped')
    this.socket = null
    this.setStatus('closed')
  }

  /** fetchHistory sends one fetch_history request and resolves with its reply. Rejects immediately (no send attempted) if not connected or another fetch_history is already in flight -- callers (sessionStream.ts) are expected to serialize their own backfill loop rather than firing concurrent requests, since the wire protocol gives this transport no way to correlate more than one at a time (see this file's own top comment). */
  async fetchHistory(cursor: string | null, limit?: number): Promise<FetchHistoryResponse> {
    if (this.socket === null || this.socket.readyState !== WebSocket.OPEN || this.awaitingSubscribedReply) {
      throw new Error('fetchHistory: not connected')
    }
    if (this.pendingFetchHistory !== null) {
      throw new Error('fetchHistory: another request is already in flight')
    }

    const waitMs = this.minFetchHistoryIntervalMs - (this.now() - this.lastFetchHistorySentAt)
    if (waitMs > 0) {
      await new Promise((resolve) => setTimeout(resolve, waitMs))
    }
    // Re-check after the wait: stop()/a disconnect could have happened
    // while this call was asleep.
    if (this.socket === null || this.socket.readyState !== WebSocket.OPEN) {
      throw new Error('fetchHistory: not connected')
    }

    const body: FetchHistoryRequest = { sessionId: this.options.sessionId, cursor, limit: limit ?? null }
    const frame = { type: 'fetch_history', ...body }

    return new Promise<FetchHistoryResponse>((resolve, reject) => {
      const timeoutHandle = setTimeout(() => {
        this.pendingFetchHistory = null
        reject(new Error('fetchHistory: timed out waiting for a reply'))
      }, this.fetchHistoryTimeoutMs)
      this.pendingFetchHistory = { resolve, reject, timeoutHandle }
      this.lastFetchHistorySentAt = this.now()
      this.socket?.send(JSON.stringify(frame))
    })
  }

  private connect(): void {
    this.setStatus(this.reconnectAttempt === 0 ? 'connecting' : 'reconnecting')
    this.awaitingSubscribedReply = true

    this.options
      .getToken()
      .then((token) => {
        if (this.userClosed) return
        const socket = this.wsFactory(this.options.url)
        this.socket = socket
        socket.addEventListener('open', () => {
          if (this.userClosed) return
          const req: SubscribeRequest = { token, clientId: this.options.clientId }
          socket.send(JSON.stringify(req))
        })
        socket.addEventListener('message', (event) => {
          this.handleMessage(typeof event.data === 'string' ? event.data : String(event.data))
        })
        socket.addEventListener('close', () => {
          if (this.socket === socket) this.socket = null
          this.failPending(new Error('fetchHistory: connection closed'))
          if (this.userClosed) {
            this.setStatus('closed')
            return
          }
          this.scheduleReconnect()
        })
        // 'error' carries no actionable browser-standard payload; the
        // 'close' handler above (which the spec guarantees follows) is
        // what actually drives reconnect. A listener is still required --
        // an unhandled 'error' event is otherwise surfaced as a console
        // error / unhandled-rejection-like warning in some environments.
        socket.addEventListener('error', () => {})
      })
      .catch((err: unknown) => {
        if (this.userClosed) return
        this.options.handlers.onProtocolError(`getToken failed: ${String(err)}`)
        this.scheduleReconnect()
      })
  }

  private handleMessage(raw: string): void {
    let parsed: unknown
    try {
      parsed = JSON.parse(raw)
    } catch {
      this.options.handlers.onProtocolError(`malformed frame (invalid JSON): ${raw.slice(0, 200)}`)
      if (this.awaitingSubscribedReply) {
        // Can't proceed without a valid handshake reply -- close and let
        // the normal reconnect path retry.
        this.socket?.close(1002, 'malformed subscribed reply')
      }
      return
    }

    if (this.awaitingSubscribedReply) {
      if (!looksLikeSubscribedPayload(parsed)) {
        this.options.handlers.onProtocolError('first frame after open was not a valid SubscribedPayload')
        this.socket?.close(1002, 'malformed subscribed reply')
        return
      }
      this.awaitingSubscribedReply = false
      this.reconnectAttempt = 0
      this.setStatus('open')
      this.options.handlers.onSubscribed(parsed)
      return
    }

    if (this.pendingFetchHistory !== null && looksLikeFetchHistoryResponse(parsed)) {
      const { resolve, timeoutHandle } = this.pendingFetchHistory
      clearTimeout(timeoutHandle)
      this.pendingFetchHistory = null
      resolve(parsed)
      return
    }

    this.options.handlers.onBroadcast(parsed)
  }

  private scheduleReconnect(): void {
    if (this.userClosed) return
    const attempt = this.reconnectAttempt
    this.reconnectAttempt += 1
    const cappedDelay = Math.min(this.backoff.maxMs, this.backoff.initialMs * this.backoff.factor ** attempt)
    const jitteredDelay = cappedDelay * (0.5 + Math.random() * 0.5)
    this.setStatus('reconnecting')
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.connect()
    }, jitteredDelay)
  }

  private failPending(error: Error): void {
    if (this.pendingFetchHistory !== null) {
      clearTimeout(this.pendingFetchHistory.timeoutHandle)
      this.pendingFetchHistory.reject(error)
      this.pendingFetchHistory = null
    }
  }

  private setStatus(status: ConnectionStatus): void {
    if (this.status === status) return
    this.status = status
    this.options.handlers.onStatusChange(status)
  }
}

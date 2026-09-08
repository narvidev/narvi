// Fake server test helper for the §12.1 WS pipeline (transport.test.ts,
// sessionStream.test.ts): a REAL local WebSocket server (the `ws` package,
// a devDependency used only from this __tests__ tree -- see
// tsconfig.app.json's own "exclude" comment) that ClientWsTransport (the
// production code, using the browser-standard global `WebSocket` client)
// connects to over an actual loopback socket. Nothing about the transport
// under test is mocked; only the SERVER's own scripted behavior is, which
// is the whole point -- these tests exercise the real frame-classification,
// reconnect, and backfill-request code paths, not a stand-in for them.
import type { AddressInfo } from 'node:net'

import { WebSocketServer, type WebSocket as ServerSocket } from 'ws'

interface Waiter {
  resolve: (value: unknown) => void
  reject: (error: Error) => void
}

export class FakeConnection {
  private readonly socket: ServerSocket
  // A real message queue, not a one-shot `.once('message', ...)` listener:
  // the client can send its next frame (e.g. a second fetch_history
  // request, fired the instant the first one's reply is processed --
  // transport.ts's own fetchHistory has no artificial delay when
  // minFetchHistoryIntervalMs is 0) before a test gets around to calling
  // nextMessage() again. A one-shot listener attached AFTER that frame
  // already arrived would simply never see it and hang forever; buffering
  // every inbound frame here, and only handing it to a waiter (or queuing
  // it if there is none yet), makes nextMessage() safe to call at any time
  // relative to when the message actually arrives.
  private readonly queue: unknown[] = []
  private readonly waiters: Waiter[] = []

  constructor(socket: ServerSocket) {
    this.socket = socket
    this.socket.on('message', (data) => {
      let parsed: unknown
      try {
        parsed = JSON.parse(data.toString())
      } catch (err) {
        const error = err instanceof Error ? err : new Error(String(err))
        const waiter = this.waiters.shift()
        if (waiter) {
          waiter.reject(error)
        }
        return
      }
      const waiter = this.waiters.shift()
      if (waiter) {
        waiter.resolve(parsed)
      } else {
        this.queue.push(parsed)
      }
    })
  }

  send(value: unknown): void {
    this.socket.send(JSON.stringify(value))
  }

  sendRaw(text: string): void {
    this.socket.send(text)
  }

  close(code?: number, reason?: string): void {
    this.socket.close(code, reason)
  }

  /** nextMessage resolves with the next inbound frame, JSON-parsed -- from the queue immediately if one is already buffered, otherwise once one arrives. */
  nextMessage(): Promise<unknown> {
    if (this.queue.length > 0) {
      return Promise.resolve(this.queue.shift())
    }
    return new Promise((resolve, reject) => {
      this.waiters.push({ resolve, reject })
    })
  }
}

export class FakeClientWsServer {
  private readonly wss: WebSocketServer

  private constructor(wss: WebSocketServer) {
    this.wss = wss
  }

  static async start(): Promise<FakeClientWsServer> {
    const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' })
    await new Promise<void>((resolve, reject) => {
      wss.once('listening', resolve)
      wss.once('error', reject)
    })
    return new FakeClientWsServer(wss)
  }

  urlFor(sessionId: string): string {
    const address = this.wss.address() as AddressInfo
    return `ws://127.0.0.1:${address.port}/sessions/${sessionId}/ws?type=client`
  }

  /** waitForConnection resolves with the next raw connection this server accepts -- await it once per expected connection attempt (e.g. once per reconnect). */
  waitForConnection(): Promise<FakeConnection> {
    return new Promise((resolve) => {
      this.wss.once('connection', (socket: ServerSocket) => {
        resolve(new FakeConnection(socket))
      })
    })
  }

  async close(): Promise<void> {
    await new Promise<void>((resolve, reject) => {
      this.wss.close((err) => (err ? reject(err) : resolve()))
    })
  }
}

/** subscribeAndReply is the common first step nearly every test needs: accept one connection, read (and discard -- callers that care about its contents use waitForConnection + nextMessage directly instead) its subscribe frame, and reply with `payload`. Returns the connection for whatever the test does next. */
export async function subscribeAndReply(server: FakeClientWsServer, payload: unknown): Promise<FakeConnection> {
  const conn = await server.waitForConnection()
  await conn.nextMessage()
  conn.send(payload)
  return conn
}

export interface FakeEventEnvelope {
  id: number
  type: string
  payload: unknown
  createdAt: string
}

export function fakeEvent(id: number, type = 'tool_call', payload: unknown = { ok: true }): FakeEventEnvelope {
  return { id, type, payload, createdAt: new Date(2026, 0, 1, 0, 0, id).toISOString() }
}

export function subscribedPayload(
  sessionId: string,
  events: FakeEventEnvelope[],
  state: Record<string, unknown> = {},
  eventsTruncated = false,
): {
  sessionId: string
  state: Record<string, unknown>
  events: FakeEventEnvelope[]
  eventsTruncated: boolean
  artifacts: unknown[]
  participants: unknown[]
} {
  return { sessionId, state, events, eventsTruncated, artifacts: [], participants: [] }
}

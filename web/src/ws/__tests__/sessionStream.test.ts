import { QueryClient } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { sessionQueryKeys } from '../../api/queryKeys'
import { buildSandboxRailModel } from '../../session/sandboxRail'
import { parseSandboxSnapshot } from '../../session/sandboxSnapshot'
import { isStillBooting } from '../../session/sessionStatus'
import { buildTimelineModel } from '../../session/timelineModel'
import { SessionStream } from '../sessionStream'
import { FakeClientWsServer, type FakeConnection, fakeEvent, subscribedPayload } from './fakeServer'

// sessionStream.test.ts drives the REAL SessionStream (real ClientWsTransport
// + real EventLog + real reduceLog + real invalidateForEvents) against a
// real local fake server -- these are the pipeline-level tests: they pin
// dedup, gap-filling/backfill, and invalidation composing correctly END TO
// END, not any one stage in isolation (eventLog.test.ts/reducer.test.ts/
// invalidation.test.ts already cover each stage on its own).

async function waitFor(predicate: () => boolean, timeoutMs = 5000): Promise<void> {
  const start = Date.now()
  while (!predicate()) {
    if (Date.now() - start > timeoutMs) {
      throw new Error('waitFor: timed out waiting for condition')
    }
    await new Promise((resolve) => setTimeout(resolve, 5))
  }
}

/** drainOneBackfillRound reads the next inbound frame (expected to be a fetch_history request) and replies with `events`/`nextCursor`. */
async function drainOneBackfillRound(conn: FakeConnection, events: ReturnType<typeof fakeEvent>[], nextCursor: string | null): Promise<unknown> {
  const request = await conn.nextMessage()
  conn.send({ events, nextCursor })
  return request
}

let server: FakeClientWsServer | undefined
let stream: SessionStream | undefined

afterEach(async () => {
  stream?.stop()
  stream = undefined
  await server?.close()
  server = undefined
})

function newStream(sessionId: string, queryClient: QueryClient): SessionStream {
  return new SessionStream({
    sessionId,
    wsUrl: server!.urlFor(sessionId),
    clientId: 'client-1',
    getToken: () => Promise.resolve('tok'),
    queryClient,
    minFetchHistoryIntervalMs: 0,
    backoff: { initialMs: 5, maxMs: 20, factor: 2 },
  })
}

describe('SessionStream', () => {
  // Regression test: getSnapshot() must return the SAME
  // object reference across repeated calls between two real state changes
  // -- React's useSyncExternalStore (web/src/session/useSessionStream.ts)
  // compares successive snapshots with Object.is, and a snapshot whose
  // identity changes on every call even with nothing new to show causes
  // an infinite render loop (confirmed live, in a real browser, before
  // this test/fix existed: "Maximum update depth exceeded"). Removing
  // SessionStream's own cachedSnapshot memoization (this file's own top
  // comment on getSnapshot/notify) makes this test fail.
  it('getSnapshot() returns a referentially stable snapshot until the next real state change', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-stable', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-stable', [fakeEvent(1)]))
    await drainOneBackfillRound(conn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    const first = stream.getSnapshot()
    const second = stream.getSnapshot()
    expect(second).toBe(first) // same reference -- no new state in between

    // A real state change (a live broadcast triggers a backfill pass,
    // sessionStream.ts's own top comment) invalidates the cache; the next
    // snapshot must be a NEW object, and stay stable again afterward.
    const backfillPromise = drainOneBackfillRound(conn, [fakeEvent(2)], null)
    conn.send({ type: 'artifact', extra: 'broadcast' })
    await backfillPromise
    await waitFor(() => stream!.getSnapshot().events.length === 2)
    const third = stream.getSnapshot()
    expect(third).not.toBe(first)
    expect(stream.getSnapshot()).toBe(third)
  })

  it('a redelivered replay (every fresh subscribe re-sends the same events, reconnect included) is applied exactly once -- removing EventLog\'s dedup guard makes this test fail', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-1', queryClient)

    const firstConnPromise = server.waitForConnection()
    stream.start()
    const firstConn = await firstConnPromise
    await firstConn.nextMessage()
    firstConn.send(subscribedPayload('sess-1', [fakeEvent(1), fakeEvent(2)]))
    await drainOneBackfillRound(firstConn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().activity.eventCount).toBe(2)

    // Reconnect: the server drops the connection; the transport reconnects
    // on its own backoff, and the fresh subscribe RE-SENDS the identical
    // events [1, 2] -- exactly what wshub/client.go's own afterID=0 replay
    // does on every single subscribe, reconnect included (confirmed by
    // reading that file directly; see web/README.md's own "Redelivery"
    // section).
    const secondConnPromise = server.waitForConnection()
    firstConn.close(1011, 'simulated drop')
    const secondConn = await secondConnPromise
    await secondConn.nextMessage()
    secondConn.send(subscribedPayload('sess-1', [fakeEvent(1), fakeEvent(2)]))
    await drainOneBackfillRound(secondConn, [], null)

    await waitFor(() => stream!.getSnapshot().connectionStatus === 'open' && stream!.getSnapshot().syncState === 'complete')
    const snapshot = stream.getSnapshot()
    expect(snapshot.events.map((e) => e.id)).toEqual([1, 2])
    // NOT 4: a redelivered event must not double-apply.
    expect(snapshot.activity.eventCount).toBe(2)
    expect(snapshot.activity.countsByType).toEqual({ tool_call: 2 })
  })

  it('a reconnect never silently drops an event that was added while disconnected', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-2', queryClient)

    const firstConnPromise = server.waitForConnection()
    stream.start()
    const firstConn = await firstConnPromise
    await firstConn.nextMessage()
    firstConn.send(subscribedPayload('sess-2', [fakeEvent(1)]))
    await drainOneBackfillRound(firstConn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    // While "disconnected", a new event (id 2) was durably committed on
    // the server. The reconnect's fresh subscribe reply reflects it.
    const secondConnPromise = server.waitForConnection()
    firstConn.close(1011, 'simulated drop')
    const secondConn = await secondConnPromise
    await secondConn.nextMessage()
    secondConn.send(subscribedPayload('sess-2', [fakeEvent(1), fakeEvent(2)]))
    await drainOneBackfillRound(secondConn, [], null)

    await waitFor(() => stream!.getSnapshot().connectionStatus === 'open' && stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().events.map((e) => e.id)).toEqual([1, 2])
  })

  it('syncState stays "syncing" (never "complete") while a truncated initial replay is still being backfilled via fetch_history, and the log ends up gap-free', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-3', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    // Simulates client.go's own initialReplayLimit/maxInitialReplayBytes
    // truncation: only event 1 comes back in the subscribed reply, even
    // though the session's real history is longer.
    conn.send(subscribedPayload('sess-3', [fakeEvent(1)]))

    const fh1 = await conn.nextMessage()
    expect(fh1).toMatchObject({ type: 'fetch_history', sessionId: 'sess-3', cursor: '1' })
    expect(stream.getSnapshot().syncState).toBe('syncing')
    conn.send({ events: [fakeEvent(2)], nextCursor: '2' })

    await waitFor(() => stream!.getSnapshot().events.length === 2)
    // A page with a non-null nextCursor means more history remains --
    // syncState must NOT claim completeness yet.
    expect(stream.getSnapshot().syncState).toBe('syncing')

    const fh2 = await conn.nextMessage()
    expect(fh2).toMatchObject({ cursor: '2' })
    conn.send({ events: [fakeEvent(3)], nextCursor: null })

    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().events.map((e) => e.id)).toEqual([1, 2, 3])
  })

  it('a live broadcast (no id, no envelope) triggers a fresh fetch_history round rather than being trusted as log data directly', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-4', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-4', [fakeEvent(1)]))
    await drainOneBackfillRound(conn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    // An anonymous live broadcast frame -- exactly the shape
    // internal/app/ports/eventbroadcaster.go's own doc comment describes
    // (the bare events.payload column, no id/type wrapper).
    conn.send({ note: 'something changed' })

    await waitFor(() => stream!.getSnapshot().syncState === 'syncing')
    await drainOneBackfillRound(conn, [fakeEvent(2)], null)

    await waitFor(() => stream!.getSnapshot().events.length === 2)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().events.map((e) => e.id)).toEqual([1, 2])
  })

  // Every frame of a streamed text part is its own row, sharing the part's
  // messageId (internal/app/sessionactor/tokenframe.go). A client whose log
  // already holds the part's first frame learns of the next one only as a
  // live signal, and must backfill it from its cursor and show it -- the
  // append-only property a payload replaced in place (same row id, so never
  // past any cursor) would break.
  it('backfills a later frame of an already-seen text part from its cursor and folds the part to the newest frame', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-frames', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-frames', [fakeEvent(1, 'token', { messageId: 'prt_plan', text: '' })]))
    await drainOneBackfillRound(conn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    const finalText = '1. Add the migration\n2. Wire the store'
    conn.send({ type: 'token', messageId: 'prt_plan', sessionId: 'sess-frames', gen: 1, text: finalText })

    const request = await drainOneBackfillRound(conn, [fakeEvent(2, 'token', { messageId: 'prt_plan', text: finalText })], null)
    expect(request).toMatchObject({ type: 'fetch_history', cursor: '1' })

    await waitFor(() => stream!.getSnapshot().events.length === 2)
    const tokens = buildTimelineModel(stream.getSnapshot().events).turns[0]!.steps[0]!.tokens
    expect(tokens).toMatchObject([{ messageId: 'prt_plan', text: finalText, cut: null }])
  })

  it('a malformed element inside an otherwise-valid events array is dropped, not applied and not fatal -- its valid siblings still land', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-5', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-5', [fakeEvent(1), { id: 'not-a-number', type: 'tool_call', payload: {}, createdAt: 'x' } as never, fakeEvent(2)]))
    await drainOneBackfillRound(conn, [], null)

    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().events.map((e) => e.id)).toEqual([1, 2])
  })

  it('newly-applied events invalidate the session\'s query keys -- breaking invalidateForEvents makes this test fail', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    const spy = vi.spyOn(queryClient, 'invalidateQueries')
    stream = newStream('sess-6', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-6', [fakeEvent(1, 'execution_complete')]))
    await drainOneBackfillRound(conn, [], null)

    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
    const invalidatedKeys = spy.mock.calls.map((call) => call[0]?.queryKey)
    expect(invalidatedKeys).toContainEqual(sessionQueryKeys.detail('sess-6'))
    expect(invalidatedKeys).toContainEqual(sessionQueryKeys.events('sess-6'))
  })

  it('connectionStatus/syncState downgrade to "syncing" the instant the connection drops, and never claim "complete" mid-outage', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-7', queryClient)

    const firstConnPromise = server.waitForConnection()
    stream.start()
    const firstConn = await firstConnPromise
    await firstConn.nextMessage()
    firstConn.send(subscribedPayload('sess-7', [fakeEvent(1)]))
    await drainOneBackfillRound(firstConn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    const secondConnPromise = server.waitForConnection()
    firstConn.close(1011, 'simulated drop')

    await waitFor(() => stream!.getSnapshot().syncState !== 'complete')
    expect(stream.getSnapshot().connectionStatus).not.toBe('open')

    const secondConn = await secondConnPromise
    await secondConn.nextMessage()
    secondConn.send(subscribedPayload('sess-7', [fakeEvent(1)]))
    await drainOneBackfillRound(secondConn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
  })

  // §12.2 item 1: state.sandbox (client.go's own sandboxWireMap) is the ONLY
  // source this codebase has for a session's current sandbox row -- see
  // session/sandboxSnapshot.ts's own top comment. This proves the WIRING:
  // handleSubscribed captures payload.state.sandbox into the snapshot
  // untouched (parsing/narrowing is session/sandboxSnapshot.ts's own job,
  // covered by that module's own unit tests).
  it('captures the subscribe reply\'s own state.sandbox into the snapshot, refreshed on every (re)subscribe', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-8', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-8', [fakeEvent(1)], { sandbox: { id: 'sb-1', gen: 2, status: 'booting', lastSeenAt: null, createdAt: 'x', updatedAt: 'y' } }))
    await drainOneBackfillRound(conn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    expect(stream.getSnapshot().sandboxState).toEqual({ id: 'sb-1', gen: 2, status: 'booting', lastSeenAt: null, createdAt: 'x', updatedAt: 'y' })

    // A reconnect's fresh subscribe reply with an UPDATED sandbox state
    // (gen bumped by a respawn) must replace the cached one, not merge or
    // retain the stale value.
    const secondConnPromise = server.waitForConnection()
    conn.close(1011, 'simulated drop')
    const secondConn = await secondConnPromise
    await secondConn.nextMessage()
    secondConn.send(subscribedPayload('sess-8', [fakeEvent(1)], { sandbox: { id: 'sb-1', gen: 3, status: 'ready', lastSeenAt: 'z', createdAt: 'x', updatedAt: 'w' } }))
    await drainOneBackfillRound(secondConn, [], null)

    await waitFor(() => stream!.getSnapshot().connectionStatus === 'open' && stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().sandboxState).toEqual({ id: 'sb-1', gen: 3, status: 'ready', lastSeenAt: 'z', createdAt: 'x', updatedAt: 'w' })
  })

  // Technical plan §3.2: the server keeps a sandbox booting after the
  // agent's `ready`, and changes its status with no reconnect. The control
  // plane broadcasts a sandbox_status event on every change, and the
  // fetch_history reply that broadcast prompts carries the row as the
  // server holds it (FetchHistoryResponse.sandbox). Taking the status from
  // the subscribe reply alone again makes this test fail.
  // The subscribe reply's row is the server's status at that moment, on a
  // control plane of any age: from it on, the page shows the server's
  // boot, and the agent's `ready` in the replayed log ends nothing while
  // the first fetch_history is still pending. Only a reply without the row
  // (a control plane older than it) moves the page to the rollout fallback,
  // where the agent's `ready` ends the boot as before. Starting the stream
  // in the fallback makes the "before the first reply" checks fail.
  describe('the boot a page shows around its first fetch_history reply', () => {
    const booting = { id: 'sb-1', gen: 1, status: 'booting', lastSeenAt: null, createdAt: 'x', updatedAt: 'y' }
    const cases: { name: string; reply: Record<string, unknown>; after: { serverReportsSandbox: boolean; railStatus: string | null; stillBooting: boolean } }[] = [
      { name: 'a current control plane: booting, before and after', reply: { events: [], nextCursor: null, sandbox: booting }, after: { serverReportsSandbox: true, railStatus: 'booting', stillBooting: true } },
      { name: 'a control plane older than the row: booting, then the agent\'s ready ends it', reply: { events: [], nextCursor: null }, after: { serverReportsSandbox: false, railStatus: 'ready', stillBooting: false } },
    ]
    for (const c of cases) {
      it(c.name, async () => {
        server = await FakeClientWsServer.start()
        stream = newStream('sess-11', new QueryClient())
        const connPromise = server.waitForConnection()
        stream.start()
        const conn = await connPromise
        await conn.nextMessage()
        const replayed = [fakeEvent(1, 'boot_progress', { type: 'boot_progress', gen: 1, phase: 'clone' }), fakeEvent(2, 'ready', { type: 'ready', gen: 1 })]
        conn.send(subscribedPayload('sess-11', replayed, { sandbox: booting }))
        await conn.nextMessage() // the first fetch_history, left pending for now
        await waitFor(() => stream!.getSnapshot().events.length === 2)

        const view = () => {
          const snap = stream!.getSnapshot()
          const sandbox = parseSandboxSnapshot(snap.sandboxState)
          const rail = buildSandboxRailModel(snap.events, sandbox, snap.serverReportsSandbox)
          const model = buildTimelineModel(snap.events)
          return {
            serverReportsSandbox: snap.serverReportsSandbox,
            railStatus: rail.status,
            stillBooting: isStillBooting('active', sandbox?.status ?? null, { serverReportsSandbox: snap.serverReportsSandbox, sawAgentReady: model.sawAgentReady }),
          }
        }
        expect(view(), 'before the first reply').toEqual({ serverReportsSandbox: true, railStatus: 'booting', stillBooting: true })

        conn.send(c.reply)
        await waitFor(() => stream!.getSnapshot().syncState === 'complete')
        expect(view(), 'after it').toEqual(c.after)

        // A reconnect: the new subscribe reply's row is the server's
        // status again, whatever the previous connection's replies were.
        const nextConnPromise = server!.waitForConnection()
        conn.close(1011, 'simulated drop')
        const nextConn = await nextConnPromise
        await nextConn.nextMessage()
        nextConn.send(subscribedPayload('sess-11', replayed, { sandbox: booting }))
        await nextConn.nextMessage() // its first fetch_history, left pending
        await waitFor(() => stream!.getSnapshot().connectionStatus === 'open')
        expect(view(), 'after a reconnect, before its first reply').toEqual({ serverReportsSandbox: true, railStatus: 'booting', stillBooting: true })
      })
    }
  })

  it('follows the sandbox every fetch_history reply carries, keeping it when a reply carries none', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-10', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    const booting = { id: 'sb-1', gen: 1, status: 'booting', lastSeenAt: null, createdAt: 'x', updatedAt: 'y' }
    conn.send(subscribedPayload('sess-10', [fakeEvent(1, 'ready', { type: 'ready', gen: 1 })], { sandbox: booting }))
    await conn.nextMessage()
    // The subscribe reply's row is the server's status at that moment, on a
    // control plane of any age (rollout compatibility, serverReportsSandbox).
    expect(stream.getSnapshot().serverReportsSandbox).toBe(true)
    conn.send({ events: [], nextCursor: null, sandbox: booting })
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')
    expect(stream.getSnapshot().sandboxState).toEqual(booting)
    expect(stream.getSnapshot().serverReportsSandbox).toBe(true)

    const steps: { name: string; reply: Record<string, unknown>; want: unknown; wantReports: boolean }[] = [
      {
        name: 'the server marks the sandbox ready',
        reply: { events: [fakeEvent(2, 'sandbox_status', { sandbox: { gen: 1, status: 'ready' } })], nextCursor: null, sandbox: { ...booting, status: 'ready' } },
        want: { ...booting, status: 'ready' },
        wantReports: true,
      },
      { name: 'a control plane older than the field', reply: { events: [fakeEvent(3)], nextCursor: null }, want: { ...booting, status: 'ready' }, wantReports: false },
      { name: 'no sandbox any more', reply: { events: [fakeEvent(4)], nextCursor: null, sandbox: null }, want: null, wantReports: true },
    ]
    for (const step of steps) {
      const before = stream.getSnapshot().events.length
      const replied = (async () => {
        await conn.nextMessage()
        conn.send(step.reply)
      })()
      conn.send({ sandbox: { gen: 1, status: 'ready' } }) // the live broadcast that prompts the fetch
      await replied
      await waitFor(() => stream!.getSnapshot().events.length === before + 1 && stream!.getSnapshot().syncState === 'complete')
      expect(stream.getSnapshot().sandboxState, step.name).toEqual(step.want)
      expect(stream.getSnapshot().serverReportsSandbox, step.name).toBe(step.wantReports)
    }
  })

  it('exposes sandboxState as null when this session has no sandbox row yet (state.sandbox absent/empty)', async () => {
    server = await FakeClientWsServer.start()
    const queryClient = new QueryClient()
    stream = newStream('sess-9', queryClient)

    const connPromise = server.waitForConnection()
    stream.start()
    const conn = await connPromise
    await conn.nextMessage()
    conn.send(subscribedPayload('sess-9', [fakeEvent(1)]))
    await drainOneBackfillRound(conn, [], null)
    await waitFor(() => stream!.getSnapshot().syncState === 'complete')

    expect(stream.getSnapshot().sandboxState).toBeNull()
  })
})

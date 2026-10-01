import { describe, expect, it } from 'vitest'

import { deriveBootProgress, deriveStatusChip, isStillBooting } from '../sessionStatus'

describe('deriveStatusChip', () => {
  it('renders cancelled as neutral (decision 1: "no more Failed badge on a session that was merely stopped")', () => {
    expect(deriveStatusChip({ status: 'cancelled', failureReason: null })).toEqual({ tone: 'neutral', label: 'cancelled' })
  })

  it('renders failed with its persisted reason, never a bare "Failed"', () => {
    expect(deriveStatusChip({ status: 'failed', failureReason: 'timeout' })).toEqual({ tone: 'crit', label: 'failed · timeout' })
  })

  it('renders completed as ok', () => {
    expect(deriveStatusChip({ status: 'completed', failureReason: null })).toEqual({ tone: 'ok', label: 'completed' })
  })

  it('renders an active session with a booting sandboxStatus as "booting n/m"', () => {
    expect(deriveStatusChip({ status: 'active', failureReason: null }, 'booting')).toEqual({ tone: 'warn', label: 'booting 3/4' })
  })

  it('renders an active session with a ready sandboxStatus as "running"', () => {
    expect(deriveStatusChip({ status: 'active', failureReason: null }, 'ready')).toEqual({ tone: 'run', label: 'running' })
  })

  it('renders an active session with sandboxStatus omitted (GetSession never populates it) as the honest, unspecific "active"', () => {
    expect(deriveStatusChip({ status: 'active', failureReason: null })).toEqual({ tone: 'run', label: 'active' })
  })

  it('never crashes on an unrecognized status value', () => {
    expect(() => deriveStatusChip({ status: 'some_future_status' as never, failureReason: null })).not.toThrow()
  })
})

describe('deriveBootProgress', () => {
  it('maps pending/spawning to 1/4 (folded together -- neither is one of the mockup\'s own 4 named nodes on its own), connecting to 2/4, booting to 3/4', () => {
    expect(deriveBootProgress('pending')).toEqual({ index: 1, total: 4 })
    expect(deriveBootProgress('spawning')).toEqual({ index: 1, total: 4 })
    expect(deriveBootProgress('connecting')).toEqual({ index: 2, total: 4 })
    expect(deriveBootProgress('booting')).toEqual({ index: 3, total: 4 })
  })

  it('returns null once ready (boot progress is over)', () => {
    expect(deriveBootProgress('ready')).toBeNull()
  })

  it('returns null for null (no sandbox row yet)', () => {
    expect(deriveBootProgress(null)).toBeNull()
  })

  it('returns null for a non-boot status (snapshotting/suspect/stopped/failed)', () => {
    expect(deriveBootProgress('snapshotting')).toBeNull()
    expect(deriveBootProgress('suspect')).toBeNull()
    expect(deriveBootProgress('stopped')).toBeNull()
    expect(deriveBootProgress('failed')).toBeNull()
  })
})

describe('isStillBooting', () => {
  // Technical plan §3.2: the boot is the server's status, never the event
  // log's -- a sandbox stays booting after the agent's `ready` until its
  // boot has run. Reading it from `ready` again (or ignoring the status)
  // makes the "still booting after its ready" row fail.
  const cases: { name: string; session: Parameters<typeof isStillBooting>[0]; sandbox: string | null; want: boolean }[] = [
    { name: 'still booting after the agent\'s ready: the server says booting', session: 'active', sandbox: 'booting', want: true },
    { name: 'a created session whose sandbox is booting', session: 'created', sandbox: 'booting', want: true },
    { name: 'every pre-ready stage reads booting', session: 'active', sandbox: 'connecting', want: true },
    { name: 'spawning reads booting', session: 'active', sandbox: 'spawning', want: true },
    { name: 'pending reads booting', session: 'created', sandbox: 'pending', want: true },
    { name: 'no sandbox yet: the first turn spawns one', session: 'created', sandbox: null, want: true },
    { name: 'the server marked it ready', session: 'active', sandbox: 'ready', want: false },
    { name: 'snapshotting is past the boot', session: 'active', sandbox: 'snapshotting', want: false },
    { name: 'a suspect sandbox is not booting', session: 'active', sandbox: 'suspect', want: false },
    { name: 'a stopped sandbox is not booting', session: 'active', sandbox: 'stopped', want: false },
    { name: 'a failed sandbox is not booting', session: 'active', sandbox: 'failed', want: false },
    // Named regression: caught live during the original browser
    // verification pass -- a seeded 'completed' session with zero events
    // rendered "Sandbox is booting…" before this function existed.
    { name: 'a completed session has no boot in progress', session: 'completed', sandbox: 'booting', want: false },
    { name: 'a cancelled session has no boot in progress', session: 'cancelled', sandbox: null, want: false },
    { name: 'a failed session has no boot in progress', session: 'failed', sandbox: 'booting', want: false },
  ]
  for (const c of cases) {
    it(c.name, () => {
      expect(isStillBooting(c.session, c.sandbox)).toBe(c.want)
      expect(isStillBooting(c.session, c.sandbox, { serverReportsSandbox: true, sawAgentReady: true }), 'the server reports the row: the agent\'s ready changes nothing').toBe(c.want)
    })
  }

  // Rollout compatibility: a control plane older than
  // FetchHistoryResponse.sandbox never sends the page a status after the
  // subscribe reply. Until a reply carries the row, the agent's `ready`
  // ends the boot, as before -- else a page subscribed while booting would
  // read booting until its next subscribe. Dropping the fallback makes the
  // "ready seen" rows fail; applying it once the server reports the row
  // makes the rows above fail.
  const rolloutCases: { name: string; session: Parameters<typeof isStillBooting>[0]; sandbox: string | null; sawAgentReady: boolean; want: boolean }[] = [
    { name: 'no row reported, the agent\'s ready seen: the boot is over, whatever the subscribe-time status', session: 'active', sandbox: 'booting', sawAgentReady: true, want: false },
    { name: 'no row reported, no ready yet: still booting', session: 'active', sandbox: 'booting', sawAgentReady: false, want: true },
    { name: 'no row reported, no ready yet, a status past the boot: still booting, as before', session: 'created', sandbox: 'ready', sawAgentReady: false, want: true },
    { name: 'no row reported: a finished session still has no boot in progress', session: 'completed', sandbox: 'booting', sawAgentReady: false, want: false },
  ]
  for (const c of rolloutCases) {
    it(c.name, () => {
      expect(isStillBooting(c.session, c.sandbox, { serverReportsSandbox: false, sawAgentReady: c.sawAgentReady })).toBe(c.want)
    })
  }
})

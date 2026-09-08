import { describe, expect, it } from 'vitest'

import type { Automation } from '@narvi/contracts/rest-dtos'

import { AUTO_PAUSE_THRESHOLD, automationStatusTone, lastRunTone, nextRunSummary, runHealthLabel, runStatusTone, targetsSummary, triggerSummary } from '../automationFormat'

function baseAutomation(overrides: Partial<Automation> = {}): Automation {
  return {
    id: 'a1',
    name: 'nightly audit',
    prompt: null,
    repos: [{ name: 'widgets', url: 'https://github.com/acme/widgets', branch: null }],
    status: 'active',
    consecutiveFailures: 0,
    createdBy: null,
    createdAt: '2026-08-20T02:00:00Z',
    updatedAt: '2026-08-20T02:00:00Z',
    triggerType: 'manual',
    triggerConfig: {},
    sandboxPathScope: null,
    sandboxMockConfigured: false,
    sandboxContractsPath: null,
    envVars: [],
    lastRunAt: null,
    lastRunStatus: null,
    artifactSummary: null,
    ...overrides,
  }
}

describe('AUTO_PAUSE_THRESHOLD', () => {
  it('is 3, matching internal/domain/automation.AutoPauseThreshold exactly', () => {
    expect(AUTO_PAUSE_THRESHOLD).toBe(3)
  })
})

describe('automationStatusTone', () => {
  it('active is ok, paused is neutral', () => {
    expect(automationStatusTone('active')).toBe('ok')
    expect(automationStatusTone('paused')).toBe('neutral')
  })
})

describe('lastRunTone', () => {
  it('maps every automation_invocation_status (minus pending) plus null', () => {
    expect(lastRunTone('succeeded')).toBe('ok')
    expect(lastRunTone('failed')).toBe('crit')
    expect(lastRunTone(null)).toBe('neutral')
  })
})

describe('runStatusTone', () => {
  it('maps every automation_run_status', () => {
    expect(runStatusTone('succeeded')).toBe('ok')
    expect(runStatusTone('failed')).toBe('crit')
    expect(runStatusTone('starting')).toBe('neutral')
    expect(runStatusTone('running')).toBe('neutral')
  })
})

describe('targetsSummary', () => {
  it('renders the single repo name when there is exactly one', () => {
    expect(targetsSummary([{ name: 'payroll-api', url: 'https://github.com/acme/payroll-api', branch: null }])).toBe('payroll-api')
  })
  it('renders a count for more than one', () => {
    expect(
      targetsSummary([
        { name: 'a', url: 'https://github.com/acme/a', branch: null },
        { name: 'b', url: 'https://github.com/acme/b', branch: null },
        { name: 'c', url: 'https://github.com/acme/c', branch: null },
      ]),
    ).toBe('3 repos')
  })
})

describe('triggerSummary', () => {
  it('cron with a schedule renders the real expression', () => {
    expect(triggerSummary('cron', { schedule: '0 2 * * *' })).toBe('cron · 0 2 * * *')
  })
  it('cron with no schedule in config falls back to the bare label, never a fabricated expression', () => {
    expect(triggerSummary('cron', {})).toBe('cron')
  })
  it('github renders event.action when both present', () => {
    expect(triggerSummary('github', { event: 'pull_request', action: 'labeled' })).toBe('github · pull_request.labeled')
  })
  it('github with only event renders just the event', () => {
    expect(triggerSummary('github', { event: 'pull_request' })).toBe('github · pull_request')
  })
  it('linear renders eventType.action', () => {
    expect(triggerSummary('linear', { eventType: 'issue', action: 'created' })).toBe('linear · issue.created')
  })
  it('manual and webhook render their own bare label', () => {
    expect(triggerSummary('manual', {})).toBe('manual')
    expect(triggerSummary('webhook', {})).toBe('webhook')
  })
})

describe('runHealthLabel', () => {
  it('renders "succeeded/terminal ok" (mockups.html\'s own "12/12 ok"/"47/48 ok")', () => {
    expect(runHealthLabel({ succeededRuns: 12, terminalRuns: 12 })).toBe('12/12 ok')
    expect(runHealthLabel({ succeededRuns: 47, terminalRuns: 48 })).toBe('47/48 ok')
  })

  it('returns null for null (never a fabricated 0/0 for "no runs yet")', () => {
    expect(runHealthLabel(null)).toBeNull()
  })

  it('returns null for undefined (the field absent on the wire entirely)', () => {
    expect(runHealthLabel(undefined)).toBeNull()
  })
})

describe('nextRunSummary', () => {
  it('a paused automation never shows a next-fire time, regardless of trigger type -- never a stale countdown', () => {
    expect(nextRunSummary(baseAutomation({ status: 'paused', triggerType: 'cron', triggerConfig: { schedule: '0 2 * * *' } }))).toBe('—')
  })
  it('an active cron automation shows its own real schedule expression, never a fabricated countdown', () => {
    expect(nextRunSummary(baseAutomation({ status: 'active', triggerType: 'cron', triggerConfig: { schedule: '0 2 * * *' } }))).toBe('0 2 * * *')
  })
  it('an active cron automation with no schedule in config falls back honestly to "scheduled"', () => {
    expect(nextRunSummary(baseAutomation({ status: 'active', triggerType: 'cron', triggerConfig: {} }))).toBe('scheduled')
  })
  it('github/linear-triggered automations show "on event" -- they have no schedule at all', () => {
    expect(nextRunSummary(baseAutomation({ status: 'active', triggerType: 'github' }))).toBe('on event')
    expect(nextRunSummary(baseAutomation({ status: 'active', triggerType: 'linear' }))).toBe('on event')
  })
  it('manual/webhook automations show "—" -- no next-fire concept exists', () => {
    expect(nextRunSummary(baseAutomation({ status: 'active', triggerType: 'manual' }))).toBe('—')
    expect(nextRunSummary(baseAutomation({ status: 'active', triggerType: 'webhook' }))).toBe('—')
  })
})

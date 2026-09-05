// GatekeeperAffordance.test.tsx -- docs/design/boundaries-design.md,
// section 4.4's own named test: three states -> three sentences, none
// containing a section sign -- the same audience-facing rule
// internal/ops/operatorcopy.go enforces mechanically over the SOURCE
// (this file is a .test.tsx, exempt from that scan, precisely so it can
// name section numbers freely while documenting the rule the source
// itself must never break).
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { CapabilityState, CapabilityStatus } from '@narvi/contracts/rest-dtos'

import { GatekeeperAffordanceForStatus, gatekeeperSentence } from '../GatekeeperAffordance'

function status(state: CapabilityState): CapabilityStatus {
  return { name: 'organization_governance', state }
}

describe('gatekeeperSentence', () => {
  it('not_installed says this build does not include the capability', () => {
    expect(gatekeeperSentence('not_installed', null)).toBe('Part of Narvi Gatekeeper. This build does not include it.')
  })

  it.each(['not_licensed', 'license_not_yet_valid', 'license_invalid'] as const)('%s says no valid license is configured', (state) => {
    expect(gatekeeperSentence(state, null)).toBe('Part of Narvi Gatekeeper. Included in this build, but no valid license is configured.')
  })

  it('license_expired names the expiry date', () => {
    const sentence = gatekeeperSentence('license_expired', '2026-01-15T00:00:00Z')
    expect(sentence).toContain('expired on')
    expect(sentence).toContain('2026')
  })

  it('license_expired with no known expiry still renders an honest sentence, never a fabricated date', () => {
    const sentence = gatekeeperSentence('license_expired', null)
    expect(sentence).toContain('an unknown date')
  })

  it('enabled renders nothing -- a real screen should already have taken this slot', () => {
    expect(gatekeeperSentence('enabled', null)).toBeNull()
  })

  it.each([
    ['not_installed', null] as const,
    ['not_licensed', null] as const,
    ['license_not_yet_valid', null] as const,
    ['license_invalid', null] as const,
    ['license_expired', '2026-01-15T00:00:00Z'] as const,
  ])('%s never cites a section number or a Step number', (state, expiresAt) => {
    const sentence = gatekeeperSentence(state, expiresAt)
    expect(sentence).not.toBeNull()
    expect(sentence ?? '').not.toMatch(/§/)
    expect(sentence ?? '').not.toMatch(/\bStep\s+\d/)
  })
})

describe('GatekeeperAffordanceForStatus', () => {
  it('renders the not_installed sentence as plain text', () => {
    const html = renderToStaticMarkup(<GatekeeperAffordanceForStatus status={status('not_installed')} licenseExpiresAt={null} />)
    expect(html).toContain('This build does not include it')
  })

  it('renders nothing for an enabled capability', () => {
    const html = renderToStaticMarkup(<GatekeeperAffordanceForStatus status={status('enabled')} licenseExpiresAt={null} />)
    expect(html).toBe('')
  })

  it('never claims Narvi Gatekeeper is sold, paid, priced, commercial or proprietary', () => {
    const states: CapabilityState[] = ['not_installed', 'not_licensed', 'license_not_yet_valid', 'license_invalid', 'license_expired', 'enabled']
    for (const state of states) {
      const html = renderToStaticMarkup(<GatekeeperAffordanceForStatus status={status(state)} licenseExpiresAt="2026-01-15T00:00:00Z" />)
      expect(html.toLowerCase()).not.toMatch(/paid|priced|purchase|proprietary|commercial|for sale/)
    }
  })

  it('never renders a section sign', () => {
    const states: CapabilityState[] = ['not_installed', 'not_licensed', 'license_not_yet_valid', 'license_invalid', 'license_expired']
    for (const state of states) {
      const html = renderToStaticMarkup(<GatekeeperAffordanceForStatus status={status(state)} licenseExpiresAt="2026-01-15T00:00:00Z" />)
      expect(html).not.toMatch(/§/)
    }
  })
})

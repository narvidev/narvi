// Proves auth/identityStatus.ts's own pure derivation -- the "identity
// auto-link status panel" (§12.2 item 7) logic, independent of rendering.
import { describe, expect, it } from 'vitest'
import type { Identity } from '@narvi/contracts/rest-dtos'

import { deriveIdentityStatuses, DISPLAYED_PROVIDERS, linkedViaCaption } from '../identityStatus'

function makeIdentity(overrides: Partial<Identity>): Identity {
  return {
    id: 'identity-1',
    provider: 'github',
    externalId: '12345',
    linkedVia: 'admin',
    createdAt: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('deriveIdentityStatuses', () => {
  it('reports every displayed provider as not connected when identities is empty', () => {
    const statuses = deriveIdentityStatuses([])
    expect(statuses.map((s) => s.provider)).toEqual([...DISPLAYED_PROVIDERS])
    expect(statuses.every((s) => !s.connected)).toBe(true)
  })

  it('reports a connected provider with its own linkedVia', () => {
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'github', linkedVia: 'auto_email' })])
    const github = statuses.find((s) => s.provider === 'github')
    expect(github).toEqual({ provider: 'github', connected: true, linkedVia: 'auto_email' })
    const slack = statuses.find((s) => s.provider === 'slack')
    expect(slack).toEqual({ provider: 'slack', connected: false })
  })

  it('reports a connected oidc identity (§41.3) exactly like any other provider row', () => {
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'oidc', linkedVia: 'admin' })])
    const oidc = statuses.find((s) => s.provider === 'oidc')
    expect(oidc).toEqual({ provider: 'oidc', connected: true, linkedVia: 'admin' })
  })

  it('never reports a "pending" status -- there is no honest self-view source for one (see this module\'s own doc comment)', () => {
    const statuses = deriveIdentityStatuses([])
    for (const status of statuses) {
      // ProviderStatus's own type has no 'pending' variant at all -- this
      // is a runtime belt-and-suspenders check that connected is always a
      // real boolean, never a third truthy-ish value smuggled in.
      expect(typeof status.connected).toBe('boolean')
    }
  })

  it('ignores an identity for a provider not in DISPLAYED_PROVIDERS (google)', () => {
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'google' })])
    expect(statuses.map((s) => s.provider)).toEqual([...DISPLAYED_PROVIDERS])
  })
})

describe('linkedViaCaption', () => {
  it('renders each enum value as an honest, non-literal caption', () => {
    expect(linkedViaCaption('auto_email')).toBe('matched by verified email')
    expect(linkedViaCaption('prompt')).toBe('confirmed via magic link')
    // Deliberately NOT "linked by an admin" -- see the module's own doc
    // comment: this value is also used for the identity created at
    // ordinary GitHub sign-in time, so that wording would be false in
    // the common case.
    expect(linkedViaCaption('admin')).toBe('connected')
    expect(linkedViaCaption('admin')).not.toMatch(/admin/i)
  })
})

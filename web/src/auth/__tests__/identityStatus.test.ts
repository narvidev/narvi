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
  it('reports every displayed provider as not connected when identities is empty (oidc configured)', () => {
    const statuses = deriveIdentityStatuses([], true)
    expect(statuses.map((s) => s.provider)).toEqual([...DISPLAYED_PROVIDERS])
    expect(statuses.every((s) => !s.connected)).toBe(true)
  })

  it('reports a connected provider with its own linkedVia', () => {
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'github', linkedVia: 'auto_email' })], true)
    const github = statuses.find((s) => s.provider === 'github')
    expect(github).toEqual({ provider: 'github', connected: true, linkedVia: 'auto_email' })
    const slack = statuses.find((s) => s.provider === 'slack')
    expect(slack).toEqual({ provider: 'slack', connected: false })
  })

  it('reports a connected oidc identity (§41.3) exactly like any other provider row, when oidc is configured', () => {
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'oidc', linkedVia: 'admin' })], true)
    const oidc = statuses.find((s) => s.provider === 'oidc')
    expect(oidc).toEqual({ provider: 'oidc', connected: true, linkedVia: 'admin' })
  })

  it('never reports a "pending" status -- there is no honest self-view source for one (see this module\'s own doc comment)', () => {
    const statuses = deriveIdentityStatuses([], true)
    for (const status of statuses) {
      // ProviderStatus's own type has no 'pending' variant at all -- this
      // is a runtime belt-and-suspenders check that connected is always a
      // real boolean, never a third truthy-ish value smuggled in.
      expect(typeof status.connected).toBe('boolean')
    }
  })

  it('ignores an identity for a provider not in DISPLAYED_PROVIDERS (google)', () => {
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'google' })], true)
    expect(statuses.map((s) => s.provider)).toEqual([...DISPLAYED_PROVIDERS])
  })

  // --- oidcConfigured gating (review round 1, finding O12): a
  // deployment with no second sign-in provider configured must show the
  // mockup's own fixed three-row baseline (github/slack/linear), never a
  // fourth row for a capability it doesn't offer. ---

  it('omits the oidc row entirely when oidc is NOT configured, even with no identities', () => {
    const statuses = deriveIdentityStatuses([], false)
    expect(statuses.map((s) => s.provider)).toEqual(['github', 'slack', 'linear'])
    expect(statuses.find((s) => s.provider === 'oidc')).toBeUndefined()
  })

  it('omits the oidc row when oidc is NOT configured, even when the member has a CONNECTED oidc identity', () => {
    // A deployment that disabled OIDC after some members had already
    // linked one -- deriveIdentityStatuses's own doc comment: the row
    // still disappears, since showing a chip for a capability this
    // deployment no longer offers would be its own kind of dishonest
    // affordance.
    const statuses = deriveIdentityStatuses([makeIdentity({ provider: 'oidc', linkedVia: 'admin' })], false)
    expect(statuses.find((s) => s.provider === 'oidc')).toBeUndefined()
    expect(statuses.map((s) => s.provider)).toEqual(['github', 'slack', 'linear'])
  })

  it('includes the oidc row when oidc IS configured, even with no oidc identity connected', () => {
    const statuses = deriveIdentityStatuses([], true)
    expect(statuses.find((s) => s.provider === 'oidc')).toEqual({ provider: 'oidc', connected: false })
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

// repoEntitlementCard.test.tsx -- RepoEntitlementStatus, the read-only half
// of RepoSettingsView.tsx's RepoEntitlementCard (an administrator's
// revocation of a repository's eligibility for new sessions): what it says
// for a repository that is open and for one that is revoked, and that the
// administrator-written reason and the revoking user's display name -- both
// free text -- render as text, never markup. Mirrors
// shadowLedgerRendering.test.tsx's own pattern: renderToStaticMarkup, no
// jsdom needed.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { RepoEntitlement } from '@narvi/contracts/rest-dtos'

import { RepoEntitlementStatus } from '../RepoSettingsView'

const XSS_IMG = '<img src=x onerror=alert(1)>'
const XSS_SCRIPT = '<script>alert(document.cookie)</script>'

function entitlement(overrides: Partial<RepoEntitlement> = {}): RepoEntitlement {
  return {
    repoFullName: 'acme/widgets',
    revoked: false,
    revokedAt: null,
    revokedByUserId: null,
    revokedByDisplayName: null,
    reason: null,
    ...overrides,
  }
}

function revoked(overrides: Partial<RepoEntitlement> = {}): RepoEntitlement {
  return entitlement({
    revoked: true,
    revokedAt: '2026-10-01T12:00:00Z',
    revokedByUserId: '7d1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b',
    revokedByDisplayName: 'Rae Voker',
    reason: 'Credentials rotated, audit pending',
    ...overrides,
  })
}

describe('RepoEntitlementStatus -- what an operator reads', () => {
  it('an open repository says new sessions are allowed', () => {
    const html = renderToStaticMarkup(<RepoEntitlementStatus entitlement={entitlement()} />)
    expect(html).toContain('New sessions are allowed on this repository.')
    expect(html).not.toContain('revoked')
  })

  it('a revoked repository says so, by whom and why, and that running turns finish unless their sandbox goes', () => {
    const html = renderToStaticMarkup(<RepoEntitlementStatus entitlement={revoked()} />)
    expect(html).toContain('New sessions on this repository are revoked.')
    expect(html).toContain('a turn already running finishes, unless its sandbox restarts or stops first, which ends it.')
    expect(html).toContain('Rae Voker')
    expect(html).toContain('Credentials rotated, audit pending')
  })

  it('a revocation whose administrator no longer exists still renders, without a name', () => {
    const html = renderToStaticMarkup(<RepoEntitlementStatus entitlement={revoked({ revokedByUserId: null, revokedByDisplayName: null })} />)
    expect(html).toContain('a user who no longer exists')
    expect(html).toContain('Credentials rotated, audit pending')
  })

  it('never reads as an installation or configuration problem', () => {
    for (const e of [entitlement(), revoked()]) {
      const html = renderToStaticMarkup(<RepoEntitlementStatus entitlement={e} />).toLowerCase()
      for (const banned of ['installed', 'configured', 'access', '§']) {
        expect(html).not.toContain(banned)
      }
    }
  })
})

describe('RepoEntitlementStatus -- free text stays text, never markup', () => {
  it('a hostile reason renders as text', () => {
    const html = renderToStaticMarkup(<RepoEntitlementStatus entitlement={revoked({ reason: `frozen ${XSS_SCRIPT}` })} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a hostile display name renders as text', () => {
    const html = renderToStaticMarkup(<RepoEntitlementStatus entitlement={revoked({ revokedByDisplayName: `Mallory ${XSS_IMG}` })} />)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })
})

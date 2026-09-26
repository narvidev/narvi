// membersRendering.test.tsx -- MembersPanel.tsx's own defining risk:
// member.displayName (a GitHub display name, admin-editable, never
// Narvi-validated) and every AuditLogEntry field (action/resourceType/
// resourceId, and detail -- an opaque per-action JSON blob that can
// legitimately embed anything a request body once carried) must render
// as plain text only. Mirrors reviewRendering.test.tsx/
// automationRendering.test.tsx's own established pattern exactly.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type { AuditLogEntry, Member } from '@narvi/contracts/rest-dtos'

import { AuditLogRow, MemberRow } from '../MembersPanel'

const XSS_SCRIPT = '<script>alert(document.cookie)</script>'
const XSS_IMG = '<img src=x onerror=alert(1)>'

function withQueryClient(node: React.ReactNode) {
  const client = new QueryClient()
  return renderToStaticMarkup(<QueryClientProvider client={client}>{node}</QueryClientProvider>)
}

function baseMember(overrides: Partial<Member> = {}): Member {
  return {
    id: 'u1',
    email: 'sarah@example.invalid',
    displayName: 'Sarah K.',
    role: 'maintainer',
    disabled: false,
    createdAt: '2026-08-20T02:00:00Z',
    identities: [{ id: 'id1', provider: 'github', externalId: 'sarahk', linkedVia: 'auto_email', createdAt: '2026-08-20T02:00:00Z' }],
    ...overrides,
  }
}

function baseAuditEntry(overrides: Partial<AuditLogEntry> = {}): AuditLogEntry {
  return {
    id: 'a1',
    actorUserId: 'u1',
    action: 'sandbox_secret.created',
    resourceType: 'sandbox_secret',
    resourceId: 'sec1',
    detail: { name: 'NPM_TOKEN' },
    correlationId: 'corr-1',
    createdAt: '2026-08-20T02:00:00Z',
    ...overrides,
  }
}

describe('MemberRow rendering -- adversarial displayName stays text', () => {
  it('a hostile displayName renders as text', () => {
    const html = withQueryClient(<MemberRow member={baseMember({ displayName: `Sarah ${XSS_SCRIPT}` })} canManage={false} onShowAudit={() => {}} />)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })
})

describe('MemberRow -- the Connected apps drawer', () => {
  it('an admin gets a Connected apps button beside the audit log, its drawer closed until opened', () => {
    const html = withQueryClient(<table><tbody><MemberRow member={baseMember()} canManage={true} onShowAudit={() => {}} /></tbody></table>)
    expect(html).toContain('>Connected apps<')
    expect(html).toContain('aria-expanded="false"')
    expect(html).toContain('>Audit log<')
    expect(html).not.toContain('memberdrawer')
  })

  it('no one else gets the button', () => {
    const html = withQueryClient(<table><tbody><MemberRow member={baseMember()} canManage={false} onShowAudit={() => {}} /></tbody></table>)
    expect(html).not.toContain('Connected apps')
  })
})

describe('AuditLogRow rendering -- adversarial action/resource/detail stays text, never markup', () => {
  it('a hostile action renders as text', () => {
    const html = renderToStaticMarkup(<table><tbody><AuditLogRow entry={baseAuditEntry({ action: `weird.action ${XSS_SCRIPT}` })} /></tbody></table>)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a hostile resourceId renders as text', () => {
    const html = renderToStaticMarkup(<table><tbody><AuditLogRow entry={baseAuditEntry({ resourceId: XSS_IMG })} /></tbody></table>)
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img')
  })

  it('a hostile string embedded in the opaque detail JSON blob renders as text, never markup', () => {
    const html = renderToStaticMarkup(<table><tbody><AuditLogRow entry={baseAuditEntry({ detail: { reason: XSS_SCRIPT } })} /></tbody></table>)
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;script&gt;')
  })

  it('a null correlationId renders a dash, never "null" as text', () => {
    const html = renderToStaticMarkup(<table><tbody><AuditLogRow entry={baseAuditEntry({ correlationId: null })} /></tbody></table>)
    expect(html).not.toContain('>null<')
    expect(html).toContain('—')
  })
})

// The identity column's whole job is to answer "how was this link proven?".
// It previously rendered a hard-coded green check on every row, so an
// admin force-link (linked_via=admin, nothing proven by the member, §13.2
// step 5) was indistinguishable from a verified one. These pin the
// distinction so it cannot silently collapse back into a constant.
describe('MemberRow identity proof marks', () => {
  it('marks an email-verified link as verified', () => {
    const html = withQueryClient(<MemberRow member={baseMember()} canManage={false} onShowAudit={() => {}} />)
    expect(html).toContain('idchip ok')
    expect(html).toContain('github ✓')
    expect(html).not.toContain('idchip pend')
  })

  it('marks a magic-link-confirmed link as verified', () => {
    const member = baseMember({ identities: [{ id: 'id1', provider: 'slack', externalId: 'U1', linkedVia: 'prompt', createdAt: '2026-08-20T02:00:00Z' }] })
    const html = withQueryClient(<MemberRow member={member} canManage={false} onShowAudit={() => {}} />)
    expect(html).toContain('idchip ok')
    expect(html).toContain('slack ✓')
  })

  it('does NOT mark an admin force-link as verified', () => {
    const member = baseMember({ identities: [{ id: 'id1', provider: 'github', externalId: 'sarahk', linkedVia: 'admin', createdAt: '2026-08-20T02:00:00Z' }] })
    const html = withQueryClient(<MemberRow member={member} canManage={false} onShowAudit={() => {}} />)
    expect(html).toContain('idchip pend')
    expect(html).not.toContain('github ✓')
    expect(html).toContain('Force-linked by an admin')
  })
})

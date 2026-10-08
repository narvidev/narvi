// autonomyFreezePanel.test.tsx -- Settings → General's autonomy freeze card
// (AutonomyFreezePanel.tsx, technical plan §40.2), rendered whole: every
// role sees the freeze, only an administrator is offered Freeze (with a
// required reason) and Unfreeze, and what each does to the cached freeze
// and the decision inbox, a 409 included.
//
// The suite runs in plain Node (no DOM), like repoEntitlementCardWiring.
// test.tsx: useQuery and useMutation are wrapped, never replaced -- the real
// hooks run against a QueryClient whose cache holds the server's answer --
// and each useMutation call's options are recorded on the way in, so a test
// can run a mutation's own onSuccess and onError against that cache.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { AutonomyFreeze } from '@narvi/contracts/rest-dtos'

import { ApiError } from '../../api/http'
import { autonomyFreezeQueryKeys, decisionInboxQueryKeys } from '../../api/queryKeys'
import { AutonomyFreezePanel, AutonomyFreezeStatus } from '../AutonomyFreezePanel'

const hooks = vi.hoisted(() => ({ mutations: [] as unknown[], forcedQuery: undefined as object | undefined }))

vi.mock('@tanstack/react-query', async (importOriginal) => {
  const actual = await importOriginal<typeof ReactQuery>()
  const useQuery = ((...args: Parameters<typeof actual.useQuery>) => {
    const result = actual.useQuery(...args)
    return hooks.forcedQuery === undefined ? result : { ...result, ...hooks.forcedQuery }
  }) as typeof actual.useQuery
  const useMutation = ((...args: Parameters<typeof actual.useMutation>) => {
    hooks.mutations.push(args[0])
    return actual.useMutation(...args)
  }) as typeof actual.useMutation
  return { ...actual, useQuery, useMutation }
})

afterEach(() => {
  hooks.mutations.length = 0
  hooks.forcedQuery = undefined
})

type CapturedMutation = { onSuccess?: (...args: unknown[]) => unknown; onError?: (...args: unknown[]) => unknown }

const KEY = autonomyFreezeQueryKeys.detail()
const INBOX = decisionInboxQueryKeys.list()

function notFrozen(): AutonomyFreeze {
  return { frozen: false, frozenAt: null, frozenByUserId: null, frozenByDisplayName: null, reason: null }
}

function frozen(): AutonomyFreeze {
  return {
    frozen: true,
    frozenAt: '2026-10-07T09:30:00Z',
    frozenByUserId: '7d1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b',
    frozenByDisplayName: 'Ada Admin',
    reason: 'an incident: hold every automatic action',
  }
}

// render renders the card for role over a cache holding freeze (and an
// inbox entry, to see it invalidated), and returns its markup, the client,
// and the freeze and unfreeze mutations' options, in the card's order.
function render(role: string | undefined, freeze: AutonomyFreeze | undefined) {
  hooks.mutations.length = 0
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  if (freeze !== undefined) {
    client.setQueryData(KEY, freeze)
  }
  client.setQueryData(INBOX, { items: [] })
  const html = renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <AutonomyFreezePanel role={role} />
    </QueryClientProvider>,
  )
  const [freezeMutation, unfreezeMutation] = hooks.mutations as CapturedMutation[]
  return { html, client, freeze: freezeMutation, unfreeze: unfreezeMutation }
}

describe('AutonomyFreezePanel -- controls by role and state', () => {
  it('an admin, not frozen, is offered Freeze with a required reason -- disabled while the reason is blank -- and no Unfreeze', () => {
    const { html } = render('admin', notFrozen())
    expect(html).toContain('Freeze autonomy')
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Freeze autonomy<\/button>/)
    expect(html).toContain('Reason (required, at most 500 characters)')
    expect(html).toContain('maxLength="500"')
    expect(html).not.toContain('>Unfreeze<')
  })

  it('an admin, frozen, is offered Unfreeze and no freeze form', () => {
    const { html } = render('admin', frozen())
    expect(html).toContain('>Unfreeze<')
    expect(html).not.toContain('Freeze autonomy')
  })

  for (const role of ['maintainer', 'member', 'viewer', undefined]) {
    it(`a ${role ?? 'not yet known'} role sees the state but neither control`, () => {
      for (const freeze of [notFrozen(), frozen()]) {
        const { html } = render(role, freeze)
        expect(html).not.toContain('Freeze autonomy')
        expect(html).not.toContain('>Unfreeze<')
        expect(html).toContain('Admin only.')
        expect(html).toContain(freeze.frozen ? 'an incident: hold every automatic action' : 'not frozen')
      }
    })
  }

  it('a failed read says the state is unknown, and offers no control', () => {
    hooks.forcedQuery = { status: 'error', fetchStatus: 'idle', isPending: false, isError: true, isSuccess: false, data: undefined, error: new ApiError(500, 'internal error', null) }
    const { html } = render('admin', undefined)
    expect(html).toContain('Couldn&rsquo;t read whether autonomy is frozen'.replace('&rsquo;', '’'))
    expect(html).not.toContain('Freeze autonomy')
    expect(html).not.toContain('>Unfreeze<')
    expect(html).not.toContain('not frozen')
  })
})

describe('AutonomyFreezePanel -- what freeze and unfreeze do to the cached freeze and the inbox', () => {
  it('a freeze writes the answered freeze into the cache and marks the inbox stale', () => {
    const { client, freeze } = render('admin', notFrozen())
    freeze.onSuccess?.(frozen(), undefined, undefined)
    expect(client.getQueryData(KEY)).toEqual(frozen())
    expect(client.getQueryState(INBOX)?.isInvalidated).toBe(true)
  })

  it('an unfreeze writes the answered freeze into the cache and marks the inbox stale', () => {
    const { client, unfreeze } = render('admin', frozen())
    unfreeze.onSuccess?.(notFrozen(), undefined, undefined)
    expect(client.getQueryData(KEY)).toEqual(notFrozen())
    expect(client.getQueryState(INBOX)?.isInvalidated).toBe(true)
  })

  for (const which of ['freeze', 'unfreeze'] as const) {
    it(`a ${which} refused with 409 marks the cached freeze stale, to be read again`, () => {
      const rendered = render('admin', which === 'freeze' ? notFrozen() : frozen())
      rendered[which].onError?.(new ApiError(409, which === 'freeze' ? 'autonomy is already frozen' : 'autonomy is not frozen', null), undefined, undefined)
      expect(rendered.client.getQueryState(KEY)?.isInvalidated).toBe(true)
    })

    it(`a ${which} failing otherwise leaves the cached freeze as it is`, () => {
      const rendered = render('admin', which === 'freeze' ? notFrozen() : frozen())
      rendered[which].onError?.(new ApiError(500, 'internal error', null), undefined, undefined)
      expect(rendered.client.getQueryState(KEY)?.isInvalidated).toBe(false)
    })
  }
})

describe('AutonomyFreezeStatus -- the freeze read-only, its free text as text', () => {
  it('names when, by whom and why, and that a person is never held', () => {
    const html = renderToStaticMarkup(<AutonomyFreezeStatus freeze={frozen()} />)
    expect(html).toContain('frozen')
    expect(html).toContain(new Date('2026-10-07T09:30:00Z').toLocaleString())
    expect(html).toContain('Ada Admin')
    expect(html).toContain('an incident: hold every automatic action')
    expect(html).toContain('own actions still work')
  })

  it('a freeze by a user who no longer exists says so', () => {
    const html = renderToStaticMarkup(<AutonomyFreezeStatus freeze={{ ...frozen(), frozenByUserId: null, frozenByDisplayName: null }} />)
    expect(html).toContain('a user who no longer exists')
  })

  it('an adversarial reason and display name stay text, never markup', () => {
    const html = renderToStaticMarkup(<AutonomyFreezeStatus freeze={{ ...frozen(), reason: '<script>alert(1)</script>', frozenByDisplayName: '<img src=x onerror=alert(1)>' }} />)
    expect(html).not.toContain('<script>')
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;script&gt;')
  })
})

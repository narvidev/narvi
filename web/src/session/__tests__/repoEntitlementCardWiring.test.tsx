// repoEntitlementCardWiring.test.tsx -- RepoEntitlementCard rendered whole
// (RepoSettingsView.tsx): which controls it offers to whom, what a 403
// shows, and what its revoke and restore do to the cached status, a 409
// included. repoEntitlementCard.test.tsx covers the read-only status text.
//
// The suite runs in plain Node (no DOM), like workflowRunsViewWiring.test.tsx:
// useQuery and useMutation are wrapped, never replaced -- the real hooks
// run against a QueryClient whose cache holds the server's answer -- and
// each call's options are recorded on the way in, so a test can run a
// mutation's own onSuccess and onError against that cache. A query result
// can be forced, to render the card as it is after a refused read.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type * as ReactQuery from '@tanstack/react-query'

import type { RepoEntitlement } from '@narvi/contracts/rest-dtos'

import { ApiError } from '../../api/http'
import { repoEntitlementQueryKeys } from '../../api/queryKeys'
import { RepoEntitlementCard } from '../RepoSettingsView'

// The options of every useMutation call, in call order, and a result the
// next useQuery reports instead of its own, when set.
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

const KEY = repoEntitlementQueryKeys.detail('acme/widgets')

function open(): RepoEntitlement {
  return { repoFullName: 'acme/widgets', revoked: false, revokedAt: null, revokedByUserId: null, revokedByDisplayName: null, reason: null }
}

function revoked(): RepoEntitlement {
  return {
    repoFullName: 'acme/widgets',
    revoked: true,
    revokedAt: '2026-10-01T12:00:00Z',
    revokedByUserId: '7d1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b',
    revokedByDisplayName: 'Rae Voker',
    reason: 'frozen for an audit',
  }
}

// render renders the card for role over a cache holding status, and
// returns its markup, the client, and the revoke and restore mutations'
// options, in the card's order.
function render(role: string, status: RepoEntitlement | undefined) {
  hooks.mutations.length = 0
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  if (status !== undefined) {
    client.setQueryData(KEY, status)
  }
  const html = renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <RepoEntitlementCard owner="acme" repo="widgets" role={role} />
    </QueryClientProvider>,
  )
  const [revoke, restore] = hooks.mutations as CapturedMutation[]
  return { html, client, revoke, restore }
}

describe('RepoEntitlementCard -- controls by role and state', () => {
  it('an admin on an open repository is offered Revoke, disabled while the reason is blank, and no Restore', () => {
    const { html } = render('admin', open())
    expect(html).toContain('Revoke new sessions')
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Revoke new sessions<\/button>/)
    expect(html).not.toContain('>Restore<')
  })

  it('an admin on a revoked repository is offered Restore and no revoke form', () => {
    const { html } = render('admin', revoked())
    expect(html).toContain('>Restore<')
    expect(html).not.toContain('Revoke new sessions')
  })

  for (const role of ['maintainer', 'member', 'viewer']) {
    it(`a ${role} sees the status but neither control`, () => {
      for (const status of [open(), revoked()]) {
        const { html } = render(role, status)
        expect(html).not.toContain('Revoke new sessions')
        expect(html).not.toContain('>Restore<')
        expect(html).toContain('Admin only.')
      }
    })
  }

  it('a refused read shows the admin-only note and no control', () => {
    hooks.forcedQuery = {
      status: 'error',
      fetchStatus: 'idle',
      isPending: false,
      isError: true,
      isSuccess: false,
      data: undefined,
      error: new ApiError(403, 'not authorized to perform this action', null),
    }
    const { html } = render('member', undefined)
    expect(html).toContain('Revoking and restoring a repository is admin-only.')
    expect(html).not.toContain('Revoke new sessions')
    expect(html).not.toContain('>Restore<')
  })
})

describe('RepoEntitlementCard -- what revoke and restore do to the cached status', () => {
  it('a revoke writes the answered status into the cache', () => {
    const { client, revoke } = render('admin', open())
    revoke.onSuccess?.(revoked(), undefined, undefined)
    expect(client.getQueryData(KEY)).toEqual(revoked())
  })

  it('a restore writes the answered status into the cache', () => {
    const { client, restore } = render('admin', revoked())
    restore.onSuccess?.(open(), undefined, undefined)
    expect(client.getQueryData(KEY)).toEqual(open())
  })

  for (const which of ['revoke', 'restore'] as const) {
    it(`a ${which} refused with 409 marks the cached status stale, to be read again`, () => {
      const rendered = render('admin', which === 'revoke' ? open() : revoked())
      rendered[which].onError?.(new ApiError(409, 'repository entitlement is already revoked', null), undefined, undefined)
      expect(rendered.client.getQueryState(KEY)?.isInvalidated).toBe(true)
    })

    it(`a ${which} failing otherwise leaves the cached status as it is`, () => {
      const rendered = render('admin', which === 'revoke' ? open() : revoked())
      rendered[which].onError?.(new ApiError(500, 'internal error', null), undefined, undefined)
      expect(rendered.client.getQueryState(KEY)?.isInvalidated).toBe(false)
    })
  }
})

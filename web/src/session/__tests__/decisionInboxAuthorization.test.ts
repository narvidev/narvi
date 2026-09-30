// decisionInboxAuthorization.test.ts -- the same "the affordance is not
// the real gate" proof settingsAuthorization.test.ts/
// automationAuthorization.test.ts establish elsewhere, applied to this
// view's own mergePullRequest (api/endpoints.ts): an unconditional network
// call with no client-side authorization logic of its own. The real gate
// is server-side, TWICE over -- a cheap role-only authz pre-check
// (authz.ActionMergePR, decisioninbox.go: "Viewer role sees the queue
// read-only") AND decisioninbox.RevalidateForMerge's own live CI/
// approval-state re-check against the actor's OWN GitHub identity at
// click time ("the rendered queue is never trusted as authority", decision
// 33) -- neither of which this client can see or short-circuit. This file
// proves the client half: the request really goes out over the wire, with
// the real method and body, and a 403/409 the server returns surfaces as
// a genuine, typed ApiError carrying the SERVER's own message, never a
// generic failure or a silently-swallowed one.
import { afterEach, describe, expect, it, vi } from 'vitest'

import { listDecisionInbox, mergePullRequest } from '../../api/endpoints'
import { ApiError } from '../../api/http'

afterEach(() => {
  vi.unstubAllGlobals()
})

function respondWith(body: unknown, status: number): ReturnType<typeof vi.fn> {
  const spy = vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } }))
  vi.stubGlobal('fetch', spy)
  return spy
}

describe('mergePullRequest -- the client always sends the real request', () => {
  it('calls POST /api/decision-inbox/merge unconditionally, regardless of the calling component\'s own canMerge/role check', async () => {
    const spy = respondWith({ merged: true, mergeCommitSha: 'abc123', message: 'Pull request merged' }, 200)

    const result = await mergePullRequest({ repoFullName: 'acme/widgets', prNumber: 42 })

    expect(spy).toHaveBeenCalledTimes(1)
    const [url, init] = spy.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/decision-inbox/merge')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({ repoFullName: 'acme/widgets', prNumber: 42 })
    expect(result).toEqual({ merged: true, mergeCommitSha: 'abc123', message: 'Pull request merged' })
  })

  // The two codes mean different things and it is worth naming them
  // precisely, because a reader builds error handling from these names.
  // 403 is the ROLE-ONLY gate (authz.ActionMergePR) plus the no-usable-git-
  // credential path -- decisioninbox.go writes it in exactly those places.
  // "This PR is not assigned to you" is NOT a 403: RevalidateForMerge returns
  // eligible=false and the handler writes 409 with the server's own reason.
  // An earlier version of this test name asserted the opposite, which would
  // have led a reader to invert the two.
  it('a 403 (a viewer, or no usable git credential -- the role-only authz.ActionMergePR gate) surfaces as a genuine ApiError carrying the server\'s own message', async () => {
    respondWith({ error: 'not authorized to perform this action' }, 403)

    await expect(mergePullRequest({ repoFullName: 'acme/widgets', prNumber: 42 })).rejects.toMatchObject({
      status: 403,
      message: 'not authorized to perform this action',
    })
  })

  it('a 409 (RevalidateForMerge\'s own live re-check failing -- CI not green, changes requested, the PR moved, or it is no longer assigned to this actor) surfaces the SERVER\'s own reason text, never a generic failure', async () => {
    respondWith({ error: 'this pull request has changes requested and cannot be merged' }, 409)

    await expect(mergePullRequest({ repoFullName: 'acme/widgets', prNumber: 42 })).rejects.toMatchObject({
      status: 409,
      message: 'this pull request has changes requested and cannot be merged',
    })
  })

  it('a 403 is never misattributed to anything other than authorization -- the ApiError carries the exact server string, not a client-invented one', async () => {
    respondWith({ error: 'not authorized to perform this action' }, 403)

    try {
      await mergePullRequest({ repoFullName: 'acme/widgets', prNumber: 42 })
      expect.unreachable('mergePullRequest should have thrown')
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError)
      expect((err as ApiError).message).toBe('not authorized to perform this action')
    }
  })
})

describe('listDecisionInbox -- the client always sends the real, unconditional read', () => {
  it('calls GET /api/decision-inbox with no query params -- needs_attention filtering is server-side only, never a client-side param', async () => {
    const spy = respondWith(
      { items: [], scmAsOf: null, scmFetchFailed: false, requiredChecksNotRead: false, decisionLatencyMedianSeconds: null, decisionLatencySampleSize: 0, decisionLatencyComputed: false },
      200,
    )

    await listDecisionInbox()

    expect(spy).toHaveBeenCalledTimes(1)
    const [url, init] = spy.mock.calls[0] as [string, RequestInit | undefined]
    expect(url).toBe('/api/decision-inbox')
    expect(init?.method ?? 'GET').toBe('GET')
  })

  it('a 500 surfaces as a genuine ApiError, never an empty queue silently standing in for a failed fetch', async () => {
    respondWith({ error: 'internal error' }, 500)
    await expect(listDecisionInbox()).rejects.toBeInstanceOf(ApiError)
  })
})

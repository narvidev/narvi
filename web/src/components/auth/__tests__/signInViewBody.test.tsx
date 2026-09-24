// signInViewBody.test.tsx (review round 2, finding P8) proves the ONE
// wiring point web/src/auth/__tests__/identityStatus.test.ts cannot
// reach: routes/sign-in.tsx's own `<IdentityStatusPanel ... oidcConfigured=
// {oidcConfigured} />` call site (now SignInViewBody's, see that file's
// own doc comment for why it moved here). That other test only calls
// deriveIdentityStatuses directly with a hand-supplied boolean -- it
// would keep passing even if the sign-in view stopped threading the REAL
// GET /auth/capabilities-derived value through at all (e.g. a future
// edit that hardcodes `oidcConfigured={true}`, reintroducing review
// round 1's own finding O12).
//
// SignInViewBody is a plain, hook-free rendering function -- no
// QueryClientProvider, no RouterProvider needed: MeQueryLike/
// LogoutMutationLike are a narrow, structural subset of react-query's own
// result types (that file's own doc comment), so a plain literal
// satisfies them. This codebase's own vitest.config.ts runs
// `environment: 'node'` deliberately (no jsdom, no
// @testing-library/react) and its own decisionInboxRendering.test.tsx/
// automationRendering.test.tsx precedent explicitly avoids building a
// real router harness for a <Link>-bearing component -- SignInViewBody's
// own extraction (out of the route file entirely) is what makes this
// component testable within that same established constraint, rather
// than reaching for one.
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'

import type { Member } from '@narvi/contracts/rest-dtos'

import { SignInViewBody, type MeQueryLike, type LogoutMutationLike } from '../SignInViewBody'

function signedInMeQuery(member: Member): MeQueryLike {
  return {
    isPending: false,
    isError: false,
    isSuccess: true,
    data: member,
    error: null,
    refetch: () => {},
  }
}

function idleLogoutMutation(): LogoutMutationLike {
  return { isPending: false, mutate: () => {} }
}

function baseMember(overrides: Partial<Member> = {}): Member {
  return {
    id: 'user-1',
    email: 'octocat@example.com',
    displayName: 'Octocat',
    role: 'member',
    disabled: false,
    createdAt: '2026-01-01T00:00:00Z',
    identities: [{ id: 'id1', provider: 'github', externalId: '555', linkedVia: 'admin', createdAt: '2026-01-01T00:00:00Z' }],
    ...overrides,
  }
}

function renderBody(oidcConfigured: boolean, member: Member) {
  return renderToStaticMarkup(
    <SignInViewBody
      search={{}}
      meQuery={signedInMeQuery(member)}
      oidcConfigured={oidcConfigured}
      onContinue={() => {}}
      logoutMutation={idleLogoutMutation()}
      logoutError={false}
    />,
  )
}

describe('SignInViewBody -- oidcConfigured wiring reaches IdentityStatusPanel', () => {
  it('shows the oidc row when capabilities report OIDC configured', () => {
    const html = renderBody(true, baseMember())
    expect(html).toContain('>oidc')
  })

  it('never shows the oidc row when capabilities report OIDC NOT configured', () => {
    const html = renderBody(false, baseMember())
    expect(html).not.toContain('>oidc')
  })

  it('still shows the always-mounted providers regardless of oidcConfigured', () => {
    const html = renderBody(false, baseMember())
    expect(html).toContain('>github')
    expect(html).toContain('>slack')
    expect(html).toContain('>linear')
  })
})

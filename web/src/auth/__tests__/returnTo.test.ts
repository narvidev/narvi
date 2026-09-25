// Proves auth/returnTo.ts's own isSafeReturnTo -- the client-side half of
// §13.1's redirect handling (defense in depth; the backend's
// internal/adapters/inbound/auth/login.go isSafeRedirectNext is the real
// authority and is proved separately by that package's own Go tests).
import { describe, expect, it } from 'vitest'

import { isSafeReturnTo, isServerRenderedReturnTo, KNOWN_RETURN_TO_ROUTES } from '../returnTo'

describe('isSafeReturnTo', () => {
  it('accepts every route in the known allowlist', () => {
    for (const route of KNOWN_RETURN_TO_ROUTES) {
      expect(isSafeReturnTo(route)).toBe(true)
    }
  })

  it('rejects an off-origin absolute URL', () => {
    expect(isSafeReturnTo('https://evil.example.test/')).toBe(false)
  })

  it('rejects a scheme-relative URL (the classic "//evil.example.test" open-redirect vector)', () => {
    expect(isSafeReturnTo('//evil.example.test/')).toBe(false)
  })

  it('rejects a backslash-prefixed variant some browsers still treat as scheme-relative', () => {
    expect(isSafeReturnTo('/\\evil.example.test/')).toBe(false)
  })

  it('rejects an empty string', () => {
    expect(isSafeReturnTo('')).toBe(false)
  })

  // This is THE mutation-test proof named in the Step's own verification
  // list: isSafeReturnTo must be an ALLOWLIST-OF-ROUTES check, never a
  // "starts with one slash" substring/prefix check -- a same-origin path
  // requirement alone still accepts any string merely SHAPED like a path,
  // real route or not. A path that satisfies that weaker, prefix-only
  // property but names no route this app actually has must still be
  // rejected here.
  it('rejects a same-origin-shaped path that is not a real, known route (proves this is a route allowlist, not a "starts with /" substring check)', () => {
    expect(isSafeReturnTo('/not-a-real-route')).toBe(false)
    expect(isSafeReturnTo('/sign-in/../../etc/passwd')).toBe(false)
    // A known route as a PREFIX of a longer path is still not an exact
    // match -- "/sign-in-evil" starts with "/sign-in" as a substring, but
    // is not "/sign-in" itself.
    expect(isSafeReturnTo('/sign-in-evil')).toBe(false)
  })
})

describe('isSafeReturnTo -- the allowlist must cover the routes that actually exist', () => {
  // It stopped at '/' and '/sign-in' while eleven more routes shipped, so a
  // signed-out operator following a deep link lost their destination. These
  // pin the two properties that matter together: a real route is kept, and
  // the guard is still an exact match rather than a prefix test.
  it.each(['/sessions', '/automations', '/workflows', '/settings', '/repo-settings', '/analytics'])(
    'keeps %s, a route this app really has',
    (route) => {
      expect(isSafeReturnTo(route)).toBe(true)
    },
  )

  it('keeps a session route and its children, id and all', () => {
    const id = '3f2504e0-4f89-11d3-9a0c-0305e82c3301'
    expect(isSafeReturnTo(`/session/${id}`)).toBe(true)
    expect(isSafeReturnTo(`/session/${id}/review`)).toBe(true)
    expect(isSafeReturnTo(`/session/${id}/runs`)).toBe(true)
  })

  it('still refuses anything that merely looks like one', () => {
    expect(isSafeReturnTo('/session/not-a-uuid')).toBe(false)
    expect(isSafeReturnTo('/settings/../../evil')).toBe(false)
    expect(isSafeReturnTo('/settingsevil')).toBe(false)
    expect(isSafeReturnTo('//evil.example')).toBe(false)
    expect(isSafeReturnTo('/session/3f2504e0-4f89-11d3-9a0c-0305e82c3301/evil')).toBe(false)
  })
})

describe('isSafeReturnTo -- the MCP consent page (technical plan §43.14)', () => {
  const id = '3f2504e0-4f89-11d3-9a0c-0305e82c3301'

  it('accepts exactly /oauth/consent?request=<uuid>, and marks it server-rendered', () => {
    expect(isSafeReturnTo(`/oauth/consent?request=${id}`)).toBe(true)
    expect(isServerRenderedReturnTo(`/oauth/consent?request=${id}`)).toBe(true)
    expect(isServerRenderedReturnTo('/settings')).toBe(false)
  })

  it('refuses every near miss', () => {
    for (const path of [
      '/oauth/consent',
      '/oauth/consent?request=not-a-uuid',
      `/oauth/consent?request=${id.toUpperCase()}`,
      `/oauth/consent?request=${id}&next=//evil.example`,
      `/oauth/consent?request=${id}#frag`,
      `/oauth/authorize?request=${id}`,
      `/oauth/consent/?request=${id}`,
      `//evil.example/oauth/consent?request=${id}`,
      `https://evil.example/oauth/consent?request=${id}`,
    ]) {
      expect(isSafeReturnTo(path), path).toBe(false)
    }
  })
})

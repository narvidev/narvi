// Proves auth/loginLinks.ts -- the two actual call sites where a
// validated `next`/return-to value is placed into a URL this app
// constructs. "if you implemented a return-to, an off-origin destination
// is refused" -- §13.1's own requirement on redirect handling.
import { describe, expect, it } from 'vitest'

import { continueNavigation, githubLoginHref, oidcLoginHref, safeContinueTarget } from '../loginLinks'

const consentPath = '/oauth/consent?request=3f2504e0-4f89-11d3-9a0c-0305e82c3301'

describe('githubLoginHref', () => {
  it('appends next when it is a known, safe route', () => {
    expect(githubLoginHref('/sign-in')).toBe('/auth/github/login?next=%2Fsign-in')
  })

  it('omits next entirely when undefined', () => {
    expect(githubLoginHref(undefined)).toBe('/auth/github/login')
  })

  it('omits next when it is an off-origin absolute URL (the open-redirect case)', () => {
    expect(githubLoginHref('https://evil.example.test/')).toBe('/auth/github/login')
  })

  it('omits next when it is a scheme-relative "//evil" URL', () => {
    expect(githubLoginHref('//evil.example.test/')).toBe('/auth/github/login')
  })

  it('omits next when it names no known route (allowlist rejection, not just malformed input)', () => {
    expect(githubLoginHref('/not-a-real-route')).toBe('/auth/github/login')
  })
})

describe('oidcLoginHref', () => {
  it('returns the bare OIDC login route when next is undefined', () => {
    expect(oidcLoginHref(undefined)).toBe('/auth/oidc/login')
  })

  it('appends next under the same rule as githubLoginHref', () => {
    expect(oidcLoginHref('/settings')).toBe('/auth/oidc/login?next=%2Fsettings')
    expect(oidcLoginHref(consentPath)).toBe(`/auth/oidc/login?next=${encodeURIComponent(consentPath)}`)
  })

  it('omits an unsafe next', () => {
    expect(oidcLoginHref('//evil.example.test/')).toBe('/auth/oidc/login')
    expect(oidcLoginHref('/not-a-real-route')).toBe('/auth/oidc/login')
  })
})

describe('the MCP consent page as a sign-in return target (technical plan §43.14)', () => {
  it('githubLoginHref carries it through sign-in', () => {
    expect(githubLoginHref(consentPath)).toBe(`/auth/github/login?next=${encodeURIComponent(consentPath)}`)
  })

  it('continueNavigation leaves the SPA with a full page load for it, and navigates client-side for a SPA route', () => {
    expect(continueNavigation(consentPath)).toEqual({ kind: 'document', href: consentPath })
    expect(continueNavigation('/settings')).toEqual({ kind: 'spa', to: '/settings' })
    expect(continueNavigation(undefined)).toEqual({ kind: 'spa', to: '/' })
    expect(continueNavigation('https://evil.example.test/')).toEqual({ kind: 'spa', to: '/' })
  })
})

describe('safeContinueTarget', () => {
  it('returns next when it is a known, safe route', () => {
    expect(safeContinueTarget('/sign-in')).toBe('/sign-in')
  })

  it('falls back to "/" when next is undefined', () => {
    expect(safeContinueTarget(undefined)).toBe('/')
  })

  it('falls back to "/" when next is an off-origin URL', () => {
    expect(safeContinueTarget('https://evil.example.test/')).toBe('/')
  })
})

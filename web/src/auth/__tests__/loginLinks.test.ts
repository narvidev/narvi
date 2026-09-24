// Proves auth/loginLinks.ts -- the two actual call sites where a
// validated `next`/return-to value is placed into a URL this app
// constructs. "if you implemented a return-to, an off-origin destination
// is refused" -- §13.1's own requirement on redirect handling.
import { describe, expect, it } from 'vitest'

import { githubLoginHref, oidcLoginHref, safeContinueTarget } from '../loginLinks'

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
  it('returns the fixed OIDC login route, no query string', () => {
    expect(oidcLoginHref()).toBe('/auth/oidc/login')
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

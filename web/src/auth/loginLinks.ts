// loginLinks.ts (§13.1's own post-sign-in redirect handling) is routes/sign-in.tsx's
// own pure logic for the two places a validated `next` destination is
// actually used -- split out (rather than inlined in that route file) so
// each has a direct unit test, and so sign-in.tsx's own file stays
// component-exports-only for oxlint's react-refresh rule.
import { isSafeReturnTo, isServerRenderedReturnTo } from './returnTo'

/**
 * githubLoginHref builds the GitHub-login navigation target: `next` is
 * appended ONLY when isSafeReturnTo accepts it; an absent/unsafe value is
 * silently dropped rather than forwarded, matching internal/adapters/
 * inbound/auth/login.go's own "absent or unsafe next is silently
 * ignored" behavior server-side (that check is the real authority --
 * this one is defense in depth, applied BEFORE the value is ever placed
 * in a URL this app constructs, see auth/returnTo.ts's own top comment).
 */
export function githubLoginHref(next: string | undefined): string {
  if (next !== undefined && isSafeReturnTo(next)) {
    return `/auth/github/login?next=${encodeURIComponent(next)}`
  }
  return '/auth/github/login'
}

/**
 * oidcLoginHref builds the OIDC SSO-login navigation target (§41.3),
 * carrying `next` under exactly githubLoginHref's rule: appended only when
 * isSafeReturnTo accepts it. internal/adapters/inbound/auth's
 * NewOIDCLoginHandler honors it the same way the GitHub handler does
 * (its own narvi_oidc_next cookie, re-validated server-side) -- which is
 * what lets an OIDC-only deployment return a signed-out visitor to the
 * MCP consent page (technical plan §43.14).
 */
export function oidcLoginHref(next: string | undefined): string {
  if (next !== undefined && isSafeReturnTo(next)) {
    return `/auth/oidc/login?next=${encodeURIComponent(next)}`
  }
  return '/auth/oidc/login'
}

/** safeContinueTarget is the already-signed-in state's own "Continue" destination -- same validation, same fallback ("/"), for the same reason. */
export function safeContinueTarget(next: string | undefined): string {
  return next !== undefined && isSafeReturnTo(next) ? next : '/'
}

/**
 * continueNavigation says HOW the already-signed-in "Continue" reaches
 * safeContinueTarget(next): client-side navigation for a route of this
 * SPA, a full page load for a safe server-rendered page (the MCP consent
 * page) that the SPA router would otherwise answer "not found".
 */
export function continueNavigation(next: string | undefined): { kind: 'spa'; to: string } | { kind: 'document'; href: string } {
  const target = safeContinueTarget(next)
  return isServerRenderedReturnTo(target) ? { kind: 'document', href: target } : { kind: 'spa', to: target }
}

// loginLinks.ts (§13.1's own post-sign-in redirect handling) is routes/sign-in.tsx's
// own pure logic for the two places a validated `next` destination is
// actually used -- split out (rather than inlined in that route file) so
// each has a direct unit test, and so sign-in.tsx's own file stays
// component-exports-only for oxlint's react-refresh rule.
import { isSafeReturnTo } from './returnTo'

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
 * oidcLoginHref builds the OIDC SSO-login navigation target (§41.3).
 * Deliberately carries no `?next=` -- unlike githubLoginHref above,
 * internal/adapters/inbound/auth's own NewOIDCLoginHandler supports no
 * such parameter (a deliberate v1 scope cut: nothing today sends a
 * signed-out visitor through the OIDC flow expecting a specific landing
 * page the way identitylink's magic-link consume handler does through
 * GitHub's), so adding one here would be a link that silently does
 * nothing on the server side.
 */
export function oidcLoginHref(): string {
  return '/auth/oidc/login'
}

/** safeContinueTarget is the already-signed-in state's own "Continue" destination -- same validation, same fallback ("/"), for the same reason. */
export function safeContinueTarget(next: string | undefined): string {
  return next !== undefined && isSafeReturnTo(next) ? next : '/'
}

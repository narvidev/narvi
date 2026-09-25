// returnTo.ts (§13.1's own post-sign-in redirect handling) validates a post-sign-in
// destination BEFORE it is ever placed in a URL this app constructs (a
// <Link>/redirect() target, or the `next` query param handed to
// GET /auth/github/login). This is client-side defense in depth, not the
// authority: internal/adapters/inbound/auth/login.go's own
// isSafeRedirectNext already re-validates server-side (and
// callback.go re-checks it AGAIN before actually redirecting), so a
// forged/tampered value reaching the backend directly is still refused
// there regardless of what this file does. What this file is FOR is
// picking which client-side navigation target this app itself ever
// offers a visitor in the first place (the `next` search param a route
// guard attaches when it redirects an unauthenticated visitor to
// /sign-in) -- it must never construct one from attacker-influenced
// input without checking it here first.
//
// Deliberately an ALLOWLIST OF KNOWN ROUTES, not a "starts with one slash,
// not two" format check (which is exactly what login.go's own
// isSafeRedirectNext already does, server-side): a same-origin PATH
// requirement alone still accepts any string shaped like a path, real
// route or not, which is a weaker property than "is this route this
// app actually has and would sensibly land a visitor back on." A
// substring/prefix check (`path.startsWith('/')`) is exactly the
// weakened form this file's own tests prove insufficient -- see
// __tests__/returnTo.test.ts's own mutation-test block.
// Every top-level route this app has. It stopped at '/' and '/sign-in'
// while eleven more shipped around it, so a signed-out operator following
// a deep link -- the normal way one arrives, from a digest or a mention --
// had their destination silently dropped and landed on the home view.
// The failure was closed rather than open, which is why nothing noticed.
//
// Session-scoped routes are matched by shape rather than listed, since
// they carry an id: the guard is still an exact match against a known
// route, with the id segment validated, never a prefix test.
const KNOWN_RETURN_TO_ROUTES: readonly string[] = [
  '/',
  '/sign-in',
  '/sessions',
  '/automations',
  '/workflows',
  '/settings',
  '/repo-settings',
  '/analytics',
]

// A session route and its children: /session/<uuid>[/plan|review|release-review|runs].
const SESSION_ROUTE_PATTERN =
  /^\/session\/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}(\/(plan|review|release-review|runs))?$/

// The MCP consent page (technical plan §43.14) is the one SERVER-rendered
// page a sign-in may return to: GET /oauth/authorize sends a signed-out
// browser to /sign-in?next=/oauth/consent?request=<uuid>, and nothing
// else. Exactly that shape -- a lower-case uuid and no other parameter --
// never a prefix of it. Because it is not a route of this SPA, reaching it
// takes a full page load (isServerRenderedReturnTo), never client-side
// navigation.
const MCP_CONSENT_RETURN_PATTERN =
  /^\/oauth\/consent\?request=[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

/**
 * isSafeReturnTo reports whether path is one of this app's own known,
 * top-level route paths -- an EXACT match against KNOWN_RETURN_TO_ROUTES
 * above, never a prefix/substring test. A future Step that adds a real
 * route (e.g. a session workspace at "/sessions/$sessionId") extends this
 * list (or, for a genuinely parameterized route, adds a narrow,
 * anchored-regex entry alongside it) -- silently falling through to "deny"
 * for anything not yet listed is the safe default in the meantime, not a
 * bug to route around with a looser check.
 */
export function isSafeReturnTo(path: string): boolean {
  return KNOWN_RETURN_TO_ROUTES.includes(path) || SESSION_ROUTE_PATTERN.test(path) || isServerRenderedReturnTo(path)
}

/**
 * isServerRenderedReturnTo reports whether path is a safe return target
 * that this SPA does NOT render -- today only the MCP consent page -- so
 * the caller must leave the SPA with a full page load to reach it.
 */
export function isServerRenderedReturnTo(path: string): boolean {
  return MCP_CONSENT_RETURN_PATTERN.test(path)
}

export { KNOWN_RETURN_TO_ROUTES }

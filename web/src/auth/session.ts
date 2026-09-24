// session.ts (§13.1/§13.2) is the sign-in view's own small
// "who is the current caller" layer, built on the §12.1 data layer
// (src/api/) rather than growing a parallel one: GET /api/me IS this
// app's only session-state signal -- there is no separate client-side
// "am I logged in" flag anywhere, by design (see this file's own
// meQueryOptions doc comment). Nothing in this module ever reads or
// writes a token/credential of any kind: the backend-issued session
// cookie (HttpOnly, host-scoped) is the ONLY thing that carries auth
// state across a request, and this module never needs to see its value
// to ask "is it still good" -- that is exactly what GET /api/me answers,
// server-side, on the SAME cookie the browser already attaches
// automatically (http.ts's own `credentials: 'same-origin'`).
import { queryOptions, type QueryClient } from '@tanstack/react-query'

import { authQueryKeys } from '../api/queryKeys'
import { getMe, getAuthCapabilities } from '../api/endpoints'
import { ApiError, setUnauthorizedHandler } from '../api/http'

/**
 * meQueryOptions is the ONE query definition every "am I signed in, and
 * as whom" check in this app shares (route guards via
 * queryClient.ensureQueryData, the sign-in view's own already-signed-in
 * state via useQuery) -- sharing one definition means they also share
 * ONE cache entry, so a route guard's own fetch (on navigation) and the
 * sign-in view's own render (if it happens to also be mounted) never
 * race each other into two independent, possibly-inconsistent requests.
 *
 * retry: false is load-bearing, not a style choice: TanStack Query's
 * default retry behavior treats every rejected queryFn identically, but a
 * 401 here is not a transient failure to retry past -- it is the
 * meaningful, expected answer for "this visitor is not signed in", and
 * retrying it would just delay every unauthenticated route guard by
 * several seconds for no benefit. A GENUINE transient failure (network
 * blip, 500) still surfaces once, as this query's own error state -- see
 * the sign-in view's own "error" state for how that renders.
 */
export const meQueryOptions = queryOptions({
  queryKey: authQueryKeys.me(),
  queryFn: ({ signal }) => getMe(signal),
  retry: false,
  // A signed-in visitor's role/identities do not change from one
  // navigation to the next in the common case; installUnauthorizedHandler
  // below (not a short staleTime) is what catches the one case they DO --
  // an expired/revoked session -- promptly, from wherever in the app it
  // is first noticed.
  staleTime: 60_000,
})

/**
 * authCapabilitiesQueryOptions (§41.3) backs GET /auth/capabilities --
 * the sign-in view's own "is OIDC configured" probe. Deliberately a
 * SEPARATE query from meQueryOptions above: this one is PUBLIC (never
 * 401s -- an unconfigured deployment still returns 200 with
 * `oidcConfigured: false`), so it is safe, and correct, to fetch even
 * while signed out -- exactly when the sign-in view needs the answer.
 * staleTime matches meQueryOptions' own reasoning: whether this
 * deployment has OIDC configured is boot-time config, not something that
 * changes within a browser session.
 */
export const authCapabilitiesQueryOptions = queryOptions({
  queryKey: authQueryKeys.capabilities(),
  queryFn: ({ signal }) => getAuthCapabilities(signal),
  staleTime: 60_000,
})

/**
 * isSignedOut reports whether err is exactly the "no valid session"
 * signal meQueryOptions' own queryFn produces (a 401 ApiError) -- the one
 * error shape every caller of this query (route guards, the sign-in
 * view) must treat as "show sign-in", as opposed to any OTHER error
 * (network failure, 500), which must surface honestly as this Step's own
 * "error" state instead of being silently folded into "signed out".
 */
export function isSignedOut(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401
}

/**
 * installUnauthorizedHandler wires http.ts's own generic, app-agnostic
 * onUnauthorized hook (see that file's own doc comment) to THIS app's one
 * real consequence: mark the cached meQueryOptions entry INVALIDATED, so
 * the next thing that reads it gets a fresh answer instead of a cached
 * "still signed in" -- catching a session that expires or is revoked
 * mid-use sooner than this query's own 60s staleTime alone would.
 *
 * That promise is only true of readers that actually consult invalidation.
 * It was not true when written: requireAuth used ensureQueryData, which
 * short-circuits on any cached data and consults neither staleness nor
 * invalidation, so the one reader this comment names ignored it and a
 * signed-out visitor passed the route guard. requireAuth now uses
 * fetchQuery, whose isStaleByTime does honour isInvalidated -- see that
 * file's own comment for why the fix belongs at the reader rather than
 * here.
 *
 * refetchType: 'none' is load-bearing, not a style choice: the default
 * ('active') would immediately re-run every MOUNTED consumer of this
 * query, including meQueryOptions itself when GET /api/me is the very
 * request that just produced this 401 -- refetching immediately would
 * hit /api/me again, get 401 again, invalidate again, in an unbounded
 * tight refetch loop (caught live: the sign-in view's own "already
 * signed in" state, still mounted right after a real sign-out, spammed
 * dozens of 401s in the browser's own network log before this was
 * fixed). 'none' marks the entry stale without forcing that immediate
 * re-fetch; useQuery(meQueryOptions) still shows its already-settled
 * result (correctly erroring, in the case that started this) until
 * something ELSE actually asks for it again.
 */
export function installUnauthorizedHandler(queryClient: QueryClient): void {
  setUnauthorizedHandler(() => {
    void queryClient.invalidateQueries({ queryKey: authQueryKeys.me(), refetchType: 'none' })
  })
}

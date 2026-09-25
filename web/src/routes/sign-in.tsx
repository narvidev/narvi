// The sign-in view (§12.2 item 7/§13.1, mockups.html's own
// `v-signin`): GitHub OAuth as the primary login, SSO (OIDC) rendered as
// a configuration-gated secondary (see the ssobtn block below for why it
// is disabled today), the identity auto-link status panel for an
// already-signed-in visitor, and an honest, non-leaking denied state.
//
// # Where the session token lives (it never reaches this file)
//
// GET /auth/github/login (the ghbtn href below) is a PLAIN, real
// navigation, not a fetch call: the browser leaves this SPA entirely,
// GitHub authenticates the visitor, and GitHub's own redirect lands the
// TOP-LEVEL browser tab back on GET /auth/github/callback --
// internal/adapters/inbound/auth/callback.go, which mints the session and
// sets it as an HttpOnly, host-scoped cookie (platform.
// WithAuthSessionCookie) before finally 302-redirecting to "/" or `next`.
// No JavaScript on this page ever sees the OAuth code, the state value,
// or the session token -- there is nothing here TO hold, by construction
// of the flow itself, not by this file's own discipline.
//
// # The "already signed in" / "loading" / "error" states
//
// meQueryOptions (auth/session.ts) is the ONE source of truth for "am I
// signed in, and as whom" -- isPending is the loading state (a lightweight
// skeleton, so a visitor who IS already signed in never sees a flash of
// the sign-in buttons before this resolves); a genuine failure (network
// error, 500 -- NOT the expected 401) is `isError && !isSignedOut(error)`,
// rendered honestly with a retry action rather than silently treated as
// "not signed in". "Expired session" is deliberately NOT a fourth,
// distinct state: internal/adapters/inbound/auth/middleware.go's own
// Authenticate collapses missing/expired/disabled into the identical 401
// ("an attacker probing... gets no signal... either way", that file's own
// comment) -- this view mirrors that same non-differentiation rather than
// inventing a client-side distinction the backend deliberately does not
// expose; installUnauthorizedHandler (auth/session.ts, wired once in
// main.tsx) is what catches a session expiring mid-use ANYWHERE in the
// app, not just on this route.
//
// # The "denied" state
//
// See components/auth/DeniedNotice.tsx's own doc comment: the real
// GitHub-OAuth denial (callback.go's allowlist-rejection branches) is a
// full top-level navigation that responds directly with its own
// plain-text page, never routing through this SPA. Nothing in this
// codebase sets `?denied=1` today -- this is honestly inert groundwork
// wired up because §12.2 item 7 names "allowlist errors" as this Step's
// own content and a real, tested rendering path is more useful than an
// unbuilt one, NOT because a live trigger exists yet.
//
// # Why the rendering itself lives elsewhere (review round 2, finding P8)
//
// SignInViewBody (components/auth/SignInViewBody.tsx) holds every
// rendering decision this view makes, including oidcConfigured's own path
// through to IdentityStatusPanel -- moved out of this file entirely
// rather than merely extracted-and-re-exported here, because
// createFileRoute('/sign-in') below breaks its own literal-path type
// inference the moment a SECOND module (a test) imports any other named
// export straight out of this file (see that file's own doc comment for
// the exact TS error). SignInView, below, is the only thing left in this
// file that calls Route.useSearch()/useQuery()/useMutation() -- it reads
// its hooks' current values and passes them straight into
// SignInViewBody unchanged.
import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { meQueryOptions, authCapabilitiesQueryOptions } from '../auth/session'
import { continueNavigation } from '../auth/loginLinks'
import { logout } from '../api/endpoints'
import { authQueryKeys } from '../api/queryKeys'
import { SignInViewBody, type SignInSearch } from '../components/auth/SignInViewBody'
import '../styles/signin.css'

export const Route = createFileRoute('/sign-in')({
  // Omits a key entirely rather than writing an explicit `false`/
  // `undefined` -- TanStack Router round-trips this return value back
  // into the URL's own query string, so an object always carrying both
  // keys would put a permanent, noisy "?denied=false" on every visit to
  // this route's plain, no-param form.
  validateSearch: (search: Record<string, unknown>): SignInSearch => {
    const out: SignInSearch = {}
    if (typeof search.next === 'string') {
      out.next = search.next
    }
    if (search.denied === true || search.denied === '1' || search.denied === 'true') {
      out.denied = true
    }
    return out
  },
  component: SignInView,
})

function SignInView() {
  const search = Route.useSearch()
  const meQuery = useQuery(meQueryOptions)
  // authCapabilitiesQuery (§41.3) is PUBLIC -- fetched regardless of
  // meQuery's own state, since the one place its answer matters is the
  // signed-out rendering branch below, which by definition runs before
  // any session exists. A failure here degrades to the SAME "disabled,
  // configuration-gated" rendering the button always had before this
  // Step -- never a loading spinner or an error blocking the rest of the
  // sign-in view, since GitHub sign-in must keep working even if this
  // one small probe is unreachable.
  const authCapabilitiesQuery = useQuery(authCapabilitiesQueryOptions)
  const oidcConfigured = authCapabilitiesQuery.data?.oidcConfigured ?? false
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [logoutError, setLogoutError] = useState(false)

  const logoutMutation = useMutation({
    mutationFn: () => logout(),
    onSuccess: async () => {
      setLogoutError(false)
      // The cookie is already cleared server-side (logout.go always
      // clears it, even on a no-op/already-gone session) -- invalidating
      // this ONE cache entry is what makes every consumer (this view
      // included) reactively notice, without a full page reload.
      await queryClient.invalidateQueries({ queryKey: authQueryKeys.me() })
    },
    onError: () => setLogoutError(true),
  })

  return (
    <SignInViewBody
      search={search}
      meQuery={meQuery}
      oidcConfigured={oidcConfigured}
      onContinue={() => {
        // A server-rendered return target (the MCP consent page) is not a
        // route of this SPA: leave it with a full page load.
        const nav = continueNavigation(search.next)
        if (nav.kind === 'document') {
          window.location.assign(nav.href)
        } else {
          void navigate({ to: nav.to })
        }
      }}
      logoutMutation={logoutMutation}
      logoutError={logoutError}
    />
  )
}

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
import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { meQueryOptions, authCapabilitiesQueryOptions, isSignedOut } from '../auth/session'
import { githubLoginHref, oidcLoginHref, safeContinueTarget } from '../auth/loginLinks'
import { logout } from '../api/endpoints'
import { authQueryKeys } from '../api/queryKeys'
import { IdentityStatusPanel } from '../components/auth/IdentityStatusPanel'
import { DeniedNotice } from '../components/auth/DeniedNotice'
import { SignedInGreeting } from '../components/auth/SignedInGreeting'
import '../styles/signin.css'

interface SignInSearch {
  /** Where to send a visitor after a successful sign-in -- re-validated by isSafeReturnTo (auth/returnTo.ts) at EVERY point it is actually used below, never trusted just because it parsed here. */
  next?: string
  /** See this file's own top "denied state" doc comment -- nothing sets this today. */
  denied?: boolean
}

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
    <div className="signin">
      <div className="authcard">
        <span className="logo">
          <svg width="20" height="20" viewBox="0 0 18 18" fill="none" aria-hidden="true">
            <circle cx="9" cy="9" r="7.2" stroke="var(--accent)" strokeWidth="2.1" />
            <circle cx="9" cy="9" r="2.6" fill="var(--accent)" />
          </svg>
          narvi
        </span>
        <p>Background coding agents for your team</p>

        {search.denied && <DeniedNotice />}

        {meQuery.isPending && (
          <div className="signin-notice" aria-live="polite">
            <p>Checking your sign-in status…</p>
          </div>
        )}

        {meQuery.isError && !isSignedOut(meQuery.error) && (
          <div className="signin-notice signin-notice-error" role="alert">
            <p>Couldn't check your sign-in status. This is a connection problem, not a rejection.</p>
            <button type="button" className="ssobtn" onClick={() => void meQuery.refetch()}>
              Try again
            </button>
          </div>
        )}

        {meQuery.isSuccess && (
          <>
            <SignedInGreeting member={meQuery.data} />
            <IdentityStatusPanel member={meQuery.data} oidcConfigured={oidcConfigured} />
            <button
              type="button"
              className="ghbtn"
              onClick={() => void navigate({ to: safeContinueTarget(search.next) })}
            >
              Continue
            </button>
            <button
              type="button"
              className="ssobtn"
              disabled={logoutMutation.isPending}
              onClick={() => logoutMutation.mutate()}
            >
              {logoutMutation.isPending ? 'Signing out…' : 'Sign out'}
            </button>
            {logoutError && (
              <p className="signin-notice-sub" role="alert">
                Sign-out failed. Try again.
              </p>
            )}
          </>
        )}

        {meQuery.isError && isSignedOut(meQuery.error) && (
          <>
            <a className="ghbtn" href={githubLoginHref(search.next)}>
              <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
                <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.42 7.42 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
              </svg>
              Continue with GitHub
            </a>
            <div className="ordiv">or</div>
            {/* SSO (OIDC) is rendered per §12.2 item 7 / §12.1's "auth
                pluggable" for visual parity with the mockup -- §41.3 made
                it real: oidcConfigured (above) is GET /auth/capabilities'
                own live answer, PUBLIC and fetched even while signed out,
                for exactly this branch. Enabled and a real link to
                /auth/oidc/login iff the backend reports it configured;
                otherwise the SAME disabled, honestly-captioned button
                this view has always rendered for "unconfigured" (matches
                this codebase's own "no X affordance at all when the
                capability is unconfigured" convention elsewhere, e.g. the
                §27.3 signing-key-rotation UI) -- never a live link to a
                route that would refuse, and never silently omitting the
                affordance the visual spec calls for. */}
            {oidcConfigured ? (
              <a className="ssobtn" href={oidcLoginHref()}>
                Continue with SSO (OIDC)
              </a>
            ) : (
              <>
                <button type="button" className="ssobtn" disabled title="Not configured for this deployment">
                  Continue with SSO (OIDC)
                </button>
                <p className="signin-notice-sub">SSO becomes available once your organization configures it.</p>
              </>
            )}

            <div className="linknote">
              <span className="lt">Identities link themselves</span>
              <span className="idrow">
                On first contact from Slack or Linear, a verified-email match connects your account automatically;
                an ambiguous match sends a one-time link instead of guessing.
              </span>
            </div>
          </>
        )}

        <p className="allow">
          Access limited to allowed domains &amp; GitHub orgs · your GitHub token is also used to attribute PRs to
          you
        </p>
      </div>
    </div>
  )
}

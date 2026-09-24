// SignInViewBody (review round 2, finding P8) is routes/sign-in.tsx's own
// rendering logic, extracted into its OWN, non-route module -- NOT left
// inline in that file, and NOT merely re-exported from it, because
// TanStack Router's createFileRoute('/sign-in') special-cases how that
// file's OWN exports type-check: importing any other named export
// straight out of a route file (even a plain component, from a test)
// broke createFileRoute's own literal-path inference elsewhere in the
// SAME file ("Argument of type '\"/sign-in\"' is not assignable to
// parameter of type 'undefined'"), the moment a second module (a test)
// imported alongside `Route`. Moving the testable part here, a module
// TanStack Router's route-tree codegen never touches at all, is what
// makes it importable from a test with no such breakage -- mirrors this
// codebase's own established "components/auth/*.tsx holds the
// presentational pieces, routes/*.tsx wires them to hooks" split
// (IdentityStatusPanel/DeniedNotice/SignedInGreeting already live here,
// not in routes/sign-in.tsx).
//
// MeQueryLike/LogoutMutationLike are the narrow subset of
// useQuery(meQueryOptions)/useMutation(...)'s own return shape this
// component actually reads -- not react-query's own (considerably wider,
// discriminated-union) UseQueryResult/UseMutationResult types. A real
// query/mutation result object structurally satisfies these already, so
// the production caller (routes/sign-in.tsx's own SignInView) passes
// theirs straight through unchanged; a test supplies a plain, hook-free
// literal instead. This is what makes THIS component testable via
// renderToStaticMarkup with neither a QueryClientProvider nor a real
// router context (Route.useSearch(), which only SignInView itself still
// calls) -- every OTHER rendering decision on this page, including
// oidcConfigured's own path all the way to IdentityStatusPanel, lives
// here, in a plain, props-only function, exactly like this codebase's
// own established DecisionInboxRow/MemberRow precedent.
import type { Member } from '@narvi/contracts/rest-dtos'

import { isSignedOut } from '../../auth/session'
import { githubLoginHref, oidcLoginHref } from '../../auth/loginLinks'
import { IdentityStatusPanel } from './IdentityStatusPanel'
import { DeniedNotice } from './DeniedNotice'
import { SignedInGreeting } from './SignedInGreeting'

export interface SignInSearch {
  /** Where to send a visitor after a successful sign-in -- re-validated by isSafeReturnTo (auth/returnTo.ts) at EVERY point it is actually used below, never trusted just because it parsed here. */
  next?: string
  /** See routes/sign-in.tsx's own top "denied state" doc comment -- nothing sets this today. */
  denied?: boolean
}

export interface MeQueryLike {
  isPending: boolean
  isError: boolean
  isSuccess: boolean
  data: Member | undefined
  error: unknown
  refetch: () => void
}

export interface LogoutMutationLike {
  isPending: boolean
  mutate: () => void
}

export interface SignInViewBodyProps {
  search: SignInSearch
  meQuery: MeQueryLike
  /**
   * oidcConfigured (§41.3, review round 1 finding O12) is ALREADY the
   * resolved `authCapabilitiesQuery.data?.oidcConfigured ?? false` value
   * by the time it reaches here -- routes/sign-in.tsx's own SignInView is
   * the only place that derivation happens, so a test rendering THIS
   * component supplies the boolean directly, exactly as it would supply
   * any other already-resolved prop.
   */
  oidcConfigured: boolean
  onContinue: () => void
  logoutMutation: LogoutMutationLike
  logoutError: boolean
}

export function SignInViewBody({ search, meQuery, oidcConfigured, onContinue, logoutMutation, logoutError }: SignInViewBodyProps) {
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

        {meQuery.isSuccess && meQuery.data && (
          <>
            <SignedInGreeting member={meQuery.data} />
            <IdentityStatusPanel member={meQuery.data} oidcConfigured={oidcConfigured} />
            <button type="button" className="ghbtn" onClick={onContinue}>
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

// identityStatus.ts (§12.2 item 7) is components/auth/
// IdentityStatusPanel.tsx's own pure, render-independent logic, split
// into its own module (rather than co-located in that component file) so
// the component file exports components only -- oxlint's own
// react(only-export-components) rule (fast-refresh boundary hygiene) --
// and so this logic has a plain unit test with no rendering involved at
// all (see __tests__/identityStatus.test.ts).
import type { Identity } from '@narvi/contracts/rest-dtos'

/**
 * DISPLAYED_PROVIDERS is the mockup's own fixed row order, extended by one
 * (§41.3): "google" is never a distinct login the sign-in view offers and
 * stays excluded, but "oidc" now is one (§13.1: "GitHub primary, generic
 * OIDC secondary") -- a signed-in caller's own OIDC identity, if any, is
 * exactly as real and exactly as worth showing here as their GitHub/Slack/
 * Linear ones.
 */
export const DISPLAYED_PROVIDERS = ['github', 'slack', 'linear', 'oidc'] as const satisfies readonly Identity['provider'][]

export type DisplayedProvider = (typeof DISPLAYED_PROVIDERS)[number]

export interface ProviderStatus {
  provider: DisplayedProvider
  connected: boolean
  linkedVia?: Identity['linkedVia']
}

/**
 * deriveIdentityStatuses is the panel's own pure logic: for each of
 * DISPLAYED_PROVIDERS, reports whether `identities` contains a row for it
 * (first match wins; the identities table's own UNIQUE(provider,
 * external_id) constraint never lets a user acquire two DIFFERENT
 * identities of the SAME provider anyway).
 *
 * oidcConfigured (review round 1, finding O12) gates the 'oidc' row
 * specifically: unlike github/slack/linear (always mounted, §13.1/§13.2),
 * a deployment with NARVI_OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET unset has
 * no second sign-in provider AT ALL (auth.OIDCConfig.Configured, §41.3)
 * -- the caller's own GET /auth/capabilities response is the one signal
 * a signed-in view has for this (the SAME oidcConfigured the sign-in
 * view's own SSO button already gates on). Filtered out of the returned
 * array entirely (not merely rendered differently) when false, matching
 * the mockup's own fixed three-row baseline (github/slack/linear) and
 * this view's established "no affordance when the capability is
 * unconfigured" convention -- even for a caller whose identities DOES
 * happen to carry a connected oidc row (e.g. OIDC was disabled for this
 * deployment after they linked one): the row still disappears, since
 * showing a chip for a capability this deployment no longer offers would
 * be its own kind of dishonest affordance.
 *
 * # Why no "pending" state, unlike the mockup's own third row
 *
 * The mockup shows "linear · pending · link sent to your Linear inbox"
 * as a live example. internal/domain's own identity_link_prompts schema
 * (§13.2) deliberately carries NO user_id: an ambiguous/unmatched
 * provider identity has no known target user until someone actually
 * clicks its magic link (§13.2's own "never guess" design) -- so there is
 * no honest way for a SELF view to say "a pending link is waiting for
 * YOU" without fabricating an attribution the schema itself does not
 * carry (see httpapi/me.go's own doc comment for the full reasoning).
 * This therefore only ever reports two states per provider: connected
 * (an Identity row exists) or not connected (it doesn't).
 */
export function deriveIdentityStatuses(identities: readonly Identity[], oidcConfigured: boolean): ProviderStatus[] {
  return DISPLAYED_PROVIDERS.filter((provider) => provider !== 'oidc' || oidcConfigured).map((provider) => {
    const match = identities.find((identity) => identity.provider === provider)
    return match ? { provider, connected: true, linkedVia: match.linkedVia } : { provider, connected: false }
  })
}

/** linkedViaCaption renders linkedVia as the mockup's own "matched by verified email"-style trailing caption -- honest about what each enum value actually means (identities.linkedVia, §13.2), never a literal echo of the enum string itself. */
export function linkedViaCaption(linkedVia: Identity['linkedVia']): string {
  switch (linkedVia) {
    case 'auto_email':
      return 'matched by verified email'
    case 'prompt':
      return 'confirmed via magic link'
    case 'admin':
      // Deliberately NOT "linked by an admin": callback.go's own
      // createUserAndIdentity uses this same enum value for the
      // identity created at ordinary GitHub sign-in time too (a
      // documented overload, that function's own doc comment) -- "an
      // admin did this" would be false in the common case.
      return 'connected'
  }
}

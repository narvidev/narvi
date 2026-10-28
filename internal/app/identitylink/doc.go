// Package identitylink is the I/O-performing orchestrator for §13.2's
// auto-link algorithm and its magic-link counterpart -- the app-layer
// half of §13.2's ("identities + full RBAC") own auto-linking brief,
// paired with the pure decision logic in internal/domain/identitylink
// (see that package's own doc.go for the exact domain/app split, mirroring
// internal/domain/turn vs internal/app/sessionactor).
//
// # Resolve -- §13.2's own numbered algorithm, steps 1-4
//
// Called by internal/adapters/inbound/{slack,linear} at the point where an
// inbound event names a provider identity (a Slack user id, a Linear
// creatorId) this codebase has never seen before:
//
//  1. Fetch the actor's profile email from the provider API -- this is
//     the CALLER's own job (internal/adapters/outbound/{slackapi,linearapi}.
//     GetUserEmail, wrapped in platform.Retry by the caller), NOT this
//     package's -- Resolve itself takes the already-fetched (email, ok)
//     pair, since retrying is provider-specific (a different client, a
//     different auth token shape) while everything AFTER a successful
//     fetch is identical regardless of which provider asked. ok=false
//     means every retry already failed; Resolve does nothing further at
//     all in that case (no guess, no prompt -- §13.2's own failure rule:
//     "never null-out an email on transient failure" -- this simply skips
//     the whole attempt rather than writing anything). The pair is a
//     ProfileEmail: the address, and whether the provider vouches for it
//     under §13.2's definition of a verified email for that provider (for
//     Slack, the email is confirmed and the member is a full member of
//     this workspace -- slackapi.UserEmail.Verified). An email the provider
//     does not vouch for is never matched: it goes straight to the link
//     prompt of step 4.
//  2. Match against identity emails a provider confirmed recently
//     (MatchUserIDs, postgres.IdentityStore.ListVerifiedUserIDsByEmail):
//     verified, with email_checked_at within Deps.EvidenceMaxAge
//     (platform.Timeouts.IdentityEmailEvidenceMaxAge) of the database
//     clock, deduplicated by user id. users.primary_email is not evidence
//     on its own, and neither is a row with no email_checked_at -- a Slack
//     automatic link nothing has confirmed since migrations/000178, or a
//     sign-in row from before sign-ins stamped theirs.
//  3. internal/domain/identitylink.Decide renders the pure verdict.
//  4. Exactly one match: INSERT the identities row (linked_via=auto_email,
//     email_verified and email_checked_at as the provider attested)
//     and an audit_log row, in ONE transaction, then delete any
//     still-pending link prompt for this same identity (a later,
//     resolved auto-link supersedes an earlier "we couldn't tell"
//     prompt). Zero or multiple matches: reuse a still-unexpired
//     identity_link_prompts row if one already exists for this
//     (provider, external_id) (never re-mint/re-send a magic link on
//     every single inbound message -- see this package's own service.go
//     for the concrete policy), otherwise mint a fresh nonce
//     (platform.GenerateToken, hashed at rest exactly like user_sessions/
//     ws_tokens already do) and persist it.
//
// An identity that is already linked resolves at once -- except a Slack
// automatic link made before §13.2's Slack definition existed (no
// email_checked_at), which resolves to no one until the member's next event
// re-checks it on the email then fetched: kept and stamped when Slack
// confirms the same email for a full member of this workspace, otherwise
// removed with an identity.auto_link_revoked audit entry, and the identity
// goes through steps 2-4 again. A link made through the link prompt or by an
// admin is never re-checked (service.go's autoLinkUnchecked and
// recheckAutoLink).
//
// Either way, Resolve NEVER blocks the calling ingress flow on any of
// this (§13.2's own explicit "the action proceeds with bot attribution
// until linked") -- its own Resolution.UserID stays invalid (Valid ==
// false) for every branch except the exactly-one-match case, so the
// caller's own turn/session/plan-decision write proceeds with bot
// attribution exactly as it always did, EXCEPT now carrying the real
// user id the moment an auto-link (or an earlier one, already on file)
// resolves it.
//
// In-channel/in-app notification (§13.2 step 3's own "notify the user
// in-channel and in-app") is split across two owners, deliberately: the
// IN-CHANNEL half (posting a Slack/Linear message) is the CALLER's job --
// Resolve returns a Resolution the caller inspects (AutoLinked /
// MagicLinkURL) and acts on with its own channel-specific client, mirroring
// how internal/app/identitylink never imports slackapi/linearapi itself
// (this package stays provider-agnostic, exactly like domain/identitylink
// does). The IN-APP half is realized via the audit_log row itself,
// surfaced through the members API's own audit-log read endpoint
// (internal/adapters/inbound/httpapi/members.go) -- there is no separate
// notifications/inbox subsystem in this codebase yet (confirmed: no
// migration, no domain package; Phase 7 is the first UI Step at all), so
// building one from scratch is explicitly out of THIS Step's scope; the
// audit trail is the one "in-app, durable, queryable" surface that
// already exists and already needs an auto-link entry regardless.
//
// # Sign-in evidence -- RefreshSignInEvidence
//
// Evidence ages: an email counts only while its provider confirmed it
// within Deps.EvidenceMaxAge, because a previous owner of an address who
// never signs in again never presents anything that could retire it. So
// every presentation re-stamps it:
//
//   - Every returning GitHub or OIDC sign-in (internal/adapters/inbound/
//     auth) calls RefreshSignInEvidence with the verified email its
//     provider just presented: one transaction, on the identity row locked
//     FOR UPDATE, writes that email as verified with email_checked_at
//     now, audits a changed email (identity.email_updated), and never
//     touches user_id or linked_via, whoever made the link. A sign-in
//     creating its user stamps the identity when it inserts it, and a
//     first sign-in that merges onto an existing user goes through
//     AutoLink, which stamps too.
//   - The user's primary email follows the sign-in identity it came from,
//     in a savepoint of the same transaction, unless another member holds
//     the new address (user.primary_email_changed). It is what Members,
//     /api/me and MCP consent display, never evidence.
//   - Slack and Linear fetch no more than before: AutoLink stamps a new
//     link, and Resolve's re-check stamps a confirmed one.
//
// Age never removes a link and never stops it resolving: Resolve's
// re-check of an unchecked Slack automatic link stays the only thing that
// removes one. Aged evidence simply stops matching; the identity then gets
// the link prompt (Slack, Linear), or a first sign-in creates a new user
// behind the allowlist -- or is refused when its email is a member's
// primary email (internal/adapters/inbound/auth's resolveFirstTimeIdentity).
//
// # Consume -- the magic-link counterpart (§13.2 step 4's own "connect
// your account" link)
//
// Called by internal/adapters/inbound/identitylink's own magic-link
// consume HTTP handler, once the clicking visitor is authenticated (a
// valid narvi_auth_session cookie -- that package's own handler redirects
// through the existing GitHub OAuth login first if not, see its own doc
// comment): validates the presented nonce (exists, not expired), inserts
// the identities row (linked_via=prompt, user_id = the now-authenticated
// user), records the audit-log entry (THIS time with a real, human
// actor_user_id -- the person who clicked their own confirmed link), and
// deletes every pending prompt for that same (provider, external_id) so a
// stale link can never be replayed.
package identitylink

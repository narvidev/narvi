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
//  2. Match against users.primary_email (postgres.UserStore.
//     GetByPrimaryEmail) and verified identities.email (postgres.
//     IdentityStore.ListVerifiedUserIDsByEmail), deduplicated by user id.
//     A Slack automatic link nothing has confirmed since migrations/000176
//     is not evidence here.
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

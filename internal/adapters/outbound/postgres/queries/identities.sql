-- Queries backing IdentityStore (§13.2 identity graph,
-- migrations/000003_identities.up.sql + migrations/000017_auth_v1.up.sql's
-- own access_token_encrypted column). CreateIdentity links a fresh user to
-- a provider identity at first-sign-in time.
-- GetIdentityByProviderAndExternalID is the OAuth callback's own
-- returning-vs-first-time-sign-in check. UpdateIdentityAccessToken
-- re-encrypts and stores a fresh provider token on every successful
-- sign-in of an ALREADY-linked identity (GitHub's own classic OAuth tokens
-- don't expire, but re-storing on each login is harmless and keeps the
-- stored token fresh if the user ever re-authorized with different
-- scopes).

-- name: CreateIdentity :one
-- email_checked_at (migrations/000177) is set only by an automatic link on
-- an email its caller reports verified (internal/app/identitylink.AutoLink);
-- every other caller leaves it NULL.
INSERT INTO identities (user_id, provider, external_id, email, email_verified, linked_via, access_token_encrypted, email_checked_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetIdentityByProviderAndExternalID :one
SELECT * FROM identities
WHERE provider = $1 AND external_id = $2;

-- name: GetIdentityByUserAndProvider :one
-- §9.3 ("e2e happy path")'s own scm-credentials endpoint uses this to
-- find a session's created_by user's GitHub identity (to decrypt its
-- access_token_encrypted) -- the OAuth callback's own lookup above goes
-- the other direction (provider+external_id -> user).
SELECT * FROM identities
WHERE user_id = $1 AND provider = $2;

-- name: UpdateIdentityAccessToken :one
UPDATE identities
SET access_token_encrypted = $2
WHERE id = $1
RETURNING *;

-- §13.2 ("identities + full RBAC", §13.2/§13.3) additions --
-- ListVerifiedIdentityUserIDsByEmail is the auto-link algorithm's own
-- "match against ... verified identity emails" half (the OTHER half,
-- users.primary_email, is GetUserByPrimaryEmail in users.sql);
-- ListIdentitiesForUser/DeleteIdentity back the members API's own
-- "linked identities incl. pending-link state" listing and admin
-- manual-unlink endpoint (§13.2 point 5, "admin can force-link").

-- name: ListVerifiedIdentityUserIDsByEmail :many
-- DISTINCT: the SAME user could plausibly have two verified identities
-- (e.g. Slack AND Linear) sharing one email -- this must still count as
-- ONE match for §13.2's own "exactly one verified match" test, never two,
-- so a caller combining this with GetUserByPrimaryEmail's own single
-- result must dedupe by user_id regardless; DISTINCT here just avoids
-- handing back an already-known duplicate in the common case.
--
-- A Slack automatic link nothing has checked since migrations/000177 (the
-- previous binary took any Slack profile email as verified) is left out
-- until identitylink.Resolve re-checks it against Slack -- the same rows
-- identitylink's autoLinkUnchecked names, and DeleteUncheckedAutoLinkedIdentity
-- deletes.
SELECT DISTINCT user_id FROM identities
WHERE email_verified = true AND lower(email) = lower($1)
  AND NOT (provider = 'slack' AND linked_via = 'auto_email' AND email_checked_at IS NULL);

-- name: ListIdentitiesForUser :many
SELECT * FROM identities
WHERE user_id = $1
ORDER BY created_at ASC;

-- name: DeleteIdentity :execrows
-- Backs the admin manual-unlink endpoint. Scoped to id, but the caller
-- (httpapi/members.go) always re-checks the row's own UserID against the
-- path's userID first -- this query alone would just as happily delete
-- ANY user's identity by id, so it is never safe to call directly from
-- an id a caller hasn't already verified belongs to the expected user.
DELETE FROM identities
WHERE id = $1;

-- name: ConfirmAutoLinkedIdentityEmail :execrows
-- The re-check of an automatic link found that the provider vouches for its
-- email under §13.2's definition (internal/app/identitylink.Resolve): the
-- email is verified, as of now. Scoped to linked_via auto_email, so it never
-- touches a link a person or an admin made. 0 rows means the row is gone or
-- is no longer an automatic link: the caller looks it up again.
UPDATE identities
SET email_verified = true, email_checked_at = now()
WHERE id = $1 AND linked_via = 'auto_email';

-- name: DeleteUncheckedAutoLinkedIdentity :execrows
-- The re-check of an automatic link found that the provider does not vouch
-- for its email (internal/app/identitylink.Resolve): the link goes. Deletes
-- only a row that is still an automatic link nothing has confirmed -- one
-- whose email is not verified, or a Slack one with no email_checked_at --
-- so it never removes a link a person or an admin made (linked_via prompt or
-- admin), nor one a concurrent re-check has just confirmed. 0 rows means
-- there was nothing of that kind left to delete.
DELETE FROM identities
WHERE id = $1 AND linked_via = 'auto_email'
  AND (NOT email_verified OR (provider = 'slack' AND email_checked_at IS NULL));

-- name: PromoteUncheckedAutoLinkToAdmin :one
-- An admin force-links (provider, external_id) to the user an automatic
-- link nothing has confirmed already names (internal/adapters/inbound/
-- httpapi's LinkMemberIdentity): the link becomes the admin's, so the
-- re-check never reads or removes it again. email_verified goes false: the
-- admin vouches for the link, not for an email the provider never
-- confirmed, so it is no evidence for matching another identity. Matches
-- only the rows DeleteUncheckedAutoLinkedIdentity would; pgx.ErrNoRows
-- when the row is anything else.
UPDATE identities
SET linked_via = 'admin', email_verified = false
WHERE id = $1 AND linked_via = 'auto_email'
  AND (NOT email_verified OR (provider = 'slack' AND email_checked_at IS NULL))
RETURNING *;

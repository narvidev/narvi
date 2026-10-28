-- Queries backing IdentityStore (§13.2 identity graph,
-- migrations/000003_identities.up.sql + migrations/000017_auth_v1.up.sql's
-- own access_token_encrypted column). CreateIdentity links a fresh user to
-- a provider identity at first-sign-in time.
-- GetIdentityByProviderAndExternalID is the OAuth callback's own
-- returning-vs-first-time-sign-in check. RefreshSignInIdentityEmail records,
-- on every successful sign-in of an ALREADY-linked identity, the email its
-- provider just presented and when, and stores a fresh provider token when
-- the sign-in has one (GitHub's own classic OAuth tokens don't expire, but
-- re-storing on each login is harmless and keeps the stored token fresh if
-- the user ever re-authorized with different scopes).

-- name: CreateIdentity :one
-- email_checked_at (migrations/000178) is set only when the email was just
-- presented as verified by its provider: an automatic link on an email its
-- caller reports verified (internal/app/identitylink.AutoLink), and a
-- GitHub or OIDC sign-in creating its user (internal/adapters/inbound/
-- auth). Every other caller leaves it NULL, and such a row is no evidence
-- for ListVerifiedIdentityUserIDsByEmail.
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

-- name: GetIdentityByIDForUpdate :one
-- Locks one identities row for the rest of the caller's transaction:
-- internal/app/identitylink.RefreshSignInEvidence reads the email it is
-- about to replace, so two concurrent sign-ins of the same identity record
-- their changes one after the other, each against the email the other left.
SELECT * FROM identities
WHERE id = $1
FOR UPDATE;

-- name: RefreshSignInIdentityEmail :one
-- A GitHub or OIDC sign-in of this identity has just presented a verified
-- email (internal/app/identitylink.RefreshSignInEvidence): store it, as
-- verified, as of now. access_token_encrypted is replaced only when the
-- sign-in brings a token (GitHub); NULL keeps the stored one (OIDC stores
-- none). Never touches user_id or linked_via: a sign-in records what the
-- provider presents, it never changes the link.
UPDATE identities
SET email = sqlc.arg(email)::text,
    email_verified = true,
    email_checked_at = now(),
    access_token_encrypted = COALESCE(sqlc.narg(access_token_encrypted), access_token_encrypted)
WHERE id = sqlc.arg(id)
RETURNING *;

-- §13.2 ("identities + full RBAC", §13.2/§13.3) additions --
-- ListVerifiedIdentityUserIDsByEmail is the auto-link algorithm's whole
-- "match against identity emails confirmed recently" step (users.
-- primary_email is not evidence on its own); ListIdentitiesForUser/
-- DeleteIdentity back the members API's own "linked identities incl.
-- pending-link state" listing and admin manual-unlink endpoint (§13.2
-- point 5, "admin can force-link").

-- name: ListVerifiedIdentityUserIDsByEmail :many
-- The one rule for what counts as evidence (§13.2 step 2): a verified
-- email its provider confirmed within max_age (platform.Timeouts.
-- IdentityEmailEvidenceMaxAge), against the database clock. A row with no
-- email_checked_at never counts -- that includes a Slack automatic link
-- nothing has checked since migrations/000178 (identitylink's
-- autoLinkUnchecked, until Resolve re-checks and stamps it), and every
-- sign-in row created before sign-ins stamped theirs, until its next
-- sign-in. Case-insensitive, as GetUserByPrimaryEmail is.
--
-- DISTINCT: the SAME user could plausibly have two verified identities
-- (e.g. Slack AND Linear) sharing one email -- this must still count as
-- ONE match for §13.2's own "exactly one verified match" test, never two.
SELECT DISTINCT user_id FROM identities
WHERE email_verified = true
  AND lower(email) = lower(sqlc.arg(email))
  AND email_checked_at > now() - sqlc.arg(max_age)::interval;

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

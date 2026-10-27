-- Queries backing UserStore (§13.2 identity graph anchor, §13.3 RBAC role,
-- migrations/000002_users.up.sql). CreateUser inserts a user row once, at
-- first-sign-in creation time (internal/adapters/inbound/auth's own OAuth
-- callback handler) -- role is set once, at creation, per that handler's
-- own initial-admin logic. GetUserByID backs both the callback handler and
-- the auth middleware's own per-request lookup.
--
-- §13.2 ("identities + full RBAC", §13.2/§13.3) additions --
-- ListUsersOrderedByCreatedAt/UpdateUserRole back the members API
-- (internal/adapters/inbound/httpapi/members.go); GetUserByPrimaryEmail
-- backs a first sign-in's check that no member already holds its email
-- (internal/adapters/inbound/auth). UpdateUserRole and
-- FollowUserPrimaryEmail are the ONLY writes to an existing user's own row
-- this codebase makes past creation time.

-- name: CreateUser :one
INSERT INTO users (primary_email, display_name, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1;

-- name: GetUserByPrimaryEmail :one
-- Case-insensitive: email addresses are conventionally
-- case-insensitive (at least in the local-part-plus-domain sense every
-- real mail provider actually applies), and this is ONLY ever compared
-- against an email a provider (GitHub/OIDC) already independently attests
-- to, never user-supplied free text -- matching lower(email) on both sides
-- is the same convention ListVerifiedIdentityUserIDsByEmail
-- (identities.sql) uses for its own verified-identity-email match. Not
-- evidence for linking (§13.2 step 2): a first sign-in uses it only to
-- refuse an email some member already holds.
SELECT * FROM users
WHERE lower(primary_email) = lower($1);

-- name: ListUsersOrderedByCreatedAt :many
-- Backs GET /api/members -- every user, oldest-first (matches
-- ListSummariesForSession's own "stable, predictable order" precedent
-- elsewhere in this codebase; oldest-first reads naturally as "founding
-- members first").
SELECT * FROM users
ORDER BY created_at ASC;

-- name: UpdateUserRole :one
-- Backs the admin-only role-change endpoint (§13.3's own "members & roles:
-- admin only" row). With FollowUserPrimaryEmail below, the only columns of
-- an existing user's row this codebase mutates past creation time.
UPDATE users
SET role = $2
WHERE id = $1
RETURNING *;

-- name: FollowUserPrimaryEmail :execrows
-- A user's primary email follows the sign-in identity it came from (§13.2,
-- internal/app/identitylink.RefreshSignInEvidence): when that identity's
-- provider presents a new email, primary_email moves to it -- only while
-- primary_email still is the identity's previous email (so a user with two
-- sign-in identities does not flip between them), only when the address
-- really changes, and only when no OTHER user holds the new one, ignoring
-- case. 0 rows means it did not move. users.primary_email's own UNIQUE is
-- case-sensitive, so a concurrent exact-case insert can still raise 23505,
-- which the caller absorbs.
UPDATE users AS u
SET primary_email = sqlc.arg(new_email)::text
WHERE u.id = sqlc.arg(id)
  AND lower(u.primary_email) = lower(sqlc.arg(previous_email)::text)
  AND lower(u.primary_email) <> lower(sqlc.arg(new_email)::text)
  AND NOT EXISTS (
    SELECT 1 FROM users AS o
    WHERE o.id <> u.id AND lower(o.primary_email) = lower(sqlc.arg(new_email)::text)
  );

-- An audit finding (H8) confirmed UpdateMemberRole had no protection
-- against demoting the last remaining admin: doing so permanently locks
-- the deployment out of every admin-only endpoint, including role
-- management itself, with no recovery path short of direct DB surgery.
-- ListActiveAdminIDsForUpdate is that guard's own query -- FOR UPDATE
-- (not a plain count) so it locks every currently-qualifying row: a
-- SECOND concurrent demotion of a DIFFERENT admin, running this SAME
-- query inside its own transaction, blocks here until the first
-- transaction commits or rolls back, then re-evaluates against the
-- now-current row versions (Postgres' own read-committed row-locking
-- semantics) rather than both transactions reading a stale "2 admins"
-- snapshot and both proceeding.

-- name: ListActiveAdminIDsForUpdate :many
SELECT id FROM users
WHERE role = 'admin' AND disabled = false
FOR UPDATE;

-- Queries backing SessionStore (§4.3). Just enough to prove the pipeline
-- end to end (create + get) — full CRUD lands with the PRs that build out
-- session-actor persistence (PR-11+).

-- name: CreateSession :one
-- repos defaults to '[]'::jsonb via COALESCE (not the column's own DEFAULT
-- clause) specifically so every EXISTING call site that never set Repos
-- (every session row created before this column existed) keeps compiling
-- and behaving identically: a nil/absent []byte param binds SQL NULL, and
-- COALESCE(NULL, '[]'::jsonb) resolves to the same empty-list default a
-- bare column-default insert would have produced.
--
-- environment_id/provenance_tag (row 10, "domain: Environment scoping",
-- §14.1) are both sqlc.narg -- nullable, optional params -- so every
-- EXISTING call site that never sets them (every session created before
-- this batch) keeps compiling and behaving identically: both stay NULL,
-- byte-for-byte today's unscoped behavior.
--
-- build_model_id ("plan mode, web", §12.2 item 3) is likewise
-- sqlc.narg -- every EXISTING call site that never sets it keeps
-- compiling and behaving identically (NULL, "use the default model
-- catalog entry", migrations/000034_plan_mode.up.sql's own convention).
--
-- build_effort (migrations/000063_turn_session_effort.up.sql,
-- §29.8) mirrors build_model_id's own shape exactly, one column over --
-- same sqlc.narg treatment, same "every existing call site keeps
-- compiling and behaving identically (NULL, use the default)" guarantee.
--
-- parent_session_id/spawn_depth ("sentinels + suggestions",
-- §17.2, migrations/000045) are likewise sqlc.narg/COALESCE-defaulted --
-- every EXISTING call site (every session created before this Step) keeps
-- compiling and behaving identically: parent_session_id stays NULL,
-- spawn_depth stays 0. httpapi.CreateSessionOnTx's own ChildSessionOptions
-- (create.go) is this Step's own mechanism that supplies non-default
-- values -- either via httpapi.SpawnChildSession (childsession.go, a
-- caller with no already-open transaction of its own) or, since the
-- Finding-1 audit fix, internal/app/outboxworker's own sentinelautofix.go,
-- which calls CreateSessionOnTx directly, inline on its own
-- claim-locked transaction (see that file's own doc comment for why it
-- cannot go through SpawnChildSession's separate transaction instead).
--
-- epistemic_check_enabled ("builder epistemic pre-action check",
-- §20.4, migrations/000066) mirrors build_model_id's own sqlc.narg
-- treatment exactly: every EXISTING call site that never sets it keeps
-- compiling and behaving identically (NULL, "use platform.Config's own
-- global default" -- off, unless an operator has turned the default on).
INSERT INTO sessions (title, spawn_source, created_by, repos, environment_id, provenance_tag, build_model_id, build_effort, parent_session_id, spawn_depth, epistemic_check_enabled)
VALUES ($1, $2, $3, COALESCE(sqlc.narg('repos'), '[]'::jsonb), sqlc.narg('environment_id'), sqlc.narg('provenance_tag'), sqlc.narg('build_model_id'), sqlc.narg('build_effort'), sqlc.narg('parent_session_id'), COALESCE(sqlc.narg('spawn_depth'), 0), sqlc.narg('epistemic_check_enabled'))
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions
WHERE id = $1;

-- name: BumpActorEpoch :one
-- Increments a session's actor_epoch and returns the new value. Called
-- once, at acquisition time, when an actor takes ownership of a session
-- (§2: "bumped on each acquisition") -- never as part of an ordinary
-- state-transition write (those only READ the epoch, via
-- GetSessionActorEpochForUpdate below, to check it hasn't moved).
UPDATE sessions SET actor_epoch = actor_epoch + 1
WHERE id = $1
RETURNING actor_epoch;

-- name: GetSessionActorEpochForUpdate :one
-- Locks and reads a session's current actor_epoch. Called at the START of
-- every transactional write, inside that same transaction, to fence a
-- stale writer (an actor whose epoch no longer matches -- proof a newer
-- actor has since taken over) before it does anything else (§2: "writes
-- with a stale epoch fail").
SELECT actor_epoch FROM sessions
WHERE id = $1
FOR UPDATE;

-- name: UpdateSessionStatus :one
-- Persists a session's derived status + failure_reason --
-- internal/domain/session.DeriveStatus's output, never written directly
-- (§11: "every state transition goes through the machine's transition
-- table").
UPDATE sessions
SET status = $2, failure_reason = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateSessionConversationID :one
-- Persists the session-level OpenCode conversation id (§3.3: "recorded at
-- turn start ... also reported on every heartbeat"; see
-- migrations/000018_session_repos.up.sql's own doc comment for why this is
-- session-scoped, not turns.conversation_id). Called by
-- internal/app/sessionactor's handleSandboxEvent whenever a "heartbeat"
-- event carries a non-nil ConversationId.
UPDATE sessions
SET opencode_conversation_id = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateSessionIntentDecisionIfNull :execrows
-- §8.3's ("intent classifier", §18.4) write-once guarded update:
-- "UPDATE sessions SET intent_decision = ... WHERE intent_decision IS
-- NULL" -- NOT read-then-write, first decision wins, no application-level
-- lock needed. RowsAffected (via :execrows) is the caller's own win/lose
-- signal: 1 means THIS call actually set intent_decision (first writer
-- wins); 0 means some other writer already set it first -- internal/app/
-- intentclassifier's persistence path treats either outcome as success,
-- never an error, since "someone already recorded a decision for this
-- session" is exactly the expected, race-safe steady state, not a
-- failure.
UPDATE sessions
SET intent_decision = $2
WHERE id = $1 AND intent_decision IS NULL;

-- name: ListFailedSessions :many
-- §16 ("decision inbox: read model + API", §16.1)'s own
-- needs_attention row source: every session currently 'failed' -- §3.2's
-- own resume/recreate lanes make every failed session resume-eligible in
-- SOME form (recreate-from-scratch at minimum, via conversation replay --
-- see internal/app/decisioninbox's own doc comment for why this Step does
-- not further narrow "resume available" beyond the status itself, since
-- no additional per-provider capability signal is available at this read-
-- model layer). Most-recently-failed first (updated_at -- set to now() by
-- UpdateSessionStatus at the exact moment a session's own status last
-- changed, which for an unarchived, still-'failed' row is the instant it
-- became failed), bounded by $1 (§21.1's own "bounded from day one"
-- discipline -- never an unbounded scan).
--
-- ADMIN-ONLY at the RBAC/httpapi layer (§16.1's own parenthetical) -- this
-- query itself carries no per-user filter: an admin's own ops-triage view
-- is system-wide, not narrowed to sessions they personally created.
SELECT * FROM sessions
WHERE status = 'failed' AND NOT archived
ORDER BY updated_at DESC
LIMIT $1;

-- name: ListSessions :many
-- Backs GET /api/sessions (§6.3/§12.2 item 1's own sidebar addition --
-- no general session-list route existed before this;
-- ListFailedSessions immediately above is a narrower, admin-only,
-- single-status read model, not this). mine_only implements §12.2 item
-- 1's own "'My sessions' = created or joined" definition exactly:
-- created_by = user_id OR a participants row exists for (session, user) --
-- false returns every unarchived session system-wide, mirroring
-- ListFailedSessions' own "no per-user filter, gated at the httpapi layer
-- instead" precedent, since this codebase has no per-session RBAC/
-- visibility concept today (httpapi/doc.go's own "every route ... 401
-- before reaching any handler, nothing narrower"). sb.status is LEFT
-- JOINed, never INNER -- a session with no sandbox row yet (status=
-- 'created', never dispatched, or long since torn down) reads back
-- NULL/Invalid here, never a fabricated default state; sandboxes.
-- UNIQUE(session_id) (migrations/000006_sandboxes.up.sql) guarantees this
-- join can add at most one row per session, never fan it out. Most-
-- recently-updated first, id DESC as a tiebreaker for rows sharing the
-- same updated_at (e.g. a batch of sessions created in the same
-- transaction), bounded by $2 -- no cursor pagination in this first cut
-- (see ListSessionsResponse's own schema doc comment for why).
SELECT sqlc.embed(s), sb.status AS sandbox_status FROM sessions s
LEFT JOIN sandboxes sb ON sb.session_id = s.id
WHERE NOT s.archived
  AND (
    NOT sqlc.arg('mine_only')::boolean
    OR s.created_by = sqlc.arg('user_id')
    OR EXISTS (SELECT 1 FROM participants p WHERE p.session_id = s.id AND p.user_id = sqlc.arg('user_id'))
  )
ORDER BY s.updated_at DESC, s.id DESC
LIMIT sqlc.arg('row_limit');

-- name: ListSessionOutcomeCountsInWindow :many
-- §12.2 item 6's own platform-wide analytics rollup: the ONE
-- Postgres read behind FOUR distinct reductions internal/domain/
-- platformanalytics performs over its rows (mirrors internal/domain/
-- reviewverdict.ListRecordsSince's own "one fetch, several pure
-- reductions" precedent) -- the "Sessions" KPI tile (sum of every
-- session_count row), "success rate" (completed vs completed+failed),
-- "sessions per day by outcome" chart (grouped by day+status), and "top
-- failure reasons" chart (grouped by failure_reason, for status IN
-- (failed, cancelled)).
--
-- A GROUP BY reduction, deliberately NOT a bounded raw-row fetch like
-- ListRecentlyDecided/ListRecordsSince use for their own, inherently
-- narrower (per-repo or per-window-of-decisions) scopes: this rollup is
-- explicitly platform-wide (every repo, every session), so its own true
-- row count has no natural per-entity bound the way a repo-scoped fetch
-- does, and an arbitrary LIMIT here would silently UNDERCOUNT the
-- "Sessions" tile the moment a real deployment's window exceeds it --
-- the exact kind of quietly-wrong number this Step exists to replace, not
-- reintroduce. Aggregating in Postgres instead keeps the RESULT set
-- small (at most a handful of days x 5 statuses x 5 failure reasons)
-- regardless of how many session rows the window actually contains.
--
-- day is truncated to UTC calendar day (date_trunc(...) AT TIME ZONE
-- 'UTC', converting back to a real timestamptz at UTC midnight) --
-- mirrors internal/domain/reviewverdict's own truncateToUTCDay, done here
-- in SQL rather than Go specifically because the day dimension IS the
-- GROUP BY key (unlike reviewverdict's own Go-side truncation over an
-- already-fetched, small, repo-scoped row set).
--
-- Bounded by sessions_created_at_idx (migrations/
-- 000125_platform_analytics_indexes.up.sql) -- $1 is the window's own
-- start (platform.Timeouts.PlatformAnalyticsWindow), never an unbounded
-- scan.
SELECT
    (date_trunc('day', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')::timestamptz AS day,
    status,
    failure_reason,
    COUNT(*) AS session_count
FROM sessions
WHERE created_at >= $1
GROUP BY (date_trunc('day', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')::timestamptz, status, failure_reason
ORDER BY day;

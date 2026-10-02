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
--
-- create_idempotency_key/create_request_sha256 (§43.8, migrations/000150)
-- are sqlc.narg too: NULL for every caller that sends no key, which is every
-- caller but POST /api/sessions with an idempotencyKey. They are written
-- together or not at all (the table's own CHECK), and a second insert by
-- the same creator with the same key fails with 23505 on
-- sessions_create_idempotency_key_uniq.
INSERT INTO sessions (title, spawn_source, created_by, repos, environment_id, provenance_tag, build_model_id, build_effort, parent_session_id, spawn_depth, epistemic_check_enabled, create_idempotency_key, create_request_sha256)
VALUES ($1, $2, $3, COALESCE(sqlc.narg('repos'), '[]'::jsonb), sqlc.narg('environment_id'), sqlc.narg('provenance_tag'), sqlc.narg('build_model_id'), sqlc.narg('build_effort'), sqlc.narg('parent_session_id'), COALESCE(sqlc.narg('spawn_depth'), 0), sqlc.narg('epistemic_check_enabled'), sqlc.narg('create_idempotency_key'), sqlc.narg('create_request_sha256'))
RETURNING *;

-- name: GetSessionByCreateIdempotencyKey :one
-- The session one user created with one idempotency key (§43.8,
-- migrations/000150), for POST /api/sessions' replay check: at most one
-- row, since (created_by, create_idempotency_key) is unique where the key is
-- set. Read with no lock: a concurrent create with the same key is settled
-- by the unique index, not by this read.
SELECT * FROM sessions
WHERE created_by = $1 AND create_idempotency_key = $2;

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

-- name: GetSessionActivityFacts :one
-- Every fact GET /api/sessions/{sessionID}/status derives a session's
-- activity from (technical plan §43.20), in ONE statement and so one MVCC
-- snapshot: two statements under READ COMMITTED could interleave with the
-- transaction that completes a plan-mode turn and inserts its plan
-- (sessionactor's recordPlanIfNeeded) and see "turn completed, no plan
-- yet" -- a false "finished". Deliberately NOT sessions.status: that
-- column is re-derived only when a turn reaches a terminal state, so a
-- queued or running turn can sit under any of its five values; it is
-- selected here only to decide whether sessions.failure_reason still
-- describes the last run (the handler's own rule), never the activity.
--
-- turn_counts is the per-state turn histogram as a JSON object (state ->
-- count), not one column per known state: a state added to turn_status
-- later still reaches internal/domain/session.DeriveActivity, which counts
-- an unknown state as work in flight, instead of being silently dropped.
-- Aggregates and single-row lookups only -- never the turns themselves
-- (turns per session are unbounded, and each row carries its prompt).
-- Every lookup leads with session_id on an existing index (turns_session_
-- id_dispatched_message_id_idx, plans_one_awaiting_approval_per_session,
-- workflow_runs_session_id_idx, workflow_step_runs_one_live_per_run,
-- session_timers' UNIQUE (session_id, name), github_pr_sessions_session_
-- id_idx, release_manifest_pending_session_id_idx,
-- release_manifest_checks_running_session_id_idx); the escalation's
-- follow-up checks go by primary key, by session_id over turns, and by
-- workflow_run_id (workflow_step_runs_run_step_idx), and repo_settings and
-- repo_entitlement_revocations by their primary keys.
--
-- escalated is the session's LIVE workflow escalation, never merely a run
-- in needs_review: nothing moves a run out of needs_review, and the next
-- turn starts a fresh run beside the parked one (migrations/000057), so
-- counting every such run would gate the session for good. A needs_review
-- run is reported only while no turn has been CREATED on the session since
-- it escalated, and its definition is not a built-in one, whose escalation
-- no person or route can act on (technical plan §43.20 gives the reasons).
-- That is: it is the session's newest workflow run (a turn that started a
-- run of its own supersedes it), it ran at least one attempt of its own (a
-- run no turn ever ran is no state the session reached), and no turn other
-- than that run's own attempts (workflow_step_runs.turn_id -- among them
-- the turn whose end escalated it, in the same transaction) was created at
-- or after the instant the run escalated (workflow_runs.updated_at, which
-- EscalateWorkflowRun stamps; the notice claim that also writes it runs in
-- the escalating transaction itself, workflowengine's escalateRun, and a
-- needs_review run is never written again unless it escalates again). New
-- work sent to the session after the escalation answers it; a turn created
-- before it never does, whether it is still queued, running or has ended --
-- one queued behind the turn whose end escalated the run, or one sent while
-- a step awaited the decision that escalated it -- so whether such a turn
-- ended a moment before or after the escalation never changes the answer.
-- Both instants are the database's own now(), each its transaction's
-- start (turns.created_at, workflow_runs.updated_at); two transactions that
-- overlap are ordered by which began first.
--
-- armed_timer_names/armed_timer_fires_at are the session's armed named
-- timers (session_timers, one row per name at most -- its UNIQUE
-- (session_id, name) index), two arrays in the same order. Every kind is
-- returned, never only the ones some list here names: the handler
-- classifies each through sessionactor.TimerCountsAsScheduledWork, whose
-- table covers every kind the code declares and counts a name it does not
-- know as work, so a kind added later can never read as settled by being
-- filtered out here. review_retrigger_can_fire is whether the §24
-- re-review debounce's fire can still insert a turn at all, on the
-- conditions it reads from rows alone: the pull request's repository
-- opted in (repo_settings.auto_retrigger_review_enabled; no row means
-- off), its automatic re-review budget is not spent
-- (github_pr_sessions.auto_retrigger_count below
-- review_auto_retrigger_budget, which the caller passes from
-- sessionactor.ReviewAutoRetriggerBudget -- the one definition the fire
-- itself compares with), and an administrator has not revoked it (§31.4:
-- no repo_entitlement_revocations row for the pull request's base
-- repository, github_pr_sessions.repo_full_name, the claim key the fire
-- reads too). Each is necessary for the fire to insert a turn, so a
-- debounce armed while any fails can only decline; the fire's other
-- decline rules -- the head already reviewed, a plan awaiting approval, a
-- live fetch that fails, a revocation of only the session's clone URL (a
-- fork's name, parsed in Go) -- are deliberately not copied here, so the
-- status errs toward scheduled on them, never toward settled.
-- release_check_pending_since/_claimed_at are a release PR's manifest
-- check still to come or still running on this session: the oldest
-- release_manifest_pending row's created_at, and the newest
-- release_manifest_checks_running row's claimed_at (migrations/000146).
-- Both the debounce and the check can create a turn on this session with
-- no new input -- technical plan §43.20's inventory -- and the handler
-- reads them as scheduled work, never settled; each is written in (or
-- before) the transaction that arms it, and removed only after the turn
-- it creates has committed.
--
-- pr_delivery_started_at is the push and pull request a completed turn
-- handed off and that have not finished (migrations/000145): the handler
-- reads it as delivering, never settled, only within
-- platform.Timeouts.MCPStatusDeliveryWindow of observed_at, so a push that
-- never reports back cannot hold the session unsettled for good. It is
-- stamped in the transaction that completes the turn, so no snapshot holds
-- that completed turn without it.
--
-- Turn order is created_at, then id -- ListTurnsForSession's own order,
-- with id breaking a tie; workflow runs are ordered the same way.
-- observed_at is the database's own statement time, the instant the
-- snapshot was taken. The two turn statuses are text, '' when their turn
-- is absent (its id is then NULL): an enum column from an outer-joined
-- subquery would be generated as a non-nullable type that cannot scan
-- NULL.
SELECT
    s.id AS session_id,
    s.status AS session_status,
    s.failure_reason AS session_failure_reason,
    s.archived AS archived,
    sb.status AS sandbox_status,
    sb.pr_delivery_started_at AS pr_delivery_started_at,
    statement_timestamp()::timestamptz AS observed_at,
    COALESCE(tc.turn_counts, '{}'::jsonb)::jsonb AS turn_counts,
    inflight.id AS in_flight_turn_id,
    COALESCE(inflight.status::text, '')::text AS in_flight_turn_status,
    inflight.dispatched_at AS in_flight_dispatched_at,
    lastrun.id AS last_run_turn_id,
    COALESCE(lastrun.status::text, '')::text AS last_run_status,
    lastrun.completed_at AS last_run_completed_at,
    newest.id AS newest_turn_id,
    awaitingplan.id AS awaiting_plan_id,
    awaitingplan.created_at AS awaiting_plan_since,
    awaitingstep.id AS awaiting_step_id,
    awaitingstep.updated_at AS awaiting_step_since,
    escalated.id AS escalated_run_id,
    escalated.updated_at AS escalated_run_since,
    COALESCE(armed.names, '{}'::text[])::text[] AS armed_timer_names,
    COALESCE(armed.fires_at, '{}'::timestamptz[])::timestamptz[] AS armed_timer_fires_at,
    COALESCE(reretrigger.can_fire, false)::boolean AS review_retrigger_can_fire,
    releasepending.pending_since::timestamptz AS release_check_pending_since,
    releaserunning.claimed_at::timestamptz AS release_check_claimed_at
FROM sessions s
LEFT JOIN sandboxes sb ON sb.session_id = s.id
LEFT JOIN LATERAL (
    SELECT jsonb_object_agg(c.status, c.n) AS turn_counts
    FROM (
        SELECT t.status::text AS status, count(*) AS n
        FROM turns t
        WHERE t.session_id = s.id
        GROUP BY t.status
    ) c
) tc ON true
LEFT JOIN LATERAL (
    SELECT t.id, t.status, t.dispatched_at
    FROM turns t
    WHERE t.session_id = s.id AND t.status IN ('dispatched', 'processing')
    ORDER BY t.created_at, t.id
    LIMIT 1
) inflight ON true
LEFT JOIN LATERAL (
    SELECT t.id, t.status, t.completed_at
    FROM turns t
    WHERE t.session_id = s.id AND t.status IN ('completed', 'failed', 'cancelled')
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT 1
) lastrun ON true
LEFT JOIN LATERAL (
    SELECT t.id
    FROM turns t
    WHERE t.session_id = s.id
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT 1
) newest ON true
LEFT JOIN LATERAL (
    SELECT p.id, p.created_at
    FROM plans p
    WHERE p.session_id = s.id AND p.status = 'awaiting_approval'
    ORDER BY p.created_at, p.id
    LIMIT 1
) awaitingplan ON true
LEFT JOIN LATERAL (
    SELECT sr.id, sr.updated_at
    FROM workflow_step_runs sr
    JOIN workflow_runs wr ON wr.id = sr.workflow_run_id
    WHERE wr.session_id = s.id AND sr.status = 'awaiting_decision'
    ORDER BY sr.updated_at, sr.id
    LIMIT 1
) awaitingstep ON true
LEFT JOIN LATERAL (
    SELECT wr.id, wr.updated_at
    FROM (
        SELECT r.id, r.status, r.workflow_definition_id, r.updated_at
        FROM workflow_runs r
        WHERE r.session_id = s.id
        ORDER BY r.created_at DESC, r.id DESC
        LIMIT 1
    ) wr
    JOIN workflow_definitions d ON d.id = wr.workflow_definition_id
    WHERE wr.status = 'needs_review'
      AND NOT d.is_built_in
      AND EXISTS (
          SELECT 1
          FROM workflow_step_runs sr
          WHERE sr.workflow_run_id = wr.id AND sr.turn_id IS NOT NULL
      )
      AND NOT EXISTS (
          SELECT 1
          FROM turns t
          WHERE t.session_id = s.id
            AND t.created_at >= wr.updated_at
            AND NOT EXISTS (
                SELECT 1
                FROM workflow_step_runs sr
                WHERE sr.workflow_run_id = wr.id AND sr.turn_id = t.id
            )
      )
) escalated ON true
LEFT JOIN LATERAL (
    SELECT
        array_agg(st.name ORDER BY st.fires_at, st.name) AS names,
        array_agg(st.fires_at ORDER BY st.fires_at, st.name) AS fires_at
    FROM session_timers st
    WHERE st.session_id = s.id
) armed ON true
LEFT JOIN LATERAL (
    SELECT bool_or(
        COALESCE(rs.auto_retrigger_review_enabled, false)
        AND gps.auto_retrigger_count < sqlc.arg('review_auto_retrigger_budget')::integer
        AND NOT EXISTS (
            SELECT 1 FROM repo_entitlement_revocations rev
            WHERE rev.repo_full_name = gps.repo_full_name
        )
    ) AS can_fire
    FROM github_pr_sessions gps
    LEFT JOIN repo_settings rs ON rs.repo_full_name = gps.repo_full_name
    WHERE gps.session_id = s.id
) reretrigger ON true
LEFT JOIN LATERAL (
    SELECT min(rmp.created_at) AS pending_since
    FROM release_manifest_pending rmp
    WHERE rmp.session_id = s.id
) releasepending ON true
LEFT JOIN LATERAL (
    SELECT max(rcr.claimed_at) AS claimed_at
    FROM release_manifest_checks_running rcr
    WHERE rcr.session_id = s.id
) releaserunning ON true
WHERE s.id = sqlc.arg('session_id');

-- name: RequestSessionStop :one
-- A person's stop request on this session (technical plan §3.3, POST
-- /api/sessions/{sessionID}/stop, migrations/000151). The session keeps
-- the latest request's instant: a repeated request moves it forward, so
-- the stop timer's handler disarms the work-creating timers armed before
-- the latest request, not only those armed before the first
-- (sessionactor.disarmWorkCreatingTimers). GREATEST, not a plain now(): two
-- requests serialized by the lock below in the other order than their
-- transactions began never move it back. Each turn keeps its own first
-- flag (RequestStopOpenTurns), which its grace runs from. Runs under
-- GetSessionActorEpochForUpdate's own lock, in the transaction that flags
-- the session's open turns and arms its stop timer.
UPDATE sessions
SET stop_requested_at = GREATEST(stop_requested_at, now())
WHERE id = $1
RETURNING stop_requested_at;

-- name: ClearSessionStopRequest :execrows
-- A person's next act that sets the session going again clears the
-- request -- stop is not an archive: the next turn a person creates on the
-- session, the approval of its plan, or a person's decision to approve or
-- revise a workflow step awaiting it. Turns keep their own flags; only the
-- session's -- what refuses a new child session, and ends a workflow run
-- whose attempt ends -- is cleared.
UPDATE sessions
SET stop_requested_at = NULL
WHERE id = $1 AND stop_requested_at IS NOT NULL;

-- name: GetSessionStopRequestedAtForShare :one
-- Read by httpapi.CreateSessionOnTx before it inserts a child session of
-- this one. FOR SHARE, not the foreign key's own KEY SHARE: KEY SHARE does
-- not conflict with RequestSessionStop's UPDATE and never reads the
-- request, FOR SHARE conflicts with the stop's locks and reads the row the
-- stop committed. So a child spawn racing a stop either commits first, and
-- the stop's walk of the parent's descendants finds it, or waits for the
-- stop and is refused.
SELECT stop_requested_at FROM sessions
WHERE id = $1
FOR SHARE;

-- name: ListChildSessionIDs :many
-- The direct children of one session (sessions_parent_session_id_idx),
-- oldest first: the stop request's walk reads them after the parent's own
-- stop has committed.
SELECT id FROM sessions
WHERE parent_session_id = $1
ORDER BY created_at, id;

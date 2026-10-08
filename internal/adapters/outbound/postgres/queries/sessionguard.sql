-- name: GetSessionGuardFacts :one
-- What the session guard (internal/domain/sessionguard, technical plan
-- §40.1) reads about one session before it admits a turn: what the session
-- has spent and the caps that apply to it, in one statement and so one
-- snapshot. Run in the transaction that holds the session's row lock
-- (SessionStore.GetActorEpochForUpdate), which every actor transaction and
-- every turn insert on an existing session takes first, and every step cost
-- is written under: a cost committed before the lock is read here, and one
-- written after waits for this transaction.
--
-- spent_usd: SUM(cost_usd) over the session's dispatched turns, derived
-- from the rows that exist, never a counter. Only a dispatched turn carries
-- cost -- a step's cost lands on the turn processing when its step_finish
-- arrives (RecordTurnStepCost), and a turn reaches processing only through
-- dispatched, which stamps dispatched_at -- so naming dispatched_at IS NOT
-- NULL loses nothing, and lets turns_session_dispatched_idx (migrations/
-- 000166) answer the read from the session's own dispatched turns. A cost
-- that arrives with no turn processing is counted nowhere, so this is a
-- lower bound of the bill (§25.15). Cast to a scale of 6, cost_usd's own,
-- so the guard converts it to micro-dollars exactly.
--
-- turn_count: how many turns the session has, every status, read from the
-- session's own range of (session_id, dispatched_message_id) (000131), the
-- one index of turns that covers every turn of a session. A turn exists
-- only once the guard admitted it and is never deleted, so the count grows
-- exactly when the session is admitted a turn: it names the crossing a
-- refusal belongs to (sessionguard.WarningKey).
--
-- automation_*: the automation that created the session, read through its
-- earliest automation_runs row (automation_runs_session_id_idx, 000165),
-- then by its own key, and its cap; NULL for a session no automation
-- created. The automation's cap takes precedence over the repositories'
-- when it is set.
--
-- cap_repo_full_name/repo_cap_usd: the strictest cap among the session's
-- repositories, and which one set it. A session names its repositories two
-- ways, read as FirstRevokedRepoForSession reads them: the clone URLs in
-- sessions.repos, parsed by the caller into repo_full_names, and the
-- pull-request claims keyed to the session -- a review session's
-- github_pr_sessions row (github_pr_sessions_session_id_idx, 000032) and a
-- sentinel auto-fix child's sentinel_fixes row
-- (sentinel_fixes_fix_child_session_id_idx, 000047). The claims name the
-- pull request's base repository, which the clone URL of a fork pull
-- request's review session opened before its spec named the base
-- (technical plan §30.4) does not until it moves, so such a session takes
-- the base repository's cap too. The three lists are one array, so
-- repo_settings is read
-- by its primary key, one probe a name, whatever the table holds.
--
-- The amounts are returned as the database prints them, scale 6 for the
-- spend and 2 for a cap, so the caller converts them to micro-dollars
-- exactly (sessionguard.ParseMicroUSD); an absent automation or cap reads
-- '' (and automation_id NULL), which sqlc cannot type as nullable through
-- a LEFT JOIN LATERAL.
--
-- pgx.ErrNoRows: the session does not exist.
SELECT
    s.id AS session_id,
    now()::timestamptz AS observed_at,
    COALESCE((SELECT SUM(t.cost_usd) FROM turns t
              WHERE t.session_id = s.id AND t.dispatched_at IS NOT NULL), 0)::numeric(20, 6)::text AS spent_usd,
    (SELECT COUNT(*) FROM turns c WHERE c.session_id = s.id)::bigint AS turn_count,
    ac.automation_id::uuid AS automation_id,
    COALESCE(ac.automation_name, '')::text AS automation_name,
    COALESCE(ac.automation_cap_usd::text, '')::text AS automation_cap_usd,
    COALESCE(rc.cap_repo_full_name, '')::text AS cap_repo_full_name,
    COALESCE(rc.repo_cap_usd::text, '')::text AS repo_cap_usd
FROM sessions s
LEFT JOIN LATERAL (
    SELECT r.automation_id
    FROM automation_runs r
    WHERE r.session_id = s.id
    ORDER BY r.created_at, r.id
    LIMIT 1
) ar ON true
LEFT JOIN LATERAL (
    SELECT a.id AS automation_id, a.name AS automation_name, a.session_spend_cap_usd AS automation_cap_usd
    FROM automations a
    WHERE a.id = ar.automation_id
) ac ON true
LEFT JOIN LATERAL (
    SELECT rs.repo_full_name AS cap_repo_full_name, rs.session_spend_cap_usd AS repo_cap_usd
    FROM repo_settings rs
    WHERE rs.repo_full_name = ANY (sqlc.arg('repo_full_names')::text[]
            || ARRAY(SELECT g.repo_full_name FROM github_pr_sessions g WHERE g.session_id = s.id)
            || ARRAY(SELECT f.repo_full_name FROM sentinel_fixes f WHERE f.fix_child_session_id = s.id))
      AND rs.session_spend_cap_usd IS NOT NULL
    ORDER BY rs.session_spend_cap_usd, rs.repo_full_name
    LIMIT 1
) rc ON true
WHERE s.id = sqlc.arg('session_id');

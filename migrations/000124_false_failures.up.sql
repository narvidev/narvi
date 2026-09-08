-- false_failures: durable, per-incident record of §5.3's own "watchdog
-- kills later proven alive" event -- the platform-wide analytics rollup
-- (§12.2 item 6's "false failures" KPI, target 0) needs a Postgres row it
-- can COUNT over a time window; recordFalseFailureIfApplicable
-- (internal/app/sessionactor/pushpr.go) already detects this exact fact
-- (a late, genuine execution_complete{outcome:completed} arriving for a
-- session whose own currently-derived state is already Failed with
-- failure_reason=timeout, §3.2's two-phase-terminalization residual) and,
-- until this migration, only ever incremented the process-lifetime OTel
-- counter turn_false_failure_total (opsmetrics.go) -- real for a live
-- dashboard, but unqueryable by time window and gone on process restart.
-- This table is the durable sibling: the OTel counter is UNCHANGED (both
-- are incremented together, from the SAME call site, in the SAME
-- transaction), so neither replaces the other -- the same "one fact, two
-- consumers" split §21.1's own head_sha paragraph already establishes for
-- a different value.
--
-- One row per detected incident (never upserted or deduplicated beyond
-- what recordFalseFailureIfApplicable's own `inserted` gate already
-- guarantees at the call site -- see that function's own doc comment):
-- session_id is NOT unique here on purpose. A session whose own terminal
-- state was wrongly Failed more than once across its life (a rare but
-- legal edge case: multiple turns, more than one of which independently
-- raced past terminal_grace) is two incidents, not one collapsed row --
-- mirrors turn_step_costs' own "kept, not discarded" precedent (migrations/
-- 000098_turns_cost_usd.up.sql/000099): a count that cannot be recomputed
-- from anything is a number nobody can check.
--
-- No FK-cascade concern beyond the ordinary one: ON DELETE CASCADE mirrors
-- every other session-scoped table in this schema (sandbox_history, events,
-- turns, ...) -- a session's own false-failure history has no meaning once
-- the session itself is gone.
CREATE TABLE false_failures (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports the analytics rollup's own "COUNT(*) WHERE detected_at >= $1"
-- window scan (§21.1's "bounded from day one" discipline, applied here to
-- a platform-wide rather than repo-scoped rollup).
CREATE INDEX false_failures_detected_at_idx ON false_failures (detected_at);

-- Step 120 ("analytics: platform-wide rollup"): three targeted indexes for
-- the new platform-wide analytics read model (internal/app/
-- platformanalytics), each backing exactly one bounded WHERE created_at
-- >= $1 window scan that did not exist before this Step -- none of
-- sessions/turns/events had an index usable for a created_at range scan
-- (sessions_failed_idx, migrations/000065, is filtered on status='failed'
-- and keyed on updated_at, a different column and a different, narrower
-- row set).
--
-- sessions_created_at_idx: backs ListSessionOutcomeCountsInWindow
-- (queries/sessions.sql) -- the single shared read behind the "Sessions"
-- KPI tile, the "sessions per day by outcome" chart, the "success rate"
-- KPI, and the "top failure reasons" chart (all four are pure-SQL
-- GROUP BY reductions over this one index's own row range, never a
-- second, wider scan).
CREATE INDEX sessions_created_at_idx ON sessions (created_at);

-- turns_cost_created_at_idx: a PARTIAL index (cost_usd IS NOT NULL) --
-- backs the "Cost" KPI tile and "cost by model" chart (queries/turns.sql),
-- both scoped to turns that actually recorded a cost figure
-- (migrations/000098_turns_cost_usd.up.sql's own "NULL means no cost data
-- has arrived yet" contract); a turn that never ran a costed step is
-- irrelevant to either rollup and is excluded from the index itself, not
-- just from the query's own WHERE clause, keeping the index materially
-- smaller than every turn this deployment has ever run.
CREATE INDEX turns_cost_created_at_idx ON turns (created_at) WHERE cost_usd IS NOT NULL;

-- events_boot_timing_created_at_idx: a PARTIAL index (type = 'boot_timing')
-- -- backs the "Boot p95" KPI tile (queries/events.sql), scoped to the
-- one event type §33.3's boot-timing relay ever emits; events is this
-- deployment's single busiest table (every typed wire event, of which
-- boot_timing is one of roughly twenty $defs, §6.1), so an unqualified
-- index on (created_at) alone would carry every other event type's rows
-- for no benefit to this one rollup.
CREATE INDEX events_boot_timing_created_at_idx ON events (created_at) WHERE type = 'boot_timing';

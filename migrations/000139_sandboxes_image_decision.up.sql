-- §19 ("warm-boot shared-image prebuilds"): internal/app/sessionactor/
-- imageresolve.go's resolveAndSetImage decides, per spawn/restore,
-- whether a session boots warm (a real, previously-built image_ref) or
-- falls back to defaultBaseImage -- but until this migration recorded
-- NOTHING about which of the two happened, or why: roughly twenty
-- distinct early-return paths (repo-access-gate denials, an image_builds
-- lookup/tracking-write failure, a fingerprint with no ready row yet, a
-- data-integrity anomaly, or a genuine warm hit) all left only a log line
-- behind -- gone the moment the pod that emitted it recycles, and
-- entirely absent from Postgres, so the cause of a cold boot could never
-- be read back once the page reloaded.
--
-- image_decision_reason is internal/domain/imagedecision.Reason's own
-- closed vocabulary, persisted here as a REAL Postgres ENUM (not a
-- CHECK-constrained TEXT column) -- this codebase's own established
-- precedent for a closed vocabulary that is genuinely queried/branched on
-- directly in SQL (sandbox_status, image_build_status; see
-- migrations/000095_environment_docker_egress.up.sql's own doc comment
-- for the criterion this repo already uses to choose between the two).
-- The whole point of this column is to be counted and compared later
-- ("warm-boot hit rate" = a GROUP BY over exactly this column) -- a
-- schema-level enum constraint, not prose asserting a Go switch is
-- exhaustive, is what keeps that count meaningful: a future write that
-- passes anything outside this closed set is rejected by Postgres itself,
-- not merely by convention.
--
-- Two DIFFERENT one-sided edits can drift this enum from
-- internal/domain/imagedecision.All(), and two DIFFERENT guards, pinned
-- by two different symbols, catch them -- see that package's own doc
-- comment for the full four-mechanism account this is a part of:
--   * This migration's own value list changes without All() following
--     (or vice versa): imagedecision_integration_test.go's own
--     TestImageDecisionReasonEnum_MatchesGoVocabulary reads this enum's
--     real, live pg_enum labels back and diffs them against All() on
--     every run against a real Postgres instance.
--   * A new Reason constant is added to that package's own const block
--     but never added to All() (so this migration's own value list has
--     no way to know it should change at all): that is a Go-only, build-
--     time problem this migration cannot see by construction -- closed
--     instead by tools/lint/narvichecks/reasoncoverage's own Analyzer
--     (run by `make lint`), which fails the build the moment such a
--     constant is declared.
--
-- Nullable, on BOTH new columns, but NOT with identical meaning:
--   * image_decision_reason: NULL means "no decision has been recorded
--     for this sandbox row's CURRENT gen yet" -- distinct from every real
--     value, including 'selected'.
--   * image_decision_fingerprint: NULL means EITHER of two things --
--     "no decision recorded for this gen yet" (the same case as above),
--     OR "a decision WAS recorded, but there was nothing to fingerprint"
--     (imagedecision.ReasonNoRepos, the one outcome with zero configured
--     repos -- see resolveAndSetImage's own top-of-function bare return,
--     imageresolve.go). A NULL fingerprint therefore does NOT by itself
--     imply no decision was recorded; check image_decision_reason for
--     that. queries/sandboxes.sql's own UpdateSandboxImageDecision
--     comment and imagedecision_integration_test.go's own
--     TestResolveAndSetImage_NoRepos_PersistsNoReposReason both already
--     depend on exactly this narrower reading.
--
-- Both columns are the ordinary state for every pre-existing sandboxes
-- row (created before this migration), for a sandbox that has never
-- reached resolveAndSetImage (e.g. a resume, which this function is
-- never called for -- dispatch.go's own doc comment), and for the brief
-- window between UpsertSandboxForSpawn's own gen bump and
-- resolveAndSetImage's later write for that same gen. NULL must never be
-- read as "cold boot" or as "warm boot" on either column -- it is
-- "unknown", the same non-committal zero value agent_version/image_digest
-- (migrations/000120_sandboxes_boot_fingerprint.up.sql) already
-- established for this exact table.
--
-- Reset to NULL on every respawn (queries/sandboxes.sql's own
-- UpsertSandboxForSpawn, ON CONFLICT branch) for the identical reason
-- agent_version/image_digest already are: a stale previous-gen reason
-- lingering into this gen's own connecting/booting window would be
-- actively misleading.
--
-- Deliberately NOT re-exposed through any new REST/DTO surface here: the
-- decision is ALSO appended, in the SAME transaction, to the existing
-- append-only events log (type = 'image_decision', payload carries the
-- identical reason plus fingerprint/context) -- and that log already
-- reaches every reader (§6.2's client-WS subscribe/fetch_history replay,
-- §6.3's existing GET .../events REST route) with no schema or route
-- change of its own, exactly the "reusing the existing collection and
-- event log rather than adding a surface" instruction this migration
-- exists to satisfy. These two new sandboxes columns mirror
-- agent_version/image_digest's own "per-gen fact on the sandboxes row"
-- STORAGE shape exactly (same table, same respawn-reset rule) -- but NOT
-- their READABILITY: agent_version/image_digest are read back and
-- rendered by a client today (internal/adapters/inbound/wshub's own
-- dispatch/client wiring); nothing reads these two columns back through
-- any REST/DTO/WS surface, so "cheap to read without scanning the event
-- log" is true only for direct SQL/operator access, not for a client --
-- a client wanting this decision has only the event log to read, exactly
-- like every other reader of it.
CREATE TYPE image_decision_reason AS ENUM (
    'selected',
    'no_repos',
    'repo_access_no_creator',
    'repo_access_creator_lookup_failed',
    'repo_access_creator_disabled',
    'repo_access_creator_viewer',
    'repo_access_creator_guard_unknown',
    'repo_access_unsupported_host',
    'repo_access_unparseable_url',
    'repo_access_cached_deny',
    'repo_access_circuit_breaker_open',
    'repo_access_no_token',
    'repo_access_no_source_control',
    'repo_access_check_indeterminate',
    'repo_access_denied',
    'image_build_lookup_failed',
    'image_build_tracking_marshal_failed',
    'image_build_tracking_upsert_failed',
    'image_build_pending',
    'image_build_not_ready',
    'image_build_ready_row_missing_ref',
    'unrecognized'
);

ALTER TABLE sandboxes ADD COLUMN image_decision_reason image_decision_reason;
ALTER TABLE sandboxes ADD COLUMN image_decision_fingerprint TEXT;

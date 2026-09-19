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
-- not merely by convention. internal/domain/imagedecision's own package
-- doc comment states the twin obligation this creates: the Go vocabulary
-- and this enum's value set must never drift apart, and an integration
-- test (imagedecision_integration_test.go) reads this enum's real,
-- live pg_enum labels back and diffs them against the Go constant list on
-- every run, so a one-sided edit fails CI rather than silently drifting.
--
-- Nullable, on BOTH new columns: NULL means "no decision has been
-- recorded for this sandbox row's CURRENT gen yet" -- distinct from every
-- real value, including image_decision_reason = 'selected'. This is the
-- ordinary state for every pre-existing sandboxes row (created before
-- this migration), for a sandbox that has never reached
-- resolveAndSetImage (e.g. a resume, which this function is never called
-- for -- dispatch.go's own doc comment), and for the brief window between
-- UpsertSandboxForSpawn's own gen bump and resolveAndSetImage's later
-- write for that same gen. NULL must never be read as "cold boot" or as
-- "warm boot" -- it is "unknown", the same non-committal zero value
-- agent_version/image_digest (migrations/000120_sandboxes_boot_
-- fingerprint.up.sql) already established for this exact table.
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
-- exists to satisfy. These two new sandboxes columns are the CURRENT-value
-- half of that pair (cheap to read without scanning the event log),
-- mirroring agent_version/image_digest's own identical role for the boot
-- fingerprint.
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
    'image_build_ready_row_missing_ref'
);

ALTER TABLE sandboxes ADD COLUMN image_decision_reason image_decision_reason;
ALTER TABLE sandboxes ADD COLUMN image_decision_fingerprint TEXT;

-- review_check_writer_app_id (finding B2): the DURABLE, Postgres-owned
-- home for this deployment's own SELF-OBSERVED writer App id -- the
-- SAME value internal/app/outboxworker's own review-check notifier used
-- to keep ONLY in an in-process atomic.Int64 (finding A2's own doc
-- comment on that field, before this fix). §5.1: "Postgres is the ONLY
-- store. No cache with authority." -- an in-process-only value that
-- decides whether a crash-recovery adoption (finding A5, "select by SHA
-- and GitHub App") can ever succeed IS a cache with authority: a fresh
-- process (a restart, a new pod) starts back at "not yet observed" even
-- though this deployment's own credential has, in every practical sense,
-- already been observed writing as some App id, possibly minutes
-- earlier, on some OTHER pull request entirely -- exactly the crash
-- between a successful CreateCheckRun and this system's own record of
-- its id that adoption exists to recover from.
--
-- ONE row, ALWAYS id = 1 (CHECK enforces it; no natural key exists for
-- "this deployment's own bot credential" today, and inventing one is not
-- this fix's job) -- mirrors digest_send_state's own "claim-before-act"
-- singleton shape in spirit, though this table is a plain
-- read-then-upsert, not a claim: at most one writer App id is ever true
-- for a given bot credential, so a concurrent write racing another is a
-- last-write-wins overwrite of the SAME fact, never two different valid
-- facts competing.
--
-- app_id is GitHub's own numeric App id, exactly the value
-- CreateCheckRun's own response already reports back (App.ID,
-- checkruns.go) -- never a separately-configured value (this table's own
-- reason for existing is identical to why the in-process cache it
-- replaces was never a config field either: platform.Config.GitHubAppID
-- names a DIFFERENT, unrelated GitHub App, finding A2's own doc comment).
--
-- Residual this does NOT close: the very FIRST check-run creation this
-- deployment ever makes, deployment-wide, still races unrecovered if a
-- crash happens between that first CreateCheckRun succeeding and this
-- row being persisted -- there is no earlier observation to fall back
-- to. Every subsequent crash, for any PR, recovers correctly once this
-- row has been written once.
CREATE TABLE review_check_writer_app_id (
    id         SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id     BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

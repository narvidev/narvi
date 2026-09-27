-- A session's status (technical plan §43.20) must not read settled while
-- the server holds work that can create a turn on that session with no
-- new input. A release PR's review session is one such case: its release
-- manifest check (migrations/000050) runs in the background after the
-- session is created, and when the check triggers the aggregate
-- composition review (§15.3) it inserts one more turn on that same
-- session (internal/app/releasereview's dispatchCompositionReview).
--
-- A claim DELETES its release_manifest_pending row before the check runs
-- (ClaimDueReleaseManifestPending, unchanged), so for the whole check --
-- up to platform.Timeouts.ReleaseManifestCheckTimeout of GitHub calls --
-- nothing in that table says a turn might still come.
--
-- release_manifest_checks_running is that fact. The claim inserts one row
-- here, in the SAME transaction as its delete, stamped with the
-- database's now(); the worker deletes it once the check has returned
-- (after any composition turn it inserted has committed), so a snapshot
-- that holds neither row already holds any turn the check inserted.
-- A row whose worker died mid-check is purged by the next tick once it is
-- older than ReleaseManifestCheckTimeout plus MCPStatusScheduledMargin,
-- the same bound past which the status stops counting it.
--
-- Why a table of its own, not a claimed_at column on the pending row:
-- keeping the claim a delete keeps it at most once in any fleet. A pod
-- still on the previous binary during a rolling deploy runs the previous
-- claim, which deletes whatever pending row it finds; it never sees this
-- table, so it can never take a check a newer pod is running. A rollback
-- (this migration's down) loses at most the status of checks in flight,
-- never runs one twice.
--
-- pending_id is the release_manifest_pending id the check was claimed
-- from (that row is gone, so no foreign key). The status reads both tables
-- by session, so each gets a session_id index.
CREATE TABLE release_manifest_checks_running (
    pending_id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX release_manifest_checks_running_session_id_idx ON release_manifest_checks_running (session_id);

CREATE INDEX release_manifest_pending_session_id_idx ON release_manifest_pending (session_id);

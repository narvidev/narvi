-- A session's status (technical plan §43.20) must not read settled while
-- the server holds work that will create a turn on that session with no
-- new input. A release PR's review session is one such case: its release
-- manifest check (migrations/000050) runs in the background after the
-- session is created, and when the check triggers the aggregate
-- composition review (§15.3) it inserts one more turn on that same
-- session (internal/app/releasereview's dispatchCompositionReview).
--
-- Until now a claim DELETED the row before the check ran, so for the whole
-- check -- up to platform.Timeouts.ReleaseManifestCheckTimeout of GitHub
-- calls -- nothing in the database said a turn might still come, and a
-- review turn that finished first read as a finished session.
--
-- claimed_at keeps the row through its one attempt: a claim now sets it to
-- the database's now(), the worker deletes the row once the check has
-- returned (after any composition turn it inserted has committed), and a
-- claimed row is never claimed again, so the check still runs at most
-- once. A row whose worker died mid-check stays claimed; the status stops
-- counting it once ReleaseManifestCheckTimeout (plus
-- MCPStatusScheduledMargin) has passed since the claim, and the worker's
-- next tick deletes it. NULL means not claimed yet.
--
-- The status reads these rows by session, so session_id gets the index
-- every other lookup in that statement already leads with.
ALTER TABLE release_manifest_pending ADD COLUMN claimed_at TIMESTAMPTZ;

CREATE INDEX release_manifest_pending_session_id_idx ON release_manifest_pending (session_id);

-- A null-phase heartbeat moves a sandbox Booting -> Ready only once the
-- same gen has shown boot evidence (technical plan §3.2,
-- migrations/000147_sandbox_boot_evidence_gen.up.sql). A sandbox-agent
-- built before boot_timing existed (2026-08-20), booting a repo with no
-- service and no Docker, never shows any: it sends null from connect to
-- the end, with nothing else that names its boot. Such a sandbox stayed
-- Booting for as long as its heartbeats flowed, and every restore of its
-- snapshot, which carries the same agent, did the same.
--
-- booting_since is when the gen in booting_since_gen entered Booting, on
-- this database's clock. Once a gen with no evidence has been Booting,
-- heartbeats still arriving, for longer than
-- platform.Timeouts.BootEvidenceFallback -- longer than any boot that
-- agent can still be running -- its null phase is accepted as boot
-- completion (internal/app/sessionactor's handleSandboxEvent). Stored
-- against the gen, as boot_evidence_gen is, so a respawn needs no reset:
-- the new gen's own Booting edge records its own start, and the previous
-- one stops matching.
--
-- No backfill. A sandbox Booting when this runs has no start recorded; the
-- first null-phase heartbeat that finds none records one, which only
-- starts the clock later than it could have.
ALTER TABLE sandboxes
    ADD COLUMN booting_since TIMESTAMPTZ,
    ADD COLUMN booting_since_gen INTEGER;

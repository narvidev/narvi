-- review_check_runs: the review's own GitHub-native result surface
-- (§8.2/§21.1/§21.1b) publisher identity claim -- ONE row
-- per (repo_full_name, pr_number), mirroring github_pr_sessions' own
-- identical per-PR claim shape (migrations/000028's own doc comment) for
-- the SAME reason: "two active identities for one pull request is worse
-- than none" (§21.1b) -- a UNIQUE-by-primary-key row is what makes a
-- second, concurrently-created identity structurally unrepresentable
-- rather than merely avoided by convention.
--
-- This table is NOT review_verdicts' analytics history (§21.1, append-
-- only, one row per POST) -- it is the publisher's own, single-row-per-PR
-- record of "which external GitHub check run currently speaks for this
-- pull request, and what was last published to it". A GitHub check run
-- is scoped to one commit SHA (GitHub's Checks tab shows only the check
-- runs that exist for a PR's CURRENT head), so external_id below is
-- expected to rotate as head_sha changes -- see internal/domain/
-- reviewcheck's own doc comment for the full "why a separate identity
-- concept from review_verdicts" reasoning, and internal/app/
-- outboxworker's own review-check notifier for the claim sequence that
-- writes this row (ensure the row exists via ON CONFLICT, lock it,
-- decide via internal/domain/reviewcheck.Supersedes, release the lock,
-- THEN call GitHub -- a real outbound HTTP call must never run while a
-- Postgres transaction is held open, mirroring ports.Notifier.Deliver's
-- own "always outside any tx" contract, so the lock spans only the
-- decide-and-reserve step, never the network call).
--
-- head_sha/attempt_id/attempt_created_at/phase/base_ref/base_sha/
-- policy_version are the fields internal/domain/reviewcheck.Emission
-- carries -- forwarded verbatim from whichever caller last won
-- Supersedes against this row (see that function's own doc comment for
-- the full ordering rule: a different, newer attempt wins by
-- attempt_created_at; the same attempt only ever moves phase forward,
-- never backward). attempt_id is nullable (NULL for a PhaseQueued row:
-- "PR enters scope" precedes any real attempt/turn existing at all) and
-- ON DELETE SET NULL, mirroring review_verdicts.attempt_id's own
-- identical policy (migrations/000130's own doc comment) -- a turn row
-- being deleted (not a real operation today) must never cascade into
-- losing this claim row's own identity.
--
-- external_id is GitHub's own check_run id (a plain integer GitHub
-- assigns on creation) -- nullable: NULL means either no external check
-- run has been created yet for this row's CURRENT head_sha, or the
-- create call is genuinely in flight (the notifier's own reserve-then-
-- call-then-record sequence, above). A caller observing NULL here before
-- calling GitHub's own create-check-run endpoint first re-lists check
-- runs for this exact (head_sha, this deployment's own GitHub App id) --
-- "select by SHA and GitHub App, so another app's same-named check is
-- never adopted" (the brief's own identity rule) -- and adopts a match
-- rather than creating a duplicate, the recovery path for a crash
-- between a successful GitHub create and this column being persisted.
--
-- (repo_full_name, pr_number) is the natural key, exactly like
-- github_pr_sessions' own identical primary key (migrations/000028's own
-- doc comment) -- this table has no session_id column of its own: a
-- reader that needs the originating session joins through
-- github_pr_sessions on the same natural key, never a second,
-- independently-maintained copy of that mapping.
CREATE TABLE review_check_runs (
    repo_full_name     TEXT NOT NULL,
    pr_number          INTEGER NOT NULL,
    head_sha           TEXT NOT NULL,
    external_id        BIGINT,
    attempt_id         UUID REFERENCES turns(id) ON DELETE SET NULL,
    attempt_created_at TIMESTAMPTZ,
    phase              TEXT NOT NULL,
    base_ref           TEXT,
    base_sha           TEXT,
    policy_version     INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_full_name, pr_number)
);

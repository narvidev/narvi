-- Queries backing AutoApprovalOutcomeStore (§21.2 stage 2) -- see
-- migrations/000070_auto_approval_outcomes.up.sql's own doc comment for
-- the contradiction-rate calibration read model's full design.

-- name: RecordAutoApprovalOutcome :exec
-- Idempotent per (repo_full_name, pr_number, head_sha) -- the SAME
-- "INSERT ... ON CONFLICT DO NOTHING" atomic-claim idiom github_pr_sessions/
-- repo_settings already establish. A second call recording a DIFFERENT
-- outcome for the same (repo, PR, head_sha) -- e.g. 'overridden' observed
-- after 'confirmed' was already recorded, which should not happen in
-- practice since a merged PR's own review_verdicts row cannot later
-- regain a fresh HasChangesRequested against that SAME now-closed PR --
-- is deliberately a no-op, never a silent overwrite: the FIRST recorded
-- outcome for a given verdict wins, mirroring review_findings' own
-- "first_seen_at is set once, never overwritten" precedent
-- (migrations/000046) for the analogous "durable first observation"
-- shape.
--
-- suppressed_in_shadow (migrations/000111) is §30.7's own calibration-read
-- exclusion: an outcome observed while the repository's egress was
-- suppressed is recorded, so the operator ledger can show it, and
-- EXCLUDED from the contradiction rate below -- that number is the
-- instrument justifying auto-merge, and moving it with verdicts nobody
-- ever saw is the falsification §30.7 rules out.
INSERT INTO auto_approval_outcomes (repo_full_name, pr_number, head_sha, outcome, suppressed_in_shadow)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_full_name, pr_number, head_sha) DO NOTHING;

-- name: CountAutoApprovalOutcomesInWindow :one
-- The contradiction-rate rollup's own single bounded aggregate query --
-- total (every recorded outcome) and contested (outcome IN ('overridden',
-- 'accepted_override')) counts for repoFullName since sinceTime, in one
-- round trip. internal/domain/reviewverdict.ContradictionRate reduces
-- these two plain integers -- this query does no rate arithmetic itself
-- (§11: no floating-point policy math in a SQL query the domain layer
-- should instead own and unit-test).
--
-- 'accepted_override' (finding F1, adversarial review) counts as
-- contested for the SAME reason 'overridden' does: a PR that merged only
-- because a human's acceptance waived the engine's own refusal
-- (internal/domain/reviewverdict.OutcomeAcceptedOverride's own doc
-- comment) is not evidence the engine's judgment stood, and excluding it
-- from this filter would mechanically drive the contradiction rate down
-- with every acceptance-driven merge -- exactly the failure §21.1b names.
SELECT
    count(*) AS total,
    count(*) FILTER (WHERE outcome IN ('overridden', 'accepted_override')) AS contested
FROM auto_approval_outcomes
WHERE repo_full_name = $1 AND decided_at > $2
  -- §30.7: a shadow-era outcome is recorded but never calibrates. See
  -- RecordAutoApprovalOutcome above.
  AND NOT suppressed_in_shadow;

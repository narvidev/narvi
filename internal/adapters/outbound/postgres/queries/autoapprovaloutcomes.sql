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
-- total and contested (outcome = 'overridden') counts for repoFullName
-- since sinceTime, in one round trip. internal/domain/reviewverdict.
-- ContradictionRate reduces these two plain integers -- this query does
-- no rate arithmetic itself (§11: no floating-point policy math in a SQL
-- query the domain layer should instead own and unit-test).
--
-- 'accepted_override' is EXCLUDED from both total and contested (round-5
-- adversarial review, finding V1 -- correcting a prior version of this
-- comment that counted it in both). §21.2 defines the rate as "the
-- fraction of auto-approved PRs a human later disagreed with" -- the
-- SAME population restdtos.RepoSettings.contradictionRatePercent's own
-- doc comment and web/src/session/RepoSettingsView.tsx's own rendered
-- copy both describe. A PR recorded 'accepted_override' was never
-- auto-approved at all: the engine REFUSED it, and a human's acceptance
-- waived that refusal (internal/domain/reviewverdict.
-- OutcomeAcceptedOverride's own doc comment). Counting it in `total`
-- (the query's PREVIOUS behaviour) put it in a denominator its own
-- outcome value says it does not belong to, and every such row diluted
-- the measured rate for repos where the engine's judgment, on the PRs it
-- actually approved, stood every time -- e.g. ten 'confirmed' plus five
-- 'accepted_override' read as 33.3% contradicted (5/15) although the
-- engine was right ten times out of ten of its own approvals. The prior
-- comment here justified counting 'accepted_override' as contested by
-- claiming excluding it "would mechanically drive the contradiction rate
-- down with every acceptance-driven merge" -- false on the arithmetic:
-- on 10 confirmed + 1 genuine 'overridden' + 5 'accepted_override',
-- folding acceptances into 'confirmed' gives 6.25% (the real hazard),
-- excluding them from both counts (this query, now) gives 9.1% (the
-- untouched true rate over the population that WAS auto-approved), and
-- counting them as contested (the prior version) gave 37.5%. Excluding
-- is conservative either way for the arm-or-don't decision this metric
-- exists to inform (an admin sees a HIGHER, not lower, rate than if
-- acceptances were silently folded into 'confirmed') -- it is simply the
-- reading its own contract already promised. An accepted-override PR is
-- still recorded (RecordAutoApprovalOutcome above) and still visible
-- per-PR in the decision inbox (internal/app/decisioninbox) -- this
-- exclusion narrows only THIS aggregate's population, it does not erase
-- the observation.
SELECT
    count(*) AS total,
    count(*) FILTER (WHERE outcome = 'overridden') AS contested
FROM auto_approval_outcomes
WHERE repo_full_name = $1 AND decided_at > $2
  AND outcome != 'accepted_override'
  -- §30.7: a shadow-era outcome is recorded but never calibrates. See
  -- RecordAutoApprovalOutcome above.
  AND NOT suppressed_in_shadow;

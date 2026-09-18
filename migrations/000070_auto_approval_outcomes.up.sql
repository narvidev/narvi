-- auto_approval_outcomes (Step 62, §21.2 stage 2): the contradiction-rate
-- CALIBRATION read model -- "the fraction of auto-approved PRs a human
-- later disagreed with (overrode or requested changes on), per repo ...
-- accumulating from day one". An admin arms a repo's auto_merge_enabled
-- toggle (migrations/000069) only once this data justifies it.
--
-- One row per (repo_full_name, pr_number, head_sha) -- the SAME natural
-- key as the review_verdicts row (migrations/000067) an outcome is
-- recorded against, so at most one outcome is ever recorded per verdict,
-- even under concurrent writers (the UNIQUE constraint below + an
-- INSERT ... ON CONFLICT DO NOTHING idempotent write, the SAME atomic-
-- claim idiom github_pr_sessions/repo_settings already establish).
--
-- outcome is exactly one of:
--   'confirmed'         -- this auto-approved PR was actually merged (a
--                          human 1-click confirm while the repo's
--                          auto-merge toggle is off, §21.2 stage 2, or the
--                          toggle's own machine-initiated merge once
--                          armed) -- the engine's own judgment stood.
--   'overridden'        -- a human disagreed BEFORE any merge happened:
--                          either GitHub's own HasChangesRequested became
--                          true, or a review:needs-human label was
--                          applied, on a PR whose latest verdict the
--                          eligibility engine would otherwise have judged
--                          auto-approved -- recorded the FIRST time this
--                          is observed for a given (repo, PR, head_sha),
--                          never re-recorded on every subsequent read.
--   'accepted_override' -- (finding F1, adversarial review, added after
--                          this table first shipped) this PR merged ONLY
--                          because an applicable review_verdict_acceptances
--                          row ("human acceptance of a verdict the engine
--                          refuses", §21.1b) waived the engine's own
--                          refusal -- NEITHER 'confirmed' (the engine did
--                          not approve it) NOR 'overridden' (that value
--                          means a pre-merge human disagreement, never an
--                          actual merge). Counted as CONTESTED by
--                          CountAutoApprovalOutcomesInWindow, exactly like
--                          'overridden' -- an acceptance-driven merge must
--                          never mechanically drive the contradiction rate
--                          down. See internal/domain/reviewverdict.
--                          OutcomeAcceptedOverride's own doc comment.
-- Recorded from TWO existing call sites that already compute every fact
-- needed at zero extra cost (internal/app/decisioninbox's own
-- buildPROpenItem for 'overridden', and the merge-completion paths --
-- httpapi.MergePullRequest and internal/app/automerge's own worker -- for
-- 'confirmed'/'accepted_override', gated on RevalidateForMerge/
-- RevalidateForAutoMerge's own viaAcceptance return value) -- never a new
-- polling/reconciliation job of its own.
--
-- Correction (§62 review findings T1/M5): this comment's own claim that
-- httpapi.MergePullRequest already called RecordConfirmed was false when
-- first written -- only the armed auto-merge worker did, so during the
-- entire toggle-off calibration window this metric exists to inform,
-- only 'overridden' rows were ever recorded. Fixed in the same commit
-- that added this correction (httpapi/decisioninbox.go now calls
-- RecordConfirmed on the human 1-click merge-completion path too) -- see
-- internal/app/reviewverdict/outcomes.go's own RecordConfirmed doc
-- comment for the full "why".
--
-- Not an ENUM: mirrors review_findings.sentinel_kind/status's own
-- established "closed vocabulary lives in Go, not the schema" precedent
-- (migrations/000046) for a column whose sibling Go package
-- (internal/domain/reviewverdict) is already the source of truth for its
-- legal values.
CREATE TABLE auto_approval_outcomes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_full_name TEXT NOT NULL,
    pr_number      INTEGER NOT NULL,
    head_sha       TEXT NOT NULL,
    outcome        TEXT NOT NULL,
    decided_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (repo_full_name, pr_number, head_sha)
);

-- Backs the contradiction-rate rollup's own bounded, per-repo,
-- decided_at-windowed scan (§21.1's "bounded from day one" discipline,
-- applied here to this Step's OWN further read model).
CREATE INDEX auto_approval_outcomes_repo_decided_idx ON auto_approval_outcomes (repo_full_name, decided_at DESC);

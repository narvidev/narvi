-- review_verdict_acceptances: "human acceptance of a verdict the engine
-- refuses" (§21.1b) -- an AUTHORISATION, never an override, that a
-- maintainer+ records to proceed past a SPECIFIC review_verdicts row's
-- own human-judgment eligibility refusal (internal/domain/autoapproval.
-- ComputeEligibleWithAcceptance's own doc comment names exactly which
-- two criteria this can waive -- Shippable != auto, or the diff-size
-- threshold -- and which stay mandatory regardless: CI green, blast
-- radius known, sensitive path, and every freshness check §21.1's
-- amendment already enforces).
--
-- Binds to ONE verdict via verdict_id (review_verdicts.id) -- an
-- immutable, append-only row that ALREADY pins ONE attempt (its own
-- attempt_id column) and ONE context (its own base_ref/base_sha/
-- ancestor_chain/policy_version columns), so verdict_id alone is what an
-- acceptance's own applicability actually keys on -- see internal/domain/
-- reviewverdict.Acceptance.Applicable's own doc comment for the full
-- "why" and for the one freshness trigger (a moved base, or a changed
-- ancestor chain, under an UNCHANGED verdict) that check deliberately
-- does NOT try to catch itself, because autoapproval.
-- ComputeEligibleWithAcceptance's own unconditional base/ancestor-chain
-- checks already catch it independently, whether or not an acceptance
-- applies.
--
-- attempt_id/head_sha/base_ref/base_sha/ancestor_chain/policy_version
-- are ALSO stored here, verbatim, redundant with the referenced verdict
-- row -- never consulted by Applicable, which needs only verdict_id --
-- kept for the SAME reason review_check_runs carries this identical
-- shape (migrations/000132's own doc comment): a human or an operator
-- reading THIS row for audit purposes should never need a join back to
-- review_verdicts to see what, exactly, was accepted.
--
-- reason is the autoapproval.Reason value ComputeEligible returned at
-- accept time (e.g. "the verdict's shippable classification is not
-- auto") -- display/audit only, never re-checked: a caller applying this
-- acceptance always re-runs ComputeEligibleWithAcceptance against the
-- pull request's CURRENT live facts, which independently re-derives
-- whichever reason(s) still apply -- this column is never itself
-- consulted to decide whether an acceptance is honored.
--
-- justification is the accepting maintainer's own required, free-text
-- explanation (§21.1b: "carries author, justification") -- untrusted,
-- human-authored content, never rendered anywhere as an instruction
-- (mirrors review_false_positive_patterns.reason's own identical
-- discipline, migrations/000073).
--
-- revoked_at/revoked_by (NULL = still active) is this table's own
-- auditable-revocation column pair, mirroring review_false_positive_
-- patterns.retired_at/retired_by's own identical shape exactly (that
-- table's own migration doc comment) -- a row is never deleted, only
-- marked revoked, so the audit trail survives a revocation.
--
-- Deliberately APPEND-ONLY, never UPDATEd for a re-accept: multiple rows
-- may exist for the SAME pull request over its lifetime (one per attempt
-- that was ever accepted) -- internal/app/reviewverdict's own
-- GetActiveAcceptance "latest non-revoked row" read decides which one,
-- if any, is currently in force. No uniqueness constraint on
-- (repo_full_name, pr_number) is needed or enforced here: that read,
-- combined with Applicable's own verdict_id check, already makes a
-- superseded row harmless even if nobody ever explicitly revokes it
-- (§21.1b: an acceptance "makes it inapplicable... without a human
-- touching it").
CREATE TABLE review_verdict_acceptances (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_full_name TEXT NOT NULL,
    pr_number      INTEGER NOT NULL,
    verdict_id     UUID NOT NULL REFERENCES review_verdicts(id),
    attempt_id     UUID REFERENCES turns(id) ON DELETE SET NULL,
    head_sha       TEXT NOT NULL,
    base_ref       TEXT,
    base_sha       TEXT,
    ancestor_chain JSONB NOT NULL DEFAULT '[]'::jsonb,
    policy_version INTEGER NOT NULL DEFAULT 0,
    reason         TEXT NOT NULL,
    justification  TEXT NOT NULL,
    accepted_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    accepted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at     TIMESTAMPTZ,
    revoked_by     UUID REFERENCES users(id) ON DELETE SET NULL
);

-- The one read query the merge/decision-inbox path needs: the LATEST
-- non-revoked acceptance for one pull request, if any -- mirrors
-- review_false_positive_patterns_repo_active_idx's own identical
-- "index the exact WHERE clause the one real read query uses" precedent
-- (migrations/000073).
CREATE INDEX review_verdict_acceptances_active_idx
    ON review_verdict_acceptances (repo_full_name, pr_number, accepted_at DESC)
    WHERE revoked_at IS NULL;

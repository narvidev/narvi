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
-- ancestor_chain/policy_version columns). verdict_id equality is
-- necessary but NOT sufficient for "one attempt" to still hold (finding
-- F1, adversarial review, corrected -- a previous version of this
-- comment claimed verdict_id alone was sufficient): an attempt that ends
-- not_assessed posts no review_verdicts row at all, so a NEWER attempt
-- can exist while the latest verdict_id for this pull request stays
-- unchanged. internal/domain/reviewverdict.Acceptance.Applicable
-- therefore also takes a caller-resolved hasNewerAttempt fact
-- (internal/app/reviewverdict.HasNewerReviewAttempt, which queries
-- turns.is_review_attempt directly via attempt_id below) -- see that
-- method's own doc comment for the full "why" and for the one freshness
-- trigger (a moved base, or a changed ancestor chain, under an UNCHANGED
-- verdict AND no new attempt) that neither check tries to catch, because
-- autoapproval.ComputeEligibleWithAcceptance's own unconditional
-- base/ancestor-chain checks already catch it independently, whether or
-- not an acceptance applies.
--
-- attempt_id/head_sha/base_ref/base_sha/ancestor_chain/policy_version
-- are ALSO stored here, verbatim, redundant with the referenced verdict
-- row, for the SAME reason review_check_runs carries this identical
-- shape (migrations/000132's own doc comment): a human or an operator
-- reading THIS row for audit purposes should never need a join back to
-- review_verdicts to see what, exactly, was accepted. attempt_id is now
-- ALSO load-bearing, not merely display data (finding F1's own fix):
-- internal/app/reviewverdict.HasNewerReviewAttempt reads it back, via
-- turns.id, to resolve the accepted attempt's own turns.created_at,
-- which is what "is a NEWER review attempt on record" is actually
-- checked against.
--
-- reason is a best-effort, no-I/O classification (httpapi.
-- AcceptReviewVerdict's own accept-time guess) of which waivable
-- eligibility criterion this acceptance most likely addresses (e.g. "the
-- verdict's shippable classification is not auto") -- NOT itself computed
-- by calling autoapproval.ComputeEligible (finding F8, adversarial
-- review: this comment, and three others, previously claimed it was) --
-- display/audit only, never re-checked: a caller applying this
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
-- if any, is currently in force.
--
-- AT MOST ONE non-revoked row per (repo_full_name, pr_number), enforced
-- below by review_verdict_acceptances_one_active_idx (finding F2,
-- adversarial review, corrected before this table's first release: an
-- earlier version of this comment argued no uniqueness constraint was
-- needed here, reasoning that GetActiveAcceptance's "latest non-revoked
-- row" read, combined with Applicable's own verdict_id check, already
-- made a superseded row harmless -- that reasoning covers a NEW verdict
-- superseding an old acceptance, but not two acceptances coexisting for
-- the SAME verdict: revoking the row GetActiveAcceptance currently
-- reports as active would then silently re-activate the OTHER one,
-- since it is still non-revoked and still Applicable to the same
-- verdict_id -- a revocation that does not revoke is worse than none).
-- postgres.ReviewVerdictAcceptanceStore.Insert now supersedes any
-- existing active row for the SAME (repo_full_name, pr_number) via
-- SupersedeActiveReviewVerdictAcceptances, immediately before the INSERT
-- (queries/reviewverdictacceptances.sql) -- two SEQUENTIAL statements,
-- deliberately never one combined WITH-clause statement (that query's own
-- doc comment: verified against real Postgres, a single `WITH superseded
-- AS (UPDATE ...) INSERT ...` statement's own INSERT does not see the
-- CTE's UPDATE for unique-constraint purposes, since both share the SAME
-- start-of-query snapshot, and fails with a spurious duplicate-key error
-- on the routine re-accept case this exists to allow). This invariant is
-- still enforced by construction, never merely by convention --
-- review_verdict_acceptances_one_active_idx itself is what guarantees it,
-- regardless of the two statements' own relative timing; the supersede
-- step is what makes the ORDINARY case (an uncontested re-accept) succeed
-- cleanly rather than colliding with its own predecessor. Two SEQUENTIAL
-- statements is NOT the same claim as "two independently-committed
-- statements", though (finding F2, adversarial review, corrected -- a
-- previous version of this table shipped exactly that mistake: Insert
-- ran both statements against the bare, pool-bound store, so a context
-- cancellation/timeout/reset between them left the prior acceptance
-- revoked with NO new row created at all). The caller now MUST supply a
-- store already scoped via WithTx to an open transaction it began itself
-- and commits after Insert returns -- mirroring OIDCSigningKeyStore.
-- Rotate's own identical "the caller owns the transaction boundary, this
-- method does not" precedent one file over -- so the supersede and the
-- insert now commit or roll back together. See
-- ReviewVerdictAcceptanceStore.Insert's own doc comment (postgres
-- package) for the concrete caller (httpapi.AcceptReviewVerdict).
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
    revoked_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    -- revocation_reason (finding F4, adversarial review) distinguishes an
    -- EXPLICIT revocation (a maintainer+'s own POST
    -- /revoke-verdict-acceptance click, RevokeReviewVerdictAcceptance)
    -- from an AUTOMATIC supersession (a fresh Accept on the SAME pull
    -- request revoking the prior active row FIRST,
    -- SupersedeActiveReviewVerdictAcceptances) -- both used to write the
    -- IDENTICAL revoked_at/revoked_by shape otherwise (revoked_by is the
    -- SAME accepting user in the supersede case, since there is no
    -- separate revoker to name), which made a superseded row
    -- indistinguishable, on this row alone, from an explicit revocation
    -- that same person chose to make -- the audit log told the same lie:
    -- the ONLY row it wrote was review_verdict.accept by the new
    -- accepter, whose detail never named the superseded row at all, so
    -- the prior acceptance simply vanished from the trail. NULL while
    -- active (revoked_at IS NULL); one of the two literal values below,
    -- always set ALONGSIDE revoked_at, never independently -- see
    -- ReviewVerdictAcceptanceStore.Insert's own doc comment
    -- (postgres package) for how a caller tells the two apart, and
    -- httpapi.AcceptReviewVerdict for the now-distinct
    -- review_verdict.accept_supersedes_prior audit action this column's
    -- own 'superseded' value pairs with.
    revocation_reason TEXT CHECK (revocation_reason IN ('explicit', 'superseded'))
);

-- Enforces "at most one active acceptance per pull request" (finding F2,
-- adversarial review) as a database-level invariant, never merely an
-- application-level convention InsertReviewVerdictAcceptance could
-- forget to uphold on some future code path -- a second concurrent
-- Accept racing InsertReviewVerdictAcceptance's own supersede-then-insert
-- (queries/reviewverdictacceptances.sql) fails this constraint rather
-- than silently creating the second live row the rest of this table's
-- own doc comment above exists to rule out. ALSO serves
-- GetActiveReviewVerdictAcceptance's own WHERE repo_full_name = $1 AND
-- pr_number = $2 AND revoked_at IS NULL ORDER BY accepted_at DESC LIMIT 1
-- directly -- a SEPARATE, narrower review_verdict_acceptances_active_idx
-- (same leading columns, same partial WHERE) used to exist alongside this
-- one; removed (finding F7, adversarial review): this unique index alone
-- already guarantees at most one matching row, so that other index's own
-- trailing accepted_at DESC could never order anything, and a scratch
-- benchmark over 80k rows confirmed dropping it left
-- GetActiveReviewVerdictAcceptance on the identical plan with the
-- identical 3 shared buffers.
CREATE UNIQUE INDEX review_verdict_acceptances_one_active_idx
    ON review_verdict_acceptances (repo_full_name, pr_number)
    WHERE revoked_at IS NULL;

-- Serves ListReviewVerdictAcceptances -- the audit view, EVERY acceptance
-- (active or revoked) for one pull request, newest-first (finding F7,
-- adversarial review: that query has no caller anywhere yet, but a
-- future audit-view caller is the reason this table, and
-- RevokeReviewVerdictAcceptance/revocation_reason immediately above,
-- keep every row rather than deleting a superseded/revoked one). That
-- query's own WHERE repo_full_name = $1 AND pr_number = $2 carries no
-- revoked_at predicate, so it matches NEITHER partial index above --
-- measured on 80k rows before this index existed: a bare Seq Scan, 1124
-- shared buffers, Rows Removed by Filter: 79996. A full (non-partial)
-- index, covering every row regardless of revoked_at, is what this
-- query's own unfiltered WHERE clause actually needs -- an unindexed
-- query with a seq-scan plan is a trap set for whoever wires this view up
-- first.
CREATE INDEX review_verdict_acceptances_pr_idx
    ON review_verdict_acceptances (repo_full_name, pr_number, accepted_at DESC);

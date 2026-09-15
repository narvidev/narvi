-- review_verdicts.base_ref/base_sha/ancestor_chain/policy_version/
-- attempt_id (§21.1's amendment): the rest of what a verdict examined,
-- beyond head_sha (migrations/000067_review_verdicts.up.sql) -- forwarded
-- verbatim, at INSERT time, from the posting turn's own
-- turns.review_verdict_context (migrations/000129) and turns.id -- see
-- that migration's own doc comment for the full "why turn-scoped"
-- reasoning this table's own columns simply store the result of.
--
-- base_ref/base_sha are this PR's own immediate base at the moment the
-- verdict's own context was fetched -- nullable, exactly like head_sha's
-- own sibling columns already nullable elsewhere on this table
-- (review_path, counter_review, ...): NULL means either a pre-amendment
-- row (this column did not exist when it was written) or a review turn
-- whose own context-fetch could not resolve a base ref at all. Either
-- way, internal/domain/autoapproval.ComputeEligible treats a NULL/empty
-- base_ref as UNKNOWN, never as a match against the PR's current base --
-- "treating unknown context as matching would reopen the hole for every
-- verdict already stored" is this amendment's own explicit backfill
-- decision: an existing row with no recorded context can never satisfy
-- the new context-freshness check, and must be re-reviewed before it can
-- arm auto-approval again.
--
-- ancestor_chain is the ordered ancestor chain (internal/domain/review.
-- AncestorLink, JSON array of {ref, sha} objects) -- NOT NULL DEFAULT
-- '[]'::jsonb, mirroring blast_radius's own identical "always a present,
-- empty array, never an absent column" convention (migrations/000067's
-- own doc comment): the "unknown context" signal is carried entirely by
-- base_ref above, so this column does not need its own independent
-- nullability to express the same fact.
--
-- policy_version is the eligibility-policy revision in effect when this
-- verdict's own context was fetched (internal/domain/autoapproval.
-- CurrentPolicyVersion) -- NOT NULL DEFAULT 0: every pre-amendment row
-- reads back 0, which CurrentPolicyVersion (starting at 1) can never
-- equal by coincidence, so an old row fails the policy-version check
-- exactly like it fails the base-ref check, for the identical backfill
-- reason.
--
-- attempt_id (§21.1's amendment: "an attempt identifier distinct from
-- the context... two attempts over identical code share a context, and
-- exactly one of them may publish the current result, which a context
-- alone cannot express") is the turn that produced this verdict
-- (turns.id) -- nullable: NULL for a pre-amendment row, since no such
-- linkage was ever recorded and none can be honestly reconstructed after
-- the fact (a session spans many turns over a PR's whole life, so
-- session_id alone cannot stand in for it). ON DELETE SET NULL mirrors
-- session_id's own identical policy immediately above on this table: a
-- turn row being deleted (not a real operation today) must never cascade
-- into deleting verdict history.
ALTER TABLE review_verdicts
    ADD COLUMN base_ref TEXT,
    ADD COLUMN base_sha TEXT,
    ADD COLUMN ancestor_chain JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN policy_version INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN attempt_id UUID REFERENCES turns(id) ON DELETE SET NULL;

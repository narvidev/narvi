-- Queries backing ReviewVerdictStore (§21.1) -- see
-- migrations/000067_review_verdicts.up.sql's own doc comment for the
-- table's full append-only design.

-- name: InsertReviewVerdict :one
-- The ONE write this table ever accepts -- always an INSERT, never an
-- UPDATE (see the table's own doc comment for why). Called from
-- httpapi.PostReviewVerdict (reviewverdict.go), inside the SAME
-- transaction as that handler's existing review_findings upserts and
-- outbox write. digest_summary/digest_arch_decisions/digest_stack_risks/
-- digest_unverified_limits (§26.1, migrations/
-- 000077_review_verdicts_digest.up.sql) and digest_description_adequacy/
-- digest_adequacy_explanation/digest_proposed_body (§26.2,
-- migrations/000078_review_verdicts_description_adequacy.up.sql) forward
-- internal/domain/reviewpost.Digest verbatim -- see those migrations' own
-- doc comments for why all seven stay nullable at the schema level
-- despite digest_summary/digest_description_adequacy/
-- digest_adequacy_explanation being APPLICATION-required on every new
-- post.
--
-- review_path (§26.3, migrations/
-- 000081_review_verdicts_review_path.up.sql) forwards turns.review_depth
-- verbatim -- nullable, NULL for a verdict posted before this Step
-- existed, or whose own turn never had a resolvable depth (the SAME
-- "safe, not dangerous, degradation" posture head_sha's own resolution
-- already has, reviewverdict.go).
--
-- counter_review/fact_check/fact_check_killed/digest_contested_points
-- (§26.4/§26.6, migrations/
-- 000084_review_verdicts_counter_review.up.sql) forward internal/domain/
-- review.CounterReviewStatus, internal/domain/reviewpost.FactCheckStatus/
-- FactCheckKilled/Digest.ContestedPoints verbatim -- see that migration's
-- own doc comment for why all four stay nullable at the schema level
-- despite fact_check being APPLICATION-required, unconditionally, on
-- every new post (unlike counter_review, deep-path-only-required).
-- suppressed_in_shadow (migrations/
-- 000105_review_verdicts_shadow_epoch.up.sql, §30.8) is ALWAYS computed
-- by internal/app/reviewverdict.Insert itself from the SAME repoFullName
-- this row is being written for, never left to a caller -- see that
-- function's own doc comment for the resolution formula (egressmode.
-- Resolve, the identical single-authority resolver postgres.OutboxStore.
-- Create already uses for the outbox's own enqueue-time stamp).
--
-- arch_decision_tags/arch_decision_roots (§31.6, migrations/
-- 000113_review_verdicts_arch_decision_tags_roots.up.sql) forward
-- turns.review_depth_decision's own ArchDecisionTags/ArchDecisionRoots
-- verbatim -- see that migration's own doc comment for the full "why
-- this carrier, computed once at turn-creation time" reasoning. JSONB
-- arrays of plain strings, mirroring blast_radius's own identical shape.
--
-- knowledge_mode (migrations/
-- 000115_review_verdicts_knowledge_mode.up.sql, §31.2 item 2) forwards
-- turns.review_knowledge_mode verbatim -- nullable, mirroring
-- review_path's own identical "NULL means posted before this Step, or no
-- resolvable value" degradation.
--
-- knowledge_influenced (migrations/
-- 000117_review_verdicts_knowledge_influenced.up.sql, §31.7's own G5) is
-- computed by the caller from THIS verdict's own posting turn's
-- turns.review_knowledge_decision (whether that record carries at least
-- one injected id) -- never left nullable: every row from this Step
-- forward gets a real, computed false-or-true answer, never an absent
-- one (see that migration's own doc comment for the full "why NOT NULL
-- DEFAULT false is safe here" reasoning, unlike knowledge_mode/
-- review_path immediately above).
INSERT INTO review_verdicts (
    repo_full_name, pr_number, head_sha,
    risk_level, premise, blast_radius, files_changed, tests_coverage, docs_drift,
    proposed_shippable, shippable, session_id,
    digest_summary, digest_arch_decisions, digest_stack_risks, digest_unverified_limits,
    digest_description_adequacy, digest_adequacy_explanation, digest_proposed_body,
    review_path,
    counter_review, fact_check, fact_check_killed, digest_contested_points,
    suppressed_in_shadow,
    arch_decision_tags, arch_decision_roots,
    knowledge_mode, knowledge_influenced
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
RETURNING *;

-- name: GetLatestReviewVerdict :one
-- The DISTINCT ON (repo, pr_number) ... ORDER BY created_at DESC
-- reduction §21.1 specifies -- scoped here to ONE (repo_full_name,
-- pr_number) pair (the one shape every real caller -- the auto-approval
-- eligibility engine, the decision inbox's own classification, the
-- revalidate-at-click/at-merge paths -- actually needs), so this is a
-- plain indexed lookup ORDER BY created_at DESC LIMIT 1, not a
-- multi-row DISTINCT ON scan -- see ListLatestAutoApprovedInRepo below
-- for the multi-PR, per-repo shape that DOES need real DISTINCT ON.
-- pgx.ErrNoRows means no verdict has ever been posted for this PR.
--
-- Deliberately UNFILTERED by suppressed_in_shadow: §30.6 is explicit
-- that review_verdicts "render in Narvi's own UI with zero new work",
-- and this is the shared read every internal, operator-facing caller
-- uses (the auto-approval eligibility engine, the decision inbox's own
-- classification, the revalidate-at-click/at-merge paths) -- excluding
-- shadow-era rows here would hide the very evaluation data those
-- surfaces exist to show an operator. GetLatestNonShadowReviewVerdict
-- below is the customer-consequential sibling this query is NOT: use
-- that one instead for anything that could arm a real, customer-visible
-- effect (§30.8: "never call-site checks").
SELECT * FROM review_verdicts
WHERE repo_full_name = $1 AND pr_number = $2
ORDER BY created_at DESC
LIMIT 1;

-- name: GetLatestNonShadowReviewVerdict :one
-- §30.8's own customer-consequential sibling of GetLatestReviewVerdict
-- above: the SAME per-PR latest-verdict reduction, but excluding any
-- verdict whose own suppressed_in_shadow stamp is true OR that predates
-- this repo's own live_egress_promoted_at fence (belt and suspenders --
-- see migrations/000104_repo_settings_live_egress_promoted_at.up.sql's
-- own doc comment for why both checks are independent, not redundant).
-- internal/app/sessionactor/reviewretrigger.go's own auto-retrigger
-- decision is this query's one caller: a shadow-era "already reviewed"
-- fact must never suppress a REAL re-review once a repo goes live, and
-- a shadow-era risk level must never be quoted in a real, customer-
-- visible budget-exhausted notice (§30.8: "the same stamp gates
-- re-trigger"). pgx.ErrNoRows means no NON-SHADOW verdict has ever been
-- posted for this PR -- indistinguishable, by design, from "no verdict
-- at all" to this query's one caller, which already treats that outcome
-- as "nothing to compare against yet".
SELECT * FROM review_verdicts rv
WHERE rv.repo_full_name = $1 AND rv.pr_number = $2
    AND NOT rv.suppressed_in_shadow
    AND rv.created_at > COALESCE(
        (SELECT rs.live_egress_promoted_at FROM repo_settings rs WHERE rs.repo_full_name = $1),
        'infinity'::timestamptz)
ORDER BY rv.created_at DESC
LIMIT 1;

-- name: ListLatestAutoApprovedInRepo :many
-- internal/app/automerge's own discovery query (§21.2 stage 2): every
-- open-as-of-last-review-candidate PR in repoFullName whose LATEST
-- verdict, within the bounded window, is Shippable == 'auto' -- the real
-- multi-row DISTINCT ON (repo, pr_number) ... ORDER BY created_at DESC
-- reduction §21.1 names, THEN filtered to shippable = 'auto' in an outer
-- query (DISTINCT ON's own "first row per group" pick must be decided by
-- created_at alone, before shippable can be tested, so the filter cannot
-- fold into the same SELECT's own WHERE clause). This is a DISCOVERY
-- aid only, bounded and cheap (no GitHub call) -- internal/app/
-- decisioninbox.RevalidateForAutoMerge is what actually re-confirms each
-- candidate live before anything merges (§21.2: "reuses the decision
-- inbox's existing server-side re-validation-at-click contract
-- unchanged"), so a candidate this query returns that has since gone
-- stale (a new commit landed, CI flipped) is simply rejected there, never
-- trusted as authority here.
--
-- §30.8: "Shadow-era verdicts must never arm auto-merge after
-- promotion... every review_verdicts row is stamped with its egress
-- mode at write time and the exclusion lives in the query, never at
-- call sites; promotion additionally sets a fence." Both checks below
-- are independent, deliberate redundancy (migrations/
-- 000104_repo_settings_live_egress_promoted_at.up.sql's own doc
-- comment): a bug in the per-row stamp alone must not be the only thing
-- standing between a shadow-era verdict and a real merge. The fence
-- join is scoped to the SAME repo_full_name this whole query is already
-- scoped to ($1), so it costs one extra indexed lookup, not a
-- correlated subquery per candidate row.
SELECT * FROM (
    SELECT DISTINCT ON (rv.repo_full_name, rv.pr_number) rv.*
    FROM review_verdicts rv
    WHERE rv.repo_full_name = $1 AND rv.created_at > $2
        AND NOT rv.suppressed_in_shadow
        AND rv.created_at > COALESCE(
            (SELECT rs.live_egress_promoted_at FROM repo_settings rs WHERE rs.repo_full_name = $1),
            'infinity'::timestamptz)
    ORDER BY rv.repo_full_name, rv.pr_number, rv.created_at DESC
) latest
WHERE shippable = 'auto'
ORDER BY created_at ASC
LIMIT $3;

-- name: ListReviewVerdictsForPR :many
-- §26.1 item 5's own merge-readout "History" rail (§12.2 item 2): every
-- verdict ever posted for ONE (repo_full_name, pr_number), newest first,
-- bounded by limit -- the SAME "bounded from day one" discipline §21.1
-- requires of every query against this table (ListReviewVerdictsInWindow
-- below is the repo-wide analytics sibling; this is the PR-scoped one no
-- existing caller needed before now).
SELECT * FROM review_verdicts
WHERE repo_full_name = $1 AND pr_number = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: ListReviewVerdictsInWindow :many
-- The analytics rollups' own shared bounded scan (§21.1: "every query
-- against this history is bounded from day one") -- every verdict for
-- repoFullName posted after sinceTime, oldest first. internal/app/
-- reviewverdict's own Timeseries/TopRiskDrivers functions both reduce
-- this SAME result set in memory (a pure, already-fetched-data
-- transform, mirroring internal/domain/decisioninbox.MedianLatency's own
-- "caller fetches, pure package reduces" split) rather than each issuing
-- its own bespoke aggregate SQL query.
SELECT * FROM review_verdicts
WHERE repo_full_name = $1 AND created_at > $2
ORDER BY created_at ASC
LIMIT $3;

-- name: ListNonShadowReviewVerdictsInWindow :many
-- §30.8's own customer-consequential sibling of
-- ListReviewVerdictsInWindow above: internal/app/digest's own daily
-- rollup (§21.3) is the ONE caller that needs this exclusion --
-- Timeseries/TopRiskDrivers above stay on the unfiltered query
-- deliberately (§30.6: shadow verdicts "render in Narvi's own UI with
-- zero new work", and those two feed exactly that internal, operator-
-- facing analytics surface, never a customer's own channel). §30.8's
-- own words: "a daily digest rollup would otherwise reveal phantom
-- reviews to the customer's channels." Same suppressed_in_shadow +
-- live_egress_promoted_at fence as ListLatestAutoApprovedInRepo/
-- GetLatestNonShadowReviewVerdict above -- see
-- migrations/000104_repo_settings_live_egress_promoted_at.up.sql's own
-- doc comment for why both checks are independent.
SELECT rv.* FROM review_verdicts rv
WHERE rv.repo_full_name = $1 AND rv.created_at > $2
    AND NOT rv.suppressed_in_shadow
    AND rv.created_at > COALESCE(
        (SELECT rs.live_egress_promoted_at FROM repo_settings rs WHERE rs.repo_full_name = $1),
        'infinity'::timestamptz)
ORDER BY rv.created_at ASC
LIMIT $3;

-- name: ListGatedArchDecisions :many
-- The knowledge-retrieval GATE itself (§31.6) -- the candidate set BOTH
-- mode A and (a later Step's) mode B share, owned by this Step:
-- "decisions from verdicts whose tags/directory-roots -- stamped at
-- INSERT time from ClassifyChangedPaths(prCtx.ChangedPaths), never from
-- the posted blast_radius column -- overlap the current PR's own freshly
-- computed tags/roots ... ORDER BY created_at DESC LIMIT k". Deliberately
-- never reads blast_radius: that column is the reviewing model's own
-- self-report (§21.2: "these ... are computed from the SERVER's own view
-- of the diff -- never from the verdict"), and keying the gate on it
-- would let an attacker who induces a false ArchDecision also choose, in
-- the SAME verdict call, the blast_radius tags that later decide where
-- that false decision surfaces.
--
-- Mode-invariant: mode A orders these rows by recency (this query's own
-- ORDER BY, unchanged); mode B (a later Step) re-ranks the SAME gated
-- candidates -- it never re-selects them.
--
-- The overlap is tags OR roots, never AND: §31.7's own reading of the
-- gate's recall loss names sharing NEITHER as the miss case ("a PR that
-- shares no paths, and hence no tags OR roots, with the current one"),
-- which is the same as saying sharing EITHER is enough to be a
-- candidate. jsonb's own `?|` operator ("do any of the strings in the
-- text array exist as top-level array elements") is exactly this
-- membership test over arch_decision_tags/arch_decision_roots' own
-- "plain JSON array of strings" shape (migrations/000113's own doc
-- comment). An empty sqlc.arg(tags)/sqlc.arg(roots) makes `?|`
-- vacuously false for every row (there is nothing to overlap) -- which
-- is what correctly routes an all-empty current-PR classification
-- straight to the recency fallback below, rather than matching
-- everything or nothing by accident.
--
-- Two exclusions, both in the SQL, never at call sites (§30.8's own
-- discipline, extended here): NOT suppressed_in_shadow excludes every
-- shadow-epoch verdict via the egress-mode stamp §30.8 already puts on
-- every verdict row (no new stamp introduced here); the NOT
-- EXISTS excludes every verdict whose PR has EVER had its arch-recap
-- contested (review_digest_section_feedback, migration 000086) --
-- scoped to (repo_full_name, pr_number, section), coarser than that
-- table's own per-content-hash identity (§26.5's own per-verdict
-- ComputeDigestSectionIdentity hash): recomputing that sha256
-- canonicalization a second time, in SQL, would be a second
-- implementation of the identical normalization that could silently
-- drift from the Go one it must always match (§31.7 already documents
-- the contestation hash as "NOT an anti-poisoning control" and
-- "paraphrase-fragile" even at its own native granularity). Per-PR is
-- the conservative direction: it can only ever exclude MORE than a
-- content-hash-exact match would, never admit a contested decision a
-- finer-grained check would have caught.
SELECT rv.* FROM review_verdicts rv
WHERE rv.repo_full_name = $1
    AND rv.digest_arch_decisions IS NOT NULL
    AND jsonb_array_length(rv.digest_arch_decisions) > 0
    AND NOT rv.suppressed_in_shadow
    AND (rv.arch_decision_tags ?| sqlc.arg(tags)::text[] OR rv.arch_decision_roots ?| sqlc.arg(roots)::text[])
    AND NOT EXISTS (
        SELECT 1 FROM review_digest_section_feedback f
        WHERE f.repo_full_name = rv.repo_full_name
          AND f.pr_number = rv.pr_number
          AND f.section = 'arch_recap'
    )
ORDER BY rv.created_at DESC
LIMIT sqlc.arg(result_limit);

-- name: ListRecentArchDecisions :many
-- The gate's own RECENCY FALLBACK (§31.6) -- the IDENTICAL two
-- exclusions as ListGatedArchDecisions above (shadow-epoch, contested),
-- with NO tag/root predicate at all. Still THE GATE, not an escape hatch
-- from it: "That overlap predicate -- including its recency fallback --
-- is the GATE, and it is mode-invariant." Called only when
-- ListGatedArchDecisions returns zero rows
-- (internal/app/reviewcontext.FetchPriorArchDecisions's own sequencing);
-- when mode B's own fallback fires it serves this SAME recency-ordered
-- window, unranked -- "a pure-recency set gives a ranker nothing
-- legitimate to exploit".
SELECT rv.* FROM review_verdicts rv
WHERE rv.repo_full_name = $1
    AND rv.digest_arch_decisions IS NOT NULL
    AND jsonb_array_length(rv.digest_arch_decisions) > 0
    AND NOT rv.suppressed_in_shadow
    AND NOT EXISTS (
        SELECT 1 FROM review_digest_section_feedback f
        WHERE f.repo_full_name = rv.repo_full_name
          AND f.pr_number = rv.pr_number
          AND f.section = 'arch_recap'
    )
ORDER BY rv.created_at DESC
LIMIT sqlc.arg(result_limit);

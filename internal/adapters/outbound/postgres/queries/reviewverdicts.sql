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
-- base_ref/base_sha/ancestor_chain/policy_version/attempt_id (§21.1's
-- amendment, migrations/000130_review_verdicts_context.up.sql) forward
-- the posting turn's own turns.review_verdict_context (unmarshaled by
-- the caller, internal/app/reviewverdict.Insert) plus that turn's own id
-- verbatim -- see that migration's own doc comment for the full nullable/
-- default shape and the backfill reasoning for a pre-amendment row.
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
    knowledge_mode, knowledge_influenced,
    base_ref, base_sha, ancestor_chain, policy_version, attempt_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34)
RETURNING *;

-- name: GetLatestReviewVerdict :one
-- The DISTINCT ON (repo, pr_number) ... reduction §21.1 specifies --
-- scoped here to ONE (repo_full_name, pr_number) pair (the one shape
-- every real caller -- the auto-approval eligibility engine, the
-- decision inbox's own classification, the revalidate-at-click/at-merge
-- paths -- actually needs), so this is a plain indexed lookup LIMIT 1,
-- not a multi-row DISTINCT ON scan -- see ListLatestAutoApprovedInRepo
-- below for the multi-PR, per-repo shape that DOES need real DISTINCT
-- ON. pgx.ErrNoRows means no verdict has ever been posted for this PR.
--
-- Ordered by the PRODUCING ATTEMPT's own creation time (turns.created_at,
-- joined via attempt_id) -- NEVER by review_verdicts.created_at alone
-- (round-10 finding D: "the authoritative verdict is whichever request
-- landed last, not the one for the current attempt", violating §21.1b's
-- "an emission carries the attempt and context it was produced for").
-- Two attempts can be in flight for one PR (§21.1b: "the record a
-- publisher is about to emit may already be superseded by the time it
-- emits"), and an OLDER attempt's own POST reaching this table AFTER a
-- NEWER attempt's must not win this read merely because its own INSERT
-- happened to commit later in wall-clock time -- this is a read-side
-- fix only, distinct from (and no substitute for) the write-side
-- refusal-on-supersession §21.1b describes and a later, unshipped GitHub
-- result publisher (§8.2, §21.1) implements against a real external
-- result. attempt_id is nullable (a
-- pre-amendment row recorded none): the LEFT JOIN's own unmatched NULL
-- falls through COALESCE to rv.created_at, preserving today's exact
-- ordering for any row that predates this column existing at all.
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
--
-- Round-11 finding B: the ORDER BY above had no tie-breaker at all --
-- two verdicts sharing the identical COALESCE(t.created_at,
-- rv.created_at) value (the common case being two attempts sharing ONE
-- producing turn, so t.created_at is literally the SAME value for both
-- rows) resolved arbitrarily, by whatever physical row order Postgres
-- happened to return them in -- not merely non-deterministic across
-- runs, but potentially DIFFERENT from what ListLatestAutoApprovedInRepo
-- below picks for the identical pair, since that query (before this fix)
-- ran a DIFFERENT reduction entirely. Two further keys make this fully
-- deterministic and, just as importantly, IDENTICAL to every other
-- per-PR "latest" reduction this table has (this file's own three:
-- GetLatestNonShadowReviewVerdict immediately below, and
-- ListLatestAutoApprovedInRepo's own inner DISTINCT ON, further down --
-- searched for; there is no fourth): rv.created_at DESC breaks a tie on
-- the coarser attempt-time key using the finer, always-populated
-- row-level post time (meaningful for two attempts that DO share a
-- producing turn, or for the pre-amendment rows where attempt_id is
-- NULL and COALESCE already fell through to rv.created_at, in which case
-- this second key is now redundant with the first but harmless); rv.id
-- DESC is the final, purely-mechanical tie-break for the residual case
-- of two rows sharing BOTH timestamps exactly (same producing turn,
-- same wall-clock instant) -- id is a random UUID
-- (migrations/000067_review_verdicts.up.sql), so this key carries no
-- temporal meaning of its own; it exists solely to make the pick
-- REPRODUCIBLE (the same query against the same data always resolves
-- the SAME winning row) rather than to express which row actually is
-- newer.
SELECT rv.* FROM review_verdicts rv
LEFT JOIN turns t ON t.id = rv.attempt_id
WHERE rv.repo_full_name = $1 AND rv.pr_number = $2
ORDER BY COALESCE(t.created_at, rv.created_at) DESC, rv.created_at DESC, rv.id DESC
LIMIT 1;

-- name: GetLatestNonShadowReviewVerdict :one
-- §30.8's own customer-consequential sibling of GetLatestReviewVerdict
-- above: the SAME per-PR latest-verdict reduction -- including the
-- IDENTICAL attempt-ordering fix immediately above this query's own doc
-- comment (round-10 finding D) and the IDENTICAL deterministic
-- tie-breaker (round-11 finding B) -- for the same reason: this query is
-- exactly as reachable by two in-flight attempts, and by two rows tied
-- on COALESCE(t.created_at, rv.created_at), as its sibling is --
-- excluding any verdict whose own suppressed_in_shadow stamp is true OR
-- that predates this repo's own live_egress_promoted_at fence (belt and
-- suspenders -- see migrations/000104_repo_settings_live_egress_promoted_at.up.sql's
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
SELECT rv.* FROM review_verdicts rv
LEFT JOIN turns t ON t.id = rv.attempt_id
WHERE rv.repo_full_name = $1 AND rv.pr_number = $2
    AND NOT rv.suppressed_in_shadow
    AND rv.created_at > COALESCE(
        (SELECT rs.live_egress_promoted_at FROM repo_settings rs WHERE rs.repo_full_name = $1),
        'infinity'::timestamptz)
ORDER BY COALESCE(t.created_at, rv.created_at) DESC, rv.created_at DESC, rv.id DESC
LIMIT 1;

-- name: ListLatestAutoApprovedInRepo :many
-- internal/app/automerge's own discovery query (§21.2 stage 2): every
-- open-as-of-last-review-candidate PR in repoFullName whose LATEST
-- verdict, within the bounded window, is Shippable == 'auto' -- the real
-- multi-row DISTINCT ON (repo, pr_number) ... ORDER BY reduction §21.1
-- names, THEN filtered to shippable = 'auto' in an outer query (DISTINCT
-- ON's own "first row per group" pick must be decided by its own
-- ordering column, before shippable can be tested, so the filter cannot
-- fold into the same SELECT's own WHERE clause).
--
-- D6 (round-12 sweep): this paragraph previously named that ordering
-- column "created_at alone" -- true before Round-11 finding B, below,
-- and contradicted by that SAME finding's own fix ever since: the inner
-- DISTINCT ON's actual ORDER BY key is COALESCE(t.created_at,
-- rv.created_at), never rv.created_at by itself. See that finding's own
-- paragraph for why (the producing attempt's own creation time, when
-- one exists, must decide the pick, exactly like GetLatestReviewVerdict/
-- GetLatestNonShadowReviewVerdict above already do).
--
-- This is a DISCOVERY
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
--
-- Round-11 finding B: the inner DISTINCT ON below used to reduce
-- "latest" by rv.created_at (post time) ALONE -- a DIFFERENT reduction
-- than GetLatestReviewVerdict/GetLatestNonShadowReviewVerdict above,
-- which order by the PRODUCING ATTEMPT's own creation time (round-10
-- finding D). Two components deciding about the SAME pull request from
-- two DIFFERENT "latest" verdicts is exactly the failure this closes:
-- this query is what actually ARMS auto-merge (internal/app/automerge's
-- own discovery query), and it must agree, row for row, with whichever
-- verdict the decision inbox and the eligibility engine's own callers
-- (GetLatestReviewVerdict) would call "latest" for the identical PR, or
-- the two can disagree about which verdict is authoritative for the same
-- code. The fix: the IDENTICAL LEFT JOIN turns / COALESCE(t.created_at,
-- rv.created_at) ordering, with the IDENTICAL deterministic tie-breakers
-- (rv.created_at DESC, then rv.id DESC) -- see
-- GetLatestReviewVerdict's own doc comment for the full "why" of each.
-- DISTINCT ON's own requirement that its ORDER BY start with the
-- DISTINCT ON columns is unchanged: rv.repo_full_name, rv.pr_number
-- still lead.
SELECT * FROM (
    SELECT DISTINCT ON (rv.repo_full_name, rv.pr_number) rv.*
    FROM review_verdicts rv
    LEFT JOIN turns t ON t.id = rv.attempt_id
    WHERE rv.repo_full_name = $1 AND rv.created_at > $2
        AND NOT rv.suppressed_in_shadow
        AND rv.created_at > COALESCE(
            (SELECT rs.live_egress_promoted_at FROM repo_settings rs WHERE rs.repo_full_name = $1),
            'infinity'::timestamptz)
    ORDER BY rv.repo_full_name, rv.pr_number, COALESCE(t.created_at, rv.created_at) DESC, rv.created_at DESC, rv.id DESC
) latest
WHERE shippable = 'auto'
ORDER BY created_at ASC, id ASC
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

-- Excludes the PR under review (rv.pr_number <> exclude_pr). This is
-- "prior decisions from this repository", and a PR's own earlier verdict
-- is not that: it is the same review's first pass. Without the exclusion
-- it is not merely POSSIBLE but the single most likely match, because a
-- re-review computes its tags and roots from the same changed paths that
-- stamped that verdict -- so the overlap is near-certain and recency puts
-- it first. Two things go wrong then: a re-review, whose whole purpose is
-- to reconsider after a push, is handed its own first-pass conclusions and
-- biased toward agreeing with itself; and the verdict it produces is
-- stamped knowledge-influenced on pure self-reference, which is exactly
-- the population a later Step would ingest and the phase KPI joins
-- contestation against.
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
    AND rv.pr_number <> sqlc.arg(exclude_pr)
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

-- Excludes the PR under review (rv.pr_number <> exclude_pr). This is
-- "prior decisions from this repository", and a PR's own earlier verdict
-- is not that: it is the same review's first pass. Without the exclusion
-- it is not merely POSSIBLE but the single most likely match, because a
-- re-review computes its tags and roots from the same changed paths that
-- stamped that verdict -- so the overlap is near-certain and recency puts
-- it first. Two things go wrong then: a re-review, whose whole purpose is
-- to reconsider after a push, is handed its own first-pass conclusions and
-- biased toward agreeing with itself; and the verdict it produces is
-- stamped knowledge-influenced on pure self-reference, which is exactly
-- the population a later Step would ingest and the phase KPI joins
-- contestation against.
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
    AND rv.pr_number <> sqlc.arg(exclude_pr)
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

-- name: ExistsReviewVerdictForAttempt :one
-- The review's own GitHub-native result surface (§8.2/§21.1/§21.1b)
-- read: has ANY review_verdicts row ever been posted
-- for attemptID (turns.id)? sessionactor's own outboxenqueue.go calls
-- this at turn-completion time to decide whether the review-check
-- publisher's PhaseTerminalNotAssessed emission is warranted -- "a
-- review that did not complete" (decision 1) means exactly this: the
-- ONE attempt that just reached a terminal turn state never posted a
-- verdict through the verdict-posting tool (httpapi.PostReviewVerdict,
-- the ONLY sanctioned path, §8.2's own RAW-COMMENT BLOCKING). Scoped to
-- attempt_id specifically, never repo_full_name/pr_number alone: a PRIOR
-- attempt's own verdict must never be read as evidence THIS attempt
-- completed (the identical "an emission carries the attempt... it was
-- produced for" discipline §21.1b states for the publisher itself).
SELECT EXISTS(
    SELECT 1 FROM review_verdicts WHERE attempt_id = $1
) AS verdict_exists;

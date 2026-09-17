-- review_verdicts (Step 62, §21.1): append-only history of every POSTED
-- review verdict (internal/domain/review's own structured Verdict type,
-- Step 45/§8.2) -- one row per POST /sessions/{id}/review/verdict call
-- (reviewverdict.go, Step 47), never an update-in-place. Pure storage:
-- every column below is forwarded verbatim from a value already computed
-- server-side (review.ComputeShippable et al.) by the time this table's
-- own INSERT runs -- nothing here re-parses posted comment text, and
-- nothing here re-derives a value this table itself could instead trust
-- from its own writer.
--
-- Why append-only, not a second "current verdict" table kept in sync by
-- convention: a PR can be re-reviewed (a new commit, a manual re-trigger,
-- §24's later automatic re-review) any number of times, and every one of
-- those verdicts is a real, dated fact worth keeping -- an update-in-place
-- table would destroy the very history §21.1's analytics rollups
-- (timeseries, top-risk-driver breakdown) read from. The LATEST verdict
-- per PR is instead a read-time reduction (queries/reviewverdicts.sql's
-- own GetLatestReviewVerdict, scoped to one (repo_full_name, pr_number)
-- pair; ListLatestAutoApprovedInRepo, same file, is the real multi-PR
-- DISTINCT ON (repo_full_name, pr_number) reduction) -- no second table,
-- no triggers, no convention a future writer could forget to honor.
--
-- head_sha (§21.1's own emphatic requirement) is the commit this verdict
-- was actually produced against -- the SAME SHA the review session's own
-- pre-fetched diff (internal/app/reviewcontext.Fetch, Step 46) was already
-- anchored to. This was originally forwarded from github_pr_sessions.
-- pending_head_sha (migrations/000068), itself populated at context-fetch
-- time by every review-trigger ingress path; migration
-- 000072_turns_review_head_sha.up.sql later dropped that column outright
-- and moved this same fact onto turns.review_head_sha instead (see that
-- migration's own doc comment for the full "why a shared, mutable
-- per-(repo,PR) column was the wrong place for this fact") -- head_sha on
-- THIS table is unaffected either way: never re-derived here, and never
-- asked of the reviewing agent (which has no reliable way to self-report
-- it). NOT NULL: a
-- verdict with no known head SHA cannot honestly participate in the
-- auto-approval eligibility engine's own stale-verdict guard (§21.2) at
-- all, so this table refuses to record one rather than silently treating
-- an unknown SHA as fresh OR as permanently stale -- reviewverdict.Insert
-- (internal/app/reviewverdict) is the one caller, and it fails the whole
-- verdict-persistence write (logged, never blocking the verdict POST
-- itself, mirroring reviewverdict.go's own existing "policy nuance, not a
-- precondition" posture for a degraded repo-settings read) whenever no
-- head SHA could be resolved, rather than inserting a row this table's
-- own schema would otherwise have to treat as a special, load-bearing NULL
-- case forever.
--
-- Stacked PRs (§21.1): this table stores exactly what a Verdict already
-- covers -- the diff against this PR's OWN immediate base, never the
-- cumulative stack diff (internal/domain/review/context.go's own
-- StackContext doc comment). Position/size/ultimate-base are review
-- CONTEXT only and are never persisted here -- storing them would invite a
-- future reader to mistake this table for something it is not: a record
-- of what a multi-PR stack's own composed diff looked like.
CREATE TABLE review_verdicts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_full_name TEXT NOT NULL,
    pr_number      INTEGER NOT NULL,
    head_sha       TEXT NOT NULL,

    -- One column per internal/domain/review.Verdict field, verbatim --
    -- see that package's own verdict.go for the authoritative field-by-
    -- field doc comments this table only ever forwards, never
    -- reinterprets. blast_radius is JSONB (a plain JSON array of tag
    -- strings), mirroring sessions.repos' own existing JSON-array-column
    -- precedent (queries/sessions.sql) rather than a native Postgres
    -- array type, which has no precedent anywhere else in this schema.
    risk_level         TEXT NOT NULL,
    premise            TEXT NOT NULL,
    blast_radius       JSONB NOT NULL DEFAULT '[]'::jsonb,
    files_changed      INTEGER NOT NULL,
    tests_coverage     TEXT NOT NULL,
    docs_drift         TEXT NOT NULL,
    proposed_shippable TEXT NOT NULL,
    -- shippable is the AUTHORITATIVE, server-computed classification
    -- (review.ComputeShippable's own return value) -- the one field the
    -- auto-approval eligibility engine (internal/domain/autoapproval,
    -- §21.2) gates on first, before any of its other criteria.
    shippable          TEXT NOT NULL,

    -- session_id is nullable and carried for traceability/debugging only
    -- (which session produced this verdict) -- ON DELETE SET NULL: a
    -- session row being deleted (not a real operation today) must never
    -- cascade into deleting verdict HISTORY, which is the one thing this
    -- whole table exists to make durable independent of the session that
    -- produced it.
    session_id     UUID REFERENCES sessions(id) ON DELETE SET NULL,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- D5 (round-12 sweep): this comment previously claimed the (repo_full_name,
-- pr_number, created_at DESC) shape below "serves a per-PR DISTINCT ON
-- directly", for BOTH queries/reviewverdicts.sql's GetLatestReviewVerdict
-- (a single-PR lookup, not actually a DISTINCT ON at all -- that query's
-- own doc comment already says so) and ListLatestAutoApprovedInRepo (the
-- real multi-PR DISTINCT ON). Round-11 finding B changed what "latest"
-- means for both queries, from a plain rv.created_at ORDER BY to
-- COALESCE(t.created_at, rv.created_at) -- a value that reaches through a
-- LEFT JOIN to turns -- and this comment was never revisited to check
-- whether the claim still held. It does not. Verified by running EXPLAIN
-- (ANALYZE, BUFFERS) against a real Postgres 17 instance (round-12 sweep),
-- at both a small seed and a stress-tested 2,000-PR/6,000-row single repo:
--   - GetLatestReviewVerdict DOES use this index, but ONLY for the
--     equality filter (repo_full_name = $1 AND pr_number = $2) -- the
--     handful of matching rows are then joined to turns and explicitly
--     re-sorted in memory on the COALESCE expression; this index's own
--     trailing created_at DESC column plays no role in that sort.
--   - ListLatestAutoApprovedInRepo never touches this index at all. The
--     planner instead uses the SIBLING review_verdicts_repo_created_idx
--     below for its WHERE filter, then an explicit Sort + Unique, entirely
--     in memory, for the DISTINCT ON itself.
-- No plain btree index on review_verdicts alone can back an ORDER BY that
-- reaches through a join to turns -- that would need either a denormalized
-- "effective latest timestamp" column kept in sync at write time, or a
-- materialized view, neither of which is justified by what EXPLAIN
-- actually measured: both queries planned and executed in well under a
-- millisecond at 2,000 PRs in one repo (Sort cost ~628, actual ~0.1ms).
-- Not added -- no measured cost justifies the added write-path complexity
-- yet; revisit if a real repo's own bounded-window verdict volume is ever
-- shown, by EXPLAIN, to make that Sort a measurable cost. This index is
-- kept regardless: it genuinely backs the equality half of both queries'
-- own per-PR/per-repo filters, which is what the paragraph below actually
-- describes.
CREATE INDEX review_verdicts_repo_pr_created_idx ON review_verdicts (repo_full_name, pr_number, created_at DESC);

-- A second, repo-only-leading index for a pure "every verdict for this
-- repo in the last N days, across every PR" scan (the analytics
-- timeseries/top-risk-driver rollups, and the digest's own per-channel
-- rollup) -- repo_full_name alone as the leading column, unlike the index
-- above, whose leading pair is (repo_full_name, pr_number).
CREATE INDEX review_verdicts_repo_created_idx ON review_verdicts (repo_full_name, created_at DESC);

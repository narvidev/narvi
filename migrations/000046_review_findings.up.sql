-- review_findings: Step 48's own resolution of the "Tension 1" the plan
-- itself leaves open (internal/domain/review/doc.go's own design call #4,
-- and §21.1/§22.1's own text, which describe per-finding persisted
-- content as though Step 45's structured verdict type "already carries"
-- it -- it does not; see internal/domain/reviewpost/finding.go's own top
-- doc comment for the full resolution).
--
-- Deliberately its OWN table, not a column/JSON blob on review_verdicts:
-- review_verdicts (Step 62, does not exist yet at this point in the plan's
-- own sequence -- see this Step's own PR description) is explicitly
-- append-only, one row per POST -- the wrong shape for a finding, which
-- needs MUTABLE status (open -> rebutted / fix_pending / fix_open /
-- fix_merged / fix_applied) that persists ACROSS re-posted verdicts, the
-- opposite of append-only. This table is designed so Step 62 can later
-- JOIN review_verdicts against it, and so Step 63's own learned-pattern
-- table (a DIFFERENT, repo-scoped table of maintainer-taught false-
-- positive TEXT patterns, out of this Step's own scope) can sit beside it
-- -- forward-compatible, required by neither.
--
-- (repo_full_name, pr_number, identity_hash) is the natural key: one row
-- per UNIQUE finding identity per PR, upserted on every verdict post that
-- re-reports the same identity -- the exact "INSERT ... ON CONFLICT
-- atomic claim" idiom github_pr_sessions (migrations/000028) and
-- repo_settings (migrations/000044) already establish, reused rather than
-- invented. identity_hash is internal/domain/reviewpost.
-- ComputeFindingIdentity's own sha256 hex digest (64 hex chars) -- a
-- SERVER-computed value, never client-supplied (see that function's own
-- doc comment) -- computed over (sentinel_kind or "general", normalized
-- file_path, normalized description), deliberately NOT file:line, so a
-- finding re-reported at a SHIFTED line number is still recognized as the
-- SAME finding (§22.1).
--
-- last_seen_head_sha/last_seen_at are updated on EVERY upsert (a finding
-- re-reported on a later commit); status/rebuttal_text/rebutted_by/
-- rebutted_at are PRESERVED across an upsert that only re-reports the
-- SAME identity -- see reviewfindings.sql's own UpsertReviewFinding query
-- for the exact ON CONFLICT clause that makes this true.
CREATE TABLE review_findings (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_full_name    TEXT NOT NULL,
    pr_number         INTEGER NOT NULL,
    identity_hash     TEXT NOT NULL,

    -- sentinel_kind is NULL for an ordinary (non-sentinel) risk-map
    -- finding -- internal/domain/reviewpost.SentinelKind's own two values
    -- ("coverage"/"docs_drift") otherwise. Not a Postgres ENUM: this
    -- column's own closed vocabulary lives in Go (reviewpost.
    -- ValidateFindingInput), matching this codebase's OWN established
    -- precedent of validating a closed string vocabulary at the
    -- application layer rather than the schema layer whenever a sibling
    -- Go package is already the source of truth for it (e.g.
    -- UpdateMemberRoleRequest.role, contracts/rest/v1/dtos.schema.json's
    -- own doc comment).
    sentinel_kind     TEXT,
    severity          TEXT NOT NULL,
    file_path         TEXT NOT NULL,
    -- line is informational only -- NEVER part of identity_hash (see that
    -- column's own doc comment above) -- nullable (a file-level finding
    -- with no specific line).
    line              INTEGER,
    description       TEXT NOT NULL,
    suggested_fix     TEXT,

    -- status is the finding's own MUTABLE lifecycle
    -- (reviewpost.FindingStatus's five values) -- 'open' is every
    -- finding's own starting state; the whole reason this is its own
    -- table rather than a column on the append-only review_verdicts
    -- history (this migration's own top doc comment).
    status            TEXT NOT NULL DEFAULT 'open',
    rebuttal_text     TEXT,
    rebutted_by       UUID REFERENCES users(id) ON DELETE SET NULL,
    rebutted_at       TIMESTAMPTZ,

    -- sentinel-auto-fix linkage (§17.3: "the original verdict is updated
    -- ... to reference [the fix PR]") -- nullable, set only once a fix
    -- session/PR exists for this finding. ON DELETE SET NULL: a fix
    -- session being deleted (not a real operation today) must never
    -- cascade into deleting the finding's own review history.
    fix_child_session_id UUID REFERENCES sessions(id) ON DELETE SET NULL,
    fix_pr_number         INTEGER,

    -- first_seen_at/last_seen_at are real, populated columns; a
    -- first_seen_head_sha/last_seen_head_sha PAIR was considered and
    -- deliberately NOT added in this Step -- named here, not silently
    -- omitted: §21.1's own head_sha column belongs to review_verdicts
    -- (Step 62, does not exist at this point in the plan's own sequence),
    -- and neither PostReviewVerdictRequest (this Step's own request DTO)
    -- nor the review turn's own rendered prompt (internal/domain/review.
    -- RenderTurnPrompt) carries a reliable head SHA a reviewing agent
    -- could honestly self-report today -- reviewcontext.Fetch fetches the
    -- pre-fetched diff AT a specific head SHA internally, but that value
    -- is never threaded back out to the prompt text or the verdict-
    -- posting tool's own request shape. Adding a headSha column here now
    -- and leaving it permanently NULL (nothing to populate it with) would
    -- be worse than not having it: a NULL-forever column reads as a bug,
    -- not a deliberate scope decision. Whichever later Step threads a
    -- real head SHA through to this handler (Step 62, or an earlier
    -- follow-up) should add it then, with a real value to store.
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (repo_full_name, pr_number, identity_hash)
);

-- The one read query this Step's own reconciliation path needs (§22.1:
-- "feeding re-review reconciliation as deterministic 'already answered'
-- facts") -- every open+rebutted finding for one PR, in a stable,
-- deterministic order.
CREATE INDEX review_findings_repo_pr_status_idx ON review_findings (repo_full_name, pr_number, status);

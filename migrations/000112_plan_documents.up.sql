-- plan_documents (§31.3's durability fix): an approved plan's own prose
-- lives, until now, ONLY in the events log (a "token" event's own
-- cumulative text, recovered at read time by a bounded, best-effort scan,
-- internal/domain/plan.ExtractContent) -- and events is surface, never
-- durable truth: it is ON DELETE CASCADE from sessions
-- (migrations/000008_events.up.sql). plans carries the SAME session_id
-- ON DELETE CASCADE (migrations/000034_plan_mode.up.sql): today, deleting
-- a session destroys not only the plan's own approval record but the
-- only copy of the text a human actually signed off on -- "an approved
-- plan is cascade-deletable" (§31.3). That is a real durability defect
-- independent of everything else §31 builds, and this migration closes
-- it on its own merits.
--
-- A separate table, never a column on plans and never a snapshot column:
-- the content must be isolable from the row that references it, so a
-- future retention policy (a §30.9-class decision, not made here) can
-- null out CONTENT ALONE with a single-column UPDATE -- exactly the
-- enabling move migrations/000110_shadow_scm_writes_heavy_content.up.sql
-- already takes for its own heavy-content column, and for the identical
-- reason: nulling this table's content never touches plans' own metadata
-- (status, decided_by, decided_at) and never rewrites that row. content
-- is therefore nullable at the schema level -- this table's only writer
-- (httpapi.DecidePlanOnTx) always populates it at approval time, but a
-- later retention null-out is a legitimate value this column must be
-- able to hold, not an exception to work around.
--
-- The FK's delete behavior is the deliberate point of this table, not an
-- afterthought. ON DELETE CASCADE -- plans' own choice for its
-- session_id, and events' for the same column -- is exactly the
-- behavior that destroyed this content in the first place; repeating it
-- here (cascading plan_documents away the moment its plans row goes)
-- would relocate today's defect one hop down and fix nothing. RESTRICT
-- instead: a plans row with a snapshot cannot be deleted -- and,
-- transitively, neither can a session with an approved, snapshotted plan
-- -- until whatever wants to remove it deals with THIS row first,
-- explicitly (null the content, or delete this row outright). That is
-- exactly what a future retention/cleanup path is expected to do; it
-- must never again be a side effect a session deletion gets for free.
CREATE TABLE plan_documents (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id    UUID NOT NULL UNIQUE REFERENCES plans(id) ON DELETE RESTRICT,
    content    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

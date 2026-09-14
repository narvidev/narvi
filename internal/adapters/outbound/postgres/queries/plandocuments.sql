-- Queries backing PlanDocumentStore (§31.3's durability fix for an
-- approved plan's own prose), migrations/000112_plan_documents.up.sql.
--
-- CreatePlanDocument is called exactly once per approved plan, by
-- httpapi.DecidePlanOnTx, in the SAME transaction as
-- ApprovePlanIfAwaitingApproval's own guarded UPDATE (queries/plans.sql)
-- -- see that migration's own comment for why this table, and its FK's
-- delete behavior, exist at all. plan_id's UNIQUE constraint makes a
-- second snapshot for the same plan a hard failure, never a silent
-- overwrite -- a plan is decided exactly once (first-verdict-wins,
-- plans_one_awaiting_approval_per_session), so a second attempt to
-- insert here signals a real bug upstream, not a legitimate re-snapshot.
--
-- GetPlanDocumentByPlanID backs this Step's own coverage measurement:
-- confirming every approved plan has exactly one row here.
--
-- structured_steps (migrations/000126_plan_documents_structured.up.sql,
-- §12.2 item 3) rides the SAME insert as content, from the SAME
-- already-recovered prose -- see that migration's own comment for why NULL
-- is the only representation of "no structure recovered".

-- name: CreatePlanDocument :one
INSERT INTO plan_documents (plan_id, content, structured_steps)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetPlanDocumentByPlanID :one
SELECT * FROM plan_documents WHERE plan_id = $1;

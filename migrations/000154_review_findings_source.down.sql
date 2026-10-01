-- Reverses 000154_review_findings_source.up.sql. Every finding's recorded
-- source and addition check, and every verdict's second fact-check run,
-- go with the columns: nothing a binary without 000154 reads. A binary
-- with it that runs 000154 again finds every finding with no source
-- recorded until its next publication, and every earlier verdict with no
-- second run recorded.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000154. Do not run it against live pods of a binary that carries 000154,
-- for two reasons:
--   - Every query of that binary that returns a whole review_findings or
--     review_verdicts row names these columns (sqlc writes each SELECT * and
--     RETURNING * out as a column list), and its verdict post writes them.
--     After this down each of them fails with SQLSTATE 42703 (column does
--     not exist), so no verdict is posted and no review readout is served
--     until the older binary replaces the pod.
--   - A pod that carries 000154 and restarts before the older binary
--     replaces it applies 000154 again at boot, which locks the older binary
--     out again ("no migration found for version 154").
-- Guarded: IF EXISTS, so it also runs on a database a rollback already
-- brought back to 153 with the columns kept (`migrate force 153`, the up
-- file's "Rolling back") and then forced to 154 again. Each drop is a
-- catalog change: it rewrites nothing, but takes ACCESS EXCLUSIVE on its
-- table for the file's one implicit transaction.
ALTER TABLE review_verdicts DROP COLUMN IF EXISTS additions_check;
ALTER TABLE review_verdicts DROP COLUMN IF EXISTS additions_fact_check_killed;
ALTER TABLE review_verdicts DROP COLUMN IF EXISTS additions_fact_check;

ALTER TABLE review_findings DROP COLUMN IF EXISTS addition_check;
ALTER TABLE review_findings DROP COLUMN IF EXISTS reported_source;

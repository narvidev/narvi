-- Technical plan §24.9's second rule, for a person's review request: a
-- review attempt a person asked for (the configured label, the web
-- re-review button, a mention) that waited behind another turn checks, as
-- it is dispatched, that the head, base and ancestor chain it recorded are
-- still the pull request's. One whose context moved does not start: it
-- ends context_moved (migrations/000159), and the request is owed to its
-- requester instead -- a row of owed_review_requests, which the session
-- actor's owed_review_request timer consumes: it fetches the pull request
-- again, composes the prompt for the head it has now, checks the
-- requester's authorization again, and inserts the re-run turn on the
-- person's path, deleting the row in the same transaction.
--
-- owed_review_requests: one row per request owed.
--   - session_id: the review session (sessions, cascading with it).
--   - requested_by: who asked (users, NULL once that account is deleted,
--     which the authorization check then refuses).
--   - trigger: the lane that asked, turns.request_trigger's value
--     ('label', 'button', 'mention').
--   - request_text: the lane's own text, before any context was folded in:
--     the label's or the button's fixed sentence, or the mention's body.
--     The re-run's prompt is composed from it for the new head.
--   - is_review_attempt: the moved turn's turns.is_review_attempt. Only a
--     review attempt is checked today, so it is always true; kept so a
--     later kind of owed request reads as what it is.
--   - context_moves: how many times in a row this request met a moved
--     context, this move included. Past ReviewContextMoveMaxConsecutive the
--     consumer drops the request and tells its requester once.
--   - moved_turn_id: the turn that ended context_moved (turns, cascading
--     with it), whose workflow attempt, when it has one, the re-run takes
--     over.
--   - created_at: when the move was found, on the database's clock -- the
--     instant a person's stop compares with: a stop requested at or after
--     it drops the request (sessionactor's dropOwedReviewRequestsForStop),
--     the rule the stop timer deletes the session's work-creating timers
--     by.
-- owed_review_requests_session_id_idx serves every read and the stop's
-- delete, all scoped to one session and taken oldest first.
--
-- turns.requested_by, turns.request_text: who asked for the turn and the
-- lane's own text, recorded by the three human lanes beside
-- request_trigger (000159), and carried onto a re-run turn; NULL for every
-- other turn. turns.context_moves: the moves in a row the request behind
-- the turn met before it was inserted, which the next move counts from;
-- NULL -- none -- for every turn but a re-run. All three are nullable so
-- that a turn that records none of them stores nothing for them (a bit of
-- the row's null bitmap each), and a session's turns take the pages they
-- took before. requested_by takes no foreign key: the turn keeps who
-- asked as the audit log does, and a key on turns would scan it to
-- validate.
--
-- No backfill. Every turn that exists when this runs records no
-- requester, no text and no move; a person's attempt an older binary
-- inserted carries no trigger either, so it is never checked.
--
-- # Locks
--
-- The ADD COLUMNs are nullable with no default: none rewrites turns. They
-- take ACCESS EXCLUSIVE on turns for an instant. The new table's foreign keys
-- take SHARE ROW EXCLUSIVE on sessions, users and turns, also for an
-- instant: the table is empty, so nothing is validated.
--
-- # Rolling deploy
--
-- The previous binary works with the table and the columns present:
--   - Every statement it sends names its columns (sqlc writes each
--     SELECT * and RETURNING * out as a column list), so it neither reads
--     nor writes them: its turns are created with no requester, no text
--     and no move.
--   - It never checks a person's attempt, so it dispatches every queued one
--     unchecked, as it always did, and owes nothing.
--   - It does not know the owed_review_request timer kind, so a row this
--     release armed that its pump claims is left to technical plan §2's
--     bound: kept at the claim cadence while it was armed less than
--     UnknownTimerGrace ago, and the session reads scheduled meanwhile.
--     This release arms the kind due at once, so a replica of it claims the
--     row within one claim window.
--   - Its re-review hold does not read owed_review_requests, so its
--     debounce may insert an automatic review while a person's request is
--     owed: one review more, never one lost.
-- migration000160_integration_test.go runs the previous binary's own
-- statements against the new table and columns.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version 160"). A
-- rollback therefore takes one of two steps first, with the control plane
-- scaled to zero:
--   - Keep the table and the columns: with the golang-migrate CLI,
--     `migrate force 159`. The previous binary then boots and works with
--     them present, as above. Requests owed when it rolls back are not
--     re-run by it; their owed_review_request timers are left to §2's
--     bound, which deletes them UnknownTimerDeleteAfter after their last
--     arm. When this release is deployed again, this file runs again and
--     leaves the table, the columns and their rows as they are, and arms
--     the timer again, due at once, for every session still owed a
--     request (the INSERT at the end, which inserts nothing on a first
--     run, the table being empty), so a request whose timer the bound
--     deleted meanwhile is re-run then rather than stranded.
--   - Drop them: run this migration's down (goto 159) with this release's
--     migrations. The down file says what it removes.
CREATE TABLE IF NOT EXISTS owed_review_requests (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id        UUID NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    requested_by      UUID NULL REFERENCES users (id) ON DELETE SET NULL,
    trigger           TEXT NOT NULL,
    request_text      TEXT NULL,
    is_review_attempt BOOLEAN NOT NULL,
    context_moves     INTEGER NOT NULL,
    moved_turn_id     UUID NOT NULL REFERENCES turns (id) ON DELETE CASCADE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS owed_review_requests_session_id_idx ON owed_review_requests (session_id, created_at);
ALTER TABLE turns ADD COLUMN IF NOT EXISTS requested_by UUID;
ALTER TABLE turns ADD COLUMN IF NOT EXISTS request_text TEXT;
ALTER TABLE turns ADD COLUMN IF NOT EXISTS context_moves INTEGER;
INSERT INTO session_timers (session_id, name, fires_at)
SELECT DISTINCT session_id, 'owed_review_request', now() FROM owed_review_requests
ON CONFLICT (session_id, name) DO NOTHING;

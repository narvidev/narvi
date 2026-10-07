-- Queries backing TurnStore (§4.3). Just enough to prove the pipeline end
-- to end (create + get), including exercising the
-- turns_one_processing_per_session partial unique index (§3.3).

-- name: CreateTurn :one
-- prompt/model_id/plan_mode (migrations/000018_session_repos.up.sql,
-- §9.3) are the turn's own dispatch-time inputs -- prompt/model_id are
-- nullable and plan_mode defaults false, so every EXISTING call site
-- (every prior Step's `CreateTurnParams{SessionID, Status}`) keeps
-- compiling and behaving identically: the zero-value nil/nil/false it
-- already implicitly got before this Step's own columns existed.
--
-- effort (migrations/000063_turn_session_effort.up.sql, §29.8)
-- mirrors model_id's own shape exactly, one column over -- plain
-- positional param like model_id itself (this query's own existing style
-- for a nullable column; sqlc generates a keyed struct either way, so
-- every EXISTING call site that never sets it -- a keyed
-- CreateTurnParams{...} literal omitting Effort -- keeps compiling and
-- behaving identically: the zero value, nil, "use the default").
--
-- review_head_sha (migrations/000072_turns_review_head_sha.up.sql, §21
-- review finding C2) mirrors effort's own identical shape one column
-- further -- nil/absent for every non-review turn (every existing call
-- site), set exactly once, at creation, by the two review-turn-creation
-- paths (internal/adapters/inbound/httpapi's createTurnLocked/
-- CreateSessionOnTx) with the commit SHA that turn's own pre-fetched
-- diff was anchored to. See that migration's own doc comment for the
-- full "why".
--
-- answer_only (migrations/000074_plan_followup.up.sql, §23.2)
-- mirrors review_head_sha's own identical shape one column further --
-- nil/absent for every existing call site (every CreateTurnParams
-- literal that predates this Step), set exactly once, at creation, by
-- createTurnLocked's own plan_followup gate (turn.go). See that
-- migration's own doc comment for the full "why NULL vs FALSE" split.
--
-- review_depth (migrations/000080_turns_review_depth.up.sql,
-- §26.3) mirrors review_head_sha's own identical shape one column
-- further -- nil/absent for every non-review turn, set exactly once, at
-- creation, by every review-turn-creation path.
--
-- review_depth_decision (migrations/000083_turns_review_depth_decision.up.sql,
-- §18.4's own precedent) is review_depth's own richer sibling --
-- the full internal/domain/reviewtriage.DecisionRecord, JSON-marshaled by
-- the caller (this query does no encoding of its own).
--
-- review_knowledge_mode/review_knowledge_decision (migrations/
-- 000114_turns_review_knowledge_mode.up.sql,
-- 000116_turns_review_knowledge_decision.up.sql, §31.2/§31.6) mirror
-- review_depth/review_depth_decision's own identical shape two columns
-- further: nil/absent for every non-review turn, set exactly once, at
-- creation, by the SAME review-turn-creation paths.
-- review_knowledge_decision is pre-marshaled JSON (internal/domain/
-- knowledge.InjectedRecord) -- this core does no encoding of its own,
-- mirroring review_depth_decision's own identical convention.
--
-- correlation_id (migrations/000121_turns_correlation_id.up.sql, §12.2
-- item 1's own session-rail gap, §5.3) mirrors review_head_sha's own
-- identical shape one column further: nil/absent for a caller with no
-- correlation id in context (a turn dispatched with no live request
-- context at all -- should not happen in production, but this column is
-- not the place to enforce that), set exactly once, at creation, from
-- whatever internal/platform.CorrelationIDFromContext(ctx) returns at
-- EVERY call site that creates a turn, never re-derived or backfilled
-- later.
--
-- review_verdict_context (migrations/000129_turns_review_verdict_context.up.sql,
-- §21.1's amendment) mirrors review_depth_decision's own identical shape
-- one column further: nil/absent for every non-review turn, set exactly
-- once, at creation, by the SAME review-turn-creation paths, pre-
-- marshaled JSON (internal/domain/reviewverdict.Context) -- this query
-- does no encoding of its own.
--
-- is_review_attempt (migrations/000133_turns_is_review_attempt.up.sql,
-- finding A4) mirrors review_head_sha's own identical shape one column
-- further, EXCEPT its zero value is a real, meaningful `false` rather
-- than NULL/absent (this is a plain boolean, not a nullable pointer
-- field) -- every call site that never sets it (every non-review-turn
-- creation, and even a REUSE-path GitHub turn that is an ordinary
-- follow-up rather than a genuine review attempt) gets `false`, the
-- safe default: dispatch.go/outboxenqueue.go both gate the review-check
-- outbox enqueue on this column being true, never merely on
-- review_head_sha being set. See that migration's own doc comment for
-- the full "why".
--
-- request_trigger (migrations/000159_turns_end_reason.up.sql, technical
-- plan §24.9) is what asked for the turn, for a lane that records it:
-- 'auto' for the automatic re-review (sessionactor's
-- insertAutoRetriggerTurn); 'label' or 'button' for a person's
-- (migrations/000160: the GitHub label lane, the web re-review button, and
-- the re-run of a request owed to its requester); nil for every other call
-- site, a mention's included. A review attempt one of those lanes asked
-- for is checked against its pull request's live context when it is
-- dispatched after waiting behind another turn (sessionactor's
-- reviewcontextcheck.go).
--
-- requested_by, request_text and context_moves (migrations/000160) are
-- what a person's request owes its re-run when its context moved: who
-- asked, the lane's own text before any context was folded in, and the
-- moves in a row it met before this turn was inserted. nil for every turn
-- no human lane records, and context_moves nil for every turn but a
-- re-run.
INSERT INTO turns (session_id, status, prompt, model_id, plan_mode, effort, review_head_sha, answer_only, review_depth, review_depth_decision, review_knowledge_mode, review_knowledge_decision, correlation_id, review_verdict_context, is_review_attempt, request_trigger, requested_by, request_text, context_moves)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, sqlc.narg('request_trigger'), sqlc.narg('requested_by'), sqlc.narg('request_text'), sqlc.narg('context_moves'))
RETURNING *;

-- name: GetTurn :one
SELECT * FROM turns
WHERE id = $1;

-- name: ExistsNewerReviewAttempt :one
-- finding F1 (adversarial review, §21.1b): reviewverdict.Acceptance.
-- Applicable's own "new attempt" half cannot be reduced to verdict-id
-- equality alone -- a review attempt that ends not_assessed posts NO
-- review_verdicts row at all (sessionactor.enqueueReviewCheckNotAssessed
-- fires exactly because ExistsReviewVerdictForAttempt is false), so
-- GetLatestReviewVerdict still returns the OLD, accepted verdict even
-- though a NEWER attempt has since run. This answers that question
-- directly against turns itself: has any genuine review attempt
-- (is_review_attempt = true, mirroring dispatch.go/outboxenqueue.go's own
-- identical gate on this same column) in the SAME session, STRICTLY
-- newer than afterCreatedAt, run since -- regardless of whether it ever
-- posted a review_verdicts row. The caller (internal/app/reviewverdict.
-- HasNewerReviewAttempt) supplies afterCreatedAt from the ACCEPTED
-- attempt's own turns.created_at (TurnStore.Get, above), so this query
-- never needs to name that attempt a second time: session_id scoped to
-- rows strictly after ITS OWN timestamp already answers "is the accepted
-- attempt still the latest review attempt in this session".
--
-- A turn with an end_reason (migrations/000159, technical plan §24.9) is
-- no attempt: one ended context_moved never ran, and the newer attempt it
-- would stand for is the one its re-request queues. So it is excluded,
-- as every reader of attempts excludes it (sessionactor's
-- TestReviewAttemptReadersExcludeContextMoved).
SELECT EXISTS(
    SELECT 1 FROM turns
    WHERE session_id = $1 AND is_review_attempt = true AND end_reason IS NULL AND created_at > $2
) AS has_newer_review_attempt;

-- name: GetNewestReviewAttempt :one
-- The newest genuine review attempt (is_review_attempt = true, the same
-- gate ExistsNewerReviewAttempt and the review-check outbox apply) in
-- sessionID's own turn history -- its id, state and creation time, never
-- the prompt. Row 182's result (technical plan §43.20) reads a pull
-- request's review state from it: in progress while it has not ended, not
-- assessed once it has ended without ExistsReviewVerdictForAttempt, and
-- assessed once it has posted. Ordered like GetLatestReviewVerdict orders
-- attempts (the producing turn's created_at), id breaking an exact tie so
-- the pick is reproducible. pgx.ErrNoRows means the session has run no
-- review attempt at all.
--
-- A turn with an end_reason is no attempt (migrations/000159, technical
-- plan §24.9): one ended context_moved never ran, so it is never read as
-- the newest attempt, not assessed.
SELECT id, status, created_at FROM turns
WHERE session_id = $1 AND is_review_attempt = true AND end_reason IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetReviewAttemptToCheck :one
-- Technical plan §24.9's context check, read by the session actor before
-- each dispatch evaluation, outside any transaction (sessionactor's
-- preReadReviewContext): the session's next turn to dispatch -- its oldest
-- pending turn no stop flagged, when none is dispatched or processing,
-- turn.NextToDispatch's pick after planDispatch's stop gate, in
-- ListTurnsForSession's order -- with what the check reads of it: whether
-- it is a review attempt and which lane asked for it, the head and the
-- context it recorded, whether it waited behind another turn, and whether
-- the session's sandbox can take it now. No row: nothing to dispatch.
--
-- queued: another turn of the session, created before it, is still open
-- or ended after it was created (or ended with no completed_at, which
-- reads as queued, never as not). completed_at is the ending replica's
-- clock and created_at the database's, so a turn that ended within their
-- skew of this one's creation can read either way: read as queued, the
-- attempt is checked when it would not have been (a read of the code host
-- that may still find a moved context); read as not, it starts unchecked,
-- as every attempt did before this rule. Only a review attempt a lane that
-- records its trigger asked for is asked -- the automatic re-review
-- ('auto') or a person's request ('label', 'button',
-- migrations/000160), the kinds the check applies to
-- (turn.ContextCheckedAtDispatch) -- so no other pick, a follow-up or an
-- attempt an older binary inserted, costs the walk. The walk reads the
-- pick's session's earlier
-- turns through (session_id, dispatched_message_id) until it finds one
-- that keeps the pick queued: when none does, every one of them, about one
-- heap buffer per earlier turn on a session whose turns lie among other
-- sessions' (technical plan §24.9 gives the measure) -- the same turns
-- ListTurnsForSession reads in the evaluation that follows.
--
-- sandbox_live: the session's sandbox is ready or suspect, the two states
-- planDispatch dispatches to: the actor reads the code host only then, so
-- an evaluation that spawns a sandbox never pays for a check it would
-- make again once the sandbox is up.
SELECT
    b.id,
    b.is_review_attempt,
    b.request_trigger,
    b.review_head_sha,
    b.review_verdict_context,
    (b.is_review_attempt AND COALESCE(b.request_trigger, '') IN ('auto', 'label', 'button') AND EXISTS (
        SELECT 1 FROM turns o
        WHERE o.session_id = b.session_id
          AND (o.created_at < b.created_at OR (o.created_at = b.created_at AND o.id < b.id))
          AND (o.status IN ('pending', 'dispatched', 'processing')
               OR o.completed_at IS NULL
               OR o.completed_at > b.created_at)
    ))::boolean AS queued,
    COALESCE((
        SELECT sb.status IN ('ready', 'suspect') FROM sandboxes sb WHERE sb.session_id = b.session_id
    ), false)::boolean AS sandbox_live
FROM turns b
WHERE b.session_id = sqlc.arg('session_id')
  AND b.status = 'pending'
  AND b.stop_requested_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM turns f
      WHERE f.session_id = sqlc.arg('session_id')
        AND f.status IN ('dispatched', 'processing')
  )
ORDER BY b.created_at, b.id
LIMIT 1;

-- name: SetTurnContextUnconfirmed :execrows
-- Technical plan §24.9: the dispatch of a queued review attempt whose
-- context could not be compared with the live one -- the code host's read
-- failed, or a fact the comparison needs is unknown -- lets it start, and
-- records here, in the dispatching transaction, that it started
-- unconfirmed. Only while the turn is still pending, the state the
-- dispatch reads it in; 0 rows otherwise.
UPDATE turns
SET context_unconfirmed_at = now()
WHERE id = $1 AND status = 'pending';

-- name: ReviewRetriggerHeld :one
-- Technical plan §24.9: whether the re-review debounce of this session
-- holds -- some turn of the session is still open (pending, dispatched or
-- processing), so an automatic review would queue behind it with a prompt
-- built for a head that may move again before it runs; or a person's
-- review request is owed (owed_review_requests, migrations/000160): its
-- re-run, for the head the pull request has now, comes first, and its
-- consumer inserts it as an open turn that holds the lane in turn, or
-- drops it and wakes the held debounce. The owed term is one probe of
-- owed_review_requests_session_id_idx. Read by the
-- debounce's fire (sessionactor's readReviewRetriggerState, then again in
-- finishReviewRetrigger just before the insert) inside the actor's
-- transaction, under the session's actor-epoch row lock that every turn
-- insert on an existing session also takes, so the answer is consistent
-- with every turn insert.
--
-- The open states are listed, not the terminal ones excluded, for the
-- planner's sake. Its estimate of "status is none of the three terminal
-- states" sums their frequencies as disjoint, and when a table of ended
-- turns rounds those frequencies to a hair over the whole, it finds the
-- sum out of range and falls back to treating them as independent: about
-- 30% of a long session's turns read as open, and a custom plan then scans
-- the table sequentially for the first one (measured: 910 buffers on
-- 80,000 turns, against 2). Listing the open states estimates what is
-- there, next to nothing. turns_open_session_id_idx (migrations/000158)
-- keeps the deny list -- every state but the three terminal ones -- and
-- this list implies it, so the read is one probe of that index under a
-- custom plan and a generic one alike, never a walk of the session's
-- history. The list is turn.IsTerminal's complement today, and
-- TestReviewRetriggerHold_HeldMatchesTurnIsTerminal fails the day
-- turn_status gains a state, so the hold never quietly reads a new open
-- state as ended.
SELECT (EXISTS (
    SELECT 1 FROM turns t
    WHERE t.session_id = sqlc.arg('session_id')
      AND t.status IN ('pending', 'dispatched', 'processing')
) OR EXISTS (
    SELECT 1 FROM owed_review_requests o
    WHERE o.session_id = sqlc.arg('session_id')
))::boolean AS held;

-- name: UpdateTurnStatus :one
-- Sets a turn's status, plus dispatched_at/completed_at/
-- dispatched_sandbox_gen when the caller supplies one (sqlc.narg +
-- COALESCE: an absent/NULL argument leaves the existing column value
-- untouched, matching dispatched_at/completed_at's own nullability --
-- each is set at most once, at the (from, trigger) transition that
-- reaches Dispatched or a terminal state respectively).
--
-- dispatched_sandbox_gen (migration 000026_turn_dispatch_gen.up.sql,
-- §3.3 "turn recovery") is stamped by TWO distinct call sites sharing this
-- SAME query, both at the moment a Prompt payload is built and about to be
-- sent: tryPlanDispatch (internal/app/sessionactor/dispatch.go), alongside
-- the SAME status=dispatched write that already sets dispatched_at, for
-- the normal Pending->Dispatched->Processing path; and tryPlanReenqueue
-- (same file), which passes the CURRENT status back unchanged (the turn
-- is already, validly, Processing -- this call re-stamps
-- dispatched_sandbox_gen only, never re-transitions status) for a
-- Processing turn whose prompt needs re-sending to a respawned sandbox
-- incarnation.
--
-- dispatched_event_id (migrations/000089_turns_dispatched_event_id.up.sql)
-- is stamped by those SAME two call sites, in the SAME write, from
-- MaxEventIDForSession (queries/events.sql): the events-log high-water
-- mark at the instant of dispatch, which the §26.4 corroboration
-- queries use as their lower bound instead of a timestamp. It follows the
-- identical sqlc.narg + COALESCE "absent argument leaves the column
-- untouched" convention as the three columns above it.
--
-- dispatched_message_id (migrations/000131_turns_dispatched_message_id.up.sql,
-- finding F3, §21.1's amendment) is stamped by the SAME two call sites, in the SAME
-- write, from the sandboxws.Prompt MessageId BuildPromptPayload embeds
-- into the wire payload itself -- GetTurnByDispatchedMessageID (below) is
-- what a verdict-posting request resolves ITS OWN turn by, instead of
-- "whichever turn is processing for this session right now". Follows the
-- identical sqlc.narg + COALESCE convention.
--
-- end_reason (migrations/000159, technical plan §24.9) is set by the one
-- write that ends a turn for a reason of its own -- the context check's
-- context_moved end (sessionactor's reviewcontextcheck.go) -- through the
-- recorder every status write goes through, so that end wakes a held
-- re-review like any other. The same sqlc.narg + COALESCE convention:
-- every other write leaves it as it is.
UPDATE turns
SET status = $2,
    dispatched_at = COALESCE(sqlc.narg('dispatched_at'), dispatched_at),
    completed_at = COALESCE(sqlc.narg('completed_at'), completed_at),
    dispatched_sandbox_gen = COALESCE(sqlc.narg('dispatched_sandbox_gen'), dispatched_sandbox_gen),
    dispatched_event_id = COALESCE(sqlc.narg('dispatched_event_id'), dispatched_event_id),
    dispatched_message_id = COALESCE(sqlc.narg('dispatched_message_id'), dispatched_message_id),
    end_reason = COALESCE(sqlc.narg('end_reason'), end_reason)
WHERE id = $1
RETURNING *;

-- name: ListTurnsForSession :many
-- Full turn history for one session, oldest first -- exactly the input
-- shape internal/domain/session.DeriveStatus requires (an ordered slice
-- of turn.Summary derived from these rows). id breaks a tie in
-- created_at (turns one transaction created share its now()), the order
-- GetSessionActivityFacts and GetReviewAttemptToCheck read too, so the
-- pick turn.NextToDispatch makes from these rows is the turn that
-- pre-read named (technical plan §24.9).
SELECT * FROM turns
WHERE session_id = $1
ORDER BY created_at ASC, id ASC;

-- name: MarkTurnProgressNotified :execrows
-- Audit finding M16 ("completeness", internal/adapters/outbound/linearapi/
-- doc.go): atomic, race-safe "has this turn already had its one mid-turn
-- progress milestone fired" guard -- mirrors ApprovePlanIfAwaitingApproval/
-- RejectPlanIfAwaitingApproval's own "guarded UPDATE, observed via
-- :execrows" idiom exactly (queries/plans.sql), just for a nullable
-- timestamp rather than an enum status. 0 rows affected means
-- progress_notified_at was already set for this turn (a second, later
-- tool_call event in the same turn -- the expected, common case once the
-- milestone has already fired once -- or a race); exactly 1 row affected
-- means THIS call is the one that gets to enqueue the Linear progress
-- notification (see internal/app/sessionactor/progressnotify.go).
UPDATE turns
SET progress_notified_at = $2
WHERE id = $1 AND progress_notified_at IS NULL;

-- name: SetTurnPromptReceiptRequest :exec
-- Technical plan §3.3, prompt receipts (migrations/000155_prompt_receipts.up.sql):
-- records whether the dispatch that just stamped dispatched_message_id
-- asked its sandbox for a receipt, in that dispatch's own transaction
-- (tryPlanDispatch, tryPlanReenqueue). message_id is that dispatch's
-- dispatched_message_id when it asked, and NULL when it did not -- a later
-- dispatch that does not ask clears an earlier request. receipt_requested_at
-- is the database's now(), the start of PromptResendWindow, and
-- receipt_checked_ready_seq the sandbox's ready_seq at the dispatch, so only
-- a ready recorded after it counts as a reconnect to answer. Every dispatch
-- starts its re-sends from 0 (receipt_resend_count).
UPDATE turns
SET receipt_requested_message_id = sqlc.narg('message_id'),
    receipt_requested_at = CASE WHEN sqlc.narg('message_id')::text IS NULL THEN NULL ELSE now() END,
    receipt_checked_ready_seq = CASE WHEN sqlc.narg('message_id')::text IS NULL THEN NULL ELSE sqlc.arg('ready_seq')::integer END,
    receipt_resend_count = 0
WHERE id = sqlc.arg('id');

-- name: GetTurnPromptReceiptState :one
-- Technical plan §3.3, prompt receipts: whether the receipt of the turn's
-- current dispatch is stored, and how long ago that dispatch asked for it,
-- in nanoseconds (a time.Duration) on the database's clock -- one
-- statement, one clock. The receipt is read by its key, never by scanning
-- types: the agent gives it the deterministic messageId
-- 'prompt_received:{promptMessageId}', so a receipt stored by any binary --
-- one that does not know the type stores it through the same generic path,
-- under the same wire messageId -- is the same row, found through
-- events_session_id_message_id_idx. The type check is an extra guard. No
-- row when the turn's dispatch asked for no receipt.
SELECT EXISTS (
           SELECT 1 FROM events e
           WHERE e.session_id = t.session_id
             AND e.message_id = 'prompt_received:' || t.dispatched_message_id
             AND e.type = 'prompt_received'
       ) AS receipt_stored,
       (EXTRACT(EPOCH FROM (now() - t.receipt_requested_at)) * 1000000000)::bigint AS since_request_nanos
FROM turns t
WHERE t.id = $1 AND t.receipt_requested_at IS NOT NULL;

-- name: MarkTurnPromptReconnectAnswered :execrows
-- Technical plan §3.3, prompt receipts: claims one same-gen reconnect of
-- the turn's sandbox for this evaluation -- moves receipt_checked_ready_seq
-- from the value the evaluation read to the sandbox's current ready_seq,
-- and only while the turn is still Processing, unflagged by a stop, on the
-- dispatch that asked for the receipt, with no other evaluation having
-- claimed this ready first. 0 rows affected means one of those no longer
-- holds, and nothing is sent. The stop guard is defense in depth behind
-- planReenqueueOrRespawn's own early return on stop_requested_at. resend
-- (0 or 1) is added to receipt_resend_count in the same statement when
-- the claim is answered by a re-send, and the count read is part of the
-- claim, so PromptResendMaxPerTurn is enforced on the same row state the
-- decision read.
UPDATE turns
SET receipt_checked_ready_seq = sqlc.arg('ready_seq')::integer,
    receipt_resend_count = receipt_resend_count + sqlc.arg('resend')::integer
WHERE id = sqlc.arg('id')
  AND status = 'processing'
  AND stop_requested_at IS NULL
  AND dispatched_message_id = sqlc.arg('message_id')::text
  AND receipt_requested_message_id = sqlc.arg('message_id')::text
  AND receipt_checked_ready_seq = sqlc.arg('checked_ready_seq')::integer
  AND receipt_checked_ready_seq < sqlc.arg('ready_seq')::integer
  AND receipt_resend_count = sqlc.arg('resend_count')::integer;

-- name: RecordTurnCheckoutRequest :execrows
-- Technical plan §21.1 and §30.4 (migrations/000167_review_turn_checkout.up.sql):
-- records, in the dispatch evaluation's own transaction, the checkout
-- command the session actor is about to send a review turn's sandbox
-- (sessionactor's reviewcheckout.go): its messageId, the gen it goes to,
-- when it is sent and the gen's ready_seq then. The first request on a gen
-- stamps checkout_requested_at, the start of the turn's bound on that gen,
-- which every later request on the same gen keeps; a request on another
-- gen starts the bound again, and its counts with it (both NULL, read as
-- 0, until the turn's first request). after_failure counts
-- the reply this request answers -- the previous send's -- as failed.
-- Only while the turn is open (pending, or processing for a re-send to a
-- new gen); 0 rows otherwise, and nothing is sent.
UPDATE turns
SET checkout_requested_at = CASE WHEN checkout_gen IS NOT DISTINCT FROM sqlc.arg('gen')::integer
                                      AND checkout_requested_at IS NOT NULL
                                 THEN checkout_requested_at ELSE now() END,
    checkout_sends = CASE WHEN checkout_gen IS NOT DISTINCT FROM sqlc.arg('gen')::integer
                          THEN COALESCE(checkout_sends, 0) + 1 ELSE 1 END,
    checkout_failures = CASE WHEN checkout_gen IS NOT DISTINCT FROM sqlc.arg('gen')::integer
                             THEN COALESCE(checkout_failures, 0) + CASE WHEN sqlc.arg('after_failure')::boolean THEN 1 ELSE 0 END
                             ELSE 0 END,
    checkout_gen = sqlc.arg('gen')::integer,
    checkout_message_id = sqlc.arg('message_id')::text,
    checkout_sent_at = now(),
    checkout_sent_ready_seq = sqlc.arg('ready_seq')::integer
WHERE id = sqlc.arg('id') AND status IN ('pending', 'processing');

-- name: GetTurnCheckoutState :one
-- Technical plan §21.1 and §30.4: the facts the session actor decides a
-- review turn's checkout on (turn.DecideReviewCheckout), read in one
-- statement on the database's clock: the turn's latest checkout request
-- (its messageId, gen, ready_seq at the send and counts), how long ago the
-- first request on that gen and the latest send were made, in nanoseconds
-- (a time.Duration; 0 when none was made), and the reply, when one is
-- stored. The reply is read by its key, never by scanning types: the agent
-- gives it the deterministic messageId 'checkout_result:{command
-- messageId}', so a reply stored by any binary -- one that does not know
-- the type stores it through its generic path, under the same wire
-- messageId -- is the same row, found through
-- events_session_id_message_id_idx. The type check is an extra guard. A
-- reply to an earlier command, or to another gen's, is never read: the
-- key is the latest command's, and the gen fence never stores a reply of
-- a gen that is no longer live.
SELECT t.checkout_message_id,
       t.checkout_gen,
       COALESCE(t.checkout_sends, 0)::integer AS checkout_sends,
       COALESCE(t.checkout_failures, 0)::integer AS checkout_failures,
       t.checkout_retired_gen,
       t.checkout_sent_ready_seq,
       COALESCE((EXTRACT(EPOCH FROM (now() - t.checkout_requested_at)) * 1000000000)::bigint, 0)::bigint AS since_request_nanos,
       COALESCE((EXTRACT(EPOCH FROM (now() - t.checkout_sent_at)) * 1000000000)::bigint, 0)::bigint AS since_send_nanos,
       (SELECT e.payload FROM events e
         WHERE e.session_id = t.session_id
           AND e.message_id = 'checkout_result:' || t.checkout_message_id
           AND e.type = 'checkout_result') AS reply
FROM turns t
WHERE t.id = $1;

-- name: SetTurnCheckedOut :execrows
-- Technical plan §21.1 and §30.4: the commit a review turn's sandbox
-- reported holding, recorded in the commit that dispatches the turn (or
-- re-sends it to a new gen) -- the audit fact that the turn ran on the
-- head it recorded. Only while the turn is open; 0 rows otherwise.
UPDATE turns
SET checked_out_sha = sqlc.arg('sha')::text
WHERE id = sqlc.arg('id') AND status IN ('pending', 'processing');

-- name: SetTurnCheckoutRetiredGen :execrows
-- Technical plan §21.1 and §30.4: the gen a review turn's failed checkouts
-- retired (sessionactor's reviewcheckout.go), recorded in the transaction
-- that retires it, so the turn retires no other: a turn retires at most
-- one gen for its checkouts. Only while the turn is open; 0 rows
-- otherwise.
UPDATE turns
SET checkout_retired_gen = sqlc.arg('gen')::integer
WHERE id = sqlc.arg('id') AND status IN ('pending', 'processing');

-- name: GetProcessingTurnForSession :one
-- §20 ("builder epistemic pre-action check", §20.2) own epistemic-
-- outcome-posting endpoint's first read -- mirrors WorkflowStore's own
-- GetRunningRunForSession/GetLiveStepRunForRun precedent (queries/
-- workflows.sql): the caller (a sandbox-authenticated POST naming no turn
-- id at all, exactly like the workflow-step-outcome endpoint) resolves
-- "the session's own CURRENTLY live turn" itself, from the sandbox-
-- authenticated session id alone. turns_one_processing_per_session
-- (migrations/000005_turns.up.sql) guarantees at most one row can ever
-- match.
SELECT * FROM turns
WHERE session_id = $1 AND status = 'processing';

-- name: GetTurnByDispatchedMessageID :one
-- finding F3 (§21.1's amendment): resolves the SPECIFIC turn a verdict-posting
-- request actually originated from, by the sandboxws.Prompt MessageId that
-- request's own header presents (see the review-verdict handler,
-- httpapi.PostReviewVerdict) -- NEVER by session-wide "current" status,
-- which GetProcessingTurnForSession above answers and which this query
-- deliberately does NOT ask: a turn that timed out and was marked 'failed'
-- while its own agent was still posting is exactly the case this query
-- must still find, so there is no "AND status = ..." filter here at all.
-- turns.dispatched_message_id is unique in PRACTICE (a UUID minted fresh
-- per dispatch, scoped further by session_id in this WHERE clause) though
-- not DB-enforced unique (corrected, G7, fourth adversarial-review round:
-- migration 000131 DOES add an index over (session_id,
-- dispatched_message_id) -- for this query's own performance, never a
-- constraint -- so pointing here at "why no index... was added" was
-- stale the moment that migration shipped; no migration's own doc
-- comment actually explains why a UNIQUE constraint specifically was
-- never added, so none is cited); a caller-observed multiple-row result
-- would be a genuine anomaly, not a normal outcome this query's own :one
-- cardinality anticipates. No matching row (dispatched_message_id absent,
-- or naming a turn from a different session entirely) is pgx.ErrNoRows,
-- mirroring GetTurn's own identical not-found convention -- the caller
-- REFUSES the request (403), never degrades and proceeds (corrected, G7,
-- fourth round: the sentence this replaced -- "the caller degrades
-- exactly like a not-found processing turn already does" -- described
-- this column's own ORIGINAL, pre-403 behavior, which migration 000131's
-- own doc comment already documents as corrected, E3, third round, for
-- the identical reason; this copy of the same stale sentence was never
-- updated along with it). See reviewverdict.go's own outcome table
-- (internal/adapters/inbound/httpapi) for the current, authoritative
-- behavior.
SELECT * FROM turns
WHERE session_id = $1 AND dispatched_message_id = $2;

-- name: GetNextTurnDispatchedEventID :one
-- The upper bound of one turn's sub-task trace (§26.4's corroboration,
-- §26.6's amendment): the lowest dispatched_event_id among the session's
-- OTHER turns that is at or above this turn's own. Every event a later
-- turn produces lands above that turn's watermark (MaxEventIDForSession,
-- queries/events.sql), so a read of this turn's trace bounded by
-- `id <= next` holds none of them. The bound is needed because a turn
-- that timed out is marked failed without stopping its agent, the next
-- turn is dispatched to the same sandbox at the same gen, and the timed-
-- out turn's late verdict is still resolved by its own message id
-- (GetTurnByDispatchedMessageID): without it, the later turn's sub-tasks
-- -- its routine first fact-check included -- would read as the earlier
-- turn's own.
--
-- ">=" rather than ">": another turn whose watermark EQUALS this one's
-- was dispatched with no event between the two dispatches, so the event
-- log cannot tell the two turns' events apart. The caller sees next ==
-- its own watermark and treats the trace as not read in full, never as
-- an empty one. A turn re-sent to a respawned sandbox carries the
-- watermark of its latest dispatch, so a bound taken from it is that
-- dispatch's. pgx.ErrNoRows: no other turn was dispatched at or after
-- this one, and the trace has no upper bound.
SELECT dispatched_event_id FROM turns
WHERE session_id = $1
  AND id <> $2
  AND dispatched_event_id >= sqlc.arg('dispatched_event_id')::bigint
ORDER BY dispatched_event_id ASC
LIMIT 1;

-- name: ExistsEarlierTurnLeftRunning :one
-- Whether the session holds an EARLIER turn, dispatched to the same
-- sandbox gen before this one, that ended without its own
-- execution_complete (§26.6's amendment): timed out, stopped, abandoned
-- or refused, so the control plane appended a synthetic one naming it
-- (`"synthetic": true, "turn_id": <id>`, sessionactor's appendEvent).
-- Nothing stops that turn's agent, so it may still be running in the same
-- sandbox, at the same gen, while this turn runs, and its late sub-tasks
-- land in this turn's window (bounded below only, by this turn's own
-- watermark) where nothing tells them from this turn's own: no sub-task
-- event names the turn or prompt it belongs to. The caller then reads
-- this turn's trace as not read in full, for both checks. A turn that
-- ended with a real execution_complete leaves no synthetic one, and
-- changes nothing; an earlier turn on another gen ran in a sandbox
-- incarnation that is gone, whose events the gen filter already excludes.
-- The rule is conservative: it holds until the sandbox's gen moves on, so
-- a session whose turn timed out reads every later turn's claims on that
-- gen as unconfirmed.
--
-- A turn whose prompt certainly never reached the sandbox is not one of
-- them: its agent never existed. sessionactor's failDispatchedTurn marks
-- that synthetic event `"delivered": false` -- a dispatch refused before
-- the prompt was sent, or a send refused with no live connection, which
-- writes nothing -- and such an event is not counted. Any other synthetic
-- event still is: a timeout, a stop, or a send failure that may have
-- followed a partial write. Only a synthetic event without the mark can
-- make a turn count, so a stray marked event for a turn that did time out
-- cannot hide that turn's own unmarked one.
SELECT EXISTS (
    SELECT 1 FROM turns earlier
    WHERE earlier.session_id = $1
      AND earlier.id <> $2
      AND earlier.dispatched_sandbox_gen = sqlc.arg('gen')::int
      AND earlier.dispatched_event_id < sqlc.arg('dispatched_event_id')::bigint
      AND EXISTS (
          SELECT 1 FROM events e
          WHERE e.session_id = earlier.session_id
            AND e.type = 'execution_complete'
            AND e.payload->>'synthetic' = 'true'
            AND e.payload->>'turn_id' = earlier.id::text
            AND COALESCE(e.payload->>'delivered', '') <> 'false'
      )
) AS left_running;

-- name: SetTurnEpistemicOutcome :execrows
-- The guarded UPDATE backing that same endpoint (§20.2) -- mirrors
-- SetWorkflowStepRunOutcome's own "WHERE ... AND status = 'running'" guard
-- exactly (queries/workflows.sql), one status value over: re-checks the
-- turn is STILL the live processing one at write time, closing the race
-- where it completed/failed/was cancelled between this endpoint's own
-- GetProcessingTurnForSession read and this write. Unguarded by "AND
-- epistemic_outcome IS NULL" -- deliberately, mirroring
-- SetWorkflowStepRunOutcome's own identical choice: an agent that calls
-- this endpoint more than once for the same still-processing turn (e.g.
-- correcting itself) gets last-write-wins, not a rejected second call.
-- 0 rows affected means the turn is no longer processing (a genuine race,
-- or a stale/foreign turn id having somehow been targeted -- this query
-- takes none, so in practice only the race).
UPDATE turns
SET epistemic_outcome = $2
WHERE id = $1 AND status = 'processing';

-- name: RecordTurnStepCost :execrows
-- §25.15's per-step cost accumulation, made idempotent on the key the wire
-- actually gives us for that: step_finish.stepId (§6.1), one per step.
--
-- ONE statement, three jobs. The CTE resolves the turn this event belongs
-- to from the sandbox-authenticated session id alone, with no preceding
-- read -- turns_one_processing_per_session (migrations/000005) guarantees
-- at most one row can match. The INSERT claims (session_id, step_id) or
-- conflicts away to nothing. The UPDATE runs ONLY over rows the INSERT
-- actually produced, so a redelivered step_finish moves no dollars and a
-- genuinely new one moves exactly its own.
--
-- The key is (session_id, step_id) and NOT (turn_id, step_id), which is
-- what it was until migration 000100. turn_id is resolved from whichever
-- turn is processing when the event lands, so it MOVES between a delivery
-- and its replay -- a replay arriving after the turn boundary resolved a
-- different turn, did not conflict, and charged the same step again.
-- Measured: one $5.00 step delivered twice charged $10.00. An idempotency
-- key cannot be derived from state that changes between the two
-- deliveries it exists to tell apart.
--
-- This replaces an earlier version gated on whether appendRawEvent had
-- INSERTED the raw event row. That flag answers "was this (session_id,
-- message_id) new to the events table", which is not the same question --
-- step_start and step_finish are two parts of one assistant message and
-- share its id, so step_start always claimed the row first and every
-- production step_finish was discarded before its cost was ever read. See
-- migrations/000099_turn_step_costs.up.sql for the full account.
--
-- Concurrency: "SET cost_usd = COALESCE(cost_usd, 0) + ..." is computed IN
-- SQL over a row Postgres locks for the duration, so two step_finish events
-- for the same turn always sum and never clobber -- unlike a Go-side
-- read-then-write, which can lose a sibling's increment. COALESCE, never a
-- bare "+", because turns.cost_usd's own migration (000098) keeps NULL as
-- the ONLY representation of "no cost has arrived yet", and a bare sum
-- against NULL stays NULL forever -- the exact "no cost yet reads as free"
-- failure §25.15 exists to prevent.
--
-- Returns rows updated (0 or 1). 0 means either a redelivery (already
-- counted) or no turn currently processing for this session -- the caller
-- distinguishes them only by logging, never by retrying: both are states
-- where adding money again would be the worse error.
--
-- KNOWN LIMIT, stated rather than implied: this attributes cost to
-- whichever turn is processing WHEN THE EVENT LANDS, not to the turn the
-- event was emitted for. They are the same turn in every ordinary case,
-- because step_finish only arrives mid-turn. They are not the same if a
-- turn terminalizes (timeout, cancel) while one of its step_finish events
-- is still in flight and the next turn has already started -- those
-- dollars land on the newer turn. Closing that needs a turn id on the
-- event itself, which §6.1 does not carry today; it is recorded here so
-- the next reader does not mistake the current behaviour for a guarantee.
WITH target AS (
    SELECT t.id FROM turns AS t WHERE t.session_id = $1 AND t.status = 'processing'
), claimed AS (
    INSERT INTO turn_step_costs (session_id, turn_id, step_id, cost_usd)
    SELECT $1, target.id, $2, $3 FROM target
    ON CONFLICT (session_id, step_id) DO NOTHING
    RETURNING turn_id, cost_usd
)
UPDATE turns
SET cost_usd = COALESCE(turns.cost_usd, 0) + claimed.cost_usd
FROM claimed
WHERE turns.id = claimed.turn_id;


-- name: ListSessionCostTotalsWithRepos :many
-- The shadow-operator surface's own LLM-spend line (§30.1: "surfaced, not suppressed" --
-- shadow burns real customer provider credit, and the evaluator must see
-- it). Reuses turns.cost_usd (migration 000098), the SAME running total
-- internal/app/sessionactor's own recordStepFinishCost (stepcost.go)
-- already maintains from step_finish.cost.usd -- this is a READ over
-- that existing figure, never a second cost-computation path.
--
-- SUM ignores a NULL per-turn total, so a session's own total_cost_usd
-- here is NULL only when EVERY one of its turns still has none -- never
-- a fabricated $0 for a session that simply has not reported a figure
-- yet (turns.cost_usd's own migration comment: "NULL, never 0, stays the
-- ONLY representation of 'no cost has arrived yet'" -- this query
-- preserves that discipline rather than collapsing it at the aggregate
-- boundary). The caller (internal/app/shadowoperator) sums these
-- per-session totals with reviewtriage.NumericToFloat64, the SAME
-- pgtype.Numeric-to-float64 conversion httpapi/workflowruns.go's own
-- per-step cost display already uses.
--
-- Joined with sessions.repos for the SAME Go-side repo resolution
-- ListShadowSuppressedOutboxWithSessionRepos uses (outbox.sql's own doc
-- comment) -- every session with at least one turn and at least one
-- named repository, LIVE or shadow alike: LLM spend is surfaced
-- regardless of egress mode (§30.1), so this performs no
-- suppressed_in_shadow filtering at all, unlike the outbox query above.
SELECT s.id AS session_id,
       s.repos AS repos,
       SUM(t.cost_usd)::numeric(14, 6) AS total_cost_usd
FROM sessions s
JOIN turns t ON t.session_id = s.id
WHERE s.repos != '[]'::jsonb
GROUP BY s.id;

-- name: GetPlatformCostSummaryInWindow :one
-- §12.2 item 6's own "Cost" KPI tile ("cost + median per
-- session"). total_cost_usd is the straight SUM of every costed turn in
-- the window; median_cost_usd_per_session is percentile_cont(0.5) over
-- each SESSION's own per-session total (the inner query), not over
-- individual turns -- "median per session" names the session as the
-- unit, exactly like the tile label says, and a per-turn median would
-- silently answer a different question (a session with many cheap turns
-- would drag a per-turn median down without changing what any session
-- actually spent). costed_session_count is the sample size behind BOTH
-- figures: 0 means no turn anywhere in the window ever recorded a cost
-- figure (turns.cost_usd IS NULL, "no cost data has arrived yet",
-- migrations/000098's own contract) -- the caller's own "not yet
-- computed" sentinel, distinct from a real, computed $0.00 (which would
-- require at least one costed turn summing to exactly zero -- rare, but
-- a real, honest answer when the sample size is nonzero).
--
-- percentile_cont returns NULL over zero input rows. COALESCEd to 0
-- rather than left nullable -- unlike total_cost_usd (a real, meaningful
-- 0 either way), a NULL median here is not a distinct RENDERABLE state:
-- the caller gates on costed_session_count alone (0 => "not yet
-- computed") and must NEVER read this column when that count is 0,
-- exactly mirroring GetBootP95InWindow's own identical "gate on the
-- count, the percentile column is meaningless below the gate" contract
-- (queries/events.sql) -- collapsing to a real, non-NULL 0::float8 here
-- (rather than relying on sqlc's own nullability inference for an
-- aggregate expression, which does not reliably mark this NULLable) is
-- what keeps the generated Go field a plain float64 that pgx can always
-- scan into, never a runtime "cannot scan NULL into *float64" for the
-- empty-window case.
--
-- Bounded by turns_cost_created_at_idx (migrations/
-- 000125_platform_analytics_indexes.up.sql).
WITH per_session AS (
    SELECT session_id, SUM(cost_usd) AS total
    FROM turns
    WHERE cost_usd IS NOT NULL AND created_at >= $1
    GROUP BY session_id
)
SELECT
    COALESCE(SUM(total), 0)::numeric(14, 6) AS total_cost_usd,
    COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY total), 0)::float8 AS median_cost_usd_per_session,
    COUNT(*)::bigint AS costed_session_count
FROM per_session;

-- name: ListCostByModelInWindow :many
-- §12.2 item 6's own "cost by model" chart. Grouped in SQL
-- (never a bounded raw-turn fetch, mirroring
-- ListSessionOutcomeCountsInWindow's own identical reasoning,
-- queries/sessions.sql) -- the result set is bounded by the number of
-- distinct models this deployment has ever dispatched, not by turn
-- volume. COALESCE(model_id, 'unknown') buckets a turn whose own
-- model_id was never recorded (every turn created before migration
-- 000018, or a call site that legitimately never set it) into an
-- explicit "unknown" label rather than silently dropping it from the
-- chart -- the bars sum to EXACTLY the "Cost" KPI tile's own
-- total_cost_usd this way, never a smaller, unexplained partial sum.
-- Sorted by spend descending, matching the mockup's own horizontal-bar
-- ordering.
--
-- Bounded by turns_cost_created_at_idx, the SAME partial index
-- GetPlatformCostSummaryInWindow uses.
SELECT
    COALESCE(model_id, 'unknown') AS model_id,
    SUM(cost_usd)::numeric(14, 6) AS total_cost_usd
FROM turns
WHERE cost_usd IS NOT NULL AND created_at >= $1
GROUP BY COALESCE(model_id, 'unknown')
ORDER BY total_cost_usd DESC;

-- name: RequestStopOpenTurns :many
-- A person's stop request (technical plan §3.3, migrations/000151): flags
-- every turn of the session open at this instant, in the transaction that
-- flags the session and arms its stop timer, under the session's
-- actor-epoch lock. A turn already flagged keeps its first instant, so the
-- grace a turn in flight gets runs from the first request. The actor
-- decides what each flag means; nothing here moves a turn.
UPDATE turns
SET stop_requested_at = COALESCE(stop_requested_at, now())
WHERE session_id = $1 AND status IN ('pending', 'dispatched', 'processing')
RETURNING id;

-- name: ListStopRequestedOpenTurns :many
-- The session's flagged turns still open, oldest first, for the actor's
-- stop timer. grace_elapsed compares the flag with now() on the database's
-- own clock -- the clock that wrote it, and the one the timer pump compares
-- fires_at with -- so neither the decision between sending the sandbox
-- `stop` and cancelling, nor the instant the timer is re-armed for
-- (stop_requested_at plus the grace), depends on the skew between the
-- database and this replica. dispatched_sandbox_gen tells the handler
-- whether cancelling a turn in flight would retire the sandbox's current
-- gen (sessionactor's deliveryHold and retireStoppedGen).
SELECT
    id,
    status,
    stop_requested_at,
    dispatched_sandbox_gen,
    COALESCE(stop_requested_at <= now() - make_interval(secs => sqlc.arg('grace_seconds')::float8), false)::boolean AS grace_elapsed
FROM turns
WHERE session_id = sqlc.arg('session_id')
  AND stop_requested_at IS NOT NULL
  AND status IN ('pending', 'dispatched', 'processing')
ORDER BY created_at ASC, id ASC;

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// TurnStore is a thin, pass-through wrapper around the sqlc-generated turn
// queries (§4.3 TurnStore). No caching, no retries, no business rules —
// that lives in domain/turn (§3.1) and app/sessionactor (§2).
type TurnStore struct {
	q *sqlcgen.Queries
	// bound reports whether q runs on a transaction (WithTx) rather than
	// the pool: CreateAndArmDispatch refuses to run on the pool.
	bound bool
}

// NewTurnStore builds a TurnStore backed by pool.
func NewTurnStore(pool *pgxpool.Pool) *TurnStore {
	return &TurnStore{q: sqlcgen.New(pool)}
}

// WithTx returns a TurnStore whose queries run on tx instead of the pool
// this store was built with — used by app/sessionactor's transactional-
// write helper (§2).
func (s *TurnStore) WithTx(tx pgx.Tx) *TurnStore {
	return &TurnStore{q: s.q.WithTx(tx), bound: true}
}

// Create inserts a new turn row and returns it, and nothing else. The
// database rejects a second concurrent 'processing' turn for the same
// session via the turns_one_processing_per_session partial unique index
// (§3.3). It arms no dispatch timer, so production code never calls it:
// every turn a path creates goes through CreateAndArmDispatch, and
// TestEveryTurnInsertArmsTheDispatchTimer fails for a CreateTurnParams
// value built anywhere else. Tests keep it to seed turns in any state.
func (s *TurnStore) Create(ctx context.Context, arg sqlcgen.CreateTurnParams) (sqlcgen.Turn, error) {
	return s.q.CreateTurn(ctx, arg)
}

// ErrTurnOutsideTransaction is CreateAndArmDispatch's answer on a store
// that is not bound to a transaction: a turn and its dispatch timer commit
// together or not at all, which two autocommit statements cannot promise.
var ErrTurnOutsideTransaction = errors.New("postgres: a turn is created only inside a transaction, with its dispatch timer")

// ErrTurnNotPending is CreateAndArmDispatch's answer for a turn that would
// not be created pending. A turn ends only through UpdateStatus, whose
// terminal writes the session actor notes so the re-review debounce wakes
// in the same transaction (technical plan §24.9); a turn inserted already
// terminal would end with no wake-up at all. Every production caller
// creates a pending turn.
var ErrTurnNotPending = errors.New("postgres: a turn is created pending")

// ErrTurnNotAdmitted is CreateAndArmDispatch's answer for a turn the
// session guard did not admit: admitted is the zero Admission, or one the
// guard minted for another session. Every turn production code creates
// passes the guard first (technical plan §40.1, internal/domain/
// sessionguard): an Admission is minted only by sessionguard.Decide,
// which internal/app/turnguard calls under the session-row lock, or by
// sessionguard.AdmitNewSession for the first turn of a session created in
// the same transaction.
var ErrTurnNotAdmitted = errors.New("postgres: a turn is created only once the session guard admitted it")

// CreateAndArmDispatch is the one way production code creates a turn
// (technical plan §2, §3.3): it inserts the turn and, in the same
// transaction, arms the session's dispatch timer due at once on the
// database's clock (ArmSessionDispatchTimer). The caller commits both, and
// after its commit asks the session's actor to plan a dispatch; when that
// trigger fails -- this replica cannot host the actor (actor_unavailable),
// another replica hosts it (actor_elsewhere), the actor has stopped, or
// the replica dies first -- the timer pump delivers the same evaluation on
// whichever replica claims the timer, and again after each claim window.
// The actor deletes the timer at the start of every dispatch evaluation
// (sessionactor's planDispatch), so after a trigger that succeeded it is
// gone within that evaluation. Refuses with ErrTurnNotAdmitted unless
// admitted admits arg.SessionID -- the session guard's proof that the
// session may take this turn (technical plan §40.1) -- with
// ErrTurnOutsideTransaction on a store not built by WithTx, and with
// ErrTurnNotPending for a turn whose status is not pending, writing nothing
// in any case.
//
// A caller writing to a session that already exists holds the session's
// actor-epoch row lock (SessionStore.GetActorEpochForUpdate) in the same
// transaction, as every writer outside the actor does: the actor's own
// transactions take that lock first, so an evaluation either sees this
// turn or runs after its timer was armed. A caller creating the session in
// the same transaction needs no lock: nothing else can see the session
// before it commits.
func (s *TurnStore) CreateAndArmDispatch(ctx context.Context, arg sqlcgen.CreateTurnParams, admitted sessionguard.Admission) (sqlcgen.Turn, error) {
	if !arg.SessionID.Valid || !admitted.Admits(arg.SessionID.Bytes) {
		return sqlcgen.Turn{}, ErrTurnNotAdmitted
	}
	if !s.bound {
		return sqlcgen.Turn{}, ErrTurnOutsideTransaction
	}
	if arg.Status != sqlcgen.TurnStatusPending {
		return sqlcgen.Turn{}, fmt.Errorf("%w: status %q", ErrTurnNotPending, arg.Status)
	}
	created, err := s.q.CreateTurn(ctx, arg)
	if err != nil {
		return sqlcgen.Turn{}, err
	}
	if err := s.q.ArmSessionDispatchTimer(ctx, arg.SessionID); err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("postgres: arm the dispatch timer: %w", err)
	}
	return created, nil
}

// TurnAdmit is the session guard's admission of a turn, run by
// LockedTurnCreator.CreateLockedTurn in its own transaction, after the
// session's row lock and before the insert, so the guard reads the
// session's spend under that lock (internal/app/turnguard's Admitter). A
// refusal is returned as its error, a *sessionguard.Refusal.
type TurnAdmit func(ctx context.Context, tx pgx.Tx) (sessionguard.Admission, error)

// LockedTurnCreator creates a turn in a transaction of its own, under the
// session's actor-epoch row lock (SessionStore.GetActorEpochForUpdate) --
// the lock REST takes to insert a turn -- through CreateAndArmDispatch, so
// the turn and its dispatch timer commit together. It serves a caller with
// no transaction of its own to join: the release composition review
// (internal/app/releasereview), which runs on the release manifest worker.
type LockedTurnCreator struct {
	pool *pgxpool.Pool
}

// NewLockedTurnCreator builds a LockedTurnCreator on pool.
func NewLockedTurnCreator(pool *pgxpool.Pool) *LockedTurnCreator {
	return &LockedTurnCreator{pool: pool}
}

// CreateLockedTurn begins a transaction, locks arg.SessionID's
// actor-epoch row, asks admit whether the session may take the turn,
// creates the turn and arms its dispatch timer (CreateAndArmDispatch), and
// commits. pgx.ErrNoRows when the session does not exist; admit's error,
// a refusal included, as it returned it; nothing is written on any error.
func (c *LockedTurnCreator) CreateLockedTurn(ctx context.Context, arg sqlcgen.CreateTurnParams, admit TurnAdmit) (sqlcgen.Turn, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("postgres: begin the turn transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(c.pool).WithTx(tx)
	if _, err := q.GetSessionActorEpochForUpdate(ctx, arg.SessionID); err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("postgres: lock the session's actor epoch: %w", err)
	}
	if admit == nil {
		return sqlcgen.Turn{}, ErrTurnNotAdmitted
	}
	admission, err := admit(ctx, tx)
	if err != nil {
		return sqlcgen.Turn{}, err
	}
	created, err := (&TurnStore{q: q, bound: true}).CreateAndArmDispatch(ctx, arg, admission)
	if err != nil {
		return sqlcgen.Turn{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("postgres: commit the turn transaction: %w", err)
	}
	return created, nil
}

// Get fetches a turn by id.
func (s *TurnStore) Get(ctx context.Context, id pgtype.UUID) (sqlcgen.Turn, error) {
	return s.q.GetTurn(ctx, id)
}

// ListForSession fetches the full turn history for a session, oldest
// first — the input shape domain/session.DeriveStatus requires.
func (s *TurnStore) ListForSession(ctx context.Context, sessionID pgtype.UUID) ([]sqlcgen.Turn, error) {
	return s.q.ListTurnsForSession(ctx, sessionID)
}

// ExistsNewerReviewAttempt reports whether ANY genuine review attempt
// (is_review_attempt = true) in sessionID's own turn history is strictly
// newer than afterCreatedAt — see ExistsNewerReviewAttempt's own
// generated doc comment (queries/turns.sql) for the full "why". The one
// caller, internal/app/reviewverdict.HasNewerReviewAttempt (finding F1,
// adversarial review, §21.1b), supplies afterCreatedAt from the ACCEPTED
// attempt's own turns.created_at (this store's own Get, above).
func (s *TurnStore) ExistsNewerReviewAttempt(ctx context.Context, sessionID pgtype.UUID, afterCreatedAt pgtype.Timestamptz) (bool, error) {
	return s.q.ExistsNewerReviewAttempt(ctx, sqlcgen.ExistsNewerReviewAttemptParams{
		SessionID: sessionID,
		CreatedAt: afterCreatedAt,
	})
}

// NewestReviewAttempt returns sessionID's newest genuine review attempt
// (is_review_attempt = true): its id, state and creation time -- see
// GetNewestReviewAttempt's own generated doc comment (queries/turns.sql).
// pgx.ErrNoRows means the session has run no review attempt. Its caller is
// row 182's result (httpapi.GetSessionResult, technical plan §43.20).
func (s *TurnStore) NewestReviewAttempt(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.GetNewestReviewAttemptRow, error) {
	return s.q.GetNewestReviewAttempt(ctx, sessionID)
}

// UpdateStatus sets a turn's status, plus dispatched_at/completed_at when
// the caller supplies one (see UpdateTurnStatusParams' generated doc for
// the COALESCE semantics). It is the one write of turns.status, and the
// session actor reaches it only through its recorder
// (sessionactor's turnWrites), which notes a terminal write so the
// re-review debounce wakes in the same transaction (technical plan §24.9);
// TestTurnStatusWritesGoThroughTheRecorder keeps every caller there.
func (s *TurnStore) UpdateStatus(ctx context.Context, arg sqlcgen.UpdateTurnStatusParams) (sqlcgen.Turn, error) {
	return s.q.UpdateTurnStatus(ctx, arg)
}

// ReviewAttemptToCheck reads sessionID's next turn to dispatch as
// technical plan §24.9's context check reads it: whether it is a review
// attempt and which lane asked for it, the head and context it recorded,
// whether it waited behind another turn, and whether the session's sandbox
// can take it now. pgx.ErrNoRows: nothing to dispatch. See
// GetReviewAttemptToCheck's generated doc comment.
func (s *TurnStore) ReviewAttemptToCheck(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.GetReviewAttemptToCheckRow, error) {
	return s.q.GetReviewAttemptToCheck(ctx, sessionID)
}

// SetContextUnconfirmed records that the pending turn id starts with its
// context unconfirmed (technical plan §24.9), and reports the rows it
// wrote: 0 once the turn is no longer pending.
func (s *TurnStore) SetContextUnconfirmed(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.SetTurnContextUnconfirmed(ctx, id)
}

// ReviewRetriggerHeld reports whether some turn of sessionID is still open
// -- pending, dispatched or processing -- so the re-review debounce's fire
// holds instead of inserting an automatic review (technical plan §24.9).
// Meaningful on a store built via WithTx, under the session's actor-epoch
// row lock, which every turn insert on an existing session also takes. See
// ReviewRetriggerHeld's doc comment in queries/turns.sql.
func (s *TurnStore) ReviewRetriggerHeld(ctx context.Context, sessionID pgtype.UUID) (bool, error) {
	return s.q.ReviewRetriggerHeld(ctx, sessionID)
}

// MarkProgressNotified atomically sets id's own progress_notified_at to
// now, but ONLY if it is still NULL -- an audit-fix batch's own addition
// (finding M16, "completeness") -- see MarkTurnProgressNotified's own
// generated doc comment (sqlcgen/turns.sql.go, sourced from queries/
// turns.sql) for the full race this guards against. Returns the number of
// rows actually updated (0 or 1): 0 means this turn already had its
// progress milestone fired by an earlier call.
func (s *TurnStore) MarkProgressNotified(ctx context.Context, id pgtype.UUID, now pgtype.Timestamptz) (int64, error) {
	return s.q.MarkTurnProgressNotified(ctx, sqlcgen.MarkTurnProgressNotifiedParams{
		ID:                 id,
		ProgressNotifiedAt: now,
	})
}

// GetProcessingTurnForSession fetches sessionID's own currently-live
// (status='processing') turn, if any (§20.2) -- mirrors
// WorkflowStore.GetRunningRunForSession's identical "resolve the caller's
// own live attempt from a session id alone" role, one layer down (a turn,
// not a workflow run). turns_one_processing_per_session (migrations/
// 000005_turns.up.sql) guarantees at most one row can ever match; a caller
// with no processing turn gets pgx.ErrNoRows, exactly like GetTurn's own
// not-found case.
func (s *TurnStore) GetProcessingTurnForSession(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Turn, error) {
	return s.q.GetProcessingTurnForSession(ctx, sessionID)
}

// GetByDispatchedMessageID (finding F3 (§21.1's amendment)) resolves the SPECIFIC
// turn a verdict-posting request actually originated from, by the
// sandboxws.Prompt MessageId that request's own header presents --
// mirrors GetProcessingTurnForSession's own "no turn id named at all"
// shape one field further: still resolved from the sandbox-authenticated
// session id alone, but now scoped to the ONE dispatch this specific
// request is provably a reply to, never to session-wide "current" status.
// A turn already marked terminal (e.g. 'failed' via timeout) is still
// found here -- deliberately: that is the exact case this method exists
// to still resolve correctly (GetTurnByDispatchedMessageID's own doc
// comment, sqlcgen/turns.sql.go). No matching row is pgx.ErrNoRows,
// exactly like GetProcessingTurnForSession's own not-found case.
func (s *TurnStore) GetByDispatchedMessageID(ctx context.Context, sessionID pgtype.UUID, dispatchedMessageID string) (sqlcgen.Turn, error) {
	return s.q.GetTurnByDispatchedMessageID(ctx, sqlcgen.GetTurnByDispatchedMessageIDParams{
		SessionID:           sessionID,
		DispatchedMessageID: &dispatchedMessageID,
	})
}

// NextDispatchedEventID returns the upper bound of turnID's sub-task trace
// (§26.6's amendment, GetNextTurnDispatchedEventID's doc comment in
// queries/turns.sql): the lowest dispatched_event_id among sessionID's
// other turns at or above dispatchedEventID, turnID's own. found is false
// when no other turn was dispatched at or after it -- the trace then has
// no upper bound. A returned value equal to dispatchedEventID means
// another turn shares the watermark, and the event log cannot tell the
// two turns' events apart.
func (s *TurnStore) NextDispatchedEventID(ctx context.Context, sessionID, turnID pgtype.UUID, dispatchedEventID int64) (next int64, found bool, err error) {
	got, err := s.q.GetNextTurnDispatchedEventID(ctx, sqlcgen.GetNextTurnDispatchedEventIDParams{
		SessionID:         sessionID,
		ID:                turnID,
		DispatchedEventID: dispatchedEventID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if got == nil {
		// Unreachable: the WHERE clause compares the column, so a NULL
		// never matches. Treated as no bound found would widen the read,
		// so it is an error instead.
		return 0, false, errors.New("postgres: next turn dispatched_event_id is NULL")
	}
	return *got, true, nil
}

// EarlierTurnLeftRunning reports whether sessionID holds a turn other than
// turnID, dispatched to sandbox gen before dispatchedEventID (turnID's own
// watermark), that ended without its own execution_complete -- so its
// agent may still be running at that gen (§26.6's amendment,
// ExistsEarlierTurnLeftRunning's doc comment in queries/turns.sql).
func (s *TurnStore) EarlierTurnLeftRunning(ctx context.Context, sessionID, turnID pgtype.UUID, gen int32, dispatchedEventID int64) (bool, error) {
	return s.q.ExistsEarlierTurnLeftRunning(ctx, sqlcgen.ExistsEarlierTurnLeftRunningParams{
		SessionID:         sessionID,
		ID:                turnID,
		Gen:               gen,
		DispatchedEventID: dispatchedEventID,
	})
}

// SetEpistemicOutcome is the guarded write backing the epistemic-outcome-
// posting endpoint (§20.2) -- mirrors WorkflowStore.
// SetStepRunOutcome's own "guarded UPDATE, observed via :execrows" idiom
// exactly, one status value over (turns.status = 'processing' rather than
// workflow_step_runs.status = 'running'). Returns the number of rows
// actually updated (0 or 1): 0 means id is no longer the live processing
// turn (a race between this endpoint's own GetProcessingTurnForSession
// read and this write -- the turn completed/failed/was cancelled in
// between).
func (s *TurnStore) SetEpistemicOutcome(ctx context.Context, id pgtype.UUID, outcome sqlcgen.TurnEpistemicOutcome) (int64, error) {
	return s.q.SetTurnEpistemicOutcome(ctx, sqlcgen.SetTurnEpistemicOutcomeParams{
		ID:               id,
		EpistemicOutcome: &outcome,
	})
}

// RecordStepCostUSD adds one step's cost onto sessionID's currently
// processing turn, exactly once per stepID (§25.15) -- see
// RecordTurnStepCost's own generated doc comment (sqlcgen/turns.sql.go,
// sourced from queries/turns.sql) for why the whole thing is one
// statement, and migrations/000099_turn_step_costs.up.sql for why the
// idempotency key is the step id rather than the raw event row's own
// insert flag.
//
// Returns the number of turns rows actually updated (0 or 1). 0 means
// EITHER this stepID was already counted (a redelivery) OR sessionID has
// no turn currently processing; the two are deliberately not
// distinguished here, because both are states where adding the money a
// second time would be the worse error.
func (s *TurnStore) RecordStepCostUSD(ctx context.Context, sessionID pgtype.UUID, stepID string, amountUSD float64) (int64, error) {
	return s.q.RecordTurnStepCost(ctx, sqlcgen.RecordTurnStepCostParams{
		SessionID: sessionID,
		StepID:    stepID,
		CostUsd:   Float64ToNumeric(&amountUSD),
	})
}

// GetPlatformCostSummaryInWindow returns sinceTime's own platform-wide
// cost summary (total + median-per-session, sample size) -- §12.2 item
// 6's own "Cost" KPI tile. See GetPlatformCostSummaryInWindow's own generated
// doc comment (sqlcgen/turns.sql.go, sourced from queries/turns.sql) for
// the full definition.
func (s *TurnStore) GetPlatformCostSummaryInWindow(ctx context.Context, sinceTime pgtype.Timestamptz) (sqlcgen.GetPlatformCostSummaryInWindowRow, error) {
	return s.q.GetPlatformCostSummaryInWindow(ctx, sinceTime)
}

// ListCostByModelInWindow returns sinceTime's own per-model cost
// breakdown, spend descending -- §12.2 item 6's own "cost by model" chart.
func (s *TurnStore) ListCostByModelInWindow(ctx context.Context, sinceTime pgtype.Timestamptz) ([]sqlcgen.ListCostByModelInWindowRow, error) {
	return s.q.ListCostByModelInWindow(ctx, sinceTime)
}

// RequestStopOpen flags every turn of the session open at this instant with
// a person's stop request (technical plan §3.3) and returns their ids. It
// moves no turn: the session's actor does, through §3.3's cancel
// transition. Meaningful only on a store built via WithTx, under the
// session's GetActorEpochForUpdate lock.
func (s *TurnStore) RequestStopOpen(ctx context.Context, sessionID pgtype.UUID) ([]pgtype.UUID, error) {
	return s.q.RequestStopOpenTurns(ctx, sessionID)
}

// ListStopRequestedOpen returns the session's flagged turns still open,
// oldest first, each with whether grace has passed since its flag on the
// database's own clock.
func (s *TurnStore) ListStopRequestedOpen(ctx context.Context, sessionID pgtype.UUID, grace time.Duration) ([]sqlcgen.ListStopRequestedOpenTurnsRow, error) {
	return s.q.ListStopRequestedOpenTurns(ctx, sqlcgen.ListStopRequestedOpenTurnsParams{
		GraceSeconds: grace.Seconds(),
		SessionID:    sessionID,
	})
}

// SetPromptReceiptRequest records, in the transaction of the dispatch that
// just stamped id's dispatched_message_id, whether that dispatch asked its
// sandbox for a prompt receipt (technical plan §3.3, prompt receipts):
// messageID is that dispatched_message_id when it asked and nil when it
// did not, which clears an earlier request; readySeq is the sandbox's
// ready_seq at the dispatch. See SetTurnPromptReceiptRequest's own
// generated doc comment.
func (s *TurnStore) SetPromptReceiptRequest(ctx context.Context, id pgtype.UUID, messageID *string, readySeq int32) error {
	return s.q.SetTurnPromptReceiptRequest(ctx, sqlcgen.SetTurnPromptReceiptRequestParams{
		MessageID: messageID,
		ReadySeq:  readySeq,
		ID:        id,
	})
}

// RecordCheckoutRequest records, in the dispatch evaluation's own
// transaction, the checkout command about to be sent to id's sandbox gen
// (technical plan §21.1, §30.4): messageID, gen, and the gen's ready_seq at
// the send. afterFailure counts the reply this request answers as failed.
// Reports the rows it wrote: 0 once the turn is no longer open, and then
// nothing is to be sent. See RecordTurnCheckoutRequest's own generated doc
// comment.
func (s *TurnStore) RecordCheckoutRequest(ctx context.Context, id pgtype.UUID, gen int32, messageID string, readySeq int32, afterFailure bool) (int64, error) {
	return s.q.RecordTurnCheckoutRequest(ctx, sqlcgen.RecordTurnCheckoutRequestParams{
		Gen: gen, AfterFailure: afterFailure, MessageID: messageID, ReadySeq: readySeq, ID: id,
	})
}

// CheckoutState reads the facts a review turn's checkout is decided on:
// its latest request, how long ago the bound and the latest send started
// on the database's clock, and the stored reply, nil when none is. See
// GetTurnCheckoutState's own generated doc comment.
func (s *TurnStore) CheckoutState(ctx context.Context, id pgtype.UUID) (sqlcgen.GetTurnCheckoutStateRow, error) {
	return s.q.GetTurnCheckoutState(ctx, id)
}

// SetCheckedOut records sha as the commit id's sandbox reported holding,
// in the commit that dispatches id, and reports the rows it wrote: 0 once
// the turn is no longer open.
func (s *TurnStore) SetCheckedOut(ctx context.Context, id pgtype.UUID, sha string) (int64, error) {
	return s.q.SetTurnCheckedOut(ctx, sqlcgen.SetTurnCheckedOutParams{Sha: sha, ID: id})
}

// SetCheckoutRetiredGen records gen as the one id's failed checkouts
// retired, so the turn retires no other, and reports the rows it wrote: 0
// once the turn is no longer open.
func (s *TurnStore) SetCheckoutRetiredGen(ctx context.Context, id pgtype.UUID, gen int32) (int64, error) {
	return s.q.SetTurnCheckoutRetiredGen(ctx, sqlcgen.SetTurnCheckoutRetiredGenParams{Gen: gen, ID: id})
}

// PromptReceiptState reports whether the receipt of id's current dispatch
// is stored, by its deterministic key, and how long ago that dispatch
// asked for it, on the database's clock. ok is false when the dispatch
// asked for no receipt.
func (s *TurnStore) PromptReceiptState(ctx context.Context, id pgtype.UUID) (receiptStored bool, sinceRequest time.Duration, ok bool, err error) {
	row, err := s.q.GetTurnPromptReceiptState(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, false, nil
	}
	if err != nil {
		return false, 0, false, err
	}
	return row.ReceiptStored, time.Duration(row.SinceRequestNanos), true, nil
}

// MarkPromptReconnectAnswered claims the sandbox ready numbered readySeq
// for id's prompt-receipt check: it moves receipt_checked_ready_seq from
// checkedReadySeq to readySeq while id is still Processing, unflagged by a
// stop, on the dispatch messageID names, which asked for the receipt, with
// resendCount re-sends made so far -- and counts one more re-send when
// resend is true. Returns the rows moved, 0 or 1; 0 means the claim failed
// and nothing is to be sent. See MarkTurnPromptReconnectAnswered's own
// generated doc comment.
func (s *TurnStore) MarkPromptReconnectAnswered(ctx context.Context, id pgtype.UUID, messageID string, checkedReadySeq, readySeq, resendCount int32, resend bool) (int64, error) {
	var add int32
	if resend {
		add = 1
	}
	return s.q.MarkTurnPromptReconnectAnswered(ctx, sqlcgen.MarkTurnPromptReconnectAnsweredParams{
		ReadySeq:        readySeq,
		Resend:          add,
		ID:              id,
		MessageID:       messageID,
		CheckedReadySeq: checkedReadySeq,
		ResendCount:     resendCount,
	})
}

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
// gone within that evaluation. Refuses with ErrTurnOutsideTransaction on a
// store not built by WithTx, writing nothing.
//
// A caller writing to a session that already exists holds the session's
// actor-epoch row lock (SessionStore.GetActorEpochForUpdate) in the same
// transaction, as every writer outside the actor does: the actor's own
// transactions take that lock first, so an evaluation either sees this
// turn or runs after its timer was armed. A caller creating the session in
// the same transaction needs no lock: nothing else can see the session
// before it commits.
func (s *TurnStore) CreateAndArmDispatch(ctx context.Context, arg sqlcgen.CreateTurnParams) (sqlcgen.Turn, error) {
	if !s.bound {
		return sqlcgen.Turn{}, ErrTurnOutsideTransaction
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
// actor-epoch row, creates the turn and arms its dispatch timer
// (CreateAndArmDispatch), and commits. pgx.ErrNoRows when the session does
// not exist; nothing is written on any error.
func (c *LockedTurnCreator) CreateLockedTurn(ctx context.Context, arg sqlcgen.CreateTurnParams) (sqlcgen.Turn, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("postgres: begin the turn transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(c.pool).WithTx(tx)
	if _, err := q.GetSessionActorEpochForUpdate(ctx, arg.SessionID); err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("postgres: lock the session's actor epoch: %w", err)
	}
	created, err := (&TurnStore{q: q, bound: true}).CreateAndArmDispatch(ctx, arg)
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
// the COALESCE semantics).
func (s *TurnStore) UpdateStatus(ctx context.Context, arg sqlcgen.UpdateTurnStatusParams) (sqlcgen.Turn, error) {
	return s.q.UpdateTurnStatus(ctx, arg)
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
// stop, on the dispatch messageID names, which asked for the receipt.
// Returns the rows moved, 0 or 1; 0 means the claim failed and nothing is
// to be sent. See MarkTurnPromptReconnectAnswered's own generated doc
// comment.
func (s *TurnStore) MarkPromptReconnectAnswered(ctx context.Context, id pgtype.UUID, messageID string, checkedReadySeq, readySeq int32) (int64, error) {
	return s.q.MarkTurnPromptReconnectAnswered(ctx, sqlcgen.MarkTurnPromptReconnectAnsweredParams{
		ReadySeq:        readySeq,
		ID:              id,
		MessageID:       messageID,
		CheckedReadySeq: checkedReadySeq,
	})
}

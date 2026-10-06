package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// OutboxStore is a thin, pass-through wrapper around the sqlc-generated
// outbox queries (§4.3 Outbox, §5.1 outbox pattern). No caching, no
// retries, no business rules -- the claim/attempt/record delivery loop
// lives in internal/app/outboxworker (§5.1, "outbox delivery"). The one
// deliberate exception is Create's own §30.8 epoch stamp (outbox_shadow.
// go) -- necessarily here, not in app/outboxworker, because it must be
// computed and persisted in the SAME statement as the enqueue itself
// (§30.6: "a single choke point for every enqueue site"), the same
// reason RenewClaim's own compare-and-swap logic (H6) already lives at
// this layer rather than one level up.
type OutboxStore struct {
	q *sqlcgen.Queries

	// platformShadow is platform.Config.ShadowMode (NARVI_SHADOW_MODE,
	// §30.8), read once at process boot and passed straight through here
	// -- never re-read from the environment per call. Consulted by
	// Create/ResolveEffectiveMode (outbox_shadow.go) whenever a row's own
	// session names no single repo to check repo_settings.
	// live_egress_enabled against.
	platformShadow bool
}

// NewOutboxStore builds an OutboxStore backed by pool. platformShadow is
// §30.8's own deployment-level master switch (platform.Config.ShadowMode)
// -- a mandatory argument, not a convenience default, so a construction
// site cannot silently forget it (mirrors shadowscm.New's own "every
// argument required" idiom): every existing call site before this Step
// passes false, preserving today's behavior exactly (nothing consulted
// this bit before now).
func NewOutboxStore(pool *pgxpool.Pool, platformShadow bool) *OutboxStore {
	return &OutboxStore{q: sqlcgen.New(pool), platformShadow: platformShadow}
}

// WithTx returns an OutboxStore whose queries run on tx instead of the
// pool this store was built with — mirrors the same WithTx convention
// every other store in this package already follows (e.g. EventStore).
// platformShadow carries over unchanged: it is a deployment-wide
// constant, never transaction-scoped.
func (s *OutboxStore) WithTx(tx pgx.Tx) *OutboxStore {
	return &OutboxStore{q: s.q.WithTx(tx), platformShadow: s.platformShadow}
}

// Create inserts a new outbox entry and returns it. arg.SuppressedInShadow
// is OVERWRITTEN here, unconditionally, with this call's own freshly-
// resolved §30.8 stamp (ResolveEffectiveMode, outbox_shadow.go) -- never
// left to whatever a caller happened to set (or, far more likely, leave
// at its own zero value) on the struct literal, so no enqueue call site
// can ever forget or get this wrong: the single choke point §30.6 names.
func (s *OutboxStore) Create(ctx context.Context, arg sqlcgen.CreateOutboxEntryParams) (sqlcgen.Outbox, error) {
	arg.SuppressedInShadow = s.ResolveEffectiveMode(ctx, arg.SessionID)
	return s.q.CreateOutboxEntry(ctx, arg)
}

// Get fetches an outbox entry by id.
func (s *OutboxStore) Get(ctx context.Context, id pgtype.UUID) (sqlcgen.Outbox, error) {
	return s.q.GetOutboxEntry(ctx, id)
}

// ListDuePending returns up to limit 'pending' rows eligible to (re)attempt
// delivery now, locked FOR UPDATE SKIP LOCKED -- callers MUST run this
// inside the same transaction that subsequently calls Claim on each
// returned row (see ListDuePendingOutboxEntries's own generated doc
// comment).
func (s *OutboxStore) ListDuePending(ctx context.Context, limit int32) ([]sqlcgen.Outbox, error) {
	return s.q.ListDuePendingOutboxEntries(ctx, limit)
}

// ListDuePendingOfKinds is ListDuePending restricted to the rows of kinds:
// one lane of a tick while the autonomy freeze holds some kinds (technical
// plan §40.2), so each held row is still claimed and its hold decided at
// the delivery's call site, and no notification waits behind held rows.
// Same transaction rule.
func (s *OutboxStore) ListDuePendingOfKinds(ctx context.Context, limit int32, kinds []string) ([]sqlcgen.Outbox, error) {
	return s.q.ListDuePendingOutboxEntriesOfKinds(ctx, sqlcgen.ListDuePendingOutboxEntriesOfKindsParams{Kinds: kinds, MaxRows: limit})
}

// Claim bumps id's row next_attempt_at forward by claimProtection (a
// provisional protection window, see ClaimOutboxEntry's own generated doc
// comment for why the outbox table needs this rather than a distinct
// in-flight status) and increments attempts -- the commit-before-the-
// real-notifier-call half of outboxworker.Builder's own two-step shape.
func (s *OutboxStore) Claim(ctx context.Context, id pgtype.UUID, nextAttemptAt pgtype.Timestamptz) (sqlcgen.Outbox, error) {
	return s.q.ClaimOutboxEntry(ctx, sqlcgen.ClaimOutboxEntryParams{
		ID:            id,
		NextAttemptAt: nextAttemptAt,
	})
}

// RenewClaim renews id's row own claim-protection window (bumping
// next_attempt_at forward to nextAttemptAt, WITHOUT incrementing
// attempts) -- audit fix H6's per-row heartbeat, called by
// outboxworker.Builder's own attempt() immediately before the real
// notifier call, from a FRESH time.Now() at that moment rather than
// claimBatch's own shared, batch-level claim-time now.
//
// expectedNextAttemptAt is a genuine optimistic-concurrency compare-and-
// swap value, not a mere status check: the next_attempt_at the CALLER
// last observed for this row (i.e. row.NextAttemptAt as returned by its
// own prior Claim/RenewClaim call). The renewal only succeeds if the
// row's CURRENT next_attempt_at still matches it -- see
// RenewOutboxClaim's own generated doc comment for why "status =
// 'pending'" alone cannot distinguish "untouched since I last observed
// it" from "a different builder already re-claimed/renewed it and is now
// mid-delivery". Returns pgx.ErrNoRows if id's row is no longer 'pending'
// OR its next_attempt_at no longer matches expectedNextAttemptAt (a
// different builder won the race in between) -- both cases mean this
// caller must not proceed to deliver.
func (s *OutboxStore) RenewClaim(ctx context.Context, id pgtype.UUID, nextAttemptAt, expectedNextAttemptAt pgtype.Timestamptz) (sqlcgen.Outbox, error) {
	return s.q.RenewOutboxClaim(ctx, sqlcgen.RenewOutboxClaimParams{
		ID:                    id,
		NextAttemptAt:         nextAttemptAt,
		ExpectedNextAttemptAt: expectedNextAttemptAt,
	})
}

// CountPending returns the number of 'pending' outbox rows total,
// deliberately NOT restricted to next_attempt_at <= now() -- audit fix
// M15/M17's own outbox_due_backlog_count gauge query, run standalone
// (outside claimBatch's own transaction) once per outboxworker.Builder
// pump tick.
func (s *OutboxStore) CountPending(ctx context.Context) (int64, error) {
	return s.q.CountPendingOutboxEntries(ctx)
}

// ListDeadLetter returns up to limit 'dead_letter' outbox rows, most-
// recently-created first -- §16's own needs_attention row source (see
// ListDeadLetterOutboxEntries' own generated doc comment for the full
// design, including why this orders by created_at rather than a "became
// dead-lettered at" instant this table does not carry).
func (s *OutboxStore) ListDeadLetter(ctx context.Context, limit int32) ([]sqlcgen.Outbox, error) {
	return s.q.ListDeadLetterOutboxEntries(ctx, limit)
}

// MarkDelivered records a successful delivery. Returns pgx.ErrNoRows if
// id's row is no longer 'pending' (an already-superseded/stale outcome --
// see MarkOutboxEntryDelivered's own generated doc comment).
func (s *OutboxStore) MarkDelivered(ctx context.Context, id pgtype.UUID) (sqlcgen.Outbox, error) {
	return s.q.MarkOutboxEntryDelivered(ctx, id)
}

// HoldNew makes id's row, just created in this store's transaction and
// never attempted, due only wait from the database's now
// (HoldNewOutboxEntry). pgx.ErrNoRows when the row is not pending or was
// attempted.
func (s *OutboxStore) HoldNew(ctx context.Context, id pgtype.UUID, wait time.Duration) (sqlcgen.Outbox, error) {
	return s.q.HoldNewOutboxEntry(ctx, sqlcgen.HoldNewOutboxEntryParams{ID: id, WaitSeconds: wait.Seconds()})
}

// MarkDeliveredInPlace records that id's notice reached its channel by
// another way, so it is never delivered (MarkOutboxEntryDeliveredInPlace).
// pgx.ErrNoRows when the row is no longer pending or a builder already
// claimed it: its delivery then stands.
func (s *OutboxStore) MarkDeliveredInPlace(ctx context.Context, id pgtype.UUID) (sqlcgen.Outbox, error) {
	return s.q.MarkOutboxEntryDeliveredInPlace(ctx, id)
}

// RecordFailure records a failed delivery attempt still eligible for
// another retry: nextAttemptAt is the caller's own domain/outbox.
// EvaluateBackoff-computed value, lastError captures the notifier's own
// error for observability, and consecutiveInterruptions is the row's run of
// shutdown interruptions as domain/outbox.EvaluateFailure decided it (zero
// for an attempt that completed). Returns pgx.ErrNoRows if id's row is no
// longer 'pending', mirroring MarkDelivered's own identical guard.
func (s *OutboxStore) RecordFailure(ctx context.Context, id pgtype.UUID, nextAttemptAt pgtype.Timestamptz, lastError string, consecutiveInterruptions int32) (sqlcgen.Outbox, error) {
	return s.q.RecordOutboxEntryFailure(ctx, sqlcgen.RecordOutboxEntryFailureParams{
		ID:                       id,
		NextAttemptAt:            nextAttemptAt,
		LastError:                &lastError,
		ConsecutiveInterruptions: consecutiveInterruptions,
	})
}

// MarkDeadLetter records a failed delivery attempt that has exhausted
// domain/outbox.MaxAttempts, with the run of shutdown interruptions
// decided as for RecordFailure. Returns pgx.ErrNoRows if id's row is no
// longer 'pending', mirroring MarkDelivered/RecordFailure's own identical
// guard.
func (s *OutboxStore) MarkDeadLetter(ctx context.Context, id pgtype.UUID, lastError string, consecutiveInterruptions int32) (sqlcgen.Outbox, error) {
	return s.q.MarkOutboxEntryDeadLetter(ctx, sqlcgen.MarkOutboxEntryDeadLetterParams{
		ID:                       id,
		LastError:                &lastError,
		ConsecutiveInterruptions: consecutiveInterruptions,
	})
}

// Defer records a failure of the class that does not consume an attempt
// (domain/outbox.ClassDeferred, technical plan §5.1 and §44.2): gives back
// the attempt the claim counted and makes the row due again at
// nextAttemptAt. lastError nil leaves the row's last error as it was (a
// delivery that never started); consecutiveInterruptions is the decided
// run.
//
// expectedNextAttemptAt is a compare-and-swap value, exactly as for
// RenewClaim: the next_attempt_at this caller last observed for the row.
// Returns pgx.ErrNoRows if the row is no longer 'pending' OR another
// builder has re-claimed it since -- the attempt then stays counted,
// since taking it back would take back that builder's (see
// DeferOutboxEntry's own generated doc comment).
func (s *OutboxStore) Defer(ctx context.Context, id pgtype.UUID, nextAttemptAt, expectedNextAttemptAt pgtype.Timestamptz, lastError *string, consecutiveInterruptions int32) (sqlcgen.Outbox, error) {
	return s.q.DeferOutboxEntry(ctx, sqlcgen.DeferOutboxEntryParams{
		ID:                       id,
		NextAttemptAt:            nextAttemptAt,
		LastError:                lastError,
		ConsecutiveInterruptions: consecutiveInterruptions,
		ExpectedNextAttemptAt:    expectedNextAttemptAt,
	})
}

// GetLatestByKindPrefix returns the most-recently-created outbox row whose
// own kind begins with kindPrefix -- §12.5's own ("integrations read
// model & routes" amendment) GET /api/integrations, "what did Narvi last
// try to POST to this surface" (see GetLatestOutboxEntryByKindPrefix's
// own generated doc comment for the full "why a LIKE prefix match, and
// its own documented fragility" reasoning). Returns pgx.ErrNoRows
// (unwrapped) if no outbox row has ever matched this prefix -- "no
// outbound attempt on record for this provider yet", never a store
// error; callers must not treat it as one.
func (s *OutboxStore) GetLatestByKindPrefix(ctx context.Context, kindPrefix string) (sqlcgen.Outbox, error) {
	return s.q.GetLatestOutboxEntryByKindPrefix(ctx, kindPrefix)
}

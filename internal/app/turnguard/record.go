package turnguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionnotice"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// NoticeKinds are the outbox kinds the guard's one notice per crossing
// travels under, one a channel (sessionnotice.Enqueue).
var NoticeKinds = sessionnotice.Kinds{
	Slack:  ports.NotificationKindSlackSessionGuard,
	Linear: ports.NotificationKindLinearSessionGuard,
	GitHub: ports.NotificationKindGitHubSessionGuard,
}

// WarningMessageID is the message id of the warning a refusal records:
// derived from the crossing (sessionguard.WarningKey) -- the session, the
// reason, and the cap and where it was set -- so the events table's own
// (session_id, message_id) dedupe stores one warning per crossing however
// many turns it refuses, and a raised cap, a new crossing, records anew.
func WarningMessageID(r sessionguard.Refusal) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(sessionguard.WarningKey(r))).String()
}

// WarningAppender writes one session "warning" event, raw under messageID,
// in the caller's transaction and reports whether its row was new: the
// session actor passes its own appendRawEvent, which broadcasts the row
// once the transaction commits.
type WarningAppender func(ctx context.Context, messageID string, raw json.RawMessage) (bool, error)

// Bound is the guard bound to one transaction: what a caller that already
// holds the session's row lock in tx admits and records through
// (internal/app/workflowengine's Deps.Guard, the session actor).
type Bound struct {
	g      *Guard
	tx     pgx.Tx
	append WarningAppender
}

// WithTx binds g to tx. appendWarning is how a warning is written; nil
// writes it through the event store in tx, broadcast by nobody. A nil
// Guard binds to nil, whose every method answers ErrNoGuard.
func (g *Guard) WithTx(tx pgx.Tx, appendWarning WarningAppender) *Bound {
	if g == nil {
		return nil
	}
	return &Bound{g: g, tx: tx, append: appendWarning}
}

// Admit is Guard.Admit in the bound transaction.
func (b *Bound) Admit(ctx context.Context, sessionID pgtype.UUID, origin sessionguard.Origin, stage Stage) (sessionguard.Admission, *sessionguard.Refusal, error) {
	if b == nil {
		return sessionguard.Admission{}, nil, ErrNoGuard
	}
	return b.g.Admit(ctx, b.tx, sessionID, origin, stage)
}

// Recorded is what Record wrote: whether the crossing's warning was new,
// its payload, and whether a notice was enqueued with it.
type Recorded struct {
	WarningInserted bool
	Warning         json.RawMessage
	NoticeEnqueued  bool
}

// Record records r for sessionRow in the bound transaction: the session's
// persisted warning, at the crossing's message id (WarningMessageID), and,
// only when that warning is new and notify is true, one outbox notice to
// the session's own channel (sessionnotice.Enqueue, NoticeKinds) -- so a
// crossing is told once, whoever observes it first: a refused creation, a
// queued turn ended at dispatch, an automatic producer. A caller passes
// notify false when it already tells the crossing another way in the same
// transaction: a workflow run escalated with the guard's text. The warning
// carries the session's sandbox gen, 0 with no sandbox.
func (b *Bound) Record(ctx context.Context, sessionRow sqlcgen.Session, r *sessionguard.Refusal, notify bool) (Recorded, error) {
	if b == nil {
		return Recorded{}, ErrNoGuard
	}
	gen := 0
	if sandboxRow, err := b.g.sandboxes.WithTx(b.tx).Get(ctx, sessionRow.ID); err == nil {
		gen = int(sandboxRow.Gen)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Recorded{}, fmt.Errorf("turnguard: read the session's sandbox gen: %w", err)
	}
	msg := sandboxws.Warning{
		Type:      "warning",
		MessageId: WarningMessageID(*r),
		SessionId: sessionRow.ID.String(),
		Gen:       gen,
		Message:   sessionguard.Text(*r),
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return Recorded{}, fmt.Errorf("turnguard: marshal the warning: %w", err)
	}
	var inserted bool
	if b.append != nil {
		inserted, err = b.append(ctx, msg.MessageId, raw)
	} else {
		var row sqlcgen.CreateEventRow
		row, err = b.g.events.WithTx(b.tx).Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionRow.ID,
			Type:      "warning",
			MessageID: msg.MessageId,
			Payload:   raw,
		})
		inserted = row.Inserted
	}
	if err != nil {
		return Recorded{}, fmt.Errorf("turnguard: record the warning: %w", err)
	}
	out := Recorded{WarningInserted: inserted, Warning: raw}
	if !inserted || !notify {
		return out, nil
	}
	enqueued, err := sessionnotice.Enqueue(ctx, sessionnotice.Stores{
		SlackThreadSessions: b.g.slack.WithTx(b.tx),
		LinearAgentSessions: b.g.linear.WithTx(b.tx),
		GitHubPRSessions:    b.g.github.WithTx(b.tx),
		Outbox:              b.g.outbox.WithTx(b.tx),
	}, sessionRow, NoticeKinds, msg.Message)
	if err != nil {
		return Recorded{}, fmt.Errorf("turnguard: enqueue the notice: %w", err)
	}
	out.NoticeEnqueued = enqueued
	return out, nil
}

// RecordRefusal records r for sessionID outside the session actor, after
// the refusing transaction rolled back: a person's or bot's turn refused at
// creation, a plan approval or workflow decision refused, the composition
// review declined. In a transaction of its own it takes the session's row
// lock, records the warning and, when it is new, the notice (Bound.Record),
// commits, and broadcasts the warning to the session's live subscribers.
// Best effort: the refusal has been answered already, so a failure here is
// logged and changes nothing the caller returns. The caller must not hold
// the session's row lock in an open transaction of its own, or this waits
// for it.
func (g *Guard) RecordRefusal(ctx context.Context, sessionID pgtype.UUID, r *sessionguard.Refusal) {
	if g == nil || r == nil {
		return
	}
	logger := platform.Logger(ctx)
	recorded, err := g.recordRefusal(ctx, sessionID, r)
	if err != nil {
		logger.Error("turnguard: record a refusal's warning and notice failed; the refusal stands", "session_id", sessionID.String(), "reason", string(r.Reason), "error", err)
		return
	}
	if recorded.WarningInserted && g.broadcaster != nil {
		g.broadcaster.Broadcast(sessionID.String(), recorded.Warning)
	}
}

func (g *Guard) recordRefusal(ctx context.Context, sessionID pgtype.UUID, r *sessionguard.Refusal) (Recorded, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return Recorded{}, fmt.Errorf("turnguard: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := g.sessions.WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		return Recorded{}, fmt.Errorf("turnguard: lock the session: %w", err)
	}
	sessionRow, err := g.sessions.WithTx(tx).Get(ctx, sessionID)
	if err != nil {
		return Recorded{}, fmt.Errorf("turnguard: read the session: %w", err)
	}
	recorded, err := g.WithTx(tx, nil).Record(ctx, sessionRow, r, true)
	if err != nil {
		return Recorded{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Recorded{}, fmt.Errorf("turnguard: commit: %w", err)
	}
	return recorded, nil
}

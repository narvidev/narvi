package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ShadowSCMWriteStore is the append-only ledger of every suppressed
// customer-visible write this platform's shadow mode records (§30.6) --
// named for its first and largest source (the GitHub port decorator and
// transport gate's own SCM writes, internal/app/shadowscm) but shared, by
// design, with every other suppressed effect that has nowhere more
// specific to go: the Slack/Linear synchronous ingress writes (§30.3,
// internal/app/shadowslack/shadowlinear) and the shadow credential mint's
// own substitution/refusal records (§30.4) all write into this same
// table, through this same store. It has a Create and a List and
// deliberately no update or delete: a suppressed effect is a historical
// fact, and there is nothing to correct about the record of something
// that never happened.
type ShadowSCMWriteStore struct {
	q *sqlcgen.Queries
}

// NewShadowSCMWriteStore builds a store over pool.
func NewShadowSCMWriteStore(pool *pgxpool.Pool) *ShadowSCMWriteStore {
	return &ShadowSCMWriteStore{q: sqlcgen.New(pool)}
}

// WithTx returns a store whose queries run on tx.
//
// This matters more here than for most stores: §30.6's record-or-fail
// semantics mean the ledger insert and whatever transaction the caller is
// already inside must succeed or fail together. A gate that recorded
// outside the caller's transaction could report a suppression the caller
// then rolled back.
func (s *ShadowSCMWriteStore) WithTx(tx pgx.Tx) *ShadowSCMWriteStore {
	return &ShadowSCMWriteStore{q: s.q.WithTx(tx)}
}

// Create appends one suppressed write.
func (s *ShadowSCMWriteStore) Create(ctx context.Context, arg sqlcgen.CreateShadowSCMWriteParams) (sqlcgen.ShadowScmWrite, error) {
	return s.q.CreateShadowSCMWrite(ctx, arg)
}

// ListForRepo returns repoFullName's suppressed writes, newest first.
func (s *ShadowSCMWriteStore) ListForRepo(ctx context.Context, repoFullName string, limit int32) ([]sqlcgen.ShadowScmWrite, error) {
	return s.q.ListShadowSCMWritesForRepo(ctx, sqlcgen.ListShadowSCMWritesForRepoParams{
		RepoFullName: repoFullName,
		Limit:        limit,
	})
}

// ShadowEgressSuppressedEventType is the stored event AppendSuppressionEvent
// writes into the session's event log. The control plane writes it, never a
// sandbox, so the session actor refuses it from the sandbox socket
// (sessionactor's serverEventTypes).
const ShadowEgressSuppressedEventType = "shadow_egress_suppressed"

// AppendSuppressionEvent writes §30.6's own third recording write: an
// `events` row so a suppression appears inline in the session workspace
// the operator is already watching.
//
// It runs on the SAME *sqlcgen.Queries as the ledger insert, so under
// WithTx the two commit together. That is deliberate but not
// load-bearing: §30.6 is explicit that `events` is surface, never durable
// truth -- it cascades with the session, and the ledger row is the
// record. The caller therefore treats a failure here as loggable, not
// fatal.
func (s *ShadowSCMWriteStore) AppendSuppressionEvent(ctx context.Context, sessionID pgtype.UUID, messageID string, payload []byte) error {
	_, err := s.q.CreateEvent(ctx, sqlcgen.CreateEventParams{
		SessionID: sessionID,
		Type:      ShadowEgressSuppressedEventType,
		MessageID: messageID,
		Payload:   payload,
	})
	return err
}

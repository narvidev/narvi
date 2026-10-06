package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// SessionGuardStore reads what the session guard decides on
// (internal/domain/sessionguard, technical plan §40.1): a session's
// recorded spend, its turn count and the caps that apply to it. A thin
// wrapper around GetSessionGuardFacts; the decision is the domain's.
type SessionGuardStore struct {
	q *sqlcgen.Queries
}

// NewSessionGuardStore builds a SessionGuardStore backed by pool.
func NewSessionGuardStore(pool *pgxpool.Pool) *SessionGuardStore {
	return &SessionGuardStore{q: sqlcgen.New(pool)}
}

// WithTx returns a SessionGuardStore whose query runs on tx: the guard
// reads its facts in the transaction that holds the session's row lock.
func (s *SessionGuardStore) WithTx(tx pgx.Tx) *SessionGuardStore {
	return &SessionGuardStore{q: s.q.WithTx(tx)}
}

// Facts reads sessionID's spend, turn count and caps (GetSessionGuardFacts's
// doc comment says what each is and why the read is exact under the session's
// row lock). repoFullNames are the owner/name of the repositories the
// session's clone URLs name; the session's pull-request claims are read
// beside them. The amounts convert to micro-dollars exactly, and an amount
// that does not is an error, never a guess. pgx.ErrNoRows, unwrapped, when
// the session does not exist.
func (s *SessionGuardStore) Facts(ctx context.Context, sessionID pgtype.UUID, repoFullNames []string) (sessionguard.Facts, error) {
	if repoFullNames == nil {
		repoFullNames = []string{}
	}
	row, err := s.q.GetSessionGuardFacts(ctx, sqlcgen.GetSessionGuardFactsParams{
		SessionID:     sessionID,
		RepoFullNames: repoFullNames,
	})
	if err != nil {
		return sessionguard.Facts{}, err
	}
	spent, err := sessionguard.ParseMicroUSD(row.SpentUsd)
	if err != nil {
		return sessionguard.Facts{}, fmt.Errorf("postgres: session guard facts: spent: %w", err)
	}
	facts := sessionguard.Facts{
		SessionID:  row.SessionID.Bytes,
		ObservedAt: row.ObservedAt.Time,
		SpentUSD:   spent,
		Turns:      row.TurnCount,
	}
	if row.AutomationCapUsd != "" {
		limit, err := sessionguard.ParseMicroUSD(row.AutomationCapUsd)
		if err != nil {
			return sessionguard.Facts{}, fmt.Errorf("postgres: session guard facts: automation cap: %w", err)
		}
		facts.AutomationCap = &limit
		facts.AutomationSource = sessionguard.CapSource{
			Kind: sessionguard.CapSourceAutomation,
			Name: row.AutomationName,
			ID:   row.AutomationID.String(),
		}
	}
	if row.RepoCapUsd != "" {
		limit, err := sessionguard.ParseMicroUSD(row.RepoCapUsd)
		if err != nil {
			return sessionguard.Facts{}, fmt.Errorf("postgres: session guard facts: repository cap: %w", err)
		}
		facts.RepoCap = &limit
		facts.RepoSource = sessionguard.CapSource{
			Kind: sessionguard.CapSourceRepo,
			Name: row.CapRepoFullName,
			ID:   row.CapRepoFullName,
		}
	}
	return facts, nil
}

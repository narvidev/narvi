package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// MCPOAuthClientStore is a thin, pass-through wrapper around the
// sqlc-generated mcp_oauth_clients queries (technical plan §43.15,
// migrations/000141_mcp_oauth.up.sql and
// 000143_mcp_oauth_client_metadata.up.sql): the OAuth clients an MCP
// client program identifies itself as when it asks a user for access --
// pre-registered by an administrator, registered dynamically, or
// described by a client ID metadata document. No caching of its own and no
// business rules -- generating a client_id, validating redirect URIs and
// deciding when a metadata document is stale are the callers' job
// (internal/domain/mcpclient, internal/adapters/inbound/httpapi's
// mcpclients.go, internal/adapters/inbound/mcpauth).
type MCPOAuthClientStore struct {
	q *sqlcgen.Queries
	// db is what q runs on -- the pool, or the transaction WithTx was
	// given -- for the one method that needs a transaction of its own
	// (DeleteUnused; on a caller's transaction, a savepoint in it).
	db txBeginner
}

// txBeginner is a *pgxpool.Pool or a pgx.Tx: something a transaction (or
// a savepoint) can be begun on.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewMCPOAuthClientStore builds an MCPOAuthClientStore backed by pool.
func NewMCPOAuthClientStore(pool *pgxpool.Pool) *MCPOAuthClientStore {
	return &MCPOAuthClientStore{q: sqlcgen.New(pool), db: pool}
}

// WithTx returns an MCPOAuthClientStore whose queries run on tx.
func (s *MCPOAuthClientStore) WithTx(tx pgx.Tx) *MCPOAuthClientStore {
	return &MCPOAuthClientStore{q: s.q.WithTx(tx), db: tx}
}

// Create inserts a new client row and returns it. A nil RedirectUris is
// stored as an empty array, never NULL (the column is NOT NULL).
func (s *MCPOAuthClientStore) Create(ctx context.Context, arg sqlcgen.CreateMCPOAuthClientParams) (sqlcgen.McpOauthClient, error) {
	if arg.RedirectUris == nil {
		arg.RedirectUris = []string{}
	}
	return s.q.CreateMCPOAuthClient(ctx, arg)
}

// GetByClientID fetches a client by its public client_id (the value an
// OAuth request carries). pgx.ErrNoRows means no such client.
func (s *MCPOAuthClientStore) GetByClientID(ctx context.Context, clientID string) (sqlcgen.McpOauthClient, error) {
	return s.q.GetMCPOAuthClientByClientID(ctx, clientID)
}

// GetByID fetches a client by its internal id.
func (s *MCPOAuthClientStore) GetByID(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthClient, error) {
	return s.q.GetMCPOAuthClientByID(ctx, id)
}

// List returns every client, oldest first.
func (s *MCPOAuthClientStore) List(ctx context.Context) ([]sqlcgen.McpOauthClient, error) {
	return s.q.ListMCPOAuthClients(ctx)
}

// Lock takes the client row's FOR UPDATE lock for the rest of the
// transaction (queries/mcp_oauth_clients.sql's own doc comment): no
// consent or code exchange can issue anything under the client until it
// ends. pgx.ErrNoRows means no such client.
func (s *MCPOAuthClientStore) Lock(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthClient, error) {
	return s.q.LockMCPOAuthClient(ctx, id)
}

// LockKeyShare takes the client row's FOR KEY SHARE lock for the rest of
// the transaction and returns the row as of that lock: the first lock of
// every transaction that issues or revokes something under the client
// (the lock order at the top of mcpoauthgrant_store.go). pgx.ErrNoRows
// means no such client -- it was deleted, and everything under it with it.
func (s *MCPOAuthClientStore) LockKeyShare(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthClient, error) {
	return s.q.LockMCPOAuthClientKeyShare(ctx, id)
}

// UpsertMetadataDocument records one fetched and validated client ID
// metadata document (technical plan §43.15): it creates the
// metadata-document client arg.ClientID identifies, or replaces its cached
// name, redirect URIs and cache stamps in place -- one statement, one row
// lock (queries/mcp_oauth_clients.sql's own doc comment). A nil
// RedirectUris is stored as an empty array. pgx.ErrNoRows means a client
// of another kind already carries this client_id, which is never
// overwritten.
func (s *MCPOAuthClientStore) UpsertMetadataDocument(ctx context.Context, arg sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams) (sqlcgen.McpOauthClient, error) {
	if arg.RedirectUris == nil {
		arg.RedirectUris = []string{}
	}
	return s.q.UpsertMCPOAuthMetadataDocumentClient(ctx, arg)
}

// MarkMetadataRefetchFailed records the first failed re-fetch of a
// metadata-document client's stale document since its last successful
// fetch (queries/mcp_oauth_clients.sql's own doc comment): failedAt, and
// graceUntil as its stale time -- provided no fetch has succeeded since
// fetchedAt (the row the caller read) and no failure is recorded yet.
// pgx.ErrNoRows means one of those happened (or the client is gone), and
// what is stored now stands.
func (s *MCPOAuthClientStore) MarkMetadataRefetchFailed(ctx context.Context, id pgtype.UUID, fetchedAt, failedAt, graceUntil time.Time) (sqlcgen.McpOauthClient, error) {
	return s.q.MarkMCPOAuthMetadataDocumentRefetchFailed(ctx, sqlcgen.MarkMCPOAuthMetadataDocumentRefetchFailedParams{
		ID:                      id,
		MetadataFetchedAt:       pgtype.Timestamptz{Time: fetchedAt, Valid: true},
		MetadataRefetchFailedAt: pgtype.Timestamptz{Time: failedAt, Valid: true},
		MetadataStaleAt:         pgtype.Timestamptz{Time: graceUntil, Valid: true},
	})
}

// DeleteUnused deletes every dynamically registered or metadata-document
// client no operator disabled, with no grant and no authorization
// request, last used before unusedBefore -- registered, last fetched, or
// last usable from its cache -- returning how many it deleted (the
// expired-credential sweep's unused-client pass). One transaction, two
// statements (queries/mcp_oauth_clients.sql's own doc comment): the
// candidates are locked FOR UPDATE, client first as the lock order
// requires, then deleted only if a fresh look still finds them unused --
// so a request or grant that committed while the lock waited keeps its
// client. On a store built WithTx, the transaction is a savepoint in the
// caller's, whose locks the caller then holds. Pre-registered clients are
// never deleted.
func (s *MCPOAuthClientStore) DeleteUnused(ctx context.Context, unusedBefore time.Time) (int64, error) {
	cutoff := pgtype.Timestamptz{Time: unusedBefore, Valid: true}
	var deleted int64
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		ids, err := q.LockUnusedMCPOAuthClients(ctx, cutoff)
		if err != nil || len(ids) == 0 {
			return err
		}
		deleted, err = q.DeleteUnusedMCPOAuthClients(ctx, sqlcgen.DeleteUnusedMCPOAuthClientsParams{Ids: ids, UnusedBefore: cutoff})
		return err
	})
	return deleted, err
}

// Delete removes a client by internal id and returns the deleted row
// (pgx.ErrNoRows when there was none). Every grant, pending authorization
// request, code, access token and refresh token issued to the client
// cascades with it.
func (s *MCPOAuthClientStore) Delete(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthClient, error) {
	return s.q.DeleteMCPOAuthClient(ctx, id)
}

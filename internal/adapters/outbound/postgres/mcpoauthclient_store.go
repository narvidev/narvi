package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// MCPOAuthClientStore is a thin, pass-through wrapper around the
// sqlc-generated mcp_oauth_clients queries (technical plan §43.15,
// migrations/000141_mcp_oauth.up.sql): the OAuth clients an MCP client
// program identifies itself as when it asks a user for access. No
// caching, no business rules -- generating a client_id and validating
// redirect URIs are the caller's job (internal/domain/mcpclient,
// internal/adapters/inbound/httpapi's mcpclients.go).
type MCPOAuthClientStore struct {
	q *sqlcgen.Queries
}

// NewMCPOAuthClientStore builds an MCPOAuthClientStore backed by pool.
func NewMCPOAuthClientStore(pool *pgxpool.Pool) *MCPOAuthClientStore {
	return &MCPOAuthClientStore{q: sqlcgen.New(pool)}
}

// WithTx returns an MCPOAuthClientStore whose queries run on tx.
func (s *MCPOAuthClientStore) WithTx(tx pgx.Tx) *MCPOAuthClientStore {
	return &MCPOAuthClientStore{q: s.q.WithTx(tx)}
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
// transaction (queries/mcp_oauth_clients.sql's own doc comment): no grant
// can be added under the client until it ends. pgx.ErrNoRows means no
// such client.
func (s *MCPOAuthClientStore) Lock(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthClient, error) {
	return s.q.LockMCPOAuthClient(ctx, id)
}

// Delete removes a client by internal id and returns the deleted row
// (pgx.ErrNoRows when there was none). Every grant, pending authorization
// request, code and access token issued to the client cascades with it.
func (s *MCPOAuthClientStore) Delete(ctx context.Context, id pgtype.UUID) (sqlcgen.McpOauthClient, error) {
	return s.q.DeleteMCPOAuthClient(ctx, id)
}

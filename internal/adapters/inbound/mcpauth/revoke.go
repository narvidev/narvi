package mcpauth

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/platform"
)

// Revoke backs POST /oauth/revoke (RFC 7009, technical plan §43.16): a
// client gives back one of its own tokens -- an access token or a refresh
// token -- and with it the whole authorization the token belongs to. The
// grant is deleted and every code and token under it cascades, so whatever
// the client still holds stops working on its next use: one rule for both
// token types, one "stops on the next call" story. token_type_hint only
// decides which kind is looked up first.
//
// The answer is 200 with an empty body whether or not anything was
// revoked (RFC 7009 section 2.2): a token that is unknown, already gone,
// or issued to ANOTHER client is left alone and answered exactly like one
// that was revoked, so a caller learns nothing about a token that is not
// its own. Only a request that cannot be read, or whose client cannot be
// identified, is an error (RFC 6749 section 5.2's shapes). The client is
// identified as at the token endpoint (authenticateClient); a disabled
// client may still revoke, since revocation only ever gives access back.
// Like the token endpoint, it is never behind the cookie middleware and
// reads no cookie: a person revokes from Settings instead.
func (s *Server) Revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	form, ok := parseOAuthForm(w, r)
	if !ok {
		return
	}
	client, _, ok := s.authenticateClient(w, r, form, "revoke")
	if !ok {
		return
	}
	token := form.Get("token")
	if token == "" {
		writeTokenError(w, errInvalidRequest, "token is required", false)
		return
	}

	grantID, found, err := s.revocableGrant(ctx, platform.HashToken(token), form.Get("token_type_hint"))
	if err != nil {
		logger.Error("mcpauth: revoke: load token failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	if !found {
		logger.Info("mcpauth: revoke: nothing to revoke", "outcome", "unknown_token", "client_id", client.ClientID)
		writeRevoked(w)
		return
	}
	if err := s.revokeGrant(ctx, client, grantID); err != nil {
		logger.Error("mcpauth: revoke failed", "error", err)
		writeTokenError(w, errServerError, "", false)
		return
	}
	writeRevoked(w)
}

// revocableGrant finds the grant the token with this hash was issued
// under: an access token or a refresh token, expired or rotated or not
// (the client is giving it back either way), looked up in the order hint
// suggests -- "refresh_token" first for that hint, access tokens first
// otherwise, including for a hint this server does not recognise (RFC 7009
// section 2.1 lets a server ignore it). found is false when neither table
// holds it.
func (s *Server) revocableGrant(ctx context.Context, tokenHash, hint string) (grantID pgtype.UUID, found bool, err error) {
	lookups := []func() (pgtype.UUID, error){
		func() (pgtype.UUID, error) {
			t, err := s.deps.Grants.GetAccessTokenByHash(ctx, tokenHash)
			return t.GrantID, err
		},
		func() (pgtype.UUID, error) {
			t, err := s.deps.Grants.GetRefreshTokenByHash(ctx, tokenHash)
			return t.GrantID, err
		},
	}
	if hint == "refresh_token" {
		lookups[0], lookups[1] = lookups[1], lookups[0]
	}
	for _, lookup := range lookups {
		id, err := lookup()
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return pgtype.UUID{}, false, err
		}
		return id, true, nil
	}
	return pgtype.UUID{}, false, nil
}

// revokeGrant deletes grantID if -- and only if -- it was issued to
// client, auditing the revocation (reason client, attributed to the
// grant's user with actor "client"). A grant already gone, or one issued
// to another client, is left alone without an error: the caller answers
// the same 200 either way.
//
// Locks follow the one order (the top of postgres/mcpoauthgrant_store.go):
// the grant's client FOR KEY SHARE, then the grant, whose deletion
// cascades to its codes and tokens. So the grant is read first, without a
// lock. A code exchange or refresh in flight under the grant holds it FOR
// KEY SHARE: the deletion waits for it to commit, then takes what it
// issued too.
func (s *Server) revokeGrant(ctx context.Context, client sqlcgen.McpOauthClient, grantID pgtype.UUID) error {
	logger := platform.Logger(ctx)

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	grants := s.deps.Grants.WithTx(tx)

	grant, err := grants.GetGrant(ctx, grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if grant.ClientID != client.ID {
		logger.Warn("mcpauth: revoke ignored", "outcome", "token_for_other_client", "client_id", client.ClientID)
		return nil
	}
	if _, err := s.deps.Clients.WithTx(tx).LockKeyShare(ctx, grant.ClientID); errors.Is(err, pgx.ErrNoRows) {
		// The client was deleted since, and this grant with it.
		return nil
	} else if err != nil {
		return err
	}
	deleted, err := grants.DeleteGrant(ctx, grant.ID)
	if err != nil {
		return err
	}
	if deleted == 1 {
		if err := auditlog.Record(ctx, s.deps.AuditLog.WithTx(tx), grant.UserID, "mcp_authorization.revoked", "mcp_authorization", grant.ID.String(), map[string]any{
			"reason":      "client",
			"actor":       "client",
			"client_id":   client.ClientID,
			"client_name": client.ClientName,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if deleted == 1 {
		logger.Info("mcpauth: grant revoked by its client", "outcome", "client", "grant_id", grant.ID.String())
	}
	return nil
}

// writeRevoked answers every revocation request that could be read and
// whose client was identified: 200 with an empty body (RFC 7009 section
// 2.2), never cached.
func writeRevoked(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
}

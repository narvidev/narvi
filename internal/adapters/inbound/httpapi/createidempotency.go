// This file (createidempotency.go) holds the two things POST /api/sessions
// decides about a create beyond the request body itself (technical plan
// §43.1/§43.8): which source the session records, and whether the request
// is a replay of one already served.

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// recordedSpawnSource is the source a session created through POST
// /api/sessions records (technical plan §43.1): mcp exactly when ctx
// carries the platform.MCPGrant auth.RequireMCPBearer attaches -- the
// request is an MCP tool call bridged to this handler -- and web
// otherwise. It never reads the request body, whose spawnSource this route
// holds to web for every caller, and never the credential type: a future
// bearer that attaches a user but no grant records web.
func recordedSpawnSource(ctx context.Context) sqlcgen.SessionSpawnSource {
	if _, ok := platform.MCPGrantFromContext(ctx); ok {
		return sqlcgen.SessionSpawnSourceMcp
	}
	return sqlcgen.SessionSpawnSourceWeb
}

// createIdempotencyIndex is migrations/000150's unique index on
// (created_by, create_idempotency_key): the constraint a concurrent create
// with the same key fails on.
const createIdempotencyIndex = "sessions_create_idempotency_key_uniq"

// idempotencyKeyReusedMessage is the 409 a key already used with a
// different request answers.
const idempotencyKeyReusedMessage = "idempotencyKey reused with a different request"

// parseIdempotencyKey parses req.IdempotencyKey, which the REST decode does
// not check against its format:uuid (encoding/json never does): Valid false
// when the request carries none, an error when it carries a value that is
// not a UUID.
func parseIdempotencyKey(req restdtos.CreateSessionRequest) (pgtype.UUID, error) {
	var key pgtype.UUID
	if req.IdempotencyKey == nil {
		return key, nil
	}
	if err := key.Scan(*req.IdempotencyKey); err != nil {
		return pgtype.UUID{}, errors.New("idempotencyKey: must be a UUID")
	}
	return key, nil
}

// createRequestSHA256 is the hash a replay is compared by: SHA-256 of req
// re-encoded without its idempotency key. Re-encoding the decoded DTO, not
// hashing the raw body, makes two requests that say the same thing hash the
// same whatever their whitespace or key order; every field the route reads
// is in it, so a request that would create a different session hashes
// differently.
func createRequestSHA256(req restdtos.CreateSessionRequest) ([]byte, error) {
	req.IdempotencyKey = nil
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

// isCreateIdempotencyConflict reports whether err is the unique violation a
// second insert of the same (creator, key) fails with.
func isCreateIdempotencyConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == createIdempotencyIndex
}

// replayCreate answers a create whose key createdBy has already used, and
// reports whether it did. The same request hash answers 200 with that
// session as it is now -- the first call answered 201 -- and nothing is
// created, audited or dispatched; a different hash answers 409. A key never
// used answers nothing and returns false, so the caller creates. A failed
// read is a 500: a replay that cannot be checked must not create a second
// session.
func replayCreate(w http.ResponseWriter, r *http.Request, sessions *postgres.SessionStore, createdBy, key pgtype.UUID, requestSHA256 []byte) bool {
	existing, err := sessions.GetByCreateIdempotencyKey(r.Context(), createdBy, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		platform.Logger(r.Context()).Error("httpapi: read session by idempotency key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return true
	}
	if !bytes.Equal(existing.CreateRequestSha256, requestSHA256) {
		writeError(w, http.StatusConflict, idempotencyKeyReusedMessage)
		return true
	}
	writeJSON(w, http.StatusOK, sessionToDTO(existing))
	return true
}

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

// idempotencyKeyOtherSourceMessage is the 409 a key answers when the
// session it started records another source than this create would: a key
// used by cookie, sent again by an MCP app, or the other way round.
const idempotencyKeyOtherSourceMessage = "idempotencyKey already used for a session started another way"

// errIdempotencyKeyNotUUID is the 400 a key not spelled as a UUID answers.
var errIdempotencyKeyNotUUID = errors.New("idempotencyKey: must be a UUID, as 8-4-4-4-12 hexadecimal digits")

// parseIdempotencyKey parses req.IdempotencyKey, which the REST decode does
// not check against its format:uuid (encoding/json never does): Valid false
// when the request carries none, an error when it carries a value that is
// not a UUID.
//
// A UUID is exactly what that format, and the MCP tool's own schema check,
// accept: 36 characters, hyphens at 8, 13, 18 and 23, hexadecimal digits
// everywhere else, in either case. Both cases are one key: it is stored as
// a uuid, so a key spelled in capitals replays the same key spelled in
// lower case. Every other spelling is refused, even one pgtype would read
// as a UUID -- 32 digits with no hyphens, braces, other characters where
// the hyphens go -- so no string the contract refuses can stand for a key.
func parseIdempotencyKey(req restdtos.CreateSessionRequest) (pgtype.UUID, error) {
	var key pgtype.UUID
	if req.IdempotencyKey == nil {
		return key, nil
	}
	if !isHyphenatedUUID(*req.IdempotencyKey) {
		return pgtype.UUID{}, errIdempotencyKeyNotUUID
	}
	if err := key.Scan(*req.IdempotencyKey); err != nil {
		return pgtype.UUID{}, errIdempotencyKeyNotUUID
	}
	return key, nil
}

// isHyphenatedUUID reports whether s is a UUID in its 8-4-4-4-12 form,
// hexadecimal digits in either case.
func isHyphenatedUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// createRequestFingerprint is the canonical form of a create request that a
// replay is compared by (technical plan §43.8): every CreateSessionRequest
// field the route reads, in a fixed order, with each set of spellings the
// route treats alike written one way. So two requests that would start the
// same session hash the same however their JSON was written -- key order,
// whitespace, unknown fields, an optional field absent or given its
// default -- and two that would start different sessions do not.
//
// Two fields are left out: idempotencyKey, the key the hash is stored
// under, and spawnSource, which the route holds to web for every caller;
// the source a session records comes from the request's context
// (recordedSpawnSource), and replayCreate compares it on its own.
// TestCreateRequestFingerprint_CoversTheRequest fails when a field is added
// to CreateSessionRequest and not here.
type createRequestFingerprint struct {
	Title        *string                        `json:"title"`
	Prompt       *string                        `json:"prompt"`
	Repos        []createRequestFingerprintRepo `json:"repos"`
	ModelID      *string                        `json:"modelId"`
	Effort       *string                        `json:"effort"`
	PlanMode     bool                           `json:"planMode"`
	BuildModelID *string                        `json:"buildModelId"`
	BuildEffort  *string                        `json:"buildEffort"`
	// EpistemicCheckEnabled stays a pointer: null follows the deployment's
	// default, which false does not.
	EpistemicCheckEnabled *bool `json:"epistemicCheckEnabled"`
	// PathScope is nil when the request's is absent, null or empty: the
	// route scopes nothing for any of the three.
	PathScope []string `json:"pathScope"`
	// MockConfig is the contracts path a present mockConfig resolves to --
	// defaultContractsPath when it names none -- and nil when mockConfig is
	// absent or null.
	MockConfig *string `json:"mockConfig"`
	Docker     bool    `json:"docker"`
	// EgressPolicy's Allowlist is nil when the request's is null or empty.
	EgressPolicy *createRequestFingerprintEgress `json:"egressPolicy"`
}

// createRequestFingerprintRepo is one repository of a fingerprint. Branch
// stays a pointer: null is the repository's default branch, and an empty
// branch is refused.
type createRequestFingerprintRepo struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Branch *string `json:"branch"`
}

// createRequestFingerprintEgress is a fingerprint's egress policy.
type createRequestFingerprintEgress struct {
	Mode      string   `json:"mode"`
	Allowlist []string `json:"allowlist"`
}

// fingerprintOf is req's canonical form (createRequestFingerprint).
func fingerprintOf(req restdtos.CreateSessionRequest) createRequestFingerprint {
	f := createRequestFingerprint{
		Title:                 req.Title,
		Prompt:                req.Prompt,
		ModelID:               req.ModelId,
		Effort:                req.Effort,
		PlanMode:              req.PlanMode,
		BuildModelID:          req.BuildModelId,
		BuildEffort:           req.BuildEffort,
		EpistemicCheckEnabled: req.EpistemicCheckEnabled,
		Docker:                req.Docker,
	}
	for _, repo := range req.Repos {
		f.Repos = append(f.Repos, createRequestFingerprintRepo{Name: repo.Name, URL: repo.Url, Branch: repo.Branch})
	}
	if req.PathScope != nil && len(*req.PathScope) > 0 {
		f.PathScope = []string(*req.PathScope)
	}
	if req.MockConfig != nil {
		contractsPath := defaultContractsPath
		if req.MockConfig.ContractsPath != nil {
			contractsPath = *req.MockConfig.ContractsPath
		}
		f.MockConfig = &contractsPath
	}
	if req.EgressPolicy != nil {
		egress := createRequestFingerprintEgress{Mode: string(req.EgressPolicy.Mode)}
		if len(req.EgressPolicy.Allowlist) > 0 {
			egress.Allowlist = req.EgressPolicy.Allowlist
		}
		f.EgressPolicy = &egress
	}
	return f
}

// createRequestSHA256 is the hash a replay is compared by: SHA-256 of req's
// canonical form (createRequestFingerprint), JSON-encoded. The encoding is
// deterministic: a struct's fields encode in declaration order.
func createRequestSHA256(req restdtos.CreateSessionRequest) ([]byte, error) {
	encoded, err := json.Marshal(fingerprintOf(req))
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
// reports whether it did. A key never used answers nothing and returns
// false, so the caller creates. Otherwise nothing is created, audited or
// dispatched, and:
//
//   - A session that records another source than source -- the one this
//     create would record -- answers 409. Keys are the user's across REST
//     and MCP, one namespace, but a replay only ever answers the surface
//     that started the session: an MCP call never gets back a session its
//     user started by cookie, and the other way round. The source is
//     compared first, so such a key is refused whatever the request.
//   - The same request hash answers 200 with that session as it is now --
//     the first call answered 201.
//   - A different hash answers 409.
//
// A failed read is a 500: a replay that cannot be checked must not create
// a second session.
func replayCreate(w http.ResponseWriter, r *http.Request, sessions *postgres.SessionStore, createdBy, key pgtype.UUID, source sqlcgen.SessionSpawnSource, requestSHA256 []byte) bool {
	existing, err := sessions.GetByCreateIdempotencyKey(r.Context(), createdBy, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		platform.Logger(r.Context()).Error("httpapi: read session by idempotency key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return true
	}
	if existing.SpawnSource != source {
		writeError(w, http.StatusConflict, idempotencyKeyOtherSourceMessage)
		return true
	}
	if !bytes.Equal(existing.CreateRequestSha256, requestSHA256) {
		writeError(w, http.StatusConflict, idempotencyKeyReusedMessage)
		return true
	}
	writeJSON(w, http.StatusOK, sessionToDTO(existing))
	return true
}

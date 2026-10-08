// This file (sandboxsecretsdelivery.go) implements §27.1's own
// ("sandbox secrets & opencode config", §27.1) CP-side DELIVERY endpoint
// for sandbox-agent: POST /sessions/{sessionID}/sandbox-secrets (note: no
// /api prefix, exactly like scm-credentials/provider-credentials/
// snapshot/review-verdict -- a sandbox-to-CP endpoint, not a browser-
// facing REST route, §5.2).
//
// Mirrors providercredentialsdelivery.go's own handshake VERBATIM, per
// §27.1's own explicit instruction ("delivery via POST /sessions/{id}/
// sandbox-secrets mirroring providercredentialsdelivery.go's handshake
// verbatim") -- itself mirroring scmcredentials.go's security posture:
// sandbox-bearer-token-authenticated (mounted OUTSIDE auth.Middleware
// entirely, alongside the other sandbox-facing routes,
// cmd/control-plane/main.go), the SAME dead-sandbox check
// (sandbox.IsDeadSandboxStatus, 410 -- checked immediately after the
// sandbox row lookup, before the gen/token comparisons) and X-Sandbox-Gen
// fencing (403 on a missing/mismatched header) at the SAME points in the
// handshake, and the SAME bearer-token verification
// (verifySandboxBearerToken, constant-time hash compare, no nil-token_hash
// bypass). Outcome table below mirrors providercredentialsdelivery.go's
// own numbered list exactly (no request body/host to check here either):
//
//  1. sessionID does not parse as a UUID -> 404.
//  2. Authorization: Bearer <token> missing/malformed -> 401 (checked
//     before the sandbox row lookup).
//  3. No sandbox row exists for sessionID -> 404.
//  4. sandbox.IsDeadSandboxStatus(sandboxRow.Status) -> 410.
//  5. X-Sandbox-Gen missing/malformed/mismatched -> 403.
//  6. The presented token fails verifySandboxBearerToken -> 401.
//  7. Otherwise -> 200 with a plain name->plaintext map of RESOLVED
//     winners only, losers never decrypted (§25.1's decrypt-only-the-
//     winner discipline, reused here unchanged) -- resolved via
//     internal/domain/providercredential.Resolve over every candidate row
//     (across every scope this Step actually resolves -- repo/
//     environment/global; NOT automation, §27.1's own schema-only
//     carve-out) that could apply to this session's own repo(s)/
//     environment, decrypted server-side (the ONLY layer that ever holds
//     cfg.TokenEncryptionKey). A name with nothing configured at any
//     scope is simply ABSENT from the map -- never a null/empty-string
//     entry, and never itself an error.
//
// # A pull request's review session whose head is not the base's
//
// A session that claims a pull request (github_pr_sessions) is delivered no
// sandbox secret at all -- global, environment or repository-scoped --
// unless its head is known to be a branch of the base repository
// (reposource.ReviewHeadInBaseRepository: the primary repo names the
// claim's repository and carries a branch; technical plan §27.1, §30.4).
// Its spec names the base repository for a pull request from a fork too,
// and its sandbox boots on the pull request's head and runs that head's
// setup.sh and start.sh with every secret delivered here in their
// environment: code a person outside the base repository may have written.
// So a fork's pull request, a deleted fork's, one whose head could not be
// read when its review started, and a legacy session still naming the fork
// get the answer a session with no secrets gets -- 200 with an empty map,
// the secrets table never read, so nothing about which secrets exist leaks
// -- logged at Info. This is the rule a CI system applies to a pull request
// from a fork: its code runs, the repository's secrets do not reach it. A
// session with no claim, and a review session whose head is a branch of its
// base repository, resolve as before. A claim read that fails is a 500,
// never "no claim", which would deliver.
//
// A single row that fails to decrypt (a genuinely corrupted/tampered
// ciphertext) is logged loudly and simply OMITTED from the response,
// never turned into a 500 for the whole request -- mirrors
// providercredentialsdelivery.go's own identical reasoning: one corrupted
// row must not deny a sandbox-agent every OTHER secret it has nothing
// wrong with. The decrypted value itself is NEVER logged, at any point.

package httpapi

import (
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// sandboxSecretsResponse is this Step's own invented, documented response
// shape: a plain map from secret name to its resolved plaintext value.
// Mirrors providerCredentialsResponse's own "small, explicit, invented
// wire shape" precedent (providercredentialsdelivery.go), simplified --
// unlike a provider credential (§8.8's discriminated api/oauth union),
// a sandbox secret is ALWAYS a plain string value (there is no oauth-kind
// concept for a general env var), so the map's own value type is a bare
// string, not a struct.
type sandboxSecretsResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// SandboxSecretsDelivery backs POST /sessions/{sessionID}/sandbox-secrets
// -- see this file's own top doc comment for the full outcome table and
// security-posture rationale.
func SandboxSecretsDelivery(
	sessions *postgres.SessionStore,
	sandboxes *postgres.SandboxStore,
	prSessions *postgres.GitHubPRSessionStore,
	sandboxSecrets *postgres.SandboxSecretStore,
	tokenEncryptionKey []byte,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		var sessionID pgtype.UUID
		if err := sessionID.Scan(chi.URLParam(r, "sessionID")); err != nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		ctx = platform.WithSessionID(ctx, sessionID.String())
		logger := platform.Logger(ctx)

		token, ok := bearerTokenFromHeader(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or malformed authorization header")
			return
		}

		sandboxRow, err := sandboxes.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: sandbox-secrets: get sandbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Dead-sandbox check FIRST, before the gen/token comparisons below
		// -- same ordering scmcredentials.go/providercredentialsdelivery.go
		// both use.
		if sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			writeError(w, http.StatusGone, "session stopped")
			return
		}

		presentedGen, genErr := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if genErr != nil || presentedGen != int(sandboxRow.Gen) {
			logger.Warn("httpapi: sandbox-secrets: rejecting: gen mismatch",
				"presented_gen_header", r.Header.Get("X-Sandbox-Gen"), "sandbox_gen", sandboxRow.Gen)
			writeError(w, http.StatusForbidden, "no usable sandbox secret for this session")
			return
		}

		if !verifySandboxBearerToken(token, sandboxRow.TokenHash) {
			writeError(w, http.StatusUnauthorized, "invalid sandbox token")
			return
		}

		sessionRow, err := sessions.Get(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: sandbox-secrets: get session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// A pull request's review session whose head is not known to be a
		// branch of its base repository gets no secret (this file's top
		// comment).
		prSession, err := prSessions.GetBySessionID(ctx, sessionID)
		switch {
		case err == nil:
			if !reposource.ReviewHeadInBaseRepository(prSession.RepoFullName, sessionRow.Repos) {
				logger.Info("httpapi: sandbox-secrets: delivering none: the review session's head is not known to be a branch of its base repository",
					"repo", prSession.RepoFullName, "pr_number", prSession.PrNumber)
				writeJSON(w, http.StatusOK, sandboxSecretsResponse{Secrets: map[string]string{}})
				return
			}
		case errors.Is(err, pgx.ErrNoRows):
			// Not a pull request's review session: resolved as before.
		default:
			logger.Error("httpapi: sandbox-secrets: read the session's pull request claim failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		repoFullNames, err := sessionRepoFullNames(sessionRow.Repos)
		if err != nil {
			logger.Error("httpapi: sandbox-secrets: parse session repos failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		var environmentID *string
		if sessionRow.EnvironmentID.Valid {
			id := sessionRow.EnvironmentID.String()
			environmentID = &id
		}

		rows, err := sandboxSecrets.ListForResolution(ctx, repoFullNames, environmentID)
		if err != nil {
			logger.Error("httpapi: sandbox-secrets: list candidates failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// repoRank mirrors ProviderCredentialsDelivery's own identical
		// primary-first tie-break re-establishment -- see that handler's
		// own doc comment for the full "why" (raw SQL row order is not a
		// guaranteed proxy for repoFullNames' own position-0-is-primary
		// convention, §3.4).
		repoRank := make(map[string]int, len(repoFullNames))
		for i, name := range repoFullNames {
			repoRank[name] = i
		}

		byName := make(map[string][]providercredential.Candidate[sqlcgen.SandboxSecret], len(rows))
		for _, row := range rows {
			byName[row.Name] = append(byName[row.Name], providercredential.Candidate[sqlcgen.SandboxSecret]{
				Scope: providercredential.Scope(row.Scope),
				Value: row,
			})
		}

		for name, candidates := range byName {
			sort.SliceStable(candidates, func(i, j int) bool {
				return sandboxSecretRepoTieBreakRank(candidates[i], repoRank) < sandboxSecretRepoTieBreakRank(candidates[j], repoRank)
			})
			byName[name] = candidates
		}

		secrets := make(map[string]string, len(byName))
		for name, candidates := range byName {
			winner, ok := providercredential.Resolve(candidates)
			if !ok {
				continue
			}
			plaintext, err := platform.DecryptToken(tokenEncryptionKey, winner.ValueEncrypted)
			if err != nil {
				// Never logs the ciphertext/plaintext -- see
				// platform.DecryptToken's own doc comment. Logged and
				// SKIPPED, not a 500 for the whole request.
				logger.Error("httpapi: sandbox-secrets: decrypt failed",
					"error", err, "name", name, "scope", string(winner.Scope))
				continue
			}
			secrets[name] = string(plaintext)
		}

		writeJSON(w, http.StatusOK, sandboxSecretsResponse{Secrets: secrets})
	}
}

// sandboxSecretRepoTieBreakRank mirrors repoTieBreakRank
// (providercredentialsdelivery.go) exactly, over sqlcgen.SandboxSecret
// instead of sqlcgen.ProviderCredential -- see that function's own doc
// comment for the full "why" (only ScopeRepo can ever have more than one
// same-name candidate here).
func sandboxSecretRepoTieBreakRank(candidate providercredential.Candidate[sqlcgen.SandboxSecret], repoRank map[string]int) int {
	if candidate.Scope != providercredential.ScopeRepo || candidate.Value.ScopeTargetID == nil {
		return -1
	}
	rank, ok := repoRank[*candidate.Value.ScopeTargetID]
	if !ok {
		return -1
	}
	return rank
}

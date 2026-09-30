// This file (providercredentialsdelivery.go) implements §25.1's own
// ("provider credential injection", §25.1/§25.3) CP-side DELIVERY
// endpoint for sandbox-agent: POST /sessions/{sessionID}/provider-
// credentials (note: no /api prefix, exactly like scm-credentials/
// snapshot/review-verdict -- a sandbox-to-CP endpoint, not a browser-
// facing REST route, §5.2).
//
// Mirrors scmcredentials.go's own exact security posture, deliberately,
// not coincidentally: sandbox-bearer-token-authenticated (mounted OUTSIDE
// auth.Middleware entirely, alongside the other sandbox-facing routes,
// cmd/control-plane/main.go), the SAME dead-sandbox check
// (sandbox.IsDeadSandboxStatus, 410 -- checked immediately after the
// sandbox row lookup, before the gen/token comparisons) and X-Sandbox-Gen
// fencing (403 on a missing/mismatched header) at the SAME points in the
// handshake, and the SAME bearer-token verification
// (verifySandboxBearerToken, constant-time hash compare, no nil-token_hash
// bypass). A provider-credential leak to a stale/terminalized sandbox is
// exactly the same class of risk scmcredentials.go's own audit history
// already fixed once for SCM tokens (that file's own top doc comment,
// "Audit remediation... design decision 2") -- this endpoint does not
// reintroduce that gap for a new secret type. Outcome table below mirrors
// that file's own numbered list, adapted (no request body/host to check
// here at all -- there is no "host" concept for a provider API key, unlike
// a git credential):
//
//  1. sessionID does not parse as a UUID -> 404.
//  2. Authorization: Bearer <token> missing/malformed -> 401 (checked
//     before the sandbox row lookup, matching scmcredentials.go's own
//     ordering).
//  3. No sandbox row exists for sessionID -> 404.
//  4. sandbox.IsDeadSandboxStatus(sandboxRow.Status) -> 410.
//  5. X-Sandbox-Gen missing/malformed/mismatched -> 403.
//  6. The presented token fails verifySandboxBearerToken -> 401.
//  7. Otherwise -> 200 with a provider-keyed map of credentialAuthValue
//     (§29.6: {"type":"api","key":"sk-..."} or {"type":"oauth",
//     "access":...,"expires":...,"accountId":...} -- see that type's own
//     doc comment for the full shape, and for why it structurally cannot
//     carry a refresh token) -- resolved via internal/domain/
//     providercredential.Resolve over every candidate row (across all 4
//     scopes, including §8.8's own ScopeUser) that could apply to this
//     session's own repo(s)/environment/creator, decrypted server-side
//     -- the creator's ScopeUser rows only when providercredential.
//     UserScopeTarget says this session reads them: never for a pull
//     request's review session, a child session or a session with no
//     creator (§29.4, §45.1) -- (the ONLY layer that ever holds
//     cfg.TokenEncryptionKey). A provider
//     with nothing configured at any scope is simply ABSENT from the map
//     -- never a null/empty-string entry, and never itself an error: the
//     overwhelming common case is zero rows configured for any given
//     session, and this endpoint degrades to an empty {} exactly as
//     gracefully as a fully-configured one.
//
// sandbox-agent (internal/sandboxagent/credentials' own CPClient.
// FetchProviderCredentials, and ultimately cmd/sandbox-agent/main.go's own
// spawn-time wiring) maps each provider name onto its own env-var name(s)
// (providercredential.EnvVarNames) -- deliberately NOT done here, so this
// response's own shape stays a plain, provider-keyed map, and the "1
// provider -> N env vars" mapping detail (google's own 3-name case) lives
// in exactly ONE place, the domain package, never duplicated between CP
// and sandbox-agent.
//
// A single row that fails to decrypt (a genuinely corrupted/tampered
// ciphertext -- platform.DecryptToken's own AES-GCM authentication tag
// catching it) is logged loudly and simply OMITTED from the response,
// never turned into a 500 for the whole request: 3 independent provider
// credentials can resolve in one call, and one corrupted row must not
// deny a sandbox-agent the other 2 it has nothing wrong with. The
// decrypted value itself is NEVER logged, at any point -- matching
// tokenencrypt.go's own "never log plaintext, key, or ciphertext"
// discipline exactly (grepped for in this Step's own diff before
// reporting done).

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/credentialscope"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// providerCredentialsResponse is this Step's own invented, documented
// response shape: a plain map from provider name ("google"/"anthropic"/
// "openai") to its resolved credential value. Mirrors scmCredentialsResponse
// 's own "small, explicit, invented wire shape" precedent (scmcredentials.go)
// -- internal/sandboxagent/credentials' own client-side type must match
// this exactly, the same reconciliation scmcredentials.go's own top doc
// comment describes for the SCM case.
//
// (§29.6) evolves the per-provider VALUE from a bare plaintext
// string into credentialAuthValue, a discriminated union -- see that
// type's own doc comment for the exact shape and, critically, for why it
// has no "refresh" field at all.
type providerCredentialsResponse struct {
	Credentials map[string]credentialAuthValue `json:"credentials"`
}

// credentialAuthValue mirrors OpenCode's own two real Auth-union member
// shapes verbatim (§29.1/§29.6, verified live against the pinned OpenCode
// 1.17.15 binary's own /doc OpenAPI schema during this Step):
// {"type":"api","key":...} for a static credential (today's behavior,
// re-labeled, never changed shape) or {"type":"oauth","access":...,
// "expires":...,"accountId":...} for a resolved user-scope row.
//
// Deliberately has NO "refresh" field in EITHER variant -- not merely
// "sent empty", genuinely ABSENT from the Go type, so there is no field
// for a bug anywhere in this response-building code to accidentally
// populate. §29.5's own rule ("the refresh token NEVER leaves the control
// plane") is enforced structurally at exactly this wire boundary: the
// oauth-kind branch below decrypts and parses the stored {access, refresh,
// expires_ms, account_id} blob (oauthCredentialBlob) but never copies its
// Refresh field into this type. sandbox-agent's own SetOAuthAuth call
// (internal/adapters/outbound/opencode, called from cmd/sandbox-agent/
// main.go) is what later builds OpenCode's own PUT /auth/openai body with
// a HARDCODED refresh:"" literal -- sourced from nowhere, least of all
// this response.
type credentialAuthValue struct {
	Type      string  `json:"type"`
	Key       *string `json:"key,omitempty"`
	Access    *string `json:"access,omitempty"`
	Expires   *int64  `json:"expires,omitempty"`
	AccountID *string `json:"accountId,omitempty"`
}

// oauthCredentialBlob is the JSON document encrypted into an oauth-kind
// provider_credentials row's own value_encrypted column (§29.4: "one
// blob, rewritten atomically on every refresh, never four separately-
// encrypted columns... {access, refresh, expires_ms, account_id}").
// Decrypted ONLY inside this handler (the one layer that ever holds
// tokenEncryptionKey, same as the api-kind path below) -- Refresh is
// parsed (required to unmarshal the blob at all) and then DELIBERATELY
// NEVER READ again past this struct -- see credentialAuthValue's own doc
// comment for the full enforcement chain.
type oauthCredentialBlob struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	ExpiresMs int64  `json:"expires_ms"`
	AccountID string `json:"account_id"`
}

// sessionRepoFullNames unmarshals rawRepos (sessions.repos' own raw JSONB
// bytes) and returns the "owner/repo" full name of each repo whose clone
// URL parses successfully via reposource.ParseOwnerRepo -- the SAME
// natural key repo_settings.repo_full_name/provider_credentials'
// own repo-scoped rows already use. A repo whose URL fails to parse is
// skipped rather than erroring the whole request -- mirrors
// sessionRepoHosts' own identical "already-trusted, already-persisted
// data; one malformed entry must not fail lookups for every other,
// well-formed repo" reasoning (scmcredentials.go).
func sessionRepoFullNames(rawRepos []byte) ([]string, error) {
	if len(rawRepos) == 0 {
		return nil, nil
	}
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rawRepos, &repos); err != nil {
		return nil, err
	}
	fullNames := make([]string, 0, len(repos))
	for _, repo := range repos {
		owner, name, err := reposource.ParseOwnerRepo(repo.URL)
		if err != nil {
			continue
		}
		fullNames = append(fullNames, owner+"/"+name)
	}
	return fullNames, nil
}

// ProviderCredentialsDelivery backs POST /sessions/{sessionID}/
// provider-credentials -- see this file's own top doc comment for the
// full outcome table and security-posture rationale.
func ProviderCredentialsDelivery(
	sessions *postgres.SessionStore,
	sandboxes *postgres.SandboxStore,
	prSessions *postgres.GitHubPRSessionStore,
	providerCredentials *postgres.ProviderCredentialStore,
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
			logger.Error("httpapi: provider-credentials: get sandbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Dead-sandbox check FIRST, before the gen/token comparisons below
		// -- same ordering scmcredentials.go/wshub/sandbox.go both use.
		if sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			writeError(w, http.StatusGone, "session stopped")
			return
		}

		presentedGen, genErr := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if genErr != nil || presentedGen != int(sandboxRow.Gen) {
			logger.Warn("httpapi: provider-credentials: rejecting: gen mismatch",
				"presented_gen_header", r.Header.Get("X-Sandbox-Gen"), "sandbox_gen", sandboxRow.Gen)
			writeError(w, http.StatusForbidden, "no usable provider credential for this session")
			return
		}

		if !verifySandboxBearerToken(token, sandboxRow.TokenHash) {
			writeError(w, http.StatusUnauthorized, "invalid sandbox token")
			return
		}

		sessionRow, err := sessions.Get(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: provider-credentials: get session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// §29.4: resolution keys on sessions.created_by, through the one
		// rule (providercredential.UserScopeTarget) the counter-reviewer's
		// own read shares: a session with no creator (an automation's), a
		// pull request's review session and a child session contribute no
		// user-scope candidate and resolve the deployment's credentials
		// alone. The review-session membership is read here, from
		// github_pr_sessions, beside the session row just loaded; a failed
		// read is a 500, never "not a review session", which would serve
		// a requester's personal link.
		scope, err := credentialscope.Load(ctx, prSessions, sessionRow)
		if err != nil {
			logger.Error("httpapi: provider-credentials: load credential scope failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		repoFullNames := scope.RepoFullNames

		rows, err := scope.Candidates(ctx, providerCredentials)
		if err != nil {
			logger.Error("httpapi: provider-credentials: list candidates failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// repoRank maps each of this session's own repo full names to its
		// position in repoFullNames (already primary-first -- see
		// credentialscope.Scope.RepoFullNames' own doc comment, §3.4
		// "position 0 = primary") -- used ONLY below to make the ScopeRepo tie-break
		// deterministic. ListProviderCredentialsForResolution's own SQL
		// query (queries/providercredentials.sql) has no secondary ORDER BY
		// key beyond `provider`, so two same-provider, different-repo rows
		// come back in whatever order Postgres happens to return them (can
		// change across a VACUUM, a row rewrite from a value rotation, a
		// query-plan change, etc.) -- array-position preservation across
		// `= ANY($1)` is not guaranteed by Postgres either, so this
		// re-establishes the primary-first order here, in Go, in front of
		// Resolve, rather than trusting raw row order.
		repoRank := make(map[string]int, len(repoFullNames))
		for i, name := range repoFullNames {
			repoRank[name] = i
		}

		byProvider := make(map[sqlcgen.ProviderCredentialProvider][]providercredential.Candidate[sqlcgen.ProviderCredential], len(rows))
		for _, row := range rows {
			byProvider[row.Provider] = append(byProvider[row.Provider], providercredential.Candidate[sqlcgen.ProviderCredential]{
				Scope: providercredential.Scope(row.Scope),
				Value: row,
			})
		}

		// Re-sort each provider's own candidate slice so that, within the
		// ScopeRepo tier, candidates appear in repoFullNames' own
		// primary-first order rather than raw SQL row order -- Resolve's
		// own documented contract (resolve.go) is "first candidate in
		// input order wins" a same-scope tie; this is the one place that
		// order gets established for real multi-repo session data. Only
		// ScopeRepo can ever tie this way: scope=global's own
		// scope_target_id is always NULL, and scope=environment is pinned
		// to this session's single environment_id, so each provider has at
		// most 1 global and at most 1 environment candidate here -- both
		// non-repo scopes get a fixed, before-everything rank (-1) from
		// repoTieBreakRank, so this sort is a no-op for them either way
		// (sort.SliceStable preserves their original relative order for
		// equal keys).
		for provider, candidates := range byProvider {
			sort.SliceStable(candidates, func(i, j int) bool {
				return repoTieBreakRank(candidates[i], repoRank) < repoTieBreakRank(candidates[j], repoRank)
			})
			byProvider[provider] = candidates
		}

		credentials := make(map[string]credentialAuthValue, len(byProvider))
		for provider, candidates := range byProvider {
			winner, ok := providercredential.Resolve(candidates)
			if !ok {
				continue
			}
			plaintext, err := platform.DecryptToken(tokenEncryptionKey, winner.ValueEncrypted)
			if err != nil {
				// Never logs the ciphertext/plaintext -- see
				// platform.DecryptToken's own doc comment. Logged and
				// SKIPPED, not a 500 for the whole request -- see this
				// file's own top doc comment for why.
				logger.Error("httpapi: provider-credentials: decrypt failed",
					"error", err, "provider", string(provider), "scope", string(winner.Scope))
				continue
			}

			// (§29.6): split by kind -- api_key re-labels today's
			// plaintext-string behavior into the "api" Auth-union member;
			// oauth parses the decrypted {access, refresh, expires_ms,
			// account_id} blob and builds the "oauth" member WITHOUT ever
			// reading Refresh past oauthCredentialBlob itself (see
			// credentialAuthValue's own doc comment).
			switch winner.Kind {
			case sqlcgen.ProviderCredentialKindOauth:
				var blob oauthCredentialBlob
				if err := json.Unmarshal(plaintext, &blob); err != nil {
					// Never logs plaintext -- matches the decrypt-failure
					// branch immediately above. Logged and SKIPPED, not a
					// 500 for the whole request.
					logger.Error("httpapi: provider-credentials: parse oauth blob failed",
						"error", err, "provider", string(provider))
					continue
				}
				expires := blob.ExpiresMs
				credentials[string(provider)] = credentialAuthValue{
					Type:      "oauth",
					Access:    &blob.Access,
					Expires:   &expires,
					AccountID: &blob.AccountID,
				}
			default:
				value := string(plaintext)
				credentials[string(provider)] = credentialAuthValue{Type: "api", Key: &value}
			}
		}

		writeJSON(w, http.StatusOK, providerCredentialsResponse{Credentials: credentials})
	}
}

// repoTieBreakRank returns candidate's own position in repoRank
// (repoFullNames' primary-first order, §3.4 "position 0 = primary") when
// candidate is a ScopeRepo row for a repo this session actually names, or
// -1 otherwise (ScopeEnvironment/ScopeGlobal, or -- defensively -- a
// ScopeRepo row whose ScopeTargetID somehow isn't among repoFullNames at
// all) so it sorts before every ranked ScopeRepo candidate and never
// participates in the repo-vs-repo tie-break this function exists for --
// see ProviderCredentialsDelivery's own re-sort comment above for why only
// ScopeRepo can ever have more than one same-provider candidate here.
func repoTieBreakRank(candidate providercredential.Candidate[sqlcgen.ProviderCredential], repoRank map[string]int) int {
	if candidate.Scope != providercredential.ScopeRepo || candidate.Value.ScopeTargetID == nil {
		return -1
	}
	rank, ok := repoRank[*candidate.Value.ScopeTargetID]
	if !ok {
		return -1
	}
	return rank
}

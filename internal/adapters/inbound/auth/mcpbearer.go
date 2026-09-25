package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// MCPAccessTokenLookup is RequireMCPBearer's one store dependency --
// *postgres.MCPOAuthGrantStore in production. An interface only so this
// package's unit tests can prove the no-cache property against a store
// whose answer changes between two calls.
type MCPAccessTokenLookup interface {
	LookupAccessToken(ctx context.Context, tokenHash string) (postgres.MCPAccessTokenPrincipal, error)
	TouchGrantLastUsed(ctx context.Context, grantID pgtype.UUID, staleBefore time.Time) error
}

// MCPBearerConfig is what RequireMCPBearer checks a token against and
// what its 401 challenge advertises -- all derived once, at boot, from
// PublicBaseURL and the MCP tool table (controlplane/serve.go).
type MCPBearerConfig struct {
	// Resource is this deployment's canonical MCP resource URI. A token
	// whose grant was issued for any other resource is refused
	// (audience binding, technical plan §43.16).
	Resource string
	// ResourceMetadataURL is the protected-resource metadata URL the 401
	// challenge points a client at.
	ResourceMetadataURL string
	// Scopes is every advertised scope, carried in the challenge's own
	// scope parameter (technical plan §43.17).
	Scopes []string
	// LastUsedWriteInterval is platform.Timeouts.
	// MCPGrantLastUsedWriteInterval.
	LastUsedWriteInterval time.Duration
}

// RequireMCPBearer is the /mcp route group's authentication gate (technical
// plan §43.2/§43.16), in place of Middleware: it accepts ONLY an
// "Authorization: Bearer" access token issued by internal/adapters/inbound/
// mcpauth, and never a cookie.
//
// On every request it hashes the presented token and reads the token, its
// grant, its client and its user in ONE lookup -- there is no cache of any
// kind in front of that read, so a revoked grant, a deleted or disabled
// client, a disabled user or a changed role all take effect on the very
// next call. It refuses unless the token and grant are unexpired, the
// client and user are not disabled, and the grant's resource is this
// deployment's own.
//
// A refusal is the same 401 {"error":"unauthorized"} body every other
// route answers, plus a WWW-Authenticate: Bearer challenge carrying
// resource_metadata and scope -- and error="invalid_token" when a token
// was presented (RFC 6750 §3.1: none when no credential was sent). The
// reason is logged, never returned. A lookup that fails for any reason
// other than "no such token" answers 500, not 401: a database fault must
// not send a client back through consent.
//
// Success attaches the SAME platform.AuthenticatedUser a cookie would
// (read from the users row on this call) plus the platform.MCPGrant, and
// strips the Authorization header from the request it passes on: nothing
// downstream -- the SDK handler, a tool, a REST twin -- ever holds the
// token (the MCP spec's token-passthrough prohibition; the bridge's own
// empty-header request is the second, independent layer).
func RequireMCPBearer(tokens MCPAccessTokenLookup, cfg MCPBearerConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			logger := platform.Logger(ctx)

			token, presented, ok := bearerToken(r)
			if !ok {
				if presented {
					logger.Warn("auth: mcp bearer rejected: malformed authorization header")
				} else {
					logger.Warn("auth: mcp bearer rejected: no bearer token")
				}
				writeBearerUnauthorized(w, cfg, presented)
				return
			}

			p, err := tokens.LookupAccessToken(ctx, platform.HashToken(token))
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("auth: mcp bearer rejected: token not found")
				writeBearerUnauthorized(w, cfg, true)
				return
			}
			if err != nil {
				logger.Error("auth: mcp bearer: lookup failed", "error", err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal error"}`))
				return
			}

			now := time.Now()
			if reason := refusalReason(p, cfg, now); reason != "" {
				logger.Warn("auth: mcp bearer rejected", "reason", reason, "grant_id", p.GrantID.String())
				writeBearerUnauthorized(w, cfg, true)
				return
			}

			if p.GrantLastUsedAt.IsZero() || p.GrantLastUsedAt.Before(now.Add(-cfg.LastUsedWriteInterval)) {
				// Best effort: a failed stamp is logged and never decides
				// the call.
				if err := tokens.TouchGrantLastUsed(ctx, p.GrantID, now.Add(-cfg.LastUsedWriteInterval)); err != nil {
					logger.Warn("auth: mcp bearer: stamp last used failed", "error", err)
				}
			}

			ctx = platform.WithUser(ctx, platform.AuthenticatedUser{
				ID:    p.UserID.String(),
				Role:  p.UserRole,
				Email: p.UserEmail,
			})
			ctx = platform.WithMCPGrant(ctx, platform.MCPGrant{
				GrantID:  p.GrantID.String(),
				ClientID: p.ClientID,
				Scopes:   p.GrantScopes,
			})
			forward := r.WithContext(ctx)
			forward.Header = r.Header.Clone()
			forward.Header.Del("Authorization")
			next.ServeHTTP(w, forward)
		})
	}
}

// refusalReason returns why p may not authenticate a call now, or "" when
// it may.
func refusalReason(p postgres.MCPAccessTokenPrincipal, cfg MCPBearerConfig, now time.Time) string {
	switch {
	case !p.TokenExpiresAt.After(now):
		return "token expired"
	case !p.GrantExpiresAt.After(now):
		return "grant expired"
	case p.ClientDisabled:
		return "client disabled"
	case p.UserDisabled:
		return "user disabled"
	case p.GrantResource != cfg.Resource:
		return "grant issued for another resource"
	default:
		return ""
	}
}

// bearerToken extracts an RFC 6750 §2.1 bearer token: exactly one
// Authorization header, scheme "Bearer" (case-insensitive), then one
// b64token. presented reports whether any Authorization header was sent
// at all; ok is false for a missing or malformed credential. A token in
// the query string is never read.
func bearerToken(r *http.Request) (token string, presented, ok bool) {
	vals := r.Header.Values("Authorization")
	if len(vals) == 0 {
		return "", false, false
	}
	if len(vals) > 1 {
		return "", true, false
	}
	scheme, rest, found := strings.Cut(vals[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", true, false
	}
	token = strings.TrimLeft(rest, " ")
	if !isB64Token(token) {
		return "", true, false
	}
	return token, true, true
}

// isB64Token reports whether s is an RFC 6750 b64token:
// 1*( ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/" ) *"=".
func isB64Token(s string) bool {
	body := strings.TrimRight(s, "=")
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '+', c == '/':
		default:
			return false
		}
	}
	return true
}

// bearerChallenge renders the WWW-Authenticate value for a refusal.
func bearerChallenge(cfg MCPBearerConfig, invalidToken bool) string {
	params := make([]string, 0, 3)
	if invalidToken {
		params = append(params, `error="invalid_token"`)
	}
	params = append(params, `resource_metadata="`+quoteChallengeValue(cfg.ResourceMetadataURL)+`"`)
	if len(cfg.Scopes) > 0 {
		params = append(params, `scope="`+quoteChallengeValue(strings.Join(cfg.Scopes, " "))+`"`)
	}
	return "Bearer " + strings.Join(params, ", ")
}

// quoteChallengeValue escapes a quoted-string's two special characters
// (RFC 9110 §5.6.4).
func quoteChallengeValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// writeBearerUnauthorized writes the 401 every refusal shares: the same
// generic body as writeUnauthorized, plus the bearer challenge.
func writeBearerUnauthorized(w http.ResponseWriter, cfg MCPBearerConfig, invalidToken bool) {
	w.Header().Set("WWW-Authenticate", bearerChallenge(cfg, invalidToken))
	w.Header().Set("Cache-Control", "no-store")
	writeUnauthorized(w)
}

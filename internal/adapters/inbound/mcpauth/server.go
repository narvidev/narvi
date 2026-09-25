package mcpauth

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
	"github.com/narvidev/narvi/internal/platform"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Config is New's own configuration.
type Config struct {
	// PublicBaseURL is platform.Config.PublicBaseURL; every identifier
	// the server advertises derives from it (DeriveIdentifiers).
	PublicBaseURL string
	// Enabled is platform.Config.MCPEnabled. The routes are mounted
	// either way (behind the 503 enabled-gate); Enabled only decides
	// whether New insists that PublicBaseURL is a bare origin, so a
	// deployment that never turns the surface on is never refused boot
	// over it.
	Enabled bool
	// Scopes is every scope this build advertises -- exactly what the
	// MCP tool table requires (internal/adapters/inbound/mcp's
	// AdvertisedScopes). The authorization endpoint refuses anything
	// else.
	Scopes []mcpscope.Scope
	// Timeouts supplies the MCP lifetimes (technical plan §43.16).
	Timeouts platform.Timeouts
}

// Deps are the stores the authorization server reads and writes.
type Deps struct {
	Pool         *pgxpool.Pool
	Clients      *postgres.MCPOAuthClientStore
	Grants       *postgres.MCPOAuthGrantStore
	UserSessions *postgres.UserSessionStore
	Users        *postgres.UserStore
	AuditLog     *postgres.AuditLogStore
}

// Server is the MCP authorization server's HTTP surface (technical plan
// §43.13-§43.16). Every exported method is one route's handler.
type Server struct {
	cfg        Config
	deps       Deps
	ids        Identifiers
	scopes     []mcpscope.Scope
	prmJSON    []byte
	asmJSON    []byte
	consentTpl *template.Template
	errorTmpl  *template.Template
}

// New builds a Server. It fails for a PublicBaseURL that is not an
// absolute URL, for one that is not a bare origin while Enabled
// (ValidatePublicBaseURL), and for a scope outside mcpscope's vocabulary
// (a wiring defect).
func New(cfg Config, deps Deps) (*Server, error) {
	ids, err := DeriveIdentifiers(cfg.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.Enabled {
		if err := ValidatePublicBaseURL(cfg.PublicBaseURL); err != nil {
			return nil, err
		}
	}
	scopes := make([]mcpscope.Scope, 0, len(cfg.Scopes))
	for _, sc := range cfg.Scopes {
		if !mcpscope.Known(sc) {
			return nil, fmt.Errorf("mcpauth: advertised scope %q is not in mcpscope.Vocabulary", sc)
		}
		scopes = append(scopes, sc)
	}
	consentTpl, err := template.ParseFS(templatesFS, "templates/consent.html")
	if err != nil {
		return nil, fmt.Errorf("mcpauth: parse consent template: %w", err)
	}
	errorTmpl, err := template.ParseFS(templatesFS, "templates/error.html")
	if err != nil {
		return nil, fmt.Errorf("mcpauth: parse error template: %w", err)
	}
	s := &Server{cfg: cfg, deps: deps, ids: ids, scopes: scopes, consentTpl: consentTpl, errorTmpl: errorTmpl}
	if s.prmJSON, err = json.Marshal(s.protectedResourceMetadata()); err != nil {
		return nil, fmt.Errorf("mcpauth: encode protected resource metadata: %w", err)
	}
	if s.asmJSON, err = json.Marshal(s.authorizationServerMetadata()); err != nil {
		return nil, fmt.Errorf("mcpauth: encode authorization server metadata: %w", err)
	}
	return s, nil
}

// Identifiers returns the identifiers this server advertises.
func (s *Server) Identifiers() Identifiers { return s.ids }

// scopeDescriptions is what the consent page says each scope allows. Every
// scope in mcpscope.Vocabulary must have one (TestScopeDescriptions_Cover
// Vocabulary): the page never shows a user a scope it cannot explain.
var scopeDescriptions = map[mcpscope.Scope]string{
	mcpscope.Read:  "Read the model catalog and this deployment's sessions, with exactly the visibility your own account has.",
	mcpscope.Write: "Act on sessions on your behalf, within what your own role allows.",
}

// errNoDescription is returned by describeScope for a scope with no
// consent-page description.
var errNoDescription = errors.New("mcpauth: scope has no consent description")

func describeScope(sc mcpscope.Scope) (string, error) {
	d, ok := scopeDescriptions[sc]
	if !ok {
		return "", fmt.Errorf("%w: %q", errNoDescription, sc)
	}
	return d, nil
}

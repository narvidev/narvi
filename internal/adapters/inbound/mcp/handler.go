package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/platform"
)

// Config is NewHandler's own configuration -- deliberately small: this
// package needs exactly one platform.Config field (PublicBaseURL, to
// resolve the one origin this surface trusts), never a whole
// *platform.Config (which would let this package reach for an unrelated
// field it has no business reading).
type Config struct {
	// PublicBaseURL is platform.Config.PublicBaseURL -- this deployment's
	// own externally-reachable base URL (already required config, used
	// for the OAuth RedirectURL the same way). Its origin (scheme+host,
	// no path) is the ONE origin RequireTrustedOrigin below ever accepts
	// (technical plan §43.2) -- a browser cross-site POST from anywhere
	// else is refused 403; a request with no Origin/Sec-Fetch-Site header
	// at all (every non-browser client) passes.
	PublicBaseURL string
}

// crossOriginProtection builds the *http.CrossOriginProtection value
// trusting cfg.PublicBaseURL's own origin, for NewHandler's own SECOND,
// defense-in-depth Origin layer (its returned handler's own
// StreamableHTTPOptions.CrossOriginProtection field, below) -- NOT for
// RequireTrustedOrigin, which performs its own explicit comparison
// instead of delegating to this type (see that function's own doc
// comment for why). Kept as a second, independent layer in case a future
// mount of NewHandler's returned handler is ever reachable without
// RequireTrustedOrigin in front of it.
func crossOriginProtection(cfg Config) (*http.CrossOriginProtection, error) {
	origin, err := originOf(cfg.PublicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("mcp: resolve trusted origin from PublicBaseURL %q: %w", cfg.PublicBaseURL, err)
	}
	protection := http.NewCrossOriginProtection()
	if err := protection.AddTrustedOrigin(origin); err != nil {
		return nil, fmt.Errorf("mcp: add trusted origin %q: %w", origin, err)
	}
	return protection, nil
}

// originForbiddenBody is RequireTrustedOrigin's own 403 body -- a fixed,
// generic message (never echoing the request's own Origin value back,
// which would just be reflecting attacker-controlled text).
const originForbiddenBody = `{"error":"cross-origin request denied"}`

// RequireTrustedOrigin returns chi middleware enforcing the Streamable
// HTTP transport's own Origin check -- mounted FIRST in the /mcp route
// group's own chain (technical plan §43.2; controlplane/serve.go),
// BEFORE RequireEnabled, auth.Middleware, or NewHandler's own returned
// handler (which also carries a SEPARATE, defense-in-depth Origin check,
// wired into the SDK's own StreamableHTTPOptions -- kept there too, but
// no longer the check that determines what a client actually observes).
//
// This performs its OWN explicit comparison -- an Origin header, if
// present, must resolve to EXACTLY cfg.PublicBaseURL's own origin, or the
// request is refused 403 -- rather than delegating to net/http's own
// CrossOriginProtection (which crossOriginProtection above still builds,
// for NewHandler's separate inner layer). That stdlib type exempts two
// shapes the Streamable HTTP transport spec's own Origin check exists
// specifically to catch: a request whose Sec-Fetch-Site header reads
// "same-origin"/"none", and -- load-bearing here -- a request whose
// Origin equals its OWN Host header, checked BEFORE its trusted-origin
// list is ever consulted (Go's net/http/csrf.go, CrossOriginProtection.
// Check). The second shape is exactly a DNS-rebinding request: an
// attacker-controlled hostname resolved to this deployment's own IP, with
// Host and Origin both naming that attacker hostname -- round 2 review of
// PR #324, finding N12, which caught a prior revision of this function
// (protection.Handler, unmodified) answering 503/401 for such a request
// instead of the 403 this comment and technical plan §43.2 both already
// claimed unconditionally. Comparing directly against cfg.PublicBaseURL's
// own origin, with no exemption, is what makes that claim true.
//
// This gate is load-bearing beyond that one gap, too: the Streamable HTTP
// transport spec requires "if the Origin header is present and invalid,
// servers MUST respond with HTTP 403 Forbidden" UNCONDITIONALLY -- not
// "once the surface is known to be enabled" or "once the caller is
// authenticated". Mounting this check FIRST, ahead of RequireEnabled,
// auth.Middleware, and versionGate, means an invalid Origin gets 403
// whatever the flag/auth/version state of the rest of the request --
// never the 503/401/-32022 those later gates would otherwise answer
// first.
func RequireTrustedOrigin(cfg Config) (func(http.Handler) http.Handler, error) {
	trusted, err := originOf(cfg.PublicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("mcp: resolve trusted origin from PublicBaseURL %q: %w", cfg.PublicBaseURL, err)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); origin != "" {
				got, err := originOf(origin)
				if err != nil || got != trusted {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(originForbiddenBody))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// NewHandler builds the complete /mcp POST handler (technical plan §43):
// OUR OWN version gate (versions.go) wrapping the official SDK's
// Streamable HTTP handler, stateless, JSON-response, Origin-protected,
// body-size-capped at the same MaxRequestBodyBytes httpapi uses. twins
// are the pre-built REST handlers this build's three tools invoke
// in-process (Twins' own doc comment) -- constructed by controlplane,
// the composition root, which alone holds the Postgres stores those
// closures capture.
//
// The returned handler's OWN CrossOriginProtection (below) is
// defense-in-depth, not this surface's primary Origin check anymore --
// see RequireTrustedOrigin's own doc comment for why a SECOND gate,
// mounted first in the route group, is what actually determines the
// observable 403 behavior now.
//
// Returns an error for a malformed cfg.PublicBaseURL (required,
// non-empty config every OAuth redirect URL in this binary already
// depends on being a real absolute URL -- see Config.PublicBaseURL's own
// doc comment); a real deployment's own boot-time GitHub OAuth wiring
// already depends on that same assumption holding. Also returns an error
// if any of the three 180 tools' own input $defs fails to compile
// (schemas.go's compileInputSchemas) -- deliberately here, at BOOT, not
// discovered lazily on some later request: round 2 review of PR #324,
// findings N1/N3/N4. See compileInputSchemas' own doc comment for why
// compiling every tool's schema here, once, and threading the resulting
// immutable map into every request's own *sdkmcp.Server (buildServer
// below) is what removes the concurrent-first-call crash entirely,
// rather than merely making it less likely.
func NewHandler(cfg Config, twins Twins) (http.Handler, error) {
	protection, err := crossOriginProtection(cfg)
	if err != nil {
		return nil, err
	}
	inputSchemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		return nil, fmt.Errorf("mcp: compile tool input schemas: %w", err)
	}

	sdkHandler := sdkmcp.NewStreamableHTTPHandler(
		func(r *http.Request) *sdkmcp.Server { return buildServer(r, twins, inputSchemas) },
		&sdkmcp.StreamableHTTPOptions{
			// Stateless (§43.3): the 2026-07-28 era is served ONLY in
			// stateless mode by the pinned SDK, the protocol itself is
			// declared stateless, and a protocol-level session table
			// would be a second authority over state (§5.1 forbids
			// exactly that).
			Stateless: true,
			// JSONResponse (§43.3): every 180 tool is a short DB read: a
			// plain application/json response avoids SSE plumbing and
			// keep-alive timers entirely, so no new platform/timeouts.go
			// constant is needed for this Step.
			JSONResponse:          true,
			CrossOriginProtection: protection,
			// The SAME 1 MiB cap httpapi.MaxRequestBodyBytes already
			// enforces on every REST body this codebase decodes.
			MaxRequestBodyBytes: MaxRequestBodyBytes,
			// DisableLocalhostProtection is deliberately left false (the
			// default): the SDK's own DNS-rebinding guard for
			// loopback-bound servers is an additional, independent
			// protection this design does not need to opt out of.
		},
	)

	return rejectBatches(versionGate(sdkHandler)), nil
}

// originOf parses rawURL (platform.Config.PublicBaseURL) into its own
// origin -- scheme + host, no path -- the shape net/http.
// CrossOriginProtection.AddTrustedOrigin requires.
func originOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("not an absolute URL (missing scheme or host): %q", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// serverOptions is shared by buildServer's own real path and its defect
// fallback, so both advertise the identical protocol-version list,
// capabilities, and instructions -- only whether a tool CALL can ever
// succeed differs between the two.
func serverOptions() *sdkmcp.ServerOptions {
	return &sdkmcp.ServerOptions{
		Instructions: instructions,
		// {"tools":{}} only (technical plan §43.5): no listChanged
		// (the tool set is static per build), no resources, no prompts,
		// no logging capability. Setting Capabilities explicitly (rather
		// than leaving it nil) is what suppresses the SDK's own default
		// {"logging":{}} capability -- see mcp.ServerOptions.Capabilities'
		// own doc comment.
		Capabilities: &sdkmcp.ServerCapabilities{Tools: &sdkmcp.ToolCapabilities{}},
		// The single source of truth (versions.go) -- this can only
		// NARROW the SDK's own broader default list, never widen it.
		SupportedProtocolVersions: SupportedProtocolVersions,
	}
}

// implementation is this build's own serverInfo -- contracts.Version
// (not a hand-maintained version string) is what a client can actually
// reason about: which DTO shapes it will get back (technical plan §43.5).
func implementation() *sdkmcp.Implementation {
	return &sdkmcp.Implementation{Name: "narvi", Version: contracts.Version}
}

// buildServer is this handler's own getServer (called once per incoming
// HTTP request in stateless mode -- mcp.NewStreamableHTTPHandler's own
// documented contract). It reads platform.UserFromContext(r.Context())
// -- guaranteed present, because this handler is only ever reached
// behind auth.Middleware (controlplane/serve.go's own /mcp route group)
// -- and builds an *sdkmcp.Server whose three tool closures capture
// r.Context() itself, not the context the SDK later passes into each
// tool handler at call time: capturing HERE is simpler to reason about
// and is what technical plan §43.7 specifies, and in stateless mode
// (a fresh Server per HTTP request) there is no "long-lived session, stale
// context" concern that would make the distinction matter.
//
// If no authenticated user is found -- unreachable behind auth.
// Middleware, defended against anyway, mirroring httpapi.
// authenticatedUserID's own identical "should never happen, defended
// against anyway" precedent -- or if a tool's own contracts-sourced
// schema fails to resolve (a defect in this build, never a legitimate
// per-request outcome), this returns a server that still advertises the
// same three tools (so tools/list stays stable) but whose every call
// answers -32603, logged loudly server-side.
func buildServer(r *http.Request, twins Twins, inputSchemas map[string]*jsonschema.Schema) *sdkmcp.Server {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	if _, ok := platform.UserFromContext(ctx); !ok {
		logger.Error("mcp: no authenticated user in context (route not mounted behind auth.Middleware?)")
		return defectServer(logger)
	}

	server := sdkmcp.NewServer(implementation(), serverOptions())
	if err := registerTools(ctx, server, twins, inputSchemas); err != nil {
		logger.Error("mcp: register tools failed", "error", err)
		return defectServer(logger)
	}
	return server
}

// defectServer builds a server advertising the same three tools (each
// resolved with a zero-value Twins{} -- never invoked, since every
// handler below answers -32603 unconditionally) so a client's tools/list
// call still sees a stable, correctly-shaped catalog even while every
// actual call fails loudly. Used only by buildServer's own two defensive
// branches (doc comment above) -- never reachable in ordinary operation.
func defectServer(logger *slog.Logger) *sdkmcp.Server {
	server := sdkmcp.NewServer(implementation(), serverOptions())
	for _, spec := range toolSpecs(Twins{}) {
		tool, err := buildTool(spec)
		if err != nil {
			logger.Error("mcp: defectServer: build tool failed", "tool", spec.Name, "error", err)
			continue
		}
		server.AddTool(tool, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
		})
	}
	return server
}

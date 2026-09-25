package mcp

import (
	"fmt"
	"net/http"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
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
	origin, err := platform.CanonicalOrigin(cfg.PublicBaseURL)
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
// BEFORE RequireEnabled, auth.RequireMCPBearer, or NewHandler's own returned
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
// auth.RequireMCPBearer, and versionGate, means an invalid Origin gets 403
// whatever the flag/auth/version state of the rest of the request --
// never the 503/401/-32022 those later gates would otherwise answer
// first.
func RequireTrustedOrigin(cfg Config) (func(http.Handler) http.Handler, error) {
	trusted, err := platform.CanonicalOrigin(cfg.PublicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("mcp: resolve trusted origin from PublicBaseURL %q: %w", cfg.PublicBaseURL, err)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// No Origin header at all: per this function's own doc
				// comment, every non-browser MCP client passes. A BROWSER
				// making a cross-site request, though, always sets its own
				// Sec-Fetch-Site fetch-metadata header (a caller cannot
				// forge or omit this from script -- the user agent alone
				// sets it), so a request naming "cross-site" here despite
				// carrying no Origin is refused too (round 3 review of PR
				// #324, finding R4): this handler's own doc comment already
				// claimed exactly this outcome, but the code never checked
				// it.
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					writeOriginForbidden(w)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			got, err := platform.CanonicalOrigin(origin)
			if err != nil || got != trusted {
				writeOriginForbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// writeOriginForbidden writes RequireTrustedOrigin's own fixed 403 body.
func writeOriginForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(originForbiddenBody))
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
//
// NewHandler does only that compile; newHandler below builds everything
// else from the map it produces (round 6 review of PR #324, findings
// U1/U2/U3).
func NewHandler(cfg Config, twins Twins) (http.Handler, error) {
	inputSchemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		return nil, fmt.Errorf("mcp: compile tool input schemas: %w", err)
	}
	return newHandler(cfg, twins, inputSchemas)
}

// newHandler is NewHandler with its tool input schemas injected: the
// middleware chain, the per-request closure, buildServer and
// registerTools -- everything except the compile itself. It is the
// outermost seam at which this package's tests can hand the real request
// path a schema map of their own (toolhandler_test.go).
//
// It refuses to build a handler at all -- a boot failure, like a compile
// error in NewHandler -- unless inputSchemas holds a non-nil
// *jsonschema.Schema for every name toolInputDefs() returns, and every
// request validates against a private copy of those entries taken here,
// never against inputSchemas itself (copyCompleteInputSchemas). Two
// things follow by construction:
//
//   - toolHandler's missing-schema branch (tools.go) is unreachable from
//     any handler this function builds, so the request path never lacks
//     a schema it might be tempted to go and compile;
//   - the copy is reachable only through the per-request closure below,
//     which hands it to buildServer -> registerTools -> toolHandler, and
//     all three only read it -- so neither a caller changing its own map
//     after this returns nor anything a request does can change what a
//     later request validates against.
func newHandler(cfg Config, twins Twins, inputSchemas map[string]*jsonschema.Schema) (http.Handler, error) {
	protection, err := crossOriginProtection(cfg)
	if err != nil {
		return nil, err
	}
	schemas, err := copyCompleteInputSchemas(inputSchemas)
	if err != nil {
		return nil, err
	}

	sdkHandler := sdkmcp.NewStreamableHTTPHandler(
		func(r *http.Request) *sdkmcp.Server { return buildServer(r, twins, schemas) },
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

// copyCompleteInputSchemas returns a fresh map holding inputSchemas'
// entry for every name toolInputDefs() returns, or an error naming the
// first such name whose entry is missing or nil. An entry for any other
// name is not copied: no tool ever looks it up.
func copyCompleteInputSchemas(inputSchemas map[string]*jsonschema.Schema) (map[string]*jsonschema.Schema, error) {
	defs := toolInputDefs()
	out := make(map[string]*jsonschema.Schema, len(defs))
	for _, name := range defs {
		sch, ok := inputSchemas[name]
		if !ok {
			return nil, fmt.Errorf("mcp: tool input schema map has no entry for %q", name)
		}
		if sch == nil {
			return nil, fmt.Errorf("mcp: tool input schema map has a nil entry for %q", name)
		}
		out[name] = sch
	}
	return out, nil
}

// serverOptions builds the options every per-request server shares --
// the same protocol-version list and capabilities for every request --
// with the instructions paragraph composed for that request's own visible
// tools (instructionsFor).
func serverOptions(instructions string) *sdkmcp.ServerOptions {
	return &sdkmcp.ServerOptions{
		Instructions: instructions,
		// {"tools":{}} only (technical plan §43.5): no listChanged
		// (the tool set is static per request), no resources, no prompts,
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
// documented contract). It reads the request's principal
// (platform.UserFromContext) and the MCP grant it was authenticated under
// (platform.MCPGrantFromContext) -- both attached by auth.RequireMCPBearer,
// the /mcp route group's own gate (controlplane/serve.go) -- and builds an
// *sdkmcp.Server holding ONLY the tools that grant's scopes satisfy
// (technical plan §43.17): a tool the grant does not cover is never
// registered, so tools/list omits it and a tools/call naming it answers
// the SDK's own "unknown tool" error, exactly as for a name that never
// existed. The instructions paragraph is composed from the same visible
// set. Each tool closure captures r.Context() itself (technical plan
// §43.7); in stateless mode (a fresh Server per HTTP request) there is no
// "long-lived session, stale context" concern that would make the
// distinction matter.
//
// A request that reaches this point without BOTH a principal and a grant
// -- unreachable behind RequireMCPBearer, defended against anyway, like
// httpapi.authenticatedUserID's own "should never happen" precedent -- or
// whose tool schemas fail to resolve (a defect in this build) gets
// defectServer: no tools at all, logged loudly server-side.
func buildServer(r *http.Request, twins Twins, inputSchemas map[string]*jsonschema.Schema) *sdkmcp.Server {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	if _, ok := platform.UserFromContext(ctx); !ok {
		logger.Error("mcp: no authenticated user in context (route not mounted behind auth.RequireMCPBearer?)")
		return defectServer()
	}
	grant, ok := platform.MCPGrantFromContext(ctx)
	if !ok {
		logger.Error("mcp: no MCP grant in context (route not mounted behind auth.RequireMCPBearer?)")
		return defectServer()
	}

	visible := visibleSpecs(toolSpecs(twins), mcpscope.FromStrings(grant.Scopes))
	server := sdkmcp.NewServer(implementation(), serverOptions(instructionsFor(visible)))
	if err := registerTools(ctx, server, visible, inputSchemas); err != nil {
		logger.Error("mcp: register tools failed", "error", err)
		return defectServer()
	}
	return server
}

// defectServer builds a server with NO tools, for buildServer's own
// defensive branches (its doc comment). A request without a principal and
// a grant must see nothing: advertising the tool table to it would
// describe the deployment to a caller no authorization vouches for
// (technical plan §43.17). tools/list answers an empty list, every
// tools/call the SDK's own "unknown tool" error, and instructions name no
// tool.
func defectServer() *sdkmcp.Server {
	return sdkmcp.NewServer(implementation(), serverOptions(instructionsFor(nil)))
}

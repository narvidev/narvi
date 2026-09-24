package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
)

// Twins bundles the pre-built REST handlers this package's tools invoke
// in-process (doc.go's own "one authorization path" section) -- each
// field is the SAME http.HandlerFunc value httpapi's own router
// registers for the browser-facing route of the same name (e.g.
// httpapi.GetModelCatalog(), httpapi.ListSessions(sessionStore)),
// constructed by controlplane (the composition root), which alone holds
// the Postgres stores those closures capture. This package never sees a
// store type at all -- tools/lint/narvichecks' mcpimportban analyzer
// enforces that structurally, and Twins' own shape (three
// http.HandlerFunc fields, nothing else) is why the ban is possible: a
// caller cannot even ATTEMPT to hand this package a *postgres.
// SessionStore instead of a handler, because there is no field to put it
// in.
type Twins struct {
	// ListModels is httpapi.GetModelCatalog() -- narvi_list_models' own
	// twin.
	ListModels http.HandlerFunc
	// ListSessions is httpapi.ListSessions(sessionStore) --
	// narvi_list_sessions' own twin.
	ListSessions http.HandlerFunc
	// GetSession is httpapi.GetSession(sessionStore) -- narvi_get_session's
	// own twin.
	GetSession http.HandlerFunc
}

// toolSpec is the ONLY place a 180 tool is declared: its wire name, its
// REST twin, the two contracts $def names its schemas come from
// (verbatim for input, bundled for output -- schemas.go), and a
// BuildRequest closure translating the raw `arguments` object a client
// sent into the twin's own URL params / query string.
type toolSpec struct {
	Name         string
	Description  string
	Twin         twin
	InputDef     string
	OutputDef    string
	BuildRequest func(arguments json.RawMessage) (urlParams map[string]string, query url.Values, err error)
}

// instructions is server/discover's and the legacy initialize handshake's
// own "instructions" field (technical plan §43.5) -- one paragraph
// telling the model what this deployment's three tools actually are.
const instructions = "This server exposes three READ-ONLY tools over this deployment's session data: narvi_list_models (the model catalog), narvi_list_sessions (this deployment's sessions; filter:\"all\" lists EVERY session on the deployment, not only the caller's own), and narvi_get_session (one session's full detail). None of these tools writes anything, and none of the write/plan/stop tools a later Step adds exist on this build."

// noArguments is the BuildRequest closure every argument-less tool
// shares (only narvi_list_models today): no urlParams, no query, no
// possible error -- the twin is invoked exactly once, unconditionally.
func noArguments(json.RawMessage) (map[string]string, url.Values, error) {
	return nil, nil, nil
}

// buildListSessionsRequest unmarshals arguments as a restdtos.
// ListSessionsToolRequest and builds ?filter=&limit= exactly as a client
// calling GET /api/sessions directly would -- omitting either query key
// entirely when the caller left the corresponding field unset, so the
// twin's OWN defaulting (filter defaults "mine"; limit defaults
// listSessionsDefaultLimit) runs completely unchanged.
func buildListSessionsRequest(arguments json.RawMessage) (map[string]string, url.Values, error) {
	var in restdtos.ListSessionsToolRequest
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return nil, nil, err
		}
	}
	query := url.Values{}
	if in.Filter != nil {
		query.Set("filter", *in.Filter)
	}
	if in.Limit != nil {
		query.Set("limit", strconv.Itoa(*in.Limit))
	}
	return nil, query, nil
}

// buildGetSessionRequest unmarshals arguments as a restdtos.
// GetSessionToolRequest (whose own generated UnmarshalJSON already
// enforces "sessionId" is present) and maps it onto the twin's own chi
// URL param name, "sessionID" -- GET /api/sessions/{sessionID}'s own
// path parameter name, unrelated to the wire argument's own camelCase
// "sessionId" spelling.
func buildGetSessionRequest(arguments json.RawMessage) (map[string]string, url.Values, error) {
	var in restdtos.GetSessionToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return nil, nil, err
	}
	return map[string]string{"sessionID": in.SessionId}, nil, nil
}

// toolSpecs is the tool table itself -- registerTools below and
// TestEveryToolHasARegisteredTwin both read this SAME function (the
// latter with a zero-value Twins{}, since it only ever inspects Twin.
// method/pathTemplate against routes.golden, never invokes a handler).
func toolSpecs(twins Twins) []toolSpec {
	return []toolSpec{
		{
			Name:         "narvi_list_models",
			Description:  "List every model this deployment's catalog offers (provider, context window, cost, reasoning-effort variants) -- the same catalog GET /api/models returns. Read-only; every authenticated role may call it.",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/models", handler: twins.ListModels},
			InputDef:     "ListModelsToolRequest",
			OutputDef:    "ModelCatalog",
			BuildRequest: noArguments,
		},
		{
			Name:         "narvi_list_sessions",
			Description:  "List this deployment's sessions -- the same list GET /api/sessions returns. filter:\"mine\" (the default) returns sessions the caller created or joined; filter:\"all\" returns EVERY unarchived session on the deployment, regardless of who created it -- there is no per-session visibility restriction in this codebase today. limit bounds how many are returned (server-side default and cap apply when omitted or too large).",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions", handler: twins.ListSessions},
			InputDef:     "ListSessionsToolRequest",
			OutputDef:    "ListSessionsResponse",
			BuildRequest: buildListSessionsRequest,
		},
		{
			Name:         "narvi_get_session",
			Description:  "Get one session's full detail by id -- the same detail GET /api/sessions/{sessionID} returns. Any authenticated role may read any session's detail; there is no per-session visibility restriction in this codebase today.",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}", handler: twins.GetSession},
			InputDef:     "GetSessionToolRequest",
			OutputDef:    "Session",
			BuildRequest: buildGetSessionRequest,
		},
	}
}

// readOnlyAnnotations is shared by all three 180 tools (technical plan
// §43.8): every one is a plain read, never destructive, always
// idempotent, and never reaches outside this deployment ("open world").
var readOnlyAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(false),
}

func boolPtr(b bool) *bool { return &b }

// toolHandler returns the low-level sdkmcp.ToolHandler for spec, closing
// over ctx (the authenticated MCP HTTP request's own context -- see
// getServer's doc comment for why THIS context, not the one the SDK
// passes into the handler at call time, is what callTwin receives).
//
// Deliberately the RAW, non-generic sdkmcp.ToolHandler API (Server.
// AddTool(*Tool, ToolHandler)), never the generic package-level
// AddTool[In, Out] wrapper: that wrapper's own automatic argument
// validation runs against the SAME InputSchema this handler's own Tool
// carries, which -- per contracts/rest/v1/dtos.schema.json's own
// ListSessionsToolRequest/filter and .limit doc comments -- deliberately
// omits "enum"/"minimum" precisely so a value the REST route itself
// still needs to reject (e.g. filter:"x", limit:0) reaches this handler
// and the REAL twin, rather than being intercepted earlier by the SDK's
// own generic validation error (a DIFFERENT message than the REST
// route's own -- verified against the pinned github.com/google/
// jsonschema-go, which DOES enforce "enum"/"minimum" but does NOT
// enforce "format", the asymmetry this design relies on). Validating
// arguments a second, divergent way here would reintroduce exactly the
// two-validation-paths problem the bridge exists to avoid.
func (spec toolSpec) toolHandler(ctx context.Context) sdkmcp.ToolHandler {
	return func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		urlParams, query, err := spec.BuildRequest(req.Params.Arguments)
		if err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		status, body := callTwin(ctx, spec.Twin, urlParams, query)
		return mapOutcome(status, body)
	}
}

// buildTool resolves spec's own contracts-sourced schemas into a real
// *sdkmcp.Tool -- shared by registerTools (the real server) and
// defectServer (the "no authenticated user in context" fallback) so
// tools/list advertises the SAME three tools either way; only whether a
// CALL can ever succeed differs between the two.
func buildTool(spec toolSpec) (*sdkmcp.Tool, error) {
	in, err := inputSchema(spec.InputDef)
	if err != nil {
		return nil, err
	}
	out, err := bundleOutputSchema(spec.OutputDef)
	if err != nil {
		return nil, err
	}
	return &sdkmcp.Tool{
		Name:         spec.Name,
		Description:  spec.Description,
		InputSchema:  in,
		OutputSchema: out,
		Annotations:  readOnlyAnnotations,
	}, nil
}

// registerTools adds all three 180 tools to s, each handler closing over
// ctx and twins per toolSpec.toolHandler's own doc comment.
func registerTools(ctx context.Context, s *sdkmcp.Server, twins Twins) error {
	for _, spec := range toolSpecs(twins) {
		tool, err := buildTool(spec)
		if err != nil {
			return err
		}
		s.AddTool(tool, spec.toolHandler(ctx))
	}
	return nil
}

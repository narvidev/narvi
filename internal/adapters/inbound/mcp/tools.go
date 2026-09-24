package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/platform"
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

// invalidArgumentError marks a BuildRequest failure caused by an argument
// value that IS schema-valid (validateArguments already accepted it)
// but cannot be carried through to the twin's own Go type -- e.g. a JSON
// Schema integer spelled in a form encoding/json's own int decoder
// rejects ("1.0", "1e2"), or one outside this platform's int range
// (round 2 review of PR #324, findings N8/N11/N17: JSON Schema's own
// "integer" type accepts any number with a zero fractional part, with no
// int64 bound, so a value like these passes validateArguments and used
// to fall into toolHandler's own "should be unreachable in practice"
// defect branch instead -- logged at ERROR, on every occurrence, for
// what is really an ordinary caller mistake this codebase's own contract
// deliberately leaves room for: ListSessionsToolRequest.limit carries no
// "maximum", see its own doc comment in dtos.schema.json). Error() is
// safe to show a client verbatim: it never repeats an internal Go
// type/field name, unlike the json.Unmarshal error a true BuildRequest
// defect carries (toolHandler's own doc.go "never leak" discipline).
// toolHandler tells this apart from every OTHER BuildRequest error (a
// genuine, should-never-happen defect, still logged at ERROR) via
// errors.As.
type invalidArgumentError struct{ msg string }

func (e *invalidArgumentError) Error() string { return e.msg }

// intFromJSONNumber converts num -- a JSON number validateArguments
// already confirmed satisfies a "type":"integer" schema constraint,
// i.e. ANY textual form with a zero fractional part per JSON Schema
// 2020-12, not merely a bare int64 literal -- into a Go int, or reports
// ok=false when num does not fit (round 2 review of PR #324, findings
// N8/N11/N17). The fast path (json.Number.Int64) covers the ordinary
// case; the slow path mirrors santhosh-tekuri/jsonschema/v6's OWN
// isInteger check (util.go, new(big.Rat).SetString(fmt.Sprint(num))) so
// this function accepts exactly the same value space the schema already
// validated as an integer -- "1.0" and "1e2" included -- rather than the
// narrower "must already look like an int64 literal" space encoding/json
// itself enforces.
func intFromJSONNumber(num json.Number) (int, bool) {
	if i, err := num.Int64(); err == nil {
		return int64ToInt(i)
	}
	r, ok := new(big.Rat).SetString(num.String())
	if !ok || !r.IsInt() {
		return 0, false
	}
	i := r.Num() // r.Denom() == 1 is exactly what r.IsInt() just confirmed
	if !i.IsInt64() {
		return 0, false
	}
	return int64ToInt(i.Int64())
}

// int64ToInt range-checks i against this platform's own int (identical
// to int64 on every platform this codebase ships to, but written this
// way -- not "always safe on 64-bit" -- so the check stays correct if
// that ever changes).
func int64ToInt(i int64) (int, bool) {
	if i < math.MinInt || i > math.MaxInt {
		return 0, false
	}
	return int(i), true
}

// listSessionsArgs is buildListSessionsRequest's own decode target --
// deliberately NOT restdtos.ListSessionsToolRequest: that generated
// type's Limit field is a plain *int, whose json.Unmarshal (through its
// own Plain-alias UnmarshalJSON) rejects any JSON Schema-valid integer
// spelling encoding/json does not accept literally as an int ("1.0",
// "1e2", anything outside the int64 range) -- exactly the values
// intFromJSONNumber above exists to accept instead (findings N8/N11/N17).
// Filter keeps using the generated enum type: it is a plain string on
// the wire, so encoding/json's default decode never has this problem,
// and reusing ListSessionsToolRequestFilter's own UnmarshalJSON keeps its
// enum check exactly as it already is.
type listSessionsArgs struct {
	Filter *restdtos.ListSessionsToolRequestFilter `json:"filter"`
	Limit  *json.Number                            `json:"limit"`
}

// buildListSessionsRequest unmarshals arguments and builds
// ?filter=&limit= exactly as a client calling GET /api/sessions directly
// would -- omitting either query key entirely when the caller left the
// corresponding field unset, so the twin's OWN defaulting (filter
// defaults "mine"; limit defaults listSessionsDefaultLimit) runs
// completely unchanged.
func buildListSessionsRequest(arguments json.RawMessage) (map[string]string, url.Values, error) {
	var in listSessionsArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return nil, nil, err
		}
	}
	query := url.Values{}
	if in.Filter != nil {
		query.Set("filter", string(*in.Filter))
	}
	if in.Limit != nil {
		limit, ok := intFromJSONNumber(*in.Limit)
		if !ok {
			return nil, nil, &invalidArgumentError{msg: fmt.Sprintf("limit %s is not representable as a bounded whole number", in.Limit.String())}
		}
		query.Set("limit", strconv.Itoa(limit))
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

// toolInputDefs returns every 180 tool's own InputDef name, in table
// order -- the exact, complete set NewHandler must compile EAGERLY at
// boot (schemas.go's own compileInputSchemas doc comment) before this
// build can serve a single /mcp request.
func toolInputDefs() []string {
	specs := toolSpecs(Twins{})
	defs := make([]string, len(specs))
	for i, spec := range specs {
		defs[i] = spec.InputDef
	}
	return defs
}

// toolHandler returns the low-level sdkmcp.ToolHandler for spec, closing
// over ctx (the authenticated MCP HTTP request's own context -- see
// getServer's doc comment for why THIS context, not the one the SDK
// passes into the handler at call time, is what callTwin receives) and
// inputSchemas (NewHandler's own eagerly-compiled map, schemas.go's
// compileInputSchemas -- built once, at boot, never written to again, so
// reading it concurrently from every tool call needs no lock).
//
// Deliberately the RAW, non-generic sdkmcp.ToolHandler API (Server.
// AddTool(*Tool, ToolHandler)), never the generic package-level
// AddTool[In, Out] wrapper -- NOT because that wrapper's own validation
// would run against a looser schema than intended (a prior version of
// this comment claimed exactly that, and it was wrong: the pinned SDK's
// raw AddTool path validates arguments against InputSchema not at all,
// on ANY path, not merely a "different, less specific" way -- see
// validateArguments' own doc comment in schemas.go, which is what this
// function now calls to close that gap itself, in one place, before any
// tool's own BuildRequest or twin ever sees an argument). The raw API is
// still the right one for an unrelated reason: it is what lets this
// handler wrap the ENTIRE call in the recover below, and what lets
// mapOutcome (outcome.go) render a twin's business refusal as
// IsError:true instead of the wrapper's own automatic, coarser
// success/failure split.
func (spec toolSpec) toolHandler(ctx context.Context, inputSchemas map[string]*jsonschema.Schema) sdkmcp.ToolHandler {
	return func(_ context.Context, req *sdkmcp.CallToolRequest) (result *sdkmcp.CallToolResult, err error) {
		// Defense in depth against ANY future twin or argument shape
		// that panics instead of erroring (bridge.go's own doc comment
		// covers the concrete defect this closes: httptest.NewRequest
		// panicking on an unparseable target built from a raw tool
		// argument) -- the MCP SDK runs every tool handler in its own
		// goroutine with no recover anywhere in ITS call stack
		// (internal/jsonrpc2), so a panic here would otherwise kill the
		// whole process, not just this request.
		defer func() {
			if r := recover(); r != nil {
				platform.Logger(ctx).Error("mcp: tool handler panicked",
					"tool", spec.Name,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				result = nil
				err = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
			}
		}()

		// Validate the raw arguments against the SAME contracts $def
		// this tool's own InputSchema advertises, BEFORE BuildRequest
		// or the twin ever sees them (schemas.go's own
		// validateArguments doc comment: the pinned SDK's raw AddTool
		// path never does this itself). Per the MCP tools spec, an
		// input-validation failure is a TOOL EXECUTION error
		// (IsError:true), never a JSON-RPC protocol error -- the model
		// can read this text and retry with corrected arguments.
		//
		// schema is looked up, never compiled, here: inputSchemas was
		// built once, eagerly, by NewHandler at boot (compileInputSchemas'
		// own doc comment) -- every name toolInputDefs() names is
		// guaranteed present, so a miss here is this package's own
		// defect (a spec.InputDef with no matching table entry), not a
		// legitimate per-request outcome.
		schema, ok := inputSchemas[spec.InputDef]
		if !ok {
			platform.Logger(ctx).Error("mcp: no compiled input schema for tool (toolInputDefs/toolSpecs drifted apart?)",
				"tool", spec.Name, "inputDef", spec.InputDef)
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
		}
		if verr := validateArguments(schema, req.Params.Arguments); verr != nil {
			invalid := &sdkmcp.CallToolResult{}
			invalid.SetError(errors.New(invalidArgumentsMessage(verr)))
			return invalid, nil
		}

		urlParams, query, buildErr := spec.BuildRequest(req.Params.Arguments)
		if buildErr != nil {
			var iae *invalidArgumentError
			if errors.As(buildErr, &iae) {
				// A schema-valid argument BuildRequest still cannot
				// carry through to the twin's own Go type (findings
				// N8/N11/N17) -- an ordinary tool execution error, safe
				// to show verbatim (invalidArgumentError's own doc
				// comment), never logged: this is an expected, named
				// outcome, not a defect.
				invalid := &sdkmcp.CallToolResult{}
				invalid.SetError(errors.New("invalid arguments: " + iae.msg))
				return invalid, nil
			}
			// Validation above already passed, so this should be
			// unreachable in practice; defended anyway, and never the
			// raw error text -- a json.Unmarshal error names internal
			// Go struct/field types the client has no business seeing
			// (doc.go's own "never leak" discipline).
			platform.Logger(ctx).Error("mcp: BuildRequest failed after schema validation passed",
				"tool", spec.Name, "error", buildErr)
			invalid := &sdkmcp.CallToolResult{}
			invalid.SetError(errors.New("invalid arguments"))
			return invalid, nil
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
// ctx, twins, and inputSchemas per toolSpec.toolHandler's own doc
// comment.
func registerTools(ctx context.Context, s *sdkmcp.Server, twins Twins, inputSchemas map[string]*jsonschema.Schema) error {
	for _, spec := range toolSpecs(twins) {
		tool, err := buildTool(spec)
		if err != nil {
			return err
		}
		s.AddTool(tool, spec.toolHandler(ctx, inputSchemas))
	}
	return nil
}

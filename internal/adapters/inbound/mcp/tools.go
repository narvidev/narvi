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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/domain/mcpscope"
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
// enforces that structurally, and Twins' own shape (http.HandlerFunc
// fields, nothing else) is why the ban is possible: a caller cannot even
// ATTEMPT to hand this package a *postgres.SessionStore instead of a
// handler, because there is no field to put it in.
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
	// GetSessionStatus is httpapi.GetSessionStatus(sessionStore, waiter,
	// timeouts) -- the twin of both narvi_get_session_status and, with
	// ?waitSeconds=, narvi_wait_for_session (technical plan §43.20).
	GetSessionStatus http.HandlerFunc
	// ListEvents is httpapi.ListEvents(sessionStore, eventStore) --
	// narvi_get_session_transcript's own twin, the paginated event
	// history (technical plan §43.20).
	ListEvents http.HandlerFunc
}

// toolSpec is the ONLY place a tool is declared: its wire name, the
// scope a grant must satisfy for the tool to exist at all for a request
// (technical plan §43.17), the one-line fragment instructionsFor names it
// with, its REST twin, the two contracts $def names its schemas come from
// (verbatim for input, bundled for output -- schemas.go), and a
// BuildRequest closure translating the raw `arguments` object a client
// sent into the twin's own URL params / query string.
type toolSpec struct {
	Name         string
	Description  string
	Scope        mcpscope.Scope
	Instruction  string
	Twin         twin
	InputDef     string
	OutputDef    string
	BuildRequest func(arguments json.RawMessage) (urlParams map[string]string, query url.Values, err error)
}

// countWords spells small tool counts the way the instructions paragraph
// reads them.
var countWords = []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}

// instructionsFor is server/discover's and the legacy initialize
// handshake's own "instructions" field (technical plan §43.5), composed
// per request from ONLY the tools that request can see (§43.17): a fixed
// paragraph naming every tool would describe the deployment to a client
// that may not use any of it. With no visible tool it names none.
func instructionsFor(visible []toolSpec) string {
	if len(visible) == 0 {
		return "This authorization gives access to no tools on this server. The user can connect this client again and approve more access."
	}
	fragments := make([]string, len(visible))
	for i, spec := range visible {
		fragments[i] = spec.Instruction
	}
	var list string
	switch len(fragments) {
	case 1:
		list = fragments[0]
	case 2:
		list = fragments[0] + " and " + fragments[1]
	default:
		list = strings.Join(fragments[:len(fragments)-1], ", ") + ", and " + fragments[len(fragments)-1]
	}
	count := strconv.Itoa(len(visible))
	if len(visible) < len(countWords) {
		count = countWords[len(visible)]
	}
	noun := "tools"
	if len(visible) == 1 {
		noun = "tool"
	}
	return "This server exposes " + count + " READ-ONLY " + noun + " over this deployment's session data: " + list + ". None of these tools writes anything."
}

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

// buildGetSessionStatusRequest is buildGetSessionRequest for
// narvi_get_session_status: restdtos.GetSessionStatusToolRequest (whose
// generated UnmarshalJSON enforces "sessionId" is present) mapped onto the
// twin's own chi URL param, "sessionID". No query: the status twin reads
// none.
func buildGetSessionStatusRequest(arguments json.RawMessage) (map[string]string, url.Values, error) {
	var in restdtos.GetSessionStatusToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return nil, nil, err
	}
	return map[string]string{"sessionID": in.SessionId}, nil, nil
}

// waitArgs is buildWaitForSessionRequest's own decode target -- not
// restdtos.WaitForSessionToolRequest, for listSessionsArgs' reason:
// waitSeconds is a *json.Number so every integer spelling the schema
// accepts reaches intFromJSONNumber.
type waitArgs struct {
	SessionID   string       `json:"sessionId"`
	WaitSeconds *json.Number `json:"waitSeconds"`
}

// longestWait is the waitSeconds narvi_wait_for_session sends its twin
// when the caller asked for no particular bound, or for one past what an
// int64 holds: the largest value the route parses, which it clamps to the
// deployment's MCPWaitMaxDuration like any other -- this package never
// learns that value, and never needs to.
var longestWait = strconv.FormatInt(math.MaxInt64, 10)

// buildWaitForSessionRequest maps narvi_wait_for_session's arguments onto
// GET /api/sessions/{sessionID}/status?waitSeconds=N: sessionId becomes the
// chi URL param, and waitSeconds is ALWAYS set, so the twin can tell a wait
// from a plain read -- the caller's value (validateArguments has already
// held it to a whole number of at least one), or longestWait when omitted
// or past int64. Never an argument error: a large wait is clamped, not
// refused.
func buildWaitForSessionRequest(arguments json.RawMessage) (map[string]string, url.Values, error) {
	var in waitArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return nil, nil, err
		}
	}
	waitSeconds := longestWait
	if in.WaitSeconds != nil {
		if n, ok := intFromJSONNumber(*in.WaitSeconds); ok {
			waitSeconds = strconv.Itoa(n)
		}
	}
	return map[string]string{"sessionID": in.SessionID}, url.Values{"waitSeconds": {waitSeconds}}, nil
}

// transcriptArgs is buildGetSessionTranscriptRequest's own decode target
// -- not restdtos.GetSessionTranscriptToolRequest, for listSessionsArgs'
// reason: limit is a *json.Number so every integer spelling the schema
// accepts reaches intFromJSONNumber, never encoding/json's narrower int
// decoder.
type transcriptArgs struct {
	SessionID string       `json:"sessionId"`
	Cursor    *string      `json:"cursor"`
	Limit     *json.Number `json:"limit"`
}

// buildGetSessionTranscriptRequest maps narvi_get_session_transcript's
// arguments onto GET /api/sessions/{sessionID}/events?cursor=&limit=
// exactly as a client calling the route directly would: sessionId becomes
// the chi URL param, and cursor/limit become query keys only when the
// caller set them, so the twin's own defaults (from the beginning, 100 per
// page, clamped at 500) run unchanged otherwise. cursor is passed through
// verbatim -- the twin itself parses it and answers 400 for a value that
// names no event id.
func buildGetSessionTranscriptRequest(arguments json.RawMessage) (map[string]string, url.Values, error) {
	var in transcriptArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return nil, nil, err
		}
	}
	query := url.Values{}
	if in.Cursor != nil {
		query.Set("cursor", *in.Cursor)
	}
	if in.Limit != nil {
		limit, ok := intFromJSONNumber(*in.Limit)
		if !ok {
			return nil, nil, &invalidArgumentError{msg: fmt.Sprintf("limit %s is not representable as a bounded whole number", in.Limit.String())}
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	return map[string]string{"sessionID": in.SessionID}, query, nil
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
			Scope:        mcpscope.Read,
			Instruction:  "narvi_list_models (the model catalog)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/models", handler: twins.ListModels},
			InputDef:     "ListModelsToolRequest",
			OutputDef:    "ModelCatalog",
			BuildRequest: noArguments,
		},
		{
			Name:         "narvi_list_sessions",
			Description:  "List this deployment's sessions -- the same list GET /api/sessions returns. filter:\"mine\" (the default) returns sessions the caller created or joined; filter:\"all\" returns EVERY unarchived session on the deployment, regardless of who created it -- there is no per-session visibility restriction in this codebase today. limit bounds how many are returned (server-side default and cap apply when omitted or too large).",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_list_sessions (this deployment's sessions; filter:\"all\" lists EVERY session on the deployment, not only the caller's own)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions", handler: twins.ListSessions},
			InputDef:     "ListSessionsToolRequest",
			OutputDef:    "ListSessionsResponse",
			BuildRequest: buildListSessionsRequest,
		},
		{
			Name:         "narvi_get_session",
			Description:  "Get one session's full detail by id -- the same detail GET /api/sessions/{sessionID} returns. Any authenticated role may read any session's detail; there is no per-session visibility restriction in this codebase today. Its status is re-derived only when a turn finishes, so it does not show queued or running work -- use narvi_get_session_status for what the session is doing now.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_get_session (one session's full detail)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}", handler: twins.GetSession},
			InputDef:     "GetSessionToolRequest",
			OutputDef:    "Session",
			BuildRequest: buildGetSessionRequest,
		},
		{
			Name:         "narvi_get_session_status",
			Description:  "Compact state of one session -- the same state GET /api/sessions/{sessionID}/status returns: whether its work is queued, running, delivering (a completed turn's branch being pushed and its pull request opened), scheduled (the server holds work that may start a turn on its own and has neither started nor declined it yet: an automatic re-review after a push to a pull request whose repository has opted in and whose re-review budget is not spent, or a release pull request's manifest check), awaiting approval by a person, idle or finished (a queued or running turn is never reported as idle or finished; settled is true only for idle, awaiting approval and finished), with suggestedDelaySeconds, how long to wait before reading it again. Does not include the transcript. To wait for the session to settle, prefer narvi_wait_for_session to repeated reads.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_get_session_status (what one session is doing now, and when to ask again)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/status", handler: twins.GetSessionStatus},
			InputDef:     "GetSessionStatusToolRequest",
			OutputDef:    "SessionActivity",
			BuildRequest: buildGetSessionStatusRequest,
		},
		{
			Name:         "narvi_wait_for_session",
			Description:  "Wait, server-side and bounded, until one session is settled -- finished, idle, or awaiting a person -- then return its state: the same state narvi_get_session_status returns (GET /api/sessions/{sessionID}/status?waitSeconds=N), with wait.reason and wait.waitedMs. The state is read at once and then about every second; a session that is queued, running, delivering a completed turn's pull request, or holding scheduled work never ends the wait early. waitSeconds bounds it (omitted, or larger than the deployment allows, means the longest: 25 seconds as shipped). wait.reason: settled; timeout (still not settled -- call this tool again rather than sleeping); interrupted (the server is restarting -- call again); capacity (this authorization already has the most waits it may run at once, 2 as shipped: the current state is returned without waiting). Does not include the transcript.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_wait_for_session (wait, at most a few seconds, until one session is finished, idle or awaiting a person)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/status", handler: twins.GetSessionStatus},
			InputDef:     "WaitForSessionToolRequest",
			OutputDef:    "SessionActivity",
			BuildRequest: buildWaitForSessionRequest,
		},
		{
			Name:         "narvi_get_session_transcript",
			Description:  "Paginated event history of one session -- the same page GET /api/sessions/{sessionID}/events returns, oldest first. Pass the previous page's nextCursor as cursor to read on; nextCursor is null on the last page. limit defaults to 100 and is capped at 500. Request it only when the detail is needed: narvi_get_session_status answers what the session is doing.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_get_session_transcript (one session's event history, a page at a time)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/events", handler: twins.ListEvents},
			InputDef:     "GetSessionTranscriptToolRequest",
			OutputDef:    "EventsResponse",
			BuildRequest: buildGetSessionTranscriptRequest,
		},
	}
}

// readOnlyAnnotations is shared by every tool in the table (technical plan
// §43.8): every one is a plain read, never destructive, always
// idempotent, and never reaches outside this deployment ("open world").
var readOnlyAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(false),
}

func boolPtr(b bool) *bool { return &b }

// toolInputDefs returns every tool's own InputDef name, in table
// order -- the exact, complete set NewHandler must compile EAGERLY at
// boot (schemas.go's own compileInputSchemas doc comment) before this
// build can serve a single /mcp request, and the set newHandler
// (handler.go) refuses to build a handler without.
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
// inputSchemas (newHandler's private copy of the map NewHandler compiled
// eagerly at boot with schemas.go's compileInputSchemas -- complete,
// checked and copied once at construction, never written to again, so
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
		// own doc comment), and newHandler refuses to build a handler
		// whose map lacks a non-nil entry for any name toolInputDefs()
		// returns -- so no handler it built ever reaches this branch. It
		// stays as a defensive refusal for any other caller: a miss is
		// this package's own defect, answered -32603 without invoking
		// the twin and without compiling anything
		// (TestToolHandler_MissingSchemaIsRefusedWithoutCompiling).
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
// *sdkmcp.Tool for registerTools.
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

// visibleSpecs returns the specs whose scope granted satisfies, in table
// order -- the ONLY tools a request may see or call (technical plan
// §43.17). mcpscope.Satisfies fails closed: a spec whose Scope was never
// set is visible to no grant at all.
func visibleSpecs(specs []toolSpec, granted []mcpscope.Scope) []toolSpec {
	out := make([]toolSpec, 0, len(specs))
	for _, spec := range specs {
		if mcpscope.Satisfies(granted, spec.Scope) {
			out = append(out, spec)
		}
	}
	return out
}

// registerTools adds exactly the tools in visible to s, each handler
// closing over ctx and inputSchemas per toolSpec.toolHandler's own doc
// comment. A tool that is not added does not exist for this request: its
// tools/call answers the SDK's own "unknown tool" error, byte-identical
// to a name that never existed.
func registerTools(ctx context.Context, s *sdkmcp.Server, visible []toolSpec, inputSchemas map[string]*jsonschema.Schema) error {
	for _, spec := range visible {
		tool, err := buildTool(spec)
		if err != nil {
			return err
		}
		s.AddTool(tool, spec.toolHandler(ctx, inputSchemas))
	}
	return nil
}

// AdvertisedScopes returns every scope this build offers: exactly the
// scopes at least one tool in the table requires (mcpscope.Advertised).
// controlplane hands this one list to both the authorization server
// (what a client may request) and the bearer gate (what its 401 challenge
// names), so the two can never disagree with the tool table.
func AdvertisedScopes() []mcpscope.Scope {
	specs := toolSpecs(Twins{})
	required := make([]mcpscope.Scope, len(specs))
	for i, spec := range specs {
		required[i] = spec.Scope
	}
	return mcpscope.Advertised(required)
}

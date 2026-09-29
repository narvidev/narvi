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
	// GetSessionResult is httpapi.GetSessionResult(deps) --
	// narvi_get_session_result's own twin: the last run, the pull requests
	// and each verdict's freshness or absence (technical plan §43.20, row
	// 182's result).
	GetSessionResult http.HandlerFunc
	// CreateSession is httpapi.CreateSession(...) -- narvi_create_session's
	// own twin, POST /api/sessions (technical plan §43.8): the same handler,
	// so the same authorization, validation, entitlement and rollout gates,
	// audit row and dispatch as a browser's create.
	CreateSession http.HandlerFunc
	// ListPlans is httpapi.ListPlans(...) -- narvi_list_plans' own twin,
	// GET /api/sessions/{sessionID}/plans (technical plan §43.21).
	ListPlans http.HandlerFunc
	// ApprovePlan is httpapi.ApprovePlan(...) -- narvi_approve_plan's own
	// twin (technical plan §43.21): the same handler, so the same role and
	// own/joined rule, open-turn gate, guarded first-verdict-wins update,
	// plan ownership check, approved-content snapshot, audit row and
	// cross-channel notices as a browser's approval.
	ApprovePlan http.HandlerFunc
	// RejectPlan is httpapi.RejectPlan(...) -- narvi_reject_plan's own twin.
	RejectPlan http.HandlerFunc
	// CreateTurn is httpapi.CreateTurn(...) -- the one twin of both
	// narvi_request_plan_revision (planMode true) and narvi_send_prompt
	// (planMode false), POST /api/sessions/{sessionID}/turns: the same
	// handler, so the same RejectIfOpen policy -- 409 while any turn is
	// open, nothing queued -- as a browser's prompt (technical plan §43.21).
	CreateTurn http.HandlerFunc
	// StopSession is httpapi.StopSession(...) -- narvi_stop_session's own
	// twin, POST /api/sessions/{sessionID}/stop (technical plan §43.22): the
	// same handler, so the same role, own/joined and review-session rule,
	// the same request written as data, the same audit row and the same walk
	// to every session the one named started, as a browser's stop.
	StopSession http.HandlerFunc
}

// toolSpec is the ONLY place a tool is declared: its wire name, the
// scope a grant must satisfy for the tool to exist at all for a request
// (technical plan §43.17), the one-line fragment instructionsFor names it
// with, its REST twin, the two contracts $def names its schemas come from
// (verbatim for input, bundled for output -- schemas.go), its own
// annotations (§43.8), whether it consults the create brake, and a
// BuildRequest closure translating the raw `arguments` object a client
// sent into the twin's own URL params, query string and body (twinCall).
type toolSpec struct {
	Name        string
	Description string
	Scope       mcpscope.Scope
	Instruction string
	Twin        twin
	InputDef    string
	OutputDef   string
	// Annotations are this tool's own hints: readOnlyAnnotations for every
	// read, a write's own for a write (TestToolAnnotations_MatchTwinMethod
	// ties them to the twin's method).
	Annotations *sdkmcp.ToolAnnotations
	// CreateBrake is true for a tool that starts a session: its call takes
	// one from the grant's bucket in Config.CreateBrake after its arguments
	// validate and before its twin runs (technical plan §43.8).
	CreateBrake bool
	// RefusedWhileTurnOpen is true for a tool whose twin refuses with 409,
	// and queues nothing, while any turn of the session is pending,
	// dispatched or processing -- approve (the stale-plan guard) and both
	// turn tools (RejectIfOpen). It changes nothing the tool does: the twin
	// decides. instructionsFor says it once, naming these tools
	// (technical plan §43.21).
	RefusedWhileTurnOpen bool
	// StopsSessions is true for a tool whose twin cancels the queued and
	// running turns of a session and of every session it started, and does
	// so again, for whatever was started since, on a repeat --
	// narvi_stop_session. Like RefusedWhileTurnOpen it changes nothing the
	// tool does; instructionsFor says it once, naming these tools
	// (technical plan §43.22).
	StopsSessions bool
	BuildRequest  func(arguments json.RawMessage) (twinCall, error)
}

// countWords spells small tool counts the way the instructions paragraph
// reads them.
var countWords = []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty"}

// instructionsFor is server/discover's and the legacy initialize
// handshake's own "instructions" field (technical plan §43.5), composed
// per request from ONLY the tools that request can see (§43.17): a fixed
// paragraph naming every tool would describe the deployment to a client
// that may not use any of it. With no visible tool it names none. It says
// the tools are read-only only when no write tool is visible; otherwise it
// names the reads and the writes apart, and says what a write can do. When
// a visible tool is refused while a turn is open (RefusedWhileTurnOpen), a
// sentence names those tools and says that nothing is queued, so a client
// waits for the session to settle rather than retrying in a loop
// (technical plan §43.21). When a visible tool stops sessions
// (StopsSessions), a last sentence names it, says it reaches every session
// the one named started and returns before their work has ended, and that a
// second call is not a no-op (technical plan §43.22).
func instructionsFor(visible []toolSpec) string {
	if len(visible) == 0 {
		return "This authorization gives access to no tools on this server. The user can connect this client again and approve more access."
	}
	var reads, writes, gated, stoppers []string
	for _, spec := range visible {
		if spec.Annotations != nil && spec.Annotations.ReadOnlyHint {
			reads = append(reads, spec.Instruction)
		} else {
			writes = append(writes, spec.Instruction)
		}
		if spec.RefusedWhileTurnOpen {
			gated = append(gated, spec.Name)
		}
		if spec.StopsSessions {
			stoppers = append(stoppers, spec.Name)
		}
	}
	if len(writes) == 0 {
		return "This server exposes " + countWord(len(visible)) + " READ-ONLY " + plural(len(visible), "tool", "tools") + " over this deployment's session data: " + joinList(reads) + ". None of these tools writes anything."
	}
	text := "This server exposes " + countWord(len(visible)) + " " + plural(len(visible), "tool", "tools") + " over this deployment's session data."
	if len(reads) > 0 {
		text += " " + capitalize(countWord(len(reads))) + " only " + plural(len(reads), "reads and changes", "read and change") + " nothing: " + joinList(reads) + "."
	}
	text += " " + capitalize(countWord(len(writes))) + " " + plural(len(writes), "acts", "act") + " as the user who approved this client, within what that user's own role allows, and can run code in their repositories and spend on models: " + joinList(writes) + "."
	if len(gated) > 0 {
		text += " " + joinList(gated) + " " + plural(len(gated), "is", "are") + " refused while a turn of the session is queued or running, and nothing is queued: wait until the session settles, then call again."
	}
	if len(stoppers) > 0 {
		text += " " + joinList(stoppers) + " " + plural(len(stoppers), "cancels", "cancel") + " the queued and running turns of a session and of every session it started, and " + plural(len(stoppers), "answers", "answer") + " before that work has ended: wait until the session settles. Calling it again is not a no-op: it also stops whatever was started since."
	}
	return text
}

// countWord spells n the way the instructions paragraph reads it.
func countWord(n int) string {
	if n >= 0 && n < len(countWords) {
		return countWords[n]
	}
	return strconv.Itoa(n)
}

// plural picks one or many by n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// capitalize upper-cases a count word's first letter, to open a sentence.
func capitalize(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

// joinList joins fragments as prose: "a", "a and b", "a, b, and c".
func joinList(fragments []string) string {
	switch len(fragments) {
	case 0:
		return ""
	case 1:
		return fragments[0]
	case 2:
		return fragments[0] + " and " + fragments[1]
	default:
		return strings.Join(fragments[:len(fragments)-1], ", ") + ", and " + fragments[len(fragments)-1]
	}
}

// noArguments is the BuildRequest closure every argument-less tool
// shares (only narvi_list_models today): no urlParams, no query, no body,
// no possible error -- the twin is invoked exactly once, unconditionally.
func noArguments(json.RawMessage) (twinCall, error) {
	return twinCall{}, nil
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
func buildListSessionsRequest(arguments json.RawMessage) (twinCall, error) {
	var in listSessionsArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return twinCall{}, err
		}
	}
	query := url.Values{}
	if in.Filter != nil {
		query.Set("filter", string(*in.Filter))
	}
	if in.Limit != nil {
		limit, ok := intFromJSONNumber(*in.Limit)
		if !ok {
			return twinCall{}, &invalidArgumentError{msg: fmt.Sprintf("limit %s is not representable as a bounded whole number", in.Limit.String())}
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	return twinCall{Query: query}, nil
}

// buildGetSessionRequest unmarshals arguments as a restdtos.
// GetSessionToolRequest (whose own generated UnmarshalJSON already
// enforces "sessionId" is present) and maps it onto the twin's own chi
// URL param name, "sessionID" -- GET /api/sessions/{sessionID}'s own
// path parameter name, unrelated to the wire argument's own camelCase
// "sessionId" spelling.
func buildGetSessionRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.GetSessionToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionId}}, nil
}

// buildGetSessionStatusRequest is buildGetSessionRequest for
// narvi_get_session_status: restdtos.GetSessionStatusToolRequest (whose
// generated UnmarshalJSON enforces "sessionId" is present) mapped onto the
// twin's own chi URL param, "sessionID". No query: the status twin reads
// none.
func buildGetSessionStatusRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.GetSessionStatusToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionId}}, nil
}

// buildGetSessionResultRequest is buildGetSessionStatusRequest for
// narvi_get_session_result: restdtos.GetSessionResultToolRequest (whose
// generated UnmarshalJSON enforces "sessionId" is present) mapped onto the
// twin's own chi URL param, "sessionID". No query: the result twin reads
// none.
func buildGetSessionResultRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.GetSessionResultToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionId}}, nil
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
func buildWaitForSessionRequest(arguments json.RawMessage) (twinCall, error) {
	var in waitArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return twinCall{}, err
		}
	}
	waitSeconds := longestWait
	if in.WaitSeconds != nil {
		if n, ok := intFromJSONNumber(*in.WaitSeconds); ok {
			waitSeconds = strconv.Itoa(n)
		}
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionID}, Query: url.Values{"waitSeconds": {waitSeconds}}}, nil
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
func buildGetSessionTranscriptRequest(arguments json.RawMessage) (twinCall, error) {
	var in transcriptArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &in); err != nil {
			return twinCall{}, err
		}
	}
	query := url.Values{}
	if in.Cursor != nil {
		query.Set("cursor", *in.Cursor)
	}
	if in.Limit != nil {
		limit, ok := intFromJSONNumber(*in.Limit)
		if !ok {
			return twinCall{}, &invalidArgumentError{msg: fmt.Sprintf("limit %s is not representable as a bounded whole number", in.Limit.String())}
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionID}, Query: query}, nil
}

// buildCreateSessionRequest maps narvi_create_session's arguments onto the
// body of POST /api/sessions (technical plan §43.8): a
// restdtos.CreateSessionRequest built field by field from the validated
// restdtos.CreateSessionToolRequest, which callTwin marshals -- so only
// that DTO's own fields can reach the twin, never an argument the client
// added. SpawnSource is web: it is the one value that route accepts, not a
// statement of where the call came from -- the route itself records mcp,
// from the grant on the context. Every field the tool does not offer is
// left at the DTO's own zero value, which the route reads as "not set", as
// a browser that never sends it; the required-nullable ones (title,
// prompt, modelId, effort) are null when omitted, and each repo's branch
// is null (the repository's default branch) when omitted.
func buildCreateSessionRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.CreateSessionToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	repos := make([]restdtos.CreateSessionRequestReposElem, len(in.Repos))
	for i, repo := range in.Repos {
		repos[i] = restdtos.CreateSessionRequestReposElem{
			Name:   repo.Name,
			Url:    repo.Url,
			Branch: restdtos.CreateSessionRequestReposElemBranch(repo.Branch),
		}
	}
	prompt := in.Prompt
	key := in.IdempotencyKey
	return twinCall{Body: restdtos.CreateSessionRequest{
		SpawnSource:    restdtos.CreateSessionRequestSpawnSourceWeb,
		Title:          restdtos.CreateSessionRequestTitle(in.Title),
		Prompt:         restdtos.CreateSessionRequestPrompt(&prompt),
		Repos:          repos,
		ModelId:        restdtos.CreateSessionRequestModelId(in.ModelId),
		Effort:         restdtos.CreateSessionRequestEffort(in.Effort),
		PlanMode:       in.PlanMode,
		BuildModelId:   restdtos.CreateSessionRequestBuildModelId(in.BuildModelId),
		BuildEffort:    restdtos.CreateSessionRequestBuildEffort(in.BuildEffort),
		IdempotencyKey: &key,
	}}, nil
}

// buildListPlansRequest is buildGetSessionRequest for narvi_list_plans:
// restdtos.ListPlansToolRequest (whose generated UnmarshalJSON enforces
// "sessionId" is present) mapped onto the twin's own chi URL param,
// "sessionID". No query and no body: GET /api/sessions/{sessionID}/plans
// reads neither.
func buildListPlansRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.ListPlansToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionId}}, nil
}

// planDecisionParams maps a plan decision's two ids onto the twin's own
// chi URL params: "sessionID" and "planId", the names POST
// /api/sessions/{sessionID}/plans/{planId}/approve|reject reads
// (httpapi.parseSessionID, parsePlanID). Neither route reads a query or a
// body, so the call carries none.
func planDecisionParams(sessionID, planID string) twinCall {
	return twinCall{URLParams: map[string]string{"sessionID": sessionID, "planId": planID}}
}

// buildApprovePlanRequest maps narvi_approve_plan's arguments, decoded
// through restdtos.ApprovePlanToolRequest (whose generated UnmarshalJSON
// enforces both keys are present), onto the approve route's URL params.
// Nothing else: every check the approval makes is the twin's (technical
// plan §43.21).
func buildApprovePlanRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.ApprovePlanToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return planDecisionParams(in.SessionId, in.PlanId), nil
}

// buildRejectPlanRequest is buildApprovePlanRequest for narvi_reject_plan,
// through restdtos.RejectPlanToolRequest.
func buildRejectPlanRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.RejectPlanToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return planDecisionParams(in.SessionId, in.PlanId), nil
}

// createTurnCall is the call both turn tools make to POST
// /api/sessions/{sessionID}/turns: sessionID as the chi URL param, and a
// restdtos.CreateTurnRequest body that callTwin marshals -- so only that
// DTO's own fields reach the twin. planMode is the tool's own constant,
// never an argument: true for narvi_request_plan_revision, false for
// narvi_send_prompt. modelID and effort are null when omitted, which the
// route reads as the default, as for a browser that sends null; no
// attachment is ever offered. The route's policy is RejectIfOpen whatever
// the body says: the body cannot ask for a queue (technical plan §43.21).
func createTurnCall(sessionID, prompt string, modelID, effort *string, planMode bool) twinCall {
	return twinCall{
		URLParams: map[string]string{"sessionID": sessionID},
		Body: restdtos.CreateTurnRequest{
			Prompt:   prompt,
			ModelId:  restdtos.CreateTurnRequestModelId(modelID),
			Effort:   restdtos.CreateTurnRequestEffort(effort),
			PlanMode: planMode,
		},
	}
}

// buildRequestPlanRevisionRequest maps narvi_request_plan_revision's
// arguments, decoded through restdtos.RequestPlanRevisionToolRequest (whose
// generated UnmarshalJSON enforces both required keys and a non-empty
// feedback), onto a plan-mode turn whose prompt is the feedback -- the
// web's own "request changes" body.
func buildRequestPlanRevisionRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.RequestPlanRevisionToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return createTurnCall(in.SessionId, in.Feedback, in.ModelId, in.Effort, true), nil
}

// buildSendPromptRequest maps narvi_send_prompt's arguments, decoded
// through restdtos.SendPromptToolRequest (whose generated UnmarshalJSON
// enforces both required keys and a non-empty prompt), onto an ordinary
// turn: planMode false.
func buildSendPromptRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.SendPromptToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return createTurnCall(in.SessionId, in.Prompt, in.ModelId, in.Effort, false), nil
}

// buildStopSessionRequest maps narvi_stop_session's arguments, decoded
// through restdtos.StopSessionToolRequest (whose generated UnmarshalJSON
// enforces "sessionId" is present), onto the stop route's one chi URL
// param, "sessionID". No query and no body: POST
// /api/sessions/{sessionID}/stop reads neither. Nothing else: who may stop
// the session, the request written as data and the walk to every session
// it started are all the twin's (technical plan §43.22).
func buildStopSessionRequest(arguments json.RawMessage) (twinCall, error) {
	var in restdtos.StopSessionToolRequest
	if err := json.Unmarshal(arguments, &in); err != nil {
		return twinCall{}, err
	}
	return twinCall{URLParams: map[string]string{"sessionID": in.SessionId}}, nil
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
			Annotations:  readOnlyAnnotations,
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
			Annotations:  readOnlyAnnotations,
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
			Annotations:  readOnlyAnnotations,
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
			Annotations:  readOnlyAnnotations,
			BuildRequest: buildGetSessionStatusRequest,
		},
		{
			Name:         "narvi_wait_for_session",
			Description:  "Wait, server-side and bounded, until one session is settled -- finished, idle, or awaiting a person -- then return its state: the same state narvi_get_session_status returns (GET /api/sessions/{sessionID}/status?waitSeconds=N), with wait.reason and wait.waitedMs. The state is read at once, then again one second after each read (as shipped), and the call returns as soon as a read finds the session settled; a session that is queued, running, delivering a completed turn's pull request, or holding scheduled work never ends the wait early. Otherwise the call blocks for waitSeconds seconds -- omitted, or larger than the deployment allows, means the longest: 25 seconds as shipped -- then reads the state once more and returns that read. wait.reason: settled; timeout (still not settled at the end of the wait -- call this tool again rather than sleeping); interrupted (the server is restarting and returned its latest read at once -- call again); capacity (the state was read once and returned at once, without waiting, because the server replica that took the call already runs as many concurrent waits as one of its three caps allows: 2 under this authorization, 4 of this user's across all of their authorizations and their browser, or 32 from all callers together -- the caps as shipped; a later call waits once a slot is free). Does not include the transcript.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_wait_for_session (wait, server-side, up to 25 seconds as shipped, until one session is finished, idle or awaiting a person)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/status", handler: twins.GetSessionStatus},
			InputDef:     "WaitForSessionToolRequest",
			OutputDef:    "SessionActivity",
			Annotations:  readOnlyAnnotations,
			BuildRequest: buildWaitForSessionRequest,
		},
		{
			Name:         "narvi_get_session_result",
			Description:  "What one session has produced -- the same result GET /api/sessions/{sessionID}/result returns: how its last run ended, with a summary of that run's final text (copied as the agent wrote it, never written by a model, at most 4,000 characters; truncated says when it was cut); the pull requests it opened, or the one it reviews (reviewScope none means there is no pull request to review, which is not a clean review), with any record that names no pull request it opened listed apart in excludedPullRequests, with why (shadow_suppressed: the repository was in shadow mode, so no pull request was created; unreadable: the record could not be read); and each pull request's review state -- absent, in_progress, not_assessed (the older verdict is in supersededVerdict and is not the answer) or assessed -- with the verdict's freshness: current only when the code host confirmed, during this call, that the pull request still matches what was reviewed (its head, base, ancestor chain and the review policy); stale or unconfirmed with a reason; not_applicable when there is no assessed verdict or the pull request is merged or no longer open. activity says whether the session can still change the result, and suggestedDelaySeconds how long to wait before reading it again -- short while it can, or while the review of a pull request it opened is still to come: every read asks the code host, so to learn when the session settles, prefer narvi_wait_for_session. Does not include the transcript.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_get_session_result (what one session produced: its last run, its pull requests, and each one's review verdict and whether it is still current)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/result", handler: twins.GetSessionResult},
			InputDef:     "GetSessionResultToolRequest",
			OutputDef:    "SessionOutcome",
			Annotations:  readOnlyAnnotations,
			BuildRequest: buildGetSessionResultRequest,
		},
		{
			Name:         "narvi_get_session_transcript",
			Description:  "Paginated event history of one session -- the same page GET /api/sessions/{sessionID}/events returns, oldest first. Pass the previous page's nextCursor as cursor to read on; nextCursor is null on the last page. limit defaults to 100 and is capped at 500. Request it only when the detail is needed: narvi_get_session_status answers what the session is doing.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_get_session_transcript (one session's event history, a page at a time)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/events", handler: twins.ListEvents},
			InputDef:     "GetSessionTranscriptToolRequest",
			OutputDef:    "EventsResponse",
			Annotations:  readOnlyAnnotations,
			BuildRequest: buildGetSessionTranscriptRequest,
		},
		{
			Name:         "narvi_list_plans",
			Description:  "List one session's plan versions -- the same list GET /api/sessions/{sessionID}/plans returns, by version: each plan's id, version, status (awaiting_approval, approved, rejected, or superseded by a newer version), who decided it and when, its text and, when the model wrote them, its numbered steps. The plan awaiting approval, if any, is the one narvi_approve_plan and narvi_reject_plan take. Any authenticated role may read any session's plans; there is no per-session visibility restriction in this codebase today.",
			Scope:        mcpscope.Read,
			Instruction:  "narvi_list_plans (one session's plan versions, each with its status and the id to approve or reject it by)",
			Twin:         twin{method: http.MethodGet, pathTemplate: "/api/sessions/{sessionID}/plans", handler: twins.ListPlans},
			InputDef:     "ListPlansToolRequest",
			OutputDef:    "ListPlansResponse",
			Annotations:  readOnlyAnnotations,
			BuildRequest: buildListPlansRequest,
		},
		{
			Name:         "narvi_create_session",
			Description:  "Start a new session as the user who approved this client -- the same as POST /api/sessions, with the same checks: that user's own role (a viewer may not start one), and every repository must be one this deployment knows. The session clones the repositories and runs a first turn with prompt at once: this runs code in those repositories and spends on models. With planMode true, the first turn writes a plan and nothing is implemented until a person approves it. idempotencyKey is required: a new UUID for each session you mean to start, and the same one only to retry this call -- a retry with the same key and the same arguments returns the session the first call started and starts nothing; the same key with different arguments is refused, and so is a key the user already used to start a session another way. Returns the Session, whose spawnSource is mcp; follow it with narvi_wait_for_session or narvi_get_session_status. One authorization may call this tool 5 times at once, then once a minute (as shipped), and a retry counts as a call: past that the call is refused, starts nothing and says how many seconds to wait. After that wait, a retry with the same key and the same arguments returns the session an earlier call with that key started, if one did.",
			Scope:        mcpscope.Write,
			Instruction:  "narvi_create_session (start a session as that user on repositories this deployment knows, with a first prompt; a new idempotencyKey for each session, the same one only to retry)",
			Twin:         twin{method: http.MethodPost, pathTemplate: "/api/sessions", handler: twins.CreateSession},
			InputDef:     "CreateSessionToolRequest",
			OutputDef:    "Session",
			Annotations:  createSessionAnnotations,
			CreateBrake:  true,
			BuildRequest: buildCreateSessionRequest,
		},
		{
			Name:                 "narvi_approve_plan",
			Description:          "Approve a plan awaiting approval, as the user who approved this client -- the same as POST /api/sessions/{sessionID}/plans/{planId}/approve, with every check that route makes: that user's own role (an admin or maintainer may approve any session's plan, a member only a plan of a session they started or joined, a viewer none); no turn of the session queued or running (a request for changes being written would otherwise be overtaken by the older plan, so the call is refused and nothing changes); and the plan still awaiting approval and belonging to that session (the first decision wins, whichever channel made it: a later one is refused). Approving queues the implementation turn at once, in the session's own conversation: it runs code in the session's repositories and spends on models, and what it changes is delivered like any turn's. A plan revision requested afterwards never withdraws that approval. Returns the plan's id, its status (approved) and the implementation turn's id; follow it with narvi_wait_for_session.",
			Scope:                mcpscope.Write,
			Instruction:          "narvi_approve_plan (approve a plan awaiting approval, which queues its implementation)",
			Twin:                 twin{method: http.MethodPost, pathTemplate: "/api/sessions/{sessionID}/plans/{planId}/approve", handler: twins.ApprovePlan},
			InputDef:             "ApprovePlanToolRequest",
			OutputDef:            "PlanActionResponse",
			Annotations:          approvePlanAnnotations,
			RefusedWhileTurnOpen: true,
			BuildRequest:         buildApprovePlanRequest,
		},
		{
			Name:         "narvi_reject_plan",
			Description:  "Reject a plan awaiting approval, as the user who approved this client -- the same as POST /api/sessions/{sessionID}/plans/{planId}/reject, with the same checks: that user's own role (as for narvi_approve_plan), and the plan still awaiting approval and belonging to that session (the first decision wins, whichever channel made it). No turn is queued and nothing runs; that plan version stays rejected, and, as for an approval, the verdict is posted to the Slack message or the Linear session the plan went to, if it went to one. To change a plan rather than drop it, use narvi_request_plan_revision instead. Returns the plan's id and its status (rejected).",
			Scope:        mcpscope.Write,
			Instruction:  "narvi_reject_plan (reject a plan awaiting approval)",
			Twin:         twin{method: http.MethodPost, pathTemplate: "/api/sessions/{sessionID}/plans/{planId}/reject", handler: twins.RejectPlan},
			InputDef:     "RejectPlanToolRequest",
			OutputDef:    "PlanActionResponse",
			Annotations:  rejectPlanAnnotations,
			BuildRequest: buildRejectPlanRequest,
		},
		{
			Name:                 "narvi_request_plan_revision",
			Description:          "Ask for the next version of a session's plan, as the user who approved this client -- the same as POST /api/sessions/{sessionID}/turns with planMode true and feedback as the prompt, the web's own request for changes, with the same checks: that user's own role (an admin or maintainer on any session, a member only on a session they started or joined, a viewer never). It queues a plan-mode turn in the session's own conversation, which runs in its sandbox and spends on models; when that turn completes it writes the next plan version, awaiting approval, and the version it replaces, if one was still awaiting approval, is superseded. Refused while any turn of the session is queued or running -- an approved implementation included: nothing is queued, that implementation keeps its approval and delivers what it changes, and the call can be made again once the session has settled (narvi_wait_for_session). Returns the new turn's id and status.",
			Scope:                mcpscope.Write,
			Instruction:          "narvi_request_plan_revision (ask for the next version of a plan, with feedback)",
			Twin:                 twin{method: http.MethodPost, pathTemplate: "/api/sessions/{sessionID}/turns", handler: twins.CreateTurn},
			InputDef:             "RequestPlanRevisionToolRequest",
			OutputDef:            "CreateTurnResponse",
			Annotations:          queueTurnAnnotations,
			RefusedWhileTurnOpen: true,
			BuildRequest:         buildRequestPlanRevisionRequest,
		},
		{
			Name:                 "narvi_send_prompt",
			Description:          "Send a prompt to a session, as the user who approved this client -- the same as POST /api/sessions/{sessionID}/turns with planMode false, with the same checks: that user's own role (an admin or maintainer on any session, a member only on a session they started or joined, a viewer never). It queues a turn in the session's own conversation, which runs code in its repositories and spends on models. Refused while any turn of the session is queued or running: nothing is queued, so wait for the session to settle (narvi_wait_for_session) and send it then. While a plan awaits approval it is refused too, unless the server reads the prompt as a change to that plan, in which case it is queued as a revision of it; to decide the plan instead, use narvi_approve_plan or narvi_reject_plan. Returns the new turn's id and status.",
			Scope:                mcpscope.Write,
			Instruction:          "narvi_send_prompt (send a prompt to a session with no turn queued or running)",
			Twin:                 twin{method: http.MethodPost, pathTemplate: "/api/sessions/{sessionID}/turns", handler: twins.CreateTurn},
			InputDef:             "SendPromptToolRequest",
			OutputDef:            "CreateTurnResponse",
			Annotations:          queueTurnAnnotations,
			RefusedWhileTurnOpen: true,
			BuildRequest:         buildSendPromptRequest,
		},
		{
			Name:          "narvi_stop_session",
			Description:   "Stop a session and every session it started, as the user who approved this client -- the same as POST /api/sessions/{sessionID}/stop, with the same checks: that user's own role (an admin or maintainer may stop any session, a member only a session they started or joined and never a pull request's review session, a viewer none). Every turn of those sessions that is queued or running when the call is made is cancelled: a queued one at once, never started, and a running one once its sandbox confirms the stop, or 30 seconds later (as shipped) if it does not; nothing is pushed for it. A turn that already completed keeps its push and pull request, and a plan keeps its status. While the stop stands, none of those sessions starts a new session of its own; the next prompt, plan approval or workflow-step decision a person makes on one sets it going again, and a turn created after the call runs normally. The call answers once the request is written, before that work has ended: follow it with narvi_wait_for_session. Calling it again is not a no-op: it also stops whatever was started since. Once the stop is written, a call that ends early -- a timeout, a dropped connection -- does not cut short the stop of the sessions it started. An internal error means either that nothing was written or that the session was stopped but not every session it started could be reached: calling it again is safe either way, and reaches the rest. Returns sessionId, requestedAt, reachedSessionIds (the session, then every session it started) and openTurns (how many turns it asked to cancel).",
			Scope:         mcpscope.Write,
			Instruction:   "narvi_stop_session (stop a session's queued and running work, and that of every session it started)",
			Twin:          twin{method: http.MethodPost, pathTemplate: "/api/sessions/{sessionID}/stop", handler: twins.StopSession},
			InputDef:      "StopSessionToolRequest",
			OutputDef:     "StopSessionResponse",
			Annotations:   stopSessionAnnotations,
			StopsSessions: true,
			BuildRequest:  buildStopSessionRequest,
		},
	}
}

// readOnlyAnnotations is shared by every read tool in the table
// (technical plan §43.8): a plain read, never destructive, always
// idempotent, and never reaching outside this deployment ("open world").
var readOnlyAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(false),
}

// createSessionAnnotations are narvi_create_session's (technical plan
// §43.8): it writes, but only adds a session, never destroys one; a retry
// with the same idempotencyKey starts nothing more; and the run it starts
// reads from and pushes to the code host, outside this deployment.
var createSessionAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    false,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(true),
}

// approvePlanAnnotations are narvi_approve_plan's (technical plan §43.21):
// it writes, but destroys nothing -- it moves a plan from awaiting to
// approved and adds a turn; a second call with the same arguments changes
// nothing more (the first verdict wins, a later one is refused); and it
// reaches outside this deployment: the implementation it queues reads from
// and pushes to the code host, and the verdict is posted to the Slack
// message or the Linear session the plan went to.
var approvePlanAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    false,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(true),
}

// rejectPlanAnnotations are narvi_reject_plan's (technical plan §43.21): a
// rejection is final for that plan version -- the one plan tool that ends
// something, so it is marked destructive -- a second call changes nothing
// more, and, though it queues no turn, it reaches outside this deployment:
// the verdict is posted to the Slack message or the Linear session the plan
// went to, as an approval's is.
var rejectPlanAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    false,
	DestructiveHint: boolPtr(true),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(true),
}

// queueTurnAnnotations are shared by narvi_request_plan_revision and
// narvi_send_prompt: each call that is accepted queues one more turn, so
// neither is idempotent; neither destroys anything; and the turn reads
// from the code host, which a completed turn's branch is pushed to -- a
// revision's as much as a prompt's.
var queueTurnAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    false,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  false,
	OpenWorldHint:   boolPtr(true),
}

// stopSessionAnnotations are narvi_stop_session's (technical plan §43.22).
// Destructive: it cancels running work, which pushes nothing. Not
// idempotent: its twin documents that a repeat is not a no-op -- it flags
// whatever is open at that moment, a turn created since the first call
// included, and writes an audit row of its own -- so a client must not
// retry it blindly. Open-world: a cancelled turn's notice is posted to the
// Slack thread or the Linear session a session came from, and a cancelled
// review attempt's check on the code host is updated, exactly as when a
// turn ends any other way.
var stopSessionAnnotations = &sdkmcp.ToolAnnotations{
	ReadOnlyHint:    false,
	DestructiveHint: boolPtr(true),
	IdempotentHint:  false,
	OpenWorldHint:   boolPtr(true),
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
//
// brake is Config.CreateBrake, consulted only by a spec whose CreateBrake is
// set, keyed by the grant the call is authorized by, after the arguments
// validate and BuildRequest succeeds and before callTwin: a refused call
// runs no twin and writes nothing, and answers isError with how long to
// wait (technical plan §43.8).
func (spec toolSpec) toolHandler(ctx context.Context, inputSchemas map[string]*jsonschema.Schema, brake CreateBrake) sdkmcp.ToolHandler {
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

		call, buildErr := spec.BuildRequest(req.Params.Arguments)
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
		if spec.CreateBrake {
			if refused, defect := createBrakeRefusal(ctx, spec, brake); refused != nil || defect != nil {
				return refused, defect
			}
		}
		status, body := callTwin(ctx, spec.Twin, call)
		return mapOutcome(status, body)
	}
}

// createBrakeRefusal takes one call from the calling grant's bucket in
// brake -- a same-key retry included: only the twin can tell a call is one
// (technical plan §43.8) -- and returns the tool result refusing the call
// when the bucket is empty -- nil, nil when the call may go on. A call with no grant
// on its context, or a handler built with no brake, is this package's own
// defect (buildServer registers no tool without a grant; NewHandler refuses
// a nil brake): -32603, never let through unbraked.
func createBrakeRefusal(ctx context.Context, spec toolSpec, brake CreateBrake) (*sdkmcp.CallToolResult, error) {
	grant, ok := platform.MCPGrantFromContext(ctx)
	if !ok || brake == nil {
		platform.Logger(ctx).Error("mcp: create brake has no grant or no brake to consult", "tool", spec.Name, "grant", ok, "brake", brake != nil)
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
	}
	allowed, retryAfter := brake.Allow(grant.GrantID)
	if allowed {
		return nil, nil
	}
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	platform.Logger(ctx).Warn("mcp: session start refused by the create brake",
		"tool", spec.Name, "grant_id", grant.GrantID, "client_id", grant.ClientID, "retry_after_seconds", seconds)
	refused := &sdkmcp.CallToolResult{}
	refused.SetError(fmt.Errorf("too many sessions started through this authorization; retry in %d s", seconds))
	return refused, nil
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
		Annotations:  spec.Annotations,
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
func registerTools(ctx context.Context, s *sdkmcp.Server, visible []toolSpec, inputSchemas map[string]*jsonschema.Schema, brake CreateBrake) error {
	for _, spec := range visible {
		tool, err := buildTool(spec)
		if err != nil {
			return err
		}
		s.AddTool(tool, spec.toolHandler(ctx, inputSchemas, brake))
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

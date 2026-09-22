package opencode

import "encoding/json"

// This file holds this adapter's own understanding of OpenCode's real
// HTTP+SSE wire shapes — EMPIRICALLY VERIFIED against the real, installed
// OpenCode 1.17.15 binary during this Step's own research pass (a real
// `opencode serve` process was started; real scripted turns — a plain-text
// reply, a bash-tool-invoking prompt, and an aborted turn — were run
// against it, and their real request/response/SSE bodies captured), unless
// a field's own comment below says otherwise (schema-derived only, not
// independently observed live). See package doc.go for the full
// verified-vs-schema-derived-vs-best-effort breakdown. No OpenCode client
// library is vendored here — this adapter is a pure HTTP+SSE client,
// mirroring internal/adapters/outbound/modal's own shape.

// --- sessionResponse: POST /session's response body ------------------------

// sessionResponse is POST /session's response body. VERIFIED live:
// {"id":"ses_...","slug":"...","projectID":"...","directory":"...",
// "cost":0,"tokens":{...},"title":"...","version":"1.17.15","time":{...}}.
// Only the two fields this adapter actually uses are modeled.
type sessionResponse struct {
	ID string `json:"id"`
}

// --- prompt_async request body ---------------------------------------------

// promptAsyncRequest is POST /session/{id}/prompt_async's request body.
// VERIFIED live via /doc (OpenAPI): {"messageID"?, "model"?:
// {"providerID","modelID"}, "agent"?, "parts": [...], "variant"?}. Only the
// fields this adapter sends are modeled; "noReply"/"tools"/"format"/"system"
// remain real but unused by this Step's own scope. "variant" (§29.8:
// "reasoning-effort overrides") carries cmd.Effort verbatim -- see
// postPromptAsync (session.go).
//
// Agent ("plan mode, web", §8.1) is this adapter's own wiring of
// cmd.PlanMode (sandboxws.Prompt) onto OpenCode's REAL, NATIVE plan/build
// agent split -- empirically confirmed live against the pinned OpenCode
// 1.17.15 binary (GET /agent returns 7 agents; "plan" is
// mode:"primary", description "Plan mode. Disallows all edit tools.",
// with a permission list structurally denying the edit tool via OpenCode's
// OWN permission engine: {"permission":"edit","pattern":"*","action":
// "deny"}, vs "build"'s unrestricted "*"->allow). Set to "plan" when
// cmd.PlanMode is true, omitted (OpenCode's own default "build" agent)
// otherwise -- see postPromptAsync (session.go) for where this is set,
// and that same investigation's own honest caveat: "plan"'s permission
// list has NO override for "bash" (only edit/task(general) are denied),
// so this is a real structural guard on file EDITS specifically, not a
// complete sandbox -- bash could in principle still write files. This is
// strictly stronger than a prompt-only instruction (which this Step does
// NOT ALSO add: a real, enforced mode already exists, so layering a
// redundant textual instruction on top would only invite the two to
// drift), but it is not a hard filesystem guarantee either -- hardening
// bash specifically (e.g. a sandbox-agent-level tool restriction) is
// explicitly out of this Step's scope, left for a future hardening pass.
type promptAsyncRequest struct {
	Model   *promptModelRef   `json:"model,omitempty"`
	Agent   *string           `json:"agent,omitempty"`
	Parts   []promptPartInput `json:"parts"`
	Variant *string           `json:"variant,omitempty"`
}

// planAgentName is the literal OpenCode agent name this adapter requests
// for a plan-mode turn -- VERIFIED live via GET /agent (see
// promptAsyncRequest's own doc comment above): one of OpenCode's own 7
// native agents, not a Narvi-invented string.
const planAgentName = "plan"

// promptModelRef is prompt_async's own "model" object shape: {"providerID",
// "modelID"} — VERIFIED live via /doc, both fields required together.
type promptModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// promptPartInput is one element of prompt_async's "parts" array. This
// adapter only ever sends a single {"type":"text","text":"..."} part
// (VERIFIED live: TextPartInput's own schema requires exactly "type" and
// "text") — OpenCode's own schema also allows file/agent/subtask part
// inputs, unused by this Step's own scope (a single free-text prompt per
// turn).
type promptPartInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- summarize request/response (POST /session/{id}/summarize, §7.2) -------

// summarizeRequest is POST /session/{id}/summarize's request body —
// VERIFIED live via GET /doc (the real OpenAPI schema): {"providerID",
// "modelID", "auto"?}, with BOTH providerID and modelID listed under
// "required" (an empty {} body independently reproduced live to return
// HTTP 400 {"name":"BadRequest","data":{"message":"Missing key\n  at
// [\"providerID\"]"}} against the pinned OpenCode 1.17.15 binary) — UNLIKE
// promptAsyncRequest's own optional "model" field above, /summarize has no
// "omit and let OpenCode pick" option at all. Auto is never set by this
// adapter (omitted, not false — its own real-world meaning, per §7.2's
// research and dispatchPart's own "compaction" case, is "did the ENGINE
// decide to compact on its own", which is never true for a call this
// adapter itself explicitly issued) and so is not modeled as a field here.
type summarizeRequest struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// --- model catalog (GET /api/model) -----------------------------------------

// modelCatalogResponse is GET /api/model's response body — VERIFIED live:
// {"location":{...},"data":[{"id","providerID",...}, ...]}. This adapter
// only uses Data's own length (§7's "on empty/failed catalog, fall back"
// quirk is a liveness check, not a per-model lookup — see
// resolveModelForced's own doc comment), so individual entries are not
// modeled further.
type modelCatalogResponse struct {
	Data []json.RawMessage `json:"data"`
}

// --- the persistent global SSE stream (GET /event) --------------------------

// sseEnvelope is the outer shape of every line on the global GET /event
// SSE stream — VERIFIED live: every line is `data: <json>\n\n`, decoding to
// {"id":"evt_...","type":"<dotted.type>","properties":{...}}.
type sseEnvelope struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// messageUpdatedProps is "message.updated"'s own properties shape —
// VERIFIED live: {"sessionID":"ses_...","info":{...}}.
type messageUpdatedProps struct {
	SessionID string              `json:"sessionID"`
	Info      openCodeMessageInfo `json:"info"`
}

// openCodeMessageInfo is the subset of OpenCode's own AssistantMessage/
// UserMessage union this adapter reads — ID and Role are VERIFIED live on
// every message.updated event (both roles, "user" and "assistant"). Error
// is schema-derived (present in the real /doc OpenAPI schema's own
// AssistantMessage.error field) but NOT populated in either successful
// scripted turn run live during this Step's own research pass — it WAS
// observed live, however, on the third scripted turn (an aborted turn),
// confirming the shape: {"error":{"name":"MessageAbortedError",
// "data":{"message":"Aborted"}}}.
//
// ModelID/ProviderID (§7.3, "the model that actually ran"): SCHEMA-DERIVED
// ONLY, and honestly weaker than this file's usual "schema-derived"
// citation — the pinned 1.17.15 binary's own /doc was never re-queried for
// this specific pair (no opencode binary is reachable in this dev
// environment; the earlier research pass that captured every OTHER field
// on this type never needed them). Sourced instead from OpenCode's own
// publicly-generated SDK type definitions for its AssistantMessage type,
// which list BOTH as required (non-optional) string fields alongside
// "error" — i.e. the engine's own report of which model produced this
// exact assistant message, not this adapter's own request-side guess.
// That source is drawn from a newer revision than the pinned binary, and a
// revision difference is a REAL, confirmed risk here, not a hypothetical
// one: the SAME fetch also showed openCodeTaggedError's own error-union
// membership narrowed relative to what THIS file documents (5 named
// variants there vs. 8 here), proving the schema has drifted release to
// release. Modeled anyway, deliberately fail-soft: json.Unmarshal leaves
// an absent key at its Go zero value ("") with no error, so if the pinned
// 1.17.15 binary's real payload turns out not to carry one or both of
// these keys, decoding degrades to the exact same "" this adapter already
// reported before this field existed — never a wrong value, only
// possibly still an absent one. See modelDisplayFromInfo's own doc
// comment (diagnostic.go) for how a partial/absent pair is handled.
type openCodeMessageInfo struct {
	ID         string               `json:"id"`
	Role       string               `json:"role"`
	Error      *openCodeTaggedError `json:"error"`
	ModelID    string               `json:"modelID"`
	ProviderID string               `json:"providerID"`
}

// openCodeTaggedError is the tagged-union shape OpenCode uses for both
// session.error's own "error" property and an assistant message's own
// "error" field — VERIFIED live for name="MessageAbortedError" (an aborted
// turn); the other 7 tagged-union member names (ProviderAuthError,
// UnknownError, MessageOutputLengthError, StructuredOutputError,
// ContextOverflowError, ContentFilterError, APIError) are schema-derived
// only (confirmed present in /doc, not independently elicited live — none
// of this Step's own scripted turns produced one).
//
// Data (this Step: "typed transient-error retry for the OpenCode adapter")
// decodes APIError's own "data" object — VERIFIED against the real,
// live-fetched /doc OpenAPI schema (components.schemas.APIError):
// {"data":{"message","statusCode"?,"isRetryable","responseHeaders"?,
// "responseBody"?,"metadata"?}, "required":["message","isRetryable"]} —
// "isRetryable" is a REQUIRED field whenever "data" is present at all, an
// explicit transient-vs-permanent verdict OpenCode itself already computed
// after calling out to the upstream model provider. A *pointer* here (not
// embedded flat on this struct) deliberately, because OTHER tagged-union
// members carry a "data" object with a DIFFERENT, unrelated shape (e.g.
// MessageAbortedError's own real live-verified payload,
// {"data":{"message":"Aborted"}}, has no "isRetryable" key at all) — see
// isTransientAPIError's own doc comment (outcome.go) for why this
// adapter never trusts Data.IsRetryable without first checking Name ==
// "APIError".
type openCodeTaggedError struct {
	Name string             `json:"name"`
	Data *openCodeErrorData `json:"data,omitempty"`
}

// openCodeErrorData is openCodeTaggedError's own "data" object, modeled
// only for the fields this package actually reads anywhere — the
// typed-transient-retry classification (isTransientAPIError, outcome.go)
// and, since §7.3 ("a retry decision is not a diagnosis"), the
// allowlisted provider-failure diagnostic (diagnostic.go) — VERIFIED
// against the real, live-fetched /doc OpenAPI schema
// (components.schemas.APIError), see openCodeTaggedError's own doc
// comment above for the full captured shape:
// {"data":{"message","statusCode"?,"isRetryable","responseHeaders"?,
// "responseBody"?,"metadata"?}, "required":["message","isRetryable"]}.
//
// THIS STRUCT IS THE ALLOWLIST §7.3 REQUIRES, enforced structurally, not
// by a comment: encoding/json silently drops any object key with no
// matching struct field, so "responseBody" and "metadata" — the real
// schema's own two other fields, exactly where credentials and the
// prompt itself live (responseBody), and a free-form catch-all
// (metadata) — are never modeled here AT ALL, which means they are never
// decoded into any Go value anywhere in this program, not merely
// "read and discarded" after the fact. A blocklist over those two named
// fields would already be one behind the real schema today (metadata is
// a THIRD field neither this struct nor any blocklist written against
// "the two known-dangerous fields" would have named) — the allowlist
// degrades safely instead: it can only ever retain less than the real
// payload carries, never accidentally more.
//
// StatusCode is OPTIONAL (e.g. a real HTTP status the upstream provider
// returned, 429/529) — corroborating detail only, never itself consulted
// for retry classification (this Step's own explicit instruction:
// classify ONLY on the typed isRetryable field, never on a substring of
// error text, and statusCode is exactly that kind of secondary signal a
// future misguided change might be tempted to string/number-match on
// instead).
//
// Message and ResponseHeaders were added for §7.3's own diagnostic
// alone — neither is read by isTransientAPIError, and ResponseHeaders is
// read by exactly one function in this package, extractProviderRequestID
// (diagnostic.go), which returns a single capped string and never the map
// itself; see that function's own doc comment for why that keeps the
// allowlist enforced by ProviderFailureDiagnostic's own field TYPES
// (string/*int only — nothing a whole map could ever be assigned to)
// rather than by convention. ResponseHeaders' own VALUE shape
// (single string vs. an array, matching net/http.Header's own JSON
// convention) is NOT independently verified live — this adapter's own
// research pass (openCodeTaggedError's own doc comment above) never
// elicited a genuine APIError from a live OpenCode process, so
// json.RawMessage defers that decision to extractProviderRequestID
// itself, which tries both shapes rather than assuming one.
type openCodeErrorData struct {
	Message         string                     `json:"message"`
	IsRetryable     bool                       `json:"isRetryable"`
	StatusCode      *int                       `json:"statusCode,omitempty"`
	ResponseHeaders map[string]json.RawMessage `json:"responseHeaders,omitempty"`
}

// messagePartUpdatedProps is "message.part.updated"'s own properties shape
// — VERIFIED live: {"sessionID":"ses_...","part":{...},"time":...}. Part is
// decoded further by dispatchPart, keyed on its own "type" discriminator
// (see partEnvelope) since Go has no generated union type for it (the same
// "no discriminated-union wrapper" situation
// internal/sandboxagent/wsbridge's own doc.go documents for inbound
// commands).
type messagePartUpdatedProps struct {
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
}

// partEnvelope is the common shape every OpenCode message-part type
// shares, peeked first to decide which concrete type to decode the same
// raw bytes into — VERIFIED live for text/step-start/step-finish/tool;
// schema-derived only for subtask (see subtaskPart's own comment).
type partEnvelope struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
}

// compactionPart is a `"type":"compaction"` part — SCHEMA-DERIVED only
// (confirmed present in the real /doc OpenAPI schema's CompactionPart def;
// not independently elicited live during this Step's own research pass —
// manually triggering one via POST /session/{id}/compact returned
// "Session compact is not available yet" on this OpenCode version, and
// organically eliciting one needs a context window large enough to force
// it). Auto distinguishes an automatic (context-window-driven) compaction
// from a manually-requested one; Overflow specifically signals the
// context window genuinely ran out of room mid-turn — an operationally
// meaningful degradation worth surfacing, see dispatchPart's "compaction"
// case (sse.go) and §7's own "handle compaction events" quirk.
type compactionPart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	Auto      bool   `json:"auto"`
	Overflow  bool   `json:"overflow"`
}

// textPart is a `"type":"text"` part — VERIFIED live: Text is the FULL
// CUMULATIVE text so far for this part id (a later update replaces an
// earlier, shorter/empty one), matching wire Token's own cumulative
// contract exactly.
type textPart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	Text      string `json:"text"`
}

// stepFinishPart is a `"type":"step-finish"` part — VERIFIED live:
// {"id","reason","messageID","type":"step-finish","tokens":{"total",
// "input","output","reasoning","cache":{"read","write"}},"cost"}.
//
// Cost is *float64, not float64 (§25.15) -- deliberately, so a "cost"
// key genuinely ABSENT from OpenCode's own JSON (a version bump dropping
// or renaming it) decodes to nil, distinguishable from an explicit
// "cost":0 (a real, priced-at-zero step -- e.g. no catalog pricing entry
// for the resolved model), which decodes to a non-nil pointer to 0. A
// plain float64 here would collapse both cases onto the identical zero
// value, making "cost stopped arriving" silently indistinguishable from
// "this step genuinely cost nothing" -- exactly the failure §25.15 (and
// the pinned-binary contract test, realturn_test.go's own
// TestRealTurn_PlainTextPrompt) exists to catch.
type stepFinishPart struct {
	ID        string           `json:"id"`
	MessageID string           `json:"messageID"`
	Tokens    stepFinishTokens `json:"tokens"`
	Cost      *float64         `json:"cost"`
}

type stepFinishTokens struct {
	Input  float64         `json:"input"`
	Output float64         `json:"output"`
	Cache  stepFinishCache `json:"cache"`
}

type stepFinishCache struct {
	Read float64 `json:"read"`
}

// toolPart is a `"type":"tool"` part — VERIFIED live across the full
// pending -> running (xN) -> completed state progression, and separately
// for an aborted call (still reaches "completed", never "error", in the
// one abort scripted live — "error" itself is schema-derived only, not
// independently observed live).
type toolPart struct {
	ID        string        `json:"id"`
	MessageID string        `json:"messageID"`
	Tool      string        `json:"tool"`
	CallID    string        `json:"callID"`
	State     toolPartState `json:"state"`
}

// toolPartState is toolPart's own "state" object — VERIFIED live: Status
// is always present; Input is always an object (possibly empty, `{}`, in
// the "pending" state); Output is a STRING (not an object!) present only
// once Status is "completed" — schema-confirmed AND live-verified;
// Error is a STRING present only once Status is "error" — schema-derived
// only (ToolStateError's own real /doc shape), not independently observed
// live (no scripted turn produced a genuine tool error). Metadata is a
// freeform, tool-specific object (real /doc schema: present on
// ToolStateRunning/ToolStateCompleted/ToolStateError, absent on
// ToolStatePending) — VERIFIED LIVE, and load-bearing, for exactly one
// tool: see taskToolMetadata below and this package's own Adapter doc
// comment for the §7.1 sub-task correlator this Step's own investigation
// found on it. For any other tool this adapter does not attempt to
// interpret it further.
type toolPartState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   *string         `json:"output"`
	Error    *string         `json:"error"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// taskToolMetadata is the "task" tool's own toolPartState.Metadata shape —
// VERIFIED LIVE (this Step's own investigation: a real `opencode serve`
// process, a real scripted prompt instructing the model to delegate via
// the task tool, the real resulting SSE trace captured and inspected) —
// §7.1's own "sub-task fan-out" correlator: ParentSessionID is the
// enclosing turn's own OpenCode session id (matches messageUpdatedProps.
// SessionID for the turn's own main lane); SessionID is the newly-spawned
// sub-agent's own, DISTINCT OpenCode session id — every message/part event
// for the sub-agent's own inner activity (text/tool/step-start/step-finish)
// subsequently arrives on the SAME global /event stream tagged with THIS
// session id as its own top-level "sessionID", not the enclosing turn's.
// Confirmed present once state.status reaches "running", and REMAINS
// present at "completed" (a real observed field key ordering difference
// from other camelCase OpenCode fields elsewhere in this file: this
// object's own keys are "parentSessionId"/"sessionId", lowercase "d", not
// the "...SessionID" spelling every top-level part/props field uses).
// See adapter.go's own package doc comment for the full writeup, including
// the surprising fact that OpenCode's own "subtask" part type (subtaskPart
// below) did NOT fire at all for this real, live-triggered invocation —
// this ordinary "tool" part is the mechanism actually observed on the wire
// for OpenCode 1.17.15's own task-tool sub-agents.
type taskToolMetadata struct {
	ParentSessionID string `json:"parentSessionId"`
	SessionID       string `json:"sessionId"`
}

// subtaskPart is a `"type":"subtask"` part — SCHEMA-DERIVED ONLY (confirmed
// present in the real, live /doc OpenAPI schema's own SubtaskPart
// definition). A LATER Step's own investigation (see adapter.go's own
// package doc comment) DID actually trigger a real task-tool invocation
// live — and this part type still never appeared on the wire: the real
// signal observed was an ordinary "tool" part (tool=="task") carrying
// taskToolMetadata (toolPartState above), not this one. This part type
// therefore remains schema-present but unverified-live, kept only as an
// extra, honestly-labeled fallback path (dispatchSubtaskStart, sse.go) in
// case some other OpenCode-internal path emits it — the task-tool+metadata
// mechanism is the PRIMARY, empirically-verified sub-task announcement
// path this adapter now relies on.
type subtaskPart struct {
	ID          string `json:"id"`
	MessageID   string `json:"messageID"`
	Description string `json:"description"`
}

// sessionIdleProps is "session.idle"'s own properties shape — VERIFIED
// live: {"sessionID":"ses_..."}. THE signal a turn (or, best-effort, a
// sub-task) has concluded.
type sessionIdleProps struct {
	SessionID string `json:"sessionID"`
}

// sessionErrorProps is "session.error"'s own properties shape — VERIFIED
// live (the aborted-turn scripted run): {"sessionID":"ses_...","error":
// {"name":"MessageAbortedError","data":{"message":"Aborted"}}}.
type sessionErrorProps struct {
	SessionID string              `json:"sessionID"`
	Error     openCodeTaggedError `json:"error"`
}

// --- final-state fetch fallback (GET /session/{id}/message) ----------------

// messageListEntry is one element of GET /session/{id}/message's response
// array — VERIFIED live: [{"info":{...},"parts":[...]}, ...]. Used only by
// the SSE-inactivity final-state fallback (§7).
type messageListEntry struct {
	Info  openCodeMessageInfo `json:"info"`
	Parts []json.RawMessage   `json:"parts"`
}

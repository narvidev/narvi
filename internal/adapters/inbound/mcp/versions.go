package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
)

// MaxRequestBodyBytes is httpapi.MaxRequestBodyBytes, the SAME 1 MiB cap
// every REST body this codebase decodes is bounded by -- referenced
// here, not re-declared, so this package's own StreamableHTTPOptions.
// MaxRequestBodyBytes (handler.go) and peekRequestID's own read bound
// below can never drift from that single figure.
const MaxRequestBodyBytes = httpapi.MaxRequestBodyBytes

// SupportedProtocolVersions is the SINGLE source of truth for which MCP
// protocol revisions this deployment speaks, newest first (technical
// plan §43.4). server/discover's own supportedVersions, the -32022
// refusal's data.supported/message, mcp.ServerOptions.
// SupportedProtocolVersions (which can only NARROW the SDK's own
// broader default list, never widen it), and every test in this package
// all read this one slice -- nothing else declares a second copy.
//
// 2026-07-28 is current (the per-request `_meta` era, no `initialize`
// handshake). 2025-11-25, 2025-06-18, and 2025-03-26 are the legacy
// Streamable-HTTP revisions this server also serves through the
// `initialize` handshake, because "existing MCP clients" -- the exit
// criterion's own phrase -- mostly still speak one of those.
// 2024-11-05 is deliberately EXCLUDED: its transport is the deprecated
// HTTP+SSE pair, which would require hosting a second endpoint shape
// entirely, not merely a version this server negotiates on the one
// endpoint it has (§43 D4).
var SupportedProtocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// protocolVersionHeader is the Streamable HTTP transport's own version
// header (net/http canonicalizes header lookups case-insensitively, so
// the exact case written here does not matter to Header.Get). Mirrors
// the pinned SDK's own unexported constant of the same name and value
// (mcp/streamable_headers.go) -- kept as our own copy rather than an
// import, since the SDK does not export it.
const protocolVersionHeader = "MCP-Protocol-Version"

// codeUnsupportedProtocolVersion is SEP-2575's JSON-RPC error code for an
// unsupported protocol version (jsonrpc.Error's own Code field; the SDK
// exports the identical value as mcp.CodeUnsupportedProtocolVersion, not
// re-imported here to keep this file's own refusal fully independent of
// the SDK -- see versionGate's own doc comment for why that independence
// is the point).
const codeUnsupportedProtocolVersion = -32022

// isSupportedVersion reports whether v is one of SupportedProtocolVersions.
func isSupportedVersion(v string) bool {
	for _, s := range SupportedProtocolVersions {
		if s == v {
			return true
		}
	}
	return false
}

// versionGate is OUR OWN version check, run in front of the SDK's own
// handler entirely (technical plan §43.4). It exists because the
// pinned SDK, while it DOES refuse an unsupported per-request `_meta`
// protocol version with the correct code/HTTP-status shape (verified
// against the pinned release's own mcp/server.go: JSON-RPC -32022, HTTP
// 400, data.supported/data.requested), builds its own error MESSAGE as
// the fixed string "unsupported protocol version" -- it never names which
// versions it actually speaks in the message text. The exit criterion
// requires exactly that ("a message naming the versions it does"), so
// this gate intercepts first and answers the refusal itself, in the
// SAME code/HTTP-status/data shape, whenever the request names an
// MCP-Protocol-Version header this deployment does not speak.
//
// If the header is ABSENT, this gate does nothing: the spec's own
// backward-compatibility allowance is that a request with no version
// header may be a pre-2025-06-18 `initialize` handshake, which carries
// its OWN protocol version inside the request body instead -- passed
// straight through to the SDK, whose legacy `initialize` handling
// negotiates (and, for a version it does not speak, counter-offers) on
// its own (§43.4 case 2). This gate is therefore never the ONLY
// version check in front of the SDK -- it narrows one specific case (a
// header naming an unsupported version) to a richer message; every other
// case (header/body mismatch, missing required `_meta` fields, a
// modern request naming NO header while doing so is required) is still
// the SDK's own job, exercised and pinned by this package's own tests
// rather than re-implemented here.
func versionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := r.Header.Get(protocolVersionHeader)
		if requested == "" || isSupportedVersion(requested) {
			next.ServeHTTP(w, r)
			return
		}
		writeUnsupportedVersion(w, r, requested)
	})
}

// jsonrpcIDProbe reads only the "id" member of a JSON-RPC request body,
// so writeUnsupportedVersion can echo it back per the JSON-RPC spec
// (an error response must carry the SAME id as the request that
// triggered it, when the id could be determined at all).
type jsonrpcIDProbe struct {
	ID json.RawMessage `json:"id"`
}

// peekRequestID reads (and consumes) r's body far enough to extract its
// own top-level "id" member, returning the literal JSON `null` if the
// body is absent, unparseable, or carries no id at all -- the JSON-RPC
// spec's own allowance for an error that occurs before the request could
// be identified. Safe to call before rejecting a request outright (this
// gate never forwards to next afterward), but MUST NOT be called on a
// path that still needs the body afterward.
func peekRequestID(r *http.Request) json.RawMessage {
	if r.Body == nil {
		return json.RawMessage("null")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxProbeBodyBytes))
	_ = r.Body.Close()
	if err != nil {
		return json.RawMessage("null")
	}
	var probe jsonrpcIDProbe
	if err := json.Unmarshal(data, &probe); err != nil || len(probe.ID) == 0 {
		return json.RawMessage("null")
	}
	return probe.ID
}

// maxProbeBodyBytes bounds peekRequestID's own read -- a request this
// gate is about to refuse outright never needs more than a shallow read
// to find its own "id" member; MaxRequestBodyBytes (the same 1 MiB cap
// httpapi and the SDK's own StreamableHTTPOptions.MaxRequestBodyBytes
// share) is already generous for that.
const maxProbeBodyBytes = MaxRequestBodyBytes

// unsupportedVersionData is SEP-2575's own `data` payload shape for a
// CodeUnsupportedProtocolVersion error -- field names/JSON tags match the
// SDK's own mcp.UnsupportedProtocolVersionData exactly (verified against
// the pinned release), reimplemented rather than imported so this
// package's own refusal never depends on that SDK type's own evolution.
type unsupportedVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// jsonrpcErrorBody is the wire shape of a JSON-RPC 2.0 error response.
type jsonrpcErrorBody struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   jsonrpcErrorObj `json:"error"`
}

type jsonrpcErrorObj struct {
	Code    int                    `json:"code"`
	Message string                 `json:"message"`
	Data    unsupportedVersionData `json:"data"`
}

// writeUnsupportedVersion writes the -32022 refusal, HTTP 400, naming
// every version SupportedProtocolVersions holds in its own message text
// (technical plan §43.4 case 1). What this improves on is the pinned
// SDK's own refusal SHAPE, not merely its wording: for a header version
// below the current protocol revision that the SDK does not recognize
// (streamable.go's own version check), the SDK already answers a
// plain-text, non-JSON-RPC HTTP 400 that DOES name every supported
// version -- just not in the JSON-RPC -32022 shape the modern spec
// requires. For a request naming an unsupported version at or above the
// current revision instead, the SDK's own -32022 refusal (server.go) has
// the correct JSON-RPC shape already, but a FIXED message text
// ("unsupported protocol version") that never names them. This function
// answers the same, correct -32022 JSON-RPC shape either way, always
// naming every version -- closing both gaps with one refusal, not just
// the second one.
func writeUnsupportedVersion(w http.ResponseWriter, r *http.Request, requested string) {
	id := peekRequestID(r)
	body := jsonrpcErrorBody{
		JSONRPC: "2.0",
		ID:      id,
		Error: jsonrpcErrorObj{
			Code:    codeUnsupportedProtocolVersion,
			Message: fmt.Sprintf("unsupported protocol version %q: this server speaks %s", requested, strings.Join(SupportedProtocolVersions, ", ")),
			Data: unsupportedVersionData{
				Supported: SupportedProtocolVersions,
				Requested: requested,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(body)
}

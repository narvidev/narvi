package mcp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// errorBody is the exact shape httpapi.writeError writes: {"error":
// "<message>"}. errorTextFrom below decodes it to recover the handler's
// own error string, verbatim, for both mapping branches that need it.
type errorBody struct {
	Error string `json:"error"`
}

// errorTextFrom extracts body's own "error" field, falling back to the
// raw body text if it is not that exact shape (should not happen for a
// twin invoked through this bridge -- every httpapi handler in this
// codebase writes errors through the SAME writeError helper -- but never
// silently drops information if some future twin doesn't).
func errorTextFrom(body []byte) string {
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err == nil && eb.Error != "" {
		return eb.Error
	}
	return string(body)
}

// mapOutcome converts one twin's raw HTTP outcome into the MCP outcome a
// tool handler returns, per technical plan §43.8's table (reproduced
// in doc.go). Returning a non-nil error means "this is a JSON-RPC
// PROTOCOL-level error" (the AddTool-wrapped handler in the pinned SDK
// forwards a *jsonrpc.Error returned this way as-is, per its own
// documented "already a structured JSON-RPC error" branch) rather than a
// successful tool result with IsError:true.
func mapOutcome(status int, body []byte) (*sdkmcp.CallToolResult, error) {
	switch status {
	case http.StatusOK:
		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(body)}},
			StructuredContent: json.RawMessage(body),
		}, nil

	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		// Both a request-STRUCTURE problem (malformed session id, a bad
		// filter/limit) and a business refusal (§43 D6: "ask a
		// maintainer", "re-list", or similar) are TOOL EXECUTION errors
		// per the MCP tools specification's own error taxonomy --
		// IsError:true, never a JSON-RPC protocol code. (A prior
		// version of this function treated 400 as a protocol error; the
		// tools spec classifies "input validation errors (e.g., date in
		// wrong format, value out of range)" as tool execution errors
		// explicitly, and toolHandler's own validateArguments call,
		// tools.go, now catches nearly every one of these before the
		// twin is ever invoked -- this branch is what remains reachable
		// for a value the schema's own value-space cannot express, and
		// it must be classified the SAME way.)
		result := &sdkmcp.CallToolResult{}
		result.SetError(errors.New(errorTextFrom(body)))
		return result, nil

	case http.StatusUnauthorized:
		// Unreachable inside the bridge: auth.RequireMCPBearer already ran
		// before this handler could ever be reached. If this fires
		// anyway, it is a defect in THIS package, not a legitimate
		// outcome to translate -- surfaced as a protocol error, never
		// the raw (potentially misleading) body text.
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}

	default:
		// 5xx and anything else this table does not name: "server
		// errors" are protocol errors per the spec's own error taxonomy
		// -- never leak the body.
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
	}
}

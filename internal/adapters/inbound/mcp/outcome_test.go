package mcp

import (
	"errors"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestOutcomeMapping is the table-driven test over technical plan
// §43.8's mapping table (doc.go's own reproduction of it), driven by a
// stub handler writing each status this test names -- TestParity_*
// (integration_test.go) additionally proves the SAME mapping holds for
// the REAL httpapi handlers, not just this synthetic table.
func TestOutcomeMapping(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		wantErr        bool
		wantErrCode    int64
		wantIsError    bool
		wantText       string
		wantStructured bool
	}{
		{
			name:           "200 success carries the body verbatim",
			status:         http.StatusOK,
			body:           `{"id":"abc","title":"hello"}`,
			wantText:       `{"id":"abc","title":"hello"}`,
			wantStructured: true,
		},
		{
			name:     "400 becomes isError:true with the handler's own text, not a protocol error",
			status:   http.StatusBadRequest,
			body:     `{"error":"malformed session id"}`,
			wantText: "malformed session id",
		},
		{
			name:     "403 becomes isError:true with the handler's own text",
			status:   http.StatusForbidden,
			body:     `{"error":"not authorized to perform this action"}`,
			wantText: "not authorized to perform this action",
		},
		{
			name:     "404 becomes isError:true with the handler's own text",
			status:   http.StatusNotFound,
			body:     `{"error":"session not found"}`,
			wantText: "session not found",
		},
		{
			name:     "409 becomes isError:true with the handler's own text",
			status:   http.StatusConflict,
			body:     `{"error":"a turn is already in flight"}`,
			wantText: "a turn is already in flight",
		},
		{
			name:        "401 is a defect signal, never leaks the body",
			status:      http.StatusUnauthorized,
			body:        `{"error":"unauthorized"}`,
			wantErr:     true,
			wantErrCode: jsonrpc.CodeInternalError,
		},
		{
			// The extracted text here is DELIBERATELY different from
			// the fixed "internal error" message mapOutcome must return
			// -- see the "never leak" check below for why a fixture
			// whose own extracted text coincides with the fixed message
			// (a prior revision of this table used exactly
			// {"error":"internal error"} here) can never tell a correct
			// mapping from a leaking one apart.
			name:        "5xx is a protocol error, body never leaked",
			status:      http.StatusInternalServerError,
			body:        `{"error":"boom: db connection refused"}`,
			wantErr:     true,
			wantErrCode: jsonrpc.CodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := mapOutcome(tt.status, []byte(tt.body))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("mapOutcome(%d, ...) error = nil, want a *jsonrpc.Error", tt.status)
				}
				var jerr *jsonrpc.Error
				if !errors.As(err, &jerr) {
					t.Fatalf("mapOutcome(%d, ...) error = %v (%T), want *jsonrpc.Error", tt.status, err, err)
				}
				if jerr.Code != tt.wantErrCode {
					t.Errorf("error.Code = %d, want %d", jerr.Code, tt.wantErrCode)
				}
				if result != nil {
					t.Errorf("result = %+v, want nil alongside a protocol error", result)
				}
				// Never leak the handler's own extracted error text into
				// a protocol-error message for the two "never leak"
				// rows -- compared against errorTextFrom(body) (the
				// SAME extraction every isError branch displays
				// verbatim), never the raw JSON body. A prior version
				// of this check compared jerr.Message against tt.body
				// directly: jerr.Message is always the fixed string
				// "internal error", and tt.body is always a raw JSON
				// object -- the two can NEVER be byte-equal, so that
				// check could never fail, whatever mapOutcome actually
				// returned (verified: swapping either branch below to
				// `Message: errorTextFrom(body)` still passed the old
				// check). Comparing against errorTextFrom(body) instead
				// -- combined with the 5xx fixture's own extracted text
				// now deliberately differing from "internal error" --
				// makes this a real assertion.
				if tt.status == http.StatusUnauthorized || tt.status >= 500 {
					if jerr.Message == errorTextFrom([]byte(tt.body)) {
						t.Errorf("error.Message leaked the handler's own text: %q", jerr.Message)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("mapOutcome(%d, ...) error = %v, want nil", tt.status, err)
			}
			if result == nil {
				t.Fatalf("mapOutcome(%d, ...) result = nil", tt.status)
			}
			if len(result.Content) != 1 {
				t.Fatalf("len(Content) = %d, want 1", len(result.Content))
			}
			text, ok := result.Content[0].(*sdkmcp.TextContent)
			if !ok {
				t.Fatalf("Content[0] = %T, want *sdkmcp.TextContent", result.Content[0])
			}
			if text.Text != tt.wantText {
				t.Errorf("Content[0].Text = %q, want %q", text.Text, tt.wantText)
			}

			wantIsError := tt.status != http.StatusOK
			if result.IsError != wantIsError {
				t.Errorf("IsError = %v, want %v", result.IsError, wantIsError)
			}
			if wantIsError && result.StructuredContent != nil {
				t.Errorf("StructuredContent = %v, want nil on an isError result", result.StructuredContent)
			}
			if tt.status == http.StatusOK && result.StructuredContent == nil {
				t.Error("StructuredContent = nil, want the decoded body on a 200")
			}
		})
	}
}

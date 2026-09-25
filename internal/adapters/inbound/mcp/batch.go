package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// codeInvalidRequest is JSON-RPC 2.0's own "Invalid Request" error code.
const codeInvalidRequest = -32600

// batchRejectedBody is the JSON-RPC 2.0 error envelope rejectBatches
// writes -- deliberately carrying no "data" and always "id":null: a
// batch's error is not any ONE call's own id (round 2 review of PR #324,
// finding N2 -- refusing the whole array is a request-level decision, so
// there is no single id to echo, unlike versionGate's own per-request
// refusal, versions.go).
type batchRejectedBody struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Error   batchRejectedErr `json:"error"`
}

type batchRejectedErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rejectBatches refuses a legacy JSON-RPC batch request -- a body whose
// first non-whitespace byte is '[' -- with a single JSON-RPC `-32600`
// error, HTTP 400, before the request ever reaches versionGate or the
// SDK (round 2 review of PR #324, finding N2). This is a STRUCTURAL fix,
// not a size limit or a rate limit: the pinned SDK accepts a JSON-RPC
// batch for any request it treats as pre-2025-06-18 (which includes
// every request with NO MCP-Protocol-Version header at all, the ordinary
// shape of a legacy `initialize` handshake this server still serves, and
// has no option to disable), fans every call in the batch out onto its
// own goroutine with no concurrency bound, and -- in this handler's own
// JSONResponse mode -- buffers every one of those replies in memory until
// the LAST one completes before writing a single byte back. A single
// ~1 MiB request built entirely of minimal tools/call objects turned
// into roughly 8,000 concurrent twin invocations and a multi-gigabyte
// reply before this fix; refusing the shape outright removes the
// amplification entirely, rather than merely capping it, and needs no
// new platform/timeouts.go entry since nothing here waits on anything.
//
// This reads the ENTIRE request body -- never a bounded look-ahead --
// before deciding (round 3 review of PR #324, findings R1/R2: a prior
// revision peeked only the first batchPeekBytes=64 bytes through a
// bufio.Reader, and treated a peek that was ENTIRELY whitespace as "not a
// batch", forwarding the body untouched. RFC 8259's own insignificant
// whitespace allowance -- space, tab, LF, CR -- has NO length bound, and
// the pinned SDK's own batch decoder (segmentio/encoding/json, reached
// through go-sdk's readBatch) skips exactly that same unbounded run
// before deciding whether a body is a batch. A look-ahead of ANY fixed
// size N is therefore a gate an attacker defeats trivially, with N bytes
// of leading whitespace -- proven against the 64-byte window: a body of
// 64 spaces followed by hundreds of legacy tools/call objects, comfortably
// under the 64 KiB cap, sailed through as "not a batch" and fanned out
// exactly like the original, un-fixed N2 defect). Reading the whole body
// here costs nothing extra that was not already going to be spent: the
// body is capped at MaxRequestBodyBytes (versions.go, 64 KiB) via the
// SAME http.MaxBytesReader shape the SDK's own StreamableHTTPOptions.
// MaxRequestBodyBytes already applies downstream, and the SDK was always
// going to io.ReadAll that identical body itself (streamable.go's
// servePOST) -- this handler merely performs that read ONE call earlier,
// then reinstalls the exact same bytes for the SDK to read again.
func rejectBatches(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				// Mirrors the SDK's own shape for the identical condition
				// (go-sdk v1.8.0 mcp/streamable.go, servePOST/
				// ephemeralConnectOpts) -- this handler now performs the
				// capped read before the SDK ever gets the chance to, so
				// it must answer the SAME 413 the SDK would have.
				http.Error(w, fmt.Sprintf("request body exceeds %d bytes", mbe.Limit), http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if firstNonSpace(body) == '[' {
			writeBatchRejected(w)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// firstNonSpace returns the first byte of b that is not JSON insignificant
// whitespace (RFC 8259 §2 -- space, tab, LF, CR, exactly the four bytes
// the pinned SDK's own batch decoder also skips, with NO bound on how
// many of them may precede the first structural byte -- round 3 review
// of PR #324, findings R1/R2), or 0 if b is empty or entirely whitespace
// (an entirely-whitespace body is never itself the '[' this function
// exists to detect, so returning "not a batch" is correct either way --
// the SDK's own downstream JSON parsing is what answers for an
// otherwise-empty/malformed body).
func firstNonSpace(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return c
	}
	return 0
}

// writeBatchRejected writes the -32600 refusal, HTTP 400.
func writeBatchRejected(w http.ResponseWriter) {
	body := batchRejectedBody{
		JSONRPC: "2.0",
		ID:      json.RawMessage("null"),
		Error: batchRejectedErr{
			Code:    codeInvalidRequest,
			Message: "batching is not supported",
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(body)
}

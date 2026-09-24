package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
)

// codeInvalidRequest is JSON-RPC 2.0's own "Invalid Request" error code.
const codeInvalidRequest = -32600

// batchPeekBytes bounds rejectBatches' own look-ahead: a JSON-RPC request
// body may legally begin with a little insignificant whitespace before
// its first structural byte ('{' for a single call, '[' for a legacy
// batch), but never more than a token amount of it in practice. 64 bytes
// is generous for that and nothing else -- this is a look-AHEAD, not a
// read limit on the request as a whole (MaxRequestBodyBytes, versions.go,
// still bounds the full body, enforced downstream by the SDK's own
// StreamableHTTPOptions.MaxRequestBodyBytes).
const batchPeekBytes = 64

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
// A request whose body is not an array is forwarded completely
// unconsumed: peekFirstNonSpaceByte below reads only a small, bounded
// look-ahead through a bufio.Reader and reinstalls THAT reader (buffered
// bytes included) as r.Body, so every byte the caller sent still reaches
// versionGate and the SDK afterward, untouched.
func rejectBatches(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, isBatch, err := peekFirstNonSpaceByte(r)
		if err != nil {
			// Could not even peek the body -- let the SDK's own body
			// handling (and MaxRequestBodyBytes) answer for it; this
			// gate has nothing useful to add for an I/O error this
			// early.
			next.ServeHTTP(w, r)
			return
		}
		if isBatch {
			writeBatchRejected(w)
			return
		}
		r.Body = body
		next.ServeHTTP(w, r)
	})
}

// peekFirstNonSpaceByte looks ahead into r.Body far enough (batchPeekBytes)
// to find its first non-whitespace byte, reporting whether that byte is
// '[' -- the sole, well-defined signature of a JSON-RPC batch (a JSON
// array), per RFC 8259's own four insignificant-whitespace characters
// (space, tab, LF, CR). It returns a replacement io.ReadCloser carrying
// the SAME bytes r.Body would have, byte for byte, whether or not this
// turns out to be a batch: a bufio.Reader's own Peek fills its internal
// buffer WITHOUT draining it, so wrapping that same *bufio.Reader as the
// new body (paired with the original Close) re-serves every peeked byte
// to the next reader, then falls through to the underlying body for the
// rest.
func peekFirstNonSpaceByte(r *http.Request) (io.ReadCloser, bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return http.NoBody, false, nil
	}
	br := bufio.NewReaderSize(r.Body, batchPeekBytes)
	peeked, err := br.Peek(batchPeekBytes)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, false, err
	}
	body := struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return body, firstNonSpace(peeked) == '[', nil
}

// firstNonSpace returns the first byte of b that is not JSON insignificant
// whitespace (RFC 8259 §2), or 0 if b is empty or entirely whitespace.
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

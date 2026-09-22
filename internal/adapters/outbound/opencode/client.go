package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxResponseBodySize bounds how much of an OpenCode response body ever
// gets read — mirroring internal/adapters/outbound/modal's own
// maxResponseBodySize precedent exactly (a misbehaving server must not
// exhaust memory by streaming an unbounded body over an otherwise-slow-
// but-alive connection). Sized generously above modal's 1 MiB since a
// message-list fallback fetch (GET /session/{id}/message) can carry
// sizeable tool output/text content.
const maxResponseBodySize = 4 << 20 // 4 MiB

// doJSON executes one OpenCode HTTP call bounded by a.requestTimeout — a
// thin wrapper around doJSONTimeout below, unchanged in behavior from
// before this Step: every EXISTING doJSON-routed caller (resolveSession,
// resolveProviderModel, postPromptAsync, postAbort, fetchFinalMessages)
// keeps exactly the same per-request bound it always had. See doJSONTimeout's
// own doc comment for the full write-up of what this does and why the
// timeout is parameterized at all (Finding 3, §7.2): forceCompaction
// (compact.go) is the one caller that needs a DIFFERENT, more generous
// bound (a.summarizeTimeout) and so calls doJSONTimeout directly instead
// of through this wrapper.
func (a *Adapter) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	return a.doJSONTimeout(ctx, a.requestTimeout, method, path, reqBody, out)
}

// doJSONTimeout is doJSON's own extracted body, now taking the per-request
// timeout as an explicit parameter (§7.2 Finding 3) rather than always
// reading a.requestTimeout internally: this Step's own live investigation
// found that a hardcoded a.requestTimeout wrap here would otherwise
// silently cap ANY caller routed through it at 30s (OpenCodeRequestTimeout,
// production default) regardless of what that specific call actually
// needs — most concretely, forceCompaction's own POST /session/{id}/
// summarize call, which this Step's own §7.2 design deliberately gives a
// separate, more generous a.summarizeTimeout (OpenCodeSummarizeTimeout,
// 120s) instead. Every OTHER existing caller is unaffected: doJSON above
// is still the thin, zero-behavior-change wrapper every one of them uses.
//
// JSON-encodes reqBody when non-nil, sends it, and — on a 2xx response —
// JSON-decodes the body into out (when out is non-nil and the body is
// non-empty). Unlike internal/adapters/outbound/modal's own `do` helper,
// failures here are plain wrapped errors, not a typed *ports.ProviderError
// — AgentRuntime (unlike SandboxProvider) has no Transient/permanent
// classification contract of its own (§4.2's interface has no such
// requirement), so there is nothing this Step's own scope asks this
// adapter to classify errors into.
//
// Wraps ctx in a per-request context.WithTimeout(timeout) before building
// the request — every caller (routed through doJSON or directly) is
// protected uniformly this way, without each call site needing to
// remember to wrap its own context. Most critically, fetchFinalMessages is
// called ONLY from finalizeByFallback — i.e. exactly when the
// SSE-inactivity fallback has already concluded something is wrong and
// needs a definitive answer; without this bound, a hung TCP connection on
// THAT call specifically could wedge an already-stuck turn forever.
// Deliberately a per-request wrap here, NOT a client-wide
// a.httpClient.Timeout: connectAndConsume's own GET /event call (sse.go)
// uses this SAME a.httpClient for the intentionally long-lived persistent
// SSE stream, which does NOT go through doJSON/doJSONTimeout at all and so
// is correctly left unaffected by this timeout. context.WithTimeout
// already takes the tighter of timeout and whatever deadline ctx might
// already carry, with no special-casing needed for that interaction --
// this is exactly what lets forceCompaction pass a.bgCtx (no deadline of
// its own) here and still end up bounded by timeout alone.
func (a *Adapter) doJSONTimeout(ctx context.Context, timeout time.Duration, method, path string, reqBody, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("opencode: encode %s %s request: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("opencode: build %s %s request: %w", method, path, err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return fmt.Errorf("opencode: %s %s: read response body: %w", method, path, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("opencode: %s %s: http %d", method, path, resp.StatusCode)
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("opencode: %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

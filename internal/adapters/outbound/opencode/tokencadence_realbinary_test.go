package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/sandboxagent/opencodeproc"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// maxTokenFramesPerTextPart is how many `token` frames the pinned runtime
// sends per assistant text part, whatever the model streams: one empty
// frame when the part opens (message.part.updated at text start) and one
// with the full text when it closes; the deltas in between arrive only as
// message.part.delta, which dispatchEvent ignores. Measured on 1.17.15 by
// the test below. The control plane stores one row per distinct frame
// (sessionactor/tokenframe.go), so this is also the bound on stored rows
// per text part, and a part's stored bytes stay linear in its final
// length. A runtime whose cadence exceeds it fails the test below before
// it can make rows and cumulative bytes grow with every delta; the answer
// then is to coalesce frames in this adapter, not to relax the bound.
const maxTokenFramesPerTextPart = 2

// Delta counts the fake provider below streams for each text part --
// far more than maxTokenFramesPerTextPart, spread over more than a second,
// so a runtime that emitted a frame per delta, or per time slice, could
// not stay under the bound.
const (
	cadenceFirstPartDeltas  = 150
	cadenceSecondPartDeltas = 30
	cadenceDeltaInterval    = 10 * time.Millisecond
)

// fakeChatCompletionsHandler is a chat-completions-compatible
// /v1/chat/completions endpoint that streams scripted replies, so the real
// runtime can run a full turn with no credential and no outside call. A
// request without tools is the runtime's own title generation; the first
// agent request streams a long text part and then calls the bash tool;
// the request carrying the tool result streams a second text part.
func fakeChatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	write := func(delta map[string]any, finish any) {
		chunk := map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": "fake-model",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if finish != nil {
			chunk["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}
		raw, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	}
	streamText := func(prefix string, n int) {
		for i := 0; i < n; i++ {
			write(map[string]any{"content": fmt.Sprintf("%s%03d ", prefix, i)}, nil)
			time.Sleep(cadenceDeltaInterval)
		}
	}

	write(map[string]any{"role": "assistant", "content": ""}, nil)
	switch {
	case !strings.Contains(string(body), `"tools"`):
		write(map[string]any{"content": "Cadence probe"}, nil)
		write(map[string]any{}, "stop")
	case !strings.Contains(string(body), `"role":"tool"`):
		streamText("w", cadenceFirstPartDeltas)
		write(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_cadence", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": `{"command":"echo cadence-probe","description":"echo"}`},
		}}}, nil)
		write(map[string]any{}, "tool_calls")
	default:
		streamText("after", cadenceSecondPartDeltas)
		write(map[string]any{}, "stop")
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// startServerWithFakeProvider spawns the real `opencode serve` exactly
// like startServer (helpers_test.go), with every XDG directory isolated,
// and a project opencode.json in its working directory that makes the
// fake provider above its default model.
func startServerWithFakeProvider(t *testing.T, providerURL string) string {
	t.Helper()

	work := t.TempDir()
	cfg := map[string]any{
		"provider": map[string]any{"fakeprov": map[string]any{
			"npm":     "@ai-sdk/openai-compatible", // the runtime's bundled chat-completions provider package id
			"name":    "Fake provider",
			"options": map[string]any{"baseURL": providerURL + "/v1", "apiKey": "not-a-credential"},
			"models":  map[string]any{"fake-model": map[string]any{"name": "Fake model"}},
		}},
		"model":       "fakeprov/fake-model",
		"small_model": "fakeprov/fake-model",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal opencode.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "opencode.json"), raw, 0o600); err != nil {
		t.Fatalf("write opencode.json: %v", err)
	}

	sup := supervisor.New()
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), testReadinessTimeout)
		defer stopCancel()
		_ = sup.StopAll(stopCtx, testReadinessPollInterval)
	})
	home := t.TempDir()
	env := []string{
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), testReadinessTimeout)
	defer cancel()
	result, err := opencodeproc.Spawn(ctx, sup, work, env, nil, nil, testReadinessTimeout, testReadinessPollInterval)
	if err != nil {
		t.Fatalf("opencodeproc.Spawn() error = %v (is the real opencode binary on PATH?)", err)
	}
	return result.BaseURL
}

// finalTextParts reads every text part's final text back from GET
// /session/{id}/message, keyed by part id.
func finalTextParts(ctx context.Context, t *testing.T, a *Adapter, conversationID string) map[string]string {
	t.Helper()
	entries, err := a.fetchFinalMessages(ctx, conversationID)
	if err != nil {
		t.Fatalf("GET /session/%s/message: %v", conversationID, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		for _, raw := range e.Parts {
			var p struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("decode part: %v", err)
			}
			if p.Type == "text" {
				out[p.ID] = p.Text
			}
		}
	}
	return out
}

// tokenFramesByPart groups a turn's `token` frames by part id (the wire
// messageId), in emission order, with the order parts first appeared.
func tokenFramesByPart(events []ports.AgentEvent) (frames map[string][]string, order []string) {
	frames = map[string][]string{}
	for _, e := range events {
		tok, ok := e.Payload.(sandboxws.Token)
		if !ok {
			continue
		}
		if _, seen := frames[tok.MessageId]; !seen {
			order = append(order, tok.MessageId)
		}
		frames[tok.MessageId] = append(frames[tok.MessageId], tok.Text)
	}
	return frames, order
}

// TestTokenCadence_RealBinary_BoundedFramesPerTextPart pins the pinned
// runtime's `token` cadence against the real binary with no credential: a
// local chat-completions-compatible fake streams two long text parts (one before a
// real bash tool call, one after), and the test asserts, per text part,
// that the adapter sent at most maxTokenFramesPerTextPart frames and that
// the LAST one is exactly the part's final text as GET
// /session/{id}/message reports it -- the frame every reader of the event
// log shows.
func TestTokenCadence_RealBinary_BoundedFramesPerTextPart(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(fakeChatCompletionsHandler))
	t.Cleanup(provider.Close)

	a := New(startServerWithFakeProvider(t, provider.URL), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	collector := &eventCollector{}
	convID, err := a.StartTurn(ctx, sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: testSessionID, Gen: 1, Text: "Probe the token cadence.",
	}, collector.sink, nil)
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	events := collector.snapshot()
	if final, ok := events[len(events)-1].Payload.(sandboxws.ExecutionComplete); !ok || final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		t.Fatalf("last event = %+v, want execution_complete{completed}", events[len(events)-1].Payload)
	}

	finals := finalTextParts(ctx, t, a, convID)
	frames, order := tokenFramesByPart(events)
	if len(order) != 2 {
		t.Fatalf("token frames arrived for %d parts %v, want 2 (the text before the tool call and the text after it)", len(order), order)
	}
	wantPrefixes := []string{"w000 ", "after000 "}
	for i, part := range order {
		got := frames[part]
		last := got[len(got)-1]
		t.Logf("text part %s: %d frames, final %d bytes", part, len(got), len(last))
		if len(got) > maxTokenFramesPerTextPart {
			t.Errorf("text part %s: %d token frames, want at most %d -- the runtime's cadence grew; coalesce in the adapter", part, len(got), maxTokenFramesPerTextPart)
		}
		want, ok := finals[part]
		if !ok {
			t.Errorf("text part %s: absent from GET /session/%s/message", part, convID)
			continue
		}
		if last != want {
			t.Errorf("text part %s: last frame = %q, want the part's final text %q", part, last, want)
		}
		if !strings.HasPrefix(want, wantPrefixes[i]) {
			t.Errorf("text part %s: final text %q, want it to start with %q (the fake provider's own stream)", part, want, wantPrefixes[i])
		}
	}
}

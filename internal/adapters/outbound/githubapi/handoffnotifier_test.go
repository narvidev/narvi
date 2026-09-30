package githubapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/handoff"
	"github.com/narvidev/narvi/internal/platform"
)

// TestHandoffNotifier_Deliver_AddsLabelAndPostsComment proves ONE Deliver
// call does both halves of a handoff-sentinel run: adds the fixed
// "handoff" label, THEN posts the already-rendered summary as a plain
// issue comment -- label first, since it's the idempotent half; see
// Deliver's own doc comment for why that ordering matters for retries.
func TestHandoffNotifier_Deliver_AddsLabelAndPostsComment(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var commentBody map[string]any
	var addLabelsBody map[string]any
	var order []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/issues/42/comments":
			order = append(order, "comment")
			_ = json.NewDecoder(r.Body).Decode(&commentBody)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/issues/42/labels":
			order = append(order, "label")
			_ = json.NewDecoder(r.Body).Decode(&addLabelsBody)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewHandoffNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.HandoffPayload{
		Owner:    "acme",
		Repo:     "widgets",
		PRNumber: 42,
		Body:     "### Handoff readiness\n\nsomething to report.",
		Label:    handoff.Label,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindHandoffSentinel,
		Payload: payload,
	}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if commentBody["body"] != "### Handoff readiness\n\nsomething to report." {
		t.Errorf("comment body = %v, want the rendered summary", commentBody["body"])
	}

	addedLabels, _ := addLabelsBody["labels"].([]any)
	if len(addedLabels) != 1 || addedLabels[0] != handoff.Label {
		t.Errorf("added labels = %v, want [%q]", addLabelsBody["labels"], handoff.Label)
	}

	if len(order) != 2 || order[0] != "label" || order[1] != "comment" {
		t.Errorf("call order = %v, want [label, comment] (label synced before the comment is posted)", order)
	}
}

// TestHandoffNotifier_Deliver_LabelFailure_NeverPostsComment proves the
// ordering contract: if adding the label itself fails, the comment is
// never posted at all -- no partial "commented but not labeled" state,
// and critically, no risk of a duplicate comment on the next retry (the
// whole point of putting the non-idempotent operation last).
func TestHandoffNotifier_Deliver_LabelFailure_NeverPostsComment(t *testing.T) {
	t.Parallel()

	var commentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/issues/42/labels" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
			return
		}
		commentCalls++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewHandoffNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.HandoffPayload{
		Owner: "acme", Repo: "widgets", PRNumber: 42, Body: "text", Label: handoff.Label,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	err = notifier.Deliver(context.Background(), ports.Notification{Kind: ports.NotificationKindHandoffSentinel, Payload: payload})
	if err == nil {
		t.Fatal("Deliver() error = nil, want a non-nil error when adding the label itself fails")
	}
	if commentCalls != 0 {
		t.Errorf("commentCalls = %d, want 0 (comment must never post after a failed label add)", commentCalls)
	}
}

// TestHandoffNotifier_Deliver_RetryAfterCommentFailure_NeverDuplicatesComment
// proves the exact scenario the label-first/comment-last ordering exists
// for: a transient failure on the (now-last) comment call after the label
// already succeeded must not cause a retried Deliver to post twice --
// only the never-yet-succeeded comment is attempted again.
func TestHandoffNotifier_Deliver_RetryAfterCommentFailure_NeverDuplicatesComment(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var labelCalls, commentCalls int
	failCommentOnce := true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/repos/acme/widgets/issues/42/labels":
			labelCalls++
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/widgets/issues/42/comments":
			commentCalls++
			if failCommentOnce {
				failCommentOnce = false
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "transient"})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewHandoffNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.HandoffPayload{
		Owner: "acme", Repo: "widgets", PRNumber: 42, Body: "text", Label: handoff.Label,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	notification := ports.Notification{Kind: ports.NotificationKindHandoffSentinel, Payload: payload}

	if err := notifier.Deliver(context.Background(), notification); err == nil {
		t.Fatal("first Deliver() error = nil, want a transient comment-post error")
	}
	if err := notifier.Deliver(context.Background(), notification); err != nil {
		t.Fatalf("second Deliver() error = %v, want nil (comment should succeed on retry)", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if commentCalls != 2 {
		t.Errorf("commentCalls = %d, want 2 (one failed attempt, one successful retry -- never a THIRD, duplicate post)", commentCalls)
	}
	if labelCalls != 2 {
		t.Errorf("labelCalls = %d, want 2 (safe no-op re-run on each Deliver call)", labelCalls)
	}
}

// TestHandoffNotifier_Deliver_InvalidPayload proves a malformed outbox
// payload is a decode error, never a panic.
func TestHandoffNotifier_Deliver_InvalidPayload(t *testing.T) {
	t.Parallel()

	adapter := githubapi.New(http.DefaultClient, "http://unused.invalid")
	notifier := mustBuild(githubapi.NewHandoffNotifier(adapter, platform.MustNewGitHubOutboundConfig("bot-token")))

	err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindHandoffSentinel,
		Payload: []byte(`not json`),
	})
	if err == nil {
		t.Fatal("Deliver() error = nil, want a non-nil decode error")
	}
}

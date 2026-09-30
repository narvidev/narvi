package githubapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// TestPostIssueComment_Success proves PostIssueComment posts the right
// shape to /repos/{owner}/{repo}/issues/{pr_number}/comments.
func TestPostIssueComment_Success(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "html_url": "https://github.com/acme/widgets/pull/42#issuecomment-1"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	err := adapter.PostIssueComment(context.Background(), "acme", "widgets", 42, "bot-token", "Turn completed.")
	if err != nil {
		t.Fatalf("PostIssueComment() error = %v, want nil", err)
	}

	if gotPath != "/repos/acme/widgets/issues/42/comments" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/issues/42/comments")
	}
	if gotAuth != "Bearer bot-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer bot-token")
	}
	if gotBody["body"] != "Turn completed." {
		t.Errorf("body = %v, want %q", gotBody["body"], "Turn completed.")
	}
}

// TestPostIssueComment_HTTPError proves a non-2xx GitHub response surfaces
// a real error via doPost's shared error-envelope parsing.
func TestPostIssueComment_HTTPError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	err := adapter.PostIssueComment(context.Background(), "acme", "widgets", 42, "bot-token", "Turn completed.")
	if err == nil {
		t.Fatal("PostIssueComment() error = nil, want non-nil")
	}
}

// TestBotNotifier_Deliver proves BotNotifier.Deliver decodes a
// githubapi.Payload and routes it through PostIssueComment with the bot
// token baked into NewBotNotifier, satisfying ports.Notifier.
func TestBotNotifier_Deliver(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewBotNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.Payload{
		Owner:    "acme",
		Repo:     "widgets",
		PRNumber: 7,
		Text:     "PR opened: https://github.com/acme/widgets/pull/7",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHub,
		Payload: payload,
	}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	if gotPath != "/repos/acme/widgets/issues/7/comments" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/issues/7/comments")
	}
	if gotAuth != "Bearer baked-in-bot-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer baked-in-bot-token")
	}
}

// TestBotNotifier_Deliver_InvalidPayload proves a malformed payload is
// reported as an error, never silently ignored.
func TestBotNotifier_Deliver_InvalidPayload(t *testing.T) {
	t.Parallel()

	adapter := githubapi.New(http.DefaultClient, "http://unused.invalid")
	notifier := mustBuild(githubapi.NewBotNotifier(adapter, platform.MustNewGitHubOutboundConfig("bot-token")))

	err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHub,
		Payload: []byte("not json"),
	})
	if err == nil {
		t.Fatal("Deliver() error = nil, want non-nil for invalid payload")
	}
}

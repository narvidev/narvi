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

// TestPreviewLinkNotifier_Deliver proves Deliver decodes a
// githubapi.PreviewLinkPayload and posts it as a narvi/preview commit
// status via the bot token baked into NewPreviewLinkNotifier, satisfying
// ports.Notifier — mirrors TestReleaseManifestNotifier_Deliver_PostsPlainComment's
// own precedent.
func TestPreviewLinkNotifier_Deliver(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewPreviewLinkNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.PreviewLinkPayload{
		Owner:       "acme",
		Repo:        "widgets",
		SHA:         "abc123sha",
		TargetURL:   "https://myapp-pr-7--acme.rwx.run",
		Description: "Preview deployed via RWX (ephemeral).",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	err = notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHubPreviewLink,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}

	if gotPath != "/repos/acme/widgets/statuses/abc123sha" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/statuses/abc123sha")
	}
	if gotAuth != "Bearer baked-in-bot-token" {
		t.Errorf("Authorization header = %q, want the baked-in bot token", gotAuth)
	}
	if gotBody["state"] != "success" {
		t.Errorf("posted state = %v, want %q", gotBody["state"], "success")
	}
	if gotBody["context"] != "narvi/preview" {
		t.Errorf("posted context = %v, want %q", gotBody["context"], "narvi/preview")
	}
	if gotBody["target_url"] != "https://myapp-pr-7--acme.rwx.run" {
		t.Errorf("posted target_url = %v, want the friendly preview URL", gotBody["target_url"])
	}
}

// TestPreviewLinkNotifier_Deliver_MalformedPayload proves a malformed
// payload is a real, returned error, never a panic or silent no-op.
func TestPreviewLinkNotifier_Deliver_MalformedPayload(t *testing.T) {
	t.Parallel()

	adapter := githubapi.New(nil, "https://example.invalid")
	notifier := mustBuild(githubapi.NewPreviewLinkNotifier(adapter, platform.MustNewGitHubOutboundConfig("tok")))

	err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHubPreviewLink,
		Payload: []byte("not json"),
	})
	if err == nil {
		t.Fatal("Deliver() error = nil, want an error for a malformed payload")
	}
}

// TestPreviewLinkNotifier_Deliver_HTTPFailurePropagates proves a GitHub
// API failure surfaces as a real Deliver error.
func TestPreviewLinkNotifier_Deliver_HTTPFailurePropagates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewPreviewLinkNotifier(adapter, platform.MustNewGitHubOutboundConfig("tok")))

	payload, _ := json.Marshal(githubapi.PreviewLinkPayload{Owner: "acme", Repo: "widgets", SHA: "sha1"})
	err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHubPreviewLink,
		Payload: payload,
	})
	if err == nil {
		t.Fatal("Deliver() error = nil, want the GitHub API's own 403 to propagate")
	}
}

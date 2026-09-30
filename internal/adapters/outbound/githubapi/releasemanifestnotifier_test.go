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

// TestReleaseManifestNotifier_Deliver_PostsPlainComment proves Deliver
// posts the manifest check's already-rendered body as a plain issue
// comment -- NEVER a formal review (ports.NotificationKindReleaseManifest's
// own doc comment: this check has no RiskLevel/Shippable of its own to
// gate a formal-review event on).
func TestReleaseManifestNotifier_Deliver_PostsPlainComment(t *testing.T) {
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
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewReleaseManifestNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.ReleaseManifestPayload{
		Owner:    "acme",
		Repo:     "widgets",
		PRNumber: 77,
		Body:     "### Release manifest check\n\nNo compliance issues found.",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	err = notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindReleaseManifest,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}

	if gotPath != "/repos/acme/widgets/issues/77/comments" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/issues/77/comments")
	}
	if gotAuth != "Bearer baked-in-bot-token" {
		t.Errorf("Authorization header = %q, want the baked-in bot token", gotAuth)
	}
	if gotBody["body"] != "### Release manifest check\n\nNo compliance issues found." {
		t.Errorf("posted comment body = %v, want the exact rendered text", gotBody["body"])
	}
}

// TestReleaseManifestNotifier_Deliver_MalformedPayload proves a
// malformed payload is a real, returned error, never a panic or silent
// no-op.
func TestReleaseManifestNotifier_Deliver_MalformedPayload(t *testing.T) {
	t.Parallel()

	adapter := githubapi.New(nil, "https://example.invalid")
	notifier := mustBuild(githubapi.NewReleaseManifestNotifier(adapter, platform.MustNewGitHubOutboundConfig("tok")))

	err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindReleaseManifest,
		Payload: []byte("not json"),
	})
	if err == nil {
		t.Fatal("Deliver() error = nil, want an error for a malformed payload")
	}
}

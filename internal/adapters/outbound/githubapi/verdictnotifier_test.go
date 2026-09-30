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
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

type capturedRequest struct {
	method string
	path   string
}

// TestVerdictNotifier_Deliver_SubmitsReviewAndSyncsLabels proves ONE
// Deliver call does both halves of posting a verdict: submits the formal
// review with the given event/body, THEN fetches the PR's own current
// labels and applies internal/domain/reviewpost.ComputeLabelSync's own
// plan against them -- covering the explicit test-coverage item this
// Step's own brief names ("label sync including the inverted needs-human
// semantics": the fake server's own current-labels response below
// includes reviewpost.LabelNeedsHuman, proving Deliver never removes it).
func TestVerdictNotifier_Deliver_SubmitsReviewAndSyncsLabels(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requests []capturedRequest
	var reviewBody map[string]any
	var addLabelsBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, capturedRequest{method: r.Method, path: r.URL.Path})

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/pulls/42/reviews":
			_ = json.NewDecoder(r.Body).Decode(&reviewBody)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/42/labels":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": reviewpost.LabelLowRisk},
				{"name": reviewpost.LabelMediumRisk},
				{"name": reviewpost.LabelNeedsHuman},
				{"name": "bug"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/issues/42/labels":
			_ = json.NewDecoder(r.Body).Decode(&addLabelsBody)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewVerdictNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.VerdictPayload{
		Owner:     "acme",
		Repo:      "widgets",
		PRNumber:  42,
		Event:     string(reviewpost.FormalReviewEventRequestChanges),
		Body:      "### Code review verdict\n\nblocked.",
		RiskLevel: string(review.RiskLevelHigh),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHubVerdict,
		Payload: payload,
	}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if reviewBody["event"] != string(reviewpost.FormalReviewEventRequestChanges) {
		t.Errorf("review event = %v, want %q", reviewBody["event"], reviewpost.FormalReviewEventRequestChanges)
	}

	addedLabels, _ := addLabelsBody["labels"].([]any)
	if len(addedLabels) != 1 || addedLabels[0] != reviewpost.LabelHighRisk {
		t.Errorf("added labels = %v, want [%q]", addLabelsBody["labels"], reviewpost.LabelHighRisk)
	}

	var deleted []string
	for _, r := range requests {
		if r.method == http.MethodDelete {
			deleted = append(deleted, r.path)
		}
	}
	wantDeletedLow := "/repos/acme/widgets/issues/42/labels/" + reviewpost.LabelLowRisk
	wantDeletedMedium := "/repos/acme/widgets/issues/42/labels/" + reviewpost.LabelMediumRisk
	wantDeletedNeedsHuman := "/repos/acme/widgets/issues/42/labels/" + reviewpost.LabelNeedsHuman

	foundLow, foundMedium := false, false
	for _, d := range deleted {
		if d == wantDeletedLow {
			foundLow = true
		}
		if d == wantDeletedMedium {
			foundMedium = true
		}
		if d == wantDeletedNeedsHuman {
			t.Errorf("DELETE issued against %q -- the needs-human escape hatch must NEVER be removed by label sync", reviewpost.LabelNeedsHuman)
		}
	}
	if !foundLow || !foundMedium {
		t.Errorf("deleted labels = %v, want both %q and %q removed", deleted, reviewpost.LabelLowRisk, reviewpost.LabelMediumRisk)
	}
}

// TestVerdictNotifier_Deliver_ReviewFailure_NeverSyncsLabels proves the
// ordering contract: if the formal review submission itself fails, label
// sync (ListLabels/AddLabels/RemoveLabel) never runs at all -- no partial
// "labels synced but no review posted" state.
func TestVerdictNotifier_Deliver_ReviewFailure_NeverSyncsLabels(t *testing.T) {
	t.Parallel()

	var labelCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/pulls/42/reviews" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
			return
		}
		labelCalls++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewVerdictNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.VerdictPayload{
		Owner:     "acme",
		Repo:      "widgets",
		PRNumber:  42,
		Event:     string(reviewpost.FormalReviewEventComment),
		Body:      "text",
		RiskLevel: string(review.RiskLevelLow),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	err = notifier.Deliver(context.Background(), ports.Notification{Kind: ports.NotificationKindGitHubVerdict, Payload: payload})
	if err == nil {
		t.Fatal("Deliver() error = nil, want a non-nil error when the review submission itself fails")
	}
	if labelCalls != 0 {
		t.Errorf("labelCalls = %d, want 0 (label sync must never run after a failed review submission)", labelCalls)
	}
}

// TestVerdictNotifier_Deliver_EmptyRiskLevel_SkipsLabelSync is rereview
// fix (§24 finding 6)'s own regression test: an empty payload.
// RiskLevel means no real verdict was ever posted for this PR at all
// (e.g. sessionactor's own §24.6 budget-exhausted notice, when every
// automatic re-review for a PR declined before ever posting one) --
// before this fix, Deliver ran label sync unconditionally, and
// reviewpost.RiskLabel's own fail-conservative default rendered an
// empty/unrecognized RiskLevel as review:high-risk, stamping a "high
// risk" label on a PR this notification never actually assessed. Proves
// the formal review is still submitted (this notification's own real
// content), but ListLabels/AddLabels/RemoveLabel are never called at all.
func TestVerdictNotifier_Deliver_EmptyRiskLevel_SkipsLabelSync(t *testing.T) {
	t.Parallel()

	var reviewSubmitted bool
	var labelCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/pulls/42/reviews" {
			reviewSubmitted = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}
		labelCalls++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	notifier := mustBuild(githubapi.NewVerdictNotifier(adapter, platform.MustNewGitHubOutboundConfig("baked-in-bot-token")))

	payload, err := json.Marshal(githubapi.VerdictPayload{
		Owner:    "acme",
		Repo:     "widgets",
		PRNumber: 42,
		Event:    string(reviewpost.FormalReviewEventComment),
		Body:     "Automatic re-review has reached its budget for this pull request.",
		// RiskLevel deliberately omitted -- "" (no verdict was ever
		// posted for this PR).
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHubVerdict,
		Payload: payload,
	}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	if !reviewSubmitted {
		t.Error("formal review was never submitted -- want it submitted regardless of label sync being skipped")
	}
	if labelCalls != 0 {
		t.Errorf("labelCalls = %d, want 0 (an empty RiskLevel must skip label sync entirely, never stamp a fail-conservative review:high-risk label)", labelCalls)
	}
}

// TestVerdictNotifier_Deliver_InvalidPayload proves a malformed outbox
// payload is a decode error, never a panic.
func TestVerdictNotifier_Deliver_InvalidPayload(t *testing.T) {
	t.Parallel()

	adapter := githubapi.New(http.DefaultClient, "http://unused.invalid")
	notifier := mustBuild(githubapi.NewVerdictNotifier(adapter, platform.MustNewGitHubOutboundConfig("bot-token")))

	err := notifier.Deliver(context.Background(), ports.Notification{
		Kind:    ports.NotificationKindGitHubVerdict,
		Payload: []byte(`not json`),
	})
	if err == nil {
		t.Fatal("Deliver() error = nil, want a non-nil decode error")
	}
}

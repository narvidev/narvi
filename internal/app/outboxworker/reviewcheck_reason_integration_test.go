//go:build integration

package outboxworker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
)

// TestReviewCheckNotifier_NotAssessedReason_NamedOnTheCheck delivers a
// not-assessed emission through the full publisher, with and without the
// reason the control plane refused the attempt for: the check a person
// reads on the pull request says why when the reason is named, and says
// exactly what it always said when none is (or when it is one this binary
// does not know).
func TestReviewCheckNotifier_NotAssessedReason_NamedOnTheCheck(t *testing.T) {
	plain := reviewcheck.ComputeOutput(reviewcheck.PhaseTerminalNotAssessed).Summary
	tests := []struct {
		name        string
		reason      string
		wantSummary func(string) bool
	}{
		{"personal link only", string(reviewcheck.NotAssessedPersonalLinkOnly), func(s string) bool {
			return strings.HasPrefix(s, plain) && strings.Contains(s, "person's own provider link") && strings.Contains(s, "deployment credential")
		}},
		{"no reason", "", func(s string) bool { return s == plain }},
		{"a reason this binary does not know", "a_future_reason", func(s string) bool { return s == plain }},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			fake := newFakeCheckRunGitHub()
			server := fake.server()
			defer server.Close()
			notifier := outboxworker.NewReviewCheckNotifier(pool, narvipg.NewReviewCheckRunStore(pool), githubapi.New(server.Client(), server.URL), "tok")

			payload, err := json.Marshal(ports.ReviewCheckPayload{
				Owner: "acme", Repo: fmt.Sprintf("reason-repo-%d-%d", i, time.Now().UnixNano()), PRNumber: 7, HeadSHA: "feedface",
				AttemptID: newTestAttempt(ctx, t, pool), AttemptCreatedAt: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC),
				Phase: string(reviewcheck.PhaseTerminalNotAssessed), NotAssessedReason: tc.reason,
			})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubReviewCheck, Payload: payload}); err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			if creates, _ := fake.counts(); creates != 1 {
				t.Fatalf("check runs created = %d, want 1", creates)
			}
			state := fake.stateFor(1)
			if state["conclusion"] != string(reviewcheck.ConclusionActionRequired) {
				t.Errorf("conclusion = %v, want %q", state["conclusion"], reviewcheck.ConclusionActionRequired)
			}
			summary, _ := state["summary"].(string)
			if !tc.wantSummary(summary) {
				t.Errorf("summary = %q, not what this case expects (plain summary %q)", summary, plain)
			}
		})
	}
}

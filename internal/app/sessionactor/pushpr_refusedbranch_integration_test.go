//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestCompleteProcessingTurn_StoredBranchThePushRuleRefuses_NoPushSent is
// the control plane's half of technical plan §35.3's "never forced". A
// session's stored branch was validated when the session was created, but
// sessions.repos is never rewritten, so one created before the push rule
// tightened can still name "+main" -- and a sandbox restored from an older
// snapshot runs the older agent, which would push it as a forced refspec.
// So the turn completes, no push command is sent, and exactly one
// session-visible warning names the repo, the branch and git's reason,
// even when the execution_complete is redelivered. No delivery is stamped.
// In shadow it holds too, and no suppressed-push row is written: live
// would not have pushed either.
func TestCompleteProcessingTurn_StoredBranchThePushRuleRefuses_NoPushSent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	tests := []struct {
		name       string
		branch     string
		live       bool
		wantReason string
	}{
		{name: "a leading plus, live", branch: "+main", live: true, wantReason: `which git reads as a forced push`},
		{name: "a colon, live", branch: ":main", live: true, wantReason: `it contains ":"`},
		{name: "a leading plus, shadow", branch: "+main", live: false, wantReason: `which git reads as a forced push`},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoFullName := "acme/refused-branch-" + string(rune('a'+i))
			sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/"+repoFullName+".git", tc.branch)
			if tc.live {
				if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
					t.Fatalf("promote repo to live egress: %v", err)
				}
			}
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			turnStore := narvipg.NewTurnStore(pool)
			processing := createProcessingTurn(ctx, t, turnStore, sessionID)

			commander := &fakeSendCommander{}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			raw := executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted)
			sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "execution_complete", Gen: 1, Raw: raw})
			waitUntil(t, 5*time.Second, func() bool {
				row, err := turnStore.Get(ctx, processing.ID)
				return err == nil && row.Status == sqlcgen.TurnStatusCompleted
			})
			// A wire redelivery of the same event records nothing more.
			sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "execution_complete", Gen: 1, Raw: raw})
			// The assertions below are absences, or a count that must not
			// grow: give any post-commit send a moment to have run.
			time.Sleep(300 * time.Millisecond)

			if got := commander.callCount(); got != 0 {
				t.Errorf("SandboxCommander.SendCommand called %d times, want 0: a branch the push rule refuses is never sent to a sandbox", got)
			}

			events, err := narvipg.NewEventStore(pool).ListForSession(ctx, sessionID, 0, 100)
			if err != nil {
				t.Fatalf("ListForSession: %v", err)
			}
			var warnings []string
			for _, e := range events {
				if e.Type != "warning" {
					continue
				}
				var w sandboxws.Warning
				if err := json.Unmarshal(e.Payload, &w); err != nil {
					t.Fatalf("unmarshal warning: %v", err)
				}
				warnings = append(warnings, w.Message)
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %q, want exactly one", warnings)
			}
			for _, want := range []string{"push was not sent", `repo "repo"`, `branch "` + tc.branch + `"`, tc.wantReason} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning %q does not name %q", warnings[0], want)
				}
			}

			var delivering bool
			if err := pool.QueryRow(ctx, `SELECT pr_delivery_started_at IS NOT NULL FROM sandboxes WHERE session_id = $1`, sessionID).Scan(&delivering); err != nil {
				t.Fatalf("read pr_delivery_started_at: %v", err)
			}
			if delivering {
				t.Error("a push/PR delivery is stamped for a push that was never sent")
			}
			if rows, err := narvipg.NewShadowSCMWriteStore(pool).ListForRepo(ctx, repoFullName, 10); err != nil || len(rows) != 0 {
				t.Errorf("suppressed-push rows = %d (%v), want 0: nothing would have been pushed in live either", len(rows), err)
			}
		})
	}
}

//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// A pull request's review session is read-only: when one of its turns
// completes, the session actor never asks its sandbox to push, whatever the
// repository's egress mode and whatever the creator's own GitHub identity.
// completeProcessingTurn is the one place that decision is taken, so these
// tests drive it end to end on real Postgres -- a real execution_complete
// through Actor.Send -- and read what reaches the sandbox commander and what
// the completing transaction wrote.

// reviewPushSessionBranch is the review session's repos[].branch: the pull
// request's head ref, exactly as GitHub ingress configures it.
const reviewPushSessionBranch = "feature-x"

// pushCommandsSent returns every command c received whose type is "push".
func pushCommandsSent(t *testing.T, c *fakeSendCommander) []sandboxws.Push {
	t.Helper()
	c.mu.Lock()
	payloads := append([]json.RawMessage(nil), c.payloads...)
	c.mu.Unlock()
	var out []sandboxws.Push
	for _, raw := range payloads {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil || probe.Type != "push" {
			continue
		}
		var p sandboxws.Push
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode push command: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// completeTurnAndDrain sends one execution_complete (outcome completed) to
// a, then sends the same event again. The actor handles one command at a
// time, and the push command for the first delivery is sent after its
// reply, still inside that delivery's handling -- so once the redelivery's
// reply arrives, anything the first delivery was going to send has been
// sent. The redelivery itself completes nothing: its message id is already
// persisted and no turn is processing any more.
func completeTurnAndDrain(ctx context.Context, t *testing.T, a *Actor, sessionID pgtype.UUID) {
	t.Helper()
	messageID := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type:      "execution_complete",
		MessageId: messageID,
		SessionId: sessionID.String(),
		Gen:       1,
		AckId:     "execution_complete:" + messageID,
		Outcome:   sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatalf("marshal execution_complete: %v", err)
	}
	for i := 0; i < 2; i++ {
		outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: messageID, Raw: raw})
		if !outcome.Persisted {
			t.Fatalf("execution_complete delivery %d was not persisted", i+1)
		}
	}
}

// createReviewPushFixtureCreator creates a member and, when linked, a
// GitHub identity carrying a stored (encrypted) OAuth token -- a creator
// whose own push would authenticate.
func createReviewPushFixtureCreator(ctx context.Context, t *testing.T, pool *pgxpool.Pool, linked bool) pgtype.UUID {
	t.Helper()
	suffix := uuid.NewString()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "review-push-" + suffix + "@example.com",
		DisplayName:  "Review Push Creator",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !linked {
		return user.ID
	}
	encrypted, err := platform.EncryptToken(testTokenEncryptionKey, []byte("gho_creatorsOwnToken"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := "review-push-" + suffix + "@example.com"
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           "review-push-" + suffix,
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create github identity: %v", err)
	}
	return user.ID
}

// TestCompleteProcessingTurn_ReviewSessionNeverPushes is the regression
// test for "a review session is read-only": a completed turn of a session
// with a github_pr_sessions row sends no push command and records no push
// cycle at all -- no delivery stamp, no egress decision, no suppressed-push
// ledger row, no push-blocked warning. The control row is the same fixture
// without the github_pr_sessions row, and must push: it proves this harness
// observes a push when one is sent, so the review rows' "none" is not an
// artefact of the harness.
func TestCompleteProcessingTurn_ReviewSessionNeverPushes(t *testing.T) {
	tests := []struct {
		name string
		// review: the session has a github_pr_sessions row.
		review bool
		// live: the repository is promoted to live egress; otherwise it
		// stays shadow, repo_settings' own default.
		live bool
		// linked: the creator has a linked GitHub identity with a token.
		linked   bool
		wantPush bool
	}{
		{name: "control: not a review session, live repository, linked creator", review: false, live: true, linked: true, wantPush: true},
		{name: "review session, live repository, linked creator", review: true, live: true, linked: true},
		{name: "review session, live repository, creator with no GitHub identity", review: true, live: true, linked: false},
		{name: "review session, shadow repository, linked creator", review: true, live: false, linked: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)

			repoFullName := "acme/review-push-" + uuid.NewString()[:8]
			creator := createReviewPushFixtureCreator(ctx, t, pool, tc.linked)
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
				CreatedBy:   creator,
				Repos:       reposJSONForTest(t, "repo", "https://github.com/"+repoFullName+".git", reviewPushSessionBranch),
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if tc.review {
				prSessions := narvipg.NewGitHubPRSessionStore(pool)
				prNumber := int32(100 + i)
				if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
					t.Fatalf("ensure github_pr_sessions row: %v", err)
				}
				if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
					t.Fatalf("set github_pr_sessions session id: %v", err)
				}
			}
			if tc.live {
				if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
					t.Fatalf("promote repo to live egress: %v", err)
				}
			}
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, session.ID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			turnStore := narvipg.NewTurnStore(pool)
			processing := createProcessingTurn(ctx, t, turnStore, session.ID)

			commander := &fakeSendCommander{}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, testTokenEncryptionKey, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, session.ID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			completeTurnAndDrain(ctx, t, a, session.ID)

			got, err := turnStore.Get(ctx, processing.ID)
			if err != nil {
				t.Fatalf("get turn: %v", err)
			}
			if got.Status != sqlcgen.TurnStatusCompleted {
				t.Fatalf("turn status = %s, want %s: the turn itself completes either way", got.Status, sqlcgen.TurnStatusCompleted)
			}

			pushes := pushCommandsSent(t, commander)
			if tc.wantPush {
				if len(pushes) != 1 || len(pushes[0].Repos) != 1 || pushes[0].Repos[0].Branch != reviewPushSessionBranch {
					t.Fatalf("push commands = %+v, want exactly one push of %q", pushes, reviewPushSessionBranch)
				}
				return
			}
			if len(pushes) != 0 {
				t.Fatalf("push commands = %+v, want none: a review session never pushes", pushes)
			}

			var stamped, decided bool
			if err := pool.QueryRow(ctx,
				`SELECT pr_delivery_started_at IS NOT NULL, pending_push_suppressed_in_shadow IS NOT NULL FROM sandboxes WHERE session_id = $1`,
				session.ID,
			).Scan(&stamped, &decided); err != nil {
				t.Fatalf("read sandbox push state: %v", err)
			}
			if stamped {
				t.Error("pr_delivery_started_at is set: a delivery was started for a session that never pushes")
			}
			if decided {
				t.Error("pending_push_suppressed_in_shadow is set: a push cycle was recorded for a session that never pushes")
			}

			var suppressedPushes int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM shadow_scm_writes WHERE session_id = $1 AND operation = 'push'`,
				session.ID,
			).Scan(&suppressedPushes); err != nil {
				t.Fatalf("count suppressed-push ledger rows: %v", err)
			}
			if suppressedPushes != 0 {
				t.Errorf("%d suppressed-push ledger rows, want 0: nothing was suppressed, there was never a push to send", suppressedPushes)
			}

			events, err := narvipg.NewEventStore(pool).ListForSession(ctx, session.ID, 0, 100)
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			for _, e := range events {
				if e.Type == "warning" {
					t.Errorf("a warning event was recorded (%s): no push was ever going to be attempted, so none was blocked", e.Payload)
				}
			}
		})
	}
}

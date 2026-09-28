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
	"github.com/narvidev/narvi/internal/app/ports"
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

// deliverPushCompleteAndDrain delivers one push_complete reporting repoName
// pushed on branch at sha, then delivers the same event again. The pull
// request and its preview are created after the first delivery's reply,
// still inside that delivery's handling, and the actor handles one command
// at a time -- so once the redelivery's reply arrives, whatever the first
// delivery was going to open has been opened. The redelivery itself opens
// nothing: its message id is already persisted.
func deliverPushCompleteAndDrain(ctx context.Context, t *testing.T, a *Actor, sessionID pgtype.UUID, repoName, branch, sha string) {
	t.Helper()
	messageID := uuid.NewString()
	raw := pushCompleteRawWithMessageID(t, messageID, sessionID.String(), 1, repoName, branch, sha)
	for i := 0; i < 2; i++ {
		outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "push_complete", Gen: 1, MessageID: messageID, Raw: raw})
		if !outcome.Persisted {
			t.Fatalf("push_complete delivery %d was not persisted", i+1)
		}
	}
}

// TestCreatePRBestEffort_ReviewSessionNeverOpensAPullRequest: a pull
// request's review session never opens a pull request. A push_complete
// delivered to one opens no pull request (neither created nor suppressed),
// records no artifact, and enqueues no preview -- no rwx_preview_dispatch
// and no github_preview_link, the commit status a preview posts. The
// repository is live, its preview is configured and the creator has a
// usable GitHub token: every condition under which any other session opens
// both. The control row is the same fixture without the github_pr_sessions
// row, and must open one pull request and enqueue its preview: it proves
// this harness observes both when they happen. The last row fails closed:
// when the review-session lookup itself fails, nothing is opened either,
// although the session has no github_pr_sessions row at all.
func TestCreatePRBestEffort_ReviewSessionNeverOpensAPullRequest(t *testing.T) {
	const pushedSHA = "cafef00d00000000000000000000000000000000"
	tests := []struct {
		name string
		// review: the session has a github_pr_sessions row.
		review bool
		// lookupFails: the github_pr_sessions table is unreadable while
		// the push_complete is handled.
		lookupFails bool
		wantOpened  bool
	}{
		{name: "control: not a review session", wantOpened: true},
		{name: "review session", review: true},
		{name: "the review-session lookup fails: fail closed", lookupFails: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)

			user := setUpPreviewTestUserAndIdentity(ctx, t, pool, "review-pr-creator")
			repoFullName := "review-pr-acme/repo-" + uuid.NewString()[:8]
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
				CreatedBy:   user.ID,
				Repos:       reposJSONForTest(t, "repo", "https://github.com/"+repoFullName+".git", reviewPushSessionBranch),
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if tc.review {
				prSessions := narvipg.NewGitHubPRSessionStore(pool)
				prNumber := int32(200 + i)
				if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
					t.Fatalf("ensure github_pr_sessions row: %v", err)
				}
				if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
					t.Fatalf("set github_pr_sessions session id: %v", err)
				}
			}
			repoSettings := narvipg.NewRepoSettingsStore(pool)
			if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
				t.Fatalf("promote repo to live egress: %v", err)
			}
			if _, err := repoSettings.UpsertPreviewSettings(ctx, repoFullName, "preview-build", "myapp-pr-{pr}", "review-pr-org"); err != nil {
				t.Fatalf("configure the repository's preview: %v", err)
			}
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, session.ID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			sourceControl := &fakeSourceControl{
				nextRef:           ports.PRRef{Number: 99, URL: "https://github.com/" + repoFullName + "/pull/99"},
				defaultBranchName: "main",
			}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, session.ID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			if tc.lookupFails {
				// Renamed away for the delivery only, and restored before
				// this test's own database reset runs (cleanups run last
				// in, first out; the pool's reset was registered first).
				renameGitHubPRSessionsAway(ctx, t, pool)
				deliverPushCompleteAndDrain(ctx, t, a, session.ID, "repo", reviewPushSessionBranch, pushedSHA)
				restoreGitHubPRSessions(ctx, t, pool)
			} else {
				deliverPushCompleteAndDrain(ctx, t, a, session.ID, "repo", reviewPushSessionBranch, pushedSHA)
			}

			artifacts, err := narvipg.NewArtifactStore(pool).ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list artifacts: %v", err)
			}
			dispatches := getOutboxRowsForSessionByKind(ctx, t, pool, session.ID, ports.NotificationKindRWXPreviewDispatch)
			links := getOutboxRowsForSessionByKind(ctx, t, pool, session.ID, ports.NotificationKindGitHubPreviewLink)

			if tc.wantOpened {
				if got := sourceControl.callCount(); got != 1 {
					t.Fatalf("CreatePR called %d times, want 1", got)
				}
				if len(artifacts) != 2 || len(dispatches) != 1 || len(links) != 1 {
					t.Fatalf("artifacts = %d, rwx_preview_dispatch = %d, github_preview_link = %d; want 2 (pr, preview), 1 and 1", len(artifacts), len(dispatches), len(links))
				}
				return
			}
			if got := sourceControl.callCount(); got != 0 {
				t.Errorf("CreatePR called %d times, want 0: a review session never opens a pull request", got)
			}
			if got := sourceControl.suppressCallCount(); got != 0 {
				t.Errorf("SuppressCreatePR called %d times, want 0: there is no pull request to suppress either", got)
			}
			if len(artifacts) != 0 {
				t.Errorf("%d artifacts recorded (first: %s %s), want none", len(artifacts), artifacts[0].Type, artifacts[0].Url)
			}
			if len(dispatches) != 0 || len(links) != 0 {
				t.Errorf("rwx_preview_dispatch = %d, github_preview_link = %d; want no preview enqueued", len(dispatches), len(links))
			}
		})
	}
}

// renameGitHubPRSessionsAway makes every read of github_pr_sessions fail
// with a genuine query error -- not pgx.ErrNoRows -- until
// restoreGitHubPRSessions. A t.Cleanup restores it too, in case the test
// fails in between; it runs before the shared database's own reset, which
// was registered earlier. No integration test in this package runs in
// parallel, so no other test can observe the table missing.
func renameGitHubPRSessionsAway(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `ALTER TABLE github_pr_sessions RENAME TO github_pr_sessions_unreadable_for_test`); err != nil {
		t.Fatalf("rename github_pr_sessions away: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `ALTER TABLE IF EXISTS github_pr_sessions_unreadable_for_test RENAME TO github_pr_sessions`); err != nil {
			t.Errorf("restore github_pr_sessions: %v", err)
		}
	})
}

// restoreGitHubPRSessions undoes renameGitHubPRSessionsAway.
func restoreGitHubPRSessions(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `ALTER TABLE github_pr_sessions_unreadable_for_test RENAME TO github_pr_sessions`); err != nil {
		t.Fatalf("restore github_pr_sessions: %v", err)
	}
}

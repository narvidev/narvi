//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
)

// This file proves, on real Postgres, the two actor-side readers of a
// session's provider-credential resolution under the one rule
// (providercredential.UserScopeTarget): the dispatch gate that refuses a
// turn whose model only a withheld personal link could run
// (credentialgate.go), and the counter-reviewer's choice of an opposing
// model (sessionconfig.go's reviewCredentialedProviders).

const credentialGateRepos = `[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`

// createLinkedMember creates a member whose own ChatGPT link (a
// scope=user/kind=oauth openai row) exists. The value is never decrypted
// on the paths these tests drive, so it is stored as opaque bytes.
func createLinkedMember(ctx context.Context, t *testing.T, pool *pgxpool.Pool, label string) pgtype.UUID {
	t.Helper()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("credential-gate-%s-%d@example.com", label, time.Now().UnixNano()),
		DisplayName:  "Linked member " + label,
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := narvipg.NewProviderCredentialStore(pool).UpsertOAuth(ctx, user.ID.String(), sqlcgen.ProviderCredentialProviderOpenai, []byte("opaque-oauth-blob"), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("link chatgpt account: %v", err)
	}
	return user.ID
}

func createGlobalCredential(ctx context.Context, t *testing.T, pool *pgxpool.Pool, provider sqlcgen.ProviderCredentialProvider) {
	t.Helper()
	if _, err := narvipg.NewProviderCredentialStore(pool).Create(ctx, sqlcgen.ProviderCredentialScopeGlobal, nil, provider, []byte("opaque-deployment-key")); err != nil {
		t.Fatalf("create global %s credential: %v", provider, err)
	}
}

func claimPullRequest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32, sessionID pgtype.UUID) {
	t.Helper()
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github pr session row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, sessionID); err != nil {
		t.Fatalf("set github pr session id: %v", err)
	}
}

// TestDispatchGate_PersonalLinkOnly drives a real EnsureDispatched against
// a live sandbox for each kind of session, the creator holding a personal
// openai link. Only a session that never resolves the link, whose turn
// names a model only that link could run, is refused -- named, before
// anything is sent; every other one is dispatched as it was before.
func TestDispatchGate_PersonalLinkOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	const openaiModel = "openai/gpt-5.4"
	const anthropicModel = "anthropic/claude-opus-4-5"

	tests := []struct {
		name string
		// kind: "review", "web", "automation" or "child".
		kind            string
		model           *string
		deployment      []sqlcgen.ProviderCredentialProvider
		wantRefused     bool
		wantCheckReason bool
	}{
		{name: "review session, model only the requester's link provides", kind: "review", model: strPtr(openaiModel), deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderAnthropic}, wantRefused: true, wantCheckReason: true},
		{name: "review session, no deployment credential at all", kind: "review", model: strPtr(openaiModel), wantRefused: true, wantCheckReason: true},
		{name: "review session, the deployment credentials the provider too", kind: "review", model: strPtr(openaiModel), deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderOpenai}},
		{name: "review session, model of a provider the link does not carry", kind: "review", model: strPtr(anthropicModel), deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderAnthropic}},
		{name: "review session, turn names no model", kind: "review"},
		{name: "web session its owner created keeps its link", kind: "web", model: strPtr(openaiModel)},
		{name: "automation session has no link to name", kind: "automation", model: strPtr(openaiModel)},
		{name: "child session with a creator", kind: "child", model: strPtr(openaiModel), wantRefused: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creator := createLinkedMember(ctx, t, pool, fmt.Sprintf("gate-%d", i))
			// Deployment rows are global, so each case clears the previous
			// case's before seeding its own.
			if _, err := pool.Exec(ctx, `DELETE FROM provider_credentials WHERE scope = 'global'`); err != nil {
				t.Fatalf("clear global credentials: %v", err)
			}
			for _, p := range tc.deployment {
				createGlobalCredential(ctx, t, pool, p)
			}

			params := sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: creator, Repos: []byte(credentialGateRepos)}
			switch tc.kind {
			case "review":
				params.SpawnSource = sqlcgen.SessionSpawnSourceGithub
			case "automation":
				params.CreatedBy = pgtype.UUID{}
			case "child":
				params.SpawnSource = sqlcgen.SessionSpawnSourceGithub
				params.ParentSessionID = createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceGithub)
				params.SpawnDepth = int32(1)
			}
			session, err := narvipg.NewSessionStore(pool).Create(ctx, params)
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if tc.kind == "review" {
				claimPullRequest(ctx, t, pool, "acme/widgets", int32(100+i), session.ID)
			}

			turnStore := narvipg.NewTurnStore(pool)
			headSHA := "c0ffee"
			created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
				SessionID:       session.ID,
				Status:          sqlcgen.TurnStatusPending,
				Prompt:          strPtr("review this pull request"),
				ModelID:         tc.model,
				ReviewHeadSha:   &headSHA,
				IsReviewAttempt: tc.kind == "review",
			})
			if err != nil {
				t.Fatalf("create pending turn: %v", err)
			}

			sandboxStore := narvipg.NewSandboxStore(pool)
			if _, err := sandboxStore.Create(ctx, session.ID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: session.ID, Status: sqlcgen.SandboxStatusReady}); err != nil {
				t.Fatalf("move sandbox to ready: %v", err)
			}

			commander := &fakeSendCommander{}
			r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, session.ID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)

			waitUntil(t, 5*time.Second, func() bool {
				got, err := turnStore.Get(ctx, created.ID)
				return err == nil && got.Status != sqlcgen.TurnStatusPending
			})
			got, err := turnStore.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("get turn: %v", err)
			}

			if !tc.wantRefused {
				if got.Status != sqlcgen.TurnStatusProcessing {
					t.Errorf("turn status = %s, want %s (dispatched as before)", got.Status, sqlcgen.TurnStatusProcessing)
				}
				if n := commander.callCount(); n != 1 {
					t.Errorf("prompts sent = %d, want 1", n)
				}
				return
			}

			if got.Status != sqlcgen.TurnStatusFailed {
				t.Fatalf("turn status = %s, want %s (refused before dispatch)", got.Status, sqlcgen.TurnStatusFailed)
			}
			if got.DispatchedAt.Valid {
				t.Error("turn dispatched_at set, want unset: a refused turn is never dispatched")
			}
			if n := commander.callCount(); n != 0 {
				t.Errorf("prompts sent = %d, want 0: a refused turn never reaches the sandbox", n)
			}

			gotSession, err := narvipg.NewSessionStore(pool).Get(ctx, session.ID)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if gotSession.Status != sqlcgen.SessionStatusFailed || gotSession.FailureReason == nil || *gotSession.FailureReason != sqlcgen.SessionFailureReasonNeverStarted {
				t.Errorf("session status/failure_reason = %s/%v, want failed/never_started", gotSession.Status, gotSession.FailureReason)
			}

			var reason string
			if err := pool.QueryRow(ctx,
				`SELECT payload->>'reason' FROM events WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`,
				session.ID,
			).Scan(&reason); err != nil {
				t.Fatalf("read the refused turn's terminal event: %v", err)
			}
			if !strings.HasPrefix(reason, string(providercredential.RefusalPersonalLinkOnly)+": ") || !strings.Contains(reason, *tc.model) {
				t.Errorf("terminal reason = %q, want it to name %q and the model %q", reason, providercredential.RefusalPersonalLinkOnly, *tc.model)
			}

			var kind string
			var payload []byte
			err = pool.QueryRow(ctx, `SELECT kind, payload FROM outbox WHERE session_id = $1`, session.ID).Scan(&kind, &payload)
			if !tc.wantCheckReason {
				if n := countOutboxRowsForSession(ctx, t, pool, session.ID); n != 0 {
					t.Errorf("outbox rows = %d, want 0 (not a review attempt)", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("read the review check emission: %v", err)
			}
			if kind != string(ports.NotificationKindGitHubReviewCheck) {
				t.Fatalf("outbox kind = %q, want %q", kind, ports.NotificationKindGitHubReviewCheck)
			}
			var check ports.ReviewCheckPayload
			if err := json.Unmarshal(payload, &check); err != nil {
				t.Fatalf("unmarshal review check payload: %v", err)
			}
			if check.Phase != string(reviewcheck.PhaseTerminalNotAssessed) || check.NotAssessedReason != string(reviewcheck.NotAssessedPersonalLinkOnly) || check.AttemptID != created.ID.String() {
				t.Errorf("review check = phase %q, reason %q, attempt %q; want %q, %q, %q",
					check.Phase, check.NotAssessedReason, check.AttemptID,
					reviewcheck.PhaseTerminalNotAssessed, reviewcheck.NotAssessedPersonalLinkOnly, created.ID.String())
			}
		})
	}
}

// TestReviewCounterReviewerModel_NeverCountsTheRequestersLink runs a real
// spawn of a review session opened by a member with a linked ChatGPT
// account, on a Narvi-authored pull request whose authoring model is
// anthropic. The counter-reviewer's preference order puts openai before
// google, so a resolution that counted the requester's link would pin
// openai; it must pick among the deployment's credentials only, and with
// none, pin nothing -- the fallback it has when no opposing provider is
// credentialed.
func TestReviewCounterReviewerModel_NeverCountsTheRequestersLink(t *testing.T) {
	tests := []struct {
		name       string
		deployment []sqlcgen.ProviderCredentialProvider
		wantPrefix string // "" means no override pinned at all
	}{
		{name: "deployment credentials google", deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderGoogle}, wantPrefix: "google/"},
		{name: "deployment credentials nothing opposing", wantPrefix: ""},
		{name: "deployment credentials openai itself", deployment: []sqlcgen.ProviderCredentialProvider{sqlcgen.ProviderCredentialProviderOpenai}, wantPrefix: "openai/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := newReviewCounterReviewerFixture(ctx, t, pool, "anthropic/claude-opus-4-5")

			creator := createLinkedMember(ctx, t, pool, "counter")
			if _, err := pool.Exec(ctx, `UPDATE sessions SET created_by = $1 WHERE id = $2`, creator, f.reviewSessionID); err != nil {
				t.Fatalf("attribute the review session to its first requester: %v", err)
			}
			for _, p := range tc.deployment {
				createGlobalCredential(ctx, t, pool, p)
			}

			got := f.spawnAndGetReviewCounterReviewerModel(ctx, t)
			switch {
			case tc.wantPrefix == "" && got != nil:
				t.Errorf("ReviewCounterReviewerModel = %q, want nil (only the requester's link carries an opposing provider)", *got)
			case tc.wantPrefix != "" && (got == nil || !strings.HasPrefix(*got, tc.wantPrefix)):
				t.Errorf("ReviewCounterReviewerModel = %v, want a %s model", got, tc.wantPrefix)
			}
		})
	}
}

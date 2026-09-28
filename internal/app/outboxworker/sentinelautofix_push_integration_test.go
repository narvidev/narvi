//go:build integration

package outboxworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// recordingCommander is a ports.SandboxCommander that records every
// command sent to a sandbox.
type recordingCommander struct {
	mu       sync.Mutex
	payloads []json.RawMessage
}

var _ ports.SandboxCommander = (*recordingCommander)(nil)

func (c *recordingCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, payload)
	return nil
}

func (c *recordingCommander) pushes(t *testing.T) []sandboxws.Push {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sandboxws.Push
	for _, raw := range c.payloads {
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

// TestSentinelAutoFixChild_IsNotAReviewSessionAndStillPushesItsFixBranch
// pins what "a review session is read-only" must leave alone: the
// sentinel auto-fix child. The origin is a real review session (it has a
// github_pr_sessions row); the child is spawned through the real notifier,
// and is not one -- no github_pr_sessions row of its own, no creator -- so
// when its turn completes the session actor still sends the push of its
// own narvi/sentinel-fix/<id> branch, and only that branch.
func TestSentinelAutoFixChild_IsNotAReviewSessionAndStillPushesItsFixBranch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	const repoFullName = "acme/widgets"
	const prNumber = 77
	const originHeadBranch = "feature-fix-me"

	// The origin: a pull request's review session, exactly as GitHub
	// ingress leaves one -- a github_pr_sessions row naming it.
	origin, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"` + originHeadBranch + `"}]`),
	})
	if err != nil {
		t.Fatalf("create origin review session: %v", err)
	}
	if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, origin.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	fix, err := sentinelFixes.Claim(ctx, repoFullName, prNumber, origin.ID, originHeadBranch)
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	const identityHash = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName, PrNumber: prNumber, IdentityHash: identityHash,
		Severity: "medium", FilePath: "internal/foo/bar.go", Description: "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	// Spawn the child through the real notifier. Its registry has no
	// provider and no commander, so the dispatch its creation triggers is
	// a logged no-op; it is shut down before the child's turn is driven
	// below, so exactly one actor ever handles the child at a time.
	spawnRegistry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry (spawn): %v", err)
	}
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, sessions, turns, narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), spawnRegistry, sentinelFixes, reviewFindings,
		&fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}, "gh-fake-bot-token", platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, prSessions,
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))
	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID:         fix.ID.String(),
		RepoFullName:          repoFullName,
		OriginPRNumber:        prNumber,
		OriginReviewSessionID: origin.ID.String(),
		OriginHeadBranch:      originHeadBranch,
		RepoName:              "widgets",
		RepoCloneURL:          "https://github.com/acme/widgets.git",
		FindingIdentityHashes: []string{identityHash},
		FindingDescriptions:   []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	// Shutdown waits for every actor it started to return; the
	// context.Canceled it reports is the run loops' own stop reason.
	if err := spawnRegistry.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("shut down the spawn registry: %v", err)
	}

	claimed, err := sentinelFixes.GetByID(ctx, fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if !claimed.FixChildSessionID.Valid {
		t.Fatal("no child session was spawned")
	}
	child, err := sessions.Get(ctx, claimed.FixChildSessionID)
	if err != nil {
		t.Fatalf("get child session: %v", err)
	}

	// What the child is, established from the real spawn rather than
	// assumed: not a review session, and no creator.
	if _, err := prSessions.GetBySessionID(ctx, child.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetBySessionID(child) error = %v, want pgx.ErrNoRows: the child must not be a review session", err)
	}
	if child.CreatedBy.Valid {
		t.Fatalf("child created_by = %v, want NULL", child.CreatedBy)
	}
	wantBranch := "narvi/sentinel-fix/" + fix.ID.String()
	var childRepos []struct {
		Branch *string `json:"branch"`
	}
	if err := json.Unmarshal(child.Repos, &childRepos); err != nil {
		t.Fatalf("unmarshal child repos: %v", err)
	}
	if len(childRepos) != 1 || childRepos[0].Branch == nil || *childRepos[0].Branch != wantBranch {
		t.Fatalf("child repos = %s, want exactly one repo on branch %q", child.Repos, wantBranch)
	}

	// Drive the child's turn to completion through a real actor whose
	// commander records what reaches the sandbox.
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, child.ID); err != nil {
		t.Fatalf("create child sandbox: %v", err)
	}
	childTurns, err := turns.ListForSession(ctx, child.ID)
	if err != nil || len(childTurns) != 1 {
		t.Fatalf("child turns = %d (err %v), want the one its creation inserted", len(childTurns), err)
	}
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: childTurns[0].ID, Status: sqlcgen.TurnStatusProcessing}); err != nil {
		t.Fatalf("move the child's turn to processing: %v", err)
	}

	commander := &recordingCommander{}
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	actor, err := registry.GetOrSpawn(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn(child): %v", err)
	}

	messageID := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: messageID, SessionId: child.ID.String(), Gen: 1,
		AckId: "execution_complete:" + messageID, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatalf("marshal execution_complete: %v", err)
	}
	// Sent twice: the actor handles one command at a time and sends the
	// push after the first delivery's reply, so the redelivery's reply
	// means the first delivery's push has already been sent (or never
	// will be). The redelivery itself completes nothing.
	for i := 0; i < 2; i++ {
		sendChildEvent(ctx, t, actor, sessionactor.SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: messageID, Raw: raw})
	}

	completed, err := turns.Get(ctx, childTurns[0].ID)
	if err != nil {
		t.Fatalf("get child turn: %v", err)
	}
	if completed.Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("child turn status = %s, want %s", completed.Status, sqlcgen.TurnStatusCompleted)
	}
	pushes := commander.pushes(t)
	if len(pushes) != 1 || len(pushes[0].Repos) != 1 || pushes[0].Repos[0].Branch != wantBranch || pushes[0].SessionId != child.ID.String() {
		t.Fatalf("push commands = %+v, want exactly one push of the child's own branch %q", pushes, wantBranch)
	}
}

func sendChildEvent(ctx context.Context, t *testing.T, a *sessionactor.Actor, cmd sessionactor.SandboxEvent) {
	t.Helper()
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	cmd.Reply = reply
	if err := a.Send(ctx, cmd); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatalf("%s was not persisted", cmd.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no reply to %s", cmd.Type)
	}
}

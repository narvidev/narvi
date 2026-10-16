//go:build integration

package sessionactor

// This file proves, on real Postgres, the session actor's half of
// technical plan §6.4, §27.1 and §30.4's rule that a pull request's review
// session whose head is not in its base repository boots without the
// repository's hooks (repohooksgate.go): SESSION_CONFIG's skipRepoHooks is
// set at a spawn and at a restore exactly when the claim's record and the
// spec do not say the head is in the base, and no turn of such a session
// is sent to a gen whose ready did not advertise it honours the key.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// repoHooksSessionCase is one review-session shape, its claim on
// acme/<name>#prNumber.
type repoHooksSessionCase struct {
	name string
	// spec is the session's sessions.repos, %[1]s the repository's name.
	spec string
	// claimed: the session has a pull request claim; record is its
	// head_in_base, nil for none recorded.
	claimed bool
	record  *bool
	// skip is whether the session boots without the repository's hooks.
	skip bool
}

func repoHooksSessionCases() []repoHooksSessionCase {
	inBase, notInBase := true, false
	const sameRepo = `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"feature-x"}]`
	const branchless = `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":null}]`
	return []repoHooksSessionCase{
		{name: "a session with no pull request", spec: sameRepo},
		{name: "a same-repository pull request's review session", spec: sameRepo, claimed: true, record: &inBase},
		{name: "a fork's pull request's review session", spec: branchless, claimed: true, record: &notInBase, skip: true},
		{name: "a review session whose claim nothing has recorded", spec: sameRepo, claimed: true, skip: true},
		{name: "a legacy deleted-fork review session", spec: `[{"name":"%[1]s","url":"https://github.com/acme/%[1]s.git","branch":"main"}]`, claimed: true, record: &notInBase, skip: true},
		{name: "a review session opened while its head could not be read", spec: branchless, claimed: true, record: &inBase, skip: true},
		{name: "a legacy fork review session, moved onto its base at this boot", spec: `[{"name":"%[1]s","url":"https://github.com/contributor/%[1]s.git","branch":"main"}]`, claimed: true, skip: true},
	}
}

// seedRepoHooksSession creates tc's GitHub session for acme/<name>,
// claimed and recorded as tc says.
func seedRepoHooksSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tc repoHooksSessionCase, name string, prNumber int32) pgtype.UUID {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(fmt.Sprintf(tc.spec, name)),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if tc.claimed {
		claimPullRequest(ctx, t, pool, "acme/"+name, prNumber, session.ID)
		if tc.record != nil {
			if err := narvipg.NewGitHubPRSessionStore(pool).RecordHeadInBase(ctx, "acme/"+name, prNumber, *tc.record); err != nil {
				t.Fatal(err)
			}
		}
	}
	return session.ID
}

// TestAssembleSessionConfig_SkipRepoHooksExactlyWhenTheHeadIsNotKnownInTheBase:
// the SESSION_CONFIG a fresh spawn and a restore deliver sets skipRepoHooks
// exactly for a claimed session whose head is not known to be a branch of
// its base repository -- the claim's record and the spec, as they are at
// that boot -- and leaves it unset for a session with no pull request and
// for a same-repository pull request's review session.
func TestAssembleSessionConfig_SkipRepoHooksExactlyWhenTheHeadIsNotKnownInTheBase(t *testing.T) {
	for i, tc := range repoHooksSessionCases() {
		for _, restore := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: ", a fresh spawn", true: ", a restore"}[restore], func(t *testing.T) {
				ctx := context.Background()
				pool := newTestPool(t)
				name := fmt.Sprintf("hooks-config-%d", i)
				sessionID := seedRepoHooksSession(ctx, t, pool, tc, name, int32(40+i))
				if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
					t.Fatal(err)
				}
				createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")
				provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "spawned-" + name}, nextRestoreRef: ports.SandboxRef{ProviderID: "restored-" + name}}
				if restore {
					seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-"+name)
				}
				r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
				t.Cleanup(func() { _ = r.Shutdown() })
				a, err := r.GetOrSpawn(ctx, sessionID)
				if err != nil {
					t.Fatalf("GetOrSpawn: %v", err)
				}
				sendEnsureDispatched(ctx, t, a)
				waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount()+provider.callCount() > 0 })

				var spec ports.CreateSpec
				if restore {
					if provider.restoreCallCount() != 1 {
						t.Fatalf("restores %d, fresh spawns %d; want one restore", provider.restoreCallCount(), provider.callCount())
					}
					spec = provider.lastRestoreCall().spec
				} else {
					if provider.callCount() != 1 {
						t.Fatalf("fresh spawns %d, restores %d; want one fresh spawn", provider.callCount(), provider.restoreCallCount())
					}
					spec = provider.lastSpec()
				}
				if got := spec.SessionConfig.SkipRepoHooks; got != tc.skip {
					t.Errorf("SessionConfig.SkipRepoHooks = %v, want %v", got, tc.skip)
				}
			})
		}
	}
}

// hooksReady is a ready of gen from an agent that runs the checkout
// command, advertising skipRepoHooks when capable.
func hooksReady(gen int, capable bool) SandboxEvent {
	id := "ready-" + uuid.NewString()
	caps := `,"capabilities":{"reviewCheckout":true}`
	if capable {
		caps = `,"capabilities":{"reviewCheckout":true,"skipRepoHooks":true}`
	}
	agentVersion, imageDigest := "dev", "unknown"
	return SandboxEvent{Type: "ready", Gen: gen, MessageID: id, AgentVersion: &agentVersion, ImageDigest: &imageDigest,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":"s","gen":%d,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"%s}`, id, gen, caps))}
}

// seedRepoHooksReadySession is seedRepoHooksSession with a ready sandbox
// at gen 1 whose capability no ready has recorded yet, and a pending turn,
// with a rig to drive it.
func seedRepoHooksReadySession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tc repoHooksSessionCase, name string, prNumber int32) (pgtype.UUID, sqlcgen.Turn, *holdRig) {
	t.Helper()
	sessionID := seedRepoHooksSession(ctx, t, pool, tc, name, prNumber)
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
		t.Fatal(err)
	}
	seedReadySandbox(ctx, t, pool, sessionID)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET skip_repo_hooks_gen = NULL WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	created := createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "a question about the pull request")
	return sessionID, created, newContextRig(ctx, t, pool, sessionID, nil)
}

// TestRepoHooksGate_AGenThatDoesNotHonourItIsNeverSentSuchASessionsTurn:
// a gen whose ready did not advertise skipRepoHooks, with no snapshot to
// discard, is never sent a turn of a session that boots without the
// repository's hooks: the turn is refused before anything is sent, naming
// the remedy. A session that boots with them is sent its turn on the same
// gen, and so is every session on a gen that advertised it.
func TestRepoHooksGate_AGenThatDoesNotHonourItIsNeverSentSuchASessionsTurn(t *testing.T) {
	for i, tc := range repoHooksSessionCases() {
		for _, capable := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: ", an agent that predates the key", true: ", a current agent"}[capable], func(t *testing.T) {
				ctx := context.Background()
				pool := newTestPool(t)
				name := fmt.Sprintf("hooks-gate-%d", i)
				sessionID, pending, rig := seedRepoHooksReadySession(ctx, t, pool, tc, name, int32(60+i))

				deliver(ctx, t, rig.actor, hooksReady(1, capable))

				got, err := narvipg.NewTurnStore(pool).Get(ctx, pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				prompts := sentPromptsOf(t, rig.commander)
				if tc.skip && !capable {
					if len(sentPayloads(rig.commander)) != 0 {
						t.Fatalf("commands %v sent to a gen that runs the repository's hooks whatever it is told, want none", sentCommandTypes(t, rig.commander))
					}
					if got.Status != sqlcgen.TurnStatusFailed || got.DispatchedAt.Valid {
						t.Fatalf("turn: %s, dispatched %v; want failed before it started", got.Status, got.DispatchedAt.Valid)
					}
					if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%repository''s hooks%' AND payload::text LIKE '%Rebuild the sandbox image%'`, sessionID); n != 1 {
						t.Fatalf("%d session warnings naming the cause and the remedy, want 1", n)
					}
					return
				}
				if len(prompts) != 1 || prompts[0].Text != "a question about the pull request" {
					t.Fatalf("prompts %+v, want the turn's, sent", prompts)
				}
				if got.Status != sqlcgen.TurnStatusProcessing {
					t.Fatalf("turn: %s, want processing", got.Status)
				}
			})
		}
	}
}

// TestRepoHooksGate_ARefusedReviewAttemptClosesItsCheckNotAssessed: the
// refused turn is a review attempt, so its check is closed not assessed
// with the reason, which says what to do.
func TestRepoHooksGate_ARefusedReviewAttemptClosesItsCheckNotAssessed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := checkoutFixture(ctx, t, pool, "acme/hooks-check", 81)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET skip_repo_hooks_gen = NULL WHERE session_id = $1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
	rig := newContextRig(ctx, t, pool, f.sessionID, nil)

	deliver(ctx, t, rig.actor, hooksReady(1, false))

	if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("review attempt: %s, want refused", got.Status)
	}
	if n := len(sentCheckouts(t, rig.commander)); n != 0 {
		t.Fatalf("%d checkouts sent, want none: the hooks' gate is asked first", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND payload::text LIKE '%repo_hooks_unsupported%'`, f.sessionID); n != 1 {
		t.Fatalf("%d not-assessed checks naming the reason, want 1", n)
	}
	var reason, delivered string
	if err := pool.QueryRow(ctx, `SELECT payload->>'reason', COALESCE(payload->>'delivered', '') FROM events WHERE session_id = $1 AND type = 'execution_complete' AND payload->>'turn_id' = $2`,
		f.sessionID, reviewTurn.ID.String()).Scan(&reason, &delivered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "repository's hooks") || delivered != "false" {
		t.Fatalf("synthetic end reason %q delivered %q; want the hooks named and marked undelivered", reason, delivered)
	}
}

// TestRepoHooksGate_ASnapshotWhoseAgentDoesNotHonourItIsRespawnedFresh: a
// gen that does not honour skipRepoHooks, with a snapshot -- whose restore
// may have brought that agent back -- is retired and the snapshot cleared;
// the next gen is spawned fresh, its SESSION_CONFIG setting skipRepoHooks,
// and the turn waits for it. A fresh gen that does not honour it either is
// refused: an old agent costs at most one respawn.
func TestRepoHooksGate_ASnapshotWhoseAgentDoesNotHonourItIsRespawnedFresh(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	fork := repoHooksSessionCases()[2]
	sessionID, pending, rig := seedRepoHooksReadySession(ctx, t, pool, fork, "hooks-snapshot", 90)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-old-agent' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}

	deliver(ctx, t, rig.actor, hooksReady(1, false))

	if n := len(sentPayloads(rig.commander)); n != 0 {
		t.Fatalf("%d commands sent to a gen that runs the repository's hooks whatever it is told, want none", n)
	}
	if rig.provider.restoreCallCount() != 0 || rig.provider.callCount() != 1 {
		t.Fatalf("restores %d, fresh spawns %d; want the next gen spawned fresh, never restored", rig.provider.restoreCallCount(), rig.provider.callCount())
	}
	if !rig.provider.lastSpec().SessionConfig.SkipRepoHooks {
		t.Fatal("the fresh gen's SESSION_CONFIG does not set skipRepoHooks")
	}
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SnapshotID != nil || sb.Gen != 2 {
		t.Fatalf("sandbox: snapshot %v, gen %d; want the snapshot cleared and gen 2", sb.SnapshotID, sb.Gen)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%snap-old-agent%' AND payload::text LIKE '%repository''s hooks%'`, sessionID); n != 1 {
		t.Fatalf("%d warnings naming the discarded snapshot and why, want 1", n)
	}
	turns := narvipg.NewTurnStore(pool)
	if got, err := turns.Get(ctx, pending.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("turn %v (%v), want pending for the fresh gen", got.Status, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	deliver(ctx, t, rig.actor, hooksReady(2, false))
	if got, err := turns.Get(ctx, pending.ID); err != nil || got.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("turn on a fresh gen that does not honour it: %v (%v), want refused", got.Status, err)
	}
	if rig.provider.callCount() != 1 {
		t.Fatalf("fresh spawns %d, want 1", rig.provider.callCount())
	}
}

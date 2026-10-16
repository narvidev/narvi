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
	// legacyFork: the spec still names the fork, which this release moves
	// at every spawn and restore, so a live gen of it was spawned by the
	// previous binary and told nothing.
	legacyFork bool
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
		{name: "a legacy fork review session, moved onto its base at this boot", spec: `[{"name":"%[1]s","url":"https://github.com/contributor/%[1]s.git","branch":"main"}]`, claimed: true, skip: true, legacyFork: true},
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

// setSnapshotProvenance records sessionID's sandbox snapshot's provenance
// as this release's mint does: the agent protocol it reported, "" for
// none, and whether its gen booted without the repository's hooks; with
// recorded false, as the previous binary's mint leaves it -- nothing
// recorded for that snapshot.
func setSnapshotProvenance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, recorded bool, protocol string, mintedWithoutHooks bool) {
	t.Helper()
	var protocolArg *string
	if protocol != "" {
		protocolArg = &protocol
	}
	if !recorded {
		if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_provenance_id = NULL, snapshot_agent_protocol = NULL, snapshot_skipped_repo_hooks = NULL WHERE session_id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = $2, snapshot_skipped_repo_hooks = $3 WHERE session_id = $1`,
		sessionID, protocolArg, mintedWithoutHooks); err != nil {
		t.Fatal(err)
	}
}

// TestAssembleSessionConfig_SkipRepoHooksExactlyWhenTheHeadIsNotKnownInTheBase:
// the SESSION_CONFIG a fresh spawn and a restore deliver sets skipRepoHooks
// exactly for a claimed session whose head is not known to be a branch of
// its base repository -- the claim's record and the spec, as they are at
// that boot -- and leaves it unset for a session with no pull request and
// for a same-repository pull request's review session. The gen records
// what it was told (skip_repo_hooks_told_gen). Each restore's snapshot is
// one the session may restore: minted by a current agent's gen that booted
// as the session does.
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
					setSnapshotProvenance(ctx, t, pool, sessionID, true, "1.28.0", tc.skip)
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
				sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				if told := sb.SkipRepoHooksToldGen != nil && *sb.SkipRepoHooksToldGen == sb.Gen; told != tc.skip {
					t.Errorf("gen %d recorded told %v (skip_repo_hooks_told_gen %v), want %v", sb.Gen, told, sb.SkipRepoHooksToldGen, tc.skip)
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
// at gen 1 whose capability no ready has recorded yet, recorded told to
// skip the repository's hooks when told, and a pending turn, with a rig to
// drive it.
func seedRepoHooksReadySession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tc repoHooksSessionCase, name string, prNumber int32, told bool) (pgtype.UUID, sqlcgen.Turn, *holdRig) {
	t.Helper()
	sessionID := seedRepoHooksSession(ctx, t, pool, tc, name, prNumber)
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
		t.Fatal(err)
	}
	seedReadySandbox(ctx, t, pool, sessionID)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET skip_repo_hooks_gen = NULL, skip_repo_hooks_told_gen = CASE WHEN $2::boolean THEN gen END WHERE session_id = $1`, sessionID, told); err != nil {
		t.Fatal(err)
	}
	created := createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "a question about the pull request")
	return sessionID, created, newContextRig(ctx, t, pool, sessionID, nil)
}

// repoHooksGen is the live gen a gate test seeds: what its SESSION_CONFIG
// told it, and whether its agent advertises it honours skipRepoHooks.
type repoHooksGen struct {
	name          string
	told, capable bool
}

func repoHooksGens() []repoHooksGen {
	return []repoHooksGen{
		{name: "told, an agent that predates the key", told: true},
		{name: "told, a current agent", told: true, capable: true},
		{name: "not told, a current agent", capable: true},
	}
}

// TestRepoHooksGate_AGenThatDidNotBootWithoutTheHooksIsNeverSentSuchASessionsTurn:
// a turn of a session that boots without the repository's hooks is sent
// only to a gen that was told to skip them and whose ready advertised it
// honours that. A told gen whose agent predates the key, with no snapshot
// to discard, has the turn refused before anything is sent, naming the
// image. A gen that was not told -- the previous binary's, and the only
// kind a spec still naming the fork can have -- ran the hooks, whatever
// its agent can do: it is retired at once and a fresh gen spawned, told,
// with no word of the image. A session that boots with the hooks is sent
// its turn on every gen.
func TestRepoHooksGate_AGenThatDidNotBootWithoutTheHooksIsNeverSentSuchASessionsTurn(t *testing.T) {
	for i, tc := range repoHooksSessionCases() {
		for j, gen := range repoHooksGens() {
			if tc.legacyFork && gen.told {
				continue
			}
			t.Run(tc.name+", "+gen.name, func(t *testing.T) {
				ctx := context.Background()
				pool := newTestPool(t)
				name := fmt.Sprintf("hooks-gate-%d-%d", i, j)
				sessionID, pending, rig := seedRepoHooksReadySession(ctx, t, pool, tc, name, int32(60+i), gen.told)

				deliver(ctx, t, rig.actor, hooksReady(1, gen.capable))

				got, err := narvipg.NewTurnStore(pool).Get(ctx, pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				prompts := sentPromptsOf(t, rig.commander)
				switch {
				case !tc.skip || (gen.told && gen.capable):
					if len(prompts) != 1 || prompts[0].Text != "a question about the pull request" {
						t.Fatalf("prompts %+v, want the turn's, sent", prompts)
					}
					if got.Status != sqlcgen.TurnStatusProcessing {
						t.Fatalf("turn: %s, want processing", got.Status)
					}
				case gen.told:
					if len(sentPayloads(rig.commander)) != 0 {
						t.Fatalf("commands %v sent to a gen whose agent runs the repository's hooks whatever it is told, want none", sentCommandTypes(t, rig.commander))
					}
					if got.Status != sqlcgen.TurnStatusFailed || got.DispatchedAt.Valid {
						t.Fatalf("turn: %s, dispatched %v; want failed before it started", got.Status, got.DispatchedAt.Valid)
					}
					if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%repository''s hooks%' AND payload::text LIKE '%Rebuild the sandbox image%'`, sessionID); n != 1 {
						t.Fatalf("%d session warnings naming the cause and the remedy, want 1", n)
					}
					if rig.provider.callCount() != 0 {
						t.Fatalf("fresh spawns %d, want none: the image is the problem", rig.provider.callCount())
					}
				default:
					if len(sentPayloads(rig.commander)) != 0 {
						t.Fatalf("commands %v sent to a gen that booted with the repository's hooks, want none", sentCommandTypes(t, rig.commander))
					}
					if got.Status != sqlcgen.TurnStatusPending {
						t.Fatalf("turn: %s, want pending for the fresh gen", got.Status)
					}
					if rig.provider.callCount() != 1 || rig.provider.restoreCallCount() != 0 {
						t.Fatalf("fresh spawns %d, restores %d; want the gen retired and one fresh spawn", rig.provider.callCount(), rig.provider.restoreCallCount())
					}
					if !rig.provider.lastSpec().SessionConfig.SkipRepoHooks {
						t.Fatal("the fresh gen's SESSION_CONFIG does not set skipRepoHooks")
					}
					sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
					if err != nil {
						t.Fatal(err)
					}
					if sb.Gen != 2 || sb.SkipRepoHooksToldGen == nil || *sb.SkipRepoHooksToldGen != 2 {
						t.Fatalf("sandbox gen %d, told gen %v; want gen 2, told", sb.Gen, sb.SkipRepoHooksToldGen)
					}
					if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%booted with the repository''s hooks%'`, sessionID); n != 1 {
						t.Fatalf("%d warnings saying the sandbox booted with the repository's hooks, want 1", n)
					}
					if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%Rebuild%'`, sessionID); n != 0 {
						t.Fatalf("%d warnings asking to rebuild the image, want none: the image is not the problem", n)
					}
				}
			})
		}
	}
}

// TestRepoHooksGate_AGenNotToldIsRetiredWithItsSnapshot: a gen of a
// session that boots without the repository's hooks, not told to skip
// them, with a snapshot -- which it may have minted -- is retired, the
// snapshot cleared, and the next gen spawned fresh, told, never restored.
func TestRepoHooksGate_AGenNotToldIsRetiredWithItsSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	fork := repoHooksSessionCases()[2]
	sessionID, pending, rig := seedRepoHooksReadySession(ctx, t, pool, fork, "hooks-not-told-snapshot", 91, false)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-hooks-ran', snapshot_provenance_id = 'snap-hooks-ran', snapshot_agent_protocol = '1.28.0', snapshot_skipped_repo_hooks = false WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}

	deliver(ctx, t, rig.actor, hooksReady(1, true))

	if n := len(sentPayloads(rig.commander)); n != 0 {
		t.Fatalf("%d commands sent to a gen that booted with the repository's hooks, want none", n)
	}
	if rig.provider.restoreCallCount() != 0 || rig.provider.callCount() != 1 {
		t.Fatalf("restores %d, fresh spawns %d; want the next gen spawned fresh", rig.provider.restoreCallCount(), rig.provider.callCount())
	}
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SnapshotID != nil || sb.Gen != 2 {
		t.Fatalf("sandbox: snapshot %v, gen %d; want the snapshot cleared and gen 2", sb.SnapshotID, sb.Gen)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%snap-hooks-ran%' AND payload::text LIKE '%booted with the%'`, sessionID); n != 1 {
		t.Fatalf("%d warnings naming the discarded snapshot and why, want 1", n)
	}
	if got, err := narvipg.NewTurnStore(pool).Get(ctx, pending.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("turn %v (%v), want pending for the fresh gen", got.Status, err)
	}
}

// TestRepoHooksGate_AReSentTurnIsGatedOnItsNewGen: a processing turn whose
// gen changed is re-sent to the new gen only through the same gate. A new
// gen told to skip the hooks whose agent predates the key fails the turn
// forward, its check closed not assessed; one that was not told is
// retired, the turn still processing for the fresh gen. Nothing is sent
// to either.
func TestRepoHooksGate_AReSentTurnIsGatedOnItsNewGen(t *testing.T) {
	for i, tc := range []struct {
		name          string
		told, capable bool
	}{
		{name: "told, an agent that predates the key", told: true},
		{name: "not told, a current agent", capable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := checkoutFixture(ctx, t, pool, fmt.Sprintf("acme/hooks-resend-%d", i), int32(95+i))
			reviewTurn := seedReviewTurn(ctx, t, f, coHead, nil, true)
			gen1, firstMessage := int32(1), "msg-gen-1"
			if _, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
				ID: reviewTurn.ID, Status: sqlcgen.TurnStatusProcessing,
				DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
				DispatchedSandboxGen: &gen1, DispatchedMessageID: &firstMessage,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = 2, skip_repo_hooks_gen = NULL, skip_repo_hooks_told_gen = CASE WHEN $2::boolean THEN 2 END WHERE session_id = $1`, f.sessionID, tc.told); err != nil {
				t.Fatal(err)
			}
			rig := newContextRig(ctx, t, pool, f.sessionID, nil)

			deliver(ctx, t, rig.actor, hooksReady(2, tc.capable))

			if n := len(sentPayloads(rig.commander)); n != 0 {
				t.Fatalf("commands %v re-sent to the new gen, want none", sentCommandTypes(t, rig.commander))
			}
			if tc.told {
				waitForTurnStatus(ctx, t, f.turns, reviewTurn.ID, sqlcgen.TurnStatusFailed)
				if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND payload::text LIKE '%repo_hooks_unsupported%'`, f.sessionID); n != 1 {
					t.Fatalf("%d not-assessed checks naming the reason, want 1", n)
				}
				return
			}
			if got := getTurn(ctx, t, f, reviewTurn.ID); got.Status != sqlcgen.TurnStatusProcessing {
				t.Fatalf("turn: %s, want still processing, for the fresh gen", got.Status)
			}
			if rig.provider.callCount() != 1 {
				t.Fatalf("fresh spawns %d, want 1: the gen that was not told is retired", rig.provider.callCount())
			}
		})
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
// told gen that does not honour skipRepoHooks, with a snapshot -- whose
// restore may have brought that agent back -- is retired and the snapshot
// cleared;
// the next gen is spawned fresh, its SESSION_CONFIG setting skipRepoHooks,
// and the turn waits for it. A fresh gen that does not honour it either is
// refused: an old agent costs at most one respawn.
func TestRepoHooksGate_ASnapshotWhoseAgentDoesNotHonourItIsRespawnedFresh(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	fork := repoHooksSessionCases()[2]
	sessionID, pending, rig := seedRepoHooksReadySession(ctx, t, pool, fork, "hooks-snapshot", 90, true)
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

// TestRepoHooksRestore_ASessionThatSkipsTheHooksNeverRestoresASnapshotThatMayRunThem:
// a session that boots without the repository's hooks is restored only
// from a snapshot minted by a current agent's gen that skipped them. A
// restore brings back the agent the snapshot holds, so one whose agent is
// older than protocol 1.28.0 or unknown would run them at the restored
// boot, and one whose gen ran them left what they wrote: each is refused
// before the restore -- the sandbox spawns fresh, told to skip them, the
// snapshot is cleared, and one warning says so.
func TestRepoHooksRestore_ASessionThatSkipsTheHooksNeverRestoresASnapshotThatMayRunThem(t *testing.T) {
	snapshots := []struct {
		name               string
		recorded           bool
		protocol           string
		mintedWithoutHooks bool
		restore            bool
	}{
		{name: "an agent of protocol 1.27.0", recorded: true, protocol: "1.27.0", mintedWithoutHooks: true},
		{name: "an agent of protocol 1.25.0", recorded: true, protocol: "1.25.0"},
		{name: "nothing recorded", recorded: false},
		{name: "no protocol reported", recorded: true},
		{name: "a current agent's gen that ran the hooks", recorded: true, protocol: "1.28.0"},
		{name: "a current agent's gen that skipped them", recorded: true, protocol: "1.28.0", mintedWithoutHooks: true, restore: true},
	}
	for i, tc := range repoHooksSessionCases() {
		if !tc.skip || tc.name == "a legacy deleted-fork review session" || tc.name == "a review session opened while its head could not be read" {
			continue
		}
		for j, snap := range snapshots {
			t.Run(tc.name+", "+snap.name, func(t *testing.T) {
				ctx := context.Background()
				pool := newTestPool(t)
				name := fmt.Sprintf("hooks-restore-%d-%d", i, j)
				sessionID := seedRepoHooksSession(ctx, t, pool, tc, name, int32(140+i))
				if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
					t.Fatal(err)
				}
				createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")
				seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-"+name)
				setSnapshotProvenance(ctx, t, pool, sessionID, snap.recorded, snap.protocol, snap.mintedWithoutHooks)
				provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "spawned-" + name}, nextRestoreRef: ports.SandboxRef{ProviderID: "restored-" + name}}
				r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
				t.Cleanup(func() { _ = r.Shutdown() })
				a, err := r.GetOrSpawn(ctx, sessionID)
				if err != nil {
					t.Fatalf("GetOrSpawn: %v", err)
				}
				sendEnsureDispatched(ctx, t, a)
				waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount()+provider.callCount() > 0 })

				sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				if snap.restore {
					if provider.restoreCallCount() != 1 || provider.callCount() != 0 {
						t.Fatalf("restores %d, fresh spawns %d; want the snapshot restored", provider.restoreCallCount(), provider.callCount())
					}
					if !provider.lastRestoreCall().spec.SessionConfig.SkipRepoHooks {
						t.Fatal("the restore's SESSION_CONFIG does not set skipRepoHooks")
					}
					return
				}
				if provider.restoreCallCount() != 0 || provider.callCount() != 1 {
					t.Fatalf("restores %d, fresh spawns %d; want no RestoreFromSnapshot, one fresh spawn", provider.restoreCallCount(), provider.callCount())
				}
				if !provider.lastSpec().SessionConfig.SkipRepoHooks {
					t.Fatal("the fresh spawn's SESSION_CONFIG does not set skipRepoHooks")
				}
				if sb.SnapshotID != nil {
					t.Fatalf("snapshot %v kept, want it cleared", *sb.SnapshotID)
				}
				if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%started fresh instead of from its last snapshot%' AND payload::text LIKE '%without the repository''s hooks%'`, sessionID); n != 1 {
					t.Fatalf("%d warnings saying why the snapshot was not restored, want 1", n)
				}
			})
		}
	}
}

// TestRepoHooksRestore_ASessionThatNowRunsTheHooksNeverRestoresAHooklessSnapshot:
// a session that booted without the repository's hooks and was
// snapshotted, then had its head recorded in the base, boots fresh rather
// than from that snapshot: its lineage never ran setup.sh, which a restore
// does not run, and its start.sh would fail on what setup.sh installs. The
// snapshot is cleared, so a later death of the fresh gen spawns fresh
// again, never back into that snapshot. A snapshot whose gen ran the hooks
// is restored as before.
func TestRepoHooksRestore_ASessionThatNowRunsTheHooksNeverRestoresAHooklessSnapshot(t *testing.T) {
	for i, mintedWithoutHooks := range []bool{true, false} {
		t.Run(map[bool]string{true: "a snapshot minted without the hooks", false: "a snapshot minted with them"}[mintedWithoutHooks], func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			name := fmt.Sprintf("hooks-flip-%d", i)
			// The review session of a pull request from a branch of its base,
			// whose head a later mention recorded in the base.
			sameRepo := repoHooksSessionCases()[1]
			sessionID := seedRepoHooksSession(ctx, t, pool, sameRepo, name, int32(150+i))
			if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
				t.Fatal(err)
			}
			turns := narvipg.NewTurnStore(pool)
			first := createPendingTurn(ctx, t, turns, sessionID, "review the pull request")
			seedStoppedSandboxWithSnapshot(ctx, t, pool, sessionID, "snap-"+name)
			setSnapshotProvenance(ctx, t, pool, sessionID, true, "1.28.0", mintedWithoutHooks)
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "spawned-" + name}, nextRestoreRef: ports.SandboxRef{ProviderID: "restored-" + name}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount()+provider.callCount() > 0 })

			if !mintedWithoutHooks {
				if provider.restoreCallCount() != 1 || provider.callCount() != 0 {
					t.Fatalf("restores %d, fresh spawns %d; want the snapshot restored", provider.restoreCallCount(), provider.callCount())
				}
				return
			}
			if provider.restoreCallCount() != 0 || provider.callCount() != 1 {
				t.Fatalf("restores %d, fresh spawns %d; want a fresh spawn, never the hookless snapshot", provider.restoreCallCount(), provider.callCount())
			}
			spec := provider.lastSpec()
			if spec.SessionConfig.SkipRepoHooks || spec.SessionConfig.BootMode != "fresh" {
				t.Fatalf("the fresh spawn's SESSION_CONFIG: skipRepoHooks %v, boot mode %s; want the hooks run, fresh -- setup.sh runs", spec.SessionConfig.SkipRepoHooks, spec.SessionConfig.BootMode)
			}
			if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%now runs the repository''s hooks%'`, sessionID); n != 1 {
				t.Fatalf("%d warnings saying why the snapshot was not restored, want 1", n)
			}
			sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sb.SnapshotID != nil {
				t.Fatalf("snapshot %v kept, want it cleared", *sb.SnapshotID)
			}

			// The fresh gen dies before it boots; the next turn spawns fresh
			// again -- no snapshot is left to restore, so no loop.
			if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: first.ID, Status: sqlcgen.TurnStatusFailed, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'failed' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
			createPendingTurn(ctx, t, turns, sessionID, "review the pull request again")
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 2 })
			if provider.restoreCallCount() != 0 {
				t.Fatalf("restores %d after the fresh gen died, want none", provider.restoreCallCount())
			}
		})
	}
}

// TestRepoHooksRestore_ARotationOfASkippingSessionNeverRestoresASnapshotTheRuleRefuses:
// the runway gate's rotation (technical plan §35.3) replaces a gen through
// tryPlanSpawn's ordinary decision, the hooks' restore check included. The
// rotating gen was told to skip the repository's hooks and honours that,
// but its snapshot reports no agent protocol, which a session that skips
// them never restores: the replacement spawns fresh, told, and the
// snapshot is cleared. With the protocol reported, the rotation restores
// its own snapshot.
func TestRepoHooksRestore_ARotationOfASkippingSessionNeverRestoresASnapshotTheRuleRefuses(t *testing.T) {
	for i, provenance := range []string{"", `{"agentProtocol":"1.28.0"}`} {
		restore := provenance != ""
		t.Run(map[bool]string{false: "no protocol reported", true: "a current agent's protocol"}[restore], func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			repo := fmt.Sprintf("acme/hooks-rotation-%d", i)
			f := newHoldFixture(ctx, t, pool, repo, int32(170+i), pgtype.UUID{})
			if _, err := f.prSessions.ClearPendingRetriggerHeadSHA(ctx, repo, int32(170+i), holdPushedHead); err != nil {
				t.Fatal(err)
			}
			seedRunwaySandbox(ctx, t, pool, f.sessionID, sqlcgen.SandboxStatusReady, 10*time.Minute, true)
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET review_checkout_gen = 1, skip_repo_hooks_gen = 1, skip_repo_hooks_told_gen = 1 WHERE session_id = $1`, f.sessionID); err != nil {
				t.Fatal(err)
			}
			createPendingTurn(ctx, t, f.turns, f.sessionID, "a question about the pull request")
			rig := newContextRig(ctx, t, pool, f.sessionID, nil)
			rig.provider.explicitStop = true
			rig.provider.nextRestoreRef = ports.SandboxRef{ProviderID: runwayRestoredObject}

			settle(ctx, t, rig.actor)
			if got := sentCommandTypes(t, rig.commander); len(got) != 1 || got[0] != "snapshot" {
				t.Fatalf("commands = %v, want the rotation's snapshot alone", got)
			}
			cmd := onlySnapshotCommand(t, rig.commander)
			deliver(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, runwayRotationSnap, provenance))
			waitUntil(t, 5*time.Second, func() bool { return rig.provider.restoreCallCount()+rig.provider.callCount() == 1 })

			if restore {
				if rig.provider.restoreCallCount() != 1 {
					t.Fatalf("restores %d, fresh spawns %d; want the rotation's snapshot restored", rig.provider.restoreCallCount(), rig.provider.callCount())
				}
				return
			}
			if rig.provider.restoreCallCount() != 0 || rig.provider.callCount() != 1 {
				t.Fatalf("restores %d, fresh spawns %d; want no RestoreFromSnapshot, one fresh spawn", rig.provider.restoreCallCount(), rig.provider.callCount())
			}
			if !rig.provider.lastSpec().SessionConfig.SkipRepoHooks {
				t.Fatal("the replacement's SESSION_CONFIG does not set skipRepoHooks")
			}
			sb, err := narvipg.NewSandboxStore(pool).Get(ctx, f.sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sb.SnapshotID != nil {
				t.Fatalf("snapshot %v kept, want the refused one cleared", *sb.SnapshotID)
			}
		})
	}
}

// TestRepoHooksGate_AResumeCarriesWhatTheGenWasTold: a resume boots nothing
// and delivers no SESSION_CONFIG, so the resumed gen was told what the gen
// it resumes was told -- one told to skip the repository's hooks stays
// told, one told nothing stays told nothing, and the gate retires it.
func TestRepoHooksGate_AResumeCarriesWhatTheGenWasTold(t *testing.T) {
	for i, told := range []bool{true, false} {
		t.Run(map[bool]string{true: "a told gen", false: "a gen told nothing"}[told], func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			fork := repoHooksSessionCases()[2]
			name := fmt.Sprintf("hooks-resume-%d", i)
			sessionID := seedRepoHooksSession(ctx, t, pool, fork, name, int32(180+i))
			if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
				t.Fatal(err)
			}
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "a question about the pull request")
			sandboxStore := seedStoppedSandboxWithProviderID(ctx, t, pool, sessionID, "resume-"+name)
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET skip_repo_hooks_told_gen = CASE WHEN $2::boolean THEN gen END WHERE session_id = $1`, sessionID, told); err != nil {
				t.Fatal(err)
			}
			before, err := sandboxStore.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			provider := &fakeSpawnProvider{resumeSupported: true}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.resumeCallCount() == 1 })

			row, err := sandboxStore.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Gen != before.Gen+1 {
				t.Fatalf("gen %d after the resume, want %d", row.Gen, before.Gen+1)
			}
			if got := skipRepoHooksTold(row); got != told {
				t.Fatalf("the resumed gen reads told %v (skip_repo_hooks_told_gen %v), want %v", got, row.SkipRepoHooksToldGen, told)
			}
		})
	}
}

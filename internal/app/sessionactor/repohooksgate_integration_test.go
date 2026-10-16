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
// none, and, when its gen booted without the repository's hooks, the
// snapshot's own id as minted without them. With recorded false, nothing
// is recorded for that snapshot, as for one minted before migration
// 000170; the previous binary records the provenance under the snapshot's
// id but never the hooks record (previousReleaseMintSnapshot).
func setSnapshotProvenance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, recorded bool, protocol string, mintedWithoutHooks bool) {
	t.Helper()
	var protocolArg *string
	if protocol != "" {
		protocolArg = &protocol
	}
	if !recorded {
		if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_provenance_id = NULL, snapshot_agent_protocol = NULL, snapshot_skipped_repo_hooks_id = NULL WHERE session_id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = $2, snapshot_skipped_repo_hooks_id = CASE WHEN $3::boolean THEN snapshot_id END WHERE session_id = $1`,
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
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-hooks-ran', snapshot_provenance_id = 'snap-hooks-ran', snapshot_agent_protocol = '1.28.0', snapshot_skipped_repo_hooks_id = NULL WHERE session_id = $1`, sessionID); err != nil {
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

// TestRepoHooksRestore_ASnapshotThePreviousBinaryMintsNeverInheritsTheRecord:
// the record that a snapshot's gen booted without the repository's hooks
// names that snapshot. This release's gen 1, told and capable, mints S1,
// recorded as minted without them. The previous binary then spawns gen 2
// and mints S2, each with its own statement sent as it sends it: S2's
// provenance is recorded under its id, a 1.28.0 agent, but the hooks
// record still names S1. The stopped sandbox of the fork's pull request's
// review session is not restored from S2: one fresh spawn, told to skip
// the hooks, one warning, the snapshot cleared. With S1 itself the live
// snapshot, it is restored.
func TestRepoHooksRestore_ASnapshotThePreviousBinaryMintsNeverInheritsTheRecord(t *testing.T) {
	for i, previousBinary := range []bool{true, false} {
		t.Run(map[bool]string{true: "the previous binary's snapshot after this release's", false: "this release's snapshot"}[previousBinary], func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			name := fmt.Sprintf("hooks-previous-mint-%d", i)
			sessionID := seedRepoHooksSession(ctx, t, pool, repoHooksSessionCases()[2], name, int32(190+i))
			if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
				t.Fatal(err)
			}
			sandboxes := narvipg.NewSandboxStore(pool)
			if _, err := sandboxes.Create(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if err := sandboxes.RecordRepoHooksTold(ctx, sessionID, 1, true); err != nil {
				t.Fatal(err)
			}
			if err := sandboxes.RecordReady(ctx, sessionID, 1, false, false, true, nil); err != nil {
				t.Fatal(err)
			}
			first, protocol := "snap-this-release", "1.28.0"
			row, err := sandboxes.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &first, AgentProtocol: &protocol})
			if err != nil {
				t.Fatal(err)
			}
			if row.SnapshotSkippedRepoHooksID == nil || *row.SnapshotSkippedRepoHooksID != first {
				t.Fatalf("snapshot_skipped_repo_hooks_id = %v, want %s", row.SnapshotSkippedRepoHooksID, first)
			}
			if previousBinary {
				if _, err := pool.Exec(ctx, previousReleaseUpsertSandboxForSpawn, sessionID, "token-hash-previous-binary", nil); err != nil {
					t.Fatalf("the previous binary's UpsertSandboxForSpawn: %v", err)
				}
				if _, err := pool.Exec(ctx, previousReleaseUpdateSandboxSnapshotID, "snap-previous-binary", false, protocol, nil, sessionID); err != nil {
					t.Fatalf("the previous binary's UpdateSandboxSnapshotID: %v", err)
				}
			}
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "spawned-" + name}, nextRestoreRef: ports.SandboxRef{ProviderID: "restored-" + name}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount()+provider.callCount() > 0 })

			if !previousBinary {
				if provider.restoreCallCount() != 1 || provider.callCount() != 0 || string(provider.lastRestoreCall().snapshotID) != first {
					t.Fatalf("restores %d, fresh spawns %d; want %s restored", provider.restoreCallCount(), provider.callCount(), first)
				}
				return
			}
			if provider.restoreCallCount() != 0 || provider.callCount() != 1 {
				t.Fatalf("restores %d, fresh spawns %d; want no RestoreFromSnapshot, one fresh spawn", provider.restoreCallCount(), provider.callCount())
			}
			if !provider.lastSpec().SessionConfig.SkipRepoHooks {
				t.Fatal("the fresh spawn's SESSION_CONFIG does not set skipRepoHooks")
			}
			sb, err := sandboxes.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sb.SnapshotID != nil {
				t.Fatalf("snapshot %v kept, want it cleared", *sb.SnapshotID)
			}
			if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%snap-previous-binary%' AND payload::text LIKE '%taken by a sandbox that booted with them%'`, sessionID); n != 1 {
				t.Fatalf("%d warnings saying why the snapshot was not restored, want 1", n)
			}
		})
	}
}

// TestRepoHooksRestore_ASessionThatNowRunsTheHooksRestoresNoSnapshotOfAHooklessLineage:
// a session that runs the repository's hooks restores no snapshot while the
// record names one minted without them. The review session's unrecorded
// claim has it skip the hooks: this release's gen 1, told, mints S1. The
// previous binary then spawns gen 2 -- a restore of S1, which runs no
// setup.sh -- and mints S2, each with its own statement, which never moves
// the record off S1. A mention then records the head in the base, so the
// session runs the hooks: the stopped sandbox is not restored from S2, which
// may descend from S1, but spawns fresh, so setup.sh runs; one warning, the
// snapshot and the record cleared. Control: when gen 1 ran the hooks, the
// record is NULL and S2 is restored -- only a hookless lineage pays the
// fresh spawn.
func TestRepoHooksRestore_ASessionThatNowRunsTheHooksRestoresNoSnapshotOfAHooklessLineage(t *testing.T) {
	for i, hookless := range []bool{true, false} {
		t.Run(map[bool]string{true: "after a snapshot minted without the hooks", false: "after a snapshot minted with them"}[hookless], func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			name := fmt.Sprintf("hooks-lineage-%d", i)
			tc := repoHooksSessionCases()[3]
			prNumber := int32(220 + i)
			sessionID := seedRepoHooksSession(ctx, t, pool, tc, name, prNumber)
			if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
				t.Fatal(err)
			}
			sandboxes := narvipg.NewSandboxStore(pool)
			if _, err := sandboxes.Create(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if err := sandboxes.RecordRepoHooksTold(ctx, sessionID, 1, hookless); err != nil {
				t.Fatal(err)
			}
			if err := sandboxes.RecordReady(ctx, sessionID, 1, false, false, true, nil); err != nil {
				t.Fatal(err)
			}
			first, protocol := "snap-S1", "1.28.0"
			if _, err := sandboxes.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &first, AgentProtocol: &protocol}); err != nil {
				t.Fatal(err)
			}
			second := "snap-S2"
			if _, err := pool.Exec(ctx, previousReleaseUpsertSandboxForSpawn, sessionID, "token-hash-previous-binary", nil); err != nil {
				t.Fatalf("the previous binary's UpsertSandboxForSpawn: %v", err)
			}
			if _, err := pool.Exec(ctx, previousReleaseUpdateSandboxSnapshotID, second, false, protocol, nil, sessionID); err != nil {
				t.Fatalf("the previous binary's UpdateSandboxSnapshotID: %v", err)
			}
			if err := narvipg.NewGitHubPRSessionStore(pool).RecordHeadInBase(ctx, "acme/"+name, prNumber, true); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, sessionID); err != nil {
				t.Fatal(err)
			}
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), sessionID, "review the pull request")
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "spawned-" + name}, nextRestoreRef: ports.SandboxRef{ProviderID: "restored-" + name}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.restoreCallCount()+provider.callCount() > 0 })

			if !hookless {
				if provider.restoreCallCount() != 1 || provider.callCount() != 0 || string(provider.lastRestoreCall().snapshotID) != second {
					t.Fatalf("restores %d, fresh spawns %d; want %s restored", provider.restoreCallCount(), provider.callCount(), second)
				}
				return
			}
			if provider.restoreCallCount() != 0 || provider.callCount() != 1 {
				t.Fatalf("restores %d, fresh spawns %d; want no RestoreFromSnapshot, one fresh spawn", provider.restoreCallCount(), provider.callCount())
			}
			if spec := provider.lastSpec(); spec.SessionConfig.SkipRepoHooks || spec.SessionConfig.BootMode != "fresh" {
				t.Fatalf("the fresh spawn's SESSION_CONFIG: skipRepoHooks %v, boot mode %s; want the hooks run, fresh -- setup.sh runs", spec.SessionConfig.SkipRepoHooks, spec.SessionConfig.BootMode)
			}
			sb, err := sandboxes.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sb.SnapshotID != nil || sb.SnapshotSkippedRepoHooksID != nil {
				t.Fatalf("snapshot %v, record %v kept; want both cleared", sb.SnapshotID, sb.SnapshotSkippedRepoHooksID)
			}
			if n := countRows(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload::text LIKE '%snap-S2%' AND payload::text LIKE '%now runs the repository''s hooks%'`, sessionID); n != 1 {
				t.Fatalf("%d warnings saying why the snapshot was not restored, want 1", n)
			}
		})
	}
}

// TestRepoHooksGate_AGenMidSnapshotIsRetiredAtOnce: a gen the gate
// retires is retired even while its snapshot after a turn is in progress
// -- one that never reports would otherwise hold the session for good,
// since the runway gate's bound on it runs after this gate -- and that
// snapshot is forgotten: the next gen boots fresh within the same
// dispatch, nothing is sent to the retired gen, and the snapshot's late
// report matches nothing. Turn 1 completes on gen 1 of the fork's pull
// request's review session, with turn 2 queued; gen 1 was not told to
// skip the repository's hooks, or was told with an agent that predates
// the key and holds a snapshot.
func TestRepoHooksGate_AGenMidSnapshotIsRetiredAtOnce(t *testing.T) {
	for i, gen := range []repoHooksGen{
		{name: "not told, a current agent", capable: true},
		{name: "told, an agent that predates the key", told: true},
	} {
		t.Run(gen.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			name := fmt.Sprintf("hooks-mid-snapshot-%d", i)
			sessionID := seedRepoHooksSession(ctx, t, pool, repoHooksSessionCases()[2], name, int32(200+i))
			if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/"+name, true); err != nil {
				t.Fatal(err)
			}
			rig := newRunwayRig(ctx, t, runwayRigOptions{pool: pool, sessionID: sessionID, beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
				if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'processing', dispatched_at = now(), dispatched_sandbox_gen = 1 WHERE session_id = $1`, sessionID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET skip_repo_hooks_told_gen = CASE WHEN $2::boolean THEN gen END,
					skip_repo_hooks_gen = CASE WHEN $3::boolean THEN gen END WHERE session_id = $1`, sessionID, gen.told, gen.capable); err != nil {
					t.Fatal(err)
				}
			}})
			queued := createTurnArmingDispatch(ctx, t, pool, sessionID, "a question queued behind the first")

			deliver(ctx, t, rig.actor, completedEvent(1))
			cmd := onlySnapshotCommand(t, rig.commander)
			waitUntil(t, 5*time.Second, func() bool { return rig.provider.callCount() == 1 })

			if n := promptCount(rig.commander); n != 0 {
				t.Fatalf("%d prompts sent, want none to the retired gen", n)
			}
			if rig.provider.restoreCallCount() != 0 {
				t.Fatalf("restores %d, want none: the gen's snapshots are discarded with it", rig.provider.restoreCallCount())
			}
			if stopped := rig.provider.stoppedProviderIDs(); len(stopped) != 1 || stopped[0] != runwayProviderGen1 {
				t.Fatalf("stopped %v, want the retired gen's object, %s", stopped, runwayProviderGen1)
			}
			if !rig.provider.lastSpec().SessionConfig.SkipRepoHooks {
				t.Fatal("the fresh gen's SESSION_CONFIG does not set skipRepoHooks")
			}
			row := rig.sandbox(ctx, t)
			if row.Gen != 2 || row.PendingSnapshotMessageID != nil || row.SnapshotID != nil {
				t.Fatalf("sandbox gen %d, pending snapshot %v, snapshot %v; want gen 2, nothing pending, nothing to restore", row.Gen, row.PendingSnapshotMessageID, row.SnapshotID)
			}
			if got, err := narvipg.NewTurnStore(pool).Get(ctx, queued.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
				t.Fatalf("the queued turn: %v, %v; want pending for the fresh gen", got.Status, err)
			}

			// The forgotten snapshot's late report matches nothing.
			deliver(ctx, t, rig.actor, snapshotReady(1, cmd.MessageId, "snap-late", `{"agentProtocol":"1.28.0"}`))
			if row := rig.sandbox(ctx, t); row.SnapshotID != nil {
				t.Fatalf("snapshot %s recorded from the retired gen's late report, want none", *row.SnapshotID)
			}
		})
	}
}

// TestRepoHooksGate_AGenAfterTheToldOneIsNotTold: what a gen was told is
// keyed by the gen. Gen 1 was told to skip the repository's hooks; gen 2
// was then spawned with nothing recorded -- the previous binary's spawn --
// so the record still names gen 1. Gen 2's ready advertises the
// capability, but gen 2 was told nothing: it is retired, never sent the
// turn, and a fresh gen spawned, told.
func TestRepoHooksGate_AGenAfterTheToldOneIsNotTold(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID, pending, rig := seedRepoHooksReadySession(ctx, t, pool, repoHooksSessionCases()[2], "hooks-told-earlier", 210, true)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = 2 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}

	deliver(ctx, t, rig.actor, hooksReady(2, true))

	if prompts := sentPromptsOf(t, rig.commander); len(prompts) != 0 {
		t.Fatalf("prompts %+v sent to a gen that was told nothing, want none", prompts)
	}
	if rig.provider.callCount() != 1 || rig.provider.restoreCallCount() != 0 {
		t.Fatalf("fresh spawns %d, restores %d; want the gen retired and one fresh spawn", rig.provider.callCount(), rig.provider.restoreCallCount())
	}
	sb, err := narvipg.NewSandboxStore(pool).Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Gen != 3 || sb.SkipRepoHooksToldGen == nil || *sb.SkipRepoHooksToldGen != 3 {
		t.Fatalf("sandbox gen %d, told gen %v; want gen 3, told", sb.Gen, sb.SkipRepoHooksToldGen)
	}
	if got, err := narvipg.NewTurnStore(pool).Get(ctx, pending.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("the turn: %v, %v; want pending for the fresh gen", got.Status, err)
	}
}

// previousReleaseUpsertSandboxForSpawn and
// previousReleaseUpdateSandboxSnapshotID are the previous release's own
// UpsertSandboxForSpawn and UpdateSandboxSnapshotID, copied byte for byte
// from main's generated sqlcgen (8c6a438f): they name none of migration
// 000175's columns, so a gen the first spawns is told nothing here, and a
// snapshot the second mints has its provenance recorded under its id but
// no record of the repository's hooks.
const previousReleaseUpsertSandboxForSpawn = `-- name: UpsertSandboxForSpawn :one
INSERT INTO sandboxes (session_id, gen, status, token_hash,
                       lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen)
VALUES ($1, 1, 'spawning', $2,
        now() + make_interval(secs => $3::integer),
        $3::integer,
        CASE WHEN $3::integer IS NULL THEN NULL ELSE 1 END)
ON CONFLICT (session_id) DO UPDATE
SET gen = sandboxes.gen + 1,
    status = 'spawning',
    token_hash = EXCLUDED.token_hash,
    last_seen_at = now(),
    agent_version = NULL,
    image_digest = NULL,
    image_decision_reason = NULL,
    image_decision_fingerprint = NULL,
    pr_delivery_started_at = NULL,
    stop_retire_gen = NULL,
    rotation_gen = NULL,
    rotation_message_id = NULL,
    rotation_started_at = NULL,
    snapshot_started_at = NULL,
    snapshot_start_message_id = NULL,
    heartbeat_ready_seq = NULL,
    heartbeats_since_ready = NULL,
    lifetime_deadline_at = CASE
        WHEN $3::integer IS NOT NULL THEN EXCLUDED.lifetime_deadline_at
        WHEN sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.lifetime_deadline_at
    END,
    lifetime_seconds = CASE
        WHEN $3::integer IS NOT NULL THEN EXCLUDED.lifetime_seconds
        WHEN sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.lifetime_seconds
    END,
    lifetime_deadline_gen = CASE
        WHEN $3::integer IS NOT NULL
          OR sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.gen + 1
    END,
    updated_at = now()
RETURNING id, session_id, gen, status, last_seen_at, created_at, updated_at, token_hash, provider_id, spawn_failure_count, last_spawn_failure_at, snapshot_id, pending_snapshot_message_id, pre_suspect_status, snapshot_suppressed_in_shadow, pending_push_suppressed_in_shadow, pending_push_cancelled, demotion_terminate_requested_at, agent_version, image_digest, image_decision_reason, image_decision_fingerprint, pr_delivery_started_at, boot_evidence_gen, booting_since, booting_since_gen, stop_retire_gen, prompt_receipt_gen, ready_seq, agent_max_frame_bytes, agent_max_frame_bytes_gen, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen, review_checkout_gen, snapshot_provenance_id, snapshot_agent_protocol, snapshot_runtime_version, snapshot_minted_at, rotation_gen, rotation_message_id, rotation_started_at, snapshot_started_at, snapshot_start_message_id, heartbeat_ready_seq, heartbeats_since_ready
`

const previousReleaseUpdateSandboxSnapshotID = `-- name: UpdateSandboxSnapshotID :one
UPDATE sandboxes
SET snapshot_id = $1,
    snapshot_suppressed_in_shadow = $2,
    pending_snapshot_message_id = NULL,
    snapshot_started_at = NULL,
    snapshot_start_message_id = NULL,
    snapshot_provenance_id = $1,
    snapshot_agent_protocol = $3,
    snapshot_runtime_version = $4,
    snapshot_minted_at = now(),
    updated_at = now()
WHERE session_id = $5
RETURNING id, session_id, gen, status, last_seen_at, created_at, updated_at, token_hash, provider_id, spawn_failure_count, last_spawn_failure_at, snapshot_id, pending_snapshot_message_id, pre_suspect_status, snapshot_suppressed_in_shadow, pending_push_suppressed_in_shadow, pending_push_cancelled, demotion_terminate_requested_at, agent_version, image_digest, image_decision_reason, image_decision_fingerprint, pr_delivery_started_at, boot_evidence_gen, booting_since, booting_since_gen, stop_retire_gen, prompt_receipt_gen, ready_seq, agent_max_frame_bytes, agent_max_frame_bytes_gen, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen, review_checkout_gen, snapshot_provenance_id, snapshot_agent_protocol, snapshot_runtime_version, snapshot_minted_at, rotation_gen, rotation_message_id, rotation_started_at, snapshot_started_at, snapshot_start_message_id, heartbeat_ready_seq, heartbeats_since_ready
`

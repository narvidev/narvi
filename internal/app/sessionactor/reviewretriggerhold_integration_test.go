//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §24.9's first rule on real Postgres,
// through the real actor: an automatic re-review holds while a turn of its
// review session is open, and every way that turn can end moves the held
// debounce to now in the transaction that ends it.

const (
	// holdPushedHead is the head the synchronize webhook left pending.
	holdPushedHead = "sha-pushed-during-the-review"
	// holdLiveHead is the head the fire's live fetch resolves: the review
	// the hold kept back is of it.
	holdLiveHead = "sha-live-after-the-review"
)

// holdFixture is one pull request's review session whose automatic
// re-review the debounce holds: the repository opted in and live, a pushed
// head pending, and -- once a case armed it -- the debounce as a held
// firing leaves it, ReviewRetriggerHoldBackstop out.
type holdFixture struct {
	pool         *pgxpool.Pool
	sessionID    pgtype.UUID
	repoFullName string
	prNumber     int32
	turns        *narvipg.TurnStore
	timers       *narvipg.TimerStore
	prSessions   *narvipg.GitHubPRSessionStore
}

func newHoldFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, prNumber int32, createdBy pgtype.UUID) *holdFixture {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   createdBy,
		Repos:       reposJSONForTest(t, "widgets", "https://github.com/"+repoFullName+".git", ""),
	})
	if err != nil {
		t.Fatalf("create the review session: %v", err)
	}
	claimPullRequest(ctx, t, pool, repoFullName, prNumber, session.ID)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote the repository to live egress: %v", err)
	}
	if _, err := repoSettings.UpsertAutoRetriggerReviewToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("opt in to the automatic re-review: %v", err)
	}
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	if _, err := prSessions.UpsertPendingRetriggerHeadSHA(ctx, repoFullName, prNumber, holdPushedHead); err != nil {
		t.Fatalf("leave a pushed head pending: %v", err)
	}
	return &holdFixture{
		pool: pool, sessionID: session.ID, repoFullName: repoFullName, prNumber: prNumber,
		turns: narvipg.NewTurnStore(pool), timers: narvipg.NewTimerStore(pool), prSessions: prSessions,
	}
}

// armDebounce arms the session's debounce to fire at fireAt, the way the
// webhook or a held firing does (UpsertSessionTimer).
func (f *holdFixture) armDebounce(ctx context.Context, t *testing.T, fireAt time.Time) {
	t.Helper()
	if _, err := f.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: f.sessionID, Name: TimerReviewRetriggerDebounce,
		FiresAt: pgtype.Timestamptz{Time: fireAt, Valid: true},
	}); err != nil {
		t.Fatalf("arm the debounce: %v", err)
	}
}

// armHeldDebounce leaves the debounce as a held firing does.
func (f *holdFixture) armHeldDebounce(ctx context.Context, t *testing.T) {
	t.Helper()
	f.armDebounce(ctx, t, time.Now().Add(platform.DefaultTimeouts().ReviewRetriggerHoldBackstop))
}

// debounce reads the session's debounce, with whether it is due on the
// database's clock and whether it sits at least half the backstop out --
// as a held firing re-arms it; ok is false when the session has none.
func (f *holdFixture) debounce(ctx context.Context, t *testing.T) (row sqlcgen.SessionTimer, due, held, ok bool) {
	t.Helper()
	half := platform.DefaultTimeouts().ReviewRetriggerHoldBackstop.Seconds() / 2
	err := f.pool.QueryRow(ctx, `
		SELECT id, session_id, name, fires_at, created_at, armed_at,
		       fires_at <= now(), fires_at > now() + make_interval(secs => $3)
		FROM session_timers WHERE session_id = $1 AND name = $2`,
		f.sessionID, TimerReviewRetriggerDebounce, half,
	).Scan(&row.ID, &row.SessionID, &row.Name, &row.FiresAt, &row.CreatedAt, &row.ArmedAt, &due, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.SessionTimer{}, false, false, false
	}
	if err != nil {
		t.Fatalf("read the debounce: %v", err)
	}
	return row, due, held, true
}

func (f *holdFixture) prSession(ctx context.Context, t *testing.T) sqlcgen.GithubPrSession {
	t.Helper()
	row, err := f.prSessions.GetBySessionID(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("read the pull request's session row: %v", err)
	}
	return row
}

// automaticReviews counts the review attempts of the live head: the ones
// the debounce's fire inserted.
func (f *holdFixture) automaticReviews(ctx context.Context, t *testing.T) int {
	t.Helper()
	return countRows(ctx, t, f.pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND is_review_attempt AND review_head_sha = $2`, f.sessionID, holdLiveHead)
}

// assertEndedAndWoken checks, in one statement, that turnID ended with
// want, that the debounce is due, and that the turn's last write and the
// debounce's were made by one transaction: equal xmin.
func (f *holdFixture) assertEndedAndWoken(ctx context.Context, t *testing.T, turnID pgtype.UUID, want sqlcgen.TurnStatus) {
	t.Helper()
	var status string
	var due, sameTx bool
	err := f.pool.QueryRow(ctx, `
		SELECT t.status::text, st.fires_at <= now(), t.xmin::text = st.xmin::text
		FROM turns t
		JOIN session_timers st ON st.session_id = t.session_id AND st.name = $2
		WHERE t.id = $1`, turnID, TimerReviewRetriggerDebounce).Scan(&status, &due, &sameTx)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("the debounce is gone after the turn ended: the wake-up must move it, never drop it")
	}
	if err != nil {
		t.Fatalf("read the ended turn and the debounce: %v", err)
	}
	if status != string(want) {
		t.Fatalf("turn status = %s, want %s", status, want)
	}
	if !due {
		t.Fatal("the debounce is not due after the turn ended: the held re-review waits out the backstop")
	}
	if !sameTx {
		t.Fatal("the turn's end and the debounce's wake-up were written by different transactions")
	}
}

// holdRig is the real session actor hosting a holdFixture's session, its
// registry wired with a review diff fetcher whose live head is
// holdLiveHead, a commander and a spawn provider.
type holdRig struct {
	registry  *Registry
	actor     *Actor
	commander *fakeSendCommander
	provider  *fakeSpawnProvider
	fetcher   *fakeReviewDiffFetcher
}

// newHoldRig hosts sessionID's actor on a registry of its own. fetcher is
// the registry's review diff fetcher; nil means a fakeReviewDiffFetcher
// whose live head is holdLiveHead, which rig.fetcher then holds. mode is
// the registry's rollout mode, open when absent.
func newHoldRig(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, fetcher reviewcontext.Fetcher, mode ...platform.RolloutMode) *holdRig {
	t.Helper()
	var rolloutMode platform.RolloutMode
	if len(mode) > 0 {
		rolloutMode = mode[0]
	}
	rig := &holdRig{
		commander: &fakeSendCommander{},
		provider:  &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-hold"}},
	}
	if fetcher == nil {
		rig.fetcher = &fakeReviewDiffFetcher{nextHeadSHA: holdLiveHead, nextBaseRef: "main", nextDiff: oneLineReadableDiff}
		fetcher = rig.fetcher
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, rig.commander, rig.provider, "http://localhost:8080", nil, nil, "", nil, false,
		RegistryOptions{
			ReviewDiffFetcher: fetcher, GitHubBotHandle: "narvi-bot",
			GitHubOutbound:       platform.MustNewGitHubOutboundConfig("test-token"),
			ReviewSizeExclusions: domainreviewtriage.DefaultSizeExclusions(),
			RolloutMode:          rolloutMode,
		})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	rig.registry = r
	if rig.actor, err = r.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return rig
}

// prCalls is how many times the fire read the pull request, read under the
// fake's lock: the actor's goroutine counts them.
func (f *fakeReviewDiffFetcher) prCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getPRCalls
}

// fireHeldDebounce delivers the debounce's firing to the actor and waits
// for it to hold: the row re-armed at least half the backstop out.
func fireHeldDebounce(ctx context.Context, t *testing.T, rig *holdRig, f *holdFixture) {
	t.Helper()
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		_, _, held, ok := f.debounce(ctx, t)
		return ok && held
	})
}

// pumpUntilDebounceHandled runs real timer-pump ticks until the debounce
// row is gone: its fire inserted the review or declined.
func pumpUntilDebounceHandled(ctx context.Context, t *testing.T, rig *holdRig, f *holdFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := rig.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		if _, _, _, ok := f.debounce(ctx, t); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the debounce was never handled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requestStop writes a person's stop the way the REST route does: every
// open turn flagged, the session's request recorded and its stop timer
// armed due, in one transaction under the session's actor-epoch lock.
func requestStop(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) pgtype.Timestamptz {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sessions := narvipg.NewSessionStore(pool)
	if _, err := sessions.WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
		t.Fatalf("lock the session: %v", err)
	}
	if _, err := narvipg.NewTurnStore(pool).WithTx(tx).RequestStopOpen(ctx, sessionID); err != nil {
		t.Fatalf("flag the open turns: %v", err)
	}
	requestedAt, err := sessions.WithTx(tx).RequestStop(ctx, sessionID)
	if err != nil {
		t.Fatalf("record the stop: %v", err)
	}
	if _, err := narvipg.NewTimerStore(pool).WithTx(tx).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID, Name: TimerStop, FiresAt: requestedAt,
	}); err != nil {
		t.Fatalf("arm the stop timer: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return requestedAt
}

// holdOpenTurn creates the turn holding the re-review: a review attempt of
// the head under review, in status, dispatched at dispatchedAt to gen 1
// when it is in flight.
func holdOpenTurn(ctx context.Context, t *testing.T, f *holdFixture, status sqlcgen.TurnStatus, prompt string, model *string, dispatchedAt time.Time) sqlcgen.Turn {
	t.Helper()
	head := "sha-under-review"
	created, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ModelID: model,
		ReviewHeadSha: &head, IsReviewAttempt: true,
	})
	if err != nil {
		t.Fatalf("create the open turn: %v", err)
	}
	if status == sqlcgen.TurnStatusPending {
		return created
	}
	gen := int32(1)
	watermark, err := narvipg.NewEventStore(f.pool).MaxEventIDForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("read the watermark: %v", err)
	}
	messageID := uuid.NewString()
	moved, err := f.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: status,
		DispatchedAt:         pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
		DispatchedSandboxGen: &gen, DispatchedEventID: &watermark, DispatchedMessageID: &messageID,
	})
	if err != nil {
		t.Fatalf("move the open turn to %s: %v", status, err)
	}
	return moved
}

// executionCompleteEnding ends the processing review with a real
// execution_complete of outcome, through the actor.
func executionCompleteEnding(outcome sandboxws.ExecutionCompleteOutcome) func(context.Context, *testing.T, *holdFixture, *holdRig) {
	return func(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig) {
		t.Helper()
		raw := executionCompleteRaw(t, f.sessionID.String(), 1, outcome)
		var evt struct {
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(raw, &evt); err != nil {
			t.Fatalf("decode execution_complete: %v", err)
		}
		if outcome := sendSandboxEventForTest(ctx, t, rig.actor, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: evt.MessageID, Raw: raw}); !outcome.Persisted {
			t.Fatal("execution_complete not persisted")
		}
	}
}

func ensureDispatchedEnding(ctx context.Context, t *testing.T, _ *holdFixture, rig *holdRig) {
	t.Helper()
	sendEnsureDispatched(ctx, t, rig.actor)
}

// TestReviewRetriggerHold_EveryTurnEndReleasesTheHoldAtOnce is the exit's
// second sentence (technical plan §24.9): every way a turn of the review
// session ends releases the hold at once. Each case starts held -- a
// pushed head pending, the debounce re-armed the backstop out after any
// stop, a turn open -- ends that turn through the real path that ends it,
// and finds, in one statement, the turn terminal, the debounce due, and
// both last written by one transaction (equal xmin). The next real pump
// tick then inserts the one review the hold kept back, of the live head.
// A case that revoked the repository restores it first: the fire itself
// drops a revoked repository's re-review (§31.4).
func TestReviewRetriggerHold_EveryTurnEndReleasesTheHoldAtOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	openaiModel := "openai/gpt-5.4"

	type ending struct {
		name string
		// linked gives the session a creator holding a personal openai
		// link, on a deployment holding only an anthropic key.
		linked bool
		// seed creates what the ending needs and the turn holding the
		// re-review, and returns that turn.
		seed func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn
		// arm runs before the ending, once the actor is hosted.
		arm  func(rig *holdRig)
		end  func(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig)
		want sqlcgen.TurnStatus
		// mode is the registry's rollout mode, open when empty.
		mode platform.RolloutMode
		// The evidence that the ending took the path the case names: the
		// synthetic execution_complete's reason ("" for a real one), the
		// prompts sent (-1 to skip), and whether the stop timer is still
		// armed once the turn has ended.
		wantReason    string
		wantSends     int
		wantStopTimer bool
		// release undoes what would keep the next review from running.
		release func(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig)
	}
	processing := func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
		seedReadySandbox(ctx, t, f.pool, f.sessionID)
		return holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now())
	}
	pendingOnReadySandbox := func(prompt string) func(context.Context, *testing.T, *holdFixture) sqlcgen.Turn {
		return func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
			seedReadySandbox(ctx, t, f.pool, f.sessionID)
			return holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, prompt, nil, time.Time{})
		}
	}
	restore := func(ctx context.Context, t *testing.T, f *holdFixture, _ *holdRig) {
		t.Helper()
		if _, err := narvipg.NewRepoEntitlementRevocationStore(f.pool).Restore(ctx, f.repoFullName); err != nil {
			t.Fatalf("restore the repository: %v", err)
		}
	}
	stopped := func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
		created := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", nil, time.Time{})
		seedReadySandbox(ctx, t, f.pool, f.sessionID)
		requestStop(ctx, t, f.pool, f.sessionID)
		return created
	}

	for i, tc := range []ending{
		{name: "execution_complete completed", seed: processing, end: executionCompleteEnding(sandboxws.ExecutionCompleteOutcomeCompleted), want: sqlcgen.TurnStatusCompleted, wantSends: -1},
		{name: "execution_complete failed", seed: processing, end: executionCompleteEnding(sandboxws.ExecutionCompleteOutcomeFailed), want: sqlcgen.TurnStatusFailed, wantSends: -1},
		{name: "execution_complete cancelled", seed: processing, end: executionCompleteEnding(sandboxws.ExecutionCompleteOutcomeCancelled), want: sqlcgen.TurnStatusCancelled, wantSends: -1},
		{
			name: "the turn's deadline",
			seed: func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
				seedReadySandbox(ctx, t, f.pool, f.sessionID)
				return holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now().Add(-2*platform.DefaultTimeouts().TurnDeadline))
			},
			end: func(ctx context.Context, t *testing.T, _ *holdFixture, rig *holdRig) {
				if err := rig.actor.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
					t.Fatalf("Send TimerFired: %v", err)
				}
			},
			want: sqlcgen.TurnStatusFailed, wantReason: "timeout", wantSends: -1,
		},
		{
			name: "a send with no live sandbox connection",
			seed: pendingOnReadySandbox("review this pull request"),
			arm: func(rig *holdRig) {
				rig.commander.mu.Lock()
				rig.commander.nextErr = ports.ErrNoLiveSandboxConnection
				rig.commander.mu.Unlock()
			},
			end:  ensureDispatchedEnding,
			want: sqlcgen.TurnStatusFailed, wantReason: "failed to deliver prompt to sandbox", wantSends: 1,
			release: func(_ context.Context, _ *testing.T, _ *holdFixture, rig *holdRig) {
				rig.commander.mu.Lock()
				rig.commander.nextErr = nil
				rig.commander.mu.Unlock()
			},
		},
		{
			name: "a revocation read at dispatch",
			seed: func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
				created := pendingOnReadySandbox("review this pull request")(ctx, t, f)
				revokeRepoForActorTest(ctx, t, f.pool, f.repoFullName)
				return created
			},
			end: ensureDispatchedEnding, want: sqlcgen.TurnStatusFailed, wantReason: "entitlement revoked by an administrator", release: restore,
		},
		{
			name: "a cohort rollout that no longer admits the repository",
			seed: func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
				if _, err := narvipg.NewRepoSettingsStore(f.pool).UpsertSessionsEnabled(ctx, f.repoFullName, false); err != nil {
					t.Fatalf("de-enroll the repository: %v", err)
				}
				return pendingOnReadySandbox("review this pull request")(ctx, t, f)
			},
			mode: rollout.ModeCohort,
			end:  ensureDispatchedEnding, want: sqlcgen.TurnStatusFailed, wantReason: "not enrolled in cohort rollout",
		},
		{
			name: "a prompt over its gen's frame bound",
			// The gen stated no read limit and no prompt receipt, so its
			// bound is the library's 32 KiB.
			seed: pendingOnReadySandbox(strings.Repeat("x", 2*platform.DefaultFrameReadLimitBytes)),
			end:  ensureDispatchedEnding, want: sqlcgen.TurnStatusFailed, wantReason: "prompt frame of",
		},
		{
			name: "a spawn refused for a revoked repository",
			seed: func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
				created := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", nil, time.Time{})
				revokeRepoForActorTest(ctx, t, f.pool, f.repoFullName)
				return created
			},
			end: ensureDispatchedEnding, want: sqlcgen.TurnStatusFailed, wantReason: "entitlement revoked by an administrator", release: restore,
		},
		{
			name: "the stop timer",
			seed: stopped,
			end: func(ctx context.Context, t *testing.T, _ *holdFixture, rig *holdRig) {
				if err := rig.actor.Send(ctx, TimerFired{Name: TimerStop}); err != nil {
					t.Fatalf("Send TimerFired: %v", err)
				}
			},
			want: sqlcgen.TurnStatusCancelled, wantReason: "stopped",
		},
		{name: "the dispatch's stop gate", seed: stopped, end: ensureDispatchedEnding, want: sqlcgen.TurnStatusCancelled, wantReason: "stopped", wantStopTimer: true},
		{
			name:   "a model only a personal link could run",
			linked: true,
			seed: func(ctx context.Context, t *testing.T, f *holdFixture) sqlcgen.Turn {
				seedReadySandbox(ctx, t, f.pool, f.sessionID)
				return holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", &openaiModel, time.Time{})
			},
			end: ensureDispatchedEnding, want: sqlcgen.TurnStatusFailed, wantReason: string(providercredential.RefusalPersonalLinkOnly),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creator pgtype.UUID
			if tc.linked {
				creator = createLinkedMember(ctx, t, pool, fmt.Sprintf("hold-%d", i))
				if _, err := pool.Exec(ctx, `DELETE FROM provider_credentials WHERE scope = 'global'`); err != nil {
					t.Fatalf("clear global credentials: %v", err)
				}
				createGlobalCredential(ctx, t, pool, sqlcgen.ProviderCredentialProviderAnthropic)
			}
			f := newHoldFixture(ctx, t, pool, fmt.Sprintf("acme/hold-end-%d", i), int32(300+i), creator)
			open := tc.seed(ctx, t, f)
			f.armHeldDebounce(ctx, t)
			armedBefore, _, _, _ := f.debounce(ctx, t)

			rig := newHoldRig(ctx, t, pool, f.sessionID, nil, tc.mode)
			if tc.arm != nil {
				tc.arm(rig)
			}
			if held, err := f.turns.ReviewRetriggerHeld(ctx, f.sessionID); err != nil || !held {
				t.Fatalf("before the ending: held %v (err %v), want held", held, err)
			}

			tc.end(ctx, t, f, rig)
			waitUntil(t, 5*time.Second, func() bool {
				got, err := f.turns.Get(ctx, open.ID)
				return err == nil && turn.IsTerminal(turn.State(got.Status))
			})
			f.assertEndedAndWoken(ctx, t, open.ID, tc.want)
			woken, _, _, _ := f.debounce(ctx, t)
			if !woken.CreatedAt.Time.Equal(armedBefore.CreatedAt.Time) {
				t.Errorf("the wake-up moved created_at from %v to %v: a stop compares with it", armedBefore.CreatedAt.Time, woken.CreatedAt.Time)
			}
			if !woken.ArmedAt.Time.After(armedBefore.ArmedAt.Time) {
				t.Errorf("the wake-up left armed_at at %v: every arm stamps it", woken.ArmedAt.Time)
			}

			// The ending took the path the case names.
			reasons := syntheticEndReasons(ctx, t, pool, f.sessionID)
			switch {
			case tc.wantReason == "" && len(reasons) != 0:
				t.Fatalf("synthetic ends %q, want none: a real execution_complete ended the turn", reasons)
			case tc.wantReason != "" && (len(reasons) != 1 || !strings.Contains(reasons[0], tc.wantReason)):
				t.Fatalf("synthetic ends %q, want one naming %q", reasons, tc.wantReason)
			}
			if tc.wantSends >= 0 {
				if got := rig.commander.callCount(); got != tc.wantSends {
					t.Fatalf("prompts sent = %d, want %d", got, tc.wantSends)
				}
			}
			if got := rig.provider.callCount(); got != 0 {
				t.Fatalf("sandboxes created = %d, want 0", got)
			}
			_, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerStop})
			if stopArmed := err == nil; stopArmed != tc.wantStopTimer {
				t.Fatalf("stop timer armed after the ending = %v (err %v), want %v", stopArmed, err, tc.wantStopTimer)
			}

			if tc.release != nil {
				tc.release(ctx, t, f, rig)
			}
			pumpUntilDebounceHandled(ctx, t, rig, f)
			if got := f.automaticReviews(ctx, t); got != 1 {
				t.Fatalf("automatic reviews of %s after the pump = %d, want exactly 1", holdLiveHead, got)
			}
			row := f.prSession(ctx, t)
			if row.PendingRetriggerHeadSha != nil || row.AutoRetriggerCount != 1 {
				t.Fatalf("after the review was queued: pending %v, count %d; want cleared and 1", row.PendingRetriggerHeadSha, row.AutoRetriggerCount)
			}
		})
	}
}

// TestReviewRetriggerHold_AStopDisarmsADebounceArmedBeforeIt: a debounce
// armed before a person's stop is work the stop drops (technical plan
// §3.3). The wake-up a cancelled turn's end owes it never brings it back:
// after the stop timer it is gone, and when the dispatch's stop gate
// cancels the flagged turn first, it stays where the hold left it, not
// due, until the stop timer deletes it. No automatic review runs either
// way.
func TestReviewRetriggerHold_AStopDisarmsADebounceArmedBeforeIt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, tc := range []struct {
		name string
		gate bool
	}{
		{name: "the stop timer cancels the turn"},
		{name: "the dispatch's stop gate cancels the turn first", gate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFixture(ctx, t, pool, fmt.Sprintf("acme/hold-stop-%d", i), int32(400+i), pgtype.UUID{})
			seedReadySandbox(ctx, t, pool, f.sessionID)
			open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", nil, time.Time{})
			f.armHeldDebounce(ctx, t)
			armed, _, _, _ := f.debounce(ctx, t)
			requestStop(ctx, t, pool, f.sessionID)
			rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

			if tc.gate {
				sendEnsureDispatched(ctx, t, rig.actor)
				waitForTurnStatus(ctx, t, f.turns, open.ID, sqlcgen.TurnStatusCancelled)
				row, due, held, ok := f.debounce(ctx, t)
				if !ok || due || !held {
					t.Fatalf("after the gate cancelled the flagged turn: debounce armed %v due %v held %v; want it left the backstop out", ok, due, held)
				}
				if !row.FiresAt.Time.Equal(armed.FiresAt.Time) {
					t.Fatalf("the gate's cancel moved the debounce from %v to %v: a debounce armed before the stop is never woken", armed.FiresAt.Time, row.FiresAt.Time)
				}
			}

			if err := rig.actor.Send(ctx, TimerFired{Name: TimerStop}); err != nil {
				t.Fatalf("Send TimerFired: %v", err)
			}
			waitForTurnStatus(ctx, t, f.turns, open.ID, sqlcgen.TurnStatusCancelled)
			waitUntil(t, 5*time.Second, func() bool {
				_, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerStop})
				return errors.Is(err, pgx.ErrNoRows)
			})
			if _, _, _, ok := f.debounce(ctx, t); ok {
				t.Fatal("the debounce armed before the stop survived it")
			}
			for range 3 {
				if err := rig.registry.PumpOnce(ctx); err != nil {
					t.Fatalf("PumpOnce: %v", err)
				}
			}
			time.Sleep(100 * time.Millisecond)
			if got := f.automaticReviews(ctx, t); got != 0 {
				t.Fatalf("automatic reviews after the stop = %d, want 0", got)
			}
			if _, _, _, ok := f.debounce(ctx, t); ok {
				t.Fatal("a debounce came back after the stop")
			}
		})
	}
}

// insertingFetcher is a review diff fetcher that, the first time the fire
// reads the pull request -- with no transaction open, between its decision
// and its insert -- commits a person's turn on the session the way every
// writer outside the actor does: under the session's actor-epoch lock,
// through CreateAndArmDispatch.
type insertingFetcher struct {
	*fakeReviewDiffFetcher
	pool      *pgxpool.Pool
	sessionID pgtype.UUID

	mu       sync.Mutex
	inserted bool
	err      error
}

func (f *insertingFetcher) GetPullRequest(ctx context.Context, owner, repo string, prNumber int32, token string) (githubapi.PullRequest, error) {
	f.mu.Lock()
	if !f.inserted {
		f.inserted = true
		f.err = f.insertPersonsTurn(ctx)
	}
	f.mu.Unlock()
	return f.fakeReviewDiffFetcher.GetPullRequest(ctx, owner, repo, prNumber, token)
}

// insertErr is the person's turn insert's error, read under the lock.
func (f *insertingFetcher) insertErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *insertingFetcher) insertPersonsTurn(ctx context.Context) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := narvipg.NewSessionStore(f.pool).WithTx(tx).GetActorEpochForUpdate(ctx, f.sessionID); err != nil {
		return err
	}
	prompt := "@narvi-bot why is this flagged?"
	if _, err := narvipg.NewTurnStore(f.pool).WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{
		SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
	}, sessionguard.AdmitNewSession(f.sessionID.Bytes)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestReviewRetriggerHold_ATurnQueuedDuringTheFetchHoldsTheReview: the
// hold is read again at the insert. A person's turn committed while the
// fire fetched the pull request -- after it decided to review, before it
// inserted -- holds the lane: no automatic turn, no budget spent, the
// pushed head kept and the debounce re-armed the backstop out.
func TestReviewRetriggerHold_ATurnQueuedDuringTheFetchHoldsTheReview(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/hold-fetch", 500, pgtype.UUID{})
	f.armDebounce(ctx, t, time.Now())
	fetcher := &insertingFetcher{
		fakeReviewDiffFetcher: &fakeReviewDiffFetcher{nextHeadSHA: holdLiveHead, nextBaseRef: "main", nextDiff: oneLineReadableDiff},
		pool:                  pool, sessionID: f.sessionID,
	}
	rig := newHoldRig(ctx, t, pool, f.sessionID, fetcher)

	fireHeldDebounce(ctx, t, rig, f)
	if err := fetcher.insertErr(); err != nil {
		t.Fatalf("insert the person's turn during the fetch: %v", err)
	}
	if got := fetcher.prCalls(); got != 1 {
		t.Fatalf("GetPullRequest calls = %d, want 1: the fire decided to review before the person's turn existed", got)
	}
	turns, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].IsReviewAttempt {
		t.Fatalf("turns = %d (first a review attempt: %v), want only the person's", len(turns), len(turns) > 0 && turns[0].IsReviewAttempt)
	}
	row := f.prSession(ctx, t)
	if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != holdPushedHead || row.AutoRetriggerCount != 0 {
		t.Fatalf("pending %v, count %d; want %s kept and nothing spent", row.PendingRetriggerHeadSha, row.AutoRetriggerCount, holdPushedHead)
	}
}

// TestReviewRetriggerHold_HeadsMatchAndBudgetStillDecideWhileHeld: the
// hold is read after the heads comparison and the budget, so with a turn
// open a head already reviewed still clears the pending head and deletes
// the debounce, and a spent budget still posts its one notice, clears and
// deletes -- neither is held for later.
func TestReviewRetriggerHold_HeadsMatchAndBudgetStillDecideWhileHeld(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, tc := range []struct {
		name       string
		seed       func(ctx context.Context, t *testing.T, f *holdFixture)
		wantNotice int
	}{
		{name: "the pushed head is already reviewed", seed: func(ctx context.Context, t *testing.T, f *holdFixture) {
			if _, err := narvipg.NewReviewVerdictStore(f.pool).Insert(ctx, sqlcgen.InsertReviewVerdictParams{
				RepoFullName: f.repoFullName, PrNumber: f.prNumber, HeadSha: holdPushedHead, RiskLevel: "low", Premise: "ok",
				BlastRadius: []byte(`[]`), FilesChanged: 1, TestsCoverage: "adequate", DocsDrift: "none",
				ProposedShippable: "auto", Shippable: "auto", SessionID: f.sessionID,
				ArchDecisionTags: []byte(`[]`), ArchDecisionRoots: []byte(`[]`), AncestorChain: []byte(`[]`),
			}); err != nil {
				t.Fatalf("insert the verdict of the pushed head: %v", err)
			}
		}},
		{name: "the budget is spent", seed: func(ctx context.Context, t *testing.T, f *holdFixture) {
			if _, err := f.pool.Exec(ctx, `UPDATE github_pr_sessions SET auto_retrigger_count = $3 WHERE repo_full_name = $1 AND pr_number = $2`,
				f.repoFullName, f.prNumber, ReviewAutoRetriggerBudget); err != nil {
				t.Fatalf("spend the budget: %v", err)
			}
		}, wantNotice: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFixture(ctx, t, pool, fmt.Sprintf("acme/hold-precedence-%d", i), int32(600+i), pgtype.UUID{})
			open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", nil, time.Time{})
			tc.seed(ctx, t, f)
			f.armDebounce(ctx, t, time.Now())
			rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

			if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
				t.Fatalf("Send TimerFired: %v", err)
			}
			waitUntil(t, 5*time.Second, func() bool {
				_, _, _, ok := f.debounce(ctx, t)
				return !ok
			})
			if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha != nil {
				t.Fatalf("pending head = %v, want cleared", *row.PendingRetriggerHeadSha)
			}
			if got := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, f.sessionID, string(ports.NotificationKindGitHubVerdict)); got != tc.wantNotice {
				t.Fatalf("budget notices = %d, want %d", got, tc.wantNotice)
			}
			turns, err := f.turns.ListForSession(ctx, f.sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 1 || turns[0].ID != open.ID {
				t.Fatalf("turns = %d, want only the open one", len(turns))
			}
			if rig.fetcher.prCalls() != 0 {
				t.Fatalf("GetPullRequest calls = %d, want 0", rig.fetcher.prCalls())
			}
		})
	}
}

// TestReviewRetriggerHold_BackstopFiringHoldsAgain: a firing at the
// backstop -- the wake-up was lost -- with the turn still open holds
// again: re-armed the backstop out, created_at kept, the pushed head kept,
// nothing fetched, inserted or spent.
func TestReviewRetriggerHold_BackstopFiringHoldsAgain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/hold-backstop", 700, pgtype.UUID{})
	seedReadySandbox(ctx, t, pool, f.sessionID)
	open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now())
	f.armDebounce(ctx, t, time.Now())
	first, _, _, _ := f.debounce(ctx, t)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

	for firing := 1; firing <= 2; firing++ {
		if firing == 2 {
			if _, err := pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() WHERE session_id = $1 AND name = $2`, f.sessionID, TimerReviewRetriggerDebounce); err != nil {
				t.Fatalf("bring the backstop due: %v", err)
			}
		}
		fireHeldDebounce(ctx, t, rig, f)
		row, _, _, _ := f.debounce(ctx, t)
		if !row.CreatedAt.Time.Equal(first.CreatedAt.Time) {
			t.Fatalf("firing %d: created_at moved from %v to %v", firing, first.CreatedAt.Time, row.CreatedAt.Time)
		}
		pr := f.prSession(ctx, t)
		if pr.PendingRetriggerHeadSha == nil || *pr.PendingRetriggerHeadSha != holdPushedHead || pr.AutoRetriggerCount != 0 {
			t.Fatalf("firing %d: pending %v, count %d; want %s kept and nothing spent", firing, pr.PendingRetriggerHeadSha, pr.AutoRetriggerCount, holdPushedHead)
		}
		turns, err := f.turns.ListForSession(ctx, f.sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 1 || turns[0].ID != open.ID || turns[0].Status != sqlcgen.TurnStatusProcessing {
			t.Fatalf("firing %d: %d turns, want only the open review, still processing", firing, len(turns))
		}
		if rig.fetcher.prCalls() != 0 {
			t.Fatalf("firing %d: GetPullRequest calls = %d, want 0", firing, rig.fetcher.prCalls())
		}
	}
}

// TestReviewRetriggerHold_HeldMatchesTurnIsTerminal: the hold's SQL and
// turn.IsTerminal read the same states as open. For every value of the
// turn_status enum, a session whose one turn is in it is held exactly when
// the domain does not call that state terminal; a session with no turn is
// not held.
func TestReviewRetriggerHold_HeldMatchesTurnIsTerminal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rows, err := pool.Query(ctx, `SELECT unnest(enum_range(NULL::turn_status))::text`)
	if err != nil {
		t.Fatal(err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 6 {
		t.Fatalf("turn_status has %d values, want the six this test was written against: %v", len(states), states)
	}
	turns := narvipg.NewTurnStore(pool)

	empty := createTestSession(ctx, t, pool)
	if held, err := turns.ReviewRetriggerHeld(ctx, empty); err != nil || held {
		t.Fatalf("a session with no turn: held %v (err %v), want not held", held, err)
	}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			if _, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatus(state)}); err != nil {
				t.Fatalf("create a %s turn: %v", state, err)
			}
			held, err := turns.ReviewRetriggerHeld(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if want := !turn.IsTerminal(turn.State(state)); held != want {
				t.Fatalf("held = %v with one %s turn, want %v (turn.IsTerminal = %v)", held, state, want, !want)
			}
		})
	}
}

// TestTransact_ATurnEndWakesTheDebounceInItsOwnTransaction pins where the
// wake-up runs: inside transact, in the transaction of the write that
// ended the turn. A transaction that ends a turn and then fails leaves the
// debounce as it was; one that commits leaves it due, written by the same
// transaction as the turn. A write that ends nothing wakes nothing, and a
// later transaction that writes no end does not inherit the earlier one's.
func TestTransact_ATurnEndWakesTheDebounceInItsOwnTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/hold-transact", 800, pgtype.UUID{})
	open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", nil, time.Time{})
	f.armHeldDebounce(ctx, t)
	before, _, _, _ := f.debounce(ctx, t)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
	a := rig.actor

	end := func(status sqlcgen.TurnStatus) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
				ID: open.ID, Status: status, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			})
			return err
		}
	}
	assertUntouched := func(stage string) {
		t.Helper()
		row, due, _, ok := f.debounce(ctx, t)
		if !ok || due || !row.FiresAt.Time.Equal(before.FiresAt.Time) || !row.ArmedAt.Time.Equal(before.ArmedAt.Time) {
			t.Fatalf("%s: debounce armed %v due %v fires %v armed_at %v; want it untouched at %v", stage, ok, due, row.FiresAt.Time, row.ArmedAt.Time, before.FiresAt.Time)
		}
	}

	failed := errors.New("a later write failed")
	if err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := end(sqlcgen.TurnStatusFailed)(ctx, tx); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("transact = %v, want the fn's error", err)
	}
	assertUntouched("the end rolled back")
	if got, err := f.turns.Get(ctx, open.ID); err != nil || got.Status != sqlcgen.TurnStatusPending {
		t.Fatalf("the rolled-back end: turn %v (err %v), want still pending", got.Status, err)
	}

	if err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: open.ID, Status: sqlcgen.TurnStatusPending})
		return err
	}); err != nil {
		t.Fatalf("transact: %v", err)
	}
	assertUntouched("a write that ends nothing")

	if err := a.transact(ctx, end(sqlcgen.TurnStatusCompleted)); err != nil {
		t.Fatalf("transact: %v", err)
	}
	f.assertEndedAndWoken(ctx, t, open.ID, sqlcgen.TurnStatusCompleted)

	f.armHeldDebounce(ctx, t)
	before, _, _, _ = f.debounce(ctx, t)
	if err := a.transact(ctx, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("transact: %v", err)
	}
	assertUntouched("the next transaction, which ends nothing")
}

// TestReviewRetriggerWake_TheTimerStoreWakesThisKind pins the kind the
// hold's and the wake-up's SQL name (TimerStore.HoldReviewRetriggerDebounce
// and WakeReviewRetriggerDebounce) to TimerReviewRetriggerDebounce, the
// kind this package arms, classifies as creating a turn and handles: the
// hold moves that row exactly the backstop past a new armed_at, keeping
// created_at, the wake-up moves it to the database's now, neither touches
// another kind's row, and neither inserts one where the session has none.
func TestReviewRetriggerWake_TheTimerStoreWakesThisKind(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/hold-kind", 900, pgtype.UUID{})

	timeouts := platform.DefaultTimeouts()
	backstop, lead := timeouts.ReviewRetriggerHoldBackstop, heldDebounceLead(timeouts)

	if n, err := f.timers.HoldReviewRetriggerDebounce(ctx, f.sessionID, backstop); err != nil || n != 0 {
		t.Fatalf("holding a session with no debounce: %d rows (err %v), want 0", n, err)
	}
	if n, err := f.timers.WakeReviewRetriggerDebounce(ctx, f.sessionID, lead); err != nil || n != 0 {
		t.Fatalf("waking a session with no debounce: %d rows (err %v), want 0", n, err)
	}
	if _, _, _, ok := f.debounce(ctx, t); ok {
		t.Fatal("the hold or the wake-up inserted a debounce")
	}

	f.armDebounce(ctx, t, time.Now())
	armed, _, _, _ := f.debounce(ctx, t)
	later := time.Now().Add(time.Hour)
	if _, err := f.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: f.sessionID, Name: TimerTurnDeadline, FiresAt: pgtype.Timestamptz{Time: later, Valid: true}}); err != nil {
		t.Fatalf("arm another kind: %v", err)
	}
	if n, err := f.timers.HoldReviewRetriggerDebounce(ctx, f.sessionID, backstop); err != nil || n != 1 {
		t.Fatalf("HoldReviewRetriggerDebounce = %d rows (err %v), want 1", n, err)
	}
	var exactlyBackstop bool
	if err := pool.QueryRow(ctx, `SELECT fires_at - armed_at = make_interval(secs => $3) AND armed_at > $4
		FROM session_timers WHERE session_id = $1 AND name = $2`,
		f.sessionID, TimerReviewRetriggerDebounce, backstop.Seconds(), armed.ArmedAt).Scan(&exactlyBackstop); err != nil || !exactlyBackstop {
		t.Fatalf("the held row: fires_at exactly the backstop past a new armed_at %v (err %v), want true", exactlyBackstop, err)
	}
	if held, _, _, _ := f.debounce(ctx, t); !held.CreatedAt.Time.Equal(armed.CreatedAt.Time) {
		t.Fatalf("the hold moved created_at from %v to %v", armed.CreatedAt.Time, held.CreatedAt.Time)
	}
	if n, err := f.timers.WakeReviewRetriggerDebounce(ctx, f.sessionID, lead); err != nil || n != 1 {
		t.Fatalf("WakeReviewRetriggerDebounce = %d rows (err %v), want 1", n, err)
	}
	if _, due, _, ok := f.debounce(ctx, t); !ok || !due {
		t.Fatalf("the %q row after the wake-up: armed %v due %v, want due", TimerReviewRetriggerDebounce, ok, due)
	}
	other, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerTurnDeadline})
	if err != nil || !other.FiresAt.Time.Equal(later.Truncate(time.Microsecond)) {
		t.Fatalf("another kind after the hold and the wake-up: fires %v (err %v), want untouched at %v", other.FiresAt.Time, err, later)
	}
	if work, ok := ClassifyTimer(TimerReviewRetriggerDebounce); !ok || work != TimerWorkCreatesTurn {
		t.Fatalf("ClassifyTimer(%q) = %v, %v; want the kind that creates a turn", TimerReviewRetriggerDebounce, work, ok)
	}
	if !HasOwnTimerHandlerForTest(TimerReviewRetriggerDebounce) {
		t.Fatal("the debounce kind has no handler of its own: the wake-up would deliver it to the unknown-kind path")
	}
}

// TestReviewRetriggerWake_OnlyAHeldDebounceIsWoken: a turn's end wakes the
// debounce the hold re-armed, and leaves a push's quiet window (technical
// plan §24.2) to run out while it runs, so a burst of pushes that
// straddles the end still reviews once, at its last head. The mark is how
// far past its last arm the row fires (heldDebounceLead), never how soon
// it fires: a held row is woken however close to its backstop the turn
// ends, and when the pump has claimed it at its backstop; a push's row is
// left alone just armed, about to run out, and with its replica's clock a
// second ahead of the database's. A push's row the pump claimed once its
// window ran out reads as held too -- the claim moves fires_at alone -- and
// waking it is harmless: the next pump tick delivers it, its firing queues
// the one review and clears the pending head, and the claim's own delivery,
// arriving after, finds nothing pending and queues nothing more. Each case
// ends an open turn through transact, the path every writer takes.
func TestReviewRetriggerWake_OnlyAHeldDebounceIsWoken(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	timeouts := platform.DefaultTimeouts()
	backstop, window := timeouts.ReviewRetriggerHoldBackstop.Seconds(), timeouts.ReviewRetriggerDebounce.Seconds()

	for i, tc := range []struct {
		name string
		// arm leaves the session's debounce in the case's state.
		arm      func(ctx context.Context, t *testing.T, f *holdFixture)
		wantWoke bool
		// then runs after the end, on a woken row.
		then func(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig)
	}{
		{name: "held, just re-armed by the hold", wantWoke: true, arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
			f.armDebounce(ctx, t, time.Now())
			if n, err := f.timers.HoldReviewRetriggerDebounce(ctx, f.sessionID, timeouts.ReviewRetriggerHoldBackstop); err != nil || n != 1 {
				t.Fatalf("hold: %d rows (err %v)", n, err)
			}
		}},
		{name: "held, its backstop five seconds away", wantWoke: true, arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
			f.setDebounceLead(ctx, t, backstop-5, backstop)
		}},
		{name: "held, claimed by the pump at its backstop", wantWoke: true, arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
			f.setDebounceLead(ctx, t, backstop, backstop+timeouts.TimerClaimDuration.Seconds())
		}},
		{
			name: "a push's row the pump claimed once its window ran out", wantWoke: true,
			arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
				// Armed a window and a second ago; the claim moved fires_at
				// TimerClaimDuration past the window's end.
				f.setDebounceLead(ctx, t, window+1, window+timeouts.TimerClaimDuration.Seconds())
			},
			then: func(ctx context.Context, t *testing.T, f *holdFixture, rig *holdRig) {
				pumpUntilDebounceHandled(ctx, t, rig, f)
				// The claim's own delivery, handled after: nothing pending.
				if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
					t.Fatalf("Send TimerFired: %v", err)
				}
				// A frame the actor refuses replies at once: a barrier
				// behind the delivery above, the actor handling commands
				// in order.
				sendSandboxEventForTest(ctx, t, rig.actor, SandboxEvent{})
				if got := f.automaticReviews(ctx, t); got != 1 {
					t.Fatalf("automatic reviews after the woken claimed row and its own delivery = %d, want exactly 1", got)
				}
				if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha != nil || row.AutoRetriggerCount != 1 {
					t.Fatalf("pending %v, count %d; want cleared and one slot spent", row.PendingRetriggerHeadSha, row.AutoRetriggerCount)
				}
				if _, _, _, ok := f.debounce(ctx, t); ok {
					t.Fatal("the debounce is still armed after its review was queued")
				}
			},
		},
		{name: "a push's quiet window, just armed by the webhook's own write", arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
			f.armDebounce(ctx, t, time.Now().Add(timeouts.ReviewRetriggerDebounce))
		}},
		{name: "a push's quiet window five seconds from running out", arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
			f.setDebounceLead(ctx, t, window-5, window)
		}},
		{name: "a push's quiet window armed by a replica a second ahead", arm: func(ctx context.Context, t *testing.T, f *holdFixture) {
			f.setDebounceLead(ctx, t, 0, window+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFixture(ctx, t, pool, fmt.Sprintf("acme/hold-wake-%d", i), int32(1000+i), pgtype.UUID{})
			open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusPending, "review this pull request", nil, time.Time{})
			tc.arm(ctx, t, f)
			before, _, _, _ := f.debounce(ctx, t)
			rig := newHoldRig(ctx, t, pool, f.sessionID, nil)
			a := rig.actor

			if err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
					ID: open.ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
				})
				return err
			}); err != nil {
				t.Fatalf("end the turn: %v", err)
			}
			if tc.wantWoke {
				f.assertEndedAndWoken(ctx, t, open.ID, sqlcgen.TurnStatusCompleted)
				if tc.then != nil {
					tc.then(ctx, t, f, rig)
				}
				return
			}
			after, due, _, ok := f.debounce(ctx, t)
			if !ok || due || !after.FiresAt.Time.Equal(before.FiresAt.Time) || !after.ArmedAt.Time.Equal(before.ArmedAt.Time) {
				t.Fatalf("a push's window after the turn ended: armed %v due %v fires %v (was %v); want it left to run out", ok, due, after.FiresAt.Time, before.FiresAt.Time)
			}
		})
	}
}

// setDebounceLead arms the session's debounce, last armed agoSeconds ago on
// the database's clock and firing leadSeconds after that arm.
func (f *holdFixture) setDebounceLead(ctx context.Context, t *testing.T, agoSeconds, leadSeconds float64) {
	t.Helper()
	f.armDebounce(ctx, t, time.Now())
	if _, err := f.pool.Exec(ctx, `UPDATE session_timers
		SET armed_at = now() - make_interval(secs => $3),
		    fires_at = now() - make_interval(secs => $3) + make_interval(secs => $4)
		WHERE session_id = $1 AND name = $2`, f.sessionID, TimerReviewRetriggerDebounce, agoSeconds, leadSeconds); err != nil {
		t.Fatalf("set the debounce's lead: %v", err)
	}
}

// TestReviewRetriggerWake_TheStopRuleComparesCreatedAt: the hold's re-arm
// and the wake-up leave alone a debounce first armed at or before the
// session's standing stop request -- the row the stop timer deletes
// (disarmWorkCreatingTimers) -- however recently it was last armed, and
// move one first armed after it. The rule reads created_at, which no
// re-arm moves, never armed_at, which a push after the stop moves past the
// request.
func TestReviewRetriggerWake_TheStopRuleComparesCreatedAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	timeouts := platform.DefaultTimeouts()

	for i, tc := range []struct {
		name      string
		afterStop bool
	}{
		{name: "first armed before the stop, last armed after it"},
		{name: "first armed after the stop", afterStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFixture(ctx, t, pool, fmt.Sprintf("acme/hold-stop-rule-%d", i), int32(1100+i), pgtype.UUID{})
			if !tc.afterStop {
				f.armDebounce(ctx, t, time.Now())
			}
			requestStop(ctx, t, pool, f.sessionID)
			if tc.afterStop {
				f.armDebounce(ctx, t, time.Now())
			}
			// Last armed after the stop, held: the mark the wake-up reads.
			f.setDebounceLead(ctx, t, 0, timeouts.ReviewRetriggerHoldBackstop.Seconds())
			var order string
			if err := pool.QueryRow(ctx, `SELECT CASE WHEN st.created_at <= s.stop_requested_at THEN 'created before' ELSE 'created after' END
				|| CASE WHEN st.armed_at > s.stop_requested_at THEN ', armed after' ELSE ', armed before' END
				FROM session_timers st JOIN sessions s ON s.id = st.session_id WHERE st.session_id = $1 AND st.name = $2`,
				f.sessionID, TimerReviewRetriggerDebounce).Scan(&order); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{false: "created before, armed after", true: "created after, armed after"}[tc.afterStop]; order != want {
				t.Fatalf("the debounce is %s the stop, want %s", order, want)
			}

			want := int64(0)
			if tc.afterStop {
				want = 1
			}
			if n, err := f.timers.HoldReviewRetriggerDebounce(ctx, f.sessionID, timeouts.ReviewRetriggerHoldBackstop); err != nil || n != want {
				t.Fatalf("HoldReviewRetriggerDebounce = %d rows (err %v), want %d", n, err, want)
			}
			if n, err := f.timers.WakeReviewRetriggerDebounce(ctx, f.sessionID, heldDebounceLead(timeouts)); err != nil || n != want {
				t.Fatalf("WakeReviewRetriggerDebounce = %d rows (err %v), want %d", n, err, want)
			}
		})
	}
}

// TestReviewRetriggerHold_ALateFiringNeverBringsBackADebounceAStopDeleted:
// the pump claims a held debounce, and before its firing reaches the actor
// a person's stop is handled -- the review is still inside its stop grace,
// so it runs on, and the stop timer deletes the debounce. The late firing
// finds the review open and holds, and its re-arm is an update: the row the
// stop deleted stays deleted. When the review then ends, nothing is woken,
// and no automatic review runs after the person's stop.
func TestReviewRetriggerHold_ALateFiringNeverBringsBackADebounceAStopDeleted(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/hold-late-firing", 1200, pgtype.UUID{})
	seedReadySandbox(ctx, t, pool, f.sessionID)
	open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now())
	f.armHeldDebounce(ctx, t)
	requestStop(ctx, t, pool, f.sessionID)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

	if err := rig.actor.Send(ctx, TimerFired{Name: TimerStop}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		_, _, _, ok := f.debounce(ctx, t)
		return !ok
	})
	if got, err := f.turns.Get(ctx, open.ID); err != nil || got.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("the review after the stop timer: %v (err %v), want still processing inside its grace", got.Status, err)
	}

	// The late firing, then the review's end, in the actor's order.
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	executionCompleteEnding(sandboxws.ExecutionCompleteOutcomeCancelled)(ctx, t, f, rig)
	waitForTurnStatus(ctx, t, f.turns, open.ID, sqlcgen.TurnStatusCancelled)
	if _, _, _, ok := f.debounce(ctx, t); ok {
		t.Fatal("a debounce exists after the late firing and the review's end: the hold re-created the row the stop deleted")
	}
	for range 3 {
		if err := rig.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.automaticReviews(ctx, t); got != 0 {
		t.Fatalf("automatic reviews after the person's stop = %d, want 0", got)
	}
	if got := rig.fetcher.prCalls(); got != 0 {
		t.Fatalf("GetPullRequest calls = %d, want 0: the late firing held, it never fetched", got)
	}
}

// TestReviewRetriggerHold_AnOptOutOrARevocationDropsTheDebounceWhileATurnIsOpen:
// the opt-in and the revocation are read before the hold, so with a review
// running, a firing for a repository that opted out, or that an
// administrator revoked, drops the debounce as it always did -- it is never
// held for later -- and leaves the pushed head as it was.
func TestReviewRetriggerHold_AnOptOutOrARevocationDropsTheDebounceWhileATurnIsOpen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, tc := range []struct {
		name string
		seed func(ctx context.Context, t *testing.T, f *holdFixture)
	}{
		{name: "the repository opted out", seed: func(ctx context.Context, t *testing.T, f *holdFixture) {
			if _, err := narvipg.NewRepoSettingsStore(f.pool).UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, false); err != nil {
				t.Fatalf("opt out: %v", err)
			}
		}},
		{name: "an administrator revoked the repository", seed: func(ctx context.Context, t *testing.T, f *holdFixture) {
			revokeRepoForActorTest(ctx, t, f.pool, f.repoFullName)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHoldFixture(ctx, t, pool, fmt.Sprintf("acme/hold-drop-%d", i), int32(1300+i), pgtype.UUID{})
			seedReadySandbox(ctx, t, pool, f.sessionID)
			open := holdOpenTurn(ctx, t, f, sqlcgen.TurnStatusProcessing, "review this pull request", nil, time.Now())
			tc.seed(ctx, t, f)
			f.armDebounce(ctx, t, time.Now())
			rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

			if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
				t.Fatalf("Send TimerFired: %v", err)
			}
			waitUntil(t, 5*time.Second, func() bool {
				_, _, _, ok := f.debounce(ctx, t)
				return !ok
			})
			row := f.prSession(ctx, t)
			if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != holdPushedHead || row.AutoRetriggerCount != 0 {
				t.Fatalf("pending %v, count %d; want %s left as it was and nothing spent", row.PendingRetriggerHeadSha, row.AutoRetriggerCount, holdPushedHead)
			}
			turns, err := f.turns.ListForSession(ctx, f.sessionID)
			if err != nil || len(turns) != 1 || turns[0].ID != open.ID {
				t.Fatalf("%d turns (err %v), want only the open review", len(turns), err)
			}
		})
	}
}

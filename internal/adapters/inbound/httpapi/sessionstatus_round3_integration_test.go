//go:build integration

// Review round 3 of the status route (technical plan §43.20), on real
// Postgres: P2's escalation ordering pinned through the real workflow
// engine, and P1's scheduled work read in the statement's one snapshot.
package httpapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestGetSessionStatus_TurnQueuedBehindTheEscalatingTurnClosesItOnceItRuns
// is the reviewers' P2 reproduction, with the behaviour §43.20 now states:
// a turn that ran after the escalation closes it. Turn A (a custom
// workflow's tracked attempt) is processing; turn B is queued behind it
// with AlwaysQueue (a GitHub mention, a re-review) -- untracked, no step
// run, no run of its own; A fails and escalates the run in its terminal
// transaction. B was created before the escalation, but it runs after it:
// while B waits, the escalation is still the latest state (queued, the
// escalation reported); from B's dispatch on it is not (running, no gate);
// and once B completes the session reads finished -- not awaiting a person
// for an escalation that later work already ran past.
func TestGetSessionStatus_TurnQueuedBehindTheEscalatingTurnClosesItOnceItRuns(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess, customDef := customWorkflowSession(ctx, t, rig, user.ID)

	a := createTurnThroughCore(ctx, t, rig, sess.ID)
	state := transitionTurn(ctx, t, rig.turns, a.ID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, a.ID, state, turn.TriggerStartProcessing)

	b, created, cerr := httpapi.CreateTurnCore(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, rig.registry,
		sess.ID, "a mention queued behind the running turn", nil, false, false, pgtype.UUID{}, httpapi.AlwaysQueue)
	if cerr != nil || !created {
		t.Fatalf("CreateTurnCore(AlwaysQueue): created %v, %+v", created, cerr)
	}
	var bAttempts int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE turn_id = $1`, b.ID).Scan(&bAttempts); err != nil {
		t.Fatal(err)
	}
	if runs := sessionRuns(ctx, t, rig, sess.ID); len(runs) != 1 || bAttempts != 0 {
		t.Fatalf("B: %d runs, %d attempts; want B untracked -- still the one run, no attempt of its own", len(runs), bAttempts)
	}

	endProcessingTurnThroughEngine(ctx, t, rig, sess.ID, a.ID, turn.TriggerFail)
	runs := sessionRuns(ctx, t, rig, sess.ID)
	if len(runs) != 1 || runs[0].WorkflowDefinitionID != customDef || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview {
		t.Fatalf("runs = %+v, want the custom run escalated by A's failure", runs)
	}
	escalated := runs[0]

	got := getStatus(t, rig, sess.ID, cookie)
	if got.Activity != restdtos.SessionActivityActivityQueued || got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindWorkflowEscalation || got.Awaiting.Id != escalated.ID.String() {
		t.Fatalf("B still queued: activity %q awaiting %+v, want queued with the escalation reported -- B has not run yet", got.Activity, got.Awaiting)
	}

	state = transitionTurn(ctx, t, rig.turns, b.ID, turn.StatePending, turn.TriggerDispatch)
	got = getStatus(t, rig, sess.ID, cookie)
	if got.Activity != restdtos.SessionActivityActivityRunning || got.Awaiting != nil {
		t.Fatalf("B dispatched after the escalation: activity %q awaiting %+v, want running and no gate", got.Activity, got.Awaiting)
	}
	state = transitionTurn(ctx, t, rig.turns, b.ID, state, turn.TriggerStartProcessing)
	transitionTurn(ctx, t, rig.turns, b.ID, state, turn.TriggerComplete)

	got = getStatus(t, rig, sess.ID, cookie)
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.Awaiting != nil || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("B ran to completion after the escalation: activity %q settled %v awaiting %+v delay %d, want finished, settled, no gate, 300", got.Activity, got.Settled, got.Awaiting, got.SuggestedDelaySeconds)
	}
	if got.LastRun == nil || got.LastRun.TurnId != b.ID.String() {
		t.Fatalf("lastRun = %+v, want B", got.LastRun)
	}
	if runs := sessionRuns(ctx, t, rig, sess.ID); len(runs) != 1 || runs[0].Status != sqlcgen.WorkflowRunStatusNeedsReview {
		t.Fatalf("runs = %+v, want the escalated run untouched -- it is the gate that closed, not the run", runs)
	}
}

// TestGetSessionStatus_ScheduledWorkNeverObservedAsFinished_Race: a writer
// commits, over and over, each piece of work that can create a turn with
// no new input the way the code commits it -- armed in one transaction,
// the turn it creates inserted in the same transaction that disarms it --
// with a turn ending in between, while readers poll the status route. No
// committed state is ever "every turn terminal, nothing armed", so a
// reader that sees one snapshot never reads finished; the scheduled state
// is observed. Reading the armed work in a second statement, in either
// order, can straddle a commit and report a false finished.
func TestGetSessionStatus_ScheduledWorkNeverObservedAsFinished_Race(t *testing.T) {
	rig := newTestRig(t)
	user, cookie := createUserWithRole(context.Background(), t, rig, sqlcgen.UserRoleMember)
	timers := narvipg.NewTimerStore(rig.pool)

	for _, tc := range []struct {
		name string
		// setup writes the loop's starting state; cycle commits one
		// iteration, leaving a processing turn behind, and returns it.
		setup func(ctx context.Context, sessionID pgtype.UUID) error
		cycle func(ctx context.Context, sessionID, processing pgtype.UUID) (pgtype.UUID, error)
	}{
		{
			name:  "an automatic re-review: debounce armed, the turn ends, the review inserted as the debounce goes",
			setup: func(context.Context, pgtype.UUID) error { return nil },
			cycle: func(ctx context.Context, sessionID, processing pgtype.UUID) (pgtype.UUID, error) {
				// The synchronize webhook's own write.
				if _, err := timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: sessionID, Name: sessionactor.TimerReviewRetriggerDebounce, FiresAt: pgtype.Timestamptz{Time: time.Now().Add(2 * time.Minute), Valid: true}}); err != nil {
					return pgtype.UUID{}, err
				}
				if err := completeTurn(ctx, rig.turns, processing); err != nil {
					return pgtype.UUID{}, err
				}
				// The actor's enqueue: the review turn and the timer's
				// deletion in one transaction (reviewretrigger.go).
				var next sqlcgen.Turn
				if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
					var err error
					if next, err = rig.turns.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true}); err != nil {
						return err
					}
					return timers.WithTx(tx).Delete(ctx, sqlcgen.DeleteSessionTimerParams{SessionID: sessionID, Name: sessionactor.TimerReviewRetriggerDebounce})
				}); err != nil {
					return pgtype.UUID{}, err
				}
				return next.ID, startTurn(ctx, rig.turns, next.ID)
			},
		},
		{
			name: "a release manifest check: enqueued, the turn ends, claimed, the composition turn inserted, the row finished",
			setup: func(ctx context.Context, sessionID pgtype.UUID) error {
				return enqueueReleaseCheck(ctx, rig, sessionID)
			},
			cycle: func(ctx context.Context, sessionID, processing pgtype.UUID) (pgtype.UUID, error) {
				if err := completeTurn(ctx, rig.turns, processing); err != nil {
					return pgtype.UUID{}, err
				}
				if _, err := rig.pool.Exec(ctx, `UPDATE release_manifest_pending SET claimed_at = now() WHERE session_id = $1 AND claimed_at IS NULL`, sessionID); err != nil {
					return pgtype.UUID{}, err
				}
				next, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
				if err != nil {
					return pgtype.UUID{}, err
				}
				if _, err := rig.pool.Exec(ctx, `DELETE FROM release_manifest_pending WHERE session_id = $1 AND claimed_at IS NOT NULL`, sessionID); err != nil {
					return pgtype.UUID{}, err
				}
				if err := enqueueReleaseCheck(ctx, rig, sessionID); err != nil {
					return pgtype.UUID{}, err
				}
				return next.ID, startTurn(ctx, rig.turns, next.ID)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			sess := createSessionForUser(ctx, t, rig, user.ID, nil)
			first, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusProcessing})
			if err != nil {
				t.Fatalf("create the first turn: %v", err)
			}
			if err := tc.setup(ctx, sess.ID); err != nil {
				t.Fatalf("setup: %v", err)
			}

			seen := newSeen()
			stop := make(chan struct{})
			var readers errgroup.Group
			pollUntil(ctx, &readers, rig, sess.ID, cookie, stop, seen)

			processing := first.ID
			for i := 0; i < 150; i++ {
				if processing, err = tc.cycle(ctx, sess.ID, processing); err != nil {
					close(stop)
					_ = readers.Wait()
					t.Fatalf("writer cycle %d: %v", i, err)
				}
			}
			close(stop)
			if err := readers.Wait(); err != nil {
				t.Fatalf("readers: %v", err)
			}
			t.Logf("snapshots observed: running %d, queued %d, scheduled %d", seen[restdtos.SessionActivityActivityRunning].Load(), seen[restdtos.SessionActivityActivityQueued].Load(), seen[restdtos.SessionActivityActivityScheduled].Load())
			if n := seen[restdtos.SessionActivityActivityFinished].Load(); n != 0 {
				t.Fatalf("finished observed %d times: the armed work and the turns were read from different snapshots", n)
			}
			if n := seen[restdtos.SessionActivityActivityIdle].Load(); n != 0 {
				t.Fatalf("idle observed %d times", n)
			}
			if seen[restdtos.SessionActivityActivityScheduled].Load() == 0 {
				t.Fatal("scheduled never observed: the race did not exercise the window it exists for")
			}
		})
	}
}

func completeTurn(ctx context.Context, turns *narvipg.TurnStore, id pgtype.UUID) error {
	_, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: id, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
	return err
}

func startTurn(ctx context.Context, turns *narvipg.TurnStore, id pgtype.UUID) error {
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: id, Status: sqlcgen.TurnStatusDispatched, DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
		return err
	}
	_, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: id, Status: sqlcgen.TurnStatusProcessing})
	return err
}

func enqueueReleaseCheck(ctx context.Context, rig testRig, sessionID pgtype.UUID) error {
	_, err := narvipg.NewReleaseManifestPendingStore(rig.pool).Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{
		SessionID: sessionID, Owner: "example", Repo: fmt.Sprintf("race-%s", sessionID.String()), PrNumber: 1, BaseRef: "main", HeadRef: "release/x",
	})
	return err
}

// TestGetSessionStatus_DeliveryNeverObservedAsFinished_Race (review round
// 3, P4): the delivery stamp is written in the transaction that completes
// the turn (completeProcessingTurn), and the status reads it in the same
// statement as the turns. A writer loops -- the turn completed AND the
// stamp set in one transaction, the next turn queued, the stamp cleared,
// the next turn started -- while readers poll: no committed state is ever
// "every turn terminal, nothing being delivered", so finished is never
// observed and delivering is. Reading the stamp in a second statement can
// straddle the completing commit and report a false finished.
func TestGetSessionStatus_DeliveryNeverObservedAsFinished_Race(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)
	if _, err := rig.sandboxes.Create(ctx, sess.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	first, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatalf("create the first turn: %v", err)
	}

	seen := newSeen()
	stop := make(chan struct{})
	var readers errgroup.Group
	pollUntil(ctx, &readers, rig, sess.ID, cookie, stop, seen)

	fail := func(format string, args ...any) {
		t.Helper()
		close(stop)
		_ = readers.Wait()
		t.Fatalf(format, args...)
	}
	processing := first.ID
	for i := 0; i < 150; i++ {
		if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
			if err := completeTurn(ctx, rig.turns.WithTx(tx), processing); err != nil {
				return err
			}
			return rig.sandboxes.WithTx(tx).StartPRDelivery(ctx, sess.ID)
		}); err != nil {
			fail("cycle %d: complete the turn and stamp its delivery: %v", i, err)
		}
		next, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending})
		if err != nil {
			fail("cycle %d: queue the next turn: %v", i, err)
		}
		if err := rig.sandboxes.EndPRDelivery(ctx, sess.ID); err != nil {
			fail("cycle %d: end the delivery: %v", i, err)
		}
		if err := startTurn(ctx, rig.turns, next.ID); err != nil {
			fail("cycle %d: start the next turn: %v", i, err)
		}
		processing = next.ID
	}
	close(stop)
	if err := readers.Wait(); err != nil {
		t.Fatalf("readers: %v", err)
	}
	t.Logf("snapshots observed: running %d, queued %d, delivering %d", seen[restdtos.SessionActivityActivityRunning].Load(), seen[restdtos.SessionActivityActivityQueued].Load(), seen[restdtos.SessionActivityActivityDelivering].Load())
	if n := seen[restdtos.SessionActivityActivityFinished].Load(); n != 0 {
		t.Fatalf("finished observed %d times: the delivery stamp and the turns were read from different snapshots", n)
	}
	if seen[restdtos.SessionActivityActivityDelivering].Load() == 0 {
		t.Fatal("delivering never observed: the race did not exercise the window it exists for")
	}
}

//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// syntheticEndReasons returns the reason of every synthetic
// execution_complete sessionID holds, oldest first.
func syntheticEndReasons(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT COALESCE(payload->>'reason', '') FROM events
		WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean
		ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("read synthetic execution_complete events: %v", err)
	}
	var out []string
	var scanErr error
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			scanErr = err
			break
		}
		out = append(out, r)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// syntheticEndsDispatched returns, per turn id, whether that turn's
// synthetic execution_complete carries the `dispatched` stamp
// (syntheticend.go): the end of a turn that was dispatched, the one a page
// is showing.
func syntheticEndsDispatched(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT payload->>'turn_id', COALESCE((payload->>'dispatched')::boolean, false) FROM events
		WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`, sessionID)
	if err != nil {
		t.Fatalf("read synthetic execution_complete events: %v", err)
	}
	out := map[string]bool{}
	var scanErr error
	for rows.Next() {
		var id string
		var dispatched bool
		if err := rows.Scan(&id, &dispatched); err != nil {
			scanErr = err
			break
		}
		out[id] = dispatched
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	return out
}

// TestSpawnRefusal_EndsTheOpenTurnsOnce is the spawn-time policy refusal
// under the durable dispatch trigger (technical plan §2, §32.4): a repo the
// cohort rollout does not admit (no repo_settings row), or an environment
// the provider cannot honor (Docker required, none offered). The turn's
// transaction armed the dispatch timer; the evaluation is refused, and it
// ends every open turn of the session forward, with the refusal's reason,
// in its own transaction -- so the timer's delete commits and nothing
// brings the refusal back. With a short claim window and idle TTL and the
// pump ticking all along, the refusal is made (and, for the rollout,
// counted) exactly once, the turns are failed with the named reason, no
// timer is left, no sandbox is asked for, and the actor idles out.
func TestSpawnRefusal_EndsTheOpenTurnsOnce(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		mode       platform.RolloutMode
		docker     bool
		sandbox    string // "" none, "stopped", or "in flight": stopped, a turn in flight on it
		wantReason string
		counted    bool
	}{
		{"rollout, no sandbox", rollout.ModeCohort, false, "", "not enrolled in cohort rollout", true},
		{"rollout, a stopped sandbox", rollout.ModeCohort, false, "stopped", "not enrolled in cohort rollout", true},
		{"rollout, a turn in flight on a stopped sandbox and one queued", rollout.ModeCohort, false, "in flight", "not enrolled in cohort rollout", true},
		{"substrate, no sandbox", rollout.ModeOpen, true, "", "cannot honor this session's environment", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			var sessionID pgtype.UUID
			if tc.docker {
				sessionID = createTestSessionWithEnvironment(ctx, t, pool, createTestEnvironmentWithDocker(ctx, t, pool))
			} else {
				sessionID = createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/acme/refused-widgets.git", "")
			}
			turns := narvipg.NewTurnStore(pool)
			var inFlight sqlcgen.Turn
			if tc.sandbox != "" {
				if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped', gen = 1, provider_id = 'old-object' WHERE session_id = $1`, sessionID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sandbox == "in flight" {
				gen := int32(1)
				created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing})
				if err != nil {
					t.Fatal(err)
				}
				if inFlight, err = turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen}); err != nil {
					t.Fatal(err)
				}
			}
			queued := createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")

			timeouts := platform.DefaultTimeouts()
			timeouts.TimerClaimDuration = 300 * time.Millisecond
			timeouts.ActorIdleTTL = time.Second
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "never"}}
			r, err := NewRegistry(ctx, pool, timeouts, nil, nil, provider, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{RolloutMode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })

			before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sqlcgen.SessionSpawnSourceWeb))
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			sendEnsureDispatched(ctx, t, a)
			for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
				if err := r.PumpOnce(ctx); err != nil {
					t.Fatalf("PumpOnce: %v", err)
				}
				time.Sleep(100 * time.Millisecond)
			}

			refusals := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sqlcgen.SessionSpawnSourceWeb)) - before
			if want := map[bool]int64{true: 1, false: 0}[tc.counted]; refusals != want {
				t.Errorf("session_rollout_refused_total grew by %d, want %d: the refusal was re-evaluated", refusals, want)
			}
			ended := []sqlcgen.Turn{queued}
			if inFlight.ID.Valid {
				ended = append(ended, inFlight)
			}
			for _, want := range ended {
				got, err := turns.Get(ctx, want.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != sqlcgen.TurnStatusFailed {
					t.Errorf("turn %s status = %s, want failed: a turn refused a spawn stays open", want.ID.String(), got.Status)
				}
			}
			reasons := syntheticEndReasons(ctx, t, pool, sessionID)
			if len(reasons) != len(ended) {
				t.Fatalf("synthetic execution_complete events = %d (%q), want one per ended turn (%d)", len(reasons), reasons, len(ended))
			}
			for _, r := range reasons {
				if !strings.Contains(r, tc.wantReason) {
					t.Errorf("synthetic execution_complete reason %q, want it to name %q", r, tc.wantReason)
				}
			}
			// The turn in flight was dispatched, the queued one never was: only
			// the in-flight turn's end may end the turn a page shows.
			stamps := syntheticEndsDispatched(ctx, t, pool, sessionID)
			if stamps[queued.ID.String()] {
				t.Errorf("the queued turn's synthetic end is stamped dispatched, want unstamped: it never dispatched")
			}
			if inFlight.ID.Valid && !stamps[inFlight.ID.String()] {
				t.Errorf("the in-flight turn's synthetic end carries no dispatched stamp, want it")
			}
			if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
				t.Error("a dispatch timer is left after the refusal: the pump would bring the refusal back")
			}
			if got := provider.callCount(); got != 0 {
				t.Errorf("CreateSandbox calls = %d, want 0", got)
			}
			sessionRow, err := narvipg.NewSessionStore(pool).Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sessionRow.Status != sqlcgen.SessionStatusFailed {
				t.Errorf("session status = %s, want failed", sessionRow.Status)
			}
			if r.lookup(sessionID) != nil {
				t.Error("the session's actor is still registered after 4x ActorIdleTTL: something kept waking it")
			}
			if warnings := sessionWarnings(ctx, t, pool, sessionID); len(warnings) != 1 || !strings.Contains(warnings[0], "ended") {
				t.Errorf("session warnings = %q, want exactly one naming the refusal", warnings)
			}
		})
	}
}

// sessionWarnings returns the message of every warning event sessionID
// holds, oldest first.
func sessionWarnings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT COALESCE(payload->>'message', '') FROM events WHERE session_id = $1 AND type = 'warning' ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("read warning events: %v", err)
	}
	var out []string
	var scanErr error
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			scanErr = err
			break
		}
		out = append(out, m)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// outboxRows returns sessionID's outbox rows of kind, their payloads
// decoded into maps.
func outboxRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, kind ports.NotificationKind) []map[string]any {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = $2 ORDER BY id`, sessionID, string(kind))
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var raws [][]byte
	var scanErr error
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			scanErr = err
			break
		}
		raws = append(raws, raw)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	out := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode outbox payload %s: %v", raw, err)
		}
		out = append(out, m)
	}
	return out
}

// TestSpawnRefusal_NotifiesWhereTheTurnCameFrom: a turn a refused spawn
// ends is told to the channel it came from, as every other ended turn is
// (enqueueOutboxNotification). A review attempt in flight on a pull
// request's review session -- its sandbox stopped, its repository no
// longer enrolled -- closes its review check as not assessed, naming the
// refusal, where it would otherwise stay in progress for good (the ended
// turn's turn_deadline, which used to send that notice, is gone). A
// pending turn on a Slack-origin session gets the Slack failure notice.
// Each session records one warning naming the refusal and the remedy.
func TestSpawnRefusal_NotifiesWhereTheTurnCameFrom(t *testing.T) {
	ctx := context.Background()
	const repoFullName = "acme/r2-review-refused"
	repos := func(t *testing.T) []byte {
		return reposJSONForTest(t, "r2-review-refused", "https://github.com/"+repoFullName+".git", "")
	}

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn)
		check func(t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, ended sqlcgen.Turn)
	}{
		{
			name: "a review attempt in flight on a review session",
			setup: func(t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
				session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, Repos: repos(t)})
				if err != nil {
					t.Fatal(err)
				}
				claimPullRequest(ctx, t, pool, repoFullName, 7, session.ID)
				if _, err := narvipg.NewSandboxStore(pool).Create(ctx, session.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped', gen = 1, provider_id = 'old-object' WHERE session_id = $1`, session.ID); err != nil {
					t.Fatal(err)
				}
				head := "cafef00d"
				turns := narvipg.NewTurnStore(pool)
				created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &head, IsReviewAttempt: true})
				if err != nil {
					t.Fatal(err)
				}
				gen := int32(1)
				inFlight, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO session_timers (session_id, name, fires_at) VALUES ($1, $2, now() + interval '1 hour')`, session.ID, TimerTurnDeadline); err != nil {
					t.Fatal(err)
				}
				return session.ID, inFlight
			},
			check: func(t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, ended sqlcgen.Turn) {
				checks := outboxRows(ctx, t, pool, sessionID, ports.NotificationKindGitHubReviewCheck)
				if len(checks) != 1 {
					t.Fatalf("review-check outbox rows = %d (%v), want the one closing the refused attempt", len(checks), checks)
				}
				got := checks[0]
				if got["phase"] != string(reviewcheck.PhaseTerminalNotAssessed) || got["attempt_id"] != ended.ID.String() ||
					got["not_assessed_reason"] != string(reviewcheck.NotAssessedRolloutNotEnrolled) {
					t.Errorf("review-check payload = %v, want terminal_not_assessed for attempt %s naming %q", got, ended.ID.String(), reviewcheck.NotAssessedRolloutNotEnrolled)
				}
				var deadlines int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`, sessionID, TimerTurnDeadline).Scan(&deadlines); err != nil {
					t.Fatal(err)
				}
				if deadlines != 0 {
					t.Errorf("turn_deadline timers = %d, want the ended turn's deleted", deadlines)
				}
			},
		},
		{
			name: "a pending turn on a Slack-origin session",
			setup: func(t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, sqlcgen.Turn) {
				session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceSlack, Repos: repos(t)})
				if err != nil {
					t.Fatal(err)
				}
				if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C-REFUSED", "1.0001", session.ID); err != nil || !ok {
					t.Fatalf("claim slack thread: ok=%v err=%v", ok, err)
				}
				return session.ID, createTurnArmingDispatch(ctx, t, pool, session.ID, "do the thing")
			},
			check: func(t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, _ sqlcgen.Turn) {
				notices := outboxRows(ctx, t, pool, sessionID, ports.NotificationKindSlack)
				if len(notices) != 1 {
					t.Fatalf("slack outbox rows = %d (%v), want the one failure notice", len(notices), notices)
				}
				if text, _ := notices[0]["text"].(string); !strings.Contains(text, "failed") || notices[0]["channel_id"] != "C-REFUSED" {
					t.Errorf("slack notice = %v, want the failure notice in the session's thread", notices[0])
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			sessionID, ended := tc.setup(t, pool)

			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "never"}}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, provider, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{RolloutMode: rollout.ModeCohort})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			sendEnsureDispatched(ctx, t, a)
			turns := narvipg.NewTurnStore(pool)
			waitUntil(t, 5*time.Second, func() bool {
				got, err := turns.Get(ctx, ended.ID)
				return err == nil && got.Status == sqlcgen.TurnStatusFailed
			})
			// The notice and the warning commit with the turn's end.
			tc.check(t, pool, sessionID, ended)
			warnings := sessionWarnings(ctx, t, pool, sessionID)
			if len(warnings) != 1 || !strings.Contains(warnings[0], repoFullName) || !strings.Contains(warnings[0], "enroll the repository") {
				t.Errorf("session warnings = %q, want one naming the repository and the remedy", warnings)
			}
			if got := provider.callCount(); got != 0 {
				t.Errorf("CreateSandbox calls = %d, want 0", got)
			}
		})
	}
}

// TestDispatchTimer_AFailedEvaluationBacksOff: a dispatch evaluation that
// fails -- here the rollout read itself, its repo_settings table made
// unreadable, a transient refusal that must not count as policy -- rolls
// back the dispatch timer's delete, and the actor backs the timer off in a
// transaction of its own: never at the claim cadence, growing with the
// timer's age, bounded, armed_at untouched, and not counted. Once the read
// works again the next delivery dispatches and the timer is gone.
func TestDispatchTimer_AFailedEvaluationBacksOff(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoFullName := "acme/" + t.Name()
	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "widgets", "https://github.com/"+repoFullName+".git", "")
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertSessionsEnabled(ctx, repoFullName, true); err != nil {
		t.Fatal(err)
	}
	createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")

	hidden := false
	hide := func(on bool) {
		t.Helper()
		stmt := `ALTER TABLE repo_settings RENAME TO repo_settings_unreadable_by_test`
		if !on {
			stmt = `ALTER TABLE repo_settings_unreadable_by_test RENAME TO repo_settings`
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		hidden = on
	}
	t.Cleanup(func() {
		if hidden {
			if _, err := pool.Exec(context.Background(), `ALTER TABLE repo_settings_unreadable_by_test RENAME TO repo_settings`); err != nil {
				t.Errorf("restore repo_settings: %v", err)
			}
		}
	})
	hide(true)

	timeouts := platform.DefaultTimeouts()
	timeouts.TimerClaimDuration = 300 * time.Millisecond
	timeouts.DispatchRetryBackoff = 2 * time.Second
	timeouts.DispatchRetryBackoffMax = 5 * time.Second
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "after-the-blip"}}
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, provider, "http://localhost:8080", nil, nil, "", nil, false, RegistryOptions{RolloutMode: rollout.ModeCohort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	// delay reads the timer's fires_at minus the database's now, and its
	// armed_at.
	delay := func() (time.Duration, time.Time) {
		t.Helper()
		var secs float64
		var armedAt time.Time
		if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM fires_at - now())::float8, armed_at FROM session_timers WHERE session_id = $1 AND name = $2`,
			sessionID, TimerDispatch).Scan(&secs, &armedAt); err != nil {
			t.Fatalf("read the dispatch timer: %v", err)
		}
		return time.Duration(secs * float64(time.Second)), armedAt
	}
	_, armedAt := delay()
	before := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sqlcgen.SessionSpawnSourceWeb))

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { d, _ := delay(); return d > time.Second })
	first, armedAfter := delay()
	if first < timeouts.DispatchRetryBackoff-500*time.Millisecond || first > timeouts.DispatchRetryBackoff+500*time.Millisecond {
		t.Fatalf("first backoff = %v, want about DispatchRetryBackoff (%v)", first, timeouts.DispatchRetryBackoff)
	}
	if !armedAfter.Equal(armedAt) {
		t.Errorf("armed_at moved from %v to %v: a backoff is not an arm", armedAt, armedAfter)
	}

	// The pump finds nothing due within the backoff: not the claim cadence.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if d, _ := delay(); d > first || d < first-1500*time.Millisecond {
		t.Fatalf("backoff %v became %v within its own window: something re-armed or claimed it", first, d)
	}

	// Growing: a timer 3s old is backed off by its age, not the base.
	if _, err := pool.Exec(ctx, `UPDATE session_timers SET created_at = created_at - interval '3 seconds' WHERE session_id = $1 AND name = $2`, sessionID, TimerDispatch); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerDispatch}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { d, _ := delay(); return d > first+500*time.Millisecond })
	if grown, _ := delay(); grown > timeouts.DispatchRetryBackoffMax {
		t.Fatalf("second backoff = %v, want it past the first (%v) and within DispatchRetryBackoffMax", grown, first)
	}
	// Bounded: an hour-old timer waits DispatchRetryBackoffMax, no longer.
	if _, err := pool.Exec(ctx, `UPDATE session_timers SET created_at = now() - interval '1 hour' WHERE session_id = $1 AND name = $2`, sessionID, TimerDispatch); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerDispatch}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { d, _ := delay(); return d > timeouts.DispatchRetryBackoffMax-time.Second })
	if capped, _ := delay(); capped > timeouts.DispatchRetryBackoffMax {
		t.Fatalf("backoff = %v, want at most DispatchRetryBackoffMax (%v)", capped, timeouts.DispatchRetryBackoffMax)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_rollout_refused_total", "spawn_source", string(sqlcgen.SessionSpawnSourceWeb)) - before; got != 0 {
		t.Errorf("session_rollout_refused_total grew by %d over transient failures, want 0", got)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("CreateSandbox calls = %d while the read failed, want 0", got)
	}

	// The read works again: the next delivery dispatches.
	hide(false)
	if err := a.Send(ctx, TimerFired{Name: TimerDispatch}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })
	waitUntil(t, 5*time.Second, func() bool { _, ok := dispatchTimer(ctx, t, pool, sessionID); return !ok })
}

// TestDispatchTimer_ABackoffNeverPostponesAReArm: the backoff after a
// failed evaluation moves only the dispatch timer that evaluation read. A
// turn created while the evaluation holds the session's actor-epoch lock
// -- on a replica that does not host the actor, so no trigger follows --
// queues behind it, is granted the lock when the evaluation rolls back,
// and re-arms the timer due at once before the backoff's own transaction
// gets the lock. That re-arm must win: the timer stays due, not pushed out
// by DispatchRetryBackoff. The evaluation is held at its read of the turns
// and made to fail by cancelling that statement.
func TestDispatchTimer_ABackoffNeverPostponesAReArm(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createTurnArmingDispatch(ctx, t, pool, sessionID, "the first turn")

	timeouts := platform.DefaultTimeouts()
	timeouts.DispatchRetryBackoff = 20 * time.Second
	timeouts.DispatchRetryBackoffMax = time.Minute
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	lockWaiters := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		return n
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `LOCK TABLE turns IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold the turns table: %v", err)
	}

	// The evaluation: timer deleted (its armed_at read), held at the turns.
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return lockWaiters() >= 1 })
	var evaluationPID int
	if err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%FROM turns%'`).Scan(&evaluationPID); err != nil {
		t.Fatalf("find the held evaluation: %v", err)
	}

	// The writer, queued behind the evaluation's epoch lock.
	var g errgroup.Group
	g.Go(func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := narvipg.NewSessionStore(pool).WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
			return err
		}
		prompt := "the turn a non-hosting replica created"
		if _, err := narvipg.NewTurnStore(pool).WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{
			SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
		}, sessionguard.AdmitNewSession(sessionID.Bytes)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	waitUntil(t, 5*time.Second, func() bool { return lockWaiters() >= 2 })

	// The evaluation fails; the writer gets the epoch lock, then waits on
	// the turns table; the backoff queues behind the writer.
	if _, err := pool.Exec(ctx, `SELECT pg_cancel_backend($1)`, evaluationPID); err != nil {
		t.Fatalf("cancel the evaluation's read: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return lockWaiters() >= 2 })
	time.Sleep(200 * time.Millisecond)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("the writer: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return lockWaiters() == 0 })
	time.Sleep(300 * time.Millisecond)

	var dueIn float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM fires_at - now())::float8 FROM session_timers WHERE session_id = $1 AND name = $2`, sessionID, TimerDispatch).Scan(&dueIn); err != nil {
		t.Fatalf("read the dispatch timer: %v", err)
	}
	if dueIn > 1 {
		t.Fatalf("dispatch timer due in %.1fs, want due at once: the backoff of the failed evaluation postponed the re-arm of a turn created after it", dueIn)
	}
}

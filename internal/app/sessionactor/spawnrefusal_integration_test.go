//go:build integration

package sessionactor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/rollout"
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

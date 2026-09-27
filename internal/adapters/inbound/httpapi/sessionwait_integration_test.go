//go:build integration

// Integration tests for the bounded wait on real Postgres (technical plan
// §43.20, row 182's piece (b)): GET /api/sessions/{sessionID}/status
// ?waitSeconds=N, through the real cookie gate and the real
// sessionactivity.Waiter. Each test serves the status route on a router of
// its own (waitServer) -- a Waiter tuned for test time, on the pool the test
// chooses -- and seeds state through the stores, so no session actor is
// ever spawned here.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// waitServer serves GET /api/sessions/{sessionID} and its /status on pool,
// behind the real cookie gate, the status route on waiter -- the wiring
// controlplane gives both, with a pool and a Waiter the test chooses.
func waitServer(t *testing.T, pool *pgxpool.Pool, waiter *sessionactivity.Waiter) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(narvipg.NewUserSessionStore(pool), narvipg.NewUserStore(pool)))
		r.Get("/{sessionID}", httpapi.GetSession(narvipg.NewSessionStore(pool)))
		r.Get("/{sessionID}/status", httpapi.GetSessionStatus(narvipg.NewSessionStore(pool), waiter, platform.DefaultTimeouts()))
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

// testWaiter is a Waiter polling every 100 ms, for at most five seconds,
// with room for perKey waits per caller.
func testWaiter(perKey int) *sessionactivity.Waiter {
	return sessionactivity.NewWaiter(sessionactivity.Config{MaxDuration: 5 * time.Second, PollInterval: 100 * time.Millisecond, MaxPerKey: perKey, MaxPerReplica: 32})
}

// getWithCookie GETs url as the cookie's user, on ctx, and returns the
// status and the body's exact bytes.
func getWithCookie(ctx context.Context, url, cookie string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// decodeActivity decodes a status body through the generated DTO, whose
// own UnmarshalJSON enforces every required field and enum.
func decodeActivity(t *testing.T, body []byte) restdtos.SessionActivity {
	t.Helper()
	var got restdtos.SessionActivity
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return got
}

// eventuallyTrue polls cond every few milliseconds for up to d.
func eventuallyTrue(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// completeInOneTransaction drives turnID from processing to completed
// through turn.Transition and derives the session's row in the same
// transaction -- the shape of the session actor's completeProcessingTurn.
func completeInOneTransaction(ctx context.Context, t *testing.T, rig testRig, sessionID, turnID pgtype.UUID) {
	t.Helper()
	to, err := turn.Transition(turn.StateProcessing, turn.TriggerComplete)
	if err != nil {
		t.Fatalf("turn.Transition: %v", err)
	}
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		if _, err := rig.turns.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnID, Status: sqlcgen.TurnStatus(to), CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
			return err
		}
		_, err := rig.sessions.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sessionID, Status: sqlcgen.SessionStatusCompleted})
		return err
	}); err != nil {
		t.Fatalf("complete the turn: %v", err)
	}
}

// processingTurn creates a turn and drives it to processing through the
// turn machine.
func processingTurn(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) pgtype.UUID {
	t.Helper()
	row := createTurn(ctx, t, rig.turns, sessionID, false)
	state := transitionTurn(ctx, t, rig.turns, row.ID, turn.StatePending, turn.TriggerDispatch)
	transitionTurn(ctx, t, rig.turns, row.ID, state, turn.TriggerStartProcessing)
	return row.ID
}

// TestWait_HoldsNoPoolConnectionWhileSleeping (technical plan §43.20; the
// control plane's actors pin a pool connection each, so a wait must never
// add to that): on a pool of TWO connections, eight waits on a session
// that never settles all get past their first read and sleep at once --
// the pool seen with no connection out while they do -- and an unrelated
// REST read on the same pool answers promptly, repeatedly, while all eight
// are still waiting. Each wait then answers "timeout" on its own. A wait
// that kept its connection across the sleep would leave six of the eight
// (and the unrelated read) queued behind the pool.
func TestWait_HoldsNoPoolConnectionWhileSleeping(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	busy := createSessionForUser(ctx, t, rig, user.ID, nil)
	createTurn(ctx, t, rig.turns, busy.ID, false) // queued, and it stays so
	other := createSessionForUser(ctx, t, rig, user.ID, nil)

	_, connStr := httpapi.IntegrationTestPoolAndConnStr(t)
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	const waits = 8
	waiter := testWaiter(waits)
	srv := waitServer(t, small, waiter)

	var g errgroup.Group
	bodies := make([][]byte, waits)
	for i := range waits {
		g.Go(func() error {
			status, body, err := getWithCookie(ctx, srv.URL+"/api/sessions/"+busy.ID.String()+"/status?waitSeconds=3", cookie)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("wait %d: %d %s", i, status, body)
			}
			bodies[i] = body
			return nil
		})
	}
	if !eventuallyTrue(2*time.Second, func() bool { return waiter.Active() == waits }) {
		t.Fatalf("only %d of %d waits got past their first read on a two-connection pool", waiter.Active(), waits)
	}

	idle := false
	for range 50 {
		if small.Stat().AcquiredConns() == 0 {
			idle = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !idle {
		t.Fatalf("the pool never had a connection free while %d waits slept", waits)
	}
	for i := range 3 {
		start := time.Now()
		status, body, err := getWithCookie(ctx, srv.URL+"/api/sessions/"+other.ID.String(), cookie)
		if err != nil || status != http.StatusOK {
			t.Fatalf("unrelated read %d: %d %s %v", i, status, body, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("unrelated read %d took %v while %d waits slept on a two-connection pool, want an answer at once", i, took, waits)
		}
	}
	if n := waiter.Active(); n != waits {
		t.Fatalf("Active = %d after the unrelated reads, want all %d waits still blocked while they ran", n, waits)
	}

	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	for i, body := range bodies {
		got := decodeActivity(t, body)
		if got.Wait == nil || got.Wait.Reason != restdtos.SessionActivityWaitReasonTimeout || got.Activity != restdtos.SessionActivityActivityQueued || got.Settled {
			t.Fatalf("wait %d = %s, want a queued, unsettled timeout", i, body)
		}
	}
}

// TestGetSessionStatus_WaitOnRealPostgres pins the route's own contract
// for ?waitSeconds=: absent or 0 is the plain read, byte for byte but for
// observedAt (the snapshot's own clock) and with no wait object; a settled
// session answers at once, "settled" after 0 ms; a malformed or negative
// value is a 400 and an unknown session a 404, whatever else is asked; and
// a running session is waited on until its turn really ends -- finished,
// "settled", after a positive wait.
func TestGetSessionStatus_WaitOnRealPostgres(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	waiter := testWaiter(2)
	srv := waitServer(t, rig.pool, waiter)
	status := func(id pgtype.UUID, query string) (int, []byte) {
		t.Helper()
		code, body, err := getWithCookie(ctx, srv.URL+"/api/sessions/"+id.String()+"/status"+query, cookie)
		if err != nil {
			t.Fatal(err)
		}
		return code, body
	}

	idle := createSessionForUser(ctx, t, rig, user.ID, nil)
	code, plain := status(idle.ID, "")
	code0, zero := status(idle.ID, "?waitSeconds=0")
	if code != http.StatusOK || code0 != http.StatusOK || bytes.Contains(plain, []byte(`"wait"`)) || bytes.Contains(zero, []byte(`"wait"`)) {
		t.Fatalf("plain read %d %s and waitSeconds=0 %d %s, want two 200s with no wait object", code, plain, code0, zero)
	}
	p, z := decodeActivity(t, plain), decodeActivity(t, zero)
	substituted := bytes.Replace(plain, []byte(p.ObservedAt.Format(time.RFC3339Nano)), []byte(z.ObservedAt.Format(time.RFC3339Nano)), 1)
	if !bytes.Equal(substituted, zero) {
		t.Fatalf("waitSeconds=0 differs from the plain read beyond observedAt:\n%s\n%s", plain, zero)
	}

	code, body := status(idle.ID, "?waitSeconds=5")
	got := decodeActivity(t, body)
	if code != http.StatusOK || got.Activity != restdtos.SessionActivityActivityIdle || got.Wait == nil || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled || got.Wait.WaitedMs != 0 {
		t.Fatalf("a wait on an idle session = %d %s, want idle, settled after 0 ms", code, body)
	}

	for _, q := range []string{"?waitSeconds=-1", "?waitSeconds=abc", "?waitSeconds=", "?waitSeconds=2.5"} {
		if code, body := status(idle.ID, q); code != http.StatusBadRequest || strings.TrimSpace(string(body)) != `{"error":"malformed waitSeconds"}` {
			t.Fatalf("%s = %d %s, want 400 malformed waitSeconds", q, code, body)
		}
	}
	unknown := pgtype.UUID{Bytes: [16]byte{0xde, 0xad}, Valid: true}
	if code, body := status(unknown, "?waitSeconds=5"); code != http.StatusNotFound || strings.TrimSpace(string(body)) != `{"error":"session not found"}` {
		t.Fatalf("a wait on an unknown session = %d %s, want 404", code, body)
	}

	running := createSessionForUser(ctx, t, rig, user.ID, nil)
	turnID := processingTurn(ctx, t, rig, running.ID)
	var answer []byte
	var answeredAt atomic.Int64
	var g errgroup.Group
	g.Go(func() error {
		code, body, err := getWithCookie(ctx, srv.URL+"/api/sessions/"+running.ID.String()+"/status?waitSeconds=5", cookie)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("wait: %d %s", code, body)
		}
		answeredAt.Store(time.Now().UnixNano())
		answer = body
		return nil
	})
	time.Sleep(350 * time.Millisecond) // three polls
	if answeredAt.Load() != 0 || waiter.Active() != 1 {
		t.Fatalf("the wait on a running session answered (%d) or is not blocked (Active %d) after three polls", answeredAt.Load(), waiter.Active())
	}
	completedAt := time.Now()
	completeInOneTransaction(ctx, t, rig, running.ID, turnID)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if after := time.Duration(answeredAt.Load() - completedAt.UnixNano()); after > time.Second {
		t.Fatalf("the wait answered %v after the turn completed, want within about one poll", after)
	}
	got = decodeActivity(t, answer)
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.Wait == nil || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled || got.Wait.WaitedMs < 300 {
		t.Fatalf("the wait = %s, want finished, settled, after at least 300 ms", answer)
	}
}

// TestWait_ConcurrentWaitersSameSession_Race: sixteen waits on one running
// session, from one caller, through the real route on the shared pool of
// four connections, all block; the turn completes; each answers exactly
// once, finished and "settled", and nothing stays counted. Run under -race.
func TestWait_ConcurrentWaitersSameSession_Race(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)
	turnID := processingTurn(ctx, t, rig, sess.ID)
	const waits = 16
	waiter := testWaiter(waits)
	srv := waitServer(t, rig.pool, waiter)

	var settled atomic.Int64
	var g errgroup.Group
	for i := range waits {
		g.Go(func() error {
			code, body, err := getWithCookie(ctx, srv.URL+"/api/sessions/"+sess.ID.String()+"/status?waitSeconds=5", cookie)
			if err != nil {
				return err
			}
			var got restdtos.SessionActivity
			if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Wait == nil {
				return fmt.Errorf("wait %d: %d %s", i, code, body)
			}
			if got.Activity != restdtos.SessionActivityActivityFinished || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled {
				return fmt.Errorf("wait %d = %s, want finished and settled", i, body)
			}
			settled.Add(1)
			return nil
		})
	}
	if !eventuallyTrue(3*time.Second, func() bool { return waiter.Active() == waits }) {
		t.Fatalf("Active = %d, want %d blocked waits", waiter.Active(), waits)
	}
	completeInOneTransaction(ctx, t, rig, sess.ID, turnID)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if settled.Load() != waits || waiter.Active() != 0 {
		t.Fatalf("%d settled answers, Active %d, want %d and 0", settled.Load(), waiter.Active(), waits)
	}
}

// TestWait_ClientDisconnectEndsPolling_HTTP: a client that goes away
// mid-wait -- its request cancelled, the connection closed -- ends the
// wait on the server within two polls (net/http cancels the request's
// context; the Waiter selects on it), so an abandoned wait neither keeps
// polling Postgres nor holds its caller's slot.
func TestWait_ClientDisconnectEndsPolling_HTTP(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)
	createTurn(ctx, t, rig.turns, sess.ID, false)
	waiter := testWaiter(2)
	srv := waitServer(t, rig.pool, waiter)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var g errgroup.Group
	g.Go(func() error {
		_, _, _ = getWithCookie(reqCtx, srv.URL+"/api/sessions/"+sess.ID.String()+"/status?waitSeconds=5", cookie)
		return nil
	})
	if !eventuallyTrue(2*time.Second, func() bool { return waiter.Active() == 1 }) {
		t.Fatalf("Active = %d, want the wait blocked", waiter.Active())
	}
	cancel()
	_ = g.Wait()
	if !eventuallyTrue(200*time.Millisecond+500*time.Millisecond, func() bool { return waiter.Active() == 0 }) {
		t.Fatalf("Active = %d two polls after the client left, want 0", waiter.Active())
	}
}

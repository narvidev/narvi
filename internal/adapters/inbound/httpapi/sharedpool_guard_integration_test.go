//go:build integration

package httpapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// guardTestGrace is poolGuard's settle grace in its own test. Every row
// whose connection stays out waits all of it, and none needs more.
const guardTestGrace = 300 * time.Millisecond

// TestPoolGuard_FailsATestOnlyForWhatItAcquired runs poolGuard.watch on a
// pool of its own, with recorders standing in for the watched tests. What
// a test acquired and kept fails it, named where it was acquired, an
// acquire the pool counts before Acquire returns it included. What the
// pool takes for itself, through its health check, does not: a guard that
// judged on the pool's count failed main's CI with the report the two
// health-check rows get from it, a count of 1 and no connection named. Nor
// does an acquire that failed, nor what an earlier test kept.
func TestPoolGuard_FailsATestOnlyForWhatItAcquired(t *testing.T) {
	_, connStr := IntegrationTestPoolAndConnStr(t)

	// leaked is the report of one connection kept, acquired in where.
	leaked := func(where string) []string {
		return []string{"1 shared-pool connections acquired during this test", "connection 1:\n", where}
	}
	tests := []struct {
		name string
		// configure, if set, adjusts the watched pool's config.
		configure func(*pgxpool.Config)
		// steps are tests one guard watches in turn on one pool. What a
		// step keeps stays out until the row ends.
		steps []guardTestStep
	}{
		{
			name:  "a query whose connection came back",
			steps: []guardTestStep{{act: queryAndReturn}},
		},
		{
			name:  "a connection acquired and never released",
			steps: []guardTestStep{{act: keepAConnection, wantReport: leaked("httpapi.keepAConnection")}},
		},
		{
			name:  "a transaction never ended",
			steps: []guardTestStep{{act: beginAndNeverEnd, wantReport: leaked("httpapi.beginAndNeverEnd")}},
		},
		{
			name:  "rows never closed",
			steps: []guardTestStep{{act: queryAndNeverClose, wantReport: leaked("httpapi.queryAndNeverClose")}},
		},
		{
			name: "an acquire the pool counts and has yet to return",
			steps: []guardTestStep{{
				act:        acquireHeldBeforeReturn,
				wantReport: []string{"1 shared-pool connections acquired during this test", "connection 1, which Acquire has yet to return:", "httpapi.acquireHeldBeforeReturn"},
			}},
		},
		{
			name:  "an acquire whose context was already canceled",
			steps: []guardTestStep{{act: acquireWithCanceledContext}},
		},
		{
			name:  "an acquire PrepareConn refuses once the pool counts it",
			steps: []guardTestStep{{act: acquireRefusedInPrepareConn}},
		},
		{
			name: "a test after one that kept a connection",
			steps: []guardTestStep{
				{act: keepAConnection, wantReport: leaked("httpapi.keepAConnection")},
				{act: queryAndReturn},
			},
		},
		{
			name:  "idle connections taken as the pool's health check takes them, through the check",
			steps: []guardTestStep{{act: takeIdleAsTheHealthCheckDoes, wantLog: "nothing this test acquired is still out"}},
		},
		{
			name: "the pool's own health check holding an idle connection through the check",
			configure: func(c *pgxpool.Config) {
				c.HealthCheckPeriod = 10 * time.Millisecond
				c.MaxConnIdleTime = time.Millisecond
			},
			steps: []guardTestStep{{act: letTheHealthCheckHoldOne, wantLog: "nothing this test acquired is still out"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			guard := newPoolGuard(guardTestGrace)
			p := newGuardTestPool(ctx, t, connStr, guard, tt.configure)
			for i, step := range tt.steps {
				watched := &guardRecorder{name: fmt.Sprintf("%s/watched-%d", t.Name(), i+1)}
				guard.watch(watched, p.Pool)
				step.act(ctx, t, p)
				watched.end()
				step.check(t, watched)
			}
		})
	}
}

// guardTestStep is one test a poolGuard watches: its body, and what the
// guard must say of it.
type guardTestStep struct {
	// act is the watched test's body. What it keeps, it gives back in a
	// cleanup of t's, after the guard's check.
	act func(ctx context.Context, t *testing.T, p *guardTestPool)
	// wantReport is what the guard's one report must hold; none means the
	// guard must not fail the test.
	wantReport []string
	// wantLog is what the guard's cross-check must log; "" means nothing.
	wantLog string
}

// check fails t unless the guard said of watched what step wants.
func (step guardTestStep) check(t *testing.T, watched *guardRecorder) {
	t.Helper()
	if len(step.wantReport) == 0 {
		if len(watched.errors) > 0 {
			t.Errorf("%s: the guard failed a test that leaked nothing:\n%s", watched.name, strings.Join(watched.errors, "\n"))
		}
	} else {
		if len(watched.errors) != 1 {
			t.Fatalf("%s: the guard reported %d failures, want 1:\n%s", watched.name, len(watched.errors), strings.Join(watched.errors, "\n"))
		}
		for _, want := range step.wantReport {
			if !strings.Contains(watched.errors[0], want) {
				t.Errorf("%s: the guard's report lacks %q:\n%s", watched.name, want, watched.errors[0])
			}
		}
	}
	logs := strings.Join(watched.logs, "\n")
	switch {
	case step.wantLog == "" && logs != "":
		t.Errorf("%s: the guard logged %q, want nothing", watched.name, logs)
	case !strings.Contains(logs, step.wantLog):
		t.Errorf("%s: the guard logged %q, want it to hold %q", watched.name, logs, step.wantLog)
	}
}

func queryAndReturn(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	if _, err := p.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
}

func keepAConnection(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(conn.Release)
}

func beginAndNeverEnd(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
}

func queryAndNeverClose(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	rows, err := p.Query(ctx, "SELECT generate_series(1, 3)")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	t.Cleanup(rows.Close)
}

// acquireHeldBeforeReturn starts an acquire and holds it in PrepareConn,
// after puddle has handed it a connection, so the pool counts it, and
// before Acquire returns it, so TraceAcquireEnd has yet to run, through
// the guard's check.
func acquireHeldBeforeReturn(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	p.prepare.shut.Store(true)
	var g errgroup.Group
	g.Go(func() error {
		conn, err := p.Acquire(ctx)
		if err != nil {
			return err
		}
		conn.Release()
		return nil
	})
	t.Cleanup(func() {
		p.prepare.open()
		if err := g.Wait(); err != nil {
			t.Errorf("the held acquire, once let go: %v", err)
		}
	})
	select {
	case <-p.prepare.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the acquire never reached PrepareConn")
	}
	if n := p.Stat().AcquiredConns(); n != 1 {
		t.Fatalf("the pool counts %d connections out while an acquire is held before it returns, want 1", n)
	}
}

// acquireWithCanceledContext asks for a connection with a context already
// canceled. Acquire fails before puddle hands it one.
func acquireWithCanceledContext(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if conn, err := p.Acquire(canceled); err == nil {
		conn.Release()
		t.Fatal("Acquire with a canceled context succeeded, want it to fail")
	}
}

// acquireRefusedInPrepareConn holds an acquire in PrepareConn, its
// connection counted, until its short context ends. PrepareConn then
// refuses it, the pool takes the connection back, and Acquire fails.
func acquireRefusedInPrepareConn(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	p.prepare.shut.Store(true)
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if conn, err := p.Acquire(short); err == nil {
		conn.Release()
		t.Fatal("Acquire through a shut PrepareConn succeeded, want it to fail")
	}
	select {
	case <-p.prepare.entered:
	default:
		t.Fatal("the acquire never reached PrepareConn")
	}
	if n := p.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("the pool counts %d connections out after PrepareConn refused the acquire, want 0", n)
	}
}

// takeIdleAsTheHealthCheckDoes takes every idle connection through
// AcquireAllIdle -- the puddle call pgxpool's health check makes, with no
// tracer call -- and holds them through the guard's check.
func takeIdleAsTheHealthCheckDoes(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	if _, err := p.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	idle := p.AcquireAllIdle(ctx)
	t.Cleanup(func() {
		for _, conn := range idle {
			conn.Release()
		}
	})
	if len(idle) == 0 {
		t.Fatal("AcquireAllIdle took no connection, want the one SELECT 1 left idle")
	}
	if n := p.Stat().AcquiredConns(); n != int32(len(idle)) {
		t.Fatalf("the pool counts %d connections out while AcquireAllIdle holds %d, want as many", n, len(idle))
	}
}

// letTheHealthCheckHoldOne leaves a connection idle past MaxConnIdleTime
// for the pool's own health check, which takes it through AcquireAllIdle
// and destroys it; the pool counts it acquired until its destructor ends,
// which BeforeClose holds through the guard's check.
func letTheHealthCheckHoldOne(ctx context.Context, t *testing.T, p *guardTestPool) {
	t.Helper()
	p.close.shut.Store(true)
	t.Cleanup(p.close.open)
	if _, err := p.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	select {
	case <-p.close.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool's health check never took the idle connection")
	}
	if n := p.Stat().AcquiredConns(); n != 1 {
		t.Fatalf("the pool counts %d connections out while its health check holds one, want 1", n)
	}
}

// guardTestPool is a pool of its own a poolGuard watches, with a hold on
// PrepareConn and one on BeforeClose.
type guardTestPool struct {
	*pgxpool.Pool
	prepare *hold
	close   *hold
}

func newGuardTestPool(ctx context.Context, t *testing.T, connStr string, guard *poolGuard, configure func(*pgxpool.Config)) *guardTestPool {
	t.Helper()
	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("parse the guard test pool's config: %v", err)
	}
	p := &guardTestPool{prepare: newHold(), close: newHold()}
	config.MaxConns = 2
	config.ConnConfig.Tracer = guard
	config.PrepareConn = func(ctx context.Context, _ *pgx.Conn) (bool, error) {
		return true, p.prepare.wait(ctx)
	}
	config.BeforeClose = func(*pgx.Conn) {
		_ = p.close.wait(context.Background())
	}
	if configure != nil {
		configure(config)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the guard test pool: %v", err)
	}
	p.Pool = pool
	t.Cleanup(func() {
		p.prepare.open()
		p.close.open()
		pool.Close()
	})
	return p
}

// hold, while shut, stops whoever waits on it until it opens: in
// PrepareConn, an acquire the pool counts that Acquire has yet to return;
// in BeforeClose, a destroyed connection the pool counts until its
// destructor ends.
type hold struct {
	shut     atomic.Bool
	entered  chan struct{}
	opened   chan struct{}
	openOnce sync.Once
}

func newHold() *hold {
	return &hold{entered: make(chan struct{}, 1), opened: make(chan struct{})}
}

// wait returns at once while h is open. While h is shut, it says so on
// entered, then waits for h to open or ctx to end.
func (h *hold) wait(ctx context.Context) error {
	if !h.shut.Load() {
		return nil
	}
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-h.opened:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (h *hold) open() { h.openOnce.Do(func() { close(h.opened) }) }

// guardRecorder stands in for the test a poolGuard watches: it keeps what
// the guard reports and logs, and end runs the cleanups the guard
// registered, last first, as a test's end does.
type guardRecorder struct {
	name     string
	cleanups []func()
	errors   []string
	logs     []string
}

func (r *guardRecorder) Helper()          {}
func (r *guardRecorder) Name() string     { return r.name }
func (r *guardRecorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *guardRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *guardRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *guardRecorder) end() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

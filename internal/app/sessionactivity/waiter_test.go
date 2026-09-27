package sessionactivity_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/platform"
)

// session is a fake status source: settled once flipped, and counting its
// reads. Safe for concurrent readers and one writer.
type session struct {
	settled atomic.Bool
	reads   atomic.Int64
}

func (s *session) read(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.reads.Add(1)
	return s.settled.Load(), nil
}

// eventually polls cond every millisecond for up to d.
func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// config is a Waiter configuration in test time: a 200 ms maximum polled
// every 10 ms, and caps high enough not to matter unless a test lowers
// them.
func config() sessionactivity.Config {
	return sessionactivity.Config{MaxDuration: 200 * time.Millisecond, PollInterval: 10 * time.Millisecond, MaxPerKey: 64, MaxPerUser: 64, MaxPerReplica: 64}
}

// as is the Caller of a wait by user under key (a grant's, or the user's
// own for a cookie request).
func as(user, key string) sessionactivity.Caller {
	return sessionactivity.Caller{Key: key, User: user}
}

// k is the caller of the tests that need only one.
var k = as("u", "k")

// TestConfigFrom_ReadsTheMCPWaitFields: the Waiter's bounds are the
// platform.Timeouts MCPWait* fields, and nothing else.
func TestConfigFrom_ReadsTheMCPWaitFields(t *testing.T) {
	to := platform.DefaultTimeouts()
	got := sessionactivity.ConfigFrom(to)
	want := sessionactivity.Config{MaxDuration: to.MCPWaitMaxDuration, PollInterval: to.MCPWaitPollInterval, MaxPerKey: to.MCPWaitMaxConcurrentPerKey, MaxPerUser: to.MCPWaitMaxConcurrentPerUser, MaxPerReplica: to.MCPWaitMaxConcurrentPerReplica}
	if got != want {
		t.Fatalf("ConfigFrom = %+v, want %+v", got, want)
	}
}

// TestBound_Table pins the clamp on the wire's whole seconds: a value past
// the maximum -- the largest int64 included, which must never overflow a
// Duration -- is the maximum, and zero or below is no wait.
func TestBound_Table(t *testing.T) {
	w := sessionactivity.NewWaiter(sessionactivity.ConfigFrom(platform.DefaultTimeouts()))
	short := sessionactivity.NewWaiter(config())
	for _, tc := range []struct {
		name    string
		w       *sessionactivity.Waiter
		seconds int64
		want    time.Duration
	}{
		{"zero", w, 0, 0},
		{"negative", w, -5, 0},
		{"one second", w, 1, time.Second},
		{"exactly the maximum", w, 25, 25 * time.Second},
		{"one past the maximum", w, 26, 25 * time.Second},
		{"a day", w, 86400, 25 * time.Second},
		{"the largest int64", w, math.MaxInt64, 25 * time.Second},
		{"a maximum below one second", short, 1, 200 * time.Millisecond},
		{"a maximum below one second, a large ask", short, math.MaxInt64, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Bound(tc.seconds); got != tc.want {
				t.Fatalf("Bound(%d) = %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}

// TestWait_SettledOnFirstReadAnswersAtOnce: a settled session is answered
// by the first read, with nothing waited and no slot taken.
func TestWait_SettledOnFirstReadAnswersAtOnce(t *testing.T) {
	w := sessionactivity.NewWaiter(config())
	s := &session{}
	s.settled.Store(true)
	out, err := w.Wait(context.Background(), k, 10, s.read)
	if err != nil || out.Reason != sessionactivity.ReasonSettled || out.Waited != 0 {
		t.Fatalf("Wait = %+v, %v, want settled with nothing waited", out, err)
	}
	if n := s.reads.Load(); n != 1 {
		t.Fatalf("%d reads, want exactly 1", n)
	}
}

// TestWait_ClampsToMaxDuration asks for ten seconds under a 200 ms
// maximum: the wait never settles, polls more than once, and answers
// "timeout" no sooner than the maximum and no later than the maximum plus
// one poll (and some scheduling slack). Mutations: the clamp removed (the
// wait runs ten seconds); the first read answered regardless (one read,
// no wait).
func TestWait_ClampsToMaxDuration(t *testing.T) {
	cfg := config()
	w := sessionactivity.NewWaiter(cfg)
	s := &session{}
	start := time.Now()
	out, err := w.Wait(context.Background(), k, 10, s.read)
	elapsed := time.Since(start)
	if err != nil || out.Reason != sessionactivity.ReasonTimeout {
		t.Fatalf("Wait = %+v, %v, want timeout", out, err)
	}
	if elapsed < cfg.MaxDuration || elapsed > cfg.MaxDuration+cfg.PollInterval+300*time.Millisecond {
		t.Fatalf("the wait took %v, want within [%v, %v + one poll]", elapsed, cfg.MaxDuration, cfg.MaxDuration)
	}
	if out.Waited < cfg.MaxDuration || out.Waited > elapsed {
		t.Fatalf("Waited = %v, want at least the maximum %v and at most the %v the call took", out.Waited, cfg.MaxDuration, elapsed)
	}
	if n := s.reads.Load(); n < 3 {
		t.Fatalf("%d reads, want the wait to have polled several times", n)
	}
	if w.Active() != 0 {
		t.Fatalf("Active = %d after the wait, want 0", w.Active())
	}
}

// TestWait_TakesAFreshReadAtTheClamp (review round 1's P3): when the bound
// comes, the wait reads once more and answers that read, so a session that
// settled after the last poll and before the clamp is answered settled,
// never a stale timeout. The fake settles right after a given read returns
// unsettled -- deterministic, no race against the clock. Two shapes: the
// shipped waitSeconds=1 against a one-second poll, scaled down, where the
// clamp comes before the first poll and is the only read after the first;
// and a poll the clamp cuts short. Mutation: the loop answering "timeout"
// once the deadline has passed at a timer, without that last read (one
// read short, reason timeout).
func TestWait_TakesAFreshReadAtTheClamp(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxDuration time.Duration
		poll        time.Duration
		settleAfter int64 // the session settles once this read has returned unsettled
		wantReads   int64
	}{
		{"the clamp comes before the first poll", 200 * time.Millisecond, time.Second, 1, 2},
		{"the clamp cuts the last poll short", 700 * time.Millisecond, 500 * time.Millisecond, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config()
			cfg.MaxDuration, cfg.PollInterval = tc.maxDuration, tc.poll
			w := sessionactivity.NewWaiter(cfg)
			var settled atomic.Bool
			var reads atomic.Int64
			out, err := w.Wait(context.Background(), k, 1, func(context.Context) (bool, error) {
				now := settled.Load()
				if reads.Add(1) == tc.settleAfter {
					settled.Store(true)
				}
				return now, nil
			})
			if err != nil || out.Reason != sessionactivity.ReasonSettled {
				t.Fatalf("Wait = %+v, %v after %d reads, want the read at the clamp answered: settled", out, err, reads.Load())
			}
			if reads.Load() != tc.wantReads {
				t.Fatalf("%d reads, want %d: the last one at the clamp", reads.Load(), tc.wantReads)
			}
			if out.Waited < tc.maxDuration {
				t.Fatalf("Waited = %v, want the answer taken at the clamp, %v", out.Waited, tc.maxDuration)
			}
		})
	}
}

// TestWait_ReturnsWithinOnePollOfSettling: the wait blocks while the
// session is unsettled and answers "settled" within about one poll of it
// settling, with the time it waited.
func TestWait_ReturnsWithinOnePollOfSettling(t *testing.T) {
	cfg := config()
	cfg.MaxDuration = 5 * time.Second
	w := sessionactivity.NewWaiter(cfg)
	s := &session{}
	var g errgroup.Group
	var out sessionactivity.Outcome
	g.Go(func() error {
		var err error
		out, err = w.Wait(context.Background(), k, 5, s.read)
		return err
	})
	if !eventually(time.Second, func() bool { return s.reads.Load() >= 3 }) {
		t.Fatal("the wait did not poll")
	}
	if w.Active() != 1 {
		t.Fatalf("Active = %d while blocked, want 1", w.Active())
	}
	flipped := time.Now()
	s.settled.Store(true)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if since := time.Since(flipped); since > cfg.PollInterval+300*time.Millisecond {
		t.Fatalf("answered %v after settling, want within about one poll (%v)", since, cfg.PollInterval)
	}
	if out.Reason != sessionactivity.ReasonSettled || out.Waited <= 0 {
		t.Fatalf("Wait = %+v, want settled after a positive wait", out)
	}
}

// TestWait_ClientDisconnectEndsPolling cancels the request's context
// mid-wait: the wait ends with the context's error, the active count is
// back to zero within two polls, and no read follows. Mutation: the loop
// on context.Background() (the wait runs to its bound, still counted).
func TestWait_ClientDisconnectEndsPolling(t *testing.T) {
	cfg := config()
	cfg.MaxDuration = 10 * time.Second
	w := sessionactivity.NewWaiter(cfg)
	s := &session{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var g errgroup.Group
	var waitErr error
	g.Go(func() error {
		_, waitErr = w.Wait(ctx, k, 10, s.read)
		return nil
	})
	if !eventually(time.Second, func() bool { return w.Active() == 1 && s.reads.Load() >= 2 }) {
		t.Fatalf("the wait never blocked: Active %d, reads %d", w.Active(), s.reads.Load())
	}
	cancel()
	if !eventually(2*cfg.PollInterval+100*time.Millisecond, func() bool { return w.Active() == 0 }) {
		t.Fatalf("Active = %d two polls after the client left, want 0", w.Active())
	}
	_ = g.Wait()
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", waitErr)
	}
	after := s.reads.Load()
	time.Sleep(5 * cfg.PollInterval)
	if s.reads.Load() != after {
		t.Fatalf("reads went on after the wait ended: %d then %d", after, s.reads.Load())
	}
}

// TestWait_ShutdownInterruptsPromptly: with a one-second poll, Interrupt
// ends a blocked wait with "interrupted" long before the next poll would
// have come, and a wait that starts afterwards answers its first read at
// once -- "settled" when it is, "interrupted" when it is not -- so no wait
// can hold the drain.
func TestWait_ShutdownInterruptsPromptly(t *testing.T) {
	cfg := sessionactivity.Config{MaxDuration: 20 * time.Second, PollInterval: time.Second, MaxPerKey: 8, MaxPerUser: 8, MaxPerReplica: 8}
	w := sessionactivity.NewWaiter(cfg)
	s := &session{}
	var g errgroup.Group
	outs := make([]sessionactivity.Outcome, 3)
	for i := range outs {
		g.Go(func() error {
			var err error
			outs[i], err = w.Wait(context.Background(), k, 20, s.read)
			return err
		})
	}
	if !eventually(time.Second, func() bool { return w.Active() == 3 }) {
		t.Fatalf("Active = %d, want 3 blocked waits", w.Active())
	}
	interruptedAt := time.Now()
	w.Interrupt()
	w.Interrupt() // idempotent
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if since := time.Since(interruptedAt); since > 250*time.Millisecond {
		t.Fatalf("waits answered %v after Interrupt, want well within one poll (%v)", since, cfg.PollInterval)
	}
	for i, out := range outs {
		if out.Reason != sessionactivity.ReasonInterrupted {
			t.Fatalf("wait %d = %+v, want interrupted", i, out)
		}
	}
	if w.Active() != 0 {
		t.Fatalf("Active = %d, want 0", w.Active())
	}

	start := time.Now()
	out, err := w.Wait(context.Background(), k, 20, s.read)
	if err != nil || out.Reason != sessionactivity.ReasonInterrupted || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("a wait after Interrupt = %+v, %v after %v, want interrupted at once", out, err, time.Since(start))
	}
	s.settled.Store(true)
	if out, err := w.Wait(context.Background(), k, 20, s.read); err != nil || out.Reason != sessionactivity.ReasonSettled {
		t.Fatalf("a settled wait after Interrupt = %+v, %v, want settled", out, err)
	}
}

// TestWait_ConcurrentWaitersSameSession_Race: sixteen waits on one session
// from sixteen callers all block, all see it settle, and each answers
// exactly once, "settled"; nothing stays counted. Run under -race.
func TestWait_ConcurrentWaitersSameSession_Race(t *testing.T) {
	cfg := config()
	cfg.MaxDuration = 5 * time.Second
	cfg.MaxPerReplica = 16
	w := sessionactivity.NewWaiter(cfg)
	s := &session{}
	const n = 16
	var answered atomic.Int64
	var mu sync.Mutex
	reasons := map[sessionactivity.Reason]int{}
	var g errgroup.Group
	for i := range n {
		g.Go(func() error {
			who := string(rune('a' + i))
			out, err := w.Wait(context.Background(), as(who, who), 5, s.read)
			if err != nil {
				return err
			}
			answered.Add(1)
			mu.Lock()
			reasons[out.Reason]++
			mu.Unlock()
			return nil
		})
	}
	if !eventually(2*time.Second, func() bool { return w.Active() == n }) {
		t.Fatalf("Active = %d, want %d blocked waits", w.Active(), n)
	}
	s.settled.Store(true)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if answered.Load() != n || reasons[sessionactivity.ReasonSettled] != n {
		t.Fatalf("%d answers, reasons %v, want %d settled", answered.Load(), reasons, n)
	}
	if w.Active() != 0 {
		t.Fatalf("Active = %d, want 0", w.Active())
	}
}

// TestWait_PerKeyCapDegradesToSnapshot: with two waits of one caller
// blocked, the caller's third answers its first read at once with
// "capacity" -- no error, one read, nothing counted -- while another
// caller's wait is admitted; the replica's cap degrades the same way; a
// settled session is answered "settled" even over a cap; and a slot given
// back is taken again. Mutations: the over-cap wait blocking, or erroring.
func TestWait_PerKeyCapDegradesToSnapshot(t *testing.T) {
	cfg := config()
	cfg.MaxDuration = 10 * time.Second
	cfg.MaxPerKey = 2
	cfg.MaxPerUser = 3
	cfg.MaxPerReplica = 3
	w := sessionactivity.NewWaiter(cfg)
	s := &session{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var g errgroup.Group
	block := func(c sessionactivity.Caller) {
		g.Go(func() error {
			_, _ = w.Wait(ctx, c, 10, s.read)
			return nil
		})
	}
	block(as("alice", "grant:a"))
	block(as("alice", "grant:a"))
	if !eventually(time.Second, func() bool { return w.Active() == 2 }) {
		t.Fatalf("Active = %d, want the caller's two waits blocked", w.Active())
	}

	over := func(c sessionactivity.Caller) {
		t.Helper()
		key := c.Key
		own := &session{}
		start := time.Now()
		out, err := w.Wait(ctx, c, 10, own.read)
		if err != nil || out.Reason != sessionactivity.ReasonCapacity || out.Waited != 0 {
			t.Fatalf("over-cap wait for %s = %+v, %v, want capacity with nothing waited, and no error", key, out, err)
		}
		if took := time.Since(start); took > 250*time.Millisecond {
			t.Fatalf("over-cap wait for %s took %v, want an answer at once", key, took)
		}
		if own.reads.Load() != 1 {
			t.Fatalf("over-cap wait for %s read %d times, want exactly one snapshot", key, own.reads.Load())
		}
	}
	over(as("alice", "grant:a"))

	block(as("bob", "grant:b"))
	if !eventually(time.Second, func() bool { return w.Active() == 3 }) {
		t.Fatalf("Active = %d, want another caller admitted", w.Active())
	}
	over(as("carol", "grant:c")) // the replica's cap

	settled := &session{}
	settled.settled.Store(true)
	if out, err := w.Wait(ctx, as("alice", "grant:a"), 10, settled.read); err != nil || out.Reason != sessionactivity.ReasonSettled {
		t.Fatalf("a settled session over the cap = %+v, %v, want settled", out, err)
	}
	if w.Active() != 3 {
		t.Fatalf("Active = %d, want the three admitted waits only", w.Active())
	}

	cancel()
	_ = g.Wait()
	if w.Active() != 0 {
		t.Fatalf("Active = %d after every wait ended, want 0", w.Active())
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	var g2 errgroup.Group
	for range 2 {
		g2.Go(func() error {
			_, _ = w.Wait(ctx2, as("alice", "grant:a"), 10, s.read)
			return nil
		})
	}
	if !eventually(time.Second, func() bool { return w.Active() == 2 }) {
		t.Fatalf("Active = %d, want the caller's slots taken again once given back", w.Active())
	}
	cancel2()
	_ = g2.Wait()
}

// TestWait_ReadErrorEndsTheWait: a failing read ends the wait with its
// error, on the first read or a later one, and gives its slot back.
func TestWait_ReadErrorEndsTheWait(t *testing.T) {
	boom := errors.New("boom")
	w := sessionactivity.NewWaiter(config())
	if _, err := w.Wait(context.Background(), k, 10, func(context.Context) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("first read failing: err = %v, want boom", err)
	}
	var calls atomic.Int64
	_, err := w.Wait(context.Background(), k, 10, func(context.Context) (bool, error) {
		if calls.Add(1) == 3 {
			return false, boom
		}
		return false, nil
	})
	if !errors.Is(err, boom) || calls.Load() != 3 {
		t.Fatalf("third read failing: err = %v after %d reads, want boom after 3", err, calls.Load())
	}
	if w.Active() != 0 {
		t.Fatalf("Active = %d, want 0", w.Active())
	}
}

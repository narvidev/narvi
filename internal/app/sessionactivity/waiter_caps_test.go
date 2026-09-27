package sessionactivity_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/platform"
)

// burst is a set of waits started together on a session that never
// settles, each either admitted -- blocked until end -- or already
// answered.
type burst struct {
	stop context.CancelFunc
	g    errgroup.Group

	mu       sync.Mutex
	admitted map[sessionactivity.Caller]int
	answered map[sessionactivity.Reason]int
}

// startBurst starts one wait per entry of callers, all released at once by
// a barrier so their admissions race, and returns once each has either
// been admitted -- it took its second read, which only an admitted wait
// takes -- or answered. The admitted ones block, on a bound far past the
// test, until end.
func startBurst(t *testing.T, w *sessionactivity.Waiter, callers []sessionactivity.Caller) *burst {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &burst{stop: cancel, admitted: map[sessionactivity.Caller]int{}, answered: map[sessionactivity.Reason]int{}}
	t.Cleanup(func() { // a test that failed before end leaves nothing running
		cancel()
		_ = b.g.Wait()
	})
	resolved := make(chan struct{}, len(callers))
	barrier := make(chan struct{})
	for _, c := range callers {
		var reads atomic.Int64
		var once sync.Once
		resolve := func(record func()) {
			once.Do(func() {
				b.mu.Lock()
				record()
				b.mu.Unlock()
				resolved <- struct{}{}
			})
		}
		b.g.Go(func() error {
			<-barrier
			out, err := w.Wait(ctx, c, 60, func(context.Context) (bool, error) {
				if reads.Add(1) == 2 {
					resolve(func() { b.admitted[c]++ })
				}
				return false, nil
			})
			if ctx.Err() != nil {
				return nil // ended by end: it was admitted
			}
			if err != nil {
				return fmt.Errorf("wait for %+v: %w", c, err)
			}
			resolve(func() { b.answered[out.Reason]++ })
			return nil
		})
	}
	close(barrier)
	timeout := time.After(10 * time.Second)
	for range callers {
		select {
		case <-resolved:
		case <-timeout:
			cancel()
			_ = b.g.Wait()
			t.Fatalf("the burst never settled: %d waits", len(callers))
		}
	}
	return b
}

// end ends every admitted wait of b and waits for them all.
func (b *burst) end(t *testing.T) {
	t.Helper()
	b.stop()
	if err := b.g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// admittedTotal is how many of b's waits were admitted, in all and for user.
func (b *burst) admittedTotal(user string) (all, users int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c, n := range b.admitted {
		all += n
		if c.User == user {
			users += n
		}
	}
	return all, users
}

// requireNothingCounted fails unless w counts no wait at all: the replica's
// count zero, and no user's or key's entry left behind.
func requireNothingCounted(t *testing.T, w *sessionactivity.Waiter) {
	t.Helper()
	replica, perUser, perKey := w.CountsForTest()
	if replica != 0 || len(perUser) != 0 || len(perKey) != 0 {
		t.Errorf("counts after every wait ended: replica %d, per user %v, per key %v, want nothing counted", replica, perUser, perKey)
	}
}

// shippedCaps is the shipped caps in test time: a 30-second bound, which no
// test here reaches, polled every 10 ms.
func shippedCaps(t *testing.T) sessionactivity.Config {
	t.Helper()
	cfg := sessionactivity.ConfigFrom(platform.DefaultTimeouts())
	if cfg.MaxPerKey != 2 || cfg.MaxPerUser != 4 || cfg.MaxPerReplica != 32 {
		t.Fatalf("shipped caps %d per key, %d per user, %d per replica, want 2, 4 and 32", cfg.MaxPerKey, cfg.MaxPerUser, cfg.MaxPerReplica)
	}
	cfg.MaxDuration, cfg.PollInterval = 30*time.Second, 10*time.Millisecond
	return cfg
}

// grants is n waits by user, spread over its grants g0..g(n/per-1), per
// waits on each.
func grants(user string, n, per int) []sessionactivity.Caller {
	out := make([]sessionactivity.Caller, 0, n)
	for i := range n {
		out = append(out, as(user, fmt.Sprintf("grant:%s-%d", user, i/per)))
	}
	return out
}

// TestWait_PerUserCapSpansEveryGrantAndTheBrowser (review round 1's P1): a
// user holds one grant per MCP client and may authorize any number, so the
// per-key cap alone let sixteen grants of one user hold all 32 of a
// replica's slots. Under the shipped caps (2 per key, 4 per user, 32 per
// replica), that user's 32 waits over sixteen grants take exactly four
// slots; a grant of theirs with nothing running, and their browser, answer
// "capacity" at once; another user's wait is admitted and really waits
// until their session settles; and once the first user's waits end, their
// four slots come back, the browser's included. Mutations: the per-user
// check dropped (the user holds 32), the per-user count never given back
// (the user holds none the second time).
func TestWait_PerUserCapSpansEveryGrantAndTheBrowser(t *testing.T) {
	w := sessionactivity.NewWaiter(shippedCaps(t))

	alice := startBurst(t, w, grants("alice", 32, 2))
	all, _ := alice.admittedTotal("alice")
	if all != 4 || alice.answered[sessionactivity.ReasonCapacity] != 28 || len(alice.answered) != 1 {
		t.Fatalf("one user's 32 waits over 16 grants: %d admitted, answered %v, want 4 admitted and 28 capacity", all, alice.answered)
	}
	replica, perUser, perKey := w.CountsForTest()
	if replica != 4 || perUser["alice"] != 4 || len(perUser) != 1 {
		t.Fatalf("counts: replica %d, per user %v, want the user's 4", replica, perUser)
	}
	for key, n := range perKey {
		if n > 2 {
			t.Fatalf("key %s counts %d waits, want at most 2", key, n)
		}
	}

	for _, c := range []sessionactivity.Caller{as("alice", "grant:alice-new"), as("alice", "user:alice")} {
		own := &session{}
		start := time.Now()
		out, err := w.Wait(context.Background(), c, 10, own.read)
		if err != nil || out.Reason != sessionactivity.ReasonCapacity || out.Waited != 0 || own.reads.Load() != 1 {
			t.Fatalf("the user's wait through %s = %+v, %v, %d reads, want capacity at once from one read", c.Key, out, err, own.reads.Load())
		}
		if took := time.Since(start); took > 250*time.Millisecond {
			t.Fatalf("the user's wait through %s took %v, want an answer at once", c.Key, took)
		}
	}

	bob := &session{}
	var g errgroup.Group
	var out sessionactivity.Outcome
	g.Go(func() error {
		var err error
		out, err = w.Wait(context.Background(), as("bob", "grant:bob-0"), 10, bob.read)
		return err
	})
	if !eventually(2*time.Second, func() bool { return bob.reads.Load() >= 3 }) {
		t.Fatalf("another user's wait never polled: %d reads", bob.reads.Load())
	}
	if replica, _, _ := w.CountsForTest(); replica != 5 {
		t.Fatalf("replica count %d while another user waits, want 5", replica)
	}
	bob.settled.Store(true)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if out.Reason != sessionactivity.ReasonSettled || out.Waited <= 0 {
		t.Fatalf("another user's wait = %+v, want a real wait that ends settled", out)
	}

	alice.end(t)
	requireNothingCounted(t, w)

	again := startBurst(t, w, []sessionactivity.Caller{
		as("alice", "user:alice"), as("alice", "user:alice"),
		as("alice", "grant:alice-0"), as("alice", "grant:alice-1"), as("alice", "grant:alice-2"),
	})
	defer again.end(t)
	if all, _ := again.admittedTotal("alice"); all != 4 || again.answered[sessionactivity.ReasonCapacity] != 1 {
		t.Fatalf("once the user's waits ended: %d admitted, answered %v, want the 4 slots back and 1 capacity", all, again.answered)
	}
}

// TestWait_ConcurrentAdmissionsNeverOvershootACap_Race (review round 1's
// P4): bursts of waits far past a cap, released together by a barrier so
// their admissions race, are admitted exactly up to the cap that binds --
// one key's, one user's over many grants, the replica's over many users,
// and the shipped caps under a burst that mixes all three -- and never one
// more: no key, no user and not the replica ever counts past its cap, and
// every other wait answers "capacity". Nothing is released before the
// counts are read, so what they show is the peak. Several rounds each,
// under -race. Mutation: the caps checked in one critical section and the
// counts taken in another (the burst overshoots).
func TestWait_ConcurrentAdmissionsNeverOvershootACap_Race(t *testing.T) {
	var mixed []sessionactivity.Caller
	for u := range 16 {
		mixed = append(mixed, grants(fmt.Sprintf("user%02d", u), 16, 4)...)
	}
	var manyUsers []sessionactivity.Caller
	for u := range 64 {
		manyUsers = append(manyUsers, grants(fmt.Sprintf("user%02d", u), 4, 4)...)
	}
	for _, tc := range []struct {
		name         string
		callers      []sessionactivity.Caller
		wantAdmitted int
	}{
		{"one key's 64 waits: the key's cap", grants("alice", 64, 64), 2},
		{"one user's 64 waits over 16 grants: the user's cap", grants("alice", 64, 4), 4},
		{"64 users' 256 waits, 4 on one grant each: the replica's cap", manyUsers, 32},
		{"16 users' 256 waits, 4 grants each: every cap at once", mixed, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := shippedCaps(t)
			for round := range 5 {
				w := sessionactivity.NewWaiter(cfg)
				b := startBurst(t, w, tc.callers)
				replica, perUser, perKey := w.CountsForTest()
				all, _ := b.admittedTotal("")
				refused := b.answered[sessionactivity.ReasonCapacity]
				b.end(t)
				if replica != tc.wantAdmitted || all != tc.wantAdmitted {
					t.Fatalf("round %d: replica count %d, %d admitted, want exactly %d", round, replica, all, tc.wantAdmitted)
				}
				if refused != len(tc.callers)-tc.wantAdmitted || len(b.answered) != 1 {
					t.Fatalf("round %d: answered %v, want %d capacity and nothing else", round, b.answered, len(tc.callers)-tc.wantAdmitted)
				}
				for user, n := range perUser {
					if n > cfg.MaxPerUser {
						t.Fatalf("round %d: user %s counts %d waits, past the cap of %d", round, user, n, cfg.MaxPerUser)
					}
				}
				for key, n := range perKey {
					if n > cfg.MaxPerKey {
						t.Fatalf("round %d: key %s counts %d waits, past the cap of %d", round, key, n, cfg.MaxPerKey)
					}
				}
				requireNothingCounted(t, w)
			}
		})
	}
}

// errBoom is a read's own failure.
var errBoom = errors.New("boom")

// waitPath is one way a wait can end, run by alice through her grant a1 on
// a Waiter of its own built from shippedCaps adjusted by cfg. held is
// called while waits the path started are admitted -- from inside the
// wait's own later read when the path's own wait is, and while blockers
// hold the cap when it is refused -- with how many the replica then
// counts. A path whose wait takes no slot, and no blocker, never calls it:
// holds says which kind the path is.
type waitPath struct {
	name  string
	holds bool
	cfg   func(*sessionactivity.Config)
	run   func(t *testing.T, w *sessionactivity.Waiter, held func(n int))
}

// alice is the caller every waitPath waits as.
var alice = as("alice", "grant:a1")

// laterRead is a Read that answers unsettled on its first call and, from
// its second -- which only an admitted wait takes -- calls then(n) with the
// read's number.
func laterRead(then func(n int64) (bool, error)) sessionactivity.Read {
	var reads atomic.Int64
	return func(context.Context) (bool, error) {
		n := reads.Add(1)
		if n == 1 {
			return false, nil
		}
		return then(n)
	}
}

// refusedWhileBlocked is a waitPath refused by a cap blockers hold: the
// blockers are admitted first, every one of them, then alice's wait answers
// "capacity" at once, held reports the blockers, and they end.
func refusedWhileBlocked(blockers []sessionactivity.Caller) func(*testing.T, *sessionactivity.Waiter, func(int)) {
	return func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		b := startBurst(t, w, blockers)
		defer b.end(t)
		if all, _ := b.admittedTotal(""); all != len(blockers) {
			t.Fatalf("%d of %d blockers admitted, want all of them", all, len(blockers))
		}
		own := &session{}
		out, err := w.Wait(context.Background(), alice, 10, own.read)
		if err != nil || out.Reason != sessionactivity.ReasonCapacity {
			t.Fatalf("the refused wait = %+v, %v, want capacity", out, err)
		}
		held(len(blockers))
	}
}

// waitPaths is every way a wait ends: answered by its first read (settled,
// a read error, a server already shutting down), or admitted and then
// ended by a later settled read, its bound, a read error, the client going
// away, an interrupt or a panic in its read; or refused by each of the
// three caps.
var waitPaths = []waitPath{
	{name: "settled at the first read", run: func(t *testing.T, w *sessionactivity.Waiter, _ func(int)) {
		s := &session{}
		s.settled.Store(true)
		if out, err := w.Wait(context.Background(), alice, 10, s.read); err != nil || out.Reason != sessionactivity.ReasonSettled {
			t.Fatalf("Wait = %+v, %v, want settled", out, err)
		}
	}},
	{name: "settled on a later read", holds: true, run: func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		out, err := w.Wait(context.Background(), alice, 10, laterRead(func(n int64) (bool, error) {
			if n == 2 {
				held(1)
				return false, nil
			}
			return true, nil
		}))
		if err != nil || out.Reason != sessionactivity.ReasonSettled {
			t.Fatalf("Wait = %+v, %v, want settled", out, err)
		}
	}},
	{name: "its bound", holds: true, cfg: func(c *sessionactivity.Config) { c.MaxDuration = 100 * time.Millisecond }, run: func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		out, err := w.Wait(context.Background(), alice, 10, laterRead(func(n int64) (bool, error) {
			if n == 2 {
				held(1)
			}
			return false, nil
		}))
		if err != nil || out.Reason != sessionactivity.ReasonTimeout {
			t.Fatalf("Wait = %+v, %v, want timeout", out, err)
		}
	}},
	{name: "a read error on the first read", run: func(t *testing.T, w *sessionactivity.Waiter, _ func(int)) {
		if _, err := w.Wait(context.Background(), alice, 10, func(context.Context) (bool, error) { return false, errBoom }); !errors.Is(err, errBoom) {
			t.Fatalf("Wait error = %v, want boom", err)
		}
	}},
	{name: "a read error on a later read", holds: true, run: func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		_, err := w.Wait(context.Background(), alice, 10, laterRead(func(n int64) (bool, error) {
			if n == 2 {
				held(1)
				return false, nil
			}
			return false, errBoom
		}))
		if !errors.Is(err, errBoom) {
			t.Fatalf("Wait error = %v, want boom", err)
		}
	}},
	{name: "the client going away", holds: true, run: func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := w.Wait(ctx, alice, 10, laterRead(func(int64) (bool, error) {
			held(1)
			cancel()
			return false, nil
		}))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait error = %v, want context.Canceled", err)
		}
	}},
	{name: "an interrupt while it waits", holds: true, run: func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		out, err := w.Wait(context.Background(), alice, 10, laterRead(func(int64) (bool, error) {
			held(1)
			w.Interrupt()
			return false, nil
		}))
		if err != nil || out.Reason != sessionactivity.ReasonInterrupted {
			t.Fatalf("Wait = %+v, %v, want interrupted", out, err)
		}
	}},
	{name: "an interrupt before it began", run: func(t *testing.T, w *sessionactivity.Waiter, _ func(int)) {
		w.Interrupt()
		if out, err := w.Wait(context.Background(), alice, 10, (&session{}).read); err != nil || out.Reason != sessionactivity.ReasonInterrupted {
			t.Fatalf("Wait = %+v, %v, want interrupted", out, err)
		}
	}},
	{name: "a panic in a later read", holds: true, run: func(t *testing.T, w *sessionactivity.Waiter, held func(int)) {
		defer func() {
			if r := recover(); r != errBoom {
				t.Fatalf("recovered %v, want the read's own panic", r)
			}
		}()
		_, _ = w.Wait(context.Background(), alice, 10, laterRead(func(int64) (bool, error) {
			held(1)
			panic(errBoom)
		}))
		t.Fatal("Wait returned, want the read's panic to propagate")
	}},
	{name: "capacity: the key's cap", holds: true, run: refusedWhileBlocked([]sessionactivity.Caller{alice, alice})},
	{name: "capacity: the user's cap", holds: true, run: refusedWhileBlocked(grants("alice", 4, 2))},
	{name: "capacity: the replica's cap", holds: true, run: refusedWhileBlocked(func() []sessionactivity.Caller {
		var out []sessionactivity.Caller
		for u := range 16 {
			out = append(out, grants(fmt.Sprintf("user%02d", u), 2, 2)...)
		}
		return out
	}())},
}

// newPathWaiter is the Waiter path p runs on.
func newPathWaiter(t *testing.T, p waitPath) *sessionactivity.Waiter {
	t.Helper()
	cfg := shippedCaps(t)
	if p.cfg != nil {
		p.cfg(&cfg)
	}
	return sessionactivity.NewWaiter(cfg)
}

// TestWait_EveryPathGivesItsSlotsBack (review round 1's P1): on every path
// a wait can end by -- an error, the client going away, its bound, an
// interrupt, a panic in its read, or a refusal by any of the three caps --
// its replica, user and key counts all come back to zero, with no entry
// left behind, while an admitted wait was counted once in each. Mutation:
// the per-user count never given back (alice's entry outlives her wait).
func TestWait_EveryPathGivesItsSlotsBack(t *testing.T) {
	for _, p := range waitPaths {
		t.Run(p.name, func(t *testing.T) {
			w := newPathWaiter(t, p)
			sawHeld := false
			p.run(t, w, func(n int) {
				sawHeld = true
				replica, perUser, perKey := w.CountsForTest()
				users, keys := 0, 0
				for _, c := range perUser {
					users += c
				}
				for _, c := range perKey {
					keys += c
				}
				if replica != n || users != n || keys != n {
					t.Errorf("while %d waits are admitted: replica %d, users %v, keys %v, want each wait counted once in each", n, replica, perUser, perKey)
				}
			})
			if sawHeld != p.holds {
				t.Fatalf("held called: %v, want %v", sawHeld, p.holds)
			}
			requireNothingCounted(t, w)
		})
	}
}

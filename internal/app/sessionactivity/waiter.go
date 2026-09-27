// Package sessionactivity is row 182's bounded wait (technical plan §43.20,
// piece (b)): the blocking form of GET /api/sessions/{sessionID}/status,
// and so of narvi_wait_for_session, which bridges to that route.
//
// A Waiter knows nothing about what a session's status is made of. Its
// caller -- the status handler, the one adapter (§43.7) -- hands it a Read
// that takes one snapshot the way a plain status read does (one statement,
// session.DeriveActivity over it) and reports whether that snapshot is
// settled. The Waiter decides only when to read again, and for how long:
//
//   - it reads at once, and returns if the session is settled;
//   - otherwise it sleeps MCPWaitPollInterval on a timer, reads again, and
//     repeats until the session is settled or MCPWaitMaxDuration has
//     passed, then answers the latest read;
//   - it holds nothing between two reads: each Read takes a pool connection
//     for its one statement and gives it back, so a thousand sleeping
//     waits hold no connection at all;
//   - it wakes only by reading Postgres, the one authority (§5.1): the
//     in-process event hub hears only the actors of its own replica, and a
//     wait may be served by any replica;
//   - it stops when the request's context ends (the client went away) and
//     when Interrupt is called (the server is shutting down), so a wait
//     never outlives the drain.
//
// Waits are capped in memory, per replica, by three nested caps checked and
// taken together: MCPWaitMaxConcurrentPerKey per key (the MCP grant, or
// the user for a cookie request), MCPWaitMaxConcurrentPerUser per user
// across every grant of theirs and their browser, and
// MCPWaitMaxConcurrentPerReplica in all. A wait past any of them does not
// wait -- it answers its first read at once, with ReasonCapacity, a normal
// answer, never an error -- so a busy replica degrades to plain status
// reads rather than refusing them.
package sessionactivity

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/platform"
)

// Reason is why a wait ended: the wire's wait.reason
// (restdtos.SessionActivityWaitReason).
type Reason string

const (
	// ReasonSettled means a read found the session settled -- finished, idle or
	// awaiting a person (session.Activity.Settled).
	ReasonSettled Reason = "settled"
	// ReasonTimeout means the wait ran to its bound, the requested seconds
	// clamped to MCPWaitMaxDuration, and the latest read is still not
	// settled. The next step is to wait again, not to sleep.
	ReasonTimeout Reason = "timeout"
	// ReasonInterrupted means the server began shutting down (Interrupt). The
	// latest read is answered at once.
	ReasonInterrupted Reason = "interrupted"
	// ReasonCapacity means one of the three caps was reached on this
	// replica: the caller's key already had MCPWaitMaxConcurrentPerKey
	// waits running, its user MCPWaitMaxConcurrentPerUser across all of
	// their keys, or the replica MCPWaitMaxConcurrentPerReplica; the first
	// read is answered at once.
	ReasonCapacity Reason = "capacity"
)

// Outcome is how one wait ended.
type Outcome struct {
	Reason Reason
	// Waited is how long the wait blocked, from its first read to its
	// answer: zero when that first read was answered at once (settled, or
	// over a cap).
	Waited time.Duration
}

// Read takes one snapshot of the session's status -- retaining it for the
// caller's answer -- and reports whether it is settled. It must run on ctx
// and release every connection it takes before it returns: the Waiter
// sleeps between two calls and holds nothing across the sleep.
type Read func(ctx context.Context) (settled bool, err error)

// Config is the Waiter's bounds: the platform.Timeouts MCPWait* fields.
// The caps nest, MaxPerKey <= MaxPerUser <= MaxPerReplica
// (platform.Timeouts.Validate).
type Config struct {
	MaxDuration   time.Duration
	PollInterval  time.Duration
	MaxPerKey     int
	MaxPerUser    int
	MaxPerReplica int
}

// Caller is whose caps one wait counts against. Neither field is a
// permission: the wait is authorized exactly as the plain read is, by its
// caller, before it ever reaches the Waiter.
type Caller struct {
	// Key is what MaxPerKey counts: the MCP grant the request was
	// authenticated under, or the signed-in user for a cookie request.
	Key string
	// User is what MaxPerUser counts: the signed-in user, whichever
	// surface the wait came through -- so every grant of one user and
	// their browser share it, however many clients they have authorized.
	User string
}

// ConfigFrom is Config from the MCPWait* fields of t, the only place those
// values live.
func ConfigFrom(t platform.Timeouts) Config {
	return Config{
		MaxDuration:   t.MCPWaitMaxDuration,
		PollInterval:  t.MCPWaitPollInterval,
		MaxPerKey:     t.MCPWaitMaxConcurrentPerKey,
		MaxPerUser:    t.MCPWaitMaxConcurrentPerUser,
		MaxPerReplica: t.MCPWaitMaxConcurrentPerReplica,
	}
}

// Waiter runs bounded waits (the package comment). One per replica: the
// composition root builds it once and hands the same value to the status
// route and to the MCP twin, so both count against the same caps, and
// wires Interrupt to the HTTP server's shutdown.
type Waiter struct {
	cfg Config

	// mu guards the three counts, which admit checks and takes, and
	// release gives back, in one critical section each.
	mu      sync.Mutex
	active  int
	perUser map[string]int
	perKey  map[string]int

	interrupted   chan struct{}
	interruptOnce sync.Once
}

// NewWaiter builds a Waiter bounded by cfg.
func NewWaiter(cfg Config) *Waiter {
	return &Waiter{
		cfg:         cfg,
		perUser:     map[string]int{},
		perKey:      map[string]int{},
		interrupted: make(chan struct{}),
	}
}

// Interrupt ends every running wait at once with ReasonInterrupted, and
// every later one after its first read. The HTTP server calls it when its
// shutdown begins (http.Server.RegisterOnShutdown), so a 25-second wait
// never holds the drain. Safe to call more than once, and concurrently.
func (w *Waiter) Interrupt() {
	w.interruptOnce.Do(func() { close(w.interrupted) })
}

// Active is how many waits are blocked right now on this Waiter (the
// mcp_session_waits_active gauge's own count, for tests).
func (w *Waiter) Active() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}

// Bound is how long a wait asking for waitSeconds may block: waitSeconds,
// clamped to MaxDuration. A value past MaxDuration's whole seconds is
// clamped before it is ever converted, so no value a caller sends can
// overflow a Duration. A value below one second (the route treats zero as
// a plain read) is no wait.
func (w *Waiter) Bound(waitSeconds int64) time.Duration {
	if waitSeconds <= 0 {
		return 0
	}
	if waitSeconds > int64(platform.DurationToSeconds(w.cfg.MaxDuration)) {
		return w.cfg.MaxDuration
	}
	return platform.SecondsToDuration(waitSeconds)
}

// Wait reads at once through read and, while the session is not settled,
// again every PollInterval, for at most Bound(waitSeconds), counting
// against caller's key, caller's user and the replica. It returns how the
// wait ended; the caller answers the snapshot its read retained last. An
// error is read's own -- the wait ends on it -- or ctx's when the request
// ended first: then nothing is left to answer.
func (w *Waiter) Wait(ctx context.Context, caller Caller, waitSeconds int64, read Read) (Outcome, error) {
	start := time.Now()
	settled, err := read(ctx)
	if err != nil {
		return Outcome{}, err
	}
	if settled {
		return Outcome{Reason: ReasonSettled}, nil
	}
	select {
	case <-w.interrupted:
		return Outcome{Reason: ReasonInterrupted}, nil
	default:
	}
	release, ok := w.admit(ctx, caller)
	if !ok {
		return Outcome{Reason: ReasonCapacity}, nil
	}
	defer release()

	deadline := start.Add(w.Bound(waitSeconds))
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Outcome{Reason: ReasonTimeout, Waited: time.Since(start)}, nil
		}
		timer := time.NewTimer(min(w.cfg.PollInterval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return Outcome{}, ctx.Err()
		case <-w.interrupted:
			timer.Stop()
			return Outcome{Reason: ReasonInterrupted, Waited: time.Since(start)}, nil
		case <-timer.C:
		}
		settled, err := read(ctx)
		if err != nil {
			return Outcome{}, err
		}
		if settled {
			return Outcome{Reason: ReasonSettled, Waited: time.Since(start)}, nil
		}
	}
}

// admit takes one of the replica's slots, one of c.User's and one of
// c.Key's, or none when any of the three caps is reached: the three checks
// and the three increments are one critical section, so concurrent
// admissions can never overshoot a cap. release gives all three back, in
// one critical section too.
func (w *Waiter) admit(ctx context.Context, c Caller) (release func(), ok bool) {
	w.mu.Lock()
	if w.active >= w.cfg.MaxPerReplica || w.perUser[c.User] >= w.cfg.MaxPerUser || w.perKey[c.Key] >= w.cfg.MaxPerKey {
		w.mu.Unlock()
		return nil, false
	}
	w.active++
	w.perUser[c.User]++
	w.perKey[c.Key]++
	w.mu.Unlock()
	gauge := activeWaitsGauge()
	if gauge != nil {
		gauge.Add(ctx, 1)
	}
	return func() {
		w.mu.Lock()
		w.active--
		giveBack(w.perUser, c.User)
		giveBack(w.perKey, c.Key)
		w.mu.Unlock()
		if gauge != nil {
			// A fresh context: the request's may already be done, and
			// the count must come back down all the same.
			gauge.Add(context.Background(), -1)
		}
	}, true
}

// giveBack takes one from counts[k], deleting the entry once it is spent,
// so the maps hold only the keys and users with a wait running. Called
// with the Waiter's mu held.
func giveBack(counts map[string]int, k string) {
	if counts[k]--; counts[k] <= 0 {
		delete(counts, k)
	}
}

// meterName follows this codebase's "narvi/<package>" meter-name
// convention (e.g. sessionactor's "narvi/sessionactor").
const meterName = "narvi/sessionactivity"

var (
	activeWaitsOnce sync.Once
	activeWaits     metric.Int64UpDownCounter
)

// activeWaitsGauge lazily builds mcp_session_waits_active against the
// global MeterProvider (platform.SetupOTel's target) exactly once, like
// reviewcontext's knowledge_block_tokens_estimated. A construction failure
// leaves it nil and every Add is skipped: losing a gauge must never fail
// or delay a wait.
func activeWaitsGauge() metric.Int64UpDownCounter {
	activeWaitsOnce.Do(func() {
		c, err := otel.Meter(meterName).Int64UpDownCounter(
			"mcp_session_waits_active",
			metric.WithDescription("Bounded session waits (GET /api/sessions/{sessionID}/status?waitSeconds=, narvi_wait_for_session; technical plan §43.20) blocked on this replica right now. Each is capped per caller and per replica, and holds no database connection while it sleeps."),
			metric.WithUnit("{wait}"),
		)
		if err != nil {
			return
		}
		activeWaits = c
	})
	return activeWaits
}

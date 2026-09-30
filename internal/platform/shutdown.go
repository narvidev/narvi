package platform

import (
	"context"
	"sync/atomic"
)

// ShutdownState is this process's own shutdown state (technical plan §5.1):
// whether this process has begun to shut down. The control plane's run path
// sets it once, when its drain begins (controlplane.App.Run, through Bind),
// and a worker that must tell an interruption its own process caused from a
// failure reads it -- the outbox delivery worker is the first
// (internal/app/outboxworker).
//
// It exists because a cancellation error cannot answer that question. A
// delivery's context wraps its worker's, so a delivery that ended with
// context.Canceled may have been cut by this process's shutdown, or by
// anything else that ends a context; and one that ended with an ordinary
// error may have been cut the same way, by a notifier that wraps the
// cancellation into an error of its own. This state is set by the one event
// it names and read by value.
//
// The zero value is ready to use and reads as not begun. Safe for concurrent
// use: one atomic, written once and read by every worker.
type ShutdownState struct {
	begun atomic.Bool
}

// Begin records that this process's shutdown has begun. It never un-begins:
// a second call changes nothing.
func (s *ShutdownState) Begin() {
	s.begun.Store(true)
}

// Begun reports whether this process's shutdown has begun.
func (s *ShutdownState) Begun() bool {
	return s.begun.Load()
}

// Bind returns a context that carries parent's values and ends only once
// this process's shutdown has begun: when parent ends, Begin runs first and
// the returned context is cancelled after it. Code that sees the returned
// context end -- a delivery whose context derives from it, cut short --
// therefore always reads Begun as true, with no window in which the
// cancellation has arrived and the state has not.
//
// A worker run on parent directly has that window: parent's cancellation
// reaches the worker at once, and whatever sets the state afterwards, in
// another goroutine, may run after the worker has already recorded its
// interrupted work as a failure.
//
// The returned cancel ends the returned context without beginning shutdown,
// and stops watching parent; call it once the worker has returned.
func (s *ShutdownState) Bind(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, func() {
		s.Begin()
		cancel()
	})
	return ctx, func() {
		stop()
		cancel()
	}
}

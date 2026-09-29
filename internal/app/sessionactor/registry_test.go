package sessionactor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/platform"
)

// TestRegistry_ActorContextReleasedWhenRunEnds proves an actor that ends on
// its own -- its idle TTL, the way every actor normally ends -- releases
// its own context (Registry.start) instead of leaving it registered on the
// Registry's lifecycle context until shutdown, one per actor ever hydrated.
// The actor's stopping channel is that context's Done channel: it closes
// only if the context is cancelled, and the lifecycle context above it is
// still live, so a closed one proves the actor's own cancel ran, which is
// also what unregisters it. Without the release, no stopping channel
// closes.
//
// No database is needed: the pool is never dialled (it points nowhere),
// the actors take no lock (the holder has no connection, so the unlock
// their shutdown runs is a no-op), and they run no command.
func TestRegistry_ActorContextReleasedWhenRunEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, "postgres://narvi@127.0.0.1:1/narvi?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	timeouts := platform.DefaultTimeouts()
	timeouts.ActorIdleTTL = time.Millisecond
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	const actors = 50
	started := make([]*Actor, actors)
	for i := range started {
		var id pgtype.UUID
		id.Bytes[0], id.Bytes[1], id.Valid = byte(i), 0xAC, true
		a := &Actor{
			sessionID: id,
			timeouts:  timeouts,
			registry:  r,
			lockGen:   r.locks.currentGen(),
			mailbox:   make(chan Command, mailboxBufferSize),
			done:      make(chan struct{}),
			logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if err := r.start(ctx, a); err != nil {
			t.Fatalf("start actor %d: %v", i, err)
		}
		started[i] = a
	}

	for i, a := range started {
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("actor %d did not idle out", i)
		}
		select {
		case <-a.stopping:
		case <-time.After(5 * time.Second):
			t.Fatalf("actor %d idled out but its context was never released: it stays registered on the Registry's lifecycle context until shutdown", i)
		}
	}
	if err := r.lifecycleCtx.Err(); err != nil {
		t.Fatalf("the Registry's lifecycle context is done (%v): the actors' contexts closed with it, which proves nothing", err)
	}
	if got := r.lookup(started[0].sessionID); got != nil {
		t.Fatalf("an idled-out actor is still in the Registry's map")
	}
}

package platform_test

import (
	"context"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/platform"
)

type shutdownKey struct{}

// TestShutdownState_Bind pins what a worker run on a bound context may rely
// on: the context ends only once shutdown has begun, so a reader that sees
// it end always reads Begun as true, and it keeps parent's values. The
// returned cancel ends it without beginning shutdown.
//
// The reader spins on the bound context rather than parking on it: a
// parked reader wakes microseconds after the cancellation, long after any
// Begin that followed it, and would not see the ordering broken.
func TestShutdownState_Bind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		end        func(cancelParent, cancelBound context.CancelFunc)
		wantBegun  bool
		iterations int
	}{
		{
			name:       "parent ends: shutdown begins before the bound context ends",
			end:        func(cancelParent, _ context.CancelFunc) { cancelParent() },
			wantBegun:  true,
			iterations: 2000,
		},
		{
			name:       "the returned cancel ends it without beginning shutdown",
			end:        func(_, cancelBound context.CancelFunc) { cancelBound() },
			wantBegun:  false,
			iterations: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for range tc.iterations {
				var state platform.ShutdownState
				parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), shutdownKey{}, "kept"))
				bound, cancelBound := state.Bind(parent)

				if state.Begun() {
					t.Fatal("Begun() = true before anything ended")
				}
				if got := bound.Value(shutdownKey{}); got != "kept" {
					t.Fatalf("bound context value = %v, want parent's", got)
				}

				var reader errgroup.Group
				var begunWhenEnded atomic.Bool
				reader.Go(func() error {
					for bound.Err() == nil { // spin: see the doc comment
						continue
					}
					begunWhenEnded.Store(state.Begun())
					return nil
				})
				tc.end(cancelParent, cancelBound)
				_ = reader.Wait()
				if got := begunWhenEnded.Load(); got != tc.wantBegun {
					t.Fatalf("Begun() once the bound context ended = %v, want %v", got, tc.wantBegun)
				}

				cancelParent()
				cancelBound()
			}
		})
	}
}

// TestShutdownState_BeginIsOneWay pins that the state is set once and never
// cleared, whatever calls follow.
func TestShutdownState_BeginIsOneWay(t *testing.T) {
	t.Parallel()

	var state platform.ShutdownState
	if state.Begun() {
		t.Fatal("zero value Begun() = true, want false")
	}
	state.Begin()
	state.Begin()
	if !state.Begun() {
		t.Fatal("Begun() after Begin = false, want true")
	}
}

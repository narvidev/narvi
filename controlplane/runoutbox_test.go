package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

// TestRunOutbox pins where this process's shutdown state is set (technical
// plan §5.1): the outbox delivery worker Run starts sees its context end
// only once the state reads begun, whether the run context ended for a
// signal or another loop's failure; its ordinary context.Canceled is no
// error, and any other error is returned.
func TestRunOutbox(t *testing.T) {
	t.Parallel()

	workerFailed := errors.New("worker failed")
	for _, tc := range []struct {
		name    string
		result  error
		wantErr error
	}{
		{name: "the worker returns its context's cancellation", result: context.Canceled},
		{name: "the worker fails on its own", result: workerFailed, wantErr: workerFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Repeated: a worker context ended before the state was set
			// would be seen by some of these iterations.
			for range 100 {
				var state platform.ShutdownState
				groupCtx, endGroup := context.WithCancel(context.Background())
				var begunWhenEnded bool
				err := runOutbox(groupCtx, &state, func(ctx context.Context) error {
					endGroup()
					<-ctx.Done()
					begunWhenEnded = state.Begun()
					return tc.result
				})
				endGroup()

				if !begunWhenEnded {
					t.Fatal("the worker's context ended before this process's shutdown state was set")
				}
				if tc.wantErr == nil && err != nil {
					t.Fatalf("runOutbox = %v, want nil for a worker ended by the shutdown", err)
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("runOutbox = %v, want %v", err, tc.wantErr)
				}
			}
		})
	}
}

package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestDeliverBatch_StopsAtFirstUnavailable proves the timer pump's batch
// loop (deliverBatch, timerpump.go) ends a batch at the first delivery
// that fails with ErrActorUnavailable -- that timer and every one after it
// are skipped, left to come back when their claims expire -- while any
// other failure, ErrSessionActorElsewhere included, lets the batch go on.
func TestDeliverBatch_StopsAtFirstUnavailable(t *testing.T) {
	t.Parallel()

	unavailable := fmt.Errorf("%w: take advisory lock: pool saturated", ErrActorUnavailable)
	for _, tc := range []struct {
		name          string
		failures      map[int]error // 0-based index -> the error deliver returns for it
		wantCalls     int
		wantDelivered int
		wantSkipped   int
	}{
		{"every delivery succeeds", nil, 5, 5, 0},
		{"unavailable on the 2nd of 5", map[int]error{1: unavailable}, 2, 1, 4},
		{"unavailable on the 1st", map[int]error{0: unavailable}, 1, 0, 5},
		{"unavailable on the last", map[int]error{4: unavailable}, 5, 4, 1},
		{"elsewhere does not end the batch", map[int]error{1: ErrSessionActorElsewhere}, 5, 5, 0},
		{"another error does not end the batch", map[int]error{1: errors.New("boom"), 2: ErrActorStopped}, 5, 5, 0},
		{"the first unavailable ends it, not a later one", map[int]error{2: unavailable, 3: unavailable}, 3, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			timers := make([]sqlcgen.SessionTimer, 5)
			for i := range timers {
				timers[i] = sqlcgen.SessionTimer{
					SessionID: pgtype.UUID{Bytes: [16]byte{byte(i + 1)}, Valid: true},
					Name:      fmt.Sprintf("timer-%d", i),
				}
			}
			var calls []string
			deliver := func(_ context.Context, timer sqlcgen.SessionTimer) error {
				calls = append(calls, timer.Name)
				return tc.failures[len(calls)-1]
			}

			delivered, skipped := deliverBatch(context.Background(), timers, deliver)

			if len(calls) != tc.wantCalls {
				t.Errorf("deliver called %d times (%v), want %d", len(calls), calls, tc.wantCalls)
			}
			for i, name := range calls {
				if want := fmt.Sprintf("timer-%d", i); name != want {
					t.Errorf("call %d delivered %q, want %q (in claim order)", i, name, want)
				}
			}
			if delivered != tc.wantDelivered || skipped != tc.wantSkipped {
				t.Errorf("deliverBatch = (delivered %d, skipped %d), want (%d, %d)", delivered, skipped, tc.wantDelivered, tc.wantSkipped)
			}
		})
	}
}

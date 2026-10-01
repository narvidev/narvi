package automation

import (
	"context"
	"time"
)

// EvaluateCronTriggersAtForTest runs one cron-trigger tick evaluated as of
// now instead of the wall clock. For tests only: the external test package
// places its ticks through this file, so whether two of them share a
// minute is fixed by the test rather than by when it happens to run.
func (e *Engine) EvaluateCronTriggersAtForTest(ctx context.Context, now time.Time) error {
	return e.evaluateCronTriggersAt(ctx, now)
}

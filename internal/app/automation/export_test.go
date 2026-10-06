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

// PumpOnceAfterClaimForTest runs one fan-out tick, running afterClaim once
// the batch's claim has committed and before any invocation fans out. For
// tests only: a freeze committed in afterClaim lands after the claim.
func (e *Engine) PumpOnceAfterClaimForTest(ctx context.Context, afterClaim func()) error {
	return e.pumpOnce(ctx, afterClaim)
}

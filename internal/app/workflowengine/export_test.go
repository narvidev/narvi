package workflowengine

import (
	"context"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReleaseHoldForTest is one held advance's release past the tick's own read
// of the freeze -- what a release does when a freeze lands after ReleaseOnce
// read it -- for the external tests. It reports whether the advance was
// applied or its run cancelled.
func (r *HeldAdvanceReleaser) ReleaseHoldForTest(ctx context.Context, h sqlcgen.WorkflowAdvanceHold) (bool, error) {
	result, err := r.releaseHold(ctx, h)
	return result == releaseApplied || result == releaseCancelled, err
}

// SetPageSizeForTest sets how many held advances one page of a release
// tick reads, so a test pages over a few holds rather than fifty.
func (r *HeldAdvanceReleaser) SetPageSizeForTest(n int32) {
	r.pageSize = n
}

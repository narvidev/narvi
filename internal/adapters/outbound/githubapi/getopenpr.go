// This file (getopenpr.go) implements ports.SourceControl.GetOpenPR
// (§21.2 stage 2) -- see that method's own doc comment (internal/app/
// ports/sourcecontrol.go) for the full "why a machine-initiated caller
// needs a DIFFERENT discovery primitive than ListOpenPRsForUser" design.

package githubapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/narvidev/narvi/internal/app/ports"
)

// GetOpenPR fetches owner/repo#number directly and builds its current
// ports.OpenPR state via buildOpenPRFromDetail (listopenprs.go) -- the
// EXACT same construction ListOpenPRsForUser's own per-candidate loop
// uses, never a second, independently-maintained one.
//
// found=false, err=nil is returned for a confirmed GitHub 404 (the PR
// does not exist, or this token cannot see it) OR a confirmed-present PR
// that is no longer open (closed or merged, round-10 finding E:
// detail.State != "open" -- openPRDetailResponse's own doc comment,
// listopenprs.go, for why checking State alone catches both) --
// distinguished from every OTHER failure (rate-limited, transient 5xx,
// decode error), which is a genuine err != nil a caller must not
// silently treat as "not open". Before this fix, a closed-or-merged PR
// (a real, decodable 200 response) reached found=true identically to a
// genuinely still-open one, which is exactly what let
// internal/app/decisioninbox.RevalidateForAutoMerge's own "this pull
// request is no longer open" refusal (revalidate.go, RevalidateForAutoMerge's
// own doc comment) go permanently unreachable: GetOpenPR itself never
// reported found=false for that case. This finer distinction is exactly
// why this method calls fetchOpenPRDetail directly rather than through
// buildOpenPR (which collapses every failure into a single bool, the
// right shape for its OWN caller's "best-effort, drop this one row"
// semantics, the wrong shape here: internal/app/automerge's own caller
// must fail a merge attempt CLOSED on a genuine error, never silently
// skip it as though the PR had simply been closed).
func (a *Adapter) GetOpenPR(ctx context.Context, owner, repo string, number int, token string) (ports.OpenPR, bool, error) {
	detail, err := a.fetchOpenPRDetail(ctx, owner, repo, number, token)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return ports.OpenPR{}, false, nil
		}
		return ports.OpenPR{}, false, err
	}
	if detail.State != "open" {
		return ports.OpenPR{}, false, nil
	}
	return a.buildOpenPRFromDetail(ctx, owner, repo, detail, token), true, nil
}

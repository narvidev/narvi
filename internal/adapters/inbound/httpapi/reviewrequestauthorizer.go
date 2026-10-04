package httpapi

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// reviewRequestAuthzSurface names this check in the authorization log.
const reviewRequestAuthzSurface = "review-request"

// ReviewRequestAuthorizer is ports.ReviewRequestAuthorizer from Narvi's own
// role-based access control (technical plan §13.3, §24.9): before the
// session actor re-runs a person's review request whose pull request moved
// while it waited, it asks again the check that request's lane applied
// when it was made, against the requester's account as it stands now. The
// two lanes whose request can be owed -- the web re-review button and the
// configured label -- both checked "re-trigger reviews"
// (authz.ActionRetriggerReview): admin or maintainer, with no member
// carve-out, owner of the session or not (RetriggerReview's check, and
// the GitHub label lane's in github/coalesce.go's REUSE branch). A
// mention's request is never owed, so no lane of it is asked here.
//
// It goes through actorauthz.AuthorizeLinkedActorVerdict, the GitHub
// lanes' linked-actor check: an unknown requester (no account, or one
// deleted since) and a disabled account are denied. A lookup that failed is
// an error, never a denial, so the request is kept and asked again
// (ports.ReviewRequestAuthorizer's contract). A trigger no owed lane
// records is denied.
type ReviewRequestAuthorizer struct {
	users *postgres.UserStore
}

var _ ports.ReviewRequestAuthorizer = (*ReviewRequestAuthorizer)(nil)

// NewReviewRequestAuthorizer builds a ReviewRequestAuthorizer reading
// accounts through users.
func NewReviewRequestAuthorizer(users *postgres.UserStore) *ReviewRequestAuthorizer {
	return &ReviewRequestAuthorizer{users: users}
}

// AuthorizeReviewRequest reports whether req's requester may still have
// their review request re-run (see the type's doc comment).
func (a *ReviewRequestAuthorizer) AuthorizeReviewRequest(ctx context.Context, req ports.ReviewRequest) (bool, error) {
	logger := platform.Logger(ctx)
	switch req.Trigger {
	case turn.RequestTriggerButton, turn.RequestTriggerLabel:
	default:
		logger.Warn("httpapi: a review request through a lane that owes none cannot be authorized", "trigger", req.Trigger)
		return false, nil
	}
	var requester pgtype.UUID
	if req.RequestedBy != "" {
		if err := requester.Scan(req.RequestedBy); err != nil {
			return false, fmt.Errorf("httpapi: parse the review requester %q: %w", req.RequestedBy, err)
		}
	}
	switch actorauthz.AuthorizeLinkedActorVerdict(ctx, logger, reviewRequestAuthzSurface, a.users, requester, authz.ActionRetriggerReview, authz.Resource{}) {
	case actorauthz.LinkedActorAllowed:
		return true, nil
	case actorauthz.LinkedActorDenied:
		return false, nil
	default:
		return false, fmt.Errorf("httpapi: the requester's authorization could not be evaluated")
	}
}

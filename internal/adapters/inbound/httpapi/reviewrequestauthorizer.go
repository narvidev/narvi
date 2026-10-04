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
// when it was made, against the requester's account as it stands now.
//
//   - the web re-review button and the configured label: "re-trigger
//     reviews" (authz.ActionRetriggerReview), admin or maintainer, with no
//     member carve-out -- RetriggerReview's check and the GitHub label
//     lane's (github/coalesce.go's REUSE branch);
//   - a mention: "prompt a session" (authz.ActionPromptSession), which a
//     member holds on a session they created or joined -- the GitHub
//     mention lane's check (a review attempt a mention asks for is a
//     session's first turn, created by the mention's author).
//
// Every lane goes through actorauthz.AuthorizeLinkedActorVerdict, the
// GitHub lanes' linked-actor check: an unknown requester (no account, or
// one deleted since) and a disabled account are denied. A lookup that
// failed is an error, never a denial, so the request is kept and asked
// again (ports.ReviewRequestAuthorizer's contract). A trigger no lane
// records is denied.
type ReviewRequestAuthorizer struct {
	users        *postgres.UserStore
	sessions     *postgres.SessionStore
	participants *postgres.ParticipantStore
}

var _ ports.ReviewRequestAuthorizer = (*ReviewRequestAuthorizer)(nil)

// NewReviewRequestAuthorizer builds a ReviewRequestAuthorizer reading
// accounts, sessions and their participants through the given stores.
func NewReviewRequestAuthorizer(users *postgres.UserStore, sessions *postgres.SessionStore, participants *postgres.ParticipantStore) *ReviewRequestAuthorizer {
	return &ReviewRequestAuthorizer{users: users, sessions: sessions, participants: participants}
}

// AuthorizeReviewRequest reports whether req's requester may still have
// their review request re-run (see the type's doc comment).
func (a *ReviewRequestAuthorizer) AuthorizeReviewRequest(ctx context.Context, req ports.ReviewRequest) (bool, error) {
	logger := platform.Logger(ctx)
	var requester pgtype.UUID
	if req.RequestedBy != "" {
		if err := requester.Scan(req.RequestedBy); err != nil {
			return false, fmt.Errorf("httpapi: parse the review requester %q: %w", req.RequestedBy, err)
		}
	}
	var action authz.Action
	var resource authz.Resource
	switch req.Trigger {
	case turn.RequestTriggerButton, turn.RequestTriggerLabel:
		action = authz.ActionRetriggerReview
	case turn.RequestTriggerMention:
		action = authz.ActionPromptSession
		if requester.Valid {
			var sessionID pgtype.UUID
			if err := sessionID.Scan(req.SessionID); err != nil {
				return false, fmt.Errorf("httpapi: parse the review session %q: %w", req.SessionID, err)
			}
			sessionRow, err := a.sessions.Get(ctx, sessionID)
			if err != nil {
				return false, fmt.Errorf("httpapi: get the review session: %w", err)
			}
			joined, err := actorauthz.OwnedOrJoined(ctx, a.participants, sessionRow, requester)
			if err != nil {
				return false, fmt.Errorf("httpapi: check the requester's participation: %w", err)
			}
			resource.OwnedOrJoined = joined
		}
	default:
		logger.Warn("httpapi: a review request through a lane that records no trigger cannot be authorized", "trigger", req.Trigger)
		return false, nil
	}
	switch actorauthz.AuthorizeLinkedActorVerdict(ctx, logger, reviewRequestAuthzSurface, a.users, requester, action, resource) {
	case actorauthz.LinkedActorAllowed:
		return true, nil
	case actorauthz.LinkedActorDenied:
		return false, nil
	default:
		return false, fmt.Errorf("httpapi: the requester's authorization could not be evaluated")
	}
}

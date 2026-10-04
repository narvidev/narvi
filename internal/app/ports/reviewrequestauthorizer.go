package ports

import "context"

// ReviewRequest is a person's review request a session owes its requester
// (technical plan §24.9): their review attempt waited behind another turn,
// met a moved context when it was dispatched, and is about to be re-run
// for the head the pull request has now -- with no new act of that person,
// so whether they may still ask for it is asked again first.
type ReviewRequest struct {
	// SessionID is the review session the request was made on.
	SessionID string
	// RequestedBy is the requester's user id; empty when it is no longer
	// known (the account was deleted), which no implementation may read as
	// authorized.
	RequestedBy string
	// Trigger is the lane the request came through: "label" or "button"
	// (internal/domain/turn's RequestTrigger* values).
	Trigger string
	// RepoFullName is the pull request's base repository, owner/repo.
	RepoFullName string
}

// ReviewRequestAuthorizer answers whether a person may still have a review
// request of theirs re-run on a session's pull request
// (AuthorizeReviewRequest). The session actor, which re-runs owed requests
// and cannot import the HTTP layer, asks it before it inserts the re-run.
//
// It holds for more than one implementation, as every port here must: the
// first answers from Narvi's own role-based access control, the check each
// lane applied when the request was made (internal/adapters/inbound/
// httpapi's ReviewRequestAuthorizer); a second can ask the code host
// whether the requester still has access to the repository, through the
// same interface.
//
// allowed false with a nil error is a verdict: the request is dropped and
// its requester told. A non-nil error says nothing about the requester --
// a lookup that failed -- and the caller keeps the request and asks again
// later, never reading it as a denial.
type ReviewRequestAuthorizer interface {
	AuthorizeReviewRequest(ctx context.Context, req ReviewRequest) (allowed bool, err error)
}

package ports

import "time"

// ReviewCheckPayload is the JSON shape internal/app/outboxworker expects
// to find in an outbox entry's own payload column for a
// NotificationKindGitHubReviewCheck row (the review's own GitHub-native
// result surface, §8.2/§21.1/§21.1b) -- a thin, wire-safe
// mirror of internal/domain/reviewcheck.Emission (that type stays pure
// domain, no json tags of its own, per this codebase's own established
// "domain types carry no wire concerns" convention -- e.g.
// reviewverdict.Context is marshaled by its OWN caller, never tagged
// itself). Owner/Repo are the split form of Emission.RepoFullName every
// other GitHub-flavored payload in this file already carries (mirrors
// VerdictPayload's own identical Owner/Repo/PRNumber shape) -- the
// notifier's own Deliver rejoins them only when it needs the natural key
// back for the Postgres claim-table lookup.
type ReviewCheckPayload struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha"`
	// AttemptID/AttemptCreatedAt are empty/zero for a PhaseQueued emission
	// (no attempt exists yet -- internal/domain/reviewcheck.Emission's own
	// doc comment on AttemptID).
	AttemptID        string    `json:"attempt_id,omitempty"`
	AttemptCreatedAt time.Time `json:"attempt_created_at,omitempty"`
	// Phase is internal/domain/reviewcheck.Phase, carried as a plain
	// string (this package has zero dependency on internal/domain,
	// mirroring every other payload type here) -- the notifier's own
	// Deliver converts it back and refuses to publish one Phase(...)
	// does not recognize as valid, exactly like every other "never trust
	// a stringly-typed payload field" boundary in this codebase.
	Phase string `json:"phase"`
	// BaseRef/BaseSHA/PolicyVersion mirror internal/domain/reviewverdict.
	// Context's own identically-named fields -- carried for parity/audit
	// with the verdict this emission may accompany; not read by the
	// notifier's own supersession decision (attempt recency is).
	BaseRef       string `json:"base_ref,omitempty"`
	BaseSHA       string `json:"base_sha,omitempty"`
	PolicyVersion int    `json:"policy_version,omitempty"`
}

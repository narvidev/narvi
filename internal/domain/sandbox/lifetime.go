package sandbox

// LifetimeKind names the lifetime class a session's sandbox is given
// (technical plan §35.2): the control plane assumes its provider lets a
// sandbox of each kind run for that kind's lifetime, counted from the claim
// that spawns or restores it, and stamps the sandbox row's
// lifetime_deadline_at from it. The values themselves are durations, so
// they live in platform/timeouts.go (Timeouts.SandboxLifetimeFor); this
// package only names the kinds and says which one a session is.
type LifetimeKind string

// The lifetime kinds. A pull request's review session is the profile
// §35.1 names -- repeated work on one session for as long as the pull
// request stays open -- so it is a kind of its own, even while both kinds
// share a value. Every other session is the default kind. Sessions an
// automation created are not told apart yet: that needs
// automation_runs.session_id, which has no index, read on every spawn.
const (
	LifetimeKindDefault LifetimeKind = "default"
	LifetimeKindReview  LifetimeKind = "review"
)

// AllLifetimeKinds returns every kind this package names, in a fixed order
// -- what platform.Timeouts.Validate checks each lifetime of.
func AllLifetimeKinds() []LifetimeKind {
	return []LifetimeKind{LifetimeKindDefault, LifetimeKindReview}
}

// LifetimeKindFor is the kind of a session's sandbox: review for a pull
// request's review session (one with a github_pr_sessions row, which the
// caller reads), default for every other session.
func LifetimeKindFor(isReviewSession bool) LifetimeKind {
	if isReviewSession {
		return LifetimeKindReview
	}
	return LifetimeKindDefault
}

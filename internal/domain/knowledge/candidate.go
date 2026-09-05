package knowledge

import "time"

// Candidate is one gated arch-decision, already selected by the (later
// Step's own) SQL gate -- never constructed by this package itself. Every
// field is either server-derived (the tags/roots the gate matched on,
// stamped at INSERT time from the changed-file paths the source PR
// actually touched -- never the reviewing model's own free-form
// self-report) or was sanitized before it was ever persisted (Decision/
// RejectedAlternative/ConventionConformance, the model's own free-text
// narrative, rendered once and never parsed back out of anything posted --
// see internal/domain/reviewpost's own ArchDecision, the type these three
// fields mirror).
type Candidate struct {
	// ID is "<verdict id>:<index>" -- durable and joinable back to the
	// verdict/decision-within-verdict it came from (a review_verdicts row
	// can carry more than one arch-decision).
	ID string

	// VerdictID is the review_verdicts row this decision was captured
	// from.
	VerdictID string

	// RepoFullName is "owner/repo" for the repository this decision was
	// recorded against -- retrieval never crosses repositories (§31.6's
	// own "strict per-repository isolation").
	RepoFullName string

	// PRNumber is the pull request whose review produced this decision.
	PRNumber int32

	// HeadSHA is that pull request's own head commit at the time the
	// decision was recorded.
	HeadSHA string

	// Decision is what the diff actually decided -- e.g. "introduced a
	// new retry queue table rather than reusing the existing outbox".
	// Mirrors internal/domain/reviewpost.ArchDecision.Decision exactly:
	// free text, rendered, never re-parsed.
	Decision string

	// RejectedAlternative is the alternative this decision implicitly
	// passed over. §31.6 calls this field out by name as the irreplaceable
	// payload retrieval exists to preserve: the road not taken leaves no
	// trace anywhere else a later reviewer could reconstruct it from.
	RejectedAlternative string

	// ConventionConformance is this decision's own conformance to the
	// repository's established conventions, as the reviewing model
	// assessed it -- free text, not a closed vocabulary.
	ConventionConformance string

	// Tags are the fixed-vocabulary blast-radius-shaped tags the gate
	// matched this candidate on -- copied from the source verdict's own
	// INSERT-time stamp (derived server-side from the PR's changed
	// paths), never from anything the reviewing model posted.
	Tags []string

	// Roots are the directory roots the gate matched this candidate on,
	// stamped the same INSERT-time way as Tags.
	Roots []string

	// CreatedAt is when this decision was recorded. The gate's own
	// fallback path orders candidates by this field, descending; a ranker
	// with no opinion (scores == nil) preserves that same order.
	CreatedAt time.Time

	// KnowledgeInfluenced records whether the turn that PRODUCED this
	// decision was itself given prior-decision context (by either mode) --
	// a permanent anti-self-reinforcement cap (§31.6's own G5) on how much
	// authority this candidate may ever carry, computed and stamped
	// elsewhere and only ever read here, never set or inferred by
	// anything in this package.
	KnowledgeInfluenced bool
}

// Query is what a ranker may know about the current PR -- every field is
// the server's own view of it, never anything a model posted.
type Query struct {
	// RepoFullName is "owner/repo" for the PR under review.
	RepoFullName string

	// Tags are the current PR's own changed-path-derived tags -- computed
	// the identical, deterministic way the gate's own Candidate.Tags were,
	// so the two are comparable.
	Tags []string

	// Roots are the current PR's own changed-path-derived directory
	// roots, computed the same way as Candidate.Roots.
	Roots []string

	// ChangedPaths is the current PR's own server-fetched list of changed
	// file paths.
	ChangedPaths []string

	// Title is the current PR's own title.
	Title string
}

// MaxInjected is the hard cap on how many candidates ever reach a rendered
// prompt block, applied by TakeTop AFTER ordering -- a ranker has no way
// to raise it: Score returns scores, never a count, and never a
// candidate.
const MaxInjected = 12

// CloneForRanking returns a deep copy of q and cands, safe to hand to a
// ranker whose implementation the caller does not control.
//
// The port's signature closes the obvious hole -- Score returns scores
// and no Candidate, so nothing can be added, dropped or re-selected
// through it -- but a []Candidate is a slice HEADER, and an
// implementation that receives one can rewrite the elements the caller
// still holds. Demonstrated rather than assumed: a ranker doing
// `cands[i].Decision = "..."` and `cands[i].Tags[0] = "..."` rewrites the
// caller's own candidates, and the substituted prose then reaches the
// review prompt having bypassed the sanitization applied when the
// decision was written (§31.7). Content substitution is a stricter
// failure than widening, and "scores, never candidates" does not close
// it: only a copy does.
//
// Both levels are copied, because copying the outer slice alone would
// leave Tags, Roots and ChangedPaths sharing their backing arrays and the
// hole open one level down. Every other field is a string or a scalar and
// is copied by assignment.
//
// The caller decides when this is worth its allocation: the public
// RecencyRanker is first-party and needs no copy, so this is not applied
// on the path every deployment takes.
func CloneForRanking(q Query, cands []Candidate) (Query, []Candidate) {
	q.Tags = cloneStrings(q.Tags)
	q.Roots = cloneStrings(q.Roots)
	q.ChangedPaths = cloneStrings(q.ChangedPaths)

	out := make([]Candidate, len(cands))
	copy(out, cands)
	for i := range out {
		out[i].Tags = cloneStrings(out[i].Tags)
		out[i].Roots = cloneStrings(out[i].Roots)
	}
	return q, out
}

// cloneStrings copies s, preserving the nil/empty distinction: a nil
// slice stays nil so a copy is indistinguishable from the original to a
// caller that checks for nil.
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

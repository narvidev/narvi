package automation

import (
	"net/url"
	"strings"
)

// This file is §8.4's own closing of the gap doc.go names explicitly
// under "§8.4: what this package adds" (trigger.go's own doc comment):
// MatchesGitHubTrigger/MatchesLinearTrigger existed, fully modeled and
// tested, wired into nothing. Two independent questions had to be answered
// in the open, not left to whatever an adapter happened to do, before
// wiring either matcher into a live webhook handler was safe:
//
//  1. WHICH event categories is a generic automation trigger even
//     subscribed to? GitHubDispatchAllowlist/LinearDispatchAllowlist below
//     are the explicit, closed, typed answer -- an event type outside
//     either set is classified with a named reason
//     (ClassifyGitHubDispatch/ClassifyLinearDispatch) and never reaches
//     MatchesGitHubTrigger/MatchesLinearTrigger at all. Expanding either
//     set is a deliberate, reviewed source edit, never an implicit
//     consequence of GitHub or Linear introducing a new webhook category
//     this deployment happens to start receiving.
//  2. Once a trigger's own Event/Action/Label/Name/Conclusion filter
//     matches, WHICH of an automation's own configured target repos does
//     this event actually concern? TargetMatchesGitHubEvent below answers
//     this -- and is where the branch-CONTAINS-vs-branch-TIP trap (§8.4's
//     own named pitfall) is actually closed, never in
//     MatchesGitHubTrigger (which stays blind to repo/branch scoping
//     entirely, see that function's own doc comment).

// GitHubDispatchAllowlist is the closed, typed set of "X-GitHub-Event"
// values a generic automation trigger is ever evaluated against --
// ClassifyGitHubDispatch's own backing register. Deliberately NOT "every
// event type this deployment's webhook happens to receive": a handler that
// dispatched on any event it was handed would have its own behavior
// defined by GitHub's future changes rather than by this repository
// (§8.4's own explicit framing). Chosen to cover exactly what this
// package's own condition builder can already express (Event/Action/Label
// from the original trigger.go, Name/Conclusion added alongside this
// allowlist) plus the two event types §8.4 names by name (check_run,
// status):
//
//   - pull_request/issues/issue_comment: the three event types
//     mockups.html's own worked example ("github · pull_request.labeled")
//     and GitHubTriggerConfig.Label already assume carry a label-bearing
//     payload.
//   - push: the ordinary "new commits landed on a branch" signal, and the
//     one event type besides pull_request whose own branch is completely
//     unambiguous (ref/after), needed to exercise TargetMatchesGitHubEvent's
//     own branch-scoping path without the status event's own tip-vs-
//     containment ambiguity muddying a first, simple case.
//   - check_run/status: §8.4's own named normalization targets --
//     "normalise the check_run and status GitHub events with name and
//     conclusion, plus deduplication".
var GitHubDispatchAllowlist = map[string]bool{
	"pull_request":  true,
	"issues":        true,
	"issue_comment": true,
	"push":          true,
	"check_run":     true,
	"status":        true,
}

// GitHubDispatchSkipReason names why a live GitHub webhook delivery was
// not evaluated against any automation trigger -- ClassifyGitHubDispatch's
// own return type, so a caller logs one fixed, typed vocabulary rather
// than inventing a string at the log call site.
type GitHubDispatchSkipReason string

const (
	// GitHubDispatchNotSkipped means eventType IS in GitHubDispatchAllowlist
	// -- the caller should proceed to MatchesGitHubTrigger.
	GitHubDispatchNotSkipped GitHubDispatchSkipReason = ""
	// GitHubDispatchSkipEventNotAllowlisted means eventType is not in
	// GitHubDispatchAllowlist -- ignored, never reaching any trigger
	// evaluation.
	GitHubDispatchSkipEventNotAllowlisted GitHubDispatchSkipReason = "event_type_not_allowlisted"
)

// ClassifyGitHubDispatch reports whether eventType should be evaluated
// against any TriggerTypeGitHub automation at all, and names the reason
// when it should not.
func ClassifyGitHubDispatch(eventType string) GitHubDispatchSkipReason {
	if GitHubDispatchAllowlist[eventType] {
		return GitHubDispatchNotSkipped
	}
	return GitHubDispatchSkipEventNotAllowlisted
}

// LinearDispatchAllowlist mirrors GitHubDispatchAllowlist's own reasoning
// for Linear's own "Linear-Event" category header -- "Issue"/"Comment" are
// LinearTriggerConfig.EventType's own worked examples (that struct's own
// doc comment). "AgentSessionEvent" is deliberately excluded: that
// category is already fully owned, end to end, by this codebase's existing
// Linear ingress pipeline (internal/adapters/inbound/linear/webhook.go) --
// a generic automation additionally firing off the SAME category as an
// ordinary agent-session turn would be a second, competing consumer of an
// event that already has one, not a genuinely new capability.
var LinearDispatchAllowlist = map[string]bool{
	"Issue":   true,
	"Comment": true,
}

// LinearDispatchSkipReason mirrors GitHubDispatchSkipReason's own shape,
// for Linear.
type LinearDispatchSkipReason string

const (
	// LinearDispatchNotSkipped means eventType IS in LinearDispatchAllowlist
	// -- the caller should proceed to MatchesLinearTrigger.
	LinearDispatchNotSkipped LinearDispatchSkipReason = ""
	// LinearDispatchSkipEventNotAllowlisted means eventType is not in
	// LinearDispatchAllowlist -- ignored, never reaching any trigger
	// evaluation.
	LinearDispatchSkipEventNotAllowlisted LinearDispatchSkipReason = "event_type_not_allowlisted"
)

// ClassifyLinearDispatch mirrors ClassifyGitHubDispatch, for Linear.
func ClassifyLinearDispatch(eventType string) LinearDispatchSkipReason {
	if LinearDispatchAllowlist[eventType] {
		return LinearDispatchNotSkipped
	}
	return LinearDispatchSkipEventNotAllowlisted
}

// RepoFullNameFromCloneURL extracts the "owner/repo" GitHub full name from
// an HTTPS clone URL of the shape reposource.ValidateRepoURL already
// requires every automation Target.URL to satisfy (e.g.
// "https://github.com/org/repo" or "...repo.git") -- ok is false when url
// does not parse, has no host, or its path does not carry at least two
// non-empty segments. Pure string/URL parsing only (net/url does no I/O)
// -- §11 is unaffected.
func RepoFullNameFromCloneURL(rawURL string) (fullName string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	if path == "" || !strings.Contains(path, "/") {
		return "", false
	}
	return path, true
}

// TargetMatchesGitHubEvent reports whether target is the specific
// (repo, branch) a live GitHub event in actually concerns -- the second of
// this file's own two questions (top doc comment). Repo scoping compares
// target's own clone URL (RepoFullNameFromCloneURL) against in.RepoFullName
// case-insensitively (GitHub repo paths are case-insensitive for routing).
// A target with no repo match never matches, regardless of branch.
//
// Branch scoping only applies when target.Branch is non-empty (a target
// with no configured branch matches any branch of a matching repo, per
// Target.Branch's own "use the repo's own default branch" zero-value
// convention). When it IS set, this is where §8.4's own named trap is
// closed: membership is decided by TipBranchNames(in.SHA, in.Branches) --
// branches whose own recorded HeadSHA equals in.SHA -- NEVER by scanning
// in.Branches for a matching Name alone. Scanning for a matching name
// alone is exactly the "a branch that merely CONTAINS the commit is not
// the branch whose tip it is" bug §8.4 names: GitHub's own `status`
// webhook payload lists every branch the commit is reachable FROM in
// branches[], not the branch whose HEAD it currently is, so a commit that
// landed on the repo's default branch is "contained" by every feature
// branch ever cut at or after it. A target scoped to one of those feature
// branches must not fire just because that commit happens to be an
// ancestor of it.
//
// A target with a configured Branch but an event carrying no branch-tip
// evidence at all (in.Branches empty -- e.g. issues/issue_comment, or any
// event type a caller failed to populate Branches for) never matches:
// fail closed, never assume a branch-scoped target is "close enough".
func TargetMatchesGitHubEvent(target Target, in GitHubEventInput) bool {
	repoFullName, ok := RepoFullNameFromCloneURL(target.URL)
	if !ok || !strings.EqualFold(repoFullName, in.RepoFullName) {
		return false
	}
	if target.Branch == "" {
		return true
	}
	if len(in.Branches) == 0 {
		return false
	}
	for _, name := range TipBranchNames(in.SHA, in.Branches) {
		if name == target.Branch {
			return true
		}
	}
	return false
}

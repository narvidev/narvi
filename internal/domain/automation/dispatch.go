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

// githubCloneHost is the ONLY host RepoFullNameFromCloneURL ever resolves
// an "owner/repo" full name against -- mirrors internal/app/ports.
// GitHubSourceControlHost's own identical literal, kept as this package's
// own separate copy (never imported from there) because that constant
// lives in an app-layer package (ports) and this one is domain (§11:
// domain never depends on app) -- see that constant's own doc comment for
// why "github.com" is, today, the one and only host this codebase's
// single real GitHub integration ever actually talks to. D7 audit fix
// (confirmed by a throwaway program run against this exact function,
// reported in this batch's own PR body): before this fix, the URL's host
// was parsed and then silently thrown away entirely -- so
// "https://gitlab.com/acme/repo" and "https://github.com/acme/repo"
// produced the IDENTICAL "acme/repo" full name, and a target hosted on
// GitLab, Bitbucket, or a GitHub Enterprise instance (any host at all)
// would match a github.com webhook event naming the same owner/repo path.
// A target naming a host this constant does not recognize (including a
// real GitHub Enterprise host -- not configurable anywhere in this
// codebase today, see ports.GitHubSourceControlHost's own doc comment)
// now fails closed instead.
const githubCloneHost = "github.com"

// RepoFullNameFromCloneURL extracts the "owner/repo" GitHub full name from
// an HTTPS clone URL of the shape reposource.ValidateRepoURL already
// requires every automation Target.URL to satisfy (e.g.
// "https://github.com/org/repo" or "...repo.git") -- ok is false when url
// does not parse, has no host, names a host other than githubCloneHost
// (case-insensitively -- GitHub's own DNS names are not case-sensitive),
// or its path does not carry EXACTLY two non-empty segments once a
// trailing slash and an optional ".git" suffix are stripped (in either
// order -- D7 audit fix: a bare TrimPrefix/TrimSuffix pair, tried in only
// ONE order, previously left "https://github.com/acme/repo/" as
// "acme/repo/" (trailing slash never stripped: a real, legitimately-typed
// clone URL that would then never again match any live GitHub event,
// full_name never carrying one) and "https://github.com/acme/repo.git/"
// as "acme/repo.git/" (the SAME bug, compounding with the ".git" suffix)
// -- confirmed empirically by this batch's own throwaway program, see the
// PR body's own input/output table). Pure string/URL parsing only
// (net/url does no I/O) -- §11 is unaffected.
func RepoFullNameFromCloneURL(rawURL string) (fullName string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	if !strings.EqualFold(u.Host, githubCloneHost) {
		return "", false
	}

	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	segments := strings.Split(path, "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		// Fail closed on anything that is not EXACTLY "owner/repo" --
		// including "https://github.com//repo" (an empty owner segment,
		// D7's own confirmed edge case: the OLD implementation's bare
		// strings.Contains(path, "/") check let this through as the
		// bogus full name "/repo").
		return "", false
	}
	return segments[0] + "/" + segments[1], true
}

// TargetMatchesGitHubEvent reports whether target is the specific
// (repo, branch) a live GitHub event in actually concerns -- the second of
// this file's own two questions (top doc comment). Repo scoping compares
// target's own clone URL (RepoFullNameFromCloneURL) against in.RepoFullName
// case-insensitively (GitHub repo paths are case-insensitive for routing).
// A target with no repo match never matches, regardless of branch.
//
// Branch scoping applies REGARDLESS of whether target.Branch is
// configured -- D4 audit fix (confirmed finding: "a guard that is correct
// and out of the path"). Before this fix, target.Branch == "" (Target.
// Branch's own "use the repo's own default branch" zero-value convention,
// and the DEFAULT automation configuration -- no UI/API caller is forced
// to set a branch at all) returned true immediately, BEFORE the
// tip-vs-containment guard below was ever reached: an unconfigured target
// fired for an event concerning ANY branch of a matching repo, while the
// run it created still carried Target.Branch == "" downstream (fanout.go),
// silently checking out the repo's ACTUAL default branch regardless of
// which branch the triggering event was actually about. That mismatch --
// "matched on branch X, ran against branch Y" -- is exactly what this
// fix closes: an unconfigured target now means EXACTLY "the repo's own
// default branch, whichever branch that genuinely is right now",
// resolved from THIS SAME event's own in.DefaultBranch (every GitHub
// webhook payload already carries "repository.default_branch" -- no
// extra lookup), NEVER a static guess and NEVER "whichever branch this
// happens to be about". Substituting in.DefaultBranch for an empty
// target.Branch and falling through to the IDENTICAL tip-check below
// keeps the run this creates (still Target.Branch == "", still resolving
// to the repo's default branch downstream) honest: it only ever fires for
// an event that is, in fact, about that same default branch.
//
// Once a (possibly substituted) branch name is in hand, this is where
// §8.4's own named trap is closed: membership is decided by
// TipBranchNames(in.SHA, in.Branches) -- branches whose own recorded
// HeadSHA equals in.SHA -- NEVER by scanning in.Branches for a matching
// Name alone. Scanning for a matching name alone is exactly the "a branch
// that merely CONTAINS the commit is not the branch whose tip it is" bug
// §8.4 names: GitHub's own `status` webhook payload lists every branch the
// commit is reachable FROM in branches[], not the branch whose HEAD it
// currently is, so a commit that landed on the repo's default branch is
// "contained" by every feature branch ever cut at or after it. A target
// scoped to one of those feature branches must not fire just because that
// commit happens to be an ancestor of it.
//
// A target whose own (possibly-substituted) Branch is non-empty, but an
// event carrying no branch-tip evidence at all (in.Branches empty -- e.g.
// issues/issue_comment, or any event type a caller failed to populate
// Branches for, OR an unconfigured target paired with a caller that never
// resolved in.DefaultBranch either) never matches: fail closed, never
// assume a branch-scoped target is "close enough".
//
// Exception, deliberately carved out BEFORE the DefaultBranch substitution
// above: eventTypesWithNoBranchConcept (issues, issue_comment) name every
// GitHubDispatchAllowlist event type that carries NO branch identity at
// all, by GitHub's own event shape -- not merely "a caller forgot to
// populate Branches" the way a genuine bug would look. For these event
// types ONLY, an UNCONFIGURED target keeps matching unconditionally
// (target.Branch == "" -> true, exactly like the pre-D4 behavior for
// every event type) -- there is no "which branch did this event actually
// concern" fact to compare a run's own eventual default-branch checkout
// against, so there is no mismatch for D4's fix to prevent: the run this
// creates was ALWAYS going to check out the repo's default branch
// regardless, since an issue/issue-comment event is never "about" any
// branch in the first place. A CONFIGURED target on one of these event
// types is UNCHANGED by this exception -- it still falls through to the
// tip-check below, which still fails closed (in.Branches is always empty
// for these two event types), exactly as it always has.
func TargetMatchesGitHubEvent(target Target, in GitHubEventInput) bool {
	repoFullName, ok := RepoFullNameFromCloneURL(target.URL)
	if !ok || !strings.EqualFold(repoFullName, in.RepoFullName) {
		return false
	}

	if target.Branch == "" && eventTypesWithNoBranchConcept[in.EventType] {
		return true
	}

	branch := target.Branch
	if branch == "" {
		branch = in.DefaultBranch
		if branch == "" {
			return false
		}
	}
	if len(in.Branches) == 0 {
		return false
	}
	for _, name := range TipBranchNames(in.SHA, in.Branches) {
		if name == branch {
			return true
		}
	}
	return false
}

// eventTypesWithNoBranchConcept is TargetMatchesGitHubEvent's own closed,
// typed carve-out (immediately above) -- every GitHubDispatchAllowlist
// event type that structurally carries no branch identity whatsoever.
// Deliberately the NARROWER of the two possible defaults (fail closed --
// requiring in.DefaultBranch evidence -- for anything NOT explicitly
// listed here, including any event type added to GitHubDispatchAllowlist
// later without ALSO being added here): mirrors this package's own
// repeated "closed, typed register, explicit membership, never an
// implicit default" convention (GitHubDispatchAllowlist/
// LinearDispatchAllowlist themselves).
var eventTypesWithNoBranchConcept = map[string]bool{
	"issues":        true,
	"issue_comment": true,
}

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

// GitHubEventOrigin classifies, for ONE GitHubDispatchAllowlist event
// type, WHO GitHub reports as having caused it -- D12 audit fix (a
// confirmed, HIGH-severity finding: "the authorization gate kills two of
// the six allowlisted event types"). Round 1's own actor-authorization
// fix (D2, resolving and authorizing the event's own top-level
// "sender.id") is right for a HUMAN-originated event -- a real GitHub
// account can complete this deployment's own GitHub OAuth login, and its
// resolved identity IS the human decision being honoured. It is
// STRUCTURALLY IMPOSSIBLE to satisfy for a MACHINE-originated one:
// check_run/status are ALWAYS posted by a CI integration, a bot, or
// GitHub's own "ghost" placeholder, NEVER a human account -- so gating
// either behind "does sender.id resolve to a linked Narvi user" rejects
// every single delivery of either event type, forever, regardless of how
// this deployment's automations are configured. See
// ClassifyGitHubEventOrigin's own doc comment for how each origin is
// actually authorized, and internal/adapters/inbound/github's own
// dispatchAutomationsBestEffort / internal/app/automation's own
// dispatchOneGitHubAutomation for where each check actually runs.
type GitHubEventOrigin int

const (
	// GitHubEventOriginHuman means a real GitHub account -- capable of
	// completing this deployment's own GitHub OAuth login -- performed
	// this event. Authorized at the EVENT level (once per delivery,
	// before any automation is even listed): the event's own top-level
	// "sender.id" is resolved and must be a linked, non-disabled account
	// holding authz.ActionCreateSession (internal/adapters/inbound/
	// github's own dispatchAutomationsBestEffort, unchanged by D12 -- see
	// that function's own doc comment).
	GitHubEventOriginHuman GitHubEventOrigin = iota
	// GitHubEventOriginMachine means GitHub itself (never a human
	// account) is always the actor for every real delivery of this event
	// type. There is no sender identity to authorize, so the
	// authorizing PRINCIPAL is instead the automation's OWN
	// configuration: a maintainer with authz.ActionCreateSession created
	// this automation and deliberately chose a check_run/status trigger
	// -- that is the human decision being honoured. Authorized PER
	// AUTOMATION (never once per delivery, since it depends on which
	// automation's own row is being evaluated): automations.created_by
	// must name a linked, non-disabled account still holding
	// authz.ActionCreateSession (internal/app/automation's own
	// dispatchOneGitHubAutomation, githubdispatch.go). automations.
	// created_by is nullable (ON DELETE SET NULL, migrations/
	// 000051_automations.up.sql's own doc comment: "an automation, like a
	// session, can outlive the user who created it") -- an automation
	// whose creator has since been deleted has no authorizing principal
	// left at all, and fails closed exactly like an unlinked GitHub
	// sender does on the human path.
	GitHubEventOriginMachine
)

// githubEventOrigins is the closed, typed, EXHAUSTIVE decision this
// package makes for every GitHubDispatchAllowlist event type -- see
// GitHubEventOrigin's own doc comment for the full "why" this type exists
// at all. GitHubDispatchAllowlist itself (below) is DERIVED from this
// map's own key set, rather than maintained as a second, independent
// list: adding an event type to the allowlist and deciding which
// authorization bucket it falls into are therefore the SAME source edit,
// not two that could drift apart and silently leave a newly-allowlisted
// event type with no origin decision at all (TestGitHubEventOriginCoversAllowlist,
// dispatch_test.go, pins this derivation).
var githubEventOrigins = map[string]GitHubEventOrigin{
	// pull_request/issues/issue_comment/push: mockups.html's own worked
	// example ("github · pull_request.labeled"), GitHubTriggerConfig.Label,
	// and the ordinary "new commits landed on a branch" signal are all
	// human actions -- a maintainer/contributor opening a PR, filing an
	// issue, commenting, or pushing commits. A bot-driven push (e.g. a CI
	// job or a dependency-update integration pushing via its own token)
	// is possible but NOT the structural default the way check_run/status
	// are -- an ordinary push is, overwhelmingly, a real person's `git
	// push` -- so this stays on the human-authorization path: a bot
	// sender simply has no linked Narvi identity and is denied exactly
	// like any other unlinked sender, which is the correct, narrower
	// default (fail closed on an unrecognized actor) rather than a
	// heuristic guess at "is this sender secretly a bot" from its own
	// login string.
	"pull_request":  GitHubEventOriginHuman,
	"issues":        GitHubEventOriginHuman,
	"issue_comment": GitHubEventOriginHuman,
	"push":          GitHubEventOriginHuman,
	// check_run/status: §8.4's own named normalization targets --
	// "normalise the check_run and status GitHub events with name and
	// conclusion, plus deduplication". GitHub itself always posts these
	// (a CI check completing, a commit status changing) -- never a human
	// account directly, regardless of deployment or configuration.
	"check_run": GitHubEventOriginMachine,
	"status":    GitHubEventOriginMachine,
}

// ClassifyGitHubEventOrigin reports eventType's own GitHubEventOrigin --
// ok is false for anything outside GitHubDispatchAllowlist (mirrors
// ClassifyGitHubDispatch's own "outside the register" contract; a caller
// that already checked ClassifyGitHubDispatch first can treat ok==false
// here as unreachable in practice).
func ClassifyGitHubEventOrigin(eventType string) (origin GitHubEventOrigin, ok bool) {
	origin, ok = githubEventOrigins[eventType]
	return origin, ok
}

// GitHubDispatchAllowlist is the closed, typed set of "X-GitHub-Event"
// values a generic automation trigger is ever evaluated against --
// ClassifyGitHubDispatch's own backing register, DERIVED from
// githubEventOrigins' own key set (immediately above) rather than
// maintained as a second, independently-edited map -- see that map's own
// doc comment for why. Deliberately NOT "every event type this
// deployment's webhook happens to receive": a handler that dispatched on
// any event it was handed would have its own behavior defined by GitHub's
// future changes rather than by this repository (§8.4's own explicit
// framing).
var GitHubDispatchAllowlist = deriveGitHubDispatchAllowlist()

func deriveGitHubDispatchAllowlist() map[string]bool {
	m := make(map[string]bool, len(githubEventOrigins))
	for eventType := range githubEventOrigins {
		m[eventType] = true
	}
	return m
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

// LinearEventOrigin classifies, for one Linear "Issue"/"Comment" webhook
// delivery, WHO Linear reports as having caused it -- U7 audit fix
// (confirmed finding: "Linear has no machine-origin equivalent" -- the
// exact functional hole D12 closed on the GitHub side, GitHubEventOrigin's
// own doc comment above). Linear's own webhook docs describe the top-level
// "actor" object's own "type" field as "user" for a real Linear account,
// and something ELSE (an OAuth client or an Integration, per Linear's own
// docs' wording -- "The actor who triggered the action. Could be a User,
// OAuth client, or Integration") for a non-human actor. Unlike GitHub,
// this is NOT determined by the event's own CATEGORY -- every Issue/
// Comment delivery uses the identical wire shape regardless of who
// triggered it -- so classification happens per-DELIVERY, from the
// actor's own reported type, never from a closed, typed event-category
// register the way GitHubEventOrigin is.
type LinearEventOrigin int

const (
	// LinearEventOriginHuman means a real Linear account (actor.type ==
	// "user") -- authorized at the EVENT level (once per delivery, before
	// any automation is even listed), mirroring GitHubEventOriginHuman's
	// own identical "resolve once, applies to every automation this
	// delivery evaluates" reasoning: internal/adapters/inbound/linear's
	// own dispatchAutomationsBestEffort.
	LinearEventOriginHuman LinearEventOrigin = iota
	// LinearEventOriginMachine means the reported actor is NOT a real
	// Linear account (any non-empty, non-"user" actor.type) -- there is no
	// human identity to authorize, so the authorizing PRINCIPAL is instead
	// the automation's OWN configuration, mirroring
	// GitHubEventOriginMachine's own identical reasoning exactly:
	// automations.created_by must name a linked, non-disabled account
	// still holding authz.ActionCreateSession (internal/app/automation's
	// own dispatchOneLinearAutomation, lineardispatch.go).
	LinearEventOriginMachine
)

// linearHumanActorType is the ONE actor.type value Linear's own docs and
// live payloads confirm names a real Linear account ("user"). Every OTHER
// non-empty type is classified machine-origin below -- deliberately the
// NARROWER of the two possible defaults for a non-"user", non-empty
// string: Linear's own docs name "OAuth client" and "Integration" as the
// two non-human actor kinds but do not give this package a confirmed,
// exhaustive register of their own wire "type" values (unlike
// GitHubDispatchAllowlist/eventTypesWithNoBranchConcept's own closed,
// verified registers elsewhere in this file) -- a closed allowlist for the
// one CONFIRMED human value, rather than an open list of every possible
// non-human one, so an actor type this package has never specifically seen
// is classified machine-origin (gated behind the automation's own
// creator) rather than silently falling through the human-path lookup
// with a type string that was never actually a Narvi-linkable user id in
// the first place.
const linearHumanActorType = "user"

// ClassifyLinearActorOrigin reports actorType's own LinearEventOrigin -- ok
// is false for an EMPTY actorType (Linear's own "actor may be null if the
// user or integration that triggered the action has since been deleted"
// case): genuinely unknown whether the deleted actor was ever human, so
// this stays on the existing, UNCHANGED human-path denial (ok == false,
// origin defaults to Human but the caller must check ok before acting on
// it, mirroring ClassifyGitHubEventOrigin's own identical ok-gated
// contract) rather than being folded into either bucket -- deliberately
// out of THIS fix's own scope (see U7's own doc comment,
// internal/adapters/inbound/linear/automationdispatch.go, for the "why").
func ClassifyLinearActorOrigin(actorType string) (origin LinearEventOrigin, ok bool) {
	if actorType == "" {
		return LinearEventOriginHuman, false
	}
	if actorType == linearHumanActorType {
		return LinearEventOriginHuman, true
	}
	return LinearEventOriginMachine, true
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
// types, EVERY target matches on repo scoping alone, regardless of
// whether Target.Branch is configured -- D14 audit fix (confirmed
// finding: this carve-out used to key on "target.Branch == "" &&
// eventTypesWithNoBranchConcept[...]", so an EXPLICITLY-configured branch
// on one of these two event types fell through to the tip-check below
// instead, which always fails closed for them, since in.Branches is
// always empty here (issues/issue_comment carry no branch-tip evidence to
// populate it with in the first place) -- silently making a configured
// branch on an issues/issue_comment automation impossible to ever
// satisfy, contradicting D4's own stated equivalence one section up that
// an unconfigured target means "exactly the repo's own default branch".
// The decision this fix makes: Target.Branch, for an event type with no
// branch concept at all, names which branch a MATCHING run should check
// out downstream (fanout.go) -- never a filter on whether this event
// concerns that branch, because no such fact exists for these two event
// types to compare it against (an issue/comment is never "about" a
// branch). So the same repo-scoped target matches an issues/issue_comment
// event whether Target.Branch is "" (checkout the repo's own default
// branch) or "release/1.0" (checkout that branch instead) -- both are
// ordinary, valid configurations of WHICH branch to run against, neither
// is a claim about which branch this event concerns.
func TargetMatchesGitHubEvent(target Target, in GitHubEventInput) bool {
	repoFullName, ok := RepoFullNameFromCloneURL(target.URL)
	if !ok || !strings.EqualFold(repoFullName, in.RepoFullName) {
		return false
	}

	if eventTypesWithNoBranchConcept[in.EventType] {
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

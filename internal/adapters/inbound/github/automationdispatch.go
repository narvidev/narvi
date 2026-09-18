package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/app/automation"
	"github.com/narvidev/narvi/internal/domain/authz"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// githubAutomationEventEnvelope is this file's own minimal, generic
// top-level shape covering every event type in domainautomation.
// GitHubDispatchAllowlist -- deliberately NOT the mention struct (payload.go),
// which only ever exists for the two comment-bearing event types and
// throws away everything a generic automation trigger needs (repository,
// labels for "issues" as well as "pull_request", check_run/status-specific
// fields). Every field GitHub does not send for a given event type simply
// decodes to its own zero value -- encoding/json never errors on an
// absent key.
type githubAutomationEventEnvelope struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`

		// DefaultBranch is "repository.default_branch" -- D4 audit fix's
		// own resolution for what an UNCONFIGURED (Target.Branch == "")
		// automation target means (domainautomation.
		// TargetMatchesGitHubEvent's own doc comment): the repo's own
		// CURRENT default branch, exactly as GitHub itself reports it on
		// every event's own embedded repository object -- never a static
		// "main" guess. Present on every event type this file builds an
		// input for (GitHub's own full repository object always carries
		// it), so populating this costs no extra lookup.
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`

	// Sender is GitHub's own "who performed this action" actor, present on
	// EVERY event type in domainautomation.GitHubDispatchAllowlist -- D2
	// audit fix's own actor-authorization input (dispatchAutomationsBestEffort
	// below): the same (provider, external_id) identity resolveCommenterActor
	// (identity.go) already resolves the @mention pipeline's own commenter
	// against, reused here rather than a second lookup.
	Sender struct {
		ID int64 `json:"id"`
	} `json:"sender"`

	// Label is the top-level "label" object GitHub's own `pull_request`/
	// `issues` "labeled"/"unlabeled" actions carry -- the ONE label that
	// was just added/removed (parsePullRequestLabeled, payload.go, already
	// reads this SAME field for the label-retrigger lane). Merged into
	// Labels below alongside PullRequest.Labels/Issue.Labels (the full
	// CURRENT label set GitHub's real payload also includes) so a trigger
	// filtering on the label that was JUST applied matches even against a
	// minimal payload that carries only this top-level field.
	Label *struct {
		Name string `json:"name"`
	} `json:"label"`

	PullRequest *struct {
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		// Base.Ref is the PR's own TARGET/integration branch ("what
		// branch is this PR being merged INTO") -- D3 audit fix's own
		// decision for what a branch-scoped target means for a
		// `pull_request` event, see buildGitHubEventInput's own doc
		// comment below for the full "why".
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"pull_request"`

	Issue *struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue"`

	// Ref/After (the `push` event's own top-level fields): Ref is
	// "refs/heads/<branch>" for an ordinary branch push (a tag push's own
	// "refs/tags/<tag>" is deliberately not treated as a branch below);
	// After is the new tip commit SHA.
	Ref   string `json:"ref"`
	After string `json:"after"`

	CheckRun *struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"head_sha"`
		CheckSuite struct {
			HeadBranch string `json:"head_branch"`
		} `json:"check_suite"`
	} `json:"check_run"`

	// SHA/State/Context (the `status` event's own top-level fields, NOT
	// nested under any child object -- GitHub's own real payload shape).
	SHA      string `json:"sha"`
	State    string `json:"state"`
	Context  string `json:"context"`
	Branches []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	} `json:"branches"`
}

const pushRefBranchPrefix = "refs/heads/"

// githubZeroSHA is GitHub's own well-known "this ref no longer exists"
// sentinel: a `push` event's own "after" field is exactly 40 zero
// characters when that push DELETED the branch named by "ref", never a
// real commit SHA -- D5 audit fix (confirmed finding: a branch deletion
// was previously classified as that branch's own new tip, since the old
// check was a bare `env.After != ""`, and this sentinel string is very
// much non-empty).
const githubZeroSHA = "0000000000000000000000000000000000000000"

// buildGitHubEventInput normalizes body (the raw webhook payload for
// eventType) into domainautomation.GitHubEventInput -- ok is false only on
// a JSON decode failure (a malformed body, which parseMention's own
// sibling call already handles/logs for the mention pipeline; this
// function's own caller treats false as "nothing to dispatch", never as a
// reason to fail the whole request, since automation dispatch must never
// suppress the mention pipeline's own, independent handling of the same
// malformed body).
//
// The top-level "label" object (GitHub's own `pull_request`/`issues`
// "labeled"/"unlabeled" actions carry exactly the one label that just
// changed there -- parsePullRequestLabeled, payload.go, already reads this
// SAME field for the label-retrigger lane) is always merged into Labels
// first, regardless of event type, ahead of whichever per-event-type
// labels[] array below also contributes to it.
//
// RepoFullName/DefaultBranch (env.Repository) are populated identically
// for every event type below -- GitHub's own full repository object,
// carrying both, is embedded on every event type in
// domainautomation.GitHubDispatchAllowlist.
//
// Per event type:
//   - pull_request (D3 audit fix -- "what does a branch-scoped target
//     mean for a pull_request event"): Labels also gets
//     pull_request.labels[] (the full CURRENT label set). DECISION: a
//     branch-scoped target means the PR's own BASE branch (what it is
//     being merged INTO), never its head branch -- mirroring GitHub
//     Actions' own `on.pull_request.branches` filter, which matches the
//     base branch by the identical convention. This is not an arbitrary
//     pick: the confirmed audit finding was TWO defects in the OLD
//     head-branch-only behavior at once -- (1) a fork PR's own head
//     branch is chosen freely by its author and belongs to a DIFFERENT
//     repository entirely, so a fork opened with head.ref == "main"
//     satisfied a target scoped to "main" on the BASE repo by naming
//     coincidence alone (a false positive, attacker-controlled); (2) an
//     ordinary, same-repo PR opened INTO main never fires a target
//     scoped to "main" at all, since a PR's own head is by definition
//     never its own base (a false negative). Base-branch scoping closes
//     BOTH: base.ref is always a real branch of the base repo itself,
//     never attacker-chosen, and "this PR targets main" is exactly what
//     a "main"-scoped target should mean. Branches therefore carries
//     base.ref FIRST (when present), paired with head.sha (the commit
//     this event actually pertains to) as its own HeadSHA -- this
//     pairing is a DELIBERATE simplification, not a claim that head.sha
//     is literally the base branch's real, independent current-tip
//     commit in git terms (a `pull_request` payload carries no such
//     fact): it exists purely so TargetMatchesGitHubEvent's existing,
//     shared TipBranchNames(in.SHA, in.Branches) machinery -- built for
//     status/check_run's OWN genuine tip-vs-containment ambiguity --
//     also serves this event type's unambiguous "which base branch does
//     this ONE event concern" question, without forking that function's
//     own logic per event type. head.ref is ALSO added, as a SECOND
//     Branches entry, but ONLY when it is genuinely a branch of the SAME
//     repository as env.Repository (sameRepo below) -- e.g. an
//     automation deliberately scoped to a long-lived, same-repo branch
//     that itself opens PRs. A head whose own repo is explicitly a
//     DIFFERENT one (a real fork) is REJECTED outright: never added,
//     regardless of what name it carries -- this is the literal "reject
//     head refs that belong to a different repository than the event's
//     own" half of the fix.
//   - issues/issue_comment: Labels also gets issue.labels[] -- D16 audit
//     fix (confirmed finding: "an issue_comment Label filter can never
//     match"): GitHub embeds the FULL parent issue resource, labels
//     included, on an issue_comment delivery exactly like it does on
//     issues itself, contrary to this comment's own previous claim that
//     it does not. Neither event type has a branch concept at all.
//   - push: Branches carries ONE entry, ref (stripped of "refs/heads/")
//     and after -- the new tip BY DEFINITION (a push necessarily moves
//     that branch's tip to after). Skipped (nil Branches, empty SHA) for
//     a tag push (Ref does not start with "refs/heads/") OR (D5 audit
//     fix) a branch DELETION (after == githubZeroSHA) -- a deleted
//     branch has no "new tip" at all, and the old bare `after != ""`
//     check let the zero-SHA sentinel itself be stored as both the event
//     SHA and the branch's own HeadSHA, firing branch-scoped automations
//     for a branch that no longer exists.
//   - check_run: SHA/Branches come from check_run.head_sha/check_suite.
//     head_branch -- GitHub itself names the one branch this run concerns,
//     no containment ambiguity.
//   - status: SHA from the top-level "sha" field, Branches from the
//     top-level "branches[]" array VERBATIM (containment semantics
//     preserved, never resolved here) -- see domainautomation.
//     GitHubEventBranch's own doc comment: this is the one event type
//     where "contains" and "tip" genuinely differ, which is why
//     TipBranchNames/TargetMatchesGitHubEvent (domain layer), not this
//     parsing step, are where that distinction is actually enforced.
func buildGitHubEventInput(eventType string, body []byte) (domainautomation.GitHubEventInput, bool) {
	var env githubAutomationEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return domainautomation.GitHubEventInput{}, false
	}

	in := domainautomation.GitHubEventInput{
		EventType:     eventType,
		Action:        env.Action,
		RepoFullName:  env.Repository.FullName,
		DefaultBranch: env.Repository.DefaultBranch,
	}

	if env.Label != nil && env.Label.Name != "" {
		in.Labels = append(in.Labels, env.Label.Name)
	}

	switch eventType {
	case eventTypePullRequest:
		if env.PullRequest != nil {
			for _, l := range env.PullRequest.Labels {
				in.Labels = append(in.Labels, l.Name)
			}
			if env.PullRequest.Head.SHA != "" {
				in.SHA = env.PullRequest.Head.SHA
				if env.PullRequest.Base.Ref != "" {
					in.Branches = append(in.Branches, domainautomation.GitHubEventBranch{Name: env.PullRequest.Base.Ref, HeadSHA: env.PullRequest.Head.SHA})
				}
				if env.PullRequest.Head.Ref != "" && sameRepo(env.PullRequest.Head.Repo.FullName, env.Repository.FullName) {
					in.Branches = append(in.Branches, domainautomation.GitHubEventBranch{Name: env.PullRequest.Head.Ref, HeadSHA: env.PullRequest.Head.SHA})
				}
			}
		}

	case "issues", "issue_comment":
		// D16 audit fix (confirmed finding: "an issue_comment Label filter
		// can never match"): GitHub embeds the FULL parent issue resource
		// on an issue_comment delivery too, labels included -- not just on
		// "issues" itself -- so this reads env.Issue.Labels for both event
		// types identically, never only the one this switch used to name.
		if env.Issue != nil {
			for _, l := range env.Issue.Labels {
				in.Labels = append(in.Labels, l.Name)
			}
		}

	case "push":
		if strings.HasPrefix(env.Ref, pushRefBranchPrefix) && env.After != "" && env.After != githubZeroSHA {
			branch := strings.TrimPrefix(env.Ref, pushRefBranchPrefix)
			in.SHA = env.After
			in.Branches = []domainautomation.GitHubEventBranch{{Name: branch, HeadSHA: env.After}}
		}

	case "check_run":
		if env.CheckRun != nil {
			in.Name = env.CheckRun.Name
			in.Conclusion = env.CheckRun.Conclusion
			in.SHA = env.CheckRun.HeadSHA
			if env.CheckRun.CheckSuite.HeadBranch != "" {
				in.Branches = []domainautomation.GitHubEventBranch{{Name: env.CheckRun.CheckSuite.HeadBranch, HeadSHA: env.CheckRun.HeadSHA}}
			}
		}

	case "status":
		in.Name = env.Context
		in.Conclusion = env.State
		in.SHA = env.SHA
		for _, b := range env.Branches {
			in.Branches = append(in.Branches, domainautomation.GitHubEventBranch{Name: b.Name, HeadSHA: b.Commit.SHA})
		}
	}

	return in, true
}

// sameRepo reports whether headRepoFullName (a pull_request event's own
// head.repo.full_name) may be trusted as naming a branch of
// baseRepoFullName (the event's own top-level "repository") -- true ONLY
// on a non-empty, case-insensitive match (GitHub repo paths route
// case-insensitively, mirroring TargetMatchesGitHubEvent's own identical
// reasoning, dispatch.go).
//
// D13 audit fix, SECURITY (confirmed finding: "the fork-head rejection
// fails open"): this function used to also return true for an EMPTY
// headRepoFullName, reasoned (wrongly) as "GitHub's own real payload
// never omits this field, so this fallback is never exercised against a
// genuine webhook delivery". That reasoning does not survive contact with
// GitHub's own documented behavior: GitHub sends "head.repo": null --
// decoding to this exact zero value -- whenever the fork that opened this
// PR has SINCE BEEN DELETED, and deleting your own fork is something the
// PR's own author (an attacker, on a public repository) controls
// unilaterally, at will, including in the same window as the webhook
// delivery this function gates. So the previous fallback was reachable by
// EXACTLY the input an attacker can produce on demand: open a fork PR
// naming any head branch they like, delete the fork, and this function's
// old "empty means same repo, trust it" branch let that attacker-chosen
// head.ref through as if it were a branch of the base repo -- the precise
// false positive TestBuildGitHubEventInput_PullRequest_BaseAndHeadBranches'
// own "fork PR" case already proves this function must reject.
//
// Unknown provenance is not "same repo" -- fail closed, exactly like
// every other guard in this file. The one legitimate case this used to
// carve out (a long-lived, same-repo branch that itself opens PRs) is
// unaffected: GitHub embeds a real, non-empty repository object on
// head.repo for as long as the repo backing it still exists, fork or
// not, so a genuine same-repo PR's own head.repo.full_name is never
// empty in practice.
func sameRepo(headRepoFullName, baseRepoFullName string) bool {
	return headRepoFullName != "" && strings.EqualFold(headRepoFullName, baseRepoFullName)
}

// githubEventSenderID extracts the top-level "sender.id" GitHub attaches
// to EVERY webhook event (the actor who performed the action this
// delivery reports) -- a second, minimal, standalone decode rather than
// widening buildGitHubEventInput's own return shape, so that function's
// existing unit tests (this package's own automationdispatch_test.go, all
// written against its current (GitHubEventInput, bool) signature) stay
// untouched by an addition that is, structurally, an unrelated concern
// (WHO sent this event, never WHAT it says). ok is false only on a JSON
// decode failure -- the identical "nothing to do" contract
// buildGitHubEventInput's own ok already establishes.
func githubEventSenderID(body []byte) (id int64, ok bool) {
	var env struct {
		Sender struct {
			ID int64 `json:"id"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, false
	}
	return env.Sender.ID, true
}

// dispatchAutomationsBestEffort is §8.4's own live-dispatch call site:
// an ADDITIONAL, independent consumer of this SAME already-claimed
// delivery -- called from NewHandler's own returned func immediately after
// the delivery-dedup claim succeeds, BEFORE any of the @mention/merge-gate/
// capture-command lanes below it, and unconditionally (regardless of which,
// if any, of those lanes goes on to also handle this exact event).
//
// Nil-safe (cfg.Automations == nil or cfg.AutomationInvocations == nil,
// this package's own handler_test.go, or any other minimal wiring that
// doesn't care about this Step): simply returns, mirroring every other
// optional Config field's identical nil-safety convention in this file.
// identities/users are ALSO required (nil fails closed exactly like the
// two Config fields, logged once) -- see the D2 section below.
//
// Recovers from a panic internally and only logs it -- automation dispatch
// evaluating a handful of trigger configs against a webhook payload is
// genuinely independent business logic from the @mention pipeline sharing
// this same delivery; a defect in one must never take down the other. This
// is the ONE place in this package that recovers from a panic at all,
// deliberately narrow in scope (see this repository's own §11 "no
// swallowed panics" convention elsewhere -- this is a documented, reviewed
// exception for exactly this "additional, independent consumer" shape, not
// a general panic-recovery precedent for the rest of this handler).
//
// # D2 audit fix: actor authorization, fail closed
//
// Before this fix, ANY GitHub account that could open an issue, post a
// comment, or open a fork PR on a watched public repository could create
// an automation invocation -- and, downstream, sandboxed agent runs
// holding this deployment's own repository credentials -- with NO
// authorization check at all, even though the pre-existing @mention
// pipeline (coalesce.go) already refuses the identical unlinked sender
// via actorauthz.AuthorizeLinkedActor. This closes that gap by calling
// the SAME primitive, never a second one: the event's own top-level
// "sender.id" (githubEventSenderID above) is resolved to a Narvi actor via
// resolveCommenterActor (identity.go -- the EXACT same direct (provider,
// external_id) lookup the @mention pipeline's own commenter resolution
// already uses, no separate/duplicated lookup), then authorized via
// actorauthz.AuthorizeLinkedActor(..., authz.ActionCreateSession, ...) --
// ActionCreateSession because an automation invocation is, structurally,
// "start new agent work" (§13.3 row 2), the SAME row CreateOrJoin's own
// WINNER path already authorizes a fresh @mention-triggered session
// against, and that row has no per-resource ownership concept to carve
// out (a brand-new invocation has no pre-existing session to own or not).
// An unauthorized or unlinked sender (actor invalid, OR AuthorizeLinkedActor
// denies a linked-but-insufficient-role actor) is skipped with a named,
// logged reason -- exactly like an out-of-allowlist event
// (ClassifyGitHubDispatch's own identical "named reason, no dispatch"
// shape, app/automation's own githubdispatch.go) -- never silently or
// implicitly. A genuine LOOKUP failure (resolveCommenterActor's own
// distinct non-nil-error return -- a transient Postgres error, saying
// NOTHING about link state) is treated the same way: fail closed, skip
// dispatch, log at Error.
//
// # D12 audit fix: this gate is HUMAN-origin only -- machine-originated
// events are authorized per-automation, downstream
//
// The design above assumes the event's own top-level "sender.id" names a
// real GitHub account capable of completing this deployment's own GitHub
// OAuth login -- true for pull_request/issues/issue_comment/push, but
// STRUCTURALLY IMPOSSIBLE for check_run/status: GitHub itself always
// posts those (a CI integration, a bot, or GitHub's own "ghost"
// placeholder), never a human account, on every real delivery of either
// type, regardless of deployment or configuration. Gating them behind
// "does sender.id resolve to a linked Narvi user" therefore rejected
// EVERY delivery of check_run/status, forever -- the two event types this
// package's own condition builder added Name/Conclusion for BY NAME
// (§8.4) could never actually dispatch in production. A confirmed,
// HIGH-severity finding, closed by classifying human-origin from
// machine-origin EXPLICITLY (domainautomation.ClassifyGitHubEventOrigin
// -- a closed, typed register, never a heuristic on the sender's own
// login/type string) and authorizing each on its OWN terms:
//
//   - GitHubEventOriginHuman (unchanged by this fix): the sender-based
//     gate immediately below, run ONCE per delivery -- the resolved
//     sender is the SAME actor regardless of which automation's own
//     trigger goes on to match, so there is nothing to gain by deferring
//     this check per-automation-row.
//   - GitHubEventOriginMachine: there is no sender identity to authorize
//     at all, so this function does none of the above for it -- no
//     sender resolution, no lookup, nothing. Authorization instead
//     happens PER AUTOMATION, inside app/automation's own
//     dispatchOneGitHubAutomation (githubdispatch.go), against THAT
//     automation's own automations.created_by: the maintainer who
//     created this automation and deliberately chose a check_run/status
//     trigger is the human decision being honoured, standing in for a
//     sender GitHub itself never sends as a completable Narvi identity.
//
// This split is deliberately NOT symmetric (human origin is gated here,
// once; machine origin is gated one layer down, per row) -- see
// GitHubEventOrigin's own doc comment (dispatch.go) for why forcing both
// onto the identical call site would blur, rather than clarify, which
// principal each origin is actually authorizing. Do NOT weaken the human
// path to make the two symmetric: an arbitrary internet actor must still
// never cause an agent run on pull_request/issues/issue_comment/push.
func dispatchAutomationsBestEffort(ctx context.Context, logger *slog.Logger, cfg Config, identities CommenterIdentityLookup, users *postgres.UserStore, eventType string, deliveryID string, body []byte) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("github: automation dispatch panicked, isolated from the rest of this delivery", "panic", r, "event_type", eventType)
		}
	}()

	if cfg.Automations == nil || cfg.AutomationInvocations == nil {
		return
	}
	if identities == nil || users == nil {
		logger.Error("github: automation dispatch: identities/users store not wired, skipping (fail closed)", "event_type", eventType)
		return
	}

	in, ok := buildGitHubEventInput(eventType, body)
	if !ok {
		logger.Warn("github: automation dispatch: malformed webhook body, skipping (mention pipeline handles/logs this independently)", "event_type", eventType)
		return
	}

	// D12 audit fix: a machine-originated event type (check_run, status)
	// has no sender identity to authorize at all -- GitHub itself is
	// always the actor. Skip straight to dispatch; per-automation
	// authorization against automations.created_by happens downstream,
	// in app/automation's own dispatchOneGitHubAutomation. Anything NOT
	// explicitly classified GitHubEventOriginMachine (including an event
	// type outside GitHubDispatchAllowlist entirely, which dispatch below
	// simply no-ops for) falls through to the human-origin sender check
	// unchanged -- the safe, narrower default.
	if origin, known := domainautomation.ClassifyGitHubEventOrigin(eventType); known && origin == domainautomation.GitHubEventOriginMachine {
		automation.DispatchGitHubWebhookEvent(ctx, logger, cfg.Automations, cfg.AutomationInvocations, users, cfg.Timeouts, eventType, deliveryID, in)
		return
	}

	senderID, ok := githubEventSenderID(body)
	if !ok {
		logger.Warn("github: automation dispatch: malformed webhook body (sender), skipping", "event_type", eventType)
		return
	}

	actor, err := resolveCommenterActor(ctx, identities, senderID)
	if err != nil {
		logger.Error("github: automation dispatch: resolve sender identity failed, skipping (fail closed)", "error", err, "event_type", eventType)
		return
	}
	if !actorauthz.AuthorizeLinkedActor(ctx, logger, authzSurface, users, actor, authz.ActionCreateSession, authz.Resource{}) {
		logger.Info("github: automation dispatch: sender not authorized, skipping", "event_type", eventType, "reason", "sender_unlinked_or_unauthorized", "sender_id", senderID)
		return
	}

	automation.DispatchGitHubWebhookEvent(ctx, logger, cfg.Automations, cfg.AutomationInvocations, users, cfg.Timeouts, eventType, deliveryID, in)
}

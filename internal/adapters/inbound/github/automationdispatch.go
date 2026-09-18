package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/narvidev/narvi/internal/app/automation"
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
	} `json:"repository"`

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
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
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
// Per event type:
//   - pull_request: Labels also gets pull_request.labels[] (the full
//     CURRENT label set); Branches carries ONE entry, the PR's own head
//     ref/sha -- unambiguous, no tip-vs-containment question for this
//     event type.
//   - issues: Labels also gets issue.labels[]; no branch concept at all.
//   - issue_comment: no labels beyond the top-level merge above (this
//     event's own payload does not embed the parent issue's labels), no
//     branch concept.
//   - push: Branches carries ONE entry, ref (stripped of "refs/heads/")
//     and after -- the new tip BY DEFINITION (a push necessarily moves
//     that branch's tip to after). Skipped (nil Branches, empty SHA) for
//     a tag push (Ref does not start with "refs/heads/").
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
		EventType:    eventType,
		Action:       env.Action,
		RepoFullName: env.Repository.FullName,
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
				in.Branches = []domainautomation.GitHubEventBranch{{Name: env.PullRequest.Head.Ref, HeadSHA: env.PullRequest.Head.SHA}}
			}
		}

	case "issues":
		if env.Issue != nil {
			for _, l := range env.Issue.Labels {
				in.Labels = append(in.Labels, l.Name)
			}
		}

	case "push":
		if strings.HasPrefix(env.Ref, pushRefBranchPrefix) && env.After != "" {
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
func dispatchAutomationsBestEffort(ctx context.Context, logger *slog.Logger, cfg Config, eventType string, body []byte) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("github: automation dispatch panicked, isolated from the rest of this delivery", "panic", r, "event_type", eventType)
		}
	}()

	if cfg.Automations == nil || cfg.AutomationInvocations == nil {
		return
	}

	in, ok := buildGitHubEventInput(eventType, body)
	if !ok {
		logger.Warn("github: automation dispatch: malformed webhook body, skipping (mention pipeline handles/logs this independently)", "event_type", eventType)
		return
	}

	automation.DispatchGitHubWebhookEvent(ctx, logger, cfg.Automations, cfg.AutomationInvocations, eventType, in)
}

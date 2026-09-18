package automation

import (
	"errors"
	"fmt"
)

// TriggerType is WHAT causes an invocation to be created for an automation
// (§8.4: "GitHub/Linear/webhook/cron triggers with a condition builder") --
// matches the automation_trigger_type Postgres enum exactly (migrations/
// 000055_automations_triggers_and_extras.up.sql).
//
// A single trigger_type/trigger_config column pair, not an
// automation_triggers side table: mockups.html's own Automations view
// (v-autos, its own "Trigger" table column) shows exactly ONE trigger per
// automation row ("cron · 02:00 UTC", "github · pull_request.labeled"),
// never a list -- confirming a one-automation-to-one-trigger shape is the
// one this codebase's own design already assumes, not a guess.
type TriggerType string

const (
	// TriggerTypeManual is an automation with no automatic trigger of its
	// own -- fired only via a direct CreateInvocation call
	// (invocationenqueue.go), exactly as every automation created before
	// this Step already behaves (backfilled default, see this migration's
	// own doc comment). Also the correct value for an automation whose
	// only intended trigger is a future, still-unbuilt "Run now" manual UI
	// action (a deliberate scope note, mirrored from automation.go's
	// identical "no caller exists yet" precedent for TriggerResume).
	TriggerTypeManual TriggerType = "manual"
	// TriggerTypeCron is a schedule-driven trigger -- TriggerConfig.Cron
	// (CronTriggerConfig) names the schedule; app/automation's own trigger
	// pump evaluates it every tick via CronMatches.
	TriggerTypeCron TriggerType = "cron"
	// TriggerTypeGitHub is a GitHub webhook-event-driven trigger --
	// TriggerConfig.GitHub (GitHubTriggerConfig) names the event/action/
	// label/name/conclusion filter, evaluated via MatchesGitHubTrigger and
	// live-dispatched (§8.4) through GitHubDispatchAllowlist -- see
	// this package's own doc.go for the full design.
	TriggerTypeGitHub TriggerType = "github"
	// TriggerTypeLinear is a Linear webhook-event-driven trigger --
	// TriggerConfig.Linear (LinearTriggerConfig) names the event/action/
	// team filter, evaluated via MatchesLinearTrigger and live-dispatched
	// (§8.4) through LinearDispatchAllowlist. Same doc.go note as
	// TriggerTypeGitHub applies.
	TriggerTypeLinear TriggerType = "linear"
	// TriggerTypeWebhook is a generic inbound-HTTP-call trigger: any
	// correctly bearer-token-authenticated POST to this automation's own
	// webhook endpoint fires it -- the "condition" IS authentication,
	// there is no further per-request filter to evaluate (unlike GitHub/
	// Linear's own event/action/label matchers). See internal/adapters/
	// inbound/httpapi's own automationwebhook.go.
	TriggerTypeWebhook TriggerType = "webhook"
)

// ErrUnknownTriggerType is ValidateTriggerType's own sentinel.
var ErrUnknownTriggerType = errors.New("automation: unknown trigger type")

// ValidateTriggerType reports whether t is one of the five recognized
// TriggerType values.
func ValidateTriggerType(t TriggerType) error {
	switch t {
	case TriggerTypeManual, TriggerTypeCron, TriggerTypeGitHub, TriggerTypeLinear, TriggerTypeWebhook:
		return nil
	default:
		return fmt.Errorf("automation: %w: %q", ErrUnknownTriggerType, string(t))
	}
}

// CronTriggerConfig is TriggerTypeCron's own trigger_config shape.
type CronTriggerConfig struct {
	// Schedule is a standard 5-field cron expression (cron.go's own
	// supported grammar) -- validated via ValidateCronExpr.
	Schedule string
}

// ErrEmptyCronSchedule is ValidateCronTriggerConfig's own sentinel for an
// empty Schedule -- distinct from cron.go's own ErrCronFieldCount/
// ErrCronFieldSyntax/ErrCronFieldRange (which all assume a non-empty
// candidate string was at least worth tokenizing).
var ErrEmptyCronSchedule = errors.New("automation: cron trigger config: schedule must not be empty")

// ValidateCronTriggerConfig validates cfg before it is accepted onto an
// automation with TriggerTypeCron.
func ValidateCronTriggerConfig(cfg CronTriggerConfig) error {
	if cfg.Schedule == "" {
		return ErrEmptyCronSchedule
	}
	if err := ValidateCronExpr(cfg.Schedule); err != nil {
		return fmt.Errorf("automation: cron trigger config: %w", err)
	}
	return nil
}

// GitHubTriggerConfig is TriggerTypeGitHub's own trigger_config shape: an
// automation fires when a GitHub webhook event arrives whose own
// EventType/Action/label/name/conclusion set matches this filter. Modeled,
// validated AND (§8.4) live-dispatched -- see this package's own doc.go
// for the full design, including GitHubDispatchAllowlist (the closed set
// of event types ever reaching MatchesGitHubTrigger at all).
type GitHubTriggerConfig struct {
	// Event is GitHub's own webhook event-type name verbatim (e.g.
	// "pull_request", "issues", "issue_comment") -- required.
	Event string
	// Action is that event's own "action" payload field (e.g. "labeled",
	// "opened") -- "" (the zero value) means "any action for this Event
	// matches", never itself a required filter.
	Action string
	// Label, when non-empty, additionally requires GitHubEventInput.Labels
	// to contain this exact label name -- "" means "no label filter".
	Label string

	// Name, when non-empty, additionally requires GitHubEventInput.Name to
	// match exactly. §8.4's own "normalise check_run and status ...
	// with name and conclusion": Name carries check_run's own `name` field
	// (e.g. "ci/lint") or status's own `context` field (e.g.
	// "continuous-integration/travis-ci") -- the two providers' own
	// differently-named equivalents of "which check/context is this",
	// normalized onto one filterable field rather than two, mirroring
	// Label's own exact-match semantics. "" means "no name filter".
	// Meaningless (always "" on the event side) for every event type that
	// carries neither field (pull_request, issues, issue_comment, push).
	Name string

	// Conclusion, when non-empty, additionally requires
	// GitHubEventInput.Conclusion to match exactly -- check_run's own
	// `conclusion` field (e.g. "success", "failure") or status's own
	// `state` field (e.g. "success", "pending"), normalized onto this one
	// field for the identical reason Name is. "" means "no conclusion
	// filter".
	Conclusion string
}

// ErrEmptyGitHubEvent is ValidateGitHubTriggerConfig's own sentinel.
var ErrEmptyGitHubEvent = errors.New("automation: github trigger config: event must not be empty")

// ErrGitHubEventNotDispatchable is ValidateGitHubTriggerConfig's own
// second sentinel -- D10 audit fix (confirmed finding: "a trigger for a
// non-allowlisted event is accepted and silently dead"). Before this
// fix, GitHubDispatchAllowlist (dispatch.go) was consulted ONLY at live
// dispatch time, never at automation-create time, so the API accepted a
// GitHubTriggerConfig naming an event outside it (a typo, or a real
// GitHub event this dispatch path simply does not subscribe to) and that
// automation was dead forever, with no feedback to whoever created it.
var ErrGitHubEventNotDispatchable = errors.New("automation: github trigger config: event is not in the dispatchable allowlist")

// ValidateGitHubTriggerConfig validates cfg before it is accepted onto an
// automation with TriggerTypeGitHub -- Event is required AND (D10 audit
// fix) must be one ClassifyGitHubDispatch itself recognizes: the SAME
// function dispatch.go's own live-dispatch path calls
// (app/automation.DispatchGitHubWebhookEvent) -- one register, consulted
// by both the create-time check here and the dispatch-time check there,
// so the two can never drift apart the way two independently-maintained
// copies of the same allowlist could. Action/Label are optional filters
// (see their own doc comments above).
func ValidateGitHubTriggerConfig(cfg GitHubTriggerConfig) error {
	if cfg.Event == "" {
		return ErrEmptyGitHubEvent
	}
	if ClassifyGitHubDispatch(cfg.Event) != GitHubDispatchNotSkipped {
		return fmt.Errorf("%w: %q", ErrGitHubEventNotDispatchable, cfg.Event)
	}
	return nil
}

// GitHubEventBranch is one entry from a GitHub `status` webhook event's own
// branches[] array, or the single synthetic entry a dispatch-wiring caller
// derives for an event type that names its own branch unambiguously
// (pull_request's head.ref/head.sha, push's ref/after, check_run's
// check_suite.head_branch/head_sha) -- see doc.go's own "branch tip, never
// containment" section for the full "why" this shape exists at all.
//
// GitHub's own docs describe a status event's branches[] field as the list
// of branches this commit is "part of" -- i.e. CONTAINS the commit, not
// the branch whose CURRENT TIP it is. Name is the branch name; HeadSHA is
// that branch's own current tip commit SHA (branches[].commit.sha in the
// raw payload) -- deliberately NOT named "SHA" alone, so a reader cannot
// mistake it for "the commit this event is about" (that is
// GitHubEventInput.SHA, compared against HeadSHA by TipBranchNames below,
// never assumed equal).
type GitHubEventBranch struct {
	Name    string
	HeadSHA string
}

// GitHubEventInput is the minimal shape MatchesGitHubTrigger/
// TargetMatchesGitHubEvent need from a live GitHub webhook event -- a
// caller (internal/app/automation's own githubdispatch.go) derives this
// from the real webhook payload; this package never parses a raw GitHub
// payload itself (§11, adapter-independence).
type GitHubEventInput struct {
	EventType string
	Action    string
	Labels    []string

	// RepoFullName is the event's own "repository.full_name" ("owner/
	// repo") -- TargetMatchesGitHubEvent's own repo-scoping half (see that
	// function's own doc comment); "" for a caller that never resolved it
	// (TargetMatchesGitHubEvent then never matches any target, fail
	// closed).
	RepoFullName string

	// DefaultBranch is the event's own "repository.default_branch" --
	// D4 audit fix's own resolution for what an UNCONFIGURED
	// (Target.Branch == "") target means, see TargetMatchesGitHubEvent's
	// own doc comment. Every GitHub webhook payload this dispatch path
	// parses embeds the full repository object this field comes from, so
	// populating it costs no extra lookup/I/O -- "" for a caller that
	// never resolved it (TargetMatchesGitHubEvent then never matches an
	// unconfigured target against this event, fail closed, exactly like
	// an unresolved RepoFullName above).
	DefaultBranch string

	// SHA is the commit this event pertains to (status: top-level "sha";
	// check_run: "check_run.head_sha"; pull_request: "pull_request.head.
	// sha"; push: "after") -- "" for an event type that carries no single
	// commit (issues, issue_comment), which by construction can never
	// satisfy TipBranchNames below (an empty SHA matches no branch's own
	// non-empty HeadSHA).
	SHA string

	// Branches is this event's own branch-tip evidence -- see
	// GitHubEventBranch's own doc comment for exactly what each entry
	// means and why containment is the wrong read of it. Empty for an
	// event type with no branch concept at all.
	Branches []GitHubEventBranch

	// Name is check_run's own `name` field or status's own `context`
	// field, normalized -- see GitHubTriggerConfig.Name's own doc comment.
	Name string
	// Conclusion is check_run's own `conclusion` field or status's own
	// `state` field, normalized -- see GitHubTriggerConfig.Conclusion's
	// own doc comment.
	Conclusion string
}

// MatchesGitHubTrigger reports whether in satisfies cfg's own filter: Event
// must match exactly; Action, if cfg.Action is non-empty, must also match
// exactly; Label, if cfg.Label is non-empty, must appear (exact string
// match) somewhere in in.Labels; Name/Conclusion, if non-empty, must each
// match in.Name/in.Conclusion exactly (§8.4's own check_run/status
// normalization). This function is deliberately blind to repo/branch
// scoping -- see TargetMatchesGitHubEvent below (and its own doc comment
// on why containment vs. tip identity is decided there, not here): a
// trigger's own Event/Action/Label/Name/Conclusion filter and an
// automation's own configured target repos are two independent questions,
// asked by two independent functions, exactly like app/automation's own
// dispatch caller evaluates them independently.
func MatchesGitHubTrigger(cfg GitHubTriggerConfig, in GitHubEventInput) bool {
	if cfg.Event != in.EventType {
		return false
	}
	if cfg.Action != "" && cfg.Action != in.Action {
		return false
	}
	if cfg.Label != "" {
		found := false
		for _, l := range in.Labels {
			if l == cfg.Label {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if cfg.Name != "" && cfg.Name != in.Name {
		return false
	}
	if cfg.Conclusion != "" && cfg.Conclusion != in.Conclusion {
		return false
	}
	return true
}

// TipBranchNames returns every branches[i].Name whose own HeadSHA equals
// sha exactly -- the ONE place this package turns a status/check_run
// event's own branch-containment evidence (GitHubEventBranch's own doc
// comment) into branch-TIP identity. A commit landed on a repo's default
// branch is, by definition, contained by every feature branch ever cut at
// or after it -- so a caller that instead scanned for a branch NAME
// appearing anywhere in branches[] (ignoring HeadSHA entirely) would treat
// every one of those feature branches as if this event were about their
// own current work, when it is not. sha == "" always returns nil (an event
// with no known commit can never be any branch's own current tip) --
// deliberately checked explicitly rather than left to fall out of the
// comparison loop below, so a zero-value GitHubEventInput reads as
// "matches nothing" by construction, not by accident.
func TipBranchNames(sha string, branches []GitHubEventBranch) []string {
	if sha == "" {
		return nil
	}
	var names []string
	for _, b := range branches {
		if b.HeadSHA == sha {
			names = append(names, b.Name)
		}
	}
	return names
}

// LinearTriggerConfig is TriggerTypeLinear's own trigger_config shape --
// mirrors GitHubTriggerConfig's own shape, Linear's own vocabulary
// (EventType/Action/TeamKey rather than Event/Action/Label): an automation
// fires when a Linear webhook event arrives whose own EventType/Action/team
// matches this filter. Modeled, validated, and live-dispatched exactly like
// GitHubTriggerConfig -- see doc.go.
type LinearTriggerConfig struct {
	// EventType is Linear's own webhook event category verbatim (e.g.
	// "Issue", "Comment") -- required.
	EventType string
	// Action is that event's own action (e.g. "create", "update") -- ""
	// means "any action for this EventType matches".
	Action string
	// TeamKey, when non-empty, additionally requires LinearEventInput.
	// TeamKey to match exactly -- "" means "no team filter".
	TeamKey string
}

// ErrEmptyLinearEventType is ValidateLinearTriggerConfig's own sentinel.
var ErrEmptyLinearEventType = errors.New("automation: linear trigger config: event type must not be empty")

// ErrLinearEventNotDispatchable mirrors ErrGitHubEventNotDispatchable's
// own D10 audit fix, for Linear.
var ErrLinearEventNotDispatchable = errors.New("automation: linear trigger config: event type is not in the dispatchable allowlist")

// ValidateLinearTriggerConfig validates cfg before it is accepted onto an
// automation with TriggerTypeLinear -- mirrors
// ValidateGitHubTriggerConfig's own D10 audit fix exactly: EventType must
// also be one ClassifyLinearDispatch itself recognizes, the SAME register
// app/automation.DispatchLinearWebhookEvent consults at live dispatch
// time.
func ValidateLinearTriggerConfig(cfg LinearTriggerConfig) error {
	if cfg.EventType == "" {
		return ErrEmptyLinearEventType
	}
	if ClassifyLinearDispatch(cfg.EventType) != LinearDispatchNotSkipped {
		return fmt.Errorf("%w: %q", ErrLinearEventNotDispatchable, cfg.EventType)
	}
	return nil
}

// LinearEventInput is the minimal shape MatchesLinearTrigger needs from a
// live Linear webhook event -- see GitHubEventInput's own doc comment for
// the identical "caller derives this, this package never parses a raw
// payload" reasoning.
type LinearEventInput struct {
	EventType string
	Action    string
	TeamKey   string
}

// MatchesLinearTrigger reports whether in satisfies cfg's own filter --
// mirrors MatchesGitHubTrigger's own exact-match-per-populated-field logic.
func MatchesLinearTrigger(cfg LinearTriggerConfig, in LinearEventInput) bool {
	if cfg.EventType != in.EventType {
		return false
	}
	if cfg.Action != "" && cfg.Action != in.Action {
		return false
	}
	if cfg.TeamKey != "" && cfg.TeamKey != in.TeamKey {
		return false
	}
	return true
}

// Package autonomy names the vocabulary of technical plan §40's autonomy
// guardrails that more than one package shares: today, the sites the
// freeze (§40.2) is consulted at, and what a skipped action records.
//
// A site is a place in the code where an action starts without a person
// asking for it right then -- §40.2's own test. Only those consult the
// freeze. A person's command never does: a prompt (REST, MCP, chat, the
// issue tracker, a code-host mention), a plan approval, the decision
// inbox's Merge click, the label and button re-trigger, a workflow step
// decision, a stop and a resume. Nor does the consumer of a person's owed
// review request (§24.9), which re-runs that person's request on the
// person's path; nor the dispatch of a turn already enqueued, whoever
// asked for it, since holding it would also hold the person's prompts
// queued behind it; nor a release's composition review, the second half
// of a review a person asked for; nor notifications about work already
// done; nor the infrastructure loops (the reconciler, image builds,
// digests, sweeps, token refresh, sandbox rotation, idle stop, a turn's
// deadline), which a freeze must never leak resources or sever a turn
// through (§32.8).
//
// A site that reads the freeze set skips its action: it records the skip
// (OutcomeSkipped, with a SkipReason) and consumes nothing, so every
// candidate is still a candidate once the freeze lifts. A skip is never a
// failed status, a counted attempt, a strike or a dead letter.
//
// Every Site a package checks is registered with internal/ops'
// ScanAutonomyFreezeSites, which fails the build when a registered site's
// check is removed, when a Site constant here has no registration, and
// when a person's path consults the freeze.
package autonomy

// Site names one automatic-action site the freeze is consulted at. Its
// value is the site label of autonomy_freeze_skip_total.
type Site string

// The automatic-action sites, each with the section that specifies its
// action.
const (
	// SiteAutoMerge is the auto-merge worker's merge of an auto-approved
	// pull request (§21.2): checked before its live re-validation and again
	// right before the merge.
	SiteAutoMerge Site = "auto_merge"
	// SiteSentinelFixMerge is the sentinel-fix merge gate's unattended
	// merge of a fix pull request when its origin merges (§17.4).
	SiteSentinelFixMerge Site = "sentinel_fix_merge"
	// SiteSentinelAutoFixSpawn is the sentinel auto-fix's branch and child
	// session (§17.2), delivered through the outbox.
	SiteSentinelAutoFixSpawn Site = "sentinel_auto_fix_spawn"
	// SiteDescriptionAutofix is the unattended rewrite of a pull request's
	// description (§26.2), delivered through the outbox.
	SiteDescriptionAutofix Site = "description_autofix"
	// SiteAutoReReview is the automatic re-review's turn insert after a
	// push (§24.3 step 4).
	SiteAutoReReview Site = "auto_re_review"
	// SiteAutomationCron is a scheduled automation's fire (§8.4).
	SiteAutomationCron Site = "automation_cron"
	// SiteAutomationFanOut is an automation invocation's start: its runs
	// and their sessions (§3.5).
	SiteAutomationFanOut Site = "automation_fan_out"
	// SiteWorkflowAdvance is the workflow engine's automatic advance to a
	// run's next attempt once a step's turn ends (§25.9): held in a row
	// while frozen, and released exactly once after.
	SiteWorkflowAdvance Site = "workflow_advance"
)

// The sites whose actions are specified but not built. Each consults the
// freeze when it is built (§40.6), and registers here then.
const (
	// SiteChainEnqueue is a chain's deterministic server-side enqueue of
	// its target (§38.3).
	SiteChainEnqueue Site = "chain_enqueue"
	// SiteTrainAdvance is a train's verdict-gated advance to its next link
	// (§39.3).
	SiteTrainAdvance Site = "train_advance"
)

// AllSites is every site built today, in the order the constants above
// declare them.
var AllSites = []Site{
	SiteAutoMerge,
	SiteSentinelFixMerge,
	SiteSentinelAutoFixSpawn,
	SiteDescriptionAutofix,
	SiteAutoReReview,
	SiteAutomationCron,
	SiteAutomationFanOut,
	SiteWorkflowAdvance,
}

// Reserved is every site whose action is not built yet.
var Reserved = []Site{
	SiteChainEnqueue,
	SiteTrainAdvance,
}

// SkipReason is why a site skipped its action.
type SkipReason string

const (
	// SkipFrozen is a skip because the freeze is set.
	SkipFrozen SkipReason = "frozen"
	// SkipFreezeUnreadable is a skip because the freeze could not be read.
	// A read that fails is a skip, never a pass: the freeze is the control
	// an operator reaches for during an incident, which is when a read is
	// likeliest to fail.
	SkipFreezeUnreadable SkipReason = "freeze_unreadable"
)

// OutcomeSkipped is the outcome a site records for an action the freeze
// held, distinct from a failure.
const OutcomeSkipped = "skipped"

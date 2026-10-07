// This file (notify.go) implements §25.9's ("workflow HITL gate +
// circuit breaker", §25.9) own notification delivery: enqueueWorkflowNotice
// posts ONE already-rendered, human-readable notice to whichever channel
// sessionRow's own spawn_source resolves to -- reused for BOTH §25.9
// events this Step fires a notice for (a step reaching awaiting_decision;
// a run escalating to needs_review), the caller supplying different text
// for each (see advance.go's own two call sites).
//
// Destination resolution is internal/app/sessionnotice's Enqueue, the one
// router every notice of this shape shares (the session guard's included),
// mirroring internal/app/sessionactor's own enqueueOutboxNotification
// (outboxenqueue.go): reverse-lookup the session's own
// slack_thread_sessions/linear_agent_sessions/github_pr_sessions row
// (whichever one exists, keyed by spawn_source), reusing the SAME three
// wire payload shapes those existing plain notifiers already consume
// (slackapi.Payload/linearapi.Payload/githubapi.Payload). A
// 'web'- or 'mcp'-origin session enqueues nothing and logs nothing: neither
// has an external channel (the browser re-reads the session, and an MCP
// client polls its status or waits on it, technical plan §43.20). A bot-
// origin session missing its own reverse-lookup row also enqueues nothing
// -- logged, never an error propagated to the caller: a failed/absent
// notification must never undo or block the state change (run escalated,
// step awaiting decision) that already committed alongside it, exactly
// like every other outbox-enqueue call site in this codebase treats its own
// best-effort notify step.

package workflowengine

import (
	"context"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/autonomy"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionnotice"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// Deps bundles every collaborator OnTurnCompleted (completion.go) and the
// HTTP decide endpoint (internal/adapters/inbound/httpapi) need to actually
// carry out a workflow.NextStep verdict (advance.go's ApplyStepOutcome) --
// every field MUST already be scoped to the caller's own open transaction
// (store.WithTx(tx)), mirroring how OnTurnCompleted's pre-existing signature
// already required its lone `workflows *postgres.WorkflowStore` parameter
// to be pre-scoped that way: §5.1's own "written in the same tx as the
// state change" applies to the outbox enqueue here exactly like it does to
// every other outbox-writing call site in this codebase, and the run/
// step-run/turn writes must all land atomically with whatever triggered
// them (a turn completing, a human's decide-endpoint call).
type Deps struct {
	// Workflows is the engine's own pre-existing dependency (§25.6) --
	// every workflow_runs/workflow_step_runs read/write in this package
	// goes through it.
	Workflows *postgres.WorkflowStore
	// Turns creates the next attempt's own ordinary turn row (advance.go's
	// dispatchNextAttempt) -- the SAME store every other turn-creation call
	// site in this codebase uses, here called directly (never through
	// createTurnLocked/CreateTurnCore) for the same reason DecidePlanOnTx's
	// own implementation-turn insert does: this caller already holds the
	// session row's lock and does not want createTurnLocked's own
	// unrelated checks (the open-turn/busy gate, the awaiting-plan gate)
	// re-run for a system/decision-triggered turn that is neither. It must
	// be bound to that caller's transaction (TurnStore.WithTx):
	// CreateAndArmDispatch refuses a pool-bound store, since the turn and
	// its dispatch timer commit together (technical plan §2, §3.3).
	Turns *postgres.TurnStore
	// SlackThreadSessions/LinearAgentSessions/GitHubPRSessions back this
	// file's own destination resolution -- the SAME three reverse-lookup
	// stores internal/app/sessionactor's own enqueueOutboxNotification
	// (outboxenqueue.go) already uses identically.
	SlackThreadSessions *postgres.SlackThreadSessionStore
	LinearAgentSessions *postgres.LinearAgentSessionStore
	GitHubPRSessions    *postgres.GitHubPRSessionStore
	// Outbox is where enqueueWorkflowNotice writes the one notification row
	// this Step ever enqueues per event.
	Outbox *postgres.OutboxStore

	// Guard is the session guard (internal/app/turnguard, technical plan
	// §40.1), bound to the caller's transaction, which holds the session's
	// row lock: every attempt this package dispatches is admitted by it
	// first (advance.go's admitNextAttempt), before its step run is
	// created, so a refused attempt leaves no orphan step run. Never nil
	// where an attempt can be dispatched: a nil Guard admits nothing, and
	// the attempt is not created.
	Guard *turnguard.Bound
	// Origin is who asked for the attempts this call may dispatch:
	// sessionguard.OriginAutomatic for the session actor's own advance
	// (OnTurnCompleted), sessionguard.OriginPerson for the decide
	// endpoint's approve and revise. It decides what a refusal does: an
	// automatic advance escalates the run with the guard's text as its one
	// notice; a person's decision is answered with the refusal and rolled
	// back.
	Origin sessionguard.Origin

	// Autonomy is the autonomy freeze (internal/app/autonomy, technical
	// plan §40.2), bound to the caller's transaction: OnTurnCompleted reads
	// it once an attempt's next step would advance, and holds the advance
	// in a row while autonomy is frozen (completion.go). Only the session
	// actor's own advance sets it -- a person's decision on a step is never
	// read against the freeze, so the decide endpoint leaves it nil, and
	// never reaches OnTurnCompleted. A nil Autonomy where OnTurnCompleted
	// runs is a wiring bug, logged at Error, and the advance proceeds (the
	// package's fail-open rule, doc.go): a source test in
	// internal/app/sessionactor keeps every caller wiring it.
	Autonomy *autonomy.Bound

	// EpistemicCheckDefault (F6, adversarial review) is the SAME
	// platform.Config.EpistemicCheckDefault value every other
	// createTurnLocked-reaching caller in this codebase now threads
	// through -- advance.go's own dispatchNextAttempt is this package's
	// ONE site that inserts a turn directly (bypassing createTurnLocked/
	// CreateTurnCore entirely, this file's own doc comment on Turns
	// explains why), so it is also this package's own one site that must
	// separately route through turn.MaybeInjectEpistemicPreamble. Every
	// workflow-engine-dispatched turn is an ordinary build turn (workflow
	// runs have no notion of a "review session" at all -- internal/domain/
	// workflow is a wholly separate subsystem from internal/adapters/
	// inbound/github's PR-review coalescing), so no F7-style
	// hardcoded-false carve-out applies here; every caller below passes
	// its own real, operator-configured default.
	EpistemicCheckDefault bool
}

// workflowNoticeKinds are the outbox kinds a workflow notice travels
// under, one a channel.
var workflowNoticeKinds = sessionnotice.Kinds{
	Slack:  ports.NotificationKindSlackWorkflowDecision,
	Linear: ports.NotificationKindLinearWorkflowDecision,
	GitHub: ports.NotificationKindGitHubWorkflowDecision,
}

// enqueueWorkflowNotice enqueues one outbox row carrying text to
// sessionRow's own resolved destination, under the workflow decision kinds
// -- sessionnotice.Enqueue, the one router of a session's notices, holds
// the destination resolution and the no-op cases this file's top doc
// comment describes. Never returns an error for a "no destination" outcome
// (a legitimate, common case -- e.g. a 'web'- or 'mcp'-origin session);
// only a genuine store failure (a reverse-lookup query itself erroring,
// not merely finding no row, or the outbox insert itself failing) is
// returned, so the caller can decide whether that is worth failing its own
// larger operation over (both of advance.go's own two call sites log and
// continue rather than propagate, mirroring OnTurnCompleted's own
// fail-open discipline for this exact class of bookkeeping/notification
// concern). enqueued reports whether a row was written.
func enqueueWorkflowNotice(ctx context.Context, deps Deps, sessionRow sqlcgen.Session, text string) (enqueued bool, err error) {
	return sessionnotice.Enqueue(ctx, sessionnotice.Stores{
		SlackThreadSessions: deps.SlackThreadSessions,
		LinearAgentSessions: deps.LinearAgentSessions,
		GitHubPRSessions:    deps.GitHubPRSessions,
		Outbox:              deps.Outbox,
	}, sessionRow, workflowNoticeKinds, text)
}

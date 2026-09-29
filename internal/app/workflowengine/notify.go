// This file (notify.go) implements §25.9's ("workflow HITL gate +
// circuit breaker", §25.9) own notification delivery: enqueueWorkflowNotice
// posts ONE already-rendered, human-readable notice to whichever channel
// sessionRow's own spawn_source resolves to -- reused for BOTH §25.9
// events this Step fires a notice for (a step reaching awaiting_decision;
// a run escalating to needs_review), the caller supplying different text
// for each (see advance.go's own two call sites).
//
// Destination resolution mirrors internal/app/sessionactor's own
// enqueueOutboxNotification (outboxenqueue.go) exactly: reverse-lookup the
// session's own slack_thread_sessions/linear_agent_sessions/
// github_pr_sessions row (whichever one exists, keyed by spawn_source),
// reusing the SAME three wire payload shapes those existing plain
// notifiers already consume (slackapi.Payload/linearapi.Payload/
// githubapi.Payload) -- no new payload type anywhere in this file. A
// 'web'-origin session, or a non-web session missing its own reverse-lookup
// row, enqueues nothing (there is no external channel to notify either
// way) -- logged, never an error propagated to the caller: a failed/absent
// notification must never undo or block the state change (run escalated,
// step awaiting decision) that already committed alongside it, exactly
// like every other outbox-enqueue call site in this codebase treats its own
// best-effort notify step.

package workflowengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
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
	// re-run for a system/decision-triggered turn that is neither.
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

// enqueueWorkflowNotice enqueues one outbox row carrying text to
// sessionRow's own resolved destination -- see this file's own top doc
// comment for the full destination-resolution/no-op-cases contract. Never
// returns an error for a "no destination" outcome (a legitimate, common
// case -- e.g. a 'web'-origin session); only a genuine store failure
// (a reverse-lookup query itself erroring, not merely finding no row, or
// the outbox insert itself failing) is returned, so the caller can decide
// whether that is worth failing its own larger operation over (both of
// advance.go's own two call sites log and continue rather than propagate,
// mirroring OnTurnCompleted's own fail-open discipline for this exact class
// of bookkeeping/notification concern).
func enqueueWorkflowNotice(ctx context.Context, deps Deps, sessionRow sqlcgen.Session, text string) error {
	logger := platform.Logger(ctx)

	if sessionRow.SpawnSource == sqlcgen.SessionSpawnSourceWeb {
		return nil
	}

	var kind ports.NotificationKind
	var payload any

	switch sessionRow.SpawnSource {
	case sqlcgen.SessionSpawnSourceSlack:
		row, err := deps.SlackThreadSessions.GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("workflowengine: enqueue workflow notice: slack-origin session has no slack_thread_sessions row; skipping")
				return nil
			}
			return fmt.Errorf("workflowengine: get slack thread session: %w", err)
		}
		kind = ports.NotificationKindSlackWorkflowDecision
		payload = slackapi.Payload{ChannelID: row.ChannelID, ThreadTS: row.ThreadTs, Text: text}

	case sqlcgen.SessionSpawnSourceLinear:
		row, err := deps.LinearAgentSessions.GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("workflowengine: enqueue workflow notice: linear-origin session has no linear_agent_sessions row; skipping")
				return nil
			}
			return fmt.Errorf("workflowengine: get linear agent session: %w", err)
		}
		kind = ports.NotificationKindLinearWorkflowDecision
		payload = linearapi.Payload{AgentSessionID: row.AgentSessionID, OrganizationID: row.OrganizationID, Text: text, Success: true}

	case sqlcgen.SessionSpawnSourceGithub:
		row, err := deps.GitHubPRSessions.GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("workflowengine: enqueue workflow notice: github-origin session has no github_pr_sessions row; skipping")
				return nil
			}
			return fmt.Errorf("workflowengine: get github pr session: %w", err)
		}
		owner, repo, ok := reposource.SplitFullName(row.RepoFullName)
		if !ok {
			logger.Warn("workflowengine: enqueue workflow notice: could not split repo_full_name; skipping", "repo_full_name", row.RepoFullName)
			return nil
		}
		kind = ports.NotificationKindGitHubWorkflowDecision
		payload = githubapi.Payload{Owner: owner, Repo: repo, PRNumber: int(row.PrNumber), Text: text}

	default:
		// A source this binary has no channel for. Session.spawnSource is
		// an open enum (contracts/manifest.json's openEnums), so a newer
		// migration can add a value that an older binary, still serving
		// during a rolling deploy, reads here. With no channel to notify,
		// nothing is enqueued; the source is logged so the gap shows.
		logger.Warn("workflowengine: enqueue workflow notice: unrecognized spawn_source; skipping", "spawn_source", string(sessionRow.SpawnSource))
		return nil
	}

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("workflowengine: marshal workflow notice payload: %w", err)
	}

	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}

	if _, err := deps.Outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID:     sessionRow.ID,
		Kind:          string(kind),
		Payload:       rawPayload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("workflowengine: create workflow notice outbox entry: %w", err)
	}
	return nil
}

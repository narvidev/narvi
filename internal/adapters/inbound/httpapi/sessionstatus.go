package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// GetSessionStatus backs GET /api/sessions/{sessionID}/status (technical
// plan §43.20): what one session's work is doing now -- queued, running,
// awaiting approval, idle or finished -- and how long a client should wait
// before asking again, as restdtos.SessionActivity. It is also the twin of
// the narvi_get_session_status MCP tool.
//
// The same gate as GetSession (get.go), deliberately: signed in (the
// /api/sessions group's own auth.Middleware), 400 on a malformed id, 404
// when the session does not exist, and no per-session visibility beyond
// that, because this codebase has none -- a bearer token reaching this
// route through the MCP bridge can never read more than its user's cookie
// could.
//
// Every fact comes from ONE statement (SessionStore.ActivityFacts), and the
// activity is session.DeriveActivity over the turn queue and the human
// gates in that snapshot -- never sessions.status, which is re-derived
// only when a turn reaches a terminal state and so can hold any of its
// five values while a turn is queued or running. The response carries no
// events: the transcript is GET /api/sessions/{sessionID}/events, a
// separate, paginated read.
func GetSessionStatus(sessions *postgres.SessionStore, timeouts platform.Timeouts) http.HandlerFunc {
	delays := statusDelayTable(timeouts)
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		facts, err := sessions.ActivityFacts(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: read session activity facts failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		dto, err := sessionActivityToDTO(facts, delays)
		if err != nil {
			logger.Error("httpapi: session activity facts are unreadable", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	}
}

// statusDelayTable is session.DelayTable from the MCPStatusDelay* fields
// of platform.Timeouts, the only place those values live.
func statusDelayTable(t platform.Timeouts) session.DelayTable {
	return session.DelayTable{
		Starting:      t.MCPStatusDelayStarting,
		Queued:        t.MCPStatusDelayQueued,
		Running:       t.MCPStatusDelayRunning,
		AwaitingHuman: t.MCPStatusDelayAwaitingHuman,
		Settled:       t.MCPStatusDelaySettled,
		Floor:         t.MCPStatusDelayFloor,
		Ceiling:       t.MCPStatusDelayCeiling,
	}
}

// activityInput is session.ActivityInput from one facts row: the turn
// histogram as the snapshot counted it, and each human gate as "an id was
// found". sessions.status is not an input.
func activityInput(facts sqlcgen.GetSessionActivityFactsRow) (session.ActivityInput, error) {
	var raw map[string]int64
	if err := json.Unmarshal(facts.TurnCounts, &raw); err != nil {
		return session.ActivityInput{}, fmt.Errorf("decode turn counts %q: %w", facts.TurnCounts, err)
	}
	counts := make(map[turn.State]int, len(raw))
	for state, n := range raw {
		counts[turn.State(state)] = int(n)
	}
	return session.ActivityInput{
		TurnCounts:                   counts,
		PlanAwaitingApproval:         facts.AwaitingPlanID.Valid,
		WorkflowStepAwaitingDecision: facts.AwaitingStepID.Valid,
		WorkflowEscalationOpen:       facts.EscalatedRunID.Valid,
	}, nil
}

// sessionActivityToDTO derives the activity and the suggested delay from
// one facts row and renders restdtos.SessionActivity.
func sessionActivityToDTO(facts sqlcgen.GetSessionActivityFactsRow, delays session.DelayTable) (restdtos.SessionActivity, error) {
	in, err := activityInput(facts)
	if err != nil {
		return restdtos.SessionActivity{}, err
	}
	activity := session.DeriveActivity(in)

	var sandboxState *sandbox.State
	var sandboxStatus *restdtos.SessionActivitySandboxStatus
	if facts.SandboxStatus != nil {
		s := sandbox.State(*facts.SandboxStatus)
		sandboxState = &s
		sandboxStatus = &restdtos.SessionActivitySandboxStatus{Value: string(*facts.SandboxStatus)}
	}
	delay := session.SuggestedReadDelay(activity, sandboxState, delays)

	return restdtos.SessionActivity{
		SessionId:             facts.SessionID.String(),
		Activity:              restdtos.SessionActivityActivity(activity),
		Settled:               activity.Settled(),
		PendingTurns:          in.TurnCounts[turn.StatePending],
		InFlightTurn:          inFlightTurnDTO(facts),
		Awaiting:              awaitingDTO(facts),
		LastRun:               lastRunDTO(facts),
		SandboxStatus:         sandboxStatus,
		Archived:              facts.Archived,
		SuggestedDelaySeconds: wholeSecondsRoundedUp(delay),
		ObservedAt:            facts.ObservedAt.Time,
	}, nil
}

// wholeSecondsRoundedUp renders a suggested delay in the wire's whole
// seconds, rounding up so a sub-second delay never reads as "now".
func wholeSecondsRoundedUp(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

func inFlightTurnDTO(facts sqlcgen.GetSessionActivityFactsRow) *restdtos.SessionActivityInFlightTurn {
	if !facts.InFlightTurnID.Valid {
		return nil
	}
	out := &restdtos.SessionActivityInFlightTurn{
		TurnId: facts.InFlightTurnID.String(),
		State:  restdtos.SessionActivityInFlightTurnState(facts.InFlightTurnStatus),
	}
	if facts.InFlightDispatchedAt.Valid {
		at := facts.InFlightDispatchedAt.Time
		out.DispatchedAt = &at
	}
	return out
}

// awaitingDTO reports the human gate open on the session, whatever the
// activity: a plan first, then a workflow step, then an escalated run --
// which the facts row carries only while that escalation is still open
// (GetSessionActivityFacts' escalated lookup).
func awaitingDTO(facts sqlcgen.GetSessionActivityFactsRow) *restdtos.SessionActivityAwaiting {
	switch {
	case facts.AwaitingPlanID.Valid:
		return &restdtos.SessionActivityAwaiting{
			Kind:  restdtos.SessionActivityAwaitingKindPlan,
			Id:    facts.AwaitingPlanID.String(),
			Since: facts.AwaitingPlanSince.Time,
		}
	case facts.AwaitingStepID.Valid:
		return &restdtos.SessionActivityAwaiting{
			Kind:  restdtos.SessionActivityAwaitingKindWorkflowStep,
			Id:    facts.AwaitingStepID.String(),
			Since: facts.AwaitingStepSince.Time,
		}
	case facts.EscalatedRunID.Valid:
		return &restdtos.SessionActivityAwaiting{
			Kind:  restdtos.SessionActivityAwaitingKindWorkflowEscalation,
			Id:    facts.EscalatedRunID.String(),
			Since: facts.EscalatedRunSince.Time,
		}
	default:
		return nil
	}
}

func lastRunDTO(facts sqlcgen.GetSessionActivityFactsRow) *restdtos.SessionActivityLastRun {
	if !facts.LastRunTurnID.Valid {
		return nil
	}
	out := &restdtos.SessionActivityLastRun{
		TurnId:        facts.LastRunTurnID.String(),
		Outcome:       restdtos.SessionActivityLastRunOutcome(facts.LastRunStatus),
		FailureReason: lastRunFailureReason(facts),
	}
	if facts.LastRunCompletedAt.Valid {
		at := facts.LastRunCompletedAt.Time
		out.FinishedAt = &at
	}
	return out
}

// lastRunFailureReason is sessions.failure_reason, and only when it can
// describe nothing but the last run: a turn row has no reason column, and
// the session's reason is that of whichever turn its status was last
// derived from. So it is given only when the last run is the session's
// newest turn, did not complete, and the session's recorded outcome is
// that very outcome (failed or cancelled); nil otherwise.
func lastRunFailureReason(facts sqlcgen.GetSessionActivityFactsRow) *restdtos.SessionActivityLastRunFailureReason {
	if !facts.LastRunTurnID.Valid || facts.LastRunTurnID != facts.NewestTurnID || facts.SessionFailureReason == nil {
		return nil
	}
	var recorded sqlcgen.SessionStatus
	switch turn.State(facts.LastRunStatus) {
	case turn.StateFailed:
		recorded = sqlcgen.SessionStatusFailed
	case turn.StateCancelled:
		recorded = sqlcgen.SessionStatusCancelled
	default:
		return nil
	}
	if facts.SessionStatus != recorded {
		return nil
	}
	return &restdtos.SessionActivityLastRunFailureReason{Value: string(*facts.SessionFailureReason)}
}

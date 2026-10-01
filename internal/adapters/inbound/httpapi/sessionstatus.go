package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactivity"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// GetSessionStatus backs GET /api/sessions/{sessionID}/status (technical
// plan §43.20): what one session's work is doing now -- queued, running,
// delivering, scheduled, awaiting approval, idle or finished -- and how
// long a client should wait before asking again, as
// restdtos.SessionActivity. It is also the twin of the
// narvi_get_session_status MCP tool.
//
// The same gate as GetSession (get.go), deliberately: signed in (the
// /api/sessions group's own auth.Middleware), 400 on a malformed id, 404
// when the session does not exist, and no per-session visibility beyond
// that, because this codebase has none -- a bearer token reaching this
// route through the MCP bridge can never read more than its user's cookie
// could.
//
// Every fact comes from ONE statement (SessionStore.ActivityFacts), and the
// activity is session.DeriveActivity over the turn queue, the delivery
// stamp, the server-side work armed to create a turn, and the human gates
// in that snapshot -- never sessions.status, which is re-derived
// only when a turn reaches a terminal state and so can hold any of its
// five values while a turn is queued or running. The response carries no
// events: the transcript is GET /api/sessions/{sessionID}/events, a
// separate, paginated read.
//
// ?waitSeconds=N makes it the bounded wait (row 182's piece (b)), and the
// twin of narvi_wait_for_session too: waiter reads the same snapshot at
// once and, while it is not settled, again every MCPWaitPollInterval, for
// at most N seconds clamped to MCPWaitMaxDuration, and the answer is the
// latest snapshot with wait set to how the wait ended
// (sessionactivity.Waiter). Every read is this route's own plain read --
// one statement on a pool connection taken for it and given back before
// the sleep -- behind the same gate: a wait sees exactly what a read
// does, only later. A caller already running MCPWaitMaxConcurrentPerKey
// waits (per MCP grant, or per user for a cookie), a user already running
// MCPWaitMaxConcurrentPerUser across every grant and the browser, or a
// replica running MCPWaitMaxConcurrentPerReplica, gets its first read at
// once, reason "capacity". Absent or 0 is the plain read, byte for byte
// (wait is omitted); a negative or malformed value is a 400.
//
// waiter is one per replica (controlplane builds it once, for this route
// and the MCP twin alike, and wires its Interrupt to the HTTP server's
// shutdown).
func GetSessionStatus(sessions *postgres.SessionStore, waiter *sessionactivity.Waiter, timeouts platform.Timeouts) http.HandlerFunc {
	delays := statusDelayTable(timeouts)
	bounds := statusBoundsFrom(timeouts)
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		waitSeconds, ok := parseWaitSeconds(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		read := func(ctx context.Context) (restdtos.SessionActivity, error) {
			facts, err := sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
			if err != nil {
				return restdtos.SessionActivity{}, err
			}
			dto, err := sessionActivityToDTO(facts, delays, bounds)
			if err != nil {
				return restdtos.SessionActivity{}, &unreadableActivityError{err: err}
			}
			return dto, nil
		}

		if waitSeconds == 0 {
			dto, err := read(ctx)
			if err != nil {
				writeStatusReadError(ctx, w, err)
				return
			}
			writeJSON(w, http.StatusOK, dto)
			return
		}

		var latest restdtos.SessionActivity
		outcome, err := waiter.Wait(ctx, waitCaller(ctx), waitSeconds, func(ctx context.Context) (bool, error) {
			dto, err := read(ctx)
			if err != nil {
				return false, err
			}
			latest = dto
			return dto.Settled, nil
		})
		if err != nil {
			writeStatusReadError(ctx, w, err)
			return
		}
		latest.Wait = &restdtos.SessionActivityWait{
			Reason:   restdtos.SessionActivityWaitReason(outcome.Reason),
			WaitedMs: int(outcome.Waited.Milliseconds()),
		}
		logger.Debug("httpapi: session status wait ended", "reason", string(outcome.Reason), "waited_ms", latest.Wait.WaitedMs, "activity", string(latest.Activity))
		writeJSON(w, http.StatusOK, latest)
	}
}

// unreadableActivityError marks a facts row sessionActivityToDTO could not
// decode -- this build's defect, not the database's -- so it is logged as
// such.
type unreadableActivityError struct{ err error }

func (e *unreadableActivityError) Error() string { return e.err.Error() }
func (e *unreadableActivityError) Unwrap() error { return e.err }

// writeStatusReadError answers a status read (or a wait's read) that
// failed: 404 when the session does not exist; when the request itself
// ended first -- the client went away mid-wait -- a 503 nobody reads,
// logged at DEBUG only, since that is an ordinary way for a wait to end;
// otherwise 500, logged.
func writeStatusReadError(ctx context.Context, w http.ResponseWriter, err error) {
	logger := platform.Logger(ctx)
	var unreadable *unreadableActivityError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "session not found")
	case ctx.Err() != nil:
		logger.Debug("httpapi: session status read ended with its request", "error", err)
		writeError(w, http.StatusServiceUnavailable, "request cancelled")
	case errors.As(err, &unreadable):
		logger.Error("httpapi: session activity facts are unreadable", "error", unreadable.err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		logger.Error("httpapi: read session activity facts failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// parseWaitSeconds reads ?waitSeconds=: absent or 0 is no wait (0 is
// returned, the plain read); a positive whole number of seconds is a wait
// of that long, clamped later by the Waiter to MCPWaitMaxDuration -- one
// too large for an int64 included, which is the maximum like any other
// large value, never a refusal. A negative value, an empty one, or
// anything but a decimal integer writes 400 and returns ok=false.
func parseWaitSeconds(w http.ResponseWriter, r *http.Request) (int64, bool) {
	values, present := r.URL.Query()["waitSeconds"]
	if !present {
		return 0, true
	}
	// Out of range, ParseInt returns math.MaxInt64 (or math.MinInt64) with
	// strconv.ErrRange: the first is a large wait the Waiter clamps like
	// any other, the second negative.
	n, err := strconv.ParseInt(values[0], 10, 64)
	if (err != nil && !errors.Is(err, strconv.ErrRange)) || n < 0 {
		writeError(w, http.StatusBadRequest, "malformed waitSeconds")
		return 0, false
	}
	return n, true
}

// waitCaller is whose caps a wait counts against (sessionactivity.Caller).
// Its key is the MCP grant the request was authenticated under
// (auth.RequireMCPBearer attaches it; every token of one client
// authorization shares it), or, for a cookie request, the signed-in user.
// Its user is the signed-in user either way -- RequireMCPBearer attaches
// the grant's user as the AuthenticatedUser, exactly as the cookie gate
// does -- so every grant of one user and their browser count against the
// one per-user cap, however many clients that user has authorized. Both
// are counts' keys and nothing more: no permission ever derives from them.
func waitCaller(ctx context.Context) sessionactivity.Caller {
	var c sessionactivity.Caller
	if user, ok := platform.UserFromContext(ctx); ok {
		c.User = user.ID
		c.Key = "user:" + user.ID
	}
	if grant, ok := platform.MCPGrantFromContext(ctx); ok {
		c.Key = "grant:" + grant.GrantID
	}
	return c
}

// statusDelayTable is session.DelayTable from the MCPStatusDelay* fields
// of platform.Timeouts, the only place those values live.
func statusDelayTable(t platform.Timeouts) session.DelayTable {
	return session.DelayTable{
		Starting:        t.MCPStatusDelayStarting,
		Queued:          t.MCPStatusDelayQueued,
		Running:         t.MCPStatusDelayRunning,
		Delivering:      t.MCPStatusDelayDelivering,
		Scheduled:       t.MCPStatusDelayScheduled,
		ScheduledMargin: t.MCPStatusScheduledMargin,
		AwaitingHuman:   t.MCPStatusDelayAwaitingHuman,
		Settled:         t.MCPStatusDelaySettled,
		Floor:           t.MCPStatusDelayFloor,
		Ceiling:         t.MCPStatusDelayCeiling,
	}
}

// statusBounds are the platform.Timeouts values that decide which work
// recorded in a facts row still counts, measured on the database's clock
// against the snapshot's own instant.
type statusBounds struct {
	// deliveryWindow bounds a push/PR delivery stamp (MCPStatusDeliveryWindow).
	deliveryWindow time.Duration
	// releaseCheckPumpInterval is how soon the release manifest worker
	// claims a row that is waiting: an unclaimed check comes due then.
	releaseCheckPumpInterval time.Duration
	// releaseCheckTimeout ends a claimed check (the worker's own context),
	// so a claimed check comes due then at the latest.
	releaseCheckTimeout time.Duration
	// releaseCheckWindow bounds a claimed check whose worker died:
	// ReleaseManifestCheckTimeout plus MCPStatusScheduledMargin.
	releaseCheckWindow time.Duration
}

func statusBoundsFrom(t platform.Timeouts) statusBounds {
	return statusBounds{
		deliveryWindow:           t.MCPStatusDeliveryWindow,
		releaseCheckPumpInterval: t.ReleaseManifestCheckPumpInterval,
		releaseCheckTimeout:      t.ReleaseManifestCheckTimeout,
		releaseCheckWindow:       t.ReleaseManifestCheckTimeout + t.MCPStatusScheduledMargin,
	}
}

// scheduledWork reads the server-side work in one facts row that can
// create a turn on the session with no new input and has neither created
// it nor declined yet (technical plan §43.20's inventory), and returns
// whether any is armed and the earliest instant one comes due:
//
//   - every armed session timer sessionactor.TimerCountsAsScheduledWork
//     says can still create a turn -- a kind it does not know included, so
//     an unclassified kind reads as work, never as settled; the re-review
//     debounce only while its repository opted in and its budget is not
//     spent (review_retrigger_can_fire, the same snapshot) -- due at its
//     fires_at (which the timer pump pushes forward while it delivers it);
//   - a release manifest check not yet claimed (release_manifest_pending),
//     due at the worker's next tick after it was enqueued;
//   - a release manifest check running (release_manifest_checks_running)
//     and claimed within releaseCheckWindow of the snapshot, due when its
//     worker's deadline ends it.
//
// Both instants in every comparison are the database's clock.
func scheduledWork(facts sqlcgen.GetSessionActivityFactsRow, bounds statusBounds) (armed bool, dueAt time.Time, err error) {
	if len(facts.ArmedTimerNames) != len(facts.ArmedTimerFiresAt) {
		return false, time.Time{}, fmt.Errorf("armed timers: %d names for %d instants", len(facts.ArmedTimerNames), len(facts.ArmedTimerFiresAt))
	}
	consider := func(at time.Time) {
		if !armed || at.Before(dueAt) {
			dueAt = at
		}
		armed = true
	}
	for i, name := range facts.ArmedTimerNames {
		if sessionactor.TimerCountsAsScheduledWork(name, facts.ReviewRetriggerCanFire) {
			consider(facts.ArmedTimerFiresAt[i].Time)
		}
	}
	if facts.ReleaseCheckPendingSince.Valid {
		consider(facts.ReleaseCheckPendingSince.Time.Add(bounds.releaseCheckPumpInterval))
	}
	if facts.ReleaseCheckClaimedAt.Valid && session.ClaimedWorkOpen(facts.ReleaseCheckClaimedAt.Time, facts.ObservedAt.Time, bounds.releaseCheckWindow) {
		consider(facts.ReleaseCheckClaimedAt.Time.Add(bounds.releaseCheckTimeout))
	}
	return armed, dueAt, nil
}

// activityInput is session.ActivityInput from one facts row: the turn
// histogram as the snapshot counted it -- every state it holds, one this
// code does not know included, so DeriveActivity can count that one as in
// flight -- each human gate as "an id was found", a completed turn's
// push/PR delivery as under way while its stamp is within the delivery
// window of the snapshot's own instant (session.PRDeliveryOpen; both
// instants are the database's clock), and the work armed to create a turn
// as scheduledWork reads it. It also returns how long until that work
// comes due, measured from the snapshot. sessions.status is not an input.
func activityInput(facts sqlcgen.GetSessionActivityFactsRow, bounds statusBounds) (session.ActivityInput, time.Duration, error) {
	var raw map[string]int64
	if err := json.Unmarshal(facts.TurnCounts, &raw); err != nil {
		return session.ActivityInput{}, 0, fmt.Errorf("decode turn counts %q: %w", facts.TurnCounts, err)
	}
	scheduled, dueAt, err := scheduledWork(facts, bounds)
	if err != nil {
		return session.ActivityInput{}, 0, err
	}
	var dueIn time.Duration
	if scheduled {
		dueIn = dueAt.Sub(facts.ObservedAt.Time)
	}
	counts := make(map[turn.State]int, len(raw))
	for state, n := range raw {
		counts[turn.State(state)] = int(n)
	}
	var deliveryStartedAt time.Time
	if facts.PrDeliveryStartedAt.Valid {
		deliveryStartedAt = facts.PrDeliveryStartedAt.Time
	}
	return session.ActivityInput{
		TurnCounts:                   counts,
		PlanAwaitingApproval:         facts.AwaitingPlanID.Valid,
		WorkflowStepAwaitingDecision: facts.AwaitingStepID.Valid,
		WorkflowEscalationOpen:       facts.EscalatedRunID.Valid,
		PRDeliveryInProgress:         session.PRDeliveryOpen(deliveryStartedAt, facts.ObservedAt.Time, bounds.deliveryWindow),
		ScheduledWork:                scheduled,
	}, dueIn, nil
}

// sessionActivityToDTO derives the activity and the suggested delay from
// one facts row and renders restdtos.SessionActivity.
func sessionActivityToDTO(facts sqlcgen.GetSessionActivityFactsRow, delays session.DelayTable, bounds statusBounds) (restdtos.SessionActivity, error) {
	in, scheduledDueIn, err := activityInput(facts, bounds)
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
	delay := session.SuggestedReadDelay(activity, sandboxState, scheduledDueIn, delays)

	return restdtos.SessionActivity{
		SessionId:             facts.SessionID.String(),
		Activity:              restdtos.SessionActivityActivity(activity),
		Settled:               activity.Settled(),
		PendingTurns:          in.TurnCounts[turn.StatePending],
		InFlightTurn:          inFlightTurnDTO(facts),
		Awaiting:              awaitingDTO(facts),
		Escalation:            escalationDTO(facts),
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

// escalationDTO reports the session's open workflow escalation whatever
// awaitingDTO reports first: the same facts row field, which carries a run
// only while its escalation is still open (GetSessionActivityFacts'
// escalated lookup). awaiting names one gate, so an escalation open beside
// a plan or a workflow step reaches a client only here -- the web's run
// view features it by this field (technical plan §43.20).
func escalationDTO(facts sqlcgen.GetSessionActivityFactsRow) *restdtos.SessionActivityEscalation {
	if !facts.EscalatedRunID.Valid {
		return nil
	}
	return &restdtos.SessionActivityEscalation{
		Id:    facts.EscalatedRunID.String(),
		Since: facts.EscalatedRunSince.Time,
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

// lastRunFailureReason is SessionActivity.lastRun.failureReason:
// lastRunFailureReasonValue, on the wire.
func lastRunFailureReason(facts sqlcgen.GetSessionActivityFactsRow) *restdtos.SessionActivityLastRunFailureReason {
	reason := lastRunFailureReasonValue(facts)
	if reason == nil {
		return nil
	}
	return &restdtos.SessionActivityLastRunFailureReason{Value: *reason}
}

// lastRunFailureReasonValue is sessions.failure_reason, and only when it
// can describe nothing but the last run: a turn row has no reason column,
// and the session's reason is that of whichever turn its status was last
// derived from. So it is given only when the last run is the session's
// newest turn, did not complete, and the session's recorded outcome is
// that very outcome (failed or cancelled); nil otherwise. The one rule for
// both the status's lastRun and the result's (sessionresult.go).
func lastRunFailureReasonValue(facts sqlcgen.GetSessionActivityFactsRow) *string {
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
	reason := string(*facts.SessionFailureReason)
	return &reason
}

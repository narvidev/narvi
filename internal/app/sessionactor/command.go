package sessionactor

import "encoding/json"

// The 5 named persistent timers §2 lists explicitly: "connecting_deadline,
// liveness_check, inactivity, turn_deadline, terminal_grace." Named
// constants here (rather than bare string literals scattered across the
// package) so a typo in a timer name is a compile error at every call
// site that matters, even though the session_timers.name column itself is
// TEXT, not an enum (see migrations/000009_session_timers.up.sql).
const (
	TimerConnectingDeadline = "connecting_deadline"
	TimerLivenessCheck      = "liveness_check"
	TimerInactivity         = "inactivity"
	TimerTurnDeadline       = "turn_deadline"
	TimerTerminalGrace      = "terminal_grace"

	// TimerReviewRetriggerDebounce is §24's own addition ("review:
	// automatic re-review on new commits", §24.2) -- the ONE named timer
	// this whole feature is built on. Unlike the 5 timers above, this one
	// is armed/re-armed from OUTSIDE the actor entirely: internal/
	// adapters/inbound/github/pullrequestsynchronize.go writes directly
	// via postgres.TimerStore.Upsert (bypassing the actor's mailbox,
	// §24.1's 4th cost item, mirroring how coalesce.go already writes
	// github_pr_sessions directly today) on every `pull_request`/
	// `synchronize` webhook event -- only the FIRING travels through the
	// ordinary timer pump into TimerFired, exactly like every other named
	// timer.
	TimerReviewRetriggerDebounce = "review_retrigger_debounce"

	// TimerStop is technical plan §3.3's stop: a person's request to stop
	// the session, armed from OUTSIDE the actor like the debounce above --
	// POST /api/sessions/{sessionID}/stop (internal/adapters/inbound/
	// httpapi's StopSession) upserts it to now, in the transaction that
	// flags the session's open turns (turns.stop_requested_at) and the
	// session. Its firing (stop.go's handleStopTimer) cancels every flagged
	// pending turn, sends a flagged turn in flight the sandbox `stop`
	// command and re-arms itself for the end of that turn's StopGrace, and
	// cancels the turn itself if it is still in flight then, retiring its
	// sandbox gen -- re-arming again while that gen still delivers a
	// completed turn's push and pull request, which the retirement waits
	// for. The timer is
	// what makes the request survive the loss of a replica: the flags and
	// the timer are rows, and whichever replica's pump delivers it next
	// does the rest.
	TimerStop = "stop"

	// TimerDispatch makes a turn's dispatch trigger durable (technical plan
	// §2, §3.3). It is armed from OUTSIDE the actor, due at once, in the
	// transaction that creates a turn -- postgres.TurnStore.
	// CreateAndArmDispatch is the one way production code creates one, and
	// its query (ArmSessionDispatchTimer, which the actor's own re-arm
	// shares, TimerStore.ArmDispatch) names this kind -- so a turn
	// committed on a session always has either a dispatch evaluation still
	// to come or this timer. The post-commit EnsureDispatched every
	// turn-creating path sends is the fast path; when it fails
	// (actor_unavailable, actor_elsewhere, a stopped actor, or a replica
	// that dies between its commit and the trigger) the timer pump delivers
	// the evaluation on whichever replica claims the timer, and again after
	// each claim window. Its handler is EnsureDispatched's
	// (handleEnsureDispatched), and every dispatch evaluation deletes it
	// first, in its own transaction (planDispatch), whatever asked for the
	// evaluation -- so after a trigger that succeeded the row is gone within
	// that evaluation, and a firing that arrives later finds nothing to do.
	TimerDispatch = "dispatch"

	// TimerOwedReviewRequest is technical plan §24.9's owed human review
	// request: a person's review attempt (the label, the button)
	// that waited behind another turn and met a moved context when it was
	// dispatched ends context_moved, and its request becomes a row of
	// owed_review_requests (migrations/000160). The dispatching transaction
	// that ends the attempt arms this timer due at once on the database's
	// clock (TimerStore.ArmOwedReviewRequest, whose query names this kind),
	// so the request survives a restart between the move and its re-run.
	// Its firing (owedreviewrequest.go's handleOwedReviewRequestTimer)
	// serves the session's oldest owed request: it reads the pull request
	// again, composes the prompt for the head it has now, asks the
	// requester's authorization again, and inserts the re-run turn -- or
	// drops the request and tells its requester -- deleting the row in the
	// same transaction, then re-arms itself while more are owed and
	// deletes itself otherwise.
	TimerOwedReviewRequest = "owed_review_request"
)

// Command is the sum type an Actor's mailbox carries (§2: "one goroutine
// + mailbox (channel of commands) per active session"). isCommand is
// unexported so no type outside this package can implement Command --
// handle's type switch in timerfired.go can therefore treat its default
// case as unreachable dead-code protection, not a real possibility to
// handle.
type Command interface {
	isCommand()
}

// TimerFired is delivered by the timer pump (timerpump.go) when a named
// persistent timer becomes due (§2) -- and, for TimerStop only, also sent
// by the stop route right after it arms that timer, so the actor acts at
// once rather than at the pump's next tick (every handler already tolerates
// a redelivered firing, since the pump's claim window can redeliver one). Name is one of the 5 constants above
// in practice, but this type does not itself restrict it -- an unknown
// name is handled defensively (kept, backed off or deleted by its row's
// age, handleUnknownTimer) by the dispatch in timerfired.go, the same
// deny-list-not-allow-list convention
// internal/domain/sandbox.IsDeadSandboxStatus and internal/domain/turn.
// IsTerminal already use.
type TimerFired struct {
	Name string
}

func (TimerFired) isCommand() {}

// SandboxEvent is delivered by internal/adapters/inbound/wshub's sandbox
// WS read loop (§3.2) for every inbound frame on a sandbox's live
// connection, once that connection's own handshake-time gen has already
// been validated (§6.1: "403 on id/gen mismatch" is enforced at connect
// time; this per-message Gen is the SEPARATE per-message half of §3.2's
// gen-fencing rule -- "stale-gen inputs are rejected and logged" -- the
// same two-layer defense commands.schema.json's own doc comment already
// requires of wsbridge's client-side dispatch, see
// internal/sandboxagent/wsbridge/dispatch.go's checkGen).
//
//   - Type is the wire message's own "type" field (e.g. "ready",
//     "heartbeat", "execution_complete", ...) -- handleSandboxEvent
//     (sandboxevent.go) switches on it for the two in-scope state
//     transitions; every other recognized type is still persisted, just
//     with no transition attempted.
//   - Gen is the wire message's own "gen" field, checked against the
//     sandbox row's CURRENT gen inside handleSandboxEvent's own
//     transaction (never against the connection-level gen the handshake
//     already validated, which may be stale by the time this particular
//     message is actually processed).
//   - MessageID is the wire message's own top-level "messageId" field --
//     every one of the 20 sandbox-ws event types requires one
//     (contracts/sandbox-ws/v1/events.schema.json) -- carried through so
//     appendRawEvent (actor.go) can dedupe a resend of an already-
//     persisted event by upsert-on-(session_id, messageId) rather than
//     appending an indistinguishable duplicate row (§6.1).
//   - Raw is the original, unmodified wire bytes -- persisted verbatim via
//     appendRawEvent (actor.go) so the append-only event log holds exactly
//     what the sandbox sent, not a lossy re-encoding through an
//     intermediate Go struct.
//   - LastBootPhase mirrors sandboxws.Heartbeat.LastBootPhase for a
//     "heartbeat" event (nil/absent for every other type) -- nil
//     specifically means "boot has completed", the signal
//     handleSandboxEvent's Booting->Ready transition watches for.
//   - Reply is how handleSandboxEvent's asynchronous mailbox-processing
//     communicates the outcome back to the wshub read-loop goroutine, which
//     owns the live *websocket.Conn and must write any resulting ack
//     itself. The caller (wshub) constructs it as
//     `make(chan SandboxEventOutcome, 1)` -- buffered so
//     handleSandboxEvent's own send into it (a non-blocking
//     select-with-default) always succeeds immediately, even if the wshub
//     side already gave up waiting on platform.Timeouts.
//     SandboxEventAckTimeout.
type SandboxEvent struct {
	Type          string
	Gen           int
	MessageID     string
	Raw           json.RawMessage
	LastBootPhase *string
	// ConversationID mirrors sandboxws.Heartbeat.ConversationId for a
	// "heartbeat" event (nil/absent for every other type, and nil for a
	// heartbeat that has nothing new to report -- HeartbeatConversationId
	// is itself a *string, so "absent" and "explicit null" both decode to
	// nil here, matching the wire schema's own documented equivalence).
	// handleSandboxEvent (sandboxevent.go) persists a non-nil value to
	// sessions.opencode_conversation_id (§3.3, design decision 6 --
	// migrations/000018_session_repos.up.sql).
	ConversationID *string
	// AgentVersion/ImageDigest (§12.2 item 1's own runtime-fingerprint
	// gap) mirror ConversationID's own shape exactly, for the "ready"
	// event instead of "heartbeat": nil/absent for every other type.
	// handleSandboxEvent persists a non-nil pair to sandboxes.
	// agent_version/image_digest (migrations/000120_sandboxes_boot_fingerprint.
	// up.sql).
	AgentVersion *string
	ImageDigest  *string
	// CallID/StepID are the wire message's own "callId" (a `tool_call` or
	// `tool_result`) and "stepId" (a `step_start` or `step_finish`), "" on
	// every other type: the correlators eventkey.StorageKey derives the
	// storage key of a `tool_call`, `tool_result` or `step_finish` from
	// (technical plan §6.1, toolevent.go). Every one of those events
	// carries its enclosing message's id as its messageId, so without them
	// it would dedupe onto that message's `step_start` row.
	CallID string
	StepID string
	Reply  chan<- SandboxEventOutcome
}

func (SandboxEvent) isCommand() {}

// EnsureDispatched is a fire-and-forget "please re-evaluate this
// session's own spawn/dispatch state right now" signal (§9.3, "e2e
// happy path", design decision 3) -- no payload, mirroring TimerFired's
// own zero-payload shape. Sent (via Actor.Send, from OUTSIDE the actor's
// own goroutine) from exactly three places: (a) httpapi.CreateSession,
// right after a turn is created; (b) httpapi.CreateTurn (§3.3, "turn
// recovery"), right after a NEW turn is created on an existing session,
// the same way; (c) this package's own handleSandboxEvent, unconditionally,
// right after its own transact commits successfully (so a heartbeat-driven
// transition to Booting/Ready is immediately followed by a fresh dispatch
// evaluation). handleEnsureDispatched (dispatch.go) is its handler --
// timerfired.go's own handleTerminalGraceTimer also reaches that SAME
// handler, but by calling it DIRECTLY (not via Send), since it already
// runs on the actor's own single command-processing goroutine.
type EnsureDispatched struct{}

func (EnsureDispatched) isCommand() {}

// SandboxEventOutcome is what handleSandboxEvent (sandboxevent.go) sends
// back on SandboxEvent.Reply once its own transaction has committed (or
// deliberately skipped persisting a stale-gen event, or a frame typed as
// one of the control plane's own events -- serverEventTypes, see
// sandboxevent.go).
//
//   - Persisted reports whether the event was actually appended to the
//     events table (false only for those two rejection paths).
//   - AckID is the original critical event's own deterministic ackId
//     (contracts/gen/go/sandboxws's own "{type}:{messageId}" convention),
//     non-empty only when the inbound wire message was one of the 6
//     critical types AND it was persisted. wshub echoes this back as a
//     fresh sandboxws.Ack{AckId: outcome.AckID, ...} on the same
//     connection when non-empty; an empty AckID means "do not send an
//     ack for this message" (either it was never a critical type, or it
//     was rejected by one of those two paths and never persisted at all).
type SandboxEventOutcome struct {
	Persisted bool
	AckID     string
}

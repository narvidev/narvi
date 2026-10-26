// This file (continuity.go) is the session actor's half of technical plan
// §35.5's fresh lineage: a sandbox spawned fresh in place of an earlier gen
// of a session that had a turn dispatched to one. The repository is cloned
// again and the agent runtime opens an empty conversation, so the agent has
// neither its context nor any work that was not pushed. The decision is the
// pure sandbox.DecideFreshLineage; the recap is the pure continuity.Build;
// this file reads what they decide on and writes what they decide.
//
// # The claim
//
// Every sandbox the control plane creates is claimed by tryPlanSpawn, so a
// fresh lineage is decided there, once, in the claim's transaction, by
// decideFreshLineage on the row the claim replaces as it was before the
// claim's upsert. Every fresh spawn also forgets the session's
// agent-runtime conversation id: the new box holds no runtime
// conversation, so its first prompt names none, and the agent's heartbeat
// records the one it opens. A restore or resume keeps the id, and is never
// marked. A claim that starts a fresh lineage marks its gen
// (sandboxes.fresh_lineage_gen); every claim's upsert clears the mark, so it
// counts only for the gen it names, and a later fresh claim that replaces a
// gen whose mark is still pending carries it over.
//
// # The recap
//
// The first prompt sent to a marked gen -- a pending turn's dispatch
// (tryPlanDispatch) or an in-flight turn's re-send (tryPlanReenqueue) --
// opens with a recap of the session's earlier turns (attachContinuityRecap):
// each turn's request and its final text, as ReadWindowFinal reads it over
// the turn's window of the log (TurnContentBounds) -- the one reader the plan
// notices and the session result use -- and, for a re-send, what the turn's
// own attempt on the lost gen had reported. continuity.Build frames it as a
// third party's report for the agent to check against the repository. It is
// held to the room the gen's prompt bound leaves, at most
// platform.ContinuityRecapMaxBytes, and never refuses a turn: when not even
// its frame fits, the turn goes without it and the mark stays for the next
// turn. It is stored on the turn with the gen it was built for, so a receipt
// re-send to that gen sends the same bytes (BuildPromptPayload), and a
// prompt to any other gen carries none of it.
//
// # The mark is cleared on reach
//
// Only evidence the control plane stores that the recap's prompt reached
// the gen clears the mark (clearFreshLineageOnReach, in handleSandboxEvent's
// transaction): the gen's prompt_received for the recap turn's prompt, or
// an event of the turn the prompt opened while that turn is processing on
// the gen -- never the dispatch's commit, nor a send that returned. A send
// that fails, a stop, a refusal or a deadline with no such evidence leave
// it, and the next turn sent to the gen carries a recap of its own.
//
// Nothing here waits: no state, edge, timer or timeout is added. The notice
// that tells a person the context was lost is not written here.

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/continuity"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// freshLineagePending reports whether row's live gen is a fresh lineage
// whose recap has not yet reached it: its mark names it.
func freshLineagePending(row sqlcgen.Sandbox) bool {
	return row.FreshLineageGen != nil && *row.FreshLineageGen == row.Gen
}

// freshLineageNote is what a fresh-spawn claim decided about the fresh
// lineage it may start, logged and counted once the claim has committed
// (logFreshLineage).
type freshLineageNote struct {
	decision sandbox.LineageDecision
	// gen is the gen the claim created.
	gen int
	// replacedGen and replacedStatus are the row the claim replaced, as it
	// was before the claim's upsert: 0 and "" for a session's first gen.
	replacedGen    int32
	replacedStatus string
	// snapshotID is the snapshot the replaced row recorded, "" for none.
	snapshotID string
	// dockerRequired is whether the session's environment runs a container
	// daemon, which is never snapshotted (§27.8).
	dockerRequired bool
	// conversationCleared is whether the claim forgot a recorded
	// agent-runtime conversation id.
	conversationCleared bool
}

// lineageRefusal is the restore a fresh-spawn claim refused, as
// sandbox.DecideFreshLineage reads it: one whose snapshot is cleared (its
// provenance, the repository-hooks boundary), or kept (§27.8, §30.4(3)).
func lineageRefusal(refusal *snapshotRefusal) sandbox.LineageRefusal {
	switch {
	case refusal == nil:
		return sandbox.LineageRefusalNone
	case refusal.clear:
		return sandbox.LineageRefusalCleared
	default:
		return sandbox.LineageRefusalKept
	}
}

// replacedLive reports whether a gen at status was still spawning,
// connecting, booting or ready when a fresh claim replaced it: given up on
// as stuck (EvaluateSpawnDecision's recovery carve-outs).
func replacedLive(status sandbox.State) bool {
	switch status {
	case sandbox.StateSpawning, sandbox.StateConnecting, sandbox.StateBooting, sandbox.StateReady:
		return true
	}
	return false
}

// decideFreshLineage settles, in the fresh-spawn claim's own transaction,
// what technical plan §35.5 asks of it: the session's agent-runtime
// conversation id is forgotten when the claim replaces an existing row, and
// sandbox.DecideFreshLineage decides, on sandboxRow -- the row the claim
// replaced, as it was before the claim's upsert -- whether newGen starts a
// fresh lineage, which is marked when it does. refusal is the restore the
// claim refused, wasRotating whether the replaced gen was rotating, and
// dockerRequired whether the session's environment runs a container daemon.
func (a *Actor) decideFreshLineage(
	ctx context.Context, tx pgx.Tx,
	sandboxRow sqlcgen.Sandbox, hasSandbox bool, newGen int,
	refusal *snapshotRefusal, wasRotating, dockerRequired bool,
) (*freshLineageNote, error) {
	note := &freshLineageNote{gen: newGen, dockerRequired: dockerRequired}
	facts := sandbox.LineageFacts{
		Claim:    sandbox.LineageClaimSpawn,
		FirstGen: !hasSandbox || sandbox.State(sandboxRow.Status) == sandbox.StatePending,
		Refusal:  lineageRefusal(refusal),
		Rotating: wasRotating,
	}
	if hasSandbox {
		note.replacedGen, note.replacedStatus = sandboxRow.Gen, string(sandboxRow.Status)
		note.snapshotID = stringOrEmpty(sandboxRow.SnapshotID)
		facts.ReplacedMarkPending = freshLineagePending(sandboxRow)
		facts.ReplacedLive = replacedLive(sandbox.State(sandboxRow.Status))

		cleared, err := a.stores.session.WithTx(tx).ClearConversationID(ctx, a.sessionID)
		if err != nil {
			return nil, fmt.Errorf("sessionactor: forget the conversation id on a fresh spawn: %w", err)
		}
		note.conversationCleared = cleared
	}
	if !facts.FirstGen {
		dispatched, err := a.stores.turn.WithTx(tx).HasTurnDispatchedBeforeGen(ctx, a.sessionID, int32(newGen))
		if err != nil {
			return nil, fmt.Errorf("sessionactor: read whether a turn was dispatched before the fresh spawn: %w", err)
		}
		facts.PriorTurnDispatched = dispatched
	}
	note.decision = sandbox.DecideFreshLineage(facts)
	if note.decision.Mark {
		if err := a.stores.sandbox.WithTx(tx).MarkFreshLineage(ctx, a.sessionID, int32(newGen)); err != nil {
			return nil, fmt.Errorf("sessionactor: mark the fresh lineage: %w", err)
		}
	}
	return note, nil
}

// freshLineageLogMessage and freshLineageCarriedLogMessage are
// logFreshLineage's messages.
const (
	freshLineageLogMessage        = "sessionactor: a fresh lineage: the sandbox was spawned fresh in place of an earlier gen, without the agent's conversation or any work that was not pushed; its first turn carries a recap"
	freshLineageCarriedLogMessage = "sessionactor: a fresh lineage carried over: the gen it replaces never received its recap, so this one owes it"
)

// logFreshLineage logs and counts what plan's claim decided about the fresh
// lineage, after the claim committed (executePlans), never inside it: a
// marked gen at WARN, or at INFO when its mark was carried over, and on
// sandbox_fresh_lineage_total. A claim that marked nothing -- a restore, a
// resume, a session's first spawn, one before any turn was dispatched --
// records nothing.
func (a *Actor) logFreshLineage(ctx context.Context, plan *spawnPlan) {
	n := plan.lineage
	if n == nil || !n.decision.Mark {
		return
	}
	fields := []any{
		"session_id", a.sessionID.String(), "gen", n.gen, "cause", string(n.decision.Cause), "carried", n.decision.Carried,
		"replaced_gen", n.replacedGen, "replaced_status", n.replacedStatus, "snapshot_id", n.snapshotID,
		"docker_required", n.dockerRequired, "conversation_cleared", n.conversationCleared,
	}
	if n.decision.Carried {
		a.logger.Info(freshLineageCarriedLogMessage, fields...)
	} else {
		a.logger.Warn(freshLineageLogMessage, fields...)
	}
	if a.opsMetrics.freshLineage == nil {
		return
	}
	a.opsMetrics.freshLineage.Add(ctx, 1, metric.WithAttributes(
		attribute.String("cause", string(n.decision.Cause)),
		attribute.Bool("carried", n.decision.Carried),
	))
}

// The outcome attribute of turn_continuity_recap_total (opsmetrics.go).
const (
	continuityRecapOutcomeAttached  = "attached"
	continuityRecapOutcomeTrimmed   = "trimmed"
	continuityRecapOutcomeNoRoom    = "no_room"
	continuityRecapOutcomeDelivered = "delivered"
)

// recapNote is what a dispatch to a fresh lineage did about its recap,
// logged and counted once the dispatch has committed (logContinuityRecap).
type recapNote struct {
	outcome string
	turnID  pgtype.UUID
	gen     int32
	// budget is the room the recap had, in JSON-encoded bytes.
	budget int
	// bytes is the recap's own size, JSON-encoded; 0 with no_room.
	bytes int
	// turns and omitted are the earlier turns it reports and leaves out.
	turns, omitted int
	// interrupted is whether it was for a re-send of a turn in flight.
	interrupted bool
}

// promptBuilder builds the prompt of a turn as it stands, for the dispatch
// that sends it.
type promptBuilder func(target sqlcgen.Turn) (json.RawMessage, error)

// attachContinuityRecap opens target's prompt to sandboxRow's gen with the
// recap of the session's earlier turns when that gen is a fresh lineage
// whose recap is still owed (technical plan §35.5), and returns the payload
// to send and what it did; payload, nil otherwise. payload is target's
// prompt built without a recap, and build builds it again for target as it
// stands. interrupted is true for a re-send of a turn in flight on a lost
// gen, whose own attempt there the recap reports too.
//
// The recap is held to the room the gen's bound leaves the payload, at most
// platform.ContinuityRecapMaxBytes (continuity.Build), so it can never push
// the prompt over the bound executeDispatch refuses at. When not even its
// frame fits, nothing is attached, and the mark stays for the next turn.
// Otherwise it is stored on the turn with the gen, in this transaction
// (SetTurnContinuityRecap), and the payload rebuilt from the turn as
// stored, so a receipt re-send rebuilt from the row sends the same bytes.
func (a *Actor) attachContinuityRecap(
	ctx context.Context, tx pgx.Tx,
	sandboxRow sqlcgen.Sandbox, turns []sqlcgen.Turn, target sqlcgen.Turn, interrupted bool,
	payload json.RawMessage, build promptBuilder,
) (json.RawMessage, *recapNote, error) {
	if !freshLineagePending(sandboxRow) {
		return payload, nil, nil
	}
	room := promptFrameBound(sandboxRow) - len(payload) - continuity.EncodedLen(continuity.Separator)
	note := &recapNote{
		turnID: target.ID, gen: sandboxRow.Gen, interrupted: interrupted,
		budget: min(platform.ContinuityRecapMaxBytes, room),
	}
	in, err := a.recapTurns(ctx, tx, turns, target, interrupted, note.budget)
	if err != nil {
		return nil, nil, err
	}
	recap, ok := continuity.Build(in, note.budget)
	if !ok {
		note.outcome = continuityRecapOutcomeNoRoom
		return payload, note, nil
	}
	if err := a.stores.turn.WithTx(tx).SetContinuityRecap(ctx, target.ID, recap.Text, sandboxRow.Gen); err != nil {
		return nil, nil, fmt.Errorf("sessionactor: store the continuity recap: %w", err)
	}
	gen := sandboxRow.Gen
	target.ContinuityRecap, target.ContinuityRecapGen = &recap.Text, &gen
	if payload, err = build(target); err != nil {
		return nil, nil, err
	}
	note.outcome = continuityRecapOutcomeAttached
	if recap.Trimmed {
		note.outcome = continuityRecapOutcomeTrimmed
	}
	note.bytes, note.turns, note.omitted = continuity.EncodedLen(recap.Text), recap.Turns, recap.Omitted
	return payload, note, nil
}

// recapTurns reads what the recap of target's prompt reports: every turn of
// turns dispatched before it, in dispatch order (TurnContentBounds' order),
// counted; the newest of them, MaxTurns at most, each with its request and
// its final text as ReadWindowFinal reads it over the turn's window
// (TurnContentBounds over turns as loaded); and, when interrupted, what
// target's own attempt on the lost gen reported -- its window from its
// dispatched_event_id as loaded, before the re-send moved it, to the log's
// end. The turns are read newest first, and the reads stop once a recap of
// what was read, within budget, leaves the oldest read turn out: no older
// one would be kept either. So at most the turns that fit and one more are
// read, each held to planContentEventFetchLimit frames.
func (a *Actor) recapTurns(
	ctx context.Context, tx pgx.Tx,
	turns []sqlcgen.Turn, target sqlcgen.Turn, interrupted bool, budget int,
) (continuity.Input, error) {
	events := a.stores.event.WithTx(tx)
	var earlier []sqlcgen.Turn
	for _, t := range turns {
		if t.DispatchedEventID != nil && t.ID != target.ID {
			earlier = append(earlier, t)
		}
	}
	sort.SliceStable(earlier, func(i, j int) bool { return *earlier[i].DispatchedEventID < *earlier[j].DispatchedEventID })

	in := continuity.Input{Earlier: len(earlier)}
	if interrupted {
		final, found, err := ReadWindowFinal(ctx, events, a.sessionID, target.DispatchedEventID, nil)
		if err != nil {
			return continuity.Input{}, err
		}
		in.Interrupted = &continuity.Attempt{Report: final, Reported: found}
	}
	var newestFirst []continuity.Turn
	for i := len(earlier) - 1; i >= 0 && len(newestFirst) < continuity.MaxTurns; i-- {
		t := earlier[i]
		lower, upper, _ := TurnContentBounds(turns, t.ID)
		final, found, err := ReadWindowFinal(ctx, events, a.sessionID, lower, upper)
		if err != nil {
			return continuity.Input{}, err
		}
		newestFirst = append(newestFirst, continuity.Turn{
			Number: i + 1, Status: turn.State(t.Status), EndReason: stringOrEmpty(t.EndReason),
			Request: stringOrEmpty(t.Prompt), Report: final, Reported: found,
		})
		in.Turns = oldestFirst(newestFirst)
		if recap, ok := continuity.Build(in, budget); !ok || recap.Turns < len(newestFirst) {
			break
		}
	}
	return in, nil
}

// oldestFirst is turns, read newest first, in the order a recap takes them.
func oldestFirst(turns []continuity.Turn) []continuity.Turn {
	out := make([]continuity.Turn, len(turns))
	for i, t := range turns {
		out[len(turns)-1-i] = t
	}
	return out
}

// The log messages of the recap's dispatch and of its arrival.
const (
	continuityRecapAttachedLogMessage  = "sessionactor: the first turn sent to a fresh lineage carries a recap of the session's earlier turns"
	continuityRecapNoRoomLogMessage    = "sessionactor: no room for the continuity recap within the gen's prompt bound; the turn is sent without it, and the next turn carries it"
	continuityRecapDeliveredLogMessage = "sessionactor: the continuity recap reached its sandbox; the fresh-lineage mark is cleared"
)

// logContinuityRecap logs and counts what a dispatch did about its gen's
// recap, after the dispatch committed (executePlans): attached or trimmed
// at INFO, no_room at WARN, each on turn_continuity_recap_total. nil
// records nothing.
func (a *Actor) logContinuityRecap(ctx context.Context, n *recapNote) {
	if n == nil {
		return
	}
	fields := []any{
		"session_id", a.sessionID.String(), "turn_id", n.turnID.String(), "gen", n.gen, "outcome", n.outcome,
		"budget_bytes", n.budget, "recap_bytes", n.bytes, "turns", n.turns, "omitted", n.omitted, "interrupted", n.interrupted,
	}
	if n.outcome == continuityRecapOutcomeNoRoom {
		a.logger.Warn(continuityRecapNoRoomLogMessage, fields...)
	} else {
		a.logger.Info(continuityRecapAttachedLogMessage, fields...)
	}
	a.recordContinuityRecap(ctx, n.outcome)
}

// recordContinuityRecap counts one turn_continuity_recap_total outcome.
func (a *Actor) recordContinuityRecap(ctx context.Context, outcome string) {
	if a.opsMetrics.continuityRecap == nil {
		return
	}
	a.opsMetrics.continuityRecap.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// promptReachEvidence reports whether cmd, an event of the live gen, is
// evidence that a prompt reached it (technical plan §35.5), and for a
// prompt_received, the prompt it is for. The evidence is a prompt_received
// that names its prompt, or an event of a turn the agent is running -- the
// step, text, tool and sub-task frames, and the agent's own
// execution_complete, which handleSandboxEvent only ever receives from the
// sandbox. A frame of the boot or of the connection -- ready, heartbeat,
// boot_progress, git_sync, boot_timing, a warning -- is not.
func promptReachEvidence(cmd SandboxEvent) (evidence bool, promptMessageID *string) {
	switch cmd.Type {
	case "prompt_received":
		// Only the prompt it names is read, as the receipt's own key is
		// (promptreceipt.go): the rest of the frame decides nothing here.
		var evt struct {
			PromptMessageID string `json:"promptMessageId"`
		}
		if err := json.Unmarshal(cmd.Raw, &evt); err != nil || evt.PromptMessageID == "" {
			return false, nil
		}
		return true, &evt.PromptMessageID
	case "step_start", "token", "tool_call", "tool_result", "step_finish", "sub_task_start", "sub_task_finish", "execution_complete":
		return true, nil
	}
	return false, nil
}

// clearFreshLineageOnReach clears row's mark, in the transaction that
// stores cmd, when cmd is evidence that the prompt carrying the gen's recap
// reached it (promptReachEvidence, ClearSandboxFreshLineageOnReach): only
// while the mark counts for row's live gen, and only for the turn that
// carries a recap built for that gen. Not gated on the event being stored
// for the first time: a replayed frame is evidence too. Reports whether
// it cleared the mark.
func (a *Actor) clearFreshLineageOnReach(ctx context.Context, tx pgx.Tx, row sqlcgen.Sandbox, cmd SandboxEvent) (bool, error) {
	if !freshLineagePending(row) {
		return false, nil
	}
	evidence, promptMessageID := promptReachEvidence(cmd)
	if !evidence {
		return false, nil
	}
	n, err := a.stores.sandbox.WithTx(tx).ClearFreshLineageOnReach(ctx, a.sessionID, row.Gen, promptMessageID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: clear the fresh-lineage mark on the recap's arrival: %w", err)
	}
	return n > 0, nil
}

// logContinuityRecapDelivered logs and counts a recap's arrival at gen,
// after the event that showed it committed.
func (a *Actor) logContinuityRecapDelivered(ctx context.Context, gen int32, eventType string) {
	a.logger.Info(continuityRecapDeliveredLogMessage,
		"session_id", a.sessionID.String(), "gen", gen, "event_type", eventType)
	a.recordContinuityRecap(ctx, continuityRecapOutcomeDelivered)
}

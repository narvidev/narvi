package reviewverdict

import (
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
)

// This file implements §26.4's own named residual, closed by §26.4:
// "Corroborating the claim against the persisted sub_task_finish trace is
// what would make the heading 'structural'... until it ships this field is
// trusted, not verified." A schema-required `CounterReview: done`
// self-report (review.CounterReviewStatus, counterreview.go) is
// presence-verified by reviewpost.ValidateVerdictInput and truth-verified
// by nothing at all -- a primary reviewer that never actually dispatched
// the `counter-reviewer` sub-task (§7.1's engine-native fan-out) can still
// self-report "done" and receive CounterReviewFloor's permissive
// ShippableAuto. §26.4's own opening paragraph already establishes that
// the trace to corroborate against exists and is durable: sub_task_finish
// is one of the six ack-guaranteed critical event types
// (ports/agentruntime.go), and sandbox-event persistence is unconditional
// for every recognized type (sessionactor/sandboxevent.go's own
// appendRawEvent, called before the switch on cmd.Type) -- a sub-agent
// that actually ran leaves a durable, queryable trace whether or not the
// primary reviewer's own self-report is honest. CounterReviewCorroborated
// (below) is the ONE pure function that reads that trace back.
//
// # Why this lives here, not in internal/domain/review
//
// Mirrors record.go's own precedent immediately above (a reviewverdict
// type wrapping review.Verdict, rather than a new field ON review.Verdict
// itself): review's own doc.go "zero external imports" convention (this
// package already imports review, see record.go/rollup.go) means review
// itself cannot depend on anything shaped like a persisted event-log
// query result, however loosely typed. This function's own two input
// slices are the SAME "already-fetched, plain typed data, zero I/O"
// discipline driftcanary.go's own FilesChangedDrifted already commits to
// -- the caller (internal/adapters/inbound/httpapi/reviewverdict.go) does
// every Postgres read and JSONB payload decode; this function does one
// thing, a pure comparison, and returns a plain bool.
//
// # Field shapes match the wire, not the DB row
//
// SubTaskStartRecord/SubTaskFinishRecord below deliberately carry only
// the two fields this comparison actually needs from each event's own
// wire shape (sandboxws.SubTaskStart.SubAgentType/SubTaskId and
// sandboxws.SubTaskFinish.SubTaskId/Outcome) -- never a sqlcgen.Event row,
// never raw JSON. The caller decodes each persisted event's own `payload`
// JSONB column into these shapes; this function never sees a database
// type or a byte slice.

// SubTaskStartRecord is the one slice of a persisted sub_task_start
// event's own wire payload (contracts/sandbox-ws/v1/events.schema.json)
// this comparison needs: which sub-task (SubTaskID) was announced as
// which named sub-agent (SubAgentType, §26.4's own new wire field,
// sourced from the task tool's own "subagent_type" dispatch parameter --
// see internal/adapters/outbound/opencode/translate.go's
// taskInputSubAgentType for the extraction, and that field's own doc
// comment for why THIS field, never the freeform Label, is what
// corroboration keys off).
//
// EventID (§26.6's amendment) is the persisted event's own events.id.
// Within one session ids are allocated in commit order (queries/
// events.sql's MaxEventIDForSession), so comparing two records' EventIDs
// orders them as the trace recorded them -- with no clock in it, the same
// reason the corroboration queries bound a turn by dispatched_event_id.
// AdditionsFactCheckInTrace is what reads it.
type SubTaskStartRecord struct {
	EventID      int64
	SubTaskID    string
	SubAgentType string
}

// SubTaskFinishRecord is the one slice of a persisted sub_task_finish
// event's own wire payload this comparison needs: which sub-task
// (SubTaskID, the SAME correlator its own sub_task_start carried) ended
// with which Outcome -- one of sandboxws's own ExecutionCompleteOutcome
// wire strings ("completed"/"failed"/"cancelled", §7.1's own "reuses the
// turn's own outcome taxonomy").
//
// EventID: see SubTaskStartRecord.
type SubTaskFinishRecord struct {
	EventID   int64
	SubTaskID string
	Outcome   string
}

// SubTaskTrace is one turn's sub-task trace as the caller read it (§26.6's
// amendment): the decoded starts and finishes -- bounded below by the
// turn's own dispatch watermark and above by the next turn's -- and
// whether that read covered the whole trace. ReadInFull is false when any
// part of it could not be read -- a failed query, a row whose payload did
// not decode (the caller skips it), a turn with no dispatch scope to read
// it in, one whose watermark another turn shares, or one dispatched after
// an earlier turn on the same gen that may still be running -- and its
// zero value is false, so a trace nobody read is never mistaken for an
// empty one. AdditionsFactCheckInTrace also reads a finish with no start
// in the trace as a trace not read in full.
type SubTaskTrace struct {
	Starts     []SubTaskStartRecord
	Finishes   []SubTaskFinishRecord
	ReadInFull bool
	// CutAtNextTurn is true when the read stopped at the next turn's
	// dispatch watermark: this turn's own events after it, if it kept
	// running, were deliberately left unread, because they cannot be told
	// from the next turn's. What the window holds is still read in full,
	// so a run found inside it counts; a run not found inside it may lie
	// past the cut, so AdditionsFactCheckInTrace reports the trace as not
	// read in full rather than as holding none.
	CutAtNextTurn bool
}

// counterReviewFinishOutcomeCompleted mirrors sandboxws.
// ExecutionCompleteOutcomeCompleted's own wire string value ("completed")
// exactly -- checked against contracts/gen/go/sandboxws/sandboxws.go
// rather than guessed, per this Step's own instruction. Named rather than
// inlined so a future SubTaskFinish outcome-taxonomy change (§7.1's own
// "reuses the turn's own outcome taxonomy" -- a taxonomy that could grow)
// only ever needs updating in one place. Deliberately a plain string, not
// an import of the sandboxws-generated type: this package's own sibling
// files (driftcanary.go, record.go) take plain typed inputs from their
// callers rather than reaching into contracts/gen themselves, and this
// function's own SubTaskFinishRecord.Outcome field follows that same
// discipline -- the caller (httpapi) is what actually touches the wire
// type when decoding a persisted payload.
const counterReviewFinishOutcomeCompleted = "completed"

// CounterReviewCorroborated reports whether starts/finishes -- BOTH
// already scoped by the caller to the SAME session, the SAME sandbox gen
// the turn being verdicted was actually dispatched at, AND an events.id
// window running from that same turn's own dispatch watermark
// (dispatched_event_id) up to the next turn's, when one was dispatched
// after it (see queries/events.sql's own ListSubTaskStartEventsForTurn/
// ListSubTaskFinishEventsForTurn doc comment for why gen-scoping ALONE was
// found insufficient -- a real cross-turn contamination gap caught by
// adversarial review -- and why both bounds are required alongside it,
// not merely session-scoping) -- together contain real, durable
// evidence that the `counter-reviewer` sub-agent (review.
// CounterReviewerAgentName, "counter-reviewer") was both dispatched and
// actually completed.
//
// True iff there exists a starts record whose SubAgentType equals
// review.CounterReviewerAgentName AND whose OWN SubTaskID also appears in
// finishes with Outcome == "completed" -- both halves of that AND
// matched to the SAME subTaskId, never "some counter-reviewer start
// exists somewhere" independently paired with "some completed finish
// exists somewhere". A session's own trace can carry sub-tasks for other
// named sub-agents in the SAME turn (architecture-scribe, fact-check,
// §26.4/§26.6) -- this function must find the RIGHT pair, not merely ANY
// start+completed-finish pair, which is exactly why it correlates by
// SubTaskID rather than counting starts and finishes independently.
//
// false for every other case: no starts at all; a counter-reviewer start
// with no matching finish at all (still "active" from the trace's own
// point of view, or the finish event simply has not landed yet -- see
// this function's own caller in httpapi/reviewverdict.go for the accepted
// race this covers); a matching finish whose Outcome is "failed" or
// "cancelled" rather than "completed"; or a finish that exists for a
// DIFFERENT subTaskId than any counter-reviewer start (never
// cross-matched to a start it does not actually belong to).
//
// Zero I/O, zero time.Now(), zero randomness (CLAUDE.md/§11) -- a pure
// function of its two plain-typed slice arguments, exactly like
// driftcanary.go's own FilesChangedDrifted immediately alongside it in
// this same package.
func CounterReviewCorroborated(starts []SubTaskStartRecord, finishes []SubTaskFinishRecord) bool {
	completedSubTaskIDs := make(map[string]bool, len(finishes))
	for _, f := range finishes {
		if f.Outcome == counterReviewFinishOutcomeCompleted {
			completedSubTaskIDs[f.SubTaskID] = true
		}
	}

	for _, s := range starts {
		if s.SubAgentType != review.CounterReviewerAgentName {
			continue
		}
		if completedSubTaskIDs[s.SubTaskID] {
			return true
		}
	}
	return false
}

// AdditionsFactCheckInTrace (§26.6's amendment) extends the corroboration
// above to the second fact-check run, over what the counter-review added:
// it reports whether trace holds a fact-check sub-task (review.
// FactCheckAgentName) that STARTED AFTER the counter-review and
// completed. That ordering is the whole rule. The first fact-check run
// starts before the counter-review by the funnel's own design (§26.6), so
// "some completed fact-check exists" would always be true on a deep
// review that ran its first pass, and would count every addition as
// checked by a run that never saw it.
//
// "After the counter-review" means after every counter-reviewer event in
// the trace: the start's EventID must exceed the EventID of every
// counter-reviewer start and of every finish belonging to one. A second
// counter-review run (a retry) that started after the fact-check could
// have added findings that fact-check never saw. There must also be a
// counter-review that completed (CounterReviewCorroborated): with none,
// there is no counter-review for a run to come after.
//
// A finish whose start is not in the trace makes the trace incomplete for
// this rule. A sub_task_finish carries no sub-agent type, so only its
// start says whether it was a counter-reviewer's, and a start can be
// missing while its finish is present: sub_task_start travels best-effort
// and can be evicted from the sandbox's send buffer during a long
// disconnect, where sub_task_finish is critical and always delivered. A
// counter-review whose start was lost would raise no bound, and a
// fact-check before it would qualify; so such a trace is read as not read
// in full.
//
// Returns:
//   - reviewpost.AdditionsTraceUnread when !trace.ReadInFull, or when a
//     finish in the trace has no start in it -- even if a qualifying run
//     is among the rows that were read: "checked" is a claim that no later
//     counter-reviewer event exists, which a partial read cannot support.
//     The caller resolves this to unconfirmed, "could not be confirmed",
//     never to "the trace shows none".
//   - reviewpost.AdditionsTraceRunFound when a qualifying run is present.
//     A read cut at the next turn's dispatch (CutAtNextTurn) still finds
//     one inside its window: the window itself was read in full.
//   - reviewpost.AdditionsTraceUnread, too, when no qualifying run is
//     present but the read was cut at the next turn's dispatch: the run
//     may lie past the cut, in events deliberately left unread.
//   - reviewpost.AdditionsTraceNoRunFound otherwise: the trace, read in
//     full, holds no qualifying run -- which includes a run whose finish
//     had not landed when the verdict was posted (§26.4's accepted race),
//     so the caller's text says "not found when the verdict was posted",
//     never "did not run".
//
// Pure: zero I/O, zero time.Now(), like CounterReviewCorroborated.
func AdditionsFactCheckInTrace(trace SubTaskTrace) reviewpost.AdditionsTrace {
	if !trace.ReadInFull || hasFinishWithoutStart(trace) {
		return reviewpost.AdditionsTraceUnread
	}
	if additionsRunInTrace(trace) {
		return reviewpost.AdditionsTraceRunFound
	}
	if trace.CutAtNextTurn {
		return reviewpost.AdditionsTraceUnread
	}
	return reviewpost.AdditionsTraceNoRunFound
}

// additionsRunInTrace is AdditionsFactCheckInTrace's ordering rule over
// the rows that were read: a completed counter-review, and a fact-check
// that started after every counter-reviewer event and completed.
func additionsRunInTrace(trace SubTaskTrace) bool {
	if !CounterReviewCorroborated(trace.Starts, trace.Finishes) {
		return false
	}

	finishesBySubTask := make(map[string][]SubTaskFinishRecord, len(trace.Finishes))
	for _, f := range trace.Finishes {
		finishesBySubTask[f.SubTaskID] = append(finishesBySubTask[f.SubTaskID], f)
	}

	var lastCounterReviewEvent int64
	for _, s := range trace.Starts {
		if s.SubAgentType != review.CounterReviewerAgentName {
			continue
		}
		lastCounterReviewEvent = max(lastCounterReviewEvent, s.EventID)
		for _, f := range finishesBySubTask[s.SubTaskID] {
			lastCounterReviewEvent = max(lastCounterReviewEvent, f.EventID)
		}
	}

	for _, s := range trace.Starts {
		if s.SubAgentType != review.FactCheckAgentName || s.EventID <= lastCounterReviewEvent {
			continue
		}
		for _, f := range finishesBySubTask[s.SubTaskID] {
			if f.Outcome == counterReviewFinishOutcomeCompleted {
				return true
			}
		}
	}
	return false
}

// hasFinishWithoutStart reports whether trace holds a sub_task_finish whose
// sub_task_start it does not hold (AdditionsFactCheckInTrace's doc comment
// says why that leaves the trace incomplete).
func hasFinishWithoutStart(trace SubTaskTrace) bool {
	started := make(map[string]bool, len(trace.Starts))
	for _, s := range trace.Starts {
		started[s.SubTaskID] = true
	}
	for _, f := range trace.Finishes {
		if !started[f.SubTaskID] {
			return true
		}
	}
	return false
}

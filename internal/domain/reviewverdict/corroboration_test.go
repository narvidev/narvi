package reviewverdict_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

func TestCounterReviewCorroborated(t *testing.T) {
	tests := []struct {
		name     string
		starts   []reviewverdict.SubTaskStartRecord
		finishes []reviewverdict.SubTaskFinishRecord
		want     bool
	}{
		{
			name:     "no starts at all: never corroborated",
			starts:   nil,
			finishes: nil,
			want:     false,
		},
		{
			name: "counter-reviewer start but no finish at all: not corroborated (still active, or race not yet visible)",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: nil,
			want:     false,
		},
		{
			name: "counter-reviewer start plus finish but outcome failed: not corroborated",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-1", Outcome: "failed"},
			},
			want: false,
		},
		{
			name: "counter-reviewer start plus finish but outcome cancelled: not corroborated",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-1", Outcome: "cancelled"},
			},
			want: false,
		},
		{
			name: "counter-reviewer start plus completed finish for the SAME subTaskId: corroborated",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-1", Outcome: "completed"},
			},
			want: true,
		},
		{
			name: "multiple unrelated sub-tasks alongside the real counter-reviewer pair: still finds the right one",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-fact-check", SubAgentType: "fact-check"},
				{SubTaskID: "sub-scribe", SubAgentType: "architecture-scribe"},
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-fact-check", Outcome: "completed"},
				{SubTaskID: "sub-scribe", Outcome: "completed"},
				{SubTaskID: "sub-1", Outcome: "completed"},
			},
			want: true,
		},
		{
			name: "only a different subAgentType present (fact-check), no counter-reviewer at all: not corroborated",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-fact-check", SubAgentType: "fact-check"},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-fact-check", Outcome: "completed"},
			},
			want: false,
		},
		{
			name: "finish exists but for a DIFFERENT subTaskId than the counter-reviewer start: must not false-positive",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-2", Outcome: "completed"},
			},
			want: false,
		},
		{
			name: "empty SubAgentType (legacy/unverified-live subtaskPart fallback path) never matches",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: ""},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-1", Outcome: "completed"},
			},
			want: false,
		},
		{
			name: "two counter-reviewer starts (e.g. a retried sub-task): either completing corroborates",
			starts: []reviewverdict.SubTaskStartRecord{
				{SubTaskID: "sub-1", SubAgentType: review.CounterReviewerAgentName},
				{SubTaskID: "sub-2", SubAgentType: review.CounterReviewerAgentName},
			},
			finishes: []reviewverdict.SubTaskFinishRecord{
				{SubTaskID: "sub-1", Outcome: "failed"},
				{SubTaskID: "sub-2", Outcome: "completed"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewverdict.CounterReviewCorroborated(tt.starts, tt.finishes)
			if got != tt.want {
				t.Errorf("CounterReviewCorroborated(%+v, %+v) = %v, want %v", tt.starts, tt.finishes, got, tt.want)
			}
		})
	}
}

// trace builds a SubTaskTrace read in full from (eventID, kind, subTaskID,
// subAgentType-or-outcome) steps, in the order given.
type traceStep struct {
	id      int64
	finish  bool
	subTask string
	value   string // subAgentType for a start, outcome for a finish
}

func traceOf(steps ...traceStep) reviewverdict.SubTaskTrace {
	trace := reviewverdict.SubTaskTrace{ReadInFull: true}
	for _, s := range steps {
		if s.finish {
			trace.Finishes = append(trace.Finishes, reviewverdict.SubTaskFinishRecord{EventID: s.id, SubTaskID: s.subTask, Outcome: s.value})
		} else {
			trace.Starts = append(trace.Starts, reviewverdict.SubTaskStartRecord{EventID: s.id, SubTaskID: s.subTask, SubAgentType: s.value})
		}
	}
	return trace
}

func start(id int64, subTask, agent string) traceStep {
	return traceStep{id: id, subTask: subTask, value: agent}
}

func finish(id int64, subTask, outcome string) traceStep {
	return traceStep{id: id, finish: true, subTask: subTask, value: outcome}
}

func TestAdditionsFactCheckInTrace(t *testing.T) {
	const (
		fc = review.FactCheckAgentName
		cr = review.CounterReviewerAgentName
	)
	tests := []struct {
		name  string
		trace reviewverdict.SubTaskTrace
		want  reviewpost.AdditionsTrace
	}{
		{
			name: "first fact-check, counter-review, then a second fact-check that completed: run found",
			trace: traceOf(
				start(1, "fc1", fc), finish(2, "fc1", "completed"),
				start(3, "cr1", cr), finish(4, "cr1", "completed"),
				start(5, "fc2", fc), finish(6, "fc2", "completed"),
			),
			want: reviewpost.AdditionsTraceRunFound,
		},
		{
			name: "every fact-check ran before the counter-review: none found",
			trace: traceOf(
				start(1, "fc1", fc), finish(2, "fc1", "completed"),
				start(3, "fc2", fc), finish(4, "fc2", "completed"),
				start(5, "cr1", cr), finish(6, "cr1", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "a fact-check started while the counter-review was running: not after it",
			trace: traceOf(
				start(1, "cr1", cr),
				start(2, "fc2", fc), finish(3, "fc2", "completed"),
				finish(4, "cr1", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "no second fact-check at all: none found",
			trace: traceOf(
				start(1, "fc1", fc), finish(2, "fc1", "completed"),
				start(3, "cr1", cr), finish(4, "cr1", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "the second fact-check failed: none found",
			trace: traceOf(
				start(1, "cr1", cr), finish(2, "cr1", "completed"),
				start(3, "fc2", fc), finish(4, "fc2", "failed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "the second fact-check's finish has not landed: none found",
			trace: traceOf(
				start(1, "cr1", cr), finish(2, "cr1", "completed"),
				start(3, "fc2", fc),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "a later counter-review run started after the second fact-check: not after every counter-review",
			trace: traceOf(
				start(1, "cr1", cr), finish(2, "cr1", "completed"),
				start(3, "fc2", fc), finish(4, "fc2", "completed"),
				start(5, "cr2", cr), finish(6, "cr2", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "a fact-check after a counter-review that never completed: no counter-review to come after",
			trace: traceOf(
				start(1, "cr1", cr), finish(2, "cr1", "failed"),
				start(3, "fc2", fc), finish(4, "fc2", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "a different sub-agent after the counter-review is not a fact-check",
			trace: traceOf(
				start(1, "cr1", cr), finish(2, "cr1", "completed"),
				start(3, "scribe", review.ArchitectureScribeAgentName), finish(4, "scribe", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "a completed finish for another sub-task is not the second run's",
			trace: traceOf(
				start(1, "cr1", cr), finish(2, "cr1", "completed"),
				start(3, "fc2", fc),
				start(4, "other", review.ArchitectureScribeAgentName), finish(5, "other", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			// A retry of the counter-review still running when the verdict
			// is posted: its start alone, with no finish yet, already puts
			// the second fact-check before a counter-reviewer event.
			name: "a later counter-review run started after the second fact-check and has not finished: none found",
			trace: traceOf(
				start(1, "fc1", fc), finish(2, "fc1", "completed"),
				start(3, "cr1", cr), finish(4, "cr1", "completed"),
				start(5, "fc2", fc), finish(6, "fc2", "completed"),
				start(7, "cr2", cr),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			// sub_task_start is best-effort and can be evicted during a long
			// disconnect while its critical finish is delivered: cr2's
			// finish, with no start, could be a counter-reviewer's run after
			// the second fact-check, so the trace is not complete.
			name: "a finish whose start is not in the trace: could not be confirmed",
			trace: traceOf(
				start(10, "fc1", fc), finish(11, "fc1", "completed"),
				start(20, "cr1", cr), finish(21, "cr1", "completed"),
				start(30, "fc2", fc), finish(31, "fc2", "completed"),
				finish(41, "cr2", "completed"),
			),
			want: reviewpost.AdditionsTraceUnread,
		},
		{
			name: "the same trace with that start present: none found",
			trace: traceOf(
				start(10, "fc1", fc), finish(11, "fc1", "completed"),
				start(20, "cr1", cr), finish(21, "cr1", "completed"),
				start(30, "fc2", fc), finish(31, "fc2", "completed"),
				start(40, "cr2", cr), finish(41, "cr2", "completed"),
			),
			want: reviewpost.AdditionsTraceNoRunFound,
		},
		{
			name: "a qualifying run in a trace not read in full: could not be confirmed",
			trace: func() reviewverdict.SubTaskTrace {
				tr := traceOf(
					start(1, "cr1", cr), finish(2, "cr1", "completed"),
					start(3, "fc2", fc), finish(4, "fc2", "completed"),
				)
				tr.ReadInFull = false
				return tr
			}(),
			want: reviewpost.AdditionsTraceUnread,
		},
		{
			name:  "the zero trace was never read: could not be confirmed",
			trace: reviewverdict.SubTaskTrace{},
			want:  reviewpost.AdditionsTraceUnread,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewverdict.AdditionsFactCheckInTrace(tt.trace); got != tt.want {
				t.Errorf("AdditionsFactCheckInTrace() = %q, want %q", got, tt.want)
			}
		})
	}
}

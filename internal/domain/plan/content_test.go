package plan

import "testing"

func i64(n int64) *int64 { return &n }

func TestExtractContent(t *testing.T) {
	tests := []struct {
		name              string
		events            []ContentEvent
		lowerBoundEventID *int64
		upperBoundEventID *int64
		want              string
	}{
		{
			name:              "no events at all falls back",
			events:            nil,
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              ContentFallbackText,
		},
		{
			name: "single token event in window, unbounded above (the original single-turn case)",
			events: []ContentEvent{
				{ID: 15, Type: "token", Text: "final plan text"},
				{ID: 14, Type: "tool_call", Text: ""},
				{ID: 11, Type: "token", Text: "final plan text"}, // cumulative -- same messageId, earlier partial superseded
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              "final plan text",
		},
		{
			name: "newest-first scan finds the LAST token event by arrival order first",
			events: []ContentEvent{
				{ID: 20, Type: "token", Text: "the full, final text"},
				{ID: 19, Type: "token", Text: "the full, fin"},
				{ID: 18, Type: "token", Text: "the full"},
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              "the full, final text",
		},
		{
			name: "events at or below lowerBound belong to an earlier turn and are excluded",
			events: []ContentEvent{
				{ID: 12, Type: "token", Text: "this turn's own text"},
				{ID: 10, Type: "token", Text: "an EARLIER turn's text -- must never be returned"},
				{ID: 9, Type: "token", Text: "even earlier"},
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              "this turn's own text",
		},
		{
			name: "lowerBound exactly matches an event id -- that event itself is excluded (exclusive bound)",
			events: []ContentEvent{
				{ID: 10, Type: "token", Text: "must never be returned -- this IS the dispatch boundary event"},
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              ContentFallbackText,
		},
		{
			name: "events strictly above upperBound belong to a LATER turn and are excluded -- the defining bug this generalization fixes",
			events: []ContentEvent{
				{ID: 30, Type: "token", Text: "a LATER turn's own text -- e.g. the approval-dispatched implementation turn"},
				{ID: 25, Type: "token", Text: "this plan version's own real content"},
				{ID: 20, Type: "tool_call", Text: ""},
			},
			lowerBoundEventID: i64(15),
			upperBoundEventID: i64(28),
			want:              "this plan version's own real content",
		},
		{
			// Regression case for a real off-by-one caught live by
			// httpapi/plans_integration_test.go against a real Postgres
			// instance, not by a unit test: a turn's own DispatchedEventID is
			// the events-log watermark that existed BEFORE any of that
			// turn's events were produced (see ExtractContent's own doc
			// comment on upperBoundEventID) -- so when the NEXT turn
			// dispatches immediately after THIS turn's own last (and only)
			// event with no intervening activity, that next turn's own
			// DispatchedEventID exactly EQUALS this turn's own last event's
			// id. That event must still be counted as belonging to THIS
			// turn, never excluded -- an exclusive upper-bound comparison
			// here would silently drop it, exactly the failure the
			// integration test caught (a plan turn whose only token event
			// happened to land exactly on the next turn's own dispatch
			// watermark fell back to ContentFallbackText instead of
			// returning its own real content).
			name: "upperBound exactly matches an event id -- that event is INCLUDED (it is this turn's own, not the next turn's)",
			events: []ContentEvent{
				{ID: 28, Type: "token", Text: "this turn's own last event -- happens to equal the NEXT turn's own dispatch watermark"},
				{ID: 20, Type: "token", Text: "an earlier token event from this same turn"},
			},
			lowerBoundEventID: i64(15),
			upperBoundEventID: i64(28),
			want:              "this turn's own last event -- happens to equal the NEXT turn's own dispatch watermark",
		},
		{
			name: "nothing in window at all falls back, even with plenty of events outside it",
			events: []ContentEvent{
				{ID: 30, Type: "token", Text: "later turn"},
				{ID: 5, Type: "token", Text: "earlier turn"},
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: i64(28),
			want:              ContentFallbackText,
		},
		{
			name: "a token event with empty text (a heartbeat/keepalive artifact) is skipped, never treated as content",
			events: []ContentEvent{
				{ID: 16, Type: "token", Text: ""},
				{ID: 15, Type: "token", Text: "the real content"},
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              "the real content",
		},
		{
			name: "non-token event types are ignored regardless of position",
			events: []ContentEvent{
				{ID: 16, Type: "tool_result", Text: ""},
				{ID: 15, Type: "sub_task_finish", Text: ""},
				{ID: 14, Type: "token", Text: "the content"},
			},
			lowerBoundEventID: i64(10),
			upperBoundEventID: nil,
			want:              "the content",
		},
		{
			name:              "nil lowerBound scans to the oldest supplied event",
			events:            []ContentEvent{{ID: 3, Type: "token", Text: "very old"}},
			lowerBoundEventID: nil,
			upperBoundEventID: nil,
			want:              "very old",
		},

		// Text parts, told apart by the payload's messageId. The turn's
		// text is its last part -- the one whose first in-window frame is
		// newest -- read as that part's newest non-empty frame.
		{
			name: "parts in order: the last part's final frame",
			events: []ContentEvent{
				{ID: 16, Type: "execution_complete"},
				{ID: 15, Type: "token", MessageID: "prt_b", Text: "1. Add the migration\n2. Wire the store"},
				{ID: 14, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 13, Type: "step_start"},
				{ID: 12, Type: "token", MessageID: "prt_a", Text: "Let me look at the repository first."},
				{ID: 11, Type: "token", MessageID: "prt_a", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "1. Add the migration\n2. Wire the store",
		},
		{
			// The turn running at deploy, on a sandbox-agent that sends live
			// ahead of its replay: the previous binary stored each part's
			// empty first frame, prt_b's final frame then arrived live, and
			// prt_a's full frame, replayed, was stored after it. The newest
			// row is prt_a's; prt_b opened last.
			name: "a later part's frame stored before an earlier part's replayed frame: the part that opened last",
			events: []ContentEvent{
				{ID: 18, Type: "execution_complete"},
				{ID: 17, Type: "token", MessageID: "prt_a", Text: "I'll start by reading the store and its migrations."},
				{ID: 16, Type: "token", MessageID: "prt_b", Text: "1. Add the migration\n2. Wire the store\n3. Tests"},
				{ID: 14, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 13, Type: "step_start"},
				{ID: 12, Type: "token", MessageID: "prt_a", Text: ""},
				{ID: 11, Type: "step_start"},
			},
			lowerBoundEventID: i64(10),
			want:              "1. Add the migration\n2. Wire the store\n3. Tests",
		},
		{
			// The same wire order, where the later part's frames all arrived
			// live after the replay had stored only the earlier part's empty
			// first frame.
			name: "a later part stored whole before an earlier part's replayed frame: the part that opened last",
			events: []ContentEvent{
				{ID: 19, Type: "execution_complete"},
				{ID: 18, Type: "token", MessageID: "prt_n", Text: "Let me look at the repository first."},
				{ID: 17, Type: "token", MessageID: "prt_p", Text: "1. Add the migration\n2. Wire the store"},
				{ID: 16, Type: "token", MessageID: "prt_p", Text: ""},
				{ID: 15, Type: "step_start"},
				{ID: 12, Type: "token", MessageID: "prt_n", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "1. Add the migration\n2. Wire the store",
		},
		{
			name: "the chosen part's newest non-empty frame, not its first non-empty one",
			events: []ContentEvent{
				{ID: 17, Type: "token", MessageID: "prt_a", Text: "Narration, recovered late."},
				{ID: 16, Type: "token", MessageID: "prt_b", Text: "1. Add the migration\n2. Wire the store\n3. Tests"},
				{ID: 15, Type: "token", MessageID: "prt_b", Text: "1. Add the migration"},
				{ID: 14, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 12, Type: "token", MessageID: "prt_a", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "1. Add the migration\n2. Wire the store\n3. Tests",
		},
		{
			// prt_b opened last, but none of its frames beyond the empty
			// first one reached the log: it has no text, so the newest-
			// opened part WITH text is read, as an empty row always was
			// skipped.
			name: "a part whose only frames are empty is passed over, even when it opened last",
			events: []ContentEvent{
				{ID: 14, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 13, Type: "step_start"},
				{ID: 12, Type: "token", MessageID: "prt_a", Text: "The plan, in the first part."},
				{ID: 11, Type: "token", MessageID: "prt_a", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "The plan, in the first part.",
		},
		{
			name: "an empty-only part between two text parts does not hide the last text part",
			events: []ContentEvent{
				{ID: 17, Type: "token", MessageID: "prt_a", Text: "Narration, recovered late."},
				{ID: 16, Type: "token", MessageID: "prt_c", Text: "The plan."},
				{ID: 15, Type: "token", MessageID: "prt_c", Text: ""},
				{ID: 13, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 11, Type: "token", MessageID: "prt_a", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "The plan.",
		},
		{
			name: "every part empty falls back",
			events: []ContentEvent{
				{ID: 13, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 12, Type: "step_start"},
				{ID: 11, Type: "token", MessageID: "prt_a", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              ContentFallbackText,
		},
		{
			name: "a single-part turn: its newest non-empty frame",
			events: []ContentEvent{
				{ID: 15, Type: "execution_complete"},
				{ID: 14, Type: "token", MessageID: "prt_only", Text: "The whole plan."},
				{ID: 13, Type: "tool_call"},
				{ID: 12, Type: "token", MessageID: "prt_only", Text: "The whole"},
				{ID: 11, Type: "token", MessageID: "prt_only", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "The whole plan.",
		},
		{
			// A frame at or below the lower bound is an earlier turn's and
			// places nothing: prt_b is ordered by its first frame in the
			// window. (The session actor stores no frame in a later window
			// for a part first stored in an earlier one, so the log does not
			// hold this; it pins what the window alone decides.)
			name: "a frame below the lower bound does not place a part",
			events: []ContentEvent{
				{ID: 14, Type: "token", MessageID: "prt_b", Text: "The plan."},
				{ID: 13, Type: "token", MessageID: "prt_b", Text: ""},
				{ID: 12, Type: "token", MessageID: "prt_a", Text: "Narration."},
				{ID: 11, Type: "token", MessageID: "prt_a", Text: ""},
				{ID: 9, Type: "token", MessageID: "prt_b", Text: ""},
			},
			lowerBoundEventID: i64(10),
			want:              "The plan.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractContent(tt.events, tt.lowerBoundEventID, tt.upperBoundEventID)
			if got != tt.want {
				t.Errorf("ExtractContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

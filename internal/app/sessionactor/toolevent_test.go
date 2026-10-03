package sessionactor

import "testing"

// TestMessageFromEarlierTurn pins where an assistant message sits against
// the Processing turn's window, by the row stored under its bare id -- its
// step_start: every event that turn produces has an id above its
// dispatched_event_id, so a message whose row lies at or below it entered
// the log before the turn existed, and a late tool_call, tool_result or
// step_finish of it adds no row.
func TestMessageFromEarlierTurn(t *testing.T) {
	t.Parallel()

	watermark := func(id int64) *int64 { return &id }
	tests := []struct {
		name              string
		messageRowID      int64
		dispatchedEventID *int64
		want              bool
	}{
		{name: "the message's step_start inside the turn's window", messageRowID: 11, dispatchedEventID: watermark(10), want: false},
		{name: "the message's step_start at the watermark itself", messageRowID: 10, dispatchedEventID: watermark(10), want: true},
		{name: "the message's step_start before the turn was dispatched", messageRowID: 3, dispatchedEventID: watermark(10), want: true},
		{name: "turn dispatched into an empty log", messageRowID: 1, dispatchedEventID: watermark(0), want: false},
		{name: "no watermark places no window", messageRowID: 1, dispatchedEventID: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := messageFromEarlierTurn(tt.messageRowID, tt.dispatchedEventID); got != tt.want {
				t.Errorf("messageFromEarlierTurn(%d, %v) = %v, want %v", tt.messageRowID, tt.dispatchedEventID, got, tt.want)
			}
		})
	}
}

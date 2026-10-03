package sessionactor

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestSyntheticExecutionComplete: the payload of the control plane's own
// turn end names the turn, says it is synthetic and why, and carries the
// dispatched stamp only for a turn that was dispatched -- the turn a page
// is showing -- never for one ended from pending.
func TestSyntheticExecutionComplete(t *testing.T) {
	t.Parallel()

	id := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	tests := []struct {
		from       turn.State
		dispatched bool
	}{
		{turn.StatePending, false},
		{turn.StateDispatched, true},
		{turn.StateProcessing, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.from), func(t *testing.T) {
			t.Parallel()
			p := syntheticExecutionComplete(id, tc.from, "stopped")
			if p["turn_id"] != id.String() || p["synthetic"] != true || p["reason"] != "stopped" {
				t.Fatalf("payload = %v, want turn_id, synthetic and reason", p)
			}
			got, stamped := p[syntheticDispatchedKey]
			if stamped != tc.dispatched || (stamped && got != true) {
				t.Errorf("payload = %v, want the dispatched stamp %v", p, tc.dispatched)
			}
			if len(p) != 3+map[bool]int{true: 1, false: 0}[tc.dispatched] {
				t.Errorf("payload = %v, want nothing else", p)
			}
		})
	}
}

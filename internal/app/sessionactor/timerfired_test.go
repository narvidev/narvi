package sessionactor

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// uuidFromByte builds a distinguishable, valid pgtype.UUID for table-test
// fixtures -- only byte 0 varies, which is enough to make each fixture
// unique and comparable via ==.
func uuidFromByte(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[0] = b
	u.Valid = true
	return u
}

func TestPgTimeOrZero(t *testing.T) {
	t.Parallel()

	now := time.Now().Truncate(time.Microsecond)

	tests := []struct {
		name string
		in   pgtype.Timestamptz
		want time.Time
	}{
		{"valid", pgtype.Timestamptz{Time: now, Valid: true}, now},
		{"invalid", pgtype.Timestamptz{Time: now, Valid: false}, time.Time{}},
		{"zero value", pgtype.Timestamptz{}, time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pgTimeOrZero(tc.in); !got.Equal(tc.want) {
				t.Errorf("pgTimeOrZero(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsConnectingPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status sandbox.State
		want   bool
	}{
		{sandbox.StatePending, false},
		{sandbox.StateSpawning, true},
		{sandbox.StateConnecting, true},
		{sandbox.StateBooting, true},
		{sandbox.StateReady, false},
		{sandbox.StateSnapshotting, false},
		{sandbox.StateSuspect, false},
		{sandbox.StateStopped, false},
		{sandbox.StateFailed, false},
		{sandbox.StateStale, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			if got := isConnectingPhase(tc.status); got != tc.want {
				t.Errorf("isConnectingPhase(%s) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestFindProcessingTurn(t *testing.T) {
	t.Parallel()

	processingID := uuidFromByte(2)

	tests := []struct {
		name   string
		turns  []sqlcgen.Turn
		wantID pgtype.UUID
		wantOK bool
	}{
		{
			name:   "no turns",
			turns:  nil,
			wantOK: false,
		},
		{
			name: "no processing turn",
			turns: []sqlcgen.Turn{
				{ID: uuidFromByte(1), Status: sqlcgen.TurnStatusCompleted},
				{ID: uuidFromByte(2), Status: sqlcgen.TurnStatusFailed},
			},
			wantOK: false,
		},
		{
			name: "one processing turn among others",
			turns: []sqlcgen.Turn{
				{ID: uuidFromByte(1), Status: sqlcgen.TurnStatusCompleted},
				{ID: processingID, Status: sqlcgen.TurnStatusProcessing},
				{ID: uuidFromByte(3), Status: sqlcgen.TurnStatusPending},
			},
			wantID: processingID,
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := findProcessingTurn(tc.turns)
			if ok != tc.wantOK {
				t.Fatalf("findProcessingTurn() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.ID != tc.wantID {
				t.Errorf("findProcessingTurn() ID = %v, want %v", got.ID, tc.wantID)
			}
		})
	}
}

func TestAnyTurnProcessing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		turns []sqlcgen.Turn
		want  bool
	}{
		{"empty", nil, false},
		{"none processing", []sqlcgen.Turn{{Status: sqlcgen.TurnStatusCompleted}}, false},
		{"one processing", []sqlcgen.Turn{{Status: sqlcgen.TurnStatusPending}, {Status: sqlcgen.TurnStatusProcessing}}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := anyTurnProcessing(tc.turns); got != tc.want {
				t.Errorf("anyTurnProcessing() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSummariesWithOverride(t *testing.T) {
	t.Parallel()

	id1, id2, id3 := uuidFromByte(1), uuidFromByte(2), uuidFromByte(3)
	turns := []sqlcgen.Turn{
		{ID: id1, Status: sqlcgen.TurnStatusCompleted},
		{ID: id2, Status: sqlcgen.TurnStatusProcessing},
		{ID: id3, Status: sqlcgen.TurnStatusPending},
	}

	got := summariesWithOverride(turns, id2, turn.StateFailed, turn.FailureReasonTimeout)

	want := []turn.Summary{
		{Status: turn.StateCompleted},
		{Status: turn.StateFailed, FailureReason: turn.FailureReasonTimeout},
		{Status: turn.StatePending},
	}

	if len(got) != len(want) {
		t.Fatalf("summariesWithOverride() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("summariesWithOverride()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSummariesForRederive(t *testing.T) {
	t.Parallel()

	turns := []sqlcgen.Turn{
		{Status: sqlcgen.TurnStatusCompleted},
		{Status: sqlcgen.TurnStatusFailed},
	}

	got := summariesForRederive(turns, turn.FailureReasonTimeout)

	want := []turn.Summary{
		{Status: turn.StateCompleted},
		{Status: turn.StateFailed, FailureReason: turn.FailureReasonTimeout},
	}

	if len(got) != len(want) {
		t.Fatalf("summariesForRederive() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("summariesForRederive()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if got := summariesForRederive(nil, turn.FailureReasonCancelled); len(got) != 0 {
		t.Errorf("summariesForRederive(nil, ...) = %v, want empty", got)
	}
}

// TestSummariesForRederive_SkipsATurnEndedContextMoved: a turn with an end
// reason is marked ignored, in the stored summaries and in the re-derive's,
// and the re-derive's recorded reason goes to the last turn that is not --
// the one DeriveStatus reads (technical plan §24.9).
func TestSummariesForRederive_SkipsATurnEndedContextMoved(t *testing.T) {
	t.Parallel()

	moved := turn.EndReasonContextMoved
	turns := []sqlcgen.Turn{
		{Status: sqlcgen.TurnStatusFailed},
		{Status: sqlcgen.TurnStatusFailed, EndReason: &moved},
	}

	got := summariesForRederive(turns, turn.FailureReasonTimeout)
	want := []turn.Summary{
		{Status: turn.StateFailed, FailureReason: turn.FailureReasonTimeout},
		{Status: turn.StateFailed, Ignored: true},
	}
	if len(got) != len(want) {
		t.Fatalf("summariesForRederive() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("summariesForRederive()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if got, want := storedTurnSummary(turns[1]), (turn.Summary{Status: turn.StateFailed, Ignored: true}); got != want {
		t.Errorf("storedTurnSummary(context_moved) = %+v, want %+v", got, want)
	}
	if got := storedTurnSummary(turns[0]); got.Ignored {
		t.Errorf("storedTurnSummary(no end reason) = %+v, want not ignored", got)
	}
}

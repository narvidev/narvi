package sandbox_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// TestClassifyRestoreConversation covers every row of the classification
// (technical plan §35.5), in its order, each with the neighbour that pins
// where it stops: nothing recorded wins over every conversation, no
// conversation on either side is none, and only the same conversation on
// both is held.
func TestClassifyRestoreConversation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		recorded          bool
		snapshot, session string
		want              sandbox.RestoreConversation
	}{
		{name: "row 1: not recorded, the same conversation on both", snapshot: "conv-a", session: "conv-a", want: sandbox.RestoreConversationUnknown},
		{name: "row 1: not recorded, no conversation on either", want: sandbox.RestoreConversationUnknown},
		{name: "row 1: not recorded, the session names one", session: "conv-a", want: sandbox.RestoreConversationUnknown},
		{name: "row 2: recorded, no conversation on either", recorded: true, want: sandbox.RestoreConversationNone},
		{name: "row 3: recorded, the same conversation", recorded: true, snapshot: "conv-a", session: "conv-a", want: sandbox.RestoreConversationHeld},
		{name: "row 4: recorded, another conversation", recorded: true, snapshot: "conv-a", session: "conv-b", want: sandbox.RestoreConversationDiffers},
		{name: "row 4: minted under none, the prompt names one", recorded: true, session: "conv-b", want: sandbox.RestoreConversationDiffers},
		{name: "row 4: minted under one, the prompt names none", recorded: true, snapshot: "conv-a", want: sandbox.RestoreConversationDiffers},
		{name: "row 4: ids that differ only in case", recorded: true, snapshot: "conv-a", session: "CONV-A", want: sandbox.RestoreConversationDiffers},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sandbox.ClassifyRestoreConversation(tc.recorded, tc.snapshot, tc.session); got != tc.want {
				t.Fatalf("ClassifyRestoreConversation(%v, %q, %q) = %q, want %q", tc.recorded, tc.snapshot, tc.session, got, tc.want)
			}
		})
	}
}

// TestClassifyRestoreConversation_Properties pins what the counter's labels
// mean over every combination: held only for a recorded snapshot whose
// conversation is the prompt's own, none only when neither names one,
// unknown exactly when nothing is recorded, and every outcome one of the
// four.
func TestClassifyRestoreConversation_Properties(t *testing.T) {
	t.Parallel()

	ids := []string{"", "conv-a", "conv-b"}
	for _, recorded := range []bool{false, true} {
		for _, snapshot := range ids {
			for _, session := range ids {
				got := sandbox.ClassifyRestoreConversation(recorded, snapshot, session)
				switch got {
				case sandbox.RestoreConversationHeld:
					if !recorded || snapshot == "" || snapshot != session {
						t.Errorf("(%v, %q, %q) = held, want held only for a recorded snapshot of the prompt's own conversation", recorded, snapshot, session)
					}
				case sandbox.RestoreConversationNone:
					if !recorded || snapshot != "" || session != "" {
						t.Errorf("(%v, %q, %q) = none, want none only when neither names a conversation", recorded, snapshot, session)
					}
				case sandbox.RestoreConversationDiffers:
					if !recorded || snapshot == session {
						t.Errorf("(%v, %q, %q) = differs, want differs only for a recorded snapshot of another conversation", recorded, snapshot, session)
					}
				case sandbox.RestoreConversationUnknown:
					if recorded {
						t.Errorf("(%v, %q, %q) = unknown for a recorded snapshot", recorded, snapshot, session)
					}
				default:
					t.Errorf("(%v, %q, %q) = %q, not one of the four outcomes", recorded, snapshot, session, got)
				}
			}
		}
	}
}

package review_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

// TestTransitionCompositionDecision covers §12.2 item 9's own "Block
// release / Acknowledge & ship [/ Unblock]" transition table
// (compositiondecision.go): two legal transitions out of Pending (both
// ending in a decided state), one further legal transition out of Blocked
// back to Pending (the confirmed-major "unblock path" fix -- Blocked is
// no longer fully terminal), Acknowledged remains genuinely terminal with
// no outgoing edge at all, and every other (current, action) pair --
// including an already-decided current, an unrecognized/zero-value
// current, and unblock attempted from anywhere but Blocked -- is illegal.
func TestTransitionCompositionDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current review.CompositionDecision
		action  review.CompositionDecisionAction
		want    review.CompositionDecision
		wantErr bool
	}{
		{
			name:    "pending + block -> blocked",
			current: review.CompositionDecisionPending,
			action:  review.CompositionDecisionActionBlock,
			want:    review.CompositionDecisionBlocked,
		},
		{
			name:    "pending + acknowledge -> acknowledged",
			current: review.CompositionDecisionPending,
			action:  review.CompositionDecisionActionAcknowledge,
			want:    review.CompositionDecisionAcknowledged,
		},
		{
			name:    "blocked + block -> illegal (no re-block)",
			current: review.CompositionDecisionBlocked,
			action:  review.CompositionDecisionActionBlock,
			wantErr: true,
		},
		{
			name:    "blocked + acknowledge -> illegal (must unblock first)",
			current: review.CompositionDecisionBlocked,
			action:  review.CompositionDecisionActionAcknowledge,
			wantErr: true,
		},
		{
			name:    "blocked + unblock -> pending (the confirmed-major fix)",
			current: review.CompositionDecisionBlocked,
			action:  review.CompositionDecisionActionUnblock,
			want:    review.CompositionDecisionPending,
		},
		{
			name:    "pending + unblock -> illegal (nothing to unblock)",
			current: review.CompositionDecisionPending,
			action:  review.CompositionDecisionActionUnblock,
			wantErr: true,
		},
		{
			name:    "acknowledged + block -> illegal (fully terminal)",
			current: review.CompositionDecisionAcknowledged,
			action:  review.CompositionDecisionActionBlock,
			wantErr: true,
		},
		{
			name:    "acknowledged + acknowledge -> illegal (fully terminal)",
			current: review.CompositionDecisionAcknowledged,
			action:  review.CompositionDecisionActionAcknowledge,
			wantErr: true,
		},
		{
			name:    "acknowledged + unblock -> illegal (fully terminal)",
			current: review.CompositionDecisionAcknowledged,
			action:  review.CompositionDecisionActionUnblock,
			wantErr: true,
		},
		{
			name:    "unrecognized current -> illegal, never treated as pending",
			current: review.CompositionDecision("garbled"),
			action:  review.CompositionDecisionActionBlock,
			wantErr: true,
		},
		{
			name:    "zero-value current -> illegal, never treated as pending",
			current: review.CompositionDecision(""),
			action:  review.CompositionDecisionActionBlock,
			wantErr: true,
		},
		{
			name:    "unrecognized action against pending -> illegal",
			current: review.CompositionDecisionPending,
			action:  review.CompositionDecisionAction("delete"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := review.TransitionCompositionDecision(tt.current, tt.action)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("TransitionCompositionDecision(%q, %q) = %q, nil, want an error", tt.current, tt.action, got)
				}
				if !errors.Is(err, review.ErrIllegalCompositionDecisionTransition) {
					t.Errorf("error = %v, want it to wrap ErrIllegalCompositionDecisionTransition", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("TransitionCompositionDecision(%q, %q) unexpected error: %v", tt.current, tt.action, err)
			}
			if got != tt.want {
				t.Errorf("TransitionCompositionDecision(%q, %q) = %q, want %q", tt.current, tt.action, got, tt.want)
			}
		})
	}
}

package review_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

// TestTransitionCompositionDecision covers §12.2 item 9's own "Block
// release / Acknowledge & ship" transition table (compositiondecision.go):
// exactly two legal transitions, both out of Pending, both terminal, and
// every other (current, action) pair -- including an already-decided
// current and an unrecognized/zero-value current -- is illegal.
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
			name:    "blocked + block -> illegal (terminal)",
			current: review.CompositionDecisionBlocked,
			action:  review.CompositionDecisionActionBlock,
			wantErr: true,
		},
		{
			name:    "blocked + acknowledge -> illegal (terminal)",
			current: review.CompositionDecisionBlocked,
			action:  review.CompositionDecisionActionAcknowledge,
			wantErr: true,
		},
		{
			name:    "acknowledged + block -> illegal (terminal)",
			current: review.CompositionDecisionAcknowledged,
			action:  review.CompositionDecisionActionBlock,
			wantErr: true,
		},
		{
			name:    "acknowledged + acknowledge -> illegal (terminal)",
			current: review.CompositionDecisionAcknowledged,
			action:  review.CompositionDecisionActionAcknowledge,
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

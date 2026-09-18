package automation_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/automation"
)

func TestEvaluateDispatchThrottle(t *testing.T) {
	tests := []struct {
		name          string
		countInWindow int
		want          bool
	}{
		{"well under threshold", 0, true},
		{"just under threshold", automation.DispatchThrottleThreshold - 1, true},
		{"exactly at threshold, blocked", automation.DispatchThrottleThreshold, false},
		{"over threshold, blocked", automation.DispatchThrottleThreshold + 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := automation.EvaluateDispatchThrottle(tt.countInWindow); got != tt.want {
				t.Fatalf("EvaluateDispatchThrottle(%d) = %v, want %v", tt.countInWindow, got, tt.want)
			}
		})
	}
}

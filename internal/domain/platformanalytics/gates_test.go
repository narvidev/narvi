package platformanalytics_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/platformanalytics"
)

func TestBootP95Computed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sampleSize int64
		want       bool
	}{
		{name: "zero samples", sampleSize: 0, want: false},
		{name: "one below the floor", sampleSize: platformanalytics.BootP95MinSamples - 1, want: false},
		{name: "exactly the floor", sampleSize: platformanalytics.BootP95MinSamples, want: true},
		{name: "well above the floor", sampleSize: platformanalytics.BootP95MinSamples * 10, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := platformanalytics.BootP95Computed(tt.sampleSize); got != tt.want {
				t.Errorf("BootP95Computed(%d) = %v, want %v", tt.sampleSize, got, tt.want)
			}
		})
	}
}

func TestCostComputed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		costedSessionCount int64
		want               bool
	}{
		{name: "zero costed sessions", costedSessionCount: 0, want: false},
		{name: "one costed session is enough", costedSessionCount: 1, want: true},
		{name: "many costed sessions", costedSessionCount: 500, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := platformanalytics.CostComputed(tt.costedSessionCount); got != tt.want {
				t.Errorf("CostComputed(%d) = %v, want %v", tt.costedSessionCount, got, tt.want)
			}
		})
	}
}

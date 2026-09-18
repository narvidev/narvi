package reviewcheck

import "testing"

func TestPhase_Valid(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		want  bool
	}{
		{"queued", PhaseQueued, true},
		{"running", PhaseRunning, true},
		{"stale", PhaseStale, true},
		{"terminal assessed", PhaseTerminalAssessed, true},
		{"terminal not assessed", PhaseTerminalNotAssessed, true},
		{"zero value", Phase(""), false},
		{"unrecognized", Phase("bogus"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.phase.Valid(); got != tt.want {
				t.Errorf("Phase(%q).Valid() = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

func TestPhase_Terminal(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		want  bool
	}{
		{"queued", PhaseQueued, false},
		{"running", PhaseRunning, false},
		{"stale", PhaseStale, true},
		{"terminal assessed", PhaseTerminalAssessed, true},
		{"terminal not assessed", PhaseTerminalNotAssessed, true},
		{"unrecognized", Phase("bogus"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.phase.Terminal(); got != tt.want {
				t.Errorf("Phase(%q).Terminal() = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

// TestPhase_RankOrdering pins the rank ordering Supersedes' own
// same-attempt comparison depends on: Queued < Running < every
// terminal-shaped phase, and the three terminal-shaped phases share a
// rank (mutually-exclusive alternate outcomes, never a further ordering
// among themselves -- see Phase's own doc comment).
func TestPhase_RankOrdering(t *testing.T) {
	if PhaseQueued.rank() >= PhaseRunning.rank() {
		t.Errorf("queued.rank() = %d, running.rank() = %d; want queued < running", PhaseQueued.rank(), PhaseRunning.rank())
	}
	if PhaseRunning.rank() >= PhaseTerminalAssessed.rank() {
		t.Errorf("running.rank() = %d, terminal_assessed.rank() = %d; want running < terminal_assessed", PhaseRunning.rank(), PhaseTerminalAssessed.rank())
	}
	if PhaseStale.rank() != PhaseTerminalAssessed.rank() || PhaseTerminalAssessed.rank() != PhaseTerminalNotAssessed.rank() {
		t.Errorf("stale/terminal_assessed/terminal_not_assessed ranks must be equal, got %d/%d/%d",
			PhaseStale.rank(), PhaseTerminalAssessed.rank(), PhaseTerminalNotAssessed.rank())
	}
}

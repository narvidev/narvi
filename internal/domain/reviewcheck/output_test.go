package reviewcheck

import "testing"

// TestComputeOutput_NotAssessedIsActionRequired pins decision 1, verbatim:
// "a review that did not complete publishes action_required". This is
// the ONE mapping the brief fixes rather than leaves to this package's
// own judgment -- a dedicated test, distinct from the table test below,
// so a future edit that quietly changes this one row is caught by name.
func TestComputeOutput_NotAssessedIsActionRequired(t *testing.T) {
	out := ComputeOutput(PhaseTerminalNotAssessed)
	if out.Status != StatusCompleted {
		t.Errorf("PhaseTerminalNotAssessed: status = %q, want %q", out.Status, StatusCompleted)
	}
	if out.Conclusion != ConclusionActionRequired {
		t.Errorf("PhaseTerminalNotAssessed: conclusion = %q, want %q", out.Conclusion, ConclusionActionRequired)
	}
}

// TestComputeOutput_NeutralNeverAppears is the brief's own explicit
// invariant: "neutral would satisfy a required check... and it must
// appear nowhere." Enumerates every named Phase PLUS the fail-closed
// default (an invalid Phase) -- the invariant must hold for the path a
// programmer error takes too, not only the five legitimate values.
func TestComputeOutput_NeutralNeverAppears(t *testing.T) {
	const neutral = Conclusion("neutral")
	phases := []Phase{
		PhaseQueued, PhaseRunning, PhaseStale,
		PhaseTerminalAssessed, PhaseTerminalNotAssessed,
		Phase(""), Phase("bogus"),
	}
	for _, p := range phases {
		if out := ComputeOutput(p); out.Conclusion == neutral {
			t.Errorf("ComputeOutput(%q).Conclusion = %q, must never be neutral", p, out.Conclusion)
		}
	}
}

// TestComputeOutput_Mapping pins the full state -> (status, conclusion)
// table this package's own ComputeOutput doc comment states.
func TestComputeOutput_Mapping(t *testing.T) {
	tests := []struct {
		phase          Phase
		wantStatus     Status
		wantConclusion Conclusion
	}{
		{PhaseQueued, StatusQueued, ConclusionNone},
		{PhaseRunning, StatusInProgress, ConclusionNone},
		{PhaseStale, StatusCompleted, ConclusionActionRequired},
		{PhaseTerminalNotAssessed, StatusCompleted, ConclusionActionRequired},
		{PhaseTerminalAssessed, StatusCompleted, ConclusionSuccess},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			out := ComputeOutput(tt.phase)
			if out.Status != tt.wantStatus {
				t.Errorf("ComputeOutput(%q).Status = %q, want %q", tt.phase, out.Status, tt.wantStatus)
			}
			if out.Conclusion != tt.wantConclusion {
				t.Errorf("ComputeOutput(%q).Conclusion = %q, want %q", tt.phase, out.Conclusion, tt.wantConclusion)
			}
			if out.Title == "" || out.Summary == "" {
				t.Errorf("ComputeOutput(%q): Title/Summary must never be blank", tt.phase)
			}
		})
	}
}

// TestComputeOutput_IncompleteNeverCarriesConclusion pins GitHub's own
// API requirement this package's doc comment names: an incomplete check
// run (status queued/in_progress) must never carry a conclusion value at
// all.
func TestComputeOutput_IncompleteNeverCarriesConclusion(t *testing.T) {
	for _, p := range []Phase{PhaseQueued, PhaseRunning} {
		out := ComputeOutput(p)
		if out.Conclusion != ConclusionNone {
			t.Errorf("ComputeOutput(%q).Conclusion = %q, want empty (incomplete status must carry no conclusion)", p, out.Conclusion)
		}
	}
}

// TestComputeOutput_UnrecognizedPhaseFailsClosed pins the fail-closed
// default: a Phase this package does not recognize must never render as
// a pass.
func TestComputeOutput_UnrecognizedPhaseFailsClosed(t *testing.T) {
	for _, p := range []Phase{Phase(""), Phase("bogus")} {
		out := ComputeOutput(p)
		if out.Conclusion == ConclusionSuccess {
			t.Errorf("ComputeOutput(%q).Conclusion = success, want a non-passing conclusion for an unrecognized phase", p)
		}
		if out.Status != StatusCompleted {
			t.Errorf("ComputeOutput(%q).Status = %q, want %q (fail closed, never left incomplete forever)", p, out.Status, StatusCompleted)
		}
	}
}

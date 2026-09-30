package workflow_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/workflow"
)

// TestNextStep is table-driven over every routing shape §25.4/§25.9
// name: the fail-conservative defaults (ok advances by Order,
// needs_fix/blocked escalate), completion at the last step, explicit
// edges winning over defaults (including the audit-loop's backward ok
// edge and plan's needs_fix self-loop), and Order-gap robustness.
func TestNextStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		def     func() workflow.Definition
		current workflow.ID
		outcome workflow.StepOutcomeStatus
		want    workflow.Next
	}{
		{
			name:    "default: ok advances to the next step in Order",
			def:     validPlanShape,
			current: "plan",
			outcome: workflow.StepOutcomeOK,
			want:    workflow.Next{Kind: workflow.NextAdvance, ToStepID: "build"},
		},
		{
			name:    "default: ok at the last step completes the run",
			def:     validPlanShape,
			current: "build",
			outcome: workflow.StepOutcomeOK,
			want:    workflow.Next{Kind: workflow.NextComplete},
		},
		{
			name:    "default: ok on a single-step definition completes",
			def:     validSingleStep,
			current: "s1",
			outcome: workflow.StepOutcomeOK,
			want:    workflow.Next{Kind: workflow.NextComplete},
		},
		{
			name:    "default: needs_fix with no edge escalates (fail-conservative)",
			def:     validSingleStep,
			current: "s1",
			outcome: workflow.StepOutcomeNeedsFix,
			want:    workflow.Next{Kind: workflow.NextEscalate},
		},
		{
			name:    "default: blocked with no edge escalates (fail-conservative)",
			def:     validPlanShape,
			current: "build",
			outcome: workflow.StepOutcomeBlocked,
			want:    workflow.Next{Kind: workflow.NextEscalate},
		},
		{
			name:    "explicit edge: plan's needs_fix self-loop re-fires the same step",
			def:     validPlanShape,
			current: "plan",
			outcome: workflow.StepOutcomeNeedsFix,
			want:    workflow.Next{Kind: workflow.NextAdvance, ToStepID: "plan"},
		},
		{
			name:    "explicit edge: audit's needs_fix routes to the fix step, not escalation",
			def:     validAuditLoopShape,
			current: "audit",
			outcome: workflow.StepOutcomeNeedsFix,
			want:    workflow.Next{Kind: workflow.NextAdvance, ToStepID: "fix"},
		},
		{
			name:    "explicit edge: fix's backward ok edge wins over completing at the last step",
			def:     validAuditLoopShape,
			current: "fix",
			outcome: workflow.StepOutcomeOK,
			want:    workflow.Next{Kind: workflow.NextAdvance, ToStepID: "audit"},
		},
		{
			name:    "audit's own ok still advances by default (loop entered only on needs_fix)",
			def:     validAuditLoopShape,
			current: "audit",
			outcome: workflow.StepOutcomeOK,
			want:    workflow.Next{Kind: workflow.NextAdvance, ToStepID: "fix"},
		},
		{
			name: "order gaps: ok advances to the smallest strictly-greater Order",
			def: func() workflow.Definition {
				d := validPlanShape()
				d.Steps[1].Order = 7
				return d
			},
			current: "plan",
			outcome: workflow.StepOutcomeOK,
			want:    workflow.Next{Kind: workflow.NextAdvance, ToStepID: "build"},
		},
		{
			name:    "blocked at a step whose only edges cover other statuses escalates",
			def:     validAuditLoopShape,
			current: "audit",
			outcome: workflow.StepOutcomeBlocked,
			want:    workflow.Next{Kind: workflow.NextEscalate},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := workflow.NextStep(tc.def(), tc.current, tc.outcome)
			if err != nil {
				t.Fatalf("NextStep() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("NextStep() = %+v, want %+v", got, tc.want)
			}
			if got.Kind != workflow.NextAdvance && got.ToStepID != "" {
				t.Errorf("Next.ToStepID = %q on Kind %s, want empty (set iff advancing)", got.ToStepID, got.Kind)
			}
		})
	}
}

func TestNextStep_UnknownStep(t *testing.T) {
	t.Parallel()

	_, err := workflow.NextStep(validPlanShape(), "ghost", workflow.StepOutcomeOK)
	if !errors.Is(err, workflow.ErrUnknownStep) {
		t.Fatalf("error = %v, want errors.Is(err, ErrUnknownStep)", err)
	}
	var unknown *workflow.UnknownStepError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want *UnknownStepError", err)
	}
	if unknown.DefinitionID != "def-plan" || unknown.StepID != "ghost" {
		t.Errorf("UnknownStepError = %+v, want DefinitionID=def-plan StepID=ghost", unknown)
	}
	if unknown.Error() == "" {
		t.Error("UnknownStepError.Error() is empty")
	}
}

func TestNextStep_InvalidOutcome(t *testing.T) {
	t.Parallel()

	// review.Shippable's "needs_human" is the vocabulary most likely to
	// be confused into this axis (§25.4: distinct, never routed through
	// StepOutcomeStatus) -- it must be a typed rejection, never a silent
	// escalation.
	_, err := workflow.NextStep(validPlanShape(), "plan", "needs_human")
	if !errors.Is(err, workflow.ErrInvalidOutcome) {
		t.Fatalf("error = %v, want errors.Is(err, ErrInvalidOutcome)", err)
	}
	var invalid *workflow.InvalidOutcomeError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want *InvalidOutcomeError", err)
	}
	if invalid.StepID != "plan" || invalid.Outcome != "needs_human" {
		t.Errorf("InvalidOutcomeError = %+v, want StepID=plan Outcome=needs_human", invalid)
	}
	if invalid.Error() == "" {
		t.Error("InvalidOutcomeError.Error() is empty")
	}
}

func TestNextStep_DanglingEdge(t *testing.T) {
	t.Parallel()

	def := validPlanShape()
	def.Steps[0].Edges[0].ToStepID = "ghost"

	_, err := workflow.NextStep(def, "plan", workflow.StepOutcomeNeedsFix)
	if !errors.Is(err, workflow.ErrDanglingEdge) {
		t.Fatalf("error = %v, want errors.Is(err, ErrDanglingEdge)", err)
	}
	var dangling *workflow.DanglingEdgeError
	if !errors.As(err, &dangling) {
		t.Fatalf("error = %v, want *DanglingEdgeError", err)
	}
	if dangling.DefinitionID != "def-plan" || dangling.Edge.ToStepID != "ghost" {
		t.Errorf("DanglingEdgeError = %+v, want DefinitionID=def-plan Edge.ToStepID=ghost", dangling)
	}
	if dangling.Error() == "" {
		t.Error("DanglingEdgeError.Error() is empty")
	}

	// The same malformed definition still routes correctly for an
	// outcome whose edge is intact -- the dangling-edge refusal is
	// per-edge defensive, not a whole-definition poison.
	got, err := workflow.NextStep(def, "plan", workflow.StepOutcomeOK)
	if err != nil {
		t.Fatalf("NextStep(ok) error = %v, want nil", err)
	}
	if want := (workflow.Next{Kind: workflow.NextAdvance, ToStepID: "build"}); got != want {
		t.Errorf("NextStep(ok) = %+v, want %+v", got, want)
	}
}

func TestNextKind_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind workflow.NextKind
		want string
	}{
		{workflow.NextAdvance, "advance"},
		{workflow.NextComplete, "complete"},
		{workflow.NextEscalate, "escalate"},
		{workflow.NextKind(99), "NextKind(99)"},
		{workflow.NextKind(-1), "NextKind(-1)"},
	}
	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("NextKind(%d).String() = %q, want %q", int(tc.kind), got, tc.want)
		}
	}
}

// TestNextStepAfterRefusal pins the machine's row for an attempt the
// platform refused before it ran: it escalates on every shape, whatever
// edge the step carries for any outcome -- a blocked self edge, a
// needs_fix self-loop, a forward or backward edge -- where NextStep
// itself would follow the edge.
func TestNextStepAfterRefusal(t *testing.T) {
	t.Parallel()

	blockedSelfEdge := func() workflow.Definition {
		d := validSingleStep()
		d.IsBuiltIn = false
		d.Steps[0].Edges = []workflow.Edge{{FromStepID: "s1", OnStatus: workflow.StepOutcomeBlocked, ToStepID: "s1"}}
		return d
	}
	tests := []struct {
		name    string
		def     func() workflow.Definition
		current workflow.ID
	}{
		{"single step, no edges", validSingleStep, "s1"},
		{"a blocked self edge is never followed", blockedSelfEdge, "s1"},
		{"a needs_fix self-loop is never followed", validPlanShape, "plan"},
		{"a needs_fix forward edge is never followed", validAuditLoopShape, "audit"},
		{"an ok backward edge is never followed", validAuditLoopShape, "fix"},
		{"a middle step never advances by Order", validAuditLoopShape, "build"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := workflow.NextStepAfterRefusal(tc.def(), tc.current)
			if err != nil {
				t.Fatalf("NextStepAfterRefusal() error = %v, want nil", err)
			}
			if got != (workflow.Next{Kind: workflow.NextEscalate}) {
				t.Errorf("NextStepAfterRefusal() = %+v, want an escalation", got)
			}
		})
	}

	if _, err := workflow.NextStepAfterRefusal(validSingleStep(), "ghost"); !errors.Is(err, workflow.ErrUnknownStep) {
		t.Errorf("NextStepAfterRefusal(unknown step) error = %v, want ErrUnknownStep", err)
	}
	if next, _ := workflow.NextStep(blockedSelfEdge(), "s1", workflow.StepOutcomeBlocked); next.Kind != workflow.NextAdvance {
		t.Errorf("control: NextStep on the blocked self edge = %+v, want it followed", next)
	}
}

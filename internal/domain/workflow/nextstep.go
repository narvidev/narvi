package workflow

import (
	"errors"
	"fmt"
)

// NextKind is what the run does after a finished step -- the closed set
// of consequences NextStep can decide.
type NextKind int

const (
	// NextAdvance moves the run to Next.ToStepID -- either an explicit
	// edge's target (which may legally be an earlier step, or the same
	// step, forming a wired retry loop) or the default next step in
	// Order after a StepOutcomeOK.
	NextAdvance NextKind = iota
	// NextComplete ends the run successfully: StepOutcomeOK on the last
	// step in Order with no explicit ok edge.
	NextComplete
	// NextEscalate hands the run to a human: StepOutcomeNeedsFix/
	// StepOutcomeBlocked with no explicit edge wired for it -- §25.4's
	// fail-conservative default. The CONSEQUENCE of escalation
	// (WorkflowRun.Status = needs_review, one notice, stop -- §25.9) is
	// the engine's to apply, not this package's.
	NextEscalate
)

var nextKindNames = [...]string{"advance", "complete", "escalate"}

// String is the name logged for a routing decision -- mirrors
// turn.Trigger.String()'s own out-of-range defensiveness.
func (k NextKind) String() string {
	if k < 0 || int(k) >= len(nextKindNames) {
		return fmt.Sprintf("NextKind(%d)", int(k))
	}
	return nextKindNames[k]
}

// Next is NextStep's verdict. ToStepID is set iff Kind == NextAdvance.
type Next struct {
	Kind     NextKind
	ToStepID ID
}

// ErrUnknownStep is the sentinel NextStep returns when currentStepID
// names no step in the definition -- wrapped by UnknownStepError so
// callers/tests distinguish it via errors.Is while getting full detail
// via errors.As, mirroring turn/plan's own ErrIllegalTransition shape.
var ErrUnknownStep = errors.New("workflow: unknown step")

// UnknownStepError reports a currentStepID NextStep could not find in
// the definition.
type UnknownStepError struct {
	DefinitionID ID
	StepID       ID
}

func (e *UnknownStepError) Error() string {
	return fmt.Sprintf("workflow: unknown step: %q in definition %q", e.StepID, e.DefinitionID)
}

func (e *UnknownStepError) Unwrap() error { return ErrUnknownStep }

// ErrInvalidOutcome is the sentinel NextStep returns for an outcome
// outside the closed StepOutcomeStatus vocabulary -- never silently
// coerced to an escalation, since a foreign value here is a caller bug
// (the outcome-posting endpoint validates the closed set before
// anything persists), not a legitimate "blocked".
var ErrInvalidOutcome = errors.New("workflow: invalid step outcome status")

// InvalidOutcomeError reports the (step, outcome) NextStep rejected.
type InvalidOutcomeError struct {
	StepID  ID
	Outcome StepOutcomeStatus
}

func (e *InvalidOutcomeError) Error() string {
	return fmt.Sprintf("workflow: invalid step outcome status: %q at step %q", e.Outcome, e.StepID)
}

func (e *InvalidOutcomeError) Unwrap() error { return ErrInvalidOutcome }

// ErrDanglingEdge is the sentinel NextStep returns when the explicit
// edge it matched targets a step not present in the definition -- a
// malformed definition ValidateDefinition would have rejected
// (ErrEdgeUnknownTarget), refused here DEFENSIVELY too rather than
// silently escalating or advancing nowhere, mirroring the "default case
// is unreachable dead-code protection" stance authz's ErrUnknownAction
// documents.
var ErrDanglingEdge = errors.New("workflow: edge targets a step not in this definition")

// DanglingEdgeError reports the edge whose target NextStep could not
// resolve.
type DanglingEdgeError struct {
	DefinitionID ID
	Edge         Edge
}

func (e *DanglingEdgeError) Error() string {
	return fmt.Sprintf("workflow: edge targets a step not in this definition: %q -> %q on %q in definition %q",
		e.Edge.FromStepID, e.Edge.ToStepID, e.Edge.OnStatus, e.DefinitionID)
}

func (e *DanglingEdgeError) Unwrap() error { return ErrDanglingEdge }

// NextStep is THE single pure decision function for what follows a
// finished step (§25.4) -- the same single-authority shape as
// turn.Transition/plan.Transition/sandbox.Transition, with the
// transition table carried as the definition's own data (Steps + Edges)
// instead of a package-level map, since every definition is its own
// machine. Given the definition, the step that just finished, and the
// StepOutcomeStatus it posted:
//
//  1. An explicit Edge on (currentStepID, outcome) always wins:
//     NextAdvance to its target -- including backward/self targets
//     (§25.9's Edge{audit, needs_fix, fix} + Edge{fix, ok, audit} loop
//     is two ordinary edges evaluated right here, no separate loop
//     mechanism). Whether a re-firing needs_fix edge is ALLOWED to
//     re-fire is the engine's loopguard consultation (§25.9), layered
//     on top of -- never inside -- this function.
//  2. No explicit edge: StepOutcomeOK advances to the step with the
//     smallest Order strictly greater than the current step's, or
//     NextComplete when none exists; StepOutcomeNeedsFix/
//     StepOutcomeBlocked yield NextEscalate (§25.4: fail-conservative
//     -- "a retry loop must be wired explicitly, never implied").
//
// Every illegal input (unknown step, foreign outcome, dangling edge
// target) returns a typed error, never a zero-value Next silently
// accepted -- mirroring Transition's own discipline. NextStep assumes a
// ValidateDefinition-clean definition but still refuses, rather than
// trusting, the two malformations that would misroute a run.
func NextStep(def Definition, currentStepID ID, outcome StepOutcomeStatus) (Next, error) {
	current, ok := stepByID(def, currentStepID)
	if !ok {
		return Next{}, &UnknownStepError{DefinitionID: def.ID, StepID: currentStepID}
	}
	if !IsValidStepOutcomeStatus(outcome) {
		return Next{}, &InvalidOutcomeError{StepID: currentStepID, Outcome: outcome}
	}

	for _, edge := range current.Edges {
		if edge.OnStatus != outcome {
			continue
		}
		if _, ok := stepByID(def, edge.ToStepID); !ok {
			return Next{}, &DanglingEdgeError{DefinitionID: def.ID, Edge: edge}
		}
		return Next{Kind: NextAdvance, ToStepID: edge.ToStepID}, nil
	}

	if outcome == StepOutcomeOK {
		if next, ok := nextInOrder(def, current.Order); ok {
			return Next{Kind: NextAdvance, ToStepID: next}, nil
		}
		return Next{Kind: NextComplete}, nil
	}

	return Next{Kind: NextEscalate}, nil
}

// NextStepAfterRefusal is the machine's row for an attempt the platform
// refused before it ever ran -- its model could run only on a credential
// the session never resolves, say (technical plan §29.4). A refusal is not
// a StepOutcomeStatus the step reported: the step never ran, so no Edge
// conditions on it (Edges condition only on StepOutcomeStatus, §25.4), and
// retrying cannot succeed until a person changes the configuration. So the
// run always escalates to a human (NextEscalate, §25.4's fail-conservative
// consequence), whatever edges the definition wires -- a blocked or
// needs_fix edge, a self edge included, is never followed: following one
// would queue the same refused attempt again, a retry loop with nothing to
// end it. Unknown steps are refused exactly as NextStep refuses them.
func NextStepAfterRefusal(def Definition, currentStepID ID) (Next, error) {
	if _, ok := stepByID(def, currentStepID); !ok {
		return Next{}, &UnknownStepError{DefinitionID: def.ID, StepID: currentStepID}
	}
	return Next{Kind: NextEscalate}, nil
}

// stepByID finds the step with the given id. A linear scan: a
// definition's step count is small by construction (the largest shape
// §25 names is 4 steps), so no index map is worth building per call.
func stepByID(def Definition, id ID) (StepDefinition, bool) {
	for _, step := range def.Steps {
		if step.ID == id {
			return step, true
		}
	}
	return StepDefinition{}, false
}

// nextInOrder returns the id of the step with the smallest Order
// strictly greater than after -- well-defined even with Order gaps (see
// ValidateDefinition's own contiguity note). In a validated definition
// orders are unique, so there is never a tie to break.
func nextInOrder(def Definition, after int) (ID, bool) {
	var (
		bestID    ID
		bestOrder int
		found     bool
	)
	for _, step := range def.Steps {
		if step.Order <= after {
			continue
		}
		if !found || step.Order < bestOrder {
			bestID, bestOrder, found = step.ID, step.Order, true
		}
	}
	return bestID, found
}

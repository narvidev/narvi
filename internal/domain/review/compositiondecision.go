package review

import "errors"

// This file (compositiondecision.go) implements §12.2 item 9's own two
// composition-finding actions ("Block release / Acknowledge & ship") as
// an explicit, table-driven transition -- CLAUDE.md/§11's own "every state
// transition goes through the machine's transition table" rule, applied
// here at the scope this decision actually needs: one non-terminal state,
// two legal, both-terminal actions out of it. This mirrors internal/domain/
// turn.transitions/internal/domain/sandbox.transitions' own "explicit map,
// never inferred from declaration order" shape, sized down: §15's own
// phasing note is explicit that this Step needs "no new domain package, no
// new state machine" (§15.5) -- this is a small transition TABLE living
// inside the existing review domain package, not a new package or a new
// machine abstraction.
//
// CompositionDecision/CompositionDecisionAction are deliberately separate
// from Shippable/ProposedShippable (this package's OTHER classification,
// shippable.go): Shippable is the server's own authoritative,
// automatically COMPUTED read of a per-PR risk-map verdict.
// CompositionDecision is never computed by this package (or any domain
// function) at all -- it is a human's own decision, recorded verbatim
// against a release's composition findings; TransitionCompositionDecision
// below only validates which decision may legally follow which, exactly
// like turn.Transition/sandbox.Transition validate a state change without
// ever originating one themselves.

// CompositionDecision is the human verdict on §15.3's own composition
// findings for one release review. Pending is the only non-terminal
// value -- a real, meaningful "not yet decided" state (distinct from an
// unrecognized/zero value, per this package's own uniform "an unset
// field is never confused with a real value" discipline, doc.go), never
// this type's Go zero value ("").
type CompositionDecision string

const (
	// CompositionDecisionPending is every composition review's own
	// starting state -- set the moment its findings are first persisted
	// (§12.2 item 9: findings arrive before any human has acted on them).
	CompositionDecisionPending CompositionDecision = "pending"
	// CompositionDecisionBlocked is the human "Block release" verdict --
	// terminal: TransitionCompositionDecision (below) accepts no further
	// action once a decision has reached this state.
	CompositionDecisionBlocked CompositionDecision = "blocked"
	// CompositionDecisionAcknowledged is the human "Acknowledge & ship"
	// verdict -- an explicit override of a composition finding, terminal
	// exactly like CompositionDecisionBlocked.
	CompositionDecisionAcknowledged CompositionDecision = "acknowledged"
)

// CompositionDecisionAction is one of the three §12.2 item 9 actions a
// human may take against a release's own composition findings.
type CompositionDecisionAction string

const (
	// CompositionDecisionActionBlock is the "Block release" action.
	CompositionDecisionActionBlock CompositionDecisionAction = "block"
	// CompositionDecisionActionAcknowledge is the "Acknowledge & ship"
	// action -- an explicit override of an already-computed composition
	// finding (§13.3: overrides in this codebase are admin-gated; see
	// internal/domain/authz.ActionAcknowledgeReleaseComposition's own doc
	// comment for the RBAC placement this decision's caller enforces --
	// this package itself carries no RBAC concept at all, §11).
	CompositionDecisionActionAcknowledge CompositionDecisionAction = "acknowledge"
	// CompositionDecisionActionUnblock is the "Unblock" action -- a
	// confirmed-major fix: a maintainer's Block used to be TERMINAL in
	// this table (no entry existed for CompositionDecisionBlocked as a
	// starting state at all), which meant the strictly MORE privileged
	// admin-only Acknowledge & ship (CompositionDecisionActionAcknowledge)
	// could never run once a maintainer had blocked -- a
	// lower-privileged action a higher-privileged one could not undo,
	// inverting this system's own privilege gate everywhere else (an
	// admin can always do at least what a maintainer can). Unblock
	// reopens a Blocked decision back to Pending -- never straight to
	// Acknowledged, so the SAME admin-only Acknowledge & ship action still
	// has to be exercised explicitly afterward, keeping the "acknowledging
	// a real finding is its own deliberate act" property this decision's
	// own doc comment already establishes for the forward direction. See
	// internal/domain/authz.ActionUnblockReleaseComposition's own doc
	// comment for why this is gated admin-only, not maintainer+ -- undoing
	// a safety-additive action is itself a risk-accepting one.
	CompositionDecisionActionUnblock CompositionDecisionAction = "unblock"
)

// ErrIllegalCompositionDecisionTransition is TransitionCompositionDecision's
// own sentinel error -- returned for any (current, action) pair not
// present in compositionDecisionTransitions below: an action attempted
// against an already-decided (Blocked/Acknowledged) composition, or an
// unrecognized current value (this type's Go zero value, "", included --
// mirrors this package's own uniform fail-conservative-enum discipline,
// doc.go: an unset/garbled CompositionDecision is refused, never silently
// treated as Pending).
var ErrIllegalCompositionDecisionTransition = errors.New("review: illegal composition decision transition")

// compositionDecisionTransitions is this decision's own explicit
// transition table -- see this file's own top doc comment for why an
// explicit map, not inferred logic. CompositionDecisionAcknowledged
// still has NO entries as a starting state (unchanged): acknowledging is
// this table's own genuinely terminal state -- an admin has already
// exercised the strictest override this decision offers, and there is no
// further action left for ANY role to take against it. CompositionDecisionBlocked,
// by contrast, is NOT fully terminal (confirmed-major fix, this file's
// own top doc comment): CompositionDecisionActionUnblock reopens it back
// to Pending, so the admin-only Acknowledge & ship path remains reachable
// even after a maintainer's Block -- a later composition review pass (a
// fresh release_manifest_checks row) is unaffected either way, since that
// always starts its own fresh row at CompositionDecisionPending
// regardless of what this table does with an EXISTING row's decision.
var compositionDecisionTransitions = map[CompositionDecision]map[CompositionDecisionAction]CompositionDecision{
	CompositionDecisionPending: {
		CompositionDecisionActionBlock:       CompositionDecisionBlocked,
		CompositionDecisionActionAcknowledge: CompositionDecisionAcknowledged,
	},
	CompositionDecisionBlocked: {
		CompositionDecisionActionUnblock: CompositionDecisionPending,
	},
}

// TransitionCompositionDecision reports the CompositionDecision current
// transitions to when action is applied, or
// ErrIllegalCompositionDecisionTransition when no such transition is
// legal for that (current, action) pair -- the caller (internal/adapters/
// inbound/httpapi's own Block/Acknowledge handlers) is expected to
// persist the returned value via a guarded UPDATE comparing against the
// SAME current value passed in here, so a concurrent decision loses the
// race with an honest 409, never a silent overwrite.
func TransitionCompositionDecision(current CompositionDecision, action CompositionDecisionAction) (CompositionDecision, error) {
	if next, ok := compositionDecisionTransitions[current][action]; ok {
		return next, nil
	}
	return "", ErrIllegalCompositionDecisionTransition
}

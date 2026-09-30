package reviewcheck

import "github.com/narvidev/narvi/internal/domain/providercredential"

// NotAssessedReason names why an attempt ended with no verdict, when the
// control plane refused the attempt itself before it ran and so knows why.
// The zero value is the common case: the attempt ran and ended without a
// verdict, for a reason that lives in its session, and the check says only
// that it did not complete (ComputeOutput).
type NotAssessedReason string

const (
	// NotAssessedPersonalLinkOnly means the review's model is available here
	// only through a person's own provider link, which a pull request's
	// review never runs on (technical plan §29.4). The same name as the
	// refused turn's own reason, providercredential.RefusalPersonalLinkOnly.
	NotAssessedPersonalLinkOnly = NotAssessedReason(providercredential.RefusalPersonalLinkOnly)

	// NotAssessedRolloutNotEnrolled means the review's sandbox could not be
	// started because the pull request's repository is not enrolled in
	// this deployment's cohort rollout (technical plan §32.4): the spawn
	// was refused and the attempt ended before it ran.
	NotAssessedRolloutNotEnrolled NotAssessedReason = "rollout_not_enrolled"

	// NotAssessedSubstrateUnsupported means the review's sandbox could not
	// be started because the deployment's sandbox provider cannot give
	// the session's environment what it requires -- Docker, or enforced
	// egress (technical plan §27.5): the spawn was refused and the attempt
	// ended before it ran.
	NotAssessedSubstrateUnsupported NotAssessedReason = "substrate_unsupported"
)

// notAssessedExplanations is the sentence each named reason adds to a
// not-assessed check's summary. A reason absent from it (the zero value,
// or one a newer binary named during a rolling deploy) adds nothing.
var notAssessedExplanations = map[NotAssessedReason]string{
	NotAssessedPersonalLinkOnly: "Its model is available here only through a person's own provider link, " +
		"and a pull request's review never runs on one. An admin can add a deployment credential " +
		"for the model's provider, or the review can use another model.",
	NotAssessedRolloutNotEnrolled: "Its repository is not enrolled in this deployment's rollout, so no sandbox " +
		"could be started for it. An admin can enroll the repository, then request the review again.",
	NotAssessedSubstrateUnsupported: "The deployment's sandbox provider cannot give this session's environment " +
		"what it requires (Docker, or enforced egress), so no sandbox could be started for it. An admin can " +
		"change the provider or the environment, then request the review again.",
}

// ComputeOutputWithReason is ComputeOutput, with a not-assessed check's
// summary extended by what reason names -- the caller-side enrichment
// Output's own doc comment allows: it appends, never replaces what
// ComputeOutput decided, and every other Phase ignores reason.
func ComputeOutputWithReason(p Phase, reason NotAssessedReason) Output {
	out := ComputeOutput(p)
	if p != PhaseTerminalNotAssessed {
		return out
	}
	if explanation, ok := notAssessedExplanations[reason]; ok {
		out.Summary += " " + explanation
	}
	return out
}

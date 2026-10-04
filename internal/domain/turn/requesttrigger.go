package turn

// The values of turns.request_trigger (migrations/000159, 000160): which
// lane asked for a turn, for the lanes that record it. A turn whose lane
// records no trigger has none (NULL).
const (
	// RequestTriggerAuto is a turn the automatic re-review asked for
	// (technical plan §24.3, §24.9): a push left a head pending and the
	// debounce's firing inserted a review attempt for it.
	RequestTriggerAuto = "auto"
	// RequestTriggerLabel is a person's request through the configured
	// GitHub re-review label (§8.2).
	RequestTriggerLabel = "label"
	// RequestTriggerButton is a person's request through the web re-review
	// button (§8.2, §12.2).
	RequestTriggerButton = "button"
	// RequestTriggerMention is a person's request through a mention of the
	// bot on the pull request (§8.2).
	RequestTriggerMention = "mention"
)

// IsHumanRequestTrigger reports whether requestTrigger names one of a
// person's three request lanes -- the label, the button, a mention --
// whose review attempt, when it meets a moved context at dispatch, is owed
// to its requester rather than asked again of the automatic lane
// (technical plan §24.9).
func IsHumanRequestTrigger(requestTrigger *string) bool {
	if requestTrigger == nil {
		return false
	}
	switch *requestTrigger {
	case RequestTriggerLabel, RequestTriggerButton, RequestTriggerMention:
		return true
	default:
		return false
	}
}

// ContextCheckedAtDispatch reports whether a turn is one technical plan
// §24.9's context check applies to, when it is dispatched after waiting
// behind another turn: a review attempt a lane that records its trigger
// asked for -- the automatic re-review, whose moved attempt goes back to
// that lane, or a person's request, whose moved attempt is owed to its
// requester. Every other turn -- a follow-up, a plan, a build, or an
// attempt an older binary inserted with no trigger -- is never checked.
func ContextCheckedAtDispatch(isReviewAttempt bool, requestTrigger *string) bool {
	if !isReviewAttempt || requestTrigger == nil {
		return false
	}
	return *requestTrigger == RequestTriggerAuto || IsHumanRequestTrigger(requestTrigger)
}

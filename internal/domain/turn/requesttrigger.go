package turn

// RequestTriggerAuto is turns.request_trigger (migrations/000159) of a
// turn the automatic re-review asked for (technical plan §24.3, §24.9): a
// push left a head pending and the debounce's firing inserted a review
// attempt for it. A turn whose lane records no trigger has none (NULL).
const RequestTriggerAuto = "auto"

// ContextCheckedAtDispatch reports whether a turn is one technical plan
// §24.9's context check applies to, when it is dispatched after waiting
// behind another turn: a review attempt the automatic re-review asked for.
// A person's review request (the label, the button, a mention) dispatches
// unchecked until a moved one can be owed to its requester rather than
// lost; every other turn -- a follow-up, a plan, a build -- is no review
// attempt and is never checked.
func ContextCheckedAtDispatch(isReviewAttempt bool, requestTrigger *string) bool {
	return isReviewAttempt && requestTrigger != nil && *requestTrigger == RequestTriggerAuto
}

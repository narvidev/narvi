package identitylink

// Decide renders §13.2's own auto-link step-2/3/4 verdict given
// matchedUserIDs -- the caller's own already-deduplicated set of user ids
// found to match a fetched provider profile email through a verified
// identities.email its provider confirmed recently (users.primary_email is
// no evidence on its own) -- this function trusts the input is already
// deduplicated and does not re-check for duplicates, since "the same user
// id appears twice" is a caller-side query-shape question this package
// has no way to detect from a plain slice of opaque id strings alone.
//
// Exactly one distinct id -> (that id, true): auto-link it. Zero, or more
// than one -> ("", false): never guess -- §13.2's own explicit rule,
// which is why this function does NOT attempt any tie-breaking (e.g.
// "prefer the oldest account") on the multiple-match branch. userIDs are
// plain, opaque strings (never pgtype.UUID) -- mirrors authz.Actor.UserID's
// own identical "adapter-independent, caller converts at the boundary"
// precedent (§11).
func Decide(matchedUserIDs []string) (userID string, ok bool) {
	if len(matchedUserIDs) == 1 {
		return matchedUserIDs[0], true
	}
	return "", false
}

package reviewverdict

// ReviewState is where one pull request's review stands, read from the
// verdicts and review attempts on record -- never re-derived from anything
// else (row 182, technical plan §43.20; §21.1b's "no consumer re-derives").
type ReviewState string

// The four ReviewState values.
const (
	// ReviewAbsent: no verdict is on record and no review attempt has run.
	ReviewAbsent ReviewState = "absent"
	// ReviewInProgress: the newest review attempt has not ended.
	ReviewInProgress ReviewState = "in_progress"
	// ReviewNotAssessed: the newest review attempt ended without posting a
	// verdict (§21.1's amendment: "not_assessed as a first-class outcome").
	ReviewNotAssessed ReviewState = "not_assessed"
	// ReviewAssessed: the newest review attempt posted the verdict that
	// answers for the pull request.
	ReviewAssessed ReviewState = "assessed"
)

// Attempt is the newest review attempt on record for a pull request -- the
// newest turn with is_review_attempt set in its review session -- as a
// caller read it: its turn id, whether it has reached a terminal state, and
// whether any verdict was ever posted for it (ReviewVerdictStore.
// ExistsForAttempt), which is only asked of a terminal attempt.
type Attempt struct {
	ID       string
	Terminal bool
	Posted   bool
}

// ReviewStatus is DeriveReviewStatus's answer: the state, the verdict that
// answers for the pull request (only when assessed), and the latest verdict
// of an earlier attempt that a newer one has not confirmed (in progress or
// not assessed).
type ReviewStatus struct {
	State      ReviewState
	Verdict    *Record
	Superseded *Record
}

// DeriveReviewStatus reads a pull request's review state from its latest
// verdict on record (latest: internal/app/reviewverdict.GetLatestRecord,
// ordered by the producing attempt's creation, exactly what the merge path
// reads; nil when none) and its newest review attempt (newest: nil when no
// attempt is on record).
//
//   - Neither: absent.
//   - A verdict and no attempt on record (a verdict recorded before attempts
//     were, whose attempt_id is empty): assessed, on that verdict.
//   - The newest attempt has not ended: in progress. A verdict it has
//     already posted is not reported until it ends; the latest verdict of an
//     earlier attempt is reported as superseded.
//   - It ended and posted nothing: not assessed, even when an older verdict
//     exists -- that verdict is superseded, never the current answer (the
//     mechanism acceptance.go's finding F1 closed for acceptances).
//   - It ended and posted: assessed, on the latest verdict.
//
// A newest attempt that posted while no verdict is on record cannot occur
// in one snapshot; it is answered not assessed, so assessed is never
// reported without a verdict to show.
func DeriveReviewStatus(latest *Record, newest *Attempt) ReviewStatus {
	switch {
	case newest == nil && latest == nil:
		return ReviewStatus{State: ReviewAbsent}
	case newest == nil:
		return ReviewStatus{State: ReviewAssessed, Verdict: latest}
	case !newest.Terminal:
		return ReviewStatus{State: ReviewInProgress, Superseded: earlierAttempts(latest, newest.ID)}
	case !newest.Posted || latest == nil:
		return ReviewStatus{State: ReviewNotAssessed, Superseded: earlierAttempts(latest, newest.ID)}
	default:
		return ReviewStatus{State: ReviewAssessed, Verdict: latest}
	}
}

// earlierAttempts is latest when an earlier attempt (or no recorded
// attempt) produced it, and nil when it is attemptID's own.
func earlierAttempts(latest *Record, attemptID string) *Record {
	if latest == nil || (latest.AttemptID != "" && latest.AttemptID == attemptID) {
		return nil
	}
	return latest
}

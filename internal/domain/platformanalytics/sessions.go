package platformanalytics

import (
	"sort"
	"time"

	"github.com/narvidev/narvi/internal/domain/session"
)

// OutcomeCount is one (day, status, failure_reason) -> count row --
// internal/app/platformanalytics's own pure conversion of one
// sqlcgen.ListSessionOutcomeCountsInWindowRow, and the single shared
// input every reduction in this file operates over (mirrors internal/
// domain/reviewverdict.Record's own "one fetch, several pure reductions"
// role). Day is already truncated to a UTC calendar day midnight by the
// caller's own SQL (queries/sessions.sql); this package trusts that and
// does no further truncation of its own.
type OutcomeCount struct {
	Day           time.Time
	Status        session.Status
	FailureReason session.FailureReason // "" when the row's own failure_reason was NULL
	Count         int
}

// TotalSessions is §12.2 item 6's own "Sessions" KPI tile: every session row
// created within the window, regardless of status -- INCLUDING
// session.StatusCreated (a session created but never dispatched a single
// turn, internal/domain/session.DeriveStatus's own "zero turns: Created"
// case). This is a deliberate reading of "how many sessions" as "how many
// session rows this deployment created", not "how many sessions did
// meaningful work" -- the latter is what SuccessRate's own denominator
// answers instead, over a narrower, explicitly-named subset. A real,
// meaningful answer whenever counts was actually fetched (see doc.go's
// own "COUNT is always real -- PROVIDED the fetch that produced it
// succeeded" note) -- no ok return here, unlike every other function in
// this file, because the caller (httpapi.GetPlatformAnalytics) already
// knows whether that fetch succeeded and gates its OWN
// sessionsTotalComputed sentinel on that, not on anything this pure
// function could tell it.
func TotalSessions(counts []OutcomeCount) int {
	total := 0
	for _, c := range counts {
		total += c.Count
	}
	return total
}

// DayBucket is one UTC calendar day's own per-status session counts --
// SessionsPerDay's own per-day output row, mirroring
// reviewverdict.DayBucket's identical shape one level up (session status
// instead of review.Shippable).
type DayBucket struct {
	Day    time.Time
	Counts map[session.Status]int
}

// SessionsPerDay buckets counts by day, summing across failure_reason for
// each (day, status) pair -- §12.2 item 6's own "sessions per day by
// outcome" chart. A (day, status) pair with zero sessions is simply
// absent from that day's own Counts map, never a zero entry -- mirrors
// reviewverdict.Timeseries' identical convention exactly.
//
// ok=false for an empty counts slice -- "not yet computed" (no session
// has ever been created in the window), distinct from a real, computed
// set of buckets.
func SessionsPerDay(counts []OutcomeCount) ([]DayBucket, bool) {
	if len(counts) == 0 {
		return nil, false
	}

	byDay := make(map[time.Time]map[session.Status]int)
	var order []time.Time
	for _, c := range counts {
		day := byDay[c.Day]
		if day == nil {
			day = make(map[session.Status]int)
			byDay[c.Day] = day
			order = append(order, c.Day)
		}
		day[c.Status] += c.Count
	}

	sort.Slice(order, func(i, j int) bool { return order[i].Before(order[j]) })

	buckets := make([]DayBucket, len(order))
	for i, day := range order {
		buckets[i] = DayBucket{Day: day, Counts: byDay[day]}
	}
	return buckets, true
}

// SuccessRate is §12.2 item 6's own "success rate" KPI: the fraction of
// DEFINITIVELY-RESOLVED sessions (status Completed or Failed) that
// resolved Completed, as a 0-100 percentage. Deliberately EXCLUDES
// session.StatusCancelled (a human-initiated stop, not a system judgment
// of success or failure -- counting it against either side would blame
// or credit the system for a decision a person made) and
// session.StatusCreated/StatusActive (no outcome has happened yet to
// judge) from BOTH the numerator and the denominator -- mirrors §12.2
// item 6's own "Review finding outcomes" precedent one section up:
// "precision computed only over definitively-resolved findings", the
// identical principle applied here to sessions instead of findings.
//
// ok=false when the denominator (completed+failed) is zero -- a
// deployment that has created sessions but had none resolve yet (all
// still Active/Created, or all merely Cancelled) has no success rate to
// report, and reporting 0% or 100% in that state would be exactly the
// "lie of omission" the KPI's own honesty requirement forbids: a
// brand-new deployment must never show a misleadingly perfect OR
// misleadingly failing rate it has no evidence for.
func SuccessRate(counts []OutcomeCount) (ratePercent float64, sampleSize int, ok bool) {
	var completed, failed int
	for _, c := range counts {
		switch c.Status {
		case session.StatusCompleted:
			completed += c.Count
		case session.StatusFailed:
			failed += c.Count
		}
	}
	denominator := completed + failed
	if denominator == 0 {
		return 0, 0, false
	}
	return float64(completed) / float64(denominator) * 100, denominator, true
}

// FailureReasonCount is one session.FailureReason's own occurrence count
// across the window -- TopFailureReasons' own per-reason output row.
type FailureReasonCount struct {
	Reason session.FailureReason
	Count  int
}

// TopFailureReasons counts session.FailureReason occurrences across
// every session whose status is Failed or Cancelled (the only two
// statuses session.DeriveStatus ever attaches a FailureReason to) --
// §12.2 item 6's own "top failure reasons" chart, restricted to the
// four REAL typed reasons this deployment's own schema actually
// persists (session_failure_reason: cancelled/failed/timeout/
// never_started -- internal/domain/turn.FailureReason's own doc comment)
// rather than the free-form, more granular examples the mockup itself
// draws ("heartbeat_silence", "provider_cold_start", ...): this
// deployment's rows do not carry that granularity, and inventing labels
// this system cannot actually attribute a session to would be exactly
// the kind of number nobody can trust this Step exists to eliminate.
//
// Sorted by count descending, tie-broken alphabetically by reason -- the
// same deterministic-ordering discipline reviewverdict.TopRiskDrivers
// already establishes.
//
// ok=false only when counts itself is empty (no session data in the
// window at all) -- a non-nil, EMPTY result with ok=true is itself a
// real, computed answer ("sessions existed, none failed or were
// cancelled"), mirroring reviewverdict.TopRiskDrivers' own identical
// "real verdicts, none tagged a risk driver" distinction.
func TopFailureReasons(counts []OutcomeCount) ([]FailureReasonCount, bool) {
	if len(counts) == 0 {
		return nil, false
	}

	byReason := make(map[session.FailureReason]int)
	for _, c := range counts {
		if c.Status != session.StatusFailed && c.Status != session.StatusCancelled {
			continue
		}
		if c.FailureReason == "" {
			continue
		}
		byReason[c.FailureReason] += c.Count
	}
	if len(byReason) == 0 {
		return nil, true
	}

	result := make([]FailureReasonCount, 0, len(byReason))
	for reason, count := range byReason {
		result = append(result, FailureReasonCount{Reason: reason, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Reason < result[j].Reason
	})
	return result, true
}

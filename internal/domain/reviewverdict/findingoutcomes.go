package reviewverdict

import (
	"sort"

	"github.com/narvidev/narvi/internal/domain/reviewpost"
)

// FindingStatusCount is one reviewpost.FindingStatus's own occurrence
// count across a window's worth of review_findings rows -- FindingOutcomes'
// own per-status output row.
type FindingStatusCount struct {
	Status reviewpost.FindingStatus
	Count  int
}

// FindingOutcomes counts statuses across statuses -- §21.1/§12.2 item 6's
// own "Review finding outcomes" KPI, read from review_findings (§8.2),
// the durable, per-finding MUTABLE-status history review_verdicts
// (append-only, no per-finding status of its own) never carries -- see
// migrations/000046_review_findings.up.sql's own doc comment for why a
// finding's status lives there and not on the verdict. Deliberately the
// RAW reviewpost.FindingStatus distribution (open/rebutted/fix_pending/
// fix_open/fix_merged/fix_applied) rather than a collapsed
// accepted/rebutted/dismissed 3-tier label: that collapsing is a
// PRESENTATION decision (mockups.html's own "Review finding outcomes"
// tile), left to whichever future work actually builds that UI (§14.4, out
// of this file's own scope) rather than guessed at here.
//
// Sorted by Count descending, tie-broken alphabetically by Status, the
// SAME deterministic-ordering discipline TopRiskDrivers already
// establishes for an analogous map-shaped rollup.
//
// ok=false for an empty statuses slice, mirroring Timeseries/
// TopRiskDrivers' own identical "not yet computed" sentinel.
func FindingOutcomes(statuses []reviewpost.FindingStatus) ([]FindingStatusCount, bool) {
	if len(statuses) == 0 {
		return nil, false
	}

	counts := make(map[reviewpost.FindingStatus]int)
	for _, s := range statuses {
		counts[s]++
	}

	result := make([]FindingStatusCount, 0, len(counts))
	for status, count := range counts {
		result = append(result, FindingStatusCount{Status: status, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Status < result[j].Status
	})
	return result, true
}

// FindingOutcomeRow is one review_findings row as the finding outcomes KPI
// reads it (§26.5/§26.6's amendment): its status, the source its latest
// publication reported ("" when none was recorded -- a row last published
// before migrations/000154), and the server's addition check.
type FindingOutcomeRow struct {
	Status        reviewpost.FindingStatus
	Source        reviewpost.FindingSource
	AdditionCheck reviewpost.AdditionCheck
}

// FindingSourceBucket is the source a finding is counted under in the
// per-source breakdown: the reported source, with a counter-review
// addition split by whether the server counted it as checked, and a row
// with no source recorded in a bucket of its own.
type FindingSourceBucket string

// The FindingSourceBucket values, in the order the breakdown lists them.
const (
	FindingSourceBucketPrimary                 FindingSourceBucket = "primary"
	FindingSourceBucketCounterReview           FindingSourceBucket = "counter_review"
	FindingSourceBucketCounterReviewUnverified FindingSourceBucket = "counter_review_unverified"
	FindingSourceBucketNotRecorded             FindingSourceBucket = "not_recorded"
)

// findingSourceBucketOrder ranks the buckets for a deterministic listing.
var findingSourceBucketOrder = map[FindingSourceBucket]int{
	FindingSourceBucketPrimary:                 0,
	FindingSourceBucketCounterReview:           1,
	FindingSourceBucketCounterReviewUnverified: 2,
	FindingSourceBucketNotRecorded:             3,
}

// Bucket is the source row is counted under. A source this package does
// not know is "not recorded": the column is only ever written from the
// validated vocabulary, so such a value says nothing trustworthy about the
// pass that produced the finding.
func (row FindingOutcomeRow) Bucket() FindingSourceBucket {
	switch row.Source {
	case reviewpost.FindingSourcePrimary:
		return FindingSourceBucketPrimary
	case reviewpost.FindingSourceCounterReview:
		if row.AdditionCheck.Checked() {
			return FindingSourceBucketCounterReview
		}
		return FindingSourceBucketCounterReviewUnverified
	default:
		return FindingSourceBucketNotRecorded
	}
}

// FindingSourceStatusCount is one (source bucket, status) pair's count --
// FindingOutcomesBySource's output row.
type FindingSourceStatusCount struct {
	Source FindingSourceBucket
	Status reviewpost.FindingStatus
	Count  int
}

// VerifiedStatuses returns the status of every row that is not an
// unverified counter-review addition -- the input FindingOutcomes counts,
// so an addition the server could not count as checked is counted apart
// (§26.6's amendment) and never mixed into the KPI's main distribution. A
// row with no source recorded stays in it, as it always was: nothing says
// it was an addition.
func VerifiedStatuses(rows []FindingOutcomeRow) []reviewpost.FindingStatus {
	statuses := make([]reviewpost.FindingStatus, 0, len(rows))
	for _, row := range rows {
		if row.Bucket() == FindingSourceBucketCounterReviewUnverified {
			continue
		}
		statuses = append(statuses, row.Status)
	}
	return statuses
}

// FindingOutcomesBySource counts rows per source bucket and status -- the
// breakdown §26.5 reads precision per source from (the share of a
// source's findings a maintainer rebutted), and the one place an
// unverified addition is counted, apart from the rest. Sorted by bucket
// (FindingSourceBucket's own order), then Count descending, then Status,
// the same deterministic discipline FindingOutcomes uses. Empty rows
// return nil.
func FindingOutcomesBySource(rows []FindingOutcomeRow) []FindingSourceStatusCount {
	if len(rows) == 0 {
		return nil
	}
	type key struct {
		source FindingSourceBucket
		status reviewpost.FindingStatus
	}
	counts := make(map[key]int)
	for _, row := range rows {
		counts[key{row.Bucket(), row.Status}]++
	}
	result := make([]FindingSourceStatusCount, 0, len(counts))
	for k, n := range counts {
		result = append(result, FindingSourceStatusCount{Source: k.source, Status: k.status, Count: n})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Source != result[j].Source {
			return findingSourceBucketOrder[result[i].Source] < findingSourceBucketOrder[result[j].Source]
		}
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Status < result[j].Status
	})
	return result
}

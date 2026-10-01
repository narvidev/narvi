package reviewverdict_test

import (
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

func TestFindingOutcomes_EmptyIsNotYetComputed(t *testing.T) {
	t.Parallel()

	outcomes, ok := reviewverdict.FindingOutcomes(nil)
	if ok {
		t.Fatalf("FindingOutcomes(nil) ok = true, want false (not yet computed)")
	}
	if outcomes != nil {
		t.Fatalf("FindingOutcomes(nil) outcomes = %v, want nil", outcomes)
	}
}

func TestFindingOutcomes_CountsAndOrdersDeterministically(t *testing.T) {
	t.Parallel()

	statuses := []reviewpost.FindingStatus{
		reviewpost.FindingStatusOpen,
		reviewpost.FindingStatusOpen,
		reviewpost.FindingStatusRebutted,
		reviewpost.FindingStatusFixApplied,
	}

	outcomes, ok := reviewverdict.FindingOutcomes(statuses)
	if !ok {
		t.Fatalf("FindingOutcomes(statuses) ok = false, want true")
	}
	if len(outcomes) != 3 {
		t.Fatalf("FindingOutcomes(statuses) = %d statuses, want 3", len(outcomes))
	}

	want := []reviewverdict.FindingStatusCount{
		{Status: reviewpost.FindingStatusOpen, Count: 2},
		{Status: reviewpost.FindingStatusFixApplied, Count: 1},
		{Status: reviewpost.FindingStatusRebutted, Count: 1},
	}
	for i, w := range want {
		if outcomes[i] != w {
			t.Errorf("outcomes[%d] = %+v, want %+v", i, outcomes[i], w)
		}
	}
}

func outcomeRow(status reviewpost.FindingStatus, source reviewpost.FindingSource, check reviewpost.AdditionCheck) reviewverdict.FindingOutcomeRow {
	return reviewverdict.FindingOutcomeRow{Status: status, Source: source, AdditionCheck: check}
}

func TestFindingOutcomeRow_Bucket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		row  reviewverdict.FindingOutcomeRow
		want reviewverdict.FindingSourceBucket
	}{
		{"primary", outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourcePrimary, ""), reviewverdict.FindingSourceBucketPrimary},
		{"checked addition", outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourceCounterReview, reviewpost.AdditionChecked), reviewverdict.FindingSourceBucketCounterReview},
		{"addition not run", outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourceCounterReview, reviewpost.AdditionNotRun), reviewverdict.FindingSourceBucketCounterReviewUnverified},
		{"addition not found", outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourceCounterReview, reviewpost.AdditionNotFound), reviewverdict.FindingSourceBucketCounterReviewUnverified},
		{"addition unconfirmed", outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourceCounterReview, reviewpost.AdditionUnconfirmed), reviewverdict.FindingSourceBucketCounterReviewUnverified},
		{"addition with no check recorded", outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourceCounterReview, ""), reviewverdict.FindingSourceBucketCounterReviewUnverified},
		{"no source recorded", outcomeRow(reviewpost.FindingStatusOpen, "", ""), reviewverdict.FindingSourceBucketNotRecorded},
		{"unknown source", outcomeRow(reviewpost.FindingStatusOpen, "scribe", ""), reviewverdict.FindingSourceBucketNotRecorded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.row.Bucket(); got != tt.want {
				t.Errorf("Bucket() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFindingOutcomes_UnverifiedAdditionsCountedApart: the KPI's main
// distribution leaves unverified additions out, and the per-source
// breakdown counts them in a bucket of their own.
func TestFindingOutcomes_UnverifiedAdditionsCountedApart(t *testing.T) {
	t.Parallel()

	rows := []reviewverdict.FindingOutcomeRow{
		outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourcePrimary, ""),
		outcomeRow(reviewpost.FindingStatusRebutted, reviewpost.FindingSourcePrimary, ""),
		outcomeRow(reviewpost.FindingStatusOpen, reviewpost.FindingSourceCounterReview, reviewpost.AdditionChecked),
		outcomeRow(reviewpost.FindingStatusRebutted, reviewpost.FindingSourceCounterReview, reviewpost.AdditionNotRun),
		outcomeRow(reviewpost.FindingStatusRebutted, reviewpost.FindingSourceCounterReview, reviewpost.AdditionUnconfirmed),
		outcomeRow(reviewpost.FindingStatusOpen, "", ""),
	}

	outcomes, ok := reviewverdict.FindingOutcomes(reviewverdict.VerifiedStatuses(rows))
	if !ok {
		t.Fatal("FindingOutcomes(verified) ok = false, want true")
	}
	wantOutcomes := []reviewverdict.FindingStatusCount{
		{Status: reviewpost.FindingStatusOpen, Count: 3},
		{Status: reviewpost.FindingStatusRebutted, Count: 1},
	}
	if !reflect.DeepEqual(outcomes, wantOutcomes) {
		t.Errorf("FindingOutcomes(verified) = %+v, want %+v (the two unverified additions counted apart)", outcomes, wantOutcomes)
	}

	wantBySource := []reviewverdict.FindingSourceStatusCount{
		{Source: reviewverdict.FindingSourceBucketPrimary, Status: reviewpost.FindingStatusOpen, Count: 1},
		{Source: reviewverdict.FindingSourceBucketPrimary, Status: reviewpost.FindingStatusRebutted, Count: 1},
		{Source: reviewverdict.FindingSourceBucketCounterReview, Status: reviewpost.FindingStatusOpen, Count: 1},
		{Source: reviewverdict.FindingSourceBucketCounterReviewUnverified, Status: reviewpost.FindingStatusRebutted, Count: 2},
		{Source: reviewverdict.FindingSourceBucketNotRecorded, Status: reviewpost.FindingStatusOpen, Count: 1},
	}
	if got := reviewverdict.FindingOutcomesBySource(rows); !reflect.DeepEqual(got, wantBySource) {
		t.Errorf("FindingOutcomesBySource() = %+v, want %+v", got, wantBySource)
	}
	if got := reviewverdict.FindingOutcomesBySource(nil); got != nil {
		t.Errorf("FindingOutcomesBySource(nil) = %+v, want nil", got)
	}
}

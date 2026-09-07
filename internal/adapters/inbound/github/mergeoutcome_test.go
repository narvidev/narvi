package github

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// This file is a WHITE-BOX (package github, not github_test) unit test
// file -- no Postgres, no build tag -- for captureMergeOutcome's own pure
// decision logic (parse, degrade, dispatch), mirroring
// pullrequestevent_test.go's own identical white-box precedent for this
// package's other narrow, database-free units. discardLogger is
// headresolve_test.go's own existing helper, reused directly (same
// package).

// fakeMergeOutcomeRecorder is a test-only MergeOutcomeRecorder -- no real
// Postgres connection, mirroring this package's own established local-fake
// precedent (fakeFalsePositiveFetcher et al., internal/app/reviewcontext).
type fakeMergeOutcomeRecorder struct {
	calls int

	gotRepoFullName string
	gotPRNumber     int32
	gotMerged       bool
	gotClosedAt     time.Time

	err error
}

func (f *fakeMergeOutcomeRecorder) RecordMergeOutcome(_ context.Context, repoFullName string, prNumber int32, merged bool, closedAt time.Time) (sqlcgen.GithubPrSession, error) {
	f.calls++
	f.gotRepoFullName = repoFullName
	f.gotPRNumber = prNumber
	f.gotMerged = merged
	f.gotClosedAt = closedAt
	if f.err != nil {
		return sqlcgen.GithubPrSession{}, f.err
	}
	return sqlcgen.GithubPrSession{RepoFullName: repoFullName, PrNumber: prNumber}, nil
}

func mergeOutcomeBody(t *testing.T, repoFullName string, prNumber int32, merged bool, closedAt string) []byte {
	t.Helper()
	closedAtField := "null"
	if closedAt != "" {
		closedAtField = fmt.Sprintf("%q", closedAt)
	}
	return []byte(fmt.Sprintf(`{
		"action": "closed",
		"repository": {"full_name": %q},
		"pull_request": {"number": %d, "merged": %t, "closed_at": %s}
	}`, repoFullName, prNumber, merged, closedAtField))
}

// TestCaptureMergeOutcome_NilRecorder_NoOp pins the nil-safety documented
// on captureMergeOutcome's own doc comment.
func TestCaptureMergeOutcome_NilRecorder_NoOp(t *testing.T) {
	t.Parallel()
	// No panic, and nothing to assert beyond that -- a nil recorder has
	// no calls to inspect.
	captureMergeOutcome(context.Background(), discardLogger(), nil, mergeOutcomeBody(t, "acme/widgets", 1, true, "2026-01-01T00:00:00Z"))
}

// TestCaptureMergeOutcome_MalformedJSON_NoOp pins the fail-safe degradation
// on a malformed payload -- never a panic, never a call to the recorder.
func TestCaptureMergeOutcome_MalformedJSON_NoOp(t *testing.T) {
	t.Parallel()
	rec := &fakeMergeOutcomeRecorder{}
	captureMergeOutcome(context.Background(), discardLogger(), rec, []byte(`not-json`))
	if rec.calls != 0 {
		t.Errorf("calls = %d, want 0 for a malformed payload", rec.calls)
	}
}

// TestCaptureMergeOutcome_MissingClosedAt_SkipsCapture is the "never
// fabricate now() as a stand-in" fail-safe -- a closed_at: null payload
// (should not happen for a genuine GitHub "closed" action) must never
// reach the recorder at all.
func TestCaptureMergeOutcome_MissingClosedAt_SkipsCapture(t *testing.T) {
	t.Parallel()
	rec := &fakeMergeOutcomeRecorder{}
	captureMergeOutcome(context.Background(), discardLogger(), rec, mergeOutcomeBody(t, "acme/widgets", 1, true, ""))
	if rec.calls != 0 {
		t.Errorf("calls = %d, want 0 when pull_request.closed_at is null", rec.calls)
	}
}

// TestCaptureMergeOutcome_Merged_RecordsVerbatim pins the "forwarded
// verbatim, never re-derived" contract: the payload's own merged=true and
// closed_at reach the recorder byte-for-byte.
//
// Mutation-verified: temporarily swapping p.PullRequest.Merged for a
// hardcoded `false` in captureMergeOutcome (mergeoutcome.go) made this
// test's gotMerged assertion fail; reverted after confirming the failure.
func TestCaptureMergeOutcome_Merged_RecordsVerbatim(t *testing.T) {
	t.Parallel()
	rec := &fakeMergeOutcomeRecorder{}
	closedAt := "2026-01-15T12:30:00Z"
	captureMergeOutcome(context.Background(), discardLogger(), rec, mergeOutcomeBody(t, "acme/widgets", 42, true, closedAt))

	if rec.calls != 1 {
		t.Fatalf("calls = %d, want 1", rec.calls)
	}
	if rec.gotRepoFullName != "acme/widgets" {
		t.Errorf("gotRepoFullName = %q, want %q", rec.gotRepoFullName, "acme/widgets")
	}
	if rec.gotPRNumber != 42 {
		t.Errorf("gotPRNumber = %d, want 42", rec.gotPRNumber)
	}
	if !rec.gotMerged {
		t.Error("gotMerged = false, want true")
	}
	wantTime, err := time.Parse(time.RFC3339, closedAt)
	if err != nil {
		t.Fatalf("test setup: parse closedAt: %v", err)
	}
	if !rec.gotClosedAt.Equal(wantTime) {
		t.Errorf("gotClosedAt = %v, want %v", rec.gotClosedAt, wantTime)
	}
}

// TestCaptureMergeOutcome_ClosedWithoutMerge_RecordsFalse covers the OTHER
// half of the boolean this table's own doc comment calls out: a close
// without a merge.
func TestCaptureMergeOutcome_ClosedWithoutMerge_RecordsFalse(t *testing.T) {
	t.Parallel()
	rec := &fakeMergeOutcomeRecorder{}
	captureMergeOutcome(context.Background(), discardLogger(), rec, mergeOutcomeBody(t, "acme/widgets", 7, false, "2026-01-15T12:30:00Z"))

	if rec.calls != 1 {
		t.Fatalf("calls = %d, want 1", rec.calls)
	}
	if rec.gotMerged {
		t.Error("gotMerged = true, want false for a close-without-merge event")
	}
}

// TestCaptureMergeOutcome_NoRowsFromStore_NeverPanics is the "no session
// to arm eligibility for" acknowledge-and-ignore path -- pgx.ErrNoRows
// from the store must never panic or otherwise misbehave.
func TestCaptureMergeOutcome_NoRowsFromStore_NeverPanics(t *testing.T) {
	t.Parallel()
	rec := &fakeMergeOutcomeRecorder{err: pgx.ErrNoRows}
	captureMergeOutcome(context.Background(), discardLogger(), rec, mergeOutcomeBody(t, "acme/widgets", 1, true, "2026-01-01T00:00:00Z"))
	if rec.calls != 1 {
		t.Fatalf("calls = %d, want 1 (the call was attempted, even though it returned ErrNoRows)", rec.calls)
	}
}

// TestCaptureMergeOutcome_GenuineStoreError_NeverPanics covers the
// remaining error path -- a real store error (not ErrNoRows) must be
// logged, never panic or propagate (this function returns nothing).
func TestCaptureMergeOutcome_GenuineStoreError_NeverPanics(t *testing.T) {
	t.Parallel()
	rec := &fakeMergeOutcomeRecorder{err: errors.New("db exploded")}
	captureMergeOutcome(context.Background(), discardLogger(), rec, mergeOutcomeBody(t, "acme/widgets", 1, true, "2026-01-01T00:00:00Z"))
	if rec.calls != 1 {
		t.Fatalf("calls = %d, want 1", rec.calls)
	}
}

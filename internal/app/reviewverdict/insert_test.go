package reviewverdict

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// TestInsert_RefusesBeforeTouchingTheStore pins the refusals Insert makes
// before it reads a setting or writes a row, which is why nil stores are
// enough here: a verdict with no head sha, and the server-only
// review.CounterReviewUncorroborated offered as the reviewer's own
// counter_review (§26.1). That the refusal names that value alone is
// pinned where Insert really writes: every integration test that posts a
// done or skipped verdict (insert_integration_test.go, httpapi's
// reviewverdict tests) would fail if it refused any other.
func TestInsert_RefusesBeforeTouchingTheStore(t *testing.T) {
	tests := []struct {
		name          string
		headSHA       string
		counterReview review.CounterReviewStatus
		wantRefusal   string
	}{
		{"no head sha", "", review.CounterReviewDone, "no known head sha"},
		{"the server-only uncorroborated state as the reviewer's report", "abc123", review.CounterReviewUncorroborated, "server-only counter-review state"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Insert(context.Background(), nil, nil, false, "acme/widgets", 7, tc.headSHA, pgtype.UUID{},
				review.Verdict{}, reviewpost.Digest{}, reviewtriage.DepthDeep, tc.counterReview, reviewpost.FactCheckDone, 0,
				nil, nil, "", false, reviewverdict.Context{}, pgtype.UUID{})
			if err == nil || !strings.Contains(err.Error(), tc.wantRefusal) {
				t.Fatalf("Insert() error = %v, want a refusal mentioning %q", err, tc.wantRefusal)
			}
		})
	}
}

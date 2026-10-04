package sessionactor

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestOwedRequestDropMessage pins what a dropped request's requester is
// told (technical plan §24.9): the lane it came through, and why -- the
// moves in a row past the bound, or an authorization no longer held.
func TestOwedRequestDropMessage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		trigger string
		why     owedRequestDrop
		moves   int32
		want    []string
	}{
		{name: "the button, past the bound", trigger: turn.RequestTriggerButton, why: owedRequestDropBound, moves: 4, want: []string{"the Re-run review button", "4 times in a row"}},
		{name: "the label, no longer authorized", trigger: turn.RequestTriggerLabel, why: owedRequestDropUnauthorized, moves: 1, want: []string{"the re-review label", "can no longer request reviews"}},
		{name: "a mention, no longer authorized", trigger: turn.RequestTriggerMention, why: owedRequestDropUnauthorized, moves: 2, want: []string{"a mention of the bot", "can no longer request reviews"}},
		{name: "a lane no human records", trigger: turn.RequestTriggerAuto, why: owedRequestDropBound, moves: 5, want: []string{"a person", "5 times in a row"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := owedRequestDropMessage(tc.trigger, tc.why, tc.moves)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("owedRequestDropMessage = %q, want it to say %q", got, w)
				}
			}
		})
	}
}

// TestStopPredates pins the stop's rule for an owed request (technical
// plan §24.9, §3.3): a standing stop requested at or after the request was
// owed drops it; one requested before it, or none, does not.
func TestStopPredates(t *testing.T) {
	t.Parallel()

	owedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	owed := sqlcgen.OwedReviewRequest{CreatedAt: pgtype.Timestamptz{Time: owedAt, Valid: true}}
	for _, tc := range []struct {
		name string
		stop pgtype.Timestamptz
		want bool
	}{
		{name: "no stop standing", stop: pgtype.Timestamptz{}},
		{name: "a stop before the request was owed", stop: pgtype.Timestamptz{Time: owedAt.Add(-time.Second), Valid: true}},
		{name: "a stop at the instant it was owed", stop: pgtype.Timestamptz{Time: owedAt, Valid: true}, want: true},
		{name: "a stop after it was owed", stop: pgtype.Timestamptz{Time: owedAt.Add(time.Second), Valid: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stopPredates(sqlcgen.Session{StopRequestedAt: tc.stop}, owed); got != tc.want {
				t.Errorf("stopPredates = %v, want %v", got, tc.want)
			}
		})
	}
}

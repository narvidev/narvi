package automation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/automation"
)

func TestValidateCronExpr(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr error
	}{
		{"every minute", "* * * * *", nil},
		{"specific time", "0 2 * * *", nil},
		{"step", "*/15 * * * *", nil},
		{"list", "0,30 9,17 * * *", nil},
		{"range", "0 9-17 * * *", nil},
		{"range with step", "0 9-17/2 * * *", nil},
		{"weekdays", "0 9 * * 1-5", nil},
		{"too few fields", "* * * *", automation.ErrCronFieldCount},
		{"too many fields", "* * * * * *", automation.ErrCronFieldCount},
		{"empty", "", automation.ErrCronFieldCount},
		{"non-numeric field", "a * * * *", automation.ErrCronFieldSyntax},
		{"minute out of range", "60 * * * *", automation.ErrCronFieldRange},
		{"hour out of range", "0 24 * * *", automation.ErrCronFieldRange},
		{"day-of-month zero", "0 0 0 * *", automation.ErrCronFieldRange},
		{"month out of range", "0 0 1 13 *", automation.ErrCronFieldRange},
		{"day-of-week out of range", "0 0 * * 7", automation.ErrCronFieldRange},
		{"backwards range", "0 17-9 * * *", automation.ErrCronFieldSyntax},
		{"empty comma item", "0,,0 * * * *", automation.ErrCronFieldSyntax},
		{"bad step", "*/0 * * * *", automation.ErrCronFieldSyntax},
		{"bad step non-numeric", "*/x * * * *", automation.ErrCronFieldSyntax},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := automation.ValidateCronExpr(tt.expr)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCronMatches(t *testing.T) {
	// 2026-08-03 is a Monday.
	mon0200 := time.Date(2026, time.August, 3, 2, 0, 0, 0, time.UTC)
	mon0201 := time.Date(2026, time.August, 3, 2, 1, 0, 0, time.UTC)
	sun0200 := time.Date(2026, time.August, 2, 2, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		expr string
		t    time.Time
		want bool
	}{
		{"every minute matches anything", "* * * * *", mon0201, true},
		{"exact minute match", "0 2 * * *", mon0200, true},
		{"exact minute no match", "0 2 * * *", mon0201, false},
		{"weekday filter matches monday", "0 2 * * 1-5", mon0200, true},
		{"weekday filter excludes sunday", "0 2 * * 1-5", sun0200, false},
		{"step matches every 15", "*/15 * * * *", time.Date(2026, time.August, 3, 2, 30, 0, 0, time.UTC), true},
		{"step excludes non-multiple", "*/15 * * * *", time.Date(2026, time.August, 3, 2, 31, 0, 0, time.UTC), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := automation.CronMatches(tt.expr, tt.t)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("CronMatches(%q, %v) = %v, want %v", tt.expr, tt.t, got, tt.want)
			}
		})
	}
}

func TestCronMatchesInvalidExpr(t *testing.T) {
	_, err := automation.CronMatches("not a cron expr", time.Now())
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestCronMatchesConvertsToUTC(t *testing.T) {
	loc := time.FixedZone("UTC-5", -5*60*60)
	// 21:00 in UTC-5 is 02:00 the next day in UTC.
	localTime := time.Date(2026, time.August, 2, 21, 0, 0, 0, loc)

	got, err := automation.CronMatches("0 2 * * *", localTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatal("expected CronMatches to convert to UTC before comparing, got false")
	}
}

// TestCronMatchesWithin is the review-fix companion to TestCronMatches --
// see CronMatchesWithin's own doc comment (cron.go) for the full "restart/
// stall gap" motivation. All cases use a 1-minute granularity (the ONLY
// granularity app/automation's own trigger pump ever passes,
// platform.Timeouts.AutomationCronGranularity).
func TestCronMatchesWithin(t *testing.T) {
	const minute = time.Minute
	// 2026-08-03 is a Monday.
	day := func(h, m int) time.Time {
		return time.Date(2026, time.August, 3, h, m, 0, 0, time.UTC)
	}

	tests := []struct {
		name   string
		expr   string
		from   time.Time
		to     time.Time
		want   bool
		wantOK bool // false means an error is expected
	}{
		{
			// Parity with CronMatches: a single-bucket window (from = to -
			// granularity) exercising the exact same one instant CronMatches
			// alone would have checked.
			name: "exact single-bucket match parity with CronMatches",
			expr: "30 2 * * *",
			from: day(2, 29), to: day(2, 30),
			want: true, wantOK: true,
		},
		{
			name: "exact single-bucket window with no match",
			expr: "30 2 * * *",
			from: day(2, 28), to: day(2, 29),
			want: false, wantOK: true,
		},
		{
			// A multi-minute gap (a restart/stall) should catch up exactly
			// the one fire that fell inside it.
			name: "multi-minute gap catches up one missed fire",
			expr: "30 2 * * *",
			from: day(2, 25), to: day(2, 35),
			want: true, wantOK: true,
		},
		{
			// A gap spanning zero matching minutes.
			name: "multi-minute gap with zero matching minutes",
			expr: "50 2 * * *",
			from: day(2, 25), to: day(2, 35),
			want: false, wantOK: true,
		},
		{
			// Boundary: `from` is EXCLUSIVE -- a schedule that fires exactly
			// at `from` is NOT counted (already accounted for by whichever
			// earlier evaluation set `from` in the first place).
			name: "from boundary is exclusive",
			expr: "29 2 * * *",
			from: day(2, 29), to: day(2, 30),
			want: false, wantOK: true,
		},
		{
			// Boundary: `to` is INCLUSIVE -- a schedule that fires exactly
			// at `to` IS counted.
			name: "to boundary is inclusive",
			expr: "30 2 * * *",
			from: day(2, 29), to: day(2, 30),
			want: true, wantOK: true,
		},
		{
			// A zero-width (or negative) window matches nothing, regardless
			// of schedule -- from/to are both real bucket boundaries so this
			// never actually happens in production (the trigger pump always
			// supplies to > from), but the function itself stays honest
			// about it rather than panicking or looping forever.
			name: "empty window never matches",
			expr: "* * * * *",
			from: day(2, 30), to: day(2, 30),
			want: false, wantOK: true,
		},
		{
			name: "invalid schedule still surfaces an error",
			expr: "not a cron expr",
			from: day(2, 0), to: day(2, 10),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := automation.CronMatchesWithin(tt.expr, tt.from, tt.to, minute)
			if !tt.wantOK {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("CronMatchesWithin(%q, %v, %v) = %v, want %v", tt.expr, tt.from, tt.to, got, tt.want)
			}
		})
	}
}

// TestCronMatchesWithin_NonPositiveGranularityErrors proves the defensive
// guard against a zero/negative granularity (never supplied by the real
// caller, but not silently infinite-looped either).
func TestCronMatchesWithin_NonPositiveGranularityErrors(t *testing.T) {
	from := time.Date(2026, time.August, 3, 2, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.August, 3, 2, 10, 0, 0, time.UTC)

	if _, err := automation.CronMatchesWithin("* * * * *", from, to, 0); err == nil {
		t.Fatal("expected an error for a zero granularity")
	}
	if _, err := automation.CronMatchesWithin("* * * * *", from, to, -time.Minute); err == nil {
		t.Fatal("expected an error for a negative granularity")
	}
}

// TestCronCatchUpFrom pins the window one cron tick evaluates: from the
// last fire under the catch-up ceiling for an automation that has fired,
// and from the minute it was created under the same ceiling for one that
// has not -- never further back than the ceiling, never later than one
// bucket back.
func TestCronCatchUpFrom(t *testing.T) {
	const granularity, catchUp = time.Minute, 10 * time.Minute
	now := time.Date(2026, 10, 6, 10, 13, 10, 0, time.UTC)
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 6, h, m, s, 0, time.UTC) }
	ptr := func(t time.Time) *time.Time { return &t }
	for _, tc := range []struct {
		name       string
		lastFired  *time.Time
		createdAt  time.Time
		wantFrom   time.Time
		wantCapped bool
	}{
		{name: "fired within the ceiling: from the last fire", lastFired: ptr(at(10, 8, 0)), createdAt: at(9, 0, 0), wantFrom: at(10, 8, 0)},
		{name: "fired before the ceiling: the ceiling, capped", lastFired: ptr(at(9, 0, 0)), createdAt: at(8, 0, 0), wantFrom: at(10, 3, 10), wantCapped: true},
		{name: "never fired, created long ago: the ceiling, not capped", createdAt: at(8, 0, 0), wantFrom: at(10, 3, 10)},
		{name: "never fired, created within the ceiling: the bucket before its creation minute", createdAt: at(10, 7, 40), wantFrom: at(10, 6, 0)},
		{name: "never fired, created this minute: one bucket back", createdAt: at(10, 13, 5), wantFrom: at(10, 12, 0)},
		{name: "never fired, a creation time that reads later than now: one bucket back", createdAt: at(10, 14, 30), wantFrom: at(10, 12, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, capped := automation.CronCatchUpFrom(tc.lastFired, tc.createdAt, now, granularity, catchUp)
			if !from.Equal(tc.wantFrom) || capped != tc.wantCapped {
				t.Fatalf("CronCatchUpFrom = (%v, %v), want (%v, %v)", from, capped, tc.wantFrom, tc.wantCapped)
			}
		})
	}
}

// TestCronCatchUpFrom_NeverFiredCatchesItsFirstOccurrence pins what the
// window means for an automation that has never fired, through
// CronMatchesWithin: an occurrence after its creation and within the
// ceiling fires on a later tick; one before its creation, or before the
// ceiling, does not.
func TestCronCatchUpFrom_NeverFiredCatchesItsFirstOccurrence(t *testing.T) {
	const granularity, catchUp = time.Minute, 10 * time.Minute
	occurrence := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	schedule := "0 10 * * *"
	for _, tc := range []struct {
		name      string
		createdAt time.Time
		tick      time.Time
		want      bool
	}{
		{name: "created before it, a tick three minutes after", createdAt: occurrence.Add(-time.Hour), tick: occurrence.Add(3*time.Minute + 10*time.Second), want: true},
		{name: "created in its own minute, a tick in it", createdAt: occurrence.Add(30 * time.Second), tick: occurrence.Add(45 * time.Second), want: true},
		{name: "created after it, a tick three minutes after", createdAt: occurrence.Add(2 * time.Minute), tick: occurrence.Add(3*time.Minute + 10*time.Second), want: false},
		{name: "created before it, a tick past the ceiling", createdAt: occurrence.Add(-time.Hour), tick: occurrence.Add(15 * time.Minute), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, _ := automation.CronCatchUpFrom(nil, tc.createdAt, tc.tick, granularity, catchUp)
			got, err := automation.CronMatchesWithin(schedule, from, tc.tick.Truncate(granularity), granularity)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("matched = %v, want %v (window from %v)", got, tc.want, from)
			}
		})
	}
}

//go:build integration

package automation_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
)

// This file pins technical plan §40.2 at the automation engine: while
// autonomy is frozen a matched cron fire is not claimed and an invocation
// is not fanned out, an event still records its invocation, nothing is
// recorded against the automation, and once the freeze lifts a missed
// cron occurrence fires at most once within the catch-up window and the
// held invocations fan out in order.

func (f *testFixture) freeze(t *testing.T) {
	t.Helper()
	settings := narvipg.NewPlatformSettingsStore(f.pool)
	if _, err := settings.Freeze(context.Background(), pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
}

func (f *testFixture) unfreeze(t *testing.T) {
	t.Helper()
	if _, err := narvipg.NewPlatformSettingsStore(f.pool).Unfreeze(context.Background()); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
}

// freezeSkips reads autonomy_freeze_skip_total{site, reason=frozen}.
func freezeSkips(t *testing.T, site domainautonomy.Site) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := automation.IntegrationMetricsReader().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("site", string(site)), attribute.String("reason", string(domainautonomy.SkipFrozen)))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "autonomy_freeze_skip_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("autonomy_freeze_skip_total is %T, want a Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// lastCronFiredAt reads the automation's last recorded cron fire.
func (f *testFixture) lastCronFiredAt(t *testing.T, id pgtype.UUID) pgtype.Timestamptz {
	t.Helper()
	row, err := f.automations.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	return row.LastCronFiredAt
}

// dailyAt returns a cron schedule matching minute, every day, and that
// minute's bucket: a fixed past minute, so whether a tick's window holds
// it is decided by the tick's own instant.
func dailyAt(minute time.Time) string {
	return fmt.Sprintf("%d %d * * *", minute.Minute(), minute.Hour())
}

// TestCronTrigger_Frozen_NoClaimNoInvocation: a matched cron fire while
// autonomy is frozen creates no invocation and claims nothing --
// last_cron_fired_at stays unset -- and the skip is counted.
func TestCronTrigger_Frozen_NoClaimNoInvocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	auto := f.createCronAutomation(t, "every minute, frozen", "* * * * *", sqlcgen.AutomationStatusActive)
	f.freeze(t)

	before := freezeSkips(t, domainautonomy.SiteAutomationCron)
	minute := time.Now().UTC().Truncate(time.Minute)
	for _, at := range []time.Time{minute.Add(10 * time.Second), minute.Add(70 * time.Second)} {
		if err := f.engine.EvaluateCronTriggersAtForTest(ctx, at); err != nil {
			t.Fatalf("tick at %s: %v", at.Format(time.TimeOnly), err)
		}
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations while frozen = %d, want 0", got)
	}
	if fired := f.lastCronFiredAt(t, auto.ID); fired.Valid {
		t.Fatalf("last_cron_fired_at = %v while frozen, want unset: a frozen fire claims nothing", fired.Time)
	}
	if got := freezeSkips(t, domainautonomy.SiteAutomationCron) - before; got != 2 {
		t.Fatalf("autonomy_freeze_skip_total{site=automation_cron} rose by %d over two frozen ticks, want 2", got)
	}
}

// TestCronTrigger_Unfreeze_FiresOnceWithinCatchUp: an occurrence missed
// while frozen fires once, on the first tick after the freeze lifts within
// the catch-up window -- frozen ticks claimed nothing, so its minute is
// still in the window -- and never again.
func TestCronTrigger_Unfreeze_FiresOnceWithinCatchUp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	missed := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	auto := f.createCronAutomation(t, "daily, missed while frozen", dailyAt(missed), sqlcgen.AutomationStatusActive)
	if _, err := f.pool.Exec(ctx, "UPDATE automations SET last_cron_fired_at = $1 WHERE id = $2", missed.Add(-5*time.Minute), auto.ID); err != nil {
		t.Fatalf("record an earlier fire: %v", err)
	}
	f.freeze(t)
	for _, at := range []time.Time{missed.Add(10 * time.Second), missed.Add(70 * time.Second)} {
		if err := f.engine.EvaluateCronTriggersAtForTest(ctx, at); err != nil {
			t.Fatalf("frozen tick at %s: %v", at.Format(time.TimeOnly), err)
		}
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations while frozen = %d, want 0", got)
	}

	f.unfreeze(t)
	for i, tc := range []struct {
		at   time.Time
		want int
	}{
		{at: missed.Add(3*time.Minute + 10*time.Second), want: 1},
		{at: missed.Add(3*time.Minute + 40*time.Second), want: 1},
		{at: missed.Add(4*time.Minute + 10*time.Second), want: 1},
	} {
		if err := f.engine.EvaluateCronTriggersAtForTest(ctx, tc.at); err != nil {
			t.Fatalf("tick %d at %s: %v", i, tc.at.Format(time.TimeOnly), err)
		}
		if got := f.countInvocationsForAutomation(t, auto.ID); got != tc.want {
			t.Fatalf("invocations after tick %d at %s = %d, want %d: the missed occurrence fires once",
				i, tc.at.Format(time.TimeOnly), got, tc.want)
		}
	}
}

// TestCronTrigger_FrozenPastCatchUp_WaitsForNextOccurrence: a freeze
// longer than the catch-up window does not fire the occurrence it held
// once lifted -- no burst, no late run -- and the next scheduled
// occurrence fires as usual.
func TestCronTrigger_FrozenPastCatchUp_WaitsForNextOccurrence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	missed := time.Now().UTC().Truncate(time.Minute).Add(-48 * time.Hour)
	auto := f.createCronAutomation(t, "daily, frozen past the window", dailyAt(missed), sqlcgen.AutomationStatusActive)
	if _, err := f.pool.Exec(ctx, "UPDATE automations SET last_cron_fired_at = $1 WHERE id = $2", missed.Add(-5*time.Minute), auto.ID); err != nil {
		t.Fatalf("record an earlier fire: %v", err)
	}
	f.freeze(t)
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, missed.Add(10*time.Second)); err != nil {
		t.Fatalf("frozen tick: %v", err)
	}

	f.unfreeze(t)
	pastWindow := missed.Add(15 * time.Minute)
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, pastWindow); err != nil {
		t.Fatalf("tick past the window: %v", err)
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations after the freeze outlasted the window = %d, want 0", got)
	}
	next := missed.Add(24*time.Hour + 10*time.Second)
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, next); err != nil {
		t.Fatalf("tick at the next occurrence: %v", err)
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 1 {
		t.Fatalf("invocations at the next occurrence = %d, want 1", got)
	}
}

// setCreatedAt back-dates an automation's creation, so a tick the test
// places in the past sees it existing then.
func (f *testFixture) setCreatedAt(t *testing.T, id pgtype.UUID, at time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), "UPDATE automations SET created_at = $1 WHERE id = $2", at, id); err != nil {
		t.Fatalf("back-date the automation: %v", err)
	}
}

// TestCronTrigger_NeverFired_Unfreeze_FiresOnceWithinCatchUp: an automation
// that has never fired catches up its first occurrence like any later one.
// Its first scheduled minute falls inside a freeze; the first tick after
// the freeze lifts, within the catch-up window, fires it once, and later
// ticks never again.
func TestCronTrigger_NeverFired_Unfreeze_FiresOnceWithinCatchUp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	occurrence := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	auto := f.createCronAutomation(t, "daily, never fired, held", dailyAt(occurrence), sqlcgen.AutomationStatusActive)
	f.setCreatedAt(t, auto.ID, occurrence.Add(-time.Hour))
	if fired := f.lastCronFiredAt(t, auto.ID); fired.Valid {
		t.Fatalf("last_cron_fired_at = %v, want unset: the automation has never fired", fired.Time)
	}
	f.freeze(t)
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(10*time.Second)); err != nil {
		t.Fatalf("frozen tick: %v", err)
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations while frozen = %d, want 0", got)
	}

	f.unfreeze(t)
	for i, at := range []time.Time{
		occurrence.Add(3*time.Minute + 10*time.Second),
		occurrence.Add(3*time.Minute + 40*time.Second),
		occurrence.Add(4*time.Minute + 10*time.Second),
	} {
		if err := f.engine.EvaluateCronTriggersAtForTest(ctx, at); err != nil {
			t.Fatalf("tick %d at %s: %v", i, at.Format(time.TimeOnly), err)
		}
		if got := f.countInvocationsForAutomation(t, auto.ID); got != 1 {
			t.Fatalf("invocations after tick %d at %s = %d, want 1: the held first occurrence fires once", i, at.Format(time.TimeOnly), got)
		}
	}
}

// TestCronTrigger_NeverFired_FrozenPastCatchUp_WaitsForNextOccurrence: a
// freeze that outlasts the catch-up window does not fire the first
// occurrence it held, and the next scheduled occurrence fires as usual.
func TestCronTrigger_NeverFired_FrozenPastCatchUp_WaitsForNextOccurrence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	occurrence := time.Now().UTC().Truncate(time.Minute).Add(-48 * time.Hour)
	auto := f.createCronAutomation(t, "daily, never fired, frozen past the window", dailyAt(occurrence), sqlcgen.AutomationStatusActive)
	f.setCreatedAt(t, auto.ID, occurrence.Add(-time.Hour))
	f.freeze(t)
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(10*time.Second)); err != nil {
		t.Fatalf("frozen tick: %v", err)
	}

	f.unfreeze(t)
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(15*time.Minute)); err != nil {
		t.Fatalf("tick past the window: %v", err)
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations after the freeze outlasted the window = %d, want 0", got)
	}
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(24*time.Hour+10*time.Second)); err != nil {
		t.Fatalf("tick at the next occurrence: %v", err)
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 1 {
		t.Fatalf("invocations at the next occurrence = %d, want 1", got)
	}
}

// TestCronTrigger_NeverFired_AnOccurrenceBeforeItsCreationNeverFires: the
// window of an automation that has never fired starts at its creation, so
// a schedule that matched minutes before the automation existed does not
// fire on its first tick, however close.
func TestCronTrigger_NeverFired_AnOccurrenceBeforeItsCreationNeverFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	occurrence := time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Hour)
	auto := f.createCronAutomation(t, "daily, created after its occurrence", dailyAt(occurrence), sqlcgen.AutomationStatusActive)
	f.setCreatedAt(t, auto.ID, occurrence.Add(2*time.Minute))
	if err := f.engine.EvaluateCronTriggersAtForTest(ctx, occurrence.Add(3*time.Minute+10*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.countInvocationsForAutomation(t, auto.ID); got != 0 {
		t.Fatalf("invocations = %d, want 0: the occurrence came before the automation existed", got)
	}
}

// assertHeldInvocation checks an invocation the freeze held: pending,
// unclaimed, with no run.
func (f *testFixture) assertHeldInvocation(t *testing.T, id pgtype.UUID) {
	t.Helper()
	inv, err := f.invocations.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if inv.Status != sqlcgen.AutomationInvocationStatusPending || inv.FannedOutAt.Valid {
		t.Fatalf("invocation status %s, fanned out %v; want pending and unclaimed", inv.Status, inv.FannedOutAt.Valid)
	}
	if runs := f.listRunsForInvocation(t, id); len(runs) != 0 {
		t.Fatalf("runs = %d, want none", len(runs))
	}
}

// assertNothingCounted checks an automation the freeze held: still active,
// no strike, and no session created for it.
func (f *testFixture) assertNothingCounted(t *testing.T, id pgtype.UUID) {
	t.Helper()
	auto, err := f.automations.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if auto.Status != sqlcgen.AutomationStatusActive || auto.ConsecutiveFailures != 0 {
		t.Fatalf("automation status %s, strikes %d; want active with none: a skip is no failure", auto.Status, auto.ConsecutiveFailures)
	}
	var sessions int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("sessions = %d, want none", sessions)
	}
}

// TestFanOut_Frozen_NothingClaimed: an event still records its invocation
// while autonomy is frozen -- the candidate -- but the fan-out claims
// nothing: no run, no session, no failed run or strike, and the skip is
// counted, which a query that excluded frozen invocations could not do.
func TestFanOut_Frozen_NothingClaimed(t *testing.T) {
	f := newFixture(t)
	auto, targets := f.createAutomation(t, "frozen fan-out", 2)
	f.freeze(t)
	inv := f.createInvocation(t, auto.ID, targets)

	before := freezeSkips(t, domainautonomy.SiteAutomationFanOut)
	if err := f.engine.PumpOnce(context.Background()); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	f.assertHeldInvocation(t, inv.ID)
	f.assertNothingCounted(t, auto.ID)
	if got := freezeSkips(t, domainautonomy.SiteAutomationFanOut) - before; got != 1 {
		t.Fatalf("autonomy_freeze_skip_total{site=automation_fan_out} rose by %d, want 1: the held invocation is counted", got)
	}
}

// TestFanOut_FreezeAfterClaim_ClaimReleased: a freeze committed after the
// claim -- before any invocation of the batch fanned out -- gives back
// every claim in the batch: each invocation pending and unclaimed, with no
// run, no session and nothing counted against its automation.
func TestFanOut_FreezeAfterClaim_ClaimReleased(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	auto, targets := f.createAutomation(t, "frozen after the claim", 2)
	first := f.createInvocation(t, auto.ID, targets)
	second := f.createInvocation(t, auto.ID, targets)
	t.Cleanup(func() { _, _ = narvipg.NewPlatformSettingsStore(f.pool).Unfreeze(context.Background()) })

	before := freezeSkips(t, domainautonomy.SiteAutomationFanOut)
	var freezeErr error
	claimedBefore := 0
	if err := f.engine.PumpOnceAfterClaimForTest(ctx, func() {
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM automation_invocations WHERE fanned_out_at IS NOT NULL`).Scan(&claimedBefore)
		_, freezeErr = narvipg.NewPlatformSettingsStore(f.pool).Freeze(ctx, pgtype.UUID{}, "an incident: frozen after the claim")
	}); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	if freezeErr != nil {
		t.Fatalf("freeze after the claim: %v", freezeErr)
	}
	if claimedBefore != 2 {
		t.Fatalf("invocations claimed when the freeze landed = %d, want 2", claimedBefore)
	}
	f.assertHeldInvocation(t, first.ID)
	f.assertHeldInvocation(t, second.ID)
	f.assertNothingCounted(t, auto.ID)
	if got := freezeSkips(t, domainautonomy.SiteAutomationFanOut) - before; got != 2 {
		t.Fatalf("autonomy_freeze_skip_total{site=automation_fan_out} rose by %d, want 2: each held invocation counted once", got)
	}
}

// TestFanOut_HeldThroughAPause_FansOutOnResume pins what pausing does to
// invocations the freeze held: it defers them, it does not discard them.
// Paused, they stay pending and unclaimed after the freeze lifts; resumed,
// every one of them fans out on the next tick.
func TestFanOut_HeldThroughAPause_FansOutOnResume(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	auto, targets := f.createAutomation(t, "held, paused, resumed", 1)
	f.freeze(t)
	held := []sqlcgen.AutomationInvocation{
		f.createInvocation(t, auto.ID, targets),
		f.createInvocation(t, auto.ID, targets),
		f.createInvocation(t, auto.ID, targets),
	}
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("frozen PumpOnce: %v", err)
	}
	if _, err := f.automations.Pause(ctx, auto.ID); err != nil {
		t.Fatalf("pause the automation: %v", err)
	}
	f.unfreeze(t)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce while paused: %v", err)
	}
	for _, inv := range held {
		f.assertHeldInvocation(t, inv.ID)
	}

	if _, err := f.automations.Resume(ctx, auto.ID); err != nil {
		t.Fatalf("resume the automation: %v", err)
	}
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after the resume: %v", err)
	}
	for i, inv := range held {
		if runs := f.listRunsForInvocation(t, inv.ID); len(runs) != 1 {
			t.Fatalf("invocation %d: runs after the resume = %d, want 1: pausing deferred it, it did not discard it", i, len(runs))
		}
	}
}

// TestFanOut_Unfreeze_FansOutInOrder: the invocations held through the
// freeze are still candidates once it lifts, and the next tick fans them
// out in the order they were created.
func TestFanOut_Unfreeze_FansOutInOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	auto, targets := f.createAutomation(t, "held then fanned out", 1)
	f.freeze(t)
	first := f.createInvocation(t, auto.ID, targets)
	second := f.createInvocation(t, auto.ID, targets)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("frozen PumpOnce: %v", err)
	}
	f.assertHeldInvocation(t, first.ID)
	f.assertHeldInvocation(t, second.ID)

	f.unfreeze(t)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after the unfreeze: %v", err)
	}
	firstRuns, secondRuns := f.listRunsForInvocation(t, first.ID), f.listRunsForInvocation(t, second.ID)
	if len(firstRuns) != 1 || len(secondRuns) != 1 {
		t.Fatalf("runs after the unfreeze = (%d, %d), want one each", len(firstRuns), len(secondRuns))
	}
	if !firstRuns[0].CreatedAt.Time.Before(secondRuns[0].CreatedAt.Time) {
		t.Fatalf("the first invocation's run was created at %v, not before the second's at %v: held invocations fan out in created_at order",
			firstRuns[0].CreatedAt.Time, secondRuns[0].CreatedAt.Time)
	}
}

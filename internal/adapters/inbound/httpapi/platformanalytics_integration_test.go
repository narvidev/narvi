//go:build integration

// Integration tests for §12.2 item 6's own ("analytics: platform-wide
// rollup") read-only analytics route (platformanalytics.go's
// own GetPlatformAnalytics), against a real Postgres instance -- sharing
// this package's own testRig (httpapi_integration_test.go), mirroring
// reviewanalytics_integration_test.go's own precedent exactly.
package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainpa "github.com/narvidev/narvi/internal/domain/platformanalytics"
)

// TestGetPlatformAnalytics_ViewerAllowed_NothingComputedYet proves TWO
// things at once, mirroring TestGetReviewAnalytics_ViewerAllowed_
// NothingComputedYet exactly one level up (platform-wide instead of
// repo-scoped): (1) authz.ActionViewAnalytics genuinely allows a VIEWER;
// (2) a freshly-migrated deployment with no session/turn/event/
// false_failures rows at all renders every SAMPLE-SIZE sentinel
// false/nil, but sessionsTotalComputed/falseFailureCountComputed BOTH
// true (their own underlying queries DID succeed here, they just found
// nothing) with sessionsTotal/falseFailureCount as real, genuinely
// computed zeros -- and never a 500. The genuinely-uncomputed case (a
// FAILED fetch) is TestGetPlatformAnalytics_
// SessionOutcomeCountsFetchFailedDegradesOnlyItsOwnFields and
// TestGetPlatformAnalytics_FalseFailureCountFetchFailedDegradesOnlyItsOwnField,
// below.
func TestGetPlatformAnalytics_ViewerAllowed_NothingComputedYet(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleViewer)

	var resp restdtos.PlatformAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.WindowDays != 30 {
		t.Errorf("WindowDays = %d, want 30", resp.WindowDays)
	}
	if !resp.SessionsTotalComputed {
		t.Errorf("SessionsTotalComputed = false, want true (the fetch itself succeeded, it just found nothing)")
	}
	if resp.SessionsTotal != 0 {
		t.Errorf("SessionsTotal = %d, want 0 (a real, meaningful, genuinely COMPUTED zero)", resp.SessionsTotal)
	}
	if !resp.FalseFailureCountComputed {
		t.Errorf("FalseFailureCountComputed = false, want true (the fetch itself succeeded, it just found nothing)")
	}
	if resp.FalseFailureCount != 0 {
		t.Errorf("FalseFailureCount = %d, want 0 (a real, meaningful, genuinely COMPUTED zero)", resp.FalseFailureCount)
	}

	if resp.SessionsPerDayComputed {
		t.Errorf("SessionsPerDayComputed = true, want false (no sessions)")
	}
	if resp.SessionsPerDay != nil {
		t.Errorf("SessionsPerDay = %v, want nil", resp.SessionsPerDay)
	}
	if resp.SuccessRateComputed {
		t.Errorf("SuccessRateComputed = true, want false (no resolved sessions)")
	}
	if resp.SuccessRatePercent != nil {
		t.Errorf("SuccessRatePercent = %v, want nil", resp.SuccessRatePercent)
	}
	if resp.SuccessRateSampleSize != 0 {
		t.Errorf("SuccessRateSampleSize = %d, want 0", resp.SuccessRateSampleSize)
	}
	if resp.CostComputed {
		t.Errorf("CostComputed = true, want false (no costed turns)")
	}
	if resp.CostTotalUsd != nil || resp.CostMedianPerSessionUsd != nil {
		t.Errorf("CostTotalUsd/CostMedianPerSessionUsd = %v/%v, want nil/nil", resp.CostTotalUsd, resp.CostMedianPerSessionUsd)
	}
	if resp.CostByModelComputed {
		t.Errorf("CostByModelComputed = true, want false")
	}
	if resp.CostByModel != nil {
		t.Errorf("CostByModel = %v, want nil", resp.CostByModel)
	}
	if resp.BootP95Computed {
		t.Errorf("BootP95Computed = true, want false (no boot_timing samples)")
	}
	if resp.BootP95Seconds != nil {
		t.Errorf("BootP95Seconds = %v, want nil", resp.BootP95Seconds)
	}
	if resp.BootP95SampleSize != 0 {
		t.Errorf("BootP95SampleSize = %d, want 0", resp.BootP95SampleSize)
	}
	if resp.TopFailureReasonsComputed {
		t.Errorf("TopFailureReasonsComputed = true, want false (no sessions)")
	}
	if resp.TopFailureReasons != nil {
		t.Errorf("TopFailureReasons = %v, want nil", resp.TopFailureReasons)
	}
}

// TestGetPlatformAnalytics_RendersComputedRollups seeds a deliberately
// MIXED fixture -- sessions of every relevant kind, not just one -- so
// every aggregate assertion below is genuinely exercised rather than
// vacuous: three completed sessions (costed on "sonnet-5", "opus-4.8",
// and with NO model_id recorded at all -- proving the "unknown" bucket),
// one failed/timeout session, one cancelled session, one still-active
// session (a turn never terminalized), and one session left exactly as
// created (zero turns dispatched at all). This is the ONE case in this
// file that exercises the actual count/grouping/percentile logic end to
// end through the real HTTP surface (internal/domain/platformanalytics
// is unit-tested directly; this is the wiring proof).
func TestGetPlatformAnalytics_RendersComputedRollups(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	// Session 1: completed, one turn costed $10 on "sonnet-5".
	completed1 := mustCreateSession(ctx, t, rig)
	seedCostedTurn(ctx, t, rig, completed1, "sonnet-5", 10)
	mustCompleteSession(ctx, t, rig, completed1)

	// Session 2: completed, one turn costed $4 on "opus-4.8" -- a
	// DIFFERENT model, so costByModel must report two distinct entries,
	// not one collapsed total.
	completed2 := mustCreateSession(ctx, t, rig)
	seedCostedTurn(ctx, t, rig, completed2, "opus-4.8", 4)
	mustCompleteSession(ctx, t, rig, completed2)

	// Session 2b: completed, one turn costed $2 with NO model_id recorded
	// -- proves ListCostByModelInWindow's own COALESCE(model_id,
	// 'unknown') bucketing: this turn's own spend must still appear (as
	// an explicit "unknown" bucket, summing correctly into costTotalUsd),
	// never silently dropped.
	completed3 := mustCreateSession(ctx, t, rig)
	seedCostedTurn(ctx, t, rig, completed3, "", 2)
	mustCompleteSession(ctx, t, rig, completed3)

	// Session 3: failed/timeout -- contributes to both successRate's
	// denominator and topFailureReasons.
	failed := mustCreateSession(ctx, t, rig)
	mustFailSessionTimeout(ctx, t, rig, failed)

	// Session 4: cancelled -- must be EXCLUDED from successRate (neither
	// numerator nor denominator) but still counted in sessionsTotal/
	// sessionsPerDay and topFailureReasons.
	cancelled := mustCreateSession(ctx, t, rig)
	mustCancelSession(ctx, t, rig, cancelled)

	// Session 5: still active (a turn left processing) -- must be
	// EXCLUDED from successRate, still counted in sessionsTotal/
	// sessionsPerDay's own activeCount. sessions.status is never
	// auto-derived by a plain turns.Create -- production code
	// (persistDerivedSessionStatus) is what would normally stamp this,
	// so the test drives the same UpdateStatus call directly.
	active := mustCreateSession(ctx, t, rig)
	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: active, Status: sqlcgen.TurnStatusProcessing}); err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: active, Status: sqlcgen.SessionStatusActive}); err != nil {
		t.Fatalf("activate session: %v", err)
	}

	// Session 6: left exactly as CreateSession's own DB default
	// (status='created', zero turns) -- proves a session nobody has
	// dispatched a single turn for is still counted honestly, in both
	// sessionsTotal and sessionsPerDay's own createdCount.
	mustCreateSession(ctx, t, rig)

	// Exactly platformanalytics.BootP95MinSamples successful boot_timing
	// samples, seconds 1..20 -- a known, hand-computable p95.
	for i := 1; i <= domainpa.BootP95MinSamples; i++ {
		seedBootTimingEvent(ctx, t, rig, completed1, fmt.Sprintf("boot-%d", i), float64(i), false)
	}
	// One FAILED boot attempt with a huge duration -- must be excluded,
	// or it would blow the p95 far past what the successful samples alone
	// produce.
	seedBootTimingEvent(ctx, t, rig, completed1, "boot-failed", 9999, true)

	// One false-failure incident.
	if _, err := rig.falseFailures.Insert(ctx, completed1); err != nil {
		t.Fatalf("seed false_failures row: %v", err)
	}

	var resp restdtos.PlatformAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if !resp.SessionsTotalComputed {
		t.Fatalf("SessionsTotalComputed = false, want true")
	}
	if resp.SessionsTotal != 7 {
		t.Errorf("SessionsTotal = %d, want 7", resp.SessionsTotal)
	}

	if !resp.SessionsPerDayComputed {
		t.Fatalf("SessionsPerDayComputed = false, want true")
	}
	if resp.SessionsPerDay == nil || len(*resp.SessionsPerDay) != 1 {
		t.Fatalf("SessionsPerDay = %v, want exactly 1 bucket (all seeded today)", resp.SessionsPerDay)
	}
	bucket := (*resp.SessionsPerDay)[0]
	if bucket.CompletedCount != 3 {
		t.Errorf("bucket.CompletedCount = %d, want 3", bucket.CompletedCount)
	}
	if bucket.FailedCount != 1 {
		t.Errorf("bucket.FailedCount = %d, want 1", bucket.FailedCount)
	}
	if bucket.CancelledCount != 1 {
		t.Errorf("bucket.CancelledCount = %d, want 1", bucket.CancelledCount)
	}
	if bucket.ActiveCount != 1 {
		t.Errorf("bucket.ActiveCount = %d, want 1", bucket.ActiveCount)
	}
	if bucket.CreatedCount != 1 {
		t.Errorf("bucket.CreatedCount = %d, want 1", bucket.CreatedCount)
	}

	// successRate: 3 completed, 1 failed -> 75%, sample size 4 --
	// cancelled(1) and active(1) must NOT appear in the denominator (which
	// would otherwise read 6, or the rate would read differently).
	if !resp.SuccessRateComputed {
		t.Fatalf("SuccessRateComputed = false, want true")
	}
	if resp.SuccessRateSampleSize != 4 {
		t.Fatalf("SuccessRateSampleSize = %d, want 4 (cancelled/active excluded)", resp.SuccessRateSampleSize)
	}
	if resp.SuccessRatePercent == nil {
		t.Fatalf("SuccessRatePercent = nil, want a computed value")
	}
	wantRate := float64(3) / float64(4) * 100
	if got := *resp.SuccessRatePercent; got < wantRate-0.01 || got > wantRate+0.01 {
		t.Errorf("SuccessRatePercent = %v, want ~%v", got, wantRate)
	}

	if !resp.FalseFailureCountComputed {
		t.Fatalf("FalseFailureCountComputed = false, want true")
	}
	if resp.FalseFailureCount != 1 {
		t.Errorf("FalseFailureCount = %d, want 1", resp.FalseFailureCount)
	}

	if !resp.CostComputed {
		t.Fatalf("CostComputed = false, want true")
	}
	if resp.CostSampleSize != 3 {
		t.Errorf("CostSampleSize = %d, want 3 (three distinct costed sessions)", resp.CostSampleSize)
	}
	if resp.CostTotalUsd == nil || *resp.CostTotalUsd != 16 {
		t.Errorf("CostTotalUsd = %v, want 16 (10 + 4 + 2)", resp.CostTotalUsd)
	}
	if resp.CostMedianPerSessionUsd == nil {
		t.Fatalf("CostMedianPerSessionUsd = nil, want a computed value")
	}
	// Median of {10, 4, 2} (odd count) = the middle value, 4.
	if got := *resp.CostMedianPerSessionUsd; got != 4 {
		t.Errorf("CostMedianPerSessionUsd = %v, want 4", got)
	}

	if !resp.CostByModelComputed {
		t.Fatalf("CostByModelComputed = false, want true")
	}
	if resp.CostByModel == nil || len(*resp.CostByModel) != 3 {
		t.Fatalf("CostByModel = %v, want exactly 3 models (including the 'unknown' bucket)", resp.CostByModel)
	}
	// Spend descending: sonnet-5 ($10), opus-4.8 ($4), unknown ($2) -- the
	// no-model_id turn's own spend must appear, not be silently dropped.
	if got := (*resp.CostByModel)[0]; got.ModelId != "sonnet-5" || got.TotalUsd != 10 {
		t.Errorf("CostByModel[0] = %+v, want {sonnet-5 10}", got)
	}
	if got := (*resp.CostByModel)[1]; got.ModelId != "opus-4.8" || got.TotalUsd != 4 {
		t.Errorf("CostByModel[1] = %+v, want {opus-4.8 4}", got)
	}
	if got := (*resp.CostByModel)[2]; got.ModelId != "unknown" || got.TotalUsd != 2 {
		t.Errorf("CostByModel[2] = %+v, want {unknown 2}", got)
	}

	if !resp.BootP95Computed {
		t.Fatalf("BootP95Computed = false, want true (exactly the minimum sample count was seeded)")
	}
	if resp.BootP95SampleSize != domainpa.BootP95MinSamples {
		t.Errorf("BootP95SampleSize = %d, want %d (the failed sample must be excluded)", resp.BootP95SampleSize, domainpa.BootP95MinSamples)
	}
	if resp.BootP95Seconds == nil {
		t.Fatalf("BootP95Seconds = nil, want a computed value")
	}
	// percentile_cont(0.95) over 1..20 (linear interpolation) = 19.05 --
	// and, critically, nowhere near the 9999s failed sample.
	if got := *resp.BootP95Seconds; got < 19 || got > 19.1 {
		t.Errorf("BootP95Seconds = %v, want ~19.05 (and never near the excluded failed sample's 9999s)", got)
	}

	if !resp.TopFailureReasonsComputed {
		t.Fatalf("TopFailureReasonsComputed = false, want true")
	}
	if resp.TopFailureReasons == nil || len(*resp.TopFailureReasons) != 2 {
		t.Fatalf("TopFailureReasons = %v, want exactly 2 reasons (timeout, cancelled)", resp.TopFailureReasons)
	}
	gotReasons := map[string]int{}
	for _, fr := range *resp.TopFailureReasons {
		gotReasons[string(fr.Reason)] = fr.Count
	}
	if gotReasons["timeout"] != 1 {
		t.Errorf("TopFailureReasons[timeout] = %d, want 1", gotReasons["timeout"])
	}
	if gotReasons["cancelled"] != 1 {
		t.Errorf("TopFailureReasons[cancelled] = %d, want 1", gotReasons["cancelled"])
	}
}

// TestGetPlatformAnalytics_SessionOutcomeCountsFetchFailedDegradesOnlyItsOwnFields
// is the defect this Step's own fix exists for, pinned at the wire level
// (web/src/session/__tests__/analyticsRendering.test.tsx proves the SAME
// property at the rendered-screen level). Before the fix, sessionsTotal
// carried NO computed sentinel at all -- a failed
// SessionOutcomeCountsInWindow fetch left it at its own Go zero value and
// the handler still answered 200, so a caller could not tell "0 sessions"
// from "the count query itself failed". deps.Sessions is built here on a
// pool that's already been Closed() -- every call through it fails
// deterministically with pgxpool.ErrClosedPool, mirroring internal/
// adapters/inbound/linear/authz_backend_error_integration_test.go's own
// "deterministic backend error, no real dropped connection needed"
// precedent -- while every OTHER store stays on the real pool. That
// proves BOTH halves of the fix at once: (1) sessionsTotalComputed AND
// the three other fields fed by this one shared read
// (sessionsPerDayComputed/successRateComputed/topFailureReasonsComputed)
// all go false together; (2) falseFailureCount/cost -- rollups whose OWN
// fetch never touched the broken pool -- still render their real,
// computed values, proving the handler's own deliberate
// partial-degrade-not-whole-request-failure posture (this file's own top
// comment) survives a real per-rollup failure, not just a "no session
// data yet" empty-window case (which TestGetPlatformAnalytics_
// ViewerAllowed_NothingComputedYet, above, already covers and could not
// tell apart from this one before the fix).
func TestGetPlatformAnalytics_SessionOutcomeCountsFetchFailedDegradesOnlyItsOwnFields(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		brokenPool, err := narvipg.NewPool(context.Background(), r.pool.Config().ConnString())
		if err != nil {
			t.Fatalf("open broken pool: %v", err)
		}
		brokenPool.Close()
		r.sessions = narvipg.NewSessionStore(brokenPool)
	})
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	// Fixture data seeded through a SEPARATE, healthy SessionStore
	// pointed at the SAME real pool (rig.pool) -- rig.sessions itself is
	// the one deliberately broken above, and is exactly the dependency
	// httpapi.GetPlatformAnalytics uses, so fixture setup must not route
	// through it. rig.turns/rig.falseFailures are untouched by the
	// mutate above and stay on the real pool throughout.
	healthySessions := narvipg.NewSessionStore(rig.pool)
	created, err := healthySessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create fixture session: %v", err)
	}
	seedCostedTurn(ctx, t, rig, created.ID, "sonnet-5", 10)
	if _, err := rig.falseFailures.Insert(ctx, created.ID); err != nil {
		t.Fatalf("seed false_failures row: %v", err)
	}

	var resp restdtos.PlatformAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d -- a per-rollup fetch failure must never fail the whole request", status, http.StatusOK)
	}

	// The failed rollup's own four fields: all uncomputed, all at their
	// own zero/nil value -- but crucially, sessionsTotalComputed being
	// false is what makes that zero honest rather than a lie.
	if resp.SessionsTotalComputed {
		t.Errorf("SessionsTotalComputed = true, want false (the fetch failed)")
	}
	if resp.SessionsTotal != 0 {
		t.Errorf("SessionsTotal = %d, want 0 (its own Go zero value -- SessionsTotalComputed is the field that actually matters here)", resp.SessionsTotal)
	}
	if resp.SessionsPerDayComputed {
		t.Errorf("SessionsPerDayComputed = true, want false")
	}
	if resp.SessionsPerDay != nil {
		t.Errorf("SessionsPerDay = %v, want nil", resp.SessionsPerDay)
	}
	if resp.SuccessRateComputed {
		t.Errorf("SuccessRateComputed = true, want false")
	}
	if resp.TopFailureReasonsComputed {
		t.Errorf("TopFailureReasonsComputed = true, want false")
	}

	// The rollups that did NOT fail must still render their real
	// computed values -- the partial-degrade property, not merely "no
	// 500".
	if !resp.FalseFailureCountComputed {
		t.Fatalf("FalseFailureCountComputed = false, want true (this rollup's own fetch never touches the broken Sessions pool)")
	}
	if resp.FalseFailureCount != 1 {
		t.Errorf("FalseFailureCount = %d, want 1", resp.FalseFailureCount)
	}
	if !resp.CostComputed {
		t.Fatalf("CostComputed = false, want true")
	}
	if resp.CostTotalUsd == nil || *resp.CostTotalUsd != 10 {
		t.Errorf("CostTotalUsd = %v, want 10", resp.CostTotalUsd)
	}
}

// TestGetPlatformAnalytics_FalseFailureCountFetchFailedDegradesOnlyItsOwnField
// is TestGetPlatformAnalytics_
// SessionOutcomeCountsFetchFailedDegradesOnlyItsOwnFields' own mirror
// image, over the OTHER field this Step's fix restored a sentinel to.
// Before the fix, falseFailureCount carried NO computed sentinel either
// -- a failed CountFalseFailuresInWindow fetch rendered "False failures:
// 0, target 0" (the target reads as MET) on a query that never ran.
// deps.FalseFailures alone is built on a Closed() pool here; every other
// store, INCLUDING deps.Sessions, stays real.
func TestGetPlatformAnalytics_FalseFailureCountFetchFailedDegradesOnlyItsOwnField(t *testing.T) {
	rig := newTestRig(t, func(r *testRig) {
		brokenPool, err := narvipg.NewPool(context.Background(), r.pool.Config().ConnString())
		if err != nil {
			t.Fatalf("open broken pool: %v", err)
		}
		brokenPool.Close()
		r.falseFailures = narvipg.NewFalseFailureStore(brokenPool)
	})
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	// rig.sessions is untouched here, so the shared helpers are safe to
	// use directly, unlike the sibling test above.
	completed := mustCreateSession(ctx, t, rig)
	seedCostedTurn(ctx, t, rig, completed, "sonnet-5", 10)
	mustCompleteSession(ctx, t, rig, completed)

	var resp restdtos.PlatformAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d -- a per-rollup fetch failure must never fail the whole request", status, http.StatusOK)
	}

	if resp.FalseFailureCountComputed {
		t.Errorf("FalseFailureCountComputed = true, want false (the fetch failed)")
	}
	if resp.FalseFailureCount != 0 {
		t.Errorf("FalseFailureCount = %d, want 0 (its own Go zero value -- FalseFailureCountComputed is the field that actually matters here)", resp.FalseFailureCount)
	}

	// Every OTHER rollup, including the one sharing the SessionStore this
	// one does not touch, must still render its real computed value.
	if !resp.SessionsTotalComputed {
		t.Fatalf("SessionsTotalComputed = false, want true")
	}
	if resp.SessionsTotal != 1 {
		t.Errorf("SessionsTotal = %d, want 1", resp.SessionsTotal)
	}
	if !resp.CostComputed {
		t.Fatalf("CostComputed = false, want true")
	}
	if resp.CostTotalUsd == nil || *resp.CostTotalUsd != 10 {
		t.Errorf("CostTotalUsd = %v, want 10", resp.CostTotalUsd)
	}
}

// TestGetPlatformAnalytics_BootP95_TooFewSamples_StaysUncomputedButReportsSampleSize
// proves the "too few samples" gate is genuinely distinct from "no data
// at all": both render bootP95Computed=false, but the sample size itself
// must still be honest, so a caller CAN distinguish 0 from "a few, just
// not enough yet" rather than collapsing both into one indistinguishable
// state.
func TestGetPlatformAnalytics_BootP95_TooFewSamples_StaysUncomputedButReportsSampleSize(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	session := mustCreateSession(ctx, t, rig)
	const seeded = domainpa.BootP95MinSamples - 1
	for i := 1; i <= seeded; i++ {
		seedBootTimingEvent(ctx, t, rig, session, fmt.Sprintf("boot-%d", i), float64(i), false)
	}

	var resp restdtos.PlatformAnalytics
	status := rig.doJSON(t, http.MethodGet, "/api/analytics", nil, &resp, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if resp.BootP95Computed {
		t.Errorf("BootP95Computed = true, want false (one below the minimum)")
	}
	if resp.BootP95Seconds != nil {
		t.Errorf("BootP95Seconds = %v, want nil", resp.BootP95Seconds)
	}
	if resp.BootP95SampleSize != seeded {
		t.Errorf("BootP95SampleSize = %d, want %d (real sample count, even though not yet computed)", resp.BootP95SampleSize, seeded)
	}
}

// -- fixture helpers, kept local to this file: none of the shapes below
// are shared with reviewanalytics_integration_test.go's own helpers. --

func mustCreateSession(ctx context.Context, t *testing.T, rig testRig) pgtype.UUID {
	t.Helper()
	created, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return created.ID
}

// seedCostedTurn creates one turn on sessionID, dispatched on modelID,
// moves it to processing, records one step's cost via the SAME
// RecordStepCostUSD path production code uses (never a hand-crafted
// UPDATE), then leaves it processing -- the caller transitions the
// SESSION itself afterward (mustCompleteSession).
// seedCostedTurn creates one costed turn on sessionID. modelID == "" means
// "never recorded" (turns.model_id left NULL) -- the case
// ListCostByModelInWindow's own COALESCE(model_id, 'unknown') bucketing
// exists for; every other caller passes a real model name.
func seedCostedTurn(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, modelID string, amountUSD float64) {
	t.Helper()
	var model *string
	if modelID != "" {
		model = &modelID
	}
	created, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, ModelID: model})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusProcessing}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}
	if _, err := rig.turns.RecordStepCostUSD(ctx, sessionID, "step-1", amountUSD); err != nil {
		t.Fatalf("record step cost: %v", err)
	}
}

func mustCompleteSession(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
	t.Helper()
	turns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil || len(turns) == 0 {
		t.Fatalf("list turns for session: %v (turns=%d)", err, len(turns))
	}
	last := turns[len(turns)-1]
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: last.ID, Status: sqlcgen.TurnStatusCompleted}); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	completedStatus := sqlcgen.SessionStatusCompleted
	if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sessionID, Status: completedStatus}); err != nil {
		t.Fatalf("complete session: %v", err)
	}
}

func mustFailSessionTimeout(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
	t.Helper()
	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusFailed}); err != nil {
		t.Fatalf("create failed turn: %v", err)
	}
	failedStatus := sqlcgen.SessionStatusFailed
	timeoutReason := sqlcgen.SessionFailureReasonTimeout
	if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sessionID, Status: failedStatus, FailureReason: &timeoutReason}); err != nil {
		t.Fatalf("fail session: %v", err)
	}
}

func mustCancelSession(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
	t.Helper()
	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusCancelled}); err != nil {
		t.Fatalf("create cancelled turn: %v", err)
	}
	cancelledStatus := sqlcgen.SessionStatusCancelled
	cancelledReason := sqlcgen.SessionFailureReasonCancelled
	if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sessionID, Status: cancelledStatus, FailureReason: &cancelledReason}); err != nil {
		t.Fatalf("cancel session: %v", err)
	}
}

func seedBootTimingEvent(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, messageID string, seconds float64, failed bool) {
	t.Helper()
	payload := fmt.Sprintf(`{"type":"boot_timing","messageId":%q,"sessionId":"ignored","gen":1,"metric":"boot_duration","seconds":%f,"failed":%t}`, messageID, seconds, failed)
	if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
		SessionID: sessionID,
		Type:      "boot_timing",
		MessageID: messageID,
		Payload:   []byte(payload),
	}); err != nil {
		t.Fatalf("seed boot_timing event: %v", err)
	}
}

//go:build integration

package automerge_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
)

// This file pins technical plan §40.2 at the auto-merge worker: while
// autonomy is frozen no candidate is read, merged, confirmed or audited;
// a freeze committed while a candidate is being re-validated stops its
// merge; and once the freeze lifts the candidate merges on the next tick.

// otelReader is the ManualReader TestMain installs for this binary.
var otelReader *sdkmetric.ManualReader

// freezeAutonomy freezes autonomy on pool, as the admin action will, and
// lifts it when the test ends if it is still frozen.
func freezeAutonomy(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	settings := narvipg.NewPlatformSettingsStore(pool)
	if _, err := settings.Freeze(context.Background(), pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
}

func unfreezeAutonomy(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := narvipg.NewPlatformSettingsStore(pool).Unfreeze(context.Background()); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
}

// autoMergeSkips reads autonomy_freeze_skip_total{site=auto_merge,
// reason=frozen}.
func autoMergeSkips(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("site", string(domainautonomy.SiteAutoMerge)), attribute.String("reason", string(domainautonomy.SkipFrozen)))
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

// armedCandidate seeds an armed repository with one eligible candidate and
// returns the fake code host that answers for it.
func armedCandidate(ctx context.Context, t *testing.T, rig *automergeTestRig, repoFullName, repo string, prNumber int32, headSHA string) *fakeAutoMergeSourceControl {
	t.Helper()
	htmlURL := rig.seedEligiblePR(ctx, t, repoFullName, prNumber, headSHA)
	if _, err := rig.repoSettings.UpsertAutoMergeToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("arm auto-merge: %v", err)
	}
	return &fakeAutoMergeSourceControl{
		prsByKey: map[string]ports.OpenPR{
			repoFullName + "#" + itoa(int(prNumber)): {
				Owner: "acme", Repo: repo, Number: int(prNumber), HTMLURL: htmlURL,
				HeadSHA: headSHA, BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA, CIConclusion: ports.CIConclusionSuccess,
			},
		},
		mergeSHA: "merged-after-the-freeze",
	}
}

// confirmedOutcomes counts the contradiction-rate outcomes recorded for
// repoFullName in the last hour: a merge records one.
func confirmedOutcomes(ctx context.Context, t *testing.T, rig *automergeTestRig, repoFullName string) int64 {
	t.Helper()
	total, _, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	return total
}

// TestAutoMerge_Frozen_NoReadNoMerge: an armed repository's eligible
// candidate, while autonomy is frozen, is skipped before anything is spent
// on it -- no GitHub read, no merge, no confirmed outcome, no audit row --
// and the skip is counted.
func TestAutoMerge_Frozen_NoReadNoMerge(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-frozen"
	sc := armedCandidate(ctx, t, rig, repoFullName, "automerge-frozen", 11, "sha-frozen")
	worker := rig.newWorker(t, sc)
	freezeAutonomy(t, rig.pool)

	before := autoMergeSkips(t)
	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}
	if got := sc.getOpenPRCallCount(); got != 0 {
		t.Errorf("GetOpenPR calls = %d, want 0: a frozen candidate spends no GitHub read", got)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Errorf("MergePR calls = %d, want 0", got)
	}
	if got := countAuditLogEntries(t, rig.pool, "auto_merge.merged"); got != 0 {
		t.Errorf("auto_merge.merged audit rows = %d, want 0", got)
	}
	if got := confirmedOutcomes(ctx, t, rig, repoFullName); got != 0 {
		t.Errorf("auto-approval outcomes = %d, want 0: a skip is no confirmation", got)
	}
	if got := autoMergeSkips(t) - before; got != 1 {
		t.Errorf("autonomy_freeze_skip_total{site=auto_merge, reason=frozen} rose by %d, want 1", got)
	}
}

// TestAutoMerge_FreezeDuringRevalidation_NoMerge: a freeze committed while
// the candidate's live re-validation is in flight -- after the first read
// found autonomy free -- stops the merge: the worker reads the freeze
// again right before merging, never trusting its first read.
func TestAutoMerge_FreezeDuringRevalidation_NoMerge(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-freeze-mid-revalidation"
	sc := armedCandidate(ctx, t, rig, repoFullName, "automerge-freeze-mid-revalidation", 12, "sha-mid")
	// The hook runs on the worker's goroutine: it records its error for
	// the test goroutine to check rather than failing the test there.
	settings := narvipg.NewPlatformSettingsStore(rig.pool)
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
	var freezeErr error
	sc.onGetOpenPR = func() {
		_, freezeErr = settings.Freeze(context.Background(), pgtype.UUID{}, "an incident: frozen mid re-validation")
	}
	worker := rig.newWorker(t, sc)

	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}
	if freezeErr != nil {
		t.Fatalf("freeze autonomy during re-validation: %v", freezeErr)
	}
	if got := sc.getOpenPRCallCount(); got != 1 {
		t.Fatalf("GetOpenPR calls = %d, want 1: the candidate was re-validated before the freeze", got)
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Errorf("MergePR calls = %d, want 0: the freeze committed during re-validation", got)
	}
	if got := countAuditLogEntries(t, rig.pool, "auto_merge.merged"); got != 0 {
		t.Errorf("auto_merge.merged audit rows = %d, want 0", got)
	}
	if got := confirmedOutcomes(ctx, t, rig, repoFullName); got != 0 {
		t.Errorf("auto-approval outcomes = %d, want 0", got)
	}
}

// TestAutoMerge_Unfreeze_MergesOnce: a candidate held through frozen ticks
// is still a candidate once the freeze lifts, and the next tick merges it
// once, confirmed and audited like any merge.
func TestAutoMerge_Unfreeze_MergesOnce(t *testing.T) {
	rig := newAutomergeTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/automerge-unfreeze"
	sc := armedCandidate(ctx, t, rig, repoFullName, "automerge-unfreeze", 13, "sha-unfreeze")
	worker := rig.newWorker(t, sc)
	freezeAutonomy(t, rig.pool)

	for tick := 0; tick < 2; tick++ {
		if err := worker.PumpOnce(ctx, time.Now()); err != nil {
			t.Fatalf("frozen PumpOnce() error = %v, want nil", err)
		}
	}
	if got := sc.mergeCallCount(); got != 0 {
		t.Fatalf("MergePR calls while frozen = %d, want 0", got)
	}

	unfreezeAutonomy(t, rig.pool)
	if err := worker.PumpOnce(ctx, time.Now()); err != nil {
		t.Fatalf("PumpOnce() after the unfreeze error = %v, want nil", err)
	}
	if got := sc.mergeCallCount(); got != 1 {
		t.Fatalf("MergePR calls after the unfreeze = %d, want 1", got)
	}
	if got := sc.mergeCalls[0].HeadSHA; got != "sha-unfreeze" {
		t.Errorf("MergePR HeadSHA = %q, want sha-unfreeze", got)
	}
	if got := countAuditLogEntries(t, rig.pool, "auto_merge.merged"); got != 1 {
		t.Errorf("auto_merge.merged audit rows = %d, want 1", got)
	}
	if got := confirmedOutcomes(ctx, t, rig, repoFullName); got != 1 {
		t.Errorf("auto-approval outcomes = %d, want 1", got)
	}
}

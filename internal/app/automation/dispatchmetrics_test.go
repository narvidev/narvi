//go:build !integration

// This file (dispatchmetrics_test.go) is Y2 audit fix's own required proof
// (confirmed MEDIUM finding: "the drop counter misses the drop with the
// largest blast radius"). The shared dispatch time budget
// (platform.Timeouts.AutomationDispatchTotalBudget) expiring during
// ListActiveGitHubAutomations/ListActiveLinearAutomations -- before a
// single automation row is even read -- drops EVERY automation that
// delivery would have matched, and, before this fix, nothing counted it:
// automation_dispatch_dropped_total (dispatchmetrics.go) was only
// incremented at the throttle_check and create stages, both of which run
// per already-listed automation. This file proves the third stage
// (githubdispatch.go/lineardispatch.go's own "list" branch) actually
// increments the real OTel counter, not just logs a line that happens to
// sit next to a counter call nobody asserts on.
//
// TestMain here installs the ONE, whole-binary-wide ManualReader-backed
// MeterProvider this file's own tests read from -- mirrors identitylink's
// own retry_test.go TestMain precedent exactly, including its own reasoning
// for why this must be exactly one global otel.SetMeterProvider call, ever,
// for the whole process (go.opentelemetry.io/otel's own global package
// upgrades an already-obtained instrument's delegate in place the FIRST
// time SetMeterProvider runs; automationDispatchDroppedTotalCounter is a
// sync.OnceValue singleton, constructed at most once regardless of how many
// tests in this binary reach it).
//
// Build-tagged !integration, specifically to avoid colliding with that:
// sharedpool_integration_test.go (package automation) already declares
// TestMain for the `-tags=integration` build of this same package/binary,
// and Go allows only one TestMain across a combined test binary -- without
// this tag, `go test -tags=integration ./internal/app/automation/...`
// would fail to compile with both TestMains present.
package automation_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/automation"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/platform"
)

var otelReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	otelReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelReader))
	otel.SetMeterProvider(mp)

	code := m.Run()
	_ = mp.Shutdown(context.Background())
	os.Exit(code)
}

// readDispatchDroppedCount sums every automation_dispatch_dropped_total
// data point labeled the given stage -- CUMULATIVE across every test in
// this binary, so callers must diff a "before" and "after" reading around
// their own dispatch call(s), exactly like identitylink's own
// readEmailFetchFailureCount precedent (retry_test.go).
func readDispatchDroppedCount(t *testing.T, stage string) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/automation-dispatch" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "automation_dispatch_dropped_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("automation_dispatch_dropped_total metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key("stage"))
				if ok && v.AsString() == stage {
					total += dp.Value
				}
			}
			return total
		}
	}
	return 0
}

// alwaysDeadlineExceededGitHubLister is a GitHubTriggerLister fake whose
// ListActiveGitHubAutomations always fails with context.DeadlineExceeded --
// dispatchBudgetExhausted's own exact trigger (dispatchgate_internal_test.go's
// TestDispatchBudgetExhausted pins the identical classification).
// MarkCreatorUnauthorized/ClearCreatorUnauthorized are never called (the
// list call itself fails, before any row -- let alone a machine-origin
// gate -- is ever reached).
type alwaysDeadlineExceededGitHubLister struct{ calls int }

func (f *alwaysDeadlineExceededGitHubLister) ListActiveGitHubAutomations(_ context.Context) ([]sqlcgen.Automation, error) {
	f.calls++
	return nil, context.DeadlineExceeded
}

func (f *alwaysDeadlineExceededGitHubLister) MarkCreatorUnauthorized(_ context.Context, _ pgtype.UUID) (int64, error) {
	return 0, errors.New("unexpected call")
}

func (f *alwaysDeadlineExceededGitHubLister) ClearCreatorUnauthorized(_ context.Context, _ pgtype.UUID) (int64, error) {
	return 0, errors.New("unexpected call")
}

// TestDispatchGitHubWebhookEvent_ListBudgetExhaustionIsCounted is Y2's own
// required proof for GitHub: the shared budget expiring during
// ListActiveGitHubAutomations must increment
// automation_dispatch_dropped_total{stage="list"} by exactly one, mirroring
// the pre-existing throttle_check/create stages' own already-tested
// behavior for their own call sites.
func TestDispatchGitHubWebhookEvent_ListBudgetExhaustionIsCounted(t *testing.T) {
	before := readDispatchDroppedCount(t, "list")

	lister := &alwaysDeadlineExceededGitHubLister{}
	automation.DispatchGitHubWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, nil, platform.DefaultTimeouts(), "pull_request", "delivery-list-budget-exhausted-github-1", domainautomation.GitHubEventInput{EventType: "pull_request", RepoFullName: "acme/repo"})

	if lister.calls != platform.DefaultTimeouts().AutomationDispatchMaxAttempts {
		t.Fatalf("ListActiveGitHubAutomations call count = %d, want %d (retried to the budget, every attempt failing)", lister.calls, platform.DefaultTimeouts().AutomationDispatchMaxAttempts)
	}

	after := readDispatchDroppedCount(t, "list")
	if delta := after - before; delta != 1 {
		t.Fatalf("automation_dispatch_dropped_total{stage=list} delta = %d, want 1", delta)
	}
}

// alwaysDeadlineExceededLinearLister is DispatchLinearWebhookEvent's own
// twin fixture.
type alwaysDeadlineExceededLinearLister struct{ calls int }

func (f *alwaysDeadlineExceededLinearLister) ListActiveLinearAutomations(_ context.Context, _ string) ([]sqlcgen.Automation, error) {
	f.calls++
	return nil, context.DeadlineExceeded
}

// TestDispatchLinearWebhookEvent_ListBudgetExhaustionIsCounted mirrors the
// GitHub proof above, for the Linear path.
func TestDispatchLinearWebhookEvent_ListBudgetExhaustionIsCounted(t *testing.T) {
	before := readDispatchDroppedCount(t, "list")

	lister := &alwaysDeadlineExceededLinearLister{}
	automation.DispatchLinearWebhookEvent(context.Background(), discardLogger(), lister, noopDeliveryInvocationCreator{}, platform.DefaultTimeouts(), "Issue", "delivery-list-budget-exhausted-linear-1", domainautomation.LinearEventInput{EventType: "Issue", OrganizationID: "org-1"})

	if lister.calls != platform.DefaultTimeouts().AutomationDispatchMaxAttempts {
		t.Fatalf("ListActiveLinearAutomations call count = %d, want %d (retried to the budget, every attempt failing)", lister.calls, platform.DefaultTimeouts().AutomationDispatchMaxAttempts)
	}

	after := readDispatchDroppedCount(t, "list")
	if delta := after - before; delta != 1 {
		t.Fatalf("automation_dispatch_dropped_total{stage=list} delta = %d, want 1", delta)
	}
}

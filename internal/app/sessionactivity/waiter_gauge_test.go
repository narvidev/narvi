// This file (waiter_gauge_test.go) proves mcp_session_waits_active, the
// OTel up-down counter technical plan §43.20 names as the active-wait
// count, comes back to zero on every path a wait can end by (review round
// 1's P5) -- the exported metric itself, not the Waiter's own count beside
// it, which a lost decrement would leave untouched.
//
// TestMain installs the ONE whole-binary ManualReader-backed MeterProvider
// the gauge is read from, the codebase's convention (automation's
// dispatchmetrics_test.go, identitylink's retry_test.go): the global
// package hands the gauge -- built once, lazily, by activeWaitsGauge -- to
// the first provider SetMeterProvider installs, so there is exactly one
// such call in this binary, before any test runs. The package has no
// integration-tagged test, so no second TestMain can collide with it.
package sessionactivity_test

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// activeWaitsGauge is mcp_session_waits_active's current value -- the sum
// of every Add since the binary started, across every Waiter, so zero
// whenever no wait is running -- or zero when it was never recorded.
func activeWaitsGauge(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Errorf("collect metrics: %v", err)
		return -1
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/sessionactivity" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "mcp_session_waits_active" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Errorf("mcp_session_waits_active is %T, want an int64 sum", m.Data)
				return -1
			}
			if sum.IsMonotonic {
				t.Errorf("mcp_session_waits_active is monotonic, want an up-down counter")
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// TestWait_ActiveGaugeReturnsToZeroOnEveryPath: on every path a wait can
// end by (waitPaths: answered by its first read, admitted and ended by a
// later settled read, its bound, a read error, the client going away, an
// interrupt or a panic in its read, or refused by each of the three caps),
// the exported gauge counts each admitted wait while it runs -- read from
// inside that wait's own later read, or while blockers hold a cap, so the
// gauge is proven wired, not merely zero -- and is back to zero once the
// waits have ended; a refused wait never moves it. Mutation: the release's
// gauge decrement dropped (the gauge climbs by one per admitted wait).
func TestWait_ActiveGaugeReturnsToZeroOnEveryPath(t *testing.T) {
	for _, p := range waitPaths {
		t.Run(p.name, func(t *testing.T) {
			if g := activeWaitsGauge(t); g != 0 {
				t.Fatalf("mcp_session_waits_active = %d before the path, want 0 (a wait before this one was never given back)", g)
			}
			w := newPathWaiter(t, p)
			sawHeld := false
			p.run(t, w, func(n int) {
				sawHeld = true
				if g := activeWaitsGauge(t); g != int64(n) {
					t.Errorf("mcp_session_waits_active = %d while %d waits are admitted, want %d", g, n, n)
				}
			})
			if sawHeld != p.holds {
				t.Fatalf("held called: %v, want %v", sawHeld, p.holds)
			}
			if g := activeWaitsGauge(t); g != 0 {
				t.Fatalf("mcp_session_waits_active = %d after the wait ended, want 0", g)
			}
		})
	}
}

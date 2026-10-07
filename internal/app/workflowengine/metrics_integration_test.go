//go:build integration

package workflowengine_test

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
)

// otelReader reads this binary's one meter provider, installed before any
// test builds an autonomy.Gate: a Gate takes its counter from the global
// provider when it is built.
var otelReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	otelReader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelReader))
	otel.SetMeterProvider(provider)
	code := m.Run()
	_ = provider.Shutdown(context.Background())
	os.Exit(code)
}

// workflowAdvanceSkips reads autonomy_freeze_skip_total{site=
// workflow_advance, reason}.
func workflowAdvanceSkips(t *testing.T, reason domainautonomy.SkipReason) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("site", string(domainautonomy.SiteWorkflowAdvance)), attribute.String("reason", string(reason)))
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

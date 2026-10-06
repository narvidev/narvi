package autonomy_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/internal/app/autonomy"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
)

var otelReader *sdkmetric.ManualReader

// TestMain installs the one ManualReader-backed meter provider this
// binary's counter assertions read, before any Gate is built: a Gate takes
// its counter from the global provider when it is built.
func TestMain(m *testing.M) {
	otelReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelReader))
	otel.SetMeterProvider(mp)
	code := m.Run()
	_ = mp.Shutdown(context.Background())
	os.Exit(code)
}

// skipCount reads autonomy_freeze_skip_total{site, reason}.
func skipCount(t *testing.T, site domainautonomy.Site, reason domainautonomy.SkipReason) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("site", string(site)), attribute.String("reason", string(reason)))
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

// closedPool is a pool whose every read fails: built without connecting,
// then closed.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://narvi@127.0.0.1:1/narvi?connect_timeout=1")
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	pool.Close()
	return pool
}

// TestGate_ReadErrorIsASkip pins §40.2's fail direction: a freeze that
// cannot be read is a skip with reason freeze_unreadable -- never a pass --
// and it is counted like any other skip, under its own reason.
func TestGate_ReadErrorIsASkip(t *testing.T) {
	gate, err := autonomy.NewGate(closedPool(t))
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	for _, site := range domainautonomy.AllSites {
		t.Run(string(site), func(t *testing.T) {
			before := skipCount(t, site, domainautonomy.SkipFreezeUnreadable)
			skip, reason := gate.Check(context.Background(), site, "test", site)
			if !skip || reason != domainautonomy.SkipFreezeUnreadable {
				t.Fatalf("Check on an unreadable freeze = (%v, %q), want (true, %q)", skip, reason, domainautonomy.SkipFreezeUnreadable)
			}
			if got := skipCount(t, site, domainautonomy.SkipFreezeUnreadable) - before; got != 1 {
				t.Fatalf("autonomy_freeze_skip_total{site=%s, reason=freeze_unreadable} rose by %d, want 1", site, got)
			}
		})
	}
}

// TestGate_RecordSkip pins the counter's labels: one per call, under the
// site and the reason given, and nothing under the other reason.
func TestGate_RecordSkip(t *testing.T) {
	gate, err := autonomy.NewGate(closedPool(t))
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	for _, tc := range []struct {
		site   domainautonomy.Site
		reason domainautonomy.SkipReason
		other  domainautonomy.SkipReason
	}{
		{domainautonomy.SiteAutoReReview, domainautonomy.SkipFrozen, domainautonomy.SkipFreezeUnreadable},
		{domainautonomy.SiteAutomationFanOut, domainautonomy.SkipFreezeUnreadable, domainautonomy.SkipFrozen},
	} {
		t.Run(string(tc.site)+"/"+string(tc.reason), func(t *testing.T) {
			before, otherBefore := skipCount(t, tc.site, tc.reason), skipCount(t, tc.site, tc.other)
			gate.RecordSkip(context.Background(), tc.site, tc.reason)
			gate.RecordSkip(context.Background(), tc.site, tc.reason)
			if got := skipCount(t, tc.site, tc.reason) - before; got != 2 {
				t.Fatalf("counter under (%s, %s) rose by %d, want 2", tc.site, tc.reason, got)
			}
			if got := skipCount(t, tc.site, tc.other) - otherBefore; got != 0 {
				t.Fatalf("counter under (%s, %s) rose by %d, want 0", tc.site, tc.other, got)
			}
		})
	}
}

// failingTx is a transaction whose every row read fails, as a statement in
// an aborted transaction does. Only QueryRow is implemented: the freeze
// read is the one statement FrozenTx sends.
type failingTx struct{ pgx.Tx }

func (failingTx) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow{} }

type failingRow struct{}

func (failingRow) Scan(...any) error {
	return errors.New("ERROR: current transaction is aborted, commands ignored until end of transaction block (SQLSTATE 25P02)")
}

// TestGate_FrozenTxReturnsTheReadError pins FrozenTx's contract: a failed
// read inside the caller's transaction is returned to the caller, which
// records the skip and rolls back -- never read as "not frozen".
func TestGate_FrozenTxReturnsTheReadError(t *testing.T) {
	gate, err := autonomy.NewGate(closedPool(t))
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	frozen, err := gate.FrozenTx(context.Background(), failingTx{})
	if err == nil {
		t.Fatalf("FrozenTx on a failing transaction = (%v, nil), want the read's error", frozen)
	}
	if frozen {
		t.Fatal("FrozenTx returned frozen alongside its error; the caller must decide from the error alone")
	}
}

// TestGate_FrozenReturnsTheReadError pins Frozen's contract: a read that
// fails is returned, never reported as "not frozen", and records no skip
// -- its caller, the outbox's lag gauge and lanes, treats the error as
// holding.
func TestGate_FrozenReturnsTheReadError(t *testing.T) {
	gate, err := autonomy.NewGate(closedPool(t))
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	before := map[domainautonomy.Site]int64{}
	for _, site := range domainautonomy.AllSites {
		before[site] = skipCount(t, site, domainautonomy.SkipFreezeUnreadable)
	}
	frozen, err := gate.Frozen(context.Background())
	if err == nil {
		t.Fatalf("Frozen on an unreadable freeze = (%v, nil), want the read's error", frozen)
	}
	if frozen {
		t.Fatal("Frozen returned frozen alongside its error; the caller must decide from the error alone")
	}
	for _, site := range domainautonomy.AllSites {
		if got := skipCount(t, site, domainautonomy.SkipFreezeUnreadable) - before[site]; got != 0 {
			t.Fatalf("Frozen recorded %d skip(s) for %s, want none: it is no site's read", got, site)
		}
	}
}

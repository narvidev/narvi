package ops

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanRegisteredInstruments_FindsLiteralNames proves the scanner's own
// core extraction: instrumentMethods' synchronous and observable
// constructors, called with a string-literal first argument, are found.
func TestScanRegisteredInstruments_FindsLiteralNames(t *testing.T) {
	dir := t.TempDir()
	src := `package fake

import "go.opentelemetry.io/otel/metric"

func build(m metric.Meter) {
	_, _ = m.Int64Counter("fake_counter_total")
	_, _ = m.Float64Histogram("fake_histogram_seconds")
	_, _ = m.Int64Gauge("fake_gauge")
	_, _ = m.Float64UpDownCounter("fake_updown")
	_, _ = m.Int64ObservableGauge("fake_observable_gauge")
	_, _ = m.Int64ObservableCounter("fake_observable_counter")
}
`
	if err := os.WriteFile(filepath.Join(dir, "fake.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fake.go: %v", err)
	}

	got, err := ScanRegisteredInstruments(dir)
	if err != nil {
		t.Fatalf("ScanRegisteredInstruments: %v", err)
	}
	want := []string{"fake_counter_total", "fake_histogram_seconds", "fake_gauge", "fake_updown", "fake_observable_gauge", "fake_observable_counter"}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("missing instrument %q in %v", name, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d instruments, want exactly %d: %v", len(got), len(want), got)
	}
}

// TestScanRegisteredInstruments_SkipsTestFiles proves a _test.go file's
// own instrument registration is never counted as real production
// telemetry -- a dashboard/alert must not be allowed to "pass" the drift
// check by pointing at a name that only exists inside a test.
func TestScanRegisteredInstruments_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	src := `package fake

import "go.opentelemetry.io/otel/metric"

func build(m metric.Meter) {
	_, _ = m.Int64Counter("test_only_counter")
}
`
	if err := os.WriteFile(filepath.Join(dir, "fake_test.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fake_test.go: %v", err)
	}
	got, err := ScanRegisteredInstruments(dir)
	if err != nil {
		t.Fatalf("ScanRegisteredInstruments: %v", err)
	}
	if _, ok := got["test_only_counter"]; ok {
		t.Error("a _test.go file's own instrument must be skipped, not counted as registered")
	}
}

// TestScanRegisteredInstruments_IgnoresNonLiteralAndUnrelatedCalls proves
// two negative cases together: a metric-shaped method call whose first
// argument is NOT a string literal (a computed name) is silently skipped
// rather than erroring or panicking, and a call to an unrelated method of
// the same name shape (not one of instrumentMethods) is ignored entirely.
func TestScanRegisteredInstruments_IgnoresNonLiteralAndUnrelatedCalls(t *testing.T) {
	dir := t.TempDir()
	src := `package fake

import "go.opentelemetry.io/otel/metric"

func build(m metric.Meter, name string) {
	_, _ = m.Int64Counter(name) // non-literal -- skipped, not an error
	_ = notAMeterMethod("some_string")
}

func notAMeterMethod(s string) string { return s }
`
	if err := os.WriteFile(filepath.Join(dir, "fake.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fake.go: %v", err)
	}
	got, err := ScanRegisteredInstruments(dir)
	if err != nil {
		t.Fatalf("ScanRegisteredInstruments: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("found %d instruments, want 0 (neither call site is a literal-named instrument registration): %v", len(got), got)
	}
}

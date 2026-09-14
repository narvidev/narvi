package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PanelType discriminates how a Panel's own Metrics should be rendered by
// whichever OTel-compatible backend the operator points the collector at
// (see doc.go / this Step's own PR description for why this repo commits
// to a backend-agnostic panel shape rather than a vendor-specific one).
type PanelType string

const (
	// PanelTypeGauge reads the latest value of a single OTel Gauge
	// instrument directly (e.g. outbox_lag_seconds) — no aggregation.
	PanelTypeGauge PanelType = "gauge"
	// PanelTypeCounterRate plots the per-interval increase of a single
	// OTel Counter instrument (e.g. orphans_reaped) — a rate, not a raw
	// monotonic total.
	PanelTypeCounterRate PanelType = "counter_rate"
	// PanelTypeHistogramQuantile plots one quantile (Panel.Quantile) of a
	// single OTel Histogram instrument (e.g. p95 of
	// sandbox_agent_boot_duration_seconds).
	PanelTypeHistogramQuantile PanelType = "histogram_quantile"
	// PanelTypeRatio divides two Counter instruments' own rates —
	// Metrics[0] (numerator) / Metrics[1] (denominator) — e.g.
	// watchdog_false_alarm_total / watchdog_activation_total. Always
	// exactly 2 metrics.
	PanelTypeRatio PanelType = "ratio"
)

// Panel is one chart on a Dashboard.
type Panel struct {
	// ID is a stable, unique-within-its-dashboard slug — the identifier
	// CheckDrift's own error messages and this Step's mutation-testing
	// verification refer to a panel by.
	ID string `json:"id"`
	// Title is the human-readable panel name.
	Title string `json:"title"`
	// Description explains what the panel shows and why it matters
	// operationally — required, not decorative: an undocumented panel is
	// exactly the kind of artifact that looks maintained but isn't.
	Description string    `json:"description"`
	Type        PanelType `json:"type"`
	// Metrics names every OTel instrument this panel reads — exactly the
	// instrument names ScanRegisteredInstruments extracts from source,
	// byte for byte (e.g. "outbox_lag_seconds", never a display label).
	// Exactly 1 entry for gauge/counter_rate/histogram_quantile; exactly
	// 2 for ratio (numerator, denominator).
	Metrics []string `json:"metrics"`
	// Quantile is required (0, 1) exclusive when Type ==
	// PanelTypeHistogramQuantile; zero-valued and ignored otherwise.
	Quantile float64 `json:"quantile,omitempty"`
	// Unit is a short display unit ("s", "ratio", "{event}", ...) — not
	// cross-checked against the instrument's own registered OTel unit
	// (metric.WithUnit) by this Step's own drift check; a mismatch there
	// is a smaller, cosmetic drift class left for a future Step, not this
	// one's own "silent forever" hazard (§10-P6's own framing this
	// package's doc.go quotes).
	Unit string `json:"unit"`
}

// Dashboard is one dashboards-as-JSON file
// (deploy/observability/dashboards/*.json).
type Dashboard struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Panels      []Panel `json:"panels"`

	sourcePath string
}

// SourcePath is the file Dashboard was loaded from — set by LoadDashboards,
// empty for a Dashboard built directly (e.g. in a unit test).
func (d Dashboard) SourcePath() string { return d.sourcePath }

// Validate reports the first structural problem with d — every field
// LoadDashboards requires present and every enum-shaped field's value
// actually legal, mirroring this codebase's own established
// CreateSpec.Validate()-at-the-edge discipline (internal/app/ports/
// createspec.go).
func (d Dashboard) Validate() error {
	if d.Title == "" {
		return fmt.Errorf("ops: dashboard: title is required")
	}
	if len(d.Panels) == 0 {
		return fmt.Errorf("ops: dashboard %q: at least one panel is required", d.Title)
	}
	seen := make(map[string]bool, len(d.Panels))
	for _, p := range d.Panels {
		if p.ID == "" {
			return fmt.Errorf("ops: dashboard %q: panel with empty id", d.Title)
		}
		if seen[p.ID] {
			return fmt.Errorf("ops: dashboard %q: duplicate panel id %q", d.Title, p.ID)
		}
		seen[p.ID] = true
		if p.Title == "" {
			return fmt.Errorf("ops: dashboard %q: panel %q: title is required", d.Title, p.ID)
		}
		if p.Description == "" {
			return fmt.Errorf("ops: dashboard %q: panel %q: description is required", d.Title, p.ID)
		}
		if len(p.Metrics) == 0 {
			return fmt.Errorf("ops: dashboard %q: panel %q: at least one metric is required", d.Title, p.ID)
		}
		switch p.Type {
		case PanelTypeGauge, PanelTypeCounterRate, PanelTypeHistogramQuantile, PanelTypeRatio:
		default:
			return fmt.Errorf("ops: dashboard %q: panel %q: unknown type %q", d.Title, p.ID, p.Type)
		}
		if p.Type == PanelTypeHistogramQuantile && (p.Quantile <= 0 || p.Quantile >= 1) {
			return fmt.Errorf("ops: dashboard %q: panel %q: type histogram_quantile requires 0<quantile<1, got %v", d.Title, p.ID, p.Quantile)
		}
		if p.Type == PanelTypeRatio && len(p.Metrics) != 2 {
			return fmt.Errorf("ops: dashboard %q: panel %q: type ratio requires exactly 2 metrics (numerator, denominator), got %d", d.Title, p.ID, len(p.Metrics))
		}
		if p.Unit == "" {
			return fmt.Errorf("ops: dashboard %q: panel %q: unit is required", d.Title, p.ID)
		}
	}
	return nil
}

// Alert is one alert rule inside an alerts-as-JSON file
// (deploy/observability/alerts/*.json).
type Alert struct {
	// Name is a stable, unique alert identifier (PascalCase, matching
	// this file's own convention — e.g. "OutboxLagHigh").
	Name        string `json:"name"`
	Description string `json:"description"`
	// Metrics names every OTel instrument this alert's Condition reads —
	// exactly 1 for a single-instrument threshold, exactly 2
	// (numerator, denominator) for a ratio condition.
	Metrics []string `json:"metrics"`
	// Condition is a short, human-readable trigger expression (not
	// executable by this repo — no alerting backend is pinned, see
	// doc.go) — e.g. "p95(outbox_lag_seconds) > 300s for 10m".
	Condition string `json:"condition"`
	Severity  string `json:"severity"` // "warning" | "critical"
	// ThresholdDerivation states, in prose, WHY this alert's own number
	// is what it is — required: "a threshold with no derivation is a
	// guess someone will silence" (this Step's own brief). Where a real
	// platform.Timeouts constant grounds the number, this names it.
	ThresholdDerivation string `json:"thresholdDerivation"`
	// Runbook is a repo-relative path to the runbook this alert should
	// send an operator to (e.g. "docs/runbooks/outbox-lag.md") — optional:
	// most alerts as shipped name one, but AutoMergeAuthDeadLetterAny
	// (deploy/observability/alerts/reliability.json, docs/
	// TECHNICAL_PLAN.md §17's own automerge dead-letter fix) deliberately
	// leaves this empty rather than pointing at an unrelated existing
	// runbook page — TestAlertRunbooksExist (drift_test.go) only checks a
	// NON-empty Runbook actually resolves to a real file, never requires
	// one.
	Runbook string `json:"runbook,omitempty"`

	sourcePath string
}

// SourcePath is the file Alert was loaded from.
func (a Alert) SourcePath() string { return a.sourcePath }

// Validate mirrors Dashboard.Validate's own discipline for Alert.
func (a Alert) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("ops: alert: name is required")
	}
	if a.Description == "" {
		return fmt.Errorf("ops: alert %q: description is required", a.Name)
	}
	if len(a.Metrics) == 0 {
		return fmt.Errorf("ops: alert %q: at least one metric is required", a.Name)
	}
	if a.Condition == "" {
		return fmt.Errorf("ops: alert %q: condition is required", a.Name)
	}
	switch a.Severity {
	case "warning", "critical":
	default:
		return fmt.Errorf("ops: alert %q: severity must be \"warning\" or \"critical\", got %q", a.Name, a.Severity)
	}
	if a.ThresholdDerivation == "" {
		return fmt.Errorf("ops: alert %q: thresholdDerivation is required — a threshold with no derivation is a guess someone will silence", a.Name)
	}
	return nil
}

// alertFile is the on-disk shape of one deploy/observability/alerts/*.json
// file — a thin wrapper so a single file can declare several related
// alerts together (mirroring Dashboard's own one-file-one-dashboard
// shape, but alerts group more naturally by subsystem than 1:1 per file).
type alertFile struct {
	Alerts []Alert `json:"alerts"`
}

// LoadDashboards reads every *.json file directly inside dir (no
// recursion — mirrors this repo's own flat contracts/*/v1/*.schema.json
// convention: one concern, one file, no nested surprise), in
// deterministic (sorted-by-filename) order, unmarshals each into a
// Dashboard, sets its SourcePath, and Validates it. The first error (a
// malformed file, or a Validate failure) aborts and is returned — this is
// the same "fail closed at the edge" discipline every loader elsewhere in
// this codebase already uses (e.g. platform.Load).
func LoadDashboards(dir string) ([]Dashboard, error) {
	paths, err := jsonFilesIn(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Dashboard, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("ops: read %s: %w", p, err)
		}
		var d Dashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("ops: unmarshal %s: %w", p, err)
		}
		d.sourcePath = p
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("ops: %s: %w", p, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// LoadAlerts mirrors LoadDashboards, flattening every file's own Alerts
// slice into one returned slice (each Alert's SourcePath is still the
// individual file it came from, for precise error messages).
func LoadAlerts(dir string) ([]Alert, error) {
	paths, err := jsonFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var out []Alert
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("ops: read %s: %w", p, err)
		}
		var f alertFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("ops: unmarshal %s: %w", p, err)
		}
		if len(f.Alerts) == 0 {
			return nil, fmt.Errorf("ops: %s: at least one alert is required", p)
		}
		for _, a := range f.Alerts {
			a.sourcePath = p
			if err := a.Validate(); err != nil {
				return nil, fmt.Errorf("ops: %s: %w", p, err)
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// jsonFilesIn returns every "*.json" path directly inside dir, sorted.
func jsonFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ops: read dir %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

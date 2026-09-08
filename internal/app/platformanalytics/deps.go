// Package platformanalytics is the app-layer half of §12.2 item 6's own
// platform-wide analytics rollup -- it aggregates Postgres
// (never itself I/O-free, unlike its sibling pure package
// internal/domain/platformanalytics, which this package calls but never
// duplicates the reduction logic of) and converts between the storage
// shapes (sqlcgen rows) and the pure domain shapes at exactly one seam,
// mirroring internal/app/reviewverdict's own identical split.
package platformanalytics

import (
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// Deps bundles every dependency this package's own functions need --
// constructed once at process wiring time (controlplane/serve.go),
// mirroring every other app-layer Deps struct in this codebase.
type Deps struct {
	Sessions      *postgres.SessionStore
	Turns         *postgres.TurnStore
	Events        *postgres.EventStore
	FalseFailures *postgres.FalseFailureStore

	Timeouts platform.Timeouts
}

// Package reviewverdict is the app-layer half of §21.1/§21.2's own
// verdict-persistence, analytics, and auto-approval-config read model --
// it aggregates Postgres (never itself I/O-free, unlike its sibling pure
// package internal/domain/reviewverdict, which this package calls but
// never duplicates the reduction logic of) and converts between the
// wire/storage shapes (sqlcgen rows, JSONB bytes) and the pure domain
// shapes (review.Verdict, reviewverdict.Record, autoapproval.
// EligibilityConfig) at exactly one seam, mirroring internal/app/
// decisioninbox's own identical split between "pure decision functions
// in internal/domain" and "Postgres aggregation here".
package reviewverdict

import (
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// Deps bundles every dependency this package's own functions need --
// constructed once at process wiring time (cmd/control-plane/main.go),
// mirroring every other app-layer Deps struct in this codebase.
type Deps struct {
	ReviewVerdicts       *postgres.ReviewVerdictStore
	RepoSettings         *postgres.RepoSettingsStore
	ReviewFindings       *postgres.ReviewFindingStore
	AutoApprovalOutcomes *postgres.AutoApprovalOutcomeStore
	// DigestSectionFeedback (§26.5) backs DigestContestationRate
	// below -- nil-safe: a nil store degrades that ONE function to
	// ok=false (never a panic), mirroring this package's own established
	// "a caller that doesn't need a given rollup simply never wires its
	// own store" convention (e.g. AutoApprovalOutcomes is already unused
	// by Timeseries/TopRiskDrivers/FindingOutcomes above).
	DigestSectionFeedback *postgres.ReviewDigestSectionFeedbackStore

	// Acceptances (§21.1b) backs Accept/Revoke/GetActiveAcceptance below
	// -- "human acceptance of a verdict the engine refuses". Nil-safe on
	// the SAME "a caller that doesn't need this rollup simply never
	// wires its own store" convention DigestSectionFeedback immediately
	// above already establishes: a nil store degrades GetActiveAcceptance
	// to ok=false (never a panic) and Accept/Revoke to a plain error,
	// never a silent no-op that could be mistaken for "no acceptance
	// exists".
	Acceptances *postgres.ReviewVerdictAcceptanceStore

	// PlatformShadow is the deployment-wide egress switch (§30.8), needed
	// alongside RepoSettings to stamp an outcome's own epoch -- see
	// recordOutcome. False on a wiring that never records outcomes.
	PlatformShadow bool

	Timeouts platform.Timeouts
}

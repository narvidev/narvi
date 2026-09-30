// Package reviewtriage is the app-layer half of §26.3's own light/deep
// review-depth routing decision (§26.3) -- it aggregates Postgres (never
// itself I/O-free, unlike its sibling pure package internal/domain/
// reviewtriage, which this package calls but never duplicates the
// decision logic of) and NEVER lets a failure in that aggregation reach
// its one caller as an error: ComputeDepth (compute.go) has no error
// return at all, by construction, mirroring internal/app/intentclassifier's
// own "never-throw contract" (§18.1) -- see that function's own doc
// comment for the full "why" this satisfies §26.3's "ANY triage error
// fails open to light" requirement structurally, not by convention.
package reviewtriage

import (
	"strconv"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// pullRequestHTMLURL builds the SAME deterministic "https://github.com/
// {owner}/{repo}/pull/{number}" shape GitHub's own REST API always
// returns as html_url -- duplicated here rather than exporting internal/
// app/outboxworker's own private identical helper, mirroring this
// codebase's own established "one small function, duplicated at each
// call site rather than exported purely for this" convention (e.g.
// internal/adapters/inbound/httpapi's own reviewTagsFromJSON).
func pullRequestHTMLURL(repoFullName string, prNumber int32) string {
	return "https://github.com/" + repoFullName + "/pull/" + strconv.Itoa(int(prNumber))
}

// Deps bundles every dependency ComputeDepth needs -- constructed once at
// process wiring time (cmd/control-plane/main.go), mirroring every other
// app-layer Deps struct in this codebase (e.g. internal/app/reviewverdict.
// Deps).
type Deps struct {
	RepoSettings   *postgres.RepoSettingsStore
	ReviewVerdicts *postgres.ReviewVerdictStore
	// Artifacts/Sessions (§26.3's own "provenance: Narvi-authored vs
	// human, and the authoring model" signal) back ResolveProvenance
	// (provenance.go) -- the SAME two stores internal/app/outboxworker's
	// own isPlatformAuthored helper and internal/app/decisioninbox's own
	// identical twin already use for "which session, if any, pushed and
	// opened this exact PR". Nil-safe: a nil Artifacts (this package's
	// own tests that don't care about provenance) degrades ResolveProvenance
	// to Provenance{} -- never a panic.
	Artifacts *postgres.ArtifactStore
	Sessions  *postgres.SessionStore
	// SizeExclusions (§26.3) are this deployment's size patterns,
	// platform.Config.ReviewSizeExcludedPaths -- files they match are left
	// out of the size a review is routed on (reviewtriage.Config.
	// SizeExclusions). Nil excludes nothing, the count every changed line
	// had before these patterns existed.
	SizeExclusions []string
}

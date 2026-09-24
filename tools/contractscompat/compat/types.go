// Package compat implements the structural compatibility checker behind
// technical plan §6.3's "make /contracts a stable external API": given a
// BASE and a HEAD copy of the same versioned JSON Schema surfaces (plus
// controlplane/testdata/routes.golden), it classifies every change as
// PATCH, MINOR, or MAJOR under the closed 41-row rule table in
// docs/../COMPATIBILITY.md, and fails closed -- refusing to classify at
// all -- on anything the table does not name.
//
// The package never touches disk or a subprocess: every entry point takes
// already-parsed JSON (as generic `any` trees decoded by encoding/json) or
// already-read file bytes. tools/contractscompat's own main.go is the only
// thing that opens files, so this library stays trivially unit-testable
// with in-memory fixtures (see compat_test.go's corpus loader) and never
// trips tools/lint/narvichecks/execimportban (no os/exec anywhere in this
// tree either, by construction -- there is simply no need for one).
package compat

// Direction classifies which side of a wire produces a schema and which
// side consumes it (§6.3 design spec §2). The compatibility rule that
// applies to a given change depends on direction: a property that becomes
// required is MAJOR for a client-produced (C2P) shape (an old client's
// request now fails validation) but only MINOR for a platform-produced
// (P2C) shape (an old client merely never looks at the new field).
type Direction string

const (
	// DirP2C is platform-produced, client-consumed (REST responses,
	// SubscribedPayload/FetchHistoryResponse, sandbox-ws commands as seen
	// by the agent, session-config, events as seen by the browser).
	DirP2C Direction = "platform-to-client"
	// DirC2P is client-produced, platform-consumed (REST *Request bodies,
	// SubscribeRequest/FetchHistoryRequest, events as sent by the agent).
	DirC2P Direction = "client-to-platform"
	// DirBoth is a def reachable from roots of both kinds (a REST helper
	// type nested under both a *Request and a response/entity), or a
	// whole surface that is inherently bidirectional (sandbox-ws events:
	// agent->CP is C2P, CP->browser is P2C). Compatible only if compatible
	// under BOTH columns of the rule table.
	DirBoth Direction = "both"
)

// Severity is the outcome the rule table assigns to one Finding. Ordered
// least to most severe so max(a, b) (see maxSeverity) picks the worse of
// two classifications -- used to combine a DirBoth def's P2C and C2P
// readings, and to combine the version/changelog CI rule's own "highest
// finding class" requirement (§6.3 design spec §4).
type Severity int

// Severity's own values, from least to most severe.
const (
	SeverityPatch Severity = iota
	SeverityMinor
	SeverityMajor
	// SeverityFailClosed is not a compatibility verdict at all: it means
	// the checker refuses to classify the change because it involves a
	// keyword, a $ref form, or a shape the closed rule table (and its
	// keyword allowlist) does not name. A FailClosed finding always makes
	// the whole run exit non-zero, same as a Major one -- extending the
	// table is a separate PR to tools/contractscompat and its corpus, per
	// the message every such Finding carries.
	SeverityFailClosed
)

func (s Severity) String() string {
	switch s {
	case SeverityPatch:
		return "PATCH"
	case SeverityMinor:
		return "MINOR"
	case SeverityMajor:
		return "MAJOR"
	case SeverityFailClosed:
		return "FAIL-CLOSED"
	default:
		return "UNKNOWN"
	}
}

// maxSeverity returns the more severe of a and b, used to combine a
// DirBoth def's two-column reading and to fold per-finding severities into
// the run's overall "highest finding class" for the version-bump rule.
func maxSeverity(a, b Severity) Severity {
	if b > a {
		return b
	}
	return a
}

// Finding is one classified (or fail-closed-refused) change, keyed to the
// rule table row that produced it wherever one applies. RuleID is the
// table's own row number as a string ("1".."41"), or a "fc-*" id for the
// handful of fail-closed guards that sit outside the numbered table (an
// unknown keyword, a non-local $ref, an unpairable oneOf, a malformed type
// array -- see keywords.go and defdiff.go).
type Finding struct {
	RuleID   string
	Severity Severity
	// Pointer is a JSON Pointer (RFC 6901), rooted at the schema file it
	// was found in, e.g. "#/$defs/Session/properties/title". For a
	// same-name change it points at the HEAD location; for a pure removal
	// (nothing to point at in HEAD) it points at the BASE location.
	Pointer string
	// Surface is the contracts-relative file path the finding belongs to,
	// e.g. "rest/v1/dtos.schema.json", or "" for a routes.golden or
	// manifest/VERSION/CHANGELOG-level finding.
	Surface string
	Message string
}

// Report is the outcome of comparing one base/head pair end to end:
// schemas, manifest, VERSION, CHANGELOG, and routes.golden together.
type Report struct {
	Findings []Finding
}

// WorstSeverity returns the most severe Severity across every finding, or
// SeverityPatch if there are none (a genuinely empty, fully-compatible
// diff).
func (r Report) WorstSeverity() Severity {
	worst := SeverityPatch
	for _, f := range r.Findings {
		worst = maxSeverity(worst, f.Severity)
	}
	return worst
}

// HasBreaking reports whether the report contains any MAJOR or
// FAIL-CLOSED finding -- the condition `make contracts-compat` (and the CI
// job) treat as a hard failure.
func (r Report) HasBreaking() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityMajor || f.Severity == SeverityFailClosed {
			return true
		}
	}
	return false
}

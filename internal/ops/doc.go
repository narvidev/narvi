// Package ops implements §5.3's ("ops: dashboards, alerts, runbooks",
// §5.3) own structural guard: dashboards and alerts are committed to the
// repo as data (deploy/observability/{dashboards,alerts}/*.json), and this
// package is what keeps them honest as the code moves. §10 ("launch
// readiness", §10-P6) extends the SAME mechanism to a second, sibling
// hazard: the per-surface user guide (docs/guides/*.md) it ships is
// committed as data too, and this package is equally what keeps ITS own
// documented commands honest as the code moves — see "The guide-drift
// extension" below.
//
// # Why this exists
//
// §10-P6 already names the hazard a hand-maintained ops artifact runs
// into: a document describing behavior nothing actually enforces just
// drifts out of sync with the code it claims to describe. For a user
// guide that means aspirational prose; for a dashboard or an alert it is
// worse, because the failure is SILENT — an alert naming a metric the
// code no longer emits (renamed, removed, or never shipped) never fires.
// It looks exactly like a healthy system with nothing wrong, forever,
// which is strictly more dangerous than having no alert at all: an
// operator trusts it precisely because it exists.
//
// # The mechanism
//
// ScanRegisteredInstruments (instruments.go) is a go/ast walk over this
// repo's own Go source — mirroring tools/lint/narvichecks/notimeliteral's
// own established "go/ast.Inspect over every non-test .go file" shape —
// collecting the string-literal instrument name from every real OTel
// metric-instrument registration call (meter.Int64Counter("name", ...)
// and its seven siblings). This is the SAME source of truth production
// code itself resolves against at runtime (otel.Meter(...).Int64Counter),
// read mechanically rather than hand-maintained, so it can never itself
// drift from what the code actually registers.
//
// LoadDashboards/LoadAlerts (schema.go) parse the committed JSON files.
// CheckDrift (drift.go) compares every metric name either file references
// against the registered set and reports one error per unregistered
// reference. drift_test.go wires this into `go test ./...` (and therefore
// `make test`, already a required CI step) against the REAL repo tree and
// the REAL deploy/observability directory — no separate CI job needed,
// and no dashboard/alert can ship naming a metric nothing emits.
//
// # The guide-drift extension (§10)
//
// "the two checks are the same idea over different sources": every piece
// above has a direct sibling scanning a DIFFERENT part of this repo's own
// source rather than a second, forked mechanism.
//
//   - ScanRegisteredRoutes (routes.go) is instruments.go's own shape
//     applied to the controlplane package's chi route wiring instead of
//     OTel instrument registration: a go/ast walk collecting every real
//     "METHOD /path" this binary actually serves.
//   - ScanIntentVocabulary (intentvocab.go) is the same shape again,
//     applied to internal/domain/intent's own exported string constants
//     (plus sqlcgen's SessionSpawnSource enum) — every real Surface/
//     Target/Mode/(record-level) Source value §18.4's IntentDecisionRecord
//     can actually carry.
//   - LoadGuides (guide.go) is LoadDashboards/LoadAlerts's own shape
//     applied to docs/guides/*.md instead of deploy/observability/*.json:
//     one file, one concern (one ingress surface), loaded in
//     deterministic order, every structural problem (a missing title, an
//     unterminated code fence, malformed embedded JSON, a command naming
//     neither or both of its two binding kinds) failing the load
//     immediately rather than being silently skipped.
//   - CheckGuideDrift (guidedrift.go) is CheckDrift's own shape: compare
//     every guide's own claims against the two scanners' real output,
//     report one GuideDriftError per unregistered reference.
//
// guidedrift_test.go wires TestNoGuideDrift into the exact same `go test
// ./...` path TestNoMetricDrift already uses — one CI-enforced guard, not
// two, over two structurally identical hazards.
//
// # The omission direction (§10-P6)
//
// Everything above catches a guide LYING about a route. It cannot catch a
// guide staying silent about one the code actually implements — the
// realistic failure, since shipping a capability and updating its guide
// are two separate acts. guideomission.go closes that half:
// RouteGuideExemptions is the explicit, code-reviewed register of routes
// deliberately outside every guide (a webhook receiver, an infrastructure
// endpoint, or sandbox-agent-only bearer-token plumbing), each with a
// reason CheckGuideDrift itself validates (non-empty, past a length floor,
// not one of a short denylist of stock non-answers) before ever letting it
// count. CheckGuideDrift then requires every route ScanRegisteredRoutes
// finds to be either documented in a guide or named in that register —
// and separately fails a register entry that is stale (names a route that
// no longer exists), a wildcard (this package implements no
// pattern-matching lookup anywhere), or a contradiction (a route claimed
// both absent from and present in the guides at once). See docs/guides/
// README.md's own "Two rules, both enforced now" section for the full
// account of what is and is not mechanically enforceable about a reason's
// truth.
package ops

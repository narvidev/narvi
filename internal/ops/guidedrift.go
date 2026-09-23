package ops

import (
	"fmt"
	"sort"
	"strings"
)

// GuideDriftError names one documented command in a per-surface user
// guide (docs/guides/*.md) whose binding — a claimed HTTP route, or a
// claimed §18.4 classifier routing-record value — does not actually exist
// in this repo's own source. Kind distinguishes which half of the binding
// failed, for a precise error message; CheckGuideDrift's own doc comment
// enumerates every value.
type GuideDriftError struct {
	Kind    string
	Source  string
	Command string
	Detail  string
}

func (e GuideDriftError) Error() string {
	return fmt.Sprintf("%s %s: command %q: %s", e.Kind, e.Source, e.Command, e.Detail)
}

// CheckGuideDrift is guidedrift's own CheckDrift (drift.go's own
// dashboards/alerts twin, "the same idea over different sources"):
// compares every SurfaceGuide's own claims against a real
// ScanRegisteredRoutes result and a real ScanIntentVocabulary result, and
// returns one GuideDriftError per unregistered reference, in a stable
// (guides in input order, commands in file order within each guide, then
// exemption-register problems in RouteGuideExemptions's own order, then
// undocumented real routes in sorted "METHOD /path" order) sequence. A
// nil/empty result means every guide file's own claimed surface is real,
// every documented command's route or classifier binding names something
// the code actually implements, every real route is either documented or
// named in exemptions with a real reason, and the register itself contains
// no stale, contradictory, or vacuous entry — the CI-passing state.
//
// Kind values: "guide-surface" (a guide file's own filename-derived
// surface, e.g. docs/guides/web.md -> "web", is not a registered
// sessions.spawn_source value), "route" (a command's Route string matches
// no ScanRegisteredRoutes entry), "classifier-surface"/"classifier-
// target"/"classifier-mode"/"classifier-source" (a command's Classifier
// field names a Surface/Target/Mode/Source value ScanIntentVocabulary
// never found in source), "route-undocumented" (a real, registered route
// is named in no guide's own Route field and in no exemptions entry —
// guideomission.go's own completeness half), and six more over
// exemptions itself (guideomission.go's own doc comment): "exemption-
// malformed-route", "exemption-wildcard", "exemption-empty-reason",
// "exemption-reason-too-short", "exemption-vacuous-reason",
// "exemption-stale", "exemption-contradicts-guide", "exemption-
// duplicate".
//
// What this does NOT catch (validates NAMES, not semantics — the exact
// limitation TestNoMetricDrift's own doc.go already states for the
// dashboards/alerts twin of this check): a route that exists but no
// longer behaves the way the guide's own prose describes, or a
// classifier binding whose Surface/Target/Mode values are each
// individually real but were never actually producible TOGETHER for that
// specific input shape (e.g. nothing stops a guide from claiming
// classifier{surface:"web", target:"review"} even though the web surface
// never calls Classify at all — see docs/guides/README.md's own "what
// this check cannot catch" section for the full list). The exemption
// register closes the OMISSION half of the route direction specifically;
// it says nothing about a classifier value's own omission — see
// guideomission.go's own top doc comment for why that asymmetry is
// deliberate, not an oversight.
func CheckGuideDrift(guides []SurfaceGuide, routes map[string]RegisteredRoute, vocab IntentVocabulary, exemptions []RouteGuideExemption) []GuideDriftError {
	var errs []GuideDriftError
	documentedRoutes := map[string]bool{}

	for _, g := range guides {
		if !vocab.Surfaces[g.Surface] {
			errs = append(errs, GuideDriftError{
				Kind:    "guide-surface",
				Source:  g.SourcePath(),
				Command: g.Title,
				Detail:  fmt.Sprintf("filename-derived surface %q is not a registered sessions.spawn_source value", g.Surface),
			})
		}

		for _, c := range g.Commands {
			if c.Route != "" {
				documentedRoutes[c.Route] = true
				if _, ok := routes[c.Route]; !ok {
					errs = append(errs, GuideDriftError{
						Kind:    "route",
						Source:  g.SourcePath(),
						Command: c.Name,
						Detail:  fmt.Sprintf("references unregistered route %q", c.Route),
					})
				}
			}

			if c.Classifier != nil {
				cl := c.Classifier
				if !vocab.Surfaces[cl.Surface] {
					errs = append(errs, GuideDriftError{
						Kind:    "classifier-surface",
						Source:  g.SourcePath(),
						Command: c.Name,
						Detail:  fmt.Sprintf("references unregistered surface %q", cl.Surface),
					})
				}
				if cl.Target != "" && !vocab.Targets[cl.Target] {
					errs = append(errs, GuideDriftError{
						Kind:    "classifier-target",
						Source:  g.SourcePath(),
						Command: c.Name,
						Detail:  fmt.Sprintf("references unregistered target %q", cl.Target),
					})
				}
				if cl.Mode != "" && !vocab.Modes[cl.Mode] {
					errs = append(errs, GuideDriftError{
						Kind:    "classifier-mode",
						Source:  g.SourcePath(),
						Command: c.Name,
						Detail:  fmt.Sprintf("references unregistered mode %q", cl.Mode),
					})
				}
				if cl.Source != "" && !vocab.Sources[cl.Source] {
					errs = append(errs, GuideDriftError{
						Kind:    "classifier-source",
						Source:  g.SourcePath(),
						Command: c.Name,
						Detail:  fmt.Sprintf("references unregistered record source %q", cl.Source),
					})
				}
			}
		}
	}

	// The other direction: every REAL route is either documented above or
	// named in exemptions with a real reason — guideomission.go's own
	// completeness half. First validate the register itself (a stale,
	// contradictory, malformed, or vacuously-reasoned entry fails exactly
	// like a bad guide command does above), then walk every registered
	// route and require it be in one of the two sets.
	exemptedRoutes := map[string]bool{}
	seenExemptionRoute := map[string]bool{}
	for _, ex := range exemptions {
		route := strings.TrimSpace(ex.Route)
		reason := strings.TrimSpace(ex.Reason)

		if route != "" && seenExemptionRoute[route] {
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-duplicate",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("route %q appears more than once in RouteGuideExemptions", route),
			})
		}
		if route != "" {
			seenExemptionRoute[route] = true
		}

		wellFormed, formatProblem := isWellFormedExemptionRoute(route)
		if !wellFormed {
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-malformed-route",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("exemption route %q is not a valid \"METHOD /path\" string: %s", ex.Route, formatProblem),
			})
		} else if exemptionRouteIsWildcard(route) {
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-wildcard",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("exemption route %q names a pattern, not one real route -- this register exempts exact routes only, never a prefix or wildcard", route),
			})
			wellFormed = false // nothing further to check against a fake string
		}

		// Order matters: an exact denylist match is checked BEFORE the
		// length floor, deliberately, even though every entry in
		// vacuousExemptionReasons is itself short enough to also fail the
		// length check -- reporting "exemption-vacuous-reason" for a bare
		// "N/A" is a more precise, more actionable message than
		// "exemption-reason-too-short" would be, and checking length
		// first would make the vacuous-reason Kind effectively
		// unreachable (every denylisted phrase is short).
		reasonValid := true
		switch {
		case reason == "":
			reasonValid = false
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-empty-reason",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  "exemption has an empty reason -- every exemption must say who or what calls this route instead of a person",
			})
		case vacuousExemptionReasons[normalizeReasonForCheck(reason)]:
			reasonValid = false
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-vacuous-reason",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("exemption reason %q is a stock non-answer, not a claim about who or what calls this route", reason),
			})
		case len(reason) < minExemptionReasonLen:
			reasonValid = false
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-reason-too-short",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("exemption reason %q is only %d characters (minimum %d) -- too short to name who or what calls this route", reason, len(reason), minExemptionReasonLen),
			})
		}

		if !wellFormed {
			continue
		}

		// exemptedRoutes only gains an entry when BOTH halves are valid --
		// a malformed/wildcard route was already `continue`d past above,
		// and an empty/vacuous/too-short reason must not let an otherwise
		// well-formed, real route count as covered either. An exemption
		// entry that fails its own validation exempts nothing: the route
		// it named stays reported as undocumented below, exactly as if no
		// entry had been written for it at all. This is the one line in
		// this file that decides whether a bad register entry can ever
		// accidentally silence the completeness check it exists to
		// enforce -- see this function's own mutation-tested "empty
		// reason" case (guidedrift_test.go) for why it is written this
		// way rather than gating only on `wellFormed`.
		if _, ok := routes[route]; !ok {
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-stale",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("route %q is registered in RouteGuideExemptions but no longer exists in the real route table -- remove the stale entry", route),
			})
		} else if reasonValid {
			exemptedRoutes[route] = true
		}

		if documentedRoutes[route] {
			errs = append(errs, GuideDriftError{
				Kind:    "exemption-contradicts-guide",
				Source:  guideExemptionSourceLabel,
				Command: route,
				Detail:  fmt.Sprintf("route %q is both exempted here AND documented in a guide -- a route cannot be claimed both absent from and present in the guides", route),
			})
		}
	}

	routeKeys := make([]string, 0, len(routes))
	for k := range routes {
		routeKeys = append(routeKeys, k)
	}
	sort.Strings(routeKeys)
	for _, route := range routeKeys {
		if documentedRoutes[route] || exemptedRoutes[route] {
			continue
		}
		reg := routes[route]
		errs = append(errs, GuideDriftError{
			Kind:    "route-undocumented",
			Source:  reg.File,
			Command: route,
			Detail:  fmt.Sprintf("route %q (registered at %s:%d) is documented in no guide and exempted by no RouteGuideExemptions entry", route, reg.File, reg.Line),
		})
	}

	return errs
}

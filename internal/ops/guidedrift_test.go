package ops

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckGuideDrift(t *testing.T) {
	vocab := IntentVocabulary{
		Surfaces: map[string]bool{"web": true, "github": true},
		Targets:  map[string]bool{"review": true, "request": true},
		Modes:    map[string]bool{"plan": true, "build": true},
		Sources:  map[string]bool{"classifier": true, "explicit": true, "fallback": true},
	}
	routes := map[string]RegisteredRoute{
		"POST /api/sessions": {Method: "POST", Path: "/api/sessions"},
	}

	// noExemptions covers every route in `routes` above, so subtests that
	// don't care about the omission direction see a clean completeness
	// pass and can assert on the OTHER kind of error alone.
	noExemptions := []RouteGuideExemption{
		{Route: "POST /api/sessions", Reason: "covered by the guide command under test in each subtest below, not by this register"},
	}

	t.Run("no drift", func(t *testing.T) {
		guides := []SurfaceGuide{{
			Surface: "web",
			Title:   "Web Guide",
			Commands: []GuideCommand{
				{Name: "Create a session", Route: "POST /api/sessions"},
				{Name: "PR mention", Classifier: &ClassifierRef{Surface: "github", Target: "review", Mode: "build", Source: "classifier"}},
			},
		}}
		if errs := CheckGuideDrift(guides, routes, vocab, nil); len(errs) != 0 {
			t.Errorf("CheckGuideDrift() = %v, want none", errs)
		}
	})

	t.Run("command references unregistered route", func(t *testing.T) {
		guides := []SurfaceGuide{{Surface: "web", Title: "Web Guide", Commands: []GuideCommand{
			{Name: "Ghost route", Route: "POST /api/does-not-exist"},
		}}}
		errs := CheckGuideDrift(guides, routes, vocab, noExemptions)
		if len(errs) != 1 {
			t.Fatalf("CheckGuideDrift() = %v, want exactly 1 error", errs)
		}
		if errs[0].Kind != "route" || errs[0].Command != "Ghost route" {
			t.Errorf("CheckGuideDrift() = %+v, want Kind=route Command=\"Ghost route\"", errs[0])
		}
	})

	t.Run("guide's own filename-derived surface is unregistered", func(t *testing.T) {
		guides := []SurfaceGuide{{Surface: "carrier-pigeon", Title: "Pigeon Guide", Commands: []GuideCommand{
			{Name: "Create a session", Route: "POST /api/sessions"},
		}}}
		// No exemptions needed: this guide DOES document "POST
		// /api/sessions" (the only real route in `routes` above), so the
		// completeness check is already satisfied — passing noExemptions
		// here would make the route BOTH documented and exempted, which
		// is exactly the contradiction the omission check exists to catch
		// and would fail this subtest for an unrelated reason.
		errs := CheckGuideDrift(guides, routes, vocab, nil)
		if len(errs) != 1 || errs[0].Kind != "guide-surface" {
			t.Fatalf("CheckGuideDrift() = %v, want exactly 1 guide-surface error", errs)
		}
	})

	t.Run("classifier references unregistered surface/target/mode/source independently", func(t *testing.T) {
		guides := []SurfaceGuide{{Surface: "web", Title: "Web Guide", Commands: []GuideCommand{
			{Name: "Bad surface", Classifier: &ClassifierRef{Surface: "carrier-pigeon"}},
			{Name: "Bad target", Classifier: &ClassifierRef{Surface: "github", Target: "teleport"}},
			{Name: "Bad mode", Classifier: &ClassifierRef{Surface: "github", Mode: "levitate"}},
			{Name: "Bad source", Classifier: &ClassifierRef{Surface: "github", Source: "telepathy"}},
		}}}
		errs := CheckGuideDrift(guides, routes, vocab, noExemptions)
		if len(errs) != 4 {
			t.Fatalf("CheckGuideDrift() = %v, want exactly 4 errors", errs)
		}
		kinds := map[string]bool{}
		for _, e := range errs {
			kinds[e.Kind] = true
		}
		for _, want := range []string{"classifier-surface", "classifier-target", "classifier-mode", "classifier-source"} {
			if !kinds[want] {
				t.Errorf("missing error kind %q in %v", want, errs)
			}
		}
	})

	t.Run("renamed route is caught the same as a never-registered one", func(t *testing.T) {
		renamedRoutes := map[string]RegisteredRoute{
			"POST /api/sessions/v2": {Method: "POST", Path: "/api/sessions/v2"},
		}
		guides := []SurfaceGuide{{Surface: "web", Title: "Web Guide", Commands: []GuideCommand{
			{Name: "Create a session", Route: "POST /api/sessions"},
		}}}
		errs := CheckGuideDrift(guides, renamedRoutes, vocab, nil)
		// One "route" error for the guide's own dead reference, AND one
		// "route-undocumented" error for the renamed route itself (real,
		// but named in no guide and no exemption) -- the rename broke BOTH
		// directions of the check at once, and both must be reported, not
		// just the one this test used to assert alone.
		if len(errs) != 2 {
			t.Fatalf("CheckGuideDrift() = %v, want exactly 2 errors", errs)
		}
		kinds := map[string]bool{}
		for _, e := range errs {
			kinds[e.Kind] = true
		}
		for _, want := range []string{"route", "route-undocumented"} {
			if !kinds[want] {
				t.Errorf("missing error kind %q in %v", want, errs)
			}
		}
	})
}

// TestCheckGuideDrift_Omission exercises the direction §10-P6's own
// exit criterion names explicitly: a real route with no guide entry and no
// exemption entry fails, and every way the exemption register itself can
// become the "dumping ground" docs/guides/README.md warns against is
// caught too.
func TestCheckGuideDrift_Omission(t *testing.T) {
	vocab := IntentVocabulary{Surfaces: map[string]bool{"web": true}}
	guides := []SurfaceGuide{{Surface: "web", Title: "Web Guide", Commands: []GuideCommand{
		{Name: "Get session", Route: "GET /api/sessions/{sessionID}"},
	}}}
	routes := map[string]RegisteredRoute{
		"GET /api/sessions/{sessionID}": {Method: "GET", Path: "/api/sessions/{sessionID}", File: "controlplane/serve.go", Line: 42},
		"POST /webhooks/probe":          {Method: "POST", Path: "/webhooks/probe", File: "controlplane/serve.go", Line: 99},
	}
	const realReason = "Inbound webhook receiver an external probing system POSTs to, never a human's browser."

	t.Run("a real route with no guide entry and no exemption fails", func(t *testing.T) {
		errs := CheckGuideDrift(guides, routes, vocab, nil)
		if len(errs) != 1 || errs[0].Kind != "route-undocumented" || errs[0].Command != "POST /webhooks/probe" {
			t.Fatalf("CheckGuideDrift() = %v, want exactly 1 route-undocumented error for \"POST /webhooks/probe\"", errs)
		}
	})

	t.Run("an exemption with a real reason closes the gap", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: realReason}}
		if errs := CheckGuideDrift(guides, routes, vocab, exemptions); len(errs) != 0 {
			t.Errorf("CheckGuideDrift() = %v, want none", errs)
		}
	})

	t.Run("empty reason fails", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: ""}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		kinds := errKinds(errs)
		if !kinds["exemption-empty-reason"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-empty-reason", errs)
		}
		// The route is still, correctly, undocumented -- an invalid
		// exemption entry must never itself count as covering the route.
		if !kinds["route-undocumented"] {
			t.Errorf("CheckGuideDrift() = %v, want route-undocumented too (an invalid exemption exempts nothing)", errs)
		}
	})

	t.Run("too-short reason fails", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: "internal thing"}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if !errKinds(errs)["exemption-reason-too-short"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-reason-too-short", errs)
		}
	})

	t.Run("a punctuation-joined concatenation of denylisted phrases is now caught clause by clause", func(t *testing.T) {
		// reasonClausesAllVacuous (guideomission.go) closes the gap this
		// subtest used to pin as accepted: splitting the reason on
		// commas/semicolons/periods/hyphens and requiring every resulting
		// clause, not just the whole string, to individually clear
		// vacuousExemptionReasons catches a concatenation like this one
		// even though the WHOLE string is neither an exact denylist entry
		// nor short enough to fail the length floor.
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: "internal only, not applicable, admin only"}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		kinds := errKinds(errs)
		if !kinds["exemption-vacuous-reason"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-vacuous-reason (every comma-separated clause is itself a stock non-answer)", errs)
		}
		if !kinds["route-undocumented"] {
			t.Errorf("CheckGuideDrift() = %v, want route-undocumented too (an invalid exemption exempts nothing)", errs)
		}
	})

	t.Run("hyphen-padding a single denylisted phrase past the length floor is now caught the same way", func(t *testing.T) {
		// The minExemptionReasonLen doc comment's own example: repeating
		// "internal" with bare hyphens between repetitions clears the
		// 30-character floor and was never a single exact
		// vacuousExemptionReasons entry as a whole string either -- each
		// hyphen-delimited clause is "internal" on its own, so this is
		// caught the identical way the comma-joined case above is.
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: "internal-internal-internal-internal"}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if !errKinds(errs)["exemption-vacuous-reason"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-vacuous-reason (hyphen-padding a single stock phrase)", errs)
		}
	})

	t.Run("known limitation: concatenating denylisted phrases with NO punctuation between them still clears both mechanical checks", func(t *testing.T) {
		// This is NOT a bug this test is pinning as correct behavior; it
		// is the honest, NARROWER boundary that remains after
		// reasonClausesAllVacuous (guideomission.go) closed the
		// punctuation-joined case above: with no comma, hyphen, or other
		// clauseSplitRE delimiter anywhere in the string, it yields
		// exactly ONE clause -- the whole string -- which is neither a
		// single exact vacuousExemptionReasons entry (it is four of them
		// run together) nor short enough to fail the length floor. Stated
		// as a passing test rather than left to be discovered later by
		// someone assuming the denylist is smarter than it is (docs/
		// guides/README.md's own "exhaustiveness claims" section makes
		// the identical call for the route/classifier scanners: a check
		// that validates a narrow, syntactic property stays honest by
		// refusing to pretend it does semantic content review). A reason
		// this hollow is exactly what "review the register as code" (this
		// Step's own brief) exists to catch instead.
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: "internal only not applicable admin only"}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if len(errs) != 0 {
			t.Errorf("CheckGuideDrift() = %v, want none (documenting the known, narrower gap, not asserting it is caught)", errs)
		}
	})

	t.Run("exact stock-phrase reason is refused by the denylist", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/probe", Reason: strings.Repeat(" ", 10) + "Not User-Facing.   " + strings.Repeat(" ", 10)}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if !errKinds(errs)["exemption-vacuous-reason"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-vacuous-reason (case/whitespace/punctuation must not evade the denylist)", errs)
		}
	})

	t.Run("wildcard entry is refused, not silently accepted as a prefix exemption", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/*", Reason: realReason}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		kinds := errKinds(errs)
		if !kinds["exemption-wildcard"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-wildcard", errs)
		}
		if !kinds["route-undocumented"] {
			t.Errorf("CheckGuideDrift() = %v, want route-undocumented too -- a wildcard must not exempt the real route it was aimed at", errs)
		}
	})

	t.Run("stale entry (route no longer registered) fails", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "POST /webhooks/does-not-exist-anymore", Reason: realReason}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if !errKinds(errs)["exemption-stale"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-stale", errs)
		}
	})

	t.Run("an entry also documented in a guide contradicts itself", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "GET /api/sessions/{sessionID}", Reason: realReason}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if !errKinds(errs)["exemption-contradicts-guide"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-contradicts-guide", errs)
		}
	})

	t.Run("duplicate route in the register fails", func(t *testing.T) {
		exemptions := []RouteGuideExemption{
			{Route: "POST /webhooks/probe", Reason: realReason},
			{Route: "POST /webhooks/probe", Reason: realReason + " (again)"},
		}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		if !errKinds(errs)["exemption-duplicate"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-duplicate", errs)
		}
	})

	t.Run("malformed exemption route (no method) fails and exempts nothing", func(t *testing.T) {
		exemptions := []RouteGuideExemption{{Route: "/webhooks/probe", Reason: realReason}}
		errs := CheckGuideDrift(guides, routes, vocab, exemptions)
		kinds := errKinds(errs)
		if !kinds["exemption-malformed-route"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-malformed-route", errs)
		}
		if !kinds["route-undocumented"] {
			t.Errorf("CheckGuideDrift() = %v, want route-undocumented too", errs)
		}
	})

	t.Run("empty register on a completely undocumented directory: every route surfaces once, nothing panics", func(t *testing.T) {
		errs := CheckGuideDrift(nil, routes, vocab, nil)
		if len(errs) != len(routes) {
			t.Fatalf("CheckGuideDrift() = %v, want exactly %d route-undocumented errors (one per real route, zero guides)", errs, len(routes))
		}
		for _, e := range errs {
			if e.Kind != "route-undocumented" {
				t.Errorf("unexpected kind %q in %v", e.Kind, errs)
			}
		}
	})

	t.Run("zero registered routes and an empty register: no completeness error", func(t *testing.T) {
		if errs := CheckGuideDrift(nil, map[string]RegisteredRoute{}, vocab, nil); len(errs) != 0 {
			t.Errorf("CheckGuideDrift() = %v, want none (nothing registered, nothing to omit)", errs)
		}
	})
}

// TestCheckGuideDrift_RouteNormalization pins the exact string-equality
// rule the omission check (and the exemption register) uses to match a
// documented/exempted route against a real one — deliberately no
// normalization beyond what ScanRegisteredRoutes's own joinRoutePath
// already bakes into the route strings it produces (routes.go's own doc
// comment: no trailing slash, ever). Both directions matter equally: two
// spellings of the SAME route must be recognized as the same route
// (trivial here — there is only one legal spelling), and two DIFFERENT
// routes must never collapse into one merely because they look similar
// (a shared prefix, or path parameters with different names in the same
// position).
func TestCheckGuideDrift_RouteNormalization(t *testing.T) {
	vocab := IntentVocabulary{Surfaces: map[string]bool{"web": true}}
	routes := map[string]RegisteredRoute{
		"GET /api/sessions":             {Method: "GET", Path: "/api/sessions"},
		"GET /api/sessions/{sessionID}": {Method: "GET", Path: "/api/sessions/{sessionID}"},
	}

	t.Run("a trailing slash does not match the real, slash-free route", func(t *testing.T) {
		guides := []SurfaceGuide{{Surface: "web", Title: "Web Guide", Commands: []GuideCommand{
			{Name: "List sessions", Route: "GET /api/sessions/"},
		}}}
		errs := CheckGuideDrift(guides, routes, vocab, nil)
		kinds := errKinds(errs)
		// The guide's own claim is unregistered (no route ends in "/" in
		// this scanner's own output)...
		if !kinds["route"] {
			t.Errorf("CheckGuideDrift() = %v, want a \"route\" error for the trailing-slash spelling", errs)
		}
		// ...AND the real route it was presumably trying to document is
		// STILL reported as undocumented -- a trailing slash must not be
		// silently treated as "close enough" in either direction.
		if !kinds["route-undocumented"] {
			t.Errorf("CheckGuideDrift() = %v, want route-undocumented for the real \"GET /api/sessions\" too", errs)
		}
	})

	t.Run("a different path-parameter name is a different route, not a fuzzy match", func(t *testing.T) {
		// Exempting "{id}" must not be read as covering the REAL route,
		// which names its own parameter "{sessionID}" -- proving this
		// check does no parameter-name normalization at all: two
		// genuinely different literal strings never collapse into one,
		// even when a human would read them as "the same shape".
		exemptions := []RouteGuideExemption{{Route: "GET /api/sessions/{id}", Reason: "Deliberately wrong param name, proving no fuzzy param-name matching happens here at all."}}
		errs := CheckGuideDrift(nil, routes, vocab, exemptions)
		kinds := errKinds(errs)
		if !kinds["exemption-stale"] {
			t.Errorf("CheckGuideDrift() = %v, want exemption-stale (\"{id}\" names no real route)", errs)
		}
		if !kinds["route-undocumented"] {
			t.Errorf("CheckGuideDrift() = %v, want route-undocumented for the real \"{sessionID}\" route, still uncovered", errs)
		}
	})

	t.Run("two routes sharing a literal prefix stay distinct", func(t *testing.T) {
		// "GET /api/sessions" must not be treated as documenting/exempting
		// "GET /api/sessions/{sessionID}" (or vice versa) merely because
		// one is a prefix of the other -- this is the same "no prefix
		// match" property the register's own wildcard refusal exists to
		// guarantee, checked here from the completeness side instead.
		guides := []SurfaceGuide{{Surface: "web", Title: "Web Guide", Commands: []GuideCommand{
			{Name: "List sessions", Route: "GET /api/sessions"},
		}}}
		errs := CheckGuideDrift(guides, routes, vocab, nil)
		if len(errs) != 1 || errs[0].Kind != "route-undocumented" || errs[0].Command != "GET /api/sessions/{sessionID}" {
			t.Fatalf("CheckGuideDrift() = %v, want exactly 1 route-undocumented error for \"GET /api/sessions/{sessionID}\" (the sibling with a param must stay separately required)", errs)
		}
	})
}

// errKinds is a small test helper: the set of Kind values present in errs,
// used where a subtest wants to assert one specific kind is/isn't present
// without also pinning the exact total count (several of these mutations
// legitimately produce a second, unrelated error alongside the one under
// test -- e.g. an invalid exemption entry also leaves its route
// undocumented).
func errKinds(errs []GuideDriftError) map[string]bool {
	kinds := make(map[string]bool, len(errs))
	for _, e := range errs {
		kinds[e.Kind] = true
	}
	return kinds
}

// TestNoGuideDrift is §10's own CI-enforcing structural guard,
// TestNoMetricDrift's own direct sibling (drift_test.go): scans this
// repo's REAL controlplane route wiring and REAL internal/domain/
// intent + sqlcgen vocabulary, loads the REAL docs/guides/*.md files, reads
// the REAL RouteGuideExemptions register (guideomission.go), and fails if
// any documented command's route or classifier binding names something the
// code does not actually implement, OR if any real route is documented in
// no guide and exempted by no register entry, OR if the register itself is
// stale, contradictory, or vacuously reasoned. Runs as a plain `go test`,
// already part of `make test`/`go test -race ./...` — no separate CI job
// or workflow edit needed.
//
// Mutation-tested by hand (see the PR description for the exact failure
// message each produced): (1) documenting a command with a route that maps
// to no real endpoint makes this test fail; (2) renaming a real route in
// the controlplane package without updating the guide makes this test
// fail identically; (3) malforming a guide file (breaking its embedded
// JSON) makes LoadGuides itself fail the test, rather than silently
// skipping that file; (4) adding a new, real, user-facing route with
// neither a guide entry nor a RouteGuideExemptions entry makes this test
// fail with "route-undocumented"; (5) an empty Reason on a
// RouteGuideExemptions entry fails with "exemption-empty-reason"; (6) a
// wildcard Route ("POST /api/foo/*") fails with "exemption-wildcard" AND
// leaves the real route it was aimed at reported undocumented; (7) a
// RouteGuideExemptions entry naming a route that no longer exists fails
// with "exemption-stale"; (8) a RouteGuideExemptions entry naming a route
// ALSO documented in a guide fails with "exemption-contradicts-guide".
// Every mutation was reverted byte-identical afterward.
func TestNoGuideDrift(t *testing.T) {
	root := repoRoot(t)

	routes, err := ScanRegisteredRoutes(filepath.Join(root, "controlplane"))
	if err != nil {
		t.Fatalf("ScanRegisteredRoutes: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("ScanRegisteredRoutes found zero routes -- almost certainly a scan-path bug (this repo has real chi routes), not a genuinely empty binary")
	}

	vocab, err := ScanIntentVocabulary(
		filepath.Join(root, "internal", "domain", "intent"),
		filepath.Join(root, "internal", "adapters", "outbound", "postgres", "sqlcgen"),
	)
	if err != nil {
		t.Fatalf("ScanIntentVocabulary: %v", err)
	}
	if len(vocab.Surfaces) == 0 || len(vocab.Targets) == 0 || len(vocab.Modes) == 0 || len(vocab.Sources) == 0 {
		t.Fatalf("ScanIntentVocabulary found an empty bucket -- almost certainly a scan-path bug: %+v", vocab)
	}

	guides, err := LoadGuides(filepath.Join(root, "docs", "guides"))
	if err != nil {
		t.Fatalf("LoadGuides: %v", err)
	}
	if len(guides) == 0 {
		t.Fatal("LoadGuides found zero per-surface guides")
	}

	// RouteGuideExemptions (guideomission.go) is read here BY VALUE, the
	// real package-level register — not a copy or a re-derived summary of
	// it, so this test and a reviewer reading guideomission.go are always
	// looking at the exact same source.
	if errs := CheckGuideDrift(guides, routes, vocab, RouteGuideExemptions); len(errs) > 0 {
		for _, e := range errs {
			t.Error(e)
		}
	}
}

// TestGuidesCoverAllFourSurfaces is a cheap companion check, same
// rationale as TestAlertRunbooksExist (drift_test.go): §10-P6 names the
// guide as covering "web/Slack/Linear/GitHub" explicitly — a missing
// surface is a smaller version of the same "looks complete but isn't"
// hazard.
func TestGuidesCoverAllFourSurfaces(t *testing.T) {
	root := repoRoot(t)
	guides, err := LoadGuides(filepath.Join(root, "docs", "guides"))
	if err != nil {
		t.Fatalf("LoadGuides: %v", err)
	}
	got := map[string]bool{}
	for _, g := range guides {
		got[g.Surface] = true
	}
	for _, want := range []string{"web", "slack", "linear", "github"} {
		if !got[want] {
			t.Errorf("no guide found for surface %q", want)
		}
	}
}

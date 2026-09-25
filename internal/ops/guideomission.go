package ops

import (
	"regexp"
	"strings"
)

// This file is the other direction CheckGuideDrift (guidedrift.go) checks:
// not "does a documented command bind to something real" (that direction
// already fails a guide that lies), but "does every real, registered route
// appear SOMEWHERE" -- either documented in a guide, or named here with a
// reason. See docs/guides/README.md's own "Two rules, both enforced now"
// section for why this half was missing, and its "Which lot owes which
// guide" table for the routes this batch resolved by adding guide entries
// instead of a register entry.
//
// Nothing in cmd/control-plane's own router wiring records which routes are
// meant for a person and which are not -- a webhook receiver, a health
// endpoint, and a route only the sandbox-agent's own bearer-token client
// ever calls are all real, working chi routes indistinguishable from a
// browser-facing one by shape alone. RouteGuideExemptions is the explicit
// claim that closes that gap: every entry names one route CheckGuideDrift
// must accept as deliberately outside every guide, and WHY.

// guideExemptionSourceLabel is the GuideDriftError.Source value every
// register-shaped error (guidedrift.go's own exemption-* Kinds) carries —
// pointing a reader at the register itself rather than a guide file, since
// that is where the problem actually lives.
const guideExemptionSourceLabel = "internal/ops/guideomission.go (RouteGuideExemptions)"

// RouteGuideExemption is one such claim. Route must be the exact "METHOD
// /path" string ScanRegisteredRoutes would report for it (routes.go's own
// joinRoutePath format -- no trailing slash, the real chi param names
// verbatim, e.g. "{sessionID}" not "{id}"): CheckGuideDrift matches it by
// plain string equality against the real, live-scanned route table, the
// SAME lookup guide.go's own Route-to-route check already uses, never a
// prefix or pattern match (see validateRouteGuideExemptions's own doc
// comment for why that is refused mechanically, not just by convention).
// Reason is a human's claim about who or what calls the route instead of a
// person's browser -- validateRouteGuideExemptions enforces what CAN be
// checked mechanically (non-empty, long enough to carry a clause, not one
// of a short list of stock non-answers); it cannot verify the reason is
// TRUE, only that it is not obviously empty of content. A wrong-but-
// substantive reason is a code-review problem, same as a wrong route string
// in a guide's own narvi-command block (docs/guides/README.md's own "prose
// is not machine-verified" section draws the identical line).
type RouteGuideExemption struct {
	Route  string
	Reason string
}

// RouteGuideExemptions is the register itself -- the one place a reviewer
// reads every route this repo ships with no per-surface guide entry, and
// CheckGuideDrift's own completeness check (guidedrift.go) reads this EXACT
// slice, never a copy of it. Three shapes recur, matching docs/guides/
// README.md's own longstanding "categories" list (itself unchecked prose
// until this file made it real):
//
//  1. A webhook RECEIVER: an external system POSTs to Narvi, not the other
//     way around -- no human ever "calls" it from this app's own UI.
//  2. Sandbox-agent-to-control-plane, bearer-token authenticated: the SAME
//     shape as the OS-level UID isolation this design already relies on
//     elsewhere (§30) -- the caller is the in-sandbox agent runtime itself,
//     authenticated by a per-session bearer token, and no narvi_auth_session
//     cookie (§13.1) ever reaches these paths. Several of these are the
//     machine-calling twin of an ordinary browser route already documented
//     in web.md (e.g. the sandbox-bearer POST .../uploads next to the
//     cookie-authenticated POST /api/sessions/{sessionID}/uploads) --
//     naming that sibling in the reason is exactly the kind of concrete,
//     checkable-by-a-human claim this register exists to hold.
//  3. Infrastructure/federation: a liveness probe or an OIDC discovery
//     document, polled by an orchestrator or an external relying party's
//     own client library, representing no session-spawning actor at all.
var RouteGuideExemptions = []RouteGuideExemption{
	{
		Route:  "POST /webhooks/automations/{automationID}",
		Reason: "Per-automation inbound webhook receiver (internal/adapters/inbound/automationwebhook): the admin-configured URL an EXTERNAL system (GitHub/Linear/CI, whichever the automation's own trigger names) POSTs to. A human never opens this URL from the app; the automation's own Settings screen (POST/GET /api/automations...) is what web.md documents instead.",
	},
	{
		Route:  "GET /health",
		Reason: "Liveness/readiness probe polled by this deployment's own container orchestrator (Kubernetes/Modal health check), never opened by a signed-in user's browser.",
	},
	{
		Route:  "GET /.well-known/openid-configuration",
		Reason: "Cloud-identity OIDC federation discovery document (httpapi/oidcdiscovery.go's OIDCDiscovery): fetched by an external OIDC relying party's own client library resolving this issuer, never by a human browsing the app.",
	},
	{
		Route:  "GET /.well-known/jwks.json",
		Reason: "Cloud-identity OIDC signing-key set (httpapi/oidcdiscovery.go's OIDCJWKS): fetched by the same external OIDC relying-party client library as the openid-configuration document above, never by a human.",
	},
	{
		Route:  "POST /sessions/{sessionID}/scm-credentials",
		Reason: "Sandbox-agent bearer route: the in-sandbox git credential helper fetches its SCM token here at boot (internal/sandboxagent/credentials.CPClient is the caller; httpapi/scmcredentials.go is the handler), never a browser.",
	},
	{
		Route:  "POST /sessions/{sessionID}/provider-credentials",
		Reason: "Sandbox-agent bearer route: the in-sandbox OpenCode runtime fetches its model-provider credentials here at boot (httpapi/providercredentialsdelivery.go), never a browser -- the human-facing CRUD over the same credentials is documented in web.md's own \"Administration & configuration\" section.",
	},
	{
		Route:  "POST /sessions/{sessionID}/sandbox-secrets",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent fetches its configured secret environment variables here at boot (httpapi/sandboxsecretsdelivery.go), never a browser -- the human-facing CRUD over the same secrets is documented in web.md.",
	},
	{
		Route:  "POST /sessions/{sessionID}/opencode-config",
		Reason: "Sandbox-agent bearer route: the in-sandbox OpenCode runtime fetches its resolved config here at boot (httpapi/opencodeconfigdelivery.go), never a browser -- the human-facing CRUD over the same config is documented in web.md.",
	},
	{
		Route:  "POST /sessions/{sessionID}/automation-env-vars",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent fetches this automation's own configured env vars here at boot (httpapi/automationenvvarsdelivery.go), never a browser -- the human-facing CRUD over the same env vars is documented in web.md's own Automations section.",
	},
	{
		Route:  "POST /sessions/{sessionID}/cloud-identity-token",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent mints a short-lived cloud-identity OIDC token here (httpapi/cloudidentitytoken.go), never a browser.",
	},
	{
		Route:  "POST /sessions/{sessionID}/cloud-identity-config",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent reads which cloud-identity bindings apply to it at boot here (httpapi/cloudidentityconfigdelivery.go), never a browser.",
	},
	{
		Route:  "POST /sessions/{sessionID}/snapshot",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent requests its own snapshot mint here (httpapi/snapshotmint.go), never a browser.",
	},
	{
		Route:  "POST /sessions/{sessionID}/review/verdict",
		Reason: "Sandbox-agent bearer route: an in-sandbox review turn posts its computed verdict here (httpapi/reviewverdict.go), never a browser -- the human-facing read of the same verdict is GET /api/sessions/{sessionID}/review, documented in web.md.",
	},
	{
		Route:  "POST /sessions/{sessionID}/workflow/step-outcome",
		Reason: "Sandbox-agent bearer route: the handler side of the generic step-outcome-posting tool (httpapi/workflowstepoutcome.go), never a browser -- but no caller is wired today. Unlike its siblings review/verdict and turn/epistemic-outcome, each backed by a real cmd/sandbox-agent tool prompt (reviewverdicttoolprompt.go, epistemicoutcometoolprompt.go), cmd/sandbox-agent has no step-outcome tool prompt of its own; nothing in this repo currently posts to this route (confirmed by repo-wide search, not merely absence of a match). OnTurnCompleted (internal/app/workflowengine/completion.go) derives an implicit outcome from a turn's own terminal trigger whenever none was explicitly posted, so the workflow engine does not depend on a caller existing.",
	},
	{
		Route:  "POST /sessions/{sessionID}/turn/epistemic-outcome",
		Reason: "Sandbox-agent bearer route: the in-sandbox devil's-advocate preamble posts its required structured signal here (httpapi/epistemicoutcome.go), never a browser.",
	},
	{
		Route:  "POST /sessions/{sessionID}/release-manifest/composition-findings",
		Reason: "Sandbox-agent bearer route: an in-sandbox release-review turn posts composition findings here (httpapi/releasecompositionfindings.go), never a browser -- the human-facing read/decide surface is GET/POST /api/sessions/{sessionID}/release-manifest..., documented in web.md.",
	},
	{
		Route:  "POST /sessions/{sessionID}/uploads",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent mints an agent-produced upload here when it follows the upload-tool note that cmd/sandbox-agent's renderUploadToolPromptText adds to its prompt (httpapi/uploadmint.go's MintUpload), never a browser -- distinct from its cookie-authenticated browser twin POST /api/sessions/{sessionID}/uploads, already documented in web.md.",
	},
	{
		Route:  "POST /sessions/{sessionID}/uploads/{uploadID}/complete",
		Reason: "Sandbox-agent bearer route: the in-sandbox agent confirms its own upload here (httpapi/uploadconfirm.go's ConfirmUpload), never a browser -- distinct from its cookie-authenticated browser twin already documented in web.md.",
	},
	{
		Route:  "GET /sessions/{sessionID}/uploads/{uploadID}/content",
		Reason: "Sandbox-agent bearer route: the in-sandbox download_file tool reads back previously uploaded content here (httpapi/uploadcontent.go's UploadContent), never a browser -- distinct from its cookie-authenticated browser twin GET /api/sessions/{sessionID}/uploads/{uploadID}/content, already documented in web.md.",
	},
	{
		Route:  "POST /mcp",
		Reason: "MCP protocol endpoint (internal/adapters/inbound/mcp, technical plan §43): a JSON-RPC Streamable HTTP surface whose commands are MCP tools, not routes, called by an MCP client program with a bearer token from this deployment's own OAuth authorization server (§43.13) -- never by a page of this web app, and never with the session cookie, which it refuses. How a person connects such a client is documented in web.md's own \"Connected apps (MCP clients)\" section (GET /oauth/authorize and the consent page). It cannot be a per-surface guide file until mcp is a sessions.spawn_source value (TestNoGuideDrift's own guide-surface rule).",
	},
	{
		Route:  "GET /.well-known/oauth-protected-resource/mcp",
		Reason: "RFC 9728 protected-resource metadata for POST /mcp (internal/adapters/inbound/mcpauth's ProtectedResourceMetadata): fetched by an MCP client's own OAuth library after a 401 from /mcp, to learn which authorization server issues tokens for it -- never opened by a person browsing the app.",
	},
	{
		Route:  "GET /.well-known/oauth-authorization-server/oauth",
		Reason: "RFC 8414 authorization-server metadata for the MCP issuer (internal/adapters/inbound/mcpauth's AuthorizationServerMetadata): fetched by an MCP client's own OAuth library to find the authorization and token endpoints -- never opened by a person browsing the app.",
	},
	{
		Route:  "POST /oauth/token",
		Reason: "OAuth token endpoint for MCP clients (internal/adapters/inbound/mcpauth's Token): the MCP client program itself exchanges an authorization code for an access token here after the person approved it on the consent page (documented in web.md), and later trades its refresh token for a new one -- no page of this web app posts to it, and it reads no cookie.",
	},
	{
		Route:  "POST /oauth/revoke",
		Reason: "OAuth token revocation endpoint (RFC 7009) for MCP clients (internal/adapters/inbound/mcpauth's Revoke): an MCP client program gives back one of its own tokens here, for instance when the person signs out of it -- no page of this web app posts to it and it reads no cookie; a person disconnects an app from Settings instead (DELETE /api/me/mcp-authorizations/{authorizationID}, documented in web.md).",
	},
}

// minExemptionReasonLen is the length floor validateRouteGuideExemptions
// enforces on RouteGuideExemption.Reason, AFTER trimming surrounding
// whitespace. A one- or two-word non-answer ("internal", "n/a", "admin
// only", "not user-facing") padded past this floor with punctuation alone
// -- repeating itself with commas or hyphens between the repetitions, say
// -- does not clear vacuousExemptionReasons either, because
// reasonClausesAllVacuous below checks each punctuation-delimited clause
// on its own, not just the reason as one whole string; see that
// function's own doc comment for the real, narrower gap that remains
// (concatenation with NO punctuation at all between the stock phrases).
// Staying short enough that a genuine one-clause reason naming a real
// caller never has to be padded to satisfy a length rule that has nothing
// to do with its content is the other half of why 30 specifically.
const minExemptionReasonLen = 30

// vacuousExemptionReasons is the short denylist of stock non-answers this
// package can name outright -- a reason that, after normalizeReasonForCheck
// below, equals one of these exactly is a dumping-ground label wearing the
// shape of a reason, not a claim about who calls the route. This list
// cannot be exhaustive (a determined author can always invent a new empty
// phrase; docs/guides/README.md's own "exhaustiveness claims" section is
// the reason this package refuses to pretend a check like this ever could
// be), so minExemptionReasonLen and human review both still apply
// regardless of whether a given reason happens to appear here.
var vacuousExemptionReasons = map[string]bool{
	"internal":            true,
	"internal only":       true,
	"internal use":        true,
	"internal use only":   true,
	"not user-facing":     true,
	"not user facing":     true,
	"not for users":       true,
	"n/a":                 true,
	"na":                  true,
	"not applicable":      true,
	"admin only":          true,
	"machine only":        true,
	"machine to machine":  true,
	"system only":         true,
	"backend only":        true,
	"todo":                true,
	"tbd":                 true,
	"misc":                true,
	"other":               true,
	"see code":            true,
	"self-explanatory":    true,
	"not applicable here": true,
}

// normalizeReasonForCheck lowercases, trims, collapses internal whitespace
// runs to one space, and strips one trailing ./!/? -- so "Not user-facing.",
// "not   user-facing", and "NOT USER-FACING" all collapse onto the same
// vacuousExemptionReasons entry rather than each needing its own listing.
func normalizeReasonForCheck(reason string) string {
	s := strings.ToLower(strings.TrimSpace(reason))
	s = strings.TrimRight(s, ".!?")
	s = strings.TrimSpace(s)
	return strings.Join(strings.Fields(s), " ")
}

// clauseSplitRE divides a reason into candidate "clauses" on the
// punctuation a concatenated-denylisted-phrase attack realistically
// glues several stock non-answers together with: commas, semicolons,
// colons, periods, exclamation/question marks, parentheses, and hyphens
// -- including a bare hyphen used purely as padding (e.g.
// "internal-internal-internal-internal", the shape minExemptionReasonLen's
// own doc comment names).
//
// Splitting on hyphens also fragments a genuine reason's own hyphenated
// compound words ("in-sandbox", "Sandbox-agent") and even one
// denylisted phrase's own internal hyphen ("not user-facing",
// "self-explanatory") into pieces that no longer match anything in
// vacuousExemptionReasons. reasonClausesAllVacuous below is unaffected by
// that imprecision in the direction that matters: it can only ever
// produce a FALSE NEGATIVE (a concatenation that happens to fragment a
// denylisted phrase's own hyphen escapes detection), never a false
// positive against a genuine reason -- a real reason's substantive
// content (a file path, a route string, a caller's name) never vanishes
// just because one adjacent compound word got split into two
// non-denylisted pieces; some OTHER clause in the same reason still
// carries it.
var clauseSplitRE = regexp.MustCompile(`[,;:.!?()\-]+`)

// reasonClausesAllVacuous reports whether EVERY non-empty clause
// clauseSplitRE divides reason into is itself, after
// normalizeReasonForCheck, an exact vacuousExemptionReasons entry --
// catching a punctuation-joined concatenation like "internal only, not
// applicable, admin only" or hyphen-padding like
// "internal-internal-internal-internal" that clears both
// minExemptionReasonLen and the whole-string exact-match check above
// (neither is, as one whole string, a single denylisted phrase, and both
// are long enough), one clause at a time.
//
// The gap this does NOT close: a reason with NO punctuation at all
// between its concatenated stock phrases (e.g. "internal only not
// applicable admin only", four denylisted words run together with plain
// spaces) still yields exactly one clause -- clauseSplitRE finds nothing
// to split on -- and that one clause, as a whole string, matches no
// single vacuousExemptionReasons entry either. That is a real, narrower,
// still-open gap, not a claim this function makes and fails to keep; see
// guidedrift_test.go's own TestCheckGuideDrift_Omission ("known
// limitation" subtest) and docs/guides/README.md's own "Two rules, both
// enforced now" section for that gap stated as a passing test rather
// than left for someone to discover later.
//
// This cannot flag a genuine reason: every one of the real
// RouteGuideExemptions entries carries substantive content (a file path,
// a route string, a caller's name) that survives clause-splitting in at
// least one clause, so at least one clause is never in
// vacuousExemptionReasons and this returns false for every one of them --
// pinned by TestNoGuideDrift itself (a false positive here would fail
// the real register's own entries on every `go test ./...` run) and,
// narrower and faster, by
// TestRouteGuideExemptions_RealReasonsPassVacuousCheck in
// guideomission_test.go.
func reasonClausesAllVacuous(reason string) bool {
	clauses := clauseSplitRE.Split(reason, -1)
	sawClause := false
	for _, c := range clauses {
		norm := normalizeReasonForCheck(c)
		if norm == "" {
			continue
		}
		sawClause = true
		if !vacuousExemptionReasons[norm] {
			return false
		}
	}
	return sawClause
}

// isWellFormedExemptionRoute reports whether route looks like a real
// "METHOD /path" route string -- the exact same shape GuideCommand.Validate
// (guide.go) already requires of a documented route's own Route field,
// deliberately reusing validRouteMethods rather than defining a second,
// possibly-diverging set. ok=false means route is too malformed to look up
// in a real route table at all (validateRouteGuideExemptions reports this
// and skips the staleness/contradiction checks below it, which need a
// well-formed string to compare).
func isWellFormedExemptionRoute(route string) (ok bool, reason string) {
	if route == "" {
		return false, "route is empty"
	}
	parts := strings.Fields(route)
	if len(parts) != 2 {
		return false, "must look like \"METHOD /path\""
	}
	if !validRouteMethods[parts[0]] {
		return false, "method must be one of GET/POST/PUT/PATCH/DELETE"
	}
	if !strings.HasPrefix(parts[1], "/") {
		return false, "path must start with \"/\""
	}
	return true, ""
}

// exemptionRouteIsWildcard reports whether a WELL-FORMED route string
// (isWellFormedExemptionRoute already passed) still tries to name more than
// one real route at once. This package deliberately implements no pattern-
// matching lookup ANYWHERE -- RouteGuideExemptions is matched against a real
// routes map by plain string equality only (guidedrift.go), so a "*" or a
// "..." here could never actually exempt a future route even by accident;
// this check exists purely to REFUSE the attempt with a precise message,
// rather than let it fall through to the generic (and, for this specific
// shape, misleadingly mild-sounding) "stale entry" error a would-be
// wildcard also always produces, since it never matches any single real
// route either.
func exemptionRouteIsWildcard(route string) bool {
	return strings.Contains(route, "*") || strings.Contains(route, "...")
}

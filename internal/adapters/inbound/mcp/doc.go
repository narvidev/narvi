// Package mcp implements the MCP (Model Context Protocol) surface: the
// entry point, transport, protocol-version gate, and the first three
// tools (technical plan §43, "the MCP surface").
//
// # What this is, plainly
//
// This surface is bearer-authenticated ONLY (technical plan §43.2): an
// access token issued by this deployment's own OAuth authorization server
// (internal/adapters/inbound/mcpauth, §43.13-§43.16) and verified on
// every call by auth.RequireMCPBearer, which attaches the SAME
// platform.AuthenticatedUser a cookie would, plus the platform.MCPGrant
// the token was issued under. No cookie authenticates this route. It is
// disabled by default (NARVI_MCP_ENABLED, platform.Config.MCPEnabled).
// The MCPGrant's scopes -- the access token's own, fixed when it was
// issued (§43.16) -- decide which tools a request can even SEE
// (§43.17, buildServer); they never widen what a tool may do, because
// every tool runs its REST twin's own authorization against the user's
// own role. Result/verdict tools, bounded wait, transcript paging, and the
// plan/prompt/stop/delegate tools belong to later rows of the plan; none
// of that is here.
//
// # Registration (controlplane/serve.go)
//
// POST /mcp is mounted at the router ROOT, not under /api/ (§43.6: a
// protocol endpoint, the same category as GET /sessions/{id}/ws or
// /webhooks/*). Its routes.golden row is still graded by
// tools/contractscompat exactly like an /api/ row, so a change to it
// still takes a contracts VERSION bump and a CHANGELOG entry (§43.6,
// contracts/COMPATIBILITY.md "Routes"). The registration form is
// load-bearing --
// internal/ops.ScanRegisteredRoutes only recognizes Get/Post/Put/Patch/
// Delete plus Route(...) groups, so this is ALWAYS:
//
//	router.Route("/mcp", func(r chi.Router) {
//	    r.Use(mcp.RequireTrustedOrigin(...)) // 403 on a bad Origin, FIRST
//	    r.Use(mcp.RequireEnabled(cfg.MCPEnabled)) // 503 when off, SECOND
//	    r.Use(auth.RequireMCPBearer(grantStore, bearerCfg)) // bearer only, THIRD
//	    r.Post("/", mcpHandler.ServeHTTP)
//	})
//
// never router.Handle/.Mount/.Method -- those are invisible to the static
// scanner and fail TestScanRegisteredRoutes_MatchesGolden. The group is
// mounted UNCONDITIONALLY regardless of the flag's value (§43.11): a surface
// that is off must be OBSERVABLE as off (503), never a route that simply
// does not exist -- the same discipline the OIDC routes already
// establish. Gate order is deliberate and load-bearing (§43.2): mcp.
// RequireTrustedOrigin runs FIRST, because the Streamable HTTP transport
// spec's own "if the Origin header is present and invalid, servers MUST
// respond with HTTP 403 Forbidden" is unconditional -- true whether the
// surface is enabled, whether the caller is authenticated, or which
// protocol version the request names, none of which any LATER gate can
// know without running first (a prior revision of this package left the
// equivalent check to run LAST, deep inside NewHandler's own returned
// handler, so an invalid Origin got 403 only when every other gate ALSO
// happened to pass -- 503/401/-32022 otherwise). mcp.RequireEnabled runs
// second, answering 503 before any credential store is ever touched (§43
// D9); only once the surface is known to be on does auth.RequireMCPBearer
// run, producing the SAME generic 401 body every other route produces,
// plus the WWW-Authenticate: Bearer challenge that tells a client where
// to find the authorization server.
//
// # The bridge: one authorization path, never a second (§43.7)
//
// Every tool in this package is implemented by invoking the EXISTING
// REST http.HandlerFunc in-process, through an httptest recorder, with
// the authenticated request's own context (see bridge.go). This is
// deliberately NOT a second implementation sharing some extracted "core"
// function: it is the identical function httpapi's own router already
// calls, so the same authz.Authorize call, the same DTO encoder, and the
// same log lines run either way. tools/lint/narvichecks' mcpimportban
// analyzer enforces the structural half of this: this package may import
// the SDK, httpapi, auth, platform, contracts (+contracts/gen/go/
// restdtos), chi, and stdlib -- never internal/adapters/outbound/postgres
// (or sqlcgen), internal/domain/authz, internal/app/actorauthz, or any
// internal/app/* service. With that ban in place, this package cannot
// reach a store or render an authz verdict except through an HTTP
// handler it did not write itself.
//
// # Argument validation (schemas.go)
//
// The pinned MCP SDK's low-level Server.AddTool(*Tool, ToolHandler) path
// -- the one this package uses, deliberately, so mapOutcome (below) can
// render a business refusal as IsError:true and toolHandler can wrap the
// whole call in a recover (tools.go) -- validates NOTHING against a
// tool's own InputSchema itself; its own doc comment says unmarshaling
// and validating arguments is entirely "the caller's responsibility". A
// prior revision of this package's docs and contracts/rest/v1/
// dtos.schema.json claimed the opposite (that the pinned SDK's own
// argument validation "DOES enforce enum/minimum" on this path) -- that
// claim was never true on the AddTool path this package actually calls;
// see git history for the correction. validateArguments is this package
// acting as that caller: every tool's raw arguments are validated, once,
// against that SAME tool's own contracts $def (enum, minimum,
// format:"uuid", additionalProperties:false, all of it -- deliberately
// NOT "maximum": no $def in this bundle declares that keyword; §43.10
// explains why ListSessionsToolRequest.limit carries no upper bound)
// before BuildRequest or the twin ever sees them.
//
// # HTTP outcome -> MCP outcome (outcome.go)
//
//   - 200                        -> a successful CallToolResult:
//     Content[0].Text and StructuredContent are the REST body's own
//     bytes, verbatim -- never re-encoded, so a caller sees byte-for-byte
//     what the REST route would have written.
//   - 400 / 403 / 404 / 409       -> a successful CallToolResult with
//     IsError:true, Content[0].Text = the REST body's own "error" string,
//     no StructuredContent. Per the MCP tools specification, an
//     input-validation failure (400) is classified a TOOL EXECUTION
//     error exactly like a business refusal (403/404/409, §43 D6: RBAC
//     denial, "not found", a conflict) -- never a JSON-RPC protocol
//     code, so the model can read the text and correct itself. In
//     practice this row is rarely reached for 400: validateArguments
//     above already rejects (also as IsError:true, with the schema
//     validator's own message) nearly everything a twin would otherwise
//     400 on; this is what remains reachable for a value the schema's
//     own value-space genuinely cannot express.
//   - 401                         -> unreachable inside the bridge
//     (auth.RequireMCPBearer already ran); if ever seen, a defect signal:
//     -32603, logged loudly.
//   - anything else                -> -32603 (jsonrpc.CodeInternalError),
//     "internal error" -- never the raw body text.
//   - a PANIC anywhere in a tool's own handler (BuildRequest, callTwin,
//     mapOutcome) -> caught by toolHandler's own recover (tools.go):
//     logged with whatever correlation id the request's own context
//     carries, -32603, "internal error". Defense in depth: bridge.go's
//     own doc comment covers the concrete defect (an unparseable
//     synthesized request panicking) this closes, but the recover
//     protects the whole call, not merely that one line.
//
// # What is emphatically NOT here
//
// No repository discovery (needs a REST route this codebase does not
// have yet, §43 D5). No result/verdict/wait/poll tools (182). No plan/
// delegate/prompt/stop tools (183). No OAuth endpoint of its own (the
// authorization server is internal/adapters/inbound/mcpauth; this package
// only reads the grant auth.RequireMCPBearer attached). No resources, no
// prompts, no logging capability -- capabilities advertise {"tools":{}}
// only, no listChanged (the tool set is static per request).
//
// # Scope-gated discovery (§43.17)
//
// Every toolSpec names the scope it requires. buildServer registers only
// the tools the request's grant satisfies (mcpscope.Satisfies, which
// fails closed on an unset or unknown scope), so a scope-less grant gets
// tools/list == [] and a tools/call naming a hidden tool gets the SDK's
// own "unknown tool" error -- the same bytes as a name that never
// existed. instructionsFor composes the instructions paragraph from the
// same visible set, and defectServer (no principal or no grant) has no
// tools at all. AdvertisedScopes is the one list of scopes this build
// offers, derived from the same table.
package mcp

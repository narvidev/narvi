// Package mcp implements the MCP (Model Context Protocol) surface: the
// entry point, transport, protocol-version gate, and the first three
// tools (technical plan §43, "the MCP surface").
//
// # What this is, plainly
//
// This surface is cookie-authenticated (the SAME narvi_auth_session
// cookie and auth.Middleware every /api/** route already uses -- see
// §43.2 below), disabled by default (NARVI_MCP_ENABLED, platform.Config.
// MCPEnabled), and NOT usable by a real, off-the-shelf MCP client: no
// browser-based client carries a cookie, and no bearer-token credential
// exists for a human caller yet. What CAN use it today is a test client
// built against this same repository (the "existing MCP clients" this
// Step's own exit criterion names) and, in a later Step, an operator's
// own scripted tooling. 181 is the Step that makes this surface usable by
// a real third-party client (OAuth 2.1 resource-server behavior, RFC 9728
// protected-resource metadata, per-principal tool filtering). 182 adds
// result/verdict tools, bounded wait, and transcript paging. 183 adds
// plan read/approve/reject, prompt-while-running, stop, and delegate
// (create session). None of that is here.
//
// # Registration (controlplane/serve.go)
//
// POST /mcp is mounted at the router ROOT, not under /api/ (§43.6: a
// protocol endpoint, the same category as GET /sessions/{id}/ws or
// /webhooks/*, never graded by tools/contractscompat's own /api/-only
// DiffRoutes). The registration form is load-bearing --
// internal/ops.ScanRegisteredRoutes only recognizes Get/Post/Put/Patch/
// Delete plus Route(...) groups, so this is ALWAYS:
//
//	router.Route("/mcp", func(r chi.Router) {
//	    r.Use(mcp.RequireEnabled(cfg.MCPEnabled)) // 503 when off, FIRST
//	    r.Use(auth.Middleware(userSessionStore, userStore)) // same /api gate
//	    r.Post("/", mcpHandler.ServeHTTP)
//	})
//
// never router.Handle/.Mount/.Method -- those are invisible to the static
// scanner and fail TestScanRegisteredRoutes_MatchesGolden. The group is
// mounted UNCONDITIONALLY regardless of the flag's value (§43.11): a surface
// that is off must be OBSERVABLE as off (503), never a route that simply
// does not exist -- the same discipline the OIDC routes already
// establish. Gate order is deliberate: RequireEnabled answers 503 before
// the session store is ever touched (§43 D9); only once the surface is
// known to be on does auth.Middleware run, producing the SAME generic
// 401 body every other route produces on a missing/expired/disabled
// session.
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
// # HTTP outcome -> MCP outcome (outcome.go)
//
//   - 200            -> a successful CallToolResult: Content[0].Text and
//     StructuredContent are the REST body's own bytes, verbatim -- never
//     re-encoded, so a caller sees byte-for-byte what the REST route
//     would have written.
//   - 400             -> a JSON-RPC PROTOCOL error, code -32602
//     (jsonrpc.CodeInvalidParams), message = the REST body's own "error"
//     string. A request-STRUCTURE problem, not a business refusal --
//     mostly unreachable in practice, since the SDK's own input-schema
//     validation (github.com/google/jsonschema-go) catches most malformed
//     arguments before this package's handler ever runs; see schemas.go's
//     own doc comment for the one keyword-enforcement gap (format:"uuid"
//     is NOT enforced by that library) that is exactly why a malformed
//     sessionId DOES still reach the bridge and this mapping.
//   - 403 / 404 / 409 -> a successful CallToolResult with IsError:true,
//     Content[0].Text = the REST body's own "error" string, no
//     StructuredContent. A business refusal the model or user can act on
//     (§43 D6: RBAC denial is a tool execution error, never a JSON-RPC
//     application code).
//   - 401             -> unreachable inside the bridge (auth.Middleware
//     already ran); if ever seen, a defect signal: -32603, logged loudly.
//   - anything else    -> -32603 (jsonrpc.CodeInternalError), "internal
//     error" -- never the raw body text.
//
// # What is emphatically NOT here
//
// No repository discovery (needs a REST route this codebase does not
// have yet, §43 D5). No result/verdict/wait/poll tools (182). No plan/
// delegate/prompt/stop tools (183). No OAuth, no bearer credential, no
// per-principal tool filtering (181). No resources, no prompts, no
// logging capability -- capabilities advertise {"tools":{}} only, no
// listChanged (the tool set is static per build).
package mcp

# Changelog

All notable changes to `/contracts` are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this
bundle's own version lives in `contracts/VERSION` (kept equal to
`contracts/package.json`'s and `contracts/manifest.json`'s `"version"`
fields). See `COMPATIBILITY.md` for
what counts as a breaking (MAJOR), additive (MINOR), or annotation-only
(PATCH) change, and for the VERSION/CHANGELOG discipline
`make contracts-compat` enforces on every PR that touches a schema,
`manifest.json`, or `controlplane/testdata/routes.golden`.

## [1.6.0]

### controlplane/testdata/routes.golden

- Added: `GET /api/members/{userID}/mcp-authorizations` and
  `DELETE /api/members/{userID}/mcp-authorizations/{authorizationID}` --
  an administrator's view of one member's MCP authorizations, and their
  revocation on the member's behalf (technical plan §43.18), admin only
  (`authz.ActionManageMembers`, `403` for every other role). The list
  answers the existing `ListMCPAuthorizationsResponse`, the very shape
  `GET /api/me/mcp-authorizations` answers; `404` when `userID` names no
  user. The revocation answers `204`, or `404` for an authorization that is
  not that member's -- whoever's it is -- and is audited as
  `mcp_authorization.revoked` with reason `admin`. Two routes added,
  graded MINOR (row 41).
- Unchanged as routes, changed in behaviour: `GET /oauth/authorize` and
  `POST /oauth/token` are now braked per client network (technical plan
  §43.14). Over the budget, the authorization endpoint answers its HTML
  error page with `429` and `Retry-After` (never a redirect), and the
  token endpoint `429` with `Retry-After` and the JSON body
  `{"error":"temporarily_unavailable",...}`. `GET /oauth/authorize` also
  refuses a client that already has 100 authorization requests waiting for
  a decision, with an HTML error page (`503`), storing nothing. Neither
  route's request or success response changed, and neither is a
  `/contracts` schema.

### rest/v1/dtos.schema.json

- Changed (descriptions only, annotation-only PATCH):
  `MCPAuthorization` and `ListMCPAuthorizationsResponse` now also name the
  two admin routes above, and say that an administrator's view of a
  member's authorizations is exactly what the member sees. No field, type,
  enum value or requiredness changed.

## [1.5.0]

### controlplane/testdata/routes.golden

- Added: `POST /oauth/register` -- the MCP authorization server's RFC 7591
  dynamic client registration endpoint (technical plan §43.14/§43.15).
  Mounted unconditionally like every `/oauth` route; it answers the
  surface's disabled response unless the deployment sets
  `NARVI_MCP_DCR_ENABLED` (off by default), and is rate-limited per client
  address. A route added, graded MINOR (row 41). Its request and response
  are RFC 7591's own JSON shapes, not `/contracts` schemas.
- Unchanged as routes, changed in behaviour: `GET /oauth/authorize` now
  also accepts an `https` `client_id` -- a client ID metadata document,
  fetched through an SSRF-guarded fetcher (`NARVI_MCP_CIMD_ENABLED`, on by
  default) -- and `GET /.well-known/oauth-authorization-server/oauth`
  advertises `client_id_metadata_document_supported` and
  `registration_endpoint` while each mechanism is on.

### rest/v1/dtos.schema.json

- Changed (descriptions only, annotation-only PATCH):
  `MCPAuthorization.clientKind` and `MCPClient.kind` no longer say only
  `preregistered` is produced: `metadata_document` and `dynamic` are
  produced now, and each value is described. Both stay open enums, with
  the same three values. `MCPAuthorization.clientName` now says a
  self-registered client chose its own name. `CreateMCPClientRequest`
  describes the stricter client-name rule every registration path now
  shares -- printable characters only, spelled out, and at most three
  combining marks stacked on one character -- and the redirect-URI and
  `clientUri` rules as they now stand: printable ASCII of at most 2048
  bytes, and the host written in plain ASCII (no percent sign in the
  authority; an internationalized host in its `xn--` form). Three kinds of
  `POST /api/mcp-clients` request that 1.4.1 accepted are now refused 400:
  - a client name with, anywhere but at either end, a character 1.4.1 let
    through that is not printable: a space other than U+0020 (a no-break,
    ideographic or thin space, and the other Unicode spaces), a line or
    paragraph separator (U+2028, U+2029), or a private-use or unassigned
    code point. 1.4.1 refused only control and invisible formatting
    characters. White space at either end is still trimmed, not refused;
  - a client name stacking more than three combining marks on one
    character;
  - a redirect or homepage URI with a percent sign in its authority: a
    percent-encoded host, or an IPv6 zone identifier.

  Nothing 1.4.1 refused is accepted now. In the schema only descriptions
  changed: no field, type, enum value or requiredness.

## [1.4.1]

### rest/v1/dtos.schema.json

- Changed (description only, annotation-only PATCH):
  `MCPAuthorization.expiresAt` now says what it is since refresh chains
  keep their own end (technical plan §43.16): when the authorization
  itself lapses, `MCPGrantMaxLifetime` after the latest consent, which is
  the latest any install of the client can keep refreshing. An install
  connected by an earlier consent must consent again sooner, when that
  consent's own chain ends. The field, its type and its value are
  unchanged.

### controlplane/testdata/routes.golden

- Added (not `/api/`, so not graded by `tools/contractscompat`'s own
  `DiffRoutes`; recorded for the same VERSION/CHANGELOG discipline):
  `POST /oauth/revoke` -- the MCP authorization server's RFC 7009 token
  revocation endpoint (technical plan §43.14/§43.16). `POST /oauth/token`
  is unchanged as a route; it now also accepts
  `grant_type=refresh_token`, and its responses carry a `refresh_token`.
  No `/api/` route changed and no DTO changed shape, so the highest
  finding class is PATCH.

## [1.4.0]

### rest/v1/dtos.schema.json

- Added: `MCPAuthorization`, `ListMCPAuthorizationsResponse` -- the
  response of `GET /api/me/mcp-authorizations` (technical plan §43.18):
  the caller's own connected MCP clients, each revocable by
  `DELETE /api/me/mcp-authorizations/{authorizationID}`.
- Added: `MCPClient`, `ListMCPClientsResponse`, `CreateMCPClientRequest`
  -- admin pre-registration of MCP clients at `GET`/`POST
  /api/mcp-clients` and `DELETE /api/mcp-clients/{clientID}` (technical
  plan §43.15). `CreateMCPClientRequest` is classified client-to-platform
  by the existing `*Request`-suffix rule; the others platform-to-client.
- All five are wholly new, independent named shapes, additive to this
  schema; no existing shape changed. None carries a token, code or other
  secret. `MCPAuthorization.clientKind` and `MCPClient.kind` are open
  enums (`manifest.json`'s `openEnums` gains both pointers): only
  `preregistered` is produced today, so consumers must tolerate the two
  values reserved for later client-registration mechanisms.

### controlplane/testdata/routes.golden

- Added: `GET /api/me/mcp-authorizations`,
  `DELETE /api/me/mcp-authorizations/{authorizationID}`,
  `GET /api/mcp-clients`, `POST /api/mcp-clients`,
  `DELETE /api/mcp-clients/{clientID}` -- the Settings routes above.
- Added (not `/api/`, so not graded by `tools/contractscompat`'s own
  `DiffRoutes`, recorded for the same VERSION/CHANGELOG discipline): the
  MCP authorization server's own routes (technical plan §43.14) --
  `GET /.well-known/oauth-protected-resource/mcp`,
  `GET /.well-known/oauth-authorization-server/oauth`,
  `GET /oauth/authorize`, `GET /oauth/consent`, `POST /oauth/consent`,
  `POST /oauth/token`. `POST /mcp` is unchanged as a route; it now accepts
  only a bearer token from that authorization server (technical plan
  §43.2).

## [1.3.0]

### rest/v1/dtos.schema.json

- Added: `ListModelsToolRequest`, `ListSessionsToolRequest`,
  `GetSessionToolRequest` -- the input shapes for the first three MCP
  tools (`narvi_list_models`, `narvi_list_sessions`, `narvi_get_session`;
  technical plan §43, "the MCP surface", Step 180). Each is the exact
  wire `inputSchema` its tool's `tools/list` entry carries, passed
  verbatim (never reflected) into the official Go SDK's `mcp.Tool.
  InputSchema`. All three are wholly new, independent named shapes,
  additive to this schema -- no existing shape changed. Classified
  client-to-platform by the existing `*Request`-suffix `by-suffix`
  direction rule (`manifest.json`), no checker change needed. The three
  tools' OUTPUT shapes are NOT new `$defs`: they reuse `ModelCatalog`,
  `ListSessionsResponse`, and `Session` unchanged, bundled with their own
  transitive `$defs` at boot (`internal/adapters/inbound/mcp/schemas.go`).

### controlplane/testdata/routes.golden

- Added: `POST /mcp` -- the MCP Streamable HTTP endpoint (§43), mounted
  unconditionally (gated 503 at request time by `NARVI_MCP_ENABLED`,
  default off). Not an `/api/` route, so `tools/contractscompat`'s own
  `DiffRoutes` (rows 40/41) does not grade it either way -- recorded here
  only for the same VERSION/CHANGELOG discipline every schema-touching PR
  follows.

## [1.2.0]

### rest/v1/dtos.schema.json

- Added: `Identity.properties.provider.enum` and
  `PendingLinkPrompt.properties.provider.enum` each gain `"oidc"` --
  `identities.provider` (Postgres) widened the same way in migration
  000140 to support generic OIDC sign-in (technical plan §41.3): an
  OIDC-linked identity now legitimately appears in `GET /api/me` and
  `GET /api/members`'s own `identities` array. Both fields are already
  open enums as of 1.1.0 (`manifest.json`'s `openEnums`), so
  `tools/contractscompat` classifies this as additive (MINOR), not
  breaking -- consumers were already required to tolerate an
  unrecognised value on these two fields. `LinkMemberIdentityRequest.
  properties.provider`'s own description is updated to match (that field
  was already an unconstrained string, application-layer-validated --
  `internal/adapters/inbound/httpapi/members.go`'s `validProviders` map
  gains the same `oidc` entry).
- Added: `AuthCapabilitiesResponse` (`oidcConfigured`) -- the response body
  of a NEW public, unauthenticated route, `GET /auth/capabilities`
  (technical plan §41.3), letting the sign-in view learn whether this
  deployment has a generic OIDC SSO provider configured before a visitor
  is signed in at all. A wholly new, independent named shape, additive to
  this schema -- no existing shape changed.

## [1.1.0]

### rest/v1/dtos.schema.json

#### Changed

- `Identity.provider` and `PendingLinkPrompt.provider` are now open enums (listed in `manifest.json`'s `openEnums`). Consumers MUST tolerate a provider value they do not recognise on these two fields. No value is added in this release; a following release adds `oidc` (second sign-in provider, technical plan §41.3).

## [1.0.0]

Baseline: `/contracts` becomes a versioned, policy-governed external API
(technical plan §6.3). No schema content changed in this release — this
entry establishes the starting point every future entry diffs against.

### rest/v1/dtos.schema.json

- Added: optional `CapabilitiesResponse.contractsVersion`, mirroring
  `contracts.Version` (this bundle's own `contracts/VERSION`), so a caller
  of `GET /api/capabilities` can see which contracts version the running
  control plane was built against.

### client-ws/v1/protocol.schema.json

- No content change; now governed by `contracts/manifest.json` and
  `tools/contractscompat`.

### sandbox-ws/v1/commands.schema.json

- No content change; now governed by `contracts/manifest.json` and
  `tools/contractscompat`.

### sandbox-ws/v1/events.schema.json

- No content change; now governed by `contracts/manifest.json` and
  `tools/contractscompat`.

### session-config/v1/session-config.schema.json

- No content change; now governed by `contracts/manifest.json` and
  `tools/contractscompat`.

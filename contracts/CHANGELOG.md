# Changelog

All notable changes to `/contracts` are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this
bundle's own version lives in `contracts/VERSION` (kept equal to
`contracts/package.json`'s `"version"` field). See `COMPATIBILITY.md` for
what counts as a breaking (MAJOR), additive (MINOR), or annotation-only
(PATCH) change, and for the VERSION/CHANGELOG discipline
`make contracts-compat` enforces on every PR that touches a schema,
`manifest.json`, or `controlplane/testdata/routes.golden`.

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

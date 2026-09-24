# Changelog

All notable changes to `/contracts` are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this
bundle's own version lives in `contracts/VERSION` (kept equal to
`contracts/package.json`'s `"version"` field). See `COMPATIBILITY.md` for
what counts as a breaking (MAJOR), additive (MINOR), or annotation-only
(PATCH) change, and for the VERSION/CHANGELOG discipline
`make contracts-compat` enforces on every PR that touches a schema,
`manifest.json`, or `controlplane/testdata/routes.golden`.

## [1.1.0]

### rest/v1/dtos.schema.json

- Added: `AuthCapabilitiesResponse` (`oidcConfigured`) -- the response body
  of a NEW public, unauthenticated route, `GET /auth/capabilities`
  (technical plan §41.3, "OIDC as a second sign-in provider"), letting the
  sign-in view learn whether this deployment has a generic OIDC SSO
  provider configured before a visitor is signed in at all. A wholly new,
  independent named shape, additive to this schema -- no existing shape
  changed.

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

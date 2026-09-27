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

## [1.9.0]

### controlplane/testdata/routes.golden

- Added: `GET /api/sessions/{sessionID}/result` -- what one session has
  produced (technical plan §43.20, row 182's result). Answers the new
  `SessionOutcome`; the same gate as `GET /api/sessions/{sessionID}`
  (signed in, `400` for a malformed id, `404` for a session that does not
  exist, no per-session visibility). It is also the twin of the new
  `narvi_get_session_result` MCP tool. A route added, graded MINOR (row
  41).

### rest/v1/dtos.schema.json

- Added: `SessionOutcome` -- the response of the result route above:
  `activity` (`SessionActivity.activity` at the same snapshot, so a
  reader knows whether the result can still change); `lastRun` (the last
  turn that ended: outcome, `failureReason` under
  `SessionActivity.lastRun.failureReason`'s rule, `startedAt`,
  `finishedAt`, `costUsd` as `WorkflowStepRun.costUsd` reads it,
  `planMode`, and `summary` -- the run's final text, copied as streamed,
  never written by a model, cut at 4,000 characters with `truncated`);
  `reviewScope` (`none`, `produced` or `reviewed`: `none` says there is
  no pull request to review, so an empty list never reads as a clean
  review); `pullRequests` (the pull requests the session opened);
  `excludedPullRequests` (the session's pull request records that name no
  pull request it opened -- a creation suppressed in shadow mode, or a
  record the server cannot read -- each listed with why, so none is
  dropped and none fails the result); `reviewedPullRequest` (the one it is
  the review session of); and `suggestedDelaySeconds` (how long to wait
  before reading the result again: 30 seconds while the session can still
  change, 60 once settled when a freshness was read live, 300 when nothing
  was, as shipped). Carries no events. A `$defs` entry added, graded MINOR
  (row 32).
- Added: `SessionOutcomePullRequest`, `SessionOutcomeReview` and
  `SessionOutcomeVerdict` -- a pull request with its review:
  `state` (`absent`, `in_progress`, `not_assessed` -- with the older
  verdict in `supersededVerdict`, never the answer -- or `assessed`),
  `verdict`, `supersededVerdict`, and `freshness` (`current` only when the
  verdict's recorded context matched the pull request's live facts, read
  from the code host in that call; `stale` or `unconfirmed` with a
  `reason` -- the merge path's own for a verdict the record decides, the
  head read live first, and `unconfirmed` when the call's time budget for
  live reads ran out; `not_applicable` for any state but `assessed`, a
  pull request merged per this system's records, or one the live read
  finds no longer open). A verdict copies its record: id, attempt, head,
  risk level, shippable, when it was posted, and its context (base ref
  and commit, null when never recorded; policy version; ancestor chain
  length). `$defs` entries added, graded MINOR (row 32).
- Added: `SessionOutcomeExcludedPullRequest` -- one entry of
  `excludedPullRequests`: `kind` (`shadow_suppressed`: the creation was
  suppressed because egress to the repository was in shadow mode, so no
  pull request exists; `unreadable`: the record could not be read),
  `repoFullName` (null when it cannot be named), `url` (the one an
  unreadable record holds; always null for a suppressed creation),
  `createdAt` and `reason`. It has no review. A `$defs` entry added, graded
  MINOR (row 32).
- Added: `rest/v1/dtos.schema.json#/$defs/SessionOutcomeExcludedPullRequest/properties/kind`
  to `manifest.json`'s `openEnums`, so a later kind of excluded record
  grades MINOR: a consumer MUST tolerate a `kind` it does not recognise.
- Added: `GetSessionResultToolRequest` -- the input schema of the new
  `narvi_get_session_result` MCP tool (`sessionId`), whose output is
  `SessionOutcome`. A `$defs` entry added, graded MINOR (row 32).

## [1.8.0]

### rest/v1/dtos.schema.json

- Added: `SessionActivity.wait` -- how a wait on
  `GET /api/sessions/{sessionID}/status?waitSeconds=N` ended (technical
  plan §43.20, row 182's bounded wait): `reason` (`settled`, `timeout`,
  `interrupted` -- the server began shutting down -- or `capacity` -- the
  replica serving it already runs the most waits one of its three caps
  allows: the caller's (per MCP authorization, or per user for a browser),
  the user's across all of their authorizations and their browser, or all
  callers' -- so the read did not wait: a normal answer, never an error)
  and `waitedMs`. Present
  only on a read that waited; a plain read (no `waitSeconds`, or `0`) is
  byte-for-byte what it was. A property added, not required, to a
  platform-to-client shape, graded MINOR (row 2).
- Changed: `SessionActivity`'s description now documents the route's new
  `waitSeconds` query parameter: a whole number of seconds, clamped to the
  deployment's maximum (25 as shipped); absent or `0` is the plain read, a
  negative or malformed value a `400`. The route itself is unchanged in
  `routes.golden`. A description changed, graded PATCH (row 35).
- Added: `WaitForSessionToolRequest` -- the input schema of the new
  `narvi_wait_for_session` MCP tool (`sessionId`, and an optional
  `waitSeconds` with `minimum: 1`, omitted meaning the longest wait the
  deployment allows), whose output is `SessionActivity` with `wait` set. A
  `$defs` entry added, graded MINOR (row 32).

## [1.7.0]

### controlplane/testdata/routes.golden

- Added: `GET /api/sessions/{sessionID}/status` -- what one session's work
  is doing now, and how long to wait before reading it again (technical
  plan §43.20). Answers the new `SessionActivity`; the same gate as
  `GET /api/sessions/{sessionID}` (signed in, `400` for a malformed id,
  `404` for a session that does not exist, no per-session visibility). It
  is also the twin of the new `narvi_get_session_status` MCP tool. A route
  added, graded MINOR (row 41).
- Unchanged as a route: `GET /api/sessions/{sessionID}/events` is now also
  the twin of the new `narvi_get_session_transcript` MCP tool; its request
  and response are unchanged.

### rest/v1/dtos.schema.json

- Added: `SessionActivity` -- the response of the status route above:
  `activity` (`idle`, `queued`, `running`, `delivering` -- a completed
  turn's branch being pushed and its pull request opened, bounded by the
  deployment's delivery window; a pull request it opens is recorded before
  it ends, except when two pushes overlap, and it can end with none --
  `scheduled` -- the server holds work that may create a turn with no new
  input and has neither created it nor declined yet: an automatic
  re-review's debounce, counted only while the pull request's repository
  has opted in and its automatic re-review budget is not spent; a release
  manifest check waiting or running --
  `awaiting_approval`, `finished`), `settled` (never while queued,
  running, delivering or scheduled; one stated limit: when a pull
  request's review session pushes to that pull request's head, the
  automatic re-review the push causes is armed only when the code host's
  notification of it arrives, so the session can read `finished` and
  settled until then, then `scheduled` and `queued` -- the same after a
  push that reached the remote but reported a failure), `pendingTurns`,
  `inFlightTurn`,
  `awaiting` (the open human gate: a plan, a workflow step, or a custom
  workflow's escalated run until a turn is created on the session after
  it escalated -- never a built-in workflow's; a turn created before the
  escalation, queued, running or ended, never closes it), `lastRun`,
  `sandboxStatus`, `archived`,
  `suggestedDelaySeconds` and `observedAt`. Derived from the session's
  turn queue, its push/PR delivery, the work armed to create a turn and
  its human gates in one database snapshot, never from `Session.status`,
  and carrying no events. A `$defs` entry added, graded MINOR (row 32).
- Added: `GetSessionStatusToolRequest` and `GetSessionTranscriptToolRequest`
  -- the input schemas of the `narvi_get_session_status` and
  `narvi_get_session_transcript` MCP tools (technical plan §43.20), whose
  outputs are the existing `SessionActivity` and `EventsResponse`.
  `sessionId` is a required uuid in both; the transcript's optional
  `cursor` is a decimal event id (the previous page's `nextCursor`) and its
  optional `limit` has `minimum: 1` and no maximum (the route clamps at
  500). Two `$defs` entries added, graded MINOR (row 32).
- Changed (description only, annotation-only PATCH): `Session.status` now
  says what it is -- derived each time a turn reaches a terminal state
  (`created` until one has, `active` when another turn was still open,
  otherwise that turn's outcome), not re-derived when a turn is queued or
  dispatched, so it does not show queued or running work and a queued or
  running turn can sit under any of its five values -- and points at
  `SessionActivity.activity` for what a session is doing now. No field,
  type, enum value or requiredness changed.

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
- Added: `POST /api/mcp-clients/{clientID}/disable` and
  `POST /api/mcp-clients/{clientID}/enable` -- an administrator disables
  or enables one MCP client of any kind (technical plan §43.15), admin only
  (`authz.ActionManageIntegrations`, `403` for every other role). No
  request body; each answers `200` with the existing `MCPClient`, as it now
  stands, `404` when `clientID` names no client, and `409` when the client
  is already in the state asked for. A disabled client is refused wherever
  a client acts from its next request, and nothing under it is deleted;
  its row is never swept, and a disabled metadata-document client's
  document is never fetched again. Audited as `mcp_client.disabled` and
  `mcp_client.enabled`. Two routes added, graded MINOR (row 41).
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
  member's authorizations is exactly what the member sees. `MCPClient`, its
  `id` and its `disabledAt` now name the disable and enable routes, and
  `disabledAt` says what a disabled client is refused. No field, type,
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

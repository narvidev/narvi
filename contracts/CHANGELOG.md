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

## [1.21.0]

### sandbox-ws/v1/events.schema.json

- Added: optional `cut` on `Token`, `ToolCall` and `ToolResult` (an
  object, closed, of the required integers `kept`, minimum 0, and
  `total`, minimum 1): set only on a frame the sandbox-agent cut to fit
  what the control plane's connection reads (technical plan §6.1), absent
  on a whole frame. The cut shortens one string at a UTF-8 boundary -- a
  `token`'s `text`, the longest string in a `tool_call`'s `input` or a
  `tool_result`'s `output` -- and ends it with a line of its own,
  `[text cut at <kept> of <total> bytes on its way from the sandbox]`;
  `kept` is the bytes of that string kept and `total` its whole length,
  in UTF-8 bytes of the unescaped string. Readers learn a cut only from
  this property, never from the text, and a cut frame yields to the stored
  whole text it was taken from and to a cut of that text keeping more.
  Nothing produces it yet: this release adds the readers, before the
  writer. A control plane that does not know it stores the raw payload,
  so the property survives, and shows the marker in the text. A property
  added, not required, grades MINOR (row 2), three times; each union
  member changes only by that property (row 30, recursing into row 2).

### rest/v1/dtos.schema.json

- Added: `Plan.cut`, required and nullable (an inline object of the
  required integers `kept` and `total`): the cut a plan's text carries
  when it is a frame the sandbox-agent cut, read from the frames `content`
  is read from (`plan.FinalText`'s cut report), never from the text; both
  -1 when the frame carried a cut the server could not read. Null for a
  whole plan, and for one whose `content` comes from its approval
  snapshot. A plan with a cut cannot be approved: the approve route
  answers `409` with the reason, and clients show it in place of Approve.
  A required property added grades MINOR producer-to-client (row 3);
  `Plan` is reachable from no request.
- Added: `DecisionInboxItem.planCutKept` and `planCutTotal`, each a
  required, nullable integer, flattened like `provenanceKind`: set on an
  awaiting plan whose text is cut, from the plan's own turn window, the
  read the approval itself makes; null otherwise. Row 3, twice, MINOR.
- Added: `SessionOutcome.lastRun.summary.cut`, required and nullable (the
  same inline object): set when the run's last text part is a cut frame.
  Row 3, MINOR.
- Changed: `SessionOutcome.lastRun.summary.text`'s description says the
  part is read at its newest frame that yields to no other. A description
  grades PATCH (row 35).
- Unchanged: no route; a cut plan's refused approval is a `409` with the
  existing `{"error": ...}` body, which this bundle does not grade.

## [1.20.0]

### sandbox-ws/v1/events.schema.json

- Added: optional `Ready.capabilities.maxFrameBytes` (integer, `minimum`
  1): the largest message, in bytes, this agent reads. An agent built from
  this release states it on every `ready`, with or without
  `promptReceipt`: its read limit, `MaxPromptFrameBytes`, 33554432. The
  control plane records it against the gen, the latest `ready` deciding,
  and holds every prompt frame to that gen to it, and never past its own
  `MaxPromptFrameBytes` (technical plan §3.3, §6.1). Absent, the gen is
  held to `MaxPromptFrameBytes` when the `ready` advertises
  `promptReceipt`, and otherwise to the WebSocket library's default read
  limit, 32768 bytes: a prompt over its gen's bound fails its turn at
  dispatch, naming both sizes, instead of being written to an agent that
  would lose it. A control plane that predates the property ignores it and
  still measures against `MaxPromptFrameBytes`, which an agent that states
  it reads. A property added, not required, grades MINOR (row 2), under the
  existing `capabilities` object; the `ready` union member changes only by
  that property (row 30, recursing into row 2).
- Unchanged: every other event, and the six critical types.

## [1.19.0]

### rest/v1/dtos.schema.json

- Added: `RepoEntitlement` (`repoFullName`, `revoked`, and `revokedAt`,
  `revokedByUserId`, `revokedByDisplayName`, `reason`, each null when the
  repository is not revoked; every field required,
  `additionalProperties: false`), the body of the three entitlement routes
  below: whether an administrator revoked a repository's eligibility for
  new sessions (technical plan §31.4, "Un-entitlement"), and when, by whom
  and why. A new `$def` grades MINOR (row 32).
- Added: `RevokeRepoEntitlementRequest` (`reason`, required, `minLength`
  1), the revoke route's request body. The reason is trimmed, must be 1
  to 500 characters and must hold no NUL character; `maxLength` is not a
  keyword this bundle's compatibility checker grades, so the
  500-character cap is the handler's (a 400) and the table's (a CHECK),
  stated in the `description`. A new `$def` grades MINOR (row 32).
- Unchanged: no existing `$def`, no enum.

### controlplane/testdata/routes.golden

- Added: `GET /api/repos/{owner}/{repo}/entitlement`,
  `POST /api/repos/{owner}/{repo}/entitlement/revoke` and
  `POST /api/repos/{owner}/{repo}/entitlement/restore` -- an
  administrator reads a repository's entitlement, revokes its eligibility
  for new sessions with a reason, and restores it (technical plan §31.4).
  All three answer `RepoEntitlement`; `403` unless
  `authz.ActionManageRepoEntitlement` admits the caller (admin only,
  §13.3), `404` for a repository the deployment does not know. Revoke
  answers `400` for a blank or over-long reason, or one holding a NUL
  character, and `409` when the repository is already revoked, keeping
  the first revocation; restore takes no body and answers `409` when the
  repository is not revoked.
  While revoked, session creation on every surface answers the refusal
  `repository entitlement revoked by an administrator: <repo>` (`403` over
  REST, a tool error over MCP), distinct from the `repository not entitled`
  refusal of an unknown repository. Three routes added grade MINOR
  (row 41).

## [1.18.0]

### sandbox-ws/v1/commands.schema.json

- Added: optional `Prompt.receiptRequested` (boolean, no `default`).
  True asks the agent to answer the prompt, and every later copy of it,
  with a `prompt_received` event and never to run a copy twice (technical
  plan §3.3, prompt receipts). The control plane sets it only on a prompt
  to a gen whose `ready` advertised `capabilities.promptReceipt`, so an
  older agent is never sent it, and a prompt to such an agent is
  byte-identical to before. An agent that ignores unknown keys runs the
  prompt once and sends no receipt. A property added, not required,
  grades MINOR (row 2).

### sandbox-ws/v1/events.schema.json

- Added: optional `Ready.capabilities` (object, closed), with one optional
  boolean, `promptReceipt`: what this gen's agent supports beyond the base
  protocol, sent on every `ready`. Absent means no capability; the control
  plane never infers one from `agentVersion`. A property added, not
  required, grades MINOR (row 2); the `ready` union member changes only by
  that property (row 30, recursing into row 2).
- Added: `PromptReceived`, the new `prompt_received` event, appended to the
  root `oneOf`: `promptMessageId` names the prompt it answers, `duplicate`
  says the agent had already received it and did not run it again, and
  `messageId` is deterministic, `prompt_received:{promptMessageId}`, so
  every copy of one prompt's receipt is stored once, by whichever
  control-plane binary stores it. Not critical: it carries no `ackId`, and
  the six critical types are unchanged. A new `$defs` entry grades MINOR
  (row 32), and a discriminated variant added under shape A grades MINOR
  (row 28); `prompt_received` was never assigned in an earlier release, so
  row 46 does not apply.
- Changed: the root description says why `prompt_received` is not among
  the critical types. A description grades PATCH (row 35).

## [1.17.0]

### rest/v1/dtos.schema.json

- Added: `PostedFinding.source` (optional, string or null), the pass of the
  review that produced a posted finding: `primary` or `counter_review`
  (technical plan §26.6's amendment). Optional in the schema. A value
  that is present must be one of the two (a garbled one is refused with
  `400`), and `counter_review` is refused off the deep path. An absent one
  is accepted in one case only: from a turn whose own stored prompt
  predates the source instruction, on a payload that reports no
  `additionsFactCheck` (a field only the new prompt names); it is stored
  as "source not recorded", the state a finding last published before
  this change already has. That case exists because this body follows the
  review prompt, which is rendered once, when the turn is created, and
  re-sent as stored: a turn rendered before this change -- queued,
  running, or re-dispatched to a respawned sandbox while the control plane
  is deployed -- posts its findings without the field, and refusing them
  would refuse a request an older client's instructions shaped. From a
  turn whose prompt asked for the field, an absent one is refused with
  `400`, like a garbled one, so an addition its reviewer forgot to label is
  never published as an ordinary finding. Self-reported: the server
  records it as stated. A property added, not required, with every older
  client's body still accepted, grades MINOR (row 2).
- Added: `PostReviewVerdictRequest.additionsFactCheck` (optional, string or
  null: `done` or `skipped`) and `additionsFactCheckKilled` (optional,
  integer or null, minimum 0), the reviewer's report of the second, diff-only
  fact-check run over what the counter-review added, recorded apart from
  `factCheck`/`factCheckKilled`. Refused off the deep path; a kill count is
  refused unless the run is reported `done`. Two properties added, not
  required, grade MINOR (row 2).
- Added: `ReviewReadoutFinding.source` and `ReviewReadoutFinding.additionCheck`
  (required, string or null): the source the finding's latest publication
  reported, null when none was recorded (a finding last published before
  this change, or posted with no source by a turn rendered before it),
  and, for a `counter_review` finding, the server's
  resolution -- `checked`, `not_run`, `not_found` or `unconfirmed`. Every
  value but `checked` marks the finding unverified. Required properties
  added to a platform-produced shape grade MINOR (row 3).
- Added: `ReviewReadoutVerdict.additionsFactCheck`,
  `additionsFactCheckKilled` and `additionsCheck` (optional, nullable): the
  second run as the reviewer reported it, and the server's resolution of
  the counter-review additions the verdict published. `additionsCheck` is
  null when the verdict published none -- including when the second run
  removed every addition, whose report and kill count are still carried --
  so every value but `checked` means additions were published marked
  unverified. Properties added, not required, grade MINOR (row 2).
- Added: `ReviewAnalytics.findingOutcomesBySource` (required, array or
  null) and the `ReviewAnalyticsFindingSourceCount` `$def` (`source`,
  `status`, `count`): every finding in the window counted per source --
  `primary`, `counter_review`, `counter_review_unverified`, `not_recorded`
  -- and status, the breakdown precision per source is read from
  (technical plan §26.5); `not_recorded` counts every finding last
  published with no source recorded. A required property added to a
  platform-produced shape grades MINOR (row 3); a new `$def` grades MINOR
  (row 32).
- Added: `DecisionInboxItem.unverifiedAdditions` (optional, integer or
  null): the still-open counter-review additions on a pull request that
  the server could not count as checked, shown apart from `findings`, as
  the posted comment, the readout and the Code review view show them. Set
  whenever `findings` is. A property added, not required, grades MINOR
  (row 2).
- Changed (description only): `DecisionInboxItem.findings` now leaves out
  those unverified additions, so the inbox row agrees with the Code review
  view. It is a display count: the merge gate's own open-findings count
  (technical plan §26.5) is unchanged and still includes them. PATCH
  (row 35).
- Changed (description only): `ReviewAnalytics.findingOutcomes` now counts
  every finding except the counter-review additions the server could not
  count as checked, which are counted apart in `findingOutcomesBySource`;
  a computed result is empty only when every finding in the window is
  such an addition. PATCH (row 35).
- Unchanged: no route, no enum. Every new string is unconstrained, like
  `ReviewReadoutVerdict.counterReview`, so a later value is not a breaking
  change.

## [1.16.0]

### client-ws/v1/protocol.schema.json

- Added: `FetchHistoryResponse.sandbox` (required, an object or null): the
  session's sandbox as the control plane holds it when the reply is
  assembled, in `SubscribedPayload.state.sandbox`'s shape; null when the
  session has no sandbox yet. The control plane now stores and broadcasts a
  `sandbox_status` event (payload `{"sandbox": {"gen", "status"}}`) in the
  transaction of every change of the sandbox's status or generation, so the
  `fetch_history` that broadcast prompts brings an open page the status the
  server derives (technical plan §3.2, §6.2) without a resubscribe. A
  required property added to a platform-produced shape grades MINOR (row 3).
  A client that predates it ignores the property, and skips the event as a
  type it does not know; a stored event's payload is outside this policy
  (`SubscribedPayload.events` elements), and this one keeps its values under
  `sandbox`, never at the top level, so a reader of an agent event's
  top-level `gen` never takes it for one. Only the control plane writes
  one: a frame typed `sandbox_status` from the sandbox socket is dropped,
  never stored or broadcast, with the control plane's other own event
  types (`image_decision`, `shadow_egress_suppressed`); the sandbox-ws
  contract defines none of them, and its own types are stored as before.
  So is a sandbox frame without a type, or with a top-level `events`,
  `nextCursor` or `sandbox` key, none of which a sandbox-ws event has:
  broadcast raw, it would read like a `fetch_history` reply. A client takes
  a frame for that reply only when it has no `type`, which the reply never
  has (`additionalProperties: false`).
- Unchanged: `SubscribeRequest`, `SubscribedPayload`, `FetchHistoryRequest`.

### rest/v1/dtos.schema.json

- Added: `SessionActivity.escalation` (required, object or null: `id`, the
  escalated run's, and `since`), the workflow escalation still open under
  `awaiting.kind` `workflow_escalation`'s rule, reported whatever gate
  `awaiting` names first (technical plan §43.20): an escalation open beside
  a plan awaiting approval or a workflow step awaiting a decision is not in
  `awaiting`, and a client showing which workflow run a person should look
  at reads it here. A required property added to a platform-produced shape
  grades MINOR (row 3). The MCP status and wait tools' output schemas carry
  it (`internal/adapters/inbound/mcp/testdata/tools.golden.json`).
- Changed, description only: `EventsResponse` no longer claims to mirror
  `FetchHistoryResponse` exactly. It is that reply's `events` and
  `nextCursor`, read through the same query, and deliberately omits the
  `sandbox` row the WS reply now carries: that row is a WS concern, what
  keeps an open page's sandbox status current after a `sandbox_status`
  broadcast, and a REST caller reads the sandbox status from
  `GET /api/sessions/:id/status` (`SessionActivity.sandboxStatus`). The MCP
  transcript tool's output schema carries the new description. Its shape
  is unchanged.
- Unchanged: no route, no enum; no other `$def` changes.

## [1.15.0]

### rest/v1/dtos.schema.json

- Added: `ListDecisionInboxResponse.requiredChecksNotRead` (required
  boolean), true when the deployment's GitHub outbound is off: the inbox
  then reads no base branch's required checks (technical plan §21.2), so
  no pull request is shown as `ready_to_merge`. A configuration, stable
  across loads, never reported through `scmFetchFailed`; the Merge
  endpoint reads the requirements itself, with the acting person's own
  credential. A required property added to a platform-produced shape
  grades MINOR (row 3).
- Added: `DecisionInboxItem.mergeableIfRequiredChecksPass` (optional,
  boolean or null), true on an ordinary pull-request row when the only
  thing keeping it from a Merge click is that the inbox, with GitHub
  outbound off, does not read its base's required checks: every other
  criterion holds, the row stays `needs_review`, and a client offers
  Merge, whose endpoint reads the requirements and refuses with 409,
  naming the check, when one is unmet. False on every other pull-request
  row, null on a row that is not a pull request. A property added, not
  required, grades MINOR (row 2).
- Unchanged: no route, no enum, no other `$def`.

## [1.14.0]

### rest/v1/dtos.schema.json

- Added: `StopSessionToolRequest` (`sessionId`), the input of the new
  `narvi_stop_session` MCP tool (technical plan §43.22, row 183), whose twin
  is `POST /api/sessions/{sessionID}/stop`. Self-contained: it references no
  other `$def`. The tool sends the route no body, as a browser sends none. A
  new `$def` grades MINOR (row 32).
- Unchanged: the tool's output reuses `StopSessionResponse`, which holds no
  enum to publish open. The MCP tool list gains the tool
  (`internal/adapters/inbound/mcp/testdata/tools.golden.json`). No route
  changes.

## [1.13.0]

### rest/v1/dtos.schema.json

- Added: `StopSessionResponse`, the `202` body of the new stop route below
  (technical plan §3.3): `sessionId`, `requestedAt` (when this request
  was made -- a repeated request answers its own, later instant),
  `reachedSessionIds` (the session named first, then every session it
  started, recursively) and `openTurns` (the turns the request flagged to
  be cancelled, across every reached session). A new `$def` grades MINOR
  (row 32).

### controlplane/testdata/routes.golden

- Added: `POST /api/sessions/{sessionID}/stop` -- a person's request to
  stop a session and every session it started (technical plan §3.3). No
  body. `400` for a malformed id and `404` for a session that does not
  exist, as `GET /api/sessions/{sessionID}` answers; `403` unless
  `authz.ActionStopSession` admits the caller: admin and maintainer on any
  session, a member on their own or joined sessions except a pull
  request's review session, a viewer never.
  Answers `202 StopSessionResponse` once the request is written: turns open
  at that instant are cancelled -- a pending one at once, a running one
  through the sandbox's own `stop`, or once `StopGrace` (30s) has passed,
  also while its sandbox still delivers an earlier turn's push and pull
  request, which then pushes nothing for it -- and turns created later run
  normally. The stopped turn's sandbox is replaced only once such a
  delivery is over, for at most `MCPStatusDeliveryWindow` (10 minutes),
  and a turn created meanwhile waits for it. A route added, graded MINOR
  (row 41).

## [1.12.0]

### rest/v1/dtos.schema.json

- Added: five `$defs`, the inputs of the MCP tools that read, approve and
  reject plans, request a plan revision and send a prompt (technical plan
  §43.21, row 183). Each is self-contained, references no other `$def`,
  and names the REST route its tool is bridged to:
  - `ListPlansToolRequest` (`sessionId`), for `narvi_list_plans` over
    `GET /api/sessions/{sessionID}/plans`;
  - `ApprovePlanToolRequest` and `RejectPlanToolRequest` (`sessionId`,
    `planId`), for `narvi_approve_plan` and `narvi_reject_plan` over
    `POST /api/sessions/{sessionID}/plans/{planId}/approve` and `/reject`;
  - `RequestPlanRevisionToolRequest` (`sessionId`, `feedback`, optional
    `modelId` and `effort`), for `narvi_request_plan_revision` over
    `POST /api/sessions/{sessionID}/turns` with `planMode: true` and the
    feedback as the prompt;
  - `SendPromptToolRequest` (`sessionId`, `prompt`, optional `modelId` and
    `effort`), for `narvi_send_prompt` over the same route with
    `planMode: false`.

  A new `$def` grades MINOR (row 32). The feedback and the prompt are
  non-empty. Neither turn tool offers `planMode` or `attachmentIds`;
  adding an optional property to one later is MINOR (row 2).
- Unchanged: the tools' outputs reuse `ListPlansResponse`,
  `PlanActionResponse` and `CreateTurnResponse`, whose status enums are
  already open. The MCP tool list gains the five tools
  (`internal/adapters/inbound/mcp/testdata/tools.golden.json`). No route
  changes.

## [1.11.0]

### rest/v1/dtos.schema.json

- Added: `CreateSessionRequest.idempotencyKey`, optional, `format: uuid`
  (technical plan §43.8, row 183). A caller-chosen key, scoped to the
  authenticated user and kept with the session it created: the same key
  with the same request answers `200` with that session and starts
  nothing; the same key with a different request is refused `409`, and so
  is a key whose session was started the other way (by cookie or over
  MCP). Requests are compared by what they ask for, however their JSON is
  written. The key is 8-4-4-4-12 hexadecimal digits in either case, both
  cases one key; any other spelling is refused `400`. An optional property
  added to a client-to-platform shape grades MINOR (row 2). Absent, `POST
  /api/sessions` behaves exactly as before.
- Added: `CreateSessionToolRequest`, the input of the new
  `narvi_create_session` MCP tool, whose twin is `POST /api/sessions`. A
  new `$def` grades MINOR (row 32). It is self-contained and has no
  `spawnSource`: the server records `mcp` from the MCP grant, so a
  caller-supplied source is refused as an unknown argument.
  `idempotencyKey` is required there. `CreateSessionRequest`'s environment
  settings are not offered; adding one later is MINOR (row 2).
- Changed (description only, annotation-only PATCH):
  `CreateSessionRequest.spawnSource` says that the MCP tool sends `web`,
  the one value the route accepts, and that the server records `mcp`.
  The field stays closed with its four values.
- The MCP tool list gains `narvi_create_session`, whose output schema
  reuses `Session` (`internal/adapters/inbound/mcp/testdata/
  tools.golden.json`). No route changes.

## [1.10.0]

### rest/v1/dtos.schema.json

- Added: `mcp` to `Session.spawnSource` -- the source a session created
  over MCP records (technical plan §43.1, row 183), set by the server from
  the MCP grant the /mcp authentication attaches, never read from a
  request. Nothing records it yet: this release makes the value exist and
  be understood by every consumer in this repository before any path
  writes it. The enum has been listed in `openEnums` since 1.9.2, so the
  value added to this platform-to-client shape grades MINOR (row 11).
  Consumers already tolerate a value they do not recognise; the web UI now
  labels this one "MCP" instead of "other".
- Unchanged, deliberately: `CreateSessionRequest.spawnSource` keeps its
  four values. The REST create path refuses any caller-supplied source
  other than `web`; an MCP session's source is never the caller's to
  choose.
- Changed (description only, annotation-only PATCH):
  `CreateSessionRequest.spawnSource` no longer says it matches Postgres
  `session_spawn_source` exactly, which stopped being true once `mcp` was
  added. It now says the field is a closed subset of the sources: `POST
  /api/sessions` accepts only `web`, and `mcp` is set by the server, never
  read from a request.
- The MCP tool output schemas that carry a `Session` list the value among
  the open enum's `examples` (`internal/adapters/inbound/mcp/testdata/
  tools.golden.json`).

## [1.9.2]

### rest/v1/dtos.schema.json

- Changed: `Session.spawnSource` is now an open enum.
  `rest/v1/dtos.schema.json#/$defs/Session/properties/spawnSource` is
  added to `manifest.json`'s `openEnums`, and the field's description says
  so. Consumers MUST tolerate a `spawnSource` value they do not recognise.
  No value is added in this release: a following release adds `mcp`, the
  source a session created over MCP records (technical plan §43.1, row
  183). `openEnums` is read from the merge base, so that addition grades
  MINOR only once this entry is already on `main`. The description change
  is annotation-only (row 35, PATCH); the `openEnums` entry is not itself
  a graded finding.
- Unchanged, deliberately: `CreateSessionRequest.spawnSource` stays out of
  `openEnums`. It is client-to-platform, where an added enum value already
  grades MINOR (row 11), and the REST create path refuses any
  caller-supplied source other than `web`, so listing it would promise a
  tolerance the platform does not offer there.

## [1.9.1]

### rest/v1/dtos.schema.json

- Changed (descriptions only, annotation-only PATCH):
  `SessionActivity.activity` and `SessionActivity.settled` no longer state
  a limit about a pull request review session's own push: a review session
  is read-only and never pushes, so the limit no longer exists, and
  `activity` now says its turns are never followed by `delivering`.
  `SessionOutcome.reviewScope`'s `reviewed` value no longer gives a review
  session's own push as an example of a pull request it opened: it opens
  none. No field, type, enum value or requiredness changed.

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
  before reading the result again: 30 seconds while the result can still
  change on its own -- the session is not settled, or the review of a pull
  request it opened is still to come -- 60 once neither holds and a
  freshness was read live, 300 when nothing was either, as shipped).
  Carries no events. A `$defs` entry added, graded MINOR (row 32).
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

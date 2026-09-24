# Compatibility policy (§6.3)

`/contracts` is versioned as a bundle, independent of the Go module and of
`web/`. `contracts/VERSION` (kept equal to `contracts/package.json`'s own
`"version"` field, enforced by `contracts/contractstest`) is the single
number that moves; `contracts/CHANGELOG.md` explains why every time it
does. `make contracts-compat` (CI job `contracts-compat`, required on every
PR) computes the same classification a human reviewer would have to
compute by hand, and fails the build on anything MAJOR — this document is
the human-readable version of the closed rule table `tools/
contractscompat/compat` implements; disagreements should be resolved by
changing the code and this file together, in one PR.

## What is covered

- `rest/v1/dtos.schema.json` — every REST request/response/entity `$defs`
  entry, AND the REST route table itself
  (`controlplane/testdata/routes.golden`, only rows under `/api/`).
- `client-ws/v1/protocol.schema.json` — the browser↔control-plane
  WebSocket protocol (subscribe/fetch-history requests and their
  responses).
- `sandbox-ws/v1/commands.schema.json` and `sandbox-ws/v1/events.schema.json`
  — the control-plane↔sandbox-agent WebSocket protocol.
- `session-config/v1/session-config.schema.json` — the `NARVI_SESSION_CONFIG`
  document handed to a sandbox at boot.

## What is explicitly NOT covered

- Post-`subscribed` broadcast frames, and the element shapes of
  `SubscribedPayload.state/events/artifacts/participants`,
  `EventsResponse.events[]`, and `ArtifactsResponse.artifacts[]`. These are
  `additionalProperties: true` interiors, hand-built maps in
  `wshub/client.go` today — promoting them to a schema is follow-up work,
  not part of this policy yet.
- HTTP status codes and headers, and anything about timing (timeouts,
  retry intervals) — those live in `platform/timeouts.go` and this
  document's own sibling docs, not in a JSON Schema.
- Two known, pre-existing drift points, documented rather than fixed by
  this policy: (a) `PreviewIntentTemplateRequest`/`UpsertIntentTemplateRequest`
  are sent by the SPA but decoded by hand-written structs in
  `internal/adapters/inbound/httpapi/classifiertemplates.go`, not through
  the generated types; (b) four REST request bodies
  (`PostReviewVerdict`, `PostEpistemicOutcome`,
  `PostReleaseCompositionFindings`, `PostWorkflowStepOutcome`) are produced
  inside the sandbox by an LLM's tool calls from prompt text
  (`cmd/sandbox-agent/*toolprompt.go`), not by codegen — a schema change to
  one of these defs does not, by itself, guarantee the prompt was updated
  to match.

## Who this governs

- **`rest/v1/dtos.schema.json` and `client-ws/v1/protocol.schema.json`**
  are offered to third parties: an external client (Gatekeeper module,
  future public API consumer, a script hitting `/api/` directly) can
  depend on them, and a MAJOR change is a real breaking change for someone
  outside this repository.
- **`sandbox-ws/v1/*.schema.json` and `session-config/v1/session-config.
  schema.json` are governed by this same checker, but are NOT offered to
  third parties.** They exist purely to keep an in-flight sandbox (an
  older sandbox-agent baked into a warm pool image, or resumed from a
  snapshot) able to talk to a newer control plane, and vice versa, during
  a rolling deploy — real version skew, just contained entirely within
  this system's own two halves. A MAJOR change here still needs the
  versioned-sibling treatment below; it is simply never a "we broke a
  customer's integration" incident the way a REST MAJOR change would be.
- **REST addressing for a deliberate breaking change is an OPEN decision.**
  REST has no version segment in its own URLs even though the schema file
  is named `rest/v1/`. The first time a genuinely breaking REST change is
  needed, the owner must decide how a `rest/v2` schema maps onto the HTTP
  surface (a new URL prefix? a header? content negotiation?) before this
  policy's versioned-sibling procedure can be followed for REST — this
  document intentionally does not answer that yet.

## Direction vocabulary

- **P2C (platform-to-client)**: platform-produced, client-consumed
  (REST responses, `SubscribedPayload`/`FetchHistoryResponse`,
  sandbox-ws commands as seen by the agent, session-config, events as
  seen by the browser).
- **C2P (client-to-platform)**: client-produced, platform-consumed
  (REST `*Request` bodies, `SubscribeRequest`/`FetchHistoryRequest`,
  events as sent by the agent).
- **Both**: reachable from roots of both kinds (a REST helper `$defs`
  entry nested under both a `*Request` and a response/entity), or the
  whole file is inherently bidirectional (sandbox-ws events: agent→CP is
  C2P, CP→browser is P2C). A Both change is compatible only if compatible
  under **both** columns below.

Direction is assigned per `contracts/manifest.json` surface row:
`by-suffix` (a root `$defs` entry named `*Request` is C2P, everything else
is P2C, refined by the both-roots reachability rule above),
`platform-to-client`, `client-to-platform`, or `both` (the whole file is
fixed to that direction).

## Consumer obligations (why some changes are safe)

A change can only be MINOR/PATCH instead of MAJOR because BOTH sides of
this system already follow these rules — they are the precondition the
whole policy rests on, not just a suggestion:

- A consumer of a P→C shape (generated Go decoders, the SPA's generated TS
  types as used by application code) **must ignore unknown properties**
  and **must ignore an unrecognized union variant** (a new event/command
  type, a new `oneOf` branch) rather than fail. This is what makes adding
  a property or a union member MINOR instead of MAJOR.
- The platform **ignores unknown properties on C→P shapes** for the same
  reason, symmetrically.

If either of those stops being true for a given generated decoder, this
whole policy's severity table stops being accurate for that surface —
that would be its own separate incident, not a `/contracts` change.

## The rule table

See `tools/contractscompat/compat`'s own package documentation for the
authoritative, machine-checked version of this table (41 rows) plus the
closed keyword allowlist and the fail-closed conditions outside it. In
summary: a property/enum-value/union-variant/def **removed** or a
constraint **tightened** on a client-produced (C2P) shape is MAJOR (an old
client's request would now be rejected); the same change on a
platform-produced (P2C) shape is usually MINOR (an old client simply never
looks at the new absence) UNLESS it removes something a consumer might
already depend on (a property disappearing, a type narrowing under it,
etc. — see the table for the exact list). **Adding** something is the
mirror image. A change to `$schema`, or any keyword outside the closed
allowlist (`$schema, $id, $defs, $ref, title, description, type,
properties, required, additionalProperties, enum, const, oneOf, anyOf,
items, format, pattern, minimum, minLength, minItems, default`), makes the
checker refuse to classify at all (fail closed) rather than guess.

## Relaxations: `openEnums` and `status: retired`

Both relaxations are read from the **merge-base** copy of
`contracts/manifest.json`, never from the head (PR) copy:

- **`openEnums`** lists canonical dotted names (`Session.status`,
  `Plan.status`, ...) of string enums where adding a new value is treated
  as MINOR instead of MAJOR on the P2C column. A PR that both adds an enum
  value AND adds that enum to `openEnums` is still scored as if the enum
  were closed (MAJOR) — opening an enum and using the opening have to be
  two different PRs, so the second one isn't grading its own homework.
- **`status: retired`** on a manifest surface row is what lets that row's
  schema file be deleted without the removal being MAJOR (rule 37) — and
  it, too, has to already say `retired` in the PR's OWN base, meaning the
  retirement itself landed in an earlier PR.

### Day-one `openEnums`

The lifecycle status enums of platform-produced REST shapes backed by a
Postgres enum column, which this project expects to grow over time:
`Session.status`, `Plan.status`, `PlanActionResponse.status`,
`CreateTurnResponse.status`, `ShadowComparisonTurn.status`,
`Automation.status`, `AutomationRun.status`, `AutomationInvocation.status`,
`WorkflowRun.status`, `WorkflowStepRun.status`,
`WorkflowStepDecideResponse.runStatus`,
`WorkflowStepDecideResponse.stepRunStatus`. This list was chosen by the
implementing agent from the schema's own enum inventory and needs owner
confirmation (see this Step's PR description) — sandbox-ws's own
event-level enums (`Artifact.status`, `GitSync.status`,
`ExecutionComplete.outcome`, and similar) were deliberately left closed
for now, since that surface is only ever version-skew between this
system's own two halves, never a third party.

## Versioned siblings (the only way to make a breaking REST/protocol change)

A MAJOR change is never made in place on an existing `vN` file. Instead:

1. Add `contracts/<surface>/v(N+1)/<file>.schema.json` as a new file,
   alongside the old one (which keeps serving old clients/sandboxes).
2. Add a manifest.json row for it.
3. Wire it into `make contracts-generate` (the Makefile's per-file
   `go-jsonschema`/`json-schema-to-typescript` invocations),
   `contracts/scripts/generate-ts.mjs`, and `contracts/embed.go`'s
   `//go:embed` pattern.
4. Add a CHANGELOG.md entry and bump VERSION's major component.
5. Commit as `feat(contracts)!: ...` (or with a `BREAKING CHANGE:`
   footer) — this is the one case in this repository where that `!` is
   actually warranted for a `/contracts` change.

### Retiring an old version (three PRs, not one)

1. PR A: set the old surface's manifest row to `"status": "deprecated"`.
2. PR B: set it to `"status": "retired"`.
3. PR C: delete the schema file, its manifest row, and its codegen wiring.
   Only PR C's own base manifest already says `retired`, which is what
   lets `tools/contractscompat` accept the deletion as something other
   than a MAJOR row-37 violation.

## VERSION / CHANGELOG discipline (enforced by the checker itself)

If anything under `/contracts` or `controlplane/testdata/routes.golden`
changed at all: `contracts/VERSION` must be strictly greater than the
base's, the bump's own class (major/minor/patch) must be at least as
severe as the worst finding in the diff, `CHANGELOG.md`'s first `## 
[x.y.z]` heading must equal the new VERSION, and that section must carry a
`### <surface path>` subsection for every surface that actually changed.
If NOTHING changed, VERSION and the CHANGELOG's top heading must stay
exactly as they were — a version bump with no content change is exactly
as wrong as a content change with no version bump.

## Extending the checker

A change the closed keyword allowlist or the 41-row rule table doesn't
name makes `tools/contractscompat` fail closed with a message naming the
JSON Pointer and asking for a separate PR first. That PR should extend
`tools/contractscompat/compat`'s allowlist/rule table AND its own corpus
(`compat_test.go`'s table plus the coverage meta-test) together, so the
new case is verified in CI going forward, not just handled once by hand.

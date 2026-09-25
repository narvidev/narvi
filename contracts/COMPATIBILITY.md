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
  entry, AND the REST route table itself (the `/api/` rows of
  `controlplane/testdata/routes.golden`).
- The rest of `controlplane/testdata/routes.golden` — every route the
  control plane serves outside `/api/`. Each has a consumer outside the
  running build, so each row is graded exactly like an `/api/` row — see
  "Routes" below. The consumers, class by class:
  - `/mcp`, `/.well-known/oauth-protected-resource/mcp`,
    `/.well-known/oauth-authorization-server/oauth` and `/oauth/*` — the
    MCP surface and its authorization server, used by third-party MCP
    clients. The client calls `/oauth/token` and `/oauth/revoke` itself;
    `/oauth/authorize` and `/oauth/consent` are opened in the user's
    browser during the client's sign-in.
  - `/.well-known/openid-configuration` and `/.well-known/jwks.json` — the
    cloud-identity issuer's discovery documents, fetched with no
    credential by a cloud provider's token service during workload
    identity federation.
  - `/auth/*` — browser sign-in and account linking. Each
    `/auth/*/callback` row is a redirect URI registered with that OAuth or
    OIDC provider; `/auth/*/login` and `/auth/*/install` are opened in the
    browser;
    `/auth/capabilities` and `/auth/logout` are called by the SPA; and
    `/auth/identity-link/{nonce}` is a link posted in a chat or
    issue-tracker message and opened in a browser.
  - `/webhooks/*` — URLs registered with webhook senders: each provider's
    own `/webhooks/<provider>` rows (sub-paths included) with that
    provider, and `/webhooks/automations/{automationID}` with whatever
    external system an automation's owner handed the URL and its token to.
  - `GET /sessions/{sessionID}/ws` — one route carrying two protocols.
    With `?type=sandbox` it is the sandbox-agent's socket
    (`sandbox-ws/v1`); with `?type=client` it is the browser's socket
    (`client-ws/v1`), which "Who this governs" below offers to third
    parties. This row is therefore in the third-party class, not only the
    sandbox one.
  - Every other `/sessions/{sessionID}/*` row (credentials, config,
    snapshot, verdict, outcome and findings reports, uploads) —
    sandbox-agent callbacks, authenticated by the sandbox's bearer token
    and called by whichever sandbox-agent is running, an older one
    included during a rolling deploy.
  - `/health` — health probes.
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

## Permitted `oneOf`/`anyOf` shapes

`tools/contractscompat` does not attempt to grade an arbitrary `oneOf`/
`anyOf` shape. Three straight adversarial review rounds found a bypass in
that approach (a general shape the checker MODELED turned out to admit a
change nothing actually reviewed) — the checker instead whitelists
EXACTLY the two shapes the five files under `/contracts` actually use
today (verified by hand and by script against every `oneOf`/`anyOf` in
every file). A union that matches neither shape, on either side of the
diff, is FAIL-CLOSED, naming the pointer — there is no third, general
case.

- **Shape A** (`sandbox-ws/v1/commands.schema.json` and
  `events.schema.json`, each a root `oneOf`): every member is a `$ref` to
  a def whose own `type` is EXACTLY the bare string `"object"` — never an
  array that merely includes `"object"`, never absent — carrying its own
  `properties.type.const` discriminator, with every member's
  discriminator value distinct from every other member's.
- **Shape B** (`rest/v1/dtos.schema.json`'s `ReviewReadout.latestVerdict`
  `anyOf`): exactly one `$ref` to a pure object def (discriminator
  optional — there is nothing to discriminate between when there is only
  one non-null shape), plus an optional bare `{"type":"null"}` literal,
  written INLINE (never itself behind a `$ref`).

An inline discriminated member (the exact object shape rows 28/29/46 are
for, just spelled without a `$ref`), a `$ref` to a mixed- or absent-
`"type"` def, a scalar `$ref`/alias chain, or a second non-null object
member beyond shape B's own single slot all fail closed — none of them
is a shape any real file uses, so none of them is graded at all. Within
shape A, an added member's discriminator value must also be distinct
from every value BASE ever assigned (row 46) — pairing by `$ref` name
alone cannot catch a variant renamed while keeping (or colliding with)
another member's wire discriminator.

## The rule table

This is the authoritative version — `tools/contractscompat/compat`'s own
`ruleTable` (meta_test.go) pins the exact same rule-id set, and
`TestCompatibilityDocRuleIDsMatchCode` (compatibility_doc_test.go) fails
the build if this table and that code-side list ever diverge, so the two
cannot drift silently the way they did before this rewrite (C22).

P→C = platform-produced, client-consumed. C→P = client-produced,
platform-consumed. "Both" (see "Direction vocabulary" above) means
compatible only if compatible under **both** columns.

| # | Change | P→C | C→P |
|---|---|---|---|
| 1 | Property removed or renamed | MAJOR | MAJOR |
| 2 | Property added, not required | MINOR | MINOR |
| 3 | Property added and required | MINOR | MAJOR |
| 4 | Property moved into required | MINOR | MAJOR |
| 5 | Property removed from required | MAJOR | MINOR |
| 6 | `type` changed outright, or the `type` keyword itself added/removed | MAJOR | MAJOR |
| 7 | `type` widened (union gains a non-null member) | MAJOR | MINOR |
| 8 | `type` narrowed (union loses a non-null member, keyword stays present both sides) | MINOR | MAJOR |
| 9 | `null` added to `type`, including shape B's own bare `{"type":"null"}` `anyOf` member added ("Permitted `oneOf`/`anyOf` shapes" above) | MAJOR | MINOR |
| 10 | `null` removed from `type`, including shape B's own bare `{"type":"null"}` member removed | MINOR | MAJOR |
| 11 | enum value added | MAJOR, unless the enum is listed in the MERGE-BASE manifest's `openEnums` (then MINOR) | MINOR |
| 12 | enum value removed | MINOR | MAJOR |
| 13 | `enum` keyword added / removed | added MINOR / removed MAJOR | added MAJOR / removed MINOR |
| 14 | `const` changed/added/removed | MAJOR | MAJOR |
| 15 | `format` added | MINOR | MAJOR |
| 16 | `format` removed | MAJOR | MINOR |
| 17 | `format` changed | MAJOR | MAJOR |
| 18 | `minimum`/`minLength`/`minItems` raised (or added, floor raised from none) | MINOR | MAJOR |
| 19 | `minimum`/`minLength`/`minItems` lowered (or removed, floor lowered to none) | MAJOR | MINOR |
| 20 | `pattern` added | MINOR | MAJOR |
| 21 | `pattern` removed | MAJOR | MINOR |
| 22 | `pattern` changed | MAJOR | MAJOR |
| 23 | `additionalProperties` `false`→`true` or `false`→schema | MINOR | MINOR |
| 24 | `additionalProperties` `true`→`false` or schema→`false` | MAJOR | MAJOR |
| 25 | `additionalProperties` `true`→schema | MAJOR | MAJOR |
| 26 | `additionalProperties` schema on both sides, content differs | recurse, same direction | recurse, same direction |
| 27 | `$ref` retargeted (siblings on the referencing node are diffed too, as part of the same comparison; also covers shape B's own single object slot retargeted to a different `$ref`) | compare dereferenced+merged schemas under rows 1-26/42; MAJOR if the old target def no longer exists (row 31) | same |
| 28 | `oneOf`/`anyOf` DISCRIMINATED variant added (shape A: a member that is a `$ref` to a pure object def carrying its own `properties.type.const`, distinct from every other member's -- keyword already present both sides; a variant whose discriminator value reuses one BASE already assigned is row 46 instead, not this row) | MINOR | MINOR |
| 29 | `oneOf`/`anyOf` DISCRIMINATED variant removed (same scope as row 28) | MAJOR | MAJOR |
| 30 | `oneOf`/`anyOf` variant changed (paired by the `$ref` target's own NAME under shape A, or shape B's single object slot when its `$ref` name is unchanged; anything not matching a permitted shape at all is FAIL-CLOSED, never paired) | recurse | recurse |
| 31 | `$defs` entry removed or renamed | MAJOR | MAJOR |
| 32 | `$defs` entry added | MINOR | MINOR |
| 33 | `default` added/changed/removed | MAJOR | MAJOR |
| 34 | root `title` or root `$id` changed | MAJOR | MAJOR |
| 35 | `description` changed/added/removed | PATCH | PATCH |
| 36 | root `$schema` changed | FAIL-CLOSED | FAIL-CLOSED |
| 37 | schema file removed/renamed, unless the BASE manifest already marks it `retired` | MAJOR (also forces a MAJOR version bump, §4) | same |
| 38 | schema file added under a new manifest row | MINOR (also forces a MAJOR version bump, §4 — a new vN sibling is how a breaking change is made) | same |
| 39 | `items` changed; `items` presence itself added/removed | recurse; presence change is MAJOR | same |
| 40 | route removed/renamed (a path-parameter rename included), or method changed, on any row of `routes.golden`, under `/api/` or not ("Routes" below) | MAJOR | n/a |
| 41 | route added, on any row of `routes.golden`, under `/api/` or not | MINOR | n/a |
| 42 | `additionalProperties` schema→`true`/absent (permissive) — distinct from row 23's `false`→anything "unlock" | MAJOR | MINOR |
| 43 | `oneOf`/`anyOf` keyword itself added or removed (not a member of an already-existing union — that's rows 28/29) | MAJOR | MAJOR |
| 44 | a surface's `direction` changed between the base and head `manifest.json` | MAJOR | MAJOR |
| 45 | `goJSONSchema` changed | MAJOR | MAJOR |
| 46 | `oneOf`/`anyOf` shape-A variant added whose `properties.type.const` discriminator value was already assigned to a DIFFERENT member in BASE (round 5, G2) — an in-flight consumer dispatches on the wire value, not the `$ref`'s own `$defs` name, so this is unsafe even when the member that used to own the value was removed in the same diff | MAJOR | MAJOR |

Also FAIL-CLOSED, outside the numbered table: any keyword not in the
closed allowlist below; a `title` on a non-root sub-schema; a non-local
`$ref`; a `type` array that isn't a known JSON Schema type vocabulary, or
that has a duplicate; an unpairable `oneOf`/`anyOf` member (including any
union that does not match one of the two permitted shapes above, on
either side of the diff — "Permitted `oneOf`/`anyOf` shapes"); a `required`
name with no matching `properties` entry on that same side (rows 3/4
still apply normally to a required name that DOES have a matching
property, even one only declared via an unchanged `$ref` target); a
`routes.golden` line, on either side of the diff, that is not exactly
`METHOD /path` or that repeats another line (`fc-routes-line`, "Routes"
below); and any
keyword — anywhere in the closed allowlist — present at a schema node
that no rule handler above actually consumed (the checker's own internal
exhaustiveness assertion; this should never fire in practice, since every
allowlisted keyword has a handler, but it is there as a structural
backstop rather than a promise kept by convention).

The closed keyword allowlist (schema positions only): `$schema, $id,
$defs, $ref, title, description, type, properties, required,
additionalProperties, enum, const, oneOf, anyOf, items, format, pattern,
minimum, minLength, minItems, default, goJSONSchema`. `$schema`, `$id`,
`title`, and `$defs` are additionally restricted to the document ROOT —
finding any of them on a nested sub-schema is itself FAIL-CLOSED.
`goJSONSchema` is go-jsonschema's own vendor extension controlling the
exact Go type generated for one property; changing it is row 45 (MAJOR),
not an annotation, because it changes what the generated Go decoder
accepts (C19) — unlike `description`, which is a genuine annotation
(row 35, PATCH).

In summary: a property/enum-value/union-variant/def **removed** or a
constraint **tightened** on a client-produced (C2P) shape is MAJOR (an old
client's request would now be rejected); the same change on a
platform-produced (P2C) shape is usually MINOR (an old client simply never
looks at the new absence) UNLESS it removes something a consumer might
already depend on (a property disappearing, a type narrowing under it,
etc. — see the table for the exact list). **Adding** something is the
mirror image.

## Routes: `controlplane/testdata/routes.golden`

The route table is graded line by line — every line, under `/api/` or
not ("What is covered" above says why):

- A line in head but not in base is row 41, MINOR: a route added.
- A line in base but not in head is row 40, MAJOR: a route removed. A
  renamed path, a renamed path parameter (`{sessionID}` → `{id}`) and a
  changed method each read as the old line removed plus a new one added,
  so all three are MAJOR. That is conservative for a parameter rename,
  which changes nothing on the wire, but at this line grain the checker
  cannot tell one from a real rename.
- Like any MAJOR finding, a removed or changed route fails the check
  whatever the VERSION bump. There is no sanctioned path for one yet: the
  versioned-sibling procedure below is for schema files, and REST
  addressing for a breaking change is still an open decision ("Who this
  governs"). Removing or changing a route needs an owner decision, and a
  checker change, first.
- Every line must be exactly `METHOD /path`: an upper-case method, one
  space, and a path starting with `/` with no whitespace in it. A line
  that is not — a blank line, a trailing field, a CRLF ending, a
  lower-case method — or that repeats another line, on either side of
  the diff, is FAIL-CLOSED (`fc-routes-line`), naming the side and the
  line number, and no route diff is reported for that run. The file is
  the control-plane binary's own `routes` output, so the fix is to
  regenerate it, never to edit it by hand.
- Any byte change to the file, even one that changes no route (a
  reorder), counts as a change for the VERSION/CHANGELOG discipline
  below.

Until this section was written the checker read only the `/api/` rows and
skipped every other line, so a route outside `/api/` could be added,
removed or re-methoded with no VERSION bump and no CHANGELOG entry.
CHANGELOG entries up to 1.4.1 that call such a route "not graded"
describe that old behaviour, not this policy.

## Relaxations: `openEnums` and `status: retired`

Both relaxations are read from the **merge-base** copy of
`contracts/manifest.json`, never from the head (PR) copy:

- **`openEnums`** lists each open string enum's own schema node as
  `"<surface path>#<json pointer>"` — e.g. `"rest/v1/dtos.schema.json#/
  $defs/Session/properties/status"` — the surface half naming the exact
  `manifest.json` "path" the entry applies to, and the pointer half the
  EXACT JSON Pointer of that enum's own schema node within THAT file —
  never a name derived by stripping `$defs`/`properties` segments out of
  the pointer, which would let a property literally named `properties`
  (or `$defs`) inherit an unrelated entry's relaxation. An entry only
  ever relaxes the surface it names (round 5 review, G3): a def sharing a
  name across two different files — `rest/v1/dtos.schema.json`'s own
  `Automation` and an unrelated `Automation` def some other surface
  happens to also declare, say — does NOT share a relaxation just because
  one of them is open; an unqualified entry, or one naming a surface not
  in this manifest, is a manifest authoring error this checker fails
  closed on (`fc-openenums-scope`) rather than silently doing nothing (or,
  before this fix, silently applying everywhere). Adding a new value to a
  pointer on this list is treated as MINOR instead of MAJOR on the P2C
  column. A PR that both adds an enum value AND adds that enum's pointer
  to `openEnums` is still scored as if the enum were closed (MAJOR) —
  opening an enum and using the opening have to be two different PRs, so
  the second one isn't grading its own homework.
- **`status: retired`** on a manifest surface row is what lets that row's
  schema file be deleted without the removal being MAJOR (rule 37) — and
  it, too, has to already say `retired` in the PR's OWN base, meaning the
  retirement itself landed in an earlier PR.

### Day-one `openEnums`

The lifecycle status enums of platform-produced REST shapes backed by a
Postgres enum column, which this project expects to grow over time (named
here by field, for readability — `contracts/manifest.json` itself stores
each entry qualified for `rest/v1/dtos.schema.json`, as its own exact
JSON Pointer within that file, per the rule above):
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

### Genesis mode: the first PR to add `contracts/manifest.json`

`openEnums` and `status: retired` are read from the merge-base manifest
precisely so a PR can never grade its own relaxation — but the very first
PR to add `contracts/manifest.json` (and any later PR whose merge-base
still predates that landing on `main`) has no merge-base manifest at all.
`tools/contractscompat` calls this **genesis mode**: it substitutes
HEAD's own manifest as a stand-in base so the surface set still lines up,
but neutralizes `openEnums` and `retired` outright — neither relaxation
is honored in genesis mode, full stop.

Per-surface **direction** (`by-suffix` / `platform-to-client` /
`client-to-platform` / `both`) is a different case: unlike the two
relaxations above, direction has no merge-base fallback to read in
genesis mode — there is no OLD direction to compare against, and every
surface still needs one to be classified at all. So in genesis mode,
direction is read from HEAD's own manifest, exactly as it always is for a
brand-new surface. This is **not verified by the tool** — nothing checks
that a genesis-mode PR's claimed directions match how those surfaces are
actually used. `contractscompat` prints an explicit `GENESIS MODE` notice
naming every surface's direction as taken from HEAD when this fires, so
the PR's human reviewer knows to check each one by hand (does
`rest/v1/dtos.schema.json` really split `*Request` shapes as
client-to-platform and everything else as platform-to-client? does a
fixed-direction file's claimed direction match how it's actually used?)
instead of assuming the tool already did.

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
`### <surface path>` subsection for every surface that actually
changed — including a surface whose only change is its `manifest.json`
row (direction, status) or a new/removed manifest row, not just a schema
content diff. If NOTHING changed, VERSION and the CHANGELOG's top heading
must stay exactly as they were — a version bump with no content change is
exactly as wrong as a content change with no version bump.

`controlplane/testdata/routes.golden` is one surface here, under that
path: any byte of it changing — a route under `/api/` or outside it, or
no route at all — needs a `### controlplane/testdata/routes.golden`
subsection, and a bump at least as large as the route grades demand
(MINOR for an added route; a removed or changed one is MAJOR and fails
the check regardless, "Routes" above).

A schema file being added (row 38) or removed (row 37) always requires
**at least a MAJOR bump**, regardless of that row's own graded
compatibility severity (row 38 is MINOR — a new file's mere existence
breaks nobody) — the MAJOR requirement here is the versioned-sibling/
retirement DISCIPLINE from the section above, not a claim that adding a
file is itself a wire-compatibility break.

## Extending the checker

A change the closed keyword allowlist or the 46-row rule table doesn't
name makes `tools/contractscompat` fail closed with a message naming the
JSON Pointer and asking for a separate PR first. That PR should extend
`tools/contractscompat/compat`'s allowlist/rule table AND its own corpus
(`compat_test.go`'s table plus the coverage meta-test) together, so the
new case is verified in CI going forward, not just handled once by hand.

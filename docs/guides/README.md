# Per-surface user guides

One file per ingress surface — [web](web.md), [Slack](slack.md),
[Linear](linear.md), [GitHub](github.md) — each documenting what that
surface **accepts**, and, just as importantly, its **honest negatives**:
what it silently ignores, what it refuses, and what it does not support at
all. Shipped behavior only. §10-P6's own framing is the reason this
directory exists at all: a hand-maintained prose guide is just a copy of
the code's behavior with no mechanism keeping it in sync, which drifts
into aspirational text the instant the code moves and nobody remembers to
update the prose. `internal/ops`'s `TestNoGuideDrift` (`go test
./internal/ops/...`, part of `make test`) is that mechanism.

## The mechanism

Every accepted command documented in a per-surface guide is followed by a
machine-readable ```` ```json narvi-command ```` fenced block, e.g.:

````
```json narvi-command
{"name": "Create a session", "route": "POST /api/sessions"}
```
````

or, for a command whose real implementation is a §18.4 classifier
routing-decision outcome rather than a literal HTTP route:

````
```json narvi-command
{"name": "PR mention is always routed as a review", "classifier": {"surface": "github", "target": "review"}}
```
````

`internal/ops.LoadGuides` parses every block in every guide file (except
this README, which is prose only); `internal/ops.CheckGuideDrift` checks
each one against:

- **`route`** — a real, currently-registered chi route, scanned directly
  out of `cmd/control-plane/main.go`'s own router wiring via
  `internal/ops.ScanRegisteredRoutes` (a `go/ast` walk, the exact same
  mechanism `ScanRegisteredInstruments` already uses to keep
  `deploy/observability/{dashboards,alerts}` honest — see
  `internal/ops/doc.go`). Written as `"METHOD /path"`, exactly matching
  the joined path a nested `router.Route("/prefix", func(r chi.Router) {
  r.Get("/sub", ...) })` group actually serves.
- **`classifier`** — a real §18.4 routing-decision shape: `surface` must
  be a genuine `sessions.spawn_source` value, and `target`/`mode`/`source`
  (each optional) must, when present, be genuine values
  `internal/domain/intent`'s own exported constants actually define —
  scanned via `internal/ops.ScanIntentVocabulary`.

`internal/ops.TestNoGuideDrift` wires this into `go test ./...` against
the real repo tree — no separate CI job, no way to merge a guide that
documents a route or a routing outcome the code doesn't actually have.

A malformed file — an unterminated code fence, invalid JSON inside one, a
command naming neither or both of `route`/`classifier`, a file with no
top-level heading or zero command blocks — **fails the check**, the same
"never silently skipped" discipline `LoadDashboards`/`LoadAlerts` already
apply to `deploy/observability/*.json` (`internal/ops/schema.go`).

## What this check cannot catch

Stated plainly, mirroring `internal/ops/doc.go`'s own identical caveat for
the dashboards/alerts twin of this mechanism: **it validates names, not
semantics.**

- A route that still exists but no longer behaves the way the guide's
  surrounding prose describes (a handler rewritten to reject a field it
  used to accept, say) passes the check untouched — only a *renamed or
  removed* route is caught.
- A `classifier` block's `surface`/`target`/`mode`/`source` values are
  each checked for existing INDEPENDENTLY, never as a combination that the
  code actually produces together for a specific input. Nothing stops a
  guide from claiming a combination the real code path never reaches (the
  web surface never calls the §18.4 ROUTING classifier — `ports.
  IntentClassifier.Classify`/`ClassifyAndRecord`, the one whose Target
  vocabulary is `build`/`review` — at session-creation time, for
  instance: see [web.md](web.md)'s own "Sessions" section, not "Plan
  mode") as long as each individual field name is real. **Precise wording
  matters here**, confirmed the hard way (an earlier draft of this exact
  sentence said "the web surface never calls the LLM classifier at all",
  full stop — false: `POST /api/sessions/{sessionID}/turns` invokes a
  SECOND, structurally different classifier, `ClassifyPlanFollowup`
  (Step 64, §23.1), whenever a turn is added to a session with a plan
  `awaiting_approval` — see [web.md](web.md)'s own "Turns" section and
  [slack.md](slack.md)'s own "Plan mode" section for what that call
  actually does). "The web surface never calls the LLM classifier" is
  the exact shape of claim this whole document is about: true of one
  specific classifier at one specific decision point, false as a blanket
  statement about "the LLM classifier" in general — and nothing in this
  mechanism would have caught the overgeneralized version, because
  neither classifier is a `route` and this sentence isn't inside a
  `narvi-command` block at all. It is unchecked prose making exactly the
  kind of "never"/"only" claim the rest of this section is about.
- Nothing checks that the *prose* around a command block accurately
  describes what the route actually does with its request body, its
  response shape, or its authorization rules — only that the route
  exists. A reviewer still has to read the diff.
- **This list does not claim to be complete, and the claim is the thing
  that keeps breaking.** An earlier draft asserted a complete accounting
  of the routes outside every guide's scope and had dropped five, one
  pair of them human-facing and in no guide at all. It was corrected, the
  corrected version asserted completeness in turn, and a later phase
  audit found it short by twenty routes — twelve of them a single phase's
  new API. A hand-maintained inventory that declares itself complete goes
  stale on the next Step that adds a route, and says nothing while it
  does.

  What IS enforced lives in `internal/ops`: `TestNoGuideDrift` scans the
  real route wiring and fails when a guide documents a route the code
  does not implement. That guard runs the direction that matters — no
  guide may lie about a route. The reverse direction, every route being
  either documented or deliberately excluded, is not enforced, and this
  list is a reading aid for it rather than a guarantee.

  The categories below are the reasons a route is deliberately outside a
  guide, and remain accurate as reasons even when the enumeration lags:

  1. **Per-automation, admin-configured inbound trigger** —
     `POST /webhooks/automations/{automationID}` — not one of the four
     canonical `sessions.spawn_source` surfaces this Step covers.
  2. **Sandbox-agent-to-control-plane bearer-authenticated routes** under
     `/sessions/{sessionID}/...`: `scm-credentials`,
     `provider-credentials`, `sandbox-secrets`, `opencode-config`,
     `cloud-identity-token`, `cloud-identity-config`, `snapshot`,
     `review/verdict`, `workflow/step-outcome`, `turn/epistemic-outcome`,
     and the bearer variant of `uploads` — machine-to-machine wiring a
     human never calls directly, not a "surface" in this guide's sense.
     **`GET /sessions/{sessionID}/ws` (the live-stream WebSocket) is
     deliberately NOT in this list** — an earlier draft put it here too,
     and that was itself a confirmed review finding: this one route's
     own handler (`wshub.NewHandler`) dispatches on its own `?type=`
     query param to one of TWO structurally different handshakes —
     `?type=sandbox` (bearer-authenticated, machine-to-machine, genuinely
     out of scope, same as the routes above) and `?type=client`
     (ws-token-authenticated, the real thing a signed-in human's browser
     opens after minting a token via `POST
     /api/sessions/{sessionID}/ws-token`). [web.md](web.md) documents
     exactly that client half as an ordinary user-facing command, which
     is correct — a single chi route can be genuinely dual-purpose, and
     when it is, "out of scope" has to be scoped to the specific
     half/query-param branch that's actually machine-to-machine, not to
     the route string as a whole.
  3. **Infrastructure/federation routes with no `sessions.spawn_source`
     concept at all**: `GET /health` (liveness/readiness), and
     `GET /.well-known/openid-configuration` /
     `GET /.well-known/jwks.json` (§27.3's cloud-identity OIDC
     federation discovery, deliberately unauthenticated — see their own
     doc comment in `cmd/control-plane/main.go` for why). None of the
     three represents any actor — human or machine — starting or
     continuing a session; there is no sense in which any per-surface
     guide's "what does this surface accept" question applies to them at
     all.

  Every route in categories 1–3 is a real, working route — none of this
  is a gap in the *check*, which never claims to enumerate every route in
  the binary, only to validate the ones a guide actually cites. The
  now-fixed gap was specifically this section's own prose claiming
  completeness while omitting five of them.

## Prose is not machine-verified

Everything in a per-surface guide sits in one of two trust classes, and
it matters which:

- **Inside a `narvi-command` block**: `route`/`classifier` values are
  checked on every `go test ./...` run against the real repo tree
  (`TestNoGuideDrift`, above). A wrong one fails CI. That is a
  guarantee.
- **Everything else — every sentence of surrounding prose, and
  especially every "Negative"/"Honest negative" section** — is an
  author's claim, not a guarantee. Nothing re-derives it from source on
  every run; it is exactly as reliable as the last person who read the
  code carefully enough to write it down, and exactly as stale as
  however long it's been since that code last changed underneath it.
  Every one of Step 78's own confirmed adversarial-review findings
  (github.md's event-type/text-command negatives, slack.md's
  awaiting-approval and DM negatives, and this file's own classifier/
  routes-accounting claims — all fixed above) was exactly this: free
  prose, most of it a *negative* ("X never happens"), asserting
  something the code contradicted, in precisely the part of a guide
  `CheckGuideDrift` cannot see. A negative is the more dangerous
  direction to get wrong — it's what an operator's threat model and
  webhook-subscription decisions are built on — and it's exactly as
  unchecked as any other sentence here.

Read a guide's prose as "true as of the last careful read," not as "CI
would have caught it if this were wrong." Only the fenced blocks carry
that stronger guarantee.

## Exhaustiveness claims: considered extending the check, didn't

Every defect Step 78's adversarial review confirmed shared one shape:
an **exhaustiveness claim** in prose — "only two", "either X or nothing
at all", "never", "the complete list" — that turned out to have a real
counterexample in the code. That's a narrower, more dangerous category
than ordinary prose drift (the previous section's concern): it's not
just "unverified", it's a specific *kind* of claim a reader relies on
precisely because it sounds closed and total.

**Considered:** extending `CheckGuideDrift` (or a sibling scanner,
alongside `ScanRegisteredRoutes`/`ScanIntentVocabulary`) to bind an
"exhaustive list" claim to a real closed set in source, the same way a
single `route` is bound to a single registration today.

**Rejected, for a reason specific to what made these 9 findings
different from a route rename.** `ScanRegisteredRoutes` and
`ScanIntentVocabulary` both work because each corresponds to exactly
ONE uniform, purely syntactic AST shape that means the same thing
everywhere it appears — a `router.<Method>("/path", ...)` call, or a
top-level exported `const` declaration. Existence of that shape IS the
fact being checked; no interpretation is needed. The exhaustiveness
claims this batch fixed are not that:

- github.md's "only two `X-GitHub-Event` values ever start or continue
  a review session" was wrong because the real dispatch logic spans
  TWO files in structurally different shapes — a `switch eventType`
  in `payload.go`, and a sequence of `if eventType == ... && cfg.Foo !=
  nil && action == "..."` guards in `handler.go` — where the second
  shape depends on a config field's runtime nil-ness that no AST walk
  evaluates. A scanner that only reads switch-case literals would have
  caught THIS finding's "3rd event type is missing" half, but not its
  "2 more commands are dispatched before the switch even runs" half —
  those aren't cases of any switch at all.
- The Slack "DMing the bot starts a session" claim and the "an
  awaiting-approval reply never dispatches" claim (both fixed above) are
  each a claim about which BRANCH of an `if`/gate is taken for a given
  input — real control-flow reachability, not name existence. Binding
  that mechanically would mean an AST walk that understands conditional
  logic well enough to prove reachability, which is a fundamentally
  different (and much harder, much more fragile-to-refactors) tool than
  "does this identifier appear in a registration call."
- The alternative — hand-maintain a declared "this is the complete set"
  list in a new JSON field, checked only for each individual member's
  existence (exactly like `classifier` fields are today, per the
  INDEPENDENTLY-checked caveat above) — provides no real completeness
  guarantee at all: a THIRD real event type could be added to the
  dispatch logic and the declared "complete" list would drift again,
  silently, the exact failure mode this whole mechanism exists to
  prevent. It would just move the aspirational claim from Markdown
  prose into a Go slice literal, still hand-maintained, still capable of
  going stale the moment someone adds a new case and forgets to touch
  the guide.

This is the same call Step 77's own fix round made for extending this
check's twin (`ScanRegisteredInstruments`) to runbook log attributes,
and for the same underlying reason: a check that validates names, not
semantics, stays sound and low-maintenance precisely by refusing to grow
into a check that requires tracing control flow to be truthful. Extending
it here would trade a small, reliable guarantee ("this identifier is
real") for the appearance of a much bigger one ("this list is complete")
that the mechanism cannot actually deliver.

**What's fixed instead, within the existing mechanism's real scope:** the
plan-mode promotion behavior above turns on a genuine `classifier`-shaped
value (`internal/domain/intent.TargetAmend` is a real, scanned constant),
and is now documented as ordinary prose cross-referencing the shared
`createTurnLocked` mechanism in both [slack.md](slack.md) and
[web.md](web.md), consistently, so the same underlying fact can't drift
apart between the two guides that both describe it. The routes-
accounting gap and the `ws` endpoint miscategorization above are both
fixed by making the affected prose itself correct and, for the Linear
OAuth pair, by moving it out of unchecked "out of scope" prose entirely
and into a real, checked `narvi-command` block in [linear.md](linear.md)
— the bounded fix the section above recommends: when a claim CAN be
expressed as a route or classifier binding, express it that way, rather
than leaving it as a sentence a future editor has to remember to keep
true by hand.

## Mutation-testing this check

Verified by hand as part of Step 78 (§10-P6), reverted byte-identical
afterward — see `internal/ops/guidedrift_test.go`'s own
`TestNoGuideDrift` doc comment and the Step's own PR description for the
exact mutations and test names:

1. Documenting a command whose `route` matches no real endpoint makes
   `TestNoGuideDrift` fail.
2. Renaming a real route in `cmd/control-plane/main.go` without updating
   the guide that documents it makes `TestNoGuideDrift` fail identically.
3. Malforming a guide file (breaking its embedded JSON, or removing a
   closing fence) makes `internal/ops.LoadGuides` itself fail — the test
   errors out rather than silently treating that file as empty.

## Two rules, one of which nothing currently enforces

**Shipped behavior only, and this stays.** A proposal from a documentation-gap
review was to let a guide describe planned behavior behind a marker — the
`[planned §N]` model Step 165 uses for `docs/FOUNDATIONS.md`. That model is
right there and wrong here, and the difference is who is reading. FOUNDATIONS
exists to state what this system *is*, where a planned item is information.
These guides are read by someone trying to do something **now**: a
planned-behavior entry is an instruction that fails, and the reader has no way
to tell which half of the page applies to them. A Step existing does not make
a feature available, and a guide is the last place a reader should have to
draw that distinction for themselves. So: no markers, no aspirational section,
no "coming soon". A capability appears here when it ships and not before —
which is also why the rule above is stated as an invariant and not a
preference. Adding the marker would weaken a property that currently holds.

**The drift check only runs one way, and the other way is the one that
happens.** `TestNoGuideDrift` fails a guide documenting a command the code does
not implement. It cannot fail a guide that **omits** a command the code does
implement. Omission is the realistic failure: shipping a capability and
updating its guide are two acts, usually by two people, at two different
times, and only one of them is enforced. A check that catches aspirational
text, living in a directory whose stated risk is aspirational text, reads as
covering the subject; it covers half of it, and the visible half.

Step 178 is where the missing direction is filed, including the reason it is
harder than its sibling: nothing in the router knows which routes are meant
for a person. A webhook receiver, a health endpoint and a BFF route the web
app calls are all real routes that belong in no user guide, so the check needs
an explicit list of what is deliberately undocumented — and that list is a
claim someone makes, never a default.

## Which lot owes which guide

A capability that ships without its guide entry is the omission above, made
concrete. Each of these names the guide it must update as part of its own
work, not afterwards:

| Lot | Guide |
|---|---|
| Ticket decomposition into a PR train, and `Stop` across it | `linear.md` — it is the surface the train is driven from |
| Automations dispatching on real webhook events, including check and status events | `github.md`, and `web.md` for where an automation is configured |
| The review's own result surface and human acceptance of a verdict | `github.md` for the check and the review, `web.md` for acceptance |
| Snapshot restore and what a restored sandbox carries | `web.md` |
| Upload and capture limits, and anything reclaiming quota | `web.md` |
| An external client connection, if one is ever adopted | a new guide file — this one is a connection procedure, not a surface's command set |

The last row is deliberately conditional: `docs/DECISIONS.md` holds that
decision, and naming the guide here does not take it.

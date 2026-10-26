# Linear guide

## Connecting a workspace

Before any of the ingress below can fire, an admin has to connect a
Linear workspace to this deployment — a human, browser-driven OAuth
flow, not machine-to-machine wiring, so it belongs in this guide rather
than [README.md](README.md#what-this-check-cannot-catch)'s
machine-to-machine exclusions:

```json narvi-command
{"name": "Start connecting a Linear workspace (OAuth)", "route": "GET /auth/linear/install"}
```

```json narvi-command
{"name": "Linear workspace OAuth callback (completes the connection)", "route": "GET /auth/linear/callback"}
```

**Negative.** Both routes require an already-signed-in Narvi user
(`auth.Middleware`) AND `domain/authz.ActionManageIntegrations`
(admin-only) — a maintainer or below gets `403`, and there is no
self-service way for a non-admin to connect a workspace.

## Everything else: the webhook

Everything Linear sends Narvi *after* a workspace is connected arrives at
one route — Linear's own `AgentSessionEvent` webhook, both a brand-new
agent session and every follow-up message on it:

```json narvi-command
{"name": "Linear AgentSessionEvent webhook", "route": "POST /webhooks/linear"}
```

Signature-verified (`Linear-Signature`, hex HMAC-SHA256 over the raw
body) and freshness-checked against `platform.Timeouts.
LinearWebhookTimestampWindow` (60s — Linear's own explicit
recommendation, deliberately tighter than the generic 5-minute window
[slack.md](slack.md)'s webhook uses) before anything else happens.
Deduplicated by Linear's own `Linear-Delivery` header; a redelivery is
acknowledged without being reprocessed. Only the `AgentSessionEvent`
category is ever acted on — payload's own `action` is then switched on:
`created` and `prompted` are handled; anything else is logged and
acknowledged as a no-op.

## Starting a session

Assign or mention Narvi's own Linear app on an issue to fire a `created`
event, starting a brand-new session (`spawnSource: "linear"`).

```json narvi-command
{"name": "Assign/mention the Narvi agent on a Linear issue to start a session (shadow-classified, never LLM-routed in a way that changes behavior)", "classifier": {"surface": "linear", "source": "classifier"}}
```

Narvi acknowledges within Linear's own required 10-second window with a
single `AgentActivity` "thought" — synchronous, not queued or retried,
mirroring [slack.md](slack.md)'s own in-thread ack precedent exactly.

**What the agent is told.** A delegation's issue text and other people's
comments reach the agent as quoted data; only your own request is an
instruction. When you mention Narvi in a comment, that comment is your
request — matched to you by your Linear user id, never by your name — and
the issue's title, description and the rest of its discussion follow it,
quoted as context. Delegating an issue without a comment gives the agent
no request of yours: it is asked to work the issue as described, with the
issue's text quoted the same way.

**Negative — same shadow-classification caveat as every other surface.**
A `created` event's own initiating text IS classified and recorded via
§18.4 — but §18.5's shadow gate has no production consumer yet, so the
recorded `target`/`mode` never changes what actually happens (see
[slack.md](slack.md)'s identical section for the full explanation). A
Linear-created session is always an ordinary build-mode session unless a
later reply's own `revise:` prefix or a plan verdict changes it.

**Difference from Slack — no missing-default-repo failure mode.** Unlike
Slack's optional `NARVI_SLACK_DEFAULT_REPO_NAME`/`URL`,
`NARVI_LINEAR_DEFAULT_REPO_NAME`/`NARVI_LINEAR_DEFAULT_REPO_URL` are
**required at boot** — a deployment with Linear ingress enabled at all
always has a default repo to create against; there is no "ingress isn't
configured yet" refusal path on this surface the way there is on Slack's.

## Replying (a `prompted` event)

Send a follow-up message in an already-started agent session to add a
turn to the Narvi session it backs.

**Negative — one turn in flight at a time, dropped not queued.** Exactly
[slack.md](slack.md)'s own `CreateTurnPolicy.DropIfOpen` behavior: a
reply arriving while a turn is already `Pending`/`Dispatched`/
`Processing` gets an honest "still working" `AgentActivity" reply and is
never queued. Different from [web.md](web.md) (`409`) and
[github.md](github.md) (always queues).

**Negative — authorization gate on every reply**, identical in shape to
Slack's: one auto-link attempt against your Linear account's verified
email, then the SAME own-or-joined `prompt_session` check. A reply from
an unresolved or unauthorized actor gets an honest denial `AgentActivity`
reply and is never added as a turn.

**Negative — an unknown or still-claiming agent session is silently
ignored.** A `prompted` event for an `agent_session_id` Narvi has no
record of (or one whose session claim hasn't landed yet) is logged and
acknowledged with no reply at all — this is an accepted race/edge case,
not a bug to report.

## Plan mode: approve, reject, revise

Identical mechanism to [slack.md](slack.md)'s own plan-mode section — the
SAME `plandomain.MatchVerdict`/`MatchRevise` deterministic parsing, the
SAME shared `httpapi.DecidePlan` core, so a plan decided on Linear is
decided everywhere (a Slack button click or a web REST call against the
same plan sees, and honestly reports, whichever decision landed first).

```json narvi-command
{"name": "Type an approve keyword (\"approve\", \"approved\", or \"lgtm\") in a reply", "route": "POST /webhooks/linear"}
```

```json narvi-command
{"name": "Type a reject keyword (\"reject\", \"rejected\", or \"no\") in a reply", "route": "POST /webhooks/linear"}
```

```json narvi-command
{"name": "Type \"revise: <feedback>\" to request changes as a real revision turn", "route": "POST /webhooks/linear"}
```

**Negative.** There are no Linear-native buttons for this today — unlike
Slack's Block Kit Approve/Reject buttons and "Request changes" modal,
Linear's own agent-session UI has no equivalent Narvi wires up yet. The
exact-match, whole-message keyword rule and the empty-`revise:`-feedback
refusal are identical to Slack's (see [slack.md](slack.md) for both).

## What Linear does not support

- **Cancellation.** Linear's own agent-session "stop" signal is
  recognized but not honored — there is no session/turn stop command in
  this codebase yet. You get an honest reply saying cancellation isn't
  supported; the in-flight turn keeps running to its own natural
  conclusion or `platform.Timeouts.TurnDeadline` (60 min).
- No slash-command-equivalent syntax beyond the plan-mode keywords above
  — every other message is treated as an ordinary prompt.
- No per-issue/per-team repo override — one deployment-wide default
  repo, same limitation as Slack.

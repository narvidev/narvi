# MCP guide

The MCP surface is `POST /mcp`, the one endpoint an MCP app — an editor
plugin or desktop assistant that speaks the Model Context Protocol — calls
to use this deployment's tools. It accepts only a bearer token that Narvi's
own authorization server issued after you approved the app; your browser
cookie is not a credential there. A session an app starts through it
records `spawnSource: "mcp"`, set by the server from the authorization the
call carries, never from anything the app sends.

```json narvi-command
{"name": "Call this deployment's MCP tools (the app's one endpoint, with the bearer token you approved)", "route": "POST /mcp"}
```

## Connecting an app

An MCP client — an editor plugin or desktop assistant that speaks the Model
Context Protocol — connects to this deployment's `POST /mcp` with a bearer
token it obtains through Narvi's own OAuth authorization server (technical
plan §43.13). You never type a token anywhere: the app opens the
authorization page in your browser, you sign in if you are not already, and
you decide on the consent page.

```json narvi-command
{"name": "Approve an MCP client's access (the app opens this in your browser; it continues to the consent page, through sign-in first if needed)", "route": "GET /oauth/authorize"}
```

```json narvi-command
{"name": "The MCP consent page (who the app is, where your browser returns, what it may do)", "route": "GET /oauth/consent"}
```

```json narvi-command
{"name": "Allow or deny the MCP client (the consent page's own form)", "route": "POST /oauth/consent"}
```

The consent page shows who the app is, never only the name it gives
itself. An app an administrator of this deployment registered says so under
its name. An app that identifies itself by the web address of its own
description (a client ID metadata document) is headed by that address's
host — the one thing about it the app cannot make up, since Narvi read the
description from that very host, following no redirect to any other — with
the name it chose shown second; check that host before you allow it. A host
with non-Latin letters is always shown in its `xn--` form, exactly as Narvi
looks it up, never as letters that could pass for another host's. An app that registered itself (possible only when
the deployment turns dynamic registration on) is headed by just that — "an
app that registered itself" — with the name it gave itself shown second, in
quotes: nothing vouches for that name, so never take it for a host Narvi
checked. The page also shows the host your browser is sent back to — with
a warning when that is your own computer — and one checkbox per kind of
access; you can uncheck any of them, never add one. An app never does more than your own role allows: its tools call the
same routes this guide documents, checked against your role on every call.

Once an app is connected, you manage it in Settings → Integrations →
Connected apps, which says who vouches for it just as the consent page did
(an administrator; the host of its description's address, written exactly
as the consent page wrote it; or nobody): what
you last allowed it, when you connected it, when it
last called, and when the authorization lapses, 90 days after your latest
approval of it. That date is the latest any copy of the app can keep
working without asking you again. A copy connected by an earlier approval
asks you sooner, when that approval's own 90 days are up. An app you
approve keeps working without asking you again: it renews its own access
in the background, for up to 90 days after the approval that connected
it. It sends you back to the consent page when it has gone unused for 30 days,
when those 90 days are up, once it has been disconnected, or when Narvi
disconnects it itself after seeing one of its renewal tokens used twice,
or by another app. That is how a copied token shows itself, but an app that retries a renewal
whose answer was lost on the network, or two copies of the app sharing one
renewal token, look exactly the same, and approving the app again is then
the way back. Each approval gets 90 days of its own: approving an app again
neither takes away nor extends access you allowed it before — what an
earlier approval allowed keeps renewing exactly as it was allowed, until
its own 90 days are up — so to withdraw access, disconnect the app. An app
can also disconnect itself, for instance when you sign out of it; it then
leaves the list just the same.

```json narvi-command
{"name": "List your own connected MCP apps", "route": "GET /api/me/mcp-authorizations"}
```

```json narvi-command
{"name": "Disconnect one of your MCP apps (its very next call is refused)", "route": "DELETE /api/me/mcp-authorizations/{authorizationID}"}
```

Administrators decide which apps may ask at all, in the same panel:

```json narvi-command
{"name": "List registered MCP clients (admin)", "route": "GET /api/mcp-clients"}
```

```json narvi-command
{"name": "Register an MCP client and get its client ID (admin)", "route": "POST /api/mcp-clients"}
```

```json narvi-command
{"name": "Delete a registered MCP client, disconnecting every user of it (admin)", "route": "DELETE /api/mcp-clients/{clientID}"}
```

```json narvi-command
{"name": "Disable an MCP client, refusing every user of it from its next call (admin)", "route": "POST /api/mcp-clients/{clientID}/disable"}
```

```json narvi-command
{"name": "Enable a disabled MCP client again (admin)", "route": "POST /api/mcp-clients/{clientID}/enable"}
```

Deleting and disabling a client differ, and what each keeps out depends
on how the app came to be known. Deleting removes the client and
every authorization issued to it, for good. That keeps out a client an
administrator registered, since only an administrator can register it
again. It does not keep out an app known by its description's address:
the next time anyone starts to connect it, Narvi reads its description
again and registers it afresh. Nor does it keep out an app that registered
itself, which can register again under a new client ID for as long as the
deployment allows that. Disabling refuses the client from its very next
call — it can no longer be approved, renew its access or call `/mcp`,
though it can still disconnect itself — and deletes nothing: each approval
stays listed and can still be revoked, and enabling the client lets it
carry on with the access it still holds, without asking anyone again,
unless that access lapsed meanwhile. A disabled client stays as it is:
Narvi never reads a disabled app's description again and never removes it
as unused, so, for an app known by its description's address, disabling
keeps that address out, which deleting would not — but that one address
only. Whoever publishes the app's description can publish it at another
address on the same host, and the app then arrives as a new client that
people can approve, its page naming the same host. An app that registered
itself can register again under a new client ID whichever you do. Only
switching off apps known by their description's address, or
self-registration (below), keeps every such app out, and it pauses every
one of them; Narvi cannot block a whole host.

Administrators also see every member's connected apps, in Settings →
Members & access, under the "Connected apps" button on the member's row: the
same list the member sees under Integrations — never a token, which exists
nowhere in plaintext once the app received it — and a Revoke action behind
a confirmation that disconnects the app on the member's behalf. The app's
very next call is refused, exactly as when the member disconnects it, and
the audit log records it as revoked by that administrator, naming the
member. It withdraws what was approved, not the app itself: the member can
approve the app again. To keep an app out, an administrator disables its
client, or deletes it if an administrator registered it — which holds for
that client only: an app that registered itself, or one known by its
description's address, can come back as a new client (above); the
on-call runbook [`mcp-client-cutoff.md`](../runbooks/mcp-client-cutoff.md)
says how, and what each step does on the app's next call.

```json narvi-command
{"name": "List a member's connected MCP apps (admin)", "route": "GET /api/members/{userID}/mcp-authorizations"}
```

```json narvi-command
{"name": "Revoke one of a member's MCP apps on their behalf, its very next call refused (admin)", "route": "DELETE /api/members/{userID}/mcp-authorizations/{authorizationID}"}
```

## What an app can do

An app asks for one or both of two kinds of access. The consent page shows
each one it asked for, checked, and you can uncheck either before you allow
the app — never add one it did not ask for:

- **Read** (`mcp:read`): the model catalog and this deployment's sessions,
  with exactly the visibility your own account has — list them, read one,
  its live status (or wait, server-side, for it to settle), what it
  produced and each pull request's review verdict, its event history, and
  its plan versions.
- **Act as you** (`mcp:write`, which includes read): start sessions, send
  prompts, approve or reject plans, request revisions and stop sessions as
  you. Everything but stopping a session is what an app can do today;
  stopping comes to the same access, so an app you allow now gains it when
  it ships, without asking you again. This can run code in your
  repositories and spend on models, within what your own role allows.

An app sees only the tools its access covers: with read alone, no write
tool is listed and a call to one is answered exactly as a call to a tool
that does not exist. Access is fixed when the app receives its token: to
give an app that has only read the right to act, connect it again and
approve both. An app never does more than your own role allows — every tool
runs the same route these guides document, checked against your role on
every call — so a viewer's app can connect and read but is refused, as the
viewer is, when it starts a session or decides a plan.

**Starting a session** (`narvi_create_session`) is `POST /api/sessions`
([web.md](web.md)) called for you: the same checks — member role or above,
and every repository one this deployment already knows — the same audit
record, now also naming the app and its authorization, and the same first
turn, queued at once with the prompt. The app names the repositories, the
prompt and, optionally, a title, models and plan mode; it cannot set a
session's environment (path scope, mocks, Docker, egress policy). Each call
carries a new `idempotencyKey` (a UUID): an app that retries a call whose
answer it lost, with the same key and the same arguments, gets back the
session its first call started and starts nothing; the same key with
different arguments is refused. Your keys are yours across the app and
`POST /api/sessions`, so a key you already used to start a session yourself
is refused to the app too, and the other way round: a retry only ever
answers the way the session was started. The session appears in your list
like any other, its source shown as MCP.

**Plans, revisions and prompts** are the plan and turn routes of
[web.md](web.md) called for you, with every check they make and nothing
else. `narvi_list_plans` reads a session's plan versions — the list
`GET /api/sessions/{sessionID}/plans` returns — each with the id the app
decides it by. `narvi_approve_plan` and `narvi_reject_plan` are
`POST .../plans/{planId}/approve` and `/reject`: an admin or maintainer may
decide any session's plan, a member only a plan of a session they started or
joined, and a viewer none; the first decision wins, whichever way it was
made — from the web, a chat app or MCP — and a later one is refused; the
audit record names the app and its authorization. Approving queues the
implementation at once, which runs code and delivers what it changes like
any turn. `narvi_request_plan_revision` asks for the plan's next version
with the app's feedback, and `narvi_send_prompt` sends an ordinary prompt:
both are `POST /api/sessions/{sessionID}/turns`, the first with plan mode
on. An approval, a revision or a prompt is refused while any turn of the
session is queued or running — with the same `409` the web gets — and
nothing is queued behind it: the app waits for the session to settle
(`narvi_wait_for_session`) and asks again. While a plan awaits approval, an
ordinary prompt is refused too, unless Narvi reads it as a change to that
plan, which it then queues as a revision.

A revision never takes back an approval. Once you approve a plan, its
implementation keeps that approval until it ends: a revision asked for
while it runs is refused, as above, and where another channel does queue
one — a mention on the code host is queued — it waits, the implementation
finishes and opens its pull request, and only then does the revision run
and write the next version for you to approve, the approved one staying
approved.

**Brakes, and what disconnecting does not do.** `/mcp` is braked per
authorization — one person's approval of one app — on each server replica:
a burst of 30 calls, then one a
second (as shipped). Past that the app is answered `429` with a
`Retry-After`, before the call is read, so nothing runs; another app you
connected, and another person's use of the same app, keep their own
brakes. Starting sessions is braked too, because each one runs code and
spends on models: 5 calls to `narvi_create_session` per authorization, then
one a minute (as shipped). A retry counts as a call, since the brake runs
before Narvi can tell the call is a retry. Past that the call is refused
with how many seconds to wait, and starts nothing; retried after that wait
with the same key and the same arguments, it gets back the session an
earlier call with that key started, if one did. Disconnecting an app, or
cutting off its client, stops its next call — it does not stop a session it
already started, nor an implementation it approved: each runs to its end
like any other.

**Negatives.** The MCP surface is off unless the deployment sets
`NARVI_MCP_ENABLED=true`; while it is off, `/oauth/...` answers `503` — but
the Settings routes above keep working, so an authorization can always be
listed and revoked. Every role, viewer included, can connect an app and
disconnect its own; only an admin can register, delete, disable or enable a
client, or see and revoke another member's apps — any other role gets `403`, and an
authorization that is not that member's is `404`, whoever it belongs to.
Starting authorizations and renewing tokens are braked per network — the
address Narvi sees a request come from (one address, or one IPv6 `/48`,
though an app reaching Narvi through a translator the address names, such as
NAT64's well-known prefix or Teredo, counts as its own IPv4 address): a
burst of ten, then one every three seconds for authorizations and every two
for tokens. Past that, the authorization page answers "Too many requests
from your network" and never sends your browser anywhere, and an app's
renewal is answered `429` — it keeps its access and renews once the brake
refills, a few seconds after the requests that emptied it stop; it is never
sent back to the consent page for it. A flood from one network never spends
another network's brake, but a network is only as fine as the address Narvi
sees. Narvi serves plain HTTP, so a deployment reached over HTTPS has a
proxy in front of it, and Narvi reads no client address a proxy forwards:
whether to trust one is a deployment decision not made yet. Behind a proxy
that hides client addresses, every user arrives from the proxy's one
address and shares one brake per endpoint, so one sender's flood refuses
everyone's authorizations and renewals for as long as it lasts. At most
100 authorizations of one app can be waiting for approval at once; past
that, the page says the app has too many sign-ins waiting —
each lapses ten minutes after the app asked, so wait a few minutes and
start again from the app. Apps
that identify themselves by their description's address are accepted unless
the deployment sets `NARVI_MCP_CIMD_ENABLED=false`, and apps that register
themselves only if it sets `NARVI_MCP_DCR_ENABLED=true`. Switching either
off pauses every app it let in, from that app's next call: the app can no
longer be approved, renew its access or call `/mcp`, though it can still
disconnect itself. A pause deletes nothing — the app stays under Connected
apps, where you can still disconnect it — and switching the setting back on
lets the app carry on with the access it still holds, without asking you
again, unless that access lapsed meanwhile (30 days after the app last
renewed it, or 90 days after your approval). To withdraw your own access
for good, disconnect the app; to cut an app off for everyone, an
administrator disables or deletes its client (above). Narvi never
reads an app's description from this machine or a private network address,
and an app whose description cannot be read, or names any address but its
own, is shown an error page the first time. Narvi trusts a description it
read for at most an hour before reading it again; a change to it affects
only approvals made afterwards. If that re-read fails — the host down, the
description gone or no longer valid — the description Narvi last read keeps
being used for one more hour from that first failure, and no longer — and
never more than two hours after Narvi last read it: a description Narvi
has not read for longer than that is not used at all. After that the app
is shown an error page until its description can be read again.
Disconnecting deletes the authorization outright, and deleting a client
deletes every authorization issued to it — neither can be undone, and one
authorization cannot be paused: the pauses are an administrator disabling
one client, and the deployment-wide one above, switching off how an app
registered.
An app registered with a redirect address that does not match exactly
(except the port of a `127.0.0.1`/`[::1]` address) is shown an error page
and your browser is never sent anywhere. Each approval is single-use and
expires ten minutes after the app asked; another signed-in account can
never decide a request someone else opened first. A browser-hosted MCP
client (one running inside a web page on another site) cannot reach
`/mcp` even with a token — the Origin check refuses it.

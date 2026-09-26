# Web guide

The web surface is Narvi's own browser-facing REST API (`spawnSource:
"web"`) — every route below is under `/api/...` except the sign-in routes
(GitHub OAuth, generic OIDC SSO, and the public sign-in-capabilities probe)
and the live session WebSocket, both listed in their own sections. Every
request (except sign-in itself) is authenticated by the
`narvi_auth_session` cookie either `GET /auth/github/callback` or
`GET /auth/oidc/callback` mints (an OAuth/OIDC redirect, never a
client-initiated `POST` — see the machine-checked block below); a request
with no valid cookie gets `401`.

Two things this guide deliberately does **not** cover — see
[README.md](README.md#what-this-check-cannot-catch) for why: the
per-automation inbound-webhook trigger
(`POST /webhooks/automations/{automationID}`), and the sandbox-agent-only
bearer-authenticated routes under `/sessions/{sessionID}/...` — those are
machine-to-machine plumbing, not something a human using the web app ever
calls. `GET /sessions/{sessionID}/ws` (below) is a different case: it is
NOT on that excluded list — it is the real, human-facing live-stream
connection this guide documents.

## Sign-in

```json narvi-command
{"name": "Sign in with GitHub", "route": "GET /auth/github/login"}
```

```json narvi-command
{"name": "GitHub OAuth callback (completes sign-in)", "route": "GET /auth/github/callback"}
```

```json narvi-command
{"name": "Sign in with SSO (OIDC)", "route": "GET /auth/oidc/login"}
```

```json narvi-command
{"name": "OIDC callback (completes sign-in)", "route": "GET /auth/oidc/callback"}
```

```json narvi-command
{"name": "Sign-in capabilities probe (whether OIDC SSO is configured for this deployment; public, unauthenticated)", "route": "GET /auth/capabilities"}
```

```json narvi-command
{"name": "Sign out", "route": "POST /auth/logout"}
```

**Negatives.** GitHub OAuth and generic OIDC SSO are the *only* two
sign-in methods — there is no password, no magic-link login, and no
other OAuth provider. `GET /auth/oidc/login`/`GET /auth/oidc/callback`
are always mounted, but refuse every request with `503` unless this
deployment configures
`NARVI_OIDC_ISSUER`/`NARVI_OIDC_CLIENT_ID`/`NARVI_OIDC_CLIENT_SECRET`
(all-or-none) — mirroring the cloud-identity discovery routes' own
identical "fail closed when unset" behavior below, rather than the route
not existing at all; `GET /auth/capabilities` reports
`oidcConfigured: false` in that case, and the sign-in view keeps its SSO
button disabled. Signing in via
either provider requires a **verified** email — GitHub's own
primary/verified `/user/emails` entry, or the OIDC ID token's own `email`
claim with `email_verified` strictly `true` (absent, `false`, or any
non-boolean value is refused, never trusted) — matching this deployment's
own allowlist (`NARVI_ALLOWED_EMAIL_DOMAINS`/`NARVI_ALLOWED_GITHUB_ORGS`/
`NARVI_ALLOWED_EMAILS`; GitHub org membership is a GitHub-only mechanism,
not available to an OIDC sign-in). An account that matches none of the
allowed mechanisms is refused sign-in outright, with no self-service way
to request access — an admin has to widen the allowlist or add the person
to `NARVI_INITIAL_ADMIN_EMAILS`. A brand-new user's role always starts at
**viewer** (§13.3's own lowest role) unless they're on the admin allowlist
— nobody self-elevates. A verified email that already matches an existing
user (by primary email or another linked identity's own verified email)
merges onto that SAME account instead of creating a second one — the
identical graph-merge rule §13.2 already applies to Slack/Linear identities.

```json narvi-command
{"name": "Consume an identity-link magic link (posted privately in a Slack/Linear reply, never a real sign-in route)", "route": "GET /auth/identity-link/{nonce}"}
```

This route is **not** a second way to sign in — it is what a Slack/Linear
identity-linking notice's own private link points at, one-time-use,
short-lived (`platform.Timeouts.IdentityLinkPromptTTL`, 24h), and it only
ever *links* a Slack/Linear identity onto an already-signed-in-or-signing-
in GitHub account.

## Sessions

```json narvi-command
{"name": "Create a session", "route": "POST /api/sessions"}
```

Requires **member role or above** (§13.3: viewer can view but never
create). The request body's `repos` field is always a list — never a
single repo, even for a one-repo session (`docs/TECHNICAL_PLAN.md`'s own
"repos are always a list" invariant). An optional `planMode` boolean
picks plan-vs-build for the session's very first turn — **this is the
one and only way the web surface picks Mode**: unlike Slack/Linear/GitHub,
a web-created session's routing decision is recorded with `source:
"explicit"`, never `"classifier"` — the LLM intent classifier is never
invoked for a REST-created session at all, and `req.spawnSource` in the
request body is ignored/overwritten server-side (this endpoint is
structurally only ever reachable as the real web surface — a client
claiming a different `spawnSource` in its own JSON body gets no different
treatment).

```json narvi-command
{"name": "List sessions", "route": "GET /api/sessions"}
```

```json narvi-command
{"name": "Get a session", "route": "GET /api/sessions/{sessionID}"}
```

```json narvi-command
{"name": "List a session's events", "route": "GET /api/sessions/{sessionID}/events"}
```

```json narvi-command
{"name": "List a session's artifacts", "route": "GET /api/sessions/{sessionID}/artifacts"}
```

```json narvi-command
{"name": "Mint a live-session WebSocket token", "route": "POST /api/sessions/{sessionID}/ws-token"}
```

```json narvi-command
{"name": "Open the live session WebSocket", "route": "GET /sessions/{sessionID}/ws"}
```

**Negatives.** The WS connection must send its own `subscribe{token,
clientId}` frame within `platform.Timeouts.ClientSubscribeTimeout` (30s)
of connecting, or the server closes it (code `4001`, "re-auth required").
The minted token expires after `platform.Timeouts.WSTokenTTL` (24h) — a
tab left open across that boundary gets closed (code `4002`, "token
expired") and needs a fresh mint. The server pings every
`platform.Timeouts.ClientWSPingInterval` (30s); an unanswered ping closes
the connection (code `4003`, "idle timeout"). A `fetch_history` frame
sent more than once per `platform.Timeouts.ClientFetchHistoryMinInterval`
(250ms) on the same connection is logged and dropped — the connection
stays open, but that one request is silently ignored.

## Turns (prompting an existing session)

```json narvi-command
{"name": "Add a turn (send a prompt) to a session", "route": "POST /api/sessions/{sessionID}/turns"}
```

**Negative — the busy behavior is REST-specific.** If the session already
has a non-terminal turn (`Pending`/`Dispatched`/`Processing`), this
endpoint refuses with **`409 Conflict`** (`CreateTurnPolicy.RejectIfOpen`,
`internal/adapters/inbound/httpapi/turn.go`) — it never silently queues a
second turn behind the first, and it never silently drops your message
either. This is deliberately different from every other surface: Slack
and Linear *drop* a message sent to a busy session (an honest in-thread
"still working" reply, nothing queued — see [slack.md](slack.md) /
[linear.md](linear.md)), and GitHub *always* queues a backlog turn behind
the current one, never refuses (see [github.md](github.md)). A web client
that gets a `409` here has to retry once the in-flight turn actually
finishes — there is no built-in queuing to fall back on.

**Negative — an awaiting-approval plan can also produce this `409`, with
one exception.** If `planMode` is `false` and the session has a plan in
`plan.StatusAwaitingApproval`, this same endpoint refuses with `409`
again (same core, a different message) — UNLESS the `plan_followup`
classifier reads the prompt as a confident amendment, in which case the
turn is silently promoted to a real plan-revision turn instead of being
refused. This is the exact same `createTurnLocked` mechanism
[slack.md](slack.md)'s own "Plan mode" section documents in full — it is
not Slack-specific, it is shared by every surface this core serves,
including this REST endpoint.

## Plan mode

```json narvi-command
{"name": "List a session's plans", "route": "GET /api/sessions/{sessionID}/plans"}
```

```json narvi-command
{"name": "Approve a plan", "route": "POST /api/sessions/{sessionID}/plans/{planId}/approve"}
```

```json narvi-command
{"name": "Reject a plan", "route": "POST /api/sessions/{sessionID}/plans/{planId}/reject"}
```

Approving/rejecting requires §13.3's `approve_plan` action — own/joined
sessions for member and above, ANY session for maintainer/admin; viewer
never. This is the exact same `httpapi.DecidePlan` core every surface's
own approve/reject path (Slack buttons and typed keywords, Linear typed
keywords, the "Request changes" modal) calls — a plan decided first on
one surface is decided everywhere; a later attempt on another surface
sees (and is told) the outcome the first attempt already produced, never
a second, conflicting decision.

## Review

```json narvi-command
{"name": "Re-trigger a review", "route": "POST /api/sessions/{sessionID}/review/retrigger"}
```

```json narvi-command
{"name": "Rebut a review finding", "route": "POST /api/sessions/{sessionID}/review/findings/{identityHash}/rebut"}
```

```json narvi-command
{"name": "Apply a review finding's suggested fix", "route": "POST /api/sessions/{sessionID}/review/findings/{identityHash}/apply-suggestion"}
```

```json narvi-command
{"name": "Get a session's code-review readout", "route": "GET /api/sessions/{sessionID}/review"}
```

**Negative.** Re-trigger/rebut/apply-suggestion all require maintainer
role or above (§13.3: "Edit review verdicts; re-trigger reviews ...
admin/maintainer" only) — a member or viewer gets `403`, even on a
session they created or joined. This is stricter than plan approval on
purpose: an ordinary member can approve their own plan, but cannot
re-open or rebut a review verdict. The readout GET carries no extra RBAC
beyond session visibility — a viewer may read it.

## Release review

The dedicated release-review screen (§12.2 item 9): a read model plus
three composition-finding actions over the same underlying
`release_manifest_checks` row.

```json narvi-command
{"name": "Get a session's release-manifest readout", "route": "GET /api/sessions/{sessionID}/release-manifest"}
```

```json narvi-command
{"name": "Block a release on a composition finding", "route": "POST /api/sessions/{sessionID}/release-manifest/block"}
```

```json narvi-command
{"name": "Acknowledge a composition finding and ship anyway", "route": "POST /api/sessions/{sessionID}/release-manifest/acknowledge"}
```

```json narvi-command
{"name": "Unblock a previously blocked release", "route": "POST /api/sessions/{sessionID}/release-manifest/unblock"}
```

**Negative — three different rows, not one RBAC gate.** Block requires
maintainer role or above (the same §13.3 row as re-triggering a review) —
blocking is the safety-additive response to a composition finding.
Acknowledge and Unblock are **admin-only**: both are a human electing to
ship (or re-open) despite an already-computed cross-PR risk signal, and
that override sits in the stricter, admin-only §13.3 row, not the
maintainer one Block uses. All three reject a transition that the
finding's own current state does not allow (an already-decided
composition, or one whose own composition review has not completed yet)
with a structured error rather than silently no-op'ing.

## Decision inbox

```json narvi-command
{"name": "List the decision inbox", "route": "GET /api/decision-inbox"}
```

```json narvi-command
{"name": "Merge a pull request from the decision inbox", "route": "POST /api/decision-inbox/merge"}
```

```json narvi-command
{"name": "Accept a review verdict the engine refused to publish as low-risk", "route": "POST /api/decision-inbox/accept-verdict"}
```

```json narvi-command
{"name": "Revoke a previously accepted verdict", "route": "POST /api/decision-inbox/revoke-verdict-acceptance"}
```

**Negative.** Accept/revoke both require maintainer role or above
(§13.3's `accept_review_verdict` action) — this is the human-acceptance
surface [github.md](github.md)'s own "Review verdicts and decision inbox"
section describes from the GitHub-ingress side; accepting binds to one
exact verdict, attempt, and review context, and a new attempt or a moved
base makes the acceptance inapplicable again, never silently still-valid.

## Uploads

```json narvi-command
{"name": "Mint an upload URL", "route": "POST /api/sessions/{sessionID}/uploads"}
```

```json narvi-command
{"name": "Confirm an upload completed", "route": "POST /api/sessions/{sessionID}/uploads/{uploadID}/complete"}
```

```json narvi-command
{"name": "Fetch uploaded content", "route": "GET /api/sessions/{sessionID}/uploads/{uploadID}/content"}
```

**Negatives.** Uploads are **feature-flagged off entirely** unless this
deployment configured object storage (`NARVI_OBJECT_STORE_ENDPOINT` and
friends) — every route above 404s/refuses cleanly on a deployment that
never set it up, rather than half-working. A minted upload URL expires
after `platform.Timeouts.UploadPresignPutTTL` (15 min); a mint that is
never confirmed within `platform.Timeouts.UploadPendingSweepAfter` (24h)
is swept away automatically (`platform.Timeouts.
UploadAbandonmentSweepInterval`, every 15 min) — there is no way to
resurrect an abandoned upload, only to mint a new one.

## Models

```json narvi-command
{"name": "Get the model catalog", "route": "GET /api/models"}
```

## Members, identities, and audit log

```json narvi-command
{"name": "List members", "route": "GET /api/members"}
```

```json narvi-command
{"name": "Change a member's role", "route": "PATCH /api/members/{userID}/role"}
```

```json narvi-command
{"name": "Link an identity to a member", "route": "POST /api/members/{userID}/identities"}
```

```json narvi-command
{"name": "Unlink an identity from a member", "route": "DELETE /api/members/{userID}/identities/{identityID}"}
```

```json narvi-command
{"name": "List the audit log", "route": "GET /api/audit-log"}
```

**Negative.** Changing roles/linking/unlinking identities is admin-only
(§13.3's own last row: "members & roles ... admin" only) — maintainer and
below get `403`. The audit log records completed state changes only; a
denied/refused request (a `403`, a rollout refusal, an authz denial) is
never itself an audit-log row (see [github.md](github.md)'s own "silent
refusal" section for the sharpest example of this).

## ChatGPT account link

```json narvi-command
{"name": "Start a ChatGPT account link", "route": "POST /api/me/chatgpt-link"}
```

```json narvi-command
{"name": "Get ChatGPT link status", "route": "GET /api/me/chatgpt-link"}
```

```json narvi-command
{"name": "Remove a ChatGPT account link", "route": "DELETE /api/me/chatgpt-link"}
```

## Connected apps (MCP clients)

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
the deployment turns dynamic registration on) says so: nothing vouches for
its name. The page also shows the host your browser is sent back to — with
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

**Negatives.** The MCP surface is off unless the deployment sets
`NARVI_MCP_ENABLED=true`; while it is off, `/oauth/...` answers `503` — but
the Settings routes above keep working, so an authorization can always be
listed and revoked. Every role, viewer included, can connect an app and
disconnect its own; only an admin can register or delete a client. Apps
that identify themselves by their description's address are accepted unless
the deployment sets `NARVI_MCP_CIMD_ENABLED=false`, and apps that register
themselves only if it sets `NARVI_MCP_DCR_ENABLED=true`. Switching either
off pauses every app it let in, from that app's next call: the app can no
longer be approved, renew its access or call `/mcp`, though it can still
disconnect itself. A pause deletes nothing — the app stays under Connected
apps, where you can still disconnect it — and switching the setting back on
lets the app carry on with the access it still holds, without asking you
again, unless that access lapsed meanwhile (30 days after the app last
renewed it, or 90 days after your approval). To cut an app off for good,
disconnect it, or have an administrator delete it. Narvi never
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
authorization cannot be paused: the only pause is the deployment-wide one
above, switching off how an app registered.
An app registered with a redirect address that does not match exactly
(except the port of a `127.0.0.1`/`[::1]` address) is shown an error page
and your browser is never sent anywhere. Each approval is single-use and
expires ten minutes after the app asked; another signed-in account can
never decide a request someone else opened first. A browser-hosted MCP
client (one running inside a web page on another site) cannot reach
`/mcp` even with a token — the Origin check refuses it.

## Administration & configuration

Everything below is settings/configuration surface, not day-to-day
session use — grouped here rather than given the same prose treatment as
the sections above because each one is a straightforward CRUD endpoint
over one settings table. Every write endpoint in this section requires
maintainer role or above at minimum; several (global secrets/credentials,
integrations, prompt-template activation, per-repo auto-merge and
sentinel-auto-fix toggles) are admin-only (§13.3's own last two rows) —
a maintainer gets `403` on those specifically, not a degraded response.

**Automations**

```json narvi-command
{"name": "Create an automation", "route": "POST /api/automations"}
```

```json narvi-command
{"name": "List automations", "route": "GET /api/automations"}
```

```json narvi-command
{"name": "Get an automation", "route": "GET /api/automations/{automationID}"}
```

```json narvi-command
{"name": "List an automation's invocations", "route": "GET /api/automations/{automationID}/invocations"}
```

```json narvi-command
{"name": "Pause an automation", "route": "POST /api/automations/{automationID}/pause"}
```

```json narvi-command
{"name": "Resume an automation", "route": "POST /api/automations/{automationID}/resume"}
```

```json narvi-command
{"name": "Rotate an automation's inbound webhook token", "route": "POST /api/automations/{automationID}/webhook-token"}
```

```json narvi-command
{"name": "Revoke an automation's inbound webhook token", "route": "DELETE /api/automations/{automationID}/webhook-token"}
```

**Intent classifier templates**

```json narvi-command
{"name": "List intent-classifier prompt templates", "route": "GET /api/intent-templates"}
```

```json narvi-command
{"name": "Preview an intent-classifier prompt template", "route": "POST /api/intent-templates/preview"}
```

```json narvi-command
{"name": "Upsert an intent-classifier prompt template", "route": "POST /api/intent-templates"}
```

**Environments**

```json narvi-command
{"name": "List environments", "route": "GET /api/environments"}
```

**Integrations**

```json narvi-command
{"name": "List configured ingress integrations", "route": "GET /api/integrations"}
```

One row per ingress surface (Slack, Linear, GitHub) — a derived read
model, never a connect/disconnect write; there is no POST/PUT/DELETE on
this route at all.

**Capabilities**

```json narvi-command
{"name": "Get the extension & licensing capability set", "route": "GET /api/capabilities"}
```

Read-only, and open to every role including viewer
(`authz.ActionViewCapabilities`, §13.3 row 1) — everyone needs to know
what this deployment is licensed for, not just an admin. Two other routes
in this section are every-role-including-viewer too (platform analytics
and your profile, both immediately below) — this is not unique to it.

**Platform analytics**

```json narvi-command
{"name": "Get the platform-wide analytics rollup", "route": "GET /api/analytics"}
```

The un-scoped sibling of `GET /api/repos/{owner}/{repo}/review-analytics`
below — same `authz.ActionViewAnalytics` gate (every role, including
viewer).

**Your profile**

```json narvi-command
{"name": "Get your own profile", "route": "GET /api/me"}
```

The sign-in view's identity auto-link panel and already-signed-in state
read. Deliberately not `/api/members/{userID}` with a self filter — see
httpapi/me.go's own doc comment — and, like capabilities above, open to
every role including viewer (`authz.ActionViewOwnProfile`).

**Per-repo settings**

```json narvi-command
{"name": "Get a repo's settings", "route": "GET /api/repos/{owner}/{repo}/settings"}
```

```json narvi-command
{"name": "Update a repo's settings", "route": "PUT /api/repos/{owner}/{repo}/settings"}
```

```json narvi-command
{"name": "List a repo's false-positive review patterns", "route": "GET /api/repos/{owner}/{repo}/false-positive-patterns"}
```

```json narvi-command
{"name": "Retire a false-positive review pattern", "route": "POST /api/repos/{owner}/{repo}/false-positive-patterns/{patternID}/retire"}
```

```json narvi-command
{"name": "Update a repo's auto-approval eligibility settings", "route": "PUT /api/repos/{owner}/{repo}/auto-approval-settings"}
```

```json narvi-command
{"name": "Toggle a repo's auto-merge", "route": "PUT /api/repos/{owner}/{repo}/auto-merge"}
```

```json narvi-command
{"name": "Toggle a repo's automatic re-review", "route": "PUT /api/repos/{owner}/{repo}/auto-retrigger-review"}
```

```json narvi-command
{"name": "Toggle a repo's description auto-fix", "route": "PUT /api/repos/{owner}/{repo}/description-autofix"}
```

```json narvi-command
{"name": "Update a repo's review-depth config", "route": "PUT /api/repos/{owner}/{repo}/review-depth"}
```

```json narvi-command
{"name": "Update a repo's review cost budget", "route": "PUT /api/repos/{owner}/{repo}/review-cost-budget"}
```

```json narvi-command
{"name": "Get a repo's review analytics", "route": "GET /api/repos/{owner}/{repo}/review-analytics"}
```

```json narvi-command
{"name": "Get a repo's digest scope", "route": "GET /api/repos/{owner}/{repo}/digest-scope"}
```

Read-only derived view of which Slack channels/Linear organizations are
"in scope for" this repo's own daily digest — not "will receive": the
scope is computed fresh on every read, never stored.

```json narvi-command
{"name": "Get a repo's preview-link config", "route": "GET /api/repos/{owner}/{repo}/preview-config"}
```

```json narvi-command
{"name": "Update a repo's preview-link config", "route": "PUT /api/repos/{owner}/{repo}/preview-config"}
```

A further, separately-gated route — deliberately NOT folded into
`PUT /settings` above, because a preview-link config's request body
carries a credential, and a credential-carrying body must never share a
shape with ordinary configuration.

```json narvi-command
{"name": "Get a repo's shadow-mode ledger", "route": "GET /api/repos/{owner}/{repo}/shadow-ledger"}
```

```json narvi-command
{"name": "Activate a repo from shadow mode to live", "route": "POST /api/repos/{owner}/{repo}/shadow-ledger/activate"}
```

**Negative.** The shadow ledger pair is **admin-only** and carries no
§13.3 table row at all — see that action's own doc comment
(`internal/domain/authz/action.go`) for why: this ledger holds a
customer's source code at rest in full, and that is treated as a
strictly narrower audience than the rest of this section's already
admin-gated rows.

**Per-repo, per-environment, and global provider credentials / sandbox
secrets** — the SAME four-verb CRUD shape at three different scopes
(§27.1/§27.2); a value set at a narrower scope always wins over a wider
one at resolution time, never the other way around.

```json narvi-command
{"name": "Create a repo provider credential", "route": "POST /api/repos/{owner}/{repo}/provider-credentials"}
```

```json narvi-command
{"name": "List a repo's provider credentials", "route": "GET /api/repos/{owner}/{repo}/provider-credentials"}
```

```json narvi-command
{"name": "Update a repo provider credential", "route": "PUT /api/repos/{owner}/{repo}/provider-credentials/{credentialID}"}
```

```json narvi-command
{"name": "Delete a repo provider credential", "route": "DELETE /api/repos/{owner}/{repo}/provider-credentials/{credentialID}"}
```

```json narvi-command
{"name": "Create an environment provider credential", "route": "POST /api/environments/{environmentID}/provider-credentials"}
```

```json narvi-command
{"name": "List an environment's provider credentials", "route": "GET /api/environments/{environmentID}/provider-credentials"}
```

```json narvi-command
{"name": "Update an environment provider credential", "route": "PUT /api/environments/{environmentID}/provider-credentials/{credentialID}"}
```

```json narvi-command
{"name": "Delete an environment provider credential", "route": "DELETE /api/environments/{environmentID}/provider-credentials/{credentialID}"}
```

```json narvi-command
{"name": "Create a global provider credential", "route": "POST /api/provider-credentials"}
```

```json narvi-command
{"name": "List global provider credentials", "route": "GET /api/provider-credentials"}
```

```json narvi-command
{"name": "Update a global provider credential", "route": "PUT /api/provider-credentials/{credentialID}"}
```

```json narvi-command
{"name": "Delete a global provider credential", "route": "DELETE /api/provider-credentials/{credentialID}"}
```

```json narvi-command
{"name": "Create a repo sandbox secret", "route": "POST /api/repos/{owner}/{repo}/sandbox-secrets"}
```

```json narvi-command
{"name": "List a repo's sandbox secrets", "route": "GET /api/repos/{owner}/{repo}/sandbox-secrets"}
```

```json narvi-command
{"name": "Update a repo sandbox secret", "route": "PUT /api/repos/{owner}/{repo}/sandbox-secrets/{secretID}"}
```

```json narvi-command
{"name": "Delete a repo sandbox secret", "route": "DELETE /api/repos/{owner}/{repo}/sandbox-secrets/{secretID}"}
```

```json narvi-command
{"name": "Create an environment sandbox secret", "route": "POST /api/environments/{environmentID}/sandbox-secrets"}
```

```json narvi-command
{"name": "List an environment's sandbox secrets", "route": "GET /api/environments/{environmentID}/sandbox-secrets"}
```

```json narvi-command
{"name": "Update an environment sandbox secret", "route": "PUT /api/environments/{environmentID}/sandbox-secrets/{secretID}"}
```

```json narvi-command
{"name": "Delete an environment sandbox secret", "route": "DELETE /api/environments/{environmentID}/sandbox-secrets/{secretID}"}
```

```json narvi-command
{"name": "Create a global sandbox secret", "route": "POST /api/sandbox-secrets"}
```

```json narvi-command
{"name": "List global sandbox secrets", "route": "GET /api/sandbox-secrets"}
```

```json narvi-command
{"name": "Update a global sandbox secret", "route": "PUT /api/sandbox-secrets/{secretID}"}
```

```json narvi-command
{"name": "Delete a global sandbox secret", "route": "DELETE /api/sandbox-secrets/{secretID}"}
```

**Negative.** A provider credential's or a sandbox secret's own value is
**write-only** once saved — every GET above returns a masked placeholder
proving a value is configured, never a partial or full reveal of the
real secret. There is no "show value" endpoint anywhere in this surface;
recovering a forgotten value means rotating it (setting a new one), never
reading the old one back.

**Environment cloud-identity, cluster binding, and OpenCode config**

```json narvi-command
{"name": "Create an environment cloud-identity binding", "route": "POST /api/environments/{environmentID}/cloud-identity-bindings"}
```

```json narvi-command
{"name": "List an environment's cloud-identity bindings", "route": "GET /api/environments/{environmentID}/cloud-identity-bindings"}
```

```json narvi-command
{"name": "Update an environment cloud-identity binding", "route": "PUT /api/environments/{environmentID}/cloud-identity-bindings/{bindingID}"}
```

```json narvi-command
{"name": "Delete an environment cloud-identity binding", "route": "DELETE /api/environments/{environmentID}/cloud-identity-bindings/{bindingID}"}
```

```json narvi-command
{"name": "Create a global cloud-identity binding", "route": "POST /api/cloud-identity-bindings"}
```

```json narvi-command
{"name": "List global cloud-identity bindings", "route": "GET /api/cloud-identity-bindings"}
```

```json narvi-command
{"name": "Update a global cloud-identity binding", "route": "PUT /api/cloud-identity-bindings/{bindingID}"}
```

```json narvi-command
{"name": "Delete a global cloud-identity binding", "route": "DELETE /api/cloud-identity-bindings/{bindingID}"}
```

```json narvi-command
{"name": "Rotate the cloud-identity OIDC signing key", "route": "POST /api/cloud-identity/signing-keys/rotate"}
```

```json narvi-command
{"name": "Get an environment's cluster binding", "route": "GET /api/environments/{environmentID}/cluster-binding"}
```

```json narvi-command
{"name": "Set an environment's cluster binding", "route": "PUT /api/environments/{environmentID}/cluster-binding"}
```

```json narvi-command
{"name": "Delete an environment's cluster binding", "route": "DELETE /api/environments/{environmentID}/cluster-binding"}
```

```json narvi-command
{"name": "Get an environment's OpenCode config", "route": "GET /api/environments/{environmentID}/opencode-config"}
```

```json narvi-command
{"name": "Set an environment's OpenCode config", "route": "PUT /api/environments/{environmentID}/opencode-config"}
```

```json narvi-command
{"name": "Delete an environment's OpenCode config", "route": "DELETE /api/environments/{environmentID}/opencode-config"}
```

```json narvi-command
{"name": "Get the global OpenCode config", "route": "GET /api/opencode-config"}
```

```json narvi-command
{"name": "Set the global OpenCode config", "route": "PUT /api/opencode-config"}
```

```json narvi-command
{"name": "Delete the global OpenCode config", "route": "DELETE /api/opencode-config"}
```

**Workflow definitions, bindings, and runs**

The workflow definition & run API (§25.10/§25.11): authoring a workflow
definition, activating it onto a repo via a binding, and the HITL
approve/reject/revise gate a running workflow step can stop at.

```json narvi-command
{"name": "List workflow definitions", "route": "GET /api/workflow-definitions"}
```

```json narvi-command
{"name": "Create a workflow definition", "route": "POST /api/workflow-definitions"}
```

```json narvi-command
{"name": "Get a workflow definition", "route": "GET /api/workflow-definitions/{id}"}
```

```json narvi-command
{"name": "Replace a workflow definition", "route": "PUT /api/workflow-definitions/{id}"}
```

```json narvi-command
{"name": "Delete a workflow definition", "route": "DELETE /api/workflow-definitions/{id}"}
```

```json narvi-command
{"name": "List workflow bindings", "route": "GET /api/workflow-bindings"}
```

```json narvi-command
{"name": "Activate a workflow binding", "route": "PUT /api/workflow-bindings"}
```

```json narvi-command
{"name": "Get a workflow run", "route": "GET /api/workflow-runs/{runId}"}
```

```json narvi-command
{"name": "List a session's workflow runs", "route": "GET /api/sessions/{sessionID}/workflow-runs"}
```

```json narvi-command
{"name": "Decide a workflow HITL step", "route": "POST /api/workflow-runs/{runId}/steps/{stepRunId}/decide"}
```

**Negative.** Definition reads/writes require maintainer role or above
(`authz.ActionManageWorkflowDefinitions`); a definition already built-in
or already bound to a repo refuses delete with a structured error rather
than silently orphaning the binding. Binding writes are **admin-only**
(`authz.ActionActivateWorkflowBinding`) — a maintainer may author a
definition but not activate it onto a repo. The two session-scoped GETs
(a run, a session's own run list) carry no RBAC beyond session
visibility, like every other session-read route in this guide.

**Diagnostics**

```json narvi-command
{"name": "View the shadow-mode classifier comparison report", "route": "GET /api/admin/shadow-compare"}
```

This one is read-only diagnostics for the intent classifier's own §18.5
shadow mode — see [github.md](github.md)'s own "shadow classification"
section for what shadow mode actually means (and does not yet do).

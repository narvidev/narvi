# Cutting off an MCP client

A routine operator procedure, not a failure mode: no alert backs it. Use it
when an MCP client -- an editor plugin or desktop assistant connected through
Narvi's own OAuth authorization server (`docs/TECHNICAL_PLAN.md` §43.13-§43.19)
-- must stop acting for the users who approved it: a compromised app, an app
that misbehaves, a user who left, or a flood of authorization requests.

Every step below takes effect on the client's **next call**. The bearer check
on `POST /mcp` reads the token, its grant, its client and its user in one query
on every call, with no cache, so there is nothing to wait for and nothing to
restart -- except where a step says it changes the deployment's configuration.

## Find the client

Each Settings screen shows only some of a client's fields; the API behind it
answers all of them.

- **Settings → Integrations → MCP clients** (admin) lists every client of
  every kind: its name, its public `clientId`, its redirect URIs, when it was
  registered and, when it is disabled, since when -- with its Delete and
  Disable (or Enable) actions. It does not show the internal `id` or the
  kind. The kind shows in the `clientId`: `narvi_mcp_c_...` is
  pre-registered, `narvi_mcp_d_...` registered itself (dynamic), and an https
  URL is a metadata-document client, the URL being its document's.
  `GET /api/mcp-clients` answers each client's `id` and `kind` too.
- **Settings → Members & access → Connected apps** on a member's row (admin)
  lists what one member has approved: each app's name and who vouches for it
  -- "Registered by an administrator of this deployment" (pre-registered),
  "Identified by <host>" (a metadata-document client, the host of its URL)
  or "Registered by the app itself" (dynamic) -- never its `clientId`.
  `GET /api/members/{userID}/mcp-authorizations` answers each
  authorization's `clientId` and `clientKind`.
- In SQL, on the control plane's database (the one `NARVI_DATABASE_URL`
  names): `SELECT id, client_id, kind, client_name, disabled_at FROM
  mcp_oauth_clients WHERE client_id = '<clientId>';`

## Stop one user's use of an app: revoke their authorization

- The user themself: Settings → Integrations → Connected apps → Revoke
  (`DELETE /api/me/mcp-authorizations/{authorizationID}`).
- An administrator, on their behalf: Settings → Members & access → the
  member's **Connected apps** → Revoke
  (`DELETE /api/members/{userID}/mcp-authorizations/{authorizationID}`). An
  authorization id that is not that member's is `404`, whoever's it is.

**Next call:** the app's access token gets `401` with
`error="invalid_token"`; its refresh token gets `invalid_grant`. Every code,
access token and refresh token issued under the authorization is deleted with
it (§43.16). Audited as `mcp_authorization.revoked` with reason `user` or
`admin` (the latter attributed to the administrator, `detail.target_user_id`
naming the member).

**It does not keep the app out:** the user can approve it again from its
consent page. To keep an app out, cut off the client itself (next section).

## Stop an app for everyone: cut off its client

Two actions, both under Settings → Integrations → MCP clients (admin), both
audited, both taking the internal `id`:

- **Disable** (`POST /api/mcp-clients/{clientID}/disable`; audited
  `mcp_client.disabled`). **Next call:** every token issued to the client
  gets `401`; its refresh gets `invalid_client`; its authorization and
  consent pages show an error page and never redirect; it may still give its
  tokens back (`POST /oauth/revoke`). Nothing is deleted: every
  authorization stays listed, and each user can still revoke theirs. A
  disabled row is never swept as unused, and a disabled metadata-document
  client's document is never fetched again. **Enable**
  (`POST /api/mcp-clients/{clientID}/enable`; audited `mcp_client.enabled`)
  undoes it: the client carries on with whatever has not lapsed meanwhile,
  with no new consent. Either answers `409` when the client is already in
  that state.
- **Delete** (`DELETE /api/mcp-clients/{clientID}`; audited
  `mcp_client.deleted`, plus one `mcp_authorization.revoked`, reason
  `client_deleted`, per authorization it took). Every authorization, pending
  request, code and token issued to the client goes with it. **Next call:**
  every user's access token gets `401`; a refresh, a code exchange or an
  authorization naming its `clientId` is refused as an unknown client --
  until something registers that `clientId` again (below). Cannot be undone.

Either action holds for one `clientId`, never for the app behind it, and
what it holds depends on how the client was registered (the kind: "Find the
client" above):

| Kind | Disable | Delete |
|---|---|---|
| pre-registered (`narvi_mcp_c_...`) | keeps that `clientId` out until enabled | keeps that `clientId` out for good: only an administrator can register the app again, and a new registration gets a new `clientId` |
| metadata document (an https URL) | keeps that exact URL out until enabled: the disabled row blocks that URL and no other | does **not** keep even that URL out: the next authorization naming it fetches its document and registers it afresh, enabled -- disable it instead |
| dynamic (`narvi_mcp_d_...`) | stops that `clientId` only | stops that `clientId` only |

An app that can present itself as another client comes back as a new one.
Each of its users would have to approve the new client, and until they do
it can do nothing:

- A **metadata-document** app is kept out by neither. Whoever controls its
  document's host can publish the document under another URL -- another
  path on that host, or the same path with a query added -- and the next
  authorization naming that URL fetches it and registers a new client,
  enabled, with no administrator involved. Its consent page names the same
  host its users already trusted. To keep such apps out, switch metadata
  documents off (next item), which pauses every one of them. There is no
  block by host: Narvi has no way to refuse every URL on one host while
  metadata documents stay on.
- A **dynamically registered** app is kept out by neither: while
  `NARVI_MCP_DCR_ENABLED=true` it can register afresh under a new
  `clientId`, and nothing ties that registration to the old one. To keep
  such apps out, switch dynamic registration off (next item), which pauses
  every one of them.
- A **pre-registered** client stays out, but an app that can also identify
  itself by a metadata document, or register itself, can come back that way
  while that mechanism is on.

**Every client of one registration mechanism** -- set
`NARVI_MCP_CIMD_ENABLED=false` (metadata-document clients) or
`NARVI_MCP_DCR_ENABLED=false` (dynamically registered clients) and restart
the control plane (configuration is read at boot only). **Next call:** each
such client is refused exactly as a disabled one is. A pause: nothing is
deleted, and switching the mechanism back on lets its clients carry on
(§43.15). Pre-registered clients are unaffected.

**The whole MCP surface** -- set `NARVI_MCP_ENABLED=false` and restart.
`POST /mcp`, the discovery documents and every `/oauth` route answer `503`;
the Settings routes above keep working, so every authorization can still be
listed and revoked.

## Sessions an app already started

An app whose user granted it "Act as you" (`mcp:write`) can start sessions,
and each one runs code in the user's repositories and spends on models
(§43.8). Every step above stops the app's **next** call, the next session
start included -- it does not stop a session the app already started, which
runs to its end like any other: no stop action exists yet, over MCP or REST.
What bounds a runaway app meanwhile is its brakes, both per authorization
and per control-plane replica: a burst of 30 calls to `POST /mcp`, then one
a second, answered `429` past that; and a burst of 5 calls to
`narvi_create_session`, then one a minute -- a retry counting as a call, so
the app starts at most that many sessions -- refused past that without
starting anything. Both refusals
are logged at WARN (`mcpauth: rate limited`, `mcp: session start refused by
the create brake`) with the `grant_id` and `client_id`, and neither is
audited.

To find what an app started, read the audit log: every session it started
has a `session.create` row whose `detail.mcp` names the authorization and
the client, and the session itself records `spawn_source = mcp`:

```sql
SELECT a.resource_id AS session_id, a.created_at, a.detail_json->'mcp' AS via
FROM audit_log a
WHERE a.action = 'session.create'
  AND a.detail_json->'mcp'->>'client_id' = '<clientId>'
ORDER BY a.created_at DESC;
```

(`detail_json->'mcp'->>'grant_id'` narrows it to one user's authorization,
the `id` the Connected apps lists answer.) The same stamp is on every other
audit row a change made through an app writes, including a
`session.repo_entitlement_denied` refusal.

## Revoke every authorization

- **Of one client:** delete the client (above). For a metadata-document
  client, disable it instead, then revoke its authorizations one by one
  through the members' Connected apps drawers (audited), or in SQL:
  `DELETE FROM mcp_oauth_grants WHERE client_id = (SELECT id FROM
  mcp_oauth_clients WHERE client_id = '<clientId>');` (not audited).
- **Of every client, deployment-wide:** there is no single API call. The
  audited way is member by member, through the admin routes above. In an
  emergency: `DELETE FROM mcp_oauth_grants;` -- every code and token cascades
  with its authorization, and every connected app gets `401` on its next call
  and `invalid_grant` on its next refresh; its users must approve it again.
  Not audited -- record it in the incident notes. Pair it with a
  cut-off above if the apps must not simply be approved again.

## When an app's users cannot authorize it

- **"This app has too many sign-ins waiting" (503):** the client already has
  `MCPMaxPendingAuthorizationRequestsPerClient` (100) authorization requests
  waiting for a decision (§43.14). Each expires ten minutes after it was made.
  A sustained flood of requests for one client keeps it at the cap. The cap
  bounds how many requests one client has waiting, not who fills them, and
  not the table (§43.14): while metadata documents or dynamic registration
  are on, a flood can name a new client each time -- another URL on a host
  it serves documents from, or a fresh registration -- each with an empty
  cap, and only the ten-minute expiry and the hourly expired-credential
  sweep limit how long each request stays. Switching the mechanism off
  (**Every client of one registration mechanism**, above) is the only hard
  bound. Each request the cap refuses is logged
  once, at WARN: `mcpauth: authorize refused` with
  `outcome=pending_request_cap`, the `client_id`, and `client_address` -- the
  network of the request that was refused, by the brakes' own key (one IPv4
  address, or one IPv6 `/48`; an IPv4 client reaching Narvi inside an IPv6
  address is named by its IPv4 address when the address says so, as the next
  entry lists) -- never its query or a cookie. A request that
  is stored is not logged, so these lines name who was refused, not who holds
  the places. To find the flood, count the lines per `client_address` for that
  `client_id` over the last few minutes: a flood sending faster than its
  requests lapse has its excess refused, so its networks are named again and
  again, while a person trying to sign in appears once or twice. Block those
  networks upstream. A flood paced to take each place just as it frees is
  refused only when someone else took a place first, so it may be named
  rarely or never, and the lines then name only the people it locks out: use
  the proxy's access log if the deployment has one, or cut the client off
  (above). Behind a proxy that hides client addresses, every line names the
  proxy.
- **"Too many requests from your network" (429), or a `429` from the token
  endpoint:** one client network (one IPv4 address, or one IPv6 /48) is over
  the authorization or token endpoint's brake (§43.14). The log line is
  `mcpauth: rate limited` with the `path` and the `client_address` -- never a
  token or the request's query. A refused refresh spends nothing: the app keeps
  its refresh token and renews once the brake refills (a few seconds). Every
  user behind the same address shares one bucket: a deployment behind a proxy
  that hides client addresses sees one network for everyone, and trusting a
  forwarded-for header is a deployment decision not yet made (§43.15). An IPv4
  client reaching Narvi inside an IPv6 address is keyed as its IPv4 address
  when the address says so (IPv4-mapped, the well-known NAT64 prefix
  `64:ff9b::/96`, IPv4-compatible, Teredo); behind a translator using a
  network-specific prefix, every IPv4 client shares that prefix's `/48`.

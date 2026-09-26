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

- **Settings → Integrations → MCP clients** (admin; `GET /api/mcp-clients`)
  lists every client of every kind: its internal `id`, its public `clientId`,
  its name, its kind -- `preregistered`, `metadata_document` (the `clientId` is
  the https URL of its metadata document) or `dynamic` -- and whether it is
  disabled.
- **Settings → Members & access → Connected apps** on a member's row (admin;
  `GET /api/members/{userID}/mcp-authorizations`) lists what one member has
  approved, with each app's `clientId`.
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

**A pre-registered client, or a dynamically registered one** -- delete it:
Settings → Integrations → MCP clients → Delete (`DELETE
/api/mcp-clients/{clientID}`, the internal `id`). Every authorization, pending
request, code and token issued to it goes with it. **Next call:** every
user's access token gets `401`; a refresh, a code exchange or an
authorization naming its `clientId` is refused as an unknown client. Audited:
`mcp_client.deleted`, plus one `mcp_authorization.revoked` (reason
`client_deleted`) per authorization it took. Permanent. A dynamically
registered app can register afresh -- under a new `clientId`, and only while
`NARVI_MCP_DCR_ENABLED=true` -- and its users would then have to approve it
again.

**A metadata-document client -- do not delete it to keep it out.** Its
`clientId` is its URL, and the next authorization naming that URL registers it
again, enabled. Disable its row instead; a disabled row is the block: it is
never swept, and its document is never fetched again (§43.15).

**Any single client, paused rather than deleted** -- disable its row. There
is no API for it; in SQL:

```sql
UPDATE mcp_oauth_clients SET disabled_at = now()
WHERE client_id = '<clientId>' AND disabled_at IS NULL;
```

**Next call:** every token issued to it gets `401`; its refresh gets
`invalid_client`; its authorization and consent pages show an error page and
never redirect; it may still give its tokens back (`POST /oauth/revoke`).
Nothing is deleted: every authorization stays listed, and each user can still
revoke theirs. Undo with `SET disabled_at = NULL`: the client carries on with
whatever has not lapsed meanwhile, with no new consent. A direct database
write is **not** audited -- record it in the incident notes.

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

## Revoke every authorization

- **Of one client:** delete the client (above). For a metadata-document
  client, disable it first, then revoke its authorizations one by one through
  the members' Connected apps drawers (audited), or in SQL:
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
  The log line is `mcpauth: authorize refused` with `outcome=pending_request_cap`
  and the `client_id`. A sustained flood of requests for one client keeps it
  at the cap -- the cap bounds the table, not who fills it -- so find the
  networks sending them (the `mcpauth: rate limited` lines below, or the
  proxy's own logs) and block them upstream, or cut the client off.
- **"Too many requests from your network" (429), or a `429` from the token
  endpoint:** one client network (one IPv4 address, or one IPv6 /48) is over
  the authorization or token endpoint's brake (§43.14). The log line is
  `mcpauth: rate limited` with the `path` and the `client_address` -- never a
  token or the request's query. A refused refresh spends nothing: the app keeps
  its refresh token and renews once the brake refills (a few seconds). Every
  user behind the same address shares one bucket: a deployment behind a proxy
  that hides client addresses sees one network for everyone, and trusting a
  forwarded-for header is a deployment decision not yet made (§43.15).

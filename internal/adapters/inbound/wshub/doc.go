// Package wshub holds the inbound WS hub for both the sandbox socket and
// the client socket (contract in §6), both mounted at the SAME route
// (GET /sessions/{sessionID}/ws), discriminated by ?type=sandbox vs
// ?type=client via handler.go's own NewHandler dispatcher.
//
// # The sandbox socket (§3.2)
//
// NewSandboxHandler (sandbox.go) backs ?type=sandbox (§6.1) -- the
// server-side mirror of internal/sandboxagent/wsbridge's client side. It
// implements the full 11-step handshake status-code table (see
// sandbox.go's own doc comment), then hands the live connection to
// readLoop (dispatch.go), which peeks each inbound frame's common
// envelope fields (type/gen/lastBootPhase/ackId), delivers a
// sessionactor.SandboxEvent to the session's own Actor
// (internal/app/sessionactor), and writes back a sandboxws.Ack when the
// Actor's reply carries a non-empty AckID. token.go implements the sandbox-
// bearer-token verify side (HashSandboxToken + verifySandboxToken) against
// sandboxes.token_hash (migrations/000015_sandbox_token_hash.up.sql).
//
// Outbound dispatch (§9.3, "e2e happy path"): commander.go's own
// SandboxRegistry is the single-recipient (not fan-out) live-connection
// registry that implements internal/app/ports.SandboxCommander --
// NewSandboxHandler registers each connection with it right before
// entering readLoop, and internal/app/sessionactor's own EnsureDispatched
// handling calls SandboxCommander.SendCommand to push a real sandboxws.
// Prompt onto a session's live connection. A concurrent SendCommand write
// and dispatch.go's own writeAck (running on the connection's own
// read-loop goroutine) are documented-safe per coder/websocket.Conn's own
// docs ("All methods may be called concurrently except for Reader and
// Read") -- verified directly via `go doc github.com/coder/websocket.Conn`
// during this Step's own design phase, not merely cited.
//
// # The client socket (§6.2)
//
// NewClientHandler (client.go) backs ?type=client (§6.2) -- see that
// file's own top comment for the complete handshake outcome table
// (sessionID/session-row validation -> Accept (deliberately WITHOUT
// sandbox.go's InsecureSkipVerify -- this socket is browser-facing, same-
// origin enforcement matters here, see client.go's own contrast comment)
// -> bounded subscribe-frame read -> ws-token verify (close 4001 re-auth /
// 4002 expired) -> insertion into the shared *Hub's session-keyed map
// (Hub.Register: so no live broadcast for this session is ever lost from
// that instant on -- it queues, buffered, even though nothing drains it
// yet) -> a single `subscribed` reply (SubscribedPayload) -> only once
// THAT reply's own write has returned does broadcast delivery to this
// connection actually begin (Hub.StartDelivery starts the drain goroutine
// that writes queued/future broadcasts to the wire) -> a read loop
// handling `fetch_history` cursor-paginated replay, rate-limited
// per-connection via ClientFetchHistoryMinInterval).
//
// Splitting "register in the map" from "start writing broadcasts to the
// wire" into those two separate calls (both in client.go) is deliberate,
// not incidental (F8 audit fix): registering the map entry first still
// closes the original lost-broadcast window (a connection not yet in the
// map simply isn't there to receive a Broadcast), while delaying the drain
// goroutine's start until after the subscribed reply's own conn.Write
// returns closes a narrower, inverse window that a single combined
// register-and-start-draining call left open -- a broadcast's drain
// goroutine and this handler's own subscribed-reply write both call
// conn.Write, and coder/websocket only serializes those calls against each
// other (no data race), it does not order them, so whichever one reached
// the wire first was whichever won that race. Deferring the drain
// goroutine's start until after the subscribed write already returned
// makes that ordering a plain program-order guarantee instead.
//
// *Hub (also in client.go) is the in-process, session-keyed connection
// registry that implements internal/app/ports.EventBroadcaster -- the
// session actor's own successful transact commits call Hub.Broadcast,
// which fans a raw stored event payload out to every subscribed
// connection for that session, sent exactly as stored (§6.2: no wrapper
// envelope for a live broadcast frame). A concurrent, periodic
// server-initiated ping (pingClientLoop, ClientWSPingInterval) is this
// same handler's own idle-liveness check -- an unanswered ping closes the
// connection with 4003 ("idle timeout"), the third custom close code
// alongside 4001/4002.
//
// # Honest gaps this package documents rather than papers over
//
//   - X-Sandbox-ID is checked for PRESENCE only, never verified against a
//     real value -- no Step yet wires a real provider-assigned
//     sandbox-instance id into the sandbox's own environment (matching
//     internal/sandboxagent/wsbridge/doc.go's own identical gap on the
//     client side, and §6.4's NARVI_IMAGE_DIGEST gap).
//   - ErrSessionActorElsewhere (this process doesn't hold the session's
//     advisory lock) maps to 503, not one of wsbridge's own 4 "fatal"
//     statuses (401/403/404/410) -- deliberately, so the sandbox-agent's
//     own already-built exponential-backoff reconnect loop is what
//     recovers, since no cross-pod routing/proxy exists anywhere in the
//     codebase yet. This is a known, honest, currently-unaddressed
//     limitation, not a permanent solution -- not scoped to any specific
//     later Step by the plan.
//   - Sandbox token MINTING (generating a fresh token + writing its hash
//     at spawn time) now exists (§9.3's own tryPlanSpawn,
//     internal/app/sessionactor/dispatch.go, calling platform.
//     GenerateToken + UpsertSandboxForSpawn) -- HashSandboxToken was
//     exported specifically so that caller could reuse this package's own
//     hashing convention rather than reinventing it, and it does.
//     verifySandboxToken's own nil-token_hash bypass (any non-empty
//     bearer token accepted while token_hash is NULL) therefore no longer
//     "covers every row today" -- every row this Step's own real spawn
//     path creates always sets token_hash. The bypass remains, unchanged,
//     as defense-in-depth for a legacy or manually-inserted row that
//     genuinely has no hash set, not because it is the normal path any
//     real spawn takes. HashSandboxToken is also this Step's own second
//     caller, internal/adapters/inbound/httpapi/scmcredentials.go, which
//     deliberately does NOT copy this bypass -- see that file's own
//     verifySandboxBearerToken doc comment for why (it hands back a real,
//     live OAuth credential on success, a materially higher-stakes
//     endpoint than gating a WS connection).
//   - Suspect-state recovery-during-grace ("any liveness signal during
//     grace returns to previous state", §3.2) is now real (§3.2,
//     "two-phase terminalization") -- a Suspect sandbox reconnecting
//     through this package IS allowed (IsDeadSandboxStatus(Suspect) is
//     false), and internal/app/sessionactor's own handleSandboxEvent now
//     both persists/bumps liveness for its events AND, when the row still
//     carries a pre_suspect_status, attempts the real recovery transition
//     back to that previously-live state in the same pass -- see that
//     file's own top comment for the full mechanics. This package itself
//     needed no change for that: it already let a Suspect reconnect
//     through unmodified.
//   - Real user auth/identity now exists (§13.1, "auth v1") -- REST
//     ws-token minting (internal/adapters/inbound/httpapi's own
//     MintWSToken) is gated behind internal/adapters/inbound/auth.
//     Middleware and scopes ws_tokens.user_id to the real authenticated
//     caller. This package's own client-WS subscribe-time verification
//     (client.go) was UNCHANGED by that Step: it only checked the
//     presented ws-token's hash against ws_tokens, nothing
//     per-participant beyond that -- participants stayed completely
//     untouched (SubscribedPayload.participants was always an empty
//     array).
//   - UPDATE (§8.11, "multiplayer presence"): the gap immediately above
//     is closed for LIVE presence specifically. verifyClientToken now
//     returns row.UserID alongside its bool, threaded into Hub.Register
//     as a new userID parameter Hub tracks per connection; Hub.
//     Participants(sessionID) reports the distinct set of real users
//     currently subscribed, and NewClientHandler resolves that set
//     (postgres.UserStore.GetByID, best-effort -- one unresolvable id is
//     dropped, never fails the whole reply) into SubscribedPayload.
//     participants as this connection's own subscribe reply is built --
//     so a fresh subscribe (including every reconnect) always reports
//     who else, if anyone, is live on this session right now. This is
//     DELIBERATELY separate from, and does NOT populate, the durable
//     `participants` Postgres table (migrations/000011) postgres.
//     ParticipantStore wraps -- that table backs §13.3's own "member on
//     own/joined" authorization predicate, a different, still-unwired
//     concern (see ParticipantStore's own doc comment, unchanged by
//     this). Live presence and "has this user ever joined this session"
//     are two different questions with two different lifetimes; this
//     Hub answers only the first, in-process and ephemeral by
//     construction (cleared the instant a connection drops, exactly
//     like every other Hub bookkeeping this package already loses on
//     process restart -- the same honest "cross-pod fan-out is NOT
//     solved here" scope this package's own Hub doc comment already
//     states applies here too: presence is only ever visible to a
//     subscriber connected to the SAME control-plane process that also
//     holds the OTHER participants' connections).
//   - Cross-pod broadcast fan-out is NOT solved here: *Hub only ever
//     reaches connections registered in the SAME process as the actor
//     that persisted the event -- the same class of honest gap as
//     ErrSessionActorElsewhere/503 above, not a Postgres LISTEN/NOTIFY or
//     message-bus solution, genuinely out of scope for this Step.
package wshub

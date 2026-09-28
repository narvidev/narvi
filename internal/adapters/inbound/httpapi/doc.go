// Package httpapi holds the REST adapter serving the web UI and external
// clients (§6.3): "The BFF-facing routes: sessions CRUD/create, events,
// artifacts, secrets, environments, automations, uploads, ws-token."
// §6.2's own plan row explicitly narrows that to exactly
// "create/get/events/artifacts" (+ ws-token, named separately by §6.2) --
// secrets/environments/automations/uploads are NOT this Step's job (see
// contracts/README.md's own scope note on rest/v1/dtos.schema.json).
//
// # Routes (all under /api/sessions, mounted by cmd/control-plane/main.go,
// behind internal/adapters/inbound/auth.Middleware -- see
// that package's own doc.go)
//
//   - POST /api/sessions -- create.go's CreateSession: decodes
//     restdtos.CreateSessionRequest (body capped via http.MaxBytesReader,
//     see create.go's own doc comment), validates repos is non-empty,
//     inserts via SessionStore.Create, responds 201 with restdtos.Session.
//   - GET /api/sessions?filter=mine|all&limit= -- listsessions.go's
//     ListSessions (§12.2 item 1): 200 with restdtos.ListSessionsResponse,
//     most-recently-updated first. filter defaults to "mine" ("created or
//     joined", SessionStore.List's own mine_only join); 400 on any other
//     filter value.
//   - GET /api/sessions/{sessionID} -- get.go's GetSession: 404 if not
//     found, else 200 with restdtos.Session.
//   - GET /api/sessions/{sessionID}/status -- sessionstatus.go's
//     GetSessionStatus (technical plan §43.20): 404 if not found, else 200
//     with restdtos.SessionActivity -- the session's live activity, derived
//     from its turn queue, its push/PR delivery, the work armed to create a
//     turn and its human gates in one snapshot (never sessions.status), and
//     a suggested delay before the next read. No events: the transcript is
//     the events route below. ?waitSeconds=N makes it the bounded wait
//     (row 182's piece (b)): the same read at once and every
//     MCPWaitPollInterval until settled or N seconds (clamped to
//     MCPWaitMaxDuration) have passed, on the replica's one
//     sessionactivity.Waiter, answered with restdtos.SessionActivity.Wait
//     set; 400 on a negative or malformed value.
//   - GET /api/sessions/{sessionID}/result -- sessionresult.go's
//     GetSessionResult (technical plan §43.20, row 182's piece (c)): 404 if
//     not found, else 200 with restdtos.SessionOutcome -- the activity, the
//     last run with its summary (its final text, capped), the pull requests
//     the session opened or reviews (a record naming none -- a creation
//     suppressed in shadow mode, or a row it cannot read -- listed apart,
//     never failing the result), and each one's review state read from
//     the record, all in one read-only snapshot; each assessed verdict's
//     freshness read live (reviewfreshness.Assess, the merge path's own
//     read and comparison), all within one budget; and how long to wait
//     before reading it again. No events.
//   - GET /api/sessions/{sessionID}/events?cursor=&limit= -- events.go's
//     ListEvents: 404 if the session doesn't exist, else 200 with
//     restdtos.EventsResponse (shaped exactly like client-ws/v1's own
//     FetchHistoryResponse -- the same underlying EventStore.
//     ListForSession read the client WS hub's own fetch_history uses, one
//     implementation, two callers).
//   - GET /api/sessions/{sessionID}/artifacts -- artifacts.go's
//     ListArtifacts: 404 if the session doesn't exist, else 200 with
//     restdtos.ArtifactsResponse.
//   - POST /api/sessions/{sessionID}/ws-token -- wstoken.go's MintWSToken:
//     404 if the session doesn't exist, else mints a fresh token
//     (platform.GenerateToken), stores only its hash
//     (platform.HashToken), and responds 200 with
//     restdtos.WSTokenResponse{Token: <plaintext>, ExpiresAt}.
//
// # The auth gap -- resolved (§13.1, "auth v1")
//
// Every route above is now mounted behind internal/adapters/inbound/auth.
// Middleware in cmd/control-plane/main.go: a request with no valid
// narvi_auth_session cookie gets 401 before reaching any handler in this
// package. Both of §6.2's own honest gaps are directly closed as a
// result:
//
//   - CreateSession now inserts created_by with the REAL authenticated
//     caller's id (platform.UserFromContext, resolved via
//     authenticatedUserID in helpers.go) instead of always NULL.
//   - MintWSToken now scopes ws_tokens.user_id to the REAL authenticated
//     caller the same way, instead of always NULL.
//
// # §9.3 ("e2e happy path") updates
//
// CreateSession now actually PERSISTS req.Repos (design decision 1,
// migrations/000018_session_repos.up.sql) and, when req.Prompt is
// non-nil, inserts a Turn row in the SAME transaction as the session
// insert, then fires EnsureDispatched on that session's own actor (see
// create.go's own doc comment for the full sequencing) -- its own
// constructor signature grew a *pgxpool.Pool, a *postgres.TurnStore, and
// a *sessionactor.Registry parameter as a result.
//
// A SIXTH route is added, mounted OUTSIDE this package's own /api/sessions
// group and outside auth.Middleware entirely (see scmcredentials.go's own
// doc comment for why):
//
//   - POST /sessions/{sessionID}/scm-credentials -- scmcredentials.go's
//     own ScmCredentials: a sandbox-bearer-token-authenticated endpoint
//     (mirrors wshub's own header-bearer-token handshake precedent, NOT
//     §13.1's cookie-based auth.Middleware) that decrypts and hands
//     back the session's own created_by user's GitHub OAuth access token
//     as a git-over-https credential (§5.2). This is the control-plane
//     side of the wire contract internal/sandboxagent/credentials.
//     CPClient (§6.4) already built and tested the client side of.
//
// # §3.2 ("snapshots & restore") updates
//
// A SEVENTH route is added, mounted the SAME way scm-credentials is
// (outside /api/sessions and outside auth.Middleware entirely):
//
//   - POST /sessions/{sessionID}/snapshot -- snapshotmint.go's own
//     SnapshotMint: another sandbox-bearer-token-authenticated endpoint,
//     called by cmd/sandbox-agent's real HandleSnapshot (via
//     internal/sandboxagent/snapshotclient) once the control plane sends
//     a "snapshot" WS command (design decision 1). Calls the real
//     ports.SandboxProvider.TakeSnapshot for the session's live sandbox
//     and hands back the real resulting snapshot id, which the sandbox
//     then reports back over the WS bridge as a CRITICAL "snapshot_ready"
//     event (design decision 2's own full round-trip reasoning).
//
// # §3.3 ("turn recovery") update
//
// An EIGHTH route is added, INSIDE the /api/sessions group (so it shares
// that group's own auth.Middleware gate, unlike scm-credentials/snapshot
// above):
//
//   - POST /api/sessions/{sessionID}/turns -- turn.go's own CreateTurn:
//     §8.7's "relaunch-and-resume" REST API. Enqueues a new Pending turn
//     on an EXISTING session (404 if it doesn't exist; 409 if a turn is
//     already in flight for it), then fires the SAME GetOrSpawn +
//     Send(EnsureDispatched{}) sequencing CreateSession already uses for
//     its own optional first turn. No "resume" flag exists anywhere on
//     this route: sessions.opencode_conversation_id already persists
//     across turns, and dispatch.go's own buildPromptPayload already
//     carries it on every Prompt automatically -- so the new turn
//     transparently continues whatever OpenCode conversation the session
//     already has, the instant it dispatches.
//
// Wiring participants/presence (§8.11) is still untouched -- a distinct,
// not-yet-scoped concern.
//
// # §5.1 ("webhook toolkit") update
//
// No new route is added by this Step -- it adds no concrete provider
// wiring at all. CreateSession's own doc comment above (create.go) is
// split in two: everything after decoding the request body is now
// createSessionCore (unexported as of this Step -- §8.10 exports it,
// see below), taking an already-decoded restdtos.CreateSessionRequest
// plus a NULLABLE creator (pgtype.UUID, Valid == false for "no human
// caller"). CreateSession itself is now a thin wrapper: decode body ->
// authenticatedUserID (UNCHANGED, still hard-required for this browser/
// REST route) -> createSessionCore -> write the JSON response. This is a
// pure refactor for this route -- every existing test in this package's
// own _test.go files passes unchanged. The point of the split is reuse:
// §8.2/§8.10's own GitHub/Slack/Linear webhook ingress handlers call
// createSessionCore directly, with their own already-verified,
// already-decoded request and a NULL creator (no cookie, no human) --
// never this package's own CreateSession, which stays browser-only.
// Whether those ingress handlers end up living in THIS package (as new
// files alongside create.go/get.go/events.go/artifacts.go/wstoken.go) or
// export createSessionCore to reach it from their own separate packages
// is left open by this Step -- see the §8.10 update immediately below
// for how that question was actually resolved.
//
// # §8.10 ("Slack ingress") update
//
// internal/adapters/inbound/slack (a separate package, mirroring
// github/linear's own reserved package-per-surface shape) is §8.10's
// own new Slack webhook ingress adapter -- see that package's own doc.go.
// It needs to reach createSessionCore from outside this package, so this
// Step exports it (CreateSessionCore) and its error type (CreateSessionError,
// with exported Status/Message fields) -- a pure rename, no behavior
// change; every call site and test in THIS package keeps compiling
// unchanged (create.go's own doc comment above CreateSessionCore has the
// full reasoning).
//
// This Step also adds two other, independent pieces used by those same
// future ingress endpoints, neither wired to a concrete provider yet:
//
//   - internal/platform/webhooksig.go: a generic, provider-agnostic
//     raw-body HMAC-SHA256 signature-verification helper
//     (VerifyWebhookSignature / VerifyWebhookTimestamp) -- see that
//     file's own doc comment for the full GitHub-vs-Slack shape
//     reasoning and the HMACWebhookSecret design-question writeup.
//   - migrations/000027_webhook_deliveries.up.sql +
//     postgres.WebhookDeliveryStore.Claim: the atomic
//     INSERT ... ON CONFLICT dedupe/coalescing claim §5.1 calls for,
//     keyed on (provider, delivery_id).
//
// # Reconciliation update: createSessionCore exported and split for tx
// support
//
// Independent webhook-ingress adapters each ended up needing "create a
// session (+ optional turn), then post-commit trigger dispatch" from
// OUTSIDE this package -- CreateSessionCore/CreateSessionError are already
// exported above (Status/Message fields, same Error()
// method), so no further export/rename is needed here. At least one
// such adapter also needs to create a session+turn while ALREADY holding
// an unrelated lock on its own already-open transaction (e.g. an atomic
// per-resource claim taken via SELECT ... FOR UPDATE) -- calling the
// pool-based CreateSessionCore from inside that critical section would
// open a SECOND, simultaneous connection out of the same pool while the
// first transaction's own connection is still held, a genuine
// connection-pool exhaustion/deadlock risk under real concurrent load.
// Rather than leave every such caller to duplicate CreateSessionCore's own
// repo/pathScope/mockConfig validation and session/turn-insert logic by
// hand, CreateSessionCore is now split into three pieces (create.go):
//
//   - validateCreateSessionRequest: every in-memory-only check
//     (repos non-empty, each repo's Name/Url/Branch, pathScope,
//     mockConfig.contractsPath) that used to run inline, extracted so it
//     can be called from two places without duplicating its logic by
//     hand.
//   - CreateSessionOnTx: everything CreateSessionCore used to do AFTER
//     validation, up to and including the optional turn insert, taking
//     an ALREADY-OPEN pgx.Tx the CALLER owns entirely -- no Begin/Commit/
//     Rollback inside it at all. It calls validateCreateSessionRequest
//     itself, at its own top, before touching tx -- necessary because it
//     is also called directly by callers that already hold their own
//     open transaction and have not necessarily validated the request
//     first. Returns hasPrompt explicitly so the caller knows, ONCE ITS
//     OWN outer transaction has committed, whether a dispatch trigger is
//     needed.
//   - TriggerDispatch: the exact "GetOrSpawn + Send(EnsureDispatched{})"
//     fire-and-forget pattern (warn-log-on-error, never returned to the
//     caller), extracted so every caller triggers dispatch identically
//     post-commit.
//   - CreateSessionCore itself: now validateCreateSessionRequest ->
//     pool.Begin -> CreateSessionOnTx -> tx.Commit -> (if hasPrompt)
//     TriggerDispatch. The explicit pre-Begin validation call is a
//     correction, not just a refactor: an earlier version of this split
//     called pool.Begin FIRST and left validation to run only inside
//     CreateSessionOnTx, i.e. AFTER the transaction/connection was
//     already open -- silently breaking the pre-existing trust-boundary
//     invariant (create.go's own doc comments above, "a rejected repo
//     spec never reaches Postgres at all") for every malformed request on
//     this pool-based path. With the pre-Begin call restored, the
//     validate -> insert -> commit sequencing is byte-for-byte the same
//     as CreateSessionCore performed before the tx-support split, so
//     CreateSession (the HTTP handler) and every existing
//     CreateSessionCore test are unaffected. A caller already holding its
//     own open transaction calls CreateSessionOnTx directly, inline on
//     that same connection, and calls TriggerDispatch itself once its own
//     outer transaction commits -- never CreateSessionCore, which is only
//     safe for a caller with no transaction of its own yet.
//
// # §8.2 ("GitHub ingress") update
//
// The real webhook handler lives in its own package,
// internal/adapters/inbound/github (mounted at POST /webhooks/github,
// cmd/control-plane/main.go) -- NOT inside this package, since it is a
// genuinely separate protocol adapter (signature verification, GitHub's
// own event/payload shapes, per-PR coalescing), not one more browser/REST
// route family. bot.go adds two small EXPORTED wrappers for that package's
// use: CreateSessionForBot (forwards to CreateSessionCore with a NULL
// creator) and CreateTurnForBot (mirrors CreateTurn's own
// lock-then-insert-then-dispatch sequencing, minus its REST-specific
// hasOpenTurn 409 gate -- see bot.go's own doc comment for why that gate
// doesn't apply to a bot-ingress caller enqueuing a coalesced backlog of
// turns on one shared session).
//
// github's own per-PR coalescing (coalesce.go, that package) uses
// CreateTurnForBot directly for its REUSE branch. Its WINNER branch, since
// the reconciliation update above landed, calls CreateSessionOnTx directly
// (inline on the SAME tx its own claim-row lock already holds) rather than
// duplicating any of CreateSessionOnTx's own validation/insert logic by
// hand, then calls TriggerDispatch itself once that outer transaction has
// committed and hasPrompt is true -- exactly the "already holding an
// open transaction of its own" caller shape CreateSessionOnTx's own doc
// comment (create.go) describes. Neither branch calls CreateSessionForBot
// for the winner path: that wrapper is pool-based (CreateSessionCore
// underneath), and opening a SECOND, separate transaction from the same
// pool while the claim-row lock's own transaction is still open would
// risk exhausting the connection pool under enough concurrent same-PR
// mentions. CreateSessionForBot itself is untouched and still fully
// tested (bot_integration_test.go) as a general-purpose, no-coalescing
// bot-session entry point for a caller that isn't simultaneously holding
// a transaction open of its own.
//
// # §13.2 ("identities + full RBAC", §13.3) update
//
// Every state-changing REST handler in this package now calls the real
// internal/domain/authz.Authorize BEFORE its own effect:
//
//   - CreateSession (create.go): ActionCreateSession, unconditional for
//     admin/maintainer/member -- viewer gets 403 before the request body
//     is even decoded.
//   - CreateTurn (turn.go): ActionPromptSession, own/joined-gated for
//     member (resolved via a plain sessions.Get + participants.Exists,
//     mirroring planauthz.go's own identical precedent), unconditional for
//     admin/maintainer, never for viewer.
//   - ApprovePlan/RejectPlan (planapprove.go): unchanged CALL SHAPE
//     (canActOnPlan, still (bool, error)) but that predicate itself
//     (planauthz.go) now renders its verdict via authz.Authorize instead
//     of its own bespoke rule set -- see that file's own doc comment.
//
// helpers.go's new authorize(w, r, action, resource) bool is the shared
// "resolve the context actor, call Authorize, write 403/500" plumbing
// CreateSession/CreateTurn both use.
//
// Every one of these four handlers' own underlying state change --
// CreateSessionOnTx (create.go), CreateTurnCore (turn.go),
// DecidePlanOnTx (decideplan.go) -- also now writes one audit_log row
// (postgres.AuditLogStore, audit.go's recordAuditLog) inside the SAME
// transaction as the change (§13.3), for EVERY caller, not just these
// REST handlers: CreateSessionOnTx's own webhook-ingress callers
// (internal/adapters/inbound/{github,slack,linear}) and DecidePlanOnTx's
// own Slack/Linear plan-verdict callers get the identical treatment,
// actor_user_id NULL for their bot-attributed changes -- mirroring
// sessions.created_by/plans.decided_by's own pre-existing NULL-for-bot
// convention exactly, never a fabricated "system user" row.
//
// A defense-in-depth viewer guard ("viewers never gain PR-reviewer
// attribution or git identity on session artifacts", §13.3) lives OUTSIDE
// this package, in internal/app/sessionactor (githubtoken.go's
// creatorMayGetPRAttribution, called from pushpr.go's createPRBestEffort)
// -- distinct from, and in addition to, ActionCreateSession above already
// refusing a viewer at session-creation time.
//
// # §13.2 ("identities + full RBAC", §13.2) second-half update
//
// Identity auto-linking (the actual DECISION/persistence logic) and the
// magic-link consume flow both live OUTSIDE this package -- internal/app/
// identitylink (Resolve/Consume, the I/O-performing orchestrator) and
// internal/adapters/inbound/identitylink (the magic-link consume HTTP
// handler), wired into internal/adapters/inbound/{slack,linear} at the
// point an inbound event first names an unknown provider identity. See
// those packages' own doc comments for the complete design.
//
// This package's OWN new addition is members.go: the backend-only members
// API §13.2/§13.3 call for -- GET /api/members (role + linked identities +
// system-wide pending-link state), PATCH /api/members/{userID}/role,
// POST/DELETE .../identities (admin manual link/unlink, §13.2 point 5),
// and GET /api/audit-log -- every one of them admin-only
// (domain/authz.ActionManageMembers). The actual Settings -> Members UI
// is Phase 7 (§13.4) and still out of scope.
package httpapi

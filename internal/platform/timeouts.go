// This file (timeouts.go) is the single source of truth for timeout/
// interval values in the system (§5.4, §11: "every new timeout/interval
// goes in platform/timeouts.go... no timeout literal anywhere else in the
// codebase"). The lint rule in tools/lint/narvichecks/notimeliteral
// enforces that by forbidding time.Duration unit literals (time.Second,
// time.Hour, ...) everywhere except this package and _test.go files.
//
// PR-02 scope is deliberately narrow: exactly the two invariant chains
// §5.4 names (the PR title's own parenthetical: "provider cap > supervisor
// > bridge > SSE", plus the HTTP-client/cold-start and
// first-connect/image-pull pairs from §4.1/§3.2). Timeouts consumed by
// later PRs (heartbeat intervals, terminal_grace, ws-token TTL, HMAC
// window, reconciler interval, ...) are added by the PR that first
// consumes them, not speculatively here.

package platform

import (
	"errors"
	"fmt"
	"time"
)

// MinTimeoutMargin is the minimum gap Validate requires between every
// adjacent pair in the timeout hierarchy (§5.4: "each with explicit
// margin"). Not given an explicit figure in the plan; 30s is a round,
// conservative floor relative to a hierarchy whose links span from
// seconds (SSE inactivity) to hours (provider hard cap).
const MinTimeoutMargin = 30 * time.Second

// Timeouts is the single struct holding every timeout/interval this PR
// covers, wired into two independent invariant chains:
//
//	Chain A (provider cap > supervisor > bridge > SSE):
//	  ProviderHardCap > SupervisorTurnCap > TurnDeadline > SSEInactivityTimeout
//
//	Chain B (two independent pairs, §4.1 / §3.2):
//	  ProviderHTTPClientTimeout > ProviderWorstColdStart
//	  FirstConnectBudget        > ImagePullBootP99
//
// Validate checks every pairwise link in both chains and requires at
// least MinTimeoutMargin of headroom, returning ALL violations found
// (joined), not just the first.
type Timeouts struct {
	// --- Chain A: provider cap > supervisor turn cap > CP turn_deadline > OpenCode SSE inactivity ---

	// ProviderHardCap is the absolute ceiling a sandbox provider enforces
	// on a running sandbox. §5.4 gives this explicitly: 2h.
	ProviderHardCap time.Duration

	// SupervisorTurnCap's CURRENT role is an invariant-chain bound ensuring
	// config sanity only: Validate below checks ProviderHardCap >
	// SupervisorTurnCap > TurnDeadline as a pairwise VALUE comparison, and
	// this field is otherwise referenced nowhere else in the codebase --
	// there is no real-time code path that reads a turn's own start time
	// and compares it against this value to actually terminate anything.
	// TurnDeadline's own already-armed named timer (handleTurnDeadlineTimer,
	// app/sessionactor/timerfired.go) is what actually terminates a
	// runaway turn in every case where the timer pump itself is healthy,
	// since TurnDeadline (60m) always fires strictly before this field's
	// own value (90m) would. This field remains reserved as a genuine
	// backstop for the (currently unhandled) case where turn_deadline
	// itself fails to fire -- a future periodic sweep across active turns
	// (a reconciler/health-check mechanism, not built today) could use it
	// for that -- but no such independent, real-time enforcement exists
	// yet; do not read this field's presence, its value, or its Validate()
	// check as proof one does. Not given an explicit value in the plan;
	// chosen as 90m, well below the 2h provider cap.
	SupervisorTurnCap time.Duration

	// TurnDeadline is the CP's turn_deadline named persistent timer
	// (§3.3 "Dispatch arms turn_deadline"; §2 "Named persistent timers").
	// Not given an explicit value in the plan; chosen as 60m, below
	// SupervisorTurnCap, so the per-turn deadline always fires before the
	// coarser supervisor cap would.
	TurnDeadline time.Duration

	// SSEInactivityTimeout is the OpenCode SSE inactivity timeout (§7:
	// "SSE inactivity timeout configurable (default 120s)").
	SSEInactivityTimeout time.Duration

	// --- Chain B: provider HTTP client timeout / cold start, first-connect budget / image pull+boot ---

	// ProviderHTTPClientTimeout is the HTTP client timeout used for calls
	// to the sandbox provider's API. §4.1: "The provider HTTP client
	// timeout MUST exceed the provider's worst cold-start." Not given an
	// explicit value; chosen as 5m, comfortably above ProviderWorstColdStart.
	ProviderHTTPClientTimeout time.Duration

	// ProviderWorstColdStart is our estimate of the worst-case provider
	// cold-start latency. §4.1: "Modal cold scheduling alone can take
	// 220s+" — taken as the stated floor.
	ProviderWorstColdStart time.Duration

	// FirstConnectBudget is the liveness budget covering provider cold
	// start + boot before the first sign of life (see
	// internal/domain/sandbox/liveness.go's EvaluateConnectingTimeout: this
	// single ceiling applies across the whole Spawning/Connecting/Booting
	// span from spawn until the FIRST liveness signal arrives; every signal
	// after that -- including a boot-progress report emitted mid-boot --
	// re-arms the watchdog and switches it onto the shorter
	// SteadyHeartbeatBudget for all subsequent checks). §3.2 gives this
	// explicitly: "first_connect_budget (default 240s, covers provider
	// cold start + boot)".
	//
	// Audit-remediation note (config/platform-hardening batch): "cold
	// start + boot" here is read as ONE ceiling over the whole
	// pre-first-signal span, not a literal sum of ProviderWorstColdStart
	// (220s, §4.1's own floor for provider scheduling ALONE) plus
	// ImagePullBootP99 (90s, this codebase's own invented sub-phase
	// estimate) stacked sequentially on top of it. Read additively, the
	// two would total 310s against this field's own 240s value -- but the
	// plan text does not actually say the two phases are sequential and
	// non-overlapping, and reading them that way would demand raising this
	// field past its own §3.2-mandated 240s value, which is not a call to
	// make unilaterally in a bundled audit batch. See ImagePullBootP99's
	// own doc comment for the matching note, and Validate()'s
	// "FirstConnectBudget > ImagePullBootP99" check for the objectively-
	// correct, deliberately weaker statement this ambiguity leaves
	// available.
	FirstConnectBudget time.Duration

	// ImagePullBootP99 is our estimate of the p99 latency of the
	// image-pull-and-boot sub-phase that FirstConnectBudget's own single
	// pre-first-signal ceiling must clear with margin. Not given an
	// explicit figure in the plan; chosen as 90s.
	//
	// Audit-remediation note (config/platform-hardening batch):
	// deliberately NOT modeled as additional time layered on top of
	// ProviderWorstColdStart's own 220s floor (§4.1: "Modal cold
	// scheduling alone can take 220s+") -- this codebase has no evidence
	// the two are sequential/non-overlapping rather than this sub-phase
	// being nested within (a portion of) that same cold-start window, and
	// FirstConnectBudget's own 240s value is §3.2-mandated, not something
	// this invented estimate should be allowed to force upward. Treat this
	// as an estimate of the boot sub-phase's own worst case, understood to
	// fit within whatever headroom the single 240s ceiling leaves once
	// cold start has resolved -- a conservative, self-contained sanity
	// floor, not one leg of a two-leg sum. See FirstConnectBudget's own
	// doc comment above for the fuller reasoning.
	ImagePullBootP99 time.Duration

	// --- PR-06 standalone additions: no ordering relationship with the
	// two chains above, so (per that PR's own instructions) not wired into
	// a fake invariant link — just plain fields with sensible defaults.

	// HMACWindow is the internal-auth HMAC freshness window (§5.2,
	// explicit: "bearer timestamp.signature, 5-min window, fail closed").
	HMACWindow time.Duration

	// ShutdownGracePeriod bounds how long `narvi serve` waits for
	// in-flight requests to drain (via http.Server.Shutdown) after
	// receiving SIGINT/SIGTERM before giving up. Not specified in the
	// plan; chosen as 10s (invented).
	ShutdownGracePeriod time.Duration

	// HealthCheckTimeout bounds how long the /health handler waits on
	// pool.Ping before reporting unhealthy, so a stuck DB never hangs the
	// handler past this. Not specified in the plan; chosen as 2s
	// (invented).
	HealthCheckTimeout time.Duration

	// --- §3.2 standalone additions: no ordering relationship with the
	// two chains above (or with the PR-06 additions), so — per that PR's
	// own precedent — just plain fields with sensible defaults, not wired
	// into a fake invariant link.

	// SteadyHeartbeatBudget is the liveness budget for a sandbox that has
	// already shown at least one sign of life (heartbeat or boot-progress
	// ping), distinct from FirstConnectBudget above. §3.2 gives this
	// explicitly: "steady_heartbeat_budget (default 90s; heartbeats every
	// 30s)".
	SteadyHeartbeatBudget time.Duration

	// TerminalGracePeriod is how long a sandbox stays in "suspect" before
	// a watchdog's silence/timeout is treated as genuinely dead (§3.2:
	// "two-phase terminalization: a watchdog never writes failed directly.
	// It writes suspect and arms terminal_grace (default 60s)").
	TerminalGracePeriod time.Duration

	// CircuitBreakerWindow is the sliding window the sandbox spawn circuit
	// breaker counts permanent failures within. §3.2 gives this explicitly:
	// "3 permanent spawn failures within 5 min blocks spawning". The
	// companion threshold (3) is a plain int, not a duration, so it lives
	// as a named constant in domain/sandbox instead of here.
	CircuitBreakerWindow time.Duration

	// SpawnCooldown is the minimum interval between spawn attempts (bypassed
	// for failed/stopped sandboxes). Not given an explicit figure in the
	// plan; chosen as 30s.
	SpawnCooldown time.Duration

	// SpawnReadyWait is how long a sandbox that reports "ready" without an
	// active WebSocket is given to reconnect before a fresh spawn is
	// considered. Not given an explicit figure in the plan; chosen as 60s.
	SpawnReadyWait time.Duration

	// SpawnStuckTimeout is the max time a sandbox may remain in a
	// spawning/connecting-style status before it is treated as dead (an
	// interrupted spawn) and a fresh spawn is allowed. Not given an explicit
	// figure in the plan; chosen as 120s.
	SpawnStuckTimeout time.Duration

	// InactivityTimeout is how long a ready, non-processing sandbox may go
	// without activity before it is stopped (and snapshotted). Not given an
	// explicit figure in the plan; chosen as 10min.
	InactivityTimeout time.Duration

	// InactivityExtension is the additional time granted (with a warning)
	// when the inactivity timeout fires but clients are still connected.
	// Not given an explicit figure in the plan; chosen as 5min.
	InactivityExtension time.Duration

	// InactivityMinCheckInterval is the minimum interval between inactivity
	// alarm checks. Not given an explicit figure in the plan; chosen as 30s.
	InactivityMinCheckInterval time.Duration

	// --- §2 standalone additions: no ordering relationship with
	// either invariant chain above (or with the PR-06/§1 additions),
	// so — per those additions' own precedent — plain fields with
	// sensible defaults, not wired into a fake invariant link.

	// ActorIdleTTL is how long a session actor may go without processing
	// any command before it evicts itself (§2, explicit: "evicts after
	// idle TTL (default 30 min without commands or connected clients)").
	ActorIdleTTL time.Duration

	// TimerPumpInterval is how often the per-pod timer pump
	// (app/sessionactor) polls session_timers for due rows (§2: "A
	// per-pod timer pump polls due timers"). Not given an explicit figure
	// in the plan; chosen as 5s — near-real-time timer delivery without
	// hammering Postgres with a poll query.
	TimerPumpInterval time.Duration

	// TimerClaimDuration is how long a timer the pump has just claimed
	// (pushed fires_at forward, see app/sessionactor's timer pump) is
	// protected from being picked up again by a concurrent or later pump
	// tick before the actor handling it finishes. Not given an explicit
	// figure in the plan; chosen as 30s — comfortably longer than a
	// single actor's expected processing time for one timer, but short
	// enough that a genuinely crashed pod's claimed timer is retried
	// reasonably quickly.
	TimerClaimDuration time.Duration

	// --- §6.4 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// HookTimeout is the max wall-clock time a single boot hook
	// (setup.sh/start.sh, §6.4) may run before sandbox-agent kills it. Not
	// specified in the plan; chosen generously as 10min since setup.sh may
	// install dependencies.
	HookTimeout time.Duration

	// ProcessStopGracePeriod is the grace period between SIGTERM and
	// SIGKILL when sandbox-agent's supervisor (internal/sandboxagent/
	// supervisor) stops one supervised process group. Not specified in the
	// plan; chosen as 10s — matches the existing, unrelated
	// ShutdownGracePeriod's own value (control-plane's own HTTP graceful
	// shutdown) only by coincidence; this is a distinct field for a
	// distinct subsystem, never reused across the two.
	ProcessStopGracePeriod time.Duration

	// SupervisorShutdownTimeout is the outer bound across ALL of
	// sandbox-agent's supervised processes during its own bounded
	// shutdown (Supervisor.StopAll), distinct from the per-process
	// ProcessStopGracePeriod above — this is the ceiling for StopAll as a
	// whole, not for any single process within it. Not specified in the
	// plan; chosen as 30s.
	SupervisorShutdownTimeout time.Duration

	// RepoSHADiscoveryTimeout bounds each individual `git -C <dir>
	// rev-parse HEAD` call sandbox-agent's boot-fingerprint assembly
	// (internal/sandboxagent/boot.DiscoverRepoSHAs) makes per repo — a
	// very minor, sub-second local git-plumbing call with no natural
	// existing Timeouts field. Not specified in the plan; chosen as 5s.
	RepoSHADiscoveryTimeout time.Duration

	// --- §14.2 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// ServiceReadinessTimeout bounds how long
	// internal/sandboxagent/services.Run waits for ONE declared
	// services.yml service (§14.2) to become ready (port dial or HTTP
	// health check succeeding) before giving up on it (PhaseTimeout). Not
	// specified in the plan; chosen as 30s — generous enough for a
	// typical dev-server/mock-server cold start without being so long
	// that a primary service's timeout stalls the whole boot sequence
	// for an unreasonable time.
	ServiceReadinessTimeout time.Duration

	// ServiceReadinessPollInterval is how often
	// internal/sandboxagent/services.Run retries a service's readiness
	// check while waiting. Not specified in the plan; chosen as 250ms —
	// frequent enough that readiness is detected promptly relative to
	// ServiceReadinessTimeout's 30s budget, without hammering the port/
	// health endpoint.
	ServiceReadinessPollInterval time.Duration

	// --- §6.4 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// RepoCloneTimeout bounds how long a single `git clone` invocation
	// (internal/sandboxagent/gitclone.CloneAll) may run before
	// sandbox-agent kills it. Not specified in the plan; chosen
	// generously as 5m since a large repo's initial clone can be slow.
	RepoCloneTimeout time.Duration

	// CredentialFetchTimeout bounds a single credential-helper call to CP
	// (internal/sandboxagent/credentials.CPClient.Fetch) minting a fresh
	// git credential. Not specified in the plan; chosen as 10s — a
	// lightweight mint call, not a large data transfer.
	CredentialFetchTimeout time.Duration

	// CredentialExpiryBuffer is the credential-helper's cache staleness
	// buffer (§5.2, explicit: "caches to disk with flock, 5-min expiry
	// buffer"): a cached credential within this buffer of its own
	// ExpiresAt is treated as already stale, never handed back as-is.
	CredentialExpiryBuffer time.Duration

	// ProviderCredentialFetchTimeout ("provider credential
	// injection", §25.1/§25.3) bounds a single call to CP's
	// /sessions/{id}/provider-credentials delivery endpoint
	// (internal/sandboxagent/credentials.CPClient.FetchProviderCredentials),
	// made once at `opencode serve` spawn time
	// (internal/sandboxagent/opencodeproc.Spawn's own caller,
	// cmd/sandbox-agent/main.go). Not specified in the plan; chosen the
	// SAME as CredentialFetchTimeout's own 10s ("a lightweight mint call,
	// not a large data transfer") -- this call resolves and decrypts at
	// most 3 rows server-side, comparably lightweight. Deliberately its
	// own field, not a reuse of CredentialFetchTimeout: the two calls hit
	// different CP endpoints for a different secret class, and §25.1's
	// own "in-memory-only fetch, no disk cache" design (unlike the SCM
	// credential-helper's own flock'd disk cache) means this timeout has
	// no CredentialExpiryBuffer-shaped sibling of its own -- a failed
	// fetch here is a one-shot, best-effort miss at spawn time, not a
	// stale-cache question.
	ProviderCredentialFetchTimeout time.Duration

	// --- §6.1 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// SandboxWSHeartbeatInterval is how often
	// internal/sandboxagent/wsbridge.Bridge sends a "heartbeat" event over
	// the sandbox WS connection. §6.1 gives this explicitly: "heartbeat
	// (30s, ...)" — unlike the other three fields in this group, this one
	// is not invented.
	SandboxWSHeartbeatInterval time.Duration

	// SandboxWSDialTimeout bounds a single sandbox-WS connect attempt
	// (internal/sandboxagent/wsbridge.Bridge.Run's call to
	// websocket.Dial). Not specified in the plan; chosen as 15s — generous
	// for a handshake round trip without letting one stuck attempt stall
	// the reconnect loop for long.
	SandboxWSDialTimeout time.Duration

	// SandboxWSReconnectMinBackoff is the initial (and floor) backoff
	// between sandbox-WS reconnect attempts after a non-fatal connect
	// failure (§6.1: "else exponential-backoff reconnect"). Not specified
	// in the plan; chosen as 1s.
	SandboxWSReconnectMinBackoff time.Duration

	// SandboxWSReconnectMaxBackoff is the ceiling the exponential backoff
	// above is capped at. Not specified in the plan; chosen as 30s.
	SandboxWSReconnectMaxBackoff time.Duration

	// --- §7 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.
	// SSEInactivityTimeout (Chain A, already above) is deliberately REUSED
	// for the OpenCode adapter's own SSE-inactivity fallback (§7) rather
	// than duplicated here — it already exists specifically for this.

	// OpenCodeReadinessTimeout bounds how long
	// internal/sandboxagent/opencodeproc waits for a freshly-spawned
	// `opencode serve` process to report healthy (GET /api/health) before
	// giving up. Not specified in the plan; chosen as 30s — OpenCode's own
	// startup may need to initialize providers/plugins, generous by the
	// same reasoning as ServiceReadinessTimeout above.
	OpenCodeReadinessTimeout time.Duration

	// OpenCodeReadinessPollInterval is how often
	// internal/sandboxagent/opencodeproc retries the health check while
	// waiting. Not specified in the plan; chosen as 250ms, matching
	// ServiceReadinessPollInterval's own precedent exactly.
	OpenCodeReadinessPollInterval time.Duration

	// --- §3.2 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — a plain field
	// with a sensible default, not wired into a fake invariant link.

	// SandboxEventAckTimeout bounds how long
	// internal/adapters/inbound/wshub's read loop waits for the session
	// actor's own reply (via the per-message SandboxEvent.Reply channel,
	// internal/app/sessionactor) on ONE inbound sandbox-WS event before
	// giving up on acking THAT message and moving on to the next frame.
	// Not specified in the plan; chosen as 5s — generous relative to a
	// single Postgres transaction (the actor's own handleSandboxEvent),
	// small relative to SandboxWSHeartbeatInterval's 30s so a lost/slow
	// ack is noticed and abandoned well before the sandbox-agent's own
	// heartbeat cadence would otherwise mask it.
	SandboxEventAckTimeout time.Duration

	// --- §6.2 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// ClientSubscribeTimeout bounds how long internal/adapters/inbound/
	// wshub's client-WS handshake waits for the browser's first inbound
	// message (the subscribe{token, clientId} frame) before closing the
	// connection with code 4001 ("re-auth required"). §6.2 gives this
	// explicitly: "subscribe{token, clientId} within 30s".
	ClientSubscribeTimeout time.Duration

	// WSTokenTTL is how long a minted ws-token (internal/platform.
	// GenerateToken, POST /api/sessions/:id/ws-token) remains valid before
	// the client hub's own subscribe-time verification rejects it with
	// close code 4002 ("token expired"). §6.2 gives this explicitly: "24h
	// TTL".
	WSTokenTTL time.Duration

	// --- §13.1 standalone additions: no ordering relationship with
	// either invariant chain above (or with any prior Step's standalone
	// additions), so — per those additions' own precedent — plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// OAuthStateTTL is how long the short-lived narvi_oauth_state cookie
	// (internal/adapters/inbound/auth's own login handler, §13.1) remains
	// valid before the CSRF-protection state value it carries is
	// considered expired (enforced by the cookie's own MaxAge/Expires, so
	// an abandoned login attempt's browser simply stops sending it). Not
	// specified in the plan; chosen as 10min — generous for a real browser
	// round-trip to GitHub and back, short enough that an abandoned login
	// attempt's own state cookie doesn't linger.
	OAuthStateTTL time.Duration

	// UserSessionTTL is how long a minted user-session (internal/platform.
	// GenerateToken, the narvi_auth_session cookie, §13.1) remains valid
	// before internal/adapters/inbound/auth's own Middleware rejects it.
	// Not specified in the plan; chosen as 30 days — a fairly standard
	// "stay signed in" web-app session length given GitHub itself is the
	// re-authentication backstop and no MFA/step-up flow exists to shorten
	// it for.
	UserSessionTTL time.Duration

	// --- §9.3 standalone additions ("e2e happy path"): no ordering
	// relationship with either invariant chain above (or with any prior
	// Step's standalone additions), so — per those additions' own
	// precedent — plain fields with sensible defaults, not wired into a
	// fake invariant link.

	// SandboxCommandSendTimeout bounds one internal/app/ports.
	// SandboxCommander.SendCommand write (internal/adapters/inbound/
	// wshub's own SandboxRegistry) -- generous for a single WS frame
	// write, small enough that a genuinely-dead connection is noticed
	// promptly. Not specified in the plan; chosen as 10s.
	SandboxCommandSendTimeout time.Duration

	// ScmCredentialTTL is the expiry window internal/adapters/inbound/
	// httpapi's scm-credentials endpoint mints for each credential it
	// hands back. §5.2 gives the SANDBOX-side cache's own staleness
	// buffer explicitly ("5-min expiry buffer") — this is a DIFFERENT
	// concept: the server-minted credential's own lifetime, which must
	// comfortably exceed a single push operation's realistic duration
	// plus that 5-min sandbox-side cache buffer. Not specified in the
	// plan; chosen as 15min.
	ScmCredentialTTL time.Duration

	// PRCreateTimeout bounds a single internal/app/ports.SourceControl.
	// CreatePR call (internal/adapters/outbound/githubapi's own real POST
	// to api.github.com, called by app/sessionactor's own
	// createPRBestEffort, pushpr.go) -- a genuine outbound network call
	// that must never run against the actor's own long-lived lifecycle
	// context unbounded. Not specified in the plan; chosen as 30s,
	// generous for a single GitHub REST API POST.
	PRCreateTimeout time.Duration

	// --- Audit-remediation (outbound-adapters lens, turn-completion
	// batch) standalone additions: no ordering relationship with either
	// invariant chain above (or with any prior Step's standalone
	// additions), so -- per those additions' own precedent -- plain
	// fields with sensible defaults, not wired into a fake invariant
	// link.

	// OpenCodeSSEReconnectInterval is how long
	// internal/adapters/outbound/opencode.Adapter.runEventLoop waits
	// before retrying a dropped persistent GET /event connection.
	// Deliberately much shorter than SSEInactivityTimeout so a dropped
	// connection has a real chance to reconnect well before any per-turn
	// SSE-inactivity fallback finalizes a turn based on stale silence --
	// fixes a confirmed audit finding where reusing SSEInactivityTimeout
	// itself as the reconnect delay made reconnection structurally
	// unable to ever win that race. Not specified in the plan; chosen as
	// 2s.
	OpenCodeSSEReconnectInterval time.Duration

	// OpenCodeRequestTimeout bounds every internal/adapters/outbound/
	// opencode.Adapter.doJSON-routed HTTP call (session resolution, model
	// catalog, prompt_async, abort, and the final-message-fetch fallback)
	// via a per-request context.WithTimeout wrap -- deliberately NOT
	// applied to the adapter's own persistent GET /event connection
	// (connectAndConsume), which is intentionally long-lived for the
	// adapter's whole lifetime. Generous enough for a legitimately slow
	// catalog/message-list response, bounded so a hung TCP connection can
	// never wedge a turn indefinitely -- most critically for the
	// SSE-inactivity fallback's own final-message fetch, which is called
	// exactly when a turn already looks stuck and must never itself be
	// able to hang forever. Not specified in the plan; chosen as 30s.
	OpenCodeRequestTimeout time.Duration

	// --- §7.2 standalone addition ("OpenCode adapter: context-overflow
	// compaction retry", §7.2): no ordering relationship with either
	// invariant chain above (or with any prior Step's standalone
	// additions), so -- per those additions' own precedent -- a plain
	// field with a sensible default, not wired into a fake invariant link.

	// OpenCodeSummarizeTimeout bounds internal/adapters/outbound/
	// opencode.Adapter.forceCompaction's own POST /session/{id}/summarize
	// call specifically -- via doJSONTimeout (client.go), NOT the shared
	// a.requestTimeout/OpenCodeRequestTimeout above, which this Step's own
	// live investigation found would otherwise silently cap it: doJSON's
	// context.WithTimeout(ctx, a.requestTimeout) wrap always takes the
	// SHORTER of the caller's own ctx deadline and a.requestTimeout,
	// regardless of what the call actually needs. Not specified in the
	// plan; chosen generously as 120s -- mirroring HookTimeout's own "not
	// specified in the plan; chosen generously" convention above, since
	// the concrete cost of a real /summarize call against a large,
	// genuinely-overflowed context is unknown: this Step's own real,
	// live-verified /summarize call (a small conversation, §7.2's own
	// research pass) completed in ~2s, but a large real-world context
	// that actually triggered a ContextOverflowError in the first place
	// could plausibly take far longer (40-90s), and OpenCodeRequestTimeout's
	// own 30s default is sized for ordinary session/catalog/message-list
	// calls, not a single non-streaming AI summarization pass. A dedicated
	// field (rather than reusing OpenCodeRequestTimeout) is what actually
	// makes it possible to give this ONE call site a different, more
	// generous bound at all.
	OpenCodeSummarizeTimeout time.Duration

	// --- Standalone addition ("OpenCode adapter: typed transient-error
	// retry"): no ordering relationship with either invariant chain above
	// (or with any prior Step's standalone additions), so -- per those
	// additions' own precedent -- a plain field with a sensible default,
	// not wired into a fake invariant link.

	// OpenCodeTransientRetryBackoff bounds internal/adapters/outbound/
	// opencode.Adapter.attemptTransientRetry's own wait before re-dispatching
	// the same prompt after a first-time transient APIError (OpenCode's own
	// typed isRetryable verdict, isTransientAPIError in that package's
	// outcome.go) -- reusing the SAME at-most-one-retry-per-turn latch
	// OpenCodeSummarizeTimeout's own §7.2 compaction retry already
	// established (ts.compacting/ts.compactionAttempted, turn.go), but this
	// failure class needs a WAIT of its own first: unlike a context
	// overflow (where forceCompaction IS the recovery action), a transient
	// provider blip (a 429/529-shaped response, per OpenCode's own
	// corroborating statusCode) is generally still likely to fail again
	// near-instantly without a short pause first -- immediately hammering
	// an overloaded or rate-limited provider a second time is not a
	// meaningfully different attempt from the first. Not specified in the
	// plan; chosen as 2s -- short enough that a genuinely transient blip's
	// own real-world recovery window (a rate-limit window resetting, a
	// brief provider-side overload clearing) has already plausibly passed
	// by the time this adapter retries, without adding a perceptible delay
	// to a turn that already failed once and is worth resolving promptly;
	// deliberately NOT as large as OpenCodeSummarizeTimeout's own 120s
	// (that field bounds a genuinely slow HTTP call this adapter must
	// WAIT OUT; this field is a deliberately-chosen pause this adapter
	// itself inserts, an entirely different kind of duration).
	OpenCodeTransientRetryBackoff time.Duration

	// SetupRerunRetryBackoff is the pause sandbox-agent inserts before its
	// ONE retry of a failed full `setup.sh` rerun (§19.6's "retry the
	// install on transient failure, then warn -- never fail the boot on
	// it"). Deliberately its own field rather than a reuse of
	// OpenCodeTransientRetryBackoff above, even though both are "one short
	// pause before one retry": the two bound genuinely different
	// operations whose constraints can diverge. That one paces a retry
	// inside a live turn, where added delay is felt directly by a waiting
	// human; this one paces a package-registry reinstall inside the boot
	// sequence, which has its own separate overall budget and a slower,
	// network-bound failure profile. Tuning one for turn latency must
	// never silently retime the other's boot behaviour -- the same reason
	// ProcessStopGracePeriod and SupervisorShutdownTimeout stay distinct.
	SetupRerunRetryBackoff time.Duration

	// --- Audit-remediation (inbound-hygiene lens, WS/REST hygiene batch)
	// standalone additions: no ordering relationship with either
	// invariant chain above (or with any prior Step's standalone
	// additions), so -- per those additions' own precedent -- plain
	// fields with sensible defaults, not wired into a fake invariant
	// link.

	// ClientWSPingInterval is how often internal/adapters/inbound/wshub's
	// client-WS handler (client.go, NewClientHandler) sends a real,
	// server-initiated websocket ping to a subscribed browser connection
	// and waits (bounded by this SAME duration) for the peer's pong --
	// the genuine liveness check for a client connection that only ever
	// passively watches live broadcasts and so never itself sends an
	// application frame. A Ping that goes unanswered proves the
	// connection is genuinely unresponsive, closed with custom code 4003
	// ("idle timeout"). Not specified in the plan; chosen as 30s,
	// matching SandboxWSHeartbeatInterval's own existing cadence
	// precedent (§6.1) for the analogous sandbox-side mechanism.
	ClientWSPingInterval time.Duration

	// ClientFetchHistoryMinInterval is the minimum time internal/adapters/
	// inbound/wshub's client-WS read loop (client.go, readClientLoop)
	// requires between two successive fetch_history requests it actually
	// processes on one connection -- each processed request runs a real
	// Postgres query (events.ListForSession), so this bounds how often a
	// single connection can trigger one, independent of the connection's
	// own liveness. A fetch_history frame arriving before this interval
	// has elapsed since the last one was processed is logged and dropped;
	// the connection stays open. Not specified in the plan; chosen as
	// 250ms -- generous for any real pagination UI (up to 4 requests/sec)
	// while preventing a tight-loop hammer.
	ClientFetchHistoryMinInterval time.Duration

	// --- Audit-remediation (outbound-adapters lens, config/platform-
	// hardening batch) standalone addition: no ordering relationship with
	// either invariant chain above (or with any prior batch's standalone
	// additions), so -- per those additions' own precedent -- a plain
	// field with a sensible default, not wired into a fake invariant link.

	// ExpiredCredentialCleanupInterval is how often
	// internal/adapters/outbound/postgres.RunExpiredTokenCleanup ticks,
	// deleting ws_tokens/user_sessions rows whose expires_at has already
	// passed (migrations/000016_ws_tokens.up.sql,
	// migrations/000017_auth_v1.up.sql -- both tables check expires_at
	// only at read/verify time; nothing else ever purges an expired row,
	// so left alone table growth is unbounded). Both tables' own TTLs
	// (WSTokenTTL 24h, UserSessionTTL 30 days) are on the order of
	// hours/days, so hourly cleanup is more than frequent enough. Not
	// specified in the plan; chosen as 1h.
	ExpiredCredentialCleanupInterval time.Duration

	// --- §3.2 standalone additions ("snapshots & restore"): no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into a
	// fake invariant link.

	// SnapshotMintTimeout bounds sandbox-agent's own call
	// (internal/sandboxagent/snapshotclient.Client.Mint) to the control
	// plane's new snapshot-mint endpoint (design decision 2, POST
	// /sessions/{id}/snapshot), which itself makes a real, network-bound
	// SandboxProvider.TakeSnapshot call server-side -- more generous than
	// CredentialFetchTimeout's own 10s (a lightweight mint call with no
	// real provider round trip behind it) since a real snapshot operation
	// can genuinely take longer. Not specified in the plan; chosen as 60s.
	SnapshotMintTimeout time.Duration

	// --- §5.3 standalone addition ("reconciler + GC"): no ordering
	// relationship with either invariant chain above (or with any prior
	// Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into a
	// fake invariant link.

	// ReconcilerInterval is how often the process-wide reconciler
	// (internal/app/reconciler, §5.3) ticks: one ports.SandboxProvider.List
	// call compared against Postgres's own expected-alive set, reaping any
	// orphaned provider-side sandbox instance found. §5.3 gives this
	// explicitly: "60s loop against the provider API".
	ReconcilerInterval time.Duration

	// --- §5.3 fix (reconciler orphan-GC debounce): a real,
	// empirically-reproduced race, not covered by either invariant chain
	// above -- but DOES need its own pairwise check against
	// ReconcilerInterval (see Validate() below) for the guarantee it
	// exists to provide to actually hold, so it is not a plain standalone
	// addition either.

	// ReconcilerOrphanConfirmationPeriod is the minimum wall-clock time a
	// provider-side ref found in provider.List() with no matching row in
	// Postgres's own expected-alive set (SandboxStore.ListLiveProviderIDs)
	// must remain CONTINUOUSLY unexplained, across separate ReconcileOnce
	// ticks, before app/reconciler.Reconciler actually calls StopSandbox on
	// it -- a debounce/minimum-confirmation-count grace period closing a
	// real race: internal/app/sessionactor/dispatch.go's own deliberate
	// three-step spawn sequencing (see that file's own top "# Sequencing"
	// comment) commits a sandboxes row already in a LIVE status
	// (status='spawning') with provider_id still NULL, THEN calls the
	// real, network-bound SandboxProvider.CreateSandbox OUTSIDE any
	// transaction, THEN commits a SECOND, LATER transact
	// (recordProviderOutcome) that finally records provider_id.
	// ListLiveSandboxProviderIDs requires provider_id IS NOT NULL, so that
	// row is invisible to the reconciler's own expected-alive set for the
	// whole window between CreateSandbox returning success and
	// provider_id actually being committed, even though status is already
	// genuinely live. A reconciler tick landing in exactly that window
	// would, without this debounce, see the real, already-created, wanted
	// cloud object as an "unexplained" ref and kill a legitimate, in-flight
	// spawn on its very first sighting -- requiring no race with a second
	// actor, no double-click: inherent to every successful spawn's own
	// normal timing.
	//
	// The real window this must comfortably exceed is bounded, at the
	// absolute outside, by ProviderHTTPClientTimeout (5m -- CreateSandbox's
	// own worst-case duration) but is realistically sub-second (ordinary
	// network latency plus one small Postgres commit). This field is
	// deliberately NOT set anywhere near that 5m theoretical ceiling:
	// app/reconciler.Reconciler's own tick-by-tick structure already
	// guarantees a ref is NEVER reaped on its first sighting regardless of
	// this value (see ReconcileOnce's own doc comment) -- under normal
	// ticking, the gap between a ref's first sighting and its second is
	// ALREADY a full ReconcilerInterval (60s), dwarfing the sub-second real
	// race window on its own. This field's actual job is a second,
	// independent safety margin against ticks landing unusually close
	// together (a slow ReconcileOnce call delaying the ticker, a
	// misconfigured smaller ReconcilerInterval in some future environment,
	// or a test driving ticks back-to-back) -- chosen as 30s: comfortably
	// above any realistic sub-second race window, matching
	// MinTimeoutMargin's own existing 30s floor elsewhere in this struct,
	// and (deliberately, see Validate() below) at least MinTimeoutMargin
	// below ReconcilerInterval's own 60s so the "reaped on the SECOND
	// consecutive tick, never the first" guarantee this whole mechanism
	// promises actually holds under the shipped default, not merely on
	// average -- a real orphan is still fully reaped within at most two
	// tick intervals (120s worst case), meaningfully faster than leaving
	// it uncleaned indefinitely.
	ReconcilerOrphanConfirmationPeriod time.Duration

	// --- §8.5 standalone additions ("image builds", §8.5-note/§10-P2):
	// no ordering relationship with either invariant chain above (or with
	// any prior Step's standalone additions), so -- per those additions'
	// own precedent -- plain fields with sensible defaults, not wired into
	// a fake invariant link.

	// RepoSHAResolutionTimeout bounds a single internal/app/ports.
	// SourceControl.ResolveBranchSHA call (app/sessionactor's own
	// resolveAndSetImage, dispatch.go/imageresolve.go) -- one or two real
	// outbound GitHub API GETs per repo (the repo's default branch, then
	// its HEAD commit), called once per repo IN A LOOP, each bounded
	// individually so one slow/hanging repo can't stall the others
	// indefinitely. Not specified in the plan; chosen as 10s, matching
	// CredentialFetchTimeout's own reasoning exactly ("a lightweight...
	// call, not a large data transfer").
	RepoSHAResolutionTimeout time.Duration

	// ImageBuildPumpInterval is how often the process-wide background
	// image-build loop (internal/app/imagebuild, mirroring app/reconciler's
	// own ReconcilerInterval-driven ticker shape) polls image_builds for
	// rows eligible to (re)attempt now. Not specified in the plan; chosen
	// as 60s, matching ReconcilerInterval's own precedent -- image builds
	// are a slow, infrequent background maintenance concern, not a
	// latency-sensitive one.
	ImageBuildPumpInterval time.Duration

	// ImageBuildBackoffBase is domain/imagebuild.BackoffConfig.BaseDelay:
	// the retry delay scheduled after a fingerprint's FIRST failed build
	// attempt. Not specified in the plan; chosen as 1min -- see
	// domain/imagebuild.EvaluateBackoff's own doc comment for the full
	// schedule this produces alongside ImageBuildBackoffMax below.
	ImageBuildBackoffBase time.Duration

	// ImageBuildBackoffMax is domain/imagebuild.BackoffConfig.MaxDelay: the
	// ceiling the exponential schedule above plateaus at. Not specified in
	// the plan; chosen as 30min -- deliberately the SAME cadence §3.5's own
	// language ("not fixed 30 min") explicitly rejects as a FIRST-failure
	// delay, but a reasonable EVENTUAL steady-state ceiling once a build is
	// confirmed persistently broken: this is the cap the schedule grows
	// INTO after repeated failures, never the delay applied from the very
	// first one, so it does not contradict §3.5.
	ImageBuildBackoffMax time.Duration

	// --- §14.3 standalone addition ("mocking + contract drift", §14.3):
	// no ordering relationship with either invariant chain above (or with
	// any prior Step's standalone additions), so -- per those additions'
	// own precedent -- a plain field with a sensible default, not wired
	// into a fake invariant link.

	// ContractsFingerprintResolutionTimeout bounds a single internal/app/
	// ports.SourceControl.ResolveContractsFingerprint call (app/
	// sessionactor's own checkContractDrift, contractdrift.go) -- one real
	// outbound GitHub Contents API GET per repo, called once per repo IN A
	// LOOP, each bounded individually so one slow/hanging repo can't stall
	// the others indefinitely -- mirrors RepoSHAResolutionTimeout's own
	// identical reasoning and value exactly (this codebase's own
	// convention is one named timeout per distinct network-call type, even
	// when two types happen to share the same chosen value -- see
	// RepoSHAResolutionTimeout's own addition in §8.5 for the precedent
	// this repeats rather than reuses). Not specified in the plan; chosen
	// as 10s, matching RepoSHAResolutionTimeout/CredentialFetchTimeout's
	// own "lightweight call, not a large data transfer" reasoning.
	ContractsFingerprintResolutionTimeout time.Duration

	// --- §3.4 standalone addition ("gitstate in-sandbox", §3.4): no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into a
	// fake invariant link.

	// GitSyncStepTimeout bounds each individual git subprocess
	// internal/sandboxagent/gitclone.SyncAll spawns while reconciling one
	// already-existing repo at boot (`git status --porcelain`, `git stash
	// push`, `git rev-parse --verify`, `git checkout`/`git checkout -b`,
	// `git stash pop`) -- every one of these is local-only (no network),
	// unlike RepoCloneTimeout's own network-bound clone/push operations, so
	// a much smaller budget than RepoCloneTimeout's 5m is appropriate; still
	// more generous than RepoSHADiscoveryTimeout's 5s since checkout/stash
	// can touch a large working tree, not just read one small ref. Not
	// specified in the plan; chosen as 30s, matching ServiceReadinessTimeout/
	// OpenCodeReadinessTimeout's own "generous for typical local operations
	// without stalling the whole boot sequence" reasoning.
	GitSyncStepTimeout time.Duration

	// --- §5.1 standalone addition ("webhook toolkit", §5.1/§5.2): no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into a
	// fake invariant link.

	// WebhookTimestampFreshnessWindow bounds how far a provider-supplied
	// webhook timestamp (e.g. Slack's X-Slack-Request-Timestamp) may drift
	// from now before platform.VerifyWebhookTimestamp rejects it as a
	// possible replay -- checked SEPARATELY from (in addition to) the
	// signature itself, mirroring Slack's own signing-secrets guidance.
	// Deliberately a DISTINCT field from HMACWindow above even though both
	// happen to default to 5 minutes: HMACWindow guards Narvi's own
	// internal "{timestamp}.{signature}" bearer scheme (hmacauth.go),
	// this one guards third-party provider webhook signatures
	// (webhooksig.go) -- two functionally distinct subsystems that must
	// stay independently rotatable/tunable, matching
	// ProcessStopGracePeriod/ShutdownGracePeriod's own "same value, two
	// distinct fields for two distinct subsystems" precedent. Not given
	// an explicit figure in the plan; chosen as 5 minutes, matching
	// Slack's own commonly recommended replay window.
	WebhookTimestampFreshnessWindow time.Duration

	// --- §8.10 standalone additions ("Linear ingress", §8.10): no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- plain fields with sensible defaults, not wired into a
	// fake invariant link.

	// LinearWebhookTimestampWindow bounds how far a Linear webhook's own
	// body-level webhookTimestamp field may drift from now before this
	// Step's webhook handler rejects it as a possible replay -- a
	// DELIBERATELY DISTINCT field from WebhookTimestampFreshnessWindow
	// above (§5.1's generic 5-minute default, "matching Slack's own
	// commonly recommended replay window") even though both guard the same
	// general class of check, because Linear's own real, current developer
	// docs (confirmed during this Step's investigation) recommend a much
	// tighter window: "verify it's within a minute of the time your system
	// sees it." Using the shared 5-minute field here would silently accept
	// a replay window 5x wider than Linear's own stated guidance -- exactly
	// the kind of same-value-different-subsystem confusion
	// ProcessStopGracePeriod/ShutdownGracePeriod's own precedent (cited by
	// WebhookTimestampFreshnessWindow's own doc comment) argues against.
	// Chosen as 60s, Linear's own explicit figure, not invented.
	LinearWebhookTimestampWindow time.Duration

	// LinearOutboundActivityTimeout bounds the one outbound Linear
	// GraphQL API call this Step makes synchronously from inside the
	// webhook handler itself (posting an initial acknowledgment Agent
	// Activity -- see internal/adapters/outbound/linearapi's own doc
	// comment for why this is a minimal direct call, not the general
	// Notifier/outbox abstraction §5.1 owns). Linear's own real docs
	// require a webhook receiver to "return a response ... within 5
	// seconds" -- this must clear that budget with real margin, so a slow
	// or hanging Linear API call never itself causes Linear to consider
	// the webhook delivery failed. Chosen as 3s: generous for a single
	// lightweight GraphQL mutation, comfortably below the 5s ceiling with
	// margin for the rest of the handler's own (fast, no-network) work.
	LinearOutboundActivityTimeout time.Duration

	// --- Audit-remediation addition (HIGH, "releasing the
	// linear_agent_sessions claim after a SetSessionID failure can spawn a
	// duplicate, independently-dispatched agent"): no ordering relationship
	// with either invariant chain above (or with any prior standalone
	// addition), so -- per those additions' own precedent -- plain fields
	// with sensible defaults, not wired into a fake invariant link.

	// LinearSetSessionIDTimeout bounds ONE attempt of the retried
	// AgentSessions.SetSessionID call in internal/adapters/inbound/linear's
	// own handleCreated (webhook.go's own setSessionIDWithRetry) -- a
	// single, local Postgres UPDATE against a row this same request
	// already won the claim on, never an outbound network call. Not
	// specified in the plan (this fix postdates it); chosen as 2s --
	// generous for a single-row UPDATE even under a transient connection
	// blip, while keeping the retry loop's own worst-case added latency
	// small relative to Linear's 5s webhook-response requirement.
	//
	// LOW audit fix ("stale/inverted doc comment"): this field's own
	// comment used to justify 2s by claiming it was "deliberately much
	// shorter than IdentityEmailFetchTimeout's own lightweight outbound API
	// call budget" -- true when IdentityEmailFetchTimeout was still 10s,
	// but false once the L5 audit fix dropped that field well below 2s
	// (300ms, later retuned to 800ms by the HIGH audit fix -- see that
	// field's own doc comment). LinearSetSessionIDTimeout (2s) is now
	// numerically LARGER than IdentityEmailFetchTimeout (800ms), the
	// OPPOSITE of what the old comment asserted. That inversion does not
	// make either value wrong: the two fields were never comparable by
	// magnitude in the first place -- this one bounds a single, LOCAL
	// Postgres UPDATE against a row already claimed by this same request
	// (a connection blip is the only realistic failure mode, and even a
	// generous budget for that is cheap), while IdentityEmailFetchTimeout
	// bounds a genuine outbound network call to a THIRD-PARTY provider API
	// under a much tighter, retry-multiplied, externally-imposed ack
	// budget (Slack's ~3s). A local operation being allowed a numerically
	// larger per-attempt ceiling than a retried outbound one is not a
	// contradiction; it simply reflects that neither field's own budget was
	// ever derived FROM the other's value.
	LinearSetSessionIDTimeout time.Duration

	// LinearSetSessionIDMaxAttempts/LinearSetSessionIDRetryBaseDelay/
	// LinearSetSessionIDRetryMaxDelay configure platform.Retry's own
	// doubling-capped-at-max backoff for setSessionIDWithRetry -- mirrors
	// IdentityEmailFetchMaxAttempts/IdentityEmailFetchRetryBaseDelay/
	// IdentityEmailFetchRetryMaxDelay's own identical shape (a foreground,
	// in-process retry bounded by the caller's own webhook-response
	// budget, internal/platform/retry.go), reused here for a DIFFERENT
	// call: by the time setSessionIDWithRetry ever runs,
	// httpapi.CreateSessionCore has ALREADY committed the real session
	// and fired TriggerDispatch (see handleCreated's own top doc comment)
	// -- retrying this idempotent UPDATE is always safe and never risks a
	// duplicate session, unlike releasing the claim and letting Linear
	// redeliver the whole `created` event. Not specified in the plan;
	// chosen as 3 attempts / 100ms base / 500ms max -- with 3 attempts,
	// the worst case (2 waits: 100ms then 200ms) adds well under a second
	// of wall-clock time to the handler's own critical path.
	LinearSetSessionIDMaxAttempts    int
	LinearSetSessionIDRetryBaseDelay time.Duration
	LinearSetSessionIDRetryMaxDelay  time.Duration

	// --- §8.10 standalone addition ("Slack ingress", §8.10): no
	// ordering relationship with either invariant chain above (or with
	// any prior Step's standalone additions), so -- per those additions'
	// own precedent -- a plain field with a sensible default, not wired
	// into a fake invariant link.

	// SlackAckTimeout bounds a single internal/adapters/inbound/slack
	// ackClient.postAck call (a real outbound POST to Slack's own
	// chat.postMessage, made synchronously in the inbound webhook request
	// path before that handler answers Slack's own delivery with 200) --
	// mirrors PRCreateTimeout's own "a genuine outbound network call that
	// must never run against an unbounded context" precedent exactly:
	// without this, a Slack API outage would hang every webhook request
	// touching a new-or-busy thread indefinitely, since neither
	// http.DefaultClient nor the request's own context carries any
	// deadline otherwise. Not specified in the plan; chosen as 10s,
	// generous for a single Slack Web API POST while still well short of
	// Slack's own ~3s "retry the webhook" outer expectation being made
	// noticeably worse.
	SlackAckTimeout time.Duration

	// --- §8.1 standalone addition ("plan mode, cross-channel", §8.1/
	// §13.3): no ordering relationship with either invariant chain above (or
	// with any prior Step's standalone additions), so -- per those
	// additions' own precedent -- a plain field with a sensible default, not
	// wired into a fake invariant link.

	// SlackInteractivityAckTimeout bounds the ENTIRE synchronous decide+
	// update sequence internal/adapters/inbound/slack/interactive.go's
	// block_actions handling runs for approve_plan/reject_plan: the shared
	// httpapi.DecidePlan call (opens a tx, locks the session row, the
	// guarded UPDATE, possibly inserting+dispatching a new turn, enqueuing
	// cross-channel notifications, committing) followed by the real,
	// synchronous chat.update call reflecting the outcome -- ONE shared
	// bounded context covers both, not two separately-budgeted calls that
	// could each individually fit within their own budget yet still
	// together exceed Slack's real window.
	//
	// Deliberately a SEPARATE, much tighter constant from SlackAckTimeout
	// above, even though both nominally guard "a Slack ack": SlackAckTimeout
	// was sized for the Events API's own in-thread ack (handler.go's
	// ackClient.postAck -- a single outbound chat.postMessage POST, with no
	// DB transaction ahead of it, and generous headroom relative to Slack's
	// own separate "~3s, then retry the whole webhook delivery" outer
	// expectation for THAT route). This field guards a COMPLETELY DIFFERENT
	// and far more time-pressured budget: Slack's own real interactivity
	// payload ack window, which Slack's docs describe as a hard ~3 seconds --
	// miss it and Slack shows the user a "dispatch_failed" error, even when
	// Narvi's own backend goes on to complete the action correctly a moment
	// later. And unlike SlackAckTimeout's single POST, this budget must
	// cover the WHOLE guarded-UPDATE transaction described above plus the
	// follow-up chat.update, so it cannot reuse SlackAckTimeout's own more
	// generous 10s value without risking exactly the kind of DB-contention-
	// blows-past-Slack's-real-budget failure this field exists to prevent.
	// Not given an explicit figure in the plan; chosen as 2.5s -- leaves
	// real margin (roughly 500ms) below Slack's own ~3s hard ceiling for
	// network/serialization overhead getting the response back to Slack,
	// while still being tight enough to fail fast (and let the handler
	// answer Slack's own required 200 promptly regardless) under DB
	// contention or a slow Slack response, rather than hang until either
	// finishes.
	SlackInteractivityAckTimeout time.Duration

	// --- §5.1 standalone additions ("outbox delivery", §5.1): no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- plain fields with sensible defaults, not wired into a
	// fake invariant link. OutboxClaimDuration is the one exception: see
	// the audit-fix note directly above that field below, matching
	// §5.3's own ReconcilerOrphanConfirmationPeriod precedent of a Step's
	// later fix adding its own single, independent pairwise check without
	// retroactively promoting the whole family into either named chain.

	// OutboxPumpInterval is how often the process-wide background outbox
	// delivery loop (internal/app/outboxworker, mirroring app/imagebuild's
	// own ImageBuildPumpInterval-driven ticker shape) polls the outbox
	// table for rows eligible to (re)attempt delivery now. Not specified in
	// the plan; chosen as 5s -- deliberately much shorter than
	// ImageBuildPumpInterval/ReconcilerInterval's own 60s: unlike a slow,
	// expensive image build or a coarse reconciliation sweep, an outbox
	// entry is a small, cheap notification a real user (in a Slack thread,
	// a Linear session, a GitHub PR) is actively waiting to see, so this
	// loop polls near-real-time, matching TimerPumpInterval's own identical
	// "near-real-time delivery without hammering Postgres" reasoning.
	OutboxPumpInterval time.Duration

	// OutboxBackoffBase is domain/outbox.BackoffConfig.BaseDelay: the retry
	// delay scheduled after an outbox entry's FIRST failed delivery
	// attempt. Not specified in the plan; chosen as 30s -- see
	// domain/outbox.EvaluateBackoff's own doc comment for the full
	// schedule this produces alongside OutboxBackoffMax below, and
	// domain/outbox.MaxAttempts's own doc comment for why this combination
	// comfortably survives resilience scenario 9's own 10-minute outage
	// (§9.3: "Slack API 500s for 10 min -> notification eventually
	// delivered, no loss") without ever dead-lettering partway through it.
	OutboxBackoffBase time.Duration

	// OutboxBackoffMax is domain/outbox.BackoffConfig.MaxDelay: the ceiling
	// the exponential schedule above plateaus at. Not specified in the
	// plan; chosen as 5min -- deliberately shorter than ImageBuildBackoffMax's
	// own 30min: a stuck image build can reasonably wait half an hour
	// between retries with no one watching in real time, but an outbox
	// entry backs a live notification a human is waiting on, so this
	// plateaus much sooner.
	OutboxBackoffMax time.Duration

	// OutboxDeliveryTimeout bounds ONE outbound notifier call
	// (ports.Notifier.Deliver, routed to whichever of the Slack/Linear/
	// GitHub adapters owns the claimed row's own kind) during a single
	// delivery attempt -- mirrors RepoSHAResolutionTimeout's own "a
	// lightweight outbound call, bounded individually so one slow/hanging
	// call can't stall the rest of the batch" reasoning exactly. Not
	// specified in the plan; chosen as 15s -- more generous than
	// RepoSHAResolutionTimeout/CredentialFetchTimeout's own 10s (a
	// notification POST can occasionally be slower than a lightweight GET,
	// e.g. Slack/Linear API tail latency), but still far short of
	// OutboxBackoffBase so a single hung call never meaningfully delays the
	// next pump tick's own batch.
	OutboxDeliveryTimeout time.Duration

	// --- Audit fix (H6, correctness -- internal/app/outboxworker/
	// builder.go): the ORIGINAL model below computed ONE now := time.Now()
	// per batch-level claimBatch call and stamped every row in that batch
	// (up to pumpBatchSize=20) with the identical claim-expiry timestamp,
	// before ANY of them were actually delivered. PumpOnce then attempts
	// each claimed row SEQUENTIALLY, one at a time, each bounded by
	// OutboxDeliveryTimeout -- so a row late in the batch could still be
	// waiting for its own attempt() call to even START, long after its
	// shared claim-expiry timestamp had already elapsed, letting a
	// concurrent tick (this pod's next tick, or another pod's own
	// Builder -- ListDuePendingOutboxEntries' own FOR UPDATE SKIP LOCKED
	// exists specifically so multiple pods run this loop concurrently)
	// re-claim that same row while the first delivery was still in
	// flight -- and, if that second builder's own delivery was ALSO still
	// in flight when the first builder finally reached its own turn, a
	// naive renewal guarded only by "status = 'pending'" (both builders'
	// own claims leave status at 'pending' -- the outbox table has no
	// third, in-flight status) would succeed for BOTH builders, and BOTH
	// would call notifier.Deliver on the same row concurrently: a genuine
	// double-delivery race, empirically reproduced against a real
	// Postgres testcontainer with a deliberately slow concurrent
	// claimant. Status alone cannot tell "untouched since I last observed
	// it" apart from "a different builder already re-claimed/renewed it
	// and is mid-delivery on it right now".
	//
	// The fix is a per-row re-claim/heartbeat (RenewOutboxClaim, called
	// from attempt() in internal/app/outboxworker/builder.go immediately
	// before the real notifier.Deliver call, using time.Now() at THAT
	// moment -- not the batch's shared claim-time now) that is ALSO a
	// genuine optimistic-concurrency compare-and-swap against the row's
	// own next_attempt_at: it only renews (and only succeeds) if the
	// row's CURRENT next_attempt_at still matches the value THIS caller
	// last observed (row.NextAttemptAt, from its own prior
	// ClaimOutboxEntry/RenewOutboxClaim return). If a different builder
	// already won the race, that builder's own claim/renewal already
	// changed next_attempt_at away from the value this caller observed,
	// so this caller's own renewal correctly fails (pgx.ErrNoRows) instead
	// of proceeding to deliver -- attempt() already treats that error as
	// "stop, do not deliver". This CAS is what actually gives the renewal
	// single-writer teeth: at most one builder's renewal for a given prior
	// next_attempt_at value can ever succeed, so at most one builder ever
	// proceeds to notifier.Deliver for a given row at a time. It never
	// increments attempts again (that already happened once, at
	// claimBatch time, via ClaimOutboxEntry). See that query's own doc
	// comment (queries/outbox.sql) for the full mechanism.
	//
	// This changes what OutboxClaimDuration itself protects: no longer "one
	// batch's own worst-case total sequential processing time" (which the
	// original doc comment reasoned about, and which no fixed value could
	// ever safely bound against an arbitrarily large pumpBatchSize/attempt
	// backlog), but "one row's own renewal window, renewed fresh
	// immediately before each real delivery attempt" -- so it now only
	// ever needs to comfortably outlast a SINGLE OutboxDeliveryTimeout-
	// bounded attempt, which Validate() below enforces as a new,
	// independent pairwise check (OutboxClaimDuration > OutboxDeliveryTimeout,
	// mirroring §5.3's own ReconcilerInterval > ReconcilerOrphanConfirmationPeriod
	// precedent of a single, narrow link added outside either named chain).

	// OutboxClaimDuration protects a just-claimed (or just-renewed) outbox
	// row from being re-selected by a concurrent/later pump tick (this
	// pod's or another pod's own outboxworker.Builder) before THIS row's
	// own real delivery attempt has recorded an outcome -- mirrors
	// TimerClaimDuration's own identical "push the due-again time forward
	// by a protection window at claim time" mechanism exactly, needed here
	// because the outbox table (unlike image_builds' own 'building'
	// status) has no third, in-flight status distinct from
	// pending/delivered/dead_letter to mark a row claimed with.
	// ClaimOutboxEntry bumps next_attempt_at forward by this duration (and
	// increments attempts) at batch-claim time; attempt() then RENEWS the
	// SAME row's own next_attempt_at by this same duration, from a FRESH
	// time.Now(), immediately before its own real notifier call, via the
	// genuine compare-and-swap described in the audit-fix note directly
	// above (guarded by the row's own next_attempt_at still matching what
	// this caller last observed, not merely status='pending') -- WITHOUT
	// incrementing attempts again. RecordOutboxEntryFailure/
	// MarkOutboxEntryDeadLetter then
	// overwrite that provisional value with the real domain/outbox.
	// EvaluateBackoff decision once the attempt's real outcome is known.
	// Self-healing exactly like TimerClaimDuration's own precedent: a pod
	// that crashes mid-delivery simply leaves the row due again once this
	// window elapses, picked up by a later tick with no separate sweep
	// needed. Not specified in the plan; chosen as 45s -- OutboxDeliveryTimeout
	// (15s) above is this window's own worst-case real single-attempt
	// duration, and Validate() below requires at least MinTimeoutMargin
	// (30s) of headroom beyond it, so 45s sits exactly at that minimum
	// margin (matching ReconcilerInterval/ReconcilerOrphanConfirmationPeriod's
	// own identical "exactly at the minimum margin, not extra slack beyond
	// it" precedent) rather than TimerClaimDuration's own unrelated 30s
	// value, which this field no longer needs to match now that it
	// protects one renewed row's own window rather than reasoning (as the
	// pre-fix comment above used to) about an entire batch's own
	// sequential processing time.
	OutboxClaimDuration time.Duration

	// --- §8.3 standalone addition ("intent classifier", §8.3/§18): no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into a
	// fake invariant link.

	// IntentClassifierLLMTimeout bounds ONE outbound ports.LLM.Complete
	// call (internal/adapters/outbound/llm's Anthropic adapter, called
	// once per session by internal/app/intentclassifier.Classify).
	// Configured directly on the Anthropic SDK client at construction time
	// (option.WithRequestTimeout) -- §18.1's own explicit rule is that
	// this is the ONLY timeout layer for this call: never a second,
	// redundant context.WithTimeout raced against it, since the SDK's own
	// internal abort always resolves first. Not specified in the plan;
	// chosen as 10s, matching RepoSHAResolutionTimeout/
	// CredentialFetchTimeout's own "lightweight call, not a large data
	// transfer" reasoning -- a structured-output classification call over
	// a short prompt is exactly that kind of call, and this is a
	// high-volume, latency-sensitive path called on every session across
	// every surface (never a "remotely complicated" reasoning task, hence
	// this Step's own choice of a fast/cheap model with no extended
	// thinking enabled).
	IntentClassifierLLMTimeout time.Duration

	// --- §13.2 standalone additions ("identities + full RBAC", §13.2):
	// no ordering relationship with either invariant chain above (or with
	// any prior Step's standalone additions), so -- per those additions'
	// own precedent -- plain fields with sensible defaults, not wired into
	// a fake invariant link.

	// IdentityEmailFetchTimeout bounds ONE outbound provider profile-email
	// API call (Slack users.info / Linear's `user(id) { email }` query,
	// internal/app/identitylink's own Resolve) -- a single attempt's own
	// budget.
	//
	// Audit fix (L5, "the retry timing doc is wrong, and the real worst
	// case blocks Slack's actual ~3s webhook ack window"): this used to be
	// 10s, matching RepoSHAResolutionTimeout/CredentialFetchTimeout's own
	// "lightweight call, not a large data transfer" reasoning -- but unlike
	// those two fields, THIS timeout is spent inside a loop of
	// IdentityEmailFetchMaxAttempts attempts (platform.Retry, via
	// identitylink.FetchEmailWithRetry), run SYNCHRONOUSLY, inline, on the
	// Slack Events API webhook request path (internal/adapters/inbound/
	// slack/identity.go's own resolveSlackActor, called from handler.go's
	// handleEvent BEFORE thread<->session mapping, turn creation, or the
	// in-thread ack -- see that package's own doc.go for the full request-
	// handling order). So the REAL worst case this field feeds into is
	// IdentityEmailFetchMaxAttempts x IdentityEmailFetchTimeout PLUS every
	// backoff wait between attempts -- not the backoff waits in isolation.
	// At the OLD 10s/3-attempts value that real worst case was
	// 3x10s+600ms =~ 30.6s -- catastrophically over Slack's own real ~3s
	// "answer this webhook or it gets redelivered" budget (see slack's own
	// doc.go), for JUST this one step, with the entire rest of the
	// handler's own work (thread mapping, session/turn creation, the ack)
	// still to come in the SAME request. That fix lowered this field to
	// 300ms.
	//
	// HIGH audit fix ("300ms is unrealistically tight, inconsistent with
	// this codebase's own established precedent for the SAME call"): the
	// 300ms value above over-corrected -- it was picked purely to make the
	// retry-loop arithmetic fit Slack's ack budget, with no grounding in
	// real Slack/Linear API latency. This codebase's own CLOSEST precedent
	// for this exact call is SlackInteractivityIdentityFetchTimeout, below
	// -- it already budgets 800ms for a SINGLE, un-retried attempt at the
	// EXACT SAME users.info/GetUserEmail call, under an even TIGHTER
	// overall budget (SlackInteractivityAckTimeout, 2500ms, shared with a
	// real DB transaction AND a second outbound call). 300ms was 2.67x
	// tighter than that already-vetted "realistic minimum" number. The
	// concrete failure this invited: a provider answering in a genuinely
	// healthy, unremarkable ~400-600ms (very plausible real RTT/TLS
	// overhead, not evidence of anything broken, and a SYSTEMATIC
	// per-provider floor rather than independent jitter) would make ALL
	// IdentityEmailFetchMaxAttempts attempts time out IDENTICALLY every
	// single time -- retrying never helps against a consistent latency
	// floor above the per-attempt ceiling -- silently and PERMANENTLY
	// falling back to bot attribution for every message from every user on
	// an entirely healthy provider, while ALSO tripping M19's own Warn
	// log/identity_email_fetch_failures_total (see that counter's own doc
	// comment, internal/app/identitylink/retry.go) on every single one of
	// those messages: exactly the false-positive "the provider API is
	// broken" noise that counter exists to avoid, even though nothing was
	// actually broken.
	//
	// Raised to 800ms -- reusing SlackInteractivityIdentityFetchTimeout's
	// own already-vetted figure directly rather than inventing a new one,
	// since it is this codebase's own closest, already-defended precedent
	// for "one lightweight attempt at this exact call, under a tight
	// budget". This comfortably covers the realistic ~400-600ms
	// healthy-but-unremarkable case with genuine margin (200ms+) to spare,
	// so a normal, healthy provider succeeds within budget in the vast
	// majority of real-world cases, not just "technically fits the
	// arithmetic if everything is fast" -- while a real hang/dead provider
	// still fails within a bounded time so the retry loop can do its job.
	// Raising the per-attempt timeout back toward 10s-scale would blow the
	// ack budget again the way the ORIGINAL 10s value did, so
	// IdentityEmailFetchMaxAttempts was lowered from 3 to 2 (see that
	// field's own doc comment for why THIS lever, not a shorter per-attempt
	// timeout, was chosen) to keep the total worst case comfortably inside
	// Slack's real ~3s ack window -- see IdentityEmailFetchRetryBaseDelay/
	// IdentityEmailFetchRetryMaxDelay's own doc comment for the full
	// worst-case arithmetic and the headroom it leaves for the rest of the
	// handler's own work in the same request.
	IdentityEmailFetchTimeout time.Duration

	// IdentityEmailFetchMaxAttempts is how many times platform.Retry calls
	// the profile-email fetch before giving up (§13.2: "a provider
	// email-API failure is a retryable error, not an empty identity...
	// retry with backoff"). Not specified in the plan; originally chosen as
	// 3 -- enough to ride out a brief blip without indefinitely delaying
	// the webhook handler's own response (this whole retry loop runs
	// SYNCHRONOUSLY, inline, on the ingress request path -- see
	// internal/app/identitylink's own doc.go for why unbounded/background
	// retry, like domain/outbox's own persisted-schedule approach, is the
	// wrong shape for this specific call). The L5 audit fix kept this at 3
	// and fixed the real worst-case budget by shrinking the per-attempt
	// timeout instead (see IdentityEmailFetchTimeout's own doc comment) --
	// that shrink is what the HIGH audit fix below found unrealistically
	// tight.
	//
	// HIGH audit fix (see IdentityEmailFetchTimeout's own doc comment for
	// the full incident): once the per-attempt timeout was raised back to
	// a realistic 800ms, 3 attempts at 800ms no longer fits Slack's ~3s ack
	// budget with meaningful headroom (3x800ms alone is already 2.4s,
	// before any backoff wait or the rest of the handler's own work).
	// Lowered to 2 -- still genuine retry-with-backoff behavior (one retry
	// after the first failure), satisfying §13.2's own explicit
	// requirement, deliberately NOT reduced to 1 (which would remove retry
	// semantics the plan explicitly wants: a single blip on an otherwise
	// healthy provider would then never get a second chance). This is the
	// "reduce attempt count" lever, used instead of shrinking the
	// per-attempt timeout back down below a realistic figure -- see
	// IdentityEmailFetchTimeout's own doc comment for why THAT lever was
	// rejected as the primary fix. See IdentityEmailFetchRetryBaseDelay/
	// IdentityEmailFetchRetryMaxDelay's own doc comment for the full
	// worst-case arithmetic this field is one factor of.
	IdentityEmailFetchMaxAttempts int

	// IdentityEmailFetchRetryBaseDelay/IdentityEmailFetchRetryMaxDelay
	// configure platform.Retry's own doubling-capped-at-max backoff
	// between attempts -- mirrors domain/outbox.BackoffConfig's identical
	// shape, but MUCH shorter: this retry loop's own caller (a Slack/
	// Linear webhook handler) is still on the hook to answer promptly,
	// unlike outbox delivery's own background, persisted-schedule retry.
	//
	// Audit fix (L5, see IdentityEmailFetchTimeout's own doc comment for
	// the full incident): the PREVIOUS doc comment here claimed the worst
	// case "adds well under 1s of wall-clock time to the handler's own
	// critical path" -- true of the two backoff WAITS alone (200ms+400ms
	// at the old values), but this silently omitted that each of
	// IdentityEmailFetchMaxAttempts attempts is ITSELF bounded by
	// IdentityEmailFetchTimeout and can genuinely take that long if the
	// provider API hangs rather than erroring quickly -- the REAL worst
	// case is IdentityEmailFetchMaxAttempts x IdentityEmailFetchTimeout +
	// every backoff wait summed, not the backoff waits in isolation. This
	// mirrors how OutboxClaimDuration's own doc comment (audit fix H6) was
	// corrected to describe its real mechanism instead of an incomplete
	// one -- no more silent omission of the per-attempt timeout's own
	// contribution to the total.
	//
	// HIGH audit fix (see IdentityEmailFetchTimeout's own doc comment for
	// the full incident): with IdentityEmailFetchTimeout now 800ms and
	// IdentityEmailFetchMaxAttempts now 2, the REAL worst case -- 2
	// attempts at 800ms EACH genuinely timing out, plus the ONE backoff
	// wait between them, pessimistically at IdentityEmailFetchRetryMaxDelay
	// (150ms, a looser bound than the 100ms the actual, undoubled first
	// wait below would reach -- with only 2 attempts, platform.Retry's own
	// doubling never gets a second wait to double INTO) -- is
	// 2x800ms + 1x150ms = 1.75s total. That leaves 1.25s (~42%) of headroom
	// under Slack's own real ~3s webhook-ack budget (internal/adapters/
	// inbound/slack's own doc.go) for the REST of the handler's own work in
	// the same request (thread<->session mapping, turn creation, the
	// in-thread ack) -- comfortably more than the fast, no-network Postgres
	// work that remains actually needs, and a full 1.25s of absolute
	// margin, not just a technically-nonzero one. See internal/platform/
	// timeouts_test.go's own
	// TestDefaultTimeouts_IdentityEmailFetchWorstCaseTimingBudget, which
	// asserts this invariant directly against that external ~3s constant.
	// Linear's own equivalent path (internal/adapters/inbound/linear/
	// identity.go's own resolveActor) shares these SAME fields but has a
	// looser real budget of its own (~10s to post its one required
	// acknowledgment activity -- internal/adapters/inbound/linear's own
	// doc.go) -- comfortably protected by a fix sized for Slack's tighter
	// requirement, with no separate Linear-specific tuning needed.
	//
	// Chosen as 100ms/150ms (unchanged by the HIGH fix -- only the
	// attempt count and per-attempt timeout above needed retuning): the
	// ACTUAL (non-pessimistic) wait with only 2 attempts is a single
	// 100ms delay (the base; there is no second wait left to double into),
	// comfortably under the 150ms pessimistic bound used above.
	IdentityEmailFetchRetryBaseDelay time.Duration
	IdentityEmailFetchRetryMaxDelay  time.Duration

	// SlackInteractivityIdentityFetchTimeout bounds the ONE identity-
	// resolution profile-email fetch attempt Slack's own interactivity
	// path (internal/adapters/inbound/slack's decideAndUpdateMessage/
	// handleViewSubmission, interactive.go) allows itself, with
	// deliberately NO retry loop at all (unlike IdentityEmailFetchTimeout/
	// IdentityEmailFetchMaxAttempts' own general-purpose, retried budget,
	// used by the Events API ingress path instead) -- this path shares
	// Slack's own hard ~3s interactivity-ack window with DecidePlan's own
	// guarded-UPDATE transaction AND the chat.update call that reflects
	// its outcome (see SlackInteractivityAckTimeout's own doc comment),
	// so there simply isn't room for a multi-attempt backoff loop here. A
	// failed/timed-out fetch on this path defers to bot attribution for
	// THIS click; the SAME still-unlinked identity gets a full, properly-
	// retried resolution attempt the next time any OTHER event from it
	// arrives (an Events API message, a later click, a modal submission).
	// Not specified in the plan; chosen as 800ms -- comfortably inside
	// SlackInteractivityAckTimeout (2500ms) with real margin left for the
	// DecidePlan+chat.update calls that follow it in the same shared
	// budget.
	//
	// HIGH audit fix (see IdentityEmailFetchTimeout's own doc comment): THIS
	// field's own 800ms was later reused, deliberately and directly, as
	// IdentityEmailFetchTimeout's own retuned per-attempt value too -- this
	// field was already this codebase's own closest, real-latency-grounded
	// precedent for "one lightweight attempt at this exact users.info/
	// GetUserEmail call", so the fix for that OTHER field's own
	// unrealistically-tight 300ms simply pointed back at this number rather
	// than inventing a new one. The two fields still serve genuinely
	// different budgets (this one shares a tighter 2500ms window with a DB
	// transaction and a second outbound call; IdentityEmailFetchTimeout
	// shares a looser ~3s window but is spent across up to
	// IdentityEmailFetchMaxAttempts attempts) -- they now simply agree on
	// what a single realistic attempt at this call costs.
	SlackInteractivityIdentityFetchTimeout time.Duration

	// IdentityLinkPromptTTL is how long a magic-link identity_link_prompts
	// row (§13.2 step 4: "a short-lived magic link") stays valid before
	// GetIdentityLinkPromptByNonceHash's own caller (internal/adapters/
	// inbound/identitylink's magic-link consume handler) must treat it as
	// expired. Not specified in the plan beyond "short-lived"; chosen as
	// 24h -- long enough that a Slack/Linear user who doesn't immediately
	// click the link (e.g. it arrives outside working hours) still has a
	// realistic same-day-ish window, but still bounded, unlike
	// UserSessionTTL's own "stay signed in" 30-day figure, which answers a
	// genuinely different question (how long a browser stays logged in,
	// not how long a one-time linking action stays offered).
	IdentityLinkPromptTTL time.Duration

	// --- Audit-remediation (completeness-vs-plan lens, GitHub PR-payload-
	// correctness batch): no ordering relationship with either invariant
	// chain above (or with any prior standalone addition), so -- per those
	// additions' own precedent -- a plain field with a sensible default,
	// not wired into a fake invariant link.

	// GitHubGetPRTimeout bounds a single internal/adapters/outbound/
	// githubapi.Adapter.GetPullRequest call (a real outbound GET
	// https://api.github.com/repos/{owner}/{repo}/pulls/{number}), made
	// synchronously from inside internal/adapters/inbound/github's own
	// webhook handler (H5 audit fix: resolving an issue_comment mention's
	// TRUE head branch/repo, since that event type's own payload never
	// carries them directly -- see headresolve.go's own doc comment) --
	// mirrors PRCreateTimeout's/SlackAckTimeout's own identical "a genuine
	// outbound network call made inline in a webhook handler must never
	// run against an unbounded context" precedent exactly. Not specified
	// in the plan (this fix postdates it); chosen as 10s, generous for a
	// single lightweight GitHub REST GET while still keeping the whole
	// webhook response prompt.
	GitHubGetPRTimeout time.Duration

	// --- §19.3 standalone addition ("warm boot: fetch-aware git sync",
	// §19.3): no ordering relationship with either invariant chain above
	// (or with any prior Step's standalone additions), so -- per those
	// additions' own precedent -- a plain field with a sensible default,
	// not wired into a fake invariant link.

	// GitFetchStepTimeout bounds each individual network-bound git
	// subprocess internal/sandboxagent/gitclone.SyncAll now spawns as its
	// own new FIRST step, before the dirty-check/checkout/pop sequence
	// GitSyncStepTimeout already bounds (§19.3: "New step in syncOne,
	// before the dirty-check/checkout: `git fetch origin <resolved-branch>
	// <default-branch>`"): a `git ls-remote --symref origin HEAD` to
	// resolve the repo's real default-branch name, then one or two
	// `git fetch origin <ref>` calls (the default branch always; the
	// resolved session/explicit branch too, as a SEPARATE invocation when
	// it differs -- verified directly against real git, not assumed, that
	// a single `git fetch origin <branch> <default>` call is atomic: if
	// EITHER named ref does not exist on the remote, the whole invocation
	// fails with nothing fetched at all, which would silently deny
	// checkoutBranch's own origin/<default-branch> fallback preference
	// (§19.3 point 2) exactly in the common case -- an invented
	// "narvi/<sessionID>" branch that (by construction) almost never
	// exists upstream -- where that fallback matters most).
	//
	// A DISTINCT field from GitSyncStepTimeout is required, not a reuse of
	// it, because this is the ONE call this package makes that is
	// genuinely network-bound: every other git subprocess SyncAll spawns
	// (`git status`, `git stash push`/`pop`, `git rev-parse --verify`,
	// `git checkout`/`git checkout -b`) operates purely on the already-
	// on-disk repository (GitSyncStepTimeout's own doc comment: "every one
	// of these is local-only (no network)"). A local-only operation and a
	// real outbound git-over-HTTPS/SSH round trip to a remote host do not
	// share a realistic latency budget, exactly the same reasoning
	// RepoCloneTimeout already uses to stay a separate field from
	// GitSyncStepTimeout rather than collapsing the two.
	//
	// Not specified in the plan beyond "propose 90s, distinct from the
	// existing local-only 30s GitSyncStepTimeout" (§19.3 point 1); chosen
	// as exactly that proposed 90s -- generous enough that a real remote
	// under ordinary load, or a large default-branch delta on a first
	// warm-boot fetch against a genuinely stale image (§19.2's own
	// predicted 10-40 minute staleness window, kept small in practice only
	// because §19.1 bakes a FULL, non-shallow clone at build time), does
	// not spuriously trip the degrade policy (§19.3 point 3) merely for
	// running a bit long -- while still bounded well below
	// RepoCloneTimeout's own 5m (a fetch against an already-warm object
	// store is a much smaller delta than an initial full clone, so it
	// does not need that same budget).
	GitFetchStepTimeout time.Duration

	// --- §19.2 standalone addition ("warm boot: refresh pump + hook
	// policy", §19.2): no ordering relationship with either invariant chain
	// above (or with any prior Step's standalone additions), so -- per
	// those additions' own precedent -- a plain field with a sensible
	// default, not wired into a fake invariant link.

	// ImageRefreshCheckInterval is how often app/imagebuild.Builder's own
	// second pump phase (the freshness pump) polls every 'ready' shared
	// image_builds row, resolving each repo's CURRENT default-branch tip
	// and comparing it against that row's own built_repo_shas (§19.2).
	// §19.2 gives this explicitly: "propose 10 min" -- distinct from, and
	// much coarser than, ImageBuildPumpInterval's own 60s (that ticker
	// claims brand-new pending/failed builds, a comparatively urgent
	// warm-hit-vs-cold-boot concern; this one only ever affects an
	// ALREADY-ready row's own staleness window, which §19.2 itself frames
	// as "10-40 minutes... acceptable because staleness is no longer a
	// correctness boundary" -- ticking every 10 minutes is more than
	// frequent enough for a latency-only concern, and avoids hammering
	// GitHub's API with a tip-SHA check per repo per shared image far more
	// often than useful).
	ImageRefreshCheckInterval time.Duration

	// --- Audit fix standalone additions ("warm-boot image access
	// control", HIGH): no ordering relationship with either invariant
	// chain above (or with any prior Step's standalone additions), so --
	// per those additions' own precedent -- plain fields with sensible
	// defaults, not wired into a fake invariant link.

	// RepoAccessCheckTimeout bounds a single internal/app/ports.
	// SourceControl.CheckRepoAccess call (app/sessionactor's own
	// resolveAndSetImage, imageresolve.go) -- one real outbound GitHub
	// repo-info GET per repo, called once per repo IN A LOOP (unless
	// already cache-hit, see RepoAccessCacheTTL below), each bounded
	// individually so one slow/hanging repo can't stall the others
	// indefinitely. Not specified in the plan; chosen as 10s, matching
	// RepoSHAResolutionTimeout/ContractsFingerprintResolutionTimeout's own
	// identical "lightweight call, not a large data transfer" reasoning --
	// this is the exact same shape of call (one bounded outbound GitHub
	// GET), just answering a different question.
	RepoAccessCheckTimeout time.Duration

	// RepoAccessCacheTTL is how long a CheckRepoAccess verdict (positive OR
	// negative) is trusted before this gate re-checks it live, keyed per
	// (session creator, repo) -- required to keep this new gate off the
	// steady-state hot path (§19.1/§19.2) deliberately built:
	// unlike RepoSHAResolutionTimeout's own SHA resolution (drift-
	// sensitive, never cacheable -- §19.2's whole rationale for removing
	// it from the spawn path), repo ACCESS changes rarely, so it is safe
	// to cache with an ordinary TTL. Not specified in the plan; chosen as
	// 10 minutes, matching ImageRefreshCheckInterval's own identical
	// "acceptable staleness for an infrequently-changing, non-correctness-
	// critical-at-this-grain fact" reasoning (§19.2) -- a revoked user
	// keeps working for at most this long before their next spawn/restore
	// re-checks and denies, which is an acceptable staleness window for an
	// access grant that the platform's own upstream SCM (GitHub) is the
	// one enforcing in the first place.
	RepoAccessCacheTTL time.Duration

	// RepoAccessCheckBreakerWindow bounds the sliding window
	// repoAccessCache's own small in-memory circuit breaker (app/
	// sessionactor's repoaccesscache.go) counts consecutive INDETERMINATE
	// CheckRepoAccess failures within, before it trips and short-circuits
	// every further repo-access check straight to a deny -- WITHOUT
	// calling CheckRepoAccess again -- for the rest of this window.
	// Audit-remediation addition (correctness-availability, finding #5):
	// an indeterminate SCM failure (network/timeout/5xx, or a rate-limited
	// 403 -- see githubapi.isRateLimitedResponse) is deliberately never
	// CACHED (RepoAccessCacheTTL above governs only genuine, definitive
	// verdicts), which on its own means a SUSTAINED outage/rate-limit
	// event would otherwise make EVERY subsequent spawn, for EVERY session
	// with repos, pay the full per-repo RepoAccessCheckTimeout again and
	// again for as long as the incident lasts -- reintroducing exactly the
	// "up to len(repos) * timeout of sequential GitHub latency per spawn"
	// cost class a prior fix's own commit message states was removed. This
	// breaker does not change the deny outcome (still fail-closed, exactly
	// like an uncached indeterminate failure already was) -- it only stops
	// paying for the network round trip once failures are clearly
	// systemic, shedding load the same way domain/sandbox.
	// EvaluateCircuitBreaker already does for sandbox-provider spawn
	// failures (reused directly here rather than duplicated, since it is
	// already a pure, generically-parameterized decision function). Not
	// specified in the plan (this fix postdates it); chosen as 2 minutes
	// -- short enough that a resolved outage is noticed and re-tried
	// promptly, comfortably shorter than RepoAccessCacheTTL's own 10
	// minutes (this is damping repeated NETWORK CALLS during a transient
	// failure, not caching an access verdict, so it does not need -- and
	// should not have -- that same long a window).
	RepoAccessCheckBreakerWindow time.Duration
	// --- Audit-remediation batch B2 addition: closes the imagebuild
	// refresh-pump crash window (see internal/app/imagebuild/doc.go and
	// migrations/000041_image_builds_refresh_lease.up.sql). No ordering
	// relationship with any invariant chain above -- a standalone field,
	// matching every other standalone addition's own precedent.

	// ImageRefreshClaimStaleAfter bounds app/imagebuild.Builder's own
	// refresh_in_progress claim LEASE: ClaimImageBuildForRefresh (and
	// ListReadyImageBuilds' matching predicate) treat a claim whose
	// refresh_started_at is older than this as abandoned and reclaimable,
	// healing a control-plane crash/SIGTERM/pod-eviction that lands
	// between ClaimImageBuildForRefresh and RecordImageRefreshSuccess/
	// RecordImageRefreshFailure -- see migrations/
	// 000041_image_builds_refresh_lease.up.sql's own doc comment for why
	// this is a lease (a bound compared against the claim's OWN
	// timestamp) rather than a startup sweep keyed to this process's own
	// boot time, which cannot safely distinguish an abandoned claim from
	// another pod's still-live one in a multi-pod deployment.
	//
	// Not specified by any plan section (this Step's own crash window was
	// originally, incorrectly, documented as "self-healing by
	// construction" -- audit-remediation batch B2 makes that true instead
	// of removing the false claim); chosen as 30 minutes, matching
	// ImageBuildBackoffMax's own exact value and its own reasoning: the
	// outer bound a legitimately slow but genuinely still-running attempt
	// should ever need (a refresh's own BuildImage call is the identical
	// provider operation attempt's own claim-time build uses, per
	// refreshBatchSize's own doc comment), comfortably above any plausible
	// single real build's duration while still reclaiming a genuinely
	// abandoned claim within a small, bounded number of
	// ImageRefreshCheckInterval (10m) ticks rather than leaving it wedged
	// for hours.
	ImageRefreshClaimStaleAfter time.Duration

	// --- Audit-remediation batch B7 addition: bounds each binary's own
	// deferred OTel shutdown/flush (see each main.go's own shutdownOTel
	// call). No ordering relationship with any invariant chain above -- a
	// standalone field, matching every other standalone addition's own
	// precedent.

	// OTelShutdownTimeout bounds platform.SetupOTel's own returned shutdown
	// func wherever either binary calls it: cmd/sandbox-agent's own
	// deferred shutdownSandboxAgentOTel(ctx) call, and (as of §33)
	// cmd/control-plane's own deferred shutdownControlPlaneOTel(ctx) call --
	// each against a fresh background context, never that binary's own
	// long-lived one (see either main.go's own doc comment: by the time
	// either deferred call runs, that context may already be canceled).
	//
	// Originally scoped to sandbox-agent alone: a single boot+session
	// process for which this really is "the last chance before the process
	// exits", unlike control-plane's own identical-looking call, which was
	// deliberately left UNBOUNDED at the time -- a long-running daemon gets
	// another periodic export anyway even if one flush is somehow missed,
	// and a bare stdout write essentially never hangs. §33 removes that
	// asymmetry: once control-plane can point SetupOTel at a real OTLP
	// endpoint (Config.OTLPEndpoint), its own shutdown flush becomes a
	// genuine network call to an operator's collector, with a real hang
	// mode a stdout write never had -- a down/unreachable collector must
	// not be allowed to block that long-running daemon's own graceful exit
	// past its configured grace period either. This field now bounds both
	// calls identically. Without a bound, a backpressured stdout exporter
	// (a slow/blocked log collector, a full pipe buffer under load, ...) or
	// an unreachable OTLP collector would let metric.NewPeriodicReader/
	// tracerProvider.Shutdown's own synchronous flush block a deferred call
	// indefinitely, hanging process teardown past whatever grace period the
	// orchestrator expects. Not specified in the plan; chosen as 10s,
	// matching ShutdownGracePeriod/ProcessStopGracePeriod's own "not
	// specified; chosen" precedent for a bounded-but-generous final-
	// teardown wait -- generous enough for either an in-process stdout
	// write or one bounded network flush attempt.
	OTelShutdownTimeout time.Duration

	// --- Batch fix/deny-unlinked-github-actors addition: bounds the
	// anti-spam dedupe window for the "please sign in via GitHub OAuth"
	// reply internal/adapters/inbound/github's own handler.go now posts
	// when an unlinked commenter's mention is denied. No ordering
	// relationship with any invariant chain above -- a standalone field,
	// matching every other standalone addition's own precedent.

	// GitHubActorNoticeTTL bounds how long a
	// github_actor_link_notices.notified_at row (migrations/
	// 000043_github_actor_link_notices.up.sql) suppresses a repeat
	// "please sign in" reply to the SAME still-unlinked commenter on the
	// SAME PR. A deliberately DISTINCT constant from IdentityLinkPromptTTL
	// above, even though both are currently 24h and both gate a
	// GitHub-adjacent "tell the user to link their account" notice: that
	// field bounds how long a Slack/Linear magic-LINK stays clickable (an
	// auto-linking mechanism GitHub has no equivalent of, see
	// actorauthz.AuthorizeLinkedActor's own doc comment); this field
	// bounds how long this package waits before repeating an ORDINARY
	// comment reply that carries no bearer secret at all. Collapsing the
	// two into one shared constant would make a future, independent
	// change to either policy (e.g. shortening the magic-link expiry for
	// security reasons) silently also retune this unrelated anti-spam
	// window. Not specified in the plan (this fix postdates it); chosen
	// as 24h, matching IdentityLinkPromptTTL's own "not specified beyond
	// short-lived" reasoning -- long enough that a repeat mention within
	// the same working day doesn't re-spam the PR thread, short enough
	// that a genuinely still-unlinked commenter is reminded again on
	// their next active day rather than only once, ever.
	GitHubActorNoticeTTL time.Duration

	// --- §8.2 standalone addition ("review sessions", §8.2): no
	// ordering relationship with either invariant chain above (or with any
	// prior standalone addition), so -- per those additions' own precedent
	// -- a plain field with a sensible default, not wired into a fake
	// invariant link.

	// GitHubPRDiffTimeout bounds a single internal/adapters/outbound/
	// githubapi.Adapter.GetPullRequestDiff call -- a real outbound GET
	// against GitHub's own pulls/{number} endpoint, content-negotiated for
	// the raw unified-diff media type rather than pullRequestResponse's own
	// JSON shape. Made synchronously, inline, by whichever review-session
	// trigger path (a PR @mention, a label retrigger, or the manual
	// re-review REST button) is about to create or reuse a review turn --
	// mirrors GitHubGetPRTimeout's own identical "a genuine outbound
	// network call made inline in a webhook/request handler must never run
	// against an unbounded context" precedent. A DISTINCT field from
	// GitHubGetPRTimeout (not a reuse), because a PR's own diff can
	// legitimately be far larger than its plain JSON resource -- a large
	// refactor or a vendored/generated-file change can genuinely take
	// longer to transfer than the small pullRequestResponse payload
	// GitHubGetPRTimeout bounds. Not specified in the plan (this Step
	// postdates it); chosen as 20s -- double GitHubGetPRTimeout's own 10s,
	// generous for a real, possibly-large diff transfer while still
	// keeping the triggering request/webhook response prompt. This fetch
	// is always best-effort (internal/app/reviewcontext.Fetch's own doc
	// comment): a timeout here degrades to "no pre-fetched diff", never a
	// reason to fail the review session's own turn creation.
	GitHubPRDiffTimeout time.Duration

	// --- §15 standalone addition ("release PR review", §15.2): no
	// ordering relationship with either invariant chain above (or with any
	// prior standalone addition), so -- per those additions' own precedent
	// -- a plain field with a sensible default, not wired into a fake
	// invariant link.

	// GitHubListMergedBetweenTimeout bounds ONE internal/adapters/outbound/
	// githubapi.Adapter.ListMergedBetween call -- a caller wraps its own
	// ctx with this before calling, mirroring GitHubPRDiffTimeout's own
	// identical "a genuine outbound network call must never run against
	// an unbounded context" precedent. Distinct from (and MUCH more
	// generous than) every other GitHub-flavored timeout in this struct:
	// ListMergedBetween is not one lightweight REST call but a bounded
	// SEQUENCE of them -- one compare, one revert search, plus up to six
	// further calls per discovered constituent PR, capped at
	// githubapi.maxConstituentPRs (100) -- so this budget covers the
	// WHOLE sequence, never a single request. Not specified in the plan
	// (this Step postdates it); chosen as 2 minutes: generous enough for
	// a real release cut bundling dozens of PRs against GitHub's real API
	// latency, while this check is always best-effort on the caller's own
	// side (internal/app/releasereview) -- a timeout here degrades to "no
	// manifest posted for this release PR", never a reason to fail
	// session creation itself.
	GitHubListMergedBetweenTimeout time.Duration

	// --- Blocking-finding fix #1 standalone addition ("release PR
	// review", §15.2): no ordering relationship with either invariant
	// chain above (or with any prior standalone addition) EXCEPT the one
	// explicit pairwise check Validate() adds just for this pair below --
	// per those additions' own precedent, otherwise plain fields with
	// sensible defaults.

	// ReleaseManifestCheckPumpInterval is how often
	// internal/app/releasereview.Worker (the background loop that claims
	// release_manifest_pending rows and runs the actual manifest check --
	// see migrations/000050_release_manifest_pending.up.sql's own doc
	// comment for the full "why") polls for rows to claim -- mirrors
	// OutboxPumpInterval's own ticker-driven shape. Not specified in the
	// plan (this fix postdates it); chosen as 10s -- a release PR's own
	// manifest check is not a live notification a human is actively
	// watching a chat thread for the way an ordinary outbox row is
	// (OutboxPumpInterval's own 5s reasoning), so this can poll a little
	// less aggressively without meaningfully delaying a maintainer who
	// will spend minutes reviewing the release PR regardless.
	ReleaseManifestCheckPumpInterval time.Duration

	// ReleaseManifestCheckTimeout bounds ONE internal/app/releasereview.
	// Worker attempt at running the actual manifest check
	// (internal/app/releasereview.Run) for a single claimed row -- the
	// SAME call the webhook handler used to make inline, now run on the
	// Worker's own long-lived loop instead, decoupled from any webhook
	// request's own context/lifetime (blocking-finding fix #1). MUST
	// stay comfortably above GitHubListMergedBetweenTimeout (enforced by
	// Validate() below): Run's own inner context.WithTimeout(ctx,
	// GitHubListMergedBetweenTimeout) call takes the EARLIER of its own
	// duration and whatever deadline this outer context already carries
	// (context.WithTimeout's own documented behavior) -- a
	// ReleaseManifestCheckTimeout shorter than GitHubListMergedBetweenTimeout
	// would silently truncate the very budget that field's own doc
	// comment describes, defeating the point of that generous 2-minute
	// allowance. Not specified in the plan; chosen as 3 minutes --
	// GitHubListMergedBetweenTimeout's own 2 minutes plus a full extra
	// minute of margin for the JSON-marshal/outbox-insert work Run does
	// after ListMergedBetween itself returns.
	ReleaseManifestCheckTimeout time.Duration

	// --- §3.5 standalone addition ("automations: engine", §3.5): the
	// two sweep thresholds are explicit in the plan ("orphaned starting
	// runs >5 min, running >90 min"); the two poll intervals are not, and
	// -- per every prior pump-interval addition's own precedent
	// (ReconcilerInterval/ImageBuildPumpInterval/OutboxPumpInterval) --
	// are plain fields with a sensible default, wired into ONE new
	// invariant check each below (mirroring ReconcilerInterval's own
	// pairwise link with ReconcilerOrphanConfirmationPeriod exactly): the
	// sweep must poll comfortably more often than the shortest threshold
	// it is responsible for confirming, or a run that just crosses
	// AutomationRunStartingOrphanThreshold could sit unswept for far
	// longer than that threshold's own name implies.

	// AutomationEnginePumpInterval is how often internal/app/automation.
	// Engine's own main loop polls for pending invocations to fan out and
	// in-flight runs to reconcile against their linked sessions' turn
	// history -- mirrors ReconcilerInterval/ImageBuildPumpInterval's own
	// ticker-driven shape. Not specified in the plan; chosen as 60s,
	// matching ReconcilerInterval's own cadence -- an automation's own run
	// is not a live chat a human is actively watching (OutboxPumpInterval's
	// own 5s reasoning does not apply here), so this can poll a good deal
	// less aggressively without meaningfully delaying a fire-and-forget
	// background job.
	AutomationEnginePumpInterval time.Duration

	// AutomationSweepInterval is how often internal/app/automation.Engine's
	// own recovery-sweep loop polls for orphaned runs (§3.5's own two
	// sweep thresholds, immediately below) -- mirrors
	// ReconcilerOrphanConfirmationPeriod's own debounce-poll shape, just
	// applied to a threshold read directly off a persisted timestamp
	// rather than an in-memory first-seen map (app/automation's own sweep
	// needs no cross-tick debounce state the way app/reconciler's orphan
	// confirmation does -- a run's own started_at/running_at is already
	// durable, so "has this been true continuously" is answered by a
	// single comparison against `now`, not by remembering what a PRIOR
	// tick observed). Not specified in the plan; chosen as 60s, matching
	// AutomationEnginePumpInterval's own cadence -- comfortably below
	// AutomationRunStartingOrphanThreshold (5 min) with wide margin
	// (enforced by Validate() below), so a run that just crosses either
	// threshold is reaped within roughly one tick, not many.
	AutomationSweepInterval time.Duration

	// AutomationRunStartingOrphanThreshold is §3.5's own explicit sweep
	// threshold: "orphaned starting runs >5 min" -- a run whose own
	// started_at is older than this, with automation.DeriveRunStatus still
	// reporting RunStatusStarting, is swept to RunStatusFailed via
	// automation.RunTriggerOrphanTimeout. §3.5, explicit.
	AutomationRunStartingOrphanThreshold time.Duration

	// AutomationRunRunningOrphanThreshold is §3.5's own explicit sweep
	// threshold: "running >90 min" -- a run whose own running_at is older
	// than this, with automation.DeriveRunStatus still reporting
	// RunStatusRunning, is swept the same way. §3.5, explicit.
	AutomationRunRunningOrphanThreshold time.Duration

	// AutomationCronGranularity is §8.4's own ("automations: triggers &
	// extras", §8.4) cron-trigger evaluation bucket size: internal/domain/
	// automation.CronMatches evaluates a schedule against a whole UTC
	// minute, and internal/app/automation's own trigger pump (triggerpump.go)
	// truncates `now` to this same duration to compute the CAS-guarded
	// "already fired for this bucket" key (automations.last_cron_fired_at).
	// Not a tunable knob the way the pump intervals above are -- it is a
	// STRUCTURAL constant (a standard 5-field cron schedule's own finest
	// resolution IS one minute, by definition of the format itself, not a
	// choice this codebase made) -- but every time.Duration unit literal
	// in this codebase lives here regardless (§5.4/§11, mechanically
	// enforced by tools/lint/narvichecks/notimeliteral), so this is that
	// one literal's own single home, referenced by name everywhere else
	// rather than written as a bare `time.Minute` a second time.
	AutomationCronGranularity time.Duration

	// AutomationCronCatchUpWindow is the review-fix companion to
	// AutomationCronGranularity above (adversarial-review finding: "cron
	// trigger pump has no catch-up for missed evaluations"): how far back a
	// control-plane restart or a stalled tick may backfill missed cron
	// minutes. Before this field existed, EvaluateCronTriggersOnce
	// (app/automation's own triggerpump.go) compared ONLY the exact current
	// instant against each schedule (domainautomation.CronMatches) -- a
	// point-in-time predicate, not a range query -- so any gap between two
	// consecutive evaluations wider than one AutomationEnginePumpInterval
	// tick (a routine deploy, a slow tick, a GC pause) silently and
	// permanently lost that minute's own fire, with no log line and no
	// retry path. Every other time-driven path in this codebase
	// (session_timers' fires_at <= now(), outbox's next_attempt_at <=
	// now(), image_builds' next_retry_at <= now()) is inherently
	// catch-up-safe after downtime by virtue of being a range/threshold
	// query -- this field makes cron trigger evaluation the same way:
	// app/automation's own trigger pump now evaluates the WINDOW
	// (max(last_cron_fired_at, now-AutomationCronCatchUpWindow), now] via
	// domainautomation.CronMatchesWithin, firing at most once per tick even
	// if multiple buckets inside that window matched.
	//
	// Deliberately NOT unbounded ("catch up from whenever this automation
	// last fired, no matter how long ago") -- an automation whose own
	// engine was down for days should not suddenly fire once for a
	// schedule that silently accumulated a long backlog; capping the
	// look-back keeps a legitimately long outage's own catch-up behavior
	// identical to a short one (fire at most once, for the most recent
	// missed bucket still inside the window), rather than scaling with
	// outage length. 10 minutes is chosen as comfortably wider than a
	// single AutomationEnginePumpInterval tick (60s) plus real-world
	// restart/rolling-deploy latency, while still being "the same day,
	// basically immediately" from an operator's own point of view -- not
	// specified by the plan, so this codebase's own established
	// "generous, round, clearly-justified default" precedent applies
	// (ProviderHTTPClientTimeout, OutboxClaimDuration, etc.).
	AutomationCronCatchUpWindow time.Duration

	// --- §4.1 standalone additions ("RWX provider + previews", §4.1.1):
	// RWXCLIExecTimeout has no ordering relationship with either invariant
	// chain above (or with any prior standalone addition), so — per those
	// additions' own precedent — a plain field with a sensible default, not
	// wired into a fake invariant link. RWXSandboxInactivityTimeout DOES
	// need its own pairwise check against ActorIdleTTL (see Validate()
	// below), so it is not a plain standalone addition either — mirroring
	// ReconcilerOrphanConfirmationPeriod's own identical "named in this
	// same standalone-additions block, but still gets a real invariant
	// check" precedent.

	// RWXCLIExecTimeout bounds ONE internal/adapters/outbound/rwx subprocess
	// invocation of the pinned `rwx` CLI (sandbox `start|exec|stop|reset|
	// list`, always with `--format json`) — every Provider method that
	// shells out wraps its own call with this bound, mirroring
	// RepoCloneTimeout's own "a real subprocess must never run against an
	// unbounded context" precedent. RWX's own published latency claims
	// ("spin up in seconds, not minutes") are directional marketing copy,
	// not an engineering p99 (§4.1.1) — no real pinned binary is reachable
	// from this codebase's own tests/CI to measure one empirically. Not
	// specified in the plan; chosen as 2 minutes: comfortably above
	// SnapshotMintTimeout's own 60s (a lighter, single provider API round
	// trip) since `rwx sandbox start` provisions an actual VM, while still
	// far below RepoCloneTimeout's 5m — a generous ceiling for a claimed-fast
	// operation, not an expected duration; real p99s replace this once
	// measured against a real account (§4.1.3).
	RWXCLIExecTimeout time.Duration

	// RWXSandboxInactivityTimeout is the value internal/adapters/outbound/
	// rwx.Provider.CreateSandbox passes as `rwx sandbox start`'s own
	// `--inactivity-timeout` flag — deliberately set ABOVE Narvi's own
	// session-idle authority (§2's ActorIdleTTL, 30 min; and
	// InactivityTimeout, 10 min, this package's own existing
	// "ready, non-processing sandbox" stop timer) rather than reused from
	// either, so NARVI's timers — not RWX's own auto-stop — are what
	// normally decide a session's idleness first (§4.1.1: "set above
	// Narvi's own session-idle authority... so Narvi's timers, not RWX's,
	// decide idleness"). A provider-initiated auto-stop that fires anyway
	// (e.g. Narvi's own timer pump missed a tick) is an ordinary, expected
	// entry into §3.2's `stopped` state feeding the resume/recreate lane —
	// never treated as a failure. Enforced with margin against ActorIdleTTL
	// by Validate() below. Not specified in the plan; chosen as 45 minutes
	// — comfortably above ActorIdleTTL's 30-minute ceiling (which already
	// exceeds InactivityTimeout's own 10 minutes, so clearing the larger of
	// the two named authorities clears both), while staying well below
	// ProviderHardCap's 2-hour ceiling.
	RWXSandboxInactivityTimeout time.Duration

	// --- §8.6 standalone additions ("uploads, blob storage & the
	// in-sandbox download_file tool", §28): UploadPresignPutTTL,
	// UploadPresignGetTTL, and ObjectStoreHTTPClientTimeout have no
	// ordering relationship with any invariant chain above (or with each
	// other), so — per the RWX-additions block's own precedent — each is a
	// plain field with a sensible default, not wired into a fake
	// invariant link. UploadAbandonmentSweepInterval DOES need a real
	// pairwise check against UploadPendingSweepAfter (see Validate()
	// below), mirroring AutomationSweepInterval's own identical
	// "standalone block, but still gets a real invariant check" precedent.

	// UploadPresignPutTTL is how long a presigned upload PUT URL remains
	// valid (§28.4: "propose 15 min — generous for the size cap on a slow
	// link, the same 'chosen generously when the concrete cost is
	// unknown' convention HookTimeout documents"). Passed to
	// ports.BlobStore.PresignPut's own PresignPutSpec.TTL by the mint
	// handler — the objstore adapter holds no timeout literal of its own
	// (§11's grep-test).
	UploadPresignPutTTL time.Duration

	// UploadPresignGetTTL is how long a presigned download GET URL
	// (the download_file tool's own 302 target, §28.5) remains valid.
	// §28.5: "propose 5 min". Deliberately much shorter than
	// UploadPresignPutTTL: a GET redirect is followed within the same
	// curl invocation that requested it, never left open on a slow link
	// the way an upload PUT might be.
	UploadPresignGetTTL time.Duration

	// UploadPendingSweepAfter is how old a `pending` upload artifact row
	// must be before the abandonment sweep marks it `failed(abandoned)`
	// and outboxes a blob_delete (§28.4: "propose 24 h... a browser that
	// minted and walked away costs one row and one sweep pass, nothing
	// more").
	UploadPendingSweepAfter time.Duration

	// UploadAbandonmentSweepInterval is the sweep's own tick cadence
	// (§28.4: "its own named interval in platform/timeouts.go, propose
	// 15 min"), independent of UploadPendingSweepAfter's threshold value
	// except for the margin Validate() checks below — mirrors
	// AutomationSweepInterval's own relationship to
	// AutomationRunStartingOrphanThreshold exactly.
	UploadAbandonmentSweepInterval time.Duration

	// ObjectStoreHTTPClientTimeout bounds ONE internal/adapters/outbound/
	// objstore Stat or Delete call (the adapter's only two real network
	// calls — PresignPut/PresignGet are pure local SigV4 signing with no
	// network round-trip at all, per blobstore.go's own doc comment, so
	// neither needs this timeout). Not specified in the plan; chosen as
	// 10s, matching RepoSHAResolutionTimeout/ContractsFingerprintResolutionTimeout's
	// own "lightweight call, not a large data transfer" reasoning — Stat
	// is a HeadObject, Delete a DeleteObject, neither moves object bytes.
	ObjectStoreHTTPClientTimeout time.Duration

	// --- §8.8 standalone additions ("models: Codex via ChatGPT-account
	// OAuth", §29.5/§29.9): no ordering relationship with either invariant
	// chain above (or any prior Step's own standalone additions), so —
	// per those additions' own precedent — plain fields with sensible
	// defaults, not wired into a fake invariant link.

	// ChatGPTOAuthRefreshMargin is how far ahead of an oauth-kind provider_
	// credentials row's own oauth_expires_at the refresh pump (§29.5)
	// starts trying to refresh it — i.e. the pump's own claim query is
	// "oauth_expires_at < now() + ChatGPTOAuthRefreshMargin". §29.5 gives
	// this explicitly: "propose 72h ... against the verified 10-day access
	// lifetime and OpenAI's own ~8-day staleness refresh in Codex CLI" —
	// generous enough that a served access token always has comfortably
	// more than ChatGPTOAuthRefreshPumpInterval of validity left even if a
	// single pump tick is missed.
	ChatGPTOAuthRefreshMargin time.Duration

	// ChatGPTOAuthRefreshPumpInterval is the refresh pump's own tick
	// cadence — mirrors OutboxPumpInterval/ReconcilerInterval's own
	// ticker-loop shape (outboxworker.Builder.Run, internal/app/
	// reconciler.Reconciler.Run), just far less frequent, since the whole
	// point of a 72h margin against a 10-day lifetime is that this pump
	// does not need to run often. §29.5 gives this explicitly: "propose
	// 6h".
	ChatGPTOAuthRefreshPumpInterval time.Duration

	// ChatGPTLinkAttemptTTL is a DEFENSIVE CAP on how long a single
	// device-flow link attempt (chatgpt_link_attempts row) stays valid --
	// NOT the primary source of its expiry. auth.openai.com's own
	// usercode response carries a real expires_at field (live-verified by
	// internal/adapters/outbound/chatgptoauth's own usercode canary,
	// despite §29.2's own original field list never naming it), which
	// internal/app/chatgptlink.StartLink uses directly as authoritative;
	// this field only clamps that server-provided value from above, so a
	// wildly-off or malicious response can never keep an attempt "live"
	// far longer than a human would plausibly still be mid-flow. Not
	// specified in the plan; chosen generously as 15m, mirroring this
	// codebase's own "chosen generously when the concrete cost is
	// unknown" convention (HookTimeout's own doc comment) and, numerically,
	// UploadPresignPutTTL's own identical "propose 15 min" figure for an
	// unrelated but similarly-shaped "how long can a human take" bound.
	ChatGPTLinkAttemptTTL time.Duration

	// ChatGPTOAuthHTTPClientTimeout bounds every outbound HTTP call this
	// Step's own new device-flow adapter (internal/adapters/outbound/
	// chatgptoauth) makes directly to auth.openai.com — the 4 calls §29.2/
	// §29.9 name (usercode, token-poll, code exchange, refresh). Not
	// specified in the plan; chosen generously as 15s — a real third-party
	// OAuth endpoint over the public internet, more generous than this
	// codebase's own "lightweight internal call" precedents
	// (RepoSHAResolutionTimeout/ContractsFingerprintResolutionTimeout/
	// ObjectStoreHTTPClientTimeout, all 10s) since neither this adapter's
	// own latency nor auth.openai.com's is under Narvi's control the way a
	// same-datacenter Postgres/internal-service call's is.
	ChatGPTOAuthHTTPClientTimeout time.Duration

	// --- §16 ("decision inbox: read model + API", §16) standalone
	// additions: no ordering relationship with any invariant chain above,
	// matching every other standalone addition's own precedent.

	// GitHubListOpenPRsForUserTimeout bounds ONE internal/adapters/
	// outbound/githubapi.Adapter.ListOpenPRsForUser call, from the app-
	// layer decision-inbox aggregator (§16.2) -- this is the SAME
	// "wrap the whole multi-call port method in one outer
	// context.WithTimeout at its one real call site" shape
	// GitHubListMergedBetweenTimeout already establishes for
	// ListMergedBetween (run.go's own listCtx), not a NEW pattern.
	// ListOpenPRsForUser's own worst case (this file's own top doc
	// comment, listopenprs.go) is comparable to or worse than
	// ListMergedBetween's (up to maxOpenPRsForUser candidate PRs, each
	// costing up to five further calls, versus ListMergedBetween's own
	// maxConstituentPRs bound) -- chosen as 3 minutes, matching
	// ReleaseManifestCheckTimeout's own identical figure for the same
	// class of "bounded but potentially many-call" outbound operation.
	GitHubListOpenPRsForUserTimeout time.Duration

	// GitHubResolveCodeOwnersTimeout bounds ONE ResolveCodeOwners call --
	// a materially CHEAPER operation than ListOpenPRsForUser above (up to
	// three candidate-location file fetches, plus one lookup per DISTINCT
	// owner/team actually named on a matching CODEOWNERS line -- typically
	// a handful, never per-changed-file) -- chosen as 30s, matching
	// GitHubPRDiffTimeout's own "double the lightweight-call baseline"
	// reasoning for an outbound sequence a bit heavier than a single GET
	// but nowhere near ListOpenPRsForUser's own worst case.
	GitHubResolveCodeOwnersTimeout time.Duration

	// GitHubMergePRTimeout bounds ONE MergePR call -- a single PUT, but to
	// an endpoint GitHub's own docs note can itself take a moment to
	// perform the merge server-side (unlike a plain metadata GET/POST);
	// chosen as 15s, half again GitHubGetPRTimeout's own 10s "lightweight
	// call" baseline rather than reusing it outright, since this call
	// both writes and is on this Step's own interactive, human-facing
	// path (§16.2's Merge endpoint) where a too-short timeout would
	// misreport a slow-but-succeeding merge as a failure.
	GitHubMergePRTimeout time.Duration

	// DecisionInboxSCMCacheTTL is §16.2's own "SCM data is cached with a
	// short TTL, and the response carries its as-of timestamp" -- shared
	// by every SCM-derived read the decision-inbox aggregator caches
	// (ListOpenPRsForUser's own result set, and each distinct
	// ResolveCodeOwners lookup), keyed independently per call (see
	// internal/app/decisioninbox's own cache). Not specified numerically
	// in the plan beyond its own worked example -- §16.2's own prose
	// gives "as of 2 min ago" as its illustrative staleness figure, which
	// this field's default reproduces exactly rather than picking an
	// unrelated round number: short enough that a human looking at their
	// own inbox is never staring at meaningfully stale CI/review state,
	// long enough that opening the inbox twice in quick succession (or
	// two people loading it moments apart) does not repeat this Step's
	// own genuinely expensive multi-call SCM fetch (this file's own
	// GitHubListOpenPRsForUserTimeout doc comment) on every single load.
	DecisionInboxSCMCacheTTL time.Duration

	// DecisionInboxStaleAfter is §16.1's own "stale items (>48h,
	// configurable) visually flagged" -- the threshold internal/domain/
	// decisioninbox.IsStale compares a row's own age against. §16.1 gives
	// the figure directly: 48 hours.
	DecisionInboxStaleAfter time.Duration

	// DecisionInboxLatencyWindow bounds how far back §16.2's own
	// decision-latency metric looks for already-decided items -- §21.1's
	// "explicit active/recent window... never an unbounded scan"
	// discipline, applied to the metric's own query. It lives here rather
	// than as a package const beside the metric because this file is
	// where every duration this project tunes lives, window-shaped ones
	// included: HMACWindow and CircuitBreakerWindow are both spans of
	// past time rather than deadlines on a single operation, and the
	// notimeliteral check treats them all alike.
	DecisionInboxLatencyWindow time.Duration

	// -- §21 ("review verdict persistence, analytics, digest &
	// automated approval", §21) --

	// ReviewVerdictAnalyticsWindow bounds every §21.1 analytics rollup
	// (timeseries, top-risk-driver breakdown, the finding-outcome KPI)
	// AND §21.2 stage 2's own contradiction-rate calibration read model
	// -- the SAME "explicit active/recent window... never an unbounded
	// scan" discipline DecisionInboxLatencyWindow already applies one
	// Step up, at the SAME 30-day figure (a month of history is long
	// enough for a stable rollup, bounded per §21.1, and this Step's own
	// read model is a direct sibling of that one).
	ReviewVerdictAnalyticsWindow time.Duration

	// AutoMergePumpInterval is how often internal/app/automerge.Worker's
	// own background tick runs (§21.2 stage 2) -- mirrors
	// AutomationEnginePumpInterval's own identical shape and reasoning: a
	// periodic background policy engine, not a live chat a human is
	// actively watching (OutboxPumpInterval's own near-real-time 5s is
	// the wrong comparison here).
	AutoMergePumpInterval time.Duration

	// AutoMergeCandidateLookback bounds how far back internal/app/
	// automerge.Worker's own DISCOVERY query (review_verdicts.
	// ListLatestAutoApproved) looks for candidate PRs -- a verdict older
	// than this is unlikely to still name an open, mergeable PR, and
	// every candidate is re-confirmed LIVE regardless (RevalidateForAutoMerge,
	// §21.2's own reused re-validation contract) before anything merges,
	// so this window only ever bounds a cheap discovery scan's own cost,
	// never eligibility itself.
	AutoMergeCandidateLookback time.Duration

	// DigestPumpInterval is how often internal/app/digest.Pump's own
	// background tick checks whether today's digest is due (§21.3) --
	// deliberately much coarser than AutoMergePumpInterval/
	// OutboxPumpInterval: a digest fires at most once per channel per
	// day (digest_send_state's own claim-before-act guarantee), so
	// polling for "is it time yet" every few minutes is ample.
	DigestPumpInterval time.Duration

	// DigestChannelDiscoveryLookback bounds how far back internal/app/
	// digest's own channel-discovery query (joining slack_thread_sessions/
	// linear_agent_sessions through github_pr_sessions, §21.3) looks for
	// "which channels has this repo's review activity actually reached
	// recently" -- the SAME 30-day figure as ReviewVerdictAnalyticsWindow/
	// DecisionInboxLatencyWindow above, for the same "a month is long
	// enough to be representative, bounded per §21.1" reasoning.
	DigestChannelDiscoveryLookback time.Duration

	// DigestContentWindow bounds the digest's own ROLLUP content -- a
	// DIFFERENT, much narrower window than DigestChannelDiscoveryLookback
	// above (which only decides "is this channel still relevant", not
	// "what to report"): §21.3 names this a DAILY digest, so this is one
	// calendar day (24h) of review_verdicts/auto_approval_outcomes
	// activity, exactly "yesterday's" worth, matching the mundane,
	// expected meaning of "daily digest".
	DigestContentWindow time.Duration

	// -- §22 fix ("review: learned false-positive patterns" /
	// content-anchored finding positioning, §22.1.1) -- no ordering
	// relationship with either invariant chain above (or with any prior
	// Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into
	// a fake invariant link.

	// FindingPositionResolveAllTimeout bounds internal/app/findingposition.
	// ResolveAll's own WHOLE relocation-fallback loop (every unmatched
	// finding's own resolver.Resolve call, run serially), NOT a single
	// call -- Resolver.Resolve's own doc comment is explicit that ONE call
	// deliberately relies on the underlying ports.LLM client's own
	// configured request timeout (the SAME client/config
	// IntentClassifierLLMTimeout already bounds at 10s, cmd/control-plane/
	// main.go), never a second, redundant per-call wrap. Without an
	// aggregate ceiling on the LOOP, N unmatched findings could block
	// httpapi.PostReviewVerdict's own synchronous, pre-transaction
	// verdict-POST handler for up to N times that per-call budget, and a
	// client cancelling mid-loop would then lose the WHOLE verdict, not
	// just its position data. Not specified in the plan (this fix
	// postdates it); chosen as 45s -- generous enough for roughly four
	// full per-finding relocation calls at IntentClassifierLLMTimeout's
	// own 10s ceiling, while keeping the worst-case added latency on this
	// synchronous request path well short of a full minute.
	FindingPositionResolveAllTimeout time.Duration

	// -- §24 ("review: automatic re-review on new commits", §24) --
	// no ordering relationship with either invariant chain above (or with
	// any prior Step's standalone additions), so -- per those additions'
	// own precedent -- a plain field with a sensible default, not wired
	// into a fake invariant link.

	// ReviewRetriggerDebounce is the trailing-edge debounce window §24.2
	// names explicitly: internal/adapters/inbound/github/
	// pullrequestsynchronize.go re-arms the review_retrigger_debounce
	// named timer (session_timers, §2) to now() + ReviewRetriggerDebounce
	// on EVERY `pull_request`/`synchronize` webhook event for a PR with a
	// review session, so a burst of pushes (a rebase, a sequence of
	// fixup commits) reviews once, at the burst's own final head, only
	// after this long a quiet period -- never once per push, and never
	// the first push in a burst (§24.2's own emphatic "leading-edge
	// throttling... recreates exactly the problem this feature exists to
	// solve"). Not specified in the plan; chosen as 2 minutes -- long
	// enough that a human pushing a short sequence of fixup commits a few
	// seconds apart (the common "oops, one more tweak" pattern this
	// debounce exists to collapse) reliably lands inside one quiet
	// window, while still short enough that a genuinely single push gets
	// reviewed promptly rather than sitting for many minutes with no
	// visible cause.
	ReviewRetriggerDebounce time.Duration

	// -- §26.5 ("review: wire the cost budget", §26.7/§26.9) -- no
	// ordering relationship with either invariant chain above (or with any
	// prior Step's standalone additions), so -- per those additions' own
	// precedent -- a plain field with a sensible default, not wired into a
	// fake invariant link.

	// ReviewCostBudgetServerReadHeaderTimeout bounds cmd/sandbox-agent's
	// own FIRST HTTP server (reviewcostbudgetserver.go) -- a tiny,
	// loopback-only (127.0.0.1) listener serving GET /review-cost-budget,
	// the real production call site internal/domain/reviewtriage.
	// ShouldSkipOptionalPass has never had one. Applied as
	// http.Server.ReadHeaderTimeout, guarding against a slow/stalled
	// client leaving a connection's headers half-read indefinitely. Not
	// specified in the plan; chosen as 5s -- matches
	// RepoSHADiscoveryTimeout/CredentialFetchTimeout's own "a very minor,
	// sub-second, purely local operation" precedent: the ONLY client this
	// server will ever see is the review agent's own tool use (bash/curl)
	// calling straight to 127.0.0.1 inside the same sandbox, never a real
	// network hop. This server's own graceful Shutdown, by contrast, reuses
	// SupervisorShutdownTimeout's own already-bounded shutdownCtx (main.go)
	// rather than adding a second, near-duplicate shutdown field -- it is
	// sequenced into that SAME bounded teardown window, not a separate one.
	ReviewCostBudgetServerReadHeaderTimeout time.Duration

	// SandboxSecretFetchTimeout ("sandbox secrets & opencode
	// config", §27.1) bounds a SINGLE ATTEMPT at CP's
	// /sessions/{id}/sandbox-secrets delivery endpoint
	// (internal/sandboxagent/credentials.CPClient.FetchSandboxSecrets),
	// tried up to SandboxSecretFetchMaxAttempts times (below) at boot,
	// before the first hook runs and before `opencode serve` spawns
	// (cmd/sandbox-agent's own fetchSandboxSecrets, sandboxsecrets.go).
	// Not specified in the plan; chosen the SAME as
	// ProviderCredentialFetchTimeout's own 10s -- mirrors that field's own
	// "lightweight mint call, not a large data transfer" reasoning
	// exactly: this call resolves and decrypts at most a handful of
	// name-keyed rows server-side, comparably lightweight. Deliberately
	// its own field, not a reuse of ProviderCredentialFetchTimeout -- the
	// two calls hit different CP endpoints for a different secret-storage
	// table, and mirroring a SEPARATE field per delivery endpoint is this
	// codebase's own established precedent (ProviderCredentialFetchTimeout's
	// own doc comment makes the identical "deliberately its own field"
	// choice against CredentialFetchTimeout).
	//
	// Adversarial-review MEDIUM fix (§27.1 explicitly says "with bounded
	// retry" -- an earlier version of this Step made exactly one attempt,
	// silently dropping every sandbox secret for a session's WHOLE
	// lifetime on one transient blip, e.g. a control-plane rolling
	// restart or an ingress 502, likeliest exactly at boot when CP is
	// under spawn-burst load): this field's own MEANING changed from
	// "the one and only attempt's budget" to "one attempt's budget,
	// tried up to SandboxSecretFetchMaxAttempts times" -- its VALUE is
	// unchanged (10s remains a reasonable single-attempt budget for this
	// lightweight call).
	SandboxSecretFetchTimeout time.Duration

	// SandboxSecretFetchMaxAttempts/SandboxSecretFetchRetryBaseDelay/
	// SandboxSecretFetchRetryMaxDelay configure platform.Retry's own
	// doubling-capped-at-max backoff around fetchSandboxSecrets' own call
	// to CPClient.FetchSandboxSecrets -- mirrors
	// IdentityEmailFetchMaxAttempts/IdentityEmailFetchRetryBaseDelay/
	// IdentityEmailFetchRetryMaxDelay's own identical shape (a foreground,
	// in-process retry bounded by the caller's own budget), for the
	// adversarial-review MEDIUM fix described on SandboxSecretFetchTimeout
	// above. Only a transport error or a CP 5xx is retried -- a
	// 401/403/404/410 is this delivery endpoint's own terminal handshake
	// fence (mirrors providercredentialsdelivery.go's identical four-way
	// shape) and is never worth retrying (cmd/sandbox-agent's own
	// deliveryretry.go, classifyDeliveryFetchError).
	//
	// Chosen as 3 attempts / 500ms base / 2s max: unlike
	// IdentityEmailFetchMaxAttempts (tightened to 2 specifically to fit
	// Slack's own ~3s webhook-ack budget, a real external constraint this
	// call has no equivalent of), sandbox boot's own real budget is
	// FirstConnectBudget (240s, §3.2 -- the control plane's own liveness
	// deadline for a sandbox's first WS connection, which every boot-time
	// activity, including this fetch, happens inside). 3 genuine attempts
	// -- two retries, not merely one -- rides out a longer transient CP
	// disruption than 2 would, while the real worst case (3 attempts at
	// the full 10s SandboxSecretFetchTimeout each, plus two backoff waits
	// pessimistically at the 2s cap) is 3x10s+2x2s = 34s -- comfortably
	// inside FirstConnectBudget even stacked with OpenCodeConfigFetch*'s
	// own identical worst case (a further 34s, since the two fetches run
	// sequentially, sandboxsecrets before opencodeconfig, in run()) and
	// every OTHER boot-time activity (git clone, hooks) sharing that same
	// 240s ceiling. 500ms/2s (unlike IdentityEmailFetch's own tuned-tight
	// 100ms/150ms) is deliberately more generous -- there is no tight
	// external ack window forcing this shorter, and a CP rolling restart
	// realistically takes low-single-digit seconds to recover from, not
	// hundreds of milliseconds.
	SandboxSecretFetchMaxAttempts    int
	SandboxSecretFetchRetryBaseDelay time.Duration
	SandboxSecretFetchRetryMaxDelay  time.Duration

	// OpenCodeConfigFetchTimeout (§27.2) bounds a SINGLE ATTEMPT
	// at CP's /sessions/{id}/opencode-config delivery endpoint
	// (internal/sandboxagent/credentials.CPClient.FetchOpenCodeConfig),
	// tried up to OpenCodeConfigFetchMaxAttempts times (below) at boot,
	// alongside SandboxSecretFetchTimeout's own call, before `opencode
	// serve` spawns. Not specified in the plan; chosen the SAME 10s --
	// this call returns at most 2 small JSON documents (global + this
	// session's own environment config), comparably lightweight to the
	// other 2 delivery-endpoint fetches above.
	//
	// Adversarial-review MEDIUM fix: this field's own MEANING changed
	// identically to SandboxSecretFetchTimeout's own -- see that field's
	// doc comment for the full "why" (applies here per §27.2's own
	// "delivered at boot... same handshake" framing, so the identical
	// gap and the identical fix apply to both OpenCode config documents,
	// not just general sandbox secrets).
	OpenCodeConfigFetchTimeout time.Duration

	// OpenCodeConfigFetchMaxAttempts/OpenCodeConfigFetchRetryBaseDelay/
	// OpenCodeConfigFetchRetryMaxDelay mirror
	// SandboxSecretFetchMaxAttempts/SandboxSecretFetchRetryBaseDelay/
	// SandboxSecretFetchRetryMaxDelay's own identical shape and identical
	// values, for fetchOpenCodeConfig's own call to
	// CPClient.FetchOpenCodeConfig -- see those fields' own doc comment
	// for the full worst-case-budget arithmetic (which already accounts
	// for BOTH fetches' own worst cases, run sequentially, summing to
	// well under FirstConnectBudget).
	OpenCodeConfigFetchMaxAttempts    int
	OpenCodeConfigFetchRetryBaseDelay time.Duration
	OpenCodeConfigFetchRetryMaxDelay  time.Duration

	// CloudIdentityTokenLifetime (§27.3) is how long a CP-minted
	// cloud-identity OIDC token (POST /sessions/{id}/cloud-identity-token)
	// remains valid before its own `exp` claim expires. §27.3 gives this
	// explicitly: "exp ≈ 10 minutes" -- taken literally as 10 * time.Minute,
	// the plan's own stated approximation, not tightened or loosened
	// further without a stated reason.
	CloudIdentityTokenLifetime time.Duration

	// CloudIdentitySigningKeyOverlapWindow (§27.3) is how long a
	// just-retired oidc_signing_keys row keeps publishing in the JWKS
	// response after RotateSigningKeys marks its own retired_at, before it
	// drops out entirely -- §27.3's own rotation rule: "an overlap window
	// >= max token lifetime -- the same overlapping-validity discipline
	// §5.2 already applies to sandbox-token rotation" (that section's own
	// "previous-gen grace window during overlapping spawns"). A token
	// signed with the old key just before rotation must still verify for
	// the rest of its own natural life, so this must stay strictly above
	// CloudIdentityTokenLifetime (checked below, this chain's own single
	// link) -- not merely "greater or equal", the same explicit-margin
	// discipline §5.4 requires of every other pairwise link in this file.
	// Not specified numerically beyond that floor; chosen as 15min --
	// comfortably above the 10min token lifetime with margin to spare
	// (§27.8's own "clock skew between CP and cloud STS endpoints bounds
	// how short exp can safely go" concern cuts the other way, toward a
	// longer overlap being safer, never shorter).
	CloudIdentitySigningKeyOverlapWindow time.Duration

	// CloudIdentityConfigFetchTimeout ("cloud identity: sandbox-
	// side consumption + kubeconfig injection", §27.3/§27.4) bounds a
	// SINGLE ATTEMPT at CP's /sessions/{id}/cloud-identity-config delivery
	// endpoint (internal/sandboxagent/credentials.CPClient.
	// FetchCloudIdentityConfig), tried up to
	// CloudIdentityConfigFetchMaxAttempts times (below) at boot, alongside
	// SandboxSecretFetchTimeout/OpenCodeConfigFetchTimeout's own calls,
	// before the first per-binding mint attempt. Not specified in the
	// plan; chosen the SAME 10s as every other boot-time delivery-endpoint
	// fetch in this file -- this call resolves at most a handful of
	// identifier-only rows server-side (bindings grouped by kind, plus one
	// cluster row), comparably lightweight.
	CloudIdentityConfigFetchTimeout time.Duration

	// CloudIdentityConfigFetchMaxAttempts/CloudIdentityConfigFetchRetryBaseDelay/
	// CloudIdentityConfigFetchRetryMaxDelay mirror
	// SandboxSecretFetchMaxAttempts/SandboxSecretFetchRetryBaseDelay/
	// SandboxSecretFetchRetryMaxDelay's own identical shape and identical
	// values (3 attempts / 500ms base / 2s max), for
	// cmd/sandbox-agent/cloudidentity.go's own fetchCloudIdentityConfig
	// call to CPClient.FetchCloudIdentityConfig -- see those fields' own
	// doc comment for the full worst-case-budget arithmetic (which already
	// accounts for every sequential boot-time fetch summing to well under
	// FirstConnectBudget; this fetch's own worst case, 3*10s+2*2s=34s,
	// stacks onto that SAME running total, still comfortably inside the
	// 240s ceiling alongside every other boot-time activity).
	CloudIdentityConfigFetchMaxAttempts    int
	CloudIdentityConfigFetchRetryBaseDelay time.Duration
	CloudIdentityConfigFetchRetryMaxDelay  time.Duration

	// CloudIdentityTokenMintTimeout (§27.3) bounds a SINGLE
	// ATTEMPT at CP's /sessions/{id}/cloud-identity-token minting endpoint
	// (internal/sandboxagent/credentials.CPClient.MintCloudIdentityToken),
	// tried up to CloudIdentityTokenMintMaxAttempts times (below) --
	// called once per resolved binding at boot (cmd/sandbox-agent/
	// cloudidentity.go's own populateCloudIdentityTokenFiles), again on
	// every half-life refresh (the SAME function's own background loop,
	// woken every CloudIdentityTokenLifetime/2 -- a computed interval, not
	// a second stored duration, so it can never drift out of sync with
	// the lifetime it is literally half of), and once per kube-credential
	// subcommand invocation (main.go's runKubeCredentialHelper, for the
	// AuthKindOIDC cluster rung). Not specified in the plan; chosen the
	// SAME 10s as CloudIdentityConfigFetchTimeout immediately above -- an
	// RS256 sign is a fast, CPU-only operation server-side (no external
	// STS round trip; §27.3 is explicit that the cloud's own STS exchange
	// happens IN-SANDBOX, never CP-side), so this is, if anything, a
	// generous bound for a lightweight mint call.
	CloudIdentityTokenMintTimeout time.Duration

	// CloudIdentityTokenMintMaxAttempts/CloudIdentityTokenMintRetryBaseDelay/
	// CloudIdentityTokenMintRetryMaxDelay mirror
	// CloudIdentityConfigFetchMaxAttempts/CloudIdentityConfigFetchRetryBaseDelay/
	// CloudIdentityConfigFetchRetryMaxDelay's own identical shape and
	// identical values -- classifyMintTokenError (cmd/sandbox-agent/
	// deliveryretry.go) is this call's own retry-classification rule
	// (401/403/404/410/503 terminal, 5xx-other-than-503 and transport
	// errors retryable -- see that function's own doc comment for the
	// full "why 503 differs from every other delivery endpoint's own
	// retryable-5xx default" reasoning, this Step's own explicit gap
	// resolution). A background refresh call's own worst case
	// (3*10s+2*2s=34s) is bounded well below the token's own remaining
	// validity window at the moment a half-life refresh fires (half of
	// CloudIdentityTokenLifetime, i.e. 5 minutes at the shipped default --
	// see cmd/sandbox-agent/cloudidentity.go's own doc comment for the
	// full "why there is a real window to retry in" reasoning, this
	// Step's own first spec-gap resolution).
	CloudIdentityTokenMintMaxAttempts    int
	CloudIdentityTokenMintRetryBaseDelay time.Duration
	CloudIdentityTokenMintRetryMaxDelay  time.Duration

	// --- §27.5 standalone addition ("sandbox substrate: docker, egress
	// policy, toolchain", §27.5): no ordering relationship with either
	// invariant chain above (or with any prior Step's standalone
	// additions), so -- per those additions' own precedent -- a plain
	// field with a sensible default, not wired into a fake invariant
	// link.

	// DockerReadinessTimeout bounds how long internal/sandboxagent/boot.
	// RunDocker waits for dockerd to create its own Unix socket
	// (boot.DefaultDockerSocketPath) before giving up -- called ONLY
	// when a session's own SessionConfig.Docker is true (cmd/sandbox-
	// agent/main.go's own runBootSequence). Not specified in the plan;
	// chosen more generously than ServiceReadinessTimeout's own 30s (a
	// typical dev-server/mock-server cold start): dockerd's own startup
	// (initializing its overlay2 graph driver, bridge networking) is a
	// heavier, real-kernel operation under §27.5's own VM runtime option,
	// not a plain process binding a port -- 60s leaves comfortable
	// headroom without letting a genuinely stuck daemon stall the whole
	// boot sequence indefinitely. Polled at ServiceReadinessPollInterval
	// (250ms) -- the identical poll cadence services.Run already uses;
	// no second poll-interval field needed for what is structurally the
	// same "poll until ready or timeout" shape.
	DockerReadinessTimeout time.Duration

	// --- §10 standalone addition ("config/data seeding", §10-P6,
	// §13.4): no ordering relationship with either invariant chain above,
	// so -- per every prior Step's own standalone-addition precedent -- a
	// plain field with a sensible default, not wired into a fake
	// invariant link.

	// SeedRunTimeout bounds cmd/control-plane's own "seed" subcommand's
	// ENTIRE run (context.WithTimeout wrapping every item internal/app/
	// seed.Run processes, not a per-item bound) -- not specified in the
	// plan; chosen generously: a seed manifest processes a small,
	// operator-authored, human-scale list of rows (participants,
	// secrets, automations, repo settings), each one a handful of plain
	// INSERT/UPDATE statements, but the whole run is still a single CLI
	// invocation with no partial-progress resumption of its own (each
	// item IS individually idempotent -- see internal/app/seed/doc.go --
	// so a timed-out run is always safe to simply re-run) -- 5 minutes
	// leaves comfortable headroom for a large manifest against a
	// slow/contended database without letting a genuinely stuck
	// connection hang the operator's terminal indefinitely.
	SeedRunTimeout time.Duration

	// --- §30.4 standalone additions ("sandbox capability: GitHub App
	// read-only installation tokens"): no ordering relationship with
	// either invariant chain above, so -- per every prior Step's own
	// standalone-addition precedent -- plain fields with sensible
	// defaults, not wired into a fake invariant link.

	// GitHubAppJWTTTL bounds the "exp" claim of the short-lived JWT
	// internal/adapters/outbound/githubapp signs with the App's own
	// private key to authenticate AS THE APP (never as an installation)
	// for the two calls that need that identity: resolving a repo's
	// installation id, and minting that installation's own access token.
	// GitHub's own API rejects an App JWT whose "exp" is more than 10
	// minutes past "iat" -- this is a hard external ceiling, not a
	// judgment call, so this value must never be raised above it. Chosen
	// as 9 minutes: comfortable headroom under the 10-minute ceiling for
	// ordinary clock skew between this process and GitHub's own clock,
	// mirroring CloudIdentityTokenLifetime's own "chosen with margin
	// under an externally-imposed bound" reasoning.
	GitHubAppJWTTTL time.Duration

	// GitHubAppJWTClockSkew backdates the App JWT's "iat" claim so a small
	// clock difference between this process and GitHub cannot make a
	// freshly-signed token look future-dated.
	//
	// Not a per-deployment budget like its neighbours -- it is a fixed
	// allowance for skew, and GitHub's own tolerance is what makes any
	// particular value right. It lives here anyway, because the rule is
	// about where duration literals live and not about whether they are
	// tunable: a literal elsewhere is one the next reader has to go find,
	// whatever the reason it was put there.
	GitHubAppJWTClockSkew time.Duration

	// GitHubAppScopeCheckTimeout bounds the one-shot, boot-time GET /app
	// call cmd/control-plane's own startup sequence makes to introspect
	// the configured GitHub App's own granted permissions (§30.4's
	// "scope introspection, fail-closed, at boot") before this process
	// ever starts serving traffic. Not specified in the plan; chosen as
	// 10s, matching RepoSHAResolutionTimeout/CredentialFetchTimeout's own
	// "a single, lightweight GitHub REST GET" reasoning.
	GitHubAppScopeCheckTimeout time.Duration

	// GitHubAppMintTimeout bounds the two sequential GitHub REST calls
	// internal/adapters/outbound/githubapp.Client.MintInstallationToken
	// makes per credential mint (GET the repo's installation id, then
	// POST that installation's own access token, scoped contents:read +
	// metadata:read) -- internal/adapters/inbound/httpapi.ScmCredentials'
	// own shadow-substitution branch calls this synchronously inside an
	// HTTP handler, so it must stay bounded. Not specified in the plan;
	// chosen as 20s -- double GitHubGetPRTimeout's own 10s single-GET
	// baseline, since this is two round trips rather than one, still
	// comfortably inside ScmCredentialTTL (15min) so a slow mint cannot
	// itself eat the credential's own advertised lifetime.
	GitHubAppMintTimeout time.Duration

	// LicenseNotBeforeSkew is internal/app/capability.Registry's own
	// nbfSkew (docs/design/boundaries-design.md, section 1.5): how far a host
	// clock is tolerated to run BEHIND the licence issuer's before a
	// grant's own "nbf" claim is treated as not-yet-valid. Widens nbf
	// only, in the direction that makes a grant activate slightly early
	// rather than slightly late -- it never widens a grant's own "exp",
	// which stays exact (a host clock running AHEAD expires a key early;
	// that is the safe direction, so nothing compensates for it). Not
	// given an explicit figure by any technical-plan section -- chosen as
	// 5 minutes, the same figure this file's own HMACWindow uses for its
	// own (unrelated) freshness window (§5.2): a deliberately round,
	// generous-enough default for the kind of drift a real self-hosted
	// deployment's own clock can have, without being so wide that a
	// genuinely stale key reads as current for long. A DISTINCT field
	// from HMACWindow despite the shared value, exactly like
	// GitHubAppJWTClockSkew's own neighbouring "distinct field,
	// coincidentally equal default" precedent above: this guards a
	// licence grant's own activation instant, never an HMAC bearer
	// token's freshness window.
	LicenseNotBeforeSkew time.Duration

	// KnowledgeRankerTimeout bounds a single call to a composed module's
	// own ports.KnowledgeRanker.Score (docs/design/boundaries-design.md,
	// section 2.2), layered onto the caller's own context by
	// controlplane's capabilitySwitchRanker before delegating to it --
	// never applied to the public knowledge.RecencyRanker, which is
	// first-party, synchronous, zero-I/O code this codebase already
	// trusts not to block. A composed module's own ranker is arbitrary
	// third-party code this switch cannot assume respects ctx's own
	// deadline unprompted, so the deadline is set explicitly rather than
	// left to the implementation to impose on itself (the KnowledgeRanker
	// port's own doc comment: "an implementation should still honor ctx's
	// own deadline ... it must never assume it will always be given
	// unlimited time").
	//
	// What this bound is, precisely, and what it is not. A Go deadline is
	// cooperative: context.WithTimeout signals, it does not interrupt. A
	// ranker that selects on ctx.Done returns at the deadline; a ranker
	// that ignores ctx entirely runs to completion and Score returns only
	// when it does -- measured at 2s against a 20ms bound while writing
	// this. So the guarantee here is "a well-behaved module ranker is
	// bounded", never "no module ranker can stall a review turn". Making
	// the stronger claim true would mean abandoning the call in a
	// goroutine and returning early, which converts a stall into a leak:
	// the goroutine still runs, still holds the caller's candidates, and
	// nothing joins it. A stall is observable and attributable; a leak is
	// neither, so the weaker guarantee is the deliberate choice and this
	// comment states it rather than letting the field name imply the
	// stronger one.
	//
	// Not given an explicit figure by any technical-plan section --
	// chosen as 10 seconds, matching GitHubAppScopeCheckTimeout/
	// OpenCodeConfigFetchTimeout/CloudIdentityConfigFetchTimeout's own
	// shared "a single, bounded external call" reasoning: generous enough
	// for a real hybrid lexical+embeddings retrieval call, short enough
	// that a cooperative ranker degrades to the gate's own order promptly.
	KnowledgeRankerTimeout time.Duration
}

// DefaultTimeouts returns the shipped defaults for every field, each
// justified above on the struct field and (briefly) inline here.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		ProviderHardCap:           2 * time.Hour,     // §5.4, explicit
		SupervisorTurnCap:         90 * time.Minute,  // not specified; chosen with margin below ProviderHardCap
		TurnDeadline:              60 * time.Minute,  // not specified; chosen with margin below SupervisorTurnCap
		SSEInactivityTimeout:      120 * time.Second, // §7, explicit
		ProviderHTTPClientTimeout: 5 * time.Minute,   // not specified; must clear ProviderWorstColdStart (§4.1) with margin
		ProviderWorstColdStart:    220 * time.Second, // §4.1, "220s+" floor
		FirstConnectBudget:        240 * time.Second, // §3.2, explicit
		ImagePullBootP99:          90 * time.Second,  // not specified; chosen with margin below FirstConnectBudget

		HMACWindow:          5 * time.Minute,  // §5.2, explicit
		ShutdownGracePeriod: 10 * time.Second, // not specified; invented
		HealthCheckTimeout:  2 * time.Second,  // not specified; invented

		SteadyHeartbeatBudget:      90 * time.Second,  // §3.2, explicit
		TerminalGracePeriod:        60 * time.Second,  // §3.2, explicit
		CircuitBreakerWindow:       5 * time.Minute,   // §3.2, explicit
		SpawnCooldown:              30 * time.Second,  // not specified; chosen
		SpawnReadyWait:             60 * time.Second,  // not specified; chosen
		SpawnStuckTimeout:          120 * time.Second, // not specified; chosen
		InactivityTimeout:          10 * time.Minute,  // not specified; chosen
		InactivityExtension:        5 * time.Minute,   // not specified; chosen
		InactivityMinCheckInterval: 30 * time.Second,  // not specified; chosen

		ActorIdleTTL:       30 * time.Minute, // §2, explicit
		TimerPumpInterval:  5 * time.Second,  // not specified; chosen
		TimerClaimDuration: 30 * time.Second, // not specified; chosen

		HookTimeout:               10 * time.Minute, // not specified; chosen generously (setup.sh may install deps)
		ProcessStopGracePeriod:    10 * time.Second, // not specified; chosen
		SupervisorShutdownTimeout: 30 * time.Second, // not specified; chosen
		RepoSHADiscoveryTimeout:   5 * time.Second,  // not specified; chosen

		ServiceReadinessTimeout:      30 * time.Second,       // not specified; chosen
		ServiceReadinessPollInterval: 250 * time.Millisecond, // not specified; chosen

		RepoCloneTimeout:       5 * time.Minute,  // not specified; chosen generously (large repos)
		CredentialFetchTimeout: 10 * time.Second, // not specified; chosen (lightweight mint call)
		CredentialExpiryBuffer: 5 * time.Minute,  // §5.2, explicit

		ProviderCredentialFetchTimeout: 10 * time.Second, // not specified; chosen, matching CredentialFetchTimeout's own reasoning

		SandboxWSHeartbeatInterval:   30 * time.Second, // §6.1, explicit
		SandboxWSDialTimeout:         15 * time.Second, // not specified; chosen
		SandboxWSReconnectMinBackoff: 1 * time.Second,  // not specified; chosen
		SandboxWSReconnectMaxBackoff: 30 * time.Second, // not specified; chosen

		OpenCodeReadinessTimeout:      30 * time.Second,       // not specified; chosen (OpenCode startup may init providers/plugins)
		OpenCodeReadinessPollInterval: 250 * time.Millisecond, // not specified; chosen, matches ServiceReadinessPollInterval

		SandboxEventAckTimeout: 5 * time.Second, // not specified; chosen

		ClientSubscribeTimeout: 30 * time.Second, // §6.2, explicit
		WSTokenTTL:             24 * time.Hour,   // §6.2, explicit

		OAuthStateTTL:  10 * time.Minute,    // not specified; chosen
		UserSessionTTL: 30 * 24 * time.Hour, // not specified; chosen ("stay signed in" duration)

		SandboxCommandSendTimeout: 10 * time.Second, // not specified; chosen
		ScmCredentialTTL:          15 * time.Minute, // not specified; chosen (comfortably exceeds a single push + the 5-min sandbox-side cache buffer)
		PRCreateTimeout:           30 * time.Second, // not specified; chosen (generous for a single GitHub REST API POST)

		OpenCodeSSEReconnectInterval: 2 * time.Second,  // not specified; chosen, deliberately short relative to SSEInactivityTimeout
		OpenCodeRequestTimeout:       30 * time.Second, // not specified; chosen, bounds every doJSON call except the persistent SSE connection

		OpenCodeSummarizeTimeout: 120 * time.Second, // not specified; chosen generously (§7.2, a single non-streaming summarization call)

		OpenCodeTransientRetryBackoff: 2 * time.Second, // not specified; chosen, short pause before retrying a transient (isRetryable) provider blip

		SetupRerunRetryBackoff: 2 * time.Second, // not specified; chosen, short pause before the ONE setup.sh rerun retry (§19.6) -- same value as the OpenCode pause today, deliberately a separate field (see its doc comment)

		ClientWSPingInterval:          30 * time.Second,       // not specified; chosen, matches SandboxWSHeartbeatInterval's own 30s cadence (§6.1)
		ClientFetchHistoryMinInterval: 250 * time.Millisecond, // not specified; chosen, generous for real pagination while blocking a tight-loop hammer

		ExpiredCredentialCleanupInterval: time.Hour, // not specified; chosen, comfortably frequent relative to both WSTokenTTL (24h) and UserSessionTTL (30 days)

		SnapshotMintTimeout: 60 * time.Second, // not specified; chosen (a real provider TakeSnapshot round trip, more generous than CredentialFetchTimeout)

		ReconcilerInterval: 60 * time.Second, // §5.3, explicit ("60s loop")

		ReconcilerOrphanConfirmationPeriod: 30 * time.Second, // not specified; chosen, comfortably above the realistic sub-second spawn-commit race window; exactly MinTimeoutMargin below ReconcilerInterval (the minimum Validate allows, zero slack beyond it) so the two-tick guarantee holds under the shipped default

		RepoSHAResolutionTimeout: 10 * time.Second, // not specified; chosen, matches CredentialFetchTimeout's own "lightweight call" reasoning

		ImageBuildPumpInterval: 60 * time.Second, // not specified; chosen, matches ReconcilerInterval's own cadence
		ImageBuildBackoffBase:  1 * time.Minute,  // not specified; chosen -- see EvaluateBackoff's own doc comment for the schedule this produces
		ImageBuildBackoffMax:   30 * time.Minute, // not specified; chosen -- the eventual steady-state ceiling, never the first-failure delay (§3.5)

		ContractsFingerprintResolutionTimeout: 10 * time.Second, // not specified; chosen, matches RepoSHAResolutionTimeout's own "lightweight call" reasoning

		GitSyncStepTimeout: 30 * time.Second, // not specified; chosen, generous for a local-only stash/checkout/pop step without stalling boot

		WebhookTimestampFreshnessWindow: 5 * time.Minute, // not specified; chosen, matches Slack's own commonly recommended replay window

		LinearWebhookTimestampWindow:  60 * time.Second, // Linear's own docs, explicit ("within a minute")
		LinearOutboundActivityTimeout: 3 * time.Second,  // not specified; chosen, comfortably below Linear's own 5s webhook-response requirement

		LinearSetSessionIDTimeout:        2 * time.Second,        // not specified; chosen, generous for a single-row Postgres UPDATE
		LinearSetSessionIDMaxAttempts:    3,                      // not specified; chosen -- originally matched IdentityEmailFetchMaxAttempts, which the HIGH audit fix (see that field's own doc comment) later lowered to 2; kept at 3 here since setSessionIDWithRetry's own retry is over a cheap, always-safe-to-retry LOCAL Postgres UPDATE (see LinearSetSessionIDTimeout's own doc comment), never the retried-outbound-call budget that fix was retuning
		LinearSetSessionIDRetryBaseDelay: 100 * time.Millisecond, // not specified; chosen, keeps the synchronous ingress path fast
		LinearSetSessionIDRetryMaxDelay:  500 * time.Millisecond, // not specified; chosen

		SlackAckTimeout: 10 * time.Second, // not specified; chosen, generous for a single Slack chat.postMessage POST, mirrors PRCreateTimeout's own reasoning

		SlackInteractivityAckTimeout: 2500 * time.Millisecond, // not specified; chosen, a SEPARATE and much tighter budget than SlackAckTimeout -- see field doc comment for why (Slack's real interactivity ack window is a hard ~3s, covering the whole decide+update sequence, not just SlackAckTimeout's single POST)

		OutboxPumpInterval:    5 * time.Second,  // not specified; chosen, near-real-time delivery, matches TimerPumpInterval's own reasoning
		OutboxBackoffBase:     30 * time.Second, // not specified; chosen -- see domain/outbox.EvaluateBackoff's own doc comment for the schedule this produces
		OutboxBackoffMax:      5 * time.Minute,  // not specified; chosen, shorter than ImageBuildBackoffMax since a live notification is being waited on
		OutboxDeliveryTimeout: 15 * time.Second, // not specified; chosen, generous for a single outbound notifier POST
		OutboxClaimDuration:   45 * time.Second, // not specified; chosen -- exactly MinTimeoutMargin above OutboxDeliveryTimeout (audit fix H6, see field doc comment)

		IntentClassifierLLMTimeout: 10 * time.Second, // not specified; chosen, matches RepoSHAResolutionTimeout's own "lightweight call" reasoning

		IdentityEmailFetchTimeout:              800 * time.Millisecond, // audit fix HIGH -- was 300ms (itself audit fix L5's shrink from 10s); see field doc comment for why 300ms was unrealistically tight and why 800ms (reusing SlackInteractivityIdentityFetchTimeout's own precedent) is the realistic figure
		IdentityEmailFetchMaxAttempts:          2,                      // audit fix HIGH -- was 3; see field doc comment for why the attempt count, not the per-attempt timeout, absorbed the budget cut this time
		IdentityEmailFetchRetryBaseDelay:       100 * time.Millisecond, // audit fix L5 -- was 200ms; see IdentityEmailFetchRetryMaxDelay's own doc comment
		IdentityEmailFetchRetryMaxDelay:        150 * time.Millisecond, // audit fix L5 -- was 1s; see field doc comment for the full worst-case timing budget
		SlackInteractivityIdentityFetchTimeout: 800 * time.Millisecond, // not specified; chosen, comfortably inside SlackInteractivityAckTimeout with margin for DecidePlan+chat.update
		IdentityLinkPromptTTL:                  24 * time.Hour,         // not specified beyond "short-lived"; chosen

		GitHubGetPRTimeout: 10 * time.Second, // not specified (fix postdates the plan); chosen, generous for a single GitHub REST GET, mirrors PRCreateTimeout/SlackAckTimeout's own reasoning

		GitFetchStepTimeout: 90 * time.Second, // §19.3, explicit ("propose 90s, distinct from the existing local-only 30s GitSyncStepTimeout")

		ImageRefreshCheckInterval: 10 * time.Minute, // §19.2, explicit ("propose 10 min")

		RepoAccessCheckTimeout:       10 * time.Second, // not specified; chosen, matches RepoSHAResolutionTimeout/ContractsFingerprintResolutionTimeout's own "lightweight call" reasoning
		RepoAccessCacheTTL:           10 * time.Minute, // not specified; chosen, matches ImageRefreshCheckInterval's own "acceptable staleness" reasoning
		RepoAccessCheckBreakerWindow: 2 * time.Minute,  // not specified (fix postdates the plan); chosen, short enough to retry a resolved outage promptly
		ImageRefreshClaimStaleAfter:  30 * time.Minute, // audit-remediation batch B2; not specified, chosen -- matches ImageBuildBackoffMax's own exact value/reasoning (see field doc comment)

		OTelShutdownTimeout: 10 * time.Second, // audit-remediation batch B7; not specified, chosen -- matches ShutdownGracePeriod/ProcessStopGracePeriod's own precedent

		GitHubActorNoticeTTL: 24 * time.Hour, // batch fix/deny-unlinked-github-actors; not specified, chosen -- see field doc comment for why this is a DISTINCT constant from IdentityLinkPromptTTL despite sharing its value

		GitHubPRDiffTimeout: 20 * time.Second, // not specified (this value postdates the plan); chosen, double GitHubGetPRTimeout's own 10s -- generous for a real, possibly-large diff transfer

		GitHubListMergedBetweenTimeout: 2 * time.Minute, // not specified (this value postdates the plan); chosen, generous for the whole bounded call sequence a real release cut makes -- see field doc comment

		ReleaseManifestCheckPumpInterval: 10 * time.Second, // blocking-finding fix #1; not specified, chosen -- see field doc comment
		ReleaseManifestCheckTimeout:      3 * time.Minute,  // blocking-finding fix #1; not specified, chosen -- GitHubListMergedBetweenTimeout (2min) plus a full extra minute of margin, see field doc comment

		AutomationEnginePumpInterval:         60 * time.Second, // §3.5; not specified, chosen, matches ReconcilerInterval's own cadence
		AutomationSweepInterval:              60 * time.Second, // §3.5; not specified, chosen, matches AutomationEnginePumpInterval's own cadence
		AutomationRunStartingOrphanThreshold: 5 * time.Minute,  // §3.5, explicit ("orphaned starting runs >5 min")
		AutomationRunRunningOrphanThreshold:  90 * time.Minute, // §3.5, explicit ("running >90 min")
		AutomationCronGranularity:            1 * time.Minute,  // §8.4; structural, not tunable -- see field doc comment
		AutomationCronCatchUpWindow:          10 * time.Minute, // §8.4 fix (missed cron evaluations); not specified, chosen -- see field doc comment

		RWXCLIExecTimeout:           2 * time.Minute,  // §4.1.1; not specified (RWX publishes no p99), chosen generously -- see field doc comment
		RWXSandboxInactivityTimeout: 45 * time.Minute, // §4.1.1; not specified, chosen with margin above ActorIdleTTL (30min) -- see field doc comment

		UploadPresignPutTTL:            15 * time.Minute, // §28.4, explicit ("propose 15 min")
		UploadPresignGetTTL:            5 * time.Minute,  // §28.5, explicit ("propose 5 min")
		UploadPendingSweepAfter:        24 * time.Hour,   // §28.4, explicit ("propose 24 h")
		UploadAbandonmentSweepInterval: 15 * time.Minute, // §28.4, explicit ("propose 15 min")
		ObjectStoreHTTPClientTimeout:   10 * time.Second, // §8.6; not specified, chosen, matches RepoSHAResolutionTimeout's own "lightweight call" reasoning

		ChatGPTOAuthRefreshMargin:       72 * time.Hour,   // §29.5, explicit ("propose 72h")
		ChatGPTOAuthRefreshPumpInterval: 6 * time.Hour,    // §29.5, explicit ("propose 6h")
		ChatGPTLinkAttemptTTL:           15 * time.Minute, // §8.8; not specified, chosen generously (human device-switch time)
		ChatGPTOAuthHTTPClientTimeout:   15 * time.Second, // §8.8; not specified, chosen generously (a real third-party OAuth endpoint over the public internet)

		GitHubListOpenPRsForUserTimeout: 3 * time.Minute,     // §16; not specified, matches ReleaseManifestCheckTimeout's own figure for a comparable bounded-but-many-call operation
		GitHubResolveCodeOwnersTimeout:  30 * time.Second,    // §16; not specified, chosen generously (a handful of file/user/team fetches)
		GitHubMergePRTimeout:            15 * time.Second,    // §16; not specified, half again GitHubGetPRTimeout's baseline (interactive, human-facing write)
		DecisionInboxSCMCacheTTL:        2 * time.Minute,     // §16.2's own worked example ("as of 2 min ago")
		DecisionInboxStaleAfter:         48 * time.Hour,      // §16.1, explicit ("stale items (>48h, configurable)")
		DecisionInboxLatencyWindow:      30 * 24 * time.Hour, // §16.2; not specified, chosen as a month of decision history -- long enough for a stable median, bounded per §21.1

		ReviewVerdictAnalyticsWindow:   30 * 24 * time.Hour, // §21.1, explicit ("bounded from day one ... default 30 days, mirroring the decision inbox's own DecisionInboxLatencyWindow, §16.2 -- never DecisionInboxStaleAfter's own much narrower 48h item-staleness flag, §16.1, a different concept entirely") -- mirrors DecisionInboxLatencyWindow's own identical "a month, bounded" reasoning
		AutoMergePumpInterval:          60 * time.Second,    // §21.2; not specified, mirrors AutomationEnginePumpInterval's own identical periodic-background-policy-engine reasoning
		AutoMergeCandidateLookback:     7 * 24 * time.Hour,  // §21.2; not specified, chosen generously -- every candidate is re-confirmed live regardless
		DigestPumpInterval:             5 * time.Minute,     // §21.3; not specified, chosen -- a digest fires at most once per channel per day, so coarse polling is ample
		DigestChannelDiscoveryLookback: 30 * 24 * time.Hour, // §21.3; not specified, mirrors ReviewVerdictAnalyticsWindow's own identical "a month, bounded" reasoning
		DigestContentWindow:            24 * time.Hour,      // §21.3, explicit ("a daily digest") -- one calendar day of rollup content, distinct from the channel-discovery lookback above

		FindingPositionResolveAllTimeout: 45 * time.Second, // §22 fix; not specified, chosen -- generous for several per-finding relocation calls (10s each) while bounding the worst case on a synchronous verdict-POST handler path

		ReviewRetriggerDebounce: 2 * time.Minute, // §24.2; not specified, chosen -- long enough to collapse a short burst of fixup-commit pushes into one quiet window, short enough that a single push still reviews promptly

		ReviewCostBudgetServerReadHeaderTimeout: 5 * time.Second, // §26.7/§26.9; not specified, chosen -- matches RepoSHADiscoveryTimeout/CredentialFetchTimeout's own "lightweight, purely local" precedent, see field doc comment

		SandboxSecretFetchTimeout:         10 * time.Second,       // §27.1; not specified, chosen, matches ProviderCredentialFetchTimeout's own reasoning
		SandboxSecretFetchMaxAttempts:     3,                      // adversarial-review MEDIUM fix (§27.1 "with bounded retry"); see field doc comment for the worst-case-budget arithmetic
		SandboxSecretFetchRetryBaseDelay:  500 * time.Millisecond, // adversarial-review MEDIUM fix; see field doc comment
		SandboxSecretFetchRetryMaxDelay:   2 * time.Second,        // adversarial-review MEDIUM fix; see field doc comment
		OpenCodeConfigFetchTimeout:        10 * time.Second,       // §27.2; not specified, chosen, matches ProviderCredentialFetchTimeout's own reasoning
		OpenCodeConfigFetchMaxAttempts:    3,                      // adversarial-review MEDIUM fix; mirrors SandboxSecretFetchMaxAttempts
		OpenCodeConfigFetchRetryBaseDelay: 500 * time.Millisecond, // adversarial-review MEDIUM fix; mirrors SandboxSecretFetchRetryBaseDelay
		OpenCodeConfigFetchRetryMaxDelay:  2 * time.Second,        // adversarial-review MEDIUM fix; mirrors SandboxSecretFetchRetryMaxDelay

		CloudIdentityTokenLifetime:           10 * time.Minute, // §27.3, explicit ("exp ≈ 10 minutes")
		CloudIdentitySigningKeyOverlapWindow: 15 * time.Minute, // §27.3; not specified numerically beyond ">= max token lifetime", chosen with margin -- see field doc comment

		CloudIdentityConfigFetchTimeout:        10 * time.Second,       // §27.3/§27.4; not specified, chosen, matches every other boot-time delivery fetch's own reasoning
		CloudIdentityConfigFetchMaxAttempts:    3,                      // mirrors SandboxSecretFetchMaxAttempts
		CloudIdentityConfigFetchRetryBaseDelay: 500 * time.Millisecond, // mirrors SandboxSecretFetchRetryBaseDelay
		CloudIdentityConfigFetchRetryMaxDelay:  2 * time.Second,        // mirrors SandboxSecretFetchRetryMaxDelay
		CloudIdentityTokenMintTimeout:          10 * time.Second,       // §27.3; not specified, chosen -- see field doc comment
		CloudIdentityTokenMintMaxAttempts:      3,                      // mirrors CloudIdentityConfigFetchMaxAttempts
		CloudIdentityTokenMintRetryBaseDelay:   500 * time.Millisecond, // mirrors CloudIdentityConfigFetchRetryBaseDelay
		CloudIdentityTokenMintRetryMaxDelay:    2 * time.Second,        // mirrors CloudIdentityConfigFetchRetryMaxDelay

		DockerReadinessTimeout: 60 * time.Second, // §27.5; not specified, chosen generously -- see field doc comment

		SeedRunTimeout: 5 * time.Minute, // §10-P6/§13.4; not specified, chosen generously -- see field doc comment

		GitHubAppJWTTTL:            9 * time.Minute,
		GitHubAppJWTClockSkew:      60 * time.Second, // §30.4; not specified, chosen with margin under GitHub's own hard 10-minute App-JWT ceiling -- see field doc comment
		GitHubAppScopeCheckTimeout: 10 * time.Second, // §30.4; not specified, chosen, matches RepoSHAResolutionTimeout's own "lightweight call" reasoning
		GitHubAppMintTimeout:       20 * time.Second, // §30.4; not specified, chosen -- see field doc comment

		LicenseNotBeforeSkew: 5 * time.Minute, // design note section 1.5, explicit ("default 5 minutes")

		KnowledgeRankerTimeout: 10 * time.Second, // design note section 2.2; not specified numerically, chosen -- see field doc comment
	}
}

// TimeoutInvariantError reports one broken pairwise link in the timeout
// hierarchy: LesserValue is not at least RequiredMargin below GreaterValue.
type TimeoutInvariantError struct {
	// Chain names the pairwise relationship, e.g.
	// "ProviderHardCap > SupervisorTurnCap".
	Chain string

	LesserField  string
	LesserValue  time.Duration
	GreaterField string
	GreaterValue time.Duration

	RequiredMargin time.Duration
}

func (e *TimeoutInvariantError) Error() string {
	gap := e.GreaterValue - e.LesserValue
	return fmt.Sprintf(
		"timeout invariant violated (%s): %s=%s and %s=%s leave only %s of margin, need at least %s",
		e.Chain, e.LesserField, e.LesserValue, e.GreaterField, e.GreaterValue,
		gap, e.RequiredMargin,
	)
}

// Validate checks every pairwise link in both invariant chains and returns
// ALL violations found (via errors.Join), not just the first, so a single
// broken link never masks another. Returns nil when every link holds with
// at least MinTimeoutMargin of headroom.
func (t Timeouts) Validate() error {
	var errs []error

	check := func(chain, greaterField string, greater time.Duration, lesserField string, lesser time.Duration) {
		if greater < lesser+MinTimeoutMargin {
			errs = append(errs, &TimeoutInvariantError{
				Chain:          chain,
				LesserField:    lesserField,
				LesserValue:    lesser,
				GreaterField:   greaterField,
				GreaterValue:   greater,
				RequiredMargin: MinTimeoutMargin,
			})
		}
	}

	// Chain A: provider cap > supervisor > bridge (turn_deadline) > SSE.
	check("ProviderHardCap > SupervisorTurnCap",
		"ProviderHardCap", t.ProviderHardCap, "SupervisorTurnCap", t.SupervisorTurnCap)
	check("SupervisorTurnCap > TurnDeadline",
		"SupervisorTurnCap", t.SupervisorTurnCap, "TurnDeadline", t.TurnDeadline)
	check("TurnDeadline > SSEInactivityTimeout",
		"TurnDeadline", t.TurnDeadline, "SSEInactivityTimeout", t.SSEInactivityTimeout)

	// Chain B: two independent pairs (§4.1, §3.2).
	check("ProviderHTTPClientTimeout > ProviderWorstColdStart",
		"ProviderHTTPClientTimeout", t.ProviderHTTPClientTimeout, "ProviderWorstColdStart", t.ProviderWorstColdStart)
	// Deliberately the weaker of two possible statements -- "the overall
	// budget exceeds the boot-sub-phase's own estimate" -- not "the overall
	// budget exceeds cold-start-plus-boot summed" (which would require
	// FirstConnectBudget > ProviderWorstColdStart+ImagePullBootP99 instead).
	// See FirstConnectBudget's and ImagePullBootP99's own doc comments
	// above for why this codebase does not currently model those two as an
	// additive sum.
	check("FirstConnectBudget > ImagePullBootP99",
		"FirstConnectBudget", t.FirstConnectBudget, "ImagePullBootP99", t.ImagePullBootP99)

	// §5.3 fix (reconciler orphan-GC debounce): ReconcilerOrphanConfirmationPeriod
	// must stay at least MinTimeoutMargin below ReconcilerInterval, or the
	// "reaped on the SECOND consecutive tick, never the first" guarantee
	// app/reconciler.Reconciler's own debounce promises silently degrades
	// to "third tick" (or later) instead -- see that field's own doc
	// comment for the full reasoning. The shipped defaults (60s/30s) sit
	// exactly at that minimum margin, not with extra slack beyond it.
	check("ReconcilerInterval > ReconcilerOrphanConfirmationPeriod",
		"ReconcilerInterval", t.ReconcilerInterval, "ReconcilerOrphanConfirmationPeriod", t.ReconcilerOrphanConfirmationPeriod)

	// Audit fix (H6, outbox claim-lease race): OutboxClaimDuration must
	// stay at least MinTimeoutMargin above OutboxDeliveryTimeout, or a
	// single real delivery attempt's own worst-case duration could outlive
	// the very claim-renewal window attempt() just refreshed to protect it
	// -- see OutboxClaimDuration's own doc comment for the full reasoning.
	check("OutboxClaimDuration > OutboxDeliveryTimeout",
		"OutboxClaimDuration", t.OutboxClaimDuration, "OutboxDeliveryTimeout", t.OutboxDeliveryTimeout)

	// Blocking-finding fix #1: ReleaseManifestCheckTimeout must stay at
	// least MinTimeoutMargin above GitHubListMergedBetweenTimeout, or
	// internal/app/releasereview.Worker's own outer context.WithTimeout(
	// ctx, ReleaseManifestCheckTimeout) call would silently cut short
	// Run's own INNER context.WithTimeout(ctx, GitHubListMergedBetweenTimeout)
	// call (context.WithTimeout always takes the EARLIER of its own
	// duration and whatever deadline the parent context already carries)
	// -- see ReleaseManifestCheckTimeout's own doc comment for the full
	// reasoning.
	check("ReleaseManifestCheckTimeout > GitHubListMergedBetweenTimeout",
		"ReleaseManifestCheckTimeout", t.ReleaseManifestCheckTimeout, "GitHubListMergedBetweenTimeout", t.GitHubListMergedBetweenTimeout)

	// §3.5 ("automations: engine", §3.5): AutomationSweepInterval must
	// stay at least MinTimeoutMargin below EACH of the two orphan
	// thresholds it polls for, or a run that just crosses one could sit
	// unswept for much longer than that threshold's own name implies --
	// mirrors ReconcilerInterval/ReconcilerOrphanConfirmationPeriod's own
	// identical pairwise reasoning, just stated as two links (one per
	// threshold) instead of one, since the two thresholds are
	// independently configurable.
	check("AutomationRunStartingOrphanThreshold > AutomationSweepInterval",
		"AutomationRunStartingOrphanThreshold", t.AutomationRunStartingOrphanThreshold, "AutomationSweepInterval", t.AutomationSweepInterval)
	check("AutomationRunRunningOrphanThreshold > AutomationSweepInterval",
		"AutomationRunRunningOrphanThreshold", t.AutomationRunRunningOrphanThreshold, "AutomationSweepInterval", t.AutomationSweepInterval)

	// §8.4 fix ("cron trigger pump has no catch-up for missed
	// evaluations"): AutomationCronCatchUpWindow must stay at least
	// MinTimeoutMargin above AutomationEnginePumpInterval (the trigger
	// pump's own tick cadence), or the catch-up window could be too
	// narrow to reliably span even ONE missed tick -- the exact gap this
	// field exists to absorb -- defeating its own purpose. Mirrors every
	// other pairwise link in this chain: a plain "wider than the interval
	// it is meant to cover, with margin" statement, nothing more elaborate.
	check("AutomationCronCatchUpWindow > AutomationEnginePumpInterval",
		"AutomationCronCatchUpWindow", t.AutomationCronCatchUpWindow, "AutomationEnginePumpInterval", t.AutomationEnginePumpInterval)

	// §4.1 ("RWX provider + previews", §4.1.1): RWXSandboxInactivityTimeout
	// must stay at least MinTimeoutMargin above ActorIdleTTL, or RWX's own
	// `--inactivity-timeout` auto-stop could fire BEFORE Narvi's own
	// session-idle authority ever gets a chance to decide idleness first —
	// exactly the inversion §4.1.1 requires this field to avoid ("set
	// above Narvi's own session-idle authority... so Narvi's timers, not
	// RWX's, decide idleness"). See RWXSandboxInactivityTimeout's own doc
	// comment for why ActorIdleTTL (the larger of the two named
	// authorities) is the one checked here.
	check("RWXSandboxInactivityTimeout > ActorIdleTTL",
		"RWXSandboxInactivityTimeout", t.RWXSandboxInactivityTimeout, "ActorIdleTTL", t.ActorIdleTTL)

	// §8.6 ("uploads, blob storage & the in-sandbox download_file
	// tool", §28.4): UploadPendingSweepAfter must stay at least
	// MinTimeoutMargin above UploadAbandonmentSweepInterval, or a pending
	// row could cross the abandonment threshold and still sit unswept for
	// much longer than that threshold's own name implies -- mirrors
	// AutomationRunStartingOrphanThreshold/AutomationSweepInterval's own
	// identical pairwise reasoning.
	check("UploadPendingSweepAfter > UploadAbandonmentSweepInterval",
		"UploadPendingSweepAfter", t.UploadPendingSweepAfter, "UploadAbandonmentSweepInterval", t.UploadAbandonmentSweepInterval)

	// (§27.3): CloudIdentitySigningKeyOverlapWindow must stay at
	// least MinTimeoutMargin above CloudIdentityTokenLifetime, or a token
	// minted right before rotation could outlive the grace window its own
	// signing key is still published for -- see
	// CloudIdentitySigningKeyOverlapWindow's own doc comment for the full
	// "why" (the same overlapping-validity discipline §5.2 already
	// requires of sandbox-token rotation).
	check("CloudIdentitySigningKeyOverlapWindow > CloudIdentityTokenLifetime",
		"CloudIdentitySigningKeyOverlapWindow", t.CloudIdentitySigningKeyOverlapWindow, "CloudIdentityTokenLifetime", t.CloudIdentityTokenLifetime)

	return errors.Join(errs...)
}

// SecondsToDuration converts a raw whole-seconds count -- e.g. an OAuth
// response's own interval/expires_in field (chatgptoauth), or a stored
// *_seconds database column (chatgpt_link_attempts.interval_seconds) --
// into a time.Duration. §8.8 ("models: Codex via ChatGPT-account OAuth")
// introduces the first callers that need to turn a plain wire/storage
// integer into a Duration outside this package; this helper exists so
// those callers never spell out time.Second themselves, which
// notimeliteral (§5.4/§11) forbids everywhere but here and _test.go files.
func SecondsToDuration[T ~int | ~int32 | ~int64](seconds T) time.Duration {
	return time.Duration(seconds) * time.Second
}

// DurationToSeconds is SecondsToDuration's own inverse, truncating toward
// zero -- e.g. for storing a time.Duration back into a database column
// typed as a raw integer seconds count.
func DurationToSeconds(d time.Duration) int32 {
	return int32(d / time.Second)
}

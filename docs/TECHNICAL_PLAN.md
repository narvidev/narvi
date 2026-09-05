# Narvi — Technical Implementation Plan

> **Audience**: AI coding agent (and human reviewer). This document is self-contained: it carries every architectural decision, invariant, and contract needed to implement the system without access to the original design discussions.

## 0. Context

Narvi runs autonomous coding agents in isolated cloud sandboxes, triggered from the web, Slack, Linear, and GitHub. This document is the self-contained specification for building it in Go. Its design is uncompromising on the properties that make an agent platform trustworthy at scale: one authoritative source of state, a single owner per session, native process supervision, and a resilience test suite (§9.3) that is a first-class exit criterion, not an afterthought.

**What we are building**: two Go services (control plane + in-sandbox agent), packaged as containers and deployable on any Kubernetes cluster or plain Docker/VMs — no cloud lock-in; Postgres as the single source of truth; S3-compatible object storage for media; and a web UI (built in phase 7 from the mockups in §12). The wire contracts in §6 are defined up front, so backend and UI are built against the same generated schemas.

**Non-goals for the initial build** (phases 0-6): the web UI (built in phase 7, §12); a replacement for OpenCode (the agent engine — Narvi wraps it); sandbox providers beyond Modal and RWX (the interface must allow adding them later — a Kubernetes-native sandbox provider is an anticipated adapter); multi-region.

## 1. Repository layout

Single Go module, hexagonal architecture. Domain has zero external dependencies.

```
/cmd
  control-plane/          # main: config, wiring, migrations, HTTP+WS server
  sandbox-agent/          # static binary shipped into sandbox images
/internal
  /domain                 # pure business logic; no I/O, no external imports
    session/              # session entity + state machine
    sandbox/              # sandbox state machine, spawn/restore/resume decisions,
                          #   circuit breaker, liveness budgets
    turn/                 # prompt lifecycle: pending→processing→terminal; queue
    gitstate/             # git state machine (stash → checkout → pop)
    automation/           # automation → invocation → runs (CAS failure accounting)
    review/               # code review: verdicts, risk map, sentinels, verdict floor
    plan/                 # plan mode: plan → HITL approval → dispatch
    intent/               # unified intent classifier (rules + LLM, shadow mode)
  /app                    # use cases; defines ports (Go interfaces)
    ports/                # all interfaces (see §4)
    sessionactor/         # one goroutine+mailbox per active session
    reconciler/           # provider reconciliation + orphan GC loop
    scheduler/            # automation cron, recovery sweeps
  /adapters
    /inbound
      httpapi/            # REST (serves the web UI + external clients)
      wshub/              # client WS + sandbox WS (contract in §6)
      github/ slack/ linear/ sentry/  # webhook ingress → normalized events
    /outbound
      postgres/           # stores, outbox, named timers
      modal/ rwx/         # sandbox providers (full interface, §4.1)
      opencode/           # anti-corruption layer around the agent engine (§7)
      githubapi/ gitlabapi/ slackapi/ linearapi/
      llm/                # Anthropic/OpenAI clients for classifier & review
      objstore/           # BlobStore adapter — S3-compatible (AWS S3, MinIO, R2, GCS, …)
  /platform               # config (typed, validated at boot), structured logging,
                          #   OTel, HMAC auth, timeout hierarchy (§5.4)
/contracts                # versioned JSON Schemas for: sandbox WS protocol,
                          #   client WS protocol, SESSION_CONFIG, REST DTOs.
                          #   Go types generated from these; TS types generated
                          #   for the frontend. Round-trip contract tests.
/migrations               # Postgres migrations (goose or golang-migrate)
/test
  resilience/             # replay of known failure scenarios (§9.3)
```

**Stack choices** (use unless a blocker emerges): Go ≥1.23; `net/http` + `chi` for routing; `coder/websocket` for WS; `pgx/v5` + `sqlc` for Postgres; `golang-migrate` for migrations; OpenTelemetry SDK; `log/slog` with a structured envelope carrying `correlation_id`, `session_id`, `sandbox_gen`.

## 2. Core runtime model: the session actor

- One goroutine + mailbox (channel of commands) per **active** session. All mutations of a session's state go through its actor — no other code path writes session/sandbox/turn rows.
- **Hydration on demand**: actor loads state from Postgres on first command, evicts after idle TTL (default 30 min without commands or connected clients).
- **Single-writer across replicas**: Postgres advisory lock keyed by session id, held for the actor's lifetime, plus a **fencing check**: every write includes the actor's `epoch` (bumped on each acquisition); writes with a stale epoch fail. A zombie actor on an old pod can never corrupt state.
- **Transactional writes**: state transition + appended event + outbox entries commit in ONE Postgres transaction. There is no such thing as a fire-and-forget state write.
- **Named persistent timers**: table `session_timers(session_id, name, fires_at)`. Names: `connecting_deadline`, `liveness_check`, `inactivity`, `turn_deadline`, `terminal_grace`. A per-pod timer pump polls due timers (`SELECT ... FOR UPDATE SKIP LOCKED`) and delivers them as actor commands. Timers survive restarts; each is armed/re-armed independently.

## 3. Domain model & state machines

### 3.1 Session

`created → active → completed | failed | cancelled` (+ `archived` flag). Status is **derived** after each turn: pending work → `active`; else terminal per last turn outcome. `cancelled ≠ failed ≠ timeout ≠ never_started` are distinct from day one (separate `status` + `failure_reason` enum).

### 3.2 Sandbox (the critical machine)

```
pending → spawning(gen) → connecting → booting → ready ⇄ snapshotting
                                          │
                       (silence/timeouts) ▼
                              suspect → [grace] → stopped | failed
recovery: stopped|stale + snapshot        → restore (new gen)
          stopped|stale + resume-capable  → resume  (same provider sandbox)
          failed + snapshot               → restore (new gen)
```

Hard rules:
- **Spawn generation (fencing)**: every spawn/restore increments `sandbox.gen` (monotonic, per session). Sandbox WS connections, provider callbacks, and status writes carry `gen`; stale-gen inputs are rejected **and logged** (they must never be able to wedge the session). DB enforces one sandbox row per session (`UNIQUE(session_id)`); history goes to `sandbox_history`.
- **Liveness = max of all signals**. `last_seen_at` is updated by: heartbeat, boot-progress report, any agent event (tool_call, token, step), any WS frame. Every watchdog measures from `last_seen_at` and re-arms on it.
- **Two liveness budgets**: `first_connect_budget` (default **240s**, covers provider cold start + boot) distinct from `steady_heartbeat_budget` (default **90s**; heartbeats every **30s**). Boot-progress reports during long boots re-arm the connecting deadline.
- **Two-phase terminalization**: a watchdog never writes `failed` directly. It writes `suspect` and arms `terminal_grace` (default 60s). Any liveness signal during grace returns to previous state. A genuinely late success (e.g. `execution_complete` arriving after terminalization) **reconciles**: turn marked completed, session status re-derived, automation run counters corrected. This narrows the false-failure window rather than eliminating it — see the note just below the list for the residual case and what closing it would require.
- **Circuit breaker**: 3 permanent spawn failures within 5 min blocks spawning for that session. Unknown provider errors default to **transient**, never permanent — a novel transient failure must not trip the breaker.

Two-phase terminalization's reconciliation above is scoped to a late signal that still finds the turn `Processing` — true whenever it arrives inside `turn_deadline`'s own, separate, much longer budget (60 min vs. `terminal_grace`'s 60s, `platform/timeouts.go`), the ordinary case. What that scoping makes unrepresentable is the contradiction class — a delivered artifact and a failure message both standing for the same turn at once — since a turn still `Processing` can only ever resolve to one clean terminal write, never a conflicting second one. The residual case stays a known limitation: a real `execution_complete` arriving after the turn has already gone terminal via its own `turn_deadline` timeout is logged and discarded, not reconciled — the user is told the turn failed while the work actually completed. Closing this residual would require late-success reconciliation to enqueue a corrective follow-up notification through the same outbox (§5.1) once the original `failed` transition's own notification has already been dispatched; without that, the contradiction class this design otherwise prevents re-enters through that door.

### 3.3 Turn (prompt)

`pending → dispatched → processing → completed | failed | cancelled`. Exactly one `processing` per session. Enqueue → if no live sandbox, trigger spawn and return (dispatch happens when sandbox connects). Dispatch arms `turn_deadline`. On terminal event: complete turn, trigger snapshot, re-derive session status, dispatch next pending. Stop/failure paths emit a **synthetic** `execution_complete` event so clients always see one terminal event per turn. The turn records the OpenCode conversation id **at turn start** (also reported on every heartbeat) so follow-up prompts on a fresh sandbox resume the same conversation — never lazily.

### 3.4 Git state (inside sandbox, enforced by sandbox-agent)

- Image builds must snapshot a **clean tree** (commit or clean `setup.sh` residue before snapshotting).
- Boot: `stash-if-dirty → checkout session branch (create from base if absent) → stash pop`. User working-tree edits are durable data — losing them is a P0.
- Branch names normalized (lowercase) before push. Repo paths: multi-repo under `/workspace/{name}`, position 0 = primary. Repos are **always a list** — no scalar single-repo mirror anywhere.

### 3.5 Automations

`automation → invocation → run(s)` (one run per target, fan-out ≤10). At-most-one failure strike per invocation via CAS (`UPDATE ... WHERE failure_counted_at IS NULL`). Auto-pause after 3 consecutive failed invocations. Recovery sweeps: orphaned `starting` runs >5 min, `running` >90 min. Failed image builds retry with exponential backoff (not fixed 30 min) and alert on streaks.

## 4. Ports (interfaces in `/internal/app/ports`)

### 4.1 SandboxProvider (complete — no out-of-interface operations)

```go
type SandboxProvider interface {
    Capabilities() Capabilities // snapshots, resume, explicitStop, imageBuilds
    CreateSandbox(ctx, CreateSpec) (SandboxRef, error)   // Spec includes gen + full SESSION_CONFIG doc
    StopSandbox(ctx, SandboxRef) error
    ResumeSandbox(ctx, SandboxRef) error                  // optional per capabilities
    TakeSnapshot(ctx, SandboxRef) (SnapshotID, error)
    RestoreFromSnapshot(ctx, SnapshotID, CreateSpec) (SandboxRef, error)
    BuildImage(ctx, ImageSpec) (BuildOutcome, error)      // image prebuilds are IN the interface; BuildOutcome{Ref, PublishedCacheVersion} since Step 43(c)'s third iteration (§19.1)
    DeleteImage(ctx, ImageRef) error
    List(ctx) ([]SandboxRef, error)                       // for reconciliation/GC
}
```

Errors are typed `ProviderError{Transient bool}` — classification by provider-specific error codes, **never** by string-matching messages. The provider HTTP client timeout MUST exceed the provider's worst cold-start (Modal cold scheduling alone can take 220s+).

Implement: `modal` (via its API; sandbox env passed as one `SESSION_CONFIG` JSON document — the provider never assembles env fragments) and `rwx` (detailed design: §4.1.1; PR preview links: §4.1.2). All Modal traffic goes through the configurable egress proxy.

### 4.1.1 RWX adapter (Step 57)

RWX (rwx.com) is the second `SandboxProvider` implementation (`adapters/outbound/rwx`), integrating RWX's real, public product exactly as `modal` integrates Modal's API and `githubapi` integrates GitHub's. Everything below is grounded in RWX's own published documentation (www.rwx.com/docs, fetched 2026-08-06); where that documentation is silent, the gap is named in §4.1.3 rather than papered over with an invented API shape — the same verified-against-the-vendor's-own-docs discipline §17.6 applied to GitHub's stacked-PR documentation.

**What RWX actually offers.** Three primitives matter to Narvi. (1) **Sandboxes** — "Run commands in persistent sandboxes" (CLI reference): per-identity VM environments configured by a YAML document (`base` OCI image + `tasks` + `background-processes` with ready checks, Docker support), spun up from RWX's content-addressed cached filesystem layers ("spin up in seconds, not minutes"), auto-stopped "after 30 minutes of inactivity" (configurable: `--first-exec-timeout`, `--inactivity-timeout`), identified "by git branch and the absolute path to the config file", with `rwx sandbox start|exec|stop|reset|list` lifecycle verbs and `rwx sandbox reset` returning to "a fresh state (discarding any changes)". (2) **Dispatched runs** — the one documented HTTP API: `POST https://cloud.rwx.com/mint/api/runs/dispatches` (`Authorization: Bearer $RWX_ACCESS_TOKEN`, JSON body `{key, params, ref, title}`, 201 → `{dispatch_id}`), then `GET …/dispatches/:dispatch_id` polled to `not_ready | error | ready` with `runs[{run_id, run_url}]`. (3) **Preview apps** — §4.1.2's whole subject.

**Transport: the pinned `rwx` CLI, never an invented REST shape.** RWX documents no HTTP API for sandbox lifecycle — its programmatic sandbox surface is the CLI (global `--format json`) plus the Dispatches API above. The adapter therefore drives the pinned `rwx` CLI binary as its transport for sandbox operations: pin the version, record it in the boot fingerprint's environment (§5.3), and run **CI contract tests against the real pinned binary** — the exact discipline §7 already applies to OpenCode, and strictly better than the Modal adapter's own openly-admitted invented wire shapes (modal/doc.go: exercised against a fake, "NOT against real Modal API docs"). The token travels to the subprocess as `RWX_ACCESS_TOKEN` (RWX's documented mechanism: "For programmatic use, set the token as the `RWX_ACCESS_TOKEN` environment variable"), never as `--access-token` argv — argv is visible to process listings (§5.2's leak-class discipline). Credential: an RWX **service account** token, per RWX's own guidance (a personal token "acts as you"; a service account "is owned by the organization, so it survives you leaving it") — wired as static adapter config exactly like `modal.Config.AuthToken`, validated fail-fast; this is a control-plane outbound credential, not a Step 53 sandbox-injected secret. Egress: the subprocess inherits proxy env vars (`HTTPS_PROXY`) so RWX traffic routes like Modal's; whether the CLI honors them is §4.1.3's to verify.

**Capabilities — declared from what RWX verifiably supports, not from hope:**

```go
Capabilities{Snapshots: false, Resume: <verified>, ExplicitStop: true, ImageBuilds: false}
```

- `ExplicitStop: true` — `rwx sandbox stop` is a real, documented operation.
- `Resume` — the one flag Step 57 must settle **empirically, as its first exit criterion**. RWX's docs imply stop→start state preservation ("the sandbox persists between commands"; `reset` exists specifically to discard changes — redundant if a plain stop already discarded them) but never state it outright. If verification confirms preservation, `Resume: true` and RWX becomes the §3.2 "stopped|stale + resume-capable → resume (same provider sandbox)" provider that `ports/capabilities.go`'s own doc comment anticipated. If not, `Resume: false` — and, with `Snapshots` also false, a stopped RWX sandbox's only recovery is recreate-from-scratch: §3.2's snapshot-restore lane simply does not exist for RWX sessions, and §3.4's push discipline is the only durability for working-tree state. That consequence is accepted and named, not hidden — callers already branch on `Capabilities()` (the port's own "optional per capabilities" contract).
- `Snapshots: false` — RWX snapshots task filesystems into its content-addressed cache automatically, but exposes no addressable take-snapshot-now/restore-from-handle API; a cache keyed by content is not a `SnapshotID` a caller can mint and hold. `TakeSnapshot`/`RestoreFromSnapshot` return the permanent `UNSUPPORTED_OPERATION` `ProviderError`, mirroring `modal.ResumeSandbox`'s established pattern.
- `ImageBuilds: false` — `rwx image build|push|pull` exist ("Launch a targeted RWX run and pull its result as an OCI image") but no image delete is documented, and `ImageBuilds` covers `BuildImage` **and** `DeleteImage`. The honest posture: report false; §19's build pump then never engages for RWX-backed sessions and the systematic fallback-to-base (§10 Phase 2) applies — which costs little, because RWX's own content-addressed layer cache already provides the warm-boot effect §19 builds by hand for Modal: unchanged setup work re-hits RWX's cache natively.

**Method mapping.** `CreateSandbox`: generate a per-(session, gen) sandbox config whose `base` image is `spec.Image` (an OCI reference; empty = adapter default) and whose identity — RWX keys sandboxes on branch + config path — embeds session id **and gen**, so two gens can never collide onto one RWX sandbox (§3.2 fencing at the provider's own identity layer); `SESSION_CONFIG` travels as ONE opaque JSON value (a single init param/env entry — §4.1: the provider never assembles env fragments); the sandbox-agent runs as the long-lived exec'd command, its supervised services as `background-processes` with ready checks (§14.2's manifest maps naturally). `StopSandbox`: `rwx sandbox stop`. `ResumeSandbox`: `rwx sandbox start` against the same identity (per the `Resume` verification above; if false, permanent `ProviderError` like Modal's). `List`: `rwx sandbox list --format json` for §4.1's reconciliation/GC (org-wide visibility to verify, §4.1.3). `SandboxRef.ProviderID` holds whatever identity the pinned CLI's JSON output reports — opaque outside the adapter (`ports/refs.go`).

**`ProviderError` classification.** Same shape as Modal's table, same §3.2 default: on the Dispatches API path, HTTP status class (network failure/429/5xx transient; 400/401/403/404/409/422 permanent; anything else transient — "unknown provider errors default to transient, never permanent"); on the CLI path, the classification inputs are the process exit code and the `--format json` error envelope — **never** prose message matching (§4.1's own rule). RWX publishes no error-code taxonomy today; the concrete exit-code/envelope table is pinned by the real-binary contract tests, and until a failure mode is pinned it classifies transient.

**Timeouts (§5.4).** RWX's published latency claims ("seconds, not minutes"; "~3s" preview-app start) are directional, not engineering figures — no p50/p99 is published. `ProviderWorstColdStart` remains governed by the fleet's worst provider (Modal's 220s+ cold scheduling dominates every RWX figure), so the §5.4 chain (`providerHTTPClientTimeout > provider worst cold start`, `first_connect_budget > image pull + boot p99`) already holds for RWX without new margins; RWX-specific p99s are measured empirically before any margin is tightened, never assumed from marketing copy. Two RWX knobs join `platform/timeouts.go` (no literal anywhere else): the CLI subprocess exec timeout, and the sandbox `--inactivity-timeout` — set **above** Narvi's own session-idle authority (§2's 30-min actor TTL and the `inactivity` timer) so Narvi's timers, not RWX's, decide idleness. A provider-initiated auto-stop that fires anyway is an ordinary entry into §3.2's `stopped` state feeding the resume/recreate lane — an expected event, never a failure.

### 4.1.2 PR preview links at the latest PR commit (Step 57)

§8.9's exit criterion, built on RWX **preview apps** — RWX's own primitive for exactly this: a run task carrying `app: {endpoint, port, timeout}` serves "traffic on publicly accessible URLs under rwx.run", with a **canonical** URL (`https://{task-cache-key}--{org-slug}.rwx.run`, "pinned to one specific build") and a **friendly** URL (`https://{endpoint}--{org-slug}.rwx.run`, always the latest build — RWX's docs: "Use for shared PR links that automatically pick up new commits", fetched 2026-08-06). Apps start on demand (~3s claimed), spin down "after 10 minutes of inactivity", restart on the next request. Narvi's job is therefore two small side effects per push — (a) trigger a preview build at the PR's newest commit, (b) attach the link to that commit — both delivered through the existing outbox machinery (§5.1), never a new delivery pathway.

1. **Trigger and enqueue point.** `push_complete` (§6.1) already carries `repos[].sha` — the pushed head. The sessionactor's own PR-creation path (`pushpr.go`'s `createPRBestEffort`, which has just ensured the PR exists and holds its `PRRef`) is the ONE enqueue point: for each pushed repo whose per-repo preview setting is present, one small fresh transaction (mirroring `recordPRArtifact`'s own established fresh-transact pattern) writes a `preview`-typed artifact row + both outbox rows below. The `preview` slot in `artifact_type` and the §6.1 `artifact` event have existed since Steps 4/5 with no writer — this is their first real producer, and §12.2 item 1's artifacts rail renders it with zero new UI machinery. Per-repo setting `{dispatchKey, endpointTemplate, orgSlug}`; absent = feature off (off by default, §24.5's posture). **These three are not on any REST shape today (amendment).** The columns exist and only `previewpr.go` reads them, so the per-repository settings screen can neither show nor change preview configuration. Exposing them is not symmetric across the three: `endpointTemplate` and `orgSlug` are ordinary identifiers and read/write normally, while `dispatchKey` is the credential authorising a run dispatch and must follow the write-only rule every other credential in this codebase follows — accepted on write, never present in any response, with a fixed non-secret placeholder proving one is configured, exactly as `SandboxSecret`/`ProviderCredential` already do. A response that returned it, in any form, would be a regression. **Its own endpoint and its own action, not the combined repo-settings PUT.** `PUT /api/repos/{owner}/{repo}/preview-config`, gated by a new admin-only `ActionConfigurePreviewLinks` on §13.3's existing row alongside `ActionToggleSentinelAutoFix`/`ActionToggleAutoMerge`/`ActionToggleAutoRetriggerReview`/`ActionToggleDescriptionAutofix` — same row, same reasoning: arming this makes every agent push trigger a build on an external provider, unattended. Two reasons it stays off the combined `PUT .../settings`: that endpoint requires every permission its fields collectively need, which `UpdateRepoSettingsRequest`'s own description already gives as the reason §21's fields were kept off it; and a request body carrying a credential has no business sharing a shape with ordinary configuration, where a future caller sending "the complete desired state" would have to resend the secret or blank it. Absent from the request means unchanged, which is the one place on this surface where partial-state semantics are correct rather than a patch in disguise — and the reason to say so here rather than let it be inferred. No new trigger surface: sessions that never push never preview.
2. **`rwx_preview_dispatch` (new outbox kind).** Delivered by the `rwx` package's own `ports.Notifier` implementation (the outboxworker routes kind→notifier exactly as for every other kind): `POST /mint/api/runs/dispatches` with `ref` = the pushed sha, `key` = the repo's `dispatchKey`, `params` = `{pr-number, head-sha, session-id}` — surfaced to the run as `event.dispatch.params`, from which the repo's own `.rwx` run definition templates its app endpoint. The build runs on RWX's infrastructure from the ref itself — fully decoupled from the session's sandbox — and repeat dispatches are cheap by construction (content-addressed cache). Delivery is the fast dispatch POST only; it never waits for the build.
3. **`github_preview_link` (new outbox kind).** Delivered by a small `githubapi` notifier via a new `CreateCommitStatus` adapter capability: `POST /repos/{owner}/{repo}/statuses/{sha}` with `context: narvi/preview`, `state: success`, `target_url` = the friendly URL rendered from `endpointTemplate` + `orgSlug`, and a description carrying the ephemerality caveat (live while RWX serves it). A commit status **is** "a preview link at a commit": each push posts at the new head, GitHub surfaces exactly the head commit's statuses on the PR, and redelivery of the same (context, sha) converges instead of duplicating — strictly better here than `PostIssueComment`, whose own notifiers document double-posting on retry as an accepted limitation (`releasemanifestnotifier.go`), and zero timeline noise per push. Not GitHub Deployments: a preview that dies with RWX's idle reaper should not masquerade as a deployment environment.
4. **Friendly, not canonical.** The friendly URL is deterministic at enqueue time (no build to await inside a `Deliver`) and never goes stale — it is RWX's own "latest build for this PR" pointer, which is precisely §8.9's promise. The canonical per-build URL requires the finished build's task cache key (pollable via `rwx results <run_id>`); that is the v2 upgrade path if template drift bites (§4.1.3), not v1 machinery.
5. **Human pushes.** v1 covers agent-originated pushes (the `push_complete` trigger). A human push to the same PR updates neither preview nor status until the next agent push; §24's `pull_request/synchronize` ingress lane (Step 65) is the natural carrier for closing that gap — sharing its debounce, arriving with that Step — recorded here, not built in Step 57.

### 4.1.3 Step 57 risks and open questions

- **Sandbox-lifecycle API surface**: CLI-only per current public docs; RWX's token page says tokens work "through the API and the CLI", so a sandbox HTTP API may exist unpublished — worth a vendor question, since it would simplify the transport. Until then, the pinned-CLI contract tests are the drift guard.
- **stop→start state preservation** (the `Resume` flag): inferred from `reset`'s own "discarding any changes" contrast, never stated by the docs. First exit criterion, settled empirically; both outcomes are designed for (§4.1.1).
- **Error taxonomy**: no published error-code table for the CLI or the Dispatches API beyond `{status: "error", error: string}`; exit-code/envelope classification is pinned by contract tests, unknown → transient (§3.2).
- **`rwx sandbox list` scope**: reconciliation/GC (§4.1 `List`) needs org-wide truth; whether the CLI lists beyond the calling device/user is unverified.
- **Egress proxy**: whether the `rwx` CLI honors subprocess proxy env vars is unverified; if not, RWX traffic bypasses the §4.1 egress posture and needs a vendor answer.
- **`endpointTemplate` duplication**: Narvi renders the friendly URL from its own copy of the endpoint template; the repo's `.rwx` definition owns the real one. Drift = a dead link (annoying, never corrupting). v2: read the built app's URL from run results instead.
- **Preview URLs are public** ("publicly accessible URLs under rwx.run"): anyone holding the link sees the app. Per-repo opt-in is the mitigation; whether RWX offers org-restricted visibility is unverified. Secrets must never reach a previewed app's client bundle — §5.2 posture, worth restating in the setting's own UI copy.
- **First-click-during-first-build**: what a friendly URL serves before its first build completes is unverified (likely RWX's building/starting interstitial; empirically confirmed in Step 57).
- **Dispatch cost**: one RWX build per agent push; content-addressed caching bounds it, and the per-repo toggle contains blast radius. If telemetry shows waste, §24.2's trailing-edge debounce idiom is the ready-made coalescer.
- **Stale Step-48 references**: `ports/sandboxprovider.go`, `ports/capabilities.go`, `ports/refs.go`, `ports/providererror.go` and `adapters/outbound/rwx/doc.go` still say RWX lands at "Step 48"/"PR-48" — written before the Phase-5 renumbering that made it Step 57. Step 57's implementation PR corrects them.

**Phasing:** Step 57, Phase 5, ∥ — depends on Step 12 (the port + Modal precedent), Step 21 (`SourceControl`/`pushpr.go`), and Step 35 (outbox delivery workers), all long since landed; independent of Steps 53-56 and of §24/Step 65 (whose synchronize lane only ever extends §4.1.2 point 5). One PR: adapter + contract tests + the two notifiers + the enqueue path + the per-repo setting, per the one-Step-one-PR convention.

### 4.2 AgentRuntime (OpenCode anti-corruption layer — see §7)

```go
type AgentRuntime interface {
    StartTurn(ctx, TurnSpec) (ConversationID, <-chan AgentEvent, error)
    ResumeConversation(ctx, ConversationID, TurnSpec) (<-chan AgentEvent, error)
    Stop(ctx) error
}
```

### 4.3 Others

`SourceControl` (GitHub + GitLab: createPR, credential minting, push specs), `Notifier` (Slack/Linear/GitHub comment delivery — consumed via outbox only), `IntentClassifier`, `LLM`, `BlobStore` (full interface — §28.1), `SessionStore`/`TurnStore`/`SandboxStore` (sqlc-backed), `Outbox`, `TimerScheduler`, `Clock` (injectable everywhere — no `time.Now()` in domain).

## 5. Cross-cutting invariants

### 5.1 Persistence
- Postgres is the ONLY store. No cache with authority. Uniqueness by constraints, not convention.
- **Outbox pattern** for every outbound side effect (Slack/Linear/GitHub notifications, webhooks): written in the same tx as the state change; a retry worker delivers with exponential backoff + dead-letter after N attempts. Never 2-attempts-then-drop.
- Dedupe/coalescing (webhook events, concurrent PR @mentions) via `INSERT ... ON CONFLICT` atomic claims. Never eventually-consistent storage for coordination.
- A human applying a label or clicking a button is a legitimate, deliberate command — the two are equivalent in kind, and neither needs Postgres's own blessing to act. What is never legitimate is treating a label the BOT ITSELF writes back onto an externally-editable surface as durable trigger state: a GitHub/Slack/Linear label is mutable by anyone with triage rights, forgeable, and — the decisive reason — a second copy of a fact Postgres already owns. Durable trigger state lives in Postgres and is only ever read back from there; a bot-written label may still exist as a human-facing status indicator, but the system itself must never treat it as authority (§24 applies this to review re-triggering specifically; the principle is general).

### 5.2 Security
- Internal auth: single HMAC helper (`platform/auth`), bearer `timestamp.signature`, 5-min window, fail closed. **Separate secrets per direction** (sandbox→CP, CP→bots, webhook ingress) so one rotation doesn't touch everything.
- Sandbox tokens: hashed at rest, one per gen, rotated on identity rotation with a **previous-gen grace window** during overlapping spawns.
- Git credentials: never long-lived in sandbox. `sandbox-agent` implements a git credential helper that POSTs to CP `/sessions/{id}/scm-credentials` (sandbox bearer), caches to disk with flock, 5-min expiry buffer, scoped https+host only. Never fall back to stale cache.
- Agent policy enforcement is **server-side**: review verdict posting, formal PR review submission, and raw-comment blocking are CP endpoints with policy checks (verdict floor, scoping to review sessions) — never prompt-only invariants.
- PR diffs and external content are untrusted input: wrap them in delimited blocks and treat them as data, never as instructions.
- Any re-run/re-review phrasing a posted verdict (§8.2/Step 47) recommends to a user is rendered server-side from the verdict's own typed fields (Step 45), never generated or reproduced by a model directly — and that exact phrasing must be recognizable by the intent classifier's deterministic fail-open fallback (§18.1's `FallbackReason`, §18.2's independent-deterministic-check requirement for an irreversible action), not only by its model-based path. A product that recommends a phrase only its LLM-backed classifier can understand becomes unusable at the exact moment that classifier is degraded — the one moment this requirement exists to cover.

### 5.3 Observability (day one, not later)
- `correlation_id` minted at ingress, propagated: webhook → CP → provider → sandbox-agent → OpenCode wrapper → back.
- `sandbox-agent` logs a **boot fingerprint** first: binary version, image digest, repo SHAs, boot mode.
- Every state transition logs `from`, `to`, `trigger`, `gen`. Every routing/classifier decision logs its inputs and verdict.
- OTel traces + metrics: spawn latency, boot phase durations, liveness gaps, watchdog activations (and how many were false alarms — target: ~0), outbox lag, orphan GC count.

### 5.4 Timeout hierarchy (single source: `platform/timeouts.go`)
One struct, validated at boot with the invariant chain asserted in a unit test:
`provider hard cap (2h) > supervisor turn cap > CP turn_deadline > OpenCode SSE inactivity timeout`, each with explicit margin. Also: `providerHTTPClientTimeout > provider worst cold start`; `first_connect_budget > image pull + boot p99`. No timeout literal anywhere else in the codebase.

## 6. Wire contracts (frontend and sandbox protocol)

These are the canonical contracts the web UI and the sandbox agent speak. Formalize each in `/contracts` with round-trip tests — `/contracts` is the single source of wire truth.

### 6.1 Sandbox WS (sandbox-agent ↔ control plane)
- Connect: `wss://…/sessions/{id}/ws?type=sandbox`, `Authorization: Bearer <sandbox_token>`, `X-Sandbox-ID` (+ NEW: `X-Sandbox-Gen`). Server: 410 when session stopped, 403 on id/gen mismatch. Agent treats 401/403/404/410 as fatal (no retry); else exponential-backoff reconnect.
- CP→agent commands: `prompt` (with author scmName/scmEmail for git attribution), `stop`, `push` (per-repo spec; CP awaits `push_complete`, 360s), `snapshot`, `shutdown`, `ack`, `git_sync_complete`.
- Agent→CP events: `ready`, `heartbeat` (30s, carries conversation id + `last_boot_phase`), `boot_progress`, `token` (cumulative text, upsert-by-messageId not append), `tool_call`/`tool_result`, `step_start`/`step_finish` (carries `cost`; NOTE: `tokens` is an **object**, not a number — a number-vs-object mismatch here silently zeroes cost tracking, so pin it in the contract test), `sub_task_start`/`sub_task_finish` (§7.1; `sub_task_start` additionally carries an optional `subAgentType`, Step 71, §26.4 — the task tool's own real `subagent_type` dispatch parameter, distinct from `label`'s freeform text, used to corroborate a self-reported `counter-reviewer` dispatch against the persisted trace), `git_sync`, `artifact`, `execution_complete`, `push_complete`/`push_error`, `session_title`, `warning`, `error`, `snapshot_ready`.
- **Ack protocol**: 6 critical types (`execution_complete`, `error`, `snapshot_ready`, `push_complete`, `push_error`, `sub_task_finish`) carry deterministic `ackId = "{type}:{messageId}"`; sender buffers (1000 events, evict oldest non-critical) and re-sends on reconnect until acked; receiver dedupes by upsert-on-messageId. `sub_task_finish` joins the critical set because it closes an "active" state the UI tracks (§12.2 item 1's live sub-lane count) exactly like `execution_complete` does at the turn level — a dropped, never-redelivered `sub_task_finish` would leave a sub-lane stuck active forever, live and in history, with no reconciliation path (the same failure class §3.2's two-phase terminalization and §9.3 #4/#7 exist to prevent at the turn level).
- **Sub-task fan-out** (§7.1): every event type emitted during turn processing additionally accepts an optional `subTaskId` (absent/null = the turn's main lane), for envelope uniformity — session/connection-lifecycle events (`ready`, `heartbeat`, `boot_progress`, `git_sync`, `session_title`, `warning`, `snapshot_ready`) never populate it, only turn/tool/step-scoped events do — so a lane is always unambiguous even when several sub-tasks' events interleave on the wire. `sub_task_start` (`subTaskId`, `label`, `parentMessageId` — the `messageId` of the main-lane `tool_call` event whose invocation spawned this sub-task) and `sub_task_finish` (`subTaskId`, `outcome`: `completed | failed | cancelled`, reusing the turn's own taxonomy, §3.3) bracket a sub-task's lifetime. The model is flat — a sub-task cannot itself spawn a further-nested sub-task.

### 6.2 Client WS (browser ↔ control plane)
Connect → `subscribe{token, clientId}` within 30s → single `subscribed` payload (full state + event replay + artifacts + participants) → broadcast stream. `fetch_history` cursor pagination. Close codes: 4001 = re-auth, 4002 = token expired. WS token: per-participant, hashed at rest, 24h TTL, minted via REST (`/api/sessions/:id/ws-token`).

### 6.3 REST
The BFF-facing routes: sessions CRUD/create, events, artifacts, secrets, environments, automations, uploads, ws-token. Generate TS types for the UI from `/contracts`; `/contracts` is the single source — no hand-written response types.

### 6.4 Sandbox boot contract
Boot modes: `build | fresh | repo_image | snapshot_restore` (env `NARVI_BOOT_MODE`). Hook policy: `setup.sh` runs only in fresh/build (fatal only in build); `start.sh` runs in all non-build modes (primary repo fatal, secondaries warn). Multi-repo ordered clones under `/workspace/{name}` + generated `AGENTS.md` manifest. Tunnel URLs delivered via provider; sandbox env persisted to `/workspace/.env.sandbox`.

**Amendment (§19.4, warm-boot shared images, Step 42):** under `repo_image`, `setup.sh`'s `ShouldRun` is no longer a flat "never" — it reruns, non-fatally, whenever the boot-time workspace has moved from the image's own built SHA. This is a breaking change to the contract as stated above (Conventional-Commits `!` marker required on the landing PR) — see §19.4 for the full redefinition and rationale.

## 7. OpenCode integration (`adapters/outbound/opencode`)

The most delicate area of the system — budget real care here.

- Pin the OpenCode version in the image; record it in the boot fingerprint.
- The adapter runs inside `sandbox-agent`: starts the OpenCode server, POSTs `prompt_async`, consumes the SSE `/event` stream, translates to typed `AgentEvent`s.
- Known quirks to handle: correlate by generated lexicographically-ascending message IDs; filter the global event stream by session id; track sub-tasks (§7.1 — not Narvi's own child sessions, §14.4); handle compaction events; dedupe tool states by `sid:callID:status`; treat "no output" as failure; final-state fetch fallback; SSE inactivity timeout configurable (default 120s).
- Model catalog: injected server-side config; must survive OpenCode upgrades — on empty/failed catalog, fall back to a pinned known-good set (a version bump can silently drop a provider's models — the fallback prevents that).
- **Contract tests in CI against the real OpenCode binary**: start it, run a scripted turn, assert every event shape against `/contracts`. An OpenCode version bump that changes any shape must fail CI, not production.

### 7.1 Sub-task fan-out

Problem this solves: some engine behaviors spawn multiple concurrent sub-agents within a single turn — OpenCode's own task-tool sub-agents today; the same class of behavior exists in other engines under other names (e.g. a coding assistant's own dynamic multi-agent workflow mode), mentioned here only as the general shape of the problem, not something Narvi special-cases per engine. Left untranslated, this reads to the control plane as one flat, interleaved event stream: unattributable, and easy to undercount on cost.

- The adapter assigns each spawned sub-task a stable `subTaskId` (derived from whatever correlator the engine itself exposes — OpenCode's own nested-task id today; not Narvi's own session concepts, per the note below) and tags every event that sub-task produces with it (§6.1), emitting `sub_task_start`/`sub_task_finish` to bracket its lifetime. `sub_task_start` additionally carries an optional `subAgentType` (Step 71, §26.4) sourced from the task tool's own real `subagent_type` dispatch parameter (VERIFIED LIVE: `{"description","prompt","subagent_type"}`) — unlike `label` (freeform, not correctness-bearing), this is the engine's own reliable dispatch parameter, which is why §26.4's post-hoc sub-task corroboration keys off it rather than `label`.
- **Not a new domain entity.** A sub-task is a presentation/wire-level grouping of events belonging to one turn — not a new Postgres row, and not Narvi's own "child session" (§14.4: a full session with its own sandbox/turns, spawned by automation/sentinel features — a materially heavier mechanism; the naming here is deliberately distinct so the two are never confused). The turn state machine (§3.3) is unaffected: one turn still has exactly one `processing` state no matter how many sub-tasks ran underneath it.
- **Cost rolls up — a real, shipped accumulator as of Step 70, but scoped to the OpenCode adapter's own in-memory turn state, not Postgres.** An earlier draft of this bullet claimed, in the present tense, that every `step_finish.cost` (§6.1) "is summed" into one turn/session total regardless of lane, before any such accumulator existed — **verified false at the time**: the only cost columns anywhere in the schema were `repo_settings.review_cost_budget_light_usd`/`..._deep_usd` (migration 000085), Step 69's own configured *ceilings*, and no running total existed anywhere. That gap is what Step 70 closes, but the fix landed adapter-side, not schema-side: `internal/adapters/outbound/opencode`'s own `turnState` (turn.go) carries a `spentUSD float64` field, summed by `dispatchPart`'s `"step-finish"` case (sse.go) for every step-finish this turn observes — main lane and every sub-task alike, since §7.1's own fan-out routes every sub-task's events back to the SAME `turnState` pointer, tagged only with a `subTaskId`, so the summation needs no lane-specific case at all. This total lives and dies with one turn's own in-process `turnState` — the individual per-step `cost` figures still flow to the control plane exactly as before, unchanged, on each `step_finish` event (§6.1), but the RUNNING SUM itself is never persisted to Postgres, never itself transmitted over the sandbox WS, and never visible outside the one `sandbox-agent` process running that turn. It exists to answer exactly one question, locally: "how much has THIS review spent so far", read back via `Adapter.CurrentTurnSpentUSD` (adapter.go) by `cmd/sandbox-agent`'s own new loopback HTTP server (§26.7's own mechanism, reviewcostbudgetserver.go) — a sandbox-process-local total is sufficient for that, and a Postgres-backed one would need a real wire-contract change and a migration this Step does not make. (Per-model cost attribution when a sub-task runs on a different model than its turn — §12.2 item 6's cost-by-model view — is still not designed here; that needs its own `step_finish` model field before it can be claimed, left to whichever future work actually adds it. A control-plane-visible, cross-turn/session cost total, if ever needed, is also still unbuilt — this accumulator does not attempt it.)
- Phasing: adapter-side tagging is Step 17 (OpenCode adapter, alongside the other quirks on this line); UI rendering of sub-task lanes is Step 82 (session timeline, lane nesting) and Step 83 (session rail, cost-breakdown roll-up) — see §12.2 item 1.

### 7.2 Context-overflow compaction retry (Step 44)

Problem this solves: a long-running turn (many tool calls/steps) can outrun the model's context window mid-turn, or a sandbox restart can reload a session's full history and overflow on the very first call after resume. Today this is invisible as a distinct failure: the adapter's tagged-union decoder already recognizes it by name (`openCodeTaggedError.Name == "ContextOverflowError"`, one of the schema-derived-only union members alongside `ProviderAuthError`/`UnknownError`/`APIError`/etc., `types.go:128-138`) but `deriveOutcome` folds every non-abort error name into the same generic path: `reason := fmt.Sprintf("opencode: %s", err.Name)` → `ExecutionCompleteOutcomeFailed` (`outcome.go:38-45`). A context-overflow turn today just fails, with no attempt at recovery — the only recourse is a human-initiated retry (§8 item 7, Recovery UX).

The adapter already has a *partial* answer for the auto-compaction case: an `Overflow`-flagged `compaction` SSE part is translated into a wire `Warning` when the engine's own auto-compaction fired mid-turn (`sse.go:314-332`, `translate.go`'s `translateCompactionOverflow`) — but that only covers the case where auto-compaction already succeeded on its own. There is no path today that *forces* a compaction after the fact and retries the failed call; `types.go:162-173` records that the adapter's own research pass found a manual-compaction endpoint (`POST /session/{id}/compact`) that returned "not available yet" on the pinned OpenCode version — a different, always-available summarization endpoint is the one this design uses instead (point 2 below).

**Design — entirely inside the OpenCode adapter (`adapters/outbound/opencode`), no port change:**

1. **Classify on the typed discriminator already decoded.** `err.Name == "ContextOverflowError"` (`types.go:136-138`) is a genuine typed signal, not string-matching a provider error body — the same discipline §4.1 requires of `ProviderError` and §18.1 requires of `FallbackReason`.
2. **Force a compaction via the endpoint that actually works.** New `compact.go`: `forceCompaction(ctx, sessionID string) error` issues `POST /session/{id}/summarize` through the adapter's existing `doJSON` helper (`client.go:47`), bounded by a new `OpenCodeSummarizeTimeout` (propose 120s — a single non-streaming summarization call, generous by the same "chosen generously when the concrete cost is unknown" convention `HookTimeout`'s own doc comment already uses, `platform/timeouts.go:250-254`). Every new timeout still lives in `platform/timeouts.go` (§5.4) — no literal anywhere else.
3. **One retry, inside the same `StartTurn` call.** In `turn.go`'s turn lifecycle: on a `ContextOverflowError`, defer surfacing that error to the caller, call `forceCompaction`, and on success re-post the same prompt exactly once. If the retried call also overflows, or `forceCompaction` itself fails, the *original* deferred error is what reaches `deriveOutcome` (with the reason string noting a compaction attempt was made) — never a silent double failure. This keeps `AgentRuntime.StartTurn`'s existing contract intact: exactly one `execution_complete`-shaped terminal event per turn (`agentruntime.go:110-119`), so §3.3's turn machine never observes the transient failure or needs a new state.
4. **No new `FailureReason`.** An unrecovered overflow still resolves to `FailureReasonFailed` (`domain/turn/failurereason.go:14-18` — the four values are migration-pinned to `session_failure_reason`, `migrations/000004_sessions.up.sql`); the differentiator lives in the reason string (`outcome.go`) and structured logs (§5.3: correlation_id-scoped, logging the classify/compact/retry decision at each step), not in a new enum value.
5. **CI contract test asserts the endpoint exists.** Extend §7's real-binary contract tests (Step 17) to assert `POST /session/{id}/summarize` is present and returns 200 on the pinned OpenCode version — mandatory, not optional, given `types.go:162-173` already proves this adapter probed a *sibling* endpoint and found it unavailable once; an OpenCode version bump that drops or renames `/summarize` must fail CI, never surface as a silent production regression.

**Why adapter-local and not a port/domain change:** the recovery action (which endpoint to call, how to detect the condition) is entirely OpenCode-specific — exactly what §7's anti-corruption layer exists to contain, and CLAUDE.md's "don't couple a port to a single adapter" rule argues against threading engine-specific recovery through `AgentRuntime` for a single implementation. A typed `AgentRuntimeError` (mirroring `ports/providererror.go`'s `Transient bool` classification) only becomes worth adding if and when a second engine adapter needs the control plane itself to arbitrate recovery — not before.

**Interaction with turn recovery (§8 item 7, §3.3):** this shrinks, rather than duplicates, the manual-retry class Recovery UX otherwise absorbs — one whole failure mode (mid-turn context overflow) resolves without ever reaching the user as a failed turn. It is unrelated to the sandbox-loss re-enqueue path (`sessionactor/dispatch.go`'s `planReenqueueOrRespawn`/`tryPlanReenqueue`, §9.3 scenario #2): that path recovers from the *sandbox* dying; this recovers from the *agent* reporting a recoverable error on a live sandbox, entirely within one `StartTurn` call, before the turn ever terminalizes.

**Phasing:** Step 44 — one PR: the classifier + `forceCompaction` + retry loop + `OpenCodeSummarizeTimeout` + table-driven unit tests on the retry decision + a fake-server test (mirroring `fake_server_test.go`'s existing precedent) + the one real-binary contract case above. Independent of every warm-boot Step (§19) — it touches only the OpenCode adapter, and can land whenever Step 17 (OpenCode adapter) is already merged, in parallel with anything else.

## 8. Feature set (exit criteria, not options)

1. **Plan mode**: persistent plans, HITL approve/reject on web/Slack/Linear/GitHub, server-side implementation dispatch on approval, plan/build model split, cross-channel verdict + archive notifications.
2. **Code review**: review sessions per PR with session reuse; atomic claim coalescing of concurrent @mentions; risk-map verdict with `review:*` labels — **a structured verdict from day one** (premise state, risk drivers, shippable class — server-computed, never self-reported, never re-parsed from posted text; full design and the automation policy built on it in §21); test-coverage & doc-drift sentinels; **server-side** verdict floor + formal-review gate + verdict-posting tool (raw issue comments blocked, scoped to review sessions); re-trigger via label/button, or automatically on new commits (debounced, off by default per repo, §24); inline diff pre-fetched into context (agent must not need to run `gh pr diff` repeatedly); suggestion safety (apply via validated endpoint); **criteria-driven auto-approval** (`visual-qa: pass/skip` unchanged; `review: low risk` **inverts** into a `review: needs-human` escape hatch — approval itself is deterministic and criteria-driven rather than label-triggered, §21); dedicated review model selection; optional sentinel auto-fix for coverage/doc-drift findings, merge-gated on the origin PR (§17, disabled by default); **review as a merge readout** (§26) — the verdict front-loads a diff-derived summary, the diff's architecture choices, and its risks to the stack, demoting findings to a collapsed appendix; a description-adequacy check with a third raise-only floor and graduated remediation; deterministic light/deep review triage, measurable per path; adversarial counter-review with contested-points surfacing on the deep path; a diff-only fact-check pass on both paths that kills only provably-wrong findings (§26.6); a per-path cost budget with dispatch-time look-ahead (§26.7); findings anchored to the diff by content, never a guessed line number (§22.1.1).
3. **Unified intent classifier** (detailed design — see §18): review-vs-request and plan-vs-build across all ingress surfaces; shadow mode (log-only) → active, permanently available, never a one-time launch gate; never-throw contract with an enumerated fallback-reason taxonomy; confidence rubric anchored on textual directness, not model self-reported certainty; DB-backed editable prompt templates with assembled-prompt preview; per-session routing decision records (§18.4).
4. **Automations**: GitHub/Linear/webhook/cron triggers with condition builder; sandbox settings honored on automation sessions; creator/status filters; `last_run` + `artifact_summary` populated; per-automation env vars/secrets.
5. **Enterprise sandbox glue** (full design in §27): cloud credentials via OIDC (provider-agnostic), kubeconfig injection for the target cluster, Docker-in-sandbox, egress proxy, repo/environment/global secrets, OpenCode config storage + injection, toolchain in images (Playwright+Chromium, ripgrep, typescript-language-server).
6. **Files** (detailed design — see §28): uploads to object storage (S3-compatible) + `download_file` tool in sandbox; failed-upload UX signal.
7. **Recovery UX**: relaunch-and-resume (conversation id replay), resume-in-place on live sandbox, Slack/Linear retry buttons, warm-on-type (composer keystrokes pre-warm a sandbox; must not create orphan sessions).
8. **Models**: Anthropic + OpenAI/Codex (ChatGPT-account OAuth — native in the pinned OpenCode binary, no plugin and no new `AgentRuntime` adapter, but NOT env-var-shaped either; full design in §29) + Gemini (via OpenCode's own already-present `google`/`google-vertex` providers, no new `AgentRuntime` adapter — §25.2) + reasoning-effort plumbing (per-session and per-message overrides — §29.8).
9. **RWX previews**: PR preview links dispatched at latest PR commit (detailed design — adapter §4.1.1, preview-link mechanism §4.1.2).
10. **Slack/Linear fidelity**: mrkdwn contract both directions; Linear progressive AgentActivity updates; thread↔session mapping.
11. **Multiplayer**: participants, presence, per-user PR attribution (viewer ≠ reviewer), PR created with the *prompting user's* OAuth token (fallback: bot + manual PR URL).
12. **Identity & access** (new — see §13): GitHub sign-in + pluggable OIDC SSO; automatic cross-channel identity linking (Slack/Linear ↔ GitHub by verified email, in-channel link prompt on ambiguity); RBAC with four roles (admin/maintainer/member/viewer) enforced server-side and channel-agnostic; audit log.
13. **Product prototyping workflow** (new — see §14): path-scoped Environments enforced via sparse-checkout (not prompt discipline); a generalized multi-service boot manifest (`services.yml`) supervised natively by `sandbox-agent`; contract-driven mocking with drift detection; a handoff-readiness sentinel that flags backend-touching or uncontracted work for engineering pickup.
14. **Decision inbox** (new — see §16): a home view listing everything waiting on the signed-in user — auto-approved PRs ready to merge (assigned directly or via CODEOWNERS through the identity graph), reviews requested, plans awaiting approval, recoverable failures — with actions inline, server-side re-validation at click time, and decision latency as an analytics KPI.
15. **Sentinel auto-fix** (new — see §17): a coverage/doc-drift finding can spawn a child session that opens its own merge-gated follow-up PR, referenced from the original verdict and auto-merged only once the origin PR lands — a per-repo toggle, off by default.

## 9. Testing strategy

### 9.1 Unit
Domain packages at ~100% branch coverage — they are pure. Seed `domain/sandbox` with an exhaustive decision-function test corpus (spawn/restore/resume decisions, circuit breaker, timeout evaluation).

### 9.2 Contract
Round-trip tests for every `/contracts` schema: Go marshal → validate → unmarshal; TS codegen compiles against frontend usage; OpenCode adapter tested against the real pinned binary (§7).

### 9.3 Resilience (the differentiator — phase 2 exit criterion)
These run as automated scenarios against a real (or provider-faked) stack. Minimum set:
1. Kill the CP pod mid-turn → actor rehydrates, turn resumes or fails-with-reason; no stuck `processing`.
2. Kill the sandbox mid-turn → suspect → grace → respawn+resume with same conversation id.
3. Slow boot (inject 5-min delay in deps install) → boot_progress keeps session alive; no false kill.
4. `execution_complete` arrives AFTER terminalization → state reconciled, automation counters corrected. (Scoped to the turn still being `Processing`, §3.2 — a signal arriving past the turn's own, separate `turn_deadline` timeout is a different, known-residual case, §3.2, not exercised by this scenario.)
5. Two concurrent spawns (double-click / retry race) → single winner by gen fencing; loser sandbox reaped by GC.
6. Stale sandbox from old gen reconnects → rejected 403, logged, session unaffected.
7. WS drop during event stream → ack protocol redelivers the 6 critical events exactly once.
8. Provider API down during spawn → typed transient error, retry with backoff, circuit breaker only on permanent.
9. Outbox: Slack API 500s for 10 min → notification eventually delivered, no loss.
10. Concurrent @mentions on one PR → exactly one review session (atomic claim).
11. Dirty working tree at relaunch → stash → checkout session branch → pop; zero lost user edits.
12. Deploy rollout (rolling restart) → zero sessions marked failed.

### 9.4 Shadow mode (phases 3-4, and §30 for the platform-wide capability)
Intent classifier and code review run in shadow mode (log-only) on real traffic before activation; divergence report per decision. **Shadow mode is a permanent capability, not a one-time launch gate** (§18.5): activating a classifier or reviewer on a surface must never delete the shadow code path, its config, or its telemetry — the same mechanism is used again for every future model swap, prompt change, or new surface, not just the first activation. Skipping the shadow window on the reasoning that tests alone prove equivalence is not a default; it requires an explicit, documented exception.

Two different mechanisms carry the word "shadow", and only one of them exists before Phase 8. The classifier's shadow is **decision-level** (Step 36): the decision is computed and logged while the deterministic path still acts — it needs no egress control at all. Running code review — or any other lane — "in shadow" against a repository Narvi must leave no trace in is an **egress property of the whole platform**, delivered only by §30's suppression machinery (Phase 8, Steps 96-104). Until that phase ships, "review in shadow" can only honestly mean reviewing repositories where posting is acceptable and treating every verdict as untrusted until a human has checked it (the Phase 5 exit as corrected below) — never log-only operation against a customer repository.

## 10. Implementation phases

Each phase ends with a working, demoable increment. Do not start phase N+1 with phase N's exit criterion red.

**Phase 0 — Foundations (1-2 wks)**
Repo skeleton per §1; config loading + validation; `platform/timeouts.go` with invariant test; logging/OTel envelope; Postgres migrations for core tables (`sessions`, `turns`, `sandboxes` + history, `events`, `session_timers`, `outbox`, `participants`, `artifacts`); `/contracts` schemas for §6 + codegen (Go + TS); CI (lint, test, contract tests).
*Exit: `make dev` boots CP against local Postgres; contracts round-trip green.*

**Phase 1 — Core (3 wks)**
Session actor + timer pump + advisory locks + fencing; sandbox & turn state machines (port decisions + tests); Modal provider; minimal `sandbox-agent` (boot contract, git clone, credential helper, WS + ack protocol, heartbeats); OpenCode adapter (happy path); client WS hub with subscribe/replay; REST endpoints the frontend needs for one session.
*Exit: end-to-end via the API (or a minimal client) — create session → prompt → streamed events → agent pushes branch → PR created. Kill-pod test (§9.3 #1) green.*

**Phase 2 — Resilience (2 wks)**
Snapshots/restore/resume; image prebuilds (fingerprint = repo SHAs + runtime version; always fall back to base image on any miss — never block a session); rebuild scheduling with backoff; reconciler + orphan GC; two-phase terminalization + reconciliation; turn recovery + resume-in-place; git state machine complete.
*Exit: full §9.3 suite green.*

**Phase 3 — Ingress & routing (2 wks)**
GitHub/Slack/Linear/webhook ingress with shared toolkit (signature verify, atomic dedupe, one `CreateSessionRequest`); intent classifier (shadow first); plan mode end-to-end; outbox delivery to all notifiers; auth hardening (host-scoped cookies, backend-issued session validation).
*Exit: bot ingress demo; classifier shadow report on real traffic.*

**Phase 4 — Warm boot & agent-turn resilience (Steps 40-44; additive, does not gate Phase 3's exit above or block Phase 5's start)**
Shared, tip-tracking image prebuilds (§19): re-keyed fingerprint, fetch-aware `gitclone.SyncAll`, freshness pump, the `repo_image` setup-rerun contract amendment, hook-output capture, and the graduated rerun ladder (§19.6, ungated — §19.9 records why); plus the OpenCode adapter's context-overflow compaction retry (§7.2), fully independent of the warm-boot work.
*Exit: per-Environment warm-boot staleness window observed within the 10–40 min range §19.2 predicts; §9.3-class fetch-fail/stale-image/non-idempotent-setup scenarios green; compaction-retry contract test green against the pinned OpenCode binary.*

**Phase 5 — Code review & automations (2 wks)**
Full §8.2 code review; automations engine + sweeps; RWX provider + previews; uploads; secrets scopes; model catalog + Codex OAuth.
*Exit: code review exercised end-to-end on live PRs of repositories where posting is acceptable (Narvi's own), every verdict reviewed for precision before being trusted. An earlier wording of this line — "code review in shadow on live PRs" — promised a capability this plan did not yet contain: log-only operation against a repository Narvi must leave no trace in requires the platform egress-suppression machinery of §30 (Phase 8), not just §9.4's precision-review discipline. Shadow evaluation on customer repositories is Phase 8's exit, not this one's.*

**Phase 6 — Rollout (1-2 wks)**
Config setup (automations, secrets, environments, settings, integrations); cohort-based rollout of sessions; operational dashboards and runbooks; SLO alerts wired; a per-surface user guide (web/Slack/Linear/GitHub), each surface documenting what it accepts AND its own honest negatives — a CI check ties every documented command to the `/contracts` route or classifier routing record (§18.4) that actually implements it, and the guide documents only shipped behavior, never aspirational text — otherwise a hand-maintained prose guide is just a copy of that same behavior with no mechanism keeping it in sync, which is exactly what the CI check exists to close.
*Exit: platform serving production traffic under monitoring.*

**Phase 7 — Web UI (~3-4 wks; see §12)**
The SPA on the generated contracts, embedded in the control-plane binary.
*Exit: all nine views in §12.2 built to the mockups (including the decision-inbox home, §16) + the UX items (§12.3); screenshot-level review against the mockups; `make dist` produces the single self-contained binary.*

**Phase 8 — Platform shadow mode (Steps 96-104; see §30)**
The zero-trace evaluation capability: egress-mode flag + fail-toward-suppress resolver; the GitHub transport gate, port decorator, and suppression ledger; outbox classification (fail-closed at boot) + epoch stamps on outbox rows and verdicts; OS-level UID isolation between sandbox-agent and the agent runtime; GitHub App fine-grained read-only installation tokens; the shadow credential mint with its cache/snapshot hygiene; the synchronous-ingress seams + the `net/http`/`os/exec` arch-test; git mirror + lane coherence (carrying the §30.9 mirror decision); the operator ledger view with "Activate" as graduation. Appended numerically after Phase 7, but in execution order it is the bridge between Phase 5's exit and any customer-facing activation: plugging Narvi into a repository it must leave no trace in is gated on this phase, regardless of Phase 6/7 status.
*Exit: a dedicated evaluation deployment (`NARVI_SHADOW_MODE=1`, GitHub-only webhooks, credential-starved per §30.4) attached to a live customer repository completes real sessions end-to-end with zero customer-visible egress — reads only on the customer's audit surface — and the suppression ledger accounts for every would-have-been effect; per-repo Activate graduates a repo to live with §30.8's promotion fence applied.*

**Phase 9 — Per-repository knowledge, two modes (Steps 105-110; see §31)**
The two-mode knowledge capability: approved-plan durability; the per-repository entitlement predicate + `sessions.repos` authorization; the mode A prior-arch-decisions block with its path-scoped selector, mode buffer, injected-ids record, and merge-outcome capture; the mode B index and hybrid retrieval (`Embeddings` port, `real[]` + `tsvector` schema, `RepoScope` isolation layers, RRF fusion, cold-corpus fallback) with its quarantine/provenance/self-reinforcement guards; `kb_search`; the OKF read-only export. Appended numerically after Phase 8; in execution order it needs only Phase 5's milestone (the review chain and its §26.5 instrument) plus Phase 8's Steps 96/98 for the epoch stamps its in-query exclusions ride — Step 108's engagement additionally waits on Step 107's own baseline readout (§31.6).
*Exit: mode A's block live on all three review seams with the contestation-×-injection KPI reporting per mode stamp, plus EITHER a repository flipped to mode B serving retrieved context with the cold-corpus fallback verified and the two-repository isolation suite green OR a recorded kill decision (§31.9) citing Step 107's own baseline readout in place of Step 108's build; contested and shadow-epoch content demonstrably excluded from live retrieval in the SQL whenever mode B exists at all.*

## 11. Working conventions for the implementing agent

- Never put I/O, `time.Now()`, or randomness in `/internal/domain` — inject `Clock`/`IDGen`.
- Every state transition goes through the machine's `Transition(from, trigger) (to, error)` table — no ad-hoc status writes, enforced by making status columns writable only via the store methods that take a transition proof.
- Every new timeout/interval goes in `platform/timeouts.go` — grep-test in CI forbids `time.Second * N` literals outside `platform` and tests.
- Table-driven tests; race detector always on (`go test -race`); `errgroup` + context for all concurrency; no naked goroutines (lint rule).
- When behavior is ambiguous, resolve it against the mockups and the §6 contracts, and keep the domain paths single: repos are always a list, tokens are always hashed, one status taxonomy.
- Commit per coherent unit with tests; keep `main` green.

## 12. Web UI (phase 7)

Design mockups of the nine views exist (decision inbox/home, session workspace, code review, release review, plan mode, automations, settings, analytics, sign-in) and are the visual spec; ask the requester for the artifact if not provided. The mockups do not necessarily cover every individual screen or state phase 7 will need (an empty state, an error state, a secondary modal not explicitly drawn) — any such screen must be derived from the same visual design system the mockups establish (tokens, typography, layout, component patterns), never designed independently of it; §11's own resolve-ambiguity-against-the-mockups rule extends to this.

### 12.1 Architecture
- **SPA, no SSR, no BFF.** Vite + React + TanStack Query/Router. Static build embedded in the control-plane binary via `go:embed`; `narvi serve` serves API + WS + UI on one port. Self-host story: one binary + Postgres.
- **Data layer generated from `/contracts`**: TS types + typed API client + WS event handlers are codegen outputs; no hand-written response types anywhere. Client pattern: WS transport → event log → reducer → query invalidation.
- **Auth pluggable**: generic OIDC (GitHub/Google/SSO as configuration), session tokens issued by the Go backend; no NextAuth. Org-specific enrichments (e.g. company URL context) live behind an extension point, not in core.
- **Theming**: light/dark via CSS custom properties, `prefers-color-scheme` + explicit toggle.

### 12.2 View inventory
1. **Session workspace**: sidebar with status taxonomy chips (running / booting n/m with progress / completed / cancelled / failed·reason), session-source icons (web/Slack/Linear/GitHub), and a creator filter with explicit labels ("My sessions" = created or joined / "All sessions"; no "Team" option — the domain has roles and identities but no team entity), typed-event timeline (collapsed tool calls with durations, per-step cost, streaming text; sub-task fan-out, §7.1, as collapsed labeled sub-lanes nested under the spawning tool call, never interleaved, with a live count while active and a distinct color/icon for a failed or cancelled sub-lane vs. completed), failure cards with persisted reason + one-click Resume (conversation replay), composer (model / effort / plan-mode toggle, warm-on-type indicator), right rail (sandbox panel: status, gen, last-seen, runtime fingerprint, correlation id, timestamped state transitions; boot phases with durations; artifacts: PR / preview / uploads; cost breakdown, inclusive of sub-task spend, §7.1). Multiplayer presence.
2. **Code review**: risk-map verdict table (area × severity × assessment) editable by maintainers, "posted via server-side verdict tool" indicator, per-sentinel status (coverage / doc-drift / visual-QA: pass/fail/skipped), finding cards (severity, file:line, failure scenario, Apply-suggestion via validated endpoint, Dismiss-with-rebuttal feeding re-review reconciliation, auto-fix PR link + merge-gated status when a coverage/doc-drift finding triggered one, §17), review history, coalesced-mention + session-reuse info, re-run action.
3. **Plan mode**: persistent versioned plan document (numbered steps with file refs, scope estimate, v1→v2 history), approval bar (Approve & build / Request changes / Reject), cross-channel awaiting indicator (web/Slack/Linear, first verdict wins), plan-model vs build-model split visible in header.
4. **Automations**: table with health column (success ratio, strike counter "n/3 before auto-pause", auto-paused chip + Resume), expandable invocation → runs rows (per-target status, typed failure reason, artifact_summary one-liner, link to session), triggers/targets/next-run, my-automations/status filters.
5. **Settings**: sectioned nav (general, environments, members & access, secrets, integrations, models, prompt templates, image builds). Environments: ordered repo cards with prebuilt-image status (fingerprint, build duration, drift-check countdown, failed-rebuild state showing backoff + base-image fallback + reason), and a sentinel auto-fix toggle (per repo, off by default, §17). Members & access (§13): role management per member, linked-identity chips (github/slack/linear with pending + resend), audit-log view. Secrets: table with scope chips and per-target resolution display (order: automation → environment → repo → global, "this value wins"). Prompt templates: versioned classifier templates, editable, assembled-prompt preview, shadow-mode divergence metric + Activate.
6. **Analytics**: my-sessions/all + date-range filters; KPI tiles (sessions, success rate, **false failures** = watchdog kills later proven alive with target 0, cost + median per session, boot p95 with sparkline); sessions-per-day stacked by outcome (status colors, legend + hover, 2px spacers); cost-by-model horizontal bars; Review finding outcomes (accepted/rebutted/dismissed — fed by §21's read model; precision computed only over definitively-resolved findings, dismiss-rate reported separately); top typed failure reasons linked to sessions.
7. **Sign-in** (§13.1): "Continue with GitHub" primary + "Continue with SSO (OIDC)" secondary; identity auto-link status panel (github/slack verified, linear pending with in-channel link); allowlist note; mention that the GitHub token also drives PR attribution.
8. **Decision inbox — the home view** (§16): sectioned queue (Ready to merge / Needs your review / Awaiting your approval / Needs attention) with per-row inline actions (Merge, Open review, Approve & build, Assign to engineering, Resume), assignment provenance printed on every row, age with stale highlighting, repo filter only (the inbox is inherently scoped to the signed-in user — a "Mine" filter there would be redundant), median time-to-decision in the toolbar. Sessions list becomes the second tab.
9. **Release review** (§15): manifest table of constituent PRs (review state, CI at merge SHA, flags for admin overrides / red-at-merge / unreviewed reverts), aggregate-diff trigger banner showing why the conditional pass fired, composition findings with Block release / Acknowledge & ship actions.

### 12.3 UX items to land with the UI
Boot progress phases instead of spinner; failure reason + resume everywhere (matching the Slack/Linear retry affordance); distinct cancelled/failed/timeout chips; sandbox "what happened" panel (transitions + fingerprint + correlation id).

**Composer send semantics (item 1's composer, Step 83, decision 5) — acceptance criteria, day one:** Enter sends and Shift+Enter inserts a newline, from the very first ship of this composer, never added later — inverting this after users have built muscle memory around one behavior is a change users route around, not adopt, so it is not a follow-up. An IME composition guard is required: confirming an in-progress IME composition (e.g. selecting a candidate while typing Japanese/Chinese/Korean, which itself uses Enter) must never itself send — the guard checks the browser's own composition state, not a heuristic over the text. Exactly ONE shared can-submit predicate drives both the Send button's disabled state and the keydown handler's own send-or-not decision — never two independently-maintained checks, since a button and a key handler silently drifting apart on when submission is allowed is the classic defect this class of UI produces. Touch/mobile gets an explicit decision rather than an unstated gap: out of scope for this ship — the mockups' existing breakpoints (`docs/design/mockups.html`, three `@media (max-width:980px)` rules collapsing `.app`/`.sidebar`/`.rail`, `.settings`/`.setnav`, and `.charts2` to single-column layouts) reflow for narrower viewports but define no touch-specific interaction anywhere, and the composer itself carries no rule inside any of them; a touch-appropriate composer affordance (mobile virtual keyboards make Shift+Enter awkward to reach) is deferred, named here rather than silently left unspecified.

### 12.4 Sequencing & exit
Built in phase 7. Definition of done: all nine views built to the mockups + §12.3 items; screenshot-level review against the mockups; `make dist` produces the single self-contained binary.

**What the screenshot review actually found, recorded rather than rounded up.** All nine views are
built and all §12.3 items shipped, including the composer's own acceptance criteria (one shared
can-submit predicate driving both the button and the key handler, and an IME guard reading the
browser's composition state). `make dist` produces the binary, and it was proved self-contained by
running it from a directory where no copy of the SPA existed on disk.

But **the first clause of this definition cannot be met as written**, and pretending otherwise would
be the exact defect this codebase spends most of its effort on. §12.2's inventory was written as a
description of the finished product, and a substantial part of it names data that no backend
component computes: there is no platform-wide analytics rollup, no composition-review pass, no
structured plan schema, no persisted sandbox fingerprint, no automation run-health aggregate, and no
wire field for four of the review readout's own listed contents. A screen cannot be "built to the
mockup" when the mockup draws a number the system does not have.

Every one of those is filed as a named gap rather than faked, and **every affected screen says on
screen what it cannot show** — "not available yet", "not tracked here", "not reported yet" — which
is the behaviour this project requires and the reason those Steps shipped correctly without them.

Two corrections to §12.2 itself, which the review surfaced. Item 2's "risk-map verdict table (area ×
severity × assessment) editable by maintainers" is **superseded by §26.1**, which deliberately
replaces that table with a header risk badge plus prose digest sections; the shipped screen matches
§26.1, so this is a stale inventory line, not a gap — though §26.1 does not restore the
"editable by maintainers" capability, and no edit path exists. Item 7's SSO/OIDC secondary is built
but unconfigurable by design, which is a deliberate posture rather than an omission.

The honest reading of this phase's exit: the UI is complete against what the system can actually
answer, and §12.2 remains the record of what it should eventually answer.

### 12.5 Integrations read model & routes (amendment)

The integrations screen in §12.2's inventory needs to show, per ingress surface (Slack, Linear,
GitHub): whether it is connected, and evidence of its last delivery. Neither is reachable today.
`authz.ActionManageIntegrations` exists and is real — but its only consumer is
`adapters/inbound/linear`, gating an ingress path, not an HTTP handler a UI could call. There is no
`/api/integrations` route and no read model behind one, so this is specified here rather than
improvised by whoever builds the screen.

**A derived read, not a new table.** Every fact the screen needs already exists: whether a surface
is configured comes from `platform.Config` (each ingress has its own required secrets, and a
partially-configured surface must read as NOT connected rather than as connected-and-broken);
per-surface delivery evidence comes from the existing `webhook_deliveries` and outbox rows. Nothing
new is persisted, which also means there is no connect/disconnect *write* here — a surface is
connected by deploying its configuration, the same posture §27.3's cloud-identity capability
already takes.

| Route | Action | Notes |
|---|---|---|
| `GET /api/integrations` | `ActionManageIntegrations` (admin) | one row per surface: configured, last inbound delivery, last outbound delivery outcome |

**Inbound and outbound are different tables and different facts, and an earlier
draft of this section conflated them.** `webhook_deliveries` is a
deduplication ledger — `(provider, delivery_id, received_at)` and nothing else —
so it answers "when did we last hear from this surface" and carries no outcome
at all. Outcomes live on `outbox`, which is the other direction entirely: what
Narvi last tried to POST to that surface, with its status, attempt count and
last error. A row therefore carries two independent timestamps, and must label
them as what they are; collapsing them into one "last activity" would make a
surface that receives fine but cannot post look healthy.

**Mapping an outbox row to a surface is a naming convention, not a constraint.**
`outbox.kind` is free text following `<provider>_<what>` (`slack_digest`,
`linear_progress`, `github_verdict`), so the provider comes from a prefix match.
Nothing enforces that convention, and a future kind that breaks it drops
silently out of this read rather than failing loudly. Say so where the mapping
is implemented, and prefer a shared, tested prefix helper over a match inlined
at the query.

**`configured` cannot currently read false, and the section should have said so.**
Every value each surface needs is required at boot: `platform.Load` appends a
`MissingRequiredEnvError` and the process refuses to start. So a running
deployment has all three surfaces configured by construction, and this field is
structurally always true. It is kept rather than dropped because it is the
honest shape for the question a reader asks, and because the day a surface
becomes optional it is where that shows — but nothing may present it as a live
check, and the screen must not imply it is one. That "a deployment must
configure all three ingress surfaces to boot at all" is itself a constraint
worth revisiting, and is filed as a named gap rather than changed here.

**Never the secrets themselves, not even shaped.** The response says *whether* a surface is
configured and nothing about what configures it — no token prefix, no length, no masked form. This
is the same rule `SandboxSecret`/`ProviderCredential` follow by carrying no `value` field at all,
and it is why "connected" is a boolean rather than anything richer.

**Last-delivery evidence is a fact with a timestamp, never a health verdict.** The screen may state
when the last delivery arrived and whether it succeeded. It must not derive "healthy"/"degraded"
from that: a quiet Slack workspace and a broken one look identical from here, and a green badge on
a surface nobody has exercised is exactly the claim this codebase's own conventions forbid.

## 13. Identity, authentication & RBAC

### 13.1 Authentication
- **GitHub OAuth is the primary login** and serves double duty: the stored user OAuth token is what attributes PRs to the real author (§8.11). Generic **OIDC** is the secondary provider for SSO (Google/enterprise IdP) — configuration, not code.
- Sessions: **backend-issued, host-scoped cookies** (HttpOnly, SameSite=Lax; never a default cookie name on a shared parent domain — a colliding cookie from a sibling app on the parent domain is a classic random-logout cause). Token/refresh handling lives in the Go control plane; the SPA holds no provider tokens.
- Signup gate: allowlist of email domains / GitHub orgs / explicit users, evaluated at first sign-in; default role assigned from config (e.g. domain match → member).
- Provider tokens encrypted at rest (AES-GCM), per-user.

### 13.2 Identity graph & auto-linking
One person = one `user` across web, GitHub, Slack, Linear.

Schema:
```
users(id, primary_email, display_name, role, disabled, created_at)
identities(user_id, provider ENUM(github,slack,linear,google), external_id,
           email, email_verified, linked_via ENUM(auto_email, prompt, admin),
           created_at, UNIQUE(provider, external_id))
identity_link_prompts(provider, external_id, nonce, expires_at)  -- pending links
```

Auto-link algorithm (runs on first event from an unknown provider identity — Slack mention, Linear webhook):
1. Fetch the actor's profile email from the provider API.
2. Match against `users.primary_email` and verified identity emails.
3. Exactly **one** verified match → auto-link (`linked_via=auto_email`), notify the user in-channel and in-app.
4. Zero or multiple matches → **never guess**: create a link prompt and reply in-channel with a short-lived magic link ("connect your account"). A state-changing action from this not-yet-linked identity is **denied** (audit-fix batch "block unlinked actor state changes" — hardened from an earlier version of this section, which let the action proceed under bot attribution while the prompt was pending; that was a confirmed audit finding, not a design that survived review) — the magic-link prompt is still sent exactly as before, and the actor simply retries the identical action once they've clicked it and linked.
5. Manual link/unlink in Settings → Members; admin can force-link.

Failure rules: a provider email-API failure is a **retryable error, not an empty identity** — retry with backoff and keep the last known value; never null-out an email on transient failure (nulling it breaks both sign-in and identity linking).

Every cross-channel action (prompt from Slack, plan approval from Linear, review re-trigger from a GitHub comment) resolves to a `user_id` before it reaches the domain; a not-yet-linked actor's state-changing action is denied (never bot attribution) while a link prompt is sent in parallel, so the same action succeeds on retry once linked. GitHub is denied the same way (batch fix/deny-unlinked-github-actors — a repo-owner-decided hardening that closed a confirmed, MEDIUM-severity authorization gap: an unlinked GitHub commenter was bypassing role gates a linked-but-restricted user could not), even though a GitHub commenter resolves directly from an existing GitHub-OAuth-login identity with no deferred "auto-link pending" mechanism at all — an unresolved GitHub commenter has simply never logged into Narvi via GitHub OAuth, a permanent case with nothing to retry, unlike Slack/Linear's own transient, self-resolving pending-link state. `AuthorizeLinkedActor`, not `AuthorizeResolvedActor`, now governs GitHub's own create/prompt gates too (`internal/adapters/inbound/github/coalesce.go`); since GitHub has no magic-link prompt to send in parallel, its own ingress instead posts a one-time, honest reply pointing the commenter at the ordinary GitHub OAuth sign-in flow (`internal/adapters/inbound/github/actornotauthorizedreply.go`), deduped per `(repo, PR, commenter)` so a repeat mention doesn't spam the same reply.

### 13.3 RBAC
Roles (global, one per user): **admin > maintainer > member > viewer**.

| Permission | admin | maintainer | member | viewer |
|---|---|---|---|---|
| View sessions / analytics | ✓ | ✓ | ✓ | ✓ (read-only) |
| Create sessions, prompt, approve plans on own/joined sessions | ✓ | ✓ | ✓ | — |
| Stop/resume any session; approve any plan | ✓ | ✓ | — | — |
| Manage automations, environments, repo/env secrets | ✓ | ✓ | — | — |
| Edit review verdicts; re-trigger reviews; auto-approval eligibility config (§21) | ✓ | ✓ | — | — |
| Integrations, global secrets, prompt-template activation, members & roles, per-repo auto-merge toggle (§21), sentinel auto-fix toggle (§17 — stricter than auto-merge since it ends in an unattended merge with no per-repo arming step, not a human Merge click), per-repo automatic re-review opt-in toggle (§24 — off by default, same admin-only row as the other automation-enabling toggles here) | ✓ | — | — | — |

Enforcement — **server-side only, channel-agnostic**:
- `domain/authz`: a table-driven `Authorize(actor, action, resource) error` — the matrix above lives in the domain as data with exhaustive tests. Every state-changing actor command (session actor mailbox, plan approval, verdict edit, automation toggle) calls it, so a Slack approval passes exactly the same check as a web one.
- HTTP middleware handles the coarse route-level gate; WS `subscribe` applies visibility rules. The UI hides what the role can't do, but the server is the authority.
- **Viewer guard**: viewers never gain PR-reviewer attribution or git identity on session artifacts.
- **Audit log**: `audit_log(actor_user_id, action, resource_type, resource_id, detail_json, correlation_id, created_at)` written in the same transaction as the change; surfaced in Settings → Members ("Audit log").

### 13.4 Phasing
- **Phase 1**: GitHub OAuth + cookie sessions + `users` table + role skeleton (admin/member) + route middleware — needed before the first end-to-end session.
- **Phase 3** (with bot ingress): identity auto-linking + link prompts, full four-role matrix, channel-agnostic Authorize on plan/review actions, audit log.
- **Phase 7** (UI): sign-in page, Settings → Members & access (role management, linked-identity chips with pending/resend, audit log view) — mocked in the design artifact.
- First-run seeding: any imported participants map to `users` by GitHub id; everyone defaults to `member`, initial admins set by config.

## 14. Product prototyping workflow (new capability)

Problem this solves: product/PM sessions are almost exclusively frontend work, but an unscoped agent will happily wander into backend files — that work is then thrown away and redone by an engineer, burning tokens and review time for nothing. The fix is **prevention, not correction**: make backend code physically absent from the sandbox rather than relying on the agent (or a prompt) to behave.

### 14.1 Scoped Environments
- Extend the Environment record (referenced from session creation and automation targets, §3.5) with an optional `path_scope: []glob` (absent = full access, unchanged behavior) and an optional `mock_config` (§14.3).
- **Enforcement is at the git layer, not a policy/prompt layer.** `domain/gitstate`'s clone step (§3.4) runs `git sparse-checkout set <globs>` per repo when `path_scope` is present. Excluded paths never materialize on the sandbox filesystem — this cannot be bypassed by prompt injection, agent "helpfulness," or a gap in OpenCode's own permission model, because there is nothing there to edit.
- `path_scope` MUST always include whatever shared contract directories the Environment's declared services reference (§14.2-§14.3), resolved explicitly by the Environment config — never inferred by the agent.
- Sessions created under a scoped Environment carry a provenance tag (alongside the existing `spawn_source`) so the label automation and the handoff sentinel (§14.4) can act on it without re-deriving intent.

### 14.2 Multi-service boot manifest (generalizes §6.4)
Today's boot contract is one `setup.sh` (bake-time) + one `start.sh` (one service, primary-fatal/secondary-warn) per repo. A prototyping Environment typically needs two services at once (frontend dev server + mock API), and pushing that onto a single shell script re-creates exactly the kind of ad-hoc multi-process supervision (backgrounding, signal traps, PID tracking) that Narvi eliminates elsewhere (§7, Step 12) — replicating it into repo-owned bash would just move the bug class, not remove it.

- New optional `.narvi/services.yml` per repo:
  ```yaml
  services:
    - name: web
      cmd: pnpm dev
      cwd: apps/web
      readiness: { port: 3000 }
      criticality: primary
    - name: mock-api
      cmd: prism mock contracts/api/openapi.yaml -p 4000
      readiness: { port: 4000 }
      criticality: primary   # in a prototyping Environment the mock IS the backend
  ```
- `sandbox-agent` supervises every declared service with the **same** process-group/reap/drain machinery already built for OpenCode/code-server/ttyd — no new supervision code path, just more entries in the same table.
- Each service's `readiness` (port poll or HTTP check) becomes a **named `boot_progress` phase** over the existing event (§6.1) — this is what lets the UI show granular boot phases (mockup decision #2) instead of one spinner, for free.
- `criticality: primary | secondary` carries the same fatal/warn semantics `start.sh` already has today — just per-service instead of per-repo.
- **Backward compatible, no forced migration**: if `services.yml` is absent, `sandbox-agent` falls back to the current `setup.sh`/`start.sh` contract unchanged (§6.4).
- `setup.sh` semantics don't change: genuinely expensive steps stay baked into the image at build time (§8.5-note). A mock server regenerated from a static spec file is cheap enough (sub-second) to run live at every boot instead of being baked — staying in sync with `contracts/api/*` at HEAD matters more than the negligible regen cost.

### 14.3 Mocking strategy
The mock is a **versioned repo artifact**, authored once and reviewed like code — never something an agent invents per session (that would just reproduce the waste this feature exists to prevent). Two supported sources, no bespoke platform code needed for either:
- **Contract-driven**: a shared `contracts/api/*.{yaml,json}` spec (the same convention as the platform's own `/contracts`, §6) drives a generated mock server (e.g. Prism), declared as a `services.yml` entry.
- **MSW reuse**: if the frontend already ships Mock Service Worker handlers for its own tests, a prototyping Environment just flips an env var to route through them — zero new infrastructure.

**Drift detection**: extend the image-build fingerprint/staleness mechanism (§8.5-note, same PR scope as image builds) to also fingerprint `contracts/api/*`. If a real backend endpoint changes without the contract being updated, this doesn't block anything — it feeds the handoff sentinel (§14.4).

### 14.4 Handoff to engineering
On PR creation from a scoped session (§14.1), `domain/review` (§8.2) runs a **handoff-readiness sentinel** alongside or instead of a normal risk verdict: it reports which endpoints the prototype calls that have no entry in `contracts/api/*`, and any backend-adjacent TODOs the agent left behind.

- **v1 (ship first, cheap)**: auto-apply a `handoff` label + post the sentinel's summary as a PR comment; optionally open a linked Linear/GitHub issue assigned to an engineering queue.
- **v2 (only if handoff volume justifies it)**: a "Send to engineering" action spawns a **child session** (existing mechanism — `parent_session_id`/`spawn_depth`, max depth 2) in a full-access Environment, pre-loaded with the prototype diff + sentinel summary, started directly in **plan mode** (§8.1) so an engineer approves an implementation plan instead of starting from a blank prompt.

Both reuse existing primitives (review sentinels, labels, child sessions, plan mode) — no new subsystem either way.

### 14.5 Phasing
- `path_scope` + sparse-checkout enforcement: extends `domain/gitstate` and the Environment/session-creation data model — Phase 1-2 (alongside Step 9/26).
- `services.yml` supervision: extends `sandbox-agent` process supervision and `boot_progress` reporting — Phase 1-2 (alongside Step 12).
- Mock contract drift-check: extends the image-build fingerprint work — Phase 2 (alongside Step 24).
- Handoff sentinel v1: extends the review sentinels — Phase 5 (alongside Step 45).
- Handoff v2 (child-session escalation): optional; add later in Phase 5 or beyond if the volume of v1 handoffs justifies the extra complexity.
- UI (Phase 7): Settings → Environments gains a path-scope + services editor; sessions can be filtered/labeled by prototyping provenance; the handoff sentinel surfaces inside the code-review view (§12.2 item 2).

## 15. Release PR review (new capability)

Problem this solves: reviewing a release PR (one that bundles many already-individually-reviewed PRs — a release-branch cut, a `develop→main` promotion) is a different job from reviewing a feature/fix PR. Line-by-line correctness was already checked per constituent PR; what's missing is (a) verifying the safety net actually held for every one of them, and (b) catching composition bugs that only emerge from PRs interacting — which no single PR's review can see. Two concrete failure classes make this non-hypothetical: a migration-numbering collision across sequential PRs (each diff clean; the conflict is only visible in the merged tree) and an endpoint rename silently regressed by an unrelated merge — neither visible in any one PR's diff.

### 15.1 Detecting a release PR
A PR is treated as a release review when it matches a configurable pattern: originates from/targets a `release/*` branch, or carries a `release` label (manually applied, or auto-applied by an automation trigger on branch-name pattern, §8.4). Detection reuses the existing intent-classification seam (§8.3) — release-vs-feature is just one more category alongside review-vs-request and plan-vs-build, not a separate classifier.

### 15.2 Manifest check (always runs)
Extends `domain/review` (§8.2) with a `ReleaseManifestCheck`, distinct from the per-PR risk-map verdict:
- `SourceControl` (§4.3) gains `ListMergedBetween(ctx, baseRef, headRef) ([]MergedPR, error)`. Each `MergedPR` carries: PR number/title, approving reviews, CI conclusion **at the merge SHA** (not the latest SHA — a force-push after approval can hide a run that was red when it actually merged), whether it merged via an admin/policy override, and whether it was later reverted (and whether that revert was itself reviewed).
- Findings are an audit, not a risk verdict — e.g. "PR #142 merged without an approving review (admin override)", "PR #156 was red at its merge SHA", "PR #160 was reverted 2h after merge; the revert itself was unreviewed."
- Fully mechanizable: no code-reasoning required, this is a compliance check, not a code review. Posted through the same server-side verdict-posting tool as any review finding (§5.2) — never a raw comment.

### 15.3 Aggregate diff review (conditional)
A pure decision function (same style as the domain decision functions, §3.2/§9.1) — `ShouldRunAggregateReview(manifest) bool` — fires when ANY of:
- ≥3 constituent PRs touch overlapping path prefixes (same subsystem).
- Any constituent PR was flagged high-risk/critical by the team's own PR-tiering.
- Any constituent PR's merge required manually resolving a conflict.

When triggered, run one review pass over the full diff `baseRef..headRef` — not per constituent PR — with a prompt **distinct from the standard risk-map verdict**: explicitly framed around composition ("do these already-individually-correct changes conflict, duplicate, or invalidate each other's assumptions"), never re-litigating logic already approved per PR. Reuses the same LLM/review pipeline (§4.3, §8.2) with a separate, versioned prompt template (same mechanism as §8.3/§12.2 item 5).

### 15.4 Premise/shippable enrichment: a later extension, not now
Neither the manifest check (§15.2) nor the aggregate diff review (§15.3) computes or consumes the per-PR `Shippable`/`PremiseState` structured verdict (§8.2/Step 45, §21) — both stay exactly the mechanical/compositional passes specified above, with no release-level premise or shippable score. Enriching either pass with that structured type later — e.g., rolling the constituent PRs' `Shippable` states into an aggregate read on the release cut itself — is a possible later extension if experience with the manifest/aggregate-diff passes shows it's needed. It is explicitly not part of this design and not scheduled.

### 15.5 Phasing
Extends the code-review domain and review-session reuse (§8.2, Step 46) plus the intent classifier (§8.3, Step 36) — Phase 5, alongside the rest of the sentinel family (Step 45/46/48/49). No new domain package, no new state machine. UI: a dedicated release-review screen (§12.2 item 9, mocked in the design artifact) — manifest table + trigger banner + composition findings.

## 16. Decision inbox (home view — new capability)

Problem this solves: the session-centric UI answers "what are the agents doing?" — an observation surface. But in an agent-heavy workflow the humans are the serial bottleneck: merges of auto-approved PRs, reviews, plan approvals, recoveries all wait on a person, scattered across GitHub notifications, Slack threads, and the sessions list. The home screen becomes a **queue of pending decisions addressed to the signed-in user**, each row carrying its action inline. Sessions remain one click away for watching execution; watching is no longer the default job.

### 16.1 Item taxonomy
Each row is a pending human decision — with one narrow, admin-toggled exception (sentinel auto-fix follow-up PRs, §17, disabled by default) that merges without appearing here at all, precisely because there is no decision left for a human to make once its own checks pass. Otherwise, every row is one of:
- **ready_to_merge**: open PR authored by a platform session, auto-approved by the deterministic eligibility engine (§21 — CI green, no floor raised, diff size under a configurable threshold, no sensitive path touched; a `review: needs-human` label forces a PR out of auto-approval regardless of criteria; `visual-qa: pass/skip` unaffected), CI green at head, and assigned to the user — directly, as requested reviewer, or via CODEOWNERS. Action: Merge (1-click confirm while a repo's auto-merge toggle, §21, is unarmed; once armed, these merge without ever appearing here).
- **needs_review**: PRs where the user is requested reviewer/code owner and the verdict is ≥ medium or a formal review is gated; includes release cuts with manifest flags (§15).
- **awaiting_approval**: plan-mode plans the user is entitled to approve (per `Authorize`, §13.3) and handoff items (§14.4) sitting in the engineering queue.
- **needs_attention**: sessions failed-with-resume-available, auto-paused automations, dead-lettered outbox deliveries (admin only).

Ranking: by decision cost then age — quick confirmations (ready_to_merge) first; per-row age shown, stale items (>48h, configurable) visually flagged. Every row prints its **assignment provenance** ("yours via CODEOWNERS · internal/app/scheduler/**" vs "assigned directly" vs "requested reviewer") — a queue whose origin the user can't trust becomes a feed they ignore.

### 16.2 Data & enforcement
- **A read model, not new state**: the inbox aggregates existing Postgres state (plans, review sessions, sessions, automations, outbox) plus SCM data. No new state machine, no new writer.
- `SourceControl` (§4.3) gains `ListOpenPRsForUser(ctx, user) ([]OpenPR, error)` (review state, CI at head SHA, labels, assignees/reviewers) and `ResolveCodeOwners(ctx, repo, paths) ([]Owner, error)`. CODEOWNERS teams resolve to persons through the identity graph (§13.2). SCM data is cached with a short TTL and the staleness is displayed ("as of 2 min ago") — never presented as live truth.
- **Actions re-validate server-side at click time**: the Merge endpoint re-checks CI status, approval state, and `Authorize(actor, merge, pr)` before calling the SCM — the rendered queue is never trusted as authority (same policy-on-the-server invariant as verdicts, §5.2). Viewer role sees the queue read-only.
- Metric: **decision latency** (median time from item entering the queue to its action) joins the analytics KPIs (§12.2 item 6) — the human bottleneck, made visible.

### 16.3 Phasing
- **Phase 5**: read model + endpoints (it aggregates code review, plans, automations — they must exist first); `SourceControl` extensions.
- **Phase 7**: the inbox is the **home view** of the new UI (mocked in the design artifact, decisions 32-34); sessions list moves to the second tab.

## 17. Sentinel auto-fix (new capability)

Problem this solves: coverage and doc-drift sentinel findings (§8.2) are almost always mechanical to fix — add the missing test, update the stale doc — yet today a sentinel finding sits as a review comment a human must action manually, adding a full extra round-trip for exactly the kind of low-risk change that shouldn't need one.

### 17.1 Trigger and scope
Fires when a review verdict (§8.2) contains a finding from the test-coverage sentinel and/or the doc-drift sentinel — no other sentinel or finding type triggers this (in particular, the handoff-readiness sentinel, §14.4, is unrelated and unaffected). **Disabled by default**; a per-repo toggle enables it (Settings → Environments, §12.2 item 5), **admin-only** (§13.3) — a stricter gate than the criteria-driven auto-approval config (§21), which still defaults to a human Merge click and only merges unattended once an admin arms that repo's own auto-merge toggle after a calibration period, whereas this one has no such default and no calibration gate at all; the risk delta justifies the stricter row. **No recursion**: a PR opened by a sentinel-auto-fix child session is never itself eligible to trigger another sentinel auto-fix, regardless of what its own review verdict finds — this is an explicit rule, not a depth-counter side effect.

### 17.2 Fix session
On trigger, the review session spawns a child session (existing mechanism, §14.4 v2 — `parent_session_id`/`spawn_depth`) in the origin PR's own environment (full access — sentinel fixes touch test/doc files, never a scoped prototyping environment), pre-loaded with the origin diff and the specific sentinel finding(s), started directly in build mode (no plan-mode gate: this is mechanical remediation, not a design decision, and the safety net is downstream, §17.4). It pushes a branch and opens a PR **against the origin PR's own branch, not `main`** — a stacked PR, since the fix has to apply on top of the code it's fixing, and the origin PR hasn't merged yet when this happens. The child session's write/edit tool capability is additionally restricted server-side to test/doc path patterns — a capability restriction enforced at the `AgentRuntime`/sandbox-session level (§4.2, §7), distinct from §14.1's `path_scope` (which restricts what's physically on disk via git, not what a present file's own tool calls may target) — defense in depth alongside §17.4's post-hoc diff-scope check, never a prompt-only instruction to "only touch tests and docs."

**Registering the pair as a real GitHub stack (amendment — GitHub has since made stacking a first-class server-side object; registering the pair makes the relationship above legible to it).** Once the fix PR exists, register it together with the origin PR as a stack: `POST /repos/{owner}/{repo}/stacks` with `pull_requests: [originPR, fixPR]` (bottom to top; two members, well inside the endpoint's own 100-PR ceiling). This is necessarily a **second** call, made only after both pull requests already exist — the endpoint groups pre-existing pull requests into a stack, it does not create them, and the origin PR was opened by its own independent flow (Step 21, `pushpr.go`) before this feature's trigger (§17.1) ever fires. A `404` (§17.6 discusses what that means for a given repository) or any other failure from this call is logged and otherwise ignored — it never fails the fix-session flow, and both pull requests stay open and correctly based on each other regardless of whether this call succeeds.

Registering the pair is **not**, however, a cosmetic layer on top of an otherwise-unaffected merge path — GitHub's own documentation is explicit that it is not: "Merging a stacked pull request requires the Stacks API. The legacy pull request merge endpoints can't merge a stack" (docs.github.com/en/pull-requests/tutorials/roll-out-stacked-prs, fetched 2026-07-31). Once this call succeeds, the origin and fix PRs are real stack members, and per that same constraint, merging either of them now requires the Stacks API, not the legacy endpoint. §17.4's system-initiated merge (below) and §21.2's auto-merge both reuse the plan's one existing, generic merge call path unchanged, and neither is Stacks-API-aware. **This is a gap this amendment introduces and does not close — an open item for whoever implements Step 48:** either give §17.4/§21.2's merge call Stacks-API awareness for a PR carrying stack context, or hold this registration call until they do; registering the pair here and then merging through the unmodified legacy path would, per GitHub's own stated constraint, simply fail.

**The fix PR's base is never resolved, only assigned (amendment).** `resolvePRBaseBranch` (`internal/app/sessionactor/pushpr.go`) resolves a repository's real current default branch for the ordinary happy-path PR (Step 21) — called unconditionally, for every repo that reaches PR creation, to compute `CreatePRSpec.Base` (`createPRBestEffort`'s per-repo loop calls it for each pushed repo, with no branch-based condition at all). `repos[].branch` is not a base-branch override — it names the session's own head branch to check out and push (`restdtos.CreateSessionRequestReposElem`'s own doc comment: null means "create the session branch from the repo's default base branch"), and a repo whose `repos[].branch` is left null is skipped before PR creation ever runs (`sendPushBestEffort`'s own doc comment) — it never reaches `resolvePRBaseBranch` at all. There is no session-config field that supplies a PR base today. Whichever code opens the fix PR (Step 48) must never route through that same resolution for this one call site: the fix PR's base is fixed by design to the origin PR's own head branch (this section's opening paragraph), because a stacked PR's base is its parent's head branch, not the repository's default — reusing `resolvePRBaseBranch` unmodified here would silently retarget the fix PR at, say, `main`, undoing the entire point of stacking it on the origin. Default-branch resolution and stacked-parent assignment are two disjoint code paths keyed on whether a deliberate parent is in play, never a single path that happens to fall back correctly.

### 17.3 Verdict update
Once the fix PR exists, the original verdict is updated (via the same server-side verdict-posting tool, §5.2 — never a raw comment) to reference it: which finding(s) it addresses, and that it will merge automatically once the origin PR merges. From this point, the finding's manual Apply-suggestion action (§12.2 item 2) is suppressed — the two remediation paths are mutually exclusive per finding, so a human and the automation can never both act on the same finding.

Whichever review session eventually posts a verdict on the fix PR itself (§8.2/Step 46 — re-triggered the same way as any other PR) is bound by §21.1's stacked-PR review-scope decision: that verdict covers only the fix PR's own small diff against the origin PR's branch, never the cumulative origin+fix diff, with the pair's position/size/ultimate base supplied to it only as context.

### 17.4 Merge gating
The fix PR does not auto-merge on its own CI-green — it waits for a merged-PR event on the origin (GitHub ingress webhook). On that event, the fix branch is **not** simply re-targeted at `main`: only the fix session's own commits are cherry-picked onto the current tip of the default branch and force-pushed to the fix branch. A bare base-retarget would only produce a clean, fix-only diff when the origin merged via a merge commit — under squash or rebase-merge the origin's original commits are never reachable in `main` by the same hashes, so retargeting would resurface the *entire* origin diff. Cherry-picking just the fix commits is merge-strategy-agnostic and is the only form of this operation this feature performs. If the cherry-pick conflicts, that is a hard stop, never an auto-resolve — the fix PR falls through to 17.4's failure path below.

**GitHub's own stack machinery does part of this on its own — verified against GitHub's own published documentation for this amendment, not assumed.** GitHub's stacked-PR reference states that when a PR merges, "the pull requests above stay open and automatically re-target the stack's base branch," and its own "Rebasing" section is explicit that this is not merely a pointer update: "When you merge a pull request at the bottom of the stack, the remaining branches are automatically rebased so the next pull request targets the default base branch" (docs.github.com/en/pull-requests/get-started/about-stacked-prs, fetched 2026-07-31, public preview). The origin PR is, by construction, the bottom of this section's two-PR stack, and the fix PR is the one remaining branch above it — so this is not a hypothetical: **the same origin-merged event that triggers this section's own cherry-pick-and-force-push (§17.1) is the exact, documented trigger for GitHub's own automatic rebase of the fix branch.** GitHub's own rewrite and Narvi's own force-push are therefore two independent writers racing to update the same ref off the same triggering event. Narvi's own mechanism does not depend on GitHub's having already run — it force-pushes the fix branch directly by name regardless of the branch's current state, so its own write, computed directly from the fix session's own commits, is a correct cherry-pick result on its own terms whichever writer lands last. **What documentation alone cannot settle, and is a genuine open question to resolve before this ships:** whether GitHub's automatic rebase can itself reject or interfere with a concurrent force-push to the same ref, and whether Narvi should therefore sequence its own push to run only after GitHub's automatic rebase has observably settled, rather than racing it.

This is a **system-initiated action, not a delegated human one** — it does not call `Authorize(actor, ...)` (§13.3), because there is no actor. It instead re-checks, explicitly, the same facts a human clicking Merge would rely on: the cherry-picked diff touches nothing but test and documentation files, CI is green at the new tip, the cherry-pick applied cleanly, and the toggle is still enabled. (The diff-scope check here is independent of, and does not rely on, §17.2's write-capability restriction on the child session — a restriction enforced at spawn time is never trusted as sufficient on its own; this re-verification runs regardless of whether that restriction held.) **Only if all four hold** does it auto-approve under the criteria-driven policy (§21) and merge. Any one failing (scope grown beyond tests/docs, CI red, a conflicting cherry-pick, toggle flipped off mid-flight) leaves the fix PR as an ordinary `needs_review` item (§16.1) instead of forcing it through — the fallback path is always a normal, human-supervised one.

### 17.5 Audit and visibility
The merge is recorded in `audit_log` (§13.3) with `actor_user_id` NULL — using the same allowance already made in the audit_log schema for actions with no human actor — and `action`/`detail_json` capturing the origin PR, the review session, the fix PR, and which of the four checks passed. If the origin PR itself is never merged (closed, abandoned), the fix PR is simply left open as an ordinary review item — never silently discarded.

### 17.6 GitHub-native stacks: registering the existing pair, and why not further

GitHub has, since the rest of this section was first written, made stacked pull requests a first-class server-side object — `POST /repos/{owner}/{repo}/stacks`, `PullRequest.stack`/`stackEntry` in GraphQL (confirmed present via live schema introspection for this amendment; the `Mutation` type carries zero stack mutations, confirming GraphQL is read-only here), and a `stack` object riding on every PR REST resource and on native `pull_request` webhook events. §17.2 already opens the fix PR as a stacked PR in the informal, base-branch-convention sense that predates all of this. This section is about making that **existing** relationship legible to GitHub's own object model and consuming the context GitHub now supplies for free — it is not the introduction of a new capability, and nothing below changes what §17.1-§17.5 already do.

**Scope: the one pair, not an N-deep producer.** Narvi registers exactly the origin+fix pair §17.2 already creates. It does not gain a capability to decompose arbitrary work into a chain of dependent PRs, because nothing else in this plan produces a chain of more than two dependent pull requests today. In particular, two mechanisms that superficially resemble a decomposition-into-multiple-units feature are not stack producers: the sub-task fan-out mechanism (§7.1) operates entirely within one turn — "a presentation/wire-level grouping of events belonging to one turn," not a new Postgres row, and, per its own doc comment, "the turn state machine (§3.3) is unaffected" no matter how many sub-tasks ran — it produces no pull request of any kind; and the product-prototyping handoff's v2 child session (§14.4) is spawned in a fresh, full-access Environment pre-loaded with the prototype diff purely as reading context for its plan-mode approval — §14.4's own text never bases that child session's own eventual work on the prototype PR's branch the way §17.2 explicitly does for the sentinel fix, so it produces an independent PR, not a second stack member. Designing an N-deep stack producer before anything in this plan actually generates that shape of work would be speculative scope with no consumer.

**Ingress: capturing stack context, honestly scoped to what's actually parsed today.** GitHub's own guarantee covers exactly two carriers: every PR's REST resource, and the native `pull_request` webhook event. Narvi's existing GitHub ingress (Step 32, `internal/adapters/inbound/github`) parses neither of those directly — `payload.go`'s two webhook payload structs (`issueCommentPayload`, `pullRequestReviewCommentPayload`) decode the `issue_comment` and `pull_request_review_comment` event types instead, and GitHub's own reference does not state that either of those two event types carries a `stack` object — only the dedicated `pull_request` event type is confirmed to (§24.1 already documents, independently of this amendment, that "nothing in this codebase today parses GitHub's `pull_request` event at all"). The one outbound REST round-trip this ingress already makes — `githubapi.Adapter.GetPullRequest`, called from `headresolve.go` to resolve an `issue_comment` mention's real head branch — decodes the PR resource today via `pullRequestResponse`/`PullRequest`, which model only `head.ref`/`head.repo`; neither type decodes `stack` yet. Capturing stack context therefore needs `pullRequestResponse` extended with a `stack {id, number, size, position, base{ref, sha}}` field (the same nullable-pointer discipline `head.repo` already gets, since a non-stacked PR carries no `stack` object at all) and `PullRequest`/`mention` threaded with the same — an incremental addition to a call this ingress already makes for every `issue_comment` mention, not a new outbound call. (Whether it is also worth adding to the `pull_request`/`synchronize` lane §24 introduces, once that lane exists, is a smaller, later question; the REST path above is sufficient for what §21.1's review-scope decision needs today.)

**Repository enablement — resolved by GitHub's own documentation, not left as a guess.** GitHub's stacked-PR rollout guide states plainly: "If your team is already using pull requests, you're set up to use stacked pull requests" (docs.github.com/en/pull-requests/tutorials/roll-out-stacked-prs, fetched 2026-07-31) — no per-repository toggle, admin action, or opt-in is described; every repository already open to pull requests already has it. Confirmed empirically against this very repository during this amendment: `GET /repos/narvidev/narvi/stacks` returns `200 OK` with an empty array today, not `404`. §17.2's log-and-ignore handling of a `404` (or any other failure) from the registration call stays in the design regardless — GitHub's own docs still describe the feature as "in public preview and subject to change," and the empirical result above is one data point on one host, not a guarantee for every GitHub Enterprise Server version or organization policy Narvi might run against.

### 17.7 Phasing
(Renumbered from §17.6 on 2026-07-31 to make room for the new §17.6 above; nothing elsewhere in this plan cited §17.6 specifically, so this is a contained rename, not a cascading renumbering.)

Extends the code-review domain and sentinel family (§8.2, Step 45/48) — Phase 5, after the sentinels themselves exist; reuses child sessions (§14.4), the verdict-posting tool, and the criteria-driven auto-approval policy (§21), so no new subsystem. UI: the toggle (Settings → Environments) and the fix-PR link on finding cards (§12.2 items 2 and 5) are mocked/built in Phase 7 alongside the rest of those views. The GitHub-native-stack amendment above (§17.6) lands in this same Step 48 PR — it makes Step 48's own existing behavior legible to GitHub's object model, not a new Step or a new phase dependency.

## 18. Unified intent classifier (detailed design)

§8.3 states the feature as an exit criterion; this section fixes the contract, rubric, and decision-record shape so Step 36 doesn't have to invent them under time pressure.

### 18.1 Never-throw contract
`IntentClassifier.Classify(ctx, input) IntentDecision` never returns a caller-fatal error — every code path resolves to one of two shapes, and callers pattern-match on `Source`, never on error type:

```go
type IntentDecision struct {
    Source         string // "classifier" | "fallback"
    Target         string // decision-specific, e.g. review/request
    Mode           string // e.g. plan/build
    Confidence     string // "high" | "medium" | "low" — classifier source only
    Reasoning      string // classifier source only; see §18.4 for storage/exposure rules
    FallbackReason string // fallback source only; enumerated, see below
}
```

`FallbackReason` is an enumerated, growable set — `no_api_key`, `timeout`, `invalid_output`, `api_error`, `unsupported_provider` — distinguished via typed errors from the underlying `LLM` port (§4.3), classified by error code, **never by string-matching** (the same discipline §4.1 already requires of `ProviderError`). An unsupported or misconfigured classifier-model choice is itself just another fallback reason, not a silent substitution that keeps reporting success.

The `LLM` port must be multi-provider **by construction**, not merely provider-agnostic in name — this is what makes `unsupported_provider` above a real, reachable state rather than dead code that could never fire. Its interface and typed errors carry no vendor-specific shape (no Anthropic/OpenAI-specific error codes, HTTP semantics, or SDK types leaking through the signature); a configured provider name is resolved through a small registry/factory at the wiring layer, and an unrecognized one is a genuinely exercised, tested code path to `unsupported_provider` — not a theoretical possibility. This mirrors `SandboxProvider`/`AgentRuntime`'s own established "one real adapter now, a second stubbed for later" precedent (§4.1/§4.2, and CLAUDE.md's own "don't couple a port to a single adapter" rule): only Anthropic needs a working adapter at Step 36 (OpenAI/Codex is §8.8's own later scope), but adding that second adapter later must require touching only a new adapter package plus one registry entry — never this port's own interface, the classifier's domain logic, or its orchestration.

Timeouts: rely on the LLM client's own request-timeout option/error; never race a manually-armed `context.WithTimeout` against it as a second, redundant layer — the SDK's own internal abort always resolves first, so an outer wrapper timeout would never actually fire. The actual value lives in `platform/timeouts.go` (§5.4), not as a literal here.

### 18.2 Confidence rubric
Anchor confidence on **how directly the input text supports the decision** — never on how certain the model reports feeling (a rubric asking the latter degrades to reporting "high" almost unconditionally):
- **high** — a clear, direct textual signal (even via a well-known synonym) that an attentive reader would not second-guess.
- **medium** — a reasonable inference from context, tone, or indirect phrasing that an attentive reader could plausibly read differently.
- **low** — no strong signal; the input plausibly supports more than one reading.

This rubric is a **single shared constant**, referenced by every ingress surface's classification call — never duplicated per surface (duplication is exactly how it drifts). It lives at the field-description level of the classifier's structured-output schema, next to the field it governs, not floated separately in a system prompt.

`needsClarification` is **derived in application code** from confidence plus how many plausible targets exist — never asked of the model directly, keeping the threshold a versionable, testable piece of code rather than model behavior. For any action that is irreversible once taken (triggering a review, dispatching a build), the classifier's signal must be corroborated by an independent deterministic check (a regex or label match) before acting; on disagreement between the two, ask for clarification rather than guessing. §5.2 states the corollary this rubric's deterministic path must also satisfy: any re-run phrasing a review verdict recommends to a user has to be one this same fallback recognizes, not just the model-based side.

### 18.3 Calibration methodology
Automated shadow-mode divergence reporting (§9.4) catches wrong *routing* decisions, but not a miscalibrated `confidence` field on an otherwise-correct decision (e.g. everything reported "high" regardless of actual ambiguity in the input) — that failure mode only surfaces via periodic **manual spot-review** of a confidence-labeled shadow sample, cross-referenced by `correlation_id` against the deterministic-fallback path's own decision on the same input. Both methods are required for calibration sign-off; an automated divergence rate alone is not sufficient.

### 18.4 Per-session routing decision record
One record per session, `IntentDecisionRecord`: `session_id`, `surface` (web/slack/linear/github), `source` (classifier/explicit/fallback), `target`, `mode`, `confidence` (nullable, classifier source only), `reasoning` (nullable, truncated to a bounded length — never rejected outright for being long, just cut off), `decided_at`, `decided_at_stage` (`create` | `first_prompt` — some surfaces have the full text at session creation, others, e.g. web with warm-on-type, only at the first real prompt), `cost_usd` (nullable — the classifier's own LLM call has a real cost; omit rather than guess when unknown, same discipline as the model catalog's cost data, §8.8).

`reasoning` is **stored for audit** (it lands in the same audit-minded posture as `audit_log`, §13.3) but **never rendered on any Slack/Linear/GitHub-facing surface** by default — same untrusted/sensitive-output handling discipline §5.2 already applies to PR diffs and external content. This resolves the storage-vs-exposure question explicitly rather than leaving it to whatever an implementation happens to do: store it, don't broadcast it.

Persisted **write-once via a guarded update** (`UPDATE sessions SET intent_decision = ... WHERE intent_decision IS NULL`), not read-then-write — first decision wins, no application-level lock needed. A decision record supplied by the calling surface is honored **only** for `spawn_source` values architecturally capable of having classified it themselves; this check is server-side and never trusts a client-supplied claim (§5.2) — anything else is silently re-synthesized server-side.

### 18.5 Shadow mode is permanent, not a launch gate
See §9.4. Activating the classifier on a surface (shadow → acting) must never delete the shadow code path, its config, or its telemetry — the same mechanism gets reused for every future model swap, prompt change, or new ingress surface, not just the first one. Skipping the shadow-mode window for a change because "tests already prove equivalence" is not a default; it requires an explicit, documented exception.

### 18.6 Classification surfaces
The classifier serves multiple independent categories through the same contract, rubric, and record shape (§18.1-§18.4) — never a parallel classifier per category: review-vs-request and plan-vs-build (Step 36), release-vs-feature (§15.1, Step 50), and — new, Step 64 — `plan_followup` (amend-vs-answer, §23), gated on an existing plan being `awaiting_approval`. An internal-only surface like `plan_followup` is never registered on any public HTTP route — private to `app/`, never wired into `httpapi`/`wshub` — a structural exclusion, not a reviewed convention; any future internal-only classification surface should follow the same rule.

### 18.7 Phasing
Detailed design underlying §8.3 (Step 36, phase 3) and the Settings → Prompt templates screen (§12.2 item 5, phase 7). No prior art exists (anywhere referenced in this document's research) for the DB-backed template storage/versioning/assembled-prompt-preview piece — it is designed from scratch when Step 36 is implemented, using this section's contract, rubric, and record schema as the foundation underneath it.

## 19. Warm-boot shared-image prebuilds (new capability)

Problem this solves: `imagebuild.Fingerprint` keys an image on exact `(base, repoSHAs, runtimeVersion)` (fingerprint.go:29-45; §8.5-note/§10-P2's own "fingerprint = repo SHAs + runtime version"). This makes a warm hit structurally rare — any push to any repo in the set invalidates the key, and the very session that registers a pending build spawns on the base image anyway (§10 Phase 2: "always fall back to base image on any miss"); a build only pays off if a *second* session later targets the identical tip SHAs. In an actively-developed repo the hit rate trends toward zero by construction, leaving Step 26's image-build pipeline attached to a near-empty cache.

The fix redefines what "warm" means: an image's job is not to *be* the session's exact workspace, it is to be a warm cache *near* it. Boot-time `gitclone.SyncAll` (§3.4) already exists precisely to close the gap between baked state and desired state (stash → checkout → pop, plus the sparse-checkout tail Step 29 already ships end-to-end for scoped Environments, §14.1). This section re-keys images on SHA/branch/scope-independent inputs, continuously refreshed from each repo's default-branch tip, and extends `SyncAll` just enough — a bounded fetch, a conditional `setup.sh` rerun — to close the now-larger gap.

### 19.1 Fingerprint and the baked/reconciled split

`Fingerprint(base string, repos map[string]string, runtimeVersion string)` is redefined: `repos` maps repo name to its normalized clone URL, not its resolved SHA. The canonical NUL-separated, name-sorted encoding (fingerprint.go:47-55) is unchanged — only the value keyed per name changes. Because `image_builds` carries no data from before this design (the table is a pure cache, never a system of record), the function is redefined outright: no version tag on the digest, no dual-scheme migration period, existing rows are simply dropped as part of the migration that adds the columns below. (Keep this as a reusable technique, not a precedent to repeat casually: the *next* time this fingerprint's inputs change, after real `image_builds` rows exist that must keep resolving during a rollout, a version-tagged digest and a dual-scheme window are the right tool — just not needed here.)

Repo URLs must stay in the key rather than dropping to just `(base, runtimeVersion)`: `SyncAll` cannot reconcile a repo-set or remote-identity mismatch — it never clones (its own doc comment: "SyncAll never runs `git clone`"), never cross-checks the on-disk `origin` against a session's configured URL, and a later `git push` targets whatever origin was baked in. A session must only ever match an image whose repo set and remotes are identical by construction, and keying on `map[name]url` guarantees that directly. In practice this yields one shared image per distinct repo set — roughly one per Environment. `path_scope` and `mock_config` (§14.1) stay excluded from the key, same as today, since scoping is reconciled at boot by the already-shipped sparse-checkout tail, not by image identity. (A repo URL's own case/trailing-slash/`.git`-suffix normalization is not yet a solved problem in Narvi: `reposource.ValidateRepoURL` validates syntax today but does not canonicalize; the fingerprint's own key-derivation step must normalize before hashing, or two differently-spelled URLs for the same remote will silently produce two images.)

Baked into the image at build time:
1. **Full (non-shallow) clone of every repo at its default-branch tip.** Full history is load-bearing for §19.3: the boot-time fetch must be a small delta against a complete object store.
2. **`setup.sh` executed against that tip**, unchanged `BootModeBuild` semantics (fatal on failure, `hook.go:41-47`). Its real payload is the warm dependency caches (`node_modules`, venvs, build caches).
3. **Pinned runtime** (`RuntimeVersion`), unchanged.
4. **A baked self-description manifest**, `/narvi/image-manifest.json`: `{fingerprint, built_at, built_repo_shas: {name: sha}, dependency_manifest_digests: {name: digest}}`. This lets sandbox-agent decide the setup-rerun question (§19.4) locally, with no extra control-plane round trip — the image describes itself. `dependency_manifest_digests` is §19.6's own tier-1 digest-skip input, produced by this build service and consumed at boot by `internal/sandboxagent/boot.ComputeDependencyManifestDigest` — since the two run in different processes (this build service is external and unmodeled, §19.1's own baked-vs-recomputed split), the algorithm below is this document's own byte-for-byte specification, not merely a description of what one implementation happens to do:

   - For each repo, walk its own working tree **recursively** from the repo root, never descending into a directory whose basename is one of the fixed, closed set `.git`, `node_modules`, `vendor`, `bower_components`, `dist`, `build`, `target`, `.venv`, `venv`, `__pycache__` (at any depth; the repo root itself is always descended into regardless of its own basename), and never following symlinks.
   - For every remaining regular file whose basename is one of the fixed, closed set `Cargo.lock`, `composer.lock`, `Gemfile.lock`, `go.sum`, `package-lock.json`, `Pipfile.lock`, `pnpm-lock.yaml`, `poetry.lock`, `requirements.txt`, `yarn.lock`, record its path relative to the repo root, with forward slashes regardless of host OS.
   - Sort the collected relative paths lexically (byte-wise on the slash-joined string) — canonical and platform-independent, so the digest never depends on filesystem/OS iteration order.
   - In that sorted order, SHA-256 each file's own raw content individually, and fold `"<relative-path>\x00<hex(sha256(content))>\n"` (`\x00`/`\n` literal bytes) for each into one outer SHA-256, in order. The final digest is `hex(outer-SHA-256)`.
   - A repo with **zero** matching files anywhere produces no meaningful digest at all — this producer must omit that repo's own entry from `dependency_manifest_digests` rather than baking a placeholder value, matching sandbox-agent's own `ComputeDependencyManifestDigest(repoDir) (digest string, found bool, err error)` contract: `found: false` is a first-class outcome, never conflated with "found, and it happens to hash to some particular value" (§19.6, adversarial-review finding B1 — a build-time producer and a boot-time recompute that both silently treated "nothing found" as a legitimate digest would match FOREVER for any repo using an unrecognized dependency ecosystem, regardless of what that repo's own real dependencies did).
5. **No sparse-checkout at build time, ever** — shared images are always built unscoped (§19.7).

`ports.ImageSpec` (`createspec.go:86-100`, currently `{Base, RepoSHAs map[string]string, RuntimeVersion}`) becomes `{Base, Repos map[string]RepoRef{URL, SHA}, RuntimeVersion}`. The builder resolves each default-branch tip SHA **at claim time** and passes concrete SHAs to `BuildImage` — builds stay pinned and reproducible; only the *key* is SHA-free. The resolved SHAs persist as `built_repo_shas` on the row and bake into the manifest. `ports.ImageSpec` stays adapter-neutral (§4.1's own "no out-of-interface operations" discipline) — nothing Modal-specific leaks into the shape, and a second `SandboxProvider` adapter receives the same spec.

**`RuntimeVersion` in the key is a simultaneous-invalidation cliff — recorded as a known operational risk, not solved here.** Because `RuntimeVersion` is a fingerprint input, bumping it changes *every* scope's fingerprint at once: no existing `ready` row matches any longer, every session falls through to the base image, and every Environment's first post-bump build is enqueued inside the same window. Nothing is lost — a runtime bump is *meant* to invalidate — but the cost arrives concentrated rather than spread, and it lands exactly when the build path is under its heaviest load, so a build-side fault that would otherwise degrade one Environment degrades all of them at once. Two properties already hold and must not be regressed: the fallback is a `fresh` boot, where `setup.sh` failure is non-fatal (`hook.go:41-47`), so a bump can never fail a session outright; and the exponential-backoff-plus-streak-alert discipline already shipped for failed builds, together with the terminal `permanently_failed` guard, keeps one hopeless scope from starving the pump for every other. What is deliberately *not* designed is any staging of the bump itself — per-scope adoption, or continuing to serve the previous runtime's image until its replacement is `ready`. That is left open because the honest fix depends on whether a bumped runtime is ever compatible with an image built against the previous one, which the pinned-runtime contract (§7) deliberately does not promise; inventing a compatibility window here would be asserting a guarantee the runtime pin refuses to give.

**Build-time dependency cache: a named gap, now on its third implementation.** Item 2 above says `setup.sh`'s real payload is the warm dependency caches, and §19.4's rerun contract leans on the same property from the other end ("`npm ci`/`pip install`/`cargo build` against a warm cache is seconds, not minutes"). Both statements are about caches *inside an already-built image*. Neither says anything about the build itself — and before this design, nothing carried a package cache *between* builds: every build started from `Base` and re-downloaded every dependency over the network. Two consequences followed. A build's duration, and more importantly its *failure probability*, scaled with a repo's total dependency count rather than with whatever actually changed. And because the unit of caching was the whole image, a build that died on one transient registry error discarded all of its work, including the parts that had already succeeded. §19.2's refresh pump turns this from a per-Environment annoyance into a steady-state cost, since it rebuilds every `ready` image whenever its repos' tips move.

**Current design: immutable versioned cache snapshots — the third iteration, kept honest by recording why the first two failed.** Every successful build publishes a brand-new, immutable, distinctly-numbered version under a cache key (`domain/imagebuild.CacheVolumeKey(Base, RuntimeVersion)` — never repo content, so a cache stays shared across every repo set built from the same base/runtime, same reasoning as before). A build mounts exactly *one* specific, already-published version read-only (`ports.CacheMount.MountVersion`) and, if it succeeds, publishes a distinct new one (`ports.CacheMount.PublishVersion`) — never the same identifier as what it mounted. There is no shared *mutable* state anywhere in this design, so there is no write window for a concurrent reader to observe, at any point, not merely a narrow one: **a lock is meaningless, not merely unnecessary**, because there is nothing left for a lock to guard. Rotating away a bad version falls out of ordinary version history — an operator escaping a corrupted or oversized version does so by removing its row from this control plane's own version bookkeeping, which makes the next-most-recent, still-good version the one new builds resolve — never a second, parallel rotation mechanism. `domain/imagebuild.WellKnownCachePaths()` (npm's `_cacache`, pip's HTTP cache, Go's module and build caches, Cargo's registry, and siblings for Yarn/pnpm/Composer/Bundler/Maven/Gradle) is unchanged: still the fixed, package-manager-agnostic set of paths a version's mount and publish apply to. `ports.ImageSpec.CacheMount{Key, MountVersion, PublishVersion, Paths}` and `ports.BuildOutcome.PublishedCacheVersion` (BuildImage's new success-path return value, confirming whether an eventual successful call genuinely still carried the mount) are the enforceable expression of this — see those types' own doc comments (`internal/app/ports/createspec.go`, `internal/app/ports/refs.go`) for the full contract, including the honestly-named build-service obligations this repository's own Go code cannot itself verify (never mutate a published version's bytes; never let a version become mountable by anyone else's request until, and only if, the build that named it as its own `PublishVersion` reports success).

**What the first two attempts got wrong, briefly, because it is exactly the mistake the third attempt exists to not repeat.** The first implementation mounted one shared, persistent volume read-write and skipped a lock on the claim that every well-known cache path is a package manager's own "content-addressed" cache, so two concurrent writers could only ever collide harmlessly. Checked against each package manager's own real, documented on-disk layout, that premise was false for nearly every path mounted: Go's module cache ships its own `cache/lock` file and mutable `@v/list`/per-module `.lock` files precisely because concurrent access is not safe without coordination; Gradle's `caches/modules-2` is binary metadata mutated in place, guarded by a lock living inside the mounted directory; npm/pip key their caches by request URL rather than response content and mutate/append in place; Composer, Bundler, and Yarn are similar. The second implementation mounted that same shared volume read-only for a build's duration, with exactly one write-back merged in after success, and reasoned "nothing writes while a build can observe it, so no lock is needed." That reasoning was itself incomplete, not merely superseded: the write-back was *itself* an unguarded writer into a volume other builds could be reading from at that exact moment — narrowing the write window from "the whole build" to "one write-back" is not the same as removing it, and a lock-free write into state a concurrent reader can observe is the identical hazard the first attempt had, just smaller. This third design does not narrow the window further; it removes it, by making every write create a new, distinctly-addressed, immutable object rather than ever touching one that already exists.

**Cross-tenant provenance is unchanged by any of this, and this document says so plainly rather than implying otherwise.** One cache key is still shared fleet-wide across every repo set built from the same `Base`/`RuntimeVersion` — versioning changes nothing about that. A published version's bytes were still produced by running some *other* repo's own `setup.sh`, and every later build that mounts that version still executes whatever it contains. Immutability guarantees *when* a version is safe to read (never concurrently with its own creation); it says nothing, and was never intended to say anything, about *whose* code produced what a build reads. This was already true of both earlier attempts and remains true here.

**Retention: specified, not left implicit — the concrete answer to "immutable versions accumulate."** Publishing on every successful, cache-requesting build makes the original unbounded-size gap worse unless something prunes it. `domain/imagebuild.RetainedCacheVersions` (5) and `PruneCacheVersions` specify the policy: keep the newest N confirmed versions per cache key, prune the rest from this control plane's own bookkeeping, applied by `app/imagebuild.Builder` immediately after every confirmed publish. A reader whose already-resolved `MountVersion` is later pruned from that bookkeeping is unaffected — pruning only ever removes a *future* build's candidate, never retroactively invalidates an already-sent request, and the one adapter that exists today (`internal/adapters/outbound/modal`) treats a mount naming an unrecognized/reclaimed version exactly like any other cache trouble: decline, and fall back to an ordinary cold build. What this control plane cannot itself do — and states as a build-service obligation rather than describing as solved — is reclaim the underlying bytes for a version once its bookkeeping row is pruned; that remains a named, deferred gap, mirroring §19.2's own "newly urgent, still deferred" image-GC posture.

**Concurrency, restated for what changed.** `internal/adapters/outbound/modal`'s own `BuildImage` still requests the cache mount on every build and, the instant its own wire protocol reports cache trouble, transparently retries once with the mount dropped — an ordinary cold build, indistinguishable on the wire from a request that never asked for a cache at all — never distinguishable from a caller via any error path (`ports.CacheMount`'s own "purely advisory" contract, unchanged in spirit across all three attempts). What is narrower now: a bare client-side transport timeout is deliberately **not** treated as cache trouble (it used to be, on the reasoning that "a degraded cache-volume subsystem typically hangs"; that broadening meant a build that legitimately exceeded `ProviderHTTPClientTimeout` for reasons unrelated to the cache retried cold — strictly slower — against the identical budget, doubling wall clock and very likely failing twice instead of succeeding once). A build service's own internal cache-mount timeout is instead represented honestly, via a fast, structured code, the same class of signal as corruption/unavailability/a not-found version — never inferred from the client's own elapsed clock. RWX remains entirely unaffected (`Capabilities().ImageBuilds` is false; its own content-addressed layer cache already gives it this effect natively, §4.1.1).

### 19.2 Rebuild triggers and the staleness window

Extend `app/imagebuild.Builder` (builder.go) with a second phase of `PumpOnce`: every `ImageRefreshCheckInterval` (propose 10 min, `platform/timeouts.go`, alongside `ImageBuildPumpInterval`'s existing 60s), for each `ready` shared row, resolve each repo's default-branch tip; if any differs from `built_repo_shas`, enqueue a refresh. The existing claim query (which already excludes `'building'` rows, doc.go) gives single-flight per fingerprint for free.

**Refresh never degrades availability**: the row stays `ready`, serving the old `image_ref`, while the new build runs; on success the builder atomically swaps `image_ref` + `built_repo_shas` + `built_at`. This requires relaxing the current "no rebuild-of-an-already-ready-fingerprint, ever" invariant (imagebuild's own doc.go: "no rebuild-of-an-already-ready-fingerprint mechanism exists") — deliberately, and only for this in-place refresh path, never for a serving-path rebuild. Expected staleness window: check interval + build duration, roughly 10–40 minutes — acceptable because staleness is no longer a *correctness* boundary once §19.3/§19.4 exist: boot-time fetch reconciles source content and the conditional setup rerun reconciles dependencies. Staleness only sets the *size* of the boot-time delta — it degrades latency, never correctness.

**Credential requirement**: the freshness pump needs GitHub credentials belonging to no session creator (today's per-spawn SHA resolution borrows the spawning creator's token, `imageresolve.go`; a shared image has no creator). The build service already needs clone credentials to build at all — the recommendation is a platform-level credential (GitHub App installation token) shared by the freshness pump and the build service, configured in `platform.Config`. This is the one genuinely new piece of infrastructure this design needs.

**Spawn-path simplification**: `imageresolve.go`'s `resolveAndSetImage` no longer needs a per-repo `ResolveBranchSHA` call at all — the fingerprint is computable from session config alone. This removes up to `len(repos) × RepoSHAResolutionTimeout` (10s each, `platform/timeouts.go`) of sequential GitHub latency from every spawn attempt, and removes the "creator has no GitHub token → cold boot" fallback class entirely. Warm boot becomes the default outcome for every session whose repo set has a ready shared image — which, after the first build of an Environment, is essentially always.

**Newly urgent, still deferred**: image GC. In-place refresh produces a superseded image ref roughly every 10–40 minutes per Environment, so the already-deferred `DeleteImage`/GC gap (imagebuild's own doc.go: "it never calls DeleteImage... no rebuild-of-an-already-ready-fingerprint mechanism exists") grows from "someday" to "schedule within the same phase this design ships in" — named explicitly here rather than silently left implicit.

### 19.3 Boot-time fetch and the degrade policy

`SyncAll` today is local-only by design: `runGit` never wires the credential helper (`sync.go:333-358`), and `checkoutBranch` falls back to `git checkout -b <branch> HEAD` when the branch isn't local (`sync.go:476-491`, the fallback itself at line 486). Under warm-from-tip images that fallback becomes a trap:
- **A session with an explicit branch that exists on the remote but not in the image**: today's code would silently create a *new, same-named* branch at the image's stale tip HEAD — work proceeds on the wrong base, and a later push is non-fast-forward against the real branch. Silent divergence — exactly the failure mode `domain/gitstate`'s whole machinery exists to prevent.
- **Invented `narvi/<sessionID>` branches**: created at image HEAD, i.e. up to the staleness window behind tip. Tolerable, but avoidably conflict-prone.

**Required changes, `internal/sandboxagent/gitclone`:**
1. New step in `syncOne`, before the dirty-check/checkout: `git fetch origin <resolved-branch> <default-branch>`, bounded by a new `GitFetchStepTimeout` (network-bound; propose 90s, distinct from the existing local-only 30s `GitSyncStepTimeout`), with the credential helper wired exactly as the clone path already does for its own remote operations.
2. `checkoutBranch` prefers `origin/<branch>` when the branch isn't local: `git checkout -b <branch> origin/<branch> --`; only when the ref exists on neither side does it fall back to `origin/<default>` (fetched) or `HEAD` (fetch itself failed).
3. **Degrade policy** — the resilience-critical detail:
   - Fetch failure with the target branch resolvable locally, or acceptable-from-HEAD (an invented branch): **warn and proceed on stale image state**, recorded in the boot log/`AGENTS.md`. Warm boot must never become network-dependent for liveness.
   - Fetch failure when the session **explicitly named** a branch that is neither local nor fetchable: **fail that repo** (primary → fatal boot, secondary → warn/exclude — the existing severity split §3.4/§6.4 already uses). Silently forking a same-named branch at a stale base is the one outcome that must never happen; this rule is non-negotiable in review.
4. New `gitstate` states/triggers for fetch outcomes, through the existing `Transition` table (state.go) and `TriggerFor*` helper convention (sequence.go), with the same table-driven test discipline the existing ten states already have.

Full clones at build time (§19.1) keep these boot-time fetches small deltas.

### 19.4 The setup-rerun contract: `workspaceMoved` (§6.4 amendment)

Today's policy — `repo_image` skips `setup.sh` entirely (`EvaluateHook`, `hook.go:41-47`: `ShouldRun: mode==Build||Fresh`) — is sound only because the exact-SHA fingerprint guarantees image content equals session content. Under warm-from-tip images, the post-`SyncAll` tree can differ arbitrarily from what `setup.sh` ran against at build time (a branch adding a dependency, a lockfile bump on the default branch since the image was built). Leaving `repo_image`'s setup hook at `ShouldRun: false` unconditionally would produce sessions with silently missing dependencies — surfacing later as confusing agent/tool errors, not as a boot error, which is the worst failure class available here.

**This is a behavioral contract change to §6.4 and must carry the Conventional-Commits breaking-change marker** (`feat(sandbox)!`) when it ships — it changes what `repo_image` has meant since Step 13.

**Redefined contract**: *`repo_image` means "`setup.sh` ran at build time against a near-tip tree; if the boot-time workspace has **moved** from the built SHA, `setup.sh` runs again, **non-fatally** (warn, continue) — and is expected to be fast, because its outputs are already warm."*

- `EvaluateHook` gains a `workspaceMoved bool` input alongside `(mode, hook, primary)`. Policy: for `HookSetup` in `BootModeRepoImage` with `workspaceMoved`, `ShouldRun: true, FatalOnFailure: false`; `HookStart`'s existing policy is unaffected.
- `workspaceMoved` per repo = (post-`SyncAll` checked-out SHA ≠ the manifest's `built_repo_shas[name]`, §19.1) — computed locally by sandbox-agent from `/narvi/image-manifest.json`. SHA equality is the *entire* cheap dependency-diff check; nothing finer-grained is built. Narvi cannot know which files are setup-relevant for arbitrary repos, and guessing (lockfile globs, ecosystem conventions) would be exactly the kind of second, magical decision path this system avoids everywhere else. SHA-equal skips (the old exact-match case falls out as a pure optimization — zero regression for a session that does land on an unmoved image); SHA-moved reruns.
- This imposes an explicit contract on user repos: **`setup.sh` must be idempotent and incremental** — the same property package managers already provide (`npm ci`/`pip install`/`cargo build` against a warm cache is seconds, not minutes). Document this as a named requirement in the environments docs, next to the delta-script contract §19.6 adds.
- Non-fatal is the correct severity: a failed rerun on an otherwise-warm workspace still leaves a mostly-working tree and a running agent that can diagnose the gap — strictly better than failing the boot, and consistent with the existing "never block a spawn" invariant (§10 Phase 2).

**This "fast because warm" expectation is not free — it needs monitoring, not just trust.** A real setup script does more than package installs (it may provision local service stacks, run codegen, seed local state), and those parts are not necessarily warm-cache no-ops even when the package-manager portion is. Combined with the fact that Narvi's `workspaceMoved` predicate (SHA inequality) fires on essentially *every* warm boot — the §19.2 staleness window plus any branch delta makes an exact SHA match the exception, not the rule — a slow rerun would silently erode the exact latency win this design exists to deliver, non-fatally and therefore invisibly unless it is measured. §19.5's rerun-duration telemetry exists specifically to catch this before it becomes an incident; §19.6's graduated ladder (Step 43) is the shipped, ungated response to this exact risk — see §19.9 for why it does not wait on this telemetry to ship, and why the telemetry instead becomes the post-ship confirmation and tuning signal for it.

### 19.5 Hook output capture and rerun telemetry

Two small, concrete gaps this design surfaces and closes, both landing no later than the hook-policy change (§19.4) itself:

**(a) Hook output capture.** Non-fatal reruns are now the most frequent nontrivial boot-time work under this design, and today `runHook` spawns hooks with no `Stdout`/`Stderr` writers at all (`hooks.go:134-142`) — a spawned hook's output goes nowhere, and a failure surfaces only as `"%s: exited %d"` (`hooks.go:159-160`) with zero diagnostic content. A *non-fatal* failed rerun with no captured output is undiagnosable by construction: the warning §19.4 promises would carry no information a person or the agent itself could act on. `runHook` must pass a caller-held, bounded, ANSI-stripped output tail (on the order of 120 lines) through the supervisor's existing `Spec.Stdout`/`Stderr` seam (`supervisor.go:38-39`, already caller-owned `io.Writer`s, added for exactly this kind of need) — held by the caller, never allocated inside the awaited `proc.Wait` call, so a timeout-triggered `proc.Stop` can never lose a buffer that was never inside the cancelled operation to begin with. Surfaced in the boot log alongside any non-fatal hook failure. Shares the existing `HookTimeout` (`platform/timeouts.go:250-254`) — a rerun replaces the original `setup.sh` invocation within the same phase, so it needs no new timeout constant of its own.

**(b) Rerun-duration telemetry.** Per-hook wall-clock, emitted from the existing hook-run bracketing in `runRepoHooks`/`runHook`, joins the OTel metrics §5.3 already lists (boot phase durations). This is the concrete measurement §19.4's "expected to be fast" claim needs, and it is the post-ship signal §19.6's own ladder (Step 43, shipped ungated — §19.9) is tuned against — shipping §19.4 without this would leave that signal unmeasurable.

### 19.6 Graduated setup-rerun ladder (Step 43)

This adds tiers between "skip" and "full rerun." An earlier draft held the whole section until §19.5(b)'s telemetry showed full `setup.sh` reruns materially eroding the warm-boot latency win. That gate has been removed for both tiers below — it never fit either tier's own real risk equally, and neither survives its own argument for keeping it (§19.9 records why both ship ungated, with no per-tier telemetry trigger left):

**On the delta script specifically, the gate was also measuring only half its own question.** Rerun-duration telemetry answers "are reruns slow enough to be worth optimizing." It cannot answer "will repo owners actually author a delta script," and a ladder tier nobody writes a script for is the more likely way this fails. That second half is answered by offering the mechanism, not by measuring the first. Weighed against that, the tier concedes remarkably little: it is purely additive and opt-in (a repo with no delta script keeps exactly today's behavior, byte for byte), its eligibility predicate is strictly conservative (it runs only when `setup.sh` itself is provably unchanged, and any git error on that check is treated as ineligible), and every failure path falls back to full `setup.sh` and then to warn-and-continue. §19.5(b)'s telemetry therefore remains the instrument that confirms the win **after** this ships — and the trigger for §19.6's own tuning — rather than the precondition for shipping it.

- **A tier above the delta script: skip the rerun entirely when the dependencies provably did not move.** `workspaceMoved` (SHA inequality, §19.4) is a deliberately conservative *trigger*, never evidence that dependencies changed — and because §19.2's staleness window makes exact SHA equality the exception rather than the rule, it fires on very nearly every warm boot. A second, narrower predicate answers the question the trigger cannot: bake a digest of each repo's dependency manifests (its lockfiles — `package-lock.json`, `pnpm-lock.yaml`, `requirements.txt`, `go.sum`, …, the set discovered recursively, at build time — §19.1 item 4 has the exact, byte-for-byte algorithm) beside `built_repo_shas` in §19.1's manifest, and recompute it at boot against the checked-out tree. An equal digest proves the baked `node_modules`/venv/build caches are exactly the ones this tree wants — but that alone is not sufficient to skip `setup.sh` outright: a digest match speaks only to the dependency-manifest surface, never to setup.sh's own non-package-manager work (§19.4's own "may provision local service stacks, run codegen, seed local state"). The skip therefore additionally requires `setup.sh` itself to be provably unchanged since the built SHA — the identical check the delta-script tier below already needs (`git diff --quiet <built_sha> HEAD -- setup.sh`) — before `setup.sh` is skipped outright with no delta script needed. This tier is strictly cheaper than every other one and it captures the common case — a branch that touches code but not dependencies or `setup.sh` itself — leaving the delta script for the case it was actually designed for, dependencies that genuinely moved. Same conservative-on-error rule as the rest of the ladder: an unreadable, absent, or unrecognized digest means ineligible, fall through to the tier below, never a silent skip — and, symmetrically, a scan that finds **zero** recognized manifests anywhere in the checked-out tree (an unsupported ecosystem, or a scoped Environment's own necessarily-truncated tree, §19.7) is likewise ineligible, never a digest that happens to equal another empty scan's own digest.
- **The stronger predicate does not make failure fatal — and the reason changes.** §19.4's non-fatal rule currently rests on the trigger proving nothing ("a moved workspace proves nothing about dependencies"). A manifest-digest mismatch *does* prove dependencies moved, so that particular argument no longer applies to this tier and must not be cited for it. The rule still holds, on a different and more durable basis: availability. A boot that dies because a package registry was briefly unreachable converts a transient network fault into a failed session, whereas a warned-and-continued boot leaves the agent running on stale-but-present dependencies plus a structured warning it can actually see and reason about. So: retry the install on transient failure, then warn — never fail the boot on it. Recorded explicitly because the tempting inference ("we finally have proof, so we can finally be strict") is exactly the wrong conclusion to draw from a better predicate.
- An optional, repo-authored delta script (e.g. `sync.sh`, alongside `setup.sh`/`start.sh` in the closed hook vocabulary, `hook.go:9-14`) runs *instead of* full `setup.sh` when `workspaceMoved` is true but `setup.sh` **itself** is unchanged between the built SHA and the checked-out HEAD.
- "Unchanged" is answered exactly, with no new hashing scheme: `git diff --quiet <built_sha> HEAD -- setup.sh`, computed by sandbox-agent from the already-baked manifest (§19.1 bakes `built_repo_shas` for exactly this). This one predicate also uniformly covers a branch *adding or removing* `setup.sh` entirely — no separate empty-case handling needed. Any git error on this check is conservative: ineligible, fall through to full `setup.sh`.
- Failure ladder, matching §19.4's own non-fatal severity throughout (never escalate to fatal — a moved workspace proves nothing about dependencies, so the "moved" predicate can never justify failing the boot): delta script fails → warn → run full `setup.sh` → if that also fails → warn and continue, same as today.
- Every ladder decision (skip / delta / full / ineligible-fallback) logs a structured reason (§5.3), so the decision itself is auditable, not just its outcome.
- One new `EvaluateHook` policy row: in `BootModeRepoImage` with `workspaceMoved`, prefer the delta script over full `setup.sh` when eligible per the predicate above.
- Shares `HookTimeout` — the delta script replaces `setup.sh` within the same phase, no new timeout constant.
- The delta-script contract is documented in the environments docs beside the `setup.sh` idempotency contract §19.4 already requires.

### 19.7 Interaction with scoped Environments

Complementary, not competing: under exact-SHA images, a `repo_image` boot with a *changed* scope was rare (it needed an exact SHA re-hit across a scope change). Under shared, tip-tracking images, the baked workspace is scope-matched *never* — shared images are always built unscoped (§19.1), so every scoped session's `repo_image` boot depends on the sparse-checkout tail Step 29 already ships end-to-end (`applySparseCheckout`, `sync.go:222-232`, run after the pop, per repo, keyed on the session's own `Environment.PathScope`). This design adds no new work to that path — it makes it load-bearing for every scoped session rather than an edge case.

This same "baked unscoped, boot-time tree possibly narrower" fact reaches §19.6's own digest-skip tier: sparse-checkout runs *before* the ladder does, so a scoped boot's own recompute can find fewer manifests than the (always-unscoped) baked digest accounted for. §19.6's tier therefore treats every scoped session as ineligible for the digest skip unconditionally, regardless of what its own truncated tree happens to contain — never a false match, and never a false mismatch either (a coverage gap is not evidence dependencies moved, and must never be logged as if it were).

One gap this surfaces, not yet closed: `snapshot_restore` can restore a *scoped* session's snapshot into an *unscoped* config, and there is no `git sparse-checkout disable` branch anywhere in `gitclone` today for the reverse direction (an unscoped session syncing against sparse-checked-out on-disk state). Add the trivial `sparse-checkout disable` branch for the unscoped case as cheap hardening in the same Step that ships §19.3's fetch step — it removes a rule-discipline dependency ("always build shared images unscoped") that would otherwise need to hold forever without ever being checked.

### 19.8 A recorded invariant, not a Step: user-configurable environment variables

Narvi has no per-scope, user-configurable environment variable surface today — `SessionConfig` carries no user env map, hooks run with sandbox-agent's own inherited environment minus `NARVI_SESSION_CONFIG` (`supervisor.EnvWithout`, `hooks.go:141`, `env.go:21-37`), and `ImageSpec` carries nothing but `{Base, Repos, RuntimeVersion}`. Nothing here proposes building that surface — it belongs to a separate feature, if and when one is scoped. This is recorded now, before any such surface exists, purely because it interacts directly with this design's own fingerprint invariant:

**Decision rule for whenever such a surface is designed**: user-configurable environment variables must be either (a) session-boot-time injection only, never passed to `BuildImage`, or (b) if build-time parity is genuinely wanted, a canonical digest of the build-time environment must join the fingerprint inputs (§19.1) — never injected into a build silently. *(2026-08-06: that separate feature is now scoped — §27.1 builds the surface and adopts exactly rule (a), boot-time injection only, with the consequence for secret-requiring `setup.sh` builds named honestly in §27.8; §27.1's own name validation enforces the `NARVI_*` reservation below.)* The reason is structural, not stylistic: §19.1 keys images on content alone and shares one image across every Environment with the same repo set; a scope-bound env value baked into a shared image without joining the key would leak one scope's configured values into every other same-repo-set Environment's sandbox. A corollary for §19.4: `workspaceMoved` (SHA inequality) is a *complete* rerun predicate only while build inputs stay content-only — a build-affecting input invisible to any SHA (an env var change with no accompanying commit) would create a baked-versus-boot divergence that SHA equality alone could never detect. If build-time env vars are ever added, the §19.1 manifest is the natural carrier: bake an env digest beside `built_repo_shas`, and extend `workspaceMoved` to "SHA moved OR env-digest moved." Separately: the `NARVI_*` env-var namespace (already established — `NARVI_BOOT_MODE`, `NARVI_SESSION_CONFIG` carrying the sandbox's own plaintext bearer token, `boot/config.go:33-40`) must be reserved and excluded before any user-settable env surface ships, the same way `hooks.go:141` already excludes `NARVI_SESSION_CONFIG` from every hook's own environment today.

### 19.9 Phasing

- **Step 40** — sandbox-agent fetch-aware sync (§19.3): `gitclone/sync.go` fetch step + credential wiring, `checkoutBranch` remote-tracking preference, degrade policy; `domain/gitstate` fetch states/triggers; new `GitFetchStepTimeout`. Independently shippable and independently valuable — it also hardens today's exact-SHA `repo_image` boots against the local-branch-missing edge, with no other behavior change.
- **Step 41** — shared fingerprint + spawn-path simplification (§19.1): `imagebuild.Fingerprint` redefinition; `imageresolve.go`'s `resolveAndSetImage` drops the per-repo `ResolveBranchSHA` loop; `image_builds` migration adds `built_repo_shas`/`built_at` (existing rows dropped, not migrated in place — pure cache); `ports.ImageSpec` → `{Base, Repos{URL,SHA}, RuntimeVersion}`; build service bakes `/narvi/image-manifest.json` and full clones.
- **Step 42** — refresh pump + hook-policy change + hook diagnostics (§19.2, §19.4, §19.5): `app/imagebuild.Builder` freshness pump, claim-time SHA resolution, in-place `image_ref` swap, the new platform GitHub credential; `EvaluateHook`'s `workspaceMoved` policy and the §6.4 amendment (breaking-change marker); bounded hook-output-tail capture through the supervisor's existing `Stdout`/`Stderr` seam; per-hook rerun-duration telemetry; the `sparse-checkout disable` hardening (§19.7). New §9.3-class resilience scenarios: fetch-fail boot, stale-image boot, refresh-in-flight spawn, non-idempotent-setup boot.
- **Step 43** — dependency-work reduction: three PRs answering one question, **none of them telemetry-gated, all three now shipped**. Earlier drafts of this section gated (b) on rerun-duration telemetry and (c) on build failure rate; both gates are recorded below as withdrawn, because each measured something that could not resolve its own tier's real risk — and both would have deferred on data this deployment cannot produce until it carries production traffic, which is a way of never deciding rather than a way of deciding later. **(a)** The dependency-manifest-digest skip (§19.6), which needs §19.1's manifest to bake that digest — **ungated**: it removes provably unnecessary work rather than accelerating slow work, and concedes no correctness, so it does not wait on §19.5's histogram. **(b)** The delta script (§19.6) — **shipped with (a), no longer gated**: §19.6 records why the rerun-duration gate only ever answered half of this tier's question (it measures whether reruns are slow, never whether repo owners will author a delta script), and why the tier concedes almost nothing regardless — purely additive, opt-in, strictly conservative eligibility, every failure path falling back to full `setup.sh` then warn-and-continue. §19.5's telemetry becomes the post-ship confirmation and the tuning trigger instead. **(c)** The build-time dependency cache (§19.1) — **also ungated; shipped last, as its own PR, for size, not held for a measurement.** An earlier draft gated this on build failure rate and duration; that gate did not survive its own section. §19.2's refresh pump rebuilds every shared image whenever a repo tip moves, so cold re-downloads are this design's steady state rather than an occasional cost — the structural case needed no measuring. And §4.1.1 records that RWX's own content-addressed layer cache "already provides the warm-boot effect §19 builds by hand for Modal": (c) is parity with a capability another provider has natively, not a speculative generalization. Its blast radius is also smaller than it first appears — `Capabilities().ImageBuilds` already lets an adapter decline image builds entirely (RWX reports false), so this lands on the adapters that actually build, Modal today. What (c) genuinely needed before it could start was a **design** answer, not a number — the concurrency semantics of one cache shared by simultaneous builds. Getting there took three iterations, not one, and the plan text now records why rather than only the final answer. The PR that first shipped (c) reached "no lock" by arguing every well-known cache path is content-addressed (the filename IS the content hash), so two concurrent builds fetching the same package would write identical bytes to the identical path — checked against each package manager's own real, documented cache layout and found false for nearly every path (§19.1's own closing paragraphs have the full per-tool findings). A second version mounted the same shared, mutable volume read-only for a build's duration with exactly one write-back after success, and reasoned that this made a lock unnecessary — but the write-back was itself an unguarded writer into state a concurrent reader could observe, the identical hazard in smaller form. The version that actually shipped, this Step's third iteration, removes the write window instead of narrowing it: **immutable versioned cache snapshots** — every successful build publishes a new, distinctly-numbered, immutable version; a build mounts exactly one already-published version read-only and can never observe one published after it started. Under that model a lock is not merely unneeded, it is meaningless — there is no shared mutable state left for one to guard. See §19.1's own closing paragraphs for the full mechanism (`ports.CacheMount{Key, MountVersion, PublishVersion, Paths}`, `ports.BuildOutcome.PublishedCacheVersion`, the retention policy that replaces the old rotation-epoch config surface, the cross-tenant-provenance caveat that versioning does not change, and `internal/adapters/outbound/modal`'s decline-and-retry-cold implementation — now narrowed to never treat a bare client-side timeout as cache trouble). Build-duration and failure-rate instrumentation shipped alongside it, ungated — to size the win and catch a regression afterwards, the same role §19.5's telemetry now plays for (a)/(b) — as `app/imagebuild`'s `image_build_duration_seconds` histogram and `image_build_attempt_total` counter.

  Two cautions this split exists to prevent. Coupling (a) to (b)'s telemetry gate would hold an obviously-correct skip hostage to a measurement about something else. And (c) shares only a theme with (a)/(b), never code: (a)/(b) reduce work at **boot** in sandbox-agent, (c) reduces work at **build** behind a port — a perfect (a) and (b) leave every build failing exactly as before, and a perfect (c) leaves the rerun firing exactly as often.

Stays as-is, unaffected by this design: stash/pop machinery, `CloneAll`'s fresh-clone path, the boot-dispatch shape (`runBootSequence`), the builder's claim/backoff/streak machinery, the separate `contract_drift` mechanism (§14.3; unification remains its own future work).

## 20. Builder epistemic pre-action check (new capability)

Problem this solves: code review (§8.2) catches mistakes after a PR exists, but nothing today checks whether a substantial build-turn action itself rests on a shaky premise *before* the agent takes it — the review side's own after-the-fact scrutiny has no analogue on the builder side, pre-hoc. Left unaddressed, a builder turn that quietly assumes something false about the codebase, the task, or its own prior steps can burn a full session before anyone finds out.

### 20.1 Devil's-advocate preamble
Before a non-trivial build-turn action (never a planning turn, §20.3), the turn prompt is preceded by a short devil's-advocate preamble: it asks the agent to consider, in order, whether anything about the action rests on an unverified assumption, contradicts something already observed in the session, or is otherwise worth a second look — using an explicit two-tier taxonomy:
- **MINOR** — worth a heads-up in the reply, not worth stopping for; the agent proceeds and says what it noticed.
- **STRONG** — worth stopping for; the agent surfaces the concern instead of acting, and waits for the user.

The taxonomy is deliberately biased toward proceeding: default to STRONG only when genuinely warranted, MINOR for everything else worth mentioning, and silence for anything that doesn't rise to either — an epistemic check that flags routine work as a concern trains users to ignore it, defeating the entire mechanism. This bias is stated explicitly in the preamble text itself, not left to the model's own judgment of "how cautious to be."

### 20.2 The structured signal is not optional
Unlike a prompt-only self-check (which produces nothing a query can ever answer — "did this actually fire, how often, was it right"), the agent's response to the preamble additionally emits a **structured, typed field** — `EpistemicOutcome`: `none/minor/strong` — persisted on the turn row, in the **same Step** that ships the preamble itself, never as later follow-up work. This is a non-negotiable part of Step 61, not a nice-to-have: without it, the false-alarm-rate question this feature exists to eventually answer (does STRONG fire too often to be useful, too rarely to matter) is simply unmeasurable — exactly the kind of zero-observability gap §5.3's "day one, not later" principle exists to prevent.

### 20.3 Plan-mode turns are excluded
A turn running under `plan_mode=true` never gets the devil's-advocate preamble — plan mode's own HITL approval step (§8.1) already is the human review of the proposed action before anything executes; injecting a second, independent caution mechanism into a turn a human is about to approve anyway would just be noise duplicating a gate that already exists. The preamble applies only to non-planning build turns.

### 20.4 Threading and defaults
The enable/disable flag follows exactly the same threading `plan_mode` already uses: a global `platform.Config` default plus an optional override field on `SessionConfig`/`TurnSpec`, resolved with the same precedence (session override wins when set, global default otherwise) — no new config-resolution mechanism. **Off by default.** The signal is collected purely for analytics while the feature is calibrated (§21's analytics rollups are the natural home for an eventual false-alarm-rate view); there is no UI prominence beyond a subtle indicator surfaced in the review view (§12.2 item 2, Step 84) once shipped.

### 20.5 Hard-gate is explicitly out of scope
A hard gate — blocking the turn outright on a STRONG outcome rather than just surfacing it — is not designed here and not scheduled. It becomes a candidate only if and when the structured signal's own telemetry (§20.2) shows STRONG firing on genuine misses at a rate that justifies the cost of interrupting a session outright; until that evidence exists, gating on an unvalidated signal would risk blocking correct work on a false alarm, which is a worse failure than the one this feature exists to catch.

### 20.6 Phasing
Step 61, Phase 5. Independent of every other new Phase 5 Step — it extends only the turn prompt-assembly path and the turn row schema.

## 21. Review verdict persistence, analytics, digest & automated approval (new capability)

Problem this solves: posted review verdicts (§8.2) leave no durable, queryable history — there is no data source an analytics view or a digest could read from, only whatever is currently visible on each PR. Worse, the auto-approval mechanism §8.2/§16.1 originally specified is label-driven (`review: low risk`, posted by a human before anything automated can happen) — still a per-PR human bottleneck, exactly the serial chokepoint the decision inbox (§16) exists to relieve everywhere else. This section fixes both: an append-only verdict history feeding analytics and a deterministic digest (§21.1, §21.3), and a fully automated, criteria-driven replacement for the label gate (§21.2) that supersedes part of §8.2/§16.1's original design.

### 21.1 Verdict persistence & analytics read model
Every posted verdict (§8.2/Step 47's structured `domain/review` type, §8.2/Step 45) appends one row to `review_verdicts` — **append-only, one row per post**, never an update-in-place; the structured type means this is pure storage, never re-parsing anything out of posted comment text (the domain object already exists before it is posted). The latest verdict per PR is a `DISTINCT ON (repo, pr_number) ... ORDER BY created_at DESC` reduction (the same idiom already used for `sandbox_history`, §5.1) — no correlated subquery, no second "current verdict" table to keep in sync by convention.

Every row also carries **`head_sha`**: the commit the verdict was actually produced against — the same SHA the review session's own pre-fetched diff (§8.2/Step 46) was anchored to. **This is not "a value already in hand", and the scoping is the whole difficulty** — an earlier draft of this paragraph said it was, and Step 62's implementation found otherwise. Two things have to be true and neither is free. First, the SHA and the diff must come from **one** read: fetching the diff from an endpoint that always reflects the PR's *current* head, then reading the SHA separately, gives you two values that merely tend to agree — the verdict is then anchored to nothing. Pin the diff to the SHA (GitHub's compare-two-commits endpoint takes both) so the anchoring holds by construction. Second, the SHA must be scoped to **the turn that examined it**, never to a per-(repo, PR) column: §8.2/Step 46's session-reuse design means one session spans many turns over a PR's whole life, so any shared mutable column is overwritten by the next context fetch, and a verdict can be recorded against a commit it never read. That defeats §21.2's stale-verdict guard precisely — the guard compares the right two fields and still passes, because the stored SHA was never the examined one. This table is not yet built (Step 62), so the column costs nothing to add now; deferring it would mean a later migration plus a backfill for which no truth exists — nobody can recover, after the fact, which SHA a historical verdict actually examined. With it, verdict staleness becomes a **queryable fact** rather than an inference: the decision inbox (§16) can show how many commits have landed on a PR since its latest verdict was issued, by comparing this column against the PR's current head. §21.2 below states the auto-approval hazard this independently closes.

**Stacked PRs: review scope is the PR's own increment, never the cumulative stack diff.** When a PR carries GitHub's own stack context (§17.6) — today, only ever the origin+sentinel-fix pair §17 registers — a review verdict still covers exactly the diff against **that PR's own** base (its immediate parent in the stack, not the stack's ultimate base), with position, size, and the stack's ultimate base supplied to the review only as context, never as additional diff to verdict over. This falls directly out of `head_sha`'s own design above: a verdict is pinned to one commit of one PR, and a verdict computed over a cumulative multi-PR diff could not be honestly attributed to any single `head_sha`, nor tracked for staleness the same way — a change to a PR below it in the stack would invalidate a cumulative verdict with no mechanism here to detect that and re-trigger it. The accepted residual: a defect that only manifests in composition with the not-yet-merged PR(s) below a given PR in the stack may go unreported at this review level — the same class of gap §15's aggregate-diff review exists to catch for merged, released work, never for an open, in-progress stack.

Every query against this history is **bounded from day one** — an explicit active/recent window (configurable, default 30 days, mirroring the decision inbox's own `DecisionInboxLatencyWindow`, §16.2 — "a month of decision history, long enough for a stable median, bounded" — deliberately NOT the inbox's much narrower 48h item-staleness flag, §16.1: a different concept entirely, how long a single queue row is allowed to sit before it is visually flagged, not how far back a rollup query reaches), never an unbounded scan; an unbounded "all PRs ever needing review" query is a design mistake to avoid at the schema stage, not a scaling problem to fix later.

Analytics rollups (timeseries, top-risk-driver breakdown, the "Review finding outcomes" KPI, §12.2 item 6) read from this same history. Any rollup not yet computed for a given window returns an explicit **"not yet computed" sentinel**, distinct from a real zero — a repo with a real 0% dismiss rate and a repo with no data yet must never render identically.

**`FilesChanged` is the reviewer's own count, and a second, server-computed count of the same quantity exists beside it.** The verdict's `FilesChanged` (§8.2/Step 45's structured type) is *self-reported by the reviewing agent*: descriptive data this history stores and the rollups above read, never checked against anything on its own account. Step 68's `ChangedFilesCount` (§26.3) is the **same quantity computed server-side**, off the `changed_files` integer GitHub already returns on the `GetPullRequest` call the review-context fetch makes anyway; it is deliberately never rendered into the prompt (mirroring `head_sha`'s own contract) and exists primarily to feed `reviewtriage`'s light/deep routing. So two numbers stand for one fact, with opposite provenance — and, since this Step, they are compared: the divergence is the cheapest signal available anywhere in this design that a verdict was produced against a diff its reviewer did not actually read in full, and the one derivable fact that would catch it no longer goes unused. `ChangedFilesCount` itself is resolved at turn-CREATION time (`review.PreFetchedContext`, above); reaching verdict-POST time, where `FilesChanged` finally becomes known, requires it to survive that gap, so it rides `reviewtriage.DecisionRecord` (§26.3's own routing-decision record, already persisted verbatim onto `turns.review_depth_decision` for every review turn) as one further field, read back exactly where `head_sha`/`review_depth` themselves already are (`httpapi.PostReviewVerdict`) — the SAME turn-scoped carrier this section's own `head_sha` paragraph above requires for the identical reason: a value computed once, at creation, must reach the ONE turn that computed it, never a later or unrelated one that happens to share the same session.

Three constraints are load-bearing on the comparison this wires (`reviewverdict.FilesChangedDrifted`) — the third added by adversarial-review hardening after this Step first shipped — and none is self-evident. It stays **diagnostic only** — never rewriting the verdict, never moving a risk level, never failing a request — because the self-reported number is what a human reads, and silently correcting it would destroy the very evidence the signal exists to surface; the function itself returns a plain boolean, with no verdict or `Shippable` parameter anywhere in its signature, so its one real caller has nothing to feed a fired result back into even by mistake — it only ever logs. And it tolerates `ChangedFilesCount == 0`, which §26.3 documents as **indistinguishable from a genuinely empty diff** whenever `GetPullRequest` fails: a non-positive server-computed count returns `false` unconditionally, before either threshold below is even evaluated, so a canary that read zero as truth would fire on every transient GitHub fault — and a canary that cries wolf is one its readers learn to ignore, strictly worse than not having built it. The canary additionally requires BOTH a ratio (`FilesChangedDriftRatioThreshold`, 50%) and an absolute-count (`FilesChangedDriftAbsoluteThreshold`, 5 files) divergence at once — named, documented constants, never a literal buried at the comparison site — because neither threshold alone is trustworthy: a ratio alone is noisy on a small PR (a one-file difference already reads as 100% off), and an absolute count alone is noisy on a large one (a handful of files out of several hundred is ordinary counting slop, not a signal worth a human's attention). And it tolerates the diff itself never having been fully delivered to the reviewing agent — the exact symmetric case of the `serverComputed <= 0` guard above: `GetPullRequest` (which resolves `ChangedFilesCount`) and `GetCompareDiff` (which resolves the diff the agent is actually shown) are independent calls, so the first can succeed while the second fails or truncates, and an agent handed no diff, or only a partial one with an explicit truncation notice, can only ever report what it saw. `reviewtriage.DecisionRecord` carries `DiffEmpty`/`DiffTruncated` alongside `ChangedFilesCount` for exactly this reason, and the canary is suppressed, never fired, whenever either is true (or unknown) — a divergence caused by the server's own delivery failure must never read as evidence the reviewer skipped something.

### 21.2 Automated approval: eligibility engine + calibrated auto-merge
This section **supersedes** the label-driven auto-approval mechanism §8.2 and §16.1 originally specified (`review: low risk` as the trigger a human posts to approve a PR for auto-merge). That design was a per-PR human bottleneck — it still required a person to read the PR and apply the label before anything automated could happen, exactly the serial-human chokepoint the decision inbox (§16) exists to relieve elsewhere. The replacement is fully automated from day one, in two decoupled stages:

**Stage 1 — auto-approval (always on).** A PR becomes auto-approved when `Shippable == auto` (Step 45's server-computed field — never the model's self-report) **and** every one of a deterministic, server-checked eligibility list holds:
- CI green at head.
- No floor raised: none of the now-four raise-only floors that compose into `Shippable` (`max(rank)` composition, `review.ComputeShippable`) — coverage and premise (§8.2/Step 45), description adequacy (§26.2/Step 67), and counter-review (§26.4/Step 69) — is above its baseline. Note this one is **subsumed by `Shippable == auto` above and needs no separate check**: `Shippable` is the `max` of the baseline and all four floors' ranks, with `auto` at rank 0, so `auto` is only reachable when all five inputs (the baseline plus the four floors) are already at rank 0. It stays listed here because it is a criterion of the policy, not because it is a second code path.
- Diff size under a configurable-per-repo threshold. **Diff size means changed-file count**, matching the configurable field's own name — not added+deleted lines. (Stated because the earlier wording was ambiguous and the threshold is user-facing.)
- No sensitive path touched — a configurable-per-repo list (migrations, auth code, `/contracts` by default, extensible per repo).

**These last two are computed from the SERVER's own view of the diff — never from the verdict.** The reviewing model reports a file count and a blast radius alongside its verdict; both are display data and neither may gate anything. This is the same "never trust the model's own verdict" rule the `Shippable` recomputation already applies, and it is stated separately here because Step 62's first implementation applied it to `Shippable` and not to the two criteria layered on top — a review agent could then self-report a one-file, empty-blast-radius verdict for a three-hundred-file PR rewriting migrations and auth, and be merged unattended. The server-fetched changed-path list and file count are the only admissible inputs.

**Cross-step dependency, stated because it is not obvious:** turning changed paths into the `BlastRadius` tags this criterion matches on requires a path→tag classifier, and the design for one lives in §26.3 (Step 68) — six Steps later. Step 62 therefore ships its own deliberately over-inclusive interim classifier so the criterion is genuinely enforceable when the engine first exists. **Step 68 does not supersede that classifier for this criterion.** Step 68's own per-repo-configurable globs (`Config.DeepPaths`, §26.3) are a separate mechanism, scoped to review-DEPTH triage only, that WRAPS this same interim classifier as one of several triage signals rather than replacing it (`reviewtriage.classifySensitivePaths` calls straight through to it). This eligibility engine's own "no sensitive path touched" check still runs Step 62's classifier unchanged, now with a per-repo-configurable sensitive-tag list (`repo_settings.sensitive_blast_radius_tags`) layered on top of it — never Step 68's raw glob patterns, which this engine never reads. Without the Step 62 classifier, this criterion cannot be honestly implemented at Step 62 at all, and the temptation is to fall back on what the model reported — which is exactly the failure above.
- The verdict being relied on was produced against the PR's CURRENT head SHA (`review_verdicts.head_sha`, §21.1) — a verdict computed against an earlier commit is stale by definition and must never itself satisfy eligibility, no matter how low-risk it once looked. This closes an auto-approval hazard independent of everything else in this list: approving on the strength of a verdict that examined DIFFERENT code. §24's automatic re-review, when a repo opts in, keeps this current proactively — but this check holds on its own regardless of whether that automation is enabled for the repo at all.

No per-PR human label is required or consulted for this decision — the LLM's verdict only ever *proposes* `Shippable`; the server recomputes it and checks every criterion above independently, the same "never trust the model's own verdict" discipline §18.1's `FallbackReason` and `domain/sandbox`'s decision functions already apply elsewhere. The existing `review: low risk` label **inverts into an escape hatch**: replaced by `review: needs-human`, which forces a specific PR out of auto-approval regardless of what the criteria say — a maintainer who knows something the criteria can't see still has a lever, just an opt-out one instead of an opt-in gate. (`visual-qa: pass/skip` is unrelated to this change and continues exactly as before.)

**Stage 2 — auto-merge (per-repo toggle, off by default).** Auto-approval alone does not merge anything. While a repo's auto-merge toggle is off (the default, and the state every repo starts in during a calibration period), an auto-approved PR surfaces in the decision inbox (§16.1's `ready_to_merge` row) as "ready to merge (auto)" with a 1-click human confirm — the human step moves from "decide if this is low-risk" to "confirm the machine's decision," a materially cheaper ask, but not a removed one. Every auto-approval outcome — confirmed as-is, or contested (a human overrides/requests changes on a PR the engine approved) — accumulates into a **contradiction-rate read model**: the fraction of auto-approved PRs a human later disagreed with, per repo. An admin arms the auto-merge toggle for a repo only once this data justifies it; the toggle's own settings row displays the reliability stats (§12.2 item 5, Step 86) next to the control, so arming it is an informed decision, not a leap of faith.

Once armed, auto-merge reuses the decision inbox's **existing** server-side re-validation-at-click contract unchanged (§16.2, Step 60: re-check CI, approval state, `Authorize` before calling the SCM) — merging is simply machine-initiated instead of human-clicked, the same checks either way. This is a deliberate reuse, not a parallel merge path: the inbox's Merge endpoint was already built to never trust its own rendered queue as authority, exactly the property an unattended merge needs.

### 21.3 Deterministic digest
A daily digest is **entirely deterministic, never LLM-narrated** — it renders from the same `review_verdicts`/analytics read model above via a template, not a model call; a digest is a compliance/status artifact, and a fixed rendering is easier to trust and to test than a fresh narration every day. Scope is **per-repo/per-channel from day one**, built entirely from EXISTING session-thread association tables (`slack_thread_sessions`/`linear_agent_sessions`, joined through `github_pr_sessions`) rather than inventing a second, separate repo↔channel mechanism: every Slack channel or Linear organization that has recently threaded a review session for a repo receives that repo's own digest — a **repo-level fan-out to every such channel**, not the decision inbox's own per-person, identity-graph-backed assignment scoping (§16.2's CODEOWNERS-through-the-identity-graph provenance) — that per-user scoping is **not built** for the digest; a channel's digest shows every repo it has recently discussed, not what any one person's own inbox would show. Sending is **claim-before-act per (date, channel)**: a `digest_send_state(date, channel)` row plus `SELECT ... FOR UPDATE SKIP LOCKED` (the same idiom §5.1 already uses for PR-mention coalescing) guarantees at-most-one send per channel per day even with concurrent ticks — no separate storage-layer serialization mechanism needed, Postgres already does this.

### 21.4 Phasing
Step 62, Phase 5, after Step 45 (verdict shape) and Step 47 (posting path) — designing the verdict schema once, before any of persistence/analytics/digest/auto-approval builds on it, avoids the parallel-reinvention trap a shared schema exists to prevent. UI: Settings → Analytics gains the review-risk section and the per-repo auto-merge toggle with calibration stats (§12.2 items 5-6, Step 86); the decision inbox's `ready_to_merge` row (§16.1, Step 87) gains the "(auto)" 1-click-confirm variant.

## 22. Learned false-positive patterns & rebuttal identity (new capability)

Problem this solves: two independent frictions compound in code review (§8.2) today. A finding's rebuttal is tracked by file:line, so an unrelated edit that merely shifts a line number makes an already-rebutted finding look brand new on the very next review pass, forcing a maintainer to re-argue the same dismissal indefinitely. And a maintainer who teaches a reviewer "that's not actually a problem in this repo" has no way to make it stick — the same false positive fires again on the next PR, because nothing captures what was learned. This section fixes both: content-based rebuttal identity, which applies to every review finding and not just the sentinel-auto-fix surface (§17) it's adjacent to (§22.1), and a repo-scoped table of maintainer-taught false-positive patterns with its own capture/injection/retirement lifecycle (§22.2-§22.4).

### 22.1 Rebuttal identity by content, not position
A finding's rebuttal (§12.2 item 2's Dismiss-with-rebuttal) is reconciled against the finding's own **persisted content** — a hash/text of the finding stored at the moment the verdict that raised it was posted (§8.2/Step 45's structured type already carries this data; storing it is not new capture, just retention) — **never by file:line alone**. A file:line-only identity breaks the moment a line shifts (an unrelated edit above it, a reformat) — the same finding then silently reads as a *new* one, and a human's already-given rebuttal is lost on the very next review pass. Content-based identity survives exactly the churn that makes file:line fragile.

### 22.1.1 Content-anchored positioning: snippet match and the relocation fallback
Refines §22.1 — folded directly into Step 63's own scope, not a follow-up Step. §22.1's stored content (the finding's own hash/text, captured once at post time) answers *identity*: is this the same finding as before. It says nothing about *position*: where the finding should be drawn on a PR whose diff has since moved. This is a distinct problem with independent production validation: OpenCodeReview (github.com/alibaba/open-code-review) — Alibaba's line-level AI code-review CLI, open-sourced after roughly two years of internal production use, ~20k★, Apache-2.0; verified directly against its own repository, fetched 2026-08-11 — computes a finding's `start_line`/`end_line` with a **sliding-window match** of the finding's own quoted `existing_code` snippet against the diff text, falls back to a dedicated small **re-location** model call (`RE_LOCATION_TASK`, `internal/diff/relocation.go`) when the match fails, and resolves both together in a second pass (`diff.ResolveLineNumbers`) after its main review loop.

`reviewpost.Finding` gains the equivalent shape — computed server-side, never by the model directly, the same never-trust-the-model-for-a-derivable-fact discipline §18.1's `FallbackReason` and §21.2's recomputed `Shippable` already apply elsewhere:

- **One field, two consumers.** The snippet §22.1 already mandates storing IS the anchor text — never a second, parallel capture of the same content. Its hash answers identity (§22.1); the same text, sliding-window-matched against the diff, answers position (here).
- **The match is a pure function**, `(filePath, snippet, diff) → (StartLine, EndLine)`, run by `reviewpost` at posting/render time — the same server-side-rendering posture §26.1 already establishes for this package ("rendering is server-side from the typed fields (`reviewpost`)"), not a second copy of the diff shipped back into a model prompt to ask it where its own finding lives. `filePath` scopes candidate lines to the finding's own file on **both** anchoring paths — the pure match and the relocation fallback alike: a finding rendered against the wrong *file* is the same "worse than none" failure as the wrong *line* the paragraph below already guards against, so file-scoping is a property of the anchoring mechanism as a whole, not an implementation detail local to one path.
- **A failed match sets both to `0` — an explicit, typed "unanchored," never a guessed line number.** A finding rendered against the *wrong* line is worse than one rendered with none: `0` is a UI-branchable fact ("position not found"), not a plausible-looking wrong answer a maintainer might act on directly.
- **Relocation fallback reuses the existing `LLM` port (§4.3), never a new call path.** A failed match triggers one small, structured, non-agentic call through the same multi-provider-by-construction port the intent classifier already requires (§18.1) — not a review-session turn, not §7.1's sub-task fan-out: a utility call with no tool access, the same class of call as classification, not review. On failure (provider error, timeout — the same typed-error discipline §18.1 requires of this port), the finding stays unanchored (`0`, per above), never a second guess stacked on the first. Gating relocation behind a size threshold (skip it on trivially small diffs where a failed match is rare) is a plausible later refinement if telemetry shows the call volume unwelcome — not designed now, and deliberately not reusing §26.3's PR-level triage threshold, which governs a different-scoped decision (whole-PR routing, not one finding's anchor). Step 63's own implementation measured the match rate against realistic prose findings (not the verbatim-code-quote snippets a naive test fixture would use) at roughly 0.11-0.36 similarity, against a threshold requiring roughly 0.4 to count as matched — i.e. the pure match is expected to fail, and the relocation fallback to fire, for most ordinary findings. Any future tuning of the match threshold or the relocation-gating decision above should start from that measured range rather than an assumption that matches are the common case.
- **No second pass, by construction — a second simplification, not a corner cut.** OpenCodeReview's own second pass exists because its findings surface one at a time from a live, iterative loop, so an early finding's position can resolve before a later one supplies information that would change it. A Narvi verdict posts once, as a single typed payload with every finding already present (Step 45's structured-verdict invariant) — there is no "later": each finding's position is a pure function of its own snippet and the one diff already in hand, resolved once, together, at render time.

**No migration, because there is nothing to migrate.** Step 63 has not shipped; no finding has ever been identified by file:line, in storage, that this would need to reconcile against. This is a real simplification the unbuilt state of this plan buys outright — the anchoring fields ship as part of Step 63's original schema, never as a later `ALTER` plus a backfill against data that predates them.

### 22.1.2 What already-answered identity is used for — and what it is not
§22.1's content-based identity answers *is this the same finding*. This subsection records what the system then **does** with that answer, because the mechanism downstream is weaker than the identity work in front of it implies, and that weakness is invisible from §22.1 alone.

Findings already reported and reconciled on a prior pass are re-supplied to the next pass as a **delimited, explicitly-labelled DATA block** (`reviewpost.RenderAlreadyAnsweredFacts`) — each carrying its file path, description, status, a short prefix of its identity hash, and, for a rebutted finding, the maintainer's own rebuttal text — under the same untrusted-content discipline §5.2 and §22.3 apply everywhere else. The instruction accompanying it is that the reviewer must not re-report any of them unless the underlying issue has *materially* changed, with paraphrase, reformat, and shifted line number each named explicitly as **not** material. A finding whose own anchoring file has left the current diff entirely renders inside that same block with an explicit **RETIRED** annotation (below) rather than the plain "already answered" framing — still one more fact in the DATA block, never a second instruction with teeth of its own.

**For a finding whose file is still in scope, that instruction is the entire enforcement, and it stays social rather than structural.** Nothing downstream compares a newly-posted finding against the already-answered set and drops a match on *resemblance* grounds: a reviewer that re-raises a settled finding on a file still part of the diff posts a verdict that carries it, and a maintainer re-argues precisely the dismissal §22.1's identity work exists to make permanent. This follows deliberately from §22.3's advisory-never-a-filter posture — the reasoning that forbids silently pre-filtering findings against a learned pattern forbids silently dropping one here on a resemblance judgement — but it is a **materially weaker guarantee than "rebuttals stick"** for that case, and it is written down so the limitation is known and argued rather than discovered later by someone who reasonably assumed §22.1 bought more than it does.

**The one refinement that *is* structural, now shipped, stays on the right side of §22.3 by construction:** retiring a finding whose anchoring code has **left the diff entirely** is a determinable fact about the diff, not a judgement about the finding's own content — `reviewpost.RenderAlreadyAnsweredFacts` is additionally given the current diff's own changed-path list (`reviewtriage.ExtractChangedPaths`, §26.3, already computed for triage and threaded through unchanged) and marks any already-answered finding whose file path is no longer among them RETIRED, directly inside the same rendered block. There is no similarity threshold anywhere in this check and no comparison of one finding's content against another's — only a finding's own file path against the diff's own current membership, which is exactly the determinable fact this subsection draws the line at. Retirement is a **note, never a silent drop**: a retired finding is still rendered, in full, inside the block — §22.3's advisory-never-a-filter posture governs this refinement exactly as it governs everything else here, and the underlying `review_findings` row and its own persisted lifecycle status are untouched by it, so retirement changes only what the next reviewing pass is *told*, never the record itself. The correct framing is that a finding is **re-anchored to the current diff**, never that anything out of diff is ignored: the file may have left the diff because the underlying problem was genuinely fixed, or because a rebase/force-push moved it into the base branch — either way it is no longer this PR's own diff to carry forward as live, and the annotation says so without deleting anything. When the caller has no reliable diff data at all (a failed or never-attempted fetch — indistinguishable, by design, from a genuinely empty diff, mirroring §21.1's own `ChangedFilesCount == 0` ambiguity), retirement is skipped entirely and every finding renders exactly as it did before this refinement shipped, never guessed at. Suppressing a finding because it *resembles* an already-answered one remains a judgement, and routing it through a silent drop remains the exact failure §22.3 rejects — no similarity threshold makes that safe, and this refinement introduces none.

**Adversarial-review hardening (post-shipment): the changed-path list is authoritative-or-absent, never partial.** "Absent from `ChangedPaths`" is not, by itself, the determinable fact this subsection licenses retirement on — three gaps between the two let a LIVE finding retire silently, closed together on the same governing principle: `reviewcontext.Fetch`'s own diff fetch is capped in size and can return a byte *prefix* with `DiffTruncated=true`; a `ChangedPaths` built from that prefix is genuine but incomplete, and is now treated identically to no data at all — retirement is skipped whenever the diff was truncated, not just when the fetch failed outright. Second, `reviewtriage.ExtractChangedPaths` now recognizes the diff-header shapes that used to contribute no path (a binary file's own "Binary files ... differ" section, a mode-only change, a git-C-style-quoted non-ASCII path), closing the same class of false-negative gap the prior rename/deletion fix already closed for this function. Third, a finding's own self-reported `FilePath` and `ChangedPaths`' own git-derived vocabulary are reconciled before comparison (stripping a diff-header `a/`/`b/` prefix, a leading `/`, and normalizing separators) — and where reconciliation still cannot confidently resolve membership either way (a case-only difference, ambiguous on a case-insensitive filesystem), retirement is withheld rather than guessed, the identical fail-safe posture `reviewpost.MatchPosition` (§22.1.1) already applies to its own file-path comparison.

### 22.2 Repo-scoped learned patterns
A repo-scoped table holds maintainer-taught false-positive descriptions: free-text patterns a maintainer teaches once, so the same class of non-issue doesn't need re-litigating on every subsequent review. Capture is via an explicit `false positive: <reason>` command on a PR thread, **dispatch-before-router** — it reuses the existing `Authorize` write-permission gate (§13.3, Step 39) directly, the same check any other state-changing actor command goes through, rather than inventing a parallel permission model for this one command.

**RBAC**: `maintainer+`, direct, immediate effect — no propose/validate intermediate flow for a `member` role. This maps the command onto §13.3's existing role matrix as-is (review verdict edits and re-triggers are already a maintainer+ action) rather than adding a new permission tier.

Upsert is **idempotent, keyed on the triggering comment id** — the same `ON CONFLICT`-with-lifecycle-preservation idiom already used for webhook dedupe (§5.1) and PR-mention coalescing, applied here so a redelivered webhook or a retried command can never double-insert the same pattern.

### 22.3 Advisory injection, never a filter
Learned patterns are injected into every review pass (first pass and re-review alike) as an **explicitly-untrusted, advisory content block** — "weigh this, verify independently, do not skip a legitimate finding on this basis alone" — following the same untrusted-external-content discipline §5.2 already requires of PR diffs. This is deliberately never a pre-filter that silently drops findings matching a pattern: a maintainer-taught pattern is a hint the reviewing agent must reason about, not a rule the pipeline blindly obeys — a wrong pattern (taught once, stale since) should be *outweighed* by a clearly legitimate finding, not used to suppress it outright.

### 22.4 Lifecycle ships in the same Step
Retire, hit-count, and an audit view for this table **ship in the same Step** as the capture mechanism — never a deferred follow-up. A learned-pattern table with no retirement path only ever grows, accumulating stale or wrong patterns with no mechanism to review or remove them; shipping capture without a lifecycle would create exactly that unreviewable, ever-growing state from day one.

### 22.5 Phasing
Step 63, Phase 5, after Step 47 (needs the verdict-posting path new patterns get weighed against) and Step 39 (`Authorize`, RBAC). §22.1.1's relocation fallback additionally depends on the `LLM` port (§4.3), already required and shipped for the intent classifier (Step 36) — no new port, one new call site on an existing one. UI: Settings → Environments gains false-positive pattern view/retire per repo (maintainer+, §12.2 item 5, Step 86); finding cards gain rebuttal history with the content-based finding-identity linkage; since §22.1.1's anchoring fields are deliberately ephemeral (never persisted — position is a pure function of the snippet and diff, and a stored position would go stale the moment the diff moves), Step 84 re-resolves each finding's position at read time by refetching the diff at the verdict's own persisted `review_verdicts.head_sha` (§21.2, Step 62) and re-running §22.1.1's match, rendering at the resolved position or explicitly flagged unanchored if the re-resolution also fails — never a stored, potentially-stale line number (§12.2 item 2, Step 84).

## 23. Plan follow-up classification (amend vs answer) (new capability)

Problem this solves: the shipped plan-mode dispatch path (Steps 37-38) originally dispatched any reply that matched no approve/reject keyword as an ordinary build turn (`planMode: false`) while the plan was still `awaiting_approval` — silently starting a build against the *unapproved* plan's assumptions. An interim server-side gate closes the dangerous half of that gap deterministically: such a reply is held (nothing dispatched, honest clarification reply), and a `revise:`-prefixed reply creates a plan-revision turn. What the interim gate cannot do is understand an *unprefixed* natural reply — a clarifying question still gets the hold-and-clarify treatment even when its intent was obvious, and a revision request must remember the prefix. This section replaces the prefix requirement with a real amend-vs-answer classification, extending §8.1 (plan mode) and §18 (unified intent classifier); the deterministic `revise:` prefix stays as an override that bypasses classification entirely.

### 23.1 A new classifier surface
`plan_followup` is a new surface on the existing unified intent classifier (§18) — amend-vs-answer, alongside the classifier's other categories (review-vs-request, plan-vs-build, release-vs-feature, §18.6). Same never-throw contract, same confidence rubric (§18.1/§18.2) — no parallel classification mechanism invented for this one case. There is a **single call site**, gated on "a plan exists and is `awaiting_approval`" — the classifier is never invoked for this purpose outside that state.

### 23.2 Enforcement at the persisted-state layer
The classification result is persisted as an `answer_only` flag on the turn/message row and **consulted at the turn-creation chokepoint** before any dispatch decision is made — never by trusting the sandbox/runtime to self-enforce which mode a turn runs in. Concretely, this is `httpapi.createTurnLocked` (the single shared core every ingress path — REST, Slack, Linear, GitHub bot — already calls through), the same chokepoint the interim awaiting-plan gate (§23 intro) already occupies; a plan-save path runs too late to gate dispatch, since a turn is already created and dispatched by the time any plan-save logic runs. This is the same "Postgres single source of truth" discipline (§5.1, CLAUDE.md) applied to a new case: the state that governs dispatch lives in the database that chokepoint already checks, not in a runtime flag a client or sandbox could misreport.

### 23.3 Fail-open direction: wait for clarification, never silent dispatch
When the classifier fails or returns low confidence while a plan is `awaiting_approval`, the fallback is **wait-for-clarification** — nothing is dispatched, the plan stays `awaiting_approval`, and the reply is an honest prompt asking the user to approve, reject, or clarify — i.e. exactly the interim gate's own hold-and-clarify behavior, which remains the floor this feature can never fall below. A classifier failure therefore degrades to the pre-classifier experience, never past it: no build turn can fire against an unapproved plan under any failure mode, at the cost of occasionally asking a user to repeat themselves when the classifier was merely unconfident, not wrong.

### 23.4 Never a public surface
Like every internal classification surface, `plan_followup` is excluded from public routes **by construction** — private to `app/`, never registered on `httpapi`/`wshub` (§18.6, mirrored here as the second surface to follow this rule; it is not merely a convention to remember but a structural property of where the code lives).

### 23.5 Phasing
Step 64, Phase 5, after Step 36 (classifier) and Steps 37-38 (plan mode, the dispatch point this amends). No UI change — the effect is entirely in the dispatch/reply path.

## 24. Automatic re-review on new commits (new capability)

Problem this solves: review sessions (§8.2/Step 46) re-trigger only when a human deliberately applies the label or clicks the button — a maintainer has to notice a PR moved and act. Left as the only path, commits pushed after a verdict was posted (an addressed finding, an unrelated follow-up commit, a force-push) can sit unreviewed indefinitely; nobody is notified there is anything to notice. This section adds a second, automatic trigger alongside the existing manual one — never replacing it — driven by the PR's own commit history rather than a human remembering to look.

### 24.1 A new ingress lane, not a small extension
This is a genuinely new GitHub ingress lane, not a small addition to the existing one, and it is costed out as such. The existing GitHub ingress (Step 32, `internal/adapters/inbound/github`) handles exactly two webhook event types — `issue_comment` and `pull_request_review_comment` — and both exist to detect a PR @mention; neither carries, and neither is ever asked to carry, a new-commits ("push more code to this PR") signal. Nothing in this codebase today parses GitHub's `pull_request` event at all. Closing this gap costs three concrete things:
- **A new webhook event type**: `X-GitHub-Event: pull_request` with `action: "synchronize"` (GitHub's own name for "new commits landed on this PR's head"), parsed into a new payload shape carrying `repository.full_name`, `pull_request.number`, and `pull_request.head.sha` — none of which the existing `issueCommentPayload`/`pullRequestReviewCommentPayload` structs (payload.go) need, since neither carries a head SHA today. Every other `action` value this event type carries (`opened`, `closed`, `labeled`, …) is acknowledged and ignored, mirroring today's `action != "created"` gate for comments.
- **The same claim/release delivery handling the existing webhook toolkit already provides** (Step 31): claimed via `postgres.WebhookDeliveryStore.Claim(provider="github", deliveryID=X-GitHub-Delivery)` on the same route, the same atomic first-writer-wins dedupe every other GitHub event already gets; a delivery this handler claims but fails to process releases that claim via the same `Release` method handler.go already calls today on a parse failure, so a human-triggered GitHub redelivery can reprocess it — no new dedupe primitive invented.
- **Per-PR routing onto the existing coalescing identity, not a new mapping table.** `github_pr_sessions` (Step 32, keyed on `(repo_full_name, pr_number)`) already IS "the one review session for this PR." A `synchronize` event looks itself up there; no row, or a row with `session_id` still NULL (nobody has ever mentioned the bot on this PR), means there is no review session to re-trigger — acknowledged, untouched, exactly like today's "no mention → acknowledged 200, no session created" case for comments. A row with a session id is the PR this event debounces a re-review for.
- **A direct, actor-bypassing write of both the debounce timer and `pending_retrigger_head_sha`, in one transaction.** Every existing named timer is armed exclusively via `armTimer`, an unexported `*Actor` method (`timerfired.go`) that requires an already-open, actor-owned `pgx.Tx`, and `command.go`'s `Command` sum type has exactly three members (`TimerFired`, `SandboxEvent`, `EnsureDispatched`) — none of which represents an inbound "new commits pushed" signal an HTTP-layer webhook handler could hand into the actor's mailbox. The `synchronize` handler therefore arms the timer the same way `coalesce.go` already writes `github_pr_sessions` directly today, bypassing the actor entirely: via the exported `postgres.TimerStore.Upsert`, in the SAME transaction as the `pending_retrigger_head_sha` upsert (§24.2) into `github_pr_sessions`. Both commit atomically as one unit or neither does, so a crash between them cannot leave a pushed commit with no armed timer.

This event carries no comment body and no commenting actor at all, so the existing mention-detection step (doc.go's step 5: detecting whether the comment body actually mentions `Config.BotHandle`) does not apply here — routing is by `(repo, PR)` identity alone, never by text.

### 24.2 Trailing-edge debounce, via the existing timer primitive
A burst of pushes (a rebase, a sequence of small fixup commits) must review once, at the burst's own final head, after a quiet period — not once per push, and not the first push in the burst. This means debouncing on the **trailing** edge, not the leading one: leading-edge throttling marks the moment of the FIRST push and then silently drops every later push inside the window, which recreates exactly the problem this feature exists to solve (unreviewed commits) at the window's own edge — the very last, most current push in a burst would be the one never reviewed.

This is built entirely on the session timer mechanism §2 already establishes (`session_timers(session_id, name, fires_at)`), never a new timer subsystem: each `synchronize` event that resolves to a real review session re-arms one named timer on that session (`review_retrigger_debounce`, a new entry in the existing `name` list — itself documentation of examples, not a closed set) to `now() + ReviewRetriggerDebounce` (a new `platform/timeouts.go` entry, alongside every other named interval this codebase defines). Re-arming a named timer is already an upsert (`UNIQUE(session_id, name)`) — the SAME idiom every one of the 5 existing named timers already uses to re-arm on new activity — so a second push before the first debounce fires simply pushes `fires_at` further out; only the LAST push in a burst survives to actually fire. No new debounce mechanism is invented, but the ARM and the FIRE happen through two different paths, unlike the other 5 named timers: the ARM is a direct write from the `synchronize` webhook handler (§24.1's fourth cost item), not something delivered through the actor's mailbox, since nothing in `Command` (`command.go`) carries that inbound signal; only the timer's later FIRING travels through the existing timer pump (§2) into the actor as a `TimerFired` command exactly like every other named timer already works (§24.3).

`github_pr_sessions` (already the per-PR identity this feature routes through, §24.1) gains one new nullable column, `pending_retrigger_head_sha`: the most recent `synchronize` event's own reported head SHA, upserted (overwritten, not appended) on every event for that PR. This is what the actor reads back when the debounce timer finally fires — the burst's own last-known head, not a value that has to be independently re-fetched from GitHub.

### 24.3 Trigger state is `review_verdicts.head_sha`, never a label
When `review_retrigger_debounce` fires and reaches the review session's actor (hydrating it on demand if idle, §2, exactly like any other timer-delivered command), the actor:
1. Re-reads the per-repo opt-in setting (§24.5) — if it cannot be read, or is off, the timer is simply dropped: no re-review, logged, fail closed.
2. Reads `github_pr_sessions.pending_retrigger_head_sha` for this PR and the latest posted verdict for the same PR — the `DISTINCT ON (repo, pr_number) ... ORDER BY created_at DESC` reduction §21.1 already defines — and compares `pending_retrigger_head_sha` against that verdict's own `head_sha` (§21.1's new column).
3. If they already match (a race where a manual re-trigger, or an earlier automatic one, already reviewed this exact SHA), there is nothing to do: clear `pending_retrigger_head_sha`, delete the timer, done.
4. Otherwise (§24.6's budget check, below), enqueues a new turn on the review session, then clears `pending_retrigger_head_sha` and deletes the `review_retrigger_debounce` timer — the same re-arm-or-delete contract every named-timer handler in this codebase already follows (`timerfired.go`'s own hard rule: a handler that leaves the claimed timer row untouched lets the claim window expire and silently redelivers the same `TimerFired` command forever). This step cannot literally be the actor calling `httpapi.CreateTurnForBot`, Step 46's own manual re-trigger path: `internal/app/sessionactor` cannot import `internal/adapters/inbound/httpapi` (httpapi already imports sessionactor throughout its bot/create/turn/plan files; the reverse would be a compile-time import cycle), and `createTurnLocked`, the function `CreateTurnForBot` itself wraps, is unexported besides. The actor instead inserts the turn directly via `a.stores.turn.Create` — the same store-level primitive `createTurnLocked` itself calls — inside the transaction this handler already has open, mirroring Step 46's manual path at the storage layer rather than calling through it. Whether the small amount of logic `createTurnLocked` wraps around that insert (the audit-log write, the awaiting-plan gate) needs duplicating here, or is inapplicable to a review session's automatic turn, is Step 65's own implementation decision, not resolved by this plan.

Trigger state is therefore a comparison of two Postgres columns (`github_pr_sessions.pending_retrigger_head_sha` vs. `review_verdicts.head_sha`) — never a label read back off the pull request. §5.1 states why this matters as a general principle: a bot-written label is mutable by anyone with triage rights, forgeable, and a second copy of a fact Postgres already owns; this feature's own trigger state must live where every other piece of durable session state already lives.

### 24.4 Resolving the apparent contradiction with Step 46
Step 46 already specifies re-trigger "via label/button" — a human applying that label, or clicking that button, is a **deliberate, in-the-moment command**, identical in kind to clicking any other action button in this UI, and this section changes nothing about it: it stays exactly as specified, dispatches immediately, still `maintainer+` direct (§13.3). What §5.1's principle rejects is a DIFFERENT thing this section could have been tempted to build instead — the BOT itself writing a label back onto the PR (e.g. a hypothetical "stale, needs re-review" label) and then reading that same label back later as the durable signal that a re-review is owed. That would be state the bot wrote for itself to read, on a surface anyone with triage rights can edit or remove, duplicating a fact (§21.1's `head_sha`) Postgres already owns. The distinction is general, not specific to this feature: a label is a legitimate channel for a HUMAN's command, never a legitimate store for the SYSTEM's own memory.

### 24.5 Off by default, per-repo opt-in, fails closed
This entire feature is **off by default**, enabled per repository (Settings, admin-only — the same row as the auto-merge and sentinel-auto-fix toggles, §13.3), for the same reason those two are admin-gated: it changes what runs unattended on a repo's own PRs. If the setting cannot be read (a transient Postgres error, a missing row), the safe direction is treated as OFF — no re-review is triggered, logged, nothing retried; a repo that hasn't explicitly opted in never gets surprised by this running anyway. **This automation never auto-approves anything on its own** — it only ever enqueues an ordinary review turn through Step 46's existing dispatch; whether the resulting verdict lets a PR through auto-approval is entirely §21.2's own eligibility engine's decision, made independently, later, from the fresh verdict this produces.

### 24.6 Per-PR re-review budget
An automated fix session (§17, sentinel auto-fix, or any future automation that pushes commits) can itself trigger the very re-review this feature exists to provide, which can in turn flag something that triggers another automated fix — a loop with no natural end. A per-PR counter (`github_pr_sessions.auto_retrigger_count`, alongside `pending_retrigger_head_sha`) bounds this: each time §24.3 step 4 actually enqueues an automatic re-review turn, the counter increments; a default budget (not given an explicit figure elsewhere in this plan — propose 10 per PR, configurable, mirroring how other new intervals in this plan are proposed) caps it. This budget governs ONLY the automatic path — a human's manual re-trigger (label/button, Step 46) is never subject to it and always works, regardless of how many automatic re-reviews already fired; the two tracks are independent by design, not merely by accident.

Once the counter reaches the budget, §24.3 step 4's "otherwise" branch stops enqueueing a turn: it still clears `pending_retrigger_head_sha` (so a later manual re-trigger starts clean) and deletes the `review_retrigger_debounce` timer (the same re-arm-or-delete contract every named-timer handler follows, `timerfired.go`) but does not dispatch. The FIRST time this happens for a given PR, the review session additionally posts one server-side verdict-tool notice (§5.2 — never a raw comment) that automatic re-review has reached its budget and further pushes need the existing manual re-trigger — a one-time event, not repeated on every subsequent debounce firing, so hitting the ceiling is observable without becoming noise. Later `synchronize` events on that same PR keep re-arming the debounce timer exactly as before (a cheap upsert either way); each firing simply finds the budget still exhausted and no-ops without posting a second notice.

### 24.7 Phasing
Step 65, Phase 5, after Step 46 (the claim/coalescing primitives this extends with a second, automatic ingress lane) and Step 62 (`review_verdicts.head_sha`, this feature's own trigger-state source) — designing this after both means it reuses primitives that already exist rather than growing them in parallel. Gates nothing else in Phase 6/7. UI: the per-repo opt-in toggle ships in Settings → Analytics alongside the other per-repo automation toggles (§12.2 items 5-6, Step 86).

## 25. Configurable workflow engine per lane + visual canvas editor (new capability)

Problem this solves: today each of Narvi's three lanes (review, request/build, plan) is one fixed
prompt → one model → one turn. There is no way to express, per lane, a configurable sequence of
steps where each step names its own agent/model, carries its own prompt, and may gate on a human
approval (HITL) — the concrete motivating case is the request lane: Gemini Pro drafts the spec →
Claude Opus scaffolds → Claude Sonnet builds → GPT (via Codex) audits, with a bounded auto-fix
loop after the audit. This section specifies a backend engine exercised by 100% of production
traffic from day one — the three existing lanes become three non-deletable, `is_built_in` system
workflows, never a parallel opt-in path — plus a canvas-style visual editor (Phase 7) on top of it.

Decisions already made, not reopened here: full drag-and-drop canvas UI (§25.12); review/request/
plan become three system workflows an admin may duplicate and customize — globally or per repo,
never in place — never delete;
Gemini ships alongside Anthropic/OpenAI in v1 (§8.8/Step 59, amended); the backend engine lands in
Phase 5 right after the automations work (Steps 51-52), the canvas editor in Phase 7 right after
Settings (Step 86); a HITL "revise" verdict is always a re-execution of the same step with the
human's text folded in as an additional instruction — exactly plan mode's own `revise:` handling
today (§8.1) — never a direct substitution of a structured artifact; and the OpenCode
credential-injection gap (§25.3) is a blocking prerequisite for this entire chantier, built first,
not Gemini-specific scope.

### 25.1 Two findings independently verified in the existing code

Per-turn model selection already exists end-to-end, no new plumbing needed: `turns.model_id`
flows through `dispatch.go`'s `buildPromptPayload` (`Model: target.ModelID`, `dispatch.go:1493`)
into `sandboxws.Prompt.model`, and the OpenCode adapter's own `resolveModel`
(`internal/adapters/outbound/opencode/session.go`) does nothing more than `strings.Cut(raw, "/")`
into provider/model — there is no Narvi-side allowlist of providers or models anywhere on this
path. It is already a generic passthrough to any `provider/model` string OpenCode itself
recognizes.

But no credential is injected into the OpenCode process for ANY provider today.
`internal/sandboxagent/opencodeproc/spawn.go` starts `opencode serve` inheriting sandbox-agent's
own OS environment verbatim (`Env: supervisor.EnvWithout(boot.SessionConfigEnvVar)` — exactly one
variable excluded); no `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or Google equivalent is wired
anywhere in this codebase. This is the actual, provider-agnostic blocking gap — not a
Gemini-specific cost — and closing it (§25.3) is a prerequisite for per-step model override to do
anything beyond what the zero-config path (§25.6) already does today.

### 25.2 Provider catalog: RESOLVED for Gemini, verified against the live binary

Amends §8.8/Step 59 ("models"). Verified 2026-08-03 directly against the pinned OpenCode 1.17.15
binary's own `GET /provider` catalog — a 4.5MB response, deliberately fetched by bypassing `rtk`
(whose own output-truncation would otherwise silently drop the payload this finding depends on).
The catalog already lists a `google` provider (`env: GOOGLE_API_KEY, GOOGLE_GENERATIVE_AI_API_KEY,
GEMINI_API_KEY`) alongside `google-vertex`/`google-vertex-anthropic` (both keyed on
`GOOGLE_VERTEX_PROJECT`/`GOOGLE_VERTEX_LOCATION`/`GOOGLE_APPLICATION_CREDENTIALS`), with 41 real
Gemini models each — `gemini-3.5-flash` checked directly: `capabilities.toolcall: true`, a
1,048,576-token context window, and real cost data (`input: 1.5, output: 9, cache.read: 0.15`). No
new `AgentRuntime` adapter is needed for Gemini — the small-scenario applies exactly as §7's
anti-corruption layer already generalizes. The one remaining gap is credential injection (§25.3),
which blocks every provider today, not Gemini specifically. A new CI contract test is still
required — mirroring §7's existing pinned-binary contract-test discipline — verifying Gemini's
actual tool-calling/streaming quality through OpenCode, since every existing contract test is
Claude-backed and proves nothing transitively about a different provider's behavior.

### 25.3 Provider credential injection (Step 53) — the blocking prerequisite

Generic secret injection into `spawn.go`'s `cmd.Env` for the OpenCode process closes the gap named
in §25.1 for every provider at once. `ActionManageRepoSecrets`/`ActionManageEnvSecrets`/
`ActionManageGlobalSecrets` (`internal/domain/authz/authorize.go`) already reserve the RBAC row for
exactly this class of data — but grepped directly, no secret-storage table exists anywhere in this
codebase yet (no `secret` column, table, or store outside one unrelated magic-link bearer token,
`migrations/000036`). This Step is the first one that actually has to build it — the same
"reserved in the RBAC/design vocabulary, not actually built" gap migration 000045's own comment
already found and named, for a different mechanism (`parent_session_id`/`spawn_depth`). Scope here
is deliberately narrow: provider API keys only, mapped provider→env-var name
(`GOOGLE_API_KEY`/`GOOGLE_GENERATIVE_AI_API_KEY`/`GEMINI_API_KEY` for `google`, `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`), sourced per-repo/per-environment/global exactly like the RBAC matrix already
anticipates. No SESSION_CONFIG change and no `AgentRuntime`/port change — this is entirely inside
`sandbox-agent`'s own process-spawn path.

### 25.4 Domain model — `internal/domain/workflow` (Step 54)

Pure, no I/O, same discipline as every other `/internal/domain` package (CLAUDE.md, §11):
- `Lane` — a closed enum (`review`/`request`/`plan`) and `LaneFor(target, mode)`, a pure mapping
  over the classifier's own existing vocabulary (`intent.TargetReview`/`TargetRequest`,
  `intent.ModePlan`/`ModeBuild`, `internal/domain/intent/rubric.go`) — not a new vocabulary
  invented alongside it.
- `WorkflowDefinition{ID, Lane, Name, IsBuiltIn, Version, Steps}`, `StepDefinition{ID, Order, Kind,
  ModelID *string, PromptTemplate, ExecutionScope (same_session|child_session),
  ConversationContinuity (continue|fresh), HITLBefore/HITLAfter, Edges}`.
- `StepOutcomeStatus` — a closed 3-value enum (`ok`/`needs_fix`/`blocked`), the same discipline
  `review.Shippable` already establishes for its own 3-value enum. This is a **distinct axis from
  `Shippable`** — an edge never conditions on anything but this fixed vocabulary, and `Shippable`
  (§21.2) is never routed through it (§25.8 states why).
- `Edge{FromStepID, OnStatus, ToStepID}`: with no explicit edge, `ok` advances to the next step in
  `Order`; `needs_fix`/`blocked` escalate by default (fail-conservative) — a retry loop must be
  wired explicitly, never implied.
- `NextStep(...)` — one pure decision function, the same shape as `turn.Transition`/
  `plan.Transition`/`sandbox.Transition` (`internal/domain/turn/state.go:167`,
  `internal/domain/plan/plan.go:118`, `internal/domain/sandbox/state.go:375`).

Why the three built-in workflows are rows, not Go constants: the "duplicate and customize"
requirement and the canvas editor both need the default to exist in exactly the same shape as a
custom workflow. All three are seeded `is_built_in = true` directly in the migration. `PUT`/
`DELETE` on an `is_built_in = true` row is refused unconditionally — a structural invariant, not an
RBAC rule.

**System template, global binding, and repo override are three distinct concepts, not three rungs
of one fallback ladder:**

- **System template** — a `WorkflowDefinition` row with `IsBuiltIn = true`. Read-only starting
  content; never itself a live setting; the only thing "system" means.
- **Global binding** — a `workflow_bindings` row keyed `(lane, repo_full_name = NULL)`. Exactly one
  per lane, seeded by the migration to point at that lane's system template — but from that point on
  it is an ordinary, independently-repointable setting, not a fallback anyone "reaches." Because it
  is seeded for every lane, this row is **never absent** — there is no "no binding configured" state
  to fail open or closed on.
- **Repo override** — a `workflow_bindings` row keyed `(lane, repo_full_name = '<owner>/<repo>')`.
  Optional, and shadows the global binding for that one repo only.

Resolution: look up `workflow_bindings` for `(lane, repo_full_name)`; if a repo-specific row exists,
use it; otherwise use the `(lane, NULL)` global row. The global row is guaranteed to exist by the
seed migration, so this is a two-step lookup with a guaranteed second step, never an "absent row →
default" fail-open branch — `workflow_bindings` has no row that resolves to nothing.

### 25.5 Circuit breaker — `internal/domain/loopguard` (Step 54)

A second new pure package: `Evaluate(State{AttemptCount}, Config{MaxAttempts}) Decision
{ShouldProceed, ShouldEscalate}` — no time window. Iteration count is read back via `COUNT(*)` on
`workflow_step_runs`, never a dedicated counter column — the same "derive it from the rows that
already exist" discipline `review_verdicts`' own `DISTINCT ON` reduction (§21.1) already applies.

### 25.6 Execution model: turns, sessions, and conversation continuity (Step 55)

By default, every step is an ordinary sequential turn on the SAME `sessions` row, dispatched
through the existing `createTurnLocked`/`CreateTurnCore` (`internal/adapters/inbound/httpapi/
turn.go:309,373`) — no new wire command, no `AgentRuntime` change. The "what happens next"
decision hooks into the sandbox-event handler this codebase already has
(`internal/app/sessionactor/sandboxevent.go:223`, `handleSandboxEvent`).

A child session is used only when real isolation is needed. `parent_session_id`/`spawn_depth`
(`migrations/000045_sessions_child_sessions.up.sql`) already exist — but note precisely what that
migration's own comment says: `spawn_depth` is recorded as observability data, not gated on
numerically; the actual "no recursion" invariant is enforced via `provenance_tag` (a
sentinel-auto-fix child session is never itself eligible to trigger another), not a depth-counter
check. The workflow engine's fix-step child session follows the SAME restriction discipline Step
48 already established (`SpawnSentinelFixChildSession`), never a numeric-depth mechanism this
codebase doesn't actually have — and is reserved for the audit-fix loop's fix step alone, never the
audit step itself.

"Fresh context" is not "a new session": `AgentRuntime.StartTurn` already branches on whether
`cmd.ConversationId` is nil to start a new OpenCode conversation inside the same sandbox
(`internal/app/ports/agentruntime.go:79`). A step that must not inherit the full chat history of
earlier steps uses `ConversationContinuity: fresh` on the SAME session, not a child session.

Typed handoff between steps, never re-parsed free text — the same discipline
`review.RenderTurnPrompt` already applies (`internal/domain/review/context.go:234`). A new,
generic step-outcome-posting tool, structurally identical to the existing verdict-posting tool
(`internal/domain/reviewpost`): `{status: ok|needs_fix|blocked, summary (advisory, never
re-parsed), structuredPayload: json.RawMessage}`. For the audit step specifically, this schema
reuses `review.Verdict` + `reviewpost.Finding[]` in full rather than reinventing them.

Concurrency: the common case (same session) hangs off the transaction the session actor already
has open. A step running in a child session is observed by a DIFFERENT actor than the one owning
the `WorkflowRun` — exactly the situation `sentinel_fixes`/`review_findings` already solve today,
via guarded `UPDATE ... WHERE status = X` writes, never the epoch-fencing mechanism.

### 25.7 Per-step model/provider binding

Confirmed by direct code reading: this is already a parameter within one existing call, not a
second port. `StepDefinition.ModelID` reuses the `provider/model` convention already in place
(§25.1). No new port, no new Narvi-side registry, for any provider OpenCode itself already
recognizes. The `LLM` port (`internal/app/ports/llm.go`) is unrelated — it serves only the
classifier and the review's single-completion calls, never an agentic turn with tool use.

### 25.8 The three built-in workflows, and the override example

- **review**: one step, `ModelID: nil`, prompt = today's unchanged text, no HITL. `Shippable`
  (§21.2) stays a separate axis, consumed after the step completes by the existing auto-approval
  machinery — never routed through `StepOutcomeStatus`.
- **plan**: one step, passthrough, no HITL of its own — **classic plan mode (Steps 37/38) remains
  the sole plan-approval authority**. This section previously specified two steps (plan → build)
  with a workflow-owned HITL gate after step 1; that shipped, and a Phase 5 audit found it put
  *two* approval mechanisms on the same session. `workflow.LaneFor` returns `LanePlan` exactly when
  `mode == intent.ModePlan` — i.e. precisely when plan mode's own `plan_status`/`decideplan.go`/
  `plan.MatchVerdict` gate is already running — so the workflow gate's own `approve` verdict
  dispatched a build turn through `workflowengine.dispatchNextAttempt`, which inserts a turn
  directly and deliberately skips `createTurnLocked`'s checks, including §23.2's persisted-state
  awaiting-plan gate. The plan built-in is therefore now a single passthrough step (migration
  `000088`), matching review/request, and honoring §25.4's "zero-config default = today's exact
  behavior" rule. Workflow-driven plan HITL is deferred until the Phase 7 canvas editor makes
  custom workflows user-facing; the `hitl_before`/`hitl_after` mechanism and its `/decide` endpoint
  remain in place for any non-built-in definition that opts into them.
- **request**: one step, passthrough, no behavior change.

The Gemini→Opus→Sonnet→Codex example is a non-built-in workflow bound as the **global** Request-lane
binding (`workflow_bindings` row keyed `(request, NULL)`) — it demonstrates that a global binding is
a real, repointable setting, not a fourth built-in default. A repo may still shadow it locally: a
scoped-Environment repo with no backend to audit against can bind a lighter, repo-specific override
(e.g. a two-step prototype flow with no audit step) via its own `(request, '<repo>')` row, which
shadows the global one for that repo alone.

### 25.9 HITL gate (Step 56)

Reuses plan mode's own cross-channel delivery mechanism (Slack/Linear/GitHub/web), not its domain
package: new `NotificationKind` values extending `planslacknotifier.go`/`linearnotifier.go` exactly
as this codebase's own precedent already does twice (`cmd/control-plane/main.go`'s notifier
routing map). Three verdicts: approve (continue), reject (end the run), revise (human text →
always a re-execution of the same step with the text as an extra instruction, never a direct
substitution of a structured artifact). Web endpoint: `POST /api/workflow-runs/:runId/steps/
:stepRunId/decide`, the same shape as `decideplan.go` — **the only decision surface that ships**.
Human-revision loops are exempt from the circuit breaker, mirroring §24.6's own exemption of
manual re-triggers.

This section previously also specified a deterministic GitHub `EditPrefix` keyword. That keyword
was built as a tested pure function and never wired to any ingress: a Phase 5 audit found
`MatchEdit` had zero call sites, and `advance.go` carried a comment citing a call site in
`completion.go` that does not exist — so a maintainer replying `edit: …` in a PR thread was parsed
by nobody while the run stayed blocked. With the plan built-in now a passthrough (§25.8), no
built-in workflow parks a HITL decision at all, so the keyword had no reachable trigger either;
it is deleted rather than kept as speculative scaffolding. A GitHub-side affordance belongs with
the Phase 7 canvas editor, when custom workflows that genuinely use `hitl_after` become
user-facing — and should be built together with the ingress branch that routes it.

The auto-fix loop itself needs no separate loop mechanism: `Edge{audit, needs_fix, fix}`,
`Edge{fix, ok, audit}` — two ordinary edges `NextStep` already evaluates. `loopguard.Evaluate` is
consulted only when the `audit → fix` edge is about to re-fire; on escalate,
`WorkflowRun.Status = needs_review`, one notice (never repeated, like §24.6), stop. This never
touches or reuses `sentinel_fixes`/`SpawnSentinelFixChildSession` (Step 48) — the fix step here is
structurally parallel to it, never a caller of it.

### 25.10 Wire contracts

New schema-first entities in `contracts/rest/v1/dtos.schema.json`: `WorkflowDefinition`/
`WorkflowStepDefinition`/`Edges`, `WorkflowBinding`, `WorkflowRun`/`WorkflowStepRun` (read-only),
`WorkflowStepDecideRequest`/`Response`. An optional `canvasPosition {x, y}`, opaque server-side. No
SESSION_CONFIG change, no `AgentRuntime` change.

**Routes (amendment — these were never specified, and the omission was load-bearing).** The
subsections above define the DTOs and the RBAC actions but never say through what endpoints either
is reached. Only §25.9's HITL decide route was ever named, and it is the only one that exists. An
editing surface (§25.12) and a run view cannot be built against entities with no routes, so the
surface is specified here rather than invented per-consumer:

| Route | Action | Notes |
|---|---|---|
| `GET /api/workflow-definitions` | `ActionManageWorkflowDefinitions` | every definition, built-in and custom |
| `GET /api/workflow-definitions/{id}` | `ActionManageWorkflowDefinitions` | one whole document |
| `POST /api/workflow-definitions` | `ActionManageWorkflowDefinitions` | create a draft, or duplicate an existing definition |
| `PUT /api/workflow-definitions/{id}` | `ActionManageWorkflowDefinitions` | whole document; refused for built-in AND for bound (below) |
| `DELETE /api/workflow-definitions/{id}` | `ActionManageWorkflowDefinitions` | refused for built-in AND for bound |
| `GET /api/workflow-bindings` | `ActionManageWorkflowDefinitions` | every `(lane, repo)` binding, global row included |
| `PUT /api/workflow-bindings` | `ActionActivateWorkflowBinding` | admin-only; binds one `(lane, repo-or-global)` |
| `GET /api/sessions/{id}/workflow-runs` | session read | a session's runs, newest first |
| `GET /api/workflow-runs/{runId}` | session read | one run WITH its ordered step runs |

**A definition is one document, not a graph assembled from three endpoints.** `WorkflowDefinition`
already embeds `steps`, and each step embeds its own outgoing `edges`, so GET returns and PUT
accepts the whole thing in one shape — complete desired state, never a partial patch, the same
convention `UpdateRepoSettingsRequest` already carries. There are deliberately no per-step or
per-edge routes: a canvas edits a graph, and a graph saved a node at a time has intermediate states
the engine could not execute.

**Validation belongs to the save, not to the canvas.** §25.12 requires the editor to constrain what
a user can draw; that is a usability duty and never the guarantee. `PUT` re-validates the whole
document server-side against the closed model — ordered steps, edges keyed only on the three
`StepOutcomeStatus` values, every `toStepId` resolving inside this definition — and refuses a graph
the engine could not run. A canvas that draws more than the engine executes is a UI bug; a canvas
that *saves* it would be a data-integrity bug.

**Duplication is the escape hatch, so it is specified rather than left to taste.** The refusals
below only work as a redirect if the copy is one obvious action; a maintainer who has to
hand-rebuild a graph node by node will instead ask an admin to unbind, which defeats the point.
`POST /api/workflow-definitions` therefore accepts either a whole new definition document or a
`{sourceDefinitionId, name}` pair. The copy is deep — every step and every edge — and it always
lands `is_built_in = false`, unbound, at version 1, whatever it was copied from. A built-in is
copyable exactly like anything else: that is how the three seeded lane defaults are meant to be
customised (§25.8's own override example), and refusing to copy one would leave no way to start
from a working graph.

**Two structural refusals, neither of them an RBAC row.** `PUT`/`DELETE` on a definition with
`is_built_in = true` is refused unconditionally (§25.4) — even for an admin, who must duplicate
instead. The second is new, and closes a real gap: see §25.11.

**There is a THIRD structural refusal, and this section originally missed it.** A definition that
any `workflow_runs` row has ever used is refused for `PUT` and `DELETE` too. Found while building
this surface, by reading the migration rather than assuming: `workflow_runs.workflow_definition_id`
and `workflow_step_runs.step_definition_id` carry no `ON DELETE` clause at all — plain `NO ACTION`,
unlike their cascading neighbours — so PUT's own delete-and-reinsert of steps, and DELETE of the
definition, would both raise a constraint violation and surface as a 500 rather than a refusal.

Fixing that as an error-mapping detail would have missed the reason. `workflow_step_runs` stores no
snapshot of what it ran — not the prompt, not the model, not the kind — and describes the executed
step **only** through `step_definition_id`. Rewriting steps under a finished run therefore silently
rewrites what the ledger says happened. The freeze protects the audit trail; the foreign key merely
enforces it.

The accepted cost, stated rather than discovered later: **running a draft even once freezes it.** A
maintainer who executes a workflow to try it out must duplicate to change it. The alternative —
snapshotting each step's own content onto its `workflow_step_run` so the ledger stands alone — is a
schema change with a real migration behind it, and is not taken here; if per-run editing history is
ever wanted, that is the design to reach for, not a weakening of this refusal.

**The refusals need a READ-side surface, and this section originally forgot
one.** All three are enforced on write, which is what makes them structural —
but an editor has to know before it lets someone work. Two of the three were
derivable from the definition shape and the bindings list, so the first editor
built re-derived them, carrying a second copy of the rules and of their wording
beside the authoritative one. The third, run history, was derivable from
nothing at all: the screen could only discover a frozen definition by letting
the operator finish and then failing the save.

`WorkflowDefinition.editRefusal` closes that: a nullable
`built_in | bound | has_runs`, computed by the same function the write path
enforces, so the rule has one home and a client renders a verdict. The list
endpoint carries it as two `EXISTS` columns on its own query — a per-definition
round trip would be the N+1 this avoids. What stays client-side is the WORDING,
which is where wording belongs; what must never be client-side again is the
rule. Each reason keeps its own remedy: duplicating and unbinding are different
actions, and a single "read-only" label that omits which one applies sends an
operator after the wrong fix.

**Runs are read-only and always carry their step runs.** `workflow_step_runs` has no list read
today, so a run view has no way to show a step sequence at all. `GET /api/workflow-runs/{runId}`
returns the run and its ordered step runs together, because a run without its steps answers no
question anybody asks.

### 25.11 RBAC

Three new actions, each mirroring an existing row in `internal/domain/authz/authorize.go`:
- `ActionManageWorkflowDefinitions` — maintainer+ (same row as `ActionManageAutomations`):
  create/edit an unbound draft.
- `ActionActivateWorkflowBinding` — admin-only (same row as `ActionActivatePromptTemplate`): bind
  `(repo, lane)` to a specific definition — `repo` may be a specific repository or the global
  (org-wide, `repo_full_name = NULL`) scope; the same action gates both.
- `ActionDecideWorkflowStep` — own/joined-aware (same row as `ActionApprovePlan`).

`is_built_in` immutability is a structural invariant, not an RBAC row (§25.4).

**"Unbound draft" is doing security work, and nothing enforced it (amendment).** The maintainer-level
row above is justified by the word *unbound* — a draft with no effect on production until an admin
activates it — and the admin-only row exists because activation "is the action that changes what
actually drives dispatch". As built, that premise is false. `resolveBinding` resolves
`(lane, repo)` to a binding and `LoadDefinition` then reads the definition **by id alone**;
`workflow_bindings.definition_version` is recorded and never consulted. So editing a definition that
is already bound changes production dispatch for every session started afterwards, immediately, with
no admin involved — the maintainer-level action reaching straight past the admin-only gate that
exists to prevent exactly that.

Two ways to close it, and only one keeps this section's own reasoning intact:

1. **Make *unbound* true.** `PUT`/`DELETE` on a definition referenced by any `workflow_bindings` row
   is refused, structurally, the same way `is_built_in` is — not as an RBAC row, so it cannot be
   configured away. A maintainer duplicates, edits the copy, and an admin activates it. Every word
   of the RBAC split above then holds as written, and the admin gate becomes the only path by which
   production behaviour changes.
2. Let edits go live and raise editing a bound definition to admin-only. This abandons the
   maintainer-level row's stated purpose and gives admins a second, unaudited route to the same
   effect as activation.

**Option 1 is the specified behaviour.** It also settles what `definition_version` means: a record
of the version that was current when the binding was made, for audit and diagnosis, never a
retrievable pin — the schema keeps one row per definition and no version history, so there is no
older version to resolve to. Nothing may be built that implies otherwise.

### 25.12 Visual canvas editor (Step 91, Phase 7)

A React Flow-style node/edge canvas for authoring a lane/repo workflow's steps and edges. It must
validate/constrain what a user can draw against the engine's closed model — ordered steps plus
3-status edges, no expression language (§25.4) — rejecting an undrawable-by-the-engine graph at
save time, not silently accepting it. Inline progress display of a running workflow in the session
view is a SMALL extension of the already-planned sub-task-lane rendering (§7.1, Steps 82/83) — not
a separate Step.

**Decisions the editor had to make that this section did not settle.** It is
three sentences long and describes the largest screen in the phase, so these
are recorded rather than left to be re-litigated:

- **Edges are authored through closed pickers, not drag-to-connect.** An edge's
  status comes from the three-value enum and its target from this definition's
  own steps, so an edge the engine could not execute is unrepresentable rather
  than merely refused. "React Flow-style" above names the canvas, not a
  particular gesture; if drag-to-connect is later wanted, it must land with the
  same two constraints, not instead of them.
- **A step has no name on the wire**, so nodes are labelled by order plus the
  first line of their prompt template. The mockup's node titles ("Draft spec",
  "Scaffold") are not backed by any field, and inventing one client-side would
  have been a fabricated label rather than a rendering of data.
- **The circuit-breaker attempt ceiling and a version history are not built.**
  `loopguard`'s configuration is on no DTO, and `version` is explicitly not a
  retrievable pin (§25.11) — there is no older version to show. Both appear in
  the mockup; neither has data behind it.

### 25.13 Risks and open questions

- **Canvas-vs-engine expressivity mismatch**: the editor is a general node/edge canvas; the
  runtime supports only ordered steps + a closed 3-status-edge model, no expression language
  (§25.12).
- **Per-step cost attribution**: CLOSED at the workflow-step level by §25.15 (Step 93), which is
  where that work is specified. §7.1's own adjacent debt -- per-model attribution when a sub-task
  runs on a different model than its parent turn, where `step_finish` carries no model to attribute
  by -- is NOT closed by it and stays open; a workflow step is a turn, and a turn already knows its
  model, which is why one was cheap and the other is not.
- **Decision inbox** (§16, Steps 60 and 87 -- the read model/API and the screen over it) is not
  extended by this chantier.
- **`LaneFor` must inherit the classifier's fail-open discipline**: `IsActive` defaults every
  surface to shadow when unconfigured (§18.5) — `LaneFor` must default the same way rather than
  block dispatch on an unresolved lane.
- **Multi-provider streaming/cost/error-taxonomy parity** if Gemini runs through OpenCode is an
  untested hypothesis, not yet validated end-to-end.

### 25.14 Phasing

Steps 53-56, Phase 5, immediately after Step 52 (automations: triggers & extras). 53 is a blocking
prerequisite for 54-56; 55 is exercised by 100% of production traffic from day one. The canvas editor
is Step 91, Phase 7 -- after Step 88, which builds the definition/run API it reads and writes, and
before ui finalize (Step 95), which must stay last.

### 25.15 Per-step model & cost attribution (Step 93)

§25.13's own risk list says this chantier "inherits, and must close" the per-step cost gap §7.1 records
as debt. The run view (Step 94) is specified to show per-step model and cost; neither reaches it today.
This section says what closes that, and — as importantly — what does not need closing.

**Model needs no wire change and no new column.** `turns.model_id` has persisted the dispatched model
since migration 000018, and §25.6 makes every workflow step an ordinary sequential turn on the same
session. A step's model is therefore a join through `workflow_step_runs.turn_id`, not a new fact. This
is deliberately NOT the §7.1 debt it resembles: that debt is per-model attribution when a SUB-TASK runs
on a different model than its parent turn, where the sandbox's own `step_finish` carries no model field
to attribute by. A workflow step is not a sub-task; it is a turn, and the turn already knows.

**Cost does need persistence.** `step_finish.cost` (§6.1) already arrives per step and is already
written to `events.payload`, but `events` carries only `session_id` — there is no `turn_id` on that
table, so no honest per-step figure can be recovered from it without correlating on payload fields,
which is a derivation, not a fact. A running total is accumulated onto the turn as those events land,
in the same transaction that persists the event.

**The idempotency key is `step_finish.stepId`, and this is load-bearing.** The wire replays: §6.1's
sender buffers events and re-sends on reconnect, so the cost write must be idempotent or a forced
reconnect inflates the bill. `stepId` is one per step and is the only key on the wire that identifies
*this step's* cost. It is specifically NOT the `inserted` flag the raw-event upsert returns — that
flag answers "was this `(session_id, message_id)` row new to the events table", which is a different
question and is false for every `step_finish` ever sent, because `step_start` and `step_finish` are
two parts of one assistant message and share its id. An implementation gated on it records nothing at
all, and every test passes if its fixture invents a fresh message id per event instead of sending the
`step_start` that always precedes the `step_finish`. Per-step rows are kept rather than discarded
after summing: a total nobody can recompute is a number nobody can check.

**Attribution is by "the turn processing when the event lands"**, not by a turn id on the event,
because §6.1 does not carry one. Same turn in every ordinary case; not the same if a turn terminalizes
while one of its `step_finish` events is in flight. Stated here so it is not mistaken for a guarantee. The adapter-local `turnState.spentUSD` accumulator
(§26.7) is NOT extended or reused: it exists to answer one sandbox-local question inside a live turn,
it never leaves that process, and making it authoritative for a control-plane read would give one
number two owners.

**The edge actually taken is not a new field.** It is derivable, exactly and without ambiguity, from
the ordered step runs and their outcome statuses: the next step run's `step_definition_id` IS the edge
target that was taken. Adding a column for it would be a third copy of a truth the run ledger already
records, and the copies would eventually disagree.

**Wire contract.** No new event type and no new `step_finish` field. §6.1's standing warning about
`cost.tokens` being an object rather than a number applies unchanged — a shape mismatch there silently
zeroes cost tracking, and the pinned-binary contract test is what keeps it honest. That test must now
also fail if `cost` itself stops arriving, since a silently-absent cost would read as a free step
rather than an unknown one.

**Not built, named:** cross-session and cross-turn cost totals; §12.2 item 6's cost-by-model view; and
per-model attribution inside a sub-task fan-out, which remains §7.1's open debt and is untouched here.

## 26. Review as a merge readout (new capability)

**Context.** When agents author most of the code under review, line-by-line human reading stops
being the bottleneck — the merge **decision** is: merge or not, on what basis. This section
restructures the review verdict from "a findings list with a badge" into a **merge readout** —
named for an instrument's readout: not the raw telemetry, but the synthesized display an operator
reads before a go/no-go call. Here the instrument is the review pipeline, the operator is whoever
decides the merge, and the readout states: what the PR does, which architecture choices it makes,
what it risks in the surrounding stack, and whether its own description tells the truth. Code
findings remain — they are the raw telemetry — demoted to supporting evidence in a collapsed
appendix. Two anchors already exist in the shipped design: `PremiseState` (Step 45)
is the embryo of exactly this posture — "should this PR exist?" — and this section grows it into
the full readout; and every review trigger path (mention, label/button, automatic re-review §24,
release detection §15) already converges on one funnel — review-session creation and dispatch
(Step 46) — where model, effort and prompt are chosen and the inline pre-fetched diff's stats are
already known. That funnel, not the intent classifier, is review's real router (§18 decides
review-vs-request at ingress; everything after that converges here), and it is where the two-path
triage below inserts.

Four Steps (66-69, end of Phase 5). Steps 66-67 deliver the paradigm's core without waiting for
any multi-agent machinery; Step 68 only pays off once the verdict's content has changed; Step 69
rides the deep path 68 creates. All four build on the merged verdict foundation (Steps 45-47) and
on the persistence/analytics instrument (§21/Step 62) — the instrument that will measure whether
the paradigm shift actually operates.

### 26.1 The digest: architecture & risk readout (Step 66)

The rendered verdict is restructured to front-load the decision:

1. **Header** (unchanged): risk badge + why-line + shippable class (Steps 45/47).
2. **"What this PR does"** — 2-4 sentences written **from the diff**, never copied from the PR
   description. The readout's keystone: simultaneously the human's summary, the reference text for
   the adequacy check (§26.2), and the per-PR headline the decision inbox (§16) and deterministic
   digest (§21.3) surface.
3. **"Architecture choices"** — each structural decision the diff makes: what was decided, the
   alternative implicitly rejected, and conformance to the repo's own conventions (its agent
   instructions file, its established patterns).
4. **"Risks to the stack"** — blast radius in the existing fixed vocabulary (`BlastRadius []Tag`,
   Step 45); coupling and deployment risks (migrations, multi-phase deploys, image rebuilds);
   reversibility; and — explicitly — what was **not** verified (honest limits).
5. **Collapsed appendix**: findings, coverage, docs-drift, worth-a-look — retained intact, demoted
   to supporting evidence.

**Typed fields, never markers.** All of this rides the verdict-posting tool's structured payload
(Step 47): `Digest{Summary, ArchDecisions []ArchDecision{Decision, RejectedAlternative,
ConventionConformance}, StackRisks, UnverifiedLimits}`. Nothing is ever parsed back out of posted
markdown (Step 45's invariant); rendering is server-side from the typed fields (`reviewpost`),
like every other verdict element. The digest columns ride the append-only `review_verdicts`
history (Step 62), so digest quality is measurable from day one like everything else.

**Enforcement.** `Digest.Summary` is required on every review from Step 66 on (the adequacy check
and the inbox headline depend on it). The full digest (architecture choices + stack risks) becomes
schema-**required** on the deep path once §26.3 defines it: the posting endpoint rejects a
deep-path verdict without it with a structured reason and the agent re-submits — the same
reject-don't-repair posture the endpoint already applies to invalid payloads — and a deep-path
verdict whose digest is semantically empty raises the `Shippable` floor (§26.2's composition). The
light path requests the full digest but does not hard-require it.

### 26.2 Description adequacy: does the PR tell the truth? (Step 67)

Confirmed gap: a PR's title and body enter review context only as untrusted input blocks (§5.2) —
nothing checks them against the diff. Closing it:

- The agent compares its own diff-derived `Digest.Summary` (§26.1) against title + body and emits
  a typed tri-state on the verdict: `DescriptionAdequacy: ok|drift|misleading`, plus a one-line
  explanation. The description remains untrusted *input* — the comparison consumes it, never obeys
  it.
- **A third raise-only floor.** `misleading` floors `Shippable` at `needs_human`, composing with
  the coverage and premise floors via the existing `max(rank)` (Step 45's
  exactly-one-pure-function-per-floor pattern — this adds the third). Deliberate divergence from
  also inflating `RiskLevel`: the server computes `Shippable`; it never fabricates risk the model
  did not report (Step 45's server-computed-only rule cuts both ways).
- **Graduated remediation.** On PRs authored by Narvi's own sessions (the session→PR linkage
  already records authorship), the agent may rewrite the PR **body** behind a per-repo
  `descriptionAutofix` flag — **default off** — preserving the original in a collapsed block. The
  write is delivered via a `SourceControl` port extension + the outbox (§5.1: every outbound side
  effect), with the Narvi-authorship and flag checks enforced server-side at delivery time (§5.2:
  never prompt-only) — never an in-sandbox `gh pr edit`. On human-authored PRs: a proposed body
  rendered in the digest, never a write. The **title is never rewritten automatically**, in either
  case.

### 26.3 Two-path triage: light and deep (Step 68)

The depth decision is made in the single funnel (Step 46's creation/dispatch path),
**deterministic-first** — the same posture as everywhere else in this system (the server does not
trust agent judgment for routing; deterministic fallbacks throughout, §18):

- **Signals**: additions+deletions and changed-file count (already fetched with the inline diff,
  Step 46); the changed **paths**, promoted to a first-class structured signal; sensitive globs
  (migrations, auth surfaces, infra-as-code, CI workflows) mapped deterministically onto the same
  `BlastRadius` tags the verdict uses; cross-cutting dispersion (number of distinct top-level path
  roots); provenance (Narvi-authored vs human, and the authoring model); the PR's own verdict
  history (Step 62); the `review:needs-human` label — human-owned and never bot-synced, unlike
  `review:low/medium/high-risk`, which `ComputeLabelSync` overwrites on every posted verdict and is
  therefore not a stable routing input, so those three are deliberately not among these signals; a
  hand-applied risk label would just be clobbered by the next verdict regardless.
- **v1 rules** (initial thresholds): any sensitive-glob hit → always deep; >600 changed lines or
  ≥3 distinct top-level path roots → deep; a prior `high` verdict on this PR (Step 62) → deep; a
  `review:needs-human` label → deep; otherwise light. **No LLM tie-break in v1** — a `review_depth`
  surface on the unified classifier (§18) remains a v2 option only if per-path analytics show a
  real grey zone (the classifier consumes free text today, so this would be new surface area, not
  a config flip).
- **Output**: `reviewDepth: light|deep`, threaded into review-session creation; recorded on the
  routing decision record (§18.4's precedent); persisted as `review_path` on the verdict row (Step
  62) so **cost and precision become measurable per path**. Depth drives model/effort: light is the
  deployment's own default review model, unchanged; deep is a per-deployment override plus forced
  high effort. No dedicated review-model-selection mechanism predates this Step — §8 item 2 names
  "dedicated review model selection" as a feature-set line, not a built mechanism — so Step 68 is
  what introduces it, as an optional operator override rather than a catalog-driven tiering system;
  document it alongside this deployment's other operator-level knobs. Depth composes with
  cross-family counter-review (§26.4): the family comes from provenance, the tier from depth.
- **Per-repo config**: `reviewDepth: {mode: auto|always_light|always_deep, deepPaths: [...]}`
  alongside the other per-repo review settings. **Any triage error fails open to light** — a
  review must never be blocked by its own router.
- **Re-review on push** (§24): depth re-evaluated on the delta, but floored at the PR's previous
  depth — once deep, a PR stays deep, with one explicit exception: a repo configured
  `reviewDepth.mode: always_light` (the per-repo config above) skips this floor entirely, using the
  fresh, unfloored decision as-is (`reviewtriage.Floor`'s own doc comment, depth.go, "D9"). An
  admin's explicit, deliberate cost-control choice outranks this history-based "always add rigor"
  floor — the two are in direct, irreconcilable tension for exactly this one case, and applying the
  floor unconditionally would silently defeat the override the moment a PR had ever gone deep once
  (staying deep on every subsequent auto-triggered push forever, even after an admin flipped the repo
  to `always_light`) and would persist a self-contradictory decision record (`mode: always_light`
  alongside `depth: deep`).

**The changed-path discovery this depends on was widened after shipping, as a side effect of a
different fix — recorded here because it changes this section's own routing volume.**
`reviewtriage.ExtractChangedPaths`, the function computing the changed **paths** signal above and
the sensitive-glob classification and root-count triggers that key off it, originally harvested
paths only from unified-diff `---`/`+++` header lines. The finding-retirement work (§22.1.2) needed
it to also recognize binary files, mode-only changes, and git-quoted non-ASCII paths — shapes that
previously contributed no path at all — so it could tell "genuinely absent from the diff" apart from
"present but in a shape the old parser didn't recognize." That fix landed in a PR about finding
retirement, not about triage, and this section was not updated at the time. Two consequences, one a
cost the routing above must now be read with, one a security improvement worth stating plainly:

- **More PRs now cross the deep-routing thresholds.** A PR whose third distinct top-level root is
  only a binary asset, or whose sensitive-glob hit was previously invisible because the changed file
  was a mode-only change, now counts. Deep means cross-family counter-review, the fact-check pass,
  and the architecture-scribe (§26.4/§26.6) — real added cost per review. At the time this widening
  landed, that cost was **unmeasured and unbounded**: §26.7's cost ceiling had no accumulator and no
  production call site yet (§7.1) — this widening was not sized before it shipped, and nothing
  existed at the time to bound its consequence even after the fact. Step 70 has since closed that gap
  (the accumulator and the loopback call site both now exist, §26.7's own updated text), but that
  closes the MEASUREMENT/bound gap going forward — it does not retroactively size what this specific
  widening cost while unbounded, which remains an open question for whoever reviews Step 70's own
  cost-per-path analytics once they accumulate real data.
- **Sensitive-path detection is genuinely better**, not merely different. A binary file committed
  under a sensitive path — the shape committed credential material or a compiled artifact typically
  takes — previously produced no path at all and triggered nothing. It now routes deep like any other
  sensitive-glob hit. This is a real closing of a blind spot, not a side effect to apologize for; it
  is named here as a security improvement alongside the cost consequence, not folded into it.

**Triage as a two-axis decision, not one — independently validated in production.** The rules above
decide *which path* a PR gets (model/effort tier). A second, narrower axis this section did not
previously name is *which passes run at all* once a path is chosen. OpenCodeReview (§26.6's full
citation) independently validates gating passes on the same kind of size signal, in production, one
level more granular than this section's own PR-level fork: `threshold :=
template.PlanModeLineThreshold` (50), `changeLines := d.Insertions + d.Deletions`, and its
planning/analysis pass is skipped entirely below it. This is not new evidence for the light/deep
fork itself — that fork already exists here — it is independent validation that gating *whole
passes* on a size signal, not just a model tier, holds up outside this plan too. §26.6's fact-check
pass is the one v1 consumer of this second axis (light path runs it same as deep, so for that pass
the axis is binary-on, not size-gated); further gating *within* the deep path (skipping
`architecture-scribe` or `counter-reviewer` themselves below some size) is named, not designed —
§26.9 resolves why it stays out of v1.

### 26.4 The deep path: adversarial counter-review (Step 69)

**One sandbox.** The primary reviewer orchestrates context-isolated sub-agents via the engine's
own sub-task fan-out (§7.1 — already shipped in Step 17: engine-native sub-agents, `subTaskId`
tagging, flat, cost rolls up). Explicitly **not** N parallel sandboxes (coalescing complexity and
N× boot cost with no real independence gain — each sub-agent already has a clean context) and
**not** Narvi child sessions (§14.4's materially heavier mechanism, the wrong tool here).

- **`architecture-scribe`** (read-only): produces the architecture-decision recap from the diff +
  repo conventions in a virgin context, uncontaminated by the primary's finding hunt.
- **`counter-reviewer`** (read-only, adversarial): receives the primary's findings + digest and
  attempts to **refute** each finding and to surface what was missed. With provider credentials
  injectable (Step 53) and the model-catalog work (Step 59), it can be pinned to an opposing model
  family via the engine's own per-sub-agent model selection — family opposed to the PR's authoring
  model, tier from depth (§26.3).
- **Synthesis**: only findings surviving counter-review are published. Inter-agent disagreements
  surface in the digest as a **"Contested points"** section — agent disagreement is precisely the
  signal that a human must decide.
- **Schema-enforced self-report, now corroborated against the persisted trace — structural, not
  merely presence-checked (Step 71).** The verdict payload carries a typed `CounterReview:
  done|skipped` field, schema-required on the deep path (rejected if absent — §26.1's
  reject-don't-repair posture); `skipped` raises the `Shippable` floor to `needs_human`. A typed
  field, never a marker parsed from markdown (Step 45's invariant, once more).
  **An earlier draft of this bullet, headed "Structural enforcement," argued that the control
  plane "cannot observe the sandbox's internals" — which contradicts this very section's own
  opening (§7.1's `subTaskId` tagging, shipped in Step 17) and was false as stated:**
  `sub_task_finish` is one of the six ack-guaranteed critical event types
  (`ports/agentruntime.go`), and sandbox-event persistence is unconditional for every recognized
  type (`sessionactor/sandboxevent.go`), so a sub-agent that actually ran leaves a durable,
  queryable trace — the control plane's own gap was never the *event*, only its *content*
  (`sub_task_finish` records that a sub-task ended, never what the counter-reviewer concluded, so
  the verdict still has to carry the outcome). Step 71 closes the remaining gap by reading that
  trace back and comparing it against the self-report, making the heading honestly "structural"
  rather than aspirational:
  - `sandboxws.SubTaskStart` gained an additive, optional `subAgentType` field (§6.1) — the task
    tool's own real `subagent_type` dispatch parameter (VERIFIED LIVE:
    `{"description","prompt","subagent_type"}`), distinct from `label`'s pre-existing freeform,
    non-correctness-bearing text. `internal/adapters/outbound/opencode/translate.go`'s
    `translateSubTaskStartFromTask` populates it from the task tool's own input; the legacy,
    unverified-live `subtaskPart` fallback path (`translateSubTaskStart`) has no task-tool input to
    source it from and leaves it absent, exactly like any producer that predates the field.
  - `internal/domain/reviewverdict.CounterReviewCorroborated` (pure, zero I/O) reports whether a
    `sub_task_start` record naming `review.CounterReviewerAgentName` and a `sub_task_finish` record
    for the SAME `subTaskId` with `outcome == "completed"` both exist in the trace it is handed.
  - `httpapi.PostReviewVerdict` reads that trace back via two queries
    (`ListSubTaskStartEventsForTurn`/`ListSubTaskFinishEventsForTurn`, filtering the `events`
    table's JSONB `payload->>'gen'`) scoped to BOTH `turns.dispatched_sandbox_gen` AND an
    `events.id > <this turn's own dispatched_event_id>` lower bound — never merely `session_id`, and
    (fixed post-review, see below) never gen alone either. Gen alone was found, by an adversarial
    review of this Step's own PR, to be insufficient: `dispatched_sandbox_gen` is bumped only on a
    fresh spawn/restore/resume, never on an ordinary dispatch to an already-live sandbox, so a
    session whose sandbox survives across multiple review turns (§24's automatic re-review, the
    ORDINARY case, not a corner case) dispatches every one of those turns at the SAME gen — letting
    an earlier turn's own real trace spuriously corroborate a later turn's self-report purely
    because both turns shared the same gen. The `dispatched_event_id` lower bound closes that gap:
    `turns_one_processing_per_session`'s own unique partial index guarantees turns execute strictly
    sequentially per session, so an earlier turn's own sub-task events always carry ids at or below
    a later turn's own dispatch watermark. Gen-scoping still stays, alongside it, for the orthogonal
    case it alone catches: a stale, late-arriving event from a genuinely different, now-dead sandbox
    incarnation.

    That lower bound is a monotonic `events.id`, **not** a timestamp. It was first written as
    `created_at >= <this turn's own dispatched_at>`, which was not sound: `events.created_at` is
    stamped by the Postgres server while `turns.dispatched_at` is a Go `time.Time` stamped by the
    application process, on a different host in any real deployment. Two clocks, agreeing only to
    whatever precision NTP happens to hold — and asymmetric in how they fail, since an application
    clock running BEHIND the database widens the window and readmits an earlier turn's trace,
    eroding exactly the guard the bound exists to provide. `turns.dispatched_event_id`
    (migration `000089`) is the events-log high-water mark, `MAX(events.id)`, stamped in the same
    transaction as the dispatch itself, so the comparison has no clock in it at all.
    `turns.dispatched_at` is deliberately left in place and unchanged — it still has a genuine,
    same-clock consumer in `turn.EvaluateTurnDeadline`, which compares it against the application's
    own `time.Now()`; re-sourcing that column from the database clock to fix corroboration would
    have broken the deadline instead. Sequence values are allocated before commit, so an event
    from an earlier turn that receives its id before this turn's dispatch and commits afterwards
    falls at or below the watermark and is EXCLUDED — the fail-conservative direction.

    Only queried when it could matter at all: deep path and a self-reported `done`.
    `dispatched_sandbox_gen` or `dispatched_event_id` being unset (a turn with no recorded dispatch)
    is treated as NOT corroborated, fail-conservative like every other closed-enum default in this
    codebase — distinct from a watermark of `0`, which is a legitimate value (a turn dispatched
    before the session had any events) that admits every event that follows.
  - `reviewpost.BuildVerdict` applies a SECOND, raise-only substitution (immediately after the
    existing light-path substitution, and gated explicitly on `ReviewDepth == DepthDeep` — never
    merely on the raw `CounterReview` value, which carries no validated meaning on the light path
    and would otherwise let this substitution silently floor light-path verdicts too): a deep-path
    `done` claim the server could NOT corroborate is downgraded to `CounterReviewSkipped` before
    `CounterReviewFloor` runs, landing on the identical `needs_human` floor an honest `skipped`
    self-report already produces. This can only ever make `Shippable` MORE conservative than the
    self-report alone, never less.
  - **A known, accepted race, not a bug:** the verdict POST and the WS event carrying
    `sub_task_finish` are two independent network round-trips from the sandbox with no server-side
    ordering guarantee between them, so a real, completed counter-review whose `sub_task_finish`
    has not yet committed to Postgres by POST time reads as uncorroborated. This fails toward
    `needs_human` — strictly more conservative, never less — the same fail-conservative bias
    `CounterReviewSkipped`'s own "every cause floors identically" posture already commits to.
    Deliberately left unaddressed: no retries, no polling, no new timeout constant.

### 26.5 Measuring the readout (Step 69, on Step 62's instrument)

- **Per-section digest feedback** extends the finding-outcome read model (§21.1): **contest only,
  per digest section** — an earlier draft said "contest/confirm", which the shipped schema
  (`migrations/000086`) does not admit and was never going to: it records contests, and a section
  carrying none is un-contested by absence. An explicit confirm would add a write per section per
  review to persist a signal already derivable from silence, and would quietly change what the
  contestation-rate KPI below divides by. Plus a maintainer command `arch recap wrong: <reason>` mirroring Step 63's
  `false positive:` command exactly — maintainer+ via the existing `Authorize` gate,
  deterministically routable (§5.2), idempotent on the triggering comment id. The recap itself
  becomes measurable and correctable. Each contest is reconciled by a content hash of the digest
  section's own persisted text (§22.1's identity discipline, extended from findings to digest
  sections) — never by section index or position, which would suffer the exact churn-fragility
  §22.1 already solves for findings: a PR update that merely reorders or rewords an unrelated
  section must never make an already-contested `ArchDecision` read as a new one.
- **KPIs** (Step 62 analytics + §12.2): digest precision (contestation rate); decision latency
  (verdict → approve — already a §16 KPI, now attributable per review path); cost per path —
  **measured since Step 62, and bounded (agent-self-governed, never server-enforced — see §26.7's
  own "why not a server-enforced gate" paragraph) as of Step 70, which gave §26.7's ceiling both a
  real accumulator and a real production call site**. Earlier drafts of this bullet went back and
  forth: one claimed the bound already delivered "as of §26.7" before either the accumulator or the
  call site existed (corrected once, to "measured today, bounded only once..."); that corrected
  version is itself now stale, since Step 70 is exactly the Step that closes the remaining gap it
  named. `internal/adapters/outbound/opencode`'s own `turnState.spentUSD` (§7.1's own corrected
  paragraph) is the accumulator; `cmd/sandbox-agent`'s loopback `GET /review-cost-budget` endpoint,
  which a review turn's own prompt now instructs the agent to call before each optional sub-task and
  calls `reviewtriage.ShouldSkipOptionalPass` for real, is the production call site. The bound is
  still exactly as strong as §26.7 always said it would be — self-reported, agent-cooperated, never a
  server-side kill switch, since this control plane has no channel to intervene inside an
  already-dispatched turn — never claim more than that; and the paradigm's proxy
  metric: **% of PRs approved with zero human inline comments** — the number that says whether the
  shift is actually operating.
- The §21.3 deterministic digest and the §16 decision inbox surface the readout's `Summary` line
  per PR — reusing their existing aggregation, no new mechanism.
- **Evals**: known-PR digest-quality cases (expected architecture decisions on reference diffs,
  seeded description-drift cases) join the shadow-precision discipline the Phase 5 milestone
  already requires.

### 26.6 Diff-only fact-check pass, both paths (Step 69)

Extends Step 69's still-unbuilt scope — a scope change to an unshipped Step, not a patch to shipped
behavior. OpenCodeReview (github.com/alibaba/open-code-review — full citation §22.1.1, fetched
2026-08-11) runs a cheap secondary pass, `REVIEW_FILTER_TASK`, that tries only to **disprove** each
finding using the diff text alone — no tool calls, no extra file reads — and kills a finding only
when it is provably wrong from the diff alone, letting anything merely uncertain through. Its own
system prompt (`internal/config/template/prompts/review_filter_task_system.md`) states the
asymmetry directly: "your task is NOT to verify whether all review comments are correct, but to
filter out only those review comments that can be confirmed as incorrect based solely on the
current diff... you should let them pass — because the Agent may have access to context that you
cannot see."

**Composition with §26.4's counter-review — not the same check, and both run on deep.**
Counter-review is deep, adversarial, tool-equipped, and runs only on the deep path; the fact-check
pass is shallow, mechanical, and runs on **both**. On the deep path they compose as a funnel:

primary reviewer's findings → **fact-check** (kills only provably-wrong-from-diff) →
**counter-review** (§26.4, adjudicates the survivors, may itself surface new findings) →
synthesis (unchanged) → publish

(`architecture-scribe` is orthogonal to this ordering — §26.4's own "virgin context, uncontaminated
by the primary's finding hunt" design means it never consumes or feeds the findings list this
funnel prunes, so this section does not sequence it relative to fact-check or counter-review.)

Fact-check running first, not last, is a deliberate cost decision: it prunes findings before the
expensive adversarial pass has to spend context and tool calls adjudicating them, directly reducing
what §26.7's cost budget has to cover. It is not redundant with counter-review despite the apparent
overlap — counter-review can talk itself out of a real defect just as easily as it can correctly
refute a fake one, where a mechanical, diff-only disproof cannot be argued with, the same "a
restriction enforced at spawn time is never trusted as sufficient on its own" logic §17.4 already
applies to its own two independent, deliberately-redundant checks. Findings counter-review itself
surfaces are **not** re-run through fact-check: counter-review, with tool access and full context,
is by construction at least as rigorous as a diff-only check for a finding it produced from
stronger evidence than fact-check could ever see — a second, weaker pass over a stronger pass's own
output would be redundant in the direction that doesn't matter.

**Mechanically an in-sandbox sub-task, not §22.1.1's `LLM` port.** Unlike §22.1.1's relocation
fallback — a post-hoc, purely mechanical rendering computation that runs after the sandbox turn has
already finished — the fact-check pass is a mid-review content judgment that must compose with, and
precede, counter-review inside the same turn. It is therefore one more sub-task the primary
reviewer's own orchestration spawns via the existing engine-native fan-out (§7.1, already shipped
Step 17), exactly like `architecture-scribe`/`counter-reviewer` — configured with no tool access,
never a CP-side call mid-turn. On the light path, where there is no scribe or counter-reviewer, it
is still available: §7.1's fan-out is a general per-turn mechanism, not something Step 68 gates by
path, so the light path's own single review turn spawns exactly one fact-check sub-task after
producing its findings, nothing else.

**Typed field, schema-required on both paths.** `FactCheck: done|skipped` (plus `FactCheckKilled
int`, the count removed, `0` when skipped) rides the same verdict payload as `CounterReview:
done|skipped` (§26.4) — schema-required unconditionally from Step 69 on, since it runs on both
paths, never only on deep.

**`skipped` never raises `Shippable` — the deliberate, load-bearing difference from `CounterReview:
skipped`.** A skipped fact-check pass means one thing only: the published findings were not pruned
of provably-wrong ones. It never means a real defect went unverified — the pass, by construction,
can only remove findings, never add or vouch for one, so its absence can only make the appendix
noisier, never less safe. `CounterReview: skipped` floors `Shippable` because a real adversarial
check was skipped on a PR whose own routing signal said it needed one; there is no equivalent
safety claim for fact-check to under-deliver on. This is a genuine, intentional difference in kind
between the two typed fields, not an oversight.

**Non-fatal on error — the same fail-open posture §26.3 already applies to triage.** If the
sub-task itself errors (provider failure, timeout, malformed output), the orchestrating primary
reviewer sets `FactCheck: skipped` and publishes the findings exactly as if the pass had never run
— never a blocked or repaired verdict. This is not merely convenient; it follows from the same
asymmetry that makes the pass safe to run on the light path at all (below): a pass that fails
**open** on error can only ever under-filter, the one failure direction the invariant below
requires.

**Why the asymmetric construction is consistent with §26.9's invariant — resolved, not skipped.**
§26.9 states: "the light path's behavior remains exactly today's review — the router may only ever
add depth, never subtract rigor from the default." A pass that removes findings from the light path
looks, on its face, like exactly the subtraction that line forbids. It is not, and the reason is the
asymmetry itself: the pass may kill a finding only when it is **provably** wrong from the diff text
alone — a fact, not a judgment call — and must let anything merely uncertain through untouched.
Rigor is the capacity to catch a real defect; a provably-incorrect finding was never a real defect
and was never contributing to that capacity, so removing it costs nothing rigor bears on, while
directly serving the precision half of this whole chantier's own KPIs (§26.5). Had the pass been
built to kill on *suspicion* rather than *proof*, it would indeed subtract rigor — it could talk
itself out of a finding the primary reviewer was right to raise, using less context than the primary
had. The asymmetry is not a nicety on top of the design; it is the specific property that makes
adding this pass to the light path compatible with the invariant at all. §26.9 amends the
invariant's own wording to say this outright, so a future reader does not have to re-derive it from
here.

### 26.7 Per-review cost budget with look-ahead (Step 69 design, Step 70 wiring)

**Honest framing: this closes a cost gap, not the context-window gap — that one is already closed.**
OpenCodeReview's own budget mechanism (full citation §22.1.1) is a **context-window** guard, not a
cost cap: `if countMessagesTokens(messages) > tokenLimit { record warning; return nil }` where
`tokenLimit := MaxTokens * 4 / 5`, checked before dispatching the next file's subtask, backed by
in-loop compression at 60%/80% of `MaxTokens`. Narvi already has the equivalent of that half — Step
44's OpenCode-adapter context-overflow compaction retry (§7.2) — and this section does not
re-propose it. What their design genuinely validates, independent of the token-vs-dollar
distinction, is a **shape**: a look-ahead check performed **before** committing to the next unit of
work, not a measurement taken after the fact. What Narvi actually lacks, and what this section
adds, is the other axis entirely — a **cost** ceiling, so that §26.5's "cost per path" becomes
bounded, not merely observed.

**Mechanism, as designed (Step 69) and as actually shipped (Step 70): check accumulated spend
before each optional pass, never predict the next one's cost.** When this section was first
written (Step 69), the running total it checks against did not exist anywhere, and an earlier draft
of this paragraph's own false claim that §7.1 "already rolls up" every `step_finish.cost` into one
running total was corrected in place — see §7.1's own corrected bullet for the full history. Step 70
is the Step that actually built it, and the shipped shape is concrete, not abstract:

- **The accumulator lives inside the sandbox, not the control plane** — `internal/adapters/outbound/
  opencode`'s own `turnState.spentUSD` (turn.go), summed by `dispatchPart`'s `"step-finish"` case
  (sse.go) for every step-finish this turn observes, main lane and every sub-task alike (§7.1's own
  fan-out already routes a sub-task's events back to the SAME `turnState`, tagged only with a
  `subTaskId`, so no lane-specific summation logic is needed). This total is in-memory and
  sandbox-process-local — it is never persisted to Postgres and never itself transmitted over the
  sandbox WS (the individual per-step costs still flow to the control plane unchanged, on each
  `step_finish` event; only the RUNNING SUM is new, and it stays inside the sandbox).
- **The check itself is a loopback HTTP call, not a number stated in the prompt.** `cmd/sandbox-agent`
  runs its own first HTTP server (`reviewcostbudgetserver.go`) — a tiny listener bound to
  `127.0.0.1` only (never `0.0.0.0`, and needing no authentication: it serves only numeric budget
  state, never a secret, and is reachable exclusively from inside this sandbox's own network
  namespace) on an EPHEMERAL port chosen at boot, avoiding any collision with a session's own
  arbitrary `services.yml` (§14.2) service ports. `internal/domain/review`'s own
  `subAgentOrchestrationInstructions` (context.go) instructs the reviewing agent, before spawning
  `counter-reviewer` or `fact-check` via the task tool, to `GET` this server's own
  `/review-cost-budget?ceilingUsd=<the per-path ceiling>` endpoint (the loopback URL itself travels
  as a placeholder token — `{{REVIEW_COST_BUDGET_TOOL_URL}}` — resolved by sandbox-agent immediately
  before the prompt reaches the engine, mirroring the verdict-posting tool's own
  `{{REVIEW_VERDICT_TOOL_URL}}` placeholder mechanism exactly, §8.2/Step 47). The handler reads the
  turn's own live `spentUSD` from the accumulator above and calls `internal/domain/reviewtriage.
  ShouldSkipOptionalPass(spentUSD, ceilingUSD)` — Step 69's own tested, exported pure function,
  finally given the production call site its own doc comment said it lacked — returning
  `{"spentUSD": ..., "ceilingUSD": ..., "shouldSkip": true|false}`. The safety margin itself is
  unchanged from Step 69's own proposal — 80%, mirroring OpenCodeReview's own `4/5` figure
  (`reviewtriage.CostBudgetSafetyMargin`, `ShouldSkipOptionalPass` returns `true` once
  `spentUSD >= ceilingUSD * 0.8`) — computed entirely server-side (inside the loopback handler), so
  the agent no longer needs to know or apply the percentage itself; the prompt still states it, for
  the agent's own context, but purely as explanation, never as something the agent must calculate.
- **This is deliberately not a prediction of what the next pass would cost** (unknowable in advance,
  and no more reliable a number here than anywhere else this plan refuses to guess) — it is a
  ceiling enforced **before** commitment, checked independently before each optional dispatch (spend
  only grows during a review, so an earlier answer never still holds later), which is what makes it
  a real bound rather than a retrospective statistic.
- **Fail-safe toward skip, not toward proceeding.** If the agent's own call to the loopback endpoint
  fails for any reason — its own tool-use erroring, a timeout, a non-2xx response, a malformed or
  unparseable body — the prompt instructs it to treat that identically to `"shouldSkip": true`: skip
  the sub-task rather than proceed as though under budget. This matches this plan's own consistent
  fail-conservative-toward-caution posture on cost (mirroring, at a smaller scale, `ShouldSkipOptionalPass`'s
  own "a zero or negative ceiling never skips, a negative spend clamps to zero" fail-conservative
  handling of its own out-of-range inputs).
- **`architecture-scribe` is excluded from this check entirely, on both paths' own wording of the
  prompt text** — never even named in the budget-check instructions on the light path (it is never
  orchestrated there at all), and explicitly carved out by name on the deep path ("this ceiling NEVER
  applies to architecture-scribe... it always runs regardless of cost") — see §26.9's own "why" for
  the exclusion; this paragraph states only that the shipped prompt text actually implements it.
- **Still self-governed, never server-enforced**, exactly as this section always said: the control
  plane still has no channel to intervene inside an already-dispatched turn (§7's anti-corruption
  layer boundary is unchanged by this Step) — the loopback endpoint gives the reviewing agent a real,
  checkable *fact* to cooperate with, in place of the self-estimate it used to be asked to produce
  from nothing, but the agent still has to make the call and obey the answer. See
  `internal/domain/review/context.go`'s own `subAgentOrchestrationInstructions` doc comment for the
  full "why this is still not a security boundary" reasoning (unchanged in kind from Step 69, now
  restated against the real mechanism rather than the abstract one).

**The budget gates optional passes only, never the primary pass a verdict depends on.** The primary
reviewer's own findings-producing pass is never itself budget-gated — there is no verdict to post
without it, and a review blocked by its own cost guard is exactly the failure §26.3's "any triage
error fails open to light" rule already refuses to allow triage to cause. The ceiling only ever
prevents *further* spend on top of a review that will be posted regardless.

**No new typed field, and no new enum value — the budget trips an existing one, the differentiator
lives in the reason string.** A breach does not invent a `BudgetExceeded` flag; it is simply one
more legitimate cause of the *next* pass's own already-specified `skipped` state — `CounterReview:
skipped` (§26.4) if the ceiling trips before counter-review would have been dispatched, `FactCheck:
skipped` (§26.6) if it trips before the fact-check sub-task. Each field's already-decided
`Shippable` consequence applies unchanged and un-special-cased: a budget-triggered `CounterReview:
skipped` floors `Shippable` to `needs_human` exactly as a tool failure would, because from the
verdict's own point of view the two are indistinguishable in consequence — an adversarial check a
sensitive or sizable PR's own routing said it needed did not happen, whatever the cause. A
budget-triggered `FactCheck: skipped` raises nothing, per §26.6's own reasoning. Exactly which cause
fired is not lost — it lives in the reason string and structured logs, the same discipline §7.2
already established for not growing `FailureReason` a new enum value per distinguishable cause:
"the differentiator lives in the reason string ... and structured logs ..., not in a new enum
value." This reuse is deliberate economy, not an oversight: this plan does not grow a parallel
outcome field for every new way an existing one can end up `skipped`.

**Per-path ceilings, per-repo tunable.** `reviewCostBudget: {light: <usd>, deep: <usd>}` joins
§26.3's `reviewDepth` config on the same per-repo settings row — initial figures proposed, not
derived (propose $0.50 light / $5 deep per review, matching this plan's own convention of proposing
a concrete, explicitly-tunable starting figure rather than leaving a blank, §24.6's
`auto_retrigger_count` budget is the precedent). Light path's own ceiling is a degenerate,
one-checkpoint case (the one optional pass it can run at all is §26.6's fact-check sub-task); deep
path's is checked once before each of the two optional sub-tasks this ceiling actually governs —
fact-check and `counter-reviewer` — in whatever order that orchestration dispatches them.
`architecture-scribe` is excluded from this check entirely (§26.9's own resolution: a
budget-triggered scribe skip would floor nothing and appear nowhere, the silent downgrade the v1
rigor invariant forbids) — it always runs regardless of cost.

### 26.8 Interplay with the workflow engine (§25)

The readout lives **inside** the review lane's single workflow step (§25.8's built-in review
workflow is one step, and stays one step); the deep path's counter-review is sub-task
orchestration *within* that step (§7.1), not workflow-engine edges. Decomposing review into
engine-visible steps (scribe → find → counter-review → synthesize) is a possible §25 v2 once both
systems are stable — explicitly not v1 scope, mirroring §25's own decision not to retrofit the
sentinel auto-fix onto the engine.

### 26.9 Decided defaults and v1 non-goals

Defaults (decided; thresholds tunable on per-path analytics): description autofix =
apply-behind-flag, per-repo, default off, Narvi-authored PRs only, body only; triage v1 = pure
deterministic, thresholds as in §26.3; **fact-check pass (§26.6) = on by default, both paths,
non-fatal on error**; **cost budget (§26.7) = on by default, both paths, propose $0.50 light / $5
deep per review, per-repo tunable**.

Non-goals for v1: no N-sandbox parallel review; no comment-parsing of any verdict element; no LLM
triage tie-break; no workflow-engine decomposition of review; **no within-deep-path pass skipping**
(resolved below); **no new outcome-field per skip cause** (§26.7 reuses `CounterReview`/`FactCheck`
unchanged); and the light path's behavior remains exactly today's review — the router may only ever
*add* depth, never subtract **rigor** from the default, where rigor means the capacity to catch a
real defect, never the raw count of findings a path happens to publish.

**Why §26.6's fact-check pass does not violate that invariant.** Full argument at §26.6's own
close; in short, the pass may kill a finding only on **proof** it is wrong from the diff alone,
never on suspicion, and must let anything merely uncertain through — the asymmetry is precisely
what keeps "removes findings" from meaning "subtracts rigor." A pass built to kill on suspicion
would violate this invariant and has no place on either path; none is proposed here.

**Mechanism 3's within-deep-path axis stays out of v1 — resolved, not left silent.** §26.3
already validates, independently, the light/deep fork itself — a PR-level, add-only decision the
invariant above governs. A finer axis it suggests — skipping `architecture-scribe` or
`counter-reviewer` *within* an already-deep-routed PR — is a genuinely different question the
original invariant never spoke to, because deep path was previously monolithic (both sub-agents,
always, once routed deep). It is **moot for the light path** (nothing to skip: no scribe, no
counter-review exist there to begin with) — the live question is only ever the deep path's own
internal composition, a new axis this plan did not previously name. V1 answers it with **no**:
every one of deep path's v1 triggers (§26.3 — sensitive-glob hit, >600 lines, ≥3 path roots) is, by
construction, a PR the router judged to need full rigor; a secondary, finer signal that skipped
scribe or counter-review on such a PR could reintroduce risk on precisely the case deep routing
exists to cover — most concretely, a small sensitive-glob-triggered diff (a three-line migration)
is exactly the shape a size-based secondary gate would be tempted to downgrade, and exactly the
shape that must not be. The invariant above is hereby extended to cover this axis explicitly: no
v1 mechanism skips `architecture-scribe` or `counter-reviewer` on a PR already routed deep, for any
reason short of §26.7's own cost ceiling. **That carve-out is narrower than an earlier draft of
this sentence stated, and the gap it left is closed here rather than papered over.** The draft
justified the carve-out by saying the ceiling "floors `Shippable` when it fires, never a silent
downgrade" — true for `counter-reviewer` and for §26.6's fact-check pass, whose skips are both
recorded in already-specified typed fields, and **false for `architecture-scribe`**, which has no
such field: a budget-triggered scribe skip would floor nothing and appear nowhere, which is
precisely the silent downgrade the invariant forbids. Nor does dispatch order rescue it — §26.7
itself dispatches the three "in whatever order that orchestration dispatches them", so a scribe
skip cannot be assumed to imply a later, recorded counter-review skip. The resolution follows from
§26.7's own stated economy (it explicitly declines to grow a parallel outcome field per new way a
pass can end up skipped): **`architecture-scribe` is excluded from budget gating entirely.** The
ceiling governs `counter-reviewer` and the fact-check pass only — the two whose skips are already
recorded. This deliberately buys a slightly weaker bound in exchange for the stronger invariant,
the same trade §26.7 already makes when it refuses to budget-gate the primary findings pass. Adding
a typed scribe-skip field and gating it too remains the open alternative, and would need §26.7's
field-economy argument reopened rather than quietly overridden. §26.6's fact-check pass is exempt from this floor for the same reason it is exempt from
the light-path invariant above — it was never a rigor-bearing pass to begin with, not because of
which path it runs on. A future, telemetry-justified version of within-deep gating remains open,
mirroring §26.3's own "v2 option only if per-path analytics show a real grey zone" deferral for the
LLM tie-break — not designed now.

**Per-mechanism v1/deferred status, stated once, for all four:**
1. Diff-only fact-check pass (§26.6) — **v1, Step 69**, both paths.
2. Content-anchored positioning + relocation fallback (§22.1.1) — **v1, Step 63**, folded into
   that Step's original scope; no separate Step, no migration.
3. Pass-routing as a validated extension of triage (§26.3) — the citation strengthening §26.3's
   existing light/deep rules is **v1 documentation**, no new behavior beyond those rules; the
   within-deep-path gating it further suggests is **explicitly deferred**, per above.
4. Per-review cost budget with look-ahead (§26.7) — **v1, Step 69 design, Step 70 wiring** (the
   `turnState.spentUSD` accumulator and the loopback `GET /review-cost-budget` production call site,
   §26.7's own updated text), both paths, per-path ceilings.

### 26.10 Risks and open questions

- **Buy vs. build: a delegation-mode spike, not a decision.** OpenCodeReview (§22.1.1's full
  citation) ships both a Claude Code plugin and a "delegation mode" — deterministic scope/rule
  resolution done by the tool itself, then the actual review handed to the host agent using the
  host's own subscription rather than a separate API key. That last part is notably the same
  ToS/cost constraint already hit once in this plan for background agents running under a Claude
  subscription (§29 is the closest analogue here). A short spike running `ocr` inside a review
  sandbox, scoped to generating only the line-level findings layer (§26.6's fact-check pass and
  §22.1.1's positioning — the two mechanisms it already validates in production), could plausibly
  be faster than reimplementing both from this plan alone. Two concrete frictions, named
  rather than papered over, are why this stays a spike and not a decision: (1) **output-format
  mismatch** — its findings would need to be adapted into `reviewpost.Finding`/the typed
  `Digest`/`Shippable` verdict this plan's whole pipeline is built on (§21's typed verdict, §26.1's
  reject-don't-repair posture at the posting endpoint) rather than free text; (2) **rule-engine
  mismatch** — its own scope/rule resolution is a second, parallel mechanism to §22.2-§22.4's
  repo-scoped learned-pattern table, and reconciling or replacing one with the other is unscoped
  work in its own right. Recorded here as an open question for whoever picks up Step 69, not
  resolved by this amendment.

### 26.11 Phasing

Steps 66-69, end of Phase 5. 66
(digest) → 67 (adequacy — needs 66's diff-derived summary as its reference text); 68 (triage) is
independent of 66-67 and valuable alone (model/effort tiering per path) but sequenced before 69
because the deep path must exist to route to; 69 (counter-review + measurement, **now also §26.6's
fact-check pass and §26.7's cost budget**) needs 66's digest structure and 68's deep path, and
rides Step 62's instrument. 66 extends Step 45's domain type, Step 47's posting tool, and Step 62's
persistence — hence the whole chantier sits after Steps 62-65. §22.1.1's snippet anchoring ships
inside Step 63, not this chantier, and has no dependency on Steps 66-69. Steps 70 (§26.7/§26.9's
cost-budget wiring) and 71 (§26.4's post-hoc sub-task corroboration) close this chantier's two
named residuals and sit immediately after 69, still inside Phase 5 and still before its milestone —
review/automation scope belongs in Phase 5 regardless of when it ships, the same standing rule
IMPLEMENTATION_PLAN.md's own Phase 5/6 boundary states. UI: the review screen's
readout layout (digest first, collapsed appendix, contested-points block) lands with the existing
review view Step (Step 84, Phase 7); no new screen.

## 27. Enterprise sandbox glue (detailed design)

§8 item 5 names seven capabilities in one line and, until this section, nothing in this plan
designed any of them — the audit that produced this section found every term in that bullet
(`kubeconfig`, `Docker-in-sandbox`, `OpenCode config storage`) appearing exactly once in the whole
document, with no Step citing it. What unifies the seven is that each is a point where a
**customer's own infrastructure meets the sandbox**: their cloud accounts, their clusters, their
container tooling, their network policy, their secrets, their agent-engine configuration, their
browser/toolchain needs. Two already-shipped anchors are reused throughout rather than reinvented:

- **The sandbox-bearer delivery channel.** Secret material never travels through the provider API
  inside SESSION_CONFIG (the one deliberate exception is the sandbox's own bootstrap bearer token,
  §5.2) — it is *pulled* by sandbox-agent at boot over the authenticated sandbox→CP channel, with
  the exact handshake `scmcredentials.go` established and `providercredentialsdelivery.go` (Step
  53) already mirrors once: bearer-token verification (constant-time hash compare), dead-sandbox
  410, `X-Sandbox-Gen` fencing 403. Every new delivery endpoint below mirrors it again, in the
  same order, for the same audit-established reasons.
- **Step 53's scope/resolution/crypto vocabulary.** `internal/domain/providercredential`'s
  `Scope`/`Resolve` (already generic over `Candidate[T]`), the partial-unique-index pattern
  (migration 000056), `platform.EncryptToken`'s AES-256-GCM at rest, and the write-only management
  API posture are the mechanisms; this section extends their use, never forks a parallel set.

Ordering below is by dependency, not the bullet's own comma order: general secrets first (§27.1),
because OpenCode config (§27.2), cloud identity (§27.3), and kubeconfig (§27.4) all lean on its
storage or delivery machinery; the substrate pieces (Docker §27.5, egress §27.6, toolchain §27.7)
close. §19.8's recorded invariant is honored throughout: nothing in this section ever passes a
user-configurable value into `BuildImage` — rule (a), boot-time injection only.

### 27.1 Repo/environment/global secrets

**A second table, deliberately — not an extension of `provider_credentials`.** Step 53's table is
narrow by design: its identity column is a closed Postgres ENUM of three provider names, each
mapped to fixed env-var name(s) by domain code (`providercredential.EnvVarNames`), consumed by
exactly one process (the `opencode serve` env, `spawn.go`). A general secret inverts both
properties: its identity IS a user-chosen env-var name, and its consumers are the whole supervised
process tree (hooks, `services.yml` services, the agent's own shell). Widening the ENUM-typed
column into free text would destroy the closed-vocabulary property Step 53's own docs treat as
load-bearing; so: new table, same idioms.

```
sandbox_secrets(id, scope sandbox_secret_scope ENUM('automation','environment','repo','global'),
                scope_target_id TEXT, name TEXT, value_encrypted BYTEA, created_at, updated_at)
```

- Same shape CHECK and partial-unique-index pair as migration 000056 (`(scope, scope_target_id,
  name)` where target NOT NULL; `(name)` where NULL); `scope_target_id` meanings identical
  (repo = `repo_full_name`, environment = `environments.id`), plus `automation` = `automations.id`.
- **Resolution order: automation → environment → repo → global, most specific wins** — the order
  §12.2 item 5's Settings mockup already displays and `providercredential`'s own doc.go already
  verified for its three scopes; `automation` slots in as the most-specific level.
  `providercredential.Resolve` is reused as-is (it is already generic); only the `scopePriority`
  table gains the fourth row. The `automation` scope ships in the schema now so the deferred
  per-automation secrets follow-up (§8.4/Step 52's explicit deferral, `automation/doc.go`) needs
  no second migration — but its CRUD and consumption wiring remain that follow-up's scope, not
  this Step's.
- **Name validation, fail-closed at save time**: POSIX env-var shape; the `NARVI_*` namespace
  rejected outright (§19.8's reservation — the live namespace is the eight `NARVI_*` vars
  `boot/config.go:33-40` already defines); the entire `OPENCODE_*` namespace rejected outright
  too (see §27.2 — a whole namespace rather than the two names that are live today, because
  every OpenCode env var OpenCode itself may add later is by construction a slot that outranks
  or redirects something Narvi injects, and a name-by-name list would go stale the moment the
  pinned engine version moves); the exact names `providercredential.EnvVarNames` covers
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, the three Google names) rejected too, so every env-var
  name has exactly one owning mechanism and a shadowing conflict between the two tables is
  unrepresentable.
- **The same validation runs again at the injection boundary**, not only at save time:
  sandbox-agent re-runs it over the map the delivery endpoint returns and drops what fails,
  per-entry, warn-and-continue. Save-time validation alone would make the reservation a rule
  every future writer has to remember, which §30 rules is not a guard; re-validating at the one
  point where a name actually becomes an env var makes the shadowing unreachable however the row
  got there — a second write path added by a later Step, a hand-run `INSERT`, or a control plane
  rolled back to a build predating the reservation.
- **Encryption, RBAC, management API**: `platform.EncryptToken` at rest; the same three
  already-reserved actions (`ActionManageRepoSecrets`/`ActionManageEnvSecrets`/
  `ActionManageGlobalSecrets`) govern this table exactly as they govern `provider_credentials` —
  one management surface, two tables behind it; write-only from the management API
  (`providercredentials.go`'s posture), values never returned, never logged.
- **Delivery**: sibling sandbox-facing endpoint `POST /sessions/{id}/sandbox-secrets` mirroring
  `providercredentialsdelivery.go`'s handshake verbatim; response is a plain name→plaintext map of
  RESOLVED winners only, losers never decrypted (Step 53's decrypt-only-the-winner discipline).
- **Injection**: sandbox-agent fetches once, before the first hook runs, with bounded retry;
  threads the map into every process it spawns — hooks (through `runRepoHooks`' existing
  `EnvWithout` seam, `hooks.go:141`), `services.yml` services, and `opencode serve` (appended
  *before* `providerCredentialEnv`, so the ordering question is moot anyway given the disjoint-name
  rule above). Degrade policy on fetch failure: **warn and continue** — recorded in the boot log
  and `AGENTS.md`, never a boot failure — the same reasoning §19.4 already settled for a failed
  setup rerun: a running agent that can diagnose a missing env var beats a dead sandbox, and
  "never block a spawn" (§10-P2) holds.
- **What this mechanism does NOT claim**: in-sandbox secrecy from the agent is a non-goal — the
  agent is the intended consumer. The real boundaries are: encrypted at rest CP-side; never
  through the provider API; never written inside any repo working tree (so never committable);
  never logged. The residual risk that an agent *writes* a secret value into code it then pushes
  is shared with every secrets mechanism in every CI system; output redaction is possible future
  work, not claimed here.

### 27.2 OpenCode config storage + injection

What OpenCode config actually is (verified — partly live in this codebase, partly against
OpenCode's own docs): JSON config (`opencode.json`(c)) merged across an ordered source list,
later-wins — remote → **global** (`~/.config/opencode/opencode.json`) → **custom**
(`OPENCODE_CONFIG` env var) → **project** (workspace `opencode.json` — the slot Step 48's
sentinel-fix agent write already targets and verified loading, `sentinelfixagent.go`) — carrying
providers/models, MCP servers, agents, permissions, LSP config, and `{env:VAR}`/`{file:path}`
substitution. That merge order is the entire injection design — Narvi occupies OpenCode's own
documented slots rather than inventing a merge:

- **Storage**: `opencode_configs(scope ENUM('environment','global'), scope_target_id, document
  JSONB, timestamps)` — same CHECK/partial-unique-index idioms as §27.1; at most one global row,
  one per environment. **Plaintext JSONB, deliberately** — this is configuration users read and
  edit in Settings, not secret material; anything secret belongs in §27.1's table and is
  referenced from the document via OpenCode's own `{env:VAR}` substitution, which resolves at
  OpenCode's load time from the process env sandbox-agent already built. Validation at save:
  parses as a JSON object, bounded size — nothing deeper, because OpenCode's own schema drifts
  with its version and a Narvi-side copy of it would be a second, staler validator (§7's
  pinned-binary contract tests are where engine-shape drift is caught).
- **Injection**: delivered at boot over a sibling sandbox-facing endpoint (same handshake), both
  scopes at once: sandbox-agent writes the global document to
  `~/.config/opencode/opencode.json` (OpenCode's global slot; base images must not bake one) and
  the environment document to a file outside the workspace, setting `OPENCODE_CONFIG` to its path
  on the `opencode serve` process. OpenCode's own precedence then composes everything correctly
  with zero Narvi-side merge code: org-global < environment < repo-committed project config < the
  sentinel-fix capability-restriction write (§17.2/Step 48, which targets the project slot) —
  i.e. **a customer-authored config can never override the security-relevant agent restriction**,
  *provided nothing else can reach a slot above project*. That proviso is load-bearing and is
  exactly what `sandbox_secrets`' `OPENCODE_*` reservation buys: `OPENCODE_CONFIG_CONTENT` is
  OpenCode's **inline** slot, which outranks project, so an environment-scoped secret under that
  name would re-enable unrestricted tools for every capability-restricted sentinel-fix session in
  that environment — and `OPENCODE_CONFIG` would redirect the environment document this Step owns.
  Neither is expressible because the namespace is reserved on both the write and the injection
  path; the guarantee above is stated as structural on that basis and on no other,
  by the engine's own documented ordering, not by a Narvi convention.
- **RBAC**: global scope admin-only (the §13.3 row that owns integrations/global secrets);
  environment scope maintainer+ (the row that owns environments/env secrets).
- **Trust note, stated plainly**: an org-authored OpenCode config can name MCP servers and
  commands — code execution inside the sandbox. This grants no privilege a repo's own committed
  `opencode.json` or `setup.sh` does not already have (the sandbox runs untrusted repo code by
  design); the management surface above is the gate, and it is role-checked server-side like
  every other state change (§13.3).

### 27.3 Cloud credentials via OIDC (provider-agnostic)

The pattern is the one GitHub Actions standardized for CI↔cloud federation, applied to sandboxes:
**Narvi's control plane becomes an OIDC identity provider; the customer's cloud IAM is configured
to trust it; the sandbox exchanges a short-lived Narvi-signed identity token for short-lived cloud
credentials via the cloud's own STS.** Narvi stores no cloud credential of any kind, ever — the
customer-side trust policy IS the grant, and revoking it is the customer's own kill switch.

- **Issuer**: CP serves `GET /.well-known/openid-configuration` + a JWKS endpoint on a new
  `platform.Config` public issuer base URL, validated at boot; the whole capability is off (and
  binding CRUD refuses, fail-closed) when unset. Signing keys: RS256; private keys generated
  CP-side and encrypted at rest with `platform.EncryptToken` (`oidc_signing_keys(kid,
  private_key_encrypted, public_jwk, created_at, retired_at)`); rotation publishes old + new in
  the JWKS for an overlap window ≥ max token lifetime — the same overlapping-validity discipline
  §5.2 already applies to sandbox-token rotation.
- **Claims**: `iss` = the issuer URL; **`sub` = a stable, deterministic, per-Environment value**
  (`narvi:environment:<environment_id>`), because `sub` is the one claim every cloud can condition
  on and Azure's federated-credential matching requires an exact, predictable subject string —
  never anything session-varying in `sub`. Session-varying context (`session_id`, `gen`, repo
  full names, provenance tag) rides as additional custom claims for clouds whose condition
  languages can use them (GCP's attribute mapping; AWS condition keys). `aud` = per-binding,
  customer-set — each cloud documents what it expects (`sts.amazonaws.com` for AWS; the workload
  identity provider resource name for GCP; `api://AzureADTokenExchange` for Azure). `exp` ≈ 10
  minutes.
- **Bindings** — what connects an Environment to a cloud role: `cloud_identity_bindings(scope
  ENUM('environment','global'), scope_target_id, kind ENUM('aws','gcp','azure','generic'),
  audience, params JSONB, timestamps)`, at most one per (scope target, kind) in v1. `params` are
  identifiers, not secrets (AWS: role ARN; GCP: workload-identity-provider resource name +
  optional service-account email; Azure: client id + tenant id; generic: the env-var name to
  publish the token path under) — stored plaintext, readable, maintainer+ managed (the §13.3
  environments row). **Deliberately no repo scope**: a deployment target is an Environment
  property (§14.1's own model — confirmed, not just assumed), not a repo property.
- **Minting**: sandbox-agent calls `POST /sessions/{id}/cloud-identity-token {audience}` over the
  sandbox-bearer channel (same handshake as every delivery endpoint here). CP refuses any audience
  no binding for this session's Environment (or global fallback) declares — it never mints
  arbitrary-audience tokens. Minting is logged with `correlation_id` (§5.3) and counted as a
  metric; `audit_log` records binding CRUD, not each 5-minute refresh (proportionate, or the audit
  log becomes noise).
- **In-sandbox consumption — file-based, zero custom tooling**: all three clouds' SDK families
  natively consume a *file-sourced* OIDC token via standard env vars, so sandbox-agent maintains
  one token file per binding under a non-workspace path (`/narvi/identity/`, 0700/0600 — never
  inside any repo tree, so never committable), refreshes each at token half-life (background,
  same supervisor discipline as everything else it runs), and sets the standard env vars on every
  spawned process: `AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN` (+ session name) for AWS's
  `AssumeRoleWithWebIdentity` flow; `GOOGLE_APPLICATION_CREDENTIALS` pointing at a generated
  external-account credential-config JSON whose `credential_source.file` is the token file, for
  GCP's STS exchange; `AZURE_FEDERATED_TOKEN_FILE` + `AZURE_CLIENT_ID` + `AZURE_TENANT_ID` for
  Azure workload identity. The clouds' own client libraries perform the exchange in-sandbox;
  **Narvi implements no per-cloud exchange code at all** — that is what "provider-agnostic"
  concretely means here, and why a fourth target (Vault, or any JWT-federating system) is just a
  `generic` binding, not a CP change.
- **Boundary**: what a compromised or over-eager sandbox can do cloud-side is exactly what the
  customer's own trust policy + role grant to that Environment's `sub` — least privilege is the
  customer's lever; Narvi's job is making `sub` fine-grained, stable, and honest. Lifetime bounds
  the tail (≤10-min tokens; minting stops at dead-sandbox/410, like every other delivery
  endpoint); a leaked token is useless against any role whose trust policy names a different
  `sub`/`aud`.

### 27.4 Kubeconfig injection for the target cluster

"The target cluster" is selected the way §14 already models deployment targets: **per-Environment**
— `cluster_bindings(environment_id UNIQUE, name, server_url, ca_bundle, auth_kind
ENUM('cloud','oidc','static'), params JSONB)`, one cluster per Environment in v1 (the bullet's own
singular). sandbox-agent renders a kubeconfig from the binding at boot, writes it under
`/narvi/identity/` (never a repo tree), and sets `KUBECONFIG` on every spawned process. Three auth
rungs, preferring federation over static material:

1. **`cloud`** — EKS/GKE/AKS ride §27.3's already-established identity with zero additional
   secret: the rendered kubeconfig uses the standard exec credential plugin for that cloud
   (`aws eks get-token` / `gke-gcloud-auth-plugin` / `kubelogin` in workload-identity mode), each
   of which consumes exactly the env vars §27.3 already set. Kubernetes' client-go
   exec-credential mechanism does the rest; the toolchain image (§27.7) carries the three plugins.
2. **`oidc`** — a self-managed cluster whose kube-apiserver is configured to trust Narvi's own
   issuer directly (`--oidc-issuer-url` + client-id + claim mappings): the kubeconfig authenticates
   via its own **`tokenFile`** field (client-go's documented mechanism — `tools/clientcmd/api.
   AuthInfo.TokenFile`: "periodically read... the last successfully read content is used as the
   bearer token"), pointed at a token sandbox-agent mints via the SAME CP endpoint §27.3's cloud
   bindings already mint through (`aud` = the cluster's configured client id) and refreshes at
   half-life through the SAME background loop — structurally identical to a §27.3
   cloud_identity_bindings token, not a separate mechanism. Authorization inside the cluster is the
   customer's own RBAC binding on the token's claims (recommend a namespace-scoped Role, never
   cluster-admin — documented, not enforced, since the cluster is the customer's).
   **This corrects an earlier version of this rung** (found structurally non-functional by
   adversarial review, Step 73): the original design gave this rung its own exec-plugin
   subcommand (`kube-credential`), justified as "the exact same shape as the git-credential-helper
   subcommand precedent (`runCredentialHelper`)". That analogy does not hold: git's own credential
   helper is spawned **by sandbox-agent itself** (`gitclone`'s clone/push calls inherit
   sandbox-agent's own environment on purpose), so its re-exec of this binary inherits
   `NARVI_SESSION_CONFIG` from sandbox-agent's own process. `kubectl`, by contrast, is only ever
   run by the agent **inside** the already-stripped `opencode serve`/hook/services.yml process tree
   — sandbox-agent never spawns `kubectl` itself — and every one of those three spawn call sites
   deliberately strips `NARVI_SESSION_CONFIG` before handing the child its environment (§25/§27.1's
   own "no legitimate need to see the sandbox's own plaintext bearer token" reasoning, applied
   identically at each site). The `kube-credential` subcommand therefore had no environment to ever
   read that variable from and failed on every invocation. The `tokenFile` mechanism above needs no
   such re-exec and no new IPC surface at all — client-go reads the credential straight off disk,
   the same way the three cloud SDKs already do for §27.3's own tokens.
3. **`static`** — an uploaded kubeconfig for clusters with no OIDC path at all, stored as a §27.1
   secret (the value is the file content; delivered and written to disk by sandbox-agent, never
   env-var-expanded). Supported honestly as the lowest rung and named as such: long-lived
   credential material at rest, exactly what the two rungs above exist to avoid.

### 27.5 Docker-in-sandbox

The well-known hard problem, named rather than hand-waved: nested containers need either privilege
(classic privileged-mode DinD — **rejected outright here**: it is root-equivalent on the host and
incompatible with §5.2's fail-closed/least-privilege posture), a syscall-emulating or user-ns
runtime (gVisor, rootless, sysbox — each with real compatibility limits), or a real kernel per
sandbox (microVM). The decisive architectural fact: **Narvi's sandboxes run on a provider's
substrate, so the isolation technology is the provider's, and the decision surfaces through the
existing port, not a new mechanism**:

- **Per-Environment `docker: required` flag** → carried in SESSION_CONFIG and, like `Gen`, also as
  a top-level `CreateSpec` field (the same deliberate-duplicate-with-`Validate` discipline
  `createspec.go` already documents — the provider must act on it without parsing the opaque doc).
  `ports.Capabilities` gains `DockerInSandbox bool`.
- **Fail-closed, twice**: session creation against an Environment requiring Docker is refused
  up-front when the configured provider reports no support (clearest possible UX), and the spawn
  path re-checks at dispatch — a Docker-requiring session is never silently run somewhere the
  requirement is unenforceable.
- **Modal concretely** (researched for this section, current as of writing): default Modal
  sandboxes run on gVisor, where dockerd's overlay2/bridge-networking stack does not run cleanly;
  Modal's VM runtime option gives the sandbox a real kernel, where Docker/compose/build behave
  normally. The Modal adapter maps the flag onto that runtime option. The costs are real and
  named: VM-runtime boot latency vs §19's warm-boot expectations, snapshot-capability parity
  under a different runtime (see §27.8 — `Capabilities()` is flat today and cannot express
  per-spawn capability variance), the option's experimental status, and per-sandbox cost.
- **The anticipated Kubernetes-native provider** (§0): sysbox-class user-ns runtimes are the
  recommended enablement path, Kata-class microVMs the stronger-isolation alternative —
  **privileged pods never**, under any configuration this plan ships.
- **In-sandbox**: when the flag is set, sandbox-agent supervises `dockerd` as one more entry in
  the same process-supervision table as everything else (§14.2's own "no new supervision code
  path" rule), with a named `boot_progress` phase; the CLI/engine binaries come from the toolchain
  image (§27.7) and the daemon simply never starts when the flag is off.

### 27.6 Egress: what §4.1 already covers, and the sandbox-side gap

The bullet's word "egress proxy" is **half-covered**. Fully covered already, needing no new
design: the control plane's own outbound traffic to the provider API routes through the
configurable proxy (§4.1; `ModalEgressProxyURL`, `modal/provider.go`'s Transport wiring) — that
shipped with Step 12. Not covered anywhere until now: the **sandbox's own egress**. Two halves,
because they are genuinely different guarantees:

- **Cooperative routing** — `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` across the sandbox process
  tree: this is *pure §27.1* — proxy URLs are secret-shaped (they conventionally carry basic-auth
  credentials; `modal/errors.go` already redacts them for exactly that reason), so a customer
  configures them as environment-scoped secrets and every spawned process inherits them. Zero new
  machinery; named here so nobody builds a parallel mechanism. Explicitly NOT enforcement — any
  process can ignore env vars.
- **Enforced policy** — per-Environment `egress_policy {mode: open|allowlist, allowlist}`,
  carried like the Docker flag (SESSION_CONFIG + top-level `CreateSpec`), **enforced at the
  provider substrate** (Modal's own sandbox network controls; NetworkPolicy for the anticipated
  Kubernetes provider), surfaced as `Capabilities.EgressPolicy` and **fail-closed** exactly like
  §27.5: a policy the configured provider cannot enforce refuses the spawn, never runs
  unenforced. A non-negotiable allowlist floor is auto-appended server-side — the CP's own
  WS/API host, the session's git hosts, and nothing less — because a sandbox that cannot reach
  the control plane or clone its repos is not a security posture, it is a boot failure.

### 27.7 Toolchain in images

Confirmed genuinely mechanical — a content addition to the **base sandbox image**, no new
architecture. The base image is the `Base` every build and cold boot already starts from
(`defaultBaseImage`, `dispatch.go:153` — today a placeholder tag; its real build definition is
where these land), NOT the per-repo-set prebuild pipeline (§19 bakes repos and dependency caches
*on top of* base; tools belong below). The additions: Playwright + Chromium (installed with its
OS deps, pinned), `ripgrep`, `typescript-language-server` (+ `typescript`), the Docker CLI/engine
binaries (daemon dormant unless §27.5's flag is set), and §27.4's three cloud exec-credential
plugins. Versions pinned and visible via the boot fingerprint's existing image digest (§5.3). The
one real consideration: Chromium adds on the order of a gigabyte, which prices into image pull
time — absorbed in practice by §19's shared warm images and already inside what
`first_connect_budget` is sized for (§5.4). That is the entire design; stating more would be
manufacturing complexity.

### 27.8 Risks and open questions

- **Public-issuer reachability (§27.3)**: AWS and Azure fetch issuer discovery/JWKS over public
  HTTPS — a firewalled self-hosted CP cannot federate with them directly. GCP accepts an uploaded
  JWKS (no public issuer needed). For fully air-gapped deployments the honest answer today is
  §27.4's `static` rung and §27.1 secrets; a token-exchange relay is out of scope until someone
  actually needs it.
- **Key-rotation cadence (§27.3)**: manual, admin-triggered rotation with the overlap window is
  v1; automatic scheduled rotation is deferred until operational experience says what cadence is
  right. Clock skew between CP and cloud STS endpoints bounds how short `exp` can safely go.
- **`sub` granularity (§27.3)**: per-Environment is the designed grain. If a real customer needs
  per-repo cloud scoping inside one Environment, that collides with Azure's exact-match subject
  requirement and needs its own design pass — named now, not solved speculatively.
- **Per-spawn capability variance (§27.5)**: `Capabilities()` is a flat, provider-level report
  (§4.1); a provider whose snapshot support differs by runtime (Modal gVisor vs VM runtime) cannot
  express that today. If VM-runtime sandboxes turn out snapshot-incapable, either `Capabilities`
  grows a per-spec dimension or Docker-requiring sessions document degraded recovery (resume-only,
  §3.2) — decided at Step 74 implementation time against the provider's real behavior, not
  guessed here.
  **Resolved at Step 74 implementation time, per this bullet's own instruction — not by real-
  provider verification (there is none to check against: `internal/adapters/outbound/modal/doc.go`
  is explicit that every wire shape this codebase's Modal adapter speaks is its own invention,
  tested against a fake `httptest.Server`), but by refusing the unverifiable operation rather than
  assuming it away.** `Capabilities` itself was NOT grown a per-spec dimension — that would have
  let a caller ask the provider a real, structural question about VM-runtime snapshot support this
  codebase has no way to answer honestly today. Instead, both halves of the operation are refused
  for a Docker-required session: `sandboxevent.go`'s `triggerSnapshotBestEffort` never triggers a
  snapshot for one in the first place (so `snapshot_id` can never become non-empty via this
  codebase's own normal flow), and `dispatch.go`'s `tryPlanSpawn` downgrades a
  `SpawnActionRestore` decision back to a plain `SpawnActionSpawn` as defense in depth against any
  OTHER way a stale `snapshot_id` could reach that column. The degraded-recovery half this bullet
  names is now real and named, not speculative: a Docker-required sandbox's only recovery path is
  a fresh respawn, losing whatever in-progress state a snapshot would otherwise have preserved.
  Proven by resilience scenario #17 ("restore-with-docker", `test/resilience/README.md`) —
  `internal/app/sessionactor/snapshot_docker_integration_test.go`.
- **Build-time secrets (§27.1, §19.8)**: rule (a) means shared-image builds run `setup.sh` with
  no user secrets. A `setup.sh` that hard-requires one (private package registry) fails its
  builds (fatal in `BootModeBuild`) and that Environment degrades to base-image cold boots — where
  the boot-time rerun DOES have the secrets and succeeds. Correct, but slow. §19.8's rule (b)
  (env-digest joins the fingerprint) is the designed escape if this bites a real Environment; not
  built until then.
- **Enforced-egress granularity (§27.6)**: whether the provider substrate enforces by domain or
  by CIDR (and how DNS is handled inside the allowlist) is provider-specific and must be verified
  against the provider's real controls at implementation time — the fail-closed rule above is what
  keeps this honest either way.
- **Snapshotting a running dockerd (§27.5)**: daemon/image-store state inside snapshots
  (§3.2/§8.5's snapshot-restore path) is untested territory; Step 74 must add a §9.3-class
  scenario for restore-with-docker before claiming it works.
  **Resolved at Step 74: not claimed to work, and structurally prevented from being attempted at
  all** — see the per-spawn-capability-variance bullet above for the mechanism (the same fix closes
  both bullets, since a snapshot of a running dockerd is exactly the untested state a restore would
  otherwise resurrect). Scenario #17 (`test/resilience/README.md`) is the §9.3-class scenario this
  bullet asked for, proving the refusal holds under a real dispatch cycle rather than merely being
  documented.

### 27.9 Phasing

Steps 72-74, opening Phase 6. Placed there,
not in Phase 5, because this is rollout-enabling platform glue, not review/automation scope:
§10-P6's own first line ("Config setup (automations, secrets, environments, settings,
integrations)") already presumes these surfaces exist, and the config/data-seeding Step that opens
the rest of Phase 6 seeds exactly what these Steps build. Step 72 (§27.1 + §27.2) extends Step
53's mechanisms and needs Step 39's `Authorize`; Step 73 (§27.3 + §27.4) builds on 72's delivery
family and the `static` rung stores through 72's table; Step 74 (§27.5 + §27.6's enforced half +
§27.7) is the ports/substrate piece (Steps 12-16's seams, §19's image pipeline adjacency) and can
run in parallel with 73. UI: no new screens — the Settings view (§12.2 item 5, Step 86) gains the
secrets table it already mocks plus cloud/cluster bindings, per-Environment Docker/egress
settings, and the OpenCode config editor.

## 28. Uploads, blob storage & the in-sandbox `download_file` tool (detailed design)

§8.6 states the feature as an exit criterion — "uploads to object storage (S3-compatible) +
`download_file` tool in sandbox; failed-upload UX signal" — and `BlobStore` appears only in §4.3's
port list, with no signature and no design anywhere else in this plan; §12.2 item 1's artifacts
panel and §6.3's `uploads` route name the *surfaces*, not the mechanism. This section fixes the
port, the transport decision, the key/limit/credential model, and the tool's actual wire protocol,
so Step 58 doesn't have to invent them under time pressure (§18's own precedent for closing exactly
this kind of gap).

Two flows, one mechanism. Files move in both directions: a user attaches a file to a prompt (a
screenshot, a spec, a CSV) for the agent to consume, and the agent produces media the session rail
must surface — §12.2 item 1's rail already plans "artifacts: PR / preview / uploads", and the
`artifacts` table has carried an `upload` enum value since Step 4
(`migrations/000012_artifacts.up.sql`) with, per that store's own doc comment, no producer yet.
Both directions are the same design: the control plane mints a short-lived presigned URL against
S3-compatible storage, the bytes move directly between the client (browser or sandbox) and storage,
and the CP verifies the result after the fact and records it in Postgres.

### 28.1 The `BlobStore` port (complete — no out-of-interface operations)

```go
type BlobStore interface {
    PresignPut(ctx, PresignPutSpec) (PresignedURL, error) // Spec: Key, ContentType, ContentLength, TTL
    PresignGet(ctx, PresignGetSpec) (PresignedURL, error) // Spec: Key, TTL, ResponseFilename
    Stat(ctx, BlobKey) (BlobInfo, error)                  // BlobInfo: SizeBytes, ETag — confirm-time verification
    Delete(ctx, BlobKey) error                            // idempotent: deleting an absent key succeeds
}
```

Errors are typed `BlobStoreError{Transient bool}` — classification by storage error code / HTTP
status class, **never** by string-matching messages — the same discipline §4.1 requires of
`ProviderError`, mirroring `ports/providererror.go`'s shape exactly ({Transient, Code, Op, Err},
one `Op` constant per method, and an `IsTransient` helper defaulting unclassified errors to
transient). `Stat` on an absent key returns a typed not-found sentinel (`ErrBlobNotFound`),
distinct from any transient failure — confirm-time verification (§28.4) branches on it, never on a
string.

- `BlobKey` is opaque to the adapter: only the CP's own key builder (§28.3) produces one; the
  adapter never parses or constructs keys.
- TTLs are passed in by the caller from `platform/timeouts.go` (§5.4) — the adapter holds no
  timeout literal of its own (§11's grep-test applies).
- Under SigV4, `PresignPut`/`PresignGet` are local signing operations — pure HMAC over the request
  descriptor, no network round-trip — so a presign cannot meaningfully fail transiently;
  `Stat`/`Delete` are real network calls and carry the full transient/permanent classification.
- `PresignedURL{URL, ExpiresAt, Headers}`: `Headers` are the exact headers the uploader must send
  (e.g. `Content-Type`) for the signature to verify; mint responses forward them verbatim.
- Deliberately absent, with reasons: no `Put`/`Get` streaming methods — nothing in this design
  requires the CP to touch bytes (§28.2), confirm-time verification is metadata-only (`Stat`), and
  §4.1's "complete" discipline cuts both ways (no speculative surface for a consumer that doesn't
  exist; a future feature that genuinely needs CP-side reads adds the method with that feature). No
  multipart-upload surface — the per-file cap (§28.4) sits far below every supported backend's
  single-PUT limit. No `List` — every object's key embeds the artifact row id that minted it
  (§28.3), so there is no orphan-blob class a bucket scan would find that the row-driven sweep
  (§28.4) doesn't already cover.

**Not sqlc-backed, deliberately.** §4.3 groups `SessionStore`/`TurnStore`/`SandboxStore` as
"(sqlc-backed)"; `BlobStore` is not in that trio and stays out: it is an outbound adapter over the
S3 HTTP API (`internal/adapters/outbound/objstore` — the package stub already names AWS S3, MinIO,
R2, GCS), exactly as `githubapi` is an adapter over GitHub's. The split preserves §5.1: object
storage holds **bytes only**, addressed by keys Postgres owns; every fact *about* an upload
(status, size, who, when, why it failed) lives on the `artifacts` row, sqlc-backed like every other
store. Object storage is never a second authority over state — a blob with no row is an orphan to
reap, never a record.

### 28.2 Transport: presigned URLs — bytes never transit the control plane

The decision the rest of this section hangs on: **the CP mints presigned URLs and the bytes move
directly between client and storage, in both directions. The CP never proxies a payload.**

The alternative — client POSTs bytes to the CP, the CP streams them to storage — was rejected on
the codebase's own already-demonstrated posture:
- §6.1's `artifact` event is the existing precedent for how media reaches clients: **by URL
  reference, never by value**. No binary payload rides the sandbox WS (its ack buffer is sized for
  control events — 1000 entries with eviction), and nothing in §6.2/§6.3 contemplates the CP as a
  byte funnel.
- Proxying couples the CP's connection/memory footprint and its timeout hierarchy (§5.4) to upload
  sizes and client link speeds — a max-size upload on a slow link would hold a CP connection open
  for minutes, competing with the WS/actor hot paths the CP exists to serve, and would need its own
  size-scaled timeout tier for no compensating benefit.
- A presigned URL is itself the credential shape §5.2 already prefers (§28.7): scoped to one object
  and one method, expiring in minutes — minting one is strictly cheaper and strictly safer than
  terminating the transfer.

What the decision costs, named honestly: (a) the storage endpoint must be **directly reachable from
browsers and from sandboxes** — a deployment requirement, carried by config (§28.7's
`PublicEndpoint`), with sandbox-side transfers subject to the same egress-proxy path the sandbox's
other outbound traffic uses (§8 item 5) — and once §27.6's per-Environment egress allowlist ships,
the object-storage host joins its server-appended floor (CP host + git hosts + storage host),
or uploads break exactly and only on allowlisted Environments; (b) the bucket needs a **CORS policy** allowing PUT/GET
from the web origin — a deployment-doc item alongside the bucket itself; (c) the CP no longer
observes the transfer — which is exactly why the lifecycle is two-phase (§28.4): the CP verifies
after the fact instead of watching bytes go by.

### 28.3 One bucket, per-session keys, isolation enforced at mint time

- **One configured bucket per deployment** (`platform.Config`, §28.7). Narvi deployments are
  single-tenant by construction — the domain has roles and identities but no org/team entity
  (§12.2 item 1's own note) — so the tenancy boundary IS the deployment: separate deployments,
  separate buckets and credentials, enforced by each deployment's own IAM policy scoping its
  credential to its one bucket.
- **Key convention**: `sessions/{session_id}/uploads/{upload_id}`, where `upload_id` is the
  artifact row's own UUID. The key carries **zero client-controlled bytes** — no filename, no user
  text (the filename lives on the row and is applied at download time via
  `response-content-disposition`, §28.5) — so path traversal, encoding surprises, and collision
  games are unrepresentable rather than validated away.
- **Within a deployment, session-level isolation is enforced at mint time by the CP, not by IAM.**
  Every presigned URL is minted only after the CP has already authorized the caller against that
  specific session (browser: cookie auth + `Authorize`; sandbox: bearer + gen handshake, §28.5),
  and only ever for a key under that session's own prefix. S3-compatible stores diverge too much in
  policy granularity (AWS IAM conditions vs. MinIO policies vs. R2 tokens) for per-prefix IAM to be
  the load-bearing mechanism across all of them: the deployment credential's bucket scope is the
  IAM layer's job; the CP — sole holder of that credential and sole minter — is the session
  boundary. The only storage credential a client ever holds is a single-object, single-method,
  minutes-lived URL.

### 28.4 Upload lifecycle: mint → transfer → confirm, verified server-side

The `artifacts` table gains upload lifecycle columns (one migration; existing `pr`/`preview` rows
take the `ready` default — they were only ever recorded after the fact, so `ready` is what they
always were):

```
artifact_status ENUM('pending','ready','failed');  status NOT NULL DEFAULT 'ready'
failure_reason  ENUM('size_exceeded','quota_exceeded','verification_failed','abandoned') NULL
blob_key TEXT NULL · size_bytes BIGINT NULL · content_type TEXT NULL · filename TEXT NULL
created_by UUID NULL REFERENCES users(id)  -- NULL = agent-produced (§17.5's no-human-actor allowance)
```

**Mint** (`POST` — the two auth variants in §28.5): the request declares `{filename, contentType,
sizeBytes}`. The CP checks the declared size against `MaxUploadBytes` (propose 100 MiB,
per-deployment config) and the session's running total against `MaxSessionUploadBytes` (propose
1 GiB — `SUM(size_bytes)` over the session's `pending`+`ready` uploads, derived from rows that
already exist, never a dedicated counter column, §25.5's own discipline), inserts the `pending`
artifact row (its `url` already the stable content path, §28.5), and returns `{uploadId, putUrl,
headers, expiresAt}`. An over-limit request is refused at mint: no row, no URL, a structured 4xx
naming the limit.

**Transfer**: the client PUTs the bytes to `putUrl` with the returned headers, within
`UploadPresignPutTTL` (propose 15 min — generous for the size cap on a slow link, the same "chosen
generously when the concrete cost is unknown" convention `HookTimeout` documents).

**Confirm** (`POST …/uploads/{uploadID}/complete`): the CP `Stat`s the object and verifies the
object exists, its actual size equals the declared size (the quota math above is only honest if the
declaration was), and both limits hold **re-checked now** — two mints racing past the session cap
is closed here, at the authoritative moment; mint-time checks are a fast-fail courtesy,
confirm-time checks are the enforcement of record, the same never-trust-the-earlier-render posture
as §16.2's re-validation-at-click. Passing: `pending → ready`, committed in the same transaction as
an appended session event (§28.6), broadcast only after commit per the `EventBroadcaster`
contract. Failing: `pending → failed` with the typed `failure_reason`, the same event append, **and
a `blob_delete` outbox entry** — an external delete is an outbound side effect (§5.1), and
fire-and-forget would leak the object forever on a crash between the status write and the delete;
the outbox's retry/dead-letter is exactly the guarantee a cleanup needs. Confirm is idempotent via
a guarded transition (`UPDATE … WHERE status = 'pending'`, the §25.6 idiom): a retried confirm of
an already-resolved row returns the recorded outcome, never re-verifies, never double-appends.
Presigned PUTs pin `Content-Length`/`Content-Type` in the signed headers where the backend honors
them, but the design never *relies* on that honoring — backend divergence again — which is why
`Stat`-at-confirm is the check of record.

**Abandonment sweep**: a `pending` row older than `UploadPendingSweepAfter` (propose 24 h) is
marked `failed(abandoned)` with a `blob_delete` outboxed (the object may half-exist), by the same
`app/scheduler` recovery-sweep machinery §3.5 already runs, on its own named interval in
`platform/timeouts.go` (propose 15 min). A browser that minted and walked away costs one row and
one sweep pass, nothing more.

**Retention is a named non-goal**: `ready` blobs live as long as their session rows do (sessions
are archived, never hard-deleted, §3.1); a retention/GC policy for archived sessions' media is
future work, recorded here the way §19.2 records image GC — named now so it gets scheduled
deliberately instead of discovered as a storage bill.

Ownership of these writes follows the existing split: artifact rows are not actor-owned state
(§2's single-writer rule covers session/sandbox/turn rows; `pushpr.go` already records PR
artifacts, and `coalesce.go`'s direct `github_pr_sessions` writes are the accepted precedent for
non-actor-owned rows, §24.1), so the upload handlers write them directly in their own
transactions. Whether the event append routes through a small actor command or the same
direct-write transaction is Step 58's own implementation decision (the §24.3 style of deferral) —
with the invariant fixed either way: row transition + event append commit atomically; broadcast
only after commit.

### 28.5 The `download_file` tool: a bearer-authenticated redirect, not a new wire type

**No new WS message types, in either direction.** The sandbox WS contract (§6.1) is untouched: no
new agent→CP event, no new CP→agent command. The established mechanism for "the agent needs
something from the CP mid-turn" is a sandbox-bearer REST endpoint — `scm-credentials` (§5.2/Step
21), `snapshot`, `review/verdict` (Step 47), `provider-credentials` (Step 53) — and in this
codebase "tool" already *means* exactly that: a CP HTTP endpoint the rendered turn prompt instructs
the agent to call, with the live URL/bearer/gen substituted into placeholder tokens by
`sandbox-agent` immediately before the prompt reaches the engine
(`cmd/sandbox-agent/reviewverdicttoolprompt.go`'s mechanism, reused — never a second substitution
scheme; no engine plugin/tool registration, no `AgentRuntime` change, §25.6's own "no new wire
command" posture). RPC-over-the-WS was rejected for the same reasons it wasn't used for verdicts:
the WS has no request/response correlation (`snapshot_ready` needed `commandMessageId` retrofitted
for even one-way correlation), and its buffer/ack machinery is sized for control events, not
transfers.

The endpoint — mounted like its siblings: outside `/api`, outside `auth.Middleware`, sandbox
bearer + `X-Sandbox-Gen`, with `scm-credentials`' own dead-sandbox/gen handshake:

```
GET /sessions/{sessionID}/uploads/{uploadID}/content
  → 302  Location: presigned GET (UploadPresignGetTTL, propose 5 min;
         response-content-disposition: attachment; filename="<row.filename>")
  → 404  uploadID unknown, not this session's, or not status='ready'
  → 403  bad bearer / gen mismatch        → 410  dead sandbox
```

One redirect makes the whole tool a single command: `curl -fL -H "Authorization: Bearer <token>"
-o <dest> <url>`. curl does not forward `Authorization` across a cross-host redirect by default, so
the storage endpoint never sees the sandbox bearer — and the presigned URL never appears in the
prompt, the transcript, or any persisted event; it exists only inside that one process's redirect
follow. Forcing `attachment` disposition means user-supplied content is never rendered inline off
the storage origin (an HTML upload must not become a page someone can be linked to — §5.2's
untrusted-content posture applied to serving).

**How the agent learns what to fetch**: the prompt-carrying REST DTOs gain optional
`attachmentIds: []uuid`, validated at the turn-creation chokepoint (`createTurnLocked` — the same
single shared core §23.2 already gates): every id must be a `status='ready'` upload artifact **of
this session**, else a structured 4xx — a failed or foreign upload can never silently ride a
prompt. The turn prompt then carries a deterministic, server-rendered attachment block (the
`review.RenderTurnPrompt` pattern): per attachment — filename, size, content type, and the exact
`download_file` command above with its placeholder tokens; `sandbox-agent` substitutes the live
values. A prompt with no attachments renders no block, and substitution on it is a byte-for-byte
no-op (`reviewverdicttoolprompt.go`'s own unconditional-substitution reasoning).

**The agent-produced direction** uses the same mint/confirm endpoints in their sandbox-bearer
variants (`POST /sessions/{sessionID}/uploads` → presigned PUT → `POST …/complete`, same 403/410
handshake), surfaced to the agent as a compact, deterministic tool note in build-turn prompts (same
server-side render + substitution; exact phrasing is Step 58's). The browser twins live inside the
auth-gated `/api` group (`POST /api/sessions/{id}/uploads`, `…/complete`), gated by a new
`Authorize` action mapped to the same §13.3 row as prompting (member+, own/joined sessions; viewers
never upload — the viewer guard holds). Browser downloads reuse the content route inside `/api`
(`GET /api/sessions/{id}/uploads/{uploadID}/content` → 302), gated by session visibility — a
download is a read, so read-only viewers may. The artifact row's `url` column stores this stable
`/api/…` content path, **never a presigned URL**: a presigned URL is an ephemeral credential, and
this codebase persists no live credential anywhere (§11's "tokens are always hashed" posture,
applied to a different credential shape) — every download mints a fresh one at click time.

Whether the web composer sequences create-session → upload → first prompt, or restricts attachments
to follow-up prompts, is a Phase 7 UI decision resolved against the mockups (§11); minting requires
an existing session id, and that constraint is this section's only contribution to the question.

### 28.6 The failed-upload UX signal

§8.6's "failed-upload UX signal" is **persisted status, not a toast**: the artifact row's
`status`/`failure_reason` (§28.4) is the durable fact, and the signal reaching live clients rides
the same channel every other session fact already rides — an appended event, broadcast and
replayable. The wire `artifact` event (§6.1) gains two additive, optional fields: `status:
ready|failed` (absent = `ready`, so every existing shape stays valid — the same
zero-producers-today additive reasoning `snapshot_ready.commandMessageId` used) and a nullable
`failureReason`; `SubscribedPayload.artifacts` elements and the REST artifact DTO carry the same
two fields, additively there too.

Upload `artifact` events are **CP-synthesized only** (the §3.3 synthetic-`execution_complete`
precedent; the schema documents the fields as never-populated-by-the-sandbox, §6.1's own
`subTaskId`-note convention). The sandbox never emits `artifact` for uploads: the CP already owns
the row before any bytes exist, and a sandbox-reported "I uploaded it" would be a second writer
over a fact Postgres already owns (§5.1's second-copy principle) — and an agent self-report this
system never trusts anyway (§21.2's discipline). The agent's confirm call is a *request to verify*:
the CP's `Stat` is what flips the status, and the confirm response tells the agent honestly whether
verification passed — so the model can retry once or tell the user — while the row and the event
already carry the truth regardless of what the model then says.

Rendering invents nothing: the session rail's artifacts panel (§12.2 item 1) shows a `failed`
upload with a status chip + reason where a `ready` row would show its download link, and the
timeline shows the same event the broadcast/replay stream already delivered. One signal, two
already-planned surfaces.

### 28.7 Credential scoping

The same shape §5.2 already fixed for git credentials, applied to storage:
- **The root storage credential exists in exactly one place**: `platform.Config` (typed,
  boot-validated, §1) — endpoint, region, bucket, access key/secret (or ambient IAM where the
  deployment provides one), optional `PublicEndpoint`, path-style toggle for MinIO-style backends.
  It never appears in `SESSION_CONFIG`, sandbox env, any prompt, or any wire shape — the
  sandbox-side env-hygiene concern §19.8 tracks has nothing to exclude here because nothing is ever
  injected.
- **What a client holds is never that credential** — only a presigned URL: one object, one method,
  minutes-lived (`UploadPresignPutTTL`/`UploadPresignGetTTL`, resident in `platform/timeouts.go`
  like every other interval, §5.4). This is the git-credential-helper pattern (§5.2: never
  long-lived in sandbox, short expiry, tightly scoped) with the mint moved server-side and the
  scope narrowed from host to single object.
- **Presigning binds the host**: URLs are signed against `PublicEndpoint` when set — a signature
  minted against an internal hostname breaks the moment a browser or sandbox resolves the public
  one (the classic S3-behind-a-proxy failure, named here so the deployment docs name the config).
- **Dev/CI story**: `docker-compose.dev.yml` gains a MinIO service for `make dev`; the adapter's
  integration tests run against a MinIO testcontainer (the `postgres:17-alpine` testcontainers
  precedent), asserting presign/PUT/`Stat`/`Delete` round-trips and the not-found/oversize
  classifications — §9.2's real-backend contract-test discipline, applied to storage.
- **Feature-flagged by configuration**: with no object-storage config present, the mint endpoints
  return a structured "uploads not configured" error and nothing else degrades — the standard
  incomplete-path flag posture (§10/CLAUDE.md), so Step 58 can land ahead of any deployment
  actually provisioning a bucket.

### 28.8 Phasing

Step 58, Phase 5, ∥ — independent of every other Phase 5 Step: nothing else consumes uploads, and
it consumes nothing beyond Step 4's `artifacts` table, Step 21's sandbox-bearer endpoint pattern,
and Step 47's prompt-placeholder mechanism. One PR: the `BlobStore` port + `objstore` adapter (+
MinIO integration tests); the artifacts migration; the mint/confirm/content endpoints in both auth
variants; `attachmentIds` + the rendered attachment block + placeholder substitution; the additive
`artifact`-event/DTO fields; the abandonment sweep; the `blob_delete` outbox kind; the config +
timeouts entries. Exit criteria: contract round-trips green including the additive fields; one
end-to-end integration test per direction (browser-shaped mint→PUT→confirm→download;
sandbox-shaped mint→PUT→confirm→`artifact` event observed); a failed-verification case proving
`failed(reason)` + the outboxed delete + the rail-visible event; and the oversized-mint refusal.
UI consumption is Phase 7 (Step 83's artifacts rail — status chip + reason on upload rows; no new
view).

## 29. Codex via ChatGPT-account OAuth + reasoning-effort overrides (Step 59's remaining scope)

Amends §8.8/Step 59's two previously-unelaborated clauses — "OpenAI/Codex (ChatGPT OAuth plugin)"
and "reasoning-effort plumbing (per-session and per-message overrides)" — exactly as §25.2 already
amended the same Step's Gemini clause. Every OpenCode-side claim below was verified 2026-08-06
directly against the pinned OpenCode 1.17.15 binary (same live-verification discipline as §25.2,
same rtk-bypass caution for large payloads); every OpenAI-side claim is sourced to OpenAI's own
published Codex authentication docs or to the pinned binary's own embedded implementation of the
flow, never to memory. This is a per-USER, subscription-tied credential — a materially different
thing from Step 53's org-level static API keys — and the design's one-sentence verdict up front:
**small scenario, but not env-var-shaped**: no new `AgentRuntime` adapter, no port change, no
OpenCode plugin — but the credential travels through OpenCode's own auth-store API
(`PUT /auth/{providerID}`), not `spawn.go`'s `cmd.Env`, and the control plane gains a small
link-and-refresh responsibility no static key needed.

### 29.1 Verified: ChatGPT OAuth is native in the pinned binary — §8.8's "plugin" phrasing is stale

`GET /provider/auth` on the pinned 1.17.15 binary — re-verified against a **clean-config**
instance (empty `XDG_CONFIG_HOME`/`XDG_DATA_HOME`, so no locally-installed plugin could be the
source), and independently confirmed by reading the flow implementation embedded in the compiled
binary itself — lists three auth methods for provider `openai`:
`oauth "ChatGPT Pro/Plus (browser)"`, `oauth "ChatGPT Pro/Plus (headless)"`, and
`api "Manually enter API Key"`. No plugin exists or is needed; §8.8's original "ChatGPT OAuth
plugin" wording predates OpenCode absorbing this natively. Three further verified mechanics anchor
the whole design:

- **`PUT /auth/{providerID}`** (the server API's `auth.set`) accepts a discriminated `Auth` union:
  `{type: "oauth", access, refresh, expires (epoch ms), accountId?, enterpriseUrl?}` or
  `{type: "api", key}`. Verified live: returns `true`, persists to that instance's own auth store
  (`$XDG_DATA_HOME/opencode/auth.json`), and flips the provider into `GET /provider`'s `connected`
  list. An **empty-string `refresh` is accepted** (verified) — load-bearing for §29.5's
  single-refresher design. There is **no `GET /auth`** — the auth store is write-only via API.
- **The flow itself is drivable over the same server API**: `POST
  /provider/{providerID}/oauth/authorize {method: <index>}` returns `{url, instructions,
  method: "auto"|"code"}`, and `POST /provider/{providerID}/oauth/callback {method}` — for
  `"auto"` methods — **blocks as the wait-for-approval poll** (verified: the call held open past a
  12s client timeout with the device grant unapproved), then persists tokens on success.
- **What OAuth mode changes inside OpenCode**: with an oauth entry set, `/config/providers` shows
  `openai` running on a substituted dummy API key (`opencode-oauth-dummy-key`) with the real
  authentication attached by OpenCode's own oauth loader — i.e. OAuth **takes over the provider**;
  an `OPENAI_API_KEY` env var delivered alongside would be silently outranked. Narvi therefore
  delivers exactly ONE credential per provider per sandbox (§29.6), never both kinds, so precedence
  is decided by Narvi's own resolution order and stays observable — never left to OpenCode
  internals.

The `openai` catalog entry itself (same `GET /provider` §25.2 used) lists 19 models including the
Codex line (e.g. `gpt-5.3-codex-spark`) — model selection needs nothing new: the §25.1 generic
`provider/model` passthrough already covers `openai/...` strings end-to-end.

### 29.2 The real OAuth flow shapes — and why the §13 web-callback pattern is impossible here

Extracted from the pinned binary's own embedded implementation (its compiled-in `openai` auth
methods), cross-checked against OpenAI's published Codex auth docs:

- **Browser method**: authorization-code + PKCE (S256) against `https://auth.openai.com/oauth/
  authorize`, client id `app_EMoamEEZ73f0CkXaXp7hrann` (Codex CLI's own public client — OpenCode
  reuses it, `originator=opencode`), scopes `openid profile email offline_access`, redirect
  hard-pinned to `http://localhost:1455/auth/callback`, exchanged at `POST /oauth/token`.
- **Headless method**: a device-authorization flow: `POST https://auth.openai.com/api/accounts/
  deviceauth/usercode {client_id}` → `{device_auth_id, user_code, interval}`; the human opens
  `https://auth.openai.com/codex/device` on ANY device and enters the short code; the initiator
  polls `POST .../api/accounts/deviceauth/token {device_auth_id, user_code}` (403/404 = pending)
  until it yields `{authorization_code, code_verifier}`, then exchanges at `POST /oauth/token`
  (`grant_type=authorization_code`, `redirect_uri=https://auth.openai.com/deviceauth/callback`).
  Matches Codex CLI's own documented `codex login --device-auth` (which OpenAI labels **Beta**).
- **Token material**: `access_token` (a JWT — 10.0-day `iat→exp` lifetime, verified on a real
  token), rotating `refresh_token`, `expires_in`, and an `id_token` whose `chatgpt_account_id`
  claim becomes the `accountId` the Codex backend requires per request.
- **Refresh**: `POST /oauth/token` (`grant_type=refresh_token`, `refresh_token`, `client_id` —
  public client, no secret) returns a NEW access token AND a **new refresh token that replaces the
  old one** (rotation). OpenAI enforces reuse detection — a consumed refresh token replayed later
  fails (`refresh_token_reused`), and OpenAI's own CI/CD guidance is explicit: one credential file
  per "serialized workflow stream", never shared "across concurrent jobs or multiple machines",
  refreshed by a single holder. This constraint shapes §29.5 more than any Narvi-side preference.

**The negative finding, stated plainly**: Narvi cannot mirror §13.1's GitHub-OAuth web-callback
pattern for this. The authorization-code redirect is pinned to `localhost:1455` for a client id
Narvi does not own, and OpenAI offers no third-party app registration granting
ChatGPT-subscription API access — there is no legitimate way to register
`narvi.example/auth/openai/callback`. The device flow is therefore not merely the convenient
choice for a server-side product; it is the only viable one.

### 29.3 The link flow: per-user, run from Settings, UI-driven polling, no worker

A ChatGPT Plus/Pro subscription is an individual seat; its token attributes usage (and burns
quota) personally. So this is a per-user account link — the same *category* as §13.2's
cross-channel identity links (a person connecting an external account to their Narvi user, once) —
but deliberately NOT an `identities` row: `identities` is the sign-in/chat identity graph, with
auto-linking driven by ingress events and a provider enum (`github,slack,linear,google`) that
§13.2's algorithm actively matches on. A model-provider credential has none of those semantics; it
belongs with the other model-provider credentials (§29.4).

Flow (all CP-side; the CP implements the four device-flow HTTP calls of §29.2 directly in one
small outbound adapter — see §29.9 for why not brokered through an OpenCode process):

1. Settings → "Connect ChatGPT account": `POST /api/me/chatgpt-link` calls
   `deviceauth/usercode`, persists a pending-attempt row — `chatgpt_link_attempts(user_id,
   device_auth_id, user_code, last_polled_at, expires_at)`, the same short-lived-nonce shape as
   §13.2's `identity_link_prompts` — and returns `{verificationUrl, userCode, expiresAt}` for the
   UI to display.
2. The user opens `auth.openai.com/codex/device` on any device and enters the code. The Settings
   page polls `GET /api/me/chatgpt-link` while open; each poll performs **at most one** upstream
   `deviceauth/token` attempt, throttled by the server-provided `interval` via `last_polled_at` —
   the human sitting on the page IS the polling loop, so there is no background goroutine, no
   timer, and nothing to leak when the page is abandoned (the attempt row simply expires;
   multi-pod safe because the state is a row, not memory).
3. On grant: exchange the code, parse `chatgpt_account_id` from the `id_token`, encrypt and store
   (§29.4), delete the attempt row, and audit-log the link (§13.3's audit table, same-transaction
   discipline).

Relink replaces the row (same upsert); unlink deletes it. RBAC in §29.9.

### 29.4 Storage: extend `provider_credentials` with a `user` scope and an `oauth` kind

Decision: **reuse Step 53's `provider_credentials` mechanism, extended — not
`identities.access_token_encrypted`, and not a new table.** Both existing mechanisms share the
same crypto (`platform.EncryptToken`, AES-256-GCM — 000056's own comment already cites 000017 as
its precedent), so the crypto is identical either way; what decides it is everything around the
row: `provider_credentials` already has the resolution model (`providercredential.Resolve`,
most-specific-wins), the sandbox-facing delivery endpoint with the full stale-sandbox/gen-fencing
security posture (`providercredentialsdelivery.go`), and the "write-only from the management API"
discipline — exactly the machinery this credential needs and `identities` has none of. Schema
changes (one migration):

- `provider_credential_scope` gains `'user'`; `scope_target_id` = `users.id` stringified (the
  same stringified-UUID convention the `environment` scope already uses). The existing partial
  unique index `(scope, scope_target_id, provider)` already covers it: one ChatGPT link per user.
- New `kind provider_credential_kind ENUM('api_key','oauth') NOT NULL DEFAULT 'api_key'`. For
  `oauth` rows, `value_encrypted` holds `EncryptToken` ciphertext of a JSON document
  `{access, refresh, expires_ms, account_id}` — one blob, rewritten atomically on every refresh,
  never four separately-encrypted columns.
- New `oauth_expires_at TIMESTAMPTZ` — a **plaintext mirror** of the blob's `expires_ms`, so the
  refresh pump (§29.5) can index-scan for expiring rows without decrypting anything (an expiry
  timestamp is not a secret — `user_sessions.expires_at`/`identity_link_prompts.expires_at` are
  already plaintext); and `oauth_needs_relink BOOLEAN NOT NULL DEFAULT false` (§29.5's terminal
  refresh-failure marker, surfaced in Settings). A CHECK constraint keeps both NULL/false for
  `api_key` rows.
- `providercredential.Scope` gains `ScopeUser` at the head of `scopePriority` (a personally-linked
  account is more specific than any environment/repo/global org key), `AllScopes`/`IsValidScope`/
  exhaustive tests updated — a pure domain change. Composes cleanly with §27.1's own `automation`
  addition to the same map: `user` rows exist only in `provider_credentials` and `automation` rows
  only in `sandbox_secrets`, so no single candidate set ever contains both and their relative
  priority is unobservable by construction.

Resolution keys on **`sessions.created_by`**: the delivery query adds user-scope candidates for
the session creator; `created_by IS NULL` (bot/automation sessions, 000004's own comment) simply
contributes no user candidate, falling through to the static-key scopes exactly as today. The
known consequence — in a multiplayer session, every participant's prompts run on the creator's
seat — is named in §29.10, not silently accepted. v1 creates `user`-scope rows ONLY via the link
flow (`kind='oauth'`, provider `openai`): a personal static API key at user scope is structurally
representable but deliberately has no creating endpoint — one less path to reason about.

### 29.5 Refresh: the control plane is the single refresher, pump-only

OpenAI's rotation + reuse-detection (§29.2) plus its own single-holder guidance dictate the shape:
exactly one logical holder may ever consume a refresh token. In Narvi that holder is the control
plane — never a sandbox (N concurrent sandboxes for one user would be exactly the "concurrent
jobs sharing one credential" case OpenAI's docs prohibit, racing each other into
`refresh_token_reused` lockouts).

- **A background refresh pump** — a small polling worker mirroring `outboxworker`'s own
  poll-loop shape — periodically claims oauth rows with `oauth_expires_at < now() + margin` via
  `SELECT ... FOR UPDATE SKIP LOCKED` (the same multi-pod claim-before-act discipline §21.3's
  digest already uses), refreshes each against `POST /oauth/token`, and atomically rewrites
  `value_encrypted` + `oauth_expires_at` with the rotated pair. Two new `platform/timeouts.go`
  entries (§5.4 — no literals elsewhere): `ChatGPTOAuthRefreshMargin` (propose 72h) and
  `ChatGPTOAuthRefreshPumpInterval` (propose 6h) — both invented generously per `HookTimeout`'s
  own "chosen generously when the concrete cost is unknown" convention, against the verified
  10-day access lifetime and OpenAI's own ~8-day staleness refresh in Codex CLI.
- **Delivery never refreshes.** The delivery endpoint serves what is stored, read-only — a
  deliberate rejection of lazy refresh-on-use: it would put a third-party auth server on the
  sandbox-boot critical path and reintroduce the concurrent-refresh race (two sandboxes booting →
  two refreshes) that the pump's SKIP LOCKED claim exists to make impossible. With the proposed
  values, a served access token always has ≥ ~66h of validity — beyond any non-pathological
  sandbox lifetime under §5.4's inactivity bounds; a sandbox that outlives it degrades per §29.6.
- **Failure taxonomy mirrors §13.2's own rule** ("a provider email-API failure is a retryable
  error, not an empty identity"): transient refresh failures keep the last stored pair and retry
  next pump cycle; a terminal `invalid_grant`/`refresh_token_reused` sets
  `oauth_needs_relink = true` — surfaced on the Settings card as "reconnect your ChatGPT account"
  — and the row stops being served. No decision-inbox integration (§16 is not extended — the same
  restraint §25.13 already applies).

### 29.6 Sandbox injection: one `auth.set` call beside Step 53's env append

The delivery response (`providercredentialsdelivery.go`'s invented, documented shape) evolves from
`provider → plaintext string` to `provider → Auth-union value` mirroring OpenCode's own two shapes
verbatim: `{"type":"api","key":...}` for static rows (today's behavior, re-labeled) and
`{"type":"oauth","access":...,"expires":...,"accountId":...}` for a resolved user-scope row — with
**no refresh token, ever**: sandbox-agent injects `refresh: ""` (verified accepted, §29.1), so a
sandbox is structurally incapable of consuming the rotating token family. CP and sandbox-agent
client types reconcile by hand exactly as `scmcredentials.go`'s own doc comment already requires
for that sibling endpoint. sandbox-agent then splits by kind:

- `api` → `providercredential.EnvVarNames` → `opencodeproc.Spawn`'s `providerCredentialEnv`,
  byte-for-byte Step 53's existing path.
- `oauth` → after `Spawn` reports healthy and BEFORE the sandbox reports ready for its first turn:
  one `PUT /auth/openai` against `Result.BaseURL` carrying `{type:"oauth", access, refresh: "",
  expires, accountId}` — sequenced inside the spawn/readiness path so a turn can never race an
  unauthenticated provider. Failure to set it is logged and emitted as a wire `Warning` (the same
  non-fatal surfacing §7.2 uses for compaction) — never a boot failure, because the credential is
  delivered independently of whether this session's turns will ever name an `openai/...` model; a
  turn that does need it then fails typed (`ProviderAuthError` is already a schema-derived member
  of the adapter's error union, §7.2) into the ordinary §8.7 recovery UX, including the
  mid-session-expiry edge case above.

No SESSION_CONFIG change, no `AgentRuntime`/port change, no new wire command — the whole
sandbox-side delta is inside sandbox-agent's existing credential-fetch + spawn path, mirroring
§25.3's own "entirely inside sandbox-agent" scoping. Two boundaries stated to prevent drift: the
`ports.LLM` path (classifier/single-completion calls — §25.7 already marks it unrelated) keeps
using CP-side static config and MUST NOT be pointed at a ChatGPT-subscription token (that token
authenticates the Codex backend, not the general OpenAI API, and per-user seats are the wrong
identity for platform-internal classification calls); and §25.2's Gemini design is untouched.

### 29.7 Contract tests and the honest verification gap

Mirroring §25.2/§7.2's pinned-binary discipline, Step 59 adds CI contract cases against the real
binary, all localhost-only: (1) `GET /provider/auth` still lists an `oauth` method for `openai`
(label prefixed "ChatGPT"); (2) `PUT /auth/openai` with the oauth shape (including `refresh: ""`)
returns `true` and flips `openai` into `GET /provider`'s `connected` list; (3) the `/doc` OpenAPI
still carries `POST /provider/{providerID}/oauth/authorize`/`callback` and the `Auth` union — an
OpenCode bump that drops or reshapes any of these must fail CI, not production. What CI
deliberately does NOT do: call `auth.openai.com` (a third-party production service has no place on
the per-PR path — the unauthenticated `deviceauth/usercode` start call is a good *scheduled*
canary for §29.2-shape drift, never a PR gate), and no end-to-end ChatGPT-account turn (it
requires a paid seat plus a human device approval — genuinely un-CI-able). The first real
verification that a Codex model answers through an OAuth-linked account is therefore a **manual
milestone inside Step 59, not a CI artifact** — the same honesty §25.13 applies to Gemini's
untested streaming parity.

### 29.8 Reasoning-effort overrides: the plumbing already half-exists, verified end-to-end

The second unelaborated §8.8 clause. Verified current state: the wire contract already carries the
field end-to-end — `sandboxws.Prompt.effort` is a required-nullable member of
`contracts/sandbox-ws/v1/commands.schema.json` ("Reasoning-effort override for this turn; null
means use the default") — but `BuildPromptPayload` hardcodes `Effort: nil` (`sessionactor/
dispatch.go`) because no column feeds it, and the OpenCode adapter drops `cmd.Effort` on the
floor. On the engine side, verified live against the pinned binary: `prompt_async` accepts an
optional top-level **`variant`** string, and the `GET /provider` catalog declares per-model
`variants` maps naming each valid value and its provider-native meaning — `openai` models:
`none/low/medium/high/xhigh` → `reasoningEffort`; `anthropic` models: `low/medium/high/(xhigh/)
max` → adaptive-`thinking` effort. So effort is the same class of generic passthrough §25.1 proved
for model, with the identical no-Narvi-side-allowlist discipline: valid values are per-model facts
owned by OpenCode's catalog (which the composer's already-mocked model/effort selector, §12.2 item
1, reads); Narvi validates nothing but nullability, and inventing a Narvi enum would drift exactly
the way a model allowlist would.

Data-model threading, mirroring `model_id`'s own established pair (the same precedent style Step
61 cites for `plan_mode`):

- `turns.effort TEXT NULL` — per-message: dispatch-time input beside `turns.model_id`
  (000018's own nullable "null = default" convention, verbatim).
- `sessions.build_effort TEXT NULL` — per-session: beside `sessions.build_model_id` (000034),
  copied onto the approval-dispatched build turn exactly as `build_model_id` → `model_id` is
  today. "Per-session/per-message" thus maps exactly as model already does: session-creation sets
  the first turn's (and, under plan mode, the build turn's) value; each subsequent message may
  override per turn. A sticky session-level default column consulted when a turn's value is null
  is deliberately NOT added — model itself has no such mechanism, and §11's "keep the domain paths
  single" forbids giving effort a second resolution ladder model doesn't have.
- Contracts: `CreateSessionRequest`/`CreateTurnRequest` gain required-nullable `effort` (mirroring
  `modelId`'s convention in both), `CreateSessionRequest` gains optional `buildEffort` (mirroring
  `buildModelId`'s optional-key convention); `sandbox-ws` needs NO change.
- Dispatch: `BuildPromptPayload` threads `target.Effort`; the adapter maps a non-nil `cmd.Effort`
  onto a new `promptAsyncRequest.Variant *string` (`variant,omitempty`) — one field, one mapping
  line; the §29.7 contract tests additionally assert `/doc` still lists `variant` on
  `prompt_async` (the same endpoint-existence discipline §7.2 applies to `/summarize`).
- Workflow engine echo: `workflow_step_definitions` gains a nullable `effort` column with the
  identical inherit-when-null semantics its `model_id` already has (§25.7/§25.8's zero-config
  proof extends unchanged) — threaded through the same engine → turn → `BuildPromptPayload` path,
  no separate mechanism. `plans.plan_effort` provenance (mirroring `plan_model_id`) is consciously
  omitted — no consumer exists; add it when §21's analytics actually wants it.

### 29.9 RBAC, adapter placement, and the rejected broker alternative

- **Linking is self-service, own-user only**: one new action row, own-aware like
  `ActionApprovePlan`'s own row (member+; viewers are read-only per §13.3 and cannot link),
  gating `POST`/`DELETE /api/me/chatgpt-link`. Admin unlink-of-any-user mirrors §13.2's
  admin force-link precedent (admin row, audit-logged). Step 53's three `ActionManage*Secrets`
  rows are untouched — they gate org-level scopes, and a user-scope row is never managed through
  the org CRUD endpoints. Token values remain write-only from every management surface, exactly
  like Step 53's rows; the sandbox delivery endpoint remains the only reader, unchanged in its
  security posture.
- **The device-flow client is one small CP-side outbound adapter** (four HTTP calls: usercode,
  token-poll, code exchange, refresh — §29.2's shapes). The alternative — brokering the flow
  through an OpenCode process's own `/provider/openai/oauth/*` endpoints so Narvi never touches
  `auth.openai.com` directly — was considered and rejected for v1: the CP image does not ship the
  OpenCode binary (only sandbox images do), so brokering means spawning a sandbox per Settings
  click (§3.2's heaviest machinery on an interactive path), holding its blocking `callback` call
  open across the approval wait, and then harvesting tokens OpenCode's API deliberately does not
  expose for reading (no `GET /auth`, §29.1 — the harvest would be a file read behind the API's
  back). If §29.2's endpoints drift (§29.10 risk 1), this broker IS the named fallback — the
  mechanics above are its specification, moved inside a sandbox.
- Phasing: Step 59, Phase 5 (§10's Phase 5 line already names "model catalog + Codex OAuth");
  depends on Step 53 (shipped — the store, delivery endpoint, and spawn-env path it extends);
  independent of Steps 54-56. UI surfaces (Settings link card beside §13.4's linked-identity
  chips; composer effort selector) land with their existing Phase 7 view Steps, not here.

### 29.10 Risks and open questions

1. **The device-flow endpoints are not a documented stable API.** OpenAI documents the *feature*
   (`codex login --device-auth`, labeled Beta) but not the endpoints; the shapes come from two
   independent public implementations (Codex CLI's source; the pinned binary's embedded
   implementation, extracted and read for this design). They can drift or gain restrictions
   (client-id allowlisting per originator is conceivable). Contained: drift breaks the LINK flow,
   loudly, at Settings — never a mid-turn corruption — and the sandbox-brokered fallback is
   specified (§29.9). The scheduled `usercode` canary (§29.7) exists to catch it before users do.
2. **Refresh-token absolute lifetime is undocumented.** The pump keeps pairs warm indefinitely in
   principle; an OpenAI-side policy change (idle-family expiry, seat downgrade, revocation) forces
   re-links. `oauth_needs_relink` + the Settings surface make that a self-service recovery, not a
   support ticket.
3. **Terms-of-service exposure is the user's, structurally.** OpenAI's terms govern personal-seat
   usage; Narvi keeps the mapping honest — one linked account per user, used only for that user's
   own sessions' turns, never pooled, never org-shared — which is exactly the per-user scope
   design. An org that wants pooled OpenAI usage configures a static `OPENAI_API_KEY` at
   org scopes (Step 53, unchanged).
4. **Multiplayer quota attribution**: participant prompts in a creator's session consume the
   creator's seat (§29.4). Accepted v1; the designed-but-unbuilt extension is per-turn
   re-injection (`PUT /auth` is callable between turns) keyed on the prompting user, if telemetry
   shows it biting.
5. **Subscription turns are mis-costed, but not by the mechanism first assumed.** The original
   claim here — that the Codex models' catalog cost object is all zeros — was **disproved during
   Step 59** by querying `GET /provider` directly against the pinned OpenCode 1.17.15 binary: every
   Codex model carries real, non-zero catalog pricing (e.g. `gpt-5.3-codex-spark`: $1.75/M input,
   $14/M output). The debt is therefore the inverse of what was written: a ChatGPT-OAuth turn is
   billed against the user's **subscription seat**, not per-token, so §7.1's cost roll-up
   **over**counts it at catalog rates rather than reporting zero. Same lineage as the
   cost-attribution debt §25.13 tracks; a "subscription turn" marker in cost analytics — which must
   suppress the catalog-rate roll-up, not fill in a missing zero — is left to the Step that closes
   that debt. (Step 59's own model-catalog snapshot carried the same wrong zeros and was corrected
   against the pinned binary at the same time.)
6. **Unknown-`variant` behavior is unverified** (rejecting vs ignoring an invalid effort value
   would need a live credentialed call to observe) — the composer constrains values to the
   catalog's per-model `variants`, so only raw API callers can hit it; named, not designed around.
7. **`accountId` handling on refresh**: the pinned binary preserves stored `accountId` across
   refreshes rather than re-deriving it each time; the CP refresher does the same. A user
   switching ChatGPT workspaces re-links — no silent account migration.

## 30. Platform shadow mode: zero-trace evaluation on live customer repositories (new capability)

Problem this solves: the project's next gate is plugging Narvi into real customer repositories to
observe everything it does internally — while leaving **zero trace** in the customer's repos and
systems. Platform-wide, not just the review lane; a single leaked comment, label, branch, PR,
commit status, Slack message, or Linear activity is total failure of the capability. The plan
already treats shadow as a permanent capability at the *decision* level (§9.4/§18.5 — classifier
decisions logged, not acted on) and already stages automation behind observed calibration (§21.2 —
auto-merge armed only once contradiction data justifies it). This section is the same shape at
platform scale: every customer-visible egress surface suppressed and recorded, an operator view of
the record, and "Activate" as an informed graduation gesture. The bar is structural: guarantees a
future contributor cannot silently un-make — never per-call-site discipline, which is exactly what
an egress inventory of this codebase shows accreting (five mutating GitHub methods already live
outside the port; a private Slack client already lives inside an inbound adapter).

Provenance, because it shaped the design: this section synthesizes an exhaustive egress inventory
of the codebase plus two adversarial passes. The second pass attacked the sandbox side
specifically and **invalidated the first draft's sandbox story as written**; its corrections are
folded into §30.4-§30.5 as requirements, not appended as caveats. On the fork that pass surfaced —
a conditional v1 resting on a conjunction of careful mitigations, or structural isolation — the
decision taken is maximal: **both** GitHub App fine-grained read-only installation tokens (§30.4)
**and** OS-level process isolation between sandbox-agent and the agent runtime it supervises
(§30.5). The codebase already concedes, verbatim, that these are the two missing pieces —
`internal/adapters/inbound/httpapi/scmcredentials.go`'s own top doc comment, closing its
review-session credential fix: "This does not claim to make a bash-capable review agent
structurally incapable of ever calling GitHub's API directly with SOME credential (that would
require OS-level process isolation between sandbox-agent and the agent runtime it supervises, or
GitHub App fine-grained/read-only installation tokens — neither of which this codebase has
today)". That concession is the honest framing for the two Steps that build them: they are not
shadow-mode features, they are the codebase's own named debt, and shadow mode is the requirement
that finally makes them non-optional.

The unifying principle, stated once: **the possibility of writing is removed at the lowest
available level (HTTP transport, credential capability), and the typed recording of what *would*
have been written happens at the highest available level (port, notifier)**. Every layer below is
an instance of it.

### 30.1 The guarantee, its exact scope, and what it deliberately excludes

**Covered — suppressed and recorded**: every write into a customer-visible system reachable
through Narvi's own egress paths. Concretely: the 11 mutating GitHub operations (6 on
`ports.SourceControl` — `CreatePR`, `UpdateFileContent`, `UpdatePRBody`, `RegisterPRStack`,
`CreateBranch`, `MergePR` — plus 5 that exist only on the concrete `*githubapi.Adapter`, outside
the port: `CreateReview` (`verdictpost.go:51`), `AddLabels`, `RemoveLabel`, `PostIssueComment`
(`adapter.go:990`), `CreateCommitStatus` (`adapter.go:1032`)); the synchronous ingress-side
replies (GitHub sign-in/not-authorized/plan-awaiting comments, the Slack ack and interactive
responses, Linear's synchronous activities — §30.3); the customer-destined subset of the 19
`ports.NotificationKind` outbox kinds (§30.2 — including the one hybrid kind, `sentinel_auto_fix`,
whose internal work passes through while its external write is caught one layer down, at the
decorated port); the
sandbox's end-of-turn `git push` (§30.4); and
`rwx_preview_dispatch` — a public preview build executing customer code is a trace on the RWX
cloud and on the open internet, so it is suppressed structurally even though it is naturally
starved in shadow (the SHA it would build is never pushed); reliance on upstream starvation alone
is never accepted anywhere in this design.

**Explicitly excluded — reads.** Clones, API GETs, and bot-token reads appear in the customer's
own GitHub audit surface, and no design can hide them while genuinely observing a live repository:
observation *is* reading. The guarantee is therefore stated as **zero writes, not zero audit-log
entries** — excluded explicitly here rather than left ambiguous, so nobody discovers the gap in a
customer conversation. The same applies to GitHub webhook delivery logs on the customer side:
Narvi receiving a webhook is visible to whoever configured it.

**Excluded as a named v1 residual — arbitrary sandbox internet egress.** No sandbox network
policy exists in this codebase: `platform.Config`'s Modal egress proxy
(`NARVI_MODAL_EGRESS_PROXY_URL`, `internal/platform/config.go`) routes only the *control plane's*
HTTP client to Modal, and has nothing to do with the sandbox's own outbound traffic. An agent —
or a prompt injection steering it — can reach any third-party service the repo references. This
is precisely why the whole sandbox story rests on capability starvation (§30.4) and isolation
(§30.5): with those in place, an unrestricted `curl` from the sandbox holds nothing that writes
to the customer's SCM and cannot recover anything that does. The v1 guarantee is exactly: *no
trace via Narvi's own egress paths, no SCM-write capability inside the sandbox, and no credential
recoverable by the agent runtime*. Full network closure is provider-infrastructure work,
agent-degrading, and out of v1 scope — revisitable as a deferred decision (§30.9).

**Surfaced, not suppressed — customer LLM spend.** Provider credentials delivered to the sandbox
(`providercredentialsdelivery.go`) are customer-scoped (BYOK, ChatGPT OAuth): a shadow evaluation
burns real customer provider credit with zero output visible to them. This is not suppressed —
the agent actually running *is* the thing being evaluated, and cutting the models would empty
"observe everything" of meaning — but the cost is surfaced as a dedicated line in the shadow
operator view (§30.6), with a deferred decision (§30.9) on forcing an org-level evaluation key
for shadow runs. Unlike SCM credentials, provider credentials **must** keep being minted; the
capability-starvation lever applies to `scm-credentials` only.

### 30.2 Layered enforcement on the control plane's own egress

**Layer 0 — the transport gate (non-negotiable).** Two verified facts make this layer possible
with one seam: there is exactly **one** production construction site for the GitHub adapter
(`cmd/control-plane/main.go:198`, `githubapi.New(nil, githubAPIBaseURL)`), and the package
constructs no second HTTP client — every request rides the constructor-injected `a.httpClient`
(the `doGet`/`doPost`/`doPut`/`doPatch` helpers at `adapter.go:346/525/1305/1345`, plus four
inline requests: `CreatePR`'s POST (`adapter.go:149`), `RemoveLabel`'s DELETE
(`verdictpost.go:114`), `GetPullRequestDiff`'s GET (`adapter.go:860`), and `GetCompareDiff`'s GET
(`adapter.go:944`) — eight sites total, all riding `a.httpClient`). A shadow
`http.RoundTripper` installed *inside* the adapter therefore sees everything: in shadow, GET/HEAD
pass; **every mutating verb is intercepted by default** — deny-by-default, no host allowlist for
writes — recorded (method, path, decoded intention, payload) into the §30.6 ledger, and answered
with a synthesized success. Why this layer is structural where the port is not: it covers all 11
current mutating methods, it covers the synchronous comments the GitHub ingress posts through the
same adapter instance (`internal/adapters/inbound/github/actornotauthorizedreply.go`,
`planawaitingreply.go` — which bypass both the port and the outbox), and it covers **any future
mutating method added to the concrete type**. Five such methods already exist outside the port
today; a 12th compiles cleanly and is invisible to every typed layer — the transport is the only
place that class of leak is contained.

**Layer 1 — the port decorator (typed recording + compile-time tripwire).** An **explicit,
non-embedded** decorator of `ports.SourceControl` (`internal/app/ports/sourcecontrol.go`)
installed at the wiring site: each of the 6 writes becomes a typed "would-have-done" ledger entry.
Five of the six additionally return a coherent synthetic result (`CreatePR`'s `PRRef` — see §30.6
for why those results must be impossible to mistake for real ones); the sixth, `MergePR`, returns
no synthetic result at all — a fabricated merge success is a false-record generator, not a stand-in
— and instead returns the typed `ShadowSuppressed` sentinel (§30.7 is the sole authority for this
one). Because the decorator implements
the interface explicitly — never by embedding — **adding a method to the port breaks the build**
until the decorator handles it. The transport gate remains the net for everything the port never
sees. The two layers are deliberate redundancy in one direction only: the decorator records with
types and keeps internal state machines coherent; the transport guarantees nothing escapes even
when the decorator's coverage is stale.

**The constructor signatures change, because the current defaults are an attractive nuisance.**
All four outbound constructors document a `nil → http.DefaultClient` convention
(`githubapi/adapter.go:76`, `slackapi/client.go`, `linearapi/client.go`,
`rwx/dispatchclient.go`). A developer writing `githubapi.New(nil, baseURL)` in a new package gets
a working, gate-free instance invisible to every layer above. Those defaults are **removed**: the
transport/gate argument becomes mandatory and non-nil, and the *live* (pass-through) variant is
constructible only from the package that resolves the shadow flag (an unexported constructor — a
capability token for egress). A new construction site cannot compile without the gate in hand,
and the zero value fails closed.

**The outbox seam lives in the consumer's constructor, not at the wiring line.** All asynchronous
egress flows through one `map[ports.NotificationKind]ports.Notifier` consumed by
`internal/app/outboxworker` (`NewBuilder`, `builder.go:63`). The obvious wrap point — the map at
its `main.go` population site — is **wrong**, and the reason is a standing trap: the map is
mutated *after* wiring (`rwx_preview_dispatch`/`github_preview_link` inserted conditionally at
`main.go:1509-1510`, `blob_delete` at `:1523`). A wrap mid-wiring would exempt every later insert
— `github_preview_link` would post a real `narvi/preview` commit status onto customer commits in
shadow. Classification therefore happens **inside `NewBuilder`**, which receives the finished
map and **refuses to start** if any registered kind lacks an explicit External/Internal
classification. Insertion order in `main.go` stops mattering, and a 20th kind cannot ship
unclassified. Go cannot force registry exhaustiveness at compile time; failing closed at boot,
before any traffic, is the strongest available equivalent (the same reasoning §5.4 applies to the
timeout hierarchy's invariant test). Classification: SUPPRESS for every customer-destined kind;
**PASS-THROUGH is mandatory** for `blob_delete` (Narvi-internal storage hygiene — suppressing it
leaks orphaned blobs forever; the trap in any blanket suppress-everything reading), for
`sentinel_auto_fix` (the **one** hybrid kind: its `Deliver` performs internal work that must run —
the child-session spawn in `internal/app/outboxworker/sentinelautofix.go` — while its external
writes go through the decorated port), and for `linear_digest` (a deliberate dead-letter,
unchanged). `github_description_autofix` is **not** a hybrid and is **SUPPRESS**, not
PASS-THROUGH: `descriptionAutofixNotifier.Deliver`
(`internal/app/outboxworker/descriptionautofix.go`) performs zero internal state mutation of its
own — every precondition it checks (`DescriptionAdequacy`, the repo's autofix flag, PR
authorship, the PR's current body) is a read; its only effect is the external `UpdatePRBody` call,
which the §30.2 Layer 1 decorator already covers. Nothing is lost by suppressing it at the outbox
like every other customer-destined kind — its one write was never going to reach GitHub either
way — and doing so keeps the classification uniform instead of resting the codebase's only
customer-visible PR-description rewrite on the port decorator and transport gate alone.

### 30.3 The synchronous ingress writes: honestly non-structural, with three required compensating controls

Four families of synchronous writes live in webhook handlers, outside both the outbox and the
port — and they fire precisely in shadow's target scenario, webhooks from a live customer system:

1. **GitHub**: the sign-in / not-authorized / plan-awaiting replies
   (`internal/adapters/inbound/github/actornotauthorizedreply.go`, `planawaitingreply.go`).
   `CommentPoster` (`planawaitingreply.go:45`) is already a narrow interface — a recording
   implementation plugs in without refactoring — and the §30.2 transport gate covers these by
   construction (same adapter instance).
2. **Slack ack**: a private `chat.postMessage` client inside the inbound adapter
   (`internal/adapters/inbound/slack/ack.go`, `newAckClient` — with its own `nil →
   http.DefaultClient` default), whose doc comment says it is deliberately outside the Notifier
   abstraction. The single-instance property that holds for GitHub **does not hold for Slack**.
3. **Slack interactive**: direct `PostEphemeral`/`UpdateMessage`/`OpenView` calls on a concrete
   `*slackapi.Client` (`internal/adapters/inbound/slack/interactive.go`).
4. **Linear**: synchronous `CreateResponseActivity`/`CreateThoughtActivity` on a concrete
   `*linearapi.Client` (`internal/adapters/inbound/linear/webhook.go`, `identity.go`).

**This perimeter cannot be made 100% structural in Go, and the plan says so rather than
pretending.** A fifth future synchronous reply path — a new config field, a new inline client;
`ack.go` is the in-repo precedent — is caught by no type system. Three compensating controls,
all three required, none sufficient alone:

- **The same single-instance property GitHub has, imposed on Slack and Linear**: one client per
  provider, built via the mandatory-gate constructors (§30.2), mutation methods behind decorated
  interfaces, and ingress packages losing the ability to construct their own clients
  (`newAckClient` moves behind the injected seam). Linear caution: a verb-level transport guard
  does not work there — everything is `POST /graphql` — so suppression is at the **client-method
  level, never the transport**.
- **A CI arch-test** (depguard/forbidigo), scoped to what it can actually enforce. Banning the
  `net/http` **import** outside `internal/adapters/outbound` and the sandbox-agent trees fails the
  repo on day one: verified, 58 non-test files across 11 packages outside those trees import
  `net/http` today — every handler in `internal/adapters/inbound/httpapi` (34 files) plus
  `inbound/auth`, `inbound/automationwebhook`, `inbound/github`, `inbound/identitylink`,
  `inbound/linear`, `inbound/slack`, `inbound/wshub`, `internal/platform`, `cmd/control-plane`,
  and `cmd/sandbox-agent` — all for the **server** side (`http.ResponseWriter`, `*http.Request`,
  status constants, `http.HandlerFunc`), which an import-level ban cannot distinguish from the
  **client** side it exists to forbid. (`os/exec` outside those trees is genuinely clean — 0 files
  — so its import ban is unchanged.) The rule is therefore two different mechanisms: `os/exec`
  stays an **import ban**; `net/http` becomes a ban on constructing or invoking **client-side
  symbols** — `http.Client`, `http.DefaultClient`, `http.DefaultTransport`,
  `http.NewRequest`/`NewRequestWithContext`, `http.Get`/`Post`/`Head`, `http.Transport` — with
  server-side `net/http` explicitly permitted everywhere (receiving/answering a request is not an
  egress capability). Both mechanisms' allowed trees are `internal/adapters/outbound`,
  `internal/sandboxagent`, **and `cmd/sandbox-agent`** (verified NOT inside the
  `internal/sandboxagent` tree, but the same binary in every sense that matters here — its own
  `net/http` use today, `reviewcostbudgetserver.go`, is server-side); `cmd/control-plane` also
  carries the client-side exception, but narrowly — as the composition root, it is the one place a
  concrete `*http.Client` is legitimately constructed and wired into an outbound adapter's
  constructor (`chatgptoauth.New(http.DefaultClient, …)`, `main.go:386`; `githubapi.New`'s own
  wiring line, until Step 97 removes its `nil` default) — never used there to issue a request
  directly. **Ratcheted, not repo-wide, from the moment Step 102 lands**: two pre-existing
  client-side call sites outside the allowed trees are real and audited, not oversights, and are
  pinned into the arch-test's initial baseline rather than silently grandfathered — GitHub's own
  identity-read calls made during OAuth sign-in (`fetchGitHubUser`, `fetchVerifiedPrimaryEmail`,
  `checkOrgMembership`/`checkAnyOrgMembership`, all GETs, `internal/adapters/inbound/auth/callback.go`
  — reads, never a customer-repo write, and no different in kind from the API GETs §30.1 already
  excludes from the guarantee) and the pre-Step-96 `newAckClient`/`SlackHTTPClient` construction in
  `internal/adapters/inbound/slack/{ack.go,handler.go}` — the exact site this Step's first
  compensating control retires by moving it behind the injected seam, so it drops out of the
  baseline once this Step ships. Any client-side symbol anywhere else, including a 6th baseline
  entry, fails CI — mechanical enforcement at merge time, the closest thing Go offers to
  compile-time for this class.
- **Credential starvation in the evaluation deployment** (§30.4): if no write-capable secret
  exists in the process or the sandbox, an ungoverned future code path is harmless by
  construction. This is the only control that survives the 12th caller, the 6th notifier, and the
  developer who imports `net/http` anyway.

### 30.4 Sandbox capability: the read-only credential is the sole structural guard against sandbox SCM writes, and it becomes a GitHub App installation token

**The model is sound by construction; the lever did not exist until now.** The mint
`POST /sessions/{id}/scm-credentials` (`internal/adapters/inbound/httpapi/scmcredentials.go`) is
the sandbox's only source of SCM credentials, verified: `SESSION_CONFIG` carries none
(`contracts/gen/go/sessionconfig/sessionconfig.go` — `SandboxToken`, `SessionId`,
`ControlPlaneWsUrl`, `Gen`, `Repos`; no git credential), nothing persists to `.git/config` (every
git op passes the helper per-invocation via `-c credential.helper=…`,
`internal/sandboxagent/gitclone/clone.go`; `RunStore` is a deliberate no-op), and the provider
spec transports none. What the sandbox does not hold, it cannot use — robust even against a
prompt-injected agent driving the GitHub API directly. But until this chantier, "refuse to mint
write" and "refuse to mint at all" were the same thing: both mintable credentials are
write-capable (the creator's OAuth token, full `repo` scope — `scmcredentials.go:497` — and the
bot token that posts reviews), and the same mint serves the **clone**. Starving the mint breaks
"Narvi must actually run".

**The lever is substitution, and the substituted credential is a GitHub App fine-grained
read-only installation token** — the decision taken, over the smaller operator-provisioned
fine-grained-PAT stopgap the design also weighed (and over a CP-side git smart-HTTP proxy, the
fallback if a customer can only ever provide OAuth). App plumbing does not exist in
`internal/platform/config.go` today and is its own Step: App id + private key in config,
installation-token minting scoped `contents:read` (+ `metadata:read`), short-lived and
auto-refreshed. In shadow, the mint returns this token: the clone succeeds, and a push — from any
holder, through any path, including paths this codebase has never seen — is refused by GitHub
itself. Enforcement by capability closes at the root the entire residue class the adversarial
pass found: a token that cannot write is equally harmless in a process environment, a disk cache,
a baked image, or a restored snapshot.

**Four corrections from the adversarial pass, each a hard requirement of the Step, none a
recommendation:**

1. **The substitution is a single server-side interception at the top of `ScmCredentials`,
   covering both mint branches.** The handler returns write-capable credentials on two distinct
   paths — review sessions get the bot token (`scmcredentials.go:403`), everything else the
   creator's decrypted OAuth (`:497`) — and review sandboxes are exactly the common shadow
   workload; substituting only the obvious creator branch would hand every shadow review sandbox
   the fully write-capable bot token. And the interception must be **server-side only**: the
   same-UID agent (until §30.5 lands) can recover the sandbox bearer from `/proc` and POST the
   mint endpoint directly, so any client-side substitution is decorative. A dedicated test
   asserts a review session in shadow receives a read-only credential.
2. **The image-build path must never hold a write token — this is an in-repo bug, not an
   external-service caveat.** `gitclone.CleanForImageBuild`
   (`internal/sandboxagent/gitclone/sync.go:941`), the pre-snapshot cleanup for `BootModeBuild`,
   cleans the repo trees under `/workspace` only — it never touches the credential cache, and by
   the time it runs the clone has already written the minted token to disk
   (`internal/sandboxagent/credentials/get.go`, `cache.go`). If the provider image captures the
   filesystem, the token is baked into the shared image every future session reuses — shadow ones
   included; a live-mode cross-session hygiene bug independent of shadow. Fix: force the
   read-only mint for `BootModeBuild` (a build only needs read), **and** purge
   `boot.Config.CredentialCacheDir` inside `CleanForImageBuild` before the snapshot — the
   read-only mint is the primary fix, the purge defense-in-depth.
3. **Snapshots must never contain a token, enforced at snapshot mint time.** The restore path
   (`internal/app/sessionactor/dispatch.go`, `RestoreFromSnapshot`) restores the full filesystem,
   and `freshCacheHit` (`credentials/get.go:47`) then serves a cached credential **without ever
   calling the mint** — a write token cached during a live session and captured in a snapshot
   arrives in a shadow session with no control-plane cooperation at all (gen/token-hash rotation
   fences the CP bearer, not the GitHub token). The primary fix is purging the credential cache
   at snapshot **mint** time (the sandbox side of the snapshot flow, `HandleSnapshot` /
   `httpapi/snapshotmint.go`'s counterpart), so no snapshot ever contains a token regardless of
   how it is later restored. A shadow bit recorded on each snapshot row, with restore refused on
   mismatch at the store/dispatch level, is kept only as fail-closed defense-in-depth — and its
   polarity is explicit: **an absent bit (every pre-existing snapshot) is treated as live and
   restore into a shadow session is refused** — the same fail-toward-suppression polarity §30.8
   imposes on the flag itself. A boot-time cache purge in all modes is also required, but it is
   *not* load-bearing on its own: whether the provider's restore re-runs the boot sequence at all
   is the open Modal warm-resume question (§30.9), and nothing here may rest on the unverified
   answer.
4. **Scope introspection, fail-closed, at boot and at mint.** Nothing in
   `internal/platform/config.go` validates any token's scope today. An operator pasting a classic
   `repo`-scoped credential into the shadow slot would silently re-arm every sandbox. The
   evaluation deployment refuses to boot, and the mint refuses to serve, unless the credential's
   introspected scope (`X-OAuth-Scopes` / the App installation's permissions) is read-only. An
   ops mistake becomes a loud fail-closed, never a quiet regression.

**The WS-push gate is consistency, not security — stated plainly because the first draft got it
wrong.** Gating `sendPushBestEffort` (`internal/app/sessionactor/pushpr.go`) only stops the
control plane from *asking* the sandbox to push. `HandlePush` in `cmd/sandbox-agent/main.go` runs
`git push` with whatever the helper mints, and a same-UID agent needs no invitation: it can run
the credential-helper subcommand itself, POST the mint directly with a `/proc`-recovered bearer,
or read the cache file. The per-turn push decision (§30.8) exists so the turn's push/PR/preview
trio resolves one mode atomically and the ledger gets its entry — UX and state coherence. The
security is the read-only token (this section) and the UID boundary (§30.5), nothing else.

**The invariant over the sandbox-facing API, stated because the adversarial pass verified it
holds today and nothing keeps it true tomorrow**: *no sandbox-bearer endpoint may perform or
enqueue an external SCM write except through the suppressed path.* The nine bearer endpoints
(`cmd/control-plane/main.go:619-690`) were each traced: `scm-credentials` is the lever above;
`review/verdict` performs **no synchronous external write** — the bot token in its request path
serves only a diff *read*, while the actual `CreateReview` + label sync happen at outbox delivery
(`githubapi.NewVerdictNotifier`, wired at `main.go:1422`) — so suppressing the `github_verdict`
kind fully closes it, provided §30.2's fail-closed-at-boot classification stands; snapshot,
provider-credentials, uploads (mint/complete/content), workflow/step-outcome and
turn/epistemic-outcome touch internal state and Narvi-owned blob storage only.
`provider-credentials` returns customer LLM credentials — an exfiltration concern, closed by
§30.5's bearer starvation, not an SCM write. New bearer endpoints inherit the invariant as a
review obligation plus the outbox classification's boot-time check for anything they enqueue.

**The demotion TTL window, documented rather than hidden**: a write credential minted just before
a live→shadow flip stays served until `ScmCredentialTTL` (15 min, `platform/timeouts.go`) plus
the helper's cache buffer elapse, and the underlying OAuth token itself never expires on that
clock. Demotion therefore **must terminate (or respawn) every sandbox of the repo** and cancel
in-flight push signals. Under shadow-by-default-at-onboarding (§30.8), this window exists only
for demotion of a formerly-live repo — a fresh evaluation's sandboxes have never held write.

### 30.5 OS-level isolation between sandbox-agent and the agent runtime

**The fact that reframes the whole sandbox analysis: there is no privilege boundary today.**
`supervisor.Spawn` sets `SysProcAttr{Setpgid: true}` and nothing else
(`internal/sandboxagent/supervisor/supervisor.go`) — no `Credential`, no UID change, no user
namespace. The agent runtime, and any shell it spawns, runs as the **same UID** as sandbox-agent.
Consequences, each verified: the env-strip in `opencodeproc/spawn.go:72`
(`EnvWithout(SessionConfigEnvVar)`) is cosmetic — the value still sits in sandbox-agent's own
`/proc/<pid>/environ`, one read away, along with the sandbox bearer; and the credential cache
(`/tmp/narvi-credentials`, mode 0600, placed "outside `/workspace`" —
`internal/sandboxagent/boot/config.go`) is a fictional boundary against a same-UID shell.

**The decision: the agent runtime runs under a different UID (and, where the substrate allows, a
user namespace) from sandbox-agent.** `supervisor.Spawn` grows a `Credential` in its
`SysProcAttr`; the credential cache and sandbox-agent's own environment become unreadable to the
runtime by kernel enforcement, not by placement convention. What this buys that the read-only
token does **not**: the token closes SCM writes, but an injected agent could still exfiltrate the
sandbox *bearer* — and with it drive every sandbox-bearer endpoint directly (§30.4's nine,
`provider-credentials` — the customer's LLM credentials — included) — and read whatever the SCM
cache holds. UID isolation
starves the runtime of the bearer and the cache in every mode, live included; it is a
platform-wide hardening that shadow merely makes non-optional, exactly as the
`scmcredentials.go` concession frames it.

Scoping discipline for the Step that builds it: inventory what the runtime *legitimately* uses
and preserve exactly that — the workspace tree, its own injected per-turn provider credential
(inherent: the runtime must call the LLM; isolation cannot and does not protect what the runtime
is handed by design, which stays governed by §30.1's surfaced-spend posture), and the
deliberately agent-facing loopback endpoints (Step 70's `127.0.0.1` review-cost-budget server —
loopback stays shared; the network namespace is not the boundary being drawn). Anything that
works today only by same-UID accident — reading the cache, reading `/proc/<sandbox-agent>`,
invoking the credential-helper subcommand against the cache — is the attack surface being
closed; if any of it turns out to be a real dependency of a legitimate agent behavior, that is a
design smell to fix at the source, never a reason to widen the boundary back. Residuals stated:
the runtime still shares the network namespace (arbitrary egress remains §30.1's residual, now
empty-handed), and kernel-level escalation is the substrate's concern (the provider's isolation
layer), not this Step's.

### 30.6 The recording model: what a suppressed effect becomes

Recording is the *product* of shadow mode — it is what the operator evaluates. The load-bearing
observation: most of "observe everything" already exists and keeps working untouched.
`review_verdicts`, `review_findings`, `events`, `artifacts`, `auto_approval_outcomes`
(migrations 000067, 000046, 000008, 000012, 000070) are written in shadow exactly as live and
render in Narvi's own UI with zero new work. The ledger covers only the **suppressed delta**.
Three writes, one read:

1. **Outbox rows**: the row already carries the full payload (migration 000010; never purged) —
   it *is* the record. Shadow adds a `suppressed_in_shadow` mark **stamped at enqueue, in the
   enqueuing transaction** (one column on `OutboxStore.Create` — a single choke point for every
   enqueue site), plus the terminal mark when the worker "delivers" it into the ledger instead of
   the world. The enqueue-time stamp is a §30.8 epoch requirement, not an implementation
   convenience. A flag column, deliberately not a new status enum — the worker's claim/backoff
   state machine stays untouched.
2. **Direct SCM writes**: a new append-only `shadow_scm_writes` table (operation,
   owner/repo/target, redacted spec JSONB, synthetic result JSONB, `session_id` `ON DELETE SET
   NULL` — the `review_verdicts` precedent: history outlives the session — `correlation_id`,
   `created_at`), written by the port decorator with **record-or-fail** semantics: suppression
   returns success only if the insert commits. A suppressed-but-unrecorded effect is a contract
   violation, and failing loudly is safe — nothing external happened. Specs enter through
   **record types that carry no `Token` field**: today every write spec carries a plaintext
   token (`ports/sourcecontrol.go`'s spec types; `decryptCreatorGitHubToken`'s documented
   call-site sprawl, `sessionactor/githubtoken.go`), and excluding the credential from the ledger
   is a compile error, not a redaction pass. The transport gate (§30.2 layer 0) records what it
   intercepts into this same table; the shadow mint records its substitution the same way, and a
   mint refused by the §30.4(4) scope check records the refusal. Medium-term, an opaque secret
   type serializable only by the gated transport shrinks the plaintext surface further — noted,
   not scoped here.
3. **Timeline**: each ledger insert appends an `events` row (`shadow_egress_suppressed`, payload
   = ledger id + summary) so suppressions appear inline in the session workspace the operator
   already watches. `events` is surface, never durable truth (it cascades with the session);
   the ledger rows are the record.

**Synthetic results are a hard requirement with one open question.** A suppressed `CreatePR` must
return a `PRRef` so internal state advances (the PR artifact record, `github_pr_sessions`). The
adversarial finding: realistic-looking synthetic refs are **durable poison** —
`isPlatformAuthored` is a pure string match over artifact URLs
(`internal/app/decisioninbox/aggregate.go`; same pattern in the description-autofix path), and
GitHub's PR counter is monotonic, so collision between a synthetic PR number and a real future
customer PR #N is a guaranteed-eventual event — after which live auto-merge could act on a
customer PR Narvi never authored. Therefore: a **dedicated synthetic-ref constructor — unexported,
negative numbers and the `shadow://…` scheme baked in — the only way any code in the codebase can
produce a value claiming to be synthetic**, pinned by a test that it is the sole production call
site, so no synthetic ref can ever match a real GitHub URL and any accidental use against the real
API fails loudly. (An ordinary integer/string field carries no such guarantee on its own — the
constructor's exclusivity is what does the work, the same capability-token idiom §30.2's live/shadow
transport constructors and §30.8's resolver package already use.) (The remaining question — how far the
synthetic scheme propagates into downstream lanes — is Step 103's chain-synthesis decision, §30.9.)

**The operator view is a read model, not new state** (§16.2's own rule). The in-plan precedents
are exact: §18.5's divergence metric + "Activate" for the classifier, §21.2's calibration stats
displayed beside the auto-merge toggle so arming is "an informed decision, not a leap of faith".
Platform shadow is the same form at scale: a repo card in Settings → Environments (where the
sentinel-autofix and auto-merge toggles already live), a ledger summary (N comments suppressed,
M PRs, K pushes refused, LLM spend, links into sessions), and **Activate as the graduation
gesture** — which applies §30.8's promotion fence. The read is a UNION over marked outbox rows +
`shadow_scm_writes`. Access is **admin-only, pending the §30.9 visibility decision** (admin-only
vs maintainer+) — not settled here: §13.3's permission matrix has no audit-view row (the closest
existing thing, Settings → Members' audit log, sits on that matrix's admin-only row), and this
ledger exposes strictly more — full suppressed-write payloads plus customer source, beyond what
even that row covers. Admin-only is the safe default until §30.9 resolves, matching this plan's
own precedent for comparable exposure (§16.1's dead-lettered-outbox-deliveries row is admin-only
for the same reason). A flag flip is an `audit_log` entry; individual suppressions are
deliberately not (no human actor; they would drown the view). Retention/PII policy for ledger
content is a deferred decision (§30.9); the cheap enabling move — a separate heavy-content column
so a later null-out doesn't rewrite the table — is taken at schema time.

### 30.7 State coherence: where "return synthesized success" would lie

Suppression must not wedge or corrupt the state machines that consume write results. Each case
below replaces a naive success with something honest:

**Auto-merge.** A suppressed `MergePR` returning fabricated success is a false-record generator:
the worker would log "merged", write an `audit_log` row with an invented SHA, and
`RecordConfirmed` would feed fake confirmations into the §21.2 contradiction-rate metric — the
very instrument that justifies arming auto-merge for real. Meanwhile the PR (in a demotion
scenario, a real one) stays open and re-candidates every tick. Instead the decorator returns a
**typed `ShadowSuppressed` sentinel** — not an error, not success — which the auto-merge worker
and the human merge handler both map to "recorded, not merged": a distinct audit action
(`shadow.would_have_merged`), no `RecordConfirmed`, no `Merged: true` to the UI, and shadow
outcomes excluded from the calibration read model by the same query-level stamp §30.8 requires
of verdicts.

**Sentinel auto-fix.** Per-write suppression composed naively is incoherent: `CreateBranch`
suppressed → the fix branch never exists on the remote → the child session, pinned to that
branch, fails its `git clone --branch` (`gitclone/clone.go`) → the one-shot claim
(`FixChildSessionID`) wedges the lane permanently, and `markFindingsFixPending` has already
flipped the findings to `fix_pending`, which also kills the *manual* apply-suggestion action
(§17.3). The evaluator would conclude the sentinel lane is broken — a falsified evaluation. Two
coherent resolutions exist and the choice is the §30.9 mirror decision: with a Narvi-owned git
mirror (a bare repo per session), the branch is created on the mirror and the child session
clones and pushes against it — the lane actually runs, end-to-end observable; without it, the
lane short-circuits **before** the claim (one ledger entry: "would have created the branch and
spawned the fix session"; findings stay `open`).

**Apply-suggestion** (`httpapi/reviewfindings.go`). Naive suppression marks the finding
`fix_applied` with a SHA that exists nowhere, and §24's re-review then re-detects the same
defect on the unchanged real head — the system contradicts itself and duplicates findings.
Instead: an explicit "recorded, not committed" response (SHA in the shadow scheme) and a
dedicated `fix_recorded` status that re-review ingestion treats as still-open (an update, never
a contradiction).

**Chains closed by GitHub's echo.** With no real PR, GitHub never sends `pull_request` webhooks
for Narvi's own PRs → no `github_pr_sessions` row → review-of-own-work, auto-approval,
auto-merge, description-autofix, and handoff are **structurally unobservable** in shadow. Either
the internal trigger is synthesized from the suppressed `CreatePR` record (create the
`github_pr_session` on the shadow ref, so downstream lanes exercise internally — Step 103), or
the plan documents that shadow validates single hops only. That choice is deferred (§30.9), but
the default posture while it is unresolved is the honest one: no downstream-lane claims in the
operator view beyond what actually ran.

**Plan approval** is coherent by construction: the suppressed Slack message means
`SetSlackMessageRef` (`internal/app/outboxworker/planslacknotifier.go`) never persists — no
buttons exist to press — and nothing wedges, because decisions flow through the web REST path
(`httpapi/decideplan.go` serves Slack/Linear/web identically). The shadow operator decides from
Narvi's own UI, which is precisely the observation channel shadow keeps open.

### 30.8 The flag: granularity, resolution, fail direction, and epoch discipline

**One authority: Postgres.** `repo_settings.live_egress_enabled bool NOT NULL DEFAULT false`,
read per call at each egress seam through the resolver package. The polarity is the entire
point: the codebase's established repo-settings read idiom treats `ErrNoRows` *and any read
error* as `false` (`sessionactor/reviewretrigger.go`'s auto-retrigger read;
`appreviewverdict.AutoMergeEnabled`'s identical shape, `internal/app/reviewverdict/config.go`).
With `live_egress_enabled`, that idiom yields **suppress-on-error for free**, and every newly
connected repo starts in shadow — which is exactly the onboarding story this capability exists
for. The inverted spelling (`shadow_mode bool`) would resolve a Postgres blip toward LIVE — the
forbidden direction. A dedicated test pins the resolver to "suppress" on `ErrNoRows`, on
arbitrary error, and on an absent row.

**A deployment-level master switch**, `NARVI_SHADOW_MODE` (the `EpistemicCheckDefault`
env-pattern), forces shadow for the whole process; effective mode = `platformShadow OR NOT
live_egress_enabled`. Its sharp edge is named: the switch is per-process and the fleet is
multi-pod (the outbox's `FOR UPDATE SKIP LOCKED` claim exists precisely because of that), so a
rolling restart that changes it produces a mixed fleet in which a still-live pod really delivers
rows a shadow pod enqueued. It is therefore reserved for **dedicated evaluation deployments**
and documented "never flip on a running fleet"; the per-repo Postgres flag remains the
transactional authority every pod sees. There is **no per-session "go live" override** — that
would reintroduce the disciplinary leak this design exists to remove; a per-session
*force-shadow* override (monotone toward suppression) is admissible later.

**Epoch discipline — the unifying correction.** The system takes its *decisions* at
enqueue/record time but the first draft read the flag at *effect* time; every lifecycle race
reduces to that gap. The rule: **stamp the effective egress mode onto every durable decision
artifact, and suppress if the stamp OR the current flag says shadow** — monotone toward
suppression, in both directions. Concretely:

- **Outbox rows born in shadow are terminally shadow** (the enqueue-time stamp, §30.6): retries
  reaching ~35 minutes and documented indefinite backlogs mean a `github_verdict` or
  `sentinel_auto_fix` row pending across a shadow→live flip would otherwise materialize as a
  real review, branch, and PR. A born-shadow row can only end in the ledger. Rows born live and
  delivered after a live→shadow demotion are suppressed by the delivery-time check —
  suppress-wins both ways. The record-or-fail → backoff → retry-after-flip race is closed by the
  same enqueue stamp.
- **Shadow-era verdicts must never arm auto-merge after promotion.** The auto-merge worker's
  candidate query (`ListLatestAutoApproved`, `internal/app/automerge/worker.go:111`, 7-day
  lookback) has no way to distinguish a verdict that was never visibly delivered. Every
  `review_verdicts` row is stamped with its egress mode at write time and the exclusion lives
  **in the query, never at call sites**; promotion additionally sets a fence — only verdicts
  after the promotion timestamp are candidates. The same stamp gates re-trigger and the §21.3
  digest (a daily rollup would otherwise reveal phantom reviews to the customer's channels).
- **The push/PR pair resolves its mode once per turn.** Push and `CreatePR` are two separate
  async stages (`sessionactor/pushpr.go`); a flip between them yields either an orphan branch in
  a customer repo or a live `CreatePR` on a never-pushed branch. The mode is resolved at push
  send, persisted with the signal, and `createPRBestEffort` honors the persisted decision —
  never a re-read. A flip takes effect at the next turn boundary, atomically for
  push+PR+preview. (Whether a mid-session flip should instead apply only to sessions created
  after it is a deferred semantic, §30.9.)
- **Snapshots crossing modes** are §30.4(3)'s fail-closed bit; **demotion with live sandboxes**
  is §30.4's mandatory termination; **live Slack buttons surviving demotion** (an Approve click
  after the flip is a synchronous `chat.update` into the customer workspace) are covered by the
  §30.3 interactive-client seam resolving the mode per request — with an optional last live act
  at demotion: `chat.update` pending plan messages to remove their buttons.

**Promotion (shadow→live)** is the safe direction on the sandbox side — a shadow repo's
sandboxes have never held more than read-only. On the state side it requires the auto-merge
fence above plus explicit quarantine of shadow-era artifacts (synthetic refs must never become
live-actionable); Activate refuses, or explicitly quarantines, while shadow-era rows exist
unhandled for the repo.

### 30.9 Residual limits, open verification items, and decisions explicitly deferred

**Open verification items** (unverifiable from this repo; must be resolved before the guarantee
is declared complete):

1. **Modal snapshot semantics.** The provider API is invented in-repo
   (`internal/adapters/outbound/modal/wire.go`); whether restore is a cold re-boot (boot-time
   purge runs) or a warm resume (it never does) is **load-bearing** for any boot-time cache
   purge. The design does not rest on the answer — §30.4(3)'s snapshot-mint-time purge is the
   control that holds either way — but the question must be answered before boot-time purging is
   credited with anything.
2. **The external image-build service.** Whether its own clone credential leaves residue in
   baked images is out of this repo's reach; closing it is a contractual/audit requirement on
   the service, tracked with the §30.4(2) in-repo fix but not satisfied by it.
3. **Sandbox network egress** remains §30.1's named v1 residual: assumed, not omitted.

**Decisions deliberately left open** — each is represented here so it surfaces at its Step
rather than being silently defaulted; none is resolved by this section:

- **Git mirror in v1 — RESOLVED: NO. Short-circuit, done properly.**

  The "Against" above was one sentence, and it understated the cost in the one way that matters
  for this section. A per-session bare mirror puts **complete customer repositories at rest on
  Narvi's own storage** — and the ledger-retention bullet below already records that customer
  code at rest has no retention or null-out policy. Shadow's promise is "we touch nothing";
  answering it by copying the whole repository onto our disks is the wrong direction, before
  counting the git service (receive-pack, auth, lifecycle, GC) that two different sandboxes must
  both reach.

  The "For" list is real, but most of it is a consequence of letting the push FAIL, not of
  lacking a mirror:
    - *"no `push_complete`, so no 'would have opened PR …' is ever recorded"* — the push signal
      already carries the session and the branch. Short-circuit the push **before** it fails and
      record the intent from what is already in hand.
    - *"every turn surfaces a `push_error` that makes Narvi look broken"* — same fix. An
      intentional, recorded suppression is not an error, and the evaluator reads it as the
      product working.
    - *"the sentinel-fix lane only runs coherently against a mirror"* — accepted. That lane
      short-circuits before the one-shot claim (§30.7), findings staying `open`.

  §30.4 endorses gating the push for exactly this: "the turn's push/PR/preview trio resolves one
  mode atomically **and the ledger gets its entry** — UX and state coherence". It denies only
  that the gate is *security*. Step 101 left `sendPushBestEffort` ungated and said so; this is
  the Step that takes it, for the stated reason.
- **Chat-originated triggers in shadow — RESOLVED: the trigger runs, and every outward effect
  it produces is suppressed and recorded.** Narvi does not refuse Slack- or Linear-originated
  work in shadow.

  The framing that made this look like a trade-off was wrong, and naming the error is the
  argument. "Refusing is cleaner, but the workspace goes silent" assumes refusal can announce
  itself. It cannot: a "shadow mode, not running this" reply IS a message in the customer's
  workspace, which is precisely the trace §30.1 calls total failure. **Refusal is exactly as
  silent as suppression.** It buys no clarity for the person in chat, and costs the evaluation
  every chat-originated path.

  So the only real difference is whether the work happens at all, and there the case is
  one-sided. Shadow exists to evaluate Narvi on live customer systems; the evaluator's own
  visibility comes from the ledger surface (§30.6, Step 104), not from the workspace they are
  deliberately being kept out of. A chat-triggered shadow session can read and can spend LLM
  budget — the former is what §30.1 already excludes from the guarantee, the latter is surfaced
  rather than suppressed by the same section. It cannot write: §30.4's read-only credential is
  the structural control and does not care what triggered the turn.

  Operative rule for Step 102's seams: an ack, an ephemeral, a `chat.update`, a view, a Linear
  response or thought activity resolved shadow is suppressed at the client-method level and
  written to the ledger with enough context for the evaluator to see what the workspace would
  have shown. **Never a synthesized success and never a visible refusal** — the trigger is
  honoured, its output is not sent.
- **Customer LLM spend**: accept as inherent (surfaced per §30.6) or force an org-level
  evaluation key for shadow runs.
- **RWX preview in shadow**: the public dispatch is suppressed either way (§30.1 — a public
  preview URL is a trace); the open choice is whether an internal, non-public rendering ships as
  an evaluator feature (a new product surface) or previews are simply absent in shadow.
- **Ledger retention/PII** (customer code at rest): retention window and null-out policy remain
  OPEN — the schema-time enabling move is taken (§30.6), the policy is not, and nothing gates
  Step 104's ship on it.

  The **visibility threshold** was the part that did gate a Step, and it is **RESOLVED:
  admin-only** — not by falling back to the default, but for a reason worth stating, because it
  also constrains when the threshold may later be widened.

  This ledger is the only surface in the product that holds **customer source code at rest, in
  full**. That is deliberate: `shadowledger.UpdateFileContent` carries `Content` whole rather
  than a length or a hash, because "what would this have written into my repository" is the
  evaluator's entire question and a digest does not answer it. The consequence is that a
  `shadow_scm_writes` row can contain more of a customer's private source than any other screen
  Narvi exposes — beyond even the Settings → Members audit-log row, which is already admin-only
  (§16.1's dead-lettered-outbox-deliveries precedent).

  And by the bullet immediately above, that corpus has **no retention window and no null-out
  policy yet**. Widening read access to a body of customer code whose lifetime nobody has
  decided is the wrong order of operations: decide how long it lives before deciding who may
  read it. So admin-only is the answer now, and **the retention decision is the gate on
  revisiting it** — maintainer+ becomes arguable once retention exists, and not before.
- **Downstream-chain synthesis vs single-hop validation — RESOLVED: single-hop, documented.**

  Synthesizing a `github_pr_session` from the suppressed `CreatePR` record would exercise
  review-of-own-work, auto-approval, description-autofix and handoff internally. Every one of
  those lanes wants to WRITE. Synthesis therefore buys internal-only observability by
  multiplying the number of write paths running inside a capability whose bar is that **one**
  leaked PR is total failure — the wrong trade at any exchange rate.

  It is also incoherent without a mirror, which the decision above settles: there is no real ref
  for those lanes to act on, so they would be reasoning about a head that exists only in a
  transient sandbox workspace.

  §30.7 already names the honest posture while this was open — "no downstream-lane claims in the
  operator view beyond what actually ran". It is now the decision, not the default.
- **Mid-session flip semantics**: next-turn-boundary (the design as written, §30.8) vs
  applies-only-to-new-sessions.

### 30.10 Phasing

Phase 8, Steps 96-104 (implementation plan). The minimal *safe* subset is Steps 96-101: a
dedicated evaluation deployment (`NARVI_SHADOW_MODE=1`, credential-starved, GitHub webhooks
only) can then be attached to a real customer repository — the transport gate, the outbox
classification, the read-only installation token, and the UID boundary close every path
reachable in that configuration, and the ledger records. Step 102 becomes **mandatory the moment
a customer's Slack or Linear is connected**. Steps 103-104 are what make the evaluation *good*
rather than merely safe: lanes observable end-to-end, and a product surface an operator can
actually evaluate from and graduate with.

## 31. Per-repository knowledge, two modes: full injection and a retrieved corpus (new capability)

Problem this solves: Narvi's review lane already *learns* per repository — maintainer-taught
false-positive patterns (§22.2) and, since Step 69, a typed architecture digest on every deep
review — but only the first is ever read back, and it is read back by full injection into the
prompt, a strategy whose cost is linear in what has been taught. At the target deployment scale
(hundreds of repositories, thousands of PRs/week aggregate, hot repositories at 100–500 PRs/week)
that strategy holds for some knowledge sources and provably breaks for others. The requirement,
decided and not re-litigated here: **two per-repository modes — mode A injects everything into the
review prompt (today's shipped pattern), mode B maintains a documentary corpus in OKF form,
embedded and queried by RAG retrieval — with strict per-repository isolation and no cross-repo
context leakage.** This section specifies both modes, the corpus, the isolation, and the poisoning
surface the corpus opens; it synthesizes a grounded design pass plus four adversarial attacks
whose corrections are folded in as requirements, not appended as caveats (the same provenance
discipline §30's own intro records).

The load-bearing discovery, stated first because everything else leans on it:
`review_verdicts.digest_arch_decisions` (JSONB, migration 000077) is **required by the schema on
every deep-path verdict** (`ErrEmptyDigestArchDecisions`,
`internal/domain/reviewpost/validate.go`) and **never read back into any future prompt** —
verified: its only consumers are the insert path (`internal/app/reviewverdict/insert.go`,
`convert.go`), the SQL read models, and the §26.5 contestation hash
(`internal/domain/reviewpost/digestsectionidentity.go`). Narvi already manufactures durable,
typed, per-repository architectural knowledge on every deep review — contractually, not
optionally — and then never reuses it. The mode B corpus is not content to invent; it is a reuse
path for content already produced. It is also what makes the volume argument concrete:
arch-decision accumulation is O(deep-path PRs) — at 300 PRs/week and a ~30% deep-path share,
~15–50+ decisions per repository per week, thousands per year. Inject-all breaks provably on that
source; retrieval is justified there and only there.

### 31.1 The two modes, and mode as a per-SOURCE property

**Mode A (low volume — the default, and today's shipped behavior extended).** Every knowledge
block is injected in full into the review prompt: the maintainer-taught false-positive patterns
(`internal/app/reviewcontext/falsepositive.go` → the LIMIT-less
`ListActiveFalsePositivePatterns` query,
`internal/adapters/outbound/postgres/queries/reviewfalsepositivepatterns.sql`), plus — new — a
"prior architecture decisions" block fed by a bounded, deterministic SELECT over
`review_verdicts.digest_arch_decisions` (§31.6 item 1 for the selector's exact shape). No
embeddings, no index, no corpus artifact, no new external dependency of any kind.

**Mode B (high volume).** A per-repository documentary corpus, rendered as OKF concept documents
(front-matter + typed body, §31.3), **embedded and queried by hybrid RAG retrieval** (a vector
leg and a lexical leg, fused by RRF — §31.5). The prior-decisions block draws from the **same
server-derived candidate gate as mode A** (§31.6's selector — gate-then-rank): retrieval
re-ranks the gated candidates, it never re-selects them, because hybrid retrieval improves
recall and does nothing for poisoning — the two concerns are orthogonal, and only a
server-derived key addresses the second (§31.5, §31.7). A pull-mode `kb_search` tool
additionally becomes available to builder/plan sessions (§31.6 item 2 — deliberately ungated,
its own posture stated there).

**The structural point: the flag is per repository, but internally the mode is a property of each
knowledge SOURCE — and only sources with two genuine strategies route on it.** The sources do not
share a scaling regime, and the design reflects that rather than flattening it:

| Source | Scaling regime | Mode A | Mode B |
|---|---|---|---|
| False-positive patterns (`review_false_positive_patterns`, migration 000073) | Bounded by human teaching throughput (`false positive:` commands behind maintainer+ RBAC, §22.2) — tens per repo, invariant to PR volume | inject-all | **inject-all (unchanged)** |
| Architecture decisions (`review_verdicts.digest_arch_decisions`, migration 000077) | Linear in deep-path PRs — the one source that grows at machine speed; never read back today | the §31.6 gate, ordered by recency | **the same gate, re-ranked by hybrid RAG (§31.5)** |
| Already-answered facts (`ListOpenAndRebuttedReviewFindings`) | Bounded per (repo, pr_number) — does not grow with the fleet | inject-all per PR | inject-all per PR (§22.1.2 reaffirmed) |
| Approved plans | Bounded by plan sessions; prose currently perishable (§31.3) | — | durable via Step 105; corpus ingestion out of Phase 9's scope (§31.3) |
| `kb_search` (pull, builder/plan sessions) | — | absent | **present** |

**The per-source doctrine is confirmed, not open**: a repository in mode B keeps its
false-positive patterns injected in full. They are a maintainer's explicit standing instructions;
their corpus is human-bounded by construction (RBAC + an explicit command + a mandatory
retirement lifecycle, §22.4); and a retrieval pass that silently dropped one would be a trust
break — a maintainer taught it precisely so it would always be present. The uniform alternative
(route everything through retrieval for symmetry) was weighed and rejected on exactly that trust
argument. The existing advisory-injection defense survives this section unchanged and is worth
restating on its own terms: `internal/domain/falsepositive/advisory.go`'s renderer is
"structurally incapable of acting as a filter" (its own doc-comment heading) — the reviewing
model, not the pipeline, weighs the patterns. That defense implicitly assumed a human-bounded
source; this section makes the bound explicit and keeps the defense standing on it.

**Similarity over already-answered facts stays rejected, in both modes.** §22.1.2 rejects
resemblance-based suppression by name, and that rejection is principled, not volumetric — high
volume argues *for* it: a resemblance threshold that is wrong x% of the time silently drops
*more* real findings as volume grows. The set is bounded per (repo, pr_number) and does not grow
with the fleet; nothing in this section touches it, and no future Step may cite this section as
grounds to revisit §22.1.2.

### 31.2 The switch, and the mode buffer this codebase has already paid to learn

**Residence and actor.** A nullable `repo_settings.review_knowledge_mode` column, `NULL` = mode A
— the exact precedent of `review_depth_mode` (migration 000082) and the cost-budget columns
(000085). Fail-safe: absent row, read error, or NULL all resolve to today's behavior. The flip is
**manual, by an admin, never automatic**: every comparable unsupervised behavior in this codebase
is an explicit admin config (000082, 000085, auto-approval 000069), and mode B changes what the
reviewer sees for the one source that actually routes on it — the prior-arch-decisions block
becomes the same gated candidate set re-ranked by retrieval instead of ordered by recency (a
flip changes the ordering of eligible rows and the substrate that serves them, never which rows
are eligible — §31.6's gate is mode-invariant), while the false-positive patterns
a maintainer taught stay injected in full, unchanged, in either mode (§31.1's per-source doctrine
— this sentence does not contradict it, an earlier draft's wording did) — which is exactly the
class of change this codebase keeps behind deliberate configuration.

**The instrumented trigger — per source, because only one source can ever move it, and a
combined gauge cannot motivate a flip at all.** A token gauge on the rendered knowledge block,
emitted at the existing render site, split into three numbers: the false-positive-pattern block,
the arch-decisions block, and the total. The split is load-bearing, not cosmetic — it corrects a
defect a single combined gauge cannot escape: per §31.1's own table, false-positive patterns are
inject-all in **both** modes, and the arch-decisions block is bounded in **both** (the gate
ordered by recency, `LIMIT k`, in mode A; the same gate re-ranked by RRF, top-N, in mode B) — so
no component a flip actually touches can
grow past any threshold, and the one component that CAN grow (false-positive patterns) is the one
a flip never touches. A single total gauge, alerted-on and flipped-on, therefore does not move
after the flip — the exact operator flapping this correction exists to prevent, now closed at its
root instead of relocated.

**What the false-positive-pattern gauge is: a health metric, never a mode-B indicator.** It should
still alert — a maintainer-taught corpus growing past a few thousand tokens is a real operational
signal (teaching hygiene, retirement backlog, §22.4) — but because §31.1 never routes this source
through retrieval, no threshold on it can ever correctly indicate mode B, and none is proposed
here. (The retracted draft's thresholds were denominated in *pattern count* — ~100, ~200 — which
was itself the tell: false-positive patterns are the only component a token count on this source
could ever actually be counting.)

**The real A→B indicator is relevance exhaustion, which no token gauge can observe — the §26.5
instrument already measures it.** What degrades as a mode-A repository scales past the point
retrieval exists to fix is not the arch-decisions block's *size* (bounded either way) but its
*relevance*: recency ordering within the gate (§31.6's selector) covers a shrinking slice of
real elapsed time as PR volume grows, so the injected decisions increasingly miss the one the
current PR needs — exactly the "the motivating case is reached deterministically... at 300
PRs/week" vs. "2-3 days for raw recency" gap §31.6 itself names, now playing out within the
gated set. And since a flip changes exactly and only that ordering (the gate is mode-invariant,
above), an ordering-relevance signal is the right indicator for it. The already-shipped §26.5 contestation KPI
(migration 000086), joined on the verdict knowledge-mode stamp (item 2 below), is the instrument
that actually observes this: a rising contestation rate on knowledge-influenced verdicts is the
real trigger, corroborated by a rising `counter_review='skipped'` / `fact_check='skipped'` rate
per verdict row since Step 69 — a growing knowledge block silently consumes the §26.7 budget's
margin (`internal/domain/reviewtriage/costbudget.go`), so mode A's degradation shows up in
*quality* (deep passes skipped) before it shows up in cost. Both discriminate cleanly where a
token count cannot: they move only when the arch block's relevance is failing, never merely
because it grew, and "A, degraded" is distinguished from "B, healthy" by the verdict mode stamp
below.

**The mode buffer — non-negotiable, because this codebase has already shipped and fixed two bugs
of exactly this class.** Step 69's D2/D9 fixes in
`internal/app/sessionactor/reviewretrigger.go` closed a pair of defects where a mutable
`repo_settings` flag resolved at two different instants produced a self-contradictory decision
record, and the hard-won rule is already written into the schema — `turns.review_depth`
(migration 000080: "set exactly once, at creation … never re-derived") and
`review_verdicts.review_path` (000081). This section applies that rule verbatim, and it is why
the grounded pass's "zero new instrumentation" promise is **retracted: a schema change is
mandatory**, cheap, and idiomatic (precedent 000081):

1. **`turns.review_knowledge_mode`** — resolved once at turn creation, never re-derived.
2. **A knowledge-mode column on `review_verdicts`, stamped at write.** Without it the flagship
   A/B KPI (§31.6 item 1) is contaminated at the instant of any flip: `review_verdicts` is
   append-only (§21.1), and joining the *current* flag against history attributes every pre-flip
   verdict to the post-flip mode. The exclusion/attribution logic lives in the query, never at
   call sites — the same in-query discipline §30.8 imposes on its egress-mode stamp.
3. **`kb_search` authorizes against the turn's stamped mode, never against `repo_settings`
   live** — otherwise a B→A flip mid-turn breaks a tool the prompt has already announced. The
   precedent is the verdict tool itself: `cmd/sandbox-agent/reviewverdicttoolprompt.go` bakes
   URL/bearer/gen into the prompt at hand-off and serves it minutes later.
4. **The cold-corpus fallback, mandatory at every A→B flip — simpler to reason about under
   gate-then-rank, and still necessary.** The composition makes the flip smaller than it once
   was: a flip no longer changes which rows are *eligible* (the §31.6 gate is mode-invariant),
   only how the gated candidates are ordered and which substrate serves them — and that
   substrate change is exactly why the rule survives restated rather than deleted: mode B reads
   the corpus *index* (ingestion watermark, G4 promotion), which is empty on flip day no matter
   how many gate-eligible verdict rows exist. The existing injection idiom
   (`FetchFalsePositivePatterns`) proudly degrades to an empty string indistinguishable from "no
   history" — correct for its source, catastrophic if copied here: the block renders "", and the
   review is silently strictly worse than the day before. Rule: **if retrieval returns fewer
   than k rows, fall back to mode A's SELECT — the same gate, ordered by recency** — a one-line
   guard that fully neutralizes the regression, and a smaller behavioral delta than before
   because the fallback now changes only ordering and substrate, never the eligibility rule —
   and stamp the degradation on the turn (it lands in the same JSONB decision record as the
   injected ids, §31.6 item 1, so degraded turns are excluded from the B arm of the KPI rather
   than contaminating it).
5. **A per-repository ingestion watermark**, so a B→A→B cycle leaves no un-embedded hole for the
   verdicts written in the interval.

**Mode B's added requirements are checked at MODE ACTIVATION, never at boot.** Mode B is opt-in
per repository, so its external dependencies — the embeddings provider's reachability and
credentials, and, on the named upgrade path only (§31.5), the pgvector extension via a
`pg_available_extensions` preflight — are validated when an admin flips a repository to B, with a
loud typed refusal on failure. Nothing mode-B-shaped runs in `applyMigrations`'
(`cmd/control-plane/main.go`) unconditional boot-time chain, which is what preserves §12.1's
"one binary + Postgres" self-host contract verbatim for every deployment that never uses mode B.

### 31.3 The mode B corpus: content, entry barrier, residence, chunking

**Phase 9's corpus contains exactly one concept type: `arch-decision`.** Two more knowledge
sources are adjacent but deliberately out of the corpus's scope for this phase — stated
explicitly because §31.1's table names both and an earlier draft's "three concept types" framing
implied a Phase 9 Step ingests them, when none does:

- **`false-positive-pattern` is never a corpus member.** §31.1's doctrine already settles that it
  is injected in full in **both** modes and never routes through retrieval — corpus membership
  would only ever matter for `kb_search` or the export, and neither needs it against a source
  inject-all already serves completely. A future Step may add it (so `kb_search` can surface it
  alongside arch-decisions during a builder session), and that Step names its own ingestion and
  chunking; nothing here presumes it.
- **`approved-plan` gets its durability fix (Step 105) but not corpus ingestion.** The only agent
  prose a human has explicitly signed (`plans.status='approved'`, human `decided_by`, migration
  000034) lives today only in `events`, which is `ON DELETE CASCADE` from `sessions` (000008, and
  the plans table's own session FK, 000034) — an approved plan is cascade-deletable, a defect
  independent of any corpus. Step 105 snapshots the approved version's prose at approval time into
  a dedicated **`plan_documents` table keyed on the plan** — a separate table, not a snapshot
  column on `plans`, so the content is isolable for a later retention null-out without rewriting
  the parent table (the same schema-time enabling move §30.6 takes for its ledger) — closing the
  data-loss defect regardless of everything else in this section. Rendering that snapshot as an
  OKF concept, chunking it (the `##`-section unit, below), embedding it, and serving it through
  `kb_search`/the export is a later Step's scope, not named in Phase 9. **The irreversible loss is
  recorded plainly: any plan whose session was deleted before Step 105 ships is gone, and no later
  Step can ever recover it.**

`arch-decision` — the typed triplet
`ArchDecision{Decision, RejectedAlternative, ConventionConformance}`
(`internal/domain/reviewpost/digest.go`). One OKF concept per decision: front-matter (content
id, repo, source {pr_number, head_sha, review_path, counter_review}, the §31.6 gate's
server-derived tags/directory-roots copied from the source verdict's INSERT-time stamp — the
filter columns mode B's gate matches on, never re-derived at ingestion, provenance class, epoch,
eligibility state, date), body = the three fields verbatim in three sections.
`RejectedAlternative` is the irreplaceable payload: the road not taken leaves no trace in any
checkout — no deterministic selector, and no amount of re-reading the repository, can
reconstruct it.

**Excluded, with reasons**: the per-PR digest prose (`Summary`, `StackRisks`,
`UnverifiedLimits`, `ContestedPoints`) — one PR's commentary, valueless to another PR's review
six months later; verdict scalars — analytics, already owned by the read models
(`internal/domain/reviewverdict/rollup.go` and neighbors); un-promoted rebuttals — the codebase
already has a deliberate promotion valve (`false positive: <reason>`, §22.2), and harvesting
rebuttals automatically would bypass that maintainer gate.

**The entry barrier: merged AND uncontested, no backfill, contested content hard-excluded — this
is `arch-decision`'s own promotion condition, not a generic corpus-wide rule.** Promotion into
retrievability (§31.7 G4) requires the source PR merged, a quarantine window elapsed, and zero
contestations against the decision — a condition that presumes a source PR exists, which is true
of the only concept type Phase 9's corpus admits (above). It is stated as this type's own
condition, not the corpus's, deliberately: a future concept type ingested later must name its own
promotion condition when it lands rather than silently inheriting "merged," which no non-PR-sourced
concept could ever satisfy — a false-positive-pattern's natural gate would be capture (maintainer
authority is already the gate, §22.2) and de-eligibility on retirement; an approved-plan's would be
the human approval itself. G4's own "default = non-retrievable" (§31.7) stays the fail-closed rule
regardless of type; what varies per type is only what *fires* it. Two consequences of this Step's
own condition are decided, not open:

- **No backfill, and the reason is a forcing fact, not taste: no COMPLETE reviewed-PR merge
  signal is recorded in Postgres today, so historical rows can NEVER be gated retroactively on
  one.** Three PARTIAL signals exist, each covering a different slice, none universal, and none
  built for this purpose: `sentinel_fixes` (migration 000047) is keyed on the reviewed PR itself
  (`UNIQUE (repo_full_name, origin_pr_number)`) and its status is advanced by that PR's own merge
  flag (`handlePullRequestClosed` reads `payload.PullRequest.Merged`, marking the row abandoned
  when false) — but only for the small subset of PRs where a sentinel auto-fix was actually
  triggered; `auto_approval_outcomes.confirmed` (migration 000070) records a merge outcome too,
  but only for PRs the auto-approval engine itself approved (§21.2) — a different, narrower subset
  again; and `audit_log`'s `merge_pr` rows (`internal/adapters/inbound/httpapi/decisioninbox.go`)
  capture a merge only when it went through the decision inbox's own 1-click endpoint, never a PR
  merged directly on GitHub outside it. None of the three is a general-purpose "was this reviewed
  PR merged" signal, and reconstructing one by unioning them would be exactly the kind of
  inferred, never-designed-for-this-purpose join this section's own G4 (§31.7) exists to avoid.
  Backfilling on any of them, or their union, would therefore still import ungatable content into
  a corpus whose entire safety story *is* the gate — and the quarantine window is retroactively
  vacuous for a backfill regardless (everything historical is already older than any window).
  Instead, **merge-outcome capture starts now** (Step 107: the GitHub ingress records the
  `closed`/merged outcome onto `github_pr_sessions`, already keyed `(repo_full_name, pr_number)`,
  migration 000028) and the corpus fills forward.
- **Contested decisions are hard-excluded at retrieval, and this does not contradict §22.3 —
  different thing, different rule.** §22.3's advisory-never-a-filter posture protects *findings*
  on their way to a **human** who can weigh an annotation; §22.1.2 extends the same care even to
  retired findings (rendered, annotated, never silently dropped). What is excluded here is
  content served to a **model as precedent** — which is exactly the poisoning vector §31.7
  exists to close. Annotating a poisoned precedent and handing it to the model anyway is not the
  cautious option; for machine-consumed precedent, exclusion is. The never-a-filter posture is
  untouched where it applies: nothing here drops, or annotates away, any finding shown to a
  human.

**Residence: Postgres is the truth, OKF is a pure rendering — argued, and decided.**

- **The truth and its lifecycle are already there.** Every source row and every mutation the
  corpus must respect (`retired_at`, contestations — migration 000086 — plan approval) lives in
  Postgres. A file-based corpus would have to mirror those mutations into git, and a retirement
  by commit does not purge git history: a corpus repository is an exfiltration channel by
  construction — customer-derived text in an unpurgeable history — with unreviewable PRs.
- **§5.1 forbids a second state authority.** A git repo carrying the "real" corpus while
  Postgres carries the lifecycle *is* a second authority, with a sync protocol as its failure
  surface.
- **Isolation becomes an auditable predicate** (a composite index prefix, §31.4), not a
  directory convention.
- **Rendering is the house idiom**: impure fetch / pure render
  (`FetchFalsePositivePatterns` / `RenderAdvisoryBlock`). "Render this row as an OKF concept" is
  one more pure function in a domain package, deterministic and reproducible on demand.

What Postgres-as-truth deliberately does NOT give: a browsable tree, a git history of the
knowledge's evolution, a review workflow on edits. The owner asked for OKF by name, and the
honest form of that ask is decided: **a read-only rendered export ships, as a small, late,
read-only Step** (§31.6 item 6) — an endpoint generating a repository's active concepts on
demand, behind the per-repository entitlement (§31.4). It is an export surface, **never an
authority, never the retrieval substrate, and never written into the customer's repository**
(which would collide with §30's zero-trace guarantee the moment an evaluated repo was involved).

**Chunking: the concept IS the chunk.** For `arch-decision` — the only type any Phase 9 Step
actually chunks or embeds, above — measured shape is ~50–150 tokens. Sub-chunking one would be
actively harmful: the knowledge is the *pair* decision / rejected-alternative (the struct's own
doc comment: it states what was built AND what was not) — splitting it amputates the precedent of
its alternative, which is precisely the retrievable insight. So: retrieval unit = whole concept,
one embedding row per concept, embedded text = rendered body prefixed by a one-line typed header,
the rest of the front-matter serving as filter columns. The remaining two rules are forward
design guidance for whichever future Step ingests the other two concept types, not exercised by
anything in this phase: a false-positive `reason` runs a sentence to a paragraph (~10–60 words) —
same whole-concept-is-the-chunk rule; an `approved-plan` is the **one exception**, unbounded
prose, unit = the `##` section, every chunk sharing the concept's id and metadata. No
paragraph-level chunking anywhere.

### 31.4 Per-repository isolation, structurally — and the entitlement boundary isolation cannot provide

**Why per-query discipline is disqualified: two cross-repo bugs already shipped.** The
false-positive queries file documents its own audit fixes:
`GetFalsePositivePattern` and `RetireFalsePositivePattern` were "keyed on id alone"
(`internal/adapters/outbound/postgres/queries/reviewfalsepositivepatterns.sql`, the file's own
comments), letting a pattern belonging to a *different* repository be read — and retired —
through the wrong repo's URL. A cross-repo read AND a cross-repo mutation, shipped, caught only
by audit. `WHERE repo_full_name = $1` as a per-query convention is exactly the class of
disciplinary guard §30 was written to replace with structure.

**The four layers (deliberate one-way redundancy, §30.2's form):**

1. **An opaque `knowledge.RepoScope` type** (unexported field,
   `internal/domain/knowledge/scope.go`): every store method takes a `RepoScope`, never a
   string — a caller holding only a string does not compile. The query type
   (`knowledge.Query{Text, K}`) **carries no repo field at all**: a cross-repo query is
   inexpressible; the vocabulary for naming a second repository does not exist. Constructors are
   capability tokens taking a trusted typed artifact — `ScopeFromPRSession(row)`,
   `ScopeFromWebhookMention(m)` — plus exactly one string-accepting constructor for authorized
   admin routes, pinned by a single-call-site architecture test (the §30.6
   synthetic-ref-constructor idiom). Zero value = fail closed, typed error.
2. **A scoped handle**: `KnowledgeStore.ForRepo(scope) *ScopedKnowledge` — query methods have no
   repo parameter; only the handle supplies the predicate from its captured scope. Forgetting
   the WHERE becomes unwritable, not merely unlikely.
3. **Schema-level confinement**: a composite FK `(doc_id, repo_full_name)` from chunks to docs —
   a chunk claiming a different repository than its parent is a constraint violation at INSERT,
   and no JOIN can smuggle foreign rows through; and **deliberately no global ANN index** — each
   search is an exact scan over the repository's own B-tree slice (§31.5's cardinality argument
   makes this free), so no cross-repository similarity structure exists anywhere for a bug to
   traverse.
4. **Fail-closed RLS on the knowledge tables only**: policy
   `USING (repo_full_name = current_setting('narvi.repo_scope', true))`, the `SET LOCAL` issued
   in exactly one place (inside the scoped handle). §30.8's polarity: an absent setting yields
   NULL, the policy evaluates false, the query returns **zero rows, never a foreign row**. One
   verification item before this layer is credited (§31.9): `FORCE ROW LEVEL SECURITY` is
   required if the application role owns the tables.

Tests: a two-repository integration test (the `*_store_integration_test.go` precedent), a
zero-value unit test, and two pinning tests — the string constructor's single call site, and a
mechanical CI scan that every named query in `knowledge.sql` carries the repo predicate.

**What these four layers do NOT defend — the adversarial pass's blocking finding, and the
entitlement decision that answers it.** The layers guarantee "one query reads exactly one
repository." The required boundary is "a caller reaches only repositories it is entitled to."
Narvi has no per-repository entitlement model today, and the gap is traversable:

- `authz.Actor` carries a deployment-global role
  (`internal/adapters/inbound/httpapi/helpers.go`); `authz.Resource` carries only
  `OwnedOrJoined` (`internal/domain/authz/authorize.go`). No repository identity exists in the
  authorization vocabulary, and no per-repo membership table exists in the schema.
- **`kb_search` as naively sketched launders an attacker-supplied scope**: `CreateSession`
  authorizes with `Resource{}` (`internal/adapters/inbound/httpapi/create.go` — correct for
  "may this role create sessions", silent on *which repos*), the request's `repos` list is
  validated in shape only and persisted verbatim into `sessions.repos` (migration 000018). A
  `member` creates a session naming a repository they have no right to; `kb_search` derives its
  scope "server-side" from that row — and serves the victim's corpus while all four isolation
  layers pass cleanly: they guarantee the query is single-repo, they cannot see that it is the
  *wrong* repo. Amplification: the sandbox clone uses the installation's credential helper for
  every repo in the session's list (`internal/sandboxagent/gitclone/clone.go`) — the source
  itself is clonable through the same laundering, independent of `kb_search`.
- **Cross-repo reads leak today**: `GET /api/repos/{owner}/{repo}/review-analytics` authorizes
  `ActionViewAnalytics` with `Resource{}` and then uses the route's repo only to scope the query
  (`internal/adapters/inbound/httpapi/reviewanalytics.go`); the same shape exists in
  `reposettings.go`, `falsepositivepatterns.go`, and `providercredentials.go`. **The four leaks
  are role-scoped, not uniformly open — precise, verified against `authz/authorize.go`'s own
  matrix**: `ActionViewAnalytics` admits every role including viewer, so any authenticated user
  reads any repository's analytics by editing the URL. The other three do not have that width;
  each still gates on its own RBAC action, just against the wrong repository.
  `falsepositivepatterns.go`'s both routes (list and retire) gate on
  `ActionManageFalsePositivePatterns` — admin+maintainer only, a member or viewer gets 403
  regardless of URL — so the leak lets any maintainer or admin read (or retire) any repository's
  taught patterns. `providercredentials.go` gates on `ActionManageRepoSecrets` (also
  admin+maintainer), so the same leak lets any maintainer or admin rotate any repository's
  provider credentials. `reposettings.go`'s fields split across several actions, some
  admin+maintainer and some admin-only, each leaking within whichever tier that field already
  requires. An OKF export would inherit this pattern with strictly more sensitive content.

**The scope decision is taken: minimal, not an RBAC rework.** A per-repository entitlement
predicate joins the authorization path, plus authorization of every `sessions.repos` entry at
session creation — before persistence and before any clone, which closes the clone amplification
in the same move. The full per-repo-roles RBAC rework is explicitly not this chantier. Two
halves, two vehicles: **the four leaking handlers above are being closed right now as an
independent fix already in flight** — repo-from-the-URL authorization added to
`reviewanalytics.go`, `reposettings.go`, `falsepositivepatterns.go`, and
`providercredentials.go`, not delivered by this section's Steps and not waited on as design
work; **the remainder — the entitlement predicate itself and the `sessions.repos` gate — is this
chantier's Step 106**, and `kb_search` and the OKF export are blocked behind it. Every
`RepoScope` constructor takes the actor alongside the trusted artifact once the predicate
exists; `sessions.repos` has a fundamentally different trust grade than
`github_pr_sessions.repo_full_name` (established by a verified webhook payload), and no safe
`ScopeFromSession` exists without the predicate. **The arch-decisions block on the webhook path
is NOT blocked**: its repository identity descends from the verified payload, which is precisely
why it is the flagship first consumer (§31.6).

### 31.5 The index and the ports

**Ship mode B without pgvector — the decisive fact is quantitative, and it falls out of the
isolation constraint.** The §12.1 objection to a vector extension is real for one specific
shipping choice: `applyMigrations` (`cmd/control-plane/main.go`) unrolls the whole embedded
migration chain at every boot, and a `CREATE EXTENSION vector` in a numbered migration would
fail the boot of every pure-mode-A deployment whose Postgres lacks the pgvector library (the
official images — including the `postgres:17-alpine` this repo standardizes on — do not ship
it; `IF NOT EXISTS` does not help when the extension is not *available* on the server). But the
stronger fact: **strict per-repository isolation caps every search at one repository's corpus —
and the calibration below is reconciled against §31's own volumetry, not against an
understated baseline.** An earlier draft calibrated "hundreds to ~2,000 chunks" against a figure
disconnected from the intro's own sizing; corrected: the intro's ~15–50+ decisions/repo/week (at
the illustrative 300-PRs/week case) implies ~780–2,600+ `arch-decision` concepts in a repository's
first year alone, and hot repositories — 100–500 PRs/week, this section's own stated range —
scale roughly proportionally to ~1,300–4,300+/year at the top of that range. So a repository at
the high-volume end mode B exists to serve can clear ~2,000 chunks within its first year, not at
a distant horizon. The latency claim survives the correction regardless: an exact cosine scan
over `real[]` rows is linear in row count at a small per-row cost (a few hundred `float32`s each),
so single-digit-to-low-double-digit milliseconds holds through the low tens of thousands of rows,
not merely at ~2,000 — an HNSW index at that cardinality is still pure overhead, and pgvector
without an ANN index performs the same sequential scan anyway. So: embeddings stored as `real[]`
in the ordinary migration chain, exact per-repo cosine in the control plane, zero extensions
beyond the existing pgcrypto (migration 000001), **zero changes to the 28
`tcpostgres.Run(ctx, "postgres:17-alpine", …)` test files or to `docker-compose.dev.yml`**, §12.1
preserved verbatim, no schema-authority split. This is still, fully, "embeddings + RAG" in the
sense of the owner's decision — only the storage primitive changes. pgvector is documented as the
**named upgrade path**, activated per mode with the §31.2 activation-time preflight, with a
concrete trigger: a per-repository corpus exceeding ~20,000 chunks. Restated honestly against the
corrected steady state above rather than the retracted ~2,000-chunk baseline: **~20,000 is a
~4–5 year runway for a sustained hot repository** (~4,300/year at the top of the stated range),
not the "10× margin... distant horizon" an earlier draft claimed from the wrong denominator — a
real multi-year margin, stated as one. No retention window is introduced on this basis alone; if
a future repository's growth outpaces this, the trigger is the forcing function to revisit it, not
a silently-outgrown assumption. Full volumetry, so the numbers are on the record:
worst-case fleet-wide storage ~6 GB, typical ~0.6 GB; initial embedding of one repository
worst-case ~500k tokens ≈ cents to low dollars; aggregate query load ~0.025 QPS — nothing, for
Postgres.

**The `Embeddings` port — separate from `LLM`, never merged.** `internal/app/ports/llm.go` is
explicitly a structured-completion port; folding embeddings in would force a degenerate shape,
and the provider sets are disjoint in practice — a merged interface would violate §11's
"an interface must hold for more than one implementation" rule in the worst way. Shape
(`internal/app/ports/embeddings.go`): `Embed(ctx, EmbeddingRequest) (EmbeddingResponse, error)`;
request = {explicit Provider, Model, ordered `Inputs []string` batch, InputKind
document|query}; response = {`Vectors [][]float32`, `Dimensions`, Usage, `CostUSD *float64`
computed by the adapter, nil if unknown — the `CompletionResponse` contract}. A typed
`*EmbeddingsError` mirroring `LLMError` exactly: typed codes, never string-matching (§18.1's
rule). **The embeddings-specific correctness invariant**: vectors from different models or
dimensions are incomparable — the store records (model, dims) per corpus, rejects mixed writes,
and a model change forces an atomic re-embed of the corpus. **The model policy is decided:
pinned per deployment**; the per-corpus model registry and a background re-embed job are
deferred until a model migration is actually needed — the (model, dims) recording is what makes
that deferral safe rather than a trap. **Adapters**: one hosted vendor
(`internal/adapters/outbound/embeddings/<vendor>`) ships first; the
**adapter for the de-facto-standard embeddings wire format — one adapter covering both a second
hosted vendor and the local inference servers that speak the same wire, and therefore the mode-B
path for air-gapped self-hosters — is deferred**, but the
port is designed so it slots in without any interface change (that is what the explicit
Provider field and the adapter-computed CostUSD are for), keeping §11's two-adapter rule
satisfiable on the port's existing shape the day it lands.

**The lexical leg, fusion, and degradation.**

- **Lexical leg: core-Postgres `tsvector`** — a generated column + GIN index in a normal
  numbered migration, working unchanged in all 28 test containers. `'simple'` config: corpus
  text is prose dense with code identifiers, and English stemming mutilates them. `pg_trgm`
  (contrib, present in the image) is deferred until a recall deficit on identifiers is actually
  demonstrated. **One property named so no future Step mis-credits this leg: lexical retrieval
  is a recall instrument, never a poisoning mitigation.** `tsvector` ranks on the stored text,
  and the stored text is authored by the model after reading PR content an attacker controls end
  to end (§31.7's write path) — an attacker who stuffs a fabricated `ArchDecision` with the
  right identifiers is ranked up by the very identifiers he chose, the same "determines its own
  retrieval" property §31.6 names for embeddings. The two legs differ in recall behavior, not in
  trust class — which is why **both legs run only inside the §31.6 gate**: membership in the
  candidate set is decided by the server-derived key before either leg scores a row.
- **RRF fusion in the DOMAIN** (`internal/domain/knowledge/fuse.go`): pure arithmetic over two
  ranked lists (score = Σ 1/(k+rank), k=60), zero I/O, table-testable with no database — exactly
  what §11 wants in a domain package. The retrieval service (app layer) orchestrates: **the
  §31.6 gate first** — the server-derived candidate set, the same membership rule mode A orders
  by recency — then the lexical leg via the store and the vector leg via `Embeddings` + cosine,
  both scored over that gated set only, `FuseRRF`, top-N fetch with provenance, then the
  sanitizing render boundary (§31.7 G2). The fusion decides order, never membership: hybrid
  retrieval improves recall within the candidate set, and that is the whole of its job.
- **Provider-unavailable degradation**: mode B imports an availability dependency mode A does
  not have — embedding the *query* at review time. Copying the degrade-to-empty idiom would
  produce LESS context than mode A would have gotten from purely local data — a silent loss.
  Rule: degrade to **the lexical leg alone** (provider-independent; RRF over one empty leg is
  well defined; still inside the gate) or to the mode A SELECT — **never to empty** — and stamp the degradation on the
  turn (§31.2 item 4's record), without which degraded reviews contaminate the B arm of the KPI.
- **§26.7 accounting**: control-plane embedding spend is invisible to the sandbox-side
  accumulator (`turnState.spentUSD` — §26.7's own mechanism) and immaterial in dollars
  (~$0.0002/query against $0.50/$5 ceilings), but it must have a displayed home: a per-repository
  counter in the operator view, §30's surfaced-not-suppressed posture. It is deliberately NOT
  wired into the review turn's own budget check — the loopback endpoint's semantics
  (`ShouldSkipOptionalPass` over the turn's own spend) stay untouched.

### 31.6 The consumers — each with its mode, its first production consumer, and its measurement

Every mechanism below ships with its first production consumer and its measurement in the SAME
Step (the rule Steps 70 and 71 exist to enforce, after this project twice shipped tested code
with zero production callers).

**1. The "prior architecture decisions" block — BOTH MODES. The flagship.** All three review-turn
producers already prepend context blocks before `review.RenderTurnPrompt` in the impure-fetch /
pure-render idiom (`internal/adapters/inbound/github/handler.go`;
`internal/adapters/inbound/httpapi/reviewretrigger.go`;
`internal/app/sessionactor/reviewretrigger.go`'s `composeAutoRetriggerPrompt`). A
`reviewcontext.FetchPriorArchDecisions`, exact sibling of `FetchFalsePositivePatterns`, slots
into all three seams with near-zero integration risk. Mode B swaps the *ranker*, never the gate:
both modes draw their candidates from the same server-derived selector below — mode A orders
them by recency, mode B re-ranks the same candidates by hybrid retrieval (gate-then-rank,
§31.5); everything else — candidate eligibility, render, sanitization, fixed delimiter,
measurement — is shared. Why
the low-volume intuition inverts at scale: at a few PRs/week, a 30-decision window covers months
and recency approximates relevance; at hundreds of PRs/week the same window covers **days** of
churn — the decision a payments-lane PR needs is four months and thousands of decisions back.

**The deterministic path-scoped selector — mode A's whole SELECT, and the candidate GATE both
modes share (the fork is resolved: build the selector first). Its key must be server-derived, and `blast_radius` is not — corrected here
because an earlier draft got this backwards and the error falsified the poisoning argument below.**
`review_verdicts.blast_radius` (JSONB, migration 000067) is not server-derived metadata: it is the
reviewing model's own self-report, forwarded verbatim. `internal/domain/review/verdict.go`'s
`Verdict` struct is exactly the posted verdict: `RiskLevel` and `Premise`, immediately above
`BlastRadius` in the same struct, are documented in so many words as "the reviewer's own"
assessment, and §21.2's own text confirms `BlastRadius` is populated the identical way — "The
reviewing model reports a file count and a blast radius alongside its verdict." Nothing in
`internal/app/reviewverdict/insert.go` re-derives it: the field is
marshaled straight into the row (`marshalTags(verdict.BlastRadius)`); and the only server-side
check anywhere in the write path is a vocabulary-membership test
(`reviewpost.ValidateVerdictInput`'s loop over `in.BlastRadius`, rejecting anything outside the
eight-tag enum via `ErrInvalidBlastRadiusTag`) — nothing compares it to the diff. §21.2 already
states the rule this triggers, in terms: *"These last two are computed from the SERVER's own view
of the diff — never from the verdict… both are display data and neither may gate anything"* —
written after Step 62's first implementation made exactly this mistake for the auto-approval
eligibility engine. Keying the selector on `blast_radius` would repeat it one section later: an
attacker who induces a false `ArchDecision` can, in the same verdict call, induce whatever
`blast_radius` tags they like, including the ones that later decide where the false decision
surfaces — precisely the "determines its own retrieval" property this paragraph itself
attributes, below, to ungated similarity retrieval, the composition this section rejects.

**The selector is not dead — its key is wrong, and the fix already exists in the tree.**
`autoapproval.ClassifyChangedPaths` (`internal/domain/autoapproval/blastradius.go`, package
`autoapproval` — not, as an earlier draft mis-cited, `internal/domain/reviewtriage/sensitiveglob.go`;
that file is package `reviewtriage` and holds `sensitiveTags`, documented in its own comment as
"a DIFFERENT, smaller set" built for §26.3's triage question, a genuine but separate mechanism)
derives the *same* eight-value `review.Tag` vocabulary `blast_radius` carries, but
**deterministically from the PR's own server-fetched changed-file paths** — the identical
admissible input §21.2 already names for the auto-approval engine — never from anything the model
posts. `prCtx.ChangedPaths` is computed at all three review seams by the same adversarially
hardened deterministic parser (`internal/app/reviewcontext/fetch.go`'s `Fetch`, via
`reviewtriage.ExtractChangedPaths`); path-scoped injection over it is an already-shipped idiom
(`FetchAlreadyAnswered` takes `ChangedPaths` at all three sites). Selector v1 = one sqlc query:
decisions from verdicts whose **tags/directory-roots — stamped at INSERT time from
`ClassifyChangedPaths(prCtx.ChangedPaths)`, never from the posted `blast_radius` column** —
overlap the current PR's own freshly computed tags/roots (the 000083 write-time precedent;
`turns.review_depth_decision`'s own already-stored "distinct roots" is the shape to follow) —
`ORDER BY created_at DESC LIMIT k`, recency fallback when overlap is empty. **That overlap
predicate — including its recency fallback — is the GATE, and it is mode-invariant.** In mode A
it is the whole selector: gate, then recency ordering, take k. In mode B the identical
membership rule runs against the corpus rows' own copied INSERT-time tags/roots (§31.3's filter
columns — never re-derived at ingestion), and lexical+dense RRF then orders the gated candidates
(§31.5): gate, then rank, take N. When the fallback fires in mode B it serves the recency window
recency-ordered — a pure-recency set gives a ranker nothing legitimate to exploit, and one
shared fallback path is one fewer divergence between the arms — and the injected-ids record's
selector field (below) records which membership rule actually ran, in both modes. At 300 PRs/week,
path-scoped windows reach back **weeks to months** for typical roots, against 2–3 days for raw
recency: the motivating case is reached deterministically. Its honestly-reported strengths,
corrected: zero new ports, zero new vendors, zero new egress channel for customer-derived content;
and, now that the key is genuinely server-derived, a real poisoning-profile advantage — under
ungated similarity retrieval an attacker's prose **determines its own retrieval** (write a "decision" that
embeds near auth-shaped queries and it surfaces indefinitely), while under
`ClassifyChangedPaths`-keyed deterministic selection the key is a fixed function of the paths the
PR actually had to touch to make its change at all — real, reviewer-visible diff content, checked
server-side, never a free-form self-report an attacker can set to anything — and recency decay
quarantines automatically. **The gate-then-rank composition extends exactly that property to
mode B**: because the gate decides membership in both modes, an attacker's prose no longer
decides *whether* his content is a candidate anywhere in this section — only its rank among
candidates a key he does not control already admitted (§31.7 states precisely what that
narrowing buys and what it does not; the ungated-similarity failure just described is now a
description of the rejected composition, not of mode B). Plus a real quality doubt in the other
direction: the corpus is micro-records of 50–150 identifier-dense tokens — terrain where dense
retrieval is notoriously weakest and path overlap is ground truth, not a proxy.

**The cost of the gate, stated plainly rather than buried: gating trades recall for poisoning
resistance.** If the gate excludes a genuinely relevant decision — the cross-cutting
architectural call recorded by a PR that shares no paths, and hence no tags or roots, with the
current one — no ranker recovers it: in this composition the rankers order candidates, they
cannot admit them. And that excluded case is a first cousin of the case this section's own
volume argument invokes to justify retrieval existing at all — the decision from months back
that a recency window misses. A path gate can miss the same decision for a different reason: not
because it is old, but because it is orthogonal. The resolution is a stated position, not a
hedge: **the gate is strict, in both modes, and the recall loss is accepted and measured.**
Three candidate resolutions were weighed, so a reader who disagrees can see where to push:

- *Gate mode B only, mode A unchanged* — rejected as a non-position: mode A's selector already
  IS the gate (above), so this is not a smaller change but the same change plus an eligibility
  disagreement between the modes, which is precisely the two-variables-at-once contamination the
  composition exists to remove from the A/B readout (the measurement note below).
- *A bounded escape hatch* — admit the top-N globally-ranked candidates from outside the gate
  under a distinct, visibly-lower G6 provenance tier, prompt-framed as weaker — rejected **for
  now**, because it re-opens, for those N slots, exactly the attacker-steered membership channel
  the gate closes: §31.7 is explicit that prompt framing bounds an attack rather than killing
  it, so the hatch would spend real poisoning surface on a recall deficit nobody has yet
  demonstrated. It is the named escalation, not a component.
- *The strict gate, chosen*: the recall loss is real, and the instrument to see it is already
  ordered — Step 107's baseline readout records the recency-fallback rate on empty overlap and
  whether contestations cluster where path overlap was thin, which is the signature of a gate
  miss. Stated honestly about its own resolution: the §26.5 instrument observes a miss only
  through its downstream damage (a recap contested where the gate had little to offer) — a
  proxy, not a direct recall measure, recorded as such. If that readout demonstrates a deficit
  the gate cannot close, the bounded lower-tier escape hatch above is the named follow-up, on
  the same evidence-first terms as `pg_trgm` and pgvector (§31.5) — armed by measurement, never
  presumed.

**What this resolution does and does not defer — stated so the sequencing cannot be misread as
re-litigating the owner's decision.** Every line of the selector is structure mode B keeps, not
scaffolding it discards: same seam, same render, same sanitization, same injected-id recording,
same KPI — and, under gate-then-rank, the same candidate gate, which mode B re-ranks rather than
replaces. Building it as mode A's SELECT defers the **commitment** to the embeddings leg, not its construction — and it
arms the engagement decision with data instead of hypothesis. Precisely, because the two halves
of the comparison do not exist at the same time: Step 107's window establishes the deterministic
arm's own baseline on the §26.5 instrument — its contestation rate, the recency-fallback rate on
empty overlap, and whether contestations cluster where path overlap was thin (the injected-ids
record makes that attribution possible). The leg is engaged if that readout shows relevance
misses path scoping cannot close, and killed if the deterministic arm already sits at the
contestation floor; only once engaged does the full recency-vs-RRF A/B run — two rankers, one
gate — on mode-stamped verdicts from the first flip (Step 108's own measurement). The engagement gate
is written into Step 108's own row. And the design's own honest counterpoint, kept: "build
nothing" was already dead before this fork existed — `RejectedAlternative` is genuinely
irreplaceable, the checkout does not record the road not taken — so the fork was only ever about
*which retrieval*, never about *whether*.

*Measurement (same-Step rule)*: per-repository A/B on the already-shipped §26.5 contestation KPI
(migration 000086, the `arch recap wrong:` command), **joined on the verdict knowledge-mode
stamp** (§31.2 item 2). The gate-then-rank composition is what makes this A/B mean something:
the two arms share one candidate-eligibility rule and differ in exactly one variable — the
ranker — where the earlier alternatives framing compared arms differing in both candidate
selection *and* ranking, an unattributable readout. Two residual deltas are named rather than
wished away, both conservative (each only ever shrinks the B arm's candidate set relative to
A's) and both visible in the injected-ids record, so the readout can quantify them instead of
guessing: **G4 promotion** (a mode B candidate must additionally be merged + quarantine-aged +
uncontested — mode A's verdict-table SELECT cannot apply the merge half to rows predating
Step 107's capture) and **the ingestion watermark** (a corpus row lags its source verdict).
Ranker-only up to those two named deltas, not beyond — the claim is a materially cleaner
readout, never a laboratory-pure one. Plus a **durable record of the injected knowledge** — ids and content
hashes of the injected decisions, in a JSONB column on the turn, the exact shape of
`review_depth_decision` (migration 000083), carrying the selector actually used and any
degradation. Not logs: logs are not durable joinable state, and without this record, when a
maintainer contests a recap, no trace survives of which corpus rows induced it (the events
stream is `ON DELETE CASCADE`) — the anti-poisoning loop would be a fiction. Corollary, per
§31.3's barrier: already-contested decisions are excluded from retrieval.

**2. `kb_search` — MODE B ONLY. Blocked behind Step 106's entitlement.** A pull tool for
builder/plan sessions, on the verdict-tool precedent (a control-plane endpoint whose
URL/bearer/gen are substituted into the prompt at hand-off,
`cmd/sandbox-agent/reviewverdicttoolprompt.go`) — not the loopback precedent: the corpus lives in
the control plane's Postgres, unreachable from the sandbox any other way. Pull beats push at high
volume: builder turns are the most voluminous class and need nothing most of the time; pull costs
only when the agent decides, with a query formed mid-task after exploring the checkout.
**`kb_search` is ungated by construction — stated so the item 1 gate is never mis-credited as
covering this surface.** A pull query has no PR behind it: there are no server-derived
`ChangedPaths` to gate on, and the query text is agent-authored. Its poisoning posture rests on
G2's sanitizing render, G4's eligibility, and G5/G6's capped, weighted provenance alone (§31.7)
— which is a reason the entry barrier and provenance weighting are corpus properties rather than
review-path properties, and one more reason this tool stays blocked behind Step 106. Scope is
derived server-side from the session row **and verified against the entitlement predicate**
(§31.4 — derivation alone is laundering); authorization is against the turn's stamped mode
(§31.2 item 3). *Measurement*: query/hit-rate/injected-ids journal, zero-result rate.

**3. Approved-plan durability — an orthogonal prerequisite, owed anyway.** The cascade defect
exists independently of any corpus (§31.3); the Step ships regardless of every other decision
here. *Consumer*: the existing approval path writes the snapshot. *Measurement*: coverage —
every post-Step approval has a `plan_documents` row.

**4. False-positive patterns — mode A forever, zero code.** The deliverable is the per-source
doctrine written down (§31.1). The advisory renderer's structural defense survives because it
implicitly assumed a human-bounded source, and that bound is structural (RBAC + explicit command
+ mandatory retirement).

**5. Similarity over already-answered facts — rejected, both modes, reaffirmed.** §31.1's last
paragraph; recorded here so the consumer list is exhaustive and the rejection is part of it.

**6. The OKF read-only export — decided yes, as a small late read-only Step, after Steps 106 and
108** (106 for the entitlement gate; 108 because this renders from that Step's own corpus tables —
Step 110's own Content already requires it, and §31.10's phasing is corrected to match). An
authenticated, entitlement-gated endpoint rendering a repository's active concepts — `arch-decision`,
the only type Phase 9's corpus admits, §31.3 — on demand (§31.3's residence argument). Never a
write path, never the retrieval substrate, never written into any customer repository.
*Consumer*: the operator/maintainer download surface itself. *Measurement*: access is
`audit_log`-journaled per export (customer-derived prose leaves the database as one document —
that is an event worth a row), plus a served-count metric.

### 31.7 Poisoning and injection: what is guaranteed structurally, and what is not

The adversarial finding, stated flatly: **the write path persists digest fields verbatim and
unsanitized, and the anti-poisoning link the grounded pass leaned on does not connect.**

**The write path.** Digest fields are free prose written by the model after reading PR content
that is entirely attacker-controlled (`internal/domain/review/sanitize.go`'s own words about
diff/title/body). `internal/app/reviewverdict/insert.go` marshals and persists verbatim; the
only barrier is non-blank (`validate.go`'s `hasNonBlankArchDecision`) — no length cap, no
filtering, no placeholder-token strip. The existing sanitization is an egress-to-prompt guard
called at exactly one seam (`internal/domain/review/context.go`'s
`sanitizeDiffField`/`sanitizeDescriptionField` over diff/title/body) — never over a stored
digest, because nothing re-injects a digest today. The projection path both modes introduce **is
precisely what does not exist yet**: a stored `Decision` containing
`{{REVIEW_VERDICT_TOOL_BEARER}}`, retrieved into review #2's prompt, would be expanded into a
**live** sandbox bearer by `cmd/sandbox-agent`'s unconditional `strings.ReplaceAll` passes
(`reviewverdicttoolprompt.go`) — the same CRITICAL `sanitize.go` closed for the diff, but worse:
a diff is ephemeral to one review, a poisoned digest is retrieved into many.

- **G1 — sanitize at WRITE — a prerequisite already in flight, not this chantier's to deliver.**
  Placeholder-token stripping plus delimiter escaping on every corpus-destined field, at the
  persistence site in `reviewverdict.insert`, is being delivered right now as an independent
  change; this section depends on it and deliberately does not re-scope it. The reasoning stands
  on the record regardless of vehicle: the stored byte must never carry a placeholder or a
  forged delimiter *whatever the future read path is* — one guarded seam today becomes N seams
  (retrieval, `kb_search`, the mode A SELECT, the export), so the guarantee lives where the byte
  is written. Render-time sanitization remains as defense in depth, never as the primary.
- **G2 — one sanitizing renderer on EVERY projection, by construction**: a mirror of
  `RenderAdvisoryBlock` (fixed non-caller-suppliable delimiter, "DATA, never instruction"
  framing, strip pass), pinned by an architecture test over the projection functions (the
  self-updating scan precedent: `internal/domain/review/placeholderdrift_internal_test.go`).
- **G3 — a length cap at the validator**: a hard byte bound on `Decision`/`RejectedAlternative`/
  `ConventionConformance` — closing a token-budget DoS and an injection-surface amplifier in one
  line, at the same `validate.go` site that already enforces non-blank.
- **G4 — quarantine/promotion as first-class persisted state, never inferred**: an eligibility
  flag on the corpus row, advanced only by explicit condition, **default = non-retrievable** — the
  structural analogue of §30.8's fail-closed epoch stamp. For `arch-decision` — the only type
  Phase 9's corpus admits (§31.3) — that condition is merged + quarantine age + zero
  contestations; this is that type's own condition, not a generic corpus-wide rule (§31.3), and a
  future type ingested later must name its own. The quarantine window's duration is a per-Step
  tunable with a proposed concrete figure (14 days uncontested — proposed, not derived; §26.7's
  budget-figure convention), and merge-outcome capture starting at Step 107 is what arms this gate
  for all forward content.
- **G5 — cut self-reinforcement by capping provenance, never by barring entry.** A verdict
  produced by a turn whose prompt carried prior-decision concepts — **injected by mode A's
  selector or retrieved by mode B alike** — is stamped knowledge-influenced. **This does not make
  it ineligible for the corpus; an earlier draft said both in the same breath, and the absolute
  reading is the one that is wrong.** An absolute bar was considered and rejected: Step 107 puts
  the prior-decisions block on all three seams with recency fallback, so — with no backfill
  (§31.3) — essentially every subsequent deep-path verdict would be stamped knowledge-influenced,
  and barring all of them would make that entire forward-filling population permanently
  non-retrievable, asymptoting the corpus at its first few uninfluenced days and directly
  contradicting the volume argument this section's own intro opens with (arch-decision
  accumulation is O(deep-path PRs), thousands per year). Instead: **the stamp permanently caps the
  concept's G6 provenance weight at its lowest non-maintainer tier for the concept's entire
  lifetime, and it may never satisfy `agent-authored, uncontested` authority — whatever G4's
  ordinary eligibility gate later resolves for it.** A knowledge-influenced decision can still
  earn ordinary eligibility (merged + quarantine + uncontested) and be retrieved; it can simply
  never present at full authority, so a poisoned convention cannot compound into stronger
  "authentic" backing across cycles — review #2's own re-assertion is itself capped, not merely
  its parent. Without the stamp at all, provenance laundering is automatic regardless of which
  form is chosen: the poisoned concept biases review #2, whose digest re-asserts the false
  convention with now-authentic authorship, cycle after cycle — and the loop needs no retrieval:
  the mode A block closes it too, which is why the stamp is not scoped to mode B. Nothing in the
  current schema records this influence, and it must not be inferred later by joining the
  injected-ids record (G4's first-class-state lesson applies to it): **the stamp ships at Step 107
  with the mode buffer** — injection starts there, so no unstamped-but-influenced verdict ever
  exists in the population Step 108 ingests — and Step 108's ingestion applies the provenance cap.
  The §31.6 injected-ids record is its evidence trail, never its substitute.
- **G6 — provenance travels with every chunk and weights it**: source PR, author class,
  merged/open, live/shadow epoch, review path, and G5's permanent knowledge-influenced cap
  (above) as one more weighted axis, not a separate mechanism. Agent-authored-from-a-low-confidence-PR
  content is structurally distinguishable from maintainer-taught, and the prompt framing forbids
  treating the former — or anything G5 has capped — as authority.

**The contestation hash is NOT an anti-poisoning control — recorded so no future Step credits it
as one.** `ArchRecapText` (`internal/domain/reviewpost/digestsectionidentity.go`) canonicalizes
and hashes **a verdict's entire recap**; the corpus chunks **per decision** — the
"retract the contested concept by content hash" join the grounded pass assumed structurally
matches nothing. Compounding: the hash's scope is per-verdict (the same poisoned decision
re-emitted in PR #2's verdict hashes differently), it is paraphrase-fragile (documented in the
file itself), and contest text is itself untrusted PR-thread content (a griefing vector for
retracting a legitimate decision). The §26.5 contestation mechanism is good recap-quality UX and
this section's KPI instrument; it is not a retraction mechanism for the corpus, and G4's
per-decision eligibility state exists precisely because it cannot be.

**What the §31.6 gate buys here, precisely — and what it does not.** In both modes the attacker
no longer decides *whether* his content is a candidate by anything he writes: membership is the
overlap of two server-derived keys — the tags/roots stamped from the paths his poisoning PR
verifiably touched, and the victim PR's own freshly computed tags/roots — and neither is
free-form text. What he retains, named so nobody rounds this up to a kill: he can still *aim*,
because his own PR's paths are his to choose — but the aim is purchased with real,
reviewer-visible diff content in a PR that must merge and survive G4's quarantine uncontested,
and it reaches only future PRs that genuinely work in the same terrain, never every PR whose
query his prose can embed near; and within any candidate set he is legitimately a member of, his
prose still steers both retrieval legs' scores (§31.5 names the lexical leg's share of this
explicitly — `tsvector` is not the safe half of the hybrid). So the gate narrows the surface —
from attacker-steered membership to attacker-influenced rank among server-selected candidates —
and shortens it (recency decay in mode A's ordering, G4's quarantine in both). It does not close
it: the semantic false precedent below survives inside the gate wholly intact, which is why G4,
G5, and G6 remain necessary, not superseded.

**The attack with no clean structural kill, said honestly: the semantic false precedent.** A PR
whose narrative induces the model to record an `ArchDecision` asserting a false convention — in
well-formed English, no tokens, no delimiters — survives every strip pass by construction.
**G4, G5, and G6 ARE the defense**: G4's eligibility gate keeps unmerged and contested content out, the
influence stamp prevents the false precedent from laundering itself into authentic authorship,
and weighted provenance keeps a single low-confidence assertion from outranking maintainer-taught
truth. They are structural and they ship with the mechanism in the same Step — but they bound
and decay the attack rather than killing it, and this section says so rather than implying
otherwise. (The §31.6 gate narrows the attacker further in BOTH modes — the paragraph above —
which is one of the reasons the fork resolved as it did, and why the gate precedes both rankers
rather than belonging to mode A alone.)

### 31.8 Interaction with platform shadow mode (§30)

- **Capture continues in shadow.** §30.1's guarantee is zero writes to CUSTOMER systems;
  internal Postgres writes are explicitly legitimate (§30.6), and the corpus accumulating during
  an evaluation is part of what the operator evaluates.
- **Epoch stamped at write, excluded in the query** (§30.8 verbatim) — with the stamp living
  where each read's rows live. Every knowledge corpus row (Step 108) carries a `captured_live`
  boolean stamped at write via Step 96's resolver, **spelled so the default resolves to
  "captured-in-shadow" — the quarantined direction** (`NOT NULL DEFAULT false`, the
  `live_egress_enabled` polarity). Mode A's SELECT reads `review_verdicts` directly and needs no
  new stamp: it rides the egress-mode stamp §30.8 already puts on every verdict row (Step 98).
  Either way the rule is the same: every live read — mode B retrieval AND mode A's SELECT alike
  — excludes shadow-epoch rows **in the SQL, never at call sites**, the same in-query discipline
  §30.8 imposes on auto-merge candidacy.
- **Graduation at Activate**: shadow-epoch knowledge rows join §30.8's existing promotion
  barrier — the operator explicitly promotes (an `audit_log` entry) or purges; the default with
  no action is excluded-from-live, monotone toward quarantine. This extends Step 104's Activate
  gesture, never a second graduation surface.
- **Deactivation without graduation = purge**, structurally cheap (one DELETE per parent table,
  the composite FK cascades). Embeddings are derived customer content at rest — the same class
  as §30.9's retention concern. **The retention fork is resolved the same way here: a short
  quarantine window before deletion** (proposed: 7 days — operator-error recovery, weighed
  against customer-content-at-rest minimization), then hard delete.
- **The embedding egress is itself surfaced**: embedding an evaluated repository's content sends
  customer-derived text to the embeddings provider and burns spend — inherent to evaluating
  mode B at all, displayed in the shadow operator view beside LLM spend (§30.1's
  surfaced-not-suppressed posture). **The key fork is resolved: shadow runs use the
  evaluation org's embedding key, never the customer's** — consistent with §30's posture that
  the cost of evaluation is the evaluator's to see and to carry where it can be.

### 31.9 Residual limits, open verification items, and what stays deferred

**Residual limits, named:**

1. **The entitlement boundary is only as delivered.** Until Step 106 lands, the four isolation
   layers hold "one query, one repo" but not "the right repo for this caller"; `kb_search` and
   the export stay blocked, and the webhook-path consumer is the only one running. The
   in-flight four-handler fix closes today's known leaks; it does not create the predicate.
2. **The semantic false precedent has no structural kill** (§31.7) — bounded and decayed by
   G4/G5/G6, not eliminated. Anyone extending the corpus to new content types re-inherits this
   limit and must re-argue it.
3. **The gate's recall loss is a real, accepted trade** (§31.6): a cross-cutting decision
   sharing no server-derived tags/roots with the current PR is invisible to both modes — the
   rankers order candidates, they cannot admit them. Accepted strict, measured through the
   gate-miss signature on the §26.5 instrument (a proxy through downstream damage, recorded as
   such — §31.6), with the bounded lower-tier escape hatch the named-and-deferred escalation if
   the deficit is ever demonstrated.
4. **`repo_full_name` as the scope key**: a repository rename/transfer silently splits a corpus
   that outlives any PR session. Consistency with `repo_settings` and `github_pr_sessions`
   argues for keeping it and inheriting any future rename fix those tables get — accepted, and
   noted rather than hidden.
5. **A new egress channel exists in mode B** — the embeddings provider receives customer-derived
   text. Surfaced (§31.8), key policy decided for shadow; the wire-compatible adapter (§31.5)
   is the named path to closing it entirely for self-hosters, and it is deferred, not dropped.
6. **No browsable corpus tree, no git history of knowledge evolution, no edit-review workflow**
   — the deliberate price of Postgres-as-truth (§31.3); the read-only export is the answer to
   the navigation half, and the other halves are out of scope until someone actually asks.
7. **Pre-durability plan loss is permanent** (§31.3) — recorded so nobody later files it as a
   backfill bug.

**Open verification items** (must be resolved before the relevant layer is credited):

1. **RLS role reality**: whether the application role owns the knowledge tables in the real
   deployments — if yes, `FORCE ROW LEVEL SECURITY` is required or layer 4 is decorative
   (§31.4).
2. **Embedding-provider batch limits and input truncation behavior** for the chosen vendor at
   the §31.3 chunk sizes — assumed unremarkable, verified at adapter time with a contract test.

**Deliberately deferred, each surfacing at its Step rather than silently defaulted:**

- **The embeddings-leg engagement readout** (§31.6 item 1): Step 108 does not start until
  Step 107's deterministic-arm baseline window has been read; the criterion is written on
  Step 108's row. Under gate-then-rank the eventual A/B is a ranker-only comparison over a
  shared candidate rule — up to §31.6's two named residual deltas — which is what makes the
  readout able to attribute a difference to the embeddings leg at all. **What is NOT deferred: the kill branch's own deliverable, stated explicitly so
  it is never confused with the engaged branch's.** If the readout kills engagement, Step 108 is
  never executed — its entire deliverable becomes a recorded kill decision citing Step 107's
  baseline readout (contestation rate, recency-fallback rate, whether contestations cluster where
  path overlap was thin), and Steps 109-110 (both of which need Step 108's corpus tables) never
  ship either. Phase 9's exit (§10) and the implementation plan's own milestone and Verification
  entries are each written as a disjunction — mode A alone plus this recorded kill decision
  satisfies them exactly as fully as a mode B flip does — precisely so none of those four places
  silently requires the engaged branch as if it were the only outcome this readout could produce.
- **Approved-plan corpus ingestion** (§31.3) — Step 105 makes the prose durable; rendering it as an
  OKF concept, chunking it, embedding it, and serving it through `kb_search`/the export is a
  later Step's scope, not named in Phase 9.
- **Per-corpus embedding-model registry + background re-embed job** — deferred until a model
  migration is actually needed; the (model, dims) recording (§31.5) is the enabling move taken
  now.
- **The wire-compatible embeddings adapter** (self-host/air-gapped mode B) — deferred; the port
  shape already accommodates it.
- **pgvector** — the named upgrade path, trigger ~20,000 chunks per repository (§31.5).
- **`pg_trgm` on the lexical leg** — deferred until an identifier-recall deficit is demonstrated
  (§31.5).

### 31.10 Phasing

Phase 9, Steps 105–110 (implementation plan). The minimal subset delivering the owner's decision
is Steps 105 + 107 (durability, and the mode A pipeline with its buffer and measurement); Step 107
alone is already production-useful — mode A gains its cross-PR memory. **Step 108 is conditional,
not minimal**: it ships — the mode B index/retrieval whose *ranker* swaps into Step 107's proven
pipeline, behind the candidate gate Step 107 built and both modes keep (§31.6) —
only if Step 107's own baseline readout (§31.6, §31.9) engages the embeddings leg; if that readout
kills engagement, Step 108's entire deliverable is a recorded kill decision, and Phase 9 closes on
mode A alone. Step 106 (entitlement) runs in parallel and gates Steps 109 (`kb_search`) and 110
(export); both additionally wait on Step 108 actually shipping — they render/query Step 108's own
corpus tables, so neither exists at all under a kill decision. Two prerequisites are in flight as
independent changes and are dependencies, not Steps: the four-handler repository-authorization fix
and G1's write-path sanitization (§31.4, §31.7).

## 32. Cohort rollout of sessions, and rollback

Step 76 (§10 Phase 6). Problem this solves: before cohort rollout, `NARVI_STAGE=production` means
every repository named on any incoming session-creation request gets a real sandbox the instant a
request reaches the control plane — REST, Slack, Linear, or a GitHub `@mention`. That is the
correct steady-state behavior, but it is the wrong FIRST behavior for a production deployment: an
operator needs to bring customer repositories onto the platform one at a time, and needs a real,
tested way to take one back off again if something goes wrong. This section specifies that gate —
two independent layers, both fail-closed — and is equally explicit about the one thing it does
NOT do: sever a turn that is already running.

### 32.1 The creation-surface invariant

Every `sqlcgen.CreateSessionParams` insert in this codebase — grepped, not assumed — is built in
exactly one place: `CreateSessionOnTx` (`internal/adapters/inbound/httpapi/create.go`). Nothing
else constructs one. Seven paths reach it, six of them live in production:

| Entry point | Call site |
|---|---|
| REST `POST /api/sessions` | `httpapi/create.go` (`CreateSession` → `CreateSessionCore`) |
| GitHub PR-mention ingress | `github/coalesce.go` (`CreateOrJoin`'s WINNER path, inline on its own claim tx) |
| Slack ingress | `slack/handler.go` (`resolveOrClaimSession` → `CreateSessionCore`) |
| Linear agent-session ingress | `linear/webhook.go` (`handleCreated` → `CreateSessionCore`) |
| Automation fan-out | `app/automation/fanout.go` (`createRunAndSession`, inline) |
| Sentinel-auto-fix child sessions | `app/outboxworker/sentinelautofix.go` (`spawnClaimedChildSession`, inline) |
| `bot.go`'s `CreateSessionForBot` / `childsession.go`'s `SpawnChildSession` | exported, no production caller today |

Two shapes feed `CreateSessionOnTx`: a direct inline call on a transaction the caller already
holds a different lock on (GitHub, sentinel-autofix, automation fan-out — each avoids opening a
second pooled connection while the first is still held, see each call site's own doc comment), or
`CreateSessionCore`, a thin `pool.Begin`/commit wrapper four callers share (REST, Slack, Linear,
and the two currently-unused bot/child-session entry points).

**Normative invariant**: `CreateSessionOnTx` takes `rolloutMode platform.RolloutMode` and
`repoSettings *postgres.RepoSettingsStore` as REQUIRED, non-variadic parameters — not folded into
`ChildSessionOptions`, this function's own existing trailing-variadic slot for genuinely optional
extras. A new session-creation path that does not thread both **does not compile**. This is a
deliberate departure from this codebase's own variadic-options convention for exactly this
function (`ChildSessionOptions` sits right next to it): an omittable gate parameter is an omitted
gate, and a check every future call site must remember to add is not a guard — this project's
standing rule since Step 74's identical fail-closed-twice mechanism (§27.5/§27.6). The workflow
engine's own turn-insert path (`internal/app/workflowengine`) and review re-triggers on an
already-tracked PR (the REUSE branch, `coalesce.go`) never construct a session at all — they
enqueue *turns* onto one that already exists — so neither is a creation surface this gate needs to
cover; they are exactly why §32.4 exists as a further, independent set of dispatch-time re-checks.
The REUSE branch specifically enqueues a turn onto a session whose sandbox is typically already
`Ready`/`Suspect` — no fresh spawn/restore/resume in the picture at all — which is precisely the
path §32.4's own turn-dispatch re-check (not its spawn-time one) covers; see that section's own
two-part breakdown for why one re-check alone was not enough.

### 32.2 Two-layer flag semantics

**Layer 1 — the master switch.** `platform.Config.RolloutMode` (`NARVI_ROLLOUT_MODE`), one of
`open`/`cohort` (`internal/domain/rollout.Mode` — `platform.RolloutMode` is a genuine Go type
alias for it, not a second, parallel enum that could drift). Unset → `open`, validated fail-fast
at boot otherwise (`platform.InvalidRolloutModeError`, mirroring `NARVI_STAGE`'s own named-error
convention, §5.4/§11) — an operator who mistypes `NARVI_ROLLOUT_MODE=cohrot` gets a boot failure,
never a silent fallback to unenforced `open`. **`open` is a byte-for-byte no-op**: every call site
short-circuits before touching `repo_settings` or deriving a single owner/repo identity, so an
existing deployment (and CI, which never sets this variable) behaves identically to before this
Step landed, including for a repo with no `repo_settings` row at all.

**Layer 2 — per-repo enrollment.** `repo_settings.sessions_enabled` (migrations/
`000096_repo_settings_sessions_enabled.up.sql`), a further `BOOLEAN NOT NULL DEFAULT false`
column on the SAME shared `repo_settings` table five other admin-configured, per-repo policy
booleans already live on (migrations/`000044_repo_settings.up.sql`'s own "one shared table, not
one bespoke table per toggle" design) — Phase 8's own Step 96 plans its own further sibling column
there, so this lands as a sibling, never a rival. A column-scoped `RepoSettingsStore.
UpsertSessionsEnabled` follows the established per-column upsert convention (`UpsertAutoMergeToggle`
et al.) — touches only this one column, so a concurrent write to any other `repo_settings` column
can never race with it at the database level.

**The shared decision.** Both layers combine through exactly one pure function,
`internal/domain/rollout.Decide(mode, []RepoAdmission)` — no I/O, no clock, no randomness — mirroring
`internal/domain/environment.CheckSubstrateCapabilities`'s own "one pure function, two independent
call sites" shape from Step 74. `mode != ModeCohort` admits unconditionally without even inspecting
its second argument. Under `ModeCohort`, **every named repo must be enrolled** — a multi-repo session
refuses on the first unenrolled repo, in request order (stop-at-first-failure, mirroring
`validateCreateSessionRequest`'s own identical precedent). **Fail-closed** on every degraded read: an
absent `repo_settings` row, any other Postgres read error, or a repo whose clone URL cannot be
resolved to a trusted identity at all (§32.3) are all folded into `RepoAdmission.Enrolled == false`
before `Decide` ever runs — Step 62 finding C3 already established that widening policy on a degraded
read is backwards, and this gate is no exception. This is nearly free here specifically: the read
runs inside the SAME transaction that is about to insert the session two statements later, on the
SAME Postgres connection — there is no real state where `repo_settings` is unreadable but that
insert would have gone on to succeed.

### 32.3 Host verification against enrollment spoofing

`reposource.ParseOwnerRepo` is deliberately host-agnostic (its own doc comment: "it never inspects
rawURL's host at all") — `https://evil.example/acme/widgets.git` derives the identical
`acme/widgets` a genuine `https://github.com/acme/widgets.git` would. Both gate halves therefore
run `reposource.CheckRepoHost(url, ports.SupportedSourceControlHosts()...)` FIRST, exactly the
pairing six existing call sites in this codebase already establish (`app/sessionactor`'s
`pushpr.go`/`contractdrift.go`/`imageresolve.go`, `app/outboxworker`'s `sentinelautofix.go`,
`app/imagebuild`'s `builder.go`, and now this Step's own `rolloutgate.go`/`dispatch.go`). Without
this pairing, an attacker naming a foreign host that happens to reuse an enrolled repo's own
owner/repo path would be silently treated as that same enrolled repo. A URL that fails the host
check, or that parses but does not resolve to an `owner/repo` shape at all, is treated identically
to an unenrolled repo — refused, never distinguished by a different error shape a caller could use
to fingerprint which check failed.

### 32.4 The dispatch-time re-check: what makes rollback real

The creation-time gate alone would make rollback a lie. A GitHub PR review session, once created,
never returns to `CreateSessionOnTx` again — every subsequent `@mention` or label re-trigger on the
SAME PR rides the REUSE branch (`coalesce.go`), which enqueues a *turn*, not a session, and every
respawn/restore/resume of its sandbox — and every dispatch of that turn once a live sandbox already
exists — is driven entirely by `app/sessionactor`'s own dispatch loop (`planDispatch`, `dispatch.go`).
De-enrolling a repo must therefore stop TWO structurally distinct things that loop can do for an
existing session, not one — this is a two-part re-check, not a single one, and an earlier version of
this section only described the first part.

**Part 1 — re-spawn/restore/resume.** `dispatch.go`'s `tryPlanSpawn` re-checks, beside the
identically-shaped Step 74 substrate check (`refuseIfSubstrateUnsupported`):
`refuseIfRolloutUnenrolled`, gated to the exact same three action kinds that are about to attempt a
real provider call (`SpawnActionSpawn`/`Restore`/`Resume` — an ordinary `Skip`/`Wait` is not itself
reaching the provider, so it stays the same silent no-op it always was). It re-derives each of the
session's own named repos' owner/repo identity (§32.3's same host-check-then-parse pairing) and
re-reads `repo_settings.sessions_enabled` fresh, on the dispatch transaction, every single call —
never a cached verdict from session-creation time. This is `internal/app/sessionactor.Registry`'s
own `rolloutMode` field (threaded through `RegistryOptions`, not a required `NewRegistry` parameter
— see that field's own doc comment for why this one is narrower than `CreateSessionOnTx`'s
requirement: its zero value is indistinguishable from `rollout.ModeOpen`, the platform-wide safe
default, not a distinct, weaker-but-plausible disabled state the way an omitted
`*postgres.RepoSettingsStore` would be, since the store itself is already unconditionally available
on every Actor via the existing `storeBundle`).

**Part 2 — dispatch a turn to an already-live sandbox.** This is the REUSE branch's own ACTUAL
path, and an adversarial review of this Step found it entirely ungated in the version of this
section that shipped first: `planDispatch`'s branch (b) — a `Pending` turn and a `Ready`/`Suspect`
sandbox that needs no spawn/restore/resume at all — routes straight to `tryPlanDispatch`, which
never calls `tryPlanSpawn` or `refuseIfRolloutUnenrolled`; `planReenqueueOrRespawn`'s own branch
(b') (Step 28's turn-recovery re-enqueue, `tryPlanReenqueue`) has the identical shape and the
identical gap. Neither function is the right place to add the check piecemeal — a third,
future way of producing a dispatch plan could just as easily forget it. Instead the re-check lives
in `executeDispatch` (`rolloutRefusalForDispatch`): the ONE function every dispatch plan, from
EITHER `tryPlanDispatch` or `tryPlanReenqueue`, must pass through before
`SandboxCommander.SendCommand` is ever called (`handleEnsureDispatched`'s own switch has exactly one
consumer of a non-nil dispatch plan) — gating there makes the bypass structurally unrepresentable
rather than a discipline three call sites have to remember independently. Because this runs AFTER
the turn's own `Pending`→`Dispatched`→`Processing` transaction has already committed (this file's
own sequencing discipline: a real network call, including this rollout read, must never run inside
a transact holding the session row's `FOR UPDATE` lock), a refusal here cannot roll that commit
back — it fails the turn FORWARD instead, via the exact same `Processing`→`Failed` machinery a
genuine `SendCommand` failure already uses (`failDispatchedTurn`, `turn.TriggerTimeout`), with a
reason string naming the rollout refusal rather than a transport error. A refused turn therefore
reaches a real terminal state (`Failed`, a `never_started`-shaped completion, one synthetic
`execution_complete` event) rather than sitting `Pending` to be silently re-attempted on every
future `EnsureDispatched` round.

Both parts share the identical pure decision (`internal/domain/rollout.Decide`) and the same
`repo_settings` resolution logic (`rolloutDecisionForSession`) — only the connection scope (the
transaction about to write a spawn claim, vs. the actor's own pool, read outside any transaction
after the turn's own commit) and what happens on refusal (roll back an about-to-happen spawn vs.
fail an already-`Processing` turn forward) differ between them, so the two gates can never drift to
a different definition of "enrolled" from one another.

### 32.5 Per-channel refusal contract

`CreateSessionError` gained one field: `RolloutRefusal bool` — a machine-checkable marker distinct
from `Status`/`Message`, so a caller can tell a permanent policy refusal apart from a transient
failure structurally, never by string-matching `Message`. Three of the six live creation paths
used to route ANY `CreateSessionOnTx`/`CreateSessionCore` error down a retry-shaped path; that is
wrong for a refusal that will reproduce identically on every retry, forever, since
`repo_settings.sessions_enabled` does not change between redeliveries of the same event.

| Channel | Non-refusal error handling (unchanged) | `RolloutRefusal` handling |
|---|---|---|
| REST | 403 status + message, generic `writeError` path | Same generic path already produces the right shape — `checkRolloutGate`'s own message already reads "repository not enrolled: \<repo\>"; no special-casing needed. A transient read failure (below) reaches this SAME generic path too, with its own distinct 503 status and "repository enrollment could not be verified: \<repo\>" message — REST never branches on `RolloutRefusal` at all, so both shapes already flow through unmodified |
| Linear | Release the delivery claim, let Linear redeliver | Takes the SAME shape as an authz denial immediately above it in `handleCreated`: acknowledge, release only the `linear_agent_sessions` claim (not the webhook-delivery claim), post an honest in-thread notice, return `ok=true` (terminal) |
| Slack | Return `(sessionResolution{}, false)`, releasing the webhook-delivery claim for a retry | Mirrors `ackNotAuthorizedText`'s own terminal in-thread ack idiom: post `ackNotEnrolledText`, return `{Skip: true}, true` — the webhook-delivery claim is kept, never released |
| GitHub | `errors.Is(err, ErrActorNotAuthorized)`'s own generic branch: release the webhook-delivery claim | A distinct sentinel, `github.ErrRolloutNotEnrolled`. Takes the permanent-denial idiom: acknowledge (200) WITHOUT releasing the webhook-delivery claim, and post **no reply on the PR at all** — stricter than the unlinked-actor branch (which does reply): an unenrolled repo gives a commenter no action to take, so there is nothing honest to say beyond silence. The github_pr_sessions claim row itself never commits (the WINNER path's own transaction rolls back), which is also why §32.6 below is true |
| Sentinel-autofix outbox | Return the error; the outbox's own backoff/retry/dead-letter machinery retries | A sentinel, `errRolloutRefused`. `Deliver` maps it to the existing terminal-skip precedent (`descriptionautofix.go`'s "confirmed negative → nil, never retried"): logs a Warn and returns `nil` — `markFindingsFixPending` is deliberately never called, since no fix session exists to record against any finding |
| Automation fan-out | `createFailedRun` records a terminal `RunStatusFailed` row with `session_id` NULL | Unmodified — this path already does the structurally correct thing for ANY `CreateSessionOnTx` refusal, rollout or otherwise |

An unenrolled repo therefore produces **zero platform egress on GitHub** specifically (the one
channel where "egress" means a visible artifact on a customer's own repository) — no comment, no
label, no status check. Every other channel gets a private, honest acknowledgment instead of
silence, since a Slack thread or a Linear agent session is not customer-repository-visible the way
a PR comment is.

**Fail-closed and terminal are different properties.** `RolloutRefusal` is set — and a channel takes
the terminal handling in the table above — ONLY when a refusal is a genuine, DEMONSTRATED policy
outcome: mode is `cohort` and the named repo's own enrollment fact was actually read (an absent
row, an explicit `sessions_enabled=false` row, or a clone URL that could never resolve to a trusted
identity at all, §32.3 — none of these can ever produce a different answer on retry). A
`repo_settings` read that fails for an INFRASTRUCTURE reason instead — a context cancellation, a
query timeout, any other degraded-read condition — still refuses THIS attempt (fail-closed never
widens: the repo is never silently admitted), but `RolloutRefusal` stays `false`. An earlier version
of this gate conflated the two, so a momentary database blip during a refusal path came back
indistinguishable from "this repo is permanently not enrolled" — Linear acked terminally, Slack
posted a permanent denial, GitHub stayed silent while keeping its claim, and the sentinel-autofix
outbox skipped terminally, all four PERMANENTLY dropping legitimate work for a repo that may be
fully enrolled, with no retry ever attempted. With the two properties separated, a transient read
failure instead takes each channel's own **existing, ordinary non-refusal path** in the table
above — Linear/Slack/the sentinel-autofix outbox retry; GitHub releases its claim for a real
redelivery — so the work is picked up again once Postgres recovers.

### 32.6 Enrollment is seed-manifest-only in v1

The existing admin repo-settings REST writes (`PutRepoSettings` et al., `httpapi/reposettings.go`)
are gated by `confirmRepoKnown`, which requires an EXISTING `github_pr_sessions` row for that repo.
That row's only writer anywhere in this codebase is `github/coalesce.go`'s own webhook ingress, and
it commits ATOMICALLY alongside the session it claims a slot for (`EnsureRow` → `LockForUpdate` →
`CreateSessionOnTx` → `SetSessionID`, one transaction) — a refused `CreateSessionOnTx` call rolls
that entire transaction back, `github_pr_sessions` row included. **A repo can therefore never
acquire the one signal REST enrollment requires by the exact mechanism this gate exists to
refuse**: under `ModeCohort`, an unenrolled repo's first-ever mention attempt is refused, which
means its claim row never commits, which means `confirmRepoKnown` keeps reporting it unknown
forever — REST enrollment is structurally impossible for exactly the repos cohort rollout needs to
enroll. `internal/app/seed`'s tool is the only writer of `sessions_enabled` in v1 for exactly this
reason: it calls `RepoSettingsStore.UpsertSessionsEnabled` directly, bypassing `confirmRepoKnown`
entirely, the same way it already bypasses that gate for every other `repo_settings` column
(`seedmanifest.RepoSetting.SessionsEnabled`, a `*bool`, reconciled by `seed/reposettings.go`
exactly like its five sibling fields). No REST enrollment path is added by this Step.

### 32.7 Observability

Refusals are logged (structured `Warn`/`Error`, carrying the repo, `spawn_source`, and mode) at
every gate call site, including a transient, read-error-caused one — every one of the THREE real
enforcement points (`httpapi.checkRolloutGate` at session-creation time; `sessionactor`'s own
`refuseIfRolloutUnenrolled` at spawn/restore/resume time and `rolloutRefusalForDispatch` at
turn-dispatch time, §32.4) logs its own refusal line with a `transient` boolean attribute, so an
operator can tell the two apart in the SAME log line rather than by absence of a metric increment
alone (Phase 6 audit fix, Finding 4 — before this fix, only the `httpapi` side carried this
attribute at all, and neither `sessionactor` gate touched the metric below in any way). Only a
GENUINE POLICY refusal — mode is `cohort` and the named repo's own enrollment fact was actually
read, never one a degraded `repo_settings` read forced closed (see §32.5's own "fail-closed and
terminal are different properties") — is additionally counted by one OTel counter,
`session_rollout_refused_total` (tagged by `spawn_source`). ALL THREE call sites register and
increment this SAME instrument name: `httpapi` constructs it lazily on first use the same way
`cloud_identity_mint_total` is (`httpapi` has no per-process constructor object to anchor eager
construction to) — mirroring the `outbox_dead_letter_total` construction pattern
(`internal/app/outboxworker/builder.go`) one layer over; `sessionactor` constructs the identical
name as part of its own `opsMetrics` bundle (Step 77's own precedent, `opsmetrics.go`), built once
per `Registry` and threaded to every `Actor` it hydrates. A metrics backend aggregates by
instrument name across meters, so this is genuinely ONE counter from an operator's own point of
view — "how many repos is the cohort gate actually keeping out", across BOTH of §32's own
enforcement points — not two separately-named ones an operator would have to remember to sum.
A transient database blip is an infrastructure problem, not a rollout decision, and counting it
here would make this metric lie to an operator about how many repos are genuinely being refused by
policy — this rule is now identical at all three call sites. Refusals are **never audit-logged** —
this codebase's own `audit_log` table records completed STATE CHANGES only (`reposettings.go`'s own
`logUnknownRepoRefusal` doc comment states this convention explicitly for the structurally
identical "role check passed but the named resource isn't one we know" refusal), and a policy
refusal is not a state change. Flipping the flag itself, by contrast, IS a state change: the seed
tool's own `seedRepoSetting` writes a `seed.repo_setting_upserted` `audit_log` row for every
`sessions_enabled` change, on the same transaction as the `repo_settings` write, exactly like every
other seed-tool reconciliation.

### 32.8 Rollback: what actually happens, and what does not

Flipping `repo_settings.sessions_enabled` to `false` for a repo (or `NARVI_ROLLOUT_MODE` back to
`open` platform-wide, the coarser lever) has an immediate, provable effect on three things:

1. **No new session is ever created** for that repo again — §32.1/§32.2's gate refuses at the
   funnel.
2. **No de-enrolled session's sandbox spawns, restores, or resumes again** — §32.4's Part 1
   dispatch-time re-check (`refuseIfRolloutUnenrolled`, `tryPlanSpawn`) refuses the next
   spawn/restore/resume attempt, whether it is triggered by ordinary respawn machinery reacting to
   a stopped/stale sandbox or by `TriggerForceRespawn` recovering a genuinely stuck one.
3. **No turn is ever dispatched to an already-live sandbox again** — §32.4's Part 2 dispatch-time
   re-check (`rolloutRefusalForDispatch`, `executeDispatch`) refuses the next attempt to hand a
   `Pending` turn to a `Ready`/`Suspect` sandbox, INCLUDING, specifically, a fresh `@mention`/label
   re-trigger enqueuing a turn on an existing session via the REUSE branch — the exact case Part 1
   alone never covers, since a `Ready`/`Suspect` sandbox needs no spawn/restore/resume at all. A
   turn refused this way reaches a real terminal state (`Failed`) immediately, rather than sitting
   `Pending` to be silently re-attempted on every future `EnsureDispatched` round.

**What this does NOT do, stated plainly rather than implied**: sever a turn that is ALREADY
`Processing` at the instant the flag flips. Nothing in this codebase writes a
`FailureReasonCancelled` outcome, and the session actor has no cancel command — this is an honest,
pre-existing limitation this Step does not attempt to close (a hard-kill mechanism is out of scope
here and would be its own Step). Concretely, rollback for a session with a turn ALREADY
`Processing` at flip time is bounded by three existing, independent timers/authorities, never
instant — a `Pending` turn on a live sandbox, by contrast, is now refused essentially immediately
(point 3 above), not bounded by any of these three:

- A turn that is actively `Processing` when the flag flips runs to its own natural conclusion, or
  is force-failed once `platform.Timeouts.TurnDeadline` elapses (the CP's own persistent
  `turn_deadline` timer) — whichever comes first.
- A sandbox with no turn in flight is subject to the ordinary session-idle authority
  (`platform.Timeouts.ActorIdleTTL`) exactly like any other idle sandbox, de-enrolled or not.
- A sandbox that is neither cleanly stopped nor ever revisited by this Actor again (a crashed pod,
  say) is eventually reclaimed by the existing reconciler orphan-GC sweep
  (`internal/app/reconciler`), the same backstop every other orphaned sandbox already relies on.

De-enrollment is therefore a **stop-new-work** guarantee — including new *turns* on an existing
session, not just new sessions or new sandboxes — with a bounded, pre-existing tail for the one
thing it genuinely cannot touch: a turn already `Processing` at the instant the flag flips. Never
an instant kill switch for THAT one case. An operator rolling back for a live incident should
assume the tail for an already-`Processing` turn specifically, not assume immediacy there — but
should expect every OTHER form of new work (a fresh spawn, or a fresh turn dispatched to an idle,
already-live sandbox) to stop refusing immediately.

### 32.9 Operator runbook

**Enroll a repository** (v1, seed-manifest-only — §32.6): add or update a `repoSettings` entry in
the seed manifest with `sessionsEnabled: true` for the target `owner/repo`, then run the seed tool
against the target deployment. Confirm via `repo_settings.sessions_enabled` for that row, or the
`seed.repo_setting_upserted` `audit_log` row the seed tool just wrote.

**Arm cohort mode platform-wide**: set `NARVI_ROLLOUT_MODE=cohort` and restart the control plane
(boot-time config, §5.4 — never a live-reloadable value). Before doing this in production, confirm
every repository that must keep working is already enrolled (§32.6) — arming cohort mode with no
repos enrolled refuses every single new session platform-wide, GitHub included.

**Roll back one repository**: set that repo's `sessionsEnabled: false` in the seed manifest and
re-run the seed tool (or a direct, deliberate `UpsertSessionsEnabled` call if the seed tool itself
is unavailable — this is the one break-glass exception to "seed-manifest-only", since the write
path is identical either way). Expect, per §32.8: new session-creation attempts for that repo
refused immediately (visible as `session_rollout_refused_total{spawn_source=...}` increments and a
`"httpapi: rollout gate: session creation refused"` log line); a *new* turn — including a fresh
`@mention`/label re-trigger riding the REUSE branch onto an existing session — refused essentially
immediately too, whether or not that session's sandbox is already live (`"sessionactor: refusing to
spawn"` for a needed spawn/restore/resume, `"sessionactor: refusing to dispatch turn"` for a
dispatch to an already-`Ready`/`Suspect` sandbox — §32.4's own two parts — each ALSO incrementing
`session_rollout_refused_total{spawn_source=...}`, the SAME instrument, since Finding 4 of the
Phase 6 audit closed the gap where these two dispatch-time refusals never touched the metric at
all); any turn already
`Processing` at rollback time continues until it completes or `TurnDeadline` fires; idle sandboxes
for that repo's sessions stop on the ordinary `ActorIdleTTL`; anything left over is reclaimed by
the next reconciler orphan-GC sweep. There is no faster path for an already-`Processing` turn than
these three existing timers/authorities today — every OTHER form of new work stops immediately.

**Roll back platform-wide**: unset `NARVI_ROLLOUT_MODE` (or set it to `open`) and restart the
control plane. This is the coarser lever — it removes the per-repo gate entirely, admitting every
repo unconditionally again, not just the ones currently enrolled. Prefer the per-repository lever
above unless the incident is platform-wide.

**Verify a rollback took effect**: attempt a fresh `@mention`/REST create against the de-enrolled
repo and confirm the channel-appropriate refusal from §32.5 (GitHub: silent 200, nothing posted;
Slack/Linear: an honest in-thread/agent-session notice; REST: 403 "repository not enrolled"). A
session created BEFORE the rollback continuing to run for up to `TurnDeadline`/`ActorIdleTTL`
longer is expected ONLY for a turn that was already `Processing` at rollback time — a fresh
`@mention` on that same PR after rollback is refused immediately regardless (§32.4 Part 2) — not a
sign the rollback failed. See §32.8.

**Distinguishing a policy refusal from a database blip during an incident**: if refusals stop
appearing, or a channel behaves as though a still-enrolled repo were refused, check whether the
SAME refusal log line every one of the three enforcement points already emits — `"...refused, repo
not enrolled"` (`httpapi`), `"...refusing to spawn..."`, `"...refusing to dispatch turn..."` (both
`sessionactor`) — carries `transient=true` (Phase 6 audit fix, Finding 4: all three now carry this
attribute uniformly; before this fix only the `httpapi` line did, and `sessionactor`'s own read-error
case had no distinguishing signal on the refusal line at all), and whether
`session_rollout_refused_total` actually incremented — it does NOT for a transient read failure, at
any of the three call sites (§32.7). A `sessionactor`-side read error specifically ALSO logs its own
lower-level `"...rollout re-check: read repo_settings failed; failing closed..."` `Warn` line, one
level below the refusal line above, from `rolloutDecisionForSession` itself — useful for confirming
WHICH repo_settings read failed and why, but the `transient` attribute on the refusal line is the
one signal to check first. A transient failure refuses that ONE attempt but is retried by the
channel's own ordinary retry path (§32.5) once Postgres recovers; it is not evidence the rollback
itself is broken, and does not need a rollback re-applied.

## 33. Metrics export path

§5.3 required observability "day one, not later", and the instruments exist — but nothing carries
them anywhere. `platform.SetupOTel` (`internal/platform/otel.go`) builds a `stdouttrace` exporter
and a `stdoutmetric` exporter, registers both globally, and stops; `go.mod` carries no OTLP module
at all. Every metric this platform emits is therefore written as JSON to its own process's stdout
and aggregated by nobody, which is precisely why §10-P6's exit criterion is unmet. The alert rules
under `deploy/observability/alerts/` are correctly derived and CI-checked against real instrument
names, and they page no one.

### 33.1 The instruments are split across two processes, and only one of them is easy

The control plane registers eleven files' worth of instruments and is a long-lived single process:
an OTLP endpoint simply works there. sandbox-agent registers four histograms
(`internal/sandboxagent/boot/telemetry.go`, `internal/sandboxagent/gitclone/telemetry.go`) and runs
**inside the ephemeral sandbox** — minutes of life, one process per session, stdout going to the
sandbox's own log stream. One of those four, `sandbox_agent_boot_duration_seconds`, is the metric
behind SLO 1 and `BootDurationP95High`.

Two properties make the sandbox side genuinely harder rather than merely different. The periodic
metric reader runs at the library default while the process may live four minutes, and the bounded
shutdown flush covers clean exits and panics but not a SIGKILL or a provider teardown — so metrics
can be lost before they are ever exported. And §27.6's egress allowlist floor, appended server-side,
admits exactly the control-plane host plus the session's git hosts (`allowlistFloorHosts`); a
collector endpoint is not in it.

### 33.2 Control-plane derivation was tested first, and fails for all four

The cheapest possible design is no transport at all: recompute the durations control-plane-side from
events already on the wire and already persisted. It does not hold, and the reasons are worth
recording so the idea is not re-proposed.

- **`sandbox_agent_boot_duration_seconds`.** A proxy exists — first "ready" arrival to the first
  nil-phase heartbeat — and is disqualifyingly lossy at both ends. `Bridge.MarkBootComplete` is a
  bare `setLastBootPhase(nil)` with **no forced heartbeat**, unlike its sibling
  `Bridge.SetConversationID`, which does push `forceHeartbeat`. Boot completion is therefore learned
  up to a full `SandboxWSHeartbeatInterval` (30s) late — against a 240s alert threshold, up to ~12%
  systematic inflation of the exact p95 the alert reads. The start is separately offset because the
  WS dial runs concurrently with boot. And SLO 1 pins the metric's semantics to the sandbox's own
  wall-clock measurement, never including the connect handshake: an arrival-time derivation is a
  different quantity wearing the same name.
- **Failed boots vanish entirely.** On `bootErr != nil` sandbox-agent cancels its context and exits;
  no wire event says a boot failed. The `failed=true` data points have no control-plane equivalent
  at all.
- **The hook and git-fetch phases emit nothing** on the wire, and the one `git_sync` checkout event
  carries no timestamp. Arrival-time deltas are unusable by construction anyway: best-effort events
  are buffered and **resent in bulk on reconnect**, so a mid-boot blip collapses every phase arrival
  onto a single instant.

### 33.3 The decision: ship the fact, record centrally

sandbox-agent sends one new **best-effort** `boot_timing` event carrying the seconds it has already
measured plus its low-cardinality tags, and the control plane records the histogram. Not raw
observations, not pre-aggregated buckets — the measured fact.

This keeps fidelity exactly: the duration is still produced by the same `time.Since` bracket on the
sandbox's own clock, so SLO 1's documented semantics survive, and the metric keeps its name and its
hand-tuned buckets. It needs no new transport, no protocol beyond one additive event type, and no
egress change — the channel is the authenticated WebSocket that is already open before boot begins,
which also dissolves the ephemeral-flush problem, since the fact crosses the wire at measurement
time rather than waiting for an export interval the process may not survive.

Three properties are load-bearing and are requirements, not implementation detail:

1. **Best-effort, never critical.** Telemetry does not earn a place among §6.1's critical acked
   types, and its loss must never fail a boot or a turn.
2. **Recording is gated on `appendRawEvent`'s `inserted` flag.** §6.1's reconnect resend would
   otherwise double-count every buffered timing — the identical defect Step 77 fixed in
   `turn_false_failure_total`, whose fix is the precedent to copy rather than re-derive.
3. **The repo name is dropped from metric attributes.** It is unbounded cardinality; it rides the
   event into the `events` log instead, where per-session debugging actually wants it.

### 33.4 Rejected: direct OTLP from the sandbox, and provider log scraping

**Direct OTLP** would require widening §27.6's allowlist floor to admit a collector host in every
customer sandbox's reachable set, and — decisively — any ingestion credential would have to live
where customer-directed, model-authored code runs. That is the exact secret class this codebase
strips from every child environment, and the reason §27.4's `kube-credential` subcommand was removed
rather than given the environment it wanted.

**Scraping the sandbox's stdout through the provider** has nothing to build on: the Modal adapter's
surface is lifecycle operations only, with no log-fetch shape. It would mean inventing a wire
operation *and* parsing an unstable exporter's JSON out of an interleaved boot log — a parallel
mechanism, and a fragile one.

### 33.5 Vendor stance, and the documents this invalidates

The alert JSON's `condition` strings stay what they are: **CI-checked threshold documentation**, not
a source to generate a backend's rule language from. §1 deliberately pins no observability vendor,
`internal/ops/schema.go` is written to that, and generating vendor formats would pin one while
duplicating a translation each deployment performs once. Evaluation remains the operator's
collector's job, as `docs/PRODUCTION_CHECKLIST.md` item 5 already states.

Shipping this invalidates text that must move with it: the checklist item asserting stdout-only
export with no OTLP anywhere (Step 111 turns it into a passable check); SLO 1's Metric paragraph and
`docs/runbooks/slow-boot-and-spawn.md`, both of which cite the sandbox-side telemetry file Step 112
deletes — the `narvi-metrics` fences themselves stay valid, since no instrument name changes; and
`internal/platform/otel.go`'s own top comment. SLO 1 additionally contains a loose claim that
boot-progress reports are emitted throughout the gitclone phase — gitclone emits `git_sync`, not
`boot_progress`; harmless for liveness, since both bump `last_seen_at`, but it should be corrected
in the same pass.

One consequence to state rather than discover: with the control plane recording these histograms, a
multi-pod deployment splits each one across pods' exporters. That is ordinary OTel bucket merging at
query time, not a defect, but it is a difference from the single-process recording it replaces.

## 34. Extension & licensing boundaries

Narvi is source-available under the Elastic License 2.0. Two companion projects are
planned on top of it: **Narvi Gatekeeper**, an organization-scale review-governance
module composed into a second binary from a separate repository, and **Narvi Desktop**,
an individual client that talks to this system only through its versioned wire API.
Neither exists yet and how either is distributed is undecided; nothing here depends on
that answer. This section specifies the seams that make composition possible **without
this repository ever knowing either project exists**. The detailed design, with Go shapes and test lists, is
`docs/design/boundaries-design.md`; what follows is the part future work must obey.

Naming, fixed: this repository is Narvi, the product. It is never "core" and never a
"community edition" — in code, comments, copy or commit messages.

### 34.1 What stays here, always

Everything per-repository: the full review pipeline, sessions and sandboxes, RBAC and
SSO, shadow mode (§30) and its Activate surface, the ingress surfaces, metrics export
(§33), and mode A of per-repository knowledge (§31). **No security capability is ever
gated by an entitlement.** A deployment with no key configured is a complete, secure
product; what a composed module may add is organization scale and compliance, never base
safety. A change that would move a shipped capability out of this repository is out of
bounds — a module is built from work that has not happened yet, never from work
subtracted from here.

### 34.2 The constraint that shapes every seam

Go's `internal` rule means a separate module can never import
`github.com/narvidev/narvi/internal/...`. Every type that crosses to a composed module
must therefore be re-exported by a non-`internal` package. Two exist for that purpose:

- `controlplane` — the importable composition root (§34.6).
- `extension` — a leaf façade re-exporting, through type aliases, exactly the types a
  composed module needs.

This is a property to keep, not a limitation to work around: **the crossing set is an
explicit, small, reviewable list**, and widening it is a deliberate PR in this
repository. Dependency direction is one-way and total: `controlplane` → `extension` →
`internal/...`. No `internal` package may import either.

### 34.3 The minimal-delta rule

If a capability can be written without touching this repository, it belongs in the
private module. If it needs a change here, this repository gains a **generic** extension
point and the private module holds the **specific** use. This repository never
references the private one — not by import, not by build tag, not by a name in a
conditional.

### 34.4 No private types in public contracts

The wire contracts in `/contracts` express **capabilities**, never a private module's
internal shapes. A read model may say that a capability is present, absent, or
unlicensed; it may never carry a licence key, a subject, or a module's own vocabulary.
The capability enum on the wire mirrors the Go enum exactly, and adding a value is a
deliberate contract change.

### 34.5 The capability registry, and the guarantee it must never touch

A single point of truth answers `Enabled(capability) bool`, computed as **installed AND
licensed AND valid-now**: a capability is enabled only when a composed module actually
supplied its implementation, the verified key grants it, and the grant's window contains
the current instant. Consequences that are requirements, not implementation notes:

- **A key can entitle; it can never create behavior.** With no module composed, the
  installed set is empty and every capability is disabled whatever the key says.
- **Every failure is a bare `false`.** Absent, malformed, wrongly signed, wrong-product,
  expired, not-yet-valid: all disabled, none returning an error a caller could discard
  on the way to treating "unknown" as "enabled" (§30.4's own reasoning about
  fail-direction at a boundary). Boot never fails on a bad key: a licensing lapse must
  not become an outage.
- **Clock skew widens `nbf` only, never `exp`.** A host clock ahead expires a key early,
  which is the safe direction; there is no grace window after expiry.
- **The registry is unreachable from any suppression path.** §30's guarantees are
  required to be structural — a future contributor cannot silently un-make them — and a
  licence state must never be an input to a suppression decision. This is enforced by a
  `narvichecks` analyzer that bans *importing* the registry, the licence domain or the
  `extension` façade outside a named allow-list of **packages** (the composition root,
  the façade, and the registry/licence packages themselves). An import is a syntactic
  fact; a banned call by name is not, because a wrapper or a method value dodges it —
  and, for the same reason, so is a **file**-level exemption inside an otherwise-banned
  package: Go import declarations are per file, but identifier scope is per package, so
  any other file there can call a package-scope helper an exempted file declares with no
  import of its own. The two HTTP handlers that gate a capability at a route boundary
  (`RequireCapability`, `GetCapabilities`) therefore hold no file-level exemption either
  — each takes the already-evaluated decision (a `func() bool`, and a func building the
  read-model response) instead of importing the registry or the licence domain itself.
  No shadow, egress, outbox, mint or session-actor package can reach the registry at
  all.
- **The registry type cannot express "deny".** It has no method that turns a public
  behavior off. Every consumer reads
  `if enabled { private implementation } else { the public one }`, and the `else` branch
  is the wiring that already exists.

Licence keys are verified offline against public keys embedded in the binary — no
phone-home, ever. Key custody and minting live in the private repository.

### 34.6 The composition root

The wiring that assembles this system is an importable package, not a `main`. It exposes
an entry point taking zero or more modules; `cmd/control-plane` is a one-line `main`, and
the private binary is the same line plus its module. Modules are an **explicit struct of
nil-able hooks** — capabilities, migrations, routes, workers, a knowledge ranker, web
assets — validated at boot: a module claiming a capability it does not implement refuses
to start.

Rejected, and worth recording: a module registering itself through `init()` side effects.
A module that can appear invisibly can also un-make a guarantee invisibly, and the
composition root could not validate the combination at boot.

Module routes mount under a reserved prefix behind the same authentication as every
other API route; module migrations run in the module's own migrations table, never
sharing a version counter with this repository's chain.

### 34.7 Gate-then-rank as a signature

§31.6 requires that eligibility and ordering be separate: a deterministic, path-scoped
selector decides which prior decisions may be injected at all, and ranking decides only
their order. The seam that lets a private ranker replace the ordering substrate makes
that **structural**: a ranker receives the already-gated candidates and returns
**scores, never candidates**. It has no return channel through which a candidate could
enter, so it cannot add, drop, replace or re-select — and the cap on how many are
injected is applied by the caller, after ordering. Degradation (a slow, failing or
inconsistent ranker) falls back to the gate's own order, never to empty, and is recorded
on the turn so the mode A/B comparison never counts a degraded turn as the B arm.

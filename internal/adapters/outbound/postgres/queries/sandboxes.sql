-- Queries backing SandboxStore (§4.3). Just enough to prove the pipeline
-- end to end (create + get) — the UNIQUE(session_id) constraint (§3.2) is
-- exercised by the integration test, not by these queries.

-- name: CreateSandbox :one
INSERT INTO sandboxes (session_id)
VALUES ($1)
RETURNING *;

-- name: GetSandbox :one
SELECT * FROM sandboxes
WHERE session_id = $1;

-- name: UpdateSandboxStatus :one
-- Sets a sandbox's status, plus last_seen_at when the caller supplies a
-- real timestamp (sqlc.narg + COALESCE, same pattern as
-- UpdateTurnStatus) -- per §3.2 "Liveness = max of all signals",
-- last_seen_at only ever moves forward on an actual signal, never as a
-- side effect of a plain status write.
--
-- agent_version/image_digest (§12.2 item 1's own boot-fingerprint gap,
-- migrations/000120) mirror last_seen_at's own sqlc.narg + COALESCE
-- shape: every event type this query's one caller (handleSandboxEvent)
-- handles passes both as nil EXCEPT a "ready" event (the only one that
-- carries them, sandbox-ws's own Ready def), so an ordinary heartbeat/
-- tool_call/etc. leaves whatever this gen's own "ready" event already
-- recorded untouched rather than clobbering it back to NULL.
UPDATE sandboxes
SET status = $2,
    last_seen_at = COALESCE(sqlc.narg('last_seen_at'), last_seen_at),
    agent_version = COALESCE(sqlc.narg('agent_version'), agent_version),
    image_digest = COALESCE(sqlc.narg('image_digest'), image_digest),
    updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: UpsertSandboxForSpawn :one
-- §9.3 ("e2e happy path"), design decision 3a: creates the sandbox row
-- (gen=1) if none exists yet, or bumps gen/resets status to 'spawning'/
-- rotates token_hash if one already does (§3.2: "every spawn/restore
-- increments sandbox.gen" -- paraphrased above, not a verbatim quote).
-- provider_id is deliberately NOT cleared here --
-- the second, post-CreateSandbox write (UpdateSandboxProviderID) is what
-- ever sets it, and a stale previous-gen provider_id lingering between
-- those two writes is harmless (SpawnState.ProviderObjectID is only read
-- BEFORE this upsert runs, from the row as it stood prior to this call).
--
-- Fix (audit finding F3): the ON CONFLICT (resume/restore/re-claim) branch
-- now also sets last_seen_at = now() -- this claim is itself a fresh sign
-- of life for the row, exactly like RecoverSandboxFromSuspect's own "this
-- write is itself the liveness signal" precedent below. A resume/restore
-- claim in particular can land on a row that sat in a terminal status
-- (Stopped/Failed/Stale) for arbitrarily long before being claimed again,
-- so without this, sinceLastSignOfLife in domain/sandbox.
-- EvaluateSpawnDecision's own Skip guard (measured from max(created_at,
-- last_seen_at)) would still reflect however long the box sat idle
-- beforehand, not this claim -- defeating that guard's "no-op a concurrent
-- second actor for free" purpose for exactly the case (resume/restore of a
-- long-terminal box) it most needs to cover. Because session_id is
-- UNIQUE, any second concurrent caller for the same session is guaranteed
-- to land on THIS branch (not the INSERT below) regardless of which one
-- physically ran first, so this is the only branch that needs to move
-- last_seen_at for that guard's sake.
--
-- The INSERT (fresh-row) branch below deliberately does NOT set
-- last_seen_at: leaving it NULL until the sandbox's own first liveness
-- signal (the column's doc comment, migrations/000006_sandboxes.up.sql)
-- is what tells domain/sandbox.EvaluateConnectingTimeout this box hasn't
-- connected yet, so it grants the longer FirstConnectBudget (240s) rather
-- than the steady-state SteadyHeartbeatBudget (90s) while it cold-starts.
-- Setting it here too was tried and reverted: it made every fresh spawn's
-- very first connecting-deadline check see a non-zero last_seen_at (==
-- created_at) and wrongly pick the 90s budget, false-positiving a
-- perfectly normal slow boot into "stuck spawn" territory. It also isn't
-- needed for the Skip guard above -- a fresh INSERT has no prior row for a
-- concurrent caller to race against; maxTime(created_at, zero) ==
-- created_at already reads as "just spawned".
-- agent_version/image_digest (§12.2 item 1, migrations/000120) ARE
-- cleared here, on the SAME respawn branch that leaves provider_id
-- untouched -- the opposite call for the opposite reason (that column's
-- own doc comment: harmless because nothing ever renders a stale
-- provider_id to a human). A fingerprint IS rendered directly on the
-- session rail, so a stale previous-gen value lingering through this
-- gen's own connecting/booting window would be actively misleading --
-- "not reported yet" (NULL) is the honest state until THIS gen's own
-- first "ready" event repopulates them.
--
-- image_decision_reason/image_decision_fingerprint (§19's own persisted-
-- decision-provenance gap, migrations/000139_sandboxes_image_decision.
-- up.sql) are cleared here for
-- the IDENTICAL reason agent_version/image_digest already are: a stale
-- previous-gen decision must never linger and be misread as THIS gen's
-- own outcome. resolveAndSetImage (imageresolve.go) writes the real value
-- for this gen immediately after this upsert runs (dispatch.go's own
-- "outside any transaction, immediately before the provider is ever
-- called" sequencing) -- NULL here is simply the honest gap between that
-- gen bump and this gen's own first real decision.
--
-- pr_delivery_started_at (technical plan §43.20, migrations/000145) is
-- cleared here too: a push the previous gen was sent can never report back
-- once that gen is replaced -- its push_complete or push_error would be
-- rejected as stale-gen -- so its delivery is over, and the session's
-- status must not keep reading it as under way until
-- MCPStatusDeliveryWindow runs out.
--
-- stop_retire_gen (technical plan §3.3, migrations/000151) is cleared too:
-- it names the gen a person's stop has still to retire, and a new gen owes
-- nothing -- the one it replaces is fenced off by the bump itself.
--
-- lifetime_deadline_at, lifetime_seconds and lifetime_deadline_gen
-- (technical plan §35.2, migrations/000162) are stamped here, in the
-- statement that creates the gen, because this is the one statement every
-- spawn, restore and resume runs, and it runs in the claim's transaction,
-- before the provider is called (dispatch.go): the database's now() plus
-- lifetime_seconds, the session kind's lifetime, is therefore never later
-- than the provider's own deadline when the provider gives a sandbox at
-- least that long. Both branches stamp it from this statement's now(), not
-- from created_at, which only the INSERT branch sets: a respawn or restore
-- keeps the first gen's created_at, and a deadline counted from it would
-- already be past. lifetime_deadline_gen records which gen the deadline is
-- for; the deadline counts only while it equals gen.
--
-- A NULL lifetime_seconds is a resume's (planResume): the same provider
-- object, never younger than it was, so a fresh stamp would be later than
-- the provider's own deadline. It carries the deadline forward to the new
-- gen when that deadline was the live gen's own, and leaves none when it
-- was not -- a gen the previous binary created reads "deadline unknown"
-- (the migration's rolling-deploy section). Named arguments only: sqlc
-- does not mix them with $n.
INSERT INTO sandboxes (session_id, gen, status, token_hash,
                       lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen)
VALUES (sqlc.arg('session_id'), 1, 'spawning', sqlc.narg('token_hash'),
        now() + make_interval(secs => sqlc.narg('lifetime_seconds')::integer),
        sqlc.narg('lifetime_seconds')::integer,
        CASE WHEN sqlc.narg('lifetime_seconds')::integer IS NULL THEN NULL ELSE 1 END)
ON CONFLICT (session_id) DO UPDATE
SET gen = sandboxes.gen + 1,
    status = 'spawning',
    token_hash = EXCLUDED.token_hash,
    last_seen_at = now(),
    agent_version = NULL,
    image_digest = NULL,
    image_decision_reason = NULL,
    image_decision_fingerprint = NULL,
    pr_delivery_started_at = NULL,
    stop_retire_gen = NULL,
    lifetime_deadline_at = CASE
        WHEN sqlc.narg('lifetime_seconds')::integer IS NOT NULL THEN EXCLUDED.lifetime_deadline_at
        WHEN sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.lifetime_deadline_at
    END,
    lifetime_seconds = CASE
        WHEN sqlc.narg('lifetime_seconds')::integer IS NOT NULL THEN EXCLUDED.lifetime_seconds
        WHEN sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.lifetime_seconds
    END,
    lifetime_deadline_gen = CASE
        WHEN sqlc.narg('lifetime_seconds')::integer IS NOT NULL
          OR sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.gen + 1
    END,
    updated_at = now()
RETURNING *;

-- name: UpdateSandboxProviderID :one
-- Records the provider's own opaque handle (internal/app/ports.SandboxRef.
-- ProviderID) once CreateSandbox actually succeeds -- a SEPARATE write
-- from UpsertSandboxForSpawn above, deliberately run in its own
-- transaction AFTER the real CreateSandbox network call returns (a
-- network call must never hold a Postgres transaction open).
UPDATE sandboxes
SET provider_id = $2, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: UpdateSandboxCircuitBreaker :one
-- Persists internal/domain/sandbox.CircuitBreakerState verbatim: called
-- with (0, NULL) when EvaluateCircuitBreaker's own ShouldReset is true,
-- and with (incremented count, now) when a *ports.ProviderError with
-- Transient=false increments the breaker (§3.2: "3 permanent spawn
-- failures within 5 min blocks spawning"). Always a direct SET, never
-- COALESCE -- unlike status/last_seen_at, both fields are meant to be
-- overwritten with exactly the caller-computed value every time,
-- including back to NULL on reset.
UPDATE sandboxes
SET spawn_failure_count = $2, last_spawn_failure_at = $3, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: UpdateSandboxSnapshotID :one
-- §3.2 ("snapshots & restore"), design decision 3: records a real,
-- sandbox-confirmed snapshot id once a "snapshot_ready" wire event
-- arrives -- read back as SpawnState.SnapshotImageID (internal/domain/
-- sandbox.EvaluateSpawnDecision's own restore-eligibility input) on a
-- later spawn decision. Deliberately a direct SET, mirroring
-- UpdateSandboxProviderID's own precedent exactly (not COALESCE-guarded
-- like status/last_seen_at -- every call here carries a real, just-
-- confirmed id meant to overwrite whatever was there before). Also
-- clears pending_snapshot_message_id back to NULL in the SAME statement:
-- this query's only caller (handleSnapshotReadyEvent's accept path) only
-- ever reaches here after already confirming the event's own
-- commandMessageId matches that column's current value, so the
-- outstanding attempt this call completes is, by construction, exactly
-- the one that column was tracking -- see that column's own migration
-- doc comment (migrations/000022_sandbox_snapshot_id.up.sql) for the full
-- race this closes.
--
-- snapshot_suppressed_in_shadow (§30.4(3), migrations/
-- 000106_sandbox_snapshot_shadow_bit.up.sql) is stamped in this SAME
-- statement, at this SAME snapshot-confirmation moment -- the effective
-- egress mode this session was resolved to have while the snapshot that
-- just completed was live, computed ONCE by the caller
-- (handleSnapshotReadyEvent) and never re-derived by anything that later
-- reads this column back (app/sessionactor/dispatch.go's own restore-time
-- refusal check).
UPDATE sandboxes
SET snapshot_id = $2, snapshot_suppressed_in_shadow = $3, pending_snapshot_message_id = NULL, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: UpdateSandboxStatusToSuspect :one
-- §3.2 ("two-phase terminalization"): the single write
-- transitionSandboxToSuspect (internal/app/sessionactor/timerfired.go)
-- performs when a watchdog moves a sandbox into Suspect. Sets status =
-- 'suspect' as a hardcoded literal -- mirroring UpsertSandboxForSpawn's
-- own hardcoded 'spawning' literal precedent exactly: this query has
-- exactly one legal target status, by construction of its own single call
-- site -- AND persists pre_suspect_status = the state being left, in the
-- SAME statement, so §3.2's own "any liveness signal during grace returns
-- to previous state" rule has somewhere to read that state back from
-- later (handleSandboxEvent's own recovery branch, sandboxevent.go,
-- RecoverSandboxFromSuspect below). Deliberately does NOT touch
-- last_seen_at -- entering Suspect is a watchdog's own classification of
-- silence, never itself a liveness signal.
UPDATE sandboxes
SET status = 'suspect',
    pre_suspect_status = $2,
    updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: RecoverSandboxFromSuspect :one
-- §3.2 ("two-phase terminalization"): the single write
-- handleSandboxEvent's own recovery branch (sandboxevent.go) performs
-- when ANY recognized inbound sandbox event arrives for a Suspect sandbox
-- that still carries a pre_suspect_status -- i.e. "any liveness signal
-- during grace returns to previous state" (§3.2), the event itself being
-- that liveness signal. Sets status = $2 (the recovered, previously-live
-- state sandbox.Transition(StateSuspect, gen, RecoverTrigger(...)) already
-- validated), clears pre_suspect_status back to NULL (no longer needed --
-- mirrors UpdateSandboxSnapshotID's own "clear the now-satisfied
-- outstanding column in the same statement" precedent), and sets
-- last_seen_at to the event's own arrival time -- unlike
-- UpdateSandboxStatusToSuspect above, THIS write is itself the liveness
-- signal that caused the recovery, so last_seen_at moves forward exactly
-- like the general per-event UpdateSandboxStatus write already does for
-- every other recognized event. Deliberately a direct SET (not
-- COALESCE'd), mirroring UpdateSandboxProviderID/UpdateSandboxSnapshotID's
-- own precedent: this call site always has a real, just-observed
-- timestamp to write.
UPDATE sandboxes
SET status = $2,
    pre_suspect_status = NULL,
    last_seen_at = $3,
    updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: MarkSandboxBootEvidence :exec
-- Records that gen $2's boot has actually run (technical plan §3.2,
-- migrations/000147_sandbox_boot_evidence_gen.up.sql): handleSandboxEvent
-- (sandboxevent.go) calls it, in the transaction that stores the event, on
-- the first event of a gen that is boot evidence. Guarded on gen so
-- evidence can only ever be recorded for the gen that is live, never
-- carried into the next one.
UPDATE sandboxes
SET boot_evidence_gen = gen,
    updated_at = now()
WHERE session_id = $1
  AND gen = $2;

-- name: RecordSandboxReady :exec
-- Technical plan §3.3, prompt receipts (migrations/000155_prompt_receipts.up.sql):
-- counts a ready of gen $gen, and records whether it advertised
-- capabilities.promptReceipt -- the latest ready of the live gen decides,
-- so one that does not advertise it clears it. handleSandboxEvent
-- (sandboxevent.go) calls it in the transaction that stores the ready.
-- It also records the read limit the ready stated
-- (capabilities.maxFrameBytes, migrations/000157_agent_max_frame_bytes.up.sql)
-- against the gen, the same way: a ready that states none (a NULL
-- max_frame_bytes) clears both columns, and the session actor holds the
-- gen's prompts to the promptReceipt rule (promptFrameBound).
-- It records capabilities.reviewCheckout against the gen by the same rule
-- (review_checkout_gen, migrations/000167_review_turn_checkout.up.sql):
-- the session actor sends a review turn's checkout only to a gen whose
-- latest ready advertised it (reviewcheckout.go).
-- Guarded on gen like MarkSandboxBootEvidence: a ready of any other gen
-- counts nothing and records nothing, so the capability can only ever be
-- recorded for the gen that is live.
UPDATE sandboxes
SET ready_seq = ready_seq + 1,
    prompt_receipt_gen = CASE WHEN sqlc.arg('prompt_receipt')::boolean THEN gen ELSE NULL END,
    review_checkout_gen = CASE WHEN sqlc.arg('review_checkout')::boolean THEN gen ELSE NULL END,
    agent_max_frame_bytes = sqlc.narg('max_frame_bytes')::integer,
    agent_max_frame_bytes_gen = CASE WHEN sqlc.narg('max_frame_bytes')::integer IS NULL THEN NULL ELSE gen END,
    updated_at = now()
WHERE session_id = sqlc.arg('session_id') AND gen = sqlc.arg('gen')::integer;

-- name: MarkSandboxBootingSince :exec
-- Records when gen $2 entered Booting, on this database's clock (technical
-- plan §3.2's boot-evidence fallback,
-- migrations/000148_sandbox_booting_since.up.sql), once per gen: a later
-- call for the same gen keeps the first start. handleSandboxEvent
-- (sandboxevent.go) calls it on the Connecting -> Booting edge, and again
-- on a null-phase heartbeat that finds no start for its gen (a sandbox
-- Booting when the column was added). Guarded on gen like
-- MarkSandboxBootEvidence.
UPDATE sandboxes
SET booting_since = now(),
    booting_since_gen = gen,
    updated_at = now()
WHERE session_id = $1
  AND gen = $2
  AND booting_since_gen IS DISTINCT FROM gen;

-- name: GetSandboxBootingElapsed :one
-- How long gen $2 has been Booting, in nanoseconds (a time.Duration),
-- measured on this database's clock from the start MarkSandboxBootingSince
-- recorded -- both ends on one clock, so no skew between control-plane
-- replicas enters it. No row when that gen has no start recorded.
SELECT (EXTRACT(EPOCH FROM (now() - booting_since)) * 1000000000)::bigint AS elapsed_nanos
FROM sandboxes
WHERE session_id = $1
  AND gen = $2
  AND booting_since_gen = gen
  AND booting_since IS NOT NULL;

-- name: ListLiveSandboxProviderIDs :many
-- §5.3 ("reconciler + GC", §5.3): the reconciler's own "expected still
-- alive" set -- the provider_id of every sandbox row currently in a LIVE
-- status, across ALL sessions. Unlike every OTHER query in this file
-- (each scoped to one session_id via WHERE session_id = $1), this one
-- genuinely needs to scan the whole table -- by design, not oversight:
-- app/reconciler.Reconciler.ReconcileOnce compares this set against
-- ports.SandboxProvider.List's real, currently-live provider-side refs,
-- and any ref with no corresponding row here is a genuine orphan (see
-- that method's own doc comment for the two ways one arises).
--
-- 'pending' is excluded: a pending sandbox has no provider object yet
-- (UpsertSandboxForSpawn only ever creates a row already in 'spawning').
-- 'stopped'/'failed' are excluded DELIBERATELY, not merely omitted: they
-- are exactly the terminal statuses whose own leaked provider objects
-- this reconciler exists to catch (StopSandbox has had no real caller
-- anywhere in this codebase before this Step), so a stale provider_id
-- still lingering on a terminal row (UpsertSandboxForSpawn's own doc
-- comment notes provider_id is never cleared on respawn) must NOT count
-- as "expected alive" here.
SELECT provider_id FROM sandboxes
WHERE status IN ('spawning', 'connecting', 'booting', 'ready', 'snapshotting', 'suspect')
  AND provider_id IS NOT NULL;

-- name: UpdateSandboxPendingSnapshotMessageID :one
-- §3.2 fix (message-id correlation): sets or clears (pass NULL)
-- pending_snapshot_message_id -- the MessageId of whichever Snapshot
-- command this sandbox is currently waiting on a snapshot_ready for.
-- triggerSnapshotBestEffort sets it, in the SAME transact that commits
-- the Ready->Snapshotting transition (sandboxevent.go); both
-- revertSnapshotBestEffort's compensating-write path and
-- handleSnapshotReadyEvent's decode-failure revert path clear it back to
-- NULL when they revert Snapshotting->Ready, so a stale attempt's
-- eventual real snapshot_ready, if it ever arrives, correctly finds no
-- matching pending id and is discarded as stale. Deliberately a direct
-- SET, mirroring UpdateSandboxProviderID's own precedent exactly (never
-- COALESCE-guarded -- every call here carries the caller's own
-- deliberately computed value, including NULL).
UPDATE sandboxes
SET pending_snapshot_message_id = $2, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: SetSandboxPendingPush :one
-- §30.8's own "the push/PR pair resolves its mode ONCE per turn"
-- (migrations/000107_sandbox_pending_push_egress_mode.up.sql): stamps
-- this session's own effective egress mode, resolved exactly once by
-- completeProcessingTurn (app/sessionactor/pushpr.go) at the moment it
-- builds the turn's own pushSignal, in the SAME transact that completes
-- the turn. Also resets pending_push_cancelled back to false in the SAME
-- statement -- a brand-new push cycle starting now supersedes whatever a
-- STALE prior cycle may have left behind (mirrors
-- UpdateSandboxSnapshotID's own "clear the prior cycle's own leftover
-- state in the same write" precedent).
UPDATE sandboxes
SET pending_push_suppressed_in_shadow = $2, pending_push_cancelled = false, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: ClearSandboxPendingPush :one
-- Consumes this sandbox's own persisted push/PR decision -- called by
-- createPRBestEffort (pushpr.go) once it has read and acted on
-- pending_push_suppressed_in_shadow/pending_push_cancelled for the
-- current push cycle, so a LATER, unrelated push_complete redelivery (or
-- the next real push cycle) never reads a stale decision back. Mirrors
-- UpdateSandboxSnapshotID's own "clear the now-satisfied outstanding
-- column" idiom.
UPDATE sandboxes
SET pending_push_suppressed_in_shadow = NULL, pending_push_cancelled = false, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: CancelSandboxPendingPush :one
-- §30.4's own "demotion ... must cancel in-flight push signals" -- sets
-- pending_push_cancelled = true for a sandbox that currently has one
-- outstanding (pending_push_suppressed_in_shadow IS NOT NULL), called by
-- the repo-demotion sweep (internal/app/seed) for every live sandbox of a
-- just-demoted repo. A no-op (pgx.ErrNoRows, the caller's own job to
-- treat as "nothing to cancel") when this sandbox has no push currently
-- outstanding -- there is nothing to cancel, and this must never
-- fabricate a pending_push_suppressed_in_shadow value that was never
-- resolved.
UPDATE sandboxes
SET pending_push_cancelled = true, updated_at = now()
WHERE session_id = $1 AND pending_push_suppressed_in_shadow IS NOT NULL
RETURNING *;

-- name: StartSandboxPRDelivery :exec
-- Technical plan §43.20 (migrations/000145_sandbox_pr_delivery_started_at.
-- up.sql): stamps the instant a completed turn's push, and the pull
-- request that follows it, began -- called by completeProcessingTurn
-- (app/sessionactor/pushpr.go) in the SAME transaction that completes the
-- turn, and only when a push will really be sent. now() is the database's
-- clock, the one GetSessionActivityFacts' observed_at is read from. A
-- later cycle's stamp overwrites an earlier one.
UPDATE sandboxes
SET pr_delivery_started_at = now(), updated_at = now()
WHERE session_id = $1;

-- name: EndSandboxPRDelivery :exec
-- Technical plan §43.20: the delivery StartSandboxPRDelivery stamped is
-- over -- its pull request's creation finished (createPRBestEffort, after
-- the artifact row is written), its push failed (push_error), or its push
-- command could not be sent. A no-op when nothing is outstanding.
UPDATE sandboxes
SET pr_delivery_started_at = NULL, updated_at = now()
WHERE session_id = $1 AND pr_delivery_started_at IS NOT NULL;

-- name: GetSandboxPRDelivery :one
-- Read by the stop timer's handler (technical plan §3.3,
-- sessionactor.deliveryHold) before it retires the sandbox gen a stopped
-- turn ran on: whether a completed turn's push and pull request, stamped
-- by StartSandboxPRDelivery, are still being delivered by this sandbox,
-- and which gen an earlier fire left a retirement owed on
-- (stop_retire_gen). observed_at is now() on the database's clock, the
-- clock that wrote the stamp and the one the timer pump compares fires_at
-- with, so neither the window's end nor the instant the stop timer is
-- re-armed for depends on the skew between the database and the replica.
SELECT status, gen, pr_delivery_started_at, stop_retire_gen, now()::timestamptz AS observed_at
FROM sandboxes
WHERE session_id = $1;

-- name: SetSandboxStopRetireGen :exec
-- Technical plan §3.3 (migrations/000151): the stop timer cancelled a
-- stopped turn in flight on gen with no word from the agent, and gen is
-- still delivering a completed turn's push and pull request, so its
-- retirement waits. Nothing is dispatched to gen until then
-- (sessionactor's planDispatch). Written only while the row is at gen.
UPDATE sandboxes
SET stop_retire_gen = sqlc.arg('gen')::integer, updated_at = now()
WHERE session_id = sqlc.arg('session_id') AND gen = sqlc.arg('gen')::integer;

-- name: ClearSandboxSnapshot :execrows
-- Technical plan §21.1 and §30.4: forgets the sandbox's snapshot, so the
-- next gen boots fresh instead of restoring it. The session actor clears
-- it in the transaction that retires a gen it will not send a review
-- turn's checkout to -- one whose agent does not advertise
-- capabilities.reviewCheckout, which a restore of the snapshot would bring
-- back -- or a gen whose checkouts keep failing on what its worktree holds,
-- which the snapshot holds too (reviewcheckout.go). The shadow bit describes
-- the snapshot, so it goes with it. 0 rows when there was none.
UPDATE sandboxes
SET snapshot_id = NULL, snapshot_suppressed_in_shadow = false, updated_at = now()
WHERE session_id = $1 AND snapshot_id IS NOT NULL;

-- name: ClearSandboxStopRetireGen :exec
-- Technical plan §3.3: the retirement SetSandboxStopRetireGen left owed is
-- done, or there is nothing left to retire (the sandbox is dead, or its
-- gen moved on). A no-op when none is owed.
UPDATE sandboxes
SET stop_retire_gen = NULL, updated_at = now()
WHERE session_id = $1 AND stop_retire_gen IS NOT NULL;

-- name: ListLiveSandboxesWithSessionRepos :many
-- §30.4's own repo-demotion sweep (internal/app/seed): every LIVE sandbox
-- (the SAME "live status" set ListLiveSandboxProviderIDs already defines
-- above), joined with its owning session's own raw repos JSONB column
-- (sessions.repos, migrations/000018_session_repos.up.sql) -- the sweep
-- parses this in Go (mirroring postgres.outboxShadow's own
-- sessionRepoFullNames, and app/sessionactor's own reposFromJSON/
-- rolloutDecisionForSession, this codebase's established "duplicate the
-- small per-package repo-JSON helper rather than share one" convention)
-- to decide which of these sandboxes belong to the just-demoted repo.
SELECT sandboxes.session_id AS session_id,
       sandboxes.provider_id AS provider_id,
       sessions.repos AS repos
FROM sandboxes
JOIN sessions ON sessions.id = sandboxes.session_id
WHERE sandboxes.status IN ('spawning', 'connecting', 'booting', 'ready', 'snapshotting', 'suspect');

-- name: MarkSandboxDemotionTerminationRequested :one
-- §30.4's own "demotion ... must terminate (or respawn) every sandbox of
-- the repo" (migrations/000108_sandbox_demotion_termination.up.sql):
-- stamped by the repo-demotion sweep (internal/app/seed) for every live
-- sandbox it finds belonging to a just-demoted repo. Read back, and acted
-- on, by app/reconciler.Reconciler's own new demotion-sweep tick.
UPDATE sandboxes
SET demotion_terminate_requested_at = now(), updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: ListSandboxesPendingDemotionTermination :many
-- app/reconciler.Reconciler's own new demotion-sweep tick reads every
-- sandbox row a repo-demotion sweep has flagged, so it can issue a real
-- ports.SandboxProvider.StopSandbox call for each -- mirrors this
-- reconciler's own existing orphan-reaping query
-- (ListLiveSandboxProviderIDs) in spirit, but scoped to rows an explicit
-- demotion flagged rather than every live row.
SELECT * FROM sandboxes WHERE demotion_terminate_requested_at IS NOT NULL;

-- name: ClearSandboxDemotionTerminationRequested :one
-- Consumes a sandbox's own demotion-termination request once
-- app/reconciler.Reconciler has successfully issued a real StopSandbox
-- call for it -- left set (so the very next tick retries) when that call
-- fails, mirroring this reconciler's own existing orphan-reap retry
-- precedent (ReconcileOnce's own doc comment).
UPDATE sandboxes
SET demotion_terminate_requested_at = NULL, updated_at = now()
WHERE session_id = $1
RETURNING *;

-- name: UpdateSandboxImageDecision :one
-- §19's own persisted-decision-provenance fix: records resolveAndSetImage's (imageresolve.go) own
-- per-spawn/-restore image-resolution outcome for THIS gen -- a real
-- warm-image selection (image_decision_reason = 'selected') or exactly
-- one of the closed set of fallback reasons
-- (internal/domain/imagedecision.Reason) that left plan.spec.Image at
-- defaultBaseImage instead. Called from its own small transact, separate
-- from planDispatch's (imageresolve.go's own "outside any transaction"
-- sequencing) -- a plain, best-effort write: a failure here is logged and
-- never blocks or fails the spawn (§10 Phase 2), mirroring
-- UpdateSandboxProviderID's own single-column-write shape exactly.
-- image_decision_fingerprint is nullable because ReasonNoRepos is the one
-- outcome with no fingerprint to compute at all (decideImage's own first
-- statement, imageresolve.go, an early return before any repos exist to
-- fingerprint).
UPDATE sandboxes
SET image_decision_reason = $2, image_decision_fingerprint = $3, updated_at = now()
WHERE session_id = $1
RETURNING *;

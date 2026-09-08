-- Step 21 ("e2e happy path") standalone additions. No single row in
-- docs/IMPLEMENTATION_PLAN.md explicitly names any of the columns below --
-- but end-to-end wiring genuinely cannot construct a real SessionConfig,
-- carry a turn's prompt across a pod restart, run the spawn circuit
-- breaker, or resume an OpenCode conversation across a respawn without
-- persisting them somewhere. Every addition here is small (one or two
-- columns) and scoped to an existing table, so they are folded into this
-- one migration rather than one file per column.

-- sessions.repos: the session's own repo list (§3.4: "position 0 =
-- primary; repos are always a list"), set once at session-creation time
-- (httpapi.CreateSession) and never mutated afterward -- a small, bounded,
-- session-scoped list, so a plain JSONB column is used rather than a
-- dedicated table (which would be premature for this shape of data).
-- Mirrors CreateSessionRequestReposElem's own shape ({branch, name, url}).
ALTER TABLE sessions ADD COLUMN repos JSONB NOT NULL DEFAULT '[]'::jsonb;

-- sessions.opencode_conversation_id: §3.3, verbatim: "The turn records the
-- OpenCode conversation id at turn start (also reported on every
-- heartbeat) so follow-up prompts on a fresh sandbox resume the same
-- conversation -- never lazily." This is deliberately a SESSION-level
-- column, not a turns one, even though turns.conversation_id already
-- exists (migrations/000005_turns.up.sql, Step 4) -- a "heartbeat" event
-- is a sandbox-level liveness signal, not a turn-scoped one (sandboxws.
-- Heartbeat carries no turn id at all, per contracts/sandbox-ws/v1), so it
-- can legitimately arrive with no turn Processing to attribute it to.
-- turns.conversation_id stays exactly as unused as it already was before
-- this Step; this column is the one this Step's own dispatch logic
-- (internal/app/sessionactor) actually reads from/writes to.
ALTER TABLE sessions ADD COLUMN opencode_conversation_id TEXT;

-- turns.prompt / model_id / plan_mode: the turn's own dispatch-time
-- inputs (design decision 2, docs/IMPLEMENTATION_PLAN.md row 21), read
-- back by internal/app/sessionactor's own EnsureDispatched handling to
-- build the outbound sandboxws.Prompt command -- persisted (not merely
-- held in memory) specifically so a pod restart mid-dispatch can still
-- correctly reconstruct what to send; this is exactly what makes the
-- resilience test's "fails-with-reason, no stuck processing" scenario
-- (§9.3 #1) a meaningful proof rather than a tautology. All three are
-- nullable/defaulted so every existing turns row, and every existing
-- test's `CreateTurnParams{SessionID, Status}` call site, remains valid
-- completely unchanged.
ALTER TABLE turns ADD COLUMN prompt TEXT;
ALTER TABLE turns ADD COLUMN model_id TEXT;
ALTER TABLE turns ADD COLUMN plan_mode BOOLEAN NOT NULL DEFAULT false;

-- sandboxes.provider_id: the provider's own opaque handle
-- (internal/app/ports.SandboxRef.ProviderID), recorded once CreateSandbox
-- actually succeeds. Read back as SpawnState.ProviderObjectID
-- (internal/domain/sandbox.EvaluateSpawnDecision's own persistent-resume
-- eligibility input) on a later spawn decision.
ALTER TABLE sandboxes ADD COLUMN provider_id TEXT;

-- sandboxes.spawn_failure_count / last_spawn_failure_at: the spawn
-- circuit breaker's own persisted state (internal/domain/sandbox.
-- CircuitBreakerState, §3.2: "3 permanent spawn failures within 5 min
-- blocks spawning"). Scoped to the one sandbox row it protects -- two
-- columns is not enough surface area to justify a separate table.
ALTER TABLE sandboxes ADD COLUMN spawn_failure_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN last_spawn_failure_at TIMESTAMPTZ;

-- workflows: Step 54's own ("domain/workflow + loopguard + schema",
-- §25.4/§25.5/§25.8) six-table schema for the configurable per-lane
-- workflow engine -- definitions/steps/edges (the authorable graph),
-- bindings (which definition a (lane, repo) resolves to), and
-- runs/step-runs (the execution ledger Steps 55-56 write). Everything
-- here is DARK as of this Step: no dispatch wiring, no behavior change,
-- no REST surface -- the engine (Step 55, §25.6) and the HITL gate
-- (Step 56, §25.9) are the first writers. Domain mirror:
-- internal/domain/workflow.
--
-- All closed vocabularies are real Postgres ENUMs, matching this
-- codebase's own automation_status/provider_credential_* precedent
-- (migrations/000051, 000056) for small, closed, code-controlled sets
-- -- never CHECK-constrained TEXT.
--
-- THE is_built_in IMMUTABILITY INVARIANT (§25.4), recorded here because
-- no API surface ships in this Step: PUT/DELETE on an is_built_in =
-- true workflow_definitions row is refused UNCONDITIONALLY -- even for
-- an admin. This is a STRUCTURAL invariant, not an RBAC rule (§25.11
-- gives it no matrix row on purpose); Steps 55-56 MUST enforce it at
-- the store/handler layer they add ("duplicate and customize -- never
-- in place, never delete"). A DB-level BEFORE UPDATE/DELETE trigger was
-- considered and deliberately NOT added: (a) it would be this repo's
-- first plpgsql anywhere under migrations/, and sqlc parses this whole
-- directory as its schema (sqlc.yaml) -- a function body is parser
-- risk with zero compensating protection while (b) no write path to
-- these tables exists at all this Step (dark), and (c) partial
-- protection already IS structural below: a definition referenced by
-- any binding or any run cannot be deleted (plain NO ACTION FKs), and
-- the three built-ins are born referenced by the three seeded global
-- bindings. The trigger question can be revisited by the Step that
-- first exposes a mutation surface, with a live threat model to weigh
-- it against.
--
-- Deliberately NO attempt-counter column anywhere in this file: §25.5
-- is explicit that loop iteration count is read back via COUNT(*) on
-- workflow_step_runs (each re-execution is its own row) -- the same
-- "derive it from the rows that already exist" discipline
-- review_verdicts' own DISTINCT ON reduction already applies. The
-- (workflow_run_id, step_definition_id) index below exists to make that
-- COUNT(*) cheap.

CREATE TYPE workflow_lane AS ENUM ('review', 'request', 'plan');
CREATE TYPE workflow_step_kind AS ENUM ('agent');
CREATE TYPE workflow_execution_scope AS ENUM ('same_session', 'child_session');
CREATE TYPE workflow_conversation_continuity AS ENUM ('continue', 'fresh');
CREATE TYPE workflow_step_outcome_status AS ENUM ('ok', 'needs_fix', 'blocked');
CREATE TYPE workflow_run_status AS ENUM ('running', 'needs_review', 'completed', 'failed', 'cancelled');
CREATE TYPE workflow_step_run_status AS ENUM ('awaiting_decision', 'running', 'completed', 'failed', 'cancelled');
CREATE TYPE workflow_step_decision AS ENUM ('approve', 'reject', 'revise');

-- workflow_definitions: one row per workflow (§25.4's
-- WorkflowDefinition, minus Steps). The three built-ins are ROWS, not
-- Go constants, because "duplicate and customize" and the canvas editor
-- (§25.12) both need the default to exist in exactly the same shape as
-- a custom workflow. version is a 1-based edit counter (provenance a
-- binding/run pins at activation/start time) -- NOT a versioned-content
-- archive; §25.4 models Version as a field OF the definition, never a
-- history side table.
--
-- The extra UNIQUE (id, lane) is deliberately redundant with the PK: it
-- is the composite-FK anchor workflow_bindings/workflow_runs below use
-- to make "a binding/run's lane always equals its definition's lane" a
-- structural guarantee instead of an application convention.
CREATE TABLE workflow_definitions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lane        workflow_lane NOT NULL,
    name        TEXT NOT NULL,
    is_built_in BOOLEAN NOT NULL DEFAULT false,
    version     INTEGER NOT NULL DEFAULT 1,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_definitions_name_nonempty CHECK (name <> ''),
    CONSTRAINT workflow_definitions_version_positive CHECK (version >= 1),
    CONSTRAINT workflow_definitions_id_lane_uniq UNIQUE (id, lane)
);

-- Exactly one built-in per lane (the three seeded system templates) --
-- a partial unique index, since custom (non-built-in) definitions may
-- multiply freely per lane.
CREATE UNIQUE INDEX workflow_definitions_built_in_lane_uniq
    ON workflow_definitions (lane)
    WHERE is_built_in;

-- Names are unique per lane -- constraint-over-convention: "duplicate
-- and customize" would otherwise silently accumulate identically-named
-- copies an admin cannot tell apart in a picker.
CREATE UNIQUE INDEX workflow_definitions_lane_name_uniq
    ON workflow_definitions (lane, name);

-- workflow_step_definitions: one row per step (§25.4's StepDefinition,
-- minus Edges). step_order (not "order": reserved word) is 1-based,
-- unique per definition, NOT required contiguous -- the domain's
-- default-advance rule is "smallest order strictly greater than the
-- current step's" (internal/domain/workflow.NextStep), well-defined
-- with gaps. model_id NULL means inherit exactly what the session would
-- use today (turns.model_id / sessions.build_model_id -- §25.8's
-- zero-config proof); non-null is the same "provider/model" passthrough
-- convention §25.1 verified end to end. prompt_template uses the
-- established "{{variable_name}}" placeholder syntax (§18.6,
-- internal/domain/intent.AssembleTemplate); '{{prompt}}' is the
-- caller's own text, making a passthrough step exactly that.
-- canvas_position is the §25.10 layout attachment: an OPAQUE JSONB blob
-- ({x, y}) the canvas editor (Step 91, §25.12) round-trips and the
-- server never interprets -- nullable because built-ins and
-- API-authored definitions have no layout until a canvas first saves
-- one.
--
-- UNIQUE (workflow_definition_id, id) is the composite-FK anchor
-- workflow_edges uses to pin both endpoints of an edge to the SAME
-- definition structurally (see below) -- redundant with the PK on
-- purpose, like workflow_definitions_id_lane_uniq above.
CREATE TABLE workflow_step_definitions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_definition_id  UUID NOT NULL REFERENCES workflow_definitions(id) ON DELETE CASCADE,
    step_order              INTEGER NOT NULL,
    kind                    workflow_step_kind NOT NULL DEFAULT 'agent',
    model_id                TEXT,
    prompt_template         TEXT NOT NULL,
    execution_scope         workflow_execution_scope NOT NULL DEFAULT 'same_session',
    conversation_continuity workflow_conversation_continuity NOT NULL DEFAULT 'continue',
    hitl_before             BOOLEAN NOT NULL DEFAULT false,
    hitl_after              BOOLEAN NOT NULL DEFAULT false,
    canvas_position         JSONB,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_step_definitions_order_positive CHECK (step_order >= 1),
    CONSTRAINT workflow_step_definitions_prompt_nonempty CHECK (prompt_template <> ''),
    CONSTRAINT workflow_step_definitions_model_id_shape CHECK (model_id IS NULL OR model_id <> ''),
    CONSTRAINT workflow_step_definitions_def_order_uniq UNIQUE (workflow_definition_id, step_order),
    CONSTRAINT workflow_step_definitions_def_id_uniq UNIQUE (workflow_definition_id, id)
);

-- workflow_edges: one row per explicit (from step, outcome) -> to step
-- routing rule (§25.4's Edge). on_status is the ONLY thing an edge may
-- condition on -- the closed 3-value outcome vocabulary, a DISTINCT
-- axis from review's Shippable (§25.4/§25.8), no expression language
-- (§25.12). With no explicit edge: ok advances to the next step in
-- order, needs_fix/blocked escalate (fail-conservative) -- a retry loop
-- (§25.9's audit->fix->audit) must be wired explicitly, never implied.
--
-- Both endpoint FKs are COMPOSITE, carrying workflow_definition_id:
-- (definition, step) -> workflow_step_definitions' own (workflow_
-- definition_id, id) unique pair -- so a cross-definition edge is
-- structurally unrepresentable, not merely invalid-by-convention
-- (internal/domain/workflow.ValidateDefinition rejects it too; this is
-- the constraint-over-convention backstop).
--
-- At most ONE edge per (from step, outcome): NextStep must be
-- deterministic, so (from_step_id, on_status) is unique -- mirroring
-- the domain's own ErrDuplicateEdge check.
CREATE TABLE workflow_edges (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_definition_id UUID NOT NULL,
    from_step_id           UUID NOT NULL,
    to_step_id             UUID NOT NULL,
    on_status              workflow_step_outcome_status NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_edges_from_step_fk FOREIGN KEY (workflow_definition_id, from_step_id)
        REFERENCES workflow_step_definitions (workflow_definition_id, id) ON DELETE CASCADE,
    CONSTRAINT workflow_edges_to_step_fk FOREIGN KEY (workflow_definition_id, to_step_id)
        REFERENCES workflow_step_definitions (workflow_definition_id, id) ON DELETE CASCADE,
    CONSTRAINT workflow_edges_from_status_uniq UNIQUE (from_step_id, on_status)
);

-- workflow_bindings: which definition (at which version) a (lane, repo)
-- pair resolves to. §25.4 names three DISTINCT concepts, not three
-- rungs of one fallback ladder:
--
--   - System template: a workflow_definitions row with is_built_in =
--     true. Read-only starting content; never itself a live setting.
--   - Global binding: the (lane, repo_full_name = NULL) row. Exactly
--     one per lane, SEEDED BELOW to point at that lane's system
--     template -- from then on an ordinary, independently-repointable
--     setting. Because it is seeded for every lane, this row is NEVER
--     absent: there is no "no binding configured" state to fail open or
--     closed on, and resolution is a two-step lookup with a guaranteed
--     second step (repo row if present, else the global row), never an
--     "absent row -> default" branch.
--   - Repo override: a (lane, repo_full_name = '<owner>/<repo>') row.
--     Optional; shadows the global binding for that one repo only.
--
-- repo_full_name reuses the "owner/repo" natural key
-- (repo_settings.repo_full_name's exact shape, migrations/000044) --
-- there is no standalone repos table anywhere in this codebase to
-- reference (provider_credentials.scope_target_id's own documented
-- finding, migrations/000056, which also anticipated this table's
-- "NULL means global" convention by name). Two partial unique indexes
-- rather than one plain UNIQUE, because a plain UNIQUE never collides
-- on NULL (the 000056 precedent, itself mirroring
-- automations_webhook_token_hash_uniq).
--
-- definition_version pins the definition's version AT
-- BINDING/ACTIVATION time (the Step-54 row's own "(repo_full_name,
-- lane) -> definition+version") -- provenance for "what was active
-- when", alongside workflow_runs' own start-time pin below.
--
-- The definition FK is COMPOSITE on (id, lane): a binding's lane always
-- equals its bound definition's lane, structurally. It is deliberately
-- NO ACTION (not CASCADE): deleting a still-bound definition must be
-- refused -- silently unbinding a lane would leave resolution pointing
-- at nothing, exactly the state §25.4 rules out.
CREATE TABLE workflow_bindings (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lane                   workflow_lane NOT NULL,
    repo_full_name         TEXT,
    workflow_definition_id UUID NOT NULL,
    definition_version     INTEGER NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_bindings_definition_lane_fk FOREIGN KEY (workflow_definition_id, lane)
        REFERENCES workflow_definitions (id, lane),
    CONSTRAINT workflow_bindings_version_positive CHECK (definition_version >= 1),
    CONSTRAINT workflow_bindings_repo_nonempty CHECK (repo_full_name IS NULL OR repo_full_name <> '')
);

CREATE UNIQUE INDEX workflow_bindings_repo_uniq
    ON workflow_bindings (lane, repo_full_name)
    WHERE repo_full_name IS NOT NULL;
CREATE UNIQUE INDEX workflow_bindings_global_uniq
    ON workflow_bindings (lane)
    WHERE repo_full_name IS NULL;

-- workflow_runs: one row per execution of a bound workflow against a
-- session (Step 55 is the first writer). lane + definition + version
-- are pinned at start time as provenance (the composite (id, lane) FK
-- again guarantees lane agreement; NO ACTION again means a definition
-- with recorded history is not deletable -- fail-conservative, history
-- outlives configuration). status vocabulary is fixed HERE; the owning
-- transition table (§11: every state transition through the machine's
-- table) ships WITH the engine in Step 55 -- dark schema now, machine
-- later, exactly like turn_status existed before its consumers.
-- 'needs_review' is §25.9's escalation parking state (circuit breaker
-- tripped or an unrouted needs_fix/blocked outcome): non-terminal, one
-- notice, waiting on a human.
--
-- At most one RUNNING run per session -- mirrors
-- turns_one_processing_per_session (migrations/000005) exactly: §25.6's
-- execution model is strictly sequential turns on one session.
-- Deliberately scoped to 'running' alone, NOT 'needs_review': a run
-- parked for a human decision must not freeze the session against new
-- work in the meantime (§25.9's escalation is "one notice ... stop",
-- not "hold the session hostage") -- so a successor run may start while
-- an escalated one still awaits its human.
--
-- No current_step column: derivable from workflow_step_runs (the
-- "derive it from the rows that already exist" discipline, same as the
-- COUNT(*) note in this file's header).
CREATE TABLE workflow_runs (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id             UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lane                   workflow_lane NOT NULL,
    workflow_definition_id UUID NOT NULL,
    definition_version     INTEGER NOT NULL,
    status                 workflow_run_status NOT NULL DEFAULT 'running',

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,

    CONSTRAINT workflow_runs_definition_lane_fk FOREIGN KEY (workflow_definition_id, lane)
        REFERENCES workflow_definitions (id, lane),
    CONSTRAINT workflow_runs_version_positive CHECK (definition_version >= 1)
);

CREATE INDEX workflow_runs_session_id_idx ON workflow_runs (session_id);
CREATE UNIQUE INDEX workflow_runs_one_running_per_session
    ON workflow_runs (session_id)
    WHERE status = 'running';

-- workflow_step_runs: one row per ATTEMPT of one step within a run --
-- a re-execution (retry edge, HITL revise) is a NEW row, never an
-- update-in-place, precisely so §25.5's COUNT(*) iteration read works
-- (see this file's header; the (workflow_run_id, step_definition_id)
-- index below serves that COUNT and the "latest attempt" lookups).
--
-- turn_id links the ordinary turn this attempt dispatched as (§25.6:
-- "every step is an ordinary sequential turn"), nullable because an
-- awaiting_decision (hitl_before-gated) attempt exists before any turn
-- does -- plain REFERENCES turns(id) with no action, exactly
-- plans.turn_id's own shape (both sides cascade away together via the
-- session).
--
-- outcome_status/outcome_summary/outcome_payload persist §25.6's typed
-- step-outcome posting ({status, summary (advisory, never re-parsed),
-- structuredPayload}) -- all NULL until the attempt's own outcome is
-- posted.
--
-- decision/decision_text/decided_at/decided_by persist §25.9's HITL
-- verdict on an awaiting_decision attempt (approve/reject/revise;
-- decision_text is the revise instruction folded into the NEXT
-- attempt's re-execution, never a substitution) -- decided_at/
-- decided_by mirror plans' own decided columns (migrations/000034)
-- exactly, ON DELETE SET NULL included.
--
-- step_definition_id is NO ACTION: an attempt's history pins the step
-- it ran, so a step definition that has EVER run is not deletable
-- (same fail-conservative stance as workflow_runs' definition FK).
--
-- At most one LIVE attempt per run -- the §25.4/§25.12 execution model
-- is ordered steps, strictly sequential, no parallelism: structurally
-- enforced, mirroring workflow_runs_one_running_per_session above.
CREATE TABLE workflow_step_runs (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_run_id    UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    step_definition_id UUID NOT NULL REFERENCES workflow_step_definitions(id),
    turn_id            UUID REFERENCES turns(id),
    status             workflow_step_run_status NOT NULL DEFAULT 'running',

    outcome_status  workflow_step_outcome_status,
    outcome_summary TEXT,
    outcome_payload JSONB,

    decision      workflow_step_decision,
    decision_text TEXT,
    decided_at    TIMESTAMPTZ,
    decided_by    UUID REFERENCES users(id) ON DELETE SET NULL,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX workflow_step_runs_run_step_idx
    ON workflow_step_runs (workflow_run_id, step_definition_id);
CREATE UNIQUE INDEX workflow_step_runs_one_live_per_run
    ON workflow_step_runs (workflow_run_id)
    WHERE status IN ('awaiting_decision', 'running');

-- ---------------------------------------------------------------------
-- Seed: the three built-in workflows (§25.4: "seeded is_built_in = true
-- directly in the migration -- fixed system data, not a per-install
-- seeding script") in exactly §25.8's zero-config shapes, plus the
-- three global bindings pointing at them.
--
-- Fixed, well-known UUIDs rather than gen_random_uuid(): the seed's own
-- later statements (steps -> definitions, edge -> steps, bindings ->
-- definitions) must reference these rows, and stable system-row
-- identities across every install beat per-install randomness for
-- fixed system data (the same reasoning that gave prompt_templates'
-- seed a stable natural key, migrations/000033). The constants below
-- are valid v4-shaped UUIDs in a reserved-looking 00000000-... block no
-- generated id will ever collide with.
--
--   review  definition 00000000-0000-4000-8000-000000000001, step ...-0011
--   request definition 00000000-0000-4000-8000-000000000002, step ...-0021
--   plan    definition 00000000-0000-4000-8000-000000000003, steps ...-0031/-0032,
--           edge ...-0041
--   global bindings ...-0051 (review) / ...-0052 (request) / ...-0053 (plan)
--
-- review/request (§25.8): ONE step each, model_id NULL (inherit
-- turns.model_id/sessions.build_model_id exactly as today),
-- prompt_template '{{prompt}}' (a passthrough render of the caller's
-- own text -- Step 55's zero-config proof diffs the resulting
-- sandboxws.Prompt against the old direct path for strict equality),
-- same_session/continue, no HITL, no edges. Shippable stays a separate
-- axis consumed after the review step completes -- never routed through
-- an edge (§25.8).
--
-- plan (§25.8): TWO steps (plan -> build), hitl_after on step 1
-- (reusing ApproveKeywords/RejectKeywords/RevisePrefix unchanged, Step
-- 56), plus the ONE explicit edge: needs_fix on step 1 loops to step 1
-- -- the revise re-execution loop, explicitly exempted from the circuit
-- breaker (§25.8/§25.9; the exemption lives in the engine's loopguard
-- consultation, not in this schema).
INSERT INTO workflow_definitions (id, lane, name, is_built_in, version) VALUES
    ('00000000-0000-4000-8000-000000000001', 'review',  'review',  true, 1),
    ('00000000-0000-4000-8000-000000000002', 'request', 'request', true, 1),
    ('00000000-0000-4000-8000-000000000003', 'plan',    'plan',    true, 1);

INSERT INTO workflow_step_definitions
    (id, workflow_definition_id, step_order, kind, model_id, prompt_template, execution_scope, conversation_continuity, hitl_before, hitl_after)
VALUES
    ('00000000-0000-4000-8000-000000000011', '00000000-0000-4000-8000-000000000001', 1, 'agent', NULL, '{{prompt}}', 'same_session', 'continue', false, false),
    ('00000000-0000-4000-8000-000000000021', '00000000-0000-4000-8000-000000000002', 1, 'agent', NULL, '{{prompt}}', 'same_session', 'continue', false, false),
    ('00000000-0000-4000-8000-000000000031', '00000000-0000-4000-8000-000000000003', 1, 'agent', NULL, '{{prompt}}', 'same_session', 'continue', false, true),
    ('00000000-0000-4000-8000-000000000032', '00000000-0000-4000-8000-000000000003', 2, 'agent', NULL, '{{prompt}}', 'same_session', 'continue', false, false);

INSERT INTO workflow_edges (id, workflow_definition_id, from_step_id, to_step_id, on_status) VALUES
    ('00000000-0000-4000-8000-000000000041', '00000000-0000-4000-8000-000000000003', '00000000-0000-4000-8000-000000000031', '00000000-0000-4000-8000-000000000031', 'needs_fix');

INSERT INTO workflow_bindings (id, lane, repo_full_name, workflow_definition_id, definition_version) VALUES
    ('00000000-0000-4000-8000-000000000051', 'review',  NULL, '00000000-0000-4000-8000-000000000001', 1),
    ('00000000-0000-4000-8000-000000000052', 'request', NULL, '00000000-0000-4000-8000-000000000002', 1),
    ('00000000-0000-4000-8000-000000000053', 'plan',    NULL, '00000000-0000-4000-8000-000000000003', 1);

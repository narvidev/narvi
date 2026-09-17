# Open decisions

Decisions this design cannot take for itself, and where each one is recorded.

## What this file is, and what it deliberately is not

It is an **index**, not a second copy. Where a decision belongs to a Step row or a technical-plan
section, that row or section stays authoritative and carries the full reasoning; this file holds a
pointer and one line of context. Copying the reasoning here would produce two records of the same
decision, free to drift apart — the failure this repository has now named in three separate places
and fixed in three more.

The exception is the last table. Those decisions have no row yet, precisely because taking them is
what would create one, so this file holds them in full until it does not have to.

**Not decided and decided-to-defer are different states.** A decision sitting here untouched is the
first. Recording a deferral is itself a decision, and it moves the entry to *taken* with `defer` as
its outcome and a reason — which is a materially different thing from an entry nobody has read. The
whole point of a register is that the difference stays visible; an open item that quietly reads as
"we're fine without it" is the same shape as an unassessed review rendering as a clean one.

Nothing here is a commitment to build. A decision to reject is as complete an outcome as a decision
to adopt, and an entry that leaves with `reject` and a reason has done its job.

## Taken, recorded in the row that owns them

| Decision | Outcome | Recorded in |
|---|---|---|
| How `sandbox-agent` stops running git against a runtime-owned `.git` | The separate git-dir — the only one of three shapes measured against real git rather than reasoned about | Step 171 |
| How a compromised sandbox token is revoked | Explicit operator revocation, the only form that is true without network reachability | Step 152 |
| How the bot token's egress is scoped | Its own independent required-set entry, never folded into another | Step 153 |
| What a plan document reads from | The `plan_documents` snapshot is the read; recomputation is the fallback, not the reverse | Step 161 |
| Whether this project takes an RWX account | **Yes** (2026-09-17). The real-binary test loses its skip, so `Resume` is settled empirically — Step 57's first exit criterion, never once run. Unblocks Phases 17 and 18 | Step 163 |
| Mode B: kill, defer or build | **Build** (2026-09-17), in the separate repository, where it is already specified as Steps E5-E7. Building it in this repository was never one of the three. Step 107's baseline readout stays the reference input and its window keeps accruing; the decision was taken without waiting for it, deliberately rather than by oversight | Step 108 |
| Whether to expose this system to existing MCP clients | **Adopt** (2026-09-17). Independent of any native-client decision, and the bulk of the cost is an authentication surface Step 158's device flow does not provide | Step 180 onward |
| Whether a newly created automation can reach its secrets | **Adopt** (2026-09-17). Taken together with the line below rather than left separate | Step 184 |
| Whether the `automation` secret scope is completed | **Adopt** (2026-09-17). It has been schema-only since migration 000090 -- the column shipped so the follow-up would need no second migration, and this is that follow-up: CRUD at the scope, and candidate resolution in the sandbox delivery path. The register had named this a separate decision; it was taken with D-02 instead, and the row says so | Step 184 |
| How automation env vars reach a session | **Real process environment** (2026-09-17), not only the prompt preamble they are today. The generic `cmd.Env` mechanism already exists and carries two sources; this is a third contributor on a built path, and the append order it joins has to be decided rather than inherited. The preamble stays -- knowing a value and resolving it in a shell are different needs | Step 187 |
| Whether browser-side errors and user feedback are captured | **Adopt** (2026-09-17). Server traces do not cover browser failures; reading them as coverage would be a capability claimed by adjacency | Step 185 |
| Whether session replay is adopted | **Adopt** (2026-09-17), decided separately from the line above because it is a choice about other people's data. Masking, activation and per-deployment data destination are all open inside the Step | Step 186 |

## Deferred — and what reopens each

A deferral is a decision, so it leaves the open list. It is **not** a closed decision, so it does not
go in the table above: a row under a heading that says *Taken* reads as settled, and nothing would
ever look at it again.

**Every entry names a condition someone can evaluate.** That is the whole content of the section. A
deferral without one is indistinguishable from an item nobody got round to, which is the state this
register exists to make impossible.

`internal/ops`'s `TestDeferredDecisionsNameATrigger` fails the build when an entry here has no
reopen condition. **What that check cannot do, stated so nobody reads it as more than it is: it
cannot tell whether a condition has already fired.** It stops a deferral being recorded without a
trigger; it does not watch the world. Evaluating these is a human act, and the natural moment is
whenever this file is opened to record a new decision.

| Decision | Deferred | Reopens when |
|---|---|---|
| A platform-supplied plugin mode (D-03) | 2026-09-17 | A repository needs a platform-pinned tool that the sandbox's own runtime configuration cannot supply. The cost that made deferral easy is the half that gets forgotten: convergence after restore — installing what is expected **and removing** what survives inside a snapshot (§35.5b) |
| Deleting a prepared medium against its quota (D-04) | 2026-09-17 | A slot quota is actually wanted. It is a different resource from a byte quota, and whoever reopens this must name bytes, slots, video duration and cancelled captures together — a design naming two of the four is wrong at the boundary |

### Deferrals that live in a plan row

These were deferred before this register existed, each inside the row that owns it. The rows stay
authoritative; this index exists because a deferral recorded only where it was found is one nobody
goes looking for. Read the row for the reasoning and the condition.

| Subject | Row |
|---|---|
| Warm boot: dependency-work reduction, the parts not in the first landing | Step 43 |
| Handoff-readiness sentinel, the deferred half | Step 49 |
| Review: learned false-positive patterns | Step 63 |
| Review triage: deterministic light/deep routing | Step 68 |
| Sandbox secrets and OpenCode config | Step 72 |
| Shadow operator surface | Step 104 |
| The embeddings provider as an un-closed egress channel — **no longer deferred**, see the mode B decision above | Step 156 |
| The `automation` secret scope, schema-only since migration 000090 — **no longer deferred**, taken with D-02 above. It was recorded only in that migration's comment and in `internal/domain/automation/doc.go`, findable from neither | Step 184 |

§31.9 carries its own block of deliberate deferrals for the knowledge capability, each surfacing at
its own Step rather than silently defaulting. That block is the authority for those; it is named
here so the set is findable from one place.

## Open, owned by a row that already names them

Each of these is stated as an open question inside the row that needs it, with its options and what
it gates. They are listed here so the set is visible in one place, not so it is described twice.

| Decision | What it gates | Stated in |
|---|---|---|
| Which cluster, and whether its node pool has the hypervisor capability — that acceptance run is what decides Kata against gVisor, not a preference | Phase 18 entirely | Step 167 |
| Whether a trigger consults a stack's direct parent or its ultimate target | The stack policy's incremental diff, implemented by Step 142 | §24.8 — the row itself does not raise it |
| Whether an external client is built at all | Phase 16, which is gated rather than scheduled | Phase 16 preamble |

**What the build decision on mode B turns on.** Step 156 stops being conditional: a hosted
embeddings provider receiving customer-derived prose is a real egress channel, and the wire-compatible
adapter that lets a self-hoster serve the same corpus is now required work rather than deferred work.
Steps 109 and 110 also come back into play — both need Step 108's corpus tables, and both were dead
under a kill. Phase 9's exit is written as a disjunction precisely so neither branch reads as the
only outcome; the build branch is the one that carries more work, not less.

## Open, with no home until they are taken

Raised by the documentation-gap inventory under `docs/analyses/`. Each is a capability an
upstream-parity analysis found and this design does not have. **That an analysis found something is
not an argument for adopting it** — these are listed as questions, with what each answer costs,
because the alternative is that they disappear from tracking and get rediscovered in a year as
though they were new.

### D-01 — Expose this system to existing MCP clients — **ADOPTED 2026-09-17**

Recorded here until its Step rows exist; once they do, they become authoritative and this entry
keeps only the pointer. The reasoning below is what the decision was taken against, and the
rejected alternatives are kept so they are not re-proposed as improvements: deferring (cheap only
if Phase 16's three prerequisites are built client-agnostically, and the real risk of deferral was
never the delay but building them FOR one client and encoding its assumptions), and rejecting
(which would have left Phase 16 as Step 157 alone).

**The question.** Whether an external MCP client may drive sessions here: discover repositories and
models, read session state, delegate work, send a prompt, approve or reject a plan, stop a run.

**Why it cannot be defaulted.** Phase 16 already describes three prerequisites — stable contracts,
native-client authentication, a resumable event stream — and none of them is this. The MCP
mentions in §27 run the other way: servers *consumed* from a sandbox's own runtime configuration.
Reading those as coverage would be a capability claimed by adjacency, which is the failure Phase 17
exists to correct.

**What adoption costs.** A separate authentication story is the bulk of it. Step 158's device flow
is for a native client and does not extend to third-party clients by itself: an OAuth surface needs
discovery, client registration, consent, refresh and revocation, plus scopes that gate both tool
invocation *and* tool discovery — a client that cannot use a tool should not be told it exists. It
must stay distinct from both the model-provider OAuth and the deployment's own secrets. Beyond
that: a compact session state with a suggested poll interval, a bounded wait that distinguishes
queued from running from awaiting-approval from finished (a non-empty queue must never render as
idle), and plan approval keeping every permission check it has on the existing paths.

**What deferral costs.** Little that compounds, provided the three Phase 16 prerequisites are built
without assuming a single client. The risk is building them *for* one client and discovering later
that they encode its assumptions.

**Independent of** any decision about a native desktop client. These are two questions that look
like one.

### D-02 — Reach secrets directly from a newly created automation — **ADOPTED 2026-09-17** — see Step 184

**The question.** Whether creating an automation offers a direct path to the secrets it will need,
and whether personal settings are visually and permissively distinct from deployment settings.

**Why it cannot be defaulted.** The existence of a Settings area and a secrets store is not this
feature, and presenting it as already covered is the precise thing to avoid.

**What adoption costs.** If retained, it reuses the existing secret scopes. A scope meaning "secret
belonging to one automation" would be an extension of the model and a **separate** decision — worth
naming now so it is not smuggled in as an implementation detail of this one.

### D-03 — A platform-supplied plugin mode — **DEFERRED 2026-09-17** — see the Deferred table above for what reopens it

**The question.** Whether the platform supplies plugins to a sandbox's runtime, enabled explicitly
per repository.

**Why it cannot be defaulted.** That a developer uses such a tool locally establishes nothing about
a product capability here, and no mandatory dependency follows from the comparison.

**What adoption costs.** Pinned versions and an update policy, plus a convergence rule after
restore: install what is expected *and* remove installations or capabilities that survive inside a
snapshot but should no longer be enabled. The second half is the one that gets forgotten, and it is
the same shape as §35.5b — a snapshot carries what it was minted with, whatever the current
configuration says.

### D-04 — Delete a prepared medium and reclaim its quota — **DEFERRED 2026-09-17** — see the Deferred table above for what reopens it

**The question.** Whether a medium that is already prepared can be explicitly deleted with its
quota returned, and whether a capture tool would show its limits before a capture starts.

**Why it cannot be defaulted.** Per-file and per-session upload ceilings, confirmation control and
abandoned-upload cleanup already exist. A slot quota is a different resource from a byte quota and
must not be assimilated into the upload system without a decision.

**What adoption costs.** The relation between bytes, any slot count, video duration, and what
happens to each on a failed or cancelled capture — four quantities that interact, and a design that
names only two of them will be wrong at the boundary.

### D-05 — Browser-side error capture, user feedback, and session replay — **ADOPTED 2026-09-17, both halves** — errors and feedback at Step 185, replay at Step 186

**The question.** Whether front-end error capture and user feedback complement the existing
OTel/OTLP foundation — and, **separately**, whether session replay is adopted.

**Why it cannot be defaulted.** Server traces and metrics do not cover browser-side failures; the
existing instrumentation is not partial coverage of this, it is coverage of something else.

**What adoption costs.** For errors and feedback: a configurable endpoint, source maps, and
association with the correct release — a stack trace that cannot be symbolicated against the
release it came from is a diagnostic that reads as present and is not. For replay: masking, an
activation decision, and where the data goes per deployment. A shared collection project and
unmasked replay are not defaults to inherit; they are choices, and the second one is a choice about
other people's data.

## Recording an outcome

Move the entry out of the last table and into *Taken*, with the outcome, one line of reasoning, and
the options rejected. If the decision creates work, the Step row becomes authoritative and this file
keeps only the pointer. Where a decision is deferred or rejected, it stays here with that outcome —
a rejected decision that vanishes is one that gets proposed again.

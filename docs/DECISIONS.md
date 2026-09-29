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
| How `objects` is shared between the agent-owned git-dir and the runtime's own `.git` | A directory symlink, not `objects/info/alternates` — alternates was measured and rejected: an agent-written object never became visible to the runtime's own git through it, and this design needs both directions to stay pushable/committable from either side | Step 171 |
| Whether `packed-refs`/`index` symlinks survive git's own lockfile-based writes (`pack-refs --all`, `gc`, a packed stash's own pop) | Yes, measured directly — git's own lockfile machinery resolves the symlink and renames the new content over the TARGET it points at, never replacing the symlink itself, so the agent-owned link is never orphaned | Step 171 |
| How a compromised sandbox token is revoked | Explicit operator revocation, the only form that is true without network reachability | Step 152 |
| How the bot token's egress is scoped | Its own independent required-set entry, never folded into another | Step 153 |
| What a plan document reads from | The `plan_documents` snapshot is the read; recomputation is the fallback, not the reverse | Step 161 |
| Whether this project takes an RWX account | **Yes** (2026-09-17). The real-binary test loses its skip, so `Resume` is settled empirically — Step 57's first exit criterion, never once run. Unblocks Phases 17 and 18 | Step 163 |
| Mode B: kill, defer or build | **Build** (2026-09-17), in the separate repository, where it is already specified as Steps E5-E7. Building it in this repository was never one of the three. Step 107's baseline readout stays the reference input and its window keeps accruing; the decision was taken without waiting for it, deliberately rather than by oversight | Step 108 |
| Whether to expose this system to existing MCP clients | **Adopt** (2026-09-17). Independent of any native-client decision, and the bulk of the cost is an authentication surface Step 158's device flow does not provide | Step 180 onward |
| Whether a newly created automation can reach its secrets | **Adopt** (2026-09-17). Taken together with the line below rather than left separate | Step 184 |
| Whether the `automation` secret scope is completed | **Adopt** (2026-09-17). It has been schema-only since migration 000090 -- the column shipped so the follow-up would need no second migration, and this is that follow-up: CRUD at the scope, and candidate resolution in the sandbox delivery path. The register had named this a separate decision; it was taken with D-02 instead, and the row says so | Step 184 |
| How automation env vars reach a session | **Real process environment** (2026-09-17), not only the prompt preamble they are today. The generic `cmd.Env` mechanism already exists and carries two sources; this is a third contributor on a built path, and the append order it joins has to be decided rather than inherited. The preamble stays -- knowing a value and resolving it in a shell are different needs | Step 187 |
| What a check reports for a review that did not complete | **`action_required`** (2026-09-18). The only conclusion whose GitHub semantics match the truth: it does not satisfy a required check, and it tells a human an action is expected. Rejected `failure` (blocks, but lies by excess) and a permanent `in_progress` (honest, but indistinguishable from a crashed worker) | Step 174 |
| Whether a repository may make the review check required | **Yes** (2026-09-18), with a `queued` check published as soon as a PR enters scope, before any review starts -- otherwise a PR nobody triggered a review on carries no check at all and becomes mergeable by absence | Step 174 |
| What the check says when enforcement is off | **Exactly what it says when enforcement is on** (2026-09-18). A check that gates nothing is still a statement to every human reading the pull request | Step 174 |
| Whether browser-side errors and user feedback are captured | **Adopt** (2026-09-17). Server traces do not cover browser failures; reading them as coverage would be a capability claimed by adjacency | Step 185 |
| Whether session replay is adopted | **Adopt** (2026-09-17), decided separately from the line above because it is a choice about other people's data. Masking, activation and per-deployment data destination are all open inside the Step | Step 186 |
| Whether `ProviderFailureDiagnostic.Message` (§7.3) retains a provider's own error text verbatim, even when the provider echoes request content or a masked credential fragment back into it | **Retained verbatim** (2026-09-22). Less diagnostic information is the wrong direction for an allowlist built specifically so a support conversation with the provider is possible at all; the plan row's own "neither credentials nor request content" exit criterion is met a different way — containment is JSON-string encoding at the wire/journal boundary (`json.Marshal`'s default HTML-escaping, `translate.go`), never redacting or truncating text on suspicion, since Message is untrusted PROVIDER output, not this adapter's own construction. `buildProviderFailureDiagnostic` (internal/adapters/outbound/opencode/diagnostic.go) copies `err.Data.Message` unfiltered; `hostileProviderMessage` (internal/adapters/outbound/opencode/diagnostic_test.go) is the regression test pinning it, exercised through the same real json.Unmarshal→build→marshal path the credential-allowlist test uses. Reverses if a real provider is ever observed constructing an error message FROM raw provider-side secret material it should not have echoed at all, rather than merely reflecting content already visible to the requester (a request header, a fragment of the prompt) back at them — that is a different, worse failure mode a verbatim policy cannot answer for | Step 177 |
| Whether this deployment runs a pool of GitHub Apps | **Adopt** (2026-09-25), without waiting for a measured saturation. Justified in this design by roles one credential cannot hold (a check run can only be created by an App, and the one App this deployment mints for must stay read-only for §30.4), by rotation without an outage, and by one identity for each kind of write; adding Apps for rate-limit headroom is the operator's choice, recorded below | Phase 20 (Steps 190-197), §44 |
| Whether review quality is measured from a human's ruling on each finding (D-10) | **Adopt** (2026-09-28): a ruling per finding with a precision role per value, the measures recorded where they happen, and readouts that state their populations. Most of the mechanisms this needs already exist here; the defects found in them are Phase 11 rows | Steps 206-208, §21.5 |
| Whether a second agent runtime and provider accounts are adopted (D-11) | **Adopt** (2026-09-28), same request. A reading of the provider's terms on subscription credentials comes before any code (Step 209); a real turn on the real SDK in CI is the adapter's first exit criterion (Step 210); an account a user connects, subscription or API key, is used only where its owner selected it, never by automatic resolution | Steps 209-211, §45 |
| Whether Apps may be added to the pool for rate-limit headroom | **The operator's choice** (2026-09-28). The pool stays justified by roles; the runbook states the choice beside Section H of GitHub's Terms of Service, and the plan neither promises nor forbids it | Step 197, §44.1 |
| Which way an unreadable diff routes | **Deep** (2026-09-28). A missing size costs a thorough review, a missing scope costs a finding; §26.3's fail-open-to-light rule stays for the router's own errors | Step 199, §26.3 |
| Whether an unresolved review conversation blocks auto-merge eligibility | **Yes**, through a per-repository setting on by default (2026-09-28), so repositories without GitHub's conversation-resolution rule are covered too; a thread this system started with no human reply does not count | Step 215, §21.2 |
| Whether a train may be replanned | **Yes, on a human's explicit request** (2026-09-28): only links not yet started are replaced, started ones stay immutable, refused while a spawn is claimed | Step 147, §39.5b |
| What a session created over MCP records as its source | **`spawn_source = mcp`** (2026-09-28), set by the server from the MCP grant the /mcp authentication attaches, never from the request or the principal's credential type; opened among the contracts' open enums by a prior PR, then the migration. Rejected: recording it as `web`, which makes an MCP client's sessions indistinguishable from the browser's | Step 183, §43.1 |
| Whether the MCP surface is braked per grant (row 182's owner decision D5, which deferred it to row 183) | **Adopt** (2026-09-29), with the first tool that writes: `POST /mcp` is braked per grant after the bearer gate, a burst of 30 calls then one a second, answered `429` before anything runs; and `narvi_create_session` is braked per grant on its own, a burst of 5 calls then one a minute, a same-key retry counting as a call, since each session spends on models. Both are in memory, per replica, logged and never audited. Rejected: keying by client network, which brakes everyone behind one proxy as one; and no brake, which leaves nothing to bound a runaway client that can start sessions | Step 183, §43.6, §43.8 |
| Whether a narrowed review request may stand for the whole pull request (D-13) | **Never** (2026-09-28). One rule, recorded once, gives each verdict its kind from the turn it is attributed to; a follow-up mention is whole only when its text is exactly the re-run command, never by a model's reading; the mention that opens a review session is always a whole review, its text carried as a question; every reader reads whole verdicts only | Step 212, §21.1b |
| Whether a verdict's findings go inline on the diff (D-09) | **Adopt** (2026-09-28), in the same write as the formal review | Step 213 |
| Whether a stack layer may be re-reviewed as a delta when only its ancestors moved (D-12) | **Adopt, experimental** (2026-09-28): off by default, capped per period, run in a shadow session of its own beside the full review, which stays the review of record, with no effect but its record, and paired with it in the readout per instruction version; letting a delta replace the full review is a later decision taken on that readout | Step 214, §24.8 |
| What happens to a pending blocking review once a newer verdict exists | **It is posted as a comment**, its text kept visible as §21.1 keeps a stale result in history, and only once a newer WHOLE verdict has been recorded: a newer attempt that recorded no whole verdict, or a narrowed verdict, never removes a block (2026-09-28, following the review of this plan's own amendment, which had let it fail open) | Step 190, §21.1b |

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
| Triaging events from an unlinked/unauthorized GitHub or Linear actor via automation dispatch (D-06) | 2026-09-18 | Someone explicitly asks for "triage every new issue, including from outsiders" as a product capability **and** a separate, reviewed containment design exists for untrusted webhook payload text reaching an agent's prompt while that agent holds this deployment's repository credentials. This is not a flag flip: fail-closed is the ONLY thing standing between an anonymous internet actor and a sandboxed agent run today, and the untrusted-text-reaches-the-prompt path this batch traced (see PR body) is a prompt-injection surface, not a UX preference — containing it is separate work from authorizing the sender |
| A Linear automation can never fire on a machine-reported (non-`"user"`) actor (D-07) | 2026-09-19 | Linear publishes, or this deployment otherwise directly observes against a real delivery, a confirmed, exhaustive register of `actor.type` wire values for non-human actors (today `ClassifyLinearActorOrigin` (internal/domain/automation/dispatch.go) only confirms `"user"`) **and** that register is structural rather than sender-chosen — e.g. tied to the event's own category the way GitHub's `check_run`/`status` are, not to which intake path (an Integration, Zapier, an email-to-issue form) happened to file the same "Issue"/"Comment" category. Absent both, denying is the only sound default: see D-06 above for why substituting a weaker authorization path here was the bug, not a feature |
| A dispatch drop from a budget-exhausted delivery is visible, but not retried or reconciled (D-08) | 2026-09-19 | Inline webhook-request dispatch stops being this deployment's only live-dispatch path — e.g. an outbox-style deferred dispatch decoupling fan-out from the webhook request itself, mirroring how `Engine` (internal/app/automation/engine.go)'s own background pump already decouples invocation creation from fan-out. Until then, `recordAutomationDispatchDropped` (internal/app/automation/dispatchmetrics.go) is the mitigation, not the fix: it makes a permanent drop OBSERVABLE, it does not make dispatch retried, reconciled, or redelivered |
| `revalidateCore` has no release-cut gate (revalidate.go) | 2026-09-19 | A release-cut PR — accepted override or not — reaches `ok=true` through `RevalidateForMerge`/`RevalidateForAutoMerge` while its own persisted §15.2 manifest check (`release_manifest_checks`) still carries a non-zero findings count. Evaluable directly from that merge's own audit row plus the PR's manifest-check row; no new instrumentation required |
| A dispatch drop caused by `AuthorizeLinkedActorVerdict` (internal/app/actorauthz/authorize.go) failing under an expired dispatch budget, inside `dispatchOneGitHubAutomation`'s own machine-origin gate (internal/app/automation/githubdispatch.go), is not counted by `automation_dispatch_dropped_total` (internal/app/automation/dispatchmetrics.go) | 2026-09-19 | `AuthorizeLinkedActorVerdict` (internal/app/actorauthz/authorize.go) stops collapsing a budget-expired lookup and an ordinary transient lookup failure into the same `LinkedActorError` verdict, discarding the underlying error — e.g. by returning it, or reporting a distinct verdict for a context-deadline failure — so this call site could increment `automation_dispatch_dropped_total` (internal/app/automation/dispatchmetrics.go) for a genuine budget exhaustion without also mislabeling an ordinary lookup failure that merely races the same deadline, which is the risk this entry defers against rather than accepting silently |
| A failed turn's provider-failure diagnostic (§7.3) has no dedicated web rendering | 2026-09-22 | Timeline.tsx (or another web/ surface) needs to show a failed turn's root cause to a signed-in user without them reading the raw GET .../events response or the sandbox-ws replay payload by hand today, the outcome's own "reason" string is the only thing rendered, and the full allowlisted record (message, union member, status code, provider request id, model, runtime version, sandbox id) sits unread in the persisted event and the operator log alike. This defers only the dedicated UI treatment -- the record itself is already fully persisted and reachable both ways, satisfying §7.3's own "traced from the diagnostic to its cause" exit criterion at the data layer |
| `git push`'s own `http.<url>.proxy` vector is not closed (`githarden.go`, `cmd/sandbox-agent`'s `pushOneRepo`) | 2026-09-23 | A repository-authored `http.<the-exact-push-url>.proxy` still routes a push through an attacker-chosen proxy: `RemoteProxyArg` closes the sibling `remote.<name>.proxy` vector for push (the remote name is always known, from `sandboxws.Push.Repos[].Remote`), the same unconditional, remote-NAME-keyed guarantee `hardeningFlags`' own `remote.origin.proxy=` already gives every OTHER network-touching git invocation that contacts a remote by name (`gitclone`'s fetch/ls-remote helpers in `sync.go`, which target `origin`). `RepoURLProxyArg` closes the URL-keyed vector too, but only at `gitclone.cloneOne`, the one call site where the contacted url IS the invocation's own command-line argument rather than a name resolved through the repository's own runtime-owned config -- `sandboxws.Push.Repos[]` carries only name/branch/remote, never a url, and reading `remote.<name>.url` back from that same runtime-owned `.git/config` to build the override would be racy (the runtime could rewrite it between the read and the push) and circular (the value read could itself be the attacker's own rewrite). Reopens when `sandboxws.Push.Repos[]` gains a validated url of its own (mirroring `sessionconfig.SessionConfigReposElem`), or gitclone exposes a race-free, non-runtime-sourced way to know the exact url a given push targets -- whichever lands first lets `RepoURLProxyArg` close this the same way it already closes clone |
| Agent Substrate as what runs the Kubernetes provider's sandboxes, in place of pods this system manages itself (§42.9). AX above it is rejected there, not deferred | 2026-09-29 | Phase 18 opens, that is, work starts on its first Step, the adapter (§42.8): re-read Substrate's then-current API and roadmap against §42.9's list of costs before writing the adapter, since its API was renamed twice in the week of the study. Or earlier, if a deployment needs snapshot restore or in-place resume on its own cluster, the two capabilities §42's pod design reports `false` |

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
| Whether a provider's subscription credentials may drive automated sessions | Step 209's first exit criterion, before any code: a reading of the provider's terms, recorded with its date; API-key accounts only if they forbid it | §45.1, Step 209 |
| Which sessions may use the pool's `work` role, in particular whether a creator with no linked GitHub identity may push through a bounded App credential | Step 195's first consumer: until decided, the binding is built and exercised by tests only. Left open when OIDC sign-in shipped: such a creator cannot push until they link GitHub. The answer must also say how the push is bounded, since the sandbox never holds a `work` installation token | §44.4, Step 195 |

**What the build decision on mode B turns on.** Step 156 stops being conditional: a hosted
embeddings provider receiving customer-derived prose is a real egress channel, and the wire-compatible
adapter that lets a self-hoster serve the same corpus is now required work rather than deferred work.
Steps 109 and 110 also come back into play — both need Step 108's corpus tables, and both were dead
under a kill. Phase 9's exit is written as a disjunction precisely so neither branch reads as the
only outcome; the build branch is the one that carries more work, not less.

## Open, with no home until they are taken

Raised by the documentation-gap inventory under `docs/analyses/`. Each is a capability an
analysis found and this design does not have. **That an analysis found something is
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

### D-06 — Triage events from unlinked/unauthorized GitHub or Linear actors — **DEFERRED 2026-09-18** — see the Deferred table above for what reopens it

**The question.** An adversarial review of live automation dispatch (§8.4) found that ANY GitHub
account that can open an issue, post a comment, or open a fork PR on a watched public repository —
and, for Linear, any workspace whose deliveries are signed with this app's shared webhook secret —
could create an automation invocation, and therefore a sandboxed agent run holding this deployment's
own repository credentials, with no authorization check at all. The fix landed here is fail-closed,
on both paths, at the actor level: for GitHub, a HUMAN-originated event (pull_request, issues,
issue_comment, push) requires the event's own sender to already be a known, linked, authorized
identity; a MACHINE-originated event (check_run, status — GitHub itself is always the actor, never a
human account, so a sender-based check is structurally unsatisfiable for these two) instead requires
the automation's OWN creator to be that same known, linked, authorized identity — both reuse the SAME
`AuthorizeLinkedActor` (internal/app/actorauthz/authorize.go) gate the pre-existing @mention pipeline
already enforces, never a second, independently-invented check. For Linear, a human-origin actor (the
event's own top-level `actor` field reporting `"user"`, Linear's own "User, OAuth client, or
Integration" vocabulary) is resolved through `LookupLinkedUserID` (internal/app/identitylink/service.go)
— a pure, side-effect-free lookup, never `Resolve` (internal/app/identitylink/service.go)'s own
auto-linking algorithm the pre-existing AgentSessionEvent path still legitimately runs — then
authorized through the identical `AuthorizeLinkedActor` (internal/app/actorauthz/authorize.go) gate;
the sending workspace having an installation is tenant scoping, a separate, additional check, never a
substitute for authorizing the actor who acted. A non-"user" (machine-reported) Linear actor is denied
outright rather than authorized any other way — see D-07 below for that narrower, separate limitation
and its own reopen condition (`ClassifyLinearActorOrigin` (internal/domain/automation/dispatch.go)
names exactly what decides it). Fail-closed on an unauthorized/unlinked HUMAN actor, on both
providers, closes the hole this entry is about, but it also forecloses a legitimate product shape —
"triage every new issue, including one filed by someone who has never signed into Narvi" — that
fail-closed cannot express. Whether to build an opt-in for that shape is the open question this entry
defers.

**Why it cannot be defaulted.** Fail-closed was the correct FIRST fix for a HIGH-severity,
confirmed-exploitable gap — it is not the correct LAST word on what this deployment is allowed to
do. But an opt-in cannot be added by flipping a flag on the authorization check alone: an unlinked,
unauthorized actor's comment/issue body is untrusted text, and this same audit traced whether that
text already reaches an agent's own prompt on this path (see the PR body's own traceability finding).
Removing the authorization gate without ALSO containing that text is not "triage from outsiders" —
it is prompt injection with this deployment's repository credentials sitting on the other side of it,
which is a materially worse position than the one D2 fixed.

**What adoption costs.** A real containment design for untrusted webhook payload text reaching an
agent's prompt — sanitisation, a trust boundary the agent's own tool calls respect, or a narrower
capability set for a run whose triggering actor is unauthenticated — reviewed and built as its own
piece of work, not assumed as a side effect of relaxing D2's own gate. Until that exists, an opt-in
here is a second copy of the exact vulnerability this batch just closed, wearing a configuration flag
instead of a code path.

### D-07 — Fire a Linear automation on a machine-reported (non-`"user"`) actor — **DEFERRED 2026-09-19** — see the Deferred table above for what reopens it

**The question.** A second adversarial review of live automation dispatch (§8.4), this time of D-06's
own fix, found that D-06's Linear half had reopened the exact class of gap it closed: it decided
"human vs. machine origin" from `actor.type`, read by `buildLinearEventInput`
(internal/adapters/inbound/linear/automationdispatch.go) — a per-payload field the SAME "Issue"/
"Comment" event category carries either value for depending only on which intake path (a direct
sign-in, an Integration, Zapier, a customer-facing form, email-to-issue) happened to file it — not a
structural fact about the event the way GitHub's check_run/status are (`GitHubEventOrigin`
(internal/domain/automation/dispatch.go) is keyed on the event TYPE, which the sender never
supplies). The shipped fix, `ClassifyLinearActorOrigin` (internal/domain/automation/dispatch.go), now
denies a `LinearEventOriginMachine` actor outright — `dispatchAutomationsBestEffort`
(internal/adapters/inbound/linear/automationdispatch.go) returns without ever listing an automation —
rather than authorizing it through the automation's own creator the way `GitHubEventOriginMachine`
legitimately does. This entry defers the resulting functional gap: a genuinely machine-originated
Linear event can never fire an automation today, even one whose creator is fully authorized.

**Why it cannot be defaulted.** The creator-authorization substitute is sound for GitHub specifically
because `check_run`/`status` are structurally impossible to originate from a human account — there is
no sender identity that substitute could ever be bypassing. Linear has published no equivalent
structural signal, and this deployment has never observed a real, confirmed wire value for a non-
`"user"` `actor.type` (Linear's own docs name "OAuth client" and "Integration" as the two non-human
kinds without giving their exact strings) — a security decision rebuilt on an unobserved external
value would repeat the exact defect this entry exists to close. Denying is therefore the only sound
default until BOTH conditions in the Deferred table row above are met.

**What adoption costs.** A structural signal Linear actually publishes and this deployment can verify
against a real delivery — not a fresh guess at another payload field to key on, which is what
produced this defect the first time. Absent that, the honest, durable fix is the one already shipped:
deny, and name the gap here rather than paper over it with a second unverified field.

### D-08 — Retry or reconcile a budget-exhausted automation dispatch drop — **DEFERRED 2026-09-19** — see the Deferred table above for what reopens it

**The question.** A live webhook delivery matching more automations than
`AutomationDispatchTotalBudget` (internal/platform/timeouts.go) can fully retry, or hitting enough
per-call Postgres latency, drops the remaining matching automations' own dispatch entirely — logged
(`recordAutomationDispatchDropped` (internal/app/automation/dispatchmetrics.go), W4 audit fix's own
visibility half) but never retried: the webhook delivery's own claim is already taken
(`CreateInvocationForDelivery` (internal/app/automation/invocationenqueue.go)), so a provider
redelivery returns at the duplicate check before dispatch ever runs again, and nothing reconciles
`automation_invocations.source_delivery_id` against the set of automations that SHOULD have fired for
it. Whether to build a retry/reconciliation mechanism for this specific gap is the open question this
entry defers.

**Why it cannot be defaulted.** Dispatch runs inline, on the webhook request path, specifically so a
matching automation fires with the lowest possible latency and no separate worker to keep alive — see
`DispatchGitHubWebhookEvent` (internal/app/automation/githubdispatch.go)'s and `DispatchLinearWebhookEvent`
(internal/app/automation/lineardispatch.go)'s own doc comments. A design that must ALSO guarantee delivery for an unbounded number of matching
automations per webhook needs dispatch off the request path entirely (an outbox-style deferred
dispatch, mirroring how `Engine` (internal/app/automation/engine.go)'s own background pump already
decouples invocation creation from fan-out) — a materially larger architectural change than adding a
retry loop to the existing inline path, which could not by itself fix the ROOT cause (a request-path
call is bounded by the provider's own webhook delivery timeout, full stop).

**What adoption costs.** Either an outbox-style deferred-dispatch redesign (the honest fix, sized like
a new Step, not a patch), or, as a narrower interim step, a periodic reconciler comparing each active
automation's own trigger config against recent deliveries it should have matched — itself nontrivial,
since "should have matched" requires re-deriving the SAME trigger-matching decision
(`MatchesGitHubTrigger`/`MatchesLinearTrigger` (internal/domain/automation/trigger.go)) outside the
request path, against whatever delivery history this deployment retains. Until either exists,
`automation_dispatch_dropped_total` is the honest, shipped mitigation: a drop is now OBSERVABLE
(alertable via `AutomationDispatchDroppedAny`, deploy/observability/alerts/reliability.json), never
retried.

### D-09 - Publish a verdict's findings as inline comments on the diff - **ADOPTED 2026-09-28** - see Step 213

Raised on 2026-09-25.

**The question.** Whether a verdict's findings are also posted as inline review comments anchored to
the commit that was reviewed, and whether a finding this system already raised in an open thread of
its own is cited rather than posted a second time.

**Why it could not be defaulted.** Today the formal review carries a body and an event, and the body
is the verdict, with its findings in a collapsed appendix. Inline comments change what a reviewer
sees on the pull request and open threads that people answer, which this system then has to read
without ever overwriting a person's conversation.

**What adoption costs, as adopted.** The comments ride the formal review's own create call, so there
is one write and one receipt, rather than an inline review and a verdict comment written separately
with the partial state between them accepted; Step 190's create-once write is the
prerequisite. Anchoring on the commit actually reviewed, with a finding that cannot be anchored still
readable in the body. Citing an open thread decided from GitHub's live thread state, never from a
mirror of past webhooks, and never when a person has replied in it. And the review sandbox posts
nothing itself: every inline write is the server's.

### D-10 - Record a human's ruling on one finding, and measure review quality from it - **ADOPTED 2026-09-28** - see Steps 206-208

Raised on 2026-09-25.

**The question.** Whether a maintainer can rule on an individual finding (false positive, accepted
risk, policy disagreement, fixed later, duplicate, unverified) as a record of its own, distinct from the
finding's status; and whether review quality is measured from it: the nature of findings
(a demonstrated defect against missing coverage), the light or deep routing actually applied, what
the fact-check pass removed against what it saw, and how often consecutive verdicts repeat a finding.

**Why it cannot be defaulted.** Pieces exist: rebuttals keyed on finding identity (Step 48), learned
false-positive patterns (Step 63), acceptance of a whole verdict (Step 188), and the review depth
persisted with each verdict (the triage decision and its reason on the turn), with the fact-check
outcome. None of them is a ruling on one finding, and an
accepted risk is not a false positive: folding the two corrupts every precision number built on them.

**As adopted.** Steps 206-208 and §21.5 are authoritative: a ruling vocabulary kept apart from Step
188's acceptance, denominators recorded where they are observed, each finding's presence in each
verdict kept as history, and "not measured" kept distinct from zero on every view.

### D-11 - A second agent runtime, and provider accounts managed in the product - **ADOPTED 2026-09-28** - see Steps 209-211

Raised on 2026-09-25.

**The question.** Whether a session or an automation may run on a second agent runtime beside OpenCode
(one built on a provider's own agent SDK), selected per session or per automation, and whether a model
provider's accounts, including subscription sign-in, are managed as product objects.

**Why it cannot be defaulted.** The runtime port was designed to take a second adapter (§4.2), so the
question is not whether one can exist but what parity it must reach before a session may select it:
the review, stack and auto-fix tools this design gives its agents, credential selection, cancellation
and resume. The easy shapes are the wrong ones: any signed-in user administering the accounts, the
first connected account silently becoming the default for sessions that pinned none, a runtime
missing this design's own tools, and no check running a real turn on it.

**As adopted.** §45 and Steps 209-211 are authoritative. Parity is §45.2's list, declared as an allow
list and refused at dispatch when missing; an account a user connects, subscription or API key, is
used only where its owner selected it, never by automatic resolution (§45.1); one real end-to-end turn in CI; and a reading of the
provider's terms before any account code.

### D-12 - Re-review a stack layer as a delta when only its ancestors moved - **ADOPTED 2026-09-28, experimental** - see Step 214

Raised on 2026-09-28.

**The question.** When an ancestor of a stacked pull request moves and the layer's own patch is
unchanged, whether the replacement review may be a delta turn: a real review turn whose prompt
carries the last published whole verdict and the ancestors' diff, asked to assess first
whether that change can reach this layer and to review in full when it can or when unsure.

**Why it could not be defaulted.** Carrying the verdict forward with no turn when the files are
disjoint is the tempting shape, and it lets a contract changed in a file the layer does not touch
(`charge(amountCents)` becoming `charge(amountEuros)`) pass unseen. The delta turn
answers that objection, but only by instruction: nothing shows the model re-assesses every
interaction correctly. Nothing here re-reviews a layer on an ancestor's move today either: that
lifecycle is §24.8's membership routing, which Step 142 builds.

**As adopted.** Step 214 is authoritative: the delta review runs in a shadow session of its own beside
the full review, which stays the review of record, so neither reads the other's reasoning, nothing
waits on the experiment, the delta verdict has no effect but its record, and the pair is the
measurement;
eligibility read from the last whole verdict's attempt, never the last launch; an identical own patch
by fingerprint; readable, bounded ancestor diffs; a cap per period; the mode and
its instructions carried in the verdict's provenance rather than exempted from the policy version. Letting a delta verdict replace the full review is a decision of
its own, taken later on the paired readout.

### D-13 - A review asked about part of a pull request, and the verdict that covers all of it - **DECIDED 2026-09-28: never** - see Step 212

Raised on 2026-09-28.

**The question.** A mention can narrow what a person wants reviewed ("review only the transfer
method"), and each mention here dispatches a review turn whose verdict joins the pull request's
history like any other. Whether such a verdict may become the one §21.1b reads for eligibility, or
whether a narrowed request is answered without replacing the verdict that covers the whole change.

**Why it could not be defaulted.** A verdict's context records head, base, ancestors and policy, and
the attempt beside it, but not a scope; and several readers decide by attempt time, so a focused
verdict would replace the whole one as the current verdict.

**As decided.** Step 212 is authoritative. One rule, applied once by the verdict endpoint and
recorded, gives a verdict the kind of the turn it is attributed to: whole for a review attempt
(`turns.is_review_attempt`) that is not a delta turn, delta for a delta turn, narrowed otherwise; the
kind stays out of the context so two attempts on identical code still share it. A follow-up mention is
a review attempt only when its whole text after the handle is the re-run command a verdict
recommends, never by a model's reading. The mention that opens a review session is always a whole
review, its text carried as a question answered beside it, which is the one case that could otherwise
let a first "review only X" stand for the whole. Every reader reads whole verdicts only.

## Recording an outcome

Move the entry out of the last table and into *Taken*, with the outcome, one line of reasoning, and
the options rejected. If the decision creates work, the Step row becomes authoritative and this file
keeps only the pointer. Where a decision is deferred or rejected, it stays here with that outcome —
a rejected decision that vanishes is one that gets proposed again.

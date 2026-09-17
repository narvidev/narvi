// This file (dispatch.go) implements handleEnsureDispatched (§9.3,
// "e2e happy path", design decision 3) -- the spawn/dispatch orchestration
// this whole file is built around. Every actual DECISION is made by an
// already-built, already-tested pure domain function
// (internal/domain/sandbox.EvaluateCircuitBreaker/EvaluateSpawnDecision,
// internal/domain/turn.NextToDispatch/Transition) -- this file's own job
// is reading fresh state, calling the right decision function, and
// writing the result back, exactly like timerfired.go's own top comment
// describes for the 5 named timers.
//
// # Sequencing (do not collapse into one transaction)
//
// The spawn branch is deliberately split into THREE steps, never one big
// transact: (1) a transact that reads fresh state, evaluates the circuit
// breaker and spawn decision, and -- if the decision is Spawn -- mints a
// token, upserts the sandboxes row (gen bump, status='spawning',
// token_hash), arms connecting_deadline, and commits; (2) OUTSIDE any
// transaction, the real (possibly slow, network-bound) SandboxProvider.
// CreateSandbox call; (3) a SECOND transact that records the outcome
// (provider_id + a Spawning->Connecting transition on success; circuit-
// breaker bookkeeping + a Suspect transition on a permanent failure; a
// no-op log on a transient one). A real network call must never hold a
// Postgres transaction open -- see actor.go's own transact doc comment
// for why every write in this package already goes through a fresh
// connection, never the long-lived advisory-lock one.
//
// The dispatch branch (a Ready/Suspect sandbox with a Pending turn ready
// to go) is, as of this fix, split the SAME three-step way as the spawn
// branch above -- it used to run entirely inside ONE transact, on the
// theory that SandboxCommander.SendCommand is "a single bounded WS frame
// write, not a slow external API call" and therefore safe to make while
// holding the transact's own FOR UPDATE lock on the session row. An
// adversarial review proved that reasoning wrong empirically: a
// deliberately-3s-delayed SendCommand blocked a concurrent
// `actor_epoch = actor_epoch + 1` UPDATE against the same row for
// essentially the whole 3s -- i.e. a slow-but-alive sandbox connection
// (bounded only by platform.Timeouts.SandboxCommandSendTimeout, currently
// 10s) could delay a legitimate second-pod takeover (BumpActorEpoch,
// hydrate.go) by up to that long. So the dispatch branch now follows
// exactly the same shape: (1) a transact that transitions the turn
// Pending->Dispatched->Processing and arms turn_deadline, and commits;
// (2) OUTSIDE any transaction, the real SandboxCommander.SendCommand call;
// (3) on failure (including ports.ErrNoLiveSandboxConnection), a SECOND,
// small transact fails the turn. Unlike the spawn branch, there is no
// reverse edge to fall back to on failure here: domain/turn's transition
// table has no Processing->Pending edge (and internal/domain/turn/state.go
// is off-limits this Step, so none is added) -- the turn is already
// committed Processing by the time SendCommand is attempted, so a send
// failure moves it forward, to Failed, via the exact same
// "fails-with-reason + synthetic execution_complete" machinery
// handleTurnDeadlineTimer (timerfired.go) already uses for a turn_deadline
// expiry (see failDispatchedTurn's own doc comment for exactly why that
// specific existing edge, not a different one, is reused here).
//
// The resume branch (§3.2, "resume") originally needed a genuinely
// different shape from spawn/restore above: sandbox.TriggerResume used to
// go STRAIGHT from Stopped/Stale to Connecting, with no interim
// "Spawning"-like state to write before calling the provider the way
// planFreshSpawn/planRestore write spawning/gen/token first. An
// adversarial review found a real, empirically-reproduced concurrency bug
// in that shape: two DIFFERENT actor instances for the SAME session (a
// legitimate stale-epoch/pod-handover takeover -- the exact same race
// executeSpawn/executeRestore already handle correctly for their OWN
// outcome-recording step) could both read the same Stopped/Stale row,
// both have EvaluateSpawnDecision return SpawnActionResume, and BOTH call
// ResumeSandbox for the identical provider object before either call
// returned -- nothing marked the row as "a resume attempt is already in
// flight" the way Spawning already does for spawn/restore.
//
// The fix (this Step's own follow-up) gives resume the SAME two-step
// shape spawn/restore already have: sandbox.TriggerResume (state.go) now
// lands in Spawning, not Connecting -- an interim "claimed, in flight"
// marker exactly like a fresh spawn's own first step -- reusing
// Spawning's own already-correct EvaluateSpawnDecision no-op guard for
// free to close the race. A new sandbox.TriggerResumeAck is the second
// step, applied once ResumeSandbox actually returns success: Spawning ->
// Connecting. So the resume branch is now genuinely a third instance of
// the SAME shape spawn/restore already use, not a special case: (1)
// tryPlanSpawn's own transact validates the (from, trigger, gen) edge via
// sandbox.Transition and writes the interim claim (planResume, reusing
// the SAME UpsertSandboxForSpawn upsert planFreshSpawn/planRestore
// already share -- its own hardcoded status='spawning' now genuinely
// matches the Transition-validated target for resume too, not merely a
// coincidence self-corrected one write later), mints a fresh token/gen
// (sandbox tokens are hashed at rest, one per gen, per §5.2 -- paraphrased,
// not verbatim -- applying to a resume's own gen bump exactly as it does
// to a spawn/restore's), and arms TimerConnectingDeadline, all before
// this transact commits; (2) OUTSIDE any transaction,
// SandboxProvider.ResumeSandbox is called against the EXISTING provider
// object (never a new one -- ResumeSandbox's own port signature returns
// only an error, so there is no new ref to record); (3) a second, fresh
// transact (recordResumeOutcome) records the outcome: on success,
// Spawning -> Connecting via TriggerResumeAck, deliberately never calling
// UpdateProviderID (the provider object never changed); on failure,
// recordSpawnFailure is reused UNCHANGED -- a permanent resume failure
// now behaves identically to a permanent spawn/restore failure (same
// circuit-breaker increment, same transitionSandboxToSuspect path), since
// it now has the exact same interim Spawning write to transition out of
// that a permanent spawn/restore failure already does. See
// planResume/executeResume/recordResumeOutcome's own doc comments below
// for the full detail, and this Step's own PR description for the
// honest, deliberately-deferred gap this still leaves open (unrelated to,
// and not fixed by, this concurrency fix): ResumeSandbox's own port
// signature (§4.1) has no CreateSpec/SESSION_CONFIG delivery channel, so
// there is still no way for the control plane to actually deliver this
// freshly minted token/gen to the already-running provider instance today
// -- left for a future RWX-based mechanism to resolve for real.

package sessionactor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/environment"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// defaultBaseImage is the CreateSpec.Image value every spawn/restore falls
// back to whenever no real, ready, matching built image exists yet for its
// own fingerprint (§10 Phase 2: "always fall back to base image on any
// miss -- never block a session") -- renamed from §9.3's own
// placeholderBaseImage (its doc comment: "the CreateSpec.Image value used
// until §8.5 ... makes real per-session image construction a thing")
// now that §8.5 ("image builds") is genuinely that thing: this is no
// longer merely a placeholder pending later work, it is the REQUIRED,
// permanent fallback value dispatch.go/imageresolve.go's resolveAndSetImage
// leaves untouched on any miss. Also doubles as ports.ImageSpec.Base for
// every fingerprint this control plane computes (domain/imagebuild.
// Fingerprint's first argument) and for the background builder's own
// BuildImage calls (internal/app/imagebuild) -- a single, system-wide base
// image today; per-Environment base image selection is a natural future
// extension, out of this Step's own scope. See ports.CreateSpec.Image's
// own doc comment ("Empty means the provider's own default base image") --
// a non-empty, clearly-named value is used instead of leaving Image empty,
// so a real SandboxProvider's own request log unambiguously shows which
// image narvi asked for. Its real build definition (§27.7:
// Playwright+Chromium, ripgrep, typescript-language-server+typescript,
// the Docker CLI/engine binaries, and §27.4's three cloud exec-
// credential plugins, every version pinned) is deploy/sandbox-image/
// Dockerfile -- wiring a real build/push pipeline that produces the
// concrete tag/digest this constant would point at is a separate,
// external-build-service concern (§19.1's own "external, opaque-to-
// this-repo build service" boundary), out of this Step's own scope.
const defaultBaseImage = "narvi/sandbox-agent:placeholder"

// scmCommitName/scmCommitEmail are the currently-unused-anywhere-in-git
// placeholder git author identity values §7's own gap already
// documented (cmd/sandbox-agent's commandHandler never actually invokes
// git commit itself -- OpenCode's own tooling does, configured with
// whatever scmName/scmEmail this Prompt carries). Real per-user git
// identity (attributing a commit to the actual prompting human) is
// explicitly out of scope for this Step too -- these two constants just
// keep the required wire fields non-empty.
const (
	scmCommitName  = "narvi-agent"
	scmCommitEmail = "agent@narvi.dev"
)

// hashSandboxToken mirrors internal/adapters/inbound/wshub.
// HashSandboxToken's algorithm exactly (SHA-256, unsalted, hex-encoded --
// a sandbox token is a high-entropy, server-generated secret, not a
// low-entropy human password, so the salted-slow-hash rationale for
// passwords does not apply). Duplicated here, rather than calling wshub's
// own exported function, because internal/adapters/inbound/wshub already
// imports internal/app/sessionactor (wshub.NewSandboxHandler takes a
// *sessionactor.Registry) -- importing wshub from here would create an
// import cycle, and app/sessionactor must never import
// internal/adapters/* regardless (internal/app/ports/doc.go's own
// import-direction rule), so this small duplication is correct, not
// merely expedient.
func hashSandboxToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// spawnPlan is what planDispatch's own transact hands back to
// handleEnsureDispatched when (and only when) the spawn branch decided to
// actually spawn, restore, OR resume -- the real provider call itself
// happens OUTSIDE that transact, in executeSpawn/executeRestore/
// executeResume, using exactly this plan.
//
// §3.2's own concurrency fix folds resume into this SAME type (see the
// resume/providerObjectID fields below) rather than keeping the separate
// resumePlan type this Step originally introduced: now that
// sandbox.TriggerResume's own first-step target is Spawning (state.go),
// resume's own write genuinely IS the same "commit an interim claim
// inside tryPlanSpawn's transact, then call the provider outside any
// transaction" shape spawn/restore already have -- planDispatch's own
// two-plan return shape (spawnPlan, dispatchPlan) is restored to exactly
// what it was before resume needed a third, structurally-different plan
// type at all.
type spawnPlan struct {
	gen  int
	spec ports.CreateSpec

	// restore is true when this plan is a Stopped/Failed/Stale ->
	// Spawning RESTORE (§3.2: "stopped|stale + snapshot -> restore (new
	// gen)"), as opposed to a plain fresh spawn (§3.2, "snapshots &
	// restore", design decision 6). handleEnsureDispatched dispatches
	// restore==true to executeRestore instead of executeSpawn. Mutually
	// exclusive with resume (below) -- EvaluateSpawnDecision's own
	// priority ordering (spawndecision.go, untouched by this fix) never
	// returns both kinds for the same call. planDispatch's own two-plan
	// return shape (§9.3) is deliberately unchanged by this addition --
	// a restore decision is still exactly branch (a) ("no live sandbox,
	// needs [re]spawning") planDispatch already recognizes, just
	// restoring from a snapshot instead of creating fresh.
	restore bool
	// snapshotID is set only when restore is true: the snapshot to
	// restore from (ports.SnapshotID(action.SnapshotImageID)).
	snapshotID ports.SnapshotID

	// resume is true when this plan is a Stopped/Stale -> Spawning
	// interim claim for a persistent RESUME of an existing provider
	// sandbox object (§3.2, "resume") -- reusing the SAME two-step
	// shape restore/spawn already have, now that TriggerResume's own
	// target is Spawning rather than a special one-step jump straight to
	// Connecting. handleEnsureDispatched dispatches resume==true to
	// executeResume instead of executeSpawn/executeRestore.
	resume bool
	// providerObjectID is set only when resume is true: the SAME
	// provider sandbox instance being resumed (action.ProviderObjectID
	// from EvaluateSpawnDecision) -- never a new one, unlike spec/
	// snapshotID above, which ask the provider to CREATE one.
	// executeResume calls ResumeSandbox with this id directly; spec is
	// left at its zero value on a resume plan and never read by
	// executeResume, since ResumeSandbox's own port signature (§4.1)
	// takes no CreateSpec at all.
	providerObjectID string

	// createdBy is sessionRow.CreatedBy (§8.5, "image builds") --
	// populated only on a fresh-spawn or restore plan (planFreshSpawn/
	// planRestore both already have sessionRow in scope); left at its zero
	// value on a resume plan, which never needs it (resolveAndSetImage,
	// imageresolve.go, is never called for resume -- see
	// handleEnsureDispatched below). Threaded through here rather than
	// re-fetching sessionRow a second time in resolveAndSetImage, which
	// runs AFTER this plan's own transact has already committed and
	// returned.
	createdBy pgtype.UUID

	// environmentID is sessionRow.EnvironmentID (§14.3, "mocking +
	// contract drift") -- populated in BOTH planFreshSpawn and planRestore,
	// mirroring createdBy's own identical population exactly (same
	// reasoning: both already have sessionRow in scope; a resume plan does
	// not, and does not need it -- checkContractDrift, contractdrift.go, is
	// never called for resume, same as resolveAndSetImage). Invalid
	// (pgtype.UUID{}.Valid == false) for an ordinary, unscoped session --
	// checkContractDrift's own first early-return checks exactly this.
	environmentID pgtype.UUID
}

// dispatchPlan is what planDispatch's own transact hands back to
// handleEnsureDispatched when (and only when) the dispatch branch decided
// to actually dispatch a Pending turn to an already-live sandbox -- the
// real SandboxCommander.SendCommand call itself happens OUTSIDE that
// transact, in executeDispatch, using exactly this plan. By the time this
// is returned, the turn has already been committed Pending->Dispatched->
// Processing (tryPlanDispatch) or was already Processing to begin with
// (tryPlanReenqueue) and turn_deadline is already armed -- see this file's
// own top comment for why.
type dispatchPlan struct {
	turnID  pgtype.UUID
	payload json.RawMessage

	// sessionRow is the SAME sqlcgen.Session row tryPlanDispatch/
	// tryPlanReenqueue's own caller already loaded inside planDispatch's
	// transact -- threaded through here, rather than re-queried a second
	// time, mirroring spawnPlan's own createdBy/environmentID precedent
	// (its own doc comment: "Threaded through here rather than re-fetching
	// sessionRow a second time"). §10's own turn-dispatch-time rollout
	// re-check (executeDispatch, below) is why this field exists: it needs
	// sessionRow.Repos to resolve admission, and it runs OUTSIDE any
	// transaction, after this plan's own transact has already committed
	// and returned.
	sessionRow sqlcgen.Session
}

// handleEnsureDispatched implements the EnsureDispatched command
// (§9.3): read fresh state, decide whether to spawn,
// resume, or restore a sandbox, or dispatch a pending turn (or do nothing
// this round), and act. Resume (§3.2) added alongside spawn/restore;
// spawn.resume is checked before spawn.restore since both are carried on
// the SAME spawnPlan type (its own doc comment above explains why) and
// are mutually exclusive by construction.
//
// §8.5 ("image builds") adds resolveAndSetImage on the spawn/restore
// branches ONLY (never resume, which has no CreateSpec at all -- see
// planResume's own doc comment) -- called here, AFTER planDispatch's own
// transact has already committed and returned, exactly like executeSpawn/
// executeRestore's own real provider call already runs OUTSIDE any
// transaction (this file's own top "# Sequencing" comment): resolving a
// fingerprint is ANOTHER network-bound step (a GitHub API call per repo,
// plus a Postgres read), inserted into that SAME outside-any-transaction
// zone, immediately before the provider is ever called -- never inside
// planDispatch's own transact. See imageresolve.go's own doc comment for
// the full "never block a spawn" design.
//
// §14.3 ("mocking + contract drift", §14.3) adds checkContractDrift
// immediately alongside resolveAndSetImage, on the SAME spawn/restore
// branches, at the SAME hook point, for the SAME reason: it is ALSO
// network-bound (a GitHub API call per repo, plus a Postgres read/best-
// effort upsert), scoped to mock-configured Environments only (its own
// first real check, contractdrift.go), and must never block a spawn --
// see that file's own doc comment for the full design. Order between the
// two calls does not matter functionally (each only reads plan.spec/
// plan.createdBy/plan.environmentID and mutates its own disjoint state),
// but they are kept adjacent here since they share this exact hook point
// for the exact same structural reason.
func (a *Actor) handleEnsureDispatched(ctx context.Context) error {
	spawn, dispatch, err := a.planDispatch(ctx)
	if err != nil {
		return err
	}
	switch {
	case spawn != nil && spawn.resume:
		return a.executeResume(ctx, spawn)
	case spawn != nil && spawn.restore:
		a.resolveAndSetImage(ctx, spawn)
		a.checkContractDrift(ctx, spawn)
		return a.executeRestore(ctx, spawn)
	case spawn != nil:
		a.resolveAndSetImage(ctx, spawn)
		a.checkContractDrift(ctx, spawn)
		return a.executeSpawn(ctx, spawn)
	case dispatch != nil:
		return a.executeDispatch(ctx, dispatch)
	default:
		// Nothing to do this round: no pending turn, branch (c)'s no-op,
		// or a defensive skip (nil provider/commander) inside one of the
		// try* helpers below.
		return nil
	}
}

// planDispatch runs the ENTIRE read-fresh-state-and-decide step inside one
// transact (§2's own epoch-fencing discipline: every read that a write
// decision depends on must be fenced the same way the write itself is).
// Returns a non-nil *spawnPlan or *dispatchPlan (never both) only when the
// corresponding branch decided to act and its own "commit state" half has
// already committed (a resume's own interim Spawning claim, exactly like
// a fresh spawn/restore's, per this file's own top comment) -- the caller
// (handleEnsureDispatched) then performs the actual network call
// (CreateSandbox / RestoreFromSnapshot / ResumeSandbox / SendCommand
// respectively) outside any transaction.
func (a *Actor) planDispatch(ctx context.Context) (*spawnPlan, *dispatchPlan, error) {
	var spawn *spawnPlan
	var dispatch *dispatchPlan

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session: %w", err)
		}

		sandboxRow, sbErr := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		hasSandbox := true
		if sbErr != nil {
			if !errors.Is(sbErr, pgx.ErrNoRows) {
				return fmt.Errorf("sessionactor: get sandbox: %w", sbErr)
			}
			hasSandbox = false
		}

		turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: list turns: %w", err)
		}

		// NextToDispatch already encodes "no in-flight turn AND a Pending
		// turn exists" -- both branches (a) and (b) below need exactly
		// that same predicate, just gated on a different sandbox-status
		// condition.
		entries := toQueueEntries(turns)
		pendingID, hasPending := turn.NextToDispatch(entries)
		if !hasPending {
			// §3.3 ("turn recovery", §9.3 scenario #2): no dispatchable
			// Pending turn -- but there may still be an in-flight
			// (Dispatched/Processing) turn whose prompt was sent to a
			// PREVIOUS sandbox incarnation that has since died (a respawn
			// happened since, or is about to). InFlightTurn/NextToDispatch
			// are mutually exclusive by construction (both are gated on
			// the SAME HasInFlightTurn predicate), so this branch and the
			// three below it never both fire for the same planDispatch
			// call.
			sp, d, err := a.planReenqueueOrRespawn(ctx, tx, sessionRow, sandboxRow, hasSandbox, entries, turns, now)
			if err != nil {
				return err
			}
			spawn = sp
			dispatch = d
			return nil
		}

		// Branch (a): no sandbox row yet, or the existing one is dead
		// (Stopped/Stale/Failed) -- needs a fresh spawn, restore, or
		// resume before anything can be dispatched.
		if !hasSandbox || sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			sp, err := a.tryPlanSpawn(ctx, tx, sessionRow, sandboxRow, hasSandbox, now)
			if err != nil {
				return err
			}
			spawn = sp
			return nil
		}

		// Branch (b): a live sandbox already exists and is Ready or Suspect
		// -- the turn can be dispatched to it right now. Suspect is
		// deliberately included here, not just Ready: a Suspect sandbox is
		// still within its terminal_grace window and may yet recover (§3.2,
		// "two-phase terminalization", sandboxevent.go's own
		// handleSandboxEvent), so a real dispatch attempt to it is allowed
		// to proceed rather than waiting idle -- if the underlying
		// SandboxCommander.SendCommand genuinely fails because the
		// sandbox is truly gone, that failure path already fails the turn
		// forward (executeDispatch/failDispatchedTurn) independently of
		// whatever the sandbox's own grace timer later decides.
		//
		// Known, honestly-documented gap: this always routes a Ready
		// sandbox straight to tryPlanDispatch, regardless of whether it
		// actually has a live WebSocket connection right now.
		// EvaluateSpawnDecision already supports recovering a Ready sandbox
		// whose WebSocket never reconnected (past SpawnConfig.ReadyWait --
		// see tryPlanSpawn's own ForceRespawn carve-out), but nothing here
		// ever reaches that path for a Ready status: tryPlanSpawn's own
		// SpawnState construction has zero wiring for HasActiveWebSocket
		// today (it is never set to anything but its false zero value; grep
		// the repo -- no production caller sets it anywhere), so it cannot
		// yet safely distinguish "Ready with a dead connection, past
		// ReadyWait" from "Ready with a perfectly live one" here. Wiring
		// this branch to tryPlanSpawn before that detection exists (via the
		// sandbox-side connection registry/SandboxCommander) would risk
		// force-respawning a healthy, actively-connected Ready sandbox out
		// from under a live session -- worse than leaving this narrower gap
		// open and documented, matching this project's own established
		// practice elsewhere (e.g. §9.3/§3.2's own documented, deliberate
		// gaps) of naming a real, known-open limitation rather than
		// silently leaving it unstated or attempting a half-solution.
		status := sandbox.State(sandboxRow.Status)
		if status == sandbox.StateReady || status == sandbox.StateSuspect {
			d, err := a.tryPlanDispatch(ctx, tx, sessionRow, sandboxRow, pendingID, turns, now)
			if err != nil {
				return err
			}
			dispatch = d
			return nil
		}

		// Branch (c): sandbox exists, is neither dead nor Ready/Suspect
		// (e.g. still Spawning/Connecting/Booting) -- defer to
		// EvaluateSpawnDecision's own judgment, exactly like branch (a)
		// does: for the vast majority of calls (a sandbox still genuinely
		// booting, well within SpawnStuckTimeout) this is a safe, cheap
		// no-op via EvaluateSpawnDecision's own Skip guard; only once the
		// sandbox is genuinely stuck past that window does it produce a
		// real SpawnActionSpawn -> TriggerForceRespawn -> actual respawn.
		// hasSandbox is always true by the time this branch is reached --
		// branch (a) above already handles the !hasSandbox case.
		sp, err := a.tryPlanSpawn(ctx, tx, sessionRow, sandboxRow, hasSandbox, now)
		if err != nil {
			return err
		}
		spawn = sp
		return nil
	})

	return spawn, dispatch, err
}

// planReenqueueOrRespawn implements §3.3's ("turn recovery", §9.3
// scenario #2) own extension to planDispatch: called ONLY when
// NextToDispatch reported no dispatchable Pending turn, this checks
// whether there is instead an in-flight (Dispatched/Processing) turn that
// needs its prompt re-sent, mirroring planDispatch's own three branches
// (a/b/c) exactly -- just gated on the in-flight turn's own
// dispatched_sandbox_gen instead of a Pending turn's mere existence:
//
//   - (a') no sandbox row at all, or a definitively dead one -- the
//     in-flight turn's own sandbox incarnation is gone; reuses
//     tryPlanSpawn UNCHANGED (spawning is spawning, regardless of whether
//     a Pending or an in-flight turn is what ultimately needs it).
//   - (b') a live sandbox, Ready or Suspect: if the in-flight turn's own
//     dispatched_sandbox_gen already matches this sandbox's CURRENT gen
//     (turn.NeedsReenqueue reports false), this is a turn being
//     correctly, actively processed by its own current-gen sandbox RIGHT
//     NOW -- a strict, hard no-op (see TestHandleEnsureDispatched_
//     ProcessingOnCurrentGenSandbox_NeverReDispatched, dispatch_
//     integration_test.go, for the regression proof this can never
//     regress). Otherwise, tryPlanReenqueue re-sends it to the current
//     sandbox.
//   - (c') sandbox exists, neither dead nor Ready/Suspect (still
//     Spawning/Connecting/Booting/Snapshotting) -- defers to
//     EvaluateSpawnDecision's own judgment exactly like branch (c) does:
//     a genuine respawn already in progress (from a prior (a') round) is
//     left alone (Skip) unless it's genuinely stuck past its own recovery
//     window.
//
// hasSandbox/entries/turns/now are exactly what planDispatch's own
// transact already loaded -- no second read.
func (a *Actor) planReenqueueOrRespawn(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, hasSandbox bool,
	entries []turn.QueueEntry[pgtype.UUID], turns []sqlcgen.Turn,
	now time.Time,
) (*spawnPlan, *dispatchPlan, error) {
	inFlightID, hasInFlight := turn.InFlightTurn(entries)
	if !hasInFlight {
		// No turn at all, or every turn is already terminal -- nothing
		// for this session to do this round, exactly like planDispatch's
		// own pre-existing early return.
		return nil, nil, nil
	}

	if !hasSandbox || sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
		sp, err := a.tryPlanSpawn(ctx, tx, sessionRow, sandboxRow, hasSandbox, now)
		return sp, nil, err
	}

	target, ok := findTurnByID(turns, inFlightID)
	if !ok {
		return nil, nil, fmt.Errorf("sessionactor: in-flight turn %s not found among loaded turns", inFlightID.String())
	}

	status := sandbox.State(sandboxRow.Status)
	if status == sandbox.StateReady || status == sandbox.StateSuspect {
		var dispatchedGen *int
		if target.DispatchedSandboxGen != nil {
			g := int(*target.DispatchedSandboxGen)
			dispatchedGen = &g
		}
		if !turn.NeedsReenqueue(dispatchedGen, int(sandboxRow.Gen)) {
			// CRITICAL no-op: this turn is already correctly, actively
			// being processed by its own current-gen, live sandbox --
			// re-dispatching it here would send its prompt a SECOND time
			// to a sandbox already working on it. See this function's own
			// doc comment above for the regression test proving this.
			return nil, nil, nil
		}
		d, err := a.tryPlanReenqueue(ctx, tx, sessionRow, sandboxRow, target, now)
		return nil, d, err
	}

	sp, err := a.tryPlanSpawn(ctx, tx, sessionRow, sandboxRow, hasSandbox, now)
	return sp, nil, err
}

// tryPlanReenqueue implements §3.3's ("turn recovery") own re-enqueue
// write: re-sends target's prompt to sandboxRow (the CURRENT, live
// sandbox incarnation), WITHOUT calling turn.Transition at all -- unlike
// tryPlanDispatch, this must NOT re-transition the turn: it is already,
// validly, Processing, and from the turn's OWN domain perspective its
// execution never stopped -- only its underlying sandbox did (mirroring
// §3.2's own "late-success reconciliation needed no new domain-turn
// edge" precedent, per this Step's own brief -- see internal/domain/
// turn/state.go's own top comment for that precedent's full writeup).
//
// Re-arms turn_deadline fresh from now -- a fair, full new budget on the
// new sandbox, rather than leaving a stale deadline still ticking down
// from the turn's ORIGINAL dispatch attempt (now irrelevant: that
// sandbox incarnation is gone). Stamps dispatched_sandbox_gen to
// sandboxRow's own current gen (the SAME UpdateTurnStatus query
// tryPlanDispatch uses, passing target's own CURRENT status back
// unchanged -- never Transition's output, since none was computed).
// Builds the SAME BuildPromptPayload tryPlanDispatch uses, which
// automatically carries sessionRow.OpencodeConversationID (§3.3) -- so
// the re-enqueued turn resumes the SAME OpenCode conversation with no
// special-cased "resume" logic needed here at all.
func (a *Actor) tryPlanReenqueue(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, target sqlcgen.Turn,
	now time.Time,
) (*dispatchPlan, error) {
	if a.commander == nil {
		// Defensive: mirrors tryPlanDispatch's own identical nil-commander
		// guard exactly -- some tests, and any future caller genuinely
		// without one, must not panic here.
		a.logger.Warn("sessionactor: reenqueue decision would proceed but no SandboxCommander is configured; skipping")
		return nil, nil
	}

	if err := a.armTimer(ctx, tx, TimerTurnDeadline, now.Add(a.timeouts.TurnDeadline)); err != nil {
		return nil, err
	}

	dispatchedGen := sandboxRow.Gen
	// The events-log high-water mark, read inside this SAME transaction so
	// it cannot straddle a concurrent insert: every event this turn's own
	// (re)dispatch goes on to produce lands strictly above it. §26.4's
	// corroboration queries use it as their lower bound, replacing a
	// created_at >= dispatched_at comparison that straddled the Postgres
	// and application clocks -- see
	// migrations/000089_turns_dispatched_event_id.up.sql.
	//
	// Note this DOES advance on re-enqueue, where dispatched_at
	// deliberately does not (this call site has never re-stamped it: a
	// re-enqueued turn keeps its ORIGINAL dispatch time). That difference
	// is intended, and it agrees with the gen filter rather than fighting
	// it: a re-enqueue re-sends this turn's prompt to a DIFFERENT sandbox
	// incarnation, so the only trace that can honestly corroborate it is
	// the one the new incarnation produces. Events from the previous,
	// now-dead incarnation carry the OLD gen and are already excluded by
	// the gen filter; advancing the watermark excludes them by id too.
	dispatchedEventID, err := a.stores.event.WithTx(tx).MaxEventIDForSession(ctx, a.sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: read events high-water mark for reenqueue: %w", err)
	}
	// messageID (finding F3 (§21.1's amendment)) is generated ONCE, here, then
	// threaded into BOTH the wire payload below AND this SAME
	// UpdateStatus write (turns.dispatched_message_id) -- the one
	// identifier httpapi.PostReviewVerdict later resolves a verdict-
	// posting turn BY, instead of "whichever turn is processing for this
	// session right now" (BuildPromptPayload's own doc comment). A fresh
	// value on EVERY re-enqueue is correct, not merely tolerated: this is
	// a genuinely new dispatch to a different sandbox incarnation, so an
	// old, now-stale MessageId a still-alive PREVIOUS incarnation might
	// still present later finds no matching row here -- httpapi.
	// PostReviewVerdict refuses the call outright (403) rather than
	// resolving the wrong turn or degrading to session-wide resolution
	// (D1/D4/D6, second adversarial-review round: see that handler's own
	// doc comment for the full "why refuse rather than degrade").
	messageID := uuid.NewString()
	if _, err := a.stores.turn.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: target.ID,
		// The turn's own CURRENT status, passed back unchanged -- this is
		// NOT a transition (no turn.Transition call; see this function's
		// own doc comment above for why one must never happen here), just
		// this shared query's own required, non-COALESCE'd Status column.
		Status:               target.Status,
		DispatchedSandboxGen: &dispatchedGen,
		DispatchedEventID:    &dispatchedEventID,
		DispatchedMessageID:  &messageID,
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: stamp dispatched_sandbox_gen for reenqueue: %w", err)
	}

	payload, err := BuildPromptPayload(a.sessionID.String(), sessionRow, sandboxRow, target, messageID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: build prompt payload (reenqueue): %w", err)
	}

	a.logger.Info("sessionactor: re-enqueuing in-flight turn to a respawned sandbox incarnation",
		"turn_id", target.ID.String(), "gen", sandboxRow.Gen)

	return &dispatchPlan{turnID: target.ID, payload: payload, sessionRow: sessionRow}, nil
}

// tryPlanSpawn implements design decision 3a's own circuit-breaker-then-
// spawn-decision sequence, and -- on SpawnActionSpawn/SpawnActionRestore/
// SpawnActionResume -- performs the token-mint/upsert/arm-timer write, all
// still inside the caller's own transact (§3.2's own concurrency fix:
// resume now writes its own interim Spawning claim here too, exactly like
// spawn/restore already did -- see this file's own top comment for why
// that write is what closes the concurrent-double-ResumeSandbox-call race
// an adversarial review found in the OLD one-step shape).
func (a *Actor) tryPlanSpawn(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, hasSandbox bool,
	now time.Time,
) (*spawnPlan, error) {
	cbState := sandbox.CircuitBreakerState{}
	if hasSandbox {
		cbState.FailureCount = int(sandboxRow.SpawnFailureCount)
		if sandboxRow.LastSpawnFailureAt.Valid {
			cbState.LastFailureTime = sandboxRow.LastSpawnFailureAt.Time
		}
	}

	cbCfg := sandbox.CircuitBreakerConfig{
		Threshold: sandbox.CircuitBreakerThreshold,
		Window:    a.timeouts.CircuitBreakerWindow,
	}
	cbDecision := sandbox.EvaluateCircuitBreaker(cbState, cbCfg, now)

	if cbDecision.ShouldReset && hasSandbox && (sandboxRow.SpawnFailureCount != 0 || sandboxRow.LastSpawnFailureAt.Valid) {
		if _, err := a.stores.sandbox.WithTx(tx).UpdateCircuitBreaker(ctx, sqlcgen.UpdateSandboxCircuitBreakerParams{
			SessionID:          a.sessionID,
			SpawnFailureCount:  0,
			LastSpawnFailureAt: pgtype.Timestamptz{},
		}); err != nil {
			return nil, fmt.Errorf("sessionactor: reset circuit breaker: %w", err)
		}
		sandboxRow.SpawnFailureCount = 0
		sandboxRow.LastSpawnFailureAt = pgtype.Timestamptz{}
	}

	if !cbDecision.ShouldProceed {
		// An honest, documented limitation (per this Step's own brief):
		// nothing re-triggers EnsureDispatched on a plain timeout with no
		// other sandbox activity at all -- a later EnsureDispatched (the
		// next sandbox event, or a future session) tries again. A
		// genuinely timer-driven retry is §9.3's own resilience-suite
		// job (§9.3 #8), not this one's.
		a.logger.Warn("sessionactor: spawn circuit breaker open; skipping spawn this round",
			"wait", cbDecision.WaitTime.String())
		return nil, nil
	}

	if a.provider == nil {
		// Defensive: a Registry constructed without a SandboxProvider
		// (some tests, and any future caller that genuinely has none)
		// must not panic here -- log and no-op, exactly like the
		// nil-commander guard in tryPlanDispatch below.
		a.logger.Warn("sessionactor: spawn decision would proceed but no SandboxProvider is configured; skipping")
		return nil, nil
	}

	spawnState := sandbox.SpawnState{Status: sandbox.StatePending}
	if hasSandbox {
		spawnState = sandbox.SpawnState{
			Status:           sandbox.State(sandboxRow.Status),
			CreatedAt:        pgTimeOrZero(sandboxRow.CreatedAt),
			ProviderObjectID: stringOrEmpty(sandboxRow.ProviderID),
			// SnapshotImageID is read back from the sandbox row's own real
			// snapshot_id column (§3.2, "snapshots & restore", design
			// decision 6) -- "" (no snapshot machinery reached it yet, or
			// this sandbox was never snapshotted) unless a real
			// "snapshot_ready" event has previously persisted one (see
			// sandboxevent.go's own new branch).
			SnapshotImageID: stringOrEmpty(sandboxRow.SnapshotID),
			LastSeenAt:      pgTimeOrZero(sandboxRow.LastSeenAt),
		}
	}

	caps := a.provider.Capabilities()
	action := sandbox.EvaluateSpawnDecision(spawnState, sandbox.SpawnConfig{
		Cooldown:        a.timeouts.SpawnCooldown,
		ReadyWait:       a.timeouts.SpawnReadyWait,
		SpawningTimeout: a.timeouts.SpawnStuckTimeout,
	}, now, false, caps.Resume)

	// §27.5's own dispatch-time half of the "fail-closed, twice" rule
	// (§27.5/§27.6, brief point A) -- deliberately gated to ONLY the
	// three action kinds that are actually about to attempt a REAL
	// provider call (Spawn/Restore/Resume): an ordinary Skip/Wait (a
	// cooldown, an in-progress spawn, the circuit breaker) is not itself
	// trying to reach the provider at all, so it must stay the same
	// silent, cheap no-op it always was -- refusing a session that is not
	// currently trying to spawn would be a confusing, spurious error with
	// nothing for it to actually be about. See refuseIfSubstrateUnsupported's
	// own doc comment for why this is a SEPARATE, independent check from
	// httpapi.CreateSessionCore's own up-front one, not a re-use of its
	// result.
	if action.Kind == sandbox.SpawnActionSpawn || action.Kind == sandbox.SpawnActionRestore || action.Kind == sandbox.SpawnActionResume {
		dockerRequired, err := a.refuseIfSubstrateUnsupported(ctx, tx, sessionRow, caps)
		if err != nil {
			return nil, err
		}

		// §10's own dispatch-time half of the "fail-closed, twice"
		// rule (§10 Phase 6, §32) -- mirrors refuseIfSubstrateUnsupported's
		// own identical gating to ONLY Spawn/Restore/Resume immediately
		// above, for the identical reason (a Skip/Wait is not itself
		// trying to reach the provider at all). See
		// refuseIfRolloutUnenrolled's own doc comment for the full "why
		// this is what makes rollback real".
		if err := a.refuseIfRolloutUnenrolled(ctx, tx, sessionRow); err != nil {
			return nil, err
		}

		// §27.8's own genuinely-unresolved snapshot-parity point (§27.5
		// brief, point D), the SAME resolution sandboxevent.go's own
		// triggerSnapshotBestEffort already applies on the way IN to a
		// snapshot -- applied here, symmetrically, on the way OUT: even
		// if action.Kind resolved to Restore (SnapshotImageID != ""),
		// never actually restore it for a Docker-required session. In
		// today's design this branch should be unreachable in practice
		// (triggerSnapshotBestEffort's own identical gate means a
		// Docker-required sandbox's snapshot_id column is never
		// populated to begin with, since environments are immutable
		// once created -- no UPDATE path exists), but defense in depth
		// against exactly the failure this codebase cannot verify is
		// safe (a cross-runtime restore: a snapshot taken under one
		// Modal runtime restored into the OTHER) is cheap here and the
		// cost of being wrong is silently running something §27.8 names
		// as unverified. Downgrading to a fresh Spawn, never refusing
		// outright, keeps §10-P2's "never block a spawn" intact -- the
		// session still gets a live sandbox, just without whatever
		// state the (untrusted, in this one case) snapshot would have
		// restored.
		if action.Kind == sandbox.SpawnActionRestore && dockerRequired {
			a.logger.Warn("sessionactor: refusing snapshot restore for a Docker-required session; forcing a fresh spawn instead of an unverified cross-runtime restore (§27.8)",
				"session_id", a.sessionID.String(), "snapshot_id", action.SnapshotImageID)
			action = sandbox.SpawnAction{Kind: sandbox.SpawnActionSpawn}
		}

		// §30.4(3)'s own fail-closed defense-in-depth (the PRIMARY fix is
		// the mint-time credential-cache purge, cmd/sandbox-agent/main.go's
		// own HandleSnapshot): refuse to restore a snapshot into a session
		// whose own effective egress mode is currently shadow unless that
		// snapshot's own row was itself stamped shadow at the moment it was
		// taken (sandboxevent.go's own handleSnapshotReadyEvent). The
		// polarity is explicit and deliberate, per §30.4(3) verbatim: "an
		// absent bit (every pre-existing snapshot) is treated as live and
		// restore into a shadow session is refused" -- so
		// sandboxRow.SnapshotSuppressedInShadow == false (its own column
		// DEFAULT, migrations/000106_sandbox_snapshot_shadow_bit.up.sql)
		// is treated as "this snapshot may have been taken live", never as
		// "safe to assume shadow". Resolved fresh here, at restore time,
		// via the SAME session-repos-aggregated formula the stamp itself
		// was written with (postgres.OutboxStore.ResolveEffectiveMode) --
		// this is a genuinely different question ("is THIS session shadow
		// RIGHT NOW") from the one the stamp itself answered ("was the
		// ORIGINATING session shadow THEN"), so it is not the same kind of
		// re-read §30.8's push/PR fix (pushpr.go) forbids -- there the bug
		// was re-deriving the SAME decision a signal had already frozen;
		// here the two reads are of two different sessions' egress modes,
		// separated by however long the snapshot has sat unused, and
		// neither one may be cached across that gap.
		//
		// Mirrors the Docker-required downgrade immediately above:
		// downgrading to a fresh Spawn, never refusing the session
		// outright, keeps §10-P2's "never block a spawn" intact -- the
		// session still gets a live sandbox, just without whatever
		// (untrusted, in this one case) state the snapshot would have
		// restored.
		if action.Kind == sandbox.SpawnActionRestore && !sandboxRow.SnapshotSuppressedInShadow &&
			a.stores.outbox.WithTx(tx).ResolveEffectiveMode(ctx, a.sessionID) {
			a.logger.Warn("sessionactor: refusing snapshot restore into a shadow session: the snapshot's own shadow bit is absent/false, treated as live (§30.4(3)); forcing a fresh spawn instead",
				"session_id", a.sessionID.String(), "snapshot_id", action.SnapshotImageID)
			action = sandbox.SpawnAction{Kind: sandbox.SpawnActionSpawn}
		}
	}

	switch action.Kind {
	case sandbox.SpawnActionSpawn:
		return a.planFreshSpawn(ctx, tx, sessionRow, hasSandbox, sandboxRow, now)
	case sandbox.SpawnActionRestore:
		// Only reachable when hasSandbox is true (EvaluateSpawnDecision's
		// own Restore branch requires SnapshotImageID != "" AND status in
		// {Stopped, Failed, Stale} -- none of which a brand-new,
		// no-sandbox-row session can have), so sandboxRow is a real,
		// already-loaded row here, not the zero value.
		return a.planRestore(ctx, tx, sessionRow, sandboxRow, action.SnapshotImageID, now)
	case sandbox.SpawnActionResume:
		// Only reachable when hasSandbox is true (EvaluateSpawnDecision's
		// own Resume branch requires ProviderObjectID != "" AND status in
		// {Stopped, Stale} -- none of which a brand-new, no-sandbox-row
		// session can have), mirroring SpawnActionRestore's own identical
		// reasoning above.
		return a.planResume(ctx, tx, sandboxRow, action.ProviderObjectID, now)
	default:
		// Skip/Wait are genuine, expected outcomes (cooldown, already in
		// progress, circuit breaker, ...). Spawn, Restore, and Resume are
		// all handled above --
		// this default case exists so a future SpawnActionKind addition
		// fails safely into a no-op (logged) rather than being silently
		// mishandled by one of the cases above.
		a.logger.Info("sessionactor: spawn decision is not Spawn/Restore/Resume this round; no-op",
			"kind", action.Kind.String(), "reason", action.Reason)
		return nil, nil
	}
}

// refuseIfSubstrateUnsupported implements §27.5's own dispatch-time
// half of the "fail-closed, twice" rule (§27.5/§27.6, brief point A):
// re-checked HERE, immediately before tryPlanSpawn is about to attempt a
// REAL spawn/restore/resume against a.provider, using the SAME pure
// decision (environment.CheckSubstrateCapabilities)
// httpapi.CreateSessionCore's own up-front check (checkSubstrateCapabilitiesUpFront,
// create.go) already ran once at session-creation time.
//
// This is a genuinely SEPARATE, independent check, not a re-use of that
// earlier result: the two run against fresh reads at two different
// times, from two different processes/call paths, and either one being
// disabled (or buggy) must not silently disable the other's own
// protection. Concretely, the gap this closes that a single up-front
// check alone cannot: an operator can reconfigure the deployment's
// SandboxProvider (or a provider's own capability set can genuinely
// change) at any point AFTER a docker/enforced-egress session was
// already created -- the FIRST spawn attempt for that session, and
// every respawn/restore/resume after it (this is tryPlanSpawn, the
// entire spawn path §27.5/§27.6's own brief names, not just the very
// first call), re-reads both the Environment's own current requirement
// and the provider's own current Capabilities fresh, every time, rather
// than trusting whatever was true when the session was first created.
//
// caps is the SAME ports.Capabilities value tryPlanSpawn's own caller
// already read from a.provider (its own single call site, immediately
// above) -- passed in rather than re-read a second time here, so this
// check can never observe a DIFFERENT provider snapshot than the one
// EvaluateSpawnDecision itself just reasoned over.
//
// On failure, this refuses the ENTIRE spawn attempt for this round with
// a real, propagated error -- deliberately NOT sandbox.SpawnActionSkip's
// own silent (nil, nil) shape: a Skip looks identical to an ordinary,
// expected cooldown/in-progress no-op, but a docker/enforced-egress
// session whose requirement the provider cannot honor is a genuine,
// persistent misconfiguration that must be loud (logged at Error, and
// surfaced to run's own "command handling failed" logger) until either
// the provider or the Environment's own requirement changes -- silently
// retrying forever would look, from the outside, exactly like a session
// that is merely waiting its turn.
//
// Returns the resolved dockerRequired alongside the error (even on a nil
// error) so its one caller (tryPlanSpawn) can also apply §27.8's own
// restore-downgrade decision without a second Environment lookup.
func (a *Actor) refuseIfSubstrateUnsupported(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, caps ports.Capabilities) (dockerRequired bool, err error) {
	_, dockerRequired, egressPolicy, err := a.environmentSubstrate(ctx, tx, sessionRow.EnvironmentID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: resolve substrate requirements for dispatch-time capability re-check: %w", err)
	}

	if err := environment.CheckSubstrateCapabilities(dockerRequired, egressPolicy.RequiresEnforcement(), caps.DockerInSandbox, caps.EgressPolicy); err != nil {
		a.logger.Error("sessionactor: refusing to spawn: configured provider cannot honor this Environment's substrate requirements (§27.5/§27.6 dispatch-time fail-closed re-check)",
			"session_id", a.sessionID.String(), "error", err)
		return dockerRequired, fmt.Errorf("sessionactor: dispatch-time substrate capability re-check failed: %w", err)
	}
	return dockerRequired, nil
}

// rolloutDecisionForSession resolves sessionRow's own repos (its JSON
// `repos` column) into []rollout.RepoAdmission facts via repoSettings,
// then returns rollout.Decide's own verdict for a.rolloutMode -- the ONE
// resolution shared, byte-for-byte, by BOTH of this Step's own dispatch-
// side rollout re-checks: refuseIfRolloutUnenrolled (tryPlanSpawn's own
// spawn/restore/resume re-check, below) and rolloutRefusalForDispatch
// (executeDispatch's own turn-dispatch re-check, this file's own "single
// placement" fix for the gap an adversarial review found -- see that
// function's own doc comment). Sharing this one resolution is what keeps
// the two gates from ever drifting to a different definition of
// "enrolled" from one another; only what surrounds it (which tx/pool the
// repo_settings read runs on, and what happens on refusal) differs
// between the two callers.
//
// repoSettings is a parameter, not always a.stores.repoSettings directly,
// because the two callers need different connection scoping:
// refuseIfRolloutUnenrolled runs INSIDE tryPlanSpawn's own transact (a
// real spawn/restore/resume write follows, on the SAME tx, if this
// admits), so it passes a.stores.repoSettings.WithTx(tx); executeDispatch
// runs OUTSIDE any transaction entirely (this file's own top "#
// Sequencing" comment: no real I/O call may run while a transact's own
// FOR UPDATE lock is held, and by the time executeDispatch runs the
// turn's own Pending->Dispatched->Processing transact has already
// committed and returned -- there is no transaction left to join), so it
// passes the actor's own pool-backed a.stores.repoSettings instead,
// mirroring resolveAndSetImage's own identical no-tx store-read shape
// (imageresolve.go).
//
// transient (Phase 6 audit fix, Finding 4) mirrors
// httpapi.checkRolloutGate's own readErrored-tracking precedent
// (rolloutgate.go): true only when decision.RepoFullName -- the SPECIFIC
// repo rollout.Decide's own "first not-enrolled repo" rule stopped at --
// was forced to Enrolled == false by a genuine repo_settings READ error,
// never by a demonstrated policy fact (an absent row, an explicit
// sessions_enabled=false row, or a URL that could never resolve at all --
// none of which can ever produce a different answer on retry, mirroring
// checkRolloutGate's own identical reasoning). Meaningless when
// decision.Admitted is true; both callers below only consult it after
// confirming a refusal. Failing closed and marking a refusal as a
// permanent policy decision are two different properties -- see
// §32.5's own "Fail-closed and terminal are different properties" -- and
// before this fix, this function's dispatch-side callers conflated them
// exactly the way checkRolloutGate itself used to before its own
// adversarial-review fix (rolloutgate.go's own top comment).
func (a *Actor) rolloutDecisionForSession(ctx context.Context, repoSettings *postgres.RepoSettingsStore, sessionRow sqlcgen.Session) (decision rollout.Decision, transient bool, err error) {
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(sessionRow.Repos, &repos); err != nil {
		return rollout.Decision{}, false, fmt.Errorf("sessionactor: rollout re-check: unmarshal session repos: %w", err)
	}

	admissions := make([]rollout.RepoAdmission, 0, len(repos))
	readErrored := make([]bool, 0, len(repos))
	for _, repo := range repos {
		// Audit-hardening precedent (imageresolve.go's own
		// repoAccessAllowedForSpawn, pushpr.go, contractdrift.go):
		// CheckRepoHost FIRST, then ParseOwnerRepo -- reposource.
		// ParseOwnerRepo is deliberately host-agnostic (its own doc
		// comment), so without this pairing a spoofed cross-host URL
		// sharing an enrolled repo's own owner/repo path would silently
		// derive that SAME enrolled identity here too.
		if err := reposource.CheckRepoHost(repo.URL, ports.SupportedSourceControlHosts()...); err != nil {
			a.logger.Warn("sessionactor: rollout re-check: repo url does not name a supported source-control host; treating as not enrolled",
				"session_id", a.sessionID.String(), "url", repo.URL, "error", err)
			admissions = append(admissions, rollout.RepoAdmission{FullName: repo.URL, Enrolled: false})
			readErrored = append(readErrored, false)
			continue
		}

		owner, name, err := reposource.ParseOwnerRepo(repo.URL)
		if err != nil {
			a.logger.Warn("sessionactor: rollout re-check: parse owner/repo from clone url failed; treating as not enrolled",
				"session_id", a.sessionID.String(), "error", err)
			admissions = append(admissions, rollout.RepoAdmission{FullName: repo.URL, Enrolled: false})
			readErrored = append(readErrored, false)
			continue
		}
		fullName := owner + "/" + name

		row, err := repoSettings.Get(ctx, fullName)
		switch {
		case err == nil:
			admissions = append(admissions, rollout.RepoAdmission{FullName: fullName, Enrolled: row.SessionsEnabled})
			readErrored = append(readErrored, false)
		case errors.Is(err, pgx.ErrNoRows):
			admissions = append(admissions, rollout.RepoAdmission{FullName: fullName, Enrolled: false})
			readErrored = append(readErrored, false)
		default:
			// A genuine read error -- fail-closed (§32, mirroring
			// checkRolloutGate's own identical C3 precedent), never
			// treated as "no row, so unenrolled" without comment.
			// readErrored records this SPECIFICALLY so the transient
			// return value below can tell a degraded read apart from a
			// demonstrated policy fact -- mirrors checkRolloutGate's own
			// identical readErrored tracking (rolloutgate.go).
			a.logger.Warn("sessionactor: rollout re-check: read repo_settings failed; failing closed (treating as not enrolled)",
				"session_id", a.sessionID.String(), "repo", fullName, "error", err)
			admissions = append(admissions, rollout.RepoAdmission{FullName: fullName, Enrolled: false})
			readErrored = append(readErrored, true)
		}
	}

	decision = rollout.Decide(a.rolloutMode, admissions)
	if !decision.Admitted {
		// rollout.Decide's own doc comment: "refuses on the FIRST [repo]
		// that is not [enrolled]" -- mirrored here, over the SAME
		// admissions slice in the SAME order, so transient always
		// describes exactly the repo Decide's own decision.RepoFullName
		// names, never a different one a later, unrelated read error
		// happened to touch (mirrors checkRolloutGate's own identical
		// refusalIsTransient derivation, rolloutgate.go).
		for i, adm := range admissions {
			if !adm.Enrolled {
				transient = readErrored[i]
				break
			}
		}
	}
	return decision, transient, nil
}

// refuseIfRolloutUnenrolled implements §10's own dispatch-time half
// of the "fail-closed, twice" rule (§10 Phase 6, §32): re-checked HERE,
// immediately before tryPlanSpawn is about to attempt a REAL
// spawn/restore/resume against a.provider, using the SAME pure decision
// (internal/domain/rollout.Decide) httpapi.CreateSessionOnTx's own
// creation-time gate (checkRolloutGate, internal/adapters/inbound/httpapi/
// rolloutgate.go) already ran once at session-creation time -- mirroring
// refuseIfSubstrateUnsupported's own identical "one pure function, two
// independent call sites" shape immediately above, one Step later.
//
// This is HALF of what makes §32's own documented rollback bound real --
// see rolloutRefusalForDispatch's own doc comment (executeDispatch,
// below) for the other half, closing the gap an adversarial review found
// in this function's own original scope: this only ever runs on the
// branch of planDispatch that is about to attempt a fresh
// spawn/restore/resume (tryPlanSpawn), never on the branch that dispatches
// a Pending turn to an ALREADY-live Ready/Suspect sandbox -- exactly the
// path a re-review turn on an existing PR session takes (the REUSE
// branch, internal/adapters/inbound/github's own coalesce.go), which
// never touches tryPlanSpawn, or httpapi.CreateSessionOnTx, again.
//
// a.rolloutMode is re-read from this SAME in-memory Actor field every
// call (not re-queried from Postgres -- platform.Config is process-wide,
// boot-time config, not a runtime-mutable row) -- fresh relative to
// sessionRow's own repo_settings lookup (rolloutDecisionForSession,
// above), which IS re-read from Postgres on tx every single call, so a
// repo de-enrolled between two consecutive dispatch attempts for the SAME
// session is caught on the very next one.
//
// On refusal, this returns a real, propagated error -- deliberately NOT
// sandbox.SpawnActionSkip's own silent (nil, nil) shape, mirroring
// refuseIfSubstrateUnsupported's own identical reasoning: a Skip looks
// identical to an ordinary, expected cooldown/in-progress no-op, but a
// session whose own repo has been de-enrolled is a genuine, persistent
// policy state that must be loud (logged at Error, surfaced to run's own
// "command handling failed" logger) until re-enrolled, never a silent,
// indefinitely-retried no-op that looks identical to the sandbox merely
// waiting its turn.
//
// Phase 6 audit fix (Finding 4): this refusal now ALSO increments
// session_rollout_refused_total (recordRolloutRefusal, opsmetrics.go) --
// the SAME instrument httpapi.checkRolloutGate already increments for
// its own session-creation-time refusals, previously never touched by
// this, the dispatch-time half of the identical mechanism. The log line
// grows a "transient" attribute, and the counter increments ONLY when
// transient is false -- a refusal rolloutDecisionForSession's own
// readErrored tracking attributes to a genuine repo_settings read error,
// not a demonstrated "not enrolled" fact, must never count here, for the
// exact reason checkRolloutGate's own doc comment gives for its
// identical rule (§32.5/§32.7).
func (a *Actor) refuseIfRolloutUnenrolled(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) error {
	if a.rolloutMode != rollout.ModeCohort {
		return nil
	}

	decision, transient, err := a.rolloutDecisionForSession(ctx, a.stores.repoSettings.WithTx(tx), sessionRow)
	if err != nil {
		return fmt.Errorf("sessionactor: dispatch-time rollout re-check: %w", err)
	}
	if decision.Admitted {
		return nil
	}

	a.logger.Error("sessionactor: refusing to spawn: configured repo is not enrolled in the cohort rollout (§10 Phase 6, §32 dispatch-time fail-closed re-check)",
		"session_id", a.sessionID.String(), "repo", decision.RepoFullName, "transient", transient)
	if !transient {
		a.recordRolloutRefusal(ctx, string(sessionRow.SpawnSource))
	}
	return fmt.Errorf("sessionactor: dispatch-time rollout re-check failed: repo %q not enrolled in cohort rollout", decision.RepoFullName)
}

// planFreshSpawn implements design decision 3a's own write (token mint,
// upsert, arm connecting_deadline, assemble a Fresh-boot-mode
// SessionConfig) for the plain SpawnActionSpawn case -- pulled out of
// tryPlanSpawn's own body so planRestore (below) can share its structure
// without a blind copy-paste. Also validates the (from, trigger, gen) edge
// via sandbox.Transition before writing (§11's own hard rule, "every state
// transition goes through the machine's transition table") -- see
// planRestore's own doc comment below for why this validation is now
// shared by both paths.
func (a *Actor) planFreshSpawn(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, hasSandbox bool, sandboxRow sqlcgen.Sandbox,
	now time.Time,
) (*spawnPlan, error) {
	// §11's own hard rule applies here just as much as anywhere else:
	// EvaluateSpawnDecision (tryPlanSpawn, above) is a pure decision
	// function, not the state machine's own authority on whether this
	// particular (from, trigger) edge is actually legal -- that authority
	// is sandbox.Transition alone. fromState/currentGen mirror exactly
	// what tryPlanSpawn's own spawnState was built from (never recomputed
	// a second, possibly-inconsistent way); newGen is computed Go-side
	// but must -- and, by construction, does -- match what
	// UpsertForSpawn's own SQL (gen = sandboxes.gen + 1, or 1 on a fresh
	// insert) independently computes for the exact same row, since both
	// run inside this same already-open transact, which already holds
	// the FOR UPDATE lock on the session row (transact's own
	// GetActorEpochForUpdate) that serializes every writer of this
	// session's sandbox row -- no concurrent writer can interleave
	// between this read and UpsertForSpawn's write below.
	fromState := sandbox.StatePending
	currentGen := 0
	if hasSandbox {
		fromState = sandbox.State(sandboxRow.Status)
		currentGen = int(sandboxRow.Gen)
	}
	newGen := currentGen + 1

	var chosenTrigger sandbox.Trigger
	switch fromState {
	case sandbox.StatePending, sandbox.StateStopped, sandbox.StateFailed, sandbox.StateStale:
		// A genuinely fresh/terminal-state respawn -- no snapshot/resume
		// available (see TriggerSpawn's own doc comment).
		chosenTrigger = sandbox.SpawnTrigger(newGen)
	case sandbox.StateSpawning, sandbox.StateConnecting, sandbox.StateBooting, sandbox.StateReady:
		// EvaluateSpawnDecision's own two recovery carve-outs: a spawn/
		// connect interrupted before the sandbox ever connected, or a
		// Ready sandbox whose WebSocket never reconnected. Abandoning a
		// stuck-but-live sandbox is worth a distinct, observable log line
		// -- unlike an ordinary spawn, this is giving up on something
		// that was (or claimed to be) alive.
		chosenTrigger = sandbox.ForceRespawnTrigger(newGen)
		a.logger.Warn("sessionactor: sandbox stuck in a live state past its recovery window; force-respawning",
			"session_id", a.sessionID.String(), "from_status", string(fromState),
			"gen", currentGen, "new_gen", newGen)
	default:
		// StateSuspect can never reach here: EvaluateSpawnDecision's own
		// Suspect branch always returns Skip (no staleness carve-out), so
		// action.Kind would already be SpawnActionSkip, not
		// SpawnActionSpawn, and this function would have returned above.
		// Any OTHER unrecognized status reaching here means
		// EvaluateSpawnDecision's own logic and this trigger-selection
		// switch have drifted out of sync -- fail loudly rather than fall
		// through to an unvalidated write.
		return nil, fmt.Errorf("sessionactor: spawn decision returned Spawn from unexpected status %q; no legal trigger for it", fromState)
	}

	if _, err := sandbox.Transition(fromState, currentGen, chosenTrigger); err != nil {
		return nil, fmt.Errorf("sessionactor: sandbox transition validation for spawn (from=%s trigger=%s gen=%d->%d): %w",
			fromState, chosenTrigger.Kind, currentGen, newGen, err)
	}

	token, err := platform.GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("sessionactor: generate sandbox token: %w", err)
	}
	tokenHash := hashSandboxToken(token)

	row, err := a.stores.sandbox.WithTx(tx).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: a.sessionID,
		TokenHash: &tokenHash,
	})
	if err != nil {
		return nil, fmt.Errorf("sessionactor: upsert sandbox for spawn: %w", err)
	}

	// Arm connecting_deadline now, at spawn-write time -- closing an
	// honest pre-existing gap (nothing armed this timer in production
	// before this Step, since nothing called CreateSandbox in production
	// before this Step either): without it, a spawn that never connects
	// would never be noticed by any watchdog.
	if err := a.armTimer(ctx, tx, TimerConnectingDeadline, now.Add(a.timeouts.FirstConnectBudget)); err != nil {
		return nil, err
	}

	cfg, err := a.assembleSessionConfig(ctx, tx, sessionRow, int(row.Gen), token, row.ID.String(), sessionconfig.SessionConfigBootModeFresh)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: assemble session config: %w", err)
	}

	// Docker/EgressPolicy are threaded from cfg's own just-assembled
	// fields (§27.5/§27.6) -- the SAME deliberate top-level
	// duplication Gen already has, above: a provider must be able to act
	// on either without ever parsing the opaque SessionConfig document.
	// spec.Validate() below is what catches the two copies ever
	// diverging (it already did, once, during this Step's own
	// development -- see ports.DockerMismatchError/EgressPolicyMismatchError).
	spec := ports.CreateSpec{Gen: int(row.Gen), SessionConfig: cfg, Image: defaultBaseImage, Docker: cfg.Docker, EgressPolicy: cfg.EgressPolicy}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("sessionactor: invalid create spec: %w", err)
	}

	return &spawnPlan{gen: int(row.Gen), spec: spec, createdBy: sessionRow.CreatedBy, environmentID: sessionRow.EnvironmentID}, nil
}

// planRestore implements design decision 6's own restore-specific write.
// It reuses the EXACT SAME UpsertSandboxForSpawn upsert planFreshSpawn
// uses above -- that query's own doc comment already documents this dual
// purpose ("every spawn/restore increments sandbox.gen"), so no second,
// parallel SQL write is built here. The one difference design decision 6
// calls out: BootMode passed to assembleSessionConfig is SnapshotRestore,
// not Fresh. Both paths validate their own (from, trigger, gen) edge via
// sandbox.Transition before writing -- planFreshSpawn via sandbox.
// SpawnTrigger/ForceRespawnTrigger, this path via sandbox.RestoreTrigger --
// proving each trigger's own FROM-state/gen-fencing legality genuinely
// gates the write, rather than trusting EvaluateSpawnDecision's own coarser
// status check alone.
func (a *Actor) planRestore(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, snapshotImageID string,
	now time.Time,
) (*spawnPlan, error) {
	// newGen mirrors exactly what UpsertSandboxForSpawn's own SQL is about
	// to compute (gen = gen + 1) -- predicted here, in Go, purely so
	// sandbox.Transition can validate the (from, trigger, gen) edge BEFORE
	// any write happens. Safe to predict rather than race: this actor is
	// the session's own single writer (§2), so no concurrent write can
	// have changed sandboxRow.Gen between planDispatch's own read and this
	// call.
	newGen := int(sandboxRow.Gen) + 1
	if _, err := sandbox.Transition(sandbox.State(sandboxRow.Status), int(sandboxRow.Gen), sandbox.RestoreTrigger(newGen)); err != nil {
		return nil, fmt.Errorf("sessionactor: sandbox transition restore (stopped/failed/stale->spawning): %w", err)
	}

	token, err := platform.GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("sessionactor: generate sandbox token: %w", err)
	}
	tokenHash := hashSandboxToken(token)

	row, err := a.stores.sandbox.WithTx(tx).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: a.sessionID,
		TokenHash: &tokenHash,
	})
	if err != nil {
		return nil, fmt.Errorf("sessionactor: upsert sandbox for restore: %w", err)
	}
	if int(row.Gen) != newGen {
		// Should be unreachable (this actor is this session's own single
		// writer, per §2 -- nothing else can have written to this row
		// between the Transition validation above and this same upsert) --
		// defensive, not an expected path.
		return nil, fmt.Errorf("sessionactor: restore gen mismatch: validated %d, upsert produced %d", newGen, row.Gen)
	}

	// Same reasoning as planFreshSpawn's own identical call: a restore
	// lands in the SAME Spawning state a fresh spawn does, so it needs the
	// SAME watchdog coverage while it waits to connect.
	if err := a.armTimer(ctx, tx, TimerConnectingDeadline, now.Add(a.timeouts.FirstConnectBudget)); err != nil {
		return nil, err
	}

	cfg, err := a.assembleSessionConfig(ctx, tx, sessionRow, int(row.Gen), token, row.ID.String(), sessionconfig.SessionConfigBootModeSnapshotRestore)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: assemble session config: %w", err)
	}

	// Docker/EgressPolicy are threaded from cfg's own just-assembled
	// fields (§27.5/§27.6) -- the SAME deliberate top-level
	// duplication Gen already has, above: a provider must be able to act
	// on either without ever parsing the opaque SessionConfig document.
	// spec.Validate() below is what catches the two copies ever
	// diverging (it already did, once, during this Step's own
	// development -- see ports.DockerMismatchError/EgressPolicyMismatchError).
	spec := ports.CreateSpec{Gen: int(row.Gen), SessionConfig: cfg, Image: defaultBaseImage, Docker: cfg.Docker, EgressPolicy: cfg.EgressPolicy}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("sessionactor: invalid create spec: %w", err)
	}

	return &spawnPlan{
		gen: int(row.Gen), spec: spec, restore: true,
		snapshotID: ports.SnapshotID(snapshotImageID), createdBy: sessionRow.CreatedBy,
		environmentID: sessionRow.EnvironmentID,
	}, nil
}

// planResume implements §3.2's own resume-specific write -- the FIRST
// step of the two-step shape sandbox.TriggerResume now has (state.go's
// own doc comment): an interim "claimed, in flight" write into Spawning,
// committed inside the caller's own transact, exactly like planFreshSpawn/
// planRestore's own identical first-step write, before any provider call
// happens. This is precisely the write whose ABSENCE the adversarial
// review this fix responds to exploited: without it, nothing marked a
// Stopped/Stale row as "a resume is already in flight," so a second actor
// instance for the same session could reach EvaluateSpawnDecision's own
// Resume branch a second time and call ResumeSandbox concurrently. Once
// this write commits, that same second actor's own EvaluateSpawnDecision
// call reads status==Spawning instead of Stopped/Stale and no-ops via its
// own existing SpawningTimeout-guarded Skip branch (spawndecision.go,
// untouched by this fix) -- the exact same protection a concurrent second
// spawn/restore attempt already gets. That guard measures from the last
// sign of life (max(created_at, last_seen_at)), so it only genuinely
// fires here because UpsertSandboxForSpawn's own ON CONFLICT branch
// resets last_seen_at to now() on every claim, including this one
// (postgres/queries/sandboxes.sql's own audit finding F3 fix) -- without
// that, a box resumed after sitting Stopped/Stale well past
// SpawningTimeout (the normal case for a resume) would still measure
// sinceLastSignOfLife from however long it already sat idle, and this
// guard would NOT skip.
//
// Reuses the EXACT SAME UpsertSandboxForSpawn upsert planFreshSpawn/
// planRestore already share -- that query's own doc comment ("every
// spawn/restore increments sandbox.gen") is now genuinely true of
// resume's own first step too: the Transition-validated target for
// TriggerResume IS Spawning now, so this upsert's hardcoded
// status='spawning' needs no post-hoc correction the way the OLD
// executeResume's single-transact shape used to need (that self-correcting
// UpdateStatus call is gone -- see this Step's own PR description for the
// now-resolved §11 "no ad-hoc status writes" nit this used to represent).
//
// Deliberately does NOT build a ports.CreateSpec/SessionConfig the way
// planFreshSpawn/planRestore do: ResumeSandbox's own port signature
// (§4.1) takes only (ctx, SandboxRef) -- no CreateSpec parameter -- so
// there is nothing for a spec to be validated or passed to here. The
// returned plan's own spec field is left at its zero value; executeResume
// never reads it. A fresh token/gen is still minted and persisted
// (sandbox tokens are hashed at rest, one per gen, per §5.2 -- paraphrased,
// not verbatim -- applying to a resume's own gen bump exactly as it does
// to a spawn/restore's), even though there is today no channel to deliver
// the plaintext token to the already-running provider instance (the
// honest, deliberately-deferred gap this file's own top comment already
// documents -- unrelated to, and not
// solved by, this concurrency fix).
func (a *Actor) planResume(
	ctx context.Context, tx pgx.Tx,
	sandboxRow sqlcgen.Sandbox, providerObjectID string,
	now time.Time,
) (*spawnPlan, error) {
	// newGen mirrors exactly what UpsertSandboxForSpawn's own SQL is about
	// to compute (gen = gen + 1) -- predicted here, in Go, purely so
	// sandbox.Transition can validate the (from, trigger, gen) edge BEFORE
	// any write happens, mirroring planRestore's own identical reasoning:
	// this actor is the session's own single writer (§2), so no
	// concurrent write can have changed sandboxRow.Gen between
	// planDispatch's own read and this call.
	newGen := int(sandboxRow.Gen) + 1
	if _, err := sandbox.Transition(sandbox.State(sandboxRow.Status), int(sandboxRow.Gen), sandbox.ResumeTrigger(newGen)); err != nil {
		return nil, fmt.Errorf("sessionactor: sandbox transition resume (stopped/stale->spawning): %w", err)
	}

	token, err := platform.GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("sessionactor: generate sandbox token: %w", err)
	}
	tokenHash := hashSandboxToken(token)

	row, err := a.stores.sandbox.WithTx(tx).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: a.sessionID,
		TokenHash: &tokenHash,
	})
	if err != nil {
		return nil, fmt.Errorf("sessionactor: upsert sandbox for resume: %w", err)
	}
	if int(row.Gen) != newGen {
		// Should be unreachable -- this actor is this session's own single
		// writer, per §2 -- mirroring planRestore's own identical
		// defensive check.
		return nil, fmt.Errorf("sessionactor: resume gen mismatch: validated %d, upsert produced %d", newGen, row.Gen)
	}

	// Same reasoning as planFreshSpawn/planRestore's own identical call: a
	// resume now lands in the SAME Spawning state a fresh spawn/restore
	// does, so it needs the SAME watchdog coverage while it waits for
	// ResumeSandbox to return and the sandbox to reconnect its WS.
	if err := a.armTimer(ctx, tx, TimerConnectingDeadline, now.Add(a.timeouts.FirstConnectBudget)); err != nil {
		return nil, err
	}

	return &spawnPlan{gen: int(row.Gen), resume: true, providerObjectID: providerObjectID}, nil
}

// executeSpawn performs the actual (possibly slow, network-bound)
// CreateSandbox call OUTSIDE any transaction, then records the outcome in
// a SECOND, fresh transact -- design decision 3a's own required
// sequencing.
//
// This was a known, documented limitation pending §5.3's reconciler
// (§5.3's own "reconciler + GC ... orphan reaping"), which now exists
// (internal/app/reconciler): if CreateSandbox
// above genuinely succeeds (createErr == nil, a real cloud resource now
// exists under ref.ProviderID) but this actor's own epoch has gone stale
// by the time the transact below runs (a legitimate pod-handoff race -- a
// newer actor already took over and bumped actor_epoch), transact's own
// epoch-fencing check correctly rolls back this whole write, INCLUDING the
// UpdateProviderID call that would have been the only durable record of
// ref.ProviderID anywhere. reconciler.Reconciler.ReconcileOnce (§9.3
// scenario 5: "two concurrent spawns ... loser sandbox reaped by GC") now
// catches exactly this: its own provider.List call sees this real,
// orphaned cloud resource with no corresponding live Postgres row and
// reaps it on a later tick -- covering every SUCH ORPHAN CREATED FROM NOW
// ON. It has no way to retroactively find/reap a resource that leaked
// before this reconciler existed; the log below remains as an immediate,
// session/gen-correlatable signal alongside the reconciler's own
// coarser-grained, provider-id-only reaping log.
func (a *Actor) executeSpawn(ctx context.Context, plan *spawnPlan) error {
	start := time.Now()
	ref, createErr := a.provider.CreateSandbox(ctx, plan.spec)
	a.recordSpawnDuration(ctx, "spawn", time.Since(start).Seconds(), createErr != nil)

	err := a.recordProviderOutcome(ctx, plan.gen, ref, createErr)

	if err != nil && createErr == nil && errors.Is(err, ErrStaleEpoch) {
		// See this function's own doc comment above: a real cloud sandbox
		// was just created but the write recording it just got rolled back
		// by a legitimate stale-epoch takeover -- internal/app/reconciler
		// now reaps this automatically on a later tick; this log remains as
		// an immediate, session/gen-correlatable signal alongside that.
		a.logger.Warn("sessionactor: spawned sandbox orphaned by stale-epoch takeover; provider resource was never recorded here and will be reaped by the reconciler's own next tick",
			"session_id", a.sessionID.String(),
			"provider_id", ref.ProviderID,
			"gen", plan.gen,
		)
	}

	return err
}

// executeRestore mirrors executeSpawn exactly (§3.2, "snapshots &
// restore", design decision 6), except it calls RestoreFromSnapshot
// instead of CreateSandbox -- see recordProviderOutcome (below) for the
// second-transact outcome-recording half both now share, including the
// SAME recordSpawnFailure/circuit-breaker reuse a permanent
// RestoreFromSnapshot failure gets (that function's own doc comment
// explains why a restore failure is still fundamentally "we tried to get
// a live sandbox and a permanent provider error came back" -- the SAME
// concern a spawn failure is, not a different one). Shares executeSpawn's
// own stale-epoch-orphan case too: a real restored provider sandbox that
// gets orphaned this way is caught by internal/app/reconciler exactly
// like a spawned one is -- see executeSpawn's own doc comment above for
// the full writeup.
func (a *Actor) executeRestore(ctx context.Context, plan *spawnPlan) error {
	start := time.Now()
	ref, restoreErr := a.provider.RestoreFromSnapshot(ctx, plan.snapshotID, plan.spec)
	a.recordSpawnDuration(ctx, "restore", time.Since(start).Seconds(), restoreErr != nil)

	err := a.recordProviderOutcome(ctx, plan.gen, ref, restoreErr)

	if err != nil && restoreErr == nil && errors.Is(err, ErrStaleEpoch) {
		a.logger.Warn("sessionactor: restored sandbox orphaned by stale-epoch takeover; provider resource was never recorded here and will be reaped by the reconciler's own next tick",
			"session_id", a.sessionID.String(),
			"provider_id", ref.ProviderID,
			"gen", plan.gen,
		)
	}

	return err
}

// recordProviderOutcome is executeSpawn/executeRestore's own shared
// second-transact outcome-recording step (§3.2, design decision 6:
// "sharing whatever helper logic is genuinely common between the two...
// rather than duplicating it blindly") -- the exact same transact body
// §9.3's own executeSpawn used to run inline, unchanged in behavior:
// given the plan's own gen and the just-returned (ref, providerErr) from
// the real provider call, either records success (provider_id + the SAME
// Spawning->Connecting transition both a fresh spawn and a restore land
// in) or classifies+records a failure via recordSpawnFailure. Returns
// ErrStaleEpoch unchanged on a legitimate pod-handoff race, exactly like
// the inline transact this replaces already did.
func (a *Actor) recordProviderOutcome(ctx context.Context, gen int, ref ports.SandboxRef, providerErr error) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		if int(sandboxRow.Gen) != gen {
			// A newer spawn/restore attempt has already superseded this
			// one (e.g. a stale-spawn recovery elsewhere raced this call)
			// -- this result no longer applies to anything.
			a.logger.Warn("sessionactor: provider result for a superseded gen; ignoring",
				"result_gen", gen, "current_gen", sandboxRow.Gen)
			return nil
		}

		if providerErr != nil {
			return a.recordSpawnFailure(ctx, tx, sandboxRow, providerErr, now)
		}

		if _, err := a.stores.sandbox.WithTx(tx).UpdateProviderID(ctx, sqlcgen.UpdateSandboxProviderIDParams{
			SessionID:  a.sessionID,
			ProviderID: &ref.ProviderID,
		}); err != nil {
			return fmt.Errorf("sessionactor: record provider id: %w", err)
		}

		to, err := sandbox.Transition(sandbox.State(sandboxRow.Status), int(sandboxRow.Gen), sandbox.ProviderAckTrigger())
		if err != nil {
			return fmt.Errorf("sessionactor: sandbox transition spawning->connecting: %w", err)
		}
		if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
			SessionID: a.sessionID,
			Status:    sqlcgen.SandboxStatus(to),
		}); err != nil {
			return fmt.Errorf("sessionactor: update sandbox status to connecting: %w", err)
		}
		return nil
	})
}

// recordSpawnFailure classifies createErr (ports.IsTransient, §3.2's own
// "unknown provider errors default to transient" contract) and either
// leaves the sandbox in Spawning for a later retry (transient) or
// increments the persisted circuit breaker and transitions Spawning->
// Suspect (permanent) -- reusing transitionSandboxToSuspect (timerfired.go)
// so a permanent spawn failure resolves to Failed via the SAME terminal_
// grace machinery every watchdog timeout already uses (§3.2: "a watchdog
// never writes failed directly").
func (a *Actor) recordSpawnFailure(ctx context.Context, tx pgx.Tx, sandboxRow sqlcgen.Sandbox, createErr error, now time.Time) error {
	if ports.IsTransient(createErr) {
		a.logger.Warn("sessionactor: transient spawn failure; left in spawning for a later retry", "error", createErr)
		return nil
	}

	var code string
	var perr *ports.ProviderError
	if errors.As(createErr, &perr) {
		code = perr.Code
	}
	a.logger.Error("sessionactor: permanent spawn failure; transitioning to suspect pending grace",
		"error", createErr, "provider_error_code", code)

	newCount := sandboxRow.SpawnFailureCount + 1
	if _, err := a.stores.sandbox.WithTx(tx).UpdateCircuitBreaker(ctx, sqlcgen.UpdateSandboxCircuitBreakerParams{
		SessionID:          a.sessionID,
		SpawnFailureCount:  newCount,
		LastSpawnFailureAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return fmt.Errorf("sessionactor: record spawn failure: %w", err)
	}

	// watchdog is deliberately "" here: this is a permanent, already-
	// classified provider error, not a watchdog-style liveness silence --
	// see watchdogKind's own doc comment (opsmetrics.go) for why this path
	// records neither watchdog_activation_total nor
	// sandbox_liveness_gap_seconds.
	if err := a.transitionSandboxToSuspect(ctx, tx, sandboxRow, now, "", 0); err != nil {
		return err
	}
	// connecting_deadline no longer applies -- the sandbox is Suspect now,
	// awaiting terminal_grace's own resolution instead.
	return a.deleteTimer(ctx, tx, TimerConnectingDeadline)
}

// executeResume (§3.2, "resume") performs the actual (possibly slow,
// network-bound) SandboxProvider.ResumeSandbox call OUTSIDE any
// transaction, exactly like executeSpawn/executeRestore's own network
// call -- and, following this fix, the REST of its own shape now
// genuinely matches theirs too, rather than being a special case: by the
// time this function is ever called, plan's own interim Spawning claim
// (planResume, above) has ALREADY committed inside tryPlanSpawn's own
// transact -- exactly like planFreshSpawn/planRestore's own Spawning
// write already commits before executeSpawn/executeRestore ever call out
// to the provider. This is precisely what closes the concurrency bug an
// adversarial review found and empirically reproduced in the OLD shape
// (killing actor A's advisory-lock connection mid-resume-call, hydrating
// actor B for the same session, and getting a SECOND ResumeSandbox call
// while actor A's first call was still blocked): a second actor instance
// for the same session now reads status==Spawning, not Stopped/Stale, and
// EvaluateSpawnDecision's own existing guard (spawndecision.go,
// untouched by this fix) no-ops instead of returning SpawnActionResume a
// second time -- genuinely, because planResume's own UpsertSandboxForSpawn
// claim just reset last_seen_at to now() on the ON CONFLICT branch (audit
// finding F3's fix, postgres/queries/sandboxes.sql), so the guard's own
// sinceLastSignOfLife reads as "just now," not however long this box sat
// Stopped/Stale beforehand -- see planResume's own doc comment above for
// the full reasoning.
//
// ref.ProviderID is the EXISTING provider object recorded on plan (never
// a new one, unlike executeSpawn/executeRestore's own ref, which comes
// back FROM the provider call) -- ResumeSandbox's own port signature
// returns only an error, so there is no ref to overwrite here, and none
// is read from the return value.
func (a *Actor) executeResume(ctx context.Context, plan *spawnPlan) error {
	ref := ports.SandboxRef{ProviderID: plan.providerObjectID}
	start := time.Now()
	resumeErr := a.provider.ResumeSandbox(ctx, ref)
	a.recordSpawnDuration(ctx, "resume", time.Since(start).Seconds(), resumeErr != nil)

	err := a.recordResumeOutcome(ctx, plan.gen, resumeErr)

	if err != nil && resumeErr == nil && errors.Is(err, ErrStaleEpoch) {
		// Mirrors executeSpawn/executeRestore's own identical stale-epoch
		// observability log: a real ResumeSandbox call just succeeded, but
		// the write recording its outcome (Connecting status) was rolled
		// back by a legitimate stale-epoch takeover -- the resumed
		// provider instance is real and live, but the control plane's own
		// bookkeeping for it never got recorded. Same reconciler coverage
		// as executeSpawn/executeRestore's own equivalent case: whichever
		// actor instance eventually WINS this session's own takeover race
		// records ITS OWN provider_id on this row (UpsertSandboxForSpawn's
		// own doc comment: "provider_id is deliberately NOT cleared" on a
		// fresh spawn -- it is simply overwritten by the next successful
		// UpdateProviderID call), so this resumed instance's provider_id
		// stops being referenced by ANY row at all once that happens --
		// internal/app/reconciler's own provider.List-vs-Postgres
		// comparison reaps it exactly like a spawn/restore orphan.
		a.logger.Warn("sessionactor: resumed sandbox's outcome orphaned by stale-epoch takeover; the resume itself succeeded at the provider but was never recorded here and will be reaped by the reconciler's own next tick",
			"session_id", a.sessionID.String(),
			"provider_id", plan.providerObjectID,
			"gen", plan.gen,
		)
	}

	return err
}

// recordResumeOutcome is executeResume's own second-transact outcome-
// recording step -- mirrors recordProviderOutcome (executeSpawn/
// executeRestore's own shared equivalent) almost exactly, with exactly
// ONE deliberate difference: it never calls UpdateProviderID. Resume
// reuses the SAME already-recorded provider object (never a new one), so
// there is nothing for that write to record that isn't already there --
// calling it anyway would be a no-op at best and, at worst, a lie about
// what actually happened (recordProviderOutcome's own UpdateProviderID
// call exists specifically to record a NEW object CreateSandbox/
// RestoreFromSnapshot just returned).
//
// On success: applies sandbox.ResumeAckTrigger() (Spawning -> Connecting)
// instead of recordProviderOutcome's sandbox.ProviderAckTrigger() --
// state.go's own doc comment on TriggerResumeAck explains why this is a
// distinct trigger kind, not a reuse of TriggerProviderAck, even though
// both share the exact same (from, to) edge: the transition LOG (§5.3)
// can tell "a spawn/restore's provider call acked" apart from "a resume's
// provider call acked" the same way TriggerForceRespawn is already kept
// distinct from TriggerSpawn despite sharing a target.
//
// On failure: reuses recordSpawnFailure UNCHANGED. This is now correct,
// not merely convenient: a permanent resume failure has, by this fix, had
// the exact same interim Spawning write a permanent spawn/restore failure
// has by the time its own outcome is recorded, so it now behaves
// IDENTICALLY -- same circuit-breaker increment, same
// transitionSandboxToSuspect path (Spawning -> Suspect, pending
// terminal_grace, §3.2's "a watchdog never writes failed directly" rule),
// same connecting_deadline cleanup. There is no longer a genuinely
// distinct resume-failure code path to maintain, so none is kept: the OLD
// recordResumeFailure (which left the row completely untouched on a
// permanent failure, because there was nothing yet to transition out of)
// is gone.
func (a *Actor) recordResumeOutcome(ctx context.Context, gen int, resumeErr error) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()

		sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get sandbox: %w", err)
		}

		if int(sandboxRow.Gen) != gen {
			// A newer spawn/restore/resume attempt has already superseded
			// this one -- mirrors recordProviderOutcome's own identical
			// guard.
			a.logger.Warn("sessionactor: resume result for a superseded gen; ignoring",
				"result_gen", gen, "current_gen", sandboxRow.Gen)
			return nil
		}

		if resumeErr != nil {
			return a.recordSpawnFailure(ctx, tx, sandboxRow, resumeErr, now)
		}

		to, err := sandbox.Transition(sandbox.State(sandboxRow.Status), int(sandboxRow.Gen), sandbox.ResumeAckTrigger())
		if err != nil {
			return fmt.Errorf("sessionactor: sandbox transition spawning->connecting (resume ack): %w", err)
		}
		if _, err := a.stores.sandbox.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
			SessionID: a.sessionID,
			Status:    sqlcgen.SandboxStatus(to),
		}); err != nil {
			return fmt.Errorf("sessionactor: update sandbox status to connecting (resume ack): %w", err)
		}
		return nil
	})
}

// tryPlanDispatch implements design decision 3b's own first half, entirely
// inside the caller's own transact: both turn transitions (Pending->
// Dispatched->Processing) and arming turn_deadline commit together. The
// actual SandboxCommander.SendCommand call is deliberately NOT made here
// -- see this file's own top comment for why a real WS frame write must
// never run while this transact's own FOR UPDATE lock on the session row
// is held -- it happens in executeDispatch, OUTSIDE any transaction, once
// this has committed.
func (a *Actor) tryPlanDispatch(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox,
	turnID pgtype.UUID, turns []sqlcgen.Turn,
	now time.Time,
) (*dispatchPlan, error) {
	if a.commander == nil {
		// Defensive: mirrors tryPlanSpawn's own nil-provider guard exactly
		// -- some tests, and any future caller genuinely without one, must
		// not panic or half-write state here. A later EnsureDispatched
		// (once a commander IS configured) retries; the turn is left
		// untouched (still Pending).
		a.logger.Warn("sessionactor: dispatch decision would proceed but no SandboxCommander is configured; skipping")
		return nil, nil
	}

	target, ok := findTurnByID(turns, turnID)
	if !ok {
		return nil, fmt.Errorf("sessionactor: dispatch turn: turn %s not found among loaded turns", turnID.String())
	}

	toDispatched, err := turn.Transition(turn.StatePending, turn.TriggerDispatch)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: turn transition pending->dispatched: %w", err)
	}
	// dispatched_sandbox_gen (§3.3, "turn recovery") is stamped here,
	// alongside dispatched_at, with the sandbox's CURRENT gen -- the
	// fencing value planDispatch's own new re-enqueue gate later compares
	// against the sandbox row's live gen to tell "already correctly
	// dispatched to this sandbox incarnation" from "sent to a
	// now-superseded one" (see migrations/000026_turn_dispatch_gen.up.sql's
	// own doc comment for the full reasoning).
	dispatchedGen := sandboxRow.Gen
	// Stamped alongside dispatched_at/dispatched_sandbox_gen, in the SAME
	// write and the SAME transaction: the events-log high-water mark at
	// this instant. §26.4's corroboration queries use it as a clock-free
	// lower bound for "this turn's own dispatch", replacing a created_at >=
	// dispatched_at comparison that straddled the Postgres and application
	// clocks -- see migrations/000089_turns_dispatched_event_id.up.sql.
	// dispatched_at itself stays exactly as it was: it still has a
	// genuine, same-clock consumer in turn.EvaluateTurnDeadline.
	dispatchedEventID, err := a.stores.event.WithTx(tx).MaxEventIDForSession(ctx, a.sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: read events high-water mark for dispatch: %w", err)
	}
	// messageID (finding F3 (§21.1's amendment)) is generated ONCE, here, then
	// threaded into BOTH the wire payload below AND this SAME
	// UpdateStatus write (turns.dispatched_message_id) -- see
	// BuildPromptPayload's own doc comment for the full "why": this is
	// the ONE identifier httpapi.PostReviewVerdict later resolves a
	// verdict-posting turn BY, instead of "whichever turn is processing
	// for this session right now".
	messageID := uuid.NewString()
	if _, err := a.stores.turn.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   turnID,
		Status:               sqlcgen.TurnStatus(toDispatched),
		DispatchedAt:         pgtype.Timestamptz{Time: now, Valid: true},
		DispatchedSandboxGen: &dispatchedGen,
		DispatchedEventID:    &dispatchedEventID,
		DispatchedMessageID:  &messageID,
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: update turn status to dispatched: %w", err)
	}

	// §3.3: dispatched -> processing happens immediately here too -- this
	// Step's own scope has no separate "the sandbox acknowledged receipt"
	// signal to gate the second transition on (see design decision 3b's
	// own reasoning); "we successfully sent it" is treated as sufficient
	// for both. Note this now commits BEFORE SendCommand is ever attempted
	// (see this file's own top comment) -- if the send subsequently fails,
	// the turn is failed forward from here (executeDispatch/
	// failDispatchedTurn), never rolled back to Pending.
	toProcessing, err := turn.Transition(turn.StateDispatched, turn.TriggerStartProcessing)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: turn transition dispatched->processing: %w", err)
	}
	if _, err := a.stores.turn.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:     turnID,
		Status: sqlcgen.TurnStatus(toProcessing),
	}); err != nil {
		return nil, fmt.Errorf("sessionactor: update turn status to processing: %w", err)
	}

	if err := a.armTimer(ctx, tx, TimerTurnDeadline, now.Add(a.timeouts.TurnDeadline)); err != nil {
		return nil, err
	}

	payload, err := BuildPromptPayload(a.sessionID.String(), sessionRow, sandboxRow, target, messageID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: build prompt payload: %w", err)
	}

	// The review's own GitHub-native result surface (§8.2/§21.1/§21.1b):
	// a github-origin session is, by construction, a
	// review session (internal/adapters/inbound/github's own doc.go --
	// github_pr_sessions is the only mechanism that ever creates one).
	// This turn just transitioned Pending -> Dispatched -> Processing
	// (above, this SAME transaction) -- exactly the moment a review
	// attempt genuinely begins, so this is the ONE call site for
	// reviewcheck.PhaseRunning. Enqueued in the SAME transaction as the
	// turn's own status write (§5.1). Every non-github session (the
	// overwhelming majority of dispatches) pays nothing beyond this one
	// SpawnSource comparison.
	if sessionRow.SpawnSource == sqlcgen.SessionSpawnSourceGithub {
		if err := a.enqueueReviewCheckRunning(ctx, tx, target); err != nil {
			return nil, err
		}
	}

	return &dispatchPlan{turnID: turnID, payload: payload, sessionRow: sessionRow}, nil
}

// executeDispatch performs the actual SandboxCommander.SendCommand call
// OUTSIDE any transaction -- design decision 3b's own required sequencing,
// mirroring executeSpawn's own "network call, then (only if needed) a
// second small transact records the outcome" shape. The turn is already
// committed Processing by the time this runs (tryPlanDispatch's own
// transact, already committed) -- on success there is nothing further to
// do; on failure, failDispatchedTurn fails it forward.
//
// §10's own SECOND, independent placement of the "fail-closed, twice"
// rollout re-check (§10 Phase 6, §32) -- closing the gap an adversarial
// review found: refuseIfRolloutUnenrolled (tryPlanSpawn, above) already
// re-checks before a fresh spawn/restore/resume, but that check runs ONLY
// on the branch of planDispatch that is about to attempt one -- an
// already-Ready/Suspect sandbox's own Pending-turn dispatch
// (tryPlanDispatch) and in-flight-turn re-enqueue (tryPlanReenqueue) never
// go through tryPlanSpawn at all (see planDispatch's own branch (b) and
// planReenqueueOrRespawn's own branch (b') doc comments). Rather than
// adding a SECOND remembered call site inside each of those two planning
// functions individually (and a third, fourth, ... for any future one),
// the check is placed here instead: executeDispatch is the ONE function
// every *dispatchPlan -- regardless of which planning branch produced it,
// today or in the future -- must pass through before
// a.commander.SendCommand is ever called (handleEnsureDispatched's own
// switch has exactly one consumer of a non-nil dispatch: this function).
// Gating here, immediately before the real SendCommand call, makes the
// bypass this Step's own review found structurally unrepresentable rather
// than merely undocumented -- a bare miss in ONE of several remembered
// call sites can no longer silently let a de-enrolled repo's session keep
// being dispatched.
//
// This runs AFTER tryPlanDispatch/tryPlanReenqueue's own transact has
// already committed the turn Processing (this file's own top "#
// Sequencing" comment: a real network call -- and this rollout read is
// exactly that -- must never run while a transact's own FOR UPDATE lock
// is held), so a refusal here cannot roll that commit back. It fails the
// turn FORWARD instead, via the EXACT SAME Processing->Failed machinery a
// genuine SendCommand failure already uses below (failDispatchedTurn) --
// this is deliberately not a new, parallel terminal path: a turn this
// actor has decided it will never actually deliver to a sandbox, for
// WHATEVER reason (a transport failure or a policy refusal), reaches the
// SAME terminal state the SAME way, with only the reason text differing.
func (a *Actor) executeDispatch(ctx context.Context, plan *dispatchPlan) error {
	if repo, refused, transient := a.rolloutRefusalForDispatch(ctx, plan.sessionRow); refused {
		a.logger.Error("sessionactor: refusing to dispatch turn: configured repo is not enrolled in the cohort rollout (§10 Phase 6, §32 turn-dispatch-time fail-closed re-check)",
			"session_id", a.sessionID.String(), "turn_id", plan.turnID.String(), "repo", repo, "transient", transient)
		if !transient {
			a.recordRolloutRefusal(ctx, string(plan.sessionRow.SpawnSource))
		}
		return a.failDispatchedTurn(ctx, plan.turnID, fmt.Sprintf("repo %q not enrolled in cohort rollout", repo))
	}

	if err := a.commander.SendCommand(a.sessionID.String(), plan.payload); err != nil {
		// Covers ports.ErrNoLiveSandboxConnection (the prompt genuinely
		// never reached a live connection) and every other send failure
		// identically.
		a.logger.Error("sessionactor: dispatch turn: send prompt command failed; failing turn",
			"turn_id", plan.turnID.String(), "error", err)
		return a.failDispatchedTurn(ctx, plan.turnID, fmt.Sprintf("failed to deliver prompt to sandbox: %v", err))
	}
	return nil
}

// rolloutRefusalForDispatch implements executeDispatch's own turn-
// dispatch-time rollout re-check -- see that function's own doc comment
// for the full "why here, why this is the one placement that makes the
// bypass structurally unrepresentable" reasoning. Shares
// rolloutDecisionForSession's own resolution logic with
// refuseIfRolloutUnenrolled (tryPlanSpawn's own spawn-time re-check,
// above) so the two gates can never drift to a different definition of
// "enrolled" from one another; only the connection this runs on differs
// -- the actor's own pool-backed a.stores.repoSettings, never a.WithTx,
// since this runs OUTSIDE any transaction (plan's own transact has
// already committed and returned by the time executeDispatch calls this).
//
// Returns ("", false, false) whenever dispatch is admitted (every
// non-cohort rollout mode, or cohort mode with every one of sessionRow's
// own named repos enrolled); (repoFullName/"<unresolvable repos>", true,
// transient) on a refusal -- transient is true ONLY for a genuine
// repo_settings read error (rolloutDecisionForSession's own readErrored
// tracking), never for the "sessionRow.Repos itself is malformed" branch
// below: a structurally malformed row can never produce a different
// answer on retry, exactly like an unresolvable URL, so it is a
// demonstrated (non-transient) refusal for recordRolloutRefusal's own
// purposes (Phase 6 audit fix, Finding 4) even though it is not itself a
// rollout.Decision.
func (a *Actor) rolloutRefusalForDispatch(ctx context.Context, sessionRow sqlcgen.Session) (repo string, refused bool, transient bool) {
	if a.rolloutMode != rollout.ModeCohort {
		return "", false, false
	}

	decision, transientDecision, err := a.rolloutDecisionForSession(ctx, a.stores.repoSettings, sessionRow)
	if err != nil {
		// sessionRow.Repos failing to unmarshal here can only mean a
		// structurally malformed row -- validateCreateSessionRequest
		// already guarantees this can never happen for a row this control
		// plane itself wrote -- fail-closed (refuse) rather than silently
		// admitting a dispatch this actor cannot even evaluate, mirroring
		// every other read-error branch in this Step's own rollout
		// checks.
		a.logger.Error("sessionactor: turn-dispatch rollout re-check: resolve admission decision failed; failing closed (refusing dispatch)",
			"session_id", a.sessionID.String(), "error", err)
		return "<unresolvable repos>", true, false
	}
	if decision.Admitted {
		return "", false, false
	}
	return decision.RepoFullName, true, transientDecision
}

// failDispatchedTurn transitions turnID -- already committed Processing by
// tryPlanDispatch/tryPlanReenqueue's own transact -- to Failed, in its own
// small, fresh transact (mirroring executeSpawn's own "network call
// already happened outside any transaction; a separate, small transact
// now records its outcome" shape exactly). domain/turn's transition table
// has no reverse edge from Processing back to Pending, and none is added
// here (internal/domain/turn/state.go is explicitly off-limits this
// Step), so the only legal move is forward.
//
// Two callers reach this, both from executeDispatch: a genuine
// SandboxCommander.SendCommand failure (this function's ORIGINAL, §9.3
// reason for existing), and §10's own turn-dispatch-time rollout
// refusal (rolloutRefusalForDispatch, above) -- deliberately the SAME
// terminal path for both, not two parallel ones: from the turn's own
// perspective, "the actor decided this prompt will never reach a
// sandbox" is one event, regardless of whether the proximate cause was a
// transport failure or a policy refusal. reason is the caller's own
// honest, human-readable account of WHICH -- carried verbatim into the
// synthetic execution_complete event's own "reason" field (never
// re-derived or classified further here).
//
// This reuses the EXACT SAME domain/turn call and "append a synthetic
// execution_complete event" logic handleTurnDeadlineTimer (timerfired.go)
// already uses for its own turn_deadline expiry: turn.Transition(
// turn.StateProcessing, turn.TriggerTimeout), gated by
// turn.RequiresSyntheticExecutionComplete, then turn.DeriveFailureReason.
// TriggerTimeout, specifically, is the only one of domain/turn's two
// Processing->Failed forward edges that fits either caller: the OTHER
// one, TriggerFail, is documented (synthetic.go) to mean "a REAL terminal
// event reporting failure already arrived from the agent" and
// RequiresSyntheticExecutionComplete(TriggerFail) is false BECAUSE of that
// -- using it here would silently skip the synthetic execution_complete
// event entirely, since no real terminal event ever arrives for a prompt
// that never reached the sandbox at all, breaking §3.3's "clients always
// see one terminal event per turn" contract. TriggerTimeout is, like
// either failure, a pure control-plane-internal decision with no real
// wire event behind it, so it is the correct existing edge to reuse for
// both -- the resulting session-level FailureReason reads "timeout" even
// though the proximate cause may be a send failure OR a rollout refusal,
// neither a deadline expiry, which is an honest, documented consequence
// of domain/turn's existing trigger vocabulary (state.go off-limits this
// Step), not a new gap invented by either caller: reason's own real text
// is what preserves the true cause, honestly, in the synthetic event's
// own "reason" field -- FailureReason is a coarse, four-value
// classification (§3.3), never meant to distinguish every possible cause
// within "the control plane gave up on this turn before a real terminal
// event could ever arrive".
func (a *Actor) failDispatchedTurn(ctx context.Context, turnID pgtype.UUID, reason string) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: list turns: %w", err)
		}

		target, ok := findTurnByID(turns, turnID)
		if !ok {
			// Gone via some other path entirely (e.g. session teardown)
			// by the time this runs -- nothing further to do.
			a.logger.Warn("sessionactor: fail dispatched turn: turn no longer found; ignoring", "turn_id", turnID.String())
			return nil
		}
		if turn.State(target.Status) != turn.StateProcessing {
			// Already resolved via some other path -- do not re-fail an
			// already-terminal (or otherwise no-longer-Processing) turn.
			a.logger.Warn("sessionactor: fail dispatched turn: turn no longer processing; ignoring",
				"turn_id", turnID.String(), "status", target.Status)
			return nil
		}

		to, err := turn.Transition(turn.StateProcessing, turn.TriggerTimeout)
		if err != nil {
			return fmt.Errorf("sessionactor: turn transition processing->failed (dispatch failure): %w", err)
		}

		now := time.Now()
		if _, err := a.stores.turn.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
			ID:          turnID,
			Status:      sqlcgen.TurnStatus(to),
			CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("sessionactor: update turn status: %w", err)
		}

		// §25.6/§25.9 ("workflow execution engine" / "workflow HITL gate +
		// circuit breaker", §25.6/§25.9): this turn just reached a real
		// terminal state because its prompt never even reached the sandbox
		// -- see OnTurnCompleted's own doc comment for why this, pushpr.go's
		// completeProcessingTurn, and timerfired.go's handleTurnDeadlineTimer
		// all three need this same hook.
		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session: %w", err)
		}
		workflowengine.OnTurnCompleted(ctx, workflowengine.Deps{
			Workflows:             a.stores.workflow.WithTx(tx),
			Turns:                 a.stores.turn.WithTx(tx),
			SlackThreadSessions:   a.stores.slackThreadSession.WithTx(tx),
			LinearAgentSessions:   a.stores.linearAgentSession.WithTx(tx),
			GitHubPRSessions:      a.stores.githubPRSession.WithTx(tx),
			Outbox:                a.stores.outbox.WithTx(tx),
			EpistemicCheckDefault: a.epistemicCheckDefault,
		}, sessionRow, turnID, turn.TriggerTimeout)

		if turn.RequiresSyntheticExecutionComplete(turn.TriggerTimeout) {
			if err := a.appendEvent(ctx, tx, "execution_complete", map[string]any{
				"turn_id":   turnID.String(),
				"synthetic": true,
				"reason":    reason,
			}); err != nil {
				return err
			}
		}

		failureReason, _ := turn.DeriveFailureReason(turn.StateProcessing, turn.TriggerTimeout)
		if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverride(turns, turnID, to, failureReason)); err != nil {
			return err
		}
		return a.deleteTimer(ctx, tx, TimerTurnDeadline)
	})
}

// BuildPromptPayload marshals a real, schema-valid sandboxws.Prompt for
// turnID (§3.3: "the turn records the OpenCode conversation id at turn
// start... so follow-up prompts on a fresh sandbox resume the same
// conversation").
//
// messageID (finding F3 (§21.1's amendment)) is the wire Prompt's own MessageId --
// deliberately a CALLER-SUPPLIED parameter, never generated inside this
// function (as it was before this fix): both real call sites (below)
// generate it ONCE, then thread the SAME value into both this payload and
// the SAME UpdateTurnStatus write that already stamps
// dispatched_sandbox_gen/dispatched_event_id (turns.dispatched_message_id,
// migrations/000131), so the wire value the sandbox actually receives and
// the value httpapi.PostReviewVerdict later resolves a verdict-posting
// turn BY are provably the SAME identifier, never two independently-
// generated ones that could drift. assertIdenticalPrompts (workflowengine_
// characterization_integration_test.go) strips this field before
// comparing, exactly as it already did when this function generated it
// internally -- a caller-supplied value changes nothing about that test's
// own byte-identity claim, which was never about MessageId in the first
// place.
//
// Exported ("workflow execution engine", §25.6) specifically so
// internal/adapters/inbound/httpapi's own characterization test
// (workflowengine_characterization_integration_test.go) can call the EXACT
// same function real dispatch uses to build the wire payload from a turn row,
// for BOTH a turn built via today's engine-mediated createTurnLocked and a
// hand-constructed turn simulating the pre-existing direct-dispatch shape
// -- proving the two produce byte-identical sandboxws.Prompt JSON is the
// whole point of that test, so it must call the real function, never a
// re-derived approximation of it. Mirrors CreateSessionCore/CreateTurnCore's
// own precedent of exporting specifically to let a caller/test across a
// package boundary reach the real, single implementation (see turn.go's
// own doc comments) -- a pure rename, no behavior change: both call sites
// below are unaffected other than the name.
func BuildPromptPayload(sessionID string, sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, target sqlcgen.Turn, messageID string) (json.RawMessage, error) {
	prompt := sandboxws.Prompt{
		Type:      "prompt",
		MessageId: messageID,
		SessionId: sessionID,
		Gen:       int(sandboxRow.Gen),
		// nil (first turn) or the session's own previously-recorded
		// conversation id (§3.3) -- sessions.opencode_conversation_id is
		// nil until the first heartbeat ever reports one.
		ConversationId: sessionRow.OpencodeConversationID,
		Text:           stringOrEmpty(target.Prompt),
		Model:          target.ModelID,
		// (§29.8): turns.effort mirrors turns.model_id's own
		// dispatch-time threading exactly -- this was the ONE remaining
		// hardcoded nil the spec's own "verified end-to-end" research
		// named explicitly (BuildPromptPayload hardcodes Effort: nil
		// because no column fed it) -- turns.effort now does.
		Effort:   target.Effort,
		ScmName:  scmCommitName,
		ScmEmail: scmCommitEmail,
		PlanMode: target.PlanMode,
	}
	return json.Marshal(prompt)
}

// toQueueEntries adapts stored turn rows into the generic
// []turn.QueueEntry[pgtype.UUID] shape HasInFlightTurn/NextToDispatch
// need (domain/turn has zero external dependencies, so it cannot know
// about pgtype.UUID or sqlcgen.Turn itself).
func toQueueEntries(turns []sqlcgen.Turn) []turn.QueueEntry[pgtype.UUID] {
	out := make([]turn.QueueEntry[pgtype.UUID], len(turns))
	for i, t := range turns {
		out[i] = turn.QueueEntry[pgtype.UUID]{ID: t.ID, Status: turn.State(t.Status)}
	}
	return out
}

// findTurnByID returns the turn in turns whose ID matches id, if any.
func findTurnByID(turns []sqlcgen.Turn, id pgtype.UUID) (sqlcgen.Turn, bool) {
	for _, t := range turns {
		if t.ID == id {
			return t, true
		}
	}
	return sqlcgen.Turn{}, false
}

// stringOrEmpty dereferences s, or returns "" if s is nil.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

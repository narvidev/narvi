package sessionactor

// This file implements the session actor's half of technical plan §6.4,
// §27.1 and §30.4's rule that a pull request's review session whose head
// is not in its base repository boots without the repository's hooks, so
// code the pull request brings never runs at boot.
//
// The decision is the claim's: reposource.ReviewSessionSkipsRepoHooks over
// the session's github_pr_sessions claim -- its head_in_base record -- and
// its spec. assembleSessionConfig writes it into SESSION_CONFIG's
// skipRepoHooks for each spawn and restore, from the claim as it is then; a
// mention that records the head in the base later reaches the next boot.
// The sandbox agent then runs none of setup.sh, sync.sh and start.sh, and
// starts none of the services .narvi/services.yml declares, in every boot
// mode.
//
// What a gen's agent can do is not what its boot did: a current agent
// advertises capabilities.skipRepoHooks on every ready
// (sandboxes.skip_repo_hooks_gen, migrations/000174) whatever its
// SESSION_CONFIG said. So each gen also records what it was told
// (sandboxes.skip_repo_hooks_told_gen, migrations/000175), written in the
// transaction that spawns or restores it and carried by a resume, and a
// turn of a session that skips the hooks is sent only to a gen that was
// told to skip them and advertised it honours that (applyRepoHooksGate,
// turn.DecideRepoHooksGate):
//
//   - a gen that was not told -- one the previous binary spawned, which
//     never writes the key -- ran the hooks: it is retired at once, its
//     snapshot cleared, and the next gen boots fresh, told;
//   - a told gen whose agent predates the key ran them too: with a
//     snapshot it is retired and the snapshot cleared; a fresh one has its
//     turn refused naming the remedy, rebuild the sandbox image;
//   - a told gen that has sent no ready yet is waited for: its ready
//     evaluates the turn again (handleSandboxEvent's dispatch after every
//     event), and the dispatch timer, armed RepoHooksReadyRecheckInterval
//     ahead, looks again if none comes.
//
// The gate only ever sees a gen after its boot, so a restore is decided
// before it (repoHooksRestoreRefusal, in tryPlanSpawn): each snapshot
// records whether the gen that minted it skipped the hooks
// (sandboxes.snapshot_skipped_repo_hooks_id, its own id when it did, with
// the rest of its provenance), and a restore crosses no boundary of the
// hooks
// (sandbox.DecideRepoHooksRestore). A session that skips them restores
// only a snapshot of an agent of protocol 1.28.0 or later whose gen
// skipped them -- an older or unknown agent would run them at the restored
// boot -- and a session that runs them never restores one whose gen
// skipped them, whose lineage never ran setup.sh. Either way the sandbox
// spawns fresh and the snapshot is cleared.
//
// The gate reads the claim as it is at dispatch. The decision only ever
// moves from skipping the hooks to running them -- a claim's record is
// written only from a read of the pull request's head, which never moves
// off its base, and the spec only ever gains a branch -- so a gen told to
// skip them is one whose session still skips them or no longer needs to;
// one whose session no longer skips them is sent its turn: its pull
// request's head is the base repository's own.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// readyAdvertisesSkipRepoHooks reports whether a ready event advertises
// capabilities.skipRepoHooks. A ready that fails its schema decode counts
// as not advertising it -- the safe direction, in which no turn of a
// session that boots without the repository's hooks is sent -- as
// readyAdvertisesReviewCheckout does.
func readyAdvertisesSkipRepoHooks(raw json.RawMessage) bool {
	evt, err := decodeReady(raw)
	if err != nil {
		return false
	}
	return evt.Capabilities != nil && evt.Capabilities.SkipRepoHooks != nil && *evt.Capabilities.SkipRepoHooks
}

// skipRepoHooksCapable reports whether row's live gen advertised, in its
// latest ready, that its agent honours SESSION_CONFIG's skipRepoHooks.
func skipRepoHooksCapable(row sqlcgen.Sandbox) bool {
	return row.SkipRepoHooksGen != nil && *row.SkipRepoHooksGen == row.Gen
}

// skipRepoHooksTold reports whether row's live gen's SESSION_CONFIG told it
// to boot without the repository's hooks.
func skipRepoHooksTold(row sqlcgen.Sandbox) bool {
	return row.SkipRepoHooksToldGen != nil && *row.SkipRepoHooksToldGen == row.Gen
}

// snapshotMintedWithoutRepoHooks reports whether row records snapshotID as
// minted by a gen that booted without the repository's hooks: the record
// names that snapshot (snapshot_skipped_repo_hooks_id). A record that names
// another snapshot is that snapshot's -- the previous binary mints without
// writing it -- so it says nothing of this one.
func snapshotMintedWithoutRepoHooks(row sqlcgen.Sandbox, snapshotID string) bool {
	return snapshotID != "" && row.SnapshotSkippedRepoHooksID != nil && *row.SnapshotSkippedRepoHooksID == snapshotID
}

// sessionSkipsRepoHooks reports whether a session whose pull request claim
// is claim -- nil for none -- and whose spec is sessionRepos boots without
// the repository's hooks (reposource.ReviewSessionSkipsRepoHooks).
func sessionSkipsRepoHooks(claim *sqlcgen.GithubPrSession, sessionRepos []byte) bool {
	if claim == nil {
		return false
	}
	return reposource.ReviewSessionSkipsRepoHooks(true, claim.RepoFullName, claim.HeadInBase, sessionRepos)
}

// applyRepoHooksGate decides, inside the dispatch evaluation's
// transaction, whether target -- the turn about to be sent to sandboxRow's
// live gen, pending, or processing and re-sent after a gen change -- may
// be sent there, as far as the repository's hooks go, and acts on
// turn.DecideRepoHooksGate's answer. See this file's top comment. Only the
// GitHub ingress claims a session, so any other session is sent as before
// without a read. A claim read that fails fails the evaluation, which is
// retried: a turn is never sent on a guess.
func (a *Actor) applyRepoHooksGate(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, turns []sqlcgen.Turn, target sqlcgen.Turn, now time.Time) (reviewCheckoutResult, error) {
	notNeeded := reviewCheckoutResult{decision: reviewCheckoutNotNeeded}
	if sessionRow.SpawnSource != sqlcgen.SessionSpawnSourceGithub || a.stores.githubPRSession == nil {
		return notNeeded, nil
	}
	claim, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notNeeded, nil
	}
	if err != nil {
		return reviewCheckoutResult{}, fmt.Errorf("sessionactor: repository hooks: read the session's pull request claim: %w", err)
	}

	action := turn.DecideRepoHooksGate(turn.RepoHooksGateFacts{
		SkipRepoHooks: sessionSkipsRepoHooks(&claim, sessionRow.Repos),
		Told:          skipRepoHooksTold(sandboxRow),
		AwaitingReady: genAwaitingReady(sandboxRow),
		GenCapable:    skipRepoHooksCapable(sandboxRow),
		HasSnapshot:   sandboxRow.SnapshotID != nil && *sandboxRow.SnapshotID != "",
	})
	switch action {
	case turn.RepoHooksProceed:
		return notNeeded, nil

	case turn.RepoHooksWait:
		if err := a.stores.timer.WithTx(tx).ArmDispatchAfter(ctx, a.sessionID, a.timeouts.RepoHooksReadyRecheckInterval); err != nil {
			return reviewCheckoutResult{}, fmt.Errorf("sessionactor: repository hooks: arm the dispatch timer: %w", err)
		}
		a.logger.Debug("sessionactor: repository hooks: the turn waits for its sandbox gen's ready",
			"turn_id", target.ID.String(), "gen", sandboxRow.Gen)
		return reviewCheckoutResult{decision: reviewCheckoutHold}, nil

	case turn.RepoHooksRetireNotTold:
		retired, err := a.retireGenClearingSnapshot(ctx, tx, sandboxRow, repoHooksNotToldSnapshotWarning)
		if err != nil {
			return reviewCheckoutResult{}, err
		}
		if err := a.forgetRetiredGensSnapshot(ctx, tx, sandboxRow); err != nil {
			return reviewCheckoutResult{}, err
		}
		if sandboxRow.SnapshotID == nil || *sandboxRow.SnapshotID == "" {
			if err := a.recordSessionWarning(ctx, tx, int(sandboxRow.Gen), repoHooksNotToldWarning); err != nil {
				return reviewCheckoutResult{}, err
			}
		}
		if err := a.armDispatchNow(ctx, tx); err != nil {
			return reviewCheckoutResult{}, err
		}
		a.logger.Warn("sessionactor: repository hooks: the sandbox gen is retired and its snapshot cleared, so the next gen boots fresh: it was not told to boot without the repository's hooks",
			"turn_id", target.ID.String(), "gen", sandboxRow.Gen, "provider_id", retired.providerID)
		return reviewCheckoutResult{
			decision: reviewCheckoutHold,
			plan:     &dispatchPlan{turnID: target.ID, sessionRow: sessionRow, checkoutRetire: retired},
		}, nil

	case turn.RepoHooksRetireGen:
		retired, err := a.retireGenClearingSnapshot(ctx, tx, sandboxRow, repoHooksRetirementWarning)
		if err != nil {
			return reviewCheckoutResult{}, err
		}
		if err := a.forgetRetiredGensSnapshot(ctx, tx, sandboxRow); err != nil {
			return reviewCheckoutResult{}, err
		}
		if err := a.armDispatchNow(ctx, tx); err != nil {
			return reviewCheckoutResult{}, err
		}
		a.logger.Warn("sessionactor: repository hooks: the sandbox gen is retired and its snapshot cleared, so the next gen boots fresh: its agent does not boot without the repository's hooks",
			"turn_id", target.ID.String(), "gen", sandboxRow.Gen, "provider_id", retired.providerID)
		return reviewCheckoutResult{
			decision: reviewCheckoutHold,
			plan:     &dispatchPlan{turnID: target.ID, sessionRow: sessionRow, checkoutRetire: retired},
		}, nil

	case turn.RepoHooksRefuse:
		refusal := repoHooksRefusal()
		if turn.State(target.Status) != turn.StatePending {
			a.logger.Warn("sessionactor: repository hooks: a turn re-sent to a new gen is failed: its agent does not boot without the repository's hooks",
				"turn_id", target.ID.String(), "gen", sandboxRow.Gen)
			return reviewCheckoutResult{
				decision: reviewCheckoutFailForward,
				plan: &dispatchPlan{turnID: target.ID, sessionRow: sessionRow, checkoutFailure: &dispatchFailure{
					reason: refusal.reason, warning: refusal.warning, notAssessed: refusal.notAssessed, refused: true,
				}},
			}, nil
		}
		if err := a.refusePendingTurn(ctx, tx, sessionRow, turns, target, refusal, int(sandboxRow.Gen), now); err != nil {
			return reviewCheckoutResult{}, err
		}
		a.logger.Warn("sessionactor: repository hooks: the turn is refused: its sandbox's agent does not boot without the repository's hooks",
			"turn_id", target.ID.String(), "gen", sandboxRow.Gen)
		return reviewCheckoutResult{decision: reviewCheckoutEnded}, nil

	default:
		return reviewCheckoutResult{}, fmt.Errorf("sessionactor: repository hooks: unhandled gate action %s", action)
	}
}

// forgetRetiredGensSnapshot forgets, in tx, the snapshot sandboxRow's gen
// -- which the gate has just retired -- had in progress, if any: the
// snapshot after a turn, or a rotation's. The gate retires such a gen at
// once rather than waiting on that snapshot: what it would record is
// discarded with the gen anyway (its gen booted with the hooks, or its
// agent predates the key), and a snapshot that never reports would hold
// the session for good, since the runway gate's bound on it
// (SnapshotReportWait) runs after this gate. Its report, should it come
// late, then matches nothing (handleSnapshotReadyEvent), as with the
// runway gate's own retirement.
func (a *Actor) forgetRetiredGensSnapshot(ctx context.Context, tx pgx.Tx, sandboxRow sqlcgen.Sandbox) error {
	if sandbox.State(sandboxRow.Status) != sandbox.StateSnapshotting && sandboxRow.PendingSnapshotMessageID == nil {
		return nil
	}
	if _, err := a.stores.sandbox.WithTx(tx).UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID: a.sessionID, PendingSnapshotMessageID: nil,
	}); err != nil {
		return fmt.Errorf("sessionactor: repository hooks: forget the retired gen's snapshot in progress: %w", err)
	}
	return nil
}

// repoHooksRefusal is the texts of a turn the gate refuses.
func repoHooksRefusal() reviewCheckoutRefusalText {
	return reviewCheckoutRefusalText{
		reason: "the sandbox's agent cannot boot without the repository's hooks, which a review of a pull request whose head is not in its base repository requires",
		warning: "This turn was not run: its sandbox's agent cannot boot without the repository's hooks, which a review of a pull request " +
			"whose head is not in its base repository requires. Rebuild the sandbox image so it carries a current agent, then ask again.",
		notAssessed: reviewcheck.NotAssessedRepoHooksUnsupported,
	}
}

// repoHooksNotToldWarning is the session warning of a gen the gate retires
// because it was not told to boot without the repository's hooks, when
// the sandbox has no snapshot to discard with it.
const repoHooksNotToldWarning = "This session's sandbox was stopped: it booted with the repository's hooks, which a review of a pull request " +
	"whose head is not in its base repository does not run. A fresh sandbox is starting from the repository. Work that was not pushed, " +
	"and the agent's own conversation, are not in the new sandbox."

// repoHooksNotToldSnapshotWarning is the session warning of a snapshot,
// snapshotID, the gate discards with a gen that was not told to boot
// without the repository's hooks.
func repoHooksNotToldSnapshotWarning(snapshotID string) string {
	return "This session's sandbox was stopped and its last snapshot, " + snapshotID + ", discarded: that sandbox booted with the " +
		"repository's hooks, which a review of a pull request whose head is not in its base repository does not run. A fresh sandbox " +
		"is starting from the repository. Work that was not pushed, and the agent's own conversation, are not in the new sandbox."
}

// repoHooksRestoreRefusal decides, in tryPlanSpawn's transaction, whether
// the restore of sandboxRow's snapshot, snapshotID, crosses a boundary of
// the repository's hooks (sandbox.DecideRepoHooksRestore, this file's top
// comment), and returns the refusal that turns it into a fresh spawn and
// clears the snapshot, nil when it does not. The session skips the hooks
// as assembleSessionConfig will decide it, a claim that cannot be read
// included.
func (a *Actor) repoHooksRestoreRefusal(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, snapshotID string) *snapshotRefusal {
	claim, claimReadFailed := a.reviewClaim(ctx, tx, sessionRow)
	skips := claimReadFailed || sessionSkipsRepoHooks(claim, sessionRow.Repos)
	provenance := snapshotProvenance(sandboxRow)
	mintedWithoutHooks := snapshotMintedWithoutRepoHooks(sandboxRow, snapshotID)
	reason := sandbox.DecideRepoHooksRestore(skips, provenance, mintedWithoutHooks)
	if reason == sandbox.RepoHooksRestoreAllowed {
		return nil
	}
	a.logger.Warn("sessionactor: refusing a snapshot restore across a boundary of the repository's hooks; spawning fresh",
		"session_id", a.sessionID.String(), "snapshot_id", snapshotID, "session_skips_repo_hooks", skips,
		"snapshot_agent_protocol", provenance.AgentProtocol, "snapshot_provenance_recorded", provenance.Recorded,
		"snapshot_skipped_repo_hooks", mintedWithoutHooks, "reason", string(reason))
	return &snapshotRefusal{
		snapshotID: snapshotID,
		warning:    repoHooksRestoreRefusalWarning(snapshotID, snapshotMintedAt(sandboxRow), provenance, reason),
		clear:      true,
	}
}

// repoHooksRestoreRefusalWarning is the session warning of a restore
// repoHooksRestoreRefusal turns into a fresh spawn.
func repoHooksRestoreRefusalWarning(snapshotID string, mintedAt pgtype.Timestamptz, p sandbox.SnapshotProvenance, reason sandbox.RepoHooksRestoreReason) string {
	var why string
	switch reason {
	case sandbox.RepoHooksRestoreMintedWithoutHooks:
		why = "this session now runs the repository's hooks, and that snapshot was taken by a sandbox that booted without them, so a fresh sandbox runs them from the start."
	case sandbox.RepoHooksRestoreAgentTooOld:
		why = "this session boots without the repository's hooks, and the sandbox agent that snapshot holds, built for protocol " + p.AgentProtocol + ", does not."
	case sandbox.RepoHooksRestoreMintedWithHooks:
		why = "this session boots without the repository's hooks, and that snapshot was taken by a sandbox that booted with them."
	default:
		why = "this session boots without the repository's hooks, and nothing recorded shows that the sandbox agent that snapshot holds does."
	}
	return freshInsteadOfSnapshot(snapshotID, mintedAt) + why + " " + snapshotRefusalLost
}

// repoHooksRetirementWarning is the session warning of a snapshot,
// snapshotID, the gate discards with a gen whose agent does not boot
// without the repository's hooks.
func repoHooksRetirementWarning(snapshotID string) string {
	return "This session's sandbox was stopped and its last snapshot, " + snapshotID + ", discarded: the sandbox's agent cannot boot " +
		"without the repository's hooks, which a review of a pull request whose head is not in its base repository requires, and a " +
		"restore of the snapshot may bring that agent back. A fresh sandbox is starting from the repository. Work that was not pushed, " +
		"and the agent's own conversation, are not in the new sandbox."
}

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
// An agent that predates the key ignores it and runs the hooks, and says
// so by not advertising capabilities.skipRepoHooks on its ready
// (sandboxes.skip_repo_hooks_gen, migrations/000173). So no turn of such a
// session is sent to a gen whose latest ready did not advertise it
// (applyRepoHooksGate, turn.DecideRepoHooksGate): a gen with a snapshot
// that could bring that agent back is retired once and the snapshot
// cleared, so the next gen boots fresh; a fresh gen that lacks it too
// takes no snapshot, since no turn runs on it, and its turn is refused
// naming the remedy -- rebuild the sandbox image. A gen that has sent no
// ready yet is waited for: its ready evaluates the turn again
// (handleSandboxEvent's dispatch after every event), and the dispatch
// timer, armed RepoHooksReadyRecheckInterval ahead, looks again if none
// comes.
//
// The gate reads the claim as it is at dispatch, not as it was at the
// gen's boot. The two agree on every gen the gate holds: a claim's record
// is written only from a read of the pull request's head, which never
// moves off its base, and the spec only ever gains a branch, so a session
// that skips the hooks at dispatch skipped them at its gen's boot. One
// that skipped them at boot and no longer does -- a mention recorded the
// head in the base since -- is sent to any gen: its pull request's head
// is the base repository's own.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
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

	case turn.RepoHooksRetireGen:
		retired, err := a.retireGenClearingSnapshot(ctx, tx, sandboxRow, repoHooksRetirementWarning)
		if err != nil {
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

// repoHooksRefusal is the texts of a turn the gate refuses.
func repoHooksRefusal() reviewCheckoutRefusalText {
	return reviewCheckoutRefusalText{
		reason: "the sandbox's agent cannot boot without the repository's hooks, which a review of a pull request whose head is not in its base repository requires",
		warning: "This turn was not run: its sandbox's agent cannot boot without the repository's hooks, which a review of a pull request " +
			"whose head is not in its base repository requires. Rebuild the sandbox image so it carries a current agent, then ask again.",
		notAssessed: reviewcheck.NotAssessedRepoHooksUnsupported,
	}
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

// This file (repoentitlement.go) implements the session actor's half of an
// administrator's revocation of a repository (technical plan §31.4,
// "Un-entitlement"). Session creation refuses a revoked repository
// (httpapi's ResolveRepoEntitlement), but a session created before the
// revocation keeps producing turns -- a person's prompt, a plan approval, a
// manual or automatic re-review, a workflow step, a Slack or Linear
// follow-up -- and none of those producers reads eligibility. So the
// revocation is read again where every turn has to pass before it reaches a
// sandbox, the two placements §32.4's rollout re-checks already use:
//
//   - refuseIfRepoRevoked, before a spawn, restore or resume (tryPlanSpawn),
//     inside the evaluation's transaction. A revoked repository ends the
//     session's open turns forward (endTurnsOnSpawnRefusal); a read that
//     failed rolls the evaluation back and its dispatch timer backs off.
//   - revocationRefusalForDispatch, before a turn's prompt is sent to a live
//     sandbox (executeDispatch), on the pool. A revoked repository fails the
//     turn as a policy refusal; a read that failed fails it as an
//     undelivered prompt and backs the dispatch timer off -- never as a
//     refusal, so a database blip never escalates a workflow run.
//
// Unlike the rollout re-checks these run in every rollout mode: a
// revocation binds every deployment. What runs is left alone: a turn
// already processing finishes (push, pull request and verdict included),
// and a live sandbox with nothing more to do idles out.
//
// A session names its repositories two ways, and both are read
// (postgres.GitHubPRSessionStore.RevokedRepoForSession): the clone URLs in
// sessions.repos, and the pull-request claims keyed to the session -- a
// review session's github_pr_sessions row and a sentinel auto-fix child's
// sentinel_fixes row. The claims name the pull request's base repository,
// which a fork pull request's clone URL does not.

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
)

// repoRevokedReason is the reason text of a turn ended because an
// administrator revoked repo: the synthetic execution_complete's "reason",
// at spawn and at dispatch alike.
func repoRevokedReason(repo string) string {
	return fmt.Sprintf("repo %q entitlement revoked by an administrator", repo)
}

// repoEntitlementUnverifiedReason is the reason text of a turn failed
// because the revocation read before its dispatch failed: an undelivered
// prompt, not a refusal.
const repoEntitlementUnverifiedReason = "repository entitlement could not be verified before dispatch"

// revokedRepoForSession reports the first of sessionRow's repositories an
// administrator revoked, read through store (the pool, or an evaluation's
// transaction). The clone URLs in sessions.repos are resolved the way the
// rollout re-check resolves them (reposource.CheckRepoHost, then
// ParseOwnerRepo); a URL that resolves to no trusted owner/repo can match
// no revocation and is skipped. A malformed repos column contributes no
// names, and the session's pull-request claims are still read.
func (a *Actor) revokedRepoForSession(ctx context.Context, store *postgres.GitHubPRSessionStore, sessionRow sqlcgen.Session) (repo string, revoked bool, err error) {
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(sessionRow.Repos, &repos); err != nil {
		a.logger.Warn("sessionactor: entitlement re-check: unmarshal session repos failed; reading the session's pull-request claims only",
			"session_id", a.sessionID.String(), "error", err)
		repos = nil
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		if err := reposource.CheckRepoHost(r.URL, ports.SupportedSourceControlHosts()...); err != nil {
			continue
		}
		owner, name, err := reposource.ParseOwnerRepo(r.URL)
		if err != nil {
			continue
		}
		names = append(names, owner+"/"+name)
	}
	return store.RevokedRepoForSession(ctx, a.sessionID, names)
}

// refuseIfRepoRevoked is the spawn-time re-read of an administrator's
// revocation (§31.4): run first in tryPlanSpawn's Spawn/Restore/Resume
// block, on the evaluation's own transaction, before
// refuseIfSubstrateUnsupported. A revoked repository returns a
// spawnRefusal, which endTurnsOnSpawnRefusal settles -- every open turn
// ended forward with the reason, the banner recorded once, a review
// attempt's check closed as not assessed naming the revocation -- and is
// counted once on session_repo_entitlement_denied_total. A read that
// failed is not a fact about the repository: it is returned as a plain
// error, the transaction rolls back, and handleEnsureDispatched backs the
// dispatch timer off.
func (a *Actor) refuseIfRepoRevoked(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) error {
	repo, revoked, err := a.revokedRepoForSession(ctx, a.stores.githubPRSession.WithTx(tx), sessionRow)
	if err != nil {
		return fmt.Errorf("sessionactor: spawn-time entitlement re-check: read revocations: %w", err)
	}
	if !revoked {
		return nil
	}
	a.logger.Error("sessionactor: refusing to spawn: an administrator revoked the session's repository (§31.4 spawn-time re-check)",
		"session_id", a.sessionID.String(), "repo", repo)
	a.recordRepoEntitlementRevoked(ctx, string(sessionRow.SpawnSource))
	return repoRevokedSpawnRefusal(repo)
}

// repoRevokedSpawnRefusal is the spawnRefusal for repo, one an
// administrator revoked.
func repoRevokedSpawnRefusal(repo string) *spawnRefusal {
	return &spawnRefusal{
		reason: repoRevokedReason(repo),
		warning: fmt.Sprintf("This session's open turns were ended: an administrator of this deployment revoked new work on its repository %q, so no sandbox could be started. "+
			"An administrator can restore the repository; then send the turn again.", repo),
		notAssessed: reviewcheck.NotAssessedRepoEntitlementRevoked,
	}
}

// revocationRefusalForDispatch is the turn-dispatch-time re-read of an
// administrator's revocation (§31.4): the first thing executeDispatch does,
// on the pool, after the turn was committed processing and before its
// prompt is sent. It returns the dispatchFailure executeDispatch fails the
// turn with, and whether to fail it:
//
//   - a revoked repository: a policy refusal (refused), with the banner and
//     a review attempt's check closed as not assessed naming the
//     revocation, counted once on session_repo_entitlement_denied_total;
//   - a read that failed: an undelivered prompt, never a refusal -- the
//     prompt_not_delivered reason and the dispatch timer backed off from
//     chainStart, as a send that failed would be, so a database blip
//     neither escalates a workflow run nor reports a revocation that did
//     not happen;
//   - otherwise nothing, and the prompt is sent.
func (a *Actor) revocationRefusalForDispatch(ctx context.Context, sessionRow sqlcgen.Session, chainStart pgtype.Timestamptz) (dispatchFailure, bool) {
	repo, revoked, err := a.revokedRepoForSession(ctx, a.stores.githubPRSession, sessionRow)
	if err != nil {
		a.logger.Error("sessionactor: dispatch turn: entitlement re-check could not read revocations; failing turn as undelivered",
			"session_id", a.sessionID.String(), "error", err)
		return dispatchFailure{
			reason:       repoEntitlementUnverifiedReason,
			notAssessed:  reviewcheck.NotAssessedPromptNotDelivered,
			backOffSince: chainStart,
		}, true
	}
	if !revoked {
		return dispatchFailure{}, false
	}
	a.logger.Error("sessionactor: refusing to dispatch turn: an administrator revoked the session's repository (§31.4 turn-dispatch-time re-check)",
		"session_id", a.sessionID.String(), "repo", repo)
	a.recordRepoEntitlementRevoked(ctx, string(sessionRow.SpawnSource))
	return dispatchFailure{
		reason: repoRevokedReason(repo),
		warning: fmt.Sprintf("This session's turn was ended: an administrator of this deployment revoked new work on its repository %q, so the turn was not sent to its sandbox. "+
			"An administrator can restore the repository; then send the turn again.", repo),
		notAssessed: reviewcheck.NotAssessedRepoEntitlementRevoked,
		refused:     true,
	}, true
}

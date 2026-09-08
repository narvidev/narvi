// This file (handoffsentinel.go) implements §14.4's own ("handoff-
// readiness sentinel", §14.4/§14.5) app-layer orchestration: given a PR
// createPRBestEffort (pushpr.go) just successfully created, decide whether
// this session's provenance_tag marks it as coming from a scoped
// Environment (§14.1) and, if so, whether there is anything worth telling
// an engineer about -- reusing §14.3's already-merged contract-drift
// signal (internal/domain/contractdrift.HasDrifted, never a second
// endpoint scanner) and this Step's own new backend-TODO diff scan
// (internal/domain/handoff.ScanTODOs).
//
// # Where this runs, and why
//
// Called from createPRBestEffort's own per-repo loop, immediately after
// recordPRArtifact succeeds for that repo -- the exact moment this session
// already has everything needed in hand (sessionRow.ProvenanceTag, the
// creator's already-decrypted OAuth token, owner/repoName, and the just-
// created PR's own number) with NO further plumbing. This deliberately
// does NOT run via a GitHub `pull_request` webhook lane (the way §8.2's
// merge-gating half does, internal/adapters/inbound/github/
// pullrequestevent.go): a scoped session creates its OWN PR (pushpr.go),
// it is never a human pushing a branch and opening one by hand, so the
// one moment "this PR was just created by a scoped session" is knowable
// with zero re-derivation is right here, inside the session that did it --
// exactly what §14.1's own provenance-tag design says: "so the label
// automation and the handoff sentinel can act on it without re-deriving
// intent."
//
// # Why NOT the verdict-posting path (§8.2)
//
// This sentinel never calls POST .../review/verdict (reviewverdict.go),
// never builds a review.Verdict, and never writes review_findings.
// §14.4's own text is explicit that this sentinel runs "alongside or
// INSTEAD OF a normal risk verdict" -- a scoped-session PR is not
// necessarily (and today, is never) also a review session (§8.2's
// mention-triggered claim is a completely separate, independent trigger).
// Piping handoff findings through the verdict-posting tool would also
// require a SentinelKind value for them (reviewpost.Finding's own
// two-value coverage/docs_drift vocabulary), which internal/domain/
// handoff/doc.go's own design call #1 explains is exactly the wrong move:
// it would silently make a handoff finding eligible for the UNRELATED
// sentinel-auto-fix child-session flow. This sentinel instead builds its
// own small, typed HandoffPayload and posts it through a dedicated outbox
// kind (ports.NotificationKindHandoffSentinel) -- reusing reviewpost.
// Finding's SHAPE and IDENTITY-HASH algorithm (per the plan's own
// instruction), never its POSTING pipeline.
//
// # Idempotency
//
// Claimed via handoffSentinelRuns.Claim, inside the SAME transaction as
// the outbox enqueue (a.transact, mirroring reviewverdict.go's own
// sentinelFix-claim-then-outbox-enqueue precedent) -- but only once this
// function already knows there is something to report. A clean PR (no
// contract drift, no TODOs) posts nothing and claims nothing: "post
// nothing" is already idempotent on its own (nothing to duplicate), so
// the claim table's only job is preventing the label/comment from being
// posted TWICE for a PR that already got one.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/contractdrift"
	"github.com/narvidev/narvi/internal/domain/handoff"
	"github.com/narvidev/narvi/internal/domain/provenance"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// PRDiffFetcher is the narrow slice of *githubapi.Adapter's own real,
// authenticated GitHub REST API surface runHandoffSentinelBestEffort
// needs -- a small, locally-defined interface (mirroring internal/app/
// reviewcontext.Fetcher's/internal/adapters/inbound/github's own
// PullRequestResolver precedent) so a unit test can inject a fake with no
// real HTTP round trip. *githubapi.Adapter satisfies this directly, with
// no adapter-side change.
type PRDiffFetcher interface {
	GetPullRequestDiff(ctx context.Context, owner, repo string, number int32, token string) (diff string, truncated bool, err error)
}

// runHandoffSentinelBestEffort implements this file's own top comment.
// token is the session creator's own already-decrypted GitHub OAuth
// token (createPRBestEffort's own token, reused here rather than
// decrypted a second time); owner/repoName describe the repo this PR was
// just opened for; configuredBranch is that repo's own CONFIGURED base
// branch (nil means "the repo's real default branch") -- the SAME value
// §14.3's checkContractDriftForRepo reads, and deliberately NEVER the
// session's own pushed branch (see checkHandoffContractDrift's own doc
// comment for why that distinction is load-bearing). prNumber is the
// just-created PR's own number. Every early return here is a plain,
// logged no-op -- this function never blocks or fails PR creation,
// mirroring checkContractDrift's/createPRBestEffort's own identical
// "never block a spawn/push" discipline.
func (a *Actor) runHandoffSentinelBestEffort(
	ctx context.Context,
	sessionRow sqlcgen.Session,
	token, owner, repoName string,
	configuredBranch *string,
	prNumber int,
) {
	if !provenance.IsScopedEnvironment(sessionRow.ProvenanceTag) {
		// An ordinary PR -- completely untouched: no environment lookup, no
		// contract-drift check, no diff fetch, no store/outbox writes.
		return
	}

	repoFullName := owner + "/" + repoName

	contractDrifted := a.checkHandoffContractDrift(ctx, sessionRow, owner, repoName, configuredBranch, token)
	todos := a.fetchHandoffTODOs(ctx, owner, repoName, prNumber, token)

	if !contractDrifted && len(todos) == 0 {
		// Nothing to report -- silence is correct, never an empty comment
		// or a label with no substance behind it.
		return
	}

	inputs := handoff.BuildFindingInputs(repoFullName, contractDrifted, todos)
	findings := make([]reviewpost.Finding, 0, len(inputs))
	for _, in := range inputs {
		if err := reviewpost.ValidateFindingInput(in); err != nil {
			// Should be unreachable (every input this package builds is
			// already well-formed by construction) -- logged rather than
			// silently dropped, but never fatal to the rest of this run.
			a.logger.Error("sessionactor: handoff sentinel: built an invalid finding input (bug); skipping this finding", "error", err)
			continue
		}
		findings = append(findings, reviewpost.BuildFinding(in))
	}
	if len(findings) == 0 {
		return
	}

	body := handoff.RenderComment(findings)

	payload, err := json.Marshal(githubapi.HandoffPayload{
		Owner:    owner,
		Repo:     repoName,
		PRNumber: prNumber,
		Body:     body,
		Label:    handoff.Label,
	})
	if err != nil {
		a.logger.Error("sessionactor: handoff sentinel: marshal outbox payload failed", "error", err)
		return
	}

	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}

	if err := a.enqueueHandoffNotification(ctx, repoFullName, int32(prNumber), payload, correlationID, contractDrifted, len(todos)); err != nil {
		a.logger.Error("sessionactor: handoff sentinel: claim/enqueue failed", "error", err)
	}
}

// checkHandoffContractDrift reuses §14.3's own contract-drift machinery
// verbatim (internal/domain/contractdrift.HasDrifted, the SAME
// contractDrift store checkContractDrift/contractdrift.go already reads)
// -- never a second endpoint scanner (internal/domain/handoff/doc.go's
// own design call #2 names why no finer-grained signal exists to reuse).
// Only ever produces true for a MockConfigured Environment (the only kind
// §14.3 ever persists a baseline snapshot for) -- a scoped-but-not-
// mock-configured Environment's PR simply never has anything to compare
// against, and this returns false, exactly like a "first sighting" does
// in checkContractDriftForRepo.
//
// Fixed confirmed finding: this function used to reuse the session's OWN
// pushed branch/sha directly (from the push_complete event that led to
// this PR being created), reasoning that doing so "saves one GitHub API
// round trip." That reasoning was wrong and the reuse was a real bug with
// two distinct failure modes depending on how the Environment's repo is
// configured:
//   - repos[].branch left nil (the common case -- session branches are
//     auto-generated as narvi/<sessionID>): checkContractDriftForRepo
//     writes its snapshot keyed on the repo's real DEFAULT branch, but
//     this function was reading under the session's own generated branch
//     -- a DIFFERENT key entirely, so the read was always pgx.ErrNoRows
//     and contractDrifted was always silently false, regardless of any
//     real drift.
//   - repos[].branch explicitly set to a fixed name: both sides resolved
//     to the SAME key, but then previous/current were BOTH derived from
//     this session's own commits (its pre-push snapshot vs. its own
//     post-push state) -- HasDrifted fired on virtually every commit the
//     session itself made, since the SHA always differs and the
//     contracts fingerprint never can (a scoped session's sparse
//     checkout cannot touch contracts/api/* at all).
//
// Both share one root cause: current must be computed from the repo's own
// CONFIGURED/base branch, independent of this session's own commits --
// the real question this signal answers is "has this repo's backend
// drifted from its contract since it was last checked", never "did THIS
// session's own push change anything". The fix: re-resolve the
// configured branch fresh, via the SAME a.sourceControl.ResolveBranchSHA
// call checkContractDriftForRepo itself makes, and build BOTH the
// comparison and the repoKey from ITS results -- provably the same key
// that writer used, byte for byte. This costs one real GitHub API round
// trip per scoped-session PR (paid only for a scoped + mock-configured
// Environment, a narrow case) -- correctness here is worth that cost.
//
// This still NEVER upserts a new snapshot back (a read-only comparison)
// -- checkContractDrift (spawn/restore time) remains the ONE writer of
// contract_drift_snapshots; this function only ever reads what that
// writer already recorded.
func (a *Actor) checkHandoffContractDrift(ctx context.Context, sessionRow sqlcgen.Session, owner, repoName string, configuredBranch *string, token string) bool {
	if !sessionRow.EnvironmentID.Valid {
		return false
	}

	env, err := a.stores.environment.Get(ctx, sessionRow.EnvironmentID)
	if err != nil {
		a.logger.Warn("sessionactor: handoff sentinel: get environment failed; skipping contract-drift check", "error", err)
		return false
	}
	if !env.MockConfigured {
		return false
	}
	if a.sourceControl == nil {
		a.logger.Warn("sessionactor: handoff sentinel: no SourceControl configured; skipping contract-drift check")
		return false
	}

	branch := ""
	if configuredBranch != nil {
		branch = *configuredBranch
	}

	shaCtx, shaCancel := context.WithTimeout(ctx, a.timeouts.ContractsFingerprintResolutionTimeout)
	sha, resolvedBranch, err := a.sourceControl.ResolveBranchSHA(shaCtx, ports.ResolveBranchSHASpec{
		Owner: owner, Repo: repoName, Branch: branch, Token: token,
	})
	shaCancel()
	if err != nil {
		a.logger.Warn("sessionactor: handoff sentinel: resolve branch sha failed; skipping contract-drift check", "error", err)
		return false
	}

	contractsPath := ""
	if env.ContractsPath != nil {
		contractsPath = *env.ContractsPath
	}

	fpCtx, cancel := context.WithTimeout(ctx, a.timeouts.ContractsFingerprintResolutionTimeout)
	fingerprint, exists, err := a.sourceControl.ResolveContractsFingerprint(fpCtx, ports.ResolveContractsFingerprintSpec{
		Owner: owner, Repo: repoName, Ref: sha, Path: contractsPath, Token: token,
	})
	cancel()
	if err != nil {
		a.logger.Warn("sessionactor: handoff sentinel: resolve contracts fingerprint failed; skipping contract-drift check", "error", err)
		return false
	}
	if !exists {
		fingerprint = ""
	}

	// Mirrors contractdrift.go's own repoKey construction EXACTLY (owner +
	// "/" + repoName + "@" + resolvedBranch) so this read hits the SAME
	// row checkContractDrift last wrote at this session's own
	// spawn/restore -- resolvedBranch (ResolveBranchSHA's own second
	// return), NEVER the raw configuredBranch/branch string, for the
	// exact reason checkContractDriftForRepo's own doc comment gives: a
	// nil-branch config resolves to the repo's real default branch name,
	// and the key must match on THAT name, not on an empty string.
	repoKey := owner + "/" + repoName + "@" + resolvedBranch

	row, err := a.stores.contractDrift.Get(ctx, repoKey)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.logger.Warn("sessionactor: handoff sentinel: get contract drift snapshot failed; skipping contract-drift check", "error", err)
		}
		// pgx.ErrNoRows means "first sighting -- nothing recorded yet",
		// exactly mirroring checkContractDriftForRepo's own identical
		// case: nothing to compare against, so no drift.
		return false
	}

	previous := contractdrift.Snapshot{RepoSHA: row.LastRepoSha, ContractsFingerprint: row.LastContractsFingerprint}
	current := contractdrift.Snapshot{RepoSHA: sha, ContractsFingerprint: fingerprint}
	return contractdrift.HasDrifted(previous, current)
}

// fetchHandoffTODOs fetches this PR's own diff (best-effort -- a fetch
// failure or a nil diffFetcher degrades to "no diff available", scanning
// nothing) and hands it to internal/domain/handoff.ScanTODOs.
func (a *Actor) fetchHandoffTODOs(ctx context.Context, owner, repoName string, prNumber int, token string) []handoff.TODOFinding {
	if a.diffFetcher == nil {
		return nil
	}

	diffCtx, cancel := context.WithTimeout(ctx, a.timeouts.GitHubPRDiffTimeout)
	diff, _, err := a.diffFetcher.GetPullRequestDiff(diffCtx, owner, repoName, int32(prNumber), token)
	cancel()
	if err != nil {
		a.logger.Warn("sessionactor: handoff sentinel: fetch pull request diff failed; skipping TODO scan", "error", err)
		return nil
	}

	return handoff.ScanTODOs(diff)
}

// enqueueHandoffNotification claims (repoFullName, prNumber) in
// handoff_sentinel_runs and, only if this call wins that claim, enqueues
// exactly one ports.NotificationKindHandoffSentinel outbox row -- both in
// the SAME transaction (a.transact), mirroring reviewverdict.go's own
// sentinelFix-claim-then-outbox-enqueue precedent exactly. A losing claim
// (another run already posted for this PR) is NOT an error -- it is the
// idempotent no-op this function exists to guarantee.
//
// contractDriftFlagged/todoCount (§12.2 item 2's own "handoff-readiness
// display" gap) are the caller's own already-computed values, forwarded
// verbatim into the claim.
func (a *Actor) enqueueHandoffNotification(ctx context.Context, repoFullName string, prNumber int32, payload []byte, correlationID *string, contractDriftFlagged bool, todoCount int) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		claimed, err := a.stores.handoffSentinelRuns.WithTx(tx).Claim(ctx, repoFullName, prNumber, a.sessionID, contractDriftFlagged, int32(todoCount))
		if err != nil {
			return fmt.Errorf("sessionactor: handoff sentinel: claim run failed: %w", err)
		}
		if !claimed {
			return nil
		}

		if _, err := a.stores.outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
			SessionID:     a.sessionID,
			Kind:          string(ports.NotificationKindHandoffSentinel),
			Payload:       payload,
			CorrelationID: correlationID,
		}); err != nil {
			return fmt.Errorf("sessionactor: handoff sentinel: enqueue outbox entry failed: %w", err)
		}
		return nil
	})
}

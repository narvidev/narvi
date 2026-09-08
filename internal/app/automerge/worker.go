// Package automerge implements §21.2 stage 2's own auto-merge worker --
// the ONLY thing an armed repo_settings.auto_merge_enabled toggle
// actually changes (§21.2: "Auto-approval alone does not merge
// anything"). Runs as a background pump, mirroring internal/app/
// automation.Engine's own identical periodic-tick shape, discovering
// candidate PRs from review_verdicts' own bounded history (a cheap,
// local Postgres read -- never a GitHub call for DISCOVERY) and then
// re-confirming each one LIVE via internal/app/decisioninbox.
// RevalidateForAutoMerge -- the SAME server-side re-validation-at-click
// contract the human-clicked Merge endpoint already uses (§21.2: "a
// deliberate reuse, not a parallel merge path"), just machine-initiated
// with the deployment's own bot token instead of any human's.
package automerge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	domainautomerge "github.com/narvidev/narvi/internal/domain/automerge"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// maxCandidatesPerRepoPerTick bounds how many auto-approved candidate PRs
// ONE repo contributes to ONE tick -- §21.1's own "bounded from day one"
// discipline, applied here to a per-repo, per-tick cap so one repo with
// an unusually large recent-verdict history can never starve every other
// armed repo's own tick out of the SAME pass.
const maxCandidatesPerRepoPerTick = 20

// Deps bundles every dependency Worker needs -- constructed once at
// process wiring time (cmd/control-plane/main.go), mirroring every other
// app-layer Deps struct in this codebase.
type Deps struct {
	DecisionInbox decisioninbox.Deps
	SourceControl ports.SourceControl
	AuditLog      *postgres.AuditLogStore

	BotToken string
	Timeouts platform.Timeouts
}

// Worker runs the auto-merge pump.
type Worker struct {
	deps      Deps
	authGuard *authGuard
}

// New builds a Worker. authGuard (authguard.go, docs/TECHNICAL_PLAN.md
// §17's own automerge dead-letter fix) is constructed here, once per
// Worker, from deps.Timeouts.AutoMergeAuthBackoffBase/
// AutoMergeAuthBackoffMax -- mirroring every other backoff-config-holding
// construction in this codebase (e.g. internal/app/outboxworker.Builder,
// which reads OutboxBackoffBase/OutboxBackoffMax the same way).
func New(deps Deps) *Worker {
	return &Worker{
		deps: deps,
		authGuard: newAuthGuard(domainautomerge.BackoffConfig{
			BaseDelay: deps.Timeouts.AutoMergeAuthBackoffBase,
			MaxDelay:  deps.Timeouts.AutoMergeAuthBackoffMax,
		}),
	}
}

// Run ticks every deps.Timeouts.AutoMergePumpInterval until ctx is
// cancelled -- mirrors internal/app/automation.Engine.Run's own identical
// ticker-loop shape (errgroup + context, no naked goroutine, CLAUDE.md/
// §11).
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.deps.Timeouts.AutoMergePumpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.PumpOnce(ctx, time.Now()); err != nil {
				platform.Logger(ctx).Error("automerge: pump tick failed", "error", err)
			}
		}
	}
}

// PumpOnce runs one full tick: every repo with auto_merge_enabled=true,
// every one of that repo's own bounded-recent auto-approved candidates,
// each re-confirmed live and merged if still eligible. Every repo/
// candidate is independently best-effort (errgroup fans them out, but a
// single candidate's own failure never aborts the others) -- mirrors
// buildAttentionItems' own "each sub-scan independently best-effort"
// precedent (internal/app/decisioninbox/aggregate.go).
func (w *Worker) PumpOnce(ctx context.Context, now time.Time) error {
	repos, err := w.deps.DecisionInbox.ReviewVerdict.RepoSettings.ListAutoMergeEnabled(ctx)
	if err != nil {
		return fmt.Errorf("automerge: list auto-merge-enabled repos: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, repo := range repos {
		repo := repo
		g.Go(func() error {
			w.pumpRepo(gctx, repo.RepoFullName, now)
			return nil
		})
	}
	return g.Wait()
}

// pumpRepo handles one repo's own candidates -- logged, never propagated,
// since PumpOnce's own errgroup fans repos out independently and a
// single repo's own failure must never abort every other armed repo's
// tick.
func (w *Worker) pumpRepo(ctx context.Context, repoFullName string, now time.Time) {
	since := now.Add(-w.deps.Timeouts.AutoMergeCandidateLookback)
	candidates, err := w.deps.DecisionInbox.ReviewVerdict.ReviewVerdicts.ListLatestAutoApproved(ctx, repoFullName, pgtype.Timestamptz{Time: since, Valid: true}, maxCandidatesPerRepoPerTick)
	if err != nil {
		platform.Logger(ctx).Error("automerge: list latest auto-approved verdicts failed", "error", err, "repo_full_name", repoFullName)
		return
	}

	for _, candidate := range candidates {
		w.mergeCandidate(ctx, repoFullName, int(candidate.PrNumber), now)
	}
}

// mergeCandidate re-confirms one candidate live and merges it if still
// eligible -- every outcome (ineligible, revalidation error, merge
// error, success) is logged; a merge audit-log row is written with NO
// human actor (pgtype.UUID{}, the zero value), mirroring §17.5's own
// "the same allowance already made in the audit_log schema for actions
// with no human actor" precedent (internal/adapters/inbound/github/
// pullrequestevent.go's own sentinel-fix merge-gate-evaluated row).
//
// now is pumpRepo's own already-injected clock value, threaded through
// unchanged rather than read fresh here (CLAUDE.md: no time.Now() outside
// an injected clock) -- w.authGuard's own backoff/dead-letter decisions
// (docs/TECHNICAL_PLAN.md §17) must be deterministic under it for tests
// to drive multiple ticks without any real wall-clock sleep.
func (w *Worker) mergeCandidate(ctx context.Context, repoFullName string, prNumber int, now time.Time) {
	logger := platform.Logger(ctx)

	if !w.authGuard.allow(repoFullName, now) {
		// Already classified, backed off, and (past MaxAuthFailures)
		// dead-lettered by an EARLIER call -- the one-time transition log
		// + audit_log row already fired (recordAuthOutcome below); a
		// repeat log line on every subsequent skipped candidate/tick would
		// reproduce exactly the "rate-limit noise" this fix exists to
		// remove, so this path is silent by design.
		return
	}

	// NOTE: no w.authGuard.recordSuccess call on RevalidateForAutoMerge's
	// own success below -- deliberately. A read-only GetOpenPR call
	// succeeding is real evidence w.deps.BotToken can still READ, but it
	// says nothing about whether it can still WRITE (merge) to this
	// repository, which is the SEPARATE, narrower permission
	// ports.ErrPermissionDenied's own repo-scope tracks -- and a real bot
	// token that has lost ONLY merge/write access (branch protection
	// requiring a role the bot lacks, while read access is untouched) is
	// exactly the scenario where every OTHER candidate's own successful
	// revalidate would otherwise reset this repo's own accumulating
	// MergePR-failure streak back to zero on every tick, so
	// domainautomerge.MaxAuthFailures could never actually be reached.
	// Only a genuinely successful MergePR (below) resets either streak.
	ok, headSHA, reason, err := decisioninbox.RevalidateForAutoMerge(ctx, w.deps.DecisionInbox, w.deps.SourceControl, repoFullName, prNumber, w.deps.BotToken)
	if err != nil {
		w.recordAuthOutcome(ctx, repoFullName, err, now)
		logger.Error("automerge: revalidate for auto-merge failed", "error", err, "repo_full_name", repoFullName, "pr_number", prNumber)
		return
	}
	if !ok {
		logger.Info("automerge: candidate no longer eligible", "repo_full_name", repoFullName, "pr_number", prNumber, "reason", reason)
		return
	}

	owner, repo, splitOK := reposource.SplitFullName(repoFullName)
	if !splitOK {
		logger.Error("automerge: repoFullName not shaped owner/repo", "repo_full_name", repoFullName)
		return
	}

	mergeCtx, cancel := context.WithTimeout(ctx, w.deps.Timeouts.GitHubMergePRTimeout)
	mergeSHA, err := w.deps.SourceControl.MergePR(mergeCtx, ports.MergePRSpec{
		Owner: owner, Repo: repo, Number: prNumber, HeadSHA: headSHA, Token: w.deps.BotToken,
	})
	cancel()
	if errors.Is(err, ports.ErrShadowSuppressed) {
		// §30.7: a suppressed merge is "recorded, not merged" -- neither a
		// success nor a failure of the merge. Its own distinct audit
		// action, and deliberately NOT RecordConfirmed: feeding a
		// confirmation the world never saw into the contradiction-rate
		// read model would corrupt the very instrument whose evidence is
		// supposed to justify arming auto-merge for real.
		//
		// Logged at Info, not Error. In a shadow deployment this is the
		// expected path for every candidate, and an error stream that is
		// entirely expected trains an operator to ignore it.
		logger.Info("automerge: merge recorded, not performed -- this repository's egress is suppressed",
			"repo_full_name", repoFullName, "pr_number", prNumber)
		// Known and bounded: this candidate is still returned by the
		// candidate query, so it re-enters here on the next tick and
		// records again. §30.8 closes that at the query level -- an
		// egress-mode stamp on the verdict row, excluded inside
		// ListLatestAutoApproved -- and says explicitly that it must be a
		// query exclusion and "never call-site checks", so a shadow guard
		// added here instead would be building the thing the spec refuses.
		if err := auditlog.Record(ctx, w.deps.AuditLog, pgtype.UUID{}, "shadow.would_have_merged", "pull_request", fmt.Sprintf("%s#%d", repoFullName, prNumber), map[string]any{
			"repo_full_name": repoFullName,
			"pr_number":      prNumber,
			"head_sha":       headSHA,
		}); err != nil {
			logger.Error("automerge: record audit log for suppressed merge failed", "error", err, "repo_full_name", repoFullName, "pr_number", prNumber)
		}
		return
	}
	if err != nil {
		w.recordAuthOutcome(ctx, repoFullName, err, now)
		logger.Error("automerge: merge pr failed", "error", err, "repo_full_name", repoFullName, "pr_number", prNumber)
		return
	}
	w.authGuard.recordSuccess(repoFullName)

	logger.Info("automerge: merged", "repo_full_name", repoFullName, "pr_number", prNumber, "merge_commit_sha", mergeSHA)

	appreviewverdict.RecordConfirmed(ctx, w.deps.DecisionInbox.ReviewVerdict, repoFullName, int32(prNumber), headSHA)

	if err := auditlog.Record(ctx, w.deps.AuditLog, pgtype.UUID{}, "auto_merge.merged", "pull_request", fmt.Sprintf("%s#%d", repoFullName, prNumber), map[string]any{
		"repo_full_name":   repoFullName,
		"pr_number":        prNumber,
		"merge_commit_sha": mergeSHA,
	}); err != nil {
		// The merge already succeeded on GitHub -- a logging failure here
		// must never claim otherwise, mirroring httpapi.MergePullRequest's
		// own identical posture for the human-clicked path.
		logger.Error("automerge: record audit log for merge failed", "error", err, "repo_full_name", repoFullName, "pr_number", prNumber)
	}
}

// recordAuthOutcome feeds one failed GitHub call (from either of
// mergeCandidate's own two call sites -- RevalidateForAutoMerge/MergePR)
// into w.authGuard, and -- ONLY on a brand-new transition into a
// dead-lettered state -- fires the one-time, durable operator-visible
// record docs/TECHNICAL_PLAN.md §17 requires: a log line ALONE is what
// the plan's own gap description calls "only rate-limit noise", so this
// pairs an Error log (immediate, for anyone watching logs live) with an
// audit_log row (durable and queryable after the fact, §17.5's own "no
// actor" precedent this package already reuses for auto_merge.merged/
// shadow.would_have_merged above) -- never a brand-new observability
// mechanism of its own.
//
// A no-op for any error that does not classify as either
// ports.ErrAuthenticationFailed/ports.ErrPermissionDenied (rate limits,
// 5xxs, "not mergeable", a stale head SHA, etc.), and equally a no-op on
// every call AFTER the one that actually tripped the transition --
// authGuard.recordFailure's own doc comment covers both cases (see
// authguard.go).
func (w *Worker) recordAuthOutcome(ctx context.Context, repoFullName string, err error, now time.Time) {
	scope, target, consecutiveFailures := w.authGuard.recordFailure(repoFullName, err, now)
	if scope == authScopeNone {
		return
	}

	logger := platform.Logger(ctx)
	logger.Error("automerge: giving up -- authentication/permission failure exhausted retries, dead-lettering",
		"scope", scope.String(), "repo_full_name", target, "consecutive_failures", consecutiveFailures, "last_error", err)

	// resourceType/resourceID vary by scope: authScopeRepo names the one
	// repository this token was denied for (a real "repository" resource,
	// target non-empty); authScopeWorker has no single resource to name
	// -- w.deps.BotToken itself is not a Postgres row -- so it is recorded
	// against a fixed "automerge_worker" resource type instead, never the
	// empty string mislabeled as a repository.
	resourceType, resourceID := "repository", target
	if scope == authScopeWorker {
		resourceType, resourceID = "automerge_worker", "bot_token"
	}
	if auditErr := auditlog.Record(ctx, w.deps.AuditLog, pgtype.UUID{}, "automerge.auth_dead_lettered", resourceType, resourceID, map[string]any{
		"scope":                scope.String(),
		"repo_full_name":       target,
		"consecutive_failures": consecutiveFailures,
		"last_error":           err.Error(),
	}); auditErr != nil {
		logger.Error("automerge: record audit log for auth dead-letter failed", "error", auditErr, "repo_full_name", target)
	}
}

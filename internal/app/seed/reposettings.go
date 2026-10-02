package seed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/repodemotion"
	"github.com/narvidev/narvi/internal/domain/seedmanifest"
)

// seedRepoSetting reconciles ONE repoSettings manifest entry against
// repo_settings (reconcile-to-declared -- see doc.go). Every field on
// seedmanifest.RepoSetting is an independent *bool: a nil field is left
// completely untouched by this function (nothing is written for it at
// all), matching this table's own existing column-scoped Upsert*
// precedent (UpsertAutoMergeToggle et al. in internal/adapters/outbound/
// postgres/reposettings_store.go). block_on_high_risk/
// sentinel_autofix_enabled are the one EXCEPTION: RepoSettingsStore.
// Upsert writes both together (it predates the column-scoped
// convention own fix only landed for the LATER
// toggles) -- so when only one of the two is declared, this function
// first reads the current row to carry the undeclared one through
// unchanged, rather than resetting it to false.
//
// # SessionsEnabled (§10 Phase 6, §32) is why this tool exists
// for cohort rollout at all
//
// Under NARVI_ROLLOUT_MODE=cohort, httpapi.CreateSessionOnTx refuses
// session creation for any named repo whose repo_settings.
// sessions_enabled is not true (fail-closed) -- and the ONLY REST path
// that could otherwise write a repo_settings row (PutRepoSettings,
// httpapi/reposettings.go) is gated by confirmRepoKnown, which requires
// an EXISTING github_pr_sessions row. That row's only writer
// (internal/adapters/inbound/github's own coalesce.go) commits it
// ATOMICALLY alongside the session it claims a slot for -- so for a
// repo that has never yet had an enrolled session, the claim rolls back
// right along with the refused session, and that repository's
// GitHubPRSessionStore.RepoEntitlement Known stays false forever. REST enrollment is therefore structurally
// impossible for EXACTLY the repos cohort rollout exists to enroll --
// this seed tool bypasses confirmRepoKnown entirely (it writes
// deps.RepoSettings directly, never through the REST layer), which is
// the reason enrollment is seed-manifest-only in v1, not merely a
// convenience choice.
//
// # LiveEgressEnabled (§30.4/§30.8) demotion also runs from here
//
// This is the only writer that can DEMOTE repo_settings.live_egress_enabled
// (a REST route now promotes -- internal/app/shadowoperator.Activate, on
// the operator surface -- but promotion only, and the demotionsweep
// analyzer enforces that split). It is therefore still the one place a
// genuine live->shadow TRANSITION can be detected and acted on: a
// write credential minted just before such a flip stays served for up to
// ScmCredentialTTL (§30.4), so demotion must terminate every live
// sandbox of the repo and cancel any in-flight push signal
// (internal/app/repodemotion.Sweep, called below once the transition
// itself has committed).
func seedRepoSetting(ctx context.Context, deps Deps, s seedmanifest.RepoSetting, dryRun bool) Item {
	key := s.RepoFullName

	current, err := deps.RepoSettings.Get(ctx, s.RepoFullName)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "read current settings: " + err.Error()}
	}
	// A missing row (pgx.ErrNoRows) means every column defaults to its
	// own safe zero value -- current is already the zero sqlcgen.
	// RepoSetting in that case, matching this table's own established
	// "absent row = every flag off" convention (migrations/
	// 000044_repo_settings.up.sql's own doc comment).

	var changed []string
	if s.BlockOnHighRisk != nil {
		changed = append(changed, fmt.Sprintf("blockOnHighRisk=%v", *s.BlockOnHighRisk))
	}
	if s.SentinelAutofixEnabled != nil {
		changed = append(changed, fmt.Sprintf("sentinelAutofixEnabled=%v", *s.SentinelAutofixEnabled))
	}
	if s.AutoMergeEnabled != nil {
		changed = append(changed, fmt.Sprintf("autoMergeEnabled=%v", *s.AutoMergeEnabled))
	}
	if s.AutoRetriggerReview != nil {
		changed = append(changed, fmt.Sprintf("autoRetriggerReviewEnabled=%v", *s.AutoRetriggerReview))
	}
	if s.DescriptionAutofix != nil {
		changed = append(changed, fmt.Sprintf("descriptionAutofixEnabled=%v", *s.DescriptionAutofix))
	}
	if s.SessionsEnabled != nil {
		changed = append(changed, fmt.Sprintf("sessionsEnabled=%v", *s.SessionsEnabled))
	}
	if s.LiveEgressEnabled != nil {
		changed = append(changed, fmt.Sprintf("liveEgressEnabled=%v", *s.LiveEgressEnabled))
	}
	if len(changed) == 0 {
		return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeSkipped, Detail: "no fields declared"}
	}
	detail := strings.Join(changed, ", ")

	if dryRun {
		return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeWouldUpsert, Detail: detail}
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "begin tx: " + err.Error()}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := deps.RepoSettings.WithTx(tx)

	if s.BlockOnHighRisk != nil || s.SentinelAutofixEnabled != nil {
		block := current.BlockOnHighRisk
		if s.BlockOnHighRisk != nil {
			block = *s.BlockOnHighRisk
		}
		sentinel := current.SentinelAutofixEnabled
		if s.SentinelAutofixEnabled != nil {
			sentinel = *s.SentinelAutofixEnabled
		}
		if _, err := store.Upsert(ctx, s.RepoFullName, block, sentinel); err != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "upsert block/sentinel: " + err.Error()}
		}
	}
	if s.AutoMergeEnabled != nil {
		if _, err := store.UpsertAutoMergeToggle(ctx, s.RepoFullName, *s.AutoMergeEnabled); err != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "upsert auto-merge toggle: " + err.Error()}
		}
	}
	if s.AutoRetriggerReview != nil {
		if _, err := store.UpsertAutoRetriggerReviewToggle(ctx, s.RepoFullName, *s.AutoRetriggerReview); err != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "upsert auto-retrigger-review toggle: " + err.Error()}
		}
	}
	if s.DescriptionAutofix != nil {
		if _, err := store.UpsertDescriptionAutofixToggle(ctx, s.RepoFullName, *s.DescriptionAutofix); err != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "upsert description-autofix toggle: " + err.Error()}
		}
	}
	if s.SessionsEnabled != nil {
		if _, err := store.UpsertSessionsEnabled(ctx, s.RepoFullName, *s.SessionsEnabled); err != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "upsert sessions-enabled: " + err.Error()}
		}
	}
	if s.LiveEgressEnabled != nil {
		if _, err := store.UpsertLiveEgressEnabled(ctx, s.RepoFullName, *s.LiveEgressEnabled); err != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "upsert live-egress-enabled: " + err.Error()}
		}
	}

	if err := auditlog.Record(ctx, deps.AuditLog.WithTx(tx), systemActor(), "seed.repo_setting_upserted", "repo_settings", s.RepoFullName, map[string]any{
		"fields": changed,
	}); err != nil {
		return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "record audit log: " + err.Error()}
	}

	// isDemotion captures the pre-write value (current, read before this
	// transaction ever began) against the NEWLY declared one -- a genuine
	// true->false TRANSITION, exactly mirroring UpsertLiveEgressEnabled's
	// own live_egress_promoted_at CASE logic (queries/reposettings.sql):
	// re-affirming an already-shadow repo, or a fresh row that starts
	// shadow, is not a demotion (there is nothing this repo's own
	// sandboxes could have held beyond read-only in either case, §30.8's
	// "shadow-by-default-at-onboarding" -- this window exists only for
	// demotion of a FORMERLY-live repo).
	isDemotion := s.LiveEgressEnabled != nil && current.LiveEgressEnabled && !*s.LiveEgressEnabled

	if err := tx.Commit(ctx); err != nil {
		return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError, Detail: "commit tx: " + err.Error()}
	}

	if isDemotion {
		// §30.4's own demotion fix, run AFTER commit (Sweep is Postgres-
		// only, never a real provider call, but the settings write itself
		// is already durable regardless of what Sweep does): every
		// currently-live sandbox of this now-demoted repo is flagged for
		// real termination (internal/app/reconciler.Reconciler's own new
		// demotion-sweep tick actually issues the StopSandbox call) and
		// has any in-flight push/PR decision cancelled. A Sweep failure is
		// reported as a genuine error -- the demotion itself already
		// committed and cannot (and should not) be rolled back for this,
		// but an operator must be loudly told that this repo's sandboxes
		// were NOT flagged, rather than quietly assuming §30.4's own
		// mandatory termination happened.
		marked, sweepErr := repodemotion.Sweep(ctx, deps.Sandboxes, s.RepoFullName)
		if sweepErr != nil {
			// Reported loudly, and no longer only reported: the flip's own
			// statement stamped demotion_sweep_pending_at, so the
			// obligation is durable and internal/app/reconciler retries it
			// until a sweep completes cleanly. Before that column existed
			// this was the ONLY signal, and re-running the manifest -- the
			// obvious response -- found false->false and swept nothing.
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError,
				Detail: fmt.Sprintf("%s (committed, but repo-demotion sweep failed: %s; the obligation is recorded and the reconciler will retry it)", detail, sweepErr.Error())}
		}
		// Cleared only on a clean sweep, for the same reason the
		// reconciler clears only on a clean sweep: a partially-swept repo
		// whose obligation is cleared is the silent gap the column exists
		// to close.
		if _, clearErr := deps.RepoSettings.ClearDemotionSweepPending(ctx, s.RepoFullName); clearErr != nil {
			return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeError,
				Detail: fmt.Sprintf("%s (swept %d sandbox(es), but clearing the demotion obligation failed: %s; the reconciler will sweep it again, which is harmless)", detail, marked, clearErr.Error())}
		}
		detail = fmt.Sprintf("%s, demotion sweep flagged %d live sandbox(es) for termination", detail, marked)
	}

	return Item{Kind: "repo_setting", Key: key, Outcome: OutcomeUpserted, Detail: detail}
}

// seedRWXPreview reconciles ONE rwxPreview manifest entry against
// repo_settings.rwx_preview_* (reconcile-to-declared, always all 3
// fields together -- see doc.go's own "integrations" scope writeup for
// why this table is this tool's answer to §10's "integrations"
// checklist item).
func seedRWXPreview(ctx context.Context, deps Deps, e seedmanifest.RWXPreview, dryRun bool) Item {
	key := e.RepoFullName
	detail := fmt.Sprintf("orgSlug=%s", e.OrgSlug)

	if dryRun {
		return Item{Kind: "rwx_preview", Key: key, Outcome: OutcomeWouldUpsert, Detail: detail}
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return Item{Kind: "rwx_preview", Key: key, Outcome: OutcomeError, Detail: "begin tx: " + err.Error()}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := deps.RepoSettings.WithTx(tx).UpsertPreviewSettings(ctx, e.RepoFullName, e.DispatchKey, e.EndpointTemplate, e.OrgSlug); err != nil {
		return Item{Kind: "rwx_preview", Key: key, Outcome: OutcomeError, Detail: "upsert failed: " + err.Error()}
	}

	if err := auditlog.Record(ctx, deps.AuditLog.WithTx(tx), systemActor(), "seed.rwx_preview_upserted", "repo_settings", e.RepoFullName, map[string]any{
		"org_slug": e.OrgSlug,
	}); err != nil {
		return Item{Kind: "rwx_preview", Key: key, Outcome: OutcomeError, Detail: "record audit log: " + err.Error()}
	}

	if err := tx.Commit(ctx); err != nil {
		return Item{Kind: "rwx_preview", Key: key, Outcome: OutcomeError, Detail: "commit tx: " + err.Error()}
	}

	return Item{Kind: "rwx_preview", Key: key, Outcome: OutcomeUpserted, Detail: detail}
}

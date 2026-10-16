package postgres

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// Float64ToNumeric converts v into a pgtype.Numeric suitable for a
// NUMERIC column write (§26.7's own review_cost_budget_light_usd/
// review_cost_budget_deep_usd -- migrations/
// 000085_repo_settings_review_cost_budget.up.sql's own doc comment: "pgx's
// own NUMERIC mapping ... converted to/from a plain float64"). v == nil
// (the caller's own "use the engine's own built-in default" case) yields
// the zero pgtype.Numeric ({Valid: false}), which pgx encodes as a genuine
// SQL NULL -- never a fabricated 0.00. strconv.FormatFloat with -1
// precision renders the SHORTEST decimal string that round-trips back to
// the exact same float64 (Go's own documented guarantee for that
// precision value), then ScanScientific -- the same real-binary-verified
// parser pgtype.Numeric's own Scan(string) path already uses -- parses it
// into the Int/Exp pair NUMERIC actually stores, so this never goes
// through a lossy fixed-precision fmt.Sprintf("%.2f", ...) that could
// silently round a caller-supplied ceiling.
func Float64ToNumeric(v *float64) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{}
	}
	var n pgtype.Numeric
	// strconv.FormatFloat never returns an error; ScanScientific's own
	// error is defended against anyway (an invalid pgtype.Numeric --
	// Valid: false -- is exactly the safe "no value" degradation a NULL
	// write already represents, never a panic or a silently wrong value).
	if err := n.ScanScientific(strconv.FormatFloat(*v, 'f', -1, 64)); err != nil {
		return pgtype.Numeric{}
	}
	return n
}

// sessionSpendCapNumeric is Float64ToNumeric for a session spend cap
// (technical plan §40.1), with one difference: a value it cannot convert is
// an error, never NULL. NULL is "no cap", so a conversion that degraded to
// it would lift the cap the caller meant to set. nil is NULL, the caller
// clearing the cap. The caller has already validated the value
// (sessionguard.ValidateSessionSpendCap), so the error is a defense.
func sessionSpendCapNumeric(capUSD *float64) (pgtype.Numeric, error) {
	if capUSD == nil {
		return pgtype.Numeric{}, nil
	}
	if math.IsNaN(*capUSD) || math.IsInf(*capUSD, 0) {
		return pgtype.Numeric{}, fmt.Errorf("postgres: session spend cap %v is not a number of dollars", *capUSD)
	}
	var n pgtype.Numeric
	if err := n.ScanScientific(strconv.FormatFloat(*capUSD, 'f', -1, 64)); err != nil {
		return pgtype.Numeric{}, fmt.Errorf("postgres: session spend cap %v: %w", *capUSD, err)
	}
	return n, nil
}

// RepoSettingsStore is a thin, pass-through wrapper around the
// sqlc-generated repo_settings queries (§8.2, §21.2) -- see
// migrations/000044_repo_settings.up.sql's own doc comment for the "one
// shared table, not one bespoke table per toggle" design this store's own
// narrow surface (today: BlockOnHighRisk alone) is expected to grow
// alongside. No caching, no retries, no business rules -- callers decide
// what a missing row (pgx.ErrNoRows, unwrapped) means for their own
// purposes (internal/adapters/inbound/httpapi/reviewverdict.go treats it,
// and any other read error, as "block_on_high_risk defaults to false",
// mirroring §24.5's own identical per-repo-policy-flag fail-closed
// precedent).
type RepoSettingsStore struct {
	q *sqlcgen.Queries
}

// NewRepoSettingsStore builds a RepoSettingsStore backed by pool.
func NewRepoSettingsStore(pool *pgxpool.Pool) *RepoSettingsStore {
	return &RepoSettingsStore{q: sqlcgen.New(pool)}
}

// WithTx returns a RepoSettingsStore whose queries run on tx instead of the
// pool this store was built with -- mirrors GitHubPRSessionStore.WithTx/
// TurnStore.WithTx exactly (every other multi-column-write store in this
// package already has this). This store's first real callers are the
// column-scoped upserts below (PutAutoApprovalSettings/
// PutAutoMergeToggle, httpapi/reposettings.go); those don't themselves need a
// shared transaction (each is already a single atomic UPDATE), but this
// package's own established integration-test fault-injection idiom (an
// already-rolled-back tx standing in for a genuine store outage, see
// internal/app/decisioninbox's own TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub)
// needs SOME WithTx to target -- adding it here rather than inventing a
// parallel fault-injection mechanism just for this one store.
func (s *RepoSettingsStore) WithTx(tx pgx.Tx) *RepoSettingsStore {
	return &RepoSettingsStore{q: s.q.WithTx(tx)}
}

// Get fetches repoFullName's own settings row. pgx.ErrNoRows (unwrapped)
// means no row exists yet -- every flag on it defaults to its own safe
// value; this is not an error condition for a caller to alarm on.
func (s *RepoSettingsStore) Get(ctx context.Context, repoFullName string) (sqlcgen.RepoSetting, error) {
	return s.q.GetRepoSettings(ctx, repoFullName)
}

// Upsert idempotently creates-or-updates repoFullName's settings row with
// blockOnHighRisk/sentinelAutofixEnabled as the new, full current values
// (never a delta/patch -- see UpsertRepoSettings' own generated doc
// comment). sentinelAutofixEnabled (§17.1) is this same table's
// own further admin-only, per-repo boolean -- migrations/000048's own doc
// comment.
func (s *RepoSettingsStore) Upsert(ctx context.Context, repoFullName string, blockOnHighRisk, sentinelAutofixEnabled bool) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertRepoSettings(ctx, sqlcgen.UpsertRepoSettingsParams{
		RepoFullName:           repoFullName,
		BlockOnHighRisk:        blockOnHighRisk,
		SentinelAutofixEnabled: sentinelAutofixEnabled,
	})
}

// UpsertAutoMergeToggle idempotently creates-or-updates repoFullName's
// §21.2 stage-2 auto-merge toggle -- touches ONLY auto_merge_enabled, leaving
// max_auto_approve_files_changed/sensitive_blast_radius_tags completely
// untouched (see UpsertAutoMergeToggle's own generated doc comment for
// the full "why" this replaces the previous, combined
// UpsertAutoApprovalSettings). Column-scoped: a concurrent
// UpsertAutoApprovalEligibility call for the SAME repo can never be
// clobbered by this write, or vice versa.
func (s *RepoSettingsStore) UpsertAutoMergeToggle(ctx context.Context, repoFullName string, autoMergeEnabled bool) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertAutoMergeToggle(ctx, sqlcgen.UpsertAutoMergeToggleParams{
		RepoFullName:     repoFullName,
		AutoMergeEnabled: autoMergeEnabled,
	})
}

// UpsertAutoApprovalEligibility idempotently creates-or-updates
// repoFullName's §21.2 stage-1 eligibility config -- the column-scoped
// sibling of UpsertAutoMergeToggle above: touches ONLY
// max_auto_approve_files_changed/sensitive_blast_radius_tags, leaving
// auto_merge_enabled completely untouched (see
// UpsertAutoApprovalEligibility's own generated doc comment). nil
// maxAutoApproveFilesChanged / nil sensitiveBlastRadiusTagsJSON both mean
// "use the engine's own built-in default"
// (internal/domain/autoapproval.DefaultEligibilityConfig).
// sensitiveBlastRadiusTagsJSON is pre-marshaled JSON bytes (a JSON array
// of review.Tag strings) -- this store does no JSON encoding of its own,
// mirroring this package's own "thin, pass-through, no business rules"
// discipline; the caller (internal/app/reviewverdict) owns the
// review.Tag <-> JSON conversion.
func (s *RepoSettingsStore) UpsertAutoApprovalEligibility(ctx context.Context, repoFullName string, maxAutoApproveFilesChanged *int32, sensitiveBlastRadiusTagsJSON []byte) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertAutoApprovalEligibility(ctx, sqlcgen.UpsertAutoApprovalEligibilityParams{
		RepoFullName:               repoFullName,
		MaxAutoApproveFilesChanged: maxAutoApproveFilesChanged,
		SensitiveBlastRadiusTags:   sensitiveBlastRadiusTagsJSON,
	})
}

// UpsertAutoRetriggerReviewToggle idempotently creates-or-updates
// repoFullName's §24.5 per-repo opt-in -- COLUMN-SCOPED (mirrors
// UpsertAutoMergeToggle's own identical shape
// pattern generalized to this further, independently-gated toggle):
// touches ONLY auto_retrigger_review_enabled, leaving every other
// repo_settings column completely untouched.
func (s *RepoSettingsStore) UpsertAutoRetriggerReviewToggle(ctx context.Context, repoFullName string, enabled bool) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertAutoRetriggerReviewToggle(ctx, sqlcgen.UpsertAutoRetriggerReviewToggleParams{
		RepoFullName:               repoFullName,
		AutoRetriggerReviewEnabled: enabled,
	})
}

// UpsertDescriptionAutofixToggle idempotently creates-or-updates
// repoFullName's §26.2 per-repo opt-in -- COLUMN-SCOPED (mirrors
// UpsertAutoMergeToggle/UpsertAutoRetriggerReviewToggle's own identical
// shape pattern generalized to this further,
// independently-gated toggle): touches ONLY description_autofix_enabled,
// leaving every other repo_settings column completely untouched.
func (s *RepoSettingsStore) UpsertDescriptionAutofixToggle(ctx context.Context, repoFullName string, enabled bool) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertDescriptionAutofixToggle(ctx, sqlcgen.UpsertDescriptionAutofixToggleParams{
		RepoFullName:              repoFullName,
		DescriptionAutofixEnabled: enabled,
	})
}

// UpsertReviewDepthConfig idempotently creates-or-updates repoFullName's
// §26.3 reviewDepth config -- COLUMN-SCOPED (mirrors
// UpsertAutoMergeToggle/UpsertAutoRetriggerReviewToggle/
// UpsertDescriptionAutofixToggle's own identical shape, the same
// independently-gated-config pattern): touches ONLY review_depth_mode/review_depth_deep_paths,
// leaving every other repo_settings column completely untouched.
// deepPathsJSON is pre-marshaled JSON bytes (a JSON array of glob-pattern
// strings) -- this store does no JSON encoding of its own, mirroring
// UpsertAutoApprovalEligibility's own identical "caller owns the
// encoding" convention.
func (s *RepoSettingsStore) UpsertReviewDepthConfig(ctx context.Context, repoFullName string, mode *string, deepPathsJSON []byte) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertReviewDepthConfig(ctx, sqlcgen.UpsertReviewDepthConfigParams{
		RepoFullName:         repoFullName,
		ReviewDepthMode:      mode,
		ReviewDepthDeepPaths: deepPathsJSON,
	})
}

// UpsertReviewCostBudget writes this repo's own §26.7 cost-ceiling config
// (§26.4) -- lightUSD/deepUSD nil means "use the engine's own built-in
// default", persisted as a genuine SQL NULL (Float64ToNumeric below),
// mirroring UpsertReviewDepthConfig's own identical nil-means-default
// convention immediately above.
func (s *RepoSettingsStore) UpsertReviewCostBudget(ctx context.Context, repoFullName string, lightUSD, deepUSD *float64) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertReviewCostBudget(ctx, sqlcgen.UpsertReviewCostBudgetParams{
		RepoFullName:             repoFullName,
		ReviewCostBudgetLightUsd: Float64ToNumeric(lightUSD),
		ReviewCostBudgetDeepUsd:  Float64ToNumeric(deepUSD),
	})
}

// LockSessionSpendCap row-locks repoFullName's settings row, creating it
// with every column at its default when there is none, and returns its
// session spend cap as it stands -- invalid (SQL NULL) for no cap. The
// first statement of a cap write: its value is the audit row's previous
// cap, and the lock holds until the caller's transaction ends, so a
// concurrent write waits and then reads this one's cap (queries/
// reposettings.sql, LockRepoSessionSpendCap). Run it on a WithTx store.
func (s *RepoSettingsStore) LockSessionSpendCap(ctx context.Context, repoFullName string) (pgtype.Numeric, error) {
	return s.q.LockRepoSessionSpendCap(ctx, repoFullName)
}

// UpsertSessionSpendCap writes repoFullName's session spend cap (technical
// plan §40.1) and returns the whole row; nil clears it. Column-scoped, as
// UpsertReviewCostBudget is. The caller validates the cap first
// (sessionguard.ValidateSessionSpendCap); a value that cannot convert is
// an error here, never a NULL that would lift the cap.
func (s *RepoSettingsStore) UpsertSessionSpendCap(ctx context.Context, repoFullName string, capUSD *float64) (sqlcgen.RepoSetting, error) {
	limit, err := sessionSpendCapNumeric(capUSD)
	if err != nil {
		return sqlcgen.RepoSetting{}, err
	}
	return s.q.UpsertSessionSpendCap(ctx, sqlcgen.UpsertSessionSpendCapParams{
		RepoFullName:       repoFullName,
		SessionSpendCapUsd: limit,
	})
}

// UpsertSessionsEnabled idempotently creates-or-updates repoFullName's
// §10 cohort-rollout enrollment gate (§10 Phase 6, §32) --
// COLUMN-SCOPED (mirrors UpsertAutoMergeToggle/
// UpsertAutoRetriggerReviewToggle/UpsertDescriptionAutofixToggle's own
// identical shape): touches ONLY sessions_enabled, leaving every other
// repo_settings column completely untouched. Called by the seed tool
// only in v1 (§32: enrollment is seed-manifest-only -- see
// internal/app/seed/reposettings.go's own doc comment for the full
// "why REST enrollment is structurally impossible for exactly the repos
// rollout needs to enroll" reasoning).
func (s *RepoSettingsStore) UpsertSessionsEnabled(ctx context.Context, repoFullName string, sessionsEnabled bool) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertSessionsEnabled(ctx, sqlcgen.UpsertSessionsEnabledParams{
		RepoFullName:    repoFullName,
		SessionsEnabled: sessionsEnabled,
	})
}

// UpsertLiveEgressEnabled idempotently creates-or-updates repoFullName's
// §30.8 egress-mode authority -- COLUMN-SCOPED (mirrors
// UpsertSessionsEnabled's own identical shape): touches ONLY
// live_egress_enabled, leaving every other repo_settings column
// completely untouched. Called by the seed tool only in v1 (internal/
// app/seed/reposettings.go) for a DEMOTION, and by internal/app/
// shadowoperator.Activate (an admin-only REST route), which only ever
// promotes -- while seed can flip it either way.
//
// The demotionsweep analyzer constrains which PACKAGES may call this, not
// which direction: it bans the name. shadowoperator is held to promotion
// by its own meta-test asserting the literal true at its single call
// site; seed is safe demoting because it pairs the flip with
// repodemotion.Sweep. An earlier version of this comment credited the
// analyzer with enforcing the direction, which it does not. See that
// file's own doc comment for the full "why" and for how this write is
// journaled to audit_log.
func (s *RepoSettingsStore) UpsertLiveEgressEnabled(ctx context.Context, repoFullName string, liveEgressEnabled bool) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertLiveEgressEnabled(ctx, sqlcgen.UpsertLiveEgressEnabledParams{
		RepoFullName:      repoFullName,
		LiveEgressEnabled: liveEgressEnabled,
	})
}

// ListAutoMergeEnabled returns every repo_settings row with
// auto_merge_enabled = true -- internal/app/automerge's own per-tick
// repo enumeration (see ListAutoMergeEnabledRepos' own generated doc
// comment for why this is the one enumeration source available).
func (s *RepoSettingsStore) ListAutoMergeEnabled(ctx context.Context) ([]sqlcgen.RepoSetting, error) {
	return s.q.ListAutoMergeEnabledRepos(ctx)
}

// UpsertPreviewSettings idempotently creates-or-updates repoFullName's RWX
// preview configuration (§4.1.2 point 1) -- dispatchKey/
// endpointTemplate/orgSlug as the new, full current values for those THREE
// columns only, leaving block_on_high_risk/sentinel_autofix_enabled
// completely untouched (UpsertRWXPreviewSettings' own generated doc
// comment). No admin-facing REST route calls this yet (§4.1's own
// scope is the dispatch/notifier mechanism, not a settings UI) -- today's
// one real caller is this package's own integration tests, exercising the
// exact write path a future settings endpoint would use.
func (s *RepoSettingsStore) UpsertPreviewSettings(ctx context.Context, repoFullName, dispatchKey, endpointTemplate, orgSlug string) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertRWXPreviewSettings(ctx, sqlcgen.UpsertRWXPreviewSettingsParams{
		RepoFullName:               repoFullName,
		RwxPreviewDispatchKey:      &dispatchKey,
		RwxPreviewEndpointTemplate: &endpointTemplate,
		RwxPreviewOrgSlug:          &orgSlug,
	})
}

// UpsertPreviewConfig idempotently creates-or-updates repoFullName's own
// §4.1.2-amendment preview-config row -- the write path behind PUT
// /api/repos/{owner}/{repo}/preview-config (httpapi/previewconfig.go),
// distinct from UpsertPreviewSettings above (that one is this package's
// own older integration-test helper, always overwriting all three
// columns together; see its own doc comment). endpointTemplate/orgSlug
// are ALWAYS written verbatim -- ordinary, full-value semantics.
//
// dispatchKeyProvided/dispatchKey mirror UpsertPreviewConfig's own two
// generated params exactly (postgres/queries/reposettings.sql) -- this
// store does NO interpretation of its own, matching this package's
// established "thin, pass-through, no business rules" discipline
// (RepoSettingsStore's own doc comment): dispatchKeyProvided=false leaves
// the stored rwx_preview_dispatch_key COMPLETELY untouched regardless of
// dispatchKey's own value ("absent means unchanged", §4.1.2 amendment's
// own words); dispatchKeyProvided=true writes dispatchKey verbatim,
// including a nil dispatchKey (a genuine SQL NULL -- httpapi/
// previewconfig.go's own PutPreviewConfig is the one place that decides
// an explicit empty string in the wire request means "clear", translating
// it to (true, nil) before it ever reaches this store).
func (s *RepoSettingsStore) UpsertPreviewConfig(ctx context.Context, repoFullName, endpointTemplate, orgSlug string, dispatchKeyProvided bool, dispatchKey *string) (sqlcgen.RepoSetting, error) {
	return s.q.UpsertPreviewConfig(ctx, sqlcgen.UpsertPreviewConfigParams{
		RepoFullName:               repoFullName,
		RwxPreviewEndpointTemplate: &endpointTemplate,
		RwxPreviewOrgSlug:          &orgSlug,
		DispatchKey:                dispatchKey,
		DispatchKeyProvided:        dispatchKeyProvided,
	})
}

// CountSuppressedRepos returns how many repositories have an explicit
// settings row that has not been promoted to live egress (§30.8).
//
// A floor, not a total: repositories with no settings row at all also
// resolve to shadow, and nothing enumerates those. Callers must present it
// as "at least N", never as a complete count.
func (s *RepoSettingsStore) CountSuppressedRepos(ctx context.Context) (int64, error) {
	return s.q.CountSuppressedRepos(ctx)
}

// ListOwedDemotionSweep returns every repo whose demotion stamped an
// obligation no sweep has yet cleared (§30.4) -- internal/app/reconciler's
// own retry. Empty on an ordinary deployment.
func (s *RepoSettingsStore) ListOwedDemotionSweep(ctx context.Context, limit int32) ([]sqlcgen.RepoSetting, error) {
	return s.q.ListReposOwedDemotionSweep(ctx, limit)
}

// ClearDemotionSweepPending clears the obligation, and must be called ONLY
// after a sweep completed without error: a partially-swept repo whose
// obligation is cleared is the silent gap the column exists to close.
func (s *RepoSettingsStore) ClearDemotionSweepPending(ctx context.Context, repoFullName string) (int64, error) {
	return s.q.ClearDemotionSweepPending(ctx, repoFullName)
}

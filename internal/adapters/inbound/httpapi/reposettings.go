// This file (reposettings.go) implements §8.2's ("server-side
// verdict", §8.2/§21.2) own admin-facing settings surface: GET/PUT
// /api/repos/{owner}/{repo}/settings, backing repo_settings (migrations/
// 000044_repo_settings.up.sql). Today this holds exactly one flag --
// blockOnHighRisk -- but the table (and this endpoint's own DTO,
// restdtos.RepoSettings) is deliberately shaped to grow further admin,
// per-repo booleans later (§17's sentinel auto-fix toggle, §21's auto-
// merge toggle, §24's automatic-re-review opt-in) without a new table or
// endpoint per toggle.
//
// Both routes are gated by domain/authz.Authorize(actor,
// authz.ActionConfigureBlockOnHighRisk, ...) -- admin only (§13.3 row 6,
// mirroring ActionToggleSentinelAutoFix's own identical placement and
// reasoning: this changes what runs UNATTENDED on a repo's own PRs, up to
// and including a hard REQUEST_CHANGES block, never a per-PR human
// judgment call the way row 5's maintainer-level actions are).
//
// Mounted behind auth.Middleware, alongside every other browser-facing
// REST route in this package (unlike scm-credentials/snapshot-mint/
// review-verdict, which are sandbox-bearer-authenticated: this is an
// ADMIN, not the sandbox agent, configuring a policy flag).
//
// fix/repo-scoped-authorization: every route in this file, plus
// reviewanalytics.go/falsepositivepatterns.go/providercredentials.go's own
// repo-scoped route group, now ALSO confirms the URL's own {owner}/{repo}
// is known to this deployment (resolveKnownRepo, below) before any store
// call runs -- the role check above alone used to authorize with an EMPTY
// authz.Resource{}, so the repo named in the URL never entered the
// decision at all. See resolveKnownRepo's own doc comment for the full
// defect and fix.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// resolveKnownRepo joins the {owner}/{repo} chi URL params into the same
// "owner/repo" shape github_pr_sessions.repo_full_name/repo_settings.
// repo_full_name already use -- both chi params are required by the route
// pattern itself, so an empty owner or repo here means a route-mounting
// bug, not malformed caller input; still guarded defensively rather than
// building a bare "/repo" or "owner/" key -- and then confirms this
// deployment actually KNOWS the resulting repo (confirmRepoKnown, below).
// Writes 404 "repo not found" -- the SAME status/message for a malformed
// route AND an unknown repo, so a caller cannot distinguish "you mistyped
// the URL" from "we don't know this repo" -- and returns ok=false on
// either.
//
// # fix/repo-scoped-authorization (this batch)
//
// Every one of this package's repo-scoped handler families used to call
// this function (then named repoFullNameFromRoute) for ONE job only --
// joining the URL's {owner}/{repo} -- and authorize the caller's ROLE with
// an empty authz.Resource{}, entirely independently: the repo named in the
// URL never entered the authorization decision at all. That meant whoever
// passed a role check for ONE repository passed it for EVERY repository,
// simply by editing the URL -- confirmed, not theoretical (see this
// batch's own commit message). This function is now the ONLY place in
// this package that turns route params into a usable repoFullName: there
// is no longer a standalone "just parse the URL" helper to reach for
// instead, so a future handler copying this file's own pattern cannot
// end up with a repo name that was never checked against this
// deployment's own knowledge of what repos exist. providercredentials.go's
// own repo-scoped route group calls this SAME function too (via its own
// repoScopeTarget adapter) -- there is exactly one implementation of this
// check in the package, not two independently-maintained copies.
//
// # Why github_pr_sessions is the entitlement signal, and why
// repo_settings/sessions.repos are NOT
//
// Narvi has no per-repo membership/entitlement model at all (see
// internal/domain/authz/doc.go, §13 -- role is global, one per user, never
// per-repo) -- so this fix is deliberately narrower than "which members
// may reach which repos": it only closes "a repository named in the URL
// must be one this deployment actually knows about", never asserting
// anything about who, among members holding the right role, may reach a
// KNOWN repo. Inventing a broader membership model here would pre-empt an
// open decision that is the repo owner's to make, not this fix's.
//
// Three tables in this schema carry a repo_full_name column (grepped for
// every migration referencing it before choosing). Only ONE is a sound,
// non-self-referential proof of "this deployment is genuinely attached to
// this repo":
//
//   - repo_settings (migrations/000044) is disqualified twice over. Its
//     own doc comment establishes "a row's ABSENCE means every flag
//     defaults to its own safe value" as ordinary, expected behavior
//     (GetRepoSettings' own doc comment: "never a 404: 'no row yet' is
//     not an error condition") -- so a row's mere PRESENCE was never
//     designed to mean "onboarded" either. Worse, it is SELF-REFERENTIAL
//     for exactly the write endpoints this fix must gate: PutRepoSettings/
//     PutAutoApprovalSettings/PutAutoMergeToggle/etc., below, themselves
//     upsert this same table -- using its own existence as the gate would
//     let a maintainer or admin who already holds the ROLE for one of
//     those actions simply write a settings row for an arbitrary,
//     never-onboarded repo first, then pass the "known repo" gate for
//     that same fabricated repo forever after. That is the exact
//     self-fulfilling bypass this fix exists to close, reintroduced by a
//     different door.
//   - sessions.repos (migrations/000018, a JSONB column) is disqualified
//     for the identical reason: CreateSession (authz.ActionCreateSession,
//     §13.3 row 2) lets any MEMBER start a session against any repo
//     string they type into the request body, with no validation that the
//     repo is real -- trivially self-serve at ordinary member privilege,
//     never proof of anything.
//   - github_pr_sessions (migrations/000028) is the one sound signal used
//     here. Its ONLY writer, anywhere in this codebase, is
//     internal/adapters/inbound/github's own webhook ingress
//     (coalesce.go) -- reachable only by a request whose
//     "X-Hub-Signature-256" HMAC verifies against this deployment's real,
//     configured GitHub webhook secret (handler.go, platform.
//     VerifyWebhookSignature). No httpapi REST handler writes this table
//     at all (grepped for before writing this comment) -- a caller of any
//     role, hitting any endpoint in this package, cannot cause a row to
//     exist here as a side effect the way it can for the two tables
//     above. And per coalesce.go's own single-transaction sequencing
//     (EnsureRow, then LockForUpdate, then SetSessionID, committed only
//     after SetSessionID succeeds -- see that file's own doc comment), a
//     row only ever COMMITS with a non-NULL session_id: a denied or
//     failed claim attempt rolls the whole transaction back, leaving no
//     row behind at all. So bare existence (ReadRepoEntitlement's
//     repo_known, no separate session_id filter needed) already means "a real,
//     HMAC-verified GitHub webhook genuinely produced a committed review
//     session for this repo".
//
// # The honest limitation this leaves
//
// A freshly onboarded repo with zero PR mentions yet has no
// github_pr_sessions row, so an admin cannot pre-configure repo settings
// or provider credentials for it before its first PR is ever reviewed.
// No other sound, non-self-referential signal exists in this schema today
// (verified above) -- inventing one (e.g. a dedicated repo-onboarding/
// allowlist table) would itself be a step toward the membership model
// this fix deliberately does not build. Reported here plainly, not
// silently worked around.
func resolveKnownRepo(w http.ResponseWriter, r *http.Request, prSessions *postgres.GitHubPRSessionStore) (string, bool) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	if owner == "" || repo == "" {
		writeError(w, http.StatusNotFound, "repo not found")
		return "", false
	}
	repoFullName := owner + "/" + repo
	if !confirmRepoKnown(w, r, prSessions, repoFullName) {
		return "", false
	}
	return repoFullName, true
}

// confirmRepoKnown checks repoFullName (already parsed/shape-validated by
// the caller) against GitHubPRSessionStore.RepoEntitlement's Known -- see
// resolveKnownRepo's own extended doc comment (above) for the full "why
// github_pr_sessions" reasoning. It reads Known ONLY, deliberately
// ignoring Revoked (§31.4): this is the scoping of the repository-scoped
// admin routes, not admission to new work, and an administrator must still
// reach a revoked repository -- its settings, its history, and the
// entitlement routes that restore it (repoentitlement.go). Writes 404
// "repo not found" and returns false on either a lookup error (fail-closed:
// an unconfirmable repo is treated as unknown, never silently let through)
// or a genuinely unknown repo. Split out from resolveKnownRepo so
// providercredentials.go's own repo-scoped route group -- which resolves
// scopeTargetID slightly differently, since its shared core functions also
// serve the environment-/global-scoped route groups -- can reuse this SAME
// check rather than a second, independently-maintained copy of it.
func confirmRepoKnown(w http.ResponseWriter, r *http.Request, prSessions *postgres.GitHubPRSessionStore, repoFullName string) bool {
	ctx := r.Context()
	facts, err := prSessions.RepoEntitlement(ctx, repoFullName)
	if err != nil {
		platform.Logger(ctx).Error("httpapi: check repo known to deployment failed", "error", err, "repo", repoFullName)
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if !facts.Known {
		logUnknownRepoRefusal(r, repoFullName)
		writeError(w, http.StatusNotFound, "repo not found")
		return false
	}
	return true
}

// logUnknownRepoRefusal logs (app logger only, Warn level) a request that
// passed its own role check but named a repo this deployment does not
// know. This codebase's own audit_log table records completed STATE
// CHANGES only -- every recordAuditLog call site in this package writes a
// row for a change that already happened (session.create, plan.<verdict>,
// false_positive_pattern.retire, ...), never a refusal of any kind -- and
// a role-based authz refusal (helpers.go's own authorize, ErrForbidden
// branch) is likewise never audit-logged today, only silently turned into
// a 403. The one comparable precedent for a REFUSAL specifically is
// internal/adapters/inbound/github/coalesce.go's own "denied by authz"
// lines (logger.Warn, no audit_log row) -- this mirrors that, not
// members.go's write-side audit rows.
func logUnknownRepoRefusal(r *http.Request, repoFullName string) {
	ctx := r.Context()
	logger := platform.Logger(ctx)
	userID := ""
	if authUser, ok := platform.UserFromContext(ctx); ok {
		userID = authUser.ID
	}
	logger.Warn("httpapi: repo-scoped request denied -- repo not known to this deployment", "repo", repoFullName, "user_id", userID, "path", r.URL.Path)
}

// GetRepoSettings backs GET /api/repos/{owner}/{repo}/settings: 403 if the
// caller fails authz.ActionConfigureBlockOnHighRisk (admin only); 200 with
// restdtos.RepoSettings otherwise -- a repo with no repo_settings row yet
// (pgx.ErrNoRows) renders as {repoFullName, blockOnHighRisk: false,
// sentinelAutofixEnabled: false}, both documented safe defaults
// (migrations/000044_repo_settings.up.sql, migrations/
// 000048_repo_settings_sentinel_autofix.up.sql), never a 404: "no row
// yet" is not an error condition for a policy flag that always has a
// well-defined value.
// authorizeAny is helpers.go's own authorize, generalized to "allowed if
// ANY one of actions succeeds" -- §21's own first need for this shape
// (GetRepoSettings below): a maintainer who can configure auto-approval
// eligibility (authz.ActionConfigureAutoApprove, §13.3 row 5) must still
// be able to READ this repo's own settings, even though the admin-only
// authz.ActionConfigureBlockOnHighRisk gate alone would otherwise exclude
// them entirely. Unlike authorize (helpers.go), which writes the 403/500
// response itself on the FIRST failure, this checks every action via the
// same pure authz.Authorize directly (no response side effect) and only
// writes a response once ALL of them have been tried -- a single
// authorize() call at read time is only ever the RIGHT shape when
// exactly one action legitimately governs an endpoint, which is no
// longer true here.
func authorizeAny(w http.ResponseWriter, r *http.Request, resource authz.Resource, actions ...authz.Action) bool {
	ctx := r.Context()
	logger := platform.Logger(ctx)

	authUser, ok := platform.UserFromContext(ctx)
	if !ok {
		logger.Error("httpapi: no authenticated user in context (route not mounted behind auth.Middleware?)")
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	actor := authz.Actor{UserID: authUser.ID, Role: authz.Role(authUser.Role)}

	for _, action := range actions {
		if err := authz.Authorize(actor, action, resource); err == nil {
			return true
		} else if !errors.Is(err, authz.ErrForbidden) {
			logger.Error("httpapi: authz.Authorize failed", "error", err, "action", string(action))
			writeError(w, http.StatusInternalServerError, "internal error")
			return false
		}
	}
	writeError(w, http.StatusForbidden, "not authorized to perform this action")
	return false
}

// GetRepoSettings backs GET /api/repos/{owner}/{repo}/settings -- see this
// file's own doc comment for the base blockOnHighRisk/sentinelAutofixEnabled
// behavior, unchanged by this addition. (§21.1/§21.2) extends the
// response with the auto-approval eligibility config, the auto-merge
// toggle, and the contradiction-rate calibration read model -- all THREE
// additive, read-only from this endpoint's own perspective (writes are
// PutAutoApprovalSettings/PutAutoMergeToggle below, each its own
// separately-gated endpoint).
func GetRepoSettings(repoSettings *postgres.RepoSettingsStore, reviewVerdictDeps appreviewverdict.Deps, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		// (§21.2): a maintainer authorized ONLY for
		// ActionConfigureAutoApprove (row 5) must still be able to read
		// this repo's own settings -- not just the admin-only row 6
		// actions this endpoint originally gated on alone (see
		// authorizeAny's own doc comment above). (§24.5) adds
		// ActionToggleAutoRetriggerReview, (§26.2) adds
		// ActionToggleDescriptionAutofix, and (§26.7) adds
		// ActionConfigureReviewCostBudget, to this SAME "any one of these
		// suffices to read" list -- all the SAME admin-only row as
		// ActionToggleAutoMerge, so this changes nothing about who could
		// already read this endpoint, only documents each new toggle's
		// own read gate explicitly.
		if !authorizeAny(w, r, authz.Resource{}, authz.ActionConfigureBlockOnHighRisk, authz.ActionConfigureAutoApprove, authz.ActionToggleAutoMerge, authz.ActionToggleAutoRetriggerReview, authz.ActionToggleDescriptionAutofix, authz.ActionConfigureReviewDepth, authz.ActionConfigureReviewCostBudget) {
			return
		}

		// fix/repo-scoped-authorization: role check above is necessary but
		// not sufficient -- see resolveKnownRepo's own doc comment for why
		// the URL's own repo must ALSO be confirmed known to this
		// deployment before any store call below ever runs.
		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		resp := restdtos.RepoSettings{RepoFullName: repoFullName}

		settings, err := repoSettings.Get(ctx, repoFullName)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				logger.Error("httpapi: get repo settings failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			// Missing row -- every flag defaults to its own safe value
			// (this table's own established precedent), resp already
			// carries the zero-value false/nil defaults for every field.
		} else {
			resp.BlockOnHighRisk = settings.BlockOnHighRisk
			resp.SentinelAutofixEnabled = settings.SentinelAutofixEnabled
			resp.AutoMergeEnabled = settings.AutoMergeEnabled
			resp.AutoRetriggerReviewEnabled = settings.AutoRetriggerReviewEnabled
			resp.DescriptionAutofixEnabled = settings.DescriptionAutofixEnabled
			if settings.MaxAutoApproveFilesChanged != nil {
				v := int(*settings.MaxAutoApproveFilesChanged)
				resp.MaxAutoApproveFilesChanged = &v
			}
			if tags := autoApprovalTagsFromJSON(settings.SensitiveBlastRadiusTags); tags != nil {
				resp.SensitiveBlastRadiusTags = &tags
			}
			resp.ReviewDepthMode, resp.ReviewDepthDeepPaths = reviewDepthFieldsFromRow(settings)
			resp.ReviewCostBudgetLightUsd, resp.ReviewCostBudgetDeepUsd = reviewCostBudgetFieldsFromRow(settings)
		}

		rate, _, sampleSize, computed, err := appreviewverdict.ContradictionRate(ctx, reviewVerdictDeps, repoFullName, time.Now())
		if err != nil {
			logger.Error("httpapi: compute contradiction rate failed", "error", err)
			// A degraded calibration read must never block viewing the
			// rest of this repo's own settings -- resp.ContradictionRateComputed
			// simply stays false (its own zero value), the SAME "not yet
			// computed" rendering a genuine lack of data produces.
		} else if computed {
			resp.ContradictionRateComputed = true
			percent := rate * 100
			resp.ContradictionRatePercent = &percent
			resp.ContradictionSampleSize = sampleSize
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// autoApprovalTagsFromJSON converts repo_settings.sensitive_blast_radius_tags'
// own raw JSONB bytes into the wire enum slice -- nil input (column NULL)
// or a decode failure both yield a nil result, rendering as the SAME
// "not configured, using the engine's own default" wire value.
func autoApprovalTagsFromJSON(raw []byte) restdtos.RepoSettingsSensitiveBlastRadiusTags {
	tags := reviewTagsFromJSON(raw)
	if len(tags) == 0 {
		return nil
	}
	out := make(restdtos.RepoSettingsSensitiveBlastRadiusTags, len(tags))
	for i, t := range tags {
		out[i] = restdtos.RepoSettingsSensitiveBlastRadiusTagsElem(t)
	}
	return out
}

// reviewDepthFieldsFromRow renders settings' own review_depth_mode/
// review_depth_deep_paths columns (§26.3) into RepoSettings'
// own two wire fields -- shared by every restdtos.RepoSettings
// construction site in this file so none of them independently drifts
// from GetRepoSettings' own field-by-field rendering, mirroring
// autoApprovalTagsFromJSON's own identical "one shared conversion, many
// call sites" precedent.
func reviewDepthFieldsFromRow(settings sqlcgen.RepoSetting) (mode restdtos.RepoSettingsReviewDepthMode, deepPaths *restdtos.RepoSettingsReviewDepthDeepPaths) {
	mode = restdtos.RepoSettingsReviewDepthMode(settings.ReviewDepthMode)
	if len(settings.ReviewDepthDeepPaths) > 0 {
		var paths []string
		if err := json.Unmarshal(settings.ReviewDepthDeepPaths, &paths); err == nil && len(paths) > 0 {
			p := restdtos.RepoSettingsReviewDepthDeepPaths(paths)
			deepPaths = &p
		}
	}
	return mode, deepPaths
}

// reviewCostBudgetFieldsFromRow renders settings' own
// review_cost_budget_light_usd/review_cost_budget_deep_usd columns
// (§26.7) into RepoSettings' own two wire fields -- mirrors
// reviewDepthFieldsFromRow's own identical "one shared conversion, many
// call sites" precedent immediately above, sharing internal/app/
// reviewtriage's own numericToFloat64 conversion rather than
// re-implementing pgtype.Numeric handling a second time.
func reviewCostBudgetFieldsFromRow(settings sqlcgen.RepoSetting) (light, deep *float64) {
	if v, ok := appreviewtriage.NumericToFloat64(settings.ReviewCostBudgetLightUsd); ok {
		light = &v
	}
	if v, ok := appreviewtriage.NumericToFloat64(settings.ReviewCostBudgetDeepUsd); ok {
		deep = &v
	}
	return light, deep
}

// reviewTagsFromJSON decodes a JSON array of tag strings (the SAME wire
// shape internal/app/reviewverdict's own unexported unmarshalTags uses --
// duplicated here, one small function, rather than exporting that
// package's own internal conversion helper purely for this one call
// site).
func reviewTagsFromJSON(raw []byte) []review.Tag {
	if len(raw) == 0 {
		return nil
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err != nil {
		return nil
	}
	tags := make([]review.Tag, len(strs))
	for i, s := range strs {
		tags[i] = review.Tag(s)
	}
	return tags
}

// PutRepoSettings backs PUT /api/repos/{owner}/{repo}/settings: 403 if the
// caller fails EITHER authz.ActionConfigureBlockOnHighRisk OR (§8.2)
// authz.ActionToggleSentinelAutoFix -- both admin-only today (§13.3 row
// 6), checked independently since this ONE endpoint now writes both
// flags together (repo_settings' own "always the full, current desired
// value, never a partial patch" precedent, migrations/000044's own doc
// comment) -- a future divergence in either action's own role matrix
// would still correctly demand BOTH permissions for this combined write,
// rather than silently falling back to whichever check happens to run
// first; 400 for a malformed request body; 200 with the resulting
// restdtos.RepoSettings otherwise. Idempotent create-or-update
// (postgres.RepoSettingsStore.Upsert).
func PutRepoSettings(repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionConfigureBlockOnHighRisk, authz.Resource{}) {
			return
		}
		if !authorize(w, r, authz.ActionToggleSentinelAutoFix, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateRepoSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		settings, err := repoSettings.Upsert(ctx, repoFullName, req.BlockOnHighRisk, req.SentinelAutofixEnabled)
		if err != nil {
			logger.Error("httpapi: upsert repo settings failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		mode, deepPaths := reviewDepthFieldsFromRow(settings)
		costBudgetLight, costBudgetDeep := reviewCostBudgetFieldsFromRow(settings)
		writeJSON(w, http.StatusOK, restdtos.RepoSettings{
			RepoFullName:             repoFullName,
			BlockOnHighRisk:          settings.BlockOnHighRisk,
			SentinelAutofixEnabled:   settings.SentinelAutofixEnabled,
			ReviewDepthMode:          mode,
			ReviewDepthDeepPaths:     deepPaths,
			ReviewCostBudgetLightUsd: costBudgetLight,
			ReviewCostBudgetDeepUsd:  costBudgetDeep,
		})
	}
}

// PutAutoApprovalSettings backs PUT /api/repos/{owner}/{repo}/auto-approval-settings
// (§21.2 stage 1) -- the auto-approval eligibility engine's own
// two per-repo-tunable criteria. Gated SOLELY by authz.ActionConfigureAutoApprove
// (maintainer+, §13.3 row 5) -- a SEPARATE endpoint from PutRepoSettings
// above specifically so a maintainer authorized for this row never needs
// (and never gets asked to hold) admin-only ActionConfigureBlockOnHighRisk/
// ActionToggleSentinelAutoFix/ActionToggleAutoMerge just to reach it (see
// UpdateAutoApprovalSettingsRequest's own doc comment, contracts/rest/v1/
// dtos.schema.json).
//
// A COLUMN-SCOPED write, via appreviewverdict.UpsertAutoApprovalEligibility
// -- touches ONLY max_auto_approve_files_changed/sensitive_blast_radius_tags
// at the SQL level, never repo_settings.auto_merge_enabled (PutAutoMergeToggle
// below owns that column, under its own admin-only gate). Replaces the
// PREVIOUS read-modify-write (read auto_merge_enabled first, pass it
// straight through unchanged on every write) -- that pattern is exactly
// the race this fix closes: a maintainer's write here and an admin's
// PutAutoMergeToggle write, landing concurrently, could each read the
// OTHER's stale pre-write value and silently clobber it, including
// reverting a toggle an admin just armed/disarmed. No read-before-write
// is needed anymore at all -- the column-scoped UPDATE simply never
// touches the column it doesn't own, so there is nothing to preserve.
func PutAutoApprovalSettings(repoSettings *postgres.RepoSettingsStore, reviewVerdictDeps appreviewverdict.Deps, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionConfigureAutoApprove, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateAutoApprovalSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		var tags []review.Tag
		if req.SensitiveBlastRadiusTags != nil {
			tags = make([]review.Tag, len(*req.SensitiveBlastRadiusTags))
			for i, t := range *req.SensitiveBlastRadiusTags {
				tags[i] = review.Tag(t)
			}
		}

		updated, err := appreviewverdict.UpsertAutoApprovalEligibility(ctx, reviewVerdictDeps, repoFullName, (*int)(req.MaxAutoApproveFilesChanged), tags)
		if err != nil {
			logger.Error("httpapi: upsert auto-approval eligibility failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, autoApprovalSettingsToRepoSettingsDTO(ctx, repoSettings, repoFullName, updated))
	}
}

// PutAutoMergeToggle backs PUT /api/repos/{owner}/{repo}/auto-merge
// (§21.2 stage 2) -- arms/disarms the per-repo unattended-merge
// toggle. Gated SOLELY by authz.ActionToggleAutoMerge (admin only, §13.3
// row 6) -- see UpdateAutoMergeToggleRequest's own doc comment for why
// this is a separate endpoint from PutAutoApprovalSettings above.
//
// Mirrored in the other direction: a COLUMN-SCOPED write, via
// appreviewverdict.UpsertAutoMergeToggle --
// touches ONLY auto_merge_enabled, never the eligibility-config columns
// PutAutoApprovalSettings above owns. See that handler's own doc comment
// for the full "why" this replaces the previous read-modify-write.
func PutAutoMergeToggle(repoSettings *postgres.RepoSettingsStore, reviewVerdictDeps appreviewverdict.Deps, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionToggleAutoMerge, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateAutoMergeToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		updated, err := appreviewverdict.UpsertAutoMergeToggle(ctx, reviewVerdictDeps, repoFullName, req.Enabled)
		if err != nil {
			logger.Error("httpapi: upsert auto-merge toggle failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, autoApprovalSettingsToRepoSettingsDTO(ctx, repoSettings, repoFullName, updated))
	}
}

// PutAutoRetriggerReviewToggle backs PUT
// /api/repos/{owner}/{repo}/auto-retrigger-review (§24.5) --
// arms/disarms the per-repo automatic-re-review-on-new-commits opt-in.
// Gated SOLELY by authz.ActionToggleAutoRetriggerReview (admin only,
// §13.3 row 6) -- see UpdateAutoRetriggerReviewToggleRequest's own doc
// comment for why this is a separate endpoint from PutRepoSettings above,
// mirroring PutAutoMergeToggle's own identical reasoning.
//
// COLUMN-SCOPED write, via postgres.RepoSettingsStore.
// UpsertAutoRetriggerReviewToggle -- touches ONLY
// auto_retrigger_review_enabled, never any other repo_settings column
// (the same column-scoped-write discipline as above, applied here from
// the start rather than as a later fix). Unlike
// PutAutoMergeToggle, this store method already returns the FULL,
// just-written repo_settings row, so no follow-up Get call is needed to
// render every OTHER field on the response.
func PutAutoRetriggerReviewToggle(repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionToggleAutoRetriggerReview, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateAutoRetriggerReviewToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		settings, err := repoSettings.UpsertAutoRetriggerReviewToggle(ctx, repoFullName, req.Enabled)
		if err != nil {
			logger.Error("httpapi: upsert auto-retrigger-review toggle failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp := restdtos.RepoSettings{
			RepoFullName:               repoFullName,
			BlockOnHighRisk:            settings.BlockOnHighRisk,
			SentinelAutofixEnabled:     settings.SentinelAutofixEnabled,
			AutoMergeEnabled:           settings.AutoMergeEnabled,
			AutoRetriggerReviewEnabled: settings.AutoRetriggerReviewEnabled,
			DescriptionAutofixEnabled:  settings.DescriptionAutofixEnabled,
		}
		if settings.MaxAutoApproveFilesChanged != nil {
			v := int(*settings.MaxAutoApproveFilesChanged)
			resp.MaxAutoApproveFilesChanged = &v
		}
		if tags := autoApprovalTagsFromJSON(settings.SensitiveBlastRadiusTags); tags != nil {
			resp.SensitiveBlastRadiusTags = &tags
		}
		resp.ReviewDepthMode, resp.ReviewDepthDeepPaths = reviewDepthFieldsFromRow(settings)
		resp.ReviewCostBudgetLightUsd, resp.ReviewCostBudgetDeepUsd = reviewCostBudgetFieldsFromRow(settings)

		writeJSON(w, http.StatusOK, resp)
	}
}

// PutDescriptionAutofixToggle backs PUT
// /api/repos/{owner}/{repo}/description-autofix (§26.2) --
// arms/disarms the per-repo Narvi-authored-PR description-autofix
// toggle. Gated SOLELY by authz.ActionToggleDescriptionAutofix (admin
// only, §13.3 row 6) -- see UpdateDescriptionAutofixToggleRequest's own
// doc comment for why this is a separate endpoint from PutRepoSettings
// above, mirroring PutAutoRetriggerReviewToggle's own identical
// reasoning.
//
// COLUMN-SCOPED write, via postgres.RepoSettingsStore.
// UpsertDescriptionAutofixToggle -- touches ONLY
// description_autofix_enabled, never any other repo_settings column (the same
// column-scoped-write discipline, applied here
// from the start). This store method returns the FULL, just-written
// repo_settings row, so no follow-up Get call is needed to render every
// OTHER field on the response, exactly like PutAutoRetriggerReviewToggle
// above.
func PutDescriptionAutofixToggle(repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionToggleDescriptionAutofix, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateDescriptionAutofixToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		settings, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, req.Enabled)
		if err != nil {
			logger.Error("httpapi: upsert description-autofix toggle failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp := restdtos.RepoSettings{
			RepoFullName:               repoFullName,
			BlockOnHighRisk:            settings.BlockOnHighRisk,
			SentinelAutofixEnabled:     settings.SentinelAutofixEnabled,
			AutoMergeEnabled:           settings.AutoMergeEnabled,
			AutoRetriggerReviewEnabled: settings.AutoRetriggerReviewEnabled,
			DescriptionAutofixEnabled:  settings.DescriptionAutofixEnabled,
		}
		if settings.MaxAutoApproveFilesChanged != nil {
			v := int(*settings.MaxAutoApproveFilesChanged)
			resp.MaxAutoApproveFilesChanged = &v
		}
		if tags := autoApprovalTagsFromJSON(settings.SensitiveBlastRadiusTags); tags != nil {
			resp.SensitiveBlastRadiusTags = &tags
		}
		resp.ReviewDepthMode, resp.ReviewDepthDeepPaths = reviewDepthFieldsFromRow(settings)
		resp.ReviewCostBudgetLightUsd, resp.ReviewCostBudgetDeepUsd = reviewCostBudgetFieldsFromRow(settings)

		writeJSON(w, http.StatusOK, resp)
	}
}

// autoApprovalSettingsToRepoSettingsDTO renders updated (this call's own
// just-written §21.2 columns) plus a fresh read of blockOnHighRisk/
// sentinelAutofixEnabled (a DIFFERENT write path, PutRepoSettings above)
// into one complete restdtos.RepoSettings -- both PutAutoApprovalSettings
// and PutAutoMergeToggle share this so their own response shape never
// independently drifts from GetRepoSettings' own field-by-field
// construction.
func autoApprovalSettingsToRepoSettingsDTO(ctx context.Context, repoSettings *postgres.RepoSettingsStore, repoFullName string, updated appreviewverdict.AutoApprovalSettings) restdtos.RepoSettings {
	resp := restdtos.RepoSettings{RepoFullName: repoFullName, AutoMergeEnabled: updated.AutoMergeEnabled}
	if updated.MaxAutoApproveFilesChanged != nil {
		v := *updated.MaxAutoApproveFilesChanged
		resp.MaxAutoApproveFilesChanged = &v
	}
	if len(updated.SensitiveBlastRadiusTags) > 0 {
		tags := make(restdtos.RepoSettingsSensitiveBlastRadiusTags, len(updated.SensitiveBlastRadiusTags))
		for i, t := range updated.SensitiveBlastRadiusTags {
			tags[i] = restdtos.RepoSettingsSensitiveBlastRadiusTagsElem(t)
		}
		resp.SensitiveBlastRadiusTags = &tags
	}

	if settings, err := repoSettings.Get(ctx, repoFullName); err == nil {
		resp.BlockOnHighRisk = settings.BlockOnHighRisk
		resp.SentinelAutofixEnabled = settings.SentinelAutofixEnabled
		resp.ReviewDepthMode, resp.ReviewDepthDeepPaths = reviewDepthFieldsFromRow(settings)
		resp.ReviewCostBudgetLightUsd, resp.ReviewCostBudgetDeepUsd = reviewCostBudgetFieldsFromRow(settings)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		platform.Logger(ctx).Error("httpapi: read back block-on-high-risk/sentinel-autofix fields failed", "error", err)
	}

	return resp
}

// reviewDepthModeString validates req's own optional mode string against
// internal/domain/reviewtriage.Mode's own three legal values (application-
// side, not schema-level -- see RepoSettings.reviewDepthMode's own doc
// comment, contracts/rest/v1/dtos.schema.json, for why a nullable-enum
// wire field is deliberately avoided). nil/empty is always legal ("use
// the built-in default"); a non-empty, unrecognized string is rejected
// with a 400 rather than silently stored and silently reinterpreted as
// "auto" later (reviewtriage.Mode's own fail-conservative-at-READ-time
// policy is for a value ALREADY on record, e.g. one written by an older
// version of this check -- this is the one place a NEW value is still
// rejectable outright, and doing so here is strictly more helpful to the
// admin submitting it than a silent, unexplained no-op would be).
func reviewDepthModeString(mode restdtos.UpdateReviewDepthConfigRequestMode) (*string, bool) {
	if mode == nil {
		return nil, true
	}
	s := string(*mode)
	switch reviewtriage.Mode(s) {
	case reviewtriage.ModeAuto, reviewtriage.ModeAlwaysLight, reviewtriage.ModeAlwaysDeep:
		return &s, true
	default:
		return nil, false
	}
}

// PutReviewDepthConfig backs PUT /api/repos/{owner}/{repo}/review-depth
// (§26.3) -- (re)configures this repo's own reviewDepth mode/
// deepPaths. Gated SOLELY by authz.ActionConfigureReviewDepth (admin
// only, §13.3 row 6) -- see UpdateReviewDepthConfigRequest's own doc
// comment (contracts/rest/v1/dtos.schema.json) for why this is a
// separate endpoint from PutRepoSettings above, mirroring
// PutAutoRetriggerReviewToggle/PutDescriptionAutofixToggle's own
// identical reasoning.
//
// COLUMN-SCOPED write, via postgres.RepoSettingsStore.
// UpsertReviewDepthConfig -- touches ONLY review_depth_mode/
// review_depth_deep_paths, never any other repo_settings column (the same
// column-scoped-write discipline, applied here
// from the start). This store method already returns the FULL,
// just-written repo_settings row, so no follow-up Get call is needed to
// render every OTHER field on the response, exactly like
// PutAutoRetriggerReviewToggle/PutDescriptionAutofixToggle above.
func PutReviewDepthConfig(repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionConfigureReviewDepth, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateReviewDepthConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		mode, ok := reviewDepthModeString(req.Mode)
		if !ok {
			writeError(w, http.StatusBadRequest, "mode must be one of auto/always_light/always_deep, or omitted/null")
			return
		}

		var deepPathsJSON []byte
		var paths []string
		if req.DeepPaths != nil {
			paths = []string(*req.DeepPaths)
		}
		marshaled, err := json.Marshal(paths)
		if err != nil {
			logger.Error("httpapi: marshal review-depth deep paths failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		deepPathsJSON = marshaled

		settings, err := repoSettings.UpsertReviewDepthConfig(ctx, repoFullName, mode, deepPathsJSON)
		if err != nil {
			logger.Error("httpapi: upsert review-depth config failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp := restdtos.RepoSettings{
			RepoFullName:               repoFullName,
			BlockOnHighRisk:            settings.BlockOnHighRisk,
			SentinelAutofixEnabled:     settings.SentinelAutofixEnabled,
			AutoMergeEnabled:           settings.AutoMergeEnabled,
			AutoRetriggerReviewEnabled: settings.AutoRetriggerReviewEnabled,
			DescriptionAutofixEnabled:  settings.DescriptionAutofixEnabled,
		}
		if settings.MaxAutoApproveFilesChanged != nil {
			v := int(*settings.MaxAutoApproveFilesChanged)
			resp.MaxAutoApproveFilesChanged = &v
		}
		if tags := autoApprovalTagsFromJSON(settings.SensitiveBlastRadiusTags); tags != nil {
			resp.SensitiveBlastRadiusTags = &tags
		}
		resp.ReviewDepthMode, resp.ReviewDepthDeepPaths = reviewDepthFieldsFromRow(settings)
		resp.ReviewCostBudgetLightUsd, resp.ReviewCostBudgetDeepUsd = reviewCostBudgetFieldsFromRow(settings)

		writeJSON(w, http.StatusOK, resp)
	}
}

// PutReviewCostBudget backs PUT /api/repos/{owner}/{repo}/review-cost-budget
// (§26.7) -- (re)configures this repo's own per-path cost
// ceilings. Gated SOLELY by authz.ActionConfigureReviewCostBudget (admin
// only, §13.3 row 6) -- see UpdateReviewCostBudgetRequest's own doc
// comment (contracts/rest/v1/dtos.schema.json) for why this is a separate
// endpoint from PutRepoSettings above, mirroring PutReviewDepthConfig's
// own identical reasoning.
//
// COLUMN-SCOPED write, via postgres.RepoSettingsStore.
// UpsertReviewCostBudget -- touches ONLY review_cost_budget_light_usd/
// review_cost_budget_deep_usd, never any other repo_settings column (the same
// column-scoped-write discipline, applied here
// from the start, exactly like PutReviewDepthConfig above). This store
// method already returns the FULL, just-written repo_settings row, so no
// follow-up Get call is needed to render every OTHER field on the
// response.
func PutReviewCostBudget(repoSettings *postgres.RepoSettingsStore, prSessions *postgres.GitHubPRSessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if !authorize(w, r, authz.ActionConfigureReviewCostBudget, authz.Resource{}) {
			return
		}

		repoFullName, ok := resolveKnownRepo(w, r, prSessions)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.UpdateReviewCostBudgetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}

		lightUSD := (*float64)(req.LightUsd)
		deepUSD := (*float64)(req.DeepUsd)
		// <= 0, not < 0: reviewtriage.CostBudget's own zero value means "no
		// ceiling configured" (ShouldSkipOptionalPass, internal/domain/
		// reviewtriage/costbudget.go's own doc comment: "a zero ceilingUSD
		// ... NEVER skips"), so an explicit lightUsd/deepUsd of 0 stored
		// here would silently collide with that "unconfigured" sentinel and
		// resolve to UNLIMITED spend -- the opposite of what an operator
		// setting an explicit 0 almost certainly intends. Rejecting it with
		// a 400 (rather than silently accepting and reinterpreting it) is
		// the SAME "never silently reinterpret a value the caller
		// explicitly set" discipline reviewDepthModeString already applies
		// to an unrecognized mode string, immediately below.
		if (lightUSD != nil && *lightUSD <= 0) || (deepUSD != nil && *deepUSD <= 0) {
			writeError(w, http.StatusBadRequest, "lightUsd and deepUsd must be positive (zero would collide with the built-in 'no ceiling configured' sentinel and silently mean unlimited), or null to use the built-in default")
			return
		}

		settings, err := repoSettings.UpsertReviewCostBudget(ctx, repoFullName, lightUSD, deepUSD)
		if err != nil {
			logger.Error("httpapi: upsert review cost budget failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp := restdtos.RepoSettings{
			RepoFullName:               repoFullName,
			BlockOnHighRisk:            settings.BlockOnHighRisk,
			SentinelAutofixEnabled:     settings.SentinelAutofixEnabled,
			AutoMergeEnabled:           settings.AutoMergeEnabled,
			AutoRetriggerReviewEnabled: settings.AutoRetriggerReviewEnabled,
			DescriptionAutofixEnabled:  settings.DescriptionAutofixEnabled,
		}
		if settings.MaxAutoApproveFilesChanged != nil {
			v := int(*settings.MaxAutoApproveFilesChanged)
			resp.MaxAutoApproveFilesChanged = &v
		}
		if tags := autoApprovalTagsFromJSON(settings.SensitiveBlastRadiusTags); tags != nil {
			resp.SensitiveBlastRadiusTags = &tags
		}
		resp.ReviewDepthMode, resp.ReviewDepthDeepPaths = reviewDepthFieldsFromRow(settings)
		resp.ReviewCostBudgetLightUsd, resp.ReviewCostBudgetDeepUsd = reviewCostBudgetFieldsFromRow(settings)

		writeJSON(w, http.StatusOK, resp)
	}
}

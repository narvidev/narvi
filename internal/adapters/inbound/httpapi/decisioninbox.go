// This file (decisioninbox.go) backs §16's own two REST endpoints
// ("decision inbox: read model + API", §16): GET /api/decision-inbox (the
// read model, ListDecisionInbox below) and POST /api/decision-inbox/merge
// (§16.2's own Merge endpoint, MergePullRequest below) -- the ONLY
// state-changing action this Step introduces, and the one place its own
// "read model, not new state" scope (aggregate.go's own doc comment)
// does not apply, since merging is inherently an action against GitHub,
// not a Postgres write.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// ListDecisionInbox backs GET /api/decision-inbox (§16.2/§16.3 -- Phase 5
// half: read model + endpoints). No authz.Authorize gate: this is a
// personalized READ of the caller's own pending decisions, the same
// "everyone, including viewer, read-only" posture §13.3 row 1 already
// grants view_sessions/view_analytics -- a viewer sees their own queue,
// just without the ability to act on any row that isn't already denied
// them by MergePullRequest's own RBAC check below. needs_attention rows
// are included only for an admin actor (§16.1's own parenthetical) --
// enforced inside decisioninbox.Build itself, never filtered here.
func ListDecisionInbox(deps decisioninbox.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		authUser, ok := platform.UserFromContext(ctx)
		if !ok {
			logger.Error("httpapi: no authenticated user in context (route not mounted behind auth.Middleware?)")
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		result, err := decisioninbox.Build(ctx, deps, actorUserID, authz.Role(authUser.Role), time.Now())
		if err != nil {
			logger.Error("httpapi: build decision inbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, decisionInboxResultToDTO(result))
	}
}

// decisionInboxResultToDTO converts the app-layer decisioninbox.Result
// into its REST wire shape (contracts/rest/v1/dtos.schema.json). A pure,
// side-effect-free mapping -- every field here already exists on result;
// this function invents nothing.
func decisionInboxResultToDTO(result decisioninbox.Result) restdtos.ListDecisionInboxResponse {
	items := make([]restdtos.DecisionInboxItem, len(result.Items))
	for i, it := range result.Items {
		items[i] = decisionInboxItemToDTO(it)
	}

	resp := restdtos.ListDecisionInboxResponse{
		Items:                     items,
		ScmAsOf:                   result.SCMAsOf,
		ScmFetchFailed:            result.SCMFetchFailed,
		DecisionLatencySampleSize: result.DecisionLatencySampleSize,
		DecisionLatencyComputed:   result.DecisionLatencyComputed,
	}
	if result.DecisionLatencyComputed {
		seconds := result.DecisionLatencyMedian.Seconds()
		resp.DecisionLatencyMedianSeconds = &seconds
	}
	return resp
}

func decisionInboxItemToDTO(it decisioninbox.Item) restdtos.DecisionInboxItem {
	dto := restdtos.DecisionInboxItem{
		Kind:           restdtos.DecisionInboxItemKind(it.Kind),
		Title:          it.Title,
		EnteredQueueAt: it.EnteredQueueAt,
		AgeSeconds:     int(it.AgeSeconds),
		Stale:          it.Stale,
	}

	if it.RepoFullName != "" {
		dto.RepoFullName = &it.RepoFullName
	}
	if it.PRNumber != 0 {
		n := it.PRNumber
		dto.PrNumber = &n
	}
	if it.HTMLURL != "" {
		dto.HtmlUrl = &it.HTMLURL
	}
	if it.HeadSHA != "" {
		dto.HeadSha = &it.HeadSHA
	}
	if it.Provenance != nil {
		dto.ProvenanceKind = &restdtos.DecisionInboxItemProvenanceKind{Value: string(it.Provenance.Kind)}
		if it.Provenance.RepoFullName != "" {
			dto.ProvenanceRepoFullName = &it.Provenance.RepoFullName
		}
		if it.Provenance.Pattern != "" {
			dto.ProvenancePattern = &it.Provenance.Pattern
		}

		// ciGreen/findings/isHandoff/hasApprovingReview/hasChangesRequested
		// are PR-shaped fields, gated on "is this row a PR at all"
		// (it.Provenance is set unconditionally by buildPROpenItem for
		// EVERY PR row) -- deliberately NOT on Kind: Kind alone cannot
		// distinguish a handoff PR
		// (KindAwaitingApproval, Provenance non-nil) from an ordinary
		// plan-approval row (KindAwaitingApproval, Provenance nil), so the
		// previous Kind==ready_to_merge/needs_review gate silently nulled
		// isHandoff for the ONE row kind that field exists to identify.
		ciGreen := it.CIGreen
		dto.CiGreen = &ciGreen
		// findings renders null, never it.Findings' own internal
		// fail-closed sentinel value, whenever the count itself could not
		// be determined -- see
		// Item.FindingsUnknown's own doc comment: that sentinel exists
		// ONLY to fail the eligibility computation closed, and must never
		// be presented on the wire as an honest, real findings count.
		if !it.FindingsUnknown {
			findings := it.Findings
			dto.Findings = &findings
		}
		isHandoff := it.IsHandoff
		dto.IsHandoff = &isHandoff
		hasApprovingReview := it.HasApprovingReview
		dto.HasApprovingReview = &hasApprovingReview
		hasChangesRequested := it.HasChangesRequested
		dto.HasChangesRequested = &hasChangesRequested

		// isRelease is set unconditionally, exactly like isHandoff above --
		// the field a client checks to render this row's own distinct
		// "release" shape. manifestFindingsCount/aggregateReviewTriggered
		// render ONLY when isRelease is true: an ordinary PR row has no
		// manifest check at all, and rendering a fabricated 0 there would
		// be indistinguishable from a genuine, computed "zero findings"
		// release-cut result. manifestCoveragePartial rides the same gate:
		// it qualifies manifestFindingsCount, so it is meaningless
		// wherever that count is absent.
		isRelease := it.IsRelease
		dto.IsRelease = &isRelease
		if it.IsRelease {
			manifestFindingsCount := it.ManifestFindingsCount
			dto.ManifestFindingsCount = &manifestFindingsCount
			aggregateReviewTriggered := it.AggregateReviewTriggered
			dto.AggregateReviewTriggered = &aggregateReviewTriggered
			manifestCoveragePartial := it.ManifestCoveragePartial
			dto.ManifestCoveragePartial = &manifestCoveragePartial

			// compositionReviewed/compositionDecision (confirmed-major
			// fix): rendered ONLY when isRelease is true, mirroring
			// manifestFindingsCount immediately above -- compositionDecision
			// stays entirely absent from the wire (never a fabricated
			// "pending") unless the composition pass has actually
			// completed (it.CompositionReviewed), so a client can never
			// mistake "not yet reviewed" for a real, still-open decision.
			compositionReviewed := it.CompositionReviewed
			dto.CompositionReviewed = &compositionReviewed
			if it.CompositionReviewed {
				dto.CompositionDecision = &restdtos.DecisionInboxItemCompositionDecision{Value: it.CompositionDecision}
			}
		}
	}
	if it.RiskLabel != "" {
		dto.RiskLabel = &it.RiskLabel
	}

	if it.PlanID != "" {
		dto.PlanId = &it.PlanID
	}
	if it.SessionID != "" {
		dto.SessionId = &it.SessionID
	}
	if it.FailureReason != "" {
		dto.FailureReason = &it.FailureReason
	}
	if it.AutomationID != "" {
		dto.AutomationId = &it.AutomationID
	}
	if it.ArtifactSummary != "" {
		dto.ArtifactSummary = &it.ArtifactSummary
	}
	if it.OutboxID != "" {
		dto.OutboxId = &it.OutboxID
	}
	if it.OutboxKind != "" {
		dto.OutboxKind = &it.OutboxKind
	}
	if it.LastError != "" {
		dto.LastError = &it.LastError
	}

	return dto
}

// MergePullRequest backs POST /api/decision-inbox/merge (§16.2's own
// Merge endpoint; mockups.html decision 33: "Auto-approved still means
// human-merged... The Merge button re-validates CI, approval state, and
// RBAC server-side at click time; the rendered queue is never trusted as
// authority"). Sequence: decode + validate the request, a CHEAP role-only
// authz pre-check (below), resolve the caller's
// own decrypted GitHub token, LIVE-revalidate (decisioninbox.
// RevalidateForMerge -- never the cache), THEN the SAME authz.Authorize
// call AGAIN -- now authoritative (ActionMergePR, OwnedOrJoined=true --
// only ever legitimately true once revalidation itself has confirmed the
// PR is currently assigned to this actor), THEN call SourceControl.
// MergePR, THEN record the audit log. Any step failing stops the
// sequence -- an already-succeeded GitHub merge is never undone by a
// later step's own failure (only the audit-log write, which is
// best-effort and logged, not retried or rolled back, mirroring this
// codebase's own "the merge already happened, a logging failure must
// never claim otherwise" discipline).
//
// The "approval state" mockups.html's own decision 33 says this endpoint
// re-validates is SPECIFICALLY GitHub's own
// HasChangesRequested fact, enforced inside RevalidateForMerge -- a hard
// block, since nobody should one-click merge a PR a human explicitly
// requested changes on. It is NOT GitHub's own HasApprovingReview: §16.1
// defines ready_to_merge's own "approved" as auto-approval BY THE
// DETERMINISTIC ELIGIBILITY ENGINE (RevalidateForMerge's own
// ComputeAutoApprovalEligible re-check), never a human GitHub review --
// requiring one here on top of that would silently redefine what
// "auto-approved" means for exactly the population this endpoint exists
// to fast-path. HasApprovingReview is surfaced to the actor as a plain
// display field instead (decisioninbox.Item.HasApprovingReview).
func MergePullRequest(deps decisioninbox.Deps, sourceControl ports.SourceControl, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		if sourceControl == nil {
			logger.Error("httpapi: merge pull request: no SourceControl configured")
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		authUser, ok := platform.UserFromContext(ctx)
		if !ok {
			logger.Error("httpapi: no authenticated user in context (route not mounted behind auth.Middleware?)")
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.MergePullRequestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		if req.RepoFullName == "" || req.PrNumber <= 0 {
			writeError(w, http.StatusBadRequest, "repoFullName and a positive prNumber are required")
			return
		}
		owner, repo, ok := reposource.SplitFullName(req.RepoFullName)
		if !ok {
			writeError(w, http.StatusBadRequest, "repoFullName must be shaped owner/repo")
			return
		}

		// Cheap, role-only authz PRE-CHECK --
		// short-circuits a viewer (denied ActionMergePR unconditionally,
		// regardless of OwnedOrJoined -- see authz.Authorize's own
		// matrix, internal/domain/authz/authorize.go) at ZERO I/O, before
		// ever paying for the expensive live SCM re-validation below (up
		// to ~250 GitHub calls under a 3-minute budget, §16.2). Passing
		// OwnedOrJoined=true here is NOT a claim that ownership is
		// already confirmed -- it cannot be, this early -- it is a
		// deliberately PERMISSIVE assumption for a role-only admit/reject
		// test: a role this check rejects (a viewer) would ALSO be
		// rejected by the real, ownership-confirmed check below, so this
		// can never produce a false ALLOW; a role it admits (member,
		// maintainer, admin) still goes on to the authoritative check
		// below, unchanged, once revalidation has actually confirmed
		// assignment. This preserves the documented "OwnedOrJoined is
		// only ever legitimately true once revalidation has confirmed
		// it" property for a MEMBER specifically (action.go's own
		// ActionMergePR doc comment) -- this pre-check does not weaken or
		// replace that, it only ever grants a cheap EARLY rejection to a
		// role the real check would reject anyway.
		actor := authz.Actor{UserID: actorUserID.String(), Role: authz.Role(authUser.Role)}
		if err := authz.Authorize(actor, authz.ActionMergePR, authz.Resource{OwnedOrJoined: true}); err != nil {
			if errors.Is(err, authz.ErrForbidden) {
				writeError(w, http.StatusForbidden, "not authorized to perform this action")
				return
			}
			logger.Error("httpapi: authz.Authorize failed", "error", err, "action", string(authz.ActionMergePR))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// pgx.ErrNoRows (no linked GitHub identity) is an expected, common
		// outcome -- never logged as an error, mirroring ApplySuggestion's
		// own identical "no usable git credential" 403 (reviewfindings.go).
		identity, err := deps.Identities.GetByUserAndProvider(ctx, actorUserID, sqlcgen.IdentityProviderGithub)
		if err != nil || identity.AccessTokenEncrypted == nil {
			writeError(w, http.StatusForbidden, "no usable git credential for this action")
			return
		}
		plaintextToken, err := platform.DecryptToken(deps.TokenEncryptionKey, identity.AccessTokenEncrypted)
		if err != nil {
			logger.Error("httpapi: decrypt actor's access token failed", "error", err)
			writeError(w, http.StatusForbidden, "no usable git credential for this action")
			return
		}
		// Never logged, here or anywhere it might propagate to -- mirrors
		// scmcredentials.go/reviewfindings.go's own identical discipline.
		token := string(plaintextToken)

		revalidateCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.GitHubListOpenPRsForUserTimeout)
		eligible, headSHA, reason, err := decisioninbox.RevalidateForMerge(revalidateCtx, deps, sourceControl, identity.ExternalID, req.RepoFullName, req.PrNumber, token)
		cancel()
		if err != nil {
			logger.Error("httpapi: revalidate pull request for merge failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !eligible {
			writeError(w, http.StatusConflict, reason)
			return
		}

		// RBAC -- a DEFENSIVE re-assertion, NOT where a member's own PR
		// ownership is actually enforced (second
		// round, correcting this comment: the previous text here claimed
		// this call's own OwnedOrJoined "becomes true, FOR REAL, once live
		// revalidation has itself confirmed" assignment, and called this
		// "the gate a member's own verdict genuinely depends on" -- both
		// provably false as written: Resource{OwnedOrJoined: true} is the
		// exact same hardcoded literal `true` the pre-check above already
		// passed for the SAME actor/action, and authz.Authorize is pure,
		// so this call can never produce a DIFFERENT verdict than the
		// pre-check already did moments earlier -- its own failure branch
		// is provably unreachable given that. No auth bypass results
		// (every role's verdict is still correct), but a maintainer
		// reading the previous comment could reasonably delete this call
		// believing revalidation's OWN confirmed-assignment fact flows
		// into it, which it does not.
		//
		// The REAL enforcement that this PR is genuinely assigned to THIS
		// actor already happened above, inside RevalidateForMerge: it
		// looks the PR up in sourceControl.ListOpenPRsForUser scoped to
		// the actor's OWN decrypted GitHub identity, and returns
		// eligible=false (a 409, this handler never reaches here) when
		// the PR is not in that actor's own assigned/requested-reviewer
		// list. This call is kept anyway as defense in depth for a future
		// edit that threads a REAL, per-call OwnedOrJoined value through
		// here -- do not delete it believing it is dead code with no
		// purpose; it is only ever dead given THIS file's CURRENT
		// all-literal-true design, not because the RBAC gate itself is
		// unnecessary.
		if err := authz.Authorize(actor, authz.ActionMergePR, authz.Resource{OwnedOrJoined: true}); err != nil {
			if errors.Is(err, authz.ErrForbidden) {
				writeError(w, http.StatusForbidden, "not authorized to perform this action")
				return
			}
			logger.Error("httpapi: authz.Authorize failed", "error", err, "action", string(authz.ActionMergePR))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		mergeCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.GitHubMergePRTimeout)
		mergeSHA, err := sourceControl.MergePR(mergeCtx, ports.MergePRSpec{
			Owner: owner, Repo: repo, Number: req.PrNumber, HeadSHA: headSHA, Token: token,
		})
		cancel()
		if errors.Is(err, ports.ErrShadowSuppressed) {
			// §30.7: recorded, not merged. The operator clicked Merge and
			// the platform did not merge, which is neither an error nor a
			// success and must not be presented as either -- a 500 would
			// tell them something broke, and a 200 would tell them the
			// pull request is merged when it is open.
			//
			// Its own audit action, and deliberately no RecordConfirmed:
			// this path is one of the two producers feeding the
			// contradiction-rate metric, and a confirmation the world
			// never saw would corrupt the instrument that justifies
			// arming auto-merge for real.
			if auditErr := auditlog.Record(ctx, auditLog, actorUserID, "shadow.would_have_merged", "pull_request", fmt.Sprintf("%s/%s#%d", owner, repo, req.PrNumber), map[string]any{
				"repo_full_name": owner + "/" + repo,
				"pr_number":      req.PrNumber,
				"head_sha":       headSHA,
			}); auditErr != nil {
				logger.Error("httpapi: record audit log for suppressed merge failed", "error", auditErr)
			}
			// Merged stays FALSE, which is the whole point: the response
			// says what is true. §30.6 requires a synthetic result to be
			// impossible to mistake for a real one, and "Merged: true"
			// with no SHA would be exactly that mistake.
			writeJSON(w, http.StatusOK, restdtos.MergePullRequestResponse{
				Merged:         false,
				MergeCommitSha: "",
				Message:        "Recorded, not merged: this repository's outgoing changes are suppressed on this deployment.",
			})
			return
		}
		if err != nil {
			var mergeErr *ports.MergePRError
			if errors.As(err, &mergeErr) {
				switch mergeErr.Status {
				case http.StatusMethodNotAllowed:
					writeError(w, http.StatusConflict, "GitHub reports this pull request is not currently mergeable")
				case http.StatusConflict:
					writeError(w, http.StatusConflict, "this pull request changed since it was last checked -- please retry")
				default:
					logger.Error("httpapi: merge pr failed", "error", err)
					writeError(w, http.StatusBadGateway, "merge failed")
				}
				return
			}
			logger.Error("httpapi: merge pr failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// This is the human 1-click merge-completion path §21.2 stage 2's
		// own contradiction-rate
		// metric names as ONE of its two 'confirmed' producers ("the
		// engine's own judgment stood") -- outcomes.go's own
		// RecordConfirmed doc comment, and migration 000070's own doc
		// comment, both ALREADY documented this call as existing here;
		// it did not, until this fix. Before this fix, RecordConfirmed's
		// ONLY real caller was the ARMED auto-merge worker
		// (internal/app/automerge/worker.go) -- so during the ENTIRE
		// toggle-off calibration window (the exact period this metric
		// exists to inform, §21.2: "an admin arms the auto-merge toggle
		// only once this data justifies it"), only 'overridden' rows
		// were EVER written, pinning the contradiction rate at 100% or
		// "not yet computed" for every repo that had not yet armed
		// auto-merge -- exactly the repos this metric is supposed to
		// help an admin decide FOR. Best-effort, mirroring
		// automerge.Worker.mergeCandidate's own identical placement
		// (after the merge succeeds, before the audit-log write): a
		// failure here must never claim the already-succeeded GitHub
		// merge failed.
		appreviewverdict.RecordConfirmed(ctx, deps.ReviewVerdict, req.RepoFullName, int32(req.PrNumber), headSHA)

		if err := auditlog.Record(ctx, auditLog, actorUserID, "merge_pr", "pull_request", fmt.Sprintf("%s#%d", req.RepoFullName, req.PrNumber), map[string]any{
			"repo_full_name":   req.RepoFullName,
			"pr_number":        req.PrNumber,
			"merge_commit_sha": mergeSHA,
		}); err != nil {
			// The merge already succeeded on GitHub -- a logging failure
			// here must never claim otherwise to the caller (mirrors
			// §17.5's own "the merge already happened" posture for the
			// system-initiated sentinel-fix merge's audit write).
			logger.Error("httpapi: record audit log for merge pr failed", "error", err)
		}

		writeJSON(w, http.StatusOK, restdtos.MergePullRequestResponse{
			Merged:         true,
			MergeCommitSha: mergeSHA,
			Message:        "Pull request merged",
		})
	}
}

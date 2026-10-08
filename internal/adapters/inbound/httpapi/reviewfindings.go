// This file (reviewfindings.go) implements §8.2's own ("sentinels +
// suggestions") two maintainer+-facing REST actions on a review finding
// (§12.2 item 2, §22.1):
//
//   - POST /api/sessions/{sessionID}/review/findings/{identityHash}/rebut
//     -- content-based rebuttal identity (§22.1): a maintainer+ dismisses
//     a finding by its own persisted identity hash, never by file:line.
//   - POST /api/sessions/{sessionID}/review/findings/{identityHash}/apply-suggestion
//     -- validates the finding's own SuggestedFix still applies against
//     the PR's CURRENT head before committing it, via the ACTING
//     maintainer's own OAuth token (never the original session creator's)
//     -- hard-stops on a stale/conflicting suggestion, exactly like
//     §17.4's own cherry-pick discipline, never auto-resolves. On a
//     repository whose outgoing changes are currently suppressed
//     (platform shadow mode, §30.7/§30.9), the commit is recorded rather
//     than made real -- an honest "recorded, not committed" response and
//     a dedicated fix_recorded finding status, never a false claim of
//     resolution (see ApplySuggestion's own doc comment below).
//
// Both routes are gated by authz.ActionEditReviewVerdict (§13.3 row 5,
// the SAME action reviewretrigger.go's own re-trigger button and any
// future verdict-edit surface already share -- editing/rebutting a
// finding is a species of "edit review verdict", not a new permission
// tier) and mounted behind auth.Middleware, alongside every other
// browser-facing REST route in this package.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// findingRouteContext resolves {sessionID}/{identityHash} into the values
// every handler in this file needs: the session must exist (404), the
// caller must pass authz.ActionEditReviewVerdict (403), and the session
// must be a GitHub PR review session (400) -- mirrors reviewretrigger.go's
// own identical "404 before 403" / "no PR to act on is a 400" sequencing
// exactly, for the same reasons given there.
func findingRouteContext(w http.ResponseWriter, r *http.Request, sessions *postgres.SessionStore, prSessions *postgres.GitHubPRSessionStore) (sessionID pgtype.UUID, prSession sqlcgen.GithubPrSession, identityHash string, ok bool) {
	sessionID, ok = parseSessionID(w, r)
	if !ok {
		return
	}
	ctx := platform.WithSessionID(r.Context(), sessionID.String())
	r2 := r.WithContext(ctx)
	logger := platform.Logger(ctx)

	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session not found")
			ok = false
			return
		}
		logger.Error("httpapi: get session for authorization failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		ok = false
		return
	}

	if !authorize(w, r2, authz.ActionEditReviewVerdict, authz.Resource{}) {
		ok = false
		return
	}

	prSession, err := prSessions.GetBySessionID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "this session has no associated GitHub pull request")
			ok = false
			return
		}
		logger.Error("httpapi: look up github_pr_sessions by session id failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		ok = false
		return
	}

	identityHash = chi.URLParam(r, "identityHash")
	if identityHash == "" {
		writeError(w, http.StatusBadRequest, "missing identityHash")
		ok = false
		return
	}

	return sessionID, prSession, identityHash, true
}

// findingToWire converts one sqlcgen.ReviewFinding row into its REST wire
// shape (restdtos.ReviewFinding).
func findingToWire(f sqlcgen.ReviewFinding) restdtos.ReviewFinding {
	return restdtos.ReviewFinding{
		IdentityHash: f.IdentityHash,
		SentinelKind: restdtos.ReviewFindingSentinelKind(f.SentinelKind),
		Severity:     restdtos.ReviewFindingSeverity(f.Severity),
		FilePath:     f.FilePath,
		Line:         restdtos.ReviewFindingLine(toIntPtr(f.Line)),
		Description:  f.Description,
		SuggestedFix: restdtos.ReviewFindingSuggestedFix(f.SuggestedFix),
		Status:       restdtos.ReviewFindingStatus(f.Status),
		RebuttalText: restdtos.ReviewFindingRebuttalText(f.RebuttalText),
	}
}

// toIntPtr converts a *int32 (the Postgres column shape) to a *int (the
// restdtos wire shape) -- nil stays nil.
func toIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

// RebutReviewFinding backs POST
// /api/sessions/{sessionID}/review/findings/{identityHash}/rebut (§22.1):
// 400 for a malformed/empty request body; 404 if no finding with this
// identity hash exists on this PR; 200 with the resulting
// restdtos.ReviewFinding otherwise. Writes a REAL audit_log row (actor_
// user_id set to the authenticated caller -- unlike the sentinel-auto-fix
// merge's own NULL-actor audit row, §17.5, this IS a human-attributed
// action).
func RebutReviewFinding(sessions *postgres.SessionStore, prSessions *postgres.GitHubPRSessionStore, reviewFindings *postgres.ReviewFindingStore, auditLog *postgres.AuditLogStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, prSession, identityHash, ok := findingRouteContext(w, r, sessions, prSessions)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.RebutFindingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
			return
		}

		finding, err := reviewFindings.MarkRebutted(ctx, prSession.RepoFullName, prSession.PrNumber, identityHash, req.RebuttalText, actorUserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "no finding with this identity hash on this pull request")
				return
			}
			logger.Error("httpapi: mark review finding rebutted failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := recordAuditLog(ctx, auditLog, actorUserID, "review_finding.rebut", "review_finding", identityHash, map[string]any{
			"repo_full_name": prSession.RepoFullName,
			"pr_number":      prSession.PrNumber,
			"rebuttal_text":  req.RebuttalText,
		}); err != nil {
			logger.Error("httpapi: record review_finding.rebut audit log failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, findingToWire(finding))
	}
}

// ApplySuggestion backs POST
// /api/sessions/{sessionID}/review/findings/{identityHash}/apply-suggestion
// (§12.2 item 2, §17.3): 400 for a malformed request, a finding with no
// SuggestedFix, or a finding whose own SuggestedFix no longer applies
// (reviewpost.ValidateSuggestionApplies); 404 if no such finding exists;
// 409 if this finding's own remediation is already owned by the OTHER,
// mutually-exclusive path (a sentinel-auto-fix child session already
// fix_pending/fix_open/fix_merged, or a prior fix_applied/fix_recorded) --
// §17.3: "the two remediation paths are mutually exclusive per finding";
// 409 too when the review session's head is not known to be a branch of
// the pull request's base repository (reposource.ReviewHeadInBaseRepository)
// -- a pull request from a fork, whose head this endpoint never writes to
// (§30.4), a legacy session still naming the fork, or a head not read when
// the review started -- with a message saying so; 403 if the
// acting maintainer has no usable GitHub credential; 200 with the
// resulting restdtos.ApplySuggestionResponse otherwise.
//
// # §30.7/§30.9 (resolved): a shadow-suppressed commit is recorded, not applied
//
// sourceControl.UpdateFileContent already goes through the §30.2 port
// decorator in production; on a repository whose outgoing changes are
// currently suppressed, that call returns a nil error and a self-evidently
// synthetic commit SHA (shadowscm.IsSyntheticCommitSHA), never a sentinel
// error the way a suppressed MergePR does. Marking the finding
// fix_applied on that result would be exactly the naive-suppression bug
// §30.7 names by name: the SHA exists nowhere, and §24's automatic
// re-review would re-detect the SAME defect on the unchanged real head --
// the system contradicting its own record. So this handler checks the
// result instead: response.applied is false, response.message says
// "recorded, not committed", and the finding is marked fix_recorded, a
// status re-review reconciliation (ListOpenAndRebuttedReviewFindings)
// treats as still-open -- an honest update on re-detection, never a
// contradiction.
//
// The commit this creates is attributed to the ACTING maintainer's own
// decrypted GitHub OAuth token (identities.GetByUserAndProvider on
// actorUserID) -- NEVER the original session creator's -- mirroring
// scm-credentials.go's own identical decrypt-and-use pattern, applied
// here to a DIFFERENT user (the one clicking Apply, not the one who
// created the review session).
func ApplySuggestion(sessions *postgres.SessionStore, prSessions *postgres.GitHubPRSessionStore, reviewFindings *postgres.ReviewFindingStore, identities *postgres.IdentityStore, sourceControl ports.SourceControl, tokenEncryptionKey []byte, timeouts platform.Timeouts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, prSession, identityHash, ok := findingRouteContext(w, r, sessions, prSessions)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		if sourceControl == nil {
			logger.Error("httpapi: apply-suggestion: no SourceControl configured")
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		finding, err := reviewFindings.Get(ctx, prSession.RepoFullName, prSession.PrNumber, identityHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "no finding with this identity hash on this pull request")
				return
			}
			logger.Error("httpapi: get review finding failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if finding.SuggestedFix == nil {
			writeError(w, http.StatusBadRequest, "this finding has no suggested fix to apply")
			return
		}
		if !reviewpost.FindingStatus(finding.Status).EligibleForManualApply() {
			writeError(w, http.StatusConflict, fmt.Sprintf("this finding's own status (%s) is not eligible for manual apply-suggestion -- its remediation is already owned by another path", finding.Status))
			return
		}

		// The PR's own CURRENT head branch -- github_pr_sessions has no
		// branch column of its own (it only ever maps repo/pr -> session
		// id, migrations/000028's own doc comment); the review turn's own
		// session row carries the branch it was created against
		// (sessions.repos[0].branch, set at PR-mention time from the
		// PR's real head branch, internal/adapters/inbound/github/
		// handler.go's reviewSessionRepo) -- the same source
		// reviewverdict.go's own sentinel-auto-fix trigger already reads.
		// Read before the acting maintainer's credential: a pull request
		// whose head this endpoint cannot write to is refused whoever asks.
		sessionRow, err := sessions.Get(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: get session for apply-suggestion branch resolution failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var repos []restdtos.CreateSessionRequestReposElem
		if jsonErr := json.Unmarshal(sessionRow.Repos, &repos); jsonErr != nil || len(repos) == 0 {
			logger.Error("httpapi: could not read the session's repos for apply-suggestion", "error", jsonErr)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !reposource.ReviewHeadInBaseRepository(prSession.RepoFullName, sessionRow.Repos) {
			// This endpoint commits to prSession.RepoFullName, the base
			// repository, at the spec's branch -- so only when that branch
			// is the pull request's head in the base (technical plan §30.4):
			// the spec's primary repo names the claim's repository and
			// carries a branch. A pull request from a fork keeps its head
			// in the fork, which this endpoint never writes to; its spec
			// names no branch, and neither does one whose head could not be
			// read when its review started. A legacy session opened before
			// the spec named the base, and not yet moved, still names the
			// fork and the fork's branch -- often "main", which in the base
			// is the base's own. Committing to a same-named branch of the
			// base would change a branch other than the pull request's, so
			// this is a conflict with the pull request's shape, answered as
			// one.
			logger.Info("httpapi: apply-suggestion refused: the review session names no head branch in the base repository",
				"repo", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			writeError(w, http.StatusConflict, applySuggestionNoHeadBranchMessage(prSession.RepoFullName))
			return
		}
		branch := *repos[0].Branch

		identity, err := identities.GetByUserAndProvider(ctx, actorUserID, sqlcgen.IdentityProviderGithub)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				logger.Error("httpapi: get acting maintainer's github identity failed", "error", err)
			}
			writeError(w, http.StatusForbidden, "no usable git credential for this action")
			return
		}
		if identity.AccessTokenEncrypted == nil {
			writeError(w, http.StatusForbidden, "no usable git credential for this action")
			return
		}
		plaintextToken, err := platform.DecryptToken(tokenEncryptionKey, identity.AccessTokenEncrypted)
		if err != nil {
			logger.Error("httpapi: decrypt acting maintainer's access token failed", "error", err)
			writeError(w, http.StatusForbidden, "no usable git credential for this action")
			return
		}
		// Never logged, here or anywhere it might propagate to -- mirrors
		// scmcredentials.go's own identical discipline.
		token := string(plaintextToken)

		owner, repo, ok := reposource.SplitFullName(prSession.RepoFullName)
		if !ok {
			logger.Error("httpapi: repo_full_name not in owner/repo shape", "repo_full_name", prSession.RepoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		getCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubGetPRTimeout)
		currentContent, currentSHA, exists, err := sourceControl.GetFileContent(getCtx, ports.GetFileContentSpec{
			Owner: owner, Repo: repo, Path: finding.FilePath, Ref: branch, Token: token,
		})
		cancel()
		if err != nil {
			logger.Error("httpapi: get file content for apply-suggestion failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !exists {
			writeError(w, http.StatusConflict, "the finding's own file no longer exists at the pull request's current head")
			return
		}

		if err := reviewpost.ValidateSuggestionApplies(finding.FilePath, currentContent, *finding.SuggestedFix); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}

		newContent, err := reviewpost.ApplySuggestionPatch(currentContent, *finding.SuggestedFix)
		if err != nil {
			logger.Error("httpapi: apply suggestion patch failed after passing validation", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		putCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubGetPRTimeout)
		commitSHA, err := sourceControl.UpdateFileContent(putCtx, ports.UpdateFileContentSpec{
			Owner:   owner,
			Repo:    repo,
			Path:    finding.FilePath,
			Content: newContent,
			SHA:     currentSHA,
			Branch:  branch,
			Message: fmt.Sprintf("Apply suggested fix for review finding %s", identityHash[:12]),
			Token:   token,
		})
		cancel()
		if err != nil {
			logger.Error("httpapi: commit applied suggestion failed", "error", err)
			writeError(w, http.StatusConflict, "failed to commit the suggested fix (the file may have changed concurrently)")
			return
		}

		// §30.7/§30.9 (resolved): a shadow-suppressed UpdateFileContent
		// returns a nil error and a self-evidently synthetic commit SHA
		// (shadowscm.Decorator's own doc comment), never a sentinel error
		// the way MergePR's own suppression does -- so this is the one
		// place that call's own result decides what happened, rather than
		// an errors.Is check. Naive suppression would mark this finding
		// fix_applied with a SHA that exists nowhere, and §24's automatic
		// re-review would then re-detect the SAME defect on the unchanged
		// real head -- the system contradicting itself and, worse, a
		// re-reviewing agent never even being told about it
		// (ListOpenAndRebuttedReviewFindings excludes fix_applied).
		// Instead: an honest "recorded, not committed" response, and the
		// dedicated fix_recorded status re-review reconciliation treats as
		// still-open -- see FindingStatusFixRecorded's own doc comment.
		// This is this codebase's established shape for exactly this kind
		// of honesty (mirrors ports.ErrShadowSuppressed's own "recorded,
		// not merged" -- decisioninbox.go/automerge/worker.go), applied
		// here to a write whose OWN suppressed branch never needed a
		// sentinel error in the first place.
		if shadowscm.IsSyntheticCommitSHA(commitSHA) {
			if _, err := reviewFindings.MarkFixRecorded(ctx, prSession.RepoFullName, prSession.PrNumber, identityHash); err != nil {
				logger.Error("httpapi: mark review finding fix recorded failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			writeJSON(w, http.StatusOK, restdtos.ApplySuggestionResponse{
				IdentityHash: identityHash,
				CommitSha:    commitSHA,
				Applied:      false,
				Message:      "Recorded, not committed: this repository's outgoing changes are suppressed on this deployment.",
			})
			return
		}

		if _, err := reviewFindings.MarkFixApplied(ctx, prSession.RepoFullName, prSession.PrNumber, identityHash); err != nil {
			logger.Error("httpapi: mark review finding fix applied failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, restdtos.ApplySuggestionResponse{
			IdentityHash: identityHash,
			CommitSha:    commitSHA,
			Applied:      true,
			Message:      "Suggested fix applied",
		})
	}
}

// applySuggestionNoHeadBranchMessage is the 409 ApplySuggestion answers for
// a review session whose head is not known to be a branch of
// baseRepoFullName, the pull request's base repository: a pull request from
// a fork, the common case -- a legacy session still naming the fork among
// them -- or one whose head could not be read when its review started.
func applySuggestionNoHeadBranchMessage(baseRepoFullName string) string {
	return "this pull request's head branch is not in " + baseRepoFullName +
		": a pull request from a fork keeps its head in the fork, which Narvi never writes to " +
		"(and a review that started while the pull request's head could not be read recorded none); " +
		"apply the suggestion on the pull request itself"
}

// This file (releasecompositionfindings.go) implements Step 125's own
// ("release composition findings", §15.3/§12.2 item 9) composition-
// findings-posting TOOL: POST /sessions/{sessionID}/release-manifest/
// composition-findings. This is the ONLY way the aggregate-diff
// composition review turn's own findings ever reach release_manifest_checks
// -- mirrors internal/domain/review.RenderCompositionReviewPrompt's own
// tool-instructions block, which names this exact endpoint (§5.2's
// "typed fields, never markers" discipline, applied to a composition
// finding exactly like PostReviewVerdict already applies it to a risk-map
// verdict).
//
// # Why an HTTP endpoint, not a genuine OpenCode/LLM function-call tool
//
// Structurally IDENTICAL to PostReviewVerdict (reviewverdict.go) and
// PostEpistemicOutcome (epistemicoutcome.go) -- see reviewverdict.go's own
// doc comment for the full "why an HTTP endpoint, sandbox-bearer
// authenticated, not a browser route" reasoning, which applies here
// without modification: this reuses the SAME sandbox-bearer-token/gen-
// fencing scheme scmcredentials.go/snapshotmint.go/reviewverdict.go/
// epistemicoutcome.go already establish for "agent-initiated, server-
// validated" actions, never a new one.
//
// # Why NOT PostReviewVerdict, extended
//
// §15.4 is explicit that the composition pass "stay[s] exactly the
// mechanical/compositional pass[]... with no release-level premise or
// shippable score" -- bending review_verdicts/PostReviewVerdict's own
// fixed, required-field-heavy shape (riskLevel/premise/shippable/digest,
// all mandatory) to also accept "please post this instead, with every one
// of those required fields omitted" would be exactly the kind of
// parallel-shape special-casing internal/domain/review/doc.go's own "no
// second path to any of these eight results" discipline exists to avoid.
// A second, much smaller tool, naming a much smaller JSON shape, is the
// honest fit.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// PostReleaseCompositionFindings backs POST /sessions/{sessionID}/
// release-manifest/composition-findings (note: no /api prefix -- a
// sandbox-to-CP endpoint, not a browser-facing REST route, mirroring
// review/verdict and turn/epistemic-outcome exactly). Outcome table
// (mirrors PostEpistemicOutcome's own, one shape over):
//
//  1. sessionID does not parse as a UUID, or no sandbox row exists for it
//     -> 404.
//  2. Authorization: Bearer <token> missing/malformed -> 401.
//  3. sandbox.IsDeadSandboxStatus(sandboxRow.Status) -> 410.
//  4. The presented X-Sandbox-Gen header is missing/malformed, or parses
//     but does not equal sandboxRow.Gen -> 403 (§9.3 scenario #6 parity).
//  5. The presented bearer token fails verifySandboxBearerToken -> 401.
//  6. Malformed request body (fails to decode as restdtos.
//     PostReleaseCompositionFindingsRequest -- the generated type's own
//     UnmarshalJSON already rejects a missing "findings" or an
//     out-of-enum "kind") -> 400.
//  7. This session has no release manifest check on record at all
//     (pgx.ErrNoRows) -- a meaningless call, mirrors reviewverdict.go's
//     own "no PR to act on" 400 precedent -> 400.
//  8. The guarded UPDATE (ReleaseManifestCheckStore.UpdateCompositionFindings,
//     "AND composition_reviewed_at IS NULL") affects zero rows -- findings
//     were already posted for this release (a retried/duplicate tool
//     call) -> 409.
//  9. Otherwise -> 201 with restdtos.PostReleaseCompositionFindingsResponse.
func PostReleaseCompositionFindings(sandboxes *postgres.SandboxStore, releaseManifestChecks *postgres.ReleaseManifestCheckStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		var sessionID pgtype.UUID
		if err := sessionID.Scan(chi.URLParam(r, "sessionID")); err != nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		ctx = platform.WithSessionID(ctx, sessionID.String())
		logger := platform.Logger(ctx)

		token, ok := bearerTokenFromHeader(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or malformed authorization header")
			return
		}

		sandboxRow, err := sandboxes.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: release-composition-findings: get sandbox failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if sandbox.IsDeadSandboxStatus(sandbox.State(sandboxRow.Status)) {
			writeError(w, http.StatusGone, "session stopped")
			return
		}

		presentedGen, genErr := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if genErr != nil || presentedGen != int(sandboxRow.Gen) {
			logger.Warn("httpapi: release-composition-findings: rejecting: gen mismatch",
				"presented_gen_header", r.Header.Get("X-Sandbox-Gen"), "sandbox_gen", sandboxRow.Gen)
			writeError(w, http.StatusForbidden, "no usable composition-findings-posting credential for this session")
			return
		}

		if !verifySandboxBearerToken(token, sandboxRow.TokenHash) {
			writeError(w, http.StatusUnauthorized, "invalid sandbox token")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.PostReleaseCompositionFindingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
			return
		}

		check, err := releaseManifestChecks.GetBySessionID(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusBadRequest, "this session has no release manifest check on record")
				return
			}
			logger.Error("httpapi: release-composition-findings: get release manifest check failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		findingsJSON, err := json.Marshal(req.Findings)
		if err != nil {
			// Every field of restdtos.ReleaseCompositionFinding is a plain
			// string -- json.Marshal cannot fail on this, mirroring this
			// codebase's own "cannot happen for this package's own types"
			// precedent (internal/app/releasereview/persist.go's own
			// marshalJSONArray). Fail conservative anyway.
			logger.Error("httpapi: release-composition-findings: marshal findings failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if req.Findings == nil {
			// A present, empty array persists, never a JSON null -- mirrors
			// this codebase's own "a present, empty array, never null"
			// guarantee elsewhere (internal/app/releasereview/persist.go's
			// own marshalJSONArray doc comment).
			findingsJSON = []byte("[]")
		}

		updated, err := releaseManifestChecks.UpdateCompositionFindings(ctx, check.ID, findingsJSON)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "composition findings were already posted for this release")
				return
			}
			logger.Error("httpapi: release-composition-findings: update composition findings failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusCreated, restdtos.PostReleaseCompositionFindingsResponse{
			SessionId:     sessionID.String(),
			ReviewedAt:    updated.CompositionReviewedAt.Time,
			FindingsCount: len(req.Findings),
		})
	}
}

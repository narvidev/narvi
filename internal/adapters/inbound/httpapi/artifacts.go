package httpapi

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// ListArtifacts backs GET /api/sessions/{sessionID}/artifacts (§6.3).
// Session existence is checked first -- 404 if it doesn't exist;
// otherwise every artifact row (unbounded -- expected to stay small, per
// ArtifactStore.ListForSession's own doc comment) as
// restdtos.ArtifactsResponse. Nothing in this codebase mints an artifact
// row yet (see internal/adapters/outbound/postgres/artifact_store.go's
// own doc comment) -- this endpoint's happy-path response is an empty
// array until a later Step (PR creation §9.3+, previews §8.2,
// uploads §8.6) starts producing rows.
//
// The wire map itself (id/type/url/metadata/createdAt/status/
// failureReason/filename/sizeBytes/contentType) is wshub.ArtifactWireMap,
// not a copy local to this file: this endpoint's REST shape and the
// client-WS subscribe/replay artifacts array are byte-identical by
// construction, one function with two callers, rather than two
// hand-maintained copies with nothing checking they agree. See that
// function's own doc comment (wshub/client.go) for why it, unlike its
// eventWireMap sibling here, is shared rather than duplicated.
func ListArtifacts(sessions *postgres.SessionStore, artifacts *postgres.ArtifactStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		if _, err := sessions.Get(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		rows, err := artifacts.ListForSession(ctx, sessionID)
		if err != nil {
			logger.Error("httpapi: list artifacts failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		wire := make([]restdtos.ArtifactsResponseArtifactsElem, len(rows))
		for i, a := range rows {
			wire[i] = wshub.ArtifactWireMap(a)
		}

		writeJSON(w, http.StatusOK, restdtos.ArtifactsResponse{Artifacts: wire})
	}
}

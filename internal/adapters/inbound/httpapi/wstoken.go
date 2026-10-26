package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/platform"
)

// MintWSToken backs POST /api/sessions/{sessionID}/ws-token (§6.2, §6.3),
// mounted (§13.1, "auth v1") behind internal/adapters/inbound/auth.
// Middleware -- see doc.go's own updated writeup: 404 if the session
// doesn't exist; then the session read check the client socket makes at
// subscribe and on every open socket (actorauthz.AuthorizeSessionRead,
// §6.2, §13.3) -- 403 for a "no", 500 for a check that cannot be made --
// so the mint and the socket can never disagree, and a client never mints
// a token the socket would refuse; otherwise mints a fresh plaintext token
// (platform.GenerateToken), persists ONLY its hash (platform.HashToken)
// scoped to the REAL authenticated caller (platform.UserFromContext, via
// authenticatedUserID in helpers.go) with a short
// platform.Timeouts.WSTokenTTL expiry (§6.2, amended: a client mints one
// right before each connection, and it only has to reach that
// connection's subscribe frame), and responds 200 with
// restdtos.WSTokenResponse carrying the PLAINTEXT token -- the only time
// it is ever returned, matching §6.2's "hashed at rest" requirement
// literally.
func MintWSToken(sessions *postgres.SessionStore, wsTokens *postgres.WSTokenStore, users *postgres.UserStore, timeouts platform.Timeouts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		userID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		if _, err := sessions.Get(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		switch actorauthz.AuthorizeSessionRead(ctx, logger, "httpapi", users, sessions, userID, sessionID) {
		case actorauthz.LinkedActorAllowed:
		case actorauthz.LinkedActorDenied:
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		token, err := platform.GenerateToken()
		if err != nil {
			logger.Error("httpapi: generate ws-token failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		expiresAt := time.Now().Add(timeouts.WSTokenTTL)
		if _, err := wsTokens.Create(ctx, sqlcgen.CreateWSTokenParams{
			SessionID: sessionID,
			UserID:    userID,
			TokenHash: platform.HashToken(token),
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		}); err != nil {
			logger.Error("httpapi: create ws-token failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, restdtos.WSTokenResponse{
			Token:     token,
			ExpiresAt: expiresAt,
		})
	}
}

package wshub

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// NewSandboxHandler builds the HTTP handler backing GET
// /sessions/{sessionID}/ws?type=sandbox (§6.1): "Connect: wss://…/sessions/
// {id}/ws?type=sandbox, Authorization: Bearer <sandbox_token>,
// X-Sandbox-ID (+ X-Sandbox-Gen). Server: 410 when session stopped, 403 on
// id/gen mismatch." internal/sandboxagent/wsbridge (§6.1/§7) is the
// CLIENT side of this exact same protocol; this is its server-side mirror.
//
// # Handshake status-code table (steps run in exactly this order; ALL must
// complete, and the actor MUST already be in hand, BEFORE
// websocket.Accept -- once Accept runs, the HTTP status is already
// committed as a 101 upgrade and no further status-code rejection is
// possible):
//
//  1. `type` query param != "sandbox" -> 400 (the client-hub type is
//     §6.2's own job).
//  2. `sessionID` path param does not parse as a UUID -> 404 (a malformed
//     id and a nonexistent one both mean "no such session" from the
//     caller's own perspective; wsbridge's own isFatalStatus only
//     special-cases 401/403/404/410, so a 400 here would fall through to
//     ITS infinite-backoff-retry path -- strictly worse for a
//     permanently-malformed URL than reusing one of the 4 statuses it
//     already treats as fatal).
//  3. `Authorization: Bearer <token>` missing or malformed -> 401.
//  4. `X-Sandbox-ID` missing/empty -> 401 (presence only -- nothing exists
//     yet to verify its VALUE against, matching wsbridge/doc.go's own
//     honest-gap note on this same header).
//  5. `X-Sandbox-Gen` missing or not a valid base-10 integer -> 401.
//  6. sandbox row not found (pgx.ErrNoRows) -> 404; any other lookup
//     error -> 500.
//  7. sandbox.IsDeadSandboxStatus(row.Status) -> 410.
//  8. parsed gen != row.Gen -> 403 (§9.3 scenario #6: "Stale sandbox from
//     old gen reconnects -> rejected 403, logged, session unaffected").
//  9. token verification fails -> 401.
//  10. registry.GetOrSpawn: ErrSessionActorElsewhere -> 503 (a deliberate,
//     honest stopgap: this process does not own this session's actor and
//     no cross-pod routing/proxy exists anywhere in the codebase yet, a
//     genuine, separate, pre-existing gap, not something solved here; 503
//     is NOT one of wsbridge's own 4 "fatal" statuses, so its own
//     already-built exponential-backoff reconnect loop is exactly the
//     right fallback -- "try again, maybe once this pod frees the lock or
//     another pod picks it up"); ErrActorUnavailable -> 503 with
//     Retry-After set to ActorHydrateTimeout in whole seconds, at least
//     one (this replica could not hydrate the actor within that bound --
//     its query pool stayed saturated, or its lock connection is down --
//     and has released any lock it took, so a reconnect after the pause
//     may land here or elsewhere); any other GetOrSpawn error -> 500.
//  11. websocket.Accept, then the read/dispatch loop (dispatch.go) runs
//     until conn.Read errors or ctx is done.
func NewSandboxHandler(registry *sessionactor.Registry, sandboxes *postgres.SandboxStore, commander *SandboxRegistry, timeouts platform.Timeouts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// (1) ?type=sandbox -- the client-hub type is §6.2's own job,
		// not yet implemented; this route only serves the sandbox half.
		if r.URL.Query().Get("type") != "sandbox" {
			http.Error(w, "unsupported or missing ws type", http.StatusBadRequest)
			return
		}

		// (2) sessionID path param -> pgtype.UUID. A Scan failure
		// (malformed UUID text) is treated identically to "no such
		// session" (404) -- see this func's own doc comment for why.
		rawSessionID := chi.URLParam(r, "sessionID")
		var sessionID pgtype.UUID
		if err := sessionID.Scan(rawSessionID); err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		// (3) Authorization: Bearer <token>
		token, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing or malformed authorization header", http.StatusUnauthorized)
			return
		}

		// (4) X-Sandbox-ID: presence only (honest gap, see doc comment).
		if r.Header.Get("X-Sandbox-ID") == "" {
			http.Error(w, "missing X-Sandbox-ID", http.StatusUnauthorized)
			return
		}

		// (5) X-Sandbox-Gen: base-10 integer.
		gen, err := strconv.Atoi(r.Header.Get("X-Sandbox-Gen"))
		if err != nil {
			http.Error(w, "missing or malformed X-Sandbox-Gen", http.StatusUnauthorized)
			return
		}

		// The handshake now knows sessionID/gen -- populate both on the
		// request context (platform/correlation.go's own PR-03-built, so-
		// far-uncalled accessors; this Step is their first real caller) so
		// platform.Logger(ctx) carries session_id/sandbox_gen on every log
		// line for the rest of this connection's lifetime, including
		// through GetOrSpawn below and the read loop.
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		ctx = platform.WithSandboxGen(ctx, int64(gen))
		logger := platform.Logger(ctx)

		// (6) sandbox row lookup.
		row, err := sandboxes.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			logger.Error("wshub: get sandbox failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// (7) dead sandbox statuses (Stopped/Stale/Failed) -> 410. A
		// Suspect sandbox is deliberately NOT dead (IsDeadSandboxStatus is
		// a deny-list) -- it must still be allowed to reconnect.
		if sandbox.IsDeadSandboxStatus(sandbox.State(row.Status)) {
			http.Error(w, "session stopped", http.StatusGone)
			return
		}

		// (8) gen mismatch -> 403 (§9.3 scenario #6).
		if gen != int(row.Gen) {
			logger.Warn("wshub: rejecting sandbox ws connect: gen mismatch",
				"presented_gen", gen, "sandbox_gen", row.Gen)
			http.Error(w, "gen mismatch", http.StatusForbidden)
			return
		}

		// (9) token verification.
		if !verifySandboxToken(token, row.TokenHash) {
			http.Error(w, "invalid sandbox token", http.StatusUnauthorized)
			return
		}

		// (10) The actor MUST be in hand BEFORE Accept (step 11) -- see
		// this func's own doc comment for why the ordering matters.
		actor, err := registry.GetOrSpawn(ctx, sessionID)
		if err != nil {
			if errors.Is(err, sessionactor.ErrSessionActorElsewhere) {
				logger.Warn("wshub: session actor owned elsewhere; no cross-pod routing built yet", "error", err)
				http.Error(w, "session actor owned elsewhere", http.StatusServiceUnavailable)
				return
			}
			if errors.Is(err, sessionactor.ErrActorUnavailable) {
				logger.Warn("wshub: session actor unavailable on this replica; asking the sandbox to reconnect later", "error", err)
				setRetryAfter(w, timeouts.ActorHydrateTimeout)
				http.Error(w, "session actor unavailable", http.StatusServiceUnavailable)
				return
			}
			logger.Error("wshub: GetOrSpawn failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// (11) Accept. InsecureSkipVerify is deliberate: this is a
		// non-browser, server-to-server, bearer-token-authenticated
		// connection -- coder/websocket's Origin-header CSRF-style check
		// exists to protect browser-facing endpoints from malicious pages,
		// which does not apply here.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			logger.Error("wshub: websocket accept failed", "error", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()

		logger.Info("wshub: sandbox ws connected")

		// Register this connection as sessionID's own live sandbox
		// connection BEFORE entering readLoop, so a SandboxCommander.
		// SendCommand call from app/sessionactor can reach it the instant
		// the handshake completes (§9.3, "e2e happy path", design
		// decision 4). unregister runs on this same goroutine's own
		// return, guaranteeing the registry never holds a dead connection
		// past this handler's own lifetime.
		unregister := commander.Register(sessionID.String(), conn)
		defer unregister()

		readLoop(ctx, conn, actor, sessionID.String(), gen, timeouts)
	}
}

// bearerToken extracts the bearer token from r's Authorization header,
// reporting ok=false if the header is missing, or present but not exactly
// "Bearer " followed by a non-empty remainder.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}

// setRetryAfter sets Retry-After to retryAfter in whole seconds, rounded
// up, and never below one -- mcpauth's own setRetryAfter, for the sandbox
// handshake's 503 (step 10 above).
func setRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// This file (client.go) implements the CLIENT half of the WS hub (§6.2):
// the browser <-> control-plane protocol, as opposed to sandbox.go's
// server-side mirror of the sandbox-agent <-> control-plane protocol
// (§6.1). It shares this package's route
// (GET /sessions/{sessionID}/ws) with the sandbox socket, discriminated by
// ?type=client vs ?type=sandbox via handler.go's own NewHandler
// dispatcher -- see that file's own doc comment.
//
// # Client-handshake outcome table
//
//  1. sessionID path param does not parse as a UUID -> 404 (mirrors the
//     sandbox handler's own established convention exactly -- see
//     sandbox.go's own doc comment for why a malformed id and a
//     nonexistent one are treated identically; §6.2 does not itself
//     mandate a pre-upgrade status code here, this is a deliberate,
//     reasoned UX choice in the same style).
//  2. session row not found (pgx.ErrNoRows) -> 404; any other lookup
//     error -> 500.
//  3. websocket.Accept(w, r, &websocket.AcceptOptions{}) -- deliberately
//     NO InsecureSkipVerify, unlike sandbox.go's own Accept call. The
//     sandbox socket is a non-browser, server-to-server, bearer-
//     authenticated connection, so disabling coder/websocket's Origin-
//     header same-origin check there is correct (see sandbox.go's own
//     comment). THIS socket is browser-facing: a malicious webpage
//     attempting a cross-origin WS connection using a victim's browser
//     session is exactly the CSRF-class attack that same-origin
//     enforcement exists to stop, and narvi serves API+WS+UI on one
//     origin (§12.1: "one binary... on one port"), so legitimate
//     same-origin browser connections are unaffected by leaving the
//     default enforcement on. Do not copy sandbox.go's InsecureSkipVerify
//     here.
//  4. The first inbound message, bounded by
//     platform.Timeouts.ClientSubscribeTimeout (§6.2: "within 30s"):
//     missing (deadline elapses first), a read error, or malformed JSON
//     -> conn.Close(4001, "re-auth required").
//  5. The parsed clientws.SubscribeRequest's token: looked up via
//     wsTokens.GetByHash(platform.HashToken(token)). Not found, or found
//     for a DIFFERENT session than the URL's own sessionID -> close 4001
//     (both are treated as "not a valid credential for THIS session",
//     never leaking which case it was). Found, for this session, but
//     expired (expires_at before now) -> close 4002 ("token expired").
//  6. Otherwise: insert the connection into the shared *Hub's session-keyed
//     map FIRST (Hub.Register), THEN assemble and write the single
//     `subscribed` reply (SubscribedPayload), and only AFTER that write
//     returns start actually delivering queued/future broadcasts to the
//     wire (Hub.StartDelivery) -- three separate steps, deliberately in
//     that order, not any other.
//
//     The first ordering constraint (registration before the reply) fixes
//     the original lost-broadcast bug: registering after the reply would
//     leave a real window, between the client observing "subscribed" and
//     this goroutine actually completing Hub.Register, during which any
//     broadcast for this session is silently and permanently lost for this
//     connection (Hub.Broadcast never replays -- a connection not yet
//     registered simply isn't there to receive it). That window is
//     normally too narrow to matter on an unloaded machine, but widens
//     under real scheduling contention -- confirmed in CI
//     (TestClientHandler_SlowConnectionDoesNotBlockOthers failed there with
//     0/50 broadcasts received, never locally reproduced). Registering
//     first makes "client observed the subscribed reply" unconditionally
//     imply "already registered", closing the race outright rather than
//     merely narrowing it.
//
//     The second ordering constraint (delaying StartDelivery until after
//     the subscribed reply's own write returns) fixes a narrower, inverse
//     problem that constraint one alone introduced (F8 audit finding):
//     Hub.Register's map-insertion and the drain goroutine that actually
//     writes queued broadcasts to conn used to start together, in one
//     call. buildSubscribedPayload runs several sequential DB reads before
//     this handler's own goroutine writes the subscribed reply; if the
//     drain goroutine is already running during that window, a broadcast
//     enqueued and drained during it can reach conn.Write before this
//     handler's own subscribed-reply conn.Write does, so the client can
//     observe a live event before its baseline -- inverting §6.2's "single
//     subscribed payload -> broadcast stream". coder/websocket serializes
//     concurrent Conn.Write calls internally (no data race, no wire
//     corruption), so this was purely a wire-ORDER bug. Splitting
//     registration (Hub.Register: map insertion only, so a broadcast fired
//     anytime after it is still never lost -- just queued, buffered,
//     unread, in order) from starting delivery (Hub.StartDelivery: the
//     drain goroutine) lets this handler defer that second step until
//     after its own subscribed-reply write has already returned, making
//     "drain start happens-after subscribed write returns" a plain
//     program-order guarantee within this one goroutine, not a race
//     against the drain goroutine. See Hub.Register's and
//     Hub.StartDelivery's own doc comments for the full mechanics.
//
//     Then: start the idle-liveness ping loop (see pingClientLoop's own
//     doc comment -- an unanswered ping closes 4003 "idle timeout"), then
//     run the post-subscribe read loop (fetch_history, rate-limited
//     per-connection via platform.Timeouts.ClientFetchHistoryMinInterval)
//     until the connection errs or closes.
//
// No registry.GetOrSpawn call belongs anywhere in this handshake: every
// step above is a plain store read (session/ws-token/turns/sandbox/
// events/artifacts), matching the sandbox handler's own precedent that
// the actor need not exist just to satisfy a read. The session's Actor
// hydrates lazily whenever something ELSE (a sandbox event, a timer)
// first needs it; a browser subscribing is not that trigger.

package wshub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// subscribeDBReadHook, when non-nil, is invoked once by NewClientHandler's
// handshake immediately after step (6) (Hub.Register: the connection's
// channel is already in the map) and before step (7) (buildSubscribedPayload's
// own sequential DB reads) begins. It exists ONLY so
// TestClientHandler_BroadcastDuringSubscribeWindowArrivesAfterSubscribed
// (client_test.go) can deterministically fire a broadcast into exactly that
// window -- the window the F8 audit finding is about -- instead of relying
// on a sleep or an unreliable scheduling race to land a broadcast there.
// Always nil in production; this handler's own normal path never sets or
// reads it for anything other than this single nil check. An
// atomic.Pointer, not a plain var: the test goroutine sets it (via
// SetSubscribeDBReadHookForTest below) before dialing, and the handler
// goroutine (spawned independently by net/http/httptest, with no
// happens-before edge to the test's own prior statements otherwise) reads
// it -- a plain var here would be a genuine, `go test -race`-detectable
// data race between those two goroutines.
var subscribeDBReadHook atomic.Pointer[func()]

// SetSubscribeDBReadHookForTest installs (or, given nil, clears) the
// package-level subscribeDBReadHook seam documented above. Exported
// SOLELY so client_test.go (package wshub_test, a separate, external test
// package with no access to this package's unexported identifiers) can
// deterministically synchronize a broadcast into the exact window between
// Hub.Register and buildSubscribedPayload's own DB reads. Not part of this
// package's real API -- never call this from production code.
func SetSubscribeDBReadHookForTest(hook func()) {
	if hook == nil {
		subscribeDBReadHook.Store(nil)
		return
	}
	subscribeDBReadHook.Store(&hook)
}

// initialReplayLimit bounds how many of a session's oldest events are
// included directly in the SubscribedPayload's own "events" field at
// subscribe time (§6.2's own design-decision note: "older history is
// available via fetch_history" -- this is a fixed, documented limit, not
// an attempt to replay a session's full history unboundedly). A plain
// count, not a duration, so (like sessionactor's own mailboxBufferSize) it
// is an ordinary Go constant, not a platform.Timeouts field.
const initialReplayLimit = 200

// maxInitialReplayBytes bounds the TOTAL marshaled size of the events
// included in one SubscribedPayload, independent of initialReplayLimit's
// own item-count cap -- item count alone is not enough: coder/websocket's
// own default per-message read limit is 32768 bytes (Conn.SetReadLimit's
// own doc), and this Step's own protocol negotiates no larger limit on
// either side, so a session containing initialReplayLimit events with
// large individual payloads (e.g. sizeable tool_call/tool_result data) can
// produce a subscribed reply that exceeds a stock client's own default and
// kills the ENTIRE handshake with ErrMessageTooBig, not just truncate the
// replay. Chosen conservatively below that 32KiB wire default, leaving
// headroom for the rest of the payload (state/artifacts/sessionId/
// participants). At least one event is always kept even if it alone
// exceeds this budget -- truncating to zero would silently hide real
// content while providing no size guarantee anyway, and a client that
// needs the rest of a large session's history already has fetch_history.
const maxInitialReplayBytes = 16 * 1024

// fetchHistoryDefaultLimit / fetchHistoryMaxLimit bound a fetch_history
// request's own page size: the client's requested Limit is honored up to
// fetchHistoryMaxLimit; a nil/absent Limit uses fetchHistoryDefaultLimit
// (FetchHistoryRequest.Limit's own doc: "Null means use the server
// default page size"). Plain counts, not durations.
const (
	fetchHistoryDefaultLimit = 100
	fetchHistoryMaxLimit     = 500
)

// NewClientHandler builds the HTTP handler backing GET
// /sessions/{sessionID}/ws?type=client (§6.2) -- this file's own top
// comment has the full handshake outcome table.
//
// registry is accepted for signature symmetry with NewSandboxHandler and
// for a later Step's likely need to route a client-originated command
// through the session's own Actor -- this Step's own handshake is
// entirely read-only (see this file's top comment) and does not call
// GetOrSpawn.
func NewClientHandler(
	registry *sessionactor.Registry,
	sessions *postgres.SessionStore,
	turns *postgres.TurnStore,
	sandboxes *postgres.SandboxStore,
	events *postgres.EventStore,
	artifacts *postgres.ArtifactStore,
	wsTokens *postgres.WSTokenStore,
	users *postgres.UserStore,
	hub *Hub,
	timeouts platform.Timeouts,
) http.HandlerFunc {
	_ = registry // see doc comment above: not used by this Step's own read-only handshake.

	return func(w http.ResponseWriter, r *http.Request) {
		// (1) sessionID path param -> pgtype.UUID.
		rawSessionID := chi.URLParam(r, "sessionID")
		var sessionID pgtype.UUID
		if err := sessionID.Scan(rawSessionID); err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		// (2) session row lookup.
		sessionRow, err := sessions.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			logger.Error("wshub: get session failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// (3) Accept -- deliberately NO InsecureSkipVerify; see this
		// file's own top comment for the full contrast with sandbox.go.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			logger.Error("wshub: client websocket accept failed", "error", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()

		// (4) the first inbound message, bounded by ClientSubscribeTimeout.
		req, ok := readSubscribeRequest(ctx, conn, timeouts, logger)
		if !ok {
			return // readSubscribeRequest already closed the connection (4001).
		}

		// (5) verify the presented ws-token.
		userID, ok := verifyClientToken(ctx, conn, wsTokens, sessionID, req.Token, logger)
		if !ok {
			return // verifyClientToken already closed the connection (4001/4002).
		}

		// (6) insert conn into the shared Hub's map BEFORE the client can
		// ever observe the "subscribed" reply below -- see this file's own
		// top comment for why this ordering, not the reverse, is required.
		// This step alone (Hub.Register) already makes every broadcast from
		// this instant on unloseable (queued, buffered, in order) -- it
		// deliberately does NOT yet start writing those broadcasts to conn;
		// that is step (7b) below, delayed on purpose (F8 audit fix; see
		// Hub.StartDelivery's own doc comment). The writer goroutine (7b)
		// eventually starts is scoped to writerCtx/writerGroup, both torn
		// down (canceled + joined) before this handler returns (including
		// on every early-return path below), so no goroutine outlives the
		// connection it serves.
		//
		// registerUserID is "" for a connection whose ws_tokens row carries
		// no user (userID.Valid false -- a deleted minting user,
		// migrations/000016's own nullable-user_id doc comment): Hub.
		// Register/Participants treat an empty string as "not a presence
		// participant", never as a real, anonymous user id.
		var registerUserID string
		if userID.Valid {
			registerUserID = userID.String()
		}
		writerCtx, cancelWriter := context.WithCancel(ctx)
		defer cancelWriter()
		var writerGroup errgroup.Group
		ch, unregister := hub.Register(sessionID.String(), conn, registerUserID)
		defer func() {
			unregister()
			cancelWriter()
			_ = writerGroup.Wait()
		}()

		// Test-only synchronization seam: see subscribeDBReadHook's own doc
		// comment. Always nil in production.
		if hook := subscribeDBReadHook.Load(); hook != nil {
			(*hook)()
		}

		// (7) assemble + write the single `subscribed` reply. Participants
		// is resolved from the Hub's own live, in-process connection
		// registry (§8's "multiplayer presence") -- the distinct set of
		// real users currently subscribed to this session, INCLUDING the
		// connection just registered above -- never from the dormant
		// `participants` Postgres table (§13.3's separate "joined"
		// authorization concept; see this package's own doc.go).
		participants := resolveParticipants(ctx, users, hub.Participants(sessionID.String()), logger)
		payload, err := buildSubscribedPayload(ctx, sessionID, sessionRow, turns, sandboxes, events, artifacts, participants)
		if err != nil {
			logger.Error("wshub: assembling subscribed payload failed", "error", err)
			_ = conn.Close(websocket.StatusInternalError, "internal error")
			return
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			logger.Error("wshub: marshal subscribed payload failed", "error", err)
			_ = conn.Close(websocket.StatusInternalError, "internal error")
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			logger.Warn("wshub: write subscribed payload failed", "error", err)
			return
		}

		logger.Info("wshub: client ws subscribed", "client_id", req.ClientId)

		// (7b) ONLY NOW -- after the subscribed reply's own conn.Write above
		// has already returned -- start actually delivering queued/future
		// broadcasts to the wire (F8 audit fix). Any broadcast fired between
		// step (6) and this line is still sitting, unlost, in ch (buffered by
		// Hub.Register); StartDelivery's drain goroutine will write it right
		// after this call, i.e. strictly after the subscribed reply, never
		// before. See Hub.StartDelivery's own doc comment for why this
		// ordering is a plain program-order guarantee, not a race.
		hub.StartDelivery(writerCtx, &writerGroup, conn, ch)

		// Idle-liveness check (audit-remediation, inbound-hygiene lens): a
		// periodic server-initiated ping, run on the SAME writerGroup/
		// writerCtx as the Hub's own writer goroutine above -- both torn
		// down together by this handler's own deferred cleanup. See
		// pingClientLoop's own doc comment.
		writerGroup.Go(func() error {
			pingClientLoop(writerCtx, conn, timeouts.ClientWSPingInterval, logger)
			return nil
		})

		// (8) post-subscribe read loop (fetch_history).
		readClientLoop(ctx, conn, sessionID, events, timeouts, logger)
	}
}

// readSubscribeRequest reads and parses the first inbound frame, bounded
// by timeouts.ClientSubscribeTimeout (§6.2: "within 30s"). The blocking
// conn.Read call runs on its own goroutine (via errgroup.Group.Go -- §11,
// no naked goroutines) so this function can race it against a plain
// time.After timer instead of handing conn.Read itself a context deadline
// -- coder/websocket arms its OWN deadline-driven close (via
// context.AfterFunc) the instant a context passed to Read carries a
// deadline, which would force ITS OWN close code/reason instead of the
// 4001 this function needs to send deliberately (mirrors dispatch_test.
// go's own startReader precedent and its documented reasoning for the
// exact same coder/websocket quirk). ok is false in every case this
// function has already closed conn itself (4001) -- the caller must
// simply return.
func readSubscribeRequest(ctx context.Context, conn *websocket.Conn, timeouts platform.Timeouts, logger *slog.Logger) (clientws.SubscribeRequest, bool) {
	type frame struct {
		data []byte
		err  error
	}
	frameCh := make(chan frame, 1)

	var eg errgroup.Group
	eg.Go(func() error {
		_, data, err := conn.Read(ctx)
		frameCh <- frame{data: data, err: err}
		return nil
	})

	var req clientws.SubscribeRequest

	select {
	case f := <-frameCh:
		if f.err != nil {
			logger.Warn("wshub: client subscribe: read error before a subscribe frame arrived; closing 4001", "error", f.err)
			_ = conn.Close(websocket.StatusCode(4001), "re-auth required")
			_ = eg.Wait()
			return req, false
		}
		if err := json.Unmarshal(f.data, &req); err != nil {
			logger.Warn("wshub: client subscribe: malformed subscribe frame; closing 4001", "error", err)
			_ = conn.Close(websocket.StatusCode(4001), "re-auth required")
			_ = eg.Wait()
			return req, false
		}
		_ = eg.Wait()
		return req, true

	case <-time.After(timeouts.ClientSubscribeTimeout):
		logger.Warn("wshub: client subscribe: no subscribe frame within timeout; closing 4001")
		_ = conn.Close(websocket.StatusCode(4001), "re-auth required")
		_ = eg.Wait() // conn is now closed; the blocked Read above returns an error and the goroutine exits.
		return req, false
	}
}

// verifyClientToken looks up token's hash and, per this file's own
// handshake outcome table: closes 4001 if no matching row exists or the
// row belongs to a different session than sessionID (both cases are
// deliberately indistinguishable to the caller -- never leak which);
// closes 4002 if the row is a genuine match but already expired;
// otherwise returns (userID, true) with the connection untouched. A
// genuine backend error during the lookup (e.g. a transient Postgres
// blip) is NOT folded into 4001 -- telling every subscribing client its
// credential is bad during an unrelated infrastructure hiccup would be
// actively misleading -- it closes with StatusInternalError instead,
// matching how the rest of this file's own error paths (e.g.
// buildSubscribedPayload's) already distinguish "not found" from "we
// failed to check".
//
// userID is row.UserID verbatim -- ws_tokens.user_id is nullable (SET
// NULL if the minting user is later deleted, migrations/000016's own doc
// comment), so a caller must check userID.Valid before treating this
// connection as belonging to a real, presence-worthy participant (§8's
// own "multiplayer presence" -- Hub.Register, below, is this method's
// one real caller that does exactly that).
func verifyClientToken(ctx context.Context, conn *websocket.Conn, wsTokens *postgres.WSTokenStore, sessionID pgtype.UUID, token string, logger *slog.Logger) (userID pgtype.UUID, ok bool) {
	row, err := wsTokens.GetByHash(ctx, platform.HashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("wshub: client subscribe: unknown ws token; closing 4001")
			_ = conn.Close(websocket.StatusCode(4001), "re-auth required")
			return pgtype.UUID{}, false
		}
		logger.Error("wshub: ws-token lookup failed", "error", err)
		_ = conn.Close(websocket.StatusInternalError, "internal error")
		return pgtype.UUID{}, false
	}
	if row.SessionID != sessionID {
		logger.Warn("wshub: client subscribe: ws token belongs to a different session; closing 4001")
		_ = conn.Close(websocket.StatusCode(4001), "re-auth required")
		return pgtype.UUID{}, false
	}
	if time.Now().After(row.ExpiresAt.Time) {
		logger.Warn("wshub: client subscribe: expired ws token; closing 4002")
		_ = conn.Close(websocket.StatusCode(4002), "token expired")
		return pgtype.UUID{}, false
	}
	return row.UserID, true
}

// buildSubscribedPayload assembles the single reply to subscribe (§6.2):
// a genuinely minimal, honestly-scoped state map (session + turns +
// sandbox-or-nil -- the full read-model shape is explicitly left to later
// PRs by the schema's own doc comment), the first initialReplayLimit
// events (EventsTruncated reports, exactly, whether that replay is the
// session's complete history or a cut prefix of it -- see the field's own
// schema doc comment), every artifact (unbounded -- expected to stay
// small), and participants (§8's "multiplayer presence") -- resolved by
// the caller (resolveParticipants, this file) from the Hub's own live
// connection registry, never computed here.
func buildSubscribedPayload(
	ctx context.Context,
	sessionID pgtype.UUID,
	sessionRow sqlcgen.Session,
	turns *postgres.TurnStore,
	sandboxes *postgres.SandboxStore,
	events *postgres.EventStore,
	artifacts *postgres.ArtifactStore,
	participants []clientws.SubscribedPayloadParticipantsElem,
) (clientws.SubscribedPayload, error) {
	turnRows, err := turns.ListForSession(ctx, sessionID)
	if err != nil {
		return clientws.SubscribedPayload{}, err
	}
	if turnRows == nil {
		turnRows = []sqlcgen.Turn{}
	}

	var sandboxState any
	sandboxRow, err := sandboxes.Get(ctx, sessionID)
	switch {
	case err == nil:
		sandboxState = sandboxWireMap(sandboxRow)
	case errors.Is(err, pgx.ErrNoRows):
		sandboxState = nil
	default:
		return clientws.SubscribedPayload{}, err
	}

	// Fetched one row past initialReplayLimit, deliberately: getting
	// initialReplayLimit+1 rows back is an exact, cheap ("+1" cost, no
	// second query) proof that more history exists beyond what this
	// payload can carry, unlike the len(rows)==limit heuristic this
	// file's own fetch_history handler (below) and httpapi's own
	// ListEvents (events.go) each use for their nextCursor -- a
	// heuristic tolerable there because a false "there might be more"
	// only costs a client one empty extra page, but wrong here would
	// mean EventsTruncated itself lies.
	eventRows, err := events.ListForSession(ctx, sessionID, 0, initialReplayLimit+1)
	if err != nil {
		return clientws.SubscribedPayload{}, err
	}
	eventRows, countTruncated := trimToReplayLimit(eventRows)

	artifactRows, err := artifacts.ListForSession(ctx, sessionID)
	if err != nil {
		return clientws.SubscribedPayload{}, err
	}

	wireEventsAll := subscribedEventsWire(eventRows)
	wireEvents := truncateEventsToByteBudget(wireEventsAll, maxInitialReplayBytes)
	byteTruncated := len(wireEvents) < len(wireEventsAll)

	return clientws.SubscribedPayload{
		SessionId: sessionID.String(),
		State: clientws.SubscribedPayloadState{
			"session": sessionRow,
			"turns":   turnRows,
			"sandbox": sandboxState,
			// correlationId (§12.2 item 1's own session-rail gap): the
			// SESSION's own most-recently-created turn's correlation id
			// (turnRows is ORDER BY created_at ASC, ListTurnsForSession's own
			// doc comment -- the last element is the newest), or nil when
			// this session has no turns yet, or its newest turn's own
			// correlation_id is itself nil (created with no live request
			// context, latestTurnCorrelationID's own doc comment). Deliberately
			// a TOP-LEVEL state key, siblings with sandbox rather than nested
			// inside it: a correlation id is a property of a REQUEST (this
			// session's latest one), never of the sandbox itself -- see
			// sandboxWireMap's own doc comment for the contrasting case
			// (agentVersion/imageDigest, which ARE sandbox properties).
			"correlationId": latestTurnCorrelationID(turnRows),
		},
		Events:          wireEvents,
		EventsTruncated: countTruncated || byteTruncated,
		Artifacts:       subscribedArtifactsWire(artifactRows),
		Participants:    participants,
	}, nil
}

// resolveParticipants resolves userIDs (Hub.Participants' own distinct,
// currently-connected set for this session -- §8's "multiplayer
// presence") into the wire shape's loosely-typed participant maps,
// carrying exactly what the mockup's presence indicator needs to render
// an avatar + display name: {"userId", "displayName"}. A userID that no
// longer resolves to a real row (UserStore.GetByID errors -- the user was
// deleted moments after this connection registered, or any other lookup
// failure) is DROPPED, logged, never fabricated -- one stale/unresolvable
// participant must not fail the whole subscribe reply, mirroring this
// file's own established "a live GitHub degrade never blocks the
// response" posture elsewhere in this package's sibling handlers. Always
// returns a non-nil slice (nil and empty are not meaningfully different
// on the wire, but SubscribedPayload.participants is schema-required, and
// this package's own established convention -- see turnRows immediately
// above -- is never to hand json.Marshal a nil slice for a required
// array field).
func resolveParticipants(ctx context.Context, users *postgres.UserStore, userIDs []string, logger *slog.Logger) []clientws.SubscribedPayloadParticipantsElem {
	participants := make([]clientws.SubscribedPayloadParticipantsElem, 0, len(userIDs))
	for _, id := range userIDs {
		var uuid pgtype.UUID
		if err := uuid.Scan(id); err != nil {
			logger.Warn("wshub: participant user id did not parse as a uuid (bug); omitting", "user_id", id, "error", err)
			continue
		}
		user, err := users.GetByID(ctx, uuid)
		if err != nil {
			logger.Warn("wshub: resolve participant user failed; omitting from presence", "user_id", id, "error", err)
			continue
		}
		participants = append(participants, clientws.SubscribedPayloadParticipantsElem{
			"userId":      user.ID.String(),
			"displayName": user.DisplayName,
		})
	}
	return participants
}

// trimToReplayLimit caps fetched (the result of requesting
// initialReplayLimit+1 rows, buildSubscribedPayload's own caller) at
// initialReplayLimit, oldest-first order preserved, and reports whether
// there was a limit+1'th row to cut -- the exact, cheap proof that more
// history exists beyond initialReplayLimit, since fetched can only ever
// hold limit+1 elements if a genuine limit+1'th row existed to fetch. A
// pure slice op (no I/O, no time, no randomness -- safe and fast to unit
// test directly, unlike the wire-shaped byte-budget check below, which
// needs realistic marshaled sizes to mean anything).
func trimToReplayLimit(fetched []sqlcgen.Event) (rows []sqlcgen.Event, truncated bool) {
	if len(fetched) > initialReplayLimit {
		return fetched[:initialReplayLimit], true
	}
	return fetched, false
}

// truncateEventsToByteBudget returns the longest PREFIX of wire (order
// preserved) whose total marshaled JSON size stays within maxBytes, always
// keeping at least the first element regardless of its own size (see
// maxInitialReplayBytes's own doc comment for why). A marshal failure on
// any one element is treated as zero-size for this size-accounting pass
// only -- the same element will surface that identical failure (fatally,
// correctly) when the whole SubscribedPayload is marshaled by this
// function's own caller, so silently skipping it HERE does not hide a
// real error, only defers where it is reported.
func truncateEventsToByteBudget(wire []clientws.SubscribedPayloadEventsElem, maxBytes int) []clientws.SubscribedPayloadEventsElem {
	if len(wire) == 0 {
		return wire
	}
	total := 0
	for i, ev := range wire {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		total += len(b)
		if total > maxBytes && i > 0 {
			return wire[:i]
		}
	}
	return wire
}

// eventWireMap builds this package's own deliberately minimal wire shape
// for one stored event row: id (for client-side de-dup against live
// broadcasts), type (needed to interpret payload -- unlike a live
// broadcast frame, a synthetic actor-appended event's own raw payload
// does not always embed its own type, see sessionactor's own appendEvent),
// payload (embedded as real JSON via json.RawMessage, never
// base64-encoded -- sqlcgen.Event.Payload is a plain []byte, which
// encoding/json would otherwise base64-encode by default), and createdAt.
// clientws's own protocol.schema.json declares these array elements
// additionalProperties:true -- this shape is this package's own choice,
// not a wire-contract requirement -- shared by the subscribe-time replay
// and every fetch_history reply so a client never needs two parsing paths
// for "replayed" vs "paginated" history.
func eventWireMap(e sqlcgen.Event) map[string]interface{} {
	return map[string]interface{}{
		"id":        e.ID,
		"type":      e.Type,
		"payload":   json.RawMessage(e.Payload),
		"createdAt": e.CreatedAt,
	}
}

// ArtifactWireMap is eventWireMap's own artifact counterpart --
// sqlcgen.Artifact.Metadata is likewise a plain []byte needing the same
// json.RawMessage treatment to avoid base64-encoding.
//
// Exported (unlike eventWireMap, httpapi/events.go's own sibling
// function) so httpapi's REST twin (artifacts.go's ListArtifacts) can
// call this exact function instead of maintaining a byte-identical
// second copy that nothing enforced stayed in sync -- the same
// package-boundary precedent HashSandboxToken (token.go) already sets
// for httpapi's reuse of a small wshub-owned helper. eventWireMap's own
// doc comment (httpapi/events.go) records a separately reasoned choice
// for that sibling function; this decision does not revisit it.
func ArtifactWireMap(a sqlcgen.Artifact) map[string]interface{} {
	// status/failureReason (§28.6) are additive fields on the
	// wire SubscribedPayload.artifacts shape, mirroring the sandbox-ws
	// artifact event's own identical additive change -- always present
	// here (never omitted), unlike that WS event's own absent-means-ready
	// convention, since this map has no existing zero-producer callers to
	// preserve byte-for-byte: every row already has a real status by
	// migration 000060's own DEFAULT 'ready' backfill.
	var failureReason interface{}
	if a.FailureReason != nil {
		failureReason = *a.FailureReason
	}
	// filename/sizeBytes/contentType (§12.2 item 1's own rail): present
	// for both this function's callers (subscribedArtifactsWire below,
	// and httpapi's ListArtifacts in artifacts.go) since this is their
	// one shared implementation. Nil (-> JSON null) for a pr/preview row;
	// only an upload row ever sets these three columns.
	var filename, contentType interface{}
	if a.Filename != nil {
		filename = *a.Filename
	}
	if a.ContentType != nil {
		contentType = *a.ContentType
	}
	var sizeBytes interface{}
	if a.SizeBytes != nil {
		sizeBytes = *a.SizeBytes
	}
	return map[string]interface{}{
		"id":            a.ID,
		"type":          a.Type,
		"url":           a.Url,
		"metadata":      json.RawMessage(a.Metadata),
		"createdAt":     a.CreatedAt,
		"status":        a.Status,
		"failureReason": failureReason,
		"filename":      filename,
		"sizeBytes":     sizeBytes,
		"contentType":   contentType,
	}
}

// sandboxWireMap is eventWireMap/ArtifactWireMap's own sandbox
// counterpart: sqlcgen.Sandbox carries several fields with zero
// legitimate client-side use -- TokenHash (the sandbox's own bearer-token
// hash, an internal credential-verification artifact), ProviderID,
// SpawnFailureCount, and LastSpawnFailureAt (internal ops/bookkeeping) --
// none of which contracts/client-ws/v1/protocol.schema.json's own
// SubscribedPayload.state requires (that field is deliberately
// additionalProperties:true, its own doc comment: "shape assembled by
// later PRs" -- this package's own choice, exactly like eventWireMap/
// ArtifactWireMap already exercise for their own siblings). Returns only
// what a client-side UI legitimately needs: id, gen, status, lastSeenAt,
// createdAt, updatedAt, agentVersion, imageDigest.
//
// agentVersion/imageDigest (§12.2 item 1's own runtime-fingerprint gap,
// migrations/000120_sandboxes_boot_fingerprint.up.sql) are a PROPERTY OF
// THIS SANDBOX (its own running binary/image for this gen) -- nil until
// this gen's own first "ready" event reports them, and reset to nil again
// on the next respawn (UpsertSandboxForSpawn's own doc comment) so a
// connecting/booting new gen never displays its predecessor's now-stale
// fingerprint. Deliberately NOT alongside correlationId (buildSubscribedPayload's
// own top-level state key) -- that is a property of a REQUEST, not of the
// sandbox, and forcing the two into one shape because a mockup happens to
// show them side by side would conflate a fact that outlives many turns
// with one that is fresh per turn.
func sandboxWireMap(s sqlcgen.Sandbox) map[string]interface{} {
	return map[string]interface{}{
		"id":           s.ID,
		"gen":          s.Gen,
		"status":       s.Status,
		"lastSeenAt":   s.LastSeenAt,
		"createdAt":    s.CreatedAt,
		"updatedAt":    s.UpdatedAt,
		"agentVersion": s.AgentVersion,
		"imageDigest":  s.ImageDigest,
	}
}

// latestTurnCorrelationID returns the correlation id of turnRows' own
// most-recently-created element (ListForSession/ListTurnsForSession's own
// ORDER BY created_at ASC -- the last element is the newest), or nil for
// an empty slice (no turns yet) or when that newest turn's own
// correlation_id is itself nil (created with no live request context in
// scope, queries/turns.sql's own CreateTurn doc comment). Never looks
// further back than the single newest turn -- an older turn's own
// correlation id would name a request that is no longer what this
// session's sandbox is talking to.
func latestTurnCorrelationID(turnRows []sqlcgen.Turn) *string {
	if len(turnRows) == 0 {
		return nil
	}
	return turnRows[len(turnRows)-1].CorrelationID
}

func subscribedEventsWire(rows []sqlcgen.Event) []clientws.SubscribedPayloadEventsElem {
	wire := make([]clientws.SubscribedPayloadEventsElem, len(rows))
	for i, e := range rows {
		wire[i] = eventWireMap(e)
	}
	return wire
}

func subscribedArtifactsWire(rows []sqlcgen.Artifact) []clientws.SubscribedPayloadArtifactsElem {
	wire := make([]clientws.SubscribedPayloadArtifactsElem, len(rows))
	for i, a := range rows {
		wire[i] = ArtifactWireMap(a)
	}
	return wire
}

// pingClientLoop runs a periodic, server-initiated liveness check on conn
// (audit-remediation, inbound-hygiene lens): every interval, it sends a
// real websocket ping and waits (bounded by that SAME interval, via a
// context.WithTimeout derived from ctx) for the peer's pong --
// github.com/coder/websocket's own Conn.Ping doc is explicit that this
// requires a concurrent Reader call to ever observe the pong ("Ping must
// be called concurrently with Reader as it does not read from the
// connection but instead waits for a Reader call to read the pong"):
// readClientLoop's own already-running conn.Read(ctx) loop (started by
// this same handler, on a different goroutine) is exactly that concurrent
// Reader call -- this function deliberately never reads from conn itself.
//
// A client that only ever subscribes and passively watches live
// broadcasts (never sending an application frame) is completely healthy
// and legitimate; a naive "no application frame within N seconds" timeout
// would incorrectly kill it. A real ping/pong round trip is the only way
// to distinguish that legitimate case from a genuinely unresponsive peer.
//
// If a Ping ever fails (the peer never answered in time, or the
// connection is already dead), that proves the connection is genuinely
// unresponsive: it is closed with the custom status code 4003 ("idle
// timeout"), the next code after this package's own existing 4001
// ("re-auth required") / 4002 ("token expired") precedents -- a distinct
// code because this is a distinct reason. Closing conn here causes
// readClientLoop's own blocked conn.Read to unblock with an error on its
// own (matching readSubscribeRequest's own already-established "conn is
// now closed; the blocked Read above returns an error" precedent in this
// same package) -- no additional signaling/cancellation plumbing is added
// here. Returns (rather than being handed an error channel) once ctx is
// done or the connection has been closed -- the caller (NewClientHandler)
// runs this via writerGroup.Go, the SAME errgroup/context already used
// for the Hub's own per-connection writer goroutine, so both share one
// teardown.
func pingClientLoop(ctx context.Context, conn *websocket.Conn, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				logger.Warn("wshub: client ws ping unanswered; closing as idle", "error", err)
				_ = conn.Close(websocket.StatusCode(4003), "idle timeout")
				return
			}
		}
	}
}

// clientEnvelope peeks just the "type" discriminator every post-subscribe
// client->CP frame carries (mirroring dispatch.go's own envelope-peek
// convention for the sandbox side). This Step's own protocol defines
// exactly one such shape, "fetch_history" -- an unrecognized type is
// logged and skipped, the same forward-compatible, deny-list-not-allow-
// list convention every other envelope-peek dispatcher in this codebase
// already uses.
type clientEnvelope struct {
	Type string `json:"type"`
}

// readClientLoop reads and dispatches inbound client-WS frames on conn
// until conn.Read errors (disconnect) or ctx is done. A read error simply
// ends the connection -- the caller's own deferred unregister/cancel
// already handle cleanup.
//
// lastFetchHistoryAt (audit-remediation, inbound-hygiene lens) is a plain
// local variable, not shared/locked state -- readClientLoop is already a
// single goroutine processing frames strictly sequentially for this ONE
// connection, so no synchronization is needed to track "the timestamp
// handleFetchHistory was last actually invoked ON THIS CONNECTION". A new
// fetch_history frame arriving before timeouts.ClientFetchHistoryMinInterval
// has elapsed since that last invocation is logged and dropped (matching
// this function's own established "log and skip, connection stays open"
// convention for other invalid input, e.g. handleFetchHistory's own
// malformed-cursor case) -- events.ListForSession is not called and no
// reply is written for it. Deliberately per-connection, not per-session or
// global: this is a rate limit on one connection's own request cadence,
// not cross-connection/cross-session coordination.
func readClientLoop(ctx context.Context, conn *websocket.Conn, sessionID pgtype.UUID, events *postgres.EventStore, timeouts platform.Timeouts, logger *slog.Logger) {
	var lastFetchHistoryAt time.Time
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var env clientEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			logger.Warn("wshub: dropping malformed inbound client frame", "error", err)
			continue
		}

		switch env.Type {
		case "fetch_history":
			now := time.Now()
			if !lastFetchHistoryAt.IsZero() && now.Sub(lastFetchHistoryAt) < timeouts.ClientFetchHistoryMinInterval {
				logger.Warn("wshub: dropping fetch_history request faster than the per-connection rate limit",
					"min_interval", timeouts.ClientFetchHistoryMinInterval)
				continue
			}
			lastFetchHistoryAt = now
			handleFetchHistory(ctx, conn, sessionID, events, data, logger)
		default:
			logger.Warn("wshub: ignoring unrecognized client frame type", "type", env.Type)
		}
	}
}

// handleFetchHistory parses data as a clientws.FetchHistoryRequest and
// replies with the requested page of this session's event log (§6.2).
// The URL's own sessionID is authoritative: a mismatched sessionId in the
// request body is rejected defensively (logged, not fatal -- the
// connection stays open) rather than trusted, since a WS connection is
// already scoped to exactly one session for its entire lifetime.
func handleFetchHistory(ctx context.Context, conn *websocket.Conn, sessionID pgtype.UUID, events *postgres.EventStore, data []byte, logger *slog.Logger) {
	var req clientws.FetchHistoryRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("wshub: malformed fetch_history request", "error", err)
		return
	}
	if req.SessionId != "" && req.SessionId != sessionID.String() {
		logger.Warn("wshub: fetch_history sessionId mismatch; ignoring body value, using the connection's own session",
			"body_session_id", req.SessionId)
	}

	var afterID int64
	if req.Cursor != nil && *req.Cursor != "" {
		parsed, err := strconv.ParseInt(*req.Cursor, 10, 64)
		if err != nil {
			logger.Warn("wshub: malformed fetch_history cursor; treating as start", "cursor", *req.Cursor, "error", err)
		} else {
			afterID = parsed
		}
	}

	limit := fetchHistoryDefaultLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
		if limit > fetchHistoryMaxLimit {
			limit = fetchHistoryMaxLimit
		}
	}

	rows, err := events.ListForSession(ctx, sessionID, afterID, int32(limit))
	if err != nil {
		logger.Error("wshub: fetch_history: list events failed", "error", err)
		return
	}

	wire := make([]clientws.FetchHistoryResponseEventsElem, len(rows))
	for i, e := range rows {
		wire[i] = eventWireMap(e)
	}

	var nextCursor *string
	if len(rows) == limit {
		s := strconv.FormatInt(rows[len(rows)-1].ID, 10)
		nextCursor = &s
	}

	resp := clientws.FetchHistoryResponse{
		Events:     wire,
		NextCursor: nextCursor,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		logger.Error("wshub: marshal fetch_history response failed", "error", err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		logger.Warn("wshub: write fetch_history response failed", "error", err)
	}
}

// hubConnBufferSize bounds how many not-yet-delivered broadcast payloads
// one registered connection's own channel holds before Hub.Broadcast
// starts dropping (and logging a warning) for it -- a plain count, not a
// duration, mirroring sessionactor's own mailboxBufferSize precedent for
// the same reason (not a platform.Timeouts field).
const hubConnBufferSize = 64

// Hub is the in-process, session-keyed registry of live client WS
// connections that makes ports.EventBroadcaster (and therefore §6.2's
// "→ broadcast stream") real. Constructed ONCE in cmd/control-plane/
// main.go and threaded through to BOTH sessionactor.NewRegistry (as the
// broadcaster every Actor's successful transact commits to, see
// internal/app/ports.EventBroadcaster's own doc comment) AND
// NewClientHandler (so it can register/unregister each subscribed
// connection) -- the single shared piece of state connecting the
// app-layer actor to the adapter-layer client sockets.
//
// Cross-pod broadcast fan-out is explicitly NOT solved here: a Hub only
// ever reaches connections registered in THIS process, the same class of
// honest, documented gap as §3.2's ErrSessionActorElsewhere/503
// stopgap (see this package's own doc.go) -- not a Postgres LISTEN/NOTIFY
// or message-bus solution, genuinely out of scope here.
type Hub struct {
	mu    sync.Mutex
	conns map[string]map[*websocket.Conn]hubConn
}

// hubConn is one registered connection's own bookkeeping: its broadcast
// delivery channel (the ONLY thing this type carried before §8's
// "multiplayer presence") plus the real user id it authenticated as --
// "" for a connection whose ws_tokens row named no user (verifyClientToken's
// own doc comment: a deleted minting user), which Participants below
// never counts as a presence participant.
type hubConn struct {
	ch     chan []byte
	userID string
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{conns: make(map[string]map[*websocket.Conn]hubConn)}
}

var _ ports.EventBroadcaster = (*Hub)(nil)

// Register inserts conn's own channel into this Hub's session-keyed map --
// and ONLY that (F8 audit fix; see this method's own contrast with
// StartDelivery below). From the instant this call returns, any
// Hub.Broadcast for sessionID enqueues into the returned ch for conn:
// buffered (hubConnBufferSize), in order, never lost even though nothing is
// draining it yet -- a plain unbuffered-nothing-listening channel send
// would block Broadcast, but this channel already has its full buffer
// capacity from the moment it is created here, before anything is
// registered, so an early broadcast simply queues.
//
// Register deliberately does NOT start the goroutine that drains ch and
// writes those queued payloads to conn -- call StartDelivery once it is
// safe for a broadcast frame to reach the wire (see that method's own doc
// comment for the invariant this split protects). Splitting these two
// concerns is what closes the F8 finding: the original single-call
// Register started draining (and therefore writing to conn) immediately,
// which could race a caller's own concurrent conn.Write for some other
// frame (e.g. NewClientHandler's "subscribed" reply) and let a broadcast
// reach the wire first.
//
// The caller must call the returned unregister exactly once (typically
// deferred), regardless of whether StartDelivery is ever called, so this
// Hub stops tracking a connection that is going away.
//
// userID (§8's "multiplayer presence") is this connection's own
// authenticated user id, or "" for one that has none (verifyClientToken's
// own doc comment) -- recorded alongside ch purely so Participants below
// can report who is live for sessionID; Register itself does nothing
// else with it.
func (h *Hub) Register(sessionID string, conn *websocket.Conn, userID string) (ch chan []byte, unregister func()) {
	ch = make(chan []byte, hubConnBufferSize)

	h.mu.Lock()
	if h.conns[sessionID] == nil {
		h.conns[sessionID] = make(map[*websocket.Conn]hubConn)
	}
	h.conns[sessionID][conn] = hubConn{ch: ch, userID: userID}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.conns[sessionID], conn)
		if len(h.conns[sessionID]) == 0 {
			delete(h.conns, sessionID)
		}
		h.mu.Unlock()
	}
}

// Participants returns the DISTINCT, currently-connected real user ids
// for sessionID (§8's "multiplayer presence") -- one entry per unique
// user regardless of how many tabs/connections that user currently has
// open for this session, in no particular order (the caller,
// resolveParticipants, does not require one). A connection registered
// with userID "" (Register's own doc comment) contributes nothing here.
// Returns nil for a session with no live connections at all (the common
// case), which resolveParticipants' own make([], 0, len(nil)) already
// treats identically to an empty slice.
func (h *Hub) Participants(sessionID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	seen := make(map[string]struct{}, len(h.conns[sessionID]))
	var ids []string
	for _, c := range h.conns[sessionID] {
		if c.userID == "" {
			continue
		}
		if _, dup := seen[c.userID]; dup {
			continue
		}
		seen[c.userID] = struct{}{}
		ids = append(ids, c.userID)
	}
	return ids
}

// StartDelivery starts conn's own dedicated writer goroutine (via eg.Go --
// §11, no naked goroutines) that drains ch -- the same channel Register
// already returned for this same conn -- and writes each queued payload to
// conn verbatim (§6.2: a live-pushed event is the raw stored payload, sent
// exactly as stored -- no wrapper envelope), until writerCtx is done or a
// write fails. This goroutine's lifetime is scoped to writerCtx, which the
// caller (NewClientHandler) derives from the connection's own request
// context and cancels the moment that connection's read loop exits -- both
// exit together, as design decision 7g requires.
//
// THE INVARIANT THIS SPLIT FROM Register EXISTS TO PROTECT (F8 audit
// finding): the caller must not call StartDelivery until its own conn.Write
// for whatever frame must reach the client first (NewClientHandler's own
// "subscribed" reply) has already returned successfully. Register alone
// only guarantees a broadcast is never LOST (queued, buffered, in order) --
// it says nothing about when it is WRITTEN to the wire, because that write
// happens on THIS goroutine, entirely independent of whatever the caller's
// own goroutine is doing with conn concurrently; coder/websocket serializes
// the two Conn.Write calls internally (no data race, no corruption), but
// whichever call reaches that internal lock first is what the client
// actually receives first. Calling StartDelivery too early re-opens
// exactly the wire-order inversion the audit flagged: a live broadcast
// reaching the wire before the connection's own baseline reply, inverting
// §6.2's "single subscribed payload -> broadcast stream". Delaying this
// call until after that reply's own conn.Write returns makes "drain start
// happens-after subscribed write returns" a plain program-order guarantee
// (sequential statements on one goroutine) rather than a race between this
// goroutine and the caller's.
func (h *Hub) StartDelivery(writerCtx context.Context, eg *errgroup.Group, conn *websocket.Conn, ch <-chan []byte) {
	eg.Go(func() error {
		for {
			select {
			case <-writerCtx.Done():
				return nil
			case payload := <-ch:
				if err := conn.Write(writerCtx, websocket.MessageText, payload); err != nil {
					return nil // the connection's own read loop will independently notice and unregister.
				}
			}
		}
	})
}

// Broadcast implements ports.EventBroadcaster: delivers payload to every
// connection currently registered for sessionID via a non-blocking send
// into that connection's own channel, dropping (and logging a warning)
// for any one connection whose buffer is already full -- this must never
// block the caller (the session actor's own single command-processing
// goroutine, see internal/app/sessionactor's Actor.broadcastPending) on a
// slow or non-draining browser client. A session with no registered
// connections (the common case -- most sessions have no live subscriber
// at any given instant) is a fast, silent no-op.
func (h *Hub) Broadcast(sessionID string, payload json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, c := range h.conns[sessionID] {
		select {
		case c.ch <- payload:
		default:
			platform.Logger(platform.WithSessionID(context.Background(), sessionID)).
				Warn("wshub: dropping broadcast for a slow/non-draining client connection")
		}
	}
}

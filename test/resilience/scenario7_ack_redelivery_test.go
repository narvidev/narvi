//go:build integration

// Resilience scenario #7 (§9.3, docs/IMPLEMENTATION_PLAN.md row 30): "WS
// drop during event stream -> ack protocol redelivers the 6 critical
// events exactly once." See test/resilience/README.md's own #7 entry for
// why this needed a genuinely new, full-pipeline test: before this file,
// TestSendCritical_ResendUntilAckedThenNeverAgain (internal/sandboxagent/
// wsbridge/bridge_test.go) proved the SENDER-side resend-until-acked
// behavior against a scripted fake WS server, for only ONE of the 6
// critical types, and TestEventStore_Create_DedupesOnSessionIDAndMessageID
// (internal/adapters/outbound/postgres) proved the RECEIVER-side dedupe
// primitive at the Postgres-store level alone -- neither exercised the
// full inbound pipeline (sandbox agent -> control plane -> Postgres) end
// to end, and neither covered more than one of the 6 types.
//
// This file wires a REAL commander (wshub.NewSandboxRegistry) and a REAL
// wshub.NewSandboxHandler behind a real httptest.Server -- the same
// production types cmd/control-plane/main.go itself wires -- so a real
// internal/sandboxagent/wsbridge.Bridge (completely unmodified) can Dial it
// exactly like a real sandbox-agent process would. Harness.
// NewRegistryWithCommander (harness_test.go) is this file's own one
// genuinely-needed additive harness extension (a real commander instead of
// nil), added there rather than here per this repo's own "small additive
// pieces belong in harness_test.go" precedent (see that method's own doc
// comment).
//
// # Forcing a real "WS drop before an ack is ever written back"
//
// internal/sandboxagent/wsbridge/bridge_test.go's own
// TestSendCritical_ResendUntilAckedThenNeverAgain forces this by scripting
// a FAKE server's own per-connection behavior (read, then abruptly
// disconnect without ever acking). That mechanism doesn't transfer
// directly to a REAL production wshub.NewSandboxHandler instance (its own
// behavior isn't ours to script) -- so this file adapts the SAME
// "script exactly what happens to each successive connection" idea one
// layer further out: wsProxy (below) sits between the real Bridge and the
// real backend server, relaying WS frames message-by-message, with each
// accepted connection consuming one scripted step (mirroring stepServer's
// own FIFO-steps-then-fallback shape exactly):
//
//   - dropAfterClient > 0: relay exactly that many client->backend
//     messages (in practice: "ready", then the critical event itself),
//     then sever BOTH sides immediately -- crucially, WITHOUT ever
//     reading or relaying anything backend->client, so an ack the real
//     backend attempts to write back for that event can never reach the
//     bridge. The backend has already fully received (and durably
//     processed) the critical event by the time this severs -- Write()
//     returning successfully means the frame already left this hop for
//     the backend's own kernel socket buffer, so closing immediately
//     afterward cannot un-send it (the same race tolerance
//     TestSendCritical_ResendUntilAckedThenNeverAgain's own scripted
//     abrupt-disconnect-after-one-read already relies on).
//   - severAfterBackend > 0: full bidirectional relay, but severs (both
//     sides) immediately after relaying that many backend->client
//     messages -- used here to force ONE MORE clean reconnect right after
//     the real ack is delivered, so this test can also prove "never
//     resent again" across that SECOND reconnect too, not merely "was
//     resent once".
//   - the zero value (the fallback once the scripted queue is exhausted):
//     full bidirectional relay, forever, no severing.
//
// # Per-subtest shape
//
// One harness + one real commander/registry/backend server is shared
// across all 6 critical types (constructed once, in the top-level test
// function), but each subtest below seeds its OWN fresh session/sandbox
// row (and its own fresh wsProxy/Bridge) -- avoiding cross-contamination
// between types while not paying for a second throwaway Postgres
// container per type. For each type: (a) send the critical event via a
// real wsbridge.Bridge.SendCritical call against the real (proxied)
// server, (b) the proxy forces the connection closed before any ack is
// ever written back, (c) confirm the bridge resends the IDENTICAL event
// after reconnecting, (d) confirm the events table shows exactly ONE row
// for (session_id, message_id) despite the resend (the same
// upsert-on-conflict dedupe primitive TestEventStore_Create_
// DedupesOnSessionIDAndMessageID already proves at the store level, now
// exercised through the full inbound WS pipeline), and (e) for the two
// types with a real, confirmable idempotent side effect
// (execution_complete completes a Processing turn exactly once;
// snapshot_ready sets the sandbox's own snapshot_id exactly once,
// correlated via its own PendingSnapshotMessageID guard), confirm that
// effect's own fingerprint is UNCHANGED across the redelivery. The other 4
// types (error/push_complete/push_error/sub_task_finish) have no bespoke
// per-type DB-mutation case in sandboxevent.go today -- the events-table
// dedupe assertion (d) alone is this codebase's own correct and sufficient
// proof of "redelivered exactly once" for those, per this PR's own brief;
// no fake per-type side effect is invented here just to have something
// more to assert.
package resilience_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const (
	ackTestGen         = 1
	ackTestDialTimeout = 2 * time.Second
	ackTestMinBackoff  = 20 * time.Millisecond
	ackTestMaxBackoff  = 100 * time.Millisecond
	ackTestHeartbeat   = 10 * time.Second // long enough to never fire during a subtest
	ackTestWait        = 5 * time.Second

	// snapshotPendingMessageID is the fixed "pending snapshot attempt"
	// message id the snapshot_ready subtest's own seed writes onto the
	// sandbox row (PendingSnapshotMessageID) and the SAME value its own
	// buildEvent echoes back as SnapshotReady.CommandMessageId --
	// handleSnapshotReadyEvent's own message-id correlation guard
	// (sandboxevent.go) requires these to match before it will accept the
	// event as completing the CURRENT snapshot attempt.
	snapshotPendingMessageID = "scenario7-pending-snapshot-msg"
	snapshotID               = "snap-scenario7"
)

// noopCommandHandler implements wsbridge.CommandHandler doing nothing --
// no subtest here ever expects an inbound command dispatch (prompt/stop/
// push/snapshot/git_sync_complete); only outbound critical events and
// their inbound "ack" replies (handled internally by Bridge, never exposed
// to CommandHandler) are exercised. Mirrors internal/sandboxagent/wsbridge/
// bridge_test.go's own noopHandler exactly, redefined locally since that
// one is unexported in a different package.
type noopCommandHandler struct{}

func (noopCommandHandler) HandlePrompt(context.Context, sandboxws.Prompt)                   {}
func (noopCommandHandler) HandleStop(context.Context, sandboxws.Stop)                       {}
func (noopCommandHandler) HandlePush(context.Context, sandboxws.Push)                       {}
func (noopCommandHandler) HandleSnapshot(context.Context, sandboxws.Snapshot)               {}
func (noopCommandHandler) HandleGitSyncComplete(context.Context, sandboxws.GitSyncComplete) {}

// wsProxyStep scripts what happens to exactly ONE accepted connection --
// see this file's own top comment for the full reasoning behind each
// field.
type wsProxyStep struct {
	dropAfterClient   int
	severAfterBackend int
}

// wsProxy relays a single WS connection between a real wsbridge.Bridge
// (the "client" from this proxy's own point of view) and a REAL backend
// wshub sandbox-WS server, message by message, so a test can intercept/
// sever the connection at an exact, deterministic point in the stream --
// see this file's own top comment for why this exists and how it adapts
// wsbridge/bridge_test.go's own scripted-fake-server precedent to a real
// backend. Steps are consumed FIFO, one per accepted connection; once
// exhausted, every further connection gets the zero-value step (full
// relay forever, matching stepServer's own fallback convention).
type wsProxy struct {
	mu         sync.Mutex
	steps      []wsProxyStep
	backendURL string
	// onRelay is called with every client->backend payload this proxy ever
	// relays, across every connection it ever accepts -- used by this
	// file's own subtests to observe "ready" (proving a fresh connection
	// is up) and the critical event itself (proving it was sent/resent),
	// mirroring bridge_test.go's own readyCh/critCh two-channel precedent.
	onRelay func(payload []byte)
}

func newWSProxy(backendURL string, onRelay func(payload []byte)) *wsProxy {
	return &wsProxy{backendURL: backendURL, onRelay: onRelay}
}

// pushStep enqueues step for the NEXT accepted connection.
func (p *wsProxy) pushStep(step wsProxyStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, step)
}

func (p *wsProxy) nextStep() wsProxyStep {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.steps) == 0 {
		return wsProxyStep{}
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	return step
}

// forwardableHandshakeHeaders mirrors internal/sandboxagent/wsbridge/
// run.go's own dial method exactly: only these 3 headers are ever needed
// for a sandbox-WS handshake (§6.1) -- selectively re-presented to the
// backend rather than blindly forwarding the incoming request's own full,
// hop-by-hop-header-laden header set (Connection/Upgrade/Sec-WebSocket-*),
// which belongs to THIS hop (bridge<->proxy), not the next one
// (proxy<->backend).
func forwardableHandshakeHeaders(r *http.Request) http.Header {
	h := http.Header{}
	for _, k := range []string{"Authorization", "X-Sandbox-ID", "X-Sandbox-Gen"} {
		if v := r.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	return h
}

func (p *wsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	step := p.nextStep()

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = clientConn.CloseNow() }()

	backendConn, _, err := websocket.Dial(r.Context(), p.backendURL+r.URL.Path+"?"+r.URL.RawQuery,
		&websocket.DialOptions{HTTPHeader: forwardableHandshakeHeaders(r)})
	if err != nil {
		return
	}
	defer func() { _ = backendConn.CloseNow() }()

	if step.dropAfterClient > 0 {
		for i := 0; i < step.dropAfterClient; i++ {
			_, data, err := clientConn.Read(r.Context())
			if err != nil {
				return
			}
			p.onRelay(data)
			if err := backendConn.Write(r.Context(), websocket.MessageText, data); err != nil {
				return
			}
		}
		// Sever immediately -- never read/relay anything backend->client,
		// including whatever ack the backend is about to attempt to write
		// back for the last message just relayed (this file's own top
		// comment explains why this is safe/faithful).
		return
	}

	relayCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var g errgroup.Group
	g.Go(func() error {
		defer cancel()
		for {
			_, data, err := clientConn.Read(relayCtx)
			if err != nil {
				return err
			}
			p.onRelay(data)
			if err := backendConn.Write(relayCtx, websocket.MessageText, data); err != nil {
				return err
			}
		}
	})
	g.Go(func() error {
		defer cancel()
		relayed := 0
		for {
			_, data, err := backendConn.Read(relayCtx)
			if err != nil {
				return err
			}
			if err := clientConn.Write(relayCtx, websocket.MessageText, data); err != nil {
				return err
			}
			relayed++
			if step.severAfterBackend > 0 && relayed >= step.severAfterBackend {
				// Clean stop (not an error) -- the deferred cancel() above
				// tears down the sibling goroutine too, and this
				// function's own deferred Close calls sever both
				// connections, forcing the bridge to reconnect right
				// after it has genuinely received its ack.
				return nil
			}
		}
	})
	_ = g.Wait()
}

// relayEnvelope peeks just the "type" discriminator common to every
// wire shape this proxy ever relays -- mirrors internal/adapters/inbound/
// wshub/dispatch.go's own identical envelope-peeking precedent.
type relayEnvelope struct {
	Type string `json:"type"`
}

// waitChan reads exactly one value from ch, failing the test if timeout
// elapses first. Mirrors internal/sandboxagent/wsbridge/bridge_test.go's
// own identical helper (redefined locally, unexported in a different
// package).
func waitChan[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for a channel value", timeout)
		var zero T
		return zero
	}
}

// countEventsByMessageID returns how many rows exist in events for
// (sessionID, messageID) -- the exact dedupe key TestEventStore_Create_
// DedupesOnSessionIDAndMessageID already proves at the store level;
// mirrors that test's and internal/adapters/inbound/wshub's own countEvents
// helper's identical raw-SQL-count shape.
func countEventsByMessageID(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID, messageID string) int {
	t.Helper()

	var n int
	if err := h.Pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND message_id = $2`,
		sessionID, messageID,
	).Scan(&n); err != nil {
		t.Fatalf("count events for message_id %q: %v", messageID, err)
	}
	return n
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ackRedeliveryCase is one of the 6 critical-type subtests below --
// everything specific to a given type lives in these 4 fields, sharing one
// otherwise-identical round-trip mechanic (runAckRedeliveryCase).
type ackRedeliveryCase struct {
	name       string
	eventType  string
	seed       func(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID)
	buildEvent func(sessionID, messageID, ackID string) any
	// verify, when non-nil, asserts this type's own real idempotent side
	// effect and returns a fingerprint string summarizing it -- called
	// once after the first delivery and once more after the redelivery;
	// runAckRedeliveryCase asserts the two fingerprints are IDENTICAL
	// (the side effect fired exactly once, never twice). nil for the 4
	// types with no bespoke per-type DB-mutation case (this file's own
	// top comment explains why the events-table dedupe assertion alone is
	// sufficient for those).
	verify func(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) string
}

func verifyExecutionCompletedOnce(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) string {
	t.Helper()

	turns, err := h.Turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turn count for session = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.Status != sqlcgen.TurnStatusCompleted {
		t.Errorf("turn status = %s, want %s (execution_complete must complete the Processing turn)", got.Status, sqlcgen.TurnStatusCompleted)
	}
	if !got.CompletedAt.Valid {
		t.Error("turn completed_at not set, want set")
	}
	return string(got.Status) + "@" + got.CompletedAt.Time.String()
}

func verifySnapshotReadyOnce(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) string {
	t.Helper()

	got, err := h.Sandboxes.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("sandbox status = %s, want %s (snapshot_ready must finalize Snapshotting->Ready)", got.Status, sqlcgen.SandboxStatusReady)
	}
	if got.SnapshotID == nil || *got.SnapshotID != snapshotID {
		t.Errorf("sandbox snapshot_id = %v, want %q", got.SnapshotID, snapshotID)
	}
	return string(got.Status) + "@" + stringOrEmpty(got.SnapshotID)
}

func seedNothing(context.Context, *testing.T, *Harness, pgtype.UUID) {}

func seedProcessingTurn(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) {
	t.Helper()

	prompt := "scenario7 ack-redelivery turn"
	created, err := h.Turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := h.Turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}
}

func seedSnapshottingSandbox(ctx context.Context, t *testing.T, h *Harness, sessionID pgtype.UUID) {
	t.Helper()

	if _, err := h.Sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusSnapshotting,
	}); err != nil {
		t.Fatalf("move sandbox to snapshotting: %v", err)
	}
	pending := snapshotPendingMessageID
	if _, err := h.Sandboxes.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{
		SessionID: sessionID, PendingSnapshotMessageID: &pending,
	}); err != nil {
		t.Fatalf("seed pending snapshot message id: %v", err)
	}
}

func ackRedeliveryCases() []ackRedeliveryCase {
	return []ackRedeliveryCase{
		{
			name:      "execution_complete",
			eventType: "execution_complete",
			seed:      seedProcessingTurn,
			buildEvent: func(sessionID, messageID, ackID string) any {
				return sandboxws.ExecutionComplete{
					Type: "execution_complete", MessageId: messageID, SessionId: sessionID, Gen: ackTestGen,
					AckId: ackID, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted, Reason: nil,
				}
			},
			verify: verifyExecutionCompletedOnce,
		},
		{
			name:      "error",
			eventType: "error",
			seed:      seedNothing,
			buildEvent: func(sessionID, messageID, ackID string) any {
				return sandboxws.SandboxErrorEvent{
					Type: "error", MessageId: messageID, SessionId: sessionID, Gen: ackTestGen,
					AckId: ackID, Message: "scenario7 injected error", Fatal: true,
				}
			},
			verify: nil,
		},
		{
			name:      "snapshot_ready",
			eventType: "snapshot_ready",
			seed:      seedSnapshottingSandbox,
			buildEvent: func(sessionID, messageID, ackID string) any {
				pending := snapshotPendingMessageID
				return sandboxws.SnapshotReady{
					Type: "snapshot_ready", MessageId: messageID, SessionId: sessionID, Gen: ackTestGen,
					AckId: ackID, SnapshotId: snapshotID, CommandMessageId: &pending,
				}
			},
			verify: verifySnapshotReadyOnce,
		},
		{
			name:      "push_complete",
			eventType: "push_complete",
			seed:      seedNothing,
			buildEvent: func(sessionID, messageID, ackID string) any {
				return sandboxws.PushComplete{
					Type: "push_complete", MessageId: messageID, SessionId: sessionID, Gen: ackTestGen,
					AckId: ackID, Repos: []sandboxws.PushCompleteReposElem{{Name: "repo1", Branch: "main", Sha: "abc123"}},
				}
			},
			verify: nil,
		},
		{
			name:      "push_error",
			eventType: "push_error",
			seed:      seedNothing,
			buildEvent: func(sessionID, messageID, ackID string) any {
				return sandboxws.PushError{
					Type: "push_error", MessageId: messageID, SessionId: sessionID, Gen: ackTestGen,
					AckId: ackID, Error: "scenario7 injected push error",
				}
			},
			verify: nil,
		},
		{
			name:      "sub_task_finish",
			eventType: "sub_task_finish",
			seed:      seedNothing,
			buildEvent: func(sessionID, messageID, ackID string) any {
				return sandboxws.SubTaskFinish{
					Type: "sub_task_finish", MessageId: messageID, SessionId: sessionID, Gen: ackTestGen,
					AckId: ackID, SubTaskId: "sub-1", Outcome: sandboxws.SubTaskFinishOutcomeCompleted,
				}
			},
			verify: nil,
		},
	}
}

// runAckRedeliveryCase drives tc's own full round trip against backendWsURL
// (the REAL, shared wshub server this test's own top-level function
// constructs once) -- see this file's own top comment for the full
// sequencing this implements.
func runAckRedeliveryCase(t *testing.T, h *Harness, backendWsURL string, tc ackRedeliveryCase) {
	ctx := context.Background()

	sessionID := h.CreateSession(ctx, t)
	if _, err := h.Sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	tc.seed(ctx, t, h, sessionID)

	readyCh := make(chan struct{}, 8)
	critCh := make(chan []byte, 8)
	onRelay := func(payload []byte) {
		var env relayEnvelope
		_ = json.Unmarshal(payload, &env)
		if env.Type == "ready" {
			readyCh <- struct{}{}
			return
		}
		critCh <- append([]byte(nil), payload...)
	}

	proxy := newWSProxy(backendWsURL, onRelay)
	// Connection #1: relay "ready" + the critical event to the real
	// backend, then sever before any ack can ever come back.
	proxy.pushStep(wsProxyStep{dropAfterClient: 2})
	// Connection #2: full relay, but sever right after the real ack (the
	// one and only backend->client message expected this round) comes
	// back -- forcing one more clean reconnect so this test can also prove
	// "never resent again" across THAT reconnect too.
	proxy.pushStep(wsProxyStep{severAfterBackend: 1})
	// Connection #3 onward: the zero-value fallback (full relay forever).

	proxyServer := httptest.NewServer(proxy)
	t.Cleanup(proxyServer.Close)
	proxyWsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http")

	sc := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: proxyWsURL + "/sessions/" + sessionID.String() + "/ws?type=sandbox",
		Gen:               ackTestGen,
		SandboxToken:      "scenario7-test-token", // any non-empty value: token_hash is NULL on a freshly created sandbox row (verifySandboxToken's own documented nil-hash bridge).
		SessionId:         sessionID.String(),
	}
	bridge := wsbridge.New(sc, "sbx-scenario7-"+tc.eventType, "test-agent-version", "test-image-digest", noopCommandHandler{},
		ackTestDialTimeout, ackTestHeartbeat, ackTestMinBackoff, ackTestMaxBackoff)

	runCtx, cancel := context.WithCancel(ctx)
	var group errgroup.Group
	group.Go(func() error { return bridge.Run(runCtx) })
	t.Cleanup(func() {
		cancel()
		if err := group.Wait(); err != nil {
			t.Errorf("bridge.Run() error = %v, want nil after ctx cancellation", err)
		}
	})

	waitChan(t, readyCh, ackTestWait) // connection #1 ready

	messageID := "msg-" + tc.eventType
	ackID := tc.eventType + ":" + messageID
	msg := tc.buildEvent(sessionID.String(), messageID, ackID)
	if err := bridge.SendCritical(runCtx, msg, ackID); err != nil {
		t.Fatalf("SendCritical() error = %v, want nil", err)
	}

	first := waitChan(t, critCh, ackTestWait) // relayed to the real backend, then severed before any ack

	waitUntil(t, ackTestWait, func() bool { return countEventsByMessageID(ctx, t, h, sessionID, messageID) == 1 })
	var fingerprintBefore string
	if tc.verify != nil {
		fingerprintBefore = tc.verify(ctx, t, h, sessionID)
	}

	waitChan(t, readyCh, ackTestWait)          // connection #2 ready (reconnect happened)
	second := waitChan(t, critCh, ackTestWait) // the RESENT critical event

	if string(first) != string(second) {
		t.Errorf("resent critical payload differs from original:\nfirst:  %s\nsecond: %s", first, second)
	}

	// The events table must show EXACTLY ONE row for this (session_id,
	// message_id) pair despite the genuine second delivery attempt just
	// relayed above -- the core assertion of this whole scenario.
	waitUntil(t, ackTestWait, func() bool { return countEventsByMessageID(ctx, t, h, sessionID, messageID) == 1 })
	if got := countEventsByMessageID(ctx, t, h, sessionID, messageID); got != 1 {
		t.Errorf("event count for (session_id, message_id=%q) = %d, want 1 (redelivered exactly once)", messageID, got)
	}

	if tc.verify != nil {
		fingerprintAfter := tc.verify(ctx, t, h, sessionID)
		if fingerprintBefore != fingerprintAfter {
			t.Errorf("idempotent side effect changed across redelivery: before=%q after=%q (want unchanged -- fired exactly once, not twice)",
				fingerprintBefore, fingerprintAfter)
		}
	}

	// Connection #2's own severAfterBackend step cuts the connection right
	// after relaying the real ack back -- proving THAT actually happened
	// (rather than merely inferring it from the DB state above) by driving
	// one more reconnect and confirming the bridge's own buffer, now
	// genuinely acked, never resends this entry again.
	waitChan(t, readyCh, ackTestWait) // connection #3 ready (second reconnect, right after the ack)
	select {
	case unexpected := <-critCh:
		t.Errorf("critical event resent a THIRD time even though it was already acked on the previous connection: %s", unexpected)
	case <-time.After(400 * time.Millisecond):
		// Correctly not resent -- the ack from connection #2 landed.
	}
}

func TestResilienceScenario7_WSDropAckRedelivery_CriticalEventsRedeliveredExactlyOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	commander := wshub.NewSandboxRegistry(h.Timeouts)
	registry := h.NewRegistryWithCommander(ctx, t, commander)
	t.Cleanup(func() { _ = registry.Shutdown() })

	router := chi.NewRouter()
	router.Get("/sessions/{sessionID}/ws", wshub.NewSandboxHandler(registry, h.Sandboxes, commander, h.Timeouts))
	backendServer := httptest.NewServer(router)
	t.Cleanup(backendServer.Close)
	backendWsURL := "ws" + strings.TrimPrefix(backendServer.URL, "http")

	for _, tc := range ackRedeliveryCases() {
		t.Run(tc.name, func(t *testing.T) {
			runAckRedeliveryCase(t, h, backendWsURL, tc)
		})
	}
}

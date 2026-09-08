// Real, in-process round-trip test for HandleSnapshot (§3.2,
// "snapshots & restore", design decision 4): a fake control-plane server
// backing BOTH the sandbox-WS endpoint and the new snapshot-mint endpoint,
// driving a real *wsbridge.Bridge connection -- unlike
// push_integration_test.go's own subprocess-based proof (needed there
// specifically because os.Executable() must resolve to the real compiled
// binary for git's own credential-helper re-invocation to work),
// HandleSnapshot needs no subprocess at all: it never shells out to git or
// re-invokes this binary, so calling it directly, in-process, against a
// real Bridge is both sufficient and simpler -- mirrors
// internal/sandboxagent/wsbridge's own bridge_test.go conventions (a real
// httptest.Server standing in for the control plane, a real Bridge.Run in
// the background) rather than a copy of push_integration_test.go's
// heavier subprocess machinery.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

const snapshotTestWait = 5 * time.Second

// fakeSnapshotCP stands in for the real control plane's sandbox-WS
// endpoint AND its new snapshot-mint endpoint (design decision 2), for an
// in-process (no subprocess, no real Postgres) proof of HandleSnapshot's
// own full round trip: obtain a real snapshotId from a fake CP, then
// report it back as a real, critical snapshot_ready event over a real
// *wsbridge.Bridge connection.
type fakeSnapshotCP struct {
	server         *httptest.Server
	sessionID      string
	mintSnapshotID string
	// mintStatus, when non-zero, makes the mint endpoint respond with this
	// HTTP status (and a failure body) instead of a real 200 + snapshotId.
	mintStatus int
	frames     chan json.RawMessage
	// gotMintGenHeader records the real X-Sandbox-Gen header value the
	// snapshot-mint request actually carried (audit remediation:
	// snapshotclient.Client.Mint now always sends one) -- proves the wiring
	// all the way from SessionConfig.Gen through HandleSnapshot's own
	// client.Mint call to the real outbound HTTP request, not merely that
	// Mint's own unit tests set the header correctly in isolation.
	gotMintGenHeader string
	// mintCalled records whether the snapshot-mint endpoint was ever hit at
	// all -- §30.4(3)'s own mint-time purge must abort HandleSnapshot
	// BEFORE the mint request is even built when the purge itself fails,
	// so a test proving that abort needs a way to observe "the mint
	// endpoint was never reached", not merely "no snapshot_ready arrived".
	mintCalled bool
}

func newFakeSnapshotCP(t *testing.T, sessionID string) *fakeSnapshotCP {
	t.Helper()
	fcp := &fakeSnapshotCP{
		sessionID: sessionID,
		frames:    make(chan json.RawMessage, 8),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/"+sessionID+"/snapshot", func(w http.ResponseWriter, r *http.Request) {
		fcp.mintCalled = true
		fcp.gotMintGenHeader = r.Header.Get("X-Sandbox-Gen")
		if fcp.mintStatus != 0 {
			http.Error(w, "mint failed", fcp.mintStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"snapshotId": fcp.mintSnapshotID})
	})
	mux.HandleFunc("/sessions/"+sessionID+"/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil { // the bridge's own first "ready" event
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			cp := make(json.RawMessage, len(data))
			copy(cp, data)
			select {
			case fcp.frames <- cp:
			default:
			}
		}
	})

	fcp.server = httptest.NewServer(mux)
	t.Cleanup(fcp.server.Close)
	return fcp
}

func (fcp *fakeSnapshotCP) wsURL() string {
	return "ws" + strings.TrimPrefix(fcp.server.URL, "http") + "/sessions/" + fcp.sessionID + "/ws?type=sandbox"
}

// waitForFrameType polls fcp.frames for one whose "type" matches want,
// within timeout -- any other frame type seen along the way (e.g. a
// heartbeat) is silently skipped, never mistaken for the one under test.
func waitForFrameType(t *testing.T, fcp *fakeSnapshotCP, want string, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case data := <-fcp.frames:
			var peek struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &peek); err != nil {
				continue
			}
			if peek.Type == want {
				return data
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q frame", want)
			return nil
		}
	}
}

// newTestBridgeHandler builds a real *commandHandler + *wsbridge.Bridge
// pair against fcp's own fake server, running Bridge.Run in the
// background (t.Cleanup tears it down) -- mirrors main.go's own two-phase
// commandHandler/Bridge construction (commandHandler's own doc comment)
// exactly. credentialCacheDir is threaded straight into cfg.
// CredentialCacheDir (empty string for every pre-existing test here,
// exactly matching this function's own prior, implicit zero-value
// behavior) so §30.4(3)'s own new snapshot-mint-time purge (HandleSnapshot)
// has a real, test-owned directory to purge for the tests that care.
func newTestBridgeHandler(ctx context.Context, t *testing.T, fcp *fakeSnapshotCP, gen int, credentialCacheDir string) *commandHandler {
	t.Helper()

	cfg := sessionconfig.SessionConfig{
		BootMode:          sessionconfig.SessionConfigBootModeFresh,
		ControlPlaneWsUrl: fcp.wsURL(),
		Gen:               gen,
		SandboxToken:      "test-sandbox-token",
		SessionId:         fcp.sessionID,
	}
	timeouts := platform.DefaultTimeouts()

	handler := &commandHandler{runCtx: ctx, cfg: boot.Config{SessionConfig: &cfg, CredentialCacheDir: credentialCacheDir}, timeouts: timeouts}
	bridge := wsbridge.New(cfg, "sbx-1", "test-agent-version", "test-image-digest", handler,
		timeouts.SandboxWSDialTimeout, timeouts.SandboxWSHeartbeatInterval,
		timeouts.SandboxWSReconnectMinBackoff, timeouts.SandboxWSReconnectMaxBackoff)
	handler.bridge = bridge

	var group errgroup.Group
	group.Go(func() error { return bridge.Run(ctx) })
	t.Cleanup(func() { _ = group.Wait() })

	return handler
}

// TestHandleSnapshot_Success_SendsRealSnapshotReady proves the full round
// trip: a real Mint call against the fake CP's snapshot-mint endpoint
// succeeds, and HandleSnapshot sends back a real, schema-valid, CRITICAL
// snapshot_ready event over the real Bridge connection carrying the exact
// id the fake CP returned.
func TestHandleSnapshot_Success_SendsRealSnapshotReady(t *testing.T) {
	fcp := newFakeSnapshotCP(t, "snapshot-success-session")
	fcp.mintSnapshotID = "snap-real-abc"

	ctx, cancel := context.WithTimeout(context.Background(), snapshotTestWait)
	t.Cleanup(cancel)

	handler := newTestBridgeHandler(ctx, t, fcp, 3, "")

	handler.HandleSnapshot(ctx, sandboxws.Snapshot{
		Type: "snapshot", MessageId: "msg-1", SessionId: fcp.sessionID, Gen: 3,
	})

	raw := waitForFrameType(t, fcp, "snapshot_ready", snapshotTestWait)

	// Audit remediation: proves the real outbound snapshot-mint request
	// carried "X-Sandbox-Gen: 3", matching this handler's own configured
	// SessionConfig.Gen -- the control plane's new gen-fencing check on
	// this endpoint would otherwise reject every real request like this
	// one.
	if fcp.gotMintGenHeader != "3" {
		t.Errorf("snapshot-mint request X-Sandbox-Gen header = %q, want %q", fcp.gotMintGenHeader, "3")
	}

	var got sandboxws.SnapshotReady
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal SnapshotReady: %v (raw: %s)", err, raw)
	}
	if got.SnapshotId != "snap-real-abc" {
		t.Errorf("SnapshotId = %q, want %q", got.SnapshotId, "snap-real-abc")
	}
	if got.SessionId != fcp.sessionID {
		t.Errorf("SessionId = %q, want %q", got.SessionId, fcp.sessionID)
	}
	if got.Gen != 3 {
		t.Errorf("Gen = %d, want 3", got.Gen)
	}
	if got.AckId != "snapshot_ready:"+got.MessageId {
		t.Errorf("AckId = %q, want %q", got.AckId, "snapshot_ready:"+got.MessageId)
	}
	if got.MessageId == "" {
		t.Error("MessageId is empty, want a freshly minted uuid")
	}
	// Message-id correlation fix: CommandMessageId must echo the Snapshot
	// command's own MessageId verbatim ("msg-1" above), NOT this event's
	// own freshly-minted MessageId -- the control plane's own
	// handleSnapshotReadyEvent needs this to tell two snapshot attempts on
	// the same live sandbox apart (gen alone can't).
	if got.CommandMessageId == nil {
		t.Fatal("CommandMessageId is nil, want it to echo the Snapshot command's own MessageId")
	}
	if *got.CommandMessageId != "msg-1" {
		t.Errorf("CommandMessageId = %q, want %q", *got.CommandMessageId, "msg-1")
	}
}

// TestHandleSnapshot_MintFailure_SendsNothing proves design decision 2's
// own honest, documented failure-reporting gap: a mint-endpoint failure
// makes HandleSnapshot log and return, sending NOTHING over the bridge --
// no snapshot_ready, no crash, no hang (there is no NACK-shaped event on
// the wire to report this failure with).
func TestHandleSnapshot_MintFailure_SendsNothing(t *testing.T) {
	fcp := newFakeSnapshotCP(t, "snapshot-failure-session")
	fcp.mintStatus = http.StatusInternalServerError

	ctx, cancel := context.WithTimeout(context.Background(), snapshotTestWait)
	t.Cleanup(cancel)

	handler := newTestBridgeHandler(ctx, t, fcp, 1, "")

	handler.HandleSnapshot(ctx, sandboxws.Snapshot{
		Type: "snapshot", MessageId: "msg-1", SessionId: fcp.sessionID, Gen: 1,
	})

	select {
	case data := <-fcp.frames:
		var peek struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &peek)
		t.Fatalf("HandleSnapshot sent a frame after a mint failure (type=%q), want nothing", peek.Type)
	case <-time.After(500 * time.Millisecond):
		// Expected: nothing sent. A short, fixed wait, not a poll-for-a-
		// positive-signal -- there is nothing to positively wait FOR when
		// asserting something did NOT happen (same reasoning
		// dispatch_integration_test.go's own circuit-breaker-blocks-spawn
		// test uses its own fixed sleep for).
	}
}

// TestHandleSnapshot_PurgesCredentialCacheBeforeMint proves §30.4(3)'s own
// primary fix: HandleSnapshot purges cfg.CredentialCacheDir BEFORE ever
// building the mint request, so a credential cached by an earlier, live
// credential-helper "get" is gone from disk by the time the snapshot
// completes -- regardless of whether the mint itself then succeeds.
func TestHandleSnapshot_PurgesCredentialCacheBeforeMint(t *testing.T) {
	fcp := newFakeSnapshotCP(t, "snapshot-purge-session")
	fcp.mintSnapshotID = "snap-purge-abc"

	cacheDir := t.TempDir()
	// 0700, matching what MkdirAll gives the real cache dir: Cache
	// refuses to write into a directory group or others can enter, since
	// under a world-writable parent another uid owning that directory can
	// redirect this process's own O_CREATE with a symlink.
	if err := os.Chmod(cacheDir, 0o700); err != nil {
		t.Fatalf("chmod cache dir: %v", err)
	}
	cache := &credentials.Cache{Dir: cacheDir}
	if err := cache.Store("github.com", credentials.Credential{Username: "x-access-token", Password: "leftover-write-token"}); err != nil {
		t.Fatalf("seed credential cache: %v", err)
	}
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) == 0 {
		t.Fatalf("precondition failed: cache dir has no seeded entries (err=%v)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), snapshotTestWait)
	t.Cleanup(cancel)

	handler := newTestBridgeHandler(ctx, t, fcp, 1, cacheDir)

	handler.HandleSnapshot(ctx, sandboxws.Snapshot{
		Type: "snapshot", MessageId: "msg-1", SessionId: fcp.sessionID, Gen: 1,
	})

	// The happy path still completes: purging the cache must never block a
	// real mint from proceeding.
	waitForFrameType(t, fcp, "snapshot_ready", snapshotTestWait)

	// The guarantee is that no credential survives into the snapshot, not
	// that the directory is gone. PurgeAll empties it and leaves it in
	// place on purpose: removing it would unclaim its name under a
	// world-writable parent, letting another uid create it and redirect a
	// later root write with a symlink.
	entries, readErr := os.ReadDir(cacheDir)
	if readErr != nil {
		t.Fatalf("credential cache dir %s does not survive HandleSnapshot: %v", cacheDir, readErr)
	}
	if len(entries) != 0 {
		t.Errorf("credential cache dir holds %d entries after HandleSnapshot, want 0 -- nothing may be captured in the snapshot", len(entries))
	}
}

// TestHandleSnapshot_PurgeFailure_AbortsWithoutMinting proves this Step's
// own chosen fail direction: when the mint-time credential-cache purge
// itself fails, HandleSnapshot aborts the ENTIRE snapshot attempt --
// never building the mint request, never contacting the control plane,
// never sending a snapshot_ready -- rather than proceeding and risking a
// snapshot that captures a leftover credential. The purge is forced to
// fail by making cacheDir's own PARENT directory unwritable (0o500, no
// write bit), so os.RemoveAll(cacheDir) cannot unlink the cacheDir entry
// itself from the (real, existing) directory it sits in.
func TestHandleSnapshot_PurgeFailure_AbortsWithoutMinting(t *testing.T) {
	fcp := newFakeSnapshotCP(t, "snapshot-purge-failure-session")
	fcp.mintSnapshotID = "snap-should-never-be-sent"

	parentDir := t.TempDir()
	cacheDir := filepath.Join(parentDir, "narvi-credentials")
	if err := os.Mkdir(cacheDir, 0o700); err != nil {
		t.Fatalf("Mkdir(cacheDir): %v", err)
	}
	// Seed an entry and make the cache dir itself unwritable, so removing
	// that entry fails.
	//
	// The injection used to be a read-only PARENT, which worked when
	// PurgeAll removed the directory itself. It no longer does: PurgeAll
	// now empties the directory and leaves it in place, deliberately, so
	// its name is never unclaimed under a world-writable /tmp where
	// another uid could claim it. A read-only parent no longer blocks
	// anything, so the failure has to be injected where the work now
	// happens.
	if err := os.WriteFile(filepath.Join(cacheDir, "leftover.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed a cache entry: %v", err)
	}
	if err := os.Chmod(cacheDir, 0o500); err != nil {
		t.Fatalf("Chmod(cacheDir, 0o500): %v", err)
	}
	// Restore write permission before t.TempDir()'s own cleanup runs, or
	// that cleanup fails for the same reason this test forces on PurgeAll.
	t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o700) })

	ctx, cancel := context.WithTimeout(context.Background(), snapshotTestWait)
	t.Cleanup(cancel)

	handler := newTestBridgeHandler(ctx, t, fcp, 1, cacheDir)

	handler.HandleSnapshot(ctx, sandboxws.Snapshot{
		Type: "snapshot", MessageId: "msg-1", SessionId: fcp.sessionID, Gen: 1,
	})

	select {
	case data := <-fcp.frames:
		var peek struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &peek)
		t.Fatalf("HandleSnapshot sent a frame after a purge failure (type=%q), want nothing", peek.Type)
	case <-time.After(500 * time.Millisecond):
		// Expected: nothing sent -- same fixed-wait reasoning as
		// TestHandleSnapshot_MintFailure_SendsNothing above.
	}

	if fcp.mintCalled {
		t.Error("HandleSnapshot called the snapshot-mint endpoint despite a failed cache purge; want the mint request never built at all")
	}
}

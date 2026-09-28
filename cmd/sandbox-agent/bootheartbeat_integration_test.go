//go:build integration

// The real sandbox-agent binary's boot-phase heartbeats (technical plan
// §3.2), with its real boot: a real clone, then a repo setup.sh that
// takes a while -- the window the clone, the git-dir sync and the repo
// hooks spend reporting no boot phase at all. A heartbeat's null
// lastBootPhase is the wire's "boot has completed" and moves the sandbox
// Booting -> Ready, so the agent must never send one before its boot has
// completed, and must show the control plane that its boot is running
// before then. Reuses push_integration_test.go's binary build, git server
// and subprocess runner; the fake control plane here only records what
// arrives on the sandbox WebSocket.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// bootHeartbeatSetupSleep is how long the test repo's setup.sh runs.
const bootHeartbeatSetupSleep = 5 * time.Second

type recordedHeartbeat struct {
	at            time.Time
	lastBootPhase *string
}

// heartbeatRecorder is the fake control plane's record of the sandbox
// WebSocket: when "ready" arrived, every heartbeat after it, and the kind
// of every frame, in arrival order (frameKind).
type heartbeatRecorder struct {
	mu         sync.Mutex
	readyAt    time.Time
	heartbeats []recordedHeartbeat
	frames     []string
}

func (r *heartbeatRecorder) record(frameType string, lastBootPhase *string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch frameType {
	case "ready":
		if r.readyAt.IsZero() {
			r.readyAt = at
		}
	case "heartbeat":
		r.heartbeats = append(r.heartbeats, recordedHeartbeat{at: at, lastBootPhase: lastBootPhase})
	}
}

func (r *heartbeatRecorder) snapshot() (time.Time, []recordedHeartbeat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readyAt, append([]recordedHeartbeat(nil), r.heartbeats...)
}

// recordFrame logs one frame's kind: "heartbeat:<phase>" ("null" for a
// null phase), "boot_timing:<metric>:failed=<failed>", or its bare type.
func (r *heartbeatRecorder) recordFrame(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, kind)
}

func (r *heartbeatRecorder) frameLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.frames...)
}

// newHeartbeatRecordingCP serves the sandbox WebSocket for sessionID,
// recording every frame, and nothing else (every other control-plane call
// the agent makes gets a 404 and degrades, as in push_integration_test.go).
func newHeartbeatRecordingCP(t *testing.T, sessionID string) (*fakeControlPlane, *heartbeatRecorder) {
	t.Helper()
	rec := &heartbeatRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/"+sessionID+"/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var peek struct {
				Type          string  `json:"type"`
				LastBootPhase *string `json:"lastBootPhase"`
				Metric        string  `json:"metric"`
				Failed        *bool   `json:"failed"`
			}
			if json.Unmarshal(data, &peek) == nil {
				rec.record(peek.Type, peek.LastBootPhase, time.Now())
				kind := peek.Type
				switch peek.Type {
				case "heartbeat":
					kind += ":" + phaseOrNull(peek.LastBootPhase)
				case "boot_timing":
					kind += ":" + peek.Metric + ":failed=" + strconv.FormatBool(peek.Failed != nil && *peek.Failed)
				}
				rec.recordFrame(kind)
			}
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &fakeControlPlane{server: server, sessionID: sessionID}, rec
}

// setUpBareRepoWithSlowSetup is setUpBareRepoAndServer's repo plus an
// executable setup.sh that sleeps bootHeartbeatSetupSleep -- a fresh boot
// runs it after the clone.
func setUpBareRepoWithSlowSetup(t *testing.T) (gitServerURL string) {
	t.Helper()
	reposParent := t.TempDir()
	bareRepoDir := filepath.Join(reposParent, "repo.git")
	mustRunGit(t, reposParent, "init", "--bare", "-b", "main", bareRepoDir)

	seedDir := t.TempDir()
	mustRunGit(t, reposParent, "clone", bareRepoDir, seedDir)
	script := "#!/bin/sh\nsleep " + strconv.Itoa(int(bootHeartbeatSetupSleep/time.Second)) + "\n"
	if err := os.WriteFile(filepath.Join(seedDir, "setup.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write setup.sh: %v", err)
	}
	mustRunGit(t, seedDir, "add", "setup.sh")
	mustRunGit(t, seedDir, "commit", "-m", "slow setup")
	mustRunGit(t, seedDir, "push", "origin", "main")

	return startGitHTTPServer(t, reposParent).URL
}

func phaseOrNull(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// TestRun_BootHeartbeats_PhaseUntilBootCompletes: the real binary's first
// heartbeat is the one it forces as its boot starts, carrying
// wsbridge.InitialBootPhase -- well inside the 30s heartbeat interval, so
// nothing else explains it, and the boot evidence the control plane needs
// before a null phase counts. No heartbeat is null while setup.sh runs;
// the first null one follows the boot's completion at once, forced by
// MarkBootComplete, and none after it carries a phase again. The boot's
// boot_duration reaches the control plane exactly once, failed=false,
// ahead of that null: run() relays the one completeBoot builds after its
// re-own pass, rather than dropping it or sending one of its own.
func TestRun_BootHeartbeats_PhaseUntilBootCompletes(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)
	gitServerURL := setUpBareRepoWithSlowSetup(t)
	workspaceDir := t.TempDir()

	fcp, rec := newHeartbeatRecordingCP(t, "boot-heartbeat-session")
	out, exited := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)
	waitForBootComplete(t, out, exited)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, hbs := rec.snapshot()
		if len(hbs) > 0 && hbs[len(hbs)-1].lastBootPhase == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no null-phase heartbeat within 10s of the boot completing (the regular interval is 30s); heartbeats: %d; output:\n%s", len(hbs), out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}

	readyAt, hbs := rec.snapshot()
	if readyAt.IsZero() {
		t.Fatal("no ready frame recorded")
	}
	first := hbs[0]
	if first.lastBootPhase == nil || *first.lastBootPhase != wsbridge.InitialBootPhase {
		t.Fatalf("first heartbeat lastBootPhase = %s, want %q: the boot-started heartbeat", phaseOrNull(first.lastBootPhase), wsbridge.InitialBootPhase)
	}
	if lag := first.at.Sub(readyAt); lag >= bootHeartbeatSetupSleep {
		t.Errorf("first heartbeat arrived %s after ready, want well under %s: the heartbeat forced as the boot starts", lag, bootHeartbeatSetupSleep)
	}

	firstNull := -1
	for i, hb := range hbs {
		if hb.lastBootPhase == nil {
			if firstNull < 0 {
				firstNull = i
			}
			continue
		}
		if firstNull >= 0 {
			t.Errorf("heartbeat %d lastBootPhase = %q after a null one: a phase after boot completion", i, *hb.lastBootPhase)
		}
		if *hb.lastBootPhase != wsbridge.InitialBootPhase {
			t.Errorf("heartbeat %d lastBootPhase = %q, want %q: this repo starts no service", i, *hb.lastBootPhase, wsbridge.InitialBootPhase)
		}
	}
	// setup.sh alone runs bootHeartbeatSetupSleep after the boot started;
	// a second of slack covers the clone before it and scheduling.
	if gap := hbs[firstNull].at.Sub(first.at); gap < bootHeartbeatSetupSleep-time.Second {
		t.Errorf("first null-phase heartbeat arrived %s after the boot-started one, want at least %s: null while setup.sh was still running",
			gap, bootHeartbeatSetupSleep-time.Second)
	}

	frames := rec.frameLog()
	firstNullFrame := slices.Index(frames, "heartbeat:null")
	var bootDurations []int
	for i, kind := range frames {
		if strings.HasPrefix(kind, "boot_timing:boot_duration:") {
			bootDurations = append(bootDurations, i)
		}
	}
	if len(bootDurations) != 1 || frames[bootDurations[0]] != "boot_timing:boot_duration:failed=false" || bootDurations[0] > firstNullFrame {
		t.Errorf("frames = %v, want exactly one boot_timing:boot_duration:failed=false, ahead of the first heartbeat:null", frames)
	}
}

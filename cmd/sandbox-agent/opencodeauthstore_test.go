package main

// Tests for spawnOpenCode's removal of OpenCode's persisted auth store
// (technical plan §29.4): a store an earlier boot left -- a restored
// snapshot's, holding a review requester's own ChatGPT link -- is gone
// before OpenCode starts, and what this boot delivers is all that exists.
// OpenCode itself is never run: a fake `opencode` on PATH stands in for it,
// either a shell script that records what it found and exits, or this test
// binary re-run as a health-only server that persists PUT /auth the way
// OpenCode does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/opencode"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// staleAuthStore is what a pre-change review sandbox's OpenCode persisted
// after the sandbox agent PUT its requester's link.
const staleAuthStore = `{"openai":{"type":"oauth","access":"REQUESTER-PERSONAL-TOKEN","refresh":"","expires":4102444800000,"accountId":"acct-requester"}}`

func writeStaleAuthStore(t *testing.T, dataDir string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(staleAuthStore), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestSpawnOpenCode_RemovesPersistedAuthStoreBeforeStart runs spawnOpenCode
// with a probing fake `opencode` that records whether the store it would
// read exists as it starts, then exits (so Spawn itself fails, after the
// removal under test).
func TestSpawnOpenCode_RemovesPersistedAuthStoreBeforeStart(t *testing.T) {
	// Not t.Parallel(): t.Setenv forbids combining the two.
	tests := []struct {
		name string
		// setup seeds the filesystem and returns the runtime env, the
		// store OpenCode reads, and a store it does not read (or "").
		setup func(t *testing.T) (runtimeEnv []string, readStore, otherStore string)
	}{
		{
			name: "a store under the runtime home is removed",
			setup: func(t *testing.T) ([]string, string, string) {
				home := t.TempDir()
				return []string{"HOME=" + home}, writeStaleAuthStore(t, filepath.Join(home, ".local", "share")), ""
			},
		},
		{
			name: "XDG_DATA_HOME wins over HOME, as OpenCode reads it; the store it does not read is left alone",
			setup: func(t *testing.T) ([]string, string, string) {
				home, data := t.TempDir(), t.TempDir()
				other := writeStaleAuthStore(t, filepath.Join(home, ".local", "share"))
				return []string{"HOME=" + home, "XDG_DATA_HOME=" + data}, writeStaleAuthStore(t, data), other
			},
		},
		{
			name: "the last HOME wins, as the process reads it",
			setup: func(t *testing.T) ([]string, string, string) {
				first, last := t.TempDir(), t.TempDir()
				other := writeStaleAuthStore(t, filepath.Join(first, ".local", "share"))
				return []string{"HOME=" + first, "HOME=" + last}, writeStaleAuthStore(t, filepath.Join(last, ".local", "share")), other
			},
		},
		{
			name: "no store is nothing to remove",
			setup: func(t *testing.T) ([]string, string, string) {
				home := t.TempDir()
				return []string{"HOME=" + home}, filepath.Join(home, ".local", "share", "opencode", "auth.json"), ""
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runtimeEnv, readStore, otherStore := tc.setup(t)
			probe := installProbingOpenCode(t, readStore)

			_, err := spawnOpenCode(context.Background(), supervisor.New(), t.TempDir(), []string{"ANTHROPIC_API_KEY=sk-deployment-key"}, runtimeEnv, nil, 5*time.Second, 50*time.Millisecond)
			if err == nil {
				t.Fatal("spawnOpenCode = nil error, want the fake's own failure to become healthy")
			}
			if strings.Contains(err.Error(), "auth store") {
				t.Fatalf("spawnOpenCode failed removing the store: %v", err)
			}

			got := readProbe(t, probe)
			if got["STORE"] != "ABSENT" {
				t.Errorf("the store OpenCode reads was %s when it started, want ABSENT", got["STORE"])
			}
			if got["ANTHROPIC_API_KEY"] != "sk-deployment-key" {
				t.Errorf("delivered api key as OpenCode saw it = %q, want it delivered", got["ANTHROPIC_API_KEY"])
			}
			if _, err := os.Stat(readStore); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stat %s = %v, want it gone", readStore, err)
			}
			if otherStore != "" {
				if _, err := os.Stat(otherStore); err != nil {
					t.Errorf("stat %s = %v, want the store OpenCode does not read left alone", otherStore, err)
				}
			}
		})
	}
}

// TestSpawnOpenCode_RefusesAStorePathThroughASymlink plants a symlink where
// the runtime home's data directory is expected: the removal must neither
// follow it (removing a file elsewhere as root) nor start OpenCode over a
// store it could not clear.
func TestSpawnOpenCode_RefusesAStorePathThroughASymlink(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	target := writeStaleAuthStore(t, filepath.Join(elsewhere, "share"))
	if err := os.Symlink(elsewhere, filepath.Join(home, ".local")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}
	probe := installProbingOpenCode(t, target)

	_, err := spawnOpenCode(context.Background(), supervisor.New(), t.TempDir(), nil, []string{"HOME=" + home}, nil, 5*time.Second, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "auth store") {
		t.Fatalf("spawnOpenCode error = %v, want a refusal to remove the store through a symlink", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("stat %s = %v, want the symlink's target untouched", target, err)
	}
	if _, err := os.Stat(probe); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat probe = %v, want OpenCode never started", err)
	}
}

// TestSpawnOpenCode_OnlyThisBootsDeliveriesRemain boots a healthy fake
// OpenCode over a store an earlier boot left, then delivers this boot's
// oauth credential through the real adapter call run() makes: the store
// holds exactly what this boot delivered, and the api key is in the
// process's environment.
func TestSpawnOpenCode_OnlyThisBootsDeliveriesRemain(t *testing.T) {
	home := t.TempDir()
	store := writeStaleAuthStore(t, filepath.Join(home, ".local", "share"))

	binDir := t.TempDir()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	script := "#!/bin/sh\nexec \"$NARVI_TEST_FAKE_OPENCODE_BIN\" -test.run='^TestFakeOpenCodeServer$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	probe := filepath.Join(t.TempDir(), "probe")
	t.Setenv("PATH", binDir)
	t.Setenv("NARVI_TEST_FAKE_OPENCODE", "1")
	t.Setenv("NARVI_TEST_FAKE_OPENCODE_BIN", self)
	t.Setenv("PROBE_FILE", probe)
	t.Setenv("XDG_DATA_HOME", "")

	sup := supervisor.New()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.StopAll(stopCtx, 2*time.Second)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := spawnOpenCode(ctx, sup, t.TempDir(), []string{"ANTHROPIC_API_KEY=sk-deployment-key"}, []string{"HOME=" + home}, nil, 60*time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("spawnOpenCode: %v", err)
	}
	if got := readProbe(t, probe); got["STORE"] != "ABSENT" || got["ANTHROPIC_API_KEY"] != "sk-deployment-key" {
		t.Fatalf("OpenCode started with store %s and api key %q, want ABSENT and the delivered key", got["STORE"], got["ANTHROPIC_API_KEY"])
	}

	adapter := opencode.New(result.BaseURL, 5*time.Second, 50*time.Millisecond, 5*time.Second, 5*time.Second, 10*time.Millisecond, result.Version, "sandbox-auth-store-test")
	defer adapter.Close()
	if err := adapter.SetOAuthAuth(ctx, "openai", opencode.OAuthCredential{Access: "THIS-BOOTS-DELIVERY", Expires: 4102444800000, AccountID: "acct-owner"}); err != nil {
		t.Fatalf("SetOAuthAuth: %v", err)
	}

	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var entries map[string]struct {
		Type   string `json:"type"`
		Access string `json:"access"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode store %s: %v", raw, err)
	}
	if len(entries) != 1 || entries["openai"].Access != "THIS-BOOTS-DELIVERY" {
		t.Errorf("store = %s, want only this boot's openai delivery", raw)
	}
	if strings.Contains(string(raw), "REQUESTER-PERSONAL-TOKEN") {
		t.Errorf("store = %s, still holds the earlier boot's link", raw)
	}
}

// installProbingOpenCode puts a fake `opencode` on PATH that records, in a
// probe file it returns the path of, whether store existed as it started
// and which api key it was given, then exits 1.
func installProbingOpenCode(t *testing.T, store string) string {
	t.Helper()
	binDir := t.TempDir()
	probe := filepath.Join(t.TempDir(), "probe")
	script := "#!/bin/sh\n" +
		`if [ -e "$STORE_PATH" ]; then echo "STORE=PRESENT" > "$PROBE_FILE"; else echo "STORE=ABSENT" > "$PROBE_FILE"; fi` + "\n" +
		`echo "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY:-}" >> "$PROBE_FILE"` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("PROBE_FILE", probe)
	t.Setenv("STORE_PATH", store)
	// Empty, as OpenCode reads it, so only the env each case hands
	// spawnOpenCode decides where the store is -- never this test
	// process's own isolated XDG_DATA_HOME.
	t.Setenv("XDG_DATA_HOME", "")
	return probe
}

func readProbe(t *testing.T, probe string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			out[name] = value
		}
	}
	return out
}

// TestFakeOpenCodeServer is not a test: it is the health-only fake OpenCode
// TestSpawnOpenCode_OnlyThisBootsDeliveriesRemain runs, this test binary
// re-executed as `opencode serve --port N ...`. It records what it started
// with, answers the two health reads Spawn makes, and persists PUT /auth/{providerID} into
// its data directory's opencode/auth.json the way OpenCode does. Run any
// other way, it returns at once.
func TestFakeOpenCodeServer(t *testing.T) {
	if os.Getenv("NARVI_TEST_FAKE_OPENCODE") != "1" {
		return
	}
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(os.Getenv("HOME"), ".local", "share")
	}
	store := filepath.Join(dataDir, "opencode", "auth.json")
	state := "ABSENT"
	if _, err := os.Stat(store); err == nil {
		state = "PRESENT"
	}
	probe := fmt.Sprintf("STORE=%s\nANTHROPIC_API_KEY=%s\n", state, os.Getenv("ANTHROPIC_API_KEY"))
	if err := os.WriteFile(os.Getenv("PROBE_FILE"), []byte(probe), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	port := ""
	for i, arg := range os.Args {
		if arg == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"healthy":true,"version":"fake"}`)
	}
	mux.HandleFunc("GET /api/health", health)
	mux.HandleFunc("GET /global/health", health)
	mux.HandleFunc("PUT /auth/{providerID}", func(w http.ResponseWriter, r *http.Request) {
		var entry json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		entries := map[string]json.RawMessage{}
		if raw, err := os.ReadFile(store); err == nil {
			_ = json.Unmarshal(raw, &entries)
		}
		entries[r.PathValue("providerID")] = entry
		raw, _ := json.Marshal(entries)
		if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(store, raw, 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "true")
	})
	_ = http.Serve(listener, mux) // until its parent stops it
}

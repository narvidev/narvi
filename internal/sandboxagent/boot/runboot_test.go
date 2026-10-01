package boot_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// freePort binds to 127.0.0.1:0, reads back the actual ephemeral port, and
// immediately closes the listener, exactly like
// internal/sandboxagent/services's own test helper of the same name (a
// separate package, so duplicated rather than shared -- these are test
// files, not production code). It is only for a service that does listen
// on the port: one that must never be reported ready takes neverBoundPort.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// neverBoundPort is the readiness port of every service here that exits
// without listening, duplicated from internal/sandboxagent/services' own
// test constant of the same name, which explains it in full. A port
// freePort released can be handed to another socket the moment it is
// closed, another parallel test's own freePort listener included; the
// readiness dial then reaches that stranger before the exit is reaped, and
// a crashed primary is reported ready. Port 1 lies below every ephemeral
// range, so the kernel never hands it out.
const neverBoundPort = 1

// writeServicesManifest writes a .narvi/services.yml under repoDir with
// the given raw content.
func writeServicesManifest(t *testing.T, repoDir, content string) {
	t.Helper()

	dir := filepath.Join(repoDir, ".narvi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// fastReadyManifestYAML is a one-service manifest whose service opens a
// real TCP listener on port almost immediately -- a YAML block scalar (|)
// is used for cmd specifically so the embedded shell/python quoting needs
// no YAML-level escaping at all.
func fastReadyManifestYAML(name string, port int, criticality string) string {
	return fmt.Sprintf(`
services:
  - name: %s
    cmd: |
      python3 -c "import socket;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1', %d));s.listen(1);import time;time.sleep(30)"
    readiness: { port: %d }
    criticality: %s
`, name, port, port, criticality)
}

// crashingManifestYAML is a one-service manifest whose service exits
// immediately -- used to exercise the fatal (primary) service-failure
// path.
func crashingManifestYAML(name string, port int, criticality string) string {
	return fmt.Sprintf(`
services:
  - name: %s
    cmd: "exit 1"
    readiness: { port: %d }
    criticality: %s
`, name, port, criticality)
}

func noopReporter(services.BootProgressEvent) {}

// noopHookRerunTiming is boot.OnHookRerunTiming's (§33.3) own noopReporter-
// style no-op, for every test in this package that does not care to
// observe it -- see hooks_test.go's own TestRunHooks_RelaysHookRerunTiming
// for the test that DOES.
func noopHookRerunTiming(_, _, _ string, _, _ bool, _ float64) {}

// collectingReporter is a ProgressReporter that records every event
// received, safe for concurrent use.
type collectingReporter struct {
	mu     sync.Mutex
	events []services.BootProgressEvent
}

func (c *collectingReporter) report(e services.BootProgressEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collectingReporter) hasPhase(name string, phase services.Phase) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.ServiceName == name && e.Phase == phase {
			return true
		}
	}
	return false
}

const (
	testReadinessTimeout      = 5 * time.Second
	testReadinessPollInterval = 30 * time.Millisecond
)

// TestRunBoot_MixedManifestAndHookFallback proves a single RunBoot call
// correctly processes one repo via services.yml supervision and a
// SIBLING repo (same call) via the classic setup.sh/start.sh fallback,
// exactly as §14.2 requires ("backward compatible, no forced migration").
func TestRunBoot_MixedManifestAndHookFallback(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()

	// repo-a: a services.yml-driven repo.
	repoADir := filepath.Join(workspaceDir, "repo-a")
	if err := os.MkdirAll(repoADir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	port := freePort(t)
	writeServicesManifest(t, repoADir, fastReadyManifestYAML("web", port, "primary"))

	// repo-b: no manifest at all -- falls back to start.sh.
	repoBMarker := filepath.Join(workspaceDir, "repo-b-marker")
	writeScript(t, filepath.Join(workspaceDir, "repo-b", "start.sh"), "touch "+repoBMarker)

	sup := supervisor.New()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.StopAll(ctx, time.Second)
	})

	reporter := &collectingReporter{}
	repos := []boot.RepoInfo{
		{Name: "repo-a", Primary: true},
		{Name: "repo-b", Primary: false},
	}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil,
		nil, nil, reporter.report, noopHookRerunTiming, 5*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileExists(t, repoBMarker)

	if !reporter.hasPhase("web", services.PhaseReady) {
		t.Errorf("reporter never observed PhaseReady for repo-a's %q service; events: %+v", "web", reporter.events)
	}
}

// TestRunBoot_AbsentManifestFallsBackToHooks proves a single repo with no
// .narvi/services.yml at all still runs its start.sh via the classic hook
// path, unchanged from §6.4's own behavior.
func TestRunBoot_AbsentManifestFallsBackToHooks(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	marker := filepath.Join(workspaceDir, "marker-start")
	writeScript(t, filepath.Join(workspaceDir, "repo-a", "start.sh"), "touch "+marker)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil,
		nil, nil, noopReporter, noopHookRerunTiming, 5*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("RunBoot(, nil) error = %v, want nil", err)
	}

	assertFileExists(t, marker)
}

// TestRunBoot_MalformedManifestIsAFatalError proves a present-but-invalid
// services.yml (here: an empty services list, per
// servicemanifest.EmptyServicesError) is a real, propagated error -- NOT
// silently treated as absent and falling back to hooks. A start.sh sitting
// right next to the malformed manifest must never run.
func TestRunBoot_MalformedManifestIsAFatalError(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo-a")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	writeServicesManifest(t, repoDir, "services: []\n")

	wouldRunMarker := filepath.Join(workspaceDir, "would-run-marker")
	writeScript(t, filepath.Join(repoDir, "start.sh"), "touch "+wouldRunMarker)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil,
		nil, nil, noopReporter, noopHookRerunTiming, 5*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err == nil {
		t.Fatal("RunBoot(, nil) error = nil, want an error for a malformed services.yml")
	}

	assertFileAbsent(t, wouldRunMarker)
}

// TestRunBoot_FatalFailureInRepoAStopsBeforeRepoB proves a fatal failure
// (a primary service that crashes before ever becoming ready) in repo-a
// stops RunBoot immediately -- repo-b, in the SAME call, is never even
// attempted. Uses the same marker-file technique as
// TestRunHooks_FatalFailureStopsImmediately.
func TestRunBoot_FatalFailureInRepoAStopsBeforeRepoB(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()

	repoADir := filepath.Join(workspaceDir, "repo-a")
	if err := os.MkdirAll(repoADir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeServicesManifest(t, repoADir, crashingManifestYAML("crashes", neverBoundPort, "primary"))

	laterMarker := filepath.Join(workspaceDir, "repo-b-marker")
	writeScript(t, filepath.Join(workspaceDir, "repo-b", "start.sh"), "touch "+laterMarker)

	sup := supervisor.New()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.StopAll(ctx, time.Second)
	})

	repos := []boot.RepoInfo{
		{Name: "repo-a", Primary: true},
		{Name: "repo-b", Primary: false},
	}

	err := boot.RunBoot(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeFresh, nil,
		nil, nil, noopReporter, noopHookRerunTiming, 5*time.Second, time.Second, testReadinessTimeout, testReadinessPollInterval, time.Millisecond, nil, nil)
	if err == nil {
		t.Fatal("RunBoot(, nil) error = nil, want a fatal error (repo-a's primary service crashed)")
	}

	assertFileAbsent(t, laterMarker)
}

// TestRunBoot_ChownsBeforeStartingServices pins an ordering that is not
// cosmetic.
//
// services.Run drops each services.yml command to the runtime uid, but
// every writer before it — gitclone, the setup hooks — runs as
// sandbox-agent. Without a chown in between, those commands land in a
// root-owned tree they can read and not write, so an ordinary services.yml
// entry that writes inside its own checkout (a dev server's build cache,
// a watcher, a code generator) fails with EACCES. The chown used to run
// only after the whole boot.
func TestRunBoot_ChownsBeforeStartingServices(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A .narvi/services.yml makes RunBoot take the services branch. The
	// service itself exits immediately; this test observes ORDERING, not
	// readiness.
	writeServicesManifest(t, repoDir, crashingManifestYAML("web", neverBoundPort, "secondary"))

	var order []string
	chown := func(got string) error {
		if got != repoDir {
			t.Errorf("chownWorkspace got %q, want the repo dir %q", got, repoDir)
		}
		order = append(order, "chown")
		return nil
	}

	sup := supervisor.New()
	t.Cleanup(func() { _ = sup.StopAll(context.Background(), time.Second) })

	_ = boot.RunBoot(context.Background(), sup, dir,
		[]boot.RepoInfo{{Name: "repo1"}}, sandboxboot.BootModeFresh,
		map[string]bool{}, map[string]boot.SetupRerunLadder{}, nil,
		func(e services.BootProgressEvent) { order = append(order, "service:"+string(e.Phase)) },
		func(_, _, _ string, _, _ bool, _ float64) {},
		time.Second, time.Second, 2*time.Second, 20*time.Millisecond, time.Millisecond,
		nil, chown)

	if len(order) == 0 {
		t.Fatal("neither the chown nor any service phase ran; this test cannot observe the ordering it exists to pin")
	}
	if order[0] != "chown" {
		t.Errorf("first event = %q, want \"chown\": the workspace must be re-owned BEFORE any dropped services.yml command starts, or it cannot write to its own checkout (order = %v)", order[0], order)
	}
}

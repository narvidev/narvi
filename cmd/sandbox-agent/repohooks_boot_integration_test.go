//go:build integration

// Technical plan §6.4, §27.1, §30.4: a pull request's review session whose
// head is not in its base repository boots without the repository's hooks,
// so code the pull request brings never runs at boot. This file runs the
// real boot path -- runBootSequence, in-process, cloning (fresh, build) or
// syncing (repo_image, snapshot_restore) real repositories served by this
// package's git-http-backend harness -- for each kind of session, the
// SESSION_CONFIG flag computed by the same rule the control plane applies
// (reposource.ReviewSessionSkipsRepoHooks): every hook writes a marker
// file, and a session that skips them leaves none, in every boot mode.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// hookMarkers are the marker files the test repositories' hooks write.
var hookMarkers = []string{"setup-ran", "start-ran", "sync-ran", "service-ran"}

// seedHookRepos serves two repositories: "app", whose setup.sh, start.sh
// and sync.sh each write a marker into markerDir, and "svc", whose
// .narvi/services.yml declares one service that writes a marker and only
// then listens on servicePort, its readiness -- so a boot that started it
// has its marker. It returns the server's base URL.
func seedHookRepos(t *testing.T, markerDir string, servicePort int) string {
	t.Helper()
	reposParent := t.TempDir()
	commit := func(name string, files map[string]string) {
		t.Helper()
		bare := filepath.Join(reposParent, name+".git")
		if err := os.MkdirAll(bare, 0o755); err != nil {
			t.Fatal(err)
		}
		mustRunGit(t, reposParent, "init", "--bare", "-b", "main", bare)
		seed := t.TempDir()
		mustRunGit(t, reposParent, "clone", bare, seed)
		for path, content := range files {
			full := filepath.Join(seed, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
			mustRunGit(t, seed, "add", path)
		}
		mustRunGit(t, seed, "commit", "-m", "seed")
		mustRunGit(t, seed, "push", "origin", "main")
	}
	hook := func(marker string) string {
		return "#!/bin/sh\ntouch \"" + filepath.Join(markerDir, marker) + "\"\n"
	}
	commit("app", map[string]string{
		"README.md": "app\n",
		"setup.sh":  hook("setup-ran"),
		"start.sh":  hook("start-ran"),
		"sync.sh":   hook("sync-ran"),
	})
	commit("svc", map[string]string{
		"README.md": "svc\n",
		".narvi/services.yml": fmt.Sprintf("services:\n  - name: marker\n    cmd: %q\n    readiness:\n      port: %d\n    criticality: primary\n",
			fmt.Sprintf(`touch '%s' && exec python3 -c "import socket,time;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1', %d));s.listen(1);time.sleep(30)"`,
				filepath.Join(markerDir, "service-ran"), servicePort), servicePort),
	})
	return startGitHTTPServer(t, reposParent).URL
}

// bootWithRepoHooks runs one real boot of the two repositories in mode,
// with SESSION_CONFIG's skipRepoHooks set to skip. A warm mode syncs a
// workspace a build left, as a repo image or a snapshot holds it.
func bootWithRepoHooks(t *testing.T, mode sandboxboot.BootMode, serverURL string, skip bool) error {
	t.Helper()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "app", Url: serverURL + "/app.git"},
		{Name: "svc", Url: serverURL + "/svc.git"},
	}
	if mode == sandboxboot.BootModeRepoImage || mode == sandboxboot.BootModeSnapshotRestore {
		for _, r := range repos {
			mustRunGit(t, workspaceDir, "clone", r.Url, r.Name)
		}
	}

	sup := supervisor.New()
	t.Cleanup(func() { _ = sup.StopAll(context.Background(), platform.DefaultTimeouts().ProcessStopGracePeriod) })
	cfg := boot.Config{
		BootMode:           mode,
		WorkspaceDir:       workspaceDir,
		CredentialCacheDir: t.TempDir(),
		RuntimeUID:         uint32(os.Getuid()),
		RuntimeGID:         uint32(os.Getgid()),
		SessionConfig: &sessionconfig.SessionConfig{
			BootMode:          sessionconfig.SessionConfigBootMode(mode),
			ControlPlaneWsUrl: "wss://unused.invalid/ws",
			Gen:               1,
			Repos:             repos,
			SandboxId:         "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
			SandboxToken:      "test-sandbox-token",
			SessionId:         "repo-hooks-test-session",
			SkipRepoHooks:     skip,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootSequenceTestTimeout)
	defer cancel()
	noopProgress := func(services.BootProgressEvent) {}
	noopGitSync := func(string, string, string) {}
	noopFetchTiming := func(string, float64, bool) {}
	noopCheckoutTiming := func(string, float64, bool) {}
	noopHookTiming := func(string, string, string, bool, bool, float64) {}
	return runBootSequence(ctx, sup, cfg, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, platform.DefaultTimeouts(), nil, nil,
		noopProgress, noopGitSync, noopFetchTiming, noopCheckoutTiming, noopHookTiming)
}

// TestRunBootSequence_AReviewSessionWhoseHeadIsNotInItsBaseRunsNoRepositoryHook:
// in every boot mode, a fork's pull request's review session runs none of
// the repository's hooks and starts none of its services -- as does every
// session the control plane tells to skip them, a claim nothing has
// recorded included (TestReviewSessionSkipsRepoHooks); a same-repository
// pull request's review session, and a session with no pull request, run
// what the mode runs.
func TestRunBootSequence_AReviewSessionWhoseHeadIsNotInItsBaseRunsNoRepositoryHook(t *testing.T) {
	const claim = "acme/widgets"
	inBase, notInBase := true, false
	const sameRepoSpec = `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":"feature-x"}]`
	const forkSpec = `[{"name":"widgets","url":"https://github.com/acme/widgets.git","branch":null}]`
	sessions := []struct {
		name     string
		skip     bool
		wantSkip bool
	}{
		{name: "a fork's pull request's review session", skip: reposource.ReviewSessionSkipsRepoHooks(true, claim, &notInBase, []byte(forkSpec)), wantSkip: true},
		{name: "a same-repository pull request's review session", skip: reposource.ReviewSessionSkipsRepoHooks(true, claim, &inBase, []byte(sameRepoSpec))},
		{name: "a session with no pull request", skip: reposource.ReviewSessionSkipsRepoHooks(false, "", nil, []byte(sameRepoSpec))},
	}
	modes := []struct {
		mode sandboxboot.BootMode
		// want is the markers a boot that runs the hooks leaves: setup.sh
		// in every mode but snapshot_restore (repo_image's runs again, its
		// workspace read as moved with no image manifest here), start.sh in
		// every mode but build, and the service in every mode.
		want []string
	}{
		{mode: sandboxboot.BootModeFresh, want: []string{"setup-ran", "start-ran", "service-ran"}},
		{mode: sandboxboot.BootModeBuild, want: []string{"setup-ran", "service-ran"}},
		{mode: sandboxboot.BootModeRepoImage, want: []string{"setup-ran", "start-ran", "service-ran"}},
		{mode: sandboxboot.BootModeSnapshotRestore, want: []string{"start-ran", "service-ran"}},
	}
	for _, s := range sessions {
		if s.skip != s.wantSkip {
			t.Fatalf("%s: skipRepoHooks = %v, want %v", s.name, s.skip, s.wantSkip)
		}
		for _, m := range modes {
			t.Run(s.name+", "+string(m.mode), func(t *testing.T) {
				markerDir := t.TempDir()
				serverURL := seedHookRepos(t, markerDir, freeTCPPort(t))

				if err := bootWithRepoHooks(t, m.mode, serverURL, s.skip); err != nil {
					t.Fatalf("runBootSequence(%s) = %v, want nil", m.mode, err)
				}
				want := map[string]bool{}
				if !s.skip {
					for _, marker := range m.want {
						want[marker] = true
					}
				}
				for _, marker := range hookMarkers {
					_, err := os.Stat(filepath.Join(markerDir, marker))
					if got := err == nil; got != want[marker] {
						t.Errorf("%s after a %s boot: present %v, want %v", marker, m.mode, got, want[marker])
					}
				}
			})
		}
	}
}

// freeTCPPort is a port nothing listens on now, for the service to bind.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

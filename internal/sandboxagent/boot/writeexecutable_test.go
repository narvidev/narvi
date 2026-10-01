package boot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// WriteExecutableForTest writes content to path as an executable (0o755),
// creating its parent directory first. Every test in this package that
// writes a script goes through it, the external boot_test package
// included -- which is why it is exported from a _test.go file.
//
// It holds syscall.ForkLock's read side from before the file is opened
// until after it is closed. That closes Go's fork/exec race over a
// freshly written script (golang/go#22315). The tests in this package run
// in parallel, and they fork all the time: hooks, git, the docker fakes.
// A fork that lands while one test's write descriptor is still open
// copies that descriptor into the child, and the child holds it until it
// execs. Exec'ing the script during that window fails with ETXTBSY,
// because the kernel still sees a writer. The failing test is then not
// the one that forked, and its own script was written correctly.
//
// Fork takes ForkLock's write side, so no fork can start while the
// descriptor is open. A child forked before the open never had it, and a
// child forked after the close never sees it. That is why the guard sits
// at the writer: the fork can come from any parallel test in the binary,
// so no single exec site could close the window.
//
// A retry at the exec site is no substitute. The one this package had
// (waitExecutable, for the docker fakes) found out whether exec worked by
// running the script. For a hook, that runs its body before RunHooks
// does. For a docker fake, it ran the fake's 30-second sleep, and it
// created the socket before RunDocker began to wait for it.
func WriteExecutableForTest(t *testing.T, path string, content []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}

	syscall.ForkLock.RLock()
	err := os.WriteFile(path, content, 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

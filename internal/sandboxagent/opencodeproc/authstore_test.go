package opencodeproc_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/narvidev/narvi/internal/sandboxagent/opencodeproc"
)

func TestAuthStoreFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		env    []string
		want   string
		wantOK bool
	}{
		{"HOME only", []string{"PATH=/bin", "HOME=/home/rt"}, "/home/rt/.local/share/opencode/auth.json", true},
		{"XDG_DATA_HOME wins over HOME", []string{"HOME=/home/rt", "XDG_DATA_HOME=/data"}, "/data/opencode/auth.json", true},
		{"XDG_DATA_HOME wins whatever the order", []string{"XDG_DATA_HOME=/data", "HOME=/home/rt"}, "/data/opencode/auth.json", true},
		{"an empty XDG_DATA_HOME falls back to HOME", []string{"XDG_DATA_HOME=/data", "HOME=/home/rt", "XDG_DATA_HOME="}, "/home/rt/.local/share/opencode/auth.json", true},
		{"the last HOME wins", []string{"HOME=/root", "HOME=/home/rt"}, "/home/rt/.local/share/opencode/auth.json", true},
		{"neither is no store", []string{"PATH=/bin", "MALFORMED"}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, ok := opencodeproc.AuthStoreFor(tc.env)
			if ok != tc.wantOK || (ok && store.Path() != tc.want) {
				t.Errorf("AuthStoreFor(%v) = (%q, %v), want (%q, %v)", tc.env, store.Path(), ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestAuthStore_Remove covers what Remove finds at the store's path: a
// file is removed; nothing is nothing to remove; a symlink as the file
// itself is removed as a link, its target untouched; a symlink or a file
// where a directory is expected is refused.
func TestAuthStore_Remove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup builds home and returns a file that must survive (or "").
		setup       func(t *testing.T, home string) string
		wantRemoved bool
		wantErr     bool
	}{
		{"a store file is removed", func(t *testing.T, home string) string {
			writeFile(t, filepath.Join(home, ".local", "share", "opencode", "auth.json"))
			return ""
		}, true, false},
		{"no data directory is nothing to remove", func(_ *testing.T, _ string) string { return "" }, false, false},
		{"a store that is a symlink is removed as a link", func(t *testing.T, home string) string {
			target := filepath.Join(t.TempDir(), "elsewhere.json")
			writeFile(t, target)
			dir := filepath.Join(home, ".local", "share", "opencode")
			mkdir(t, dir)
			if err := os.Symlink(target, filepath.Join(dir, "auth.json")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			return target
		}, true, false},
		{"a symlinked directory on the way is refused", func(t *testing.T, home string) string {
			elsewhere := t.TempDir()
			target := filepath.Join(elsewhere, "opencode", "auth.json")
			writeFile(t, target)
			mkdir(t, filepath.Join(home, ".local"))
			if err := os.Symlink(elsewhere, filepath.Join(home, ".local", "share")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			return target
		}, false, true},
		{"a file where a directory is expected is refused", func(t *testing.T, home string) string {
			writeFile(t, filepath.Join(home, ".local"))
			return ""
		}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			survivor := tc.setup(t, home)
			store, ok := opencodeproc.AuthStoreFor([]string{"HOME=" + home})
			if !ok {
				t.Fatal("AuthStoreFor = false, want a store")
			}
			removed, err := store.Remove()
			if removed != tc.wantRemoved || (err != nil) != tc.wantErr {
				t.Fatalf("Remove() = (%v, %v), want removed %v, error %v", removed, err, tc.wantRemoved, tc.wantErr)
			}
			if tc.wantRemoved {
				if _, err := os.Lstat(store.Path()); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("lstat %s = %v, want gone", store.Path(), err)
				}
			}
			if survivor != "" {
				if _, err := os.Stat(survivor); err != nil {
					t.Errorf("stat %s = %v, want it untouched", survivor, err)
				}
			}
		})
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

package gitdir

import (
	"fmt"
	"os"
	"syscall"
)

// EnsureRoot creates root (the sandbox-wide git-dir root, boot.Config.
// GitDirRoot, default /var/lib/narvi/gitdirs -- deliberately root-owned
// and NOT world-writable, unlike /tmp) if it does not already exist, then
// asserts it is genuinely ours: a real directory, owned by this process's
// own uid, with no group- or other-write bit set.
//
// Mirrors internal/sandboxagent/credentials/cache.go's own assertDirIsOurs
// (~lines 77-106) precisely, and for the identical reason: os.MkdirAll is
// a no-op when the directory already exists, whatever its owner or mode,
// so the ownership/mode check is the entire point, not a redundant
// belt-and-braces afterthought. Unlike the credential cache's own default
// (under /tmp, world-writable), this root's own default sits under
// /var/lib/narvi -- but a caller-overridden root, or a first boot racing
// an unexpected pre-existing mount, could still hand this a directory the
// runtime (or anything else) already created and controls. A directory
// that is not ours, or not ours alone, is a hard failure -- FAIL BOOT --
// rather than something to repair: repairing it would race whatever
// created it, and a git-dir root that cannot be trusted must not be used
// to seed every later repository's own agent-owned config.
func EnsureRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("gitdir: create root %s: %w", root, err)
	}
	return assertDirIsOurs(root)
}

// assertRealDir fails unless path is Lstat-able and is a real directory,
// never a symlink -- shared by Seed (step 0: refuse a symlinked worktree
// or worktree .git before ever seeding anything) and Run (the same guard,
// re-checked on EVERY spawn -- see Run's own doc comment for why a
// symlink swap happening strictly BETWEEN two Seed/Run calls must still
// be caught, which is why Seed's own one-time check at seed time is not
// sufficient on its own).
func assertRealDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, refusing to trust it", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// assertDirIsOurs fails unless dir is a real directory (not a symlink),
// owned by this process's own uid, with no group or other write bit set.
// See EnsureRoot's own doc comment for why this exists and why it fails
// closed rather than repairing.
func assertDirIsOurs(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("gitdir: stat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("gitdir: %s is a symlink, refusing to trust it as the git-dir root", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("gitdir: %s is not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("gitdir: cannot read ownership of %s", dir)
	}
	if uint64(stat.Uid) != uint64(os.Getuid()) {
		return fmt.Errorf("gitdir: %s is owned by uid %d, not this process (uid %d); refusing to seed agent git-dirs under a directory another user controls", dir, stat.Uid, os.Getuid())
	}
	// The WRITE bits, not every bit -- same reasoning as credentials/
	// cache.go's own identical check: group- or other-writable lets
	// another uid create, replace, or symlink an entry under this root,
	// which is how an agent-owned config file ends up written wherever an
	// attacker chose instead. Group- or other-readable is not the same
	// problem and is not rejected here.
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("gitdir: %s has mode %#o; refusing to seed agent git-dirs under a directory group or others can write to", dir, perm)
	}
	return nil
}

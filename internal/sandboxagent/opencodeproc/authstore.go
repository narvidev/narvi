package opencodeproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// authStoreFile is the file OpenCode keeps its provider auth in, under its
// data directory: `PUT /auth/{providerID}` persists there, and OpenCode
// treats any provider with an entry in it as connected (technical plan
// §29.1).
const authStoreFile = "auth.json"

// AuthStore is where the OpenCode process a given environment starts keeps
// its provider auth: $XDG_DATA_HOME/opencode/auth.json, or
// $HOME/.local/share/opencode/auth.json when XDG_DATA_HOME is unset or
// empty -- OpenCode's own resolution of its data directory.
//
// The sandbox agent writes nothing there itself except the oauth-kind
// credentials the control plane delivered on the current boot (PUT
// /auth/{providerID}, after the process is healthy); api-kind credentials
// reach OpenCode as environment variables and never touch the file. A
// store left from an earlier boot -- restored with a snapshot's
// filesystem, say -- can therefore only hold credentials that boot was
// delivered and this one may not be: a pull request's review session,
// before its user scope was withheld, was delivered its requester's own
// ChatGPT link. Removing it before OpenCode starts leaves exactly what this
// boot delivers.
type AuthStore struct {
	// base is the data directory's root as the environment names it
	// ($XDG_DATA_HOME, or $HOME), opened following symlinks.
	base string
	// rel are the directories below base, walked without following one.
	rel []string
}

// AuthStoreFor resolves the auth store of an OpenCode process started with
// env, read the way the process reads it: for a name set more than once,
// the last value wins (os/exec keeps the last of duplicate keys). false
// when env names neither XDG_DATA_HOME nor HOME, so OpenCode has no data
// directory to persist into.
func AuthStoreFor(env []string) (AuthStore, bool) {
	var home, dataHome string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch name {
		case "HOME":
			home = value
		case "XDG_DATA_HOME":
			dataHome = value
		}
	}
	switch {
	case dataHome != "":
		return AuthStore{base: dataHome, rel: []string{"opencode"}}, true
	case home != "":
		return AuthStore{base: home, rel: []string{".local", "share", "opencode"}}, true
	default:
		return AuthStore{}, false
	}
}

// Path is the auth store file's path, for logs and tests.
func (s AuthStore) Path() string {
	return filepath.Join(append(append([]string{s.base}, s.rel...), authStoreFile)...)
}

// Remove deletes the auth store file, reporting whether one existed.
//
// The runtime owns its home and everything under it, so it can plant a
// symlink where a directory is expected, and the sandbox agent calling
// this is root in production. So only base is resolved as named; each
// directory below it is opened O_NOFOLLOW relative to its parent, and the
// file is unlinked relative to the last one, which never follows a final
// symlink either (the link itself would be removed). A symlink or a
// non-directory on the way is refused with an error rather than followed
// or stepped around: the caller does not start OpenCode over a store it
// could not clear. A path that does not exist, at any level, is nothing to
// remove.
func (s AuthStore) Remove() (bool, error) {
	dir, err := os.Open(s.base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("opencodeproc: open auth store base %s: %w", s.base, err)
	}
	defer func() { _ = dir.Close() }()

	for _, name := range s.rel {
		next, err := openDirNoFollow(int(dir.Fd()), name)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return false, nil
			}
			return false, fmt.Errorf("opencodeproc: auth store path %s: %s is not a plain directory: %w", s.Path(), name, err)
		}
		_ = dir.Close()
		dir = next
	}

	for {
		err := unix.Unlinkat(int(dir.Fd()), authStoreFile, 0)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOENT):
			return false, nil
		default:
			return false, fmt.Errorf("opencodeproc: remove auth store %s: %w", s.Path(), err)
		}
	}
}

// openDirNoFollow opens name, relative to dirfd, as a directory, failing
// with ELOOP or ENOTDIR -- never following -- if name is a symlink or not a
// directory.
func openDirNoFollow(dirfd int, name string) (*os.File, error) {
	for {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), name), nil
	}
}

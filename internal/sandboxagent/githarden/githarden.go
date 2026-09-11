// Package githarden supplies the flags every git command sandbox-agent
// runs must carry, and the reason they are not optional.
//
// §30.5 drops the agent runtime to its own UID and hands it the workspace,
// so the runtime owns the repositories it works in -- including each
// repository's own .git directory. Sandbox-agent still runs git in those
// same repositories, as itself, to clone, sync, read a head SHA and push.
// That leaves two problems, and only the first one announces itself.
//
// The loud one: git refuses to operate on a repository owned by another
// user ("detected dubious ownership") unless that path is declared safe.
// Without this, every push and every head-sha read fails outright.
//
// The quiet one, which is why declaring the path safe is not on its own a
// fix: a repository the runtime owns is a repository the runtime can write
// hooks into. `git push` runs .git/hooks/pre-push, so declaring the path
// safe and stopping there hands a prompt-injected agent arbitrary
// execution AS SANDBOX-AGENT -- recovering precisely the identity §30.5
// exists to take away. Verified rather than reasoned: in a container, a
// runtime-planted pre-push hook printed "HOOK RAN as uid=0" under a root
// push with only safe.directory set, and did not run once hooksPath was
// pointed away.
//
// So both go together, always, at every call site -- which is why they
// live in one function instead of being remembered eight times. Nothing
// sandbox-agent does with git ever wants a repository-authored hook or a
// repository-authored configuration value; those exist for humans working
// in their own checkouts.
package githarden

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// noHooksPath points git at a location that holds no hooks and cannot be
// made to hold any. /dev/null is not a directory, so every hook lookup
// under it simply finds nothing -- no directory to create, no directory
// anyone could later write into, and no ordering question about who
// created it first.
const noHooksPath = "/dev/null"

// Args returns git's own arguments for a command operating on repoDir,
// with the hardening ahead of whatever the caller wants to run.
//
// Callers pass what they would have passed anyway; the flags this adds
// come first because git requires its -c options before the subcommand.
// hardeningFlags is the ONE list, used by both entry points below.
//
// It was two lists, and that is a defect waiting to happen: a key added
// to one and forgotten in the other leaves a live hole reachable through
// whichever call site was missed. The phase audit found credential.helper
// missing from both, which is the cheaper version of the same mistake.
//
// Every entry names a git config key that (a) makes git RUN a command and
// (b) is settable from the repository's own .git/config -- which the
// agent runtime owns after the workspace chown, and which a
// prompt-injected agent can therefore write.
//
// This list is the guarantee. It is NOT complete for all time: git adds
// config keys, and one class is deliberately not covered below.
func hardeningFlags(repoDir string) []string {
	return []string{
		// The repository is owned by the runtime, not by this process.
		// Scoped to this exact path rather than the wildcard: a wildcard
		// would also cover any other foreign-owned repository this
		// process is ever pointed at, including one it did not create.
		"-c", "safe.directory=" + repoDir,

		// A repository-authored hook must never execute as this process.
		"-c", "core.hooksPath=" + noHooksPath,

		// credential.helper is the most dangerous of these, and it was
		// missed until the phase audit. It names a command git runs, AND
		// git hands that command the credential over its own protocol --
		// so a value planted in the runtime-owned config both executes as
		// this process and exfiltrates the SCM token in one step.
		//
		// The empty value is load-bearing and is not a stylistic reset:
		// credential.helper is MULTI-VALUED, so adding Narvi's own helper
		// without this would APPEND it to whatever the repository already
		// configured rather than replacing it. An empty value discards
		// every earlier helper, which is git's own documented behaviour.
		// The caller's own -c credential.helper=... then lands after this
		// one and is the only survivor.
		"-c", "credential.helper=",

		// core.sshCommand names the program git runs for every ssh
		// transport operation.
		"-c", "core.sshCommand=",

		// diff.external replaces git's own diff with a named command.
		"-c", "diff.external=",

		// core.pager runs a command over git's output. cat, not empty: an
		// empty pager makes git fall back to its built-in default rather
		// than disabling paging.
		"-c", "core.pager=cat",

		// core.fsmonitor names a command git runs on ordinary operations.
		"-c", "core.fsmonitor=",
	}
}

// Content filters (filter.<driver>.clean / .smudge) were deliberately left
// off the list above, and NOT because they are safe -- the reason is
// worth recording precisely, because a first pass at closing this got a
// mechanism wrong in a way that mattered, and the correction is the part
// worth keeping.
//
// Unlike every key above, a filter's driver name is chosen by the
// repository's own .gitattributes ("<path> filter=<anything>"), so there
// is no fixed "filter.X.clean" key a -c entry could reset once and cover
// every case: "-c filter.*.clean=" is not a wildcard to git, it names a
// literal, useless config section called "*". git itself documents no
// flag that disables the filter mechanism wholesale (gitattributes(5)).
//
// The tempting first-pass answer -- "the UID boundary (§30.5) already
// covers this, since a repository-authored command either runs as the
// runtime already, or never runs at all" -- does not survive contact
// with what §30.5 actually grants. The runtime owns .git BECAUSE of that
// boundary, which makes writing filter.<anything>.smudge into
// .git/config, and a matching filter=<anything> into .gitattributes, an
// ORDINARY, unprivileged act for it -- not a violation of the boundary.
// The violation is what happens next: sandbox-agent's OWN later git
// invocations against that SAME repository -- internal/sandboxagent/
// gitclone's SyncAll reconciling an already-existing workspace exactly
// like a repo_image/snapshot_restore boot presents one, and its
// CleanForImageBuild running `checkout -- .` at image-bake time -- read
// that config back and would run the planted command AS SANDBOX-AGENT.
// Same shape as the pre-push-hook exploit this file's own top comment
// documents, one call removed; verified the same way, not assumed (see
// githarden_test.go and internal/sandboxagent/gitclone's own tests).
//
// The SECOND mechanism tried -- writing "* -filter" into $GIT_DIR/info/
// attributes, gitattributes(5)'s own highest-precedence attributes
// source -- and why it does NOT close this class, is recorded on
// NeutralizeFiltersBestEffort's own doc comment below, in full, because
// getting this wrong once already is exactly why the reasoning belongs
// in the code and not just in a PR description. Read it before adding a
// second call site or reasoning about what this file actually covers.
//
// CONCLUSION: this class is NOT closed by anything in this package. A
// file living inside .git is state inside a directory the runtime owns,
// which makes it racable by construction -- the same property that makes
// the filter's driver name itself unfixable by a -c entry. Closing it
// for real needs the .git ownership boundary itself to change (§30.5 no
// longer handing this exact directory to the runtime), which is out of
// this file's scope. NeutralizeFiltersBestEffort is kept anyway because
// it turns a SILENT bypass into a LOUD, fatal error for a non-racing
// attacker -- a real, if narrow, improvement -- never because it closes
// the class. Do not read its presence as a completed control.
//
// The trade-off a real fix would still need, decided here rather than
// left to be discovered by a user with a checkout full of pointer files:
// git-lfs is itself implemented as exactly this kind of content filter.
// Today that trade is free regardless of any of the above --
// deploy/sandbox-image/Dockerfile installs `git`, never `git-lfs`, so no
// repository's LFS content is materialized by sandbox-agent's own
// clone/sync either way; a repository using LFS already gets pointer
// files, not real blobs. If git-lfs is ever added to the image AND a
// real, race-free fix is ever built, that fix will need a deliberate,
// named exception for the literal driver name "lfs" -- not a silent
// regression discovered later.

// Args returns git's own arguments for a command operating on repoDir,
// with the hardening ahead of whatever the caller wants to run.
//
// Callers pass what they would have passed anyway; the flags this adds
// come first because git requires its -c options before the subcommand.
func Args(repoDir string, rest ...string) []string {
	args := append([]string{"-C", repoDir}, hardeningFlags(repoDir)...)
	return append(args, rest...)
}

// Spec builds a supervisor.Spec for a hardened git invocation. Use this
// rather than assembling one by hand: a Spec built elsewhere is a git
// command running without the flags above, and the failure mode is silent
// in one direction and destructive in the other.
func Spec(repoDir string, rest ...string) supervisor.Spec {
	return supervisor.Spec{Path: "git", Args: Args(repoDir, rest...)}
}

// Harden rewrites an already-assembled git argument list, inserting the
// flags immediately after the "-C <dir>" the caller supplied.
//
// This exists for call sites that build their arguments elsewhere and pass
// them through one shared runner: hardening the runner covers every one of
// them at once, which is the only way this stays true as callers are
// added. An argument list with no "-C <dir>" is returned unchanged and
// with no repository-scoped safe.directory -- there is no repository path
// to scope it to, and inventing a wildcard would quietly widen the very
// thing this narrows.
func Harden(args []string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" {
			repoDir := args[i+1]
			flags := hardeningFlags(repoDir)
			out := make([]string, 0, len(args)+len(flags))
			out = append(out, args[:i+2]...)
			out = append(out, flags...)
			return append(out, args[i+2:]...)
		}
	}
	return args
}

// filterAttributesOverride is written into $GIT_DIR/info/attributes.
// gitattributes(5): "the highest precedence" attributes source, ahead of
// even the repository's own tracked .gitattributes. "-filter" UNSETS the
// `filter` attribute (gitattributes(5)'s own dash-prefix syntax) for
// every path ("*"), regardless of what any lower-precedence
// .gitattributes assigns -- so no path is ever treated as having a
// filter driver at all, under any name. gitattributes(5) also documents
// what happens next: "a missing filter driver definition... is not an
// error but makes the filter a no-op passthru" -- checkout still
// succeeds, with the blob's real, unconverted content.
const filterAttributesOverride = "* -filter\n"

// NeutralizeFiltersBestEffort is a MITIGATION, not a closure, of the one
// class hardeningFlags' own -c entries cannot reach. The name says so on
// purpose -- read this whole comment before calling it, relying on it,
// trusting its return value, or reusing its shape elsewhere.
//
// The first version of this function (this Step's own history) called
// os.WriteFile(path, ...) with no O_NOFOLLOW. .git/info is a directory
// the agent runtime owns (§30.5): one unprivileged command --
// `mkdir -p .git/info && ln -sfn /dev/null .git/info/attributes` --
// made that write land on /dev/null while the function still returned
// nil. A caller checking only the error saw success; git read an empty
// attributes file; the repository's tracked filter=evil survived; the
// planted filter.evil.smudge ran as sandbox-agent on the next checkout.
// The same missing O_NOFOLLOW is separately an arbitrary-file-clobber
// primitive: point the symlink anywhere on the host and this function
// truncates it and writes "* -filter" into it, as sandbox-agent, once
// per call. Verified against real git in a container, not assumed --
// same house style as the pre-push-hook finding above.
//
// This version opens O_NOFOLLOW and fstats the descriptor it actually
// wrote to (never the path again -- a second lookup is a second race),
// refusing to trust anything that is not a plain regular file. That
// closes the SILENT half: a symlink (or any other non-regular node)
// planted at this exact path BEFORE this call runs, and left alone
// afterward, now makes this function return a real, non-nil error,
// which every call site below propagates as fatal -- never a quiet
// success on a write that landed somewhere else.
//
// It does NOT, and cannot by construction, close the RACING half: an
// adversary who unlinks and re-plants the symlink in the window between
// this call returning and the git subprocess it precedes actually
// opening the same path for itself. This function's own fstat inspects
// only the descriptor IT opened, at the moment IT opened it -- it has no
// visibility into what git's own, separate, later open() of that same
// path will find, because .git/info is a directory the runtime owns and
// can freely unlink-and-recreate at any point sandbox-agent does not
// control, including between this call's close() and the very next
// spawn's open(). Measured, not asserted: a concurrent, single, entirely
// unprivileged goroutine looping unlink+re-symlink with no artificial
// delay, no throttling, and no attempt to target the open() call
// specifically, still won a real double-digit percentage of trials
// racing this exact write-then-exec window against a real `git checkout`
// spawn (a throwaway, uncommitted reproduction -- see this comment's own
// closing note on why the measurement itself is not committed to this
// package's test suite). A more targeted racer -- multiple goroutines, or
// one that races the open() syscall specifically rather than the whole
// spawn -- should be assumed to do materially better; the qualitative
// conclusion does not depend on the exact percentage either way: ANY
// adversary-favorable, nonzero win rate on a control meant to stop
// arbitrary command execution as sandbox-agent (root, in production --
// workspaceowner.go) is a real, live bypass, not a rounding error.
// Re-asserting immediately before every spawn -- which every call site
// below already does -- does not change this: assert, runtime unlinks
// and re-symlinks (both unprivileged, in a directory it owns), re-assert
// returns nil, `git check-attr` still reports the repository's own
// filter. This measurement is deliberately NOT a committed test: its
// outcome is a probability, not a fact a table-driven assertion can pin,
// and a test whose pass/fail depends on OS scheduling is exactly the
// kind of flaky assertion this codebase's own conventions refuse to
// stabilize on a single data point.
//
// Also narrower than "every path under .git/info" might suggest: this
// only guards the FINAL path component. If the runtime replaces the
// .git/info DIRECTORY itself with a symlink before MkdirAll runs, that
// intermediate-component substitution is not caught by O_NOFOLLOW on the
// final open (which only inspects "attributes", not "info"). Not fixed
// here -- doing so needs a symlink-safe directory walk (openat-style,
// platform-specific) that would still not close the race above, so it
// would add real complexity for no improvement to the actual guarantee.
//
// repoDir's .git/info directory is created (MkdirAll) if it does not
// already exist. A failure at any step -- MkdirAll, the O_NOFOLLOW open,
// the fstat, the regular-file check, or the write itself -- is returned,
// never swallowed: every call site treats it as fatal, because proceeding
// with the git call anyway would mean running it with a write this
// function cannot vouch for.
func NeutralizeFiltersBestEffort(repoDir string) (retErr error) {
	infoDir := filepath.Join(repoDir, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return fmt.Errorf("githarden: create %s: %w", infoDir, err)
	}
	path := filepath.Join(infoDir, "attributes")

	// O_NOFOLLOW: refuse to open through a symlink planted at this exact
	// path. Without it, a symlink to /dev/null (or anywhere else) makes
	// the write below succeed silently against whatever the symlink
	// names, not against a real attributes file in this repository.
	//
	// O_NONBLOCK: found by this file's own test suite, not anticipated --
	// a FIFO planted at this exact path (mkfifo, no symlink involved, so
	// O_NOFOLLOW alone does not stop it) makes a plain O_WRONLY open
	// BLOCK INDEFINITELY until some other process opens the other end for
	// reading, which nothing here ever does. That is a denial-of-service
	// primitive worse than the bypass this function exists to fix: every
	// git command sandbox-agent would ever run against this repository
	// again hangs forever, with no timeout anywhere in this call chain to
	// save it. O_NONBLOCK makes the open itself return ENXIO immediately
	// instead of blocking when the far end names a FIFO with no reader;
	// it is a documented no-op for a plain regular file, so it changes
	// nothing about the success path.
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return fmt.Errorf("githarden: open %s (refusing to follow a symlink or block on a non-regular node planted there): %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	// A checked Close, not a discarded one: on some filesystems a write
	// failure (out of space, a quota) only surfaces at close, and this
	// function's whole point is to never report success for a write it
	// cannot vouch for. Only overwrites retErr when nothing earlier
	// already failed -- Close's own error is worth reporting, never worth
	// masking a more specific one already in hand.
	defer func() {
		if closeErr := f.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("githarden: close %s: %w", path, closeErr)
		}
	}()

	// fstat the FD this call itself opened -- not a fresh Stat(path),
	// which would be its own new race against whatever replaced path
	// since the open above. A non-regular result here (a FIFO, a device
	// node reachable some other way than a symlink, ...) is refused just
	// as loudly as the symlink case O_NOFOLLOW already catches.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("githarden: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("githarden: %s is not a regular file (mode %s) -- refusing to trust it", path, info.Mode())
	}

	if _, err := f.Write([]byte(filterAttributesOverride)); err != nil {
		return fmt.Errorf("githarden: write %s: %w", path, err)
	}
	return nil
}

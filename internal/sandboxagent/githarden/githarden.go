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
// worth recording precisely, because the audit that reopened this found
// the first-pass answer wrong.
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
// NeutralizeFilters (below) closes it. Not a -c entry -- none exists for
// an unbounded driver-name space -- but a write to $GIT_DIR/info/
// attributes, gitattributes(5)'s own highest-precedence attributes
// source, unsetting the `filter` attribute for every path so no driver
// name is ever looked up at all. internal/sandboxagent/gitclone's runGit
// calls it before every spawn, for the same "one function, not eight call
// sites" reason hardeningFlags itself exists.
//
// The trade-off, decided here rather than left to be discovered by a
// user with a checkout full of pointer files: git-lfs is itself
// implemented as exactly this kind of content filter. Today that trade
// is free -- deploy/sandbox-image/Dockerfile installs `git`, never
// `git-lfs`, so no repository's LFS content is materialized by
// sandbox-agent's own clone/sync regardless of this file; a repository
// using LFS already gets pointer files, not real blobs, from
// sandbox-agent's own checkout. If git-lfs is ever added to the image,
// this blanket unset would need a deliberate, named exception for the
// literal driver name "lfs" -- not a silent regression discovered later.

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

// NeutralizeFilters closes the one class hardeningFlags' own -c entries
// cannot reach -- see the comment above hardeningFlags for why a content
// filter's unbounded, repository-chosen driver name rules out a -c
// entry, and why "the UID boundary already covers this" is not actually
// true. Call it once for repoDir before any git command that can
// populate a working tree (checkout, stash pop, or an internal one
// either performs, such as switching branches) -- internal/sandboxagent/
// gitclone's runGit does this for every hardened invocation it makes, so
// callers routed through it never have to remember this themselves.
//
// Deliberately NOT one of hardeningFlags' -c entries: this is a file
// write, not a command-line flag, and unlike that list it must be
// RE-ASSERTED before every call that can populate a working tree,
// because the agent runtime owns .git (§30.5) and can overwrite this
// file between calls exactly as freely as it can write a filter driver
// into .git/config in the first place. A single write at clone time
// would not survive a single runtime turn.
//
// repoDir's .git/info directory is created if it does not already exist
// (a fresh clone has no reason to have written to it yet). A write
// failure is returned, never swallowed: silently proceeding without this
// in place is the exact "quiet" failure direction this package exists to
// close, not a recoverable degradation.
//
// Verified against real git, not assumed: githarden_test.go arms a
// filter.<driver>.smudge that writes a marker file, and shows it does
// NOT run once this has been called against the same repoDir, and DOES
// run when it has not.
func NeutralizeFilters(repoDir string) error {
	infoDir := filepath.Join(repoDir, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return fmt.Errorf("githarden: create %s: %w", infoDir, err)
	}
	path := filepath.Join(infoDir, "attributes")
	if err := os.WriteFile(path, []byte(filterAttributesOverride), 0o644); err != nil {
		return fmt.Errorf("githarden: write %s: %w", path, err)
	}
	return nil
}

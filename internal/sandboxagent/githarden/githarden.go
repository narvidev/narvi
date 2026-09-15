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

// Three command classes are deliberately left off the list above, and NOT
// because they are safe. Two adversarial audit rounds, both reproduced
// against real git, established that none of the three can be closed by
// anything this package does, and record why here so the gap cannot rot
// into a comment nobody rechecks; see githarden_test.go for the
// executable proof each class actually runs.
//
//  1. filter.<driver>.clean/.smudge. A filter's driver name is chosen by
//     the repository's own .gitattributes ("<path> filter=<anything>"),
//     so there is no fixed "filter.X.clean" key a -c entry could reset
//     once and cover every case: "-c filter.*.clean=" is not a wildcard
//     to git, it names a literal, useless config section called "*".
//     git documents no flag that disables the filter mechanism wholesale
//     (gitattributes(5)).
//
//  2. merge.<driver>.driver. Same shape as (1) -- the driver name is
//     chosen by the repository's own .gitattributes ("<path>
//     merge=<anything>") -- and reachable through a command this
//     codebase actually runs: internal/sandboxagent/gitclone's syncOne
//     runs `git stash pop --index` to restore a stashed working tree,
//     which invokes the configured merge driver on conflict. Root in
//     production (workspaceowner.go).
//
//  3. remote.<name>.uploadpack / remote.<name>.receivepack. Unlike (1)
//     and (2), this key has no attributes half at all to reason about --
//     it lives only in .git/config, names a command git runs as the
//     LOCAL side of the pack protocol, and fires deterministically, with
//     no race, on a plain `git fetch` or `git push` against a remote
//     configured with that key. Nothing in gitattributes(5) or a -c
//     override touches it.
//
// A tempting answer for all three -- "the UID boundary (§30.5) already
// covers this, since a repository-authored command either runs as the
// runtime already, or never runs at all" -- does not survive contact
// with what §30.5 actually grants. The runtime owns .git BECAUSE of that
// boundary, which makes writing any of the three keys above into
// .git/config (and, for (1)/(2), a matching attribute into
// .gitattributes) an ORDINARY, unprivileged act for it -- not a
// violation of the boundary. The violation is what happens next:
// sandbox-agent's OWN later git invocations against that SAME repository
// -- gitclone's SyncAll reconciling an already-existing workspace exactly
// like a repo_image/snapshot_restore boot presents one, its
// CleanForImageBuild running `checkout -- .` at image-bake time, or any
// plain fetch/push -- read that config back and run the planted command
// AS SANDBOX-AGENT. Same shape as the pre-push-hook exploit this file's
// own top comment documents, one call removed.
//
// A first attempt at a mitigation for (1) alone -- writing "* -filter"
// into $GIT_DIR/info/attributes, gitattributes(5)'s own highest-
// precedence attributes source -- was tried, measured against real git,
// and WITHDRAWN. It failed on every axis that matters: the attributes
// file it wrote into lives inside the same runtime-owned directory the
// attack starts from, so the runtime can delete or replace it between
// the write and git's read (0 of 60 racing trials blocked in
// measurement); it addressed only (1), leaving (2) and (3) untouched;
// and reaching it required a root-privileged os.WriteFile into a
// directory an attacker controls, which is itself a primitive an
// attacker could redirect (via a symlink planted at the parent
// directory) into truncating an arbitrary file on the host. A mitigation
// that adds a root-privileged write into an attacker-owned directory,
// for a race it cannot win, is net-negative -- worse than doing nothing.
//
// CONCLUSION: none of these three classes is closed by anything in this
// package, or closable by any flag, attributes override, or file written
// into .git from a process that does not itself own .git. The only real
// remedy is structural: sandbox-agent must stop running git against a
// .git directory the sandbox runtime owns (filed as a follow-up plan
// row; find it by its own citation, never by a Step number -- Step
// numbers do not belong in this source per this codebase's own
// convention). Until that lands, this is a recorded, accepted gap, not a
// fixed one.
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

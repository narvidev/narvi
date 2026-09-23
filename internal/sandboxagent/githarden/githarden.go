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

		// -- transport class: which underlying connection mechanism git
		// uses to reach a remote at all, closed by protocol.<name>.allow.
		//
		// Unlike filter.<driver>/merge.<driver> (the driver NAME is chosen
		// by the repository's own .gitattributes -- an unbounded
		// namespace no fixed -c key can reset) or remote.<name>.uploadpack/
		// receivepack in its ssh/local-transport form (see this package's
		// second doc comment for why that instantiation closes as a
		// consequence of the allowlist below, not by a key of its own),
		// every key here has a FIXED, enumerable name -- which is exactly
		// what makes a -c override able to close it.
		//
		// sandbox-agent only ever legitimately speaks https:
		// reposource.ValidateRepoURL (internal/domain/reposource) accepts
		// nothing but an absolute "https://" URL, with a non-empty host,
		// at session-config time -- every other scheme (including a bare
		// local path, "ssh://", "git://", "ext::", "http://") is rejected
		// before it ever reaches a git subprocess. This allowlist is not
		// a guess at what SHOULD be permitted after that; it is exactly,
		// and only, what this codebase's own clone/fetch/push/ls-remote
		// call sites already use.
		//
		// protocol.allow alone is NOT sufficient: it is a DEFAULT policy
		// for protocols with no policy of their own name
		// (protocol.<name>.allow), so a repository-authored
		// "protocol.ssh.allow = always" in the runtime-owned .git/config
		// outranks a command-line "-c protocol.allow=never" entirely --
		// verified directly against real git: the general fallback does
		// NOT override a specific per-protocol policy set anywhere else.
		// Every dangerous protocol is therefore named here explicitly, so
		// the SAME key the runtime could set is the one this list resets
		// on the command line, which command-line -c always wins for
		// (verified: an explicit "-c protocol.file.allow=never" DOES
		// override a repository-configured "protocol.file.allow=always").
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.http.allow=never",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.git.allow=never",
		"-c", "protocol.ssh.allow=never",
		"-c", "protocol.ext.allow=never",

		// core.gitProxy names a command git runs (as "command host port")
		// instead of connecting directly, but ONLY for the "git://" protocol
		// -- already denied above by protocol.git.allow=never, which is what
		// ACTUALLY closes this key: verified directly against real git that
		// "-c core.gitProxy=none" (or an empty value) does NOT, on its own,
		// override a repository-configured core.gitProxy. Unlike
		// credential.helper, whose empty value is documented to discard
		// every earlier entry, core.gitProxy is multi-valued with "first
		// match wins" semantics and no reset behaviour of its own -- a
		// file-sourced entry is consulted before anything the command line
		// adds, so it wins regardless of what a later "-c core.gitProxy=..."
		// says. Set anyway, harmlessly, as declared intent and defense in
		// depth for the (today nonexistent) case where no repository-level
		// value is present at all: "none" is git's own documented value for
		// "use no proxy", applied with no trailing "for <domain>" so it
		// matches every host git might ever try it against.
		"-c", "core.gitProxy=none",

		// http.proxy reroutes the one transport still permitted (https)
		// through a proxy of the repository's choosing. Not a command by
		// itself, but a redirection/exfiltration primitive for the
		// surviving transport, neutralised the same way credential.helper
		// is: an empty value overrides any repository- or environment-
		// supplied proxy and forces a direct connection.
		"-c", "http.proxy=",
	}
}

// Three command classes were ONCE all left off the list above, and NOT
// because they were safe. Two adversarial audit rounds, both reproduced
// against real git, established that none of the three could be closed by
// anything reachable through a fixed -c key -- and recorded why here so
// the gap could not rot into a comment nobody rechecks; see
// githarden_test.go for the executable proof each class actually runs.
//
//  1. filter.<driver>.clean/.smudge. A filter's driver name is chosen by
//     the repository's own .gitattributes ("<path> filter=<anything>"),
//     so there is no fixed "filter.X.clean" key a -c entry could reset
//     once and cover every case: "-c filter.*.clean=" is not a wildcard
//     to git, it names a literal, useless config section called "*".
//     git documents no flag that disables the filter mechanism wholesale
//     (gitattributes(5)). STILL OPEN.
//
//  2. merge.<driver>.driver. Same shape as (1) -- the driver name is
//     chosen by the repository's own .gitattributes ("<path>
//     merge=<anything>") -- and reachable through a command this
//     codebase actually runs: internal/sandboxagent/gitclone's syncOne
//     runs `git stash pop --index` to restore a stashed working tree,
//     which invokes the configured merge driver on conflict. Root in
//     production (workspaceowner.go). STILL OPEN.
//
//  3. remote.<name>.uploadpack / remote.<name>.receivepack. Unlike (1)
//     and (2), this key has no attributes half at all to reason about --
//     it lives only in .git/config, names a command git runs as the
//     LOCAL side of the pack protocol, and fires deterministically, with
//     no race, on a plain `git fetch` or `git push` against a remote
//     configured with that key. NOW CLOSED, as a consequence rather than
//     by a key of its own: this key is consulted only for a local
//     ("file") or ssh transport (verified: it is inert over the smart-HTTP
//     transport this codebase actually uses -- see
//     TestArgs_RealHTTPSCloneAndFetchStillWork, githarden_test.go), and
//     the transport-class hardening below denies both. See
//     TestTransportClass_FileUploadPackNoLongerExecutes for the executable
//     proof, and its own doc comment for why (1)/(2) do NOT get the same
//     treatment: their driver NAME is repository-chosen and unbounded, so
//     no fixed transport-style allowlist reaches them the way a fixed
//     protocol name does here.
//
// A tempting answer for (1) and (2) -- "the UID boundary (§30.5) already
// covers this, since a repository-authored command either runs as the
// runtime already, or never runs at all" -- does not survive contact
// with what §30.5 actually grants. The runtime owns .git BECAUSE of that
// boundary, which makes writing either key above into .git/config (and a
// matching attribute into .gitattributes) an ORDINARY, unprivileged act
// for it -- not a violation of the boundary. The violation is what
// happens next: sandbox-agent's OWN later git invocations against that
// SAME repository -- gitclone's SyncAll reconciling an already-existing
// workspace exactly like a repo_image/snapshot_restore boot presents one,
// its CleanForImageBuild running `checkout -- .` at image-bake time, or
// any plain fetch/push -- read that config back and run the planted
// command AS SANDBOX-AGENT. Same shape as the pre-push-hook exploit this
// file's own top comment documents, one call removed. The SAME reasoning
// is why the transport class above could NOT be closed by relying on
// §30.5 either -- it took a fixed, enumerable key set instead, which (1)
// and (2) do not have.
//
// A first attempt at a mitigation for (1) alone -- writing "* -filter"
// into $GIT_DIR/info/attributes, gitattributes(5)'s own highest-
// precedence attributes source -- was tried, measured against real git,
// and WITHDRAWN. It failed on every axis that matters: the attributes
// file it wrote into lives inside the same runtime-owned directory the
// attack starts from, so the runtime can delete or replace it between
// the write and git's read (0 of 60 racing trials blocked in
// measurement); it addressed only (1), leaving (2) and (3) untouched (at
// the time -- (3) closed later, by the transport class, not by this
// mitigation or anything like it); and reaching it required a
// root-privileged os.WriteFile into a directory an attacker controls,
// which is itself a primitive an attacker could redirect (via a symlink
// planted at the parent directory) into truncating an arbitrary file on
// the host. A mitigation that adds a root-privileged write into an
// attacker-owned directory, for a race it cannot win, is net-negative --
// worse than doing nothing.
//
// CONCLUSION: (1) and (2) remain open, closed by nothing in this package
// or closable by any flag, attributes override, or file written into
// .git from a process that does not itself own .git. The only real
// remedy for those two is structural: sandbox-agent must stop running
// git against a .git directory the sandbox runtime owns (filed as a
// follow-up plan row; find it by its own citation, never by a Step
// number -- Step numbers do not belong in this source per this
// codebase's own convention). Until that lands, they are a recorded,
// accepted gap, not a fixed one. (3) is the one exception: it is fixed,
// by the transport-class hardening above, precisely because -- unlike
// (1) and (2) -- it never had an unbounded, repository-chosen namespace
// to hide in.
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

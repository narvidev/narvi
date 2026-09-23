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
	"os"
	"strings"

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
		// uses to reach a remote at all.
		//
		// This used to be closed by protocol.<name>.allow ALONE, enumerated
		// one fixed name at a time (https/http/file/git/ssh/ext). That
		// enumeration is NOT this class's actual guarantee any more, and a
		// two-round adversarial audit is why: git also supports an
		// ARBITRARY remote helper named "<anything>::" (git-remote-<name>,
		// resolved from PATH at run time) plus ftp/ftps (git ships
		// git-remote-ftp(s) by default) -- neither has a name this list can
		// enumerate in advance, because the attacker chooses the name.
		// Verified directly: a runtime-owned .git/config naming remote
		// "foo::whatever", with "protocol.foo.allow=always" and an
		// "alias.remote-foo" shell-out planted alongside it, EXECUTES that
		// alias under every -c flag this list used to rely on alone --
		// there is no "protocol.foo.allow=never" to add, because "foo" is
		// not a name anyone enumerated ahead of time. Same story for a
		// planted "protocol.ftp.allow=always": git-remote-ftp reaches the
		// network under the same enumerated flags.
		//
		// What actually closes this now is GIT_ALLOW_PROTOCOL (see Env,
		// below) set in the SPAWNED PROCESS'S OWN ENVIRONMENT, never in
		// .git/config: it is a positive allowlist of transport names git
		// will ever consider at all, and unlike every -c/config-sourced
		// key in this file, no repository-level config can widen it back
		// open -- a repository can set "protocol.foo.allow=always" all it
		// wants; git still refuses "foo" outright if GIT_ALLOW_PROTOCOL
		// does not name it, before ever consulting protocol.allow or any
		// per-name policy. Verified directly: the exact "foo::" alias RCE
		// above, and the exact ftp reachability above, both stop
		// ("transport 'foo' not allowed" / "transport 'ftp' not allowed")
		// the moment GIT_ALLOW_PROTOCOL=https is added to the child's
		// environment -- with no other change.
		//
		// The -c protocol.<name>.allow flags below are KEPT, not replaced:
		// defense in depth for the fixed, enumerable names that DO have
		// one (https/http/file/git/ssh/ext/ftp/ftps), consistent with
		// every other key in hardeningFlags. But the guarantee this class
		// now rests on is GIT_ALLOW_PROTOCOL, precisely because it is the
		// one mechanism here that does not depend on the attacker's
		// namespace already being known.
		//
		// sandbox-agent only ever legitimately speaks https:
		// reposource.ValidateRepoURL (internal/domain/reposource) accepts
		// nothing but an absolute "https://" URL, with a non-empty host,
		// at session-config time -- every other scheme (including a bare
		// local path, "ssh://", "git://", "ext::", "http://", "ftp://",
		// or an arbitrary "<name>::" helper) is rejected before it ever
		// reaches a git subprocess. This allowlist is not a guess at what
		// SHOULD be permitted after that; it is exactly, and only, what
		// this codebase's own clone/fetch/push/ls-remote call sites
		// already use -- and exactly what GIT_ALLOW_PROTOCOL's own single
		// value (AllowedProtocol, "https") states again, in the one place
		// a repository-owned config can never reach.
		//
		// protocol.allow alone is NOT sufficient: it is a DEFAULT policy
		// for protocols with no policy of their own name
		// (protocol.<name>.allow), so a repository-authored
		// "protocol.ssh.allow = always" in the runtime-owned .git/config
		// outranks a command-line "-c protocol.allow=never" entirely --
		// verified directly against real git: the general fallback does
		// NOT override a specific per-protocol policy set anywhere else.
		// Every dangerous protocol WITH A FIXED NAME is therefore still
		// named here explicitly, so the SAME key the runtime could set is
		// the one this list resets on the command line, which command-line
		// -c always wins for (verified: an explicit
		// "-c protocol.file.allow=never" DOES override a
		// repository-configured "protocol.file.allow=always").
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.http.allow=never",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.git.allow=never",
		"-c", "protocol.ssh.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.ftp.allow=never",
		"-c", "protocol.ftps.allow=never",

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
		// through a proxy of the repository's choosing -- a
		// redirection/exfiltration primitive for the surviving transport,
		// not a command by itself.
		//
		// This does NOT "force a direct connection", and an earlier
		// version of this comment claimed it did -- verified directly
		// against real git that the claim was false. http.proxy is a
		// urlmatch-scored config family: http.<url>.proxy and
		// remote.<name>.proxy are BOTH more specific than the general
		// http.proxy this line resets, and git prefers the most specific
		// match regardless of which config SOURCE it came from. A
		// repository-authored "http.<the-exact-clone-url>.proxy" or
		// "remote.origin.proxy" therefore still wins over this reset alone
		// -- confirmed by routing a real fetch, with only this flag
		// applied, through a proxy address nothing was listening on: the
		// fetch failed via the proxy, not via a direct connection.
		//
		// remote.<name>.proxy is closed here for the one remote name every
		// clone/fetch/ls-remote call site in this codebase ever legitimately
		// uses ("origin" -- git-clone(1) never receives an --origin
		// override anywhere in this codebase, so the default name is the
		// ONLY name a fresh clone's own remote is ever created with). This
		// is a literal, non-urlmatch config key, so a command-line -c
		// unconditionally wins the same way protocol.file.allow does above
		// -- verified directly: "-c remote.origin.proxy=" DOES override a
		// repository-configured "remote.origin.proxy" pointed at an address
		// nothing listens on. `git push`'s own remote name is
		// session-controlled (sandboxws.Push.Repos[].Remote, validated by
		// reposource.ValidateRemoteName) and is NOT always "origin" -- its
		// own call site (cmd/sandbox-agent's pushOneRepo) adds a SECOND,
		// identically-shaped override for whatever remote name it actually
		// validated, on top of this one.
		//
		// http.<url>.proxy is the residual this flag alone does not close,
		// but remote.<name>.proxy above already closes it in full for every
		// call site that ever contacts a remote by NAME -- which is every
		// gitclone call site EXCEPT the clone itself: gitclone's own fetch/
		// ls-remote helpers (resolveDefaultBranch, gitFetchRef, sync.go) both
		// run `... origin ...`, so remote.origin.proxy= above already wins
		// for them unconditionally, regardless of what url remote.origin.url
		// (read from this repository's own runtime-owned .git/config, not
		// from session config) actually resolves to. An earlier version of
		// this package also added RepoURLProxyArg, below, at those two call
		// sites, keyed to the validated SESSION url -- that was not a second
		// layer of defense, it was dead: the key it built could only ever
		// match remote.origin.proxy's own guarantee when the session url and
		// the runtime-owned remote.origin.url happened to already agree, and
		// would silently match nothing the one time it would matter (the
		// runtime having rewritten remote.origin.url), while the comments at
		// both call sites credited it with closing the vector regardless.
		// Removed; see gitclone/sync.go's own doc comments on
		// resolveDefaultBranch and gitFetchRef, and
		// TestTransportClass_RemoteOriginProxyClosesRewrittenURL below for the
		// executable proof that remote.origin.proxy= alone is what actually
		// holds here.
		//
		// gitclone.cloneOne is the one call site where RepoURLProxyArg is
		// still real, load-bearing defense-in-depth: `git clone`'s own
		// initial fetch contacts the url given on ITS OWN command line
		// (repo.Url, from validated session config) to create the "origin"
		// remote in the first place, so the url RepoURLProxyArg keys off of
		// there is, by construction, the exact url this invocation ever
		// contacts -- unlike fetch/ls-remote above, there is no separate,
		// potentially-rewritten remote.origin.url in play for that same
		// invocation to diverge from. `git push` (cmd/sandbox-agent) is the
		// one network-touching call site with NO validated URL of its own at
		// all (sandboxws.Push.Repos[] carries name/branch/remote, never a
		// url) -- its own http.<url>.proxy exposure is therefore a recorded,
		// accepted residual, not a fixed one; see docs/DECISIONS.md.
		"-c", "http.proxy=",
		"-c", "remote.origin.proxy=",
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
// in one direction and destructive in the other. Env is set via Env(nil)
// (this process's own environment plus GIT_ALLOW_PROTOCOL) -- the same
// default every OTHER call site in this codebase now builds explicitly;
// a caller that needs a different base environment should build its own
// Spec with Env(base) rather than use this one.
func Spec(repoDir string, rest ...string) supervisor.Spec {
	return supervisor.Spec{Path: "git", Args: Args(repoDir, rest...), Env: Env(nil)}
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
//
// That "unchanged" fallback is deliberate for --version/no-repository
// callers, and was ALSO, silently, exactly the bug in gitclone.cloneOne's
// own `git clone` invocation: clone's own target directory is its last
// POSITIONAL argument, never a "-C <dir>" (a fresh clone's own -C target
// would have to already exist, and it does not yet), so a `git clone`
// argument list handed to Harden has no "-C" for this loop to find and is
// returned COMPLETELY UNCHANGED -- every flag below, silently absent, with
// nothing about the call site itself hinting that Harden did nothing. Use
// ArgsForClone (below) for `git clone` specifically -- never Harden --
// precisely because this failure mode exists and looks, at the call site,
// identical to correct usage.
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

// ArgsForClone returns the hardening flags for a `git clone` invocation
// whose target directory (dir) does not exist yet, followed by rest (the
// caller's own "clone" subcommand and its arguments, in full, e.g.
// ["clone", "-c", "credential.helper=...", "--", url, dir]).
//
// Neither Args nor Harden fits `git clone`'s own shape: both key off a
// "-C <dir>" that must precede the subcommand, but `git -C <dir> clone`
// requires <dir> to ALREADY exist (-C changes directory before running
// anything), which a fresh clone's own target never does at invocation
// time -- dir is clone's own trailing POSITIONAL argument instead. This
// function scopes safe.directory to dir directly (hardeningFlags itself
// tolerates a path that does not exist yet -- it is only ever compared
// against an owner UID once git actually opens it) without pretending a
// "-C <dir>" belongs in the argument list at all.
func ArgsForClone(dir string, rest ...string) []string {
	return append(hardeningFlags(dir), rest...)
}

// AllowedProtocol is the one git transport scheme sandbox-agent's own git
// invocations are ever allowed to use, and the value every hardened git
// invocation's own GIT_ALLOW_PROTOCOL is set to (see Env, below).
// reposource.ValidateRepoURL enforces the SAME restriction at
// session-config time (an absolute "https://" URL only) -- this constant
// is the identical restriction re-stated at the one layer a
// runtime-owned .git/config can never reach.
const AllowedProtocol = "https"

// gitAllowProtocolEnv is the literal "NAME=VALUE" environment entry Env
// appends -- GIT_ALLOW_PROTOCOL is a colon-separated ALLOWLIST git
// consults before it ever opens a transport, checked by git itself, not
// by any config value: a repository can set
// "protocol.<anything>.allow=always" for a name of its own choosing (an
// arbitrary "<name>::" remote helper resolved from PATH, or ftp/ftps,
// which this package's own -c protocol.<name>.allow flags can only ever
// enumerate one FIXED name at a time), and GIT_ALLOW_PROTOCOL still
// refuses it -- verified directly (see githarden_test.go): a runtime-armed
// "foo::" remote helper with a shell-executing git alias, and a
// runtime-armed ftp:// remote, both stop cold the moment this is added to
// the child's own environment, with no other change.
const gitAllowProtocolEnv = "GIT_ALLOW_PROTOCOL=" + AllowedProtocol

// Env returns base with GIT_ALLOW_PROTOCOL set to AllowedProtocol,
// appended LAST so it wins over anything base already carries
// (exec.Cmd's own documented "last duplicate key wins" Env behavior). A
// nil base becomes this process's own os.Environ() first -- Spec.Env's
// own "nil means inherit everything" default cannot be preserved AND
// have one variable added to it at the same time (there is no way to ask
// exec.Cmd to "inherit, plus this one thing"; the only way to add one
// entry is to build the full slice), so every hardened git invocation
// that used to leave Spec.Env at its nil zero value now calls Env(nil)
// instead, and every one that already built an explicit slice (LC_ALL=C,
// supervisor.EnvWithout(...), ...) wraps it in Env(...) rather than
// appending the literal itself, so this is the one place the value can
// ever drift.
//
// This is not optional the way the -c protocol.<name>.allow flags in
// hardeningFlags are effectively optional defense-in-depth for a FIXED
// name: GIT_ALLOW_PROTOCOL is the actual guarantee for the class neither
// hardeningFlags nor any -c flag can enumerate in advance. See
// hardeningFlags' own "transport class" doc comment above for the
// verified "foo::"/ftp findings this closes that the -c flags alone do
// not.
func Env(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	return append(base, gitAllowProtocolEnv)
}

// RemoteProxyArg returns the -c override that closes remote.<remoteName>.
// proxy for one already-validated (reposource.ValidateRemoteName) git
// remote name.
//
// remote.<name>.proxy is a LITERAL config key, not urlmatch-scored the
// way http.<url>.proxy is (RepoURLProxyArg, below) -- a repository-set
// "remote.origin.proxy" is closed by an identically-named command-line -c
// the same unconditional way "-c protocol.file.allow=never" already wins
// over a repository-configured "protocol.file.allow=always" (verified
// directly: a command-line "-c remote.<name>.proxy=" DOES override a
// repository-configured "remote.<name>.proxy" pointed at an address
// nothing listens on -- the fetch then fails by attempting the ACTUAL
// remote host directly, never the planted proxy address). hardeningFlags
// itself already carries "remote.origin.proxy=" unconditionally, for the
// one remote name every clone/fetch/ls-remote call site in this codebase
// ever creates -- this function exists for the one call site where the
// remote name is NOT fixed: `git push`'s own remote is
// session-controlled (sandboxws.Push.Repos[].Remote).
func RemoteProxyArg(remoteName string) []string {
	return []string{"-c", "remote." + remoteName + ".proxy="}
}

// RepoURLProxyArg returns the -c override that closes http.<repoURL>.proxy
// for the EXACT url this invocation will contact, or nil if repoURL is
// empty or unsafe to embed (see below) -- there is no fixed key
// hardeningFlags itself can carry for this, because the key's own name is
// the url, which varies per repo/call.
//
// http.<url>.proxy is urlmatch-scored: git prefers the MOST SPECIFIC
// matching entry, regardless of which config SOURCE (file vs command
// line) it came from, and a repository can set an entry at ANY
// specificity up to and including the exact request url. The exact
// request url is therefore the most specific match ANY entry can ever
// have for this request -- an attacker cannot register something MORE
// specific than the literal url git is about to use -- so a command-line
// entry for that SAME exact url is guaranteed to tie, at worst, any
// repository-authored entry, and verified directly: git resolves that tie
// in the command line's favor (a repository-set http.<the-exact-url>.
// proxy, "neutralised" by a command-line entry for that same exact url,
// stops routing through the planted address; a repository-set entry for
// a LESS specific prefix of the same url loses outright, as expected).
//
// repoURL must be the literal string this invocation's own command line
// will contact directly -- never read back from the repository's own
// (potentially runtime-rewritten) remote.<name>.url, which would be racy
// and circular. That is why gitclone.cloneOne is this function's one
// caller: `git clone`'s own target url IS its own command-line argument,
// so the two can never diverge. gitclone's fetch/ls-remote helpers
// (resolveDefaultBranch, gitFetchRef, sync.go) do NOT call this -- they
// contact remote.origin.url, resolved from the repository's own
// runtime-owned config, which this function's repoURL parameter (the
// validated SESSION url) is not guaranteed to match; their own guarantee
// is hardeningFlags' unconditional, remote-NAME-keyed
// "remote.origin.proxy=" instead (see hardeningFlags' own http.proxy doc
// comment for the full reasoning, and for why a call site with no
// validated url of its own, e.g. `git push`, gets no override from this
// function at all).
//
// A repoURL containing "=" is rejected (returns nil) rather than risking
// a corrupted override: git's own "-c key=value" parsing splits on the
// FIRST "=" in the whole argument, so a url containing one would split
// the intended http.<url>.proxy key itself in two, silently producing an
// inert, wrong config entry instead of the intended override.
// reposource.ValidateRepoURL does not forbid "=" (query strings are legal
// URL syntax), so this is checked here, defensively, rather than assumed
// impossible upstream.
func RepoURLProxyArg(repoURL string) []string {
	if repoURL == "" || strings.Contains(repoURL, "=") {
		return nil
	}
	return []string{"-c", "http." + repoURL + ".proxy="}
}

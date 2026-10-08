// Package reposource validates the three session-controlled pieces of
// data that name a git repo reference before ANY of them is allowed to
// reach a real `git` subprocess's argument list or a filesystem path:
// a repo's Name (sessionconfig.SessionConfig.Repos[].Name /
// sandboxws.Push.Repos[].Name), its clone URL
// (sessionconfig.SessionConfig.Repos[].Url), and its Branch/Remote
// (sessionconfig.SessionConfig.Repos[].Branch / sandboxws.Push.
// Repos[].Branch / .Remote).
//
// This package exists to close two confirmed classes of bug, both found
// in internal/sandboxagent/gitclone.cloneOne and cmd/sandbox-agent's own
// pushOneRepo, and both rooted in the same fact: these three fields flow
// in, entirely unvalidated, from a session-controlled request body, all
// the way to a real subprocess boundary.
//
//  1. Git argument injection. `git clone`/`git push` each receive a
//     repo URL / branch / remote as trailing POSITIONAL arguments. git's
//     own option parser reorders interspersed options and non-options --
//     it does not know or care that a caller INTENDED a given string to
//     be "just a URL" or "just a branch name". A value beginning with
//     "-" is parsed as an OPTION, not a plain string (e.g.
//     "--upload-pack=<cmd>" for clone, "--receive-pack=<cmd>" for push --
//     both real git flags that shell out to an arbitrary command), and a
//     value using an exotic-but-allowed-by-default git transport (e.g.
//     "ext::sh -c '<cmd>'") is parsed as an alternate transport, not a
//     plain https remote. Either can execute an arbitrary command at
//     clone/push time using nothing but session-controlled input.
//     ValidateRepoURL closes this with an ALLOWLIST, not a denylist: only
//     an absolute "https://" URL with a non-empty host is accepted, which
//     rejects every alternate transport and every bare "-"-prefixed
//     string in one rule, without trying to enumerate "known bad"
//     schemes one at a time. ValidateBranch closes the branch half of the
//     same bug by rejecting any value beginning with "-" (branches
//     legitimately contain "/", e.g. "feature/foo", so a charset
//     allowlist is not appropriate there), and accepts only a name git
//     itself accepts as a branch that `git push` reads as exactly that
//     branch: no leading "+" (a forced push), no ":" (a deletion or
//     another destination). ValidateRemoteName closes the
//     remote half more strictly still, with its OWN charset allowlist
//     (the same one ValidateRepoName uses, [a-zA-Z0-9_.-]+, explicitly
//     rejecting the literal "." and ".." segments too) rather than
//     merely rejecting a leading "-": a remote name is conceptually a
//     bare identifier ("origin", "upstream", "fork"), never a path or a
//     URL, so this also closes a redirection angle a leading-dash-only
//     check would have missed entirely -- a path-shaped remote value
//     (e.g. a filesystem path to an attacker-controlled bare repo) has no
//     leading dash at all, yet would make `git push` send the real commit
//     to that rogue destination instead of the real "origin" if allowed
//     through, and the same allowlist also rejects every alternate-
//     transport string ("ext::", "fd::", ...) via the same excluded ":"
//     character.
//  2. Path traversal via an unconstrained repo name. Both cloneOne and
//     pushOneRepo build their working directory as
//     filepath.Join(workspaceDir, repo.Name) with no constraint on
//     repo.Name at all -- a name containing ".." (e.g. "../../etc")
//     escapes the sandbox's intended workspace tree via ordinary
//     lexical path-traversal. ValidateRepoName closes this by requiring
//     the name to match ONLY `[a-zA-Z0-9_.-]+` (which already excludes
//     "/", ruling out any multi-segment traversal) and by additionally,
//     explicitly rejecting the literal strings "." and ".." -- both of
//     which are composed entirely of characters the charset above
//     already allows, and would therefore otherwise slip through the
//     charset check alone. A charset regex that is even slightly too
//     permissive here is exactly the kind of near-miss this package
//     exists to not repeat (see internal/domain/environment's own
//     hasDotDotSegment, which had to guard the analogous "a exact ..
//     SEGMENT, not merely a .. substring" distinction for path_scope
//     globs) -- this package's own tests cover the equivalent
//     character-class/escape tricks (e.g. "[.][.]/etc", `\.\./etc`), not
//     just the literal ".." substring.
//
// This package performs ONLY validation -- it never builds a git
// argument list, never invokes git, never touches a filesystem path
// (§11: no I/O in /internal/domain). Wiring these validators to an
// actual `git`-invoking call site is each caller's own job:
// internal/sandboxagent/gitclone (clone) and cmd/sandbox-agent's own
// pushOneRepo (push) both call this package directly; a later, separate
// piece of work reuses the SAME package from the control plane's own
// session-creation path, before a SessionConfig is ever handed to a
// sandbox at all -- this package is deliberately import-cycle-free and
// side-effect-free so that reuse costs nothing.
package reposource

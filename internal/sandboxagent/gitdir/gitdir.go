// Package gitdir builds and maintains the AGENT-OWNED git-dir Step 171
// (§30.5) requires every sandbox-agent git invocation to use instead of a
// runtime-owned worktree's own ".git" -- see internal/sandboxagent/
// githarden's own top doc comment for why: a repository the runtime owns
// is a repository the runtime can plant a filter/merge driver, a hook, or
// a transport command into, and have it run AS SANDBOX-AGENT the next time
// this codebase's own git touches that same directory.
//
// The shape, measured directly against the real git binary rather than
// assumed (see Seed's own doc comment for the exact sequence):
//
//   - config and hooks/ are REAL, agent-owned files/directories --
//     nothing the runtime ever wrote can be read from them.
//   - objects, refs, packed-refs, logs, info, and the index are SYMLINKED
//     to the runtime worktree's own .git -- this is data that carries no
//     command, so sharing it keeps a runtime commit pushable by
//     sandbox-agent and vice versa. objects/refs/index/logs/info are
//     directory or file symlinks to the corresponding path under
//     <worktree>/.git; alternates (objects/info/alternates) was measured
//     and rejected -- it does not make agent-written objects visible to
//     the runtime's own git (a real, both-ways requirement here).
//   - HEAD is the one piece kept as a REAL agent-owned file, synced BOTH
//     ways by SyncHeadIn/SyncHeadOut below, because git refuses a
//     symlinked HEAD outright ("fatal: not a git repository").
//   - core.sparseCheckout/core.sparseCheckoutCone is the one per-side
//     config bool mirrored into the runtime's own config after every
//     agent-side `sparse-checkout set|disable`, and imported from it at
//     Seed time -- everything else in the runtime's own .git/config never
//     needs to be read at all.
//
// Run, below, is the one choke point every agent git invocation in this
// codebase is expected to go through, so no call site can forget the
// SyncHeadIn/SyncHeadOut bracket a bare githarden.Spec call would silently
// skip.
package gitdir

import (
	"path/filepath"

	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
)

// Layout is one sandbox's own git-dir root plus its workspace directory --
// the two inputs Repo needs to build one repository's own githarden.Repo
// pair. Root is boot.Config.GitDirRoot; WorkspaceDir is boot.Config.
// WorkspaceDir. Deliberately plain strings, not a *boot.Config: this
// package must not import internal/sandboxagent/boot (boot itself imports
// this package, for the boot-time Seed loop and the fingerprint/
// deps-ladder git calls this Step routes through it -- an import the other
// direction would cycle).
type Layout struct {
	Root         string
	WorkspaceDir string
}

// Repo builds the githarden.Repo pair for one repository name: WorkTree
// under l.WorkspaceDir (matching every other call site's own
// filepath.Join(workspaceDir, name) convention -- gitclone.CloneAll/
// SyncAll, cmd/sandbox-agent's own pushOneRepo/headSHA), and GitDir under
// l.Root, at the SAME leaf name -- so a git-dir never collides across
// repos and never has to be invented or looked up separately from the
// worktree it belongs to.
func (l Layout) Repo(name string) githarden.Repo {
	return githarden.Repo{
		WorkTree: filepath.Join(l.WorkspaceDir, name),
		GitDir:   filepath.Join(l.Root, name),
	}
}

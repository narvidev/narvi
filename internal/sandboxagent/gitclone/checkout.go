package gitclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/environment"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// PullCheckoutOutcome is what CheckoutPullRef did for one repo. Its values
// are the ones the sandbox WebSocket's checkout_result carries
// (contracts/sandbox-ws/v1/events.schema.json, CheckoutResult), less
// "busy", which the agent answers before it ever calls CheckoutPullRef.
type PullCheckoutOutcome string

const (
	// PullCheckoutCheckedOut means the worktree holds HeadSHA, the commit
	// asked for, with every tracked change discarded, every path the
	// previous index tracked and HeadSHA lacks deleted (ignored or not, as
	// a plain forced checkout deletes it), every untracked file that is not
	// ignored removed, no operation a turn started (a merge, a rebase, a
	// cherry-pick, ...) left in progress, and, in a scoped session, no path
	// of HeadSHA outside the scope on disk.
	PullCheckoutCheckedOut PullCheckoutOutcome = "checked_out"
	// PullCheckoutSHAAbsent means the ref was fetched, and the commit asked for
	// is not in the repository. The worktree is untouched.
	PullCheckoutSHAAbsent PullCheckoutOutcome = "sha_absent"
	// PullCheckoutFetchFailed means the ref could not be fetched. The worktree
	// is untouched.
	PullCheckoutFetchFailed PullCheckoutOutcome = "fetch_failed"
	// PullCheckoutFailed means anything else, named in Err.
	PullCheckoutFailed PullCheckoutOutcome = "failed"
)

// PullHeadLocalRef is the local ref CheckoutPullRef fetches a pull
// request's head into, force-updated on every fetch: outside refs/heads,
// refs/remotes and refs/tags, so no branch, remote-tracking ref or tag of
// the repository is ever moved by it.
const PullHeadLocalRef = "refs/narvi/pull-head"

// PullCheckoutNetworkGitSpawns and PullCheckoutMaxLocalGitSpawns bound one
// CheckoutPullRef call: one fetch, bounded by fetchTimeout, then at most
// PullCheckoutMaxLocalGitSpawns local git processes, each bounded by its
// own stepTimeout, and each by stopGrace more when it has to be stopped.
// The longest path is a scoped repo whose index tracks a path the target
// lacks and holds a skip-worktree entry whose file is present: rev-parse of
// the ref and of the target (2), the diff-index that lists what the index
// tracks and the target lacks (1), the read-tree that resets the index to
// the target and the update-index that puts those entries back (2),
// ls-files and the update-index that clears those bits (2), the
// sparse-checkout set and the five-process mirror to the runtime's config
// (6), the forced checkout and the runtime HEAD write its move causes (2),
// clean (1), the sparse-checkout set that checks the scope holds (1) and
// rev-parse HEAD (1). Each of those two update-index steps runs only when it
// has an entry to write. An unscoped repo takes at most 15: the
// sparse-checkout disable and its mirror (6) in place of the five sparse
// steps before the checkout, and no check after it. Clearing an operation a
// turn left in progress spawns nothing. TestCheckoutPullRef_SpawnCount pins
// every path.
const (
	PullCheckoutNetworkGitSpawns  = 1
	PullCheckoutMaxLocalGitSpawns = 18
)

// PullCheckoutResult is one repo's outcome from CheckoutPullRef.
type PullCheckoutResult struct {
	Outcome PullCheckoutOutcome
	// HeadSHA is the worktree's HEAD after a PullCheckoutCheckedOut
	// outcome, and "" otherwise.
	HeadSHA string
	// RefSHA is the ref's tip as fetched, whatever the outcome, and "" when
	// it was never read.
	RefSHA string
	// Err says why the outcome is not PullCheckoutCheckedOut; nil when it
	// is.
	Err error
}

// CheckoutPullRef checks out, in the one repo repo names, the pull request
// head ref names, read from repo.Url -- the pull request's base repository,
// which keeps every pull request's head as refs/pull/<number>/head
// (technical plan §21.1, §30.4) -- and leaves the worktree exactly at
// wantSHA, or at the ref's tip when wantSHA is "" (the boot, which has no
// recorded head to ask for). Every step goes through runGit's own choke
// point, gitdir.Run, against the agent-owned git-dir:
//
//  1. repo.Name, ref, wantSHA and pathScope are validated before anything
//     is spawned; an invalid one is PullCheckoutFailed.
//  2. `fetch origin --no-tags -- +<ref>:refs/narvi/pull-head`, with the
//     per-invocation credential helper, bounded by fetchTimeout. A
//     failure is PullCheckoutFetchFailed.
//  3. `rev-parse --verify --quiet refs/narvi/pull-head^{commit}` gives
//     RefSHA.
//  4. The target is wantSHA, or RefSHA when wantSHA is "". A wantSHA that
//     `rev-parse --verify --quiet <target>^{commit}` does not find -- a
//     ref that lags the push that made it, or a head that moved past it
//     and was never fetched -- is PullCheckoutSHAAbsent, and the worktree
//     is left as it was.
//  5. Whatever a previous turn left in the shared index and in the
//     runtime's .git is reset before anything is checked out, and no file
//     is written into the worktree doing so. An operation the turn left in
//     progress (a merge, cherry-pick, revert, rebase, am or bisect that
//     stopped) is forgotten by clearInterruptedOperation: the runtime's own
//     --continue, --abort or `bisect reset` would otherwise act on it over
//     the checked-out tree and move it off the target. Then the index is
//     made the union of the target's tree and every entry the index had
//     that the target lacks. `diff-index --cached` lists those entries,
//     `read-tree --reset -i <target>` makes the index the target's tree,
//     discarding unmerged entries, and `update-index --index-info` puts the
//     listed entries back at stage 0 (droppedIndexInfo). -i keeps the
//     read-tree from checking the worktree, which it never writes, so a
//     changed skip-worktree file cannot stop it. A path the target has is
//     then an index entry the forced checkout rewrites or, outside the
//     scope, removes, even where the turn left an untracked file in its
//     place; and a path the target lacks is one it deletes, as a plain
//     forced checkout from the previous index does, ignored or not. Left
//     out of the index, an added path's untracked file would be kept as its
//     content, a deleted path that the target's .gitignore matches would
//     stay on disk, and an unmerged entry would make the scope's set fail
//     on every attempt. Last, the sparse state the runtime may have changed
//     since the boot, and every skip-worktree bit, are reset: a forced
//     checkout never rewrites an entry carrying the bit, nor does clean
//     remove its file. An unscoped session runs `sparse-checkout disable`,
//     which clears every bit. A scoped one clears the bit of every entry
//     whose file is present and removes a directory a turn made where the
//     index has a file (prepareScopedEntries), then runs `sparse-checkout
//     set` with its own patterns, which recomputes every other bit without
//     ever writing an out-of-scope path (§14.1). Here set may leave a
//     changed file, or an untracked one in the way, and says so: the forced
//     checkout discards them, and step 7 fails the checkout if it did not.
//     Both are mirrored to the runtime's config.
//  6. `checkout --force --detach <target> --`, then `clean -ffd`: every
//     tracked change, staged or not, is discarded, and every untracked
//     file that is not ignored is removed, nested repositories included.
//     Ignored files are kept, so what setup.sh installed survives. The
//     detached HEAD is synced out to the runtime's worktree by gitdir.Run
//     (gitdir.SyncHeadOut).
//  7. A scoped session's patterns are set again, and a path git leaves on
//     disk despite them fails the checkout: the worktree is then not the
//     target's scoped tree (§14.1).
//  8. `rev-parse HEAD` gives HeadSHA, which must be the target.
//
// chownRepo re-owns the worktree for the runtime (nil skips it) on every
// outcome from the fetch on, whatever it is: the fetch, the checkout and
// the rest ran as sandbox-agent's own identity, and a sha_absent fetch
// alone can leave new object directories the runtime could not write to.
// A failed re-own fails a checkout that had succeeded, and is added to the
// error of one that had not.
//
// It is never a caller's job to know the ref is fresh: the fetch is the
// first step every time. A review session is read-only and never pushes
// (technical plan §30.4), so discarding a worktree's changes here loses
// nothing a turn could deliver (§3.4).
func CheckoutPullRef(
	ctx context.Context,
	sup *supervisor.Supervisor,
	layout gitdir.Layout,
	cred *syscall.Credential,
	chownRepo func(dir string) error,
	repo sessionconfig.SessionConfigReposElem,
	ref, wantSHA string,
	pathScope []string,
	fetchTimeout, stepTimeout, stopGrace time.Duration,
) (result PullCheckoutResult) {
	if err := validatePullCheckout(repo, ref, wantSHA, pathScope); err != nil {
		return PullCheckoutResult{Outcome: PullCheckoutFailed, Err: err}
	}
	credHelperArg, err := CredHelperGitArg()
	if err != nil {
		return PullCheckoutResult{Outcome: PullCheckoutFailed, Err: fmt.Errorf("gitclone: determine credential helper: %w", err)}
	}
	handle := layout.Repo(repo.Name)

	defer func() {
		if chownRepo == nil {
			return
		}
		if err := chownRepo(handle.WorkTree); err != nil {
			reown := fmt.Errorf("gitclone: re-own %s for the runtime: %w", repo.Name, err)
			if result.Outcome == PullCheckoutCheckedOut {
				result.Outcome, result.HeadSHA, result.Err = PullCheckoutFailed, "", reown
				return
			}
			result.Err = fmt.Errorf("%w; %w", result.Err, reown)
		}
	}()

	fetchArgs := []string{"-c", "credential.helper=" + credHelperArg, "fetch", "origin", "--no-tags", "--", "+" + ref + ":" + PullHeadLocalRef}
	if _, err := runGitStep(ctx, sup, handle, cred, fetchArgs, fetchTimeout, stopGrace); err != nil {
		return PullCheckoutResult{Outcome: PullCheckoutFetchFailed, Err: fmt.Errorf("gitclone: fetch %s for %s: %w", ref, repo.Name, err)}
	}

	refSHA, found, err := resolveCommit(ctx, sup, handle, cred, PullHeadLocalRef, stepTimeout, stopGrace)
	if err != nil {
		return PullCheckoutResult{Outcome: PullCheckoutFailed, Err: fmt.Errorf("gitclone: read %s for %s: %w", ref, repo.Name, err)}
	}
	if !found {
		return PullCheckoutResult{Outcome: PullCheckoutFailed, Err: fmt.Errorf("gitclone: %s for %s was fetched but names no commit", ref, repo.Name)}
	}
	result = PullCheckoutResult{RefSHA: refSHA}

	target := refSHA
	if wantSHA != "" {
		target = wantSHA
		if _, found, err := resolveCommit(ctx, sup, handle, cred, target, stepTimeout, stopGrace); err != nil {
			result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: look up %s in %s: %w", target, repo.Name, err)
			return result
		} else if !found {
			result.Outcome = PullCheckoutSHAAbsent
			result.Err = fmt.Errorf("gitclone: commit %s is not in %s; %s is at %s", target, repo.Name, ref, refSHA)
			return result
		}
	}

	if err := clearInterruptedOperation(layout.WorkspaceDir, repo.Name); err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: clear the operation a turn left in progress in %s: %w", repo.Name, err)
		return result
	}
	if err := resetIndexToTarget(ctx, sup, handle, cred, target, stepTimeout, stopGrace); err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: reset the index of %s to %s: %w", repo.Name, target, err)
		return result
	}

	scoped := len(pathScope) > 0
	if scoped {
		err = prepareScopedEntries(ctx, sup, layout, repo.Name, cred, stepTimeout, stopGrace)
		if err == nil {
			// What set leaves on disk here -- a changed out-of-scope file,
			// an untracked file in the way -- the forced checkout discards,
			// and the check after it fails the checkout on anything left.
			_, err = sparseCheckoutSet(ctx, sup, handle, cred, pathScope, stepTimeout, stopGrace)
		}
		if err == nil {
			err = gitdir.MirrorSparseCheckout(ctx, sup, handle, cred, stepTimeout, stopGrace)
		}
	} else {
		err = disableSparseCheckout(ctx, sup, handle, cred, stepTimeout, stopGrace)
	}
	if err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: reset the sparse state of %s: %w", repo.Name, err)
		return result
	}

	if _, err := runGitStep(ctx, sup, handle, cred, []string{"checkout", "--quiet", "--force", "--detach", target, "--"}, stepTimeout, stopGrace); err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: check out %s in %s: %w", target, repo.Name, err)
		return result
	}
	if _, err := runGitStep(ctx, sup, handle, cred, []string{"clean", "-ffd", "--quiet"}, stepTimeout, stopGrace); err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: clean %s: %w", repo.Name, err)
		return result
	}
	if scoped {
		left, err := sparseCheckoutSet(ctx, sup, handle, cred, pathScope, stepTimeout, stopGrace)
		if err != nil {
			result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: check the path scope of %s: %w", repo.Name, err)
			return result
		}
		if left != "" {
			result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: %s is not %s's scoped tree: git left paths on disk despite the path scope: %s", repo.Name, target, left)
			return result
		}
	}

	headSHA, err := runGitStep(ctx, sup, handle, cred, []string{"rev-parse", "HEAD"}, stepTimeout, stopGrace)
	if err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: read HEAD of %s: %w", repo.Name, err)
		return result
	}
	if headSHA != target {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: %s holds %s after checking out %s", repo.Name, headSHA, target)
		return result
	}

	result.Outcome, result.HeadSHA = PullCheckoutCheckedOut, headSHA
	return result
}

// resetIndexToTarget makes repo's index the union of target's tree and
// every entry the index had that target lacks, each at stage 0 (step 5 of
// CheckoutPullRef): `diff-index --cached` lists those entries while the
// index still has them, `read-tree --reset -i <target>` makes the index
// target's tree, discarding every unmerged entry, and `update-index -z
// --index-info` writes the listed entries back, through its stdin, so
// their number is not bounded by an argv. It runs only when there is an
// entry to write. An entry that collides with one of target's paths (a
// file where target has a directory, or the reverse) replaces it, as it
// stood in the previous index.
func resetIndexToTarget(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, target string, timeout, stopGrace time.Duration) error {
	listed, err := runGitStepRaw(ctx, sup, repo, cred, []string{"diff-index", "--cached", "-z", "--no-renames", "--diff-filter=AU", target, "--"}, nil, timeout, stopGrace)
	if err != nil {
		return err
	}
	info, err := droppedIndexInfo(listed)
	if err != nil {
		return fmt.Errorf("read what the index tracks and %s lacks: %w", target, err)
	}
	if _, err := runGitStep(ctx, sup, repo, cred, []string{"read-tree", "--reset", "-i", target}, timeout, stopGrace); err != nil {
		return err
	}
	if len(info) == 0 {
		return nil
	}
	_, err = runGitStepRaw(ctx, sup, repo, cred, []string{"update-index", "-z", "--index-info"}, info, timeout, stopGrace)
	return err
}

// emptyBlobSHA1 is the object name of the empty blob in a SHA-1
// repository, which the agent's git-dir always is (gitdir.Seed).
const emptyBlobSHA1 = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"

// droppedIndexInfo turns the -z output of `diff-index --cached --no-renames
// --diff-filter=AU <target>` into the `update-index -z --index-info` input
// that puts each listed entry back at stage 0:
//
//   - An entry the index has and target lacks (A) keeps its own mode and
//     object.
//   - An unmerged path target lacks (U, with no mode on target's side) has
//     no single object; it is written with the empty blob's name. The
//     forced checkout only deletes it, and never reads that object.
//   - An unmerged path target has is left out: read-tree has put target's
//     entry there.
//
// A path that is not local to the worktree, or that has a .git component,
// is left out too: git never writes either, and update-index refuses both.
// Anything else that is not diff-index's raw shape is an error.
func droppedIndexInfo(listed string) ([]byte, error) {
	var info bytes.Buffer
	fields := strings.Split(listed, "\x00")
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("unexpected diff-index output: %d fields", len(fields))
	}
	for i := 0; i < len(fields); i += 2 {
		meta, path := fields[i], fields[i+1]
		parts := strings.Fields(strings.TrimPrefix(meta, ":"))
		if !strings.HasPrefix(meta, ":") || len(parts) != 5 {
			return nil, fmt.Errorf("unexpected diff-index record %q", meta)
		}
		targetMode, mode, object, status := parts[0], parts[1], parts[3], parts[4]
		if !filepath.IsLocal(path) || hasDotGitComponent(path) {
			continue
		}
		switch {
		case status == "A":
		case status == "U" && targetMode == "000000":
			mode, object = "100644", emptyBlobSHA1
		default:
			continue
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", mode, object, path)
	}
	return info.Bytes(), nil
}

// hasDotGitComponent reports whether a slash-separated path has a
// component that names a .git directory, in any case.
func hasDotGitComponent(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if strings.EqualFold(component, ".git") {
			return true
		}
	}
	return false
}

// interruptedOperationState is every entry under a worktree's .git that
// records an operation git started there and did not finish: a merge, a
// cherry-pick or revert of one commit or of a sequence, a rebase of either
// backend or an am, and a bisect. git's status reads them to say the
// operation is in progress, and its --continue, --abort and `bisect reset`
// act on them.
var interruptedOperationState = []string{
	"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "MERGE_RR", "AUTO_MERGE", "SQUASH_MSG",
	"CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer",
	"REBASE_HEAD", "rebase-merge", "rebase-apply",
	"BISECT_START", "BISECT_LOG", "BISECT_TERMS", "BISECT_EXPECTED_REV", "BISECT_ANCESTORS_OK",
	"BISECT_NAMES", "BISECT_RUN", "BISECT_FIRST_PARENT", "BISECT_HEAD",
}

// clearInterruptedOperation removes every interruptedOperationState entry
// from the .git of repoName's worktree under workspaceDir. They live only
// in the runtime's .git, never in the agent-owned git-dir, so no agent git
// command can clear them. The worktree and its .git are reached through
// openRealDir, from an os.Root opened at the workspace directory, which the
// session config names: a symlink the runtime made of either is never
// followed, to another repo of the workspace or out of it, and makes the
// clear fail.
func clearInterruptedOperation(workspaceDir, repoName string) error {
	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	gitDir, err := openRealDir(root, repoName, ".git")
	if err != nil {
		return err
	}
	if gitDir == nil {
		return fmt.Errorf("%s/.git is not a directory reached without a symlink", repoName)
	}
	defer func() { _ = gitDir.Close() }()
	for _, name := range interruptedOperationState {
		if err := gitDir.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

// openRealDir opens, as an os.Root, the directory components names under
// root, one component at a time, each from its parent's own Root. It
// returns nil, and no error, when one of them is absent, a symlink or not a
// directory: git never follows a symlink in a path's leading directories
// (it replaces the symlink instead), and neither does anything that removes
// a path here. A component swapped for a symlink between its Lstat and its
// open is still resolved only inside its parent.
func openRealDir(root *os.Root, components ...string) (*os.Root, error) {
	cur, owned := root, false
	release := func() {
		if owned {
			_ = cur.Close()
		}
	}
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		info, err := cur.Lstat(component)
		if errors.Is(err, fs.ErrNotExist) {
			release()
			return nil, nil
		}
		if err != nil {
			release()
			return nil, err
		}
		if !info.IsDir() {
			release()
			return nil, nil
		}
		next, err := cur.OpenRoot(component)
		release()
		if err != nil {
			return nil, err
		}
		cur, owned = next, true
	}
	if !owned {
		return nil, errors.New("no directory named")
	}
	return cur, nil
}

// removeTurnsDirectory removes the directory at path, an index entry's path
// in repoName's worktree, when it is a real directory reached through real
// directories only (openRealDir). A path below a symlink is left to the
// forced checkout, which replaces the symlink as git does.
func removeTurnsDirectory(root *os.Root, repoName, path string) error {
	components := []string{repoName}
	if dir := filepath.Dir(path); dir != "." {
		components = append(components, strings.Split(filepath.ToSlash(dir), "/")...)
	}
	parent, err := openRealDir(root, components...)
	if err != nil || parent == nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	base := filepath.Base(path)
	info, err := parent.Lstat(base)
	if err != nil || !info.IsDir() {
		return nil
	}
	return parent.RemoveAll(base)
}

// prepareScopedEntries readies a scoped session's index and worktree for
// the forced checkout, entry by entry, from one `ls-files -s -v -z`:
//
//   - A directory a turn made where the index has a file or a symlink is
//     removed (removeTurnsDirectory). The forced checkout replaces such a
//     directory with the entry only where it writes the entry, never where
//     the scope leaves the path out; clean leaves it, and sparse-checkout
//     set reports it as left on disk, so out of the scope it would fail
//     every later checkout. A directory reached through a symlink in the
//     path's leading directories is not that path's, and is left alone,
//     as git leaves it. A submodule's directory is a directory in the index
//     too, and is left alone; so a directory a turn fills at the path of a
//     submodule outside the scope still fails each scoped checkout until it
//     is removed (no review session is scoped today).
//   - The skip-worktree bit of every entry whose file is present is
//     cleared -- an entry a previous turn marked with `update-index
//     --skip-worktree` and wrote, in scope or not. Only those: every other
//     bit is the scoped session's own, and `sparse-checkout set`
//     recomputes it. git clears such a bit itself when it reads the index
//     with sparse-checkout on in the reading config, as every scoped
//     checkout leaves the agent's; but a boot's Seed imports the runtime's
//     setting, which the runtime may have turned off, and a git older than
//     that rule never clears it. Left set on a present file the target
//     does not change, the bit would keep the file through the forced
//     checkout as it is; cleared, the file is an ordinary change the
//     forced checkout discards, rewriting it in scope and removing it out
//     of scope.
//
// Every path is looked up, and a directory removed, through an os.Root
// opened at the workspace directory, as clearInterruptedOperation's
// removals are. An index path that is not local to the worktree is
// refused: git never writes one, so the index was written by hand.
func prepareScopedEntries(ctx context.Context, sup *supervisor.Supervisor, layout gitdir.Layout, name string, cred *syscall.Credential, timeout, stopGrace time.Duration) error {
	repo := layout.Repo(name)
	out, err := runGitStep(ctx, sup, repo, cred, []string{"ls-files", "-s", "-v", "-z"}, timeout, stopGrace)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(layout.WorkspaceDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var present []string
	for _, entry := range strings.Split(out, "\x00") {
		// "<tag> <mode> <object> <stage>\t<path>"
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 {
			return fmt.Errorf("unexpected index entry %q", entry)
		}
		if !filepath.IsLocal(path) {
			return fmt.Errorf("index entry %q is not a path inside the worktree", path)
		}
		inWorkspace := filepath.Join(name, path)
		info, err := root.Lstat(inWorkspace)
		if err != nil {
			continue
		}
		if info.IsDir() && fields[1] != gitlinkMode {
			if err := removeTurnsDirectory(root, name, path); err != nil {
				return fmt.Errorf("remove the directory at %s, a file in the index: %w", path, err)
			}
			continue
		}
		if fields[0] == "S" || fields[0] == "s" {
			present = append(present, path)
		}
	}
	if len(present) == 0 {
		return nil
	}
	_, err = runGitStep(ctx, sup, repo, cred, append([]string{"update-index", "--no-skip-worktree", "--"}, present...), timeout, stopGrace)
	return err
}

// gitlinkMode is the index mode of a submodule's commit.
const gitlinkMode = "160000"

// validatePullCheckout runs every check CheckoutPullRef makes before it
// spawns anything: the repo name and url (validateRepoSpec), the ref
// (reposource.ValidatePullHeadRef), the commit asked for when there is one
// (reposource.ValidateCommitSHA), and the path scope.
func validatePullCheckout(repo sessionconfig.SessionConfigReposElem, ref, wantSHA string, pathScope []string) error {
	if err := validateRepoSpec(repo); err != nil {
		return err
	}
	if err := reposource.ValidatePullHeadRef(ref); err != nil {
		return fmt.Errorf("gitclone: invalid pull request ref: %w", err)
	}
	if wantSHA != "" {
		if err := reposource.ValidateCommitSHA(wantSHA); err != nil {
			return fmt.Errorf("gitclone: invalid commit: %w", err)
		}
	}
	if err := environment.ValidatePathScope(pathScope); err != nil {
		return fmt.Errorf("gitclone: invalid path scope: %w", err)
	}
	return nil
}

// errGitStepExit marks a git step that ran and exited non-zero, as opposed
// to one that could not be spawned or did not finish.
var errGitStepExit = errors.New("git exited non-zero")

// gitStepStderrMaxBytes bounds how much of a failed step's stderr its
// error carries: the end, where git says why.
const gitStepStderrMaxBytes = 1024

// runGitStep runs `git <args...>` against repo through gitdir.Run, bounded
// by timeout, and returns its trimmed stdout. A non-zero exit is an error
// wrapping errGitStepExit and carrying the end of git's stderr, so a
// failed fetch says why (an authentication refusal, a missing ref, ...).
func runGitStep(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, args []string, timeout, stopGrace time.Duration) (string, error) {
	out, err := runGitStepRaw(ctx, sup, repo, cred, args, nil, timeout, stopGrace)
	return strings.TrimSpace(out), err
}

// runGitStepRaw is runGitStep with stdin, when it is not nil, fed to git,
// and git's stdout returned as it is, for a -z output whose first or last
// path may start or end with a space.
func runGitStepRaw(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, args []string, stdin []byte, timeout, stopGrace time.Duration) (string, error) {
	var stdout, stderr bytes.Buffer
	spec := supervisor.Spec{
		Path:   "git",
		Args:   githarden.Args(repo, args...),
		Env:    githarden.Env(nil),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if stdin != nil {
		spec.Stdin = bytes.NewReader(stdin)
	}
	result, err := gitdir.Run(ctx, sup, repo, cred, spec, timeout, stopGrace)
	name := gitStepName(args)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", name, err)
	}
	if result.Err != nil {
		return "", fmt.Errorf("git %s: %w", name, result.Err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git %s: %w (%d): %s", name, errGitStepExit, result.ExitCode, stderrTail(stderr.String()))
	}
	return stdout.String(), nil
}

// gitStepName is the subcommand args runs, for an error: the first
// argument that is not a "-c <key=value>" pair. The credential helper's
// path never reaches an error this way.
func gitStepName(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return "(none)"
}

// stderrTail is the end of s, trimmed, at most gitStepStderrMaxBytes.
func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= gitStepStderrMaxBytes {
		return s
	}
	return "..." + s[len(s)-gitStepStderrMaxBytes:]
}

// resolveCommit runs `git rev-parse --verify --quiet <rev>^{commit}`: the
// commit rev names and true when there is one, "" and false when there is
// none (exit 1), and an error for anything else. The ^{commit} peel makes
// git read the object, so a full sha absent from the repository is not
// echoed back as if it were there.
func resolveCommit(ctx context.Context, sup *supervisor.Supervisor, repo githarden.Repo, cred *syscall.Credential, rev string, timeout, stopGrace time.Duration) (string, bool, error) {
	var stdout bytes.Buffer
	spec := supervisor.Spec{
		Path:   "git",
		Args:   githarden.Args(repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}"),
		Env:    githarden.Env(nil),
		Stdout: &stdout,
	}
	result, err := gitdir.Run(ctx, sup, repo, cred, spec, timeout, stopGrace)
	if err != nil {
		return "", false, fmt.Errorf("git rev-parse --verify: %w", err)
	}
	if result.Err != nil {
		return "", false, fmt.Errorf("git rev-parse --verify: %w", result.Err)
	}
	switch result.ExitCode {
	case 0:
		return strings.TrimSpace(stdout.String()), true, nil
	case 1:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("git rev-parse --verify: exited %d", result.ExitCode)
	}
}

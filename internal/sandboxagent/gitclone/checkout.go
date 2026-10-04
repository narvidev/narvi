package gitclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// asked for, with every tracked change discarded and every untracked file
	// that is not ignored removed.
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
// The longest path is an unscoped repo whose worktree was left sparse:
// rev-parse of the ref and of the target (2), the forced checkout and the
// runtime HEAD write its move causes (2), clean (1), the sparse-checkout
// check, disable and the five-process mirror to the runtime's config (7),
// and rev-parse HEAD (1). A scoped repo takes 12, the set and its mirror
// (6) in place of those 7, and an unscoped repo that was never sparse 7.
// TestCheckoutPullRef_SpawnCount pins all three.
const (
	PullCheckoutNetworkGitSpawns  = 1
	PullCheckoutMaxLocalGitSpawns = 13
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
//  5. `checkout --force --detach <target> --`, then `clean -ffd`: every
//     tracked change, staged or not, is discarded, and every untracked
//     file that is not ignored is removed, nested repositories included.
//     Ignored files are kept, so what setup.sh installed survives. The
//     detached HEAD is synced out to the runtime's worktree by gitdir.Run
//     (gitdir.SyncHeadOut).
//  6. pathScope is re-applied, or sparse-checkout disabled for an
//     unscoped session, by the same rule syncOne's deferred step follows,
//     since the checkout reads whatever sparse state the worktree held.
//  7. `rev-parse HEAD` gives HeadSHA, which must be the target.
//  8. chownRepo re-owns the worktree for the runtime (nil skips it): the
//     fetch and the checkout ran as sandbox-agent's own identity.
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
) PullCheckoutResult {
	if err := validatePullCheckout(repo, ref, wantSHA, pathScope); err != nil {
		return PullCheckoutResult{Outcome: PullCheckoutFailed, Err: err}
	}
	credHelperArg, err := CredHelperGitArg()
	if err != nil {
		return PullCheckoutResult{Outcome: PullCheckoutFailed, Err: fmt.Errorf("gitclone: determine credential helper: %w", err)}
	}
	handle := layout.Repo(repo.Name)

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
	result := PullCheckoutResult{RefSHA: refSHA}

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

	if _, err := runGitStep(ctx, sup, handle, cred, []string{"checkout", "--quiet", "--force", "--detach", target, "--"}, stepTimeout, stopGrace); err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: check out %s in %s: %w", target, repo.Name, err)
		return result
	}
	if _, err := runGitStep(ctx, sup, handle, cred, []string{"clean", "-ffd", "--quiet"}, stepTimeout, stopGrace); err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: clean %s: %w", repo.Name, err)
		return result
	}

	if len(pathScope) > 0 {
		err = applySparseCheckout(ctx, sup, handle, cred, pathScope, stepTimeout, stopGrace)
	} else {
		err = disableSparseCheckoutIfEnabled(ctx, sup, handle, cred, stepTimeout, stopGrace)
	}
	if err != nil {
		result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: path scope for %s: %w", repo.Name, err)
		return result
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

	if chownRepo != nil {
		if err := chownRepo(handle.WorkTree); err != nil {
			result.Outcome, result.Err = PullCheckoutFailed, fmt.Errorf("gitclone: re-own %s for the runtime: %w", repo.Name, err)
			return result
		}
	}

	result.Outcome, result.HeadSHA = PullCheckoutCheckedOut, headSHA
	return result
}

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
	var stdout, stderr bytes.Buffer
	spec := supervisor.Spec{
		Path:   "git",
		Args:   githarden.Args(repo, args...),
		Env:    githarden.Env(nil),
		Stdout: &stdout,
		Stderr: &stderr,
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
	return strings.TrimSpace(stdout.String()), nil
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

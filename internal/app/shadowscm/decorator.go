// This file implements §30.2's layer 1: the typed port decorator.
//
// It exists alongside the transport gate, and the redundancy runs in one
// direction on purpose. The gate below guarantees nothing escapes even
// when this layer's coverage is stale; this layer records with real types
// and keeps the state machines that consume write results coherent, which
// a transport that only sees bytes cannot do.
//
// The decorator implements ports.SourceControl EXPLICITLY -- never by
// embedding the interface. That is the whole point of the file. An
// embedded interface would satisfy the compiler for any method added to
// the port later and pass it straight through to the live implementation,
// which is exactly the silent leak this design exists to prevent. Written
// out method by method, adding a method to the port BREAKS THE BUILD until
// someone decides, deliberately, whether it is a read or a write.
//
// Reads are forwarded untouched: reading a customer's repository leaves no
// trace, and suppressing reads would make the evaluation impossible rather
// than safe.

package shadowscm

import (
	"context"
	"errors"
	"fmt"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/shadowledger"
)

// Decorator wraps a live ports.SourceControl and suppresses its writes for
// repositories the resolver reports as shadow.
type Decorator struct {
	live   ports.SourceControl
	ledger shadowledger.Store

	// isLive reports whether repoFullName may really be written to.
	// Resolved per call, never cached: a cached answer keeps suppressing
	// after a promotion and keeps emitting after a demotion.
	isLive func(ctx context.Context, repoFullName string) bool
}

// New builds a Decorator. Every argument is required; there is no
// convenience default that yields a pass-through, because a pass-through
// obtained by omission is the failure mode this layer exists to remove.
func New(live ports.SourceControl, ledger shadowledger.Store, isLive func(context.Context, string) bool) (*Decorator, error) {
	if live == nil {
		return nil, errors.New("shadowscm: a live SourceControl is required")
	}
	if ledger == nil {
		return nil, errors.New("shadowscm: a ledger is required -- suppression that cannot be recorded is not suppression")
	}
	if isLive == nil {
		return nil, errors.New("shadowscm: an egress resolver is required")
	}
	return &Decorator{live: live, ledger: ledger, isLive: isLive}, nil
}

// This assertion is the tripwire, and the reason the implementation is
// written out rather than embedded: a method added to the port stops this
// line compiling until it is handled here.
var _ ports.SourceControl = (*Decorator)(nil)

func repoName(owner, repo string) string { return owner + "/" + repo }

func (d *Decorator) record(ctx context.Context, e shadowledger.Entry) error {
	return shadowledger.Record(ctx, d.ledger, e)
}

// CreatePR opens a pull request, or records that it would have.
func (d *Decorator) CreatePR(ctx context.Context, spec ports.CreatePRSpec) (ports.PRRef, error) {
	if d.isLive(ctx, repoName(spec.Owner, spec.Repo)) {
		return d.live.CreatePR(ctx, spec)
	}
	return d.SuppressCreatePR(ctx, spec)
}

// SuppressCreatePR records what a CreatePR would have done and returns the
// synthetic ref, WITHOUT consulting the current egress mode at all.
//
// It exists for one caller shape: a decision already frozen earlier in the
// same cycle. §30.8 resolves the push/PR pair's mode ONCE, at push send,
// and persists it -- so by the time the PR stage runs, a repo promoted
// shadow->live in between would make CreatePR's own isLive check say
// "live" and open a real pull request on a branch that was only ever
// pushed under shadow. The caller holding that frozen stamp calls here
// instead, which is the "stamp" half of §30.8's rule: suppress if the
// stamp OR the current flag says shadow, monotone toward suppression in
// both directions. The other half needs nothing extra -- a repo demoted
// live->shadow mid-cycle is caught by CreatePR's own check above.
//
// Recording is the reason this is a method rather than an early return in
// the caller. A suppressed effect that leaves no ledger row is §30.6's
// named contract violation, and the synthetic ref is what keeps the
// caller's own downstream state (the PR artifact) coherent, §30.7.
func (d *Decorator) SuppressCreatePR(ctx context.Context, spec ports.CreatePRSpec) (ports.PRRef, error) {
	synthetic := syntheticPRRef(spec.Owner, spec.Repo)
	if err := d.record(ctx, shadowledger.Entry{
		Operation:    "create_pr",
		RepoFullName: repoName(spec.Owner, spec.Repo),
		Target:       spec.Head,
		Spec: shadowledger.CreatePR{
			Owner: spec.Owner, Repo: spec.Repo, Head: spec.Head,
			Base: spec.Base, Title: spec.Title, Body: spec.Body,
		},
		Result: synthetic,
	}); err != nil {
		return ports.PRRef{}, err
	}
	return synthetic, nil
}

// UpdateFileContent commits a file change, or records that it would have.
func (d *Decorator) UpdateFileContent(ctx context.Context, spec ports.UpdateFileContentSpec) (string, error) {
	if d.isLive(ctx, repoName(spec.Owner, spec.Repo)) {
		return d.live.UpdateFileContent(ctx, spec)
	}
	if err := d.record(ctx, shadowledger.Entry{
		Operation:    "update_file_content",
		RepoFullName: repoName(spec.Owner, spec.Repo),
		Target:       spec.Path,
		Spec: shadowledger.UpdateFileContent{
			Owner: spec.Owner, Repo: spec.Repo, Path: spec.Path,
			Content: spec.Content, SHA: spec.SHA, Branch: spec.Branch, Message: spec.Message,
		},
		Result: syntheticCommitSHA,
	}); err != nil {
		return "", err
	}
	return syntheticCommitSHA, nil
}

// UpdatePRBody rewrites a pull request body, or records that it would have.
func (d *Decorator) UpdatePRBody(ctx context.Context, spec ports.UpdatePRBodySpec) error {
	if d.isLive(ctx, repoName(spec.Owner, spec.Repo)) {
		return d.live.UpdatePRBody(ctx, spec)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation:    "update_pr_body",
		RepoFullName: repoName(spec.Owner, spec.Repo),
		Target:       fmt.Sprintf("%d", spec.Number),
		Spec: shadowledger.UpdatePRBody{
			Owner: spec.Owner, Repo: spec.Repo, Number: spec.Number, Body: spec.Body,
		},
	})
}

// RegisterPRStack groups pull requests into a stack, or records that it would have.
func (d *Decorator) RegisterPRStack(ctx context.Context, spec ports.RegisterPRStackSpec) error {
	if d.isLive(ctx, repoName(spec.Owner, spec.Repo)) {
		return d.live.RegisterPRStack(ctx, spec)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation:    "register_pr_stack",
		RepoFullName: repoName(spec.Owner, spec.Repo),
		Spec: shadowledger.RegisterPRStack{
			Owner: spec.Owner, Repo: spec.Repo, PRNumbers: spec.PRNumbers,
		},
	})
}

// CreateBranch pushes a branch, or records that it would have.
func (d *Decorator) CreateBranch(ctx context.Context, spec ports.CreateBranchSpec) error {
	if d.isLive(ctx, repoName(spec.Owner, spec.Repo)) {
		return d.live.CreateBranch(ctx, spec)
	}
	return d.record(ctx, shadowledger.Entry{
		Operation:    "create_branch",
		RepoFullName: repoName(spec.Owner, spec.Repo),
		Target:       spec.Branch,
		Spec: shadowledger.CreateBranch{
			Owner: spec.Owner, Repo: spec.Repo, Branch: spec.Branch, SHA: spec.SHA,
		},
	})
}

// MergePR records and then returns ports.ErrShadowSuppressed -- never a
// synthetic merge commit SHA. See that sentinel's own doc comment and
// §30.7 for why this one write refuses a stand-in: an invented SHA becomes
// an audit row and a fake confirmation feeding the very metric whose
// evidence is supposed to justify arming auto-merge for real.
func (d *Decorator) MergePR(ctx context.Context, spec ports.MergePRSpec) (string, error) {
	if d.isLive(ctx, repoName(spec.Owner, spec.Repo)) {
		return d.live.MergePR(ctx, spec)
	}
	if err := d.record(ctx, shadowledger.Entry{
		Operation:    "merge_pr",
		RepoFullName: repoName(spec.Owner, spec.Repo),
		Target:       fmt.Sprintf("%d", spec.Number),
		Spec: shadowledger.MergePR{
			Owner: spec.Owner, Repo: spec.Repo, Number: spec.Number, HeadSHA: spec.HeadSHA,
			MergeMethod: spec.MergeMethod, CommitTitle: spec.CommitTitle, CommitMessage: spec.CommitMessage,
		},
		// Result stays nil: the row records that a merge was suppressed and
		// that nothing was invented in its place.
	}); err != nil {
		return "", err
	}
	return "", ports.ErrShadowSuppressed
}

// ResolveBranchSHA is a read and is forwarded unchanged.
func (d *Decorator) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return d.live.ResolveBranchSHA(ctx, spec)
}

// IsAncestor (D3, second adversarial-review round) is a read and is
// forwarded unchanged, mirroring ResolveBranchSHA's own identical
// precedent immediately above.
func (d *Decorator) IsAncestor(ctx context.Context, spec ports.IsAncestorSpec) (bool, error) {
	return d.live.IsAncestor(ctx, spec)
}

// ResolveContractsFingerprint is a read and is forwarded unchanged.
func (d *Decorator) ResolveContractsFingerprint(ctx context.Context, spec ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return d.live.ResolveContractsFingerprint(ctx, spec)
}

// CheckRepoAccess is a read and is forwarded unchanged.
func (d *Decorator) CheckRepoAccess(ctx context.Context, spec ports.CheckRepoAccessSpec) (bool, error) {
	return d.live.CheckRepoAccess(ctx, spec)
}

// GetFileContent is a read and is forwarded unchanged.
func (d *Decorator) GetFileContent(ctx context.Context, spec ports.GetFileContentSpec) (string, string, bool, error) {
	return d.live.GetFileContent(ctx, spec)
}

// GetPRBody is a read and is forwarded unchanged.
func (d *Decorator) GetPRBody(ctx context.Context, owner, repo string, number int, token string) (string, bool, error) {
	return d.live.GetPRBody(ctx, owner, repo, number, token)
}

// ListMergedBetween is a read and is forwarded unchanged.
func (d *Decorator) ListMergedBetween(ctx context.Context, spec ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return d.live.ListMergedBetween(ctx, spec)
}

// ListOpenPRsForUser is a read and is forwarded unchanged.
func (d *Decorator) ListOpenPRsForUser(ctx context.Context, spec ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return d.live.ListOpenPRsForUser(ctx, spec)
}

// GetOpenPR is a read and is forwarded unchanged.
func (d *Decorator) GetOpenPR(ctx context.Context, owner, repo string, number int, token string) (ports.OpenPR, bool, error) {
	return d.live.GetOpenPR(ctx, owner, repo, number, token)
}

// ResolveCodeOwners is a read and is forwarded unchanged.
func (d *Decorator) ResolveCodeOwners(ctx context.Context, spec ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return d.live.ResolveCodeOwners(ctx, spec)
}

// ---- Reads that live on the concrete adapter rather than on the port.
//
// §30.2 observes that methods exist outside ports.SourceControl; these two
// are the read half of that, reached by consumers through their own
// narrower interfaces. They are forwarded like every other read, and they
// are here so that one object satisfies every consumer -- handing the
// undecorated adapter to some callers and the decorator to others would
// mean two objects with different egress behaviour circulating under
// names that do not say which is which.
//
// The type assertion is deliberate rather than a widened field: the
// decorator's contract is ports.SourceControl, and a live implementation
// that does not carry these is a legitimate one (every test fake). Such an
// implementation reaching here is a wiring mistake, and the error says so
// instead of panicking.

// GetPullRequestDiff is a read and is forwarded unchanged.
func (d *Decorator) GetPullRequestDiff(ctx context.Context, owner, repo string, number int32, token string) (string, bool, error) {
	f, ok := d.live.(interface {
		GetPullRequestDiff(context.Context, string, string, int32, string) (string, bool, error)
	})
	if !ok {
		return "", false, errors.New("shadowscm: the wrapped source control does not fetch pull-request diffs")
	}
	return f.GetPullRequestDiff(ctx, owner, repo, number, token)
}

// GetCompareDiff is a read and is forwarded unchanged.
func (d *Decorator) GetCompareDiff(ctx context.Context, owner, repo, base, head, token string) (string, bool, error) {
	f, ok := d.live.(interface {
		GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error)
	})
	if !ok {
		return "", false, errors.New("shadowscm: the wrapped source control does not fetch compare diffs")
	}
	return f.GetCompareDiff(ctx, owner, repo, base, head, token)
}

// GetPullRequest is a read and is forwarded unchanged.
func (d *Decorator) GetPullRequest(ctx context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error) {
	f, ok := d.live.(interface {
		GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error)
	})
	if !ok {
		return githubapi.PullRequest{}, errors.New("shadowscm: the wrapped source control does not fetch pull requests")
	}
	return f.GetPullRequest(ctx, owner, repo, number, token)
}

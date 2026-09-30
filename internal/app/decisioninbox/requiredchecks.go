package decisioninbox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is where a base branch's required checks (§21.2, "CI green
// means the required checks, not the checks that reported") become the
// fact the eligibility engine reads: autoapproval.EligibilityInput.
// RequiredChecks. Both of this package's eligibility call sites -- the
// read model (computeRealEligibility, aggregate.go) and the merge path
// (revalidateCore, revalidate.go, which the auto-merge worker shares) --
// build the fact here, from the same ports.OpenPR the CI read came from,
// and decide through the same engine.
//
// Which credential reads the requirements follows who acts:
//   - the merge path reads them with the credential that makes the merge:
//     the person's own token on a Merge click, the bot's on the auto-merge
//     worker. A person's Merge click therefore does not depend on GitHub
//     outbound, exactly as the merge it makes does not.
//   - the read model, which merges nothing, reads them as the bot
//     (Deps.GitHubOutbound), one cached answer per base branch for every
//     actor. With GitHub outbound off it reads nothing, and says so: its
//     rows are not ready to merge, the inbox is not degraded, and
//     Result.RequiredChecksNotRead is set -- a configuration, not a
//     failure to retry.
//
// They also differ in WHEN they read, for a reason each states at its call
// site: the merge path reads before its probe, so a required check that is
// still running or failed is named in the refusal it returns; the read
// model shows no reason, and reads only once its probe has passed, like
// its other live reads. The auto-merge worker reads each base once per
// tick (RequiredChecksMemo).
//
// A commit status an App posted counts for a check that names an App only
// when that App, by id, posted it. The head's own read identifies the App
// when a check run of the same App at the head carries its id; when none
// does, the fact identifies it by slug (resolveStatusApps,
// ports.SourceControl.ResolveAppID) with the same credential as the
// requirements. A slug that names no App is an answer: that App cannot be
// identified, and its status never counts. Any other failure to read an
// App is a failed read of the requirements, handled exactly like one: the
// fact is "could not be read", a probe stands it in as "requires
// nothing", and it degrades the inbox only when it decides the row. Each
// slug is read at most once per inbox load or auto-merge tick
// (appIDReads), and a failure is logged once there.

// requiredChecksSpec is the read of pr's base branch's requirements with
// token.
func requiredChecksSpec(pr ports.OpenPR, token string) ports.ListRequiredChecksSpec {
	return ports.ListRequiredChecksSpec{Owner: pr.Owner, Repo: pr.Repo, Branch: pr.BaseRef, Token: token}
}

// RequiredChecksMemo holds one auto-merge tick's reads of base branches'
// requirements, so candidates into the same base share one read instead
// of repeating it per candidate: the auto-merge worker creates one per
// tick and hands it to RevalidateForAutoMerge. A failed read is kept too,
// for the rest of the tick, and the next tick reads again. A person's
// Merge click never uses one: it reads live, every time. Safe for
// concurrent use.
type RequiredChecksMemo struct {
	mu      sync.Mutex
	entries map[requiredChecksCacheKey]requiredChecksMemoEntry
	// apps is the tick's App reads (appIDReads).
	apps *appIDReads
}

type requiredChecksMemoEntry struct {
	required []ports.RequiredCheck
	err      error
}

// NewRequiredChecksMemo returns an empty memo, for one tick.
func NewRequiredChecksMemo() *RequiredChecksMemo {
	return &RequiredChecksMemo{entries: map[requiredChecksCacheKey]requiredChecksMemoEntry{}, apps: newAppIDReads()}
}

// appReads is the App reads to share: the tick's when m is a tick's memo,
// and a fresh set, for one call, when m is nil (a Merge click).
func (m *RequiredChecksMemo) appReads() *appIDReads {
	if m == nil || m.apps == nil {
		return newAppIDReads()
	}
	return m.apps
}

// readRequiredChecksLive reads spec's requirements through sourceControl,
// bounded by deps.Timeouts.DecisionInboxRequiredChecksTimeout -- once per
// base per tick when memo is non-nil, every call when it is nil.
func readRequiredChecksLive(ctx context.Context, deps Deps, sourceControl ports.SourceControl, memo *RequiredChecksMemo, spec ports.ListRequiredChecksSpec) ([]ports.RequiredCheck, error) {
	key := requiredChecksCacheKey{owner: spec.Owner, repo: spec.Repo, branch: spec.Branch}
	if memo != nil {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if e, ok := memo.entries[key]; ok {
			return e.required, e.err
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.DecisionInboxRequiredChecksTimeout)
	required, err := sourceControl.ListRequiredChecks(readCtx, spec)
	cancel()
	if memo != nil {
		memo.entries[key] = requiredChecksMemoEntry{required: required, err: err}
	}
	return required, err
}

// requiredChecksFact is the fact for one read of pr's base requirements:
// evaluated against the checks pr's own CI read listed at its head, their
// Apps identified by resolve where the requirements need it
// (resolveStatusApps), when every read succeeded. When the requirements
// read failed, or an App read did (a slug that names no App is not a
// failure), it is the zero value -- "could not be read", which the engine
// refuses on -- returned with the failure. A failed read is never turned
// into "requires nothing", nor into a considered shortfall.
func requiredChecksFact(ctx context.Context, required []ports.RequiredCheck, readErr error, pr ports.OpenPR, resolve appIDResolver) (autoapproval.RequiredChecks, error) {
	if readErr != nil {
		return autoapproval.RequiredChecks{}, readErr
	}
	head, err := resolveStatusApps(ctx, required, pr, resolve)
	if err != nil {
		return autoapproval.RequiredChecks{}, err
	}
	return autoapproval.ReadRequiredChecks(toDomainRequiredChecks(required), toDomainHeadChecks(head), !pr.HeadChecksListDegraded), nil
}

// appIDResolver identifies the App behind slug by id
// (ports.SourceControl.ResolveAppID), with the credential the requirements
// were read with.
type appIDResolver func(ctx context.Context, slug string) (int64, error)

// liveAppIDResolver resolves through sourceControl with token, each call
// bounded by deps.Timeouts.DecisionInboxResolveAppIDTimeout -- the merge
// path's resolver. The adapter keeps each App's id for
// platform.Timeouts.GitHubAppIDCacheTTL once read, so a slug is not read
// again on every merge.
func liveAppIDResolver(deps Deps, sourceControl ports.SourceControl, token string) appIDResolver {
	return func(ctx context.Context, slug string) (int64, error) {
		callCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.DecisionInboxResolveAppIDTimeout)
		defer cancel()
		return sourceControl.ResolveAppID(callCtx, ports.ResolveAppIDSpec{Slug: slug, Token: token})
	}
}

// appIDReads holds one inbox load's, or one auto-merge tick's, App reads
// by slug -- the id, or the error -- so each slug is read at most once
// there, whatever the number of rows and eligibility calls, and a failure
// is logged once. The next load or tick reads again: nothing here outlives
// it. Safe for concurrent use.
type appIDReads struct {
	mu      sync.Mutex
	entries map[string]appIDRead
}

type appIDRead struct {
	id  int64
	err error
}

func newAppIDReads() *appIDReads {
	return &appIDReads{entries: map[string]appIDRead{}}
}

// through returns a resolver answering from r, and reading with read the
// first time a slug is asked for.
func (r *appIDReads) through(read appIDResolver) appIDResolver {
	if r == nil {
		r = newAppIDReads()
	}
	return func(ctx context.Context, slug string) (int64, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if e, ok := r.entries[slug]; ok {
			return e.id, e.err
		}
		id, err := read(ctx, slug)
		switch {
		case errors.Is(err, ports.ErrAppNotFound):
			platform.Logger(ctx).Info("decisioninbox: no App has the slug of the bot account that posted a commit status; its status will not count for a required check naming an App", "app_slug", slug)
		case err != nil:
			platform.Logger(ctx).Warn("decisioninbox: read the App behind a commit status failed -- the base branch's required checks read as not read, and eligibility fails closed via ReasonRequiredChecksUnknown unless another criterion refuses first", "error", err, "app_slug", slug)
		}
		r.entries[slug] = appIDRead{id: id, err: err}
		return id, err
	}
}

// resolveStatusApps returns pr's head checks with the App behind a commit
// status identified by id where the requirements need it: a status an App
// posted (its slug known, HeadCheck.AppSlug) that no check run at the head
// identified, carrying the name of a required check that names an App.
// Nothing else is resolved, so a base that names no App costs no read.
// Each slug is resolved at most once per call. A slug that names no App
// (ports.ErrAppNotFound) leaves the id zero: an App the rule reads as
// unidentified, whose status never counts for a check that names an App.
// Any other failure is returned: the head's Apps could not be read.
func resolveStatusApps(ctx context.Context, required []ports.RequiredCheck, pr ports.OpenPR, resolve appIDResolver) ([]ports.HeadCheck, error) {
	appNamed := make(map[string]bool, len(required))
	for _, c := range required {
		if c.AppID != 0 {
			appNamed[c.Name] = true
		}
	}
	if len(appNamed) == 0 {
		return pr.HeadChecks, nil
	}
	out := make([]ports.HeadCheck, len(pr.HeadChecks))
	copy(out, pr.HeadChecks)
	resolved := map[string]int64{}
	notFound := map[string]bool{}
	for i, c := range out {
		if c.Source != ports.HeadCheckSourceStatus || c.Poster != ports.HeadCheckPosterApp || c.AppID != 0 || c.AppSlug == "" || !appNamed[c.Name] {
			continue
		}
		if notFound[c.AppSlug] {
			continue
		}
		id, ok := resolved[c.AppSlug]
		if !ok {
			got, err := resolve(ctx, c.AppSlug)
			switch {
			case errors.Is(err, ports.ErrAppNotFound):
				notFound[c.AppSlug] = true
				continue
			case err != nil:
				return nil, fmt.Errorf("decisioninbox: read the App behind the commit status %q (app %q): %w", c.Name, c.AppSlug, err)
			}
			resolved[c.AppSlug] = got
			id = got
		}
		out[i].AppID = id
	}
	return out, nil
}

// probeRequiredChecks is the stand-in a probe uses for fact: fact itself
// when the requirements were read, and "read, requiring nothing" when they
// were not -- the most lenient value, like the probes' assumed base SHA.
// An unread requirement can only refuse, never approve, so a probe that
// refuses with this stand-in refuses with the real fact too, on its own
// reason; and the final call, which gets the real fact, is the one that
// refuses on the failed read -- only once every other criterion has
// passed, so a transient failure never stands in for a pull request's
// real, lasting reason.
func probeRequiredChecks(fact autoapproval.RequiredChecks) autoapproval.RequiredChecks {
	if fact.Known {
		return fact
	}
	return autoapproval.ReadRequiredChecks(nil, nil, true)
}

func toDomainRequiredChecks(in []ports.RequiredCheck) []autoapproval.RequiredCheck {
	out := make([]autoapproval.RequiredCheck, 0, len(in))
	for _, c := range in {
		out = append(out, autoapproval.RequiredCheck{Name: c.Name, AppID: c.AppID})
	}
	return out
}

func toDomainHeadChecks(in []ports.HeadCheck) []autoapproval.HeadCheck {
	out := make([]autoapproval.HeadCheck, 0, len(in))
	for _, c := range in {
		out = append(out, autoapproval.HeadCheck{
			Name:              c.Name,
			Source:            toDomainCheckSource(c.Source),
			AppID:             c.AppID,
			Poster:            toDomainPoster(c.Poster),
			EarlierFromOthers: c.EarlierFromOthers,
			State:             toDomainCheckState(c.State),
		})
	}
	return out
}

// toDomainPoster maps a port poster to the domain's. An unknown value maps
// to unknown, which never lets a status count for a check tied to an App.
func toDomainPoster(p ports.HeadCheckPoster) autoapproval.Poster {
	switch p {
	case ports.HeadCheckPosterApp:
		return autoapproval.PosterApp
	case ports.HeadCheckPosterPerson:
		return autoapproval.PosterPerson
	default:
		return autoapproval.PosterUnknown
	}
}

// toDomainCheckSource maps a port source to the domain's. An unknown
// source maps to a commit status, which never satisfies a check tied to
// an App -- the conservative reading.
func toDomainCheckSource(s ports.HeadCheckSource) autoapproval.CheckSource {
	if s == ports.HeadCheckSourceCheckRun {
		return autoapproval.CheckSourceCheckRun
	}
	return autoapproval.CheckSourceStatus
}

// toDomainCheckState maps a port state to the domain's. An unknown state
// maps to failed, which never satisfies a requirement.
func toDomainCheckState(s ports.HeadCheckState) autoapproval.CheckState {
	switch s {
	case ports.HeadCheckStatePassed:
		return autoapproval.CheckStatePassed
	case ports.HeadCheckStatePending:
		return autoapproval.CheckStatePending
	default:
		return autoapproval.CheckStateFailed
	}
}

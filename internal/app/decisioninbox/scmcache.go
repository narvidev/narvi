// Package decisioninbox is the app-layer read-model aggregator backing
// the decision inbox ("decision inbox: read model + API", §16).
// It combines existing Postgres state (plans, sessions, automations,
// outbox, review_findings, sentinel_fixes, artifacts) with live
// SourceControl data into the four-kind taxonomy internal/domain/
// decisioninbox classifies and ranks -- see aggregate.go's own doc
// comment for the full per-kind design. This package holds NO new
// authoritative state of its own: the SCM cache below is exactly what
// §16.2 calls for ("SCM data is cached with a short TTL... never
// presented as live truth"), never a second source of truth for anything
// Postgres or GitHub already owns.
package decisioninbox

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// ttlEntry is one cached value plus when it was fetched.
type ttlEntry[V any] struct {
	value     V
	fetchedAt time.Time
	expiresAt time.Time
}

// ttlCache is a small, generic, mutex-guarded TTL cache. Mirrors
// internal/app/sessionactor/repoaccesscache.go's own established shape
// (opportunistic expired-entry sweep on write, no dedicated background
// sweeper -- that file's own doc comment: "no generic TTL-cache utility
// exists anywhere in this repo", confirmed still true immediately before
// writing this one) with ONE addition that cache doesn't need: fetchedAt
// is returned back to the caller by get, since §16.2's whole point --
// unlike repoAccessCache's own pure allow/deny gate -- is SURFACING
// staleness to the end user ("as of 2 min ago"), never silently trusting
// the cache.
//
// Generic over (K, V) rather than one hand-copied cache per SCM data kind
// this Step caches (ListOpenPRsForUser's own []ports.OpenPR,
// ResolveCodeOwners' own []ports.Owner) -- a genuinely generic shape is
// straightforward to write once and reuse, rather than repeating
// repoAccessCache's own "no existing generic TTL-cache utility" gap a
// second time in the same codebase.
type ttlCache[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]ttlEntry[V]
}

func newTTLCache[K comparable, V any]() *ttlCache[K, V] {
	return &ttlCache[K, V]{entries: make(map[K]ttlEntry[V])}
}

// get returns the cached value for key, plus when it was fetched, if a
// live (unexpired) entry exists. ok=false covers both "never fetched" and
// "fetched, but the TTL has since elapsed" uniformly.
func (c *ttlCache[K, V]) get(key K, now time.Time) (value V, fetchedAt time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, found := c.entries[key]
	if !found || !now.Before(entry.expiresAt) {
		var zero V
		return zero, time.Time{}, false
	}
	return entry.value, entry.fetchedAt, true
}

// set records a fresh value for key, fetched at now, valid until now+ttl.
func (c *ttlCache[K, V]) set(key K, value V, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sweepExpiredLocked(now)
	c.entries[key] = ttlEntry[V]{value: value, fetchedAt: now, expiresAt: now.Add(ttl)}
}

// sweepExpiredLocked removes every already-expired entry -- callers must
// already hold c.mu. Piggybacks on set's own call (which already holds
// the lock and has a fresh now), mirroring repoAccessCache.
// sweepExpiredLocked's own identical precedent exactly.
func (c *ttlCache[K, V]) sweepExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

// codeOwnersCacheKey identifies one ResolveCodeOwners lookup -- a PR's own
// changed-file set is typically stable across repeated inbox loads within
// one cache TTL window, so caching per (repo, ref, joined paths) still
// hits efficiently on a repeat load of the SAME PR.
type codeOwnersCacheKey struct {
	owner, repo, ref string
	pathsKey         string
}

func codeOwnersKey(owner, repo, ref string, paths []string) codeOwnersCacheKey {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	return codeOwnersCacheKey{owner: owner, repo: repo, ref: ref, pathsKey: strings.Join(sorted, "\x00")}
}

// branchSHACacheKey identifies one ResolveBranchSHA lookup -- (owner,
// repo, branch), mirroring codeOwnersCacheKey's own identical per-repo/
// per-ref keying immediately above.
type branchSHACacheKey struct {
	owner, repo, branch string
}

// SCMCache is the §16.2 short-TTL cache wrapping every live SourceControl
// read the decision-inbox aggregator makes -- constructed once and
// threaded through Deps, exactly like repoAccessCache is constructed once
// per Registry and threaded through every Actor (see that file's own doc
// comment for the identical reasoning: a per-request cache would mean a
// second concurrent request moments later never benefits from the first
// request's already-paid-for fetch).
type SCMCache struct {
	sourceControl ports.SourceControl
	timeouts      platform.Timeouts

	openPRs           *ttlCache[string, []ports.OpenPR]
	codeOwners        *ttlCache[codeOwnersCacheKey, []ports.Owner]
	branchSHAs        *ttlCache[branchSHACacheKey, string]
	isAncestorResults *ttlCache[isAncestorCacheKey, bool]
}

// NewSCMCache builds an SCMCache wrapping sourceControl.
func NewSCMCache(sourceControl ports.SourceControl, timeouts platform.Timeouts) *SCMCache {
	return &SCMCache{
		sourceControl:     sourceControl,
		timeouts:          timeouts,
		openPRs:           newTTLCache[string, []ports.OpenPR](),
		codeOwners:        newTTLCache[codeOwnersCacheKey, []ports.Owner](),
		branchSHAs:        newTTLCache[branchSHACacheKey, string](),
		isAncestorResults: newTTLCache[isAncestorCacheKey, bool](),
	}
}

// ResolveBranchSHA returns spec's own branch's CURRENT commit SHA,
// live-fetching on a cache miss/expiry and caching the result for
// platform.Timeouts.DecisionInboxSCMCacheTTL -- mirrors
// ListOpenPRsForUser/ResolveCodeOwners above exactly.
//
// D2 (second adversarial-review round): this method is what
// computeRealEligibility (aggregate.go) now calls to supply
// autoapproval.EligibilityInput.CurrentBaseSHA, replacing a direct read
// of ports.OpenPR.BaseSHA -- GitHub's own per-PR CACHED "base.sha"
// snapshot, verified to lag the base branch's real tip by an unknown,
// sometimes month-scale margin (see that field's own doc comment,
// ports/sourcecontrol.go). revalidateCore (revalidate.go) already
// resolves this SAME kind of value -- a LIVE branch-tip read -- but
// deliberately bypasses every cache (that function's own doc comment:
// "the whole point of this function is a fresh read"), since it backs an
// ACTION endpoint (merge). computeRealEligibility backs a READ MODEL
// instead (§16.2: "SCM data is cached with a short TTL... never presented
// as live truth" -- explicitly the posture this whole cache exists for),
// so caching this call here, exactly like every other SCM read this
// aggregator makes, is correct: both callers now supply the SAME KIND of
// value (a live-resolved tip, never the stale per-PR snapshot) to
// autoapproval.ComputeEligible, differing only in how fresh "live" is
// allowed to be for their own, differently-scoped purposes.
//
// A resolution failure is propagated as err, exactly like
// ListOpenPRsForUser/ResolveCodeOwners above -- computeRealEligibility's
// own caller degrades that to an empty CurrentBaseSHA, which
// autoapproval.ComputeEligible's own ReasonBaseSHAUnknown guard then
// fails closed on, mirroring revalidateCore's own identical degradation.
func (c *SCMCache) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec, now time.Time) (sha string, asOf time.Time, err error) {
	key := branchSHACacheKey{owner: spec.Owner, repo: spec.Repo, branch: spec.Branch}
	if cached, fetchedAt, ok := c.branchSHAs.get(key, now); ok {
		return cached, fetchedAt, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeouts.DecisionInboxResolveBranchSHATimeout)
	defer cancel()
	sha, _, err = c.sourceControl.ResolveBranchSHA(callCtx, spec)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("decisioninbox: resolve branch sha: %w", err)
	}

	fetchedAt := time.Now()
	c.branchSHAs.set(key, sha, fetchedAt, c.timeouts.DecisionInboxSCMCacheTTL)
	return sha, fetchedAt, nil
}

// isAncestorCacheKey identifies one IsAncestor lookup -- (owner, repo,
// ancestor sha, descendant sha), mirroring branchSHACacheKey's own
// identical per-repo keying immediately above, one comparison further.
type isAncestorCacheKey struct {
	owner, repo, ancestor, descendant string
}

// IsAncestor (D3, second adversarial-review round) reports whether
// spec.Ancestor is an ancestor of (or identical to) spec.Descendant,
// live-fetching on a cache miss/expiry and caching the result for
// platform.Timeouts.DecisionInboxSCMCacheTTL -- mirrors ResolveBranchSHA
// immediately above exactly, and for the identical reason: this backs
// computeRealEligibility's own READ MODEL (aggregate.go), which §16.2
// already licenses to serve a short-TTL-cached view rather than an
// instantaneous-fresh one.
//
// See ports.SourceControl.IsAncestor's own doc comment for what this
// answers and why: computeRealEligibility (and revalidateCore, which
// bypasses this cache exactly like it bypasses ResolveBranchSHA's own
// cache, for the identical "action endpoint needs a fresh read" reason)
// consult this only when a verdict's own recorded base sha differs from
// the base branch's current live tip but the base REF is unchanged --
// tolerating an ORDINARY, unrelated merge to the base branch between
// review and merge/revalidation, while still refusing a genuine rewrite.
func (c *SCMCache) IsAncestor(ctx context.Context, spec ports.IsAncestorSpec, now time.Time) (isAncestor bool, err error) {
	key := isAncestorCacheKey{owner: spec.Owner, repo: spec.Repo, ancestor: spec.Ancestor, descendant: spec.Descendant}
	if cached, _, ok := c.isAncestorResults.get(key, now); ok {
		return cached, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeouts.DecisionInboxIsAncestorTimeout)
	defer cancel()
	isAncestor, err = c.sourceControl.IsAncestor(callCtx, spec)
	if err != nil {
		return false, fmt.Errorf("decisioninbox: is ancestor: %w", err)
	}

	c.isAncestorResults.set(key, isAncestor, time.Now(), c.timeouts.DecisionInboxSCMCacheTTL)
	return isAncestor, nil
}

// ListOpenPRsForUser returns spec's own open-PR list, live-fetching on a
// cache miss/expiry and caching the result for platform.Timeouts.
// DecisionInboxSCMCacheTTL. asOf is the instant the returned data was
// ACTUALLY fetched (a cache hit returns the ORIGINAL fetch's own instant,
// never now) -- the "as of 2 min ago" staleness §16.2 requires be
// displayed, never silently masked.
//
// truncated (threading through
// ports.SourceControl.ListOpenPRsForUser's own identical return, which
// this method previously consumed only to decide whether to cache,
// silently dropping it from its OWN return signature) carries the same
// "degraded/partial read" meaning: true iff one of
// GitHub's two underlying search queries itself failed while the other
// still returned a real, if incomplete, result. A cache HIT always
// reports truncated=false -- see the "never cached" paragraph below for
// why a truncated result can never enter c.openPRs in the first place, so
// there is nothing for a hit to ever return but false here. The caller
// (aggregate.go's buildPRItems) folds this into Result.SCMFetchFailed --
// see that field's own doc comment for the full producer list.
//
// A truncated fetch is NEVER cached: the caller still gets today's
// best-effort partial result (never blanked out), but writing it into the
// cache for the full TTL would silently present a transient, partial read
// as a confirmed-complete, fresh empty-or-partial queue for up to
// DecisionInboxSCMCacheTTL -- the exact "never presented as live truth"
// hazard §16.2 exists to prevent. Only the NEXT request is affected, by
// retrying live instead of serving a stale, possibly-still-wrong cache
// hit.
func (c *SCMCache) ListOpenPRsForUser(ctx context.Context, spec ports.ListOpenPRsForUserSpec, now time.Time) (prs []ports.OpenPR, asOf time.Time, truncated bool, err error) {
	if cached, fetchedAt, ok := c.openPRs.get(spec.GitHubExternalID, now); ok {
		return cached, fetchedAt, false, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeouts.GitHubListOpenPRsForUserTimeout)
	prs, truncated, err = c.sourceControl.ListOpenPRsForUser(callCtx, spec)
	cancel()
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("decisioninbox: list open prs for user: %w", err)
	}

	// fetchedAt ("born-expired entries"): anchored on
	// FETCH COMPLETION, a fresh now taken AFTER the (potentially slow, up
	// to GitHubListOpenPRsForUserTimeout == 3 minutes) call above returns
	// -- deliberately NOT the pre-fetch `now` parameter this method was
	// called with. expiresAt was previously computed from that pre-fetch
	// instant while the fetch itself could take up to 3 minutes against a
	// 2-minute TTL (DecisionInboxSCMCacheTTL): a slow fetch for a heavy
	// user could write an entry get() would immediately reject as already
	// expired, so the cache never amortized for exactly the users it
	// exists to help. The displayed asOf below is this SAME completion
	// instant -- still the real, honest "when the data was actually
	// fetched" moment §16.2 requires, just measured at the end of the
	// call rather than the (arbitrarily earlier, for a slow fetch) start.
	fetchedAt := time.Now()

	if truncated {
		platform.Logger(ctx).Warn("decisioninbox: list open prs for user returned a truncated/degraded result -- not caching", "github_external_id", spec.GitHubExternalID)
		return prs, fetchedAt, true, nil
	}

	c.openPRs.set(spec.GitHubExternalID, prs, fetchedAt, c.timeouts.DecisionInboxSCMCacheTTL)
	return prs, fetchedAt, false, nil
}

// ResolveCodeOwners returns spec's own owner resolution, live-fetching on
// a cache miss/expiry and caching the result -- mirrors ListOpenPRsForUser
// above, keyed on (owner, repo, ref, sorted paths).
//
// fetchedAt/asOf are anchored on FETCH COMPLETION, exactly like
// ListOpenPRsForUser's own identical "born-expired entries" fix above
// (that fix was previously applied
// ONLY to the openPRs cache -- this method still anchored both expiresAt
// and the returned asOf on the caller's PRE-fetch `now` parameter, stale
// by the whole duration of whichever ResolveCodeOwners call this races
// against inside the SAME Build call, e.g. the openPRs fetch that
// typically precedes it). A slow call (bounded by
// GitHubResolveCodeOwnersTimeout) against the SAME DecisionInboxSCMCacheTTL
// this cache uses for openPRs could otherwise write an entry get() would
// immediately reject as already expired.
func (c *SCMCache) ResolveCodeOwners(ctx context.Context, spec ports.ResolveCodeOwnersSpec, now time.Time) (owners []ports.Owner, asOf time.Time, err error) {
	key := codeOwnersKey(spec.Owner, spec.Repo, spec.Ref, spec.Paths)
	if cached, fetchedAt, ok := c.codeOwners.get(key, now); ok {
		return cached, fetchedAt, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeouts.GitHubResolveCodeOwnersTimeout)
	defer cancel()
	owners, err = c.sourceControl.ResolveCodeOwners(callCtx, spec)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("decisioninbox: resolve code owners: %w", err)
	}

	fetchedAt := time.Now()
	c.codeOwners.set(key, owners, fetchedAt, c.timeouts.DecisionInboxSCMCacheTTL)
	return owners, fetchedAt, nil
}

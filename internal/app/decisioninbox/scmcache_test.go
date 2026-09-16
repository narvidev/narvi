// Unit tests for SCMCache (scmcache.go) -- deliberately NOT gated behind
// the "integration" build tag: SCMCache wraps a ports.SourceControl (a
// plain interface, faked below) and holds no Postgres dependency of its
// own, so these run under a plain `go test`, no container required.
// Mirrors this package's own aggregate_integration_test.go
// fakeDecisionInboxSourceControl precedent, but as a SEPARATE fake (that
// one lives behind the "integration" build tag and would not be visible
// to this file).
package decisioninbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeSCMCacheSourceControl is a minimal, call-counting, concurrency-safe
// test-only ports.SourceControl -- narrowed to exactly the three methods
// SCMCache itself calls (ListOpenPRsForUser, ResolveCodeOwners,
// ResolveBranchSHA -- D2's own addition, second adversarial-review
// round); every other method returns a plain "not implemented" error.
type fakeSCMCacheSourceControl struct {
	mu sync.Mutex

	openPRsByExternalID map[string][]ports.OpenPR
	openPRsCallCount    map[string]int
	// openPRsDelay simulates a slow live fetch -- the "born-expired
	// entries" test below needs a REAL fetch duration that outlasts a
	// (deliberately tiny, test-only) TTL to actually exercise the bug.
	openPRsDelay time.Duration
	// openPRsTruncated/openPRsErr (
	// this fake previously hardcoded truncated=false and never errored,
	// so neither the truncated->never-cached path nor the plain-error
	// path below this struct had any test coverage at all) let a test
	// drive ListOpenPRsForUser's own two degraded outcomes.
	openPRsTruncated bool
	openPRsErr       error

	// codeOwnersDelay/codeOwnersCallCount mirror openPRsDelay/
	// openPRsCallCount immediately above, for ResolveCodeOwners instead
	// (the born-expired fix was
	// previously applied only to the openPRs cache -- ResolveCodeOwners'
	// own identical bug had no test at all, mirroring
	// TestSCMCache_ListOpenPRsForUser_SlowFetchDoesNotBornExpire below).
	codeOwnersDelay     time.Duration
	codeOwnersCallCount int

	// resolveBranchSHA/resolveBranchSHAErr/resolveBranchSHADelay/
	// resolveBranchSHACallCount (D2, second adversarial-review round)
	// mirror the openPRs fields' own identical shape, one method further,
	// backing SCMCache.ResolveBranchSHA's own tests below.
	resolveBranchSHA          string
	resolveBranchSHAErr       error
	resolveBranchSHADelay     time.Duration
	resolveBranchSHACallCount int

	// isAncestorResult/isAncestorErr/isAncestorCallCount (D3, second
	// adversarial-review round) back SCMCache.IsAncestor's own tests
	// below, mirroring resolveBranchSHA's own identical shape.
	isAncestorResult    bool
	isAncestorErr       error
	isAncestorCallCount int
}

var _ ports.SourceControl = (*fakeSCMCacheSourceControl)(nil)

func (f *fakeSCMCacheSourceControl) ListOpenPRsForUser(ctx context.Context, spec ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	f.mu.Lock()
	if f.openPRsCallCount == nil {
		f.openPRsCallCount = map[string]int{}
	}
	f.openPRsCallCount[spec.GitHubExternalID]++
	delay := f.openPRsDelay
	prs := f.openPRsByExternalID[spec.GitHubExternalID]
	truncated := f.openPRsTruncated
	err := f.openPRsErr
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	if err != nil {
		return nil, false, err
	}

	return prs, truncated, nil
}

func (f *fakeSCMCacheSourceControl) callCount(externalID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openPRsCallCount[externalID]
}

func (f *fakeSCMCacheSourceControl) ResolveCodeOwners(ctx context.Context, _ ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	f.mu.Lock()
	f.codeOwnersCallCount++
	delay := f.codeOwnersDelay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (f *fakeSCMCacheSourceControl) codeOwnersCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codeOwnersCallCount
}
func (f *fakeSCMCacheSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("fakeSCMCacheSourceControl: CreatePR not implemented")
}
func (f *fakeSCMCacheSourceControl) ResolveBranchSHA(ctx context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.mu.Lock()
	f.resolveBranchSHACallCount++
	delay := f.resolveBranchSHADelay
	sha := f.resolveBranchSHA
	err := f.resolveBranchSHAErr
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if err != nil {
		return "", "", err
	}
	return sha, spec.Branch, nil
}

func (f *fakeSCMCacheSourceControl) resolveBranchSHACalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolveBranchSHACallCount
}

// IsAncestor (D3, second adversarial-review round) backs
// SCMCache.IsAncestor's own tests below -- mirrors ResolveBranchSHA's own
// identical shape immediately above, one comparison further.
func (f *fakeSCMCacheSourceControl) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	f.mu.Lock()
	f.isAncestorCallCount++
	err := f.isAncestorErr
	result := f.isAncestorResult
	f.mu.Unlock()

	if err != nil {
		return false, err
	}
	return result, nil
}

func (f *fakeSCMCacheSourceControl) isAncestorCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isAncestorCallCount
}
func (f *fakeSCMCacheSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("fakeSCMCacheSourceControl: ResolveContractsFingerprint not implemented")
}
func (f *fakeSCMCacheSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("fakeSCMCacheSourceControl: CheckRepoAccess not implemented")
}
func (f *fakeSCMCacheSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("fakeSCMCacheSourceControl: GetFileContent not implemented")
}
func (f *fakeSCMCacheSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("fakeSCMCacheSourceControl: UpdateFileContent not implemented")
}
func (f *fakeSCMCacheSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("fakeSCMCacheSourceControl: RegisterPRStack not implemented")
}
func (f *fakeSCMCacheSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("fakeSCMCacheSourceControl: ListMergedBetween not implemented")
}
func (f *fakeSCMCacheSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("fakeSCMCacheSourceControl: CreateBranch not implemented")
}
func (f *fakeSCMCacheSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("fakeSCMCacheSourceControl: MergePR not implemented")
}
func (f *fakeSCMCacheSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("fakeSCMCacheSourceControl: GetOpenPR not implemented")
}
func (f *fakeSCMCacheSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("fakeSCMCacheSourceControl: GetPRBody not implemented")
}
func (f *fakeSCMCacheSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("fakeSCMCacheSourceControl: UpdatePRBody not implemented")
}

// TestSCMCache_ListOpenPRsForUser_IsolatesByExternalID proves the cache
// key (spec.GitHubExternalID) is what actually isolates one user's own
// cached PR list from another's -- SCMCache is
// constructed ONCE, process-wide, and shared across every actor's own
// inbox load; the cache key is the ENTIRE tenant-isolation boundary. A
// mutation replacing that key with a constant would make this test's own
// second call (bob) return alice's already-cached result instead of ever
// reaching bob's own fake data.
func TestSCMCache_ListOpenPRsForUser_IsolatesByExternalID(t *testing.T) {
	t.Parallel()

	fake := &fakeSCMCacheSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{
			"alice-external-id": {{Number: 111, Title: "alice's private PR"}},
			"bob-external-id":   {{Number: 222, Title: "bob's private PR"}},
		},
	}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())
	now := time.Now()

	alicePRs, _, _, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "alice-external-id"}, now)
	if err != nil {
		t.Fatalf("alice's call error = %v", err)
	}
	if len(alicePRs) != 1 || alicePRs[0].Number != 111 {
		t.Fatalf("alice's PRs = %+v, want exactly PR #111", alicePRs)
	}

	bobPRs, _, _, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "bob-external-id"}, now)
	if err != nil {
		t.Fatalf("bob's call error = %v", err)
	}
	if len(bobPRs) != 1 || bobPRs[0].Number != 222 {
		t.Fatalf("bob's PRs = %+v, want exactly PR #222 -- got alice's own cached result instead (cache key is not isolating by external ID)", bobPRs)
	}
	for _, pr := range bobPRs {
		if pr.Number == 111 {
			t.Fatalf("bob's own result leaked alice's PR #111: %+v", bobPRs)
		}
	}

	// Both distinct external IDs must have actually reached the
	// underlying fetch -- neither a hit against the OTHER's entry.
	if got := fake.callCount("alice-external-id"); got != 1 {
		t.Errorf("alice fetch called %d times, want 1", got)
	}
	if got := fake.callCount("bob-external-id"); got != 1 {
		t.Errorf("bob fetch called %d times, want 1", got)
	}
}

// TestSCMCache_ListOpenPRsForUser_CacheHitReturnsOriginalFetchInstant
// proves a cache HIT returns the ORIGINAL fetch's own instant, never the
// hit's own later `now` ( §16.2's own "never
// presented as live truth" invariant: `return cached, now, nil` on the
// hit path would pass every OTHER existing test, since the one prior
// cache-hit coverage shared `now` between the miss and the hit call,
// making the two instants indistinguishable). A real time.Sleep between
// the two calls here makes firstAsOf and a hit-path `now` bug
// unmistakably distinguishable.
func TestSCMCache_ListOpenPRsForUser_CacheHitReturnsOriginalFetchInstant(t *testing.T) {
	t.Parallel()

	fake := &fakeSCMCacheSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{"1": {{Number: 1}}},
	}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())

	_, firstAsOf, _, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, time.Now())
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	secondNow := time.Now()
	_, secondAsOf, _, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, secondNow)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}

	if got := fake.callCount("1"); got != 1 {
		t.Fatalf("fetch called %d times, want 1 (the second call must be a cache hit, well within DecisionInboxSCMCacheTTL)", got)
	}
	if !secondAsOf.Equal(firstAsOf) {
		t.Errorf("second call's own asOf = %v, want %v (the ORIGINAL fetch instant) -- got the hit's own later `now` (%v) instead", secondAsOf, firstAsOf, secondNow)
	}
}

// TestSCMCache_ListOpenPRsForUser_SlowFetchDoesNotBornExpire proves a
// cache entry's own expiresAt is anchored on fetch COMPLETION, never the
// PRE-fetch `now` the caller supplied: expiresAt used to be computed from the
// pre-fetch `now` while the fetch itself could take up to
// GitHubListOpenPRsForUserTimeout (3 minutes, production) against a much
// shorter DecisionInboxSCMCacheTTL (2 minutes, production) -- a slow
// fetch for a heavy user could write an entry get() would immediately
// reject as already expired, so the cache never amortized for exactly
// the users it exists to help. This test reproduces the same SHAPE of
// bug at test-friendly durations: a fetch (200ms) that deliberately
// outlasts the TTL (50ms).
func TestSCMCache_ListOpenPRsForUser_SlowFetchDoesNotBornExpire(t *testing.T) {
	t.Parallel()

	const ttl = 50 * time.Millisecond
	const fetchDelay = 200 * time.Millisecond

	fake := &fakeSCMCacheSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{"1": {{Number: 1}}},
		openPRsDelay:        fetchDelay,
	}
	timeouts := platform.DefaultTimeouts()
	timeouts.DecisionInboxSCMCacheTTL = ttl
	timeouts.GitHubListOpenPRsForUserTimeout = 10 * time.Second // plenty of headroom over fetchDelay
	cache := decisioninbox.NewSCMCache(fake, timeouts)

	preFetchNow := time.Now()
	_, _, _, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, preFetchNow)
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}
	if got := fake.callCount("1"); got != 1 {
		t.Fatalf("fetch called %d times after first call, want 1", got)
	}

	// A realistic, immediate follow-up request: `now` captured fresh,
	// right after the first call returns.
	secondNow := time.Now()
	if !secondNow.After(preFetchNow.Add(ttl)) {
		t.Fatalf("test setup invariant violated: the fetch (%s) must outlast the TTL (%s) for this test to actually exercise the bug -- preFetchNow=%v secondNow=%v", fetchDelay, ttl, preFetchNow, secondNow)
	}

	_, _, _, err = cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, secondNow)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if got := fake.callCount("1"); got != 1 {
		t.Errorf("fetch called %d times after second call, want 1 (still a cache hit -- expiresAt must be anchored on fetch COMPLETION, not the pre-fetch `now`)", got)
	}
}

// TestSCMCache_ListOpenPRsForUser_TruncatedResultIsNeitherHiddenNorCached
// proves TWO facts that were previously
// untested (this method's own fake used to hardcode truncated=false and
// never error): (1) SCMCache.ListOpenPRsForUser's own truncated return
// actually surfaces the underlying port's truncated=true (rather than
// silently dropping it, which the type signature alone does not prove --
// a mutation returning a hardcoded `false` here would still compile); and
// (2) a truncated result is NEVER cached -- a second call moments later,
// still well within the TTL, must re-fetch live rather than serving a
// cache hit, since caching a known-partial read would silently present it
// as confirmed-complete for the rest of the TTL window.
func TestSCMCache_ListOpenPRsForUser_TruncatedResultIsNeitherHiddenNorCached(t *testing.T) {
	t.Parallel()

	fake := &fakeSCMCacheSourceControl{
		openPRsByExternalID: map[string][]ports.OpenPR{"1": {{Number: 1}}},
		openPRsTruncated:    true,
	}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())
	now := time.Now()

	prs, _, truncated, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, now)
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true (the underlying SourceControl call reported truncated=true)")
	}
	if len(prs) != 1 {
		t.Errorf("prs = %+v, want the best-effort partial result still returned, never blanked out", prs)
	}

	// A second call moments later, still well inside the TTL, must NOT be
	// a cache hit.
	_, _, _, err = cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, now)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if got := fake.callCount("1"); got != 2 {
		t.Errorf("fetch called %d times, want 2 (a truncated result must never be cached, so the second call must re-fetch live rather than hit)", got)
	}
}

// TestSCMCache_ListOpenPRsForUser_PropagatesUnderlyingError adds error
// injection alongside the truncated-result coverage above -- the fake
// never errored before this test.
func TestSCMCache_ListOpenPRsForUser_PropagatesUnderlyingError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom: github is down")
	fake := &fakeSCMCacheSourceControl{openPRsErr: wantErr}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())

	_, _, _, err := cache.ListOpenPRsForUser(context.Background(), ports.ListOpenPRsForUserSpec{GitHubExternalID: "1"}, time.Now())
	if err == nil {
		t.Fatal("ListOpenPRsForUser() error = nil, want the underlying SourceControl error wrapped")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("ListOpenPRsForUser() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestSCMCache_ResolveCodeOwners_SlowFetchDoesNotBornExpire is the P2-2
// regression test: the "born-expired entries"
// fix (anchoring expiresAt/asOf on fetch COMPLETION, not the caller's
// pre-fetch `now`) was previously applied ONLY to the openPRs cache --
// ResolveCodeOwners still anchored both on the pre-fetch `now`, stale by
// the whole duration of whichever call races against it inside the SAME
// Build (typically the openPRs fetch that precedes it). Mirrors
// TestSCMCache_ListOpenPRsForUser_SlowFetchDoesNotBornExpire above exactly,
// at the same test-friendly durations, for ResolveCodeOwners instead.
func TestSCMCache_ResolveCodeOwners_SlowFetchDoesNotBornExpire(t *testing.T) {
	t.Parallel()

	const ttl = 50 * time.Millisecond
	const fetchDelay = 200 * time.Millisecond

	fake := &fakeSCMCacheSourceControl{codeOwnersDelay: fetchDelay}
	timeouts := platform.DefaultTimeouts()
	timeouts.DecisionInboxSCMCacheTTL = ttl
	timeouts.GitHubResolveCodeOwnersTimeout = 10 * time.Second // plenty of headroom over fetchDelay
	cache := decisioninbox.NewSCMCache(fake, timeouts)

	spec := ports.ResolveCodeOwnersSpec{Owner: "acme", Repo: "widgets", Ref: "main", Paths: []string{"x.go"}}

	preFetchNow := time.Now()
	_, _, err := cache.ResolveCodeOwners(context.Background(), spec, preFetchNow)
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}
	if got := fake.codeOwnersCalls(); got != 1 {
		t.Fatalf("fetch called %d times after first call, want 1", got)
	}

	// A realistic, immediate follow-up request: `now` captured fresh,
	// right after the first call returns.
	secondNow := time.Now()
	if !secondNow.After(preFetchNow.Add(ttl)) {
		t.Fatalf("test setup invariant violated: the fetch (%s) must outlast the TTL (%s) for this test to actually exercise the bug -- preFetchNow=%v secondNow=%v", fetchDelay, ttl, preFetchNow, secondNow)
	}

	_, _, err = cache.ResolveCodeOwners(context.Background(), spec, secondNow)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if got := fake.codeOwnersCalls(); got != 1 {
		t.Errorf("fetch called %d times after second call, want 1 (still a cache hit -- expiresAt must be anchored on fetch COMPLETION, not the pre-fetch `now`)", got)
	}
}

// TestSCMCache_ResolveBranchSHA_CacheHitWithinTTL is D2's own regression
// test (second adversarial-review round): a second call for the SAME
// (owner, repo, branch) within DecisionInboxSCMCacheTTL must be served
// from cache -- exactly one live fetch, mirroring TestSCMCache_
// ListOpenPRsForUser_CacheHitReturnsOriginalFetchInstant's own identical
// shape one method further.
func TestSCMCache_ResolveBranchSHA_CacheHitWithinTTL(t *testing.T) {
	t.Parallel()

	fake := &fakeSCMCacheSourceControl{resolveBranchSHA: "sha-abc123"}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())
	spec := ports.ResolveBranchSHASpec{Owner: "acme", Repo: "widgets", Branch: "main", Token: "tok"}

	now := time.Now()
	sha1, _, err := cache.ResolveBranchSHA(context.Background(), spec, now)
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}
	if sha1 != "sha-abc123" {
		t.Errorf("first call sha = %q, want %q", sha1, "sha-abc123")
	}
	if got := fake.resolveBranchSHACalls(); got != 1 {
		t.Fatalf("fetch called %d times after first call, want 1", got)
	}

	sha2, _, err := cache.ResolveBranchSHA(context.Background(), spec, now.Add(time.Second))
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if sha2 != "sha-abc123" {
		t.Errorf("second call sha = %q, want %q (cached)", sha2, "sha-abc123")
	}
	if got := fake.resolveBranchSHACalls(); got != 1 {
		t.Errorf("fetch called %d times after second call, want 1 (still a cache hit)", got)
	}
}

// TestSCMCache_ResolveBranchSHA_ExpiredEntryRefetches proves the OTHER
// half: a call past DecisionInboxSCMCacheTTL genuinely re-fetches, rather
// than serving a stale, expired entry forever.
func TestSCMCache_ResolveBranchSHA_ExpiredEntryRefetches(t *testing.T) {
	t.Parallel()

	fake := &fakeSCMCacheSourceControl{resolveBranchSHA: "sha-abc123"}
	timeouts := platform.DefaultTimeouts()
	timeouts.DecisionInboxSCMCacheTTL = 50 * time.Millisecond
	cache := decisioninbox.NewSCMCache(fake, timeouts)
	spec := ports.ResolveBranchSHASpec{Owner: "acme", Repo: "widgets", Branch: "main", Token: "tok"}

	// A fixed, injected clock -- deliberately NEVER time.Now() -- so this
	// test is provable against the cache's own logical clock alone (E9,
	// third adversarial-review round). This previously seeded `now` from
	// a REAL time.Now() and re-checked at now+TTL+1ms: a 1ms margin
	// against a SEPARATE, real time.Now() call inside ResolveBranchSHA
	// itself (scmcache.go's own fetchedAt, at the time) that any
	// scheduling delay between capturing `now` here and that internal
	// call actually running could exceed -- silently keeping the
	// "expired" entry alive and failing this test on a loaded machine.
	// Now that ResolveBranchSHA stores against the SAME `now` it is
	// handed, never a second, independent clock read, expiry is a pure
	// function of the two `now` values THIS TEST controls -- no
	// wall-clock proximity involved anywhere.
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if _, _, err := cache.ResolveBranchSHA(context.Background(), spec, now); err != nil {
		t.Fatalf("first call error = %v", err)
	}

	fake.mu.Lock()
	fake.resolveBranchSHA = "sha-def456"
	fake.mu.Unlock()

	// Exactly at the TTL boundary, no margin past it needed:
	// ttlCache.get's own `!now.Before(entry.expiresAt)` check treats
	// now == expiresAt as already expired.
	now2 := now.Add(timeouts.DecisionInboxSCMCacheTTL)
	sha2, asOf2, err := cache.ResolveBranchSHA(context.Background(), spec, now2)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if sha2 != "sha-def456" {
		t.Errorf("second call (past TTL) sha = %q, want %q (a fresh live fetch, not the expired cached value)", sha2, "sha-def456")
	}
	// E9's own decisive assertion: a fresh fetch's own asOf must be
	// EXACTLY the injected clock this call was handed. Reverting
	// scmcache.go's own fetchedAt to a real time.Now() call fails this
	// deterministically (a real wall-clock reading almost never equals
	// this test's own fixed, arbitrary 2026-09-16 instant), never
	// flakily -- the exact property a mutation of that fix must be
	// caught by.
	if !asOf2.Equal(now2) {
		t.Errorf("second call asOf = %v, want exactly %v (E9: a fresh fetch's own fetchedAt must be the injected clock this call was handed, never a second, real time.Now() read)", asOf2, now2)
	}
	if got := fake.resolveBranchSHACalls(); got != 2 {
		t.Errorf("fetch called %d times, want 2 (the second call must genuinely re-fetch)", got)
	}
}

// TestSCMCache_ResolveBranchSHA_PropagatesUnderlyingError mirrors
// TestSCMCache_ListOpenPRsForUser_PropagatesUnderlyingError one method
// further.
func TestSCMCache_ResolveBranchSHA_PropagatesUnderlyingError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom: github is down")
	fake := &fakeSCMCacheSourceControl{resolveBranchSHAErr: wantErr}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())

	_, _, err := cache.ResolveBranchSHA(context.Background(), ports.ResolveBranchSHASpec{Owner: "acme", Repo: "widgets", Branch: "main"}, time.Now())
	if err == nil {
		t.Fatal("ResolveBranchSHA() error = nil, want the underlying SourceControl error wrapped")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("ResolveBranchSHA() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestSCMCache_IsAncestor_CacheHitWithinTTL is D3's own regression test
// (second adversarial-review round): a second call for the SAME (owner,
// repo, ancestor, descendant) tuple within DecisionInboxSCMCacheTTL must
// be served from cache -- exactly one live call.
func TestSCMCache_IsAncestor_CacheHitWithinTTL(t *testing.T) {
	t.Parallel()

	fake := &fakeSCMCacheSourceControl{isAncestorResult: true}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())
	spec := ports.IsAncestorSpec{Owner: "acme", Repo: "widgets", Ancestor: "sha-old", Descendant: "sha-new", Token: "tok"}

	now := time.Now()
	got1, err := cache.IsAncestor(context.Background(), spec, now)
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}
	if !got1 {
		t.Error("first call = false, want true")
	}
	if got := fake.isAncestorCalls(); got != 1 {
		t.Fatalf("fetch called %d times after first call, want 1", got)
	}

	got2, err := cache.IsAncestor(context.Background(), spec, now.Add(time.Second))
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if !got2 {
		t.Error("second call = false, want true (cached)")
	}
	if got := fake.isAncestorCalls(); got != 1 {
		t.Errorf("fetch called %d times after second call, want 1 (still a cache hit)", got)
	}
}

// TestSCMCache_IsAncestor_PropagatesUnderlyingError mirrors
// TestSCMCache_ResolveBranchSHA_PropagatesUnderlyingError one method
// further.
func TestSCMCache_IsAncestor_PropagatesUnderlyingError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom: github is down")
	fake := &fakeSCMCacheSourceControl{isAncestorErr: wantErr}
	cache := decisioninbox.NewSCMCache(fake, platform.DefaultTimeouts())

	_, err := cache.IsAncestor(context.Background(), ports.IsAncestorSpec{Owner: "acme", Repo: "widgets", Ancestor: "a", Descendant: "b"}, time.Now())
	if err == nil {
		t.Fatal("IsAncestor() error = nil, want the underlying SourceControl error wrapped")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("IsAncestor() error = %v, want it to wrap %v", err, wantErr)
	}
}

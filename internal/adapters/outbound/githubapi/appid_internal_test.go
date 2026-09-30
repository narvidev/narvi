package githubapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
)

// TestResolveAppID_KeepsAnAnswerForItsTTL pins the App-id cache's bound
// (§21.2): a slug can pass to another App once freed, so an answer is kept
// only until its TTL runs out, then read again -- a moved slug is caught
// within the TTL, never kept for the process's lifetime. With no TTL,
// nothing is kept.
func TestResolveAppID_KeepsAnAnswerForItsTTL(t *testing.T) {
	t.Parallel()

	const ttl = 10 * time.Minute
	tests := []struct {
		name string
		ttl  time.Duration
		// elapsed is how long after the first call the second one is made.
		elapsed      time.Duration
		wantSecondID int64
		wantRequests int
	}{
		{name: "within the TTL the kept id is answered", ttl: ttl, elapsed: ttl - time.Second, wantSecondID: 111, wantRequests: 1},
		{name: "at the TTL the slug is read again, and a moved slug gets its new App", ttl: ttl, elapsed: ttl, wantSecondID: 222, wantRequests: 2},
		{name: "past the TTL the slug is read again", ttl: ttl, elapsed: ttl + time.Hour, wantSecondID: 222, wantRequests: 2},
		{name: "with no TTL nothing is kept", ttl: 0, elapsed: time.Second, wantSecondID: 222, wantRequests: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				requests++
				// The slug's owner changes after the first read.
				id := 111
				if requests > 1 {
					id = 222
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(id) + `}`))
			}))
			defer server.Close()

			start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			now := start
			a := New(server.Client(), server.URL).WithAppIDCacheTTL(tc.ttl)
			a.now = func() time.Time { return now }

			if id, err := a.ResolveAppID(context.Background(), ports.ResolveAppIDSpec{Slug: "foo", Token: "t1"}); err != nil || id != 111 {
				t.Fatalf("first ResolveAppID() = (%d, %v), want (111, nil)", id, err)
			}
			now = start.Add(tc.elapsed)
			if id, err := a.ResolveAppID(context.Background(), ports.ResolveAppIDSpec{Slug: "foo", Token: "t2"}); err != nil || id != tc.wantSecondID {
				t.Errorf("second ResolveAppID() = (%d, %v), want (%d, nil)", id, err, tc.wantSecondID)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != tc.wantRequests {
				t.Errorf("requests = %d, want %d", requests, tc.wantRequests)
			}
		})
	}
}

// TestKeepAppID_Bound pins the cache's size bound: when it is full, expired
// entries are dropped to make room, and a new answer is not kept while
// every entry is still live.
func TestKeepAppID_Bound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// liveEntries is how many of the maxCachedAppIDs entries have not
		// expired yet.
		liveEntries int
		wantKept    bool
	}{
		{name: "a full cache of expired entries makes room", liveEntries: 0, wantKept: true},
		{name: "a full cache with one expired entry makes room", liveEntries: maxCachedAppIDs - 1, wantKept: true},
		{name: "a full cache of live entries keeps no new answer", liveEntries: maxCachedAppIDs, wantKept: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			a := New(nil, "").WithAppIDCacheTTL(time.Minute)
			a.now = func() time.Time { return now }
			a.appIDs = map[string]appIDEntry{}
			for i := 0; i < maxCachedAppIDs; i++ {
				expires := now.Add(-time.Second)
				if i < tc.liveEntries {
					expires = now.Add(time.Second)
				}
				a.appIDs["slug-"+strconv.Itoa(i)] = appIDEntry{id: int64(i + 1), expires: expires}
			}

			a.keepAppID("new-slug", 999)
			id, kept := a.cachedAppID("new-slug")
			if kept != tc.wantKept || (kept && id != 999) {
				t.Errorf("cachedAppID(new-slug) = (%d, %v), want kept %v", id, kept, tc.wantKept)
			}
			if len(a.appIDs) > maxCachedAppIDs {
				t.Errorf("cache holds %d entries, want at most %d", len(a.appIDs), maxCachedAppIDs)
			}
		})
	}
}

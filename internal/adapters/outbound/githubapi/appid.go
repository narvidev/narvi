// This file (appid.go) implements ports.SourceControl.ResolveAppID: the id
// of the App behind a slug, for a commit status an App's bot account
// posted (§21.2, a required check tied to an App). The per-ref statuses
// listing names a status's poster by login, "<slug>[bot]", never by App
// id; a check run of the same App at the head carries the id, and
// fetchCIConclusionLive takes it from there when one does. When none does,
// the id is read here, from GET /apps/{app_slug} -- a public read.

package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/narvidev/narvi/internal/app/ports"
)

// maxCachedAppIDs bounds how many slugs ResolveAppID keeps at once. Past
// this many live entries, a new answer is still returned, only not kept.
const maxCachedAppIDs = 1024

// appResponse is the subset of GitHub's GET /apps/{app_slug} response
// ResolveAppID reads.
type appResponse struct {
	ID int64 `json:"id"`
}

// appIDEntry is one kept answer: the id, and when it stops being used.
type appIDEntry struct {
	id      int64
	expires time.Time
}

// WithAppIDCacheTTL makes ResolveAppID keep each successful answer for
// ttl (platform.Timeouts.GitHubAppIDCacheTTL) and returns a. An App keeps
// its id, but a slug can pass to another App once freed -- a rename or a
// deletion frees it -- so an answer is kept for a bounded time, never for
// the adapter's lifetime: a slug that moved is read again within ttl, and
// until then its statuses are still attributed to the App that held it.
// Zero, the default, keeps nothing. Call it once, at wiring, before the
// adapter is shared.
func (a *Adapter) WithAppIDCacheTTL(ttl time.Duration) *Adapter {
	a.appIDTTL = ttl
	return a
}

// AppIDCacheTTL is the TTL WithAppIDCacheTTL set: how long a successful
// ResolveAppID answer is kept.
func (a *Adapter) AppIDCacheTTL() time.Duration {
	return a.appIDTTL
}

// ResolveAppID implements ports.SourceControl -- see this file's doc
// comment. A successful answer is kept for the TTL WithAppIDCacheTTL set;
// a failure is never kept, so the next call reads again. A slug that names
// no App (404) is ports.ErrAppNotFound; every other failure, and an answer
// with no positive id, is a failed read.
func (a *Adapter) ResolveAppID(ctx context.Context, spec ports.ResolveAppIDSpec) (int64, error) {
	if spec.Slug == "" {
		return 0, errors.New("githubapi: resolve app id: empty slug")
	}
	if id, ok := a.cachedAppID(spec.Slug); ok {
		return id, nil
	}

	path := fmt.Sprintf("%s/apps/%s", a.apiBaseURL, url.PathEscape(spec.Slug))
	body, err := a.doGet(ctx, path, spec.Token)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return 0, fmt.Errorf("githubapi: resolve app id of %q: %w", spec.Slug, ports.ErrAppNotFound)
		}
		return 0, fmt.Errorf("githubapi: resolve app id of %q: %w", spec.Slug, err)
	}
	var app appResponse
	if err := json.Unmarshal(body, &app); err != nil {
		return 0, fmt.Errorf("githubapi: resolve app id of %q: decode app: %w", spec.Slug, err)
	}
	if app.ID <= 0 {
		return 0, fmt.Errorf("githubapi: resolve app id of %q: the app reported no id", spec.Slug)
	}
	a.keepAppID(spec.Slug, app.ID)
	return app.ID, nil
}

// cachedAppID is slug's kept answer, while it has not expired.
func (a *Adapter) cachedAppID(slug string) (int64, bool) {
	a.appIDsMu.Lock()
	defer a.appIDsMu.Unlock()
	entry, ok := a.appIDs[slug]
	if !ok || !a.clock().Before(entry.expires) {
		return 0, false
	}
	return entry.id, true
}

// keepAppID keeps id for slug for the TTL, dropping expired entries first
// when the cache is full.
func (a *Adapter) keepAppID(slug string, id int64) {
	if a.appIDTTL <= 0 {
		return
	}
	a.appIDsMu.Lock()
	defer a.appIDsMu.Unlock()
	now := a.clock()
	if a.appIDs == nil {
		a.appIDs = map[string]appIDEntry{}
	}
	if _, ok := a.appIDs[slug]; !ok && len(a.appIDs) >= maxCachedAppIDs {
		for s, e := range a.appIDs {
			if !now.Before(e.expires) {
				delete(a.appIDs, s)
			}
		}
		if len(a.appIDs) >= maxCachedAppIDs {
			return
		}
	}
	a.appIDs[slug] = appIDEntry{id: id, expires: now.Add(a.appIDTTL)}
}

// clock is the adapter's time source for the App-id cache: time.Now,
// unless a test set one.
func (a *Adapter) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

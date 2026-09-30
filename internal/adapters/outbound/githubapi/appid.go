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
	"net/url"

	"github.com/narvidev/narvi/internal/app/ports"
)

// maxCachedAppIDs bounds how many slugs ResolveAppID keeps. An App's id
// never changes, so an answer is kept for the adapter's lifetime; past
// this many distinct slugs, a new answer is still returned, only not
// kept.
const maxCachedAppIDs = 1024

// appResponse is the subset of GitHub's GET /apps/{app_slug} response
// ResolveAppID reads.
type appResponse struct {
	ID int64 `json:"id"`
}

// ResolveAppID implements ports.SourceControl -- see this file's doc
// comment. A successful answer is kept for the adapter's lifetime (an
// App's id never changes); a failure is never kept, so the next call reads
// again. A slug that names no App (404), any other failure, and an answer
// with no positive id are errors.
func (a *Adapter) ResolveAppID(ctx context.Context, spec ports.ResolveAppIDSpec) (int64, error) {
	if spec.Slug == "" {
		return 0, errors.New("githubapi: resolve app id: empty slug")
	}
	a.appIDsMu.Lock()
	id, ok := a.appIDs[spec.Slug]
	a.appIDsMu.Unlock()
	if ok {
		return id, nil
	}

	path := fmt.Sprintf("%s/apps/%s", a.apiBaseURL, url.PathEscape(spec.Slug))
	body, err := a.doGet(ctx, path, spec.Token)
	if err != nil {
		return 0, fmt.Errorf("githubapi: resolve app id of %q: %w", spec.Slug, err)
	}
	var app appResponse
	if err := json.Unmarshal(body, &app); err != nil {
		return 0, fmt.Errorf("githubapi: resolve app id of %q: decode app: %w", spec.Slug, err)
	}
	if app.ID <= 0 {
		return 0, fmt.Errorf("githubapi: resolve app id of %q: the app reported no id", spec.Slug)
	}

	a.appIDsMu.Lock()
	if a.appIDs == nil {
		a.appIDs = map[string]int64{}
	}
	if len(a.appIDs) < maxCachedAppIDs {
		a.appIDs[spec.Slug] = app.ID
	}
	a.appIDsMu.Unlock()
	return app.ID, nil
}

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// twin is one REST route this package's tools invoke in-process --
// "twin" because it is the SAME http.HandlerFunc httpapi's own router
// registers for the browser-facing route of the same name, never a
// second implementation (doc.go's own "one authorization path" section).
type twin struct {
	// method is the twin's own HTTP method ("GET" for every 180 tool;
	// later tools may need "POST").
	method string
	// pathTemplate is the twin's own chi route pattern, e.g.
	// "/api/sessions/{sessionID}" -- also what
	// TestEveryToolHasARegisteredTwin checks against routes.golden, so
	// this must read EXACTLY like that golden's own path column.
	pathTemplate string
	// handler is the real, already-constructed httpapi handler --
	// httpapi.GetModelCatalog(), httpapi.ListSessions(sessionStore), etc.
	handler http.HandlerFunc
}

// callTwin invokes t.handler in-process, through an httptest recorder,
// with ctx (the MCP request's own context AFTER auth.Middleware ran --
// see handler.go's getServer) carrying the authenticated
// platform.AuthenticatedUser forward exactly as if this were a real
// /api/** request. urlParams populates chi's own URLParams the same way
// the real router would for a path like "/api/sessions/{sessionID}"
// (chi.URLParam(r, "sessionID") inside the handler reads this, not the
// literal request path); query becomes the request's own ?query=string.
//
// This is the ENTIRE adapter's contact with the application: the same
// authz.Authorize call, the same DTO encoder, the same log lines run
// either way, because this is the identical function, not a second
// implementation of it (doc.go's own "one authorization path" section;
// tools/lint/narvichecks' mcpimportban analyzer makes the alternative --
// reaching a store or authz.Authorize directly from this package --
// impossible to compile).
func callTwin(ctx context.Context, t twin, urlParams map[string]string, query url.Values) (status int, body []byte) {
	path := t.pathTemplate
	for name, value := range urlParams {
		path = replaceURLParam(path, name, value)
	}

	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req := httptest.NewRequest(t.method, target, nil)

	rctx := chi.NewRouteContext()
	for name, value := range urlParams {
		rctx.URLParams.Add(name, value)
	}
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// replaceURLParam substitutes "{name}" with value in template -- just
// enough templating for this package's own chi route patterns; the REAL
// param resolution inside the invoked handler always comes from
// chi.URLParam via the RouteContext callTwin sets up above, never from
// this substituted path string itself (which exists only so the
// constructed *http.Request has a syntactically sensible URL to report
// in logs/traces). net/url.PathEscape is deliberately NOT applied to
// value here: chi's own URLParams -- the actual source of truth every
// handler reads -- already carries the raw, unescaped value.
func replaceURLParam(template, name, value string) string {
	return strings.ReplaceAll(template, "{"+name+"}", value)
}

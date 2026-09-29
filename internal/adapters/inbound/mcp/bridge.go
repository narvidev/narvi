package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/narvidev/narvi/internal/platform"
)

// twin is one REST route this package's tools invoke in-process --
// "twin" because it is the SAME http.HandlerFunc httpapi's own router
// registers for the browser-facing route of the same name, never a
// second implementation (doc.go's own "one authorization path" section).
type twin struct {
	// method is the twin's own HTTP method: GET for every read tool, POST
	// for narvi_create_session. TestWriteTwinsRequireWriteScope ties it to
	// the tool's scope.
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

// twinCall is what one tool call hands its twin (technical plan §43.7):
// the chi URL params, the query string, and the request body. It is all a
// tool's BuildRequest can decide -- never the method or the path, which are
// the twin's own, and never a header.
type twinCall struct {
	// URLParams populates chi's own route context, the way the real router
	// would for a path like "/api/sessions/{sessionID}".
	URLParams map[string]string
	// Query becomes the request's own ?query=string.
	Query url.Values
	// Body is the request DTO the twin decodes -- a contracts/gen/go/
	// restdtos request value, built by the tool's BuildRequest from its
	// validated arguments -- or nil for no body. callTwin marshals it
	// itself, so only that DTO's own fields can ever reach the twin:
	// never the raw arguments the client sent, which the tool's input
	// schema governs, not the twin's.
	Body any
}

// callTwin invokes t.handler in-process, through an httptest recorder,
// with ctx (the MCP request's own context AFTER auth.RequireMCPBearer ran --
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
//
// The synthesized *http.Request is built directly (a literal
// &http.Request{...}), NEVER by parsing a request line from
// attacker-controlled text. A prior version of this function called
// httptest.NewRequest(t.method, target, nil), which runs http.ReadRequest
// over the literal string "<method> <target> HTTP/1.0\r\n\r\n" and PANICS
// -- rather than returning an error -- when that text does not parse as
// a request line. Any tool argument containing a space, an invalid
// %-escape, a tab, or a bare CR/LF reached that parser through urlParams
// and killed the whole process: the MCP SDK runs every tool handler in
// its own goroutine with no recover anywhere in its call stack, so
// neither net/http's per-connection recovery nor chi's own
// middleware.Recoverer could ever catch it (toolHandler's own recover,
// tools.go, is the second, independent layer of defense against exactly
// that -- this function's job is to make sure there is no request line
// left for attacker data to corrupt in the first place). Building the
// *http.Request directly instead means Header is always a freshly
// allocated, empty http.Header: no header or cookie derived from a tool
// argument can ever reach the synthesized request -- and neither can the
// bearer token that authenticated the MCP request itself. The MCP
// authorization spec forbids passing that token through (technical plan
// §43.16); the twin authorizes from the context principal, never from a
// credential, and TestBridge_NoAuthorizationHeaderReachesTwin pins that
// the request it receives carries no header at all.
//
// A body (call.Body) is the JSON encoding of that DTO, with its length set
// and still no header -- not even Content-Type: no twin reads one (the
// create handler decodes its body without checking it), so there is nothing
// to add and the no-header rule stays whole
// (TestBridge_PostTwinGetsTheDTOBodyAndNoHeader). A GET twin handed a body
// is this package's own defect -- a read tool's BuildRequest built one --
// and is refused with a 500, which mapOutcome answers -32603, without
// invoking the twin (TestBridge_GetTwinWithBodyIsADefect); so is a body
// that cannot be encoded.
func callTwin(ctx context.Context, t twin, call twinCall) (status int, body []byte) {
	path := t.pathTemplate
	for name, value := range call.URLParams {
		path = replaceURLParam(path, name, value)
	}

	u := &url.URL{Path: path}
	if len(call.Query) > 0 {
		u.RawQuery = call.Query.Encode()
	}

	req := &http.Request{
		Method:     t.method,
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Body:       http.NoBody,
	}

	if call.Body != nil {
		if t.method == http.MethodGet {
			platform.Logger(ctx).Error("mcp: a GET twin was handed a request body (a read tool's BuildRequest built one)",
				"path", t.pathTemplate)
			return http.StatusInternalServerError, nil
		}
		encoded, err := json.Marshal(call.Body)
		if err != nil {
			platform.Logger(ctx).Error("mcp: encode twin request body failed", "path", t.pathTemplate, "error", err)
			return http.StatusInternalServerError, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(encoded))
		req.ContentLength = int64(len(encoded))
	}

	rctx := chi.NewRouteContext()
	for name, value := range call.URLParams {
		// The RAW, unescaped value -- chi.URLParam is what every real
		// handler actually reads (e.g. httpapi's own parseSessionID),
		// never the literal path string above (that string exists
		// only so the constructed *http.Request carries a
		// syntactically sensible URL to report in logs/traces).
		rctx.URLParams.Add(name, value)
	}
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// replaceURLParam substitutes "{name}" with url.PathEscape(value) in
// template -- just enough templating for this package's own chi route
// patterns; the REAL param resolution inside the invoked handler always
// comes from chi.URLParam via the RouteContext callTwin sets up above
// (populated with the RAW, unescaped value), never from this substituted
// path string itself. PathEscape IS applied here -- a prior version of
// this function skipped it, reasoning that chi's own URLParams was the
// only source of truth any handler reads; that is still true, but this
// string is also used to build a real *url.URL{Path: ...} value (see
// callTwin above), and an unescaped raw argument can make that URL's own
// Path/RawQuery/String() ill-formed for any downstream code (logging,
// tracing, metrics) that relies on it.
func replaceURLParam(template, name, value string) string {
	return strings.ReplaceAll(template, "{"+name+"}", url.PathEscape(value))
}

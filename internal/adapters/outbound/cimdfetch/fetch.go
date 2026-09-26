package cimdfetch

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/narvidev/narvi/internal/platform"
)

// MaxDocumentBytes bounds a metadata document: one byte more is read, and
// a document that long is refused rather than truncated.
const MaxDocumentBytes = 64 << 10

// ErrUnexpectedStatus is wrapped when the document host (after any
// redirects) answers anything but 200.
var ErrUnexpectedStatus = errors.New("cimdfetch: the document host did not answer 200")

// ErrNotJSON is wrapped when the document's Content-Type is not
// application/json.
var ErrNotJSON = errors.New("cimdfetch: the document is not application/json")

// ErrTooLarge is wrapped when the document is longer than
// MaxDocumentBytes.
var ErrTooLarge = errors.New("cimdfetch: the document is too large")

// ErrUnframedBody is wrapped when the document's response does not say
// where its body ends -- neither a Content-Length nor the chunked transfer
// coding -- so that only the connection closing would end it: such a body,
// cut short, reads exactly like a whole one (TLS itself takes a bare close
// at a record boundary for a clean end), so none is ever used.
var ErrUnframedBody = errors.New("cimdfetch: the document's response does not say where it ends")

// errNoClient is what a Fetcher built with no GuardedClient answers.
var errNoClient = errors.New("cimdfetch: this fetcher was built with no guarded client, so it can make no requests")

// Result is one fetched document.
type Result struct {
	// Body is the document, at most MaxDocumentBytes, not yet parsed.
	Body []byte
	// MaxAge is the lifetime the response's own Cache-Control gave the
	// document -- zero for no-store or no-cache -- when HasMaxAge is true.
	// The caller may only let it SHORTEN how long it trusts the document.
	MaxAge    time.Duration
	HasMaxAge bool
}

// Fetcher fetches client ID metadata documents through a GuardedClient.
type Fetcher struct {
	client  *http.Client
	timeout time.Duration
}

// New builds a Fetcher that makes every request through client and
// bounds each whole Fetch -- redirects, handshakes, body -- by timeout
// (platform.Timeouts.MCPClientMetadataFetchTimeout in production). A nil
// client yields a Fetcher that refuses every fetch: the zero value fails
// closed, the rule technical plan §30.2 set for every outbound
// constructor.
func New(client *GuardedClient, timeout time.Duration) *Fetcher {
	f := &Fetcher{client: &http.Client{Transport: refusingTransport{}}, timeout: timeout}
	if client != nil {
		f.client = client.http
	}
	return f
}

// Fetch GETs the metadata document at rawURL and returns it unparsed,
// with the lifetime its response gave it. Every refusal wraps one of this
// package's errors (or the context's, on timeout).
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Result, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Result{}, fmt.Errorf("cimdfetch: parse the document URL: %w", err)
	}
	if err := requireHTTPS(u); err != nil {
		return Result{}, err
	}
	if err := hostAsWritten(rawURL, u); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("cimdfetch: build the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Asked for here, not by the transport, so the transport hands the body
	// over as it was framed -- one it decompressed itself would no longer
	// say whether a Content-Length ended it -- and Fetch decodes it below.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := f.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("cimdfetch: fetch the document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%w: status %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Result{}, fmt.Errorf("%w: Content-Type %q", ErrNotJSON, resp.Header.Get("Content-Type"))
	}
	if !framed(resp) {
		return Result{}, fmt.Errorf("%w: neither a Content-Length nor chunked", ErrUnframedBody)
	}
	var doc io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		// Compressed whether or not it was asked for, as a hostile host
		// may: the cap below is on the decoded document.
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Result{}, fmt.Errorf("cimdfetch: read the document: %w", err)
		}
		doc = zr
	}
	body, err := io.ReadAll(io.LimitReader(doc, MaxDocumentBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("cimdfetch: read the document: %w", err)
	}
	// The read must have ENDED within the timeout, not merely stopped.
	// When the timeout fires the transport closes the connection, and a
	// TLS close begins with a close_notify the server sees as the client
	// leaving: a server that answers it by ending the body cleanly -- a
	// handler returning when its request's context is cancelled -- hands
	// the transport a clean end after the deadline, which it reports as
	// one (only a read ERROR is turned into the context's). That body was
	// cut short by the fetch's own giving up, and is never a document.
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("cimdfetch: the document was not read within the fetch's timeout: %w", err)
	}
	if len(body) > MaxDocumentBytes {
		return Result{}, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxDocumentBytes)
	}
	maxAge, hasMaxAge := cacheMaxAge(resp.Header)
	return Result{Body: body, MaxAge: maxAge, HasMaxAge: hasMaxAge}, nil
}

// framed reports whether resp says where its body ends -- a Content-Length
// or the chunked transfer coding -- so that a body cut short ends in an
// error, never in what reads as a clean end. The guarded client speaks
// HTTP/1.1 only; a response framed any other way fails closed.
func framed(resp *http.Response) bool {
	if resp.ContentLength >= 0 {
		return true
	}
	te := resp.TransferEncoding
	return len(te) > 0 && te[len(te)-1] == "chunked"
}

// maxAgeSeconds caps a max-age directive before it becomes a Duration, so
// no value can overflow; any cap far above the caller's own ceiling is as
// good as another.
const maxAgeSeconds = 1 << 31

// cacheMaxAge reads the lifetime a response's Cache-Control gives it:
// no-store or no-cache is zero (re-fetch next time); otherwise the
// smallest well-formed max-age; otherwise none. Malformed directives are
// ignored, and nothing else -- Expires included -- is read: the caller
// only ever uses this to shorten a fixed ceiling, so ignoring a header can
// never make a document trusted for longer.
func cacheMaxAge(h http.Header) (time.Duration, bool) {
	var (
		maxAge time.Duration
		found  bool
	)
	for _, line := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(line, ",") {
			name, value, hasValue := strings.Cut(strings.TrimSpace(directive), "=")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-store", "no-cache":
				return 0, true
			case "max-age":
				if !hasValue {
					continue
				}
				n, err := strconv.ParseUint(strings.Trim(strings.TrimSpace(value), `"`), 10, 64)
				if err != nil {
					continue
				}
				if n > maxAgeSeconds {
					n = maxAgeSeconds
				}
				if d := platform.SecondsToDuration(int64(n)); !found || d < maxAge {
					maxAge, found = d, true
				}
			}
		}
	}
	return maxAge, found
}

// refusingTransport answers every request with an error naming the cause:
// what a Fetcher built with no GuardedClient uses.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, errNoClient
}

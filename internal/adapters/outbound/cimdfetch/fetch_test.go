package cimdfetch_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
)

// testFetchTimeout bounds every fetch in this file; only the timeout test
// ever waits for it.
const testFetchTimeout = 2 * time.Second

// docHost and friends are the names this file's fake resolver answers.
// Every one is a *.example name: no test here ever resolves a real name
// or reaches the network -- a refused address is refused before its
// socket connects.
const (
	docHost          = "doc.example"
	internalHost     = "internal.example"
	mixedHost        = "mixed.example"
	loopHost         = "loop.example"
	rebindHost       = "rebind.example"
	toPrivateHost    = "to-private.example"
	toMetadataHost   = "to-metadata.example"
	otherHost        = "other.example"
	docHostOtherCase = "DOC.example"
	// dottedIHost has an "i", which a Location may spell U+0130 (capital
	// I with a dot): strings.ToLower folds that to "i", but IDNA -- what
	// net/http resolves and dials -- maps it to "i" and a combining dot,
	// dottedIHostIDNA, another domain altogether.
	dottedIHost     = "info.example"
	dottedIHostIDNA = "xn--info-qwc.example"
	// kelvinHost has a "k", which a Location may spell U+212A (the Kelvin
	// sign): folded to "k" by both strings.ToLower and IDNA.
	kelvinHost = "kilo.example"
)

// fakeResolver answers each host from a fixed sequence of answers (the
// last one repeats) and counts the lookups, so a test can make a name's
// answer change between two dials.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	calls   map[string]int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
}

func (r *fakeResolver) set(host string, answers ...[]netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[host] = answers
}

func (r *fakeResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq, ok := r.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := r.calls[host]
	r.calls[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], nil
}

func addrs(list ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(list))
	for _, a := range list {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

// docServer is an in-test HTTPS server on loopback with its own
// certificate for every *.example name above, counting the requests it
// serves.
type docServer struct {
	*httptest.Server
	addr  netip.AddrPort
	leaf  *x509.Certificate
	roots *x509.CertPool
	hits  atomic.Int32
}

func newDocServer(t *testing.T, h http.Handler) *docServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cimdfetch test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{docHost, internalHost, mixedHost, loopHost, rebindHost, toPrivateHost, toMetadataHost, otherHost, dottedIHost, dottedIHostIDNA, kelvinHost},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	s := &docServer{leaf: leaf, roots: x509.NewCertPool()}
	s.roots.AddCert(leaf)
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		h.ServeHTTP(w, r)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	s.addr = netip.MustParseAddrPort(s.Listener.Addr().String())
	return s
}

// url is https://<host>:<this server's port><path>.
func (s *docServer) url(host, path string) string {
	return fmt.Sprintf("https://%s:%d%s", host, s.addr.Port(), path)
}

// seamed builds a Fetcher through the test seams: the fake resolver, this
// server's certificate, and this server's exact address allowed.
func (s *docServer) seamed(res *fakeResolver) *cimdfetch.Fetcher {
	return cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{
		Resolver:       res,
		AllowAddrPorts: []netip.AddrPort{s.addr},
		RootCAs:        s.roots,
	}), testFetchTimeout)
}

const validDoc = `{"client_id":"x","client_name":"Editor Plugin","redirect_uris":["http://127.0.0.1/callback"]}`

// documentHandler serves validDoc at /client.json and routes the rest
// through extra.
func documentHandler(extra func(w http.ResponseWriter, r *http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if extra != nil && extra(w, r) {
			return
		}
		if r.URL.Path == "/client.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "max-age=600")
			_, _ = w.Write([]byte(validDoc))
			return
		}
		http.NotFound(w, r)
	})
}

// TestCIMDFetch_FetchesThroughTheSeamOnly: an in-test server on loopback
// is fetched only through the test seam that allows its exact address;
// the production guard (no seam but the certificate) refuses the very
// same URL before connecting, and the server never sees a request.
func TestCIMDFetch_FetchesThroughTheSeamOnly(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, documentHandler(nil))
	res := newFakeResolver()
	res.set(docHost, addrs("127.0.0.1"))

	got, err := srv.seamed(res).Fetch(context.Background(), srv.url(docHost, "/client.json"))
	if err != nil {
		t.Fatalf("Fetch through the seam: %v", err)
	}
	if string(got.Body) != validDoc || !got.HasMaxAge || got.MaxAge != 10*time.Minute {
		t.Fatalf("Fetch = body %q max-age (%v, %v), want the document and max-age 10m", got.Body, got.MaxAge, got.HasMaxAge)
	}

	before := srv.hits.Load()
	production := cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{Resolver: res, RootCAs: srv.roots}), testFetchTimeout)
	for _, target := range []string{srv.url(docHost, "/client.json"), srv.url("127.0.0.1", "/client.json")} {
		if _, err := production.Fetch(context.Background(), target); !errors.Is(err, cimdfetch.ErrForbiddenAddress) {
			t.Errorf("production guard, %s: err = %v, want ErrForbiddenAddress", target, err)
		}
	}
	if srv.hits.Load() != before {
		t.Errorf("the production guard reached the loopback server")
	}
}

// TestCIMDFetch_RefusesPrivateTargets is the SSRF threat row's own proof
// (technical plan §43.19): every non-public target is refused at dial
// time -- as a literal, as a name resolving to it, as the target of a
// redirect (which stays on the first URL's own origin, so a redirect
// reaches a private address only through its host's name resolving there
// on the redirect's own dial), and as a name whose answer changes between
// two dials -- and the error says so.
func TestCIMDFetch_RefusesPrivateTargets(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, documentHandler(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/to-self":
			http.Redirect(w, r, "/client.json", http.StatusFound)
		case "/rebind-first":
			http.Redirect(w, r, "/rebind-second", http.StatusFound)
		default:
			return false
		}
		return true
	}))
	res := newFakeResolver()
	res.set(docHost, addrs("127.0.0.1"))
	res.set(internalHost, addrs("10.0.0.7"))
	res.set(mixedHost, addrs("192.168.0.9", "172.16.3.4", "fd00::5"))
	// Each of these answers the test server's own (allowed) address on
	// the first lookup and a refused one on every later one -- what an
	// attacker's DNS does to a check-then-connect fetcher, and the only
	// way a same-origin redirect can lead into a private address.
	res.set(toPrivateHost, addrs("127.0.0.1"), addrs("10.0.0.9"))
	res.set(toMetadataHost, addrs("127.0.0.1"), addrs("169.254.169.254"))
	res.set(rebindHost, addrs("127.0.0.1"), addrs("169.254.169.254"))
	fetch := srv.seamed(res)

	for _, tc := range []struct {
		name, url string
	}{
		{"IPv4 loopback", "https://127.0.0.1/client.json"},
		{"IPv6 loopback", "https://[::1]/client.json"},
		{"10/8", "https://10.1.2.3/client.json"},
		{"172.16/12", "https://172.16.5.4/client.json"},
		{"192.168/16", "https://192.168.1.1/client.json"},
		{"169.254/16", "https://169.254.10.10/client.json"},
		{"the cloud metadata address", "https://169.254.169.254/latest/meta-data/"},
		{"100.64/10 (CGNAT)", "https://100.64.1.1/client.json"},
		{"fc00::/7 (ULA)", "https://[fc00::1]/client.json"},
		{"fd00::/8 (ULA)", "https://[fd12:3456::1]/client.json"},
		{"fe80::/10 (link-local)", "https://[fe80::1]/client.json"},
		{"IPv4-mapped loopback", "https://[::ffff:127.0.0.1]/client.json"},
		{"IPv4-mapped private", "https://[::ffff:10.0.0.1]/client.json"},
		{"NAT64-embedded private", "https://[64:ff9b::a00:1]/client.json"},
		{"unspecified", "https://0.0.0.0/client.json"},
		{"a name resolving to a private address", "https://" + internalHost + "/client.json"},
		{"a name resolving only to private addresses", "https://" + mixedHost + "/client.json"},
		{"a redirect into a private address", srv.url(toPrivateHost, "/to-self")},
		{"a redirect into the cloud metadata address", srv.url(toMetadataHost, "/to-self")},
		{"a DNS answer that changes between checks", srv.url(rebindHost, "/rebind-first")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetch.Fetch(context.Background(), tc.url)
			if !errors.Is(err, cimdfetch.ErrForbiddenAddress) {
				t.Fatalf("Fetch(%s) = %v, want ErrForbiddenAddress", tc.url, err)
			}
		})
	}

	// Each redirect and rebinding case was refused on its SECOND dial,
	// after the first reached the server: each dial resolved and was
	// checked afresh.
	for _, host := range []string{toPrivateHost, toMetadataHost, rebindHost} {
		if n := res.lookups(host); n != 2 {
			t.Errorf("%s was resolved %d times, want 2 (once per dial)", host, n)
		}
	}
}

// TestCIMDFetch_RefusesCrossOriginRedirects: a redirect is followed only
// within the origin -- scheme, host and port -- the fetch began with
// (technical plan §43.15). The host of the client_id URL is what the
// consent page shows as the client's identity, so the document must be
// served by that origin: an open redirect on one host must never lend its
// name to a document served by another. A redirect to another host --
// even one the guard would let the fetch reach, on the same allowed
// address and port with a certificate that verifies -- or to another
// port is refused before it is followed; a same-origin redirect, the
// host differing only in ASCII case, is followed. A Location whose host is
// not written in plain ASCII is refused too, percent-encoded or raw UTF-8
// (technical plan §43.15): U+0130 in place of an "i" lower-cases to the
// very host the fetch began on, while net/http resolves, dials and
// verifies another one -- which this test makes reachable and trusted, so
// only the redirect policy stands in the way.
func TestCIMDFetch_RefusesCrossOriginRedirects(t *testing.T) {
	t.Parallel()
	var otherHostHits, idnaHostHits atomic.Int32
	other := newDocServer(t, documentHandler(nil))
	srv := newDocServer(t, documentHandler(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.Host, otherHost) {
			otherHostHits.Add(1)
		}
		if r.TLS != nil && strings.HasPrefix(r.TLS.ServerName, "xn--") {
			idnaHostHits.Add(1)
		}
		if r.URL.Path == "/out" {
			// An open redirect: wherever "to" says, byte for byte (not
			// http.Redirect, which would percent-encode a raw UTF-8 host).
			w.Header().Set("Location", r.URL.Query().Get("to"))
			w.WriteHeader(http.StatusFound)
			return true
		}
		return false
	}))
	res := newFakeResolver()
	res.set(docHost, addrs("127.0.0.1"))
	res.set(docHostOtherCase, addrs("127.0.0.1"))
	res.set(otherHost, addrs("127.0.0.1"))
	res.set(dottedIHost, addrs("127.0.0.1"))
	res.set(dottedIHostIDNA, addrs("127.0.0.1"))
	res.set(kelvinHost, addrs("127.0.0.1"))
	roots := x509.NewCertPool()
	roots.AddCert(srv.leaf)
	roots.AddCert(other.leaf)
	// Both servers are reachable and trusted: nothing but the redirect
	// policy can refuse what follows.
	fetch := cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{
		Resolver:       res,
		AllowAddrPorts: []netip.AddrPort{srv.addr, other.addr},
		RootCAs:        roots,
	}), testFetchTimeout)
	out := func(to string) string { return srv.url(docHost, "/out?to="+url.QueryEscape(to)) }
	outOn := func(host, to string) string { return srv.url(host, "/out?to="+url.QueryEscape(to)) }
	port := fmt.Sprint(srv.addr.Port())

	for _, tc := range []struct {
		name    string
		url     string
		wantErr error // nil: the fetch succeeds
	}{
		{"another host on the same allowed address and port", out(srv.url(otherHost, "/client.json")), cimdfetch.ErrCrossOriginRedirect},
		{"the same host on another port", out(other.url(docHost, "/client.json")), cimdfetch.ErrCrossOriginRedirect},
		{"another host after a same-origin hop", out(out(srv.url(otherHost, "/client.json"))), cimdfetch.ErrCrossOriginRedirect},
		{"another scheme", out("http://" + docHost + "/client.json"), cimdfetch.ErrNotHTTPS},
		{"a relative redirect", out("/client.json"), nil},
		{"an absolute same-origin redirect", out(srv.url(docHost, "/client.json")), nil},
		{"the same origin with the host in another case", out(srv.url(docHostOtherCase, "/client.json")), nil},
		{"a scheme-relative same-origin redirect", out("//" + docHost + ":" + port + "/client.json"), nil},
		{"U+0130 for the i, percent-encoded", outOn(dottedIHost, "https://%C4%B0nfo.example:"+port+"/client.json"), cimdfetch.ErrHostNotPlainASCII},
		{"U+0130 for the i, raw UTF-8", outOn(dottedIHost, "https://\u0130nfo.example:"+port+"/client.json"), cimdfetch.ErrHostNotPlainASCII},
		{"U+0130 for the i, scheme-relative", outOn(dottedIHost, "//\u0130nfo.example:"+port+"/client.json"), cimdfetch.ErrHostNotPlainASCII},
		{"U+212A for the k, percent-encoded", outOn(kelvinHost, "https://%E2%84%AAilo.example:"+port+"/client.json"), cimdfetch.ErrHostNotPlainASCII},
		{"U+212A for the k, raw UTF-8", outOn(kelvinHost, "https://\u212Ailo.example:"+port+"/client.json"), cimdfetch.ErrHostNotPlainASCII},
		{"no redirect: U+0130 in the first URL, percent-encoded", "https://%C4%B0nfo.example:" + port + "/client.json", cimdfetch.ErrHostNotPlainASCII},
		{"no redirect: U+0130 in the first URL, raw UTF-8", "https://\u0130nfo.example:" + port + "/client.json", cimdfetch.ErrHostNotPlainASCII},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fetch.Fetch(context.Background(), tc.url)
			switch {
			case tc.wantErr == nil && (err != nil || string(got.Body) != validDoc):
				t.Fatalf("Fetch(%s) = %q, %v, want the document", tc.url, got.Body, err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("Fetch(%s) = %v, want %v", tc.url, err, tc.wantErr)
			}
		})
	}
	if n := otherHostHits.Load(); n != 0 {
		t.Errorf("a redirect reached %s %d times, want never", otherHost, n)
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("a redirect reached the server on another port %d times, want never", n)
	}
	if n, m := idnaHostHits.Load(), res.lookups(dottedIHostIDNA); n != 0 || m != 0 {
		t.Errorf("a redirect reached %s (%d requests, %d lookups), want never", dottedIHostIDNA, n, m)
	}
}

// stallTimeout is the fetch timeout of the stalled-body case: long enough
// for a loopback handshake under load to reach the body, where the
// deadline must strike.
const stallTimeout = 250 * time.Millisecond

// TestCIMDFetch_RefusesHTTPAndOversize: https only, first and redirected;
// at most three redirects; application/json only; 64 KiB at most, the
// cap itself allowed; 200 only; a body that says where it ends (a
// Content-Length or chunked) and is read to that end; a whole-fetch
// timeout; and a fetcher built with no guarded client fetches nothing.
func TestCIMDFetch_RefusesHTTPAndOversize(t *testing.T) {
	t.Parallel()
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validDoc))
	}))
	t.Cleanup(plain.Close)
	plainAddr := netip.MustParseAddrPort(plain.Listener.Addr().String())

	// The stalled body stays stalled until the stall case lets it go,
	// after its fetch has returned: nothing but the client giving up can
	// end that fetch.
	release := make(chan struct{})
	stalled := make(chan struct{}, 1)
	srv := newDocServer(t, documentHandler(func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path
		switch {
		case path == "/to-http":
			http.Redirect(w, r, plain.URL+"/client.json", http.StatusFound)
		case path == "/to-ftp":
			http.Redirect(w, r, "ftp://"+docHost+"/client.json", http.StatusFound)
		case strings.HasPrefix(path, "/hops/"):
			var n int
			_, _ = fmt.Sscanf(strings.TrimPrefix(path, "/hops/"), "%d", &n)
			if n == 0 {
				r.URL.Path = "/client.json"
				return false
			}
			http.Redirect(w, r, fmt.Sprintf("/hops/%d", n-1), http.StatusFound)
		case path == "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(validDoc))
		case path == "/json-charset":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(validDoc))
		case path == "/untyped":
			_, _ = w.Write([]byte(validDoc))
		case path == "/gzip-doc", path == "/gzip-over-cap":
			// Compressed whatever was asked, as a hostile host would: the
			// cap is on the DECODED document, so a small compressed body
			// inflating past it is refused.
			body := []byte(validDoc)
			if path == "/gzip-over-cap" {
				body = []byte(strings.Repeat(" ", 1<<20))
			}
			var zipped bytes.Buffer
			zw := gzip.NewWriter(&zipped)
			_, _ = zw.Write(body)
			_ = zw.Close()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(zipped.Bytes())
		case path == "/at-cap", path == "/over-cap":
			size := cimdfetch.MaxDocumentBytes
			if path == "/over-cap" {
				size++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat(" ", size)))
		case path == "/sized":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", fmt.Sprint(len(validDoc)))
			_, _ = w.Write([]byte(validDoc))
		case path == "/chunked":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validDoc))
			w.(http.Flusher).Flush()
		case path == "/close-delimited":
			// No length and no chunking: only the connection closing ends
			// this body.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Transfer-Encoding", "identity")
			_, _ = w.Write([]byte(validDoc))
		case path == "/short-of-its-length":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", fmt.Sprint(len(validDoc)+10))
			_, _ = w.Write([]byte(validDoc))
		case path == "/chunked-cut-short":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validDoc[:len(validDoc)/2]))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case path == "/not-found":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		case path == "/stall":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validDoc[:len(validDoc)/2]))
			w.(http.Flusher).Flush()
			stalled <- struct{}{}
			// Not r.Context(): the server cancels it when the client's
			// TLS close_notify arrives, and a handler returning then ends
			// the body cleanly -- the very end a timed-out fetch must not
			// take for a document (deadline_test.go makes that race
			// certain). Here the stall outlasts the client.
			<-release
		default:
			return false
		}
		return true
	}))
	res := newFakeResolver()
	res.set(docHost, addrs("127.0.0.1"))
	fetcher := cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{
		Resolver: res,
		// The plain-HTTP server is allowed too, so what refuses it is the
		// scheme rule and nothing else.
		AllowAddrPorts: []netip.AddrPort{srv.addr, plainAddr},
		RootCAs:        srv.roots,
	}), testFetchTimeout)

	for _, tc := range []struct {
		name    string
		url     string
		wantErr error // nil: the fetch succeeds
	}{
		{"http://", plain.URL + "/client.json", cimdfetch.ErrNotHTTPS},
		{"an https to http redirect", srv.url(docHost, "/to-http"), cimdfetch.ErrNotHTTPS},
		{"a redirect to another scheme", srv.url(docHost, "/to-ftp"), cimdfetch.ErrNotHTTPS},
		{"three redirects", srv.url(docHost, "/hops/3"), nil},
		{"four redirects", srv.url(docHost, "/hops/4"), cimdfetch.ErrTooManyRedirects},
		{"text/html", srv.url(docHost, "/html"), cimdfetch.ErrNotJSON},
		{"no Content-Type", srv.url(docHost, "/untyped"), cimdfetch.ErrNotJSON},
		{"application/json with a charset", srv.url(docHost, "/json-charset"), nil},
		{"exactly 64 KiB", srv.url(docHost, "/at-cap"), nil},
		{"64 KiB + 1", srv.url(docHost, "/over-cap"), cimdfetch.ErrTooLarge},
		{"1 MiB compressed to about 1 KiB", srv.url(docHost, "/gzip-over-cap"), cimdfetch.ErrTooLarge},
		{"404", srv.url(docHost, "/not-found"), cimdfetch.ErrUnexpectedStatus},
		{"a body with a Content-Length", srv.url(docHost, "/sized"), nil},
		{"a chunked body", srv.url(docHost, "/chunked"), nil},
		{"a body only the connection closing ends", srv.url(docHost, "/close-delimited"), cimdfetch.ErrUnframedBody},
		{"a body short of its Content-Length", srv.url(docHost, "/short-of-its-length"), io.ErrUnexpectedEOF},
		{"a chunked body cut short", srv.url(docHost, "/chunked-cut-short"), io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetcher.Fetch(context.Background(), tc.url)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Fetch(%s) = %v, want success", tc.url, err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("Fetch(%s) = %v, want %v", tc.url, err, tc.wantErr)
			}
		})
	}
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the plain-HTTP server was reached %d times, want never", n)
	}

	t.Run("a compressed document is decoded, then capped", func(t *testing.T) {
		got, err := fetcher.Fetch(context.Background(), srv.url(docHost, "/gzip-doc"))
		if err != nil || string(got.Body) != validDoc {
			t.Fatalf("Fetch of a gzip-encoded document = %q, %v, want the decoded document", got.Body, err)
		}
	})

	t.Run("the whole fetch is bounded by one timeout", func(t *testing.T) {
		defer close(release)
		short := cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{
			Resolver: res, AllowAddrPorts: []netip.AddrPort{srv.addr}, RootCAs: srv.roots,
		}), stallTimeout)
		got, err := short.Fetch(context.Background(), srv.url(docHost, "/stall"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Fetch of a stalled body = %q, %v, want context.DeadlineExceeded", got.Body, err)
		}
		// The deadline struck the body: the server had sent half the
		// document and was still stalled when the fetch gave up.
		select {
		case <-stalled:
		default:
			t.Fatalf("the fetch timed out before the server began the body: %v", err)
		}
	})

	t.Run("a fetcher built with no guarded client fetches nothing", func(t *testing.T) {
		before := srv.hits.Load()
		if _, err := cimdfetch.New(nil, testFetchTimeout).Fetch(context.Background(), srv.url(docHost, "/client.json")); err == nil {
			t.Fatal("Fetch with no guarded client succeeded")
		}
		if srv.hits.Load() != before {
			t.Error("a fetcher with no guarded client reached the server")
		}
	})
}

package cimdfetch_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	docHost      = "doc.example"
	internalHost = "internal.example"
	mixedHost    = "mixed.example"
	loopHost     = "loop.example"
	rebindHost   = "rebind.example"
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
		DNSNames:              []string{docHost, internalHost, mixedHost, loopHost, rebindHost},
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
	s := &docServer{roots: x509.NewCertPool()}
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
// time -- as a literal, as a name resolving to it, as a redirect target,
// and as a name whose answer changes between two dials -- and the error
// says so.
func TestCIMDFetch_RefusesPrivateTargets(t *testing.T) {
	t.Parallel()
	srv := newDocServer(t, documentHandler(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/to-private":
			http.Redirect(w, r, "https://10.0.0.9/client.json", http.StatusFound)
		case "/to-metadata":
			http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusTemporaryRedirect)
		case "/to-loop-name":
			// The same loopback address as this server, but another port:
			// the seam allows one exact pair and nothing else.
			http.Redirect(w, r, fmt.Sprintf("https://%s:%d/client.json", loopHost, srv0Port(r)+1), http.StatusFound)
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
	res.set(loopHost, addrs("127.0.0.1"))
	// rebind.example answers the test server's own (allowed) address on
	// the first lookup and the cloud metadata address on every later one:
	// what an attacker's DNS does to a check-then-connect fetcher.
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
		{"a redirect into a private address", srv.url(docHost, "/to-private")},
		{"a redirect into the cloud metadata address", srv.url(docHost, "/to-metadata")},
		{"a redirect to another port on loopback", srv.url(docHost, "/to-loop-name")},
		{"a DNS answer that changes between checks", srv.url(rebindHost, "/rebind-first")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetch.Fetch(context.Background(), tc.url)
			if !errors.Is(err, cimdfetch.ErrForbiddenAddress) {
				t.Fatalf("Fetch(%s) = %v, want ErrForbiddenAddress", tc.url, err)
			}
		})
	}

	// The rebinding case was refused on its SECOND dial, after the first
	// reached the server: each dial resolved and was checked afresh.
	if n := res.lookups(rebindHost); n != 2 {
		t.Errorf("rebind.example was resolved %d times, want 2 (once per dial)", n)
	}
}

// srv0Port is the port the request arrived on.
func srv0Port(r *http.Request) int {
	ap, err := netip.ParseAddrPort(r.Host)
	if err == nil {
		return int(ap.Port())
	}
	_, port, _ := net.SplitHostPort(r.Host)
	var n int
	_, _ = fmt.Sscanf(port, "%d", &n)
	return n
}

// TestCIMDFetch_RefusesHTTPAndOversize: https only, first and redirected;
// at most three redirects; application/json only; 64 KiB at most, the
// cap itself allowed; 200 only; a whole-fetch timeout; and a fetcher
// built with no guarded client fetches nothing.
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

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
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
		case path == "/at-cap", path == "/over-cap":
			size := cimdfetch.MaxDocumentBytes
			if path == "/over-cap" {
				size++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat(" ", size)))
		case path == "/not-found":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		case path == "/stall":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
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
		{"404", srv.url(docHost, "/not-found"), cimdfetch.ErrUnexpectedStatus},
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

	t.Run("the whole fetch is bounded by one timeout", func(t *testing.T) {
		short := cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{
			Resolver: res, AllowAddrPorts: []netip.AddrPort{srv.addr}, RootCAs: srv.roots,
		}), 100*time.Millisecond)
		if _, err := short.Fetch(context.Background(), srv.url(docHost, "/stall")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Fetch of a stalled body = %v, want context.DeadlineExceeded", err)
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

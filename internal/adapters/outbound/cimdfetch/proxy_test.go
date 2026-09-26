package cimdfetch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/cimdfetch"
)

// Environment the proxy test hands its child process.
const (
	// proxyChildEnv marks the child: it runs the fetches, not the proxy.
	proxyChildEnv = "NARVI_CIMDFETCH_PROXY_CHILD"
	// proxyAddrEnv is the recording proxy's address, which the child's
	// guard is allowed to reach.
	proxyAddrEnv = "NARVI_CIMDFETCH_PROXY_ADDR"
	// proxyChildTimeout bounds the child process: a failure deadline, not
	// a pacing delay.
	proxyChildTimeout = 2 * time.Minute
)

// proxyVariables are every variable net/http reads a proxy from.
var proxyVariables = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"}

// TestCIMDFetch_IgnoresEnvironmentProxy: the fetcher never goes through a
// proxy the environment names (technical plan §43.15) -- a proxy makes the
// connection on the guard's behalf, where the dial-time address check sees
// only the proxy, never where the proxy goes. net/http reads HTTPS_PROXY
// and HTTP_PROXY once per process, so the fetches run in a child process
// whose environment names a recording proxy from its very start (the child
// first checks that net/http does see it there, or the test would prove
// nothing). The proxy's address is one the child's guard may reach, so
// only the fetcher's own policy keeps a request from it: a document is
// still fetched directly, a name resolving to a private address is still
// refused at dial time, and the proxy is never asked anything.
func TestCIMDFetch_IgnoresEnvironmentProxy(t *testing.T) {
	if os.Getenv(proxyChildEnv) != "" {
		fetchBesideAnEnvironmentProxy(t)
		return
	}
	t.Parallel()

	var (
		mu    sync.Mutex
		asked []string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.Host)
		mu.Unlock()
		http.Error(w, "this proxy must never be asked", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	env := make([]string, 0, len(os.Environ())+len(proxyVariables)+2)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		keep := true
		for _, v := range proxyVariables {
			keep = keep && name != v
		}
		if keep {
			env = append(env, kv)
		}
	}
	env = append(env,
		proxyChildEnv+"=1",
		proxyAddrEnv+"="+proxy.Listener.Addr().String(),
		"HTTPS_PROXY="+proxy.URL, "https_proxy="+proxy.URL,
		"HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL,
	)

	ctx, cancel := context.WithTimeout(context.Background(), proxyChildTimeout)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCIMDFetch_IgnoresEnvironmentProxy$", "-test.count=1", "-test.v")
	child.Env = env
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("the fetching child failed: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 0 {
		t.Fatalf("the environment's proxy was asked %d times (%v), want never", len(asked), asked)
	}
}

// fetchBesideAnEnvironmentProxy is the child's half: its environment
// names the recording proxy.
func fetchBesideAnEnvironmentProxy(t *testing.T) {
	proxyAddr := netip.MustParseAddrPort(os.Getenv(proxyAddrEnv))
	probe := httptest.NewRequest(http.MethodGet, "https://"+docHost+"/client.json", nil)
	if u, err := http.ProxyFromEnvironment(probe); err != nil || u == nil || u.Host != proxyAddr.String() {
		t.Fatalf("net/http does not see the environment's proxy for %s (got %v, %v): this test would prove nothing", probe.URL, u, err)
	}

	srv := newDocServer(t, documentHandler(nil))
	res := newFakeResolver()
	res.set(docHost, addrs("127.0.0.1"))
	res.set(internalHost, addrs("10.0.0.7"))
	fetch := cimdfetch.New(cimdfetch.NewGuardedClient(cimdfetch.GuardConfig{
		Resolver:       res,
		AllowAddrPorts: []netip.AddrPort{srv.addr, proxyAddr},
		RootCAs:        srv.roots,
	}), testFetchTimeout)

	if got, err := fetch.Fetch(context.Background(), srv.url(docHost, "/client.json")); err != nil || string(got.Body) != validDoc {
		t.Fatalf("Fetch of the document with a proxy in the environment = %q, %v, want it fetched directly", got.Body, err)
	}
	if _, err := fetch.Fetch(context.Background(), "https://"+internalHost+"/client.json"); !errors.Is(err, cimdfetch.ErrForbiddenAddress) {
		t.Fatalf("Fetch of a name resolving to a private address with a proxy in the environment = %v, want ErrForbiddenAddress", err)
	}
}

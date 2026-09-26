package cimdfetch

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// deadlineTestTimeout is the fetch timeout of the deadline test: long
// enough for a loopback handshake under load to reach the body, which is
// where the test needs the deadline to strike.
const deadlineTestTimeout = 250 * time.Millisecond

// holdOpenFailureDeadline bounds how long a held-open socket waits for the
// server's answer to close_notify: a failure deadline, not a pacing delay.
const holdOpenFailureDeadline = 10 * time.Second

// holdOpenConn is the client's socket under the TLS layer, with one change:
// its Close -- which crypto/tls calls just after writing close_notify --
// waits until a whole TLS record has come back since that last write
// began. The server's answer to close_notify therefore reaches the TLS
// reader before the socket is gone, every time, instead of only when the
// scheduler happens to allow it (the interleaving the CI failure hit).
type holdOpenConn struct {
	net.Conn
	mu        sync.Mutex
	sinceSent []byte // bytes read since the last Write began
	closing   bool
	answered  chan struct{}
	once      sync.Once
}

func newHoldOpenConn(c net.Conn) *holdOpenConn {
	return &holdOpenConn{Conn: c, answered: make(chan struct{})}
}

func (c *holdOpenConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.sinceSent = c.sinceSent[:0]
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *holdOpenConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.sinceSent = append(c.sinceSent, p[:n]...)
	c.signalIfAnsweredLocked()
	c.mu.Unlock()
	return n, err
}

// signalIfAnsweredLocked closes answered once the socket is closing and a
// whole TLS record (5-byte header, then the length it gives) has been read
// since the last write -- close_notify -- began.
func (c *holdOpenConn) signalIfAnsweredLocked() {
	b := c.sinceSent
	if c.closing && len(b) >= 5 && len(b) >= 5+(int(b[3])<<8|int(b[4])) {
		c.once.Do(func() { close(c.answered) })
	}
}

func (c *holdOpenConn) Close() error {
	c.mu.Lock()
	c.closing = true
	c.signalIfAnsweredLocked()
	c.mu.Unlock()
	select {
	case <-c.answered:
	case <-time.After(holdOpenFailureDeadline):
	}
	return c.Conn.Close()
}

// TestFetch_ABodyEndedBecauseTheFetchGaveUpIsATimeout is the CI failure's
// own reproduction, made deterministic. When a fetch's timeout fires, the
// transport closes the connection, and crypto/tls's Close sends
// close_notify before it closes the socket. The server reads that as the
// client leaving and cancels the request's context; a handler that
// returns then ends the chunked body cleanly. If that clean end reaches
// the client before its socket is gone, net/http reports it as a clean
// end -- it turns only a read ERROR into the context's -- so the body,
// cut short by the fetch's own giving up, reads as a whole one. The rig
// holds the client's socket open until the server's answer is in, which
// makes that interleaving certain; it first proves the transport does
// hand back a clean end after the deadline, then that Fetch answers the
// timeout all the same.
func TestFetch_ABodyEndedBecauseTheFetchGaveUpIsATimeout(t *testing.T) {
	t.Parallel()
	const firstHalf = `{"client_id":"https://doc.example/client.json",`
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(firstHalf))
		w.(http.Flusher).Flush()
		// Stalled until the client leaves; then the body is ended cleanly.
		<-r.Context().Done()
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	gc := NewGuardedClient(GuardConfig{
		AllowAddrPorts: []netip.AddrPort{netip.MustParseAddrPort(srv.Listener.Addr().String())},
		RootCAs:        roots,
	})
	tr := gc.http.Transport.(httpsOnly).next.(*http.Transport)
	guardedDial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := guardedDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return newHoldOpenConn(c), nil
	}

	// The rig's precondition: through this very client, the stalled body
	// ends CLEANLY after the deadline, cut short.
	ctx, cancel := context.WithTimeout(context.Background(), deadlineTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/client.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gc.http.Do(req)
	if err != nil {
		t.Fatalf("the rig's own request: %v", err)
	}
	raw, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if rerr != nil || ctx.Err() == nil || string(raw) != firstHalf {
		t.Fatalf("the rig did not reproduce the interleaving: read %q, %v, context %v; want the first half, a clean end, and a deadline already past",
			raw, rerr, ctx.Err())
	}

	got, err := New(gc, deadlineTestTimeout).Fetch(context.Background(), srv.URL+"/client.json")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch of a body its server ended because the fetch gave up = %q, %v, want context.DeadlineExceeded", got.Body, err)
	}
}

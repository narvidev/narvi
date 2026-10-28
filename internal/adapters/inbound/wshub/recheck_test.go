package wshub

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/internal/app/actorauthz"
)

// accessCheckMetrics is a reader on the global OTel meter provider, set
// once per test process: clientWSAccessCheckTotal is built from the global
// provider (sync.OnceValue), which hands every instrument made before a
// provider is set over to the first one set, so this reader sees the
// counter whichever test used it first.
var accessCheckMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
})

// accessCheckCount is client_ws_access_check_total{phase, outcome} so far.
func accessCheckCount(t *testing.T, phase, outcome string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := accessCheckMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	want := attribute.NewSet(attribute.String("phase", phase), attribute.String("outcome", outcome))
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != wshubMeterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "client_ws_access_check_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("client_ws_access_check_total is a %T, want an int64 sum", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// recheckRun is one recheckClientAccessLoop on the server side of a real
// socket pair, and the client side of it.
type recheckRun struct {
	client   *websocket.Conn
	cancel   context.CancelFunc // cancels the loop's ctx
	loopDone chan struct{}      // closed when the loop returns
}

// startRecheckLoop serves one socket whose handler runs
// recheckClientAccessLoop with check and revoke, and dials it. The handler
// holds the socket open after the loop returns, until the test ends, so
// the client sees only what the loop itself sent.
func startRecheckLoop(t *testing.T, interval time.Duration, check func(context.Context) actorauthz.LinkedActorVerdict, revoke func()) recheckRun {
	t.Helper()
	loopCtx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	run := recheckRun{cancel: cancel, loopDone: make(chan struct{})}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		recheckClientAccessLoop(loopCtx, conn, interval, check, revoke, logger)
		close(run.loopDone)
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	t.Cleanup(cancel)

	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	run.client = client
	return run
}

// TestRecheckClientAccessLoop drives the open socket's re-check with
// scripted verdicts: each row asserts the close code, how many checks ran,
// the outcome counted, and that the connection was revoked before the
// client saw the close -- and that a check cut short by the connection
// ending is neither counted nor acted on.
func TestRecheckClientAccessLoop(t *testing.T) {
	const interval = 20 * time.Millisecond

	tests := []struct {
		name       string
		verdicts   []actorauthz.LinkedActorVerdict // the check's answers, in order
		wantClose  websocket.StatusCode
		wantChecks int32
		wantCounts map[string]int64 // outcome -> recheck count added
	}{
		{
			name:       "denied closes 4001",
			verdicts:   []actorauthz.LinkedActorVerdict{actorauthz.LinkedActorDenied},
			wantClose:  websocket.StatusCode(4001),
			wantChecks: 1,
			wantCounts: map[string]int64{"denied": 1},
		},
		{
			name:       "error closes 1011",
			verdicts:   []actorauthz.LinkedActorVerdict{actorauthz.LinkedActorError},
			wantClose:  websocket.StatusInternalError,
			wantChecks: 1,
			wantCounts: map[string]int64{"error": 1},
		},
		{
			name:       "allowed keeps checking until denied",
			verdicts:   []actorauthz.LinkedActorVerdict{actorauthz.LinkedActorAllowed, actorauthz.LinkedActorAllowed, actorauthz.LinkedActorDenied},
			wantClose:  websocket.StatusCode(4001),
			wantChecks: 3,
			wantCounts: map[string]int64{"allowed": 2, "denied": 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := map[string]int64{}
			for _, outcome := range []string{"allowed", "denied", "error"} {
				before[outcome] = accessCheckCount(t, accessCheckPhaseRecheck, outcome)
			}

			var checks atomic.Int32
			var revoked atomic.Int32
			check := func(context.Context) actorauthz.LinkedActorVerdict {
				n := int(checks.Add(1))
				return tc.verdicts[min(n, len(tc.verdicts))-1]
			}
			run := startRecheckLoop(t, interval, check, func() { revoked.Add(1) })

			readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, data, err := run.client.Read(readCtx)
			if err == nil {
				t.Fatalf("Read = %s, want the socket closed %d", data, tc.wantClose)
			}
			// Read at the close, before anything else: the loop revoked
			// before it wrote the close frame.
			if got := revoked.Load(); got != 1 {
				t.Errorf("revoke ran %d times when the client saw the close, want 1, before the close", got)
			}
			if got := websocket.CloseStatus(err); got != tc.wantClose {
				t.Errorf("close code = %d (%v), want %d", got, err, tc.wantClose)
			}

			select {
			case <-run.loopDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the loop did not return after closing the socket")
			}
			if got := checks.Load(); got != tc.wantChecks {
				t.Errorf("checks = %d, want %d", got, tc.wantChecks)
			}
			for _, outcome := range []string{"allowed", "denied", "error"} {
				if got := accessCheckCount(t, accessCheckPhaseRecheck, outcome) - before[outcome]; got != tc.wantCounts[outcome] {
					t.Errorf("client_ws_access_check_total{phase=recheck,outcome=%s} rose by %d, want %d", outcome, got, tc.wantCounts[outcome])
				}
			}
		})
	}

	t.Run("a check cut short by the connection ending is neither counted nor acted on", func(t *testing.T) {
		before := accessCheckCount(t, accessCheckPhaseRecheck, "error")

		started := make(chan struct{}, 1)
		var revoked atomic.Int32
		check := func(ctx context.Context) actorauthz.LinkedActorVerdict {
			started <- struct{}{}
			<-ctx.Done()
			return actorauthz.LinkedActorError // what a store read returns once its context is cancelled
		}
		run := startRecheckLoop(t, interval, check, func() { revoked.Add(1) })

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the loop never ran a check")
		}
		run.cancel()
		select {
		case <-run.loopDone:
		case <-time.After(5 * time.Second):
			t.Fatal("the loop did not return once its context was cancelled")
		}

		if got := accessCheckCount(t, accessCheckPhaseRecheck, "error") - before; got != 0 {
			t.Errorf("client_ws_access_check_total{phase=recheck,outcome=error} rose by %d, want 0", got)
		}
		if got := revoked.Load(); got != 0 {
			t.Errorf("revoke ran %d times, want 0", got)
		}
		// No close frame: a short read only times out.
		readCtx, cancel := context.WithTimeout(context.Background(), 5*interval)
		defer cancel()
		_, data, err := run.client.Read(readCtx)
		if err == nil {
			t.Fatalf("Read = %s, want nothing sent", data)
		}
		if got := websocket.CloseStatus(err); got != -1 {
			t.Errorf("the socket was closed %d, want it left open", got)
		}
	})
}

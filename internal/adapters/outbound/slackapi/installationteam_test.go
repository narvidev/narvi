package slackapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/platform"
)

var otelReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	otelReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelReader))
	otel.SetMeterProvider(mp)

	code := m.Run()

	_ = mp.Shutdown(context.Background())
	os.Exit(code)
}

// teamLookupFailureCount reads slack_installation_team_lookup_failures_total
// for reason.
func teamLookupFailureCount(t *testing.T, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/slackapi" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "slack_installation_team_lookup_failures_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("reason")); ok && v.AsString() == reason {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// slowSlack is the per-call latency a profile-email attempt's budget is
// sized to tolerate: platform.Timeouts.IdentityEmailFetchTimeout's doc
// comment calls ~400-600ms healthy.
const slowSlack = 450 * time.Millisecond

// TestGetUserEmail_SlowSlackFitsOneAttempt proves a cold client verifies a
// member within one attempt's budget when Slack answers every call in
// slowSlack: auth.test runs alongside users.info, so the attempt costs one
// call's latency, not two -- for the Events API's retried attempt and the
// click's single one alike.
func TestGetUserEmail_SlowSlackFitsOneAttempt(t *testing.T) {
	t.Parallel()

	to := platform.DefaultTimeouts()
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{
		{"Events API attempt (IdentityEmailFetchTimeout)", to.IdentityEmailFetchTimeout},
		{"click attempt (SlackInteractivityIdentityFetchTimeout)", to.SlackInteractivityIdentityFetchTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake, client := newSlowFakeSlackUsers(t, fullMember(), map[string]any{"ok": true, "team_id": installationTeam}, slowSlack, slowSlack)

			ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
			defer cancel()
			user, ok, err := client.GetUserEmail(ctx, "U123")
			if err != nil || !ok {
				t.Fatalf("GetUserEmail() = (_, %v, %v), want an answer within %v", ok, err, tc.budget)
			}
			if !user.Verified() {
				t.Errorf("Verified() = false, want true (decoded %+v)", user)
			}
			if n := fake.count("/auth.test"); n != 1 {
				t.Errorf("auth.test calls = %d, want 1", n)
			}
		})
	}
}

// TestGetUserEmail_TeamLookupOutlivesTheAttempt proves the lookup runs on
// its own budget, not the attempt's: an attempt that gives up before
// auth.test answers fails (retryable), the lookup still finishes and is
// kept, and the next attempt is verified without asking again.
func TestGetUserEmail_TeamLookupOutlivesTheAttempt(t *testing.T) {
	t.Parallel()

	fake, client := newSlowFakeSlackUsers(t, fullMember(), map[string]any{"ok": true, "team_id": installationTeam}, 0, slowSlack)

	short, cancel := context.WithTimeout(context.Background(), slowSlack/3)
	defer cancel()
	if _, ok, err := client.GetUserEmail(short, "U123"); err == nil || ok {
		t.Fatalf("GetUserEmail() under a budget shorter than auth.test = (_, %v, %v), want an error", ok, err)
	}

	deadline := time.Now().Add(5 * slowSlack)
	for {
		user, ok, err := client.GetUserEmail(context.Background(), "U123")
		if err != nil || !ok {
			t.Fatalf("GetUserEmail() after the lookup = (_, %v, %v), want an answer", ok, err)
		}
		if user.Verified() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the installation's workspace was never learned (decoded %+v)", user)
		}
	}
	if n := fake.count("/auth.test"); n != 1 {
		t.Errorf("auth.test calls = %d, want 1: the attempt that gave up must leave its lookup running, and the next one must share it", n)
	}
}

// TestGetUserEmail_ConcurrentCallersShareOneLookup proves every caller
// waiting while the lookup is in flight shares it: one auth.test call for
// many concurrent cold fetches.
func TestGetUserEmail_ConcurrentCallersShareOneLookup(t *testing.T) {
	t.Parallel()

	fake, client := newSlowFakeSlackUsers(t, fullMember(), map[string]any{"ok": true, "team_id": installationTeam}, 0, slowSlack)

	var group errgroup.Group
	for range 8 {
		group.Go(func() error {
			user, ok, err := client.GetUserEmail(context.Background(), "U123")
			if err != nil {
				return err
			}
			if !ok || !user.Verified() {
				return errors.New("a concurrent fetch was not verified")
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("/auth.test"); n != 1 {
		t.Errorf("auth.test calls = %d, want 1 shared between 8 concurrent fetches", n)
	}
}

// TestWarmInstallationTeam proves the startup warm-up returns at once,
// whatever Slack's latency, and leaves the workspace learned for the first
// fetch.
func TestWarmInstallationTeam(t *testing.T) {
	t.Parallel()

	fake, client := newSlowFakeSlackUsers(t, fullMember(), map[string]any{"ok": true, "team_id": installationTeam}, 0, slowSlack)

	start := time.Now()
	client.WarmInstallationTeam()
	if took := time.Since(start); took >= slowSlack {
		t.Errorf("WarmInstallationTeam took %v, want it to return before auth.test answers (%v)", took, slowSlack)
	}

	deadline := time.Now().Add(5 * slowSlack)
	for fake.count("/auth.test") == 0 || time.Now().Before(start.Add(slowSlack)) {
		if time.Now().After(deadline) {
			t.Fatal("the warm-up never asked auth.test")
		}
		time.Sleep(slowSlack / 10)
	}

	// A fetch now joins or finds the warm-up's answer; it never asks again.
	user, ok, err := client.GetUserEmail(context.Background(), "U123")
	if err != nil || !ok || !user.Verified() {
		t.Fatalf("GetUserEmail() after the warm-up = (%+v, %v, %v), want verified", user, ok, err)
	}
	if n := fake.count("/auth.test"); n != 1 {
		t.Errorf("auth.test calls = %d, want 1", n)
	}
}

// TestGetUserEmail_TeamLookupTimeoutIsItsOwn proves the lookup is bounded
// by WithInstallationTeamLookupTimeout, never by the caller: with a budget
// shorter than auth.test's latency the lookup fails even for a caller that
// waits forever, and nothing is verified.
func TestGetUserEmail_TeamLookupTimeoutIsItsOwn(t *testing.T) {
	t.Parallel()

	_, client := newSlowFakeSlackUsers(t, fullMember(), map[string]any{"ok": true, "team_id": installationTeam}, 0, slowSlack)
	client.WithInstallationTeamLookupTimeout(slowSlack / 3)

	user, ok, err := client.GetUserEmail(context.Background(), "U123")
	if err == nil || ok || user.Verified() {
		t.Fatalf("GetUserEmail() = (%+v, %v, %v), want an error: the lookup's own budget ran out", user, ok, err)
	}
}

// syncBuffer is a bytes.Buffer safe for the logger and the test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestInstallationTeamLookupFailure_IsLoggedAndCounted proves a lookup that
// learns no workspace is never silent: a Warn and
// slack_installation_team_lookup_failures_total, by reason -- and fails
// closed, no member verified. Not parallel: it swaps the default logger and
// reads a process-wide counter, so it runs alone, before every parallel
// test resumes.
func TestInstallationTeamLookupFailure_IsLoggedAndCounted(t *testing.T) {
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for _, tc := range []struct {
		name     string
		authTest map[string]any
		reason   string
		wantErr  bool
	}{
		{"auth.test fails", map[string]any{"ok": false, "error": "invalid_auth"}, "request_failed", true},
		{"auth.test names no team", map[string]any{"ok": true}, "no_team", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := teamLookupFailureCount(t, tc.reason)
			logsBefore := strings.Count(logs.String(), `"level":"WARN"`)

			_, client := newFakeSlackUsers(t, fullMember(), tc.authTest)
			user, _, err := client.GetUserEmail(context.Background(), "U123")
			if (err != nil) != tc.wantErr {
				t.Fatalf("GetUserEmail() error = %v, want error: %v", err, tc.wantErr)
			}
			if user.Verified() {
				t.Errorf("Verified() = true, want false: no workspace learned")
			}

			if delta := teamLookupFailureCount(t, tc.reason) - before; delta != 1 {
				t.Errorf("slack_installation_team_lookup_failures_total{reason=%q} delta = %d, want 1", tc.reason, delta)
			}
			if got := strings.Count(logs.String(), `"level":"WARN"`) - logsBefore; got != 1 {
				t.Errorf("Warn lines = %d, want 1; logs: %s", got, logs.String())
			}
		})
	}
}

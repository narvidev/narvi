// This file (retry_test.go) proves the M19 audit fix's own distinction for
// FetchEmailWithRetry (retry.go): every retry attempt genuinely failing
// TRANSIENTLY (platform.Retry exhausts every attempt) must log a Warn line
// AND increment the new identity_email_fetch_failures_total counter --
// while a fetch that SUCCEEDS but reports a genuine "no email on file"
// outcome (ok=false, err=nil), OR one where the fetch closure positively
// identifies the failure as PERMANENT (platform.Permanent, e.g. Slack's own
// definitive user-not-found), must trigger NEITHER -- proving all three
// cases this finding cares about are actually told apart in the real code,
// not just superficially satisfied (e.g. by logging/counting on every
// ok=false return regardless of why).
//
// A plain (non-integration) test: FetchEmailWithRetry itself has no
// Postgres dependency at all -- only Resolve (service.go) does, covered by
// this package's own service_integration_test.go.
package identitylink_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/platform"
)

// otelReader is the SINGLE ManualReader backing the SINGLE, GLOBAL SDK
// MeterProvider TestMain below registers for this whole test binary --
// mirrors internal/app/outboxworker's own TestMain/otelReader precedent
// exactly, adapted to this package's own "narvi/identitylink" meter.
//
// identitylink's own emailFetchFailures counter (retry.go) is constructed
// as a package-level var, at package-load time, BEFORE this TestMain ever
// runs -- obtained from the global otel.Meter proxy active at that moment.
// That is safe and by design (see that var's own doc comment): every
// meter/instrument obtained from the global provider before
// otel.SetMeterProvider is ever called is transparently upgraded in place
// the instant it runs, so the counter still ends up recording against
// THIS test's own ManualReader-backed provider below.
var otelReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	otelReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelReader))
	otel.SetMeterProvider(mp)

	code := m.Run()

	_ = mp.Shutdown(context.Background())
	os.Exit(code)
}

// readEmailFetchFailureCount sums every identity_email_fetch_failures_total
// data point labeled provider -- CUMULATIVE across every test in this
// binary (see TestMain's own doc comment), so callers must diff a
// "before" and "after" reading around their own FetchEmailWithRetry
// call(s), exactly like outboxworker's own readDeadLetterCount precedent.
func readEmailFetchFailureCount(ctx context.Context, t *testing.T, provider sqlcgen.IdentityProvider) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "narvi/identitylink" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "identity_email_fetch_failures_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("identity_email_fetch_failures_total metric data = %T, want metricdata.Sum[int64]", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key("provider"))
				if ok && v.AsString() == string(provider) {
					total += dp.Value
				}
			}
			return total
		}
	}
	return 0
}

// fastRetryTimeouts returns small, fast platform.Timeouts values for these
// tests -- FetchEmailWithRetry exercises platform.Retry's own REAL backoff
// sleeps (retry.go does not mock time), so this keeps the unit tests
// themselves fast rather than reusing platform.DefaultTimeouts()'s own
// (deliberately larger, production-sized) IdentityEmailFetch* values.
func fastRetryTimeouts() platform.Timeouts {
	return platform.Timeouts{
		IdentityEmailFetchTimeout:        50 * time.Millisecond,
		IdentityEmailFetchMaxAttempts:    3,
		IdentityEmailFetchRetryBaseDelay: 2 * time.Millisecond,
		IdentityEmailFetchRetryMaxDelay:  4 * time.Millisecond,
	}
}

// TestFetchEmailWithRetry_AllAttemptsFail_LogsWarnAndIncrementsCounter
// proves M19's own "case 1" (every retry attempt genuinely fails with a
// real error): a Warn log line naming the provider and the real underlying
// error, AND the new identity_email_fetch_failures_total counter
// incrementing by exactly 1, labeled by provider.
func TestFetchEmailWithRetry_AllAttemptsFail_LogsWarnAndIncrementsCounter(t *testing.T) {
	ctx := context.Background()
	const provider = sqlcgen.IdentityProviderSlack
	to := fastRetryTimeouts()

	before := readEmailFetchFailureCount(ctx, t, provider)

	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	wantErr := errors.New("provider API is down")
	callCount := 0
	email, ok := identitylink.FetchEmailWithRetry(ctx, logger, to, provider, func(context.Context) (identitylink.ProfileEmail, bool, error) {
		callCount++
		return identitylink.ProfileEmail{}, false, wantErr
	})

	if ok {
		t.Errorf("ok = true, want false (every attempt failed)")
	}
	if email != (identitylink.ProfileEmail{}) {
		t.Errorf("email = %+v, want empty", email)
	}
	if callCount != to.IdentityEmailFetchMaxAttempts {
		t.Errorf("call count = %d, want %d (every attempt exhausted)", callCount, to.IdentityEmailFetchMaxAttempts)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, `"level":"WARN"`) {
		t.Errorf("expected a Warn-level log line, got: %s", logOut)
	}
	if !strings.Contains(logOut, string(provider)) {
		t.Errorf("expected the log line to mention provider %q, got: %s", provider, logOut)
	}
	if !strings.Contains(logOut, wantErr.Error()) {
		t.Errorf("expected the log line to include the real underlying error %q, got: %s", wantErr.Error(), logOut)
	}

	after := readEmailFetchFailureCount(ctx, t, provider)
	if delta := after - before; delta != 1 {
		t.Errorf("identity_email_fetch_failures_total delta = %d, want 1", delta)
	}
}

// TestFetchEmailWithRetry_PermanentError_LogsNothingAndDoesNotIncrementCounter
// proves the follow-up audit fix's own "case 3" (the fetch closure
// positively identifies the failure as PERMANENT via platform.Permanent --
// e.g. slackapi.ErrSlackUserNotFound for a Slack user id that no longer
// resolves): a closure that returns platform.Permanent(someErr) on its
// FIRST call must NOT trigger the Warn log or the counter, and must stop
// after exactly one call (platform.Retry's own "stop immediately on a
// Permanent error" behavior) -- distinct from
// TestFetchEmailWithRetry_AllAttemptsFail_LogsWarnAndIncrementsCounter
// above, which proves a genuine TRANSIENT error that exhausts every retry
// attempt SHOULD still trigger both.
func TestFetchEmailWithRetry_PermanentError_LogsNothingAndDoesNotIncrementCounter(t *testing.T) {
	ctx := context.Background()
	const provider = sqlcgen.IdentityProviderSlack
	to := fastRetryTimeouts()

	before := readEmailFetchFailureCount(ctx, t, provider)

	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	wantErr := errors.New("slackapi: user_not_found")
	callCount := 0
	email, ok := identitylink.FetchEmailWithRetry(ctx, logger, to, provider, func(context.Context) (identitylink.ProfileEmail, bool, error) {
		callCount++
		return identitylink.ProfileEmail{}, false, platform.Permanent(wantErr)
	})

	if ok {
		t.Errorf("ok = true, want false (a permanent error never resolves to a real email)")
	}
	if email != (identitylink.ProfileEmail{}) {
		t.Errorf("email = %+v, want empty", email)
	}
	if callCount != 1 {
		t.Errorf("call count = %d, want 1 (a Permanent error must stop retrying immediately)", callCount)
	}

	if logOut := logBuf.String(); logOut != "" {
		t.Errorf("expected NO log output for a Permanent error (normal, expected outcome, e.g. user-not-found), got: %s", logOut)
	}

	after := readEmailFetchFailureCount(ctx, t, provider)
	if delta := after - before; delta != 0 {
		t.Errorf("identity_email_fetch_failures_total delta = %d, want 0 (a Permanent error is not a broken fetch)", delta)
	}
}

// TestFetchEmailWithRetry_ParentContextEnds_LogsNothingAndDoesNotIncrementCounter
// proves the re-review follow-up fix: platform.Retry returns ctx.Err()
// directly (unwrapped, never touching the `permanent` flag) when the
// CALLER's own context is canceled/expires while sleeping between
// attempts -- e.g. the enclosing webhook handler's own request context
// ending for reasons that have nothing to do with the Slack/Linear API.
// That must NOT be conflated with a genuinely broken fetch either.
//
// Deterministic by construction, not a wall-clock race: the fetch closure
// itself cancels the outer context on its FIRST call (after returning a
// plain transient error), so platform.Retry's own backoff-sleep select
// sees ctx.Done() already closed before its timer fires, guaranteeing
// ctx.Err() is what FetchEmailWithRetry receives back.
func TestFetchEmailWithRetry_ParentContextEnds_LogsNothingAndDoesNotIncrementCounter(t *testing.T) {
	bgCtx := context.Background()
	ctx, cancel := context.WithCancel(bgCtx)
	const provider = sqlcgen.IdentityProviderSlack
	to := fastRetryTimeouts()

	// bgCtx (never canceled), not ctx, for both metric reads -- ctx is
	// deliberately canceled by the closure below partway through, and an
	// already-canceled context could make otelReader.Collect itself fail.
	before := readEmailFetchFailureCount(bgCtx, t, provider)

	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	callCount := 0
	email, ok := identitylink.FetchEmailWithRetry(ctx, logger, to, provider, func(context.Context) (identitylink.ProfileEmail, bool, error) {
		callCount++
		cancel()
		return identitylink.ProfileEmail{}, false, errors.New("slackapi: transient network error")
	})

	if ok {
		t.Errorf("ok = true, want false (the caller's own context ended before a real answer)")
	}
	if email != (identitylink.ProfileEmail{}) {
		t.Errorf("email = %+v, want empty", email)
	}
	if callCount != 1 {
		t.Errorf("call count = %d, want 1 (the canceled context must stop retrying during the backoff sleep, before a second attempt)", callCount)
	}

	if logOut := logBuf.String(); logOut != "" {
		t.Errorf("expected NO log output when the caller's own context ends (not evidence of a broken fetch), got: %s", logOut)
	}

	after := readEmailFetchFailureCount(bgCtx, t, provider)
	if delta := after - before; delta != 0 {
		t.Errorf("identity_email_fetch_failures_total delta = %d, want 0 (the caller's own context ending is not a broken fetch)", delta)
	}
}

// TestFetchEmailWithRetry_NoEmailOnFile_LogsNothingAndDoesNotIncrementCounter
// proves M19's own "case 2" (the fetch SUCCEEDS but the provider genuinely
// reports no email on file -- ok=false, err=nil): NEITHER the Warn log NOR
// the new counter fire, proving this normal, expected, non-actionable
// outcome is not conflated with case 1's genuinely broken fetch.
func TestFetchEmailWithRetry_NoEmailOnFile_LogsNothingAndDoesNotIncrementCounter(t *testing.T) {
	ctx := context.Background()
	const provider = sqlcgen.IdentityProviderLinear
	to := fastRetryTimeouts()

	before := readEmailFetchFailureCount(ctx, t, provider)

	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	callCount := 0
	email, ok := identitylink.FetchEmailWithRetry(ctx, logger, to, provider, func(context.Context) (identitylink.ProfileEmail, bool, error) {
		callCount++
		// The provider's own API call SUCCEEDED (nil error) -- it simply
		// has no email on file for this user. Never retried: platform.Retry
		// treats a nil error as success on the first attempt.
		return identitylink.ProfileEmail{}, false, nil
	})

	if ok {
		t.Errorf("ok = true, want false (provider reported no email)")
	}
	if email != (identitylink.ProfileEmail{}) {
		t.Errorf("email = %+v, want empty", email)
	}
	if callCount != 1 {
		t.Errorf("call count = %d, want 1 (a successful fetch, even one reporting no email, is never retried)", callCount)
	}

	if logOut := logBuf.String(); logOut != "" {
		t.Errorf("expected NO log output for a genuine 'no email on file' outcome, got: %s", logOut)
	}

	after := readEmailFetchFailureCount(ctx, t, provider)
	if delta := after - before; delta != 0 {
		t.Errorf("identity_email_fetch_failures_total delta = %d, want 0 (a genuine 'no email' outcome is not a broken fetch)", delta)
	}
}

// TestFetchEmailWithRetry_Success_LogsNothingAndDoesNotIncrementCounter
// rounds out the happy path: a genuinely successful fetch (a real email)
// returns it unchanged -- its Verified attestation included, whichever way
// the provider answered -- and triggers neither the Warn log nor the
// counter.
func TestFetchEmailWithRetry_Success_LogsNothingAndDoesNotIncrementCounter(t *testing.T) {
	tests := []struct {
		name string
		want identitylink.ProfileEmail
	}{
		{"verified email", identitylink.ProfileEmail{Address: "person@example.com", Verified: true}},
		{"unverified email", identitylink.ProfileEmail{Address: "person@example.com", Verified: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			const provider = sqlcgen.IdentityProviderSlack
			to := fastRetryTimeouts()

			before := readEmailFetchFailureCount(ctx, t, provider)

			var logBuf strings.Builder
			logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			email, ok := identitylink.FetchEmailWithRetry(ctx, logger, to, provider, func(context.Context) (identitylink.ProfileEmail, bool, error) {
				return tt.want, true, nil
			})

			if !ok {
				t.Errorf("ok = false, want true (fetch succeeded with a real email)")
			}
			if email != tt.want {
				t.Errorf("email = %+v, want %+v", email, tt.want)
			}
			if logOut := logBuf.String(); logOut != "" {
				t.Errorf("expected NO log output for a successful fetch, got: %s", logOut)
			}

			after := readEmailFetchFailureCount(ctx, t, provider)
			if delta := after - before; delta != 0 {
				t.Errorf("identity_email_fetch_failures_total delta = %d, want 0", delta)
			}
		})
	}
}

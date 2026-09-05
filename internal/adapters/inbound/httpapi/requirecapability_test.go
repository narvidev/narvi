package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/domain/license"
)

// okHandler is the next handler every RequireCapability test wraps --
// records whether it was ever reached.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// TestRequireCapability_503WhenDisabled proves the middleware closes the
// route with a 503 -- and never reaches the wrapped handler -- for every
// disabled reason: a nil registry (no module composed at all) and a
// non-nil registry that simply does not enable this capability.
func TestRequireCapability_503WhenDisabled(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		reg  *capability.Registry
	}{
		{
			name: "nil registry",
			reg:  nil,
		},
		{
			name: "capability not installed",
			reg: capability.New(nil,
				&license.Grant{Capabilities: []license.Capability{license.CapabilityOrganizationGovernance}, NotBefore: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
				nil, func() time.Time { return now }, 0),
		},
		{
			name: "no grant at all",
			reg: capability.New([]license.Capability{license.CapabilityOrganizationGovernance}, nil, nil,
				func() time.Time { return now }, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reached bool
			mw := httpapi.RequireCapability(tt.reg, license.CapabilityOrganizationGovernance)
			handler := mw(okHandler(&reached))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/ext/whatever", nil)
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
			}
			if reached {
				t.Error("wrapped handler was reached, want it never called when the capability is disabled")
			}
		})
	}
}

// TestRequireCapability_PassesWhenEnabled proves a registry that
// genuinely enables the capability lets the request through untouched.
func TestRequireCapability_PassesWhenEnabled(t *testing.T) {
	now := time.Now()
	reg := capability.New([]license.Capability{license.CapabilityOrganizationGovernance},
		&license.Grant{Capabilities: []license.Capability{license.CapabilityOrganizationGovernance}, NotBefore: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
		nil, func() time.Time { return now }, 0)

	var reached bool
	mw := httpapi.RequireCapability(reg, license.CapabilityOrganizationGovernance)
	handler := mw(okHandler(&reached))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ext/whatever", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !reached {
		t.Error("wrapped handler was never reached, want it called when the capability is enabled")
	}
}

// TestRequireCapability_ReEvaluatesPerRequest proves the SAME
// middleware-wrapped handler closes on the very next request once the
// underlying grant's own window lapses -- no caching of a first verdict,
// mirroring RequireCloudIdentityCapability's own per-request evaluation.
func TestRequireCapability_ReEvaluatesPerRequest(t *testing.T) {
	current := time.Now()
	grant := &license.Grant{
		Capabilities: []license.Capability{license.CapabilityOrganizationGovernance},
		NotBefore:    current.Add(-time.Hour),
		ExpiresAt:    current.Add(time.Hour),
	}
	reg := capability.New([]license.Capability{license.CapabilityOrganizationGovernance}, grant, nil,
		func() time.Time { return current }, 0)

	var reached bool
	mw := httpapi.RequireCapability(reg, license.CapabilityOrganizationGovernance)
	handler := mw(okHandler(&reached))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ext/whatever", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d (grant valid)", rec.Code, http.StatusOK)
	}
	if !reached {
		t.Fatal("first request: wrapped handler was never reached")
	}

	current = current.Add(2 * time.Hour) // advance past ExpiresAt -- same *Registry, same middleware value
	reached = false

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ext/whatever", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request (post-expiry) status = %d, want %d -- capability must close on the very next request", rec.Code, http.StatusServiceUnavailable)
	}
	if reached {
		t.Fatal("second request (post-expiry): wrapped handler was reached, want it never called once the grant expired")
	}
}

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/license"
	"github.com/narvidev/narvi/internal/platform"
)

// capabilitiesRequest builds an authenticated GET /api/capabilities
// request carrying role in its context -- platform.WithUser directly,
// exactly as platform.UserFromContext's own doc comment sanctions for a
// test, since this handler needs no real Postgres (unlike almost every
// other handler in this package, which is why this file is a plain
// _test.go rather than an integration one).
func capabilitiesRequest(role authz.Role) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	return req.WithContext(platform.WithUser(req.Context(), platform.AuthenticatedUser{ID: "00000000-0000-0000-0000-000000000001", Role: string(role)}))
}

// buildResponse mirrors controlplane.buildCapabilitiesResponse's own
// row-building loop, necessarily duplicated here rather than imported:
// GetCapabilities itself no longer accepts a *capability.Registry at all
// (capabilities.go's own doc comment explains why), and controlplane's
// own builder is unexported composition-root wiring this package must
// not reach into either way. A _test.go file may still import
// internal/app/capability and internal/domain/license directly --
// demotionsweep.skipFile's own "a test constructing a registry is not a
// production decision point" reasoning, applied by
// capabilityimportban's own identical _test.go exemption.
func buildResponse(reg *capability.Registry, gatekeeperInstalled bool) restdtos.CapabilitiesResponse {
	rows := make([]restdtos.CapabilityStatus, 0, len(license.All))
	for _, c := range license.All {
		rows = append(rows, restdtos.CapabilityStatus{
			Name:  restdtos.CapabilityStatusName(c),
			State: restdtos.CapabilityState(reg.State(c)),
		})
	}
	return restdtos.CapabilitiesResponse{
		GatekeeperInstalled: gatekeeperInstalled,
		LicenseExpiresAt:    reg.ExpiresAt(),
		Capabilities:        rows,
	}
}

// TestGetCapabilities_EveryRole proves authz.ActionViewCapabilities is
// genuinely a §13.3 row 1 action: every one of the four roles, viewer
// included, gets 200 -- and, since this response is a deployment-wide
// fact rather than anything scoped to the caller, the SAME body,
// regardless of which role asked.
func TestGetCapabilities_EveryRole(t *testing.T) {
	now := time.Now()
	grant := &license.Grant{
		Capabilities: []license.Capability{license.CapabilityOrganizationGovernance},
		NotBefore:    now.Add(-time.Hour),
		ExpiresAt:    now.Add(time.Hour),
	}
	reg := capability.New([]license.Capability{license.CapabilityOrganizationGovernance}, grant, nil, func() time.Time { return now }, 0)
	handler := httpapi.GetCapabilities(func() restdtos.CapabilitiesResponse { return buildResponse(reg, true) })

	var firstBody string
	for _, role := range authz.AllRoles {
		t.Run(string(role), func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, capabilitiesRequest(role))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (role %s must be allowed to read capabilities)", rec.Code, http.StatusOK, role)
			}
			if firstBody == "" {
				firstBody = rec.Body.String()
			} else if rec.Body.String() != firstBody {
				t.Errorf("body for role %s differs from the first role's own body -- this read model must not vary by caller", role)
			}
		})
	}
}

// TestGetCapabilities_NeverCarriesTheKey proves the response body never
// leaks the configured licence grant's own identifying material: neither
// KeyID (the closest thing this design has to "a fingerprint of the
// key" -- the wire format's own rotation label) nor Subject (the opaque
// customer id) ever reach the raw JSON bytes, however distinctive a
// value either one is given here. Mutation-verified: temporarily adding
// either field to the handler's own response makes this test fail (see
// this Step's own PR description for the exact mutation performed).
func TestGetCapabilities_NeverCarriesTheKey(t *testing.T) {
	const leakKeyID = "leak-marker-kid-should-never-appear-on-the-wire"
	const leakSubject = "leak-marker-subject-should-never-appear-on-the-wire"

	now := time.Now()
	grant := &license.Grant{
		KeyID:        leakKeyID,
		Subject:      leakSubject,
		Capabilities: []license.Capability{license.CapabilityOrganizationGovernance, license.CapabilityCompliance, license.CapabilityKnowledgeRetrieval},
		NotBefore:    now.Add(-time.Hour),
		ExpiresAt:    now.Add(time.Hour),
	}
	reg := capability.New(
		[]license.Capability{license.CapabilityOrganizationGovernance, license.CapabilityCompliance, license.CapabilityKnowledgeRetrieval},
		grant, nil, func() time.Time { return now }, 0,
	)
	handler := httpapi.GetCapabilities(func() restdtos.CapabilitiesResponse { return buildResponse(reg, true) })

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, capabilitiesRequest(authz.RoleAdmin))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if strings.Contains(body, leakKeyID) {
		t.Errorf("response body contains the grant's own KeyID %q -- a capability fingerprint must never reach this wire response:\n%s", leakKeyID, body)
	}
	if strings.Contains(body, leakSubject) {
		t.Errorf("response body contains the grant's own Subject %q -- the licence subject must never reach this wire response:\n%s", leakSubject, body)
	}
}

// TestGetCapabilities_StatesMatchRegistry proves three things at once for
// a table of registry configurations: (1) every row's own state matches
// *capability.Registry.State(c) computed independently for the SAME c;
// (2) all three rows are always present, in internal/domain/license.
// All's own fixed order, regardless of what is installed or licensed;
// (3) gatekeeperInstalled/licenseExpiresAt on the response match what was
// fed into GetCapabilities/the registry's own ExpiresAt.
func TestGetCapabilities_StatesMatchRegistry(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }
	expiry := now.Add(30 * 24 * time.Hour)

	tests := []struct {
		name                string
		reg                 *capability.Registry
		gatekeeperInstalled bool
	}{
		{
			name:                "nil registry: no module composed at all",
			reg:                 nil,
			gatekeeperInstalled: false,
		},
		{
			name: "module composed, no licence key",
			reg: capability.New(
				[]license.Capability{license.CapabilityOrganizationGovernance, license.CapabilityCompliance, license.CapabilityKnowledgeRetrieval},
				nil, nil, nowFunc, 0,
			),
			gatekeeperInstalled: true,
		},
		{
			name: "module composed, one capability fully enabled, others not granted",
			reg: capability.New(
				[]license.Capability{license.CapabilityOrganizationGovernance, license.CapabilityCompliance, license.CapabilityKnowledgeRetrieval},
				&license.Grant{Capabilities: []license.Capability{license.CapabilityOrganizationGovernance}, NotBefore: now.Add(-time.Hour), ExpiresAt: expiry},
				nil, nowFunc, 0,
			),
			gatekeeperInstalled: true,
		},
		{
			name: "module composed, licence expired",
			reg: capability.New(
				[]license.Capability{license.CapabilityCompliance},
				&license.Grant{Capabilities: []license.Capability{license.CapabilityCompliance}, NotBefore: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
				nil, nowFunc, 0,
			),
			gatekeeperInstalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := httpapi.GetCapabilities(func() restdtos.CapabilitiesResponse { return buildResponse(tt.reg, tt.gatekeeperInstalled) })

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, capabilitiesRequest(authz.RoleAdmin))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var resp restdtos.CapabilitiesResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
			}

			if resp.GatekeeperInstalled != tt.gatekeeperInstalled {
				t.Errorf("GatekeeperInstalled = %v, want %v", resp.GatekeeperInstalled, tt.gatekeeperInstalled)
			}

			wantExpiresAt := tt.reg.ExpiresAt()
			switch {
			case wantExpiresAt == nil && resp.LicenseExpiresAt != nil:
				t.Errorf("LicenseExpiresAt = %v, want nil", resp.LicenseExpiresAt)
			case wantExpiresAt != nil && (resp.LicenseExpiresAt == nil || !resp.LicenseExpiresAt.Equal(*wantExpiresAt)):
				t.Errorf("LicenseExpiresAt = %v, want %v", resp.LicenseExpiresAt, wantExpiresAt)
			}

			if len(resp.Capabilities) != len(license.All) {
				t.Fatalf("len(Capabilities) = %d, want %d (one row per license.All entry, always)", len(resp.Capabilities), len(license.All))
			}

			for i, c := range license.All {
				row := resp.Capabilities[i]
				if string(row.Name) != string(c) {
					t.Errorf("Capabilities[%d].Name = %q, want %q (license.All's own fixed order)", i, row.Name, c)
				}
				wantState := restdtos.CapabilityState(tt.reg.State(c))
				if row.State != wantState {
					t.Errorf("Capabilities[%d] (%s).State = %q, want %q (capability.Registry.State(%s) computed independently)", i, c, row.State, wantState, c)
				}
			}
		})
	}
}

// TestGetCapabilities_CallsBuildPerRequest proves build is invoked fresh
// on every request, never cached from Mount time -- the same "no stale
// snapshot" requirement RequireCapability's own
// TestRequireCapability_ReEvaluatesPerRequest pins for the sibling
// capability gate, applied here to the func GetCapabilities itself now
// takes. controlplane's own capabilityResponseBuilder closes over the
// registry, not a value already read from it, specifically so this
// property holds in production too.
func TestGetCapabilities_CallsBuildPerRequest(t *testing.T) {
	var calls int
	handler := httpapi.GetCapabilities(func() restdtos.CapabilitiesResponse {
		calls++
		return restdtos.CapabilitiesResponse{}
	})

	for i := 1; i <= 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, capabilitiesRequest(authz.RoleAdmin))

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i, rec.Code, http.StatusOK)
		}
		if calls != i {
			t.Fatalf("after request %d: build called %d times, want %d -- GetCapabilities must call build fresh per request, never cache it", i, calls, i)
		}
	}
}

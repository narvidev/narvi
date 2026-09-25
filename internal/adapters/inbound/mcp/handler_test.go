package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testTwins() Twins {
	return Twins{
		ListModels:   stubHandler(http.StatusOK, `{"providers":[]}`),
		ListSessions: stubHandler(http.StatusOK, `{"sessions":[]}`),
		GetSession:   stubHandler(http.StatusOK, `{"id":"x"}`),
	}
}

// rawPost drives handler directly via ServeHTTP (httptest.NewRequest/
// NewRecorder -- no real listening socket, no net/http client: see
// newTestHandler's own doc comment for why) with the given headers plus
// the Accept/Content-Type headers every Streamable HTTP request needs
// regardless of era, returning the raw status/body.
func rawPost(t testing.TB, handler http.Handler, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

// TestDisabled_503BeforeAuth pins technical plan §43.11 / D9: the
// disabled gate answers 503 BEFORE the auth gate ever runs (no cookie,
// flag off -> 503, never 401); flag on, no cookie -> the ordinary 401.
func TestDisabled_503BeforeAuth(t *testing.T) {
	t.Run("disabled, unauthenticated -> 503 with the capability body", func(t *testing.T) {
		handler := newTestHandler(t, false, false, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, nil)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", status)
		}
		if string(body) != disabledBody {
			t.Errorf("body = %s, want %s", body, disabledBody)
		}
	})

	t.Run("enabled, unauthenticated -> 401 unauthorized", func(t *testing.T) {
		handler := newTestHandler(t, true, false, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if string(body) != `{"error":"unauthorized"}` {
			t.Errorf("body = %s, want the generic unauthorized body", body)
		}
	})
}

// TestOrigin_CrossSiteRefused_TrustedAndAbsentPass pins technical plan
// §43.2: a cross-site browser Origin is refused 403; the trusted
// origin (PublicBaseURL's own origin, "http://example.test" -- see
// newTestHandler) passes; no Origin header at all (every non-browser
// client) passes.
func TestOrigin_CrossSiteRefused_TrustedAndAbsentPass(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())

	t.Run("cross-site Origin refused 403", func(t *testing.T) {
		status, _ := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": "https://evil.example"})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("trusted origin passes", func(t *testing.T) {
		status, _ := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": "http://example.test"})
		if status == http.StatusForbidden {
			t.Fatalf("status = %d, trusted origin was refused", status)
		}
	})

	t.Run("no Origin header passes", func(t *testing.T) {
		status, _ := rawPost(t, handler, "/mcp", `{}`, nil)
		if status == http.StatusForbidden {
			t.Fatalf("status = %d, absent Origin was refused", status)
		}
	})
}

// TestOrigin_DNSRebindingRefused pins the fix for round 2 review finding
// N12: net/http's own CrossOriginProtection.Handler exempts a request
// whose Origin equals its own Host header -- checked BEFORE its own
// trusted-origin list is ever consulted -- and exempts
// Sec-Fetch-Site:"same-origin"/"none" outright. A DNS-rebinding request
// (an attacker-controlled hostname resolved to this deployment's own IP)
// has EXACTLY that shape: Host and Origin both name the attacker's own
// hostname, and a same-origin XHR/fetch from that page sends
// Sec-Fetch-Site: same-origin. A prior revision of RequireTrustedOrigin
// (protection.Handler, unmodified) answered 503/401 for such a request
// instead of the 403 handler.go's own doc comment and technical plan
// §43.2 both already claimed unconditionally. RequireTrustedOrigin now
// performs its own explicit comparison against cfg.PublicBaseURL's own
// origin, with no such exemption, so this request -- Host and Origin
// BOTH "attacker.example:1234", never PublicBaseURL's own
// "http://example.test" -- must be refused 403 regardless.
func TestOrigin_DNSRebindingRefused(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Host = "attacker.example:1234"
	req.Header.Set("Origin", "http://attacker.example:1234")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 (a DNS-rebinding-style Origin -- equal to its own Host, with Sec-Fetch-Site:same-origin -- must never be exempted)", rec.Code, rec.Body.String())
	}
}

// TestOrigin_NearMissesRefused is a table test against the REAL
// RequireTrustedOrigin (round 3 review of PR #324, finding R4): every
// existing test in this file that asserts a 403 sends an Origin whose
// entire HOST differs from the trusted origin ("http://example.test" --
// testPublicBaseURL), so a comparison weakened along a single axis --
// ignore the port, ignore the scheme, accept a subdomain, accept "null",
// or match only a PREFIX of the trusted origin string -- would still pass
// every one of them. This table instead varies exactly ONE dimension per
// case, so a comparison weakened along any single axis is caught.
//
// Run through BOTH the disabled and the unauthenticated handler
// configuration, never enabled+authenticated: those two later gates
// (RequireEnabled, auth.Middleware) answer their OWN status (503/401) for
// the trusted origin, so RequireTrustedOrigin's own result -- 403 or not
// -- is never masked by NewHandler's own separate, inner
// CrossOriginProtection layer, which only ever runs once a request has
// already cleared enabled+authenticated (see
// TestNewHandler_InnerCrossOriginProtection_RefusesCrossSite's own doc
// comment).
func TestOrigin_NearMissesRefused(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		refused bool
	}{
		{"exact trusted origin", "http://example.test", false},
		{"different port", "http://example.test:8080", true},
		{"different scheme", "https://example.test", true},
		{"same-site subdomain", "http://sub.example.test", true},
		{"parent domain", "http://test", true},
		{"literal null", "null", true},
		{"prefix-extended host (trusted origin string + suffix)", "http://example.test.evil.com", true},
		{"trailing slash (still no path in the ORIGIN itself)", "http://example.test/", false},
		{"uppercase scheme and host", "HTTP://EXAMPLE.TEST", false},
		{"IPv6 literal, unrelated host", "http://[::1]:8080", true},
		{"explicit default port (:80) equals implicit", "http://example.test:80", false},
		{"explicit non-default port (:443)", "http://example.test:443", true},
	}

	for _, gate := range []struct {
		name          string
		enabled, auth bool
	}{
		{"disabled", false, true},
		{"unauthenticated", true, false},
	} {
		t.Run(gate.name, func(t *testing.T) {
			handler := newTestHandler(t, gate.enabled, gate.auth, testTwins())
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					status, body := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": tc.origin})
					if tc.refused {
						if status != http.StatusForbidden {
							t.Fatalf("Origin %q: status = %d, body = %s, want 403 (refused)", tc.origin, status, body)
						}
						return
					}
					if status == http.StatusForbidden {
						t.Fatalf("Origin %q: status = %d, body = %s, want NOT 403 (this origin must pass RequireTrustedOrigin)", tc.origin, status, body)
					}
				})
			}
		})
	}
}

// TestOrigin_HTTPSDefaultPortAndIPv6TrustedOrigin extends
// TestOrigin_NearMissesRefused's own table along two dimensions it never
// covers (round 4 review of PR #324, finding S8): every case in that
// table -- and every other unit test in this package -- trusts
// testPublicBaseURL ("http://example.test"), so canonicalOrigin's own
// https default-port branch (handler.go's defaultPortFor("https") ==
// "443") and its IPv6 re-bracketing were never exercised against a real
// RequireTrustedOrigin/NewHandler pair built from a base URL that
// actually needs either one. Mutation-verified against modified copies of
// handler.go: making defaultPortFor return "80" for "https" flips BOTH
// https cases below (accepts :80, refuses :443); deleting the IPv6
// re-bracketing (`if strings.Contains(host, ":") { host = "[" + host +
// "]" }`) makes NewHandler itself fail to construct for the IPv6 base,
// asserted directly below.
func TestOrigin_HTTPSDefaultPortAndIPv6TrustedOrigin(t *testing.T) {
	t.Run("https trusted origin", func(t *testing.T) {
		const httpsBase = "https://narvi.example"
		cases := []struct {
			name    string
			origin  string
			refused bool
		}{
			{"exact trusted origin, no port", httpsBase, false},
			{"explicit default port (:443) equals implicit", "https://narvi.example:443", false},
			{"explicit non-default port (:80)", "https://narvi.example:80", true},
		}
		for _, gate := range []struct {
			name          string
			enabled, auth bool
		}{
			{"disabled", false, true},
			{"unauthenticated", true, false},
		} {
			t.Run(gate.name, func(t *testing.T) {
				handler := newTestHandlerWithBaseURL(t, gate.enabled, gate.auth, testTwins(), httpsBase)
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						status, body := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": tc.origin})
						if tc.refused {
							if status != http.StatusForbidden {
								t.Fatalf("Origin %q: status = %d, body = %s, want 403 (refused)", tc.origin, status, body)
							}
							return
						}
						if status == http.StatusForbidden {
							t.Fatalf("Origin %q: status = %d, body = %s, want NOT 403 (this origin must pass RequireTrustedOrigin)", tc.origin, status, body)
						}
					})
				}
			})
		}
	})

	t.Run("IPv6 trusted origin", func(t *testing.T) {
		const ipv6Base = "https://[::1]:8443"

		// NewHandler must actually SUCCEED booting against an IPv6
		// PublicBaseURL: crossOriginProtection's own AddTrustedOrigin
		// call re-parses canonicalOrigin's serialized origin, which is
		// no longer a valid authority (a bare "::1:8443", indistinguishable
		// from a host with an extra port segment) the instant the IPv6
		// literal's own brackets are dropped.
		if _, err := NewHandler(Config{PublicBaseURL: ipv6Base}, testTwins()); err != nil {
			t.Fatalf("NewHandler(Config{PublicBaseURL: %q}) = %v, want nil -- a deployment configured with an IPv6 PublicBaseURL must still boot", ipv6Base, err)
		}

		cases := []struct {
			name    string
			origin  string
			refused bool
		}{
			{"exact trusted IPv6 origin", ipv6Base, false},
			{"a DIFFERENT IPv6 host that only looks similar once brackets are lost", "https://[::1:8443]", true},
		}
		for _, gate := range []struct {
			name          string
			enabled, auth bool
		}{
			{"disabled", false, true},
			{"unauthenticated", true, false},
		} {
			t.Run(gate.name, func(t *testing.T) {
				handler := newTestHandlerWithBaseURL(t, gate.enabled, gate.auth, testTwins(), ipv6Base)
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						status, body := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": tc.origin})
						if tc.refused {
							if status != http.StatusForbidden {
								t.Fatalf("Origin %q: status = %d, body = %s, want 403 (refused)", tc.origin, status, body)
							}
							return
						}
						if status == http.StatusForbidden {
							t.Fatalf("Origin %q: status = %d, body = %s, want NOT 403 (this origin must pass RequireTrustedOrigin)", tc.origin, status, body)
						}
					})
				}
			})
		}
	})
}

// TestOrigin_AbsentOriginCrossSiteFetchMetadataRefused pins the other
// half of finding R4: RequireTrustedOrigin's own doc comment (and
// technical plan §43.2) claim "a request with no Origin header at all
// (every non-browser MCP client) passes" -- but a BROWSER making a
// cross-site request always sets its own Sec-Fetch-Site fetch-metadata
// header, a value script cannot forge, so a request naming
// "cross-site" there despite carrying no Origin is a browser under attack
// conditions this deployment has no legitimate reason to accept, and must
// be refused exactly like a bad Origin would be. A request with NEITHER
// header (the ordinary non-browser MCP client this surface targets) still
// passes.
//
// Round 4 review of PR #324, finding S6: a prior revision of this test
// built its handler with newTestHandler(t, true, true, ...) (enabled AND
// authenticated) -- the ONE gate state in which a request that clears
// RequireTrustedOrigin goes on to reach NewHandler's own SEPARATE, inner
// net/http CrossOriginProtection layer (handler.go's own doc comment),
// which refuses a cross-site Sec-Fetch-Site request entirely on its own,
// with its own 403 and its own plain-text body. Deleting
// RequireTrustedOrigin's own absent-Origin check therefore left this test
// green: the inner SDK layer answered the same 403 the test asserted,
// for a completely different reason. Mirroring
// TestOrigin_NearMissesRefused's own "disabled or unauthenticated, never
// enabled+authenticated" discipline (that test's own doc comment) drives
// the request through RequireTrustedOrigin ALONE -- the later gates
// answer 503/401 for a trusted origin instead of ever reaching the inner
// layer -- so this test can only pass because RequireTrustedOrigin
// itself, not the SDK's own masking layer, produced the 403.
func TestOrigin_AbsentOriginCrossSiteFetchMetadataRefused(t *testing.T) {
	for _, gate := range []struct {
		name          string
		enabled, auth bool
	}{
		{"disabled", false, true},
		{"unauthenticated", true, false},
	} {
		t.Run(gate.name, func(t *testing.T) {
			handler := newTestHandler(t, gate.enabled, gate.auth, testTwins())

			t.Run("no Origin, Sec-Fetch-Site: cross-site -> refused", func(t *testing.T) {
				status, body := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"})
				if status != http.StatusForbidden {
					t.Fatalf("status = %d, body = %s, want 403", status, body)
				}
				if string(body) != originForbiddenBody {
					t.Fatalf("body = %s, want RequireTrustedOrigin's own %s -- a different body means some OTHER layer answered 403", body, originForbiddenBody)
				}
			})

			t.Run("no Origin, no Sec-Fetch-Site -> passes (non-browser client)", func(t *testing.T) {
				status, body := rawPost(t, handler, "/mcp", `{}`, nil)
				if status == http.StatusForbidden {
					t.Fatalf("status = %d, body = %s, want NOT 403", status, body)
				}
			})
		})
	}
}

// TestNewHandler_InnerCrossOriginProtection_RefusesCrossSite pins the fix
// for round 2 review finding N19: NewHandler's OWN doc comment calls its
// returned handler "Origin-protected" as a second, defense-in-depth
// layer (StreamableHTTPOptions.CrossOriginProtection), independent of
// RequireTrustedOrigin mounted in front of it in every other test in this
// file. None of those other tests can tell that inner layer apart from
// "no protection at all", because RequireTrustedOrigin's own 403 always
// fires first. This test mounts NewHandler's returned handler DIRECTLY,
// deliberately WITHOUT RequireTrustedOrigin in front (mirroring a rig
// that only wires RequireEnabled + auth, which is exactly what this
// package's own mcp_test parity rig, integration_test.go's
// newMCPTestRig, used to do before this fix), so a cross-site Origin
// reaching this handler is refused ONLY if the inner layer itself is
// still there. Setting NewHandler's own StreamableHTTPOptions.
// CrossOriginProtection to nil (go-sdk v1.8.0: nil means "no protection
// applied") would make this exact request succeed instead, failing this
// test.
func TestNewHandler_InnerCrossOriginProtection_RefusesCrossSite(t *testing.T) {
	cfg := Config{PublicBaseURL: testPublicBaseURL}
	mcpHandler, err := NewHandler(cfg, testTwins())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	handler := RequireEnabled(true)(fakeAuth(true, testUser)(mcpHandler))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 from NewHandler's OWN inner CrossOriginProtection layer (no RequireTrustedOrigin is mounted in front of it here)", rec.Code, rec.Body.String())
	}
}

// TestOrigin_RunsBeforeEveryOtherGate proves technical plan §43.2/§43.6's
// own gate order is load-bearing, not merely tidier: RequireTrustedOrigin
// is mounted FIRST in the /mcp route group's own chain (newTestHandler),
// so an invalid Origin gets refused 403 REGARDLESS of the flag, auth, or
// protocol-version state of the rest of the request -- the Streamable
// HTTP transport spec's own "MUST respond with HTTP 403 Forbidden" for
// an invalid Origin is unconditional. A prior revision of this package
// left the equivalent check to run LAST, deep inside NewHandler's own
// returned handler (behind RequireEnabled and auth.Middleware in
// controlplane/serve.go's own route group), so each of the three cases
// below used to answer 503/401/-32022 INSTEAD of 403 whenever the
// request also failed that later gate.
func TestOrigin_RunsBeforeEveryOtherGate(t *testing.T) {
	badOrigin := map[string]string{"Origin": "https://evil.example"}

	t.Run("disabled + bad Origin -> still 403, not 503", func(t *testing.T) {
		handler := newTestHandler(t, false, true, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, badOrigin)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the disabled gate must never run first)", status, body)
		}
	})

	t.Run("unauthenticated + bad Origin -> still 403, not 401", func(t *testing.T) {
		handler := newTestHandler(t, true, false, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, badOrigin)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the auth gate must never run first)", status, body)
		}
	})

	t.Run("unsupported protocol version + bad Origin -> still 403, not -32022", func(t *testing.T) {
		handler := newTestHandler(t, true, true, testTwins())
		headers := map[string]string{"Origin": "https://evil.example", protocolVersionHeader: "1900-01-01"}
		status, body := rawPost(t, handler, "/mcp", `{}`, headers)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the version gate must never run first)", status, body)
		}
	})
}

// TestUnsupportedVersion_Is32022ThroughRealHandler pins technical plan
// §43.4 through the REAL NewHandler, not a stub: every TestVersionGate_*
// case in versions_test.go builds versionGate(passthroughHandler(...))
// directly, around a plain 418-teapot sentinel, never through NewHandler
// or newTestHandler -- so a regression that stopped wiring versionGate
// in front of the real SDK handler at all (e.g. handler.go's own `return
// versionGate(sdkHandler), nil` collapsing to `return sdkHandler, nil`)
// would leave every one of those tests green while every REAL request
// through this surface silently lost the gate.
func TestUnsupportedVersion_Is32022ThroughRealHandler(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())

	status, body := rawPost(t, handler, "/mcp",
		`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
		map[string]string{protocolVersionHeader: "1900-01-01"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil {
		t.Fatalf("error = nil, want a JSON-RPC -32022 protocol error (body: %s)", body)
	}
	if env.Error.Code != -32022 {
		t.Errorf("error.code = %d, want -32022", env.Error.Code)
	}
	for _, v := range SupportedProtocolVersions {
		if !strings.Contains(env.Error.Message, v) {
			t.Errorf("error.message = %q does not name version %q", env.Error.Message, v)
		}
	}
}

// TestMethodNotAllowed_GetAndDelete pins technical plan §43.3:
// Streamable HTTP in stateless mode is POST-only.
func TestMethodNotAllowed_GetAndDelete(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/mcp", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
		})
	}
}

// TestMaxRequestBodyBytes_LargerBodyRefused pins technical plan §43.3: a
// request body larger than MaxRequestBodyBytes (round 2 review of PR
// #324, finding N2: deliberately shrunk to 64 KiB, far below
// httpapi.MaxRequestBodyBytes's own 1 MiB -- a tool call's own arguments
// are a handful of small fields, never a file upload) is refused (413),
// not silently accepted at the SDK's own larger DefaultMaxRequestBodyBytes
// (4 MiB) -- which is exactly what removing
// `MaxRequestBodyBytes: MaxRequestBodyBytes` from handler.go's own
// StreamableHTTPOptions would fall back to.
func TestMaxRequestBodyBytes_LargerBodyRefused(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	headers := map[string]string{protocolVersionHeader: "2026-07-28", "Mcp-Method": "tools/list"}

	// Comfortably under the cap: accepted.
	t.Run("under the cap is accepted", func(t *testing.T) {
		padding := strings.Repeat("x", 1024)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"padding":%q}}}`, padding)
		status, respBody := rawPost(t, handler, "/mcp", body, headers)
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %s, want 200", status, respBody)
		}
	})

	// Comfortably over the cap: refused before the SDK ever parses it as
	// JSON.
	t.Run("over the cap is refused 413", func(t *testing.T) {
		padding := strings.Repeat("x", 2*MaxRequestBodyBytes)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"padding":%q}}}`, padding)
		if len(body) <= MaxRequestBodyBytes {
			t.Fatalf("test body is %d bytes, want more than MaxRequestBodyBytes (%d)", len(body), MaxRequestBodyBytes)
		}
		status, respBody := rawPost(t, handler, "/mcp", body, headers)
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, body = %s, want 413", status, respBody)
		}
	})
}

// legacyInitializeBody builds a pre-2025-06-18-shaped `initialize` call
// naming protocolVersion -- no MCP-Protocol-Version header, no `_meta`
// triple: exactly the shape a legacy client sends before it knows what
// the server speaks.
func legacyInitializeBody(id int, protocolVersion string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"initialize","params":{"protocolVersion":"` + protocolVersion + `","capabilities":{},"clientInfo":{"name":"test-client","version":"0"}}}`
}

// TestLegacyInitialize_UnsupportedVersionIsCounterOffered pins technical
// plan §43.4 case 2: a legacy `initialize` naming an unsupported
// version (2024-11-05) is answered with a version the server DOES speak
// (never the requested one) -- a counter-offer, not an error.
func TestLegacyInitialize_UnsupportedVersionIsCounterOffered(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	status, body := rawPost(t, handler, "/mcp", legacyInitializeBody(1, "2024-11-05"), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error != nil {
		t.Fatalf("error = %+v, want a successful counter-offer result", env.Error)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v (result: %s)", err, env.Result)
	}
	if result.ProtocolVersion == "2024-11-05" {
		t.Errorf("result.protocolVersion = %q, must never echo the unsupported requested version", result.ProtocolVersion)
	}
	found := false
	for _, v := range SupportedProtocolVersions {
		if v == result.ProtocolVersion {
			found = true
		}
	}
	if !found {
		t.Errorf("result.protocolVersion = %q, want one of %v", result.ProtocolVersion, SupportedProtocolVersions)
	}
}

// TestLegacyInitialize_SupportedVersionIsEchoed pins technical plan
// §43.4 case 2's own positive case, and (via the follow-up call) §43.12
// test 4: a following tools/list with the negotiated MCP-Protocol-Version
// header works, with no Mcp-Session-Id needed at all (stateless).
func TestLegacyInitialize_SupportedVersionIsEchoed(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	status, body := rawPost(t, handler, "/mcp", legacyInitializeBody(1, "2025-06-18"), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v (result: %s)", err, env.Result)
	}
	if result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("result.protocolVersion = %q, want the exact requested (and supported) version echoed back", result.ProtocolVersion)
	}

	status, body = rawPost(t, handler, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		map[string]string{protocolVersionHeader: "2025-06-18"})
	if status != http.StatusOK {
		t.Fatalf("follow-up tools/list: status = %d, body = %s, want 200", status, body)
	}
	var env2 jsonrpcEnvelope
	if err := json.Unmarshal(body, &env2); err != nil {
		t.Fatalf("unmarshal follow-up: %v (body: %s)", err, body)
	}
	if env2.Error != nil {
		t.Fatalf("follow-up tools/list error = %+v, want success", env2.Error)
	}
}

// TestHeaderBodyMismatch_Is32020 pins the SDK's own header/body agreement
// check for a modern (>= 2026-07-28) request: Mcp-Method must match the
// JSON-RPC method.
func TestHeaderBodyMismatch_Is32020(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, respBody := rawPost(t, handler, "/mcp", body, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "tools/call", // deliberately WRONG
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, respBody)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, respBody)
	}
	if env.Error == nil {
		t.Fatal("error = nil, want a header-mismatch error")
	}
	if env.Error.Code != -32020 {
		t.Errorf("error.code = %d, want -32020", env.Error.Code)
	}
}

// TestMissingMeta_Is32602 pins the SDK's own required-`_meta`-field check
// for a modern request: clientCapabilities is required alongside
// protocolVersion.
func TestMissingMeta_Is32602(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	status, respBody := rawPost(t, handler, "/mcp", body, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "tools/list",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, respBody)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, respBody)
	}
	if env.Error == nil {
		t.Fatal("error = nil, want a missing-_meta error")
	}
	if env.Error.Code != -32602 {
		t.Errorf("error.code = %d, want -32602", env.Error.Code)
	}
}

// TestServerDiscover_AdvertisesConstant pins technical plan §43.5:
// server/discover's own supportedVersions is EXACTLY
// SupportedProtocolVersions, and _meta carries this build's own
// serverInfo.
func TestServerDiscover_AdvertisesConstant(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, respBody := rawPost(t, handler, "/mcp", body, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "server/discover",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, respBody)
	}
	var env struct {
		Result struct {
			SupportedVersions []string       `json:"supportedVersions"`
			Capabilities      map[string]any `json:"capabilities"`
			Instructions      string         `json:"instructions"`
			Meta              map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, respBody)
	}
	if len(env.Result.SupportedVersions) != len(SupportedProtocolVersions) {
		t.Fatalf("supportedVersions = %v, want %v", env.Result.SupportedVersions, SupportedProtocolVersions)
	}
	for i, v := range SupportedProtocolVersions {
		if env.Result.SupportedVersions[i] != v {
			t.Errorf("supportedVersions[%d] = %q, want %q", i, env.Result.SupportedVersions[i], v)
		}
	}
	// Assert the EXACT capabilities map, not merely "has tools, lacks
	// resources": a prior version of this check let "tools" be ANY
	// shape (e.g. {"listChanged":true}, the SDK's own default when
	// serverOptions() forgets to set Capabilities explicitly) and never
	// looked at "logging"/"prompts" at all, so dropping
	// `Capabilities: &sdkmcp.ServerCapabilities{...}` from serverOptions()
	// entirely (handler.go) -- which falls back to the SDK's own
	// default, {"logging":{},"tools":{"listChanged":true}} -- passed this
	// test undetected.
	wantCapabilities := map[string]any{"tools": map[string]any{}}
	if !reflect.DeepEqual(env.Result.Capabilities, wantCapabilities) {
		t.Errorf("capabilities = %#v, want EXACTLY %#v (no listChanged, no logging, no resources, no prompts)", env.Result.Capabilities, wantCapabilities)
	}
	if !strings.Contains(strings.ToLower(env.Result.Instructions), "read-only") {
		t.Errorf("instructions = %q, want it to say the tools are read-only", env.Result.Instructions)
	}
	serverInfo, _ := env.Result.Meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if serverInfo["name"] != "narvi" {
		t.Errorf("result._meta.serverInfo.name = %v, want \"narvi\"", serverInfo["name"])
	}
	if serverInfo["version"] == "" || serverInfo["version"] == nil {
		t.Errorf("result._meta.serverInfo.version = %v, want contracts.Version", serverInfo["version"])
	}
}

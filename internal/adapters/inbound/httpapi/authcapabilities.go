// This file (authcapabilities.go) implements GET /auth/capabilities
// (technical plan §41.3, "OIDC as a second sign-in provider"): the ONE
// public, UNAUTHENTICATED signal the sign-in view (web/src/routes/
// sign-in.tsx) needs before a visitor is signed in at all -- whether this
// deployment has a generic OIDC SSO provider configured, so its SSO
// button can be a real link to GET /auth/oidc/login instead of a
// permanently disabled one.
//
// Deliberately its OWN route and its OWN response shape
// (restdtos.AuthCapabilitiesResponse), never a branch grafted onto GET
// /api/capabilities (capabilities.go, immediately alongside this file):
// that is a DIFFERENT read model entirely (technical plan §34's
// licensed-module capabilities -- organization_governance/compliance/
// knowledge_retrieval), mounted behind auth.Middleware and therefore
// structurally unusable by a signed-out visitor -- the sign-in view's own
// one caller for THIS handler.
package httpapi

import (
	"net/http"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
)

// GetAuthCapabilities backs GET /auth/capabilities: unconditional 200 for
// every caller, signed in or not -- there is nothing here to authorize,
// only a fact about how this deployment is configured.
//
// oidcConfigured is threaded in as a plain bool, computed once at Mount
// time from platform.Config.OIDCIssuer (controlplane/serve.go's own
// oidcConfig.Configured()) -- boot-time config, not a value that can
// change without a restart, so (unlike GetCapabilities' own build func,
// re-evaluated per request against a licence that CAN change live) a
// closed-over constant is the honest shape here.
func GetAuthCapabilities(oidcConfigured bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, restdtos.AuthCapabilitiesResponse{
			OidcConfigured: oidcConfigured,
		})
	}
}

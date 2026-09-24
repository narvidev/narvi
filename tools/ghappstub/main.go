// Command ghappstub is a tiny, local stand-in for GitHub's own REST API,
// built for exactly one caller: Makefile's own verify-control-plane-image
// target (§41.1's exit-criterion proof, docs/TECHNICAL_PLAN.md). It answers
// GET /app with the read-only permission set
// internal/domain/scmscope.ValidateReadOnly accepts
// ({"contents":"read","metadata":"read"}), so the packaged image's own
// real, UNMODIFIED verifyGitHubAppScopeAtBoot check (controlplane/boot.go)
// can run against it and pass at boot -- proving the image actually opens
// its listener and serves /health, without either reaching the real
// api.github.com (unreachable, and undesirable as a CI dependency even
// when reachable) or weakening the boot-time scope check itself for any
// stage. internal/platform.Config.GitHubAPIBaseURL
// (NARVI_GITHUB_API_BASE_URL) is what lets the verify target point the
// real githubapp.Client at this stub instead of the hard-coded
// api.github.com literal that used to make this proof impossible (§41.1
// review round 1, finding P1).
//
// Listens on 0.0.0.0:<port>, never 127.0.0.1: the verify target reaches
// this process from inside a container via
// --add-host=host.docker.internal:host-gateway, which resolves to an
// address that only a wildcard-bound listener answers on -- on both
// Linux's docker0 bridge gateway and Docker Desktop's own gateway alike.
// port defaults to 18081, overridable via the PORT env var.
//
// Any request other than GET /app answers 404 -- this stub exists to
// answer exactly the one call verifyGitHubAppScopeAtBoot makes at boot,
// nothing else (in particular, it does NOT implement installation-token
// minting: the verify target's own proof only exercises the boot-time
// scope check, never a real session/mint flow).
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

// readOnlyAppPermissions is the exact shape
// internal/domain/scmscope.ValidateReadOnly accepts: every entry must be
// "read", and the map must be non-empty (an empty map is itself refused,
// scmscope.EmptyPermissionsError) -- mirrors
// internal/adapters/outbound/githubapp.Client's own
// readOnlyMintPermissions precedent (the two permissions §30.4's mint
// request actually asks for), reused here as the two this stub grants.
var readOnlyAppPermissions = map[string]string{
	"contents": "read",
	"metadata": "read",
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "18081"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"permissions": readOnlyAppPermissions,
		}); err != nil {
			log.Printf("ghappstub: encode /app response: %v", err)
		}
	})

	addr := "0.0.0.0:" + port
	log.Printf("ghappstub: listening on %s (GET /app only)", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

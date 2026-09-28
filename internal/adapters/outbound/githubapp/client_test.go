package githubapp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
)

// testPrivateKey is generated once per test process -- there is no real
// GitHub App reachable from this environment (this package's own doc.go
// says so plainly), so every test here exercises Client against a fake
// httptest.Server standing in for api.github.com, never the real GitHub
// API.
func testPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test RSA key: %v", err)
	}
	return key
}

// bearerAuth extracts the raw JWT from an "Authorization: Bearer <jwt>"
// header, failing the test if the header is missing/malformed -- every
// fake handler below uses this to prove a real, well-formed Bearer JWT
// was actually sent, without re-verifying its signature (a handler proving
// its own request shape is not the same claim as this package's own
// jwt_test.go, which verifies the signature itself).
func bearerAuth(t *testing.T, r *http.Request) string {
	t.Helper()
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		t.Fatalf("Authorization header = %q, want a Bearer token", auth)
	}
	return strings.TrimPrefix(auth, prefix)
}

func TestClient_AppPermissions(t *testing.T) {
	t.Run("success decodes permissions", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearerAuth(t, r)
			if r.Method != http.MethodGet || r.URL.Path != "/app" {
				t.Fatalf("request = %s %s, want GET /app", r.Method, r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          42,
				"permissions": map[string]string{"contents": "read", "metadata": "read"},
			})
		}))
		defer server.Close()

		client := githubapp.New(server.Client(), server.URL, 42, testPrivateKey(t), time.Minute, 60*time.Second)
		perms, err := client.AppPermissions(context.Background())
		if err != nil {
			t.Fatalf("AppPermissions() error = %v, want nil", err)
		}
		want := map[string]string{"contents": "read", "metadata": "read"}
		if len(perms) != len(want) || perms["contents"] != "read" || perms["metadata"] != "read" {
			t.Errorf("AppPermissions() = %v, want %v", perms, want)
		}
	})

	t.Run("non-2xx is a plain error naming no body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials","secret_leak":"should never surface"}`))
		}))
		defer server.Close()

		client := githubapp.New(server.Client(), server.URL, 42, testPrivateKey(t), time.Minute, 60*time.Second)
		_, err := client.AppPermissions(context.Background())
		if err == nil {
			t.Fatal("AppPermissions() error = nil, want an error on http 401")
		}
		if strings.Contains(err.Error(), "secret_leak") || strings.Contains(err.Error(), "Bad credentials") {
			t.Errorf("AppPermissions() error = %q, must never embed the response body", err.Error())
		}
	})
}

func TestClient_MintInstallationToken(t *testing.T) {
	t.Run("success resolves installation then mints a scoped token", func(t *testing.T) {
		var sawInstallationLookup, sawMintRequest bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearerAuth(t, r)
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/installation":
				sawInstallationLookup = true
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 777})
			case r.Method == http.MethodPost && r.URL.Path == "/app/installations/777/access_tokens":
				sawMintRequest = true
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode mint request body: %v", err)
				}
				perms, _ := body["permissions"].(map[string]any)
				if perms["contents"] != "read" || perms["metadata"] != "read" {
					t.Errorf("mint request permissions = %v, want exactly contents:read + metadata:read", perms)
				}
				// Assert the SET, not a sample of it. Naming one
				// forbidden key ("administration") reads like a scope
				// check and is not one: it leaves pull_requests:write,
				// workflows:write and every other permission GitHub
				// offers free to appear here and be minted.
				for name, level := range perms {
					if name != "contents" && name != "metadata" {
						t.Errorf("mint request asks for permission %q=%v; the request must carry contents and metadata and nothing else", name, level)
					}
					if level != "read" {
						t.Errorf("mint request asks for %q at level %v; every requested level must be read", name, level)
					}
				}
				repos, _ := body["repositories"].([]any)
				if len(repos) != 1 || repos[0] != "widgets" {
					t.Errorf("mint request repositories = %v, want [widgets]", repos)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"token":       "ghs_fake_token_never_real",
					"expires_at":  time.Now().Add(time.Hour).Format(time.RFC3339),
					"permissions": map[string]string{"contents": "read", "metadata": "read"},
				})
			default:
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}))
		defer server.Close()

		client := githubapp.New(server.Client(), server.URL, 42, testPrivateKey(t), time.Minute, 60*time.Second)
		token, err := client.MintInstallationToken(context.Background(), "acme", []string{"widgets"})
		if err != nil {
			t.Fatalf("MintInstallationToken() error = %v, want nil", err)
		}
		if !sawInstallationLookup {
			t.Error("MintInstallationToken() never resolved the repo's own installation id")
		}
		if !sawMintRequest {
			t.Error("MintInstallationToken() never minted an access token against the resolved installation")
		}
		if token.Value != "ghs_fake_token_never_real" {
			t.Errorf("MintInstallationToken().Value = %q, want the fake server's own token", token.Value)
		}
		if token.Permissions["contents"] != "read" || token.Permissions["metadata"] != "read" {
			t.Errorf("MintInstallationToken().Permissions = %v, want contents:read + metadata:read", token.Permissions)
		}
	})

	t.Run("no repo names is a local error, no request sent", func(t *testing.T) {
		requested := false
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			requested = true
		}))
		defer server.Close()

		client := githubapp.New(server.Client(), server.URL, 42, testPrivateKey(t), time.Minute, 60*time.Second)
		_, err := client.MintInstallationToken(context.Background(), "acme", nil)
		if err == nil {
			t.Fatal("MintInstallationToken() error = nil, want an error for zero repo names")
		}
		if requested {
			t.Error("MintInstallationToken() made an HTTP request despite having no repo names to mint against")
		}
	})

	t.Run("installation lookup failure surfaces, mint never attempted", func(t *testing.T) {
		mintCalled := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "access_tokens") {
				mintCalled = true
			}
			// A well-formed JSON body on the 404, carrying a NON-ZERO id --
			// this isolates the STATUS CODE check itself (doAppRequest's own
			// status-range guard). A plain-text 404 body, or a JSON body
			// with id 0, would still produce SOME error even with the
			// status check removed (a decode failure, or the id==0 guard
			// below) -- masking a status-check regression behind an
			// unrelated failure instead of actually pinning this line.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 777})
		}))
		defer server.Close()

		client := githubapp.New(server.Client(), server.URL, 42, testPrivateKey(t), time.Minute, 60*time.Second)
		_, err := client.MintInstallationToken(context.Background(), "acme", []string{"widgets"})
		if err == nil {
			t.Fatal("MintInstallationToken() error = nil, want an error when installation lookup 404s")
		}
		if mintCalled {
			t.Error("MintInstallationToken() attempted to mint a token despite a failed installation lookup")
		}
		var notInstalled *githubapp.InstallationNotFoundError
		if !errors.As(err, &notInstalled) || notInstalled.Owner != "acme" || notInstalled.Repo != "widgets" {
			t.Errorf("MintInstallationToken() error = %v, want *InstallationNotFoundError naming acme/widgets: a 404 on the installation lookup means the App is not installed there", err)
		}
	})

	// Only a 404 on the installation lookup means "not installed". Any
	// other failure -- another status on the lookup, or a 404 on the mint
	// itself -- stays an ordinary error.
	t.Run("only a 404 on the installation lookup is a missing installation", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			lookupStatus int
			mintStatus   int
		}{
			{name: "lookup 500", lookupStatus: http.StatusInternalServerError, mintStatus: http.StatusCreated},
			{name: "lookup 403", lookupStatus: http.StatusForbidden, mintStatus: http.StatusCreated},
			{name: "lookup ok, mint 404", lookupStatus: http.StatusOK, mintStatus: http.StatusNotFound},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "access_tokens") {
						w.WriteHeader(tc.mintStatus)
						return
					}
					w.WriteHeader(tc.lookupStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 777})
				}))
				defer server.Close()

				client := githubapp.New(server.Client(), server.URL, 42, testPrivateKey(t), time.Minute, 60*time.Second)
				_, err := client.MintInstallationToken(context.Background(), "acme", []string{"widgets"})
				if err == nil {
					t.Fatal("MintInstallationToken() error = nil, want an error")
				}
				var notInstalled *githubapp.InstallationNotFoundError
				if errors.As(err, &notInstalled) {
					t.Errorf("MintInstallationToken() error = %v, typed as a missing installation; want an ordinary failure", err)
				}
			})
		}
	})
}

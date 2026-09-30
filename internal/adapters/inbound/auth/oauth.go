package auth

import (
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"github.com/narvidev/narvi/internal/platform"
)

// oauthCallbackPath is the fixed path NewGitHubOAuthConfig appends to
// cfg.PublicBaseURL to build the OAuth RedirectURL -- kept as a named
// constant so this constructed URL and cmd/control-plane/main.go's own
// route registration (GET /auth/github/callback) can never drift apart.
const oauthCallbackPath = "/auth/github/callback"

// NewGitHubOAuthConfig builds the golang.org/x/oauth2.Config wired to
// GitHub's own OAuth endpoints (golang.org/x/oauth2/github.Endpoint --
// AuthURL/TokenURL already correctly set for GitHub). Scopes requested:
//
//   - user:email -- the verified-primary-email allowlist check (§13.1).
//   - read:org   -- the GitHub-org-membership allowlist check (§13.1).
//   - repo       -- NEVER used by this Step. GitHub OAuth cannot
//     incrementally add scopes to an already-issued token without the
//     user re-authorizing, and §8.11/§13.1's own stated purpose for
//     storing this token at all is §9.3's future PR-creation need
//     (the PR created with the session creator's OAuth token) -- requesting
//     it now avoids forcing every already-signed-up user through a
//     second re-authorization later just to grant a scope that was
//     always the point of storing their token in the first place.
func NewGitHubOAuthConfig(cfg platform.Config) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		Endpoint:     github.Endpoint,
		RedirectURL:  cfg.PublicBaseURL + oauthCallbackPath,
		Scopes:       []string{"user:email", "read:org", "repo"},
	}
}

//go:build integration

// Integration tests proving batch fix/audit-github-actor-rbac's own H4 fix
// actually fires from a REAL POST /webhooks/github request: a disabled or
// viewer-role commenter is denied (no session/turn created), and a
// maintainer commenter is allowed -- exactly mirroring internal/adapters/
// inbound/{slack,linear}'s own identical pre-existing coverage
// (identity_integration_test.go in each package) for the SAME domain/authz.
// Authorize gate, now shared via internal/app/actorauthz. Reuses
// newTestPool/testWebhookSecret/testBotHandleIntegration/sign/
// issueCommentBody from handler_integration_test.go (same package, same
// build tag).
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// identityTestRig mirrors testRig (handler_integration_test.go) but also
// exposes the stores this file's own fixtures/assertions need directly
// (users/identities), and wires the coalescer's new Identities/Users/
// Participants fields -- unlike newTestRig, which leaves all three nil
// (safe there only because those tests' own synthetic payloads never
// carry a comment.user, so the resolved actor always stays invalid and
// those fields are never dereferenced).
type identityTestRig struct {
	pool       *pgxpool.Pool
	sessions   *narvipg.SessionStore
	turns      *narvipg.TurnStore
	identities *narvipg.IdentityStore
	users      *narvipg.UserStore
	server     *httptest.Server
}

func newIdentityTestRig(t *testing.T) identityTestRig {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	rig := identityTestRig{
		pool:       pool,
		sessions:   narvipg.NewSessionStore(pool),
		turns:      narvipg.NewTurnStore(pool),
		identities: narvipg.NewIdentityStore(pool),
		users:      narvipg.NewUserStore(pool),
	}

	coalescer := &githubingress.SessionCoalescer{
		Pool:         pool,
		PRSessions:   narvipg.NewGitHubPRSessionStore(pool),
		Sessions:     rig.sessions,
		Turns:        rig.turns,
		Environments: narvipg.NewEnvironmentStore(pool),
		Registry:     registry,
		AuditLog:     narvipg.NewAuditLogStore(pool),
		Identities:   rig.identities,
		Users:        rig.users,
		Participants: narvipg.NewParticipantStore(pool),
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	handler := githubingress.NewHandler(coalescer, deliveries, githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
	})

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)

	return rig
}

// post sends body to rig's own real handler with a valid signature,
// mirroring postWebhook (handler_integration_test.go) -- duplicated here
// (rather than reused) only because it takes identityTestRig, a distinct
// struct type from that file's own testRig, matching this package's own
// established "small, documented duplication over a forced shared
// abstraction" precedent (doc.go's own hasOpenTurn-shaped note elsewhere
// in this codebase).
func (rig identityTestRig) post(t *testing.T, body []byte, deliveryID string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/webhooks/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Hub-Signature-256", sign([]byte(testWebhookSecret), body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", deliveryID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// createLinkedUser creates a Narvi user with role, and a matching "github"
// identities row for commenterID -- the direct (provider, external_id)
// link a real GitHub OAuth sign-in would have already produced (§13.1),
// which is exactly what resolveCommenterActor (identity.go) looks up.
func (rig identityTestRig) createLinkedUser(t *testing.T, commenterID int64, role sqlcgen.UserRole) sqlcgen.User {
	t.Helper()
	ctx := context.Background()

	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("github-commenter-%d@example.com", commenterID),
		DisplayName:  "GitHub Commenter",
		Role:         role,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	email := user.PrimaryEmail
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        user.ID,
		Provider:      sqlcgen.IdentityProviderGithub,
		ExternalID:    strconv.FormatInt(commenterID, 10),
		Email:         &email,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAutoEmail,
	}); err != nil {
		t.Fatalf("create fixture github identity: %v", err)
	}

	return user
}

// issueCommentBodyWithCommenter mirrors issueCommentBody
// (handler_integration_test.go) but also sets comment.user.{id,login} --
// the field this batch's own payload.go parsing newly reads. ALSO sets the
// top-level "sender" field to the SAME commenter (D2 audit fix's own
// automation-dispatch actor-authorization input, githubEventSenderID,
// automationdispatch.go) -- a real GitHub `issue_comment`/"created"
// delivery's own top-level sender IS the comment's author, exactly like
// this mirrors.
func issueCommentBodyWithCommenter(repoFullName, repoName, cloneURL string, prNumber int, label string, commenterID int64, commenterLogin string) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "created",
		"sender": map[string]any{"id": commenterID, "login": commenterLogin},
		"issue": map[string]any{
			"number":       prNumber,
			"pull_request": map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repoFullName, prNumber)},
		},
		"comment": map[string]any{
			"body": fmt.Sprintf("@%s please review (%s)", testBotHandleIntegration, label),
			"user": map[string]any{"id": commenterID, "login": commenterLogin},
		},
		"repository": map[string]any{
			"full_name": repoFullName,
			"name":      repoName,
			"clone_url": cloneURL,
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// TestGitHubIntegration_MentionDeniedForDisabledCommenter is this batch's
// own regression test for the H4 audit finding: a mention from a GitHub
// commenter who IS already linked to a Narvi account, but that account is
// disabled, must NOT create a session -- exactly like a disabled user's
// Slack/Linear mention is already denied (§13.2), and exactly like
// auth.Middleware's own Authenticate already rejects that SAME disabled
// user's web session outright.
func TestGitHubIntegration_MentionDeniedForDisabledCommenter(t *testing.T) {
	ctx := context.Background()
	rig := newIdentityTestRig(t)

	const commenterID = 90001001
	user := rig.createLinkedUser(t, commenterID, sqlcgen.UserRoleMember)
	if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("disable fixture user: %v", err)
	}

	body := issueCommentBodyWithCommenter("acme/disabled-repo", "disabled-repo", "https://github.com/acme/disabled-repo.git", 501, "disabled-deny", commenterID, "disabled-user")

	status := rig.post(t, body, "delivery-disabled-deny-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (denied, but still acknowledged)", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a disabled commenter's linked account must never create a session)", sessionCount)
	}
}

// TestGitHubIntegration_MentionDeniedForViewerCommenter proves the role
// gate itself: a linked `viewer` (never allowed ActionCreateSession,
// §13.3 row 2) is denied, exactly like a viewer's Slack/Linear mention
// already is.
func TestGitHubIntegration_MentionDeniedForViewerCommenter(t *testing.T) {
	ctx := context.Background()
	rig := newIdentityTestRig(t)

	const commenterID = 90001002
	rig.createLinkedUser(t, commenterID, sqlcgen.UserRoleViewer)

	body := issueCommentBodyWithCommenter("acme/viewer-repo", "viewer-repo", "https://github.com/acme/viewer-repo.git", 502, "viewer-deny", commenterID, "viewer-user")

	status := rig.post(t, body, "delivery-viewer-deny-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (denied, but still acknowledged)", status, http.StatusOK)
	}

	var sessionCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("session count = %d, want 0 (a viewer must never create a session, even a linked one)", sessionCount)
	}
}

// TestGitHubIntegration_MentionAllowedForMaintainerCommenter proves the
// OTHER side of the same gate: a linked `maintainer` IS allowed, and the
// resulting session is attributed to that real user (created_by = the
// commenter's own Narvi user id, not NULL/bot-attributed) -- mirroring
// Slack's resolveOrClaimSession / Linear's handleCreated passing their own
// resolved creator through to session creation identically.
func TestGitHubIntegration_MentionAllowedForMaintainerCommenter(t *testing.T) {
	ctx := context.Background()
	rig := newIdentityTestRig(t)

	const commenterID = 90001003
	user := rig.createLinkedUser(t, commenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter("acme/maintainer-repo", "maintainer-repo", "https://github.com/acme/maintainer-repo.git", 503, "maintainer-allow", commenterID, "maintainer-user")

	status := rig.post(t, body, "delivery-maintainer-allow-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var sessionID, createdByText string
	if err := rig.pool.QueryRow(ctx,
		`SELECT id::text, coalesce(created_by::text, '') FROM sessions WHERE spawn_source = 'github' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&sessionID, &createdByText); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if createdByText != user.ID.String() {
		t.Errorf("created_by = %q, want %q (the linked maintainer's own user id, not NULL/bot-attributed)", createdByText, user.ID.String())
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 1 {
		t.Errorf("turn count = %d, want 1", turnCount)
	}
}

// TestGitHubIntegration_ReplyOnExistingSessionDeniedForUnownedMember
// proves the REUSE-path gate: a linked `member` who neither created nor
// joined an already-existing PR review session is denied
// ActionPromptSession when they comment again on the SAME PR -- exactly
// mirroring Slack's/Linear's own "existing thread/session reply" gate.
func TestGitHubIntegration_ReplyOnExistingSessionDeniedForUnownedMember(t *testing.T) {
	ctx := context.Background()
	rig := newIdentityTestRig(t)

	const repoFullName = "acme/reuse-deny-repo"
	const prNumber = 504

	// First mention: a DIFFERENT already-linked maintainer, creating the
	// PR's own review session -- batch fix/deny-unlinked-github-actors
	// means an unresolved (bot-attributed) commenter is now denied outright
	// on the WINNER path too, so this first mention can no longer be
	// unlinked the way it was before that batch (it would simply never
	// create the session this test's own REUSE-path denial assertion below
	// depends on existing at all).
	const creatorCommenterID = 90001003
	rig.createLinkedUser(t, creatorCommenterID, sqlcgen.UserRoleMaintainer)

	first := issueCommentBodyWithCommenter(repoFullName, "reuse-deny-repo", "https://github.com/acme/reuse-deny-repo.git", prNumber, "first-mention", creatorCommenterID, "session-creator")
	firstStatus := rig.post(t, first, "delivery-reuse-deny-first")
	if firstStatus != http.StatusOK {
		t.Fatalf("first mention status = %d, want %d", firstStatus, http.StatusOK)
	}

	// Second mention on the SAME PR, from a linked member with no
	// ownership/participation in that now-existing session.
	const commenterID = 90001004
	rig.createLinkedUser(t, commenterID, sqlcgen.UserRoleMember)

	second := issueCommentBodyWithCommenter(repoFullName, "reuse-deny-repo", "https://github.com/acme/reuse-deny-repo.git", prNumber, "second-mention", commenterID, "unowned-member")
	secondStatus := rig.post(t, second, "delivery-reuse-deny-second")
	if secondStatus != http.StatusOK {
		t.Fatalf("second mention status = %d, want %d (denied, but still acknowledged)", secondStatus, http.StatusOK)
	}

	var sessionCount, turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%reuse-deny-repo%'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("session count = %d, want exactly 1 (never a second session for the same PR)", sessionCount)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns t JOIN sessions s ON s.id = t.session_id WHERE s.spawn_source = 'github' AND s.repos::text LIKE '%reuse-deny-repo%'`).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 1 {
		t.Errorf("turn count = %d, want exactly 1 (the first mention's own turn only -- the denied reply must not add a second)", turnCount)
	}
}

// TestGitHubIntegration_ReplyOnExistingSessionAllowedForOwningMember
// proves the OTHER side of the reuse-path gate: a linked `member` who DID
// create the existing PR review session (the first mention itself was
// from this same linked identity) is allowed to reply, via the §13.3 row 2
// own/joined carve-out.
func TestGitHubIntegration_ReplyOnExistingSessionAllowedForOwningMember(t *testing.T) {
	ctx := context.Background()
	rig := newIdentityTestRig(t)

	const repoFullName = "acme/reuse-allow-repo"
	const prNumber = 505
	const commenterID = 90001005

	user := rig.createLinkedUser(t, commenterID, sqlcgen.UserRoleMember)

	first := issueCommentBodyWithCommenter(repoFullName, "reuse-allow-repo", "https://github.com/acme/reuse-allow-repo.git", prNumber, "first-mention", commenterID, "owning-member")
	firstStatus := rig.post(t, first, "delivery-reuse-allow-first")
	if firstStatus != http.StatusOK {
		t.Fatalf("first mention status = %d, want %d", firstStatus, http.StatusOK)
	}

	second := issueCommentBodyWithCommenter(repoFullName, "reuse-allow-repo", "https://github.com/acme/reuse-allow-repo.git", prNumber, "second-mention", commenterID, "owning-member")
	secondStatus := rig.post(t, second, "delivery-reuse-allow-second")
	if secondStatus != http.StatusOK {
		t.Fatalf("second mention status = %d, want %d", secondStatus, http.StatusOK)
	}

	var sessionID string
	var createdByText string
	if err := rig.pool.QueryRow(ctx,
		`SELECT id::text, coalesce(created_by::text, '') FROM sessions WHERE spawn_source = 'github' AND repos::text LIKE '%reuse-allow-repo%'`,
	).Scan(&sessionID, &createdByText); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if createdByText != user.ID.String() {
		t.Fatalf("created_by = %q, want %q", createdByText, user.ID.String())
	}

	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 2 {
		t.Errorf("turn count = %d, want 2 (both the creating mention's own turn AND the owning member's own reply)", turnCount)
	}
}

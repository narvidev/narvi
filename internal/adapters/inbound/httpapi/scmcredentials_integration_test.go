//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// repoFullNamesFromJSON extracts "owner/repo" from every {name, url,
// branch} entry in reposJSON -- the exact shape sessions.repos carries in
// production. Used only by this file's own test fixtures, to seed
// repo_settings.live_egress_enabled ahead of a session's own creation.
func repoFullNamesFromJSON(t *testing.T, reposJSON string) []string {
	t.Helper()
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(reposJSON), &repos); err != nil {
		t.Fatalf("unmarshal reposJSON fixture %q: %v", reposJSON, err)
	}
	fullNames := make([]string, 0, len(repos))
	for _, repo := range repos {
		owner, name, err := reposource.ParseOwnerRepo(repo.URL)
		if err != nil {
			t.Fatalf("parse owner/repo from fixture URL %q: %v", repo.URL, err)
		}
		fullNames = append(fullNames, owner+"/"+name)
	}
	return fullNames
}

// promoteRepoLive upserts repo_settings.live_egress_enabled = true for
// every repo named in reposJSON -- §30.4's own shadow substitution
// defaults every UN-promoted repo to shadow (§30.8), so any test in this
// file that builds a session directly (bypassing
// createSessionWithGitHubIdentityAndRepos/createOwnedGitHubReviewSessionWithRepos,
// which already do this) and wants to exercise a LIVE-path check (steps
// 7-10, never this Step's own step 6.5) must call this first.
func promoteRepoLive(ctx context.Context, t *testing.T, r testRig, reposJSON string) {
	t.Helper()
	for _, fullName := range repoFullNamesFromJSON(t, reposJSON) {
		if _, err := r.repoSettings.UpsertLiveEgressEnabled(ctx, fullName, true); err != nil {
			t.Fatalf("upsert live_egress_enabled for %s: %v", fullName, err)
		}
	}
}

// fakeReadOnlyMinter is this rig's own fake of httpapi.ReadOnlyMinter --
// there is no real GitHub App reachable from this environment (see
// internal/adapters/outbound/githubapp's own doc.go), so every test that
// exercises §30.4's shadow-substitution branch does so against this fake
// rather than a real GitHub API call. Configurable per test via its own
// exported fields (mirroring fakeSnapshotProvider's own established
// pattern in this same package): Token/Err let a test control what the
// "mint" itself returns, and the three Saw* fields record what was
// actually asked for so a test can assert on it.
type fakeReadOnlyMinter struct {
	Token githubapp.Token
	Err   error

	SawOwner     string
	SawRepoNames []string
	CallCount    int
}

// newFakeReadOnlyMinter returns a fake preconfigured to succeed with a
// fixed, obviously-fake, genuinely read-only token -- the safe default
// for every test in this rig that does not care about the shadow branch's
// own mint result specifically.
func newFakeReadOnlyMinter() *fakeReadOnlyMinter {
	return &fakeReadOnlyMinter{
		Token: githubapp.Token{
			Value:       "ghs_fakeReadOnlyInstallationToken",
			ExpiresAt:   time.Now().Add(time.Hour),
			Permissions: map[string]string{"contents": "read", "metadata": "read"},
		},
	}
}

func (m *fakeReadOnlyMinter) MintInstallationToken(_ context.Context, owner string, repoNames []string) (githubapp.Token, error) {
	m.CallCount++
	m.SawOwner = owner
	m.SawRepoNames = repoNames
	if m.Err != nil {
		return githubapp.Token{}, m.Err
	}
	return m.Token, nil
}

// sha256Hex mirrors wshub.HashSandboxToken's own algorithm exactly (SHA-256,
// hex-encoded) -- duplicated here rather than imported since this is a
// small, dependency-free test helper and importing wshub purely for one
// hash call in a test would be a heavier dependency than necessary.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// scmCredentialsResponse mirrors internal/adapters/inbound/httpapi's own
// (unexported) response shape for this test's own decode target.
type scmCredResponse struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// createSandboxWithToken creates a sandbox row for sessionID and sets its
// token_hash to HashToken-equivalent of plaintextToken -- mirrors
// wshub.HashSandboxToken's own algorithm (SHA-256 hex) directly (this test
// package cannot import wshub's unexported internals, and importing the
// whole package just for this one hash in a test is unnecessary -- the
// algorithm is trivial and stable).
func createSandboxWithToken(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, plaintextToken string) {
	t.Helper()
	if _, err := r.sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	hash := sha256Hex(plaintextToken)
	if _, err := r.pool.Exec(ctx, `UPDATE sandboxes SET token_hash = $2 WHERE session_id = $1`, sessionID, hash); err != nil {
		t.Fatalf("set token_hash: %v", err)
	}
}

// createSessionWithGitHubIdentity creates a session whose created_by user
// has a real, encrypted GitHub access token -- the SAME real
// EncryptToken flow §13.1's own OAuth callback uses, not a shortcut --
// and whose sessions.repos names a single repo on host "github.com" (the
// SAME host postScmCredentials' own default request body names), matching
// the audit remediation's own host-scoping check (design decision 1) so
// every pre-existing test in this file that relies on this helper keeps
// proving what it always proved, rather than incidentally tripping the
// new host check instead of whatever it actually intends to exercise. Use
// createSessionWithGitHubIdentityAndRepos directly for a test that needs a
// DIFFERENT repo host (e.g. a host-mismatch test).
func createSessionWithGitHubIdentity(ctx context.Context, t *testing.T, r testRig, plaintextAccessToken string) sqlcgen.Session {
	t.Helper()
	return createSessionWithGitHubIdentityAndRepos(ctx, t, r, plaintextAccessToken,
		`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`)
}

// createSessionWithGitHubIdentityAndRepos is createSessionWithGitHubIdentity's
// general-purpose form: reposJSON is written verbatim into sessions.repos
// (the same JSONB shape internal/adapters/inbound/httpapi.CreateSession's
// own real path persists, {branch, name, url} per repo).
//
// §30.4's own shadow substitution: every repo named in reposJSON is
// upserted LIVE (repo_settings.live_egress_enabled = true) before this
// function returns -- every PRE-EXISTING test in this file that uses this
// helper (or createSessionWithGitHubIdentity) is proving the LIVE
// creator-OAuth path (§9.3's own "e2e happy path", the audit-remediation
// checks, gen/host-scoping), not §30.4's shadow branch, and repo_settings.
// live_egress_enabled defaults to false for EVERY repo (§30.8) -- without
// this seed, every one of those tests would silently start receiving a
// substituted read-only credential instead of the real one they assert
// against. A test that specifically wants the shadow branch (this Step's
// own dedicated tests) creates its OWN session against a repo it
// deliberately leaves un-promoted, or explicitly demotes one back via
// r.repoSettings.UpsertLiveEgressEnabled(ctx, fullName, false).
func createSessionWithGitHubIdentityAndRepos(ctx context.Context, t *testing.T, r testRig, plaintextAccessToken, reposJSON string) sqlcgen.Session {
	t.Helper()

	for _, fullName := range repoFullNamesFromJSON(t, reposJSON) {
		if _, err := r.repoSettings.UpsertLiveEgressEnabled(ctx, fullName, true); err != nil {
			t.Fatalf("upsert live_egress_enabled for %s: %v", fullName, err)
		}
	}

	externalID := fmt.Sprintf("scm-test-github-id-%d", time.Now().UnixNano())
	user, err := r.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: externalID + "@example.com",
		DisplayName:  "SCM Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	encrypted, err := platform.EncryptToken(r.tokenEncryptionKey, []byte(plaintextAccessToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}

	email := externalID + "@example.com"
	if _, err := r.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           externalID,
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   user.ID,
		Repos:       []byte(reposJSON),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

// moveSandboxStatus sets sessionID's sandbox row to status via a plain
// UpdateStatus call -- mirrors internal/adapters/inbound/wshub's own
// _test.go helper of the identical name and behavior exactly (that
// package's own copy is unexported to its own test file; duplicated here
// rather than shared cross-package, matching every other rig helper this
// file already keeps as its own copy).
func moveSandboxStatus(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, status sqlcgen.SandboxStatus) {
	t.Helper()
	if _, err := r.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID,
		Status:    status,
	}); err != nil {
		t.Fatalf("move sandbox to %s: %v", status, err)
	}
}

// bumpSandboxGen re-spawns sessionID's own sandbox row via a real
// UpsertForSpawn call -- which (per that query's own doc comment) bumps
// gen, resets status to spawning, and rotates token_hash to
// newPlaintextToken's own hash -- and returns the resulting row. Used to
// prove the gen check compares against the sandbox row's REAL current gen
// rather than a hardcoded "1": a freshly created sandbox starts at gen 1
// (createSandboxWithToken), so this bumps it to gen 2.
func bumpSandboxGen(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, newPlaintextToken string) sqlcgen.Sandbox {
	t.Helper()
	hash := sha256Hex(newPlaintextToken)
	row, err := r.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID,
		TokenHash: &hash,
	})
	if err != nil {
		t.Fatalf("bump sandbox gen: %v", err)
	}
	return row
}

// postScmCredentials posts the defaults every PRE-EXISTING test in this
// file relies on: X-Sandbox-Gen "1" (matching a freshly created sandbox
// row's own default gen, §3.2) and request body host "github.com"
// (matching createSessionWithGitHubIdentity's own default repo host) --
// this audit remediation's own two new checks (host-scoping, gen fencing)
// must not silently change what those pre-existing tests were already
// proving. Use postScmCredentialsFull directly for a test that needs a
// non-default gen/host (or no X-Sandbox-Gen header at all).
func postScmCredentials(t *testing.T, r testRig, sessionID string, bearer string) (int, scmCredResponse) {
	t.Helper()
	return postScmCredentialsFull(t, r, sessionID, bearer, "1", "github.com")
}

// postScmCredentialsFull is postScmCredentials' general-purpose form: gen
// is sent as the X-Sandbox-Gen header verbatim, or omitted entirely when
// gen == "" (matching a real caller that never sends the header at all,
// not merely an empty one).
func postScmCredentialsFull(t *testing.T, r testRig, sessionID, bearer, gen, host string) (int, scmCredResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/sessions/"+sessionID+"/scm-credentials",
		strings.NewReader(fmt.Sprintf(`{"host":%q}`, host)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got scmCredResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

// postScmCredentialsForceReadOnly mirrors postScmCredentialsFull exactly,
// except the request body also carries "forceReadOnly": true -- §30.4(2)'s
// own signal a BootModeBuild credential-helper invocation sends.
func postScmCredentialsForceReadOnly(t *testing.T, r testRig, sessionID, bearer, gen, host string) (int, scmCredResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/sessions/"+sessionID+"/scm-credentials",
		strings.NewReader(fmt.Sprintf(`{"host":%q,"forceReadOnly":true}`, host)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got scmCredResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

// TestScmCredentials_Success proves the full real flow: a valid sandbox
// bearer token + a session whose user has a real, encrypted GitHub access
// token -> 200 with a credential whose password decrypts to the SAME
// plaintext a real §13.1 OAuth flow would have encrypted.
func TestScmCredentials_Success(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	const plaintextAccessToken = "gho_realGitHubAccessToken"
	session := createSessionWithGitHubIdentity(ctx, t, rig, plaintextAccessToken)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	before := time.Now()
	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Username != "x-access-token" {
		t.Errorf("Username = %q, want %q", got.Username, "x-access-token")
	}
	if got.Password != plaintextAccessToken {
		t.Errorf("Password = %q, want %q (must decrypt to the SAME plaintext originally encrypted)", got.Password, plaintextAccessToken)
	}
	wantExpiry := before.Add(platform.DefaultTimeouts().ScmCredentialTTL)
	if got.ExpiresAt.Sub(wantExpiry).Abs() > time.Minute {
		t.Errorf("ExpiresAt = %v, want close to %v", got.ExpiresAt, wantExpiry)
	}
}

// TestScmCredentials_MissingBearer proves a missing Authorization header
// is 401.
func TestScmCredentials_MissingBearer(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_x")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

// TestScmCredentials_InvalidBearer proves a wrong (but present) bearer
// token is 401.
func TestScmCredentials_InvalidBearer(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_x")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "totally-wrong-token")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

// TestScmCredentials_UnknownSession proves a well-formed but nonexistent
// session id is 404.
func TestScmCredentials_UnknownSession(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postScmCredentials(t, rig, "11111111-1111-1111-1111-111111111111", "any-token")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestScmCredentials_MalformedSessionID proves a malformed session id is
// ALSO 404 -- mirrors wshub/sandbox.go's own "malformed and nonexistent
// both mean no such session" precedent (this caller is sandbox-agent
// code, never a browser).
func TestScmCredentials_MalformedSessionID(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postScmCredentials(t, rig, "not-a-uuid", "any-token")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestScmCredentials_NoCreatedBy proves a session with no created_by user
// (created_by NULL) -> 403, the honest "no bot fallback" gap.
func TestScmCredentials_NoCreatedBy(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	// Repos matches postScmCredentials' own default request host
	// ("github.com") so this test exercises the created_by check it
	// actually names, rather than incidentally tripping the audit
	// remediation's own (unrelated) host-scoping check instead.
	// §30.4's own shadow substitution: promoted live so this test still
	// reaches the created_by check (step 8) rather than this Step's own
	// shadow branch (step 6.5) -- see promoteRepoLive's own doc comment.
	promoteRepoLive(ctx, t, rig, reviewSessionRepos)
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestScmCredentials_NoGitHubIdentity proves a session whose user has no
// linked GitHub identity at all -> 403.
func TestScmCredentials_NoGitHubIdentity(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "no-github@example.com", DisplayName: "No GitHub", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Repos matches postScmCredentials' own default request host -- see
	// the identical comment in TestScmCredentials_NoCreatedBy above.
	// §30.4's own shadow substitution: promoted live -- see
	// promoteRepoLive's own doc comment.
	promoteRepoLive(ctx, t, rig, reviewSessionRepos)
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID,
		Repos: []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestScmCredentials_NoGitHubIdentity_FallsBackToBotToken proves review
// round 1's own finding O5: a creator with NO github identity at all --
// the ordinary case for a user who signed in ONLY through OIDC (§41.3)
// and has never linked GitHub -- now falls back to the SAME static bot
// credential a review session already receives (step 7), when this
// deployment has one configured. Otherwise identical to
// TestScmCredentials_NoGitHubIdentity above, except rig.botToken is set:
// without this fallback, such a creator's live session could never push
// at all, so sessionactor's own §8.11 createPRBestEffort bot-fallback PR
// path could never even be reached (only theoretically correct).
func TestScmCredentials_NoGitHubIdentity_FallsBackToBotToken(t *testing.T) {
	const realBotToken = "bot-token-for-oidc-only-creator"
	rig := newTestRig(t, func(r *testRig) { r.botToken = realBotToken })
	ctx := context.Background()

	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "oidc-only@example.com", DisplayName: "OIDC Only", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Deliberately NO identities row of any provider -- this user has
	// never linked a GitHub account.
	promoteRepoLive(ctx, t, rig, reviewSessionRepos)
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID,
		Repos: []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (a bot token IS configured for this deployment)", status, http.StatusOK)
	}
	if got.Password != realBotToken {
		t.Errorf("Password = %q, want the bot token %q -- never the (nonexistent) creator token", got.Password, realBotToken)
	}
}

// TestScmCredentials_NilTokenHash_Rejected proves this endpoint's own
// bearer check (verifySandboxBearerToken) does NOT inherit wshub's own
// WS-handshake nil-token_hash bypass ("accept any non-empty presented
// token while token_hash is NULL") -- a sandbox row with no token_hash
// ever set (created directly via sandboxes.Create, mirroring a legacy or
// not-yet-minted row) must reject EVERY presented bearer token with 401,
// never fall through to a real, decrypted credential. Confirmed
// unreachable via any real spawn path this Step's own code takes (which
// always sets token_hash), but this test locks in the deliberately
// stricter behavior regardless -- see verifySandboxBearerToken's own doc
// comment for why this endpoint cannot afford to inherit that bypass.
func TestScmCredentials_NilTokenHash_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	if _, err := rig.sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatalf("create sandbox (token_hash left NULL): %v", err)
	}

	status, _ := postScmCredentials(t, rig, session.ID.String(), "any-non-empty-token")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (a nil token_hash must reject, never bypass, on this endpoint)", status, http.StatusUnauthorized)
	}
}

// TestScmCredentials_TamperedCiphertext proves a real, tampered/corrupted
// access_token_encrypted value (AES-GCM's own authentication tag catching
// the tamper at platform.DecryptToken time, not a mocked failure) hits the
// SAME 403 "no usable credential" branch as the other sub-cases in this
// outcome class -- this sub-case was previously confirmed only by code
// inspection, never independently exercised by a real corrupted ciphertext
// flowing through the real DecryptToken call.
func TestScmCredentials_TamperedCiphertext(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	// Corrupt the stored ciphertext directly, in place -- a real tampered
	// value, not a mock -- so DecryptToken's own AES-GCM authentication
	// check must be what catches this, exactly like this file's own
	// tokenencrypt.go doc comment promises ("Returns an error... if
	// ciphertext was tampered with in any way").
	if _, err := rig.pool.Exec(ctx,
		`UPDATE identities SET access_token_encrypted = access_token_encrypted || '\xff'::bytea WHERE user_id = $1`,
		session.CreatedBy,
	); err != nil {
		t.Fatalf("corrupt access_token_encrypted: %v", err)
	}

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (tampered ciphertext must hit the same no-usable-credential branch as every other sub-case)", status, http.StatusForbidden)
	}
}

// TestScmCredentials_NoStoredToken proves an identity with no
// access_token_encrypted at all -> 403.
func TestScmCredentials_NoStoredToken(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "no-token@example.com", DisplayName: "No Token", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	email := "no-token@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "ext-no-token",
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	// Repos matches postScmCredentials' own default request host -- see the
	// identical comment in TestScmCredentials_NoCreatedBy above.
	// §30.4's own shadow substitution: promoted live -- see
	// promoteRepoLive's own doc comment.
	promoteRepoLive(ctx, t, rig, reviewSessionRepos)
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID,
		Repos: []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestScmCredentials_GitHubIdentityNoStoredToken_NeverFallsBackToBot
// proves review round 1's own O5 distinction the OTHER direction: a
// creator who DOES have a github identity, but whose stored token is
// unusable (nil, here -- TestScmCredentials_TamperedCiphertext above
// covers the decrypt-failure sub-case identically), must NEVER receive
// the bot-token fallback TestScmCredentials_NoGitHubIdentity_
// FallsBackToBotToken proves for the "no identity at all" case --
// otherwise identical to TestScmCredentials_NoStoredToken above, except
// rig.botToken IS configured here. Falling back would misrepresent an
// existing account's broken credential as "no linked account" to
// whoever reviews the resulting push/PR.
func TestScmCredentials_GitHubIdentityNoStoredToken_NeverFallsBackToBot(t *testing.T) {
	const realBotToken = "bot-token-must-never-cover-a-broken-existing-identity"
	rig := newTestRig(t, func(r *testRig) { r.botToken = realBotToken })
	ctx := context.Background()

	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "no-token-with-bot@example.com", DisplayName: "No Token, Bot Configured", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	email := "no-token-with-bot@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "ext-no-token-with-bot",
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	promoteRepoLive(ctx, t, rig, reviewSessionRepos)
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID,
		Repos: []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (an existing-but-unusable identity must never fall back to the bot token)", status, http.StatusForbidden)
	}
	if got.Password == realBotToken {
		t.Error("Password == the bot token -- an existing GitHub identity with no stored token must never receive the bot fallback")
	}
}

// --- Host scoping (audit remediation: security-crosscutting &
// docs-completeness-vs-plan lenses, design decision 1) ---

// TestScmCredentials_HostMismatch_Rejected proves a session whose repos
// are all on ONE host (gitlab.example.com) is rejected (403) for a request
// naming a DIFFERENT host (github.com, postScmCredentials' own default) --
// §5.2 "scoped https+host only": the decrypted token must never be handed
// back for a host the session's own repos don't actually use.
func TestScmCredentials_HostMismatch_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentityAndRepos(ctx, t, rig, "gho_realGitHubAccessToken",
		`[{"name":"other","url":"https://gitlab.example.com/foo/bar","branch":null}]`)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (requested host github.com is not among the session's own repo hosts)", status, http.StatusForbidden)
	}
}

// TestScmCredentials_HostMatch_Succeeds proves a session whose repos ARE
// on the requested host succeeds -- the positive counterpart to
// TestScmCredentials_HostMismatch_Rejected, and also proves the match is
// case-insensitive: the persisted repo URL's own host is mixed-case
// ("GitHub.COM") while the requested host is lowercase ("github.com"),
// per this batch's own design decision 1 ("matching ordinary HTTP
// host-header comparison convention").
func TestScmCredentials_HostMatch_Succeeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentityAndRepos(ctx, t, rig, "gho_realGitHubAccessToken",
		`[{"name":"narvi","url":"https://GitHub.COM/narvidev/narvi","branch":null}]`)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (case-insensitive host match)", status, http.StatusOK)
	}
	if got.Password != "gho_realGitHubAccessToken" {
		t.Errorf("Password = %q, want the real decrypted token", got.Password)
	}
}

// TestScmCredentials_NoRepos_Rejected proves a session with NO repos at
// all (an empty list, not merely a mismatched one) is rejected for any
// requested host -- there is nothing to match against.
func TestScmCredentials_NoRepos_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentityAndRepos(ctx, t, rig, "gho_x", `[]`)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (no repos to match against)", status, http.StatusForbidden)
	}
}

// TestScmCredentials_MalformedReposJSON proves a real, corrupted
// sessions.repos value (not a mocked failure) is a 500, not a 403 -- the
// SAME "corrupt a real column directly via raw SQL" technique
// TestScmCredentials_TamperedCiphertext already established in this file,
// applied to sessionRepoHosts' own json.Unmarshal error path. sessions.
// repos is already-trusted, already-persisted data by the time this
// endpoint runs (see sessionRepoHosts' own doc comment), so corruption
// here is a genuine server-side anomaly, not a caller-attributable
// rejection -- it must never be silently folded into the 403 "no usable
// credential" outcome class.
func TestScmCredentials_MalformedReposJSON(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	if _, err := rig.pool.Exec(ctx,
		`UPDATE sessions SET repos = '"not an array"'::jsonb WHERE id = $1`,
		session.ID,
	); err != nil {
		t.Fatalf("corrupt sessions.repos: %v", err)
	}

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (malformed sessions.repos is a server-side anomaly, not a caller-attributable rejection)", status, http.StatusInternalServerError)
	}
}

// --- Dead-sandbox check (audit remediation, design decision 2) ---

// TestScmCredentials_DeadSandboxStatus proves a sandbox in a dead status
// is rejected with 410, even with an otherwise-valid bearer token, gen,
// and host -- mirrors internal/adapters/inbound/wshub/sandbox.go's own
// precedent exactly (same status code, same "session stopped" message
// convention). "Stale" (internal/domain/sandbox.StateStale, the domain's
// third dead state) is deliberately NOT in this table: migrations/
// 000006_sandboxes.up.sql's own sandbox_status Postgres ENUM never defines
// a 'stale' value at all (confirmed directly against that migration
// before writing this test, not assumed) -- it is a domain-only, computed
// state that is never literally persisted to this column in production
// either, matching internal/adapters/inbound/wshub's own sandbox_test.go
// precedent exactly: despite ITS doc comment also naming all 3 dead
// statuses, it too only ever exercises Failed against a real row, for the
// identical reason.
func TestScmCredentials_DeadSandboxStatus(t *testing.T) {
	tests := []struct {
		name   string
		status sqlcgen.SandboxStatus
	}{
		{"stopped", sqlcgen.SandboxStatusStopped},
		{"failed", sqlcgen.SandboxStatusFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()

			session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
			createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
			moveSandboxStatus(ctx, t, rig, session.ID, tc.status)

			status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
			if status != http.StatusGone {
				t.Errorf("status = %d, want %d (dead sandbox status %s)", status, http.StatusGone, tc.status)
			}
		})
	}
}

// TestScmCredentials_SuspectSandbox_NotDead proves a Suspect sandbox --
// deliberately NOT in the dead-status deny-list -- still succeeds, exactly
// like wshub's own "Suspect is deliberately not dead" precedent: a
// sandbox merely suspected of having missed a heartbeat must still be
// able to mint git credentials.
func TestScmCredentials_SuspectSandbox_NotDead(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")
	moveSandboxStatus(ctx, t, rig, session.ID, sqlcgen.SandboxStatusSuspect)

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d (Suspect is not a dead status)", status, http.StatusOK)
	}
}

// --- Gen fencing (audit remediation, design decision 2) ---

// TestScmCredentials_GenMismatch_Rejected proves a stale/wrong
// X-Sandbox-Gen -> 403, mirroring wshub's own gen-mismatch reasoning
// (§9.3 scenario #6).
func TestScmCredentials_GenMismatch_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token") // gen 1

	status, _ := postScmCredentialsFull(t, rig, session.ID.String(), "sandbox-bearer-token", "999", "github.com")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (gen mismatch)", status, http.StatusForbidden)
	}
}

// TestScmCredentials_MissingGen_Rejected proves an ABSENT X-Sandbox-Gen
// header -- not merely a mismatched one -- is rejected the SAME way (403):
// there is nothing to compare against sandboxRow.Gen, so a missing header
// fails identically to a wrong value rather than surfacing as some other,
// distinguishable status.
func TestScmCredentials_MissingGen_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentialsFull(t, rig, session.ID.String(), "sandbox-bearer-token", "" /* no X-Sandbox-Gen header at all */, "github.com")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (missing X-Sandbox-Gen header)", status, http.StatusForbidden)
	}
}

// TestScmCredentials_CorrectCurrentGen_Succeeds proves the gen check
// compares against the sandbox row's REAL current gen, not a hardcoded
// "1": bumps the sandbox to gen 2 via a real UpsertForSpawn respawn (the
// SAME query production respawns use), then proves presenting gen "2"
// succeeds.
func TestScmCredentials_CorrectCurrentGen_Succeeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token") // gen 1
	bumped := bumpSandboxGen(ctx, t, rig, session.ID, "respawned-bearer-token")

	if bumped.Gen != 2 {
		t.Fatalf("bumped.Gen = %d, want 2 (test setup assumption: UpsertForSpawn bumps an existing gen-1 row to gen 2)", bumped.Gen)
	}

	status, got := postScmCredentialsFull(t, rig, session.ID.String(), "respawned-bearer-token", fmt.Sprintf("%d", bumped.Gen), "github.com")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (correct current gen)", status, http.StatusOK)
	}
	if got.Password != "gho_realGitHubAccessToken" {
		t.Errorf("Password = %q, want the real decrypted token", got.Password)
	}
}

// --- Creator disabled/role recheck (audit finding M8) ---

// TestScmCredentials_DisabledCreator_Denied proves a session whose creator
// was disabled AFTER session/sandbox creation -- mid-turn, e.g. an admin's
// own offboarding or incident-response disable -- is denied a credential
// (403), even though the sandbox bearer token, gen, and host are all
// otherwise valid. Mirrors internal/app/sessionactor's own
// TestHandleSandboxEvent_PushComplete_DisabledCreator_SkipsPRCreation
// (pushpr_integration_test.go): same staleness scenario, same session
// creator, just exercised at THIS endpoint -- the earlier,
// credential-minting half of the push_complete chain -- instead of that
// test's later PR-creation half.
func TestScmCredentials_DisabledCreator_Denied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	// Disable the session creator mid-turn, after the session/sandbox
	// already exist. No UserStore mutation exists for Disabled today
	// (only ListMembers' own read exposure, httpapi/members.go) -- set it
	// directly via raw SQL, mirroring pushpr_integration_test.go's own
	// established precedent for this exact gap.
	if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, session.CreatedBy); err != nil {
		t.Fatalf("disable session creator: %v", err)
	}

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (session creator is now disabled)", status, http.StatusForbidden)
	}
	if got.Password != "" {
		t.Errorf("Password = %q, want empty (no real credential ever handed back for a disabled creator)", got.Password)
	}
}

// TestScmCredentials_DemotedToViewerCreator_Denied proves a session whose
// creator was demoted to viewer AFTER session/sandbox creation is ALSO
// denied (403) -- the same §13.3 viewer-guard threshold
// creatorMayGetPRAttribution already enforces at PR-creation time
// (internal/app/sessionactor/githubtoken.go), now enforced here too, at
// credential-minting time. Uses a real UserStore.UpdateRole call (the same
// mutation an admin's own real role-change endpoint performs), not raw
// SQL, since that store method already exists.
func TestScmCredentials_DemotedToViewerCreator_Denied(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	if _, err := rig.users.UpdateRole(ctx, session.CreatedBy, sqlcgen.UserRoleViewer); err != nil {
		t.Fatalf("demote session creator to viewer: %v", err)
	}

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (session creator is now a viewer)", status, http.StatusForbidden)
	}
	if got.Password != "" {
		t.Errorf("Password = %q, want empty (no real credential ever handed back for a demoted creator)", got.Password)
	}
}

// TestScmCredentials_StillMemberCreator_Succeeds is the positive
// counterpart to the two tests above: a session creator who is still an
// active member (never disabled, never demoted) succeeds exactly like
// TestScmCredentials_Success -- proving the new disabled/role recheck
// itself introduces no false-positive rejection for the ordinary,
// still-eligible case.
func TestScmCredentials_StillMemberCreator_Succeeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentity(ctx, t, rig, "gho_realGitHubAccessToken")
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (creator is still an active member)", status, http.StatusOK)
	}
	if got.Password != "gho_realGitHubAccessToken" {
		t.Errorf("Password = %q, want the real decrypted token", got.Password)
	}
}

// createOwnedGitHubReviewSessionWithRepos is
// createOwnedGitHubReviewSession's (reviewretrigger_integration_test.go)
// own general-purpose form: also sets sessions.repos to reposJSON, so a
// review session fixture used here can pass THIS file's own host-scoping
// check (step 6) before ever reaching the review-session branch (step
// 6.5) this file's own new tests exercise -- createOwnedGitHubReviewSession
// itself sets no repos at all (its own callers never needed to clear
// step 6 first).
func (r testRig) createOwnedGitHubReviewSessionWithRepos(ctx context.Context, t *testing.T, ownerID pgtype.UUID, repoFullName string, prNumber int32, reposJSON string) sqlcgen.Session {
	t.Helper()

	// §30.4's own shadow substitution: this helper's own tests are
	// proving the LIVE review-bot-token path (step 7), not this Step's
	// shadow branch (step 6.5) -- see promoteRepoLive's own doc comment.
	promoteRepoLive(ctx, t, r, reposJSON)

	session, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   ownerID,
		Repos:       []byte(reposJSON),
	})
	if err != nil {
		t.Fatalf("create test github review session: %v", err)
	}
	if err := r.prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := r.prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	return session
}

// reviewSessionRepos is the SAME repos JSON shape createSessionWithGitHubIdentity's
// own default names ("github.com" host, matching postScmCredentials' own
// default request body) -- kept as its own named constant here (rather
// than inlined at each call site) purely for readability.
const reviewSessionRepos = `[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`

// TestScmCredentials_ReviewSession_UsesBotToken proves the audit
// remediation this Step's own confirmed finding required: a review
// session (a github_pr_sessions row exists for it) mints rig.botToken --
// never the session creator's own personal GitHub OAuth token, even
// though that creator DOES have one, real and encrypted, on file. This is
// the positive proof that a review sandbox's own credential-helper flow
// can no longer walk away with an arbitrary human commenter's broad,
// cross-repo personal credential.
func TestScmCredentials_ReviewSession_UsesBotToken(t *testing.T) {
	const realBotToken = "bot-token-for-review-sessions"
	rig := newTestRig(t, func(r *testRig) { r.botToken = realBotToken })
	ctx := context.Background()

	owner, _ := rig.createAuthenticatedUser(ctx, t)
	// The creator's OWN personal GitHub identity/token -- proving this
	// branch does not merely happen to succeed because no personal
	// credential exists; a real one exists and is deliberately never used.
	encrypted, err := platform.EncryptToken(rig.tokenEncryptionKey, []byte("gho_creatorsOwnPersonalToken"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := "review-session-owner@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               owner.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           "review-session-owner-external-id",
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	session := rig.createOwnedGitHubReviewSessionWithRepos(ctx, t, owner.ID, "acme/scm-creds-review", 11, reviewSessionRepos)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Username != "x-access-token" {
		t.Errorf("Username = %q, want %q", got.Username, "x-access-token")
	}
	if got.Password != realBotToken {
		t.Errorf("Password = %q, want the bot token %q -- never the creator's own personal token", got.Password, realBotToken)
	}
	if got.Password == "gho_creatorsOwnPersonalToken" {
		t.Fatalf("Password equals the creator's own personal GitHub token -- the exact credential-exposure gap this Step's audit remediation closes")
	}
}

// TestScmCredentials_ReviewSession_NoCreatorGuard_StillSucceeds proves the
// review-session branch (step 7, unaffected by this Step's own new step
// 6.5 shadow-substitution check, since this session's repo is promoted
// live below) is reached and succeeds via botToken even when the session
// has no created_by user at all (CreatedBy invalid) -- steps 8-10
// (created_by/disabled/viewer/identity checks) exist only to gate a
// PER-USER credential this branch never looks up, so none of them can
// block it.
func TestScmCredentials_ReviewSession_NoCreatorGuard_StillSucceeds(t *testing.T) {
	const realBotToken = "bot-token-no-creator"
	rig := newTestRig(t, func(r *testRig) { r.botToken = realBotToken })
	ctx := context.Background()

	// §30.4's own shadow substitution: promoted live so this test still
	// reaches the review-bot-token branch (step 7) -- see
	// promoteRepoLive's own doc comment.
	promoteRepoLive(ctx, t, rig, reviewSessionRepos)
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(reviewSessionRepos),
	})
	if err != nil {
		t.Fatalf("create test github review session with no creator: %v", err)
	}
	if err := rig.prSessions.EnsureRow(ctx, "acme/scm-creds-review-nocreator", 12); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := rig.prSessions.SetSessionID(ctx, "acme/scm-creds-review-nocreator", 12, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (a review session needs no created_by user at all)", status, http.StatusOK)
	}
	if got.Password != realBotToken {
		t.Errorf("Password = %q, want the bot token %q", got.Password, realBotToken)
	}
}

// TestScmCredentials_NonReviewSession_HostScopingStillEnforced proves the
// new review-session branch is checked AFTER host-scoping (step 6), never
// before it: an ordinary (non-review) session whose repos name a
// DIFFERENT host than the request is still rejected exactly like
// TestScmCredentials_HostMismatch_Rejected, unaffected by this Step's own
// addition.
func TestScmCredentials_NonReviewSession_HostScopingStillEnforced(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session := createSessionWithGitHubIdentityAndRepos(ctx, t, rig, "gho_realGitHubAccessToken",
		`[{"name":"narvi","url":"https://gitlab.com/narvidev/narvi","branch":null}]`)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (host mismatch must still be enforced ahead of the review-session check)", status, http.StatusForbidden)
	}
}

// --- §30.4's own shadow substitution: the four hard requirements ---

// failingLedgerStore is a shadowledger.Store that always fails -- used
// only by TestScmCredentials_Shadow_LedgerRecordFailureIs500 to prove the
// record-or-fail path itself, which a real Postgres-backed store cannot
// be made to do on demand without actually breaking the connection.
type failingLedgerStore struct{}

func (failingLedgerStore) Create(context.Context, sqlcgen.CreateShadowSCMWriteParams) (sqlcgen.ShadowScmWrite, error) {
	return sqlcgen.ShadowScmWrite{}, fmt.Errorf("failingLedgerStore: simulated write failure")
}

// TestScmCredentials_Shadow_ReviewSessionReceivesReadOnlyCredential is
// §30.4(1)'s own NAMED requirement: "A dedicated test asserts a review
// session in shadow receives a read-only credential." Without the single
// interception point placed BEFORE the review-session branch, this
// session -- a github_pr_sessions row exists for it, and its repo is
// deliberately left un-promoted (shadow is repo_settings' own default,
// §30.8) -- would reach step 7 and receive rig.botToken, the fully
// write-capable credential §30.4(1) says every shadow review sandbox must
// never see.
func TestScmCredentials_Shadow_ReviewSessionReceivesReadOnlyCredential(t *testing.T) {
	const realBotToken = "bot-token-must-never-reach-a-shadow-review-sandbox"
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) {
		r.botToken = realBotToken
		r.readOnlyMinter = minter
	})
	ctx := context.Background()

	owner, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "shadow-review-owner@example.com", DisplayName: "Shadow Review Owner", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	encrypted, err := platform.EncryptToken(rig.tokenEncryptionKey, []byte("gho_creatorsOwnPersonalToken"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := "shadow-review-owner@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: owner.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "shadow-review-owner-external-id",
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin, AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	// Deliberately NOT calling promoteRepoLive: repo_settings defaults
	// this repo to shadow, exactly the ordinary state of any newly
	// connected, not-yet-promoted repository (§30.8).
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   owner.ID,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/shadow-owner/shadow-repo","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := rig.prSessions.EnsureRow(ctx, "shadow-owner/shadow-repo-review", 21); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := rig.prSessions.SetSessionID(ctx, "shadow-owner/shadow-repo-review", 21, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Password == realBotToken {
		t.Fatal("Password equals the bot token -- a shadow review sandbox received the fully write-capable credential §30.4(1) requires it never see")
	}
	if got.Password != minter.Token.Value {
		t.Errorf("Password = %q, want the substituted read-only token %q", got.Password, minter.Token.Value)
	}
	if minter.CallCount != 1 {
		t.Errorf("readOnlyMinter called %d times, want 1", minter.CallCount)
	}
	if minter.SawOwner != "shadow-owner" || len(minter.SawRepoNames) != 1 || minter.SawRepoNames[0] != "shadow-repo" {
		t.Errorf("minter saw owner=%q repoNames=%v, want owner=%q repoNames=[shadow-repo]", minter.SawOwner, minter.SawRepoNames, "shadow-owner")
	}
}

// TestScmCredentials_Shadow_NonReviewSessionReceivesReadOnlyCredential
// proves the SAME interception point also covers the OTHER mint branch
// (the creator's decrypted OAuth token, steps 8-10) -- §30.4(1) requires
// ONE interception covering BOTH branches, not two independent fixes.
func TestScmCredentials_Shadow_NonReviewSessionReceivesReadOnlyCredential(t *testing.T) {
	const realCreatorToken = "gho_creatorTokenMustNeverReachAShadowSandbox"
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	// Deliberately using createSessionWithGitHubIdentityAndRepos's own
	// LOWER-LEVEL pieces rather than the helper itself, since that helper
	// promotes its repo live by design (every OTHER test in this file
	// wants that) -- this test wants the opposite, the default un-promoted
	// state.
	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "shadow-nonreview@example.com", DisplayName: "Shadow NonReview", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	encrypted, err := platform.EncryptToken(rig.tokenEncryptionKey, []byte(realCreatorToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := "shadow-nonreview@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: user.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "shadow-nonreview-external-id",
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin, AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: user.ID,
		Repos: []byte(`[{"name":"shadow-repo","url":"https://github.com/shadow-owner2/shadow-repo","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Password == realCreatorToken {
		t.Fatal("Password equals the creator's own real OAuth token -- reached the LIVE creator-OAuth branch despite the repo being un-promoted (shadow)")
	}
	if got.Password != minter.Token.Value {
		t.Errorf("Password = %q, want the substituted read-only token %q", got.Password, minter.Token.Value)
	}
}

// TestScmCredentials_Shadow_ForceReadOnlyAppliesEvenWhenLive proves
// §30.4(2)'s own client signal: a BootModeBuild credential-helper request
// (forceReadOnly: true) gets the read-only substitution even for a repo
// that IS promoted live -- "a build only needs read" is unconditional, not
// merely a fallback for an un-promoted repo.
func TestScmCredentials_Shadow_ForceReadOnlyAppliesEvenWhenLive(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	const plaintextAccessToken = "gho_realGitHubAccessTokenForceReadOnlyTest"
	session := createSessionWithGitHubIdentity(ctx, t, rig, plaintextAccessToken)
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentialsForceReadOnly(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "github.com")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Password == plaintextAccessToken {
		t.Fatal("Password equals the creator's own real OAuth token -- forceReadOnly must apply even though this repo is promoted live")
	}
	if got.Password != minter.Token.Value {
		t.Errorf("Password = %q, want the substituted read-only token %q", got.Password, minter.Token.Value)
	}
}

// TestScmCredentials_Shadow_MultiOwnerRefused proves a session whose
// matched repos on the requested host span more than one distinct owner
// is refused (403) rather than served by a credential scoped to only one
// of them -- a GitHub App installation token cannot cover two accounts at
// once, and silently picking one owner would leave the sandbox believing
// it has a credential that only half-works.
func TestScmCredentials_Shadow_MultiOwnerRefused(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos: []byte(`[
			{"name":"repo-one","url":"https://github.com/owner-one/repo-one","branch":null},
			{"name":"repo-two","url":"https://github.com/owner-two/repo-two","branch":null}
		]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (repos on this host span two distinct owners)", status, http.StatusForbidden)
	}
	if minter.CallCount != 0 {
		t.Errorf("readOnlyMinter called %d times, want 0 (must refuse before ever attempting a mint)", minter.CallCount)
	}
}

// TestScmCredentials_Shadow_ScopeCheckFailureRefusesAndRecordsLedger
// proves §30.4(4)'s own per-mint scope introspection: a minted token
// carrying anything beyond read access is refused (403, the same generic
// body), never returned to the sandbox -- and the refusal is recorded
// into shadow_scm_writes with the SAME record-or-fail semantics the rest
// of the ledger already enforces.
func TestScmCredentials_Shadow_ScopeCheckFailureRefusesAndRecordsLedger(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	minter.Token.Permissions = map[string]string{"contents": "write", "metadata": "read"}
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       []byte(`[{"name":"scoped-repo","url":"https://github.com/scope-owner/scoped-repo","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", status, http.StatusForbidden)
	}
	if got.Password == minter.Token.Value {
		t.Fatal("Password equals the over-scoped minted token -- it must never be returned once the scope check refuses it")
	}

	// rig.shadowLedger is typed shadowledger.Store (Create only, so a
	// test can swap in a failing fake -- see failingLedgerStore above);
	// reading the row back uses a fresh, ordinary reader over the SAME
	// pool the write actually went to.
	ledgerReader := narvipg.NewShadowSCMWriteStore(rig.pool)
	rows, err := ledgerReader.ListForRepo(ctx, "scope-owner/scoped-repo", 10)
	if err != nil {
		t.Fatalf("ListForRepo: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("shadow_scm_writes rows for scope-owner/scoped-repo = %d, want 1", len(rows))
	}
	if rows[0].Operation != "scm_credential_mint_refused" {
		t.Errorf("rows[0].Operation = %q, want %q", rows[0].Operation, "scm_credential_mint_refused")
	}
	if strings.Contains(string(rows[0].SpecJson), minter.Token.Value) {
		t.Errorf("rows[0].SpecJson = %s, must never carry the (rejected) token value", rows[0].SpecJson)
	}
}

// TestScmCredentials_Shadow_LedgerRecordFailureIs500 proves record-or-fail
// end to end through this handler: when the scope check refuses a mint
// AND the ledger itself cannot record that refusal, the handler must fail
// loudly (500), never silently 403 as if the refusal were safely
// evidenced.
func TestScmCredentials_Shadow_LedgerRecordFailureIs500(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	minter.Token.Permissions = map[string]string{"contents": "write"}
	rig := newTestRig(t, func(r *testRig) {
		r.readOnlyMinter = minter
		r.shadowLedger = failingLedgerStore{}
	})
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (a refusal that cannot be recorded must fail loudly)", status, http.StatusInternalServerError)
	}
}

// TestScmCredentials_Shadow_MintErrorIsInternalError proves a genuine
// failure to reach GitHub while minting (as opposed to a scope-check
// refusal of a successfully minted token) surfaces as a plain 500, never
// as a 403 that could be confused with a deliberate security refusal.
func TestScmCredentials_Shadow_MintErrorIsInternalError(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	minter.Err = fmt.Errorf("simulated GitHub API failure")
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", status, http.StatusInternalServerError)
	}
}

// TestScmCredentials_UnparseableRepoIsNotWriteCapable closes a fail-open
// the review found, and the hole is worth stating exactly.
//
// TWO parsers read the same session repos. The authorization gate accepts
// any repo URL that has a host; the substitution set additionally
// requires an owner/repo path. A URL like "https://github.com/acme" --
// which session creation accepts, validating scheme and host only --
// therefore passed the gate and contributed NOTHING to the set. The
// substitution loop then never ran a single iteration, and the handler
// fell through to the write-capable branches.
//
// The deployment-wide switch was bypassed along with it, because it was
// only ever read INSIDE that loop: a dedicated evaluation deployment with
// the master switch ON would still have handed out a write-capable token.
// A malformed URL decided whether a credential could write.
func TestScmCredentials_UnparseableRepoIsNotWriteCapable(t *testing.T) {
	const realBotToken = "bot-token-must-never-be-reached-through-an-unparseable-url"
	const realCreatorToken = "gho_creatorTokenMustNeverBeReachedThroughAnUnparseableURL"
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) {
		r.botToken = realBotToken
		r.readOnlyMinter = minter
	})
	ctx := context.Background()

	owner, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unparseable-owner@example.com", DisplayName: "Unparseable Owner", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	encrypted, err := platform.EncryptToken(rig.tokenEncryptionKey, []byte(realCreatorToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := "unparseable-owner@example.com"
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: owner.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: "unparseable-owner-external-id",
		Email: &email, EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAdmin, AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	// A host with no owner/repo path: accepted by the authorization gate,
	// invisible to the substitution set.
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		CreatedBy:   owner.ID,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/acme","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	// The outcome is a refusal, not a read-only token, and that is the
	// correct end of this path: with no owner/repo the mint has nothing
	// to scope a token TO, and no credential at all is strictly safer
	// than a write-capable one. What this test pins is the negative --
	// neither write-capable secret is reachable through a URL shape.
	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
	if got.Password == realBotToken || got.Password == realCreatorToken {
		t.Fatal("a session whose repo URL could not be read as owner/repo received a WRITE-CAPABLE credential; an unparseable URL must never decide that")
	}
	if minter.CallCount != 0 {
		t.Errorf("readOnlyMinter called %d times, want 0: there is no owner to scope a read-only token to", minter.CallCount)
	}
}

// TestScmCredentials_Shadow_SubstitutionIsRecorded proves §30.6's own
// "the shadow mint records its substitution" end to end -- the SUCCESS
// counterpart to the refusal test above, and the record that was missing.
//
// A refusal announces itself: the sandbox gets a 403 and stops. A
// successful substitution announces itself to nobody -- the sandbox is
// handed a working credential and only discovers what it lost if it tries
// to push. This row is the only place that event is ever visible, which
// is what makes it the more load-bearing of the two records, not the
// lesser one.
func TestScmCredentials_Shadow_SubstitutionIsRecorded(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) { r.readOnlyMinter = minter })
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/subst-owner/subst-repo","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Password != minter.Token.Value {
		t.Fatalf("Password = %q, want the substituted read-only token", got.Password)
	}

	ledgerReader := narvipg.NewShadowSCMWriteStore(rig.pool)
	rows, err := ledgerReader.ListForRepo(ctx, "subst-owner/subst-repo", 10)
	if err != nil {
		t.Fatalf("ListForRepo: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("shadow_scm_writes rows for subst-owner/subst-repo = %d, want 1: a served substitution must leave evidence", len(rows))
	}
	if rows[0].Operation != "scm_credential_substituted" {
		t.Errorf("rows[0].Operation = %q, want %q", rows[0].Operation, "scm_credential_substituted")
	}
	if rows[0].SessionID != session.ID {
		t.Errorf("rows[0].SessionID = %v, want the session that asked %v", rows[0].SessionID, session.ID)
	}
	if strings.Contains(string(rows[0].SpecJson), minter.Token.Value) {
		t.Errorf("rows[0].SpecJson = %s, must never carry the substituted token's own value", rows[0].SpecJson)
	}
}

// TestScmCredentials_Shadow_SubstitutionLedgerFailureIs500 holds the
// SUCCESS path to record-or-fail through the handler: a substitution that
// cannot be evidenced must not be served. The sandbox gets a 500, never a
// working read-only credential the ledger has no record of.
func TestScmCredentials_Shadow_SubstitutionLedgerFailureIs500(t *testing.T) {
	minter := newFakeReadOnlyMinter()
	rig := newTestRig(t, func(r *testRig) {
		r.readOnlyMinter = minter
		r.shadowLedger = failingLedgerStore{}
	})
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       []byte(`[{"name":"narvi","url":"https://github.com/narvidev/narvi","branch":null}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, got := postScmCredentials(t, rig, session.ID.String(), "sandbox-bearer-token")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (a substitution that cannot be recorded must fail loudly)", status, http.StatusInternalServerError)
	}
	if got.Password != "" {
		t.Errorf("Password = %q; no credential may be served when its substitution could not be recorded", got.Password)
	}
}

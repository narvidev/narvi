//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeGitHub stands in for GitHub's REST API, in process: installed as
// http.DefaultTransport before Build, it is what the composition root's
// transport-gated GitHub adapter captures (githubapi.NewGatedClient), so no
// request leaves the machine. It answers every request 404 -- each reader
// degrades on a failed read, and the request itself is the evidence -- and
// records the Authorization header of every call made to api.github.com.
type fakeGitHub struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.github.com" {
		f.mu.Lock()
		f.calls = append(f.calls, req.Method+" "+req.URL.Path+" "+req.Header.Get("Authorization"))
		f.mu.Unlock()
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)),
		Request:    req,
	}, nil
}

// takeCalls returns the calls recorded since the last take, and forgets them.
func (f *fakeGitHub) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// installFakeGitHub makes f the process's default transport for the rest
// of the test.
func installFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	prev := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = prev })
	return f
}

// lockedBuffer is a bytes.Buffer safe to write from the logger and read
// from the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, raw := range bytes.Split(b.buf.Bytes(), []byte("\n")) {
		var line map[string]any
		if json.Unmarshal(raw, &line) == nil {
			out = append(out, line)
		}
	}
	return out
}

// captureInfoLog sends slog's default logger, at Info, to a buffer for the
// rest of the test, and puts back slog's default and the standard log
// package's output and flags afterwards (captureWarnings' own reasoning,
// mcp_oauth_integration_test.go).
func captureInfoLog(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	prev, prevOutput, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})
	return b
}

// assertGitHubAxesLine checks Build's one boot line against the axes the
// Config it was given actually has.
func assertGitHubAxesLine(t *testing.T, b *lockedBuffer, wantIngress, wantOutbound bool, wantCredential string) {
	t.Helper()
	for _, line := range b.lines() {
		if line["msg"] != "narvi control-plane: GitHub axes" {
			continue
		}
		if line["ingress"] != wantIngress || line["outbound"] != wantOutbound || line["outbound_credential"] != wantCredential {
			t.Errorf("GitHub axes line = ingress=%v outbound=%v outbound_credential=%v, want ingress=%v outbound=%v outbound_credential=%v",
				line["ingress"], line["outbound"], line["outbound_credential"], wantIngress, wantOutbound, wantCredential)
		}
		return
	}
	t.Error("Build logged no \"narvi control-plane: GitHub axes\" line")
}

// TestBuild_GitHubOutboundReachesEveryOptionalReader proves the composition
// root hands cfg.GitHubOutbound -- not nil -- to every optional reader of
// the axis, which would otherwise degrade silently: with GitHub outbound on,
// each reader reaches GitHub (an in-process fake) as the bot. The review
// readout, the session result, the verdict tool and the re-review button
// are driven through the real router; the actor's two uses are read off the
// registry (sessionactor.Registry.HasGitHubOutbound), since they are
// otherwise reachable only through a timer or a spawned fix session.
func TestBuild_GitHubOutboundReachesEveryOptionalReader(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)
	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}
	if cfg.GitHubOutbound == nil {
		t.Fatal("default Config has GitHub outbound off -- this test would be vacuous")
	}
	const wantAuth = "Bearer test-github-bot-token"
	github := installFakeGitHub(t)

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.registry.Shutdown() })
	server := httptest.NewServer(app.Router)
	t.Cleanup(server.Close)

	if !app.registry.HasGitHubOutbound() {
		t.Error("the session actor registry was built without GitHub outbound (RegistryOptions.GitHubOutbound)")
	}

	ctx := context.Background()
	cookie := createMaintainerSession(ctx, t, pool)
	const repoFullName, prNumber = "acme/outbound-wiring", int32(5)
	session := seedAssessedReviewSession(ctx, t, pool, repoFullName, prNumber)

	expectBotCall := func(t *testing.T, step string) {
		t.Helper()
		calls := github.takeCalls()
		for _, c := range calls {
			if strings.HasSuffix(c, " "+wantAuth) {
				return
			}
		}
		t.Errorf("%s made no GitHub call as the bot (calls: %v) -- the composition root did not hand it cfg.GitHubOutbound", step, calls)
	}
	do := func(t *testing.T, method, path string, body io.Reader, header http.Header) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, body)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for k, v := range header {
			req.Header[k] = v
		}
		// The test's own requests go through a plain transport, never the
		// fake that stands in for GitHub.
		resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	cookieHeader := http.Header{"Cookie": []string{platform.AuthSessionCookieName + "=" + cookie}}
	github.takeCalls()

	if status := do(t, http.MethodGet, "/api/sessions/"+session.String()+"/review", nil, cookieHeader); status != http.StatusOK {
		t.Fatalf("GET review = %d, want 200", status)
	}
	expectBotCall(t, "the review readout (GET .../review)")

	if status := do(t, http.MethodGet, "/api/sessions/"+session.String()+"/result", nil, cookieHeader); status != http.StatusOK {
		t.Fatalf("GET result = %d, want 200", status)
	}
	expectBotCall(t, "the session result's freshness read (GET .../result)")

	const bearer, dispatchID = "outbound-wiring-sandbox-bearer", "outbound-wiring-dispatch"
	seedVerdictToolTurn(ctx, t, pool, session, bearer, dispatchID, "h1")
	verdictHeader := http.Header{
		"Content-Type":                  []string{"application/json"},
		"Authorization":                 []string{"Bearer " + bearer},
		"X-Sandbox-Gen":                 []string{"1"},
		"X-Sandbox-Dispatch-Message-Id": []string{dispatchID},
	}
	if status := do(t, http.MethodPost, "/sessions/"+session.String()+"/review/verdict", strings.NewReader(verdictWithOneFinding(t)), verdictHeader); status != http.StatusCreated {
		t.Fatalf("POST review/verdict = %d, want 201", status)
	}
	expectBotCall(t, "the verdict tool's finding anchoring (POST .../review/verdict)")

	if status := do(t, http.MethodPost, "/api/sessions/"+session.String()+"/review/retrigger", nil, cookieHeader); status != http.StatusCreated {
		t.Fatalf("POST review/retrigger = %d, want 201", status)
	}
	expectBotCall(t, "the re-review button's prefetch (POST .../review/retrigger)")
}

// createMaintainerSession creates a maintainer and a signed-in session for
// them, returning the session cookie's value.
func createMaintainerSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "outbound-wiring@example.com",
		DisplayName:  "Outbound Wiring Test Maintainer",
		Role:         sqlcgen.UserRoleMaintainer,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := narvipg.NewUserSessionStore(pool).Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	return token
}

// seedAssessedReviewSession creates the review session of repoFullName#n
// whose newest review attempt ended with a verdict at head h1 -- a result
// whose freshness is read live.
func seedAssessedReviewSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string, n int32) pgtype.UUID {
	t.Helper()
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	claims := narvipg.NewGitHubPRSessionStore(pool)

	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create review session: %v", err)
	}
	if err := claims.EnsureRow(ctx, repoFullName, n); err != nil {
		t.Fatalf("ensure claim: %v", err)
	}
	if err := claims.SetSessionID(ctx, repoFullName, n, sess.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	attempt, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create review attempt: %v", err)
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	for _, arg := range []sqlcgen.UpdateTurnStatusParams{
		{ID: attempt.ID, Status: sqlcgen.TurnStatusDispatched, DispatchedAt: now},
		{ID: attempt.ID, Status: sqlcgen.TurnStatusProcessing},
		{ID: attempt.ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: now},
	} {
		if _, err := turns.UpdateStatus(ctx, arg); err != nil {
			t.Fatalf("drive review attempt to %s: %v", arg.Status, err)
		}
	}
	baseRef, baseSHA := "main", "b1"
	if _, err := narvipg.NewReviewVerdictStore(pool).Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName: repoFullName, PrNumber: n, HeadSha: "h1",
		RiskLevel: "low", Premise: "ok", BlastRadius: []byte(`[]`), FilesChanged: 1,
		TestsCoverage: "adequate", DocsDrift: "none", ProposedShippable: "auto", Shippable: "auto",
		SessionID: sess.ID, ArchDecisionTags: []byte(`[]`), ArchDecisionRoots: []byte(`[]`), AncestorChain: []byte(`[]`),
		BaseRef: &baseRef, BaseSha: &baseSHA, PolicyVersion: 1, AttemptID: attempt.ID,
	}); err != nil {
		t.Fatalf("insert verdict: %v", err)
	}
	return sess.ID
}

// seedVerdictToolTurn gives sessionID a sandbox answering to bearer and a
// processing review turn dispatched as dispatchID, pinned to headSHA -- what
// the verdict tool needs to accept a verdict and re-anchor its findings.
func seedVerdictToolTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, bearer, dispatchID, headSHA string) {
	t.Helper()
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sum := sha256.Sum256([]byte(bearer))
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET token_hash = $2 WHERE session_id = $1`, sessionID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("set sandbox token hash: %v", err)
	}
	turns := narvipg.NewTurnStore(pool)
	turn, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	messageID := dispatchID
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &messageID}); err != nil {
		t.Fatalf("stamp dispatched message id: %v", err)
	}
}

// verdictWithOneFinding is a light-path verdict carrying one finding, so the
// verdict tool re-fetches the diff to anchor it.
func verdictWithOneFinding(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"riskLevel":         "low",
		"premise":           "ok",
		"blastRadius":       []string{},
		"filesChanged":      1,
		"testsCoverage":     "adequate",
		"docsDrift":         "none",
		"proposedShippable": "auto",
		"summary":           "Outbound wiring verdict.",
		"findings": []map[string]any{
			{"severity": "medium", "filePath": "main.go", "description": "the loop reads past the end of items"},
		},
		"digest": map[string]any{
			"summary":             "Reworks the loop.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body describes this change.",
		},
		"factCheck":       "done",
		"factCheckKilled": 0,
	})
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	return string(body)
}

//go:build integration

// Integration tests for ListDecisionInbox/MergePullRequest (
// "decision inbox: read model + API", §16) against a REAL Postgres
// instance -- gated behind the "integration" build tag. Deliberately a
// SELF-CONTAINED router (auth.Middleware + these two routes only) rather
// than extending this package's own shared testRig (httpapi_integration_
// test.go): testRig is a large, heavily-shared fixture many other tests
// already depend on, and these two handlers need nothing from it beyond
// the SAME shared Postgres pool (newTestPool, already in scope in this
// package) and the SAME auth.Middleware every other route already uses.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// decisionInboxTokenKey is a fixed, valid 32-byte AES key -- mirrors
// httpapi_integration_test.go's own tokenEncryptionKey literal exactly
// ("01234567890123456789012345678901", 32 bytes).
var decisionInboxTokenKey = []byte("01234567890123456789012345678901")

// decisionInboxTestRig is this file's own small, self-contained fixture --
// see this file's own top doc comment for why it does not reuse testRig.
type decisionInboxTestRig struct {
	pool                  *pgxpool.Pool
	users                 *narvipg.UserStore
	userSessions          *narvipg.UserSessionStore
	identities            *narvipg.IdentityStore
	sessions              *narvipg.SessionStore
	artifacts             *narvipg.ArtifactStore
	auditLog              *narvipg.AuditLogStore
	reviewVerdicts        *narvipg.ReviewVerdictStore
	githubPRSessions      *narvipg.GitHubPRSessionStore
	releaseManifestChecks *narvipg.ReleaseManifestCheckStore
	server                *httptest.Server
}

// seedAutoApprovedVerdict inserts a Shippable=auto review_verdicts row at
// headSHA -- (§21.1/§21.2): the REAL auto-approval eligibility
// engine now requires one before any PR classifies ready_to_merge or
// re-validates eligible at merge/click time, mirroring internal/app/
// decisioninbox's own identical seedAutoApprovedVerdict helper
// (aggregate_integration_test.go) -- see that helper's own doc comment
// for why this must be genuinely present, never assumed, in a fixture
// this file claims is "otherwise fully eligible".
func (rig *decisionInboxTestRig) seedAutoApprovedVerdict(ctx context.Context, t *testing.T, repoFullName string, prNumber int32, headSHA string) {
	t.Helper()
	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if _, err := appreviewverdict.Insert(ctx, rig.reviewVerdicts, narvipg.NewRepoSettingsStore(rig.pool), false, repoFullName, prNumber, headSHA, pgtype.UUID{}, verdict, reviewpost.Digest{Summary: "Test-seeded verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil, "", false); err != nil {
		t.Fatalf("seed auto-approved review_verdicts row for %s#%d: %v", repoFullName, prNumber, err)
	}
}

// fakeMergeSourceControl is a minimal test-only ports.SourceControl,
// configurable per test -- mirrors reviewfindings_integration_test.go's
// own applySuggestionFakeSourceControl precedent, narrowed to exactly the
// two methods decisioninbox.RevalidateForMerge/Build actually call
// (ListOpenPRsForUser, MergePR); every other method returns a plain "not
// implemented" error.
type fakeMergeSourceControl struct {
	openPRs    []ports.OpenPR
	mergeSHA   string
	mergeErr   error
	mergeCalls []ports.MergePRSpec

	// listOpenPRsCalls counts every ListOpenPRsForUser call this fake
	// receives -- a viewer, unconditionally denied ActionMergePR, must be
	// rejected at the cheap
	// role-only pre-check BEFORE the expensive live SCM re-validation ever
	// calls this method at all.
	listOpenPRsCalls int

	// truncated is threaded straight through to ListOpenPRsForUser's own
	// return -- ports.SourceControl.ListOpenPRsForUser's own "one of
	// GitHub's two underlying search queries itself failed while the
	// other still returned a real, if incomplete, result" signal
	// (scmcache.go's own doc comment on its identically-named return).
	// Every existing test in this file leaves this at its bool
	// zero-value (false, an ordinary complete fetch) -- only
	// TestListDecisionInbox_ScmFetchFailedWithRealScmAsOf sets it, to
	// drive a genuine partial-fetch read through the real SCMCache/Build
	// pipeline, never a hand-built response.
	truncated bool
}

var _ ports.SourceControl = (*fakeMergeSourceControl)(nil)

func (f *fakeMergeSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	f.listOpenPRsCalls++
	return f.openPRs, f.truncated, nil
}
func (f *fakeMergeSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, nil
}
func (f *fakeMergeSourceControl) MergePR(_ context.Context, spec ports.MergePRSpec) (string, error) {
	f.mergeCalls = append(f.mergeCalls, spec)
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	return f.mergeSHA, nil
}
func (f *fakeMergeSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("not implemented")
}
func (f *fakeMergeSourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "", "", errors.New("not implemented")
}
func (f *fakeMergeSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("not implemented")
}
func (f *fakeMergeSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("not implemented")
}
func (f *fakeMergeSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("not implemented")
}
func (f *fakeMergeSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeMergeSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("not implemented")
}
func (f *fakeMergeSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("not implemented")
}
func (f *fakeMergeSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("not implemented")
}
func (f *fakeMergeSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("not implemented")
}
func (f *fakeMergeSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("not implemented")
}
func (f *fakeMergeSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	// §21.2 stage 2: this fake backs the HUMAN-clicked Merge
	// endpoint's own tests (MergePullRequest -> RevalidateForMerge),
	// which never calls GetOpenPR at all (that is RevalidateForAutoMerge's
	// own machine-caller primitive) -- not implemented is the correct,
	// never-exercised default here.
	return ports.OpenPR{}, false, errors.New("not implemented")
}

func newDecisionInboxTestRig(t *testing.T, sourceControl ports.SourceControl) *decisionInboxTestRig {
	t.Helper()
	pool := newTestPool(t)

	users := narvipg.NewUserStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)
	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	githubPRSessions := narvipg.NewGitHubPRSessionStore(pool)
	releaseManifestChecks := narvipg.NewReleaseManifestCheckStore(pool)

	deps := decisioninbox.Deps{
		Plans:                 narvipg.NewPlanStore(pool),
		Sessions:              sessions,
		Participants:          narvipg.NewParticipantStore(pool),
		Automations:           narvipg.NewAutomationStore(pool),
		Outbox:                narvipg.NewOutboxStore(pool, false),
		ReviewFindings:        reviewFindings,
		SentinelFixes:         narvipg.NewSentinelFixStore(pool),
		Artifacts:             artifacts,
		Identities:            identities,
		GitHubPRSessions:      githubPRSessions,
		ReleaseManifestChecks: releaseManifestChecks,
		SCMCache:              decisioninbox.NewSCMCache(sourceControl, platform.DefaultTimeouts()),
		TokenEncryptionKey:    decisionInboxTokenKey,
		Timeouts:              platform.DefaultTimeouts(),
		// (§21.1/§21.2): the REAL auto-approval eligibility
		// engine's own store dependencies.
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts:       reviewVerdicts,
			RepoSettings:         narvipg.NewRepoSettingsStore(pool),
			ReviewFindings:       reviewFindings,
			AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts:             platform.DefaultTimeouts(),
		},
	}
	auditLog := narvipg.NewAuditLogStore(pool)

	router := chi.NewRouter()
	router.Route("/api/decision-inbox", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Get("/", httpapi.ListDecisionInbox(deps))
		r.Post("/merge", httpapi.MergePullRequest(deps, sourceControl, auditLog))
	})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &decisionInboxTestRig{
		pool: pool, users: users, userSessions: userSessions, identities: identities,
		sessions: sessions, artifacts: artifacts, auditLog: auditLog, reviewVerdicts: reviewVerdicts,
		githubPRSessions: githubPRSessions, releaseManifestChecks: releaseManifestChecks, server: server,
	}
}

// createAuthenticatedUser mirrors testRig.createAuthenticatedUser
// (httpapi_integration_test.go) exactly, duplicated here since this file
// deliberately does not construct a full testRig (this file's own top doc
// comment).
func (rig *decisionInboxTestRig) createAuthenticatedUser(ctx context.Context, t *testing.T, role sqlcgen.UserRole) (sqlcgen.User, string) {
	t.Helper()
	unique := fmt.Sprintf("decisioninbox-test-%d", time.Now().UnixNano())
	user, err := rig.users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: unique + "@example.com", DisplayName: "Test User", Role: role})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := rig.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create user session: %v", err)
	}
	return user, token
}

func (rig *decisionInboxTestRig) linkGitHub(ctx context.Context, t *testing.T, userID pgtype.UUID, externalID string) {
	t.Helper()
	encrypted, err := platform.EncryptToken(decisionInboxTokenKey, []byte("fake-gh-token"))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if _, err := rig.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: userID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: externalID,
		EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}
}

// markPlatformAuthored records a 'pr'-typed artifact for htmlURL, backed
// by a freshly created session -- the durable signal isPlatformAuthored
// (internal/app/decisioninbox) checks before ever classifying a PR
// ready_to_merge.
func (rig *decisionInboxTestRig) markPlatformAuthored(ctx context.Context, t *testing.T, createdBy pgtype.UUID, htmlURL string) {
	t.Helper()
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: createdBy})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := rig.artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}
}

func (rig *decisionInboxTestRig) doJSON(t *testing.T, method, path string, body []byte, v any, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, rig.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp.StatusCode
}

func TestListDecisionInbox_RequiresAuth(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})

	status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestListDecisionInbox_EmptyForFreshUser(t *testing.T) {
	rig := newDecisionInboxTestRig(t, &fakeMergeSourceControl{})
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)

	var got restdtos.ListDecisionInboxResponse
	status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.Items) != 0 {
		t.Errorf("Items = %+v, want empty for a fresh user with no linked GitHub identity and no plans", got.Items)
	}
	if got.ScmAsOf != nil {
		t.Errorf("ScmAsOf = %v, want nil (no linked GitHub identity)", *got.ScmAsOf)
	}
	if got.DecisionLatencyComputed {
		t.Error("DecisionLatencyComputed = true, want false (no decisions in the window yet)")
	}
}

// TestListDecisionInbox_ScmFetchFailedWithRealScmAsOf_GenuinePartialFetch
// is §16's own decision-inbox partial-fetch state, driven end to end
// through this package's real HTTP route, decisioninbox.Build, and the
// real *decisioninbox.SCMCache -- never a hand-built
// restdtos.ListDecisionInboxResponse. Result.SCMFetchFailed's own doc
// comment on aggregate.go documents this exact combination (scmAsOf
// non-nil AND scmFetchFailed true) as a genuine, real state -- "a
// partial-but-real fetch can legitimately carry both a real as-of
// instant and a flag telling the caller not to present the rows present
// as complete" -- but before this test, nothing exercised it beyond
// internal/app/decisioninbox's own TestBuild_SCMFetchFailedSignal, which
// calls decisioninbox.Build directly (the app layer only); this is the
// same real condition, one layer further out, proving the wire response
// this endpoint actually returns carries both fields together, not just
// the Result the app layer builds internally.
//
// The ONLY fake in this test is fakeMergeSourceControl itself, at the
// outermost ports.SourceControl seam -- truncated:true is exactly what a
// real githubapi.Adapter would return had one of its own two paginated
// GitHub search queries failed while the other still returned a real,
// if incomplete, page (scmcache.go's own doc comment); everything
// between that seam and the JSON this test decodes (SCMCache's TTL
// cache, buildPRItems' degraded-folding, decisionInboxResultToDTO's own
// wire mapping) is the real, unmodified production code path.
func TestListDecisionInbox_ScmFetchFailedWithRealScmAsOf_GenuinePartialFetch(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1500"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1500, Title: "partial fetch", HTMLURL: htmlURL,
				HeadSHA: "headsha1500", Assignees: []ports.PRPerson{{ExternalID: "9007", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
		truncated: true,
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9007")

	var got restdtos.ListDecisionInboxResponse
	status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got.ScmAsOf == nil {
		t.Error("ScmAsOf = nil, want a real timestamp -- the fetch itself succeeded (truncated is a PARTIAL result, not a failed one)")
	} else if got.ScmAsOf.IsZero() {
		t.Error("ScmAsOf is the zero time, want a real fetch-completion instant")
	}
	if !got.ScmFetchFailed {
		t.Error("ScmFetchFailed = false, want true -- a truncated GitHub read must not be presented as a complete one")
	}
	// The partial batch still surfaces what it DID get -- a degraded
	// fetch must never suppress the rows it successfully read, only flag
	// them as possibly incomplete via ScmFetchFailed above.
	found := false
	for _, it := range got.Items {
		if it.PrNumber != nil && *it.PrNumber == 1500 {
			found = true
		}
	}
	if !found {
		t.Errorf("PR #1500 missing from a partial-but-real fetch's own Items: %+v", got.Items)
	}
}

func TestMergePullRequest_HappyPath(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1204"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1204, Title: "low risk", HTMLURL: htmlURL,
				HeadSHA: "headsha1204", Assignees: []ports.PRPerson{{ExternalID: "9001", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
		mergeSHA: "merged-sha-abc",
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9001")
	rig.markPlatformAuthored(ctx, t, user.ID, htmlURL)
	rig.seedAutoApprovedVerdict(ctx, t, "acme/widgets", 1204, "headsha1204")

	// §30.7 stamps each recorded auto-approval outcome with the egress
	// mode that held when it was observed, and the calibration query
	// excludes shadow ones. A repo with no repo_settings row resolves
	// shadow, so arm it BEFORE the merge -- a stamp already written
	// cannot be changed afterwards. This test is about whether a
	// 'confirmed' outcome is recorded, not about egress mode.
	if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertLiveEgressEnabled(ctx, "acme/widgets", true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: "acme/widgets", PrNumber: 1204})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var got restdtos.MergePullRequestResponse
	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if !got.Merged || got.MergeCommitSha != "merged-sha-abc" {
		t.Errorf("response = %+v, want a successful merge with the fake's own sha", got)
	}
	if len(fakeSCM.mergeCalls) != 1 || fakeSCM.mergeCalls[0].HeadSHA != "headsha1204" {
		t.Errorf("MergePR calls = %+v, want exactly one call with HeadSHA=headsha1204 (the freshly revalidated head, never a stale/client-supplied one)", fakeSCM.mergeCalls)
	}

	// This human 1-click merge path must now record a 'confirmed'
	// auto-approval outcome -- previously, RecordConfirmed's only real caller was the armed auto-merge
	// worker, so this endpoint (the ONE merge path every unarmed repo
	// actually uses, for the entire calibration window §21.2 names this
	// metric as existing to inform) never contributed a single
	// 'confirmed' row.
	total, contested, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, "acme/widgets", pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	if total != 1 || contested != 0 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (1, 0) -- the human 1-click merge must record a 'confirmed' outcome", total, contested)
	}
}

// TestMergePullRequest_ShadowSuppressed_RecordedNotMerged pins §30.7's
// mapping for the human path. An operator who clicks Merge on a repository
// whose egress is suppressed must be told what happened -- not shown an
// error, which would say the platform broke, and not shown a success,
// which would say the pull request is merged when it is open.
//
// The review that found this had the endpoint returning HTTP 500
// "internal error", because the sentinel fell through to the generic
// branch. The sentinel existed and no caller handled it.
func TestMergePullRequest_ShadowSuppressed_RecordedNotMerged(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1204"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1204, Title: "low risk", HTMLURL: htmlURL,
				HeadSHA: "headsha1204", Assignees: []ports.PRPerson{{ExternalID: "9001", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
		mergeErr: ports.ErrShadowSuppressed,
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9001")
	rig.markPlatformAuthored(ctx, t, user.ID, htmlURL)
	rig.seedAutoApprovedVerdict(ctx, t, "acme/widgets", 1204, "headsha1204")

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: "acme/widgets", PrNumber: 1204})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var got restdtos.MergePullRequestResponse
	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d -- a suppressed merge is neither an error nor a failure of the merge", status, http.StatusOK)
	}
	if got.Merged {
		t.Error("response says Merged=true for a suppressed merge; the pull request is still open")
	}
	if got.MergeCommitSha != "" {
		t.Errorf("response carries a merge SHA %q for a merge that never happened", got.MergeCommitSha)
	}

	// And deliberately NO confirmed outcome: feeding a confirmation the
	// world never saw into the contradiction-rate read model would corrupt
	// the instrument whose evidence justifies arming auto-merge for real.
	total, contested, err := narvipg.NewAutoApprovalOutcomeStore(rig.pool).CountInWindow(ctx, "acme/widgets", pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("count auto-approval outcomes: %v", err)
	}
	if total != 0 || contested != 0 {
		t.Errorf("outcome counts = (total=%d, contested=%d), want (0, 0) -- a suppressed merge must not record a confirmation", total, contested)
	}
}

func TestMergePullRequest_NotAssignedToCaller(t *testing.T) {
	fakeSCM := &fakeMergeSourceControl{} // no open PRs for anyone
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9001")

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: "acme/widgets", PrNumber: 999})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var errBody map[string]string
	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, &errBody, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %+v)", status, http.StatusConflict, errBody)
	}
	if len(fakeSCM.mergeCalls) != 0 {
		t.Errorf("MergePR called %d times, want 0 -- revalidation must reject before ever calling MergePR", len(fakeSCM.mergeCalls))
	}
}

func TestMergePullRequest_NotPlatformAuthored(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1205"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1205, Title: "human PR", HTMLURL: htmlURL,
				HeadSHA: "headsha1205", Assignees: []ports.PRPerson{{ExternalID: "9001", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9001")
	// Deliberately NO markPlatformAuthored call -- this PR was not opened
	// by any Narvi session.

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: "acme/widgets", PrNumber: 1205})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, nil, token)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
	if len(fakeSCM.mergeCalls) != 0 {
		t.Errorf("MergePR called %d times, want 0", len(fakeSCM.mergeCalls))
	}
}

// TestMergePullRequest_Viewer_Returns403 proves authz.Authorize
// (ActionMergePR) actually gates this endpoint end to end -- every OTHER merge test in this file
// authenticates as Member, so a deleted/bypassed RBAC gate would pass the
// whole suite; §16.2's own "Viewer role sees the queue read-only" would
// have no executable guard at all. Mirrors this repo's own established
// TestApprovePlan_Viewer_NotOwnerOrParticipant_Returns403 convention
// (planapprove_integration_test.go), which the merge endpoint uniquely
// lacked.
//
// Also proves the same regression guard in the same request applies here:
// a viewer must be rejected by the cheap, role-only authz
// pre-check BEFORE the expensive live SCM re-validation ever runs --
// ListOpenPRsForUser must never be called at all for a role
// unconditionally denied ActionMergePR.
func TestMergePullRequest_Viewer_Returns403(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1206"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1206, Title: "viewer attempt", HTMLURL: htmlURL,
				HeadSHA: "headsha1206", Assignees: []ports.PRPerson{{ExternalID: "9003", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	viewer, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleViewer)
	rig.linkGitHub(ctx, t, viewer.ID, "9003")
	rig.markPlatformAuthored(ctx, t, viewer.ID, htmlURL)

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: "acme/widgets", PrNumber: 1206})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, nil, token)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", status, http.StatusForbidden)
	}
	if len(fakeSCM.mergeCalls) != 0 {
		t.Errorf("MergePR called %d times, want 0 -- a viewer must never reach the actual merge call", len(fakeSCM.mergeCalls))
	}
	if fakeSCM.listOpenPRsCalls != 0 {
		t.Errorf("ListOpenPRsForUser called %d times, want 0 (a viewer must be rejected by the cheap role-only pre-check BEFORE the expensive live SCM re-validation ever runs)", fakeSCM.listOpenPRsCalls)
	}
}

// TestMergePullRequest_MergePRErrorStatusMapping covers the merge
// handler's own ports.MergePRError status mapping end to end: 405 (not currently mergeable) and 409 (the PR changed
// since it was last checked -- GitHub's own optimistic-concurrency
// signal, exactly the HeadSHA-moved race this endpoint's own re-
// validation exists to catch) both map to 409; any OTHER GitHub status
// maps to 502. fakeMergeSourceControl.mergeErr was declared and read by
// production code but set by NO test before this one -- this whole
// switch was unexercised. Also asserts NO audit_log row is ever written
// on a failed merge (auditlog.Record is only ever reached after a
// SUCCESSFUL SourceControl.MergePR call in the handler's own sequence),
// especially for the 409 case, where a naive implementation might be
// tempted to log "attempted" regardless.
func TestMergePullRequest_MergePRErrorStatusMapping(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/5001"
	const repoFullName = "acme/widgets"
	const prNumber = 5001
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: prNumber, Title: "status mapping", HTMLURL: htmlURL,
				HeadSHA: "headsha5001", Assignees: []ports.PRPerson{{ExternalID: "9004", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9004")
	rig.markPlatformAuthored(ctx, t, user.ID, htmlURL)
	rig.seedAutoApprovedVerdict(ctx, t, repoFullName, prNumber, "headsha5001")

	body, err := json.Marshal(restdtos.MergePullRequestRequest{RepoFullName: repoFullName, PrNumber: prNumber})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	tests := []struct {
		name       string
		mergeErr   error
		wantStatus int
	}{
		{"NotMergeable405MapsTo409", &ports.MergePRError{Status: http.StatusMethodNotAllowed, Message: "not mergeable"}, http.StatusConflict},
		{"HeadSHAMoved409MapsTo409", &ports.MergePRError{Status: http.StatusConflict, Message: "sha mismatch"}, http.StatusConflict},
		{"OtherGitHubStatusMapsTo502", &ports.MergePRError{Status: http.StatusInternalServerError, Message: "github had a bad day"}, http.StatusBadGateway},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeSCM.mergeErr = tc.mergeErr
			fakeSCM.mergeCalls = nil

			status := rig.doJSON(t, http.MethodPost, "/api/decision-inbox/merge", body, nil, token)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if len(fakeSCM.mergeCalls) != 1 {
				t.Fatalf("MergePR called %d times, want 1", len(fakeSCM.mergeCalls))
			}

			entries, err := rig.auditLog.List(ctx, 100, 0)
			if err != nil {
				t.Fatalf("list audit log: %v", err)
			}
			wantResourceID := fmt.Sprintf("%s#%d", repoFullName, prNumber)
			for _, e := range entries {
				if e.ResourceID == wantResourceID {
					t.Errorf("audit log row written for a FAILED merge (%+v), want none", e)
				}
			}
		})
	}
}

// TestListDecisionInbox_HandoffPR_FieldsPopulated is the DTO-mapping half
// of the handoff-PR-fields invariant (the domain-Item half is covered
// separately in aggregate_integration_test.go's
// TestBuild_PRLabelVariations): a
// handoff-labeled PR's ciGreen/findings/isHandoff/hasApprovingReview/
// hasChangesRequested must all render non-null over the wire even though
// it rides kind=awaiting_approval instead of an ordinary ready_to_merge/
// needs_review kind -- decisionInboxItemToDTO's own gate used to be keyed
// on Kind, which nulled exactly these fields for exactly this row.
//
// HasApprovingReview/HasChangesRequested are deliberately set to TRUE in
// the fixture below ("HasApprovingReview is
// asserted non-nil only, never for its value" -- a fixture that leaves
// both at their bool zero-value cannot tell "populated with the real
// value" apart from "always renders false regardless of input"). Both
// fields are independent GitHub review facts and can legitimately be
// true at once (different reviewers).
func TestListDecisionInbox_HandoffPR_FieldsPopulated(t *testing.T) {
	const htmlURL = "https://github.com/acme/widgets/pull/1300"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1300, Title: "prototype: handoff", HTMLURL: htmlURL,
				HeadSHA: "headsha1300", Assignees: []ports.PRPerson{{ExternalID: "9005", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk", "handoff"},
				HasApprovingReview: true, HasChangesRequested: true,
			},
		},
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9005")

	var got restdtos.ListDecisionInboxResponse
	status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var row *restdtos.DecisionInboxItem
	for i := range got.Items {
		if got.Items[i].PrNumber != nil && *got.Items[i].PrNumber == 1300 {
			row = &got.Items[i]
		}
	}
	if row == nil {
		t.Fatalf("PR #1300 missing from the response entirely: %+v", got.Items)
	}
	if row.Kind != restdtos.DecisionInboxItemKindAwaitingApproval {
		t.Errorf("Kind = %q, want awaiting_approval", row.Kind)
	}
	if row.IsHandoff == nil || !*row.IsHandoff {
		t.Errorf("IsHandoff = %v, want a non-nil pointer to true", row.IsHandoff)
	}
	if row.CiGreen == nil || !*row.CiGreen {
		t.Errorf("CiGreen = %v, want a non-nil pointer to true", row.CiGreen)
	}
	if row.Findings == nil {
		t.Error("Findings = nil, want a non-nil pointer (even if the count is 0)")
	}
	if row.HasApprovingReview == nil || !*row.HasApprovingReview {
		t.Errorf("HasApprovingReview = %v, want a non-nil pointer to TRUE (fixture sets HasApprovingReview: true -- previously this was asserted non-nil only, never for its real value)", row.HasApprovingReview)
	}
	if row.HasChangesRequested == nil || !*row.HasChangesRequested {
		t.Errorf("HasChangesRequested = %v, want a non-nil pointer to TRUE (this DTO field previously did not exist on the wire at all)", row.HasChangesRequested)
	}
}

// claimReviewSession mirrors coalesce.go's own atomic
// EnsureRow+LockForUpdate+SetSessionID claim sequence -- the one
// legitimate way a github_pr_sessions row is ever produced, reused here
// so this file's fixtures stay faithful to the real write path rather
// than a raw INSERT.
func (rig *decisionInboxTestRig) claimReviewSession(ctx context.Context, t *testing.T, repoFullName string, prNumber int32) pgtype.UUID {
	t.Helper()
	tx, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txStore := rig.githubPRSessions.WithTx(tx)
	if err := txStore.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("EnsureRow: %v", err)
	}
	if _, err := txStore.LockForUpdate(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("LockForUpdate: %v", err)
	}
	created, err := rig.sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create review session: %v", err)
	}
	if err := txStore.SetSessionID(ctx, repoFullName, prNumber, created.ID); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim tx: %v", err)
	}
	return created.ID
}

// TestListDecisionInbox_SessionIdAndReleaseCut_FieldsPopulated is the
// wire-level proof for the two gaps closed on top of
// TestListDecisionInbox_HandoffPR_FieldsPopulated's own baseline: a PR
// Narvi has actually reviewed carries its own real sessionId on the wire
// (never a stale/absent one), a PR Narvi has never been mentioned on
// renders sessionId null, and a release cut renders isRelease/
// manifestFindingsCount/aggregateReviewTriggered -- all three null for an
// ordinary PR row, never a fabricated zero/false standing in for "not a
// release cut at all".
func TestListDecisionInbox_SessionIdAndReleaseCut_FieldsPopulated(t *testing.T) {
	const repo = "acme/rockets"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{Owner: "acme", Repo: "rockets", Number: 1400, Title: "mentioned PR", HTMLURL: "https://github.com/acme/rockets/pull/1400",
				HeadSHA: "sha1400", Assignees: []ports.PRPerson{{ExternalID: "9100", Login: "octocat"}}, CIConclusion: ports.CIConclusionSuccess},
			{Owner: "acme", Repo: "rockets", Number: 1401, Title: "never mentioned PR", HTMLURL: "https://github.com/acme/rockets/pull/1401",
				HeadSHA: "sha1401", Assignees: []ports.PRPerson{{ExternalID: "9100", Login: "octocat"}}, CIConclusion: ports.CIConclusionSuccess},
			{Owner: "acme", Repo: "rockets", Number: 1402, Title: "release/2026.09.01 -- 1 PR", HTMLURL: "https://github.com/acme/rockets/pull/1402",
				HeadSHA: "sha1402", Assignees: []ports.PRPerson{{ExternalID: "9100", Login: "octocat"}}, CIConclusion: ports.CIConclusionSuccess},
		},
	}
	rig := newDecisionInboxTestRig(t, fakeSCM)
	ctx := context.Background()

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9100")

	session1400 := rig.claimReviewSession(ctx, t, repo, 1400)
	// PR #1401 is deliberately left unclaimed.
	session1402 := rig.claimReviewSession(ctx, t, repo, 1402)
	if _, err := rig.releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID: session1402, RepoFullName: repo, PrNumber: 1402, BaseRef: "main", HeadRef: "release/2026.09.01",
		ConstituentPrCount: 1, CoveragePartial: false, AggregateReviewTriggered: false,
		AggregateReviewTriggerReasons: []byte(`[]`),
		Findings:                      []byte(`[{"kind":"admin_override","prNumber":50,"prTitle":"hotfix","detail":"merged via admin override"}]`),
		MergedPrs:                     []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert release manifest check: %v", err)
	}

	var got restdtos.ListDecisionInboxResponse
	status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	byPR := map[int]*restdtos.DecisionInboxItem{}
	for i := range got.Items {
		if got.Items[i].PrNumber != nil {
			byPR[*got.Items[i].PrNumber] = &got.Items[i]
		}
	}

	pr1400 := byPR[1400]
	if pr1400 == nil {
		t.Fatalf("PR #1400 missing from the response entirely: %+v", got.Items)
	}
	if pr1400.SessionId == nil || *pr1400.SessionId != session1400.String() {
		t.Errorf("PR #1400 SessionId = %v, want a non-nil pointer to %q (its own claimed review session)", pr1400.SessionId, session1400.String())
	}
	if pr1400.IsRelease == nil || *pr1400.IsRelease {
		t.Errorf("PR #1400 IsRelease = %v, want a non-nil pointer to false", pr1400.IsRelease)
	}
	if pr1400.ManifestFindingsCount != nil {
		t.Errorf("PR #1400 ManifestFindingsCount = %v, want nil (not a release cut)", pr1400.ManifestFindingsCount)
	}
	if pr1400.AggregateReviewTriggered != nil {
		t.Errorf("PR #1400 AggregateReviewTriggered = %v, want nil (not a release cut)", pr1400.AggregateReviewTriggered)
	}

	pr1401 := byPR[1401]
	if pr1401 == nil {
		t.Fatalf("PR #1401 missing from the response entirely: %+v", got.Items)
	}
	if pr1401.SessionId != nil {
		t.Errorf("PR #1401 (never mentioned) SessionId = %v, want nil -- never presenting an 'Open review' action this backend cannot serve", *pr1401.SessionId)
	}

	pr1402 := byPR[1402]
	if pr1402 == nil {
		t.Fatalf("PR #1402 missing from the response entirely: %+v", got.Items)
	}
	if pr1402.Kind != restdtos.DecisionInboxItemKindNeedsReview {
		t.Errorf("PR #1402 Kind = %q, want needs_review", pr1402.Kind)
	}
	if pr1402.IsRelease == nil || !*pr1402.IsRelease {
		t.Errorf("PR #1402 IsRelease = %v, want a non-nil pointer to true", pr1402.IsRelease)
	}
	if pr1402.ManifestFindingsCount == nil || *pr1402.ManifestFindingsCount != 1 {
		t.Errorf("PR #1402 ManifestFindingsCount = %v, want a non-nil pointer to 1", pr1402.ManifestFindingsCount)
	}
	if pr1402.AggregateReviewTriggered == nil || *pr1402.AggregateReviewTriggered {
		t.Errorf("PR #1402 AggregateReviewTriggered = %v, want a non-nil pointer to false", pr1402.AggregateReviewTriggered)
	}
	if pr1402.SessionId == nil || *pr1402.SessionId != session1402.String() {
		t.Errorf("PR #1402 SessionId = %v, want a non-nil pointer to %q (the review session that produced its own manifest check)", pr1402.SessionId, session1402.String())
	}
}

// TestListDecisionInbox_FindingsUnknownRendersNullNotTheFailClosedSentinel
// is the wire-level regression test proving that the fail-closed sentinel
// (openFindingsUnknownFailClosed, the
// synthetic value 1) exists ONLY to fail buildPROpenItem's own eligibility
// computation closed on a genuine ReviewFindings store error -- it must
// never be presented on the wire as an honest, real findings count. This
// deliberately builds its OWN router (rather than reusing
// newDecisionInboxTestRig) so ReviewFindings alone can be wired to a
// store backed by an ALREADY-ROLLED-BACK transaction (every query through
// it fails immediately with a real Postgres/pgx error, mirroring this
// package's own aggregate_integration_test.go precedent in the
// decisioninbox app package) while every other dependency stays healthy.
func TestListDecisionInbox_FindingsUnknownRendersNullNotTheFailClosedSentinel(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	users := narvipg.NewUserStore(pool)
	userSessions := narvipg.NewUserSessionStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	brokenReviewFindings := narvipg.NewReviewFindingStore(pool).WithTx(tx)

	const htmlURL = "https://github.com/acme/widgets/pull/1400"
	fakeSCM := &fakeMergeSourceControl{
		openPRs: []ports.OpenPR{
			{
				Owner: "acme", Repo: "widgets", Number: 1400, Title: "findings count unknown", HTMLURL: htmlURL,
				HeadSHA: "headsha1400", Assignees: []ports.PRPerson{{ExternalID: "9006", Login: "octocat"}},
				CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"},
			},
		},
	}

	reviewVerdicts := narvipg.NewReviewVerdictStore(pool)
	deps := decisioninbox.Deps{
		Plans:              narvipg.NewPlanStore(pool),
		Sessions:           sessions,
		Participants:       narvipg.NewParticipantStore(pool),
		Automations:        narvipg.NewAutomationStore(pool),
		Outbox:             narvipg.NewOutboxStore(pool, false),
		ReviewFindings:     brokenReviewFindings,
		SentinelFixes:      narvipg.NewSentinelFixStore(pool),
		Artifacts:          artifacts,
		Identities:         identities,
		SCMCache:           decisioninbox.NewSCMCache(fakeSCM, platform.DefaultTimeouts()),
		TokenEncryptionKey: decisionInboxTokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		// (§21.1/§21.2): computeRealEligibility runs regardless of
		// the findings-count failure above (never short-circuited by it),
		// so this must be a real, non-nil store too -- no verdict exists
		// for PR #1400 either way, so GetLatest reports ok=false
		// (gracefully), never a nil-pointer panic.
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts:       reviewVerdicts,
			RepoSettings:         narvipg.NewRepoSettingsStore(pool),
			ReviewFindings:       brokenReviewFindings,
			AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts:             platform.DefaultTimeouts(),
		},
	}

	router := chi.NewRouter()
	router.Route("/api/decision-inbox", func(r chi.Router) {
		r.Use(auth.Middleware(userSessions, users))
		r.Get("/", httpapi.ListDecisionInbox(deps))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	rig := &decisionInboxTestRig{
		pool: pool, users: users, userSessions: userSessions, identities: identities,
		sessions: sessions, artifacts: artifacts, reviewVerdicts: reviewVerdicts, server: server,
	}

	user, token := rig.createAuthenticatedUser(ctx, t, sqlcgen.UserRoleMember)
	rig.linkGitHub(ctx, t, user.ID, "9006")
	rig.markPlatformAuthored(ctx, t, user.ID, htmlURL)

	var got restdtos.ListDecisionInboxResponse
	status := rig.doJSON(t, http.MethodGet, "/api/decision-inbox", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	var row *restdtos.DecisionInboxItem
	for i := range got.Items {
		if got.Items[i].PrNumber != nil && *got.Items[i].PrNumber == 1400 {
			row = &got.Items[i]
		}
	}
	if row == nil {
		t.Fatalf("PR #1400 missing from the response entirely: %+v", got.Items)
	}
	// The store error must fail the READ-MODEL classification closed
	// (never ready_to_merge for a PR whose open-findings count could not
	// be confirmed zero) -- otherwise this test would prove the wire
	// nulling half of P3-3 while silently missing a regression on the
	// eligibility half A1 already fixed.
	if row.Kind != restdtos.DecisionInboxItemKindNeedsReview {
		t.Errorf("Kind = %q, want needs_review (a findings-count store error must still fail the eligibility computation closed)", row.Kind)
	}
	if row.Findings != nil {
		t.Errorf("Findings = %d, want nil -- the internal fail-closed sentinel (openFindingsUnknownFailClosed) must never be rendered on the wire as an honest, real count", *row.Findings)
	}
}

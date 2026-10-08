package credentialscope

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/providercredential"
)

type fakePRSessions struct {
	claim string
	err   error
	calls int
}

func (f *fakePRSessions) GetBySessionID(_ context.Context, _ pgtype.UUID) (sqlcgen.GithubPrSession, error) {
	f.calls++
	return sqlcgen.GithubPrSession{RepoFullName: f.claim}, f.err
}

// fakeLister answers like ListProviderCredentialsForResolution: every row
// whose scope the arguments reach, user rows only for the user asked for.
type fakeLister struct {
	rows []sqlcgen.ProviderCredential
}

func (f *fakeLister) ListForResolution(_ context.Context, repoFullNames []string, environmentID, userID *string) ([]sqlcgen.ProviderCredential, error) {
	var out []sqlcgen.ProviderCredential
	for _, row := range f.rows {
		switch row.Scope {
		case sqlcgen.ProviderCredentialScopeGlobal:
			out = append(out, row)
		case sqlcgen.ProviderCredentialScopeUser:
			if userID != nil && row.ScopeTargetID != nil && *row.ScopeTargetID == *userID {
				out = append(out, row)
			}
		case sqlcgen.ProviderCredentialScopeRepo:
			for _, name := range repoFullNames {
				if row.ScopeTargetID != nil && *row.ScopeTargetID == name {
					out = append(out, row)
				}
			}
		case sqlcgen.ProviderCredentialScopeEnvironment:
			if environmentID != nil && row.ScopeTargetID != nil && *row.ScopeTargetID == *environmentID {
				out = append(out, row)
			}
		}
	}
	return out, nil
}

func uuidFrom(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("scan uuid %q: %v", s, err)
	}
	return u
}

const (
	creatorID = "0b7e2f64-8d7a-4b8e-9d5f-3a1c2e4f5a6b"
	parentID  = "7c1d9e2a-3b4f-4a5c-8d6e-9f0a1b2c3d4e"
	envID     = "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"
)

// TestLoad reads each kind of session into the Origin the rule decides
// from, with its github_pr_sessions membership read through the reader
// every time.
func TestLoad(t *testing.T) {
	repos := []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null},{"name":"bad","url":"not a url","branch":null},{"name":"gears","url":"https://github.com/acme/gears","branch":null}]`)
	baseBranchRepos := []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":"feature-x"},{"name":"bad","url":"not a url","branch":null},{"name":"gears","url":"https://github.com/acme/gears","branch":null}]`)
	legacyForkRepos := []byte(`[{"name":"widgets","url":"https://github.com/contributor/widgets","branch":"main"},{"name":"bad","url":"not a url","branch":null},{"name":"gears","url":"https://github.com/acme/gears","branch":null}]`)
	allRepos := []string{"acme/widgets", "acme/gears"}
	tests := []struct {
		name       string
		session    sqlcgen.Session
		claim      string
		prErr      error
		wantOrigin providercredential.SessionOrigin
		wantUser   bool
		// noRepos: the repository scope is not resolved at all;
		// otherwise it is allRepos.
		noRepos bool
	}{
		{
			name:       "web session its owner created",
			session:    sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID), Repos: repos},
			prErr:      pgx.ErrNoRows,
			wantOrigin: providercredential.SessionOrigin{CreatedBy: creatorID},
			wantUser:   true,
		},
		{
			name:       "review session of a pull request from a branch of its base repository",
			session:    sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID), Repos: baseBranchRepos, SpawnSource: sqlcgen.SessionSpawnSourceGithub},
			claim:      "acme/widgets",
			wantOrigin: providercredential.SessionOrigin{CreatedBy: creatorID, PullRequestReview: true},
		},
		{
			// The spec names the base with no branch: a fork's pull
			// request, a deleted fork's, or a head not read. Its sandbox
			// runs the pull request's head, so the base repository's
			// repository-scoped credentials are not resolved.
			name:       "review session whose head is not known to be in its base repository",
			session:    sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID), Repos: repos, SpawnSource: sqlcgen.SessionSpawnSourceGithub},
			claim:      "acme/widgets",
			wantOrigin: providercredential.SessionOrigin{CreatedBy: creatorID, PullRequestReview: true},
			noRepos:    true,
		},
		{
			name:       "legacy review session still naming the fork, with the fork's branch",
			session:    sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID), Repos: legacyForkRepos, SpawnSource: sqlcgen.SessionSpawnSourceGithub},
			claim:      "acme/widgets",
			wantOrigin: providercredential.SessionOrigin{CreatedBy: creatorID, PullRequestReview: true},
			noRepos:    true,
		},
		{
			name:       "automation session",
			session:    sqlcgen.Session{Repos: repos},
			prErr:      pgx.ErrNoRows,
			wantOrigin: providercredential.SessionOrigin{},
		},
		{
			name:       "child session",
			session:    sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID), ParentSessionID: uuidFrom(t, parentID), Repos: repos},
			prErr:      pgx.ErrNoRows,
			wantOrigin: providercredential.SessionOrigin{CreatedBy: creatorID, Child: true},
		},
		{
			name:       "a github-origin session no pull request row names is not a review session",
			session:    sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID), Repos: repos, SpawnSource: sqlcgen.SessionSpawnSourceGithub},
			prErr:      pgx.ErrNoRows,
			wantOrigin: providercredential.SessionOrigin{CreatedBy: creatorID},
			wantUser:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prSessions := &fakePRSessions{claim: tc.claim, err: tc.prErr}
			scope, err := Load(context.Background(), prSessions, tc.session)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if prSessions.calls != 1 {
				t.Errorf("github_pr_sessions reads = %d, want 1", prSessions.calls)
			}
			if scope.Origin != tc.wantOrigin {
				t.Errorf("Origin = %+v, want %+v", scope.Origin, tc.wantOrigin)
			}
			switch {
			case tc.noRepos && len(scope.RepoFullNames) != 0:
				t.Errorf("RepoFullNames = %v, want none: the review session's head is not known to be in its base repository", scope.RepoFullNames)
			case !tc.noRepos && !reflect.DeepEqual(scope.RepoFullNames, allRepos):
				t.Errorf("RepoFullNames = %v, want %v (unparseable skipped, order kept)", scope.RepoFullNames, allRepos)
			}
			id := scope.UserID()
			switch {
			case tc.wantUser && (id == nil || *id != creatorID):
				t.Errorf("UserID() = %v, want the creator %q", id, creatorID)
			case !tc.wantUser && id != nil:
				t.Errorf("UserID() = %q, want nil", *id)
			}
		})
	}
}

// TestLoad_MembershipReadFailureIsAnError pins the fail-closed half: a
// github_pr_sessions read that fails for any reason but "no row" is an
// error, never "not a review session", which would serve the link.
func TestLoad_MembershipReadFailureIsAnError(t *testing.T) {
	session := sqlcgen.Session{CreatedBy: uuidFrom(t, creatorID)}
	if _, err := Load(context.Background(), &fakePRSessions{err: errors.New("connection reset")}, session); err == nil {
		t.Fatal("Load = nil error, want the read failure")
	}
}

func TestLoad_EnvironmentAndMalformedRepos(t *testing.T) {
	session := sqlcgen.Session{EnvironmentID: uuidFrom(t, envID)}
	scope, err := Load(context.Background(), &fakePRSessions{err: pgx.ErrNoRows}, session)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if scope.EnvironmentID == nil || *scope.EnvironmentID != envID {
		t.Errorf("EnvironmentID = %v, want %q", scope.EnvironmentID, envID)
	}

	if _, err := Load(context.Background(), &fakePRSessions{err: pgx.ErrNoRows}, sqlcgen.Session{Repos: []byte(`{"not":"a list"}`)}); err == nil {
		t.Error("Load with repos that are not a JSON list = nil error, want one")
	}
}

func ptr(s string) *string { return &s }

// TestScope_CandidatesAndProviders runs one deployment (a global anthropic
// key) and one creator's personal openai link through each kind: the
// personal link is a candidate only where the rule resolves it, and
// Providers puts it on the withheld side everywhere else.
func TestScope_CandidatesAndProviders(t *testing.T) {
	rows := []sqlcgen.ProviderCredential{
		{Scope: sqlcgen.ProviderCredentialScopeGlobal, Provider: sqlcgen.ProviderCredentialProviderAnthropic},
		{Scope: sqlcgen.ProviderCredentialScopeUser, ScopeTargetID: ptr(creatorID), Provider: sqlcgen.ProviderCredentialProviderOpenai, Kind: sqlcgen.ProviderCredentialKindOauth},
	}
	anthropic := map[providercredential.Provider]bool{providercredential.ProviderAnthropic: true}
	both := map[providercredential.Provider]bool{providercredential.ProviderAnthropic: true, providercredential.ProviderOpenAI: true}
	openai := map[providercredential.Provider]bool{providercredential.ProviderOpenAI: true}
	none := map[providercredential.Provider]bool{}

	tests := []struct {
		name           string
		origin         providercredential.SessionOrigin
		wantCandidates int
		wantResolvable map[providercredential.Provider]bool
		wantPersonal   map[providercredential.Provider]bool
	}{
		{"web session", providercredential.SessionOrigin{CreatedBy: creatorID}, 2, both, none},
		{"review session", providercredential.SessionOrigin{CreatedBy: creatorID, PullRequestReview: true}, 1, anthropic, openai},
		{"child session", providercredential.SessionOrigin{CreatedBy: creatorID, Child: true}, 1, anthropic, openai},
		{"automation session", providercredential.SessionOrigin{}, 1, anthropic, none},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope := Scope{Origin: tc.origin}
			candidates, err := scope.Candidates(context.Background(), &fakeLister{rows: rows})
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			if len(candidates) != tc.wantCandidates {
				t.Errorf("len(Candidates) = %d, want %d (%+v)", len(candidates), tc.wantCandidates, candidates)
			}

			resolvable, personal, err := scope.Providers(context.Background(), &fakeLister{rows: rows})
			if err != nil {
				t.Fatalf("Providers: %v", err)
			}
			if !reflect.DeepEqual(resolvable, tc.wantResolvable) {
				t.Errorf("resolvable = %v, want %v", resolvable, tc.wantResolvable)
			}
			if !reflect.DeepEqual(personal, tc.wantPersonal) {
				t.Errorf("personal = %v, want %v", personal, tc.wantPersonal)
			}
		})
	}
}

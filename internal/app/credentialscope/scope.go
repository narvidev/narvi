// Package credentialscope reads what one session's provider-credential
// resolution is keyed on (technical plan §29.4), for the two places that
// resolve it: the sandbox's credential delivery
// (httpapi.ProviderCredentialsDelivery) and the counter-reviewer's choice
// of an opposing model (sessionactor's reviewCredentialedProviders), plus
// the dispatch gate that refuses a turn whose model only a withheld
// personal link could run. Each of them loads a Scope here and lists its
// candidates through it, so the three cannot key resolution differently.
//
// Whether the user scope is read at all is providercredential.
// UserScopeTarget's decision, the one rule; this package only supplies it
// with facts read fresh: the session row the caller has just loaded, and
// the session's github_pr_sessions membership, read from that table on
// every Load, never cached.
package credentialscope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reposource"
)

// PRSessionReader is the github_pr_sessions reverse lookup Load reads
// membership through (postgres.GitHubPRSessionStore satisfies it).
// pgx.ErrNoRows means the session is not a pull request's review session.
type PRSessionReader interface {
	GetBySessionID(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.GithubPrSession, error)
}

// CandidateLister is the provider_credentials read a Scope lists through
// (postgres.ProviderCredentialStore satisfies it).
type CandidateLister interface {
	ListForResolution(ctx context.Context, repoFullNames []string, environmentID, userID *string) ([]sqlcgen.ProviderCredential, error)
}

// Scope is one session's resolution inputs: the repos and environment its
// deployment-scope candidates key on, and the facts the user-scope rule
// reads.
type Scope struct {
	// RepoFullNames is every "owner/repo" of sessions.repos whose clone
	// URL parses, primary first (§3.4, "position 0 = primary"). A repo
	// that does not parse is skipped, never an error: one malformed entry
	// must not withhold every other repo's credential.
	//
	// It is empty for a pull request's review session whose head is not
	// known to be a branch of its base repository
	// (reposource.ReviewHeadInBaseRepository; technical plan §30.4): a
	// pull request from a fork, a deleted fork's, one whose head could not
	// be read when its review started, a legacy session still naming the
	// fork. Its spec names the base repository, but its sandbox runs the
	// pull request's head -- code a person outside the base repository may
	// have written -- and its agent runtime runs tools over it, so the
	// base repository's repository-scoped credentials are not resolved for
	// it; the environment and global scopes are, as for any review session.
	RepoFullNames []string
	// EnvironmentID is sessions.environment_id, stringified; nil when the
	// session has no Environment.
	EnvironmentID *string
	// Origin is what providercredential.UserScopeTarget decides from.
	Origin providercredential.SessionOrigin
}

// Load builds sessionRow's Scope, reading its github_pr_sessions
// membership through prSessions. A failed membership read is an error,
// never "not a review session": taken as the latter, it would resolve a
// review session's requester's personal link. So is a sessions.repos value
// that is not a JSON list.
func Load(ctx context.Context, prSessions PRSessionReader, sessionRow sqlcgen.Session) (Scope, error) {
	repoFullNames, err := repoFullNames(sessionRow.Repos)
	if err != nil {
		return Scope{}, fmt.Errorf("credentialscope: parse session repos: %w", err)
	}

	review := true
	claim, err := prSessions.GetBySessionID(ctx, sessionRow.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return Scope{}, fmt.Errorf("credentialscope: read github pr session: %w", err)
		}
		review = false
	}
	if review && !reposource.ReviewHeadInBaseRepository(claim.RepoFullName, sessionRow.Repos) {
		repoFullNames = nil
	}

	scope := Scope{
		RepoFullNames: repoFullNames,
		Origin: providercredential.SessionOrigin{
			Child:             sessionRow.ParentSessionID.Valid,
			PullRequestReview: review,
		},
	}
	if sessionRow.EnvironmentID.Valid {
		id := sessionRow.EnvironmentID.String()
		scope.EnvironmentID = &id
	}
	if sessionRow.CreatedBy.Valid {
		scope.Origin.CreatedBy = sessionRow.CreatedBy.String()
	}
	return scope, nil
}

// UserID is the users.id whose user-scope rows this session's resolution
// reads, or nil when it reads none -- providercredential.UserScopeTarget,
// the one rule.
func (s Scope) UserID() *string {
	id, ok := providercredential.UserScopeTarget(s.Origin)
	if !ok {
		return nil
	}
	return &id
}

// Candidates lists every provider_credentials row this session's
// resolution considers: its repos', its environment's and the global rows,
// and its user-scope rows only when UserID names someone.
func (s Scope) Candidates(ctx context.Context, lister CandidateLister) ([]sqlcgen.ProviderCredential, error) {
	return lister.ListForResolution(ctx, s.RepoFullNames, s.EnvironmentID, s.UserID())
}

// Providers returns the providers this session's resolution reaches
// (resolvable: what its sandbox is delivered) and the providers its
// creator's own link carries that this resolution withholds (personal) --
// providercredential.PersonalLinkOnly's two inputs. personal is empty
// whenever the session resolves its creator's link (nothing is withheld)
// or has no creator (there is no one whose link it would be).
func (s Scope) Providers(ctx context.Context, lister CandidateLister) (resolvable, personal map[providercredential.Provider]bool, err error) {
	// Listed for the creator whether or not the rule resolves their link:
	// which side their user-scope rows fall on is decided below, by the
	// same rule Candidates applies.
	var creator *string
	if s.Origin.CreatedBy != "" {
		id := s.Origin.CreatedBy
		creator = &id
	}
	rows, err := lister.ListForResolution(ctx, s.RepoFullNames, s.EnvironmentID, creator)
	if err != nil {
		return nil, nil, fmt.Errorf("credentialscope: list provider credentials: %w", err)
	}

	withheld := s.UserID() == nil
	byProvider := make(map[providercredential.Provider][]providercredential.Candidate[struct{}], len(rows))
	personal = make(map[providercredential.Provider]bool)
	for _, row := range rows {
		p := providercredential.Provider(row.Provider)
		scope := providercredential.Scope(row.Scope)
		if scope == providercredential.ScopeUser && withheld {
			personal[p] = true
			continue
		}
		byProvider[p] = append(byProvider[p], providercredential.Candidate[struct{}]{Scope: scope})
	}
	resolvable = make(map[providercredential.Provider]bool, len(byProvider))
	for p, candidates := range byProvider {
		if _, ok := providercredential.Resolve(candidates); ok {
			resolvable[p] = true
		}
	}
	return resolvable, personal, nil
}

// repoFullNames reads sessions.repos' raw JSONB into the "owner/repo" full
// names its clone URLs parse to, in order, skipping any that do not.
func repoFullNames(rawRepos []byte) ([]string, error) {
	if len(rawRepos) == 0 {
		return nil, nil
	}
	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rawRepos, &repos); err != nil {
		return nil, err
	}
	fullNames := make([]string, 0, len(repos))
	for _, repo := range repos {
		owner, name, err := reposource.ParseOwnerRepo(repo.URL)
		if err != nil {
			continue
		}
		fullNames = append(fullNames, owner+"/"+name)
	}
	return fullNames, nil
}

package providercredential

import "strings"

// SessionOrigin is what one session's own rows say about whose credentials
// its turns may run on: the three facts UserScopeTarget decides from. Every
// caller builds it from rows it has just read, never from a flag cached on
// the session: PullRequestReview in particular is read from
// github_pr_sessions each time.
type SessionOrigin struct {
	// CreatedBy is sessions.created_by, stringified; "" when the session
	// has none (an automation's session, a bot-attributed one).
	CreatedBy string
	// Child is true when sessions.parent_session_id is set: the platform
	// started this session from another one (today, only a sentinel
	// auto-fix child of a review session).
	Child bool
	// PullRequestReview is true when a github_pr_sessions row names the
	// session: it is a pull request's review session.
	PullRequestReview bool
}

// UserScopeTarget is the one rule deciding whether a session's provider
// credential resolution reads the user scope, and for whom (technical plan
// §29.4). Both readers of that resolution call it, through
// internal/app/credentialscope: the sandbox's credential delivery
// (httpapi.ProviderCredentialsDelivery) and the counter-reviewer's choice
// of an opposing model (sessionactor's reviewCredentialedProviders); so
// does the dispatch gate that refuses a turn only a withheld link could
// run (sessionactor's credentialgate.go).
//
// It returns the creator's users.id, and true, only for a session a person
// created and runs themselves. It returns false for:
//
//   - a session with no creator (an automation's, a bot-attributed one):
//     there is no one whose link it could be;
//   - a pull request's review session: its creator is merely the first
//     linked person who triggered it, so resolving their link would run
//     every later review of that pull request, the automatic lane's and
//     other maintainers' included, on that one person's own seat and send
//     the diff there -- a change of where code goes that nobody chose
//     (§45.1). §29.10 accepted the seat sharing of a multiplayer session
//     its creator chose to open; a review session has no such creator;
//   - a child session, whatever its created_by: the platform started it,
//     not a person running their own work, and a link is used only for its
//     owner's own sessions (§29.10 item 3); §45.1 states the same for every
//     child ("a selection never passes to a child").
//
// A session this returns false for keeps the deployment's credentials
// (environment, repo and global scopes).
func UserScopeTarget(o SessionOrigin) (userID string, ok bool) {
	if o.CreatedBy == "" || o.PullRequestReview || o.Child {
		return "", false
	}
	return o.CreatedBy, true
}

// Refusal names why a turn is refused before it runs because of where its
// provider credential would have come from. It is carried verbatim at the
// head of the refused turn's terminal reason and session warning, and on a
// review's check.
type Refusal string

// RefusalPersonalLinkOnly means the turn names a model whose provider this
// session can reach only through its creator's own provider link, and the
// session never resolves that link (UserScopeTarget). The turn is refused,
// never run on the link and never on some other model in its place.
const RefusalPersonalLinkOnly Refusal = "personal_link_only"

// ModelProvider returns the Provider a "provider/model" id names, and
// false when it names none of AllProviders (no "/" at all, or a provider
// this table never stores credentials for).
func ModelProvider(model string) (Provider, bool) {
	name, _, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok {
		return "", false
	}
	p := Provider(strings.ToLower(strings.TrimSpace(name)))
	if !IsValidProvider(p) {
		return "", false
	}
	return p, true
}

// PersonalLinkOnly reports whether a turn naming model must be refused with
// RefusalPersonalLinkOnly, and the provider that model needs: resolvable
// holds the providers the session's own resolution reaches (what its
// sandbox is delivered), personal the providers its creator's own link
// carries and that resolution withholds. A model whose provider resolves,
// or that no link carries either, is not refused here: the first runs, and
// the second fails as a turn with no credential always has.
func PersonalLinkOnly(model string, resolvable, personal map[Provider]bool) (Provider, bool) {
	p, ok := ModelProvider(model)
	if !ok {
		return "", false
	}
	if resolvable[p] || !personal[p] {
		return "", false
	}
	return p, true
}

// PersonalLinkOnlyMessage is the refused turn's terminal reason: the
// Refusal's name first, so it reads the same wherever it is shown, then
// the model, the provider and what would let it run.
func PersonalLinkOnlyMessage(model string, p Provider) string {
	return string(RefusalPersonalLinkOnly) + ": the model " + model + " is available here only through a person's own " +
		string(p) + " link, which this session never runs on. Add a deployment credential for " +
		string(p) + " to use this model, or choose another."
}

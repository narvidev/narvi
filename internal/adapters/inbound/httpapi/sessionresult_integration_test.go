//go:build integration

// Integration tests for GET /api/sessions/{sessionID}/result (technical
// plan §43.20, row 182's result) on real Postgres: the last run and its
// bounded summary, the pull requests a session opened or reviews, and each
// one's review state read from the record, with its freshness read live
// from a fake code host -- never current without that live confirmation.
//
// No test here spawns a session actor: every row is seeded through the
// stores, so one rig per test function holds no connection beyond the
// requests' own.
package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// resultCodeHost is a fake code host for the result's live freshness read,
// answering per pull request ("owner/repo#n") and per branch
// ("owner/repo:branch"), and counting the pull request reads -- the entry
// to every live read -- per pull request. Any other port method panics:
// the result never calls one.
type resultCodeHost struct {
	ports.SourceControl

	mu       sync.Mutex
	prs      map[string]resultHostPR
	branches map[string]string
	ancestry map[string]bool
	reads    map[string]int
	tokens   map[string]bool
}

type resultHostPR struct {
	pr    ports.OpenPR
	found bool
	err   error
}

func newResultCodeHost() *resultCodeHost {
	return &resultCodeHost{prs: map[string]resultHostPR{}, branches: map[string]string{}, ancestry: map[string]bool{}, reads: map[string]int{}, tokens: map[string]bool{}}
}

func prKey(owner, repo string, n int) string { return fmt.Sprintf("%s/%s#%d", owner, repo, n) }

// open reports the pull request open at head, based on base.
func (h *resultCodeHost) open(owner, repo string, n int, head, base string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prs[prKey(owner, repo, n)] = resultHostPR{found: true, pr: ports.OpenPR{Owner: owner, Repo: repo, Number: n, HeadSHA: head, BaseRef: base}}
}

func (h *resultCodeHost) set(owner, repo string, n int, p resultHostPR) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prs[prKey(owner, repo, n)] = p
}

func (h *resultCodeHost) branch(owner, repo, name, sha string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.branches[owner+"/"+repo+":"+name] = sha
}

func (h *resultCodeHost) readsOf(owner, repo string, n int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads[prKey(owner, repo, n)]
}

func (h *resultCodeHost) GetOpenPR(_ context.Context, owner, repo string, number int, token string) (ports.OpenPR, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := prKey(owner, repo, number)
	h.reads[key]++
	h.tokens[token] = true
	p, ok := h.prs[key]
	if !ok {
		return ports.OpenPR{}, false, errors.New("fake code host: no such pull request " + key)
	}
	return p.pr, p.found, p.err
}

func (h *resultCodeHost) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sha, ok := h.branches[spec.Owner+"/"+spec.Repo+":"+spec.Branch]
	if !ok {
		return "", "", errors.New("fake code host: no such branch " + spec.Branch)
	}
	return sha, spec.Branch, nil
}

func (h *resultCodeHost) IsAncestor(_ context.Context, spec ports.IsAncestorSpec) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	answer, ok := h.ancestry[spec.Ancestor+".."+spec.Descendant]
	if !ok {
		return false, errors.New("fake code host: unknown ancestry " + spec.Ancestor + ".." + spec.Descendant)
	}
	return answer, nil
}

// newResultRig is a rig whose result route reads freshness from host (nil:
// no code host configured), with a member signed in.
func newResultRig(t *testing.T, host *resultCodeHost) (testRig, sqlcgen.User, string) {
	t.Helper()
	rig := newTestRig(t, func(r *testRig) {
		if host != nil {
			r.resultSourceControl = host
		}
	})
	user, cookie := createUserWithRole(context.Background(), t, rig, sqlcgen.UserRoleMember)
	return rig, user, cookie
}

// getResult reads the result route as the cookie's user, decoded through
// the generated DTO (whose UnmarshalJSON enforces every required field and
// enum) and as a raw map, for key-level assertions.
func getResult(t *testing.T, r testRig, sessionID pgtype.UUID, cookie string) (restdtos.SessionOutcome, map[string]any) {
	t.Helper()
	var raw json.RawMessage
	if status := r.doJSON(t, http.MethodGet, "/api/sessions/"+sessionID.String()+"/result", nil, &raw, cookie); status != http.StatusOK {
		t.Fatalf("GET result: %d (%s), want 200", status, raw)
	}
	var got restdtos.SessionOutcome
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode SessionOutcome: %v (%s)", err, raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	return got, m
}

// reviewSession creates a session and makes it repoFullName#n's review
// session (the per-pull-request claim).
func reviewSession(ctx context.Context, t *testing.T, r testRig, ownerID pgtype.UUID, repoFullName string, n int32) sqlcgen.Session {
	t.Helper()
	sess := createSessionForUser(ctx, t, r, ownerID, nil)
	if err := r.prSessions.EnsureRow(ctx, repoFullName, n); err != nil {
		t.Fatalf("ensure claim row: %v", err)
	}
	if err := r.prSessions.SetSessionID(ctx, repoFullName, n, sess.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return sess
}

// reviewAttempt creates a review attempt on sessionID and drives it along
// the turn machine through triggers.
func reviewAttempt(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, triggers ...turn.Trigger) sqlcgen.Turn {
	t.Helper()
	row, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create review attempt: %v", err)
	}
	state := turn.StatePending
	for _, trig := range triggers {
		state = transitionTurn(ctx, t, r.turns, row.ID, state, trig)
	}
	return row
}

var endedCompleted = []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing, turn.TriggerComplete}

// verdictAt records a verdict for repoFullName#n posted by attemptID (zero
// for none), at head, with the recorded context rc (a nil BaseRef records
// none, as a verdict from before context tracking).
type recordedContext struct {
	baseRef, baseSHA *string
	chain            []review.AncestorLink
	policy           int32
}

func str(s string) *string { return &s }

func currentContext() recordedContext {
	return recordedContext{baseRef: str("main"), baseSHA: str("b1"), policy: autoapproval.CurrentPolicyVersion}
}

func verdictAt(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, repoFullName string, n int32, attemptID pgtype.UUID, head string, rc recordedContext) sqlcgen.ReviewVerdict {
	t.Helper()
	chain, err := json.Marshal(rc.chain)
	if err != nil {
		t.Fatalf("marshal chain: %v", err)
	}
	if rc.chain == nil {
		chain = []byte(`[]`)
	}
	row, err := r.reviewVerdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:      repoFullName,
		PrNumber:          n,
		HeadSha:           head,
		RiskLevel:         "low",
		Premise:           "ok",
		BlastRadius:       []byte(`[]`),
		FilesChanged:      1,
		TestsCoverage:     "adequate",
		DocsDrift:         "none",
		ProposedShippable: "auto",
		Shippable:         "auto",
		SessionID:         sessionID,
		ArchDecisionTags:  []byte(`[]`),
		ArchDecisionRoots: []byte(`[]`),
		AncestorChain:     chain,
		BaseRef:           rc.baseRef,
		BaseSha:           rc.baseSHA,
		PolicyVersion:     rc.policy,
		AttemptID:         attemptID,
	})
	if err != nil {
		t.Fatalf("insert verdict: %v", err)
	}
	return row
}

// prArtifact records a pull request the session opened, as the session
// actor does (recordPRArtifact): the bare repo name and the number.
func prArtifact(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, bareRepo string, n int, url string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"repo": bareRepo, "number": n})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if _, err := r.artifacts.Create(ctx, sqlcgen.CreateArtifactParams{SessionID: sessionID, Type: sqlcgen.ArtifactTypePr, Url: url, Metadata: meta}); err != nil {
		t.Fatalf("record pull request artifact: %v", err)
	}
}

// sessionWithRepos creates a session for ownerID whose repositories are
// repos (name -> clone url).
func sessionWithRepos(ctx context.Context, t *testing.T, r testRig, ownerID pgtype.UUID, repos map[string]string) sqlcgen.Session {
	t.Helper()
	// The shape sessions.repos always holds ({branch, name, url}, branch
	// nullable): the session actor's own decoder requires every key.
	type repo struct {
		Branch *string `json:"branch"`
		Name   string  `json:"name"`
		URL    string  `json:"url"`
	}
	var list []repo
	for name, url := range repos {
		list = append(list, repo{Name: name, URL: url})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	sess, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: ownerID, Repos: raw})
	if err != nil {
		t.Fatalf("create session with repos: %v", err)
	}
	return sess
}

func reasonOf(f restdtos.SessionOutcomeReviewFreshness) string {
	if f.Reason == nil {
		return ""
	}
	return *f.Reason
}

// TestResult_ReviewStatus_Table is row 182's result exit on real Postgres:
// absent, in_progress, not_assessed, stale, unconfirmed and not_applicable
// are distinct on the wire; a verdict is current only when the live read
// confirms it; a merged pull request is decided from its claim with no live
// read at all, while one closed without merging -- perhaps reopened since
// -- is read live; a verdict the merge path's probe decides (a bumped
// policy, a context never recorded) costs the pull request read alone and
// gets the merge path's own reason, a moved head included; and a session
// with no pull request says reviewScope none, never an empty list that
// could read as clean.
func TestResult_ReviewStatus_Table(t *testing.T) {
	ctx := context.Background()
	host := newResultCodeHost()
	rig, user, cookie := newResultRig(t, host)
	const owner, repo = "acme", "widgets"
	full := owner + "/" + repo

	// A pull request whose newest attempt posted a verdict at h1 on main
	// at b1 under the current policy (unless rc says otherwise), on its
	// own review session.
	assessed := func(n int32, rc recordedContext) sqlcgen.Session {
		sess := reviewSession(ctx, t, rig, user.ID, full, n)
		a := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
		verdictAt(ctx, t, rig, sess.ID, full, n, a.ID, "h1", rc)
		return sess
	}
	host.branch(owner, repo, "main", "b1")

	type want struct {
		scope      restdtos.SessionOutcomeReviewScope
		state      restdtos.SessionOutcomeReviewState
		verdict    bool
		superseded bool
		freshness  restdtos.SessionOutcomeReviewFreshnessState
		reason     string
		reads      int // live pull request reads
	}
	tests := []struct {
		name  string
		n     int32
		setup func() sqlcgen.Session
		want  want
	}{
		{
			name: "a pull request never reviewed: absent",
			n:    101,
			setup: func() sqlcgen.Session {
				sess := createSessionForUser(ctx, t, rig, user.ID, nil)
				prArtifact(ctx, t, rig, sess.ID, repo, 101, "https://github.com/acme/widgets/pull/101")
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeProduced, state: restdtos.SessionOutcomeReviewStateAbsent, freshness: restdtos.SessionOutcomeReviewFreshnessStateNotApplicable},
		},
		{
			name: "a newer attempt running over an older verdict: in_progress, the older superseded",
			n:    102,
			setup: func() sqlcgen.Session {
				sess := assessed(102, currentContext())
				reviewAttempt(ctx, t, rig, sess.ID, turn.TriggerDispatch, turn.TriggerStartProcessing)
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateInProgress, superseded: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateNotApplicable},
		},
		{
			name: "the newest attempt ended without posting over an older verdict: not_assessed, the older superseded",
			n:    103,
			setup: func() sqlcgen.Session {
				sess := assessed(103, currentContext())
				reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateNotAssessed, superseded: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateNotApplicable},
		},
		{
			name: "the live head moved: stale",
			n:    104,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 104, "h2", "main")
				return assessed(104, currentContext())
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateStale, reason: string(autoapproval.ReasonStaleVerdict), reads: 1},
		},
		{
			name: "the live read fails: unconfirmed",
			n:    105,
			setup: func() sqlcgen.Session {
				host.set(owner, repo, 105, resultHostPR{err: errors.New("code host unavailable")})
				return assessed(105, currentContext())
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateUnconfirmed, reason: reviewfreshness.ReasonPullRequestUnread, reads: 1},
		},
		{
			name: "the policy was bumped: stale, from the probe on the pull request read alone",
			n:    106,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 106, "h1", "main")
				rc := currentContext()
				rc.policy = autoapproval.CurrentPolicyVersion - 1
				return assessed(106, rc)
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateStale, reason: string(autoapproval.ReasonPolicyVersionMismatch), reads: 1},
		},
		{
			name: "everything as recorded: current",
			n:    107,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 107, "h1", "main")
				return assessed(107, currentContext())
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateCurrent, reads: 1},
		},
		{
			name: "merged per the claim: not_applicable, with no live read",
			n:    108,
			setup: func() sqlcgen.Session {
				sess := assessed(108, currentContext())
				if _, err := rig.prSessions.RecordMergeOutcome(ctx, full, 108, true, time.Now()); err != nil {
					t.Fatalf("record merge: %v", err)
				}
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateNotApplicable, reason: "the pull request has been merged"},
		},
		{
			name: "no context recorded: unconfirmed, from the probe on the pull request read alone",
			n:    109,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 109, "h1", "main")
				return assessed(109, recordedContext{policy: autoapproval.CurrentPolicyVersion})
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateUnconfirmed, reason: string(autoapproval.ReasonContextUnknown), reads: 1},
		},
		{
			// The merge path's probe compares the live head before anything
			// the record lacks (finding P8): stale, as the merge path says.
			name: "no context recorded and the head moved: stale, the merge path's reason",
			n:    115,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 115, "h2", "main")
				return assessed(115, recordedContext{policy: autoapproval.CurrentPolicyVersion})
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateStale, reason: string(autoapproval.ReasonStaleVerdict), reads: 1},
		},
		{
			// The closed webhook stamps the claim once and nothing clears it
			// on reopen (finding P2): a pull request closed without merging is
			// read live, and reopened it reads current.
			name: "closed without merging, then reopened: read live, current",
			n:    116,
			setup: func() sqlcgen.Session {
				sess := assessed(116, currentContext())
				if _, err := rig.prSessions.RecordMergeOutcome(ctx, full, 116, false, time.Now()); err != nil {
					t.Fatalf("record the close: %v", err)
				}
				host.open(owner, repo, 116, "h1", "main")
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateCurrent, reads: 1},
		},
		{
			name: "closed without merging and still closed: not_applicable, from the live read",
			n:    117,
			setup: func() sqlcgen.Session {
				sess := assessed(117, currentContext())
				if _, err := rig.prSessions.RecordMergeOutcome(ctx, full, 117, false, time.Now()); err != nil {
					t.Fatalf("record the close: %v", err)
				}
				host.set(owner, repo, 117, resultHostPR{found: false})
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateNotApplicable, reason: reviewfreshness.ReasonNoLongerOpen, reads: 1},
		},
		{
			name: "the base moved forward only: current",
			n:    110,
			setup: func() sqlcgen.Session {
				// This pull request's base is its own branch, so the fake's
				// branch map stays per-row.
				host.open(owner, repo, 110, "h1", "trunk-110")
				host.branch(owner, repo, "trunk-110", "b2")
				host.mu.Lock()
				host.ancestry["b1..b2"] = true
				host.mu.Unlock()
				rc := currentContext()
				rc.baseRef = str("trunk-110")
				return assessed(110, rc)
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateCurrent, reads: 1},
		},
		{
			name: "the base was rewritten under the same ref: stale",
			n:    112,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 112, "h1", "trunk-112")
				host.branch(owner, repo, "trunk-112", "b3")
				host.mu.Lock()
				host.ancestry["b1..b3"] = false
				host.mu.Unlock()
				rc := currentContext()
				rc.baseRef = str("trunk-112")
				return assessed(112, rc)
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateStale, reason: string(autoapproval.ReasonBaseMoved), reads: 1},
		},
		{
			// The same head, and a base branch whose tip is the very commit
			// recorded -- only the base ref says the pull request moved.
			name: "retargeted onto another base at the same commit: stale",
			n:    113,
			setup: func() sqlcgen.Session {
				host.open(owner, repo, 113, "h1", "release-113")
				host.branch(owner, repo, "release-113", "b1")
				return assessed(113, currentContext())
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateStale, reason: string(autoapproval.ReasonBaseMoved), reads: 1},
		},
		{
			// Head and base unchanged; the pull request now sits on a parent.
			name: "stacked since the verdict: stale",
			n:    114,
			setup: func() sqlcgen.Session {
				host.set(owner, repo, 114, resultHostPR{found: true, pr: ports.OpenPR{Owner: owner, Repo: repo, Number: 114, HeadSHA: "h1", BaseRef: "main",
					AncestorChain: []ports.PRAncestorLink{{Ref: "parent-114"}}}})
				host.branch(owner, repo, "parent-114", "p1")
				return assessed(114, currentContext())
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeReviewed, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateStale, reason: string(autoapproval.ReasonAncestorChainChanged), reads: 1},
		},
		{
			name: "no longer open per the live read: not_applicable",
			n:    111,
			setup: func() sqlcgen.Session {
				host.set(owner, repo, 111, resultHostPR{found: false})
				sess := createSessionForUser(ctx, t, rig, user.ID, nil)
				prArtifact(ctx, t, rig, sess.ID, repo, 111, "https://github.com/acme/widgets/pull/111")
				// A verdict on record with no attempt (recorded before
				// attempts were): assessed.
				verdictAt(ctx, t, rig, sess.ID, full, 111, pgtype.UUID{}, "h1", currentContext())
				return sess
			},
			want: want{scope: restdtos.SessionOutcomeReviewScopeProduced, state: restdtos.SessionOutcomeReviewStateAssessed, verdict: true, freshness: restdtos.SessionOutcomeReviewFreshnessStateNotApplicable, reason: reviewfreshness.ReasonNoLongerOpen, reads: 1},
		},
	}

	seen := map[string]bool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := tc.setup()
			got, raw := getResult(t, rig, sess.ID, cookie)
			if got.ReviewScope != tc.want.scope {
				t.Fatalf("reviewScope = %q, want %q", got.ReviewScope, tc.want.scope)
			}
			var review restdtos.SessionOutcomeReview
			switch tc.want.scope {
			case restdtos.SessionOutcomeReviewScopeReviewed:
				if got.ReviewedPullRequest == nil || got.ReviewedPullRequest.RepoFullName != full || got.ReviewedPullRequest.Number != int(tc.n) {
					t.Fatalf("reviewedPullRequest = %+v, want %s#%d", got.ReviewedPullRequest, full, tc.n)
				}
				review = got.ReviewedPullRequest.Review
			default:
				if len(got.PullRequests) != 1 || got.PullRequests[0].RepoFullName != full || got.PullRequests[0].Number != int(tc.n) || got.ReviewedPullRequest != nil {
					t.Fatalf("pullRequests = %+v reviewed %+v, want %s#%d alone", got.PullRequests, got.ReviewedPullRequest, full, tc.n)
				}
				review = got.PullRequests[0].Review
			}
			if review.State != tc.want.state || (review.Verdict != nil) != tc.want.verdict || (review.SupersededVerdict != nil) != tc.want.superseded {
				t.Fatalf("review = state %q verdict %v superseded %v, want %q %v %v", review.State, review.Verdict != nil, review.SupersededVerdict != nil, tc.want.state, tc.want.verdict, tc.want.superseded)
			}
			if review.Freshness.State != tc.want.freshness || reasonOf(review.Freshness) != tc.want.reason {
				t.Fatalf("freshness = %q (%q), want %q (%q)", review.Freshness.State, reasonOf(review.Freshness), tc.want.freshness, tc.want.reason)
			}
			if reads := host.readsOf(owner, repo, int(tc.n)); reads != tc.want.reads {
				t.Fatalf("live pull request reads = %d, want %d", reads, tc.want.reads)
			}
			if review.Verdict != nil && (review.Verdict.HeadSha != "h1" || review.Verdict.AttemptId == nil && tc.n != 111) {
				t.Fatalf("verdict = %+v, want the recorded one at h1 with its attempt", review.Verdict)
			}
			if _, ok := raw["events"]; ok {
				t.Fatal("the result carries events")
			}
			seen[string(review.State)+"/"+string(review.Freshness.State)] = true
		})
	}

	t.Run("no pull request in scope: reviewScope none, never an empty list alone", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		got, raw := getResult(t, rig, sess.ID, cookie)
		if got.ReviewScope != restdtos.SessionOutcomeReviewScopeNone || len(got.PullRequests) != 0 || got.ReviewedPullRequest != nil {
			t.Fatalf("got scope %q pullRequests %v reviewed %v, want none, [], null", got.ReviewScope, got.PullRequests, got.ReviewedPullRequest)
		}
		if list, ok := raw["pullRequests"].([]any); !ok || len(list) != 0 {
			t.Fatalf("pullRequests = %#v, want an empty array on the wire", raw["pullRequests"])
		}
		if v, ok := raw["reviewedPullRequest"]; !ok || v != nil {
			t.Fatalf("reviewedPullRequest = %#v (present %v), want null", v, ok)
		}
	})

	// Every state the row names is distinct on the wire.
	for _, pair := range []string{"absent/not_applicable", "in_progress/not_applicable", "not_assessed/not_applicable", "assessed/stale", "assessed/unconfirmed", "assessed/current", "assessed/not_applicable"} {
		if !seen[pair] {
			t.Errorf("state/freshness %q never observed", pair)
		}
	}
	if !host.tokens["result-bot-token"] || len(host.tokens) != 1 {
		t.Errorf("live reads used tokens %v, want the bot token alone", host.tokens)
	}
}

// TestResult_NeverCurrentWithoutLiveConfirmation: a verdict that reads
// current when the code host confirms every fact reads anything but current
// the moment any live fact cannot be read -- the pull request, the base
// branch, the fast-forward check -- or no code host is configured at all.
func TestResult_NeverCurrentWithoutLiveConfirmation(t *testing.T) {
	ctx := context.Background()
	const owner, repo, full = "acme", "gadgets", "acme/gadgets"

	// seed records an assessed verdict for acme/gadgets#n at h1, on base
	// (at b1), under the current policy.
	seed := func(t *testing.T, rig testRig, user sqlcgen.User, n int32, base string) sqlcgen.Session {
		sess := reviewSession(ctx, t, rig, user.ID, full, n)
		a := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
		rc := currentContext()
		rc.baseRef = str(base)
		verdictAt(ctx, t, rig, sess.ID, full, n, a.ID, "h1", rc)
		return sess
	}
	freshnessOf := func(t *testing.T, rig testRig, sess sqlcgen.Session, cookie string) restdtos.SessionOutcomeReviewFreshness {
		got, _ := getResult(t, rig, sess.ID, cookie)
		if got.ReviewedPullRequest == nil || got.ReviewedPullRequest.Review.State != restdtos.SessionOutcomeReviewStateAssessed {
			t.Fatalf("reviewedPullRequest = %+v, want an assessed verdict", got.ReviewedPullRequest)
		}
		return got.ReviewedPullRequest.Review.Freshness
	}

	host := newResultCodeHost()
	rig, user, cookie := newResultRig(t, host)

	// The baseline: every live fact readable, the base moved forward only
	// (b1 to b2, confirmed).
	host.open(owner, repo, 1, "h1", "main")
	host.branch(owner, repo, "main", "b2")
	host.ancestry["b1..b2"] = true
	if f := freshnessOf(t, rig, seed(t, rig, user, 1, "main"), cookie); f.State != restdtos.SessionOutcomeReviewFreshnessStateCurrent {
		t.Fatalf("baseline freshness = %q (%q), want current -- the rows below prove nothing otherwise", f.State, reasonOf(f))
	}

	// Each row is the baseline with one live fact made unreadable.
	breaks := []struct {
		name  string
		n     int32
		base  string
		setup func()
		want  string
	}{
		{"the pull request read fails", 2, "main", func() {
			host.set(owner, repo, 2, resultHostPR{err: errors.New("unavailable")})
		}, reviewfreshness.ReasonPullRequestUnread},
		{"the base branch cannot be resolved", 3, "base-3", func() {
			host.open(owner, repo, 3, "h1", "base-3")
		}, "the base branch's current commit could not be read from the code host"},
		{"the fast-forward check fails", 4, "base-4", func() {
			host.open(owner, repo, 4, "h1", "base-4")
			host.branch(owner, repo, "base-4", "b4") // b1..b4: the fake cannot say
		}, "the base branch moved, and whether it only moved forward could not be confirmed with the code host"},
	}
	for _, b := range breaks {
		t.Run(b.name, func(t *testing.T) {
			b.setup()
			sess := seed(t, rig, user, b.n, b.base)
			f := freshnessOf(t, rig, sess, cookie)
			if f.State != restdtos.SessionOutcomeReviewFreshnessStateUnconfirmed || reasonOf(f) != b.want {
				t.Fatalf("freshness = %q (%q), want unconfirmed (%q) -- never current without live confirmation", f.State, reasonOf(f), b.want)
			}
		})
	}

	t.Run("no code host configured", func(t *testing.T) {
		bare, member, memberCookie := newResultRig(t, nil)
		sess := seed(t, bare, member, 5, "main")
		f := freshnessOf(t, bare, sess, memberCookie)
		if f.State != restdtos.SessionOutcomeReviewFreshnessStateUnconfirmed || reasonOf(f) != reviewfreshness.ReasonNoCodeHost {
			t.Fatalf("freshness = %q (%q), want unconfirmed, no code host", f.State, reasonOf(f))
		}
	})
}

// TestResult_OwnPushMovedHeadIsNotCurrent pins §43.20's stated limit from
// the result's side: a pull request review session's own push moves the
// pull request's head after its verdict, and until the code host's
// notification of that push arms a re-review the status reads finished and
// settled -- but the result reads the head live, so the pre-push verdict is
// stale there, never current.
func TestResult_OwnPushMovedHeadIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	host := newResultCodeHost()
	rig, user, cookie := newResultRig(t, host)
	const owner, repo, full, n = "acme", "gizmos", "acme/gizmos", 7

	sess := reviewSession(ctx, t, rig, user.ID, full, n)
	a := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
	verdictAt(ctx, t, rig, sess.ID, full, n, a.ID, "before-push", currentContext())
	host.branch(owner, repo, "main", "b1")

	host.open(owner, repo, n, "before-push", "main")
	got, _ := getResult(t, rig, sess.ID, cookie)
	if f := got.ReviewedPullRequest.Review.Freshness; f.State != restdtos.SessionOutcomeReviewFreshnessStateCurrent {
		t.Fatalf("before the push: freshness %q (%q), want current", f.State, reasonOf(f))
	}

	// The session's own push lands on the pull request's head; nothing is
	// recorded, and no notification has arrived to arm a re-review.
	host.open(owner, repo, n, "after-push", "main")
	status := getStatus(t, rig, sess.ID, cookie)
	if status.Activity != restdtos.SessionActivityActivityFinished || !status.Settled {
		t.Fatalf("status after the push = %q settled %v, want finished and settled -- the stated limit this test is about", status.Activity, status.Settled)
	}
	got, _ = getResult(t, rig, sess.ID, cookie)
	f := got.ReviewedPullRequest.Review.Freshness
	if f.State != restdtos.SessionOutcomeReviewFreshnessStateStale || reasonOf(f) != string(autoapproval.ReasonStaleVerdict) {
		t.Fatalf("after the push: freshness %q (%q), want stale: the verdict was produced against an earlier commit", f.State, reasonOf(f))
	}
	if got.Activity != restdtos.SessionOutcomeActivity(status.Activity) {
		t.Fatalf("result activity %q, status %q: want the same", got.Activity, status.Activity)
	}
}

// TestResult_LastRunFailureReasonOnlyWhenDerivable: the result's lastRun
// gives the session's recorded failure reason only when it can describe
// nothing but the last run, exactly as the status's lastRun does (one
// rule): the run is the newest turn, did not complete, and the session's
// recorded outcome is that run's.
func TestResult_LastRunFailureReasonOnlyWhenDerivable(t *testing.T) {
	ctx := context.Background()
	rig, user, cookie := newResultRig(t, nil)
	reason := func(r sqlcgen.SessionFailureReason) *sqlcgen.SessionFailureReason { return &r }

	tests := []struct {
		name       string
		end        []turn.Trigger
		newerTurn  bool
		recorded   sqlcgen.SessionStatus
		recordedFR *sqlcgen.SessionFailureReason
		want       string
	}{
		{"failed, newest, recorded failed: timeout", []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing, turn.TriggerFail}, false, sqlcgen.SessionStatusFailed, reason(sqlcgen.SessionFailureReasonTimeout), "timeout"},
		{"cancelled, newest, recorded cancelled", []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing, turn.TriggerCancel}, false, sqlcgen.SessionStatusCancelled, reason(sqlcgen.SessionFailureReasonCancelled), "cancelled"},
		{"completed: no reason", endedCompleted, false, sqlcgen.SessionStatusCompleted, nil, ""},
		{"failed, but a newer turn exists", []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing, turn.TriggerFail}, true, sqlcgen.SessionStatusFailed, reason(sqlcgen.SessionFailureReasonFailed), ""},
		{"failed, but the recorded outcome is another's", []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing, turn.TriggerFail}, false, sqlcgen.SessionStatusCompleted, reason(sqlcgen.SessionFailureReasonFailed), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := createSessionForUser(ctx, t, rig, user.ID, nil)
			run := createTurn(ctx, t, rig.turns, sess.ID, false)
			state := turn.StatePending
			for _, trig := range tc.end {
				state = transitionTurn(ctx, t, rig.turns, run.ID, state, trig)
			}
			if tc.newerTurn {
				createTurn(ctx, t, rig.turns, sess.ID, false)
			}
			if _, err := rig.sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sess.ID, Status: tc.recorded, FailureReason: tc.recordedFR}); err != nil {
				t.Fatalf("record the session's outcome: %v", err)
			}
			got, _ := getResult(t, rig, sess.ID, cookie)
			if got.LastRun == nil || got.LastRun.TurnId != run.ID.String() || string(got.LastRun.Outcome) != string(state) {
				t.Fatalf("lastRun = %+v, want the run, %s", got.LastRun, state)
			}
			var gotReason string
			if got.LastRun.FailureReason != nil {
				gotReason, _ = got.LastRun.FailureReason.Value.(string)
			}
			if gotReason != tc.want {
				t.Fatalf("failureReason = %q, want %q", gotReason, tc.want)
			}
			status := getStatus(t, rig, sess.ID, cookie)
			var statusReason string
			if status.LastRun != nil && status.LastRun.FailureReason != nil {
				statusReason, _ = status.LastRun.FailureReason.Value.(string)
			}
			if statusReason != gotReason {
				t.Fatalf("status lastRun.failureReason %q, result %q: want the one rule", statusReason, gotReason)
			}
			if got.LastRun.StartedAt == nil || got.LastRun.FinishedAt == nil {
				t.Fatalf("lastRun times = %v %v, want both recorded", got.LastRun.StartedAt, got.LastRun.FinishedAt)
			}
		})
	}
}

// tokenFrame records one streamed text frame of part, as the session actor
// stores per-frame rows (a storage key per frame, the part id in the
// payload).
func tokenFrame(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, storageKey, part, text string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"type": "token", "messageId": part, "text": text})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "token", MessageID: storageKey, Payload: payload}); err != nil {
		t.Fatalf("record token frame: %v", err)
	}
}

// dispatchedRun creates a turn, stamps its dispatch at the session's event
// watermark, and returns it with the watermark.
func dispatchedRun(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	run := createTurn(ctx, t, r.turns, sessionID, false)
	watermark, err := r.events.MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: run.ID, Status: sqlcgen.TurnStatusDispatched, DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, DispatchedEventID: &watermark}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return run
}

// sessionOutcomeKeys is SessionOutcome's own property list from
// /contracts, and lastRunKeys its lastRun's: the result carries exactly
// these, so no transcript rides along with it.
func sessionOutcomeKeys(t *testing.T) (top, lastRun []string) {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	def := doc.Defs["SessionOutcome"]
	for name := range def.Properties {
		top = append(top, name)
	}
	for name := range def.Properties["lastRun"].Properties {
		lastRun = append(lastRun, name)
	}
	sort.Strings(top)
	sort.Strings(lastRun)
	return top, lastRun
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestResult_SummaryBoundedAndNoTranscript: the summary is the last run's
// final text -- the text part that opened last in its own window, read at
// its newest frame, never the newest row -- cut at 4,000 characters with
// truncated set, null when the run left no text, never a later turn's text;
// and the body is exactly SessionOutcome's keys, with no events.
func TestResult_SummaryBoundedAndNoTranscript(t *testing.T) {
	ctx := context.Background()
	rig, user, cookie := newResultRig(t, nil)
	wantTop, wantLastRun := sessionOutcomeKeys(t)

	summaryOf := func(t *testing.T, sessionID pgtype.UUID) (restdtos.SessionOutcomeLastRunSummary, map[string]any) {
		t.Helper()
		got, raw := getResult(t, rig, sessionID, cookie)
		if strings.Join(keysOf(raw), ",") != strings.Join(wantTop, ",") {
			t.Fatalf("result keys %v, want exactly SessionOutcome's %v", keysOf(raw), wantTop)
		}
		lastRun, _ := raw["lastRun"].(map[string]any)
		if strings.Join(keysOf(lastRun), ",") != strings.Join(wantLastRun, ",") {
			t.Fatalf("lastRun keys %v, want exactly %v", keysOf(lastRun), wantLastRun)
		}
		return got.LastRun.Summary, raw
	}

	t.Run("the part that opened last, not the newest row; never a later turn's text", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		run := dispatchedRun(ctx, t, rig, sess.ID)
		tokenFrame(ctx, t, rig, sess.ID, "prt_a", "prt_a", "Looking at the code")
		tokenFrame(ctx, t, rig, sess.ID, "prt_b", "prt_b", "Done: the fix")
		tokenFrame(ctx, t, rig, sess.ID, "prt_b#2", "prt_b", "Done: the fix is in place.")
		// The earlier part's frame stored last (the replay at a deploy):
		// the newest row, and not the final text.
		tokenFrame(ctx, t, rig, sess.ID, "prt_a#2", "prt_a", "Looking at the code, replayed")
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateDispatched, turn.TriggerStartProcessing)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateProcessing, turn.TriggerComplete)
		// A later turn in flight, with text of its own.
		later := dispatchedRun(ctx, t, rig, sess.ID)
		tokenFrame(ctx, t, rig, sess.ID, "prt_later", "prt_later", "A LATER TURN'S TEXT")

		summary, raw := summaryOf(t, sess.ID)
		if summary.Text == nil || *summary.Text != "Done: the fix is in place." || summary.Truncated {
			t.Fatalf("summary = %v truncated %v, want the last-opened part's newest frame", summary.Text, summary.Truncated)
		}
		if lastRun, _ := raw["lastRun"].(map[string]any); lastRun["turnId"] != run.ID.String() || lastRun["turnId"] == later.ID.String() {
			t.Fatalf("lastRun = %v, want the ended run", lastRun)
		}
	})

	t.Run("cut at 4,000 characters, never inside one", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		run := dispatchedRun(ctx, t, rig, sess.ID)
		long := strings.Repeat("é", 3990) + strings.Repeat("字", 50)
		tokenFrame(ctx, t, rig, sess.ID, "prt_long", "prt_long", long)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateDispatched, turn.TriggerStartProcessing)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateProcessing, turn.TriggerComplete)

		summary, _ := summaryOf(t, sess.ID)
		if summary.Text == nil || !summary.Truncated {
			t.Fatalf("summary = %v truncated %v, want a cut text", summary.Text, summary.Truncated)
		}
		if n := utf8.RuneCountInString(*summary.Text); n != 4000 || !utf8.ValidString(*summary.Text) || !strings.HasPrefix(long, *summary.Text) {
			t.Fatalf("summary is %d characters (valid %v), want the first 4,000 of the text", n, utf8.ValidString(*summary.Text))
		}
	})

	t.Run("exactly 4,000 characters: not truncated", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		run := dispatchedRun(ctx, t, rig, sess.ID)
		exact := strings.Repeat("x", 4000)
		tokenFrame(ctx, t, rig, sess.ID, "prt_x", "prt_x", exact)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateDispatched, turn.TriggerStartProcessing)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateProcessing, turn.TriggerComplete)
		if summary, _ := summaryOf(t, sess.ID); summary.Text == nil || *summary.Text != exact || summary.Truncated {
			t.Fatalf("summary truncated %v, want the whole text, not truncated", summary.Truncated)
		}
	})

	t.Run("no text at all: null, not the plan placeholder", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		run := dispatchedRun(ctx, t, rig, sess.ID)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateDispatched, turn.TriggerStartProcessing)
		transitionTurn(ctx, t, rig.turns, run.ID, turn.StateProcessing, turn.TriggerComplete)
		summary, raw := summaryOf(t, sess.ID)
		if summary.Text != nil || summary.Truncated {
			t.Fatalf("summary = %q truncated %v, want null", *summary.Text, summary.Truncated)
		}
		if s, _ := raw["lastRun"].(map[string]any)["summary"].(map[string]any); s["text"] != nil {
			t.Fatalf("summary.text = %#v on the wire, want null", s["text"])
		}
	})

	t.Run("no run ended: lastRun null", func(t *testing.T) {
		sess := createSessionForUser(ctx, t, rig, user.ID, nil)
		createTurn(ctx, t, rig.turns, sess.ID, false)
		got, raw := getResult(t, rig, sess.ID, cookie)
		if got.LastRun != nil || raw["lastRun"] != nil || got.Activity != restdtos.SessionOutcomeActivityQueued {
			t.Fatalf("lastRun %+v activity %q, want null and queued", got.LastRun, got.Activity)
		}
	})
}

// TestResult_ProducedPRRepoFullNameFromSessionRepos: a pull request
// artifact records only the bare repository name; the result names its
// owner from the session's own repository of that name -- the derivation
// that opened the pull request -- and falls back to the URL only when the
// session's repositories cannot say.
func TestResult_ProducedPRRepoFullNameFromSessionRepos(t *testing.T) {
	ctx := context.Background()
	rig, user, cookie := newResultRig(t, nil)

	tests := []struct {
		name  string
		repos map[string]string
		url   string
		want  string
	}{
		{"the session's repository names the owner", map[string]string{"widgets": "https://github.com/acme/widgets.git"}, "https://github.com/acme/widgets/pull/12", "acme/widgets"},
		{"the session's repository wins over the URL", map[string]string{"widgets": "https://github.com/acme/widgets"}, "https://github.com/elsewhere/widgets/pull/12", "acme/widgets"},
		{"the session's repositories are ambiguous: the URL", map[string]string{"a": "https://github.com/one/widgets", "b": "https://github.com/two/widgets"}, "https://github.com/two/widgets/pull/12", "two/widgets"},
		{"no session repository of that name: the URL", map[string]string{"other": "https://github.com/acme/other"}, "https://github.com/acme/widgets/pull/12", "acme/widgets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessionWithRepos(ctx, t, rig, user.ID, tc.repos)
			prArtifact(ctx, t, rig, sess.ID, "widgets", 12, tc.url)
			got, _ := getResult(t, rig, sess.ID, cookie)
			if len(got.PullRequests) != 1 || got.PullRequests[0].RepoFullName != tc.want || got.PullRequests[0].Number != 12 || got.PullRequests[0].Url != tc.url {
				t.Fatalf("pullRequests = %+v, want %s#12 at %s", got.PullRequests, tc.want, tc.url)
			}
		})
	}

	t.Run("the verdict is looked up under the derived name", func(t *testing.T) {
		sess := sessionWithRepos(ctx, t, rig, user.ID, map[string]string{"widgets": "https://github.com/acme/widgets"})
		prArtifact(ctx, t, rig, sess.ID, "widgets", 13, "https://github.com/acme/widgets/pull/13")
		reviewer := reviewSession(ctx, t, rig, user.ID, "acme/widgets", 13)
		reviewAttempt(ctx, t, rig, reviewer.ID, endedCompleted...)
		got, _ := getResult(t, rig, sess.ID, cookie)
		if len(got.PullRequests) != 1 || got.PullRequests[0].Review.State != restdtos.SessionOutcomeReviewStateNotAssessed {
			t.Fatalf("pullRequests = %+v, want acme/widgets#13 not_assessed (its review session's attempt ended without a verdict)", got.PullRequests)
		}
	})
}

// TestResult_UnknownAndMalformedSession: the status route's own gate.
func TestResult_UnknownAndMalformedSession(t *testing.T) {
	rig, _, cookie := newResultRig(t, nil)
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/00000000-0000-0000-0000-000000000000/result", nil, nil, cookie); status != http.StatusNotFound {
		t.Errorf("unknown session: %d, want 404", status)
	}
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/not-a-uuid/result", nil, nil, cookie); status != http.StatusBadRequest {
		t.Errorf("malformed id: %d, want 400", status)
	}
}

// neverCreates is the live side under the production shadow decorator in
// the tests below: a suppressed creation must never reach it.
type neverCreates struct {
	ports.SourceControl
	t *testing.T
}

func (n neverCreates) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	n.t.Error("the live CreatePR was called for a creation the decorator suppresses")
	return ports.PRRef{}, errors.New("never")
}

// suppressedPRArtifact makes the production shadow decorator suppress a
// pull request creation on owner/repo -- the repository resolving to
// shadow, as a repository never promoted does -- and records the ref it
// hands back exactly as the session actor's recordPRArtifact records any
// created pull request: the bare repository name and the ref's number as
// metadata, the ref's URL as the URL.
func suppressedPRArtifact(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, owner, repo string) ports.PRRef {
	t.Helper()
	decorator, err := shadowscm.New(neverCreates{t: t}, r.shadowLedger, func(context.Context, string) bool { return false })
	if err != nil {
		t.Fatalf("build the shadow decorator: %v", err)
	}
	ref, err := decorator.CreatePR(ctx, ports.CreatePRSpec{Owner: owner, Repo: repo, Head: "narvi/shadow-" + repo, Base: "main", Title: "t", Body: "b", Token: "creator-token"})
	if err != nil {
		t.Fatalf("suppressed CreatePR: %v", err)
	}
	if !shadowscm.IsSyntheticPRRef(ref) {
		t.Fatalf("the decorator answered %+v, want its synthetic ref -- this test proves nothing otherwise", ref)
	}
	meta, err := json.Marshal(map[string]any{"repo": repo, "number": ref.Number})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if _, err := r.artifacts.Create(ctx, sqlcgen.CreateArtifactParams{SessionID: sessionID, Type: sqlcgen.ArtifactTypePr, Url: ref.URL, Metadata: meta}); err != nil {
		t.Fatalf("record the suppressed pull request artifact: %v", err)
	}
	return ref
}

// endedRun records a run that completed, so the result has a lastRun.
func endedRun(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	run := createTurn(ctx, t, r.turns, sessionID, false)
	state := turn.StatePending
	for _, trig := range endedCompleted {
		state = transitionTurn(ctx, t, r.turns, run.ID, state, trig)
	}
	return run
}

// TestResult_ExcludedPullRequestsNeverFailTheResult is finding P1's
// regression on real Postgres: the artifact the production shadow decorator
// leaves when it suppresses a pull request creation -- the default for
// every repository never promoted -- and a record this build cannot read
// each answer 200, listed in excludedPullRequests with why, never as a pull
// request and never dropped, while the rest of the result -- the last run,
// a real pull request beside them and its verdict's live freshness --
// stands.
func TestResult_ExcludedPullRequestsNeverFailTheResult(t *testing.T) {
	ctx := context.Background()
	host := newResultCodeHost()
	rig, user, cookie := newResultRig(t, host)
	repos := map[string]string{"widgets": "https://github.com/acme/widgets.git", "gadgets": "https://github.com/acme/gadgets.git"}

	// exclusionsOf reads the result and returns its excluded entries, with
	// the raw ones, asserting no pull request is the synthetic one.
	exclusionsOf := func(t *testing.T, sessionID pgtype.UUID) (restdtos.SessionOutcome, []map[string]any) {
		t.Helper()
		got, raw := getResult(t, rig, sessionID, cookie)
		for _, pr := range got.PullRequests {
			if pr.Number < 1 || strings.HasPrefix(pr.Url, "shadow-suppressed://") {
				t.Fatalf("pullRequests lists %+v, which is no pull request", pr)
			}
		}
		list, ok := raw["excludedPullRequests"].([]any)
		if !ok {
			t.Fatalf("excludedPullRequests = %#v, want an array on the wire", raw["excludedPullRequests"])
		}
		var entries []map[string]any
		for _, e := range list {
			entries = append(entries, e.(map[string]any))
		}
		if got.LastRun == nil {
			t.Fatal("lastRun is null: the result did not stand around the excluded record")
		}
		return got, entries
	}
	wantShadow := func(t *testing.T, got restdtos.SessionOutcomeExcludedPullRequest, raw map[string]any) {
		t.Helper()
		if got.Kind != restdtos.SessionOutcomeExcludedPullRequestKindShadowSuppressed || got.RepoFullName == nil || *got.RepoFullName != "acme/widgets" || got.Reason == "" {
			t.Fatalf("excluded = %+v, want shadow_suppressed on acme/widgets with a reason", got)
		}
		if v, ok := raw["url"]; !ok || v != nil {
			t.Fatalf("a suppressed creation's url = %#v (present %v), want null: no pull request exists", v, ok)
		}
	}

	t.Run("a suppressed creation alone", func(t *testing.T) {
		sess := sessionWithRepos(ctx, t, rig, user.ID, repos)
		endedRun(ctx, t, rig, sess.ID)
		suppressedPRArtifact(ctx, t, rig, sess.ID, "acme", "widgets")
		got, raw := exclusionsOf(t, sess.ID)
		if len(got.PullRequests) != 0 || got.ReviewScope != restdtos.SessionOutcomeReviewScopeNone || len(got.ExcludedPullRequests) != 1 {
			t.Fatalf("pullRequests %+v scope %q excluded %+v, want none opened, scope none, the suppressed one excluded", got.PullRequests, got.ReviewScope, got.ExcludedPullRequests)
		}
		wantShadow(t, got.ExcludedPullRequests[0], raw[0])
	})

	t.Run("a suppressed creation beside a real pull request and its verdict", func(t *testing.T) {
		sess := sessionWithRepos(ctx, t, rig, user.ID, repos)
		endedRun(ctx, t, rig, sess.ID)
		suppressedPRArtifact(ctx, t, rig, sess.ID, "acme", "widgets")
		prArtifact(ctx, t, rig, sess.ID, "gadgets", 7, "https://github.com/acme/gadgets/pull/7")
		reviewer := reviewSession(ctx, t, rig, user.ID, "acme/gadgets", 7)
		a := reviewAttempt(ctx, t, rig, reviewer.ID, endedCompleted...)
		verdictAt(ctx, t, rig, reviewer.ID, "acme/gadgets", 7, a.ID, "h1", currentContext())
		host.open("acme", "gadgets", 7, "h1", "main")
		host.branch("acme", "gadgets", "main", "b1")

		got, raw := exclusionsOf(t, sess.ID)
		if len(got.PullRequests) != 1 || got.PullRequests[0].RepoFullName != "acme/gadgets" || got.PullRequests[0].Number != 7 {
			t.Fatalf("pullRequests = %+v, want acme/gadgets#7 alone", got.PullRequests)
		}
		if f := got.PullRequests[0].Review.Freshness; got.PullRequests[0].Review.State != restdtos.SessionOutcomeReviewStateAssessed || f.State != restdtos.SessionOutcomeReviewFreshnessStateCurrent {
			t.Fatalf("acme/gadgets#7 review = %+v, want assessed and current, read live", got.PullRequests[0].Review)
		}
		if got.ReviewScope != restdtos.SessionOutcomeReviewScopeProduced || len(got.ExcludedPullRequests) != 1 {
			t.Fatalf("scope %q excluded %+v, want produced, the suppressed one excluded", got.ReviewScope, got.ExcludedPullRequests)
		}
		wantShadow(t, got.ExcludedPullRequests[0], raw[0])
	})

	t.Run("a record this build cannot read, beside a real pull request", func(t *testing.T) {
		sess := sessionWithRepos(ctx, t, rig, user.ID, repos)
		endedRun(ctx, t, rig, sess.ID)
		// Valid JSON (the column is jsonb), of no shape the actor writes.
		if _, err := rig.artifacts.Create(ctx, sqlcgen.CreateArtifactParams{SessionID: sess.ID, Type: sqlcgen.ArtifactTypePr, Url: "https://code.example.invalid/somewhere", Metadata: []byte(`{"repo":["not","a","name"],"number":"seven"}`)}); err != nil {
			t.Fatalf("record the garbage artifact: %v", err)
		}
		prArtifact(ctx, t, rig, sess.ID, "gadgets", 8, "https://github.com/acme/gadgets/pull/8")

		got, raw := exclusionsOf(t, sess.ID)
		if len(got.PullRequests) != 1 || got.PullRequests[0].Number != 8 {
			t.Fatalf("pullRequests = %+v, want acme/gadgets#8 alone", got.PullRequests)
		}
		if len(got.ExcludedPullRequests) != 1 {
			t.Fatalf("excluded = %+v, want the garbage record", got.ExcludedPullRequests)
		}
		e := got.ExcludedPullRequests[0]
		if e.Kind != restdtos.SessionOutcomeExcludedPullRequestKindUnreadable || e.Reason == "" || e.Url == nil || *e.Url != "https://code.example.invalid/somewhere" {
			t.Fatalf("excluded = %+v, want unreadable, with a reason and the URL it holds", e)
		}
		if v, ok := raw[0]["repoFullName"]; !ok || v != nil {
			t.Fatalf("an unreadable record's repoFullName = %#v (present %v), want null", v, ok)
		}
	})

	t.Run("no pull request record at all: an empty array, never absent", func(t *testing.T) {
		sess := sessionWithRepos(ctx, t, rig, user.ID, repos)
		endedRun(ctx, t, rig, sess.ID)
		if got, entries := exclusionsOf(t, sess.ID); len(entries) != 0 || len(got.ExcludedPullRequests) != 0 {
			t.Fatalf("excluded = %+v, want []", got.ExcludedPullRequests)
		}
	})
}

// stallingCodeHost never answers a pull request read before its caller
// gives up, and counts the reads it was asked for.
type stallingCodeHost struct {
	ports.SourceControl
	started atomic.Int32
}

func (h *stallingCodeHost) GetOpenPR(ctx context.Context, _, _ string, _ int, _ string) (ports.OpenPR, bool, error) {
	h.started.Add(1)
	<-ctx.Done()
	return ports.OpenPR{}, false, ctx.Err()
}

// TestResult_LiveReadBudgetBoundsTheCall is finding P4's bound: a code host
// that never answers holds a result call for SessionResultLiveReadBudget --
// every assessed pull request's read at once, not one after another -- and
// not for the per-read bounds, which add up past the MCP wait bound; each
// verdict then reads unconfirmed, saying it ran out of time.
func TestResult_LiveReadBudgetBoundsTheCall(t *testing.T) {
	ctx := context.Background()
	host := &stallingCodeHost{}
	const budget = 300 * time.Millisecond
	rig := newTestRig(t, func(r *testRig) {
		r.resultSourceControl = host
		r.resultTimeouts.SessionResultLiveReadBudget = budget
	})
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	if per := rig.resultTimeouts.GitHubGetOpenPRTimeout; per < 10*budget {
		t.Fatalf("GitHubGetOpenPRTimeout %v is not far above the budget %v: this test could not tell them apart", per, budget)
	}

	// A review session of acme/slow#1 that also opened acme/slow#2, both
	// assessed: two live reads.
	sess := sessionWithRepos(ctx, t, rig, user.ID, map[string]string{"slow": "https://github.com/acme/slow.git"})
	if err := rig.prSessions.EnsureRow(ctx, "acme/slow", 1); err != nil {
		t.Fatalf("ensure claim row: %v", err)
	}
	if err := rig.prSessions.SetSessionID(ctx, "acme/slow", 1, sess.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	a := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
	verdictAt(ctx, t, rig, sess.ID, "acme/slow", 1, a.ID, "h1", currentContext())
	prArtifact(ctx, t, rig, sess.ID, "slow", 2, "https://github.com/acme/slow/pull/2")
	verdictAt(ctx, t, rig, sess.ID, "acme/slow", 2, pgtype.UUID{}, "h1", currentContext())

	began := time.Now()
	got, _ := getResult(t, rig, sess.ID, cookie)
	elapsed := time.Since(began)

	if elapsed < budget || elapsed > budget+5*time.Second {
		t.Fatalf("the call took %v, want the budget %v and little more", elapsed, budget)
	}
	if n := host.started.Load(); n != 2 {
		t.Fatalf("pull request reads begun = %d, want both, at once", n)
	}
	if got.ReviewedPullRequest == nil || len(got.PullRequests) != 1 {
		t.Fatalf("result = %+v, want the reviewed and the opened pull request", got)
	}
	for _, review := range []restdtos.SessionOutcomeReview{got.ReviewedPullRequest.Review, got.PullRequests[0].Review} {
		if review.Freshness.State != restdtos.SessionOutcomeReviewFreshnessStateUnconfirmed || reasonOf(review.Freshness) != reviewfreshness.ReasonOutOfTime {
			t.Fatalf("freshness = %q (%q), want unconfirmed, out of time", review.Freshness.State, reasonOf(review.Freshness))
		}
	}
	if got.SuggestedDelaySeconds != int(rig.resultTimeouts.SessionResultDelayLiveRead/time.Second) {
		t.Fatalf("suggestedDelaySeconds = %d, want the settled, read-live value", got.SuggestedDelaySeconds)
	}
}

// TestResult_SuggestedDelay pins suggestedDelaySeconds on the wire: the
// unsettled value while the result can still change on its own -- the
// session is not settled, or the review session behind the review of a
// pull request it opened is not (round 2's finding P5: a review running,
// queued, or re-run on a schedule changes the result with no new input,
// so the 300 seconds "nothing changes" is wrong there) -- the read-live
// value once nothing can and a verdict's freshness was read live, and the
// settled value when nothing was: including when no code host is
// configured, so nothing could be.
func TestResult_SuggestedDelay(t *testing.T) {
	ctx := context.Background()
	to := platform.DefaultTimeouts()
	seconds := func(d time.Duration) int { return int(d / time.Second) }
	assessedAndCurrent := func(t *testing.T, rig testRig, user sqlcgen.User, host *resultCodeHost) sqlcgen.Session {
		sess := reviewSession(ctx, t, rig, user.ID, "acme/delays", 3)
		a := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
		verdictAt(ctx, t, rig, sess.ID, "acme/delays", 3, a.ID, "h1", currentContext())
		if host != nil {
			host.open("acme", "delays", 3, "h1", "main")
			host.branch("acme", "delays", "main", "b1")
		}
		return sess
	}

	host := newResultCodeHost()
	rig, user, cookie := newResultRig(t, host)
	timers := narvipg.NewTimerStore(rig.pool)

	// opened is a finished session that opened acme/<repo>#1, and that pull
	// request's own review session -- another session, whose activity is
	// the producer's result's only way to change on its own.
	opened := func(t *testing.T, repo string) (producer, reviewer sqlcgen.Session) {
		t.Helper()
		producer = sessionWithRepos(ctx, t, rig, user.ID, map[string]string{repo: "https://github.com/acme/" + repo + ".git"})
		endedRun(ctx, t, rig, producer.ID)
		prArtifact(ctx, t, rig, producer.ID, repo, 1, "https://github.com/acme/"+repo+"/pull/1")
		return producer, reviewSession(ctx, t, rig, user.ID, "acme/"+repo, 1)
	}

	tests := []struct {
		name  string
		setup func(t *testing.T) sqlcgen.Session
		state restdtos.SessionOutcomeReviewState // the opened pull request's, when there is one
		want  int
	}{
		{"queued: unsettled", func(t *testing.T) sqlcgen.Session {
			queued := createSessionForUser(ctx, t, rig, user.ID, nil)
			createTurn(ctx, t, rig.turns, queued.ID, false)
			return queued
		}, "", seconds(to.SessionResultDelayUnsettled)},
		{"finished, nothing to read live: settled", func(t *testing.T) sqlcgen.Session {
			finished := createSessionForUser(ctx, t, rig, user.ID, nil)
			endedRun(ctx, t, rig, finished.ID)
			return finished
		}, "", seconds(to.SessionResultDelaySettled)},
		{"finished, a verdict read live: read-live", func(t *testing.T) sqlcgen.Session {
			return assessedAndCurrent(t, rig, user, host)
		}, "", seconds(to.SessionResultDelayLiveRead)},
		{"finished, the review of a pull request it opened is running: unsettled", func(t *testing.T) sqlcgen.Session {
			producer, reviewer := opened(t, "delays-running")
			reviewAttempt(ctx, t, rig, reviewer.ID, turn.TriggerDispatch, turn.TriggerStartProcessing)
			return producer
		}, restdtos.SessionOutcomeReviewStateInProgress, seconds(to.SessionResultDelayUnsettled)},
		{"finished, the review of a pull request it opened is queued: unsettled", func(t *testing.T) sqlcgen.Session {
			producer, reviewer := opened(t, "delays-queued")
			reviewAttempt(ctx, t, rig, reviewer.ID)
			return producer
		}, restdtos.SessionOutcomeReviewStateInProgress, seconds(to.SessionResultDelayUnsettled)},
		{"finished, a re-review of a pull request it opened is scheduled: unsettled", func(t *testing.T) sqlcgen.Session {
			producer, reviewer := opened(t, "delays-scheduled")
			reviewAttempt(ctx, t, rig, reviewer.ID, endedCompleted...)
			// The repository opted in, its budget unspent, and a push's
			// debounce armed: the review runs again with no new input.
			if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, auto_retrigger_review_enabled) VALUES ('acme/delays-scheduled', true)`); err != nil {
				t.Fatalf("opt the repository in: %v", err)
			}
			if _, err := timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: reviewer.ID, Name: sessionactor.TimerReviewRetriggerDebounce, FiresAt: pgtype.Timestamptz{Time: time.Now().Add(2 * time.Minute), Valid: true}}); err != nil {
				t.Fatalf("arm the re-review: %v", err)
			}
			return producer
		}, restdtos.SessionOutcomeReviewStateNotAssessed, seconds(to.SessionResultDelayUnsettled)},
		{"finished, the review of a pull request it opened ended, nothing to read live: settled", func(t *testing.T) sqlcgen.Session {
			producer, reviewer := opened(t, "delays-ended")
			reviewAttempt(ctx, t, rig, reviewer.ID, endedCompleted...)
			return producer
		}, restdtos.SessionOutcomeReviewStateNotAssessed, seconds(to.SessionResultDelaySettled)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := tc.setup(t)
			got, raw := getResult(t, rig, sess.ID, cookie)
			if tc.state != "" && (len(got.PullRequests) != 1 || got.PullRequests[0].Review.State != tc.state || got.Activity != restdtos.SessionOutcomeActivityFinished) {
				t.Fatalf("activity %q pullRequests %+v, want finished with one opened pull request whose review is %q -- this row proves nothing otherwise", got.Activity, got.PullRequests, tc.state)
			}
			if got.SuggestedDelaySeconds != tc.want {
				t.Fatalf("suggestedDelaySeconds = %d, want %d", got.SuggestedDelaySeconds, tc.want)
			}
			if _, ok := raw["suggestedDelaySeconds"].(float64); !ok {
				t.Fatalf("suggestedDelaySeconds = %#v on the wire, want a number", raw["suggestedDelaySeconds"])
			}
		})
	}

	// The finding's own sequence: nothing new reaches the producing
	// session, yet its result changes when the review ends -- short while
	// it runs, the live-read value once its verdict is read live.
	t.Run("the review ends with no new input: short while it runs, then read-live", func(t *testing.T) {
		producer, reviewer := opened(t, "delays-sequence")
		attempt := reviewAttempt(ctx, t, rig, reviewer.ID, turn.TriggerDispatch, turn.TriggerStartProcessing)
		got, _ := getResult(t, rig, producer.ID, cookie)
		if got.PullRequests[0].Review.State != restdtos.SessionOutcomeReviewStateInProgress || got.SuggestedDelaySeconds != seconds(to.SessionResultDelayUnsettled) {
			t.Fatalf("while the review runs: %q, %ds, want in_progress, %ds", got.PullRequests[0].Review.State, got.SuggestedDelaySeconds, seconds(to.SessionResultDelayUnsettled))
		}
		transitionTurn(ctx, t, rig.turns, attempt.ID, turn.StateProcessing, turn.TriggerComplete)
		verdictAt(ctx, t, rig, reviewer.ID, "acme/delays-sequence", 1, attempt.ID, "h1", currentContext())
		host.open("acme", "delays-sequence", 1, "h1", "main")
		host.branch("acme", "delays-sequence", "main", "b1")
		got, _ = getResult(t, rig, producer.ID, cookie)
		if r := got.PullRequests[0].Review; r.State != restdtos.SessionOutcomeReviewStateAssessed || r.Freshness.State != restdtos.SessionOutcomeReviewFreshnessStateCurrent || got.SuggestedDelaySeconds != seconds(to.SessionResultDelayLiveRead) {
			t.Fatalf("once the review ended: %q/%q, %ds, want assessed/current, %ds", r.State, r.Freshness.State, got.SuggestedDelaySeconds, seconds(to.SessionResultDelayLiveRead))
		}
	})

	t.Run("no code host: nothing is read live, settled", func(t *testing.T) {
		bare, member, memberCookie := newResultRig(t, nil)
		sess := assessedAndCurrent(t, bare, member, nil)
		got, _ := getResult(t, bare, sess.ID, memberCookie)
		if got.SuggestedDelaySeconds != seconds(to.SessionResultDelaySettled) {
			t.Fatalf("suggestedDelaySeconds = %d, want the settled value", got.SuggestedDelaySeconds)
		}
	})
}

// TestResult_OneSnapshot_NeverMixesInstants proves the handler's one
// repeatable-read snapshot (finding P5): the result's first read is taken,
// then -- while the handler waits on review_verdicts, locked by another
// transaction -- a newer review attempt and that attempt's verdict are
// committed. The result must report the review exactly as it stood at its
// snapshot: the older attempt's verdict, assessed. Read outside the
// snapshot (a store bound to the pool, or a weaker isolation level), the
// newer verdict would be paired with the older attempt.
func TestResult_OneSnapshot_NeverMixesInstants(t *testing.T) {
	ctx := context.Background()
	rig, user, cookie := newResultRig(t, nil)
	const full, n = "acme/snapshot", 5

	sess := reviewSession(ctx, t, rig, user.ID, full, n)
	older := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
	olderVerdict := verdictAt(ctx, t, rig, sess.ID, full, n, older.ID, "h1", currentContext())

	lockTx, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }()
	if _, err := lockTx.Exec(ctx, "LOCK TABLE review_verdicts IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock review_verdicts: %v", err)
	}

	var got restdtos.SessionOutcome
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		// Not rig.doJSON: that fails the test from its own goroutine.
		req, err := http.NewRequestWithContext(gctx, http.MethodGet, rig.server.URL+"/api/sessions/"+sess.ID.String()+"/result", nil)
		if err != nil {
			return err
		}
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET result: %d (%s)", resp.StatusCode, raw)
		}
		return json.Unmarshal(raw, &got)
	})

	// Wait until the handler, its snapshot already taken, waits on the lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := lockTx.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE relation = 'review_verdicts'::regclass AND NOT granted").Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the result never waited on review_verdicts: this test proves nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A newer attempt, and its verdict, committed after the snapshot.
	newer, err := rig.turns.WithTx(lockTx).Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create the newer attempt: %v", err)
	}
	newerVerdict := insertVerdictOn(ctx, t, rig.reviewVerdicts.WithTx(lockTx), sess.ID, full, n, newer.ID, "h2")
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	r := got.ReviewedPullRequest
	if r == nil || r.Review.State != restdtos.SessionOutcomeReviewStateAssessed || r.Review.Verdict == nil {
		t.Fatalf("reviewedPullRequest = %+v, want assessed, as at the snapshot", r)
	}
	if id := r.Review.Verdict.VerdictId; id != olderVerdict.ID.String() {
		t.Fatalf("verdict = %s, want the older attempt's %s -- not %s, committed after the snapshot", id, olderVerdict.ID.String(), newerVerdict.ID.String())
	}
}

// insertVerdictOn records a verdict for repoFullName#n posted by attemptID
// at head through store (bound to the caller's transaction).
func insertVerdictOn(ctx context.Context, t *testing.T, store interface {
	Insert(context.Context, sqlcgen.InsertReviewVerdictParams) (sqlcgen.ReviewVerdict, error)
}, sessionID pgtype.UUID, repoFullName string, n int32, attemptID pgtype.UUID, head string) sqlcgen.ReviewVerdict {
	t.Helper()
	rc := currentContext()
	row, err := store.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName: repoFullName, PrNumber: n, HeadSha: head,
		RiskLevel: "low", Premise: "ok", BlastRadius: []byte(`[]`), FilesChanged: 1, TestsCoverage: "adequate", DocsDrift: "none",
		ProposedShippable: "auto", Shippable: "auto", SessionID: sessionID,
		ArchDecisionTags: []byte(`[]`), ArchDecisionRoots: []byte(`[]`), AncestorChain: []byte(`[]`),
		BaseRef: rc.baseRef, BaseSha: rc.baseSHA, PolicyVersion: rc.policy, AttemptID: attemptID,
	})
	if err != nil {
		t.Fatalf("insert verdict: %v", err)
	}
	return row
}

// TestResult_LastRunValues pins lastRun's values, not only their presence
// (finding P6): planMode is the run's, costUsd the total of its recorded
// step costs, startedAt its dispatch and finishedAt its completion -- two
// different instants.
func TestResult_LastRunValues(t *testing.T) {
	ctx := context.Background()
	rig, user, cookie := newResultRig(t, nil)
	sess := createSessionForUser(ctx, t, rig, user.ID, nil)
	run := createTurn(ctx, t, rig.turns, sess.ID, true)

	dispatchedAt := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: run.ID, Status: sqlcgen.TurnStatusDispatched, DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true}}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	transitionTurn(ctx, t, rig.turns, run.ID, turn.StateDispatched, turn.TriggerStartProcessing)
	for step, cost := range map[string]float64{"step-1": 1.00, "step-2": 0.23} {
		if n, err := rig.turns.RecordStepCostUSD(ctx, sess.ID, step, cost); err != nil || n != 1 {
			t.Fatalf("record %s's cost: %d rows, %v", step, n, err)
		}
	}
	transitionTurn(ctx, t, rig.turns, run.ID, turn.StateProcessing, turn.TriggerComplete)
	row, err := rig.turns.Get(ctx, run.ID)
	if err != nil || !row.CompletedAt.Valid {
		t.Fatalf("read the ended run: %v (completed %v)", err, row.CompletedAt.Valid)
	}

	got, raw := getResult(t, rig, sess.ID, cookie)
	lr := got.LastRun
	if lr == nil || lr.TurnId != run.ID.String() {
		t.Fatalf("lastRun = %+v, want the run", lr)
	}
	if !lr.PlanMode {
		t.Error("planMode = false, want the plan-mode run's true")
	}
	if lr.CostUsd == nil || math.Abs(*lr.CostUsd-1.23) > 1e-9 {
		t.Errorf("costUsd = %v, want 1.23, the sum of the run's step costs", lr.CostUsd)
	}
	if lr.StartedAt == nil || !lr.StartedAt.Equal(dispatchedAt) {
		t.Errorf("startedAt = %v, want the dispatch at %v", lr.StartedAt, dispatchedAt)
	}
	if lr.FinishedAt == nil || !lr.FinishedAt.Equal(row.CompletedAt.Time) || lr.FinishedAt.Equal(dispatchedAt) {
		t.Errorf("finishedAt = %v, want the completion at %v, not the dispatch", lr.FinishedAt, row.CompletedAt.Time)
	}
	if v, ok := raw["lastRun"].(map[string]any)["costUsd"].(float64); !ok || math.Abs(v-1.23) > 1e-9 {
		t.Errorf("costUsd on the wire = %#v, want the number 1.23", raw["lastRun"].(map[string]any)["costUsd"])
	}
}

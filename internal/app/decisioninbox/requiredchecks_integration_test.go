//go:build integration

package decisioninbox_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	decisioninboxdomain "github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/platform"
)

// TestRequiredChecks_InboxAndMergePathAgree drives §21.2's "CI green means
// the required checks, not the checks that reported" through both of this
// package's eligibility call sites on real Postgres, one fully eligible
// pull request per scenario with exactly one fact changed: the decision
// inbox's read model (Build: is the row ready_to_merge) and the merge path
// (RevalidateForMerge, which the auto-merge worker shares: does the click
// merge, and what does its refusal say). The two must agree on every
// scenario but one -- GitHub outbound off, where the inbox reads no
// requirements and the click reads them itself -- the refusal must name
// the check, the read model must read the base's requirements as the bot,
// and the click with the person's own token, the one it merges with.
func TestRequiredChecks_InboxAndMergePathAgree(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const humanToken = "the-actors-own-github-token"
	build := func(name string, appID int64) ports.RequiredCheck {
		return ports.RequiredCheck{Name: name, AppID: appID}
	}
	run := func(name string, appID int64, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceCheckRun, AppID: appID, Poster: ports.HeadCheckPosterApp, State: state}
	}
	status := func(name string, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceStatus, Poster: ports.HeadCheckPosterPerson, State: state}
	}
	// appStatus is a commit status the App slug's bot account posted, with
	// no check run of that App at the head to carry its id.
	appStatus := func(name, slug string, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceStatus, AppSlug: slug, Poster: ports.HeadCheckPosterApp, State: state}
	}
	overEarlierFromOthers := func(h ports.HeadCheck) ports.HeadCheck {
		h.EarlierFromOthers = true
		return h
	}
	const passed, pending, failed = ports.HeadCheckStatePassed, ports.HeadCheckStatePending, ports.HeadCheckStateFailed

	tests := []struct {
		name string
		// required is what the base branch requires; readErr fails the read.
		required []ports.RequiredCheck
		readErr  error
		// noBot runs with GitHub outbound off (Deps.GitHubOutbound nil).
		noBot bool
		// head is the PR's CI read at its head.
		head []ports.HeadCheck
		// apps is every App the code host can identify by slug; a slug
		// absent from it names no App. wantAppReads is how many App reads
		// each path makes (the inbox's only when it reads the requirements).
		apps         map[string]int64
		wantAppReads int
		ci           ports.CIConclusion
		headMoved    bool
		changedFiles []string
		// headShort marks the head's check listing incomplete (its
		// statuses beyond one page).
		headShort bool
		// baseRewritten moves the base branch's live tip to a commit that
		// does not descend from the verdict's base: a confirmed base move.
		baseRewritten bool
		// zeroTimeout sets DecisionInboxRequiredChecksTimeout to zero: a
		// read made under it has no time at all. zeroAppTimeout does the
		// same to DecisionInboxResolveAppIDTimeout.
		zeroTimeout    bool
		zeroAppTimeout bool
		wantEligible   bool
		// wantMergeOnly: the merge path merges while the inbox, which reads
		// no requirements with GitHub outbound off, shows needs_review.
		wantMergeOnly bool
		// wantReason is a substring of RevalidateForMerge's refusal.
		wantReason   string
		wantDegraded bool
	}{
		{
			name:         "a base requiring nothing keeps today's answer",
			head:         []ports.HeadCheck{run("build", 1, passed)},
			ci:           ports.CIConclusionSuccess,
			wantEligible: true,
		},
		{
			name:       "a required check that has not reported is ineligible and named",
			required:   []ports.RequiredCheck{build("ci/slow-external", 0)},
			head:       []ports.HeadCheck{run("build", 1, passed)},
			ci:         ports.CIConclusionSuccess,
			wantReason: `required check "ci/slow-external" has not reported at the current head`,
		},
		{
			name:       "a failing check beside a passing status of the same name is ineligible",
			required:   []ports.RequiredCheck{build("ci/build", 0)},
			head:       []ports.HeadCheck{run("ci/build", 1, failed), status("ci/build", passed)},
			ci:         ports.CIConclusionFailure,
			wantReason: `required check "ci/build" did not pass at the current head`,
		},
		{
			name:       "a required check satisfied by another App than the one named does not count",
			required:   []ports.RequiredCheck{build("ci/build", 15368)},
			head:       []ports.HeadCheck{run("ci/build", 99, passed)},
			ci:         ports.CIConclusionSuccess,
			wantReason: `required check "ci/build" from the App the base branch names (App id 15368) has not reported at the current head; a report of that name came from another source`,
		},
		{
			name:         "a required check an App reports as a commit status is satisfied by that App's status, the App identified by slug",
			wantAppReads: 1,
			required:     []ports.RequiredCheck{build("coverage/patch", 254)},
			head:         []ports.HeadCheck{appStatus("coverage/patch", "coverage", passed), run("build", 15368, passed)},
			apps:         map[string]int64{"coverage": 254},
			ci:           ports.CIConclusionSuccess,
			wantEligible: true,
		},
		{
			name:         "another App's commit status never satisfies a check tied to an App",
			wantAppReads: 1,
			required:     []ports.RequiredCheck{build("coverage/patch", 254)},
			head:         []ports.HeadCheck{appStatus("coverage/patch", "other-ci", passed), run("build", 15368, passed)},
			apps:         map[string]int64{"coverage": 254, "other-ci": 99},
			ci:           ports.CIConclusionSuccess,
			wantReason:   `required check "coverage/patch" from the App the base branch names (App id 254) has not reported at the current head; a report of that name came from another source`,
		},
		{
			// The masking case: the named App's own earlier status is
			// hidden under another App's later one.
			name:         "another App's commit status over an earlier one that may be the App's leaves the check not confirmed",
			wantAppReads: 1,
			required:     []ports.RequiredCheck{build("coverage/patch", 254)},
			head:         []ports.HeadCheck{overEarlierFromOthers(appStatus("coverage/patch", "other-ci", passed)), run("build", 15368, passed)},
			apps:         map[string]int64{"coverage": 254, "other-ci": 99},
			ci:           ports.CIConclusionSuccess,
			wantReason:   `required check "coverage/patch" from the App the base branch names (App id 254) could not be confirmed at the current head (its latest commit status came from another source`,
		},
		{
			name:         "a commit status from an App that cannot be identified leaves the check not confirmed",
			wantAppReads: 1,
			required:     []ports.RequiredCheck{build("coverage/patch", 254)},
			head:         []ports.HeadCheck{appStatus("coverage/patch", "coverage", passed), run("build", 15368, passed)},
			ci:           ports.CIConclusionSuccess,
			wantReason:   `required check "coverage/patch" from the App the base branch names (App id 254) could not be confirmed at the current head (the App that posted its commit status could not be identified)`,
		},
		{
			// Beside a check that does name one: only that check's status
			// is read for.
			name:         "a commit status an App posted for a check naming no App needs no App read",
			required:     []ports.RequiredCheck{build("deploy/preview", 0), build("coverage/patch", 254)},
			head:         []ports.HeadCheck{appStatus("deploy/preview", "previews", passed), appStatus("coverage/patch", "coverage", passed), run("build", 15368, passed)},
			apps:         map[string]int64{"coverage": 254},
			ci:           ports.CIConclusionSuccess,
			wantAppReads: 1,
			wantEligible: true,
		},
		{
			name:           "the App read runs under DecisionInboxResolveAppIDTimeout",
			required:       []ports.RequiredCheck{build("coverage/patch", 254)},
			head:           []ports.HeadCheck{appStatus("coverage/patch", "coverage", passed), run("build", 15368, passed)},
			apps:           map[string]int64{"coverage": 254},
			zeroAppTimeout: true,
			ci:             ports.CIConclusionSuccess,
			wantAppReads:   1,
			wantReason:     `required check "coverage/patch" from the App the base branch names (App id 254) could not be confirmed at the current head (the App that posted its commit status could not be identified)`,
		},
		{
			name:       "a person's commit status never satisfies a check tied to an App",
			required:   []ports.RequiredCheck{build("coverage/patch", 254)},
			head:       []ports.HeadCheck{status("coverage/patch", passed), run("build", 15368, passed)},
			ci:         ports.CIConclusionSuccess,
			wantReason: `required check "coverage/patch" from the App the base branch names (App id 254) has not reported at the current head`,
		},
		{
			name:       "an App-named check that failed names its App",
			required:   []ports.RequiredCheck{build("ci/build", 15368)},
			head:       []ports.HeadCheck{run("ci/build", 15368, failed), run("ci/build", 99, passed)},
			ci:         ports.CIConclusionFailure,
			wantReason: `required check "ci/build" from the App the base branch names (App id 15368) did not pass at the current head`,
		},
		{
			name:       "a required check not seen in a head listing short of its statuses is unconfirmed",
			required:   []ports.RequiredCheck{build("ci/slow-external", 0)},
			head:       []ports.HeadCheck{run("build", 1, passed)},
			ci:         ports.CIConclusionSuccess,
			headShort:  true,
			wantReason: `required check "ci/slow-external" could not be confirmed at the current head (its commit statuses were not all read)`,
		},
		{
			name:       "a required check still running is named, not read as CI not green",
			required:   []ports.RequiredCheck{build("ci/build", 0)},
			head:       []ports.HeadCheck{run("ci/build", 1, pending)},
			ci:         ports.CIConclusionUnknown,
			wantReason: `required check "ci/build" is still running at the current head`,
		},
		{
			name:         "a base requiring narvi/review is not made ineligible by it",
			required:     []ports.RequiredCheck{build(reviewcheck.CheckName, 0), build("ci/build", 0)},
			head:         []ports.HeadCheck{run("ci/build", 1, passed), run(reviewcheck.CheckName, 7, pending)},
			ci:           ports.CIConclusionSuccess,
			wantEligible: true,
		},
		{
			name:       "a failing check the base does not require, beside satisfied required checks, is ineligible",
			required:   []ports.RequiredCheck{build("ci/build", 0)},
			head:       []ports.HeadCheck{run("ci/build", 1, passed), run("security-scan", 1, failed)},
			ci:         ports.CIConclusionFailure,
			wantReason: string(autoapproval.ReasonCINotGreen),
		},
		{
			name:         "required checks satisfied by the named App and a status are eligible",
			required:     []ports.RequiredCheck{build("ci/build", 15368), build("deploy/preview", 0)},
			head:         []ports.HeadCheck{run("ci/build", 15368, passed), status("deploy/preview", passed)},
			ci:           ports.CIConclusionSuccess,
			wantEligible: true,
		},
		{
			name:         "a failed read of the requirements is ineligible, never today's read alone",
			readErr:      errors.New("rulesets: http 502"),
			head:         []ports.HeadCheck{run("build", 1, passed)},
			ci:           ports.CIConclusionSuccess,
			wantReason:   string(autoapproval.ReasonRequiredChecksUnknown),
			wantDegraded: true,
		},
		{
			// The inbox reads nothing without the bot and says so, not
			// degraded; the click reads with the person's token and merges.
			name:          "with GitHub outbound off the inbox reads nothing and the Merge click still merges",
			noBot:         true,
			required:      []ports.RequiredCheck{build("build", 0)},
			head:          []ports.HeadCheck{run("build", 1, passed)},
			ci:            ports.CIConclusionSuccess,
			wantMergeOnly: true,
		},
		{
			// Freshness is checked before the requirements: the confirmed
			// base move decides the row, so the failed read neither
			// degrades the inbox nor is the reason given.
			name:          "a failed read beside a confirmed base move refuses on the base move, undegraded",
			readErr:       errors.New("rulesets: http 502"),
			head:          []ports.HeadCheck{run("build", 1, passed)},
			ci:            ports.CIConclusionSuccess,
			baseRewritten: true,
			wantReason:    string(autoapproval.ReasonBaseMoved),
		},
		{
			// A transient read failure never stands in for a lasting
			// reason: the stale verdict is what the refusal says.
			name:       "a failed read beside a stale verdict refuses on the stale verdict",
			readErr:    errors.New("rulesets: http 502"),
			head:       []ports.HeadCheck{run("build", 1, passed)},
			ci:         ports.CIConclusionSuccess,
			headMoved:  true,
			wantReason: string(autoapproval.ReasonStaleVerdict),
		},
		{
			// The same for a criterion checked after the required checks:
			// the failed read stands in the probe as "requires nothing".
			name:         "a failed read beside a sensitive path refuses on the sensitive path",
			readErr:      errors.New("rulesets: http 502"),
			head:         []ports.HeadCheck{run("build", 1, passed)},
			ci:           ports.CIConclusionSuccess,
			changedFiles: []string{"migrations/000999_drop_everything.up.sql"},
			wantReason:   string(autoapproval.ReasonSensitivePathTouched),
		},
		{
			name:         "the read runs under DecisionInboxRequiredChecksTimeout",
			zeroTimeout:  true,
			head:         []ports.HeadCheck{run("build", 1, passed)},
			ci:           ports.CIConclusionSuccess,
			wantReason:   string(autoapproval.ReasonRequiredChecksUnknown),
			wantDegraded: true,
		},
	}

	users := narvipg.NewUserStore(pool)
	identities := narvipg.NewIdentityStore(pool)
	tokenKey := []byte("01234567890123456789012345678901")

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := 700 + i
			repo := "required-checks-" + strconv.Itoa(n)
			repoFullName := "acme/" + repo
			actorGitHubID := "rc-actor-" + strconv.Itoa(n)

			actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: actorGitHubID + "@example.com", DisplayName: "Actor", Role: sqlcgen.UserRoleMember})
			if err != nil {
				t.Fatalf("create actor: %v", err)
			}
			encrypted, err := platform.EncryptToken(tokenKey, []byte(humanToken))
			if err != nil {
				t.Fatalf("encrypt token: %v", err)
			}
			if _, err := identities.Create(ctx, sqlcgen.CreateIdentityParams{
				UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: actorGitHubID,
				EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encrypted,
			}); err != nil {
				t.Fatalf("create identity: %v", err)
			}

			rs := newRevalidateStores(pool)
			pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, n)
			pr.HeadChecks = tc.head
			pr.CIConclusion = tc.ci
			if tc.headMoved {
				pr.HeadSHA = "a-newer-head"
			}
			if tc.changedFiles != nil {
				pr.ChangedFiles = tc.changedFiles
				pr.ChangedFilesCount = len(tc.changedFiles)
			}
			pr.HeadChecksListDegraded = tc.headShort
			if tc.baseRewritten {
				rs.sourceControl.resolveBranchSHA = "sha-rewritten-base"
				rs.sourceControl.isAncestorResult = false
			}
			rs.replaceTargetPR(actorGitHubID, pr)
			if tc.required != nil {
				rs.sourceControl.requiredChecksByBranch = map[string][]ports.RequiredCheck{pr.BaseRef: tc.required}
			}
			rs.sourceControl.requiredChecksErr = tc.readErr
			rs.sourceControl.appIDsBySlug = tc.apps

			deps := rs.deps
			timeouts := platform.DefaultTimeouts()
			if tc.zeroTimeout {
				timeouts.DecisionInboxRequiredChecksTimeout = 0
			}
			if tc.zeroAppTimeout {
				timeouts.DecisionInboxResolveAppIDTimeout = 0
			}
			deps.Timeouts = timeouts
			deps.SCMCache = decisioninbox.NewSCMCache(rs.sourceControl, timeouts)
			deps.TokenEncryptionKey = tokenKey
			if tc.noBot {
				deps.GitHubOutbound = nil
			}

			// The read model.
			result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			item := findItemByPR(result.Items, n)
			if item == nil {
				t.Fatalf("PR #%d missing from the inbox", n)
			}
			inboxReady := item.Kind == decisioninboxdomain.KindReadyToMerge
			if inboxReady != tc.wantEligible {
				t.Errorf("inbox Kind = %s, want ready_to_merge %v", item.Kind, tc.wantEligible)
			}
			if result.SCMFetchFailed != tc.wantDegraded {
				t.Errorf("SCMFetchFailed = %v, want %v", result.SCMFetchFailed, tc.wantDegraded)
			}
			if result.RequiredChecksNotRead != tc.noBot {
				t.Errorf("RequiredChecksNotRead = %v, want %v", result.RequiredChecksNotRead, tc.noBot)
			}
			inboxCalls := rs.sourceControl.requiredChecksCalls
			rs.sourceControl.requiredChecksCalls = nil
			if tc.noBot && len(inboxCalls) != 0 {
				t.Errorf("the inbox read the requirements %d times with no bot credential, want 0", len(inboxCalls))
			}
			for _, c := range inboxCalls {
				if c.Token != testBotToken {
					t.Errorf("inbox ListRequiredChecks token = %q, want the bot's", c.Token)
				}
			}
			inboxResolves := rs.sourceControl.resolveAppIDCalls
			rs.sourceControl.resolveAppIDCalls = nil
			for _, c := range inboxResolves {
				if c.Token != testBotToken {
					t.Errorf("inbox ResolveAppID token = %q, want the bot's, like the requirements read", c.Token)
				}
			}

			// The merge path.
			ok, _, reason, _, _, err := decisioninbox.RevalidateForMerge(ctx, deps, rs.sourceControl, actorGitHubID, repoFullName, n, humanToken)
			if err != nil {
				t.Fatalf("RevalidateForMerge() error = %v", err)
			}
			wantMerge := tc.wantEligible || tc.wantMergeOnly
			if ok != wantMerge {
				t.Errorf("RevalidateForMerge() ok = %v (reason %q), want %v", ok, reason, wantMerge)
			}
			if !tc.wantMergeOnly && ok != inboxReady {
				t.Errorf("the inbox (Kind %s) and the merge path (ok %v) disagree", item.Kind, ok)
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Errorf("RevalidateForMerge() reason = %q, want it to contain %q", reason, tc.wantReason)
			}

			mergeCalls := rs.sourceControl.requiredChecksCalls
			if len(mergeCalls) != 1 {
				t.Errorf("the Merge click read the requirements %d times, want once", len(mergeCalls))
			}
			for _, c := range append(inboxCalls, mergeCalls...) {
				if c.Owner != "acme" || c.Repo != repo || c.Branch != pr.BaseRef {
					t.Errorf("ListRequiredChecks spec = %+v, want acme/%s@%s", c, repo, pr.BaseRef)
				}
			}
			for _, c := range mergeCalls {
				if c.Token != humanToken {
					t.Errorf("Merge click ListRequiredChecks token = %q, want the person's own (the credential the merge is made with)", c.Token)
				}
			}
			mergeResolves := rs.sourceControl.resolveAppIDCalls
			for _, c := range mergeResolves {
				if c.Token != humanToken {
					t.Errorf("Merge click ResolveAppID token = %q, want the person's own, like the requirements read", c.Token)
				}
			}
			// An App is identified only where a requirement names one and
			// no check run carried its id.
			if len(mergeResolves) != tc.wantAppReads || (!tc.noBot && len(inboxResolves) != tc.wantAppReads) {
				t.Errorf("ResolveAppID calls = (inbox %d, merge %d), want %d each", len(inboxResolves), len(mergeResolves), tc.wantAppReads)
			}
		})
	}
}

// TestRequiredChecks_AcceptanceReadout pins the decision inbox's acceptance
// readout around the base's required checks (§21.2). An accepted row whose
// only blocker is a failed read of the requirements says they could not be
// read -- the engine's own reason, the inbox degraded. With GitHub outbound
// off it says the inbox does not read them, and the inbox is not degraded:
// a configuration. And when a confirmed base move already decides the row,
// a failed read neither degrades the inbox nor names the blocker.
func TestRequiredChecks_AcceptanceReadout(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tokenKey := []byte("01234567890123456789012345678901")

	tests := []struct {
		name          string
		readErr       error
		noBot         bool
		baseRewritten bool
		wantReason    string
		wantDegraded  bool
	}{
		{
			name:         "a failed read that decides the row names it, and degrades the inbox",
			readErr:      errors.New("rulesets: http 502"),
			wantReason:   string(autoapproval.ReasonRequiredChecksUnknown),
			wantDegraded: true,
		},
		{
			name:       "GitHub outbound off names the configuration, and does not degrade the inbox",
			noBot:      true,
			wantReason: "the checks this pull request's base branch requires are not read in the inbox while this deployment's GitHub outbound is off",
		},
		{
			name:          "a failed read beside a confirmed base move is not the blocker, and does not degrade the inbox",
			readErr:       errors.New("rulesets: http 502"),
			baseRewritten: true,
			wantReason:    "this pull request no longer meets the auto-approval eligibility criteria, even with its accepted override applied",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := 801 + i
			repoFullName := "acme/required-checks-acceptance-" + strconv.Itoa(n)
			actorGitHubID := "rc-acceptance-actor-" + strconv.Itoa(n)
			actor, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: actorGitHubID + "@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
			if err != nil {
				t.Fatalf("create actor: %v", err)
			}
			encrypted, err := platform.EncryptToken(tokenKey, []byte("person-token"))
			if err != nil {
				t.Fatalf("encrypt token: %v", err)
			}
			if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
				UserID: actor.ID, Provider: sqlcgen.IdentityProviderGithub, ExternalID: actorGitHubID,
				EmailVerified: true, LinkedVia: sqlcgen.IdentityLinkedViaAutoEmail, AccessTokenEncrypted: encrypted,
			}); err != nil {
				t.Fatalf("create identity: %v", err)
			}

			rs := newRevalidateStores(pool)
			pr := rs.eligiblePR(ctx, t, pool, actorGitHubID, repoFullName, n)
			record, hasVerdict, err := appreviewverdict.GetLatest(ctx, rs.deps.ReviewVerdict, repoFullName, int32(n))
			if err != nil || !hasVerdict {
				t.Fatalf("GetLatest() = (%v, %v), want the seeded verdict", hasVerdict, err)
			}
			var verdictID pgtype.UUID
			if err := verdictID.Scan(record.ID); err != nil {
				t.Fatalf("scan verdict id: %v", err)
			}
			if _, _, err := appreviewverdict.Accept(ctx, rs.deps.ReviewVerdict.Acceptances, appreviewverdict.AcceptInput{
				RepoFullName:  repoFullName,
				PRNumber:      int32(n),
				VerdictID:     verdictID,
				AttemptID:     seedReviewAttemptTurn(ctx, t, pool),
				HeadSHA:       record.HeadSHA,
				Context:       record.Context,
				Reason:        string(autoapproval.ReasonNotShippableAuto),
				Justification: "Accepted so the readout below has an acceptance to describe.",
				AcceptedBy:    actor.ID,
			}); err != nil {
				t.Fatalf("Accept() error = %v", err)
			}
			rs.replaceTargetPR(actorGitHubID, pr)
			rs.sourceControl.requiredChecksErr = tc.readErr
			if tc.baseRewritten {
				rs.sourceControl.resolveBranchSHA = "sha-rewritten-base"
				rs.sourceControl.isAncestorResult = false
			}

			deps := rs.deps
			deps.SCMCache = decisioninbox.NewSCMCache(rs.sourceControl, platform.DefaultTimeouts())
			deps.TokenEncryptionKey = tokenKey
			if tc.noBot {
				deps.GitHubOutbound = nil
			}

			result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMaintainer, time.Now())
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			item := findItemByPR(result.Items, n)
			if item == nil {
				t.Fatalf("PR #%d missing from the inbox", n)
			}
			if item.AcceptanceID == "" {
				t.Fatal("the row carries no acceptance -- the fixture does not exercise the readout")
			}
			if item.AcceptanceMergeable {
				t.Error("AcceptanceMergeable = true, want false")
			}
			if item.AcceptanceMergeBlockedReason != tc.wantReason {
				t.Errorf("AcceptanceMergeBlockedReason = %q, want %q", item.AcceptanceMergeBlockedReason, tc.wantReason)
			}
			if result.SCMFetchFailed != tc.wantDegraded {
				t.Errorf("SCMFetchFailed = %v, want %v", result.SCMFetchFailed, tc.wantDegraded)
			}
		})
	}
}

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
// pull request per scenario with exactly one required-check fact changed:
// the decision inbox's read model (Build: is the row ready_to_merge) and
// the merge path (RevalidateForMerge, which the auto-merge worker shares:
// does the click merge, and what does its refusal say). The two must
// agree on every scenario, the refusal must name the check, and both must
// read the base's requirements as the bot -- never with the person's own
// token.
func TestRequiredChecks_InboxAndMergePathAgree(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const humanToken = "the-actors-own-github-token"
	build := func(name string, appID int64) ports.RequiredCheck {
		return ports.RequiredCheck{Name: name, AppID: appID}
	}
	run := func(name string, appID int64, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceCheckRun, AppID: appID, State: state}
	}
	status := func(name string, state ports.HeadCheckState) ports.HeadCheck {
		return ports.HeadCheck{Name: name, Source: ports.HeadCheckSourceStatus, State: state}
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
		head         []ports.HeadCheck
		ci           ports.CIConclusion
		headMoved    bool
		changedFiles []string
		// zeroTimeout sets DecisionInboxRequiredChecksTimeout to zero: a
		// read made under it has no time at all.
		zeroTimeout  bool
		wantEligible bool
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
			wantReason: `required check "ci/build" has not reported at the current head from the App the base branch names (App id 15368)`,
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
			name:         "with GitHub outbound off the requirements cannot be read",
			noBot:        true,
			head:         []ports.HeadCheck{run("build", 1, passed)},
			ci:           ports.CIConclusionSuccess,
			wantReason:   string(autoapproval.ReasonRequiredChecksUnknown),
			wantDegraded: true,
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
			rs.replaceTargetPR(actorGitHubID, pr)
			if tc.required != nil {
				rs.sourceControl.requiredChecksByBranch = map[string][]ports.RequiredCheck{pr.BaseRef: tc.required}
			}
			rs.sourceControl.requiredChecksErr = tc.readErr

			deps := rs.deps
			timeouts := platform.DefaultTimeouts()
			if tc.zeroTimeout {
				timeouts.DecisionInboxRequiredChecksTimeout = 0
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
			if got := item.Kind == decisioninboxdomain.KindReadyToMerge; got != tc.wantEligible {
				t.Errorf("inbox Kind = %s, want ready_to_merge %v", item.Kind, tc.wantEligible)
			}
			if result.SCMFetchFailed != tc.wantDegraded {
				t.Errorf("SCMFetchFailed = %v, want %v", result.SCMFetchFailed, tc.wantDegraded)
			}

			// The merge path.
			ok, _, reason, _, _, err := decisioninbox.RevalidateForMerge(ctx, deps, rs.sourceControl, actorGitHubID, repoFullName, n, humanToken)
			if err != nil {
				t.Fatalf("RevalidateForMerge() error = %v", err)
			}
			if ok != tc.wantEligible {
				t.Errorf("RevalidateForMerge() ok = %v (reason %q), want %v", ok, reason, tc.wantEligible)
			}
			if ok != (item.Kind == decisioninboxdomain.KindReadyToMerge) {
				t.Errorf("the inbox (Kind %s) and the merge path (ok %v) disagree", item.Kind, ok)
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Errorf("RevalidateForMerge() reason = %q, want it to contain %q", reason, tc.wantReason)
			}

			calls := rs.sourceControl.requiredChecksCalls
			if tc.noBot && len(calls) != 0 {
				t.Errorf("ListRequiredChecks called %d times with no bot credential, want 0", len(calls))
			}
			if !tc.noBot && len(calls) == 0 {
				t.Error("ListRequiredChecks never called, want the base's requirements read")
			}
			for _, c := range calls {
				if c.Token != testBotToken {
					t.Errorf("ListRequiredChecks token = %q, want the bot's (never the person's own)", c.Token)
				}
				if c.Owner != "acme" || c.Repo != repo || c.Branch != pr.BaseRef {
					t.Errorf("ListRequiredChecks spec = %+v, want acme/%s@%s", c, repo, pr.BaseRef)
				}
			}
		})
	}
}

// TestRequiredChecks_AcceptanceReadoutNamesTheUnreadRequirements pins the
// decision inbox's acceptance readout on a failed read of the base's
// required checks: an accepted row whose only blocker is that failed read
// says the requirements could not be read -- the engine's own reason --
// never that the base commit could not be confirmed.
func TestRequiredChecks_AcceptanceReadoutNamesTheUnreadRequirements(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const actorGitHubID, repoFullName, n = "rc-acceptance-actor", "acme/required-checks-acceptance", 801
	users := narvipg.NewUserStore(pool)
	actor, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "rc-acceptance@example.com", DisplayName: "Maintainer", Role: sqlcgen.UserRoleMaintainer})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	tokenKey := []byte("01234567890123456789012345678901")
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
	rs.sourceControl.requiredChecksErr = errors.New("rulesets: http 502")

	deps := rs.deps
	deps.SCMCache = decisioninbox.NewSCMCache(rs.sourceControl, platform.DefaultTimeouts())
	deps.TokenEncryptionKey = tokenKey

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
		t.Error("AcceptanceMergeable = true with the base's requirements unread, want false")
	}
	if item.AcceptanceMergeBlockedReason != string(autoapproval.ReasonRequiredChecksUnknown) {
		t.Errorf("AcceptanceMergeBlockedReason = %q, want %q", item.AcceptanceMergeBlockedReason, autoapproval.ReasonRequiredChecksUnknown)
	}
}

//go:build integration

package decisioninbox_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	decisioninboxdomain "github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins the decision inbox's side of the autonomy freeze
// (technical plan §40.2, §16.1): one banner, the same for every role, read
// once per load; a held mark only on the ready_to_merge rows whose
// automatic merge the freeze actually holds; a freeze that cannot be read
// never reported as "not frozen"; and the workflow advances the freeze
// holds, listed to whoever may decide those runs' steps.

// freezeInboxDeps is the Deps every test here builds the inbox with: the
// real stores, the freeze's two among them, over sc.
func freezeInboxDeps(pool *pgxpool.Pool, sc ports.SourceControl, tokenKey []byte) decisioninbox.Deps {
	return decisioninbox.Deps{
		GitHubOutbound: testBotOutbound,
		Plans:          narvipg.NewPlanStore(pool), Sessions: narvipg.NewSessionStore(pool), Participants: narvipg.NewParticipantStore(pool),
		Automations: narvipg.NewAutomationStore(pool), Outbox: narvipg.NewOutboxStore(pool, false),
		ReviewFindings: narvipg.NewReviewFindingStore(pool), SentinelFixes: narvipg.NewSentinelFixStore(pool),
		Artifacts: narvipg.NewArtifactStore(pool), Identities: narvipg.NewIdentityStore(pool),
		PlatformSettings:   narvipg.NewPlatformSettingsStore(pool),
		Workflows:          narvipg.NewWorkflowStore(pool),
		SCMCache:           decisioninbox.NewSCMCache(sc, platform.DefaultTimeouts()),
		TokenEncryptionKey: tokenKey,
		Timeouts:           platform.DefaultTimeouts(),
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool), RepoSettings: narvipg.NewRepoSettingsStore(pool),
			ReviewFindings: narvipg.NewReviewFindingStore(pool), AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(pool),
			Timeouts: platform.DefaultTimeouts(),
		},
	}
}

// eligiblePR seeds repoFullName#number as a platform-authored pull request
// with an auto-approved verdict at head, and returns the open pull request
// the code host reports for it, assigned to the actor: a ready_to_merge
// row.
func eligiblePR(ctx context.Context, t *testing.T, pool *pgxpool.Pool, actor sqlcgen.User, actorExternalID, owner, repo string, number int, head string) ports.OpenPR {
	t.Helper()
	htmlURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, CreatedBy: actor.ID})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	if _, err := narvipg.NewArtifactStore(pool).Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("mark %s platform-authored: %v", htmlURL, err)
	}
	seedAutoApprovedVerdict(ctx, t, pool, owner+"/"+repo, int32(number), head)
	return ports.OpenPR{
		Owner: owner, Repo: repo, Number: number, Title: fmt.Sprintf("%s #%d", repo, number),
		HTMLURL: htmlURL, HeadSHA: head, BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
		Assignees:    []ports.PRPerson{{ExternalID: actorExternalID, Login: "actor"}},
		CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now(),
	}
}

// TestDecisionInbox_Frozen_BannerAndHeldOnlyOnArmedRepos: while autonomy
// is frozen every role's inbox carries the same freeze -- when, by whom,
// why -- and only a ready_to_merge row in a repository with auto-merge
// armed is marked held: the one merge the freeze actually holds. A
// ready_to_merge row in an unarmed repository waits on a person anyway, and
// a needs_review row is never the worker's; neither is marked. Before the
// freeze nothing is marked and the banner reads not frozen.
func TestDecisionInbox_Frozen_BannerAndHeldOnlyOnArmedRepos(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const actorExternalID = "7001"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "freeze-actor@example.com", actorExternalID, tokenKey)

	armed := eligiblePR(ctx, t, pool, actor, actorExternalID, "acme", "armed", 1, "sha-armed")
	unarmed := eligiblePR(ctx, t, pool, actor, actorExternalID, "acme", "unarmed", 2, "sha-unarmed")
	// Not platform-authored: needs_review, in the armed repository.
	review := ports.OpenPR{
		Owner: "acme", Repo: "armed", Number: 3, Title: "a person's pull request", HTMLURL: "https://github.com/acme/armed/pull/3",
		HeadSHA: "sha-review", BaseRef: testEligibleBaseRef, BaseSHA: testEligibleBaseSHA,
		Assignees:    []ports.PRPerson{{ExternalID: actorExternalID, Login: "actor"}},
		CIConclusion: ports.CIConclusionSuccess, Labels: []string{"review:low-risk"}, CreatedAt: time.Now(),
	}
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	if _, err := repoSettings.UpsertAutoMergeToggle(ctx, "acme/armed", true); err != nil {
		t.Fatalf("arm auto-merge on acme/armed: %v", err)
	}
	if _, err := repoSettings.UpsertAutoMergeToggle(ctx, "acme/unarmed", false); err != nil {
		t.Fatalf("leave acme/unarmed unarmed: %v", err)
	}
	sc := &fakeDecisionInboxSourceControl{openPRsByExternalID: map[string][]ports.OpenPR{actorExternalID: {armed, unarmed, review}}}
	deps := freezeInboxDeps(pool, sc, tokenKey)

	kinds := func(result decisioninbox.Result) map[int]decisioninbox.Item {
		got := map[int]decisioninbox.Item{}
		for _, n := range []int{1, 2, 3} {
			item := findItemByPR(result.Items, n)
			if item == nil {
				t.Fatalf("PR #%d missing from the inbox", n)
			}
			got[n] = *item
		}
		return got
	}

	before, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build() before the freeze: %v", err)
	}
	if before.AutonomyFreeze.AutonomyFrozen || before.AutonomyFreezeUnread {
		t.Fatalf("before the freeze: frozen %v, unread %v; want neither", before.AutonomyFreeze.AutonomyFrozen, before.AutonomyFreezeUnread)
	}
	for n, item := range kinds(before) {
		if item.HeldByFreeze {
			t.Errorf("PR #%d held before any freeze", n)
		}
	}
	if got := kinds(before); got[1].Kind != decisioninboxdomain.KindReadyToMerge || got[2].Kind != decisioninboxdomain.KindReadyToMerge || got[3].Kind != decisioninboxdomain.KindNeedsReview {
		t.Fatalf("kinds = %s, %s, %s; want ready_to_merge, ready_to_merge, needs_review", got[1].Kind, got[2].Kind, got[3].Kind)
	}

	admin, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "freeze-admin@example.com", DisplayName: "Ada Admin", Role: sqlcgen.UserRoleAdmin})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := narvipg.NewPlatformSettingsStore(pool).Freeze(ctx, admin.ID, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}

	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMaintainer, authz.RoleMember, authz.RoleViewer} {
		frozen, err := decisioninbox.Build(ctx, deps, actor.ID, role, time.Now())
		if err != nil {
			t.Fatalf("Build() as %s while frozen: %v", role, err)
		}
		f := frozen.AutonomyFreeze
		if !f.AutonomyFrozen || frozen.AutonomyFreezeUnread || !f.AutonomyFrozenAt.Valid || f.AutonomyFrozenBy != admin.ID ||
			f.AutonomyFrozenByDisplayName == nil || *f.AutonomyFrozenByDisplayName != "Ada Admin" ||
			f.AutonomyFreezeReason == nil || *f.AutonomyFreezeReason != "an incident: hold every automatic action" {
			t.Fatalf("as %s: freeze = %+v, unread %v; want the admin's freeze with its reason", role, f, frozen.AutonomyFreezeUnread)
		}
		got := kinds(frozen)
		if !got[1].HeldByFreeze {
			t.Errorf("as %s: the armed repository's ready_to_merge row is not held", role)
		}
		if got[2].HeldByFreeze {
			t.Errorf("as %s: the unarmed repository's ready_to_merge row is held: nothing automatic would merge it", role)
		}
		if got[3].HeldByFreeze {
			t.Errorf("as %s: a needs_review row is held", role)
		}
		if got[1].Kind != decisioninboxdomain.KindReadyToMerge {
			t.Errorf("as %s: the held row's kind = %s, want it still listed ready_to_merge", role, got[1].Kind)
		}
	}
}

// TestDecisionInbox_FreezeUnreadable_NeverReadsUnfrozen: when the freeze
// cannot be read -- here its store's pool is closed, while autonomy is in
// fact frozen -- the load says so (AutonomyFreezeUnread) rather than
// reading not frozen, and marks no row held, since nothing says the freeze
// holds it. An inbox with no freeze store wired says the same.
func TestDecisionInbox_FreezeUnreadable_NeverReadsUnfrozen(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const actorExternalID = "7002"
	tokenKey := []byte("01234567890123456789012345678901")
	actor := decisionInboxActorFixture(ctx, t, pool, "freeze-unread-actor@example.com", actorExternalID, tokenKey)
	armed := eligiblePR(ctx, t, pool, actor, actorExternalID, "acme", "armed-unread", 1, "sha-armed-unread")
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertAutoMergeToggle(ctx, "acme/armed-unread", true); err != nil {
		t.Fatalf("arm auto-merge: %v", err)
	}
	if _, err := narvipg.NewPlatformSettingsStore(pool).Freeze(ctx, pgtype.UUID{}, "an incident"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	sc := &fakeDecisionInboxSourceControl{openPRsByExternalID: map[string][]ports.OpenPR{actorExternalID: {armed}}}

	closed, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatalf("open a second pool: %v", err)
	}
	closed.Close()

	for _, tc := range []struct {
		name     string
		settings *narvipg.PlatformSettingsStore
	}{
		{"the read fails", narvipg.NewPlatformSettingsStore(closed)},
		{"no store wired", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := freezeInboxDeps(pool, sc, tokenKey)
			deps.PlatformSettings = tc.settings
			result, err := decisioninbox.Build(ctx, deps, actor.ID, authz.RoleMember, time.Now())
			if err != nil {
				t.Fatalf("Build(): %v", err)
			}
			if !result.AutonomyFreezeUnread {
				t.Fatalf("AutonomyFreezeUnread = false with an unreadable freeze (freeze %+v): a failed read must never read as not frozen", result.AutonomyFreeze)
			}
			if result.AutonomyFreeze.AutonomyFrozen {
				t.Errorf("AutonomyFreeze reads frozen with nothing read: %+v", result.AutonomyFreeze)
			}
			item := findItemByPR(result.Items, 1)
			if item == nil || item.Kind != decisioninboxdomain.KindReadyToMerge {
				t.Fatalf("PR #1 = %+v, want it listed ready_to_merge", item)
			}
			if item.HeldByFreeze {
				t.Error("PR #1 marked held although the freeze could not be read")
			}
		})
	}
}

// heldAdvanceOn seeds, on a new session created by createdBy, a workflow
// run of a definition named name whose advance the freeze holds, held at
// heldAt, and returns the run's id.
func heldAdvanceOn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, createdBy pgtype.UUID, title, name string, heldAt time.Time) (sessionID, runID pgtype.UUID) {
	t.Helper()
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: createdBy, Title: &title})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var defID, stepID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_definitions (lane, name, is_built_in, version) VALUES ('request', $1, false, 1) RETURNING id`, name).Scan(&defID); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_step_definitions (workflow_definition_id, step_order, kind, prompt_template) VALUES ($1, 1, 'agent', '{{prompt}}') RETURNING id`, defID).Scan(&stepID); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	workflows := narvipg.NewWorkflowStore(pool)
	run, err := workflows.CreateRun(ctx, session.ID, "request", defID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	attempt, err := workflows.CreateStepRun(ctx, run.ID, stepID)
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	if held, err := workflows.HoldAdvance(ctx, run.ID, attempt.ID, session.ID); err != nil || !held {
		t.Fatalf("hold the advance: held %v, err %v", held, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_advance_holds SET held_at = $2 WHERE workflow_run_id = $1`, run.ID, heldAt); err != nil {
		t.Fatalf("date the hold: %v", err)
	}
	return session.ID, run.ID
}

// TestDecisionInbox_HeldWorkflowAdvances_ListedToWhoMayDecide: the
// workflow advances the freeze holds are listed, oldest first, with their
// workflow and session, to whoever may decide those runs' steps
// (authz.ActionDecideWorkflowStep): an administrator and a maintainer see
// every one, a member those on sessions they created or joined, a viewer
// none, even on their own session. A released hold is listed no more.
func TestDecisionInbox_HeldWorkflowAdvances_ListedToWhoMayDecide(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	users := narvipg.NewUserStore(pool)
	user := func(email string, role sqlcgen.UserRole) sqlcgen.User {
		u, err := users.Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: email, DisplayName: email, Role: role})
		if err != nil {
			t.Fatalf("create %s: %v", email, err)
		}
		return u
	}
	admin := user("held-admin@example.com", sqlcgen.UserRoleAdmin)
	maintainer := user("held-maintainer@example.com", sqlcgen.UserRoleMaintainer)
	owner := user("held-owner@example.com", sqlcgen.UserRoleMember)
	joiner := user("held-joiner@example.com", sqlcgen.UserRoleMember)
	stranger := user("held-stranger@example.com", sqlcgen.UserRoleMember)
	viewer := user("held-viewer@example.com", sqlcgen.UserRoleViewer)

	now := time.Now().UTC().Truncate(time.Second)
	ownedSession, ownedRun := heldAdvanceOn(ctx, t, pool, owner.ID, "the owner's build", "build then test", now.Add(-2*time.Minute))
	_, adminRun := heldAdvanceOn(ctx, t, pool, admin.ID, "the admin's build", "build then deploy", now.Add(-time.Minute))
	_, viewerRun := heldAdvanceOn(ctx, t, pool, viewer.ID, "the viewer's build", "build alone", now.Add(-3*time.Minute))
	if _, err := narvipg.NewParticipantStore(pool).Create(ctx, ownedSession, joiner.ID); err != nil {
		t.Fatalf("join the owner's session: %v", err)
	}

	deps := freezeInboxDeps(pool, &fakeDecisionInboxSourceControl{}, []byte("01234567890123456789012345678901"))
	runsOf := func(u sqlcgen.User) []string {
		result, err := decisioninbox.Build(ctx, deps, u.ID, authz.Role(u.Role), time.Now())
		if err != nil {
			t.Fatalf("Build() as %s: %v", u.PrimaryEmail, err)
		}
		runs := make([]string, len(result.HeldWorkflowAdvances))
		for i, h := range result.HeldWorkflowAdvances {
			runs[i] = h.WorkflowRunID
		}
		return runs
	}
	all := []string{viewerRun.String(), ownedRun.String(), adminRun.String()}
	for _, tc := range []struct {
		who  sqlcgen.User
		want []string
	}{
		{admin, all},
		{maintainer, all},
		{owner, []string{ownedRun.String()}},
		{joiner, []string{ownedRun.String()}},
		{stranger, nil},
		{viewer, nil},
	} {
		if got := runsOf(tc.who); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("held advances for %s = %v, want %v", tc.who.PrimaryEmail, got, tc.want)
		}
	}

	result, err := decisioninbox.Build(ctx, deps, owner.ID, authz.RoleMember, time.Now())
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if len(result.HeldWorkflowAdvances) != 1 {
		t.Fatalf("owner's held advances = %+v, want one", result.HeldWorkflowAdvances)
	}
	h := result.HeldWorkflowAdvances[0]
	if h.SessionID != ownedSession.String() || h.WorkflowName != "build then test" || h.SessionTitle == nil || *h.SessionTitle != "the owner's build" || !h.HeldAt.Equal(now.Add(-2*time.Minute)) {
		t.Errorf("held advance = %+v, want the owner's session, its workflow and title, held two minutes ago", h)
	}

	if _, err := narvipg.NewWorkflowStore(pool).ReleaseAdvanceHold(ctx, ownedRun); err != nil {
		t.Fatalf("release the owner's hold: %v", err)
	}
	if got := runsOf(owner); len(got) != 0 {
		t.Errorf("held advances after the release = %v, want none", got)
	}
}

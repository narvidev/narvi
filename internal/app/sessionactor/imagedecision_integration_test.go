//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	domainimagebuild "github.com/narvidev/narvi/internal/domain/imagebuild"
	"github.com/narvidev/narvi/internal/domain/imagedecision"
)

// This file proves §19's own gap (the image decision is unreadable after
// the fact): every resolveAndSetImage outcome (imageresolve.go) now
// persists an imagedecision.Reason onto the session's own sandboxes row
// AND appends an "image_decision" event, reusing the existing collection
// and event log -- see imageresolve.go's own top "# Persisted decision
// provenance" comment for the full design this file exercises end to end,
// against a REAL Postgres instance.

// pgEnumLabels returns the ordered set of labels a Postgres ENUM type
// currently carries, straight from pg_enum/pg_type -- used by
// TestImageDecisionReasonEnum_MatchesGoVocabulary (below) to prove the
// migration's own CREATE TYPE list and imagedecision.All() can never
// silently drift apart.
func pgEnumLabels(ctx context.Context, t *testing.T, pool *pgxpool.Pool, typeName string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT e.enumlabel
		FROM pg_type t
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE t.typname = $1
		ORDER BY e.enumsortorder`, typeName)
	if err != nil {
		t.Fatalf("query pg_enum for %s: %v", typeName, err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatalf("scan enumlabel: %v", err)
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pg_enum rows: %v", err)
	}
	return labels
}

// TestImageDecisionReasonEnum_MatchesGoVocabulary is the drift check
// imagedecision's own package doc comment promises: the real, live
// image_decision_reason Postgres enum (migrations/000139_sandboxes_
// image_decision.up.sql) and imagedecision.All() must name EXACTLY the
// same set, order aside -- a one-sided edit (a new Go Reason with no
// matching migration, or vice versa) fails here rather than silently
// drifting into "the database silently rejects a value the Go code
// believes is valid" or "the Go vocabulary undercounts what the schema
// actually allows".
func TestImageDecisionReasonEnum_MatchesGoVocabulary(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	dbLabels := pgEnumLabels(ctx, t, pool, "image_decision_reason")

	var goLabels []string
	for _, r := range imagedecision.All() {
		goLabels = append(goLabels, string(r))
	}

	sort.Strings(dbLabels)
	sort.Strings(goLabels)

	if len(dbLabels) != len(goLabels) {
		t.Fatalf("image_decision_reason enum has %d labels in Postgres, %d in imagedecision.All(): db=%v go=%v",
			len(dbLabels), len(goLabels), dbLabels, goLabels)
	}
	for i := range dbLabels {
		if dbLabels[i] != goLabels[i] {
			t.Fatalf("image_decision_reason enum mismatch at index %d: db=%q go=%q (full sets: db=%v go=%v)",
				i, dbLabels[i], goLabels[i], dbLabels, goLabels)
		}
	}

	// ReasonNone must NEVER be one of the enum's own labels -- see its own
	// doc comment for why.
	for _, label := range dbLabels {
		if label == string(imagedecision.ReasonNone) {
			t.Fatalf("image_decision_reason enum unexpectedly carries ReasonNone (%q) -- it must never be a persistable value", label)
		}
	}
}

// TestImageDecisionReasonColumn_RejectsValueOutsideEnum is the schema-
// constraint half of the closed-vocabulary guarantee, proven directly
// against a real Postgres instance: an attempt to write a string outside
// the enum's own closed set is a Postgres error, not merely a Go-side
// convention nothing enforces. This is the mutation-tested guard the PR
// body's own log records: see that log for the migration-file mutation
// (temporarily degrading the column to TEXT) that proved this test
// actually fails without the real ENUM type backing it.
func TestImageDecisionReasonColumn_RejectsValueOutsideEnum(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID,
		TokenHash: nil,
	}); err != nil {
		t.Fatalf("seed sandbox row: %v", err)
	}

	_, err := pool.Exec(ctx,
		`UPDATE sandboxes SET image_decision_reason = $2 WHERE session_id = $1`,
		sessionID, "not_a_real_reason")
	if err == nil {
		t.Fatal("UPDATE sandboxes with an out-of-vocabulary image_decision_reason succeeded, want a Postgres error (enum constraint not enforced)")
	}
	t.Logf("got the expected Postgres rejection: %v", err)
}

// TestResolveAndSetImage_NoRepos_PersistsNoReposReason proves the ONE
// early return this whole gap's own worst instance used to be: a bare
// `return` with no log line, no persisted trace, nothing. A session with
// zero configured repos now persists imagedecision.ReasonNoRepos onto
// sandboxes AND appends a matching "image_decision" event, with no
// fingerprint (there is nothing to fingerprint).
func TestResolveAndSetImage_NoRepos_PersistsNoReposReason(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSession(ctx, t, pool) // no repos, no creator

	sourceControl := &fakeSourceControl{}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-norepos"}}
	r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
	t.Cleanup(func() { _ = r.Shutdown() })

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	sandboxStore := narvipg.NewSandboxStore(pool)
	var sb sqlcgen.Sandbox
	waitUntil(t, 5*time.Second, func() bool {
		sb, err = sandboxStore.Get(ctx, sessionID)
		return err == nil && sb.ImageDecisionReason != nil
	})
	if sb.ImageDecisionReason == nil {
		t.Fatal("sandboxes.image_decision_reason is still NULL, want ReasonNoRepos")
	}
	if got := imagedecision.Reason(*sb.ImageDecisionReason); got != imagedecision.ReasonNoRepos {
		t.Errorf("sandboxes.image_decision_reason = %q, want %q", got, imagedecision.ReasonNoRepos)
	}
	if sb.ImageDecisionFingerprint != nil {
		t.Errorf("sandboxes.image_decision_fingerprint = %q, want nil (nothing to fingerprint)", *sb.ImageDecisionFingerprint)
	}

	assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonNoRepos, "", sb.Gen)
}

// TestResolveAndSetImage_NoCreatedByUser_PersistsNoCreatorReason proves an
// automation-created session (created_by IS NULL) persists
// imagedecision.ReasonRepoAccessNoCreator, WITH a real fingerprint (the
// repo-access gate runs, and is denied, strictly after Fingerprint is
// computed -- see decideImage's own doc comment on why that reordering is
// deliberate).
func TestResolveAndSetImage_NoCreatedByUser_PersistsNoCreatorReason(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, // no creator
		"repo1", "https://github.com/acme/repo1.git", "main")

	sourceControl := &fakeSourceControl{}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-nocreator"}}
	r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
	t.Cleanup(func() { _ = r.Shutdown() })

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	wantFingerprint := domainimagebuild.Fingerprint(defaultBaseImage,
		map[string]string{"repo1": "https://github.com/acme/repo1.git"}, testRuntimeVersion)

	sb := waitForImageDecision(ctx, t, pool, sessionID)
	if got := imagedecision.Reason(*sb.ImageDecisionReason); got != imagedecision.ReasonRepoAccessNoCreator {
		t.Errorf("sandboxes.image_decision_reason = %q, want %q", got, imagedecision.ReasonRepoAccessNoCreator)
	}
	if sb.ImageDecisionFingerprint == nil || *sb.ImageDecisionFingerprint != wantFingerprint {
		t.Errorf("sandboxes.image_decision_fingerprint = %v, want %q", sb.ImageDecisionFingerprint, wantFingerprint)
	}

	assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonRepoAccessNoCreator, wantFingerprint, sb.Gen)
}

// TestResolveAndSetImage_RepoAccessDenied_PersistsDeniedReason proves the
// actual attack case the repo-access gate exists to close
// (repoaccessgate_integration_test.go's own scope) ALSO now leaves a
// durable, countable trace: imagedecision.ReasonRepoAccessDenied.
func TestResolveAndSetImage_RepoAccessDenied_PersistsDeniedReason(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-decision-denied")
	sessionID := createTestSessionWithRepos(ctx, t, pool, creator,
		"private-repo", "https://github.com/victim-org/private-repo.git", "main")

	sourceControl := &fakeSourceControl{denyAllAccess: true}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-decision-denied"}}
	r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
	t.Cleanup(func() { _ = r.Shutdown() })

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "read the secrets")

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	wantFingerprint := domainimagebuild.Fingerprint(defaultBaseImage,
		map[string]string{"private-repo": "https://github.com/victim-org/private-repo.git"}, testRuntimeVersion)

	sb := waitForImageDecision(ctx, t, pool, sessionID)
	if got := imagedecision.Reason(*sb.ImageDecisionReason); got != imagedecision.ReasonRepoAccessDenied {
		t.Errorf("sandboxes.image_decision_reason = %q, want %q", got, imagedecision.ReasonRepoAccessDenied)
	}
	assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonRepoAccessDenied, wantFingerprint, sb.Gen)
}

// TestResolveAndSetImage_Miss_PersistsPendingReason proves the ordinary,
// non-error "no build has ever completed for this repo set yet" case
// persists imagedecision.ReasonImageBuildPending (never conflated with a
// genuine failure).
func TestResolveAndSetImage_Miss_PersistsPendingReason(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-decision-pending")
	sessionID := createTestSessionWithRepos(ctx, t, pool, creator,
		"repo1", "https://github.com/acme/repo1.git", "main")

	sourceControl := &fakeSourceControl{}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-decision-pending"}}
	r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
	t.Cleanup(func() { _ = r.Shutdown() })

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	wantFingerprint := domainimagebuild.Fingerprint(defaultBaseImage,
		map[string]string{"repo1": "https://github.com/acme/repo1.git"}, testRuntimeVersion)

	sb := waitForImageDecision(ctx, t, pool, sessionID)
	if got := imagedecision.Reason(*sb.ImageDecisionReason); got != imagedecision.ReasonImageBuildPending {
		t.Errorf("sandboxes.image_decision_reason = %q, want %q", got, imagedecision.ReasonImageBuildPending)
	}
	assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonImageBuildPending, wantFingerprint, sb.Gen)
}

// TestResolveAndSetImage_WarmHit_PersistsSelectedReasonWithBuiltRepoShas
// proves the one non-fallback outcome is ALSO persisted (§19's own point:
// "warm-boot hit rate" needs the successes counted too, not only the
// fallbacks) -- and that the warm-hit row's own built_repo_shas rides the
// event payload, honestly labeled as repo-COMMIT identity (see
// persistImageDecisionBestEffort's own doc comment for why this is
// explicitly not "which dependency manifest changed").
func TestResolveAndSetImage_WarmHit_PersistsSelectedReasonWithBuiltRepoShas(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-decision-selected")
	sessionID := createTestSessionWithRepos(ctx, t, pool, creator,
		"repo1", "https://github.com/acme/repo1.git", "main")

	fingerprint := domainimagebuild.Fingerprint(defaultBaseImage, map[string]string{"repo1": "https://github.com/acme/repo1.git"}, testRuntimeVersion)

	imageBuildStore := narvipg.NewImageBuildStore(pool)
	seedReadyImageBuild(ctx, t, imageBuildStore, fingerprint, "narvi/built-image:decision-selected")

	sourceControl := &fakeSourceControl{}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-decision-selected"}}
	r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
	t.Cleanup(func() { _ = r.Shutdown() })

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	sb := waitForImageDecision(ctx, t, pool, sessionID)
	if got := imagedecision.Reason(*sb.ImageDecisionReason); got != imagedecision.ReasonSelected {
		t.Errorf("sandboxes.image_decision_reason = %q, want %q", got, imagedecision.ReasonSelected)
	}
	if sb.ImageDecisionFingerprint == nil || *sb.ImageDecisionFingerprint != fingerprint {
		t.Errorf("sandboxes.image_decision_fingerprint = %v, want %q", sb.ImageDecisionFingerprint, fingerprint)
	}

	payload := assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonSelected, fingerprint, sb.Gen)
	rawShas, ok := payload["built_repo_shas"]
	if !ok {
		t.Fatal(`image_decision event payload has no "built_repo_shas" key, want the warm-hit row's own repo-commit map`)
	}
	shasJSON, err := json.Marshal(rawShas)
	if err != nil {
		t.Fatalf("re-marshal built_repo_shas: %v", err)
	}
	var shas map[string]string
	if err := json.Unmarshal(shasJSON, &shas); err != nil {
		t.Fatalf("unmarshal built_repo_shas: %v", err)
	}
	if shas["repo1"] != "sha-warm-hit" {
		t.Errorf(`built_repo_shas["repo1"] = %q, want "sha-warm-hit" (seedReadyImageBuild's own seeded value)`, shas["repo1"])
	}
}

// TestUpsertSandboxForSpawn_Respawn_ResetsImageDecisionToNull proves the
// zero-value discipline migrations/000139_sandboxes_image_decision.up.sql
// documents: a respawn (gen bump) resets image_decision_reason/
// image_decision_fingerprint back to NULL, exactly like agent_version/
// image_digest already do -- a stale PREVIOUS-gen reason must never linger
// and be misread as the new gen's own outcome.
func TestUpsertSandboxForSpawn_Respawn_ResetsImageDecisionToNull(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sandboxStore := narvipg.NewSandboxStore(pool)

	sessionID := createTestSession(ctx, t, pool)

	first, err := sandboxStore.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID})
	if err != nil {
		t.Fatalf("first UpsertForSpawn: %v", err)
	}
	if first.Gen != 1 {
		t.Fatalf("first gen = %d, want 1", first.Gen)
	}

	reason := sqlcgen.ImageDecisionReasonSelected
	fingerprint := "fingerprint-from-gen-1"
	if _, err := sandboxStore.UpdateImageDecision(ctx, sqlcgen.UpdateSandboxImageDecisionParams{
		SessionID:                sessionID,
		ImageDecisionReason:      &reason,
		ImageDecisionFingerprint: &fingerprint,
	}); err != nil {
		t.Fatalf("UpdateImageDecision for gen 1: %v", err)
	}

	afterFirstDecision, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("Get after gen-1 decision: %v", err)
	}
	if afterFirstDecision.ImageDecisionReason == nil || *afterFirstDecision.ImageDecisionReason != sqlcgen.ImageDecisionReasonSelected {
		t.Fatalf("sanity check failed: gen-1 decision did not persist")
	}

	second, err := sandboxStore.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID})
	if err != nil {
		t.Fatalf("second UpsertForSpawn (respawn): %v", err)
	}
	if second.Gen != 2 {
		t.Fatalf("second gen = %d, want 2 (respawn must bump gen)", second.Gen)
	}
	if second.ImageDecisionReason != nil {
		t.Errorf("gen-2 image_decision_reason = %q, want nil (must reset on respawn, not leak gen 1's decision)", *second.ImageDecisionReason)
	}
	if second.ImageDecisionFingerprint != nil {
		t.Errorf("gen-2 image_decision_fingerprint = %q, want nil (must reset on respawn)", *second.ImageDecisionFingerprint)
	}
}

// TestResolveAndSetImage_ReadyRowMissingRef_PersistsAnomalyReason proves
// the SECOND completely silent path this fix closes: before this Step,
// row.Status == 'ready' with a nil/empty image_ref fell through to the
// exact same bare "return" as the pending/building case -- NO log line at
// all, indistinguishable from an ordinary still-building state. It now
// persists its own distinct imagedecision.ReasonImageBuildReadyRowMissingRef.
func TestResolveAndSetImage_ReadyRowMissingRef_PersistsAnomalyReason(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	creator := createTestUserWithGitHubToken(ctx, t, pool, "gh-fake-token-decision-anomaly")
	sessionID := createTestSessionWithRepos(ctx, t, pool, creator,
		"repo1", "https://github.com/acme/repo1.git", "main")

	fingerprint := domainimagebuild.Fingerprint(defaultBaseImage, map[string]string{"repo1": "https://github.com/acme/repo1.git"}, testRuntimeVersion)

	imageBuildStore := narvipg.NewImageBuildStore(pool)
	repoURLs, err := json.Marshal(map[string]string{"repo1": "https://github.com/acme/repo1"})
	if err != nil {
		t.Fatalf("marshal repo urls: %v", err)
	}
	if err := imageBuildStore.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           defaultBaseImage,
		RepoUrls:       repoURLs,
		RuntimeVersion: testRuntimeVersion,
	}); err != nil {
		t.Fatalf("seed pending image_builds row: %v", err)
	}
	if _, err := imageBuildStore.Claim(ctx, fingerprint); err != nil {
		t.Fatalf("claim image_builds row: %v", err)
	}
	// Force the data-integrity anomaly directly: a 'ready' row with a NULL
	// image_ref should never happen through this codebase's own real
	// write paths (RecordSuccess always writes a real ref alongside
	// status='ready') -- simulating the "should not happen" case this
	// guard exists for, the same way a defensive branch elsewhere in this
	// codebase is tested by directly seeding the shape it guards against.
	if _, err := pool.Exec(ctx, `UPDATE image_builds SET status = 'ready', image_ref = NULL WHERE fingerprint = $1`, fingerprint); err != nil {
		t.Fatalf("force ready-row-missing-ref anomaly: %v", err)
	}

	sourceControl := &fakeSourceControl{}
	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-decision-anomaly"}}
	r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
	t.Cleanup(func() { _ = r.Shutdown() })

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

	if got := provider.lastSpec().Image; got != defaultBaseImage {
		t.Errorf("CreateSpec.Image = %q, want the base image %q (a ready row missing image_ref must never be used)", got, defaultBaseImage)
	}

	sb := waitForImageDecision(ctx, t, pool, sessionID)
	if got := imagedecision.Reason(*sb.ImageDecisionReason); got != imagedecision.ReasonImageBuildReadyRowMissingRef {
		t.Errorf("sandboxes.image_decision_reason = %q, want %q", got, imagedecision.ReasonImageBuildReadyRowMissingRef)
	}
	assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonImageBuildReadyRowMissingRef, fingerprint, sb.Gen)
}

// TestResolveAndSetImage_DisclosingReasons_EventCoarsensReasonButColumnStaysPrecise
// is A2's own audit fix (disclosure, "warm-boot decision disclosure"),
// proven end to end against a real Postgres instance: ReasonRepoAccess
// CreatorDisabled/CreatorViewer/NoToken each name admin-only session-
// creator account state (§13.3; Settings -> Members' own admin-only
// ListMembers/UpdateMemberRole, members.go) -- but the "image_decision"
// event is served, unconditionally, to any logged-in reader
// (httpapi.ListEvents/client-WS replay check neither role nor session
// membership). For each of the three, this proves the sandboxes COLUMN
// keeps the exact, precise reason (an operator loses nothing), while the
// SERVED EVENT's own "reason" field reads the coarser, already-non-
// sensitive imagedecision.ReasonRepoAccessDenied instead -- never the
// precise value, which would disclose the creator's account state to a
// reader who cannot see it anywhere else in this product.
func TestResolveAndSetImage_DisclosingReasons_EventCoarsensReasonButColumnStaysPrecise(t *testing.T) {
	tests := []struct {
		name       string
		repoName   string
		wantColumn imagedecision.Reason
		setUser    func(t *testing.T, pool *pgxpool.Pool) pgtype.UUID
	}{
		{
			name:       "disabled creator",
			repoName:   "repo-decision-disabled",
			wantColumn: imagedecision.ReasonRepoAccessCreatorDisabled,
			setUser: func(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				creator := createTestUserWithGitHubToken(context.Background(), t, pool, "gh-fake-token-decision-disabled")
				if _, err := pool.Exec(context.Background(), `UPDATE users SET disabled = true WHERE id = $1`, creator); err != nil {
					t.Fatalf("disable fixture user: %v", err)
				}
				return creator
			},
		},
		{
			name:       "viewer creator",
			repoName:   "repo-decision-viewer",
			wantColumn: imagedecision.ReasonRepoAccessCreatorViewer,
			setUser: func(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				creator := createTestUserWithGitHubToken(context.Background(), t, pool, "gh-fake-token-decision-viewer")
				if _, err := narvipg.NewUserStore(pool).UpdateRole(context.Background(), creator, sqlcgen.UserRoleViewer); err != nil {
					t.Fatalf("demote fixture user to viewer: %v", err)
				}
				return creator
			},
		},
		{
			name:       "creator with no linked github identity/token",
			repoName:   "repo-decision-no-token",
			wantColumn: imagedecision.ReasonRepoAccessNoToken,
			setUser: func(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				user, err := narvipg.NewUserStore(pool).Create(context.Background(), sqlcgen.CreateUserParams{
					PrimaryEmail: fmt.Sprintf("imagedecision-test-no-token-%d@example.com", time.Now().UnixNano()),
					DisplayName:  "Image Decision No-Token Test User",
					Role:         sqlcgen.UserRoleMember,
				})
				if err != nil {
					t.Fatalf("create fixture user with no linked github identity: %v", err)
				}
				return user.ID
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)

			creator := tc.setUser(t, pool)
			repoURL := "https://github.com/acme/" + tc.repoName + ".git"
			sessionID := createTestSessionWithRepos(ctx, t, pool, creator, tc.repoName, repoURL, "main")

			sourceControl := &fakeSourceControl{}
			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-" + tc.repoName}}
			r := newImageBuildTestRegistry(t, ctx, pool, provider, sourceControl)
			t.Cleanup(func() { _ = r.Shutdown() })

			turnStore := narvipg.NewTurnStore(pool)
			createPendingTurn(ctx, t, turnStore, sessionID, "do the thing")

			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

			sb := waitForImageDecision(ctx, t, pool, sessionID)
			if got := imagedecision.Reason(*sb.ImageDecisionReason); got != tc.wantColumn {
				t.Errorf("sandboxes.image_decision_reason = %q, want the PRECISE %q -- the column must never be coarsened, only the served event", got, tc.wantColumn)
			}

			// The served event must read the COARSER, already-non-sensitive
			// bucket. Asserting reason=ReasonRepoAccessDenied here (rather
			// than tc.wantColumn) is the disclosure assertion itself: if
			// participantVisibleReason ever regressed to passing the
			// precise reason through unchanged, no event with
			// reason="repo_access_denied" would ever appear and this call
			// times out and fails, exactly as it should.
			if got := sourceControl.accessCallCount(); got != 0 {
				t.Errorf("CheckRepoAccess call count = %d, want 0 (CheckCreatorGuard/getToken must deny before SourceControl is ever reached)", got)
			}
			assertImageDecisionEvent(ctx, t, pool, sessionID, imagedecision.ReasonRepoAccessDenied, "", sb.Gen)
		})
	}
}

// waitForImageDecision polls sandboxStore.Get until image_decision_reason
// is populated (resolveAndSetImage's own persistence runs asynchronously,
// on the actor's own mailbox goroutine, strictly after the provider call
// this package's other tests already wait on) and returns the row.
func waitForImageDecision(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) sqlcgen.Sandbox {
	t.Helper()
	sandboxStore := narvipg.NewSandboxStore(pool)
	var sb sqlcgen.Sandbox
	var err error
	waitUntil(t, 5*time.Second, func() bool {
		sb, err = sandboxStore.Get(ctx, sessionID)
		return err == nil && sb.ImageDecisionReason != nil
	})
	if err != nil {
		t.Fatalf("get sandbox row: %v", err)
	}
	return sb
}

// assertImageDecisionEvent polls the session's own event log for exactly
// one "image_decision" event whose payload's own "reason" field matches
// want (the EVENT's own reason -- audit fix A2, "warm-boot decision
// disclosure": this is participantVisibleReason's own coarsened form for
// the three disclosing reasons, NOT necessarily the sandboxes column's
// own precise value; callers testing one of those three pass
// imagedecision.ReasonRepoAccessDenied here, not the precise reason), and
// -- when wantFingerprint is non-empty -- whose "fingerprint" field
// matches it too. Also asserts the payload's own "gen" field (audit fix
// A4, "the append-only half cannot be attributed to a generation")
// against wantGen -- every existing caller passes the SAME sandboxes row
// (sqlcgen.Sandbox.Gen) waitForImageDecision already read back, rather
// than a hardcoded literal, so this stays correct even if this package's
// own gen-numbering ever changes. Returns the decoded payload so callers
// needing more (built_repo_shas) can inspect it further.
func assertImageDecisionEvent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, want imagedecision.Reason, wantFingerprint string, wantGen int32) map[string]any {
	t.Helper()
	eventStore := narvipg.NewEventStore(pool)

	var matched map[string]any
	waitUntil(t, 5*time.Second, func() bool {
		rows, err := eventStore.ListForSession(ctx, sessionID, 0, 100)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.Type != "image_decision" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				continue
			}
			if payload["reason"] == string(want) {
				matched = payload
				return true
			}
		}
		return false
	})

	if matched == nil {
		t.Fatalf(`no "image_decision" event found with reason=%q for session %s`, want, sessionID.String())
	}
	if wantFingerprint != "" {
		if got, _ := matched["fingerprint"].(string); got != wantFingerprint {
			t.Errorf(`image_decision event payload["fingerprint"] = %q, want %q`, got, wantFingerprint)
		}
	}
	gotGen, ok := matched["gen"].(float64)
	if !ok {
		t.Fatalf(`image_decision event payload["gen"] = %v (%T), want a JSON number`, matched["gen"], matched["gen"])
	}
	if int32(gotGen) != wantGen {
		t.Errorf(`image_decision event payload["gen"] = %v, want %d`, gotGen, wantGen)
	}
	return matched
}

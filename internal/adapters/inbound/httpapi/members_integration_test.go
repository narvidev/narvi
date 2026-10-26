//go:build integration

package httpapi_test

// This file proves the members API's own 5 endpoints (members.go) --
// admin-only ListMembers/UpdateMemberRole/LinkMemberIdentity/
// UnlinkMemberIdentity/ListAuditLog -- which shipped with NO tests of any
// kind (an audit finding, H8) despite the 5 admin-only authorize() guards,
// the 409-already-linked conflict, idempotent relink, no-differential-404
// unlink, and role-string validation all being real, load-bearing
// behavior. It also proves this SAME batch's own two accompanying
// hardening fixes: UpdateMemberRole's last-admin guard (refusing a role
// change that would leave zero active admins) and LinkMemberIdentity's
// race-safe already-linked check (the conflict lookup and the Create now
// share one transaction, so a concurrent duplicate-link request reliably
// gets 409, never a raw constraint-violation 500).
//
// Setup mirrors this package's own established rig (newTestRig,
// createUserWithRole -- planapprove_integration_test.go's own helper,
// reused as-is since this file lives in the SAME httpapi_test package).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
)

// --- Local wire-shape mirrors ---
//
// members.go itself no longer has any hand-written DTOs of its own: its 5
// endpoints now respond with the generated, exported contracts/gen/go/
// restdtos types directly (restdtos.Identity, restdtos.Member,
// restdtos.PendingLinkPrompt, restdtos.ListMembersResponse, restdtos.
// AuditLogEntry, restdtos.ListAuditLogResponse -- an audit finding,
// wire-contract, promoted these into /contracts as a pure migration; see
// contracts/README.md's own "Members/audit-log DTOs" section). The local
// shapes below are kept anyway, purely as a test-side convenience: plain
// string fields (no restdtos-generated enum type's own strict
// UnmarshalJSON to fight with when asserting on a response) matching the
// real JSON tags exactly -- mirroring planapprove_integration_test.go's
// own planActionResponseForTest precedent, which uses the identical
// convention for planapprove.go's own STILL-unexported response shape.

type identityDTOForTest struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	ExternalID string    `json:"externalId"`
	LinkedVia  string    `json:"linkedVia"`
	CreatedAt  time.Time `json:"createdAt"`
}

type memberDTOForTest struct {
	ID          string               `json:"id"`
	Email       string               `json:"email"`
	DisplayName string               `json:"displayName"`
	Role        string               `json:"role"`
	Disabled    bool                 `json:"disabled"`
	CreatedAt   time.Time            `json:"createdAt"`
	Identities  []identityDTOForTest `json:"identities"`
}

type pendingLinkPromptDTOForTest struct {
	Provider   string    `json:"provider"`
	ExternalID string    `json:"externalId"`
	ExpiresAt  time.Time `json:"expiresAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

type listMembersResponseForTest struct {
	Members            []memberDTOForTest            `json:"members"`
	PendingLinkPrompts []pendingLinkPromptDTOForTest `json:"pendingLinkPrompts"`
}

type auditLogEntryDTOForTest struct {
	ID           string                 `json:"id"`
	ActorUserID  *string                `json:"actorUserId"`
	Action       string                 `json:"action"`
	ResourceType string                 `json:"resourceType"`
	ResourceID   string                 `json:"resourceId"`
	Detail       map[string]interface{} `json:"detail"`
}

type listAuditLogResponseForTest struct {
	Entries []auditLogEntryDTOForTest `json:"entries"`
}

// errorResponseForTest mirrors helpers.go's own writeError body shape
// ({"error": message}).
type errorResponseForTest struct {
	Error string `json:"error"`
}

// auditLogDetailFor fetches the single action audit_log row's own
// detail_json for resourceID, decoded into a plain map -- mirrors
// decideplan_integration_test.go's own auditLogDetailForPlanDecision
// precedent exactly (that file's own identically-shaped helper, scoped to
// resource_type='plan'), generalized here since this file's own audit-fix
// (M9, completeness) additions below need the same shape for THREE
// different (action, resource_type) pairs -- member.role_changed/user,
// identity.force_linked/identity, identity.unlinked/identity -- each of
// which, before this fix, had an existing test proving the row EXISTS with
// the right action/actor (see each test's own doc comment below) but never
// decoded/asserted its own detail_json shape at all.
func auditLogDetailFor(ctx context.Context, t *testing.T, r testRig, action, resourceID string) map[string]any {
	t.Helper()
	var detailRaw []byte
	if err := r.pool.QueryRow(ctx,
		`SELECT detail_json FROM audit_log WHERE action = $1 AND resource_id = $2`,
		action, resourceID,
	).Scan(&detailRaw); err != nil {
		t.Fatalf("query audit_log detail_json for action %q resource_id %q: %v", action, resourceID, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("unmarshal audit_log detail_json: %v", err)
	}
	return detail
}

// --- authorize()'s admin-only gate: all 5 endpoints, every non-admin role ---

// TestMembersRoutes_NonAdmin_Returns403 proves every one of the 5
// endpoints in members.go rejects a viewer/member/maintainer caller with
// 403 -- §13.3's own "members & roles: admin only" row, bundled behind
// ONE authz.Action (authz.ActionManageMembers) for every route here,
// including the two plain read endpoints (see members.go's own top doc
// comment).
func TestMembersRoutes_NonAdmin_Returns403(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	roles := []sqlcgen.UserRole{sqlcgen.UserRoleMember, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleViewer}
	for _, role := range roles {
		_, token := createUserWithRole(ctx, t, rig, role)

		endpoints := []struct {
			name   string
			method string
			path   string
			body   []byte
		}{
			{"ListMembers", http.MethodGet, "/api/members", nil},
			{"UpdateMemberRole", http.MethodPatch, "/api/members/" + target.ID.String() + "/role", []byte(`{"role":"viewer"}`)},
			{"LinkMemberIdentity", http.MethodPost, "/api/members/" + target.ID.String() + "/identities", []byte(`{"provider":"github","externalId":"nonadmin-probe"}`)},
			{"UnlinkMemberIdentity", http.MethodDelete, "/api/members/" + target.ID.String() + "/identities/" + uuid.NewString(), nil},
			{"ListAuditLog", http.MethodGet, "/api/audit-log", nil},
		}
		for _, ep := range endpoints {
			t.Run(string(role)+"/"+ep.name, func(t *testing.T) {
				status := rig.doJSON(t, ep.method, ep.path, ep.body, nil, token)
				if status != http.StatusForbidden {
					t.Errorf("status = %d, want %d (role=%s)", status, http.StatusForbidden, role)
				}
			})
		}
	}
}

// --- LinkMemberIdentity ---

// TestLinkMemberIdentity_HappyPath_Returns201 proves the plain success
// path: a brand-new (provider, externalId) linked by an admin gets 201,
// linked_via "admin", and a same-transaction audit_log row.
func TestLinkMemberIdentity_HappyPath_Returns201(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"provider":"slack","externalId":"U12345"}`)
	var got identityDTOForTest
	status := rig.doJSON(t, http.MethodPost, "/api/members/"+target.ID.String()+"/identities", body, &got, adminToken)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if got.Provider != "slack" || got.ExternalID != "U12345" {
		t.Errorf("provider/externalId = %q/%q, want slack/U12345", got.Provider, got.ExternalID)
	}
	if got.LinkedVia != "admin" {
		t.Errorf("LinkedVia = %q, want %q", got.LinkedVia, "admin")
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'identity.force_linked' AND actor_user_id = $1`, admin.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count audit log rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit log rows = %d, want 1", count)
	}

	// Audit-fix batch addition (M9, completeness): the row's own existence
	// was already proven above -- this is the previously-missing half, that
	// its detail_json actually carries the shape members.go's own
	// LinkMemberIdentity writes (user_id/provider/external_id), not merely
	// SOME detail blob.
	detail := auditLogDetailFor(ctx, t, rig, "identity.force_linked", got.ID)
	if detail["user_id"] != target.ID.String() {
		t.Errorf("detail_json[user_id] = %v, want %q", detail["user_id"], target.ID.String())
	}
	if detail["provider"] != "slack" {
		t.Errorf("detail_json[provider] = %v, want %q", detail["provider"], "slack")
	}
	if detail["external_id"] != "U12345" {
		t.Errorf("detail_json[external_id] = %v, want %q", detail["external_id"], "U12345")
	}
}

// TestLinkMemberIdentity_AlreadyLinkedSameUser_Idempotent_Returns200
// proves relinking the SAME (provider, externalId) to the SAME user is
// idempotent: 200 (not 201), the same identity id back, and no second
// identities row created.
func TestLinkMemberIdentity_AlreadyLinkedSameUser_Idempotent_Returns200(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"provider":"linear","externalId":"user-abc"}`)
	var first identityDTOForTest
	status := rig.doJSON(t, http.MethodPost, "/api/members/"+target.ID.String()+"/identities", body, &first, adminToken)
	if status != http.StatusCreated {
		t.Fatalf("first link status = %d, want %d", status, http.StatusCreated)
	}

	var second identityDTOForTest
	status = rig.doJSON(t, http.MethodPost, "/api/members/"+target.ID.String()+"/identities", body, &second, adminToken)
	if status != http.StatusOK {
		t.Fatalf("relink status = %d, want %d (idempotent, not an error)", status, http.StatusOK)
	}
	if second.ID != first.ID {
		t.Errorf("relink returned a DIFFERENT identity id: %s vs %s", second.ID, first.ID)
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM identities WHERE provider = 'linear' AND external_id = 'user-abc'`,
	).Scan(&count); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if count != 1 {
		t.Errorf("identities rows = %d, want exactly 1 (idempotent relink must not create a second row)", count)
	}
}

// TestLinkMemberIdentity_UncheckedAutoLinkBecomesAdminLink proves the
// same-user, already-linked answer stays true for an automatic Slack link
// nothing has confirmed under §13.2's Slack rule (no email_checked_at): the
// force-link turns it into the admin link asked for -- in place, audited as
// a force-link, with no verified email left to count as evidence -- so it
// resolves to the member at once and a later re-check never removes it.
// Any other existing link answers 200 unchanged, nothing written.
func TestLinkMemberIdentity_UncheckedAutoLinkBecomesAdminLink(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	actor, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	linkDeps := identitylink.Deps{
		Pool: rig.pool, Users: rig.users, Identities: rig.identities,
		LinkPrompts: narvipg.NewIdentityLinkPromptStore(rig.pool), AuditLog: narvipg.NewAuditLogStore(rig.pool),
		PublicBaseURL: "https://narvi.example.com", PromptTTL: time.Hour,
	}

	for i, tc := range []struct {
		name          string
		via           sqlcgen.IdentityLinkedVia
		verified      bool
		stamped       bool
		wantLinkedVia string
	}{
		{"unchecked Slack automatic link", sqlcgen.IdentityLinkedViaAutoEmail, true, false, "admin"},
		{"stamped Slack automatic link", sqlcgen.IdentityLinkedViaAutoEmail, true, true, "auto_email"},
		{"Slack link via the link prompt", sqlcgen.IdentityLinkedViaPrompt, false, false, "prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
			externalID := "U-FORCE-" + string(rune('A'+i))
			email := "force-" + string(rune('a'+i)) + "@example.com"
			seed := sqlcgen.CreateIdentityParams{
				UserID: target.ID, Provider: sqlcgen.IdentityProviderSlack, ExternalID: externalID,
				Email: &email, EmailVerified: tc.verified, LinkedVia: tc.via,
			}
			if tc.stamped {
				seed.EmailCheckedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
			}
			seeded, err := rig.identities.Create(ctx, seed)
			if err != nil {
				t.Fatalf("seed identity: %v", err)
			}

			var got identityDTOForTest
			body := []byte(`{"provider":"slack","externalId":"` + externalID + `"}`)
			status := rig.doJSON(t, http.MethodPost, "/api/members/"+target.ID.String()+"/identities", body, &got, adminToken)
			if status != http.StatusOK {
				t.Fatalf("force-link status = %d, want %d (already linked to this member)", status, http.StatusOK)
			}
			if got.ID != seeded.ID.String() || got.LinkedVia != tc.wantLinkedVia {
				t.Errorf("force-link answered %+v, want id %s, linkedVia %q", got, seeded.ID, tc.wantLinkedVia)
			}

			row, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, externalID)
			if err != nil {
				t.Fatalf("GetByProviderAndExternalID: %v", err)
			}
			if string(row.LinkedVia) != tc.wantLinkedVia {
				t.Errorf("stored linked_via = %q, want %q", row.LinkedVia, tc.wantLinkedVia)
			}
			converted := tc.wantLinkedVia == "admin"
			if converted && row.EmailVerified {
				t.Error("stored email_verified = true, want false: the admin vouches for the link, not the email")
			}

			var entries int
			if err := rig.pool.QueryRow(ctx,
				`SELECT count(*) FROM audit_log WHERE action = 'identity.force_linked' AND resource_id = $1`, seeded.ID.String(),
			).Scan(&entries); err != nil {
				t.Fatalf("count audit entries: %v", err)
			}
			if want := map[bool]int{true: 1, false: 0}[converted]; entries != want {
				t.Fatalf("identity.force_linked entries = %d, want %d", entries, want)
			}
			if converted {
				var actorID pgtype.UUID
				if err := rig.pool.QueryRow(ctx,
					`SELECT actor_user_id FROM audit_log WHERE action = 'identity.force_linked' AND resource_id = $1`, seeded.ID.String(),
				).Scan(&actorID); err != nil {
					t.Fatalf("read audit actor: %v", err)
				}
				if actorID != actor.ID {
					t.Errorf("audit actor = %v, want the admin %v", actorID, actor.ID)
				}
				detail := auditLogDetailFor(ctx, t, rig, "identity.force_linked", seeded.ID.String())
				if detail["converted_from"] != "auto_email" || detail["user_id"] != target.ID.String() || detail["external_id"] != externalID {
					t.Errorf("audit detail = %v, want converted_from auto_email, user_id %v, external_id %q", detail, target.ID, externalID)
				}
				if ids, err := identitylink.MatchUserIDs(ctx, linkDeps, email); err != nil || len(ids) != 0 {
					t.Errorf("MatchUserIDs(%q) = (%v, %v), want no match: the email was never confirmed", email, ids, err)
				}
			}

			// Resolves to the member, and an unconfirmed email on the
			// member's next event removes nothing.
			if userID, linked, err := identitylink.LookupLinkedUserID(ctx, linkDeps, sqlcgen.IdentityProviderSlack, externalID); err != nil || !linked || userID != target.ID {
				t.Errorf("LookupLinkedUserID = (%v, %v, %v), want linked to %v", userID, linked, err, target.ID)
			}
			res, err := identitylink.Resolve(ctx, linkDeps, sqlcgen.IdentityProviderSlack, externalID, identitylink.ProfileEmail{Address: email, Verified: false}, true)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.UserID != target.ID {
				t.Errorf("Resolve on an unconfirmed email = %+v, want the link to %v", res, target.ID)
			}
			if _, err := rig.identities.GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderSlack, externalID); err != nil {
				t.Errorf("identity after the next event: %v, want it kept", err)
			}
		})
	}
}

// TestLinkMemberIdentity_AlreadyLinkedDifferentUser_Returns409 proves the
// OTHER half of the same check: the SAME (provider, externalId) already
// linked to a DIFFERENT user is refused with 409, not silently
// reassigned.
func TestLinkMemberIdentity_AlreadyLinkedDifferentUser_Returns409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	targetA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	targetB, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"provider":"google","externalId":"dup-ext-id"}`)
	status := rig.doJSON(t, http.MethodPost, "/api/members/"+targetA.ID.String()+"/identities", body, nil, adminToken)
	if status != http.StatusCreated {
		t.Fatalf("first link status = %d, want %d", status, http.StatusCreated)
	}

	var errBody errorResponseForTest
	status = rig.doJSON(t, http.MethodPost, "/api/members/"+targetB.ID.String()+"/identities", body, &errBody, adminToken)
	if status != http.StatusConflict {
		t.Fatalf("second link (different target user) status = %d, want %d", status, http.StatusConflict)
	}
	if errBody.Error == "" {
		t.Error("expected a non-empty error message")
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM identities WHERE provider = 'google' AND external_id = 'dup-ext-id'`,
	).Scan(&count); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if count != 1 {
		t.Errorf("identities rows = %d, want exactly 1 (still just targetA's own row)", count)
	}
}

// TestLinkMemberIdentity_ConcurrentDuplicateLink_ExactlyOneSucceeds is
// this batch's own proof of the H8 race fix: two concurrent
// LinkMemberIdentity requests for the SAME (provider, externalId), aimed
// at two DIFFERENT target users, must resolve to exactly one 201
// (whichever wins the race) and one 409 (the loser) -- NEVER a raw 500
// from an unhandled unique-constraint violation, which is what the
// pre-fix code (conflict check outside the Create's own transaction)
// could produce under real concurrency. Mirrors
// planapprove_integration_test.go's own TestApprovePlan_
// ConcurrentDoubleApprove_ExactlyOneWins errgroup-based pattern exactly.
func TestLinkMemberIdentity_ConcurrentDuplicateLink_ExactlyOneSucceeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	targetA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	targetB, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"provider":"github","externalId":"race-ext-id"}`)
	pathA := "/api/members/" + targetA.ID.String() + "/identities"
	pathB := "/api/members/" + targetB.ID.String() + "/identities"

	var eg errgroup.Group
	statuses := make(chan int, 2)
	eg.Go(func() error {
		statuses <- rig.doJSON(t, http.MethodPost, pathA, body, nil, adminToken)
		return nil
	})
	eg.Go(func() error {
		statuses <- rig.doJSON(t, http.MethodPost, pathB, body, nil, adminToken)
		return nil
	})
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(statuses)

	var created, conflict int
	for s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			// A 200 would mean the "loser" resolved to its OWN target user
			// somehow -- never expected here (targetA != targetB), but not
			// a raw 500 either; counted separately so the assertions below
			// still catch it as a shape violation.
			t.Errorf("unexpected 200 among concurrent link responses (targetA != targetB, so no idempotent-relink case applies here)")
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d among concurrent link responses (must never be a raw 500)", s)
		}
	}
	if created != 1 {
		t.Errorf("created (201) = %d, want exactly 1", created)
	}
	if conflict != 1 {
		t.Errorf("conflict (409) = %d, want exactly 1", conflict)
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM identities WHERE provider = 'github' AND external_id = 'race-ext-id'`,
	).Scan(&count); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if count != 1 {
		t.Errorf("identities rows = %d, want exactly 1 (only the winner's own row)", count)
	}
}

// --- UnlinkMemberIdentity ---

// TestUnlinkMemberIdentity_HappyPath_Returns204 proves the plain success
// path: 204, the row is actually gone, and a same-transaction audit_log
// row was written.
func TestUnlinkMemberIdentity_HappyPath_Returns204(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	linkBody := []byte(`{"provider":"github","externalId":"unlink-me"}`)
	var linked identityDTOForTest
	status := rig.doJSON(t, http.MethodPost, "/api/members/"+target.ID.String()+"/identities", linkBody, &linked, adminToken)
	if status != http.StatusCreated {
		t.Fatalf("seed link status = %d, want %d", status, http.StatusCreated)
	}

	status = rig.doJSON(t, http.MethodDelete, "/api/members/"+target.ID.String()+"/identities/"+linked.ID, nil, nil, adminToken)
	if status != http.StatusNoContent {
		t.Fatalf("unlink status = %d, want %d", status, http.StatusNoContent)
	}

	// target also carries its OWN default GitHub identity from
	// createUserWithRole's own seeding (a different external_id than
	// "unlink-me") -- so the assertion is "the unlinked id is gone", not
	// "zero identities remain".
	remaining, err := rig.identities.ListForUser(ctx, target.ID)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	for _, i := range remaining {
		if i.ID.String() == linked.ID {
			t.Errorf("unlinked identity %s still present among target's own identities", linked.ID)
		}
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'identity.unlinked' AND actor_user_id = $1`, admin.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count audit log rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit log rows = %d, want 1", count)
	}

	// Audit-fix batch addition (M9, completeness): decode/assert the
	// detail_json shape members.go's own UnlinkMemberIdentity writes
	// (user_id/provider/external_id) -- the row's own existence was already
	// proven above.
	detail := auditLogDetailFor(ctx, t, rig, "identity.unlinked", linked.ID)
	if detail["user_id"] != target.ID.String() {
		t.Errorf("detail_json[user_id] = %v, want %q", detail["user_id"], target.ID.String())
	}
	if detail["provider"] != "github" {
		t.Errorf("detail_json[provider] = %v, want %q", detail["provider"], "github")
	}
	if detail["external_id"] != "unlink-me" {
		t.Errorf("detail_json[external_id] = %v, want %q", detail["external_id"], "unlink-me")
	}
}

// TestUnlinkMemberIdentity_NoDifferentialNotFoundSignal proves the
// handler's own documented promise: a 404 for an identityID that never
// existed, one that exists but belongs to a DIFFERENT user than the
// path's userID, and one that WAS linked but is now already unlinked, all
// come back with the exact same body -- a caller probing identity ids
// belonging to other members (or re-probing an already-unlinked one)
// learns nothing extra from the response, mirroring auth.Middleware's own
// "no differential signal" precedent this handler's doc comment cites.
func TestUnlinkMemberIdentity_NoDifferentialNotFoundSignal(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	userA, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	userB, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	linkBody := []byte(`{"provider":"slack","externalId":"probe-ext-id"}`)
	var linked identityDTOForTest
	status := rig.doJSON(t, http.MethodPost, "/api/members/"+userA.ID.String()+"/identities", linkBody, &linked, adminToken)
	if status != http.StatusCreated {
		t.Fatalf("seed link status = %d, want %d", status, http.StatusCreated)
	}

	var neverExisted, wrongUser, alreadyUnlinked errorResponseForTest

	// Case 1: identityID never existed at all.
	status = rig.doJSON(t, http.MethodDelete, "/api/members/"+userA.ID.String()+"/identities/"+uuid.NewString(), nil, &neverExisted, adminToken)
	if status != http.StatusNotFound {
		t.Fatalf("never-existed status = %d, want %d", status, http.StatusNotFound)
	}

	// Case 2: identityID is real (linked to userA), probed via userB's own
	// path.
	status = rig.doJSON(t, http.MethodDelete, "/api/members/"+userB.ID.String()+"/identities/"+linked.ID, nil, &wrongUser, adminToken)
	if status != http.StatusNotFound {
		t.Fatalf("wrong-user status = %d, want %d", status, http.StatusNotFound)
	}

	// Case 3: genuinely unlink it via the CORRECT path, then probe again.
	status = rig.doJSON(t, http.MethodDelete, "/api/members/"+userA.ID.String()+"/identities/"+linked.ID, nil, nil, adminToken)
	if status != http.StatusNoContent {
		t.Fatalf("real unlink status = %d, want %d", status, http.StatusNoContent)
	}
	status = rig.doJSON(t, http.MethodDelete, "/api/members/"+userA.ID.String()+"/identities/"+linked.ID, nil, &alreadyUnlinked, adminToken)
	if status != http.StatusNotFound {
		t.Fatalf("already-unlinked status = %d, want %d", status, http.StatusNotFound)
	}

	if neverExisted.Error != wrongUser.Error || wrongUser.Error != alreadyUnlinked.Error {
		t.Errorf("404 bodies differ across never-existed/wrong-user/already-unlinked: %q vs %q vs %q -- this handler's own doc comment promises NO differential signal",
			neverExisted.Error, wrongUser.Error, alreadyUnlinked.Error)
	}
}

// --- UpdateMemberRole ---

// TestUpdateMemberRole_ValidTransition_Returns200 proves the plain
// success path: a well-formed role string updates the target's role, the
// response carries it, the DB reflects it, and a same-transaction
// audit_log row was written.
func TestUpdateMemberRole_ValidTransition_Returns200(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"role":"maintainer"}`)
	var got memberDTOForTest
	status := rig.doJSON(t, http.MethodPatch, "/api/members/"+target.ID.String()+"/role", body, &got, adminToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Role != "maintainer" {
		t.Errorf("Role = %q, want %q", got.Role, "maintainer")
	}

	updated, err := rig.users.GetByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("get updated user: %v", err)
	}
	if updated.Role != sqlcgen.UserRoleMaintainer {
		t.Errorf("db role = %q, want %q", updated.Role, sqlcgen.UserRoleMaintainer)
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'member.role_changed' AND actor_user_id = $1`, admin.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count audit log rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit log rows = %d, want 1", count)
	}

	// Audit-fix batch addition (M9, completeness): decode/assert the
	// detail_json shape members.go's own UpdateMemberRole writes
	// (from_role/to_role) -- the row's own existence was already proven
	// above. target was seeded as UserRoleMember (createUserWithRole) and
	// this test's own request body above changed it to "maintainer".
	detail := auditLogDetailFor(ctx, t, rig, "member.role_changed", target.ID.String())
	if detail["from_role"] != string(sqlcgen.UserRoleMember) {
		t.Errorf("detail_json[from_role] = %v, want %q", detail["from_role"], sqlcgen.UserRoleMember)
	}
	if detail["to_role"] != string(sqlcgen.UserRoleMaintainer) {
		t.Errorf("detail_json[to_role] = %v, want %q", detail["to_role"], sqlcgen.UserRoleMaintainer)
	}
}

// TestUpdateMemberRole_ResponseIncludesIdentities proves this batch's own
// fix for an audit finding (HIGH, wire-contract): UpdateMemberRole's own
// response actually populates identities with the target's real
// currently-linked identities -- never null, and not merely
// createUserWithRole's own single seeded identity, but every identity
// force-linked onto the target afterward too. Before this fix, the
// response left identities nil (`"identities":null` on the wire), which
// only became a formal contract violation once this same batch's own
// /contracts migration made Member.identities schema-required and
// non-nullable -- a frontend trusting that contract and calling
// response.identities.map(...) after a role change would otherwise crash.
func TestUpdateMemberRole_ResponseIncludesIdentities(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	// Force-link a SECOND identity onto target first, so the assertion
	// below can't be satisfied merely by createUserWithRole's own default
	// seeded GitHub identity alone.
	linkBody := []byte(`{"provider":"slack","externalId":"role-change-probe"}`)
	status := rig.doJSON(t, http.MethodPost, "/api/members/"+target.ID.String()+"/identities", linkBody, nil, adminToken)
	if status != http.StatusCreated {
		t.Fatalf("seed second identity status = %d, want %d", status, http.StatusCreated)
	}

	body := []byte(`{"role":"maintainer"}`)
	var got memberDTOForTest
	status = rig.doJSON(t, http.MethodPatch, "/api/members/"+target.ID.String()+"/role", body, &got, adminToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	if got.Identities == nil {
		t.Fatal("Identities = nil, want a non-nil slice (Member.identities is schema-required and non-nullable)")
	}
	if len(got.Identities) != 2 {
		t.Fatalf("Identities = %d, want 2 (createUserWithRole's own seeded GitHub identity + the Slack one force-linked above)", len(got.Identities))
	}
	var sawSlack bool
	for _, i := range got.Identities {
		if i.Provider == "slack" && i.ExternalID == "role-change-probe" {
			sawSlack = true
		}
	}
	if !sawSlack {
		t.Errorf("Identities = %+v, want to include the force-linked slack identity", got.Identities)
	}
}

// TestUpdateMemberRole_InvalidRoleString_Returns400 proves an
// unrecognized role string is rejected outright, never silently ignored
// or defaulted, and never touches the target's own row.
func TestUpdateMemberRole_InvalidRoleString_Returns400(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"role":"superadmin"}`)
	status := rig.doJSON(t, http.MethodPatch, "/api/members/"+target.ID.String()+"/role", body, nil, adminToken)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}

	unchanged, err := rig.users.GetByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if unchanged.Role != sqlcgen.UserRoleMember {
		t.Errorf("role changed to %q despite a 400, want unchanged %q", unchanged.Role, sqlcgen.UserRoleMember)
	}
}

// TestUpdateMemberRole_DemoteLastAdmin_Returns409 proves this batch's own
// H8 last-admin guard: with exactly one active admin in the whole
// deployment, that admin demoting THEMSELVES (the "self-demotion" half of
// the finding) is refused with 409, and their role is left unchanged --
// never a silent lockout.
func TestUpdateMemberRole_DemoteLastAdmin_Returns409(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := []byte(`{"role":"member"}`)
	var errBody errorResponseForTest
	status := rig.doJSON(t, http.MethodPatch, "/api/members/"+admin.ID.String()+"/role", body, &errBody, adminToken)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d", status, http.StatusConflict)
	}
	if errBody.Error == "" {
		t.Error("expected a non-empty error message")
	}

	unchanged, err := rig.users.GetByID(ctx, admin.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if unchanged.Role != sqlcgen.UserRoleAdmin {
		t.Errorf("db role = %q, want unchanged %q (a 409 must never change role)", unchanged.Role, sqlcgen.UserRoleAdmin)
	}
}

// TestUpdateMemberRole_DemoteNonLastAdmin_Succeeds proves the guard's OTHER
// half: demoting an admin is fine as long as at least one OTHER active
// admin remains.
func TestUpdateMemberRole_DemoteNonLastAdmin_Succeeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, callerToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	otherAdmin, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := []byte(`{"role":"maintainer"}`)
	status := rig.doJSON(t, http.MethodPatch, "/api/members/"+otherAdmin.ID.String()+"/role", body, nil, callerToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (demoting a non-last admin must succeed)", status, http.StatusOK)
	}

	updated, err := rig.users.GetByID(ctx, otherAdmin.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if updated.Role != sqlcgen.UserRoleMaintainer {
		t.Errorf("db role = %q, want %q", updated.Role, sqlcgen.UserRoleMaintainer)
	}
}

// TestUpdateMemberRole_ConcurrentDemoteBothLastAdmins_ExactlyOneSucceeds is
// this batch's own proof that the last-admin guard is race-safe, not just
// correct sequentially: with exactly two active admins, both self-demoting
// AT THE SAME TIME must resolve to exactly one 200 (whichever wins the
// race) and one 409 (the loser, who is now the sole remaining admin) --
// never both succeeding, which would leave zero admins and permanently
// lock the deployment out of every admin-only endpoint, the exact failure
// mode this guard exists to prevent. Mirrors
// TestLinkMemberIdentity_ConcurrentDuplicateLink_ExactlyOneSucceeds's own
// errgroup-based pattern above.
func TestUpdateMemberRole_ConcurrentDemoteBothLastAdmins_ExactlyOneSucceeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	adminA, tokenA := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	adminB, tokenB := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	body := []byte(`{"role":"member"}`)
	pathA := "/api/members/" + adminA.ID.String() + "/role"
	pathB := "/api/members/" + adminB.ID.String() + "/role"

	var eg errgroup.Group
	statuses := make(chan int, 2)
	eg.Go(func() error {
		statuses <- rig.doJSON(t, http.MethodPatch, pathA, body, nil, tokenA)
		return nil
	})
	eg.Go(func() error {
		statuses <- rig.doJSON(t, http.MethodPatch, pathB, body, nil, tokenB)
		return nil
	})
	if err := eg.Wait(); err != nil {
		t.Fatalf("errgroup.Wait: %v", err)
	}
	close(statuses)

	var ok, conflict int
	for s := range statuses {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d among concurrent demote responses (must never be a raw 500, and never both 200)", s)
		}
	}
	if ok != 1 {
		t.Errorf("ok (200) = %d, want exactly 1", ok)
	}
	if conflict != 1 {
		t.Errorf("conflict (409) = %d, want exactly 1", conflict)
	}

	var activeAdmins int
	if err := rig.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin' AND disabled = false`,
	).Scan(&activeAdmins); err != nil {
		t.Fatalf("count active admins: %v", err)
	}
	if activeAdmins != 1 {
		t.Errorf("active admins = %d, want exactly 1 (never zero)", activeAdmins)
	}
}

// --- ListMembers / ListAuditLog: happy-path shape coverage ---

// TestListMembers_HappyPath proves an admin sees every member (with
// role/disabled/linked identities) plus any still-pending link prompt.
func TestListMembers_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	member, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	if _, err := rig.linkPrompts.Create(ctx, sqlcgen.CreateIdentityLinkPromptParams{
		Provider:   sqlcgen.IdentityProviderSlack,
		ExternalID: "pending-slack-id",
		NonceHash:  "test-nonce-hash",
		ExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("seed link prompt: %v", err)
	}

	var got listMembersResponseForTest
	status := rig.doJSON(t, http.MethodGet, "/api/members", nil, &got, adminToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	found := false
	for _, m := range got.Members {
		if m.ID == member.ID.String() {
			found = true
			if m.Role != "member" {
				t.Errorf("member role = %q, want %q", m.Role, "member")
			}
			if m.Disabled {
				t.Error("member Disabled = true, want false")
			}
		}
	}
	if !found {
		t.Error("seeded member not found in ListMembers response")
	}

	if len(got.PendingLinkPrompts) != 1 {
		t.Fatalf("PendingLinkPrompts = %d, want 1", len(got.PendingLinkPrompts))
	}
	if got.PendingLinkPrompts[0].ExternalID != "pending-slack-id" {
		t.Errorf("pending prompt externalId = %q, want %q", got.PendingLinkPrompts[0].ExternalID, "pending-slack-id")
	}
}

// TestListAuditLog_HappyPath proves an admin can read back a real
// audit_log row written by another one of this same handler set's own
// state changes.
func TestListAuditLog_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	body := []byte(`{"role":"maintainer"}`)
	status := rig.doJSON(t, http.MethodPatch, "/api/members/"+target.ID.String()+"/role", body, nil, adminToken)
	if status != http.StatusOK {
		t.Fatalf("seed role change status = %d, want %d", status, http.StatusOK)
	}

	var got listAuditLogResponseForTest
	status = rig.doJSON(t, http.MethodGet, "/api/audit-log", nil, &got, adminToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	found := false
	for _, e := range got.Entries {
		if e.Action == "member.role_changed" && e.ResourceID == target.ID.String() {
			found = true
		}
	}
	if !found {
		t.Error("expected the seeded member.role_changed entry in ListAuditLog's response")
	}
}

// TestListAuditLog_OneMalformedDetailJSON_DegradesGracefully seeds one
// audit_log row via a raw SQL insert (bypassing auditlog.Record entirely
// -- that helper's own signature only ever accepts a map[string]any, so
// it could never itself produce this shape) whose detail_json is
// well-formed JSON but NOT an object (a shape no CHECK constraint on the
// column rules out for some bad/legacy row) and proves the page still
// returns 200 with every OTHER row intact, and the malformed row itself
// present with its own detail degraded to {} -- rather than a raw 500
// that takes out this entire page for every admin over ONE bad row (an
// audit finding: LOW).
func TestListAuditLog_OneMalformedDetailJSON_DegradesGracefully(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	target, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	// A real, well-formed row via the normal role-change path, so there's
	// something to prove survives alongside the malformed one below.
	body := []byte(`{"role":"maintainer"}`)
	status := rig.doJSON(t, http.MethodPatch, "/api/members/"+target.ID.String()+"/role", body, nil, adminToken)
	if status != http.StatusOK {
		t.Fatalf("seed well-formed row status = %d, want %d", status, http.StatusOK)
	}

	// Raw SQL insert, deliberately bypassing auditlog.Record: detail_json
	// is a JSON array, not an object.
	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO audit_log (actor_user_id, action, resource_type, resource_id, detail_json, correlation_id)
		 VALUES ($1, 'test.malformed_detail', 'test', $2, '[1,2,3]'::jsonb, NULL)`,
		admin.ID, target.ID.String(),
	); err != nil {
		t.Fatalf("seed malformed-detail row: %v", err)
	}

	var got listAuditLogResponseForTest
	status = rig.doJSON(t, http.MethodGet, "/api/audit-log", nil, &got, adminToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (one malformed row must never 500 the whole page)", status, http.StatusOK)
	}

	var sawGoodRow, sawMalformedRow bool
	for _, e := range got.Entries {
		if e.Action == "member.role_changed" && e.ResourceID == target.ID.String() {
			sawGoodRow = true
		}
		if e.Action == "test.malformed_detail" {
			sawMalformedRow = true
			if len(e.Detail) != 0 {
				t.Errorf("malformed row's own detail = %+v, want substituted {} (empty)", e.Detail)
			}
		}
	}
	if !sawGoodRow {
		t.Error("expected the well-formed member.role_changed row to still be present")
	}
	if !sawMalformedRow {
		t.Error("expected the malformed-detail row itself to still be present (degraded, not dropped)")
	}
}

// TestListAuditLog_NullDetailJSON_DegradesGracefully covers the one shape
// TestListAuditLog_OneMalformedDetailJSON_DegradesGracefully's own probe
// does NOT catch on its own: a top-level JSON `null`. encoding/json treats
// unmarshaling `null` into a map as a no-op success (err == nil, dest left
// nil), not a type mismatch the way an array/number/string/bool detail_json
// is -- so the isolation check must also treat a nil decoded map as
// malformed, or this row's own detail would ship as the literal `null`,
// violating dtos.schema.json's non-nullable AuditLogEntry.detail contract
// exactly the way Member.identities used to.
func TestListAuditLog_NullDetailJSON_DegradesGracefully(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	admin, adminToken := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)

	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO audit_log (actor_user_id, action, resource_type, resource_id, detail_json, correlation_id)
		 VALUES ($1, 'test.null_detail', 'test', $2, 'null'::jsonb, NULL)`,
		admin.ID, admin.ID.String(),
	); err != nil {
		t.Fatalf("seed null-detail row: %v", err)
	}

	var got listAuditLogResponseForTest
	status := rig.doJSON(t, http.MethodGet, "/api/audit-log", nil, &got, adminToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (a null detail_json must never 500 the whole page)", status, http.StatusOK)
	}

	var sawRow bool
	for _, e := range got.Entries {
		if e.Action == "test.null_detail" {
			sawRow = true
			if len(e.Detail) != 0 {
				t.Errorf("null-detail row's own detail = %+v, want substituted {} (empty), never the literal null", e.Detail)
			}
		}
	}
	if !sawRow {
		t.Error("expected the null-detail row itself to still be present (degraded, not dropped)")
	}
}

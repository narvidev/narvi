//go:build integration

package httpapi_test

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestReviewRequestAuthorizer_AsksEachLanesOwnCheckAgain pins the
// role-based ports.ReviewRequestAuthorizer (technical plan §24.9, §13.3):
// before a person's owed review request is re-run, the check its lane
// applied when the request was made is asked again, of the account as it
// stands now. The button and the label are "re-trigger reviews" -- admin
// or maintainer, with no member carve-out, so a member who created or
// joined the session is denied as any other member is. An unknown
// requester, a disabled account and a lane that owes no request (the
// automatic one; a mention records none) are denied, a maintainer's too;
// a requester id that does not parse is an error, never a denial.
func TestReviewRequestAuthorizer_AsksEachLanesOwnCheckAgain(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	maintainer, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	admin, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleAdmin)
	disabled, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	owner, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	joined, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	stranger, _ := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/owed-authorizer", 91)
	if _, err := rig.participants.Create(ctx, session.ID, joined.ID); err != nil {
		t.Fatalf("join the session: %v", err)
	}
	authorizer := httpapi.NewReviewRequestAuthorizer(rig.users)

	for _, tc := range []struct {
		name      string
		requester string
		trigger   string
		allowed   bool
		wantErr   bool
	}{
		{name: "a maintainer's button request", requester: maintainer.ID.String(), trigger: turn.RequestTriggerButton, allowed: true},
		{name: "a maintainer's label request", requester: maintainer.ID.String(), trigger: turn.RequestTriggerLabel, allowed: true},
		{name: "an admin's label request", requester: admin.ID.String(), trigger: turn.RequestTriggerLabel, allowed: true},
		{name: "a member's button request, on a session they created", requester: owner.ID.String(), trigger: turn.RequestTriggerButton},
		{name: "a member's label request, on a session they created", requester: owner.ID.String(), trigger: turn.RequestTriggerLabel},
		{name: "a member's label request, on a session they joined", requester: joined.ID.String(), trigger: turn.RequestTriggerLabel},
		{name: "a member's label request, on a session not theirs", requester: stranger.ID.String(), trigger: turn.RequestTriggerLabel},
		{name: "a disabled maintainer's button request", requester: disabled.ID.String(), trigger: turn.RequestTriggerButton},
		{name: "a request whose requester is no longer known", requester: "", trigger: turn.RequestTriggerButton},
		{name: "the automatic lane, which owes no request", requester: maintainer.ID.String(), trigger: turn.RequestTriggerAuto},
		{name: "a maintainer's mention, which records no lane", requester: maintainer.ID.String(), trigger: "mention"},
		{name: "a requester id that does not parse", requester: "not-a-user-id", trigger: turn.RequestTriggerButton, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authorizer.AuthorizeReviewRequest(ctx, ports.ReviewRequest{
				SessionID: session.ID.String(), RequestedBy: tc.requester, Trigger: tc.trigger, RepoFullName: "acme/owed-authorizer",
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("AuthorizeReviewRequest error = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.allowed {
				t.Fatalf("AuthorizeReviewRequest = %v, want %v", got, tc.allowed)
			}
		})
	}
}

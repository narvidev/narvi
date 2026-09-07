package authz_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/authz"
)

// TestAuthorizeRepo_KnownAdmitsRegardlessOfRole proves §31.4's own minimal
// scope directly: this predicate is NOT a per-repo-roles RBAC rework --
// admission.Known is the ONLY admitting fact, so every role (including
// viewer, which cannot even create a session at all per the SEPARATE
// ActionCreateSession gate) receives the IDENTICAL verdict for the same
// known repo.
//
// Mutation anchor: hard-coding AuthorizeRepo to always deny (e.g.
// `return &RepoForbiddenError{...}` unconditionally) fails every case here.
func TestAuthorizeRepo_KnownAdmitsRegardlessOfRole(t *testing.T) {
	t.Parallel()

	for _, role := range authz.AllRoles {
		role := role
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			actor := authz.Actor{UserID: "u1", Role: role}
			admission := authz.RepoAdmission{FullName: "acme/widgets", Known: true}
			if err := authz.AuthorizeRepo(actor, admission); err != nil {
				t.Errorf("AuthorizeRepo(%s, known) = %v, want nil", role, err)
			}
		})
	}
}

// TestAuthorizeRepo_UnknownDeniesRegardlessOfRole is
// TestAuthorizeRepo_KnownAdmitsRegardlessOfRole's own mirror image: a repo
// this deployment does not know about is refused for EVERY role, including
// admin -- entitlement is fail-closed on the repo fact alone, never
// escalatable by role.
//
// Mutation anchor: changing `if admission.Known` to `if !admission.Known`
// (inverting the polarity) makes this test fail AND flips
// TestAuthorizeRepo_KnownAdmitsRegardlessOfRole from pass to fail --
// together the two pin the exact polarity.
func TestAuthorizeRepo_UnknownDeniesRegardlessOfRole(t *testing.T) {
	t.Parallel()

	for _, role := range authz.AllRoles {
		role := role
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			actor := authz.Actor{UserID: "u1", Role: role}
			admission := authz.RepoAdmission{FullName: "acme/widgets", Known: false}
			err := authz.AuthorizeRepo(actor, admission)
			if err == nil {
				t.Fatalf("AuthorizeRepo(%s, unknown) = nil, want a forbidden error", role)
			}
			if !errors.Is(err, authz.ErrRepoForbidden) {
				t.Errorf("errors.Is(err, ErrRepoForbidden) = false, want true (err=%v)", err)
			}
			var forbidden *authz.RepoForbiddenError
			if !errors.As(err, &forbidden) {
				t.Fatalf("errors.As(err, *RepoForbiddenError) = false, want true (err=%v)", err)
			}
			if forbidden.RepoFullName != "acme/widgets" {
				t.Errorf("forbidden.RepoFullName = %q, want %q", forbidden.RepoFullName, "acme/widgets")
			}
			if forbidden.Actor != actor {
				t.Errorf("forbidden.Actor = %+v, want %+v", forbidden.Actor, actor)
			}
		})
	}
}

// TestAuthorizeRepo_ZeroValueAdmissionDenies proves the fail-closed default
// directly on the zero value: a caller that forgets to set Known (e.g. a
// bug that never actually resolves the fact before calling this function)
// gets Known == false, Go's own zero value for bool -- denied, never
// silently admitted. Mirrors authz.Resource{}'s own "zero value is always
// the safe thing to pass" precedent, but in the OPPOSITE direction here:
// Resource{}.OwnedOrJoined == false is safe because every action's own
// allow set is still checked; RepoAdmission{}.Known == false is safe
// because it is the ONLY fact this predicate checks at all.
func TestAuthorizeRepo_ZeroValueAdmissionDenies(t *testing.T) {
	t.Parallel()

	err := authz.AuthorizeRepo(authz.Actor{}, authz.RepoAdmission{})
	if err == nil {
		t.Fatal("AuthorizeRepo(zero actor, zero admission) = nil, want a forbidden error")
	}
	if !errors.Is(err, authz.ErrRepoForbidden) {
		t.Errorf("errors.Is(err, ErrRepoForbidden) = false, want true (err=%v)", err)
	}
}

// TestRepoForbiddenError_ErrorStringNamesRepoAndActor proves the error
// string carries enough to attribute a denial in a log line without a
// second lookup -- mirrors ForbiddenError's own identical "actor+action in
// the string" precedent (authorize_test.go covers that sibling type).
func TestRepoForbiddenError_ErrorStringNamesRepoAndActor(t *testing.T) {
	t.Parallel()

	err := authz.AuthorizeRepo(authz.Actor{UserID: "u42", Role: authz.RoleMember}, authz.RepoAdmission{FullName: "acme/widgets", Known: false})
	if err == nil {
		t.Fatal("AuthorizeRepo = nil, want a forbidden error")
	}
	got := err.Error()
	if !strings.Contains(got, "u42") || !strings.Contains(got, "acme/widgets") {
		t.Errorf("Error() = %q, want it to name both the actor (%q) and the repo (%q)", got, "u42", "acme/widgets")
	}
}

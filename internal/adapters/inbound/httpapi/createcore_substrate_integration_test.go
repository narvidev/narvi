//go:build integration

// This file (createcore_substrate_integration_test.go) proves §27.5's
// own up-front half of the "fail-closed, twice" rule (§27.5/§27.6, brief
// point A): CreateSessionCore refuses a docker-required or
// enforced-egress session outright, BEFORE any Postgres write, when the
// configured provider's own Capabilities() does not support it.
//
// Deliberately in package httpapi (not httpapi_test), matching
// createcore_integration_test.go's own precedent exactly (see that
// file's own top doc comment for why) -- checkSubstrateCapabilitiesUpFront
// is unexported, but every test here calls it only indirectly, through
// the real, exported CreateSessionCore entry point.
package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeSubstrateProvider is a minimal, test-only ports.SandboxProvider
// whose ONLY interesting behavior is a caller-configured Capabilities()
// return value -- every other method is never called by any test in this
// file (CreateSessionCore's own up-front check consults Capabilities()
// alone, never CreateSandbox) and panics if it ever is, so a test that
// accidentally reaches the real dispatch path fails loudly rather than
// silently succeeding for the wrong reason.
type fakeSubstrateProvider struct {
	caps ports.Capabilities
}

var _ ports.SandboxProvider = (*fakeSubstrateProvider)(nil)

func (f *fakeSubstrateProvider) Capabilities() ports.Capabilities { return f.caps }
func (f *fakeSubstrateProvider) CreateSandbox(context.Context, ports.CreateSpec) (ports.SandboxRef, error) {
	panic("fakeSubstrateProvider: CreateSandbox unexpectedly called -- the up-front check should have refused before dispatch ever ran")
}
func (f *fakeSubstrateProvider) StopSandbox(context.Context, ports.SandboxRef) error {
	panic("fakeSubstrateProvider: StopSandbox unexpectedly called")
}
func (f *fakeSubstrateProvider) ResumeSandbox(context.Context, ports.SandboxRef) error {
	panic("fakeSubstrateProvider: ResumeSandbox unexpectedly called")
}
func (f *fakeSubstrateProvider) TakeSnapshot(context.Context, ports.SandboxRef) (ports.SnapshotID, error) {
	panic("fakeSubstrateProvider: TakeSnapshot unexpectedly called")
}
func (f *fakeSubstrateProvider) RestoreFromSnapshot(context.Context, ports.SnapshotID, ports.CreateSpec) (ports.SandboxRef, error) {
	panic("fakeSubstrateProvider: RestoreFromSnapshot unexpectedly called")
}
func (f *fakeSubstrateProvider) BuildImage(context.Context, ports.ImageSpec) (ports.BuildOutcome, error) {
	panic("fakeSubstrateProvider: BuildImage unexpectedly called")
}
func (f *fakeSubstrateProvider) DeleteImage(context.Context, ports.ImageRef) error {
	panic("fakeSubstrateProvider: DeleteImage unexpectedly called")
}
func (f *fakeSubstrateProvider) List(context.Context) ([]ports.SandboxRef, error) {
	panic("fakeSubstrateProvider: List unexpectedly called")
}

// newSubstrateTestRegistry builds a *sessionactor.Registry wired to
// provider, for this file's own tests only.
func newSubstrateTestRegistry(t *testing.T, ctx context.Context, provider ports.SandboxProvider) *sessionactor.Registry {
	t.Helper()
	pool := newCoreTestPool(t)
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	return registry
}

// dockerRequestFixture is the one repos+spawnSource shape every test in
// this file reuses, varying only req.Docker/req.EgressPolicy.
func dockerRequestFixture() restdtos.CreateSessionRequest {
	return restdtos.CreateSessionRequest{
		SpawnSource: restdtos.CreateSessionRequestSpawnSourceGithub,
		Repos: []restdtos.CreateSessionRequestReposElem{
			{Name: "narvi", Url: "https://github.com/narvidev/narvi"},
		},
	}
}

// TestCreateSessionCore_RefusesDockerRequiredSessionWhenProviderUnsupported
// is a MUTATION-TESTABLE guard (§27.5 brief: "remove the up-front
// session-creation refusal → a named test must fail"): a request with
// docker=true against a provider reporting DockerInSandbox=false is
// refused with 422, and -- critically -- NO environments row and NO
// sessions row is created at all: this check runs before ANY Postgres
// write, so it can never be confused with (or accidentally rely on) the
// dispatch-time re-check ever running. This is the up-front half of "a
// test that disables one and shows the other still refuses, in both
// directions": dispatch is never even reached here, proving the up-front
// check alone is sufficient.
func TestCreateSessionCore_RefusesDockerRequiredSessionWhenProviderUnsupported(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	provider := &fakeSubstrateProvider{caps: ports.Capabilities{DockerInSandbox: false}}
	registry := newSubstrateTestRegistry(t, ctx, provider)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	req := dockerRequestFixture()
	req.Docker = true

	var nilCreator pgtype.UUID
	_, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionCore: got nil error, want a refusal for docker=true against a DockerInSandbox=false provider")
	}
	if cerr.Status != http.StatusUnprocessableEntity {
		t.Errorf("cerr.Status = %d, want %d; message=%q", cerr.Status, http.StatusUnprocessableEntity, cerr.Message)
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("sessions count = %d, want 0 -- the up-front refusal must run BEFORE any Postgres write", sessionCount)
	}
	var envCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM environments WHERE docker_required = true`).Scan(&envCount); err != nil {
		t.Fatalf("count environments: %v", err)
	}
	if envCount != 0 {
		t.Errorf("docker_required environments count = %d, want 0 -- the up-front refusal must run BEFORE any Postgres write", envCount)
	}
}

// TestCreateSessionCore_AllowsDockerRequiredSessionWhenProviderSupported
// is the refusal test's own positive control: the identical request
// against a provider reporting DockerInSandbox=true succeeds and
// actually persists a docker_required=true environments row -- proving
// the up-front check is a real gate, not something that happens to
// refuse every request regardless of provider support.
func TestCreateSessionCore_AllowsDockerRequiredSessionWhenProviderSupported(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	provider := &fakeSubstrateProvider{caps: ports.Capabilities{DockerInSandbox: true}}
	registry := newSubstrateTestRegistry(t, ctx, provider)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	req := dockerRequestFixture()
	req.Docker = true

	var nilCreator pgtype.UUID
	created, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionCore: status=%d message=%q", cerr.Status, cerr.Message)
	}

	if !created.EnvironmentID.Valid {
		t.Fatal("created.EnvironmentID is not valid -- docker=true must create a session-scoped Environment")
	}
	env, err := environments.Get(ctx, created.EnvironmentID)
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if !env.DockerRequired {
		t.Error("environments.docker_required = false, want true")
	}
}

// TestCreateSessionCore_RefusesEgressAllowlistSessionWhenProviderUnsupported
// mirrors the Docker test above for §27.6's own egress half: an
// allowlist-mode egressPolicy against a provider reporting
// EgressPolicy=false is refused up front, before any Postgres write.
func TestCreateSessionCore_RefusesEgressAllowlistSessionWhenProviderUnsupported(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	provider := &fakeSubstrateProvider{caps: ports.Capabilities{EgressPolicy: false}}
	registry := newSubstrateTestRegistry(t, ctx, provider)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	req := dockerRequestFixture()
	req.EgressPolicy = &restdtos.CreateSessionRequestEgressPolicy{
		Mode:      restdtos.CreateSessionRequestEgressPolicyModeAllowlist,
		Allowlist: []string{"registry.npmjs.org"},
	}

	var nilCreator pgtype.UUID
	_, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr == nil {
		t.Fatal("CreateSessionCore: got nil error, want a refusal for an allowlist egressPolicy against an EgressPolicy=false provider")
	}
	if cerr.Status != http.StatusUnprocessableEntity {
		t.Errorf("cerr.Status = %d, want %d; message=%q", cerr.Status, http.StatusUnprocessableEntity, cerr.Message)
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("sessions count = %d, want 0", sessionCount)
	}
}

// TestCreateSessionCore_AllowsEgressOpenModeRegardlessOfProviderSupport
// proves EgressPolicy.RequiresEnforcement()'s own contract holds
// end-to-end through the real HTTP-layer check: an "open" mode policy
// never needs provider support at all (every provider already defaults
// to unrestricted egress), so this succeeds even though the configured
// provider reports EgressPolicy=false.
func TestCreateSessionCore_AllowsEgressOpenModeRegardlessOfProviderSupport(t *testing.T) {
	ctx := context.Background()
	pool := newCoreTestPool(t)
	provider := &fakeSubstrateProvider{caps: ports.Capabilities{EgressPolicy: false}}
	registry := newSubstrateTestRegistry(t, ctx, provider)

	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	environments := narvipg.NewEnvironmentStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)

	req := dockerRequestFixture()
	req.EgressPolicy = &restdtos.CreateSessionRequestEgressPolicy{
		Mode:      restdtos.CreateSessionRequestEgressPolicyModeOpen,
		Allowlist: []string{},
	}

	var nilCreator pgtype.UUID
	created, cerr := CreateSessionCore(ctx, pool, sessions, turns, environments, auditLog, registry, req, nilCreator, false, platform.RolloutModeOpen, repoSettings, prSessions)
	if cerr != nil {
		t.Fatalf("CreateSessionCore: status=%d message=%q, want success for an open-mode egressPolicy regardless of provider support", cerr.Status, cerr.Message)
	}
	if !created.EnvironmentID.Valid {
		t.Fatal("created.EnvironmentID is not valid -- a present egressPolicy key must create a session-scoped Environment")
	}
}

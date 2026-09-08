//go:build integration

// This file is the pure-move refactor's proof obligation: it builds the
// REAL composition root (Build) against a real, migrated Postgres and
// asserts App.Routes() -- the live chi router's own route table -- equals
// testdata/routes.golden exactly. That golden was captured, via
// internal/ops.ScanRegisteredRoutes, from cmd/control-plane's OWN wiring
// BEFORE this package existed at all (see this PR's own commit message
// for exactly how and when); if this test still passes after the move,
// the move changed no route.
//
// newTestPool below mirrors test/resilience/harness_test.go's own
// newHarness and internal/adapters/inbound/httpapi's own newTestPool
// (container-start-plus-migrate) exactly -- this package builds its own
// copy rather than importing either of theirs, per this repo's own
// established "each DB-touching test package owns a small copy of this
// helper" precedent (see either file's own doc comment for the same
// reasoning). setRequiredEnv/testGitHubAppPrivateKeyPEM below are an
// identical copy of internal/platform/config_test.go's own fixtures of the
// same names, for the same reason: this is the first package outside
// internal/platform itself that needs a platform.Load()-valid Config
// (Build's own signature takes *platform.Config directly, and constructing
// one field-by-field would either duplicate platform.Load()'s own
// defaulting/parsing logic or risk drifting from it -- going through Load
// itself, exactly like production boot does, avoids both).
package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver, used only for the migrate handle below
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/narvidev/narvi/extension"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/integrations"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/migrations"
)

// setRequiredEnv sets every env var platform.Load requires to succeed to a
// valid dummy value, for the duration of the calling test, via t.Setenv --
// an exact copy of internal/platform/config_test.go's own helper of the
// same name (see this file's own top doc comment for why this package
// keeps its own copy rather than importing that one). NARVI_DATABASE_URL
// is overwritten by the caller immediately afterward to point at this
// test's own real container instead of this fixture's placeholder value.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NARVI_STAGE", "development")
	t.Setenv("NARVI_DATABASE_URL", "postgres://narvi:narvi@localhost:5432/narvi_test?sslmode=disable")
	t.Setenv("NARVI_HMAC_SANDBOX_SECRET", "test-sandbox-secret")
	t.Setenv("NARVI_HMAC_BOTS_SECRET", "test-bots-secret")
	t.Setenv("NARVI_HMAC_WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("NARVI_GITHUB_CLIENT_ID", "test-github-client-id")
	t.Setenv("NARVI_GITHUB_CLIENT_SECRET", "test-github-client-secret")
	t.Setenv("NARVI_GITHUB_WEBHOOK_SECRET", "test-github-webhook-secret")
	t.Setenv("NARVI_GITHUB_BOT_HANDLE", "test-bot")
	t.Setenv("NARVI_GITHUB_BOT_TOKEN", "test-github-bot-token")
	t.Setenv("NARVI_PUBLIC_BASE_URL", "http://localhost:8080")
	t.Setenv("NARVI_TOKEN_ENCRYPTION_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=") // base64 of exactly 32 bytes
	t.Setenv("NARVI_ALLOWED_EMAIL_DOMAINS", "example.com")
	t.Setenv("NARVI_ALLOWED_GITHUB_ORGS", "")
	t.Setenv("NARVI_ALLOWED_EMAILS", "")
	t.Setenv("NARVI_INITIAL_ADMIN_EMAILS", "")
	t.Setenv("NARVI_MODAL_BASE_URL", "https://modal.example.test")
	t.Setenv("NARVI_MODAL_AUTH_TOKEN", "test-modal-auth-token")
	t.Setenv("NARVI_MODAL_EGRESS_PROXY_URL", "")
	t.Setenv("NARVI_OPENCODE_RUNTIME_VERSION", "")
	t.Setenv("NARVI_LINEAR_WEBHOOK_SECRET", "test-linear-webhook-secret")
	t.Setenv("NARVI_LINEAR_CLIENT_ID", "test-linear-client-id")
	t.Setenv("NARVI_LINEAR_CLIENT_SECRET", "test-linear-client-secret")
	t.Setenv("NARVI_LINEAR_DEFAULT_REPO_NAME", "narvi")
	t.Setenv("NARVI_LINEAR_DEFAULT_REPO_URL", "https://github.com/narvidev/narvi")
	t.Setenv("NARVI_SLACK_SIGNING_SECRET", "test-slack-signing-secret")
	t.Setenv("NARVI_SLACK_BOT_TOKEN", "test-slack-bot-token")
	t.Setenv("NARVI_ANTHROPIC_API_KEY", "test-anthropic-api-key")
	t.Setenv("NARVI_INTENT_CLASSIFIER_PROVIDER", "anthropic")
	t.Setenv("NARVI_INTENT_CLASSIFIER_MODEL", "claude-haiku-4-5")
	t.Setenv("NARVI_GITHUB_APP_ID", "123456")
	t.Setenv("NARVI_GITHUB_APP_PRIVATE_KEY", testGitHubAppPrivateKeyPEM)
}

// testGitHubAppPrivateKeyPEM is a fixed, test-only 2048-bit RSA private
// key, base64-encoded PEM (PKCS#1, "BEGIN RSA PRIVATE KEY" -- the shape
// GitHub itself issues) -- an exact copy of internal/platform/config_test.
// go's own fixture of the same name, never used against any real GitHub
// App or any other credential in this codebase.
const testGitHubAppPrivateKeyPEM = "LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcEFJQkFBS0NBUUVBMlozYVRnSXR6cE1YaXd0UVZGZW16VlZsd0JYY1E1RUJ1UUNnT281SG1tNnpyV25DCjliR0xUSzF4S1I1eFVqLy9Rck9taXJBb25lZlB3QmhnVmloTXpoL1NYMHpPOUgvWWhiS3F5aVVsRHI3ck0vMlYKMDZhVWxFdEtLcEZwbWVDemNSaUtPaGFTeWt1Q09XYVJzQWpWMzFSUGVVTi9MaVl6VUswTmlhL2piU05BRENYQwp3MzJKUjh0TmE1U1VwOFJ6amJkdWdUd0EvT1l4SXZTNmZSTnYvM0lWUXVBSGZaTFhHTmFhZGt3KzBPem1LY2x3Ck9NQmZZejRYRjdzOHFGOWR5eFhscm84eTNZak1COGcxYXJBcG9IeExyVlZ5S0xmN3h6VWJ3cG1uTW9QS2NKanYKMXN4bG5BMFhWcEwrZk1RN3RoV3dkOUZuSDBpOS8vcCs0c0dmeXdJREFRQUJBb0lCQUNIUngyQ0NOQzQ3YTlnLwpETi9lczF5TDNnRkpKRzhYdFFYVVZCSmxsRGtxNVIrWkpTUmIwRU05WFMyL3ZtckM2VityWGNHRitQbjVVYThQCjJzRHBDRzZzUVZ4d0ttV1RETXBTWnZwOVpWSHlWOGsvcXE0MjREWmZzUW9HaVR2UjBQRk5tQVhKQmswTUNSUDAKbmNXV3llNG9ReVdjV01LS1MwVkpiNllyUUpQd01lYVpwbkxEUGsvUDFhZnZyVkxHMG81SXRZNUxGa1dHaVdTYgpDbCtwOERGbytSWlFmVW1ERzdEY2hPWHIvZTJvd1NEODFIV2N3SXlzUlZxcGxPL3d4M2xuMjJIaUR5dVVxWTN4CmNQS2w5RHZ6c0I1NzZVU09MS0IvSGkvTjNIemc1bCt2V1MzZVZYeXovWlZYRnkrY3NkVkV6eG03QjA4QWlZK1UKRmo3Q3hjRUNnWUVBLzVDMkRWazMwQXUzRElYTnpxalFXUWJYV3BsWG5MNFU1NUJsUExYSWt3QlRHVU4rNFlOMQppQUNOM00wMUkyVnRHQ1B4a2pVbTg1WWxMc2tJYitaNGpyeW9NWk5XQzd1dzNyQ01LYit4aCtmZmdpdWNjSnRMClBMWGNsU203NzFBQ2FBT2FOR0R5RkduK3V3UzM5WDZ6MGJvaXIzc1I2NkFkVGx6TzJVYlhDNnNDZ1lFQTJmeWQKemhaYXRaMGxsYnZrcHdTbnozOEd5aWpnQjI1NmNyQ1dBSFo1dmwrVndvZ2Y0ZnllT0tacTFIalZkVSs3cUJOUgpEcGJDYVlkQWEwTXdMdXZ2Ti9jWVROVkpYUlM2S2ZOL3ZLUXBFQVFSMXN4L29Rd2s5TW9lbjJoSFoxVnBzM0pCClYrdVk3cDE0bVYxR2JIdjM1V3hQeXdKR1FWdlZiMklVWnNaK25HRUNnWUVBNVYwNUpxM0YyNkJIL3FNdjNLUEIKcWNUc0RsSEZRZFdPNld5OGowb080Mi9OSk1WZzRJQ2RRUnhPTmJhdVZFQTVNd3MvU1pzT2hGdGlyNlNaUCtTMgptbFJURjN0R0pHMmxCWmVwaythSkxKSThGSldUWjdUWVIzcG9xQzYyanNkZUFZQUtLNncrVjNmeHVHTTV2c2lpCkZqNVoxdWc3WXg5bWJlZjVkU09RNk5VQ2dZQlBWRGx4aUgwV1hzd1F3OElnYmZkTDhlUmNxYWR0ek96TzFDaWkKbm5zTHB1bHZVKzZXWlVLSFJ6alZmZXZndDFXSmd3NGFpdzdSTEtGcTU1YWZYTWsveXJLVE00TnhWbHV4YktYdAoxcWdDNWhnLzNVZ05LY2hCTlZVVG1mVnlTNGtkL3RSODFJWmhQL2xsaHFaY1VIa1VpdWcyN3VyMldoOUFXNmNsCkI5T0h3UUtCZ1FEekt2YzMvWDZ6NzdMeUFqb1BIZUpIbXQyL2tSWllJQjNmUUlsRkg0R3JoVUg1TXdLNklIeUkKWGxPUU53ZHVwdm5QaXlHS0dYeUwvcHJSVGdxQXpGMUFPNW0xWG8wVlJnMVZTeGp6Y1RPTU0zVGpnYU5GYmlMdwpreUovZjlhdzhrUTU2RFA2OWlzV1BKaVUyQko1blZLUTJPVEJwSHNTa2h5eS94amZaT29zbFE9PQotLS0tLUVORCBSU0EgUFJJVkFURSBLRVktLS0tLQo="

// newTestPool spins up one throwaway Postgres container, applies every
// embedded migration, and returns a ready pool plus that container's own
// connection string (so the caller can point NARVI_DATABASE_URL at it
// before calling platform.Load). t.Cleanup tears down the pool and the
// container. Mirrors test/resilience/harness_test.go's own newHarness
// (same testcontainers image/options, same golang-migrate iofs source) --
// see this file's own top doc comment for why this package builds its own
// copy rather than importing that one.
func newTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("narvi_test"),
		tcpostgres.WithUsername("narvi"),
		tcpostgres.WithPassword("narvi"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrateDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = migrateDB.Close() }()

	dbDriver, err := migratepg.WithInstance(migrateDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migratepg.WithInstance: %v", err)
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		t.Fatalf("migrate.NewWithInstance: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool, connStr
}

// TestBuild_RouteTableMatchesGolden is this PR's own proof obligation --
// see this file's own top doc comment.
func TestBuild_RouteTableMatchesGolden(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	goldenBytes, err := os.ReadFile(filepath.Join("testdata", "routes.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	wantRoutes := strings.Split(strings.TrimRight(string(goldenBytes), "\n"), "\n")
	sort.Strings(wantRoutes)

	gotRoutes := app.Routes()

	wantSet := make(map[string]bool, len(wantRoutes))
	for _, r := range wantRoutes {
		wantSet[r] = true
	}
	gotSet := make(map[string]bool, len(gotRoutes))
	for _, r := range gotRoutes {
		gotSet[r] = true
	}

	var missing, extra []string
	for _, r := range wantRoutes {
		if !gotSet[r] {
			missing = append(missing, r)
		}
	}
	for _, r := range gotRoutes {
		if !wantSet[r] {
			extra = append(extra, r)
		}
	}

	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("App.Routes() does not match testdata/routes.golden.\nmissing (in golden, not in App.Routes()): %s\nextra (in App.Routes(), not in golden): %s",
			formatRouteList(missing), formatRouteList(extra))
	}
}

// formatRouteList renders routes for TestBuild_RouteTableMatchesGolden's
// own failure message -- "(none)" rather than an empty, easy-to-miss line
// when one side of the diff is clean.
func formatRouteList(routes []string) string {
	if len(routes) == 0 {
		return "(none)"
	}
	return "\n  " + strings.Join(routes, "\n  ")
}

// TestBuild_IngressDisabled_RoutesUnmounted proves §12.5's own "a surface
// not named is not mounted at all -- no route, no webhook endpoint" claim
// against the REAL composition root and the REAL chi route table, not
// merely against a unit-level boolean (httpapi.configuredForProvider's own
// tests already cover that half). Narrowing NARVI_INGRESS_ENABLED to a
// single surface removes every OTHER surface's own webhook/OAuth routes
// from App.Routes() entirely, and a live request to one of the removed
// paths 404s exactly like any other unknown path -- never a handler
// quietly built against an empty secret and left reachable.
func TestBuild_IngressDisabled_RoutesUnmounted(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NARVI_INGRESS_ENABLED", "github")

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	gotRoutes := app.Routes()
	gotSet := make(map[string]bool, len(gotRoutes))
	for _, r := range gotRoutes {
		gotSet[r] = true
	}

	// Slack/Linear routes must be entirely ABSENT from the live route
	// table -- not merely unreachable behind some other gate.
	for _, disabledRoute := range []string{
		"POST /webhooks/slack",
		"POST /webhooks/slack/interactive",
		"GET /auth/linear/install",
		"GET /auth/linear/callback",
		"POST /webhooks/linear",
	} {
		if gotSet[disabledRoute] {
			t.Errorf("App.Routes() contains %q, want absent (NARVI_INGRESS_ENABLED=github disables Slack/Linear entirely)", disabledRoute)
		}
	}
	// GitHub's own webhook route must still be present -- only Slack/Linear
	// were disabled, proving this isn't an accidental "everything got
	// unmounted" failure mode.
	if !gotSet["POST /webhooks/github"] {
		t.Error(`App.Routes() does not contain "POST /webhooks/github", want present (GitHub is the one enabled surface)`)
	}

	// A live request to a disabled surface's own path 404s exactly like
	// any other unknown path -- proving the absence end to end against a
	// real http.Handler, not just against the route-table listing above.
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/webhooks/slack"},
		{http.MethodPost, "/webhooks/slack/interactive"},
		{http.MethodPost, "/webhooks/linear"},
		{http.MethodGet, "/auth/linear/install"},
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(req.method, req.path, nil)
		app.Router.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want %d (surface disabled, route must not exist)", req.method, req.path, rec.Code, http.StatusNotFound)
		}
	}
}

// TestBuild_GitHubIngressDisabled_NoGitHubPreviewLinkNotifier proves the
// fix for the leak TestBuild_IngressDisabled_RoutesUnmounted's own route
// table could never see: with GitHub ingress disabled, cfg.GitHubBotToken
// reads empty (gitHubBotTokenEnvVarName's own doc comment, platform/
// config.go), yet the pre-fix outboxNotifiers wiring in Build still
// registered github_preview_link -- githubapi.NewPreviewLinkNotifier
// authenticated with that empty token -- whenever cfg.RWXAccessToken was
// set, because that registration was gated on RWXAccessToken alone, never
// on githubIngressEnabled. Any row enqueued for that kind (ordinary
// steady-state traffic on a preview-enabled repo, not a stale/edge case --
// see sessionactor.enqueuePreviewBestEffort's own call site) would then
// post "Authorization: Bearer" with nothing after it, retry the full
// backoff ladder, dead-letter, and (because the kind's "github" prefix
// feeds the /api/integrations read model) misattribute the failure to
// GitHub as configured=false/lastOutboundStatus=failed.
//
// platform.Load's own RWXPreviewsRequireGitHubIngressError now refuses to
// boot a real deployment in this state at all, so this test builds its
// *platform.Config the same way TestBuild_IngressDisabled_RoutesUnmounted
// does and then mutates it directly, AFTER Load already succeeded on a
// valid combination -- proving Build's own registration gate is a real,
// independent backstop (as its own doc comment in serve.go claims), not
// dead code that merely happens to never execute because Load rejects the
// input first.
//
// Asserts on the registered notifier set itself
// (outboxworker.Builder.HasNotifier), not on the route table --
// TestBuild_IngressDisabled_RoutesUnmounted's own proof is blind to this
// class of defect entirely, since routes and outbox notifiers are two
// separate registrations in Build.
func TestBuild_GitHubIngressDisabled_NoGitHubPreviewLinkNotifier(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	// Mutate cfg AFTER a successful Load, to the exact state
	// RWXPreviewsRequireGitHubIngressError refuses to let a real
	// deployment boot into -- see this test's own top doc comment for why.
	cfg.IngressEnabled = map[integrations.Provider]bool{
		integrations.ProviderSlack:  true,
		integrations.ProviderLinear: true,
		integrations.ProviderGitHub: false,
	}
	cfg.RWXAccessToken = "test-rwx-access-token"
	cfg.GitHubBotToken = "" // what an operator running this combination would actually have.

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if app.outboxBuilder.HasNotifier(ports.NotificationKindGitHubPreviewLink) {
		t.Error("outboxBuilder has a notifier registered for github_preview_link with GitHub ingress disabled -- it would post with an empty GitHubBotToken")
	}
	// rwx_preview_dispatch needs no GitHub credential at all -- it must
	// stay registered on RWXAccessToken alone, proving the fix narrowed
	// the gate rather than disabling the whole RWX-configured block.
	if !app.outboxBuilder.HasNotifier(ports.NotificationKindRWXPreviewDispatch) {
		t.Error("outboxBuilder has no notifier registered for rwx_preview_dispatch, want registered (RWXAccessToken alone is sufficient for this kind)")
	}
}

// TestBuild_EveryAPIRouteCarriesAGuardBeyondTheGlobalChain closes the half
// of the proof routes.golden cannot express.
//
// The golden pins WHICH paths exist. It says nothing about what guards
// them, because App.Routes discards chi.Walk's middleware chain -- so a
// route could keep its path and silently lose auth.Middleware and the
// golden would still match. That is the most dangerous defect this
// package's extraction could have introduced, and the one its own proof
// was blind to.
//
// The property asserted here is relational, not a snapshot: every /api
// route must carry MORE middlewares than the router's own global chain.
// The baseline is read from the live router rather than hardcoded, so
// adding a global middleware raises the bar for every route instead of
// silently lowering it -- a hardcoded "at least 3" would keep passing for
// an unguarded route the day a third global middleware lands.
//
// /api is the whole scope on purpose. The sandbox-facing routes
// (/sessions/{sessionID}/..., /auth/..., /health, /.well-known/...) sit at
// exactly the global baseline because they authenticate inside their own
// handlers -- HMAC and bearer schemes chi middleware never sees -- so a
// middleware-count rule would be meaningless there, and asserting one
// would only teach a future reader to weaken it.
func TestBuild_EveryAPIRouteCarriesAGuardBeyondTheGlobalChain(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mux, ok := app.Router.(*chi.Mux)
	if !ok {
		t.Fatalf("app.Router is %T, not *chi.Mux -- this test reads the global middleware chain off the mux", app.Router)
	}
	baseline := len(mux.Middlewares())
	if baseline == 0 {
		t.Fatal("global middleware chain is empty -- Recoverer and the correlation-id middleware are both expected, so this test would be vacuous")
	}

	var unguarded []string
	apiRoutes := 0
	err = chi.Walk(app.Router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/") {
			return nil
		}
		apiRoutes++
		if len(mws) <= baseline {
			unguarded = append(unguarded, fmt.Sprintf("%s %s (%d middlewares, global chain is %d)", method, route, len(mws), baseline))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	if apiRoutes == 0 {
		t.Fatal("walked zero /api routes -- the prefix changed and this test is now vacuous")
	}
	if len(unguarded) > 0 {
		sort.Strings(unguarded)
		t.Errorf("%d /api route(s) carry nothing beyond the global middleware chain -- an /api route with no auth.Middleware is an unauthenticated endpoint:\n\t%s",
			len(unguarded), strings.Join(unguarded, "\n\t"))
	}
}

// TestBuild_BadKey_BootsAsNarvi is docs/design/boundaries-design.md,
// section 1.6's own named test: a malformed NARVI_LICENSE_KEY yields the
// exact same route table as no key at all, and Build never refuses to
// boot over it (technical plan §34.5: "boot never fails on a bad key --
// a licensing lapse must not become an outage"). Both builds compose
// zero modules, matching the public binary's own shape exactly -- the
// row this proof cares about is "public build, any key" from the
// design note's own section 1.3 table, all four of whose key-state rows
// collapse to the SAME "silent, nothing enabled" behavior.
func TestBuild_BadKey_BootsAsNarvi(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	appNoKey, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build (no key): %v", err)
	}

	cfgBadKey := *cfg
	cfgBadKey.LicenseKey = "not-even-close-to-a-valid-narvi1-license-key"
	appBadKey, err := Build(context.Background(), &cfgBadKey, pool)
	if err != nil {
		t.Fatalf("Build (bad key): %v -- boot must never fail on a bad licence key", err)
	}

	if !slices.Equal(appNoKey.Routes(), appBadKey.Routes()) {
		t.Errorf("route table differs between no key and a bad key:\nno key:  %v\nbad key: %v", appNoKey.Routes(), appBadKey.Routes())
	}
}

// fakeModuleWorker is a trivial extension.Worker used only to prove
// mountModules threads a module's own Workers through to the built App
// without error -- Run itself is never called by this test (no listener,
// no errgroup), so this never actually executes.
type fakeModuleWorker struct{}

func (fakeModuleWorker) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestBuild_WithModules_AddsOnlyExtRoutes is docs/design/
// boundaries-design.md, section 3.5's own named test: composing a module adds
// ONLY /api/ext/<name>/... routes, changes no existing route, and hands
// that module's own Mount a Runtime with every field populated.
func TestBuild_WithModules_AddsOnlyExtRoutes(t *testing.T) {
	setRequiredEnv(t)

	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	baseApp, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build (no modules): %v", err)
	}
	baseRoutes := baseApp.Routes()

	fakeModule := extension.Module{
		Name: "acmetest",
		Mount: func(r chi.Router, rt extension.Runtime) {
			r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			if rt.Capabilities == nil {
				t.Error("extension.Runtime.Capabilities is nil in Mount")
			}
			if rt.RequireAuth == nil {
				t.Error("extension.Runtime.RequireAuth is nil in Mount")
			}
			if rt.RequireCapability == nil {
				t.Error("extension.Runtime.RequireCapability is nil in Mount")
			}
			if rt.Audit == nil {
				t.Error("extension.Runtime.Audit is nil in Mount")
			}
			if rt.Pool == nil {
				t.Error("extension.Runtime.Pool is nil in Mount")
			}
		},
		Workers: func(extension.Runtime) []extension.Worker {
			return []extension.Worker{fakeModuleWorker{}}
		},
	}

	moduleApp, err := Build(context.Background(), cfg, pool, fakeModule)
	if err != nil {
		t.Fatalf("Build (with module): %v", err)
	}
	if len(moduleApp.moduleWorkers) != 1 {
		t.Errorf("moduleApp.moduleWorkers has %d entries, want exactly 1 (the fake module's own Workers)", len(moduleApp.moduleWorkers))
	}
	moduleRoutes := moduleApp.Routes()

	baseSet := make(map[string]bool, len(baseRoutes))
	for _, r := range baseRoutes {
		baseSet[r] = true
	}

	var extRoutes []string
	for _, r := range moduleRoutes {
		if !baseSet[r] {
			extRoutes = append(extRoutes, r)
		}
	}

	if len(extRoutes) == 0 {
		t.Fatal("Build with a composed module added zero new routes")
	}
	for _, r := range extRoutes {
		if !strings.Contains(r, "/api/ext/acmetest") {
			t.Errorf("new route %q is not under /api/ext/acmetest -- a composed module must add ONLY its own /api/ext/<name>/ routes", r)
		}
	}

	// Every route the base (moduleless) app already had must still be
	// present, unchanged, once a module is composed -- a module must add
	// routes, never remove or shadow a public one.
	moduleSet := make(map[string]bool, len(moduleRoutes))
	for _, r := range moduleRoutes {
		moduleSet[r] = true
	}
	for _, r := range baseRoutes {
		if !moduleSet[r] {
			t.Errorf("public route %q (present with no modules composed) disappeared once a module was composed", r)
		}
	}

	// The module's own route must carry MORE middlewares than the
	// router's own global chain -- extension.Module.Mount's own doc
	// comment promises a module route "is already mounted ... behind the
	// same authentication as every other API route", so a module route
	// with nothing beyond the global chain would be reachable
	// unauthenticated, mirroring
	// TestBuild_EveryAPIRouteCarriesAGuardBeyondTheGlobalChain's own
	// reasoning for public routes.
	mux, ok := moduleApp.Router.(*chi.Mux)
	if !ok {
		t.Fatalf("moduleApp.Router is %T, not *chi.Mux", moduleApp.Router)
	}
	baseline := len(mux.Middlewares())
	if baseline == 0 {
		t.Fatal("global middleware chain is empty -- this test would be vacuous")
	}
	foundModuleRoute := false
	err = chi.Walk(moduleApp.Router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.Contains(route, "/api/ext/acmetest") {
			return nil
		}
		foundModuleRoute = true
		if len(mws) <= baseline {
			t.Errorf("module route %s %s carries %d middleware(s), no more than the global chain's own %d -- it must be authenticated by construction", method, route, len(mws), baseline)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if !foundModuleRoute {
		t.Fatal("chi.Walk found no route under /api/ext/acmetest -- this test is vacuous")
	}
}

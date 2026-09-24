.PHONY: build vet fmt tidy lint lint-web-assets test test-integration \
	test-integration-group-1 test-integration-group-2 test-integration-group-3 test-integration-group-4 \
	contracts-generate contracts-check dev \
	web-typecheck web-lint web-check-dto-types web-test web-build web-check dist \
	verify-control-plane-image

build:
	go build ./...

vet:
	go vet ./...

fmt:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt needs to be run on:"; \
		echo "$$files"; \
		exit 1; \
	fi

tidy:
	go mod tidy

lint:
	golangci-lint run ./...
	go run ./tools/lint/narvichecks ./...
	# NOTE: this pass uses the DEFAULT build context, so any file behind a
	# build tag is absent from it and is analyzed by nothing. Exactly one
	# non-test file in this repo is tagged today
	# (internal/adapters/inbound/webui's embed file, //go:build
	# web_assets) and it SHIPS in the release binary -- so it is covered
	# by lint-web-assets below, which `dist` depends on, rather than here:
	# the tag does not compile until `make web-build` has produced the
	# bundle, and `make lint` must keep working in a checkout with no node.

test:
	go test -race ./...

# test-integration runs with -p 1 (serialize package test binaries) --
# deliberately, not for correctness but for HOST RESOURCE CONTENTION: every
# DB-touching package spins up its own throwaway Postgres container via
# testcontainers-go (each package's own newTestPool/newTestPoolPair helper,
# ~20 of them repo-wide). internal/adapters/inbound/httpapi's own
# integration suite -- by far the heaviest single-package container churn
# in the repo, at the time these CI hangs happened -- used to spin one up
# per test function (~170 of them); it now starts exactly ONE for its
# whole test binary instead (see internal/adapters/inbound/httpapi/
# sharedpool_integration_test.go's own top doc comment), which is why
# group 1 below (httpapi alone) no longer needs the same PEAK-load
# reasoning this comment originally documented it for -- it is kept at
# -p 1 anyway simply because -p has no effect on a single package.
# Three separate CI runs (30831633470, 30834918806, 30838285218 -- see
# fix/sse-broadcast-race's own commit history), from BEFORE that change,
# each hung for the full Go 10-minute test-binary panic timeout on a
# DIFFERENT testcontainers-go internal call (ContainerStart, then
# wait.(*LogStrategy).WaitUntilReady, then wait.(*HostPortStrategy).
# WaitUntilReady/Exec) -- three genuinely different code paths, all
# inside the same third-party library's own Docker-daemon-facing
# machinery, all in the SAME package (httpapi). A per-call
# context.WithTimeout and, later, an independent goroutine-plus-watchdog
# race (see sharedpool_integration_test.go's own startSharedTestContainer
# doc comment, formerly httpapi_integration_test.go's own newTestPool)
# were each verified, directly and locally, to correctly bound this exact
# call in isolation -- yet the SAME construct still failed to cut the
# call off in real CI. The common thread across all three is HOST-LEVEL
# contention, not a single fixable call site: by default `go test ./...`
# runs multiple packages' own test binaries concurrently (bounded by
# GOMAXPROCS), each spinning up its own container(s) via the SAME Docker
# daemon on the SAME constrained runner, under -race's own substantial
# CPU/memory overhead on top -- plausibly severe enough, in aggregate, to
# make even independent per-process timers unreliable, not just Docker
# itself slow. -p 1 trades a longer, fully-serialized wall-clock run for
# dramatically lower PEAK concurrent Docker/host load, directly targeting
# that shared root cause rather than another per-call timeout mechanism
# -- still the right default for the OTHER ~19 packages in this target,
# even though httpapi's own contribution to that peak is now much smaller.
test-integration:
	go test -tags=integration -race -p 1 ./...

# test-integration-group-N -- CI (.github/workflows/ci.yml) runs these four
# targets as separate matrix legs on separate runner VMs (each with its own,
# uncontended Docker daemon) instead of one `test-integration` on a single
# runner, to claw back the wall-clock `-p 1` gave up above WITHOUT
# reintroducing the same-host container contention that caused the hangs
# -p 1 fixed in the first place. See ci.yml's own comment on the
# `test-integration` job for the bin-packing rationale, the timing data
# it's based on, and how these four groups were chosen.
#
# Groups 2-4 run at -p 2 (group 1 is httpapi alone -- a single package, so
# -p has no effect there, left at -p 1). This was verified, not assumed:
# group 3 alone ran 5 independent real CI repeats at -p 2 (4m56s-5m13s,
# vs. a 7m32s -p 1 baseline -- a consistent ~33% improvement) with zero
# hang recurrence before this was extended to groups 2 and 4 as well.
# Groups 2 and 4 were not individually put through that same 5-repeat
# verification -- the extension rests on group 3's result plus the same
# per-VM isolation PR #133's own job-matrix split already provides (no
# cross-group Docker daemon contention, only WITHIN-group contention is
# still possible at -p 2). If a hang symptom (a "test timed out after
# 10m0s" panic) resurfaces on group 2 or 4 specifically, that's a real
# signal this extension went too far for those groups' own package mix --
# drop that one group back to -p 1 rather than reverting all of them.
#
# Groups 1-3 are deliberately short, explicit package lists -- the handful
# of packages heavy enough to matter for balancing. Group 4 is everything
# else, computed from `go list -tags=integration ./...` (the same package
# universe `go test -tags=integration ./...` would use -- test/resilience,
# for instance, only exists as a package at all under that tag) rather than
# hand-listed, so a newly added package is automatically covered (in group
# 4) the moment it exists, instead of silently never running in CI until
# someone remembers to add it to a group.
INTEGRATION_MODULE := github.com/narvidev/narvi

INTEGRATION_GROUP_1 := $(INTEGRATION_MODULE)/internal/adapters/inbound/httpapi

INTEGRATION_GROUP_2 := \
	$(INTEGRATION_MODULE)/internal/app/sessionactor \
	$(INTEGRATION_MODULE)/internal/app/outboxworker \
	$(INTEGRATION_MODULE)/internal/app/actorauthz \
	$(INTEGRATION_MODULE)/internal/app/reconciler

INTEGRATION_GROUP_3 := \
	$(INTEGRATION_MODULE)/internal/adapters/inbound/slack \
	$(INTEGRATION_MODULE)/internal/adapters/outbound/opencode \
	$(INTEGRATION_MODULE)/internal/app/imagebuild \
	$(INTEGRATION_MODULE)/internal/adapters/inbound/auth \
	$(INTEGRATION_MODULE)/cmd/sandbox-agent \
	$(INTEGRATION_MODULE)/test/resilience

test-integration-group-1:
	go test -tags=integration -race -p 1 $(INTEGRATION_GROUP_1)

test-integration-group-2:
	go test -tags=integration -race -p 2 $(INTEGRATION_GROUP_2)

test-integration-group-3:
	go test -tags=integration -race -p 2 $(INTEGRATION_GROUP_3)

test-integration-group-4:
	@tmp="$$(mktemp)"; \
	printf '%s\n' $(INTEGRATION_GROUP_1) $(INTEGRATION_GROUP_2) $(INTEGRATION_GROUP_3) > "$$tmp"; \
	pkgs="$$(go list -tags=integration ./... | grep -vxF -f "$$tmp")"; \
	rm -f "$$tmp"; \
	go test -tags=integration -race -p 2 $$pkgs

# dev is a LOCAL DEV convenience only (docker-compose.dev.yml), distinct
# from the self-host production story (§12.1: "one binary + Postgres") —
# this just brings up a throwaway local Postgres and runs `narvi serve`
# against it. Every env var below is an obviously-fake, dev-only value
# supplied inline by this recipe so platform.Load() (internal/platform/
# config.go) succeeds: the 3 HMAC secrets, the GitHub OAuth credentials,
# the GitHub webhook secret + bot handle (Step 32, "GitHub ingress") and the
# GitHub bot token (Step 35, "outbox delivery"),
# NARVI_PUBLIC_BASE_URL (matches config.go's own defaultHTTPAddr, ":8080"),
# NARVI_TOKEN_ENCRYPTION_KEY (a real base64 encoding of exactly 32 random
# bytes -- Load() rejects anything else for AES-256-GCM), one signup
# allowlist mechanism (NARVI_ALLOWED_GITHUB_ORGS, satisfying Load()'s
# OR-of-three-allowlists requirement), the Modal base URL/auth token
# (a syntactically valid URL with a real host, but nothing actually
# listens there -- spawning a real sandbox locally needs further setup;
# this only restores "make dev boots", not "make dev's Modal calls work"),
# and Step 34's own Linear webhook secret/OAuth credentials/default repo
# (also placeholders -- nothing actually verifies a Linear webhook
# signature or exchanges a Linear OAuth code locally either), and
# (Step 33, "Slack ingress") the Slack signing secret/bot token pair --
# nothing actually listens for any of these locally, so a real Slack/
# Linear webhook or chat.postMessage/OAuth exchange still needs further
# setup, same caveat as Modal.
# Config.Load() still requires every one of these unconditionally in Go —
# nothing is made "optional in development" there.
# §30.4 ("sandbox capability: GitHub App read-only installation tokens")
# adds NARVI_GITHUB_APP_ID/NARVI_GITHUB_APP_PRIVATE_KEY: a fixed, test-only
# 2048-bit RSA key (base64-encoded PEM, never used against any real GitHub
# App -- the identical placeholder internal/platform/config_test.go's own
# testGitHubAppPrivateKeyPEM uses) so Load() succeeds locally. Nothing
# actually mints a real installation token against this placeholder --
# spawning a sandbox that needs one locally needs a real GitHub App
# configured, same caveat as Modal/Slack/Linear above.
# Step 58 ("uploads, blob storage & the in-sandbox download_file tool",
# §28.7) adds the NARVI_OBJECT_STORE_* block, pointed at
# docker-compose.dev.yml's own objectstore service (versity/versitygw,
# replacing MinIO -- see that file's own comment): the root user/password/
# bucket here match that file's own ROOT_ACCESS_KEY/ROOT_SECRET_KEY and its
# own self-provisioned bucket name EXACTLY -- unlike every credential
# above, these are not placeholders, uploads actually work end to end
# against this local objectstore service. NARVI_OBJECT_STORE_USE_PATH_STYLE
# is required true for it (§28.7's own path-style toggle, same requirement
# MinIO-style backends generally have).
dev:
	docker compose -f docker-compose.dev.yml up -d --wait
	NARVI_STAGE=development \
	NARVI_DATABASE_URL=postgres://narvi:narvi@localhost:$${NARVI_DEV_PG_PORT:-5432}/narvi?sslmode=disable \
	NARVI_HMAC_SANDBOX_SECRET=dev-only-insecure-sandbox-secret \
	NARVI_HMAC_BOTS_SECRET=dev-only-insecure-bots-secret \
	NARVI_HMAC_WEBHOOK_SECRET=dev-only-insecure-webhook-secret \
	NARVI_GITHUB_CLIENT_ID=dev-github-client-id-placeholder \
	NARVI_GITHUB_CLIENT_SECRET=dev-github-client-secret-placeholder \
	NARVI_GITHUB_WEBHOOK_SECRET=dev-only-insecure-github-webhook-secret \
	NARVI_GITHUB_BOT_HANDLE=narvi-bot \
	NARVI_GITHUB_BOT_TOKEN=dev-github-bot-token-placeholder \
	NARVI_PUBLIC_BASE_URL=http://localhost:8080 \
	NARVI_TOKEN_ENCRYPTION_KEY=X4x5GAK5D4bwFxg5fEzToXLfPfe2XwZp8U3CR/Pl1Z4= \
	NARVI_ALLOWED_GITHUB_ORGS=dev-org-placeholder \
	NARVI_MODAL_BASE_URL=http://localhost:9999 \
	NARVI_MODAL_AUTH_TOKEN=dev-modal-token-placeholder \
	NARVI_LINEAR_WEBHOOK_SECRET=dev-linear-webhook-secret-placeholder \
	NARVI_LINEAR_CLIENT_ID=dev-linear-client-id-placeholder \
	NARVI_LINEAR_CLIENT_SECRET=dev-linear-client-secret-placeholder \
	NARVI_LINEAR_DEFAULT_REPO_NAME=narvi \
	NARVI_LINEAR_DEFAULT_REPO_URL=https://github.com/narvidev/narvi \
	NARVI_SLACK_SIGNING_SECRET=dev-slack-signing-secret-placeholder \
	NARVI_SLACK_BOT_TOKEN=dev-slack-bot-token-placeholder \
	NARVI_ANTHROPIC_API_KEY=dev-anthropic-api-key-placeholder \
	NARVI_INTENT_CLASSIFIER_PROVIDER=anthropic \
	NARVI_INTENT_CLASSIFIER_MODEL=claude-haiku-4-5 \
	NARVI_OBJECT_STORE_ENDPOINT=http://localhost:$${NARVI_DEV_OBJSTORE_PORT:-9000} \
	NARVI_OBJECT_STORE_REGION=us-east-1 \
	NARVI_OBJECT_STORE_BUCKET=narvi-dev-uploads \
	NARVI_OBJECT_STORE_ACCESS_KEY_ID=narvi \
	NARVI_OBJECT_STORE_SECRET_ACCESS_KEY=narvi-dev-secret \
	NARVI_OBJECT_STORE_USE_PATH_STYLE=true \
	NARVI_GITHUB_APP_ID=999999 \
	NARVI_GITHUB_APP_PRIVATE_KEY=LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcEFJQkFBS0NBUUVBMlozYVRnSXR6cE1YaXd0UVZGZW16VlZsd0JYY1E1RUJ1UUNnT281SG1tNnpyV25DCjliR0xUSzF4S1I1eFVqLy9Rck9taXJBb25lZlB3QmhnVmloTXpoL1NYMHpPOUgvWWhiS3F5aVVsRHI3ck0vMlYKMDZhVWxFdEtLcEZwbWVDemNSaUtPaGFTeWt1Q09XYVJzQWpWMzFSUGVVTi9MaVl6VUswTmlhL2piU05BRENYQwp3MzJKUjh0TmE1U1VwOFJ6amJkdWdUd0EvT1l4SXZTNmZSTnYvM0lWUXVBSGZaTFhHTmFhZGt3KzBPem1LY2x3Ck9NQmZZejRYRjdzOHFGOWR5eFhscm84eTNZak1COGcxYXJBcG9IeExyVlZ5S0xmN3h6VWJ3cG1uTW9QS2NKanYKMXN4bG5BMFhWcEwrZk1RN3RoV3dkOUZuSDBpOS8vcCs0c0dmeXdJREFRQUJBb0lCQUNIUngyQ0NOQzQ3YTlnLwpETi9lczF5TDNnRkpKRzhYdFFYVVZCSmxsRGtxNVIrWkpTUmIwRU05WFMyL3ZtckM2VityWGNHRitQbjVVYThQCjJzRHBDRzZzUVZ4d0ttV1RETXBTWnZwOVpWSHlWOGsvcXE0MjREWmZzUW9HaVR2UjBQRk5tQVhKQmswTUNSUDAKbmNXV3llNG9ReVdjV01LS1MwVkpiNllyUUpQd01lYVpwbkxEUGsvUDFhZnZyVkxHMG81SXRZNUxGa1dHaVdTYgpDbCtwOERGbytSWlFmVW1ERzdEY2hPWHIvZTJvd1NEODFIV2N3SXlzUlZxcGxPL3d4M2xuMjJIaUR5dVVxWTN4CmNQS2w5RHZ6c0I1NzZVU09MS0IvSGkvTjNIemc1bCt2V1MzZVZYeXovWlZYRnkrY3NkVkV6eG03QjA4QWlZK1UKRmo3Q3hjRUNnWUVBLzVDMkRWazMwQXUzRElYTnpxalFXUWJYV3BsWG5MNFU1NUJsUExYSWt3QlRHVU4rNFlOMQppQUNOM00wMUkyVnRHQ1B4a2pVbTg1WWxMc2tJYitaNGpyeW9NWk5XQzd1dzNyQ01LYit4aCtmZmdpdWNjSnRMClBMWGNsU203NzFBQ2FBT2FOR0R5RkduK3V3UzM5WDZ6MGJvaXIzc1I2NkFkVGx6TzJVYlhDNnNDZ1lFQTJmeWQKemhaYXRaMGxsYnZrcHdTbnozOEd5aWpnQjI1NmNyQ1dBSFo1dmwrVndvZ2Y0ZnllT0tacTFIalZkVSs3cUJOUgpEcGJDYVlkQWEwTXdMdXZ2Ti9jWVROVkpYUlM2S2ZOL3ZLUXBFQVFSMXN4L29Rd2s5TW9lbjJoSFoxVnBzM0pCClYrdVk3cDE0bVYxR2JIdjM1V3hQeXdKR1FWdlZiMklVWnNaK25HRUNnWUVBNVYwNUpxM0YyNkJIL3FNdjNLUEIKcWNUc0RsSEZRZFdPNld5OGowb080Mi9OSk1WZzRJQ2RRUnhPTmJhdVZFQTVNd3MvU1pzT2hGdGlyNlNaUCtTMgptbFJURjN0R0pHMmxCWmVwaythSkxKSThGSldUWjdUWVIzcG9xQzYyanNkZUFZQUtLNncrVjNmeHVHTTV2c2lpCkZqNVoxdWc3WXg5bWJlZjVkU09RNk5VQ2dZQlBWRGx4aUgwV1hzd1F3OElnYmZkTDhlUmNxYWR0ek96TzFDaWkKbm5zTHB1bHZVKzZXWlVLSFJ6alZmZXZndDFXSmd3NGFpdzdSTEtGcTU1YWZYTWsveXJLVE00TnhWbHV4YktYdAoxcWdDNWhnLzNVZ05LY2hCTlZVVG1mVnlTNGtkL3RSODFJWmhQL2xsaHFaY1VIa1VpdWcyN3VyMldoOUFXNmNsCkI5T0h3UUtCZ1FEekt2YzMvWDZ6NzdMeUFqb1BIZUpIbXQyL2tSWllJQjNmUUlsRkg0R3JoVUg1TXdLNklIeUkKWGxPUU53ZHVwdm5QaXlHS0dYeUwvcHJSVGdxQXpGMUFPNW0xWG8wVlJnMVZTeGp6Y1RPTU0zVGpnYU5GYmlMdwpreUovZjlhdzhrUTU2RFA2OWlzV1BKaVUyQko1blZLUTJPVEJwSHNTa2h5eS94amZaT29zbFE9PQotLS0tLUVORCBSU0EgUFJJVkFURSBLRVktLS0tLQo= \
	go run ./cmd/control-plane serve

# contracts-generate regenerates every codegen target under contracts/gen from
# the JSON Schemas under /contracts (§6). Go output uses the go-jsonschema
# tool pinned by go.mod's `tool` directive (no separately-installed binary
# required); TS output uses contracts/scripts/generate-ts.mjs, which requires
# contracts/node_modules to already be installed (`cd contracts && npm ci`).
#
# The go-jsonschema runs are followed by ./tools/contractspatch, which turns
# the DEFINED pointer types go-jsonschema emits for nullable properties
# (`type AutomationLastRunAt *time.Time`) into type ALIASES. A defined type
# whose underlying type is a pointer has an empty method set and cannot be
# given one (Go forbids a pointer receiver base type), so such a field never
# reaches time.Time's own UnmarshalJSON and fails to decode any non-null
# value. See that command's package comment for the full writeup;
# contracts/contractstest/restdtos_test.go's TestAutomationRoundTrip and
# TestShadowLedgerSummaryRoundTrip for the behavioral regression tests; and
# contracts/contractstest/genshape_test.go for the structural guard that
# fails if this step is ever skipped. The patch is idempotent, so
# contracts-check's regenerate-and-diff stays clean.
contracts-generate:
	go tool go-jsonschema contracts/sandbox-ws/v1/commands.schema.json contracts/sandbox-ws/v1/events.schema.json -p sandboxws -o contracts/gen/go/sandboxws/sandboxws.go
	go tool go-jsonschema contracts/client-ws/v1/protocol.schema.json -p clientws -o contracts/gen/go/clientws/clientws.go
	go tool go-jsonschema contracts/session-config/v1/session-config.schema.json -p sessionconfig -o contracts/gen/go/sessionconfig/sessionconfig.go
	go tool go-jsonschema contracts/rest/v1/dtos.schema.json -p restdtos -o contracts/gen/go/restdtos/restdtos.go
	go run ./tools/contractspatch contracts/gen/go/sandboxws/sandboxws.go contracts/gen/go/clientws/clientws.go contracts/gen/go/sessionconfig/sessionconfig.go contracts/gen/go/restdtos/restdtos.go
	cd contracts && npm run generate

# contracts-check (§9.2/§10 exit criterion "contracts round-trip green") is
# the drift check: snapshot contracts/gen, regenerate everything, fail if
# that changed anything versus the snapshot, then typecheck the TS output
# against contracts/tsconfig.json (which also covers the hand-written
# contracts/typecheck fixtures). Deliberately plain-diff based (not
# git-diff-based) so it works the same whether or not contracts/gen is
# already committed, and regardless of any git wrapper on PATH.
contracts-check:
	@tmp="$$(mktemp -d)" && \
	cp -R contracts/gen "$$tmp/gen-before" && \
	$(MAKE) contracts-generate && \
	if ! diff -ru "$$tmp/gen-before" contracts/gen; then \
		echo "contracts/gen is out of date with the schemas under /contracts (diff above): run 'make contracts-generate' and commit the result"; \
		rm -rf "$$tmp"; \
		exit 1; \
	fi; \
	rm -rf "$$tmp"
	cd contracts && npm run typecheck

# web-* targets (§12.1, "ui bootstrap"): the frontend's own equivalent of
# vet/lint/test/build above, over web/ (Vite + React + TanStack Query/
# Router) instead of the Go module. Each assumes `cd web && npm ci` has
# already been run -- mirrors contracts-generate/contracts-check's own
# identical assumption about contracts/node_modules (see that target's own
# comment) rather than auto-installing on every invocation.
#
# web-typecheck and web-build both regenerate src/routeTree.gen.ts first
# (web/package.json's own "generate-routes" script, via @tanstack/
# router-cli) -- that file is gitignored and normally produced by Vite's
# own dev/build lifecycle, but tsc needs it to already exist before either
# of THESE targets' own tsc pass runs, on a checkout where `vite dev`/
# `vite build` has never run yet.
web-typecheck:
	cd web && npm run typecheck

web-lint:
	cd web && npm run lint

# web-check-dto-types (§12.1: "no hand-written response types anywhere")
# enforces that no .ts/.tsx file under web/src redeclares a type/interface
# name contracts/gen/ts/*.ts already generates -- see
# web/scripts/check-no-dto-redeclaration.mjs's own top comment for why this
# specific, name-collision shape was chosen over a field-shape comparison.
web-check-dto-types:
	cd web && npm run check-dto-types

# web-test (§12.1's own data layer, Step 80: "WS transport -> event log ->
# reducer -> query invalidation"): typechecks web/src/**/__tests__ (its own
# standalone tsconfig.vitest.json -- see web/README.md's own "Testing"
# section for why that tree is not part of web-typecheck's own tsc -b
# project) and runs the real pipeline tests (vitest) -- transport.test.ts/
# sessionStream.test.ts drive the real transport/reducer against a real
# local fake WS server, not hand-made objects.
web-test:
	cd web && npm test

# web-build produces the web UI's static bundle, written DIRECTLY into
# internal/adapters/inbound/webui/dist/ (web/vite.config.ts's own outDir --
# see that file's own comment for why go:embed's own directive syntax rules
# out the more conventional web/dist here). Required before
# `go build -tags web_assets ./cmd/control-plane` can compile at all (see
# internal/adapters/inbound/webui's own doc comment) -- the single-binary
# `make dist` recipe that wires the two together end to end is a later
# Step's own scope, not built here.
web-build:
	cd web && npm run build

# web-check is a LOCAL convenience only (typecheck + lint + the DTO-name
# guard + test + build, in the order most likely to fail fast) -- CI
# (.github/workflows/ci.yml's own `web` job) runs the same npm scripts as
# separate steps instead, for per-concern failure attribution in the
# Actions UI rather than one opaque `make` invocation.
web-check: web-typecheck web-lint web-check-dto-types web-test web-build

# dist is the single-binary recipe web-build's own comment and
# assets_embed.go/doc.go (internal/adapters/inbound/webui) both name and
# defer: wiring `make web-build` and `go build -tags web_assets` together
# end to end (§12.1: "one binary + Postgres", §12.4: "make dist produces
# the single self-contained binary"). Depends on web-build so `make dist`
# alone -- from a checkout with only `cd web && npm ci` already run --
# produces a complete binary with no separate step; web-build's own `vite
# build` empties and rewrites its outDir every run (vite.config.ts's
# default emptyOutDir), so this never silently embeds a stale dist/ left
# over from a previous branch or build. Only cmd/control-plane is built:
# cmd/sandbox-agent embeds no web assets and is not the "single self-
# contained binary" §12.4 means -- it stays on plain `go build` outside
# this target, unaffected by the web_assets tag. Output is `narvi` at the
# repo root (IMPLEMENTATION_PLAN.md's own "make dist produces the
# standalone narvi binary"; TECHNICAL_PLAN.md §12.1's "narvi serve" is
# this same binary invoking cmd/control-plane/main.go's own "serve"
# subcommand) -- gitignored the same way /control-plane and
# /sandbox-agent already are. No stripping or version-stamping: no
# build-time version pipeline exists anywhere in this repo yet (see
# internal/sandboxagent/boot/config.go's own defaultAgentVersion comment),
# so inventing one only for this target would be its own, un-asked-for
# scope rather than something §12.4 requires.
# lint-web-assets runs the arch-test over the code the DEFAULT build
# context cannot see. `make lint`'s own narvichecks pass reads only
# untagged files, so a client-side net/http symbol or an os/exec import
# behind //go:build web_assets would ship in the release binary having
# been analyzed by nothing at all -- an arch-test blind to shipped code is
# not an arch-test.
#
# The tag is passed via GOFLAGS, deliberately: `narvichecks -tags X` is
# accepted and SILENTLY IGNORED (the analysis framework's own flag set
# does not define -tags), so that spelling exits 0 having checked
# nothing. A check that cannot fail is worse than no check, because it
# reads as coverage.
#
# Depends on web-build for the same reason `dist` does: the tagged file
# embeds `all:dist` and does not compile until that bundle exists.
lint-web-assets: web-build
	GOFLAGS=-tags=web_assets go run ./tools/lint/narvichecks ./...

dist: web-build lint-web-assets
	go build -tags web_assets -o narvi ./cmd/control-plane

# verify-control-plane-image is Step 162's own exit-criterion proof
# (docs/TECHNICAL_PLAN.md §41.1), run locally and by
# .github/workflows/ci.yml's own "control-plane-image" job on every PR AND
# every v*.*.* tag push (publish-control-plane-image `needs:` that same
# job -- §41.1 review round 1, finding P6 -- so nothing reaches GHCR
# without this target passing against that exact commit first).
#
# Brings up ONLY docker-compose.dev.yml's own postgres service -- never
# its objectstore service (§41.1 review round 1, finding P4, written when
# that service was still MinIO+minio-init): the object-store variables are
# entirely optional, feature-flagged on NARVI_OBJECT_STORE_ENDPOINT alone
# (internal/platform/config.go), and this target's own containers never
# set it, so no running object-store backend is ever needed to boot. This
# was originally ALSO a deliberate sidestep of MinIO's own broken
# anonymous pull (minio-init's image had stopped answering Docker Hub
# pulls) -- that reason no longer applies now that docker-compose.dev.yml's
# objectstore service pulls cleanly (see that file's own comment), but the
# underlying reason to skip it here still holds on its own: this target
# has no reason to bring any object-store backend up at all.
#
# Runs the compose stack under its OWN project name (-p), fully separate
# from `make dev`'s default project: this target must not disturb (or be
# disturbed by) a developer's already-running dev stack, and bringing up
# a SEPARATE, disposable Postgres (own container, own volume, own
# network, torn down in the trap below) is simpler and safer than trying
# to share one — a fresh, guaranteed-freshly-migrated database beats
# reusing whatever schema state a long-lived `make dev` volume happens to
# be in. NARVI_DEV_PG_PORT is overridden to a dedicated port for this
# project alone, so its own host-published port binding can never collide
# with a `make dev` stack that is already up on the default one.
#
# The control-plane container(s) below reach Postgres over that compose
# project's OWN network, by the plain service name "postgres" (compose's
# embedded DNS resolves it, confirmed empirically), NOT via
# host.docker.internal + the published host port the way this target used
# to. That published port is bound to 127.0.0.1 only (docker-compose.dev.
# yml), which host.docker.internal resolves straight to on Docker
# Desktop's own loopback-routing gateway -- but NOT on a plain Linux
# Engine, where host.docker.internal resolves to the docker0 bridge
# gateway, and a 127.0.0.1-bound published port's own docker-proxy/DNAT
# rule never answers traffic arriving from there (§41.1 review round 1,
# finding P4 -- reproduced directly: a container dialing the docker0
# gateway got connection-refused against a port docker-compose.dev.yml
# publishes exactly this way). The compose-network approach sidesteps the
# whole distinction: confirmed working identically against a real Docker
# Desktop instance and Linux Engine's own bridge-network DNS resolution
# path alike, since neither depends on host.docker.internal at all.
#
# This Step's own exit criterion (§41.1) has three parts, checked in
# order below -- each one replacing what an earlier version of this
# target tried to prove with a single `curl` loop, which could not
# actually prove any of them (§41.1 review round 1, findings P1-P3: a
# live probe that only checks "not 404" cannot distinguish a genuinely
# absent route from one behind auth middleware that answers 401 for ANY
# unmatched sub-path — including a REMOVED one — and never notices an
# EXTRA route the golden doesn't have at all):
#
#   1. `docker run <image> routes` (the "routes" subcommand,
#      controlplane/routescmd.go) against a fresh, UNMIGRATED Postgres,
#      piped straight to `cmp` against controlplane/testdata/
#      routes.golden. "routes" is READ-ONLY (§41.1 review round 2, finding
#      Q1/Q3 -- it used to apply migrations before Build, which made a
#      read-only listing forward-migrate whatever database it was pointed
#      at; it no longer does, since Build's router construction does not
#      need a migrated schema at all). It loads config, opens the pool,
#      and calls the EXACT SAME Build serve() calls -- it just never
#      calls GitHub and never starts a listener (no
#      verifyGitHubAppScopeAtBoot, no app.Run) -- so this is a clean,
#      byte-for-byte route-table-identity proof, independent of GitHub
#      reachability AND of the schema's migration state.
#      TestRunRoutesCommand_MatchesGolden
#      (controlplane/routescmd_integration_test.go) pins this same
#      output format in Go; this is that same proof, run against the
#      actual packaged image instead of `go test`.
#   2. The image actually SERVES real HTTP traffic. tools/ghappstub is a
#      tiny local stand-in for GitHub's own REST API (GET /app only),
#      built and run as a plain background process on THIS host (never a
#      new image pull). The control-plane container is pointed at it via
#      NARVI_GITHUB_API_BASE_URL (internal/platform/config.go -- §41.1
#      review round 1 finding P2's own fix for the hard-coded
#      "https://api.github.com" literal that made this proof impossible:
#      serve()'s own real, UNMODIFIED verifyGitHubAppScopeAtBoot check
#      used to refuse to boot against a bogus fixture App ID, and the
#      only alternatives were reaching the real GitHub API from CI or
#      weakening the boot-time check itself, which §30.4(4) forbids).
#      ghappstub grants exactly the read-only permission set that check
#      accepts, so the packaged image's own real scope check runs,
#      succeeds, and /health then answers a genuine 200 -- proving the
#      image opens its listener and serves, not merely that its router
#      object can be constructed. Reached via
#      --add-host=host.docker.internal:host-gateway (this one IS the
#      right tool for reaching a wildcard-bound HOST PROCESS, unlike the
#      Postgres case above -- host-gateway resolves to an address the
#      HOST'S OWN kernel routes to any 0.0.0.0-bound process, on both
#      Docker Desktop and a plain Linux Engine, which is exactly what
#      tools/ghappstub's own doc comment requires of it).
#   3. A second, freshly-run container with ZERO configuration must exit
#      non-zero with platform.Config's own validation message on stderr,
#      never a panic/stack trace -- unchanged from before, and checked
#      FIRST below since it needs neither Postgres nor ghappstub.
#
# --platform linux/amd64 is pinned deliberately, not left to the host's
# own default: .github/workflows/ci.yml's own control-plane-image and
# publish-control-plane-image jobs both build on ubuntu-latest (amd64),
# and deploy/control-plane/deployment.yaml targets an ordinary amd64 cloud
# node pool -- an arm64 host (e.g. Apple Silicon) building without this
# pin gets a DIFFERENT image than the one CI verifies and the one that
# ships. §41.1 review round 1 (finding P7) asked whether this pin is still
# earning its keep, since the control-plane BINARY itself builds cleanly
# for arm64 (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build
# ./cmd/control-plane` succeeds). It is: `make dist` (what this
# Dockerfile's own build stage runs) depends on lint-web-assets, which
# loads every package under GOFLAGS=-tags=web_assets, INCLUDING _test.go
# files that never ship in the binary -- and
# internal/platform/otel_test.go fails to even compile under
# GOOS=linux/GOARCH=arm64 (undefined: syscall.Dup2, confirmed directly
# against this exact worktree, both as a bare `go vet` and through the
# built narvichecks binary). So a plain `docker build .` with no
# --platform, on a real arm64 host, DOES fail in stage 1 without this pin
# -- a pre-existing, out-of-scope arm64 test bug (unix.Dup2/syscall.Dup3
# would work there instead), left unfixed here and named in this PR's own
# description; the pin stays.
NARVI_CPVERIFY_COMPOSE_PROJECT := narvi-cpverify
NARVI_CPVERIFY_PG_PORT := 25432
NARVI_CPVERIFY_GHAPPSTUB_PORT := 18081

verify-control-plane-image:
	set -eu; \
	GHAPPSTUB_PID=""; \
	trap ' \
		docker rm -f narvi-control-plane-verify >/dev/null 2>&1 || true; \
		if [ -n "$$GHAPPSTUB_PID" ]; then kill "$$GHAPPSTUB_PID" >/dev/null 2>&1 || true; fi; \
		NARVI_DEV_PG_PORT=$(NARVI_CPVERIFY_PG_PORT) docker compose -f docker-compose.dev.yml -p $(NARVI_CPVERIFY_COMPOSE_PROJECT) down -v >/dev/null 2>&1 || true \
	' EXIT; \
	docker build --platform linux/amd64 -t narvi-control-plane:verify .; \
	NARVI_DEV_PG_PORT=$(NARVI_CPVERIFY_PG_PORT) docker compose -f docker-compose.dev.yml -p $(NARVI_CPVERIFY_COMPOSE_PROJECT) up -d --wait postgres; \
	NETWORK=$(NARVI_CPVERIFY_COMPOSE_PROJECT)_default; \
	DBURL=postgres://narvi:narvi@postgres:5432/narvi?sslmode=disable; \
	echo "--- checking unconfigured boot refuses cleanly ---"; \
	set +e; \
	docker run --rm narvi-control-plane:verify serve >/tmp/narvi-control-plane-noconfig.stdout 2>/tmp/narvi-control-plane-noconfig.stderr; \
	status=$$?; \
	set -e; \
	if [ "$$status" -eq 0 ]; then \
		echo "expected non-zero exit with no configuration at all, got 0" >&2; \
		exit 1; \
	fi; \
	if ! grep -qi "missing required" /tmp/narvi-control-plane-noconfig.stderr; then \
		echo "expected platform.Config's own validation message (\"missing required ...\") on stderr" >&2; \
		cat /tmp/narvi-control-plane-noconfig.stderr >&2; \
		exit 1; \
	fi; \
	if grep -qiE 'panic:|goroutine [0-9]+ \[' /tmp/narvi-control-plane-noconfig.stderr; then \
		echo "got a panic/stack trace on stderr, not a clean validation error" >&2; \
		cat /tmp/narvi-control-plane-noconfig.stderr >&2; \
		exit 1; \
	fi; \
	echo "unconfigured boot OK: exit=$$status, stderr names platform.Config's own validation message, no panic"; \
	echo "--- checking the route table (docker run <image> routes) ---"; \
	docker run --rm --network "$$NETWORK" \
		-e NARVI_STAGE=development \
		-e NARVI_DATABASE_URL="$$DBURL" \
		-e NARVI_HMAC_SANDBOX_SECRET=dev-only-insecure-sandbox-secret \
		-e NARVI_HMAC_BOTS_SECRET=dev-only-insecure-bots-secret \
		-e NARVI_HMAC_WEBHOOK_SECRET=dev-only-insecure-webhook-secret \
		-e NARVI_GITHUB_CLIENT_ID=dev-github-client-id-placeholder \
		-e NARVI_GITHUB_CLIENT_SECRET=dev-github-client-secret-placeholder \
		-e NARVI_GITHUB_WEBHOOK_SECRET=dev-only-insecure-github-webhook-secret \
		-e NARVI_GITHUB_BOT_HANDLE=narvi-bot \
		-e NARVI_GITHUB_BOT_TOKEN=dev-github-bot-token-placeholder \
		-e NARVI_PUBLIC_BASE_URL=http://localhost:18080 \
		-e NARVI_TOKEN_ENCRYPTION_KEY=X4x5GAK5D4bwFxg5fEzToXLfPfe2XwZp8U3CR/Pl1Z4= \
		-e NARVI_ALLOWED_GITHUB_ORGS=dev-org-placeholder \
		-e NARVI_MODAL_BASE_URL=http://host.docker.internal:9999 \
		-e NARVI_MODAL_AUTH_TOKEN=dev-modal-token-placeholder \
		-e NARVI_LINEAR_WEBHOOK_SECRET=dev-linear-webhook-secret-placeholder \
		-e NARVI_LINEAR_CLIENT_ID=dev-linear-client-id-placeholder \
		-e NARVI_LINEAR_CLIENT_SECRET=dev-linear-client-secret-placeholder \
		-e NARVI_LINEAR_DEFAULT_REPO_NAME=narvi \
		-e NARVI_LINEAR_DEFAULT_REPO_URL=https://github.com/narvidev/narvi \
		-e NARVI_SLACK_SIGNING_SECRET=dev-slack-signing-secret-placeholder \
		-e NARVI_SLACK_BOT_TOKEN=dev-slack-bot-token-placeholder \
		-e NARVI_ANTHROPIC_API_KEY=dev-anthropic-api-key-placeholder \
		-e NARVI_INTENT_CLASSIFIER_PROVIDER=anthropic \
		-e NARVI_INTENT_CLASSIFIER_MODEL=claude-haiku-4-5 \
		-e NARVI_GITHUB_APP_ID=999999 \
		-e NARVI_GITHUB_APP_PRIVATE_KEY=LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcEFJQkFBS0NBUUVBMlozYVRnSXR6cE1YaXd0UVZGZW16VlZsd0JYY1E1RUJ1UUNnT281SG1tNnpyV25DCjliR0xUSzF4S1I1eFVqLy9Rck9taXJBb25lZlB3QmhnVmloTXpoL1NYMHpPOUgvWWhiS3F5aVVsRHI3ck0vMlYKMDZhVWxFdEtLcEZwbWVDemNSaUtPaGFTeWt1Q09XYVJzQWpWMzFSUGVVTi9MaVl6VUswTmlhL2piU05BRENYQwp3MzJKUjh0TmE1U1VwOFJ6amJkdWdUd0EvT1l4SXZTNmZSTnYvM0lWUXVBSGZaTFhHTmFhZGt3KzBPem1LY2x3Ck9NQmZZejRYRjdzOHFGOWR5eFhscm84eTNZak1COGcxYXJBcG9IeExyVlZ5S0xmN3h6VWJ3cG1uTW9QS2NKanYKMXN4bG5BMFhWcEwrZk1RN3RoV3dkOUZuSDBpOS8vcCs0c0dmeXdJREFRQUJBb0lCQUNIUngyQ0NOQzQ3YTlnLwpETi9lczF5TDNnRkpKRzhYdFFYVVZCSmxsRGtxNVIrWkpTUmIwRU05WFMyL3ZtckM2VityWGNHRitQbjVVYThQCjJzRHBDRzZzUVZ4d0ttV1RETXBTWnZwOVpWSHlWOGsvcXE0MjREWmZzUW9HaVR2UjBQRk5tQVhKQmswTUNSUDAKbmNXV3llNG9ReVdjV01LS1MwVkpiNllyUUpQd01lYVpwbkxEUGsvUDFhZnZyVkxHMG81SXRZNUxGa1dHaVdTYgpDbCtwOERGbytSWlFmVW1ERzdEY2hPWHIvZTJvd1NEODFIV2N3SXlzUlZxcGxPL3d4M2xuMjJIaUR5dVVxWTN4CmNQS2w5RHZ6c0I1NzZVU09MS0IvSGkvTjNIemc1bCt2V1MzZVZYeXovWlZYRnkrY3NkVkV6eG03QjA4QWlZK1UKRmo3Q3hjRUNnWUVBLzVDMkRWazMwQXUzRElYTnpxalFXUWJYV3BsWG5MNFU1NUJsUExYSWt3QlRHVU4rNFlOMQppQUNOM00wMUkyVnRHQ1B4a2pVbTg1WWxMc2tJYitaNGpyeW9NWk5XQzd1dzNyQ01LYit4aCtmZmdpdWNjSnRMClBMWGNsU203NzFBQ2FBT2FOR0R5RkduK3V3UzM5WDZ6MGJvaXIzc1I2NkFkVGx6TzJVYlhDNnNDZ1lFQTJmeWQKemhaYXRaMGxsYnZrcHdTbnozOEd5aWpnQjI1NmNyQ1dBSFo1dmwrVndvZ2Y0ZnllT0tacTFIalZkVSs3cUJOUgpEcGJDYVlkQWEwTXdMdXZ2Ti9jWVROVkpYUlM2S2ZOL3ZLUXBFQVFSMXN4L29Rd2s5TW9lbjJoSFoxVnBzM0pCClYrdVk3cDE0bVYxR2JIdjM1V3hQeXdKR1FWdlZiMklVWnNaK25HRUNnWUVBNVYwNUpxM0YyNkJIL3FNdjNLUEIKcWNUc0RsSEZRZFdPNld5OGowb080Mi9OSk1WZzRJQ2RRUnhPTmJhdVZFQTVNd3MvU1pzT2hGdGlyNlNaUCtTMgptbFJURjN0R0pHMmxCWmVwaythSkxKSThGSldUWjdUWVIzcG9xQzYyanNkZUFZQUtLNncrVjNmeHVHTTV2c2lpCkZqNVoxdWc3WXg5bWJlZjVkU09RNk5VQ2dZQlBWRGx4aUgwV1hzd1F3OElnYmZkTDhlUmNxYWR0ek96TzFDaWkKbm5zTHB1bHZVKzZXWlVLSFJ6alZmZXZndDFXSmd3NGFpdzdSTEtGcTU1YWZYTWsveXJLVE00TnhWbHV4YktYdAoxcWdDNWhnLzNVZ05LY2hCTlZVVG1mVnlTNGtkL3RSODFJWmhQL2xsaHFaY1VIa1VpdWcyN3VyMldoOUFXNmNsCkI5T0h3UUtCZ1FEekt2YzMvWDZ6NzdMeUFqb1BIZUpIbXQyL2tSWllJQjNmUUlsRkg0R3JoVUg1TXdLNklIeUkKWGxPUU53ZHVwdm5QaXlHS0dYeUwvcHJSVGdxQXpGMUFPNW0xWG8wVlJnMVZTeGp6Y1RPTU0zVGpnYU5GYmlMdwpreUovZjlhdzhrUTU2RFA2OWlzV1BKaVUyQko1blZLUTJPVEJwSHNTa2h5eS94amZaT29zbFE9PQotLS0tLUVORCBSU0EgUFJJVkFURSBLRVktLS0tLQo= \
		narvi-control-plane:verify routes > /tmp/narvi-control-plane-routes.actual; \
	if ! cmp -s controlplane/testdata/routes.golden /tmp/narvi-control-plane-routes.actual; then \
		echo "route table does not match controlplane/testdata/routes.golden byte-for-byte:" >&2; \
		diff -u controlplane/testdata/routes.golden /tmp/narvi-control-plane-routes.actual >&2 || true; \
		exit 1; \
	fi; \
	echo "routes OK: docker run <image> routes matches controlplane/testdata/routes.golden byte-for-byte"; \
	echo "--- checking the image actually serves (ghappstub + /health) ---"; \
	go build -o /tmp/narvi-ghappstub ./tools/ghappstub; \
	PORT=$(NARVI_CPVERIFY_GHAPPSTUB_PORT) /tmp/narvi-ghappstub & \
	GHAPPSTUB_PID=$$!; \
	stub_ok=""; \
	for i in $$(seq 1 30); do \
		if curl -fsS "http://127.0.0.1:$(NARVI_CPVERIFY_GHAPPSTUB_PORT)/app" >/dev/null 2>&1; then stub_ok=1; break; fi; \
		sleep 1; \
	done; \
	if [ -z "$$stub_ok" ]; then \
		echo "tools/ghappstub never became ready" >&2; \
		exit 1; \
	fi; \
	docker rm -f narvi-control-plane-verify >/dev/null 2>&1 || true; \
	docker run -d --name narvi-control-plane-verify \
		--network "$$NETWORK" \
		--add-host=host.docker.internal:host-gateway \
		-p 18080:8080 \
		-e NARVI_STAGE=development \
		-e NARVI_DATABASE_URL="$$DBURL" \
		-e NARVI_GITHUB_API_BASE_URL=http://host.docker.internal:$(NARVI_CPVERIFY_GHAPPSTUB_PORT) \
		-e NARVI_HMAC_SANDBOX_SECRET=dev-only-insecure-sandbox-secret \
		-e NARVI_HMAC_BOTS_SECRET=dev-only-insecure-bots-secret \
		-e NARVI_HMAC_WEBHOOK_SECRET=dev-only-insecure-webhook-secret \
		-e NARVI_GITHUB_CLIENT_ID=dev-github-client-id-placeholder \
		-e NARVI_GITHUB_CLIENT_SECRET=dev-github-client-secret-placeholder \
		-e NARVI_GITHUB_WEBHOOK_SECRET=dev-only-insecure-github-webhook-secret \
		-e NARVI_GITHUB_BOT_HANDLE=narvi-bot \
		-e NARVI_GITHUB_BOT_TOKEN=dev-github-bot-token-placeholder \
		-e NARVI_PUBLIC_BASE_URL=http://localhost:18080 \
		-e NARVI_TOKEN_ENCRYPTION_KEY=X4x5GAK5D4bwFxg5fEzToXLfPfe2XwZp8U3CR/Pl1Z4= \
		-e NARVI_ALLOWED_GITHUB_ORGS=dev-org-placeholder \
		-e NARVI_MODAL_BASE_URL=http://host.docker.internal:9999 \
		-e NARVI_MODAL_AUTH_TOKEN=dev-modal-token-placeholder \
		-e NARVI_LINEAR_WEBHOOK_SECRET=dev-linear-webhook-secret-placeholder \
		-e NARVI_LINEAR_CLIENT_ID=dev-linear-client-id-placeholder \
		-e NARVI_LINEAR_CLIENT_SECRET=dev-linear-client-secret-placeholder \
		-e NARVI_LINEAR_DEFAULT_REPO_NAME=narvi \
		-e NARVI_LINEAR_DEFAULT_REPO_URL=https://github.com/narvidev/narvi \
		-e NARVI_SLACK_SIGNING_SECRET=dev-slack-signing-secret-placeholder \
		-e NARVI_SLACK_BOT_TOKEN=dev-slack-bot-token-placeholder \
		-e NARVI_ANTHROPIC_API_KEY=dev-anthropic-api-key-placeholder \
		-e NARVI_INTENT_CLASSIFIER_PROVIDER=anthropic \
		-e NARVI_INTENT_CLASSIFIER_MODEL=claude-haiku-4-5 \
		-e NARVI_GITHUB_APP_ID=999999 \
		-e NARVI_GITHUB_APP_PRIVATE_KEY=LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcEFJQkFBS0NBUUVBMlozYVRnSXR6cE1YaXd0UVZGZW16VlZsd0JYY1E1RUJ1UUNnT281SG1tNnpyV25DCjliR0xUSzF4S1I1eFVqLy9Rck9taXJBb25lZlB3QmhnVmloTXpoL1NYMHpPOUgvWWhiS3F5aVVsRHI3ck0vMlYKMDZhVWxFdEtLcEZwbWVDemNSaUtPaGFTeWt1Q09XYVJzQWpWMzFSUGVVTi9MaVl6VUswTmlhL2piU05BRENYQwp3MzJKUjh0TmE1U1VwOFJ6amJkdWdUd0EvT1l4SXZTNmZSTnYvM0lWUXVBSGZaTFhHTmFhZGt3KzBPem1LY2x3Ck9NQmZZejRYRjdzOHFGOWR5eFhscm84eTNZak1COGcxYXJBcG9IeExyVlZ5S0xmN3h6VWJ3cG1uTW9QS2NKanYKMXN4bG5BMFhWcEwrZk1RN3RoV3dkOUZuSDBpOS8vcCs0c0dmeXdJREFRQUJBb0lCQUNIUngyQ0NOQzQ3YTlnLwpETi9lczF5TDNnRkpKRzhYdFFYVVZCSmxsRGtxNVIrWkpTUmIwRU05WFMyL3ZtckM2VityWGNHRitQbjVVYThQCjJzRHBDRzZzUVZ4d0ttV1RETXBTWnZwOVpWSHlWOGsvcXE0MjREWmZzUW9HaVR2UjBQRk5tQVhKQmswTUNSUDAKbmNXV3llNG9ReVdjV01LS1MwVkpiNllyUUpQd01lYVpwbkxEUGsvUDFhZnZyVkxHMG81SXRZNUxGa1dHaVdTYgpDbCtwOERGbytSWlFmVW1ERzdEY2hPWHIvZTJvd1NEODFIV2N3SXlzUlZxcGxPL3d4M2xuMjJIaUR5dVVxWTN4CmNQS2w5RHZ6c0I1NzZVU09MS0IvSGkvTjNIemc1bCt2V1MzZVZYeXovWlZYRnkrY3NkVkV6eG03QjA4QWlZK1UKRmo3Q3hjRUNnWUVBLzVDMkRWazMwQXUzRElYTnpxalFXUWJYV3BsWG5MNFU1NUJsUExYSWt3QlRHVU4rNFlOMQppQUNOM00wMUkyVnRHQ1B4a2pVbTg1WWxMc2tJYitaNGpyeW9NWk5XQzd1dzNyQ01LYit4aCtmZmdpdWNjSnRMClBMWGNsU203NzFBQ2FBT2FOR0R5RkduK3V3UzM5WDZ6MGJvaXIzc1I2NkFkVGx6TzJVYlhDNnNDZ1lFQTJmeWQKemhaYXRaMGxsYnZrcHdTbnozOEd5aWpnQjI1NmNyQ1dBSFo1dmwrVndvZ2Y0ZnllT0tacTFIalZkVSs3cUJOUgpEcGJDYVlkQWEwTXdMdXZ2Ti9jWVROVkpYUlM2S2ZOL3ZLUXBFQVFSMXN4L29Rd2s5TW9lbjJoSFoxVnBzM0pCClYrdVk3cDE0bVYxR2JIdjM1V3hQeXdKR1FWdlZiMklVWnNaK25HRUNnWUVBNVYwNUpxM0YyNkJIL3FNdjNLUEIKcWNUc0RsSEZRZFdPNld5OGowb080Mi9OSk1WZzRJQ2RRUnhPTmJhdVZFQTVNd3MvU1pzT2hGdGlyNlNaUCtTMgptbFJURjN0R0pHMmxCWmVwaythSkxKSThGSldUWjdUWVIzcG9xQzYyanNkZUFZQUtLNncrVjNmeHVHTTV2c2lpCkZqNVoxdWc3WXg5bWJlZjVkU09RNk5VQ2dZQlBWRGx4aUgwV1hzd1F3OElnYmZkTDhlUmNxYWR0ek96TzFDaWkKbm5zTHB1bHZVKzZXWlVLSFJ6alZmZXZndDFXSmd3NGFpdzdSTEtGcTU1YWZYTWsveXJLVE00TnhWbHV4YktYdAoxcWdDNWhnLzNVZ05LY2hCTlZVVG1mVnlTNGtkL3RSODFJWmhQL2xsaHFaY1VIa1VpdWcyN3VyMldoOUFXNmNsCkI5T0h3UUtCZ1FEekt2YzMvWDZ6NzdMeUFqb1BIZUpIbXQyL2tSWllJQjNmUUlsRkg0R3JoVUg1TXdLNklIeUkKWGxPUU53ZHVwdm5QaXlHS0dYeUwvcHJSVGdxQXpGMUFPNW0xWG8wVlJnMVZTeGp6Y1RPTU0zVGpnYU5GYmlMdwpreUovZjlhdzhrUTU2RFA2OWlzV1BKaVUyQko1blZLUTJPVEJwSHNTa2h5eS94amZaT29zbFE9PQotLS0tLUVORCBSU0EgUFJJVkFURSBLRVktLS0tLQo= \
		narvi-control-plane:verify serve; \
	echo "waiting for /health..."; \
	ok=""; \
	for i in $$(seq 1 60); do \
		if curl -fsS http://localhost:18080/health >/dev/null 2>&1; then ok=1; break; fi; \
		sleep 1; \
	done; \
	if [ -z "$$ok" ]; then \
		echo "control-plane container never became healthy" >&2; \
		docker logs narvi-control-plane-verify >&2 || true; \
		exit 1; \
	fi; \
	echo "serve OK: /health returned 200 (the image's own real, unmodified GitHub App scope check passed against tools/ghappstub)"; \
	echo "verify-control-plane-image: PASS"

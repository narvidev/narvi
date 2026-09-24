# Dockerfile packages the control plane as a standard container image
# (§41.1, docs/TECHNICAL_PLAN.md's "control-plane packaging" -- Step 162
# closing the gap docs/FOUNDATIONS.md's own claims-parity review found:
# the document said "packaged as standard containers" while no Dockerfile,
# chart or manifest existed for it anywhere in this repository).
#
# Two named stages. Stage one ("build") runs `make dist` ITSELF -- the
# Makefile's own `dist: web-build lint-web-assets` target followed by
# `go build -tags web_assets -o narvi ./cmd/control-plane` -- never a
# parallel copy of those commands, so this image and `make dist`'s own
# documented output cannot drift from each other: whatever `make dist`
# produces on a real checkout is byte-for-byte what this stage produces.
# Stage two is a distroless, non-root runtime carrying nothing but the
# static binary stage one just built.
#
# deploy/sandbox-image/Dockerfile is a SEPARATE artifact (the base image
# every sandbox boots from, §27.7) and is unchanged by this file.

# --- Stage 1: build -----------------------------------------------------
#
# golang:1.26-bookworm, pinned by tag AND digest (§41.1's own "pin base
# images by version, preferably digest" instruction) -- must track this
# repo's own go.mod "go" directive, exactly like deploy/sandbox-image/
# Dockerfile's own builder stage.
FROM golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS build

# `make dist` needs BOTH toolchains in the SAME stage (it runs
# `make web-build` and the Go build back to back) -- the upstream golang
# image carries no Node.js at all, so it is installed here from the
# official nodejs.org distribution, pinned by exact version and verified
# by SHA-256 (not an OS package repository, which would leave this
# stage's own toolchain less reproducible than the base image pin right
# above it). node-version must track .github/workflows/ci.yml's own
# `actions/setup-node` `node-version: "24"` used for every other build of
# web/ in this repository.
ARG NODE_VERSION=24.11.0
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) NODE_ARCH=x64; NODE_SHA256=46da9a098973ab7ba4fca76945581ecb2eaf468de347173897044382f10e0a0a ;; \
        arm64) NODE_ARCH=arm64; NODE_SHA256=33a6673b2c7bffeae9deec7f9f8b31aad9119b08f13d49b2ca3ee3bebfe8260f ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz"; \
    echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1; \
    rm /tmp/node.tar.xz; \
    node --version && npm --version

WORKDIR /src

# Go module cache primed from go.mod/go.sum alone, so a rebuild that only
# touches source (not dependencies) doesn't repeat the download.
COPY go.mod go.sum ./
RUN go mod download

# npm dependencies primed from web/'s own lockfile alone, same reasoning
# -- `npm ci` requires exactly these two files and nothing else from web/.
COPY web/package.json web/package-lock.json web/
RUN cd web && npm ci

# Everything else. Deliberately after both dependency-priming steps above,
# so an ordinary source change never invalidates either download layer.
COPY . .

# `make dist` itself -- see this file's own top comment for why this is
# never reimplemented as a parallel list of commands. CGO_ENABLED=0
# produces a truly static binary: pgx/v5 (this repository's own Postgres
# driver, go.mod) is pure Go, and `go build` with CGO disabled also forces
# Go's own pure-Go DNS resolver rather than glibc's, which is what makes a
# "static" (not merely "base") distroless final stage below possible at
# all -- confirmed empirically against this exact recipe (`file` on the
# resulting binary reports "statically linked").
RUN CGO_ENABLED=0 make dist

# --- Stage 2: runtime -----------------------------------------------------
#
# distroless "static", not "base": this binary is fully static (see the
# build stage's own CGO_ENABLED=0 comment), so it needs no libc at all --
# the smaller, more minimal variant is the correct one, not merely an
# available one. ":nonroot" runs as UID/GID 65532 by default (confirmed
# via `docker inspect`); deploy/control-plane/deployment.yaml's own
# securityContext states that same UID explicitly rather than relying on
# it implicitly. Pinned by tag AND digest, matching the build stage.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Populated by CI at build time (.github/workflows/ci.yml's own "publish
# control-plane image" job) with the pushed tag and the git SHA -- the
# SAME two values sandbox-agent's own boot fingerprint already logs
# (§5.3), so a running control plane can be matched to its source. Both
# default to "unknown" for a local/dev build that never passes either
# --build-arg, which is why Load's own boot-time validation (not these
# labels) is what this image's exit criterion actually depends on.
ARG NARVI_VERSION=unknown
ARG NARVI_REVISION=unknown
LABEL org.opencontainers.image.title="narvi-control-plane" \
      org.opencontainers.image.description="Narvi control plane: one binary + Postgres (docs/TECHNICAL_PLAN.md §12.1)" \
      org.opencontainers.image.source="https://github.com/narvidev/narvi" \
      org.opencontainers.image.licenses="Elastic-2.0" \
      org.opencontainers.image.version="${NARVI_VERSION}" \
      org.opencontainers.image.revision="${NARVI_REVISION}"

# Migrations stay embedded in the binary (migrations/, go:embed) exactly
# as they are for a plain `make dist` checkout -- no separate migrations
# directory is copied in here.
COPY --from=build /src/narvi /narvi

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/narvi"]
CMD ["serve"]

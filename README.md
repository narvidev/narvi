# Narvi

> ⚠️ **Work in progress.** Narvi is under active development and is **not feature-complete**. Large parts of the system described below are still being built. It has **not been security-reviewed for production use** and should not be run against real infrastructure, real credentials, or real user data yet.

**Autonomous coding agents, in isolated sandboxes.**

> *Im Narvi hain echant* — "I, Narvi, made these." Narvi was the craftsman who forged the Doors of Durin. This project builds doors, well made: a clean ports-and-adapters core with swappable edges.

Narvi runs autonomous coding agents inside isolated cloud sandboxes. A person — or an automation — starts a session from the web, Slack, Linear, or GitHub; Narvi provisions a sandbox, runs the agent against the target repositories, streams the work back in real time, and opens a pull request attributed to the requester. Sessions are durable, recoverable, and multiplayer.

This project was started after running [OpenInspect](https://github.com/ColeMurray/background-agents) in production at [Fountain](https://github.com/onboardiq). Narvi draws on that experience, but is an independent project — not a fork or a rewrite of OpenInspect, and shares no code with it.

Narvi — this repository — is the engine and team web UI, complete on its own. Two companion projects are planned on top of it: **Narvi Desktop**, an individual client, and **Narvi Gatekeeper**, organization-scale review governance. Neither exists yet, and how either is distributed is undecided. What is decided is the dividing line: everything per-repository stays here — the full review pipeline, RBAC, SSO via OIDC, sandbox isolation — and no security capability ever moves out of this repository. See [`LICENSING.md`](LICENSING.md).

## Architecture at a glance

- **Two Go services** — a control plane and an in-sandbox agent — packaged as containers that run on any Kubernetes cluster or plain Docker/VMs, with **Postgres as the single source of truth**. Host it wherever you like; no cloud lock-in.
- **Hexagonal (ports & adapters):** a pure domain core; sandbox providers, source-control, notifiers, and the agent runtime are interchangeable adapters behind interfaces.
- **One authoritative state machine per session**, with fencing and named persistent timers — consistency and liveness guaranteed by construction, not by convention.
- **A thin web UI** embedded in the control-plane binary (`go:embed`) — self-host is one binary plus a database.

## Documents

| Path | What it is |
|---|---|
| [`docs/TECHNICAL_PLAN.md`](docs/TECHNICAL_PLAN.md) | The full technical specification: architecture, domain model, ports, cross-cutting invariants, wire contracts, feature set, identity/RBAC, the product-prototyping workflow, release review, and the decision inbox. Self-contained. |
| [`docs/IMPLEMENTATION_PLAN.md`](docs/IMPLEMENTATION_PLAN.md) | The work broken into 161 Steps across 17 phases (0–16) — 132 of scheduled work (3 of them gated on a product decision), 3 routed out to a separate repository, plus 26 named gaps (Phase 11) that gate nothing — each Step shippable as one pull request and referencing the technical-plan section that specifies it. |
| [`docs/design/mockups.html`](docs/design/mockups.html) | Visual specification — nine UI views with numbered design decisions. Open in a browser. |
| [`docs/environments.md`](docs/environments.md) | Requirements for an Environment's own `setup.sh`/`start.sh` — notably the `setup.sh` idempotency contract every repo must satisfy under warm-boot shared images. |

## Building it

The specification is written to be executed by an AI coding agent, one Step at a time. See [`CLAUDE.md`](CLAUDE.md) for how to drive it.

## Development

```bash
make build   # go build ./...
make lint    # golangci-lint + the project's custom static-analysis checks
make test    # go test -race ./...
```

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). A signed
[Contributor License Agreement](CLA.md) is required on your first pull request
(an automated check walks you through signing). For anything
security-sensitive, see [SECURITY.md](SECURITY.md).

## License

Copyright (C) 2026 Benoît LELEVÉ.

Source-available under the [Elastic License 2.0](LICENSE) (ELv2).

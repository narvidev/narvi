# Boundaries design: capability registry, knowledge seam, composition root, UI slot

Status: design note, 2026-09-04. Not a plan amendment; the `docs/TECHNICAL_PLAN.md`
section is separate work. Naming is fixed: this repository is **Narvi**, the product;
the composed module is **Narvi Gatekeeper** (separate repository `narvidev/enterprise`,
module codename `khazad`). Nothing here, in code or copy, calls this repository
"core" or a "community edition".

This note designs four seams. Each section states the decision, the concrete Go
shapes and where they live, what is structural versus documentary, the tests that
prove it, the alternative rejected, and the questions the code could not settle.

## 0. The constraint that shapes all four: Go's `internal` rule

`github.com/narvidev/enterprise` cannot import any
`github.com/narvidev/narvi/internal/...` path — the Go tool refuses it for any
importer outside the tree rooted at `github.com/narvidev/narvi`. Today the only
non-`internal` Go packages this module exposes are `cmd/*`, `contracts/*`,
`migrations`, and `tools/*`. Every port in `internal/app/ports`, every domain type,
`platform.Config`, `auth.Middleware`: unreachable by path.

Two consequences, applied throughout:

1. **Two new non-`internal` packages.** `github.com/narvidev/narvi/controlplane`
   (the composition root, item 3) and `github.com/narvidev/narvi/extension` (the
   module-facing façade). `extension` is a leaf package that *re-exports through
   type aliases* exactly the internal types a private module needs
   (`type KnowledgeRanker = ports.KnowledgeRanker`, etc.). An alias declared in a
   non-internal package is usable from any module; alias identity is preserved, so
   a private type implementing the aliased interface satisfies the internal one.
   Transitive imports of `internal/...` by `extension` are legal (the rule is on
   the importing file's own import paths, not on what a dependency imports).
2. **The crossing set is explicit and small.** Every type that crosses to the
   private module is named in `extension`, and growing that set is a deliberate
   public-repo PR. This is the property that lets an analyzer (item 1) and an
   arch-test (item 3) pin the whole boundary.

Direction of dependencies, enforced by the analyzer in section 1.4:
`controlplane` -> `extension` -> `internal/...`. No `internal` package may import
`extension` or `controlplane`.

Tooling that hardcodes today's layout and must move with it, all in the same PR as
item 3: `internal/ops/guidedrift_test.go` (`ScanRegisteredRoutes(root/cmd/control-plane)`),
`internal/ops/stepref_test.go` and `internal/ops/sectionref_test.go` (scan roots),
`internal/ops/drift_test.go` (instrument roots),
`tools/lint/narvichecks/httpclientban/httpclientban.go`
(`compositionRootDir = "/cmd/control-plane/"`).

## 1. Capability registry (item B1)

### 1.1 Decision

A single point of truth, `Enabled(capability) bool`, computed from three facts: a
capability is **installed** (a composed module supplied its implementation),
**licensed** (the verified key grants it), and the grant is **valid now** (per-call
clock check). Absent, malformed, unparseable, unsigned, expired or not-yet-valid
keys all yield `false` for every capability, silently for the public product and
with a boot-time `Warn` in a private build. The public binary composes no module,
so `installed` is empty and `Enabled` is `false` for everything regardless of any
key: a key can entitle, it can never *create* behavior.

Capability decisions are made in exactly two kinds of place: the composition root
(choosing which implementation to wire into an app-layer seam) and an HTTP
route-group gate (`httpapi.RequireCapability`) on module routes. Nothing in
`internal/domain`, `internal/app` (other than the registry package itself) or any
adapter may consult the registry. That is enforced by an analyzer, not by review.

### 1.2 Shapes and locations

**`internal/domain/license`** (pure: stdlib only, no I/O, no `time.Now()`, takes
`now` as a parameter).

```go
package license

// Capability is the closed vocabulary. Adding one is a deliberate PR here;
// the wire enum in contracts/rest/v1/dtos.schema.json mirrors it exactly.
type Capability string

const (
    CapabilityOrganizationGovernance Capability = "organization_governance"
    CapabilityCompliance             Capability = "compliance"
    CapabilityKnowledgeRetrieval     Capability = "knowledge_retrieval"
)

var All = []Capability{CapabilityOrganizationGovernance, CapabilityCompliance, CapabilityKnowledgeRetrieval}

// Product is the audience every key must carry.
const Product = "narvi-gatekeeper"

type Keyset map[string]ed25519.PublicKey

func IssuerKeys() Keyset

type Grant struct {
    KeyID        string
    Subject      string
    Capabilities []Capability
    IssuedAt     time.Time
    NotBefore    time.Time
    ExpiresAt    time.Time
}

func (g Grant) Has(c Capability) bool

// ValidAt is the ONLY time check. nbfSkew tolerates a host clock that is
// behind the issuer's. It never widens ExpiresAt: a host clock that is ahead
// makes a key expire early, which is the safe direction, and there is no
// grace window after exp.
func (g Grant) ValidAt(now time.Time, nbfSkew time.Duration) bool

var (
    ErrMalformed         = errors.New("license: malformed key")
    ErrUnknownKey        = errors.New("license: unknown key id")
    ErrBadSignature      = errors.New("license: signature verification failed")
    ErrWrongProduct      = errors.New("license: key is not for this product")
    ErrUnknownCapability = errors.New("license: key names a capability this build does not define")
    ErrNotYetValid       = errors.New("license: key not yet valid (check the host clock)")
    ErrExpired           = errors.New("license: key expired")
)

// Parse checks syntax, key id, signature and product. It deliberately does
// NOT check time: the grant's window is re-evaluated on every call by the
// registry, so a key that expires mid-process stops enabling anything at the
// next call, not at the next restart.
func Parse(raw string, keys Keyset) (Grant, error)

// Verify is Parse plus ValidAt, for boot-time logging only.
func Verify(raw string, keys Keyset, now time.Time, nbfSkew time.Duration) (Grant, error)
```

**`internal/app/capability`** (app layer: may hold an injected clock).

```go
package capability

type State string

const (
    StateEnabled      State = "enabled"
    StateNotInstalled State = "not_installed"
    StateNotLicensed  State = "not_licensed"
    StateExpired      State = "license_expired"
    StateNotYetValid  State = "license_not_yet_valid"
    StateInvalid      State = "license_invalid"
)

type Registry struct { /* unexported: installed set, *license.Grant, parse error, now func() time.Time, nbfSkew */ }

func New(installed []license.Capability, grant *license.Grant, parseErr error, now func() time.Time, nbfSkew time.Duration) *Registry

// Enabled is the single point of truth. Nil-safe: a nil *Registry answers
// false for everything. Returns a bare bool -- there is no error for a caller
// to discard on the way to accidentally treating "unknown" as "enabled"
// (egressmode.Resolve's own reasoning).
func (r *Registry) Enabled(c license.Capability) bool

func (r *Registry) State(c license.Capability) State
func (r *Registry) ExpiresAt() *time.Time
```

`Enabled(c)` is exactly
`installed[c] && grant != nil && grant.Has(c) && grant.ValidAt(now(), nbfSkew)`.
There is no method that expresses a restriction; the type can only say "this
private implementation may be used".

**`internal/platform`**: `Config.LicenseKey string` from `NARVI_LICENSE_KEY`
(optional; `Load` does not validate its content — validation is the domain's, and a
bad key must never fail boot). `Timeouts.LicenseNotBeforeSkew` (default 5 minutes,
the `HMACWindow`/`GitHubAppJWTClockSkew` family). `platform` does not import
`license`.

**`extension`**: `type Capability = license.Capability`, the constants re-exported,
and

```go
// Capabilities is what a module receives. Satisfied by *capability.Registry.
type Capabilities interface {
    Enabled(Capability) bool
}
```

**`internal/adapters/inbound/httpapi`**: two files, neither on the analyzer's
allow-list — this package imports none of the three banned packages, by file or
by package (see section 1.4 for why a former *file*-level entry for exactly
these two was removed rather than kept or widened):

- `capabilities.go`: `GetCapabilities(build func() restdtos.CapabilitiesResponse)`
  for `GET /api/capabilities` (item 4). `build` is called inside the handler, once
  per request, never cached; `controlplane` closes over the registry and
  `gatekeeperInstalled` and hands this file only the already-assembled response.
- `requirecapability.go`: `RequireCapability(enabled func() bool)`, the exact
  shape of `RequireCloudIdentityCapability` (`cloudidentitycapability.go:60`): 503
  with a stable message when `!enabled()`, evaluated per request so an expiry
  mid-process closes the route on the next request. That precedent's argument
  applies verbatim — a capability that is off must be observable as off, never
  indistinguishable from a route that was never built. Only module routes are ever
  wrapped with it. `controlplane` supplies `enabled` as
  `func() bool { return reg.Enabled(c) }`.

**`controlplane`** (item 3) is where the key is parsed once, the boot log line is
written, and the registry is constructed with `installed` = the union of every
composed module's `Capabilities`.

### 1.3 Fail direction, exhaustively

| Key state | `Parse` | `Enabled(c)` | Boot log (private build) | Public build |
|---|---|---|---|---|
| absent | no grant | false | Info: no key; Gatekeeper capabilities disabled | silent |
| malformed / unknown kid / bad signature / wrong product / unknown capability | typed error | false | Warn naming the error | silent (Debug) |
| not yet valid beyond `nbfSkew` | grant kept | false until `nbf` | Warn: check the host clock | silent |
| expired | grant kept | false | Warn with `exp` | silent |
| valid, `c` not granted | grant | false | — | — |
| valid, `c` granted, not installed | grant | false | Info: licensed but not installed | n/a |
| valid, granted, installed | grant | **true** | Info: subject, expiry, capabilities | n/a |

Boot never fails on a key. The public product's behavior is identical in every row
because, with no module composed, `installed` is empty and no public code path
reads the registry (section 1.6, test 3).

### 1.4 What is structural: the `capabilityimportban` analyzer

**Why an analyzer and not an arch-test.** The requirement is that a future
contributor cannot silently put a capability check on a shadow-mode suppression
path. An arch-test scanning for `.Enabled(` selectors is a by-name call ban (the
`demotionsweep` shape) and can be dodged by a method value or a wrapper. An
**import ban** is harder to dodge: the only way for a FILE to reach the registry,
the grant, or the façade is for that file itself to import one of three packages,
and an import declaration is a syntactic fact. This is `execimportban`'s mechanism
with `demotionsweep`'s allow-list discipline. It runs in `make lint` and in
`lint-web-assets` like the other six.

**What it bans.** Any import of

- `github.com/narvidev/narvi/internal/domain/license`
- `github.com/narvidev/narvi/internal/app/capability`
- `github.com/narvidev/narvi/extension`

from any file outside: the allowed **packages**
`github.com/narvidev/narvi/controlplane`, `github.com/narvidev/narvi/extension`,
`github.com/narvidev/narvi/internal/app/capability`,
`github.com/narvidev/narvi/internal/domain/license` — matched on the exact Go
import path, never a filesystem substring, so a checkout under a directory that
happens to share one of those names cannot silently exempt every file in it; and
`_test.go` files (a test constructing a registry is not a production decision
point — `demotionsweep.skipFile`'s reasoning).

**There is no file-level entry on this allow-list — and there used to be one.**
`internal/adapters/inbound/httpapi/capabilities.go` and `requirecapability.go`
were each individually exempted by name, inside `httpapi`, a package this
analyzer otherwise bans like any other. That is dodgeable, and an exit audit of
this guarantee reproduced it directly: Go import declarations are per FILE, but
identifier scope is per PACKAGE, so a package-level `var`/`func` either exempted
file declared was callable — with no import of its own — from any OTHER file in
`httpapi`, including `scmcredentials.go`, a real §30 suppression path in that
same package. The reproduction added a package-level helper closing over a
`*capability.Registry` to `capabilities.go`, then called it from
`scmcredentials.go` with no new import there; `go build`, `go vet` and this
analyzer all passed, and a licence state had become an input to that file's own
shadow-substitution decision with nothing reporting it. The fix was not to widen
the allow-list (a second file-level entry is the same shape of hole) or to move
the two files into their own sub-package (a package-level entry then, still
reachable from every file inside it): it was to make `httpapi` need neither
banned import at all. `RequireCapability` and `GetCapabilities` (section 1.2
above) each now take an already-evaluated decision — a `func() bool`, and a func
building the response DTO, respectively — injected by `controlplane`, so neither
file, nor any other file in that package, ever holds a `*capability.Registry` or
a `license.Capability` for a sibling file to reach through.

Diagnostic text names the requirement: "a capability check is a decision point at a
system boundary; the shadow-mode suppression guarantees are structural and must
never depend on one — wire the private implementation from the composition root
instead".

**What this buys specifically.** `internal/app/egressmode`, `shadowledger`,
`shadowscm`, `shadowslack`, `shadowlinear`, `shadowoperator`, `readonlymint`,
`repodemotion`, `outboxworker`, `sessionactor`,
`internal/adapters/outbound/githubapi` (the transport gate) and
`httpapi/scmcredentials.go` can none of them import the registry, so no suppression
decision — the transport gate's `resolve`, the decorator's `isLive`,
`OutboxStore.Create`'s stamp, the mint substitution, the push short-circuit — can
take a license state as an input. `egressmode.Resolve`'s inputs stay exactly
`Deps{RepoSettings, PlatformShadow}`. A future 12th seam is covered the same way,
because the ban is on the registry's importers, not on a list of suppression sites.

**Second structural half: the type.** `Registry` has `Enabled` and `State` and
nothing else. There is no `Disabled`, no `Deny`, no way to express "turn a public
behavior off". Every consumer is, by construction,
`if reg.Enabled(c) { private } else { public }` and the `else` branch is the wiring
that exists in the public binary.

**What stays documentary.** The private module is outside this analyzer's reach. It
receives `extension.Capabilities` and may consult it wherever it likes; a private
route that forgets `RequireCapability` is the private repo's defect. The enterprise
repo therefore runs the same analyzers with the same directory layout.

### 1.5 The license key, concretely

- **Primitive: Ed25519** (`crypto/ed25519`, stdlib). Rejected: RS256 JWT (a license
  is not a JWT — no `alg` header to confuse, no library, a 64-byte signature);
  HMAC (a shared secret would ship in the public binary and let anyone mint keys —
  the signature must be asymmetric).
- **Format:** `narvi1.<base64url(payload JSON)>.<base64url(signature)>`. The
  signature is over the bytes `"narvi1." + payloadB64` (domain-separated by the
  version prefix, so a `narvi2` payload can never verify under `narvi1` rules). The
  format version is in the prefix, not a header, mirroring `oidcsigning.Verify`'s
  "hardcode the algorithm, never trust the token to name it".
- **Claims** (all required): `kid`, `sub` (opaque deployment id), `product`
  (`"narvi-gatekeeper"`), `iat`, `nbf`, `exp` (Unix seconds), `caps` (every name
  must be in `license.All` for the deployed build, else `ErrUnknownCapability`).
  No seat counts in v1. Nothing about the deployment's data. No host binding in v1
  (decided: a self-hosted deployment legitimately changes hostname, and binding
  converts a routine migration into a support incident).
- **Where the public key lives:** `internal/domain/license/keys.go`, a literal
  `Keyset` of `kid -> 32 bytes`. Public keys are public; embedding them is the whole
  of "offline verification, no phone-home". The signing seed never exists in this
  repository; the signing tool and its key custody are the private repo's.
- **Rotation:** kids, overlapping. A key issued under a kid a deployed binary does
  not know is `ErrUnknownKey` (fail-closed), so a new kid ships in a public release
  *before* keys are issued under it.
- **Clock:** the registry holds `now func() time.Time` injected by the composition
  root; domain functions take `now` explicitly. `nbfSkew` widens `nbf` only. A host
  clock behind by more than the skew yields "not yet valid" and a boot `Warn`; a
  host clock ahead expires the key early. Both fail closed. No post-`exp` grace.
- **Delivery:** `NARVI_LICENSE_KEY` env var only in v1 — the same channel as every
  other deployment fact.
- **What tampering buys:** editing `keys.go` in a self-hosted public build enables
  nothing — the public binary contains no private implementation. In a private
  build it is the license circumvention ELv2 forbids; a legal control, named as
  such.

### 1.6 Tests

1. `license`: `TestParse` over {absent, malformed prefix, wrong part count, bad
   base64, bad JSON, unknown kid, bad signature, wrong product, unknown capability,
   valid}; `TestGrant_ValidAt` over {before nbf beyond skew, within skew, at nbf,
   between, at exp, after exp} with an explicit `now`;
   `TestIssuerKeys_AreWellFormed`; `TestIssuerKeys_RejectTestSignedKeys` (a key
   signed by the test pair fails against the production keyset — the negative that
   matters).
2. `capability`: `TestEnabled_Matrix` — {nil registry, no grant, invalid, expired,
   not-yet-valid, granted-not-installed, installed-not-granted, both} x `All`;
   exactly one cell is `true`. `TestEnabled_ExpiryMidProcess` with a fake clock
   proves per-call re-evaluation. `TestState_MatchesEnabled`.
3. `controlplane`: `TestBuild_WithoutModules_NeverConsultsCapabilities` (counting
   stub registry, zero modules, assert zero `Enabled` calls);
   `TestBuild_BadKey_BootsAsNarvi` (a malformed key yields the same route table as
   no key).
4. `capabilityimportban`: `analysistest` fixtures — a violation in
   `internal/app/shadowscm`, in `internal/app/sessionactor`, in
   `internal/domain/review`, and (the fixture for section 1.4's own
   file-level-exemption bridge) in a file inside the `httpapi` fixture package
   itself; silence in `controlplane`, `extension`, `internal/app/capability`,
   `internal/domain/license`, in `httpapi/capabilities.go`/`requirecapability.go`
   (silent only because, like the real files, they import nothing banned any
   more), and in a `_test.go`. `TestSkipFile_NoFileLevelExemption` pins that no
   file in `httpapi` is exempt by
   name.
5. `httpapi`: `TestRequireCapability_503WhenDisabled`, `_PassesWhenEnabled`,
   `_ReEvaluatesPerRequest`; `TestGetCapabilities_CallsBuildPerRequest` pins the
   same "evaluated per request, never cached" property for the read model.

### 1.7 Rejected alternatives

- **A capability flag in `repo_settings` or a feature-flag table.** Reintroduces a
  per-row authority that Postgres does not own the truth of (a key does), and makes
  "absent = public behavior" a query-result property instead of a type property.
- **Gating inside app packages.** The requirement forbids scattered checks; the
  analyzer makes the alternative unwritable.
- **Boot refusal on an invalid key in a private build.** A private binary with a
  bad key is Narvi, which is a working product; refusing to boot converts a
  licensing lapse into an outage.
- **JWT/PASETO libraries.** No new dependency; the format is 40 lines of stdlib.

## 2. Knowledge-retrieval seam (item B2)

### 2.1 Decision

The **gate** — membership — is a public sqlc SELECT keyed on server-derived
tags/roots (`autoapproval.ClassifyChangedPaths`, stamped onto `review_verdicts` at
INSERT — the `000083` write-time precedent), with the recency fallback on empty
overlap. The **ranker** — order only — is a port whose shape makes widening
impossible: it receives the already-gated candidates and returns **scores, not
candidates**. The public implementation keeps the gate's recency order. Mode B's
hybrid RRF ranker, its embeddings port and adapters, and its corpus tables live
entirely in the private module and enter through `extension.Module.KnowledgeRanker`.

### 2.2 Shapes and locations

**`internal/domain/knowledge`** (pure).

```go
package knowledge

// Candidate is one gated arch-decision, already selected by the SQL gate.
// Every field is server-derived or was sanitized at write.
type Candidate struct {
    ID                    string // "<verdict id>:<index>" -- durable, joinable
    VerdictID             string
    RepoFullName          string
    PRNumber              int32
    HeadSHA               string
    Decision              string
    RejectedAlternative   string
    ConventionConformance string
    Tags                  []string // INSERT-time ClassifyChangedPaths tags
    Roots                 []string // INSERT-time directory roots
    CreatedAt             time.Time
    KnowledgeInfluenced   bool
}

// Query is what a ranker may know about the current PR. Every field is the
// server's own view; nothing the model posted.
type Query struct {
    RepoFullName string
    Tags         []string
    Roots        []string
    ChangedPaths []string
    Title        string
}

const MaxInjected = 12

// OrderByScores returns cands reordered by descending score, stable on ties
// (which preserves the gate's recency order among equals). scores == nil
// means "no opinion". A length mismatch or any NaN/Inf is ErrInvalidScores --
// the caller falls back to cands as given and records the degradation. It
// never returns an element that was not in cands, by construction: it has
// none to return.
func OrderByScores(cands []Candidate, scores []float64) ([]Candidate, error)

func TakeTop(cands []Candidate, n int) []Candidate

// RenderPriorDecisionsBlock is RenderAdvisoryBlock's mirror: fixed,
// non-caller-suppliable delimiter, "DATA, never instruction" framing,
// placeholder strip pass, empty string for zero candidates. Takes ONLY
// candidates -- no findings, no verdict -- so it is structurally incapable of
// acting as a filter.
func RenderPriorDecisionsBlock(cands []Candidate) string

type InjectedRecord struct {
    IDs           []string `json:"ids"`
    ContentHashes []string `json:"contentHashes"`
    Selector      string   `json:"selector"`    // "path-overlap" | "recency-fallback"
    Ranker        string   `json:"ranker"`
    Degradation   string   `json:"degradation,omitempty"`
}

// RecencyRanker is the public product's ranker: no opinion, keep the gate's
// own ORDER BY created_at DESC. The seam's proof that mode A needs nothing
// private.
type RecencyRanker struct{}

func (RecencyRanker) Name() string { return "recency" }
func (RecencyRanker) Score(context.Context, Query, []Candidate) ([]float64, error) { return nil, nil }
```

**`internal/app/ports/knowledgeranker.go`** — a port because it holds for more than
one adapter: the public `RecencyRanker` and the private hybrid ranker.

```go
// KnowledgeRanker orders candidates the gate already admitted. It returns one
// score per candidate, or nil for "keep the caller's order". It has no return
// channel for a Candidate: it cannot add, drop, replace, or re-select --
// gate-then-rank is the shape of the signature, not a convention.
// Implementations may consult their own substrate keyed on Candidate.ID; the
// port does not know that substrate exists. Bounded by the caller's context; a
// slow ranker degrades to the gate order, never to empty.
type KnowledgeRanker interface {
    Name() string
    Score(ctx context.Context, q knowledge.Query, cands []knowledge.Candidate) ([]float64, error)
}
```

**`internal/app/reviewcontext/archdecisions.go`** — the exact sibling of
`FetchFalsePositivePatterns`: impure fetch here, pure render in the domain,
best-effort, never returns an error of its own.

```go
type ArchDecisionsFetcher interface {
    // ListGatedArchDecisions is the GATE: verdicts whose INSERT-time tags/roots
    // overlap (tags, roots), live-epoch only, uncontested, ORDER BY created_at
    // DESC LIMIT k. Mode-invariant.
    ListGatedArchDecisions(ctx context.Context, repoFullName string, tags, roots []string, k int32) ([]knowledge.Candidate, error)
    ListRecentArchDecisions(ctx context.Context, repoFullName string, k int32) ([]knowledge.Candidate, error)
}

// FetchPriorArchDecisions: gate -> score -> order -> take N -> render ->
// record. The caller PREPENDS block to the prompt before RenderTurnPrompt,
// exactly like its two siblings, and persists rec onto the turn.
func FetchPriorArchDecisions(ctx context.Context, logger *slog.Logger, fetcher ArchDecisionsFetcher, ranker ports.KnowledgeRanker, timeouts platform.Timeouts, q knowledge.Query) (block string, rec knowledge.InjectedRecord)
```

Internals, in order: `ListGatedArchDecisions`; if empty, `ListRecentArchDecisions`
and `Selector = "recency-fallback"`; `ranker.Score` under
`context.WithTimeout(ctx, timeouts.KnowledgeRankerTimeout)`; on error/timeout keep
the gate order and set `Degradation`; `OrderByScores`; `TakeTop(MaxInjected)`;
`RenderPriorDecisionsBlock`; fill `rec`. A fetch failure logs and returns
`"", rec{Degradation: "fetch-error"}` — no block, the fail-safe.

**The three seams** gain one call each, placed as the *first* prepend:
`internal/adapters/inbound/github/handler.go`,
`internal/adapters/inbound/httpapi/reviewretrigger.go`,
`internal/app/sessionactor/reviewretrigger.go` (`composeAutoRetriggerPrompt`). Each
takes the ranker from its existing `Config`/`Deps`/`RegistryOptions`, wired once in
the composition root.

**`extension`** re-exports: `type KnowledgeRanker = ports.KnowledgeRanker`,
`type KnowledgeQuery = knowledge.Query`,
`type KnowledgeCandidate = knowledge.Candidate`.

**Composition** (`controlplane/modules.go`): the public default is
`knowledge.RecencyRanker{}`. When a module supplies a ranker, the composition root
wraps it:

```go
// capabilitySwitchRanker consults the registry PER CALL, so a license that
// expires mid-process reverts to the public ranker at the next review without
// a restart, and rec.Ranker records which one actually ran.
type capabilitySwitchRanker struct {
    reg     *capability.Registry
    private ports.KnowledgeRanker
    public  ports.KnowledgeRanker
}
```

This wrapper is the only place `CapabilityKnowledgeRetrieval` is ever read for
ranking, and it lives in the one package the analyzer allows.

### 2.3 How the shape makes widening impossible

- A ranker's only output is `[]float64`. There is no `[]Candidate` return, no
  `Query` mutation (passed by value), no callback that could inject.
  `OrderByScores` builds its result by indexing into the caller's own slice.
- N is applied *after* ordering by `TakeTop(MaxInjected)` in the caller; a ranker
  cannot raise it.
- Selection and fallback are decided before the ranker is called and recorded in
  `rec.Selector`; the ranker cannot change which branch ran.
- The gate query excludes shadow-epoch and contested rows in SQL before any ranker
  sees a row.
- Degradation is monotone toward the public order, never toward empty, and stamped
  on the record so the A/B never counts a degraded turn as the B arm.

### 2.4 Step 107 with zero enterprise code present

Everything above except the switch wrapper's private branch is public. Step 107
ships: the two sqlc queries, `internal/domain/knowledge`, `ports.KnowledgeRanker`,
`reviewcontext.FetchPriorArchDecisions`, the three seam calls, the INSERT-time
tags/roots stamp, the mode buffer and stamps, wired with `RecencyRanker{}`. With no
module the hook is `nil` and the public ranker is used. Nothing in the private repo
needs to exist.

Step 108, if engaged, is private-only: `ports.Embeddings` and its adapter, the
corpus migrations (contributed through `extension.Module.Migrations`), the
ingestion watermark and provenance state, RRF fusion, and a `KnowledgeRanker` whose
`Score` returns `nil` for a repository whose mode is not B. Should the owner later
open the mode B substrate, the port and the domain RRF move here unchanged; the
seam does not care where the ranker is compiled.

### 2.5 Tests

1. `knowledge`: `TestOrderByScores` — nil scores keep order; equal scores keep
   order (stability = recency preserved); length mismatch and NaN return
   `ErrInvalidScores`; the result is a permutation of the input (assert multiset
   equality by ID — the widening-impossible property as a test); `TestTakeTop`;
   `TestRenderPriorDecisionsBlock_Empty/_Delimited/_StripsPlaceholders`;
   `TestRecencyRanker_NoOpinion`.
2. `reviewcontext`: `TestFetchPriorArchDecisions` over {gate hit, empty overlap ->
   fallback, fetch error -> empty block, ranker error -> gate order + degradation,
   ranker timeout, invalid scores}, asserting `rec` each time.
3. Arch-test: `TestKnowledgeRankerImplementationsReturnScoresOnly` — parse every
   type implementing `KnowledgeRanker` and assert `Score`'s result types are
   exactly `([]float64, error)`; trivially true today, and what makes a future
   "helpful" `Score(...) ([]Candidate, error)` a CI failure rather than a review
   catch.
4. Seam pinning: add the new delimiter to the placeholder-drift test's expected set.
5. Integration: the gate query against a real Postgres — overlap match, no match ->
   fallback, shadow-epoch row excluded, contested row excluded.

### 2.6 Rejected alternatives

- **Ranker returns `[]Candidate`, caller verifies subset.** Verification is a check
  someone can weaken; a scores-only signature has nothing to weaken.
- **Ranker as a full retriever with the gate applied afterwards.** Inverts
  gate-then-rank: the private side would decide membership first and the public
  side would filter — the gate becomes a filter over attacker-steerable output and
  the A/B loses its single-variable property.
- **Putting `KnowledgeRanker` in the domain instead of `ports`.** It is an I/O
  boundary with two implementations; ports is where those live.

## 3. Composition-root extraction (item B3)

### 3.1 Decision

Move the composition root, verbatim, out of `cmd/control-plane` into a new
non-internal package `github.com/narvidev/narvi/controlplane`, keep
`cmd/control-plane/main.go` as a one-line `main`, and inject modules through an
explicit struct of nil-able hooks defined in `github.com/narvidev/narvi/extension`.
The private binary is then genuinely thin:

```go
package main

import (
    "os"

    "github.com/narvidev/enterprise/khazad"
    "github.com/narvidev/narvi/controlplane"
)

func main() { os.Exit(controlplane.Main(os.Args, khazad.Module())) }
```

`cmd/control-plane/main.go` (~2536 lines) becomes:

```go
package main

import (
    "os"

    "github.com/narvidev/narvi/controlplane"
)

func main() { os.Exit(controlplane.Main(os.Args)) }
```

Two PRs, in this order: (1) `refactor(controlplane): extract the composition root`
— a pure move, no hooks, with the route-table golden captured *before* the move;
(2) `feat(extension): module hooks` — adds `extension` and the insertion points.
PR 1's golden proves PR 2 adds nothing to the public router.

### 3.2 Shapes

```go
package controlplane

// Main dispatches "serve" | "seed" exactly as cmd/control-plane's main did,
// returning the exit code instead of calling os.Exit, so it is testable.
func Main(args []string, modules ...extension.Module) int

// Build is serve()'s wiring with the listening and the signal handling
// removed: every store, adapter, decorator, route group, notifier map and
// background loop, plus the module hooks, assembled against an already-open
// pool. Boot pre-flights that are not wiring (the GitHub App scope check,
// migrations) run in Main before Build.
func Build(ctx context.Context, cfg *platform.Config, pool *pgxpool.Pool, modules ...extension.Module) (*App, error)

type App struct {
    Router chi.Router
    // unexported: registry, workers, shutdown hooks
}

// Run listens on addr and runs every background loop through ONE errgroup
// with the same context.Canceled carve-out serve() has today.
func (a *App) Run(ctx context.Context, addr string) error

// Routes returns every "METHOD /path" chi.Walk finds, sorted -- the
// characterization test's subject.
func (a *App) Routes() []string
```

Files: `serve.go` (today's `serve()` body, split into `Main`/`Build`/`Run` with no
other edits), `seed.go`, `migrate.go`, `health.go`, `boot.go`, `modules.go`, plus
the moved `main_test.go` content.

```go
package extension

// Module is everything a composed module can contribute. Every field is
// optional; a zero Module contributes nothing. A struct, not an interface: the
// public repo adds hooks additively (a new field's zero value is "absent")
// without breaking a private module compiled against an older shape, and the
// composition root validates the combination at boot.
type Module struct {
    // Name is a [a-z0-9-]+ identifier; routes mount under /api/ext/<Name>/ and
    // migrations use the table "<Name>_schema_migrations".
    Name string

    // Capabilities is the INSTALLED set this module implements. A module that
    // lists a capability must supply its implementation, and vice versa --
    // Build refuses to boot otherwise.
    Capabilities []Capability

    // Migrations, when non-nil, is applied after the public chain against the
    // same database, tracked in the module's own migrations table so two
    // chains never share a version counter. Applied regardless of license
    // state: schema presence is not licensed behavior.
    Migrations fs.FS

    KnowledgeRanker KnowledgeRanker

    // Mount registers the module's routes on r, already mounted at
    // /api/ext/<Name>/ behind RequireAuth; the module applies
    // rt.RequireCapability per route group. Under /api/ so the SPA fallback's
    // protected prefixes cover it.
    Mount func(r chi.Router, rt Runtime)

    // Workers are run through the composition root's errgroup with the same
    // shutdown semantics as every public loop.
    Workers func(rt Runtime) []Worker

    // WebAssets, when non-nil, replaces webui.DistFS.
    WebAssets fs.FS
}

type Worker interface {
    Run(ctx context.Context) error
}

// Runtime is what a module receives. Only external types and this package's
// own aliases -- growing it is a deliberate PR.
type Runtime struct {
    Pool              *pgxpool.Pool
    Logger            *slog.Logger
    Capabilities      Capabilities
    RequireAuth       func(http.Handler) http.Handler
    RequireCapability func(Capability) func(http.Handler) http.Handler
    PublicBaseURL     string
    Audit             func(ctx context.Context, actorUserID string, action, targetType, targetID string, detail map[string]any) error
}

type Actor = authz.Actor
func ActorFromContext(ctx context.Context) (Actor, bool)
```

**Insertion points in `Build`**, each a few lines: after public migrations (module
migrations, `MigrationsTable: name + "_schema_migrations"`); before the router
(`license.Parse` + `capability.New`); at `webui.Mount(router, assets)`; after the
last public route group (`router.Route("/api/ext/"+m.Name, ...)` with
`auth.Middleware`); at the errgroup; at the ranker wiring.
`GET /api/capabilities` is registered by the public root unconditionally.

### 3.3 What stays in `cmd/control-plane`

`main.go` with the one-line `main`. Nothing else. `make dist` still builds
`./cmd/control-plane` with `-tags web_assets`; `webui`'s build-tag pair is
untouched.

### 3.4 Tooling that must move in PR 1

- `internal/ops/guidedrift_test.go`: `ScanRegisteredRoutes(root/controlplane)` —
  otherwise the guide-drift test finds zero routes and every documented command
  "drifts".
- `internal/ops/stepref_test.go`, `sectionref_test.go`, `drift_test.go`: add
  `controlplane` and `extension` to the roots.
- `tools/lint/narvichecks/httpclientban/httpclientban.go`:
  `compositionRootDir = "/controlplane/"`.
- `internal/adapters/inbound/webui/mount_test.go` and `doc.go` comments naming
  `cmd/control-plane/main.go`; likewise
  `httpapi/cloudidentitycapability.go`'s comment.

### 3.5 Proof that the public binary is unchanged

1. **Route-table golden.** Before the move, generate
   `controlplane/testdata/routes.golden` from
   `ops.ScanRegisteredRoutes("cmd/control-plane")` (the AST scanner already in the
   repo). After the move, `TestBuild_RouteTableMatchesGolden` builds with no
   modules and asserts `App.Routes()` equals the golden.
   `TestBuild_WithModules_AddsOnlyExtRoutes` asserts the delta with a fake module
   is exactly `/api/ext/<name>/...`.
2. **Outbox classification unchanged.** `NewBuilder` still receives the finished
   map; a boot with the map as built today passes its own refusal check.
3. **Registry-independence.** `TestBuild_WithoutModules_NeverConsultsCapabilities`.
4. **Binary parity.** CI builds both `cmd/control-plane` and, in the enterprise
   repo, the private main against a pinned public commit; `make dist` and
   `lint-web-assets` unchanged.
5. **Behavioral parity beyond routes** is the existing suite: every integration
   test that boots handlers does so through the same constructors `Build` calls.

### 3.6 Rejected alternatives

- **Functional options.** Hides the extension surface in a growing list of option
  constructors; nothing enumerates "what can a module do", and an option taking an
  internal type is unusable from the private module anyway.
- **A global registry populated by `init()` in the private module.** Exactly the
  silent side-effect the audit history warns against: a module that registers
  itself invisibly can also un-make a guarantee invisibly; the composition root
  could not validate the combination at boot.
- **`internal/controlplane` plus a thin exported shim.** The shim's parameters must
  be non-internal types, so the façade is needed regardless; two layers for one job.
- **Splitting `serve()` into per-area wiring files now.** Worth doing, but a second
  refactor whose diff would hide PR 1's "pure move" property. Later.

## 4. UI extension slot (item B4)

### 4.1 Decision

One new derived read model, `GET /api/capabilities`, drives everything the public
SPA renders about Narvi Gatekeeper. The SPA gains a runtime slot registry with
named slot ids; in the public bundle nothing registers, so every slot renders
either nothing or an honest affordance stating that the capability is part of Narvi
Gatekeeper and why it is unavailable here. Real Gatekeeper screens are separate
later work (section 4.5); this seam lands the three things that work must not have
to retrofit.

### 4.2 Wire shape (`contracts/rest/v1/dtos.schema.json`, three new `$defs`)

`CapabilityState` (enum: `enabled`, `not_installed`, `not_licensed`,
`license_expired`, `license_not_yet_valid`, `license_invalid` — matching
`capability.State` exactly, so the screen can say which);
`CapabilityStatus` (`name` enum matching `license.Capability`, `state`);
`CapabilitiesResponse` (`gatekeeperInstalled` bool — whether any module is composed,
never which one; `licenseExpiresAt` nullable date-time — never the key, never the
subject; `capabilities` array). All `additionalProperties: false`.

`licenseExpiresAt` is a nullable date-time, so `tools/contractspatch` aliases its
generated pointer type and `TestGeneratedNullablePointerTypesAreAliases` guards it.
The `Integration` DTO's rule applies verbatim: a derived read, never the secrets
themselves in any form.

Handler `httpapi.GetCapabilities`, mounted at `/api/capabilities` behind
`auth.Middleware`, authorized for every role including viewer. Rows are emitted for
every `license.All` entry in fixed order.

### 4.3 SPA side

- `api/endpoints.ts`: `getCapabilities(signal)`; `api/queryKeys.ts`:
  `capabilityQueryKeys.list()`; `ext/useCapabilities.ts`: a `useQuery` with a long
  `staleTime` (the value changes at restart or at expiry).
- `ext/slots.ts`: a closed list of slot ids as a TypeScript union,
  `registerSlot(id, Component)` writing into a module-level map, and
  `<SlotOutlet id fallback>` rendering the registered component when present, else
  `fallback`. In the public bundle the map stays empty forever.
- `ext/GatekeeperAffordance.tsx`: three sentences — "Part of Narvi Gatekeeper. This
  build does not include it." / "…included in this build, but no valid license is
  configured." / "…The license expired on {date}." Copy never cites a section
  number (`internal/ops/operatorcopy.go` scans JSX text) and never names a Step.
  The idiom is `sign-in.tsx`'s "SSO becomes available once your organization
  configures it."
- Placement: **no ninth Settings tab** — the eight-tab mockup keeps
  screenshot parity. The affordance rides the existing card column of
  `RepoSettingsView.tsx`, after `ShadowLedgerCard`, behind
  `<SlotOutlet id="repo-settings.governance" …>`.

### 4.4 Tests

`httpapi`: `TestGetCapabilities_EveryRole` (viewer included),
`_NeverCarriesTheKey` (the response body contains no substring of the configured
key), `_StatesMatchRegistry`. SPA (vitest): `slots.test.ts` (unregistered renders
fallback; registered renders component; registering twice is an error),
`GatekeeperAffordance.test.tsx` (three states -> three sentences; none contains a
section sign). `operatorcopy` covers the copy mechanically.

### 4.5 The harder question: private Gatekeeper screens

**Out of scope for this seam, as separate later work.** A real Gatekeeper screen
needs a bundle built from the public `web/` sources plus private components,
embedded in the private binary. That is a build-pipeline design (a private Vite
project consuming `web/` as a dependency, its own `outDir`, its own `go:embed`,
route-tree composition, its own DTO guard) and must not be folded into a
capability-affordance change.

**What this seam must not preclude, and therefore does now:**

1. `extension.Module.WebAssets fs.FS`. `webui.Mount(r chi.Router, assets fs.FS)` is
   already generic over `fs.FS`; the composition root passes the module's assets
   when present. Five lines today; the future bundle drops in with no public-repo
   change.
2. `main.tsx` refactored into an exported
   `bootstrap(options: { slots?: Partial<Record<SlotId, Component>> })` that builds
   the QueryClient, router and providers exactly as today, with `main.tsx` reduced
   to `bootstrap({})`.
3. The slot registry is runtime, not build-time conditional imports, and slot ids
   are the only coupling; the wire read model is the one both the affordance and
   the private screens consult, so they cannot disagree about state.

**Named for the later work, not designed here:** route additions (the file-based
route tree is per project; a private build needs virtual-route composition or a
merged routes directory); `web/package.json` `exports`/`files` so `web/` is
consumable as a dependency; the private project's own DTO-redeclaration guard;
pinning the public commit the bundle is built from.

### 4.6 Rejected alternatives

- **Folding capability state into `GET /api/me`.** Conflates identity with
  deployment facts and would force every consumer of `Member` to see licensing.
- **Build-time `import.meta.env` conditionals.** Puts the private module's
  existence into the public build configuration, and cannot represent "installed
  but not licensed".
- **Hiding the affordance entirely when not installed.** Dishonest by omission: the
  product's own rule is that a screen says what it cannot show.

## 5. Structural versus documentary, per item

| Item | Structural (a contributor cannot silently un-make it) | Documentary |
|---|---|---|
| 1 Registry | `capabilityimportban` analyzer; `Registry` has no deny method; the `installed AND licensed AND valid-now` conjunction; no error return; boot never fails on a key; `nbf`-only skew | The private module's own use of `RequireCapability`; key custody; the keyset |
| 2 Knowledge | Scores-only port; `OrderByScores` is a permutation by construction; the gate in SQL before any ranker; degradation never empty; the ranker-signature arch-test | Step 108's corpus provenance state (private) |
| 3 Composition | Route-table golden; registry-independence test; boot refusal on an inconsistent `Module`; per-module migrations table; `/api/ext/<name>/` under the protected prefix | `Runtime` surface growth; the enterprise repo running narvichecks |
| 4 UI slot | Wire enum mirrors the Go enum; `operatorcopy` on the affordance; the `WebAssets` hook | Slot placement; the private bundle pipeline |

## 6. Sequencing

1. `refactor(controlplane): extract the composition root` — item 3 PR 1, with the
   route golden and the tooling retargets.
2. `feat(extension): module hooks and the capability registry` — items 1 and 3 PR 2,
   with the analyzer.
3. `feat(contracts): capabilities read model and the SPA slot` — item 4.
4. Step 107 lands the knowledge seam (item 2) on its own schedule; it needs only
   the `KnowledgeRanker` hook from (2), and can precede it with `RecencyRanker`
   hardwired.

## 7. Decisions applied

Applied here rather than deferred: no host binding in the key (v1);
`NARVI_LICENSE_KEY` only, no Settings paste-in (v1); per-module migrations table,
never a shared version counter; the enterprise repo adopts this layout and runs the
public analyzers; no ninth Settings tab (mockup parity preserved; the affordance
rides the repo-settings card column); `license_*` gauges deferred until the
registry has shipped.

**The v1 capability vocabulary, decided:** `organization_governance`, `compliance`,
`knowledge_retrieval` — snake_case, matching this repository's own REST enum
convention (`not_a_pr`, `public_api`, `needs_human`, `data_layer`), not the
kebab-case `governance`/`knowledge-retrieval` placeholders this section used to
carry. Settled here, before item 4 shipped: the names enter a published wire enum
the moment that item ships, and changing an enum value afterward is a breaking
contract change.

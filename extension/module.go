// This file (module.go) defines Module -- everything a composed module
// can contribute to the composition root -- and the two supporting
// shapes, Worker and Runtime, a module needs to contribute it.

package extension

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Module is everything a composed module can contribute. Every field is
// optional; a zero Module contributes nothing. A STRUCT, deliberately not
// an interface: this repository can grow the set of hooks a module may
// implement additively (a new field's own zero value is simply "this
// module does not use it"), without breaking a private module compiled
// against an older version of this struct, and the composition root
// (controlplane.Build) validates the whole combination at boot rather
// than trusting each field in isolation.
//
// Rejected: a module registering itself through an init() side effect
// instead of a value passed explicitly to controlplane.Main/Build. A
// module that can appear invisibly can also un-make a guarantee
// invisibly, and the composition root could not validate the combination
// at boot at all (docs/design/boundaries-design.md, section 3.6).
type Module struct {
	// Name identifies this module: matched against `^[a-z0-9-]+$` by
	// controlplane.Build, which refuses to boot otherwise. Routes mount
	// under /api/ext/<Name>/, and migrations (below) are tracked in a
	// table named "<Name>_schema_migrations" -- both derived from this
	// one field, so it must be unique among every module composed into
	// the same binary.
	Name string

	// Capabilities is the INSTALLED set this module implements --
	// docs/design/boundaries-design.md, section 1.1's own "installed" input to
	// the capability registry's conjunction. Every entry must be a
	// capability this build defines (license.All); controlplane.Build
	// refuses to boot otherwise. CapabilityKnowledgeRetrieval carries one
	// further requirement of its own -- see KnowledgeRanker's own doc
	// comment immediately below.
	Capabilities []Capability

	// Migrations, when non-nil, is applied after Narvi's own public
	// migration chain, against the same database, tracked in this
	// module's own "<Name>_schema_migrations" table so the two chains
	// never share a version counter. Applied regardless of licence
	// state: schema presence is not licensed behavior (only what reads
	// or writes through it is).
	Migrations fs.FS

	// KnowledgeRanker, when non-nil, replaces the public product's own
	// recency ordering (internal/domain/knowledge.RecencyRanker) for this
	// module's own declared CapabilityKnowledgeRetrieval --
	// controlplane.Build wraps it so the capability registry is consulted
	// on every call, never once at boot, so an expiring licence reverts
	// to the public ranker at the very next review with no restart. This
	// field and CapabilityKnowledgeRetrieval are a BICONDITIONAL, both
	// halves enforced by controlplane.Build refusing to boot otherwise
	// (validateModules' own doc comment): a module declaring the
	// capability without supplying a ranker has advertised ranking
	// behavior it does not implement, and one supplying a ranker without
	// declaring the capability would run private ranking logic the
	// capability registry never gates. See the port's own doc comment
	// (internal/app/ports/knowledgeranker.go) for why its Score method
	// can only ever reorder the candidates a server-derived gate already
	// admitted, never add, drop, or replace one.
	KnowledgeRanker KnowledgeRanker

	// Mount registers this module's own routes on r, which is already
	// mounted at /api/ext/<Name>/ behind the same authentication as
	// every other API route (rt.RequireAuth) -- so a module route is
	// authenticated by construction, never by remembering to apply
	// rt.RequireAuth itself. A module gates any route that backs a
	// specific paid capability with rt.RequireCapability, per route
	// group; this repository's own capabilityimportban analyzer cannot
	// see into a private module, so a route that forgets that gate is
	// the private repository's own defect (docs/design/
	// boundaries-design.md, section 1.4's own "what stays documentary").
	Mount func(r chi.Router, rt Runtime)

	// Workers are run through the composition root's own errgroup, with
	// the same context-cancellation shutdown semantics as every public
	// background loop -- never a naked goroutine, never a separately
	// managed lifetime.
	Workers func(rt Runtime) []Worker

	// WebAssets, when non-nil, replaces the public web UI's own embedded
	// bundle (webui.DistFS) -- the seam a future private frontend bundle
	// (composed from the public web/ sources plus private components)
	// drops into with no further change to this repository.
	WebAssets fs.FS
}

// Worker is one background loop a Module contributes -- run through the
// composition root's own errgroup exactly like every internal background
// loop (reconciler, outbox delivery, ...): Run must return promptly once
// ctx is canceled, and a non-nil error other than context.Canceled fails
// that errgroup's own Wait, exactly as it would for an internal loop.
type Worker interface {
	Run(ctx context.Context) error
}

// Runtime is what a module receives, in both Mount and Workers. Every
// field is either a plain external type or one of this package's own
// aliases -- never an internal one -- so growing this struct is exactly
// as deliberate, and exactly as reviewable, as growing Module itself.
type Runtime struct {
	// Pool is the same, already-open *pgxpool.Pool the public binary's
	// own stores use.
	Pool *pgxpool.Pool

	// Logger is this process's own structured logger.
	Logger *slog.Logger

	// Capabilities lets a module ask whether one of ITS OWN declared
	// capabilities is currently enabled -- e.g. inside a Worker that
	// should idle, rather than run, while unlicensed.
	Capabilities Capabilities

	// RequireAuth is the SAME middleware (auth.Middleware) every public
	// route is already mounted behind, already applied to the router
	// Mount receives -- exposed here for a module that needs to apply it
	// a second time to a sub-router of its own, or to compose it with
	// RequireCapability in a specific order.
	RequireAuth func(http.Handler) http.Handler

	// RequireCapability returns middleware that closes an entire route
	// group with a 503 (httpapi.RequireCapability's own shape,
	// re-exported through this closure since httpapi is internal) unless
	// Capabilities.Enabled(c) -- evaluated per request, so an expiring
	// licence closes the group at the very next request, not merely at
	// the next restart.
	RequireCapability func(Capability) func(http.Handler) http.Handler

	// PublicBaseURL is the same externally reachable base URL
	// (platform.Config.PublicBaseURL) every public link-building call
	// site already uses.
	PublicBaseURL string

	// Audit records one audit_log row (technical plan §13.3's own
	// "written in the same transaction as the change" requirement does
	// NOT apply across this boundary -- a module's own transaction is its
	// own concern; this call is a single, independently-committed
	// INSERT). actorUserID is the acting user's id, or "" for a
	// system/bot-attributed action; detail is marshaled to JSON verbatim.
	Audit func(ctx context.Context, actorUserID string, action, targetType, targetID string, detail map[string]any) error
}

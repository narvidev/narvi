package sessionactor

import (
	"encoding/json"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// eventTypePassThroughs are the functions that store an event under a type
// they were handed rather than one they name: the two append helpers, and
// the sandbox ingress, which stores a frame under the frame's own type only
// after refusing serverEventTypes (handleSandboxEvent, appendTokenFrame).
var eventTypePassThroughs = map[string]bool{
	"internal/app/sessionactor/actor.go:appendRawEvent":            true,
	"internal/app/sessionactor/timerfired.go:appendEvent":          true,
	"internal/app/sessionactor/sandboxevent.go:handleSandboxEvent": true,
	"internal/app/sessionactor/tokenframe.go:appendTokenFrame":     true,
}

// TestServerWrittenEventTypesAreReserved keeps serverEventTypes whole, so a
// type the control plane starts writing cannot be left open to the
// sandbox socket. It type-checks the production packages and reads every
// event type they store: the type argument of the actor's appendEvent and
// appendRawEvent, and the Type field of a sqlcgen.CreateEventParams
// literal, each a constant or else handed through by one of
// eventTypePassThroughs. Every constant type must be a sandbox-ws event
// type (contracts/sandbox-ws/v1/events.schema.json) or reserved; every
// reserved type must be written somewhere and must not be a contract type
// -- reserving one would drop the agent's own frames of it.
func TestServerWrittenEventTypesAreReserved(t *testing.T) {
	t.Parallel()

	contract := sandboxWSEventTypes(t)
	written := map[string][]string{}
	passThroughs := map[string]bool{}
	for _, f := range productionFiles(t) {
		if f.info == nil {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			site := f.rel + ":" + fn.Name.Name
			record := func(typeExpr ast.Expr) {
				if value, ok := f.constString(typeExpr); ok {
					written[value] = append(written[value], f.fset.Position(typeExpr.Pos()).String())
					return
				}
				if !eventTypePassThroughs[site] {
					t.Errorf("%s: an event stored under a type that is not a constant: name the type, so TestServerWrittenEventTypesAreReserved can check it is reserved", f.fset.Position(typeExpr.Pos()))
					return
				}
				passThroughs[site] = true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if m, ok := f.method(sel); ok && m.is(sessionactorPkgPath, "Actor") && (m.name == "appendEvent" || m.name == "appendRawEvent") && len(n.Args) > 2 {
						record(n.Args[2])
					}
				case *ast.CompositeLit:
					if !isNamed(f.info.TypeOf(n), f.module+"/"+sqlcgenPkgPath, "CreateEventParams") {
						return true
					}
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Type" {
								record(kv.Value)
							}
						}
					}
				}
				return true
			})
		}
	}

	if len(written[SandboxStatusEventType]) == 0 {
		t.Fatalf("no write of %q found: the scan is broken", SandboxStatusEventType)
	}
	typeNames := make([]string, 0, len(written))
	for typ := range written {
		typeNames = append(typeNames, typ)
	}
	sort.Strings(typeNames)
	for _, typ := range typeNames {
		if !contract[typ] && !serverEventTypes[typ] {
			t.Errorf("event type %q (written at %v) is no sandbox-ws event type, so the control plane alone writes it: add it to serverEventTypes (sandboxstatus.go), or a sandbox can store one the page takes for the server's", typ, written[typ])
		}
	}
	for typ := range serverEventTypes {
		if contract[typ] {
			t.Errorf("serverEventTypes reserves %q, a sandbox-ws event type: the agent's own frames of it would be dropped", typ)
		}
		if len(written[typ]) == 0 {
			t.Errorf("serverEventTypes reserves %q, which no production code writes: the scan is broken, or the entry is stale", typ)
		}
	}
	for site := range eventTypePassThroughs {
		if !passThroughs[site] {
			t.Errorf("%s no longer stores an event under a type it was handed: the scan is broken, or eventTypePassThroughs is stale", site)
		}
	}
}

// isNamed reports whether typ is the named type pkgPath.name.
func isNamed(typ types.Type, pkgPath, name string) bool {
	named, ok := types.Unalias(typ).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath && named.Obj().Name() == name
}

// sandboxWSEventTypes reads the event types the sandbox-ws contract
// defines: each event $def's constant `type`.
func sandboxWSEventTypes(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sandboxStatusModuleRoot(t), "contracts", "sandbox-ws", "v1", "events.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties struct {
				Type struct {
					Const string `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, def := range schema.Defs {
		if def.Properties.Type.Const != "" {
			out[def.Properties.Type.Const] = true
		}
	}
	if len(out) < 20 {
		t.Fatalf("read %d sandbox-ws event types, want at least 20: the schema moved, or the read is broken", len(out))
	}
	return out
}

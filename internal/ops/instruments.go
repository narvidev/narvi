package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// RegisteredInstrument records where one metric instrument was
// registered — kept for error messages/debugging, not consulted by
// CheckDrift beyond the map key it lives under (instruments.go's own
// Name field duplicates the map key deliberately, so a RegisteredInstrument
// value is still self-describing if ever extracted from the map).
type RegisteredInstrument struct {
	Name string
	File string
	Line int
}

// instrumentMethods is the set of go.opentelemetry.io/otel/metric.Meter
// constructor method names this codebase's own established convention
// uses to register a new instrument (metric.Meter's own real method set,
// confirmed against every otel.Meter(...).<Method>(...) call site this
// repo has at the time this scanner was written: Int64Counter,
// Int64Gauge, Float64Histogram — see internal/ops/instruments_test.go's
// own doc comment for the verification). Every entry's own first
// argument is always the instrument's own string name, per the OTel API
// itself — this scanner relies on that being call-site position 0.
var instrumentMethods = map[string]bool{
	"Int64Counter":         true,
	"Float64Counter":       true,
	"Int64UpDownCounter":   true,
	"Float64UpDownCounter": true,
	"Int64Histogram":       true,
	"Float64Histogram":     true,
	"Int64Gauge":           true,
	"Float64Gauge":         true,

	// The asynchronous (observable) constructors take the instrument's
	// name first too -- controlplane's db_pool_* instruments, read from
	// pgxpool.Stat() at each collection, are the first to use them.
	"Int64ObservableCounter":         true,
	"Float64ObservableCounter":       true,
	"Int64ObservableUpDownCounter":   true,
	"Float64ObservableUpDownCounter": true,
	"Int64ObservableGauge":           true,
	"Float64ObservableGauge":         true,
}

// ScanRegisteredInstruments walks every .go file under each of roots
// (skipping _test.go files, exactly like tools/lint/narvichecks/
// notimeliteral's own skipFile — a test-only instrument, if one ever
// existed, is never real production telemetry a dashboard/alert should be
// allowed to reference), parses it via go/parser, and collects the
// string-literal instrument name from every call matching
// `<expr>.<Method>("name", ...)` where Method is one of
// instrumentMethods. A non-literal first argument (a computed string) is
// silently skipped rather than erroring — this codebase's own convention
// (confirmed by inspecting every real call site) is always a literal, and
// a future dynamic name would need this scanner's own attention anyway,
// not a hard failure blocking an unrelated build.
func ScanRegisteredInstruments(roots ...string) (map[string]RegisteredInstrument, error) {
	found := map[string]RegisteredInstrument{}
	fset := token.NewFileSet()

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("ops: parse %s: %w", path, perr)
			}

			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !instrumentMethods[sel.Sel.Name] {
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				name, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				pos := fset.Position(call.Pos())
				found[name] = RegisteredInstrument{Name: name, File: path, Line: pos.Line}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return found, nil
}

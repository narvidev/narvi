package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The packages ScanAutonomyFreezeSites reads by name.
const (
	autonomyGateImportPath   = "github.com/narvidev/narvi/internal/app/autonomy"
	autonomyDomainImportPath = "github.com/narvidev/narvi/internal/domain/autonomy"
	autonomyDomainDir        = "internal/domain/autonomy"
)

// autonomyGateReads are the Gate methods that read the freeze for a site
// about to act; autonomyGateMethods adds the ones a person's path must not
// call either.
var (
	autonomyGateReads   = map[string]bool{"Check": true, "FrozenTx": true}
	autonomyGateMethods = map[string]bool{"Check": true, "FrozenTx": true, "Frozen": true, "RecordSkip": true}
	// autonomyGateHolders are the names a Gate is held under -- a field or
	// a variable named gate, autonomy or Autonomy -- which is how this
	// name-based check tells a Gate's method from another type's.
	autonomyGateHolders = map[string]bool{"gate": true, "autonomy": true, "Autonomy": true}
)

// AutonomyFreezeCheck is one place an automatic-action site reads the
// autonomy freeze (technical plan §40.2), registered so the build fails
// when the read is removed.
type AutonomyFreezeCheck struct {
	// Site is the internal/domain/autonomy constant's name, e.g.
	// "SiteAutoMerge".
	Site string
	// File and Function are where the read is: the function or method
	// whose body reads the freeze through a Gate (Check or FrozenTx)
	// exactly Reads times.
	File     string
	Function string
	Reads    int
	// NamedInFile and NamedIn, when set, are where Site's constant is
	// named, when Function reads the site from a table rather than naming
	// it: a top-level declaration of NamedInFile. Otherwise Function's own
	// body names it.
	NamedInFile string
	NamedIn     string
	Reason      string
}

// AutonomyFreezeChecks registers every read of the freeze an
// automatic-action site makes. Every Site constant internal/domain/autonomy
// declares, but for the reserved ones, has at least one entry.
var AutonomyFreezeChecks = []AutonomyFreezeCheck{
	{
		Site: "SiteAutoMerge", File: "internal/app/automerge/worker.go", Function: "mergeCandidate", Reads: 2,
		Reason: "the auto-merge worker reads the freeze before a candidate's auth-guard reservation and live re-validation, and again right before its merge",
	},
	{
		Site: "SiteSentinelFixMerge", File: "internal/adapters/inbound/github/pullrequestevent.go", Function: "handlePullRequestClosed", Reads: 1,
		Reason: "the sentinel-fix merge gate reads the freeze once an allowed gate would merge, before the fresh stack read and the merge",
	},
	{
		Site: "SiteSentinelAutoFixSpawn", File: "internal/app/outboxworker/builder.go", Function: "attempt", Reads: 1,
		NamedInFile: "internal/app/outboxworker/freeze.go", NamedIn: "notificationKindFreeze",
		Reason: "the outbox holds the kinds its freeze table marks as held, reading the freeze per attempt after the claim is renewed",
	},
	{
		Site: "SiteDescriptionAutofix", File: "internal/app/outboxworker/builder.go", Function: "attempt", Reads: 1,
		NamedInFile: "internal/app/outboxworker/freeze.go", NamedIn: "notificationKindFreeze",
		Reason: "the outbox holds the kinds its freeze table marks as held, reading the freeze per attempt after the claim is renewed",
	},
	{
		Site: "SiteAutoReReview", File: "internal/app/sessionactor/reviewretrigger.go", Function: "readReviewRetriggerState", Reads: 1,
		Reason: "the automatic re-review's first phase reads the freeze in its transaction before any GitHub read",
	},
	{
		Site: "SiteAutoReReview", File: "internal/app/sessionactor/reviewretrigger.go", Function: "finishReviewRetrigger", Reads: 1,
		Reason: "the automatic re-review reads the freeze again at its insert, since a freeze can land while it fetches",
	},
	{
		Site: "SiteAutomationCron", File: "internal/app/automation/triggerpump.go", Function: "evaluateCronAutomation", Reads: 1,
		Reason: "a matched cron fire reads the freeze before it claims the fire, so a frozen fire claims nothing",
	},
	{
		Site: "SiteAutomationFanOut", File: "internal/app/automation/fanout.go", Function: "claimBatch", Reads: 1,
		Reason: "the fan-out reads the freeze in its claim transaction, so nothing is claimed while frozen",
	},
	{
		Site: "SiteAutomationFanOut", File: "internal/app/automation/fanout.go", Function: "pumpOnce", Reads: 1,
		Reason: "each claimed invocation reads the freeze again right before its runs start, giving back the claims a freeze landed on",
	},
	{
		Site: "SiteWorkflowAdvance", File: "internal/app/workflowengine/completion.go", Function: "OnTurnCompleted", Reads: 1,
		Reason: "the workflow engine reads the freeze in the transaction ending an attempt whose next step would advance, before the session guard is asked, and holds the advance in a row while frozen",
	},
	{
		Site: "SiteWorkflowAdvance", File: "internal/app/workflowengine/release.go", Function: "releaseHold", Reads: 1,
		Reason: "the releaser reads the freeze again inside each held advance's own transaction, under the session's lock, before it deletes the hold and applies the advance",
	},
}

// AutonomyFreezeHumanPath is one file holding a person's command, which
// must never consult the freeze (rule (c)), and why.
type AutonomyFreezeHumanPath struct {
	File   string
	Reason string
}

// AutonomyFreezeHumanPaths is rule (c)'s list: a person's commands, which
// §40.2 says the freeze never stops.
var AutonomyFreezeHumanPaths = []AutonomyFreezeHumanPath{
	{File: "internal/adapters/inbound/httpapi/decisioninbox.go", Reason: "the decision inbox's Merge click is a person merging"},
	{File: "internal/adapters/inbound/httpapi/reviewretrigger.go", Reason: "the review button is a person asking for a review"},
	{File: "internal/adapters/inbound/httpapi/turn.go", Reason: "a prompt is a person's command"},
	{File: "internal/adapters/inbound/httpapi/decideplan.go", Reason: "a plan approval is a person's decision"},
	{File: "internal/adapters/inbound/httpapi/decideworkflowstep.go", Reason: "a workflow step decision is a person's decision"},
	{File: "internal/app/sessionactor/owedreviewrequest.go", Reason: "the owed request's re-run is a person's request, inserted on the person's path"},
	{File: "internal/adapters/inbound/mcp/tools.go", Reason: "an MCP tool call is a person's command through their client"},
	{File: "internal/adapters/inbound/httpapi/autonomyfreeze.go", Reason: "freezing and lifting the freeze are an administrator's commands; gated by the freeze, an unreadable freeze would skip the very unfreeze meant to end it"},
}

// AutonomyFreezeViolation is one place the source breaks §40.2's rule that
// every automatic-action site reads the freeze and no person's command
// does. Rule names the check that caught it:
//
//   - "a": a registered read is gone -- its file or function missing, its
//     function reading the freeze through a Gate other than its registered
//     number of times, or its site's constant no longer named where the
//     entry says.
//   - "b": a Site constant internal/domain/autonomy declares, not reserved,
//     has no entry in AutonomyFreezeChecks; or an entry names a constant
//     that is not declared, or that is reserved.
//   - "c": a person's path imports internal/app/autonomy, or calls a Gate
//     method.
//
// What the guard cannot see, by its nature as a name-based check on
// syntax: a Gate held under another name, a read whose result is ignored,
// and a site the domain package never declared.
type AutonomyFreezeViolation struct {
	File   string // slash-separated, relative to the scanned root
	Line   int
	Rule   string
	Detail string
}

func (v AutonomyFreezeViolation) String() string {
	return fmt.Sprintf("%s:%d: rule (%s): %s", v.File, v.Line, v.Rule, v.Detail)
}

// ScanAutonomyFreezeSites checks the tree under root against
// AutonomyFreezeChecks and AutonomyFreezeHumanPaths, and returns every
// AutonomyFreezeViolation, sorted. It is the build-time backstop behind
// §40.2's call-site checks: a site that loses its read of the freeze, a
// new site that never registered, or a person's command that starts
// consulting the freeze fails the build.
func ScanAutonomyFreezeSites(root string) ([]AutonomyFreezeViolation, error) {
	return scanAutonomyFreezeSites(root, AutonomyFreezeChecks, AutonomyFreezeHumanPaths)
}

// scanAutonomyFreezeSites is ScanAutonomyFreezeSites with its tables given,
// so a synthetic tree can be checked against its own.
func scanAutonomyFreezeSites(root string, checks []AutonomyFreezeCheck, humanPaths []AutonomyFreezeHumanPath) ([]AutonomyFreezeViolation, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	parse := func(rel string) (*ast.File, error) {
		if f, ok := parsed[rel]; ok {
			return f, nil
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			return nil, err
		}
		parsed[rel] = f
		return f, nil
	}

	var out []AutonomyFreezeViolation
	add := func(file string, pos token.Pos, rule, detail string) {
		line := 0
		if pos.IsValid() {
			line = fset.Position(pos).Line
		}
		out = append(out, AutonomyFreezeViolation{File: file, Line: line, Rule: rule, Detail: detail})
	}

	// Rule (a).
	for _, c := range checks {
		file, err := parse(c.File)
		if err != nil {
			add(c.File, token.NoPos, "a", fmt.Sprintf("%s's read in %s: cannot parse the file: %v", c.Site, c.Function, err))
			continue
		}
		fn := findFunc(file, c.Function)
		if fn == nil {
			add(c.File, token.NoPos, "a", fmt.Sprintf("%s's read is registered in %s, which the file no longer declares", c.Site, c.Function))
			continue
		}
		if reads := countGateReads(fn.Body); reads != c.Reads {
			add(c.File, fn.Pos(), "a", fmt.Sprintf("%s reads the autonomy freeze through a Gate %d time(s), registered for %s %d time(s) -- a site that stops reading it acts while frozen (§40.2); update internal/ops.AutonomyFreezeChecks only with the reads themselves", c.Function, reads, c.Site, c.Reads))
		}
		namedFile, namedNode, where := file, ast.Node(fn), c.Function
		if c.NamedInFile != "" {
			namedFile, err = parse(c.NamedInFile)
			if err != nil {
				add(c.NamedInFile, token.NoPos, "a", fmt.Sprintf("%s is named in %s: cannot parse the file: %v", c.Site, c.NamedIn, err))
				continue
			}
			namedNode, where = findDecl(namedFile, c.NamedIn), c.NamedIn
			if namedNode == nil {
				add(c.NamedInFile, token.NoPos, "a", fmt.Sprintf("%s is registered as named in %s, which the file no longer declares", c.Site, c.NamedIn))
				continue
			}
		}
		if !namesDomainConst(namedFile, namedNode, c.Site) {
			file := c.File
			if c.NamedInFile != "" {
				file = c.NamedInFile
			}
			add(file, namedNode.Pos(), "a", fmt.Sprintf("%s no longer names %s: the read registered for that site is gone", where, c.Site))
		}
	}

	// Rule (b).
	declared, reserved, err := domainSiteConsts(root)
	if err != nil {
		return nil, err
	}
	registered := map[string]bool{}
	for _, c := range checks {
		registered[c.Site] = true
		switch {
		case reserved[c.Site]:
			add(c.File, token.NoPos, "b", fmt.Sprintf("internal/ops.AutonomyFreezeChecks registers %s, which internal/domain/autonomy reserves for a site not built yet: move it out of Reserved", c.Site))
		case !declared[c.Site]:
			add(c.File, token.NoPos, "b", fmt.Sprintf("internal/ops.AutonomyFreezeChecks registers %s, which internal/domain/autonomy does not declare: drop the stale entry", c.Site))
		}
	}
	var names []string
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !reserved[name] && !registered[name] {
			add(autonomyDomainDir, token.NoPos, "b", fmt.Sprintf("%s is a site internal/domain/autonomy declares, with no read of the freeze registered in internal/ops.AutonomyFreezeChecks -- every automatic-action site reads the freeze (§40.2)", name))
		}
	}

	// Rule (c).
	for _, h := range humanPaths {
		file, err := parse(h.File)
		if err != nil {
			add(h.File, token.NoPos, "c", fmt.Sprintf("a person's path (%s): cannot parse the file: %v", h.Reason, err))
			continue
		}
		for _, imp := range file.Imports {
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == autonomyGateImportPath {
				add(h.File, imp.Pos(), "c", "a person's path imports internal/app/autonomy -- the freeze never stops a person ("+h.Reason+", §40.2)")
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && autonomyGateMethods[sel.Sel.Name] && autonomyGateHolders[terminalName(sel.X)] {
				add(h.File, call.Pos(), "c", "a person's path calls the autonomy gate's "+sel.Sel.Name+" -- the freeze never stops a person ("+h.Reason+", §40.2)")
			}
			return true
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Detail < out[j].Detail
	})
	return out, nil
}

// findFunc returns file's function or method named name.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	return nil
}

// findDecl returns file's top-level function, variable or constant named
// name.
func findDecl(file *ast.File, name string) ast.Node {
	if fn := findFunc(file, name); fn != nil {
		return fn
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for _, id := range vs.Names {
					if id.Name == name {
						return vs
					}
				}
			}
		}
	}
	return nil
}

// countGateReads counts the calls in body of a Gate read method on a
// receiver held under a Gate's name.
func countGateReads(body ast.Node) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && autonomyGateReads[sel.Sel.Name] && autonomyGateHolders[terminalName(sel.X)] {
			n++
		}
		return true
	})
	return n
}

// terminalName is the last name of a selector chain: gate for gate,
// Autonomy for w.deps.Autonomy.
func terminalName(x ast.Expr) string {
	switch x := x.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// namesDomainConst reports whether node, in file, names internal/domain/
// autonomy's constant name through that package's import.
func namesDomainConst(file *ast.File, node ast.Node, name string) bool {
	pkgNames := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != autonomyDomainImportPath {
			continue
		}
		pkgName := "autonomy"
		if imp.Name != nil {
			pkgName = imp.Name.Name
		}
		pkgNames[pkgName] = true
	}
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkgNames[pkg.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// domainSiteConsts reads internal/domain/autonomy's non-test files under
// root: every constant declared of type Site, and the names its Reserved
// variable lists.
func domainSiteConsts(root string) (declared, reserved map[string]bool, err error) {
	dir := filepath.Join(root, filepath.FromSlash(autonomyDomainDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("ops: read %s: %w", autonomyDomainDir, err)
	}
	declared, reserved = map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("ops: parse %s: %w", e.Name(), err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if gen.Tok == token.CONST {
					if typ, ok := vs.Type.(*ast.Ident); ok && typ.Name == "Site" {
						for _, id := range vs.Names {
							declared[id.Name] = true
						}
					}
					continue
				}
				for i, id := range vs.Names {
					if id.Name != "Reserved" || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.CompositeLit); ok {
						for _, elt := range lit.Elts {
							if ident, ok := elt.(*ast.Ident); ok {
								reserved[ident.Name] = true
							}
						}
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		return nil, nil, fmt.Errorf("ops: %s declares no Site constant: the parse is broken, not the tree", autonomyDomainDir)
	}
	return declared, reserved, nil
}

package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// requiredConfigErrorTypes are the two composite-literal type names
// Load's own source uses for a plain "this env var is missing" failure --
// see this file's own top doc comment for why both, and why nothing else
// in config.go carries an EnvVar field with this same "value = the exact
// env var name" meaning.
var requiredConfigErrorTypes = map[string]bool{
	"MissingRequiredEnvError": true,
	"InvalidHMACSecretError":  true,
}

// deploySecretExtraVars is the named, documented exception list this
// file's own top comment explains in full: variables platform.Config
// READS but never marks "required" through the mechanical
// MissingRequiredEnvError/InvalidHMACSecretError shape
// RequiredConfigEnvVars scans for, which a real, fully-featured
// deployment still needs a placeholder for. Kept here, not inferred, so a
// reviewer sees the exact, closed list rather than a scan whose "required"
// set silently grew a special case.
var deploySecretExtraVars = []string{
	// The allowlist group (§13.1): EmptyAllowlistError fires only when
	// all three are empty together, so none of the three is ever the
	// EnvVar value of a MissingRequiredEnvError on its own.
	"NARVI_ALLOWED_EMAIL_DOMAINS",
	"NARVI_ALLOWED_GITHUB_ORGS",
	"NARVI_ALLOWED_EMAILS",
	// Object storage (§28.7): REGION/BUCKET already surface through
	// RequiredConfigEnvVars (they sit inside the `if objectStoreEndpoint
	// != ""` block, but the composite literal is still found regardless
	// of that guard -- see this file's own top doc comment). ENDPOINT
	// itself is the feature flag Load gates the whole bundle on, so it
	// never appears as an EnvVar value of its own; the credential pair
	// and the path-style/public-endpoint knobs never throw a
	// MissingRequiredEnvError at all (InvalidObjectStoreCredentialsError
	// only fires on a MISMATCHED pair). All four are added back by hand
	// so the template is usable against a real S3/MinIO bucket, not just
	// the two fields Load's own scan happens to find.
	"NARVI_OBJECT_STORE_ENDPOINT",
	"NARVI_OBJECT_STORE_ACCESS_KEY_ID",
	"NARVI_OBJECT_STORE_SECRET_ACCESS_KEY",
	"NARVI_OBJECT_STORE_PUBLIC_ENDPOINT",
	"NARVI_OBJECT_STORE_USE_PATH_STYLE",
}

// RequiredConfigEnvVars is this repository's own drift guard (§41.1): the
// repository claims deploy/control-plane/secret.yaml names "every required
// platform.Config variable" (that file's own top comment, and this
// package's own doc.go house style of keeping a claim honest with a test
// rather than discipline). This function is the mechanism, on the same
// go/ast-scan model routes.go and instruments.go already established:
// read the ground truth (internal/platform/config.go's own source) rather
// than a hand-maintained copy of it, so a future change to Load's own
// validation cannot silently outrun the committed Secret template.
//
// # What "required" means here
//
// platform.Config.Load's own validation is not a flat list: some
// variables are unconditionally required (NARVI_STAGE, NARVI_DATABASE_URL,
// ...), others only once a feature surface is turned on (the five Linear
// fields, only when NARVI_INGRESS_ENABLED includes "linear"; the object
// store bundle, only once NARVI_OBJECT_STORE_ENDPOINT is set). Modeling
// that full conditional structure statically would mean re-implementing
// Load's own control flow a second time -- exactly the "second, parallel
// copy" this repo's own conventions (CLAUDE.md: "no I/O... every state
// transition"; the Dockerfile's own top comment for `make dist`) refuse
// everywhere else.
//
// Every one of those variables -- unconditional or feature-gated alike --
// shares ONE mechanical shape in Load's own source, though: the exact
// moment Load decides a variable is missing, it appends either
// &MissingRequiredEnvError{EnvVar: xEnvVarName} (the general case) or
// &InvalidHMACSecretError{EnvVar: xEnvVarName} (the three HMAC secrets'
// own distinctly-worded sibling, config.go's own InvalidHMACSecretError
// doc comment). This function finds every composite literal
// of either type, anywhere in the file, regardless of which `if` guards
// it -- an unconditionally-required field and a feature-gated one look
// identical to this scan, which is exactly the property that makes this
// scan immune to Load's own branching. A Secret template built for a
// deployment that enables every surface (the only kind three-plain-
// manifests-no-Helm, §41.1's own "one configuration surface" principle,
// can describe) needs every one of these set regardless.
//
// Two known variables are DELIBERATELY excluded from this mechanical
// scan, and named here rather than silently missed:
//
//   - The three allowlist variables (NARVI_ALLOWED_EMAIL_DOMAINS/
//     _GITHUB_ORGS/_EMAILS) are required as a GROUP -- Load's own
//     EmptyAllowlistError fires only when all three are empty at once,
//     never naming one -- so no single one of them ever appears in a
//     MissingRequiredEnvError. deploySecretExtraVars (above) adds all three
//     back in by name (a deployment needs at least one set, and a
//     template that showed none of them would be actively misleading).
//   - The object store credential pair (NARVI_OBJECT_STORE_ACCESS_KEY_ID/
//     _SECRET_ACCESS_KEY) is optional even once object storage itself is
//     turned on (Load's own InvalidObjectStoreCredentialsError only
//     fires on a MISMATCHED pair, both-or-neither), so neither name is
//     required in Load's own sense either. deploySecretExtraVars adds
//     the whole object-store bundle back in (the endpoint, region and
//     bucket variables ARE mechanically required once
//     NARVI_OBJECT_STORE_ENDPOINT is set, but every OTHER
//     NARVI_OBJECT_STORE_* variable stays invisible to this scan without
//     it) because a real deployment that wants object storage needs
//     every one of them, and the Secret template exists to be useful to
//     an operator, not merely mechanically minimal.
//
// Both exceptions are handled the same way ScanRegisteredRoutes's own doc
// comment handles ITS scanner's blind spots: named in prose, not
// silently absorbed into the "required" set as if the AST walk had found
// them on its own.
//
// Concretely, RequiredConfigEnvVars parses configPath
// (internal/platform/config.go) and returns two sets: required (every
// NARVI_* env var name that can appear as the EnvVar field of a
// MissingRequiredEnvError or InvalidHMACSecretError composite literal
// ANYWHERE in the file -- see above for why "anywhere", not "on an
// unconditional path", is the correct scope) and read (every NARVI_* env
// var name passed to os.Getenv or os.LookupEnv anywhere in the file, the
// full superset TestDeploySecretTemplate's own "names one it no longer
// reads" direction checks against).
//
// Mirrors ScanRegisteredRoutes's own two-pass shape (routes.go): a first
// pass over top-level `const` declarations resolves every
// `xEnvVarName = "NARVI_..."` identifier to its string value, and a
// second ast.Inspect walk resolves every CallExpr/CompositeLit site back
// through that same map.
//
// # Fail closed, not silently skip (§41.1 review round 1, finding P5)
//
// An earlier version of this function silently `continue`d past three
// shapes it could not classify: an EnvVar value that was a literal string
// rather than a resolvable const identifier, an EnvVar value that WAS an
// identifier but did not resolve to any top-level const (e.g. a helper
// function's own parameter -- a `requireEnv(&errs, name)`-shaped
// indirection around MissingRequiredEnvError construction), and a
// "missing required" signal built any other way entirely (e.g.
// `fmt.Errorf("missing required %s", xEnvVarName)` instead of one of the
// two blessed composite-literal types). Every one of those is a real,
// reachable way to make a variable required that this scanner would then
// under-count -- proven by mutation: adding any of the three to config.go
// left TestDeploySecretTemplate green even though secret.yaml named none
// of the newly-required variables. The only backstop left was
// `len(required)==0`, which catches a complete refactor but not a
// realistic partial one (§41.2's own future NARVI_SANDBOX_PROVIDER
// "refused at boot with the missing names" phrasing reads exactly like
// the fmt.Errorf shape above).
//
// This version closes that gap two ways rather than one, because the two
// known-missed shapes need different treatment:
//
//   - A composite literal of one of requiredConfigErrorTypes now accepts
//     a literal string EnvVar value directly (no identifier resolution
//     needed -- there is nothing left to resolve), and now treats an
//     Ident EnvVar value that does NOT resolve via constValue as a hard
//     scan error (via scanErr below) instead of silently doing nothing:
//     a value in this exact shape is unambiguously claiming "this
//     variable is required," and this scanner has no business pretending
//     it didn't see the claim just because it can't name the variable.
//   - A `fmt.Errorf(format, args...)` call whose format string contains
//     "required" or "missing" (case-insensitively) is now ALSO treated as
//     a required-variable signal: every arg that is an Ident resolving
//     via constValue is added to required exactly as if it had come from
//     a MissingRequiredEnvError literal. Scoped to format strings that
//     actually say so, rather than every fmt.Errorf call in the file, so
//     an unrelated validation message (e.g. "invalid X: must be a
//     positive integer") is never mistaken for a required-variable claim.
//
// Finally, as a backstop under BOTH of the above rather than a
// replacement for either: every "NARVI_..." string literal found ANYWHERE
// in the file (not merely inside a recognized shape) must end up in
// required or read by the time the walk finishes, checked once, after the
// walk, against the accumulated literals map -- a literal this scanner
// still cannot account for, in a shape nobody anticipated yet, fails the
// scan outright rather than being invisibly absorbed. Every literal
// config.go declares today already round-trips through some xEnvVarName
// const that IS read via os.Getenv/LookupEnv somewhere in the file, so
// this backstop costs nothing today (§41.1 review's own finding: "every
// current site in config.go resolves") -- it only ever fires the moment a
// FUTURE change introduces a truly unclassifiable one.
func RequiredConfigEnvVars(configPath string) (required map[string]bool, read map[string]bool, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("ops: parse %s: %w", configPath, err)
	}

	constValue := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			constValue[vs.Names[0].Name] = v
		}
	}

	required = map[string]bool{}
	read = map[string]bool{}
	// literalLine records the FIRST line every "NARVI_..." string literal
	// was found at, anywhere in the file -- the fail-closed backstop's own
	// input, checked once after the walk below finishes.
	literalLine := map[string]int{}

	var scanErr error
	ast.Inspect(file, func(n ast.Node) bool {
		if scanErr != nil {
			return false
		}
		switch node := n.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			v, unquoteErr := strconv.Unquote(node.Value)
			if unquoteErr != nil || !strings.HasPrefix(v, "NARVI_") {
				return true
			}
			if _, seen := literalLine[v]; !seen {
				literalLine[v] = fset.Position(node.Pos()).Line
			}

		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkgIdent.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv"):
				if len(node.Args) == 0 {
					return true
				}
				argIdent, ok := node.Args[0].(*ast.Ident)
				if !ok {
					return true
				}
				if v, ok := constValue[argIdent.Name]; ok {
					read[v] = true
				}

			case pkgIdent.Name == "fmt" && sel.Sel.Name == "Errorf":
				if len(node.Args) < 2 {
					return true
				}
				formatLit, ok := node.Args[0].(*ast.BasicLit)
				if !ok || formatLit.Kind != token.STRING {
					return true
				}
				format, unquoteErr := strconv.Unquote(formatLit.Value)
				if unquoteErr != nil {
					return true
				}
				lower := strings.ToLower(format)
				if !strings.Contains(lower, "required") && !strings.Contains(lower, "missing") {
					return true
				}
				for _, arg := range node.Args[1:] {
					argIdent, ok := arg.(*ast.Ident)
					if !ok {
						continue
					}
					if v, ok := constValue[argIdent.Name]; ok {
						required[v] = true
					}
				}
			}

		case *ast.CompositeLit:
			typeIdent, ok := node.Type.(*ast.Ident)
			if !ok || !requiredConfigErrorTypes[typeIdent.Name] {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				keyIdent, ok := kv.Key.(*ast.Ident)
				if !ok || keyIdent.Name != "EnvVar" {
					continue
				}
				switch val := kv.Value.(type) {
				case *ast.Ident:
					if v, ok := constValue[val.Name]; ok {
						required[v] = true
					} else {
						scanErr = fmt.Errorf("ops: %s:%d: %s{EnvVar: %s} -- %s does not resolve to any top-level NARVI_* const in this file (a helper function's own parameter, perhaps); RequiredConfigEnvVars cannot classify this required-variable claim, and a claim it cannot see is a claim deploy/control-plane/secret.yaml's own drift guard cannot protect either",
							configPath, fset.Position(val.Pos()).Line, typeIdent.Name, val.Name, val.Name)
						return false
					}
				case *ast.BasicLit:
					if val.Kind != token.STRING {
						scanErr = fmt.Errorf("ops: %s:%d: %s{EnvVar: ...} -- EnvVar is a non-string literal RequiredConfigEnvVars cannot classify",
							configPath, fset.Position(val.Pos()).Line, typeIdent.Name)
						return false
					}
					if v, unquoteErr := strconv.Unquote(val.Value); unquoteErr == nil {
						required[v] = true
					}
				default:
					scanErr = fmt.Errorf("ops: %s:%d: %s{EnvVar: ...} -- EnvVar is neither an identifier nor a string literal; RequiredConfigEnvVars cannot classify this required-variable claim",
						configPath, fset.Position(kv.Value.Pos()).Line, typeIdent.Name)
					return false
				}
			}
		}
		return true
	})
	if scanErr != nil {
		return nil, nil, scanErr
	}

	for v, line := range literalLine {
		if required[v] || read[v] {
			continue
		}
		return nil, nil, fmt.Errorf("ops: %s:%d: found the literal %q, which looks like an env var name, in a shape RequiredConfigEnvVars does not recognize as either a required-variable signal or a plain os.Getenv/LookupEnv read -- classify it explicitly (route it through a top-level xxxEnvVarName const referenced by identifier, exactly like every other variable in this file) or teach this scanner its new shape; a variable this scanner cannot see is a variable deploy/control-plane/secret.yaml's own drift guard cannot protect either",
			configPath, line, v)
	}

	return required, read, nil
}

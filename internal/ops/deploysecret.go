package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
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
// through that same map -- so a site that passes the identifier (every
// real call in this file does) is found, and a site that were to pass a
// literal string directly is silently skipped, exactly like
// stringLiteralValue's own "a non-literal argument is skipped, never
// errored" convention in routes.go.
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

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "os" {
				return true
			}
			if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
				return true
			}
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
				valIdent, ok := kv.Value.(*ast.Ident)
				if !ok {
					continue
				}
				if v, ok := constValue[valIdent.Name]; ok {
					required[v] = true
				}
			}
		}
		return true
	})

	return required, read, nil
}

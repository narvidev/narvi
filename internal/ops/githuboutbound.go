package ops

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/narvidev/narvi/internal/platform"
)

// platformImportPath is the package that owns §12.5's GitHub outbound axis
// -- the one place allowed to read the bot credential and build its typed
// holder.
const platformImportPath = "github.com/narvidev/narvi/internal/platform"

// GitHubOutboundViolation is one place the source breaks §12.5's rule that
// the GitHub bot credential reaches its consumers only as
// *platform.GitHubOutboundConfig, built by platform.Load. Rule names the
// check that caught it:
//
//   - "a": the bot token's environment variable is named in a string
//     literal outside internal/platform (non-test code) -- someone reading
//     the variable around Load.
//   - "b": platform.GitHubOutboundConfig is named outside internal/platform
//     other than behind a pointer (a composite literal, new(), a value
//     variable -- each a config Load never validated), or its constructors
//     are referenced from non-test code outside internal/platform, where
//     Load is the only legitimate caller.
//   - "c": the composition root (non-test controlplane/*.go) unwraps the
//     token with .BotToken() -- it must hand consumers the typed config,
//     never the string.
type GitHubOutboundViolation struct {
	File   string // slash-separated, relative to the scanned root
	Line   int
	Rule   string
	Detail string
}

func (v GitHubOutboundViolation) String() string {
	return fmt.Sprintf("%s:%d: rule (%s): %s", v.File, v.Line, v.Rule, v.Detail)
}

// ScanGitHubOutbound walks every .go file under root (skipping testdata,
// vendor, node_modules and .git) and returns every GitHubOutboundViolation,
// sorted. It is the backstop behind what the compiler already enforces --
// no string field for the token on platform.Config, and constructors that
// refuse a nil config -- for the shapes a type cannot rule out.
//
// The variable's name is discovered from platform.Load's own behaviour,
// not written here: loading with GitHub outbound declared on and no token
// reports it as the one missing variable with a RequiredBy reason (rule R4).
// So this file names it nowhere, and needs no exemption from rule (a).
func ScanGitHubOutbound(root string) ([]GitHubOutboundViolation, error) {
	tokenVar, err := gitHubBotTokenEnvVar()
	if err != nil {
		return nil, err
	}

	var violations []GitHubOutboundViolation
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/platform/") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("ops: parse %s: %w", path, parseErr)
		}
		isTest := strings.HasSuffix(rel, "_test.go")
		inControlplane := !strings.Contains(strings.TrimPrefix(rel, "controlplane/"), "/") && strings.HasPrefix(rel, "controlplane/")
		violations = append(violations, scanGitHubOutboundFile(fset, file, rel, tokenVar, isTest, inControlplane)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})
	return violations, nil
}

// scanGitHubOutboundFile applies the three rules to one parsed file.
func scanGitHubOutboundFile(fset *token.FileSet, file *ast.File, rel, tokenVar string, isTest, inControlplane bool) []GitHubOutboundViolation {
	var out []GitHubOutboundViolation
	add := func(pos token.Pos, rule, detail string) {
		out = append(out, GitHubOutboundViolation{File: rel, Line: fset.Position(pos).Line, Rule: rule, Detail: detail})
	}

	// The local name(s) internal/platform is imported under in this file.
	platformNames := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != platformImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			platformNames["platform"] = true
		case imp.Name.Name == ".":
			add(imp.Pos(), "b", "internal/platform is dot-imported, so GitHubOutboundConfig could be named without its package")
		case imp.Name.Name != "_":
			platformNames[imp.Name.Name] = true
		}
	}

	// Every platform.GitHubOutboundConfig that IS behind a pointer.
	pointed := map[*ast.SelectorExpr]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if star, ok := n.(*ast.StarExpr); ok {
			if sel, ok := star.X.(*ast.SelectorExpr); ok {
				pointed[sel] = true
			}
		}
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if isTest || n.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(n.Value); err == nil && strings.Contains(value, tokenVar) {
				add(n.Pos(), "a", tokenVar+" is named outside internal/platform: only platform.Load reads it")
			}
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && platformNames[pkg.Name] {
				switch n.Sel.Name {
				case "GitHubOutboundConfig":
					if !pointed[n] {
						add(n.Pos(), "b", "platform.GitHubOutboundConfig named other than behind a pointer -- a value Load never validated")
					}
				case "NewGitHubOutboundConfig", "MustNewGitHubOutboundConfig":
					if !isTest {
						add(n.Pos(), "b", "platform."+n.Sel.Name+" referenced outside internal/platform in non-test code -- platform.Load is its only production caller")
					}
				}
			}
			if inControlplane && !isTest && n.Sel.Name == "BotToken" {
				add(n.Pos(), "c", ".BotToken() in the composition root -- hand consumers the typed *platform.GitHubOutboundConfig, never the string")
			}
		}
		return true
	})
	return out
}

// gitHubBotTokenEnvVar returns the bot token's environment variable name as
// platform.Load reports it: loading with GitHub outbound declared on and
// nothing else set, it is the one missing variable that carries a
// RequiredBy reason.
func gitHubBotTokenEnvVar() (string, error) {
	loadErr := probeLoad(map[string]string{
		platform.StageEnvVarName: string(platform.StageProduction),
		"NARVI_OUTBOUND_ENABLED": "github",
	})
	for _, leaf := range flattenJoinedErrors(loadErr) {
		var missing *platform.MissingRequiredEnvError
		if errors.As(leaf, &missing) && missing.RequiredBy != "" {
			return missing.EnvVar, nil
		}
	}
	return "", fmt.Errorf("ops: platform.Load reported no variable required by GitHub outbound (got %v) -- the outbound guard cannot name the bot token", loadErr)
}

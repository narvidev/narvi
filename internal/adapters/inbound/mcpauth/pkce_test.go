package mcpauth

import (
	"crypto/sha256"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestValidPKCEValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("a", 43), true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 42), false},
		{strings.Repeat("a", 129), false},
		{strings.Repeat("A", 20) + "0123456789-._~" + strings.Repeat("z", 9), true},
		{strings.Repeat("a", 42) + "+", false},
		{strings.Repeat("a", 42) + "/", false},
		{strings.Repeat("a", 42) + "=", false},
		{strings.Repeat("a", 42) + " ", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := validPKCEValue(tc.in); got != tc.want {
			t.Errorf("validPKCEValue(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestVerifyPKCES256 pins RFC 7636 appendix B's own example and the
// refusals around it.
func TestVerifyPKCES256(t *testing.T) {
	t.Parallel()

	const rfcVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const rfcChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if !verifyPKCES256(rfcVerifier, rfcChallenge) {
		t.Fatalf("verifyPKCES256(RFC 7636 example) = false, want true")
	}
	if s256(rfcVerifier) != rfcChallenge {
		t.Fatalf("test helper s256 disagrees with RFC 7636 appendix B")
	}
	tests := []struct {
		name      string
		verifier  string
		challenge string
	}{
		{"wrong verifier", rfcVerifier[:len(rfcVerifier)-1] + "l", rfcChallenge},
		{"plain method's own shape: verifier equals challenge", rfcChallenge, rfcChallenge},
		{"empty challenge", rfcVerifier, ""},
		{"challenge with padding", rfcVerifier, rfcChallenge + "="},
	}
	for _, tc := range tests {
		if verifyPKCES256(tc.verifier, tc.challenge) {
			t.Errorf("%s: verifyPKCES256 = true, want false", tc.name)
		}
	}
}

// TestPKCE_UsesConstantTimeCompare is the timing-attack threat row's own
// structural half: verifyPKCES256 must decide through
// subtle.ConstantTimeCompare, and contain no other ==/!= comparison a
// later edit could swap in for it (the only permitted one is the
// "ConstantTimeCompare(...) == 1" result check).
func TestPKCE_UsesConstantTimeCompare(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "pkce.go", nil, 0)
	if err != nil {
		t.Fatalf("parse pkce.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "verifyPKCES256" {
			fn = d
		}
	}
	if fn == nil {
		t.Fatal("verifyPKCES256 not found in pkce.go")
	}
	constantTimeCalls, otherComparisons := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ConstantTimeCompare" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "subtle" {
					constantTimeCalls++
				}
			}
		case *ast.BinaryExpr:
			if e.Op != token.EQL && e.Op != token.NEQ {
				return true
			}
			call, ok := e.X.(*ast.CallExpr)
			isCTC := false
			if ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ConstantTimeCompare" {
					isCTC = true
				}
			}
			if !isCTC {
				otherComparisons++
			}
		}
		return true
	})
	if constantTimeCalls != 1 {
		t.Errorf("verifyPKCES256 calls subtle.ConstantTimeCompare %d times, want exactly 1", constantTimeCalls)
	}
	if otherComparisons != 0 {
		t.Errorf("verifyPKCES256 contains %d ==/!= comparison(s) other than the ConstantTimeCompare result check", otherComparisons)
	}
}

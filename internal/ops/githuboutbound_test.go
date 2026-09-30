package ops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestGitHubOutboundGuard is the backstop behind §12.5's outbound axis,
// run against this repository's real tree: the bot token's variable is
// read nowhere but internal/platform, GitHubOutboundConfig is never built
// or held by value outside it, its constructors have no production caller
// but platform.Load, and the composition root never unwraps the token.
func TestGitHubOutboundGuard(t *testing.T) {
	violations, err := ScanGitHubOutbound(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanGitHubOutbound: %v", err)
	}
	if len(violations) > 0 {
		lines := make([]string, len(violations))
		for i, v := range violations {
			lines[i] = v.String()
		}
		t.Errorf("%d GitHub outbound violation(s) -- the bot credential reaches consumers only as *platform.GitHubOutboundConfig, built by platform.Load (technical plan §12.5):\n\t%s",
			len(violations), strings.Join(lines, "\n\t"))
	}
}

// TestScanGitHubOutbound_Rules proves each rule fires on the shape it
// exists for, and stays quiet on the shapes it allows, over a synthetic
// tree per case.
func TestScanGitHubOutbound_Rules(t *testing.T) {
	const imp = `import "github.com/narvidev/narvi/internal/platform"` + "\n"
	tests := []struct {
		name  string
		path  string
		src   string
		rules []string // the rules expected to fire, in order
	}{
		{"a: variable read outside platform", "internal/foo/foo.go",
			"package foo\nimport \"os\"\nvar _ = os.Getenv(\"NARVI_GITHUB_BOT_TOKEN\")\n", []string{"a"}},
		{"a: a test may set it to drive Load", "internal/foo/foo_test.go",
			"package foo\nimport \"os\"\nfunc f() { _ = os.Setenv(\"NARVI_GITHUB_BOT_TOKEN\", \"x\") }\n", nil},
		{"a: a comment naming it is not a read", "internal/foo/foo.go",
			"package foo\n// NARVI_GITHUB_BOT_TOKEN is read by platform.Load.\nvar x = 1\n", nil},
		{"a: platform itself reads it", "internal/platform/x.go",
			"package platform\nconst n = \"NARVI_GITHUB_BOT_TOKEN\"\n", nil},
		{"b: composite literal", "internal/foo/foo.go",
			"package foo\n" + imp + "var _ = platform.GitHubOutboundConfig{}\n", []string{"b"}},
		{"b: address of a composite literal", "internal/foo/foo_test.go",
			"package foo\n" + imp + "var _ = &platform.GitHubOutboundConfig{}\n", []string{"b"}},
		{"b: new()", "internal/foo/foo.go",
			"package foo\n" + imp + "var _ = new(platform.GitHubOutboundConfig)\n", []string{"b"}},
		{"b: a value variable", "internal/foo/foo.go",
			"package foo\n" + imp + "var v platform.GitHubOutboundConfig\n", []string{"b"}},
		{"b: under another import name", "internal/foo/foo.go",
			"package foo\nimport p \"github.com/narvidev/narvi/internal/platform\"\nvar _ = p.GitHubOutboundConfig{}\n", []string{"b"}},
		{"b: behind a pointer is the one allowed shape", "internal/foo/foo.go",
			"package foo\n" + imp + "func f(c *platform.GitHubOutboundConfig) []*platform.GitHubOutboundConfig { return nil }\n", nil},
		{"b: constructor called from production code", "internal/foo/foo.go",
			"package foo\n" + imp + "var _, _ = platform.NewGitHubOutboundConfig(\"x\")\n", []string{"b"}},
		{"b: test constructor called from production code", "controlplane/x.go",
			"package controlplane\n" + imp + "var _ = platform.MustNewGitHubOutboundConfig(\"x\")\n", []string{"b"}},
		{"b: constructors are fine in tests", "internal/foo/foo_test.go",
			"package foo\n" + imp + "var _ = platform.MustNewGitHubOutboundConfig(\"x\")\n", nil},
		{"c: composition root unwraps the token", "controlplane/x.go",
			"package controlplane\n" + imp + "func f(c *platform.Config) string { return c.GitHubOutbound.BotToken() }\n", []string{"c"}},
		{"c: a controlplane test may", "controlplane/x_test.go",
			"package controlplane\n" + imp + "func f(c *platform.Config) string { return c.GitHubOutbound.BotToken() }\n", nil},
		{"c: a consumer unwraps inside itself", "internal/foo/foo.go",
			"package foo\n" + imp + "func f(c *platform.GitHubOutboundConfig) string { return c.BotToken() }\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}

			violations, err := ScanGitHubOutbound(root)
			if err != nil {
				t.Fatalf("ScanGitHubOutbound: %v", err)
			}
			var got []string
			for _, v := range violations {
				got = append(got, v.Rule)
			}
			if !slices.Equal(got, tc.rules) {
				t.Errorf("rules fired = %v, want %v (violations: %v)", got, tc.rules, violations)
			}
		})
	}
}

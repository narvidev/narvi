package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuideCommand_Validate(t *testing.T) {
	tests := []struct {
		name    string
		c       GuideCommand
		wantErr bool
	}{
		{"valid route", GuideCommand{Name: "Create a session", Route: "POST /api/sessions"}, false},
		{"valid classifier", GuideCommand{Name: "PR mention", Classifier: &ClassifierRef{Surface: "github", Target: "review"}}, false},
		{"missing name", GuideCommand{Route: "POST /api/sessions"}, true},
		{"neither route nor classifier", GuideCommand{Name: "x"}, true},
		{"both route and classifier", GuideCommand{Name: "x", Route: "POST /api/sessions", Classifier: &ClassifierRef{Surface: "web"}}, true},
		{"malformed route: no method", GuideCommand{Name: "x", Route: "/api/sessions"}, true},
		{"malformed route: lowercase method", GuideCommand{Name: "x", Route: "post /api/sessions"}, true},
		{"malformed route: unknown method", GuideCommand{Name: "x", Route: "FETCH /api/sessions"}, true},
		{"malformed route: path missing leading slash", GuideCommand{Name: "x", Route: "GET api/sessions"}, true},
		{"classifier missing surface", GuideCommand{Name: "x", Classifier: &ClassifierRef{Target: "review"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadGuides(t *testing.T) {
	t.Run("valid guide with route and classifier commands", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Web Guide\n\nSome prose.\n\n```json narvi-command\n{\"name\": \"Create a session\", \"route\": \"POST /api/sessions\"}\n```\n\nMore prose.\n\n```json narvi-command\n{\"name\": \"PR mention\", \"classifier\": {\"surface\": \"github\", \"target\": \"review\"}}\n```\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		// README.md must be ignored entirely, even if malformed.
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a guide, no heading, no commands"), 0o644); err != nil {
			t.Fatalf("write README.md: %v", err)
		}

		guides, err := LoadGuides(dir)
		if err != nil {
			t.Fatalf("LoadGuides: %v", err)
		}
		if len(guides) != 1 {
			t.Fatalf("len(guides) = %d, want 1 (README.md must be excluded)", len(guides))
		}
		g := guides[0]
		if g.Surface != "web" {
			t.Errorf("Surface = %q, want \"web\"", g.Surface)
		}
		if g.Title != "Web Guide" {
			t.Errorf("Title = %q, want \"Web Guide\"", g.Title)
		}
		if len(g.Commands) != 2 {
			t.Fatalf("len(Commands) = %d, want 2", len(g.Commands))
		}
		if g.Commands[0].Route != "POST /api/sessions" {
			t.Errorf("Commands[0].Route = %q", g.Commands[0].Route)
		}
		if g.Commands[1].Classifier == nil || g.Commands[1].Classifier.Surface != "github" {
			t.Errorf("Commands[1].Classifier = %+v", g.Commands[1].Classifier)
		}
	})

	t.Run("missing heading fails closed", func(t *testing.T) {
		dir := t.TempDir()
		content := "No heading here.\n\n```json narvi-command\n{\"name\": \"x\", \"route\": \"GET /health\"}\n```\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		if _, err := LoadGuides(dir); err == nil {
			t.Error("LoadGuides() error = nil, want an error for a file with no top-level heading")
		}
	})

	t.Run("zero command blocks fails closed", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Web Guide\n\nNo command blocks at all.\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		if _, err := LoadGuides(dir); err == nil {
			t.Error("LoadGuides() error = nil, want an error for a file with zero narvi-command blocks")
		}
	})

	t.Run("unterminated fence fails closed, never silently skipped", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Web Guide\n\n```json narvi-command\n{\"name\": \"x\", \"route\": \"GET /health\"}\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		if _, err := LoadGuides(dir); err == nil {
			t.Error("LoadGuides() error = nil, want an error for an unterminated fence")
		}
	})

	t.Run("malformed JSON inside a fence fails closed", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Web Guide\n\n```json narvi-command\n{not valid json at all\n```\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		if _, err := LoadGuides(dir); err == nil {
			t.Error("LoadGuides() error = nil, want an error for malformed embedded JSON")
		}
	})

	t.Run("command failing Validate fails closed", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Web Guide\n\n```json narvi-command\n{\"name\": \"\", \"route\": \"GET /health\"}\n```\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		if _, err := LoadGuides(dir); err == nil {
			t.Error("LoadGuides() error = nil, want an error for a command missing its required name")
		}
	})

	t.Run("unknown JSON field fails closed", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Web Guide\n\n```json narvi-command\n{\"name\": \"x\", \"route\": \"GET /health\", \"typo\": true}\n```\n"
		if err := os.WriteFile(filepath.Join(dir, "web.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write web.md: %v", err)
		}
		if _, err := LoadGuides(dir); err == nil {
			t.Error("LoadGuides() error = nil, want an error for an unrecognized field in a narvi-command block")
		}
	})
}

// TestExtractCommandBlocks_HiddenFenceInHTMLComment is G2's own mutation
// proof: a narvi-command fence written between <!-- and --> renders
// invisibly in any real markdown viewer, so it must document nothing.
// Before this fix, extractCommandBlocks was a pure line scanner with no
// comment awareness at all and extracted this block exactly like a real,
// visible one.
func TestExtractCommandBlocks_HiddenFenceInHTMLComment(t *testing.T) {
	content := []byte("# Web Guide\n\n" +
		"<!--\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Hidden\", \"route\": \"GET /hidden-in-comment\"}\n" +
		"```\n" +
		"-->\n\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Visible\", \"route\": \"GET /visible\"}\n" +
		"```\n")

	commands, err := extractCommandBlocks("web.md", content)
	if err != nil {
		t.Fatalf("extractCommandBlocks: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("extractCommandBlocks() returned %d commands, want exactly 1 (the comment-hidden fence must not count): %+v", len(commands), commands)
	}
	if commands[0].Route != "GET /visible" {
		t.Errorf("commands[0].Route = %q, want \"GET /visible\" (the visible fence, not the hidden one)", commands[0].Route)
	}
}

// TestExtractCommandBlocks_HiddenFenceInEnclosingFence is G2's own second
// mutation proof: a narvi-command fence nested inside a longer, unrelated
// example fence (the shape docs/guides/README.md itself uses to show a
// reader what the mechanism looks like, wrapped in a longer ```` fence so
// the inner ``` renders as literal text, not a live block) must not be
// extracted as real documentation either — it documents the FORMAT, not a
// claim about the named route. Before this fix, extractCommandBlocks had
// no notion of fence nesting at all and matched the inner line exactly
// like a top-level one.
func TestExtractCommandBlocks_HiddenFenceInEnclosingFence(t *testing.T) {
	content := []byte("# Web Guide\n\n" +
		"Example:\n\n" +
		"````\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Example only\", \"route\": \"GET /example-only\"}\n" +
		"```\n" +
		"````\n\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Visible\", \"route\": \"GET /visible\"}\n" +
		"```\n")

	commands, err := extractCommandBlocks("web.md", content)
	if err != nil {
		t.Fatalf("extractCommandBlocks: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("extractCommandBlocks() returned %d commands, want exactly 1 (the fence nested inside the outer example fence must not count): %+v", len(commands), commands)
	}
	if commands[0].Route != "GET /visible" {
		t.Errorf("commands[0].Route = %q, want \"GET /visible\" (the visible fence, not the nested example)", commands[0].Route)
	}
}

// TestExtractCommandBlocks_InlineHTMLCommentMarkerInProseIsNotAComment is
// this Step's own regression proof for the guide-drift-omission PR's own
// defect: the OLD comment-detector entered HTML-comment mode whenever a
// line merely CONTAINED htmlCommentOpen ("<!--"), so prose that just
// mentions the marker inline -- never opening a real comment -- swallowed
// every visible narvi-command fence after it to EOF, with no error. Real
// CommonMark only starts an HTML-comment block when the line ITSELF
// (after trimming) STARTS WITH "<!--"; this line does not (it starts with
// "The plan message"), so the fence immediately below must still be
// extracted. Before the fix, this test failed with 0 commands, not 1.
func TestExtractCommandBlocks_InlineHTMLCommentMarkerInProseIsNotAComment(t *testing.T) {
	content := []byte("# Web Guide\n\n" +
		"The plan message embeds a hidden `<!--` marker so edits can find it.\n\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Visible\", \"route\": \"GET /visible\"}\n" +
		"```\n")

	commands, err := extractCommandBlocks("web.md", content)
	if err != nil {
		t.Fatalf("extractCommandBlocks: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("extractCommandBlocks() returned %d commands, want exactly 1 (an inline \"<!--\" mention in prose must not open a comment): %+v", len(commands), commands)
	}
	if commands[0].Route != "GET /visible" {
		t.Errorf("commands[0].Route = %q, want \"GET /visible\"", commands[0].Route)
	}
}

// TestExtractCommandBlocks_FenceInfoStringWithBacktickIsNotAFenceOpener is
// this Step's own second regression proof: the OLD fence-opener detector
// treated ANY line starting with a 3-or-more backtick run as an enclosing
// fence open, even when its info string itself contained a backtick (e.g.
// "```x``` is inline code" -- a real CommonMark reader never treats this
// as a fence open at all, since a fence's info string may not itself
// contain a backtick). The old code wrongly entered enclosing-fence mode
// on that line, which then swallowed the very next, real narvi-command
// fence as literal example text. Before the fix, this test failed with 0
// commands, not 1.
func TestExtractCommandBlocks_FenceInfoStringWithBacktickIsNotAFenceOpener(t *testing.T) {
	content := []byte("# Web Guide\n\n" +
		"```x``` is inline code\n\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Visible\", \"route\": \"GET /visible\"}\n" +
		"```\n")

	commands, err := extractCommandBlocks("web.md", content)
	if err != nil {
		t.Fatalf("extractCommandBlocks: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("extractCommandBlocks() returned %d commands, want exactly 1 (an info string with a backtick must not open an enclosing fence): %+v", len(commands), commands)
	}
	if commands[0].Route != "GET /visible" {
		t.Errorf("commands[0].Route = %q, want \"GET /visible\"", commands[0].Route)
	}
}

// TestExtractCommandBlocks_UnterminatedHTMLCommentAtEOF pins the honest
// choice this rewrite makes for the failure mode H1 flagged: an HTML
// comment opened but never closed before EOF swallows every remaining
// line, including any real narvi-command fence inside it -- exactly like
// the inline-marker false positive above, just genuinely open this time.
// Silently accepting that (returning whatever commands were found before
// the comment opened, with no error) would make a guide silently pass
// with an aspirational-but-unchecked route hidden past the unterminated
// comment; extractCommandBlocks must instead fail closed.
func TestExtractCommandBlocks_UnterminatedHTMLCommentAtEOF(t *testing.T) {
	content := []byte("# Web Guide\n\n" +
		"<!-- opened but never closed\n\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Hidden\", \"route\": \"GET /hidden\"}\n" +
		"```\n")

	_, err := extractCommandBlocks("web.md", content)
	if err == nil {
		t.Fatal("extractCommandBlocks() returned nil error, want an error for an HTML comment still open at EOF")
	}
	if !strings.Contains(err.Error(), "unterminated") || !strings.Contains(err.Error(), "comment") {
		t.Errorf("extractCommandBlocks() error = %q, want it to mention an unterminated comment", err.Error())
	}
}

// TestExtractCommandBlocks_UnterminatedEnclosingFenceAtEOF is the
// enclosing-fence twin of the comment case immediately above: a longer
// example fence opened but never closed before EOF swallows every
// remaining line as literal text, including any real narvi-command fence
// inside it. extractCommandBlocks must fail closed here too, exactly like
// the already-pinned "unterminated narvi-command fence" case above it in
// this file.
func TestExtractCommandBlocks_UnterminatedEnclosingFenceAtEOF(t *testing.T) {
	content := []byte("# Web Guide\n\n" +
		"````\n\n" +
		"```json narvi-command\n" +
		"{\"name\": \"Hidden\", \"route\": \"GET /hidden\"}\n" +
		"```\n")

	_, err := extractCommandBlocks("web.md", content)
	if err == nil {
		t.Fatal("extractCommandBlocks() returned nil error, want an error for an enclosing fence still open at EOF")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("extractCommandBlocks() error = %q, want it to mention an unterminated fence", err.Error())
	}
}

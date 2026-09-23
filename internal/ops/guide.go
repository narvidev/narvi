package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// commandFenceOpen/commandFenceClose delimit one machine-checkable
// "narvi-command" block inside a per-surface guide markdown file — a
// fenced ```json code block with this exact info string, e.g.:
//
//	```json narvi-command
//	{"name": "Create a session", "route": "POST /api/sessions"}
//	```
//
// JSON (not YAML) deliberately: this codebase already commits dashboards/
// alerts as JSON (schema.go) and has no YAML dependency anywhere —
// reusing JSON keeps this a genuinely narrower extension of the same
// mechanism, and encoding/json's own strict decoding is exactly the
// "malformed input fails loudly" behavior LoadGuides needs (§10-P6: "a CI
// check ties every documented command to the route ... that actually
// implements it, so the guide can never drift into aspirational text").
const (
	commandFenceOpen  = "```json narvi-command"
	commandFenceClose = "```"
)

// validRouteMethods is the exact set of HTTP methods a GuideCommand.Route
// may name — mirrors chiRouterMethods (routes.go), spelled upper-case to
// match the "METHOD /path" convention every route string in this
// package's own maps and every guide file uses.
var validRouteMethods = map[string]bool{
	"GET":    true,
	"POST":   true,
	"PUT":    true,
	"PATCH":  true,
	"DELETE": true,
}

// ClassifierRef is a GuideCommand's OTHER binding shape (§18.4): "this
// documented behavior is a real §18.4 routing-decision outcome", rather
// than a literal HTTP route. Target/Mode/Source are each OPTIONAL —
// omitted means "this surface's own real Classify call decides it
// per-input" (e.g. Slack/Linear's free-text prompts, which supply no
// DeterministicTarget) rather than a value CheckGuideDrift can pin down
// structurally; PRESENT means a real, checkable claim (e.g. GitHub's own
// @mention/label triggers, whose Target is deterministically corroborated
// to "review" every time — coalesce.go's own doc comment). Surface is
// always required: every classifier-bound command claims to be routed by
// SOME real ingress surface.
type ClassifierRef struct {
	Surface string `json:"surface"`
	Target  string `json:"target,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Source  string `json:"source,omitempty"`
}

// GuideCommand is one documented, machine-checkable accepted command in a
// per-surface guide — the unit CheckGuideDrift verifies against either a
// real ScanRegisteredRoutes entry or a real IntentVocabulary value.
// Exactly one of Route/Classifier is set (Validate enforces this) — a
// documented command is either "this HTTP route implements it" or "this
// §18.4 routing outcome implements it", never both, never neither (a
// command with neither would be exactly the unchecked aspirational prose
// this whole mechanism exists to forbid).
type GuideCommand struct {
	// Name is the human-readable command description shown in the guide
	// prose immediately around this block, and the identifier
	// GuideDriftError.Command names.
	Name string `json:"name"`
	// Route is "METHOD /path", checked verbatim against
	// ScanRegisteredRoutes's own joined-path keys (routes.go).
	Route string `json:"route,omitempty"`
	// Classifier is checked against a real ScanIntentVocabulary result.
	Classifier *ClassifierRef `json:"classifier,omitempty"`

	sourcePath string
	line       int
}

// Validate reports the first structural problem with c, mirroring
// Dashboard.Validate/Alert.Validate's own "every field the loader
// requires present and every enum-shaped field's value actually legal"
// discipline (schema.go).
func (c GuideCommand) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("ops: guide command: name is required")
	}
	hasRoute := c.Route != ""
	hasClassifier := c.Classifier != nil
	if hasRoute == hasClassifier {
		return fmt.Errorf("ops: guide command %q: exactly one of \"route\" or \"classifier\" is required", c.Name)
	}
	if hasRoute {
		parts := strings.Fields(c.Route)
		if len(parts) != 2 || !validRouteMethods[parts[0]] || !strings.HasPrefix(parts[1], "/") {
			return fmt.Errorf("ops: guide command %q: route %q must look like \"METHOD /path\" (METHOD one of GET/POST/PUT/PATCH/DELETE)", c.Name, c.Route)
		}
	}
	if hasClassifier && strings.TrimSpace(c.Classifier.Surface) == "" {
		return fmt.Errorf("ops: guide command %q: classifier.surface is required", c.Name)
	}
	return nil
}

// SurfaceGuide is one docs/guides/*.md file's own parsed shape — one
// ingress surface's worth of documented commands, mirroring Dashboard's
// own "one file, one concern" convention (schema.go's LoadDashboards doc
// comment).
type SurfaceGuide struct {
	// Surface is the filename stem (docs/guides/web.md -> "web") — the
	// guide's own claimed ingress surface, itself checked against
	// IntentVocabulary.Surfaces by CheckGuideDrift (a guide file for a
	// surface that does not exist is exactly as much drift as a command
	// referencing a dead route).
	Surface string
	// Title is the file's first "# " heading.
	Title    string
	Commands []GuideCommand

	sourcePath string
}

// SourcePath is the file SurfaceGuide was loaded from.
func (g SurfaceGuide) SourcePath() string { return g.sourcePath }

// LoadGuides reads every "*.md" file directly inside dir EXCEPT
// README.md (the human-facing index, not a per-surface guide — mirrors
// docs/runbooks/README.md's identical role there), in deterministic
// (sorted-by-filename) order, extracts every narvi-command block, and
// requires at least one per file. The first error — a missing directory,
// an unterminated fence, malformed JSON inside one, a GuideCommand that
// fails Validate, or a file with zero command blocks or no "# " title —
// aborts and is returned: the SAME "fail closed at the edge, never
// silently skip a bad file" discipline LoadDashboards/LoadAlerts already
// establish (schema.go), required verbatim by this Step's own brief ("a
// malformed or unparseable guide file must fail the check, never be
// silently skipped").
func LoadGuides(dir string) ([]SurfaceGuide, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ops: read dir %s: %w", dir, err)
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		if e.Name() == "README.md" {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)

	out := make([]SurfaceGuide, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("ops: read %s: %w", p, err)
		}

		commands, err := extractCommandBlocks(p, raw)
		if err != nil {
			return nil, err
		}
		if len(commands) == 0 {
			return nil, fmt.Errorf("ops: %s: at least one narvi-command block is required", p)
		}

		title := firstHeading(raw)
		if title == "" {
			return nil, fmt.Errorf("ops: %s: missing a top-level \"# \" heading", p)
		}

		out = append(out, SurfaceGuide{
			Surface:    strings.TrimSuffix(filepath.Base(p), ".md"),
			Title:      title,
			Commands:   commands,
			sourcePath: p,
		})
	}

	return out, nil
}

// htmlCommentOpen/htmlCommentClose delimit an HTML comment in a guide's
// raw markdown source — content a real markdown renderer (and therefore
// every reader) never shows. extractCommandBlocks must not accept a
// narvi-command fence found between these as real documentation: a route
// "documented" only inside a comment is exactly as undocumented as one
// with no guide entry at all, just harder to notice in a diff.
const (
	htmlCommentOpen  = "<!--"
	htmlCommentClose = "-->"
)

// leadingBacktickRun returns the length of s's leading run of '`'
// characters (0 if s does not start with one) — the minimum needed to
// recognize a markdown fence delimiter line without a full CommonMark
// parser: a fence opens with a run of 3-or-more backticks, optionally
// followed by an info string.
func leadingBacktickRun(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

// isBareFenceCloseLine reports whether trimmed can CLOSE an already-open
// fence that itself opened with a backtick run of length need. Real
// CommonMark closing-fence syntax is strict about this: unlike an opening
// fence, a closing fence line may contain nothing but the backtick run
// itself (no info string) — "```json narvi-command" can OPEN a fence but
// can never CLOSE one, exactly like a real markdown renderer treats it.
// That asymmetry is exactly what lets a narvi-command fence nest, as
// inert literal text, inside a longer outer example fence: the outer
// fence's own bare "```"/"````" close is the only line able to end it: an
// inner "```json narvi-command" line never can, so it never
// (mis-)terminates the outer fence early and is never itself mistaken for
// live documentation while the outer fence remains open.
func isBareFenceCloseLine(trimmed string, need int) bool {
	if trimmed == "" {
		return false
	}
	for _, r := range trimmed {
		if r != '`' {
			return false
		}
	}
	return len(trimmed) >= need
}

// extractCommandBlocks is a deliberately simple, purpose-built line
// scanner over content — not a general CommonMark parser (this repo
// controls every byte of docs/guides/*.md, exactly like schema.go's own
// JSON loader controls every byte of deploy/observability/*.json; a full
// markdown parser would be a dependency this narrow, strict extraction
// doesn't need). It looks for a line matching commandFenceOpen EXACTLY
// (after trimming surrounding whitespace), OUTSIDE both an HTML comment
// and any other, enclosing markdown fence, and collects every following
// line up to a line matching commandFenceClose exactly, as one block's
// raw JSON. An opened fence with no matching close before EOF is a hard
// error (fail closed) rather than silently dropping the trailing partial
// block — and so are an HTML comment or an enclosing fence STILL OPEN at
// EOF (checked once the line loop finishes, below): either one, left
// open, would otherwise swallow every remaining line — including every
// narvi-command block after it — with no error and no visible sign in a
// diff, exactly the "looks like it was checked but wasn't" failure this
// whole mechanism exists to refuse. See
// TestExtractCommandBlocks_UnterminatedHTMLCommentAtEOF/
// TestExtractCommandBlocks_UnterminatedEnclosingFenceAtEOF.
//
// Comment-open and fence-open detection both follow real CommonMark
// rules, not a loose substring test — a loose test has a concrete false-
// positive cost against guide prose that mentions either syntax:
//
//   - HTML-comment mode is entered only when the TRIMMED line STARTS WITH
//     htmlCommentOpen, never merely CONTAINS it. Prose like "The plan
//     message embeds a hidden `<!--` marker so edits can find it." is an
//     inline-code example of the syntax, not a real opening comment, and
//     must not be treated as one — a loose "contains" test would
//     enter comment mode on that line and never leave, silently dropping
//     every narvi-command block after it to EOF. See
//     TestExtractCommandBlocks_InlineHTMLCommentMarkerInProseIsNotAComment.
//   - A line is treated as an ENCLOSING fence opener only when its info
//     string (everything after the leading backtick run) itself contains
//     NO backtick. Prose like the sentence "```x``` is inline code"
//     starts with a 3-backtick run but is not a real fence open; a loose
//     "3-or-more leading backticks" test would wrongly enter enclosing-
//     fence mode on that line and treat the very next real narvi-command
//     fence as literal text inside it. See
//     TestExtractCommandBlocks_FenceInfoStringWithBacktickIsNotAFenceOpener.
//
// Two shapes of "looks documented but no reader can ever see it" are
// refused, not merely mis-parsed:
//
//   - A narvi-command fence written between htmlCommentOpen/
//     htmlCommentClose is invisible to a rendered guide — it is simply
//     never scanned for at all while a comment is open, so it produces no
//     GuideCommand and CheckGuideDrift's completeness check treats the
//     route it named as still undocumented, exactly as if the fence were
//     never written.
//   - A narvi-command fence nested inside a longer, unrelated example
//     fence (e.g. a "how to document a route" walkthrough wrapped in its
//     own ```` ```` ```` block) is real, visible markdown, but it
//     documents the FORMAT, not a live claim that guide makes about that
//     specific route — exactly the illustrative-not-authoritative text
//     this mechanism must not treat as a real binding. This scanner
//     tracks at most one open "enclosing" fence at a time (true
//     CommonMark fences do not nest either: a fence's own content is
//     entirely literal, including anything that looks like another fence,
//     until its own matching close), so any narvi-command-shaped line
//     found while one is open is left as literal text, never extracted.
//
// All four shapes above are mutation-tested in guide_test.go
// (TestExtractCommandBlocks_HiddenFenceInHTMLComment/
// TestExtractCommandBlocks_HiddenFenceInEnclosingFence/
// TestExtractCommandBlocks_InlineHTMLCommentMarkerInProseIsNotAComment/
// TestExtractCommandBlocks_FenceInfoStringWithBacktickIsNotAFenceOpener/
// TestExtractCommandBlocks_UnterminatedHTMLCommentAtEOF/
// TestExtractCommandBlocks_UnterminatedEnclosingFenceAtEOF).
func extractCommandBlocks(sourcePath string, content []byte) ([]GuideCommand, error) {
	lines := strings.Split(string(content), "\n")

	var commands []GuideCommand
	inComment := false
	commentStartLine := 0
	enclosingFenceLen := 0 // 0 = not currently inside an unrelated fence
	enclosingFenceStartLine := 0
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(raw)

		if inComment {
			if strings.Contains(raw, htmlCommentClose) {
				inComment = false
			}
			continue
		}

		if enclosingFenceLen > 0 {
			if isBareFenceCloseLine(trimmed, enclosingFenceLen) {
				enclosingFenceLen = 0
			}
			continue // every line inside an enclosing fence is literal text
		}

		if strings.HasPrefix(trimmed, htmlCommentOpen) {
			if !strings.Contains(raw, htmlCommentClose) {
				inComment = true
				commentStartLine = i + 1
			}
			continue // a same-line "<!-- ... -->" comment hides its own line too
		}

		if trimmed != commandFenceOpen {
			if run := leadingBacktickRun(trimmed); run >= 3 && !strings.Contains(trimmed[run:], "`") {
				enclosingFenceLen = run // entering an unrelated fence
				enclosingFenceStartLine = i + 1
			}
			continue
		}

		startLine := i + 1 // 1-indexed, the fence-open line itself

		var body strings.Builder
		i++
		closed := false
		for ; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == commandFenceClose {
				closed = true
				break
			}
			body.WriteString(lines[i])
			body.WriteString("\n")
		}
		if !closed {
			return nil, fmt.Errorf("ops: %s:%d: unterminated %q fence", sourcePath, startLine, commandFenceOpen)
		}

		var cmd GuideCommand
		dec := json.NewDecoder(strings.NewReader(body.String()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cmd); err != nil {
			return nil, fmt.Errorf("ops: %s:%d: malformed narvi-command JSON: %w", sourcePath, startLine, err)
		}
		cmd.sourcePath = sourcePath
		cmd.line = startLine
		if err := cmd.Validate(); err != nil {
			return nil, fmt.Errorf("ops: %s:%d: %w", sourcePath, startLine, err)
		}

		commands = append(commands, cmd)
	}

	// An HTML comment or enclosing fence still open when the file ends is
	// refused, not silently accepted -- see this function's own top doc
	// comment for why silently accepting either here is the actual defect
	// this rewrite fixes: each swallows every line after it, so treating
	// EOF as an implicit close would drop every real narvi-command block
	// past that point with no error at all.
	if inComment {
		return nil, fmt.Errorf("ops: %s:%d: unterminated %q comment (still open at EOF)", sourcePath, commentStartLine, htmlCommentOpen)
	}
	if enclosingFenceLen > 0 {
		return nil, fmt.Errorf("ops: %s:%d: unterminated enclosing fence opened with a %d-backtick run (still open at EOF)", sourcePath, enclosingFenceStartLine, enclosingFenceLen)
	}

	return commands, nil
}

// firstHeading returns the text of content's first top-level ("# ")
// markdown heading, or "" if none exists.
func firstHeading(content []byte) string {
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

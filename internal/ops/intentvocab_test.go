package ops

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestScanIntentVocabulary_ExtractsPrefixedStringConstants proves the
// scanner's own core job across both scan roots: every Target/Mode/
// RecordSource-prefixed string constant under intentRoot lands in its own
// bucket by VALUE (never by identifier name), every SessionSpawnSource-
// prefixed one under sqlcgenRoot lands in Surfaces, and an unrelated
// constant (wrong prefix, or a non-string value) is excluded from every
// bucket.
func TestScanIntentVocabulary_ExtractsPrefixedStringConstants(t *testing.T) {
	intentDir := t.TempDir()
	intentSrc := `package intent

const (
	TargetReview  = "review"
	TargetRequest = "request"
)

const (
	ModePlan  = "plan"
	ModeBuild = "build"
)

const (
	RecordSourceClassifier = "classifier"
	RecordSourceExplicit   = "explicit"
	RecordSourceFallback   = "fallback"
)

// ConfidenceHigh is deliberately a DIFFERENT prefix -- must never land in
// any of the three buckets above.
const ConfidenceHigh = "high"

// MaxReasoningLength is deliberately a non-string constant -- must never
// land in any bucket even though its own name starts with nothing that
// matches, proving int/iota-shaped constants are silently skipped.
const MaxReasoningLength = 2000
`
	if err := os.WriteFile(filepath.Join(intentDir, "rubric.go"), []byte(intentSrc), 0o644); err != nil {
		t.Fatalf("write rubric.go: %v", err)
	}

	sqlcDir := t.TempDir()
	sqlcSrc := `package sqlcgen

type SessionSpawnSource string

const (
	SessionSpawnSourceWeb    SessionSpawnSource = "web"
	SessionSpawnSourceSlack  SessionSpawnSource = "slack"
	SessionSpawnSourceLinear SessionSpawnSource = "linear"
	SessionSpawnSourceGithub SessionSpawnSource = "github"
	SessionSpawnSourceMcp    SessionSpawnSource = "mcp"
)

// SessionStatusReady is a DIFFERENT enum entirely -- must never land in
// Surfaces.
const SessionStatusReady = "ready"
`
	if err := os.WriteFile(filepath.Join(sqlcDir, "models.go"), []byte(sqlcSrc), 0o644); err != nil {
		t.Fatalf("write models.go: %v", err)
	}

	got, err := ScanIntentVocabulary(intentDir, sqlcDir)
	if err != nil {
		t.Fatalf("ScanIntentVocabulary: %v", err)
	}

	for _, want := range []string{"review", "request"} {
		if !got.Targets[want] {
			t.Errorf("missing target %q in %v", want, got.Targets)
		}
	}
	for _, want := range []string{"plan", "build"} {
		if !got.Modes[want] {
			t.Errorf("missing mode %q in %v", want, got.Modes)
		}
	}
	for _, want := range []string{"classifier", "explicit", "fallback"} {
		if !got.Sources[want] {
			t.Errorf("missing record source %q in %v", want, got.Sources)
		}
	}
	for _, want := range []string{"web", "slack", "linear", "github", "mcp"} {
		if !got.Surfaces[want] {
			t.Errorf("missing surface %q in %v", want, got.Surfaces)
		}
	}

	if got.Targets["high"] || got.Modes["high"] || got.Sources["high"] {
		t.Errorf("ConfidenceHigh's value leaked into a bucket its own prefix does not belong to: %+v", got)
	}
	if len(got.Surfaces) != 5 {
		t.Errorf("Surfaces = %v, want exactly the 5 real spawn sources (SessionStatusReady must be excluded)", got.Surfaces)
	}
}

// TestScanIntentVocabulary_SkipsTestFiles mirrors this package's other two
// scanners' identical convention.
func TestScanIntentVocabulary_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	src := `package intent

const TargetTestOnly = "test_only"
`
	if err := os.WriteFile(filepath.Join(dir, "rubric_test.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write rubric_test.go: %v", err)
	}
	got, err := ScanIntentVocabulary(dir, dir)
	if err != nil {
		t.Fatalf("ScanIntentVocabulary: %v", err)
	}
	if got.Targets["test_only"] {
		t.Error("a _test.go file's own constant must be skipped, not counted as registered")
	}
}

// TestScanIntentVocabulary_SurfacesMatchTheMigrations reads the real
// sqlcgen package and the real migrations: the Surfaces ScanIntentVocabulary
// finds are exactly the session_spawn_source values the up migrations
// create or add. A migration that adds a value without a sqlc regeneration
// fails here, whether or not any code uses the new constant yet -- and so
// does a guide naming that value as its surface, which CheckGuideDrift
// would otherwise refuse. The list is also pinned, so a lost migration
// fails too.
func TestScanIntentVocabulary_SurfacesMatchTheMigrations(t *testing.T) {
	root := repoRoot(t)
	vocab, err := ScanIntentVocabulary(
		filepath.Join(root, "internal", "domain", "intent"),
		filepath.Join(root, "internal", "adapters", "outbound", "postgres", "sqlcgen"),
	)
	if err != nil {
		t.Fatalf("ScanIntentVocabulary: %v", err)
	}
	var surfaces []string
	for s := range vocab.Surfaces {
		surfaces = append(surfaces, s)
	}
	sort.Strings(surfaces)

	fromMigrations := spawnSourcesFromUpMigrations(t, MigrationsDir(root))
	sort.Strings(fromMigrations)

	if !slices.Equal(surfaces, fromMigrations) {
		t.Errorf("sqlcgen's SessionSpawnSource constants = %v, but the up migrations define %v: regenerate sqlc (`sqlc generate`)", surfaces, fromMigrations)
	}
	if want := []string{"github", "linear", "mcp", "slack", "web"}; !slices.Equal(fromMigrations, want) {
		t.Errorf("session_spawn_source values in the up migrations = %v, want %v", fromMigrations, want)
	}
}

var (
	createSpawnSourceType = regexp.MustCompile(`(?is)CREATE\s+TYPE\s+session_spawn_source\s+AS\s+ENUM\s*\(([^)]*)\)`)
	addSpawnSourceValue   = regexp.MustCompile(`(?i)ALTER\s+TYPE\s+session_spawn_source\s+ADD\s+VALUE\s+(?:IF\s+NOT\s+EXISTS\s+)?'([^']*)'`)
	sqlStringLiteral      = regexp.MustCompile(`'([^']*)'`)
)

// spawnSourcesFromUpMigrations returns every session_spawn_source value the
// up migrations under dir create or add, with SQL line comments removed
// first, so a value merely mentioned in a comment is not counted.
func spawnSourcesFromUpMigrations(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatalf("list up migrations in %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no up migrations in %s", dir)
	}
	var values []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var code strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			code.WriteString(line)
			code.WriteString("\n")
		}
		sql := code.String()
		for _, m := range createSpawnSourceType.FindAllStringSubmatch(sql, -1) {
			for _, lit := range sqlStringLiteral.FindAllStringSubmatch(m[1], -1) {
				values = append(values, lit[1])
			}
		}
		for _, m := range addSpawnSourceValue.FindAllStringSubmatch(sql, -1) {
			values = append(values, m[1])
		}
	}
	if len(values) == 0 {
		t.Fatalf("found no session_spawn_source values in %s -- almost certainly a parse bug", dir)
	}
	return values
}

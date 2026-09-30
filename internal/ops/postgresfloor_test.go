package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

// postgresFloorPhrase is the one phrase the documents below state the oldest
// supported Postgres server in.
var postgresFloorPhrase = regexp.MustCompile(`\bPostgres (\d{1,2}) or later\b`)

// testedOnPhrase names the tested version, not the floor, and is the one
// other way the documents below may name a Postgres major.
var testedOnPhrase = regexp.MustCompile(`\btested on Postgres \d{1,2}\b`)

// otherFloorWordings are the other ways a floor could be worded. Each is
// refused in the documents below, whatever its number, so a floor stated
// outside postgresFloorPhrase fails here instead of going stale when the
// constant moves. A bare "Postgres N" is among them.
var otherFloorWordings = []*regexp.Regexp{
	regexp.MustCompile(`\b\d{1,2}(?:\.\d+)? (?:or|and) (?:later|newer|above|higher)\b`),
	regexp.MustCompile(`(?i)\bat least (?:postgres(?:ql)? |version )?\d{1,2}\b`),
	regexp.MustCompile(`(?i)\bpostgres(?:ql)? ?(?:≥|>=) ?\d{1,2}`),
	regexp.MustCompile(`(?i)\bpostgres(?:ql)? \d{1,2}\+`),
	regexp.MustCompile(`(?i)\bversion \d{1,2}(?:[^.\d]|$)`),
	regexp.MustCompile(`\bPostgreSQL \d`),
	regexp.MustCompile(`\bPostgres \d{1,2}\b`),
}

// quotedSpan is text quoted from elsewhere, such as what a document said
// before: a quotation is not a statement of the floor.
var quotedSpan = regexp.MustCompile(`"[^"\n]*"`)

// postgresFloorImage is the image `make test-integration-postgres-floor`
// runs the floor's suites on.
var postgresFloorImage = regexp.MustCompile(`(?m)^POSTGRES_FLOOR_IMAGE := postgres:(\d+)-`)

// floorDoc is a document that states the floor. region, when set, cuts it
// to the part checked for other wordings: the technical plan is long, and
// only §1 and §5.1 state the floor. The phrase itself is checked in the
// whole document.
type floorDoc struct {
	rel    string
	region func(string) (string, error)
}

// planSections returns the technical plan's §1 and §5.1.
func planSections(plan string) (string, error) {
	var out strings.Builder
	for _, bounds := range [][2]string{{"\n## 1. ", "\n## 2. "}, {"\n### 5.1 ", "\n### 5.2 "}} {
		start := strings.Index(plan, bounds[0])
		if start < 0 {
			return "", fmt.Errorf("no heading %q", strings.TrimSpace(bounds[0]))
		}
		end := strings.Index(plan[start:], bounds[1])
		if end < 0 {
			return "", fmt.Errorf("no heading %q after %q", strings.TrimSpace(bounds[1]), strings.TrimSpace(bounds[0]))
		}
		out.WriteString(plan[start : start+end])
	}
	return out.String(), nil
}

// TestPostgresFloor_StatedWhereBootRefusesIt checks that every document
// stating the oldest supported Postgres server states the major version boot
// refuses below (platform.MinPostgresServerVersionNum): the production
// checklist an operator reads, technical plan §1 and §5.1, and the deploy
// secret beside the database URL. Each states it only as "Postgres N or
// later", at least once. Any other wording this test recognises
// (otherFloorWordings) is refused outright, whatever its number, so a floor
// moved in the constant cannot leave a differently worded statement behind;
// "tested on Postgres N" names the tested version and is allowed, and a
// quotation, in double quotes, is not a statement and is skipped. And the CI
// suites run on the floor must run on that same version.
func TestPostgresFloor_StatedWhereBootRefusesIt(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	floor := platform.MinPostgresServerVersionNum / 10000
	if platform.MinPostgresServerVersionNum%10000 != 0 {
		t.Fatalf("MinPostgresServerVersionNum = %d, want a major version's first release (major × 10000): the documents state a major", platform.MinPostgresServerVersionNum)
	}

	for _, doc := range []floorDoc{
		{rel: filepath.Join("docs", "PRODUCTION_CHECKLIST.md")},
		{rel: filepath.Join("docs", "TECHNICAL_PLAN.md"), region: planSections},
		{rel: filepath.Join("deploy", "control-plane", "secret.yaml")},
	} {
		raw, err := os.ReadFile(filepath.Join(root, doc.rel))
		if err != nil {
			t.Fatalf("read %s: %v", doc.rel, err)
		}
		text := quotedSpan.ReplaceAllString(string(raw), `""`)

		matches := postgresFloorPhrase.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			t.Errorf("%s states no Postgres floor (%q), want it to name Postgres %d", doc.rel, postgresFloorPhrase, floor)
		}
		for _, m := range matches {
			if got, _ := strconv.Atoi(m[1]); got != floor {
				t.Errorf("%s says %q, but boot refuses a server older than Postgres %d (platform.MinPostgresServerVersionNum)", doc.rel, m[0], floor)
			}
		}

		region := text
		if doc.region != nil {
			if region, err = doc.region(text); err != nil {
				t.Fatalf("%s: %v", doc.rel, err)
			}
		}
		region = postgresFloorPhrase.ReplaceAllString(region, "")
		region = testedOnPhrase.ReplaceAllString(region, "")
		for _, re := range otherFloorWordings {
			for _, m := range re.FindAllString(region, -1) {
				t.Errorf("%s names a Postgres version as %q: state the floor only as \"Postgres N or later\", which this test holds to platform.MinPostgresServerVersionNum", doc.rel, strings.TrimSpace(m))
			}
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	m := postgresFloorImage.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("the Makefile sets no POSTGRES_FLOOR_IMAGE (%q)", postgresFloorImage)
	}
	if got, _ := strconv.Atoi(string(m[1])); got != floor {
		t.Errorf("the Makefile runs the floor's suites on %q, but boot refuses a server older than Postgres %d", m[0], floor)
	}
}

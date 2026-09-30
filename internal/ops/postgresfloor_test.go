package ops

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/narvidev/narvi/internal/platform"
)

// postgresFloorStatement is how a document states the oldest Postgres
// server the control plane runs against.
var postgresFloorStatement = regexp.MustCompile(`Postgres (\d+) or later`)

// postgresFloorImage is the image `make test-integration-postgres-floor`
// runs the floor's suites on.
var postgresFloorImage = regexp.MustCompile(`(?m)^POSTGRES_FLOOR_IMAGE := postgres:(\d+)-`)

// TestPostgresFloor_StatedWhereBootRefusesIt checks that every document
// stating the oldest supported Postgres server names the major version boot
// refuses below (platform.MinPostgresServerVersionNum): the production
// checklist an operator reads, technical plan §1's stack choices, and the
// deploy secret's database URL. And that the CI suites run on the floor run
// on that same version. Moving the floor fails here until every one of them
// moves with it.
func TestPostgresFloor_StatedWhereBootRefusesIt(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	floor := platform.MinPostgresServerVersionNum / 10000
	if platform.MinPostgresServerVersionNum%10000 != 0 {
		t.Fatalf("MinPostgresServerVersionNum = %d, want a major version's first release (major × 10000): the documents state a major", platform.MinPostgresServerVersionNum)
	}

	for _, rel := range []string{
		filepath.Join("docs", "PRODUCTION_CHECKLIST.md"),
		filepath.Join("docs", "TECHNICAL_PLAN.md"),
		filepath.Join("deploy", "control-plane", "secret.yaml"),
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		matches := postgresFloorStatement.FindAllSubmatch(raw, -1)
		if len(matches) == 0 {
			t.Errorf("%s states no Postgres floor (%q), want it to name Postgres %d", rel, postgresFloorStatement, floor)
		}
		for _, m := range matches {
			if got, _ := strconv.Atoi(string(m[1])); got != floor {
				t.Errorf("%s says %q, but boot refuses a server older than Postgres %d (platform.MinPostgresServerVersionNum)", rel, m[0], floor)
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

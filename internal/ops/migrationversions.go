package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// MigrationFile is one golang-migrate file under /migrations, split into
// the parts this package needs to check: its numeric version, the slug
// after it, and which direction it runs.
type MigrationFile struct {
	Version   int
	Slug      string
	Direction string // "up" or "down"
	Name      string // the file's own base name, for error messages
}

// LoadMigrationFiles parses every *.sql file directly under dir into a
// MigrationFile. A name that does not match golang-migrate's own
// NNNNNN_slug.{up,down}.sql shape is an error rather than a skip, and that
// choice is the point: golang-migrate itself does the opposite. Its iofs
// driver calls source.DefaultParse per entry and `continue`s on a parse
// error (source/iofs/iofs.go, v4.19.1), so a migration whose filename is
// merely typo'd is silently never run and nothing anywhere says so. This
// loader refuses instead, because a migration no parser can see is a
// migration no check below covers.
func LoadMigrationFiles(dir string) ([]MigrationFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %s: %w", dir, err)
	}

	var out []MigrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		mf, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Direction < out[j].Direction
	})
	return out, nil
}

func parseMigrationName(name string) (MigrationFile, error) {
	base := strings.TrimSuffix(name, ".sql")

	var direction string
	switch {
	case strings.HasSuffix(base, ".up"):
		direction, base = "up", strings.TrimSuffix(base, ".up")
	case strings.HasSuffix(base, ".down"):
		direction, base = "down", strings.TrimSuffix(base, ".down")
	default:
		return MigrationFile{}, fmt.Errorf("migration %q: name must end .up.sql or .down.sql", name)
	}

	under := strings.Index(base, "_")
	if under <= 0 {
		return MigrationFile{}, fmt.Errorf("migration %q: name must be NNNNNN_slug.%s.sql", name, direction)
	}
	digits := base[:under]
	version, err := strconv.Atoi(digits)
	if err != nil {
		return MigrationFile{}, fmt.Errorf("migration %q: version prefix %q is not a number: %w", name, digits, err)
	}
	if len(digits) != 6 {
		return MigrationFile{}, fmt.Errorf("migration %q: version prefix %q must be exactly 6 digits", name, digits)
	}
	slug := base[under+1:]
	if slug == "" {
		return MigrationFile{}, fmt.Errorf("migration %q: slug after the version is empty", name)
	}
	return MigrationFile{Version: version, Slug: slug, Direction: direction, Name: name}, nil
}

// CheckMigrationVersions returns one message per structural problem in a
// set of migration files, empty when there are none.
//
// Why this is a check and not a convention. golang-migrate's iofs driver
// returns source.ErrDuplicateMigration from Init when two files share a
// (version, direction) -- verified in source/iofs/iofs.go at v4.19.1, the
// version this module pins. Init, not the migration step: the driver never
// constructs, so a duplicate does not degrade gracefully or skip one file,
// it stops every migration run including a fresh deployment's first.
//
// And the way a duplicate arrives is not carelessness: two branches
// developed in parallel each take "the next number" from the same main,
// each is individually green, and the collision exists only in the merge
// that neither branch's CI ever built. That is why this runs as an
// ordinary unit test over the real directory rather than living in a
// contributing guide -- with the caveat that it binds on whatever tree it
// runs against, so it catches such a pair on main immediately after the
// second merge, and on a branch only once that branch is brought up to
// date. Requiring branches to be current before merge is what turns it
// from a fast alarm into a gate.
//
// Sequential-with-no-gaps is deliberately NOT required HERE. A gap that
// stays a gap forever (an abandoned branch's own reserved-then-unused
// number) is harmless to golang-migrate, and demanding contiguity would
// turn every such branch into a renumbering chore, which is how a check
// earns being disabled.
//
// U5 audit fix: that is a narrower claim than "gaps are always harmless",
// and the distinction matters. golang-migrate's postgres driver tracks a
// SINGLE applied version (schema_migrations' own one-row version+dirty
// shape), not a per-migration applied set -- Up() walks forward from
// whatever that stored version already is. A gap that gets FILLED IN
// LATER, after a HIGHER-numbered migration has already run on a real
// environment, is therefore silently and PERMANENTLY skipped: the
// backfilled migration's own version is now behind the stored one, so
// Up() never revisits it, on that environment, ever. This is real and not
// hypothetical for the tree this batch lands in: this branch depends on
// 000135 (owned by a sibling branch, not yet merged) existing before
// 000136/000137 run anywhere for real. CheckMigrationGaps below is the
// check for THAT hazard -- see its own doc comment for why it must never
// run as an ordinary `go test` next to this one (a legitimately pending
// branch would fail its own CI for a gap that is not yet a mistake).
func CheckMigrationVersions(files []MigrationFile) []string {
	var problems []string

	// One up and one down per version, and never two of either.
	type pair struct{ up, down []string }
	byVersion := map[int]*pair{}
	order := []int{}
	for _, f := range files {
		p, ok := byVersion[f.Version]
		if !ok {
			p = &pair{}
			byVersion[f.Version] = p
			order = append(order, f.Version)
		}
		if f.Direction == "up" {
			p.up = append(p.up, f.Name)
		} else {
			p.down = append(p.down, f.Name)
		}
	}
	sort.Ints(order)

	for _, v := range order {
		p := byVersion[v]
		if len(p.up) > 1 {
			problems = append(problems, fmt.Sprintf(
				"version %06d has %d .up.sql files (%s) -- golang-migrate refuses a duplicate version, so this breaks every migration run, not just this one",
				v, len(p.up), strings.Join(p.up, ", ")))
		}
		if len(p.down) > 1 {
			problems = append(problems, fmt.Sprintf(
				"version %06d has %d .down.sql files (%s) -- golang-migrate refuses a duplicate version",
				v, len(p.down), strings.Join(p.down, ", ")))
		}
		if len(p.up) == 0 {
			problems = append(problems, fmt.Sprintf("version %06d has a .down.sql but no .up.sql (%s)", v, strings.Join(p.down, ", ")))
		}
		if len(p.down) == 0 {
			problems = append(problems, fmt.Sprintf("version %06d has an .up.sql but no .down.sql (%s) -- migrations/README.md requires each .down.sql to undo its own .up.sql", v, strings.Join(p.up, ", ")))
		}
		// A shared version with mismatched slugs is the parallel-branch
		// collision in its most confusing form: the numbers agree, the
		// names do not, and a reader cannot tell which pair is which.
		if len(p.up) == 1 && len(p.down) == 1 {
			upSlug, _ := parseMigrationName(p.up[0])
			downSlug, _ := parseMigrationName(p.down[0])
			if upSlug.Slug != downSlug.Slug {
				problems = append(problems, fmt.Sprintf(
					"version %06d pairs slug %q (up) with %q (down) -- a pair must share one slug",
					v, upSlug.Slug, downSlug.Slug))
			}
		}
	}
	return problems
}

// CheckMigrationGaps returns one message per missing version number
// strictly between the lowest and highest version present in files --
// U5 audit fix (my own mistake, not a prior implementer's: I renumbered
// this branch's own migration from 000135 to 000136 because a sibling
// branch had also taken 000135, and described the resulting merge order
// as a constraint someone would remember. The review established it is
// worse than that -- see CheckMigrationVersions' own doc comment above for
// the exact mechanism (golang-migrate's postgres driver tracks a single
// applied version, not a per-migration set) that makes a backfilled gap a
// PERMANENT skip on any environment that already migrated past the higher
// number, not merely a confusing ordering rule.
//
// # Why this cannot run as an ordinary `go test`, next to CheckMigrationVersions
//
// This branch legitimately HAS a gap (000135) for as long as the sibling
// that owns it has not yet merged -- that is not a mistake, it is the
// expected, temporary state of two branches developed in parallel, each
// individually green. Running this check inside `go test -race ./...`
// (which this repo's own `make test` runs on EVERY push AND EVERY pull
// request, per .github/workflows/ci.yml) would fail THIS branch's own CI
// for exactly the reason U5 says is fine to have, defeating the entire
// point: a check that blocks legitimate, temporary, in-flight work trains
// people to work around it, which is how a check earns being disabled
// (CheckMigrationVersions' own doc comment, same file, makes the identical
// point about a DIFFERENT hazard).
//
// The property this check actually needs to enforce is "no gap survives a
// MERGE to main" -- main is the one tree every real deployment's own
// migration run actually walks, and once code lands there, every
// migration that is part of main's own history must already be present:
// a gap on main is never legitimately temporary the way one on a feature
// branch is. TestNoMigrationGaps (migrationversions_test.go) is therefore
// gated on running ONLY when this process is CI's own post-merge run
// against main (GITHUB_EVENT_NAME == "push" && GITHUB_REF_NAME == "main"
// -- GitHub Actions' own default env vars, requiring no workflow-file
// change to populate) -- skipped everywhere else: a developer's own
// machine, and every pull-request CI run, THIS branch's own included. See
// that test's own doc comment for the full "why here, and why this is the
// honest resolution rather than a workflow-level `if:` this package
// cannot see or enforce on its own".
func CheckMigrationGaps(files []MigrationFile) []string {
	if len(files) == 0 {
		return nil
	}

	present := map[int]bool{}
	minVersion, maxVersion := files[0].Version, files[0].Version
	for _, f := range files {
		present[f.Version] = true
		if f.Version < minVersion {
			minVersion = f.Version
		}
		if f.Version > maxVersion {
			maxVersion = f.Version
		}
	}

	var problems []string
	for v := minVersion; v <= maxVersion; v++ {
		if !present[v] {
			problems = append(problems, fmt.Sprintf(
				"migrations/ is missing version %06d (present versions span %06d-%06d) -- golang-migrate's own postgres driver tracks a single applied version, not a per-migration set, so a lower-numbered migration that lands on main AFTER a higher one has already run on a real environment is skipped forever, never applied; this check only runs against main, where every merged migration must already be present",
				v, minVersion, maxVersion))
		}
	}
	return problems
}

// MigrationsDir is the repo-relative directory LoadMigrationFiles reads.
func MigrationsDir(root string) string { return filepath.Join(root, "migrations") }

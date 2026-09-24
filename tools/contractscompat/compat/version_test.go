package compat

import "testing"

// TestVersionChangelogDiscipline is the exit criterion's RED #14 ("change
// a schema and leave VERSION/CHANGELOG untouched, or PATCH-bump for a
// property addition") plus the mirror GREEN cases: §6.3 design spec §4's
// CI rule, exercised directly against CheckVersionAndChangelog rather
// than through the whole Compare pipeline.
func TestVersionChangelogDiscipline(t *testing.T) {
	tests := []struct {
		name string
		in   VersionCheckInput
		want bool // true if at least one Major "version-changelog" finding is expected
	}{
		{
			name: "nothing changed, nothing bumped: clean",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.0.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog:    []ChangelogSection{{Version: "1.0.0", Subsurface: map[string]bool{}}},
				AnythingChanged:  false,
			},
			want: false,
		},
		{
			name: "RED 14a: nothing changed but VERSION bumped anyway",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.1.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog:    []ChangelogSection{{Version: "1.0.0", Subsurface: map[string]bool{}}},
				AnythingChanged:  false,
			},
			want: true,
		},
		{
			name: "RED 14b: schema changed but VERSION left untouched",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.0.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog:    []ChangelogSection{{Version: "1.0.0", Subsurface: map[string]bool{}}},
				AnythingChanged:  true,
				WorstFinding:     SeverityMinor,
				ChangedSurfaces:  []string{"rest/v1/dtos.schema.json"},
			},
			want: true,
		},
		{
			name: "RED 14c: MINOR-class change but only a PATCH bump",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.0.1",
				BaseChangelogTop: "1.0.0",
				HeadChangelog: []ChangelogSection{{Version: "1.0.1", Subsurface: map[string]bool{
					"rest/v1/dtos.schema.json": true,
				}}},
				AnythingChanged: true,
				WorstFinding:    SeverityMinor,
				ChangedSurfaces: []string{"rest/v1/dtos.schema.json"},
			},
			want: true,
		},
		{
			name: "GREEN: MINOR change, MINOR bump, changelog heading and subsection present",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.1.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog: []ChangelogSection{{Version: "1.1.0", Subsurface: map[string]bool{
					"rest/v1/dtos.schema.json": true,
				}}},
				AnythingChanged: true,
				WorstFinding:    SeverityMinor,
				ChangedSurfaces: []string{"rest/v1/dtos.schema.json"},
			},
			want: false,
		},
		{
			name: "MAJOR change over-satisfied by a MAJOR bump: still fine",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "2.0.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog: []ChangelogSection{{Version: "2.0.0", Subsurface: map[string]bool{
					"rest/v1/dtos.schema.json": true,
				}}},
				AnythingChanged: true,
				WorstFinding:    SeverityMajor,
				ChangedSurfaces: []string{"rest/v1/dtos.schema.json"},
			},
			want: false,
		},
		{
			name: "changelog top heading doesn't match new VERSION",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.1.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog: []ChangelogSection{{Version: "1.0.5", Subsurface: map[string]bool{
					"rest/v1/dtos.schema.json": true,
				}}},
				AnythingChanged: true,
				WorstFinding:    SeverityMinor,
				ChangedSurfaces: []string{"rest/v1/dtos.schema.json"},
			},
			want: true,
		},
		{
			name: "changelog missing a subsection for a changed surface",
			in: VersionCheckInput{
				BaseVersion: "1.0.0", HeadVersion: "1.1.0",
				BaseChangelogTop: "1.0.0",
				HeadChangelog:    []ChangelogSection{{Version: "1.1.0", Subsurface: map[string]bool{}}},
				AnythingChanged:  true,
				WorstFinding:     SeverityMinor,
				ChangedSurfaces:  []string{"rest/v1/dtos.schema.json"},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := CheckVersionAndChangelog(tc.in)
			got := containsFinding(findings, "version-changelog", SeverityMajor)
			if got != tc.want {
				t.Fatalf("want breaking=%v, got findings=%+v", tc.want, findings)
			}
		})
	}
}

package autonomy_test

import (
	"regexp"
	"testing"

	"github.com/narvidev/narvi/internal/domain/autonomy"
)

// TestSites_NamesAreDistinctLabels pins the site vocabulary: every site,
// built or reserved, is a distinct lower_snake label (it is a metric label
// value), and no name is both built and reserved.
func TestSites_NamesAreDistinctLabels(t *testing.T) {
	label := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[autonomy.Site]string{}
	for _, tc := range []struct {
		kind  string
		sites []autonomy.Site
	}{
		{kind: "built", sites: autonomy.AllSites},
		{kind: "reserved", sites: autonomy.Reserved},
	} {
		for _, s := range tc.sites {
			if !label.MatchString(string(s)) {
				t.Errorf("%s site %q is not a lower_snake label", tc.kind, s)
			}
			if prev, ok := seen[s]; ok {
				t.Errorf("site %q is listed as %s and as %s", s, prev, tc.kind)
			}
			seen[s] = tc.kind
		}
	}
	if got, want := len(autonomy.AllSites), 7; got != want {
		t.Errorf("len(AllSites) = %d, want %d: a site added or removed is registered with internal/ops' ScanAutonomyFreezeSites too", got, want)
	}
}

// TestSkipReasons pins the two reasons a skip records, which are label
// values of autonomy_freeze_skip_total and words of a skipped outbox row's
// last_error.
func TestSkipReasons(t *testing.T) {
	for _, tc := range []struct {
		reason autonomy.SkipReason
		want   string
	}{
		{autonomy.SkipFrozen, "frozen"},
		{autonomy.SkipFreezeUnreadable, "freeze_unreadable"},
	} {
		if string(tc.reason) != tc.want {
			t.Errorf("reason = %q, want %q", tc.reason, tc.want)
		}
	}
	if autonomy.OutcomeSkipped != "skipped" {
		t.Errorf("OutcomeSkipped = %q, want skipped", autonomy.OutcomeSkipped)
	}
}

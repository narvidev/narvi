package httpapi

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/shadowsentinel"
	"github.com/narvidev/narvi/internal/platform"
)

// TestCapSummary_Table pins the 4,000-character cap: counted in code
// points, never cut inside one, truncated exactly when something was cut.
func TestCapSummary_Table(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantChars     int
		wantTruncated bool
	}{
		{"empty", "", 0, false},
		{"short", "done", 4, false},
		{"exactly the cap", strings.Repeat("a", resultSummaryMaxChars), resultSummaryMaxChars, false},
		{"one past the cap", strings.Repeat("a", resultSummaryMaxChars+1), resultSummaryMaxChars, true},
		{"multibyte at the cap", strings.Repeat("é", resultSummaryMaxChars) + "字", resultSummaryMaxChars, true},
		{"multibyte under the cap", strings.Repeat("字", resultSummaryMaxChars-1), resultSummaryMaxChars - 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := capSummary(tc.text)
			if n := utf8.RuneCountInString(got); n != tc.wantChars || truncated != tc.wantTruncated {
				t.Errorf("capSummary = %d characters, truncated %v; want %d, %v", n, truncated, tc.wantChars, tc.wantTruncated)
			}
			if !utf8.ValidString(got) || !strings.HasPrefix(tc.text, got) {
				t.Error("capSummary cut inside a character or changed the text")
			}
		})
	}
}

func TestParsePRURL_Table(t *testing.T) {
	tests := []struct {
		url         string
		owner, repo string
		number      int32
		ok          bool
	}{
		{"https://github.com/acme/widgets/pull/12", "acme", "widgets", 12, true},
		{"https://github.com/acme/widgets/pull/12/", "acme", "widgets", 12, true},
		{"https://github.com/acme/widgets/issues/12", "", "", 0, false},
		{"https://github.com/acme/widgets/pull/0", "", "", 0, false},
		{"https://github.com/acme/widgets/pull/x", "", "", 0, false},
		{"https://github.com/acme/pull/12", "", "", 0, false},
		{"::not a url", "", "", 0, false},
	}
	for _, tc := range tests {
		owner, repo, number, ok := parsePRURL(tc.url)
		if owner != tc.owner || repo != tc.repo || number != tc.number || ok != tc.ok {
			t.Errorf("parsePRURL(%q) = %q %q %d %v, want %q %q %d %v", tc.url, owner, repo, number, ok, tc.owner, tc.repo, tc.number, tc.ok)
		}
	}
}

// TestReadPRArtifact_Table pins how a pull request artifact is read: the
// session's own repository of the recorded bare name names the owner, the
// URL only when the session's repositories cannot say; the ref a suppressed
// creation hands back -- in either layer's URL shape -- is recognised by the
// one predicate and reported shadow_suppressed, never as a pull request;
// and a row naming no pull request this build can resolve is reported
// unreadable, with why, never as an error that fails the result.
func TestReadPRArtifact_Table(t *testing.T) {
	repos := func(pairs ...string) []byte {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < len(pairs); i += 2 {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"branch":null,"name":"` + pairs[i] + `","url":"` + pairs[i+1] + `"}`)
		}
		b.WriteString("]")
		return []byte(b.String())
	}
	created := time.Unix(1, 0)
	art := func(meta, url string) sqlcgen.Artifact {
		return sqlcgen.Artifact{Type: sqlcgen.ArtifactTypePr, Url: url, Metadata: []byte(meta), CreatedAt: pgtype.Timestamptz{Time: created, Valid: true}}
	}
	synthetic := fmt.Sprintf(`{"repo":"widgets","number":%d}`, shadowsentinel.PRNumber)
	acme := repos("w", "https://github.com/acme/widgets.git")
	type want struct {
		repoFullName string // "" for none
		number       int32
		kind         restdtos.SessionOutcomeExcludedPullRequestKind // "" for a pull request
		url          string                                         // an excluded entry's url; "" for null
		reason       string                                         // an excluded entry's reason, when pinned
	}
	shadow := restdtos.SessionOutcomeExcludedPullRequestKindShadowSuppressed
	unreadable := restdtos.SessionOutcomeExcludedPullRequestKindUnreadable
	tests := []struct {
		name  string
		a     sqlcgen.Artifact
		repos []byte
		want  want
	}{
		{"the session's repository", art(`{"repo":"widgets","number":3}`, "https://github.com/fork/widgets/pull/3"), acme, want{repoFullName: "acme/widgets", number: 3}},
		{"ambiguous repositories: the URL", art(`{"repo":"widgets","number":3}`, "https://github.com/two/widgets/pull/3"), repos("a", "https://github.com/one/widgets", "b", "https://github.com/two/widgets"), want{repoFullName: "two/widgets", number: 3}},
		{"no metadata: the URL alone", art(``, "https://github.com/acme/widgets/pull/9"), repos(), want{repoFullName: "acme/widgets", number: 9}},
		{"the decorator's suppressed creation", art(synthetic, shadowsentinel.URLScheme+"acme/widgets/pull/not-created"), acme,
			want{kind: shadow, repoFullName: "acme/widgets", reason: reasonShadowSuppressed}},
		{"the transport gate's suppressed creation", art(synthetic, shadowsentinel.URLScheme+"acme/widgets/not-created"), acme,
			want{kind: shadow, repoFullName: "acme/widgets", reason: reasonShadowSuppressed}},
		{"a suppressed creation whose owner the session's repositories cannot name", art(synthetic, shadowsentinel.URLScheme+"acme/widgets/pull/not-created"), repos("a", "https://github.com/one/widgets", "b", "https://github.com/two/widgets"),
			want{kind: shadow}},
		{"ambiguous and the URL names another repository", art(`{"repo":"widgets","number":3}`, "https://github.com/two/gadgets/pull/3"), repos("a", "https://github.com/one/widgets", "b", "https://github.com/two/widgets"),
			want{kind: unreadable, url: "https://github.com/two/gadgets/pull/3"}},
		{"nothing names the repository", art(`{}`, "not a url"), repos(), want{kind: unreadable, url: "not a url", reason: reasonArtifactNoPullRequest}},
		{"malformed metadata", art(`{"repo":`, "https://github.com/acme/widgets/pull/9"), repos(), want{kind: unreadable, url: "https://github.com/acme/widgets/pull/9", reason: reasonArtifactMetadata}},
		{"no URL and no number", art(`{"repo":"widgets"}`, ""), acme, want{kind: unreadable, reason: reasonArtifactNoPullRequest}},
		// Number 0 is not the sentinel: only the one predicate says
		// suppressed, so a zero-valued ref is unreadable, never a pull request.
		{"a zero number with a non-https URL", art(`{"repo":"widgets","number":0}`, "narvi-shadow://acme/widgets"), acme, want{kind: unreadable, url: "narvi-shadow://acme/widgets", reason: reasonArtifactNoPullRequest}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, excluded := readPRArtifact(tc.a, tc.repos)
			if tc.want.kind == "" {
				if excluded != nil {
					t.Fatalf("excluded %+v, want the pull request %s#%d", *excluded, tc.want.repoFullName, tc.want.number)
				}
				if got.repoFullName != tc.want.repoFullName || got.number != tc.want.number || got.url != tc.a.Url || !got.createdAt.Equal(created) {
					t.Errorf("got %+v, want %s#%d", got, tc.want.repoFullName, tc.want.number)
				}
				return
			}
			if excluded == nil {
				t.Fatalf("read as the pull request %+v, want excluded as %s", got, tc.want.kind)
			}
			if excluded.Kind != tc.want.kind || excluded.Reason == "" || !excluded.CreatedAt.Equal(created) {
				t.Errorf("excluded = %+v, want kind %s with a reason and the record's time", *excluded, tc.want.kind)
			}
			if tc.want.reason != "" && excluded.Reason != tc.want.reason {
				t.Errorf("reason = %q, want %q", excluded.Reason, tc.want.reason)
			}
			if gotRepo := derefOr(excluded.RepoFullName); gotRepo != tc.want.repoFullName {
				t.Errorf("repoFullName = %q, want %q", gotRepo, tc.want.repoFullName)
			}
			if gotURL := derefOr(excluded.Url); gotURL != tc.want.url {
				t.Errorf("url = %q, want %q (never the synthetic one)", gotURL, tc.want.url)
			}
		})
	}
}

func derefOr[T ~*string](p T) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestMergedPerClaim_Table: only a merge is decided from the claim. The
// closed stamp is written once and never cleared on reopen, so a pull
// request closed without merging is read live (finding P2).
func TestMergedPerClaim_Table(t *testing.T) {
	yes, no := true, false
	closedAt := pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true}
	tests := []struct {
		name   string
		claim  *sqlcgen.GithubPrSession
		merged bool
	}{
		{"no claim", nil, false},
		{"open", &sqlcgen.GithubPrSession{}, false},
		{"merged", &sqlcgen.GithubPrSession{PrMerged: &yes, PrClosedAt: closedAt}, true},
		{"closed unmerged, perhaps reopened since: read live", &sqlcgen.GithubPrSession{PrMerged: &no, PrClosedAt: closedAt}, false},
		{"a closed stamp alone: read live", &sqlcgen.GithubPrSession{PrClosedAt: closedAt}, false},
	}
	for _, tc := range tests {
		if got := mergedPerClaim(tc.claim); got != tc.merged {
			t.Errorf("%s: mergedPerClaim = %v, want %v", tc.name, got, tc.merged)
		}
	}
}

// TestResultReadDelay_Table pins the suggested delay: the unsettled value
// whenever the session can still change, whatever was read live; once
// settled, the live-read value when a freshness was read live, else the
// settled value; always within [floor, ceiling].
func TestResultReadDelay_Table(t *testing.T) {
	to := platform.DefaultTimeouts()
	tests := []struct {
		activity session.Activity
		readLive bool
		want     time.Duration
	}{
		{session.ActivityRunning, true, to.SessionResultDelayUnsettled},
		{session.ActivityQueued, false, to.SessionResultDelayUnsettled},
		{session.ActivityDelivering, false, to.SessionResultDelayUnsettled},
		{session.ActivityScheduled, true, to.SessionResultDelayUnsettled},
		{session.Activity("a value this build does not know"), false, to.SessionResultDelayUnsettled},
		{session.ActivityFinished, true, to.SessionResultDelayLiveRead},
		{session.ActivityAwaitingApproval, true, to.SessionResultDelayLiveRead},
		{session.ActivityFinished, false, to.SessionResultDelaySettled},
		{session.ActivityIdle, false, to.SessionResultDelaySettled},
	}
	for _, tc := range tests {
		if got := resultReadDelay(tc.activity, tc.readLive, to); got != tc.want {
			t.Errorf("resultReadDelay(%q, live %v) = %v, want %v", tc.activity, tc.readLive, got, tc.want)
		}
	}
	clamped := to
	clamped.SessionResultDelayUnsettled = time.Second
	clamped.SessionResultDelaySettled = time.Hour
	if got := resultReadDelay(session.ActivityRunning, false, clamped); got != to.SessionResultDelayFloor {
		t.Errorf("below the floor: %v, want the floor %v", got, to.SessionResultDelayFloor)
	}
	if got := resultReadDelay(session.ActivityFinished, false, clamped); got != to.SessionResultDelayCeiling {
		t.Errorf("above the ceiling: %v, want the ceiling %v", got, to.SessionResultDelayCeiling)
	}
}

// TestReviewScope_NeverAnEmptyListAlone: no pull request in scope is said
// explicitly as none.
func TestReviewScope_NeverAnEmptyListAlone(t *testing.T) {
	if got := reviewScope(&restdtos.SessionOutcome{PullRequests: []restdtos.SessionOutcomePullRequest{}}); got != restdtos.SessionOutcomeReviewScopeNone {
		t.Errorf("no pull request: %q, want none", got)
	}
	if got := reviewScope(&restdtos.SessionOutcome{PullRequests: []restdtos.SessionOutcomePullRequest{{}}}); got != restdtos.SessionOutcomeReviewScopeProduced {
		t.Errorf("one produced: %q, want produced", got)
	}
	if got := reviewScope(&restdtos.SessionOutcome{PullRequests: []restdtos.SessionOutcomePullRequest{{}}, ReviewedPullRequest: &restdtos.SessionOutcomeReviewedPullRequest{}}); got != restdtos.SessionOutcomeReviewScopeReviewed {
		t.Errorf("reviewed and produced: %q, want reviewed", got)
	}
}

// TestVerdictDTO_UnrecordedIsNull: a context field, or an attempt, never
// recorded is null on the wire, never an empty string.
func TestVerdictDTO_UnrecordedIsNull(t *testing.T) {
	legacy := verdictDTO(reviewverdict.Record{ID: "v", HeadSHA: "h", Verdict: review.Verdict{RiskLevel: review.RiskLevelLow, Shippable: review.ShippableAuto}})
	if legacy.AttemptId != nil || legacy.Context.BaseRef != nil || legacy.Context.BaseSha != nil || legacy.Context.AncestorChainLength != 0 {
		t.Errorf("legacy verdict = %+v, want null attempt and context", legacy)
	}
	full := verdictDTO(reviewverdict.Record{ID: "v", AttemptID: "a", HeadSHA: "h", Context: reviewverdict.Context{BaseRef: "main", BaseSHA: "b", PolicyVersion: 1, AncestorChain: []review.AncestorLink{{Ref: "p", SHA: "s"}}}})
	if full.AttemptId == nil || *full.AttemptId != "a" || *full.Context.BaseRef != "main" || *full.Context.BaseSha != "b" || full.Context.PolicyVersion != 1 || full.Context.AncestorChainLength != 1 {
		t.Errorf("verdict = %+v, want its record copied", full)
	}
}

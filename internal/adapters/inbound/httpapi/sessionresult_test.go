package httpapi

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
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

// TestProducedPRFromArtifact_Table pins how a pull request artifact is
// resolved: the session's own repository of the recorded bare name names
// the owner; the URL only when the session's repositories cannot say; a row
// naming no repository and number is this build's defect, reported.
func TestProducedPRFromArtifact_Table(t *testing.T) {
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
	art := func(meta, url string) sqlcgen.Artifact {
		return sqlcgen.Artifact{Type: sqlcgen.ArtifactTypePr, Url: url, Metadata: []byte(meta), CreatedAt: pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true}}
	}
	tests := []struct {
		name    string
		a       sqlcgen.Artifact
		repos   []byte
		want    string
		wantN   int32
		wantErr bool
	}{
		{"the session's repository", art(`{"repo":"widgets","number":3}`, "https://github.com/fork/widgets/pull/3"), repos("w", "https://github.com/acme/widgets.git"), "acme/widgets", 3, false},
		{"ambiguous repositories: the URL", art(`{"repo":"widgets","number":3}`, "https://github.com/two/widgets/pull/3"), repos("a", "https://github.com/one/widgets", "b", "https://github.com/two/widgets"), "two/widgets", 3, false},
		{"no metadata: the URL alone", art(``, "https://github.com/acme/widgets/pull/9"), repos(), "acme/widgets", 9, false},
		{"ambiguous and the URL names another repository", art(`{"repo":"widgets","number":3}`, "https://github.com/two/gadgets/pull/3"), repos("a", "https://github.com/one/widgets", "b", "https://github.com/two/widgets"), "", 0, true},
		{"nothing names the repository", art(`{}`, "not a url"), repos(), "", 0, true},
		{"malformed metadata", art(`{"repo":`, "https://github.com/acme/widgets/pull/9"), repos(), "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := producedPRFromArtifact(tc.a, tc.repos)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && (got.repoFullName != tc.want || got.number != tc.wantN || got.url != tc.a.Url) {
				t.Errorf("got %+v, want %s#%d", got, tc.want, tc.wantN)
			}
		})
	}
}

func TestClosedPerClaim_Table(t *testing.T) {
	yes, no := true, false
	closedAt := pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true}
	tests := []struct {
		name   string
		claim  *sqlcgen.GithubPrSession
		closed bool
	}{
		{"no claim", nil, false},
		{"open", &sqlcgen.GithubPrSession{}, false},
		{"merged", &sqlcgen.GithubPrSession{PrMerged: &yes, PrClosedAt: closedAt}, true},
		{"closed unmerged", &sqlcgen.GithubPrSession{PrMerged: &no, PrClosedAt: closedAt}, true},
	}
	for _, tc := range tests {
		if reason, closed := closedPerClaim(tc.claim); closed != tc.closed || (closed && reason == "") {
			t.Errorf("%s: closedPerClaim = %q %v, want closed %v with a reason", tc.name, reason, closed, tc.closed)
		}
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

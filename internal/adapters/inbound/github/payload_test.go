package github

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

func TestCompileMentionPattern(t *testing.T) {
	tests := []struct {
		name      string
		botHandle string
		body      string
		want      bool
	}{
		{name: "exact mention matches", botHandle: "narvi-bot", body: "@narvi-bot please review", want: true},
		{name: "case-insensitive matches", botHandle: "narvi-bot", body: "hey @Narvi-Bot take a look", want: true},
		{name: "mid-sentence matches", botHandle: "narvi-bot", body: "cc @narvi-bot, thanks!", want: true},
		{name: "no mention at all", botHandle: "narvi-bot", body: "please review this PR", want: false},
		{name: "different handle does not match", botHandle: "narvi-bot", body: "@some-other-bot review this", want: false},
		{name: "longer handle superstring rejected by word boundary", botHandle: "narvi", body: "@narvi-bot-2 review this", want: false},
		{name: "email-shaped string rejected", botHandle: "narvi-bot", body: "contact user@narvi-bot.example for help", want: false},
		{name: "handle at start of body matches", botHandle: "narvi-bot", body: "@narvi-bot hello", want: true},
		{name: "team mention sharing handle as prefix rejected", botHandle: "narvi", body: "please cc @narvi/maintainers for review", want: false},
		{name: "team mention with dash slug sharing handle as prefix rejected", botHandle: "narvi", body: "@narvi/team-x take a look", want: false},
		{name: "dotted suffix sharing handle as prefix rejected", botHandle: "narvi", body: "cc @narvi.bot for triage", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			re := compileMentionPattern(tc.botHandle)
			got := re.MatchString(tc.body)
			if got != tc.want {
				t.Errorf("compileMentionPattern(%q).MatchString(%q) = %v, want %v", tc.botHandle, tc.body, got, tc.want)
			}
		})
	}
}

const testBotHandle = "narvi-bot"

func TestParseIssueComment(t *testing.T) {
	mentionRE := compileMentionPattern(testBotHandle)

	tests := []struct {
		name    string
		body    string
		wantOK  bool
		wantErr bool
		check   func(t *testing.T, m mention)
	}{
		{
			name: "PR comment mentioning bot, action created -- actionable",
			body: `{
				"action": "created",
				"issue": {"number": 42, "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/42"}},
				"comment": {"body": "@narvi-bot please review"},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: true,
			check: func(t *testing.T, m mention) {
				if m.RepoFullName != "acme/widgets" {
					t.Errorf("RepoFullName = %q, want %q", m.RepoFullName, "acme/widgets")
				}
				if m.RepoName != "widgets" {
					t.Errorf("RepoName = %q, want %q", m.RepoName, "widgets")
				}
				if m.RepoCloneURL != "https://github.com/acme/widgets.git" {
					t.Errorf("RepoCloneURL = %q, want %q", m.RepoCloneURL, "https://github.com/acme/widgets.git")
				}
				if m.PRNumber != 42 {
					t.Errorf("PRNumber = %d, want 42", m.PRNumber)
				}
				if m.HeadBranch != nil {
					t.Errorf("HeadBranch = %v, want nil (issue_comment carries no head ref)", *m.HeadBranch)
				}
				if m.CommentBody != "@narvi-bot please review" {
					t.Errorf("CommentBody = %q, want %q", m.CommentBody, "@narvi-bot please review")
				}
			},
		},
		{
			name: "plain issue comment (not a PR) -- ignored",
			body: `{
				"action": "created",
				"issue": {"number": 7},
				"comment": {"body": "@narvi-bot please review"},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: false,
		},
		{
			name: "PR comment not mentioning bot -- ignored",
			body: `{
				"action": "created",
				"issue": {"number": 42, "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/42"}},
				"comment": {"body": "looks good to me"},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: false,
		},
		{
			name: "edited action -- ignored",
			body: `{
				"action": "edited",
				"issue": {"number": 42, "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/42"}},
				"comment": {"body": "@narvi-bot please review"},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: false,
		},
		{
			name:    "malformed JSON -- error",
			body:    `{not json`,
			wantOK:  false,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ok, err := parseIssueComment([]byte(tc.body), mentionRE)
			if tc.wantErr {
				if err == nil {
					t.Fatal("parseIssueComment() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseIssueComment() error = %v, want nil", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("parseIssueComment() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestParsePullRequestReviewComment(t *testing.T) {
	mentionRE := compileMentionPattern(testBotHandle)

	tests := []struct {
		name    string
		body    string
		wantOK  bool
		wantErr bool
		check   func(t *testing.T, m mention)
	}{
		{
			name: "review comment mentioning bot on a fork PR -- actionable, the base repo cloned, the fork recorded as the head's",
			body: `{
				"action": "created",
				"comment": {"body": "@narvi-bot what do you think of this line?"},
				"pull_request": {
					"number": 99,
					"head": {"ref": "feature-x", "sha": "sha-fork-head", "repo": {"name": "widgets", "full_name": "contributor/widgets", "clone_url": "https://github.com/contributor/widgets.git"}}
				},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: true,
			check: func(t *testing.T, m mention) {
				if m.RepoFullName != "acme/widgets" {
					t.Errorf("RepoFullName = %q, want %q (base repo, the claim key)", m.RepoFullName, "acme/widgets")
				}
				if m.RepoName != "widgets" || m.RepoCloneURL != "https://github.com/acme/widgets.git" {
					t.Errorf("RepoName/RepoCloneURL = %q/%q, want the BASE repo's: the head is read from its pull ref, never the fork", m.RepoName, m.RepoCloneURL)
				}
				if m.HeadRepoFullName != "contributor/widgets" {
					t.Errorf("HeadRepoFullName = %q, want the fork %q", m.HeadRepoFullName, "contributor/widgets")
				}
				if m.PRNumber != 99 {
					t.Errorf("PRNumber = %d, want 99", m.PRNumber)
				}
				if m.HeadBranch == nil || *m.HeadBranch != "feature-x" {
					t.Errorf("HeadBranch = %v, want %q", m.HeadBranch, "feature-x")
				}
				if m.HeadSHA == nil || *m.HeadSHA != "sha-fork-head" {
					t.Errorf("HeadSHA = %v, want %q", m.HeadSHA, "sha-fork-head")
				}
			},
		},
		{
			name: "review comment mentioning bot on a same-repository PR -- the head repository is the base",
			body: `{
				"action": "created",
				"comment": {"body": "@narvi-bot what do you think of this line?"},
				"pull_request": {
					"number": 98,
					"head": {"ref": "feature-y", "repo": {"name": "widgets", "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"}}
				},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: true,
			check: func(t *testing.T, m mention) {
				if m.RepoCloneURL != "https://github.com/acme/widgets.git" || m.HeadRepoFullName != "acme/widgets" {
					t.Errorf("RepoCloneURL/HeadRepoFullName = %q/%q, want the base repo for both", m.RepoCloneURL, m.HeadRepoFullName)
				}
			},
		},
		{
			name: "no mention -- ignored",
			body: `{
				"action": "created",
				"comment": {"body": "nice catch"},
				"pull_request": {"number": 99, "head": {"ref": "feature-x", "repo": {"name": "widgets", "clone_url": "https://github.com/contributor/widgets.git"}}},
				"repository": {"full_name": "acme/widgets"}
			}`,
			wantOK: false,
		},
		{
			name: "deleted action -- ignored",
			body: `{
				"action": "deleted",
				"comment": {"body": "@narvi-bot look again"},
				"pull_request": {"number": 99, "head": {"ref": "feature-x", "repo": {"name": "widgets", "clone_url": "https://github.com/contributor/widgets.git"}}},
				"repository": {"full_name": "acme/widgets"}
			}`,
			wantOK: false,
		},
		{
			name:    "malformed JSON -- error",
			body:    `{not json`,
			wantOK:  false,
			wantErr: true,
		},
		{
			// L15 audit fix: GitHub's own webhook documentation states
			// pull_request.head.repo is nullable -- null when the head
			// repository has been deleted (e.g. a fork removed after the
			// PR was opened). Before this fix, a non-pointer Repo struct
			// field silently unmarshalled this null into an empty-valued
			// struct (empty Name/CloneURL), which would have made the
			// session try to clone an empty repo spec entirely. This
			// proves the fix: falls back to the BASE repo (repository.name/
			// clone_url), exactly like parseIssueComment's own existing
			// fallback for the analogous situation, and is still a genuine,
			// actionable mention (ok=true, no error). The head repository is
			// unknown, so the spec will carry no branch.
			name: "head.repo null (deleted fork) -- the base repo, head repository unknown, still actionable",
			body: `{
				"action": "created",
				"comment": {"body": "@narvi-bot what do you think of this line?"},
				"pull_request": {
					"number": 99,
					"head": {"ref": "feature-x", "repo": null}
				},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantOK: true,
			check: func(t *testing.T, m mention) {
				if m.RepoFullName != "acme/widgets" {
					t.Errorf("RepoFullName = %q, want %q", m.RepoFullName, "acme/widgets")
				}
				if m.RepoName != "widgets" {
					t.Errorf("RepoName = %q, want %q (base repo fallback)", m.RepoName, "widgets")
				}
				if m.RepoCloneURL != "https://github.com/acme/widgets.git" {
					t.Errorf("RepoCloneURL = %q, want %q (base repo fallback, NOT empty)", m.RepoCloneURL, "https://github.com/acme/widgets.git")
				}
				if m.HeadBranch == nil || *m.HeadBranch != "feature-x" {
					t.Errorf("HeadBranch = %v, want %q (head.ref itself is never null, only head.repo)", m.HeadBranch, "feature-x")
				}
				if m.HeadRepoFullName != "" {
					t.Errorf("HeadRepoFullName = %q, want empty: a deleted head repository is unknown", m.HeadRepoFullName)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ok, err := parsePullRequestReviewComment([]byte(tc.body), mentionRE)
			if tc.wantErr {
				if err == nil {
					t.Fatal("parsePullRequestReviewComment() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePullRequestReviewComment() error = %v, want nil", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("parsePullRequestReviewComment() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

// TestParseMention_UnrecognizedEventType proves an event type genuinely
// outside this package's own three-lane dispatch (issue_comment,
// pull_request_review_comment, pull_request -- payload.go's own parseMention
// doc comment) is acknowledged and ignored, never an error. "star" is used
// here rather than "pull_request" (this test's own pre-existing event type
// choice): pull_request is NOW a recognized-and-dispatched event type
// ("review sessions", §8.2's own label-retrigger lane) -- see
// TestParsePullRequestLabeled below for that lane's own dedicated coverage,
// including its own "labeled action, but no configured label matches" case,
// which is a DIFFERENT reason for ok=false than "this event type is not
// dispatched on at all".
func TestParseMention_UnrecognizedEventType(t *testing.T) {
	mentionRE := compileMentionPattern(testBotHandle)
	_, ok, err := parseMention("star", []byte(`{}`), mentionRE, "run-review")
	if err != nil {
		t.Fatalf("parseMention() error = %v, want nil", err)
	}
	if ok {
		t.Error("parseMention() ok = true, want false for an event type this adapter doesn't act on")
	}
}

// TestParseMention_CommenterIdentity is batch fix/audit-github-actor-rbac's
// own table-driven test for the new comment.user.{id,login} parsing (the
// H4 audit finding this batch closes: GitHub ingress never even parsed WHO
// commented) -- table-driven over BOTH event shapes this package's own
// parseMention dispatches on, since GitHub uses the identical comment.user
// shape for each (payload.go's own doc comment on pullRequestReviewCommentPayload.
// Comment.User).
func TestParseMention_CommenterIdentity(t *testing.T) {
	mentionRE := compileMentionPattern(testBotHandle)

	tests := []struct {
		name               string
		eventType          string
		body               string
		wantCommenterID    int64
		wantCommenterLogin string
	}{
		{
			name:      "issue_comment carries commenter id/login",
			eventType: eventTypeIssueComment,
			body: `{
				"action": "created",
				"issue": {"number": 42, "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/42"}},
				"comment": {"body": "@narvi-bot please review", "user": {"id": 555111, "login": "octo-reviewer"}},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantCommenterID:    555111,
			wantCommenterLogin: "octo-reviewer",
		},
		{
			name:      "issue_comment with no comment.user -- zero-valued, never an error",
			eventType: eventTypeIssueComment,
			body: `{
				"action": "created",
				"issue": {"number": 42, "pull_request": {"url": "https://api.github.com/repos/acme/widgets/pulls/42"}},
				"comment": {"body": "@narvi-bot please review"},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			wantCommenterID:    0,
			wantCommenterLogin: "",
		},
		{
			name:      "pull_request_review_comment carries commenter id/login",
			eventType: eventTypePullRequestReviewComment,
			body: `{
				"action": "created",
				"comment": {"body": "@narvi-bot what do you think?", "user": {"id": 777222, "login": "line-commenter"}},
				"pull_request": {
					"number": 99,
					"head": {"ref": "feature-x", "repo": {"name": "widgets", "clone_url": "https://github.com/contributor/widgets.git"}}
				},
				"repository": {"full_name": "acme/widgets"}
			}`,
			wantCommenterID:    777222,
			wantCommenterLogin: "line-commenter",
		},
		{
			name:      "pull_request_review_comment with no comment.user -- zero-valued, never an error",
			eventType: eventTypePullRequestReviewComment,
			body: `{
				"action": "created",
				"comment": {"body": "@narvi-bot what do you think?"},
				"pull_request": {
					"number": 99,
					"head": {"ref": "feature-x", "repo": {"name": "widgets", "clone_url": "https://github.com/contributor/widgets.git"}}
				},
				"repository": {"full_name": "acme/widgets"}
			}`,
			wantCommenterID:    0,
			wantCommenterLogin: "",
		},
		{
			// §8.2 ("review sessions", §8.2): a label-triggered retrigger
			// has no comment/commenter at all -- CommenterID/CommenterLogin
			// instead come from GitHub's own "sender" field (the label
			// applier), mirroring the comment.user shape (mention.
			// CommenterID's own doc comment, payload.go).
			name:      "pull_request labeled event carries sender id/login",
			eventType: eventTypePullRequest,
			body: `{
				"action": "labeled",
				"label": {"name": "run-review"},
				"sender": {"id": 999333, "login": "maintainer-x"},
				"pull_request": {
					"number": 99,
					"head": {"ref": "feature-x", "repo": {"name": "widgets", "clone_url": "https://github.com/contributor/widgets.git"}}
				},
				"repository": {"full_name": "acme/widgets"}
			}`,
			wantCommenterID:    999333,
			wantCommenterLogin: "maintainer-x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ok, err := parseMention(tc.eventType, []byte(tc.body), mentionRE, "run-review")
			if err != nil {
				t.Fatalf("parseMention() error = %v, want nil", err)
			}
			if !ok {
				t.Fatal("parseMention() ok = false, want true (a genuine, actionable mention)")
			}
			if m.CommenterID != tc.wantCommenterID {
				t.Errorf("CommenterID = %d, want %d", m.CommenterID, tc.wantCommenterID)
			}
			if m.CommenterLogin != tc.wantCommenterLogin {
				t.Errorf("CommenterLogin = %q, want %q", m.CommenterLogin, tc.wantCommenterLogin)
			}
		})
	}
}

// TestParsePullRequestLabeled is §8.2's ("review sessions", §8.2) own
// table-driven test for the manual re-trigger-via-label lane: a
// pull_request/"labeled" event whose label.name matches this deployment's
// own configured reReviewLabel is actionable; every other action/label
// combination is acknowledged and ignored.
func TestParsePullRequestLabeled(t *testing.T) {
	const reReviewLabel = "run-review"

	tests := []struct {
		name          string
		body          string
		reReviewLabel string
		wantOK        bool
		wantErr       bool
		check         func(t *testing.T, m mention)
	}{
		{
			name: "matching label on labeled action -- actionable",
			body: `{
				"action": "labeled",
				"label": {"name": "run-review"},
				"sender": {"id": 42, "login": "maintainer-x"},
				"pull_request": {
					"number": 7,
					"head": {"ref": "feature-x", "repo": {"name": "widgets", "full_name": "contributor/widgets", "clone_url": "https://github.com/contributor/widgets.git"}}
				},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			reReviewLabel: reReviewLabel,
			wantOK:        true,
			check: func(t *testing.T, m mention) {
				if m.RepoFullName != "acme/widgets" {
					t.Errorf("RepoFullName = %q, want %q", m.RepoFullName, "acme/widgets")
				}
				if m.PRNumber != 7 {
					t.Errorf("PRNumber = %d, want 7", m.PRNumber)
				}
				if m.HeadBranch == nil || *m.HeadBranch != "feature-x" {
					t.Errorf("HeadBranch = %v, want %q", m.HeadBranch, "feature-x")
				}
				if m.RepoName != "widgets" || m.RepoCloneURL != "https://github.com/acme/widgets.git" {
					t.Errorf("RepoName/RepoCloneURL = %q/%q, want the BASE repo's, never the fork's", m.RepoName, m.RepoCloneURL)
				}
				if m.HeadRepoFullName != "contributor/widgets" {
					t.Errorf("HeadRepoFullName = %q, want the fork %q", m.HeadRepoFullName, "contributor/widgets")
				}
				if m.CommentBody != labelRetriggerPromptText {
					t.Errorf("CommentBody = %q, want the fixed labelRetriggerPromptText constant", m.CommentBody)
				}
				if m.Stack != nil {
					t.Errorf("Stack = %+v, want nil (this fixture reports no stack object)", m.Stack)
				}
			},
		},
		{
			name: "matching label with a stack object present",
			body: `{
				"action": "labeled",
				"label": {"name": "run-review"},
				"sender": {"id": 42, "login": "maintainer-x"},
				"pull_request": {
					"number": 7,
					"head": {"ref": "feature-x", "repo": {"name": "widgets", "clone_url": "https://github.com/contributor/widgets.git"}},
					"stack": {"size": 3, "position": 2, "base": {"ref": "main", "sha": "deadbeef"}}
				},
				"repository": {"full_name": "acme/widgets"}
			}`,
			reReviewLabel: reReviewLabel,
			wantOK:        true,
			check: func(t *testing.T, m mention) {
				if m.Stack == nil {
					t.Fatal("Stack = nil, want non-nil when the payload embeds one")
				}
				// Position/Size deliberately distinguishable (2 vs 3, not an
				// equal-valued pair) -- a confirmed audit finding noted that a
				// Position/Size field swap in parsePullRequestLabeled's own
				// mapping would pass undetected against a fixture where both
				// fields carried the same value.
				want := review.StackContext{Position: 2, Size: 3, UltimateBaseRef: "main", UltimateBaseSHA: "deadbeef"}
				if *m.Stack != want {
					t.Errorf("Stack = %+v, want %+v", *m.Stack, want)
				}
			},
		},
		{
			name: "null head.repo falls back to the base repo",
			body: `{
				"action": "labeled",
				"label": {"name": "run-review"},
				"sender": {"id": 42, "login": "maintainer-x"},
				"pull_request": {
					"number": 7,
					"head": {"ref": "feature-x", "repo": null}
				},
				"repository": {"full_name": "acme/widgets", "name": "widgets", "clone_url": "https://github.com/acme/widgets.git"}
			}`,
			reReviewLabel: reReviewLabel,
			wantOK:        true,
			check: func(t *testing.T, m mention) {
				if m.RepoName != "widgets" || m.RepoCloneURL != "https://github.com/acme/widgets.git" {
					t.Errorf("RepoName/RepoCloneURL = %q/%q, want the base repo", m.RepoName, m.RepoCloneURL)
				}
				if m.HeadRepoFullName != "" {
					t.Errorf("HeadRepoFullName = %q, want empty: a deleted head repository is unknown", m.HeadRepoFullName)
				}
			},
		},
		{
			name: "non-matching label ignored",
			body: `{
				"action": "labeled",
				"label": {"name": "bug"},
				"pull_request": {"number": 7, "head": {"ref": "feature-x", "repo": null}},
				"repository": {"full_name": "acme/widgets"}
			}`,
			reReviewLabel: reReviewLabel,
			wantOK:        false,
		},
		{
			name: "non-labeled action ignored even with a matching label name present",
			body: `{
				"action": "unlabeled",
				"label": {"name": "run-review"},
				"pull_request": {"number": 7, "head": {"ref": "feature-x", "repo": null}},
				"repository": {"full_name": "acme/widgets"}
			}`,
			reReviewLabel: reReviewLabel,
			wantOK:        false,
		},
		{
			name: "other pull_request actions (opened, synchronize, closed) ignored",
			body: `{
				"action": "synchronize",
				"pull_request": {"number": 7, "head": {"ref": "feature-x", "repo": null}},
				"repository": {"full_name": "acme/widgets"}
			}`,
			reReviewLabel: reReviewLabel,
			wantOK:        false,
		},
		{
			name: "empty configured reReviewLabel never matches, even a labeled event with an empty label name",
			body: `{
				"action": "labeled",
				"label": {"name": ""},
				"pull_request": {"number": 7, "head": {"ref": "feature-x", "repo": null}},
				"repository": {"full_name": "acme/widgets"}
			}`,
			reReviewLabel: "",
			wantOK:        false,
		},
		{
			name:          "malformed JSON is an error",
			body:          `not valid json`,
			reReviewLabel: reReviewLabel,
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ok, err := parsePullRequestLabeled([]byte(tc.body), tc.reReviewLabel)
			if tc.wantErr {
				if err == nil {
					t.Fatal("parsePullRequestLabeled() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePullRequestLabeled() error = %v, want nil", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("parsePullRequestLabeled() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

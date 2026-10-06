package sessionactor

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/provenance"
)

// sessionRowWithRepos builds a minimal sqlcgen.Session carrying a real,
// randomly generated id and reposJSON as its raw repos bytes -- enough for
// assembleSessionConfig, which reads ID/Repos/ProvenanceTag from its
// sessionRow argument (ProvenanceTag left nil here -- see
// sessionRowWithReposAndProvenance below for a sentinel-auto-fix session).
func sessionRowWithRepos(t *testing.T, reposJSON string) sqlcgen.Session {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan uuid: %v", err)
	}
	return sqlcgen.Session{ID: id, Repos: []byte(reposJSON)}
}

// sessionRowWithReposAndProvenance is sessionRowWithRepos' own sibling,
// additionally setting ProvenanceTag -- used by
// TestAssembleSessionConfig_CapabilityRestricted below to prove
// CapabilityRestricted's own confirmed-finding test gap: the wiring from
// a session's own provenance_tag to SessionConfig.CapabilityRestricted
// (sessionconfig.go) had zero test coverage anywhere in this diff before
// this fix.
func sessionRowWithReposAndProvenance(t *testing.T, reposJSON, provenanceTag string) sqlcgen.Session {
	t.Helper()
	row := sessionRowWithRepos(t, reposJSON)
	row.ProvenanceTag = &provenanceTag
	return row
}

// TestPublicWsBaseURL is table-driven over http->ws and https->wss scheme
// derivation, plus an unrecognized-scheme rejection -- the one function
// design decision 6 relies on instead of a second, separately configured
// ws(s):// base URL config field.
func TestPublicWsBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "http to ws", in: "http://localhost:8080", want: "ws://localhost:8080"},
		{name: "https to wss", in: "https://narvi.example.com", want: "wss://narvi.example.com"},
		{name: "https with port", in: "https://narvi.example.com:8443", want: "wss://narvi.example.com:8443"},
		{name: "unrecognized scheme rejected", in: "ftp://example.com", wantErr: true},
		{name: "malformed url rejected", in: "://not-a-url", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := publicWsBaseURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("publicWsBaseURL(%q) error = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("publicWsBaseURL(%q) error = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("publicWsBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReposFromJSON proves the sessions.repos JSONB round-trip: empty/nil
// input yields no repos, and a real repos array unmarshals into the exact
// SessionConfig wire shape.
func TestReposFromJSON(t *testing.T) {
	t.Parallel()

	t.Run("nil input yields no repos", func(t *testing.T) {
		t.Parallel()
		got, err := reposFromJSON(nil)
		if err != nil {
			t.Fatalf("reposFromJSON(nil) error = %v, want nil", err)
		}
		if len(got) != 0 {
			t.Errorf("reposFromJSON(nil) = %+v, want empty", got)
		}
	})

	t.Run("empty array input yields no repos", func(t *testing.T) {
		t.Parallel()
		got, err := reposFromJSON([]byte(`[]`))
		if err != nil {
			t.Fatalf("reposFromJSON([]) error = %v, want nil", err)
		}
		if len(got) != 0 {
			t.Errorf("reposFromJSON([]) = %+v, want empty", got)
		}
	})

	t.Run("real repos array round-trips", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null},{"name":"gadgets","url":"https://github.com/acme/gadgets","branch":"feature/x"}]`)

		got, err := reposFromJSON(raw)
		if err != nil {
			t.Fatalf("reposFromJSON() error = %v, want nil", err)
		}
		if len(got) != 2 {
			t.Fatalf("reposFromJSON() len = %d, want 2", len(got))
		}
		if got[0].Name != "widgets" || got[0].Url != "https://github.com/acme/widgets" || got[0].Branch != nil {
			t.Errorf("repos[0] = %+v, unexpected", got[0])
		}
		if got[1].Name != "gadgets" || got[1].Branch == nil || *got[1].Branch != "feature/x" {
			t.Errorf("repos[1] = %+v, unexpected", got[1])
		}
	})

	t.Run("malformed JSON is a real error", func(t *testing.T) {
		t.Parallel()
		if _, err := reposFromJSON([]byte(`not json`)); err == nil {
			t.Fatal("reposFromJSON(malformed) error = nil, want error")
		}
	})
}

// TestAssembleSessionConfig proves assembleSessionConfig wires every field
// from its own inputs correctly (BootMode now threaded through as a
// caller-supplied argument -- §3.2, "snapshots & restore", design
// decision 6b -- rather than hardcoded), using a minimal Actor constructed
// directly (no Postgres needed -- assembleSessionConfig only reads
// a.publicBaseURL and its own arguments). Sub-tests cover both bootMode
// values a real caller passes today (Fresh from planFreshSpawn,
// SnapshotRestore from planRestore); Build/RepoImage stay unused
// placeholders (§8.5's own job) but assembleSessionConfig itself does
// not restrict which value it is handed.
func TestAssembleSessionConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bootMode sessionconfig.SessionConfigBootMode
	}{
		{name: "fresh spawn", bootMode: sessionconfig.SessionConfigBootModeFresh},
		{name: "snapshot restore", bootMode: sessionconfig.SessionConfigBootModeSnapshotRestore},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &Actor{publicBaseURL: "https://narvi.example.com"}
			sessionRow := sessionRowWithRepos(t, `[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`)
			sandboxID := uuid.NewString()

			// sessionRow.EnvironmentID is left at its zero value (invalid)
			// by sessionRowWithRepos -- environmentPathScope's own first
			// check short-circuits before ever touching tx, so a nil tx is
			// safe here (no Postgres needed, matching this test's own doc
			// comment above).
			cfg, err := a.assembleSessionConfig(context.Background(), nil, sessionRow, 3, "plaintext-token", sandboxID, tc.bootMode)
			if err != nil {
				t.Fatalf("assembleSessionConfig() error = %v, want nil", err)
			}
			if cfg.PathScope != nil {
				t.Errorf("PathScope = %v, want nil (no Environment attached)", cfg.PathScope)
			}

			if cfg.BootMode != tc.bootMode {
				t.Errorf("BootMode = %q, want %q", cfg.BootMode, tc.bootMode)
			}
			wantWSURL := "wss://narvi.example.com/sessions/" + sessionRow.ID.String() + "/ws?type=sandbox"
			if cfg.ControlPlaneWsUrl != wantWSURL {
				t.Errorf("ControlPlaneWsUrl = %q, want %q", cfg.ControlPlaneWsUrl, wantWSURL)
			}
			if cfg.CorrelationId != nil {
				t.Errorf("CorrelationId = %v, want nil", cfg.CorrelationId)
			}
			if cfg.Gen != 3 {
				t.Errorf("Gen = %d, want 3", cfg.Gen)
			}
			if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "widgets" {
				t.Errorf("Repos = %+v, unexpected", cfg.Repos)
			}
			if cfg.SandboxId != sandboxID {
				t.Errorf("SandboxId = %q, want %q (must round-trip the sandboxID argument unmodified)", cfg.SandboxId, sandboxID)
			}
			if cfg.SandboxToken != "plaintext-token" {
				t.Errorf("SandboxToken = %q, want %q", cfg.SandboxToken, "plaintext-token")
			}
			if cfg.SessionId != sessionRow.ID.String() {
				t.Errorf("SessionId = %q, want %q", cfg.SessionId, sessionRow.ID.String())
			}
			if cfg.CapabilityRestricted {
				t.Errorf("CapabilityRestricted = true, want false (an ordinary session, no provenance_tag set at all)")
			}
		})
	}
}

// TestAssembleSessionConfig_CapabilityRestricted is the confirmed-finding
// regression test for SessionConfig.CapabilityRestricted's own wiring
// (sessionconfig.go): true exactly when sessionRow.ProvenanceTag is
// provenance.SentinelAutoFix, false for every other value (including nil,
// covered by TestAssembleSessionConfig above, and an ordinary, unrelated
// non-empty tag). Before this fix, no test anywhere in this diff ever
// constructed a sessionRow with ProvenanceTag set at all, so a regression
// silently setting CapabilityRestricted to a constant false (or deleting
// the line entirely) would have compiled and passed every existing test.
func TestAssembleSessionConfig_CapabilityRestricted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		provenanceTag *string
		want          bool
	}{
		{name: "sentinel-auto-fix provenance restricts capability", provenanceTag: strPtr(provenance.SentinelAutoFix), want: true},
		{name: "nil provenance tag is unrestricted", provenanceTag: nil, want: false},
		{name: "an unrelated provenance tag is unrestricted", provenanceTag: strPtr("something_else"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &Actor{publicBaseURL: "https://narvi.example.com"}
			var sessionRow sqlcgen.Session
			if tc.provenanceTag != nil {
				sessionRow = sessionRowWithReposAndProvenance(t, `[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`, *tc.provenanceTag)
			} else {
				sessionRow = sessionRowWithRepos(t, `[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`)
			}

			cfg, err := a.assembleSessionConfig(context.Background(), nil, sessionRow, 1, "plaintext-token", uuid.NewString(), sessionconfig.SessionConfigBootModeFresh)
			if err != nil {
				t.Fatalf("assembleSessionConfig() error = %v, want nil", err)
			}
			if cfg.CapabilityRestricted != tc.want {
				t.Errorf("CapabilityRestricted = %v, want %v (provenance_tag = %v)", cfg.CapabilityRestricted, tc.want, tc.provenanceTag)
			}
		})
	}
}

// TestAssembleSessionConfig_ReviewCounterReviewerModel_NilWithoutPRSessionStore
// pins §26.4's own fail-safe degradation: an Actor with no
// githubPRSession store wired at all (the zero-value *Actor this file's
// own TestAssembleSessionConfig/TestAssembleSessionConfig_CapabilityRestricted
// already construct, and every non-review-session production Actor in
// practice too) must never attempt a nil-store lookup -- reviewCounterReviewerModel's
// own doc comment: "nil ... for every session that is not a GitHub PR
// review session at all". Never touches tx (passed nil here, exactly like
// every other test in this file) at all in this path.
func TestAssembleSessionConfig_ReviewCounterReviewerModel_NilWithoutPRSessionStore(t *testing.T) {
	t.Parallel()

	a := &Actor{publicBaseURL: "https://narvi.example.com"}
	sessionRow := sessionRowWithRepos(t, `[{"name":"widgets","url":"https://github.com/acme/widgets","branch":null}]`)

	cfg, err := a.assembleSessionConfig(context.Background(), nil, sessionRow, 1, "plaintext-token", uuid.NewString(), sessionconfig.SessionConfigBootModeFresh)
	if err != nil {
		t.Fatalf("assembleSessionConfig() error = %v, want nil", err)
	}
	if cfg.ReviewCounterReviewerModel != nil {
		t.Errorf("ReviewCounterReviewerModel = %v, want nil (no githubPRSession store wired at all)", cfg.ReviewCounterReviewerModel)
	}
}

func strPtr(s string) *string { return &s }

// TestPullRequestRef pins which repo a review session's pull request ref is
// put on (technical plan §21.1, §30.4): only one whose url names the
// claim's repository, the base repository that keeps refs/pull/<n>/head,
// compared case-insensitively and with or without ".git". A repo naming
// another repository -- a review session opened while its spec still
// named the fork -- gets none, and boots on its branch as before, and so
// does every session with no claim.
func TestPullRequestRef(t *testing.T) {
	t.Parallel()

	claim := &sqlcgen.GithubPrSession{RepoFullName: "acme/widgets", PrNumber: 7}
	tests := []struct {
		name  string
		claim *sqlcgen.GithubPrSession
		url   string
		want  string // "" means no ref
	}{
		{name: "the base repository", claim: claim, url: "https://github.com/acme/widgets.git", want: "refs/pull/7/head"},
		{name: "the base repository without .git", claim: claim, url: "https://github.com/acme/widgets", want: "refs/pull/7/head"},
		{name: "the base repository in another case", claim: claim, url: "https://github.com/ACME/Widgets.git", want: "refs/pull/7/head"},
		{name: "the fork", claim: claim, url: "https://github.com/contributor/widgets.git"},
		{name: "another repository of the same owner", claim: claim, url: "https://github.com/acme/gadgets.git"},
		{name: "a url with no owner/repo path", claim: claim, url: "https://github.com/acme"},
		{name: "a claim whose number makes no ref", claim: &sqlcgen.GithubPrSession{RepoFullName: "acme/widgets"}, url: "https://github.com/acme/widgets.git"},
		{name: "no claim", url: "https://github.com/acme/widgets.git"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pullRequestRef(tc.claim, sessionconfig.SessionConfigReposElem{Name: "widgets", Url: tc.url})
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("pullRequestRef() = %q, want none", *got)
			case tc.want != "" && (got == nil || *got != tc.want):
				t.Errorf("pullRequestRef() = %v, want %q", got, tc.want)
			}
		})
	}
}

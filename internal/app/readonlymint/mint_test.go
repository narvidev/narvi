package readonlymint_test

import (
	"github.com/jackc/pgx/v5/pgtype"

	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapp"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/readonlymint"
	"github.com/narvidev/narvi/internal/app/shadowledger"
)

// fakeMinter is a readonlymint.Minter test double -- there is no real
// GitHub App reachable from this environment (internal/adapters/outbound/
// githubapp's own doc.go), so every test here exercises Mint against this
// fake rather than a real GitHub API call.
type fakeMinter struct {
	token githubapp.Token
	err   error

	sawOwner     string
	sawRepoNames []string
}

func (f *fakeMinter) MintInstallationToken(_ context.Context, owner string, repoNames []string) (githubapp.Token, error) {
	f.sawOwner = owner
	f.sawRepoNames = repoNames
	if f.err != nil {
		return githubapp.Token{}, f.err
	}
	return f.token, nil
}

// fakeLedgerStore mirrors internal/app/shadowledger's own fakeStore
// (writer_test.go) and internal/app/shadowscm's own identical fake
// (decorator_test.go) -- a store that either records the insert or fails
// on demand.
type fakeLedgerStore struct {
	rows []sqlcgen.CreateShadowSCMWriteParams
	err  error
}

func (s *fakeLedgerStore) Create(_ context.Context, arg sqlcgen.CreateShadowSCMWriteParams) (sqlcgen.ShadowScmWrite, error) {
	if s.err != nil {
		return sqlcgen.ShadowScmWrite{}, s.err
	}
	s.rows = append(s.rows, arg)
	return sqlcgen.ShadowScmWrite{}, nil
}

func readOnlyToken() githubapp.Token {
	return githubapp.Token{
		Value:       "ghs_fake_read_only_token",
		ExpiresAt:   time.Now().Add(time.Hour),
		Permissions: map[string]string{"contents": "read", "metadata": "read"},
	}
}

// testSessionID is a fixed, valid session UUID: Mint carries it through
// to the ledger record untouched, so its value only has to be stable.
var testSessionID = pgtype.UUID{Bytes: [16]byte{0x5c, 0x0d, 0xef, 0x01, 0x23, 0x45, 0x46, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56, 0x78}, Valid: true}

// TestMint_Success proves a genuinely read-only minted token is returned
// as-is AND that the substitution is recorded (§30.6).
//
// This test previously asserted the opposite -- "want 0 rows for a
// successful read-only mint" -- and passed, which is how the missing
// record survived: the assertion encoded the gap as the expectation.
func TestMint_Success(t *testing.T) {
	minter := &fakeMinter{token: readOnlyToken()}
	ledger := &fakeLedgerStore{}

	got, err := readonlymint.Mint(context.Background(), minter, ledger, "acme", []string{"widgets"}, "acme/widgets", "github.com", testSessionID)
	if err != nil {
		t.Fatalf("Mint() error = %v, want nil", err)
	}
	if got.Value != minter.token.Value {
		t.Errorf("Mint() = %+v, want the minter's own token", got)
	}
	if len(ledger.rows) != 1 {
		t.Fatalf("ledger recorded %d rows, want 1: a successful substitution is invisible everywhere else", len(ledger.rows))
	}
	row := ledger.rows[0]
	if row.Operation != "scm_credential_substituted" {
		t.Errorf("row.Operation = %q, want %q", row.Operation, "scm_credential_substituted")
	}
	if row.SessionID != testSessionID {
		t.Errorf("row.SessionID = %v, want the caller's own session %v", row.SessionID, testSessionID)
	}
	if strings.Contains(string(row.SpecJson), minter.token.Value) {
		t.Errorf("the substituted token's own value reached spec_json: %s", row.SpecJson)
	}
	if minter.sawOwner != "acme" || len(minter.sawRepoNames) != 1 || minter.sawRepoNames[0] != "widgets" {
		t.Errorf("minter saw owner=%q repoNames=%v, want owner=acme repoNames=[widgets]", minter.sawOwner, minter.sawRepoNames)
	}
}

// TestMint_MinterFailure_NothingRecorded proves a genuine mint failure
// (never reaching a scope decision at all) is surfaced as a plain error,
// distinct from ErrRefusedByScopeCheck, and records nothing -- there is no
// token to refuse.
func TestMint_MinterFailure_NothingRecorded(t *testing.T) {
	minter := &fakeMinter{err: errors.New("simulated GitHub API failure")}
	ledger := &fakeLedgerStore{}

	_, err := readonlymint.Mint(context.Background(), minter, ledger, "acme", []string{"widgets"}, "acme/widgets", "github.com", testSessionID)
	if err == nil {
		t.Fatal("Mint() error = nil, want an error when the underlying mint itself fails")
	}
	var scopeErr *readonlymint.ErrRefusedByScopeCheck
	if errors.As(err, &scopeErr) {
		t.Errorf("Mint() error = %v (*ErrRefusedByScopeCheck), want a plain mint error -- this is not a scope refusal", err)
	}
	if len(ledger.rows) != 0 {
		t.Errorf("ledger recorded %d rows, want 0 -- a mint failure is not a scope-check refusal", len(ledger.rows))
	}
}

// TestMint_ScopeCheckFailure_RefusesAndRecords is §30.4(4)'s own "the mint
// refuses to serve" requirement: a token minted with anything beyond read
// access is never returned, the refusal is recorded into the ledger with
// no token field, and the returned error is *ErrRefusedByScopeCheck.
func TestMint_ScopeCheckFailure_RefusesAndRecords(t *testing.T) {
	overScoped := readOnlyToken()
	overScoped.Permissions = map[string]string{"contents": "write", "metadata": "read"}
	minter := &fakeMinter{token: overScoped}
	ledger := &fakeLedgerStore{}

	got, err := readonlymint.Mint(context.Background(), minter, ledger, "acme", []string{"widgets"}, "acme/widgets", "github.com", testSessionID)
	if err == nil {
		t.Fatal("Mint() error = nil, want a scope-check refusal")
	}
	var scopeErr *readonlymint.ErrRefusedByScopeCheck
	if !errors.As(err, &scopeErr) {
		t.Fatalf("Mint() error = %v, want *readonlymint.ErrRefusedByScopeCheck", err)
	}
	if got.Value != "" {
		t.Errorf("Mint() token = %+v, want the zero value on a scope-check refusal", got)
	}

	if len(ledger.rows) != 1 {
		t.Fatalf("ledger recorded %d rows, want 1", len(ledger.rows))
	}
	row := ledger.rows[0]
	if row.Operation != "scm_credential_mint_refused" {
		t.Errorf("row.Operation = %q, want %q", row.Operation, "scm_credential_mint_refused")
	}
	if row.RepoFullName != "acme/widgets" {
		t.Errorf("row.RepoFullName = %q, want %q", row.RepoFullName, "acme/widgets")
	}
	// A refusal that cannot be attributed to the session that asked is
	// far weaker evidence: the ledger is read to answer "what did this
	// session try to do", not just "did anything get refused anywhere".
	if row.SessionID != testSessionID {
		t.Errorf("row.SessionID = %v, want the caller's own session %v", row.SessionID, testSessionID)
	}
	if row.ResultJson != nil {
		t.Errorf("row.ResultJson = %s, want nil -- nothing was invented in place of the refused token", row.ResultJson)
	}
	if strings.Contains(string(row.SpecJson), overScoped.Value) {
		t.Errorf("row.SpecJson = %s, must never carry the (refused) token value", row.SpecJson)
	}

	var decoded shadowledger.ScmCredentialMintRefused
	if err := json.Unmarshal(row.SpecJson, &decoded); err != nil {
		t.Fatalf("unmarshal row.SpecJson: %v", err)
	}
	if decoded.Host != "github.com" || decoded.GrantedPermissions["contents"] != "write" {
		t.Errorf("decoded spec = %+v, want Host=github.com and GrantedPermissions[contents]=write", decoded)
	}
}

// TestMint_LedgerRecordFailure_IsAHardError proves record-or-fail end to
// end: when the scope check refuses AND the ledger itself cannot record
// that refusal, Mint must return an error that is NOT ErrRefusedByScopeCheck
// -- a caller must be able to tell "refused, evidenced" apart from
// "refused, but this process cannot even prove it," and must never treat
// the latter as an ordinary, safe-to-403 refusal.
func TestMint_LedgerRecordFailure_IsAHardError(t *testing.T) {
	overScoped := readOnlyToken()
	overScoped.Permissions = map[string]string{"contents": "write"}
	minter := &fakeMinter{token: overScoped}
	ledger := &fakeLedgerStore{err: errors.New("simulated ledger write failure")}

	_, err := readonlymint.Mint(context.Background(), minter, ledger, "acme", []string{"widgets"}, "acme/widgets", "github.com", testSessionID)
	if err == nil {
		t.Fatal("Mint() error = nil, want an error when the ledger cannot record the refusal")
	}
	var scopeErr *readonlymint.ErrRefusedByScopeCheck
	if errors.As(err, &scopeErr) {
		t.Errorf("Mint() error = %v (*ErrRefusedByScopeCheck), want a distinct record-failure error", err)
	}
}

// TestMint_Success_LedgerFailureFailsTheMint holds the SUCCESS path to the
// same record-or-fail rule §30.6 states for suppression generally: a
// substitution this process cannot evidence must not be served.
//
// Failing here is safe precisely because it happens before the token
// leaves: nothing external has happened, and the sandbox gets an error
// rather than a silently unrecorded read-only credential.
func TestMint_Success_LedgerFailureFailsTheMint(t *testing.T) {
	minter := &fakeMinter{token: readOnlyToken()}
	ledger := &fakeLedgerStore{err: errors.New("simulated ledger write failure")}

	got, err := readonlymint.Mint(context.Background(), minter, ledger, "acme", []string{"widgets"}, "acme/widgets", "github.com", testSessionID)
	if err == nil {
		t.Fatal("Mint() error = nil; a substitution that could not be recorded must not be served")
	}
	if got.Value != "" {
		t.Errorf("Mint() returned token %q alongside an error; it must return no credential at all", got.Value)
	}
	var refused *readonlymint.ErrRefusedByScopeCheck
	if errors.As(err, &refused) {
		t.Errorf("error = %v, typed as a scope refusal; a ledger failure is a different fault and callers map the two to different statuses", err)
	}
}

// TestMintUnrecorded runs the same mint and scope check as Mint and never
// touches a ledger: it takes none. A read-only token is returned as-is; an
// over-scoped one is refused with *ErrRefusedByScopeCheck carrying what
// GitHub granted, and no token; a failed mint is a plain error.
func TestMintUnrecorded(t *testing.T) {
	overScoped := readOnlyToken()
	overScoped.Permissions = map[string]string{"contents": "write", "metadata": "read"}

	tests := []struct {
		name        string
		minter      *fakeMinter
		wantToken   bool
		wantRefusal bool
	}{
		{name: "read-only token: returned", minter: &fakeMinter{token: readOnlyToken()}, wantToken: true},
		{name: "over-scoped token: refused", minter: &fakeMinter{token: overScoped}, wantRefusal: true},
		{name: "mint fails: a plain error", minter: &fakeMinter{err: errors.New("simulated GitHub API failure")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readonlymint.MintUnrecorded(context.Background(), tc.minter, "acme", []string{"widgets"})
			if tc.minter.sawOwner != "acme" || len(tc.minter.sawRepoNames) != 1 || tc.minter.sawRepoNames[0] != "widgets" {
				t.Errorf("minter saw owner=%q repoNames=%v, want owner=acme repoNames=[widgets]", tc.minter.sawOwner, tc.minter.sawRepoNames)
			}
			if tc.wantToken {
				if err != nil || got.Value != tc.minter.token.Value {
					t.Fatalf("MintUnrecorded() = %+v, %v; want the minter's own token and no error", got, err)
				}
				return
			}
			if err == nil || got.Value != "" {
				t.Fatalf("MintUnrecorded() = %+v, %v; want no token and an error", got, err)
			}
			var refused *readonlymint.ErrRefusedByScopeCheck
			if errors.As(err, &refused) != tc.wantRefusal {
				t.Fatalf("error = %v; typed as a scope refusal: %v, want %v", err, !tc.wantRefusal, tc.wantRefusal)
			}
			if tc.wantRefusal && refused.GrantedPermissions["contents"] != "write" {
				t.Errorf("refusal GrantedPermissions = %v, want what GitHub granted (contents: write)", refused.GrantedPermissions)
			}
		})
	}
}

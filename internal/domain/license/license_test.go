package license_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/license"
)

// wireClaims mirrors license's own unexported payload shape --
// deliberately duplicated here (this is package license_test, a
// black-box test) rather than reusing an unexported type, so these tests
// exercise the PUBLIC wire format docs/design/boundaries-design.md,
// section 1.5, documents, never this package's own internal
// representation.
type wireClaims struct {
	KeyID        string   `json:"kid"`
	Subject      string   `json:"sub"`
	Product      string   `json:"product"`
	IssuedAt     int64    `json:"iat"`
	NotBefore    int64    `json:"nbf"`
	ExpiresAt    int64    `json:"exp"`
	Capabilities []string `json:"caps"`
}

// sign renders c as "narvi1.<payload>.<sig>", signed by priv -- the exact
// wire format Parse decodes, built independently of anything in
// parse.go.
func sign(t *testing.T, priv ed25519.PrivateKey, c wireClaims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	msg := "narvi1." + payloadB64
	sig := ed25519.Sign(priv, []byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// validClaims returns a claims value that verifies cleanly against
// "test-kid" and decodes to a Grant valid right now.
func validClaims() wireClaims {
	now := time.Now()
	return wireClaims{
		KeyID:        "test-kid",
		Subject:      "customer-1",
		Product:      license.Product,
		IssuedAt:     now.Add(-time.Hour).Unix(),
		NotBefore:    now.Add(-time.Hour).Unix(),
		ExpiresAt:    now.Add(24 * time.Hour).Unix(),
		Capabilities: []string{string(license.CapabilityOrganizationGovernance)},
	}
}

func mustGenerateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// TestParse covers docs/design/boundaries-design.md, section 1.6's own
// named table: every way a raw key can fail before Parse ever trusts its own
// content, plus the clean, valid case.
func TestParse(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	keys := license.Keyset{"test-kid": pub}
	_, otherPriv := mustGenerateKey(t)

	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{
			name:    "absent",
			raw:     "",
			wantErr: license.ErrMalformed,
		},
		{
			name:    "malformed prefix",
			raw:     "narvi2." + base64.RawURLEncoding.EncodeToString([]byte("{}")) + ".c2ln",
			wantErr: license.ErrMalformed,
		},
		{
			name:    "wrong part count",
			raw:     "narvi1.onlyonepart",
			wantErr: license.ErrMalformed,
		},
		{
			name:    "bad base64 payload",
			raw:     "narvi1.not-valid-base64!!!.c2ln",
			wantErr: license.ErrMalformed,
		},
		{
			name: "bad JSON",
			raw: func() string {
				payloadB64 := base64.RawURLEncoding.EncodeToString([]byte("not json"))
				sig := ed25519.Sign(priv, []byte("narvi1."+payloadB64))
				return "narvi1." + payloadB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
			}(),
			wantErr: license.ErrMalformed,
		},
		{
			name: "unknown kid",
			raw: func() string {
				c := validClaims()
				c.KeyID = "no-such-kid"
				return sign(t, priv, c)
			}(),
			wantErr: license.ErrUnknownKey,
		},
		{
			name:    "bad signature",
			raw:     sign(t, otherPriv, validClaims()), // claims "test-kid" but signed by a different key entirely
			wantErr: license.ErrBadSignature,
		},
		{
			name: "wrong product",
			raw: func() string {
				c := validClaims()
				c.Product = "some-other-narvi-product"
				return sign(t, priv, c)
			}(),
			wantErr: license.ErrWrongProduct,
		},
		{
			name: "unknown capability",
			raw: func() string {
				c := validClaims()
				c.Capabilities = []string{"not-a-real-capability"}
				return sign(t, priv, c)
			}(),
			wantErr: license.ErrUnknownCapability,
		},
		{
			name:    "valid",
			raw:     sign(t, priv, validClaims()),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := license.Parse(tt.raw, keys)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Parse() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Parse() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestParse_ValidPayloadDecodesGrant proves the "valid" case above
// decodes every claim into the matching Grant field, not merely a nil
// error.
func TestParse_ValidPayloadDecodesGrant(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	keys := license.Keyset{"test-kid": pub}
	c := validClaims()

	grant, err := license.Parse(sign(t, priv, c), keys)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if grant.KeyID != c.KeyID {
		t.Errorf("grant.KeyID = %q, want %q", grant.KeyID, c.KeyID)
	}
	if grant.Subject != c.Subject {
		t.Errorf("grant.Subject = %q, want %q", grant.Subject, c.Subject)
	}
	if !grant.Has(license.CapabilityOrganizationGovernance) {
		t.Errorf("grant.Has(CapabilityOrganizationGovernance) = false, want true (claims named %v)", c.Capabilities)
	}
	if got := grant.IssuedAt.Unix(); got != c.IssuedAt {
		t.Errorf("grant.IssuedAt.Unix() = %d, want %d", got, c.IssuedAt)
	}
	if got := grant.NotBefore.Unix(); got != c.NotBefore {
		t.Errorf("grant.NotBefore.Unix() = %d, want %d", got, c.NotBefore)
	}
	if got := grant.ExpiresAt.Unix(); got != c.ExpiresAt {
		t.Errorf("grant.ExpiresAt.Unix() = %d, want %d", got, c.ExpiresAt)
	}
}

// TestGrant_Has proves Has checks membership only, never epistemic
// validity (that is ValidAt's own, separate job).
func TestGrant_Has(t *testing.T) {
	g := license.Grant{Capabilities: []license.Capability{license.CapabilityOrganizationGovernance}}
	if !g.Has(license.CapabilityOrganizationGovernance) {
		t.Error("Has(CapabilityOrganizationGovernance) = false, want true")
	}
	if g.Has(license.CapabilityKnowledgeRetrieval) {
		t.Error("Has(CapabilityKnowledgeRetrieval) = true, want false")
	}
}

// TestGrant_ValidAt covers docs/design/boundaries-design.md, section
// 1.6's own named table over the nbf/exp boundary, with an explicit `now` for every
// case -- no wall-clock read anywhere in this test or in ValidAt itself.
func TestGrant_ValidAt(t *testing.T) {
	nbf := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exp := nbf.Add(24 * time.Hour)
	const skew = 5 * time.Minute
	g := license.Grant{NotBefore: nbf, ExpiresAt: exp}

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before nbf beyond skew", nbf.Add(-skew).Add(-time.Second), false},
		{"within skew", nbf.Add(-skew / 2), true},
		{"at nbf", nbf, true},
		{"between", nbf.Add(12 * time.Hour), true},
		{"at exp", exp, false},
		{"after exp", exp.Add(time.Second), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := g.ValidAt(tt.now, skew); got != tt.want {
				t.Errorf("ValidAt(%v, %v) = %v, want %v", tt.now, skew, got, tt.want)
			}
		})
	}
}

// TestVerify proves Verify is exactly Parse-plus-ValidAt: a syntactically
// valid, correctly signed grant that is not yet valid or already expired
// still fails, with the specific error ValidAt's own two failure
// directions map to.
func TestVerify(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	keys := license.Keyset{"test-kid": pub}

	tests := []struct {
		name    string
		claims  func() wireClaims
		now     time.Time
		wantErr error
	}{
		{
			name: "not yet valid",
			claims: func() wireClaims {
				c := validClaims()
				c.NotBefore = time.Now().Add(time.Hour).Unix()
				return c
			},
			now:     time.Now(),
			wantErr: license.ErrNotYetValid,
		},
		{
			name: "expired",
			claims: func() wireClaims {
				c := validClaims()
				c.ExpiresAt = time.Now().Add(-time.Hour).Unix()
				return c
			},
			now:     time.Now(),
			wantErr: license.ErrExpired,
		},
		{
			name:    "valid",
			claims:  validClaims,
			now:     time.Now(),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := sign(t, priv, tt.claims())
			_, err := license.Verify(raw, keys, tt.now, 0)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Verify() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestIssuerKeys_AreWellFormed proves every entry in the embedded
// production keyset (empty today -- see keys.go's own doc comment) is a
// non-empty kid mapped to a well-formed Ed25519 public key. Vacuously
// true while the map is empty; stays meaningful the day a real key is
// added.
func TestIssuerKeys_AreWellFormed(t *testing.T) {
	for kid, pub := range license.IssuerKeys() {
		if kid == "" {
			t.Error("IssuerKeys() contains an empty key id")
		}
		if len(pub) != ed25519.PublicKeySize {
			t.Errorf("IssuerKeys()[%q] has length %d, want %d", kid, len(pub), ed25519.PublicKeySize)
		}
	}
}

// TestIssuerKeys_RejectTestSignedKeys is the negative that matters
// (design note section 1.6): a key signed by a freshly generated, test-only Ed25519 pair
// must NEVER verify against the real, embedded production keyset --
// proving a test fixture can never be mistaken for a production key,
// however this package's own vocabulary or defaults change later.
func TestIssuerKeys_RejectTestSignedKeys(t *testing.T) {
	_, priv := mustGenerateKey(t)
	raw := sign(t, priv, validClaims())

	_, err := license.Parse(raw, license.IssuerKeys())
	if !errors.Is(err, license.ErrUnknownKey) {
		t.Fatalf("Parse() against the production keyset error = %v, want ErrUnknownKey -- a test-signed key must never verify against production keys", err)
	}
}

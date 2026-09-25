package mcpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// PKCE (RFC 7636) bounds: a code_verifier, and the code_challenge a client
// sends, are 43 to 128 characters of the unreserved set
// [A-Z] / [a-z] / [0-9] / "-" / "." / "_" / "~".
const (
	pkceMinLength = 43
	pkceMaxLength = 128
)

// validPKCEValue reports whether s is a syntactically valid PKCE
// code_verifier or code_challenge.
func validPKCEValue(s string) bool {
	if len(s) < pkceMinLength || len(s) > pkceMaxLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// verifyPKCES256 reports whether BASE64URL(SHA256(verifier)) equals
// challenge (RFC 7636 §4.6, method S256 -- the only method this server
// accepts). The comparison is constant-time: this is the one place a
// client-supplied value is compared against a stored secret-derived one,
// and TestPKCE_UsesConstantTimeCompare pins that no plain string
// comparison creeps back in.
func verifyPKCES256(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

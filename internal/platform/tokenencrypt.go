// This file (tokenencrypt.go) implements the AES-256-GCM helpers backing
// §13.1's "Provider tokens encrypted at rest (AES-GCM), per-user" — used by
// internal/adapters/inbound/auth's own OAuth callback handler to encrypt a
// freshly obtained GitHub access token before it is stored in
// identities.access_token_encrypted (migrations/000017_auth_v1.up.sql).
// Standard library only (crypto/aes + crypto/cipher + crypto/rand),
// matching this codebase's existing stdlib-first crypto convention
// (hmacauth.go, tokenhash.go) exactly — no third-party crypto dependency
// added for this.
//
// Nothing in §13.1 ever calls DecryptToken outside this file's own
// round-trip test — §9.3's SourceControl adapter (§8.11: the PR is created
// with the session creator's OAuth token) is the actual future consumer.

package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// tokenEncryptNonceSize is the standard AES-GCM nonce size in bytes (12) —
// a plain byte count, not a duration/interval, so (matching tokenhash.go's
// own wsTokenByteLength precedent) it is an ordinary Go constant rather
// than a platform.Timeouts field.
const tokenEncryptNonceSize = 12

// ErrCiphertextTooShort is returned by DecryptToken when ciphertext is
// shorter than the nonce it must have prepended — a plainly malformed or
// truncated value, detected explicitly rather than risking a slice-bounds
// panic.
var ErrCiphertextTooShort = errors.New("platform: ciphertext shorter than nonce")

// EncryptToken encrypts plaintext with AES-256-GCM under key (exactly 32
// bytes — Config.TokenEncryptionKey, already base64-decoded and
// length-validated once at Load() time, see config.go; never re-decoded
// per call). A fresh crypto/rand-sourced nonce is generated on EVERY call
// and PREPENDED to the returned ciphertext:
// aesgcm.Seal(nonce, nonce, plaintext, nil) — Seal appends its own
// ciphertext+tag after whatever is already in the dst slice, so passing
// nonce as dst naturally prepends it. Two calls encrypting the same
// plaintext under the same key therefore produce different ciphertexts
// every time (proven by this file's own test), since the nonce is
// genuinely fresh, never reused.
//
// Never logs plaintext, key, or the returned ciphertext — callers must
// keep the same discipline (see internal/adapters/inbound/auth's own
// security notes on this exact point).
func EncryptToken(key, plaintext []byte) ([]byte, error) {
	aesgcm, err := newAESGCM(key)
	if err != nil {
		return nil, fmt.Errorf("platform: encrypt token: %w", err)
	}

	nonce := make([]byte, tokenEncryptNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("platform: encrypt token: read nonce: %w", err)
	}

	return aesgcm.Seal(nonce, nonce, plaintext, nil), nil
}

// DecryptToken reverses EncryptToken: extracts the first
// tokenEncryptNonceSize bytes of ciphertext as the nonce, then verifies and
// decrypts the remainder. Returns an error (never a partial/best-effort
// plaintext) if ciphertext was tampered with in any way — AES-GCM's own
// authentication tag catches this, not just confidentiality (proven by
// this file's own tampered-ciphertext test).
func DecryptToken(key, ciphertext []byte) ([]byte, error) {
	aesgcm, err := newAESGCM(key)
	if err != nil {
		return nil, fmt.Errorf("platform: decrypt token: %w", err)
	}

	if len(ciphertext) < tokenEncryptNonceSize {
		return nil, ErrCiphertextTooShort
	}
	nonce, sealed := ciphertext[:tokenEncryptNonceSize], ciphertext[tokenEncryptNonceSize:]

	plaintext, err := aesgcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		// Deliberately wraps only aesgcm.Open's own error, never the
		// ciphertext/nonce bytes themselves — an authentication failure
		// message must not become an accidental leak vector for the
		// encrypted value it's reporting on.
		return nil, fmt.Errorf("platform: decrypt token: open: %w", err)
	}
	return plaintext, nil
}

// newAESGCM builds the shared cipher.AEAD both EncryptToken and
// DecryptToken use, so the two can never drift apart on cipher/mode
// choice.
func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return aesgcm, nil
}

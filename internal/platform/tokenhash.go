// This file (tokenhash.go) implements the hash/mint helpers backing
// §6.2's ws-token mechanism: "WS token: per-participant, hashed at rest,
// minted via REST (/api/sessions/:id/ws-token) right before each
// connection, and valid for WSTokenTTL." Used by BOTH
// internal/adapters/inbound/httpapi's minting handler (GenerateToken +
// HashToken, writing ws_tokens.token_hash) and internal/adapters/inbound/
// wshub's client-subscribe verification (HashToken only, comparing against
// the stored hash).
//
// This is a SEPARATE mechanism from internal/adapters/inbound/wshub's own
// (§3.2) HashSandboxToken/verifySandboxToken: ws-tokens and sandbox
// tokens have different mint/verify call sites, different backing tables
// (ws_tokens vs sandboxes.token_hash), and different consumers (a browser
// client vs a sandbox-agent process) -- see wshub/token.go's own doc
// comment, which this file deliberately does not touch or import.

package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// wsTokenByteLength is the number of raw random bytes GenerateToken reads
// from crypto/rand before base64-encoding them -- a plain byte count, not
// a duration/interval, so (like sessionactor's own mailboxBufferSize) it
// is an ordinary Go constant rather than a platform.Timeouts field. 32
// bytes (256 bits) is comfortably high-entropy for a bearer credential.
const wsTokenByteLength = 32

// HashToken hex-encodes the SHA-256 digest of token -- the exact same
// algorithm as wshub.HashSandboxToken (SHA-256, unsalted: a ws-token is a
// high-entropy, server-generated secret, not a low-entropy human
// password, so the salted-slow-hash rationale for passwords does not
// apply here either), kept as a separate function in a separate package
// specifically because ws-tokens and sandbox tokens are independent
// mechanisms with independent call sites (see this file's own top
// comment) -- never call wshub.HashSandboxToken for a ws-token or vice
// versa, even though the underlying digest algorithm happens to match.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateToken mints a fresh, high-entropy bearer token: wsTokenByteLength
// bytes from crypto/rand, base64.RawURLEncoding-encoded. The returned
// string is the PLAINTEXT token -- returned to the minting caller exactly
// once (the REST ws-token response, §6.2) and never persisted itself; only
// HashToken(token) is ever written to ws_tokens.token_hash, matching
// §6.2's "hashed at rest" requirement literally.
func GenerateToken() (string, error) {
	buf := make([]byte, wsTokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

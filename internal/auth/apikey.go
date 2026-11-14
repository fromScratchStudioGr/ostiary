package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// APIKeyPrefix marks a bearer token as a gateway API key rather than a JWT
// — cheap to check (one prefix comparison) before ever touching Redis or
// Postgres, and unambiguous since a JWT's own structure (header.payload.sig)
// never produces this prefix.
const APIKeyPrefix = "ost_"

// GenerateAPIKey creates a new random gateway API key. raw is shown to the
// caller exactly once; only hash (its SHA-256 digest) is ever stored, and
// displayHint (its last 4 characters) is what a key listing shows
// afterward to help the user tell their keys apart.
func GenerateAPIKey() (raw, hash, displayHint string) {
	buf := make([]byte, 32)
	rand.Read(buf)

	raw = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash = HashAPIKey(raw)
	displayHint = raw[len(raw)-4:]
	return raw, hash, displayHint
}

// HashAPIKey returns the SHA-256 hex digest of a raw gateway API key — a
// one-way lookup key, unlike ProviderKey's envelope encryption, since a
// gateway API key is only ever compared, never decrypted back to plaintext.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IsAPIKey reports whether token is a gateway API key rather than a JWT.
func IsAPIKey(token string) bool {
	return strings.HasPrefix(token, APIKeyPrefix)
}

// ExtractAPIKeyToken returns the raw gateway API key from a request,
// checking the standard Authorization: Bearer header first and falling
// back to the x-api-key header (Anthropic's convention), then the
// x-goog-api-key header (Gemini's), so each provider's official library
// works unchanged. Used by both RequireAPIKey and the cache layer's token
// hashing, so they never disagree on what "the token" is for a given request.
func ExtractAPIKeyToken(r *http.Request) (string, bool) {
	// The scheme is case-insensitive (RFC 7235) and is separated from the
	// token by a space: "Bearerost_abc" isn't a Bearer header.
	if scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " "); strings.EqualFold(scheme, "Bearer") {
		if trimmed := strings.TrimSpace(token); trimmed != "" {
			return trimmed, true
		}
	}

	for _, header := range []string{"x-api-key", "x-goog-api-key"} {
		if token := strings.TrimSpace(r.Header.Get(header)); token != "" {
			return token, true
		}
	}

	return "", false
}

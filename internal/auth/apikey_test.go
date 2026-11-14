package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	raw, hash, displayHint := GenerateAPIKey()

	t.Run("is recognised as a gateway API key", func(t *testing.T) {
		if !strings.HasPrefix(raw, APIKeyPrefix) || !IsAPIKey(raw) {
			t.Errorf("key %q doesn't carry the %q prefix, so the gateway would take it for a JWT", raw, APIKeyPrefix)
		}
	})

	t.Run("is the prefix followed by 32 random bytes in unpadded URL-safe base64", func(t *testing.T) {
		// Strict decoding also rejects the characters that aren't safe in a
		// header or URL (+, /, =).
		secret, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(raw, APIKeyPrefix))
		if err != nil {
			t.Fatalf("the part after the prefix isn't unpadded URL-safe base64: %v", err)
		}
		// A shorter secret would still work, and quietly weaken every key.
		if len(secret) != 32 {
			t.Errorf("the key holds %d bytes of secret, want 32", len(secret))
		}
	})

	t.Run("returns the hash that HashAPIKey gives for the key, and never the key itself", func(t *testing.T) {
		if hash != HashAPIKey(raw) {
			t.Errorf("hash = %q, want HashAPIKey(raw) = %q, or a stored hash would never match a presented key", hash, HashAPIKey(raw))
		}
		if hash == raw || strings.Contains(hash, raw) {
			t.Error("the hash contains the raw key, which must never be stored")
		}
	})

	t.Run("returns the last four characters of the key as the display hint", func(t *testing.T) {
		if len(displayHint) != 4 || !strings.HasSuffix(raw, displayHint) {
			t.Errorf("display hint = %q, want the last 4 characters of %q", displayHint, raw)
		}
	})
}

func TestIsAPIKey(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"a gateway API key", "ost_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", true},
		{"a JWT", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl", false},
		{"an empty token", "", false},
		{"the prefix somewhere other than the start", "xost_abc", false},
		{"the prefix without its underscore", "gwabc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAPIKey(tt.token); got != tt.want {
				t.Errorf("IsAPIKey(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestExtractAPIKeyToken(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
		wantOK  bool
	}{
		{"a bearer token", map[string]string{"Authorization": "Bearer ost_abc"}, "ost_abc", true},
		{"a bearer token with extra whitespace around it", map[string]string{"Authorization": "Bearer \t ost_abc  "}, "ost_abc", true},
		{"the scheme in lowercase", map[string]string{"Authorization": "bearer ost_abc"}, "ost_abc", true},
		{"the scheme in uppercase", map[string]string{"Authorization": "BEARER ost_abc"}, "ost_abc", true},
		{"an x-api-key header", map[string]string{"x-api-key": "ost_abc"}, "ost_abc", true},
		{"an x-api-key header with whitespace around it", map[string]string{"x-api-key": "  ost_abc "}, "ost_abc", true},
		{
			"both headers: the bearer token wins",
			map[string]string{"Authorization": "Bearer ost_bearer", "x-api-key": "ost_header"}, "ost_bearer", true,
		},
		{"an x-goog-api-key header", map[string]string{"x-goog-api-key": "ost_abc"}, "ost_abc", true},
		{
			"both key headers: x-api-key wins",
			map[string]string{"x-api-key": "ost_anthropic", "x-goog-api-key": "ost_gemini"}, "ost_anthropic", true,
		},
		{
			"an x-api-key header that's only whitespace falls back to x-goog-api-key",
			map[string]string{"x-api-key": "  ", "x-goog-api-key": "ost_gemini"}, "ost_gemini", true,
		},

		{"no headers", nil, "", false},
		{"an x-api-key header that's only whitespace", map[string]string{"x-api-key": "  "}, "", false},
		{"an Authorization scheme other than Bearer", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, "", false},
		{
			"another scheme falls back to x-api-key",
			map[string]string{"Authorization": "Basic dXNlcjpwYXNz", "x-api-key": "ost_header"}, "ost_header", true,
		},
		{"a token stuck to the scheme with no space", map[string]string{"Authorization": "Bearerost_abc"}, "", false},
		{
			"a token stuck to the scheme falls back to x-api-key",
			map[string]string{"Authorization": "Bearerost_abc", "x-api-key": "ost_header"}, "ost_header", true,
		},
		{"a scheme that only starts with Bearer", map[string]string{"Authorization": "Bearerx ost_abc"}, "", false},
		{"the scheme alone", map[string]string{"Authorization": "Bearer"}, "", false},
		{"a bearer header without a token", map[string]string{"Authorization": "Bearer "}, "", false},
		{
			"a bearer header without a token falls back to x-api-key",
			map[string]string{"Authorization": "Bearer ", "x-api-key": "ost_header"}, "ost_header", true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}

			got, ok := ExtractAPIKeyToken(req)

			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ExtractAPIKeyToken() = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

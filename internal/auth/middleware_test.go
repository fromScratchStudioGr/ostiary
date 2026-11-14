package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/config"

	"github.com/golang-jwt/jwt/v5"
)

func TestCookieSecure(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		sameSite http.SameSite
		want     bool
	}{
		// Local development runs over plain HTTP, where a Secure cookie
		// would never be sent back.
		{"development with Lax", "development", http.SameSiteLaxMode, false},
		{"development with Strict", "development", http.SameSiteStrictMode, false},
		{"development with SameSite unset", "development", 0, false},

		// Browsers reject SameSite=None without Secure, so it wins even
		// while ENV still says development.
		{"development with None", "development", http.SameSiteNoneMode, true},

		{"production with Lax", "production", http.SameSiteLaxMode, true},
		{"production with None", "production", http.SameSiteNoneMode, true},
		{"production with Strict", "production", http.SameSiteStrictMode, true},

		// Anything that isn't exactly "development" is treated as deployed.
		{"staging", "staging", http.SameSiteLaxMode, true},
		{"environment not set", "", http.SameSiteLaxMode, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Environment: tt.env, CookieSameSite: tt.sameSite}

			if got := cookieSecure(cfg); got != tt.want {
				t.Errorf("cookieSecure(env=%q, sameSite=%v) = %v, want %v", tt.env, tt.sameSite, got, tt.want)
			}
		})
	}
}

const jwtSecret = "test-jwt-secret"

// claimsFor returns claims like the ones login issues, valid for an hour.
func claimsFor(userID uint, username string) Claims {
	now := time.Now()
	return Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
}

// signToken signs claims with the given method and key.
func signToken(t *testing.T, method jwt.SigningMethod, key any, claims Claims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return token
}

func TestRequireJWT(t *testing.T) {
	cfg := config.Config{JWTAuthentication: config.JWTAuthentication{Secret: jwtSecret}}

	// send puts a request through RequireJWT, and reports how many times the
	// wrapped handler ran and what it found in the request.
	type outcome struct {
		rec    *httptest.ResponseRecorder
		calls  int
		claims *Claims
		start  time.Time
	}
	send := func(mutate func(*http.Request)) outcome {
		var out outcome
		handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
			out.calls++
			out.claims = ClaimsFromContext(r)
			out.start = RequestStartFromContext(r)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte("reached"))
		})
		req := httptest.NewRequest(http.MethodGet, "/anything", nil)
		mutate(req)
		out.rec = httptest.NewRecorder()
		handler(out.rec, req)
		return out
	}
	withSession := func(token string) func(*http.Request) {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token}) }
	}

	t.Run("lets a valid session through, with its claims and the request start time", func(t *testing.T) {
		token := signToken(t, jwt.SigningMethodHS256, []byte(jwtSecret), claimsFor(42, "alice"))

		before := time.Now()
		out := send(withSession(token))
		after := time.Now()

		if out.calls != 1 {
			t.Fatalf("the wrapped handler ran %d times, want once", out.calls)
		}
		if out.rec.Code != http.StatusAccepted || out.rec.Body.String() != "reached" {
			t.Errorf("response = %d %q, want the wrapped handler's own", out.rec.Code, out.rec.Body.String())
		}
		if out.claims == nil || out.claims.UserID != 42 || out.claims.Subject != "alice" {
			t.Errorf("claims in the request = %+v, want user 42, alice", out.claims)
		}
		if out.start.Before(before) || out.start.After(after) {
			t.Errorf("request start = %v, want a time between %v and %v", out.start, before, after)
		}
	})

	t.Run("answers 401 with the reason, without running the wrapped handler, to anything else", func(t *testing.T) {
		valid := signToken(t, jwt.SigningMethodHS256, []byte(jwtSecret), claimsFor(42, "alice"))

		expired := claimsFor(42, "alice")
		expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))

		// Correctly signed, but valid forever.
		noExpiry := claimsFor(42, "alice")
		noExpiry.ExpiresAt = nil

		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}

		// Another user's payload under this token's signature, as someone
		// trying to become that user would build it.
		other := signToken(t, jwt.SigningMethodHS256, []byte(jwtSecret), claimsFor(1, "admin"))
		parts, otherParts := strings.Split(valid, "."), strings.Split(other, ".")
		swapped := parts[0] + "." + otherParts[1] + "." + parts[2]

		const missing, invalid = "Missing access token", "Invalid or expired token"
		tests := []struct {
			name    string
			mutate  func(*http.Request)
			message string
		}{
			{"no cookie", func(*http.Request) {}, missing},
			{"a cookie with another name", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "token", Value: valid}) }, missing},
			{"a valid token in the Authorization header instead of the cookie", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid) }, missing},
			{"an empty session cookie", withSession(""), invalid},
			{"something that isn't a JWT", withSession("not-a-jwt"), invalid},
			{"a token signed with another secret", withSession(signToken(t, jwt.SigningMethodHS256, []byte("another-secret"), claimsFor(42, "alice"))), invalid},
			{"an expired token", withSession(signToken(t, jwt.SigningMethodHS256, []byte(jwtSecret), expired)), invalid},
			{"a token with no expiry", withSession(signToken(t, jwt.SigningMethodHS256, []byte(jwtSecret), noExpiry)), invalid},
			{"a payload swapped in after signing", withSession(swapped), invalid},
			{"a token signed with a non-HMAC algorithm", withSession(signToken(t, jwt.SigningMethodES256, ecKey, claimsFor(42, "alice"))), invalid},
			{"a token with no signature (alg none)", withSession(signToken(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claimsFor(42, "alice"))), invalid},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				out := send(tt.mutate)

				if out.calls != 0 {
					t.Errorf("the wrapped handler ran %d times for a rejected request", out.calls)
				}
				if out.rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", out.rec.Code)
				}
				var body api.ErrorResponse
				if err := json.NewDecoder(out.rec.Body).Decode(&body); err != nil {
					t.Fatalf("decoding the body: %v", err)
				}
				if body.Error != tt.message {
					t.Errorf("error = %q, want %q", body.Error, tt.message)
				}
			})
		}
	})
}

func TestRequireAPIKey(t *testing.T) {
	// send puts a request through RequireAPIKey after setting headers on it,
	// and reports how many times the wrapped handler ran and what it found in
	// the request.
	type outcome struct {
		rec    *httptest.ResponseRecorder
		calls  int
		claims *Claims
		start  time.Time
	}
	send := func(headers map[string]string) outcome {
		var out outcome
		handler := RequireAPIKey(func(w http.ResponseWriter, r *http.Request) {
			out.calls++
			out.claims = ClaimsFromContext(r)
			out.start = RequestStartFromContext(r)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte("reached"))
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		out.rec = httptest.NewRecorder()
		handler(out.rec, req)
		return out
	}

	t.Run("lets a gateway API key through, from either header, with the request start time", func(t *testing.T) {
		tests := []struct {
			name    string
			headers map[string]string
		}{
			{"in the Authorization header", map[string]string{"Authorization": "Bearer ost_abc"}},
			{"in the x-api-key header", map[string]string{"x-api-key": "ost_abc"}},
			// Only the prefix is checked here.
			{"any token with the prefix", map[string]string{"Authorization": "Bearer ost_not-a-real-key"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				before := time.Now()
				out := send(tt.headers)
				after := time.Now()

				if out.calls != 1 {
					t.Fatalf("the wrapped handler ran %d times, want once", out.calls)
				}
				if out.rec.Code != http.StatusAccepted || out.rec.Body.String() != "reached" {
					t.Errorf("response = %d %q, want the wrapped handler's own", out.rec.Code, out.rec.Body.String())
				}
				if out.start.Before(before) || out.start.After(after) {
					t.Errorf("request start = %v, want a time between %v and %v", out.start, before, after)
				}
				if out.claims != nil {
					t.Errorf("claims = %+v, want none: an API key isn't a user session", out.claims)
				}
			})
		}
	})

	t.Run("answers 401 with the reason, without running the wrapped handler, to anything else", func(t *testing.T) {
		const missing, invalid = "Missing access token", "Invalid API key"
		jwtToken := signToken(t, jwt.SigningMethodHS256, []byte(jwtSecret), claimsFor(42, "alice"))

		tests := []struct {
			name    string
			headers map[string]string
			message string
		}{
			{"no token", nil, missing},
			{"an Authorization scheme other than Bearer", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, missing},
			{"a bearer header without a token", map[string]string{"Authorization": "Bearer "}, missing},
			{"a JWT, which is a user session and not an API key", map[string]string{"Authorization": "Bearer " + jwtToken}, invalid},
			{"a token without the prefix in x-api-key", map[string]string{"x-api-key": "sk-abc"}, invalid},
			{"the prefix without its underscore", map[string]string{"Authorization": "Bearer gwabc"}, invalid},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				out := send(tt.headers)

				if out.calls != 0 {
					t.Errorf("the wrapped handler ran %d times for a rejected request", out.calls)
				}
				if out.rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", out.rec.Code)
				}
				var body api.ErrorResponse
				if err := json.NewDecoder(out.rec.Body).Decode(&body); err != nil {
					t.Fatalf("decoding the body: %v", err)
				}
				if body.Error != tt.message {
					t.Errorf("error = %q, want %q", body.Error, tt.message)
				}
			})
		}
	})
}

package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func loginConfig(env string, sameSite http.SameSite) config.Config {
	return config.Config{
		Environment:    env,
		CookieSameSite: sameSite,
		JWTAuthentication: config.JWTAuthentication{
			Secret:            jwtSecret,
			ExpirationInHours: 2,
		},
	}
}

// createUser stores a user whose password is the given one.
func createUser(t *testing.T, db *gorm.DB, username, password, status string) models.User {
	t.Helper()
	// The cheapest cost: the handler compares against any.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	user := models.User{Username: username, Password: string(hash), Status: status}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return user
}

// sessionCookie returns the session cookie a response sets, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c
		}
	}
	return nil
}

func TestLoginHandler(t *testing.T) {
	db := testutil.Postgres(t)
	createUser(t, db, "alice", "correct-password", "active")
	createUser(t, db, "revoked-rita", "rita-password", "revoked")

	login := func(cfg config.Config, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		rec := httptest.NewRecorder()
		LoginHandler(cfg, db)(rec, req)
		return rec
	}
	credentials := func(username, password string) string {
		body, _ := json.Marshal(LoginRequest{Username: username, Password: password})
		return string(body)
	}

	t.Run("signs a user in with the right credentials", func(t *testing.T) {
		cfg := loginConfig("development", http.SameSiteLaxMode)

		before := time.Now()
		rec := login(cfg, credentials("alice", "correct-password"))
		after := time.Now()

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body LoginResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body.Username != "alice" {
			t.Errorf("body = %+v (%v), want username alice", body, err)
		}

		cookie := sessionCookie(rec)
		if cookie == nil {
			t.Fatal("no session cookie was set")
		}
		var user models.User
		db.Where("username = ?", "alice").First(&user)
		claims := &Claims{}
		_, err := jwt.ParseWithClaims(cookie.Value, claims, func(*jwt.Token) (any, error) { return []byte(jwtSecret), nil },
			jwt.WithExpirationRequired())
		if err != nil {
			t.Fatalf("the cookie isn't a valid JWT signed with the secret: %v", err)
		}
		if claims.UserID != user.ID || claims.Subject != "alice" {
			t.Errorf("claims = user %d, %q; want user %d, alice", claims.UserID, claims.Subject, user.ID)
		}
		wantExpiry := cfg.JWTAuthentication.ExpirationInHours
		lowest := before.Add(time.Duration(wantExpiry)*time.Hour - time.Second)
		highest := after.Add(time.Duration(wantExpiry) * time.Hour)
		if exp := claims.ExpiresAt.Time; exp.Before(lowest) || exp.After(highest) {
			t.Errorf("token expires at %v, want %d hours from now (between %v and %v)", exp, wantExpiry, lowest, highest)
		}
	})

	t.Run("sets a session cookie the browser keeps for as long as the token lasts, and scripts can't read", func(t *testing.T) {
		for _, cfg := range []config.Config{
			loginConfig("development", http.SameSiteLaxMode),
			loginConfig("production", http.SameSiteNoneMode),
		} {
			t.Run(cfg.Environment, func(t *testing.T) {
				cookie := sessionCookie(login(cfg, credentials("alice", "correct-password")))
				if cookie == nil {
					t.Fatal("no session cookie was set")
				}

				if !cookie.HttpOnly {
					t.Error("the cookie is readable by scripts")
				}
				if cookie.Path != "/" {
					t.Errorf("Path = %q, want /", cookie.Path)
				}
				if want := 2 * 60 * 60; cookie.MaxAge != want {
					t.Errorf("MaxAge = %d, want %d", cookie.MaxAge, want)
				}
				if want := cookieSecure(cfg); cookie.Secure != want {
					t.Errorf("Secure = %v, want %v", cookie.Secure, want)
				}
				if cookie.SameSite != cfg.CookieSameSite {
					t.Errorf("SameSite = %v, want %v", cookie.SameSite, cfg.CookieSameSite)
				}
			})
		}
	})

	t.Run("issues a session that RequireJWT accepts", func(t *testing.T) {
		cfg := loginConfig("development", http.SameSiteLaxMode)
		cookie := sessionCookie(login(cfg, credentials("alice", "correct-password")))
		if cookie == nil {
			t.Fatal("no session cookie was set")
		}

		var claims *Claims
		handler := RequireJWT(cfg, func(_ http.ResponseWriter, r *http.Request) { claims = ClaimsFromContext(r) })
		req := httptest.NewRequest(http.MethodGet, "/anything", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code == http.StatusUnauthorized || claims == nil || claims.Subject != "alice" {
			t.Errorf("RequireJWT rejected the session login issued: %d, claims %+v", rec.Code, claims)
		}
	})

	t.Run("turns a sign-in down, with the reason and no session cookie", func(t *testing.T) {
		cfg := loginConfig("development", http.SameSiteLaxMode)
		tests := []struct {
			name    string
			body    string
			status  int
			message string // an exact message, unless it ends in "…"
		}{
			{"a body that isn't JSON", "not json", http.StatusBadRequest, "Invalid payload"},
			{"no username", credentials("", "correct-password"), http.StatusBadRequest, "Username…"},
			{"no password", credentials("alice", ""), http.StatusBadRequest, "Password…"},
			{"a user that doesn't exist", credentials("nobody", "correct-password"), http.StatusUnauthorized, "Invalid credentials"},
			{"the wrong password", credentials("alice", "wrong-password"), http.StatusUnauthorized, "Invalid credentials"},
			{"a revoked user with the right password", credentials("revoked-rita", "rita-password"), http.StatusForbidden, "Account access revoked"},
			// The password is checked first, so the status isn't revealed to
			// someone who doesn't know it.
			{"a revoked user with the wrong password", credentials("revoked-rita", "wrong-password"), http.StatusUnauthorized, "Invalid credentials"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := login(cfg, tt.body)

				if rec.Code != tt.status {
					t.Errorf("status = %d, want %d", rec.Code, tt.status)
				}
				var body api.ErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decoding the body: %v", err)
				}
				if prefix, partial := strings.CutSuffix(tt.message, "…"); partial {
					if !strings.Contains(body.Error, prefix) {
						t.Errorf("error = %q, want one mentioning %q", body.Error, prefix)
					}
				} else if body.Error != tt.message {
					t.Errorf("error = %q, want %q", body.Error, tt.message)
				}
				if cookie := sessionCookie(rec); cookie != nil {
					t.Errorf("a session cookie was set for a rejected sign-in: %+v", cookie)
				}
			})
		}
	})
}

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogoutHandler(t *testing.T) {
	logout := func(cfgEnv string, sameSite http.SameSite) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		LogoutHandler(loginConfig(cfgEnv, sameSite))(rec, httptest.NewRequest(http.MethodPost, "/logout", nil))
		return rec
	}

	// The request carries no session, and logging out still works.
	t.Run("answers 204 and tells the browser to drop the session cookie", func(t *testing.T) {
		rec := logout("development", http.SameSiteLaxMode)

		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
		cookie := sessionCookie(rec)
		if cookie == nil {
			t.Fatal("no session cookie was sent, so the browser would keep the old one")
		}
		if cookie.Value != "" {
			t.Errorf("Value = %q, want it empty", cookie.Value)
		}
		// A negative MaxAge becomes "Max-Age=0", which deletes the cookie now.
		if cookie.MaxAge != -1 {
			t.Errorf("MaxAge = %d, want -1 (delete now)", cookie.MaxAge)
		}
	})

	// A browser only replaces a cookie when the new one has the same name,
	// path and domain, and rejects one that has weaker attributes (a
	// non-Secure cookie can't overwrite a Secure one), so logout has to send
	// the same attributes login does.
	t.Run("clears the cookie with the same attributes login sets it with", func(t *testing.T) {
		tests := []struct {
			env      string
			sameSite http.SameSite
		}{
			{"development", http.SameSiteLaxMode},
			{"production", http.SameSiteLaxMode},
			{"production", http.SameSiteNoneMode},
		}
		for _, tt := range tests {
			t.Run(tt.env+" "+sameSiteName(tt.sameSite), func(t *testing.T) {
				cfg := loginConfig(tt.env, tt.sameSite)

				cookie := sessionCookie(logout(tt.env, tt.sameSite))
				if cookie == nil {
					t.Fatal("no session cookie was sent")
				}

				if cookie.Path != "/" || !cookie.HttpOnly {
					t.Errorf("Path = %q, HttpOnly = %v; want / and true, as login sets them", cookie.Path, cookie.HttpOnly)
				}
				if want := cookieSecure(cfg); cookie.Secure != want {
					t.Errorf("Secure = %v, want %v", cookie.Secure, want)
				}
				if cookie.SameSite != tt.sameSite {
					t.Errorf("SameSite = %v, want %v", cookie.SameSite, tt.sameSite)
				}
			})
		}
	})
}

func sameSiteName(s http.SameSite) string {
	switch s {
	case http.SameSiteLaxMode:
		return "lax"
	case http.SameSiteNoneMode:
		return "none"
	case http.SameSiteStrictMode:
		return "strict"
	}
	return "unset"
}

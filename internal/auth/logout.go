package auth

import (
	"net/http"

	"github.com/fromScratchStudioGr/ostiary/internal/config"
)

func LogoutHandler(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   cookieSecure(cfg),
			SameSite: cfg.CookieSameSite,
			MaxAge:   -1,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

package httpx

import (
	"net/http"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
)

// RequireSameOrigin rejects any state-changing request whose Origin header
// is present but doesn't match allowedOrigin — defense in depth against
// CSRF on top of the session cookie's SameSite setting. GET/HEAD are exempt
// since they're not supposed to mutate anything.
func RequireSameOrigin(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != allowedOrigin {
				api.WriteError(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// Package httpx holds small HTTP middleware shared across this module's
// multiple server binaries (cmd/gateway, cmd/admin).
package httpx

import "net/http"

// WithCORS allows the configured origin (e.g. a local frontend dev server)
// to call this API from a browser, handling the preflight OPTIONS request
// browsers send ahead of any cross-origin call that carries a JSON body or
// an Authorization header.
//
// credentials should be true only for a server that relies on cookies
// (currently just cmd/admin, whose session cookie carries its JWT) —
// browsers refuse to send or accept cookies on a cross-origin request
// unless the response carries Access-Control-Allow-Credentials, and that
// header is only valid paired with a specific allowedOrigin, never "*".
func WithCORS(allowedOrigin string, credentials bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if credentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

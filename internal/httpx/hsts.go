package httpx

import "net/http"

// WithHSTS sends Strict-Transport-Security on every response when enabled,
// telling browsers to never contact this origin over plain HTTP again for
// the given duration — protects against a downgrade to HTTP even if
// someone types an http:// URL or follows an http:// link.
//
// Only pass enabled=true in real deployments served over HTTPS. Sending it
// during local HTTP dev would make the browser refuse to reach plain
// http://localhost for the next year.
func WithHSTS(enabled bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enabled {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

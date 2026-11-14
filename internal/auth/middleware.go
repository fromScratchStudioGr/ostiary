package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/metrics"

	"github.com/golang-jwt/jwt/v5"
)

type contextKey string

const (
	claimsContextKey contextKey = "claims"
	requestStartKey  contextKey = "requestStart"

	// SessionCookieName is the httpOnly cookie the admin panel's JWT is
	// carried in. Set on login, read on every authenticated request,
	// cleared on logout.
	SessionCookieName = "session"
)

// cookieSecure decides the session cookie's Secure flag. Secure is forced
// on whenever SameSite is "none", since browsers reject that combination
// outright, the environment check alone can't be trusted to get this
// right, since it's easy to leave SameSite=none configured while ENV still
// says development.
func cookieSecure(cfg config.Config) bool {
	return cfg.Environment != "development" || cfg.CookieSameSite == http.SameSiteNoneMode
}

func RequireJWT(cfg config.Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestStart := time.Now()

		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			slog.Warn("requireJWT: missing access token", "remote_addr", r.RemoteAddr, "path", r.URL.Path)
			api.WriteError(w, "Missing access token", http.StatusUnauthorized)
			return
		}
		token := cookie.Value

		stopJWTVerify := metrics.Observe("jwt_verify")
		claims := &Claims{}
		parsedToken, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return []byte(cfg.JWTAuthentication.Secret), nil
		}, jwt.WithExpirationRequired())
		stopJWTVerify()
		if err != nil || !parsedToken.Valid {
			slog.Warn("requireJWT: invalid or expired token", "remote_addr", r.RemoteAddr, "path", r.URL.Path, "error", err)
			api.WriteError(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), claimsContextKey, claims)
		ctx = context.WithValue(ctx, requestStartKey, requestStart)
		next(w, r.WithContext(ctx))
	}
}

func RequireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestStart := time.Now()

		token, ok := ExtractAPIKeyToken(r)
		if !ok {
			slog.Warn("requireAPIKey: missing access token", "remote_addr", r.RemoteAddr, "path", r.URL.Path)
			api.WriteError(w, "Missing access token", http.StatusUnauthorized)
			return
		}

		if !IsAPIKey(token) {
			slog.Warn("requireAPIKey: not a gateway API key", "remote_addr", r.RemoteAddr, "path", r.URL.Path)
			api.WriteError(w, "Invalid API key", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), requestStartKey, requestStart)
		next(w, r.WithContext(ctx))
	}
}

func ClaimsFromContext(r *http.Request) *Claims {
	claims, _ := r.Context().Value(claimsContextKey).(*Claims)
	return claims
}

func RequestStartFromContext(r *http.Request) time.Time {
	start, _ := r.Context().Value(requestStartKey).(time.Time)
	return start
}

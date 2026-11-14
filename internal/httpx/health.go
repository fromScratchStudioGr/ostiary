package httpx

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

type HealthCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// RequireToken lets a request through only if its x-api-key header holds
// token. It guards /health and /metrics, each with a token of its own.
func RequireToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("x-api-key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// HealthHandler runs every check and reports the result, along with the
// running version.
func HealthHandler(version string, checks ...HealthCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		results := make(map[string]string, len(checks))
		healthy := true
		for _, c := range checks {
			if err := c.Check(ctx); err != nil {
				results[c.Name] = err.Error()
				healthy = false
			} else {
				results[c.Name] = "ok"
			}
		}

		status := "ok"
		statusCode := http.StatusOK
		if !healthy {
			status = "unhealthy"
			statusCode = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(map[string]any{
			"status":  status,
			"version": version,
			"checks":  results,
		})
	}
}

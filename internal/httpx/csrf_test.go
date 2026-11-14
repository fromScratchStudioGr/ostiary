package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
)

func TestRequireSameOrigin(t *testing.T) {
	const allowed = "https://panel.test"

	// send puts a request with the given method and Origin header (none when
	// empty) through RequireSameOrigin, and returns the response and how many
	// times the wrapped handler ran.
	send := func(method, origin string) (*httptest.ResponseRecorder, int) {
		calls := 0
		handler := RequireSameOrigin(allowed, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte("reached"))
		}))
		req := httptest.NewRequest(method, "/anything", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec, calls
	}

	tests := []struct {
		name    string
		method  string
		origin  string
		blocked bool
	}{
		{"a state-changing request from the allowed origin", http.MethodPost, allowed, false},
		{"a state-changing request without an Origin header", http.MethodPost, "", false},

		{"POST from another origin", http.MethodPost, "https://evil.test", true},
		{"PATCH from another origin", http.MethodPatch, "https://evil.test", true},
		{"PUT from another origin", http.MethodPut, "https://evil.test", true},
		{"DELETE from another origin", http.MethodDelete, "https://evil.test", true},

		// A browser sends "Origin: null" from sandboxed frames and after some
		// redirects: it's present and isn't the allowed origin.
		{"the null origin", http.MethodPost, "null", true},

		// The origin has to match as a whole.
		{"the same host on another port", http.MethodPost, "https://panel.test:8443", true},
		{"the same host on another scheme", http.MethodPost, "http://panel.test", true},
		{"the same origin with a trailing slash", http.MethodPost, allowed + "/", true},
		{"a subdomain of the allowed origin", http.MethodPost, "https://evil.panel.test", true},
		{"a host that only starts with the allowed origin", http.MethodPost, allowed + ".evil.test", true},

		// Reads aren't supposed to change anything, so they're exempt.
		{"GET from another origin", http.MethodGet, "https://evil.test", false},
		{"HEAD from another origin", http.MethodHead, "https://evil.test", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, calls := send(tt.method, tt.origin)

			if tt.blocked {
				if calls != 0 {
					t.Errorf("the wrapped handler ran %d times for a blocked request", calls)
				}
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403", rec.Code)
				}
				return
			}
			if calls != 1 {
				t.Errorf("the wrapped handler ran %d times, want once", calls)
			}
			if rec.Code != http.StatusAccepted || rec.Body.String() != "reached" {
				t.Errorf("response = %d %q, want the wrapped handler's own", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("a blocked request gets the API's JSON error", func(t *testing.T) {
		rec, _ := send(http.MethodPost, "https://evil.test")

		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body api.ErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decoding the body: %v", err)
		}
		if body.Error != "Forbidden" {
			t.Errorf("error = %q, want Forbidden", body.Error)
		}
	})
}

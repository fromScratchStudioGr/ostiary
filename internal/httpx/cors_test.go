package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const panelOrigin = "https://panel.test"

// serve sends a request through WithCORS and returns the response, and how
// many times the wrapped handler was called. Tests read headers from
// rec.Result(), which is what was sent when the status was written:
// rec.Header() would also show a header set afterwards, which a real server
// drops.
func serve(t *testing.T, credentials bool, method string, next http.HandlerFunc) (*httptest.ResponseRecorder, int) {
	t.Helper()
	calls := 0
	handler := WithCORS(panelOrigin, credentials, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		next(w, r)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, "/anything", nil))
	return rec, calls
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestWithCORS(t *testing.T) {
	t.Run("lets the configured origin call the API with the methods and headers the panel uses", func(t *testing.T) {
		rec, _ := serve(t, false, http.MethodGet, ok)

		want := map[string]string{
			"Access-Control-Allow-Origin":  panelOrigin,
			"Access-Control-Allow-Methods": "GET, POST, PATCH, DELETE, OPTIONS",
			"Access-Control-Allow-Headers": "Content-Type, Authorization",
		}
		for header, value := range want {
			if got := rec.Result().Header.Get(header); got != value {
				t.Errorf("%s = %q, want %q", header, got, value)
			}
		}
	})

	t.Run("allows credentials only when asked to", func(t *testing.T) {
		tests := []struct {
			name        string
			credentials bool
			want        string
		}{
			{"asked", true, "true"},
			{"not asked", false, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec, _ := serve(t, tt.credentials, http.MethodGet, ok)

				if got := rec.Result().Header.Get("Access-Control-Allow-Credentials"); got != tt.want {
					t.Errorf("Access-Control-Allow-Credentials = %q, want %q", got, tt.want)
				}
			})
		}
	})

	t.Run("passes a request on to the wrapped handler and keeps its response", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			t.Run(method, func(t *testing.T) {
				var seen string
				rec, calls := serve(t, false, method, func(w http.ResponseWriter, r *http.Request) {
					seen = r.Method
					w.WriteHeader(http.StatusCreated)
					w.Write([]byte("created"))
				})

				if calls != 1 || seen != method {
					t.Errorf("wrapped handler called %d times with %q, want once with %q", calls, seen, method)
				}
				if rec.Code != http.StatusCreated || rec.Body.String() != "created" {
					t.Errorf("response = %d %q, want 201 \"created\"", rec.Code, rec.Body.String())
				}
			})
		}
	})

	t.Run("adds the headers to an error response too, so the browser lets the page read it", func(t *testing.T) {
		rec, _ := serve(t, true, http.MethodPost, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if got := rec.Result().Header.Get("Access-Control-Allow-Origin"); got != panelOrigin {
			t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, panelOrigin)
		}
	})

	t.Run("answers a preflight request itself, with the headers and no content", func(t *testing.T) {
		rec, calls := serve(t, true, http.MethodOptions, ok)

		if calls != 0 {
			t.Errorf("the wrapped handler was called %d times, want none", calls)
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want it empty", rec.Body.String())
		}
		if got := rec.Result().Header.Get("Access-Control-Allow-Origin"); got != panelOrigin {
			t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, panelOrigin)
		}
		if got := rec.Result().Header.Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
		}
	})
}
